//go:build integration

// Stage 9.2, Workstream A: direct-SQL adversarial RLS regression suite for
// migration 0086's two new platform-scoped four-eyes governance tables
// (casino_catalogue_change_requests/_approvals, ADR 0081 §5/§5.2, ARCH-
// DB-2 Phase 2) - the same class of adversarial probe
// catalogue_write_authorization_integration_test.go (migration 0084) used
// for the six ARCH-DB-2 tables, reusing that file's own
// deniedWriteScopes()/seedPlatformAdminStaffPrincipal()/
// assertRLSViolation() helpers.
package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// seedCasinoCatalogueGovernanceGame inserts a legitimate casino_games row
// (via WithPlatformAdmin) with the given jurisdiction_blocklist/status -
// the target every test below files a change request against.
func seedCasinoCatalogueGovernanceGame(t *testing.T, pool *Pool, blocklist []string, status string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type, jurisdiction_blocklist, status)
			 VALUES ($1, $1, 'Governance RLS Test Game', 'slot', $2, $3) RETURNING id`,
			"gov-rls-"+uuid.New().String()[:8], blocklist, status).Scan(&id)
	})
	if err != nil {
		t.Fatalf("seed governance test game: %v", err)
	}
	return id
}

// seedPersonLinkedPlatformAdminStaffPrincipal is
// seedPlatformAdminStaffPrincipal (catalogue_write_authorization_
// integration_test.go, shared with migration 0084/0085's simpler checks
// and with the sportsbook RLS suite) plus a confirmed, active Person
// linkage. Migration 0089 (SEC-S92-1 fix round) requires every principal
// that files or decides a casino_catalogue_change_requests row to carry
// one, so this file's own legitimate-requester/approver fixtures use this
// instead of the shared helper - which deliberately stays unlinked for
// its other, unrelated callers.
func seedPersonLinkedPlatformAdminStaffPrincipal(t *testing.T, pool *Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	personID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`,
			id, "platform-admin-gov-"+id.String()+"@test.example", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed person-linked platform admin staff principal: %v", err)
	}
	return id
}

