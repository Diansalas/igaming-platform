//go:build integration

package alerting_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertworld"
)

// Migration 0157 (ALERT-RETENTION-1): the sanctioned purge of resolved alert history.

func resolveAlert(t *testing.T, w *alertworld.World, admin, id uuid.UUID) {
	t.Helper()
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alerts SET state = 'resolved', resolve_reason_code = 'retention_test' WHERE id = $1`, id)
		return err
	}); err != nil {
		t.Fatalf("resolve alert: %v", err)
	}
}

// backdateResolved rewrites resolved_at of an already-resolved alert. A resolved alert is terminal and
// immutable by trigger, so the OWNER does this in a scratch database with the trigger and FORCE RLS lifted for
// the duration of one committed transaction. It is test set-up only.
func backdateResolved(t *testing.T, w *alertworld.World, id uuid.UUID, age time.Duration) {
	t.Helper()
	ctx := context.Background()
	tx, err := w.Owner.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		`ALTER TABLE alerts DISABLE TRIGGER alerts_guard`,
		`ALTER TABLE alerts NO FORCE ROW LEVEL SECURITY`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE alerts SET resolved_at = now() - make_interval(secs => $2) WHERE id = $1`, id, age.Seconds()); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`ALTER TABLE alerts FORCE ROW LEVEL SECURITY`,
		`ALTER TABLE alerts ENABLE TRIGGER alerts_guard`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// seedHistory raises an alert with n extra occurrences and one delivery row, resolves it and ages it.
func seedHistory(t *testing.T, w *alertworld.World, admin, tenant uuid.UUID, extraOccurrences int, resolve bool, age time.Duration) uuid.UUID {
	t.Helper()
	id := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, "d:"+uuid.NewString())
	if err := w.Pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < extraOccurrences+1; i++ {
			if _, err := tx.Exec(ctx, `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, id); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.Pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event, unrouted_reason) VALUES ($1, 0, 0, 'unrouted', 'no_route')`, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if resolve {
		resolveAlert(t, w, admin, id)
		backdateResolved(t, w, id, age)
	}
	return id
}

func purge(w *alertworld.World, older time.Duration, batch int) (a, o, d int64, capped bool, err error) {
	err = w.Pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT alerts_deleted, occurrences_deleted, deliveries_deleted, capped FROM alerting_purge_resolved(make_interval(secs => $1), $2)`,
			older.Seconds(), batch).Scan(&a, &o, &d, &capped)
	})
	return
}

func count(t *testing.T, w *alertworld.World, admin uuid.UUID, table string) int {
	t.Helper()
	var n int
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&n)
	}); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

const day = 24 * time.Hour

func TestMigration0157_PurgeRemovesOnlyOldResolvedHistory(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_p")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)

	oldResolved := seedHistory(t, w, admin, tenant, 2, true, 400*day)   // 3 occurrences, 1 delivery
	recentResolved := seedHistory(t, w, admin, tenant, 0, true, 10*day) // resolved, inside the 90-day window
	openOld := seedHistory(t, w, admin, tenant, 1, false, 0)            // open: never pruned
	acked := seedHistory(t, w, admin, tenant, 0, false, 0)              // acked: never pruned
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alerts SET state = 'acked' WHERE id = $1`, acked)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	a, o, d, capped, err := purge(w, 90*day, 100)
	if err != nil {
		t.Fatal(err)
	}
	if a != 1 || o != 3 || d != 1 || capped {
		t.Fatalf("purge = alerts %d occurrences %d deliveries %d capped %v, want 1/3/1/false", a, o, d, capped)
	}
	remaining := map[uuid.UUID]bool{}
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM alerts`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			remaining[id] = true
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if remaining[oldResolved] || !remaining[recentResolved] || !remaining[openOld] || !remaining[acked] {
		t.Fatalf("wrong survivors: old resolved present=%v recent resolved=%v open=%v acked=%v",
			remaining[oldResolved], remaining[recentResolved], remaining[openOld], remaining[acked])
	}
	// children of the survivors are intact (open alert kept its 2 occurrences)
	if n := count(t, w, admin, `alert_occurrences WHERE alert_id = '`+openOld.String()+`'`); n != 2 {
		t.Fatalf("open alert occurrences = %d, want 2", n)
	}

	// the run is audited, counts only
	var ra, ro, rd int64
	var older time.Duration
	var cappedRow bool
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT alerts_deleted, occurrences_deleted, deliveries_deleted, capped, extract(epoch FROM older_than)::bigint * 1000000000 FROM alert_retention_runs`).
			Scan(&ra, &ro, &rd, &cappedRow, (*int64)(&older))
	}); err != nil {
		t.Fatalf("retention audit row: %v", err)
	}
	if ra != 1 || ro != 3 || rd != 1 || cappedRow || older != 90*day {
		t.Fatalf("audit row = %d/%d/%d capped=%v older=%v", ra, ro, rd, cappedRow, older)
	}

	// a second run has nothing to do and is still audited
	if a, o, d, _, err := purge(w, 90*day, 100); err != nil || a+o+d != 0 {
		t.Fatalf("second purge = %d/%d/%d err %v", a, o, d, err)
	}
	if n := count(t, w, admin, `alert_retention_runs`); n != 2 {
		t.Fatalf("runs = %d, want 2", n)
	}
}

