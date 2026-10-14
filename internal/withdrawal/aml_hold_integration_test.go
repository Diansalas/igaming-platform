//go:build integration

package withdrawal

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// ADR 0116 s5.1 (LF): `held` gates only NEW value leaving the platform (the withdrawal request, and the payout claim in
// internal/payments). It never gates a release, a completion, a failure, a cancellation or an approval of a request that
// already exists: a held balance stays in the ledger untouched and nothing is recalled.

func (w *a13World) net(t *testing.T, account uuid.UUID) int64 {
	t.Helper()
	var n int64
	if err := w.owner.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(SUM(CASE direction WHEN 'credit' THEN amount ELSE -amount END), 0)::bigint
			FROM ledger_entries WHERE ledger_account_id = $1`, account).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *a13World) holdAll(t *testing.T) {
	t.Helper()
	prooftest.Install(t, tenantTestURL(t))
	amlfix.HoldFor(t, w.owner, w.rt, w.f.tenantID, w.f.personID, nil, aml.ScopeAllValueMovement)
}

func tenantTestURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return u
}

// A withdrawal request on a held player is denied with the SHARED public result and moves no money.
func TestAMLHold_DeniesNewWithdrawalRequest_SharedPublicResult_NoMoneyMoves(t *testing.T) {
	w := newA13(t)
	cash, hold, _ := w.accounts()
	cashBefore, holdBefore := w.net(t, cash), w.net(t, hold)
	w.holdAll(t)

	_, err := requestWithdrawal(t, w.rt, w.f, 700, "aml-held-"+uuid.NewString())
	var denied *KYCDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("a held player's withdrawal request must be denied by the KYC/AML gate: %v", err)
	}
	d := denied.Decision
	if d.Allowed || d.Outcome != kyc.OutcomePending || d.Code != kyc.AccountReviewCode {
		t.Fatalf("the public result must be the shared account-review result: %+v", d)
	}
	if got := w.net(t, cash); got != cashBefore {
		t.Fatalf("a denied request moves no cash: %d -> %d", cashBefore, got)
	}
	if got := w.net(t, hold); got != holdBefore {
		t.Fatalf("a denied request holds nothing: %d -> %d", holdBefore, got)
	}
}

// Nothing that already exists is gated: staff approval, cancellation, rejection (release), completion and failure all run
// under an active all_value_movement hold, and the funds move exactly as they would without it.
func TestAMLHold_NeverGatesReleasesCompletionsOrApprovals(t *testing.T) {
	w := newA13(t)
	// Drive requests to their pre-states BEFORE the hold exists (a request cannot be created under a hold).
	toApprove := w.at(t, StatePendingReview)
	toReject := w.at(t, StatePendingReview)
	toCancel := w.at(t, StateRequested)
	toComplete := w.at(t, StateSubmitted)
	toFail := w.at(t, StateSubmitted)
	cash, hold, clearing := w.accounts()
	cashBefore, holdBefore, clearBefore := w.net(t, cash), w.net(t, hold), w.net(t, clearing)

	w.holdAll(t)

	run := func(what string, fn func(ctx context.Context, tx pgx.Tx) error) {
		t.Helper()
		if err := w.rtTx(fn); err != nil {
			t.Fatalf("%s must NOT be gated by an AML hold: %v", what, err)
		}
	}
	run("a staff approval", func(ctx context.Context, tx pgx.Tx) error {
		ok, err := Approve(ctx, tx, toApprove.ID, w.approver, false, nil, alwaysEligible, auth.StepUpAssertion{})
		if err == nil && !ok {
			err = errors.New("not approved")
		}
		return err
	})
	run("a rejection (hold release)", func(ctx context.Context, tx pgx.Tx) error {
		return Reject(ctx, tx, toReject.ID, w.approver, "aml_nongated", alwaysEligible)
	})
	run("a cancellation", func(ctx context.Context, tx pgx.Tx) error { return Cancel(ctx, tx, toCancel.ID) })
	run("a payout completion", func(ctx context.Context, tx pgx.Tx) error {
		return Complete(ctx, tx, toComplete.ID, a13PSP, "ptx-"+toComplete.ID.String())
	})
	run("a payout failure (release)", func(ctx context.Context, tx pgx.Tx) error { return Fail(ctx, tx, toFail.ID, "aml_nongated") })

	for id, want := range map[uuid.UUID]State{toApprove.ID: StateApproved, toReject.ID: StateRejected, toCancel.ID: StateCancelled, toComplete.ID: StateCompleted, toFail.ID: StateFailed} {
		if got := w.get(t, id); got.State != want {
			t.Fatalf("request %s: state %s, want %s", id, got.State, want)
		}
	}
	// The ledger: released holds returned to cash (reject + cancel + fail), the completed payout left via clearing, the approved
	// request still holds its funds. The hold itself posted nothing.
	rel := toReject.Amount + toCancel.Amount + toFail.Amount
	if got := w.net(t, cash) - cashBefore; got != rel {
		t.Fatalf("player_cash must receive exactly the released holds: +%d, want +%d", got, rel)
	}
	if got := holdBefore - w.net(t, hold); got != rel+toComplete.Amount {
		t.Fatalf("player_withdrawal_hold must drop by the released and the completed holds: -%d, want -%d", got, rel+toComplete.Amount)
	}
	_ = clearBefore
}
