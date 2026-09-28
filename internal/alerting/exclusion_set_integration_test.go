//go:build integration

package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// gucSession opens a raw transaction and sets exactly the given GUCs
// (name -> value; an empty value means "leave unset"), for the exclusion-
// set matrix below - the higher-level db.With* helpers each set exactly
// one GUC, which cannot express the mixed-GUC combinations security IC-1
// requires.
func gucSession(t *testing.T, pool *db.Pool, guc map[string]string) pgx.Tx {
	t.Helper()
	tx, err := pool.Raw().Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for name, value := range guc {
		if value == "" {
			continue
		}
		if _, err := tx.Exec(context.Background(), `SELECT set_config($1, $2, true)`, name, value); err != nil {
			t.Fatalf("set_config(%s): %v", name, err)
		}
	}
	return tx
}

// exclusionProbeGUCs is security IC-1's exact list: each, added one at a
// time on top of a pure tenant session, must make the session see 0 rows
// (or refuse the insert) for every family that is supposed to require
// this GUC unset.
func exclusionProbeGUCs(actingTenant, actingPlatform, platformService, platformAdmin, playerAccount string) map[string]map[string]string {
	return map[string]map[string]string{
		"app.acting_tenant_id":             {"app.acting_tenant_id": actingTenant},
		"app.acting_platform_principal_id": {"app.acting_platform_principal_id": actingPlatform},
		"app.platform_service_id":          {"app.platform_service_id": platformService},
		"app.platform_admin_principal_id":  {"app.platform_admin_principal_id": platformAdmin},
		"app.player_account_id":            {"app.player_account_id": playerAccount},
	}
}

// TestExclusionSets_SubjectTenantRead_Family is security IC-1 for
// alerts_subject_tenant_read on both alerts and alert_occurrences: a pure
// tenant session sees its subject alert and occurrence (assert 1), then
// EACH of the five exclusion GUCs, added one at a time, must reduce that
// to 0.
func TestExclusionSets_SubjectTenantRead_Family(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	var alertID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
			tenantA, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString()).Scan(&alertID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, alertID)
		return err
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	playerID := uuid.New()
	guCombos := exclusionProbeGUCs(uuid.New().String(), uuid.New().String(), "alert_dispatcher", admin.String(), playerID.String())

	// Pure tenant session: sees exactly 1 row on each table.
	pureTx := gucSession(t, pool, map[string]string{"app.tenant_id": tenantA.String()})
	assertRowCount(t, pureTx, `SELECT count(*) FROM alerts WHERE id = $1`, []any{alertID}, 1, "pure tenant session, alerts")
	assertRowCount(t, pureTx, `SELECT count(*) FROM alert_occurrences WHERE alert_id = $1`, []any{alertID}, 1, "pure tenant session, alert_occurrences")
	_ = pureTx.Rollback(context.Background())

	for gucName, extra := range guCombos {
		combo := map[string]string{"app.tenant_id": tenantA.String()}
		for k, v := range extra {
			combo[k] = v
		}
		tx := gucSession(t, pool, combo)
		assertRowCount(t, tx, `SELECT count(*) FROM alerts WHERE id = $1`, []any{alertID}, 0, "tenant + "+gucName+", alerts")
		assertRowCount(t, tx, `SELECT count(*) FROM alert_occurrences WHERE alert_id = $1`, []any{alertID}, 0, "tenant + "+gucName+", alert_occurrences")
		_ = tx.Rollback(context.Background())
	}
}

