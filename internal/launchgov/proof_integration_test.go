//go:build integration

package launchgov_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// ---- raw SQL as igaming_runtime (the threat model: a stolen runtime credential) ---------------

// spec describes a raw request INSERT and the digest a proof for it must bind.
type spec struct {
	tenant   uuid.UUID
	brand    *uuid.UUID
	action   string
	reason   string
	model    string
	status   string
	juris    []uuid.UUID
	operator string
	note     string
}

func (s spec) kind() string {
	if s.brand != nil {
		return "brand"
	}
	return "tenant"
}

func (s spec) licensed() bool {
	return s.action == "activate" || s.action == "reactivate" || s.action == "ratify"
}

func (s spec) digest() string {
	d := actorproof.LaunchRequest{TenantID: s.tenant, SubjectKind: s.kind(), BrandID: s.brand, Action: s.action,
		ReadinessSnapshotHash: strings.Repeat("0", 64), ReasonCode: s.reason, ConditionsNote: s.note}
	if s.licensed() {
		m, st, op := s.model, s.status, s.operator
		d.LicensingModel, d.LicensingStatus, d.ResponsibleOperatorName, d.JurisdictionIDs = &m, &st, &op, s.juris
	}
	return d.Digest()
}

func newSpec(tenant uuid.UUID, brand *uuid.UUID, action string) spec {
	s := spec{tenant: tenant, brand: brand, action: action, reason: "initial_launch"}
	if action == "suspend" {
		s.reason = "compliance_hold"
	}
	if s.licensed() {
		s.model, s.status, s.operator, s.juris = "other_manually_approved", "conditional", "Operator Ltd", []uuid.UUID{uuid.New()}
	}
	return s
}

// insert runs the raw request INSERT in the CURRENT transaction (no proof attached by it).
func (s spec) insert(ctx context.Context, tx pgx.Tx) (uuid.UUID, string, error) {
	var model, st, op *string
	juris := []uuid.UUID{}
	if s.licensed() {
		model, st, op = &s.model, &s.status, &s.operator
		juris = s.juris
	}
	var id uuid.UUID
	var hash string
	err := tx.QueryRow(ctx, `
		INSERT INTO launch_authorisation_requests
		    (tenant_id, subject_kind, brand_id, action, reason_code, readiness_snapshot, readiness_snapshot_hash,
		     licensing_model, licensing_status, responsible_operator_name, jurisdiction_ids, conditions_note)
		VALUES ($1, $2, $3, $4, $5, '{}'::jsonb, repeat('0', 64), $6, $7, $8, $9, $10)
		RETURNING id, payload_hash`,
		s.tenant, s.kind(), s.brand, s.action, s.reason, model, st, op, juris, s.note).Scan(&id, &hash)
	return id, hash, err
}

func setProof(ctx context.Context, tx pgx.Tx, tok string) error {
	_, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok)
	return err
}

func (w *world) forger() prooftest.Forger { return prooftest.ProcessForger(w.t, w.ownerURL) }

// asPlatform runs fn on the RUNTIME pool as the given platform principal.
func (w *world) asPlatform(staff uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.rt.WithPlatformAdmin(context.Background(), staff, fn)
}

func (w *world) wantCode(what string, err error, code string) {
	w.t.Helper()
	if got := pgCode(err); got != code {
		w.t.Fatalf("%s: want SQLSTATE %s, got %q (err=%v)", what, code, got, err)
	}
}

