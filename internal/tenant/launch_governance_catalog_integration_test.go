//go:build integration

// ADR 0112 slice 1: catalog, grants, RLS-isolation, append-only and startup-refusal tests.
// See launch_governance_integration_test.go for the behavioural (guard) tests.
package tenant

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// LF5: the BEFORE-row trigger firing order is pinned in the catalog. PostgreSQL fires
// triggers of one event in name order, so the order is a property of the NAMES.
func TestLaunchGov_LF5_TriggerFiringOrder(t *testing.T) {
	owner := brandPinOwnerPool(t)
	type trg struct {
		table, name string
		onInsert    bool
		onUpdate    bool
		before      bool
	}
	var all []trg
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT c.relname, t.tgname,
			       (t.tgtype & 4) <> 0, (t.tgtype & 16) <> 0, (t.tgtype & 2) <> 0
			  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			   AND c.relname IN ('tenants', 'brands', 'launch_authorisation_requests', 'launch_authorisation_approvals', 'launch_status_transitions')
			   AND (t.tgtype & 1) <> 0`) // row-level only
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x trg
			if err := rows.Scan(&x.table, &x.name, &x.onInsert, &x.onUpdate, &x.before); err != nil {
				return err
			}
			all = append(all, x)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	order := func(table string, update bool) []string {
		var names []string
		for _, x := range all {
			if x.table == table && x.before && ((update && x.onUpdate) || (!update && x.onInsert)) {
				names = append(names, x.name)
			}
		}
		sort.Strings(names) // PostgreSQL fires same-event triggers in name order
		return names
	}
	index := func(names []string, n string) int {
		for i, x := range names {
			if x == n {
				return i
			}
		}
		return -1
	}
	// tenants: the closure gate (exclusive tenant lock, GP020) fires BEFORE the launch guard.
	tu := order("tenants", true)
	if g, l := index(tu, "tenants_status_change_gate"), index(tu, "zz_launch_status_governed"); g < 0 || l < 0 || g >= l {
		t.Fatalf("tenants BEFORE UPDATE order %v: tenants_status_change_gate must fire before zz_launch_status_governed", tu)
	}
	// brands: the launch guard is the (only) BEFORE UPDATE status trigger; since slice 2 (0129) it takes the
	// per-brand gameplay lock inside it (TestBrandGate_Catalog_0129 pins that).
	if bu := order("brands", true); index(bu, "zz_launch_status_governed") < 0 {
		t.Fatalf("brands BEFORE UPDATE order %v: zz_launch_status_governed missing", bu)
	}
	for _, tb := range []string{"tenants", "brands"} {
		if ins := order(tb, false); index(ins, "zz_launch_status_insert_guard") < 0 {
			t.Fatalf("%s BEFORE INSERT order %v: zz_launch_status_insert_guard missing", tb, ins)
		}
	}
	// The governed tables: whatever else fires, zz_actor_proof_guard (ADR 0110, slice 3) must stay LAST.
	for _, tb := range []string{"launch_authorisation_requests", "launch_authorisation_approvals", "launch_status_transitions"} {
		for _, update := range []bool{false, true} {
			names := order(tb, update)
			if len(names) == 0 {
				continue
			}
			if i := index(names, "zz_actor_proof_guard"); i >= 0 && i != len(names)-1 {
				t.Fatalf("%s: zz_actor_proof_guard must be the last BEFORE trigger, order %v", tb, names)
			}
			for _, n := range names {
				if strings.HasPrefix(n, "zz_") && n != "zz_actor_proof_guard" {
					t.Fatalf("%s: trigger %q would sort at or after zz_actor_proof_guard; guards on the governed tables are named launch_*", tb, n)
				}
			}
		}
	}
	// And the guards exist on the governed tables.
	if got := order("launch_authorisation_requests", false); index(got, "launch_requests_guard") < 0 {
		t.Fatalf("requests BEFORE INSERT order %v", got)
	}
}

// ADR 0108: every launch function pins search_path and none is SECURITY DEFINER.
func TestLaunchGov_Functions_PinSearchPath_NoSecurityDefiner(t *testing.T) {
	owner := brandPinOwnerPool(t)
	var bad []string
	var n int
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT p.proname, p.prosecdef, COALESCE(array_to_string(p.proconfig, ','), '')
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace AND p.proname LIKE 'launch\_%'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name, cfg string
			var secdef bool
			if err := rows.Scan(&name, &secdef, &cfg); err != nil {
				return err
			}
			n++
			if secdef || !strings.Contains(cfg, "search_path=pg_catalog, public, pg_temp") {
				bad = append(bad, name)
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if n < 12 {
		t.Fatalf("expected at least 12 launch_* functions, found %d (the pin would be vacuous)", n)
	}
	if len(bad) > 0 {
		t.Fatalf("functions that are SECURITY DEFINER or do not pin search_path: %v", bad)
	}
}

// S6: no new brands policy; A-18: the three new tables expose nothing to a scope-less session.
func TestLaunchGov_S6_NoNewBrandsPolicy_A18_NoNullArm(t *testing.T) {
	owner := brandPinOwnerPool(t)
	rt := brandPinRuntimePool(t)
	ctx := context.Background()
	var names []string
	if err := owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT policyname FROM pg_policies WHERE schemaname = 'public' AND tablename = 'brands' ORDER BY 1`)
		if err != nil {
			return err
		}
		names, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// acting_lock is migration 0113's pre-existing restrictive acting policy; 0128 adds none.
	want := []string{"acting_lock", "brand_public_read", "brand_tenant_delete", "brand_tenant_insert", "brand_tenant_update"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("brands policies changed: %v, want exactly %v (ADR 0112 S6: no new brands policy)", names, want)
	}

	// Seed one row in each table, then show a SCOPE-LESS runtime session sees and writes nothing.
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM launch_authorisation_requests) + (SELECT count(*) FROM launch_authorisation_approvals) + (SELECT count(*) FROM launch_status_transitions)`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a scope-less session must see no row of the three tables: n=%d err=%v", n, err)
	}
	err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO launch_authorisation_requests (tenant_id, subject_kind, action, reason_code, readiness_snapshot, readiness_snapshot_hash)
			VALUES ($1, 'tenant', 'suspend', 'x', '{}'::jsonb, repeat('0', 64))`, f.tenantID)
		return err
	})
	if c := pgCodeOf(err); c != "LA001" && c != "42501" {
		t.Fatalf("a scope-less session must not insert a request: %q (%v)", c, err)
	}
}

