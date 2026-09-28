//go:build integration

package adjustment

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
)

// tenantTx runs fn in a tenant-family session for actor (WithPrincipalScope).
func (w *world) tenantTx(actor staffMember, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.pool.WithPrincipalScope(context.Background(), w.Tenant, actor.ID, fn)
}

// B-4 (ADV): the payload is immutable after submission; a stale or foreign
// payload_hash, and an evidence_ref_hash differing from the pinned one, are
// all refused.
func TestB4_PayloadBindingRefusals(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(900, ReasonExternalInstruction))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	other, err := w.submit(w.F1, w.credit(901, ReasonExternalInstruction))
	if err != nil {
		t.Fatalf("submit other: %v", err)
	}

	for name, sql := range map[string]string{
		"amount":            `UPDATE ledger_adjustment_requests SET amount = amount + 1 WHERE id = $1`,
		"direction":         `UPDATE ledger_adjustment_requests SET direction = 'debit_player' WHERE id = $1`,
		"reason":            `UPDATE ledger_adjustment_requests SET reason_code = 'goodwill_credit' WHERE id = $1`,
		"evidence_ref_hash": `UPDATE ledger_adjustment_requests SET evidence_ref_hash = repeat('cd', 32) WHERE id = $1`,
		"payload_hash":      `UPDATE ledger_adjustment_requests SET payload_hash = repeat('00', 32) WHERE id = $1`,
		"initiated_by":      `UPDATE ledger_adjustment_requests SET initiated_by = gen_random_uuid() WHERE id = $1`,
		"pinned required":   `UPDATE ledger_adjustment_requests SET required_at_submission = 1 WHERE id = $1`,
	} {
		err := w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, r.ID)
			return err
		})
		if pgCode(err) != "MA030" {
			t.Fatalf("%s change after submission: expected MA030, got %v", name, err)
		}
	}

	// A foreign payload_hash (another request's) and a stale/forged one.
	for name, hash := range map[string]string{
		"foreign": other.PayloadHash,
		"stale":   strings.Repeat("0", 64),
	} {
		_, err := w.svc.Decide(ctxFor(w.F2), w.target(), r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: hash, ReasonCode: "x"}, Meta{})
		if pgCode(err) != "MA031" {
			t.Fatalf("%s payload_hash: expected MA031, got %v", name, err)
		}
	}
	// A hash over the same payload with a DIFFERENT evidence_ref_hash (F6:
	// evidence is inside the payload hash).
	var alt string
	if err := w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT ledger_adjustment_payload_hash(r) FROM ledger_adjustment_requests r WHERE r.id = $1`, r.ID).Scan(&alt)
	}); err != nil {
		t.Fatalf("recompute hash: %v", err)
	}
	if alt != r.PayloadHash {
		t.Fatalf("payload hash is not DB-reproducible: %s vs %s", alt, r.PayloadHash)
	}
	var altEvidence string
	if err := w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT k2_sha256_hex(k2_canonical(r.tenant_id::text, r.wallet_id::text, r.player_account_id::text, r.brand_id::text,
			r.account_type, r.asset_code, r.direction, r.amount::text, r.reason_code, r.causation_transaction_id::text, repeat('cd', 32), r.note_hash))
			FROM ledger_adjustment_requests r WHERE r.id = $1`, r.ID).Scan(&altEvidence)
	}); err != nil {
		t.Fatalf("canonical recompute: %v", err)
	}
	if altEvidence == r.PayloadHash {
		t.Fatal("evidence_ref_hash is not covered by the payload hash (F6)")
	}
	_, err = w.svc.Decide(ctxFor(w.F2), w.target(), r.ID, DecisionInput{Decision: DecisionApprove, PayloadHash: altEvidence, ReasonCode: "x"}, Meta{})
	if pgCode(err) != "MA031" {
		t.Fatalf("differing evidence hash: expected MA031, got %v", err)
	}
	if got := w.request(r.ID); got.State != StatePending {
		t.Fatalf("request moved: %s", got.State)
	}
	// The genuine hash still works.
	if out, err := w.decide(w.F2, r, DecisionApprove); err != nil || !out.Executed {
		t.Fatalf("genuine approval: %v %+v", err, out)
	}
	w.assertInvariants()
}

// B-5 (AZ): self-approval, the same Person under a second principal, an
// unlinked (NULL-Person) staff member, and the beneficiary as initiator or
// approver - all refused (LF-11 distinct-Person floor, S-12).
func TestB5_IndependenceFloorRefusals(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(300, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Self-approval.
	if _, err := w.decide(w.F1, r, DecisionApprove); pgCode(err) != "MA031" {
		t.Fatalf("self-approval: expected MA031, got %v", err)
	}

	// Same Person as the initiator under a second (granted) principal.
	twin := mkStaff(t, w.pool, w.Tenant, "finance", w.F1.PersonID)
	secondAdmin := w.staff(w.Tenant, "tenant_admin")
	w.grantTenantBy(secondAdmin, twin, capability.CapabilityLedgerAdjustmentApprove)
	if _, err := w.decide(twin, r, DecisionApprove); pgCode(err) != "MA031" {
		t.Fatalf("same Person approving: expected MA031, got %v", err)
	}

	// An unlinked staff member (NULL person_id) is refused as a K2 actor.
	unlinked := staffMember{ID: uuid.New(), TenantID: w.Tenant, Role: "finance"}
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES ($1, $2, $3, 'x', 'finance', 'active')`,
			unlinked.ID, w.Tenant, "k2-unlinked-"+unlinked.ID.String()+"@x.invalid")
		return err
	}); err != nil {
		t.Fatalf("unlinked staff: %v", err)
	}
	if _, err := w.decide(unlinked, r, DecisionApprove); pgCode(err) != "MA001" && pgCode(err) != "MA002" && pgCode(err) != "MA032" {
		t.Fatalf("unlinked Person approving: expected MA002/MA032, got %v", err)
	}
	if _, err := w.submit(unlinked, w.credit(1, ReasonOperationalErrorCorrection)); pgCode(err) != "MA002" && pgCode(err) != "MA032" {
		t.Fatalf("unlinked Person initiating: expected MA002/MA032, got %v", err)
	}

	// The beneficiary (the player's own Person) as initiator or approver.
	benef := mkStaff(t, w.pool, w.Tenant, "finance", w.PlayerPerson)
	w.grantTenant(benef, capability.CapabilityLedgerAdjustmentInitiate)
	w.grantTenant(benef, capability.CapabilityLedgerAdjustmentApprove)
	if _, err := w.submit(benef, w.credit(1, ReasonOperationalErrorCorrection)); pgCode(err) != "MA032" {
		t.Fatalf("beneficiary initiating: expected MA032, got %v", err)
	}
	if _, err := w.decide(benef, r, DecisionApprove); pgCode(err) != "MA032" {
		t.Fatalf("beneficiary approving: expected MA032, got %v", err)
	}
	if got := w.request(r.ID); got.State != StatePending {
		t.Fatalf("request moved: %s", got.State)
	}
	w.assertInvariants()
}

// grantTenantBy issues a G-T grant requested by a specific tenant admin
// (needed when the default tenant admin shares a Person with the grantee).
func (w *world) grantTenantBy(requester, f staffMember, c capability.Capability) uuid.UUID {
	w.t.Helper()
	saved := w.TenantAdmin
	w.TenantAdmin = requester
	defer func() { w.TenantAdmin = saved }()
	return w.grantTenant(f, c)
}
