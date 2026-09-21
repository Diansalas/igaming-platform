//go:build integration

// Stage 9.2 fix round (qa + code-reviewer, real coverage gap): unlike its
// sibling sb_jurisdiction_restrictions (which has
// sportsbook_jurisdiction_restrictions_rls_integration_test.go), before
// this fix round sb_exposure_limits had no direct-SQL RLS adversarial test
// proving a tenant-scoped connection cannot read/write ANOTHER tenant's
// exposure limits, and no test that a player-scoped connection is denied
// entirely (ADR 0083 §12.1 item 11's third, previously-unimplemented
// claim). This file closes that gap, mirroring
// sportsbook_jurisdiction_restrictions_rls_integration_test.go's own
// structure/rationale - adapted for sb_exposure_limits' DIFFERENT RLS
// shape (migration 0088): a plain, TENANT-scoped, two-policy
// tenant_staff_scope FOR ALL, not sb_jurisdiction_restrictions'
// platform-admin-write/read-open shape. There is deliberately no
// platform-admin, no catalogue-sync-service and no read-open path here at
// all - a limit is trading-book intelligence with no player-facing or
// platform-wide read path (ADR 0083 §6.2.3).
package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const sbExposureLimitInsertSQL = `INSERT INTO sb_exposure_limits
	(tenant_id, scope_kind, asset_code, max_open_potential_payout, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
	VALUES ($1, 'selection', 'EUR', 100000, 'ref', 'reason', 'staff', $2) RETURNING id`

// seedExposureLimitForRLSTest inserts one active sb_exposure_limits row for
// tenantID via a genuinely tenant-scoped connection - the one legitimate
// writer this table has.
func seedExposureLimitForRLSTest(t *testing.T, pool *Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sbExposureLimitInsertSQL, tenantID, uuid.New()).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed sb_exposure_limits row for tenant %s: %v", tenantID, err)
	}
	return id
}

// TestSportsbookExposureLimitsRLS_TenantScopedConnectionCanReadAndWriteItsOwn
// proves the one legitimate scope (a genuinely tenant-scoped, non-player
// connection) can both write and read its own tenant's rows.
func TestSportsbookExposureLimitsRLS_TenantScopedConnectionCanReadAndWriteItsOwn(t *testing.T) {
	pool := testPool(t)
	tenantID := createTestTenant(t, pool)

	limitID := seedExposureLimitForRLSTest(t, pool, tenantID)
	if limitID == uuid.Nil {
		t.Fatal("expected a real exposure limit id")
	}

	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sb_exposure_limits WHERE id = $1`, limitID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("read own tenant's exposure limit: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected the tenant-scoped connection to see its own row, got count %d", count)
	}

	// The one mutation migration 0088's immutability trigger permits
	// (status/reason_code) also succeeds under the tenant's own scope.
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE sb_exposure_limits SET status = 'disabled', reason_code = 'test-disable' WHERE id = $1`, limitID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to update 1 row, updated %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected the tenant-scoped connection to disable its own row: %v", err)
	}
}

// TestSportsbookExposureLimitsRLS_CrossTenantIsolation proves tenant B's
// connection cannot read OR write tenant A's sb_exposure_limits rows -
// ADR 0083 §12.1 item 11's third claim, previously asserted only by the
// migration file's own header comment, never empirically.
func TestSportsbookExposureLimitsRLS_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	tenantA := createTestTenant(t, pool)
	tenantB := createTestTenant(t, pool)

	limitID := seedExposureLimitForRLSTest(t, pool, tenantA)

	t.Run("read", func(t *testing.T) {
		var count int
		err := pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM sb_exposure_limits WHERE id = $1`, limitID).Scan(&count)
		})
		if err != nil {
			t.Fatalf("tenant B's own read must not itself error: %v", err)
		}
		if count != 0 {
			t.Fatalf("expected tenant B to see ZERO of tenant A's rows, got %d", count)
		}
	})

	t.Run("write (insert naming tenant A's id)", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx, sbExposureLimitInsertSQL, tenantA, uuid.New()).Scan(&id)
		})
		if err == nil {
			t.Fatal("expected tenant B's connection to be refused inserting a row naming tenant A's tenant_id")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != pgRLSViolationCode {
			t.Fatalf("expected SQLSTATE %s (RLS violation), got: %v", pgRLSViolationCode, err)
		}
	})

	t.Run("write (update tenant A's row under tenant B's scope)", func(t *testing.T) {
		err := pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE sb_exposure_limits SET status = 'disabled', reason_code = 'tenant-b-should-not' WHERE id = $1`, limitID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 0 {
				t.Fatalf("expected zero rows affected (RLS scopes the row out entirely, never a visible conflict), got %d", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("tenant B's UPDATE must affect zero rows silently, not error: %v", err)
		}

		// Confirm tenant A's row is genuinely untouched.
		var status string
		err = pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status FROM sb_exposure_limits WHERE id = $1`, limitID).Scan(&status)
		})
		if err != nil {
			t.Fatalf("read tenant A's row status: %v", err)
		}
		if status != "active" {
			t.Fatalf("expected tenant A's row to remain 'active' after tenant B's no-op update attempt, got %q", status)
		}
	})
}

// TestSportsbookExposureLimitsRLS_PlayerScopedConnectionDeniedEntirely
// proves a player-scoped connection is refused ANY access at all - there
// is no player-facing read path for a limit (ADR 0083 §6.2.3), unlike
// sb_jurisdiction_restrictions' deliberately read-open policy.
func TestSportsbookExposureLimitsRLS_PlayerScopedConnectionDeniedEntirely(t *testing.T) {
	pool := testPool(t)
	tenantID := createTestTenant(t, pool)
	limitID := seedExposureLimitForRLSTest(t, pool, tenantID)

	t.Run("read", func(t *testing.T) {
		var count int
		err := pool.WithPlayerScope(context.Background(), tenantID, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM sb_exposure_limits WHERE id = $1`, limitID).Scan(&count)
		})
		if err != nil {
			t.Fatalf("a player-scoped SELECT must not itself error: %v", err)
		}
		if count != 0 {
			t.Fatalf("expected a player-scoped connection to see ZERO exposure limit rows, got %d", count)
		}
	})

	t.Run("write", func(t *testing.T) {
		err := pool.WithPlayerScope(context.Background(), tenantID, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx, sbExposureLimitInsertSQL, tenantID, uuid.New()).Scan(&id)
		})
		if err == nil {
			t.Fatal("expected a player-scoped connection to be refused writing sb_exposure_limits")
		}
		assertRLSViolation(t, err)
	})
}

