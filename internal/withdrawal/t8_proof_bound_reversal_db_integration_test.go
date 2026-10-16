//go:build integration

// ADR 0110 T8 amendment (migration 0165) at the DATABASE: every 'reverse' decision in withdrawal_approvals is
// bound to a signed actor proof (operation withdrawal_payout_reversal:decide, scope tenant, target the
// withdrawal request id, payload the payout digest carried by the reason_code), checked by
// zz_actor_proof_guard, the LAST BEFORE ROW trigger on the table; WD026 counts only proof-bound decisions.
// The forgery probes run as igaming_runtime in a tenant session (the A5 security probe SR5-2).
package withdrawal

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// t8AsPrincipal makes staff the session principal of the caller's tenant transaction (the shape the
// production decision runs in: alerting.NewPrincipalRunner) and makes sure the per-process proof key is an
// ACTIVE key of the database the transaction runs on (owner pools only; idempotent).
func t8AsPrincipal(ctx context.Context, tx pgx.Tx, staff uuid.UUID) error {
	if _, err := prooftest.EnsureKey(ctx, tx); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, staff.String())
	return err
}

// t8PlainTenantSession clears the principal and the proof again (the plain tenant session of the caller).
func t8PlainTenantSession(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', '', true), set_config('app.actor_proof', '', true)`)
	return err
}

// t8Attacher is the test stand-in of internal/payments' attacher: staff becomes the session principal and
// prooftest signs for the session's actor (the production signer signs for the verified token subject).
func t8Attacher(staff uuid.UUID) ReversalProofAttacher {
	return func(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, digest string) error {
		if err := t8AsPrincipal(ctx, tx, staff); err != nil {
			return err
		}
		return prooftest.AttachForSession(ctx, tx, actorproof.OpPayoutReversalDecide, requestID.String(), digest)
	}
}

// t8VoteUnproven plants a 'reverse' row WITHOUT a proof, the shape of a decision recorded before migration
// 0165 (a legacy row: actor_proof_nonce NULL). Only the table owner can do this (it switches the proof
// guard off inside the caller's transaction and back on); the runtime role cannot (tested below).
func t8VoteUnproven(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest, staff uuid.UUID, reason string) error {
	if _, err := tx.Exec(ctx, `ALTER TABLE withdrawal_approvals DISABLE TRIGGER zz_actor_proof_guard`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals
		(tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code,
		 threshold_amount_at_decision, request_amount_at_decision)
		VALUES ($1, $2, $3, false, 'reverse', $4, 0, $5)`, wr.TenantID, wr.ID, staff, reason, wr.Amount); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE withdrawal_approvals ENABLE TRIGGER zz_actor_proof_guard`)
	return err
}

// t8Runtime connects as igaming_runtime to the database of the shared test pool.
func t8Runtime(t *testing.T) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Fatal("TEST_RUNTIME_DATABASE_URL is required: the forgery probe runs as igaming_runtime")
	}
	p, err := db.Connect(context.Background(), u, 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	var role string
	if err := p.Raw().QueryRow(context.Background(), `SELECT current_user`).Scan(&role); err != nil || role != "igaming_runtime" {
		t.Fatalf("runtime pool connects as %q (%v), want igaming_runtime", role, err)
	}
	return p
}

func t8Owner(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return u
}

// t8E11 posts the pinned reversal and moves the request to reversed (the E11 legs a forger would write).
func t8E11(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest) error {
	id, err := a5PostReversal(ctx, tx, wr, wr.ID.String()+":reversed", "ret-"+wr.ID.String(), wr.Amount)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed', reversal_ledger_transaction_id = $1 WHERE id = $2`, id, wr.ID)
	return err
}

func t8InsertReverse(ctx context.Context, tx pgx.Tx, wr WithdrawalRequest, approver uuid.UUID, reason string) error {
	_, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals
		(tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision, reason_code,
		 threshold_amount_at_decision, request_amount_at_decision)
		VALUES ($1, $2, $3, false, 'reverse', $4, 0, $5)`, wr.TenantID, wr.ID, approver, reason, wr.Amount)
	return err
}

func t8State(t *testing.T, pool *db.Pool, wr WithdrawalRequest) (State, int) {
	t.Helper()
	var st State
	var n int
	mustRunTx(t, pool, wr.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, wr.ID).Scan(&st); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND decision = 'reverse'`, wr.ID).Scan(&n)
	})
	return st, n
}

