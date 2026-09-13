//go:build integration

// Stage 3C hardening: the withdrawal self-approval adversarial tests
// (directive items 1 and 8.A-8.D). Closes withdrawal-state-machine.md §5
// bypass #2, which Stage 3B left documented-but-open because staff_users
// had no relationship to persons. Migration 0029 added
// staff_users.person_id plus an authoritative BEFORE INSERT trigger on
// withdrawal_approvals; newApproveWithdrawalHandler now passes a real
// BeneficiaryCheck. These tests exercise the FULL stack (HTTP -> service
// -> trigger) exactly as a real attacker would reach it, not just the
// trigger or the closure in isolation.
package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// mustGetPlayerPersonID resolves a player_account's own person_id -
// needed to deliberately construct a staff account that IS (test A/B) or
// is NOT (test C) the same human as the withdrawing player.
func mustGetPlayerPersonID(t *testing.T, pool *db.Pool, tenantID, playerAccountID uuid.UUID) uuid.UUID {
	t.Helper()
	var personID uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
		if err != nil {
			return err
		}
		personID = account.PersonID
		return nil
	})
	if err != nil {
		t.Fatalf("get player person id: %v", err)
	}
	return personID
}

// mustCreateStaffWithPerson is mustCreateStaff (identity_flow_integration_test.go)
// plus an explicit, possibly-nil person_id - needed to construct the
// dual-role (staff who is also a player) fixture these tests are about.
// Deliberately calls identity.CreateStaffUser directly rather than
// extending the shared mustCreateStaff helper's signature, since every
// OTHER caller of that helper has no legitimate reason to ever supply a
// person id.
func mustCreateStaffWithPerson(t *testing.T, pool *db.Pool, tenantID uuid.UUID, role identity.StaffRole, password string, personID *uuid.UUID) identity.StaffUser {
	t.Helper()
	suffix := uuid.NewString()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("failed to hash password: %v", err)
	}
	var staff identity.StaffUser
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		staff, err = identity.CreateStaffUser(ctx, tx, tenantID, "dualrole-staff-"+suffix+"@test.com", hash, role, personID)
		return err
	})
	if err != nil {
		t.Fatalf("create staff with person: %v", err)
	}
	return staff
}

// mustOpenReviewQueue triggers the requested->pending_review promotion
// (see newListPendingWithdrawalsHandler's own doc comment - opening the
// staff review queue is what actually performs that transition this
// stage, since no automated risk engine exists yet) by calling it as
// financeToken. Every self-approval test needs the withdrawal to have
// left `requested` before Approve will accept it.
func mustOpenReviewQueue(t *testing.T, srv *httptest.Server, financeToken string) {
	t.Helper()
	resp := getJSON(t, srv, "/v1/admin/withdrawals", financeToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("failed to open review queue: status %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// TestWithdrawalApprove_SelfApprovalRejected is adversarial test 8.A: the
// withdrawing player's own person, approving through their OWN staff
// account (the simplest, most direct form of the bypass), must fail -
// end to end, over real HTTP, against real Postgres.
func TestWithdrawalApprove_SelfApprovalRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	personID := mustGetPlayerPersonID(t, pool, tenant.ID, player.ID)

	// The dual-role fixture: a finance staff account whose person_id is
	// the SAME person as the withdrawing player.
	dualRoleStaff := mustCreateStaffWithPerson(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1", &personID)
	staffToken := mustLoginStaff(t, srv, tenant.Slug, dualRoleStaff.Email, "a-decent-password-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)

	mustOpenReviewQueue(t, srv, staffToken.AccessToken)

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", staffToken.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected self-approval to be rejected, got 200 OK")
	}
	var apiErr apierror.Error
	decodeBody(t, resp, &apiErr)
	if resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 409 or 403 for self-approval, got %d: %+v", resp.StatusCode, apiErr)
	}

	if count := countWithdrawalApprovals(t, pool, tenant.ID, wr.ID); count != 0 {
		t.Fatalf("expected zero withdrawal_approvals rows after a rejected self-approval, got %d", count)
	}
}

// TestWithdrawalApprove_SamePersonAlternateStaffIdentityRejected is
// adversarial test 8.B: the same human, holding TWO distinct staff
// accounts (distinct staff_users.id, distinct email/login), still cannot
// approve their own withdrawal through the second account. This is what
// actually proves the rule keys on PERSON, not on staff_users.id - a
// naive "distinct approver_principal_id" check alone would let this
// through, since bypass #1 (distinct staff PRINCIPAL) and bypass #2
// (distinct PERSON) are genuinely different invariants.
func TestWithdrawalApprove_SamePersonAlternateStaffIdentityRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	personID := mustGetPlayerPersonID(t, pool, tenant.ID, player.ID)

	// TWO distinct staff accounts, same person_id - simulating one human
	// who was (deliberately or by an onboarding mistake) issued a second
	// staff login.
	_ = mustCreateStaffWithPerson(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1", &personID)
	alternateStaff := mustCreateStaffWithPerson(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-2", &personID)
	alternateToken := mustLoginStaff(t, srv, tenant.Slug, alternateStaff.Email, "a-decent-password-2")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)

	mustOpenReviewQueue(t, srv, alternateToken.AccessToken)

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", alternateToken.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Fatal("expected the alternate staff identity of the same person to be rejected, got 200 OK")
	}
	if resp.StatusCode != http.StatusConflict && resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 409 or 403, got %d", resp.StatusCode)
	}
	if count := countWithdrawalApprovals(t, pool, tenant.ID, wr.ID); count != 0 {
		t.Fatalf("expected zero withdrawal_approvals rows, got %d", count)
	}
}