// TestSportsbookExposureLimitsRLS_OtherScopesDenied proves the remaining
// connection scopes - a bare WithoutTenant connection and a
// WithPlatformService (catalogue-sync) connection - are ALSO refused: this
// table accepts writes from exactly one scope (a genuinely tenant-scoped,
// non-player connection), unlike sb_jurisdiction_restrictions (platform-
// admin) or the five sb_* catalogue tables (the catalogue-sync service
// principal).
func TestSportsbookExposureLimitsRLS_OtherScopesDenied(t *testing.T) {
	pool := testPool(t)
	tenantID := createTestTenant(t, pool)

	t.Run("WithoutTenant", func(t *testing.T) {
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx, sbExposureLimitInsertSQL, tenantID, uuid.New()).Scan(&id)
		})
		if err == nil {
			t.Fatal("expected WithoutTenant to be refused writing sb_exposure_limits")
		}
		assertRLSViolation(t, err)
	})

	t.Run("WithPlatformService", func(t *testing.T) {
		err := pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx, sbExposureLimitInsertSQL, tenantID, uuid.New()).Scan(&id)
		})
		if err == nil {
			t.Fatal("expected WithPlatformService to be refused writing sb_exposure_limits - this table is NOT one of the five sb_* catalogue tables")
		}
		assertRLSViolation(t, err)
	})

	t.Run("WithPlatformAdmin", func(t *testing.T) {
		err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx, sbExposureLimitInsertSQL, tenantID, uuid.New()).Scan(&id)
		})
		if err == nil {
			t.Fatal("expected WithPlatformAdmin to be refused writing sb_exposure_limits - unlike sb_jurisdiction_restrictions, this table is tenant-owned, not platform-admin-owned")
		}
		assertRLSViolation(t, err)
	})
}

// TestSportsbookExposureLimitsRLS_DenyDeleteAndTruncate proves migration
// 0088's deny-delete/deny-truncate triggers - a limit is disabled, never
// deleted, and the table can never be emptied by TRUNCATE.
func TestSportsbookExposureLimitsRLS_DenyDeleteAndTruncate(t *testing.T) {
	pool := testPool(t)
	tenantID := createTestTenant(t, pool)
	limitID := seedExposureLimitForRLSTest(t, pool, tenantID)

	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM sb_exposure_limits WHERE id = $1`, limitID)
		return err
	})
	if err == nil {
		t.Fatal("expected DELETE to be refused by the deny-delete trigger")
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE sb_exposure_limits`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE to be refused by the deny-truncate trigger")
	}
}

// TestSportsbookExposureLimitsRLS_ImmutableIdentityFrozenColumns proves the
// shared catalogue_enforce_immutable_identity trigger freezes this table's
// own identity columns (including max_open_potential_payout itself - ADR
// 0083 §6.2.3: "changing a ceiling means a new row"), while
// status/reason_code stay mutable.
func TestSportsbookExposureLimitsRLS_ImmutableIdentityFrozenColumns(t *testing.T) {
	pool := testPool(t)
	tenantID := createTestTenant(t, pool)
	limitID := seedExposureLimitForRLSTest(t, pool, tenantID)

	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sb_exposure_limits SET max_open_potential_payout = 999999 WHERE id = $1`, limitID)
		return err
	})
	if err == nil {
		t.Fatal("expected max_open_potential_payout to be immutable after insert")
	}

	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sb_exposure_limits SET status = 'disabled', reason_code = 'immutability-test' WHERE id = $1`, limitID)
		return err
	})
	if err != nil {
		t.Fatalf("expected status/reason_code to remain mutable: %v", err)
	}
}
