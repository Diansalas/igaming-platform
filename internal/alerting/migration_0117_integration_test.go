//go:build integration

package alerting_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertworld"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// Migration 0117 (ALERT-DELIVERY-1 routing readiness): schema, guards, RLS,
// search_path pins, TEMP-shadow probes and the reversible down. Every test
// that uses a World runs through the real runtime role; owner-only probes run
// in rolled-back transactions on a throwaway database.

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func insertRoute(ctx context.Context, tx pgx.Tx, enabled bool, recipient *string, reason *string) error {
	_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code)
		VALUES ('platform', 'p1', 0, 'mock', $1, $2, $3)`, recipient, enabled, reason)
	return err
}

func strp(s string) *string { return &s }

func TestMigration0117_R1_EnabledRequiresARecipient_DisabledMayBeStaged_DefaultIsDisabled(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_r1")
	admin := w.SeedAdmin()
	run := func(f func(ctx context.Context, tx pgx.Tx) error) error {
		return w.Pool.WithPlatformAdmin(context.Background(), admin, f)
	}
	err := run(func(ctx context.Context, tx pgx.Tx) error {
		return insertRoute(ctx, tx, true, nil, strp("initial_setup"))
	})
	if pgCode(err) != "23514" || !strings.Contains(err.Error(), "alert_routes_enabled_requires_target_check") {
		t.Fatalf("enabled with no recipient must violate R1, got %v", err)
	}
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		return insertRoute(ctx, tx, false, nil, strp("initial_setup"))
	}); err != nil {
		t.Fatalf("a disabled route with no recipient must be accepted: %v", err)
	}
	// DEFAULT false: an insert that never names enabled is disabled.
	var enabled bool
	if err := run(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, reason_code)
			VALUES ('platform', 'p2', 0, 'log', 'ops', 'initial_setup') RETURNING enabled`).Scan(&enabled)
	}); err != nil {
		t.Fatal(err)
	}
	if enabled {
		t.Fatal("enabled must default to false (fail closed)")
	}
}

func TestMigration0117_ReasonCode_RequiredAndClosedVocabulary(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_rc")
	admin := w.SeedAdmin()
	err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return insertRoute(ctx, tx, false, strp("ops"), nil)
	})
	if pgCode(err) != "AR002" {
		t.Fatalf("a missing reason_code must be refused (AR002), got %v", err)
	}
	err = w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return insertRoute(ctx, tx, false, strp("ops"), strp("because i said so"))
	})
	if pgCode(err) != "23514" {
		t.Fatalf("free text reason_code must violate the vocabulary CHECK, got %v", err)
	}
}

func TestMigration0117_EnabledAndReasonAreImmutable_OnlySupersessionMayUpdate(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_imm")
	admin := w.SeedAdmin()
	id := w.AddRoute(admin, alerting.SeverityP2, 0, alerting.ChannelMock, "ops", false)
	err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alert_routes SET enabled = true WHERE id = $1`, id)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "only superseded_at/superseded_by may change") {
		t.Fatalf("flipping enabled in place must be refused, got %v", err)
	}
	err = w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alert_routes SET reason_code = 'correction' WHERE id = $1`, id)
		return err
	})
	if err == nil {
		t.Fatal("rewriting reason_code in place must be refused")
	}
}

