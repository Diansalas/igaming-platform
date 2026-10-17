//go:build integration

package alerting_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Migration 0170 (F5(b)): the alert Kind kyc.residence_into_open_market. Column-for-column parity with the Go
// mirror is asserted for EVERY Kind by TestKindParity_FullColumn_BothDirections_50; this file pins the migration's
// own behaviour: the seed row, the down round trip, and the down refusal while alerts of the Kind exist.

func TestMigration0170_SeedRoundTripAndDownRefusal(t *testing.T) {
	pool, dir := scratchThroughVersion(t, "m170_dn", 170)
	ctx := context.Background()
	kindRow := func() (n int, sev string, keys []string) {
		t.Helper()
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM alert_kinds WHERE kind = 'kyc.residence_into_open_market'`).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			return tx.QueryRow(ctx, `SELECT severity, allowed_keys FROM alert_kinds WHERE kind = 'kyc.residence_into_open_market'`).Scan(&sev, &keys)
		}); err != nil {
			t.Fatal(err)
		}
		return n, sev, keys
	}
	n, sev, keys := kindRow()
	if n != 1 || sev != "p2" || strings.Join(keys, ",") != "verification_id,brand_id,request_id" {
		t.Fatalf("seed row = %d %q %v", n, sev, keys)
	}
	for _, k := range keys {
		if strings.Contains(k, "country") {
			t.Fatalf("the payload must never allow a country key: %v", keys)
		}
	}
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down with no alerts: %v", err)
	}
	if n, _, _ := kindRow(); n != 0 {
		t.Fatal("down must remove the Kind")
	}
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || len(applied) != 1 || applied[0] != 170 {
		t.Fatalf("re-up: %v %v", applied, err)
	}

	admin := ownerAdmin(t, pool)
	tenantID := uuid.New()
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, $2, $2, 'own_licence', 'active')`,
			tenantID, "m170-"+tenantID.String()[:8])
		return err
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (kind, discriminator, subject_tenant_id, attributes) VALUES ('kyc.residence_into_open_market', 'verification:x', $1, '{}'::jsonb)`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	if err == nil || !strings.Contains(err.Error(), "alerts of kind kyc.residence_into_open_market exist") {
		t.Fatalf("down must refuse while an alert of the Kind exists, got %v", err)
	}
	if n, _, _ := kindRow(); n != 1 {
		t.Fatal("a refused down must leave the Kind in place")
	}
}
