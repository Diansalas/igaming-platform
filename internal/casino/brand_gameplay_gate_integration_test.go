//go:build integration

// ADR 0112 decision LF1, SLICE 2 (migration 0129): a verified NEW casino bet for a
// player of a brand that is pending_launch, suspended or closed is refused inside the
// posting transaction (ErrBrandNotActive, rejection class brand_not_active, audit
// casino_callback.rejected_brand_not_active): no ledger row, no wallet change. Wins of
// rounds already accepted, a rollback of a posted bet (terminal stake return) and an
// exact replay are NOT brand-gated. Runtime role (asserted NOT rolsuper AND NOT
// rolbypassrls); fixtures through the owner pool of the same private database.
package casino

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func setBrandStatusForGate(t *testing.T, w gateWorld, status string) {
	t.Helper()
	if status == "pending_launch" {
		if err := launchfix.ForcePending(context.Background(), t, w.owner, w.f.tenantID, &w.f.brandID); err != nil {
			t.Fatalf("force brand pending_launch: %v", err)
		}
		return
	}
	if err := launchfix.TrySetBrandStatus(context.Background(), w.f.tenantID, w.f.brandID, status); err != nil {
		t.Fatalf("set brand status %s: %v", status, err)
	}
}

// recordBrand writes the durable record exactly as recordCasinoCallbackRejection does,
// after asserting the refusal carries the BRAND class (never the tenant class).
func (w gateWorld) recordBrand(t *testing.T, err error) {
	t.Helper()
	var rejected *CallbackRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("a refusal must be a *CallbackRejectedError so the HTTP layer records it, got %T: %v", err, err)
	}
	if rejected.Rejection.Class != RejectionBrandNotActive {
		t.Fatalf("rejection class %q, want %q", rejected.Rejection.Class, RejectionBrandNotActive)
	}
	if werr := w.rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordCallbackRejection(ctx, tx, w.f.tenantID, rejected.ProviderID, rejected.Rejection, "req-brand-gate-test")
		return err
	}); werr != nil {
		t.Fatalf("record rejection: %v", werr)
	}
}

func TestBrandGate_Casino_Matrix(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	for _, status := range []string{"pending_launch", "suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			w := newGateWorld(t, owner, rt)
			// History while ACTIVE: b-1 (to be rolled back: a terminal stake return),
			// b-2 (open round, to be won), b-3 (to be replayed).
			for _, d := range []struct {
				ref, round string
				amount     int64
			}{{"b-1", "round-1", 1000}, {"b-2", "round-2", 700}, {"b-3", "round-3", 300}} {
				if _, err := w.deliver(t, CallbackEventBet, d.ref, "", d.round, d.amount); err != nil {
					t.Fatalf("active bet %s: %v", d.ref, err)
				}
			}
			setBrandStatusForGate(t, w, status)
			txs0, entries0 := ledgerCountsForGate(t, owner, w.f.tenantID)
			cash0 := cashBalance(t, owner, w.f)

			// 1. A new bet is refused with the BRAND reason; nothing posts; one audit row.
			_, err := w.deliver(t, CallbackEventBet, "b-new", "", "round-new", 500)
			if !errors.Is(err, ErrBrandNotActive) || errors.Is(err, ErrTenantNotActive) {
				t.Fatalf("a new bet on a %s brand: want ErrBrandNotActive (not the tenant error), got %v", status, err)
			}
			w.recordBrand(t, err)
			if txs, entries := ledgerCountsForGate(t, owner, w.f.tenantID); txs != txs0 || entries != entries0 || cashBalance(t, owner, w.f) != cash0 {
				t.Fatalf("a refused bet changed the ledger/wallet: tx %d->%d entries %d->%d", txs0, txs, entries0, entries)
			}
			if got := countAuditRows(t, owner, w.f.tenantID, AuditActionCallbackRejectedBrandNotActive); got != 1 {
				t.Fatalf("expected one brand refusal audit row, got %d", got)
			}
			if got := countAuditRows(t, owner, w.f.tenantID, AuditActionCallbackRejectedTenantNotActive); got != 0 {
				t.Fatalf("the brand refusal must not be recorded as a tenant refusal (%d rows)", got)
			}

			// 2. An exact replay of a bet posted while active returns the original.
			rep, err := w.deliver(t, CallbackEventBet, "b-3", "", "round-3", 300)
			if err != nil || !rep.Replayed || rep.Outcome != OutcomeSucceeded {
				t.Fatalf("a replay must return the original: %+v err=%v", rep, err)
			}
			if txs, _ := ledgerCountsForGate(t, owner, w.f.tenantID); txs != txs0 {
				t.Fatal("a replay posted")
			}

			// 3. A rollback of a posted bet (terminal stake return) applies.
			if _, err := w.deliver(t, CallbackEventRollback, "rb-1", "b-1", "round-1", 0); err != nil {
				t.Fatalf("a rollback of a posted bet must apply on a %s brand: %v", status, err)
			}
			// 4. A win for a round the brand already accepted applies (the tenant is active).
			if _, err := w.deliver(t, CallbackEventWin, "w-2", "", "round-2", 1400); err != nil {
				t.Fatalf("a win for an accepted round must apply on a %s brand: %v", status, err)
			}
			if got := countTx(t, w, "casino_rollback"); got != 1 {
				t.Fatalf("expected one rollback, got %d", got)
			}
			if got := countTx(t, w, "casino_win"); got != 1 {
				t.Fatalf("expected one win, got %d", got)
			}
			if got := countTx(t, w, "casino_bet"); got != 3 {
				t.Fatalf("expected exactly the three active-era bets, got %d", got)
			}
			assertLedgerInvariants(t, w)
		})
	}
}