// SR5-2, the exact probe: as igaming_runtime in a tenant session, two well-shaped 'reverse' rows naming two
// real, active, linked staff of the tenant, the compensating posting and E11 in ONE transaction. Before
// 0165 it committed (state reversed, cash credited). Now the first decision row is refused: CG001 without a
// principal, AP001 when the forger also impersonates the staff member by GUC (no proof), AP002 with a
// proof under a key the database does not hold. Nothing commits.
func TestT8DB_SR52Probe_ForgedReverseRowsRefused_AsRuntimeTenantSession(t *testing.T) {
	owner := testPool(t)
	rt := t8Runtime(t)
	prooftest.Install(t, t8Owner(t))
	f := seedFixture(t, owner, 10_000)
	ref := "t8-sr52-" + uuid.NewString()[:8]
	wr := a5Completed(t, f, 100, ref)
	reason := a5Payload(wr, ref).ReasonCode(wr.ID)
	s1, s2 := mustCreateApprover(t, owner, f.tenantID), mustCreateApprover(t, owner, f.tenantID)
	forge := func(prepare func(ctx context.Context, tx pgx.Tx, staff uuid.UUID) error) error {
		return rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			for _, s := range []uuid.UUID{s1, s2} {
				if err := prepare(ctx, tx, s); err != nil {
					return err
				}
				if err := t8InsertReverse(ctx, tx, wr, s, reason); err != nil {
					return err
				}
			}
			return t8E11(ctx, tx, wr)
		})
	}
	wrongKey := prooftest.Forger{KID: prooftest.ProcessForger(t, t8Owner(t)).KID, Key: prooftest.NewKey(t)}
	for _, c := range []struct {
		name, want string
		prepare    func(ctx context.Context, tx pgx.Tx, staff uuid.UUID) error
	}{
		{"plain tenant session (no principal)", "CG001", func(context.Context, pgx.Tx, uuid.UUID) error { return nil }},
		{"impersonated principal, no proof", "AP001", func(ctx context.Context, tx pgx.Tx, staff uuid.UUID) error {
			_, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true), set_config('app.actor_proof', '', true)`, staff.String())
			return err
		}},
		{"impersonated principal, proof under a wrong key", "AP002", func(ctx context.Context, tx pgx.Tx, staff uuid.UUID) error {
			tok := wrongKey.Valid(t, staff, actorproof.ScopeTenant, f.tenantID, actorproof.OpPayoutReversalDecide, wr.ID.String(),
				strings.TrimPrefix(reason, ReversalReasonPrefix))
			_, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true), set_config('app.actor_proof', $2, true)`, staff.String(), tok)
			return err
		}},
	} {
		if got := a5Code(forge(c.prepare)); got != c.want {
			t.Fatalf("%s: want %s, got %q", c.name, c.want, got)
		}
	}
	if st, n := t8State(t, owner, wr); st != StateCompleted || n != 0 {
		t.Fatalf("a refused forgery must leave the payout completed with no decision rows: %s, %d", st, n)
	}
	// Non-vacuity: the same runtime transaction with two GENUINE proofs (the server-side issuer's) commits.
	err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, s := range []uuid.UUID{s1, s2} {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, s.String()); err != nil {
				return err
			}
			if err := prooftest.AttachForSession(ctx, tx, actorproof.OpPayoutReversalDecide, wr.ID.String(), strings.TrimPrefix(reason, ReversalReasonPrefix)); err != nil {
				return err
			}
			if err := t8InsertReverse(ctx, tx, wr, s, reason); err != nil {
				return err
			}
		}
		return t8E11(ctx, tx, wr)
	})
	if err != nil {
		t.Fatalf("two proof-bound decisions of distinct Persons must execute E11: %v", err)
	}
	if st, n := t8State(t, owner, wr); st != StateReversed || n != 2 {
		t.Fatalf("after the proof-bound quorum: %s, %d rows", st, n)
	}
	// The runtime role cannot switch the proof guard off (only the owner can: ADR 0110 s9.4).
	err = rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE withdrawal_approvals DISABLE TRIGGER zz_actor_proof_guard`)
		return err
	})
	if got := a5Code(err); got != "42501" {
		t.Fatalf("the runtime role must not disable the proof guard: %q (%v)", got, err)
	}
}

// Every proof defect is refused with its ADR 0110 code, in the principal-scoped tenant session the
// production decision runs in, as igaming_runtime: missing (AP001), malformed (AP001), unknown kid / bad MAC
// (AP002), expired / future / over-long (AP003), wrong operation / target / payload / actor / scope / tenant
// (AP004), an approver column that is not the session's principal (AP004), replay (AP005). Shape refusals
// of the guard: an automated reverse row (WD028), a nonce on a non-reverse row (WD028).
func TestT8DB_ProofDefects_Refused(t *testing.T) {
	owner := testPool(t)
	rt := t8Runtime(t)
	ou := t8Owner(t)
	prooftest.Install(t, ou)
	fg := prooftest.ProcessForger(t, ou)
	f := seedFixture(t, owner, 10_000)
	ref := "t8-def-" + uuid.NewString()[:8]
	wr := a5Completed(t, f, 100, ref)
	other := a5Completed(t, f, 100, ref+"-o")
	p := a5Payload(wr, ref)
	digest := p.Digest(wr.ID)
	reason := p.ReasonCode(wr.ID)
	s1, s2 := mustCreateApprover(t, owner, f.tenantID), mustCreateApprover(t, owner, f.tenantID)
	now := time.Now().Unix()
	op := actorproof.OpPayoutReversalDecide
	insert := func(principal, approver uuid.UUID, tok string) error {
		return rt.WithPrincipalScope(context.Background(), f.tenantID, principal, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok); err != nil {
				return err
			}
			return t8InsertReverse(ctx, tx, wr, approver, reason)
		})
	}
	unknownKID := prooftest.Forger{KID: "t8unknownkid", Key: fg.Key}
	for _, c := range []struct{ name, want, tok string }{
		{"missing", "AP001", ""},
		{"malformed", "AP001", "v1|garbage"},
		{"unknown kid", "AP002", unknownKID.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest)},
		{"bad mac", "AP002", prooftest.Forger{KID: fg.KID, Key: prooftest.NewKey(t)}.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest)},
		{"expired", "AP003", fg.Token(t, s1, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest, now-120, now-60)},
		{"issued in the future", "AP003", fg.Token(t, s1, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest, now+60, now+90)},
		{"lifetime over 60 s", "AP003", fg.Token(t, s1, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest, now-1, now+120)},
		{"wrong operation", "AP004", fg.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, actorproof.OpResolutionApprove, wr.ID.String(), digest)},
		{"wrong target", "AP004", fg.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, op, other.ID.String(), digest)},
		{"wrong payload", "AP004", fg.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), a5Payload(other, ref+"-o").Digest(wr.ID))},
		{"wrong actor", "AP004", fg.Valid(t, s2, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest)},
		{"wrong scope", "AP004", fg.Valid(t, s1, actorproof.ScopePlatformActing, f.tenantID, op, wr.ID.String(), digest)},
		{"wrong tenant", "AP004", fg.Valid(t, s1, actorproof.ScopeTenant, uuid.New(), op, wr.ID.String(), digest)},
	} {
		if got := a5Code(insert(s1, s1, c.tok)); got != c.want {
			t.Fatalf("%s: want %s, got %q", c.name, c.want, got)
		}
	}
	// The approver column must be the session's (proven) principal: s1's genuine proof, a row naming s2.
	if got := a5Code(insert(s1, s2, fg.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest))); got != "AP004" {
		t.Fatalf("approver column is not the proven actor: want AP004, got %q", got)
	}
	if _, n := t8State(t, owner, wr); n != 0 {
		t.Fatalf("refusals recorded %d rows", n)
	}
	// Replay: a valid token is consumed once; the same token again is AP005 (before the per-approver UNIQUE).
	tok := fg.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest)
	if err := insert(s1, s1, tok); err != nil {
		t.Fatalf("a valid proof must be admitted: %v", err)
	}
	if got := a5Code(insert(s1, s1, tok)); got != "AP005" {
		t.Fatalf("replay: want AP005, got %q", got)
	}
	// An automated reverse row is refused by the guard's own shape rule (WD028), before the verifier.
	tok2 := fg.Valid(t, s2, actorproof.ScopeTenant, f.tenantID, op, wr.ID.String(), digest)
	err := rt.WithPrincipalScope(context.Background(), f.tenantID, s2, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok2); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval,
			decision, reason_code, threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, $3, true, 'reverse', $4, 0, $5)`,
			wr.TenantID, wr.ID, s2, reason, wr.Amount)
		return err
	})
	if got := a5Code(err); got != "WD028" {
		t.Fatalf("an automated reverse row: want WD028, got %q (%v)", got, err)
	}
	// A write whose transaction fails AFTER the proof was consumed does not burn the nonce (ADR 0110 s5: the
	// nonce row rolls back with the write), so the legitimate retry with the same token succeeds once.
	errLater := errors.New("a later step of the decision transaction failed")
	err = rt.WithPrincipalScope(context.Background(), f.tenantID, s2, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok2); err != nil {
			return err
		}
		if err := t8InsertReverse(ctx, tx, wr, s2, reason); err != nil {
			return err
		}
		return errLater
	})
	if !errors.Is(err, errLater) {
		t.Fatalf("setup: the consumed-then-rolled-back write: %v", err)
	}
	if err := insert(s2, s2, tok2); err != nil {
		t.Fatalf("the nonce of a rolled-back write is not burned: %v", err)
	}
	// A nonce on an approve / reject row is refused (only the guard writes it, on reverse rows).
	err = rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval,
			decision, threshold_amount_at_decision, request_amount_at_decision, actor_proof_nonce) VALUES ($1, $2, gen_random_uuid(), true, 'reject', 0, 1, $3)`,
			wr.TenantID, other.ID, prooftest.Nonce22(t))
		return err
	})
	if got := a5Code(err); got != "WD028" {
		t.Fatalf("a nonce on a reject row: want WD028, got %q (%v)", got, err)
	}
	// The two admitted rows carry the nonce of the proof they consumed, and the nonce table binds it to the
	// actor, operation, target and payload (owner read: actor_proof_nonces is owner-only).
	var bound int
	if err := owner.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_approvals a JOIN actor_proof_nonces n ON n.nonce = a.actor_proof_nonce
			WHERE a.withdrawal_request_id = $1 AND a.decision = 'reverse' AND n.actor = a.approver_principal_id AND n.scope = 'tenant'
			  AND n.tenant = a.tenant_id AND n.operation = $2 AND n.target = $1::text AND n.payload_hash = $3`, wr.ID, op, digest).Scan(&bound)
	}); err != nil || bound != 2 {
		t.Fatalf("both admitted rows must be bound to their consumed proof: %d %v", bound, err)
	}
}

