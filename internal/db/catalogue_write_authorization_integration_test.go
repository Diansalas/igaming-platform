//go:build integration

// Direct-SQL adversarial regression suite for migration 0084 (ADR 0081,
// ARCH-DB-2's ruling): the six platform-wide catalogue tables
// (casino_games, sb_sports, sb_competitions, sb_events, sb_markets,
// sb_selections) gained ENABLE+FORCE row-level security, two write
// identities (app.platform_admin_principal_id for casino_games,
// app.platform_service_id = 'sportsbook_catalogue_sync' for the five
// sb_* tables), immutable-identity triggers, and deny-DELETE/deny-
// TRUNCATE triggers. Every assertion here connects under a real scope and
// issues real SQL directly against the six tables - not merely a Go-level
// unit test of the wrapper functions - reproducing ADR 0081 §7.6's
// eleven-item invariant list (items covered here: 1-9; idempotent
// SyncCatalogue and end-to-end bet placement, items 10-11, are covered by
// internal/sportsbook's and internal/httpserver's own integration suites,
// re-scoped by this same change - see this package's
// platform_service_test.go for the Go-level WithPlatformService
// contract tests).
package db

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// catalogueChain is a full, valid sport -> competition -> event -> market
// -> selection row chain, seeded once per test via the legitimate writer
// scope (WithPlatformService), plus one legitimately-seeded casino_games
// row (via WithPlatformAdmin) - the fixture every adversarial attempt
// below targets.
type catalogueChain struct {
	sportID, competitionID, eventID, marketID, selectionID uuid.UUID
	gameID                                                 uuid.UUID
}

func seedCatalogueChain(t *testing.T, pool *Pool) catalogueChain {
	t.Helper()
	ref := uuid.New().String()[:8]
	var c catalogueChain

	err := pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_sports (external_ref, code, name) VALUES ($1, $2, 'ARCH-DB-2 Sport') RETURNING id`,
			"archdb2-sport-"+ref, "archdb2-sport-"+ref).Scan(&c.sportID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_competitions (sport_id, external_ref, name) VALUES ($1, $2, 'ARCH-DB-2 Competition') RETURNING id`,
			c.sportID, "archdb2-comp-"+ref).Scan(&c.competitionID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_events (competition_id, external_ref, name, start_time) VALUES ($1, $2, 'ARCH-DB-2 Event', $3) RETURNING id`,
			c.competitionID, "archdb2-event-"+ref, time.Now().Add(48*time.Hour)).Scan(&c.eventID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_markets (event_id, external_ref, name) VALUES ($1, $2, 'ARCH-DB-2 Market') RETURNING id`,
			c.eventID, "archdb2-market-"+ref).Scan(&c.marketID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator)
			 VALUES ($1, $2, 'ARCH-DB-2 Selection', 200, 100) RETURNING id`,
			c.marketID, "archdb2-sel-"+ref).Scan(&c.selectionID)
	})
	if err != nil {
		t.Fatalf("seed catalogue chain via WithPlatformService: %v", err)
	}

	err = pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type) VALUES ($1, $2, 'ARCH-DB-2 Game', 'slot') RETURNING id`,
			"archdb2-provider-"+ref, "archdb2-game-"+ref).Scan(&c.gameID)
	})
	if err != nil {
		t.Fatalf("seed catalogue chain casino_games via WithPlatformAdmin: %v", err)
	}
	return c
}

// seedPlatformAdminStaffPrincipal inserts a genuine platform-scoped
// (tenant_id IS NULL) staff_users row and returns its id. Migration 0085
// (SEC-S91-3) added a trigger requiring app.platform_admin_principal_id
// to resolve to a REAL staff_users row before casino_games can be
// written, so WithPlatformAdmin(ctx, uuid.New(), ...) alone no longer
// suffices for a LEGITIMATE casino_games write in this suite - a random,
// unregistered uuid is now exactly the adversarial case migration 0085
// exists to reject (see TestCatalogueRLS_CasinoGamesRequiresRealPlatformPrincipal
// below). password_hash is a dummy literal - these principals are never
// used to log in, only to satisfy the trigger's staff_users lookup.
func seedPlatformAdminStaffPrincipal(t *testing.T, pool *Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			id, "platform-admin-"+id.String()+"@test.example")
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin staff principal: %v", err)
	}
	return id
}

