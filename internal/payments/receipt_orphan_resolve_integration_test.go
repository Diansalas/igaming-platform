//go:build integration

// PAY-RECEIPT-ORPHAN-RESOLVE-1 (ADR 0095 section 42.8; security R-a, ledger-finance C-1a/C-1b).
//
// C-1a: an ORPHANED deferred receipt (stored unresolved, never drained) that later lands in one of the
// three anomaly branches of ApplyReceiptEvidence on redelivery must be closed, so it stops counting toward
// DeferredReceiptCap. An already-resolved duplicate is left untouched. No state or money changes.
// C-1b: payment.decline_reason_bounded is written exactly once per receipt, when it is first stored.
package payments

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type orRow struct {
	n          int
	resolved   bool
	resolution string
	attemptID  *uuid.UUID
	resolvedAt *time.Time
}

func (e t4DrainEnv) orReceipt(t *testing.T, provider, ref string) orRow {
	t.Helper()
	var r orRow
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1 AND provider_id=$2 AND provider_reference=$3`,
			e.f.tenantID, provider, ref).Scan(&r.n); err != nil || r.n == 0 {
			return err
		}
		var res *string
		if err := tx.QueryRow(ctx, `SELECT resolved_at IS NOT NULL, resolution, attempt_id, resolved_at FROM payment_provider_events WHERE tenant_id=$1 AND provider_id=$2 AND provider_reference=$3`,
			e.f.tenantID, provider, ref).Scan(&r.resolved, &res, &r.attemptID, &r.resolvedAt); err != nil {
			return err
		}
		if res != nil {
			r.resolution = *res
		}
		return nil
	}); err != nil {
		t.Fatalf("read receipt: %v", err)
	}
	return r
}

func (e t4DrainEnv) orPlant(t *testing.T, provider string, ev ReceiptEvidence) {
	t.Helper()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, dup, err := insertReceiptDeduped(ctx, tx, e.f.tenantID, provider, ev, DispositionDeferredUnresolved)
		if err == nil && dup {
			t.Fatalf("setup: planted orphan was a duplicate")
		}
		return err
	}); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}
}

func (e t4DrainEnv) orUnapplied(t *testing.T, provider string) int {
	t.Helper()
	var n int
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		n, err = CountUnappliedReceipts(ctx, tx, e.f.tenantID, provider, DeferredReceiptCap)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e t4DrainEnv) orDeliver(provider string, ev ReceiptEvidence) (ReceiptDisposition, error) {
	return rvApplyReceipt(e.pool, e.orch, e.f.tenantID, provider, ev)
}

// orSecondAmbiguous creates a second ref-less ambiguous deposit attempt (a different intent).
func (e t4DrainEnv) orSecondAmbiguous(t *testing.T, key string) PaymentAttempt {
	t.Helper()
	return rvInit(t, e.pool, e.orch, e.f, MockAmountAmbiguous, key).Attempt
}

func (e t4DrainEnv) orBind(t *testing.T, id uuid.UUID, ref string) {
	t.Helper()
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return MarkAccepted(ctx, tx, id, EvidenceCallback, ref, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("bind %s: %v", ref, err)
	}
}

type orMoney struct {
	state    AttemptState
	ledgerTx int64
	entries  int64
	events   int64
}

func (e t4DrainEnv) orMoney(t *testing.T) orMoney {
	t.Helper()
	return orMoney{
		state:    mustGetAttempt(t, e.pool, e.f.tenantID, e.attempt.ID).State,
		ledgerTx: e.count(t, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1`, e.f.tenantID),
		entries:  e.count(t, `SELECT count(*) FROM ledger_entries WHERE tenant_id=$1`, e.f.tenantID),
		events:   e.count(t, `SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1`, e.f.tenantID),
	}
}

type orBranch struct {
	name     string
	provider func(e t4DrainEnv) string
	ev       func(t *testing.T, e t4DrainEnv) ReceiptEvidence
	want     ReceiptResolution
	hook     func(t *testing.T, e t4DrainEnv, ev ReceiptEvidence) func()
}

