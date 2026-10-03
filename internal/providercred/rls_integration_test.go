//go:build integration

// RLS, bridge and structure (security review §1.6 "RLS, bridge and
// structure"), as the NOBYPASSRLS runtime role.
package providercred

import (
	"context"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type policyRow struct {
	name, cmd string
	qual      *string
	check     *string
}

func policies(t *testing.T, f *fx, table string) []policyRow {
	t.Helper()
	var out []policyRow
	err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT policyname, cmd, qual, with_check FROM pg_policies WHERE tablename = $1 ORDER BY policyname`, table)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p policyRow
			if err := rows.Scan(&p.name, &p.cmd, &p.qual, &p.check); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func count(t *testing.T, tx pgx.Tx, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := tx.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPCHandles_RLS_TenantIsolation(t *testing.T) {
	f := newFx(t)
	a, b := f.tenant(), f.tenant()
	f.register(f.spec(a, "acme", "k1"))
	hb, _ := f.register(f.spec(b, "acme", "k1"))
	ctx := context.Background()

	if err := f.rt.WithTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM provider_credential_handles WHERE tenant_id = $1`, b); n != 0 {
			t.Fatalf("tenant A sees %d of B's handles", n)
		}
		if n := count(t, tx, `SELECT count(*) FROM provider_credential_handles WHERE id = $1`, hb.ID); n != 0 {
			t.Fatal("tenant A sees B's handle by id")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM provider_credential_handles WHERE tenant_id IN ($1, $2)`, a, b); n != 0 {
			t.Fatalf("WithoutTenant sees %d handles", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.WithPlatformAdmin(ctx, f.requester, func(ctx context.Context, tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM provider_credential_handles WHERE tenant_id IN ($1, $2)`, a, b); n != 0 {
			t.Fatalf("platform scope sees %d handles (no platform policy may exist)", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.rt.WithPlayerScope(ctx, b, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM provider_credential_handles WHERE tenant_id = $1`, b); n != 0 {
			t.Fatalf("player scope sees %d of its own tenant's handles", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// An insert naming tenant B under tenant A's scope is refused.
	r, _ := f.file(f.spec(b, "acme", "k2"))
	err := f.directHandleInsert(a, r, nil)
	if err == nil {
		t.Fatal("an insert with tenant_id = B under A's scope must be refused")
	}
	// And an UPDATE of B's handle from A's scope touches nothing.
	if err := f.rt.WithTenant(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE provider_credential_handles SET revoked_by = $2, revoke_reason = 'tenant_request', status = 'revoked' WHERE id = $1`, hb.ID, f.requester)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatal("tenant A revoked tenant B's handle")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPCHandles_NoPlatformOrDualScopePolicy(t *testing.T) {
	f := newFx(t)
	ps := policies(t, f, "provider_credential_handles")
	want := map[string]string{"tenant_isolation_read": "SELECT", "tenant_isolation_insert": "INSERT", "tenant_isolation_update": "UPDATE"}
	if len(ps) != len(want) {
		t.Fatalf("handles policies = %+v, want exactly %v", ps, want)
	}
	for _, p := range ps {
		if want[p.name] != p.cmd {
			t.Fatalf("unexpected policy %s (%s)", p.name, p.cmd)
		}
		text := deref(p.qual) + " " + deref(p.check)
		if strings.Contains(text, "platform_admin_principal_id") || strings.Contains(text, "IS NULL) OR") {
			t.Fatalf("policy %s is platform or dual-scoped: %s", p.name, text)
		}
		if !strings.Contains(text, "app.player_account_id") || !strings.Contains(text, "app.tenant_id") {
			t.Fatalf("policy %s lacks the tenant or the player-unset predicate: %s", p.name, text)
		}
	}
	var forced, enabled bool
	if err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'provider_credential_handles'`).Scan(&enabled, &forced)
	}); err != nil || !enabled || !forced {
		t.Fatalf("RLS must be enabled and forced: enabled=%v forced=%v err=%v", enabled, forced, err)
	}
}

func TestPCHandles_SchemaHasNoSecretColumns(t *testing.T) {
	f := newFx(t)
	forbidden := regexp.MustCompile(`secret|key_material|private|password`)
	err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT table_name, column_name FROM information_schema.columns
			WHERE table_name IN ('provider_credential_handles', 'provider_credential_change_requests', 'provider_credential_change_approvals')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var table, col string
			if err := rows.Scan(&table, &col); err != nil {
				return err
			}
			n++
			if col != "secret_ref" && forbidden.MatchString(col) {
				t.Errorf("%s.%s looks like secret material", table, col)
			}
		}
		if n < 40 {
			t.Fatalf("scanned only %d columns - the check would be vacuous", n)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPCGovernance_BridgePoliciesSelectUpdateOnly(t *testing.T) {
	f := newFx(t)
	for table, want := range map[string]map[string]string{
		"provider_credential_change_requests":  {"platform_admin_scope": "ALL", "tenant_consume_read": "SELECT", "tenant_consume_apply": "UPDATE"},
		"provider_credential_change_approvals": {"platform_admin_scope": "ALL", "tenant_consume_read": "SELECT"},
	} {
		ps := policies(t, f, table)
		if len(ps) != len(want) {
			t.Fatalf("%s policies = %+v, want %v", table, ps, want)
		}
		for _, p := range ps {
			if want[p.name] != p.cmd {
				t.Fatalf("%s: unexpected policy %s (%s)", table, p.name, p.cmd)
			}
			text := deref(p.qual) + " " + deref(p.check)
			if !strings.Contains(text, "app.player_account_id") {
				t.Fatalf("%s.%s lacks the player-unset predicate (B2): %s", table, p.name, text)
			}
			if strings.HasPrefix(p.name, "tenant_") && strings.Contains(text, "platform_admin_principal_id") {
				t.Fatalf("%s.%s: a tenant bridge policy must not reference the platform GUC", table, p.name)
			}
		}
	}
	for _, table := range []string{"provider_credential_change_requests", "provider_credential_change_approvals"} {
		var forced bool
		if err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE relname = $1`, table).Scan(&forced)
		}); err != nil || !forced {
			t.Fatalf("%s must FORCE RLS", table)
		}
	}
}

