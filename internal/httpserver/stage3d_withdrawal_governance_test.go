//go:build integration

// Stage 3D hardening: the withdrawal-governance adversarial tests the
// directive's item 3 (test items C, E, F, G - A, B, D, H are already
// covered by Stage 3C's stage3c_self_approval_test.go, which this stage
// left intact since the underlying self-approval/multi-account invariants
// did not change) and item 9 (direct-SQL DB-level enforcement). These
// exercise the FULL stack (HTTP -> service -> trigger) exactly as Stage
// 3C's own tests do, proving the NEW mandatory-Person-linkage/active-
// status/RBAC-separation invariants (docs/decisions/0024) the same way.
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// TestWithdrawalApprove_UnlinkedStaffAccountRejected is adversarial test
// item C: a finance staff account with NO Person linkage at all (the
// legacy/pre-remediation case Stage 3C left optional and Stage 3D now
// forbids) must be refused, even though it holds PermWithdrawalApprove
// and is not the withdrawal's beneficiary.
func TestWithdrawalApprove_UnlinkedStaffAccountRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	unlinked := mustCreateUnlinkedStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
	token := mustLoginStaff(t, srv, tenant.Slug, unlinked.Email, "a-decent-password-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)
	mustOpenReviewQueue(t, srv, token.AccessToken)

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", token.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for an unlinked staff account approving, got %d: %+v", resp.StatusCode, apiErr)
	}
	if count := countWithdrawalApprovals(t, pool, tenant.ID, wr.ID); count != 0 {
		t.Fatalf("expected zero withdrawal_approvals rows after a rejected unlinked-staff approval, got %d", count)
	}
}

// TestWithdrawalReject_UnlinkedStaffAccountRejected proves the same rule
// for reject - business decision #1's list is "approve, reject, or
// submit", not approve alone.
func TestWithdrawalReject_UnlinkedStaffAccountRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	unlinked := mustCreateUnlinkedStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
	token := mustLoginStaff(t, srv, tenant.Slug, unlinked.Email, "a-decent-password-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)
	mustOpenReviewQueue(t, srv, token.AccessToken)

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/reject", token.AccessToken, map[string]string{"reason_code": "test"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for an unlinked staff account rejecting, got %d: %+v", resp.StatusCode, apiErr)
	}
	if count := countWithdrawalApprovals(t, pool, tenant.ID, wr.ID); count != 0 {
		t.Fatalf("expected zero withdrawal_approvals rows after a rejected unlinked-staff rejection attempt, got %d", count)
	}
}

// TestWithdrawalSubmit_UnlinkedStaffAccountRejected proves the same rule
// for submit - the one transition with no withdrawal_approvals INSERT for
// the database trigger to catch, so this is the only enforcement point
// (newSubmitWithdrawalHandler's own explicit eligibility check).
func TestWithdrawalSubmit_UnlinkedStaffAccountRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	genuine := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
	genuineToken := mustLoginStaff(t, srv, tenant.Slug, genuine.Email, "a-decent-password-1")
	unlinked := mustCreateUnlinkedStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-2")
	unlinkedToken := mustLoginStaff(t, srv, tenant.Slug, unlinked.Email, "a-decent-password-2")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)

	// A single (below-threshold) approval by a genuinely eligible staff
	// member gets the request to `approved`, ready for submission.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'EUR', 1000000, 2, now() - interval '1 hour')`,
			tenant.ID,
		)
		return err
	}); err != nil {
		t.Fatalf("configure withdrawal policy: %v", err)
	}
	mustOpenReviewQueue(t, srv, genuineToken.AccessToken)
	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", genuineToken.AccessToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve withdrawal: status %d", resp.StatusCode)
	}

	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/submit", unlinkedToken.AccessToken, map[string]string{"payment_method": "card"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for an unlinked staff account submitting, got %d: %+v", resp.StatusCode, apiErr)
	}
}

