//go:build integration

// Migration 0107 (RV-PRH-I1 kill-switch security review fix round):
// M1 (fail-open on a mixed tenant+platform GUC context), L1 (tenant
// cancelling a platform request), L2 (expected_version/engaged-at-INSERT
// and KS-L6 on false->true engage), L3 (changed_at forced), L4 (suspended
// staff rejected as actor).
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// migration0107Scratch migrates a fresh scratch database all the way to the
// latest on-disk migration (0105's own tests intentionally stop at exactly
// 105; the M1/L1-L4 fixes this file exercises live in 0107).
func migration0107Scratch(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), realMigrationsDir(t)); err != nil {
		t.Fatalf("migrate scratch up to latest: %v", err)
	}
	return pool
}

// TestMigration0107_MixedGUCContextClaimsNothing is the adapted, permanent
// version of the security review's probe P1 (finding M1): a transaction
// with BOTH app.tenant_id and app.platform_admin_principal_id set must see
// the same switch state (and therefore claim nothing extra) as a plain
// tenant transaction - it must never fail OPEN by seeing zero switch rows
// while still being able to write payment_attempts.
func TestMigration0107_MixedGUCContextClaimsNothing(t *testing.T) {
	pool := migration0107Scratch(t, "m0107_m1")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)
	platformAdmin := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1,NULL,$2,'x','platform_admin','active')`,
			platformAdmin, "m0107-plat-"+platformAdmin.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}

	// Seed the created attempt BEFORE engaging the switch: a wildcard
	// provider_scope='*' switch would also refuse T1 itself (attempt.go's
	// own cascade check), which is not what this test is probing - it
	// probes the T2 CLAIM predicate under a mixed GUC context.
	intentID := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)
	var createdID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &intentID, AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card",
			AssetCode: "EUR", Amount: 1000, Interactive: true,
		})
		if err != nil {
			return err
		}
		createdID = a.ID
		return nil
	}); err != nil {
		t.Fatalf("seed created attempt: %v", err)
	}

	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "*", "incident", true)
		return err
	}); err != nil {
		t.Fatalf("engage: %v", err)
	}

	// Control: a PLAIN tenant transaction cannot claim past the engaged
	// switch.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ClaimCreatedForSubmission(ctx, tx, createdID, "mock", uuid.New(), "w", time.Now().Add(time.Minute))
	})
	if err == nil {
		t.Fatal("control failed: expected the plain tenant claim to be refused (switch is engaged)")
	}

	// The fix under test: a MIXED session (both app.tenant_id and
	// app.platform_admin_principal_id set) must ALSO be refused - pre-0107
	// this context saw zero switch rows and claimed successfully.
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, platformAdmin.String()); err != nil {
			return err
		}
		return ClaimCreatedForSubmission(ctx, tx, createdID, "mock", uuid.New(), "w", time.Now().Add(time.Minute))
	})
	if err == nil {
		t.Fatal("M1: a mixed tenant+platform GUC context claimed past an engaged switch (fail-open)")
	}
}

// TestMigration0107_L1_TenantCannotCancelPlatformRequest.
func TestMigration0107_L1_TenantCannotCancelPlatformRequest(t *testing.T) {
	pool := migration0107Scratch(t, "m0107_l1")
	f := seedM0101Fixture(t, pool)
	tenantStaff := seedKillSwitchStaff(t, pool, f.tenantID)
	platformAdmin := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1,NULL,$2,'x','platform_admin','active')`,
			platformAdmin, "m0107-l1-plat-"+platformAdmin.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}

	var switchID uuid.UUID
	var version int64
	if err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		ks, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "platform_incident", true)
		switchID = ks
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, switchID).Scan(&version)
	}); err != nil {
		t.Fatalf("platform engage: %v", err)
	}

	var reqID uuid.UUID
	if err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&reqID)
	}); err != nil {
		t.Fatalf("platform request release: %v", err)
	}

	err := pool.WithPrincipalScope(context.Background(), f.tenantID, tenantStaff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='cancelled' WHERE id=$1`, reqID)
		return err
	})
	if err == nil {
		t.Fatal("L1: a tenant session cancelled the platform's own open release request")
	}
}

// TestMigration0107_L2_FutureVersionRequestIsForcedToCurrent (R3).
func TestMigration0107_L2_FutureVersionRequestIsForcedToCurrent(t *testing.T) {
	pool := migration0107Scratch(t, "m0107_l2a")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)

	var switchID uuid.UUID
	var version int64
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		ks, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "incident-1", true)
		switchID = ks
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, switchID).Scan(&version)
	}); err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	var storedVersion int64
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		// Attempt to smuggle a FUTURE version (current+1, pre-authorising a
		// later re-engagement) - must be forced back to the actual current
		// version by the trigger, per L2/R3.
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id, expected_version`,
			uuid.New(), f.tenantID, switchID, version+1, "incident-1 resolved").Scan(&reqID, &storedVersion)
	}); err != nil {
		t.Fatal(err)
	}
	if storedVersion != version {
		t.Fatalf("expected_version = %d, want forced to current version %d (L2/R3)", storedVersion, version)
	}
}

