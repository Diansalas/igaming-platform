//go:build integration

// PRH-I1 kill switch, migration 0105 (ADR 0095 §10.2/§10.2.1/§10.2.2,
// numbering note in the migration file's own header). Tests run on scratch
// databases only, following migration_0101_integration_test.go's pattern.
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

// migration0105Scratch is named after the migration that introduced the
// kill switch, but it deliberately migrates a fresh scratch database all
// the way to HEAD (the real on-disk migrations/ directory), not just
// through 0105. This is the fix for RV-PRH-I1 security re-verification 1's
// N1 finding: migration 0106 `CREATE OR REPLACE`s the three kill-switch
// trigger functions, so a scratch DB pinned at exactly 0105 exercises the
// SUPERSEDED function bodies, not the live ones - the fix round's original
// "K10/K11/K14/K19 killed" claim was measured against dead code as a
// result. None of this file's tests are about migration 0105's own history
// in isolation; they are behavioural tests of the kill switch as a whole,
// so there is no reason to pin them below head. Any future migration that
// further CREATE OR REPLACEs a guard function covered here must not
// reintroduce a pin - see the "future migrations" rule this comment
// documents.
func migration0105Scratch(t *testing.T, prefix string) *db.Pool {
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

type m0105Fixture struct {
	tenantID          uuid.UUID
	platformPrincipal uuid.UUID
	tenantPrincipalA  uuid.UUID
	tenantPrincipalB  uuid.UUID
	otherTenantID     uuid.UUID
}

func seedM0105Fixture(t *testing.T, pool *db.Pool) m0105Fixture {
	t.Helper()
	f := m0105Fixture{
		tenantID:          uuid.New(),
		otherTenantID:     uuid.New(),
		platformPrincipal: uuid.New(),
		tenantPrincipalA:  uuid.New(),
		tenantPrincipalB:  uuid.New(),
	}
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		for _, tid := range []uuid.UUID{f.tenantID, f.otherTenantID} {
			if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1,$2,'m0105 tenant','under_platform_licence')`,
				tid, "m0105-"+tid.String()[:8]); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active')`,
			f.platformPrincipal, "m0105-platform-"+f.platformPrincipal.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed m0105 platform fixture: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, $2, $3, 'x', 'tenant_admin', 'active')`,
			f.tenantPrincipalA, f.tenantID, "m0105-a-"+f.tenantPrincipalA.String()+"@test.example"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, $2, $3, 'x', 'tenant_admin', 'active')`,
			f.tenantPrincipalB, f.tenantID, "m0105-b-"+f.tenantPrincipalB.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed m0105 tenant fixture: %v", err)
	}
	return f
}

func insertKillSwitchRow(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerScope, opScope, reason string, engaged bool) (uuid.UUID, error) {
	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO payment_kill_switches (id, tenant_id, provider_scope, operation_scope, engaged, reason_code)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		id, tenantID, providerScope, opScope, engaged, reason)
	return id, err
}

func TestMigration0105_EngageForcesActorAndScope(t *testing.T) {
	pool := migration0105Scratch(t, "m0105a")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual_containment", true)
		switchID = id
		return err
	})
	if err != nil {
		t.Fatalf("engage: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var changedBy uuid.UUID
		var changedByScope, engagedByScope string
		var version int64
		if err := tx.QueryRow(ctx, `SELECT changed_by, changed_by_scope, engaged_by_scope, version FROM payment_kill_switches WHERE id=$1`, switchID).
			Scan(&changedBy, &changedByScope, &engagedByScope, &version); err != nil {
			return err
		}
		if changedBy != f.tenantPrincipalA {
			t.Errorf("changed_by = %s, want %s", changedBy, f.tenantPrincipalA)
		}
		if changedByScope != "tenant" || engagedByScope != "tenant" {
			t.Errorf("changed_by_scope=%s engaged_by_scope=%s, want tenant/tenant", changedByScope, engagedByScope)
		}
		if version != 1 {
			t.Errorf("version = %d, want 1", version)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
}

func TestMigration0105_ClientSuppliedActorIsIgnored(t *testing.T) {
	pool := migration0105Scratch(t, "m0105b")
	f := seedM0105Fixture(t, pool)

	imposter := uuid.New() // never inserted into staff_users
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_kill_switches (id, tenant_id, provider_scope, operation_scope, engaged, reason_code, changed_by, changed_by_scope)
			 VALUES ($1,$2,'*','deposit',true,'x',$3,'platform')`,
			uuid.New(), f.tenantID, imposter)
		return err
	})
	if err != nil {
		t.Fatalf("insert with spoofed actor should still succeed (forced, not rejected): %v", err)
	}
	// Confirm it was actually forced to the real session principal, not the
	// imposter value the statement attempted to write.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var changedBy uuid.UUID
		var scope string
		if err := tx.QueryRow(ctx, `SELECT changed_by, changed_by_scope FROM payment_kill_switches WHERE tenant_id=$1`, f.tenantID).Scan(&changedBy, &scope); err != nil {
			return err
		}
		if changedBy == imposter || scope == "platform" {
			t.Fatalf("actor/scope were NOT forced from the session: changed_by=%s scope=%s", changedBy, scope)
		}
		if changedBy != f.tenantPrincipalA || scope != "tenant" {
			t.Fatalf("changed_by=%s scope=%s, want %s/tenant", changedBy, scope, f.tenantPrincipalA)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigration0105_DeleteIsRejected(t *testing.T) {
	pool := migration0105Scratch(t, "m0105c")
	f := seedM0105Fixture(t, pool)

	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "x", true)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM payment_kill_switches WHERE id=$1`, id)
		return err
	})
	if err == nil {
		t.Fatal("expected DELETE to be rejected")
	}
}

