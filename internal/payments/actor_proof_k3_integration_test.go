//go:build integration

// PRH-2 R5 (SIGNED-ACTOR-PROOF, ADR 0110, migration 0120) for K3 manual
// payment resolutions (ADR 0101). An authorized DEFENSIVE test of this
// repository's own control on a private scratch database with synthetic data,
// as the REAL runtime role (TEST_RUNTIME_DATABASE_URL, asserted neither
// superuser nor BYPASSRLS): arbitrary SQL that sets the actor GUCs of two real,
// eligible finance staff cannot request or approve a resolution without a proof
// that is exactly right for that actor, operation, target and payload.
//
// Non-vacuity: every negative matrix has a control in which the same write with
// an exactly-bound proof is accepted; the positive test drives the real
// ManualResolutionService as the runtime role to a genuine two-person execution.
package payments

import (
	"context"
	"crypto/rand"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// k3RuntimeWorld builds a K3 world on a scratch database migrated to the latest
// migration (0120 in force), with the per-process signing key provisioned by the
// owner, and returns the world (owner pool for fixtures) plus a pool connected
// as the runtime role. w.svc speaks as the runtime role.
func k3RuntimeWorld(t *testing.T, opts k3Opts) (*k3World, *db.Pool, prooftest.Forger) {
	t.Helper()
	rtBase := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtBase == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role test")
	}
	scratchURL := scratchdb.New(t, "k3sap_")
	su, err := url.Parse(scratchURL)
	if err != nil {
		t.Fatal(err)
	}
	ru, err := url.Parse(rtBase)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = su.Path
	ctx := context.Background()
	owner, err := db.Connect(ctx, scratchURL, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(owner.Close)
	dbName := strings.TrimPrefix(su.Path, "/")
	// The runtime role's grants on the scratch database (priv_db.sh pattern:
	// database-level GRANTs only; no role is created or altered).
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO igaming_runtime`,
		`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO igaming_runtime`,
	} {
		if _, err := owner.Raw().Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, err := owner.MigrateUp(ctx, realMigrationsDir(t)); err != nil {
		t.Fatalf("migrate scratch to latest: %v", err)
	}
	prooftest.InstallVia(t, owner.Raw(), scratchURL)
	kid, key := prooftest.ProcessKey(t, scratchURL)
	rt, err := db.Connect(ctx, ru.String(), 4, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime role: %v", err)
	}
	t.Cleanup(rt.Close)
	var super, bypass bool
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatal(err)
	}
	if super || bypass {
		t.Fatalf("must run as a role that is neither superuser nor BYPASSRLS (super=%v bypassrls=%v)", super, bypass)
	}
	w := newK3WorldOn(t, owner, opts)
	w.svc = NewManualResolutionService(rt, w.reg)
	return w, rt, prooftest.Forger{KID: kid, Key: key}
}

func (w *k3World) approvalRows(id uuid.UUID) int {
	w.t.Helper()
	var n int
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_manual_resolution_approvals WHERE resolution_id = $1`, id).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *k3World) resolutionRows() int {
	w.t.Helper()
	var n int
	if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.f.tenantID).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *k3World) proofNonces() int {
	w.t.Helper()
	var n int
	if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM actor_proof_nonces WHERE tenant = $1`, w.f.tenantID).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// k3Attack is arbitrary SQL as the runtime role: exactly the GUCs a stolen
// credential can set (tenant + principal), plus whatever proof the attacker
// chooses.
func k3Attack(rt *db.Pool, w *k3World, actor k3Staff, proof string, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return rt.WithPrincipalScope(context.Background(), w.f.tenantID, actor.ID, func(ctx context.Context, tx pgx.Tx) error {
		if proof != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); err != nil {
				return err
			}
		}
		return fn(ctx, tx)
	})
}

func k3RawApprove(ctx context.Context, tx pgx.Tx, w *k3World, r ManualResolution, decision, payload string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO payment_manual_resolution_approvals
			(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
		VALUES ($1, $2, $3, $4, $5, 'tenant', $5, 0, 'k3-attack')`, w.f.tenantID, r.ID, decision, payload, uuid.Nil)
	return err
}