// deniedWriteScope is one connection scope that migration 0084's write
// policies must structurally refuse for ALL six tables - this is the
// "proves the fix is not theatre" set (ADR 0081 §7.6 item 3): every one
// of these is a scope some pre-existing, legitimate platform read path
// already uses.
type deniedWriteScope struct {
	name string
	run  func(pool *Pool, fn TxFunc) error
}

func deniedWriteScopes() []deniedWriteScope {
	return []deniedWriteScope{
		{"WithTenant", func(pool *Pool, fn TxFunc) error {
			return pool.WithTenant(context.Background(), uuid.New(), fn)
		}},
		{"WithPlayerScope", func(pool *Pool, fn TxFunc) error {
			return pool.WithPlayerScope(context.Background(), uuid.New(), uuid.New(), fn)
		}},
		{"WithoutTenant", func(pool *Pool, fn TxFunc) error {
			return pool.WithoutTenant(context.Background(), fn)
		}},
	}
}

// assertRowsAffectedZero fails the test unless tag reports zero rows
// affected - the UPDATE-side counterpart to assertRLSViolation: a denied
// scope's UPDATE is not necessarily an ERROR (RLS's USING clause simply
// makes the target row invisible, so ordinary UPDATE semantics report
// "0 rows matched"), but it must never silently take effect.
func assertRowsAffectedZero(t *testing.T, label string, tag pgconn.CommandTag, err error) {
	t.Helper()
	if err != nil {
		// An error is also an acceptable "did not take effect" outcome
		// (e.g. WITH CHECK failing on a matched row) - the RLS-violation
		// SQLSTATE is asserted where the shape of the policy makes that
		// the guaranteed path (INSERT); for UPDATE we accept either.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == pgRLSViolationCode {
			return
		}
		t.Fatalf("%s: unexpected error (not an RLS violation): %v", label, err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("%s: expected 0 rows affected for a denied-scope UPDATE, got %d", label, tag.RowsAffected())
	}
}

// --- Items 1-3: write scope matrix ---

// TestCatalogueRLS_DeniedScopesCannotWriteAnySixTables is ADR 0081 §7.6
// items 1-3: WithTenant, WithPlayerScope, and WithoutTenant - the exact
// scope both production writers used BEFORE migration 0084 - can insert
// or update none of the six tables.
func TestCatalogueRLS_DeniedScopesCannotWriteAnySixTables(t *testing.T) {
	pool := testPool(t)
	c := seedCatalogueChain(t, pool)

	cases := []struct {
		table     string
		insertSQL string
		insertRef string
		updateSQL string
		targetID  uuid.UUID
	}{
		{"casino_games",
			`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type) VALUES ($1, $1, 'Denied Insert', 'slot')`,
			uuid.New().String(),
			`UPDATE casino_games SET name = 'Denied Update' WHERE id = $1`, c.gameID},
		{"sb_sports",
			`INSERT INTO sb_sports (external_ref, code, name) VALUES ($1, $1, 'Denied Insert')`,
			uuid.New().String(),
			`UPDATE sb_sports SET name = 'Denied Update' WHERE id = $1`, c.sportID},
		{"sb_competitions",
			fmt.Sprintf(`INSERT INTO sb_competitions (sport_id, external_ref, name) VALUES ('%s', $1, 'Denied Insert')`, c.sportID),
			uuid.New().String(),
			`UPDATE sb_competitions SET name = 'Denied Update' WHERE id = $1`, c.competitionID},
		{"sb_events",
			fmt.Sprintf(`INSERT INTO sb_events (competition_id, external_ref, name, start_time) VALUES ('%s', $1, 'Denied Insert', now() + interval '1 day')`, c.competitionID),
			uuid.New().String(),
			`UPDATE sb_events SET name = 'Denied Update' WHERE id = $1`, c.eventID},
		{"sb_markets",
			fmt.Sprintf(`INSERT INTO sb_markets (event_id, external_ref, name) VALUES ('%s', $1, 'Denied Insert')`, c.eventID),
			uuid.New().String(),
			`UPDATE sb_markets SET name = 'Denied Update' WHERE id = $1`, c.marketID},
		{"sb_selections",
			fmt.Sprintf(`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator) VALUES ('%s', $1, 'Denied Insert', 100, 100)`, c.marketID),
			uuid.New().String(),
			`UPDATE sb_selections SET name = 'Denied Update' WHERE id = $1`, c.selectionID},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.table, func(t *testing.T) {
			for _, scope := range deniedWriteScopes() {
				scope := scope
				t.Run(scope.name+"/INSERT", func(t *testing.T) {
					err := scope.run(pool, func(ctx context.Context, tx pgx.Tx) error {
						_, err := tx.Exec(ctx, tc.insertSQL, tc.insertRef)
						return err
					})
					assertRLSViolation(t, err)
				})
				t.Run(scope.name+"/UPDATE", func(t *testing.T) {
					var tag pgconn.CommandTag
					err := scope.run(pool, func(ctx context.Context, tx pgx.Tx) error {
						var innerErr error
						tag, innerErr = tx.Exec(ctx, tc.updateSQL, tc.targetID)
						return innerErr
					})
					assertRowsAffectedZero(t, tc.table+"/"+scope.name, tag, err)
				})
			}
		})
	}
}

