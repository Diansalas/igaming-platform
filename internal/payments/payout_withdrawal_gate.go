package payments

// F4 (platform-freeze work list 2026-10-16; A6 SR6 I-2 / residual 4): the WD-RG-1 withdrawal RG/risk gate re-run
// at every payout dispatch point that leads to a NEW Withdraw call.
//
//   - T1p (ClaimForDispatch, payout.go): before the L1 lock; a denial PARKS the payout in `approved` (hold kept,
//     no attempt, no provider call) with a denying decision row, a staff-attributed denial audit and the
//     withdrawal.payout_gate_denied alert; an evaluation failure rolls the claim back (retryable).
//   - T2 re-claim / T12 resend (payout_sweep.go): before the L1 lock; a denial ESCALATES the attempt (T16, the
//     existing sticky "needs a human" state the KYC and destination denials at T2 / T12 already use: no resend,
//     no hold release) with a denying decision row, a system audit and the alert; an evaluation failure rolls the
//     tick back (the attempt is re-leased on a later tick: retry, never a send).
//
// Rules are the request-time rules (internal/withdrawal/dispatchgate.go). Not gated, by design: the QueryStatus
// poll, the evidence / completion / decline / dispute paths (money may already have left: the platform records
// the truth, never refuses it). The database backstop (migration 0169) refuses approved -> submitted and a payout
// attempt's created|ambiguous -> submitting without an allowing decision row of the same transaction (WD053) and
// re-checks the account and wallet status in SQL (WD052).

import (
	"context"
	"errors"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// withdrawalGateAlertCodeShape bounds the reason code an F4 alert attribute may carry (the closed rg / risk code
// vocabulary is lower-case words); anything else is reported as unclassified.
var withdrawalGateAlertCodeShape = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// WithdrawalGateDeniedAlert builds the withdrawal.payout_gate_denied alert (F4) for subject ("withdrawal:<id>" at
// T1p, "payout_attempt:<id>" at T2 / T12). Attributes are closed codes only (gate code, reason code, stage):
// never an amount, an account or a person.
func WithdrawalGateDeniedAlert(tenantID uuid.UUID, subject string, stage withdrawal.DispatchStage, d withdrawalgate.Decision) alerting.Alert {
	reason := d.Reason
	if !withdrawalGateAlertCodeShape.MatchString(reason) {
		reason = alertReasonUnclassified
	}
	return alerting.Alert{
		Kind: alerting.KindWithdrawalPayoutGateDenied, SubjectTenantID: tenantID,
		Discriminator: subject + ":gate:" + d.GateCode,
		Attributes:    map[string]alerting.AttrValue{"gate_code": d.GateCode, "reason_code": reason, "stage": string(stage)},
	}
}

// withdrawalGateForSweeper is the gate the sweeper uses: the orchestrator's (a nil orchestrator or gate refuses).
func (s *Sweeper) withdrawalGate() withdrawalgate.Gate {
	if s.Orchestrator == nil {
		return nil
	}
	return s.Orchestrator.withdrawalGate
}

// evaluateRedispatchGate runs the WD-RG-1 gate for the withdrawal request behind a payout attempt at T2 / T12,
// BEFORE the caller's L1 lock (ADR 0082 A7-N1). The gate input is the request's immutable columns, so the unlocked
// read is sound. An evaluation failure is an error wrapping withdrawal.ErrGateUnavailable: the caller's
// transaction rolls back and the attempt is retried on a later tick (never a send).
func (s *Sweeper) evaluateRedispatchGate(ctx context.Context, tx pgx.Tx, attempt PaymentAttempt) (withdrawalgate.Decision, error) {
	pre, err := withdrawal.GetByID(ctx, tx, *attempt.WithdrawalRequestID)
	if err != nil {
		return withdrawalgate.Decision{}, err
	}
	// lic11: the tenant's withdrawal restriction policy is evaluated at the RE-DISPATCH stage (a configured hold_for_review or
	// block becomes a WD056 denial: escalate, like every gate denial here).
	return withdrawal.EvaluateDispatchGate(ctx, tx, s.withdrawalGate(), pre, withdrawal.AtRedispatch())
}

// applyRedispatchGate finishes the T2 / T12 gate step AFTER the L1 lock (wr is the locked, `submitted` request).
// On ALLOW it records the allowing decision row (the 0169 backstop requires it in this transaction before the
// attempt's created|ambiguous -> submitting) and returns true. On DENY it parks: an attempt already escalated is
// only rescheduled (idempotent, no new row); otherwise the denying decision row, Escalate (T16: no resend, no
// release), the system audit and, LAST, the alert. Returns false on a denial.
func applyRedispatchGate(ctx context.Context, tx pgx.Tx, wr withdrawal.WithdrawalRequest, attempt PaymentAttempt, d withdrawalgate.Decision, nextActionAt time.Time) (bool, error) {
	attemptID := attempt.ID
	if d.Allowed {
		if err := withdrawal.RecordDispatchGateDecision(ctx, tx, wr, withdrawal.DispatchStageRedispatch, &attemptID, d); err != nil {
			return false, err
		}
		return true, nil
	}
	if attempt.EscalatedAt != nil {
		return false, RescheduleNonTerminal(ctx, tx, attempt.ID, nextActionAt)
	}
	if err := withdrawal.RecordDispatchGateDecision(ctx, tx, wr, withdrawal.DispatchStageRedispatch, &attemptID, d); err != nil {
		return false, err
	}
	if err := Escalate(ctx, tx, attempt.ID, nextActionAt); err != nil {
		return false, err
	}
	md := withdrawalGateDenialMetadata(wr, withdrawal.DispatchStageRedispatch, d)
	md["withdrawal_request_id"] = wr.ID.String()
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: wr.TenantID, ActorType: audit.ActorSystem, Action: "payments.payout_redispatch_denied_by_withdrawal_gate",
		TargetType: "payment_attempt", TargetID: attempt.ID.String(), Outcome: audit.OutcomeDenied, Metadata: md,
	}); err != nil {
		return false, err
	}
	return false, alerting.RaiseGuarded(ctx, tx, WithdrawalGateDeniedAlert(wr.TenantID, "payout_attempt:"+attempt.ID.String(), withdrawal.DispatchStageRedispatch, d))
}