func k3RawRequest(ctx context.Context, tx pgx.Tx, w *k3World, in ResolutionRequestInput) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO payment_manual_resolutions
			(tenant_id, attempt_id, operation, kind, finding_code, basis_code, context_code, evidence_ref_hash,
			 amount, asset_code, brand_id, reason_code, attempt_state_at_submission, ever_possibly_sent_at_submission,
			 payload_hash, requested_by, requested_by_scope, requested_by_person_id, tenant_status_at_submission,
			 required_at_submission, contributing_policy_ids, expires_at)
		VALUES ($1, $2, 'deposit', $3, $4, $5, $6, $7, 1, '-', $8, $9, '-', false, '-', $8, 'tenant', $8, '-', 1, '{}', now())`,
		w.f.tenantID, in.AttemptID, string(in.Kind), nilIfEmpty(in.FindingCode), nilIfEmpty(in.BasisCode),
		nilIfEmpty(in.ContextCode), nilIfEmpty(in.EvidenceRefHash), uuid.Nil, in.ReasonCode)
	return err
}

func k3SubmitDigest(w *k3World, in ResolutionRequestInput) string {
	return actorproof.Digest(actorproof.S(w.f.tenantID.String()), actorproof.S(in.AttemptID.String()), actorproof.S(string(in.Kind)),
		actorproof.SP(in.FindingCode), actorproof.SP(in.BasisCode), actorproof.SP(in.ContextCode), actorproof.SP(in.EvidenceRefHash),
		actorproof.S(in.ReasonCode))
}

// (1a) ADVERSARIAL, approve: GUC impersonation of eligible finance staff.
func TestActorProofK3_Adversarial_Approve_FailsClosed(t *testing.T) {
	w, rt, f := k3RuntimeWorld(t, k3Opts{base: 2})
	_, a := w.ambiguousPayout(100)
	_, a2 := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid)) // genuine, server-issued proof
	r2 := w.mustRequest(w.f1, w.m2In(a2.ID, ResolutionM2DeclareNotPaid))
	tid := w.f.tenantID
	nowU := time.Now().Unix()
	const op = "payment_force_resolve:approve"

	badKey := make([]byte, 32)
	_, _ = rand.Read(badKey)
	cases := []struct {
		name  string
		actor k3Staff
		proof string
		want  string
	}{
		{"no proof (GUC impersonation only)", w.f2, "", "AP001"},
		{"garbage proof", w.f2, "x", "AP001"},
		{"wrong signing key", w.f2, prooftest.Forger{KID: f.KID, Key: badKey}.Valid(t, w.f2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), "AP002"},
		{"unknown kid", w.f2, prooftest.Forger{KID: "nokid", Key: f.Key}.Valid(t, w.f2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), "AP002"},
		{"expired", w.f2, f.Token(t, w.f2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash, nowU-120, nowU-60), "AP003"},
		{"future", w.f2, f.Token(t, w.f2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash, nowU+600, nowU+630), "AP003"},
		{"lifetime > 60s", w.f2, f.Token(t, w.f2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash, nowU, nowU+3600), "AP003"},
		{"different operation", w.f2, f.Valid(t, w.f2.ID, "tenant", tid, "payment_force_resolve:reject", r.ID.String(), r.PayloadHash), "AP004"},
		{"different target", w.f2, f.Valid(t, w.f2.ID, "tenant", tid, op, r2.ID.String(), r.PayloadHash), "AP004"},
		{"different actor", w.f2, f.Valid(t, w.f3.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), "AP004"},
		{"different tenant", w.f2, f.Valid(t, w.f2.ID, "tenant", uuid.New(), op, r.ID.String(), r.PayloadHash), "AP004"},
		{"different scope", w.f2, f.Valid(t, w.f2.ID, "platform_acting", tid, op, r.ID.String(), r.PayloadHash), "AP004"},
		{"different payload", w.f2, f.Valid(t, w.f2.ID, "tenant", tid, op, r.ID.String(), strings.Repeat("0", 64)), "AP004"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := k3Attack(rt, w, c.actor, c.proof, func(ctx context.Context, tx pgx.Tx) error {
				return k3RawApprove(ctx, tx, w, r, "approve", r.PayloadHash)
			})
			if err == nil {
				t.Fatal("the approval was ACCEPTED without a valid proof for exactly this write")
			}
			k3RequireCode(t, err, c.want)
		})
	}
	t.Run("approve proof presented for a reject decision", func(t *testing.T) {
		err := k3Attack(rt, w, w.f2, f.Valid(t, w.f2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), func(ctx context.Context, tx pgx.Tx) error {
			return k3RawApprove(ctx, tx, w, r, "reject", r.PayloadHash)
		})
		k3RequireCode(t, err, "AP004")
	})
	t.Run("replayed nonce", func(t *testing.T) {
		tok := f.Valid(t, w.f2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash)
		consume := func() error {
			return k3Attack(rt, w, w.f2, tok, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `SELECT actor_proof_require($1, 'tenant', $2, $3, $4, $5)`, w.f2.ID, tid, op, r.ID.String(), r.PayloadHash)
				return err
			})
		}
		if err := consume(); err != nil {
			t.Fatalf("first consumption: %v", err)
		}
		err := k3Attack(rt, w, w.f2, tok, func(ctx context.Context, tx pgx.Tx) error {
			return k3RawApprove(ctx, tx, w, r, "approve", r.PayloadHash)
		})
		k3RequireCode(t, err, "AP005")
		k3RequireCode(t, consume(), "AP005")
	})
	if got := w.approvalRows(r.ID); got != 0 {
		t.Fatalf("approval rows exist after the attack matrix: %d", got)
	}
	if got := w.resolution(r.ID).State; got != ResolutionPending {
		t.Fatalf("resolution %s", got)
	}

	// CONTROL (non-vacuity): the exactly-bound proof is accepted.
	if err := k3Attack(rt, w, w.f2, f.Valid(t, w.f2.ID, "tenant", tid, op, r.ID.String(), r.PayloadHash), func(ctx context.Context, tx pgx.Tx) error {
		return k3RawApprove(ctx, tx, w, r, "approve", r.PayloadHash)
	}); err != nil {
		t.Fatalf("control: %v", err)
	}
	if got := w.approvalRows(r.ID); got != 1 {
		t.Fatalf("control: want 1 approval, got %d", got)
	}
	if got := w.resolution(r.ID).State; got != ResolutionPending {
		t.Fatalf("control: required=2, one approval must leave the resolution pending, got %s", got)
	}
	w.assertInvariants()
}

// (1b) ADVERSARIAL, request + cancel.
func TestActorProofK3_Adversarial_Request_And_Cancel_FailClosed(t *testing.T) {
	w, rt, f := k3RuntimeWorld(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	in := w.m2In(a.ID, ResolutionM2DeclareNotPaid)
	digest := k3SubmitDigest(w, in)
	tid := w.f.tenantID
	other := in
	other.ReasonCode = "other-reason"
	const op = "payment_force_resolve:request"
	cases := []struct {
		name  string
		proof string
		want  string
	}{
		{"no proof", "", "AP001"},
		{"garbage", "a|b", "AP001"},
		{"different payload digest", f.Valid(t, w.f1.ID, "tenant", tid, op, "new", k3SubmitDigest(w, other)), "AP004"},
		{"different target (a concrete id, not 'new')", f.Valid(t, w.f1.ID, "tenant", tid, op, uuid.NewString(), digest), "AP004"},
		{"different actor", f.Valid(t, w.f2.ID, "tenant", tid, op, "new", digest), "AP004"},
		{"different operation", f.Valid(t, w.f1.ID, "tenant", tid, "payment_force_resolve:cancel", "new", digest), "AP004"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := k3Attack(rt, w, w.f1, c.proof, func(ctx context.Context, tx pgx.Tx) error { return k3RawRequest(ctx, tx, w, in) })
			if err == nil {
				t.Fatal("the request was ACCEPTED without a valid proof for exactly this write")
			}
			k3RequireCode(t, err, c.want)
		})
	}
	if got := w.resolutionRows(); got != 0 {
		t.Fatalf("resolution rows after the attack matrix: %d", got)
	}
	// CONTROL, then REPLAY.
	tok := f.Valid(t, w.f1.ID, "tenant", tid, op, "new", digest)
	if err := k3Attack(rt, w, w.f1, tok, func(ctx context.Context, tx pgx.Tx) error { return k3RawRequest(ctx, tx, w, in) }); err != nil {
		t.Fatalf("control: %v", err)
	}
	if got := w.resolutionRows(); got != 1 {
		t.Fatalf("control: want 1 resolution, got %d", got)
	}
	k3RequireCode(t, k3Attack(rt, w, w.f1, tok, func(ctx context.Context, tx pgx.Tx) error { return k3RawRequest(ctx, tx, w, in) }), "AP005")
	if got := w.resolutionRows(); got != 1 {
		t.Fatalf("replay created another resolution: %d", got)
	}
	// Cancel: the requester's GUCs alone cannot cancel.
	var rid uuid.UUID
	if err := w.pool.WithPrincipalScope(context.Background(), tid, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_manual_resolutions WHERE tenant_id = $1`, tid).Scan(&rid)
	}); err != nil {
		t.Fatal(err)
	}
	err := k3Attack(rt, w, w.f1, "", func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET state = 'cancelled' WHERE id = $1`, rid)
		return e
	})
	k3RequireCode(t, err, "AP001")
	if got := w.resolution(rid).State; got != ResolutionPending {
		t.Fatalf("an unproven cancel changed the state to %s", got)
	}
	w.assertInvariants()
}

// (1c) The acting-session shape.
func TestActorProofK3_Adversarial_ActingSession_FailsClosed(t *testing.T) {
	w, rt, f := k3RuntimeWorld(t, k3Opts{base: 2})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	acting := func(proof string) error {
		return rt.WithPlatformActingInTenant(context.Background(), w.acting.ID, w.f.tenantID, r.ID, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
			if proof != "" {
				if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, proof); err != nil {
					return err
				}
			}
			return k3RawApprove(ctx, tx, w, r, "approve", r.PayloadHash)
		})
	}
	const op = "payment_force_resolve:approve"
	k3RequireCode(t, acting(""), "AP001")
	k3RequireCode(t, acting(f.Valid(t, w.acting.ID, "tenant", w.f.tenantID, op, r.ID.String(), r.PayloadHash)), "AP004")
	k3RequireCode(t, acting(f.Valid(t, w.acting2.ID, "platform_acting", w.f.tenantID, op, r.ID.String(), r.PayloadHash)), "AP004")
	if got := w.approvalRows(r.ID); got != 0 {
		t.Fatalf("approval rows after the acting attack: %d", got)
	}
	if err := acting(f.Valid(t, w.acting.ID, "platform_acting", w.f.tenantID, op, r.ID.String(), r.PayloadHash)); err != nil {
		t.Fatalf("control: %v", err)
	}
	if got := w.approvalRows(r.ID); got != 1 {
		t.Fatalf("control: want 1 approval, got %d", got)
	}
}

// (2) POSITIVE through the real ManualResolutionService as the runtime role:
// genuine two-person execution (distinct requester and approver), exactly one
// posting, SUM(D)=SUM(C), projection=rebuild, refused retries consume no nonce.
func TestActorProofK3_Positive_TwoPerson_RealServicePath(t *testing.T) {
	w, _, _ := k3RuntimeWorld(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	n0 := w.proofNonces()
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	if got := w.proofNonces(); got != n0+1 {
		t.Fatalf("request must consume exactly one nonce, consumed %d", got-n0)
	}
	// The requester cannot approve (distinct-person floor) even with a valid proof.
	if _, err := w.decide(w.f1, r, ResolutionApprove); k3Code(err) != "MR031" {
		t.Fatalf("self-approval: want MR031, got %v", err)
	}
	if got := w.proofNonces(); got != n0+1 {
		t.Fatalf("a refused write consumed a nonce: %d", got-n0)
	}
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("second distinct person: %v %+v", err, out)
	}
	after := w.proofNonces()
	// Client retry of the same logical approval after success: a fresh proof is
	// issued, the resolution's own guards refuse it, nothing more is posted.
	for i := 0; i < 2; i++ {
		if _, err := w.decide(w.f2, r, ResolutionApprove); err == nil {
			t.Fatal("a retried approval of an executed resolution was accepted")
		}
	}
	if got := w.proofNonces(); got != after {
		t.Fatalf("refused retries consumed nonces: %d", got-after)
	}
	if got := w.resolution(r.ID).State; got != ResolutionExecuted {
		t.Fatalf("state %s", got)
	}
	w.assertInvariants()
}

// (2b) POSITIVE, acting path + cancel, with server-issued proofs.
func TestActorProofK3_Positive_ActingPath_And_Cancel(t *testing.T) {
	w, _, _ := k3RuntimeWorld(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	out, err := w.svc.Decide(k3Ctx(w.acting), w.target(w.acting), r.ID,
		ResolutionDecisionInput{Decision: ResolutionApprove, PayloadHash: r.PayloadHash, ReasonCode: "k3-test"}, ResolutionMeta{RequestID: "acting"})
	if err != nil || !out.Executed {
		t.Fatalf("acting approval with a platform_acting proof: %v %+v", err, out)
	}
	_, a2 := w.ambiguousPayout(100)
	r2 := w.mustRequest(w.f1, w.m2In(a2.ID, ResolutionM2DeclareNotPaid))
	if _, err := w.svc.Cancel(k3Ctx(w.f2), w.target(w.f2), r2.ID, ResolutionMeta{}); err == nil {
		t.Fatal("a non-requester cancelled")
	}
	c, err := w.svc.Cancel(k3Ctx(w.f1), w.target(w.f1), r2.ID, ResolutionMeta{})
	if err != nil || c.State != ResolutionCancelled {
		t.Fatalf("requester cancel with a proof: %v %+v", err, c)
	}
	w.assertInvariants()
}
