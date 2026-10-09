//go:build integration

// PAY-PAYOUT-UNBOUND-HOLD-1 (Class-B B11; ledger-finance PO-1/PO-2, security
// D2-L1; ADR 0095 section 35.2).
//
// A payout attempt disputed (T10) with an UNBOUND reason
// (invalid_provider_reference:<reason>) must keep its withdrawal hold on EVERY
// automatic path. "Keep" means, for each path driven below:
//
//   - withdrawal_requests stays `submitted` with no release_ledger_transaction_id;
//   - no withdrawal_failed and no withdrawal_completed posting, and no other
//     ledger transaction at all;
//   - the wallet's player_withdrawal_hold, player_cash and psp_clearing balances
//     are unchanged, SUM(debits) = SUM(credits) and every projection equals its
//     rebuild;
//   - the attempt stays `disputed` with its ORIGINAL terminal reason (a later
//     "late_*" or second park must never overwrite it, because M2's allow-list
//     keys on that reason), with no schedule (next_action_at NULL) and no
//     escalation;
//   - no provider Withdraw call is made (no resend).
//
// Two parked shapes are exercised for every path:
//
//	sync   the REAL phase C park of a Withdraw result whose reference is
//	       malformed (attempt never binds a reference; payout.go ApplyPayoutResult)
//	poll   the REAL QueryStatus park of a pending payout that ALREADY holds a
//	       valid reference X whose poll returned a malformed echo (the attempt
//	       keeps X; payout.go applyPayoutStatusEvidenceInTx)
//
// The reverse collision (a payout settlement reference equal to an existing
// deposit ledger key, PAY-PAYOUT-REFBIND-1 / B10) is parked as
// provider_reference_conflict; it must keep the hold and never loop.
//
// MOCK provider throughout (a scripted wrapper around the MOCK adapter); one
// private scratch database; no migration.
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

const (
	b11AuditInvalidRef = "payments.payout_parked_invalid_reference"
	b11AuditConflict   = "payments.payout_parked_reference_conflict"
	b11AuditLate       = "payments.payout_late_contradicting_evidence"
	b11AuditEscalated  = "payments.payout_resend_escalated"
	b11AuditResolve    = "withdrawal.resolve_attempted.http"
	b11ReasonPrefix    = "invalid_provider_reference:"
)

func b11Bad() string { return "b11-bad\x01" + uuid.NewString()[:8] }
func b11Ref() string { return "b11-ref-" + uuid.NewString()[:12] }

// b11Snap is everything an automatic path must leave exactly as it found it.
type b11Snap struct {
	hold, cash, clearing                             int64
	ledgerTxs, failed, completed, rejected, reversed int
	wrState                                          withdrawal.State
	wrReleased                                       bool
	wrRef                                            string
	aState                                           AttemptState
	aReason, aRef                                    string
	aEscalated, aNextSet                             bool
	aSubmitCount                                     int
	withdrawCalls, depositCalls                      int64
}

func (w *k3World) b11Snap(wrID, attemptID uuid.UUID) b11Snap {
	w.t.Helper()
	wr := w.withdrawalOf(wrID)
	a := w.attempt(attemptID)
	s := b11Snap{
		hold: w.walletBalance("player_withdrawal_hold"), cash: w.walletBalance("player_cash"), clearing: w.pspClearing(),
		ledgerTxs: w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID),
		failed:    w.ledgerTxCount("withdrawal_failed"), completed: w.ledgerTxCount("withdrawal_completed"),
		rejected: w.ledgerTxCount("withdrawal_rejected"), reversed: w.ledgerTxCount("withdrawal_reversed"),
		wrState: wr.State, wrReleased: wr.ReleaseLedgerTransactionID != nil,
		aState: a.State, aEscalated: a.EscalatedAt != nil, aNextSet: a.NextActionAt != nil, aSubmitCount: a.SubmitCount,
		withdrawCalls: w.prov.calls[k3CallWithdraw].Load(), depositCalls: w.prov.calls[k3CallDeposit].Load(),
	}
	if wr.ProviderReference != nil {
		s.wrRef = *wr.ProviderReference
	}
	if a.TerminalReason != nil {
		s.aReason = *a.TerminalReason
	}
	if a.ProviderReference != nil {
		s.aRef = *a.ProviderReference
	}
	return s
}

// b11Parked is one payout parked with an unbound reason.
type b11Parked struct {
	shape string // "sync" | "poll"
	wr    withdrawal.WithdrawalRequest
	stale PaymentAttempt // the struct a worker held BEFORE the park (submitting / pending)
	fresh PaymentAttempt // re-read after the park (disputed)
	ref   string         // the bound reference X ("" for the sync shape)
	base  b11Snap
}

func (p *b11Parked) staleAs(state AttemptState) PaymentAttempt {
	a := p.stale
	a.State = state
	if state == AttemptAmbiguous {
		a.EverPossiblySent = true
		if a.SubmitCount < 1 {
			a.SubmitCount = 1
		}
	}
	return a
}

// staleSweeperLeased is a stale `submitting` copy as the sweeper's own batch
// claim leaves it (PollPayoutStatus's dispatch-in-flight guard exempts it).
func (p *b11Parked) staleSweeperLeased() PaymentAttempt {
	a := p.staleAs(AttemptSubmitting)
	owner := SweeperBatchLeaseOwner
	a.LeaseOwner = &owner
	a.LeaseUntil = nil
	return a
}

func (w *k3World) b11Claim(amount int64) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	w.t.Helper()
	w.ensureWithdrawalPolicy()
	wr := w.approveWithdrawal(amount, "b11-"+uuid.NewString())
	claim, err := w.orch.ClaimForDispatch(context.Background(), w.pool, KYCEnforcementPayoutGate{}, w.f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		w.t.Fatalf("ClaimForDispatch: %v", err)
	}
	return wr, claim.Attempt
}

func (w *k3World) b11Dispatch(att PaymentAttempt, script func(WithdrawRequest) WithdrawResult) GateResult[WithdrawResult] {
	w.t.Helper()
	w.prov.setWithdraw(script)
	defer w.prov.setWithdraw(nil)
	adapter, ok := w.orch.Provider(*att.ProviderID)
	if !ok {
		w.t.Fatalf("provider %s not registered", *att.ProviderID)
	}
	return DispatchWithdraw(context.Background(), w.pool, MockCredentialResolver{}, adapter, att, WithDestinations(pitest.Shared()))
}

func (w *k3World) b11Finish(shape string, wr withdrawal.WithdrawalRequest, stale PaymentAttempt, ref string) *b11Parked {
	w.t.Helper()
	fresh := w.attempt(stale.ID)
	if fresh.State != AttemptDisputed || fresh.TerminalReason == nil || !strings.HasPrefix(*fresh.TerminalReason, b11ReasonPrefix) {
		w.t.Fatalf("setup (%s): want disputed/%s*, got state=%s reason=%v", shape, b11ReasonPrefix, fresh.State, fresh.TerminalReason)
	}
	if shape == "sync" && fresh.ProviderReference != nil {
		w.t.Fatalf("setup: the sync park must not bind a reference, got %q", *fresh.ProviderReference)
	}
	if shape == "poll" && (fresh.ProviderReference == nil || *fresh.ProviderReference != ref) {
		w.t.Fatalf("setup: the poll park must keep the bound reference %q, got %v", ref, fresh.ProviderReference)
	}
	if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		w.t.Fatalf("setup: the park must keep the hold, got state=%s release=%v", got.State, got.ReleaseLedgerTransactionID)
	}
	if hold := w.walletBalance("player_withdrawal_hold"); hold < wr.Amount {
		w.t.Fatalf("setup: hold balance %d does not cover the withdrawal %d (vacuity)", hold, wr.Amount)
	}
	return &b11Parked{shape: shape, wr: wr, stale: stale, fresh: fresh, ref: ref, base: w.b11Snap(wr.ID, stale.ID)}
}

// parkSync is the REAL phase C park: Withdraw returns `outcome` with a malformed
// reference (payoutAdapterCall validates the reference before it looks at the
// outcome, so a "paid" or "declined" answer with a hostile reference parks too).
func (w *k3World) b11ParkSync(amount int64, outcome Outcome) *b11Parked {
	w.t.Helper()
	wr, att := w.b11Claim(amount)
	gr := w.b11Dispatch(att, func(WithdrawRequest) WithdrawResult {
		return WithdrawResult{Outcome: outcome, ProviderReference: b11Bad(), DeclineReason: "insufficient_funds"}
	})
	if gr.Class != ErrorClassProviderRefInvalid {
		w.t.Fatalf("setup: want class %s, got %s", ErrorClassProviderRefInvalid, gr.Class)
	}
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, att, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		w.t.Fatalf("park (sync): %v", err)
	}
	return w.b11Finish("sync", wr, att, "")
}

// parkPoll is the REAL QueryStatus park of a pending payout that holds X.
func (w *k3World) b11ParkPoll(amount int64, outcome Outcome) *b11Parked {
	w.t.Helper()
	wr, pend := w.payout(amount)
	x := *pend.ProviderReference
	w.prov.setStatus(x, StatusResult{Outcome: outcome, ProviderReference: b11Bad(), Amount: wr.Amount, AssetCode: "EUR", DeclineReason: "insufficient_funds"})
	if err := PollPayoutStatus(context.Background(), w.pool, w.orch, MockCredentialResolver{}, w.f.tenantID, pend, time.Now().Add(time.Minute), nil); err != nil {
		w.t.Fatalf("park (poll): %v", err)
	}
	// From here the provider reports the payout PAID under X: every later poll of
	// X must still not release the hold.
	w.prov.setStatus(x, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: x, Amount: wr.Amount, AssetCode: "EUR"})
	return w.b11Finish("poll", wr, pend, x)
}

