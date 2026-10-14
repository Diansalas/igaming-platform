//go:build integration

package db

// ADR 0119 S4b, migration 0151: the catalog pins (the step-up column grants; no actor self-policy).

import (
	"context"
	"strings"
	"testing"
)

// The catalog after 0151: NO staff_actor_self_* policy (not built, S4b-R3); the runtime role's sessions UPDATE columns
// are exactly the S3 three plus the S4b step-up three.
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
	if len(got) != 0 {
		t.Fatalf("no staff_actor_self_* policy may exist yet: %+v", got)
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
