//go:build integration

// PRH-2 R5, review finding H2 (SIGNED-ACTOR-PROOF, ADR 0110, migration 0120):
// the SAME mechanism extended to the K1 capability-grant and financial
// policy-change guards. An authorized DEFENSIVE test on a private scratch
// database with synthetic data, as the REAL runtime role (asserted neither
// superuser nor BYPASSRLS): a session that sets the GUCs of two real platform
// admins can no longer grant itself a ledger_adjustment:* / payment_force_resolve:*
// capability, nor lower the required approvals, without a proof exactly bound to
// each step. Non-vacuity: every matrix has an exactly-bound control that succeeds.
package adjustment

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func utcMicro(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func k1CreateDigest(tenantID, grantee uuid.UUID, capName string, vf time.Time, vu *time.Time, reason string) string {
	return actorproof.Digest(actorproof.S(tenantID.String()), actorproof.S(grantee.String()), actorproof.S(capName),
		actorproof.TS(&vf), actorproof.TS(vu), actorproof.S(reason))
}

func rawGrantRequest(ctx context.Context, tx pgx.Tx, tenantID, grantee uuid.UUID, capName string, vf time.Time, vu *time.Time, reason string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO staff_capability_grant_requests (tenant_id, grantee_staff_id, capability, valid_from, valid_until, reason_code)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`, tenantID, grantee, capName, vf, vu, reason).Scan(&id)
	return id, err
}

func rawGrantDecision(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, decision string) error {
	_, err := tx.Exec(ctx, `INSERT INTO staff_capability_grant_approvals (request_id, decision, reason_code) VALUES ($1, $2, 'attack')`, requestID, decision)
	return err
}

func rawGrant(ctx context.Context, tx pgx.Tx, requestID, approvalID uuid.UUID) error {
	_, err := tx.Exec(ctx, `INSERT INTO staff_capability_grants (request_id, approval_id) VALUES ($1, $2)`, requestID, approvalID)
	return err
}

// platformAttack is arbitrary SQL as the runtime role impersonating a platform admin.
func platformAttack(rt *db.Pool, actor staffMember, proof string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return rt.WithPlatformAdmin(context.Background(), actor.ID, func(ctx context.Context, tx pgx.Tx) error {
		if proof != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); err != nil {
				return err
			}
		}
		return fn(ctx, tx)
	})
}

func (w *world) grantCount(grantee uuid.UUID, capName string) int {
	w.t.Helper()
	var n int
	if err := w.pool.WithPlatformAdmin(context.Background(), w.AdminA.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grants WHERE grantee_staff_id = $1 AND capability = $2`, grantee, capName).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// (H2-a) A runtime session impersonating TWO platform admins cannot give a
// platform principal a ledger_adjustment capability: the request, the co-approval
// and the grant all need proofs.
func TestActorProofK1_Adversarial_GrantSelfEscalation_FailsClosed(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	f := processForger(t)
	const capName = "payment_force_resolve:approve"
	target := w.Acting2 // a platform admin with no payment_force_resolve grant
	vf := utcMicro(time.Now())
	vu := utcMicro(time.Now().Add(time.Hour))
	reason := "esc"
	good := k1CreateDigest(w.Tenant, target.ID, capName, vf, &vu, reason)
	other := k1CreateDigest(w.Tenant, target.ID, "ledger_adjustment:approve", vf, &vu, reason)
	req := func(proof string) error {
		return platformAttack(rt, w.AdminA, proof, func(ctx context.Context, tx pgx.Tx) error {
			_, err := rawGrantRequest(ctx, tx, w.Tenant, target.ID, capName, vf, &vu, reason)
			return err
		})
	}
	// Platform scope proofs carry an EMPTY tenant field.
	pf := func(actor staffMember, op, tgt, payload string) string {
		return f.Valid(t, actor.ID, "platform", uuid.Nil, op, tgt, payload)
	}
	nowU := time.Now().Unix()
	for _, c := range []struct {
		name, proof, want string
	}{
		{"no proof", "", "AP001"},
		{"garbage", "x", "AP001"},
		{"wrong key", prooftest.Forger{KID: f.KID, Key: make([]byte, 32)}.Valid(t, w.AdminA.ID, "platform", uuid.Nil, "capability_grant:request", "new", good), "AP002"},
		{"expired", f.Token(t, w.AdminA.ID, "platform", uuid.Nil, "capability_grant:request", "new", good, nowU-120, nowU-60), "AP003"},
		{"different capability than the proof binds", pf(w.AdminA, "capability_grant:request", "new", other), "AP004"},
		{"different actor (AdminB's proof)", pf(w.AdminB, "capability_grant:request", "new", good), "AP004"},
		{"different operation (approve)", pf(w.AdminA, "capability_grant:approve", "new", good), "AP004"},
		{"tenant scope with a tenant for a platform session", f.Valid(t, w.AdminA.ID, "tenant", w.Tenant, "capability_grant:request", "new", good), "AP004"},
		{"platform scope for a K2 operation", pf(w.AdminA, "ledger_adjustment:approve", "new", good), "AP004"},
	} {
		t.Run("request/"+c.name, func(t *testing.T) {
			err := req(c.proof)
			if err == nil {
				t.Fatal("the request was ACCEPTED without a valid proof for exactly this write")
			}
			requireCode(t, err, c.want)
		})
	}
	// CONTROL: the exactly-bound proof creates the request ...
	var reqID uuid.UUID
	if err := platformAttack(rt, w.AdminA, pf(w.AdminA, "capability_grant:request", "new", good), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, err = rawGrantRequest(ctx, tx, w.Tenant, target.ID, capName, vf, &vu, reason)
		return err
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	// ... but the co-approval by the SECOND impersonated admin is refused without a proof,
	// and so is a direct grant insert.
	dig := ""
	if err := w.pool.WithPlatformAdmin(context.Background(), w.AdminB.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		dig, err = prooftest.K1RequestDigest(ctx, tx, reqID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, proof, want string }{
		{"approve/no proof", "", "AP001"},
		{"approve/forged under a wrong key", prooftest.Forger{KID: f.KID, Key: make([]byte, 32)}.Valid(t, w.AdminB.ID, "platform", uuid.Nil, "capability_grant:approve", reqID.String(), dig), "AP002"},
		{"approve/proof of the REQUESTER (same actor cannot be both)", pf(w.AdminA, "capability_grant:approve", reqID.String(), dig), "AP004"},
		{"approve/reject proof for an approve", pf(w.AdminB, "capability_grant:reject", reqID.String(), dig), "AP004"},
		{"approve/proof for another request", pf(w.AdminB, "capability_grant:approve", uuid.NewString(), dig), "AP004"},
		{"approve/proof for other content", pf(w.AdminB, "capability_grant:approve", reqID.String(), strings.Repeat("0", 64)), "AP004"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := platformAttack(rt, w.AdminB, c.proof, func(ctx context.Context, tx pgx.Tx) error { return rawGrantDecision(ctx, tx, reqID, "approve") })
			if err == nil {
				t.Fatal("the co-approval was ACCEPTED without a valid proof")
			}
			requireCode(t, err, c.want)
		})
	}
	if got := w.grantCount(target.ID, capName); got != 0 {
		t.Fatalf("a grant exists after the attack matrix: %d", got)
	}
	// The same-transaction grant insert (approval + grant) cannot be forged either.
	err := platformAttack(rt, w.AdminB, "", func(ctx context.Context, tx pgx.Tx) error {
		var aid uuid.UUID
		if e := tx.QueryRow(ctx, `INSERT INTO staff_capability_grant_approvals (request_id, decision, reason_code) VALUES ($1, 'approve', 'x') RETURNING id`, reqID).Scan(&aid); e != nil {
			return e
		}
		return rawGrant(ctx, tx, reqID, aid)
	})
	requireCode(t, err, "AP001")
	if got := w.grantCount(target.ID, capName); got != 0 {
		t.Fatalf("a grant exists after the forged approval+grant: %d", got)
	}
	// CONTROL: the real second admin with an exactly-bound proof approves and the grant is created.
	if err := platformAttack(rt, w.AdminB, pf(w.AdminB, "capability_grant:approve", reqID.String(), dig), func(ctx context.Context, tx pgx.Tx) error {
		var aid uuid.UUID
		if e := tx.QueryRow(ctx, `INSERT INTO staff_capability_grant_approvals (request_id, decision, reason_code) VALUES ($1, 'approve', 'x') RETURNING id`, reqID).Scan(&aid); e != nil {
			return e
		}
		return rawGrant(ctx, tx, reqID, aid)
	}); err != nil {
		t.Fatalf("control approval + grant: %v", err)
	}
	if got := w.grantCount(target.ID, capName); got != 1 {
		t.Fatalf("control: want exactly 1 grant, got %d", got)
	}
}

// (H2-b) Tenant-scope K1 request (tenant admin impersonation), cancel (initiator-only,
// proof-bound), expiry-only-when-expired, and revoke.
func TestActorProofK1_Adversarial_TenantRequest_Cancel_Expire_Revoke(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 1})
	f := processForger(t)
	const capName = "payment_force_resolve:request"
	grantee := w.FinanceNoGrant
	vf := utcMicro(time.Now())
	good := k1CreateDigest(w.Tenant, grantee.ID, capName, vf, nil, "r")
	tenantAttack := func(actor staffMember, proof string, fn func(ctx context.Context, tx pgx.Tx) error) error {
		return rt.WithPrincipalScope(context.Background(), w.Tenant, actor.ID, func(ctx context.Context, tx pgx.Tx) error {
			if proof != "" {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); err != nil {
					return err
				}
			}
			return fn(ctx, tx)
		})
	}
	mk := func(proof string) (uuid.UUID, error) {
		var id uuid.UUID
		err := tenantAttack(w.TenantAdmin, proof, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			id, e = rawGrantRequest(ctx, tx, w.Tenant, grantee.ID, capName, vf, nil, "r")
			return e
		})
		return id, err
	}
	if _, err := mk(""); pgCode(err) != "AP001" {
		t.Fatalf("tenant-admin impersonation without a proof: want AP001, got %v", err)
	}
	if _, err := mk(f.Valid(t, w.F1.ID, "tenant", w.Tenant, "capability_grant:request", "new", good)); pgCode(err) != "AP004" {
		t.Fatalf("proof of another actor: want AP004, got %v", err)
	}
	if _, err := mk(f.Valid(t, w.TenantAdmin.ID, "platform", uuid.Nil, "capability_grant:request", "new", good)); pgCode(err) != "AP004" {
		t.Fatalf("platform-scope proof for a tenant session: want AP004, got %v", err)
	}
	reqID, err := mk(f.Valid(t, w.TenantAdmin.ID, "tenant", w.Tenant, "capability_grant:request", "new", good))
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	dig := ""
	if err := w.pool.WithPrincipalScope(context.Background(), w.Tenant, w.TenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		dig, e = prooftest.K1RequestDigest(ctx, tx, reqID)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	cancel := func(actor staffMember, proof string) error {
		return tenantAttack(actor, proof, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE staff_capability_grant_requests SET status = 'cancelled' WHERE id = $1`, reqID)
			return e
		})
	}
	// Initiator-only: another eligible actor in the same tenant, WITH a perfect proof for itself, is refused.
	if err := cancel(w.F1, f.Valid(t, w.F1.ID, "tenant", w.Tenant, "capability_grant:cancel", reqID.String(), dig)); pgCode(err) != "CG010" {
		t.Fatalf("a non-requester cancel: want CG010, got %v", err)
	}
	if err := cancel(w.TenantAdmin, ""); pgCode(err) != "AP001" {
		t.Fatalf("an unproven cancel by the requester: want AP001, got %v", err)
	}
	// Expiry only when actually expired.
	if err := tenantAttack(w.TenantAdmin, "", func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_capability_grant_requests SET status = 'expired' WHERE id = $1`, reqID)
		return e
	}); pgCode(err) != "CG010" {
		t.Fatalf("marking a live request expired: want CG010, got %v", err)
	}
	if err := cancel(w.TenantAdmin, f.Valid(t, w.TenantAdmin.ID, "tenant", w.Tenant, "capability_grant:cancel", reqID.String(), dig)); err != nil {
		t.Fatalf("control: the requester's proven cancel: %v", err)
	}

	// Revoke: bound to the grant, the tenant and the reason; an existing real grant of F1.
	gid := w.grantIDs[grantKey(w.F1.ID, "ledger_adjustment:initiate")]
	revoke := func(actor staffMember, proof, reason string) error {
		return tenantAttack(actor, proof, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `UPDATE staff_capability_grants SET revoked_at = now(), revoke_reason_code = $2 WHERE id = $1`, gid, reason)
			return e
		})
	}
	rd := actorproof.Digest(actorproof.S(gid.String()), actorproof.S(w.Tenant.String()), actorproof.S("rev"))
	if err := revoke(w.TenantAdmin, "", "rev"); pgCode(err) != "AP001" {
		t.Fatalf("unproven revoke: want AP001, got %v", err)
	}
	if err := revoke(w.TenantAdmin, f.Valid(t, w.TenantAdmin.ID, "tenant", w.Tenant, "capability_grant:revoke", gid.String(), rd), "other-reason"); pgCode(err) != "AP004" {
		t.Fatalf("revoke with a different reason than the proof binds: want AP004, got %v", err)
	}
	if err := revoke(w.TenantAdmin, f.Valid(t, w.TenantAdmin.ID, "tenant", w.Tenant, "capability_grant:revoke", gid.String(), rd), "rev"); err != nil {
		t.Fatalf("control: proven revoke: %v", err)
	}
}

