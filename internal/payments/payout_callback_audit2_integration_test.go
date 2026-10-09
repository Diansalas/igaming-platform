//go:build integration

// PAY-PAYOUT-CALLBACK-AUDIT-2 (ADR 0095 section 42.8; security F-2, ledger-finance L-3): the
// no-state-change terminal mismatch audit rows (payments.callback_amount_asset_mismatch_terminal on a
// DECLINED and on a SUCCEEDED attempt, and the M-1 foreign-reference row) are written once per NEW
// receipt, never again per redelivery (the L-e precedent). The raise stays unconditional (the alert
// dedupes). These tests add the two-goroutine concurrency cover for the audited cells and R-5/R-6/M-1.
package payments

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// runTwo runs f(0) and f(1) concurrently (released together) and fails on any error.
func runTwo(t *testing.T, f func(i int) error) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = f(i)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent delivery #%d: %v", i, err)
		}
	}
}

// R-5 (declined), R-6 (succeeded) and M-1 (foreign reference): two goroutines deliver the SAME
// mismatched event. Exactly ONE audit row (the first, new receipt), exactly ONE alert row (the
// raise is unconditional, so its occurrences count reaches 2), one receipt row, no state change.
func TestCallbackAudit2_Concurrent_IdenticalDeliveries_OneAuditRow_OneAlertRow_SignalCells(t *testing.T) {
	pool := depositV2ScratchPool(t)
	type cell struct {
		name   string
		action string
		reason string
		setup  func(t *testing.T, e rbEnv, key string) (PaymentAttempt, b12Out)
		ref    func(key string) string
		amount int64
		still  func(e rbEnv, t *testing.T, o b12Out)
	}
	cells := []cell{
		{"R5_declined_mismatch", r5Terminal, alertReasonPayoutMismatchedSuccessOnDeclined,
			func(t *testing.T, e rbEnv, key string) (PaymentAttempt, b12Out) {
				a, _, o := e.r5Declined(t, key)
				return a, o
			}, func(k string) string { return k + "-ref" }, 499, rbEnv.r5AssertStillDeclined},
		{"R6_succeeded_mismatch", r5Terminal, alertReasonPayoutMismatchedSuccessOnSucceeded,
			func(t *testing.T, e rbEnv, key string) (PaymentAttempt, b12Out) {
				return e.r6Succeeded(t, key, key+"-ref")
			},
			func(k string) string { return k + "-ref" }, 499, rbEnv.r6AssertStillSucceeded},
		{"M1_succeeded_foreign_ref", auditActionPayoutSucceededForeignRef, alertReasonPayoutForeignRefSuccessOnSucceeded,
			func(t *testing.T, e rbEnv, key string) (PaymentAttempt, b12Out) {
				return e.r6Succeeded(t, key, key+"-ref")
			},
			func(k string) string { return k + "-foreign" }, 500, rbEnv.r6AssertStillSucceeded},
	}
	n := 0
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			for rep := 0; rep < 3; rep++ {
				n++
				pid := fmt.Sprintf("mock-ca2-sig-%d", n)
				f, orch, _ := fpOrch(t, pool, pid)
				e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
				key := fmt.Sprintf("ca2-sig-%d", n)
				a, o := c.setup(t, e, key)
				base := e.r8Events(t)
				runTwo(t, func(int) error { return e.r5Callback(a, c.ref(key), c.amount, "EUR") })

				if got := e.b12AuditCount(t, c.action, a.ID); got != 1 {
					t.Fatalf("rep %d: audit rows = %d, want exactly 1 for two identical concurrent deliveries", rep, got)
				}
				rows := e.b12Rows(t)
				if len(rows) != 1 || rows[0].State != "open" || rows[0].Occurrences != 2 {
					t.Fatalf("rep %d: want ONE open alert row (raise is unconditional: occurrences 2), got %+v", rep, rows)
				}
				if want := "payout_attempt:" + a.ID.String() + ":reason:" + c.reason; rows[0].Discriminator != want {
					t.Fatalf("rep %d: discriminator %s, want %s", rep, rows[0].Discriminator, want)
				}
				if got := e.r8Events(t); got != base+1 {
					t.Fatalf("rep %d: the receipt must be deduped to one row: %d -> %d", rep, base, got)
				}
				c.still(e, t, o)
			}
		})
	}
}

