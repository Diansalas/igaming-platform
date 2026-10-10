//go:build integration

// R3-GAME-POSTINGS-NONACTIVE-1 (owner decision 2026-10-05, ADR 0095 section
// 40.5): sportsbook bet placement and every settlement path (settle, rollback
// of a current settlement, rollback tombstone, void of an open bet, void after
// settlement) refuse a NEW posting for a suspended or closed tenant, in the
// posting transaction, with no ledger write, no wallet change and an audit
// record. The operations under test run on the RUNTIME role pool (asserted
// NOT rolsuper AND NOT rolbypassrls); fixtures are seeded through the owner
// pool of the same private database.
package sportsbook

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// runtimePool connects as the runtime role and asserts it is neither a
// superuser nor BYPASSRLS, so RLS and the trigger guards are really in force.
func runtimePool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_RUNTIME_DATABASE_URL must be set: these tests must run as the runtime role")
	}
	rt, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect as runtime role: %v", err)
	}
	t.Cleanup(rt.Close)
	var super, bypass bool
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatalf("read runtime role flags: %v", err)
	}
	if super || bypass {
		t.Fatalf("runtime role must be NOT rolsuper and NOT rolbypassrls, got super=%v bypassrls=%v", super, bypass)
	}
	return rt
}

// setTenantStatus sets a tenant status. A CLOSED tenant with open sportsbook
// bets can no longer be produced through the guarded path (migration 0121,
// Q-GP-1); the R3 tests still need exactly that state (a tenant closed before
// the guard existed, or by a privileged path, with stranded bets), so for
// 'closed' the status-change trigger is disabled for the duration of this one
// owner transaction only (transactional DDL: no other session ever sees it
// disabled). Every other status change goes through the trigger.
func setTenantStatus(t *testing.T, owner *db.Pool, tenantID uuid.UUID, status string) {
	t.Helper()
	if status == "pending_launch" {
		if err := launchfix.ForcePending(context.Background(), t, owner, tenantID, nil); err != nil {
			t.Fatalf("force tenant pending_launch: %v", err)
		}
		return
	}
	// ADR 0112: the real launch guards run through the governed fixture; only the unrelated
	// GP020 closure gate is suspended for 'closed', inside this one owner transaction.
	var h launchfix.Hooks
	if status == "closed" {
		h.BeforeUpdate = func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `ALTER TABLE tenants DISABLE TRIGGER tenants_status_change_gate`)
			return err
		}
		// The error is RETURNED so the transaction aborts (transactional DDL:
		// the trigger then stays enabled for every session).
		h.AfterUpdate = func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `ALTER TABLE tenants ENABLE TRIGGER tenants_status_change_gate`)
			return err
		}
	}
	if err := launchfix.TrySetTenantStatusHooked(context.Background(), tenantID, status, h); err != nil {
		t.Fatalf("set tenant status %s: %v", status, err)
	}
}

// ledgerCounts returns the tenant's ledger transaction and entry counts.
func ledgerCounts(t *testing.T, owner *db.Pool, tenantID uuid.UUID) (txs, entries int) {
	t.Helper()
	err := owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&txs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&entries)
	})
	if err != nil {
		t.Fatalf("ledger counts: %v", err)
	}
	return txs, entries
}

func auditCount(t *testing.T, owner *db.Pool, tenantID uuid.UUID, action string) int {
	t.Helper()
	var n int
	err := owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenantID, action).Scan(&n)
	})
	if err != nil {
		t.Fatalf("audit count: %v", err)
	}
	return n
}

func betStatusOf(t *testing.T, owner *db.Pool, f sbFixture, betID uuid.UUID) BetStatus {
	t.Helper()
	var s BetStatus
	err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := GetBetByID(ctx, tx, betID)
		s = b.Status
		return err
	})
	if err != nil {
		t.Fatalf("bet status: %v", err)
	}
	return s
}

