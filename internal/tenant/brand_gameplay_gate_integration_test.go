//go:build integration

// ADR 0112 decision LF1, SLICE 2 (migration 0129): the per-brand gameplay gate
// primitive, the lock-key function, the exclusive lock inside the governed brand
// status guard, the deferred new-stake backstop's catalog shape, and the 0129
// up / down / up round trip (down restores 0128 byte for byte). Behaviour of the
// gate on real wagering is in internal/sportsbook and internal/casino
// brand_gameplay_gate_integration_test.go.
package tenant

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

const migration0129Version = 129

// tryBrandKeyShared reports whether a FRESH transaction on p can take the brand gate key
// SHARED right now (false = someone holds it EXCLUSIVE).
func tryBrandKeyShared(t *testing.T, p *db.Pool, brandID uuid.UUID) bool {
	t.Helper()
	var ok bool
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock_shared(public.brand_status_gate_key($1))`, brandID).Scan(&ok)
	}); err != nil {
		t.Fatalf("try brand key: %v", err)
	}
	return ok
}

func TestBrandGate_KeyFunction_DistinctFromTenantKey_AndStable(t *testing.T) {
	owner := brandPinOwnerPool(t)
	id := uuid.New()
	var k1, k2, tk int64
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT public.brand_status_gate_key($1), public.brand_status_gate_key($1), public.tenant_status_gate_key($1)`, id).Scan(&k1, &k2, &tk)
	}); err != nil {
		t.Fatal(err)
	}
	if k1 != k2 {
		t.Fatal("brand_status_gate_key is not deterministic")
	}
	if k1 == tk {
		t.Fatal("the brand key must not equal the tenant key of the same uuid (separate namespaces)")
	}
}

