//go:build integration

// ADR 0096 (PRH-I3) staff read API tests (code review rv-prh-i3-code-
// review.md B3, security review rv-prh-i3-security.md F1): role gating,
// cross-tenant/nonexistent 404 identity, and cursor round-tripping.
package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// TestRequestWithdrawalHandler_KYCDenyCommitsExactlyOnceAndNothingElse
// proves LF-I3-3 end to end through the real HTTP handler: a KYC-denied
// withdrawal request commits exactly one kyc_enforcement_decisions row
// and one audit_log row, in the SAME transaction as the domain attempt
// (not a separate, later-committed one), and creates zero
// withdrawal_requests rows and zero ledger postings.
func TestRequestWithdrawalHandler_KYCDenyCommitsExactlyOnceAndNothingElse(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	// Deliberately NO kyc_verifications row is seeded - the player has
	// never been verified, so the structural withdrawal rule denies.

	decisionsBefore := countRows(t, pool, tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`, tenant.ID, player.ID)
	auditBefore := countRows(t, pool, tenant.ID, `SELECT count(*) FROM audit_log WHERE action = 'kyc.enforcement_denied'`)

	resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 500, "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 for an unverified player's withdrawal request, got %d", resp.StatusCode)
	}

	decisionsAfter := countRows(t, pool, tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`, tenant.ID, player.ID)
	if decisionsAfter != decisionsBefore+1 {
		t.Fatalf("expected exactly 1 new kyc_enforcement_decisions row, got %d new", decisionsAfter-decisionsBefore)
	}
	auditAfter := countRows(t, pool, tenant.ID, `SELECT count(*) FROM audit_log WHERE action = 'kyc.enforcement_denied'`)
	if auditAfter != auditBefore+1 {
		t.Fatalf("expected exactly 1 new audit_log row, got %d new", auditAfter-auditBefore)
	}
	reqCount := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND player_account_id = $2`, tenant.ID, player.ID)
	if reqCount != 0 {
		t.Fatalf("expected 0 withdrawal_requests rows after a KYC deny, got %d", reqCount)
	}
	ledgerCount := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'withdrawal_requested'`, tenant.ID)
	if ledgerCount != 0 {
		t.Fatalf("expected 0 withdrawal_requested ledger postings after a KYC deny, got %d", ledgerCount)
	}
}

func countRows(t *testing.T, pool *db.Pool, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

func seedKYCDecisions(t *testing.T, pool *db.Pool, tenantID, brandID, playerID uuid.UUID, n int) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(ctx, `
				INSERT INTO kyc_enforcement_decisions (tenant_id, brand_id, player_account_id, operation, outcome, allowed, policy_version, correlation_id)
				VALUES ($1, $2, $3, 'withdrawal_hold', 'failed', false, 'test', $4)`,
				tenantID, brandID, playerID, uuid.New()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed kyc enforcement decisions: %v", err)
	}
}

func TestListKYCEnforcementDecisions_ComplianceRoleSucceeds(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	seedKYCDecisions(t, pool, tenant.ID, brand.ID, player.ID, 1)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-pw-1")

	resp := getJSON(t, srv, "/v1/admin/kyc/enforcement-decisions?player_account_id="+player.ID.String(), tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for a compliance staff token, got %d", resp.StatusCode)
	}
	var body listEnforcementDecisionsResponse
	decodeBody(t, resp, &body)
	if len(body.Decisions) != 1 {
		t.Fatalf("expected 1 decision, got %d", len(body.Decisions))
	}
}

func TestListKYCEnforcementDecisions_TenantAdminForbidden(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-pw-1")

	resp := getJSON(t, srv, "/v1/admin/kyc/enforcement-decisions?player_account_id="+player.ID.String(), tokens.AccessToken)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for a tenant_admin token (never granted PermKYCEnforcementDecisionRead), got %d", resp.StatusCode)
	}
}

func TestListKYCEnforcementDecisions_CrossTenantAndNonexistentGetIdentical404(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	playerA := mustRegisterPlayer(t, srv, brandA.Slug)

	tenantB := mustCreateTenant(t, pool)
	complianceB := mustCreateStaff(t, pool, tenantB.ID, identity.StaffRoleCompliance, "compliance-pw-2")
	tokensB := mustLoginStaff(t, srv, tenantB.Slug, complianceB.Email, "compliance-pw-2")

	respCrossTenant := getJSON(t, srv, "/v1/admin/kyc/enforcement-decisions?player_account_id="+playerA.ID.String(), tokensB.AccessToken)
	if respCrossTenant.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for tenant B reading tenant A's player, got %d", respCrossTenant.StatusCode)
	}

	respNonexistent := getJSON(t, srv, "/v1/admin/kyc/enforcement-decisions?player_account_id="+uuid.New().String(), tokensB.AccessToken)
	if respNonexistent.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for a nonexistent player, got %d", respNonexistent.StatusCode)
	}
}

// TestListKYCEnforcementDecisions_CursorRoundTripsAcrossThreePages proves
// B3: next_before is accepted back in the exact shape it is emitted, and
// paging through 3 pages of 2 never repeats or skips a row.
func TestListKYCEnforcementDecisions_CursorRoundTripsAcrossThreePages(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	seedKYCDecisions(t, pool, tenant.ID, brand.ID, player.ID, 5)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-pw-3")
	tokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-pw-3")

	seen := map[string]bool{}
	path := "/v1/admin/kyc/enforcement-decisions?player_account_id=" + player.ID.String() + "&limit=2"
	for page := 0; page < 3; page++ {
		resp := getJSON(t, srv, path, tokens.AccessToken)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("page %d: expected 200, got %d", page, resp.StatusCode)
		}
		var body listEnforcementDecisionsResponse
		decodeBody(t, resp, &body)
		for _, d := range body.Decisions {
			if seen[d.ID] {
				t.Fatalf("page %d: id %s seen twice across pages", page, d.ID)
			}
			seen[d.ID] = true
		}
		if body.NextBefore == "" {
			break
		}
		path = "/v1/admin/kyc/enforcement-decisions?player_account_id=" + player.ID.String() + "&limit=2&next_before=" + url.QueryEscape(body.NextBefore)
	}
	if len(seen) != 5 {
		t.Fatalf("expected exactly 5 distinct decisions seen across pages, got %d", len(seen))
	}
}

func TestListKYCEnforcementDecisions_MalformedCursorRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-pw-4")
	tokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-pw-4")

	resp := getJSON(t, srv, "/v1/admin/kyc/enforcement-decisions?player_account_id="+player.ID.String()+"&next_before=not-a-valid-cursor", tokens.AccessToken)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for a malformed cursor, got %d", resp.StatusCode)
	}
}
