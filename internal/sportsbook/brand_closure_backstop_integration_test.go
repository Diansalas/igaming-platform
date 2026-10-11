//go:build integration

// ADR 0112 slice 3 review condition (security C1 / ledger-finance condition 2): the DATABASE
// refuses a BRAND closure while an open sportsbook bet of that brand exists (LA024), whatever
// path writes the status. The key-holder path (the launchfix governed sequence, which bypasses
// the Go service entirely) is used, so the refusal under test is the trigger's own. A tenant's
// open bets are refused by tenants_status_change_gate (GP020; tenant_closure tests).
package sportsbook

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func brandStatusOf(t *testing.T, owner *db.Pool, f sbFixture) string {
	t.Helper()
	var s string
	if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1`, f.brandID).Scan(&s)
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBrandClosure_OpenBetRefusedByDatabase_LA024(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	f, actor, betID := newStdBet(t, owner)
	ctx := context.Background()

	err := launchfix.TrySetBrandStatusOn(ctx, t, owner, f.tenantID, f.brandID, "closed")
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "LA024" {
		t.Fatalf("a brand closure with an open bet must be refused LA024 by the database, got %v", err)
	}
	if pe.Detail != "open_sportsbook_bets=1" {
		t.Fatalf("detail must carry the count only: %q", pe.Detail)
	}
	if got := brandStatusOf(t, owner, f); got != "active" {
		t.Fatalf("the refused closure must leave the brand active, got %s", got)
	}

	// A settled bet does not block; a reopened one does again.
	mustSimulate(t, rt, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeWon, stdPayout))
	mustSimulate(t, rt, f.tenantID, rollbackEvent(betID, actor, 1))
	err = launchfix.TrySetBrandStatusOn(ctx, t, owner, f.tenantID, f.brandID, "closed")
	if !errors.As(err, &pe) || pe.Code != "LA024" {
		t.Fatalf("a reopened bet must block the brand closure again: %v", err)
	}
	mustSimulate(t, rt, f.tenantID, settleEvent(betID, actor, 2, SettlementOutcomeWon, stdPayout))
	if err := launchfix.TrySetBrandStatusOn(ctx, t, owner, f.tenantID, f.brandID, "closed"); err != nil {
		t.Fatalf("settled: the brand closure must succeed: %v", err)
	}
	if got := brandStatusOf(t, owner, f); got != "closed" {
		t.Fatalf("brand status %s", got)
	}
}

// Another brand's open bet does not block this brand's closure: a SECOND brand of the SAME tenant
// (and a brand of another tenant) closes while the first brand has an open bet.
func TestBrandClosure_OtherBrandsBetDoesNotBlock(t *testing.T) {
	owner := testPool(t)
	ctx := context.Background()
	f, _, _ := newStdBet(t, owner)
	var second uuid.UUID
	if err := owner.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, f.tenantID, "second brand", "second-"+uuid.NewString())
		second = b.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := launchfix.TrySetBrandStatusOn(ctx, t, owner, f.tenantID, second, "closed"); err != nil {
		t.Fatalf("a sibling brand with no open bet closes while this tenant's other brand has one: %v", err)
	}
	other := seedFixture(t, owner)
	if err := launchfix.TrySetBrandStatusOn(ctx, t, owner, other.tenantID, other.brandID, "closed"); err != nil {
		t.Fatalf("a brand of another tenant closes: %v", err)
	}
	// ... and the brand that DOES have the open bet is still refused.
	err := launchfix.TrySetBrandStatusOn(ctx, t, owner, f.tenantID, f.brandID, "closed")
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "LA024" {
		t.Fatalf("the brand with the bet: %v", err)
	}
}
