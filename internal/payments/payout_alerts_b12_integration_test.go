//go:build integration

// B12 (PAY-PAYOUT-DISPUTE-ALERT-1, raise only): every payout dispute write site (T10 parks, T14,
// the B10 provider_reference_conflict park, the T15 never-sent park) and the payout T16
// escalation raise exactly ONE durable P1 on the platform-owned Kind payment.webhook_integrity
// with discriminator payout_attempt:<id>:reason:<closed reason>. Nothing here resolves, releases,
// settles or posts anything: the audit rows are exactly the pre-existing ones, the hold is
// unchanged, the ledger gains no transaction and stays balanced.
package payments

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// b12Before is the money/state snapshot taken immediately before the act.
type b12Before struct {
	ledgerTx    int
	wrState     withdrawal.State
	releaseTxID *uuid.UUID
}

// b12Out is what a site's act returns to the shared assertions.
type b12Out struct {
	wr         withdrawal.WithdrawalRequest
	a          PaymentAttempt
	before     b12Before
	wantState  AttemptState // the attempt state after the act
	escalation bool         // a T16 escalation: no dispute, escalated_at set
}

func (e rbEnv) b12Snapshot(t *testing.T, wr withdrawal.WithdrawalRequest) b12Before {
	t.Helper()
	w := fpReqState(t, e.pool, e.f, wr.ID)
	return b12Before{ledgerTx: fpLedgerTx(t, e.pool, e.f), wrState: w.State, releaseTxID: w.ReleaseLedgerTransactionID}
}