func TestMigration0157_BatchCapIsReportedAndDrainsAcrossRuns(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_b")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	for i := 0; i < 5; i++ {
		seedHistory(t, w, admin, tenant, 0, true, 400*day)
	}
	a, _, _, capped, err := purge(w, 90*day, 3)
	if err != nil || a != 3 || !capped {
		t.Fatalf("first run alerts=%d capped=%v err=%v, want 3/true", a, capped, err)
	}
	a, _, _, capped, err = purge(w, 90*day, 3)
	if err != nil || a != 2 || capped {
		t.Fatalf("second run alerts=%d capped=%v err=%v, want 2/false", a, capped, err)
	}
}

func TestMigration0157_FunctionRefusesEverySessionButTheDispatcherIdentity(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_auth")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	call := func(ctx context.Context, tx pgx.Tx) error {
		var a, o, d int64
		var c bool
		return tx.QueryRow(ctx, `SELECT alerts_deleted, occurrences_deleted, deliveries_deleted, capped FROM alerting_purge_resolved(INTERVAL '90 days', 10)`).Scan(&a, &o, &d, &c)
	}
	if err := w.Pool.WithTenant(context.Background(), tenant, call); pgCode(err) != "AR202" {
		t.Errorf("tenant session: want AR202, got %v", err)
	}
	if err := w.Pool.WithPlatformAdmin(context.Background(), admin, call); pgCode(err) != "AR202" {
		t.Errorf("platform admin session: want AR202, got %v", err)
	}
	if err := w.Pool.WithoutTenant(context.Background(), call); err == nil {
		t.Error("a session with no scope must be refused")
	}
	for _, c := range []struct {
		name string
		sql  string
	}{
		{"window below the floor", `SELECT * FROM alerting_purge_resolved(INTERVAL '6 days', 10)`},
		{"null window", `SELECT * FROM alerting_purge_resolved(NULL, 10)`},
		{"zero batch", `SELECT * FROM alerting_purge_resolved(INTERVAL '90 days', 0)`},
		{"huge batch", `SELECT * FROM alerting_purge_resolved(INTERVAL '90 days', 50001)`},
	} {
		err := w.Pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, c.sql)
			if err != nil {
				return err
			}
			rows.Close()
			return rows.Err()
		})
		if pgCode(err) != "AR203" {
			t.Errorf("%s: want AR203, got %v", c.name, err)
		}
	}
	if n := count(t, w, admin, `alert_retention_runs`); n != 0 {
		t.Errorf("a refused call must not write an audit row, found %d", n)
	}
}