// TestPCGovernance_StaffUsersPoliciesUnchanged compares staff_users'
// dual_scope_isolation policy with migration 0011's exact policy
// (normalized by pg_get_expr): the provider-credential work must never
// widen it. The only other policies allowed on staff_users are the
// closed ADR 0099 (migration 0112) acting family, whose predicates are
// pinned by internal/db's K1 tests; any other policy fails here.
func TestPCGovernance_StaffUsersPoliciesUnchanged(t *testing.T) {
	f := newFx(t)
	ps := policies(t, f, "staff_users")
	const want = `(((tenant_id IS NOT NULL) AND (tenant_id = (NULLIF(current_setting('app.tenant_id'::text, true), ''::text))::uuid)) OR ((tenant_id IS NULL) AND (NULLIF(current_setting('app.tenant_id'::text, true), ''::text) IS NULL)))`
	adr0099Acting := map[string]string{
		"acting_read":         "SELECT",
		"acting_lock":         "UPDATE",
		"acting_fence_select": "SELECT",
		"acting_fence_insert": "INSERT",
		"acting_fence_update": "UPDATE",
		"acting_fence_delete": "DELETE",
	}
	var sawIsolation bool
	for _, p := range ps {
		if p.name == "dual_scope_isolation" {
			if p.cmd != "ALL" || deref(p.qual) != want || deref(p.check) != want {
				t.Fatalf("staff_users.dual_scope_isolation changed from migration 0011: %+v", p)
			}
			sawIsolation = true
			continue
		}
		if cmd, ok := adr0099Acting[p.name]; !ok || cmd != p.cmd {
			t.Fatalf("staff_users has an unexpected policy %s (%s); only 0011's dual_scope_isolation and the ADR 0099 acting family are allowed: %+v", p.name, p.cmd, ps)
		}
	}
	if !sawIsolation {
		t.Fatalf("staff_users.dual_scope_isolation is missing: %+v", ps)
	}
}