// TestCatalogueRLS_PlatformAdminWritesCasinoGamesOnlyNotSportsbook is ADR
// 0081 §7.6 item 4: WithPlatformAdmin can write casino_games and cannot
// write any sb_* table (the two identities are deliberately NOT
// interchangeable, per ADR 0081 §3.3's "not merely 'is set'" string-
// equality predicate on the sb_* side, and the complete absence of a
// platform-admin write policy on any sb_* table).
func TestCatalogueRLS_PlatformAdminWritesCasinoGamesOnlyNotSportsbook(t *testing.T) {
	pool := testPool(t)
	c := seedCatalogueChain(t, pool)
	ref := uuid.New().String()

	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type) VALUES ($1, $1, 'Admin Insert', 'slot')`,
			ref)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected 1 row inserted, got %d", tag.RowsAffected())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("WithPlatformAdmin failed to write casino_games: %v", err)
	}

	sbCases := []struct {
		table     string
		insertSQL string
	}{
		{"sb_sports", `INSERT INTO sb_sports (external_ref, code, name) VALUES ($1, $1, 'Admin Insert')`},
		{"sb_competitions", fmt.Sprintf(`INSERT INTO sb_competitions (sport_id, external_ref, name) VALUES ('%s', $1, 'Admin Insert')`, c.sportID)},
		{"sb_events", fmt.Sprintf(`INSERT INTO sb_events (competition_id, external_ref, name, start_time) VALUES ('%s', $1, 'Admin Insert', now() + interval '1 day')`, c.competitionID)},
		{"sb_markets", fmt.Sprintf(`INSERT INTO sb_markets (event_id, external_ref, name) VALUES ('%s', $1, 'Admin Insert')`, c.eventID)},
		{"sb_selections", fmt.Sprintf(`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator) VALUES ('%s', $1, 'Admin Insert', 100, 100)`, c.marketID)},
	}
	for _, tc := range sbCases {
		tc := tc
		t.Run(tc.table, func(t *testing.T) {
			err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, tc.insertSQL, uuid.New().String())
				return err
			})
			assertRLSViolation(t, err)
		})
	}
}

// TestCatalogueRLS_PlatformServiceWritesSportsbookOnlyNotCasinoGames is
// ADR 0081 §7.6 item 5: WithPlatformService(ServiceSportsbookCatalogueSync)
// can write all five sb_* tables and cannot write casino_games.
func TestCatalogueRLS_PlatformServiceWritesSportsbookOnlyNotCasinoGames(t *testing.T) {
	pool := testPool(t)
	ref := uuid.New().String()[:8]

	err := pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		var sportID, compID, eventID, marketID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_sports (external_ref, code, name) VALUES ($1, $1, 'Service Insert') RETURNING id`,
			"svc-sport-"+ref).Scan(&sportID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_competitions (sport_id, external_ref, name) VALUES ($1, $2, 'Service Insert') RETURNING id`,
			sportID, "svc-comp-"+ref).Scan(&compID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_events (competition_id, external_ref, name, start_time) VALUES ($1, $2, 'Service Insert', now() + interval '1 day') RETURNING id`,
			compID, "svc-event-"+ref).Scan(&eventID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_markets (event_id, external_ref, name) VALUES ($1, $2, 'Service Insert') RETURNING id`,
			eventID, "svc-market-"+ref).Scan(&marketID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator) VALUES ($1, $2, 'Service Insert', 100, 100)`,
			marketID, "svc-sel-"+ref)
		return err
	})
	if err != nil {
		t.Fatalf("WithPlatformService failed to write the sb_* chain: %v", err)
	}

	err = pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type) VALUES ($1, $1, 'Service Insert', 'slot')`,
			uuid.New().String())
		return err
	})
	assertRLSViolation(t, err)
}

// --- Item 6: SELECT stays open for every scope ---

// TestCatalogueRLS_SelectStaysOpenForEveryScope is ADR 0081 §7.6 item 6:
// every scope, including WithoutTenant, can still SELECT all six tables -
// the read side is deliberately unaffected by this migration.
func TestCatalogueRLS_SelectStaysOpenForEveryScope(t *testing.T) {
	pool := testPool(t)
	seedCatalogueChain(t, pool)

	tables := []string{"casino_games", "sb_sports", "sb_competitions", "sb_events", "sb_markets", "sb_selections"}
	scopes := append(deniedWriteScopes(),
		deniedWriteScope{"WithPlatformAdmin", func(pool *Pool, fn TxFunc) error {
			return pool.WithPlatformAdmin(context.Background(), uuid.New(), fn)
		}},
		deniedWriteScope{"WithPlatformService", func(pool *Pool, fn TxFunc) error {
			return pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, fn)
		}},
	)

	for _, table := range tables {
		table := table
		for _, scope := range scopes {
			scope := scope
			t.Run(table+"/"+scope.name, func(t *testing.T) {
				var count int
				err := scope.run(pool, func(ctx context.Context, tx pgx.Tx) error {
					return tx.QueryRow(ctx, `SELECT count(*) FROM `+table).Scan(&count)
				})
				if err != nil {
					t.Fatalf("SELECT under %s failed: %v", scope.name, err)
				}
				if count < 1 {
					t.Errorf("SELECT under %s returned %d rows, expected at least the seeded fixture", scope.name, count)
				}
			})
		}
	}
}

// --- Item 7: WithPlatformService rejects an unknown service before opening a tx ---

// TestCatalogueRLS_WithPlatformServiceRejectsUnknownServiceBeforeTx is ADR
// 0081 §7.6 item 7: an unknown/attacker-influenced service string is
// rejected before any transaction is opened - fn must never be invoked.
func TestCatalogueRLS_WithPlatformServiceRejectsUnknownServiceBeforeTx(t *testing.T) {
	pool := testPool(t)

	called := false
	err := pool.WithPlatformService(context.Background(), PlatformService("not_a_real_service"), func(ctx context.Context, tx pgx.Tx) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("expected an error for an unknown service identity, got nil")
	}
	if called {
		t.Fatal("fn was invoked despite an unknown service identity - a transaction was opened when it must not have been")
	}
	wantSubstr := `db: WithPlatformService called with unknown service identity "not_a_real_service"`
	if err.Error() != wantSubstr {
		t.Fatalf("expected error %q, got %q", wantSubstr, err.Error())
	}

	called = false
	err = pool.WithPlatformService(context.Background(), "", func(ctx context.Context, tx pgx.Tx) error {
		called = true
		return nil
	})
	if err == nil {
		t.Fatal("expected an error for an empty service identity, got nil")
	}
	if called {
		t.Fatal("fn was invoked despite an empty service identity")
	}
	if err.Error() != "db: WithPlatformService called with empty service identity" {
		t.Fatalf("unexpected error message: %q", err.Error())
	}
}

// --- Item 8: immutable columns raise on genuine change, not on a same-value rewrite ---

func TestCatalogueRLS_ImmutableColumnsRaiseOnGenuineChangeOnly(t *testing.T) {
	pool := testPool(t)
	c := seedCatalogueChain(t, pool)

	t.Run("casino_games.provider_id", func(t *testing.T) {
		principalID := seedPlatformAdminStaffPrincipal(t, pool)
		err := pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE casino_games SET provider_id = 'a-different-provider' WHERE id = $1`, c.gameID)
			return err
		})
		assertImmutableViolation(t, err, "provider_id")

		err = pool.WithPlatformAdmin(context.Background(), principalID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE casino_games SET provider_id = provider_id, name = 'Same-value rewrite' WHERE id = $1`, c.gameID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expected 1 row updated, got %d", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("a same-value rewrite of provider_id must succeed, got: %v", err)
		}
	})

	t.Run("sb_events.competition_id", func(t *testing.T) {
		otherChain := seedCatalogueChain(t, pool)
		err := pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE sb_events SET competition_id = $2 WHERE id = $1`, c.eventID, otherChain.competitionID)
			return err
		})
		assertImmutableViolation(t, err, "competition_id")

		err = pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE sb_events SET competition_id = competition_id, name = 'Same-value rewrite' WHERE id = $1`, c.eventID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expected 1 row updated, got %d", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("a same-value rewrite of competition_id must succeed, got: %v", err)
		}
	})

	t.Run("sb_selections.external_ref", func(t *testing.T) {
		err := pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE sb_selections SET external_ref = $2 WHERE id = $1`, c.selectionID, uuid.New().String())
			return err
		})
		assertImmutableViolation(t, err, "external_ref")
	})

	t.Run("sb_selections.odds_numerator is deliberately mutable", func(t *testing.T) {
		err := pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE sb_selections SET odds_numerator = 999 WHERE id = $1`, c.selectionID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expected 1 row updated, got %d", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("odds_numerator must remain mutable (ADR 0081 §2.5.2), got: %v", err)
		}
	})
}

func assertImmutableViolation(t *testing.T, err error, wantColumnSubstr string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an immutable-identity violation naming %q, got nil", wantColumnSubstr)
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != "P0001" {
		t.Fatalf("expected SQLSTATE P0001 (raise_exception), got %s: %v", pgErr.Code, err)
	}
	// DELETE/TRUNCATE refusals name the operation (TG_OP), not a column -
	// "immutable after creation" only applies to the identity-column path.
	wantAlsoSubstr := "immutable after creation"
	if wantColumnSubstr == "DELETE" || wantColumnSubstr == "TRUNCATE" {
		wantAlsoSubstr = "is not permitted"
	}
	if !strings.Contains(pgErr.Message, wantColumnSubstr) || !strings.Contains(pgErr.Message, wantAlsoSubstr) {
		t.Fatalf("expected message to mention %q and %q, got %q", wantColumnSubstr, wantAlsoSubstr, pgErr.Message)
	}
}

// --- Item 9: DELETE and TRUNCATE are refused loudly ---
//
// The TRUNCATE subtests used to live here too, against the shared
// TEST_DATABASE_URL database. CI run #331 (docs/plans/stage-10.3-planning/
// 08-ci-331-lock-contention.md) failed
// casino_games_TRUNCATE intermittently: TRUNCATE casino_games CASCADE
// takes ACCESS EXCLUSIVE locks on casino_games and every table that
// (transitively, via CASCADE) references it - casino_game_availability,
// casino_launch_sessions, risk_rules, casino_provider_rounds,
// casino_catalogue_change_requests for casino_games; sb_competitions etc.
// for sb_sports. On the shared database, other packages' integration
// tests (internal/casino, internal/httpserver, ...) run concurrently
// against those same tables. A repro
// (docs/plans/stage-10.3-planning/evidence/ci-331-truncate-lock-repro.txt)
// confirmed this reliably reproduces a ~1.00s failure - matching CI run
// #331's reported duration - via Postgres's own deadlock detector
// (SQLSTATE 40P01, deadlock_timeout=1s, unmodified default; NOT a
// lock_timeout, which is not configured anywhere in this codebase) once
// another session holds a lock on a referencing table and later also
// waits on casino_games itself. That is a real SQLSTATE the deny-truncate
// trigger never gets a chance to raise, so assertImmutableViolation
// (which requires P0001) correctly fails - the trigger did not get to
// run, a lock conflict aborted the statement first.
//
// TestCatalogueRLS_TruncateIsRefusedLoudly (below) proves the same
// invariant on an isolated scratch database (internal/testsupport/
// scratchdb, full migration chain applied) that no other package's test
// can ever hold a lock on, removing the exposure entirely rather than
// masking it with a retry or a longer timeout.
func TestCatalogueRLS_DeleteAndTruncateAreRefusedLoudly(t *testing.T) {
	pool := testPool(t)
	c := seedCatalogueChain(t, pool)

	t.Run("casino_games DELETE", func(t *testing.T) {
		err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM casino_games WHERE id = $1`, c.gameID)
			return err
		})
		assertImmutableViolation(t, err, "DELETE")
	})

	t.Run("sb_selections DELETE", func(t *testing.T) {
		err := pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DELETE FROM sb_selections WHERE id = $1`, c.selectionID)
			return err
		})
		assertImmutableViolation(t, err, "DELETE")
	})
}

// TestCatalogueRLS_TruncateIsRefusedLoudly is
// TestCatalogueRLS_DeleteAndTruncateAreRefusedLoudly's TRUNCATE coverage,
// moved (CI run #331, see the comment above) onto a scratch database
// created by internal/testsupport/scratchdb with the full migration chain
// applied via Pool.MigrateUp - not the shared TEST_DATABASE_URL database
// every other package's integration test also runs against. No other
// package can ever attach to this database, so no concurrently-running
// test can hold a lock on casino_games, sb_sports, or anything CASCADE
// would touch, and the deny-truncate trigger is always the thing that
// aborts the statement - never a lock conflict racing it. This is the
// real migrated schema (same migrations/ directory, same triggers, same
// RLS policies) - only the database instance is private to this test.
func TestCatalogueRLS_TruncateIsRefusedLoudly(t *testing.T) {
	pool := catalogueScratchPool(t)
	// Only the referencing rows this seeds matter here (they are what
	// makes CASCADE necessary below) - neither subtest reads any of the
	// returned ids.
	seedCatalogueChain(t, pool)

	t.Run("casino_games TRUNCATE", func(t *testing.T) {
		// CASCADE is required here only to get past Postgres's own
		// plan-time "cannot truncate a table referenced in a foreign key
		// constraint" refusal (casino_game_availability etc. reference
		// casino_games) - the deny-truncate trigger below still fires and
		// aborts the WHOLE statement (including the cascade) before any
		// row is actually removed, so this proves the trigger refuses
		// loudly rather than relying on the FK guard to do the refusing.
		err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE casino_games CASCADE`)
			return err
		})
		assertImmutableViolation(t, err, "TRUNCATE")
	})

	t.Run("sb_sports TRUNCATE", func(t *testing.T) {
		// CASCADE for the same reason as casino_games above (sb_sports is
		// referenced by sb_competitions).
		err := pool.WithPlatformService(context.Background(), ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `TRUNCATE sb_sports CASCADE`)
			return err
		})
		assertImmutableViolation(t, err, "TRUNCATE")
	})
}

