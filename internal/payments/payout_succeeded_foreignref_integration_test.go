//go:build integration

// PAY-PAYOUT-SUCCEEDED-REF-MISMATCH-1 (ADR 0095 section 42.8, ledger-finance M-1; raise only): a
// success with MATCHING amount/asset but a DIFFERENT provider reference, arriving by callback on an
// already-SUCCEEDED payout (resolved to the attempt through the merchant reference, the new
// reference being unbound), used to fold silently into duplicate_effect with no audit and no alert.
// It now writes ONE audit row and raises ONE durable P1 (reason
// foreign_reference_success_on_succeeded_payout) as the last statement of the cell. No state change,
// no rebind of the stored reference, no release, settlement or posting, no delivery.
package payments

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

func (e rbEnv) m1StoredRef(t *testing.T, o b12Out) string {
	t.Helper()
	got := mustGetAttempt(t, e.pool, e.f.tenantID, o.a.ID)
	if got.ProviderReference == nil {
		return ""
	}
	return *got.ProviderReference
}

func TestM1_ForeignRefSuccessOnSucceededPayout_AuditAndOneP1_NoStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-m1")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-m1"}
	other, _, _ := fpOrch(t, pool, "mock-m1-other")
	a, o := e.r6Succeeded(t, "m1", "m1-stored-ref")

	disp, err := e.r5CallbackDisp(a, "m1-foreign-ref", 500, "EUR")
	if err != nil {
		t.Fatalf("callback: %v", err)
	}
	if disp != DispositionDuplicateEffect {
		t.Fatalf("disposition = %s, want %s (raise only, no effect)", disp, DispositionDuplicateEffect)
	}
	e.r6AssertStillSucceeded(t, o)
	if got := e.m1StoredRef(t, o); got != "m1-stored-ref" {
		t.Fatalf("the stored reference must never be rebound, got %q", got)
	}
	e.b12AssertOneAlert(t, a.ID, alertReasonPayoutForeignRefSuccessOnSucceeded, "m1-foreign-ref", "m1-stored-ref", "EUR")

	rows := r8ReadAudit(t, e, f.tenantID, auditActionPayoutSucceededForeignRef, a.ID)
	if len(rows) != 1 {
		t.Fatalf("want exactly one foreign-reference audit row, got %d", len(rows))
	}
	r := rows[0]
	if r.ActorType != "system" || r.TargetType != "payment_attempt" || r.Outcome != "denied" {
		t.Fatalf("audit shape: %+v", r)
	}
	for k, want := range map[string]any{
		"reason": alertReasonPayoutForeignRefSuccessOnSucceeded, "attempt_state": "succeeded", "evidence": "callback",
		"provider_id": "mock-m1", "stored_reference": "m1-stored-ref", "echoed_provider_reference": "m1-foreign-ref",
		"stored_asset_code": "EUR",
	} {
		if r.Metadata[k] != want {
			t.Fatalf("audit metadata %s = %v, want %v (%v)", k, r.Metadata[k], want, r.Metadata)
		}
	}
	// The foreign-reference receipt is resolved anomaly_other (not applied) and stored once.
	var resolution string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolution FROM payment_provider_events WHERE tenant_id=$1 AND provider_reference=$2`,
			f.tenantID, "m1-foreign-ref").Scan(&resolution)
	}); err != nil || resolution != string(ResolutionAnomalyOther) {
		t.Fatalf("foreign-reference receipt resolution = %q (%v), want %s", resolution, err, ResolutionAnomalyOther)
	}
	if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 0 {
		t.Fatalf("the amount/asset mismatch audit must not be written (they match), got %d", n)
	}
	if n := e.r8DisputeAudits(t, a.ID); n != 0 {
		t.Fatalf("nothing is parked: no dispute audit, got %d", n)
	}
	// Tenant isolation: another tenant sees neither the alert nor the audit row.
	if n := alertinjectForOther(t, e, other.tenantID); n != 0 {
		t.Fatalf("another tenant must see no alert, got %d rows", n)
	}
	if rows := r8ReadAudit(t, e, other.tenantID, auditActionPayoutSucceededForeignRef, a.ID); len(rows) != 0 {
		t.Fatalf("another tenant must not see the audit row, got %d", len(rows))
	}
}

// A true duplicate (the SAME reference, matching amount/asset) raises nothing and audits nothing.
func TestM1_SameRefDuplicateSuccess_DoesNotRaise(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-m1-same")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-m1-same"}
	a, o := e.r6Succeeded(t, "m1-same", "m1-same-ref")
	for i := 0; i < 3; i++ {
		disp, err := e.r5CallbackDisp(a, "m1-same-ref", 500, "EUR")
		if err != nil || disp != DispositionDuplicateEffect {
			t.Fatalf("delivery #%d: %s %v", i, disp, err)
		}
	}
	if rows := e.b12Rows(t); len(rows) != 0 {
		t.Fatalf("a same-reference duplicate must not raise: %+v", rows)
	}
	if n := e.b12AuditCount(t, auditActionPayoutSucceededForeignRef, a.ID); n != 0 {
		t.Fatalf("a same-reference duplicate writes no audit row, got %d", n)
	}
	e.r6AssertStillSucceeded(t, o)
}

// Replay: three byte-identical foreign-reference deliveries leave ONE open alert whose occurrences
// grow to 3; the receipt is deduped; money, ledger and state are unchanged.
func TestM1_Replay_OneAlertRow_OccurrencesGrow_NoStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-m1-replay")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-m1-replay"}
	a, o := e.r6Succeeded(t, "m1-replay", "m1-replay-ref")
	base := e.r8Events(t)
	for i := 0; i < 3; i++ {
		if err := e.r5Callback(a, "m1-replay-foreign", 500, "EUR"); err != nil {
			t.Fatalf("delivery #%d: %v", i, err)
		}
	}
	rows := e.b12Rows(t)
	if len(rows) != 1 || rows[0].State != "open" || rows[0].Occurrences != 3 ||
		!strings.HasSuffix(rows[0].Discriminator, ":reason:"+alertReasonPayoutForeignRefSuccessOnSucceeded) {
		t.Fatalf("want ONE open alert whose occurrences grew to 3, got %+v", rows)
	}
	if got := e.r8Events(t); got != base+1 {
		t.Fatalf("the receipt must be deduped to one row: %d -> %d", base, got)
	}
	// PAY-PAYOUT-CALLBACK-AUDIT-2: the audit row is written once for the first (new) receipt, not per redelivery.
	if n := e.b12AuditCount(t, auditActionPayoutSucceededForeignRef, a.ID); n != 1 {
		t.Fatalf("foreign-reference audit rows = %d, want 1 after three identical deliveries", n)
	}
	e.r6AssertStillSucceeded(t, o)
}

// Failure semantics as R-6 (ADR 0102 7.2/7.3).
func TestM1_AlertFailureSemantics(t *testing.T) {
	pool := depositV2ScratchPool(t)
	n := 0
	mk := func(t *testing.T) (rbEnv, PaymentAttempt, b12Out, int) {
		n++
		pid := fmt.Sprintf("mock-m1-f-%d", n)
		f, orch, _ := fpOrch(t, pool, pid)
		e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
		a, o := e.r6Succeeded(t, fmt.Sprintf("m1-f-%d", n), "m1-f-ref")
		return e, a, o, e.r8Events(t)
	}
	audits := func(e rbEnv, a PaymentAttempt) int {
		return e.b12AuditCount(t, auditActionPayoutSucceededForeignRef, a.ID)
	}
	t.Run("deterministic", func(t *testing.T) {
		e, a, o, _ := mk(t)
		alertinjectInstall(t, e, "P0001", true)
		if err := e.r5Callback(a, "m1-f-foreign", 500, "EUR"); err != nil {
			t.Fatalf("a deterministic alert failure must not fail the delivery: %v", err)
		}
		if audits(e, a) != 1 {
			t.Fatalf("the audit row must be committed")
		}
		if rows := e.b12Rows(t); len(rows) != 0 {
			t.Fatalf("no alert is visible after a persistent failure: %+v", rows)
		}
		e.r6AssertStillSucceeded(t, o)
	})
	t.Run("in_tx_failure_detached_retry_persists", func(t *testing.T) {
		e, a, o, _ := mk(t)
		alertinjectInstall(t, e, "P0001", false)
		if err := e.r5Callback(a, "m1-f-foreign", 500, "EUR"); err != nil {
			t.Fatalf("callback: %v", err)
		}
		e.b12AssertOneAlert(t, a.ID, alertReasonPayoutForeignRefSuccessOnSucceeded)
		e.r6AssertStillSucceeded(t, o)
	})
	t.Run("transient_rolls_back_then_converges", func(t *testing.T) {
		e, a, o, base := mk(t)
		t.Run("faulty", func(t *testing.T) {
			alertinjectInstall(t, e, "40P01", true)
			if err := e.r5Callback(a, "m1-f-foreign", 500, "EUR"); err == nil {
				t.Fatalf("a transient alert failure must propagate")
			}
			if audits(e, a) != 0 || e.r8Events(t) != base {
				t.Fatalf("audit row and receipt must roll back with the delivery")
			}
		})
		if err := e.r5Callback(a, "m1-f-foreign", 500, "EUR"); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		if audits(e, a) != 1 {
			t.Fatalf("one audit row after recovery")
		}
		e.b12AssertOneAlert(t, a.ID, alertReasonPayoutForeignRefSuccessOnSucceeded)
		e.r6AssertStillSucceeded(t, o)
	})
}

// Deposits are unchanged by M-1: a matching success with a DIFFERENT reference on a SUCCEEDED
// deposit writes no foreign-reference audit row, raises nothing (payout only) and changes nothing.
func TestM1_Deposit_ForeignRefSuccessOnSucceeded_Unchanged(t *testing.T) {
	pool := depositV2ScratchPool(t)
	pid := "mock-psp-m1-dep"
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider(pid, "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{pid: provider}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(provider)})
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "m1-dep",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	ref := "m1-dep-ref"
	if res.Attempt.ProviderReference != nil {
		ref = *res.Attempt.ProviderReference
	}
	deliver := func(r string) ReceiptDisposition {
		var disp ReceiptDisposition
		pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			disp, err = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, pid, ReceiptEvidence{
				EventType: "deposit", ProviderReference: r, MerchantReference: res.Attempt.MerchantReference,
				Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
			})
			return err
		})
		if err != nil {
			t.Fatalf("ApplyReceiptEvidence: %v", err)
		}
		pending.Flush(context.Background())
		return disp
	}
	if d := deliver(ref); d != DispositionApplied {
		t.Fatalf("setup: matching success disposition = %s, want applied", d)
	}
	if d := deliver("m1-dep-foreign"); d != DispositionDuplicateEffect {
		t.Fatalf("a deposit foreign-reference success must stay duplicate_effect, got %s", d)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID); got.State != AttemptSucceeded {
		t.Fatalf("state = %s, want succeeded", got.State)
	}
	if rows := alertinject.ForSubject(t, pool, f.tenantID); len(rows) != 0 {
		t.Fatalf("a deposit must raise no payout alert, got %+v", rows)
	}
	if n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2 AND target_id=$3`,
		f.tenantID, auditActionPayoutSucceededForeignRef, res.Attempt.ID.String()); n != 0 {
		t.Fatalf("a deposit writes no payout foreign-reference audit row, got %d", n)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}
