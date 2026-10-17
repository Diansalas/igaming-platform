//go:build integration

// F4 residual round 1 (migration 0171): the ALLOW decision of the WD-RG-1 dispatch gate is a signed, nonce-consumed,
// scope/operation/target/payload-bound system actor proof verified in SQL (ADR 0110). A forged allow row by the runtime
// credential is refused; every binding is exercised.
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
)

func f4r1Forger(t *testing.T) prooftest.Forger {
	t.Helper()
	return prooftest.ProcessForger(t, os.Getenv("TEST_DATABASE_URL"))
}

// f4r1Digest is the payload hash the database recomputes for a dispatch allow of wr (stage dispatch, no attempt).
func f4r1Digest(wr WithdrawalRequest, stage DispatchStage, attempt *uuid.UUID) string {
	o := "allow"
	return dispatchAllowDigest(wr, stage, attempt, &o, nil)
}

func f4r1Token(t *testing.T, f prooftest.Forger, tenant uuid.UUID, target, payload string) string {
	t.Helper()
	return f.Valid(t, actorproof.SystemActorDispatchGate(), actorproof.ScopeSystem, tenant, actorproof.OpDispatchGateAllow, target, payload)
}

// insertAllowRaw inserts an ALLOWED dispatch row with raw SQL (what a stolen runtime credential can do) after setting
// the proof GUC to tok (empty = none).
func (w *a13World) insertAllowRaw(tok string, wr WithdrawalRequest, riskOutcome string) func(context.Context, pgx.Tx) error {
	return func(ctx context.Context, tx pgx.Tx) error {
		if tok != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, tok); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed, risk_outcome)
			VALUES ($1, $2, 'dispatch', true, NULLIF($3, ''))`, wr.TenantID, wr.ID, riskOutcome)
		return err
	}
}

func TestF4R1_ForgedAllowRow_IsRefused_AndE7StaysRefused(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	f := f4r1Forger(t)

	// 1. No proof at all: AP001, in the very statement; the E7 in the same transaction is never reached.
	wantCode(t, "forged allow, no proof", w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := w.insertAllowRaw("", wr, "allow")(ctx, tx); err != nil {
			return err
		}
		return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
	}), "AP001")
	// 2. A caller-chosen nonce column on a row is ignored/overwritten: still AP001.
	wantCode(t, "forged allow with a chosen nonce column", w.rtExec(`INSERT INTO withdrawal_gate_decisions
		(tenant_id, withdrawal_request_id, stage, allowed, actor_proof_nonce) VALUES ($1, $2, 'dispatch', true, 'AAAAAAAAAAAAAAAAAAAAAA')`, wr.TenantID, wr.ID), "AP001")
	// 3. Wrong key: AP002.
	bad := prooftest.Forger{KID: f.KID, Key: []byte("0123456789abcdef0123456789abcdef")}
	wantCode(t, "wrong key", w.rtTx(w.insertAllowRaw(f4r1Token(t, bad, wr.TenantID, wr.ID.String(), f4r1Digest(wr, DispatchStageDispatch, nil)), wr, "allow")), "AP002")
	// 4. A correctly keyed signer but the row's figures differ from the digest (the attacker cannot sign: this models a
	//    captured proof for a different outcome): AP004.
	wantCode(t, "digest of another outcome", w.rtTx(w.insertAllowRaw(f4r1Token(t, f, wr.TenantID, wr.ID.String(), f4r1Digest(wr, DispatchStageDispatch, nil)), wr, "review")), "AP004")
	// Nothing was recorded and the request is still approved.
	var n int
	mustRunTx(t, w.owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1`, wr.ID).Scan(&n)
	})
	if n != 0 || w.get(t, wr.ID).State != StateApproved {
		t.Fatalf("a refused forgery records nothing and moves nothing: rows=%d state=%s", n, w.get(t, wr.ID).State)
	}
	// A DENYING row needs no proof (it only restricts) and its nonce column is forced NULL.
	if err := w.rtExec(`INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed, gate_code, reason_code, actor_proof_nonce)
		VALUES ($1, $2, 'dispatch', false, 'WD051', 'x', 'AAAAAAAAAAAAAAAAAAAAAA')`, wr.TenantID, wr.ID); err != nil {
		t.Fatalf("an unsigned denial is admitted: %v", err)
	}
	mustRunTx(t, w.owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND actor_proof_nonce IS NOT NULL`, wr.ID).Scan(&n)
	})
	if n != 0 {
		t.Fatal("a denial row must never carry a nonce")
	}
}

func TestF4R1_ValidProof_IsAccepted_NonceRecorded_E7Passes(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	f := f4r1Forger(t)
	tok := f4r1Token(t, f, wr.TenantID, wr.ID.String(), f4r1Digest(wr, DispatchStageDispatch, nil))
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := w.insertAllowRaw(tok, wr, "allow")(ctx, tx); err != nil {
			return err
		}
		return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
	}); err != nil {
		t.Fatalf("a valid system proof must admit the allow row and E7: %v", err)
	}
	nonce := strings.Split(tok, "|")[10]
	var got string
	mustRunTx(t, w.owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_proof_nonce FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND allowed`, wr.ID).Scan(&got)
	})
	if got != nonce {
		t.Fatalf("the row records the consumed nonce %q, got %q", nonce, got)
	}
}