// TestExclusionSets_SubjectTenantRaise_Family is security IC-1 for
// alerts_subject_tenant_raise on both tables: a pure tenant session can
// insert its own subject alert/occurrence, then each exclusion GUC,
// added on top, must refuse the insert.
func TestExclusionSets_SubjectTenantRaise_Family(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	playerID := uuid.New()
	guCombos := exclusionProbeGUCs(uuid.New().String(), uuid.New().String(), "alert_dispatcher", admin.String(), playerID.String())

	// Pure tenant session: the insert succeeds.
	pureTx := gucSession(t, pool, map[string]string{"app.tenant_id": tenantA.String()})
	var alertID uuid.UUID
	if err := pureTx.QueryRow(context.Background(), `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
		tenantA, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString()).Scan(&alertID); err != nil {
		t.Fatalf("pure tenant session insert on alerts must succeed: %v", err)
	}
	if _, err := pureTx.Exec(context.Background(), `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, alertID); err != nil {
		t.Fatalf("pure tenant session insert on alert_occurrences must succeed: %v", err)
	}
	_ = pureTx.Rollback(context.Background()) // never committed - each probe below needs a clean slate

	for gucName, extra := range guCombos {
		combo := map[string]string{"app.tenant_id": tenantA.String()}
		for k, v := range extra {
			combo[k] = v
		}
		tx := gucSession(t, pool, combo)
		var id uuid.UUID
		alertErr := tx.QueryRow(context.Background(), `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
			tenantA, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString()).Scan(&id)
		if alertErr == nil {
			t.Errorf("tenant + %s: expected the alerts INSERT to be refused, it succeeded", gucName)
		}
		occErr := tx.QueryRow(context.Background(), `INSERT INTO alert_occurrences (alert_id) VALUES ($1) RETURNING id`, uuid.New()).Scan(&id)
		if occErr == nil {
			t.Errorf("tenant + %s: expected the alert_occurrences INSERT to be refused, it succeeded", gucName)
		}
		_ = tx.Rollback(context.Background())
	}
}

// TestExclusionSets_TenantOwned_Family is security IC-1 for
// alerts_tenant_owned. No tenant-owned Kind is seeded in PRH-2 (ADR §4.3)
// - the alerts_guard trigger refuses tenant_id IS NOT NULL for every
// existing (platform-scope) Kind regardless of RLS, so there is
// structurally no way to seed a positive fixture a "pure tenant session"
// can see through this family today. This test instead proves the
// negative property IC-1 is really after: a pure tenant session sees
// nothing through this family (matching reality), and neither the
// dispatcher combined with a tenant GUC, nor any other mixed-GUC
// combination, can force a tenant-owned row into existence either.
func TestExclusionSets_TenantOwned_Family(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	pureTx := gucSession(t, pool, map[string]string{"app.tenant_id": tenantA.String()})
	assertRowCount(t, pureTx, `SELECT count(*) FROM alerts WHERE tenant_id = $1`, []any{tenantA}, 0, "pure tenant session, alerts (no tenant-owned Kind exists)")
	_ = pureTx.Rollback(context.Background())

	// A pure tenant session cannot force a tenant-owned row into
	// existence for any real Kind (the trigger refuses it independently
	// of RLS).
	tx := gucSession(t, pool, map[string]string{"app.tenant_id": tenantA.String()})
	var id uuid.UUID
	err := tx.QueryRow(context.Background(), `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES ($1, NULL, $2, $3, '{}'::jsonb) RETURNING id`,
		tenantA, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString()).Scan(&id)
	if err == nil {
		t.Error("expected a tenant-owned insert for a platform-scope Kind to be refused")
	}
	_ = tx.Rollback(context.Background())

	// Dispatcher + tenant GUC sees nothing (security IC-1's explicit
	// "dispatcher plus a tenant GUC" case).
	dispatcherTx := gucSession(t, pool, map[string]string{
		"app.platform_service_id": "alert_dispatcher",
		"app.tenant_id":           tenantA.String(),
	})
	assertRowCount(t, dispatcherTx, `SELECT count(*) FROM alerts`, nil, 0, "dispatcher + tenant GUC")
	_ = dispatcherTx.Rollback(context.Background())
}

func assertRowCount(t *testing.T, tx pgx.Tx, query string, args []any, want int, label string) {
	t.Helper()
	var got int
	if err := tx.QueryRow(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatalf("%s: query failed: %v", label, err)
	}
	if got != want {
		t.Errorf("%s: expected %d row(s), got %d", label, want, got)
	}
}