// (H2-c) Financial policy changes: a runtime session impersonating two platform
// admins cannot lower the required approvals (propose + approve both need proofs).
func TestActorProofK1_Adversarial_PolicyChange_FailsClosed(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 2})
	f := processForger(t)
	before := w.evaluate(OperationKind, nil)
	if before.Required != 2 {
		t.Fatalf("baseline required approvals: %d", before.Required)
	}
	eff := utcMicro(time.Now())
	base := 1
	digest := actorproof.Digest(actorproof.S(ChangeKindPolicy), actorproof.S(OperationKind), actorproof.S(LevelPlatform), nil, nil, nil, nil,
		actorproof.S(w.Asset), intText(&base), nil, nil, actorproof.TS(&eff), nil)
	propose := func(proof string) (uuid.UUID, error) {
		var id uuid.UUID
		err := platformAttack(rt, w.AdminA, proof, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `INSERT INTO financial_approval_policy_changes
				(change_kind, operation_kind, level, asset_code, base_required_approvals, effective_from, content_hash, requested_by, requested_by_scope, requested_by_person_id, expires_at)
				VALUES ('policy', $1, 'platform', $2, 1, $3, '-', $4, 'platform', $4, now()) RETURNING id`,
				OperationKind, w.Asset, eff, uuid.Nil).Scan(&id)
		})
		return id, err
	}
	pf := func(actor staffMember, op, tgt, payload string) string {
		return f.Valid(t, actor.ID, "platform", uuid.Nil, op, tgt, payload)
	}
	if _, err := propose(""); pgCode(err) != "AP001" {
		t.Fatalf("unproven proposal: want AP001, got %v", err)
	}
	if _, err := propose(pf(w.AdminB, "financial_policy_change:propose", "new", digest)); pgCode(err) != "AP004" {
		t.Fatalf("proof of another admin: want AP004, got %v", err)
	}
	if _, err := propose(pf(w.AdminA, "financial_policy_change:propose", "new", strings.Repeat("0", 64))); pgCode(err) != "AP004" {
		t.Fatalf("proof for different content: want AP004, got %v", err)
	}
	id, err := propose(pf(w.AdminA, "financial_policy_change:propose", "new", digest)) // control
	if err != nil {
		t.Fatalf("control proposal: %v", err)
	}
	var hash string
	if err := w.pool.WithPlatformAdmin(context.Background(), w.AdminB.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT content_hash FROM financial_approval_policy_changes WHERE id = $1`, id).Scan(&hash)
	}); err != nil {
		t.Fatal(err)
	}
	approve := func(proof, decision string) error {
		return platformAttack(rt, w.AdminB, proof, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO financial_approval_policy_change_approvals (change_id, decision, content_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
				VALUES ($1, $2, $3, $4, 'platform', $4, 0, 'x')`, id, decision, hash, uuid.Nil)
			return e
		})
	}
	for _, c := range []struct{ name, proof, want string }{
		{"no proof", "", "AP001"},
		{"wrong key", prooftest.Forger{KID: f.KID, Key: make([]byte, 32)}.Valid(t, w.AdminB.ID, "platform", uuid.Nil, "financial_policy_change:approve", id.String(), hash), "AP002"},
		{"proposer's proof", pf(w.AdminA, "financial_policy_change:approve", id.String(), hash), "AP004"},
		{"reject proof for an approve", pf(w.AdminB, "financial_policy_change:reject", id.String(), hash), "AP004"},
		{"different change", pf(w.AdminB, "financial_policy_change:approve", uuid.NewString(), hash), "AP004"},
	} {
		t.Run("approve/"+c.name, func(t *testing.T) {
			err := approve(c.proof, "approve")
			if err == nil {
				t.Fatal("the policy approval was ACCEPTED without a valid proof")
			}
			requireCode(t, err, c.want)
		})
	}
	if got := w.evaluate(OperationKind, nil).Required; got != 2 {
		t.Fatalf("required approvals changed to %d by an unproven policy flow", got)
	}
	// CONTROL: the real second admin with an exactly-bound proof approves; the policy takes effect.
	if err := approve(pf(w.AdminB, "financial_policy_change:approve", id.String(), hash), "approve"); err != nil {
		t.Fatalf("control approval: %v", err)
	}
	if got := w.evaluate(OperationKind, nil).Required; got != 1 {
		t.Fatalf("control: the genuinely two-person-approved policy must take effect, required = %d", got)
	}
}

