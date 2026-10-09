//go:build integration

// R17 (ADR 0095 section 47): H(8) decision 19 at the receipt/callback cascade-creation site
// (receipt.go applyResolvedReceiptEvidence). Runtime role through the shared test pool; the
// fail-closed tests use the throwaway-scratch-database technique of
// h8_brand_failclosed_scratch_integration_test.go (table owner revokes, no shared role touched).
package payments

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

func r17Orch(idA, idB string) (*Orchestrator, *loopProvider, *loopProvider) {
	a, b := newLoopProvider(idA), newLoopProvider(idB)
	b.AcceptAllAmounts = true
	return NewOrchestrator(
		map[string]PaymentProvider{a.providerID: a, b.providerID: b},
		MultiWebhookCredentialResolver{a.providerID: NewMockWebhookCredentials(a.MockProvider), b.providerID: NewMockWebhookCredentials(b.MockProvider)}), a, b
}

// r17Deliver applies one cascadable decline callback for ref as provider providerID.
func r17Deliver(pool *db.Pool, orch *Orchestrator, f orchFixture, providerID, ref string) (ReceiptDisposition, error) {
	var d ReceiptDisposition
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, providerID, ReceiptEvidence{
			EventType: "deposit", ProviderReference: ref, Outcome: OutcomeDeclined, Amount: 5000, AssetCode: "EUR",
			DeclineReason: "provider_unavailable", Cascadable: true, DeclineStage: DeclineAfterAcceptance,
		})
		return err
	})
	return d, err
}

func r17Receipts(t *testing.T, pool *db.Pool, tenantID uuid.UUID, ref string) int {
	return depScan[int](t, pool, tenantID, `SELECT count(*) FROM payment_provider_events WHERE provider_reference = $1`, ref)
}

type r17Case struct {
	name          string
	tenant, brand string // status set AFTER the pending deposit exists ("" = leave active)
	wantChild     bool
	wantAudit     string // action that must be written exactly once ("" = neither)
}

func TestR17_ReceiptSite_CascadeGate_Matrix(t *testing.T) {
	pool := depositV2ScratchPool(t)
	orch, provA, provB := r17Orch("mock-psp-r17-a", "mock-psp-r17-b")
	const brandAct, tenAct = "payment.cascade_skipped_brand_inactive", "payment.cascade_skipped_resolution_only"
	cases := []r17Case{
		{"control active/active", "", "", true, ""},
		{"brand suspended", "", "suspended", false, brandAct},
		{"brand closed", "", "closed", false, brandAct},
		{"tenant suspended", "suspended", "", false, tenAct},
		{"tenant closed", "closed", "", false, tenAct},
		{"both inactive: tenant reason only, never conflated", "suspended", "suspended", false, tenAct},
	}
	type run struct {
		f   orchFixture
		att PaymentAttempt
		ref string
	}
	runs := make([]run, len(cases))
	for i := range cases {
		f := seedOrchFixture(t, pool)
		registerCapability(t, pool, f, provA, 100)
		registerCapability(t, pool, f, provB, 200)
		a, ref := pendingDeposit(t, pool, orch, f, "r17-m-"+uuid.NewString()[:8], 5000)
		runs[i] = run{f, a, ref}
	}
	for i, c := range cases {
		r := runs[i]
		if c.tenant != "" {
			setTenantStatus(t, pool, r.f.tenantID, c.tenant)
		}
		if c.brand != "" {
			setBrandStatus(t, pool, r.f.tenantID, r.f.brandID, c.brand)
		}
	}
	for i, c := range cases {
		c, r := c, runs[i]
		t.Run(c.name, func(t *testing.T) {
			intentID := *r.att.DepositIntentID
			ledgerBefore := ledgerTxCount(t, pool, r.f.tenantID)
			if d, err := r17Deliver(pool, orch, r.f, "mock-psp-r17-a", r.ref); err != nil || d != DispositionApplied {
				t.Fatalf("delivery: %s %v", d, err)
			}
			wantAttempts := 1
			if c.wantChild {
				wantAttempts = 2
			}
			if n := attemptsForIntent(t, pool, r.f.tenantID, intentID); n != wantAttempts {
				t.Fatalf("attempts: want %d got %d", wantAttempts, n)
			}
			if got := mustGetAttempt(t, pool, r.f.tenantID, r.att.ID); got.State != AttemptDeclined {
				t.Fatalf("the decline must stand, got %s", got.State)
			}
			if !c.wantChild {
				if st := depScan[string](t, pool, r.f.tenantID, `SELECT status FROM deposit_intents WHERE id = $1`, intentID); st != "declined" {
					t.Fatalf("intent must end declined, got %s", st)
				}
			}
			check := func(stage string) {
				au := auditActions(t, pool, r.f.tenantID)
				for _, a := range []string{brandAct, tenAct} {
					want := 0
					if a == c.wantAudit {
						want = 1
					}
					if au[a] != want {
						t.Fatalf("%s: audit %s want %d got %d (%v)", stage, a, want, au[a], au)
					}
				}
			}
			check("first delivery")
			if ledgerTxCount(t, pool, r.f.tenantID) != ledgerBefore {
				t.Fatal("no ledger transaction may result from a decline/skip")
			}
			assertLedgerBalanced(t, pool, r.f.tenantID)
			// Redelivery of the same event: idempotent, no second audit row, no child, one receipt.
			if _, err := r17Deliver(pool, orch, r.f, "mock-psp-r17-a", r.ref); err != nil {
				t.Fatalf("redelivery: %v", err)
			}
			if n := attemptsForIntent(t, pool, r.f.tenantID, intentID); n != wantAttempts {
				t.Fatalf("redelivery changed attempts: %d", n)
			}
			check("redelivery")
			if n := r17Receipts(t, pool, r.f.tenantID, r.ref); n != 1 {
				t.Fatalf("one receipt row expected, got %d", n)
			}
		})
	}
}

