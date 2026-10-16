//go:build integration

// F4 (A6 SR6 I-2) end to end: the WD-RG-1 gate re-run at the staff "submit payout" call. A player account suspended
// after approval is refused with 409 conflict and a GENERIC message (no reason code in the body), the withdrawal stays
// `approved` with its hold, one denying dispatch decision, one denied staff audit and one alert commit, and there is no
// attempt and no ledger posting. Once the account is active again the same submit goes through.
package httpserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
)

func TestSubmitWithdrawalHandler_F4_DispatchGateDenial_Is409_AndParks(t *testing.T) {
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
		t.Fatalf("configure withdrawal policy: %v", err)
	}
	mustOpenReviewQueue(t, srv, financeToken.AccessToken)
	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/approve", financeToken.AccessToken, nil)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("approve withdrawal: status %d", resp.StatusCode)
	}
	setStatus := func(s identity.PlayerAccountStatus) {
		t.Helper()
		if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			return identity.SetPlayerAccountStatus(ctx, tx, player.ID, s)
		}); err != nil {
			t.Fatal(err)
		}
	}
	ledgerBefore := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)

	setStatus(identity.PlayerStatusSuspended)
	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID.String()+"/submit", financeToken.AccessToken, map[string]string{"payment_method": "bank_transfer"})
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("a dispatch-time gate denial must be 409, got %d: %s", resp.StatusCode, body)
	}
	for _, leak := range []string{"WD050", "player_account_not_active", "suspended"} {
		if strings.Contains(string(body), leak) {
			t.Fatalf("the 409 body must be generic, it carries %q: %s", leak, body)
		}
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE id = $1 AND state = 'approved'`, wr.ID); n != 1 {
		t.Fatal("the request must stay approved")
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1 AND NOT allowed AND gate_code = 'WD050'`, wr.ID); n != 1 {
		t.Fatalf("want one denying dispatch decision, got %d", n)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.submit.http' AND target_id = $2
		AND outcome = 'denied' AND actor_id = $3 AND metadata->>'gate_code' = 'WD050'`, tenant.ID, wr.ID.String(), finance.ID); n != 1 {
		t.Fatalf("want one staff-attributed denied submit audit, got %d", n)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM alerts WHERE kind = 'withdrawal.payout_gate_denied' AND discriminator = $1`, "withdrawal:"+wr.ID.String()+":gate:WD050"); n != 1 {
		t.Fatalf("want one withdrawal.payout_gate_denied alert, got %d", n)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, wr.ID); n != 0 {
		t.Fatalf("want no attempt, got %d", n)
	}
	if n := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID); n != ledgerBefore {
		t.Fatalf("want no ledger posting, %d -> %d", ledgerBefore, n)
	}

	setStatus(identity.PlayerStatusActive)
	mustSubmitWithdrawal(t, srv, financeToken.AccessToken, wr.ID.String())
}