func (w *k3World) b11Shapes() []struct {
	name string
	park func(amount int64, outcome Outcome) *b11Parked
} {
	return []struct {
		name string
		park func(amount int64, outcome Outcome) *b11Parked
	}{{"sync", w.b11ParkSync}, {"poll", w.b11ParkPoll}}
}

// held asserts every "the hold is kept" property against the post-park baseline.
func (w *k3World) b11Held(p *b11Parked, what string) {
	w.t.Helper()
	now := w.b11Snap(p.wr.ID, p.stale.ID)
	b := p.base
	if now != b {
		w.t.Fatalf("%s: state moved after the park\n before: %+v\n  after: %+v", what, b, now)
	}
	if now.wrState != withdrawal.StateSubmitted || now.wrReleased {
		w.t.Fatalf("%s: withdrawal not held: %+v", what, now)
	}
	if now.aState != AttemptDisputed || !strings.HasPrefix(now.aReason, b11ReasonPrefix) || now.aNextSet || now.aEscalated {
		w.t.Fatalf("%s: attempt changed: %+v", what, now)
	}
	if now.withdrawCalls != b.withdrawCalls {
		w.t.Fatalf("%s: a provider Withdraw call was made (resend)", what)
	}
	w.assertInvariants()
}

func (w *k3World) b11Audits(action string, targetID uuid.UUID) int {
	return w.countRows(`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`, w.f.tenantID, action, targetID.String())
}

func (w *k3World) b11Sweeper() *Sweeper {
	return &Sweeper{Pool: w.pool, Orchestrator: w.orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
}

// b11OKOrConflict accepts a clean no-op, a CAS conflict, or (a definite success
// that carries no reference anywhere, possible only for the never-bound shape)
// ErrInvalidPayoutEvidence: all three roll back or write nothing, so the hold is
// kept; the caller's b11Held asserts that.
func b11OKOrConflict(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil && !errors.Is(err, ErrAttemptStateConflict) && !errors.Is(err, ErrInvalidPayoutEvidence) {
		t.Fatalf("%s: want nil, ErrAttemptStateConflict or ErrInvalidPayoutEvidence, got %v", what, err)
	}
}

// ---------------------------------------------------------------------------
// 1. The park itself: normal path, every outcome, retry, audit.

func TestB11_Park_KeepsHold_EveryOutcome_BothShapes(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		for _, oc := range []Outcome{OutcomePending, OutcomeSucceeded, OutcomeDeclined} {
			t.Run(fmt.Sprintf("%s/%s", shape.name, oc), func(t *testing.T) {
				p := shape.park(300, oc)
				// The provider said "succeeded" or "declined" and the reference was
				// hostile: nothing may complete or fail.
				w.b11Held(p, "after park")
				if got := w.b11Audits(b11AuditInvalidRef, p.stale.ID); got != 1 {
					t.Fatalf("want exactly one park audit, got %d", got)
				}
			})
		}
	}
}

