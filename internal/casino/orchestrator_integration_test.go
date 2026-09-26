//go:build integration

// Stage 4A provider conformance/adversarial suite - the real-PostgreSQL
// half of directive items A-T (see conformance_test.go's doc comment for
// the split: this file covers anything touching wallet/ledger/idempotency/
// RLS/transaction state, per CLAUDE.md's "use real PostgreSQL integration
// tests wherever the operation touches" those). Follows the exact
// fixture/testPool conventions internal/payments/orchestrator_integration_
// test.go established.
package casino

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// pgRLSViolationCode is the Postgres SQLSTATE for "new row violates
// row-level security policy" - see internal/withdrawal/adversarial_test.go's
// identical helper; duplicated per this repo's per-package test-helper
// convention rather than shared.
const pgRLSViolationCode = "42501"

func assertRLSViolation(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a row-level-security violation, got nil")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a *pgconn.PgError, got %T: %v", err, err)
	}
	if pgErr.Code != pgRLSViolationCode {
		t.Fatalf("expected SQLSTATE %s (row-level security violation), got %s: %v", pgRLSViolationCode, pgErr.Code, err)
	}
}

type casinoFixture struct {
	tenantID        uuid.UUID
	brandID         uuid.UUID
	playerAccountID uuid.UUID
	walletID        uuid.UUID
}

// seedCasinoFixture mirrors internal/payments/orchestrator_integration_
// test.go's seedOrchFixture exactly (tenant/person/brand/player/wallet).
func seedCasinoFixture(t *testing.T, pool *db.Pool) casinoFixture {
	t.Helper()
	f := casinoFixture{tenantID: uuid.New(), brandID: uuid.New(), playerAccountID: uuid.New()}
	personID := uuid.New()

	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes now
	// require a genuinely platform-admin-scoped transaction.
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx,
			`INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1, $2, 'Test Tenant', 'under_platform_licence')`,
			f.tenantID, "t-"+f.tenantID.String()[:8])
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("expected to insert 1 tenant row, inserted %d", tag.RowsAffected())
		}
		_, err = tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform rows: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Test Brand')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			f.playerAccountID, f.tenantID, f.brandID, personID, f.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, "EUR")
		if err != nil {
			return err
		}
		f.walletID = w.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed tenant rows: %v", err)
	}
	return f
}

// seedPlatformAdminStaffPrincipal inserts a genuine platform-scoped
// (tenant_id IS NULL) staff_users row and returns its id. Migration 0085
// (SEC-S91-3) added a trigger requiring app.platform_admin_principal_id
// to resolve to a REAL staff_users row before casino_games can be
// written, so WithPlatformAdmin(ctx, uuid.New(), ...) alone no longer
// suffices for a legitimate casino_games write in this suite -
// password_hash is a dummy literal, these principals are never used to
// log in, only to satisfy the trigger's staff_users lookup.
//
// Migration 0089 (SEC-S92-1 fix round) requires every principal that
// files or decides a casino_catalogue_change_requests row to carry a
// confirmed, active Person linkage - so this default fixture now creates
// one, each principal resolving to its OWN distinct person (two calls
// are two genuinely distinct humans, never a same-person bypass). A test
// that specifically needs an UNLINKED or SUSPENDED principal, or two
// principals sharing one person, uses a purpose-built helper instead
// (seedUnlinkedPlatformAdminStaffPrincipal,
// seedSuspendedPlatformAdminStaffPrincipal,
// seedTwoPlatformPrincipalsSharingPerson), never this one - mirroring
// internal/httpserver's mustCreateStaff/mustCreateStaffWithPerson split.
func seedPlatformAdminStaffPrincipal(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	personID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`,
			id, "platform-admin-"+id.String()+"@test.example", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin staff principal: %v", err)
	}
	return id
}

// seedUnlinkedPlatformAdminStaffPrincipal inserts a platform-scoped staff
// principal with NO Person linkage (person_id IS NULL) - the exact shape
// security found empirically reachable from cmd/seed-admin's default
// output, and the shape migration 0089's fix now refuses outright rather
// than silently treating as "no person to compare against".
func seedUnlinkedPlatformAdminStaffPrincipal(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', NULL, 'active')`,
			id, "platform-admin-unlinked-"+id.String()+"@test.example")
		return err
	})
	if err != nil {
		t.Fatalf("seed unlinked platform admin staff principal: %v", err)
	}
	return id
}

