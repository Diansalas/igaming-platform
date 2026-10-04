//go:build integration

// ADR 0102 I-wire (ALERT-DELIVERY-1): durable P1 alerts for the deposit
// evidence transactions. Assertions are on durable alert rows. The financial
// rules proven here:
//   - the dispute / receipt / posting outcome NEVER depends on the alert row
//     (an injected raise failure inside the evidence transaction leaves the
//     dispute committed, the money untouched, and the response unchanged);
//   - a swallowed in-tx raise is re-raised post-commit (detached);
//   - only the alert statement's own error is swallowed (25P02 is not masked);
//   - discriminators are server-side ids and closed reasons, attributes stay on
//     the allowlist, and no raw provider reference or error text leaks.
package payments

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

func iwAlerts(t *testing.T, e *depRefEnv) []alertinject.Row {
	t.Helper()
	return alertinject.ForSubject(t, e.pool, e.f.tenantID)
}

// iwParkAlert asserts exactly one payment.webhook_integrity P1 for the attempt
// with the closed reason, carrying only the allowlisted provider_id attribute.
func iwParkAlert(t *testing.T, e *depRefEnv, attemptID uuid.UUID, reasonValue string, leakProbes ...string) alertinject.Row {
	t.Helper()
	want := "attempt:" + attemptID.String() + ":reason:" + reasonValue
	var hit []alertinject.Row
	for _, r := range iwAlerts(t, e) {
		if r.Kind == string(alerting.KindPaymentWebhookIntegrity) && r.Discriminator == want {
			hit = append(hit, r)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("want exactly one P1 with discriminator %q, got %d (all: %+v)", want, len(hit), iwAlerts(t, e))
	}
	r := hit[0]
	if r.Severity != "p1" {
		t.Fatalf("severity %s, want p1", r.Severity)
	}
	if r.Attributes["provider_id"] != e.id {
		t.Fatalf("provider_id attribute = %v, want %q", r.Attributes["provider_id"], e.id)
	}
	for k := range r.Attributes {
		if k != "provider_id" {
			t.Fatalf("unexpected attribute %q on a park alert: %v", k, r.Attributes)
		}
	}
	blob := fmt.Sprint(r.Attributes, r.Discriminator)
	for _, p := range leakProbes {
		if p != "" && strings.Contains(blob, p) {
			t.Fatalf("alert leaks %q: %s", p, blob)
		}
	}
	return r
}

func TestIWire_T10Park_SyncAmountMismatch_RaisesP1(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-sam")
	ref := "iw-sync-mm-" + uuid.NewString()
	e.p.setScript(scriptSyncEcho(ref, 4999, "EUR"))
	res := rvInit(t, pool, e.orch, e.f, 5000, "iw-sam")
	a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonSyncAmountMismatch)
	iwParkAlert(t, e, a.ID, TerminalReasonSyncAmountMismatch, ref)
}

func TestIWire_T10Park_ProviderReferenceConflict_RaisesP1(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-prc")
	const payoutRef = "iw-payout-bound-ref"
	seedPayoutAttemptBoundTo(t, pool, e.f, e.id, payoutRef)
	e.p.setScript(scriptOutcome(OutcomePending, payoutRef))
	res := rvInit(t, pool, e.orch, e.f, 5000, "iw-prc")
	a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonProviderReferenceConflict)
	iwParkAlert(t, e, a.ID, TerminalReasonProviderReferenceConflict, payoutRef)
}

// invalid_provider_reference:<detail> collapses to its closed prefix; the
// hostile reference never reaches the alert.
func TestIWire_T10Park_InvalidProviderReference_RaisesP1_NoRawReference(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-ipr")
	e.p.setScript(scriptOutcome(OutcomePending, "ref\x07bellX"))
	res := rvInit(t, pool, e.orch, e.f, 5000, "iw-ipr")
	a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonInvalidProviderReference+":control_char")
	iwParkAlert(t, e, a.ID, TerminalReasonInvalidProviderReference, "bellX", "control_char")
}

