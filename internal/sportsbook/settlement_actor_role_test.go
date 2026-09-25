//go:build integration

// Security review P3-3: the settlement actor gate must re-check the
// STORED role, not rely solely on the JWT permission check that already
// ran in the HTTP middleware chain before SimulateSettlementEvent is ever
// called. A demoted risk manager whose access token has not yet expired
// must still be rejected here.
package sportsbook

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// seedActiveStaffWithRole creates an ACTIVE staff user of an arbitrary
// role in tenantID - unlike seedRiskManager, the role is a parameter, so
// this test can seed a staff account that is active and in the right
// tenant but simply NOT a risk manager.
func seedActiveStaffWithRole(t *testing.T, pool *db.Pool, tenantID uuid.UUID, role identity.StaffRole) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := identity.CreateStaffUser(ctx, tx, tenantID, "wrong-role-"+uuid.NewString()[:8]+"@example.test", "x", role, nil)
		id = s.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed staff with role %s: %v", role, err)
	}
	return id
}

// TestSettlementActor_ActiveWrongRole_Rejected pins P3-3: an active,
// correct-tenant staff user whose STORED role is not risk_manager is
// rejected the same as an inactive/unknown/cross-tenant actor
// (ErrSettlementActorNotActive), fail closed. This is independent of
// auth.RequirePermission, which sportsbook.SimulateSettlementEvent itself
// does not call - it is exercised only via the HTTP middleware chain in
// production, so a role check inside the service itself is the only thing
// that protects a direct/future non-HTTP caller, and it is what protects
// against a token minted before a demotion took effect.
func TestSettlementActor_ActiveWrongRole_Rejected(t *testing.T) {
	pool := testPool(t)

	roles := []identity.StaffRole{
		identity.StaffRoleTenantAdmin,
		identity.StaffRoleFinance,
		identity.StaffRoleCompliance,
		identity.StaffRoleSupport,
	}
	for _, role := range roles {
		t.Run(string(role), func(t *testing.T) {
			f, _, betID := newStdBet(t, pool)
			wrongRoleStaff := seedActiveStaffWithRole(t, pool, f.tenantID, role)
			ev := voidEvent(betID, wrongRoleStaff, "push")
			ev.TenantID = f.tenantID
			_, err := callSimulate(t, pool, f.tenantID, ev)
			if !errors.Is(err, ErrSettlementActorNotActive) {
				t.Fatalf("expected ErrSettlementActorNotActive for role %s, got %v", role, err)
			}

			// Fail closed, all the way: no posting, no history row, and the
			// bet is unaffected.
			hist := settlementHistory(t, pool, f.tenantID, betID)
			if len(hist) != 0 {
				t.Fatalf("expected no history rows written, got %d", len(hist))
			}
			if got := betStatus(t, pool, f.tenantID, betID); got != BetStatusOpen {
				t.Fatalf("expected the bet to remain open, got %q", got)
			}
		})
	}
}

// TestSettlementActor_RiskManagerRole_Accepted is the control: an active
// risk_manager in the right tenant is accepted, so the role check above
// is proven to actually gate on ROLE, not merely reject everything.
func TestSettlementActor_RiskManagerRole_Accepted(t *testing.T) {
	pool := testPool(t)
	f, actor, betID := newStdBet(t, pool)
	res := mustSimulate(t, pool, f.tenantID, voidEvent(betID, actor, "push"))
	if res.Result != SettlementResultApplied {
		t.Fatalf("expected the risk manager actor to be accepted, got %q", res.Result)
	}
}