// RLS isolation: tenant A's session cannot read tenant B's requests / transitions; the platform
// session reads both; a tenant session reads its own.
func TestLaunchGov_RLS_TenantIsolation(t *testing.T) {
	w := newLG(t)
	a := seedBrandPinTenant(t, w.owner)
	b := seedBrandPinTenant(t, w.owner)
	sa := w.tenantStaff(t, a.tenantID)
	var reqB uuid.UUID
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqB, _, err = lgRequest(ctx, tx, b.tenantID, &b.brandID, "suspend")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	count := func(session func(fn func(context.Context, pgx.Tx) error) error, q string, args ...any) int {
		var n int
		if err := session(func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, q, args...).Scan(&n) }); err != nil {
			t.Fatal(err)
		}
		return n
	}
	asA := func(fn func(context.Context, pgx.Tx) error) error { return w.tenantRT(a.tenantID, sa, fn) }
	asPlatform := func(fn func(context.Context, pgx.Tx) error) error { return w.platformRT(w.ops.Requester, fn) }
	if n := count(asA, `SELECT count(*) FROM launch_authorisation_requests WHERE id = $1`, reqB); n != 0 {
		t.Fatalf("tenant A reads tenant B's request: %d", n)
	}
	if n := count(asA, `SELECT count(*) FROM launch_status_transitions WHERE tenant_id = $1`, b.tenantID); n != 0 {
		t.Fatalf("tenant A reads tenant B's transitions: %d", n)
	}
	if n := count(asA, `SELECT count(*) FROM launch_status_transitions WHERE tenant_id = $1`, a.tenantID); n != 2 {
		t.Fatalf("tenant A must read its own two owner_provisioned transitions, got %d", n)
	}
	if n := count(asPlatform, `SELECT count(*) FROM launch_authorisation_requests WHERE id = $1`, reqB); n != 1 {
		t.Fatalf("the platform session must read tenant B's request, got %d", n)
	}
	// Tenant A cannot cancel or otherwise touch B's request (0 rows through RLS).
	if err := asA(func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1`, reqB)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Errorf("tenant A updated %d rows of tenant B's request", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// A player-shaped session sees nothing and can write nothing.
	pid := uuid.New()
	var seen int
	if err := w.rt.WithPlayerScope(context.Background(), a.tenantID, pid, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM launch_authorisation_requests) + (SELECT count(*) FROM launch_status_transitions)`).Scan(&seen)
	}); err != nil || seen != 0 {
		t.Fatalf("a player session must see none: n=%d err=%v", seen, err)
	}
}

