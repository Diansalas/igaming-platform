//go:build integration

package withdrawal

// ADR 0119 S4b section 9.3: the withdrawal policy's require_step_up. Above the threshold a human approval needs the
// approver's own step-up assertion no older than 2 minutes on the database clock; a 3-minute-old step-up (fresh
// enough for the route's financial 10 minutes) is refused, a 1-minute-old one passes; an assertion for another
// principal, one in the future beyond the skew, the zero assertion and an automated decision never satisfy it; a
// refusal writes nothing.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/stepup"
)

func TestApprove_RequireStepUp_FreshnessMatrix(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000_000)
	mustSetWithdrawalPolicyStepUp(t, pool, f.tenantID, "EUR", 50_000, true)
	newPending := func(key string) WithdrawalRequest {
		wr := mustRequestWithdrawal(t, pool, f, 100_000, key)
		mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MoveToPendingReview(ctx, tx, wr.ID) })
		return wr
	}
	approvals := func(id uuid.UUID) int {
		var n int
		mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1`, id).Scan(&n)
		})
		return n
	}
	for _, c := range []struct {
		name      string
		age       time.Duration
		other     bool
		automated bool
		zero      bool
		dbClock   bool // age is measured from the approval transaction's own now() (the clock Approve reads)
		ok        bool
	}{
		{name: "exactly 120 s old on the database clock", age: 120 * time.Second, dbClock: true, ok: true},
		{name: "120 s + 1 microsecond old on the database clock", age: 120*time.Second + time.Microsecond, dbClock: true},
		{name: "one minute old", age: time.Minute, ok: true},
		{name: "three minutes old (route-fresh, policy-stale)", age: 3 * time.Minute},
		{name: "another principal's assertion", age: 0, other: true},
		{name: "in the future beyond the skew", age: -time.Minute},
		{name: "no assertion", zero: true},
		{name: "automated decision with a fresh assertion", age: 0, automated: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			wr := newPending("wd-s4b-" + uuid.NewString()[:8])
			approver := mustCreateApprover(t, pool, f.tenantID)
			subject := approver
			if c.other {
				subject = uuid.New()
			}
			a := stepup.Assertion{Subject: subject, SessionID: uuid.New(), MFAAt: time.Now().Add(-c.age), Class: stepup.Financial}
			if c.zero {
				a = stepup.Assertion{}
			}
			var elig ApproverEligibility = alwaysEligible
			if c.automated {
				elig = nil
			}
			err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if c.dbClock {
					// now() is the transaction's start time, constant inside it: Approve reads the same instant.
					var txNow time.Time
					if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&txNow); err != nil {
						return err
					}
					a.MFAAt = txNow.Add(-c.age)
				}
				_, err := Approve(ctx, tx, wr.ID, approver, c.automated, nil, elig, a)
				return err
			})
			if c.ok {
				if err != nil || approvals(wr.ID) != 1 {
					t.Fatalf("want the approval recorded: %v (approvals %d)", err, approvals(wr.ID))
				}
				return
			}
			if !errors.Is(err, ErrStepUpRequired) {
				t.Fatalf("want ErrStepUpRequired, got %v", err)
			}
			if approvals(wr.ID) != 0 {
				t.Fatal("a step-up refusal must write nothing")
			}
		})
	}
}