// Same tenant, a SECOND brand: suspending one brand never affects the other brand's child.
func TestR17_ReceiptSite_OtherBrandSameTenant_Unaffected(t *testing.T) {
	pool := depositV2ScratchPool(t)
	orch, provA, provB := r17Orch("mock-psp-r17o-a", "mock-psp-r17o-b")
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, provA, 100)
	registerCapability(t, pool, f, provB, 200)
	f2 := f
	f2.brandID, f2.playerAccountID = uuid.New(), uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'R17 Brand 2')`, f2.brandID, f.tenantID, "r17-"+f2.brandID.String()[:8]); err != nil {
			return err
		}
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, f2.playerAccountID, f.tenantID, f2.brandID, personID, f2.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f2.brandID, f2.playerAccountID, "EUR")
		f2.walletID = w.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	a1, ref1 := pendingDeposit(t, pool, orch, f, "r17-o1", 5000)
	a2, ref2 := pendingDeposit(t, pool, orch, f2, "r17-o2", 5000)
	setBrandStatus(t, pool, f.tenantID, f.brandID, "suspended")
	for _, ref := range []string{ref1, ref2} {
		if _, err := r17Deliver(pool, orch, f, "mock-psp-r17o-a", ref); err != nil {
			t.Fatal(err)
		}
	}
	if n := attemptsForIntent(t, pool, f.tenantID, *a1.DepositIntentID); n != 1 {
		t.Fatalf("suspended brand: no child, got %d attempts", n)
	}
	if n := attemptsForIntent(t, pool, f.tenantID, *a2.DepositIntentID); n != 2 {
		t.Fatalf("other brand of the same tenant must get its child, got %d attempts", n)
	}
	if n := auditActions(t, pool, f.tenantID)["payment.cascade_skipped_brand_inactive"]; n != 1 {
		t.Fatalf("one brand-skip audit row expected, got %d", n)
	}
}

// Stub-intent trap: the gate must load the REAL brand_id. With an active brand, a gate that handed
// the stub (BrandID == uuid.Nil) to skipCascadeChildForBrand would refuse; and the loaded intent
// must be the one passed (direct contract on the helper).
func TestR17_ReceiptSite_GateLoadsRealBrand_NotStub(t *testing.T) {
	pool := depositV2ScratchPool(t)
	orch, provA, provB := r17Orch("mock-psp-r17s-a", "mock-psp-r17s-b")
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, provA, 100)
	registerCapability(t, pool, f, provB, 200)
	a, _ := pendingDeposit(t, pool, orch, f, "r17-s1", 5000)
	var skipped bool
	var audited int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		skipped, err = gateReceiptCascadeChild(ctx, tx, a)
		return err
	}); err != nil || skipped {
		t.Fatalf("active tenant+brand: gate must not skip (a nil-brand stub would): skipped=%v err=%v", skipped, err)
	}
	setBrandStatus(t, pool, f.tenantID, f.brandID, "suspended")
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		skipped, err = gateReceiptCascadeChild(ctx, tx, a)
		return err
	}); err != nil || !skipped {
		t.Fatalf("suspended brand: gate must skip: skipped=%v err=%v", skipped, err)
	}
	audited = auditActions(t, pool, f.tenantID)["payment.cascade_skipped_brand_inactive"]
	if audited != 1 {
		t.Fatalf("audit row carrying the real brand id expected once, got %d", audited)
	}
	// The audit metadata names the intent's real brand, never the nil uuid.
	if got := depScan[string](t, pool, f.tenantID, `SELECT metadata->>'brand_id' FROM audit_log WHERE action = 'payment.cascade_skipped_brand_inactive' AND tenant_id = $1`, f.tenantID); got != f.brandID.String() {
		t.Fatalf("audit brand_id = %s, want the intent's brand %s", got, f.brandID)
	}
	// Foreign tenant's attempt/intent: a mismatching tenant fails closed (error, never skip=false).
	other := seedOrchFixture(t, pool)
	bogus := a
	bogus.TenantID = other.tenantID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := gateReceiptCascadeChild(ctx, tx, bogus)
		return err
	}); err == nil || !strings.Contains(err.Error(), "intent tenant differs") {
		t.Fatalf("an attempt whose tenant differs from the intent's must fail closed with the tenant-mismatch error, got %v", err)
	}
}

// Fail closed with rollback and redelivery convergence (scratch DB; owner revokes UPDATE on brands
// so the FOR SHARE brand read fails with 42501). The WHOLE delivery rolls back, receipt dedupe row
// included; after the grant is restored the redelivery converges to the control outcome.
func TestR17_Scratch_ReceiptSite_BrandReadError_RollsBackDelivery_RedeliveryConverges(t *testing.T) {
	owner := depositV2ScratchPool(t)
	rt := h8RuntimeOnScratch(t, owner)
	orch, provA, provB := r17Orch("mock-psp-r17f-a", "mock-psp-r17f-b")
	f := seedOrchFixture(t, owner)
	registerCapability(t, owner, f, provA, 100)
	registerCapability(t, owner, f, provB, 200)
	a, ref := pendingDeposit(t, owner, orch, f, "r17-f1", 5000)
	intentID := *a.DepositIntentID
	before := h8Snap(t, owner, f, intentID)

	h8RevokeBrandUpdate(t, owner)
	// The gate itself returns the underlying read error (42501), it does not merely rely on the
	// aborted transaction failing a later statement.
	gerr := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := gateReceiptCascadeChild(ctx, tx, a)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(gerr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("gate must surface the brand read error (42501), got %v", gerr)
	}
	if _, err := r17Deliver(rt, orch, f, "mock-psp-r17f-a", ref); err == nil {
		t.Fatal("a brand read error at the receipt site must fail the delivery")
	}
	if got := mustGetAttempt(t, owner, f.tenantID, a.ID); got.State != AttemptPending {
		t.Fatalf("decline must be rolled back, attempt state %s", got.State)
	}
	if n := r17Receipts(t, owner, f.tenantID, ref); n != 0 {
		t.Fatalf("the receipt dedupe row must roll back with the delivery, got %d", n)
	}
	if after := h8Snap(t, owner, f, intentID); after != before {
		t.Fatalf("rows changed by a rolled-back delivery: before=%+v after=%+v", before, after)
	}
	au := auditActions(t, owner, f.tenantID)
	if au["payment.cascade_skipped_brand_inactive"] != 0 || au["payment.cascade_skipped_resolution_only"] != 0 {
		t.Fatalf("no skip audit may survive a rolled-back delivery: %v", au)
	}

	if _, err := owner.Raw().Exec(context.Background(), `GRANT UPDATE ON public.brands TO igaming_runtime`); err != nil {
		t.Fatalf("restore grant on scratch: %v", err)
	}
	if d, err := r17Deliver(rt, orch, f, "mock-psp-r17f-a", ref); err != nil || d != DispositionApplied {
		t.Fatalf("redelivery: %s %v", d, err)
	}
	if n := attemptsForIntent(t, owner, f.tenantID, intentID); n != 2 {
		t.Fatalf("redelivery must converge to the control outcome (child created), got %d attempts", n)
	}
	if n := r17Receipts(t, owner, f.tenantID, ref); n != 1 {
		t.Fatalf("one receipt row after convergence, got %d", n)
	}
}

// Tenant read error at the receipt-site gate (scratch DB, owner revokes SELECT on tenants from the
// runtime role): the gate returns the error (fail closed), never "active", never skip=false.
func TestR17_Scratch_ReceiptSite_TenantReadError_FailsClosed(t *testing.T) {
	owner := depositV2ScratchPool(t)
	rt := h8RuntimeOnScratch(t, owner)
	orch, provA, provB := r17Orch("mock-psp-r17t-a", "mock-psp-r17t-b")
	f := seedOrchFixture(t, owner)
	registerCapability(t, owner, f, provA, 100)
	registerCapability(t, owner, f, provB, 200)
	a, _ := pendingDeposit(t, owner, orch, f, "r17-t1", 5000)
	if _, err := owner.Raw().Exec(context.Background(), `REVOKE SELECT ON public.tenants FROM igaming_runtime`); err != nil {
		t.Fatalf("revoke on scratch: %v", err)
	}
	gerr := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		skipped, err := gateReceiptCascadeChild(ctx, tx, a)
		if skipped {
			t.Error("a read error must not be reported as a skip")
		}
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(gerr, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("gate must surface the tenant read error (42501), got %v", gerr)
	}
}

// R20 (ADR 0095 47.2 review addendum): the same gate reached through the deferred-receipt DRAIN
// (ApplyDeferredReceiptsForAttempt, as phase C / poll / sweeper host it), not a callback delivery.
func r20Drain(t *testing.T, pool *db.Pool, orch *Orchestrator, f orchFixture, intentID, attemptID uuid.UUID) int {
	t.Helper()
	var applied int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT id FROM deposit_intents WHERE id = $1 FOR UPDATE`, intentID); err != nil {
			return err
		}
		a, err := GetAttemptByID(ctx, tx, attemptID)
		if err != nil {
			return err
		}
		applied, err = ApplyDeferredReceiptsForAttempt(ctx, tx, orch, a)
		return err
	}); err != nil {
		t.Fatalf("drain: %v", err)
	}
	return applied
}