// A caller-supplied nonce on a reverse row is never trusted: the guard overwrites it with the nonce of the
// proof it verified; with no valid proof the row is refused whatever nonce it names.
func TestT8DB_CallerSuppliedNonceIgnored(t *testing.T) {
	owner := testPool(t)
	rt := t8Runtime(t)
	ou := t8Owner(t)
	prooftest.Install(t, ou)
	fg := prooftest.ProcessForger(t, ou)
	f := seedFixture(t, owner, 10_000)
	ref := "t8-nonce-" + uuid.NewString()[:8]
	wr := a5Completed(t, f, 100, ref)
	p := a5Payload(wr, ref)
	s1 := mustCreateApprover(t, owner, f.tenantID)
	planted := prooftest.Nonce22(t)
	ins := func(tok string) error {
		return rt.WithPrincipalScope(context.Background(), f.tenantID, s1, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval,
				decision, reason_code, threshold_amount_at_decision, request_amount_at_decision, actor_proof_nonce) VALUES ($1, $2, $3, false, 'reverse', $4, 0, $5, $6)`,
				wr.TenantID, wr.ID, s1, p.ReasonCode(wr.ID), wr.Amount, planted)
			return err
		})
	}
	if got := a5Code(ins("")); got != "AP001" {
		t.Fatalf("a planted nonce without a proof: want AP001, got %q", got)
	}
	tok := fg.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, actorproof.OpPayoutReversalDecide, wr.ID.String(), p.Digest(wr.ID))
	if err := ins(tok); err != nil {
		t.Fatal(err)
	}
	var nonce string
	mustRunTx(t, owner, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_proof_nonce FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND decision = 'reverse'`, wr.ID).Scan(&nonce)
	})
	if nonce == planted || nonce != strings.Split(tok, "|")[10] {
		t.Fatalf("the stored nonce must be the consumed proof's (%q), not the planted one (%q): %q", strings.Split(tok, "|")[10], planted, nonce)
	}
}