// (H2-d) The NULL-tenant encoding is confined to scope 'platform' and the K1/policy
// operations, and the tenant / platform_acting checks are not weakened.
func TestActorProofK1_NullTenantEncodingIsConfined(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	_ = newWorld(t, worldOpts{base: 1})
	f := processForger(t)
	ctx := context.Background()
	actor, tgt, pl := uuid.New(), uuid.NewString(), strings.Repeat("0", 64)
	call := func(proof, scope string, tenantID *uuid.UUID, op string) error {
		return rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, e := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); e != nil {
				return e
			}
			_, e := tx.Exec(ctx, `SELECT actor_proof_require($1, $2, $3, $4, $5, $6)`, actor, scope, tenantID, op, tgt, pl)
			return e
		})
	}
	tn := uuid.New()
	// platform + tenant given -> refused; platform for a K2 op -> refused.
	requireCode(t, call(f.Valid(t, actor, "platform", tn, "capability_grant:approve", tgt, pl), "platform", &tn, "capability_grant:approve"), "AP004")
	requireCode(t, call(f.Valid(t, actor, "platform", uuid.Nil, "ledger_adjustment:approve", tgt, pl), "platform", nil, "ledger_adjustment:approve"), "AP004")
	// tenant scope with NO tenant -> refused (never a NULL-tenant tenant proof).
	requireCode(t, call(f.Valid(t, actor, "tenant", uuid.Nil, "capability_grant:approve", tgt, pl), "tenant", nil, "capability_grant:approve"), "AP001")
	// platform_acting with no tenant -> refused.
	requireCode(t, call(f.Valid(t, actor, "platform_acting", uuid.Nil, "ledger_adjustment:approve", tgt, pl), "platform_acting", nil, "ledger_adjustment:approve"), "AP001")
	// a tenant proof presented where the session is platform (and vice versa) -> binding mismatch.
	requireCode(t, call(f.Valid(t, actor, "tenant", tn, "capability_grant:approve", tgt, pl), "platform", nil, "capability_grant:approve"), "AP004")
	requireCode(t, call(f.Valid(t, actor, "platform", uuid.Nil, "capability_grant:approve", tgt, pl), "tenant", &tn, "capability_grant:approve"), "AP004")
	// the exactly-bound platform proof is accepted (control).
	if err := call(f.Valid(t, actor, "platform", uuid.Nil, "capability_grant:approve", tgt, pl), "platform", nil, "capability_grant:approve"); err != nil {
		t.Fatalf("control: %v", err)
	}
}

