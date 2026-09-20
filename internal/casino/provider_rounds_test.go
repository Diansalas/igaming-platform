//go:build integration

// Stage 8 (docs/decisions/0080-provider-integration-readiness-without-
// external-contracts.md, Decision 1) - proves casino_provider_rounds
// (migration 0080) and BindProviderRound (rounds.go) actually deliver the
// property the ADR exists for: a provider-declared round id is durably
// bound to exactly one (tenant, session, player, brand) the first time it
// is bet on, and a later delivery naming the SAME round id under a
// DIFFERENT session/player/brand (in the same tenant) is rejected outright
// - never silently overwritten - with the whole postBet operation aborted
// (no partial ledger effect). Follows this package's own
// orchestrator_integration_test.go fixture/testPool conventions exactly.
package casino

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// countLedgerTransactionsForProviderTx reports how many ledger_transactions
// rows exist for (tenantID, providerID, providerTxID) - directive-style
// idempotency/no-partial-post verification, mirroring the inline count
// queries orchestrator_integration_test.go's own stress tests already use
// (e.g. TestConcurrentStress_ManyDuplicateBetDeliveries), factored out here
// since this file needs it from more than one test.
func countLedgerTransactionsForProviderTx(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID, providerTxID string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3`,
			tenantID, providerID, providerTxID,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count ledger transactions for provider_tx_id=%s: %v", providerTxID, err)
	}
	return count
}

// countProviderRoundRows reports how many casino_provider_rounds rows exist
// for (tenantID, providerID, providerRoundID) under tenant-staff scope.
func countProviderRoundRows(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID, providerRoundID string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM casino_provider_rounds WHERE tenant_id = $1 AND provider_id = $2 AND provider_round_id = $3`,
			tenantID, providerID, providerRoundID,
		).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count casino_provider_rounds for provider_round_id=%s: %v", providerRoundID, err)
	}
	return count
}