// The audited dispute cells (T14, T15, callback amount/asset mismatch, tombstone): two goroutines
// deliver the same event, and (where the cell is reference-agnostic) two CONFLICTING events naming
// different references. Exactly one dispute, one dispute audit row and one alert row; the loser is a
// no-op cell. No money moves.
func TestCallbackAudit2_Concurrent_DisputeCells_OneAuditRow_OneAlertRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	for i, c := range r8Cells() {
		for _, conflicting := range []bool{false, true} {
			if conflicting && c.name == "tombstone_precedes_success" {
				continue // the tombstone is keyed on the one reference
			}
			t.Run(fmt.Sprintf("%s/conflicting=%v", c.name, conflicting), func(t *testing.T) {
				pid := fmt.Sprintf("mock-ca2-d-%d-%v", i, conflicting)
				f, orch, _ := fpOrch(t, pool, pid)
				e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
				wr, a := c.setup(t, e)
				snap := e.b12Snapshot(t, wr)
				runTwo(t, func(g int) error {
					ref, amount := c.ref, c.amount
					if conflicting && g == 1 {
						ref += "-other"
						if c.amount != 500 {
							amount = c.amount - 1 // still a mismatch for the mismatch cell
						}
					}
					return e.b12CallbackErr(a, OutcomeSucceeded, ref, amount)
				})
				got := mustGetAttempt(t, pool, f.tenantID, a.ID)
				if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != c.reason {
					t.Fatalf("want disputed/%s, got %s %v", c.reason, got.State, got.TerminalReason)
				}
				if n := e.r8DisputeAudits(t, a.ID); n != 1 {
					t.Fatalf("dispute audit rows = %d, want exactly 1", n)
				}
				e.b12AssertOneAlert(t, a.ID, c.reason)
				e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: snap})
			})
		}
	}
}

// The deferred-receipt drain applies each stored receipt exactly once (duplicate=false), so a
// mismatched success that was DEFERRED (unresolvable at delivery) and is applied by the drain on a
// now-SUCCEEDED payout still writes its one terminal-mismatch audit row and raises once.
func TestCallbackAudit2_DeferredDrain_MismatchedSuccessOnSucceeded_AuditedOnce(t *testing.T) {
	pool := depositV2ScratchPool(t)
	pid := "mock-ca2-drain"
	f, orch, _ := fpOrch(t, pool, pid)
	e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
	_, a := e.claim(t, "ca2-drain")
	deliver := func(merchantRef string, amount int64) error {
		pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(e.pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			_, err := ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.pid, ReceiptEvidence{
				EventType: "payout", ProviderReference: "ca2-drain-ref", MerchantReference: merchantRef,
				Outcome: OutcomeSucceeded, Amount: amount, AssetCode: "EUR",
			})
			return err
		})
		if err == nil {
			pending.Flush(context.Background())
		}
		return err
	}
	// 1. A mismatched success naming only a not-yet-bound reference: unresolvable, so DEFERRED.
	if err := deliver("", 499); err != nil {
		t.Fatalf("deferred delivery: %v", err)
	}
	if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 0 {
		t.Fatalf("nothing is audited while the receipt is deferred, got %d", n)
	}
	// 2. The matching success resolves through the merchant reference, settles the payout, binds the
	// reference and drains the deferred receipt onto the now-succeeded attempt (the R-6 cell).
	if err := deliver(a.MerchantReference, 500); err != nil {
		t.Fatalf("matching delivery: %v", err)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("want succeeded, got %s", got.State)
	}
	if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 1 {
		t.Fatalf("the drained mismatched receipt must write its one audit row, got %d", n)
	}
	e.b12AssertOneAlert(t, a.ID, alertReasonPayoutMismatchedSuccessOnSucceeded)
}

// ---- S-1 / C-1: the audit gate is "already RESOLVED", not "already existed" ----

