//go:build integration

package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
)

// QA gate: two staff accounts linked to ONE non-requester Person, both holding the
// approve grant, on a required = 2 resolution, are ONE approver. The approvals guard
// refuses the second insert (MR031, "this Person already decided"); with that guard
// bypassed (owner side, scratch database) the recount's DISTINCT ON (person) still
// counts one and the resolution does not execute. Kills M12, M16 and the joint MJ.
func TestK3_Y12_TwoAccountsOnePersonCountOnce(t *testing.T) {
	pool := depositV2ScratchPool(t) // B13-B: head schema (see TestK3_Y08)
	w := newK3WorldOn(t, pool, k3Opts{base: 2})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	person := uuid.New()
	twin1 := k3MkStaff(t, w.pool, w.f.tenantID, "finance", person)
	twin2 := k3MkStaff(t, w.pool, w.f.tenantID, "finance", person)
	w.grantTenant(twin1, capability.CapabilityPaymentForceResolveApprove)
	w.grantTenant(twin2, capability.CapabilityPaymentForceResolveApprove)

	if out, err := w.decide(twin1, r, ResolutionApprove); err != nil || out.Executed || out.Counted != 1 {
		t.Fatalf("first twin: %v %+v", err, out)
	}
	// The guard: the second account of the same Person is refused.
	if _, err := w.decide(twin2, r, ResolutionApprove); k3Code(err) != "MR031" {
		t.Fatalf("second account of the same Person: want MR031, got %v", err)
	}

	// Guard bypassed (a scratch-DB owner statement in a rolled-back transaction):
	// the recount itself counts the Person once.
	ctx := context.Background()
	err := w.pool.WithPrincipalScope(ctx, w.f.tenantID, twin2.ID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE payment_manual_resolution_approvals DISABLE TRIGGER payment_manual_resolution_approvals_guard`); err != nil {
			t.Fatalf("scratch DB: cannot disable the approvals guard: %v", err)
		}
		// B13-B: this test now runs on the head schema (see the pool line above), where 0120 also installs the
		// signed-actor-proof guard on this table; the owner bypass disables it as well (scratch database only).
		if _, err := tx.Exec(ctx, `ALTER TABLE payment_manual_resolution_approvals DISABLE TRIGGER zz_actor_proof_guard`); err != nil {
			t.Fatalf("scratch DB: cannot disable the actor-proof guard: %v", err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO payment_manual_resolution_approvals
			(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $5, 0, 'k3-twin')`, w.f.tenantID, r.ID, r.PayloadHash, twin2.ID, person); err != nil {
			t.Fatalf("bypass insert: %v", err)
		}
		var counted, required int
		if err := tx.QueryRow(ctx, `SELECT counted, required FROM payment_manual_resolution_execution_status($1)`, r.ID).Scan(&counted, &required); err != nil {
			t.Fatalf("recount: %v", err)
		}
		if counted != 1 || required != 2 {
			t.Errorf("two approvals by ONE Person were counted as %d (required %d), want 1 of 2", counted, required)
		}
		// And the final UPDATE to executing is refused by the DB recount.
		err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'executing' WHERE id = $1`, r.ID)
			return err
		})
		if k3Code(err) != "MR030" {
			t.Errorf("executing on one Person's two approvals: want MR030, got %v", err)
		}
		return errK3Rollback
	})
	if err != nil && err != errK3Rollback && !isRollbackSentinel(err) {
		t.Fatal(err)
	}
	if got := w.resolution(r.ID).State; got != ResolutionPending {
		t.Fatalf("resolution %s", got)
	}
}

func isRollbackSentinel(err error) bool { return err != nil && err.Error() == errK3Rollback.Error() }