// The park writes one audit row (denied, closed reason, withdrawal id) and never
// the hostile reference text.
func TestB11_Park_Audit_OneRow_NoHostileText(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			rows := w.sysQuery(`SELECT outcome, actor_type, metadata::text AS md FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
				w.f.tenantID, b11AuditInvalidRef, p.stale.ID.String())
			if len(rows) != 1 {
				t.Fatalf("want one audit row, got %d", len(rows))
			}
			md := rows[0]["md"].(string)
			if rows[0]["outcome"] != "denied" || rows[0]["actor_type"] != "system" {
				t.Fatalf("audit outcome/actor: %v / %v", rows[0]["outcome"], rows[0]["actor_type"])
			}
			for _, want := range []string{p.wr.ID.String(), b11ReasonPrefix + string(providerref.ReasonControlChar)} {
				if !strings.Contains(md, want) {
					t.Fatalf("audit metadata %s lacks %q", md, want)
				}
			}
			if strings.Contains(md, "b11-bad") || strings.Contains(md, `\u0001`) {
				t.Fatalf("audit metadata leaks the hostile reference: %s", md)
			}
		})
	}
}

// Retry: the same park evidence delivered again (a sweeper retry, a redelivered
// result) commits without error, writes no second audit row and does not
// overwrite the reason.
func TestB11_Park_Retry_Idempotent_OneAudit(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	p := w.b11ParkSync(300, OutcomePending)
	gr := GateResult[WithdrawResult]{Class: ErrorClassProviderRefInvalid, Err: providerref.ValidatePaymentReference("withdraw.provider_reference", b11Bad())}
	for i := 0; i < 3; i++ {
		if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, p.wr.ID, p.stale, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
	}
	// A DIFFERENT closed reason arriving later must not replace the first one.
	gr2 := GateResult[WithdrawResult]{Class: ErrorClassProviderRefInvalid, Err: providerref.ValidatePaymentReference("withdraw.provider_reference", strings.Repeat("x", providerref.MaxBytes+1))}
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, p.wr.ID, p.stale, gr2, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("second reason: %v", err)
	}
	w.b11Held(p, "after retries")
	if got := w.b11Audits(b11AuditInvalidRef, p.stale.ID); got != 1 {
		t.Fatalf("retries must not re-audit: %d rows", got)
	}
}

// Error + hostile reference together (adapter returned an error AND a bad
// reference): payoutAdapterCall validates first, so it parks, never "ambiguous".
func TestB11_Park_AdapterErrorWithHostileReference_Parks(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, att := w.b11Claim(300)
	bad := b11Bad()
	adapter := &b11ErrProvider{MockProvider: w.mock, ref: bad}
	res, class, err := payoutAdapterCall(adapter, att, resolvedDestination{paymentMethod: att.PaymentMethod})(context.Background(), CallContext{})
	if class != ErrorClassProviderRefInvalid || err == nil {
		t.Fatalf("want ProviderRefInvalid with an error, got %s / %v", class, err)
	}
	if res.ProviderReference != "" {
		t.Fatalf("the hostile reference must be scrubbed from the result, got %q", res.ProviderReference)
	}
	if e := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, att, GateResult[WithdrawResult]{Value: res, Class: class, Err: err}, EvidenceSync, WithDestinations(pitest.Shared())); e != nil {
		t.Fatal(e)
	}
	p := w.b11Finish("sync", wr, att, "")
	w.b11Held(p, "adapter error + hostile ref")
}

type b11ErrProvider struct {
	*MockProvider
	ref string
}

func (p *b11ErrProvider) Withdraw(context.Context, WithdrawRequest) (WithdrawResult, error) {
	return WithdrawResult{Outcome: OutcomePending, ProviderReference: p.ref}, errors.New("transport reset")
}

// ---------------------------------------------------------------------------
// 2. Every automatic path.

// Sweeper: a disputed attempt is never scheduled (the table's CHECK) and so is
// never claimed; the batch pass makes no provider call and moves nothing.
func TestB11_Auto_Sweeper_RunOnce_And_ProcessFresh(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			// The CHECK that keeps it unschedulable (payment_attempts_check9).
			err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, `UPDATE payment_attempts SET next_action_at = now() WHERE id = $1`, p.stale.ID)
				return e
			})
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != "23514" {
				t.Fatalf("scheduling a disputed attempt must violate the CHECK (23514), got %v", err)
			}
			calls := w.prov.callTotal()
			stats := w.b11Sweeper().RunOnce(context.Background(), []uuid.UUID{w.f.tenantID})
			if stats.Claimed != 0 || len(stats.Errors) != 0 {
				t.Fatalf("the sweeper must claim nothing: %+v", stats)
			}
			if err := w.b11Sweeper().processPayoutAttempt(context.Background(), w.f.tenantID, p.fresh); err != nil {
				t.Fatalf("processPayoutAttempt(disputed): %v", err)
			}
			if w.prov.callTotal() != calls {
				t.Fatalf("a provider call was made for a disputed payout")
			}
			w.b11Held(p, "sweeper")
		})
	}
}

// A sweeper that claimed the attempt BEFORE the park landed still holds a stale
// pending/submitting copy; every route that copy takes must end without a release.
func TestB11_Auto_Sweeper_StaleCopy_QueryStatus(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			s := w.b11Sweeper()
			stale := p.stale
			if shape.name == "sync" {
				stale = p.staleSweeperLeased() // no reference: the no-reference fallback branch
			}
			// For the poll shape the provider reports it PAID under X (set by the park).
			err := s.processPayoutAttempt(context.Background(), w.f.tenantID, stale)
			b11OKOrConflict(t, err, "processPayoutAttempt(stale)")
			w.b11Held(p, "stale sweeper copy")
			if got := w.b11Audits(b11AuditLate, p.stale.ID); got != 0 {
				t.Fatalf("a benign replay on a disputed attempt writes no late-evidence audit, got %d", got)
			}
		})
	}
}

// The QueryStatus evidence matrix, applied from every stale state: no class of
// poll result (success, decline, pending, ambiguous, transport error, a second
// malformed reference) may release the hold or change the first reason.
func TestB11_Auto_QueryStatus_Matrix_FromEveryStaleState(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			okRef := p.ref
			if okRef == "" {
				okRef = b11Ref()
			}
			full := func(oc Outcome, ref string) StatusResult {
				return StatusResult{Outcome: oc, ProviderReference: ref, Amount: p.wr.Amount, AssetCode: "EUR", DeclineReason: "insufficient_funds"}
			}
			hostile := providerref.ValidatePaymentReference("query_status.provider_reference", b11Bad())
			cases := []struct {
				name string
				gr   GateResult[StatusResult]
			}{
				{"success_exact", GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: full(OutcomeSucceeded, okRef)}},
				{"success_empty_echo", GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: full(OutcomeSucceeded, "")}},
				{"success_amount_mismatch", GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: StatusResult{Outcome: OutcomeSucceeded, ProviderReference: okRef, Amount: p.wr.Amount + 1, AssetCode: "EUR"}}},
				{"success_other_ref", GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: full(OutcomeSucceeded, b11Ref())}},
				{"decline", GateResult[StatusResult]{Class: ErrorClassDefiniteDecline, Value: full(OutcomeDeclined, okRef)}},
				{"pending", GateResult[StatusResult]{Class: ErrorClassPending, Value: full(OutcomePending, okRef)}},
				{"ambiguous", GateResult[StatusResult]{Class: ErrorClassAmbiguous, Value: full(OutcomePending, okRef)}},
				{"transport_error", GateResult[StatusResult]{Class: ErrorClassAmbiguous, Err: errors.New("read timeout")}},
				{"second_hostile_reference", GateResult[StatusResult]{Class: ErrorClassProviderRefInvalid, Err: hostile}},
			}
			for _, from := range []AttemptState{AttemptSubmitting, AttemptPending, AttemptAmbiguous} {
				for _, c := range cases {
					stale := p.staleAs(from)
					err := applyPayoutStatusEvidence(context.Background(), w.pool, w.f.tenantID, p.wr.ID, stale, c.gr, EvidenceQueryStatus, time.Now().Add(time.Minute), nil, WithDestinations(pitest.Shared()))
					b11OKOrConflict(t, err, fmt.Sprintf("%s from %s", c.name, from))
					w.b11Held(p, fmt.Sprintf("status %s from stale %s", c.name, from))
				}
			}
			if got := w.b11Audits(b11AuditInvalidRef, p.stale.ID); got != 1 {
				t.Fatalf("a second hostile reference must not re-audit the park: %d rows", got)
			}
		})
	}
}

// Polling through PollPayoutStatus itself (the shared sweeper / staff /resolve
// function): fresh disputed copy, stale copy, with and without a staff actor.
func TestB11_Auto_PollPayoutStatus_FreshAndStale_WithAndWithoutActor(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			actor := testSubmitActor()
			for _, c := range []struct {
				name  string
				stale PaymentAttempt
				actor *SubmitActor
			}{
				{"fresh_no_actor", p.fresh, nil}, {"fresh_staff", p.fresh, &actor},
				{"stale_pending_no_actor", p.staleAs(AttemptPending), nil}, {"stale_pending_staff", p.staleAs(AttemptPending), &actor},
				{"stale_ambiguous", p.staleAs(AttemptAmbiguous), nil},
				{"stale_sweeper_submitting", p.staleSweeperLeased(), nil},
			} {
				err := PollPayoutStatus(context.Background(), w.pool, w.orch, MockCredentialResolver{}, w.f.tenantID, c.stale, time.Now().Add(time.Minute), c.actor)
				b11OKOrConflict(t, err, c.name)
				w.b11Held(p, c.name)
			}
		})
	}
}

// T12: a resend attempt for the (stale) ambiguous copy polls first, sees the
// attempt is no longer ambiguous and stops. The manifest says resend is
// allowed and the cap is far away, so only the state check can stop it.
func TestB11_Auto_T12_Resend_NeverHappens(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	m := w.mock.Capabilities().Manifest
	m.IdempotentSubmission = true
	w.mock.SetManifest(m)
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			s := w.b11Sweeper()
			s.MaxResubmits = 50
			for _, a := range []PaymentAttempt{p.staleAs(AttemptAmbiguous), p.fresh} {
				err := s.resubmitPayoutAmbiguous(context.Background(), w.f.tenantID, a)
				b11OKOrConflict(t, err, "resubmitPayoutAmbiguous")
				w.b11Held(p, "T12 from "+string(a.State))
			}
			// The CAS itself, and the dispatch tail a resend would reach.
			err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return ResubmitAmbiguous(ctx, tx, p.stale.ID, uuid.New(), "b11", time.Now().Add(time.Minute), 50)
			})
			if !errors.Is(err, ErrAttemptStateConflict) {
				t.Fatalf("T12 CAS on a disputed attempt: want ErrAttemptStateConflict, got %v", err)
			}
			w.b11Held(p, "T12 CAS")
		})
	}
}

// T2 re-claim of a "created" attempt never applies to a disputed one.
func TestB11_Auto_T2_Reclaim_Refused(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			err := w.b11Sweeper().reclaimPayoutCreated(context.Background(), w.f.tenantID, p.fresh)
			if !errors.Is(err, ErrAttemptStateConflict) {
				t.Fatalf("T2 on a disputed attempt: want ErrAttemptStateConflict, got %v", err)
			}
			w.b11Held(p, "T2")
		})
	}
}

// Synchronous Withdraw evidence arriving after the park (a resend that was in
// flight, a late phase C): success, decline, pending, ambiguous, not-sent, a
// second malformed reference.
func TestB11_Auto_SyncEvidence_AfterPark(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			ref := p.ref
			if ref == "" {
				ref = b11Ref()
			}
			wres := func(oc Outcome, r string) WithdrawResult {
				return WithdrawResult{Outcome: oc, ProviderReference: r, DeclineReason: "insufficient_funds"}
			}
			hostile := providerref.ValidatePaymentReference("withdraw.provider_reference", b11Bad())
			cases := []struct {
				name string
				gr   GateResult[WithdrawResult]
				ever bool
			}{
				{"succeeded_own_ref", GateResult[WithdrawResult]{Class: ErrorClassSucceeded, Value: wres(OutcomeSucceeded, ref)}, false},
				{"succeeded_fresh_ref", GateResult[WithdrawResult]{Class: ErrorClassSucceeded, Value: wres(OutcomeSucceeded, b11Ref())}, false},
				{"succeeded_no_ref", GateResult[WithdrawResult]{Class: ErrorClassSucceeded, Value: wres(OutcomeSucceeded, "")}, false},
				{"declined", GateResult[WithdrawResult]{Class: ErrorClassDefiniteDecline, Value: wres(OutcomeDeclined, "")}, false},
				{"declined_with_ref", GateResult[WithdrawResult]{Class: ErrorClassDefiniteDecline, Value: wres(OutcomeDeclined, ref)}, false},
				{"pending_fresh_ref", GateResult[WithdrawResult]{Class: ErrorClassPending, Value: wres(OutcomePending, b11Ref())}, false},
				{"ambiguous", GateResult[WithdrawResult]{Class: ErrorClassAmbiguous, Value: wres(OutcomePending, "")}, false},
				{"not_sent", GateResult[WithdrawResult]{Class: ErrorClassNotSent, Value: wres(OutcomePending, "")}, false},
				{"not_sent_on_resend", GateResult[WithdrawResult]{Class: ErrorClassNotSent, Value: wres(OutcomePending, "")}, true},
				{"second_hostile_reference", GateResult[WithdrawResult]{Class: ErrorClassProviderRefInvalid, Err: hostile}, false},
			}
			for _, from := range []AttemptState{AttemptSubmitting, AttemptPending, AttemptAmbiguous} {
				for _, c := range cases {
					stale := p.staleAs(from)
					stale.EverPossiblySent = stale.EverPossiblySent || c.ever
					err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, p.wr.ID, stale, c.gr, EvidenceSync, WithDestinations(pitest.Shared()))
					b11OKOrConflict(t, err, fmt.Sprintf("%s from %s", c.name, from))
					w.b11Held(p, fmt.Sprintf("sync %s from stale %s", c.name, from))
				}
			}
		})
	}
}

// Verified callbacks (success, decline, pending, mismatched success), delivered
// twice, by merchant reference, by the bound reference and with a fresh one.
func TestB11_Auto_Callbacks_Success_Decline_Pending_Redelivered(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			refs := []string{b11Ref()}
			if p.ref != "" {
				refs = append(refs, p.ref)
			}
			for _, ref := range refs {
				for _, oc := range []Outcome{OutcomeSucceeded, OutcomeDeclined, OutcomePending} {
					for _, amt := range []int64{p.wr.Amount, p.wr.Amount - 1} {
						for delivery := 0; delivery < 2; delivery++ {
							ev := ReceiptEvidence{
								EventType: "payout", ProviderReference: ref, MerchantReference: p.stale.MerchantReference,
								Outcome: oc, Amount: amt, AssetCode: "EUR", DeclineReason: "insufficient_funds",
							}
							var disp ReceiptDisposition
							if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
								var e error
								disp, e = ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, w.provider, ev)
								return e
							}); err != nil {
								t.Fatalf("callback %s ref=%s amt=%d delivery=%d: %v", oc, ref, amt, delivery, err)
							}
							if disp == DispositionApplied {
								t.Fatalf("a callback on a disputed unbound payout must not be 'applied' (%s)", oc)
							}
							w.b11Held(p, fmt.Sprintf("callback %s ref=%s amt=%d delivery=%d (%s)", oc, ref, amt, delivery, disp))
						}
					}
				}
			}
		})
	}
}

// Late evidence router, called directly for every reason string payout.go uses.
func TestB11_Auto_LateEvidence_NoOp_NoAudit_ReasonKept(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			for _, reason := range []string{"late_success_after_terminal", "late_decline_after_terminal", "late_contradicting_evidence"} {
				for _, from := range []AttemptState{AttemptSubmitting, AttemptPending, AttemptAmbiguous} {
					if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
						return applyPayoutLateEvidence(ctx, tx, p.staleAs(from), EvidenceCallback, reason, p.wr.ID, OutcomeSucceeded, "")
					}); err != nil {
						t.Fatalf("%s from %s: %v", reason, from, err)
					}
					w.b11Held(p, reason)
				}
			}
			if got := w.b11Audits(b11AuditLate, p.stale.ID); got != 0 {
				t.Fatalf("late evidence on a disputed attempt is a benign no-op, got %d audit rows", got)
			}
		})
	}
}

// Escalation (T16) and expiry-style rescheduling: the stale ambiguous copy that
// gives up on resend escalates; on a disputed attempt that is a no-op without
// an audit row, and the escalation CAS itself refuses.
func TestB11_Auto_Escalation_NoOp(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			for _, reason := range []string{"non_idempotent_manifest", "max_resubmits_exhausted", "resubmit_cas_refused"} {
				if err := w.b11Sweeper().escalateAmbiguousPayout(context.Background(), w.f.tenantID, p.staleAs(AttemptAmbiguous), time.Now().Add(time.Minute), reason); err != nil {
					t.Fatalf("escalate (%s): %v", reason, err)
				}
				w.b11Held(p, "escalate "+reason)
			}
			if got := w.b11Audits(b11AuditEscalated, p.stale.ID); got != 0 {
				t.Fatalf("no escalation audit may be written for a disputed attempt, got %d", got)
			}
		})
	}
}

// Every attempt-state writer refuses a disputed attempt, and the database guard
// refuses the raw transitions (defence in depth under the Go routing above).
func TestB11_Auto_AttemptWriters_AllRefuseDisputed(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			id := p.stale.ID
			next := time.Now().Add(time.Minute)
			cascadable := false
			writers := map[string]func(ctx context.Context, tx pgx.Tx) error{
				"Touch (T17)":           func(ctx context.Context, tx pgx.Tx) error { return Touch(ctx, tx, id) },
				"Escalate (T16)":        func(ctx context.Context, tx pgx.Tx) error { return Escalate(ctx, tx, id, next) },
				"RescheduleNonTerminal": func(ctx context.Context, tx pgx.Tx) error { return RescheduleNonTerminal(ctx, tx, id, next) },
				"ClaimCreated (T2)": func(ctx context.Context, tx pgx.Tx) error {
					return ClaimCreatedForSubmission(ctx, tx, id, w.provider, uuid.New(), "b11", next)
				},
				"MarkNotSent (T5)": func(ctx context.Context, tx pgx.Tx) error { return MarkNotSent(ctx, tx, id, uuid.New(), next) },
				"MarkAccepted (T4)": func(ctx context.Context, tx pgx.Tx) error {
					return MarkAccepted(ctx, tx, id, EvidenceCallback, b11Ref(), next)
				},
				"AmbiguousFromPending": func(ctx context.Context, tx pgx.Tx) error {
					return MarkAmbiguousFromPending(ctx, tx, id, EvidenceSweeper, next)
				},
				"AmbiguousFromSubmitting": func(ctx context.Context, tx pgx.Tx) error {
					return MarkAmbiguousFromSubmitting(ctx, tx, id, EvidenceSweeper, next)
				},
				"ApplySuccess (T7)": func(ctx context.Context, tx pgx.Tx) error {
					return ApplySuccess(ctx, tx, id, SuccessEvidence{Evidence: EvidenceCallback, ProviderReference: b11Ref()})
				},
				"ApplyDecline (T8)": func(ctx context.Context, tx pgx.Tx) error {
					return ApplyDecline(ctx, tx, id, DeclineEvidence{Evidence: EvidenceCallback, Reason: "provider_declined", Stage: DeclineAtSubmission, Cascadable: &cascadable})
				},
				"DisputeFromNonTerminal (second park)": func(ctx context.Context, tx pgx.Tx) error {
					return ApplyDisputeFromNonTerminal(ctx, tx, id, EvidenceCallback, "provider_reference_mismatch")
				},
				"DisputeFromNeverSent": func(ctx context.Context, tx pgx.Tx) error {
					return ApplyDisputeFromNeverSent(ctx, tx, id, EvidenceCallback, "success_for_never_sent_attempt")
				},
				"DisputeFromDeclinedPayout": func(ctx context.Context, tx pgx.Tx) error {
					return ApplyDisputeFromDeclinedPayout(ctx, tx, id, EvidenceCallback, "success_after_payout_declined")
				},
				"TombstonePrecedesSuccess": func(ctx context.Context, tx pgx.Tx) error {
					return ApplyTombstonePrecedesSuccess(ctx, tx, id, EvidenceCallback)
				},
			}
			for name, fn := range writers {
				err := w.pool.WithTenant(context.Background(), w.f.tenantID, fn)
				if !errors.Is(err, ErrAttemptStateConflict) {
					t.Errorf("%s on a disputed attempt: want ErrAttemptStateConflict, got %v", name, err)
				}
			}
			// Raw SQL: the guard trigger refuses disputed -> anything.
			for _, sql := range []string{
				`UPDATE payment_attempts SET state = 'succeeded', resolved_at = now() WHERE id = $1`,
				`UPDATE payment_attempts SET state = 'declined', resolved_at = now() WHERE id = $1`,
				`UPDATE payment_attempts SET state = 'pending', next_action_at = now() WHERE id = $1`,
			} {
				err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					_, e := tx.Exec(ctx, sql, id)
					return e
				})
				if err == nil {
					t.Errorf("raw %q on a disputed attempt succeeded", sql)
				}
			}
			w.b11Held(p, "writers")
		})
	}
}

// ---------------------------------------------------------------------------
// 3. M2 force-resolution refuses the unbound reasons: Go and database.

func TestB11_M2_Refused_GoAndDB_EveryUnboundReason(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	reasons := []string{"invalid_provider_reference"}
	for _, r := range []providerref.Reason{providerref.ReasonEmpty, providerref.ReasonTooLong, providerref.ReasonInvalidUTF8, providerref.ReasonControlChar, providerref.ReasonReservedNamespace} {
		reasons = append(reasons, b11ReasonPrefix+string(r))
	}
	// Go allow-list.
	for _, r := range reasons {
		r := r
		if M2ResolvableDispute(AttemptDisputed, &r) {
			t.Errorf("M2ResolvableDispute admits %q", r)
		}
	}
	// A control: the probe is not vacuous - an admitted reason IS admitted.
	admitted := "provider_reference_mismatch"
	if !M2ResolvableDispute(AttemptDisputed, &admitted) {
		t.Fatalf("control: Go allow-list must admit %q", admitted)
	}
	// Database allow-list (payment_m2_admits). The function requires an EXECUTING
	// resolution whose target_state equals the new state and whose kind is
	// m2_declare_paid for 'succeeded' and m2_declare_not_paid for 'declined'
	// (declare-not-paid is the kind that runs withdrawal.Fail, i.e. releases the
	// hold), so each new state is probed under its OWN resolution; a probe under
	// the wrong kind would return false for every reason and prove nothing.
	_, aPaid := w.ambiguousPayout(120)
	rPaid := w.mustRequest(w.f1, w.m2In(aPaid.ID, ResolutionM2DeclarePaid))
	_, aNot := w.ambiguousPayout(121)
	rNot := w.mustRequest(w.f1, w.m2In(aNot.ID, ResolutionM2DeclareNotPaid))
	probe := func(a PaymentAttempt, r ManualResolution, reason, newState string) bool {
		var got bool
		if err := w.inExecuting(r, w.f2, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT payment_m2_admits($1, 'disputed', $2, $3, $4, 'operator')`, a.ID, reason, *a.WithdrawalRequestID, newState).Scan(&got)
		}); err != nil {
			t.Fatalf("db probe: %v", err)
		}
		return got
	}
	// POSITIVE CONTROLS: an admitted reason IS admitted for each state under its own kind.
	if !probe(aPaid, rPaid, admitted, "succeeded") {
		t.Fatalf("control: payment_m2_admits must admit %q -> succeeded under m2_declare_paid", admitted)
	}
	if !probe(aNot, rNot, admitted, "declined") {
		t.Fatalf("control: payment_m2_admits must admit %q -> declined under m2_declare_not_paid", admitted)
	}
	for _, reason := range reasons {
		if probe(aPaid, rPaid, reason, "succeeded") {
			t.Errorf("payment_m2_admits admits %q -> succeeded", reason)
		}
		if probe(aNot, rNot, reason, "declined") {
			t.Errorf("payment_m2_admits admits %q -> declined (the hold-releasing kind)", reason)
		}
	}
	// End to end through the service, both kinds, both shapes: refused (MR012),
	// hold untouched.
	for _, shape := range w.b11Shapes() {
		t.Run(shape.name, func(t *testing.T) {
			p := shape.park(300, OutcomePending)
			for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
				_, err := w.request(w.f1, w.m2In(p.stale.ID, kind))
				k3RequireCode(t, err, "MR012")
				w.b11Held(p, "M2 "+string(kind))
			}
		})
	}
}