func TestR20_DeferredDrain_CascadeGate(t *testing.T) {
	pool := depositV2ScratchPool(t)
	orch, provA, provB := r17Orch("mock-psp-r20-a", "mock-psp-r20-b")
	const brandAct, tenAct = "payment.cascade_skipped_brand_inactive", "payment.cascade_skipped_resolution_only"
	cases := []r17Case{
		{"control active/active still creates the child", "", "", true, ""},
		{"brand suspended", "", "suspended", false, brandAct},
		{"tenant suspended", "suspended", "", false, tenAct},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			f := seedOrchFixture(t, pool)
			registerCapability(t, pool, f, provA, 100)
			registerCapability(t, pool, f, provB, 200)
			a, ref := pendingDeposit(t, pool, orch, f, "r20-"+uuid.NewString()[:8], 5000)
			intentID := *a.DepositIntentID
			// The decline arrives as a deferred (unresolved) receipt, then the status flips, then the drain runs.
			ev := ReceiptEvidence{EventType: "deposit", ProviderReference: ref, Outcome: OutcomeDeclined, Amount: 5000, AssetCode: "EUR",
				DeclineReason: "provider_unavailable", Cascadable: true, DeclineStage: DeclineAfterAcceptance}
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, dup, err := insertReceiptDeduped(ctx, tx, f.tenantID, *a.ProviderID, ev, DispositionDeferredUnresolved)
				if err == nil && dup {
					t.Fatal("setup: planted receipt was a duplicate")
				}
				return err
			}); err != nil {
				t.Fatalf("plant deferred decline: %v", err)
			}
			if c.tenant != "" {
				setTenantStatus(t, pool, f.tenantID, c.tenant)
			}
			if c.brand != "" {
				setBrandStatus(t, pool, f.tenantID, f.brandID, c.brand)
			}
			ledgerBefore := ledgerTxCount(t, pool, f.tenantID)
			if n := r20Drain(t, pool, orch, f, intentID, a.ID); n != 1 {
				t.Fatalf("drain applied %d receipts, want 1", n)
			}
			wantAttempts := 1
			if c.wantChild {
				wantAttempts = 2
			}
			check := func(stage string) {
				if n := attemptsForIntent(t, pool, f.tenantID, intentID); n != wantAttempts {
					t.Fatalf("%s: attempts want %d got %d", stage, wantAttempts, n)
				}
				if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptDeclined {
					t.Fatalf("%s: the decline must stand, got %s", stage, got.State)
				}
				if !c.wantChild {
					if st := depScan[string](t, pool, f.tenantID, `SELECT status FROM deposit_intents WHERE id = $1`, intentID); st != "declined" {
						t.Fatalf("%s: intent must end declined, got %s", stage, st)
					}
				}
				if un := depScan[int](t, pool, f.tenantID, `SELECT count(*) FROM payment_provider_events WHERE provider_reference = $1 AND resolved_at IS NULL`, ref); un != 0 {
					t.Fatalf("%s: receipt must be resolved, %d unresolved", stage, un)
				}
				au := auditActions(t, pool, f.tenantID)
				for _, act := range []string{brandAct, tenAct} {
					want := 0
					if act == c.wantAudit {
						want = 1
					}
					if au[act] != want {
						t.Fatalf("%s: audit %s want %d got %d (%v)", stage, act, want, au[act], au)
					}
				}
				if ledgerTxCount(t, pool, f.tenantID) != ledgerBefore {
					t.Fatalf("%s: no ledger transaction may result from a decline/skip", stage)
				}
				assertLedgerBalanced(t, pool, f.tenantID)
			}
			check("first drain")
			// Idempotency: a re-drain finds nothing unresolved; a redelivery of the event is a no-op.
			if n := r20Drain(t, pool, orch, f, intentID, a.ID); n != 0 {
				t.Fatalf("re-drain applied %d, want 0", n)
			}
			check("re-drain")
			if _, err := r17Deliver(pool, orch, f, *a.ProviderID, ref); err != nil {
				t.Fatalf("redelivery: %v", err)
			}
			check("redelivery")
			if n := r17Receipts(t, pool, f.tenantID, ref); n != 1 {
				t.Fatalf("one receipt row expected, got %d", n)
			}
		})
	}
}