func settlementRows(t *testing.T, owner *db.Pool, f sbFixture, betID uuid.UUID) int {
	t.Helper()
	var n int
	err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bet_settlements WHERE bet_id = $1`, betID).Scan(&n)
	})
	if err != nil {
		t.Fatalf("settlement rows: %v", err)
	}
	return n
}

// placeBetRT is placeBet on an arbitrary pool.
func placeBetRT(t *testing.T, pool *db.Pool, f sbFixture, sel Selection, stake int64, key string) (PlaceBetResult, error) {
	t.Helper()
	var result PlaceBetResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = PlaceBet(ctx, tx, PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: stake,
			ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
			IdempotencyKey: key,
		})
		return err
	})
	return result, err
}

var nonActiveStatuses = []string{"suspended", "closed"}

// gateStatuses is the GP010 gate matrix (ADR 0112 / ledger-finance L-2): pending_launch is refused
// by the gameplay gate exactly like suspended and closed.
var gateStatuses = []string{"suspended", "closed", "pending_launch"}

func TestTenantNotActive_PlaceBetRefusedAndReplayIsRead(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for _, status := range gateStatuses {
		t.Run(status, func(t *testing.T) {
			f := seedFixture(t, owner)
			fundWallet(t, owner, f, 100_000)
			sel := seedSelection(t, owner, seedSelectionParams{})

			// A bet placed while active, then the tenant becomes non-active.
			first, err := placeBetRT(t, rt, f, sel, 1000, "placed-while-active")
			if err != nil || !first.Accepted {
				t.Fatalf("active placement: accepted=%v err=%v", first.Accepted, err)
			}
			setTenantStatus(t, owner, f.tenantID, status)
			txs0, entries0 := ledgerCounts(t, owner, f.tenantID)
			cash0 := cashBalance(t, owner, f)

			// A NEW bet is refused: a decline (200 shape), nothing written.
			res, err := placeBetRT(t, rt, f, sel, 1000, "new-after-"+status)
			if err != nil {
				t.Fatalf("a refusal is a decline, not a Go error: %v", err)
			}
			if res.Accepted || res.RejectionCategory != RejectionTenantNotActive {
				t.Fatalf("expected tenant_not_active decline, got %+v", res)
			}
			txs1, entries1 := ledgerCounts(t, owner, f.tenantID)
			if txs1 != txs0 || entries1 != entries0 || cashBalance(t, owner, f) != cash0 {
				t.Fatalf("refused bet changed the ledger: tx %d->%d entries %d->%d", txs0, txs1, entries0, entries1)
			}
			if auditCount(t, owner, f.tenantID, "sportsbook_bet.denied_tenant_not_active") != 1 {
				t.Fatal("expected one sportsbook_bet.denied_tenant_not_active audit record")
			}
			var bets int
			if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bets WHERE idempotency_key = $1`, "new-after-"+status).Scan(&bets)
			}); err != nil || bets != 0 {
				t.Fatalf("no bet row may exist for the refused key: n=%d err=%v", bets, err)
			}

			// Replay of the previously POSTED bet: a read, original outcome,
			// no new movement, evidence intact.
			replay, err := placeBetRT(t, rt, f, sel, 1000, "placed-while-active")
			if err != nil || !replay.Accepted || replay.Bet.ID != first.Bet.ID {
				t.Fatalf("replay must return the original bet: %+v err=%v", replay, err)
			}
			txs2, entries2 := ledgerCounts(t, owner, f.tenantID)
			if txs2 != txs0 || entries2 != entries0 {
				t.Fatalf("replay posted something: tx %d->%d", txs0, txs2)
			}

			if status == "closed" {
				// ADR 0112 section 3.1: closed is terminal (HD-CTF-9 stays open); the reopening
				// this test used to perform is refused by the database (LA020), and the closed
				// tenant keeps declining new bets.
				if err := launchfix.TrySetTenantStatus(context.Background(), f.tenantID, "active"); !launchfix.IsClosedTerminalRefusal(err) {
					t.Fatalf("a closed tenant must not be reopenable: the database must answer LA020, got %v", err)
				}
				again, err := placeBetRT(t, rt, f, sel, 1000, "after-failed-reopen")
				if err != nil || again.Accepted || again.RejectionCategory != RejectionTenantNotActive {
					t.Fatalf("a closed tenant must keep declining a new bet: %+v err=%v", again, err)
				}
				return
			}
			// Reactivation restores placement (an active tenant is unchanged).
			setTenantStatus(t, owner, f.tenantID, "active")
			ok, err := placeBetRT(t, rt, f, sel, 1000, "after-reactivation")
			if err != nil || !ok.Accepted {
				t.Fatalf("active tenant must place bets: %+v err=%v", ok, err)
			}
		})
	}
}