// TestWithdrawalApprove_InactiveStaffAccountRejected is adversarial test
// item E: a linked, otherwise-eligible finance staff account whose
// status is changed to inactive AFTER its access token was issued must
// still be refused - proving eligibility is re-checked against live
// database state on every decision, never cached in or trusted from the
// JWT itself (this platform's tokens carry no staff-status claim at all;
// see internal/auth's session design).
func TestWithdrawalApprove_InactiveStaffAccountRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
	token := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "a-decent-password-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)
	mustOpenReviewQueue(t, srv, token.AccessToken)

	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, staff.ID)
		return err
	}); err != nil {
		t.Fatalf("suspend staff account: %v", err)
	}

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", token.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for a suspended staff account approving with an already-issued token, got %d: %+v", resp.StatusCode, apiErr)
	}
	if count := countWithdrawalApprovals(t, pool, tenant.ID, wr.ID); count != 0 {
		t.Fatalf("expected zero withdrawal_approvals rows after a rejected suspended-staff approval, got %d", count)
	}
}

// TestWithdrawalApprove_TenantAdminWithoutWithdrawalPermissionRejected is
// adversarial test item F, exercised against the specific role Stage 3C's
// specialist review identified as the actual privilege-escalation risk:
// tenant_admin holds PermStaffManage (a broad administrative role) but,
// as of Stage 3D's business decision #4/#5, no withdrawal-decision
// permission at all - it must be refused at the RBAC layer, never
// reaching internal/withdrawal or the database trigger.
func TestWithdrawalApprove_TenantAdminWithoutWithdrawalPermissionRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-nowd-pw-1")
	tenantAdminToken := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-nowd-pw-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)

	for _, tc := range []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, "/v1/admin/withdrawals/" + wr.ID.String() + "/approve", nil},
		{http.MethodPost, "/v1/admin/withdrawals/" + wr.ID.String() + "/reject", map[string]string{"reason_code": "x"}},
		{http.MethodPost, "/v1/admin/withdrawals/" + wr.ID.String() + "/submit", map[string]string{"payment_method": "card"}},
	} {
		resp := postJSON(t, srv, tc.path, tenantAdminToken.AccessToken, tc.body)
		if resp.StatusCode != http.StatusForbidden {
			var apiErr apierror.Error
			decodeBody(t, resp, &apiErr)
			t.Errorf("%s: expected 403 for tenant_admin (staff-management authority, no withdrawal authority), got %d: %+v", tc.path, resp.StatusCode, apiErr)
		}
		resp.Body.Close()
	}
	if count := countWithdrawalApprovals(t, pool, tenant.ID, wr.ID); count != 0 {
		t.Fatalf("expected zero withdrawal_approvals rows, got %d", count)
	}
}

// TestStaffPersonLink_RemediatesUnlinkedAccountThenCanApprove is the
// positive remediation path directive item 1 requires exist alongside
// the fail-closed rule: a legacy unlinked staff account is denied
// (proven above), an admin links it via the new person-link endpoint,
// and it can THEN approve normally.
func TestStaffPersonLink_RemediatesUnlinkedAccountThenCanApprove(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	unlinked := mustCreateUnlinkedStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
	token := mustLoginStaff(t, srv, tenant.Slug, unlinked.Email, "a-decent-password-1")

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-link-pw-1")
	tenantAdminToken := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-link-pw-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)
	mustOpenReviewQueue(t, srv, token.AccessToken)

	// Denied before remediation.
	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", token.AccessToken, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 before remediation, got %d", resp.StatusCode)
	}

	newPersonID := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, newPersonID)
		return err
	}); err != nil {
		t.Fatalf("seed remediation person: %v", err)
	}

	resp = postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff/"+unlinked.ID.String()+"/person-link",
		tenantAdminToken.AccessToken, map[string]string{"person_id": newPersonID.String()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 204 linking the staff account, got %d: %+v", resp.StatusCode, apiErr)
	}

	// Approved after remediation.
	resp2 := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", token.AccessToken, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, resp2, &apiErr)
		t.Fatalf("expected 200 approving after remediation, got %d: %+v", resp2.StatusCode, apiErr)
	}
}

