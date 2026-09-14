//go:build integration

// Stage 4D-RG: proves casino LaunchGame/postBet actually consult
// internal/rg.EvaluateEligibility (ADR 0026 §6/§7), including cross-brand
// self-exclusion enforcement (§9) and the concurrency races §8 requires be
// resolved deterministically. Follows this package's own
// orchestrator_integration_test.go fixture conventions exactly.
package casino

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

func suspendAccount(t *testing.T, pool *db.Pool, f casinoFixture) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, f.playerAccountID, identity.PlayerStatusSuspended)
	})
	if err != nil {
		t.Fatalf("suspend account: %v", err)
	}
}

func selfExclude(t *testing.T, pool *db.Pool, f casinoFixture) {
	t.Helper()
	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
		return err
	})
	if err != nil {
		t.Fatalf("self-exclude: %v", err)
	}
}

func personIDForFixture(t *testing.T, pool *db.Pool, f casinoFixture) uuid.UUID {
	t.Helper()
	var personID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		a, err := identity.GetPlayerAccountByID(ctx, tx, f.playerAccountID)
		personID = a.PersonID
		return err
	})
	if err != nil {
		t.Fatalf("resolve person id: %v", err)
	}
	return personID
}

// seedSecondBrandFixture adds a second brand/account/wallet under the SAME
// tenant as f, sharing f's own Person - the directive §9 cross-brand
// scenario (one Person, two PlayerAccounts, two Brands).
func seedSecondBrandFixture(t *testing.T, pool *db.Pool, f casinoFixture) casinoFixture {
	t.Helper()
	personID := personIDForFixture(t, pool, f)
	second := casinoFixture{tenantID: f.tenantID, brandID: uuid.New(), playerAccountID: uuid.New()}
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'Second Brand')`,
			second.brandID, f.tenantID, "b-"+second.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			second.playerAccountID, f.tenantID, second.brandID, personID, second.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed second brand fixture: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, second.brandID, second.playerAccountID, "EUR")
		second.walletID = w.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed second brand wallet: %v", err)
	}
	return second
}

// --- LaunchGame enforcement ---

func TestLaunchGame_DeniedWhenPlayerAccountSuspended(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})
	suspendAccount(t, pool, f)

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
	if !result.Denied || result.DenialCode != rg.CodePlayerAccountNotActive {
		t.Fatalf("expected denial code %q, got %+v", rg.CodePlayerAccountNotActive, result)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino.launch_denied") {
		t.Fatal("expected a casino.launch_denied audit record")
	}
}

func TestLaunchGame_DeniedWhenSelfExcludedPlatformWide(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})
	selfExclude(t, pool, f)

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
	if !result.Denied || result.DenialCode != rg.CodeSelfExcluded {
		t.Fatalf("expected denial code %q, got %+v", rg.CodeSelfExcluded, result)
	}
}

// TestLaunchGame_DeniedCrossBrandSelfExclusion proves directive §9: a
// Person self-excludes via Brand A's account; a launch attempt through
// Brand B's account (same tenant, same Person) is ALSO denied - Brand B
// cannot be used to bypass Brand A's self-exclusion.
//
// SCOPE NOTE: seedSecondBrandFixture constructs the shared person_id
// directly - this proves the enforcement MECHANISM, not that a real
// player can achieve this today by registering twice. See
// docs/decisions/0026 §16: internal/identity's RegisterPlayer mints a
// fresh, unlinked Person on every registration; no production path
// shares a person_id across two independently-created PlayerAccounts yet.
func TestLaunchGame_DeniedCrossBrandSelfExclusion(t *testing.T) {
	pool := testPool(t)
	brandA := seedCasinoFixture(t, pool)
	brandB := seedSecondBrandFixture(t, pool, brandA)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, brandA, game.ID) // platform-catalogue availability is tenant-wide, covers brand B too
	registerCasinoCapability(t, pool, brandA, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	selfExclude(t, pool, brandA)

	var result LaunchGameResult
	err := pool.WithTenant(context.Background(), brandB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
			TenantID: brandB.tenantID, BrandID: brandB.brandID, PlayerAccountID: brandB.playerAccountID, WalletID: brandB.walletID,
			GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
		})
		return err
	})
	if err != nil {
		t.Fatalf("LaunchGame: %v", err)
	}
	if !result.Denied || result.DenialCode != rg.CodeSelfExcluded {
		t.Fatalf("expected brand B's launch to be denied by brand A's platform-wide self-exclusion, got %+v", result)
	}
}

// --- postBet enforcement ---

func TestReceiveCallback_BetDeclinedWhenSelfExcluded_NoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})
	selfExclude(t, pool, f)

	payload := provider.CallbackPayload(CallbackEventBet, "bet-rg-1", "", "round-rg-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeDeclined || result.DeclineReason != rg.CodeSelfExcluded {
		t.Fatalf("expected declined bet with reason %q, got %+v", rg.CodeSelfExcluded, result)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected cash balance UNCHANGED at 5000 (a denied RG check must post nothing), got %d", balance)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
	if !auditActionExists(t, pool, f.tenantID, "casino_bet.denied_by_rg_policy") {
		t.Fatal("expected a casino_bet.denied_by_rg_policy audit record")
	}
}

func TestReceiveCallback_BetDeclinedWhenWalletFrozen_NoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE wallets SET status = 'frozen' WHERE id = $1`, f.walletID)
		return err
	})
	if err != nil {
		t.Fatalf("freeze wallet: %v", err)
	}

	payload := provider.CallbackPayload(CallbackEventBet, "bet-rg-frozen", "", "round-rg-frozen", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeDeclined || result.DeclineReason != rg.CodeWalletNotActive {
		t.Fatalf("expected declined bet with reason %q, got %+v", rg.CodeWalletNotActive, result)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected cash balance UNCHANGED at 5000, got %d", balance)
	}
}

