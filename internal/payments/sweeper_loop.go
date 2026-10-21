// PRH-2 H (CP-W1): the sweeper PROCESS. Until this file, Sweeper.RunOnce had
// no running caller outside tests, so nothing ever polled a pending deposit or
// dispatched, resent or resolved a payout attempt. RunSweeperLoop mirrors
// reconciliation.RunSchedulerLoop (one pass immediately, then every interval;
// a pass's failures are logged and counted, never crash the loop or the
// process) and activates BOTH halves of the Sweeper: the deposit poll and the
// payout dispatch (T2 re-claim), resend (T12) and resolve paths. The payout half
// runs only when Sweeper.PayoutKYCGate is non-nil (payout_sweep.go), which is
// why ValidateForLoop refuses a Sweeper without it: a silently inert payout
// half is exactly the code-reviewer H3 finding.
//
// Correctness (merged S-8 / LF-8) rests on, in this order: the row lease with
// FOR UPDATE SKIP LOCKED (claimBatch, SweeperBatchLeaseOwner), the claim-token
// CAS on every state-changing transition, ledger idempotency
// (provider_id, provider_tx_id) and the 0107 indexes. The optional per-tenant
// advisory lock (Sweeper.TenantAdvisoryHint) is only an efficiency hint; it is
// taken inside claimBatch's own short tx and is never held across a provider
// call. No transaction is open during QueryStatus, Deposit or Withdraw: every
// item runs as a sequence of short WithTenant transactions around gate-wrapped
// provider calls, exactly as RunOnce does.
//
// Non-active tenants are RESOLUTION-ONLY (security addendum §2, ADR 0095 §7.3
// amendment): a tenant with non-terminal attempts is swept whatever its status,
// because in-flight money must resolve, but a suspended or closed tenant gets
// no NEW money-moving call - no created-attempt dispatch, no cascade child, no
// payout T2 re-claim or T12 resend. See sweeper_resolution_only.go.
package payments

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// SweeperLoopDefaultInterval is ADR 0095 §7.3's RECOMMENDATION sweep tick
// (reversible engineering default, not a measured value).
const SweeperLoopDefaultInterval = 15 * time.Second

// SweeperItemTimeout is the drain budget an item gets AFTER the loop's context is cancelled
// (shutdown). It is NOT applied to an item in normal operation: a provider call is bounded by
// the gate's manifest CallTimeout (DefaultCallTimeout 30 s), and a result obtained from phase B
// must always be recorded (LF F1: an item deadline shorter than a slow Deposit left the attempt
// `submitting` with no reference, never polled). It is a best-effort bound, not a guarantee:
//
//   - payout items re-detach with WithoutCancel inside DispatchWithdraw/PollPayoutStatus/
//     ApplyPayoutResult (payoutOutboundCallBound up to 60 s plus payoutPhaseCTimeout), so a payout
//     item can outlive this budget and main's 10 s bounded wait;
//   - deposit phase C and the poll's result application run on their own detached context
//     bounded by depositPhaseCTimeout, so a phase-B result is recorded even near the budget.
//
// Deposit drain can overshoot: after the budget a deposit item may still run its detached phase C
// (up to depositPhaseCTimeout), so ~8+5 s can exceed main's 10 s wait.
// An item cut short is money-safe: its lease expires; a PAYOUT submitting attempt converges via
// QueryStatus / T6 ambiguous (resend only under T12's idempotent-manifest rules). A DEPOSIT
// `submitting` attempt with no reference bound (crash, or phase C exceeding depositPhaseCTimeout)
// is never polled: it resolves only by a callback matched on merchant reference or by
// reconciliation (no deposit T16, ADR 0095 §36 residual).
// Cross-tenant effect: the pass is sequential, so one tenant's slow provider delays the next
// tenants in the pass (head-of-line); the §7.3 concurrency caps are the registered follow-up.
const SweeperItemTimeout = 8 * time.Second

// depositPhaseCTimeout bounds the detached context deposit phase C / poll application run on
// (mirrors payoutPhaseCTimeout).
const depositPhaseCTimeout = 5 * time.Second