// TestWithdrawalApprove_DifferentPersonSucceeds is adversarial test 8.C:
// proves the rejections above are about the self-approval condition
// specifically, not a blanket failure of the approve path - a genuinely
// different person (no person_id at all, the ordinary case for almost
// every staff account) can approve normally.
func TestWithdrawalApprove_DifferentPersonSucceeds(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	financeStaff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
	financeToken := mustLoginStaff(t, srv, tenant.Slug, financeStaff.Email, "a-decent-password-1")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)

	mustOpenReviewQueue(t, srv, financeToken.AccessToken)

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", financeToken.AccessToken, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var apiErr apierror.Error
		decodeBody(t, resp, &apiErr)
		t.Fatalf("expected a genuinely different person's approval to succeed, got %d: %+v", resp.StatusCode, apiErr)
	}
	if count := countWithdrawalApprovals(t, pool, tenant.ID, wr.ID); count != 1 {
		t.Fatalf("expected exactly 1 withdrawal_approvals row, got %d", count)
	}
}

// TestWithdrawalApprovalsDenySelfApproval_DirectSQLBypassAttempt proves
// the enforcement is authoritative at the database itself (the Stage 3C
// directive's own phrase: "not merely audit-detectable"), not merely a
// property of going through newApproveWithdrawalHandler - a raw SQL
// INSERT into withdrawal_approvals, bypassing internal/withdrawal.Approve
// and this package entirely, must still be rejected by migration 0029's
// trigger.
func TestWithdrawalApprovalsDenySelfApproval_DirectSQLBypassAttempt(t *testing.T) {
	pool, _ := testEnv(t)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	personID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed person: %v", err)
	}

	var playerAccountID, walletID uuid.UUID
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		playerAccountID = uuid.New()
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerAccountID, tenant.ID, brand.ID, personID, playerAccountID.String()+"@example.com",
		); err != nil {
			return err
		}
		walletID = uuid.New()
		_, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'EUR')`,
			walletID, tenant.ID, brand.ID, playerAccountID,
		)
		return err
	})
	if err != nil {
		t.Fatalf("seed player/wallet: %v", err)
	}

	dualRoleStaff := mustCreateStaffWithPerson(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1", &personID)

	fundWallet(t, pool, tenant.ID, brand.ID, playerAccountID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, playerAccountID, walletID, "EUR", 5000)

	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_approvals
				(id, tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision,
				 threshold_amount_at_decision, request_amount_at_decision)
			 VALUES ($1, $2, $3, $4, false, 'approve', $5, $6)`,
			uuid.New(), tenant.ID, wr.ID, dualRoleStaff.ID, 1_000_000, wr.Amount,
		)
		return err
	})
	if err == nil {
		t.Fatal("expected the database trigger to reject a direct self-approval INSERT, got nil error")
	}
}