// R20 security L-2: a broken intent invariant at the receipt-site gate emits an operator-visible
// structured error (closed reason token, ids only) and STILL fails closed: the host tx rolls back.
func TestR20_ReceiptGate_BrokenInvariant_LogsAndRollsBack(t *testing.T) {
	pool := depositV2ScratchPool(t)
	orch, provA, provB := r17Orch("mock-psp-r20l-a", "mock-psp-r20l-b")
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, provA, 100)
	registerCapability(t, pool, f, provB, 200)
	a, ref := pendingDeposit(t, pool, orch, f, "r20-l1", 5000)
	other := seedOrchFixture(t, pool)

	missing := a
	missingID := uuid.New()
	missing.DepositIntentID = &missingID
	cross := a
	cross.TenantID = other.tenantID
	noIntent := a
	noIntent.DepositIntentID = nil

	cases := []struct {
		name, reason string
		att          PaymentAttempt
		intent       *uuid.UUID
	}{
		{"missing intent", "intent_missing", missing, &missingID},
		{"cross-tenant intent", "intent_tenant_mismatch", cross, a.DepositIntentID},
		{"attempt without intent", "attempt_without_intent", noIntent, nil},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
			t.Cleanup(func() { slog.SetDefault(prev) })
			evRef := ref + "-" + c.reason
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				// Written first in the same tx: must vanish with the rollback.
				if _, _, err := insertReceiptDeduped(ctx, tx, f.tenantID, "mock-psp-r20l-a",
					ReceiptEvidence{EventType: "deposit", ProviderReference: evRef, Outcome: OutcomeDeclined, Amount: 5000, AssetCode: "EUR"}, DispositionDeferredUnresolved); err != nil {
					return err
				}
				skipped, err := gateReceiptCascadeChild(ctx, tx, c.att)
				if skipped {
					t.Error("a broken invariant must never report a skip")
				}
				return err
			})
			if err == nil {
				t.Fatal("gate must return an error (fail closed)")
			}
			if n := r17Receipts(t, pool, f.tenantID, evRef); n != 0 {
				t.Fatalf("host transaction must roll back, %d receipt rows survived", n)
			}
			out := buf.String()
			for _, want := range []string{"level=ERROR", "payments_receipt_cascade_gate_invariant_broken", "reason=" + c.reason,
				"attempt_id=" + c.att.ID.String(), "tenant_id=" + c.att.TenantID.String()} {
				if !strings.Contains(out, want) {
					t.Fatalf("log missing %q:\n%s", want, out)
				}
			}
			if c.intent != nil && !strings.Contains(out, "deposit_intent_id="+c.intent.String()) {
				t.Fatalf("log missing intent id:\n%s", out)
			}
			if strings.Contains(out, err.Error()) || strings.Count(out, "\n") != 1 {
				t.Fatalf("log must be one line without error text:\n%s", out)
			}
		})
	}
	// Control: a healthy attempt logs nothing.
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := gateReceiptCascadeChild(ctx, tx, a)
		return err
	}); err != nil || buf.Len() != 0 {
		t.Fatalf("healthy gate must not log: err=%v log=%q", err, buf.String())
	}
}