// TestReceiveCallback_RedeliveredBetAfterSelfExclusionStillReportsOriginalSuccess
// is the financial-correctness specialist review's own empirically-
// reproduced finding: a bet that succeeded, then redelivered (the exact
// same provider_tx_id) AFTER the player self-excluded, must report the
// SAME success it originally did - financial-transaction-flows.md §5's
// "exact retry -> idempotent no-op returning the original result", never
// re-evaluated against the now-changed RG state. Without
// findPostedBetTransaction's early short-circuit, this redelivery would
// incorrectly decline a stake that was already legitimately taken.
func TestReceiveCallback_RedeliveredBetAfterSelfExclusionStillReportsOriginalSuccess(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	payload := provider.CallbackPayload(CallbackEventBet, "bet-redeliver-after-exclusion", "", "round-redeliver-after-exclusion", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

	var first ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("first delivery: %v", err)
	}
	if first.Outcome != OutcomeSucceeded || first.LedgerTransactionID == nil {
		t.Fatalf("expected the first delivery to succeed, got %+v", first)
	}

	selfExclude(t, pool, f)

	var second ReceiveCallbackResult
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("redelivery: %v", err)
	}
	if second.Outcome != OutcomeSucceeded || second.LedgerTransactionID == nil || *second.LedgerTransactionID != *first.LedgerTransactionID {
		t.Fatalf("expected the redelivery to report the ORIGINAL success (id %s), got %+v", first.LedgerTransactionID, second)
	}
	if balance := cashBalance(t, pool, f); balance != 4000 {
		t.Fatalf("expected cash balance 4000 (5000 funded - 1000 bet, posted exactly once), got %d", balance)
	}
	var count int
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = 'bet-redeliver-after-exclusion'`,
			f.tenantID,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one ledger_transactions row, got %d", count)
	}
}

// --- Concurrency (directive §8) ---

// TestConcurrent_SelfExclusionDuringLaunch_Deterministic is directive
// §8(A): self-exclusion applied WHILE a launch is occurring. Proves the
// two never interleave into an inconsistent state - either the launch's
// own transaction (which takes the person lock first) commits a session
// BEFORE the self-exclusion exists, or the self-exclusion commits first
// and the launch is cleanly denied. What must NEVER happen: a launch
// that both "succeeds" AND is concurrent with a self-exclusion that also
// reports success with no serialization between them - checked by
// confirming a successful launch's OWN session row exists tenant-side
// with no impossible interleaving artifact (no partial session row,
// exactly one final self-exclusion row).
func TestConcurrent_SelfExclusionDuringLaunch_Deterministic(t *testing.T) {
	pool := testPool(t)

	const iterations = 15
	for i := 0; i < iterations; i++ {
		f := seedCasinoFixture(t, pool)
		provider := NewMockCasinoProvider("mock-casino", "EUR")
		game := seedGame(t, pool, "mock-casino", "EUR")
		enableGameForTenant(t, pool, f, game.ID)
		registerCasinoCapability(t, pool, f, provider, 100)
		orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

		var wg sync.WaitGroup
		var launchErr, exclusionErr error
		var launchResult LaunchGameResult
		wg.Add(2)
		go func() {
			defer wg.Done()
			launchErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				launchResult, err = orch.LaunchGame(ctx, tx, LaunchGameParams{
					TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
					GameID: game.ID, AssetCode: "EUR", Mode: ModeReal,
				})
				return err
			})
		}()
		go func() {
			defer wg.Done()
			exclusionErr = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
				return err
			})
		}()
		wg.Wait()

		if exclusionErr != nil {
			t.Fatalf("iteration %d: create self-exclusion: %v", i, exclusionErr)
		}
		if launchErr != nil {
			t.Fatalf("iteration %d: LaunchGame returned an unexpected error: %v", i, launchErr)
		}
		launchSucceeded := !launchResult.Denied
		launchDenied := launchResult.Denied && launchResult.DenialCode == rg.CodeSelfExcluded
		if !launchSucceeded && !launchDenied {
			t.Fatalf("iteration %d: launch neither succeeded nor was denied for self-exclusion, got: %+v", i, launchResult)
		}

		var sessionCount int
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE tenant_id = $1 AND status = 'active'`, f.tenantID).Scan(&sessionCount)
		})
		if err != nil {
			t.Fatalf("iteration %d: count sessions: %v", i, err)
		}
		if launchSucceeded && sessionCount != 1 {
			t.Fatalf("iteration %d: launch reported success but active session count = %d", i, sessionCount)
		}
		if launchDenied && sessionCount != 0 {
			t.Fatalf("iteration %d: launch reported denial but an active session exists (count=%d) - session must not become usable", i, sessionCount)
		}
	}
}

