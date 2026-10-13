//go:build integration

package httpserver

// ADR 0119 S4b sections 9.3 and 9.5 over HTTP: the withdrawal policy's require_step_up (2 minutes) behind the route's
// financial class (10 minutes), and the approver's in-transaction FOR SHARE re-assertion against the token's role.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestStepUp_WithdrawalPolicy_TwoMinutesBehindTheFinancialRoute(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tn := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tn)
	mustRegisterCapability(t, pool, tn.ID, mock)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	staff := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleFinance, "a-decent-password-1")
	login := mustLoginStaff(t, srv, tn.Slug, staff.Email, "a-decent-password-1")
	fundWallet(t, pool, tn.ID, brand.ID, player.ID, "EUR", 1_000_000)
	if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, require_step_up, effective_from)
			VALUES ($1, 'EUR', 1000, 2, true, now() - interval '1 hour')`, tn.ID)
		return err
	}); err != nil {
		t.Fatalf("policy: %v", err)
	}
	wr := mustCreateWithdrawalRequest(t, pool, tn.ID, brand.ID, player.ID, walletIDFor(t, pool, tn.ID, player.ID, "EUR"), "EUR", 5000)
	mustOpenReviewQueue(t, srv, login.AccessToken)
	path := "/v1/admin/withdrawals/" + wr.ID.String() + "/approve"
	post := func(tok string) (int, apierror.Error, http.Header) {
		resp := postJSON(t, srv, path, tok, nil)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		var e apierror.Error
		_ = json.Unmarshal(b, &e)
		return resp.StatusCode, e, resp.Header
	}
	// Three minutes: the route's financial window (10 min) passes, the policy (2 min) refuses: 403 max_age 120.
	three := authtest.LiveStaffTokenAged(t, pool, testStaffMFAKeys(), issuer, staff.ID, tn.ID, auth.RoleFinance, 3*time.Minute)
	status, e, h := post(three)
	if status != http.StatusForbidden || e.Code != apierror.CodeStepUpRequired || e.MaxAge == nil || *e.MaxAge != 120 ||
		!strings.Contains(h.Get("WWW-Authenticate"), `max_age="120"`) {
		t.Fatalf("3-minute step-up under require_step_up: %d %+v %q", status, e, h.Get("WWW-Authenticate"))
	}
	if n := countWithdrawalApprovals(t, pool, tn.ID, wr.ID); n != 0 {
		t.Fatalf("the refusal wrote %d approvals", n)
	}
	// Eleven minutes: the route itself refuses (financial, 600).
	eleven := authtest.LiveStaffTokenAged(t, pool, testStaffMFAKeys(), issuer, staff.ID, tn.ID, auth.RoleFinance, 11*time.Minute)
	if status, e, _ := post(eleven); status != http.StatusForbidden || e.MaxAge == nil || *e.MaxAge != 600 {
		t.Fatalf("11-minute step-up: %d %+v", status, e)
	}
	// One minute: recorded.
	one := authtest.LiveStaffTokenAged(t, pool, testStaffMFAKeys(), issuer, staff.ID, tn.ID, auth.RoleFinance, time.Minute)
	if status, e, _ := post(one); status != http.StatusOK {
		t.Fatalf("1-minute step-up: %d %+v", status, e)
	}
	if n := countWithdrawalApprovals(t, pool, tn.ID, wr.ID); n != 1 {
		t.Fatalf("want exactly one approval, got %d", n)
	}
}

// Section 9.5: the approver's own row is re-read FOR SHARE inside the approval transaction and compared with the
// token's role (behind the chain: liveness would refuse a role mismatch first; this pins the in-transaction rule for
// the R13 window).
func TestStepUp_WithdrawalApprove_ReassertsRoleInTransaction(t *testing.T) {
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
	ctx := suspendedFinanceContext(tn.ID, staff.ID) // a finance-role context for an ACTIVE finance row
	ctx.Role = string(auth.RoleRiskManager)         // ... claiming another role than the row holds
	rec := behindChain(t, newApproveWithdrawalHandler(financialTestDeps(pool, issuer, orchestrator)), http.MethodPost,
		"/v1/admin/withdrawals/"+wr.ID.String()+"/approve", map[string]string{"id": wr.ID.String()}, ctx, nil)
	if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "session_revoked") {
		t.Fatalf("a role mismatch at the in-transaction re-assertion: %d %s", rec.Code, rec.Body)
	}
	if n := countWithdrawalApprovals(t, pool, tn.ID, wr.ID); n != 0 {
		t.Fatalf("the refused approval wrote %d rows", n)
	}
}