func TestPCGovernance_NoSecurityDefiner(t *testing.T) {
	f := newFx(t)
	var names []string
	err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT proname, prosecdef FROM pg_proc WHERE proname LIKE 'provider_credential%'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var secdef bool
			if err := rows.Scan(&name, &secdef); err != nil {
				return err
			}
			if secdef {
				t.Errorf("function %s is SECURITY DEFINER (B7)", name)
			}
			names = append(names, name)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	if len(names) != 10 {
		t.Fatalf("expected the 10 migration-0096 functions, found %v", names)
	}
}

func TestPCMigration_RuntimeGrantsMinimal(t *testing.T) {
	f := newFx(t)
	type check struct {
		table, priv string
		want        bool
	}
	checks := []check{
		{"provider_credential_handles", "SELECT", true}, {"provider_credential_handles", "INSERT", true},
		{"provider_credential_handles", "UPDATE", false}, {"provider_credential_handles", "DELETE", false},
		{"provider_credential_handles", "TRUNCATE", false},
		{"provider_credential_change_requests", "SELECT", true}, {"provider_credential_change_requests", "INSERT", true},
		{"provider_credential_change_requests", "UPDATE", false}, {"provider_credential_change_requests", "DELETE", false},
		{"provider_credential_change_requests", "TRUNCATE", false},
		{"provider_credential_change_approvals", "SELECT", true}, {"provider_credential_change_approvals", "INSERT", true},
		{"provider_credential_change_approvals", "UPDATE", false}, {"provider_credential_change_approvals", "DELETE", false},
		{"provider_credential_change_approvals", "TRUNCATE", false},
	}
	updatable := map[string][]string{
		"provider_credential_handles":         {"status", "status_changed_at", "not_after", "revoked_at", "revoked_by", "revoke_reason"},
		"provider_credential_change_requests": {"state", "applied_at", "applied_by_principal_id", "applied_handle_id"},
	}
	err := f.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		for _, c := range checks {
			var got bool
			if err := tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', $1, $2)`, c.table, c.priv).Scan(&got); err != nil {
				return err
			}
			if got != c.want {
				t.Errorf("igaming_runtime %s on %s = %v, want %v", c.priv, c.table, got, c.want)
			}
		}
		for table, cols := range updatable {
			allowed := map[string]bool{}
			for _, c := range cols {
				allowed[c] = true
			}
			rows, err := tx.Query(ctx, `SELECT column_name, has_column_privilege('igaming_runtime', $1, column_name, 'UPDATE')
				FROM information_schema.columns WHERE table_name = $1`, table)
			if err != nil {
				return err
			}
			for rows.Next() {
				var col string
				var can bool
				if err := rows.Scan(&col, &can); err != nil {
					rows.Close()
					return err
				}
				if can != allowed[col] {
					t.Errorf("igaming_runtime UPDATE(%s.%s) = %v, want %v", table, col, can, allowed[col])
				}
			}
			rows.Close()
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPCGovernance_TenantScopeCannotInsertRequestOrApproval(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	r, _ := f.file(f.spec(tenant, "acme", "k1"))
	err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_credential_change_requests
			(target_tenant_id, domain, provider_id, purpose, key_id, secret_ref, fingerprint, not_before,
			 predecessor_disposition, reason_code, requested_by_principal_id)
			VALUES ($1,'casino','acme','webhook_verify','k9',$2,$3,now(),'none','initial_registration',$4)`,
			tenant, memRef(tenant, "casino", "acme", "n"), randomFingerprint(t), f.requester)
		return err
	})
	if err == nil {
		t.Fatal("tenant scope must not file a request")
	}
	err = f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO provider_credential_change_approvals (request_id, approver_principal_id, decision, content_hash)
			VALUES ($1, $2, 'approve', $3)`, r.ID, f.approver, r.ContentHash)
		return err
	})
	if err == nil {
		t.Fatal("tenant scope must not approve")
	}
}