// Append-only: UPDATE / DELETE / TRUNCATE on approvals and transitions, DELETE / TRUNCATE on
// requests, are refused with LA099 even for the table owner.
func TestLaunchGov_AppendOnly_LA099(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()
	f := seedBrandPinTenant(t, w.owner)
	brand := w.pendingBrand(t, f.tenantID)
	var reqID uuid.UUID
	var hash string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, hash, err = lgRequest(ctx, tx, f.tenantID, &brand, "activate")
		if err != nil {
			return err
		}
		if err := lgSetPlatform(ctx, tx, w.ops.Approver1); err != nil {
			return err
		}
		return lgApprove(ctx, tx, reqID, hash, "approve")
	}); err != nil {
		t.Fatal(err)
	}
	// RLS has no UPDATE / DELETE policy on these tables for ANY role, so a statement from the
	// owner (still bound by FORCE RLS) finds no row at all; with RLS lifted for one rolled-back
	// transaction (owner-only DDL) the append-only trigger itself must refuse with LA099.
	ownerPlatform := func(q string, args ...any) error {
		return w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, args...)
			return err
		})
	}
	errRollback := errors.New("rollback probe")
	guardRefusal := func(table, q string, args ...any) error {
		var got error
		err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE `+table+` NO FORCE ROW LEVEL SECURITY`); err != nil {
				return err
			}
			_, got = tx.Exec(ctx, q, args...)
			return errRollback
		})
		if !errors.Is(err, errRollback) {
			t.Fatalf("probe transaction: %v", err)
		}
		return got
	}
	rowsAffected := func(q string, args ...any) int64 {
		var n int64
		if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, q, args...)
			n = tag.RowsAffected()
			return err
		}); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
		return n
	}
	for _, c := range []struct {
		what, table, q string
		args           []any
	}{
		{"UPDATE approvals", "launch_authorisation_approvals", `UPDATE launch_authorisation_approvals SET reason_code = 'x' WHERE request_id = $1`, []any{reqID}},
		{"DELETE approvals", "launch_authorisation_approvals", `DELETE FROM launch_authorisation_approvals WHERE request_id = $1`, []any{reqID}},
		{"UPDATE transitions", "launch_status_transitions", `UPDATE launch_status_transitions SET to_status = 'closed' WHERE tenant_id = $1`, []any{f.tenantID}},
		{"DELETE transitions", "launch_status_transitions", `DELETE FROM launch_status_transitions WHERE tenant_id = $1`, []any{f.tenantID}},
		{"DELETE requests", "launch_authorisation_requests", `DELETE FROM launch_authorisation_requests WHERE id = $1`, []any{reqID}},
	} {
		if n := rowsAffected(c.q, c.args...); n != 0 {
			t.Fatalf("%s through RLS affected %d rows, want 0", c.what, n)
		}
		wantCode(t, c.what+" (RLS lifted)", guardRefusal(c.table, c.q, c.args...), "LA099")
	}
	wantCode(t, "TRUNCATE approvals", ownerPlatform(`TRUNCATE launch_authorisation_approvals`), "LA099")
	wantCode(t, "TRUNCATE transitions", ownerPlatform(`TRUNCATE launch_status_transitions`), "LA099")
	// requests is referenced by foreign keys, which refuses TRUNCATE (0A000) before the trigger can.
	if c := pgCodeOf(ownerPlatform(`TRUNCATE launch_authorisation_requests`)); c != "LA099" && c != "0A000" {
		t.Fatalf("TRUNCATE requests: want LA099 or 0A000, got %q", c)
	}
	// The runtime role has no such privilege at all.
	err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM launch_status_transitions WHERE tenant_id = $1`, f.tenantID)
		return err
	})
	wantCode(t, "runtime DELETE transitions (no privilege)", err, "42501")
}

// S13 / 4.6: the runtime role's grants are exactly SELECT+INSERT (and UPDATE on four request
// state columns), and every foreign key from the new tables is ON DELETE RESTRICT.
func TestLaunchGov_Grants_And_FKsRestrict(t *testing.T) {
	owner := brandPinOwnerPool(t)
	ctx := context.Background()
	privs := map[string]map[string]bool{}
	if err := owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, tbl := range []string{"launch_authorisation_requests", "launch_authorisation_approvals", "launch_status_transitions"} {
			privs[tbl] = map[string]bool{}
			for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
				var has bool
				if err := tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', $1, $2)`, "public."+tbl, p).Scan(&has); err != nil {
					return err
				}
				privs[tbl][p] = has
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for tbl, got := range privs {
		if !got["SELECT"] || !got["INSERT"] || got["DELETE"] || got["TRUNCATE"] || got["REFERENCES"] || got["TRIGGER"] {
			t.Fatalf("%s: unexpected runtime privileges %v", tbl, got)
		}
		if got["UPDATE"] {
			t.Fatalf("%s: runtime must have no TABLE-level UPDATE (only column-level on the request state columns), got %v", tbl, got)
		}
	}
	cols := map[string]bool{}
	if err := owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.column_privileges
			WHERE grantee = 'igaming_runtime' AND table_name = 'launch_authorisation_requests' AND privilege_type = 'UPDATE'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				return err
			}
			cols[c] = true
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(cols) != 4 || !cols["status"] || !cols["decided_at"] || !cols["refusal_code"] || !cols["executing_txid"] {
		t.Fatalf("runtime column-level UPDATE on requests = %v, want exactly the four state columns", cols)
	}

	// Every FK from the new tables to a non-new table is RESTRICT (confdeltype 'r').
	var offenders []string
	if err := owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT conrelid::regclass::text || '.' || conname FROM pg_constraint
			WHERE contype = 'f' AND confdeltype <> 'r'
			  AND conrelid IN ('launch_authorisation_requests'::regclass, 'launch_authorisation_approvals'::regclass, 'launch_status_transitions'::regclass)`)
		if err != nil {
			return err
		}
		offenders, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(offenders) > 0 {
		t.Fatalf("foreign keys that are not ON DELETE RESTRICT: %v", offenders)
	}
	// ... and deleting a tenant that has history is refused (history is never erased).
	f := seedBrandPinTenant(t, owner)
	err := owner.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM tenants WHERE id = $1`, f.tenantID)
		return err
	})
	wantCode(t, "deleting a tenant with launch history", err, "23503")
}

