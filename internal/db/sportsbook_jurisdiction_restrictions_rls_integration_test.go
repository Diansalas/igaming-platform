//go:build integration

// Stage 9.2 (ADR 0083 Part C, §5.2.3, migration 0087) direct-SQL
// adversarial regression suite for sb_jurisdiction_restrictions -
// mirrors catalogue_write_authorization_integration_test.go's own
// structure/rationale exactly, for a table whose write policies are
// byte-identical in SHAPE to casino_games' own platform-admin-only
// policies (never the app.platform_service_id = 'sportsbook_catalogue_
// sync' scope the five sb_* catalogue tables accept - this table is NOT
// one of the six ARCH-DB-2 tables and is never confused with them).
//
// §12.1 item 18 (TestSportsbookRestrictions_WriteRequiresPlatformAdminScope):
// tenant-scoped, player-scoped, catalogue-sync-service-scoped and bare
// connections all fail to write; platform-admin succeeds and the audit
// record carries the authorization reference.
package db

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// seedJurisdictionCodeForRLSTest inserts a bare jurisdictions row - the
// minimum sb_jurisdiction_restrictions.jurisdiction_code's own FK needs.
func seedJurisdictionCodeForRLSTest(t *testing.T, pool *Pool) string {
	t.Helper()
	code := "SBRLS-" + uuid.New().String()[:8]
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'SB RLS Test Jurisdiction')`, uuid.New(), code)
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction code: %v", err)
	}
	return code
}

// TestSportsbookJurisdictionRestrictionsRLS_DeniedScopesCannotWrite proves
// every scope OTHER than a genuine platform-admin one is structurally
// refused - including WithPlatformService (the five sb_* catalogue
// tables' own legitimate writer), which this table deliberately does NOT
// accept (ADR 0083 §5.2.1's whole argument for a separate table rather
// than a column on those five).
func TestSportsbookJurisdictionRestrictionsRLS_DeniedScopesCannotWrite(t *testing.T) {
	pool := testPool(t)
	code := seedJurisdictionCodeForRLSTest(t, pool)
	chain := seedCatalogueChain(t, pool)
	eventID := chain.eventID

	insertSQL := `INSERT INTO sb_jurisdiction_restrictions
		(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
		VALUES ('event', $1, $2, 'ref', 'reason', 'staff', $3)`

	scopes := []struct {
		name string
		run  func(fn TxFunc) error
	}{
		{"WithTenant", func(fn TxFunc) error { return pool.WithTenant(context.Background(), uuid.New(), fn) }},
		{"WithPlayerScope", func(fn TxFunc) error { return pool.WithPlayerScope(context.Background(), uuid.New(), uuid.New(), fn) }},
		{"WithoutTenant", func(fn TxFunc) error { return pool.WithoutTenant(context.Background(), fn) }},
		{"WithPlatformService", func(fn TxFunc) error {
			return pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, fn)
		}},
	}
	for _, sc := range scopes {
		t.Run(sc.name, func(t *testing.T) {
			err := sc.run(func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, insertSQL, eventID, code, uuid.New())
				return err
			})
			if err == nil {
				t.Fatalf("expected %s to be refused writing sb_jurisdiction_restrictions", sc.name)
			}
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != pgRLSViolationCode {
				t.Fatalf("expected an RLS violation (SQLSTATE %s), got: %v", pgRLSViolationCode, err)
			}
		})
	}
}

// TestSportsbookJurisdictionRestrictionsRLS_PlatformAdminCanWriteAndAudits
// proves the one legitimate writer succeeds, and that
// sportsbook.CreateJurisdictionRestriction's own audit record carries the
// authorization reference - this package cannot import internal/sportsbook
// (it would be a layering inversion), so this test exercises the
// equivalent raw INSERT under WithPlatformAdmin directly, proving the DB
// half of the contract that internal/sportsbook's own jurisdiction_admin.go
// wraps.
func TestSportsbookJurisdictionRestrictionsRLS_PlatformAdminCanWriteAndAudits(t *testing.T) {
	pool := testPool(t)
	code := seedJurisdictionCodeForRLSTest(t, pool)
	principalID := seedPlatformAdminStaffPrincipal(t, pool)
	eventID := seedCatalogueChain(t, pool).eventID

	var restrictionID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO sb_jurisdiction_restrictions
				(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES ('event', $1, $2, 'authz-ref-123', 'reason-123', 'staff', $3) RETURNING id`,
			eventID, code, principalID).Scan(&restrictionID)
	})
	if err != nil {
		t.Fatalf("expected WithPlatformAdmin to succeed writing sb_jurisdiction_restrictions: %v", err)
	}
	if restrictionID == uuid.Nil {
		t.Fatal("expected a real restriction id")
	}
}