func TestPCGovernance_PlayerScopeSeesNothing(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	r, _ := f.file(f.spec(tenant, "acme", "k1"))
	f.approve(r, f.approver)
	if _, err := f.apply(r); err != nil {
		t.Fatal(err)
	}
	err := f.rt.WithPlayerScope(context.Background(), tenant, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		for _, table := range []string{"provider_credential_handles", "provider_credential_change_requests", "provider_credential_change_approvals"} {
			if n := count(t, tx, `SELECT count(*) FROM `+table); n != 0 {
				t.Errorf("player scope sees %d rows of %s", n, table)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPCGovernance_TenantBCannotSeeOrConsumeTenantARequest(t *testing.T) {
	f := newFx(t)
	a, b := f.tenant(), f.tenant()
	r, _ := f.file(f.spec(a, "acme", "k1"))
	f.approve(r, f.approver)
	err := f.rt.WithTenant(context.Background(), b, func(ctx context.Context, tx pgx.Tx) error {
		if n := count(t, tx, `SELECT count(*) FROM provider_credential_change_requests WHERE id = $1`, r.ID); n != 0 {
			t.Fatal("tenant B sees A's request")
		}
		if n := count(t, tx, `SELECT count(*) FROM provider_credential_change_approvals WHERE request_id = $1`, r.ID); n != 0 {
			t.Fatal("tenant B sees A's approvals")
		}
		tag, err := tx.Exec(ctx, `UPDATE provider_credential_change_requests SET state = 'applied' WHERE id = $1`, r.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatal("tenant B marked A's request")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// B inserting a handle that names A's request: invisible -> PC001 (or
	// the composite FK, which binds the request's tenant to the handle's).
	err = f.directHandleInsert(b, r, func(h *Handle) {
		h.TenantID = b
		h.SecretRef = memRef(b, "casino", "acme", "x")
	})
	if err == nil {
		t.Fatal("tenant B consumed tenant A's approved request")
	}
	if _, err := f.apply(r); err != nil {
		t.Fatalf("A's request must still be applicable by A: %v", err)
	}
}

func TestPCGovernance_PlatformScopeCannotMarkApplied(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h, _ := f.register(f.spec(tenant, "acme", "k1"))
	r, _ := f.file(f.spec(tenant, "acme", "k2"))
	f.approve(r, f.approver)
	err := f.rt.WithPlatformAdmin(context.Background(), f.requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_credential_change_requests
			SET state = 'applied', applied_handle_id = $2, applied_by_principal_id = $3 WHERE id = $1`, r.ID, h.ID, f.requester)
		return err
	})
	if pgCode(err) != "PC034" {
		t.Fatalf("platform scope must not mark applied (B4), got %v", err)
	}
}

func TestPCGovernance_TenantScopeCannotMarkAppliedWithoutHandle(t *testing.T) {
	f := newFx(t)
	tenant := f.tenant()
	h, _ := f.register(f.spec(tenant, "acme", "k1")) // a real handle of this tenant, pointing to ANOTHER request
	r, _ := f.file(f.spec(tenant, "acme", "k2"))
	f.approve(r, f.approver)
	err := f.rt.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_credential_change_requests
			SET state = 'applied', applied_handle_id = $2, applied_by_principal_id = $3 WHERE id = $1`, r.ID, h.ID, f.requester)
		return err
	})
	if pgCode(err) != "PC034" {
		t.Fatalf("tenant scope must not burn an approval without a handle pointing back (B4), got %v", err)
	}
}