// TestTenantNotActive_SettlementPathsRefused drives every posting branch of
// SimulateSettlementEvent against a non-active tenant that is still refused.
// A void of an open bet and a void after settlement are terminal stake returns
// and stay ALLOWED (Q-GP-5, 2026-10-06): see stake_return_integration_test.go.
func TestTenantNotActive_SettlementPathsRefused(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	type step struct {
		name string
		// prepare runs while the tenant is ACTIVE.
		prepare func(t *testing.T, f sbFixture, actor, betID uuid.UUID)
		event   func(betID, actor uuid.UUID) SettlementEvent
		want    BetStatus
	}
	steps := []step{
		{"settle_open_bet", func(*testing.T, sbFixture, uuid.UUID, uuid.UUID) {},
			func(b, a uuid.UUID) SettlementEvent { return settleEvent(b, a, 1, SettlementOutcomeWon, stdPayout) }, BetStatusOpen},
		{"rollback_current_settlement", func(t *testing.T, f sbFixture, a, b uuid.UUID) {
			mustSimulate(t, owner, f.tenantID, settleEvent(b, a, 1, SettlementOutcomeWon, stdPayout))
		}, func(b, a uuid.UUID) SettlementEvent { return rollbackEvent(b, a, 1) }, BetStatusSettledWon},
	}
	for _, status := range gateStatuses {
		for _, s := range steps {
			t.Run(status+"/"+s.name, func(t *testing.T) {
				f, actor, betID := newStdBet(t, owner)
				s.prepare(t, f, actor, betID)
				setTenantStatus(t, owner, f.tenantID, status)
				txs0, entries0 := ledgerCounts(t, owner, f.tenantID)
				cash0, locked0, house0 := cashBalance(t, owner, f), lockedCashBalance(t, owner, f), houseBalance(t, owner, f)
				rows0 := settlementRows(t, owner, f, betID)
				audits0 := auditCount(t, owner, f.tenantID, settlementAuditActionRejected)

				res, err := simulateSettlement(t, rt, f.tenantID, s.event(betID, actor))
				if err != nil {
					t.Fatalf("a refusal is a recorded rejection result, not a Go error: %v", err)
				}
				if !res.Rejected() || res.RejectionCode != SettlementRejectTenantNotActive || res.Alert {
					t.Fatalf("expected SETTLEMENT_TENANT_NOT_ACTIVE (no alert), got %+v", res)
				}
				txs1, entries1 := ledgerCounts(t, owner, f.tenantID)
				if txs1 != txs0 || entries1 != entries0 {
					t.Fatalf("ledger changed: tx %d->%d entries %d->%d", txs0, txs1, entries0, entries1)
				}
				if cashBalance(t, owner, f) != cash0 || lockedCashBalance(t, owner, f) != locked0 || houseBalance(t, owner, f) != house0 {
					t.Fatal("wallet/house projection changed")
				}
				if settlementRows(t, owner, f, betID) != rows0 {
					t.Fatal("a settlement history row was written")
				}
				if got := betStatusOf(t, owner, f, betID); got != s.want {
					t.Fatalf("bet status %s, want %s (unchanged)", got, s.want)
				}
				if auditCount(t, owner, f.tenantID, settlementAuditActionRejected) != audits0+1 {
					t.Fatal("expected exactly one new sportsbook_bet.settlement_rejected audit record")
				}
			})
		}
	}
}

