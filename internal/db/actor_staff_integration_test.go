//go:build integration

package db

// ADR 0119 S4b, migration 0151: the catalog pins (the step-up column grants). ADR 0119 S5 + S6 (migration 0139): the
// s9.5(b) actor self-policy pair now exists, in exactly this shape.

import (
	"context"
	"strings"
	"testing"
)

// The catalog: exactly the two staff_actor_self_* policies of 0139 (SELECT, and UPDATE with WITH CHECK false: lockable,
// never updatable); the runtime role's sessions UPDATE columns are exactly the S3 three plus the S4b step-up three.
func TestMigration0151_CatalogPins(t *testing.T) {
	owner := testPool(t)
	ctx := context.Background()
	type pol struct{ cmd, qual, check string }
	got := map[string]pol{}
	rows, err := owner.Raw().Query(ctx, `SELECT polname, polcmd::text, polpermissive, coalesce(pg_get_expr(polqual, polrelid), ''), coalesce(pg_get_expr(polwithcheck, polrelid), '')
		FROM pg_policy WHERE polrelid = 'public.staff_users'::regclass AND polname LIKE 'staff_actor_self_%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, cmd, qual, check string
		var permissive bool
		if err := rows.Scan(&name, &cmd, &permissive, &qual, &check); err != nil {
			t.Fatal(err)
		}
		if !permissive {
			t.Errorf("%s must be permissive", name)
		}
		got[name] = pol{cmd, qual, check}
	}
	rows.Close()
	// ADR 0119 S5 + S6 review fix SR-7: the pair admits PLATFORM rows only (tenant_id IS NULL), so a forged
	// app.actor_staff_id naming a tenant staff member of another tenant reveals nothing.
	const key = "((tenant_id IS NULL) AND (id = (NULLIF(current_setting('app.actor_staff_id'::text, true), ''::text))::uuid))"
	want := map[string]pol{
		"staff_actor_self_select": {"r", key, ""},
		"staff_actor_self_lock":   {"w", key, "false"},
	}
	if len(got) != len(want) {
		t.Fatalf("staff_actor_self_* policies: got %+v, want %+v", got, want)
	}
	for n, w := range want {
		if got[n] != w {
			t.Fatalf("%s: got %+v, want %+v", n, got[n], w)
		}
	}
	var cols []string
	r2, err := owner.Raw().Query(ctx, `SELECT column_name FROM information_schema.column_privileges
		WHERE table_schema = 'public' AND table_name = 'sessions' AND grantee = 'igaming_runtime' AND privilege_type = 'UPDATE' ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	for r2.Next() {
		var c string
		if err := r2.Scan(&c); err != nil {
			t.Fatal(err)
		}
		cols = append(cols, c)
	}
	r2.Close()
	if strings.Join(cols, ",") != "assurance_kid,assurance_seal,last_used_at,mfa_at,replaced_by_session_id,revoked_at" {
		t.Fatalf("runtime UPDATE columns on sessions: %v", cols)
	}
}