// seedSuspendedPlatformAdminStaffPrincipal inserts a platform-scoped
// staff principal that IS person-linked (a distinct person from any
// other fixture) but whose status is 'suspended' - proves migration
// 0089's new status='active' check, which migration 0086 had nowhere at
// all, actually fires.
func seedSuspendedPlatformAdminStaffPrincipal(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	personID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'suspended')`,
			id, "platform-admin-suspended-"+id.String()+"@test.example", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed suspended platform admin staff principal: %v", err)
	}
	return id
}

// seedGame registers a platform-catalogue title for providerID - a
// platform-administrative action (db.Pool.WithPlatformAdmin, required
// since migration 0084/ADR 0081 gave casino_games ENABLE+FORCE RLS),
// mirroring newUpsertCasinoGameHandler.
func seedGame(t *testing.T, pool *db.Pool, providerID string, assetCodes ...string) Game {
	t.Helper()
	var g Game
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		g, err = UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: providerID, ProviderGameID: "game-" + uuid.New().String()[:8],
			Name: "Test Game", GameType: "slot", SupportedAssets: assetCodes, Status: GameStatusActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed game: %v", err)
	}
	return g
}

// mintSession creates a real casino_launch_sessions row for f, in real
// ('non-demo') mode, and returns its id - the session identifier a bet
// callback must now carry (specialist review finding: postBet resolves
// the actual wallet/player/mode from this platform-owned row, never from
// a payload-supplied player_account_id). Seeds its own fresh platform-
// catalogue game internally (financial tests care about the bet/win/
// rollback posting logic, not catalogue eligibility, which LaunchGame's
// own dedicated tests already cover) - mints the session directly via
// CreateLaunchSession rather than the full LaunchGame eligibility chain.
func mintSession(t *testing.T, pool *db.Pool, f casinoFixture, providerID, assetCode string) uuid.UUID {
	t.Helper()
	game := seedGame(t, pool, providerID, assetCode)
	var sessionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		session, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: providerID, ProviderGameID: game.ProviderGameID,
			AssetCode: assetCode, Mode: ModeReal,
		})
		if err != nil {
			return err
		}
		sessionID = session.ID
		return nil
	})
	if err != nil {
		t.Fatalf("mint launch session: %v", err)
	}
	return sessionID
}

func enableGameForTenant(t *testing.T, pool *db.Pool, f casinoFixture, gameID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetGameAvailability(ctx, tx, f.tenantID, nil, gameID, true)
		return err
	})
	if err != nil {
		t.Fatalf("enable game for tenant: %v", err)
	}
}

// registerCasinoCapability writes an active, tenant-wide capability row
// mirroring provider's own declared capability exactly, exactly like
// payments' registerCapability helper.
func registerCasinoCapability(t *testing.T, pool *db.Pool, f casinoFixture, provider CasinoProvider, priority int) {
	t.Helper()
	declared := provider.Capabilities()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportsCatalogue: declared.SupportsCatalogue, SupportsLaunch: declared.SupportsLaunch, SupportsBalance: declared.SupportsBalance,
			SupportsBet: declared.SupportsBet, SupportsWin: declared.SupportsWin, SupportsRollback: declared.SupportsRollback,
			SupportedAssets: declared.SupportedAssets, SupportedGameTypes: declared.SupportedGameTypes,
			Priority: priority, Status: CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("register capability for %s: %v", declared.ProviderID, err)
	}
}

// fundWallet credits f's player_cash wallet via a reason-coded
// manual_adjustment (CLAUDE.md's four-eyes rule - simulated here as a
// direct test fixture, not an actual staff action), so bet-debit tests
// exercise Flow 5's real sufficiency gate against a funded balance rather
// than accidentally testing the decline path when they mean to test the
// success path.
func fundWallet(t *testing.T, pool *db.Pool, f casinoFixture, amount int64) {
	t.Helper()
	reason := "test fixture funding"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		adjustmentAccountID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fund-" + uuid.New().String(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adjustmentAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("fund wallet: %v", err)
	}
}

func cashBalance(t *testing.T, pool *db.Pool, f casinoFixture) int64 {
	t.Helper()
	var balance int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := wallet.GetByID(ctx, tx, f.walletID)
		if err != nil {
			return err
		}
		summary, err := wallet.GetSummary(ctx, tx, w)
		if err != nil {
			return err
		}
		balance = summary.CashBalance
		return nil
	})
	if err != nil {
		t.Fatalf("read cash balance: %v", err)
	}
	return balance
}

// extractToken pulls the launch token out of MockCasinoProvider's
// synthetic LaunchURL ("https://mock-casino.invalid/launch/<game>?token=<tok>").
func extractToken(t *testing.T, launchURL string) string {
	t.Helper()
	parsed, err := url.Parse(launchURL)
	if err != nil {
		t.Fatalf("parse launch url: %v", err)
	}
	token := parsed.Query().Get("token")
	if token == "" {
		t.Fatalf("launch url has no token query parameter: %s", launchURL)
	}
	return token
}

// auditActionExists reports whether audit_log carries at least one row for
// action under tenantID - directive item S ("auditability").
func auditActionExists(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string) bool {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenantID, action).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query audit_log for %s: %v", action, err)
	}
	return count > 0
}

// sumDebitsCredits proves Invariant #1 (SUM(DEBITS) == SUM(CREDITS)) for
// tenantID's own ledger entries - directive item T.
func sumDebitsCredits(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (debits, credits int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT
				COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
				COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
			 FROM ledger_entries WHERE tenant_id = $1`,
			tenantID,
		).Scan(&debits, &credits)
	})
	if err != nil {
		t.Fatalf("sum debits/credits: %v", err)
	}
	return debits, credits
}

