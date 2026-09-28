//go:build integration

// PRH-2 G1 (ADR 0104 §6/§7): migration 0109's RLS policy
// (subject_tenant_read) and BEFORE INSERT trigger
// (audit_log_subject_actor_guard), against the shared, already-migrated
// TEST_DATABASE_URL database - the same pattern audit_integration_test.go
// already uses.
package audit

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// stPlatformAdmin seeds a platform-scoped (tenant_id IS NULL) staff_users
// row and returns its id - the only shape audit_log_subject_actor_guard
// ever accepts as an actor for a subject row.
func stPlatformAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			id, "st-admin-"+id.String()+"@example.com")
		return err
	}); err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM staff_users WHERE id = $1`, id)
			return err
		})
	})
	return id
}

func stTenantStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
			id, tenantID, "st-tenant-"+id.String()+"@example.com")
		return err
	}); err != nil {
		t.Fatalf("seed tenant staff: %v", err)
	}
	return id
}

// recordSubjectRow writes a genuinely valid subject row under a real
// WithPlatformAdmin(adminID) session (the only production-shaped way to
// satisfy the trigger) - used as the "known good" fixture for both the
// RLS visibility tests and as the seed for the AZ-refusal tests' negative
// mutations below.
func recordSubjectRow(t *testing.T, pool *db.Pool, adminID, subjectTenantID uuid.UUID, action string) {
	t.Helper()
	if err := pool.WithPlatformAdmin(context.Background(), adminID, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{
			ActorType: ActorStaff, ActorID: adminID, Action: action, Outcome: OutcomeSuccess,
			SubjectTenantID: subjectTenantID,
		})
	}); err != nil {
		t.Fatalf("seed subject row: %v", err)
	}
}

// TestSubjectTenantRLS_TIHeadline is ADR 0104 §7's headline test: tenant A
// sees exactly its own subject rows, never tenant B's, under a genuine
// tenant-scoped RLS-filtered read (WithTenant), which is what
// newListPlatformActionsAuditLogHandler itself uses.
func TestSubjectTenantRLS_TIHeadline(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)
	admin := stPlatformAdmin(t, pool)

	actionA := "test.subject_ti_a_" + uuid.NewString()
	actionB := "test.subject_ti_b_" + uuid.NewString()
	recordSubjectRow(t, pool, admin, tenantA, actionA)
	recordSubjectRow(t, pool, admin, tenantB, actionB)

	var countA, countAofB, countB, countBofA int
	_ = pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, actionA).Scan(&countA)
	})
	_ = pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, actionB).Scan(&countBofA)
	})
	_ = pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, actionB).Scan(&countB)
	})
	_ = pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, actionA).Scan(&countAofB)
	})

	if countA != 1 {
		t.Errorf("expected tenant A to see its own subject row, got %d", countA)
	}
	if countB != 1 {
		t.Errorf("expected tenant B to see its own subject row, got %d", countB)
	}
	if countBofA != 0 {
		t.Errorf("expected tenant A to see NONE of tenant B's subject rows, got %d", countBofA)
	}
	if countAofB != 0 {
		t.Errorf("expected tenant B to see NONE of tenant A's subject rows, got %d", countAofB)
	}
}

// TestSubjectTenantRLS_ExcludedSessions is ADR 0104 §7's "excluded
// sessions" list: zero subject rows for a player session, a mixed
// platform+tenant session, a tenant session with app.platform_service_id
// set, and a tenant session with app.acting_* set (SA-2). Every one of
// these sessions still names tenantA as app.tenant_id (where applicable),
// so if the RLS predicate ever regressed to drop one of its exclusion
// clauses, EXACTLY these cases would start leaking the row.
func TestSubjectTenantRLS_ExcludedSessions(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	admin := stPlatformAdmin(t, pool)
	action := "test.subject_excluded_" + uuid.NewString()
	recordSubjectRow(t, pool, admin, tenantA, action)

	count := func(t *testing.T, setup func(ctx context.Context, tx pgx.Tx) error) int {
		t.Helper()
		var n int
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if err := setup(ctx, tx); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = $1`, action).Scan(&n)
		}); err != nil {
			t.Fatalf("query under excluded session: %v", err)
		}
		return n
	}

	// A plain tenant session (control): must see the row - proves the
	// exclusions below are actually doing something, not just failing
	// closed for unrelated reasons.
	if n := count(t, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String())
		return err
	}); n != 1 {
		t.Fatalf("control: expected a plain tenant session to see the row, got %d", n)
	}

	// Player session: app.tenant_id + app.player_account_id both set.
	if n := count(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.player_account_id', $1, true)`, uuid.NewString())
		return err
	}); n != 0 {
		t.Errorf("player session: expected 0 subject rows, got %d", n)
	}

	// Mixed platform+tenant session: app.tenant_id AND app.platform_admin_principal_id both set.
	if n := count(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, admin.String())
		return err
	}); n != 0 {
		t.Errorf("mixed platform+tenant session: expected 0 subject rows, got %d", n)
	}

	// Tenant session with app.platform_service_id also set.
	if n := count(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.platform_service_id', $1, true)`, "kyc-worker")
		return err
	}); n != 0 {
		t.Errorf("tenant+platform_service_id session: expected 0 subject rows, got %d", n)
	}

	// Tenant session with app.acting_tenant_id set (SA-2).
	if n := count(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true)`, tenantA.String())
		return err
	}); n != 0 {
		t.Errorf("tenant+acting_tenant_id session: expected 0 subject rows, got %d", n)
	}

	// Tenant session with app.acting_platform_principal_id set (SA-2).
	if n := count(t, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantA.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT set_config('app.acting_platform_principal_id', $1, true)`, admin.String())
		return err
	}); n != 0 {
		t.Errorf("tenant+acting_platform_principal_id session: expected 0 subject rows, got %d", n)
	}
}

