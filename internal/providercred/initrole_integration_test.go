//go:build integration

package providercred

import (
	"context"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// runtimePrivileges snapshots igaming_runtime's table- and column-level
// privileges on every table in the schema.
func runtimePrivileges(t *testing.T, w *scratchWorld) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	rows, err := w.owner.Raw().Query(context.Background(), `
		SELECT 'table:' || table_name || ':' || privilege_type FROM information_schema.role_table_grants
		 WHERE grantee = 'igaming_runtime' AND table_schema = 'public'
		UNION ALL
		SELECT 'column:' || table_name || '.' || column_name || ':' || privilege_type FROM information_schema.column_privileges
		 WHERE grantee = 'igaming_runtime' AND table_schema = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatal(err)
		}
		out[k] = true
	}
	return out
}

// runInitAppRoleTail runs deploy/init-app-role.sql from its runtime-role
// section onward (the part that is re-run to backfill an already-migrated
// database) against the scratch database, as its owner.
func runInitAppRoleTail(t *testing.T, w *scratchWorld) {
	t.Helper()
	raw, err := os.ReadFile("../../deploy/init-app-role.sql")
	if err != nil {
		t.Fatal(err)
	}
	const marker = "-- --- PLAT-ROLESPLIT-1: non-owning runtime application role ---"
	i := strings.Index(string(raw), marker)
	if i < 0 {
		t.Fatal("init-app-role.sql marker not found")
	}
	u, err := url.Parse(w.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.ReplaceAll(string(raw[i:]), "igaming_platform_dev", pgx.Identifier{strings.TrimPrefix(u.Path, "/")}.Sanitize())
	conn, err := pgx.Connect(context.Background(), w.ownerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(context.Background(), script); err != nil {
		t.Fatalf("init-app-role.sql tail: %v", err)
	}
}

// TestInitAppRole_RerunKeepsProviderCredentialGrants: re-running the
// idempotent deploy/init-app-role.sql backfill against an already-migrated
// database must leave the provider-credential tables at migration 0096's
// least-privilege grants (its blanket "GRANT ... ON ALL TABLES" would
// otherwise silently widen them), and a second run changes nothing at all.
func TestInitAppRole_RerunKeepsProviderCredentialGrants(t *testing.T) {
	w := newScratchWorld(t, "pc_initrole_", "")
	isPC := func(k string) bool { return strings.Contains(k, ":provider_credential_") }
	afterMigration := runtimePrivileges(t, w)

	runInitAppRoleTail(t, w)
	afterFirst := runtimePrivileges(t, w)
	for k := range afterFirst {
		if isPC(k) && !afterMigration[k] {
			t.Errorf("init-app-role.sql widened %s", k)
		}
	}
	for k := range afterMigration {
		if isPC(k) && !afterFirst[k] {
			t.Errorf("init-app-role.sql removed %s", k)
		}
	}
	for _, must := range []string{
		"table:provider_credential_handles:SELECT", "table:provider_credential_handles:INSERT",
		"column:provider_credential_handles.revoke_reason:UPDATE",
		"table:provider_credential_change_requests:INSERT", "column:provider_credential_change_requests.state:UPDATE",
		"table:provider_credential_change_approvals:SELECT",
	} {
		if !afterFirst[must] {
			t.Errorf("missing expected grant %s", must)
		}
	}
	for _, mustNot := range []string{
		"table:provider_credential_handles:UPDATE", "table:provider_credential_handles:DELETE",
		"column:provider_credential_handles.activation_request_id:UPDATE",
		"table:provider_credential_change_requests:DELETE", "table:provider_credential_change_approvals:UPDATE",
	} {
		if afterFirst[mustNot] {
			t.Errorf("unexpected grant %s", mustNot)
		}
	}

	runInitAppRoleTail(t, w)
	afterSecond := runtimePrivileges(t, w)
	if len(afterSecond) != len(afterFirst) {
		t.Fatalf("a second run changed the privilege count %d -> %d", len(afterFirst), len(afterSecond))
	}
	for k := range afterFirst {
		if !afterSecond[k] {
			t.Fatalf("a second run removed %s", k)
		}
	}
}

// TestInitAppRole_RerunKeepsCasinoCallbackRejectionsGrants: the same proof
// as TestInitAppRole_RerunKeepsProviderCredentialGrants, but for migration
// 0097's casino_callback_rejections (Stage 10.3 W2b). Re-running the
// idempotent deploy/init-app-role.sql backfill against an already-migrated
// database must leave the table at migration 0097's own least-privilege
// grant - SELECT and INSERT only, since the table is pure append-only
// history with no mutable column at all (its blanket "GRANT ... ON ALL
// TABLES" would otherwise silently re-grant UPDATE/DELETE) - and a second
// run changes nothing at all.
func TestInitAppRole_RerunKeepsCasinoCallbackRejectionsGrants(t *testing.T) {
	w := newScratchWorld(t, "ccr_initrole_", "")
	isCCR := func(k string) bool { return strings.Contains(k, ":casino_callback_rejections:") }
	afterMigration := runtimePrivileges(t, w)

	runInitAppRoleTail(t, w)
	afterFirst := runtimePrivileges(t, w)
	for k := range afterFirst {
		if isCCR(k) && !afterMigration[k] {
			t.Errorf("init-app-role.sql widened %s", k)
		}
	}
	for k := range afterMigration {
		if isCCR(k) && !afterFirst[k] {
			t.Errorf("init-app-role.sql removed %s", k)
		}
	}
	for _, must := range []string{
		"table:casino_callback_rejections:SELECT", "table:casino_callback_rejections:INSERT",
	} {
		if !afterFirst[must] {
			t.Errorf("missing expected grant %s", must)
		}
	}
	for _, mustNot := range []string{
		"table:casino_callback_rejections:UPDATE", "table:casino_callback_rejections:DELETE",
		"table:casino_callback_rejections:TRUNCATE",
	} {
		if afterFirst[mustNot] {
			t.Errorf("unexpected grant %s", mustNot)
		}
	}

	runInitAppRoleTail(t, w)
	afterSecond := runtimePrivileges(t, w)
	if len(afterSecond) != len(afterFirst) {
		t.Fatalf("a second run changed the privilege count %d -> %d", len(afterFirst), len(afterSecond))
	}
	for k := range afterFirst {
		if !afterSecond[k] {
			t.Fatalf("a second run removed %s", k)
		}
	}
}