// b12AssertNoMoneyMoved: the raise changed no financial fact. Same withdrawal state, the same
// release posting (or none), no new ledger transaction, debits == credits.
func (e rbEnv) b12AssertNoMoneyMoved(t *testing.T, o b12Out) {
	t.Helper()
	w := fpReqState(t, e.pool, e.f, o.wr.ID)
	if w.State != o.before.wrState {
		t.Fatalf("withdrawal state %s -> %s: a payout alert must not change it", o.before.wrState, w.State)
	}
	if (w.ReleaseLedgerTransactionID == nil) != (o.before.releaseTxID == nil) ||
		(w.ReleaseLedgerTransactionID != nil && *w.ReleaseLedgerTransactionID != *o.before.releaseTxID) {
		t.Fatalf("the hold release changed: %v -> %v", o.before.releaseTxID, w.ReleaseLedgerTransactionID)
	}
	if n := fpLedgerTx(t, e.pool, e.f); n != o.before.ledgerTx {
		t.Fatalf("ledger transactions %d -> %d: nothing may post", o.before.ledgerTx, n)
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
}

func (e rbEnv) b12Rows(t *testing.T) []alertinject.Row {
	t.Helper()
	return alertinject.ForSubject(t, e.pool, e.f.tenantID)
}

// b12AssertOneAlert asserts the subject tenant sees exactly ONE alert: the payout P1 for the
// attempt with the closed reason, open, with exactly one occurrence, carrying only provider_id,
// and containing none of the leak probes (provider text, references).
func (e rbEnv) b12AssertOneAlert(t *testing.T, attemptID uuid.UUID, reason string, leakProbes ...string) alertinject.Row {
	t.Helper()
	rows := e.b12Rows(t)
	if len(rows) != 1 {
		t.Fatalf("want exactly one alert for the subject tenant, got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	want := "payout_attempt:" + attemptID.String() + ":reason:" + reason
	if r.Kind != string(alerting.KindPaymentWebhookIntegrity) || r.Discriminator != want {
		t.Fatalf("alert kind/discriminator = %s / %s, want %s / %s", r.Kind, r.Discriminator, alerting.KindPaymentWebhookIntegrity, want)
	}
	if r.Severity != "p1" || r.State != "open" || r.Occurrences != 1 {
		t.Fatalf("alert severity=%s state=%s occurrences=%d, want p1/open/1", r.Severity, r.State, r.Occurrences)
	}
	if len(r.Attributes) != 1 || r.Attributes["provider_id"] != e.pid {
		t.Fatalf("attributes %v, want only provider_id=%q", r.Attributes, e.pid)
	}
	// The discriminator legitimately embeds the attempt's own random UUID, whose hex can contain a probe
	// such as "4111" by chance (a false positive seen in ~1 of 1000 runs). Probe the blob with that known
	// identifier removed, so only provider-supplied text can trip a probe.
	blob := strings.ReplaceAll(fmt.Sprint(r.Attributes, r.Discriminator), attemptID.String(), "<attempt>")
	for _, p := range leakProbes {
		if p != "" && strings.Contains(blob, p) {
			t.Fatalf("the alert leaks %q: %s", p, blob)
		}
	}
	return r
}

func (e rbEnv) b12AuditCount(t *testing.T, action string, attemptID uuid.UUID) int {
	t.Helper()
	return fpCount(t, e.pool, e.f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2 AND target_id=$3`,
		e.f.tenantID, action, attemptID.String())
}

// b12Callback applies a payout callback exactly like the webhook handler does: inside
// alerting.InTx, flushed after the commit.
func (e rbEnv) b12Callback(t *testing.T, a PaymentAttempt, outcome Outcome, ref string, amount int64) {
	t.Helper()
	if err := e.b12CallbackErr(a, outcome, ref, amount); err != nil {
		t.Fatalf("ApplyReceiptEvidence(%s): %v", outcome, err)
	}
}

func (e rbEnv) b12CallbackErr(a PaymentAttempt, outcome Outcome, ref string, amount int64) error {
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(e.pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		_, err := ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.pid, ReceiptEvidence{
			EventType: "payout", ProviderReference: ref, MerchantReference: a.MerchantReference,
			Outcome: outcome, Amount: amount, AssetCode: "EUR", DeclineReason: "insufficient_funds",
		})
		return err
	})
	if err == nil {
		pending.Flush(context.Background())
	}
	return err
}

func (e rbEnv) b12Decline(t *testing.T, wr withdrawal.WithdrawalRequest, a PaymentAttempt) {
	t.Helper()
	e.apply(t, wr, a, rbResult(ErrorClassDefiniteDecline, OutcomeDeclined, ""))
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptDeclined {
		t.Fatalf("setup: want declined, got %s", got.State)
	}
}

func (e rbEnv) b12Ambiguous(t *testing.T, key string) (withdrawal.WithdrawalRequest, PaymentAttempt) {
	t.Helper()
	wr, a := e.claim(t, key)
	e.apply(t, wr, a, GateResult[WithdrawResult]{Class: ErrorClassAmbiguous})
	got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID)
	if got.State != AttemptAmbiguous {
		t.Fatalf("setup: want ambiguous, got %s", got.State)
	}
	return wr, got
}

func (e rbEnv) b12Sweeper() *Sweeper {
	return &Sweeper{Pool: e.pool, Orchestrator: e.orch, PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: MockCredentialResolver{}, Lease: SweeperDefaultLease}
}

func b12Escalate(reason string) func(t *testing.T, e rbEnv) b12Out {
	return func(t *testing.T, e rbEnv) b12Out {
		wr, a := e.b12Ambiguous(t, "b12-esc-"+reason)
		before := e.b12Snapshot(t, wr)
		if err := e.b12Sweeper().escalateAmbiguousPayout(context.Background(), e.f.tenantID, a, time.Now().Add(time.Minute), reason); err != nil {
			t.Fatalf("escalateAmbiguousPayout: %v", err)
		}
		return b12Out{wr: wr, a: a, before: before, wantState: AttemptAmbiguous, escalation: true}
	}
}

type b12Case struct {
	name   string
	reason string // the closed alert reason in the discriminator
	audits map[string]int
	run    func(t *testing.T, e rbEnv) b12Out
	probes []string // text that must never appear in the alert
}

func b12Cases() []b12Case {
	foreign := "B12-FOREIGN-REF-9f3a"
	invalidRef := func() GateResult[WithdrawResult] {
		over := strings.Repeat("a", providerref.MaxBytes+1)
		return GateResult[WithdrawResult]{
			Class: ErrorClassProviderRefInvalid, Value: WithdrawResult{Outcome: OutcomeSucceeded, ProviderReference: over},
			Err: providerref.Validate("withdraw.provider_reference", over),
		}
	}
	return []b12Case{
		{name: "sync_invalid_provider_reference", reason: TerminalReasonInvalidProviderReference,
			audits: map[string]int{"payments.payout_parked_invalid_reference": 1}, probes: []string{"aaaaaaaa", "too_long"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.claim(t, "b12-sync-inv")
				before := e.b12Snapshot(t, wr)
				e.apply(t, wr, a, invalidRef())
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "status_invalid_provider_reference", reason: TerminalReasonInvalidProviderReference,
			audits: map[string]int{"payments.payout_parked_invalid_reference": 1}, probes: []string{"aaaaaaaa", "too_long"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.claim(t, "b12-st-inv")
				before := e.b12Snapshot(t, wr)
				over := strings.Repeat("a", providerref.MaxBytes+1)
				e.applyStatus(t, wr, a, GateResult[StatusResult]{Class: ErrorClassProviderRefInvalid, Err: providerref.Validate("query_status.provider_reference", over)})
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "sync_provider_reference_mismatch", reason: "provider_reference_mismatch",
			audits: map[string]int{"payments.payout_provider_reference_mismatch": 1}, probes: []string{foreign, "b12-bound-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.seedBound(t, "b12-sync-rm", "b12-bound-ref-1")
				before := e.b12Snapshot(t, wr)
				e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, foreign))
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "status_provider_reference_mismatch", reason: "provider_reference_mismatch",
			audits: map[string]int{"payments.payout_provider_reference_mismatch": 1}, probes: []string{foreign, "b12-bound-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.seedBound(t, "b12-st-rm", "b12-bound-ref-2")
				before := e.b12Snapshot(t, wr)
				e.applyStatus(t, wr, a, GateResult[StatusResult]{Class: ErrorClassSucceeded,
					Value: StatusResult{Outcome: OutcomeSucceeded, ProviderReference: foreign, Amount: 500, AssetCode: "EUR"}})
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "callback_provider_reference_mismatch", reason: "provider_reference_mismatch",
			audits: map[string]int{"payments.payout_provider_reference_mismatch": 1}, probes: []string{foreign, "b12-bound-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.seedBound(t, "b12-cb-rm", "b12-bound-ref-3")
				before := e.b12Snapshot(t, wr)
				e.b12Callback(t, a, OutcomeSucceeded, foreign, 500)
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "status_amount_asset_mismatch", reason: "amount_asset_mismatch",
			audits: map[string]int{"payments.payout_amount_asset_mismatch": 1}, probes: []string{"499", "B12-PROVIDER-ASSET"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.claim(t, "b12-st-am")
				before := e.b12Snapshot(t, wr)
				e.applyStatus(t, wr, a, GateResult[StatusResult]{Class: ErrorClassSucceeded,
					Value: StatusResult{Outcome: OutcomeSucceeded, ProviderReference: "b12-am-ref", Amount: 499, AssetCode: "EUR"}})
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "callback_amount_asset_mismatch", reason: TerminalReasonCallbackAmountAssetMismatch,
			audits: map[string]int{}, probes: []string{"b12-cam-ref", "499"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.claim(t, "b12-cb-am")
				before := e.b12Snapshot(t, wr)
				e.b12Callback(t, a, OutcomeSucceeded, "b12-cam-ref", 499)
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "callback_success_for_never_sent_attempt_T15", reason: TerminalReasonSuccessForNeverSentAttempt,
			audits: map[string]int{}, probes: []string{"b12-t15-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := notSentPayoutAttempt(t, e.pool, e.orch, e.f, 500, "b12-t15")
				if a.State != AttemptCreated {
					t.Fatalf("setup: want created, got %s", a.State)
				}
				before := e.b12Snapshot(t, wr)
				e.b12Callback(t, a, OutcomeSucceeded, "b12-t15-ref", 500)
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "callback_tombstone_precedes_success", reason: TerminalReasonTombstonePrecedesSuccess,
			audits: map[string]int{}, probes: []string{"b12-tomb-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				ref := "b12-tomb-ref-1"
				if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
						TenantID: e.f.tenantID, TransactionType: ledger.TxTombstone,
						IdempotencyKey: "tombstone:" + e.pid + ":" + ref, ProviderID: &e.pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
					})
					return err
				}); err != nil {
					t.Fatalf("seed tombstone: %v", err)
				}
				wr, a := e.claim(t, "b12-tomb")
				before := e.b12Snapshot(t, wr)
				e.b12Callback(t, a, OutcomeSucceeded, ref, 500)
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "callback_T14_success_after_payout_declined", reason: "success_after_payout_declined",
			audits: map[string]int{}, probes: []string{"b12-t14-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.claim(t, "b12-t14")
				e.b12Decline(t, wr, a)
				before := e.b12Snapshot(t, wr)
				e.b12Callback(t, a, OutcomeSucceeded, "b12-t14-ref", 500)
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "sync_T14_late_success_after_terminal", reason: "late_success_after_terminal",
			audits: map[string]int{"payments.payout_late_contradicting_evidence": 1}, probes: []string{"b12-late-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, a := e.claim(t, "b12-late-s")
				e.b12Decline(t, wr, a)
				before := e.b12Snapshot(t, wr)
				e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, "b12-late-ref"))
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "sync_T14_late_contradicting_evidence", reason: "late_contradicting_evidence",
			audits: map[string]int{"payments.payout_late_contradicting_evidence": 1}, probes: []string{foreign, "b12-bound-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				wr, pending := e.seedBound(t, "b12-lce", "b12-bound-ref-4")
				// The attempt is declined by status evidence; the stale pending snapshot then
				// receives a success echoing a different reference: the park CAS conflicts and
				// the contradiction handler routes it to T14.
				e.applyStatus(t, wr, pending, GateResult[StatusResult]{Class: ErrorClassDefiniteDecline,
					Value: StatusResult{Outcome: OutcomeDeclined, ProviderReference: "b12-bound-ref-4"}})
				if got := mustGetAttempt(t, e.pool, e.f.tenantID, pending.ID); got.State != AttemptDeclined {
					t.Fatalf("setup: want declined, got %s", got.State)
				}
				before := e.b12Snapshot(t, wr)
				e.apply(t, wr, pending, rbResult(ErrorClassSucceeded, OutcomeSucceeded, foreign))
				return b12Out{wr: wr, a: pending, before: before, wantState: AttemptDisputed}
			}},
		{name: "B10_provider_reference_conflict_park", reason: TerminalReasonProviderReferenceConflict,
			audits: map[string]int{rbConflictAudit: 1}, probes: []string{"b12-shared-ref"},
			run: func(t *testing.T, e rbEnv) b12Out {
				e.seedBound(t, "b12-b10-a", "b12-shared-ref-5")
				wr, a := e.claim(t, "b12-b10-b")
				before := e.b12Snapshot(t, wr)
				e.apply(t, wr, a, rbResult(ErrorClassPending, OutcomePending, "b12-shared-ref-5"))
				return b12Out{wr: wr, a: a, before: before, wantState: AttemptDisputed}
			}},
		{name: "T16_escalation_non_idempotent_manifest", reason: "non_idempotent_manifest",
			audits: map[string]int{"payments.payout_resend_escalated": 1}, run: b12Escalate("non_idempotent_manifest")},
		{name: "T16_escalation_max_resubmits_exhausted", reason: "max_resubmits_exhausted",
			audits: map[string]int{"payments.payout_resend_escalated": 1}, run: b12Escalate("max_resubmits_exhausted")},
		{name: "T16_escalation_resubmit_cas_refused", reason: "resubmit_cas_refused",
			audits: map[string]int{"payments.payout_resend_escalated": 1}, run: b12Escalate("resubmit_cas_refused")},
	}
}

// Normal: each closed reason, at each site, raises exactly one P1 with the exact discriminator
// and attributes; the audit rows are exactly the pre-existing ones; no money moves.
func TestB12_EachSiteAndReason_RaisesExactlyOneAlert_AuditUnchanged_NoMoney(t *testing.T) {
	pool := depositV2ScratchPool(t)
	cases := b12Cases()
	covered := map[string]bool{}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, orch, _ := fpOrch(t, pool, fmt.Sprintf("mock-b12-%d", i))
			e := rbEnv{pool: pool, f: f, orch: orch, pid: fmt.Sprintf("mock-b12-%d", i)}
			o := tc.run(t, e)

			got := mustGetAttempt(t, pool, f.tenantID, o.a.ID)
			if got.State != o.wantState {
				t.Fatalf("attempt state %s, want %s", got.State, o.wantState)
			}
			if o.escalation {
				if got.EscalatedAt == nil || got.TerminalReason != nil {
					t.Fatalf("an escalation sets escalated_at and no terminal reason: %+v", got)
				}
			} else if got.TerminalReason == nil {
				t.Fatalf("a dispute carries a terminal reason")
			}
			e.b12AssertOneAlert(t, o.a.ID, tc.reason, tc.probes...)
			for action, n := range tc.audits {
				if c := e.b12AuditCount(t, action, o.a.ID); c != n {
					t.Fatalf("audit %s = %d, want %d (audit behaviour must be unchanged)", action, c, n)
				}
			}
			e.b12AssertNoMoneyMoved(t, o)
			covered[tc.reason] = true
		})
	}
	// Every closed reason reachable from a payout site is exercised by the table above.
	for reason := range PayoutDisputeReasons() {
		if reason == "success_for_never_sent_attempt" || reason == "reversal_tombstone_precedes_success" {
			continue // exercised below under their receipt-cell names
		}
		if reason == "late_decline_after_terminal" {
			continue // LF M-1/C1: a decline replay is a clean no-op, see TestB12_LateDeclineReplay_*
		}
		if b13bTerminalReasonsCoveredElsewhere[reason] {
			continue // B13-B: exercised with the destination fixtures in b13b_destination_integration_test.go (park audit + exactly one P1)
		}
		if !covered[reason] {
			t.Errorf("closed payout reason %q has no row in the table", reason)
		}
	}
	for reason := range payoutEscalationReasons {
		if b13bEscalationReasonsCoveredElsewhere[reason] {
			continue // B13-B: TestB13B_T2_Reclaim_EscalatesOnBlockedDestination / TestB13B_T12_Resend_GatedByTheDestination
		}
		if !covered[reason] {
			t.Errorf("closed escalation reason %q has no row in the table", reason)
		}
	}
	if !covered[TerminalReasonSuccessForNeverSentAttempt] || !covered[TerminalReasonTombstonePrecedesSuccess] {
		t.Errorf("the T15 and tombstone cells must be in the table")
	}
}

// Escalation end to end: the call-site reason literals (not only the helper) reach the alert.
func TestB12_Escalation_ThroughResubmitPayoutAmbiguous_ReasonsReachTheAlert(t *testing.T) {
	pool := depositV2ScratchPool(t)

	t.Run("non_idempotent_manifest", func(t *testing.T) {
		f, orch, _ := fpOrch(t, pool, "mock-b12-e2e-ni")
		e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-e2e-ni"}
		wr, a := e.b12Ambiguous(t, "b12-e2e-ni")
		before := e.b12Snapshot(t, wr)
		if err := e.b12Sweeper().resubmitPayoutAmbiguous(context.Background(), f.tenantID, a); err != nil {
			t.Fatalf("resubmitPayoutAmbiguous: %v", err)
		}
		e.b12AssertOneAlert(t, a.ID, "non_idempotent_manifest")
		e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: before})
	})

	t.Run("max_resubmits_exhausted", func(t *testing.T) {
		f := seedPayoutFixture(t, pool, 100_000, true)
		inner := NewMockProvider("mock-b12-e2e-mr", "EUR")
		spy := &withdrawCountingProvider{MockProvider: inner}
		idem := &idempotentAmbiguousProvider{withdrawCountingProvider: spy}
		registerCapability(t, pool, f.orchFixture, idem, 100)
		orch := NewOrchestrator(map[string]PaymentProvider{"mock-b12-e2e-mr": idem}, MultiWebhookCredentialResolver{"mock-b12-e2e-mr": NewMockWebhookCredentials(inner)}).WithPayoutDestinations(pitest.Shared())
		e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-e2e-mr"}
		wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "b12-e2e-mr")
		claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
		if err != nil {
			t.Fatalf("ClaimForDispatch: %v", err)
		}
		gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, idem, claim.Attempt, WithDestinations(pitest.Shared()))
		if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
			t.Fatalf("ApplyPayoutResult: %v", err)
		}
		a := mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
		if a.State != AttemptAmbiguous {
			t.Fatalf("setup: want ambiguous, got %s", a.State)
		}
		before := e.b12Snapshot(t, wr)
		sw := e.b12Sweeper()
		sw.MaxResubmits = 1
		if err := sw.resubmitPayoutAmbiguous(context.Background(), f.tenantID, a); err != nil {
			t.Fatalf("resubmitPayoutAmbiguous: %v", err)
		}
		if spy.count() != 1 {
			t.Fatalf("no resend may happen at the cap, Withdraw calls = %d", spy.count())
		}
		e.b12AssertOneAlert(t, a.ID, "max_resubmits_exhausted")
		e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: before})
	})
}

// An already-escalated attempt does not escalate (or raise) again; the T14 receipt replay after
// the dispute is a no-op cell and raises nothing more.
func TestB12_NoSecondRaise_OnReplayOrAlreadyEscalated(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-replay")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-replay"}

	wr, a := e.b12Ambiguous(t, "b12-replay-esc")
	sw := e.b12Sweeper()
	for i := 0; i < 3; i++ {
		if err := sw.escalateAmbiguousPayout(context.Background(), f.tenantID, mustGetAttempt(t, pool, f.tenantID, a.ID), time.Now().Add(time.Minute), "non_idempotent_manifest"); err != nil {
			t.Fatalf("escalate #%d: %v", i, err)
		}
	}
	e.b12AssertOneAlert(t, a.ID, "non_idempotent_manifest") // occurrences == 1
	if n := e.b12AuditCount(t, "payments.payout_resend_escalated", a.ID); n != 1 {
		t.Fatalf("escalation audits = %d, want 1", n)
	}

	wr2, a2 := e.claim(t, "b12-replay-t14")
	e.b12Decline(t, wr2, a2)
	e.b12Callback(t, a2, OutcomeSucceeded, "b12-replay-ref", 500)
	e.b12Callback(t, a2, OutcomeSucceeded, "b12-replay-ref", 500) // redelivery: a no-op cell
	var t14 int
	for _, r := range e.b12Rows(t) {
		if strings.Contains(r.Discriminator, a2.ID.String()) {
			t14++
			if r.Occurrences != 1 {
				t.Fatalf("a replayed receipt must not add an occurrence: %+v", r)
			}
		}
	}
	if t14 != 1 {
		t.Fatalf("T14 alerts for the attempt = %d, want 1", t14)
	}
	// A stale synchronous success replayed on the already-disputed attempt is the benign
	// "already resolved" cell of applyPayoutLateEvidence: no dispute write, no new occurrence.
	e.apply(t, wr2, a2, rbResult(ErrorClassSucceeded, OutcomeSucceeded, "b12-replay-ref"))
	for _, r := range e.b12Rows(t) {
		if strings.Contains(r.Discriminator, a2.ID.String()) && r.Occurrences != 1 {
			t.Fatalf("a benign replay on a disputed attempt must not raise again: %+v", r)
		}
	}
	if rows := e.b12Rows(t); len(rows) != 2 {
		t.Fatalf("alerts = %d, want 2 (one escalation, one T14): %+v", len(rows), rows)
	}
	_ = wr
}

// Duplicate discriminator: the same attempt and reason raised again is ONE open alert with a
// growing occurrence count; a different reason on the same attempt is a separate alert.
func TestB12_DuplicateDiscriminator_OneOpenAlertGrowingOccurrences(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-dup")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-dup"}
	_, a := e.claim(t, "b12-dup")

	raise := func(reason string) {
		t.Helper()
		pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			return raisePayoutDisputeAlert(ctx, tx, a, reason)
		})
		if err != nil {
			t.Fatalf("raise: %v", err)
		}
		pending.Flush(context.Background())
	}
	raise("success_after_payout_declined")
	raise("success_after_payout_declined")
	raise("success_after_payout_declined")
	rows := e.b12Rows(t)
	if len(rows) != 1 || rows[0].Occurrences != 3 || rows[0].State != "open" {
		t.Fatalf("want one open alert with 3 occurrences, got %+v", rows)
	}
	raise("amount_asset_mismatch")
	if rows := e.b12Rows(t); len(rows) != 2 {
		t.Fatalf("a different reason is a different alert, got %+v", rows)
	}
}

// An unknown reason is never echoed into the discriminator: it becomes "unclassified".
func TestB12_UnknownReason_BecomesUnclassified_NeverEchoed(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-unc")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-unc"}
	_, a := e.claim(t, "b12-unc")
	hostile := "PROVIDER SAID: card 4111 closed"
	if _, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		return raisePayoutDisputeAlert(ctx, tx, a, hostile)
	}); err != nil {
		t.Fatalf("raise: %v", err)
	}
	e.b12AssertOneAlert(t, a.ID, alertReasonUnclassified, "PROVIDER", "4111")
}

// Tenant isolation: the alert's subject is the attempt's tenant; another tenant sees nothing.
func TestB12_TenantIsolation_SubjectOnly(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-ti")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-ti"}
	other, _, _ := fpOrch(t, pool, "mock-b12-ti2")

	wr, a := e.claim(t, "b12-ti")
	e.b12Decline(t, wr, a)
	e.b12Callback(t, a, OutcomeSucceeded, "b12-ti-ref", 500)
	e.b12AssertOneAlert(t, a.ID, "success_after_payout_declined")
	if rows := alertinject.ForSubject(t, pool, other.tenantID); len(rows) != 0 {
		t.Fatalf("another tenant must not see the alert: %+v", rows)
	}
}

// ---------------------------------------------------------------------------------------------
// Failure semantics (ADR 0102 7.2/7.3).

type b12Site struct {
	name string
	// setup prepares the attempt and returns the act, the alert reason, and a check that the
	// dispute/escalation did (committed=true) or did not (false) persist.
	setup func(t *testing.T, e rbEnv) (act func() error, attemptID uuid.UUID, reason, auditAction string, committed func(t *testing.T) bool)
}

func b12Sites() []b12Site {
	return []b12Site{
		{name: "sync_phase_C", setup: func(t *testing.T, e rbEnv) (func() error, uuid.UUID, string, string, func(*testing.T) bool) {
			wr, a := e.claim(t, "b12-f-sync")
			over := strings.Repeat("a", providerref.MaxBytes+1)
			gr := GateResult[WithdrawResult]{Class: ErrorClassProviderRefInvalid, Value: WithdrawResult{ProviderReference: over},
				Err: providerref.Validate("withdraw.provider_reference", over)}
			return func() error {
					return ApplyPayoutResult(context.Background(), e.pool, e.f.tenantID, wr.ID, a, gr, EvidenceSync, WithDestinations(pitest.Shared()))
				}, a.ID, TerminalReasonInvalidProviderReference, "payments.payout_parked_invalid_reference",
				func(t *testing.T) bool { return mustGetAttempt(t, e.pool, e.f.tenantID, a.ID).State == AttemptDisputed }
		}},
		{name: "status_evidence", setup: func(t *testing.T, e rbEnv) (func() error, uuid.UUID, string, string, func(*testing.T) bool) {
			wr, a := e.claim(t, "b12-f-st")
			over := strings.Repeat("a", providerref.MaxBytes+1)
			gr := GateResult[StatusResult]{Class: ErrorClassProviderRefInvalid, Err: providerref.Validate("query_status.provider_reference", over)}
			return func() error {
					return applyPayoutStatusEvidence(context.Background(), e.pool, e.f.tenantID, wr.ID, a, gr, EvidenceQueryStatus, time.Now().Add(time.Minute), nil, WithDestinations(pitest.Shared()))
				}, a.ID, TerminalReasonInvalidProviderReference, "payments.payout_parked_invalid_reference",
				func(t *testing.T) bool { return mustGetAttempt(t, e.pool, e.f.tenantID, a.ID).State == AttemptDisputed }
		}},
		{name: "callback_T14", setup: func(t *testing.T, e rbEnv) (func() error, uuid.UUID, string, string, func(*testing.T) bool) {
			wr, a := e.claim(t, "b12-f-cb")
			e.b12Decline(t, wr, a)
			return func() error { return e.b12CallbackErr(a, OutcomeSucceeded, "b12-f-cb-ref", 500) },
				a.ID, "success_after_payout_declined", "",
				func(t *testing.T) bool { return mustGetAttempt(t, e.pool, e.f.tenantID, a.ID).State == AttemptDisputed }
		}},
		{name: "T16_escalation", setup: func(t *testing.T, e rbEnv) (func() error, uuid.UUID, string, string, func(*testing.T) bool) {
			_, a := e.b12Ambiguous(t, "b12-f-esc")
			return func() error {
					return e.b12Sweeper().escalateAmbiguousPayout(context.Background(), e.f.tenantID, a, time.Now().Add(time.Minute), "non_idempotent_manifest")
				}, a.ID, "non_idempotent_manifest", "payments.payout_resend_escalated",
				func(t *testing.T) bool { return mustGetAttempt(t, e.pool, e.f.tenantID, a.ID).EscalatedAt != nil }
		}},
	}
}

// A DETERMINISTIC alert failure (P0001 / 23514) never rolls the dispute back: the dispute and its
// audit row commit, the call succeeds, and the only effect is that no alert is visible.
func TestB12_DeterministicAlertFailure_DoesNotRollBackTheDispute(t *testing.T) {
	pool := depositV2ScratchPool(t)
	n := 0
	for _, site := range b12Sites() {
		for _, code := range []string{"P0001", "23514"} {
			t.Run(site.name+"/"+code, func(t *testing.T) {
				n++
				id := fmt.Sprintf("mock-b12-det-%d", n)
				f, orch, _ := fpOrch(t, pool, id)
				e := rbEnv{pool: pool, f: f, orch: orch, pid: id}
				act, attemptID, _, auditAction, committed := site.setup(t, e)
				alertinject.Install(t, pool, f.tenantID, alertinject.Persistent, code)
				if err := act(); err != nil {
					t.Fatalf("a deterministic alert failure must never fail the business transaction: %v", err)
				}
				if !committed(t) {
					t.Fatalf("the dispute/escalation must be committed despite the alert failure")
				}
				if auditAction != "" && e.b12AuditCount(t, auditAction, attemptID) != 1 {
					t.Fatalf("the audit row must be committed with the dispute")
				}
				if rows := e.b12Rows(t); len(rows) != 0 {
					t.Fatalf("a persistent failure leaves no tenant-visible alert: %+v", rows)
				}
			})
		}
	}
}

// The detached retry after the commit persists the P1 when only the in-transaction raise failed.
func TestB12_InTxFailure_DetachedRetryAfterCommitPersistsTheP1(t *testing.T) {
	pool := depositV2ScratchPool(t)
	n := 0
	for _, site := range b12Sites() {
		t.Run(site.name, func(t *testing.T) {
			n++
			id := fmt.Sprintf("mock-b12-ret-%d", n)
			f, orch, _ := fpOrch(t, pool, id)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: id}
			act, attemptID, reason, _, committed := site.setup(t, e)
			alertinject.Install(t, pool, f.tenantID, alertinject.InTxOnly, "P0001")
			if err := act(); err != nil {
				t.Fatalf("act: %v", err)
			}
			if !committed(t) {
				t.Fatalf("the dispute/escalation must be committed")
			}
			e.b12AssertOneAlert(t, attemptID, reason) // persisted by Pending.Flush after the commit
		})
	}
}

// A TRANSIENT alert failure (deadlock class) propagates and rolls the WHOLE transaction back: the
// dispute, its audit row and any state change are not committed, so redelivery or the sweeper
// retries the evidence.
func TestB12_TransientAlertFailure_PropagatesAndRollsBackEverything(t *testing.T) {
	pool := depositV2ScratchPool(t)
	n := 0
	for _, site := range b12Sites() {
		t.Run(site.name, func(t *testing.T) {
			n++
			id := fmt.Sprintf("mock-b12-tr-%d", n)
			f, orch, _ := fpOrch(t, pool, id)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: id}
			act, attemptID, _, auditAction, committed := site.setup(t, e)
			alertinject.Install(t, pool, f.tenantID, alertinject.Persistent, "40P01")
			if err := act(); err == nil {
				t.Fatalf("a transient alert failure must propagate")
			}
			if committed(t) {
				t.Fatalf("the whole transaction must roll back on a transient alert failure")
			}
			if auditAction != "" && e.b12AuditCount(t, auditAction, attemptID) != 0 {
				t.Fatalf("the audit row must roll back with the dispute")
			}
			if rows := e.b12Rows(t); len(rows) != 0 {
				t.Fatalf("no alert on a rolled-back transaction: %+v", rows)
			}
		})
	}
}

// Money safety under the fault paths: a deterministic failure leaves the hold, the ledger and the
// balance exactly as the dispute-without-alert would.
func TestB12_FaultPaths_MoneyUnchanged(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-money")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-money"}
	wr, a := e.claim(t, "b12-money")
	e.b12Decline(t, wr, a)
	before := e.b12Snapshot(t, wr)
	alertinject.Install(t, pool, f.tenantID, alertinject.Persistent, "P0001")
	if err := e.b12CallbackErr(a, OutcomeSucceeded, "b12-money-ref", 500); err != nil {
		t.Fatalf("callback: %v", err)
	}
	e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: before})
}

// LF M-1/C1: a decline replayed on an already-declined attempt (a stale snapshot) is a clean
// no-op: the attempt stays declined, no dispute, no audit row, no alert, nothing posts. A
// SUCCESS after the decline stays T14 (the table above), and a decline echoing a foreign
// reference is still parked by the reference guard and raises.
func TestB12_LateDeclineReplay_IsCleanNoOp(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-ld")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-ld"}
	wr, a := e.claim(t, "b12-ld")
	e.b12Decline(t, wr, a)
	before := e.b12Snapshot(t, wr)
	e.apply(t, wr, a, rbResult(ErrorClassDefiniteDecline, OutcomeDeclined, "")) // stale replay
	got := mustGetAttempt(t, pool, f.tenantID, a.ID)
	if got.State != AttemptDeclined || got.TerminalReason != nil {
		t.Fatalf("a decline replay must leave the attempt declined, got %s %v", got.State, got.TerminalReason)
	}
	if n := e.b12AuditCount(t, "payments.payout_late_contradicting_evidence", a.ID); n != 0 {
		t.Fatalf("no dispute audit for a decline replay, got %d", n)
	}
	if rows := e.b12Rows(t); len(rows) != 0 {
		t.Fatalf("no alert for a decline replay: %+v", rows)
	}
	e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: before})
}

// LF LOW-3: a concurrent decline (callback and status evidence) racing the synchronous phase-C
// decline converges on one clean declined attempt: no dispute, no alert, the hold released once.
func TestB12_ConcurrentDeclines_ConvergeCleanly_NoAlert(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-cd")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-cd"}
	wr, a := e.claim(t, "b12-cd")
	ledgerBefore := fpLedgerTx(t, pool, f)

	errs := make(chan error, 3)
	start := make(chan struct{})
	go func() {
		<-start
		errs <- ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, a, rbResult(ErrorClassDefiniteDecline, OutcomeDeclined, ""), EvidenceSync, WithDestinations(pitest.Shared()))
	}()
	go func() {
		<-start
		errs <- e.b12CallbackErr(a, OutcomeDeclined, "b12-cd-ref", 500)
	}()
	go func() {
		<-start
		errs <- applyPayoutStatusEvidence(context.Background(), pool, f.tenantID, wr.ID, a,
			GateResult[StatusResult]{Class: ErrorClassDefiniteDecline, Value: StatusResult{Outcome: OutcomeDeclined}},
			EvidenceQueryStatus, time.Now().Add(time.Minute), nil, WithDestinations(pitest.Shared()))
	}()
	close(start)
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("concurrent decline #%d: %v", i, err)
		}
	}
	got := mustGetAttempt(t, pool, f.tenantID, a.ID)
	if got.State != AttemptDeclined {
		t.Fatalf("want one clean declined attempt, got %s %v", got.State, got.TerminalReason)
	}
	if rows := e.b12Rows(t); len(rows) != 0 {
		t.Fatalf("concurrent declines must raise nothing: %+v", rows)
	}
	w := fpReqState(t, pool, f, wr.ID)
	if w.State != withdrawal.StateFailed || w.ReleaseLedgerTransactionID == nil {
		t.Fatalf("the hold must be released exactly once: %+v", w)
	}
	if n := fpLedgerTx(t, pool, f); n != ledgerBefore+1 {
		t.Fatalf("ledger transactions %d -> %d, want exactly one release posting", ledgerBefore, n)
	}
	loAssertBalanced(t, pool, f.tenantID)
}

// LF LOW-3: retry convergence. A transient alert failure rolls the whole dispute back; once the
// fault is gone (the injected trigger is dropped when the subtest ends) the redelivery converges
// to exactly one dispute and one alert (occurrences 1), and a further duplicate adds nothing.
func TestB12_TransientFailureThenRedelivery_ConvergesToOneDisputeOneAlert(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-rc")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-rc"}
	wr, a := e.claim(t, "b12-rc")
	e.b12Decline(t, wr, a)
	before := e.b12Snapshot(t, wr)

	t.Run("faulty_delivery", func(t *testing.T) {
		alertinject.Install(t, pool, f.tenantID, alertinject.Persistent, "40P01")
		if err := e.b12CallbackErr(a, OutcomeSucceeded, "b12-rc-ref", 500); err == nil {
			t.Fatalf("the transient failure must propagate")
		}
		if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptDeclined {
			t.Fatalf("the failed delivery must roll the dispute back, got %s", got.State)
		}
	}) // cleanup drops the trigger here

	for i := 0; i < 2; i++ {
		if err := e.b12CallbackErr(a, OutcomeSucceeded, "b12-rc-ref", 500); err != nil {
			t.Fatalf("redelivery #%d: %v", i, err)
		}
	}
	if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptDisputed {
		t.Fatalf("redelivery must converge to disputed, got %s", got.State)
	}
	e.b12AssertOneAlert(t, a.ID, "success_after_payout_declined") // occurrences == 1
	e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: before})
}

// Security C-3 / L-2: a tenant-B session reading tenant A's payout alerts sees zero rows, by the
// alerts RLS (unfiltered SELECT under the other tenant's session), for each payout alert kind of site.
func TestB12_OtherTenantSessionSeesZeroPayoutAlertRows(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-b12-rls")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-b12-rls"}
	other, _, _ := fpOrch(t, pool, "mock-b12-rls2")
	wr, a := e.claim(t, "b12-rls")
	e.b12Decline(t, wr, a)
	e.b12Callback(t, a, OutcomeSucceeded, "b12-rls-ref", 500)
	_, amb := e.b12Ambiguous(t, "b12-rls-esc")
	if err := e.b12Sweeper().escalateAmbiguousPayout(context.Background(), f.tenantID, amb, time.Now().Add(time.Minute), "non_idempotent_manifest"); err != nil {
		t.Fatalf("escalate: %v", err)
	}
	if rows := e.b12Rows(t); len(rows) != 2 {
		t.Fatalf("tenant A must see its two alerts, got %+v", rows)
	}
	var n, occ int
	if err := pool.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator LIKE 'payout_attempt:%'`).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_occurrences`).Scan(&occ)
	}); err != nil {
		t.Fatalf("tenant B read: %v", err)
	}
	if n != 0 || occ != 0 {
		t.Fatalf("tenant B session read %d alerts / %d occurrences of tenant A", n, occ)
	}
}

// Explicit lists (LF L-3), not a prefix: a new destination_* reason must be added here deliberately, with its own test.
var b13bTerminalReasonsCoveredElsewhere = map[string]bool{
	TerminalReasonDestinationMismatch:         true, // TestB13B_Echo_Sync_Mismatch_ParksEveryOutcome, _Poll, _Callback
	TerminalReasonDestinationIntegrityFailure: true, // TestB13B_Evidence_MissingOrTamperedSnapshot_ParksAsIntegrityFailure
}

var b13bEscalationReasonsCoveredElsewhere = map[string]bool{
	alertReasonPayoutDestinationNotUsable:     true, // TestB13B_T2_Reclaim_EscalatesOnBlockedDestination/suspended
	TerminalReasonDestinationIntegrityFailure: true, // TestB13B_T2_Reclaim_EscalatesOnBlockedDestination/snapshot_*
}