// sweeperItemTimeout is SweeperItemTimeout, overridable only by tests.
var sweeperItemTimeout = SweeperItemTimeout

var errSweeperPanic = errors.New("payments: sweeper recovered from panic")

var sweeperMeter = otel.Meter("github.com/Diansalas/igaming-platform/internal/payments")

// Counters carry no tenant, provider or attempt label (bounded cardinality).
var (
	sweeperPassesTotal, _ = sweeperMeter.Int64Counter("payments_sweeper_passes_total",
		metric.WithDescription("Completed payments sweeper passes (PRH-2 H)."))
	sweeperItemsTotal, _ = sweeperMeter.Int64Counter("payments_sweeper_items_total",
		metric.WithDescription("Sweeper items by result: processed, error, panic (PRH-2 H)."))
	sweeperTenantFailuresTotal, _ = sweeperMeter.Int64Counter("payments_sweeper_tenant_failures_total",
		metric.WithDescription("Sweeper tenant-level failures by phase: list, claim, panic (PRH-2 H)."))
	sweeperResolutionOnlyBlocksTotal, _ = sweeperMeter.Int64Counter("payments_sweeper_resolution_only_blocks_total",
		metric.WithDescription("New money-moving dispatches withheld for a non-active tenant, by site (security addendum §2)."))

	sweeperLastPassUnix atomic.Int64
	_, _                = sweeperMeter.Int64ObservableGauge("payments_sweeper_last_pass_unix_seconds",
		metric.WithDescription("Unix time the last sweeper pass completed; a stalled sweeper stops advancing it."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if v := sweeperLastPassUnix.Load(); v != 0 {
				o.Observe(v)
			}
			return nil
		}))
)

func recordSweeperItem(ctx context.Context, result string) {
	if sweeperItemsTotal != nil {
		sweeperItemsTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
	}
}

func recordSweeperTenantFailure(ctx context.Context, phase string) {
	if sweeperTenantFailuresTotal != nil {
		sweeperTenantFailuresTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("phase", phase)))
	}
}

func recordResolutionOnlyBlock(ctx context.Context, site string) {
	if sweeperResolutionOnlyBlocksTotal != nil {
		sweeperResolutionOnlyBlocksTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("site", site)))
	}
}

// ValidateForLoop reports a Sweeper that must not be started as a process:
// a missing dependency, or a nil PayoutKYCGate (which would leave the whole
// payout half inert without a word).
func (s *Sweeper) ValidateForLoop() error {
	switch {
	case s == nil:
		return errors.New("payments: sweeper is nil")
	case s.Pool == nil:
		return errors.New("payments: sweeper has no pool")
	case s.Orchestrator == nil:
		return errors.New("payments: sweeper has no orchestrator")
	case s.KYCGate == nil:
		return errors.New("payments: sweeper has no deposit KYC gate")
	case s.PayoutKYCGate == nil:
		return errors.New("payments: sweeper has no payout KYC gate (payout sweeping would be inert)")
	}
	// A nil CredResolver is deliberately NOT refused: with no resolver wired
	// (production, test support off, no real credential subsystem) callProvider
	// fails every call closed as NotSent, exactly as the HTTP path does, and the
	// platform must still start. runSweeperLoop warns about it instead.
	return nil
}

// SweepPassStats summarises one RunPass (test/observability only).
type SweepPassStats struct {
	Listed    bool // the tenant listing succeeded (H-CR-10)
	Tenants   int
	Claimed   int
	Processed int
	Errors    int
	Panics    int
	// ScreeningParked / ScreeningParkedOverdue (lic26 N-9): persons whose payout is parked ONLY for an outstanding automatic AML
	// screening, and those parked longer than the gating policy's alert threshold. Counts only, never an identifier.
	ScreeningParked        int
	ScreeningParkedOverdue int
	// lic29: persons waiting for an OFFICER (counted apart from the dead-worker signal above), the persons scanned this pass, whether
	// the keyset cycle continues next pass, and how many tenants' reads failed.
	ScreeningParkedOfficer        int
	ScreeningParkedOfficerOverdue int
	ScreeningParkedScanned        int
	ScreeningParkedPartial        bool
	ScreeningParkedReadFailures   int
}

