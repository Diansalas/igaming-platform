//go:build integration

package alerting_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertworld"
)

// Migration 0156 (A9): email/webhook channel kinds, SR-7 four-eyes staging, Kind reconciliation.sweep_failed.

// insertRouteFull is the STAGING path as the API drives it: a disabled email/webhook version is inserted with
// staged_for_enable = true (SR9-A9-3), so it can later be approved. Every other row has the flag false.
func insertRouteFull(ctx context.Context, tx pgx.Tx, sev string, step int, kind string, recipient string, enabled bool, staged *uuid.UUID) (uuid.UUID, error) {
	return insertRouteRaw(ctx, tx, sev, step, kind, recipient, enabled, staged, !enabled && (kind == "email" || kind == "webhook"), "initial_setup")
}

func insertRouteRaw(ctx context.Context, tx pgx.Tx, sev string, step int, kind string, recipient string, enabled bool, staged *uuid.UUID, stageForEnable bool, reason string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code, staged_route_id, staged_for_enable)
		VALUES ('platform', $1, $2, $3, $4, $5, $7, $6, $8) RETURNING id`, sev, step, kind, recipient, enabled, staged, reason, stageForEnable).Scan(&id)
	return id, err
}

func TestMigration0156_EmailAndWebhookRoutesMayBeStagedDisabledButNotEnabledAlone(t *testing.T) {
	w := alertworld.NewWorld(t, "m156_chan")
	adminA := w.SeedAdmin()
	for _, kind := range []string{"email", "webhook"} {
		// disabled staging works for both new kinds
		if err := w.Pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := insertRouteFull(ctx, tx, "p2", 0, kind, "interim-owner", false, nil)
			return err
		}); err != nil {
			t.Fatalf("%s: disabled staged route: %v", kind, err)
		}
		// enabling alone is refused with the SR-7 code, exactly as 0117 did for any human kind
		err := w.Pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := insertRouteFull(ctx, tx, "p1", 1, kind, "interim-owner", true, nil)
			return err
		})
		if pgCode(err) != "AR001" || !strings.Contains(err.Error(), "SR-7: four-eyes approval required") ||
			!strings.Contains(err.Error(), "stage the route disabled") {
			t.Fatalf("%s: enabling without staging must be refused (AR001, with the staging instruction), got %v", kind, err)
		}
	}
	// A kind outside the vocabulary is still refused by the CHECK.
	for _, bad := range []string{"pagerduty", "sms", "slack", "Email", "email "} {
		err := w.Pool.WithPlatformAdmin(context.Background(), adminA, func(ctx context.Context, tx pgx.Tx) error {
			_, err := insertRouteFull(ctx, tx, "p3", 0, bad, "x", false, nil)
			return err
		})
		if pgCode(err) != "23514" {
			t.Fatalf("channel kind %q must violate the CHECK, got %v", bad, err)
		}
	}
}

func TestMigration0156_FourEyes_OneAdminStagesAnotherEnables(t *testing.T) {
	w := alertworld.NewWorld(t, "m156_4e")
	a, b := w.SeedAdmin(), w.SeedAdmin()
	ctx := context.Background()
	as := func(admin uuid.UUID, f func(ctx context.Context, tx pgx.Tx) error) error {
		return w.Pool.WithPlatformAdmin(ctx, admin, f)
	}
	var staged uuid.UUID
	if err := as(a, func(ctx context.Context, tx pgx.Tx) (err error) {
		staged, err = insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", false, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	refused := func(name string, admin uuid.UUID, sev string, step int, kind, recipient string, st *uuid.UUID) {
		t.Helper()
		err := as(admin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := insertRouteFull(ctx, tx, sev, step, kind, recipient, true, st)
			return err
		})
		if pgCode(err) != "AR001" {
			t.Errorf("%s: want AR001, got %v", name, err)
		}
	}
	refused("same admin approves own staging", a, "p1", 0, "email", "interim-owner", &staged)
	refused("recipient differs from the staged one", b, "p1", 0, "email", "someone-else", &staged)
	refused("severity differs", b, "p2", 0, "email", "interim-owner", &staged)
	refused("step differs", b, "p1", 1, "email", "interim-owner", &staged)
	refused("channel differs", b, "p1", 0, "webhook", "interim-owner", &staged)
	missing := uuid.New()
	refused("staged route does not exist", b, "p1", 0, "email", "interim-owner", &missing)

	// A staged_route_id is meaningless on a non-human or disabled version: fail closed.
	err := as(b, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertRouteFull(ctx, tx, "p1", 0, "log", "ops", true, &staged)
		return err
	})
	if pgCode(err) != "AR001" {
		t.Errorf("staged_route_id on a non-human enable must be refused, got %v", err)
	}
	err = as(b, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", false, &staged)
		return err
	})
	if pgCode(err) != "AR001" {
		t.Errorf("staged_route_id on a disabled version must be refused, got %v", err)
	}

	// The second admin approves the identical staged version.
	var enabled uuid.UUID
	if err := as(b, func(ctx context.Context, tx pgx.Tx) (err error) {
		enabled, err = insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", true, &staged)
		return err
	}); err != nil {
		t.Fatalf("approval by a different admin: %v", err)
	}
	var by uuid.UUID
	var ref *uuid.UUID
	if err := as(b, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT created_by, staged_route_id FROM alert_routes WHERE id = $1`, enabled).Scan(&by, &ref)
	}); err != nil {
		t.Fatal(err)
	}
	if by != b || ref == nil || *ref != staged {
		t.Fatalf("enabled route created_by=%v staged=%v, want %v / %v", by, ref, b, staged)
	}

	// Each staged route is consumed once (another admin, same staging).
	c := w.SeedAdmin()
	err = as(c, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", true, &staged)
		return err
	})
	if pgCode(err) != "23505" {
		t.Errorf("a staged route must be consumable once (unique), got %v", err)
	}

	// An ENABLED route cannot itself serve as a staging record.
	err = as(c, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", true, &enabled)
		return err
	})
	if pgCode(err) != "AR001" {
		t.Errorf("an enabled route as staging record must be refused, got %v", err)
	}

	// staged_route_id is immutable.
	err = as(b, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alert_routes SET staged_route_id = NULL WHERE id = $1`, enabled)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "only superseded_at/superseded_by may change") {
		t.Errorf("staged_route_id must be immutable, got %v", err)
	}
}

func TestMigration0156_NonHumanRoutesAreUnaffectedByFourEyes(t *testing.T) {
	w := alertworld.NewWorld(t, "m156_nh")
	a := w.SeedAdmin()
	if id := w.AddRoute(a, alerting.SeverityP2, 0, alerting.ChannelMock, "ops", true); id == uuid.Nil {
		t.Fatal("enabled mock route must still be allowed")
	}
	if id := w.AddRoute(a, alerting.SeverityP3, 0, alerting.ChannelLog, "ops", true); id == uuid.Nil {
		t.Fatal("enabled log route must still be allowed")
	}
}

func TestMigration0156_DeliveryRowsMayNameTheNewChannels(t *testing.T) {
	w := alertworld.NewWorld(t, "m156_dl")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	for i, kind := range []string{"email", "webhook"} {
		id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
		if err := w.Pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, channel_kind) VALUES ($1, 0, 0, 'sent', $2)`, id, kind)
			return err
		}); err != nil {
			t.Fatalf("%d %s: %v", i, kind, err)
		}
	}
	id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
	err := w.Pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, channel_kind) VALUES ($1, 0, 0, 'sent', 'sms')`, id)
		return err
	})
	if pgCode(err) != "23514" {
		t.Fatalf("an unknown delivery channel must violate the CHECK, got %v", err)
	}
}

func TestMigration0156_SweepFailedKind_DispatcherIdentityOnly(t *testing.T) {
	w := alertworld.NewWorld(t, "m156_sf")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	ctx := context.Background()

	raise := func(f func(ctx context.Context, tx pgx.Tx) error) error {
		return w.Pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, f)
	}
	insert := func(ctx context.Context, tx pgx.Tx, attrs string) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes)
			VALUES (NULL, NULL, 'reconciliation.sweep_failed', 'phase:list_active', $1::jsonb) RETURNING id`, attrs).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, id)
		return err
	}
	if err := raise(func(ctx context.Context, tx pgx.Tx) error {
		return insert(ctx, tx, `{"phase":"list_active","sqlstate_class":"08"}`)
	}); err != nil {
		t.Fatalf("the dispatcher identity must be able to raise the Kind: %v", err)
	}
	// attribute allowlist still binds
	if err := raise(func(ctx context.Context, tx pgx.Tx) error { return insert(ctx, tx, `{"error":"boom"}`) }); err == nil {
		t.Fatal("an attribute outside the allowlist must be refused")
	}
	// the dispatcher is still restricted to the meta kinds plus exactly this one
	err := raise(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator) VALUES (NULL, NULL, 'staff.platform_break_glass_used', 'x')`)
		return err
	})
	if err == nil {
		t.Fatal("the dispatcher identity must not gain other Kinds")
	}
	// a tenant session cannot raise it (not tenant-raisable, no subject allowed)
	err = w.Pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator) VALUES (NULL, $1, 'reconciliation.sweep_failed', 'phase:list_active')`, tenant)
		return err
	})
	if err == nil {
		t.Fatal("a tenant session raised reconciliation.sweep_failed")
	}
	err = w.Pool.WithTenant(ctx, tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator) VALUES (NULL, NULL, 'reconciliation.sweep_failed', 'phase:list_active')`)
		return err
	})
	if err == nil {
		t.Fatal("a tenant session raised the subjectless Kind")
	}
	// severity is forced from alert_kinds
	var sev string
	if err := w.Pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT severity FROM alerts WHERE kind = 'reconciliation.sweep_failed'`).Scan(&sev)
	}); err != nil || sev != "p1" {
		t.Fatalf("severity = %q, %v; want p1", sev, err)
	}
}