// ---------------------------------------------------------------------------
// 4. Rollback and partial failure.

// The park is one transaction: a failure after the dispute write rolls the whole
// of it back (attempt back to submitting, hold intact), and the real apply then
// parks once.
func TestB11_Rollback_PartialPark_LeavesNothing(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, att := w.b11Claim(300)
	before := w.b11Snap(wr.ID, att.ID)
	boom := errors.New("injected failure after the dispute write")
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if e := ApplyDisputeFromNonTerminal(ctx, tx, att.ID, EvidenceSync, b11ReasonPrefix+string(providerref.ReasonControlChar)); e != nil {
			return e
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("want the injected error, got %v", err)
	}
	if now := w.b11Snap(wr.ID, att.ID); now != before || now.aState != AttemptSubmitting {
		t.Fatalf("the rolled-back park left traces: %+v vs %+v", now, before)
	}
	if got := w.b11Audits(b11AuditInvalidRef, att.ID); got != 0 {
		t.Fatalf("a rolled-back park left %d audit rows", got)
	}
	gr := w.b11Dispatch(att, func(WithdrawRequest) WithdrawResult {
		return WithdrawResult{Outcome: OutcomePending, ProviderReference: b11Bad()}
	})
	if e := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, att, gr, EvidenceSync, WithDestinations(pitest.Shared())); e != nil {
		t.Fatal(e)
	}
	p := w.b11Finish("sync", wr, att, "")
	w.b11Held(p, "park after rollback")
	if got := w.b11Audits(b11AuditInvalidRef, att.ID); got != 1 {
		t.Fatalf("want one audit row, got %d", got)
	}
}