// sweeperParkedAlertTimeout bounds the parked-screening read and the alert raise of one tenant.
const sweeperParkedAlertTimeout = 5 * time.Second

// sweeperParkedFailAlertAfter is how many CONSECUTIVE passes the parked-screening read may fail for one tenant before an alert is
// raised (lic29, security F-2): a monitor that can no longer see must not fail silently. ENGINEERING value.
const sweeperParkedFailAlertAfter = 3

// reportScreeningParked (lic26 N-9, lic29) is the PRODUCTION observation of the persons the automatic AML screening gate holds. It
// runs here, in a loop that is NOT the screening worker, so a DEAD worker (which can no longer raise its own alert) is visible. One
// pass reads ONE keyset batch (aml.ParkedScanBatch persons, in person_id order) from a per-tenant cursor, so every person is observed
// once per cycle and a pass costs a bounded read; "partial" in the stats means the cycle continues next pass. Persons stuck for want
// of a screening result and persons waiting for an OFFICER are counted and alerted APART (different source in the discriminator). The
// read is its own tenant transaction; log lines and alerts carry COUNTS only (no person, account or amount: tipping-off), and a
// failure never stops the sweep, but N consecutive failures raise aml.screening_attention (reason monitor_failing, platform-routed).
func (s *Sweeper) reportScreeningParked(ctx context.Context, logger *slog.Logger, tenantID uuid.UUID, st *SweepPassStats) {
	if ctx.Err() != nil {
		return
	}
	s.parkedMu.Lock()
	if s.parkedCursor == nil {
		s.parkedCursor, s.parkedFails = map[uuid.UUID]*uuid.UUID{}, map[uuid.UUID]int{}
	}
	after := s.parkedCursor[tenantID]
	s.parkedMu.Unlock()

	var sc aml.ParkedScan
	rctx, cancel := context.WithTimeout(ctx, sweeperParkedAlertTimeout)
	err := s.guarded(func() error {
		switch {
		case s.testScreeningScan != nil:
			var e error
			sc, e = s.testScreeningScan(tenantID, after)
			return e
		case s.testScreeningParked != nil:
			p, o, e := s.testScreeningParked(tenantID)
			sc = aml.ParkedScan{Parked: p, Overdue: o}
			return e
		}
		return s.Pool.WithTenant(rctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			sc, e = aml.ScreeningParkedScan(ctx, tx, tenantID, after, aml.ParkedScanBatch)
			return e
		})
	})
	cancel()
	if err != nil {
		recordSweeperTenantFailure(ctx, "screening_parked")
		st.Errors++
		st.ScreeningParkedReadFailures++
		s.parkedMu.Lock()
		s.parkedFails[tenantID]++
		n := s.parkedFails[tenantID]
		s.parkedMu.Unlock()
		if logger != nil {
			logger.Error("payments sweeper: screening-parked summary failed", "tenant_id", tenantID, "consecutive_failures", n)
		}
		if n >= sweeperParkedFailAlertAfter {
			if aerr := aml.RaiseScreeningMonitorFailing(ctx, s.Pool, tenantID, sweeperParkedAlertTimeout); aerr != nil {
				recordSweeperTenantFailure(ctx, "screening_parked_alert")
				st.Errors++
				if logger != nil {
					logger.Error("payments sweeper: screening-monitor alert failed", "tenant_id", tenantID)
				}
			}
		}
		return
	}
	s.parkedMu.Lock()
	s.parkedFails[tenantID] = 0
	s.parkedCursor[tenantID] = sc.Next // nil: the cycle is complete, the next pass starts again from the start
	s.parkedMu.Unlock()

	st.ScreeningParked += sc.Parked
	st.ScreeningParkedOverdue += sc.Overdue
	st.ScreeningParkedOfficer += sc.Officer
	st.ScreeningParkedOfficerOverdue += sc.OfficerOverdue
	st.ScreeningParkedScanned += sc.Scanned
	if sc.Next != nil {
		st.ScreeningParkedPartial = true
	}
	if (sc.Parked > 0 || sc.Officer > 0) && logger != nil {
		logger.Warn("payments sweeper: payouts parked for an outstanding AML screening", "tenant_id", tenantID, "parked", sc.Parked, "overdue", sc.Overdue,
			"officer_queue", sc.Officer, "officer_overdue", sc.OfficerOverdue, "scanned", sc.Scanned, "partial", sc.Next != nil)
	}
	for _, a := range []struct {
		source string
		n      int
	}{{"sweeper", sc.Overdue}, {"sweeper_officer", sc.OfficerOverdue}} {
		if a.n == 0 {
			continue
		}
		if aerr := aml.RaiseScreeningTrapped(ctx, s.Pool, tenantID, a.source, a.n, sweeperParkedAlertTimeout); aerr != nil {
			recordSweeperTenantFailure(ctx, "screening_parked_alert")
			st.Errors++
			if logger != nil {
				logger.Error("payments sweeper: screening-parked alert failed", "tenant_id", tenantID)
			}
		}
	}
}