// (R5) The same person approving twice CONCURRENTLY, each with its own fresh proof,
// produces exactly one approval and one posting at most.
func TestActorProof_Positive_SamePersonApprovingTwiceConcurrently_OneApproval(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 2})
	w.svc = NewService(rt)
	causation := w.fund(10_000)
	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation
	r, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = w.decide(w.F2, r, DecisionApprove)
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, e := range errs {
		if e == nil {
			ok++
		}
	}
	if ok != 1 {
		t.Fatalf("exactly one concurrent approval by the same person must succeed, got %d (%v)", ok, errs)
	}
	if got := w.approvalCount(r.ID); got != 1 {
		t.Fatalf("want exactly one approval row, got %d", got)
	}
	if got := w.adjustmentLedgerTxCount(); got != 0 {
		t.Fatalf("required=2 with one distinct approver must not post, got %d", got)
	}
	w.assertInvariants()
}

// (L1) The proof binds the actor column the earlier guard forced: a trigger whose
// forced actor differs from the proven actor is refused. Exercised by a scratch-only
// direct call of the verifier-bound comparison through a session whose GUC actor
// differs from a proof for another actor (AP004 is the same refusal class).
func TestActorProof_ActorColumnMustEqualProvenActor(t *testing.T) {
	rt := tempRevokeRuntimePool(t)
	w := newWorld(t, worldOpts{base: 2})
	w.svc = NewService(rt)
	f := processForger(t)
	causation := w.fund(10_000)
	in := w.debit(300, ReasonCompensatingEntry)
	in.CausationTransactionID = &causation
	r, err := w.submit(w.F1, in)
	if err != nil {
		t.Fatal(err)
	}
	// A session for F2 whose proof is F3's: the decided_by column (F2, forced from
	// the session) is not the proven actor (F3).
	err = attackSession(rt, w, w.F2, f.Valid(t, w.F3.ID, "tenant", w.Tenant, "ledger_adjustment:approve", r.ID.String(), r.PayloadHash), func(ctx context.Context, tx pgx.Tx) error {
		return attackDecideSQL(ctx, tx, w, r.ID, "approve", r.PayloadHash)
	})
	requireCode(t, err, "AP004")
}