// TestBrandGate_Casino_RaceSuspensionFirst: a casino bet racing an in-flight governed
// brand suspension WAITS for it and is then refused (x20 under -race).
func TestBrandGate_Casino_RaceSuspensionFirst(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("iter-%02d", i), func(t *testing.T) {
			w := newGateWorld(t, owner, rt)
			held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
			var once sync.Once
			releaseOnce := func() { once.Do(func() { close(release) }) }
			t.Cleanup(releaseOnce)
			go func() {
				done <- launchfix.TrySetBrandStatusHolding(context.Background(), w.f.tenantID, w.f.brandID, "suspended", func() {
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
				res ReceiveCallbackResult
				err error
			}
			posted := make(chan out, 1)
			go func() {
				r, err := w.deliver(t, CallbackEventBet, "race-b", "", "round-race", 100)
				posted <- out{r, err}
			}()
			select {
			case o := <-posted:
				t.Fatalf("the bet must wait for the in-flight brand suspension, finished early: %+v err=%v", o.res, o.err)
			case <-time.After(300 * time.Millisecond):
			}
			releaseOnce()
			if err := <-done; err != nil {
				t.Fatalf("brand suspension: %v", err)
			}
			if o := <-posted; !errors.Is(o.err, ErrBrandNotActive) {
				t.Fatalf("the bet must see the committed suspension and be refused: %+v err=%v", o.res, o.err)
			}
			if got := countTx(t, w, "casino_bet"); got != 0 {
				t.Fatalf("a refused bet posted (%d)", got)
			}
			assertLedgerInvariants(t, w)
		})
	}
}

// TestBrandGate_Casino_RaceBetFirst: a bet that passed the gate and posted (its
// transaction still open) makes the governed brand suspension wait; the bet stands.
func TestBrandGate_Casino_RaceBetFirst(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("iter-%02d", i), func(t *testing.T) {
			w := newGateWorld(t, owner, rt)
			inTx, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			releaseOnce := func() { once.Do(func() { close(release) }) }
			t.Cleanup(releaseOnce)
			payload := w.provider.CallbackPayload(w.f.tenantID, CallbackEventBet, "first-b", "", "round-first", "game-1", 100, "EUR", OutcomeSucceeded, "", w.f.playerAccountID, w.sessionID)
			posted := make(chan error, 1)
			go func() {
				posted <- rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := w.orch.receiveCallbackInTx(ctx, tx, w.f.tenantID, "mock-casino", payload); err != nil {
						return err
					}
					close(inTx)
					<-release
					return nil
				})
			}()
			select {
			case <-inTx:
			case err := <-posted:
				t.Fatalf("bet ended before the hold: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatal("bet never reached the hold")
			}
			done := make(chan error, 1)
			go func() {
				done <- launchfix.TrySetBrandStatus(context.Background(), w.f.tenantID, w.f.brandID, "suspended")
			}()
			select {
			case err := <-done:
				t.Fatalf("the brand suspension must wait for the in-flight bet, finished early: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
			releaseOnce()
			if err := <-posted; err != nil {
				t.Fatalf("the in-flight bet must commit: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatalf("brand suspension: %v", err)
			}
			if got := countTx(t, w, "casino_bet"); got != 1 {
				t.Fatalf("the committed bet must stand, got %d bets", got)
			}
			if _, err := w.deliver(t, CallbackEventBet, "after-b", "", "round-after", 100); !errors.Is(err, ErrBrandNotActive) {
				t.Fatalf("a bet after the suspension must be refused: %v", err)
			}
			assertLedgerInvariants(t, w)
		})
	}
}
