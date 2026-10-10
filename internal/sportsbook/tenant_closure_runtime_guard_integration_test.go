//go:build integration

package sportsbook

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ADR 0112 / code review C-5: the closure tests in tenant_closure_integration_test.go realise an
// ADMITTED closure through the governed OWNER-run fixture (launchfix), so they do not exercise the
// runtime role writing the status. This test closes that gap with the runtime role: a tenant with
// no open rounds passes the GP020 closure gate and is then refused by the governed-status guard
// (LA020) and stays active.
//
// RESIDUAL (restore in slice 3): the pre-0128 closure tests asserted the `tenant.status_change`
// audit row written by ChangeStatus itself. ChangeStatus now fails closed
// (ErrGovernedStatusChangeRequired) and the owner-run fixture writes no audit row, so that
// assertion is not made here; the slice-3 launchgov executor owns the audit row and its test must
// assert it.
func TestTenantClosure_RuntimeRoleRawClosureOfEmptyTenant_RefusedByTheLaunchGuard(t *testing.T) {
	owner := testPool(t)
	rt := runtimePool(t)
	empty := seedFixture(t, owner)
	err := rt.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'closed' WHERE id = $1`, empty.tenantID)
		return err
	})
	if srPgCode(err) != "LA020" {
		t.Fatalf("a runtime-role closure with no governed transition must be LA020, got %v", err)
	}
	if got := tenantStatusOf(t, owner, empty.tenantID); got != "active" {
		t.Fatalf("status %s", got)
	}
}