// The runtime role has no DELETE privilege on any alert table, and setting the flag itself changes nothing.
func TestMigration0157_RuntimeRoleCannotDeleteDirectly_FlagOrNot(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_dd")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	id := seedHistory(t, w, admin, tenant, 1, true, 400*day)
	for _, table := range []string{"alert_deliveries", "alert_occurrences", "alerts", "alert_kinds", "alert_routes"} {
		for _, flag := range []bool{false, true} {
			err := w.Pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
				if flag {
					if _, err := tx.Exec(ctx, `SELECT set_config('app.alert_retention_purge', 'on', true)`); err != nil {
						return err
					}
				}
				_, err := tx.Exec(ctx, `DELETE FROM `+table)
				return err
			})
			if err == nil || !strings.Contains(err.Error(), "permission denied") {
				t.Errorf("runtime DELETE FROM %s (flag=%v): want permission denied, got %v", table, flag, err)
			}
		}
	}
	// Direct purge-function bypass attempts through TRUNCATE are still refused for the owner as well.
	err := w.Owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE alerts CASCADE`)
		return err
	})
	if err == nil {
		t.Error("TRUNCATE alerts must remain refused")
	}
	if count(t, w, admin, `alerts WHERE id = '`+id.String()+`'`) != 1 {
		t.Error("the history disappeared without the purge function")
	}
}

// Even the table OWNER cannot delete without going through the sanctioned path: no flag means RLS hides every
// row from DELETE; the flag alone still leaves the row-eligibility trigger.
func TestMigration0157_OwnerDeleteNeedsTheFlagAndAnEligibleRow(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_own")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	recent := seedHistory(t, w, admin, tenant, 0, true, 1*day)
	old := seedHistory(t, w, admin, tenant, 0, true, 400*day)
	open := seedHistory(t, w, admin, tenant, 0, false, 0)
	ctx := context.Background()

	exec := func(flag bool, sql string, args ...any) (int64, error) {
		tx, err := w.Owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if flag {
			// The DELETE policies need BOTH the dispatcher identity and the purge flag; the trigger then adds the owner
			// check and the row-eligibility check, which is what these cases exercise.
			for _, g := range []string{`SELECT set_config('app.platform_service_id', 'alert_dispatcher', true)`, `SELECT set_config('app.alert_retention_purge', 'on', true)`} {
				if _, err := tx.Exec(ctx, g); err != nil {
					t.Fatal(err)
				}
			}
		}
		tag, err := tx.Exec(ctx, sql, args...)
		return tag.RowsAffected(), err
	}
	// no flag: RLS filters every row out of the DELETE (0 rows, no error) - nothing is deleted
	if n, err := exec(false, `DELETE FROM alerts WHERE id = $1`, old); err != nil || n != 0 {
		t.Errorf("owner DELETE without the flag: rows %d err %v, want 0 rows", n, err)
	}
	// the flag alone (no dispatcher identity) is not enough either: RLS still hides every row
	func() {
		tx, err := w.Owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('app.alert_retention_purge', 'on', true)`); err != nil {
			t.Fatal(err)
		}
		tag, err := tx.Exec(ctx, `DELETE FROM alerts WHERE id = $1`, old)
		if err != nil || tag.RowsAffected() != 0 {
			t.Errorf("owner DELETE with the flag but no dispatcher identity: rows %d err %v, want 0 rows", tag.RowsAffected(), err)
		}
	}()
	// flag, recent resolved alert: the eligibility trigger refuses
	if _, err := exec(true, `DELETE FROM alert_deliveries WHERE alert_id = $1`, recent); pgCode(err) != "AR201" {
		t.Errorf("owner+flag DELETE of a recent resolved alert's deliveries: want AR201, got %v", err)
	}
	// flag, recent resolved alert: the DELETE policy admits the row (resolved) and the trigger's own 7-day floor refuses it
	if _, err := exec(true, `DELETE FROM alerts WHERE id = $1`, recent); pgCode(err) != "AR201" {
		t.Errorf("owner+flag DELETE of a recent resolved ALERT row: want AR201 (7-day floor), got %v", err)
	}
	// flag, open alert: the DELETE policy (state = resolved) hides the alert row; its children are refused by the trigger
	if n, err := exec(true, `DELETE FROM alerts WHERE id = $1`, open); err != nil || n != 0 {
		t.Errorf("owner+flag DELETE of an open alert: rows %d err %v, want 0 rows", n, err)
	}
	if _, err := exec(true, `DELETE FROM alert_occurrences WHERE alert_id = $1`, open); pgCode(err) != "AR201" {
		t.Errorf("owner+flag DELETE of an open alert's occurrences: want AR201, got %v", err)
	}
}