// TestCreateStaff_TenantAdminCannotCreateFinanceRole is a regression test
// for the P1 finding this stage's own specialist review (security/
// ledger-finance/architect, independently) surfaced: removing withdrawal
// permissions from RoleTenantAdmin's OWN role definition
// (internal/auth/permission.go) does not, by itself, prevent a
// tenant_admin from using PermStaffManage to mint a BRAND NEW
// finance-role staff account - complete with a password and person_id of
// the tenant_admin's own choosing - and immediately log in as it,
// self-escalating from "can manage staff" to "can approve withdrawals".
// newCreateStaffHandler now refuses role="finance" from any tenant-
// scoped caller outright.
func TestCreateStaff_TenantAdminCannotCreateFinanceRole(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-mint-pw-1")
	token := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-mint-pw-1")

	resp := postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff", token.AccessToken, map[string]string{
		"email": "shadow-finance@example.com", "password": "a-decent-password-1", "role": "finance",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for tenant_admin creating a finance-role staff account, got %d: %+v", resp.StatusCode, apiErr)
	}

	// tenant_admin remains able to create every OTHER role for its own
	// tenant - the restriction is specific to the one role that actually
	// carries withdrawal authority, not a blanket lockout of staff
	// management.
	for _, role := range []string{"tenant_admin", "support", "compliance"} {
		resp := postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff", token.AccessToken, map[string]string{
			"email": role + "-ok@example.com", "password": "a-decent-password-1", "role": role,
		})
		if resp.StatusCode != http.StatusCreated {
			var apiErr apierror.Error
			decodeBody(t, resp, &apiErr)
			t.Errorf("expected 201 for tenant_admin creating a %q staff account, got %d: %+v", role, resp.StatusCode, apiErr)
		}
		resp.Body.Close()
	}
}

// TestCreateStaff_PlatformAdminCanCreateFinanceRole is the positive
// control for the restriction above: a platform-scoped caller (the ONE
// principal not reachable by any single tenant's own staff-management
// authority) can still provision a tenant's finance staff, and that
// account is immediately usable to approve.
func TestCreateStaff_PlatformAdminCanCreateFinanceRole(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)
	platformAdmin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "pa-mint-pw-1")
	platformToken := mustLoginStaff(t, srv, "", platformAdmin.Email, "pa-mint-pw-1")

	resp := postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff", platformToken.AccessToken, map[string]string{
		"email": "real-finance@example.com", "password": "a-decent-password-1", "role": "finance",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 201 for platform_admin creating a finance-role staff account, got %d: %+v", resp.StatusCode, apiErr)
	}
	var created staffResponse
	decodeBody(t, resp, &created)

	// Link it to a real Person (platform_admin holds PermStaffManage
	// too) and prove it can approve - the restriction targets WHO can
	// create the credential, not the finance role's own capability.
	personID := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	linkResp := postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff/"+created.ID+"/person-link",
		platformToken.AccessToken, map[string]string{"person_id": personID.String()})
	linkResp.Body.Close()
	if linkResp.StatusCode != http.StatusNoContent {
		t.Fatalf("expected 204 linking the platform_admin-created finance staff, got %d", linkResp.StatusCode)
	}

	financeToken := mustLoginStaff(t, srv, tenant.Slug, "real-finance@example.com", "a-decent-password-1")
	player := mustRegisterPlayer(t, srv, brand.Slug)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)
	mustOpenReviewQueue(t, srv, financeToken.AccessToken)

	approveResp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", financeToken.AccessToken, nil)
	defer approveResp.Body.Close()
	if approveResp.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, approveResp, &apiErr)
		t.Fatalf("expected 200 for the platform_admin-provisioned finance staff approving, got %d: %+v", approveResp.StatusCode, apiErr)
	}
}

// TestStaffPersonLink_ApproverCannotRelinkOwnAccountViaAPI is adversarial
// test item G exercised from the actual attacker's own perspective (a
// gap this stage's own qa specialist review flagged: the sibling test
// below only covers a THIRD PARTY - a tenant_admin - attempting to
// relink someone ELSE's account, never the linked staff member using
// their OWN token against their OWN record). RoleFinance holds no
// PermStaffManage, so a finance staff member - the only role that can
// actually approve withdrawals - cannot call the person-link endpoint at
// all, on their own account or anyone else's; this is denied at the RBAC
// layer before ever reaching identity.LinkStaffPersonID or migration
// 0034's append-only trigger.
func TestStaffPersonLink_ApproverCannotRelinkOwnAccountViaAPI(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1") // already linked
	token := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "a-decent-password-1")

	otherPersonID := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, otherPersonID)
		return err
	}); err != nil {
		t.Fatalf("seed other person: %v", err)
	}

	resp := postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff/"+staff.ID.String()+"/person-link",
		token.AccessToken, map[string]string{"person_id": otherPersonID.String()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 403 for a finance staff member attempting to relink their own account, got %d: %+v", resp.StatusCode, apiErr)
	}

	var currentPersonID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, staff.ID).Scan(&currentPersonID)
	}); err != nil {
		t.Fatalf("read back staff person_id: %v", err)
	}
	if currentPersonID != *staff.PersonID {
		t.Fatalf("expected person_id to remain %s, got %s", *staff.PersonID, currentPersonID)
	}
}

