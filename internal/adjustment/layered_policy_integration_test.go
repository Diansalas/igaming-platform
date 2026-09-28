//go:build integration

package adjustment

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Layered independence of the HD-PRH2-7 tighten-only control (MA010). It is
// enforced at three points - the proposal pre-check (TestB10), the change
// APPROVAL guard, and the policy-row INSERT trigger (the binding control) -
// so the last two mask each other in any ordinary test. On a scratch DB,
// each subtest disables exactly ONE of the two layers and proves the other
// still refuses a platform-requested loosening approved by a tenant
// principal (§3.3: a non-tightening tenant row needs platform requester AND
// platform approver).
func TestLayered_TightenOnlyEachLayerIndependently(t *testing.T) {
	ctx := context.Background()
	for _, disable := range []string{"financial_approval_policy_change_approvals_guard", "financial_approval_policies_guard"} {
		t.Run("only "+disable+" disabled", func(t *testing.T) {
			pool := scratchPoolThrough(t, "k2tight_", 113)
			w := newWorldOn(t, pool, worldOpts{base: 1})
			ta2 := w.staff(w.Tenant, "tenant_admin")
			w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelTenant,
				BaseRequiredApprovals: intPtr(2)}, w.TenantAdmin, ta2)
			c, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelTenant,
				TenantID: &w.Tenant, BaseRequiredApprovals: intPtr(1)}, w.AdminA)
			if err != nil {
				t.Fatal(err)
			}
			if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
				table := "financial_approval_policy_change_approvals"
				if disable == "financial_approval_policies_guard" {
					table = "financial_approval_policies"
				}
				_, err := tx.Exec(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER `+disable)
				return err
			}); err != nil {
				t.Fatalf("disable one layer (scratch): %v", err)
			}
			err = w.policySession(ta2, func(ctx context.Context, tx pgx.Tx, call PolicyCall) error {
				if disable == "financial_approval_policy_change_approvals_guard" {
					// The guard normally forces these; supply them honestly.
					_, err := tx.Exec(ctx, `INSERT INTO financial_approval_policy_change_approvals
						(change_id, tenant_id, decision, content_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
						VALUES ($1, $2, 'approve', $3, $4, 'tenant', $5, txid_current(), 'layered')`, c.ID, w.Tenant, c.ContentHash, ta2.ID, ta2.PersonID)
					return err
				}
				_, err := DecidePolicyChangeInTx(ctx, tx, call, c.ID, DecisionApprove, c.ContentHash, "layered")
				return err
			})
			if pgCode(err) != "MA010" {
				t.Fatalf("with %s disabled, the other layer must still refuse the tenant-approved loosening (MA010), got %v", disable, err)
			}
		})
	}
}