// sweepTenantIDs lists EVERY tenant, whatever its status: a suspended or
// closed tenant's in-flight attempts must still resolve (security addendum §2).
// Each tenant costs one indexed range scan (idx_payment_attempts_due) when it
// has nothing due. The order rotates by `offset` so one tenant is not always
// first (the round-robin of ADR 0095 §7.2 point 1).
func sweepTenantIDs(ctx context.Context, s *Sweeper, offset int) ([]uuid.UUID, error) {
	var ids []uuid.UUID
	err := s.Pool.WithoutTenant(ctx, func(actx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(actx, `SELECT id FROM tenants ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return rows.Err()
	})
	if err != nil || len(ids) < 2 {
		return ids, err
	}
	k := offset % len(ids)
	return append(ids[k:], ids[:k]...), nil
}

// RunPass runs one sweep pass over all tenants. A tenant's failure or panic is
// contained: the remaining tenants are still swept (tenant fault isolation). It
// stops STARTING new items once ctx is cancelled; an item already started is
// drained to completion (see SweeperItemTimeout).
func (s *Sweeper) RunPass(ctx context.Context, logger *slog.Logger, offset int) SweepPassStats {
	var st SweepPassStats
	tenants, err := sweepTenantIDs(ctx, s, offset)
	if err != nil {
		recordSweeperTenantFailure(ctx, "list")
		if logger != nil {
			logger.Error("payments sweeper: failed to list tenants", "error", err)
		}
		return st
	}
	st.Listed = true
	for _, tenantID := range tenants {
		if ctx.Err() != nil {
			break
		}
		st.Tenants++
		s.sweepTenant(ctx, logger, tenantID, &st)
	}
	return st
}

func (s *Sweeper) sweepTenant(ctx context.Context, logger *slog.Logger, tenantID uuid.UUID, st *SweepPassStats) {
	if ctx.Err() != nil {
		return // shutdown: do not claim on a dead context (H-CR-8)
	}
	s.reportScreeningParked(ctx, logger, tenantID, st)
	var ids []uuid.UUID
	if err := s.guarded(func() error {
		if s.testBeforeClaim != nil {
			s.testBeforeClaim(tenantID)
		}
		var err error
		ids, err = s.claimBatch(ctx, tenantID)
		return err
	}); err != nil {
		if ctx.Err() != nil && !errors.Is(err, errSweeperPanic) {
			return // cancelled mid-claim: not a failure
		}
		recordSweeperTenantFailure(ctx, "claim")
		st.Errors++
		if errors.Is(err, errSweeperPanic) {
			st.Panics++
		}
		if logger != nil {
			logger.Error("payments sweeper: claim batch failed", "tenant_id", tenantID, "error", err)
		}
		return
	}
	st.Claimed += len(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			// Shutdown: items already leased reappear when their lease expires
			// (the batch lease is the crash-recovery mechanism, not a claim).
			return
		}
		// Shutdown drain: the item runs on a context detached from the loop's cancellation.
		// The item context carries no deadline in normal operation; only once the loop's context is
		// cancelled does it get sweeperItemTimeout to drain (context.AfterFunc below).
		itemCtx, cancelItem := context.WithCancel(context.WithoutCancel(ctx))
		itemDone := make(chan struct{})
		stopWatch := context.AfterFunc(ctx, func() {
			select {
			case <-time.After(sweeperItemTimeout):
				cancelItem()
			case <-itemDone:
			}
		})
		err := s.guarded(func() error { return s.processAttempt(itemCtx, tenantID, id) })
		close(itemDone)
		stopWatch()
		cancelItem()
		switch {
		case err == nil:
			st.Processed++
			recordSweeperItem(ctx, "processed")
		case errors.Is(err, errSweeperPanic):
			st.Errors++
			st.Panics++
			recordSweeperItem(ctx, "panic")
			recordSweeperTenantFailure(ctx, "panic")
			if logger != nil {
				logger.Error("payments sweeper: recovered from panic", "tenant_id", tenantID, "attempt_id", id)
			}
		default:
			st.Errors++
			recordSweeperItem(ctx, "error")
			if logger != nil {
				logger.Error("payments sweeper: item failed", "tenant_id", tenantID, "attempt_id", id, "error", err)
			}
		}
	}
}

// guarded runs fn, converting a panic into errSweeperPanic. The loop runs in a
// bare goroutine with no recoverMiddleware, so an unrecovered panic would crash
// the whole platform process. The recovered value is deliberately NOT included in
// the error: it could carry provider or credential material.
func (s *Sweeper) guarded(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%w", errSweeperPanic)
		}
	}()
	return fn()
}

// RunSweeperLoop runs RunPass immediately and then on every interval until ctx
// is cancelled, then returns once the pass in flight has drained. A Sweeper that
// fails ValidateForLoop is refused at Error level and never started.
func RunSweeperLoop(ctx context.Context, s *Sweeper, logger *slog.Logger, interval time.Duration) {
	if interval <= 0 {
		interval = SweeperLoopDefaultInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	runSweeperLoop(ctx, s, logger, ticker.C, nil)
}

// runSweeperLoop is RunSweeperLoop with an injectable tick source (T-1 clock)
// and an optional after-pass callback, so tests drive passes deterministically.
func runSweeperLoop(ctx context.Context, s *Sweeper, logger *slog.Logger, ticks <-chan time.Time, afterPass func(SweepPassStats)) {
	if err := s.ValidateForLoop(); err != nil {
		if logger != nil {
			logger.Error("payments sweeper: refusing to start", "error", err)
		}
		return
	}
	if s.CredResolver == nil && logger != nil {
		logger.Error("payments sweeper: no outbound credential resolver wired; every provider call is refused fail-closed and in-flight attempts will not resolve")
	}
	pass := 0
	runOnce := func() {
		defer func() {
			if r := recover(); r != nil && logger != nil {
				logger.Error("payments sweeper: recovered from panic in pass")
			}
		}()
		st := s.RunPass(ctx, logger, pass)
		pass++
		if logger != nil && (st.ScreeningParked > 0 || st.Errors > 0) {
			// lic26 N-9: the pass summary is exported, counts only.
			logger.Info("payments sweeper: pass", "tenants", st.Tenants, "claimed", st.Claimed, "processed", st.Processed, "errors", st.Errors,
				"panics", st.Panics, "screening_parked", st.ScreeningParked, "screening_parked_overdue", st.ScreeningParkedOverdue,
				"screening_officer_queue", st.ScreeningParkedOfficer, "screening_officer_overdue", st.ScreeningParkedOfficerOverdue,
				"screening_scanned", st.ScreeningParkedScanned, "screening_partial", st.ScreeningParkedPartial,
				"screening_read_failures", st.ScreeningParkedReadFailures)
		}
		if st.Listed {
			// A pass that could not even list tenants did not sweep: leave the gauge so a stalled-sweeper rule fires (H-CR-10).
			sweeperLastPassUnix.Store(time.Now().Unix())
		}
		if sweeperPassesTotal != nil {
			sweeperPassesTotal.Add(ctx, 1)
		}
		if afterPass != nil {
			afterPass(st)
		}
	}
	runOnce()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			runOnce()
		}
	}
}