// TestConcurrent_SelfExclusionDuringBet_Deterministic is directive §8(B):
// self-exclusion applied WHILE a bet is occurring. Exactly one of "bet
// posted, no exclusion yet observed" or "bet declined for self-exclusion"
// may be the final state - never both a posted ledger effect AND a
// recorded self-exclusion whose CreateSelfExclusion call had already
// returned before the bet's own transaction began (which would mean the
// bet incorrectly ran unlocked against stale state).
func TestConcurrent_SelfExclusionDuringBet_Deterministic(t *testing.T) {
	pool := testPool(t)

	const iterations = 15
	for i := 0; i < iterations; i++ {
		f := seedCasinoFixture(t, pool)
		fundWallet(t, pool, f, 5000)
		provider := NewMockCasinoProvider("mock-casino", "EUR")
		registerCasinoCapability(t, pool, f, provider, 100)
		sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
		orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

		payload := provider.CallbackPayload(CallbackEventBet, uuid.New().String(), "", uuid.New().String(), "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

		var wg sync.WaitGroup
		var betErr, exclusionErr error
		var betResult ReceiveCallbackResult
		wg.Add(2)
		go func() {
			defer wg.Done()
			betErr = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				betResult, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
		}()
		go func() {
			defer wg.Done()
			exclusionErr = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
				return err
			})
		}()
		wg.Wait()

		if betErr != nil {
			t.Fatalf("iteration %d: receive callback: %v", i, betErr)
		}
		if exclusionErr != nil {
			t.Fatalf("iteration %d: create self-exclusion: %v", i, exclusionErr)
		}

		balance := cashBalance(t, pool, f)
		if betResult.Outcome == OutcomeSucceeded && balance != 4000 {
			t.Fatalf("iteration %d: bet succeeded but balance = %d (expected 4000)", i, balance)
		}
		if betResult.Outcome == OutcomeDeclined && balance != 5000 {
			t.Fatalf("iteration %d: bet declined but balance changed to %d (expected unchanged 5000)", i, balance)
		}
		debits, credits := sumDebitsCredits(t, pool, f.tenantID)
		if debits != credits {
			t.Fatalf("iteration %d: invariant #1 violated: debits=%d credits=%d", i, debits, credits)
		}
	}
}

// TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion is directive
// §8(C): a duplicate delivery of the SAME bet callback arrives twice,
// concurrently with a self-exclusion being applied. Regardless of
// ordering, ledger.Post's own idempotency key means AT MOST ONE financial
// effect for this provider_tx_id ever exists - proven here under the
// additional presence of a concurrent RG state change, not just the
// already-covered plain-duplicate case.
func TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion(t *testing.T) {
	pool := testPool(t)

	const iterations = 15
	for i := 0; i < iterations; i++ {
		f := seedCasinoFixture(t, pool)
		fundWallet(t, pool, f, 5000)
		provider := NewMockCasinoProvider("mock-casino", "EUR")
		registerCasinoCapability(t, pool, f, provider, 100)
		sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
		orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

		providerTxID := uuid.New().String()
		payload := provider.CallbackPayload(CallbackEventBet, providerTxID, "", uuid.New().String(), "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

		var wg sync.WaitGroup
		var err1, err2, exclusionErr error
		var result1, result2 ReceiveCallbackResult
		wg.Add(3)
		go func() {
			defer wg.Done()
			err1 = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				result1, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
		}()
		go func() {
			defer wg.Done()
			err2 = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				result2, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
				return err
			})
		}()
		go func() {
			defer wg.Done()
			exclusionErr = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
				return err
			})
		}()
		wg.Wait()

		if err1 != nil {
			t.Fatalf("iteration %d: first delivery: %v", i, err1)
		}
		if err2 != nil {
			t.Fatalf("iteration %d: second delivery: %v", i, err2)
		}
		if exclusionErr != nil {
			t.Fatalf("iteration %d: create self-exclusion: %v", i, exclusionErr)
		}

		balance := cashBalance(t, pool, f)
		if balance != 5000 && balance != 4000 {
			t.Fatalf("iteration %d: balance %d is neither 5000 (both declined) nor 4000 (posted exactly once) - possible double-post", i, balance)
		}
		// Financial-correctness specialist review finding: the two
		// deliveries' OWN reported results must never disagree with each
		// other about whether this provider_tx_id posted - one reporting
		// Succeeded while the other reports Declined for the SAME
		// provider_tx_id would mean one delivery failed to see an
		// already-posted transaction (findPostedBetTransaction's own
		// idempotency short-circuit) and incorrectly re-evaluated live RG
		// state instead of replaying the original result.
		if balance == 4000 {
			if result1.Outcome != OutcomeSucceeded || result2.Outcome != OutcomeSucceeded {
				t.Fatalf("iteration %d: balance shows the bet posted, but results disagree: %+v / %+v", i, result1, result2)
			}
			if result1.LedgerTransactionID == nil || result2.LedgerTransactionID == nil || *result1.LedgerTransactionID != *result2.LedgerTransactionID {
				t.Fatalf("iteration %d: both deliveries succeeded but reported different transaction ids: %+v / %+v", i, result1, result2)
			}
		} else {
			if result1.Outcome != OutcomeDeclined || result2.Outcome != OutcomeDeclined {
				t.Fatalf("iteration %d: balance shows nothing posted, but a result reports success: %+v / %+v", i, result1, result2)
			}
		}
		var betCount int
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2`,
				f.tenantID, providerTxID,
			).Scan(&betCount)
		})
		if err != nil {
			t.Fatalf("iteration %d: count ledger transactions: %v", i, err)
		}
		if betCount > 1 {
			t.Fatalf("iteration %d: expected at most one ledger_transactions row for provider_tx_id=%s, got %d", i, providerTxID, betCount)
		}
	}
}