// S6 technique: the status guard restores app.tenant_id after reading the decision records in
// the platform-executor shape, so later statements of the transaction keep their scope.
func TestLaunchGov_S6_StatusGuardRestoresTenantGUC(t *testing.T) {
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)
	var after string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
			VALUES ($1, 'brand', $2, 'governed', 'active', 'suspended', $3)`, f.tenantID, f.brandID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE brands SET status = 'suspended' WHERE id = $1`, f.brandID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.tenant_id', true), '')`).Scan(&after); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1`, id)
		return err
	}); err != nil {
		t.Fatalf("S6 executor shape: %v", err)
	}
	if after != f.tenantID.String() {
		t.Fatalf("app.tenant_id after the brand UPDATE = %q, want it restored to %s", after, f.tenantID)
	}
}

// Security S-7: a role that is a MEMBER of the table-owner role (it inherits ownership and can disable
// triggers and RLS) is refused by the production startup check even though it owns nothing itself.
func TestLaunchGov_S7_ProductionCheckRefusesMemberOfOwnerRole(t *testing.T) {
	ctx := context.Background()
	scratch := scratchdb.New(t, "s7own_")
	owner, err := db.Connect(ctx, scratch, 3, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	if _, err := owner.MigrateUp(ctx, "../../migrations"); err != nil {
		t.Fatal(err)
	}
	member, err := db.Connect(ctx, scratchdb.AsAdmin(t, scratch), 3, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(member.Close)
	var directOwner bool
	if err := member.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT bool_or(tableowner = current_user) FROM pg_tables WHERE schemaname = 'public'`).Scan(&directOwner)
	}); err != nil {
		t.Fatal(err)
	}
	if directOwner {
		t.Fatal("test premise: the member role must not own any table directly")
	}
	if err := db.VerifyRuntimeRoleInProduction(ctx, "production", member); err == nil || !strings.Contains(err.Error(), "owns tables") {
		t.Fatalf("a member of the owner role must be refused in production, got %v", err)
	}
}

// S11: the production startup check refuses the table-owner role (it owns tenants and brands)
// and accepts the non-owning runtime role. The owner-only paths of S2 / LF2 are therefore
// unreachable from a production process.
func TestLaunchGov_S11_ProductionStartupRefusesTableOwner(t *testing.T) {
	owner := brandPinOwnerPool(t)
	rt := brandPinRuntimePool(t)
	ctx := context.Background()
	err := db.VerifyRuntimeRoleInProduction(ctx, "production", owner)
	if err == nil || !strings.Contains(err.Error(), "owns tables") {
		t.Fatalf("production start as the table-owner role must be refused, got %v", err)
	}
	// The refusal is for the right reason: the owner role owns tenants AND brands.
	var owns bool
	if err := owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT bool_and(tableowner = current_user) FROM pg_tables WHERE schemaname = 'public' AND tablename IN ('tenants', 'brands')`).Scan(&owns)
	}); err != nil || !owns {
		t.Fatalf("the owner pool must own tenants and brands: owns=%v err=%v", owns, err)
	}
	if err := db.VerifyRuntimeRoleInProduction(ctx, "production", rt); err != nil {
		t.Fatalf("the non-owning runtime role must pass the production check: %v", err)
	}
	// Non-production environments are unchanged (the dev owner connection stays allowed).
	if err := db.VerifyRuntimeRoleInProduction(ctx, "development", owner); err != nil {
		t.Fatalf("development must not be affected: %v", err)
	}
}
