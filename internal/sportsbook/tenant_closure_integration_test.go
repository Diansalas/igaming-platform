//go:build integration

// PRH-2 R5, Q-GP-1 (owner decision 2026-10-06, ADR 0095 section 40.6): a
// tenant must not transition to CLOSED while open gaming rounds exist. The
// check lives in the tenants_status_change_gate trigger (migration 0121), under
// the same per-tenant advisory lock pair the gameplay gate uses, so there is
// no check-then-act window and every writer of tenants.status is covered.
// Casino rounds have no representable open state (ADR 0095 section 40.6,
// Q-GP-6), so the open bets under test are sportsbook bets. The operations
// run on the RUNTIME role pool (asserted NOT rolsuper AND NOT rolbypassrls).
package sportsbook

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// changeStatus drives tenant.ChangeStatus first, so every refusal that precedes the launch
// guard (GP020 closure refusal and its audit, unknown tenant, validation) is the real
// ChangeStatus behaviour. Since ADR 0112 / migration 0128 ChangeStatus cannot itself write
// the governed decision a status change now needs (it fails closed with
// ErrGovernedStatusChangeRequired AFTER the closure gate has admitted the change), so the
// admitted change is then realised through the governed fixture (launchfix), which runs
// the real guards. That is the sequence the slice-3 executor will perform.
func changeStatus(rt *db.Pool, tenantID uuid.UUID, status string) error {
	err := tenant.ChangeStatus(context.Background(), rt, tenant.ChangeStatusParams{
		TenantID: tenantID, NewStatus: status, ActorID: uuid.New(), ReasonCode: "r5_closure_test",
		RequestID: "req-" + uuid.NewString(),
	})
	if !errors.Is(err, tenant.ErrGovernedStatusChangeRequired) {
		return err
	}
	ferr := launchfix.TrySetTenantStatus(context.Background(), tenantID, status)
	var pe *pgconn.PgError
	if ferr != nil && !errors.As(ferr, &pe) {
		// No governed action exists for the move (for example a reopening): the refusal ChangeStatus
		// already reported stands.
		return err
	}
	return tenant.TranslateStatusChangeError(ferr)
}

func tenantStatusOf(t *testing.T, owner *db.Pool, tenantID uuid.UUID) string {
	t.Helper()
	var s string
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&s)
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