func TestIWire_T10Park_PollAmountAndReferenceMismatch_RaiseP1(t *testing.T) {
	pool := testPool(t)
	t.Run("poll_amount_mismatch", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-iw-pam")
		a, ref := e.ambiguousBound(t, "iw-pam")
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 4999, "EUR")))
		e.assertPollParked(t, a, ref, TerminalReasonPollAmountMismatch)
		iwParkAlert(t, e, a.ID, TerminalReasonPollAmountMismatch, ref)
	})
	t.Run("poll_reference_mismatch", func(t *testing.T) {
		e := newDepRefEnv(t, pool, "mock-iw-prm")
		a, ref := e.ambiguousBound(t, "iw-prm")
		e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess("other-ref-VALID1", 5000, "EUR")))
		e.assertPollParked(t, a, ref, TerminalReasonPollReferenceMismatch)
		iwParkAlert(t, e, a.ID, TerminalReasonPollReferenceMismatch, ref, "other-ref-VALID1")
	})
}

// PAY-POLL-DECLINED-ALERT-RECON-1 (a): the declined-attempt contradiction
// audit action is a P1 Kind (closed sub-reason in the discriminator).
func TestIWire_PollEvidenceContradictsTerminalAttempt_RaisesP1(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-pdc")
	a, ref := e.ambiguousBound(t, "iw-pdc")
	e.deliverCallbackOnce(t, OutcomeDeclined, 5000, "provider_unavailable")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 4999, "EUR")))
	if n := e.auditCount(t, "payments.poll_evidence_contradicts_terminal_attempt", a.ID); n != 1 {
		t.Fatalf("setup: contradiction audits = %d, want 1", n)
	}
	iwParkAlert(t, e, a.ID, alertReasonPollContradictsTermin+":"+TerminalReasonPollAmountMismatch, ref)
}

// A receipt whose success contradicts the live attempt's amount: T10 from the
// callback path (ApplyReceiptEvidence), transaction owned by alerting.InTx.
func iwApplyReceiptInTx(t *testing.T, e *depRefEnv, ev ReceiptEvidence) (ReceiptDisposition, error) {
	t.Helper()
	var d ReceiptDisposition
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(e.pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.id, ev)
		return err
	})
	if err == nil {
		pending.Flush(context.Background())
	}
	return d, err
}

func TestIWire_ReceiptT10_CallbackAmountAssetMismatch_RaisesP1(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-cam")
	a, ref := e.ambiguousBound(t, "iw-cam")
	d, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR"})
	if err != nil || d != DispositionApplied {
		t.Fatalf("receipt: %v %v", d, err)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != TerminalReasonCallbackAmountAssetMismatch {
		t.Fatalf("attempt state=%s reason=%v", got.State, got.TerminalReason)
	}
	iwParkAlert(t, e, a.ID, TerminalReasonCallbackAmountAssetMismatch, ref)
}

// Failure injection INSIDE the receipt transaction: the dispute commits, the
// money is untouched, and the post-commit detached raise persists the P1.
func TestIWire_ReceiptT10_InjectedRaiseFailure_DisputeCommits_DetachedP1Persists(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-inj")
	a, ref := e.ambiguousBound(t, "iw-inj")
	alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")
	d, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR"})
	if err != nil || d != DispositionApplied {
		t.Fatalf("an alert failure must never fail the receipt: %v %v", d, err)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	if got.State != AttemptDisputed {
		t.Fatalf("the dispute must be committed despite the alert failure, state=%s", got.State)
	}
	if e.depositTxCount(t) != 0 || cashBalance(t, pool, e.f) != 0 {
		t.Fatalf("a callback mismatch must never post")
	}
	iwParkAlert(t, e, a.ID, TerminalReasonCallbackAmountAssetMismatch, ref) // persisted by the detached retry
}

func TestIWire_ReceiptT10_PersistentRaiseFailure_DisputeStillCommits(t *testing.T) {
	for _, code := range []string{"P0001", "23514"} {
		t.Run(code, func(t *testing.T) {
			pool := testPool(t)
			e := newDepRefEnv(t, pool, "mock-iw-per"+strings.ToLower(code[:2]))
			a, ref := e.ambiguousBound(t, "iw-per")
			alertinject.Install(t, pool, e.f.tenantID, alertinject.Persistent, code)
			d, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR"})
			if err != nil || d != DispositionApplied {
				t.Fatalf("receipt: %v %v", d, err)
			}
			if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID); got.State != AttemptDisputed {
				t.Fatalf("state=%s, want disputed", got.State)
			}
			if rows := iwAlerts(t, e); len(rows) != 0 {
				t.Fatalf("a persistent failure leaves no tenant-visible alert (the platform fallback is tenant-less): %+v", rows)
			}
		})
	}
}

// Addendum (a): the narrow swallow. An already-aborted outer transaction
// (25P02) and a deadlock-class error from the alert statement are NOT masked.
func TestIWire_NarrowSwallow_25P02AndDeadlockPropagate(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-ns")
	a, _ := e.ambiguousBound(t, "iw-ns")

	t.Run("25P02_outer_tx_already_aborted", func(t *testing.T) {
		var raiseErr error
		_, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			// Abort the outer transaction with a failing statement the caller (wrongly) swallows.
			_, _ = tx.Exec(ctx, `SELECT 1/0`)
			raiseErr = raiseDepositParkAlert(ctx, tx, a, e.id, TerminalReasonCallbackAmountAssetMismatch)
			return raiseErr
		})
		// The raise itself must return the 25P02 (CR finding 6): asserting only
		// that the transaction failed is vacuous, a commit of an aborted
		// transaction fails regardless.
		var pgErr *pgconn.PgError
		if !errors.As(raiseErr, &pgErr) || pgErr.Code != "25P02" {
			t.Fatalf("RaiseGuarded on an aborted outer transaction must return SQLSTATE 25P02, got %v", raiseErr)
		}
		if err == nil {
			t.Fatal("an aborted outer transaction must propagate, never be masked by the alert swallow")
		}
	})

	t.Run("40P01_from_the_alert_statement_propagates_and_rolls_back", func(t *testing.T) {
		alertinject.Install(t, pool, e.f.tenantID, alertinject.Persistent, "40P01")
		_, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			return raiseDepositParkAlert(ctx, tx, a, e.id, TerminalReasonCallbackAmountAssetMismatch)
		})
		if err == nil {
			t.Fatal("a deadlock-class error from the alert statement must propagate (retried by redelivery), not be swallowed")
		}
	})
}

