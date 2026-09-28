//go:build integration

// ADR 0082 §6 tests 1 and 4 - the two previously-proven cycles whose first
// edge is taken inside internal/casino.
package casino

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

func loCasinoAccount(t *testing.T, pool *db.Pool, f casinoFixture, walletID *uuid.UUID, at ledger.AccountType) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, walletID, at, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("resolve %s account: %v", at, err)
	}
	return id
}

// --- §6 test 1: LOCK-1 -----------------------------------------------------

// TestLockOrder_ConcurrentBetAndWinOnSameWallet_NoDeadlock is finding
// LOCK-1 itself, stated exactly as ADR 0082 §1.4 states it:
//
//	postBet acquires player_cash (EXPLICITLY, via the now-deleted
//	lockCashBalance, before ledger.Post is ever entered) and then
//	house_gaming (IMPLICITLY, via migration 0023's AFTER INSERT trigger
//	on its second entry). postWinDirectCash acquires house_gaming and
//	then player_cash, because Flow 6's entry slice is [house Dr, cash Cr].
//	Same two rows, opposite order.
//
// This is also the test that proves the Stage 9 proposal ("sort the
// entries inside ledger.Post") was insufficient: a sort inside Post
// reorders postWinDirectCash's two locks and postBet's second one, but
// postBet's player_cash lock is ALREADY HELD before Post is called, so
// the cycle survives the sort. Only moving the lock acquisition itself
// into ledger.LockProjectionsForPosting - over the COMPLETE account set,
// before any of it is taken - closes it.
//
// Shown failing on HEAD with SQLSTATE 40P01 before the fix.
//
// The bet and the win deliberately name DIFFERENT rounds and different
// provider_tx_ids, so nothing else serializes them: postBet's L0.1
// delivery advisory lock is keyed on provider_tx_id and postWin's L2
// lock is keyed on the round's own correlation id.
func TestLockOrder_ConcurrentBetAndWinOnSameWallet_NoDeadlock(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// Seed a settled round so the win below has a real, unreversed bet to
	// settle - resolveWinOrigin derives the credited wallet and account
	// TYPE from that bet's own ledger entries, never from the payload.
	const settledRound = "lockorder-settled-round"
	seedBet := provider.CallbackPayload(f.tenantID, CallbackEventBet, "lockorder-seed-bet", "", settledRound, "game-1",
		1_000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", seedBet)
		return err
	}); err != nil {
		t.Fatalf("seed bet: %v", err)
	}

	cashID := loCasinoAccount(t, pool, f, &f.walletID, ledger.AccountPlayerCash)
	houseID := loCasinoAccount(t, pool, f, nil, ledger.AccountHouseGaming)

	blockerCash := loHoldProjectionRow(t, pool, f.tenantID, cashID, "player_cash")
	blockerHouse := loHoldProjectionRow(t, pool, f.tenantID, houseID, "house_gaming")

	betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "lockorder-race-bet", "", "lockorder-race-round", "game-1",
		500, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	winPayload := provider.CallbackPayload(f.tenantID, CallbackEventWin, "lockorder-race-win", "", settledRound, "game-1",
		700, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)

	var betResult ReceiveCallbackResult
	startBet := func() *loRacer {
		return loStartRacer(t, pool, f.tenantID, "postBet(cash,house)", func(ctx context.Context, tx pgx.Tx) error {
			var err error
			betResult, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
			return err
		})
	}
	startWin := func() *loRacer {
		return loStartRacer(t, pool, f.tenantID, "postWinDirectCash(house,cash)", func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", winPayload)
			return err
		})
	}

	racerBet, racerWin, errBet, errWin := loRunABBA(t, pool, startBet, startWin, []*loBlocker{blockerCash, blockerHouse})
	loAssertNoDeadlock(t, "LOCK-1 (bet vs. win on one wallet)",
		map[string]error{racerBet.name: errBet, racerWin.name: errWin})
	if errBet != nil {
		t.Fatalf("the bet must resolve to an outcome, never an error: %v", errBet)
	}
	if errWin != nil {
		t.Fatalf("the win must settle: %v", errWin)
	}
	if betResult.Outcome != OutcomeSucceeded {
		t.Fatalf("the wallet is funded for both; expected the bet to succeed, got %q/%q",
			betResult.Outcome, betResult.DeclineReason)
	}

	// 100000 funded - 1000 seed bet - 500 race bet + 700 win.
	if want, got := int64(100_000-1_000-500+700), cashBalance(t, pool, f); got != want {
		t.Fatalf("expected player_cash %d, got %d", want, got)
	}
	loAssertBalanced(t, pool, f.tenantID)
	loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
}

