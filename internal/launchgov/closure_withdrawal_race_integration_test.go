//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// withdrawalFixture prepares a funded, KYC-approved player with a verified payout instrument and
// returns a function that runs the REAL withdrawal.RequestWithdrawal (H-SEC-11 gate, hold posting,
// insert) in the given transaction.
func (w *world) withdrawalFixture(tenantID, brandID uuid.UUID) func(ctx context.Context, tx pgx.Tx) (withdrawal.WithdrawalRequest, error) {
	w.t.Helper()
	player, wl := w.playerWithWallet(tenantID, brandID)
	w.credit(tenantID, wl, 1_000_000, ledger.AccountPSPClearing)
	var person uuid.UUID
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, player).Scan(&person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			VALUES ($1, $2, $3, $4, $5, 'approved', 'mock')`, uuid.New(), tenantID, brandID, player, person)
		return err
	}); err != nil {
		w.t.Fatalf("kyc fixture: %v", err)
	}
	instrument := pitest.Bind(w.t, w.owner, tenantID, player, "EUR")
	return func(ctx context.Context, tx pgx.Tx) (withdrawal.WithdrawalRequest, error) {
		return withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: tenantID, BrandID: brandID, PlayerAccountID: player, PersonID: person, WalletID: wl, AssetCode: "EUR",
			Amount: 5000, IdempotencyKey: uuid.NewString(), PayoutInstrumentID: instrument, Destinations: pitest.Shared(),
		})
	}
}

func (w *world) withdrawalCount(tenantID uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenantID).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// REVERSE ORDER (slice-3 LOW item): a withdrawal request is in flight and holds the status gate
// SHARED (tenant key; brand row FOR SHARE) when a closure arrives. The closure must WAIT for it,
// and once the request commits the closure must be refused LA023 (a hold-bearing withdrawal now
// exists): the closure never commits over an in-flight request. Brand and tenant subjects.
func TestRace_WithdrawalRequestHoldsSharedLock_ClosureWaits_ThenLA023(t *testing.T) {
	for _, kind := range []string{"brand", "tenant"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			tid := w.activeTenant()
			b := w.activeBrand(tid)
			request := w.withdrawalFixture(tid, b)
			sub := launchgov.Subject{TenantID: tid}
			if kind == "brand" {
				sub.BrandID = &b
			}

			started, release := make(chan struct{}), make(chan struct{})
			wdDone := make(chan error, 1)
			go func() {
				wdDone <- w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := request(ctx, tx); err != nil {
						return err
					}
					close(started)
					<-release // the request transaction stays open, holding the gate shared
					return nil
				})
			}()
			select {
			case <-started:
			case err := <-wdDone:
				t.Fatalf("the withdrawal request failed before the closure: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatal("the withdrawal request did not start")
			}

			closeDone := make(chan error, 1)
			go func() {
				if kind == "brand" {
					closeDone <- launchfix.TrySetBrandStatus(ctx, tid, b, "closed")
				} else {
					closeDone <- launchfix.TrySetTenantStatus(ctx, tid, "closed")
				}
			}()
			select {
			case err := <-closeDone:
				close(release)
				t.Fatalf("the closure must WAIT for the in-flight withdrawal request, but returned: %v", err)
			case <-time.After(800 * time.Millisecond):
			}

			close(release)
			if err := <-wdDone; err != nil {
				t.Fatalf("the withdrawal request must commit: %v", err)
			}
			select {
			case err := <-closeDone:
				if got := pgCode(err); got != "LA023" {
					t.Fatalf("after the request committed the closure must be refused LA023 by the database, got %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the closure never returned after the request committed")
			}
			if st := w.status(sub); st != "active" {
				t.Fatalf("the refused closure must leave the subject active, got %s", st)
			}
			if n := w.withdrawalCount(tid); n != 1 {
				t.Fatalf("the in-flight request committed exactly once, got %d", n)
			}
		})
	}
}

// FORWARD ORDER (the counterpart, run live in the slice-3 re-review and pinned here): the closure
// holds the status gate EXCLUSIVE when a withdrawal request arrives. The request waits, then reads
// the closed subject and is refused (nothing is created, no hold is posted).
func TestRace_ClosureHoldsExclusiveLock_WithdrawalRequestWaits_ThenRefused(t *testing.T) {
	for _, kind := range []string{"brand", "tenant"} {
		kind := kind
		t.Run(kind, func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			tid := w.activeTenant()
			b := w.activeBrand(tid)
			request := w.withdrawalFixture(tid, b)

			wdDone := make(chan error, 1)
			blockedWhileHeld := false
			hold := func() {
				go func() {
					wdDone <- w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
						_, err := request(ctx, tx)
						return err
					})
				}()
				select {
				case err := <-wdDone:
					wdDone <- err // put it back for the assertion below
				case <-time.After(800 * time.Millisecond):
					blockedWhileHeld = true
				}
			}
			var err error
			if kind == "brand" {
				err = launchfix.TrySetBrandStatusHoldingOn(ctx, t, w.owner, tid, b, "closed", hold)
			} else {
				err = launchfix.TrySetTenantStatusHoldingOn(ctx, t, w.owner, tid, "closed", hold)
			}
			if err != nil {
				t.Fatalf("the closure (no hold-bearing withdrawal existed) must commit: %v", err)
			}
			if !blockedWhileHeld {
				t.Fatal("the withdrawal request must WAIT while the closure holds the gate")
			}
			select {
			case werr := <-wdDone:
				if !errors.Is(werr, tenant.ErrNotActiveForPaymentInitiation) {
					t.Fatalf("after the closure committed the request must be refused as not active, got %v", werr)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the withdrawal request never returned")
			}
			if n := w.withdrawalCount(tid); n != 0 {
				t.Fatalf("a refused request creates nothing, got %d", n)
			}
		})
	}
}