// walletIDFor resolves a player's own wallet id for assetCode - a thin
// wrapper so these tests don't need to thread wallet.Wallet structs
// around; fundWallet already created the row via wallet.GetOrCreate.
func walletIDFor(t *testing.T, pool *db.Pool, tenantID, playerAccountID uuid.UUID, assetCode string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM wallets WHERE player_account_id = $1 AND asset_code = $2`, playerAccountID, assetCode).Scan(&id)
	})
	if err != nil {
		t.Fatalf("resolve wallet id: %v", err)
	}
	return id
}

// TestWithdrawalApprove_MultiAccountIdentityBypassRejected closes a P1
// finding from this stage's own specialist review pass (code-reviewer):
// counting DISTINCT approver_principal_id alone let one human, holding
// two staff logins linked to the SAME person, supply BOTH of the two
// required approvals for a THIRD PARTY's withdrawal. Migration 0029's
// self-approval trigger never fires here - neither login is the
// withdrawing player's own beneficiary - so this is a distinct bypass
// from A/B/C above ("multi-account identity bypass" as its own named
// attack in the Stage 3C directive, not the self-approval case).
// internal/withdrawal.Approve's readiness count now collapses two staff
// logins sharing a person_id into one via
// COALESCE(staff_users.person_id, approver_principal_id), closing it.
func TestWithdrawalApprove_MultiAccountIdentityBypassRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	player := mustRegisterPlayer(t, srv, brand.Slug)

	mallory := uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, mallory)
		return err
	}); err != nil {
		t.Fatalf("seed colluding person: %v", err)
	}
	staffA := mustCreateStaffWithPerson(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1", &mallory)
	staffB := mustCreateStaffWithPerson(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-2", &mallory)
	tokenA := mustLoginStaff(t, srv, tenant.Slug, staffA.Email, "a-decent-password-1")
	tokenB := mustLoginStaff(t, srv, tenant.Slug, staffB.Email, "a-decent-password-2")

	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100000)
	wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)

	mustOpenReviewQueue(t, srv, tokenA.AccessToken)

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", tokenA.AccessToken, nil)
	var first approveWithdrawalResponse
	decodeBody(t, resp, &first)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("first approval (staffA): status %d", resp.StatusCode)
	}
	if first.Approved {
		t.Fatal("expected a single approval not to satisfy the two-approval requirement")
	}

	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", tokenB.AccessToken, nil)
	var second approveWithdrawalResponse
	decodeBody(t, resp, &second)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second approval (staffB, same person as staffA): status %d", resp.StatusCode)
	}
	if second.Approved {
		t.Fatal("expected a second staff login resolving to the SAME person as the first to NOT satisfy the two-approval requirement - that is one human approving twice, not two humans")
	}
	if count := countWithdrawalApprovals(t, pool, tenant.ID, wr.ID); count != 2 {
		t.Fatalf("expected exactly 2 recorded approval rows (both decisions ARE recorded, just not both counted toward readiness), got %d", count)
	}

	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		got, err := withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return err
		}
		if got.State != withdrawal.StatePendingReview {
			t.Fatalf("expected state to remain pending_review after two same-person approvals, got %q", got.State)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify still pending: %v", err)
	}

	// A genuinely different, third person's approval DOES complete it -
	// proving the fix rejects only the same-person collusion attempt,
	// not approvals in general.
	genuineFinance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-3")
	genuineToken := mustLoginStaff(t, srv, tenant.Slug, genuineFinance.Email, "a-decent-password-3")
	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", genuineToken.AccessToken, nil)
	var third approveWithdrawalResponse
	decodeBody(t, resp, &third)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("third approval (genuinely different person): status %d", resp.StatusCode)
	}
	if !third.Approved {
		t.Fatal("expected a genuinely different second person's approval to complete the two-approval requirement")
	}
}