// (L2/R4) zz_actor_proof_guard is the LAST BEFORE ROW trigger on every governed table,
// for every event it covers, so every existing guard's own refusal precedes it.
func TestActorProof_ProofTriggerIsLastBeforeRowTriggerOnEveryGovernedTable(t *testing.T) {
	pool := testPool(t)
	tables := []string{
		"ledger_adjustment_requests", "ledger_adjustment_approvals", "payment_manual_resolutions", "payment_manual_resolution_approvals",
		"staff_capability_grant_requests", "staff_capability_grant_approvals", "staff_capability_grants",
		"financial_approval_policy_changes", "financial_approval_policy_change_approvals",
		// HSEC-APPROVED-HOLD-RELEASE-1 (migration 0124): nine -> eleven governed tables.
		"withdrawal_hold_resolutions", "withdrawal_hold_resolution_approvals",
	}
	for _, tbl := range tables {
		var names []string
		var zzCount int
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			// tgtype bit 1 = ROW, bit 2 = BEFORE; non-internal user triggers only.
			rows, err := tx.Query(ctx, `SELECT tgname FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal
				AND (tgtype & 1) = 1 AND (tgtype & 2) = 2 ORDER BY tgname COLLATE "C"`, tbl)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var n string
				if err := rows.Scan(&n); err != nil {
					return err
				}
				names = append(names, n)
			}
			if err := rows.Err(); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgrelid = $1::regclass AND tgname = 'zz_actor_proof_guard'`, tbl).Scan(&zzCount)
		}); err != nil {
			t.Fatal(err)
		}
		if zzCount != 1 {
			t.Fatalf("%s: want exactly one zz_actor_proof_guard trigger, found %d", tbl, zzCount)
		}
		if len(names) == 0 || names[len(names)-1] != "zz_actor_proof_guard" {
			t.Fatalf("%s: zz_actor_proof_guard must sort LAST among the BEFORE ROW triggers (fire order is by name), got %v", tbl, names)
		}
	}
}