// --- B. Launch contract (full flow) + C. game-session isolation ---

func TestLaunchGame_MintsSingleUseSessionEmbeddedInLaunchURL(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	var result LaunchGameResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	if result.LaunchURL == "" || result.SessionID == uuid.Nil {
		t.Fatalf("expected a launch url and session id, got %+v", result)
	}

	token := extractToken(t, result.LaunchURL)

	var session LaunchSession
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		session, err = ResolveLaunchToken(ctx, tx, token)
		return err
	})
	if err != nil {
		t.Fatalf("ResolveLaunchToken (first): %v", err)
	}
	if session.PlayerAccountID != f.playerAccountID || session.TenantID != f.tenantID {
		t.Fatalf("resolved session identity mismatch: %+v", session)
	}

	// Single-use enforcement: a second resolution of the SAME token must
	// fail, even immediately after the first (ADR 0025 §3).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ResolveLaunchToken(ctx, tx, token)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionNotActive) {
		t.Fatalf("expected ErrLaunchSessionNotActive on second resolution, got %v", err)
	}

	if !auditActionExists(t, pool, f.tenantID, "casino.launched") {
		t.Fatal("expected a casino.launched audit record")
	}
}

func TestLaunchSessions_CrossTenantRLSBlocksReadAndForgedInsert(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	gameA := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, fA, gameA.ID)
	registerCasinoCapability(t, pool, fA, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	var result LaunchGameResult
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: fA.tenantID, BrandID: fA.brandID, PlayerAccountID: fA.playerAccountID, WalletID: fA.walletID,
			GameID: gameA.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err != nil {
		t.Fatalf("LaunchGame under tenant A: %v", err)
	}

	// Tenant B's staff scope must see zero rows for tenant A's session,
	// even querying by its exact id - proving isolation is RLS itself.
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE id = $1`, result.SessionID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected tenant B's scope to see zero rows for tenant A's session, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant staff-scope read: %v", err)
	}

	// Tenant B's PLAYER scope (a different player entirely) must also see
	// nothing - player_self_scope filters by player_account_id, never by
	// tenant alone.
	err = pool.WithPlayerScope(context.Background(), fB.tenantID, fB.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE id = $1`, result.SessionID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected tenant B's player scope to see zero rows for tenant A's session, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant player-scope read: %v", err)
	}

	// The WITH CHECK half: tenant A's own scope cannot forge an insert
	// claiming to belong to tenant B.
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_launch_sessions
				(id, tenant_id, brand_id, player_account_id, wallet_id, game_id, provider_id, provider_game_id,
				 asset_code, mode, token_hash, status, expires_at)
			 VALUES ($1, $2, $3, $4, $5, $6, 'mock-casino', 'game-x', 'EUR', 'real', 'forged-hash', 'active', now() + interval '1 minute')`,
			uuid.New(), fB.tenantID, fB.brandID, fB.playerAccountID, fB.walletID, gameA.ID,
		)
		return err
	})
	assertRLSViolation(t, err)
}

// --- N/O/P/Q. Launch-eligibility failure modes ---

func TestLaunchGame_InvalidGameNotFound(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": NewMockCasinoProvider("mock-casino", "EUR")}, nil)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: uuid.New(), AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !errors.Is(err, ErrGameNotFound) {
		t.Fatalf("expected ErrGameNotFound, got %v", err)
	}
}

func TestLaunchGame_DisabledGameAtPlatformLevel(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// Platform pulls the title (a licence problem, not a tenant decision).
	err := pool.WithPlatformAdmin(context.Background(), seedPlatformAdminStaffPrincipal(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		_, err := UpsertGame(ctx, tx, UpsertGameInput{
			ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID, Name: game.Name, GameType: game.GameType,
			SupportedAssets: game.SupportedAssets, Status: GameStatusDisabled,
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable game: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !errors.Is(err, ErrGameDisabled) {
		t.Fatalf("expected ErrGameDisabled, got %v", err)
	}
}

func TestLaunchGame_NotAvailableForTenantUntilOptedIn(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// Deliberately never call enableGameForTenant - fail-closed opt-in.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !errors.Is(err, ErrGameNotAvailable) {
		t.Fatalf("expected ErrGameNotAvailable, got %v", err)
	}
}

func TestLaunchGame_InvalidAssetRejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR") // EUR only
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR", "USD")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "USD", Mode: ModeReal,
		})
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for an asset the GAME doesn't support, got %v", err)
	}
}

func TestLaunchGame_DisabledProviderCapabilityRejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// Deliberately never register a capability row at all - "no route
	// configured" must behave identically to "explicitly disabled."
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable with no capability row, got %v", err)
	}
}

func TestLaunchGame_UnhealthyProviderCircuitOpenRejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	provider.SetHealth(ProviderHealth{CircuitState: CircuitOpen})
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable for an open circuit, got %v", err)
	}
}

// --- Bet/Win/Rollback financial boundary (Flows 5-7) ---

func TestReceiveCallback_BetPostsFlow5AndDebitsPlayerCash(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-1", "", "round-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (bet): %v", err)
	}
	if result.Outcome != OutcomeSucceeded || result.LedgerTransactionID == nil {
		t.Fatalf("expected a succeeded bet with a ledger transaction id, got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 4000 {
		t.Fatalf("expected cash balance 4000 (5000 funded - 1000 bet), got %d", balance)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino_bet.posted") {
		t.Fatal("expected a casino_bet.posted audit record")
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

func TestReceiveCallback_BetDeclinedOnInsufficientFundsNeverPosts(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 500)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// A bet for MORE than the funded balance must be declined - Flow 5's
	// sufficiency gate, checked inside the same transaction as the
	// prospective debit (invariant #15) - and must post NOTHING to the
	// ledger, not even a declined-but-recorded transaction.
	oversized := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-oversized", "", "round-oversized", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var declineResult ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		declineResult, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", oversized)
		return err
	})
	if err != nil {
		t.Fatalf("oversized bet: %v", err)
	}
	if declineResult.Outcome != OutcomeDeclined || declineResult.DeclineReason != "insufficient_funds" {
		t.Fatalf("expected a declined bet with reason insufficient_funds, got %+v", declineResult)
	}
	if declineResult.LedgerTransactionID != nil {
		t.Fatal("a declined bet must not report a ledger transaction id")
	}
	if balance := cashBalance(t, pool, f); balance != 500 {
		t.Fatalf("expected the funded balance (500) unchanged after a declined bet, got %d", balance)
	}

	var txCount int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'bet-oversized'`).Scan(&txCount)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions: %v", err)
	}
	if txCount != 0 {
		t.Fatalf("expected zero ledger_transactions rows for a declined bet, got %d", txCount)
	}

	// A bet within the funded balance still succeeds afterward - proves
	// the rejection above is about sufficiency, not a blanket block.
	within := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-within", "", "round-within", "game-1", 300, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var okResult ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		okResult, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", within)
		return err
	})
	if err != nil {
		t.Fatalf("within-balance bet: %v", err)
	}
	if okResult.Outcome != OutcomeSucceeded {
		t.Fatalf("expected the within-balance bet to succeed, got %+v", okResult)
	}
	if balance := cashBalance(t, pool, f); balance != 200 {
		t.Fatalf("expected cash balance 200 (500 - 300) after the within-balance bet, got %d", balance)
	}
}

