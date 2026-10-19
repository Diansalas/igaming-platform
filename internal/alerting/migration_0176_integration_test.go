//go:build integration

package alerting_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// Migration 0176 (licensing slice 1): the Kinds licence.expired / licence.expiring, the licence_versions history and the
// down refusals. Column-for-column parity of the Kinds with the Go mirror is covered by TestKindParity_*.

func seedLicence(t *testing.T, pool *db.Pool, admin uuid.UUID) uuid.UUID {
	t.Helper()
	jur, lic := uuid.New(), uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'J')`, jur, "J-"+jur.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', 'L-1')`, lic, jur)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return lic
}

func TestMigration0176_KindsSeededAndDispatcherOnly(t *testing.T) {
	pool, _ := scratchThroughVersion(t, "m176_k", 176)
	ctx := context.Background()
	for kind, sev := range map[string]string{"licence.expired": "p1", "licence.expiring": "p2"} {
		var gotSev, scope string
		var subj, tenantRaisable bool
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT severity, scope, requires_subject, in_tx_raisable_by_tenant FROM alert_kinds WHERE kind = $1`, kind).Scan(&gotSev, &scope, &subj, &tenantRaisable)
		}); err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		if gotSev != sev || scope != "platform" || subj || tenantRaisable {
			t.Fatalf("%s: %s %s subject=%v tenantRaisable=%v", kind, gotSev, scope, subj, tenantRaisable)
		}
	}

	lic := uuid.New()
	attrs := map[string]alerting.AttrValue{"trigger": "status_expired", "tenant_count": 1, "brand_count": 0, "tenant_ids": uuid.NewString(), "brand_ids": "", "truncated": false}
	// The dispatcher identity raises it, and a repeat attaches an occurrence to the SAME open alert.
	for i := 0; i < 2; i++ {
		if err := alerting.RaiseLicenceSweep(ctx, pool, alerting.KindLicenceExpired, lic, attrs); err != nil {
			t.Fatalf("raise %d: %v", i, err)
		}
	}
	var alerts, occ int
	admin := ownerAdmin(t, pool)
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'licence.expired' AND discriminator = $1`, "licence:"+lic.String()).Scan(&alerts); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_occurrences WHERE kind = 'licence.expired'`).Scan(&occ)
	}); err != nil {
		t.Fatal(err)
	}
	if alerts != 1 || occ < 2 {
		t.Fatalf("alerts=%d occurrences=%d, want 1 open alert with 2 occurrences", alerts, occ)
	}

	// A platform admin, a tenant session and a scopeless session cannot raise it (RESTRICTIVE dispatcher-only policy).
	tenant := uuid.New()
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, 'm176', $2, 'own_licence', 'active')`, tenant, "m176-"+tenant.String()[:8])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO alerts (kind, discriminator, attributes) VALUES ('licence.expiring', 'licence:x', '{}'::jsonb)`
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, ins); return err }); err == nil {
		t.Fatal("a platform admin must not be able to raise licence.expiring")
	}
	if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, ins); return err }); err == nil {
		t.Fatal("a tenant session must not be able to raise licence.expiring")
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, ins); return err }); err == nil {
		t.Fatal("a scopeless session must not be able to raise licence.expiring")
	}
	// An attribute outside the Kind's allowlist, a wrong Kind and a nil licence are refused in Go.
	if err := alerting.RaiseLicenceSweep(ctx, pool, alerting.KindLicenceExpired, lic, map[string]alerting.AttrValue{"country": "MT"}); err == nil {
		t.Fatal("an attribute outside the allowlist must be refused")
	}
	if err := alerting.RaiseLicenceSweep(ctx, pool, alerting.KindAlertingUnrouted, lic, nil); err == nil {
		t.Fatal("a non-licence Kind must be refused")
	}
	if err := alerting.RaiseLicenceSweep(ctx, pool, alerting.KindLicenceExpiring, uuid.Nil, nil); err == nil {
		t.Fatal("a nil licence id must be refused")
	}
}

func TestMigration0176_DownRefusesWhileChangeHistoryExists_AndRoundTripsWhenEmpty(t *testing.T) {
	pool, dir := scratchThroughVersion(t, "m176_dn1", 176)
	ctx := context.Background()
	admin := ownerAdmin(t, pool)
	// A fresh database has no versions: a clean down and re-up round trip works.
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("clean down: %v", err)
	}
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || len(applied) != 1 || applied[0] != 176 {
		t.Fatalf("re-up: %v %v", applied, err)
	}
	// A licence with only its creation snapshot loses nothing on a down; once it has CHANGED, the history would be lost:
	// the down must refuse and leave the schema intact.
	lic := seedLicence(t, pool, admin)
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE licences SET status = 'suspended' WHERE id = $1`, lic)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	if err == nil || !strings.Contains(err.Error(), "licence_versions history exists") {
		t.Fatalf("down must refuse while versions exist, got %v", err)
	}
	var n int
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licence_versions WHERE licence_id = $1`, lic).Scan(&n)
	}); err != nil || n != 2 {
		t.Fatalf("a refused down must leave the history: n=%d err=%v", n, err)
	}
}

func TestMigration0176_DownRefusesWhileLicenceAlertsExist(t *testing.T) {
	pool, dir := scratchThroughVersion(t, "m176_dn2", 176)
	ctx := context.Background()
	if err := alerting.RaiseLicenceSweep(ctx, pool, alerting.KindLicenceExpiring, uuid.New(), map[string]alerting.AttrValue{"trigger": "review_due"}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	if err == nil || !strings.Contains(err.Error(), "alerts of kind licence.expired / licence.expiring exist") {
		t.Fatalf("down must refuse while licence alerts exist, got %v", err)
	}
}

func TestMigration0176_BaselineVersionForExistingLicences(t *testing.T) {
	pool, dir := scratchThroughVersion(t, "m176_bl", 175)
	ctx := context.Background()
	admin := ownerAdmin(t, pool)
	jur, lic := uuid.New(), uuid.New()
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'J')`, jur, "J-"+jur.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number, status) VALUES ($1, $2, 'platform', 'OLD-1', 'suspended')`, lic, jur)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"0176_licence_lifecycle_and_expiry_alerts.up.sql", "0176_licence_lifecycle_and_expiry_alerts.down.sql"} {
		b, err := os.ReadFile(filepath.Join("../../migrations", n))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, n), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || len(applied) != 1 || applied[0] != 176 {
		t.Fatalf("up: %v %v", applied, err)
	}
	var no int
	var op, status string
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT version_no, operation, status FROM licence_versions WHERE licence_id = $1`, lic).Scan(&no, &op, &status)
	}); err != nil || no != 1 || op != "baseline" || status != "suspended" {
		t.Fatalf("baseline version: %d %q %q %v", no, op, status, err)
	}
	// Baseline rows alone do not block the down.
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down with only baseline rows: %v", err)
	}
}