// A caller bug (attempt of another withdrawal) fails before any write.
func TestB11_PartialFailure_WrongWithdrawal_Refused_NoWrite(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wrB, attB := w.b11Claim(200)
	p := w.b11ParkSync(300, OutcomePending)
	before := w.b11Snap(wrB.ID, attB.ID)
	gr := GateResult[WithdrawResult]{Class: ErrorClassProviderRefInvalid, Err: providerref.ValidatePaymentReference("w", b11Bad())}
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wrB.ID, p.stale, gr, EvidenceSync, WithDestinations(pitest.Shared())); err == nil {
		t.Fatalf("applying attempt A's evidence to withdrawal B must be refused")
	}
	if now := w.b11Snap(wrB.ID, attB.ID); now != before {
		t.Fatalf("withdrawal B moved: %+v vs %+v", now, before)
	}
	w.b11Held(p, "wrong withdrawal")
}

// ---------------------------------------------------------------------------
// 5. Concurrency: a park racing a definite outcome.

func TestB11_Concurrency_ParkRacingDefiniteOutcome(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	for _, outcome := range []string{"success", "decline"} {
		for i := 0; i < 4; i++ {
			wr, att := w.b11Claim(150)
			park := GateResult[WithdrawResult]{Class: ErrorClassProviderRefInvalid, Err: providerref.ValidatePaymentReference("w", b11Bad())}
			var other GateResult[WithdrawResult]
			if outcome == "success" {
				other = GateResult[WithdrawResult]{Class: ErrorClassSucceeded, Value: WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: b11Ref()}}
			} else {
				other = GateResult[WithdrawResult]{Class: ErrorClassDefiniteDecline, Value: WithdrawResult{Outcome: OutcomeDeclined, DeclineReason: "insufficient_funds"}}
			}
			start := make(chan struct{})
			errs := make([]error, 2)
			var wg sync.WaitGroup
			for j, gr := range []GateResult[WithdrawResult]{park, other} {
				wg.Add(1)
				go func(j int, gr GateResult[WithdrawResult]) {
					defer wg.Done()
					<-start
					errs[j] = ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, att, gr, EvidenceSync, WithDestinations(pitest.Shared()))
				}(j, gr)
			}
			close(start)
			wg.Wait()
			for j, e := range errs {
				if e != nil {
					t.Fatalf("%s race %d, racer %d: %v", outcome, i, j, e)
				}
			}
			a, got := w.attempt(att.ID), w.withdrawalOf(wr.ID)
			switch a.State {
			case AttemptDisputed:
				// The park won: the hold is kept whatever the other racer said.
				if got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
					t.Fatalf("%s race %d: disputed attempt but withdrawal %s released=%v", outcome, i, got.State, got.ReleaseLedgerTransactionID != nil)
				}
				if a.TerminalReason == nil || !strings.HasPrefix(*a.TerminalReason, b11ReasonPrefix) {
					t.Fatalf("%s race %d: reason %v", outcome, i, a.TerminalReason)
				}
			case AttemptSucceeded:
				if outcome != "success" || got.State != withdrawal.StateCompleted {
					t.Fatalf("%s race %d: succeeded attempt, withdrawal %s", outcome, i, got.State)
				}
			case AttemptDeclined:
				if outcome != "decline" || got.State != withdrawal.StateFailed {
					t.Fatalf("%s race %d: declined attempt, withdrawal %s", outcome, i, got.State)
				}
			default:
				t.Fatalf("%s race %d: unexpected attempt state %s", outcome, i, a.State)
			}
		}
	}
	w.assertInvariants()
}

// ---------------------------------------------------------------------------
// 6. Reverse collision (B10): payout settlement reference == existing deposit key.

// postDepositKey writes a deposit ledger transaction whose (provider, provider_tx_id)
// is ref - the key a payout settlement would use.
func (w *k3World) b11PostDepositKey(ref string) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		clearing, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
		if err != nil {
			return err
		}
		pid := w.provider
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxDeposit,
			IdempotencyKey: w.provider + ":" + ref, ProviderID: &pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearing, Direction: ledger.Debit, Amount: 1000},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: 1000},
			},
		})
		return err
	})
}

// b11ParkReverse parks a sync payout success whose reference equals a deposit key.
func (w *k3World) b11ParkReverse(amount int64) (*b11Parked, string, GateResult[WithdrawResult]) {
	w.t.Helper()
	ref := b11Ref()
	w.b11PostDepositKey(ref)
	wr, att := w.b11Claim(amount)
	gr := w.b11Dispatch(att, func(WithdrawRequest) WithdrawResult {
		return WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: ref}
	})
	if gr.Class != ErrorClassSucceeded {
		w.t.Fatalf("setup: want a succeeded gate result, got %s", gr.Class)
	}
	before := w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID)
	if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, wr.ID, att, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		w.t.Fatalf("apply (reverse collision): %v", err)
	}
	fresh := w.attempt(att.ID)
	if fresh.State != AttemptDisputed || fresh.TerminalReason == nil || *fresh.TerminalReason != TerminalReasonProviderReferenceConflict {
		w.t.Fatalf("want disputed/provider_reference_conflict, got %s %v", fresh.State, fresh.TerminalReason)
	}
	if fresh.ProviderReference != nil {
		w.t.Fatalf("the colliding reference must not be bound, got %q", *fresh.ProviderReference)
	}
	if after := w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID); after != before {
		w.t.Fatalf("the park posted: ledger tx %d -> %d", before, after)
	}
	if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil || got.ProviderReference != nil {
		w.t.Fatalf("hold/binding changed: %+v", got)
	}
	p := &b11Parked{shape: "reverse", wr: wr, stale: att, fresh: fresh, base: w.b11Snap(wr.ID, att.ID)}
	return p, ref, gr
}

// b11HeldReverse is b11Held for the conflict reason.
func (w *k3World) b11HeldReverse(p *b11Parked, what string) {
	w.t.Helper()
	now := w.b11Snap(p.wr.ID, p.stale.ID)
	if now != p.base {
		w.t.Fatalf("%s: moved\n before: %+v\n  after: %+v", what, p.base, now)
	}
	if now.aReason != TerminalReasonProviderReferenceConflict || now.wrState != withdrawal.StateSubmitted || now.wrReleased {
		w.t.Fatalf("%s: %+v", what, now)
	}
	w.assertInvariants()
}

func TestB11_ReverseCollision_Parks_NoLoop_RedeliveryStable_OneAudit(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	p, ref, gr := w.b11ParkReverse(300)
	if got := w.b11Audits(b11AuditConflict, p.stale.ID); got != 1 {
		t.Fatalf("want one conflict audit, got %d", got)
	}
	rows := w.sysQuery(`SELECT metadata->>'bound_to' AS bt, metadata->>'provider_reference' AS pr FROM audit_log WHERE tenant_id = $1 AND action = $2 AND target_id = $3`,
		w.f.tenantID, b11AuditConflict, p.stale.ID.String())
	if rows[0]["bt"] != "ledger_deposit" || rows[0]["pr"] != ref {
		t.Fatalf("audit bound_to/reference: %v", rows[0])
	}
	// Redelivery of the same result (sweeper retry / redelivered phase C): no
	// error (no rollback loop on the ledger idempotency check), no second audit.
	for i := 0; i < 3; i++ {
		if err := ApplyPayoutResult(context.Background(), w.pool, w.f.tenantID, p.wr.ID, p.stale, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
			t.Fatalf("redelivery %d: %v", i, err)
		}
		w.b11HeldReverse(p, "redelivery")
	}
	// Decline and pending for the same reference, the callback and the status
	// path also end without a release or a loop.
	for _, oc := range []Outcome{OutcomeSucceeded, OutcomeDeclined, OutcomePending} {
		ev := ReceiptEvidence{EventType: "payout", ProviderReference: ref, MerchantReference: p.stale.MerchantReference, Outcome: oc, Amount: p.wr.Amount, AssetCode: "EUR", DeclineReason: "insufficient_funds"}
		for d := 0; d < 2; d++ {
			if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, e := ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, w.provider, ev)
				return e
			}); err != nil {
				t.Fatalf("callback %s: %v", oc, err)
			}
			w.b11HeldReverse(p, "callback "+string(oc))
		}
	}
	st := GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: StatusResult{Outcome: OutcomeSucceeded, ProviderReference: ref, Amount: p.wr.Amount, AssetCode: "EUR"}}
	for _, from := range []AttemptState{AttemptSubmitting, AttemptPending, AttemptAmbiguous} {
		err := applyPayoutStatusEvidence(context.Background(), w.pool, w.f.tenantID, p.wr.ID, p.staleAs(from), st, EvidenceQueryStatus, time.Now().Add(time.Minute), nil, WithDestinations(pitest.Shared()))
		b11OKOrConflict(t, err, "status success")
		w.b11HeldReverse(p, "status from "+string(from))
	}
	stats := w.b11Sweeper().RunOnce(context.Background(), []uuid.UUID{w.f.tenantID})
	if stats.Claimed != 0 || len(stats.Errors) != 0 {
		t.Fatalf("the sweeper must not retry a parked payout: %+v", stats)
	}
	w.b11HeldReverse(p, "sweeper")
	if got := w.b11Audits(b11AuditConflict, p.stale.ID); got != 1 {
		t.Fatalf("redeliveries must not re-audit: %d rows", got)
	}
	// M2 refuses it as well (PayoutDisputeReasons: false).
	for _, kind := range []ResolutionKind{ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid} {
		_, err := w.request(w.f1, w.m2In(p.stale.ID, kind))
		k3RequireCode(t, err, "MR012")
	}
	w.b11HeldReverse(p, "M2")
}

