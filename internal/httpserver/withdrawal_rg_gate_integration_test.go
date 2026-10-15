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
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/risk"
)

// WD-RG-1 HTTP surface: POST /v1/me/withdrawals answers a gate denial with ONE generic 403 (no account-state or
// risk-rule oracle), a gate that cannot decide with a retryable 503, nothing written and the key not consumed; and a
// withdrawal is allowed during a self-exclusion (ADR 0115).
func TestRequestWithdrawalHandler_WDRG1_GateMapping(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100_000)
	mustApproveKYCForWithdrawal(t, pool, tenant.ID, brand.ID, player.ID)
	inst := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR").String()
	body := func(key string, amount int) map[string]any {
		return map[string]any{"asset_code": "EUR", "amount": amount, "idempotency_key": key, "payout_instrument_id": inst}
	}
	rows := func() int {
		return countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID)
	}

	// 1. a risk rule denies > 500: the generic 403, nothing written.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &tenant.ID, Operation: risk.OperationWithdrawal, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction,
			Threshold: 500, AssetCode: "EUR", RuleKind: risk.RuleHardLimit, Action: risk.ActionDeny,
			CreatedByActorType: "staff", CreatedByActorID: uuid.New(), Description: "wdrg1 http",
		})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, body("wdrg1-deny", 501))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("risk denial: status %d, want 403", resp.StatusCode)
	}
	if got := apiCode(t, resp); got != apierror.CodeForbidden {
		t.Fatalf("risk denial: code %s, want %s", got, apierror.CodeForbidden)
	}
	if rows() != 0 {
		t.Fatal("a gate denial writes no request")
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.denied_by_risk'`, tenant.ID); n != 1 {
		t.Fatalf("the denial audit must be committed, got %d rows", n)
	}
	// the key was not consumed: an allowed amount with the same key succeeds
	if resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, body("wdrg1-deny", 400)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("same key, allowed amount: status %d, want 201", resp.StatusCode)
	}

	// 2. a self-excluded player can still withdraw (created through the real self-exclusion route).
	if r := postJSON(t, srv, "/v1/me/rg/self-exclusion", player.Tokens.AccessToken, map[string]any{}); r.StatusCode != http.StatusCreated {
		t.Fatalf("self-exclusion: status %d", r.StatusCode)
	}
	if resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, body("wdrg1-excluded", 100)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("a withdrawal during a self-exclusion must be accepted, status %d", resp.StatusCode)
	}

	// 3. an inactive account is refused with the same generic 403.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, player.ID, identity.PlayerStatusSuspended)
	}); err != nil {
		t.Fatal(err)
	}
	before := rows()
	resp = postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, body("wdrg1-suspended", 100))
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("a suspended account: status %d, want 403 (or a 401 from the auth layer)", resp.StatusCode)
	}
	if rows() != before {
		t.Fatal("a refused request writes nothing")
	}
}

// A gate that cannot decide (here: two equally specific configurable rules for the same operation, which the
// risk engine reports as a configuration conflict error) is a retryable 503 and writes nothing.
func TestRequestWithdrawalHandler_WDRG1_GateCannotDecide_Is503(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newFinancialTestServer(t, pool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 100_000)
	mustApproveKYCForWithdrawal(t, pool, tenant.ID, brand.ID, player.ID)
	inst := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR").String()
	for i := 0; i < 2; i++ {
		if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			_, e := risk.CreateRule(ctx, tx, risk.CreateRuleParams{
				TenantID: &tenant.ID, Operation: risk.OperationWithdrawal, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction,
				Threshold: int64(1000 + i), AssetCode: "EUR", RuleKind: risk.RuleConfigurableLimit, Action: risk.ActionDeny,
				CreatedByActorType: "staff", CreatedByActorID: uuid.New(), Description: "conflict",
			})
			return e
		}); err != nil {
			t.Fatalf("create conflicting rule %d: %v", i, err)
		}
	}
	resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 100, "idempotency_key": "wdrg1-503", "payout_instrument_id": inst})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a gate that cannot decide: status %d, want 503", resp.StatusCode)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1`, tenant.ID); n != 0 {
		t.Fatalf("nothing may be written, got %d requests", n)
	}
}
