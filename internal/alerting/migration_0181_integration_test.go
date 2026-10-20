//go:build integration

package alerting_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Migration 0181 (lic10): the alert Kind aml.screening_attention. Column-for-column parity with the Go mirror is asserted for
// EVERY Kind by the full parity test; this file pins the migration's own behaviour: the seed row, the down round trip (the
// whole of 0181, residence maker-checker and AML screening triggers, is removed on an empty database and comes back), and the
// down refusal while an alert of the Kind exists. NOT RUN by its author (database harness refused): the owner runs it.

func TestMigration0181_SeedRoundTripAndDownRefusal(t *testing.T) {
	pool, dir := scratchThroughVersion(t, "m181_dn", 181)
	ctx := context.Background()
	kindRow := func() (n int, sev string, keys []string) {
		t.Helper()
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM alert_kinds WHERE kind = 'aml.screening_attention'`).Scan(&n); err != nil {
				return err
			}
			if n == 0 {
				return nil
			}
			return tx.QueryRow(ctx, `SELECT severity, allowed_keys FROM alert_kinds WHERE kind = 'aml.screening_attention'`).Scan(&sev, &keys)
		}); err != nil {
			t.Fatal(err)
		}
		sort.Strings(keys)
		return n, sev, keys
	}
	tables := func() (n int) {
		t.Helper()
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relname IN
				('residence_approval_policies', 'residence_approval_policy_approvals', 'residence_determination_checks',
				 'aml_screening_trigger_policies', 'aml_screening_requests')`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	n, sev, keys := kindRow()
	if n != 1 || sev != "p2" || strings.Join(keys, ",") != "reason,request_id,trigger" {
		t.Fatalf("seed row = %d %q %v", n, sev, keys)
	}
	for _, k := range keys {
		if strings.Contains(k, "person") || strings.Contains(k, "list") || strings.Contains(k, "match") {
			t.Fatalf("the payload must never allow a person, list or match key: %v", keys)
		}
	}
	if tables() != 5 {
		t.Fatalf("0181 must create 5 tables, found %d", tables())
	}
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down on an empty database: %v", err)
	}
	if n, _, _ := kindRow(); n != 0 || tables() != 0 {
		t.Fatal("down must remove the Kind and all five tables")
	}
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || len(applied) != 1 || applied[0] != 181 {
		t.Fatalf("re-up: %v %v", applied, err)
	}

	admin := ownerAdmin(t, pool)
	tenantID := uuid.New()
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, $2, $2, 'own_licence', 'active')`,
			tenantID, "m181-"+tenantID.String()[:8])
		return err
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (kind, discriminator, subject_tenant_id, attributes) VALUES ('aml.screening_attention', 'request:x', $1, '{}'::jsonb)`, tenantID)
		return err
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	if err == nil || !strings.Contains(err.Error(), "alerts of kind aml.screening_attention exist") {
		t.Fatalf("down must refuse while an alert of the Kind exists, got %v", err)
	}
	if n, _, _ := kindRow(); n != 1 || tables() != 5 {
		t.Fatal("a refused down must leave the Kind and the tables in place")
	}
}