// TestTenantNotActive_SettlementReplayIsRead: an exact redelivery of an
// already-posted settlement still returns the original outcome for a closed
// tenant, with no new posting and the evidence intact.
func TestTenantNotActive_SettlementReplayIsRead(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, betID := newStdBet(t, owner)
	first := mustSimulate(t, owner, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	setTenantStatus(t, owner, f.tenantID, "closed")
	txs0, entries0 := ledgerCounts(t, owner, f.tenantID)

	res, err := simulateSettlement(t, rt, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if err != nil || res.Rejected() || res.Result != SettlementResultReplayed {
		t.Fatalf("replay must succeed as replayed: %+v err=%v", res, err)
	}
	if len(res.LedgerTransactionIDs) != 1 || res.LedgerTransactionIDs[0] != first.LedgerTransactionIDs[0] {
		t.Fatalf("replay must return the original ledger transaction: %+v vs %+v", res.LedgerTransactionIDs, first.LedgerTransactionIDs)
	}
	txs1, entries1 := ledgerCounts(t, owner, f.tenantID)
	if txs1 != txs0 || entries1 != entries0 {
		t.Fatalf("replay posted: tx %d->%d", txs0, txs1)
	}
	if got := betStatusOf(t, owner, f, betID); got != BetStatusSettledWon {
		t.Fatalf("existing evidence changed: %s", got)
	}
}

// TestTenantNotActive_CrossTenantIsolation: closing tenant A does not affect
// tenant B, and tenant B's actor cannot reach tenant A's bet.
func TestTenantNotActive_CrossTenantIsolation(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	fa, actorA, betA := newStdBet(t, owner)
	fb, actorB, betB := newStdBet(t, owner)
	setTenantStatus(t, owner, fa.tenantID, "closed")

	resA, err := simulateSettlement(t, rt, fa.tenantID, settleEvent(betA, actorA, 1, SettlementOutcomeWon, stdPayout))
	if err != nil || resA.RejectionCode != SettlementRejectTenantNotActive {
		t.Fatalf("tenant A must be refused: %+v err=%v", resA, err)
	}
	resB, err := simulateSettlement(t, rt, fb.tenantID, settleEvent(betB, actorB, 1, SettlementOutcomeWon, stdPayout))
	if err != nil || resB.Rejected() || resB.Result != SettlementResultApplied {
		t.Fatalf("tenant B must be unaffected: %+v err=%v", resB, err)
	}
	// Tenant B's scope against tenant A's bet id: RLS hides it (NOT_FOUND),
	// never the closed-tenant path and never a posting.
	resX, err := simulateSettlement(t, rt, fb.tenantID, settleEvent(betA, actorB, 1, SettlementOutcomeWon, stdPayout))
	if err != nil || resX.RejectionCode != SettlementRejectBetNotFound {
		t.Fatalf("cross-tenant bet id must be NOT_FOUND: %+v err=%v", resX, err)
	}
	if got := betStatusOf(t, owner, fa, betA); got != BetStatusOpen {
		t.Fatalf("tenant A bet changed: %s", got)
	}
}

// TestTenantNotActive_DatabaseBackstopRefusesGameplayTypesOnly: the 0118
// trigger refuses every gameplay ledger type for a non-active tenant (SQLSTATE
// GP010) and leaves the other transaction types alone (payments, staff
// manual adjustment).
func TestTenantNotActive_DatabaseBackstopRefusesGameplayTypesOnly(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f := seedFixture(t, owner)
	setTenantStatus(t, owner, f.tenantID, "closed")
	txs0, _ := ledgerCounts(t, owner, f.tenantID)

	for _, tt := range []ledger.TransactionType{
		ledger.TxCasinoBet, ledger.TxCasinoWin, ledger.TxCasinoRollback,
		ledger.TxSportsbookBet, ledger.TxSportsbookSettlement, ledger.TxSportsbookVoid, ledger.TxSportsbookRollback,
	} {
		// Raw insert: the trigger is the control under test, below any
		// application check. Other constraint violations would surface as a
		// different SQLSTATE, so GP010 proves the guard fired.
		err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id)
				 VALUES ($1, $2, $3, $4)`, f.tenantID, string(tt), "raw-"+string(tt), uuid.New())
			return err
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "GP010" {
			t.Fatalf("%s: expected SQLSTATE GP010, got %v", tt, err)
		}
	}
	if txs1, _ := ledgerCounts(t, owner, f.tenantID); txs1 != txs0 {
		t.Fatalf("backstop let a row through: %d -> %d", txs0, txs1)
	}

	// A staff manual adjustment (non-gameplay type) still posts on a closed
	// tenant: the gate is scoped to gameplay types only.
	reason := "closed tenant staff correction"
	err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		adj, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment, IdempotencyKey: "closed-adj-" + uuid.NewString(),
			CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adj, Direction: ledger.Debit, Amount: 5},
				{LedgerAccountID: cash, Direction: ledger.Credit, Amount: 5},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("non-gameplay posting on a closed tenant must be unaffected: %v", err)
	}
}

// TestTenantNotActive_StatusChangeVsSettlementRace proves there is no
// check-then-act window in either order, using two real connections.
func TestTenantNotActive_StatusChangeVsSettlementRace(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)

	t.Run("status_change_first_then_posting_refused", func(t *testing.T) {
		f, actor, betID := newStdBet(t, owner)
		updated, release, closerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var releaseOnceGuard sync.Once
		releaseOnce := func() { releaseOnceGuard.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce) // never leave a held transaction behind on a failed assertion
		go func() {
			// SUSPEND (not close): the bet is still open, and closing a tenant
			// with an open bet is now refused (Q-GP-1, migration 0121).
			closerDone <- launchfix.TrySetTenantStatusHolding(context.Background(), f.tenantID, "suspended", func() {
				close(updated)
				<-release // hold the status-change transaction open (uncommitted)
			})
		}()
		<-updated
		type out struct {
			res SettlementResult
			err error
		}
		posted := make(chan out, 1)
		go func() {
			r, err := simulateSettlement(t, rt, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
			posted <- out{r, err}
		}()
		select {
		case o := <-posted:
			t.Fatalf("the posting must wait for the in-flight status change, finished early: %+v err=%v", o.res, o.err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-closerDone; err != nil {
			t.Fatalf("status change: %v", err)
		}
		o := <-posted
		if o.err != nil || o.res.RejectionCode != SettlementRejectTenantNotActive {
			t.Fatalf("posting must see the committed suspension and be refused: %+v err=%v", o.res, o.err)
		}
		if got := betStatusOf(t, owner, f, betID); got != BetStatusOpen {
			t.Fatalf("bet status %s", got)
		}
	})

	t.Run("posting_first_then_status_change_waits", func(t *testing.T) {
		f, actor, betID := newStdBet(t, owner)
		inTx, release := make(chan struct{}), make(chan struct{})
		var releaseOnceGuard sync.Once
		releaseOnce := func() { releaseOnceGuard.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce) // never leave a held transaction behind on a failed assertion
		restore := SetSettlementAfterPostHookForTest(func(context.Context, pgx.Tx) error {
			close(inTx)
			<-release // the posting holds its transaction open after the check and the post
			return nil
		})
		defer restore()
		posted := make(chan error, 1)
		go func() {
			_, err := simulateSettlement(t, rt, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
			posted <- err
		}()
		<-inTx
		closed := make(chan error, 1)
		go func() {
			closed <- launchfix.TrySetTenantStatus(context.Background(), f.tenantID, "closed")
		}()
		select {
		case err := <-closed:
			t.Fatalf("the status change must wait for the in-flight posting, finished early: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-posted; err != nil {
			t.Fatalf("posting: %v", err)
		}
		if err := <-closed; err != nil {
			t.Fatalf("status change: %v", err)
		}
		// The posting (which began before the closure committed) is the
		// last movement; the next one is refused.
		if got := betStatusOf(t, owner, f, betID); got != BetStatusSettledWon {
			t.Fatalf("in-flight posting must have committed: %s", got)
		}
		res, err := simulateSettlement(t, rt, f.tenantID, rollbackEvent(betID, actor, 1))
		if err != nil || res.RejectionCode != SettlementRejectTenantNotActive {
			t.Fatalf("after the closure the next posting is refused: %+v err=%v", res, err)
		}
	})
}

// TestTenantNotActive_GateFailsClosedWhenTheRowIsNotVisible: under a
// player-scoped connection tenants_read hides the row; the shared primitive
// must refuse rather than treat a missing status as active. A tenant-scoped
// connection on an active tenant passes (positive control).
func TestTenantNotActive_GateFailsClosedWhenTheRowIsNotVisible(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f := seedFixture(t, owner)

	err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tenant.RequireActiveForGameplay(ctx, tx, f.tenantID)
	})
	if err != nil {
		t.Fatalf("active tenant, tenant scope: must pass, got %v", err)
	}
	err = rt.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		return tenant.RequireActiveForGameplay(ctx, tx, f.tenantID)
	})
	if !errors.Is(err, tenant.ErrNotActiveForGameplay) {
		t.Fatalf("a row hidden by RLS must fail closed, got %v", err)
	}
	if err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tenant.RequireActiveForGameplay(ctx, tx, uuid.Nil)
	}); !errors.Is(err, tenant.ErrNotActiveForGameplay) {
		t.Fatalf("a nil tenant id must be refused, got %v", err)
	}
}

// TestTenantNotActive_TombstonesAreAlwaysWritten (ledger-finance C1): a
// rollback of a never-seen settlement moves no money and protects against a
// late settle, so its tombstone is written even for a suspended tenant, is
// idempotent, and the late settle stays refused (also after reactivation).
func TestTenantNotActive_TombstonesAreAlwaysWritten(t *testing.T) {
	for _, status := range gateStatuses {
		t.Run(status, func(t *testing.T) { tombstonesAlwaysWritten(t, status) })
	}
}

func tombstonesAlwaysWritten(t *testing.T, status string) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, betID := newStdBet(t, owner)
	setTenantStatus(t, owner, f.tenantID, status)
	txs0, entries0 := ledgerCounts(t, owner, f.tenantID)

	res, err := simulateSettlement(t, rt, f.tenantID, rollbackEvent(betID, actor, 1))
	if err != nil || res.Rejected() || res.Result != SettlementResultTombstoned {
		t.Fatalf("tombstone must be written on a suspended tenant: %+v err=%v", res, err)
	}
	txs1, entries1 := ledgerCounts(t, owner, f.tenantID)
	if txs1 != txs0+1 || entries1 != entries0 {
		t.Fatalf("expected exactly one tombstone and no entries: tx %d->%d entries %d->%d", txs0, txs1, entries0, entries1)
	}
	again, err := simulateSettlement(t, rt, f.tenantID, rollbackEvent(betID, actor, 1))
	if err != nil || again.Result != SettlementResultReplayed {
		t.Fatalf("tombstone replay must be idempotent: %+v err=%v", again, err)
	}
	if txs2, _ := ledgerCounts(t, owner, f.tenantID); txs2 != txs1 {
		t.Fatalf("replay wrote: %d -> %d", txs1, txs2)
	}
	late, err := simulateSettlement(t, rt, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if err != nil || late.RejectionCode != SettlementRejectTombstoned {
		t.Fatalf("late settle must be refused as tombstoned: %+v err=%v", late, err)
	}
	if status == "closed" {
		// closed is terminal (ADR 0112 3.1): there is no reactivation for a closed tenant.
		return
	}
	setTenantStatus(t, owner, f.tenantID, "active")
	late, err = simulateSettlement(t, rt, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	if err != nil || late.RejectionCode != SettlementRejectTombstoned {
		t.Fatalf("after reactivation the late settle stays refused as tombstoned: %+v err=%v", late, err)
	}
}

// TestTenantNotActive_BackstopReplayWithDifferentTypeWritesNothing (security):
// a raw insert reusing another transaction's existing idempotency_key but a
// different gameplay type on a closed tenant must not add a row (the trigger's
// replay exemption lets it reach the unique index, which refuses it).
func TestTenantNotActive_BackstopReplayWithDifferentTypeWritesNothing(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, betID := newStdBet(t, owner)
	mustSimulate(t, owner, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	var key string
	if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT idempotency_key FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_settlement'`, f.tenantID).Scan(&key)
	}); err != nil {
		t.Fatal(err)
	}
	setTenantStatus(t, owner, f.tenantID, "closed")
	txs0, _ := ledgerCounts(t, owner, f.tenantID)
	err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1, 'casino_win', $2, $3)`,
			f.tenantID, key, uuid.New())
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected a unique violation (23505), got %v", err)
	}
	if txs1, _ := ledgerCounts(t, owner, f.tenantID); txs1 != txs0 {
		t.Fatalf("row count changed: %d -> %d", txs0, txs1)
	}
}

// TestTenantNotActive_LockAndPostTombstoneExemptionIsNarrow (delta review D1,
// D3): the exemption applies only when EVERY input is an entry-less
// tombstone. A tombstone mixed with a settlement, or a tombstone carrying
// entries, still hits the gate; ledger.Post refuses a tombstone with entries.
func TestTenantNotActive_LockAndPostTombstoneExemptionIsNarrow(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for _, status := range gateStatuses {
		f := seedFixture(t, owner)
		setTenantStatus(t, owner, f.tenantID, status)
		tomb := ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxTombstone}
		settle := ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxSportsbookSettlement}
		withEntries := ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxTombstone,
			Entries: []ledger.EntryInput{{LedgerAccountID: uuid.New(), Direction: ledger.Debit, Amount: 1}}}
		for name, ins := range map[string][]ledger.TransactionInput{
			"tombstone_mixed_with_settlement": {tomb, settle},
			"settlement_mixed_with_tombstone": {settle, tomb},
			"tombstone_with_entries":          {withEntries},
		} {
			err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := lockAndPost(ctx, tx, false, ins...)
				return err
			})
			if !errors.Is(err, ErrSettlementTenantNotActive) {
				t.Fatalf("%s/%s: the gate must apply, got %v", status, name, err)
			}
		}
		// ledger.Post refuses a tombstone that carries entries.
		err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			// Real, balanced entries, so only the tombstone rule can refuse it.
			cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
			if err != nil {
				return err
			}
			adj, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
			if err != nil {
				return err
			}
			in := ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxTombstone,
				IdempotencyKey: "tomb-entries-" + uuid.NewString(), CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: adj, Direction: ledger.Debit, Amount: 1},
					{LedgerAccountID: cash, Direction: ledger.Credit, Amount: 1},
				},
			}
			_, err = ledger.Post(ctx, tx, in)
			return err
		})
		if !errors.Is(err, ledger.ErrInvalidEntry) {
			t.Fatalf("%s: a tombstone with entries must be ErrInvalidEntry, got %v", status, err)
		}
	}
}