// catalogueScratchPool creates an isolated scratch database (see
// TestCatalogueRLS_TruncateIsRefusedLoudly's comment) with the full
// migration chain applied, and connects to it exactly like testPool
// connects to the shared database (same Connect call, same non-superuser/
// non-BYPASSRLS enforcement in db.Connect's verifyNotPrivileged).
func catalogueScratchPool(t *testing.T) *Pool {
	t.Helper()
	url := scratchdb.New(t, "catrls_truncate_")
	pool, err := Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), "../../migrations"); err != nil {
		t.Fatalf("migrate scratch database: %v", err)
	}
	return pool
}

// --- Item 10 (fix round, SEC-S91-3): casino_games writes require the
// platform-admin principal to resolve to a REAL staff_users row ---
//
// Migration 0084's casino_games write policies only check that
// app.platform_admin_principal_id is set to SOME non-null uuid - not that
// it names a real platform-scoped staff principal, unlike the five sb_*
// tables, whose policies pin the exact literal service-identity string
// 'sportsbook_catalogue_sync'. Migration 0085 closes that gap with a
// BEFORE INSERT/UPDATE trigger, mirroring migration 0044's
// asset_change_requests_require_platform_principal precedent.
func TestCatalogueRLS_CasinoGamesRequiresRealPlatformPrincipal(t *testing.T) {
	pool := testPool(t)

	t.Run("bogus uuid, not tied to any staff_users row, is rejected", func(t *testing.T) {
		bogusPrincipalID := uuid.New() // deliberately never inserted into staff_users
		err := pool.WithPlatformAdmin(context.Background(), bogusPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type) VALUES ($1, $1, 'SEC-S91-3 Bogus Principal', 'slot')`,
				uuid.New().String())
			return err
		})
		if err == nil {
			t.Fatal("expected the bogus platform-admin principal to be rejected, got nil")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
		}
		// P0001 (raise_exception, from the new trigger), NOT 42501 (RLS
		// violation) - this is the whole point: the RLS policy alone
		// would have let this uuid through since it is non-null, so the
		// rejection has to come from the trigger, at a distinct SQLSTATE,
		// or this test would not actually distinguish "closed by the
		// trigger" from "would have been closed anyway".
		if pgErr.Code != "P0001" {
			t.Fatalf("expected SQLSTATE P0001 (the new trigger's raise_exception), got %s: %v", pgErr.Code, err)
		}
		if !strings.Contains(pgErr.Message, "does not resolve to a real platform-scoped") {
			t.Fatalf("expected message naming the unresolved platform principal, got %q", pgErr.Message)
		}
	})

	t.Run("bogus uuid also rejected on UPDATE", func(t *testing.T) {
		c := seedCatalogueChain(t, pool)
		bogusPrincipalID := uuid.New()
		err := pool.WithPlatformAdmin(context.Background(), bogusPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE casino_games SET name = 'SEC-S91-3 Bogus Principal Update' WHERE id = $1`, c.gameID)
			return err
		})
		if err == nil {
			t.Fatal("expected the bogus platform-admin principal to be rejected on UPDATE, got nil")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) {
			t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
		}
		if pgErr.Code != "P0001" {
			t.Fatalf("expected SQLSTATE P0001, got %s: %v", pgErr.Code, err)
		}
	})

	t.Run("a real platform-scoped staff_users principal is accepted", func(t *testing.T) {
		realPrincipalID := seedPlatformAdminStaffPrincipal(t, pool)
		var gameID uuid.UUID
		err := pool.WithPlatformAdmin(context.Background(), realPrincipalID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type) VALUES ($1, $1, 'SEC-S91-3 Real Principal', 'slot') RETURNING id`,
				uuid.New().String()).Scan(&gameID)
		})
		if err != nil {
			t.Fatalf("expected a real platform-scoped staff principal to be accepted, got: %v", err)
		}
		if gameID == uuid.Nil {
			t.Fatal("expected a real game id to be returned")
		}
	})

	t.Run("a tenant-scoped staff_users principal is rejected (not merely a nonexistent one)", func(t *testing.T) {
		// Belt-and-braces: a principal that DOES resolve to a staff_users
		// row, but a tenant-scoped one, must be refused exactly like a
		// wholly nonexistent uuid - resolving to SOME row is not enough,
		// it must resolve to a tenant_id IS NULL row specifically.
		tenantID := uuid.New()
		err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx,
				`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'SEC-S91-3 Tenant', 'under_platform_licence')`,
				tenantID, "sec-s91-3-"+tenantID.String()[:8])
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
			}
			return nil
		})
		if err != nil {
			t.Fatalf("seed tenant for tenant-scoped staff principal: %v", err)
		}

		tenantScopedStaffID := uuid.New()
		err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, $2, $3, 'x', 'tenant_admin')`,
				tenantScopedStaffID, tenantID, "sec-s91-3-tenant-staff-"+tenantScopedStaffID.String()+"@test.example")
			return err
		})
		if err != nil {
			t.Fatalf("seed tenant-scoped staff_users row: %v", err)
		}

		err = pool.WithPlatformAdmin(context.Background(), tenantScopedStaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO casino_games (provider_id, provider_game_id, name, game_type) VALUES ($1, $1, 'SEC-S91-3 Tenant-Scoped Principal', 'slot')`,
				uuid.New().String())
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