func TestReceiveCallback_WinPostsFlow6AndCreditsPlayerCash(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-win-1", "", "round-win-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("bet: %v", err)
	}

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-1", "", "round-win-1", "game-1", 2500, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	var result ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (win): %v", err)
	}
	if result.Outcome != OutcomeSucceeded {
		t.Fatalf("expected a succeeded win, got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 6500 {
		t.Fatalf("expected cash balance 6500 (5000 funded - 1000 bet + 2500 win), got %d", balance)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino_win.posted") {
		t.Fatal("expected a casino_win.posted audit record")
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// financial-transaction-flows.md §6: a win naming a round with no matching
// prior bet is an integrity alert, never silently posted.
func TestReceiveCallback_WinWithNoPriorBetIsIntegrityAlert(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-orphan-1", "", "round-never-bet", "game-1", 5000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if !errors.Is(err, ErrBetNotFound) {
		t.Fatalf("expected ErrBetNotFound, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("an orphan win must never post, got balance %d", balance)
	}
}

// G/H/I. Idempotency, duplicate callback handling, and replay protection -
// a redelivered bet/win/rollback callback (identical payload, identical
// signature) must never create a second financial effect.
func TestReceiveCallback_RedeliveredBetWinRollbackAreIdempotent(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-idem-1", "", "round-idem-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	for i := 0; i < 2; i++ {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", betPayload)
			return err
		})
		if err != nil {
			t.Fatalf("bet delivery %d: %v", i, err)
		}
	}
	if balance := cashBalance(t, pool, f); balance != 4000 {
		t.Fatalf("expected exactly one bet's worth (5000-1000=4000) after a redelivered bet callback, got %d", balance)
	}

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-idem-1", "", "round-idem-1", "game-1", 3000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	for i := 0; i < 2; i++ {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
			return err
		})
		if err != nil {
			t.Fatalf("win delivery %d: %v", i, err)
		}
	}
	if balance := cashBalance(t, pool, f); balance != 7000 {
		t.Fatalf("expected exactly one win's worth applied (4000+3000=7000) after a redelivered win callback, got %d", balance)
	}

	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-idem-1", "win-idem-1", "round-idem-1", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	for i := 0; i < 2; i++ {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
			return err
		})
		if err != nil {
			t.Fatalf("rollback delivery %d: %v", i, err)
		}
	}
	// Rollback of the win reverses it exactly: 7000 - 3000 = 4000.
	if balance := cashBalance(t, pool, f); balance != 4000 {
		t.Fatalf("expected the win rolled back exactly once (4000) after a redelivered rollback callback, got %d", balance)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated after redelivered bet/win/rollback: debits=%d credits=%d", debits, credits)
	}
}

