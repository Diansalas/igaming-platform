//go:build integration

package httpserver

// ADR 0119 S4b section 9.5 (ported from the ledger-finance review): withdrawal approve through the REAL
// withdrawal.Approve with the real approverEligibilityCheckLocked, in both orders. A suspension (ErrApproverInactive,
// the pre-existing 403) or a role change (db.ErrActorStaffNotLive) committed after lockRequestForUpdate and before the
// eligibility read is seen and nothing is written; with the approval holding the approver's row FOR SHARE first, the
// suspension waits (55P03 under a short lock_timeout), the approval stands, and the suspension completes afterwards.

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func TestStepUp_WithdrawalApprove_ReassertionBothOrders_RealApprove(t *testing.T) {
	for _, c := range []string{"suspend_first", "role_first", "approval_first"} {
		t.Run(c, func(t *testing.T) {
			pool, issuer := testEnv(t)
			orchestrator, mock := newMockOrchestrator()
			srv := newFinancialTestServer(t, pool, issuer, orchestrator)
			tn := mustCreateTenant(t, pool)
			brand := mustCreateBrand(t, pool, tn)
			mustRegisterCapability(t, pool, tn.ID, mock)
			player := mustRegisterPlayer(t, srv, brand.Slug)
			staff := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleFinance, "a-decent-password-1")
			login := mustLoginStaff(t, srv, tn.Slug, staff.Email, "a-decent-password-1")
			fundWallet(t, pool, tn.ID, brand.ID, player.ID, "EUR", 100000)
			wr := mustCreateWithdrawalRequest(t, pool, tn.ID, brand.ID, player.ID, walletIDFor(t, pool, tn.ID, player.ID, "EUR"), "EUR", 5000)
			mustOpenReviewQueue(t, srv, login.AccessToken)
			ctx := context.Background()
			change := func(sql, lockTimeout string) error {
				return pool.WithTenant(ctx, tn.ID, func(ctx context.Context, tx2 pgx.Tx) error {
					if lockTimeout != "" {
						if _, err := tx2.Exec(ctx, `SET LOCAL lock_timeout = '`+lockTimeout+`'`); err != nil {
							return err
						}
					}
					_, err := tx2.Exec(ctx, sql, staff.ID)
					return err
				})
			}
			var waitErr error
			err := pool.WithTenant(ctx, tn.ID, func(ctx context.Context, tx pgx.Tx) error {
				inner := approverEligibilityCheckLocked(ctx, tx, staff.ID, string(auth.RoleFinance), staff.StaffVersion)
				elig := func(id uuid.UUID) (bool, bool, error) {
					switch c {
					case "suspend_first":
						if err := change(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, ""); err != nil {
							t.Errorf("suspend: %v", err)
						}
					case "role_first":
						if err := change(`UPDATE staff_users SET role = 'support' WHERE id = $1`, ""); err != nil {
							t.Errorf("role: %v", err)
						}
					}
					l, a, err := inner(id)
					if c == "approval_first" {
						waitErr = change(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, "300ms")
					}
					return l, a, err
				}
				_, err := withdrawal.Approve(ctx, tx, wr.ID, staff.ID, false, nil, elig, auth.StepUpAssertion{})
				return err
			})
			switch c {
			case "suspend_first":
				if !errors.Is(err, withdrawal.ErrApproverInactive) {
					t.Fatalf("want ErrApproverInactive, got %v", err)
				}
			case "role_first":
				if !errors.Is(err, db.ErrActorStaffNotLive) {
					t.Fatalf("want ErrActorStaffNotLive, got %v", err)
				}
			case "approval_first":
				if err != nil {
					t.Fatalf("approval holding the row first must stand: %v", err)
				}
				if pgCode(waitErr) != "55P03" {
					t.Fatalf("the suspension must wait on the FOR SHARE: %v", waitErr)
				}
				if err := change(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, "2s"); err != nil {
					t.Fatalf("suspension after the commit: %v", err)
				}
			}
			want := 0
			if c == "approval_first" {
				want = 1
			}
			if n := countWithdrawalApprovals(t, pool, tn.ID, wr.ID); n != want {
				t.Fatalf("approvals %d, want %d", n, want)
			}
		})
	}
}
