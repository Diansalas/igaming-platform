//go:build integration

package alerting_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// Migration 0178 (security review F-3): licence.sweep_failed (p1) and licence.unmonitored (p2) are seeded, platform scope,
// subjectless, raisable only under the alert_dispatcher identity; the down refuses while such alerts exist. Column-for-column
// parity with the Go mirror is covered by TestKindParity_*.
func TestMigration0178_SweepKindsSeededAndDispatcherOnly(t *testing.T) {
	pool, _ := scratchThroughVersion(t, "m178_k", 178)
	ctx := context.Background()
	for kind, sev := range map[string]string{"licence.sweep_failed": "p1", "licence.unmonitored": "p2"} {
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
	// The dispatcher identity raises licence.sweep_failed; a repeat of the same phase attaches to ONE open alert.
	for i := 0; i < 2; i++ {
		if err := alerting.RaiseLicenceSweepFailed(ctx, pool, alerting.LicenceSweepPhaseListTenants, 1); err != nil {
			t.Fatalf("raise %d: %v", i, err)
		}
	}
	// ...and licence.unmonitored for a licence.
	lic := uuid.New()
	if err := alerting.RaiseLicenceSweep(ctx, pool, alerting.KindLicenceUnmonitored, lic, map[string]alerting.AttrValue{"trigger": "no_expiry_recorded", "tenant_count": 1, "tenant_ids": uuid.NewString()}); err != nil {
		t.Fatalf("raise unmonitored: %v", err)
	}
	admin := ownerAdmin(t, pool)
	var failedAlerts, failedOcc, unmon int
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'licence.sweep_failed' AND discriminator = 'sweep:list_tenants'`).Scan(&failedAlerts); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alert_occurrences WHERE kind = 'licence.sweep_failed'`).Scan(&failedOcc); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'licence.unmonitored' AND discriminator = $1`, "licence:"+lic.String()).Scan(&unmon)
	}); err != nil {
		t.Fatal(err)
	}
	if failedAlerts != 1 || failedOcc < 2 || unmon != 1 {
		t.Fatalf("sweep_failed alerts=%d occurrences=%d unmonitored=%d", failedAlerts, failedOcc, unmon)
	}

	// A platform admin, a tenant session and a scopeless session cannot raise either Kind.
	tenant := uuid.New()
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, 'm178', $2, 'own_licence', 'active')`, tenant, "m178-"+tenant.String()[:8])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"licence.sweep_failed", "licence.unmonitored"} {
		ins := `INSERT INTO alerts (kind, discriminator, attributes) VALUES ('` + kind + `', 'forged', '{}'::jsonb)`
		if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, ins); return err }); err == nil {
			t.Fatalf("a platform admin must not be able to raise %s", kind)
		}
		if err := pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, ins); return err }); err == nil {
			t.Fatalf("a tenant session must not be able to raise %s", kind)
		}
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, ins); return err }); err == nil {
			t.Fatalf("a scopeless session must not be able to raise %s", kind)
		}
	}
	// An unknown phase, an attribute outside the allowlist and a nil licence are refused in Go.
	if err := alerting.RaiseLicenceSweepFailed(ctx, pool, "everything", 1); err == nil {
		t.Fatal("an unknown phase must be refused")
	}
	if err := alerting.RaiseLicenceSweep(ctx, pool, alerting.KindLicenceUnmonitored, lic, map[string]alerting.AttrValue{"country": "MT"}); err == nil {
		t.Fatal("an attribute outside the allowlist must be refused")
	}
	if err := alerting.RaiseLicenceSweep(ctx, pool, alerting.KindLicenceUnmonitored, uuid.Nil, nil); err == nil {
		t.Fatal("a nil licence id must be refused")
	}
}

func TestMigration0178_DownRefusesWhileSweepAlertsExist_AndRoundTripsWhenEmpty(t *testing.T) {
	pool, dir := scratchThroughVersion(t, "m178_dn", 178)
	ctx := context.Background()
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("clean down: %v", err)
	}
	if applied, err := pool.MigrateUp(ctx, dir); err != nil || len(applied) != 1 || applied[0] != 178 {
		t.Fatalf("re-up: %v %v", applied, err)
	}
	if err := alerting.RaiseLicenceSweepFailed(ctx, pool, alerting.LicenceSweepPhaseRaise, 3); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(ctx, dir, 1)
	if err == nil || !strings.Contains(err.Error(), "licence.sweep_failed / licence.unmonitored exist") {
		t.Fatalf("down must refuse while sweep alerts exist, got %v", err)
	}
}