func TestMigration0157_RetentionRunsIsAppendOnlyAndAdminReadable(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_runs")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	if _, _, _, _, err := purge(w, 90*day, 10); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE alert_retention_runs SET alerts_deleted = 0`,
		`DELETE FROM alert_retention_runs`,
		`TRUNCATE alert_retention_runs`,
		`INSERT INTO alert_retention_runs (older_than, cutoff, batch_limit, alerts_deleted, occurrences_deleted, deliveries_deleted, capped) VALUES (INTERVAL '8 days', now(), 1, 0, 0, 0, false)`,
	} {
		err := w.Pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
		if err == nil {
			t.Errorf("runtime admin session executed %q on the retention audit", sql)
		}
	}
	if n := count(t, w, admin, `alert_retention_runs`); n != 1 {
		t.Fatalf("admin sees %d runs, want 1", n)
	}
	var n int
	if err := w.Pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_retention_runs`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a tenant session sees %d retention runs (err %v), want 0", n, err)
	}
}

// The runtime role never reaches the audit table's trigger (no UPDATE/DELETE privilege, no policy); the trigger is the
// last line for anyone who does (the owner). Prove it with RLS lifted in a rolled-back owner transaction.
func TestMigration0157_RetentionRunsTriggerRefusesEvenTheOwnerWithRLSLifted(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_trg")
	if _, _, _, _, err := purge(w, 90*day, 10); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, sql := range []string{`UPDATE alert_retention_runs SET alerts_deleted = 99`, `DELETE FROM alert_retention_runs`, `TRUNCATE alert_retention_runs`} {
		tx, err := w.Owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE alert_retention_runs NO FORCE ROW LEVEL SECURITY`); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, sql)
		_ = tx.Rollback(ctx)
		if err == nil || !strings.Contains(err.Error(), "append-only") {
			t.Errorf("%q: want the append-only refusal, got %v", sql, err)
		}
	}
}

func TestMigration0157_ResolvedDedupKeyOpensAFreshAlertAfterThePurge(t *testing.T) {
	w := alertworld.NewWorld(t, "m157_dd2")
	admin := w.SeedAdmin()
	tenant := w.SeedTenant(admin)
	disc := "d:" + uuid.NewString()
	first := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, disc)
	resolveAlert(t, w, admin, first)
	backdateResolved(t, w, first, 400*day)
	if a, _, _, _, err := purge(w, 90*day, 10); err != nil || a != 1 {
		t.Fatalf("purge alerts=%d err=%v", a, err)
	}
	second := w.SeedAlert(tenant, alerting.KindPaymentWebhookIntegrity, disc)
	if second == first {
		t.Fatal("expected a new alert id")
	}
}

func TestMigration0157_DownRefusesWhileAuditRowsExistAndRoundTripsWhenEmpty(t *testing.T) {
	pool, dir := scratchThroughVersion(t, "m157_dn", 157)
	ctx := context.Background()
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("down on an empty database: %v", err)
	}
	// the 0110 DELETE refusal is back on the original triggers
	admin := ownerAdmin(t, pool)
	_ = admin
	applied, err := pool.MigrateUp(ctx, dir)
	if err != nil || len(applied) != 1 || applied[0] != 157 {
		t.Fatalf("re-up: %v %v", applied, err)
	}
	if err := pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM alerting_purge_resolved(INTERVAL '90 days', 10)`)
		return err
	}); err != nil {
		t.Fatalf("purge on the owner pool: %v", err)
	}
	if _, err := pool.MigrateDown(ctx, dir, 1); err == nil || !strings.Contains(err.Error(), "0157 down refused") {
		t.Fatalf("down must refuse with an audit row present, got %v", err)
	}
}