// TestStaffPersonLink_CannotRelinkOnceSet is adversarial test item G: an
// approver (or anyone with PermStaffManage) attempting to CHANGE an
// existing staff->Person link - e.g. to launder an approval trail by
// relinking a compromised or colluding account to a different person -
// must be refused, both via the admin API (identity.ErrAlreadyLinkedOrNotFound)
// and via a direct SQL UPDATE bypassing the API entirely (migration
// 0034's staff_users_person_id_append_only trigger).
func TestStaffPersonLink_CannotRelinkOnceSet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1") // already linked
	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-relink-pw-1")
	tenantAdminToken := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-relink-pw-1")

	otherPersonID := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, otherPersonID)
		return err
	}); err != nil {
		t.Fatalf("seed other person: %v", err)
	}

	// (1) The admin API refuses to relink an already-linked account.
	resp := postJSON(t, srv, "/v1/admin/tenants/"+tenant.ID.String()+"/staff/"+staff.ID.String()+"/person-link",
		tenantAdminToken.AccessToken, map[string]string{"person_id": otherPersonID.String()})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected 409 attempting to relink an already-linked staff account via the API, got %d: %+v", resp.StatusCode, apiErr)
	}

	// (2) A direct SQL UPDATE bypassing the API entirely is refused by the
	// database's own append-only trigger - the authoritative backstop.
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET person_id = $1 WHERE id = $2`, otherPersonID, staff.ID)
		return err
	})
	if err == nil {
		t.Fatal("expected the database to reject a direct UPDATE changing an already-set staff_users.person_id, got nil error")
	}

	// The original link must be completely unchanged.
	var currentPersonID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, staff.ID).Scan(&currentPersonID)
	}); err != nil {
		t.Fatalf("read back staff person_id: %v", err)
	}
	if currentPersonID != *staff.PersonID {
		t.Fatalf("expected person_id to remain %s, got %s - the append-only invariant was violated", *staff.PersonID, currentPersonID)
	}
}

// TestWithdrawalSubmit_PendingReviewBypassRejected is SM13
// (rv-prh-i1-payout-security.md/registry PAY-SEC-TESTS-1): the four-eyes
// bypass mutant weakens LockApprovedForSubmission's own state check to
// also accept `pending_review` (before any approval at all) - this
// mutant was confirmed, on a working database, to SURVIVE the full
// withdrawal/payments/httpserver suites. A withdrawal that has only been
// promoted to the review queue (mustOpenReviewQueue's own
// requested->pending_review side effect), with ZERO approvals, must be
// refused on submit exactly like any other non-approved state - proving
// the four-eyes gate is actually load-bearing at the one call site that
// matters (payments.ClaimForDispatch -> withdrawal.LockApprovedForSubmission),
// not merely asserted in review prose.
func TestWithdrawalSubmit_PendingReviewBypassRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "sm13-finance-pw-1")
	financeToken := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "sm13-finance-pw-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)

	// Promote requested -> pending_review, WITHOUT any approval at all -
	// the exact shape SM13's mutant would let through.
	mustOpenReviewQueue(t, srv, financeToken.AccessToken)

	var stateBefore string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, wr.ID).Scan(&stateBefore)
	}); err != nil {
		t.Fatalf("read state before: %v", err)
	}
	if stateBefore != "pending_review" {
		t.Fatalf("setup: expected pending_review after opening the review queue, got %q", stateBefore)
	}

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/submit", financeToken.AccessToken, map[string]string{"payment_method": "card"})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("SM13 regression: expected 409 submitting a pending_review (zero-approval) withdrawal, got %d: %+v", resp.StatusCode, apiErr)
	}

	var stateAfter string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, wr.ID).Scan(&stateAfter)
	}); err != nil {
		t.Fatalf("read state after: %v", err)
	}
	if stateAfter != "pending_review" {
		t.Fatalf("SM13 regression: expected the withdrawal to remain pending_review after a rejected submit, got %q", stateAfter)
	}
	if count := countRows(t, pool, tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, wr.ID); count != 0 {
		t.Fatalf("SM13 regression: expected zero payment attempts after a rejected pending_review submit, got %d", count)
	}
}