// WD026 counts ONLY proof-bound decisions: two unproven decisions (legacy shape, planted by the owner) of
// two distinct Persons do not reach the floor; one proven + one unproven is one Person; two proven execute.
// The Go count agrees (RecordPayoutReversalDecision counts proof-bound decisions only).
func TestT8DB_UnprovenDecisionsDoNotCount_WD026AndGo(t *testing.T) {
	pool := testPool(t)
	prooftest.Install(t, t8Owner(t))
	f := seedFixture(t, pool, 10_000)
	ref := "t8-wd026-" + uuid.NewString()[:8]
	reasonFor := func(wr WithdrawalRequest, r string) string { return a5Payload(wr, r).ReasonCode(wr.ID) }
	u1, u2 := mustCreateApprover(t, pool, f.tenantID), mustCreateApprover(t, pool, f.tenantID)
	p1, p2 := mustCreateApprover(t, pool, f.tenantID), mustCreateApprover(t, pool, f.tenantID)

	wr := a5Completed(t, f, 100, ref+"-a")
	r := reasonFor(wr, ref+"-a")
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := t8VoteUnproven(ctx, tx, wr, u1, r); err != nil {
			return err
		}
		if err := t8VoteUnproven(ctx, tx, wr, u2, r); err != nil {
			return err
		}
		return t8E11(ctx, tx, wr)
	})
	if got := a5Code(err); got != "WD026" || !strings.Contains(err.Error(), "(found 0)") {
		t.Fatalf("two unproven decisions: want WD026 (found 0), got %q (%v)", got, err)
	}
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := t8VoteUnproven(ctx, tx, wr, u1, r); err != nil {
			return err
		}
		if err := a5Vote(ctx, tx, wr, p1, r); err != nil {
			return err
		}
		return t8E11(ctx, tx, wr)
	})
	if got := a5Code(err); got != "WD026" || !strings.Contains(err.Error(), "(found 1)") {
		t.Fatalf("one proven + one unproven: want WD026 (found 1), got %q (%v)", got, err)
	}
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := t8VoteUnproven(ctx, tx, wr, u1, r); err != nil {
			return err
		}
		if err := a5Vote(ctx, tx, wr, p1, r); err != nil {
			return err
		}
		if err := a5Vote(ctx, tx, wr, p2, r); err != nil {
			return err
		}
		return t8E11(ctx, tx, wr)
	})

	// The Go count: an unproven decision on the same digest is invisible to RecordPayoutReversalDecision.
	wr2 := a5Completed(t, f, 100, ref+"-b")
	pl := a5Payload(wr2, ref+"-b")
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return t8VoteUnproven(ctx, tx, wr2, u1, pl.ReasonCode(wr2.ID)) })
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return t8VoteUnproven(ctx, tx, wr2, u2, pl.ReasonCode(wr2.ID)) })
	no := func(uuid.UUID) (bool, error) { return false, nil }
	var d ReversalDecision
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = RecordPayoutReversalDecision(ctx, tx, wr2.ID, p1, pl, no, alwaysEligible, auth.StepUpAssertion{}, t8Attacher(p1))
		return err
	})
	if d.Counted != 1 || d.Ready {
		t.Fatalf("two unproven + one proven decision must count 1 (not ready): %+v", d)
	}
	// A nil attacher is refused before anything is written.
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordPayoutReversalDecision(ctx, tx, wr2.ID, p2, pl, no, alwaysEligible, auth.StepUpAssertion{}, nil)
		return err
	})
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("a nil proof attacher: %v, want ErrInvalidInput", err)
	}
	// An attacher that attaches nothing: the database refuses the row (AP001), the Go layer surfaces it.
	err = runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordPayoutReversalDecision(ctx, tx, wr2.ID, p2, pl, no, alwaysEligible, auth.StepUpAssertion{},
			func(ctx context.Context, tx pgx.Tx, _ uuid.UUID, _ string) error { return t8AsPrincipal(ctx, tx, p2) })
		return err
	})
	if got := a5Code(err); got != "AP001" {
		t.Fatalf("a decision without a proof: want AP001, got %q (%v)", got, err)
	}
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		d, err = RecordPayoutReversalDecision(ctx, tx, wr2.ID, p2, pl, no, alwaysEligible, auth.StepUpAssertion{}, t8Attacher(p2))
		return err
	})
	if d.Counted != 2 || !d.Ready {
		t.Fatalf("two proven decisions are the quorum: %+v", d)
	}
}