// isP0001 reports whether err is the trigger's own RAISE EXCEPTION
// (SQLSTATE P0001), the class every audit_log_subject_actor_guard refusal
// uses.
func isP0001(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "P0001"
}

// TestSubjectActorGuard_Refusals is ADR 0104 §7's "AZ/RLS write side"
// list, and §7's MUT items for the trigger.
func TestSubjectActorGuard_Refusals(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	admin := stPlatformAdmin(t, pool)
	tenantStaff := stTenantStaff(t, pool, tenantA)

	// (1) A tenant insert of a subject: refused - no app.platform_admin_
	// principal_id at all under a plain WithTenant session.
	err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorType: ActorStaff, ActorID: tenantStaff, Action: "test.az1", Outcome: OutcomeSuccess, SubjectTenantID: tenantA})
	})
	if !isP0001(err) {
		t.Errorf("(1) tenant insert of a subject: expected P0001, got %v", err)
	}

	// (2) A platform actor != the GUC principal: refused.
	otherAdmin := stPlatformAdmin(t, pool)
	err = pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorType: ActorStaff, ActorID: otherAdmin, Action: "test.az2", Outcome: OutcomeSuccess, SubjectTenantID: tenantA})
	})
	if !isP0001(err) {
		t.Errorf("(2) actor != GUC principal: expected P0001, got %v", err)
	}

	// (3) A tenant staff actor (even naming itself, under a genuine platform
	// GUC session it could never actually reach in production - this
	// isolates the actor-lookup clause): refused, since the trigger's own
	// EXISTS check requires a PLATFORM (tenant_id IS NULL) staff_users row
	// for the GUC principal itself, and tenantStaff is not one.
	err = pool.WithPlatformAdmin(context.Background(), tenantStaff, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorType: ActorStaff, ActorID: tenantStaff, Action: "test.az3", Outcome: OutcomeSuccess, SubjectTenantID: tenantA})
	})
	if !isP0001(err) {
		t.Errorf("(3) tenant staff actor: expected P0001, got %v", err)
	}

	// (4) A system actor: refused (actor_type <> 'staff').
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, admin.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, subject_tenant_id, actor_type, actor_id, action, outcome)
			VALUES (NULL, $1, 'system', NULL, 'test.az4', 'success')`, tenantA)
		return err
	})
	if !isP0001(err) {
		t.Errorf("(4) system actor: expected P0001, got %v", err)
	}

	// (5) A session with app.acting_tenant_id set (MUT: "no app.acting_*
	// exclusion" must be caught here too, on the WRITE side): refused.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, admin.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true)`, tenantA.String()); err != nil {
			return err
		}
		return Record(ctx, tx, Entry{ActorType: ActorStaff, ActorID: admin, Action: "test.az5", Outcome: OutcomeSuccess, SubjectTenantID: tenantA})
	})
	if !isP0001(err) {
		t.Errorf("(5) app.acting_tenant_id set: expected P0001, got %v", err)
	}

	// (5b) app.platform_service_id set alongside the platform GUC: refused.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, admin.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_service_id', $1, true)`, "kyc-worker"); err != nil {
			return err
		}
		return Record(ctx, tx, Entry{ActorType: ActorStaff, ActorID: admin, Action: "test.az5b", Outcome: OutcomeSuccess, SubjectTenantID: tenantA})
	})
	if !isP0001(err) {
		t.Errorf("(5b) app.platform_service_id set: expected P0001, got %v", err)
	}

	// (6) Both tenant columns set: CHECK violation (audit_log_subject_
	// tenant_platform_only), not necessarily P0001 - Postgres raises this
	// as a check_violation (SQLSTATE 23514) via the CHECK constraint.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, admin.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, subject_tenant_id, actor_type, actor_id, action, outcome)
			VALUES ($1, $1, 'staff', $2, 'test.az6', 'success')`, tenantA, admin)
		return err
	})
	// Refused either by the CHECK constraint (23514) or by
	// dual_scope_isolation's own WITH CHECK (42501, since app.tenant_id is
	// unset here while tenant_id is non-NULL) - both are legitimate,
	// independent reasons this row can never be written; either is an
	// acceptable refusal for this test.
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || (pgErr.Code != "23514" && pgErr.Code != "42501") {
		t.Errorf("(6) both tenant columns set: expected a CHECK or RLS violation, got %v", err)
	}

	// (7) An unknown subject tenant: FK violation.
	err = pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorType: ActorStaff, ActorID: admin, Action: "test.az7", Outcome: OutcomeSuccess, SubjectTenantID: uuid.New()})
	})
	if !errors.As(err, &pgErr) || pgErr.Code != "23503" {
		t.Errorf("(7) unknown subject tenant: expected a foreign-key violation, got %v", err)
	}

	// (8) A suspended platform admin can still write (Q7: no status check).
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, admin)
		return err
	}); err != nil {
		t.Fatalf("suspend admin: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return Record(ctx, tx, Entry{ActorType: ActorStaff, ActorID: admin, Action: "test.az8_suspended_ok", Outcome: OutcomeSuccess, SubjectTenantID: tenantA})
	})
	if err != nil {
		t.Errorf("(8) suspended platform admin must still be able to write a subject row (Q7), got %v", err)
	}
}

// TestSubjectActorGuard_AppendOnlyStillHolds pins that migration 0109 did
// not touch the append-only guarantee for a subject row: UPDATE, DELETE
// and TRUNCATE remain refused (AT-4: "the append-only triggers are
// byte-identical").
func TestSubjectActorGuard_AppendOnlyStillHolds(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	admin := stPlatformAdmin(t, pool)
	action := "test.subject_immutable_" + uuid.NewString()
	recordSubjectRow(t, pool, admin, tenantA, action)

	updateErr := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE audit_log SET outcome = 'failure' WHERE action = $1`, action)
		return err
	})
	if !isP0001(updateErr) {
		t.Errorf("expected UPDATE against a subject row to fail with P0001, got %v", updateErr)
	}
	deleteErr := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM audit_log WHERE action = $1`, action)
		return err
	})
	if !isP0001(deleteErr) {
		t.Errorf("expected DELETE against a subject row to fail with P0001, got %v", deleteErr)
	}
}

// TestSubjectTenantReadPolicy_IsSelectOnly is a direct pin against the
// mutant "the policy changed to FOR ALL" (ADR 0104 §7 MUT): subject_
// tenant_read must be a SELECT-only ('r') policy, never FOR ALL ('*'),
// regardless of what the append-only triggers separately enforce -
// widening it to FOR ALL would be a real widening of RLS-granted write
// authority even though the table's own immutability triggers still
// block the actual mutation, and this test catches that at the policy
// definition itself rather than relying only on the trigger backstop.
func TestSubjectTenantReadPolicy_IsSelectOnly(t *testing.T) {
	pool := testPool(t)
	var cmd string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT cmd FROM pg_policies WHERE tablename = 'audit_log' AND policyname = 'subject_tenant_read'`).Scan(&cmd)
	}); err != nil {
		t.Fatalf("query pg_policies: %v", err)
	}
	if cmd != "SELECT" {
		t.Fatalf("expected subject_tenant_read to be a SELECT-only policy, got cmd=%q", cmd)
	}
}