func TestMigration0105_FourEyesReleaseHappyPath(t *testing.T) {
	pool := migration0105Scratch(t, "m0105d")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual", true)
		if err != nil {
			return err
		}
		switchID = id
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, id).Scan(&version)
	})
	if err != nil {
		t.Fatalf("engage: %v", err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&reqID)
	})
	if err != nil {
		t.Fatalf("create release request: %v", err)
	}

	// Requester approving their own request is refused (four-eyes).
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='approved' WHERE id=$1`, reqID)
		return err
	})
	if err == nil {
		t.Fatal("expected self-approval to be refused")
	}

	// A distinct principal approves and releases in one transaction.
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='approved' WHERE id=$1`, reqID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=false, release_request_id=$2 WHERE id=$1`, switchID, reqID)
		return err
	})
	if err != nil {
		t.Fatalf("approve+release: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var engaged bool
		if err := tx.QueryRow(ctx, `SELECT engaged FROM payment_kill_switches WHERE id=$1`, switchID).Scan(&engaged); err != nil {
			return err
		}
		if engaged {
			t.Fatal("expected switch to be released")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigration0105_StaleVersionCannotRelease(t *testing.T) {
	pool := migration0105Scratch(t, "m0105e")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual", true)
		if err != nil {
			return err
		}
		switchID = id
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, id).Scan(&version)
	})
	if err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&reqID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Re-engage (a reason-code touch) bumps the version, invalidating the
	// outstanding request before it is approved.
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET reason_code='updated' WHERE id=$1`, switchID)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='approved' WHERE id=$1`, reqID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=false, release_request_id=$2 WHERE id=$1`, switchID, reqID)
		return err
	})
	if err == nil {
		t.Fatal("expected release against a stale expected_version to be refused")
	}
}