// Security H-2 structural guard: no route can be enabled for a human-notification kind.
// No such kind exists (channel_kind is limited to log/mock), so the guard is exercised by
// lifting the kind CHECK in a rolled-back owner transaction.
func TestMigration0117_NoRouteCanBeEnabledForAHumanNotificationKind(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_sr7")
	admin := w.SeedAdmin()

	for kind, want := range map[string]bool{"log": false, "mock": false, "pager_x": true, "": true} {
		var got bool
		if err := w.Pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT alerting_channel_kind_is_human_notification($1)`, kind).Scan(&got)
		}); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("alerting_channel_kind_is_human_notification(%q) = %v, want %v", kind, got, want)
		}
	}

	ctx := context.Background()
	tx, err := w.Owner.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, admin.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `ALTER TABLE alert_routes DROP CONSTRAINT alert_routes_channel_kind_check`); err != nil {
		t.Fatalf("lift the kind CHECK (rolled back): %v", err)
	}
	ins := func(enabled bool) error {
		if _, err := tx.Exec(ctx, `SAVEPOINT s`); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code)
			VALUES ('platform', 'p1', 0, 'pager_x', 'rota', $1, 'initial_setup')`, enabled)
		if err != nil {
			_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT s`)
		}
		return err
	}
	err = ins(true)
	if pgCode(err) != "AR001" || !strings.Contains(err.Error(), "SR-7: four-eyes approval required; not built") {
		t.Fatalf("enabling a route for a human-notification kind must be refused (AR001), got %v", err)
	}
	if err := ins(false); err != nil {
		t.Fatalf("a DISABLED route for a human kind may still be staged: %v", err)
	}
}

func TestMigration0117_UnroutedReason_ConstraintsAreStructural(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_ur")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
	disp := func(f func(ctx context.Context, tx pgx.Tx) error) error {
		return w.Pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, f)
	}
	cases := []struct {
		name string
		sql  string
		args []any
	}{
		{"unrouted without a reason", `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event) VALUES ($1, 0, 0, 'unrouted')`, []any{id}},
		{"reason outside the vocabulary", `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, unrouted_reason) VALUES ($1, 0, 0, 'unrouted', 'whatever')`, []any{id}},
		{"reason on a non-unrouted row", `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, unrouted_reason) VALUES ($1, 0, 0, 'claimed', 'no_route')`, []any{id}},
	}
	for _, c := range cases {
		err := disp(func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, c.sql, c.args...); return err })
		if pgCode(err) != "23514" {
			t.Errorf("%s: want a CHECK violation, got %v", c.name, err)
		}
	}
	for _, reason := range []string{"no_route", "channel_disabled", "no_sink"} {
		a := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
		err := disp(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, unrouted_reason) VALUES ($1, 0, 0, 'unrouted', $2)`, a, reason)
			return err
		})
		if err != nil {
			t.Errorf("reason %s must be accepted: %v", reason, err)
		}
	}
}

// RLS / tenant isolation: routing configuration is platform-scope only.
func TestMigration0117_RLS_TenantsAndUnvalidatedSessionsSeeNoRoutingConfiguration(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_rls")
	admin := w.SeedAdmin()
	tenantA := w.SeedTenant(admin)
	tenantB := w.SeedTenant(admin)
	w.AddRoute(admin, alerting.SeverityP1, 0, alerting.ChannelMock, "ops", true)
	w.SeedAlert(tenantA, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())

	count := func(sql string, run func(ctx context.Context, f func(ctx context.Context, tx pgx.Tx) error) error) int {
		var n int
		if err := run(context.Background(), func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql).Scan(&n) }); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	tenantRun := func(id uuid.UUID) func(context.Context, func(context.Context, pgx.Tx) error) error {
		return func(ctx context.Context, f func(context.Context, pgx.Tx) error) error {
			return w.Pool.WithTenant(ctx, id, f)
		}
	}
	for _, id := range []uuid.UUID{tenantA, tenantB} {
		if n := count(`SELECT count(*) FROM alert_routes`, tenantRun(id)); n != 0 {
			t.Fatalf("tenant %s sees %d alert_routes rows, want 0", id, n)
		}
		if n := count(`SELECT count(*) FROM alert_deliveries`, tenantRun(id)); n != 0 {
			t.Fatalf("tenant %s sees %d alert_deliveries rows, want 0", id, n)
		}
		// A tenant cannot write a route (RLS WITH CHECK).
		err := w.Pool.WithTenant(context.Background(), id, func(ctx context.Context, tx pgx.Tx) error {
			return insertRoute(ctx, tx, false, strp("x"), strp("initial_setup"))
		})
		if err == nil {
			t.Fatal("a tenant session must not be able to insert a route")
		}
	}
	// A platform GUC that is not a real platform-scoped staff principal sees nothing and writes nothing.
	ghost := uuid.New()
	if n := count(`SELECT count(*) FROM alert_routes`, func(ctx context.Context, f func(context.Context, pgx.Tx) error) error {
		return w.Pool.WithPlatformAdmin(ctx, ghost, f)
	}); n != 0 {
		t.Fatalf("an unvalidated platform principal sees %d routes, want 0", n)
	}
	// The validated platform admin sees the route; the dispatcher identity reads but cannot write routes.
	if n := count(`SELECT count(*) FROM alert_routes`, func(ctx context.Context, f func(context.Context, pgx.Tx) error) error {
		return w.Pool.WithPlatformAdmin(ctx, admin, f)
	}); n != 1 {
		t.Fatalf("platform admin sees %d routes, want 1", n)
	}
	if n := count(`SELECT count(*) FROM alert_routes`, func(ctx context.Context, f func(context.Context, pgx.Tx) error) error {
		return w.Pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, f)
	}); n != 1 {
		t.Fatalf("dispatcher sees %d routes, want 1", n)
	}
	err := w.Pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return insertRoute(ctx, tx, false, strp("x"), strp("initial_setup"))
	})
	if err == nil {
		t.Fatal("the dispatcher identity must not be able to write routes")
	}
}

