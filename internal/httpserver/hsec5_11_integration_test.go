//go:build integration

// H-SEC-5 / H-SEC-11: the three HTTP initiation routes (player deposit, player
// withdrawal request, staff payout submit) answer 409 TENANT_OR_BRAND_NOT_ACTIVE
// for a non-active tenant or brand, create nothing, and a retry with the same
// idempotency key after reactivation succeeds. The gate itself lives in the
// domain functions (payments / withdrawal packages); this file pins the HTTP
// mapping and the end-to-end no-side-effect property on the runtime role.
package httpserver

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func hsecSetTenantStatus(t *testing.T, pool *db.Pool, tenantID uuid.UUID, status string) {
	t.Helper()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = $2 WHERE id = $1`, tenantID, status)
		return err
	}); err != nil {
		t.Fatalf("set tenant status: %v", err)
	}
}

func hsecSetBrandStatus(t *testing.T, pool *db.Pool, tenantID, brandID uuid.UUID, status string) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET status = $2 WHERE id = $1`, brandID, status)
		return err
	}); err != nil {
		t.Fatalf("set brand status: %v", err)
	}
}

type hsecHTTPCase struct {
	name       string
	set, reset func(t *testing.T, pool *db.Pool, tenantID, brandID uuid.UUID)
}

var hsecHTTPCases = []hsecHTTPCase{
	{"tenant_suspended",
		func(t *testing.T, p *db.Pool, tn, _ uuid.UUID) { hsecSetTenantStatus(t, p, tn, "suspended") },
		func(t *testing.T, p *db.Pool, tn, _ uuid.UUID) { hsecSetTenantStatus(t, p, tn, "active") }},
	{"tenant_closed",
		func(t *testing.T, p *db.Pool, tn, _ uuid.UUID) { hsecSetTenantStatus(t, p, tn, "closed") },
		func(t *testing.T, p *db.Pool, tn, _ uuid.UUID) { hsecSetTenantStatus(t, p, tn, "active") }},
	{"brand_suspended",
		func(t *testing.T, p *db.Pool, tn, b uuid.UUID) { hsecSetBrandStatus(t, p, tn, b, "suspended") },
		func(t *testing.T, p *db.Pool, tn, b uuid.UUID) { hsecSetBrandStatus(t, p, tn, b, "active") }},
	{"brand_closed",
		func(t *testing.T, p *db.Pool, tn, b uuid.UUID) { hsecSetBrandStatus(t, p, tn, b, "closed") },
		func(t *testing.T, p *db.Pool, tn, b uuid.UUID) { hsecSetBrandStatus(t, p, tn, b, "active") }},
}

func assertNotActiveRefusal(t *testing.T, resp *http.Response, what string) {
	t.Helper()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("%s: want 409, got %d", what, resp.StatusCode)
	}
	if e := decodeAPIError(t, resp); e.Code != apierror.CodeTenantOrBrandNotActive {
		t.Fatalf("%s: want code %s, got %s", what, apierror.CodeTenantOrBrandNotActive, e.Code)
	}
}

