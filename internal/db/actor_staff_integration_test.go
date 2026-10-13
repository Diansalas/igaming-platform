//go:build integration

package db

// ADR 0119 S4b section 9.5(b), migration 0151, as igaming_runtime: the staff_actor_self_select / staff_actor_self_lock
// pair. Under WithTenant(target) a platform actor's own staff_users row is hidden by the dual-scope policy; with
// app.actor_staff_id set (SetActorStaffIDForCurrentTx) it is visible and FOR SHARE-lockable, and ONLY that row; the
// pair never admits an UPDATE (WITH CHECK (false)); the restrictive KYC-worker fence still hides it; without the GUC
// nothing changes.

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestActorStaffSelfPolicies_AsRuntimeRole(t *testing.T) {
	owner := testPool(t)
	rtURL := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtURL == "" {
		t.Fatal("TEST_RUNTIME_DATABASE_URL is required (a skip is not evidence)")
	}
	rt, err := Connect(context.Background(), rtURL, 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	ctx := context.Background()
	tenantID := createTestTenant(t, owner)
	admin, other := uuid.New(), uuid.New()
	if err := owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range []uuid.UUID{admin, other} {
			if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active')`,
				id, "s4b-actor-"+id.String()+"@fixture.invalid"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed platform staff: %v", err)
	}
	count := func(ctx context.Context, tx pgx.Tx, sql string, args ...any) int {
		t.Helper()
		var n int
		if err := tx.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return n
	}
	// Without the GUC: hidden under WithTenant(target).
	if err := rt.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if n := count(ctx, tx, `SELECT count(*) FROM staff_users WHERE id = ANY($1)`, []uuid.UUID{admin, other}); n != 0 {
			t.Fatalf("platform rows visible under a tenant scope without the GUC: %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// With the GUC: exactly the actor's own row, lockable FOR SHARE.
	if err := rt.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := SetActorStaffIDForCurrentTx(ctx, tx, admin); err != nil {
			return err
		}
		rows, err := ScanActorStaffRows(tx.Query(ctx, `SELECT id, tenant_id, role, status, person_id FROM staff_users WHERE id = ANY($1) ORDER BY id FOR SHARE`,
			[]uuid.UUID{admin, other}))
		if err != nil {
			return err
		}
		if len(rows) != 1 || rows[0].ID != admin || CheckActorStaffLive(rows, admin, "platform_admin") != nil {
			t.Fatalf("with the GUC: want exactly the actor's live row, got %+v", rows)
		}
		return nil
	}); err != nil {
		t.Fatalf("FOR SHARE through the GUC: %v", err)
	}
	// The pair never admits an UPDATE: through the GUC an UPDATE is refused by RLS (WITH CHECK (false)).
	err = rt.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := SetActorStaffIDForCurrentTx(ctx, tx, admin); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE staff_users SET display_name = 'hijack' WHERE id = $1`, admin)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" || !strings.Contains(pgErr.Message, "row-level security") {
		t.Fatalf("an UPDATE through staff_actor_self_lock must be an RLS violation, got %v", err)
	}
	// The restrictive KYC-worker fence still applies (a worker session setting the GUC sees nothing).
	if err := rt.WithPlatformService(ctx, ServiceKYCSubmissionWorker, func(ctx context.Context, tx pgx.Tx) error {
		if err := SetActorStaffIDForCurrentTx(ctx, tx, admin); err != nil {
			return err
		}
		if n := count(ctx, tx, `SELECT count(*) FROM staff_users WHERE id = $1`, admin); n != 0 {
			t.Fatalf("the KYC-worker fence must still hide the row: %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := SetActorStaffIDForCurrentTx(ctx, nil, uuid.Nil); err == nil {
		t.Fatal("a nil staff id must be refused before any statement")
	}
}

// The catalog: exactly the two policies, permissive, PUBLIC, with the pinned expressions; the runtime role's sessions
// UPDATE columns are exactly the S3 three plus the S4b step-up three.
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
	sel, lock := got["staff_actor_self_select"], got["staff_actor_self_lock"]
	if len(got) != 2 || sel.cmd != "r" || lock.cmd != "w" || sel.check != "" || lock.check != "false" ||
		!strings.Contains(sel.qual, "app.actor_staff_id") || sel.qual != lock.qual {
		t.Fatalf("staff_actor_self_* policies: %+v", got)
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
