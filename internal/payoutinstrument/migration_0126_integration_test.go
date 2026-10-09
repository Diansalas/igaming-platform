//go:build integration

package payoutinstrument

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// B13-B migration 0126 (placeholder number; ADR 0111 section 18). It replaces ONE function,
// withdrawal_requests_payout_binding_guard(), so a NEW withdrawal without a binding is refused.
const migration0126Version = 126

// seed0126 creates a tenant/brand/player/wallet on a scratch pool and returns their ids.
func seed0126(t *testing.T, pool *db.Pool) (tenantID, brandID, playerID, walletID uuid.UUID) {
	t.Helper()
	tenantID, brandID, playerID, personID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1,$2,'t','under_platform_licence')`, tenantID, "m126-"+tenantID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1,$2,'b','b')`, brandID, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,'m@example.test','x','active')`,
			playerID, tenantID, brandID, personID); err != nil {
			return err
		}
		wl, err := wallet.GetOrCreate(ctx, tx, tenantID, brandID, playerID, "EUR")
		walletID = wl.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return
}

// Whole-schema: 0126 changes exactly one function body and nothing else; the down restores the
// 0123 schema byte for byte; re-up equals the first up.
func TestMigration0126_UpDownUp_WholeSchema(t *testing.T) {
	pool := scratchThrough(t, "b13m126_", migration0126Version-1)
	pre := schemaSnapshot(t, pool)
	dir := migDir(t, migration0126Version)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	up := schemaSnapshot(t, pool)
	if up == pre {
		t.Fatal("0126 changed nothing?")
	}
	changed := strings.Split(snapDiff(pre, up), "\n")
	for _, l := range changed {
		if !strings.Contains(l, "function:withdrawal_requests_payout_binding_guard()") {
			t.Errorf("0126 changed something other than the guard function: %s", l)
		}
	}
	if len(changed) != 2 { // one "-" (old body hash) and one "+" (new body hash)
		t.Errorf("expected exactly the guard function's body hash to differ, got %d differences:\n%s", len(changed), snapDiff(pre, up))
	}
	// L-4: the guard source names PI046 only while 0126 is applied (the startup check keys on it).
	guardSrc := func() string {
		var src string
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'withdrawal_requests_payout_binding_guard' AND pronamespace = 'public'::regnamespace`).Scan(&src)
		}); err != nil {
			t.Fatal(err)
		}
		return src
	}
	if !strings.Contains(guardSrc(), bindingGuardMarker) {
		t.Fatal("0126 up: the guard source must contain PI046")
	}
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if strings.Contains(guardSrc(), bindingGuardMarker) {
		t.Fatal("0126 down: the 0123 guard must not contain PI046")
	}
	if got := schemaSnapshot(t, pool); got != pre {
		t.Fatalf("0126 down did not restore the 0123 schema exactly:\n%s", snapDiff(pre, got))
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatalf("0126 re-up differs from the first up:\n%s", snapDiff(up, got))
	}
}

// Legacy rows are UNAFFECTED: a NULL/NULL row that exists before 0126 survives the up and the down
// unchanged (no backfill, no rewrite), a NEW NULL/NULL insert is refused (PI046) only while 0126 is
// applied, and the down (0123 behaviour) tolerates it again.
func TestMigration0126_LegacyRowsUnaffectedAndNullArmClosed(t *testing.T) {
	pool := scratchThrough(t, "b13m126l_", migration0126Version-1)
	tenantID, brandID, playerID, walletID := seed0126(t, pool)
	insert := func() (uuid.UUID, error) {
		var id uuid.UUID
		err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key)
				VALUES ($1,$2,$3,$4,'EUR',100,$5) RETURNING id`, tenantID, brandID, playerID, walletID, "k-"+uuid.NewString()).Scan(&id)
		})
		return id, err
	}
	legacy, err := insert()
	if err != nil {
		t.Fatalf("0123 tolerates NULL/NULL: %v", err)
	}
	dir := migDir(t, migration0126Version)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	readLegacy := func() (nullBinding bool, state string) {
		if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT payout_instrument_id IS NULL AND payout_instrument_fingerprint IS NULL, state FROM withdrawal_requests WHERE id = $1`, legacy).Scan(&nullBinding, &state)
		}); err != nil {
			t.Fatalf("read legacy row: %v", err)
		}
		return
	}
	if nb, st := readLegacy(); !nb || st != "requested" {
		t.Fatalf("legacy row changed by the up: null=%v state=%s", nb, st)
	}
	_, err = insert()
	requireCode(t, err, "PI046", "NULL/NULL insert while 0126 is applied")
	// the legacy row can still be transitioned (its other triggers are untouched) but not bound
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if nb, _ := readLegacy(); !nb {
		t.Fatal("legacy row changed by the down")
	}
	if _, err := insert(); err != nil {
		t.Fatalf("after the down the 0123 arm tolerates NULL/NULL again: %v", err)
	}
}