func TestF4R1_ProofBindings_AnotherRequestAttemptTenantStageOperationScopeActor_AreRefused(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	other := w.at(t, StateApproved)
	f := f4r1Forger(t)
	good := f4r1Digest(wr, DispatchStageDispatch, nil)
	otherTenant := uuid.New()
	attempt := uuid.New()
	now := time.Now().Unix()
	cases := []struct {
		name string
		tok  string
		code string
	}{
		{"another request (target)", f4r1Token(t, f, wr.TenantID, other.ID.String(), good), "AP004"},
		{"another tenant", f4r1Token(t, f, otherTenant, wr.ID.String(), good), "AP004"},
		{"another request's digest", f4r1Token(t, f, wr.TenantID, wr.ID.String(), f4r1Digest(other, DispatchStageDispatch, nil)), "AP004"},
		{"redispatch stage digest", f4r1Token(t, f, wr.TenantID, wr.ID.String(), f4r1Digest(wr, DispatchStageRedispatch, nil)), "AP004"},
		{"digest bound to an attempt", f4r1Token(t, f, wr.TenantID, wr.ID.String(), f4r1Digest(wr, DispatchStageDispatch, &attempt)), "AP004"},
		{"digest of a different amount", f4r1Token(t, f, wr.TenantID, wr.ID.String(), func() string { x := wr; x.Amount++; return f4r1Digest(x, DispatchStageDispatch, nil) }()), "AP004"},
		{"another operation", f.Valid(t, actorproof.SystemActorDispatchGate(), actorproof.ScopeSystem, wr.TenantID, "withdrawal_payout_reversal:decide", wr.ID.String(), good), "AP004"},
		{"staff scope", f.Valid(t, actorproof.SystemActorDispatchGate(), actorproof.ScopeTenant, wr.TenantID, actorproof.OpDispatchGateAllow, wr.ID.String(), good), "AP004"},
		{"another actor", f.Valid(t, uuid.New(), actorproof.ScopeSystem, wr.TenantID, actorproof.OpDispatchGateAllow, wr.ID.String(), good), "AP004"},
		{"expired", f.Token(t, actorproof.SystemActorDispatchGate(), actorproof.ScopeSystem, wr.TenantID, actorproof.OpDispatchGateAllow, wr.ID.String(), good, now-120, now-60), "AP003"},
		{"lifetime over 60 s", f.Token(t, actorproof.SystemActorDispatchGate(), actorproof.ScopeSystem, wr.TenantID, actorproof.OpDispatchGateAllow, wr.ID.String(), good, now, now+600), "AP003"},
	}
	for _, c := range cases {
		wantCode(t, c.name, w.rtTx(w.insertAllowRaw(c.tok, wr, "allow")), c.code)
	}
	// The same system proof is not accepted for ANY other governed table (the verifier closes the operation to scope
	// 'system' and the scope to the operation): a K1 style write with it is refused by the verifier itself.
	wantCode(t, "system scope for another operation", w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, f4r1Token(t, f, wr.TenantID, wr.ID.String(), good)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `SELECT actor_proof_require($1, 'system', $2, 'ledger_adjustment:approve', $3, $4)`,
			actorproof.SystemActorDispatchGate(), wr.TenantID, wr.ID.String(), good)
		return err
	}), "AP004")
	// Every refusal above recorded nothing.
	var n int
	mustRunTx(t, w.owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1`, wr.ID).Scan(&n)
	})
	if n != 0 {
		t.Fatalf("refusals record nothing, found %d rows", n)
	}
}

func TestF4R1_ReplayedProof_IsRefused_AndARolledBackUseDoesNotBurnIt(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	f := f4r1Forger(t)
	tok := f4r1Token(t, f, wr.TenantID, wr.ID.String(), f4r1Digest(wr, DispatchStageDispatch, nil))
	// A rolled-back use (a later failure in the claim) does not burn the nonce: the retry succeeds.
	errRollback := errors.New("later failure")
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := w.insertAllowRaw(tok, wr, "allow")(ctx, tx); err != nil {
			return err
		}
		return errRollback
	}); !errors.Is(err, errRollback) {
		t.Fatalf("want the rollback error, got %v", err)
	}
	if err := w.rtTx(w.insertAllowRaw(tok, wr, "allow")); err != nil {
		t.Fatalf("the retry of a rolled-back proof must succeed: %v", err)
	}
	// Committed: the same proof can never be used again (nonce UNIQUE), not even for the same row shape.
	wantCode(t, "replay", w.rtTx(w.insertAllowRaw(tok, wr, "allow")), "AP005")
}

func TestF4R1_ConcurrentUseOfOneProof_ExactlyOneWins(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	f := f4r1Forger(t)
	tok := f4r1Token(t, f, wr.TenantID, wr.ID.String(), f4r1Digest(wr, DispatchStageDispatch, nil))
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = w.rtTx(w.insertAllowRaw(tok, wr, "allow"))
		}()
	}
	wg.Wait()
	ok, replay := 0, 0
	for _, e := range errs {
		switch {
		case e == nil:
			ok++
		case a13Code(e) == "AP005":
			replay++
		default:
			t.Fatalf("unexpected error: %v", e)
		}
	}
	if ok != 1 || replay != 3 {
		t.Fatalf("one proof, one consumption: ok=%d replay=%d", ok, replay)
	}
}

// The real writer (RecordDispatchGateDecision) signs, the proof binds the stage and attempt: a normal allow passes E7;
// an unsigned issuer fails closed as ErrDispatchProofUnavailable; a denial needs no issuer.
func TestF4R1_RecordDispatchGateDecision_SignsAllow_FailsClosedWithoutIssuer(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := f4Record(ctx, tx, wr.ID, f4Allow); err != nil {
			return err
		}
		return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
	}); err != nil {
		t.Fatalf("a normal signed allow admits E7: %v", err)
	}

	wr2 := w.at(t, StateApproved)
	saved := actorproof.Default()
	actorproof.SetDefault(nil)
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error { return f4Record(ctx, tx, wr2.ID, f4Allow) })
	actorproof.SetDefault(saved)
	if !errors.Is(err, ErrDispatchProofUnavailable) || !errors.Is(err, ErrGateUnavailable) {
		t.Fatalf("no issuer: want ErrDispatchProofUnavailable (wrapping ErrGateUnavailable), got %v", err)
	}
}

// Migration 0171 on a THROWAWAY scratch database: up -> down restores the 0170 schema byte for byte -> up again equals
// the first up; the down refuses (WD095) while a proof-bound decision exists, changing nothing.
func TestMigration0171_UpDownUp_SnapshotAndDownRefusal(t *testing.T) {
	ctx := context.Background()
	pool := m132Scratch(t, "m0171_")
	if _, err := pool.MigrateUp(ctx, m132Dir(t, 170)); err != nil {
		t.Fatalf("migrate to 0170: %v", err)
	}
	snapBefore := m132Snapshot(t, pool)
	full := m132Dir(t, 171)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0171 up: %v", err)
	}
	snapUp := m132Snapshot(t, pool)
	if snapUp == snapBefore {
		t.Fatal("0171 changed nothing (non-vacuity)")
	}
	for _, want := range []string{"withdrawal_gate_decisions_zz_actor_proof_guard", "actor_proof_nonce"} {
		if !strings.Contains(snapUp, want) {
			t.Fatalf("0171 up must create %q", want)
		}
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("migrate 0171 down: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapBefore {
		t.Fatalf("0171 down must restore the previous schema byte for byte:\n%s", m132Diff(snapBefore, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0171 up again: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("0171 up again must equal the first up:\n%s", m132Diff(snapUp, got))
	}
	// Down refusal: one proof-bound decision row.
	f := seedFixture(t, pool, 10_000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "m0171-"+uuid.NewString())
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MoveToPendingReview(ctx, tx, wr.ID) })
	approver := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, decision,
			threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, $3, 'approve', 0, 100)`, f.tenantID, wr.ID, approver); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, wr.ID); err != nil {
			return err
		}
		return f4Record(ctx, tx, wr.ID, f4Allow)
	})
	_, err := pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "WD095") {
		t.Fatalf("0171 down must refuse WD095 while a proof-bound decision exists, got %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("a refused down must change nothing:\n%s", m132Diff(snapUp, got))
	}
}
