//go:build integration

package sportsbook

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// seedRiskManager creates an active risk_manager staff user in the
// fixture's tenant - the sole grantee of sportsbook_settlement:simulate
// (ADR 0088 §9.1) and the actor every settlement test drives events as.
func seedRiskManager(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := identity.CreateStaffUser(ctx, tx, tenantID, "rm-"+uuid.NewString()[:8]+"@example.test", "x", identity.StaffRoleRiskManager, nil)
		id = s.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed risk manager: %v", err)
	}
	return id
}

// simulateSettlement runs one simulated settlement event in its own
// tenant-scoped transaction, as the HTTP handler does.
func simulateSettlement(t *testing.T, pool *db.Pool, tenantID uuid.UUID, ev SettlementEvent) (SettlementResult, error) {
	t.Helper()
	ev.TenantID = tenantID
	var res SettlementResult
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = SimulateSettlementEvent(ctx, tx, ev)
		return err
	})
	return res, err
}

// mustSimulate is simulateSettlement that fails the test on a Go error or
// a rejection.
func mustSimulate(t *testing.T, pool *db.Pool, tenantID uuid.UUID, ev SettlementEvent) SettlementResult {
	t.Helper()
	res, err := simulateSettlement(t, pool, tenantID, ev)
	if err != nil {
		t.Fatalf("simulate %s: %v", ev.EventType, err)
	}
	if res.Rejected() {
		t.Fatalf("simulate %s: rejected %s", ev.EventType, res.RejectionCode)
	}
	return res
}

func settleEvent(betID, actor uuid.UUID, g int, outcome string, payout int64) SettlementEvent {
	return SettlementEvent{BetID: betID, ActorStaffID: actor, EventType: SettlementEventSettle, Generation: g,
		Outcome: outcome, ClaimPayoutAmount: payout, ClaimAssetCode: "EUR"}
}

func rollbackEvent(betID, actor uuid.UUID, g int) SettlementEvent {
	return SettlementEvent{BetID: betID, ActorStaffID: actor, EventType: SettlementEventRollback, Generation: g}
}

func voidEvent(betID, actor uuid.UUID, reason string) SettlementEvent {
	return SettlementEvent{BetID: betID, ActorStaffID: actor, EventType: SettlementEventVoid, VoidReason: reason}
}