func TestMigration0105_TenantCannotTouchPlatformEngagedRow(t *testing.T) {
	pool := migration0105Scratch(t, "m0105f")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), f.platformPrincipal, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "*", "platform_containment", true)
		switchID = id
		return err
	})
	if err != nil {
		t.Fatalf("platform engage: %v", err)
	}

	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET reason_code='tenant_tries' WHERE id=$1`, switchID)
		return err
	})
	if err == nil {
		t.Fatal("expected a tenant session to be refused on a platform-engaged row")
	}
}

func TestMigration0105_PlatformTakeoverCancelsOpenTenantRequest(t *testing.T) {
	pool := migration0105Scratch(t, "m0105g")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual", true)
		if err != nil {
			return err
		}
		switchID = id
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, id).Scan(&version)
	})
	if err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&reqID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Platform takes over the containment (re-engage while already engaged).
	err = pool.WithPlatformAdmin(context.Background(), f.platformPrincipal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=true, reason_code='platform_takeover' WHERE id=$1`, switchID)
		return err
	})
	if err != nil {
		t.Fatalf("platform takeover: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var engagedByScope, status string
		if err := tx.QueryRow(ctx, `SELECT engaged_by_scope FROM payment_kill_switches WHERE id=$1`, switchID).Scan(&engagedByScope); err != nil {
			return err
		}
		if engagedByScope != "platform" {
			t.Fatalf("engaged_by_scope = %s, want platform", engagedByScope)
		}
		if err := tx.QueryRow(ctx, `SELECT status FROM payment_kill_switch_release_requests WHERE id=$1`, reqID).Scan(&status); err != nil {
			return err
		}
		if status != "cancelled" {
			t.Fatalf("open tenant request status = %s, want cancelled (KS-L6)", status)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestMigration0105_ClaimPredicateBlocksSubmissionWhenEngaged(t *testing.T) {
	pool := migration0105Scratch(t, "m0105h")
	f := seedM0101Fixture(t, pool)

	depID := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)
	var attemptID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &depID, AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR",
			Amount: 1000, Interactive: true,
		})
		attemptID = a.ID
		return err
	})
	if err != nil {
		t.Fatalf("insert created: %v", err)
	}

	// Engage a wildcard-provider deposit kill switch for this tenant.
	staffPrincipal := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1,$2,$3,'x','tenant_admin','active')`,
			staffPrincipal, f.tenantID, "m0105h-"+staffPrincipal.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed staff: %v", err)
	}
	if err := pool.WithPrincipalScope(context.Background(), f.tenantID, staffPrincipal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "incident", true)
		return err
	}); err != nil {
		t.Fatalf("engage kill switch: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ClaimCreatedForSubmission(ctx, tx, attemptID, "mock", uuid.New(), "worker", time.Now().Add(time.Minute))
	})
	if err == nil {
		t.Fatal("expected T2 claim to be refused while a matching kill switch is engaged")
	}
}

// TestMigration0105_ScopeColumnsAreImmutable is the direct MX19 mutation
// target (RV-0095 N1): re-scoping an engaged row would be a single-actor
// release under a different identity, so provider_scope/operation_scope
// (and tenant_id/id) must never change after INSERT, on any UPDATE -
// engaged or not.
func TestMigration0105_ScopeColumnsAreImmutable(t *testing.T) {
	pool := migration0105Scratch(t, "m0105i")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "mock", "deposit", "x", false)
		switchID = id
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET provider_scope='*' WHERE id=$1`, switchID)
		return err
	})
	if err == nil {
		t.Fatal("expected provider_scope to be immutable after insert")
	}

	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET operation_scope='*' WHERE id=$1`, switchID)
		return err
	})
	if err == nil {
		t.Fatal("expected operation_scope to be immutable after insert")
	}
}

// TestMigration0105_DirectReleaseWithoutApprovedRequestIsRejected is the
// direct MX17 mutation target (S95-C5): a bare
// `UPDATE payment_kill_switches SET engaged=false ...` with no (or an
// invalid) release_request_id must never succeed - release is only ever
// reachable through the four-eyes request/approve path.
func TestMigration0105_DirectReleaseWithoutApprovedRequestIsRejected(t *testing.T) {
	pool := migration0105Scratch(t, "m0105l")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual", true)
		switchID = id
		return err
	})
	if err != nil {
		t.Fatal(err)
	}

	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=false WHERE id=$1`, switchID)
		return err
	})
	if err == nil {
		t.Fatal("expected a direct release with no release_request_id to be refused")
	}
}

// TestMigration0105_ApprovalMustReleaseInTheSameTransaction is the direct
// MX21 mutation target (RV-0095 N4): an approval committed in an EARLIER
// transaction must never be usable to release later - decided_txid must
// equal txid_current() at the moment of the release UPDATE itself, not
// merely "was approved at some point".
func TestMigration0105_ApprovalMustReleaseInTheSameTransaction(t *testing.T) {
	pool := migration0105Scratch(t, "m0105k")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual", true)
		if err != nil {
			return err
		}
		switchID = id
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, id).Scan(&version)
	})
	if err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&reqID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Approve and COMMIT, in its own transaction, separate from the release.
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='approved' WHERE id=$1`, reqID)
		return err
	})
	if err != nil {
		t.Fatalf("approve: %v", err)
	}

	// A LATER, separate transaction attempting the release must be refused:
	// the approval's decided_txid no longer equals txid_current().
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=false, release_request_id=$2 WHERE id=$1`, switchID, reqID)
		return err
	})
	if err == nil {
		t.Fatal("expected a release in a transaction OTHER than the one that approved it to be refused")
	}
}

