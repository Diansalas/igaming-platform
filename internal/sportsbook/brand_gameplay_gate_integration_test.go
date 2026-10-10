//go:build integration

// ADR 0112 decision LF1, SLICE 2 (migration 0129): brand status gates NEW
// wagering exactly like the tenant R3 gate (0118/0121). A sportsbook bet
// placement for a player of a brand that is pending_launch, suspended or closed
// is declined (RejectionBrandNotActive, audit sportsbook_bet.denied_brand_not_active)
// under the per-brand status lock; terminal stake returns (void of an open bet,
// void after settlement) and settlements of bets the brand already accepted are
// NOT brand-gated. The operations run on the RUNTIME role pool (asserted NOT
// rolsuper AND NOT rolbypassrls); fixtures are seeded through the owner pool of
// the same private database.
package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

var brandNonActiveStatuses = []string{"pending_launch", "suspended", "closed"}

// setBrandStatusSB moves a brand to status: pending_launch through the test-only
// ForcePending path (no governed move reaches it), everything else through the
// governed fixture (the real guards, including the 0129 exclusive brand lock).
func setBrandStatusSB(t *testing.T, owner *db.Pool, f sbFixture, status string) {
	t.Helper()
	if status == "pending_launch" {
		if err := launchfix.ForcePending(context.Background(), t, owner, f.tenantID, &f.brandID); err != nil {
			t.Fatalf("force brand pending_launch: %v", err)
		}
		return
	}
	if err := launchfix.TrySetBrandStatus(context.Background(), f.tenantID, f.brandID, status); err != nil {
		t.Fatalf("set brand status %s: %v", status, err)
	}
}