// The same collision found by the poll: the payout is pending under X, a deposit
// with the same provider key posts afterwards, and the poll reports success.
func TestB11_ReverseCollision_PollSuccess_ParksKeepsHold(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, pend := w.payout(300)
	x := *pend.ProviderReference
	w.b11PostDepositKey(x)
	w.prov.setStatus(x, StatusResult{Outcome: OutcomeSucceeded, ProviderReference: x, Amount: wr.Amount, AssetCode: "EUR"})
	ledgerBefore := w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID)
	for i := 0; i < 3; i++ {
		cur := pend
		if i > 0 {
			cur = w.attempt(pend.ID)
		}
		if err := PollPayoutStatus(context.Background(), w.pool, w.orch, MockCredentialResolver{}, w.f.tenantID, cur, time.Now().Add(time.Minute), nil); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	a, got := w.attempt(pend.ID), w.withdrawalOf(wr.ID)
	if a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != TerminalReasonProviderReferenceConflict {
		t.Fatalf("want disputed/provider_reference_conflict, got %s %v", a.State, a.TerminalReason)
	}
	if got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		t.Fatalf("hold released: %s", got.State)
	}
	if n := w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID); n != ledgerBefore {
		t.Fatalf("ledger moved: %d -> %d", ledgerBefore, n)
	}
	if n := w.b11Audits(b11AuditConflict, pend.ID); n != 1 {
		t.Fatalf("want one conflict audit, got %d", n)
	}
	w.assertInvariants()
}

// ---------------------------------------------------------------------------
// 7. Tenant isolation.

func TestB11_TenantIsolation(t *testing.T) {
	a := newK3World(t, k3Opts{base: 1})
	b := newK3WorldOn(t, a.pool, k3Opts{base: 1})
	pa := a.b11ParkSync(300, OutcomePending)
	pb := b.b11ParkSync(200, OutcomePending)

	// Tenant B cannot drive tenant A's attempt (RLS: not found) ...
	gr := GateResult[WithdrawResult]{Class: ErrorClassSucceeded, Value: WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: b11Ref()}}
	if err := ApplyPayoutResult(context.Background(), a.pool, b.f.tenantID, pa.wr.ID, pa.stale, gr, EvidenceSync, WithDestinations(pitest.Shared())); err == nil {
		t.Fatalf("tenant B applied evidence to tenant A's payout")
	}
	err := applyPayoutStatusEvidence(context.Background(), a.pool, b.f.tenantID, pa.wr.ID, pa.stale,
		GateResult[StatusResult]{Class: ErrorClassSucceeded, Value: StatusResult{Outcome: OutcomeSucceeded, ProviderReference: b11Ref(), Amount: pa.wr.Amount, AssetCode: "EUR"}},
		EvidenceQueryStatus, time.Now().Add(time.Minute), nil, WithDestinations(pitest.Shared()))
	if err == nil {
		t.Fatalf("tenant B applied status evidence to tenant A's payout")
	}
	// ... a callback in tenant B naming A's merchant reference resolves nothing.
	if err := b.pool.WithTenant(context.Background(), b.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := ApplyReceiptEvidence(ctx, tx, b.orch, b.f.tenantID, a.provider, ReceiptEvidence{
			EventType: "payout", ProviderReference: b11Ref(), MerchantReference: pa.stale.MerchantReference,
			Outcome: OutcomeSucceeded, Amount: pa.wr.Amount, AssetCode: "EUR"})
		return e
	}); err != nil {
		t.Fatalf("cross-tenant callback must be harmless, got %v", err)
	}
	// ... and B's sweeper and A's own activity leave each other's holds alone.
	b.b11Sweeper().RunOnce(context.Background(), []uuid.UUID{b.f.tenantID})
	a.b11Sweeper().RunOnce(context.Background(), []uuid.UUID{a.f.tenantID})
	a.b11Held(pa, "tenant A after tenant B's attempts")
	b.b11Held(pb, "tenant B")
	if n := b.countRows(`SELECT count(*) FROM payment_attempts WHERE id = $1`, pa.stale.ID); n != 0 {
		t.Fatalf("tenant B can read tenant A's attempt")
	}
}

// ---------------------------------------------------------------------------
// 8. Reconciliation: what the code does for a disputed UNBOUND payout.
//
// B11 pinned the then-current behaviour (ADR 0095 section 35.2, PO-1): DETECTION
// ratified, CLEARING and the missing standing coverage ruled DEFECTIVE for
// payouts. PAY-PAYOUT-UNBOUND-STANDING-1 (ADR 0095 section 35.6) FLIPPED those
// pins deliberately; the tests below now pin the fixed behaviour:
//
//   - a `succeeded` PAYOUT statement line resolving to the parked payout (by
//     merchant reference, or by reference) raises pay_captured_unposted IN-RUN
//     (unchanged) and, once persisted, STANDING on every later run, unwindowed
//     (the K3 loader collects disputed payout attempts whose captureClass is
//     unbound; checkStandingUnbound runs per operation on PAYOUT lines);
//   - deposit-shaped signals (a deposit_reversal line, a tombstone, even naming
//     the same reference) NEVER clear a payout finding;
//   - the only clearing is a withdrawal_completed posting keyed by the line
//     reference and attributable to the parked attempt (no other attempt holds
//     that reference);
//   - the finding text is the payout reading: never allocation, resolution NOT
//     IMPLEMENTED (R-K3-8, PAY-PAYOUT-UNBOUND-RESOLVE-1), M1 only acknowledges.
//
// Bound-class payout parks are covered in-run and standing by
// checkUnmatchedAttempts (unchanged by STANDING-1 except the wording).

// b11PayoutHint is the exact payout ExpectedValue (STANDING-1 item 3).
const b11PayoutHint = "resolution: PSP-side recall/return or governed completion against the hold (NOT IMPLEMENTED, R-K3-8); never allocation; M1 only acknowledges"

// b11M4ScopeHint is the exact ExpectedValue for an unbound payout park INSIDE
// RESOLVE-1's scope (ADR 0111 §4.1 / C-6: an unbound reason with NO provider
// reference). FLIPPED deliberately by the RESOLVE-1 L-4 tightening (ADR 0111
// §4.6 "the unbound-park hint names M4", §7.3 "may start now"; task
// PAY-PAYOUT-UNBOUND-RESOLVE-1 tightenings, r16-res1): the sync and
// reverse-collision parks hold no reference and now name M4 (NOT
// IMPLEMENTED); the poll park holds X and keeps b11PayoutHint. FLIPPED again
// deliberately by migration 0125 (PAY-PAYOUT-UNBOUND-RESOLVE-1 r19, ADR 0111
// §4.6 / §10.4 hint condition): M4 exists, so "NOT IMPLEMENTED" became "MOCK
// only" while the T10 flag stands.
const b11M4ScopeHint = "resolution: PSP-side recall/return, or the evidence-backed four-eyes resolution M4 (PAY-PAYOUT-UNBOUND-RESOLVE-1, ADR 0111 §4; MOCK only); never allocation; M1 only acknowledges"

// b11RequirePayoutCU asserts exactly one pay_captured_unposted for the payout
// attempt keyed on lineRef, correctly represented as a PAYOUT finding.
func (w *k3World) b11RequirePayoutCU(ms []reconciliation.Mismatch, attempt uuid.UUID, lineRef string, standing bool, what string) reconciliation.Mismatch {
	w.t.Helper()
	got := w.b11CU(ms, attempt)
	if len(got) != 1 {
		w.t.Fatalf("%s: want exactly one pay_captured_unposted for payout %s, got %d:\n%s", what, attempt, len(got), render(ms))
	}
	m := got[0]
	wantHint := b11PayoutHint
	if a := w.attempt(attempt); a.ProviderReference == nil {
		wantHint = b11M4ScopeHint // RESOLVE-1 L-4 (see b11M4ScopeHint)
	}
	switch {
	case m.MismatchKind != reconciliation.MismatchKindPayCapturedUnposted,
		!strings.Contains(m.ReconciliationKey, "check=captured_unposted"),
		!strings.Contains(m.ReconciliationKey, "provider_reference="+lineRef),
		m.ExpectedValue != wantHint,
		strings.Contains(m.ExpectedValue, "LEDGER-SUSPENSE"),
		!strings.Contains(m.ActualValue, "op=payout"),
		!strings.Contains(m.ActualValue, "kind=payout"),
		!strings.Contains(m.ActualValue, "terminal_reason="):
		w.t.Fatalf("%s: payout finding misrepresented:\n%s", what, render(got))
	}
	if standing != strings.Contains(m.ActualValue, "standing: persisted line") {
		w.t.Fatalf("%s: standing=%t but detail is %q", what, standing, m.ActualValue)
	}
	return m
}

// b11Tombstone writes a tombstone ledger row on (provider, ref) - a deposit-shaped signal.
func (w *k3World) b11Tombstone(ref string) {
	w.t.Helper()
	pid := w.provider
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "tombstone:" + pid + ":" + ref, ProviderID: &pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
		})
		return err
	})
}

// b11DepositReversal is a deposit_reversal statement line naming original.
func (w *k3World) b11DepositReversal(original, status string, amount int64) statement.PaymentStatementLine {
	rev := w.payoutLine("b11-rev-"+uuid.NewString()[:8], "", status, amount)
	rev.Kind = statement.PaymentLineDepositReversal
	rev.OriginalProviderReference = original
	return rev
}

// b11Complete is a TEST STAND-IN for the governed completion against the hold
// (PAY-PAYOUT-UNBOUND-RESOLVE-1 is NOT IMPLEMENTED): it posts withdrawal_completed
// for the withdrawal under (provider, settlementRef) through the real
// withdrawal.Complete.
func (w *k3World) b11Complete(wrID uuid.UUID, settlementRef string) {
	w.t.Helper()
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return withdrawal.Complete(ctx, tx, wrID, w.provider, settlementRef)
	})
	w.assertInvariants()
}