// readProviderRoundTimestamps reads the single casino_provider_rounds row
// for (tenantID, providerID, providerRoundID) - test callers only invoke
// this once exactly one row is already known to exist (countProviderRoundRows
// == 1).
func readProviderRoundTimestamps(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID, providerRoundID string) (firstSeen, lastSeen time.Time) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT first_seen_at, last_seen_at FROM casino_provider_rounds WHERE tenant_id = $1 AND provider_id = $2 AND provider_round_id = $3`,
			tenantID, providerID, providerRoundID,
		).Scan(&firstSeen, &lastSeen)
	})
	if err != nil {
		t.Fatalf("read casino_provider_rounds timestamps for provider_round_id=%s: %v", providerRoundID, err)
	}
	return firstSeen, lastSeen
}

// TestPostBet_BindsProviderRound_IdempotentAcrossTwoBetsOnSameRound proves
// the ADR's core binding property: two DISTINCT bets (different
// provider_tx_id - a redelivery of the SAME provider_tx_id would short-
// circuit at postBet's own pre-existing idempotency check before ever
// reaching BindProviderRound, per findPostedBetTransaction's doc comment)
// posted on the SAME round, by the SAME session/player, upsert to exactly
// ONE casino_provider_rounds row, with last_seen_at advancing on the
// second bet.
func TestPostBet_BindsProviderRound_IdempotentAcrossTwoBetsOnSameRound(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	const roundID = "round-idempotent-bind"

	firstPayload := provider.CallbackPayload(CallbackEventBet, "bet-idempotent-bind-1", "", roundID, "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", firstPayload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected first bet to succeed, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("first ReceiveCallback: %v", err)
	}

	if got := countProviderRoundRows(t, pool, f.tenantID, "mock-casino", roundID); got != 1 {
		t.Fatalf("expected exactly 1 casino_provider_rounds row after the first bet, got %d", got)
	}
	firstSeenAt, lastSeenAfterFirst := readProviderRoundTimestamps(t, pool, f.tenantID, "mock-casino", roundID)

	// Real wall-clock time must advance between the two binds for
	// last_seen_at to be provably different (Postgres now() resolves to
	// microseconds, two separate transactions on a live clock virtually
	// always differ - this sleep just removes any doubt).
	time.Sleep(10 * time.Millisecond)

	secondPayload := provider.CallbackPayload(CallbackEventBet, "bet-idempotent-bind-2", "", roundID, "game-1", 500, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", secondPayload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected second bet on the same round to succeed, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("second ReceiveCallback: %v", err)
	}

	if got := countProviderRoundRows(t, pool, f.tenantID, "mock-casino", roundID); got != 1 {
		t.Fatalf("expected STILL exactly 1 casino_provider_rounds row after a second bet on the same round (idempotent upsert), got %d", got)
	}
	newFirstSeenAt, lastSeenAfterSecond := readProviderRoundTimestamps(t, pool, f.tenantID, "mock-casino", roundID)

	if !newFirstSeenAt.Equal(firstSeenAt) {
		t.Fatalf("expected first_seen_at to stay fixed at %v, got %v", firstSeenAt, newFirstSeenAt)
	}
	if !lastSeenAfterSecond.After(lastSeenAfterFirst) {
		t.Fatalf("expected last_seen_at to advance past %v after the second bet, got %v", lastSeenAfterFirst, lastSeenAfterSecond)
	}
}

// TestPostBet_SecondPlayerSameProviderRound_RejectedAndNoLedgerEffect proves
// the actual security property Stage 8 requires: a provider_round_id
// already bound (same tenant, same provider) to player A's session cannot
// be claimed by a DIFFERENT player B's session, even in the SAME brand -
// BindProviderRound must reject with ErrProviderRoundOwnershipConflict, and
// player B's bet must post ZERO ledger effect (the whole postBet operation
// aborts, never a partial post).
func TestPostBet_SecondPlayerSameProviderRound_RejectedAndNoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fundWallet(t, pool, fA, 100000)

	playerB, walletB := seedSecondPlayerWallet(t, pool, fA)
	fB := casinoFixture{tenantID: fA.tenantID, brandID: fA.brandID, playerAccountID: playerB, walletID: walletB}
	fundWallet(t, pool, fB, 100000)

	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, fA, provider, 100)
	sessionA := mintSession(t, pool, fA, "mock-casino", "EUR")
	sessionB := mintSession(t, pool, fB, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	const sharedRoundID = "round-cross-player-collision"

	betAPayload := provider.CallbackPayload(CallbackEventBet, "bet-player-a", "", sharedRoundID, "game-1", 1000, "EUR", OutcomeSucceeded, "", fA.playerAccountID, sessionA)
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, fA.tenantID, "mock-casino", betAPayload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected player A's bet to succeed, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player A ReceiveCallback: %v", err)
	}

	balanceBBefore := cashBalance(t, pool, fB)

	betBPayload := provider.CallbackPayload(CallbackEventBet, "bet-player-b", "", sharedRoundID, "game-1", 750, "EUR", OutcomeSucceeded, "", fB.playerAccountID, sessionB)
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, fA.tenantID, "mock-casino", betBPayload)
		return err
	})
	if !errors.Is(err, ErrProviderRoundOwnershipConflict) {
		t.Fatalf("expected ErrProviderRoundOwnershipConflict for player B's collision, got %v", err)
	}

	// No partial post: player B's own provider_tx_id must never have
	// reached ledger_transactions at all, and their balance must be
	// untouched.
	if got := countLedgerTransactionsForProviderTx(t, pool, fA.tenantID, "mock-casino", "bet-player-b"); got != 0 {
		t.Fatalf("expected zero ledger_transactions rows for player B's rejected bet, got %d", got)
	}
	if got := cashBalance(t, pool, fB); got != balanceBBefore {
		t.Fatalf("expected player B's balance to be untouched (%d), got %d", balanceBBefore, got)
	}

	// The original binding (player A's) must remain exactly as it was -
	// still exactly one row, still owned by player A.
	if got := countProviderRoundRows(t, pool, fA.tenantID, "mock-casino", sharedRoundID); got != 1 {
		t.Fatalf("expected exactly 1 casino_provider_rounds row (player A's, untouched), got %d", got)
	}
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		round, err := LookupProviderRound(ctx, tx, fA.tenantID, "mock-casino", sharedRoundID)
		if err != nil {
			return err
		}
		if round.PlayerAccountID != fA.playerAccountID {
			t.Fatalf("expected the surviving binding to still belong to player A (%s), got %s", fA.playerAccountID, round.PlayerAccountID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("lookup surviving provider round: %v", err)
	}
}

// TestPostBet_SameProviderRoundIDDifferentTenant_IndependentRows proves the
// ADR's own "Uniqueness scope" decision (docs/decisions/0080-provider-
// integration-readiness-without-external-contracts.md, Decision 1): the
// SAME literal provider_round_id string used by two GENUINELY DIFFERENT
// tenants must bind as two fully independent casino_provider_rounds rows -
// the unique constraint is scoped to (tenant_id, provider_id,
// provider_round_id), never provider_id alone / never global.
func TestPostBet_SameProviderRoundIDDifferentTenant_IndependentRows(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	fundWallet(t, pool, fA, 100000)
	fundWallet(t, pool, fB, 100000)

	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, fA, provider, 100)
	registerCasinoCapability(t, pool, fB, provider, 100)
	sessionA := mintSession(t, pool, fA, "mock-casino", "EUR")
	sessionB := mintSession(t, pool, fB, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	const sharedRoundID = "round-shared-literal-across-tenants"

	betAPayload := provider.CallbackPayload(CallbackEventBet, "bet-tenant-a", "", sharedRoundID, "game-1", 1000, "EUR", OutcomeSucceeded, "", fA.playerAccountID, sessionA)
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, fA.tenantID, "mock-casino", betAPayload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected tenant A's bet to succeed, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant A ReceiveCallback: %v", err)
	}

	betBPayload := provider.CallbackPayload(CallbackEventBet, "bet-tenant-b", "", sharedRoundID, "game-1", 1000, "EUR", OutcomeSucceeded, "", fB.playerAccountID, sessionB)
	err = pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, fB.tenantID, "mock-casino", betBPayload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected tenant B's bet (SAME literal round id, different tenant) to succeed independently, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("tenant B ReceiveCallback: %v", err)
	}

	if got := countProviderRoundRows(t, pool, fA.tenantID, "mock-casino", sharedRoundID); got != 1 {
		t.Fatalf("expected exactly 1 casino_provider_rounds row for tenant A, got %d", got)
	}
	if got := countProviderRoundRows(t, pool, fB.tenantID, "mock-casino", sharedRoundID); got != 1 {
		t.Fatalf("expected exactly 1 casino_provider_rounds row for tenant B, got %d", got)
	}
}

// TestPostBet_SameBrandTenantButDifferentBrand_RejectedAndNoLedgerEffect
// proves brand is part of what an existing binding's ownership pins:
// a second player in a DIFFERENT brand of the SAME tenant naming an
// already-bound provider_round_id is rejected identically to the cross-
// player-same-brand case above - never silently rebranded.
func TestPostBet_DifferentBrandSameTenant_RejectedAndNoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fundWallet(t, pool, fA, 100000)

	sibling := seedSiblingBrand(t, pool, fA.tenantID)
	fundWallet(t, pool, sibling, 100000)

	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, fA, provider, 100)
	sessionA := mintSession(t, pool, fA, "mock-casino", "EUR")
	sessionSibling := mintSession(t, pool, sibling, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})

	const sharedRoundID = "round-cross-brand-collision"

	betAPayload := provider.CallbackPayload(CallbackEventBet, "bet-brand-a", "", sharedRoundID, "game-1", 1000, "EUR", OutcomeSucceeded, "", fA.playerAccountID, sessionA)
	err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		result, err := orch.ReceiveCallback(ctx, tx, fA.tenantID, "mock-casino", betAPayload)
		if err != nil {
			return err
		}
		if result.Outcome != OutcomeSucceeded {
			t.Fatalf("expected brand A's bet to succeed, got %+v", result)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("brand A ReceiveCallback: %v", err)
	}

	balanceSiblingBefore := cashBalance(t, pool, sibling)

	betSiblingPayload := provider.CallbackPayload(CallbackEventBet, "bet-brand-sibling", "", sharedRoundID, "game-1", 750, "EUR", OutcomeSucceeded, "", sibling.playerAccountID, sessionSibling)
	err = pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, fA.tenantID, "mock-casino", betSiblingPayload)
		return err
	})
	if !errors.Is(err, ErrProviderRoundOwnershipConflict) {
		t.Fatalf("expected ErrProviderRoundOwnershipConflict for the sibling brand's collision, got %v", err)
	}

	if got := countLedgerTransactionsForProviderTx(t, pool, fA.tenantID, "mock-casino", "bet-brand-sibling"); got != 0 {
		t.Fatalf("expected zero ledger_transactions rows for the sibling brand's rejected bet, got %d", got)
	}
	if got := cashBalance(t, pool, sibling); got != balanceSiblingBefore {
		t.Fatalf("expected the sibling brand player's balance to be untouched (%d), got %d", balanceSiblingBefore, got)
	}
	if got := countProviderRoundRows(t, pool, fA.tenantID, "mock-casino", sharedRoundID); got != 1 {
		t.Fatalf("expected exactly 1 casino_provider_rounds row (brand A's, untouched), got %d", got)
	}
}

// TestPostBet_DeclinedByRG_BindsNoProviderRound proves the Stage 8 review
// fix (P1): BindProviderRound must run in postBet ONLY after RG
// eligibility, Risk, and the insufficient-funds check have all already
// passed - never before. All three decline paths return
// (OutcomeDeclined, nil error), a nil error, so their enclosing
// transaction still COMMITS - if BindProviderRound ran earlier (alongside
// the session/asset-match checks), a bet that is never actually going to
// post would still durably claim the round id, potentially pre-empting
// the legitimate bet that later (re)tries it.
func TestPostBet_DeclinedByRG_BindsNoProviderRound(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider})
	selfExclude(t, pool, f)

	const roundID = "round-rg-blocked-no-bind"
	payload := provider.CallbackPayload(CallbackEventBet, "bet-rg-blocked-no-bind", "", roundID, "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeDeclined {
		t.Fatalf("expected the bet to be RG-declined, got %+v", result)
	}
	if got := countProviderRoundRows(t, pool, f.tenantID, "mock-casino", roundID); got != 0 {
		t.Fatalf("expected ZERO casino_provider_rounds rows for a round whose only delivery was RG-declined, got %d", got)
	}
}

// TestBindProviderRound_SameProviderRoundIDDifferentLaunchSession_SamePlayerBrandSucceeds
// proves the Stage 8 review fix (P2): the ownership-conflict predicate is
// scoped to (player_account_id, brand_id) only - NOT launch_session_id -
// so a legitimate continuation of the SAME round by the SAME player/brand
// under a DIFFERENT launch session (e.g. a free-spins round spanning a
// session timeout) succeeds and advances launch_session_id to the newer
// session, rather than being rejected as a conflict.
func TestBindProviderRound_SameProviderRoundIDDifferentLaunchSession_SamePlayerBrandSucceeds(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	sessionOne := mintSession(t, pool, f, "mock-casino", "EUR")
	sessionTwo := mintSession(t, pool, f, "mock-casino", "EUR")

	const roundID = "round-session-continuation"

	var gameID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT game_id FROM casino_launch_sessions WHERE id = $1`, sessionOne).Scan(&gameID); err != nil {
			return err
		}
		return BindProviderRound(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, sessionOne, gameID, "mock-casino", roundID, nil)
	})
	if err != nil {
		t.Fatalf("bind under the first launch session: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return BindProviderRound(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, sessionTwo, gameID, "mock-casino", roundID, nil)
	})
	if err != nil {
		t.Fatalf("expected a same-player/same-brand continuation under a DIFFERENT launch session to succeed, got %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		round, err := LookupProviderRound(ctx, tx, f.tenantID, "mock-casino", roundID)
		if err != nil {
			return err
		}
		if round.LaunchSessionID != sessionTwo {
			t.Fatalf("expected launch_session_id to have advanced to the second session %s, got %s", sessionTwo, round.LaunchSessionID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("lookup after continuation: %v", err)
	}
}

// TestCasinoProviderRoundsTrigger_RejectsIdentityColumnUpdate proves
// migration 0080's casino_provider_rounds_immutable_fields trigger rejects
// a direct SQL UPDATE changing player_account_id or provider_round_id on
// an existing row - enforced at the database level, not by application
// discipline (CLAUDE.md's "Financial / ledger rules": "enforced by ... not
// by discipline in application code").
func TestCasinoProviderRoundsTrigger_RejectsIdentityColumnUpdate(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")

	const roundID = "round-trigger-immutability"
	var gameID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT game_id FROM casino_launch_sessions WHERE id = $1`, sessionID).Scan(&gameID); err != nil {
			return err
		}
		return BindProviderRound(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, sessionID, gameID, "mock-casino", roundID, nil)
	})
	if err != nil {
		t.Fatalf("seed binding: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE casino_provider_rounds SET player_account_id = $1 WHERE tenant_id = $2 AND provider_id = 'mock-casino' AND provider_round_id = $3`,
			uuid.New(), f.tenantID, roundID)
		return err
	})
	if err == nil {
		t.Fatal("expected the trigger to reject a direct UPDATE changing player_account_id, got nil error")
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE casino_provider_rounds SET provider_round_id = 'round-trigger-immutability-hijacked' WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_round_id = $2`,
			f.tenantID, roundID)
		return err
	})
	if err == nil {
		t.Fatal("expected the trigger to reject a direct UPDATE changing provider_round_id, got nil error")
	}

	if got := countProviderRoundRows(t, pool, f.tenantID, "mock-casino", roundID); got != 1 {
		t.Fatalf("expected the original row to still exist, untouched, got %d matching rows", got)
	}
}
