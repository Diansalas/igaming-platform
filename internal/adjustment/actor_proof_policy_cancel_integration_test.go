//go:build integration

package adjustment

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Policy-change CANCEL under SIGNED-ACTOR-PROOF (migration 0120, review finding C2):
// an unproven cancel by the requester's GUCs is refused (AP001); another admin's
// cancel is refused by the existing guard (MA012) even with a perfect proof for
// that other admin; the requester's proven cancel succeeds (control) and the Go
// service path (CancelPolicyChangeInTx) signs it.
func TestActorProofK1_PolicyChange_Cancel(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 2})
	f := processForger(t)
	propose := func() PolicyChange {
		c, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
			AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(1)}, w.AdminA)
		if err != nil {
			t.Fatalf("genuine proposal: %v", err)
		}
		return c
	}
	cancel := func(actor staffMember, proof string, id uuid.UUID) error {
		return platformAttack(rt, actor, proof, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE financial_approval_policy_changes SET status = 'cancelled' WHERE id = $1`, id)
			return err
		})
	}
	c := propose()
	id := c.ID
	pf := func(actor staffMember) string {
		return f.Valid(t, actor.ID, "platform", uuid.Nil, "financial_policy_change:cancel", c.ID.String(), c.ContentHash)
	}
	if err := cancel(w.AdminA, "", id); pgCode(err) != "AP001" {
		t.Fatalf("unproven cancel by the requester: want AP001, got %v", err)
	}
	if err := cancel(w.AdminB, pf(w.AdminB), id); pgCode(err) != "MA012" {
		t.Fatalf("non-requester cancel with a perfect proof for itself: want MA012, got %v", err)
	}
	if err := cancel(w.AdminA, pf(w.AdminB), id); pgCode(err) != "AP004" {
		t.Fatalf("requester session with another admin's proof: want AP004, got %v", err)
	}
	if err := cancel(w.AdminA, pf(w.AdminA), id); err != nil {
		t.Fatalf("control: the requester's proven cancel: %v", err)
	}
	// The Go service path signs the cancel for the authenticated principal.
	c2 := propose()
	if err := w.policySession(w.AdminA, func(ctx context.Context, tx pgx.Tx, call PolicyCall) error {
		_, err := CancelPolicyChangeInTx(ctx, tx, call, c2.ID)
		return err
	}); err != nil {
		t.Fatalf("service cancel: %v", err)
	}
}