// Concurrency: one genuine proof presented by two sessions at once (a captured-token race) admits exactly
// one row; the other blocks on the nonce UNIQUE and is refused AP005 once the first commits.
func TestT8DB_ConcurrentReplayOfOneProof_ExactlyOneRow(t *testing.T) {
	owner := testPool(t)
	rt := t8Runtime(t)
	ou := t8Owner(t)
	prooftest.Install(t, ou)
	fg := prooftest.ProcessForger(t, ou)
	f := seedFixture(t, owner, 10_000)
	for round := 0; round < 4; round++ {
		ref := "t8-race-" + uuid.NewString()[:8]
		wr := a5Completed(t, f, 100, ref)
		p := a5Payload(wr, ref)
		s1 := mustCreateApprover(t, owner, f.tenantID)
		tok := fg.Valid(t, s1, actorproof.ScopeTenant, f.tenantID, actorproof.OpPayoutReversalDecide, wr.ID.String(), p.Digest(wr.ID))
		var wg sync.WaitGroup
		errs := make([]error, 2)
		start := make(chan struct{})
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				errs[i] = rt.WithPrincipalScope(context.Background(), f.tenantID, s1, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok); err != nil {
						return err
					}
					if err := t8InsertReverse(ctx, tx, wr, s1, p.ReasonCode(wr.ID)); err != nil {
						return err
					}
					_, err := tx.Exec(ctx, `SELECT pg_sleep(0.05)`)
					return err
				})
			}(i)
		}
		close(start)
		wg.Wait()
		ok, replay := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case a5Code(err) == "AP005":
				replay++
			default:
				t.Fatalf("round %d: unexpected %v", round, err)
			}
		}
		if ok != 1 || replay != 1 {
			t.Fatalf("round %d: want one admitted and one AP005, got %d / %d", round, ok, replay)
		}
		if _, n := t8State(t, owner, wr); n != 1 {
			t.Fatalf("round %d: %d rows", round, n)
		}
	}
}