// ca2AssertReceiptResolved: the mismatched receipt (amount) is resolved as anomaly_other and attached to the attempt.
func ca2AssertReceiptResolved(t *testing.T, pool *db.Pool, tenantID uuid.UUID, ref string, amount int64, attemptID uuid.UUID) {
	t.Helper()
	var resolution string
	var gotAttempt *uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT resolution, attempt_id FROM payment_provider_events WHERE tenant_id=$1 AND provider_reference=$2 AND amount=$3`,
			tenantID, ref, amount).Scan(&resolution, &gotAttempt)
	}); err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	if resolution != string(ResolutionAnomalyOther) || gotAttempt == nil || *gotAttempt != attemptID {
		t.Fatalf("receipt resolution=%q attempt_id=%v, want %s / %s", resolution, gotAttempt, ResolutionAnomalyOther, attemptID)
	}
}

func (e rbEnv) ca2Unresolved(t *testing.T, ref string) int {
	t.Helper()
	return fpCount(t, e.pool, e.f.tenantID, `SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1 AND provider_reference=$2 AND resolved_at IS NULL`, e.f.tenantID, ref)
}

// The exact two-transaction orphan (security probe): tx A stores a mismatched success as a DEFERRED
// receipt but has not committed while tx B resolves through the merchant reference, settles, binds the
// reference and drains (it cannot see A's row under READ COMMITTED). The orphan stays unresolved.
// The first redelivery is its first application: ONE audit row, the receipt becomes resolved; the next
// redeliveries write nothing more. The raise stays per delivery (one alert row).
func TestCallbackAudit2_OrphanedDeferredReceipt_RedeliveredAfterConcurrentBind_AuditedOnce_Resolved(t *testing.T) {
	pool := depositV2ScratchPool(t)
	pid := "mock-ca2-orphan"
	f, orch, _ := fpOrch(t, pool, pid)
	e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
	_, a := e.claim(t, "ca2-orphan")
	const ref = "ca2-orphan-ref"
	deliver := func(merchantRef string, amount int64) error {
		pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(e.pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			_, err := ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.pid, ReceiptEvidence{
				EventType: "payout", ProviderReference: ref, MerchantReference: merchantRef,
				Outcome: OutcomeSucceeded, Amount: amount, AssetCode: "EUR",
			})
			return err
		})
		if err == nil {
			pending.Flush(context.Background())
		}
		return err
	}
	inserted, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			disp, err := ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.pid, ReceiptEvidence{
				EventType: "payout", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 499, AssetCode: "EUR",
			})
			if err == nil && disp != DispositionDeferredUnresolved {
				err = fmt.Errorf("tx A disposition = %s, want deferred_unresolved", disp)
			}
			close(inserted)
			<-release
			return err
		})
	}()
	<-inserted
	if err := deliver(a.MerchantReference, 500); err != nil {
		t.Fatalf("matching delivery: %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("tx A: %v", err)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("want succeeded, got %s", got.State)
	}
	if e.ca2Unresolved(t, ref) != 1 || e.b12AuditCount(t, r5Terminal, a.ID) != 0 {
		t.Fatalf("setup: the orphan must be unresolved and unaudited")
	}
	for i := 0; i < 3; i++ {
		if err := deliver("", 499); err != nil {
			t.Fatalf("redelivery #%d: %v", i, err)
		}
		if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 1 {
			t.Fatalf("after redelivery #%d: terminal-mismatch audit rows = %d, want exactly 1", i, n)
		}
		if n := e.ca2Unresolved(t, ref); n != 0 {
			t.Fatalf("after redelivery #%d: the orphaned receipt must be resolved, %d unresolved", i, n)
		}
	}
	ca2AssertReceiptResolved(t, pool, f.tenantID, ref, 499, a.ID)
	rows := e.b12Rows(t)
	if len(rows) != 1 || rows[0].State != "open" {
		t.Fatalf("want one open alert row, got %+v", rows)
	}
}

// LF sequence for a payout: a mismatched success is DEFERRED (nothing resolves it), the payout is then
// settled by the SYNC path (which binds the reference and never drains), and the provider redelivers.
func TestCallbackAudit2_DeferredThenSyncBind_Redelivered_AuditedOnce_Resolved(t *testing.T) {
	pool := depositV2ScratchPool(t)
	pid := "mock-ca2-syncbind"
	f, orch, _ := fpOrch(t, pool, pid)
	e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
	wr, a := e.claim(t, "ca2-syncbind")
	const ref = "ca2-syncbind-ref"
	deliver := func(amount int64) error { return e.r5Callback(PaymentAttempt{}, ref, amount, "EUR") }
	if err := deliver(499); err != nil {
		t.Fatalf("deferred delivery: %v", err)
	}
	if e.ca2Unresolved(t, ref) != 1 {
		t.Fatalf("setup: the mismatched success must be stored deferred and unresolved")
	}
	e.apply(t, wr, a, rbResult(ErrorClassSucceeded, OutcomeSucceeded, ref))
	if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("want succeeded via the sync path, got %s", got.State)
	}
	if e.ca2Unresolved(t, ref) != 1 || e.b12AuditCount(t, r5Terminal, a.ID) != 0 {
		t.Fatalf("setup: the sync path never drains, so the receipt stays unresolved and unaudited")
	}
	for i := 0; i < 3; i++ {
		if err := deliver(499); err != nil {
			t.Fatalf("redelivery #%d: %v", i, err)
		}
	}
	if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 1 {
		t.Fatalf("terminal-mismatch audit rows = %d, want exactly 1", n)
	}
	if n := e.ca2Unresolved(t, ref); n != 0 {
		t.Fatalf("the orphaned receipt must be resolved, %d unresolved", n)
	}
	ca2AssertReceiptResolved(t, pool, f.tenantID, ref, 499, a.ID)
	e.b12AssertOneAlertRowCount(t, a.ID, alertReasonPayoutMismatchedSuccessOnSucceeded)
}

func (e rbEnv) b12AssertOneAlertRowCount(t *testing.T, attemptID uuid.UUID, reason string) {
	t.Helper()
	rows := e.b12Rows(t)
	want := "payout_attempt:" + attemptID.String() + ":reason:" + reason
	if len(rows) != 1 || rows[0].Discriminator != want || rows[0].State != "open" {
		t.Fatalf("want ONE open alert row %s, got %+v", want, rows)
	}
}

// Deposit form of the orphan: the terminal-mismatch audit row is the deposit's ONLY trace of the
// mismatched success, so it must not be lost. An unresolved stored receipt (the orphan, planted through
// the production insert) is redelivered after the deposit succeeded: one audit row, receipt resolved,
// no alert, no state change; further redeliveries add nothing.
func TestCallbackAudit2_Deposit_OrphanedReceipt_Redelivered_AuditedOnce_Resolved(t *testing.T) {
	pool := depositV2ScratchPool(t)
	pid := "mock-psp-ca2-dep"
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider(pid, "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{pid: provider}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "ca2-dep",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	ref := "ca2-dep-ref"
	if res.Attempt.ProviderReference != nil {
		ref = *res.Attempt.ProviderReference
	}
	mismatched := ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 4999, AssetCode: "EUR"}
	deliver := func(ev ReceiptEvidence) {
		pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
			_, err := ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, pid, ev)
			return err
		})
		if err != nil {
			t.Fatalf("ApplyReceiptEvidence: %v", err)
		}
		pending.Flush(context.Background())
	}
	deliver(ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: res.Attempt.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"})
	if got := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID); got.State != AttemptSucceeded {
		t.Fatalf("setup: want succeeded, got %s", got.State)
	}
	// Plant the orphan AFTER the success (so no drain can take it): a deferred, unresolved receipt for the mismatched event.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, dup, err := insertReceiptDeduped(ctx, tx, f.tenantID, pid, mismatched, DispositionDeferredUnresolved)
		if err == nil && dup {
			err = fmt.Errorf("setup: receipt unexpectedly a duplicate")
		}
		return err
	}); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}
	count := func(q string) int {
		return fpCount(t, pool, f.tenantID, q, f.tenantID, ref)
	}
	unresolved := func() int {
		return count(`SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1 AND provider_reference=$2 AND resolved_at IS NULL AND amount=4999`)
	}
	if unresolved() != 1 {
		t.Fatalf("setup: the planted orphan must be unresolved")
	}
	for i := 0; i < 3; i++ {
		deliver(mismatched)
		n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2 AND target_id=$3`,
			f.tenantID, r5Terminal, res.Attempt.ID.String())
		if n != 1 {
			t.Fatalf("after redelivery #%d: audit rows = %d, want exactly 1", i, n)
		}
	}
	if unresolved() != 0 {
		t.Fatalf("the orphaned deposit receipt must be resolved")
	}
	ca2AssertReceiptResolved(t, pool, f.tenantID, ref, 4999, res.Attempt.ID)
	if rows := alertinject.ForSubject(t, pool, f.tenantID); len(rows) != 0 {
		t.Fatalf("a deposit raises no alert today, got %+v", rows)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID); got.State != AttemptSucceeded {
		t.Fatalf("state changed to %s", got.State)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}
