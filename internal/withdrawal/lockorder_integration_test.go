//go:build integration

// ADR 0082 §6 test 3 - finding LOCK-1c, and §6 test 5's withdrawal-shaped
// companion (the ledger_accounts creation-order residual §1.7 names).
package withdrawal

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// loRequestState reads a request's current state - used to prove the
// racing transition actually happened, never inferred from a balance.
func loRequestState(t *testing.T, pool *db.Pool, tenantID, requestID uuid.UUID) State {
	t.Helper()
	var wr WithdrawalRequest
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = GetByID(ctx, tx, requestID)
		return err
	})
	if err != nil {
		t.Fatalf("read request state: %v", err)
	}
	return wr.State
}

// TestLockOrder_ConcurrentWithdrawalRequestAndRejection_NoDeadlock is
// finding LOCK-1c (ADR 0082 §1.7), recorded for the first time by that
// inventory:
//
//	RequestWithdrawal takes (player_cash, player_withdrawal_hold) - the
//	first EXPLICITLY, via the now-deleted lockCashBalanceForUpdate, and
//	the second implicitly inside ledger.Post. Reject, Fail and Cancel
//	take the SAME PAIR in the OPPOSITE order, because Flow 4's release
//	posting is [Dr player_withdrawal_hold, Cr player_cash].
//
//	The withdrawal_requests row lock does NOT serialize them, and that
//	is the whole subtlety: a second withdrawal request is a DIFFERENT
//	request row on the same wallet, so the only thing the two
//	transactions share is the pair of projection rows.
//
// All three release transitions are covered as sub-cases, because they
// are three separately-written functions that happen to post the same
// shape - exactly the kind of duplication that lets a fix land on one and
// miss the others.
//
// Shown failing on HEAD with SQLSTATE 40P01 before the fix. Closed by
// ledger.Post's internal L3 pre-lock plus the deletion of
// lockCashBalanceForUpdate: §4.6 required no change at all to Reject/
// Fail/Cancel themselves, whose lockRequestForUpdate call is class L1 and
// already precedes L3.
func TestLockOrder_ConcurrentWithdrawalRequestAndRejection_NoDeadlock(t *testing.T) {
	cases := []struct {
		name string
		// prepare moves the seeded request into the state the release
		// transition requires, and returns the release closure.
		prepare func(t *testing.T, pool *db.Pool, f fixture, requestID uuid.UUID) func(ctx context.Context, tx pgx.Tx) error
		want    State
	}{
		{
			name: "Reject",
			prepare: func(t *testing.T, pool *db.Pool, f fixture, requestID uuid.UUID) func(context.Context, pgx.Tx) error {
				mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					return MoveToPendingReview(ctx, tx, requestID)
				})
				approverID := mustCreateApprover(t, pool, f.tenantID)
				return func(ctx context.Context, tx pgx.Tx) error {
					return Reject(ctx, tx, requestID, approverID, "failed_kyc_check", alwaysEligible)
				}
			},
			want: StateRejected,
		},
		{
			name: "Fail",
			prepare: func(t *testing.T, pool *db.Pool, f fixture, requestID uuid.UUID) func(context.Context, pgx.Tx) error {
				mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					return MoveToPendingReview(ctx, tx, requestID)
				})
				mustSetWithdrawalPolicy(t, pool, f.tenantID, "EUR", 1_000_000, 2, time.Now().Add(-time.Hour))
				approverID := mustCreateApprover(t, pool, f.tenantID)
				mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					_, err := Approve(ctx, tx, requestID, approverID, false, nil, alwaysEligible)
					return err
				})
				mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					return MarkSubmitted(ctx, tx, requestID, "mockpsp", "lockorder-payout-"+requestID.String()[:8])
				})
				return func(ctx context.Context, tx pgx.Tx) error {
					return Fail(ctx, tx, requestID, "psp_declined")
				}
			},
			want: StateFailed,
		},
		{
			name: "Cancel",
			prepare: func(t *testing.T, pool *db.Pool, f fixture, requestID uuid.UUID) func(context.Context, pgx.Tx) error {
				// Cancel is only legal from `requested`, so nothing to
				// prepare - which is also why it is the sub-case with the
				// least incidental serialization.
				return func(ctx context.Context, tx pgx.Tx) error {
					return Cancel(ctx, tx, requestID)
				}
			},
			want: StateCancelled,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			f := seedFixture(t, pool, 10_000)

			// The request that will be released during the race.
			released := mustRequestWithdrawal(t, pool, f, 1_000, "lockorder-release-"+tc.name)
			release := tc.prepare(t, pool, f, released.ID)

			blockerCash := loHoldProjectionRow(t, pool, f.tenantID, f.cashAccountID, "player_cash")
			blockerHold := loHoldProjectionRow(t, pool, f.tenantID, f.holdAccountID, "player_withdrawal_hold")

			// A: a NEW request - (player_cash, player_withdrawal_hold).
			startRequest := func() *loRacer {
				return loStartRacer(t, pool, f.tenantID, "RequestWithdrawal(cash,hold)", func(ctx context.Context, tx pgx.Tx) error {
					_, err := RequestWithdrawal(ctx, tx, RequestParams{
						TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID, WalletID: f.walletID,
						AssetCode: "EUR", Amount: 500, IdempotencyKey: "lockorder-new-" + tc.name,
					})
					return err
				})
			}
			// B: the release - (player_withdrawal_hold, player_cash).
			startRelease := func() *loRacer {
				return loStartRacer(t, pool, f.tenantID, tc.name+"(hold,cash)", release)
			}

			racerReq, racerRel, errReq, errRel := loRunABBA(t, pool, startRequest, startRelease,
				[]*loBlocker{blockerCash, blockerHold})
			loAssertNoDeadlock(t, "LOCK-1c (new request vs. "+tc.name+" of a different request)",
				map[string]error{racerReq.name: errReq, racerRel.name: errRel})
			if errReq != nil {
				t.Fatalf("the new withdrawal request must be accepted (the wallet is funded for both): %v", errReq)
			}
			if errRel != nil {
				t.Fatalf("the %s transition must succeed: %v", tc.name, errRel)
			}
			if got := loRequestState(t, pool, f.tenantID, released.ID); got != tc.want {
				t.Fatalf("expected the released request to be in state %q, got %q", tc.want, got)
			}

			// 10000 - 1000 held (released) - 500 newly held.
			if want, got := int64(10_000-500), cashBalance(t, pool, f); got != want {
				t.Fatalf("expected player_cash %d, got %d", want, got)
			}
			if want, got := int64(500), holdBalance(t, pool, f); got != want {
				t.Fatalf("expected player_withdrawal_hold %d (only the new request's hold), got %d", want, got)
			}
			loAssertBalanced(t, pool, f.tenantID)
			loAssertProjectionMatchesRebuild(t, pool, f.tenantID)
		})
	}
}