// --- §6 test 4: LOCK-1d ----------------------------------------------------

// TestLockOrder_ConcurrentBetAndGrantConversion_NoDeadlock is finding
// LOCK-1d: the cross-class cycle between a wallet_balance_projection ROW
// lock and a bonus grant ADVISORY lock, which no amount of projection-row
// sorting touches.
//
//	casino.postBet holds player_cash (and house_gaming) from BEFORE
//	ledger.Post and then, AFTER Post returns, calls
//	bonus.RecordCashFundedWageringContribution, which acquires the grant
//	advisory lock (HR-10 requires the contribution stay inside the bet's
//	own transaction, so it cannot simply be moved out).
//	bonus.applyGrantConversion does the exact reverse: it holds the grant
//	advisory lock from ConvertGrant's first statement and only then takes
//	player_cash, inside Post.
//
// ADR 0082 §3.3 closes it with class L0.2, a PLAYER-scoped advisory lock
// taken before both: postBet takes it directly, and every grant-level
// caller gets it for free because AdvisoryLockGrant now acquires it as an
// internal precondition.
//
// Sub-test "grant advisory then player_cash posting" is the cycle itself
// and is the one shown failing on HEAD with 40P01. It drives the bonus
// side through the REAL primitives - bonus.AdvisoryLockGrant followed by
// a ledger.Post of applyGrantConversion's exact two-leg shape
// (Dr player_bonus / Cr player_cash) - rather than through ConvertGrant,
// for a reason worth recording rather than hiding:
//
//	ConvertGrant requires the Grant to be in status `completed`, while
//	postBet's contribution path only ever locks a Grant that is
//	`activated`/`in_progress` (HasActiveWageringGrant /
//	listActiveWageringGrants). So on today's code the SAME Grant cannot
//	simultaneously satisfy both sides through their production entry
//	points, which means ADR 0082 §1.8's "same player, same grant"
//	framing overstates how reachable LOCK-1d is via ConvertGrant
//	specifically. The LOCK ORDERING it describes is nonetheless exactly
//	what applyGrantConversion does, is exactly what a held-disposition
//	ACTION_ROUTE_TO_CASH resolution also does, and would become
//	reachable the moment any grant-advisory-holding path that posts to
//	player_cash becomes callable for an active Grant. The cycle is
//	therefore reproduced at the level of the real locking primitives,
//	and the fix is proven to close it, rather than the test being
//	quietly downgraded to something that cannot fail.
//
// Sub-test "production ConvertGrant against a concurrent bet" then runs
// the real, fully-gated production paths and asserts they neither
// deadlock nor mis-post.
func TestLockOrder_ConcurrentBetAndGrantConversion_NoDeadlock(t *testing.T) {
	t.Run("grant advisory then player_cash posting", func(t *testing.T) {
		pool := testPool(t)
		f := seedCasinoFixture(t, pool)
		fundWallet(t, pool, f, 100_000)
		provider := NewMockCasinoProvider("mock-casino", "EUR")
		registerCasinoCapability(t, pool, f, provider, 100)
		sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
		orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

		co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
		grantID := seedActivatedGrant(t, pool, f, co, "lockorder-lock1d")

		cashID := loCasinoAccount(t, pool, f, &f.walletID, ledger.AccountPlayerCash)
		bonusID := loCasinoAccount(t, pool, f, &f.walletID, ledger.AccountPlayerBonus)

		// Seed the player_bonus side so the conversion-shaped posting has
		// value to move, and so every projection row involved already
		// exists (this test isolates the ENTRY/ADVISORY ordering cycle,
		// not the absent-row case).
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxBonusGrant,
				IdempotencyKey: "lockorder-lock1d-seed", CorrelationID: uuid.New(),
				Entries:   []ledger.EntryInput{{LedgerAccountID: bonusID, Direction: ledger.Credit, Amount: 5_000}},
				BonusCost: &ledger.BonusCostAttribution{Funding: ledger.FundingOperator},
			})
			return err
		}); err != nil {
			t.Fatalf("seed bonus balance: %v", err)
		}

		blockerCash := loHoldProjectionRow(t, pool, f.tenantID, cashID, "player_cash")
		blockerGrant := loHoldWith(t, pool, f.tenantID, "bonus_grant advisory", func(ctx context.Context, tx pgx.Tx) error {
			return bonus.AdvisoryLockGrant(ctx, tx, f.tenantID, grantID)
		})

		betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "lockorder-1d-bet", "", "lockorder-1d-round", "game-1",
			400, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

		// A: the bet. Holds player_cash + house_gaming, then wants the
		// grant advisory lock for its wagering contribution.
		startBet := func() *loRacer {
			return loStartRacer(t, pool, f.tenantID, "postBet(cash -> grant advisory)", func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
				return err
			})
		}
		// B: applyGrantConversion's exact lock sequence and exact posting
		// shape. Holds the grant advisory lock, then wants player_cash.
		startConvert := func() *loRacer {
			return loStartRacer(t, pool, f.tenantID, "grantConversion(advisory -> cash)", func(ctx context.Context, tx pgx.Tx) error {
				if err := bonus.AdvisoryLockGrant(ctx, tx, f.tenantID, grantID); err != nil {
					return err
				}
				_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
					TenantID: f.tenantID, TransactionType: ledger.TxBonusConversion,
					IdempotencyKey: "lockorder-1d-conversion", CorrelationID: uuid.New(),
					Entries: []ledger.EntryInput{
						{LedgerAccountID: bonusID, Direction: ledger.Debit, Amount: 5_000},
						{LedgerAccountID: cashID, Direction: ledger.Credit, Amount: 5_000},
					},
					BonusCost: &ledger.BonusCostAttribution{Funding: ledger.FundingOperator},
				})
				return err
			})
		}

		racerBet, racerConvert, errBet, errConvert := loRunABBA(t, pool, startBet, startConvert, []*loBlocker{blockerCash, blockerGrant})
		loAssertNoDeadlock(t, "LOCK-1d (projection row vs. grant advisory lock)",
			map[string]error{racerBet.name: errBet, racerConvert.name: errConvert})
		if errBet != nil {
			t.Fatalf("the bet must resolve to an outcome, never an error: %v", errBet)
		}
		if errConvert != nil {
			t.Fatalf("the conversion-shaped posting must succeed: %v", errConvert)
		}
		loAssertBalanced(t, pool, f.tenantID)
		loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
	})

	t.Run("production ConvertGrant against a concurrent bet", func(t *testing.T) {
		pool := testPool(t)
		f := seedCasinoFixture(t, pool)
		fundWallet(t, pool, f, 100_000)
		provider := NewMockCasinoProvider("mock-casino", "EUR")
		registerCasinoCapability(t, pool, f, provider, 100)
		sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
		orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

		co := seedBonusCampaignOffer(t, pool, f.tenantID, f.brandID)
		// One Grant the conversion will act on (completed) and one the
		// bet's wagering-contribution path will lock (activated) - the only
		// arrangement today's status gating actually permits.
		convertGrant := seedActivatedGrant(t, pool, f, co, "lockorder-1d-convert")
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := bonus.UpdateGrantStatus(ctx, tx, f.tenantID, convertGrant,
				bonus.GrantActivated, bonus.GrantCompleted, time.Now().UTC())
			return err
		}); err != nil {
			t.Fatalf("move the conversion grant to completed: %v", err)
		}

		// A seed bet, so house_gaming has a projection row to hold (this
		// sub-test isolates the ordering cycle, not the absent-row case).
		seedBet := provider.CallbackPayload(f.tenantID, CallbackEventBet, "lockorder-1d-prod-seed", "", "lockorder-1d-prod-seed-round",
			"game-1", 100, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", seedBet)
			return err
		}); err != nil {
			t.Fatalf("seed bet: %v", err)
		}

		// The active Grant is seeded AFTER the seed bet, and the ordering is
		// load-bearing rather than cosmetic. This fixture's Offer carries no
		// wagering multiplier and its Grant no granted_amount, so the FIRST
		// cash-funded contribution against a Grant also completes it
		// (RecordCashFundedWageringContribution -> checkWageringCompletion ->
		// CheckAndCompleteGrant). Seeded before the seed bet - as it was
		// until this assertion was added - the Grant was already `completed`
		// by the time the RACING bet ran, so that bet found no active
		// wagering Grant, never took a grant advisory lock at all, and the
		// sub-test silently stopped exercising the grant side of LOCK-1d
		// while still passing. Seeding it here keeps it `activated` for the
		// racing bet, which is what loAssertWageringContribution below then
		// proves actually happened.
		activeGrant := seedActivatedGrant(t, pool, f, co, "lockorder-1d-active")

		cashID := loCasinoAccount(t, pool, f, &f.walletID, ledger.AccountPlayerCash)
		houseID := loCasinoAccount(t, pool, f, nil, ledger.AccountHouseGaming)
		blockerCash := loHoldProjectionRow(t, pool, f.tenantID, cashID, "player_cash")
		blockerHouse := loHoldProjectionRow(t, pool, f.tenantID, houseID, "house_gaming")

		betPayload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "lockorder-1d-prod-bet", "", "lockorder-1d-prod-round", "game-1",
			400, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)

		startBet := func() *loRacer {
			return loStartRacer(t, pool, f.tenantID, "postBet", func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", betPayload)
				return err
			})
		}
		startConvert := func() *loRacer {
			return loStartRacer(t, pool, f.tenantID, "bonus.ConvertGrant", func(ctx context.Context, tx pgx.Tx) error {
				_, err := bonus.ConvertGrant(ctx, tx, f.tenantID, convertGrant, nil, bonus.ConvertGrantParams{
					ActorType: bonus.ActorSystem, ActorID: uuid.New(),
				})
				return err
			})
		}

		racerBet, racerConvert, errBet, errConvert := loRunABBA(t, pool, startBet, startConvert, []*loBlocker{blockerCash, blockerHouse})
		loAssertNoDeadlock(t, "LOCK-1d (production ConvertGrant vs. bet)",
			map[string]error{racerBet.name: errBet, racerConvert.name: errConvert})
		if errBet != nil {
			t.Fatalf("the bet must resolve to an outcome, never an error: %v", errBet)
		}
		if errConvert != nil {
			t.Fatalf("ConvertGrant must not fail: %v", errConvert)
		}
		// The racing bet must actually have taken activeGrant's OWN L0.3
		// advisory lock, or this sub-test degenerates into two unrelated
		// transactions and proves nothing about grant-path contention.
		// postBet records a cash-funded wagering contribution against the
		// single active grant inside the bet's own transaction (HR-10), and
		// that call is AdvisoryLockGrant's caller - so the contribution row
		// carrying the racing bet's own stake is the observable proof the
		// lock was taken while ConvertGrant held the player scope for the
		// other grant. Asserted rather than assumed: before this, the
		// fixture created activeGrant and immediately discarded it.
		loAssertWageringContribution(t, pool, f.tenantID, activeGrant, 400)
		loAssertBalanced(t, pool, f.tenantID)
		loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
	})
}

