//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// PAY-SEC-LAUNCH-1 / S-L1 (security review of the payout dispatch path, probe HP3): a finance staff account linked to
// the SAME Person as the withdrawing player must not submit or resolve that player's payout. Approve has had the
// BeneficiaryCheck since Stage 3C; submit and /resolve checked linked-and-active only, so the beneficiary controlled the
// dispatch timing and the payout rail. Both routes now refuse with 403 before anything is claimed, polled or written.

func sl1BeneficiaryStaff(t *testing.T, f stage3CResolutionFixture) (identity.StaffUser, tokenPairResponse) {
	t.Helper()
	var personID uuid.UUID
	if err := f.pool.WithTenant(context.Background(), f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pa.person_id FROM withdrawal_requests wr JOIN player_accounts pa ON pa.id = wr.player_account_id WHERE wr.id = $1`,
			f.withdrawalID).Scan(&personID)
	}); err != nil {
		t.Fatalf("read the withdrawing player's Person: %v", err)
	}
	staff := mustCreateStaffWithPerson(t, f.pool, f.tenant.ID, identity.StaffRoleFinance, "a-decent-password-9", &personID)
	return staff, mustLoginStaff(t, f.srv, f.tenant.Slug, staff.Email, "a-decent-password-9")
}

func sl1Status(t *testing.T, resp *http.Response) (int, apierror.Error) {
	t.Helper()
	defer resp.Body.Close()
	var apiErr apierror.Error
	if resp.StatusCode != http.StatusOK {
		decodeBody(t, resp, &apiErr)
	}
	return resp.StatusCode, apiErr
}

func TestSL1_Submit_StaffLinkedToThePlayersOwnPersonIsForbidden(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	_, bToken := sl1BeneficiaryStaff(t, f)

	attemptsBefore := countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, f.withdrawalID)
	decisionsBefore := countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND operation = 'withdrawal_payout'`, f.tenant.ID)
	code, apiErr := sl1Status(t, postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/submit", bToken.AccessToken, map[string]string{"payment_method": "bank_transfer"}))
	if code != http.StatusForbidden {
		t.Fatalf("a staff account linked to the withdrawing player's own Person must get 403 on submit, got %d: %+v", code, apiErr)
	}
	if n := countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'approved'`, f.withdrawalID); n != 1 {
		t.Fatal("the refused submit must leave the withdrawal approved")
	}
	if n := countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, f.withdrawalID); n != attemptsBefore {
		t.Fatalf("the refused submit must create no attempt (%d -> %d)", attemptsBefore, n)
	}
	if n := countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND operation = 'withdrawal_payout'`, f.tenant.ID); n != decisionsBefore {
		t.Fatalf("the refused submit must evaluate nothing downstream (%d -> %d decision rows)", decisionsBefore, n)
	}
	// Control: an unrelated eligible finance account is NOT refused, so the 403 above is the beneficiary rule and not a
	// broken fixture.
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)
	if submitted.State != "submitted" {
		t.Fatalf("control submit: state %q", submitted.State)
	}
}

func TestSL1_Resolve_StaffLinkedToThePlayersOwnPersonIsForbidden_NoPollNoRescheduleNoStateChange(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)
	_, bToken := sl1BeneficiaryStaff(t, f)

	var pollsBefore int
	if err := f.pool.WithTenant(context.Background(), f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT poll_count FROM payment_attempts WHERE withdrawal_request_id = $1`, f.withdrawalID).Scan(&pollsBefore)
	}); err != nil {
		t.Fatal(err)
	}
	f.mock.Resolve(submitted.ProviderReference, payments.OutcomeSucceeded, "", false)

	code, apiErr := sl1Status(t, postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/resolve", bToken.AccessToken, nil))
	if code != http.StatusForbidden {
		t.Fatalf("a staff account linked to the withdrawing player's own Person must get 403 on /resolve, got %d: %+v", code, apiErr)
	}
	var pollsAfter int
	if err := f.pool.WithTenant(context.Background(), f.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT poll_count FROM payment_attempts WHERE withdrawal_request_id = $1`, f.withdrawalID).Scan(&pollsAfter)
	}); err != nil {
		t.Fatal(err)
	}
	if pollsAfter != pollsBefore {
		t.Fatalf("the refused /resolve must not poll or reschedule the attempt (poll_count %d -> %d)", pollsBefore, pollsAfter)
	}
	if n := countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'submitted'`, f.withdrawalID); n != 1 {
		t.Fatal("the refused /resolve must leave the withdrawal submitted")
	}
	// Control: the unrelated finance account resolves it.
	if resolved := mustResolve(t, f); resolved.State != "completed" {
		t.Fatalf("control resolve: state %q", resolved.State)
	}
}

// Another staff member whose Person differs from the player's is unaffected (the rule keys on the Person, not on
// being "a second finance account").
func TestSL1_Submit_StaffWithADifferentPersonIsNotRefused(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	other := mustCreateStaff(t, f.pool, f.tenant.ID, identity.StaffRoleFinance, "a-decent-password-8")
	otherToken := mustLoginStaff(t, f.srv, f.tenant.Slug, other.Email, "a-decent-password-8")
	if submitted := mustSubmitWithdrawal(t, f.srv, otherToken.AccessToken, f.withdrawalID); submitted.State != "submitted" {
		t.Fatalf("state %q", submitted.State)
	}
}

// SR11-4: a refused beneficiary action is durable (Denied audit row), not log-only; likewise the existing self-approval refusal.
func TestSL1_RefusalsAreAuditedDenied(t *testing.T) {
	f := setupStage3CResolutionFixture(t, 10000)
	staff, token := sl1BeneficiaryStaff(t, f)
	count := func(action string) int {
		return countRows(t, f.pool, f.tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND actor_id = $3 AND target_id = $4 AND outcome = 'denied' AND metadata->>'reason' = 'staff_is_beneficiary'`,
			f.tenant.ID, action, staff.ID, f.withdrawalID)
	}
	if code, _ := sl1Status(t, postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/submit", token.AccessToken, map[string]string{"payment_method": "bank_transfer"})); code != http.StatusForbidden {
		t.Fatalf("submit: %d", code)
	}
	if n := count("withdrawal.submit.http"); n != 1 {
		t.Fatalf("want one denied submit audit row, got %d", n)
	}
	submitted := mustSubmitWithdrawal(t, f.srv, f.financeToken.AccessToken, f.withdrawalID)
	_ = submitted
	if code, _ := sl1Status(t, postJSON(t, f.srv, "/v1/admin/withdrawals/"+f.withdrawalID+"/resolve", token.AccessToken, nil)); code != http.StatusForbidden {
		t.Fatalf("resolve: %d", code)
	}
	if n := count("withdrawal.resolve.http"); n != 1 {
		t.Fatalf("want one denied resolve audit row, got %d", n)
	}
}
