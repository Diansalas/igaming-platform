//go:build integration

package alerting

import (
	"context"
	"strings"
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

	// A COMMITTED seed alert (not the transient one used for the pure-
	// session insert probe below) - IR-1: the occurrence probe under each
	// mixed-GUC combo must target a REAL, existing alert id. Probing with
	// a random id would be refused by the FK/existence check alone,
	// regardless of RLS, making the probe vacuous (it would never
	// actually reach the alerts_subject_tenant_raise policy on
	// alert_occurrences at all).
	var seedAlertID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
			tenantA, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString()).Scan(&seedAlertID)
	}); err != nil {
		t.Fatalf("seed committed alert: %v", err)
	}

	// Pure tenant session: both inserts succeed.
	pureTx := gucSession(t, pool, map[string]string{"app.tenant_id": tenantA.String()})
	var alertID uuid.UUID
	if err := pureTx.QueryRow(context.Background(), `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
		tenantA, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString()).Scan(&alertID); err != nil {
		t.Fatalf("pure tenant session insert on alerts must succeed: %v", err)
	}
	if _, err := pureTx.Exec(context.Background(), `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, seedAlertID); err != nil {
		t.Fatalf("pure tenant session insert on alert_occurrences (against the real seed alert) must succeed: %v", err)
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
		// Probed against the REAL, committed seed alert - so a refusal
		// here can only come from the alerts_subject_tenant_raise policy
		// on alert_occurrences itself, never from the row simply not
		// existing.
		occErr := tx.QueryRow(context.Background(), `INSERT INTO alert_occurrences (alert_id) VALUES ($1) RETURNING id`, seedAlertID).Scan(&id)
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

	// IR-1: seed a genuinely visible row FIRST (a committed subject-tenant
	// alert), so the "dispatcher + tenant GUC sees nothing" assertion
	// below is meaningful regardless of what else exists in a shared
	// testPool database - without a known-visible row, `count = 0` could
	// trivially hold even if the mixed session wrongly satisfied some
	// OTHER family's predicate that happened to match nothing anyway.
	var visibleAlertID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
			tenantA, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString()).Scan(&visibleAlertID)
	}); err != nil {
		t.Fatalf("seed a visible alert: %v", err)
	}
	// Confirm it really is visible to a pure tenant session before probing
	// the mixed one - otherwise "sees nothing" below would be vacuous.
	confirmTx := gucSession(t, pool, map[string]string{"app.tenant_id": tenantA.String()})
	assertRowCount(t, confirmTx, `SELECT count(*) FROM alerts WHERE id = $1`, []any{visibleAlertID}, 1, "sanity: pure tenant session sees the seeded alert")
	_ = confirmTx.Rollback(context.Background())

	// Dispatcher + tenant GUC sees nothing (security IC-1's explicit
	// "dispatcher plus a tenant GUC" case) - not even the alert we just
	// proved is otherwise visible.
	dispatcherTx := gucSession(t, pool, map[string]string{
		"app.platform_service_id": "alert_dispatcher",
		"app.tenant_id":           tenantA.String(),
	})
	assertRowCount(t, dispatcherTx, `SELECT count(*) FROM alerts WHERE id = $1`, []any{visibleAlertID}, 0, "dispatcher + tenant GUC, the known-visible alert")
	assertRowCount(t, dispatcherTx, `SELECT count(*) FROM alerts`, nil, 0, "dispatcher + tenant GUC, whole table")
	_ = dispatcherTx.Rollback(context.Background())
}

// TestExclusionSets_SubjectTenantRaise_OccurrencesPolicyTextPinsExclusions
// is IR-1's follow-up: behaviourally, alert_occurrences_guard's own
// alerting_session_scope() call ALREADY raises for every one of the five
// C-102-9 exclusion GUCs combined with app.tenant_id (each one independently
// makes the session unresolvable or "mixed" before RLS is ever evaluated -
// see that function's own logic), so a BEHAVIOURAL probe of
// alerts_subject_tenant_raise ON alert_occurrences can never distinguish
// "the trigger blocked it" from "the RLS policy blocked it": the trigger
// always fires first and always blocks it, by construction, regardless of
// what the RLS policy's own WITH CHECK says. Confirmed empirically: a
// hand-applied mutant that stripped all five exclusion clauses from that
// policy left TestExclusionSets_SubjectTenantRaise_Family passing (the
// mutant SURVIVED the behavioural probe).
//
// This test instead pins the POLICY DEFINITION TEXT directly via
// pg_get_expr/pg_policies, so removing the clauses is caught regardless
// of the trigger's own redundant defence - the two controls are
// independently verified, not conflated.
func TestExclusionSets_SubjectTenantRaise_OccurrencesPolicyTextPinsExclusions(t *testing.T) {
	pool := testPool(t)
	var withCheck string
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT pg_get_expr(pol.polwithcheck, pol.polrelid)
			FROM pg_policy pol
			JOIN pg_class c ON c.oid = pol.polrelid
			WHERE c.relname = 'alert_occurrences' AND pol.polname = 'alerts_subject_tenant_raise'
		`).Scan(&withCheck)
	})
	if err != nil {
		t.Fatalf("read policy definition: %v", err)
	}
	for _, guc := range []string{
		"app.player_account_id",
		"app.platform_admin_principal_id",
		"app.platform_service_id",
		"app.acting_tenant_id",
		"app.acting_platform_principal_id",
	} {
		if !strings.Contains(withCheck, guc) {
			t.Errorf("expected alerts_subject_tenant_raise ON alert_occurrences' WITH CHECK to reference %q, got: %s", guc, withCheck)
		}
	}
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