// Catalog pin (security R1 addition 1, ADR 0110 s3): zz_actor_proof_guard is the LAST BEFORE ROW trigger on
// withdrawal_approvals, fires on INSERT, and its function pins search_path and is not SECURITY DEFINER; the
// two replaced 0162 functions keep their pinned search_path.
func TestT8DB_ProofGuardIsLastBeforeRowTrigger_Catalog(t *testing.T) {
	pool := testPool(t)
	var names []string
	var def string
	var cfg []string
	var secdef bool
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tgname FROM pg_trigger WHERE tgrelid = 'withdrawal_approvals'::regclass AND NOT tgisinternal
			AND (tgtype & 1) = 1 AND (tgtype & 2) = 2 ORDER BY tgname COLLATE "C"`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				rows.Close()
				return err
			}
			names = append(names, n)
		}
		rows.Close()
		if err := tx.QueryRow(ctx, `SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgrelid = 'withdrawal_approvals'::regclass AND tgname = 'zz_actor_proof_guard'`).Scan(&def); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT COALESCE(proconfig, '{}'), prosecdef FROM pg_proc WHERE proname = 'actor_proof_withdrawal_approvals_guard'`).Scan(&cfg, &secdef)
	}); err != nil {
		t.Fatal(err)
	}
	if len(names) == 0 || names[len(names)-1] != "zz_actor_proof_guard" {
		t.Fatalf("zz_actor_proof_guard must sort LAST among the BEFORE ROW triggers of withdrawal_approvals, got %v", names)
	}
	if !strings.Contains(def, "BEFORE INSERT ON public.withdrawal_approvals FOR EACH ROW EXECUTE FUNCTION actor_proof_withdrawal_approvals_guard()") {
		t.Fatalf("trigger definition: %s", def)
	}
	if strings.Join(cfg, ",") != "search_path=pg_catalog, public, pg_temp" || secdef {
		t.Fatalf("guard function: proconfig %v, security definer %v", cfg, secdef)
	}
}