func (w *k3World) b11Recon(src statement.PaymentStatementSource) []reconciliation.Mismatch {
	w.t.Helper()
	snap := func() [4]int {
		return [4]int{
			w.countRows(`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, w.f.tenantID),
			w.countRows(`SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND state <> 'submitted'`, w.f.tenantID),
			w.countRows(`SELECT count(*) FROM payment_attempts WHERE tenant_id = $1 AND state <> 'disputed'`, w.f.tenantID),
			w.countRows(`SELECT count(*) FROM payment_attempts WHERE tenant_id = $1 AND state = 'disputed'`, w.f.tenantID),
		}
	}
	before := snap()
	ms := w.stmtRun(src)
	if after := snap(); after != before {
		w.t.Fatalf("a reconciliation run moved money or state: %v -> %v", before, after)
	}
	w.assertInvariants()
	return ms
}

func (w *k3World) b11CU(ms []reconciliation.Mismatch, attempt uuid.UUID) []reconciliation.Mismatch {
	return mismatchesOf(ms, reconciliation.MismatchKindPayCapturedUnposted, "attempt="+attempt.String())
}

// FLIPPED under PAY-PAYOUT-UNBOUND-STANDING-1 (was
// TestB11_Recon_UnboundPayoutPark_InRunFinding_NoStanding_ClearingSignals, which
// pinned "no standing" and "a deposit_reversal clears" as CURRENT behaviour).
func TestB11_Recon_UnboundPayoutPark_InRunAndStanding_DepositSignalsNeverClear(t *testing.T) {
	for _, shapeName := range []string{"sync", "poll"} {
		t.Run(shapeName, func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			var p *b11Parked
			if shapeName == "sync" {
				p = w.b11ParkSync(300, OutcomeSucceeded)
			} else {
				p = w.b11ParkPoll(300, OutcomePending)
			}
			lineRef := p.ref
			if lineRef == "" {
				lineRef = b11Ref() // the PSP's own, valid reference for the payout it executed
			}
			okLine := w.payoutLine(lineRef, p.stale.MerchantReference, "succeeded", p.wr.Amount)

			// (d) first, while no succeeded line has been persisted: a pending,
			// declined or reversed line is not the signal (in-run or standing).
			for _, st := range []string{"pending", "declined", "reversed"} {
				ms := w.b11Recon(w.source(false, w.payoutLine(lineRef, p.stale.MerchantReference, st, p.wr.Amount)))
				if len(w.b11CU(ms, p.stale.ID)) != 0 {
					t.Fatalf("a %s line must not raise pay_captured_unposted:\n%s", st, render(ms))
				}
			}
			if ms := w.b11Recon(w.pastSource(false)); len(w.b11CU(ms, p.stale.ID)) != 0 {
				t.Fatalf("persisted non-succeeded lines must not raise a standing finding:\n%s", render(ms))
			}

			// (a) the in-run finding: exactly one, represented as a PAYOUT finding.
			ms := w.b11Recon(w.source(false, okLine))
			w.b11RequirePayoutCU(ms, p.stale.ID, lineRef, false, "in-run")
			if n := len(ms); n != 1 {
				t.Fatalf("the run must report exactly that finding, got %d:\n%s", n, render(ms))
			}
			w.b11Held(p, "recon in-run")

			// (b) repeatable in-run, still exactly one (in-run and standing dedupe).
			again := w.b11Recon(w.source(false, okLine))
			w.b11RequirePayoutCU(again, p.stale.ID, lineRef, false, "in-run again")

			// (c) FLIPPED (was "no standing coverage", a GAP): with no line in the
			// run, the persisted succeeded line keeps the finding outstanding on
			// EVERY run, unwindowed.
			for i := 0; i < 3; i++ {
				w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, lineRef, true, fmt.Sprintf("standing run %d", i))
				w.b11Held(p, "recon standing")
			}
			// A later pending/declined line for the same reference does not hide it.
			for _, st := range []string{"pending", "declined"} {
				ms := w.b11Recon(w.source(false, w.payoutLine(lineRef, p.stale.MerchantReference, st, p.wr.Amount)))
				if len(w.b11CU(ms, p.stale.ID)) != 1 {
					t.Fatalf("a later %s line must not hide the standing finding:\n%s", st, render(ms))
				}
			}

			// (e) FLIPPED (was "a deposit_reversal naming the line reference clears",
			// ruled DEFECTIVE): a deposit_reversal line naming the line reference,
			// completed (succeeded or reversed), in the same import as the payout
			// line and persisted for later runs, NEVER clears a payout finding.
			for _, st := range []string{"succeeded", "reversed"} {
				w.b11RequirePayoutCU(w.b11Recon(w.source(false, okLine, w.b11DepositReversal(lineRef, st, p.wr.Amount))), p.stale.ID, lineRef, false, "deposit_reversal ("+st+") in-run")
				w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, lineRef, true, "persisted deposit_reversal ("+st+")")
			}
			// The same from a non-MOCK import (eligible to clear deposit findings).
			w.b11RequirePayoutCU(w.b11Recon(w.source(true, w.b11DepositReversal(lineRef, "succeeded", p.wr.Amount))), p.stale.ID, lineRef, true, "real-import deposit_reversal")
			w.b11Held(p, "recon after deposit_reversal")
		})
	}
}

// FLIPPED under PAY-PAYOUT-UNBOUND-STANDING-1 (was
// TestB11_Recon_UnboundPayoutPark_TombstoneClears, CURRENT behaviour ruled
// defective): a tombstone on the line's reference (a deposit-shaped signal)
// clears neither the in-run nor the standing payout finding.
func TestB11_Recon_UnboundPayoutPark_TombstoneNeverClears(t *testing.T) {
	for _, shapeName := range []string{"sync", "poll"} {
		t.Run(shapeName, func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			var p *b11Parked
			if shapeName == "sync" {
				p = w.b11ParkSync(300, OutcomeSucceeded)
			} else {
				p = w.b11ParkPoll(300, OutcomePending)
			}
			ref := p.ref
			if ref == "" {
				ref = b11Ref()
			}
			line := w.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
			w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "setup")
			w.b11Tombstone(ref)
			w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "tombstone, in-run")
			w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "tombstone, standing")
			w.b11RequirePayoutCU(w.b11Recon(w.pastSource(true)), p.stale.ID, ref, true, "tombstone, standing, real import exists")
		})
	}
}

// FLIPPED under PAY-PAYOUT-UNBOUND-STANDING-1 (was
// TestB11_Recon_ReverseCollisionPark_InRunFinding, whose "no standing coverage"
// pin was a GAP). The reverse-collision park (provider_reference_conflict, no
// bound reference): in-run AND standing. This is the fail-open case the
// ruling named: the PSP refund of the REAL deposit R (a deposit_reversal naming
// R) must not clear the payout's possible-double-payout finding on R.
func TestB11_Recon_ReverseCollisionPark_InRunAndStanding_DepositRefundNeverClears(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	p, ref, _ := w.b11ParkReverse(300)
	line := w.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
	w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "in-run")
	w.b11HeldReverse(p, "recon")
	w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "standing")
	// The real deposit R is refunded by the PSP.
	w.b11RequirePayoutCU(w.b11Recon(w.source(false, w.b11DepositReversal(ref, "succeeded", 1000))), p.stale.ID, ref, true, "deposit refund on R")
	w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "persisted deposit refund on R")
	w.b11HeldReverse(p, "recon after deposit refund")
}

// STANDING-1 item 2: the ONLY clearing is a withdrawal_completed keyed by the
// line reference. b11Complete stands in for the governed completion against
// the hold (PAY-PAYOUT-UNBOUND-RESOLVE-1, NOT IMPLEMENTED).
func TestB11_Recon_UnboundPayoutPark_WithdrawalCompletedOnLineRefClears(t *testing.T) {
	for _, shapeName := range []string{"sync", "poll"} {
		t.Run(shapeName, func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			var p *b11Parked
			if shapeName == "sync" {
				p = w.b11ParkSync(300, OutcomeSucceeded)
			} else {
				p = w.b11ParkPoll(300, OutcomePending)
			}
			ref := p.ref
			if ref == "" {
				ref = b11Ref()
			}
			line := w.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
			w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "setup in-run")
			w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "setup standing")

			w.b11Complete(p.wr.ID, ref)
			for i, src := range []statement.PaymentStatementSource{w.pastSource(false), w.source(false, line), w.pastSource(false)} {
				if ms := w.b11Recon(src); len(w.b11CU(ms, p.stale.ID)) != 0 {
					t.Fatalf("run %d: a withdrawal_completed keyed by the line reference must clear the payout finding:\n%s", i, render(ms))
				}
			}
		})
	}
	// A completion of the SAME withdrawal under a DIFFERENT reference is not keyed
	// by the line reference: the finding stays.
	t.Run("completion_on_another_reference_does_not_clear", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		p := w.b11ParkSync(300, OutcomeSucceeded)
		ref := b11Ref()
		w.b11RequirePayoutCU(w.b11Recon(w.source(false, w.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount))), p.stale.ID, ref, false, "setup")
		w.b11Complete(p.wr.ID, b11Ref())
		w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "completion on another reference")
	})
	// Security review L-1 (a): POSITIVE attribution. A withdrawal_completed that
	// merely shares the line reference - a legacy/unattempted withdrawal's
	// completion (no payment_attempts row: reconciliation counts it as
	// legacyUnattempted), or an unlinked posting released by no withdrawal at
	// all - is not the parked attempt's own release and never clears.
	t.Run("legacy_unattempted_completion_on_the_line_reference_does_not_clear", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		p := w.b11ParkSync(300, OutcomeSucceeded)
		ref := b11Ref()
		line := w.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
		w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "setup")
		legacy := w.approveWithdrawal(120, "b11-legacy-"+uuid.NewString())
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			return withdrawal.MarkSubmitted(ctx, tx, legacy.ID, w.provider, "b11-legacy-instr-"+uuid.NewString()[:8])
		})
		w.b11Complete(legacy.ID, ref)
		if n := w.countRows(`SELECT count(*) FROM payment_attempts WHERE tenant_id = $1 AND withdrawal_request_id = $2`, w.f.tenantID, legacy.ID); n != 0 {
			t.Fatalf("setup: the legacy withdrawal must have no payment attempt, got %d", n)
		}
		w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "legacy completion, standing")
		w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "legacy completion, in-run")
		if got := w.withdrawalOf(p.wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
			t.Fatalf("the parked payout's hold moved: %s", got.State)
		}
	})
	t.Run("unlinked_completion_posting_on_the_line_reference_does_not_clear", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		p := w.b11ParkSync(300, OutcomeSucceeded)
		ref := b11Ref()
		line := w.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
		w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "setup")
		pid := w.provider
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			cash, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerCash, "EUR")
			if err != nil {
				return err
			}
			clearing, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
			if err != nil {
				return err
			}
			_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalCompleted,
				IdempotencyKey: pid + ":" + ref, ProviderID: &pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: cash, Direction: ledger.Debit, Amount: 50},
					{LedgerAccountID: clearing, Direction: ledger.Credit, Amount: 50},
				},
			})
			return err
		})
		w.assertInvariants()
		w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "unlinked completion, standing")
		w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "unlinked completion, in-run")
	})
	// The line reference is ANOTHER payout attempt's provider reference (the
	// line resolves to that attempt by reference and names p by merchant
	// reference). Even p's own withdrawal completed under that reference does
	// not clear: the reference is held by the other attempt, so the completion is
	// not attributable to p (fail closed on ambiguity).
	t.Run("completion_keyed_by_a_reference_another_attempt_holds_does_not_clear", func(t *testing.T) {
		w := newK3World(t, k3Opts{base: 1})
		p := w.b11ParkSync(300, OutcomeSucceeded)
		_, other := w.payout(150)
		ref := *other.ProviderReference
		line := w.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
		ms := w.b11Recon(w.source(false, line))
		if got := w.b11CU(ms, p.stale.ID); len(got) != 1 {
			t.Fatalf("setup: want the merchant cross-check finding for p:\n%s", render(ms))
		}
		w.b11Complete(p.wr.ID, ref)
		w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "completion keyed by another attempt's reference")
	})
}

// STANDING-1: unrelated financial activity never resolves the finding - other
// attempts, other references, and another payout's completion under the SAME
// reference (borrowed attribution may raise, never clear).
func TestB11_Recon_UnboundPayoutPark_UnrelatedActivityNeverClears(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	p := w.b11ParkSync(300, OutcomeSucceeded)
	ref := b11Ref()
	line := w.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
	w.b11RequirePayoutCU(w.b11Recon(w.source(false, line)), p.stale.ID, ref, false, "setup")

	// Another unbound payout park, its own line and its own completion.
	q := w.b11ParkSync(200, OutcomeSucceeded)
	qRef := b11Ref()
	w.b11RequirePayoutCU(w.b11Recon(w.source(false, w.payoutLine(qRef, q.stale.MerchantReference, "succeeded", q.wr.Amount))), q.stale.ID, qRef, false, "second park")
	w.b11Complete(q.wr.ID, qRef)
	// Another payout completed normally under its own reference.
	other, otherPend := w.payout(150)
	w.b11Complete(other.ID, *otherPend.ProviderReference)
	// Deposit-shaped activity on OTHER references.
	unrelated := b11Ref()
	w.b11Tombstone(unrelated)
	ms := w.b11Recon(w.source(true, w.b11DepositReversal(b11Ref(), "succeeded", 300)))
	w.b11RequirePayoutCU(ms, p.stale.ID, ref, true, "after unrelated activity")
	if len(w.b11CU(ms, q.stale.ID)) != 0 {
		t.Fatalf("control: the second park's own completion must clear ITS finding:\n%s", render(ms))
	}

	// Another payout attempt completed under the SAME reference as p's line: the
	// line names p by merchant reference, but the completion belongs to the
	// other attempt (it holds ref as its settlement reference). Not attributable
	// to p: the finding stays, in-run (via the merchant cross-check) and standing.
	a, aAtt := w.payout(250)
	w.b11Complete(a.ID, ref)
	st := w.b11RequirePayoutCU(w.b11Recon(w.pastSource(false)), p.stale.ID, ref, true, "another attempt's completion under the same reference, standing")
	if !strings.Contains(st.ActualValue, "holder_attempt="+aAtt.ID.String()) {
		t.Fatalf("the standing detail must name the attempt holding the line reference (payout settlement): %s", st.ActualValue)
	}
	ms = w.b11Recon(w.source(false, line))
	if got := w.b11CU(ms, p.stale.ID); len(got) != 1 {
		t.Fatalf("another attempt's completion under the same reference must not clear (in-run):\n%s", render(ms))
	}
	// p itself is untouched (b11Held's tenant-wide balances legitimately moved
	// with the other completions, so check p's own rows).
	if got, att := w.withdrawalOf(p.wr.ID), w.attempt(p.stale.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil ||
		att.State != AttemptDisputed || att.TerminalReason == nil || !strings.HasPrefix(*att.TerminalReason, b11ReasonPrefix) {
		t.Fatalf("the parked payout moved: withdrawal %s, attempt %s", got.State, att.State)
	}
	w.assertInvariants()
}

// The decision half: a BOUND-class payout dispute (callback_amount_asset_mismatch
// on a payout that holds X) IS covered, in-run and standing, by the same capture
// classes - reconciliation does not special-case the operation.
func TestB11_Recon_BoundClassPayoutPark_InRunAndStanding(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, pend := w.payout(300)
	x := *pend.ProviderReference
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := ApplyReceiptEvidence(ctx, tx, w.orch, w.f.tenantID, w.provider, ReceiptEvidence{
			EventType: "payout", ProviderReference: x, MerchantReference: pend.MerchantReference,
			Outcome: OutcomeSucceeded, Amount: wr.Amount - 1, AssetCode: "EUR"})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	a := w.attempt(pend.ID)
	if a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != TerminalReasonCallbackAmountAssetMismatch || a.ProviderReference == nil {
		t.Fatalf("setup: %s %v", a.State, a.TerminalReason)
	}
	if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateSubmitted || got.ReleaseLedgerTransactionID != nil {
		t.Fatalf("hold released by a disputed callback: %s", got.State)
	}
	if got := w.b11CU(w.b11Recon(w.source(false, w.payoutLine(x, pend.MerchantReference, "succeeded", wr.Amount-1))), pend.ID); len(got) != 1 {
		t.Fatalf("in-run: want one finding, got %d", len(got))
	}
	if got := w.b11CU(w.b11Recon(w.pastSource(false)), pend.ID); len(got) != 1 {
		t.Fatalf("standing: want one finding, got %d", len(got))
	}
}

// Tenant isolation of the finding: another tenant's run never sees it, and
// another tenant's financial activity - even under the SAME provider id and the
// SAME reference, of every clearing shape (a deposit_reversal line, a tombstone,
// a withdrawal_completed, a succeeded payout line naming A's merchant
// reference) - never raises, clears or resolves tenant A's payout finding
// (PAY-PAYOUT-UNBOUND-STANDING-1; every K3 read carries tenant_id = $1, RLS is
// the second line).
func TestB11_Recon_TenantIsolation(t *testing.T) {
	a := newK3World(t, k3Opts{base: 1})
	b := newK3WorldOn(t, a.pool, k3Opts{base: 1})
	p := a.b11ParkSync(300, OutcomeSucceeded)
	ref := b11Ref()
	line := a.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
	a.b11RequirePayoutCU(a.b11Recon(a.source(false, line)), p.stale.ID, ref, false, "setup: tenant A in-run")
	a.b11RequirePayoutCU(a.b11Recon(a.pastSource(false)), p.stale.ID, ref, true, "setup: tenant A standing")

	// Tenant B (its own provider) with a line naming A's merchant reference: a
	// missing-platform-record finding, never a captured-unposted for A's payout.
	bl := b.payoutLine(b11Ref(), p.stale.MerchantReference, "succeeded", p.wr.Amount)
	ms := b.b11Recon(b.source(false, bl))
	if got := b.b11CU(ms, p.stale.ID); len(got) != 0 {
		t.Fatalf("tenant B reported tenant A's attempt:\n%s", render(ms))
	}
	if got := mismatchesOf(ms, reconciliation.MismatchKindPayCapturedUnposted, ""); len(got) != 0 {
		t.Fatalf("tenant B has a captured_unposted finding it should not:\n%s", render(ms))
	}

	// Tenant B, under tenant A's PROVIDER ID and A's line reference: a
	// withdrawal_completed of B's own withdrawal (THE payout clearing shape), and
	// an import carrying a completed deposit_reversal naming the reference plus a
	// succeeded payout line naming A's merchant reference. (A tombstone on the
	// same key cannot coexist with the completion in tenant B - one ledger key
	// per (tenant, provider, provider_tx_id) - and never clears a payout even
	// inside tenant A, TestB11_Recon_UnboundPayoutPark_TombstoneNeverClears.)
	pid := a.provider
	bwr, _ := b.payout(120)
	b.tx(func(ctx context.Context, tx pgx.Tx) error { return withdrawal.Complete(ctx, tx, bwr.ID, pid, ref) })
	rev := b.b11DepositReversal(ref, "succeeded", p.wr.Amount)
	rev.ProviderID = pid
	bLine := a.payoutLine(ref, p.stale.MerchantReference, "succeeded", p.wr.Amount)
	ms = b.b11Recon(k3Source{provider: pid, real: true, lines: []statement.PaymentStatementLine{bLine, rev}})
	if got := mismatchesOf(ms, reconciliation.MismatchKindPayCapturedUnposted, ""); len(got) != 0 {
		t.Fatalf("tenant B (provider A's id) has a captured_unposted finding it should not:\n%s", render(ms))
	}
	b.assertInvariants()

	// Tenant A's finding is untouched by all of it, in-run and standing.
	a.b11RequirePayoutCU(a.b11Recon(a.pastSource(false)), p.stale.ID, ref, true, "tenant A standing after tenant B activity")
	a.b11RequirePayoutCU(a.b11Recon(a.source(false, line)), p.stale.ID, ref, false, "tenant A in-run after tenant B activity")
	a.b11Held(p, "tenant A after tenant B activity")
}