// TestMigration0105_RequestIdentityColumnsAreImmutable is the direct MX25
// mutation target (RV-0095 N5): a single actor must never be able to
// rewrite requested_by (or any other INSERT-only column) on an open
// request to manufacture a "distinct" approver, or to redirect the
// request at a different switch/version/reason after the fact.
func TestMigration0105_RequestIdentityColumnsAreImmutable(t *testing.T) {
	pool := migration0105Scratch(t, "m0105j")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual", true)
		if err != nil {
			return err
		}
		switchID = id
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, id).Scan(&version)
	})
	if err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&reqID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// A same-status ("open" -> "open") no-op update is refused anyway (the
	// status transition must actually move), so requested_by immutability
	// must be probed ALONGSIDE a legitimate approval - the same principal
	// (tenantPrincipalA) rewriting requested_by to itself while a DISTINCT
	// approver (tenantPrincipalB) approves, exactly the ADR's "a single
	// actor can never rewrite requested_by to manufacture a distinct
	// approver" scenario. The requested_by change must be refused even
	// though the status transition itself (open -> approved) is legitimate.
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET requested_by=$2, status='approved' WHERE id=$1`, reqID, f.tenantPrincipalB)
		return err
	})
	if err == nil {
		t.Fatal("expected requested_by to be immutable - a single actor must never be able to manufacture a distinct approver")
	}

	// Likewise, reason_code must stay pinned even across a legitimate
	// status transition.
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET reason_code='different', status='approved' WHERE id=$1`, reqID)
		return err
	})
	if err == nil {
		t.Fatal("expected reason_code to be immutable even across a legitimate status transition")
	}
}

// TestMigration0105_M6_K19_PlatformGUCRequiresGenuinePlatformStaff is the
// direct RV-PRH-I1 security review K19 mutation target: the session
// resolver must independently re-validate that the id placed in
// app.platform_admin_principal_id resolves to a genuine platform-scoped
// (tenant_id IS NULL) staff_users row, not merely accept any value present
// in that GUC. A tenant-scoped staff id placed there must be refused.
//
// RV-PRH-I1 re-verification 1's N1 finding notes this test alone is a
// narrow probe: staff_users' own RLS already hides a tenant-scoped staff
// row from a platform-only (WithoutTenant) session for a reason unrelated
// to payment_kill_switch_session()'s own WHERE clause, so this test does
// not by itself demonstrate that the function's tenant_id IS NULL / status
// = 'active' predicate is what is doing the work. See
// TestMigration0105_N1_K19_RandomUUIDInPlatformGUCIsRefused immediately
// below for the broader probe (a UUID matching no staff_users row at all)
// that is unaffected by that RLS interaction and therefore kills the
// mutation that drops the WHOLE principal check, not just the tenant_id
// clause.
func TestMigration0105_M6_K19_PlatformGUCRequiresGenuinePlatformStaff(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_k19")
	f := seedM0105Fixture(t, pool)

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, f.tenantPrincipalA.String()); err != nil {
			return err
		}
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "x", true)
		return err
	})
	if err == nil {
		t.Fatal("K19: a tenant-scoped staff id in app.platform_admin_principal_id was accepted as a platform actor")
	}
}

// TestMigration0105_N1_K19_RandomUUIDInPlatformGUCIsRefused is the broader
// K19 probe RV-PRH-I1 re-verification 1 required: a UUID that matches NO
// staff_users row at all (not merely a tenant-scoped one) must still be
// refused as a platform actor. Unlike the tenant-staff-id variant above,
// no row exists for this id under ANY RLS visibility, in ANY session
// scope, so a green result here can only mean the session resolver's
// EXISTS(...) check itself is running - it cannot be explained away by an
// RLS side effect. This directly kills "K19: drop the whole principal
// predicate in payment_kill_switch_session()".
func TestMigration0105_N1_K19_RandomUUIDInPlatformGUCIsRefused(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_n1_k19a")
	f := seedM0105Fixture(t, pool)

	randomActor := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, randomActor.String()); err != nil {
			return err
		}
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "x", true)
		return err
	})
	if err == nil {
		t.Fatal("K19: a random UUID matching no staff_users row was accepted as a platform actor")
	}

	// Positive control: a genuine platform staff row in the same GUC must
	// still succeed, so this test cannot be satisfied by the resolver
	// simply refusing everything.
	err = pool.WithPlatformAdmin(context.Background(), f.platformPrincipal, func(ctx context.Context, tx pgx.Tx) error {
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "payout", "y", true)
		return err
	})
	if err != nil {
		t.Fatalf("expected a genuine platform staff principal to still succeed: %v", err)
	}
}