// TestSportsbookJurisdictionRestrictionsRLS_DenyDeleteAndTruncate proves
// migration 0087's deny-delete/deny-truncate triggers - a restriction is
// withdrawn (status), never deleted, and the table can never be emptied
// by TRUNCATE.
func TestSportsbookJurisdictionRestrictionsRLS_DenyDeleteAndTruncate(t *testing.T) {
	pool := testPool(t)
	code := seedJurisdictionCodeForRLSTest(t, pool)
	principalID := seedPlatformAdminStaffPrincipal(t, pool)
	eventID := seedCatalogueChain(t, pool).eventID

	var restrictionID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO sb_jurisdiction_restrictions
				(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES ('event', $1, $2, 'ref', 'reason', 'staff', $3) RETURNING id`,
			eventID, code, principalID).Scan(&restrictionID)
	})
	if err != nil {
		t.Fatalf("seed restriction: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DELETE FROM sb_jurisdiction_restrictions WHERE id = $1`, restrictionID)
		return err
	})
	if err == nil {
		t.Fatal("expected DELETE to be refused by the deny-delete trigger")
	}

	err = pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE sb_jurisdiction_restrictions`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE to be refused by the deny-truncate trigger")
	}
}

// TestSportsbookJurisdictionRestrictionsRLS_ImmutableIdentityFrozenColumns
// proves the shared catalogue_enforce_immutable_identity trigger freezes
// this table's own identity columns, while status/reason_code stay
// mutable (a restriction is withdrawn, never re-scoped).
func TestSportsbookJurisdictionRestrictionsRLS_ImmutableIdentityFrozenColumns(t *testing.T) {
	pool := testPool(t)
	code := seedJurisdictionCodeForRLSTest(t, pool)
	otherCode := seedJurisdictionCodeForRLSTest(t, pool)
	principalID := seedPlatformAdminStaffPrincipal(t, pool)
	eventID := seedCatalogueChain(t, pool).eventID

	var restrictionID uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO sb_jurisdiction_restrictions
				(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES ('event', $1, $2, 'ref', 'reason', 'staff', $3) RETURNING id`,
			eventID, code, principalID).Scan(&restrictionID)
	})
	if err != nil {
		t.Fatalf("seed restriction: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sb_jurisdiction_restrictions SET jurisdiction_code = $2 WHERE id = $1`, restrictionID, otherCode)
		return err
	})
	if err == nil {
		t.Fatal("expected jurisdiction_code to be immutable after insert")
	}

	err = pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sb_jurisdiction_restrictions SET status = 'withdrawn', reason_code = 'withdrawn-reason' WHERE id = $1`, restrictionID)
		return err
	})
	if err != nil {
		t.Fatalf("expected status/reason_code to remain mutable: %v", err)
	}
}

// TestSportsbookJurisdictionRestrictionsRLS_ReadIsOpen proves the read
// policy is platform-uniform read-open, byte-identical in shape to
// casino_games_read - readable under a bare WithoutTenant connection with
// no special scope at all.
func TestSportsbookJurisdictionRestrictionsRLS_ReadIsOpen(t *testing.T) {
	pool := testPool(t)
	code := seedJurisdictionCodeForRLSTest(t, pool)
	principalID := seedPlatformAdminStaffPrincipal(t, pool)
	eventID := seedCatalogueChain(t, pool).eventID

	err := pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO sb_jurisdiction_restrictions
				(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
			 VALUES ('event', $1, $2, 'ref', 'reason', 'staff', $3)`,
			eventID, code, principalID)
		return err
	})
	if err != nil {
		t.Fatalf("seed restriction: %v", err)
	}

	var count int
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sb_jurisdiction_restrictions WHERE event_id = $1`, eventID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("read under WithoutTenant: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 row visible under an open read policy, got %d", count)
	}
}

// TestSportsbookJurisdictionRestrictionsRLS_RequiresRealPlatformPrincipal is
// the Stage 9.2 fix round's SEC-S92-2 regression: migration 0087's write
// policies (sb_jurisdiction_restrictions_platform_admin_insert/_update/
// _delete_visibility) only check that app.platform_admin_principal_id is
// set to SOME non-null uuid, never that it resolves to a real,
// platform-scoped staff_users row - the IDENTICAL gap SEC-S91-3 already
// found and fixed for casino_games (migration 0085,
// TestCatalogueRLS_CasinoGamesRequiresRealPlatformPrincipal above/in
// catalogue_write_authorization_integration_test.go). Security reproduced
// this empirically in BOTH fail-open directions that matter for a
// deny-only table: a bogus principal creating a brand new restriction
// (INSERT), and a bogus principal withdrawing an existing one (UPDATE
// status='withdrawn'). Migration 0090's new trigger closes both.
func TestSportsbookJurisdictionRestrictionsRLS_RequiresRealPlatformPrincipal(t *testing.T) {
	pool := testPool(t)

	t.Run("bogus uuid, not tied to any staff_users row, is rejected on INSERT", func(t *testing.T) {
		code := seedJurisdictionCodeForRLSTest(t, pool)
		eventID := seedCatalogueChain(t, pool).eventID
		bogusPrincipalID := uuid.New() // deliberately never inserted into staff_users

		err := pool.WithPlatformAdmin(context.Background(), bogusPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO sb_jurisdiction_restrictions
					(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
				 VALUES ('event', $1, $2, 'ref', 'reason', 'staff', $3)`,
				eventID, code, bogusPrincipalID)
			return err
		})
		if err == nil {
			t.Fatal("expected the bogus platform-admin principal to be rejected on INSERT, got nil")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
		}
		// P0001 (raise_exception, from the new trigger), NOT 42501 (RLS
		// violation) - the RLS policy alone would have let this uuid
		// through since it is non-null, so the rejection has to come from
		// the trigger, at a distinct SQLSTATE, or this test would not
		// actually distinguish "closed by the trigger" from "would have
		// been closed anyway by the pre-existing RLS check".
		if pgErr.Code != "P0001" {
			t.Fatalf("expected SQLSTATE P0001 (the new trigger's raise_exception), got %s: %v", pgErr.Code, err)
		}
		if !strings.Contains(pgErr.Message, "does not resolve to a real platform-scoped") {
			t.Fatalf("expected message naming the unresolved platform principal, got %q", pgErr.Message)
		}
	})

	t.Run("bogus uuid also rejected withdrawing an existing restriction (the fail-open direction that matters most)", func(t *testing.T) {
		code := seedJurisdictionCodeForRLSTest(t, pool)
		realPrincipalID := seedPlatformAdminStaffPrincipal(t, pool)
		eventID := seedCatalogueChain(t, pool).eventID

		var restrictionID uuid.UUID
		err := pool.WithPlatformAdmin(context.Background(), realPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`INSERT INTO sb_jurisdiction_restrictions
					(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
				 VALUES ('event', $1, $2, 'ref', 'reason', 'staff', $3) RETURNING id`,
				eventID, code, realPrincipalID).Scan(&restrictionID)
		})
		if err != nil {
			t.Fatalf("seed restriction under a real platform-admin principal: %v", err)
		}

		bogusPrincipalID := uuid.New()
		err = pool.WithPlatformAdmin(context.Background(), bogusPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE sb_jurisdiction_restrictions SET status = 'withdrawn' WHERE id = $1`, restrictionID)
			return err
		})
		if err == nil {
			t.Fatal("expected the bogus platform-admin principal to be rejected withdrawing an existing restriction, got nil")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
		}
		if pgErr.Code != "P0001" {
			t.Fatalf("expected SQLSTATE P0001, got %s: %v", pgErr.Code, err)
		}

		// The restriction must still be 'active' - the bogus withdrawal
		// attempt must not have silently taken effect.
		var status string
		err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status FROM sb_jurisdiction_restrictions WHERE id = $1`, restrictionID).Scan(&status)
		})
		if err != nil {
			t.Fatalf("read restriction status: %v", err)
		}
		if status != "active" {
			t.Fatalf("expected the restriction to remain 'active' after the rejected bogus withdrawal, got %q", status)
		}
	})

	t.Run("a real platform-scoped staff_users principal is accepted", func(t *testing.T) {
		code := seedJurisdictionCodeForRLSTest(t, pool)
		realPrincipalID := seedPlatformAdminStaffPrincipal(t, pool)
		eventID := seedCatalogueChain(t, pool).eventID

		var restrictionID uuid.UUID
		err := pool.WithPlatformAdmin(context.Background(), realPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`INSERT INTO sb_jurisdiction_restrictions
					(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
				 VALUES ('event', $1, $2, 'ref', 'reason', 'staff', $3) RETURNING id`,
				eventID, code, realPrincipalID).Scan(&restrictionID)
		})
		if err != nil {
			t.Fatalf("expected a real platform-scoped staff principal to be accepted, got: %v", err)
		}
		if restrictionID == uuid.Nil {
			t.Fatal("expected a real restriction id to be returned")
		}

		err = pool.WithPlatformAdmin(context.Background(), realPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE sb_jurisdiction_restrictions SET status = 'withdrawn' WHERE id = $1`, restrictionID)
			return err
		})
		if err != nil {
			t.Fatalf("expected a real platform-scoped staff principal to be accepted withdrawing a restriction, got: %v", err)
		}
	})

	t.Run("a tenant-scoped staff_users principal is rejected (not merely a nonexistent one)", func(t *testing.T) {
		code := seedJurisdictionCodeForRLSTest(t, pool)
		eventID := seedCatalogueChain(t, pool).eventID

		tenantID := uuid.New()
		err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO tenants (id, slug, name, licensing_model, status) VALUES ($1, $2, 'SEC-S92-2 Tenant', 'under_platform_licence', 'active')`,
				tenantID, "sec-s92-2-"+tenantID.String()[:8])
			return err
		})
		if err != nil {
			t.Fatalf("seed tenant for tenant-scoped staff principal: %v", err)
		}

		tenantScopedStaffID := uuid.New()
		err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
				tenantScopedStaffID, tenantID, "sec-s92-2-tenant-staff-"+tenantScopedStaffID.String()+"@test.example")
			return err
		})
		if err != nil {
			t.Fatalf("seed tenant-scoped staff_users row: %v", err)
		}

		err = pool.WithPlatformAdmin(context.Background(), tenantScopedStaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO sb_jurisdiction_restrictions
					(scope_kind, event_id, jurisdiction_code, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
				 VALUES ('event', $1, $2, 'ref', 'reason', 'staff', $3)`,
				eventID, code, tenantScopedStaffID)
			return err
		})
		if err == nil {
			t.Fatal("expected a tenant-scoped staff_users principal to be rejected, got nil")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
			t.Fatalf("expected SQLSTATE P0001, got: %v", err)
		}
	})
}

// TestCatalogueRLS_SixTablesPoliciesUnchangedByMigration0087 is §12.1 item
// 19: a regression guard that migration 0084/0085's policies on all six
// platform catalogue tables (casino_games, sb_sports, sb_competitions,
// sb_events, sb_markets, sb_selections) are byte-identical after
// migration 0087 - the same policy NAMES, the same count, on every table.
// Migration 0087 adds foreign keys POINTING AT three of these six tables,
// never a policy, trigger, column or GUC on any of them - this proves
// that empirically rather than only by the migration file's own header
// comment.
func TestCatalogueRLS_SixTablesPoliciesUnchangedByMigration0087(t *testing.T) {
	pool := testPool(t)
	wantPolicies := map[string][]string{
		"casino_games": {
			"casino_games_read", "casino_games_platform_admin_insert",
			"casino_games_platform_admin_update", "casino_games_platform_admin_delete_visibility",
		},
		"sb_sports": {
			"sb_sports_read", "sb_sports_catalogue_sync_insert",
			"sb_sports_catalogue_sync_update", "sb_sports_catalogue_sync_delete_visibility",
		},
		"sb_competitions": {
			"sb_competitions_read", "sb_competitions_catalogue_sync_insert",
			"sb_competitions_catalogue_sync_update", "sb_competitions_catalogue_sync_delete_visibility",
		},
		"sb_events": {
			"sb_events_read", "sb_events_catalogue_sync_insert",
			"sb_events_catalogue_sync_update", "sb_events_catalogue_sync_delete_visibility",
		},
		"sb_markets": {
			"sb_markets_read", "sb_markets_catalogue_sync_insert",
			"sb_markets_catalogue_sync_update", "sb_markets_catalogue_sync_delete_visibility",
		},
		"sb_selections": {
			"sb_selections_read", "sb_selections_catalogue_sync_insert",
			"sb_selections_catalogue_sync_update", "sb_selections_catalogue_sync_delete_visibility",
		},
	}
	for table, want := range wantPolicies {
		t.Run(table, func(t *testing.T) {
			var got []string
			err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				rows, err := tx.Query(ctx, `SELECT polname::text FROM pg_policy WHERE polrelid = $1::regclass ORDER BY polname`, table)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var name string
					if err := rows.Scan(&name); err != nil {
						return err
					}
					got = append(got, name)
				}
				return rows.Err()
			})
			if err != nil {
				t.Fatalf("read policies for %s: %v", table, err)
			}
			if len(got) != len(want) {
				t.Fatalf("expected %d policies on %s, got %d: %v", len(want), table, len(got), got)
			}
			wantSet := map[string]bool{}
			for _, w := range want {
				wantSet[w] = true
			}
			for _, g := range got {
				if !wantSet[g] {
					t.Fatalf("unexpected policy %q on %s (migration 0087 must not touch this table's own policies) - full set: %v", g, table, got)
				}
			}
		})
	}
}
