//go:build integration

package marketgate

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

func (w world) brandOpen(t *testing.T, pool *db.Pool, ops ...Operation) (bool, error) {
	t.Helper()
	var open bool
	err := pool.WithTenant(context.Background(), w.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		open, err = BrandOpen(ctx, tx, w.tenantID, w.brandID, ops...)
		return err
	})
	return open, err
}

func TestBrandOpen_ConfiguredOpenClosedAndPerOperation(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)

	if open, err := w.brandOpen(t, pool, OpCasinoBet); err != nil || open {
		t.Fatalf("an unconfigured brand is closed (fail closed), got open=%v err=%v", open, err)
	}
	w.openMarket(t, pool, "MT")
	for _, op := range []Operation{OpCasinoBet, OpSportsbookWager, OpRegistration, OpDeposit, OpLaunch} {
		if open, err := w.brandOpen(t, pool, op); err != nil || !open {
			t.Fatalf("%s: want open, got open=%v err=%v", op, open, err)
		}
	}

	// Only the casino-wagering rung is closed for the only country: casino bets are closed, others stay open.
	casino := "casino"
	w.operationPolicy(t, pool, "MT", "wagering", &casino, operatingmarket.StateDisabled)
	if open, _ := w.brandOpen(t, pool, OpCasinoBet); open {
		t.Fatal("casino wagering must be closed")
	}
	if open, _ := w.brandOpen(t, pool, OpDeposit); !open {
		t.Fatal("deposit must still be open")
	}
	// ALL listed operations must be permitted in ONE country.
	if open, _ := w.brandOpen(t, pool, OpDeposit, OpCasinoBet); open {
		t.Fatal("a country that closes one of the listed operations does not satisfy the set")
	}
	// A second country open for everything reopens it.
	w.openMarket(t, pool, "DE")
	if open, err := w.brandOpen(t, pool, OpDeposit, OpCasinoBet); err != nil || !open {
		t.Fatalf("second country: want open, got open=%v err=%v", open, err)
	}
}

func TestBrandOpen_DisablingTheTenantRungClosesItImmediately(t *testing.T) {
	pool := testPool(t)
	w := seedWorld(t, pool)
	w.openMarket(t, pool, "MT")
	if open, _ := w.brandOpen(t, pool, OpSportsbookWager); !open {
		t.Fatal("setup: open")
	}
	w.tenantPolicy(t, pool, "MT", operatingmarket.StateDisabled)
	if open, err := w.brandOpen(t, pool, OpSportsbookWager); err != nil || open {
		t.Fatalf("after the disable: want closed, got open=%v err=%v", open, err)
	}
}

func TestBrandOpen_TenantIsolationAndBadInput(t *testing.T) {
	pool := testPool(t)
	a, b := seedWorld(t, pool), seedWorld(t, pool)
	a.openMarket(t, pool, "MT")
	if open, _ := b.brandOpen(t, pool, OpCasinoBet); open {
		t.Fatal("tenant B must not inherit tenant A's open market")
	}
	// B's transaction asked about A's identifiers: the scope assertion errors (closed).
	err := pool.WithTenant(context.Background(), b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := BrandOpen(ctx, tx, a.tenantID, a.brandID, OpCasinoBet)
		return err
	})
	if err == nil || !errors.Is(err, operatingmarket.ErrTransactionScope) {
		t.Fatalf("want ErrTransactionScope, got %v", err)
	}
	if _, err := a.brandOpen(t, pool, Operation("withdrawal")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unknown operation: %v", err)
	}
	err = pool.WithTenant(context.Background(), a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := BrandOpen(ctx, tx, a.tenantID, uuid.Nil, OpCasinoBet)
		return err
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("nil brand: %v", err)
	}
}