// --- F-1: bootstrap vs. the first postBet of a round -----------------------

// TestLockOrder_BootstrapAndFirstBetOfRound_NoDeadlock is the architect
// review's F-1 finding (docs/plans/prh2-hardening-round/reviews/
// b-architect-qa-pop.md), reproduced against the real locking primitives on
// both sides:
//
//	BootstrapLaunch's getLaunchSessionForBootstrap locks the SAME
//	casino_launch_sessions row bootstrap is consuming, then (via
//	bootstrapGateDenialReason -> evaluateAndAuditEligibility) takes RG's
//	person-scoped advisory lock (rg.go's lockPerson, called from
//	rg.EvaluateEligibility).
//
//	postBet, on an active session, takes the SAME RG advisory lock FIRST
//	(evaluateAndAuditEligibility, orchestrator.go ~:1570), and only
//	afterwards calls BindProviderRound (~:1785), whose INSERT into
//	casino_provider_rounds carries a foreign key to casino_launch_sessions
//	(migration 0080) - Postgres takes an IMPLICIT FOR KEY SHARE lock on the
//	referenced session row to enforce that FK, with no explicit SQL
//	anywhere naming it.
//
// Session-then-RG vs. RG-then-session on the same two resources is the
// textbook ABBA shape. B is always the REAL production BindProviderRound
// plus the real rg.EvaluateEligibility. A's own lock-acquisition step is
// what security's B-C3 finding corrected (docs/plans/prh2-hardening-round/
// reviews/b-security.md): the ORIGINAL version of this test built its own
// `fmt.Sprintf("... %s", lockClause)` query for BOTH the mutant and the fix
// case, so it never actually called getLaunchSessionForBootstrap at all -
// security changed bootstrap.go:242 back to FOR UPDATE and every test in
// this suite, including this one, still passed. Fixed:
//
//   - "harness control: FOR UPDATE deadlocks (mutant)" keeps the
//     Sprintf-built raw query, on purpose - it exists ONLY to prove the
//     harness itself (loRunABBA, the blockers, loIsDeadlock) can detect a
//     40P01 in this exact two-resource shape, never to stand in for
//     production code. This is why it is a "harness control", not "the
//     mutant case of the production test" - it was the earlier design's
//     naming that blurred the two.
//   - "production: getLaunchSessionForBootstrap (fix)" is the ONLY
//     sub-test that pins production behaviour: racer A calls
//     getLaunchSessionForBootstrap directly - the exact function
//     BootstrapLaunch itself calls, not a hand-written equivalent query -
//     so a regression of that function's own lock clause is what this
//     sub-test is wired to catch. Re-applying the exact mutant this
//     finding names (bootstrap.go:242, FOR NO KEY UPDATE -> FOR UPDATE)
//     and re-running ONLY this sub-test is recorded as killing it in
//     docs/plans/payment-readiness/evidence/prh2-casino-b-mutation-kill.txt
//     (B-C3 entry) - the harness control above is not sufficient evidence
//     of that on its own, which is exactly what B-C3 found.
func TestLockOrder_BootstrapAndFirstBetOfRound_NoDeadlock(t *testing.T) {
	t.Run("harness control: FOR UPDATE deadlocks (mutant)", func(t *testing.T) {
		runBootstrapFirstBetLockOrderCase(t, func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, tokenHash string) error {
			var discard uuid.UUID
			return tx.QueryRow(ctx,
				`SELECT id FROM casino_launch_sessions WHERE token_hash = $1 AND tenant_id = $2 FOR UPDATE`,
				tokenHash, tenantID).Scan(&discard)
		}, true)
	})
	t.Run("production: getLaunchSessionForBootstrap (fix)", func(t *testing.T) {
		runBootstrapFirstBetLockOrderCase(t, func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, tokenHash string) error {
			_, _, err := getLaunchSessionForBootstrap(ctx, tx, tenantID, tokenHash)
			return err
		}, false)
	})
}