// ADR 0102 8 rows 1: multiple success (T13d from a callback). Row 2's backstop
// is exercised by the INVDEP1 backstop test, extended with the durable P1.
func TestIWire_MultipleSuccess_RaisesP1_KeyedByIntent(t *testing.T) {
	s := newInvDep1Setup(t, "iw-ms-orig", "iw-ms-fb")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "iw-ms")
	ref := *res.Attempt.ProviderReference
	childID := declineCascadableAndFindChild(t, s.pool, s.orch, s.f, "iw-ms-orig", s.pa, ref)
	child := dispatchViaSweeper(t, s.pool, s.orch, s.f, childID)
	childRef := *child.ProviderReference
	if _, err := rvCallback(s.pool, s.orch, s.f, "iw-ms-fb",
		s.pb.CallbackPayload(s.f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success: %v", err)
	}
	if _, err := rvApplyReceipt(s.pool, s.orch, s.f.tenantID, "iw-ms-orig", ReceiptEvidence{
		EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	}); err != nil {
		t.Fatalf("T13d must not error: %v", err)
	}
	rows := alertinject.ForSubject(t, s.pool, s.f.tenantID)
	var hit []alertinject.Row
	for _, r := range rows {
		if r.Kind == string(alerting.KindPaymentMultipleSuccessForIntent) {
			hit = append(hit, r)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("want exactly one multiple-success alert, got %+v", rows)
	}
	r := hit[0]
	if r.Discriminator != "intent:"+res.Intent.ID.String() || r.Severity != "p1" {
		t.Fatalf("discriminator/severity: %+v", r)
	}
	if r.Attributes["attempt_id"] != res.Attempt.ID.String() || r.Attributes["evidence_kind"] != string(EvidenceCallback) || r.Attributes["provider_id"] != "iw-ms-orig" {
		t.Fatalf("attributes: %v", r.Attributes)
	}
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Fatalf("PAY-DOUBLE-CREDIT-1: balance=%d, want 5000", b)
	}
}