// platformAudit returns the platform-scope audit rows (tenant_id IS NULL) for a
// target tenant and action.
func platformAudit(t *testing.T, owner *db.Pool, tenantID uuid.UUID, action string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT outcome, metadata FROM audit_log WHERE tenant_id IS NULL AND action = $1 AND target_id = $2 ORDER BY created_at`, action, tenantID.String())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var outcome string
			var md map[string]any
			if err := rows.Scan(&outcome, &md); err != nil {
				return err
			}
			md["_outcome"] = outcome
			out = append(out, md)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestTenantClosure_RefusedWithOpenBetAllowedWhenResolved(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, betID := newStdBet(t, owner)
	sel := seedSelection(t, owner, seedSelectionParams{})
	second, err := placeBet(t, owner, f, sel, stdStake, "second-"+uuid.NewString())
	if err != nil || !second.Accepted {
		t.Fatalf("second bet: %v", err)
	}

	// Closure with two open bets is refused, typed, counts only, audited.
	err = changeStatus(rt, f.tenantID, "closed")
	var blocked *tenant.CloseBlockedError
	if !errors.Is(err, tenant.ErrCloseBlockedOpenRounds) || !errors.As(err, &blocked) || blocked.SportsbookOpenBets != 2 {
		t.Fatalf("expected a CloseBlockedError with 2 open bets, got %v", err)
	}
	if got := tenantStatusOf(t, owner, f.tenantID); got != "active" {
		t.Fatalf("a refused closure changed the status: %s", got)
	}
	rows := platformAudit(t, owner, f.tenantID, "tenant.status_change_refused_open_rounds")
	if len(rows) != 1 || rows[0]["_outcome"] != "denied" || rows[0]["sportsbook_open_bets"] != float64(2) ||
		rows[0]["code"] != tenant.CodeCloseBlockedOpenRounds || rows[0]["requested_status"] != "closed" {
		t.Fatalf("expected one denied audit row with counts, got %+v", rows)
	}
	for k := range rows[0] {
		switch k {
		case "_outcome", "sportsbook_open_bets", "code", "requested_status", "reason_code":
		default:
			t.Fatalf("the refusal audit row must carry counts only, found key %q", k)
		}
	}

	// Resolve via the approved path while still ACTIVE: void one bet, settle the other.
	mustSimulate(t, rt, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	err = changeStatus(rt, f.tenantID, "closed")
	if !errors.As(err, &blocked) || blocked.SportsbookOpenBets != 1 {
		t.Fatalf("one open bet must still block: %v", err)
	}
	mustSimulate(t, rt, f.tenantID, settleEvent(second.Bet.ID, actor, 1, SettlementOutcomeLost, 0))
	if err := changeStatus(rt, f.tenantID, "closed"); err != nil {
		t.Fatalf("closure after every round is resolved must succeed: %v", err)
	}
	if got := tenantStatusOf(t, owner, f.tenantID); got != "closed" {
		t.Fatalf("status %s", got)
	}
	// ADR 0112: the success audit row is the governed executor's (slice 3), not
	// ChangeStatus's, which can no longer write a status change by itself.
	if n := len(platformAudit(t, owner, f.tenantID, "tenant.status_changed")); n != 0 {
		t.Fatalf("ChangeStatus must not write a success audit row any more, got %d", n)
	}
	// ADR 0112 section 3.1: closed is terminal (HD-CTF-9 stays open); the database refuses
	// the reopening that ChangeStatus used to allow, with SQLSTATE LA020.
	if err := changeStatus(rt, f.tenantID, "active"); !errors.Is(err, tenant.ErrGovernedStatusChangeRequired) {
		t.Fatalf("reopening a closed tenant must be refused (LA020): %v", err)
	}
	if got := tenantStatusOf(t, owner, f.tenantID); got != "closed" {
		t.Fatalf("a refused reopening changed the status: %s", got)
	}
}

func TestTenantClosure_AllowedWhenNoRoundsAndSuspendUnaffected(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	empty := seedFixture(t, owner)
	if err := changeStatus(rt, empty.tenantID, "closed"); err != nil {
		t.Fatalf("a tenant with no rounds must close: %v", err)
	}

	f, actor, betID := newStdBet(t, owner)
	// Suspension (not closure) is allowed with an open bet, and so is reactivation.
	if err := changeStatus(rt, f.tenantID, "suspended"); err != nil {
		t.Fatalf("suspend with an open bet must be allowed: %v", err)
	}
	if err := changeStatus(rt, f.tenantID, "active"); err != nil {
		t.Fatalf("reactivation with an open bet must be allowed: %v", err)
	}
	if err := changeStatus(rt, f.tenantID, "suspended"); err != nil {
		t.Fatal(err)
	}
	// Closing a SUSPENDED tenant with an open bet is refused too.
	if err := changeStatus(rt, f.tenantID, "closed"); !errors.Is(err, tenant.ErrCloseBlockedOpenRounds) {
		t.Fatalf("closing a suspended tenant with an open bet must be refused, got %v", err)
	}
	// The approved sequence: while suspended, the stake return (void) is allowed
	// (Q-GP-5); then the closure succeeds. A settlement with payout is NOT
	// possible for a suspended tenant (still refused), so a bet that must pay
	// out has to be settled BEFORE suspending (remaining owner question).
	mustSimulate(t, rt, f.tenantID, voidEvent(betID, actor, "market_cancelled"))
	if err := changeStatus(rt, f.tenantID, "closed"); err != nil {
		t.Fatalf("closure after the void must succeed: %v", err)
	}
}

func TestTenantClosure_ReopenedBetBlocksAndSettledDoesNot(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, betID := newStdBet(t, owner)
	mustSimulate(t, rt, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	// A settled bet does not block; a settlement rolled back (bet reopened) does.
	mustSimulate(t, rt, f.tenantID, rollbackEvent(betID, actor, 1))
	if err := changeStatus(rt, f.tenantID, "closed"); !errors.Is(err, tenant.ErrCloseBlockedOpenRounds) {
		t.Fatalf("a reopened bet must block the closure, got %v", err)
	}
	mustSimulate(t, rt, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))
	if err := changeStatus(rt, f.tenantID, "closed"); err != nil {
		t.Fatalf("settled: %v", err)
	}
}

func TestTenantClosure_TenantIsolation(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	fa := seedFixture(t, owner)
	fb, _, _ := newStdBet(t, owner) // tenant B has an open bet
	if err := changeStatus(rt, fa.tenantID, "closed"); err != nil {
		t.Fatalf("another tenant's open bet must not block this closure: %v", err)
	}
	// And tenant B's refusal reports only B's own count.
	var blocked *tenant.CloseBlockedError
	if err := changeStatus(rt, fb.tenantID, "closed"); !errors.As(err, &blocked) || blocked.SportsbookOpenBets != 1 {
		t.Fatalf("tenant B: %v", err)
	}
	if got := tenantStatusOf(t, owner, fb.tenantID); got != "active" {
		t.Fatalf("tenant B status %s", got)
	}
	// No refusal row was written for tenant A.
	if n := len(platformAudit(t, owner, fa.tenantID, "tenant.status_change_refused_open_rounds")); n != 0 {
		t.Fatalf("tenant A has %d refusal rows", n)
	}
}

// Any writer of tenants.status is covered: raw SQL by the runtime role (under a
// platform-admin transaction, the only scope its policy admits) is refused by
// the trigger itself, with SQLSTATE GP020; the GUCs the trigger swaps for its
// count are restored, so later statements of the same transaction still work.
func TestTenantClosure_RawSQLIsRefusedAndGUCsRestored(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, _, _ := newStdBet(t, owner)
	err := rt.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'closed' WHERE id = $1`, f.tenantID)
		return err
	})
	if srPgCode(err) != "GP020" {
		t.Fatalf("raw UPDATE to closed with an open bet must be GP020, got %v", err)
	}
	if got := tenantStatusOf(t, owner, f.tenantID); got != "active" {
		t.Fatalf("status %s", got)
	}
	if blocked := tenant.TranslateStatusChangeError(err); !errors.Is(blocked, tenant.ErrCloseBlockedOpenRounds) {
		t.Fatalf("GP020 must translate to the typed error, got %v", blocked)
	}

	// Success path: close an empty tenant, then a platform-only write in the
	// SAME transaction (needs app.tenant_id unset) must still be admitted.
	empty := seedFixture(t, owner)
	other := seedFixture(t, owner)
	// ADR 0112: the closure itself is the governed fixture's (the raw runtime UPDATE above
	// stays, to prove the GP020 refusal fires before the launch guard); right after the
	// closure UPDATE, in the SAME transaction, the scope settings must be restored and a
	// platform-only write must still be admitted.
	err = launchfix.TrySetTenantStatusHooked(context.Background(), empty.tenantID, "closed", launchfix.Hooks{
		AfterUpdate: func(ctx context.Context, tx pgx.Tx) error {
			var tenantGUC, playerGUC, adminGUC string
			if err := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.tenant_id', true), ''), COALESCE(current_setting('app.player_account_id', true), ''), COALESCE(current_setting('app.platform_admin_principal_id', true), '')`).Scan(&tenantGUC, &playerGUC, &adminGUC); err != nil {
				return err
			}
			if tenantGUC != "" || playerGUC != "" || adminGUC == "" {
				return errors.New("the trigger did not restore the transaction's scope settings")
			}
			tag, err := tx.Exec(ctx, `UPDATE tenants SET updated_at = now() WHERE id = $1`, other.tenantID)
			if err != nil {
				return err
			}
			if tag.RowsAffected() != 1 {
				return errors.New("the platform-scoped write after the closure was filtered out by RLS")
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("closure then platform write in one transaction: %v", err)
	}
}

func TestTenantClosure_ValidationAndTranslation(t *testing.T) {
	rt := runtimePool(t)
	if err := tenant.ChangeStatus(context.Background(), rt, tenant.ChangeStatusParams{TenantID: uuid.New(), NewStatus: "gone", ActorID: uuid.New(), ReasonCode: "x"}); !errors.Is(err, tenant.ErrInvalidStatusChange) {
		t.Fatalf("invalid status: %v", err)
	}
	if err := tenant.ChangeStatus(context.Background(), rt, tenant.ChangeStatusParams{TenantID: uuid.New(), NewStatus: "closed", ActorID: uuid.New()}); !errors.Is(err, tenant.ErrInvalidStatusChange) {
		t.Fatalf("missing reason: %v", err)
	}
	if err := changeStatus(rt, uuid.New(), "closed"); !errors.Is(err, tenant.ErrUnknownTenant) {
		t.Fatalf("unknown tenant: %v", err)
	}
}

// Closure vs a bet placement that is in flight: either the closure waits for the
// bet and then SEES it (refused), or the closure holds the lock first and the
// bet then reads the new status (declined). The bet never slips in after the
// check.
func TestTenantClosure_RaceWithBetPlacement(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)

	t.Run("bet_in_flight_first_closure_sees_it", func(t *testing.T) {
		f := seedFixture(t, owner)
		fundWallet(t, owner, f, 100_000)
		sel := seedSelection(t, owner, seedSelectionParams{})
		inTx, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		releaseOnce := func() { once.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce)
		placed := make(chan error, 1)
		go func() {
			placed <- rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				res, err := PlaceBet(ctx, tx, PlaceBetParams{
					TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
					SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: 1000,
					ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
					IdempotencyKey: "race-" + uuid.NewString(),
				})
				if err != nil || !res.Accepted {
					return errors.New("bet not accepted")
				}
				close(inTx)
				<-release
				return nil
			})
		}()
		select {
		case <-inTx:
		case err := <-placed:
			t.Fatalf("placement ended early: %v", err)
		}
		closed := make(chan error, 1)
		go func() { closed <- changeStatus(rt, f.tenantID, "closed") }()
		select {
		case err := <-closed:
			t.Fatalf("the closure must wait for the in-flight bet placement: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-placed; err != nil {
			t.Fatal(err)
		}
		if err := <-closed; !errors.Is(err, tenant.ErrCloseBlockedOpenRounds) {
			t.Fatalf("the closure must see the committed bet and be refused: %v", err)
		}
		if got := tenantStatusOf(t, owner, f.tenantID); got != "active" {
			t.Fatalf("status %s", got)
		}
	})

	t.Run("closure_in_flight_first_bet_declined", func(t *testing.T) {
		f := seedFixture(t, owner)
		fundWallet(t, owner, f, 100_000)
		sel := seedSelection(t, owner, seedSelectionParams{})
		updated, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var once sync.Once
		releaseOnce := func() { once.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce)
		go func() {
			done <- launchfix.TrySetTenantStatusHolding(context.Background(), f.tenantID, "closed", func() {
				close(updated)
				<-release
			})
		}()
		select {
		case <-updated:
		case err := <-done:
			t.Fatalf("closure ended early: %v", err)
		}
		type out struct {
			res PlaceBetResult
			err error
		}
		placed := make(chan out, 1)
		go func() {
			r, err := placeBetRT(t, rt, f, sel, 1000, "race-late-"+uuid.NewString())
			placed <- out{r, err}
		}()
		select {
		case o := <-placed:
			t.Fatalf("the bet must wait for the in-flight closure: %+v err=%v", o.res, o.err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-done; err != nil {
			t.Fatalf("closure: %v", err)
		}
		o := <-placed
		if o.err != nil || o.res.Accepted || o.res.RejectionCategory != RejectionTenantNotActive {
			t.Fatalf("the bet must be declined after the closure: %+v err=%v", o.res, o.err)
		}
		var open int
		if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM sportsbook_bets WHERE tenant_id = $1`, f.tenantID).Scan(&open)
		}); err != nil || open != 0 {
			t.Fatalf("no bet may exist: n=%d err=%v", open, err)
		}
	})
}

// Closure vs a settlement or void in flight: the closure waits, then sees the
// resolved bet and succeeds.
func TestTenantClosure_RaceWithResolution(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	for _, kind := range []string{"void", "settlement"} {
		t.Run(kind, func(t *testing.T) {
			f, actor, betID := newStdBet(t, owner)
			inTx, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseOnce := func() { once.Do(func() { close(release) }) }
			t.Cleanup(releaseOnce)
			restore := SetSettlementAfterPostHookForTest(func(context.Context, pgx.Tx) error {
				close(inTx)
				<-release
				return nil
			})
			defer restore()
			ev := voidEvent(betID, actor, "market_cancelled")
			if kind == "settlement" {
				ev = settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout)
			}
			resolved := make(chan error, 1)
			go func() {
				_, err := simulateSettlement(t, rt, f.tenantID, ev)
				resolved <- err
			}()
			select {
			case <-inTx:
			case err := <-resolved:
				t.Fatalf("resolution ended before the hook: %v", err)
			}
			closed := make(chan error, 1)
			go func() { closed <- changeStatus(rt, f.tenantID, "closed") }()
			select {
			case err := <-closed:
				t.Fatalf("the closure must wait for the in-flight %s: %v", kind, err)
			case <-time.After(500 * time.Millisecond):
			}
			releaseOnce()
			if err := <-resolved; err != nil {
				t.Fatal(err)
			}
			if err := <-closed; err != nil {
				t.Fatalf("the closure must succeed once the bet is resolved: %v", err)
			}
			if got := tenantStatusOf(t, owner, f.tenantID); got != "closed" {
				t.Fatalf("status %s", got)
			}
			srInvariants(t, owner, f.tenantID)
		})
	}
}