var expectedPinnedAlertFunctions = []string{
	"alert_deliveries_guard", "alert_kinds_deny_write", "alert_occurrences_guard", "alert_routes_guard",
	"alerting_attributes_are_flat_scalars", "alerting_channel_kind_is_human_notification",
	"alerting_session_scope", "alerting_validated_platform_admin", "alerts_guard",
}

// Security H-1 catalogue: EXACTLY these alert functions exist and every one pins search_path with pg_temp last.
func TestMigration0117_EveryAlertFunctionPinsSearchPath_ExactCatalogue(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_cat")
	got := map[string]string{}
	if err := w.Pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT proname, coalesce(array_to_string(proconfig, ','), '') FROM pg_proc
			WHERE pronamespace = 'public'::regnamespace AND (proname LIKE 'alert%')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n, c string
			if err := rows.Scan(&n, &c); err != nil {
				return err
			}
			got[n] = c
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	var names []string
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(expectedPinnedAlertFunctions, ",") {
		t.Fatalf("alert function set changed:\n got %v\nwant %v\n(update the pin list consciously)", names, expectedPinnedAlertFunctions)
	}
	for n, c := range got {
		if !strings.Contains(c, "search_path=pg_catalog, public, pg_temp") {
			t.Errorf("%s: proconfig = %q, want the pinned search_path with pg_temp last", n, c)
		}
	}
}

// TEMP-shadow probes. After migration 0116 revokes TEMP creation from the runtime role these run through the
// OWNER pool in a rolled-back transaction. Each probe has a control: with the pin lifted (also rolled back)
// the shadow WORKS, proving the probe has teeth.
func TestMigration0117_TempShadowProbe_StaffUsersCannotForgeAPlatformAdmin(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_tmp1")
	fake := uuid.New()
	probe := func(lift bool) (uuid.UUID, error) {
		ctx := context.Background()
		tx, err := w.Owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if lift {
			if _, err := tx.Exec(ctx, `ALTER FUNCTION alerting_validated_platform_admin() RESET search_path`); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range []string{
			`CREATE TEMP TABLE staff_users (id uuid, tenant_id uuid)`,
			`INSERT INTO pg_temp.staff_users VALUES ('` + fake.String() + `', NULL)`,
			`SELECT set_config('app.platform_admin_principal_id', '` + fake.String() + `', true)`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		var got uuid.UUID
		err = tx.QueryRow(ctx, `SELECT alerting_validated_platform_admin()`).Scan(&got)
		return got, err
	}
	if got, err := probe(true); err != nil || got != fake {
		t.Fatalf("control: with the pin lifted the temp shadow must forge the principal (got %v, %v); the probe has no teeth", got, err)
	}
	if got, err := probe(false); err == nil {
		t.Fatalf("the pinned function must not be fooled by a temp staff_users table (returned %v)", got)
	}
}

func TestMigration0117_TempShadowProbe_AlertsCannotHideASimulationAlertFromTheDeliveryGuard(t *testing.T) {
	w := alertworld.NewWorld(t, "m117_tmp2")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	sim := w.SeedAlert(tenant, alerting.KindSimulationCasinoPlay, "d:"+uuid.NewString())
	probe := func(lift bool) error {
		ctx := context.Background()
		tx, err := w.Owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if lift {
			if _, err := tx.Exec(ctx, `ALTER FUNCTION alert_deliveries_guard() RESET search_path`); err != nil {
				t.Fatal(err)
			}
		}
		for _, s := range []string{
			`CREATE TEMP TABLE alerts (id uuid, tenant_id uuid, subject_tenant_id uuid, simulation boolean)`,
			`INSERT INTO pg_temp.alerts VALUES ('` + sim.String() + `', NULL, '` + tenant.String() + `', false)`,
			`SELECT set_config('app.platform_service_id', 'alert_dispatcher', true)`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				t.Fatalf("%s: %v", s, err)
			}
		}
		_, err = tx.Exec(ctx, `INSERT INTO public.alert_deliveries (alert_id, escalation_step, attempt_no, event) VALUES ($1, 0, 0, 'claimed')`, sim)
		return err
	}
	if err := probe(true); err != nil {
		t.Fatalf("control: with the pin lifted the temp alerts shadow must let a simulation alert be delivered; the probe has no teeth: %v", err)
	}
	err := probe(false)
	if err == nil || !strings.Contains(err.Error(), "suppressed_simulation") {
		t.Fatalf("AL-9 must still refuse delivery of a simulation alert despite the temp shadow, got %v", err)
	}
}

// Down/up on a throwaway database migrated only through 0117.
const migration0117Version = int64(117)

func scratchThrough0117(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	src := "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n, perr := strconv.ParseInt(e.Name()[:4], 10, 64)
		if perr != nil || n > migration0117Version {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(src, e.Name()))
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Connect(context.Background(), scratchdb.New(t, prefix), 10, 5_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate through 0117: %v", err)
	}
	if applied[len(applied)-1] != migration0117Version {
		t.Fatalf("last applied %v", applied)
	}
	return pool, dir
}

func ownerAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`, id, id.String()+"@p.test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMigration0117_DownRefusesWithAnyRouteRow(t *testing.T) {
	pool, dir := scratchThrough0117(t, "m117_dn1")
	admin := ownerAdmin(t, pool)
	// a DISABLED route is still a refusal reason: after the down it would become live.
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return insertRoute(ctx, tx, false, strp("ops"), strp("initial_setup"))
	}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(context.Background(), dir, 1)
	if err == nil || !strings.Contains(err.Error(), "AR099") && !strings.Contains(err.Error(), "0117 down refused") {
		t.Fatalf("down must refuse with any alert_routes row, got %v", err)
	}
	// still at 0117 with the new columns
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_routes WHERE enabled = false`).Scan(&n)
	}); err != nil {
		t.Fatalf("a refused down must leave the schema intact: %v", err)
	}
}