// ---- SF4-5: a silent fail-closed gate raises a (throttled) alert -------------------------------------------------

// gateUnavailableAlertAfter is the number of CONSECUTIVE fail-closed ticks of one attempt (T2 / T12) after which the
// withdrawal.payout_gate_unavailable alert is raised. A single transient failure is retried silently; a persistent one
// (a down risk engine, a missing signing key) must not stall payouts unnoticed. At T1p (a staff request) every
// refusal raises. Both dedup on an open alert per request / attempt, so repeats only add occurrences (throttle).
const gateUnavailableAlertAfter = 3

// gateFailCounts: attempt id -> consecutive fail-closed ticks (process-local, a throttle only).
var gateFailCounts = newGateFailCounter(gateFailCountsMax, gateFailStaleAfter, time.Now)

// gateFailCountsMax bounds the counter's memory. Closure security review F-4: reaching it must never SILENCE the alert.
// The previous version wiped the whole map at the bound, so with more than gateFailCountsMax attempts failing closed at
// once (a missing key everywhere: the case the alert exists for) no attempt ever reached the threshold.
const gateFailCountsMax = 4096

// gateFailStaleAfter: an entry not touched for this long belongs to an attempt the sweeper no longer ticks (it moved on
// through another path); only such entries are evicted to make room.
const gateFailStaleAfter = time.Hour

// gateFailCounter counts consecutive fail-closed ticks per attempt with bounded memory. At the bound it first evicts
// stale entries (not touched for staleAfter); if the counter is still full, the failing attempt it cannot track is
// reported as AT the threshold (more than max attempts failing closed at once is itself the outage the alert is for), so
// a mass outage always alerts and nothing that is still failing is ever dropped.
type gateFailCounter struct {
	mu         sync.Mutex
	max        int
	staleAfter time.Duration
	now        func() time.Time
	m          map[uuid.UUID]gateFailEntry
}

type gateFailEntry struct {
	n    int
	last time.Time
}

func newGateFailCounter(limit int, staleAfter time.Duration, now func() time.Time) *gateFailCounter {
	return &gateFailCounter{max: limit, staleAfter: staleAfter, now: now, m: map[uuid.UUID]gateFailEntry{}}
}

// reset forgets an attempt (a tick that did not fail closed).
func (c *gateFailCounter) reset(id uuid.UUID) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}

// fail records one fail-closed tick of id and reports the consecutive count to compare with the threshold (overflow
// reports gateUnavailableAlertAfter: alert now).
func (c *gateFailCounter) fail(id uuid.UUID) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	e, tracked := c.m[id]
	if !tracked && len(c.m) >= c.max {
		for k, v := range c.m {
			if now.Sub(v.last) >= c.staleAfter {
				delete(c.m, k)
			}
		}
		if len(c.m) >= c.max {
			return gateUnavailableAlertAfter
		}
	}
	e.n++
	e.last = now
	c.m[id] = e
	return e.n
}

// len is the number of tracked attempts (tests).
func (c *gateFailCounter) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}

// gateUnavailableCause maps an error wrapping withdrawal.ErrGateUnavailable to a closed cause code.
func gateUnavailableCause(err error) string {
	if errors.Is(err, withdrawal.ErrDispatchProofUnavailable) {
		return "proof_unavailable"
	}
	return "evaluation_failed"
}

// WithdrawalGateUnavailableAlert builds the withdrawal.payout_gate_unavailable alert for subject ("withdrawal:<id>" at
// T1p, "payout_attempt:<id>" at T2 / T12). Attributes are closed codes only.
func WithdrawalGateUnavailableAlert(tenantID uuid.UUID, subject string, stage withdrawal.DispatchStage, cause string) alerting.Alert {
	return alerting.Alert{
		Kind: alerting.KindWithdrawalPayoutGateUnavailable, SubjectTenantID: tenantID,
		Discriminator: subject + ":gate_unavailable",
		Attributes:    map[string]alerting.AttrValue{"stage": string(stage), "cause": cause},
	}
}

// raiseGateUnavailable raises the alert detached (the refused transaction rolled back); best effort, never surfaced.
func raiseGateUnavailable(ctx context.Context, pool *db.Pool, tenantID uuid.UUID, subject string, stage withdrawal.DispatchStage, cause string) {
	_ = alerting.RaiseDetached(ctx, alerting.NewTenantRunner(pool, tenantID), WithdrawalGateUnavailableAlert(tenantID, subject, stage, cause))
}

// observeRedispatchGate counts consecutive fail-closed ticks of a T2 / T12 attempt and raises the alert at the threshold;
// any tick that did not fail closed resets the count. It returns err unchanged.
func (s *Sweeper) observeRedispatchGate(ctx context.Context, tenantID uuid.UUID, attempt PaymentAttempt, err error) error {
	if err == nil || !errors.Is(err, withdrawal.ErrGateUnavailable) {
		gateFailCounts.reset(attempt.ID)
		return err
	}
	if n := gateFailCounts.fail(attempt.ID); n >= gateUnavailableAlertAfter {
		raiseGateUnavailable(ctx, s.Pool, tenantID, "payout_attempt:"+attempt.ID.String(), withdrawal.DispatchStageRedispatch, gateUnavailableCause(err))
	}
	return err
}