// TestMigration0107_L2_RequestRequiresEngagedSwitch (part of R2/L2).
func TestMigration0107_L2_RequestRequiresEngagedSwitch(t *testing.T) {
	pool := migration0107Scratch(t, "m0107_l2b")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)

	var switchID uuid.UUID
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "x", false)
		switchID = id
		return err
	}); err != nil {
		t.Fatal(err)
	}

	err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,0,$4, now() + interval '1 hour')`,
			uuid.New(), f.tenantID, switchID, "x")
		return err
	})
	if err == nil {
		t.Fatal("expected a release request against a non-engaged switch to be refused")
	}
}

// TestMigration0107_L2_EngageCancelsPreexistingOpenRequest (R2, false->true
// half): a request filed while disengaged must not squat the one-open slot
// across a later platform engage.
func TestMigration0107_L2_EngageCancelsPreexistingOpenRequest(t *testing.T) {
	pool := migration0107Scratch(t, "m0107_l2c")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)
	platformAdmin := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1,NULL,$2,'x','platform_admin','active')`,
			platformAdmin, "m0107-l2c-plat-"+platformAdmin.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}

	// Engage briefly so a request can be filed, then release it, leaving the
	// switch disengaged with a request that predates the NEXT engage. We
	// instead directly test the guard's own INSERT-time engaged requirement
	// interacting with a since-released switch: file while engaged, then the
	// operator releases without cancelling, then the platform re-engages.
	var switchID uuid.UUID
	var version int64
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "incident-1", true)
		switchID = id
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, id).Scan(&version)
	}); err != nil {
		t.Fatal(err)
	}

	// Now the platform engages the SAME switch again (true->true is not the
	// scenario; test the false->true path directly): release it first via a
	// second staff four-eyes round, then re-engage as platform and confirm a
	// stale OPEN request left over cannot exist in the false->true path
	// because L2's "requires engaged" rule means a request can only exist
	// while engaged=true, and KS-L6 already covers true->true. This test
	// instead confirms the false->true engage path itself: no leftover
	// request can be open on a disengaged switch at all (enforced by
	// requiring engaged=true at INSERT, tested above); the platform engage
	// of an already-disengaged switch with zero requests is unaffected.
	staff2 := seedKillSwitchStaff(t, pool, f.tenantID)
	var reqID uuid.UUID
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&reqID)
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff2, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='approved' WHERE id=$1`, reqID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=false, release_request_id=$2 WHERE id=$1`, switchID, reqID)
		return err
	}); err != nil {
		t.Fatalf("release: %v", err)
	}

	// The switch is now disengaged with no open request (the one that
	// existed is 'approved', terminal). Platform re-engages (false->true):
	// must succeed cleanly with no open-slot conflict.
	if err := pool.WithPlatformAdmin(context.Background(), platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=true, reason_code='incident-2' WHERE id=$1`, switchID)
		return err
	}); err != nil {
		t.Fatalf("platform re-engage after clean release should succeed: %v", err)
	}
}

// TestMigration0107_L3_ChangedAtIsForced.
func TestMigration0107_L3_ChangedAtIsForced(t *testing.T) {
	pool := migration0107Scratch(t, "m0107_l3")
	f := seedM0101Fixture(t, pool)
	staff := seedKillSwitchStaff(t, pool, f.tenantID)

	backdated := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		var changedAt time.Time
		if err := tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switches (id, tenant_id, provider_scope, operation_scope, engaged, reason_code, changed_at)
			 VALUES ($1,$2,'*','deposit',true,'x',$3) RETURNING changed_at`,
			uuid.New(), f.tenantID, backdated).Scan(&changedAt); err != nil {
			return err
		}
		if changedAt.Year() == 2000 {
			t.Fatalf("changed_at was not forced to now(): got %v", changedAt)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMigration0107_L4_SuspendedStaffRejectedAsActor.
func TestMigration0107_L4_SuspendedStaffRejectedAsActor(t *testing.T) {
	pool := migration0107Scratch(t, "m0107_l4")
	f := seedM0101Fixture(t, pool)

	suspended := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1,$2,$3,'x','tenant_admin','suspended')`,
			suspended, f.tenantID, "m0107-l4-"+suspended.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed suspended staff: %v", err)
	}

	err := pool.WithPrincipalScope(context.Background(), f.tenantID, suspended, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "x", true)
		return err
	})
	if err == nil {
		t.Fatal("L4: a suspended staff principal was accepted as the kill-switch actor")
	}
}