func TestReceiveCallback_RollbackReversesFlow5BetExactly(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-rollback-1", "", "round-rollback-1", "game-1", 750, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("bet: %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 250 {
		t.Fatalf("expected 250 (1000-750) after bet, got %d", balance)
	}

	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-1", "bet-rollback-1", "round-rollback-1", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	var result ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if result.Tombstoned {
		t.Fatal("a rollback of an already-posted bet must not be a tombstone")
	}
	if balance := cashBalance(t, pool, f); balance != 1000 {
		t.Fatalf("expected balance back to the funded 1000 after rollback, got %d", balance)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino_bet.rolled_back") {
		t.Fatal("expected a casino_bet.rolled_back audit record")
	}
}

// A SECOND, distinct rollback reference naming an already-rolled-back
// original must be rejected - mirrors payments'
// TestReceiveCallback_SecondReversalOfSameDepositRejected exactly.
func TestReceiveCallback_SecondDistinctRollbackOfSameBetRejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-double-rollback", "", "round-double-rollback", "game-1", 400, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("bet: %v", err)
	}

	firstRollback := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-first", "bet-double-rollback", "round-double-rollback", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", firstRollback)
		return err
	})
	if err != nil {
		t.Fatalf("first rollback: %v", err)
	}

	secondRollback := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-second-distinct", "bet-double-rollback", "round-double-rollback", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", secondRollback)
		return err
	})
	if !errors.Is(err, ErrAlreadyRolledBack) {
		t.Fatalf("expected ErrAlreadyRolledBack, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 1000 {
		t.Fatalf("expected balance still 1000 (unwound bet) after the rejected second rollback, got %d", balance)
	}
}

func TestReceiveCallback_RollbackOfNeverSeenOriginalWritesTombstone(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-orphan-1", "bet-never-posted", "round-orphan", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback (rollback of unseen original): %v", err)
	}
	if !result.Tombstoned {
		t.Fatal("expected a tombstone for a rollback of a bet/win never posted to the ledger")
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("a tombstoned rollback must never affect the balance, got %d", balance)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino_rollback.tombstoned") {
		t.Fatal("expected a casino_rollback.tombstoned audit record")
	}
}