// TestMigration0105_N1_K19_SuspendedPlatformStaffUUIDIsRefused is the
// "non-platform staff UUID" half of RV-PRH-I1 re-verification 1's N1
// requirement: a UUID that DOES match a staff_users row, and one that IS
// platform-scoped (tenant_id IS NULL), but is not active (suspended), must
// still be refused. This is distinct from the L4 attempt-side suspension
// test - it exercises payment_kill_switch_session() specifically, which is
// also called from the release-request path, not just the attempt-claim
// path.
func TestMigration0105_N1_K19_SuspendedPlatformStaffUUIDIsRefused(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_n1_k19b")
	f := seedM0105Fixture(t, pool)

	suspendedPlatformStaff := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', 'suspended')`,
			suspendedPlatformStaff, "m0105-n1-k19b-"+suspendedPlatformStaff.String()+"@test.example")
		return err
	}); err != nil {
		t.Fatalf("seed suspended platform staff: %v", err)
	}

	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, suspendedPlatformStaff.String()); err != nil {
			return err
		}
		_, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "x", true)
		return err
	})
	if err == nil {
		t.Fatal("K19/L4: a suspended platform-scoped staff id in app.platform_admin_principal_id was accepted as a platform actor")
	}
}

// TestMigration0105_M6_K10_RequestMustBindToItsOwnSwitch is the direct K10
// mutation target: an approved request for switch X must never release a
// DIFFERENT switch Y, even one belonging to the same tenant.
func TestMigration0105_M6_K10_RequestMustBindToItsOwnSwitch(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_k10")
	f := seedM0105Fixture(t, pool)

	var switchX, switchY uuid.UUID
	var versionX int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		x, err := insertKillSwitchRow(ctx, tx, f.tenantID, "provider-x", "deposit", "x", true)
		if err != nil {
			return err
		}
		switchX = x
		y, err := insertKillSwitchRow(ctx, tx, f.tenantID, "provider-y", "deposit", "y", true)
		if err != nil {
			return err
		}
		switchY = y
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, x).Scan(&versionX)
	})
	if err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '1 hour') RETURNING id`,
			uuid.New(), f.tenantID, switchX, versionX, "resolved").Scan(&reqID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Approve and attempt to release switch Y with it IN THE SAME
	// transaction (the decided_txid = txid_current() rule requires this
	// regardless of K10 - approving and releasing in separate transactions
	// would already be refused for that unrelated reason, masking whether
	// the kill_switch_id binding check is doing anything).
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='approved' WHERE id=$1`, reqID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=false, release_request_id=$2 WHERE id=$1`, switchY, reqID)
		return err
	})
	if err == nil {
		t.Fatal("K10: a request approved for switch X released a different switch Y")
	}
}

// TestMigration0105_M6_K11_ExpiredRequestCannotRelease is the direct K11
// mutation target.
func TestMigration0105_M6_K11_ExpiredRequestCannotRelease(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_k11")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual", true)
		if err != nil {
			return err
		}
		switchID = id
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, id).Scan(&version)
	})
	if err != nil {
		t.Fatal(err)
	}

	var reqID uuid.UUID
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() - interval '1 minute') RETURNING id`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&reqID)
	})
	if err != nil {
		t.Fatal(err)
	}

	// Approve and release in the SAME transaction (decided_txid requires it
	// regardless of K11).
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalB, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='approved' WHERE id=$1`, reqID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE payment_kill_switches SET engaged=false, release_request_id=$2 WHERE id=$1`, switchID, reqID)
		return err
	})
	if err == nil {
		t.Fatal("K11: an expired request still released the switch")
	}
}

// TestMigration0105_M6_K14_ExpiresAtCappedAt24Hours is the direct K14
// mutation target.
func TestMigration0105_M6_K14_ExpiresAtCappedAt24Hours(t *testing.T) {
	pool := migration0105Scratch(t, "m0105_k14")
	f := seedM0105Fixture(t, pool)

	var switchID uuid.UUID
	var version int64
	err := pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		id, err := insertKillSwitchRow(ctx, tx, f.tenantID, "*", "deposit", "manual", true)
		if err != nil {
			return err
		}
		switchID = id
		return tx.QueryRow(ctx, `SELECT version FROM payment_kill_switches WHERE id=$1`, id).Scan(&version)
	})
	if err != nil {
		t.Fatal(err)
	}

	var expiresAt time.Time
	err = pool.WithPrincipalScope(context.Background(), f.tenantID, f.tenantPrincipalA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
			 VALUES ($1,$2,$3,$4,$5, now() + interval '100 days') RETURNING expires_at`,
			uuid.New(), f.tenantID, switchID, version, "resolved").Scan(&expiresAt)
	})
	if err != nil {
		t.Fatal(err)
	}
	if expiresAt.After(time.Now().Add(24*time.Hour + time.Minute)) {
		t.Fatalf("K14: expires_at = %v, expected capped at <= now()+24h", expiresAt)
	}
}