func orBranches() []orBranch {
	own := func(e t4DrainEnv) string { return e.provider }
	return []orBranch{
		{name: "cross_provider", provider: func(e t4DrainEnv) string { return e.provider + "-other" },
			ev: func(t *testing.T, e t4DrainEnv) ReceiptEvidence {
				return ReceiptEvidence{EventType: "deposit", ProviderReference: "or-xp-ref", MerchantReference: e.attempt.MerchantReference, Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR"}
			}, want: ResolutionAnomalyCrossProvider},
		{name: "reference_conflict", provider: own,
			ev: func(t *testing.T, e t4DrainEnv) ReceiptEvidence {
				c := e.orSecondAmbiguous(t, "or-conflict-c")
				e.orBind(t, c.ID, "or-conflict-refC")
				return ReceiptEvidence{EventType: "deposit", ProviderReference: "or-conflict-refC", MerchantReference: e.attempt.MerchantReference, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR"}
			}, want: ResolutionAnomalyReferenceConflict},
		{name: "event_type_mismatch", provider: own,
			ev: func(t *testing.T, e t4DrainEnv) ReceiptEvidence {
				return ReceiptEvidence{EventType: "payout", ProviderReference: "or-et-ref", MerchantReference: e.attempt.MerchantReference, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR"}
			}, want: ResolutionAnomalyOther},
		{name: "late_reference_conflict_recheck", provider: own,
			ev: func(t *testing.T, e t4DrainEnv) ReceiptEvidence {
				return ReceiptEvidence{EventType: "deposit", ProviderReference: "or-late-refX", MerchantReference: e.attempt.MerchantReference, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR"}
			}, want: ResolutionAnomalyReferenceConflict,
			hook: func(t *testing.T, e t4DrainEnv, ev ReceiptEvidence) func() {
				c := e.orSecondAmbiguous(t, "or-late-c")
				fired := false
				testHookBeforeReferenceConflictRecheck = func() {
					if !fired {
						fired = true
						e.orBind(t, c.ID, ev.ProviderReference)
					}
				}
				t.Cleanup(func() { testHookBeforeReferenceConflictRecheck = nil })
				return func() {
					if !fired {
						t.Fatal("setup: recheck hook never fired")
					}
				}
			}},
	}
}

func orRun(t *testing.T, name string) (t4DrainEnv, orBranch, ReceiptEvidence, string, func()) {
	for i, b := range orBranches() {
		if b.name != name {
			continue
		}
		e := t4DrainSetup(t, "mock-orph-"+strings.ReplaceAll(name, "_", "-"), "orph-"+name)
		_ = i
		ev := b.ev(t, e)
		prov := b.provider(e)
		e.orPlant(t, prov, ev)
		var verify func()
		if b.hook != nil {
			verify = b.hook(t, e, ev)
		}
		return e, b, ev, prov, verify
	}
	t.Fatalf("no branch %s", name)
	return t4DrainEnv{}, orBranch{}, ReceiptEvidence{}, "", nil
}

func orBranchNames() []string {
	var n []string
	for _, b := range orBranches() {
		n = append(n, b.name)
	}
	return n
}

// Each anomaly branch: the orphan is unresolved and counts; the redelivery returns the unchanged
// anomaly disposition, closes it exactly once (attempt_id NULL, the branch's resolution), it stops
// counting, nothing else changes, and further redeliveries leave the receipt untouched.
func TestOrphanResolve_AnomalyBranches_OrphanClosedOnRedelivery_NoMoneyNoState(t *testing.T) {
	for _, name := range orBranchNames() {
		t.Run(name, func(t *testing.T) {
			e, b, ev, prov, verify := orRun(t, name)
			if got := e.orUnapplied(t, prov); got != 1 {
				t.Fatalf("setup: orphan must count toward the cap, got %d", got)
			}
			before := e.orMoney(t)
			disp, err := e.orDeliver(prov, ev)
			if err != nil || disp != DispositionAnomaly {
				t.Fatalf("redelivery: disp=%s err=%v, want anomaly", disp, err)
			}
			if verify != nil {
				verify()
				testHookBeforeReferenceConflictRecheck = nil
			}
			row := e.orReceipt(t, prov, ev.ProviderReference)
			if row.n != 1 || !row.resolved || row.resolution != string(b.want) || row.attemptID != nil {
				t.Fatalf("receipt = %+v, want 1 row resolved as %s with NULL attempt", row, b.want)
			}
			if got := e.orUnapplied(t, prov); got != 0 {
				t.Fatalf("the closed orphan must stop counting toward the cap, got %d", got)
			}
			after := e.orMoney(t)
			if after.state != before.state || after.ledgerTx != before.ledgerTx || after.entries != before.entries || after.events != before.events {
				t.Fatalf("state/money/receipt count changed: %+v -> %+v", before, after)
			}
			assertLedgerBalanced(t, e.pool, e.f.tenantID)
			for i := 0; i < 2; i++ {
				if disp, err = e.orDeliver(prov, ev); err != nil || disp != DispositionAnomaly {
					t.Fatalf("repeat redelivery: disp=%s err=%v", disp, err)
				}
			}
			row2 := e.orReceipt(t, prov, ev.ProviderReference)
			if row2.resolvedAt == nil || !row2.resolvedAt.Equal(*row.resolvedAt) || row2.resolution != row.resolution || row2.n != 1 {
				t.Fatalf("a resolved duplicate must be untouched: %+v -> %+v", row, row2)
			}
		})
	}
}

// An already-resolved duplicate (resolved by another path, with an attempt) is left exactly as it was.
func TestOrphanResolve_AlreadyResolvedDuplicate_Untouched(t *testing.T) {
	for _, name := range orBranchNames() {
		t.Run(name, func(t *testing.T) {
			e, _, ev, prov, verify := orRun(t, name)
			if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var id uuid.UUID
				if err := tx.QueryRow(ctx, `SELECT id FROM payment_provider_events WHERE tenant_id=$1 AND provider_id=$2 AND provider_reference=$3`,
					e.f.tenantID, prov, ev.ProviderReference).Scan(&id); err != nil {
					return err
				}
				a := e.attempt.ID
				return ResolveReceipt(ctx, tx, id, &a, string(ResolutionApplied))
			}); err != nil {
				t.Fatalf("pre-resolve: %v", err)
			}
			before := e.orReceipt(t, prov, ev.ProviderReference)
			if disp, err := e.orDeliver(prov, ev); err != nil || disp != DispositionAnomaly {
				t.Fatalf("redelivery: disp=%s err=%v", disp, err)
			}
			if verify != nil {
				testHookBeforeReferenceConflictRecheck = nil
			}
			after := e.orReceipt(t, prov, ev.ProviderReference)
			if after.resolution != string(ResolutionApplied) || after.attemptID == nil || *after.attemptID != e.attempt.ID ||
				after.resolvedAt == nil || !after.resolvedAt.Equal(*before.resolvedAt) {
				t.Fatalf("resolved duplicate changed: %+v -> %+v", before, after)
			}
		})
	}
}

// Two concurrent redeliveries of the orphan: both succeed (no CAS error leaks), one resolution.
func TestOrphanResolve_Concurrent_TwoRedeliveries_ResolvedOnce_NoError(t *testing.T) {
	for _, name := range []string{"cross_provider", "reference_conflict", "event_type_mismatch"} {
		t.Run(name, func(t *testing.T) {
			e, b, ev, prov, _ := orRun(t, name)
			for rep := 0; rep < 20; rep++ {
				// A fresh orphan per repetition (distinct provider reference => distinct fingerprint).
				evr := ev
				evr.ProviderReference = ev.ProviderReference + "-" + strconv.Itoa(rep)
				if b.want == ResolutionAnomalyReferenceConflict {
					evr = ev // the conflict is keyed on the one bound reference; the single orphan is raced once below
					if rep > 0 {
						continue
					}
				}
				e.orPlant(t, prov, evr)
				runTwo(t, func(int) error {
					disp, err := e.orDeliver(prov, evr)
					if err == nil && disp != DispositionAnomaly {
						t.Errorf("disp = %s, want anomaly", disp)
					}
					return err
				})
				row := e.orReceipt(t, prov, evr.ProviderReference)
				if row.n != 1 || !row.resolved || row.resolution != string(b.want) || row.attemptID != nil {
					t.Fatalf("rep %d: receipt = %+v, want one row resolved as %s", rep, row, b.want)
				}
			}
			if got := e.orUnapplied(t, prov); got != 0 {
				t.Fatalf("unapplied = %d, want 0", got)
			}
		})
	}
}

// Tenant isolation: closing one tenant's orphan never touches another tenant's unresolved receipt.
func TestOrphanResolve_TenantIsolation_OtherTenantsOrphanUntouched(t *testing.T) {
	e, _, ev, prov, _ := orRun(t, "event_type_mismatch")
	f2 := seedOrchFixture(t, e.pool)
	other := ReceiptEvidence{EventType: "deposit", ProviderReference: "or-other-tenant-ref", Outcome: OutcomeSucceeded, Amount: 777, AssetCode: "EUR"}
	if err := e.pool.WithTenant(context.Background(), f2.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, dup, err := insertReceiptDeduped(ctx, tx, f2.tenantID, prov, other, DispositionDeferredUnresolved)
		if err == nil && dup {
			t.Fatalf("setup duplicate")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if disp, err := e.orDeliver(prov, ev); err != nil || disp != DispositionAnomaly {
		t.Fatalf("disp=%s err=%v", disp, err)
	}
	n := fpCount(t, e.pool, f2.tenantID, `SELECT count(*) FROM payment_provider_events WHERE tenant_id=$1 AND resolved_at IS NULL`, f2.tenantID)
	if n != 1 {
		t.Fatalf("the other tenant's orphan must stay unresolved, got %d unresolved", n)
	}
	if got := e.orUnapplied(t, prov); got != 0 {
		t.Fatalf("this tenant's orphan must be closed, unapplied=%d", got)
	}
}

// Payout form of the event-type mismatch: an orphaned deferred "deposit" event whose merchant reference
// later turns out to name a PAYOUT attempt.
func TestOrphanResolve_Payout_EventTypeMismatch_OrphanClosed(t *testing.T) {
	pool := depositV2ScratchPool(t)
	pid := "mock-or-payout"
	f, orch, _ := fpOrch(t, pool, pid)
	e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
	_, a := e.claim(t, "or-payout")
	ev := ReceiptEvidence{EventType: "deposit", ProviderReference: "or-payout-ref", MerchantReference: a.MerchantReference, Outcome: OutcomeSucceeded, Amount: 500, AssetCode: "EUR"}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, dup, err := insertReceiptDeduped(ctx, tx, f.tenantID, pid, ev, DispositionDeferredUnresolved)
		if err == nil && dup {
			t.Fatalf("setup duplicate")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if e.ca2Unresolved(t, ev.ProviderReference) != 1 {
		t.Fatalf("setup: orphan must be unresolved")
	}
	before := mustGetAttempt(t, pool, f.tenantID, a.ID)
	rows := e.r8Events(t)
	for i := 0; i < 3; i++ {
		disp, err := rvApplyReceipt(pool, orch, f.tenantID, pid, ev)
		if err != nil || disp != DispositionAnomaly {
			t.Fatalf("redelivery #%d: disp=%s err=%v", i, disp, err)
		}
	}
	if e.ca2Unresolved(t, ev.ProviderReference) != 0 || e.r8Events(t) != rows {
		t.Fatalf("orphan must be closed without new rows")
	}
	if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != before.State {
		t.Fatalf("state changed %s -> %s", before.State, got.State)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
}

// ---- C-1b ----

func orLongReason() string { return strings.Repeat("x", 200) }

func (e t4DrainEnv) orBoundedAudits(t *testing.T) int {
	t.Helper()
	return int(e.count(t, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action='payment.decline_reason_bounded'`, e.f.tenantID))
}

func orDecline(ref, merchant string) ReceiptEvidence {
	return ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: merchant, Outcome: OutcomeDeclined, Amount: MockAmountAmbiguous, AssetCode: "EUR", DeclineReason: orLongReason()}
}

// Deferred, then drained by a T4 callback: exactly one audit row, written at the first store, targeting
// the receipt; the drain and a later redelivery of the resolved receipt add none.
func TestOrphanResolve_C1b_DeferredThenDrained_OneAuditRow(t *testing.T) {
	e := t4DrainSetup(t, "mock-orph-c1b-a", "orph-c1b-a")
	ref := "orph-c1b-a-" + uuid.NewString()
	dec := orDecline(ref, "")
	if d, err := e.orDeliver(e.provider, dec); err != nil || d != DispositionDeferredUnresolved {
		t.Fatalf("deferred: %s %v", d, err)
	}
	if n := e.orBoundedAudits(t); n != 1 {
		t.Fatalf("after the deferred store: audit rows = %d, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM audit_log a JOIN payment_provider_events r ON r.id::text = a.target_id WHERE a.tenant_id=$1 AND a.action='payment.decline_reason_bounded' AND r.provider_reference=$2`, e.f.tenantID, ref); n != 1 {
		t.Fatalf("the audit row must target the receipt, got %d", n)
	}
	pend := ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: e.attempt.MerchantReference, Outcome: OutcomePending, Amount: MockAmountAmbiguous, AssetCode: "EUR"}
	if _, err := e.orDeliver(e.provider, pend); err != nil {
		t.Fatalf("T4: %v", err)
	}
	if got := e.orReceipt(t, e.provider, ref); !got.resolved {
		t.Fatalf("the deferred decline must have been drained: %+v", got)
	}
	if n := e.orBoundedAudits(t); n != 1 {
		t.Fatalf("after the drain: audit rows = %d, want 1", n)
	}
	if d, err := e.orDeliver(e.provider, dec); err != nil || d != DispositionDuplicateEffect {
		t.Fatalf("resolved duplicate: %s %v", d, err)
	}
	if n := e.orBoundedAudits(t); n != 1 {
		t.Fatalf("a resolved duplicate must add none: audit rows = %d", n)
	}
}

// Orphan: deferred (audited at the store), the attempt binds the reference WITHOUT draining, the
// redelivery applies and closes the orphan, and writes no second audit row.
func TestOrphanResolve_C1b_OrphanedThenRedelivered_OneAuditRow(t *testing.T) {
	e := t4DrainSetup(t, "mock-orph-c1b-b", "orph-c1b-b")
	ref := "orph-c1b-b-" + uuid.NewString()
	dec := orDecline(ref, "")
	if d, err := e.orDeliver(e.provider, dec); err != nil || d != DispositionDeferredUnresolved {
		t.Fatalf("deferred: %s %v", d, err)
	}
	e.orBind(t, e.attempt.ID, ref)
	if got := e.orReceipt(t, e.provider, ref); got.resolved {
		t.Fatalf("setup: the orphan must stay unresolved")
	}
	for i := 0; i < 3; i++ {
		if _, err := e.orDeliver(e.provider, dec); err != nil {
			t.Fatalf("redelivery #%d: %v", i, err)
		}
		if n := e.orBoundedAudits(t); n != 1 {
			t.Fatalf("after redelivery #%d: audit rows = %d, want 1", i, n)
		}
	}
	if got := e.orReceipt(t, e.provider, ref); !got.resolved {
		t.Fatalf("the orphan must be closed: %+v", got)
	}
	assertLedgerBalanced(t, e.pool, e.f.tenantID)
}

// A directly applied new receipt and a new anomaly receipt each get exactly one row; redelivery none.
func TestOrphanResolve_C1b_NewApplied_And_NewAnomaly_OneAuditRowEach(t *testing.T) {
	e := t4DrainSetup(t, "mock-orph-c1b-c", "orph-c1b-c")
	ref := "orph-c1b-c-" + uuid.NewString()
	e.orBind(t, e.attempt.ID, ref)
	dec := orDecline(ref, "")
	anom := orDecline("orph-c1b-c-anom", e.attempt.MerchantReference)
	anom.EventType = "payout"
	for i, ev := range []ReceiptEvidence{dec, anom} {
		for j := 0; j < 3; j++ {
			if _, err := e.orDeliver(e.provider, ev); err != nil {
				t.Fatalf("event %d delivery %d: %v", i, j, err)
			}
			if n := e.orBoundedAudits(t); n != i+1 {
				t.Fatalf("event %d delivery %d: audit rows = %d, want %d", i, j, n, i+1)
			}
		}
	}
}

// Deliberate interleaving (security L-2 / LF L-1): a lock-free orphan close lands INSIDE the drain's window
// between its select and its strict ResolveReceipt. The orphan is a deposit receipt for reference R whose
// merchant reference names a DIFFERENT attempt, so the redelivery classifies it as a reference-conflict
// anomaly (the tolerant close) while the binding attempt's drain selects the same row. The drain must
// not see ErrAttemptStateConflict (that would roll back the whole binding transaction), and the receipt
// ends resolved exactly once.
func TestOrphanResolve_Interleaved_AnomalyCloseDuringDrain_NoConflictLeaksToBindingTx(t *testing.T) {
	e := t4DrainSetup(t, "mock-orph-interleave", "orph-interleave")
	c := e.orSecondAmbiguous(t, "orph-interleave-c")
	const ref = "orph-interleave-refR"
	e.orBind(t, e.attempt.ID, ref)
	ev := ReceiptEvidence{EventType: "deposit", ProviderReference: ref, MerchantReference: c.MerchantReference,
		Outcome: OutcomePending, Amount: MockAmountAmbiguous, AssetCode: "EUR"}
	e.orPlant(t, e.provider, ev)

	type res struct {
		disp ReceiptDisposition
		err  error
	}
	done := make(chan res, 1)
	fired := false
	testHookAfterDeferredSelect = func() {
		fired = true
		go func() {
			d, err := e.orDeliver(e.provider, ev)
			done <- res{d, err}
		}()
		// Give the competing close time to commit if nothing blocks it (the FOR UPDATE makes it wait).
		time.Sleep(700 * time.Millisecond)
	}
	t.Cleanup(func() { testHookAfterDeferredSelect = nil })

	var applied int
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, e.intent); err != nil {
			return err
		}
		a, err := GetAttemptByID(ctx, tx, e.attempt.ID)
		if err != nil {
			return err
		}
		applied, err = ApplyDeferredReceiptsForAttempt(ctx, tx, e.orch, a)
		return err
	})
	testHookAfterDeferredSelect = nil
	if err != nil {
		t.Fatalf("the binding transaction's drain failed (a conflict leaked): %v", err)
	}
	if !fired {
		t.Fatal("setup: the drain hook never fired")
	}
	r := <-done
	if r.err != nil || r.disp != DispositionAnomaly {
		t.Fatalf("competing redelivery: disp=%s err=%v, want anomaly with no error", r.disp, r.err)
	}
	row := e.orReceipt(t, e.provider, ref)
	if row.n != 1 || !row.resolved {
		t.Fatalf("receipt = %+v, want one resolved row", row)
	}
	if applied != 1 || row.attemptID == nil || *row.attemptID != e.attempt.ID {
		t.Fatalf("the drain (which held the lock first) must have applied it: applied=%d receipt=%+v", applied, row)
	}
	if got := e.orUnapplied(t, e.provider); got != 0 {
		t.Fatalf("unapplied = %d", got)
	}
	assertLedgerBalanced(t, e.pool, e.f.tenantID)
}