// M. Cross-tenant isolation for the callback/financial boundary: a bet
// posted under tenant A must be entirely invisible to tenant B's scope,
// even querying the identical provider_id/round/reference.
func TestReceiveCallback_CrossTenantBetIsInvisible(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	fundWallet(t, pool, fA, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, fA, provider, 100)
	registerCasinoCapability(t, pool, fB, provider, 100)
	sessionID := mintSession(t, pool, fA, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(fA.tenantID, CallbackEventBet, "bet-cross-tenant", "", "round-cross-tenant", "game-1", 1000, "EUR", OutcomeSucceeded, "", fA.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, fA.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("bet under tenant A: %v", err)
	}

	// A win callback under tenant B's scope, naming the exact same
	// provider_id/round_id, must find no matching bet under tenant B - RLS
	// on ledger_transactions scopes the EXISTS check itself, not just an
	// explicit WHERE tenant_id clause in application code.
	winPayload := provider.CallbackPayload(fB.tenantID, CallbackEventWin, "win-cross-tenant", "", "round-cross-tenant", "game-1", 2000, "EUR", OutcomeSucceeded, "", fB.playerAccountID, uuid.Nil)
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, fB.tenantID, "mock-casino", winPayload)
		return err
	})
	if !errors.Is(err, ErrBetNotFound) {
		t.Fatalf("expected ErrBetNotFound under tenant B's scope (cross-tenant round id collision must not be visible), got %v", err)
	}
	if balance := cashBalance(t, pool, fB); balance != 0 {
		t.Fatalf("tenant B's wallet must not be affected, got balance %d", balance)
	}

	var count int
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'bet-cross-tenant'`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("cross-tenant ledger_transactions read: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected tenant B's scope to see zero rows for tenant A's bet, got %d", count)
	}
}

// R. Unauthorized access - the player-scope-exclusion guard: a
// WithPlayerScope-scoped transaction (a player's own session context) must
// never be able to read or write casino_game_availability or
// casino_provider_capabilities at all, even its own tenant's rows - the
// tenant_isolation policy's own USING clause requires
// app.player_account_id IS NULL, so a player-scoped SELECT is not an
// error, it is silently filtered to zero rows (RLS's row-filtering
// semantics for USING, as opposed to the error a WITH CHECK violation on
// a write raises) - mirrors the identical guard proven for
// withdrawal_policies/provider_capabilities in earlier stages.
func TestCasinoConfigTables_PlayerScopeExclusionGuard(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)

	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_game_availability WHERE tenant_id = $1`, f.tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected a player-scoped transaction to see zero casino_game_availability rows, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player-scoped read of casino_game_availability: %v", err)
	}

	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_provider_capabilities WHERE tenant_id = $1`, f.tenantID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected a player-scoped transaction to see zero casino_provider_capabilities rows, got %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player-scoped read of casino_provider_capabilities: %v", err)
	}

	// The WITH CHECK half: a player-scoped transaction cannot forge a
	// write into either table, even naming its own tenant. Uses a SECOND,
	// not-yet-opted-in game so this fails on the RLS check itself, not on
	// the unique index already covering game's own row from
	// enableGameForTenant above.
	game2 := seedGame(t, pool, "mock-casino", "EUR")
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_game_availability (id, tenant_id, brand_id, game_id, enabled) VALUES ($1, $2, NULL, $3, true)`,
			uuid.New(), f.tenantID, game2.ID,
		)
		return err
	})
	assertRLSViolation(t, err)
}

// T. Financial invariant preservation across a mixed bet/win/rollback
// sequence, including a decline path, using the ledger's own accounting
// rather than the wallet projection alone.
func TestCasinoFlows_FinancialInvariantsHoldAcrossMixedSequence(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	events := []struct {
		eventType    CallbackEventType
		providerTxID string
		original     string
		roundID      string
		amount       int64
	}{
		{CallbackEventBet, "seq-bet-1", "", "seq-round-1", 1000},
		{CallbackEventWin, "seq-win-1", "", "seq-round-1", 1800},
		{CallbackEventBet, "seq-bet-2", "", "seq-round-2", 500},
		{CallbackEventRollback, "seq-rollback-1", "seq-bet-2", "seq-round-2", 0},
	}
	for _, e := range events {
		sid := uuid.Nil
		if e.eventType == CallbackEventBet {
			sid = sessionID
		}
		payload := provider.CallbackPayload(f.tenantID, e.eventType, e.providerTxID, e.original, e.roundID, "game-1", e.amount, "EUR", OutcomeSucceeded, "", f.playerAccountID, sid)
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
			return err
		})
		if err != nil {
			t.Fatalf("event %s (%s): %v", e.eventType, e.providerTxID, err)
		}
	}

	// 5000 (funded) - 1000 (bet1) + 1800 (win1) - 500 (bet2) + 500 (rollback of bet2) = 5800.
	if balance := cashBalance(t, pool, f); balance != 5800 {
		t.Fatalf("expected cash balance 5800 after the mixed sequence, got %d", balance)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
	if debits == 0 {
		t.Fatal("expected a non-zero number of ledger entries to have been posted")
	}
}

// --- Regression tests for specialist-review P1 findings ---

// A bet with no session_id at all must be rejected - the whole point of
// binding a bet to a platform-issued session (never a payload-supplied
// player_account_id alone).
func TestReceiveCallback_BetWithNoSessionRejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-no-session", "", "round-no-session", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("expected ErrLaunchSessionRequired, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected balance unchanged at 5000, got %d", balance)
	}
}

// A bet naming a session minted in demo mode must never post a real
// financial effect.
func TestReceiveCallback_BetWithDemoSessionRejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	game := seedGame(t, pool, "mock-casino", "EUR")
	var demoSessionID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		session, _, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, ProviderID: "mock-casino", ProviderGameID: game.ProviderGameID,
			AssetCode: "EUR", Mode: ModeDemo,
		})
		demoSessionID = session.ID
		return err
	})
	if err != nil {
		t.Fatalf("mint demo session: %v", err)
	}
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-demo", "", "round-demo", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, demoSessionID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("expected ErrLaunchSessionRequired for a demo-mode session, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected balance unchanged at 5000, got %d", balance)
	}
}