// seedSiblingBrandSB adds a second ACTIVE brand (owner-provisioned) with its own
// player and wallet inside f's tenant.
func seedSiblingBrandSB(t *testing.T, owner *db.Pool, f sbFixture) sbFixture {
	t.Helper()
	s := sbFixture{tenantID: f.tenantID, brandID: uuid.New(), playerAccountID: uuid.New()}
	personID := uuid.New()
	if err := owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatalf("seed sibling person: %v", err)
	}
	if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'Sibling Brand', 'active')`,
			s.brandID, f.tenantID, "sb-"+s.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			s.playerAccountID, f.tenantID, s.brandID, personID, s.playerAccountID.String()+"@example.com"); err != nil {
			return err
		}
		s.walletID = uuid.New()
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			s.walletID, f.tenantID, s.brandID, s.playerAccountID)
		return err
	}); err != nil {
		t.Fatalf("seed sibling brand: %v", err)
	}
	return s
}

func bgPgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// bgReconcileClean runs the ledger-vs-projection reconciliation for the tenant and
// requires zero balance / projection findings: the brand gate never touches balances
// or projections. The only other finding admitted is the stream's own
// "unlinked manual adjustment" class, produced by this package's fixture funding
// (fundWallet posts a raw manual_adjustment, not a governed one) and unrelated to the
// gate; anything else fails.
func bgReconcileClean(t *testing.T, owner *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	var run reconciliation.Run
	var mismatches []reconciliation.Mismatch
	if err := owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, mismatches, err = reconciliation.RunLedgerVsProjection(ctx, tx, tenantID, time.Now().Add(-time.Hour), time.Now().Add(time.Minute))
		return err
	}); err != nil {
		t.Fatalf("reconciliation: %v", err)
	}
	for _, m := range mismatches {
		// PRE-EXISTING, unrelated to the gate (recorded as a finding in the slice-2
		// report): a void of an OPEN bet resolves the tenant's house_gaming account
		// (settlement.go loadBetLedgerAccounts) without posting to it, and this stream
		// reports a never-posted account with no projection row as balance_mismatch
		// (zero totals, empty projected asset/type). Admitted only in exactly that shape.
		if m.MismatchKind == reconciliation.MismatchKindBalanceMismatch &&
			strings.HasSuffix(m.ExpectedValue, " debit=0 credit=0") && m.ActualValue == "asset= type= debit=0 credit=0" {
			continue
		}
		if m.MismatchKind != reconciliation.MismatchKindLedgerUnlinkedManualAdjustment {
			t.Fatalf("reconciliation finding %s on %s: expected %s actual %s", m.MismatchKind, m.ReconciliationKey, m.ExpectedValue, m.ActualValue)
		}
	}
	if len(mismatches) == 0 && run.Status != reconciliation.StatusClean {
		t.Fatalf("reconciliation run status %s with no findings", run.Status)
	}
}

// TestBrandGate_Sportsbook_Matrix: for each non-active brand status, a new bet is
// declined with the brand reason (never the tenant reason), nothing posts and one
// audit row is written; a replay of a bet accepted while active returns the
// original; a void of an open bet and a void after settlement (terminal stake
// returns) and a settlement with payout of an already-accepted bet still apply;
// a sibling brand of the same tenant is unaffected; invariants and reconciliation hold.
func TestBrandGate_Sportsbook_Matrix(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for _, status := range brandNonActiveStatuses {
		t.Run(status, func(t *testing.T) {
			f, actor, openBet := newStdBet(t, owner) // open bet A: to be voided (stake return)
			sel := seedSelection(t, owner, seedSelectionParams{})
			replayKey := "bg-replay-" + uuid.NewString()
			b, err := placeBet(t, owner, f, sel, stdStake, replayKey) // open bet B: replayed, then settled with payout
			if err != nil || !b.Accepted {
				t.Fatalf("bet B: %+v err=%v", b, err)
			}
			c, err := placeBet(t, owner, f, sel, stdStake, "bg-c-"+uuid.NewString()) // bet C: settled now, voided after
			if err != nil || !c.Accepted {
				t.Fatalf("bet C: %+v err=%v", c, err)
			}
			mustSimulate(t, owner, f.tenantID, settleEvent(c.Bet.ID, actor, 1, SettlementOutcomeWon, stdPayout))
			sib := seedSiblingBrandSB(t, owner, f)
			fundWallet(t, owner, sib, funded)

			setBrandStatusSB(t, owner, f, status)
			txs0, entries0 := ledgerCounts(t, owner, f.tenantID)
			cash0 := cashBalance(t, owner, f)

			// 1. A new wager is declined with the BRAND reason; nothing posts.
			res, err := placeBetRT(t, rt, f, sel, 100, "bg-new-"+uuid.NewString())
			if err != nil || res.Accepted || res.RejectionCategory != RejectionBrandNotActive || res.RejectionCode != "brand_not_active" {
				t.Fatalf("a new bet on a %s brand must be declined brand_not_active: %+v err=%v", status, res, err)
			}
			if txs, entries := ledgerCounts(t, owner, f.tenantID); txs != txs0 || entries != entries0 || cashBalance(t, owner, f) != cash0 {
				t.Fatalf("a declined bet changed the ledger/wallet: tx %d->%d entries %d->%d", txs0, txs, entries0, entries)
			}
			if got := auditCount(t, owner, f.tenantID, "sportsbook_bet.denied_brand_not_active"); got != 1 {
				t.Fatalf("expected one brand denial audit row, got %d", got)
			}
			if got := auditCount(t, owner, f.tenantID, "sportsbook_bet.denied_tenant_not_active"); got != 0 {
				t.Fatalf("the brand refusal must not be recorded as a tenant refusal (%d rows)", got)
			}

			// 2. A replay of a bet accepted while ACTIVE returns the original, posts nothing.
			again, err := placeBetRT(t, rt, f, sel, stdStake, replayKey)
			if err != nil || !again.Accepted || again.Bet.ID != b.Bet.ID {
				t.Fatalf("a replay of an accepted bet must return the original: %+v err=%v", again, err)
			}
			if txs, _ := ledgerCounts(t, owner, f.tenantID); txs != txs0 {
				t.Fatalf("a replay posted")
			}

			// 3. Terminal stake returns stay allowed (Q-GP-5 analogue).
			vres, err := simulateSettlement(t, rt, f.tenantID, voidEvent(openBet, actor, "market_cancelled"))
			if err != nil || vres.Rejected() || vres.Result != SettlementResultApplied || vres.BetStatus != BetStatusVoid {
				t.Fatalf("a void of an open bet must apply on a %s brand: %+v err=%v", status, vres, err)
			}
			vas, err := simulateSettlement(t, rt, f.tenantID, voidEvent(c.Bet.ID, actor, "data_error"))
			if err != nil || vas.Rejected() || vas.Result != SettlementResultApplied || len(vas.LedgerTransactionIDs) != 2 {
				t.Fatalf("a void after settlement must apply on a %s brand: %+v err=%v", status, vas, err)
			}

			// 4. Settlement (with payout) of a bet the brand already accepted is not
			//    brand-gated (the tenant is active).
			sres, err := simulateSettlement(t, rt, f.tenantID, settleEvent(b.Bet.ID, actor, 1, SettlementOutcomeWon, stdPayout))
			if err != nil || sres.Rejected() || sres.Result != SettlementResultApplied {
				t.Fatalf("settlement of an accepted bet must apply on a %s brand: %+v err=%v", status, sres, err)
			}

			// 5. A sibling brand of the same tenant is unaffected.
			sb, err := placeBetRT(t, rt, sib, sel, 100, "bg-sib-"+uuid.NewString())
			if err != nil || !sb.Accepted {
				t.Fatalf("a bet on the sibling (active) brand must be accepted: %+v err=%v", sb, err)
			}

			// 6. Still refused afterwards; invariants and reconciliation.
			res2, err := placeBetRT(t, rt, f, sel, 100, "bg-new2-"+uuid.NewString())
			if err != nil || res2.Accepted || res2.RejectionCategory != RejectionBrandNotActive {
				t.Fatalf("still refused: %+v err=%v", res2, err)
			}
			srInvariants(t, owner, f.tenantID)
			bgReconcileClean(t, owner, f.tenantID)
		})
	}
}

// TestBrandGate_Sportsbook_TenantReasonWins: a tenant AND its brand non-active
// report the tenant reason (the tenant gate runs first); never conflated.
func TestBrandGate_Sportsbook_TenantReasonWins(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f := seedFixture(t, owner)
	fundWallet(t, owner, f, funded)
	sel := seedSelection(t, owner, seedSelectionParams{})
	setBrandStatusSB(t, owner, f, "suspended")
	setTenantStatus(t, owner, f.tenantID, "suspended")
	res, err := placeBetRT(t, rt, f, sel, 100, "bg-both-"+uuid.NewString())
	if err != nil || res.Accepted || res.RejectionCategory != RejectionTenantNotActive {
		t.Fatalf("tenant and brand suspended: want the tenant reason, got %+v err=%v", res, err)
	}
	if got := auditCount(t, owner, f.tenantID, "sportsbook_bet.denied_brand_not_active"); got != 0 {
		t.Fatalf("no brand denial expected, got %d", got)
	}
}

// TestBrandGate_DatabaseBackstop: a code path that skipped the Go gate still cannot
// commit a new stake (casino_bet / sportsbook_bet) moving a player wallet of a
// non-active brand (0129 deferred trigger, SQLSTATE GP011). Other gameplay types are
// not brand-gated; a replay through ledger.Post inserts nothing and passes.
func TestBrandGate_DatabaseBackstop(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, _, _ := newStdBet(t, owner)
	replayKey := "bg-backstop-replay-" + uuid.NewString()
	post := func(pool *db.Pool, txType ledger.TransactionType, key string, debitPlayer bool) error {
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
			if err != nil {
				return err
			}
			other, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerLockedCash, "EUR")
			if err != nil {
				return err
			}
			if !debitPlayer { // a house-only pair: no player wallet moves
				if cash, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountHouseGaming, "EUR"); err != nil {
					return err
				}
				if other, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountProviderPayable, "EUR"); err != nil {
					return err
				}
			}
			_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: txType, IdempotencyKey: key, CorrelationID: uuid.NewSHA1(uuid.NameSpaceOID, []byte(key)),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: cash, Direction: ledger.Debit, Amount: 10},
					{LedgerAccountID: other, Direction: ledger.Credit, Amount: 10},
				},
			})
			return err
		})
	}
	// While the brand is active the same raw posting commits (the backstop is not
	// vacuous by refusing everything).
	if err := post(rt, ledger.TxSportsbookBet, replayKey, true); err != nil {
		t.Fatalf("an active-brand raw stake must commit: %v", err)
	}
	setBrandStatusSB(t, owner, f, "suspended")
	txs0, _ := ledgerCounts(t, owner, f.tenantID)
	for _, tt := range []ledger.TransactionType{ledger.TxSportsbookBet, ledger.TxCasinoBet} {
		if err := post(rt, tt, "bg-backstop-"+string(tt)+"-"+uuid.NewString(), true); bgPgCode(err) != "GP011" {
			t.Fatalf("%s on a suspended brand must be refused GP011 at commit, got %v", tt, err)
		}
	}
	if txs, _ := ledgerCounts(t, owner, f.tenantID); txs != txs0 {
		t.Fatalf("the backstop let a row through: %d -> %d", txs0, txs)
	}
	// The table-owner role is bound too (a trigger, not a grant).
	if err := post(owner, ledger.TxCasinoBet, "bg-backstop-owner-"+uuid.NewString(), true); bgPgCode(err) != "GP011" {
		t.Fatalf("the owner role must also be refused GP011, got %v", err)
	}
	// A replay inserts nothing and is not refused.
	if err := post(rt, ledger.TxSportsbookBet, replayKey, true); err != nil {
		t.Fatalf("a replay of a stake posted while active must pass: %v", err)
	}
	// Not brand-gated: a casino_win (an existing round's settlement) on the same wallet.
	if err := post(rt, ledger.TxCasinoWin, "bg-backstop-win-"+uuid.NewString(), true); err != nil {
		t.Fatalf("a casino_win must not be brand-gated: %v", err)
	}
	// A wager row moving no player wallet has no brand and passes.
	if err := post(rt, ledger.TxCasinoBet, "bg-backstop-house-"+uuid.NewString(), false); err != nil {
		t.Fatalf("a house-only row is not a player stake: %v", err)
	}
	loAssertBalancedSportsbook(t, owner, f.tenantID)
	srAssertProjectionMatchesRebuild(t, owner, f.tenantID)
}

// TestBrandGate_Sportsbook_RaceSuspensionFirst: a placement racing an in-flight
// governed brand suspension (status UPDATE done, not committed) WAITS for it and is
// then declined. x20 under -race. LF9's sibling for suspension.
func TestBrandGate_Sportsbook_RaceSuspensionFirst(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("iter-%02d", i), func(t *testing.T) {
			f := seedFixture(t, owner)
			fundWallet(t, owner, f, funded)
			sel := seedSelection(t, owner, seedSelectionParams{})
			held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var once sync.Once
			releaseOnce := func() { once.Do(func() { close(release) }) }
			t.Cleanup(releaseOnce)
			go func() {
				done <- launchfix.TrySetBrandStatusHolding(context.Background(), f.tenantID, f.brandID, "suspended", func() {
					close(held)
					<-release
				})
			}()
			select {
			case <-held:
			case err := <-done:
				t.Fatalf("suspension ended before the hold: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatal("suspension never reached the hold")
			}
			type out struct {
				res PlaceBetResult
				err error
			}
			placed := make(chan out, 1)
			go func() {
				r, err := placeBetRT(t, rt, f, sel, 100, "bg-race-"+uuid.NewString())
				placed <- out{r, err}
			}()
			select {
			case o := <-placed:
				t.Fatalf("the placement must wait for the in-flight brand suspension, finished early: %+v err=%v", o.res, o.err)
			case <-time.After(300 * time.Millisecond):
			}
			releaseOnce()
			if err := <-done; err != nil {
				t.Fatalf("brand suspension: %v", err)
			}
			o := <-placed
			if o.err != nil || o.res.Accepted || o.res.RejectionCategory != RejectionBrandNotActive {
				t.Fatalf("the placement must see the committed suspension and be declined: %+v err=%v", o.res, o.err)
			}
			srInvariants(t, owner, f.tenantID)
		})
	}
}

// TestBrandGate_Sportsbook_RacePlacementFirst: a placement that already passed the
// gate and posted (transaction still open) makes a governed brand suspension WAIT
// until it commits; the bet stands, the next one is declined. x20 under -race.
func TestBrandGate_Sportsbook_RacePlacementFirst(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("iter-%02d", i), func(t *testing.T) {
			f := seedFixture(t, owner)
			fundWallet(t, owner, f, funded)
			sel := seedSelection(t, owner, seedSelectionParams{})
			inTx, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseOnce := func() { once.Do(func() { close(release) }) }
			t.Cleanup(releaseOnce)
			var accepted PlaceBetResult
			posted := make(chan error, 1)
			go func() {
				posted <- rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					r, err := PlaceBet(ctx, tx, PlaceBetParams{
						TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
						SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 100,
						ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
						IdempotencyKey: "bg-first-" + uuid.NewString(),
					})
					if err != nil {
						return err
					}
					accepted = r
					close(inTx)
					<-release
					return nil
				})
			}()
			select {
			case <-inTx:
			case err := <-posted:
				t.Fatalf("placement ended before the hold: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatal("placement never reached the hold")
			}
			done := make(chan error, 1)
			go func() { done <- launchfix.TrySetBrandStatus(context.Background(), f.tenantID, f.brandID, "suspended") }()
			select {
			case err := <-done:
				t.Fatalf("the brand suspension must wait for the in-flight placement, finished early: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			releaseOnce()
			if err := <-posted; err != nil || !accepted.Accepted {
				t.Fatalf("the in-flight placement must commit: %+v err=%v", accepted, err)
			}
			if err := <-done; err != nil {
				t.Fatalf("brand suspension: %v", err)
			}
			if got := betStatusOf(t, owner, f, accepted.Bet.ID); got != BetStatusOpen {
				t.Fatalf("the committed bet must stand open, got %s", got)
			}
			next, err := placeBetRT(t, rt, f, sel, 100, "bg-after-"+uuid.NewString())
			if err != nil || next.Accepted || next.RejectionCategory != RejectionBrandNotActive {
				t.Fatalf("a bet after the suspension must be declined: %+v err=%v", next, err)
			}
			srInvariants(t, owner, f.tenantID)
		})
	}
}

// TestBrandGate_LockOrder_NoDeadlock: placements (tenant key S, brand key S, then the
// ADR 0082 L0.2..L4 classes), terminal voids (tenant key S, L1 bet row, L2, L3), governed
// brand suspend/reactivate (brand row, brand key X) and governed tenant suspend/reactivate
// (tenant key X) run concurrently for 20 rounds. No 40P01 (deadlock) and no other
// unexpected error; every outcome is accepted or a typed decline; invariants hold.
func TestBrandGate_LockOrder_NoDeadlock(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f := seedFixture(t, owner)
	fundWallet(t, owner, f, 1_000_000)
	sel := seedSelection(t, owner, seedSelectionParams{})
	actor := seedRiskManager(t, owner, f.tenantID)
	var mu sync.Mutex
	var failures []string
	fail := func(format string, args ...any) {
		mu.Lock()
		failures = append(failures, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	for round := 0; round < 20; round++ {
		// An open bet per round for the void worker, placed while everything is active.
		seed, err := placeBet(t, owner, f, sel, 100, fmt.Sprintf("bg-lo-seed-%d-%s", round, uuid.NewString()))
		if err != nil || !seed.Accepted {
			t.Fatalf("round %d seed bet: %+v err=%v", round, seed, err)
		}
		var wg sync.WaitGroup
		for p := 0; p < 3; p++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				r, err := placeBetRT(t, rt, f, sel, 100, fmt.Sprintf("bg-lo-%d-%d-%s", round, p, uuid.NewString()))
				if err != nil {
					fail("round %d placement %d: %v (sqlstate %s)", round, p, err, bgPgCode(err))
					return
				}
				if !r.Accepted && r.RejectionCategory != RejectionBrandNotActive && r.RejectionCategory != RejectionTenantNotActive {
					fail("round %d placement %d: unexpected decline %+v", round, p, r)
				}
			}(p)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := simulateSettlement(t, rt, f.tenantID, voidEvent(seed.Bet.ID, actor, "market_cancelled"))
			if err != nil || res.Rejected() {
				fail("round %d void: %+v err=%v (sqlstate %s)", round, res, err, bgPgCode(err))
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := launchfix.TrySetBrandStatus(context.Background(), f.tenantID, f.brandID, "suspended"); err != nil {
				fail("round %d brand suspend: %v (sqlstate %s)", round, err, bgPgCode(err))
				return
			}
			if err := launchfix.TrySetBrandStatus(context.Background(), f.tenantID, f.brandID, "active"); err != nil {
				fail("round %d brand reactivate: %v (sqlstate %s)", round, err, bgPgCode(err))
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := launchfix.TrySetTenantStatus(context.Background(), f.tenantID, "suspended"); err != nil {
				fail("round %d tenant suspend: %v (sqlstate %s)", round, err, bgPgCode(err))
				return
			}
			if err := launchfix.TrySetTenantStatus(context.Background(), f.tenantID, "active"); err != nil {
				fail("round %d tenant reactivate: %v (sqlstate %s)", round, err, bgPgCode(err))
			}
		}()
		wg.Wait()
		if len(failures) > 0 {
			t.Fatalf("lock-order run failed:\n%v", failures)
		}
	}
	srInvariants(t, owner, f.tenantID)
	bgReconcileClean(t, owner, f.tenantID)
}

// TestBrandGate_CrossTenantBrandIsolation: the Go gate never reads another tenant's
// brand (a foreign brand id fails closed even though that brand is active), and a
// suspended brand of tenant B does not affect tenant A's bets.
func TestBrandGate_CrossTenantBrandIsolation(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	a := seedFixture(t, owner)
	b := seedFixture(t, owner)
	fundWallet(t, owner, a, funded)
	sel := seedSelection(t, owner, seedSelectionParams{})
	setBrandStatusSB(t, owner, b, "suspended")

	if err := rt.WithTenant(context.Background(), a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tenant.RequireBrandActiveForGameplay(ctx, tx, a.tenantID, a.brandID)
	}); err != nil {
		t.Fatalf("tenant A's own active brand: %v", err)
	}
	// Tenant A's session naming tenant B's (suspended) brand, and naming B's brand
	// with B's tenant id: both fail closed (not visible / not this tenant).
	for name, ids := range map[string][2]uuid.UUID{
		"foreign brand under own tenant id": {a.tenantID, b.brandID},
		"foreign tenant and brand ids":      {b.tenantID, b.brandID},
		"own brand under foreign tenant id": {b.tenantID, a.brandID},
	} {
		err := rt.WithTenant(context.Background(), a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tenant.RequireBrandActiveForGameplay(ctx, tx, ids[0], ids[1])
		})
		if !errors.Is(err, tenant.ErrBrandNotActiveForGameplay) {
			t.Fatalf("%s: must fail closed with ErrBrandNotActiveForGameplay, got %v", name, err)
		}
	}
	res, err := placeBetRT(t, rt, a, sel, 100, "bg-iso-"+uuid.NewString())
	if err != nil || !res.Accepted {
		t.Fatalf("tenant A's bet must be unaffected by tenant B's brand: %+v err=%v", res, err)
	}
	if got := auditCount(t, owner, a.tenantID, "sportsbook_bet.denied_brand_not_active"); got != 0 {
		t.Fatalf("no denial expected for tenant A, got %d", got)
	}
}