func (w *world) countRequests(sub launchgov.Subject) int {
	w.t.Helper()
	var n int
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM launch_authorisation_requests WHERE tenant_id = $1 AND brand_id IS NOT DISTINCT FROM $2`, sub.TenantID, sub.BrandID).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// The headline of the slice: GUC impersonation is now REFUSED by the database.
func TestProof_GUCImpersonation_IsRefused_AsRuntimeRole(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	jid := w.jurisdiction(tid)
	legit := w.mustRequest(w.req, w.activation(sub, jid))

	// (a) Impersonating the requester: a raw request INSERT with the GUCs and NO proof.
	other := w.pendingBrand(tid)
	err := w.asPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := newSpec(tid, &other, "activate").insert(ctx, tx)
		return err
	})
	w.wantCode("raw request without a proof", err, "AP001")

	// (b) Impersonating an approver: a raw approval with the GUCs and NO proof.
	err = w.asPlatform(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		return rawApproval(ctx, tx, legit.ID, legit.PayloadHash, "approve")
	})
	w.wantCode("raw approval without a proof", err, "AP001")

	// (c) A complete GUC-driven governed sequence (the slice-1 S-3 exposure): approve, execute, transition,
	// status UPDATE. The first write already fails; nothing downstream is reachable.
	err = w.asPlatform(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		if err := rawApproval(ctx, tx, legit.ID, legit.PayloadHash, "approve"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, legit.ID)
		return err
	})
	w.wantCode("GUC-impersonated approval + execution", err, "AP001")
	// Even with the approvals "somehow" absent the executing step alone is refused (S1).
	err = w.asPlatform(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, legit.ID)
		return err
	})
	w.wantCode("execution with no approval", err, "LA011")
	if got := w.status(sub); got != "pending_launch" {
		t.Fatalf("a refused impersonation changed the brand: %s", got)
	}
	got, apps, err := w.svc.GetRequest(ctx, w.req, tid, legit.ID)
	if err != nil || got.Status != launchgov.ReqPending || len(apps) != 0 {
		t.Fatalf("a refused impersonation must leave the request pending with no approval: %+v apps=%d err=%v", got, len(apps), err)
	}

	// (d) A tenant principal session: no proof, no request.
	ts := w.tenantStaff(tid, "tenant_admin")
	err = w.rt.WithPrincipalScope(ctx, tid, ts.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := newSpec(tid, &other, "activate").insert(ctx, tx)
		return err
	})
	w.wantCode("tenant-session raw request without a proof", err, "AP001")
}

func rawApproval(ctx context.Context, tx pgx.Tx, reqID uuid.UUID, hash, decision string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO launch_authorisation_approvals (request_id, decision, payload_hash, readiness_snapshot_hash_at_decision, reason_code)
		VALUES ($1, $2, $3, repeat('0', 64), 'reviewed_ok')`, reqID, decision, hash)
	return err
}