func TestMigration0156_DownRefusesWithNewChannelDataAndRoundTrips(t *testing.T) {
	pool, dir := scratchThroughVersion(t, "m156_dn", 156)
	ctx := context.Background()
	// round trip on an empty database
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down on an empty database: %v", err)
	}
	var has bool
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'alert_routes' AND column_name = 'staged_route_id')`).Scan(&has)
	}); err != nil || has {
		t.Fatalf("down must remove staged_route_id (has=%v err=%v)", has, err)
	}
	applied, err := pool.MigrateUp(ctx, dir)
	if err != nil || len(applied) != 1 || applied[0] != 156 {
		t.Fatalf("re-up: %v %v", applied, err)
	}
	admin := ownerAdmin(t, pool)
	if err := pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertRouteFull(ctx, tx, "p2", 0, "webhook", "interim-owner", false, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = pool.MigrateDown(ctx, dir, 1)
	if err == nil || !strings.Contains(err.Error(), "0156 down refused") {
		t.Fatalf("down must refuse while a webhook route exists, got %v", err)
	}
}

// The staged version expires after 7 days. created_at is forced to now() by the guard, so the OWNER backdates it
// in a scratch database with the guard trigger and FORCE RLS lifted for one committed transaction (test set-up only).
func TestMigration0156_StagedRouteOlderThanSevenDaysCannotBeApproved(t *testing.T) {
	w := alertworld.NewWorld(t, "m156_old")
	a, b := w.SeedAdmin(), w.SeedAdmin()
	ctx := context.Background()
	var staged uuid.UUID
	if err := w.Pool.WithPlatformAdmin(ctx, a, func(ctx context.Context, tx pgx.Tx) (err error) {
		staged, err = insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", false, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	backdate := func(age string) {
		tx, err := w.Owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, stmt := range []string{`ALTER TABLE alert_routes DISABLE TRIGGER alert_routes_guard`, `ALTER TABLE alert_routes NO FORCE ROW LEVEL SECURITY`} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE alert_routes SET created_at = now() - $2::interval WHERE id = $1`, staged, age); err != nil {
			t.Fatal(err)
		}
		for _, stmt := range []string{`ALTER TABLE alert_routes FORCE ROW LEVEL SECURITY`, `ALTER TABLE alert_routes ENABLE TRIGGER alert_routes_guard`} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
	approve := func() error {
		return w.Pool.WithPlatformAdmin(ctx, b, func(ctx context.Context, tx pgx.Tx) error {
			_, err := insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", true, &staged)
			return err
		})
	}
	backdate("8 days")
	if err := approve(); pgCode(err) != "AR001" || !strings.Contains(err.Error(), "older than 7 days") {
		t.Fatalf("an 8-day-old staging must be refused (AR001, older than 7 days), got %v", err)
	}
	backdate("6 days")
	if err := approve(); err != nil {
		t.Fatalf("a 6-day-old staging must still be approvable: %v", err)
	}
}