func TestMigration0117_DownRefusesWithAnUnroutedReason(t *testing.T) {
	pool, dir := scratchThrough0117(t, "m117_dn2")
	admin := ownerAdmin(t, pool)
	var tenant, alert uuid.UUID
	tenant = uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'm117', $2, 'own_licence')`, tenant, "m117-"+tenant.String()[:8])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, 'payment.webhook_integrity', 'd:x', '{}'::jsonb) RETURNING id`, tenant).Scan(&alert)
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, unrouted_reason) VALUES ($1, 0, 0, 'unrouted', 'no_route')`, alert)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err == nil {
		t.Fatal("down must refuse while a delivery row carries an unrouted_reason")
	}
}

func TestMigration0117_UpDownUp_AndVerify(t *testing.T) {
	pool, dir := scratchThrough0117(t, "m117_ud")
	ctx := context.Background()
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down on an empty database: %v", err)
	}
	// After the down: the 0110 shape is back (no enabled column, recipient_ref NOT NULL) and the pins remain.
	var hasEnabled bool
	var nullable string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'alert_routes' AND column_name = 'enabled')`).Scan(&hasEnabled); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT is_nullable FROM information_schema.columns WHERE table_name = 'alert_routes' AND column_name = 'recipient_ref'`).Scan(&nullable)
	}); err != nil {
		t.Fatal(err)
	}
	if hasEnabled || nullable != "NO" {
		t.Fatalf("down did not restore the 0110 shape: enabled=%v recipient_ref nullable=%s", hasEnabled, nullable)
	}
	applied, err := pool.MigrateUp(ctx, dir)
	if err != nil || len(applied) != 1 || applied[0] != migration0117Version {
		t.Fatalf("re-up: %v %v", applied, err)
	}
	report, err := pool.VerifyMigrations(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range report.Results {
		if res.Status == db.MigrationCheckMismatch || res.Status == db.MigrationCheckMissingFile {
			t.Fatalf("checksum drift after the round trip: %+v", res)
		}
	}
	// 0116 (REVOKE TEMP) lives on another branch: a gap exactly at 0116 is tolerated until it merges.
	for _, g := range report.VersionGaps {
		if !strings.Contains(g, "116") {
			t.Fatalf("unexpected migration gap %v", report.VersionGaps)
		}
	}
}