// The binding matrix of the request proof: wrong key, expiry, operation, target, payload, scope, a
// digest that omits a bound field, replay, and rebinding to another tenant / brand / action.
func TestProof_RequestBinding_Matrix(t *testing.T) {
	w := newWorld(t)
	f := w.forger()
	tid := w.activeTenant()
	b1, b2 := w.pendingBrand(tid), w.pendingBrand(tid)
	otherTenant := w.activeTenant()
	ob := w.pendingBrand(otherTenant)
	req := w.ops.Requester
	now := time.Now().Unix()

	// try inserts spec s as the platform requester carrying token tok (built from the spec's digest by build).
	try := func(s spec, build func(digest string) string) error {
		return w.asPlatform(req, func(ctx context.Context, tx pgx.Tx) error {
			if tok := build(s.digest()); tok != "" {
				if err := setProof(ctx, tx, tok); err != nil {
					return err
				}
			}
			_, _, err := s.insert(ctx, tx)
			return err
		})
	}
	valid := func(op string) func(string) string {
		return func(d string) string { return f.Valid(t, req, "platform", uuid.Nil, op, "new", d) }
	}

	s1 := newSpec(tid, &b1, "activate")
	w.wantCode("no proof", try(s1, func(string) string { return "" }), "AP001")
	w.wantCode("malformed proof", try(s1, func(string) string { return "v1|garbage" }), "AP001")
	wrongKey := prooftest.Forger{KID: f.KID, Key: prooftest.NewKey(t)}
	w.wantCode("wrong key (bad MAC)", try(s1, func(d string) string {
		return wrongKey.Valid(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", d)
	}), "AP002")
	w.wantCode("unknown kid", try(s1, func(d string) string {
		return prooftest.Forger{KID: "nokid", Key: prooftest.NewKey(t)}.Valid(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", d)
	}), "AP002")
	w.wantCode("expired", try(s1, func(d string) string {
		return f.Token(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", d, now-120, now-60)
	}), "AP003")
	w.wantCode("issued in the future", try(s1, func(d string) string {
		return f.Token(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", d, now+300, now+330)
	}), "AP003")
	w.wantCode("lifetime too long", try(s1, func(d string) string {
		return f.Token(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", d, now, now+3600)
	}), "AP003")
	w.wantCode("wrong operation (approve)", try(s1, valid(actorproof.OpLaunchApprove)), "AP004")
	w.wantCode("wrong operation (:suspend on a non-suspend request)", try(s1, valid(actorproof.OpLaunchSuspend)), "AP004")
	w.wantCode("wrong operation family", try(s1, valid("ledger_adjustment:initiate")), "AP004")
	w.wantCode("wrong target", try(s1, func(d string) string {
		return f.Valid(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, uuid.NewString(), d)
	}), "AP004")
	w.wantCode("wrong payload", try(s1, func(string) string {
		return f.Valid(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", strings.Repeat("ab", 32))
	}), "AP004")
	w.wantCode("wrong actor (another platform admin's proof)", try(s1, func(d string) string {
		return f.Valid(t, w.ops.Approver1, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", d)
	}), "AP004")
	w.wantCode("platform session with a tenant-scope proof", try(s1, func(d string) string {
		return f.Valid(t, req, "tenant", tid, actorproof.OpLaunchRequest, "new", d)
	}), "AP004")
	w.wantCode("platform_acting scope", try(s1, func(d string) string {
		return f.Valid(t, req, "platform_acting", tid, actorproof.OpLaunchRequest, "new", d)
	}), "AP004")
	w.wantCode("platform scope with a tenant field", try(s1, func(d string) string {
		return f.Valid(t, req, "platform", tid, actorproof.OpLaunchRequest, "new", d)
	}), "AP004")

	// ':request' on a suspend request, and a digest that omits a bound field (brand_id / action).
	active := w.activeBrand(tid)
	ss := newSpec(tid, &active, "suspend")
	w.wantCode("':request' on a suspend request", try(ss, valid(actorproof.OpLaunchRequest)), "AP004")
	omit := func(mut func(*actorproof.LaunchRequest)) func(string) string {
		return func(string) string {
			d := actorproof.LaunchRequest{TenantID: s1.tenant, SubjectKind: "brand", BrandID: s1.brand, Action: s1.action,
				ReadinessSnapshotHash: strings.Repeat("0", 64), ReasonCode: s1.reason}
			m, st, op := s1.model, s1.status, s1.operator
			d.LicensingModel, d.LicensingStatus, d.ResponsibleOperatorName, d.JurisdictionIDs = &m, &st, &op, s1.juris
			mut(&d)
			return f.Valid(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", d.Digest())
		}
	}
	w.wantCode("digest that omits brand_id", try(s1, omit(func(d *actorproof.LaunchRequest) { d.BrandID = nil })), "AP004")
	w.wantCode("digest that omits the action", try(s1, omit(func(d *actorproof.LaunchRequest) { d.Action = "" })), "AP004")
	w.wantCode("digest for another reason_code", try(s1, omit(func(d *actorproof.LaunchRequest) { d.ReasonCode = "other" })), "AP004")
	w.wantCode("digest for another readiness hash", try(s1, omit(func(d *actorproof.LaunchRequest) { d.ReadinessSnapshotHash = strings.Repeat("1", 64) })), "AP004")

	// Rebinding: a proof for (tenant A, brand 1, activate) cannot carry another brand, tenant or action.
	signed := func(s spec) func(string) string { // always signs s's digest, whatever is inserted
		return func(string) string {
			return f.Valid(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", s.digest())
		}
	}
	w.wantCode("proof for brand 1 used for brand 2", try(func() spec { x := s1; x.brand = &b2; return x }(), signed(s1)), "AP004")
	w.wantCode("proof for tenant A used for tenant B's brand", try(func() spec { x := s1; x.tenant, x.brand = otherTenant, &ob; return x }(), signed(s1)), "AP004")
	w.wantCode("proof for activate used for close", try(func() spec {
		x := newSpec(tid, &b1, "close")
		x.reason = "offboarding"
		return x
	}(), signed(s1)), "AP004")
	w.wantCode("proof for one reason_code used for another", try(func() spec { x := s1; x.reason = "initial_launch2"; return x }(), signed(s1)), "AP004")
	// A TENANT subject carries no brand, so the tenant id is the only thing that names it: a proof
	// for tenant A's closure cannot be spent on tenant B's.
	closeA := newSpec(tid, nil, "close")
	closeA.reason = "offboarding"
	closeB := closeA
	closeB.tenant = otherTenant
	w.wantCode("proof for tenant A's closure used for tenant B's", try(closeB, signed(closeA)), "AP004")

	// Nothing above created a request.
	if n := w.countRequests(launchgov.Subject{TenantID: tid, BrandID: &b1}) + w.countRequests(launchgov.Subject{TenantID: tid, BrandID: &b2}); n != 0 {
		t.Fatalf("a refused proof must create no request, found %d", n)
	}

	// A valid proof is accepted exactly once; replay (same nonce) is AP005 even for a fresh transaction.
	good := f.Valid(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", s1.digest())
	if err := try(s1, func(string) string { return good }); err != nil {
		t.Fatalf("a valid proof must be accepted: %v", err)
	}
	w.wantCode("replay of a consumed proof", try(s1, func(string) string { return good }), "AP005")
	// ... and the nonce of a ROLLED-BACK attempt is not burnt.
	s2 := newSpec(tid, &b2, "activate")
	good2 := f.Valid(t, req, "platform", uuid.Nil, actorproof.OpLaunchRequest, "new", s2.digest())
	rollback := w.asPlatform(req, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, good2); err != nil {
			return err
		}
		if _, _, err := s2.insert(ctx, tx); err != nil {
			return err
		}
		return context.Canceled // roll back after a successful verification
	})
	if rollback != context.Canceled {
		t.Fatalf("unexpected: %v", rollback)
	}
	if err := try(s2, func(string) string { return good2 }); err != nil {
		t.Fatalf("a proof consumed in a rolled-back transaction must still be usable: %v", err)
	}
}

// Approval and cancel proofs: bound to the request id, the decision and the actor.
func TestProof_ApprovalAndCancelBinding(t *testing.T) {
	w := newWorld(t)
	f := w.forger()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	b1, b2 := w.pendingBrand(tid), w.pendingBrand(tid)
	r1 := w.mustRequest(w.req, w.activation(launchgov.Subject{TenantID: tid, BrandID: &b1}, jid))
	r2 := w.mustRequest(w.req, w.activation(launchgov.Subject{TenantID: tid, BrandID: &b2}, jid))
	ap1 := w.ops.Approver1

	approve := func(who uuid.UUID, reqID uuid.UUID, hash, decision, tok string) error {
		return w.asPlatform(who, func(ctx context.Context, tx pgx.Tx) error {
			if tok != "" {
				if err := setProof(ctx, tx, tok); err != nil {
					return err
				}
			}
			return rawApproval(ctx, tx, reqID, hash, decision)
		})
	}
	// An approval / rejection proof binds the digest of (payload_hash, the readiness hash the decider
	// saw, the reason code) (security L1); rawApproval inserts the zero hash and 'reviewed_ok'.
	ad := func(h string) string {
		return actorproof.LaunchApprovalDigest(h, strings.Repeat("0", 64), "reviewed_ok")
	}
	tok := func(who uuid.UUID, op string, target uuid.UUID, hash string) string {
		if op == actorproof.OpLaunchApprove || op == actorproof.OpLaunchReject {
			hash = ad(hash)
		}
		return f.Valid(t, who, "platform", uuid.Nil, op, target.String(), hash)
	}
	w.wantCode("approval proof signed for another readiness hash (L1)", approve(ap1, r1.ID, r1.PayloadHash, "approve",
		f.Valid(t, ap1, "platform", uuid.Nil, actorproof.OpLaunchApprove, r1.ID.String(),
			actorproof.LaunchApprovalDigest(r1.PayloadHash, strings.Repeat("1", 64), "reviewed_ok"))), "AP004")
	w.wantCode("approval proof signed for another reason code (L1)", approve(ap1, r1.ID, r1.PayloadHash, "approve",
		f.Valid(t, ap1, "platform", uuid.Nil, actorproof.OpLaunchApprove, r1.ID.String(),
			actorproof.LaunchApprovalDigest(r1.PayloadHash, strings.Repeat("0", 64), "policy_concern"))), "AP004")
	w.wantCode("approval proof that binds the bare payload hash only (the old shape)", approve(ap1, r1.ID, r1.PayloadHash, "approve",
		f.Valid(t, ap1, "platform", uuid.Nil, actorproof.OpLaunchApprove, r1.ID.String(), r1.PayloadHash)), "AP004")
	w.wantCode("approval proof for request 1 used on request 2", approve(ap1, r2.ID, r2.PayloadHash, "approve", tok(ap1, actorproof.OpLaunchApprove, r1.ID, r2.PayloadHash)), "AP004")
	w.wantCode("approval proof with another payload", approve(ap1, r1.ID, r1.PayloadHash, "approve", tok(ap1, actorproof.OpLaunchApprove, r1.ID, r2.PayloadHash)), "AP004")
	w.wantCode("approve proof used for a reject", approve(ap1, r1.ID, r1.PayloadHash, "reject", tok(ap1, actorproof.OpLaunchApprove, r1.ID, r1.PayloadHash)), "AP004")
	w.wantCode("reject proof used for an approve", approve(ap1, r1.ID, r1.PayloadHash, "approve", tok(ap1, actorproof.OpLaunchReject, r1.ID, r1.PayloadHash)), "AP004")
	w.wantCode("another approver's proof", approve(ap1, r1.ID, r1.PayloadHash, "approve", tok(w.ops.Approver2, actorproof.OpLaunchApprove, r1.ID, r1.PayloadHash)), "AP004")
	w.wantCode("tenant-scope approve proof", approve(ap1, r1.ID, r1.PayloadHash, "approve",
		f.Valid(t, ap1, "tenant", tid, actorproof.OpLaunchApprove, r1.ID.String(), ad(r1.PayloadHash))), "AP004")
	w.wantCode("platform_acting approve proof", approve(ap1, r1.ID, r1.PayloadHash, "approve",
		f.Valid(t, ap1, "platform_acting", tid, actorproof.OpLaunchApprove, r1.ID.String(), ad(r1.PayloadHash))), "AP004")
	w.wantCode("approval with a request proof", approve(ap1, r1.ID, r1.PayloadHash, "approve", tok(ap1, actorproof.OpLaunchRequest, r1.ID, r1.PayloadHash)), "AP004")
	w.wantCode("expired approval proof", approve(ap1, r1.ID, r1.PayloadHash, "approve",
		f.Token(t, ap1, "platform", uuid.Nil, actorproof.OpLaunchApprove, r1.ID.String(), ad(r1.PayloadHash), time.Now().Unix()-120, time.Now().Unix()-60)), "AP003")
	good := tok(ap1, actorproof.OpLaunchApprove, r1.ID, r1.PayloadHash)
	if err := approve(ap1, r1.ID, r1.PayloadHash, "approve", good); err != nil {
		t.Fatalf("the correct approval proof must be accepted: %v", err)
	}
	// Replay on the SAME request by a different transaction: the nonce is spent (AP005 comes first,
	// before the one-decision-per-Person check, because the proof guard is the last BEFORE trigger
	// and the earlier guard would have refused with LA012; either way the second approval is refused).
	err := approve(ap1, r1.ID, r1.PayloadHash, "approve", good)
	if c := pgCode(err); c != "LA012" && c != "AP005" {
		t.Fatalf("replay of a consumed approval proof: %v", err)
	}
	// Replay on another request is AP004/AP005 (target mismatch is checked before the nonce).
	err = approve(w.ops.Approver2, r2.ID, r2.PayloadHash, "approve", good)
	if c := pgCode(err); c != "AP004" {
		t.Fatalf("a consumed proof rebound to another request: %v", err)
	}

	// Cancel: only the requester's proof, bound to this request and its payload.
	cancel := func(who uuid.UUID, id uuid.UUID, tk string) error {
		return w.asPlatform(who, func(ctx context.Context, tx pgx.Tx) error {
			if tk != "" {
				if err := setProof(ctx, tx, tk); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1`, id)
			return err
		})
	}
	w.wantCode("cancel without a proof", cancel(w.ops.Requester, r2.ID, ""), "AP001")
	w.wantCode("cancel with a proof for another request", cancel(w.ops.Requester, r2.ID, tok(w.ops.Requester, actorproof.OpLaunchCancel, r1.ID, r1.PayloadHash)), "AP004")
	w.wantCode("cancel with a request proof", cancel(w.ops.Requester, r2.ID, tok(w.ops.Requester, actorproof.OpLaunchRequest, r2.ID, r2.PayloadHash)), "AP004")
	w.wantCode("cancel by someone else (the earlier guard)", cancel(w.ops.Approver2, r2.ID, tok(w.ops.Approver2, actorproof.OpLaunchCancel, r2.ID, r2.PayloadHash)), "LA011")
	if err := cancel(w.ops.Requester, r2.ID, tok(w.ops.Requester, actorproof.OpLaunchCancel, r2.ID, r2.PayloadHash)); err != nil {
		t.Fatalf("the requester's proof-bound cancel: %v", err)
	}
}

// A pending suspension is executed only by its own requester (migration 0130): another platform
// session cannot drive a suspension it did not file, even with the GUCs.
func TestProof_SuspensionExecutionBoundToRequester(t *testing.T) {
	w := newWorld(t)
	f := w.forger()
	tid := w.activeTenant()
	brand := w.activeBrand(tid)
	s := newSpec(tid, &brand, "suspend")
	var id uuid.UUID
	if err := w.asPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := setProof(ctx, tx, f.Valid(t, w.ops.Requester, "platform", uuid.Nil, actorproof.OpLaunchSuspend, "new", s.digest())); err != nil {
			return err
		}
		var err error
		id, _, err = s.insert(ctx, tx)
		return err
	}); err != nil {
		t.Fatalf("filing a (left pending) suspension: %v", err)
	}
	err := w.asPlatform(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id)
		return err
	})
	w.wantCode("another session executing someone else's suspension", err, "AP004")
	if got := w.status(launchgov.Subject{TenantID: tid, BrandID: &brand}); got != "active" {
		t.Fatalf("brand status %s", got)
	}
}