// runBootstrapFirstBetLockOrderCase is shared by both sub-tests above.
// lockSession is racer A's own session-row-acquisition step - either the
// harness control's raw Sprintf query, or (for the sub-test that actually
// pins production behaviour) a direct call to getLaunchSessionForBootstrap
// itself.
func runBootstrapFirstBetLockOrderCase(t *testing.T, lockSession func(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, tokenHash string) error, wantDeadlock bool) {
	t.Helper()
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 100_000)
	game := seedGame(t, pool, "mock-casino", "EUR")
	session, token := mintSessionForBootstrap(t, pool, f, game, ModeReal, "EUR", 0)
	sessionID := session.ID
	tokenHash := hashLaunchToken(token)

	elig := rg.EligibilityParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
	}

	// blockerSession forces racer A (the bootstrap-sim) to queue on
	// the session row before it ever reaches the RG lock - ANY
	// conflicting mode does this, so the blocker itself always uses
	// the strongest lock (FOR UPDATE), independently of which
	// acquisition step A itself uses below.
	blockerSession := loHoldWith(t, pool, f.tenantID, "session row (FOR UPDATE)", func(ctx context.Context, tx pgx.Tx) error {
		var discard uuid.UUID
		return tx.QueryRow(ctx, `SELECT id FROM casino_launch_sessions WHERE id = $1 FOR UPDATE`, sessionID).Scan(&discard)
	})
	// blockerRG forces racer B (the postbet-sim) to queue on the RG
	// advisory lock before A ever tries it, so B is first in that
	// lock's wait queue - the ordering loRunABBA's own doc comment
	// requires for a deterministic interleaving.
	blockerRG := loHoldWith(t, pool, f.tenantID, "RG advisory lock", func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.EvaluateEligibility(ctx, tx, elig)
		return err
	})

	// A: bootstrap-sim. Acquires the session row via lockSession, then
	// wants the RG advisory lock next, exactly as
	// bootstrapGateDenialReason does via evaluateAndAuditEligibility.
	startBootstrapSim := func() *loRacer {
		return loStartRacer(t, pool, f.tenantID, "bootstrap-sim(session,RG)", func(ctx context.Context, tx pgx.Tx) error {
			if err := lockSession(ctx, tx, f.tenantID, tokenHash); err != nil {
				return err
			}
			_, err := rg.EvaluateEligibility(ctx, tx, elig)
			return err
		})
	}
	// B: postbet-sim. Takes the RG advisory lock first, exactly as
	// postBet's own evaluateAndAuditEligibility call does, then
	// calls the REAL BindProviderRound - whose FK to
	// casino_launch_sessions takes the IMPLICIT FOR KEY SHARE lock
	// this whole finding is about.
	startPostBetSim := func() *loRacer {
		return loStartRacer(t, pool, f.tenantID, "postbet-sim(RG,session FK)", func(ctx context.Context, tx pgx.Tx) error {
			if _, err := rg.EvaluateEligibility(ctx, tx, elig); err != nil {
				return err
			}
			return BindProviderRound(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, sessionID, game.ID,
				"mock-casino", "lockorder-f1-round-"+sessionID.String(), nil)
		})
	}

	racerA, racerB, errA, errB := loRunABBA(t, pool, startBootstrapSim, startPostBetSim, []*loBlocker{blockerSession, blockerRG})

	deadlockedA, deadlockedB := loIsDeadlock(errA), loIsDeadlock(errB)
	if wantDeadlock {
		if !deadlockedA && !deadlockedB {
			t.Fatalf("expected a 40P01 deadlock between %q and %q; got errA=%v errB=%v",
				racerA.name, racerB.name, errA, errB)
		}
		// Postgres's deadlock detector aborts exactly one side; the
		// other must then complete normally once the victim's locks
		// are released.
		if deadlockedA && errB != nil {
			t.Fatalf("the non-victim racer %q must still succeed once the deadlock victim aborts: %v", racerB.name, errB)
		}
		if deadlockedB && errA != nil {
			t.Fatalf("the non-victim racer %q must still succeed once the deadlock victim aborts: %v", racerA.name, errA)
		}
	} else {
		loAssertNoDeadlock(t, "F-1 (bootstrap session lock vs. postBet's FK-driven lock)",
			map[string]error{racerA.name: errA, racerB.name: errB})
		if errA != nil {
			t.Fatalf("%s must not error under the fix: %v", racerA.name, errA)
		}
		if errB != nil {
			t.Fatalf("%s must not error under the fix: %v", racerB.name, errB)
		}
	}
}

// loAssertWageringContribution fails unless grantID has a
// bonus_wagering_progress row whose staked amount is exactly wantStake -
// i.e. unless a bet of that stake was attributed to that Grant, which
// only happens on the path that acquires the Grant's advisory lock.
func loAssertWageringContribution(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID, wantStake int64) {
	t.Helper()
	var rows []bonus.WageringProgress
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = bonus.ListWageringProgressByGrant(ctx, tx, tenantID, grantID)
		return err
	}); err != nil {
		t.Fatalf("list wagering progress for grant %s: %v", grantID, err)
	}
	staked := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.StakedBonusAmount == nil {
			continue
		}
		staked = append(staked, r.StakedBonusAmount.String())
		if r.StakedBonusAmount.Cmp(big.NewInt(wantStake)) == 0 {
			return
		}
	}
	t.Fatalf("expected grant %s to carry a wagering contribution of %d from the racing bet (proving the bet took "+
		"that grant's advisory lock); got contributions %v", grantID, wantStake, staked)
}