// Failure injected in the T13d transaction: the second capture stays disputed,
// unposted, and the P1 still lands via the post-commit detached raise.
func TestIWire_MultipleSuccess_InjectedRaiseFailure_StaysDisputedUnposted_DetachedP1(t *testing.T) {
	s := newInvDep1Setup(t, "iw-msi-orig", "iw-msi-fb")
	res := rvInit(t, s.pool, s.orch, s.f, 5000, "iw-msi")
	ref := *res.Attempt.ProviderReference
	childID := declineCascadableAndFindChild(t, s.pool, s.orch, s.f, "iw-msi-orig", s.pa, ref)
	child := dispatchViaSweeper(t, s.pool, s.orch, s.f, childID)
	childRef := *child.ProviderReference
	if _, err := rvCallback(s.pool, s.orch, s.f, "iw-msi-fb",
		s.pb.CallbackPayload(s.f.tenantID, CallbackEventDeposit, childRef, "", OutcomeSucceeded, 5000, "EUR", "", false)); err != nil {
		t.Fatalf("fallback success: %v", err)
	}
	alertinject.Install(t, s.pool, s.f.tenantID, alertinject.InTxOnly, "P0001")

	var d ReceiptDisposition
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(s.pool, s.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = ApplyReceiptEvidence(ctx, tx, s.orch, s.f.tenantID, "iw-msi-orig", ReceiptEvidence{
			EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"})
		return err
	})
	if err != nil || d != DispositionAnomaly {
		t.Fatalf("T13d must commit as an anomaly even if the alert raise fails: %v %v", d, err)
	}
	pending.Flush(context.Background())

	assertDisputedMultipleSuccess(t, s.pool, s.f.tenantID, res.Attempt.ID)
	if b := cashBalance(t, s.pool, s.f); b != 5000 {
		t.Fatalf("PAY-DOUBLE-CREDIT-1: balance=%d, want 5000", b)
	}
	if n := ledgerDepositTxCount(t, s.pool, s.f.tenantID, res.Intent.ID); n != 1 {
		t.Fatalf("deposit postings=%d, want exactly 1", n)
	}
	var found bool
	for _, r := range alertinject.ForSubject(t, s.pool, s.f.tenantID) {
		if r.Kind == string(alerting.KindPaymentMultipleSuccessForIntent) && r.Discriminator == "intent:"+res.Intent.ID.String() {
			found = true
		}
	}
	if !found {
		t.Fatal("the post-commit detached raise must persist the multiple-success P1")
	}
}

// Phase C (driveCreatedAttempt / InitiateDepositAttempt) owns its transaction
// through alerting.InTx: an in-tx raise failure in the T10 park is recovered by
// the Flush that follows the commit.
func TestIWire_PhaseC_T10Park_InjectedRaiseFailure_ParkCommits_DetachedP1Persists(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-pc")
	alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")
	e.p.setScript(scriptSyncEcho("iw-pc-"+uuid.NewString(), 4999, "EUR"))
	res := rvInit(t, pool, e.orch, e.f, 5000, "iw-pc")
	a := assertParkedNoMoney(t, e, e.f, res, TerminalReasonSyncAmountMismatch)
	iwParkAlert(t, e, a.ID, TerminalReasonSyncAmountMismatch)
}

// The sweeper's status-evidence transaction is an InTx owner too.
func TestIWire_Sweeper_T10Park_InjectedRaiseFailure_ParkCommits_DetachedP1Persists(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-sw")
	a, ref := e.ambiguousBound(t, "iw-sw")
	alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")
	e.mustNoSweepErrors(t, e.poll(t, a, ref, pollSuccess(ref, 4999, "EUR")))
	e.assertPollParked(t, a, ref, TerminalReasonPollAmountMismatch)
	iwParkAlert(t, e, a.ID, TerminalReasonPollAmountMismatch, ref)
}

// Tenant isolation: tenant B never sees tenant A's park alert, and the
// discriminator embeds only server-side ids.
func TestIWire_TenantIsolation_ParkAlertInvisibleToOtherTenant(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-ti")
	f2 := e.addTenant(t)
	e.p.setScript(scriptSyncEcho("iw-ti-"+uuid.NewString(), 4999, "EUR"))
	res := rvInit(t, pool, e.orch, e.f, 5000, "iw-ti")
	assertParkedNoMoney(t, e, e.f, res, TerminalReasonSyncAmountMismatch)
	if rows := alertinject.ForSubject(t, pool, f2.tenantID); len(rows) != 0 {
		t.Fatalf("tenant B must not see tenant A's alerts: %+v", rows)
	}
	if rows := iwAlerts(t, e); len(rows) != 1 {
		t.Fatalf("tenant A must see exactly its own alert: %+v", rows)
	}
}