// Migration 0165 on a scratch database: decisions recorded BEFORE it (legacy, unproven) stay as they are
// (nonce NULL) and no longer count: E11 on two legacy decisions of distinct Persons is WD026 after the up.
// up -> down restores the 0164 schema byte for byte -> up again equals the first up; the down refuses
// (WD096) while a proof-bound decision exists, changing nothing.
func TestMigration0165_LegacyRowsDoNotCount_UpDownUp_DownRefusal(t *testing.T) {
	ctx := context.Background()
	pool := m132Scratch(t, "m0165_")
	if _, err := pool.MigrateUp(ctx, m132Dir(t, 164)); err != nil {
		t.Fatalf("migrate to 0164: %v", err)
	}
	prooftest.InstallVia(t, pool.Raw(), t.Name()+"/"+uuid.NewString())
	snapBefore := m132Snapshot(t, pool)
	wr := a5ScratchCompleted(t, pool)
	legacyReason := ReversalReasonPrefix + strings.Repeat("6b", 32)
	l1, l2 := mustCreateApprover(t, pool, wr.TenantID), mustCreateApprover(t, pool, wr.TenantID)
	mustRunTx(t, pool, wr.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := t8InsertReverse(ctx, tx, wr, l1, legacyReason); err != nil {
			return err
		}
		return t8InsertReverse(ctx, tx, wr, l2, legacyReason)
	})
	full := m132Dir(t, 165)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0165 up: %v", err)
	}
	snapUp := m132Snapshot(t, pool)
	for _, want := range []string{
		"column:withdrawal_approvals:actor_proof_nonce:text:YES:",
		"index:withdrawal_approvals:withdrawal_approvals_actor_proof_nonce_key:",
		"constraint:withdrawal_approvals:withdrawal_approvals_actor_proof_nonce_shape:",
		"trigger:withdrawal_approvals:zz_actor_proof_guard:",
	} {
		if !strings.Contains(snapUp, want) {
			t.Fatalf("0165 up must create %q", want)
		}
	}
	var legacyNonces int
	mustRunTx(t, pool, wr.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND actor_proof_nonce IS NULL AND decision = 'reverse'`, wr.ID).Scan(&legacyNonces)
	})
	if legacyNonces != 2 {
		t.Fatalf("the two legacy decisions keep a NULL nonce (nothing edited): %d", legacyNonces)
	}
	err := runTx(pool, wr.TenantID, func(ctx context.Context, tx pgx.Tx) error { return t8E11(ctx, tx, wr) })
	if got := a5Code(err); got != "WD026" || !strings.Contains(err.Error(), "(found 0)") {
		t.Fatalf("legacy decisions must not count after 0165: want WD026 (found 0), got %q (%v)", got, err)
	}
	// Down with legacy rows only: allowed (they carry no proof); the snapshot returns to 0164 exactly.
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("migrate 0165 down: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapBefore {
		t.Fatalf("0165 down must restore the 0164 schema byte for byte:\n%s", m132Diff(snapBefore, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0165 up again: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("0165 up again must equal the first up:\n%s", m132Diff(snapUp, got))
	}
	// Down refusal: one proof-bound decision is enough.
	wr2 := a5ScratchCompleted(t, pool)
	staff := mustCreateApprover(t, pool, wr2.TenantID)
	mustRunTx(t, pool, wr2.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return a5Vote(ctx, tx, wr2, staff, ReversalReasonPrefix+strings.Repeat("9a", 32))
	})
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "WD096") {
		t.Fatalf("0165 down must refuse WD096 while a proof-bound decision exists, got %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("a refused down must change nothing:\n%s", m132Diff(snapUp, got))
	}
}