func TestHSEC5_HTTP_DepositInitiation_NonActiveRefused_RetryAfterReactivation(t *testing.T) {
	for _, c := range hsecHTTPCases {
		t.Run(c.name, func(t *testing.T) {
			pool, issuer := testEnv(t)
			orchestrator, mock := newMockOrchestrator()
			srv := newFinancialTestServer(t, pool, issuer, orchestrator)
			tenant := mustCreateTenant(t, pool)
			brand := mustCreateBrand(t, pool, tenant)
			mustRegisterCapability(t, pool, tenant.ID, mock)
			player := mustRegisterPlayer(t, srv, brand.Slug)
			mustActivatePlayer(t, pool, tenant.ID, player.ID)

			// Control: active/active.
			resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
				"asset_code": "EUR", "amount": 15000, "payment_method": "card", "idempotency_key": "ctl-" + uuid.NewString(),
			})
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("control: want 201, got %d", resp.StatusCode)
			}
			resp.Body.Close()
			intents := countRows(t, pool, tenant.ID, `SELECT count(*) FROM deposit_intents WHERE tenant_id = $1`, tenant.ID)
			attempts := countRows(t, pool, tenant.ID, `SELECT count(*) FROM payment_attempts WHERE tenant_id = $1`, tenant.ID)
			ledger := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)

			c.set(t, pool, tenant.ID, brand.ID)
			key := "retry-" + uuid.NewString()
			body := map[string]any{"asset_code": "EUR", "amount": 15000, "payment_method": "card", "idempotency_key": key}
			assertNotActiveRefusal(t, postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, body), "deposit")
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM deposit_intents WHERE tenant_id = $1`, tenant.ID); n != intents {
				t.Fatalf("deposit intent created while refused: %d -> %d", intents, n)
			}
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM payment_attempts WHERE tenant_id = $1`, tenant.ID); n != attempts {
				t.Fatalf("attempt created while refused: %d -> %d", attempts, n)
			}
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID); n != ledger {
				t.Fatalf("ledger row created while refused: %d -> %d", ledger, n)
			}

			c.reset(t, pool, tenant.ID, brand.ID)
			resp = postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, body)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("retry after reactivation: want 201, got %d", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestHSEC11_HTTP_WithdrawalRequest_NonActiveRefused_RetryAfterReactivation(t *testing.T) {
	for _, c := range hsecHTTPCases {
		t.Run(c.name, func(t *testing.T) {
			pool, issuer := testEnv(t)
			orchestrator, _ := newMockOrchestrator()
			srv := newFinancialTestServer(t, pool, issuer, orchestrator)
			tenant := mustCreateTenant(t, pool)
			brand := mustCreateBrand(t, pool, tenant)
			player := mustRegisterPlayer(t, srv, brand.Slug)
			mustActivatePlayer(t, pool, tenant.ID, player.ID)
			fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 1_000_000)
			mustApproveKYCForWithdrawal(t, pool, tenant.ID, brand.ID, player.ID)

			resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, map[string]any{
				"asset_code": "EUR", "amount": 4000, "idempotency_key": "ctl-" + uuid.NewString(),
			})
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("control: want 201, got %d", resp.StatusCode)
			}
			resp.Body.Close()
			reqs := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID)
			ledger := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)

			c.set(t, pool, tenant.ID, brand.ID)
			key := "retry-" + uuid.NewString()
			body := map[string]any{"asset_code": "EUR", "amount": 4000, "idempotency_key": key}
			assertNotActiveRefusal(t, postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, body), "withdrawal request")
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID); n != reqs {
				t.Fatalf("withdrawal request created while refused: %d -> %d", reqs, n)
			}
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID); n != ledger {
				t.Fatalf("hold/ledger row created while refused: %d -> %d", ledger, n)
			}

			c.reset(t, pool, tenant.ID, brand.ID)
			resp = postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, body)
			if resp.StatusCode != http.StatusCreated {
				t.Fatalf("retry after reactivation: want 201, got %d", resp.StatusCode)
			}
			resp.Body.Close()
		})
	}
}

func TestHSEC11_HTTP_StaffSubmit_NonActiveRefused_StaysApproved_ThenSubmits(t *testing.T) {
	for _, c := range hsecHTTPCases {
		t.Run(c.name, func(t *testing.T) {
			pool, issuer := testEnv(t)
			orchestrator, mock := newMockOrchestrator()
			srv := newFinancialTestServer(t, pool, issuer, orchestrator)
			tenant := mustCreateTenant(t, pool)
			brand := mustCreateBrand(t, pool, tenant)
			mustRegisterCapability(t, pool, tenant.ID, mock)
			player := mustRegisterPlayer(t, srv, brand.Slug)
			finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "a-decent-password-1")
			financeToken := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "a-decent-password-1")
			fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 1_000_000)
			wr := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, player.ID, walletIDFor(t, pool, tenant.ID, player.ID, "EUR"), "EUR", 5000)
			if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx,
					`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
					 VALUES ($1, 'EUR', 1000000, 2, now() - interval '1 hour')`, tenant.ID)
				return err
			}); err != nil {
				t.Fatalf("policy: %v", err)
			}
			mustOpenReviewQueue(t, srv, financeToken.AccessToken)
			resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", financeToken.AccessToken, nil)
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("approve: %d", resp.StatusCode)
			}
			ledger := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)

			c.set(t, pool, tenant.ID, brand.ID)
			assertNotActiveRefusal(t, postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/submit", financeToken.AccessToken, map[string]string{"payment_method": "card"}), "submit")
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'approved'`, wr.ID); n != 1 {
				t.Fatal("the request must stay approved")
			}
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, wr.ID); n != 0 {
				t.Fatalf("attempt created while refused: %d", n)
			}
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID); n != ledger {
				t.Fatalf("ledger row created while refused: %d -> %d", ledger, n)
			}

			c.reset(t, pool, tenant.ID, brand.ID)
			resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/submit", financeToken.AccessToken, map[string]string{"payment_method": "card"})
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("submit after reactivation: want 200, got %d", resp.StatusCode)
			}
			if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, wr.ID); n != 1 {
				t.Fatalf("exactly one attempt after the successful submit, got %d", n)
			}
		})
	}
}
