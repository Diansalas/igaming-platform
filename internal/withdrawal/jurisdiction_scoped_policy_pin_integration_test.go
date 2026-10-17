//go:build integration

package withdrawal

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestWithdrawalPolicies_JurisdictionScopedRowIsRejectedByCheck pins the
// fail-closed shape of the only jurisdiction-keyed payments policy column:
// every caller passes a nil jurisdiction to ResolveApprovalPolicy (no
// per-withdrawal jurisdiction exists until HDR-J-1/HDR-J-3), so a
// jurisdiction-scoped policy row would be silently never selected - an
// absent jurisdiction letting a jurisdiction-dependent policy be evaluated
// as if it did not exist. Migration 0033's CHECK
// withdrawal_policies_jurisdiction_not_yet_resolvable makes such a row
// impossible to author. When a later migration lifts that CHECK together
// with a resolver-fed jurisdiction, this test must be replaced by one
// proving unresolved + jurisdiction-scoped policy => deny.
func TestWithdrawalPolicies_JurisdictionScopedRowIsRejectedByCheck(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 0)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, jurisdiction_code, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'ZZ-SECOND', 'EUR', 100, 2, $2)`, f.tenantID, time.Now().Add(-time.Hour))
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23514" || pgErr.ConstraintName != "withdrawal_policies_jurisdiction_not_yet_resolvable" {
		t.Fatalf("expected check violation withdrawal_policies_jurisdiction_not_yet_resolvable, got %v", err)
	}
}