// A win must credit the SAME wallet the round's own bet debited, never a
// different player_account_id the callback payload happens to name -
// empirically reproduced by ledger-finance's specialist review before this
// fix (a win named a different player and that player was credited).
func TestReceiveCallback_WinCreditsBettorsWalletNeverPayloadPlayer(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fundWallet(t, pool, fA, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, fA, provider, 100)
	sessionID := mintSession(t, pool, fA, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(fA.tenantID, CallbackEventBet, "bet-misdirect", "", "round-misdirect", "game-1", 1000, "EUR", OutcomeSucceeded, "", fA.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, fA.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("bet: %v", err)
	}

	// A DIFFERENT player within the SAME tenant places no bet at all, but
	// the win callback names their player_account_id instead of the
	// actual bettor's.
	otherPlayerID := uuid.New()
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		personID := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			otherPlayerID, fA.tenantID, fA.brandID, personID, otherPlayerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed other player: %v", err)
	}

	winPayload := provider.CallbackPayload(fA.tenantID, CallbackEventWin, "win-misdirect", "", "round-misdirect", "game-1", 4000, "EUR", OutcomeSucceeded, "", otherPlayerID, uuid.Nil)
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, fA.tenantID, "mock-casino", winPayload)
		return err
	})
	if err != nil {
		t.Fatalf("win: %v", err)
	}

	// The ACTUAL bettor (fA) must be the one credited, never otherPlayerID.
	if balance := cashBalance(t, pool, fA); balance != 8000 {
		t.Fatalf("expected the actual bettor to be credited (5000-1000+4000=8000), got %d", balance)
	}
	var otherPlayerHasWallet bool
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM wallets WHERE player_account_id = $1)`, otherPlayerID).Scan(&otherPlayerHasWallet)
	})
	if err != nil {
		t.Fatalf("check other player wallet: %v", err)
	}
	if otherPlayerHasWallet {
		t.Fatal("the payload-named player must never have had a wallet created or credited for this win")
	}
}

// A win on a round whose bet has already been rolled back (a voided
// round) is the same class of integrity violation as an orphan win.
func TestReceiveCallback_WinOnRolledBackBetRejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-voided", "", "round-voided", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("bet: %v", err)
	}
	rollbackPayload := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-voided", "bet-voided", "round-voided", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackPayload)
		return err
	})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}

	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "win-voided", "", "round-voided", "game-1", 9000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", winPayload)
		return err
	})
	if !errors.Is(err, ErrBetNotFound) {
		t.Fatalf("expected ErrBetNotFound for a win on a voided round, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected balance unchanged at the funded 5000, got %d", balance)
	}
}

// A bet/win callback whose own Outcome disagrees with "succeeded" must
// never post as though it succeeded - qa specialist review finding,
// empirically reproduced before this fix (a declined-outcome bet posted a
// full real debit).
func TestReceiveCallback_NonSucceededOutcomeRejected(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	declined := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-declined-outcome", "", "round-declined-outcome", "game-1", 1000, "EUR", OutcomeDeclined, "provider_declined", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", declined)
		return err
	})
	if !errors.Is(err, ErrOutcomeNotSucceeded) {
		t.Fatalf("expected ErrOutcomeNotSucceeded, got %v", err)
	}

	ambiguous := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-ambiguous-outcome", "", "round-ambiguous-outcome", "game-1", 1000, "EUR", OutcomeAmbiguous, "", f.playerAccountID, sessionID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", ambiguous)
		return err
	})
	if !errors.Is(err, ErrOutcomeNotSucceeded) {
		t.Fatalf("expected ErrOutcomeNotSucceeded for an ambiguous outcome, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected balance unchanged at 5000, got %d", balance)
	}
}

// A tenant's own CasinoProviderCapability is an actual kill switch on the
// money path, not just at launch time - multi-tenancy specialist review
// finding: previously, disabling (or never configuring) a tenant's own
// capability row had NO effect on whether bet/win/rollback callbacks
// posted.
func TestReceiveCallback_DisabledCapabilityBlocksCallback(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// No capability row registered at all - must behave as disabled.
	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-no-capability", "", "round-no-capability", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable with no capability row, got %v", err)
	}

	// Register, then explicitly disable - a tenant turning off a provider
	// mid-dispute must actually stop new callbacks from posting.
	registerCasinoCapability(t, pool, f, provider, 100)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, provider, f.tenantID, nil, CapabilityConfig{
			SupportsCatalogue: true, SupportsLaunch: true, SupportsBalance: true,
			SupportsBet: true, SupportsWin: true, SupportsRollback: true,
			SupportedAssets: []string{"EUR"}, SupportedGameTypes: []string{"slot", "table", "live"},
			Priority: 100, Status: CapabilityDisabled,
		})
		return err
	})
	if err != nil {
		t.Fatalf("disable capability: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("expected ErrProviderUnavailable once disabled, got %v", err)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected balance unchanged at 5000, got %d", balance)
	}
}

// --- Concurrency tests (CLAUDE.md's mandatory financial-test list;
// specialist review finding: none existed for casino before this fix) ---

// Two concurrent, DISTINCT rollback references for the SAME original bet
// must never both succeed - ledger-finance specialist review finding,
// empirically reproduced before the FOR UPDATE fix in postRollback
// (money was created: a single bet was reversed twice).
func TestReceiveCallback_ConcurrentDistinctRollbacksOnlyOneSucceeds(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-concurrent-rollback", "", "round-concurrent-rollback", "game-1", 400, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", betPayload)
		return err
	})
	if err != nil {
		t.Fatalf("bet: %v", err)
	}

	rollbackA := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-concurrent-a", "bet-concurrent-rollback", "round-concurrent-rollback", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	rollbackB := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rollback-concurrent-b", "bet-concurrent-rollback", "round-concurrent-rollback", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)

	var wg sync.WaitGroup
	results := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		results[0] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackA)
			return err
		})
	}()
	go func() {
		defer wg.Done()
		results[1] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", rollbackB)
			return err
		})
	}()
	wg.Wait()

	succeeded, rejected := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrAlreadyRolledBack):
			rejected++
		default:
			t.Fatalf("unexpected concurrent rollback error: %v", err)
		}
	}
	if succeeded != 1 || rejected != 1 {
		t.Fatalf("expected exactly one success and one ErrAlreadyRolledBack, got %d succeeded, %d rejected", succeeded, rejected)
	}
	if balance := cashBalance(t, pool, f); balance != 1000 {
		t.Fatalf("expected the bet reversed exactly once (balance back to funded 1000), got %d", balance)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// Two concurrent deliveries of the IDENTICAL bet callback must post
// exactly one financial effect - the database-enforced idempotency
// mechanism, proven under real concurrency rather than only sequentially.
func TestReceiveCallback_ConcurrentDuplicateBetsOnlyOneEffect(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-concurrent-dup", "", "round-concurrent-dup", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("delivery %d: %v", i, err)
		}
	}
	if balance := cashBalance(t, pool, f); balance != 4000 {
		t.Fatalf("expected exactly one bet's worth (5000-1000=4000) despite %d concurrent identical deliveries, got %d", n, balance)
	}
	var txCount int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'bet-concurrent-dup'`).Scan(&txCount)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions: %v", err)
	}
	if txCount != 1 {
		t.Fatalf("expected exactly 1 ledger_transactions row despite %d concurrent identical deliveries, got %d", n, txCount)
	}
}