// The governed brand status change holds the brand key EXCLUSIVE until it commits; a
// tenant status change and a same-status brand UPDATE do not take it.
func TestBrandGate_GovernedBrandChange_HoldsKeyExclusive(t *testing.T) {
	owner := brandPinOwnerPool(t)
	f := seedBrandPinTenant(t, owner)
	ctx := context.Background()

	if !tryBrandKeyShared(t, owner, f.brandID) {
		t.Fatal("the brand key must be free before any status change")
	}
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	released := false
	releaseOnce := func() {
		if !released {
			released = true
			close(release)
		}
	}
	t.Cleanup(releaseOnce)
	go func() {
		done <- launchfix.TrySetBrandStatusHoldingOn(ctx, t, owner, f.tenantID, f.brandID, "suspended", func() {
			close(held)
			<-release
		})
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatalf("suspension ended before the hold: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("suspension never reached the hold")
	}
	if tryBrandKeyShared(t, owner, f.brandID) {
		t.Fatal("an in-flight governed brand status change must hold the brand gate key EXCLUSIVE")
	}
	other := seedBrandPinTenant(t, owner)
	if !tryBrandKeyShared(t, owner, other.brandID) {
		t.Fatal("another brand's key must be unaffected")
	}
	releaseOnce()
	if err := <-done; err != nil {
		t.Fatalf("suspension: %v", err)
	}
	if !tryBrandKeyShared(t, owner, f.brandID) {
		t.Fatal("the key must be released at commit")
	}

	// A tenant status change takes the tenant key only.
	g := seedBrandPinTenant(t, owner)
	held2, release2, done2 := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	released2 := false
	release2Once := func() {
		if !released2 {
			released2 = true
			close(release2)
		}
	}
	t.Cleanup(release2Once)
	go func() {
		done2 <- launchfix.TrySetTenantStatusHoldingOn(ctx, t, owner, g.tenantID, "suspended", func() {
			close(held2)
			<-release2
		})
	}()
	select {
	case <-held2:
	case err := <-done2:
		t.Fatalf("tenant suspension ended before the hold: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("tenant suspension never reached the hold")
	}
	if !tryBrandKeyShared(t, owner, g.brandID) {
		t.Fatal("a tenant status change must not take the brand key")
	}
	release2Once()
	if err := <-done2; err != nil {
		t.Fatalf("tenant suspension: %v", err)
	}

	// A same-status UPDATE (another column) changes no status and takes no key.
	h := seedBrandPinTenant(t, owner)
	inTx, release3, done3 := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	released3 := false
	release3Once := func() {
		if !released3 {
			released3 = true
			close(release3)
		}
	}
	t.Cleanup(release3Once)
	go func() {
		done3 <- owner.WithTenant(ctx, h.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `UPDATE brands SET name = name || ' x' WHERE id = $1`, h.brandID); err != nil {
				return err
			}
			close(inTx)
			<-release3
			return nil
		})
	}()
	select {
	case <-inTx:
	case err := <-done3:
		t.Fatalf("name update ended before the hold: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("name update never reached the hold")
	}
	if !tryBrandKeyShared(t, owner, h.brandID) {
		t.Fatal("an UPDATE that does not change the status must not take the brand key")
	}
	release3Once()
	if err := <-done3; err != nil {
		t.Fatalf("name update: %v", err)
	}
}

// RequireBrandActiveForGameplay: active passes; pending_launch / suspended / closed, a nil
// id, a foreign brand and a brand named under the wrong tenant fail closed with the brand
// sentinel (never the tenant one). Runtime role.
func TestRequireBrandActiveForGameplay_Matrix_FailClosed(t *testing.T) {
	owner := brandPinOwnerPool(t)
	rt := brandPinRuntimePool(t)
	ctx := context.Background()
	check := func(scope, tenantID, brandID uuid.UUID) error {
		return rt.WithTenant(ctx, scope, func(ctx context.Context, tx pgx.Tx) error {
			return RequireBrandActiveForGameplay(ctx, tx, tenantID, brandID)
		})
	}
	a := seedBrandPinTenant(t, owner)
	b := seedBrandPinTenant(t, owner)
	if err := check(a.tenantID, a.tenantID, a.brandID); err != nil {
		t.Fatalf("active brand must pass: %v", err)
	}
	for name, c := range map[string][3]uuid.UUID{
		"nil brand":                        {a.tenantID, a.tenantID, uuid.Nil},
		"nil tenant":                       {a.tenantID, uuid.Nil, a.brandID},
		"foreign active brand, own tenant": {a.tenantID, a.tenantID, b.brandID},
		"own active brand, foreign tenant": {a.tenantID, b.tenantID, a.brandID},
		"nonexistent brand":                {a.tenantID, a.tenantID, uuid.New()},
	} {
		err := check(c[0], c[1], c[2])
		if !errors.Is(err, ErrBrandNotActiveForGameplay) || errors.Is(err, ErrNotActiveForGameplay) {
			t.Fatalf("%s: want ErrBrandNotActiveForGameplay only, got %v", name, err)
		}
	}
	// The brand gate never reads the tenant (decision 23 analogue): a suspended tenant with
	// an active brand passes the brand-only check.
	launchfix.SetTenantStatus(t, b.tenantID, "suspended")
	if err := check(b.tenantID, b.tenantID, b.brandID); err != nil {
		t.Fatalf("the brand gate must not read the tenant status: %v", err)
	}
	for _, st := range []string{"pending_launch", "suspended", "closed"} {
		f := seedBrandPinTenant(t, owner)
		if st == "pending_launch" {
			if err := launchfix.ForcePending(ctx, t, owner, f.tenantID, &f.brandID); err != nil {
				t.Fatal(err)
			}
		} else {
			launchfix.SetBrandStatus(t, f.tenantID, f.brandID, st)
		}
		err := check(f.tenantID, f.tenantID, f.brandID)
		if !errors.Is(err, ErrBrandNotActiveForGameplay) || !strings.Contains(err.Error(), "status="+st) {
			t.Fatalf("%s brand: want ErrBrandNotActiveForGameplay with status=%s, got %v", st, st, err)
		}
	}
}

// Catalog: the 0129 objects exist with the reviewed shape.
func TestBrandGate_Catalog_0129(t *testing.T) {
	owner := brandPinOwnerPool(t)
	var def string
	var deferrable, deferred bool
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT pg_get_triggerdef(t.oid), t.tgdeferrable, t.tginitdeferred
			  FROM pg_trigger t
			 WHERE t.tgrelid = 'public.ledger_transactions'::regclass AND t.tgname = 'ledger_wager_brand_active_guard'`).Scan(&def, &deferrable, &deferred)
	}); err != nil {
		t.Fatalf("ledger_wager_brand_active_guard missing: %v", err)
	}
	if !deferrable || !deferred || !strings.Contains(def, "AFTER INSERT") ||
		!strings.Contains(def, "'casino_bet'") || !strings.Contains(def, "'sportsbook_bet'") {
		t.Fatalf("unexpected trigger shape: deferrable=%v deferred=%v %s", deferrable, deferred, def)
	}
	// Only the two NEW-STAKE types: terminal stake returns and settlements are not brand-gated.
	for _, notGated := range []string{"casino_win", "casino_rollback", "sportsbook_settlement", "sportsbook_void", "sportsbook_rollback", "tombstone"} {
		if strings.Contains(def, "'"+notGated+"'") {
			t.Fatalf("the brand backstop must not cover %s: %s", notGated, def)
		}
	}
	for _, fn := range []string{"brand_status_gate_key", "ledger_wager_brand_active_guard", "launch_subject_status_guard"} {
		var secdef bool
		var cfg string
		if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT p.prosecdef, COALESCE(array_to_string(p.proconfig, ','), '')
				FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace AND p.proname = $1`, fn).Scan(&secdef, &cfg)
		}); err != nil {
			t.Fatalf("%s: %v", fn, err)
		}
		if secdef || !strings.Contains(cfg, "search_path=pg_catalog, public, pg_temp") {
			t.Fatalf("%s must pin search_path and not be SECURITY DEFINER (secdef=%v cfg=%q)", fn, secdef, cfg)
		}
	}
	var src string
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'launch_subject_status_guard' AND pronamespace = 'public'::regnamespace`).Scan(&src)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(src, "pg_advisory_xact_lock(public.brand_status_gate_key(OLD.id))") {
		t.Fatal("zz_launch_status_governed's function must take the brand gate key EXCLUSIVE (0129)")
	}
}

// 0129 up / down / up on a THROWAWAY scratch database: the down restores the 0128 schema
// byte for byte (including launch_subject_status_guard's body), and up again is identical.
func TestMigration0129_UpDownUp_RestoresMigration0128(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0129_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0129Version-1)); err != nil {
		t.Fatal(err)
	}
	snap128 := lgSnapshot(t, pool)
	full := lgMigDir(t, migration0129Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	snap129 := lgSnapshot(t, pool)
	if snap129 == snap128 {
		t.Fatal("0129 changed nothing in the schema snapshot (vacuous)")
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0129 down: %v", err)
	}
	if got := lgSnapshot(t, pool); got != snap128 {
		t.Fatalf("0129 down did not restore the 0128 schema:\n%s", lgSnapDiff(snap128, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := lgSnapshot(t, pool); got != snap129 {
		t.Fatalf("up after down differs from the first up:\n%s", lgSnapDiff(snap129, got))
	}
}
