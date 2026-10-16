//go:build integration

// ADR 0110 T8 amendment (migration 0165), the Go entry point: DecidePayoutReversal signs the decider's actor
// proof (withdrawal_payout_reversal:decide, scope tenant, target the request, payload the payout digest)
// through actorproof.Issuer.Attach, the same way the K3 (M2) and hold-release services do; a signing failure
// is returned (never skipped) and records nothing. WD027 (the tombstone branch) counts only proof-bound
// decisions.
package payments

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/stepup"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// t8AsPrincipalWithProof makes staff the session principal and attaches a proof for a 'reverse' decision of
// requestID on reason (prooftest signs for the session's actor, as the server-side issuer signs for the
// authenticated token subject).
func t8AsPrincipalWithProof(ctx context.Context, tx pgx.Tx, staff, requestID uuid.UUID, reason string) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, staff.String()); err != nil {
		return err
	}
	return prooftest.AttachForSession(ctx, tx, actorproof.OpPayoutReversalDecide, requestID.String(),
		strings.TrimPrefix(reason, withdrawal.ReversalReasonPrefix))
}

func t8PgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// The production signer: both decisions of the executed reversal carry the nonce of the proof each consumed,
// bound in actor_proof_nonces to the decider, scope tenant, the tenant, the operation, the request and the
// payload digest. A missing issuer, a key the database does not hold and a staff request without a fresh
// step-up are each refused (ErrNoIssuer / AP002 / ErrStepUpMissing) and record nothing.
func TestT8_DecidePayoutReversal_SignsForTheDecider_AndFailuresRecordNothing(t *testing.T) {
	e := newRBEnv(t, "mock-t8-sign")
	wr, _ := e.a5Completed(t, "t8-s", "t8-ref-s")
	p := e.a5Payload("t8-ref-s")
	s1, s2 := e.a5Staff(t), e.a5Staff(t)
	noRows := func(t *testing.T) {
		t.Helper()
		if n := e.a5Count(t, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id=$1 AND decision='reverse'`, wr.ID); n != 0 {
			t.Fatalf("a refused call recorded %d decisions", n)
		}
	}
	t.Run("no issuer", func(t *testing.T) {
		prev := actorproof.Default()
		actorproof.SetDefault(nil)
		defer actorproof.SetDefault(prev)
		if _, err := e.a5Decide(s1, wr.ID, p); !errors.Is(err, actorproof.ErrNoIssuer) {
			t.Fatalf("got %v, want ErrNoIssuer", err)
		}
		noRows(t)
	})
	t.Run("key unknown to the database", func(t *testing.T) {
		kid := prooftest.RandomKID(t)
		iss, err := actorproof.NewIssuer(kid, map[string][]byte{kid: prooftest.NewKey(t)})
		if err != nil {
			t.Fatal(err)
		}
		_, err = DecidePayoutReversal(context.Background(), e.pool, e.f.tenantID, wr.ID, PayoutReversalInput{
			ApproverID: s1, Payload: p, Checks: a5Checks(wr.ID), Proofs: iss})
		if got := t8PgCode(err); got != "AP002" {
			t.Fatalf("got %q (%v), want AP002", got, err)
		}
		noRows(t)
	})
	t.Run("staff request without a fresh step-up (signer backstop)", func(t *testing.T) {
		ctx := stepup.WithStaffRequest(context.Background())
		_, err := DecidePayoutReversal(ctx, e.pool, e.f.tenantID, wr.ID, PayoutReversalInput{ApproverID: s1, Payload: p, Checks: a5Checks(wr.ID)})
		if !errors.Is(err, actorproof.ErrStepUpMissing) {
			t.Fatalf("got %v, want ErrStepUpMissing", err)
		}
		noRows(t)
	})
	e.a5MustDecide(t, s1, wr.ID, p, PayoutReversalRecorded)
	e.a5MustDecide(t, s2, wr.ID, p, PayoutReversalExecuted)
	if n := e.a5Count(t, `SELECT count(*) FROM withdrawal_approvals a JOIN actor_proof_nonces n ON n.nonce = a.actor_proof_nonce
		WHERE a.withdrawal_request_id = $1 AND a.decision = 'reverse' AND n.actor = a.approver_principal_id AND n.scope = 'tenant'
		  AND n.tenant = a.tenant_id AND n.operation = 'withdrawal_payout_reversal:decide' AND n.target = $1::text AND n.payload_hash = $2`,
		wr.ID, p.Digest(wr.ID)); n != 2 {
		t.Fatalf("both decisions must be bound to the proof each consumed, got %d", n)
	}
	loAssertBalanced(t, e.pool, e.f.tenantID)
	loAssertProjectionMatchesRebuild(t, e.pool, e.f.tenantID)
}

// WD027 counts ONLY proof-bound decisions: two grant holders' decisions WITHOUT a proof (the shape recorded
// before 0165, planted by the owner) are zero counted Persons, so the payout tombstone is refused (found 0)
// and the Go quorum does not see them either. A holder whose legacy vote occupies the per-approver slot
// cannot vote again (ErrDuplicateApproval: residual, legacy rows only); two other proof-bound grant
// holders complete the tombstone branch.
func TestT8_WD027_UnprovenGrantHolderDecisionsDoNotCount(t *testing.T) {
	e, w := a5K3(t)
	wr, a := w.payout(500)
	ref := *a.ProviderReference
	p := e.a5Payload(ref)
	reason := p.ReasonCode(wr.ID)
	if err := e.pool.WithPrincipalScope(context.Background(), e.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE withdrawal_approvals DISABLE TRIGGER zz_actor_proof_guard`); err != nil {
			return err
		}
		for _, s := range []uuid.UUID{w.f1.ID, w.f2.ID} {
			if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval,
				decision, reason_code, threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, $3, false, 'reverse', $4, 0, 500)`,
				e.f.tenantID, wr.ID, s, reason); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `ALTER TABLE withdrawal_approvals ENABLE TRIGGER zz_actor_proof_guard`)
		return err
	}); err != nil {
		t.Fatalf("owner plant (legacy unproven decisions): %v", err)
	}
	err := e.pool.WithPrincipalScope(context.Background(), e.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		pid := e.pid
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: e.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "tombstone:" + pid + ":" + ref, ProviderID: &pid, ProviderTxID: &ref, CorrelationID: wr.ID})
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "WD027" || !strings.Contains(pgErr.Message, "(found 0)") {
		t.Fatalf("two unproven grant-holder decisions: want WD027 (found 0), got %v", err)
	}
	if _, err := e.a5Decide(w.f1.ID, wr.ID, p); !errors.Is(err, withdrawal.ErrDuplicateApproval) {
		t.Fatalf("a legacy vote occupies its approver's slot: %v, want ErrDuplicateApproval", err)
	}
	if r := e.a5MustDecide(t, w.f3.ID, wr.ID, p, PayoutReversalRecorded); r.Counted != 1 {
		t.Fatalf("the Go quorum must ignore the two unproven decisions: %+v", r)
	}
	e.a5MustDecide(t, w.f4.ID, wr.ID, p, PayoutReversalTombstoned)
	if n := e.a5Count(t, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND decision = 'reverse' AND actor_proof_nonce IS NOT NULL`, wr.ID); n != 2 {
		t.Fatalf("proof-bound decisions = %d, want 2", n)
	}
	w.assertInvariants()
}