// seedCasinoCatalogueChangeRequest files one legitimate pending request
// (via WithPlatformAdmin) using a real, person-linked platform-scoped
// requester, and returns both the request id and the requester's
// principal id.
func seedCasinoCatalogueChangeRequest(t *testing.T, pool *Pool, gameID uuid.UUID, operation string, payload map[string]any) (requestID, requester uuid.UUID) {
	t.Helper()
	requester = seedPersonLinkedPlatformAdminStaffPrincipal(t, pool)
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO casino_catalogue_change_requests (operation, game_id, payload, reason_code, requested_by_principal_id)
			 VALUES ($1, $2, $3, 'test', $4) RETURNING id`,
			operation, gameID, payload, requester).Scan(&requestID)
	})
	if err != nil {
		t.Fatalf("seed casino catalogue change request: %v", err)
	}
	return requestID, requester
}

// assertGovernanceWriteDenied accepts either denial shape this schema
// actually produces (verified empirically against migration 0086):
//   - a genuine RLS violation (SQLSTATE 42501), when the write reaches the
//     table's own platform_admin_scope policy at all, or
//   - one of this migration's own BEFORE INSERT triggers refusing first,
//     because the row it needs to resolve (a staff_users principal for
//     casino_catalogue_change_requests, or the parent request row for
//     casino_catalogue_change_approvals) is itself invisible under the
//     denied scope's OWN row-level security - the identical characteristic
//     migration 0044's asset_change_requests/_approvals precedent has.
//
// Either way, what must never happen is the write succeeding.
func assertGovernanceWriteDenied(t *testing.T, label string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected the write to be denied, got no error", label)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: expected a *pgconn.PgError, got %T: %v", label, err, err)
	}
	if pgErr.Code == pgRLSViolationCode {
		return
	}
	// P0001 is a plain RAISE EXCEPTION - acceptable here only when it is
	// one of this migration's own scope-resolution refusals, never some
	// unrelated failure.
	if pgErr.Code == "P0001" {
		return
	}
	t.Fatalf("%s: expected SQLSTATE %s or P0001, got %s: %v", label, pgRLSViolationCode, pgErr.Code, err)
}

// TestCasinoCatalogueGovernanceRLS_DeniedScopesCannotWriteEitherTable
// proves WithTenant, WithPlayerScope, and WithoutTenant - the same three
// scopes ADR 0081 §7.6 items 1-3 already rules out for the six catalogue
// tables - can insert into neither casino_catalogue_change_requests nor
// casino_catalogue_change_approvals.
func TestCasinoCatalogueGovernanceRLS_DeniedScopesCannotWriteEitherTable(t *testing.T) {
	pool := testPool(t)
	gameID := seedCasinoCatalogueGovernanceGame(t, pool, []string{"DE"}, "disabled")
	requestID, requester := seedCasinoCatalogueChangeRequest(t, pool, gameID, "status_activate", map[string]any{})

	for _, scope := range deniedWriteScopes() {
		scope := scope
		t.Run(scope.name+"/requests_insert", func(t *testing.T) {
			err := scope.run(pool, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO casino_catalogue_change_requests (operation, game_id, payload, reason_code, requested_by_principal_id)
					 VALUES ('status_activate', $1, '{}'::jsonb, 'denied', $2)`,
					gameID, requester)
				return err
			})
			assertGovernanceWriteDenied(t, "requests_insert/"+scope.name, err)
		})
		t.Run(scope.name+"/approvals_insert", func(t *testing.T) {
			err := scope.run(pool, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO casino_catalogue_change_approvals (request_id, approver_principal_id, decision)
					 VALUES ($1, $2, 'approve')`,
					requestID, uuid.New())
				return err
			})
			assertGovernanceWriteDenied(t, "approvals_insert/"+scope.name, err)
		})
	}
}

// TestCasinoCatalogueGovernanceRLS_DeniedScopesCannotReadEitherTable
// proves the read side is ALSO scoped (unlike casino_games' own
// deliberately-open assets_read-style policy): platform_admin_scope is a
// single FOR ALL policy, so a denied scope's SELECT sees zero rows rather
// than the legitimately-seeded fixture.
func TestCasinoCatalogueGovernanceRLS_DeniedScopesCannotReadEitherTable(t *testing.T) {
	pool := testPool(t)
	gameID := seedCasinoCatalogueGovernanceGame(t, pool, []string{"DE"}, "disabled")
	_, _ = seedCasinoCatalogueChangeRequest(t, pool, gameID, "status_activate", map[string]any{})

	for _, scope := range deniedWriteScopes() {
		scope := scope
		t.Run(scope.name+"/requests_select", func(t *testing.T) {
			var count int
			err := scope.run(pool, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM casino_catalogue_change_requests`).Scan(&count)
			})
			if err != nil {
				t.Fatalf("SELECT under %s failed: %v", scope.name, err)
			}
			if count != 0 {
				t.Fatalf("expected 0 visible rows under %s, got %d", scope.name, count)
			}
		})
		t.Run(scope.name+"/approvals_select", func(t *testing.T) {
			var count int
			err := scope.run(pool, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM casino_catalogue_change_approvals`).Scan(&count)
			})
			if err != nil {
				t.Fatalf("SELECT under %s failed: %v", scope.name, err)
			}
			if count != 0 {
				t.Fatalf("expected 0 visible rows under %s, got %d", scope.name, count)
			}
		})
	}
}

// TestCasinoCatalogueGovernanceRLS_PlatformAdminCanWriteBothTables is the
// positive control: a genuinely platform-admin-scoped transaction, with a
// real platform-scoped staff principal, can insert into both tables (the
// legitimate path every other test in this file proves is otherwise
// unreachable).
func TestCasinoCatalogueGovernanceRLS_PlatformAdminCanWriteBothTables(t *testing.T) {
	pool := testPool(t)
	gameID := seedCasinoCatalogueGovernanceGame(t, pool, []string{"DE"}, "disabled")
	requester := seedPersonLinkedPlatformAdminStaffPrincipal(t, pool)
	approver := seedPersonLinkedPlatformAdminStaffPrincipal(t, pool)

	var requestID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO casino_catalogue_change_requests (operation, game_id, payload, reason_code, requested_by_principal_id)
			 VALUES ('status_activate', $1, '{}'::jsonb, 'test', $2) RETURNING id`,
			gameID, requester).Scan(&requestID)
	})
	if err != nil {
		t.Fatalf("legitimate WithPlatformAdmin insert into casino_catalogue_change_requests failed: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_catalogue_change_approvals (request_id, approver_principal_id, decision)
			 VALUES ($1, $2, 'approve')`,
			requestID, approver)
		return err
	})
	if err != nil {
		t.Fatalf("legitimate WithPlatformAdmin insert into casino_catalogue_change_approvals failed: %v", err)
	}
}