// A launch token can be resolved (consumed) exactly once even under
// concurrent resolution attempts - the atomic compare-and-swap
// (UPDATE ... WHERE status = 'active') claimed in launch.go's own doc
// comment, proven here under real concurrency rather than only
// sequentially.
func TestResolveLaunchToken_ConcurrentResolutionOnlyOneSucceeds(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	var result LaunchGameResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	token := extractToken(t, result.LaunchURL)

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		go func() {
			defer wg.Done()
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := ResolveLaunchToken(ctx, tx, token)
				return err
			})
		}()
	}
	wg.Wait()

	succeeded, rejected := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrLaunchSessionNotActive):
			rejected++
		default:
			t.Fatalf("unexpected concurrent resolve error: %v", err)
		}
	}
	if succeeded != 1 || rejected != n-1 {
		t.Fatalf("expected exactly 1 success and %d rejections, got %d succeeded, %d rejected", n-1, succeeded, rejected)
	}
}

// --- Additional adversarial coverage from specialist review ---

// Cross-tenant forged-INSERT coverage for the two config tables
// (casino_game_availability, casino_provider_capabilities) -
// TestLaunchSessions_CrossTenantRLSBlocksReadAndForgedInsert already
// covers casino_launch_sessions; architect specialist review flagged the
// other two tables as untested for this specific adversarial shape.
func TestCasinoConfigTables_CrossTenantForgedInsertBlocked(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")

	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_game_availability (id, tenant_id, brand_id, game_id, enabled) VALUES ($1, $2, NULL, $3, true)`,
			uuid.New(), fB.tenantID, game.ID,
		)
		return err
	})
	assertRLSViolation(t, err)

	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO casino_provider_capabilities
				(id, tenant_id, brand_id, provider_id, supports_catalogue, supports_launch, supports_balance,
				 supports_bet, supports_win, supports_rollback, supported_assets, supported_game_types,
				 callback_capabilities, priority, status)
			 VALUES ($1, $2, NULL, 'mock-casino', true, true, true, true, true, true, '{}', '{}', 'webhook', 0, 'active')`,
			uuid.New(), fB.tenantID,
		)
		return err
	})
	assertRLSViolation(t, err)
}
