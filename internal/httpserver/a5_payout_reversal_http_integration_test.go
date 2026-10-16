//go:build integration

package httpserver

// A5 / WITHDRAWAL-REVERSAL-1 over HTTP: POST /v1/admin/withdrawals/{id}/payout-reversal. Authorization is
// server-side only: the permission (support -> 403), the critical step-up class (an 11-minute-old
// step-up -> 403 step_up_required), the tenant scope (another tenant's finance staff -> 404), the
// decider from the token, never the body (an unknown body field -> 400). Two distinct finance Persons
// complete the quorum; the second call posts the compensating entry; a repeat is a 409.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

func TestA5HTTP_PayoutReversal_AuthorizationAndQuorum(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tn := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tn)
	mustRegisterCapability(t, pool, tn.ID, mock)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	fundWallet(t, pool, tn.ID, brand.ID, player.ID, "EUR", 100_000)
	wr := mustCreateWithdrawalRequest(t, pool, tn.ID, brand.ID, player.ID, walletIDFor(t, pool, tn.ID, player.ID, "EUR"), "EUR", 5000)
	ref := "a5http-" + wr.ID.String()[:8]
	if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := withdrawal.MoveToPendingReview(ctx, tx, wr.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, is_automated_approval, decision,
			threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, gen_random_uuid(), true, 'approve', 0, 5000)`, tn.ID, wr.ID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, wr.ID); err != nil {
			return err
		}
		if err := withdrawal.MarkSubmitted(ctx, tx, wr.ID, "mockpsp", "out-"+ref); err != nil {
			return err
		}
		return withdrawal.Complete(ctx, tx, wr.ID, "mockpsp", ref)
	}); err != nil {
		t.Fatalf("setup: walk to completed: %v", err)
	}

	f1 := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleFinance, "a-decent-password-1")
	f2 := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleFinance, "a-decent-password-1")
	support := mustCreateStaff(t, pool, tn.ID, identity.StaffRoleSupport, "a-decent-password-1")
	other := mustCreateTenant(t, pool)
	foreign := mustCreateStaff(t, pool, other.ID, identity.StaffRoleFinance, "a-decent-password-1")
	tok := func(s identity.StaffUser, tenant identity.Tenant, role auth.Role, age time.Duration) string {
		return authtest.LiveStaffTokenAged(t, pool, testStaffMFAKeys(), issuer, s.ID, tenant.ID, role, age)
	}
	path := "/v1/admin/withdrawals/" + wr.ID.String() + "/payout-reversal"
	body := map[string]any{"provider_id": "mockpsp", "original_provider_reference": ref, "return_reference": "ret-" + ref,
		"amount": "5000", "asset_code": "EUR", "evidence_ref_hash": strings.Repeat("7e", 32)}
	post := func(token string, b any) (int, map[string]any, apierror.Error) {
		resp := postJSON(t, srv, path, token, b)
		defer func() { _ = resp.Body.Close() }()
		raw, _ := io.ReadAll(resp.Body)
		var ok map[string]any
		var e apierror.Error
		_ = json.Unmarshal(raw, &ok)
		_ = json.Unmarshal(raw, &e)
		return resp.StatusCode, ok, e
	}
	reverseRows := func() int {
		var n int
		if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1 AND decision = 'reverse'`, wr.ID).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if st, _, e := post(tok(support, tn, auth.RoleSupport, time.Minute), body); st != http.StatusForbidden {
		t.Fatalf("support role: %d %+v", st, e)
	}
	if st, _, e := post(tok(f1, tn, auth.RoleFinance, 11*time.Minute), body); st != http.StatusForbidden || e.Code != apierror.CodeStepUpRequired {
		t.Fatalf("stale step-up on a critical route: %d %+v", st, e)
	}
	if st, _, e := post(tok(foreign, other, auth.RoleFinance, time.Minute), body); st != http.StatusNotFound {
		t.Fatalf("another tenant's finance staff: %d %+v", st, e)
	}
	withActor := map[string]any{}
	for k, v := range body {
		withActor[k] = v
	}
	withActor["approver_principal_id"] = f2.ID.String()
	if st, _, e := post(tok(f1, tn, auth.RoleFinance, time.Minute), withActor); st != http.StatusBadRequest {
		t.Fatalf("a decider in the body must be refused: %d %+v", st, e)
	}
	bad := map[string]any{}
	for k, v := range body {
		bad[k] = v
	}
	bad["amount"] = "50.00"
	if st, _, e := post(tok(f1, tn, auth.RoleFinance, time.Minute), bad); st != http.StatusBadRequest {
		t.Fatalf("a fractional amount: %d %+v", st, e)
	}
	// SR5-7: a platform_admin token is refused by the tenant scope (403) and records nothing.
	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "a-decent-password-1")
	adminTok := authtest.LiveStaffTokenAged(t, pool, testStaffMFAKeys(), issuer, admin.ID, uuid.Nil, auth.RolePlatformAdmin, time.Minute)
	if st, _, e := post(adminTok, body); st != http.StatusForbidden {
		t.Fatalf("platform_admin token: %d %+v", st, e)
	}
	// SR5-7: staff whose Person is the paid player's Person gets 403 and ONE durable Denied audit row.
	var playerPerson uuid.UUID
	if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, player.ID).Scan(&playerPerson)
	}); err != nil {
		t.Fatal(err)
	}
	benef := uuid.New()
	if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, 'x', 'finance', 'active', $4)`, benef, tn.ID, "a5-benef-"+benef.String()+"@test.invalid", playerPerson)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	benefTok := authtest.LiveStaffTokenAged(t, pool, testStaffMFAKeys(), issuer, benef, tn.ID, auth.RoleFinance, time.Minute)
	if st, _, e := post(benefTok, body); st != http.StatusForbidden {
		t.Fatalf("the beneficiary: %d %+v", st, e)
	}
	var denied int
	if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.payout_reversal.http'
			AND outcome = 'denied' AND actor_id = $2 AND target_id = $3`, tn.ID, benef, wr.ID.String()).Scan(&denied)
	}); err != nil || denied != 1 {
		t.Fatalf("the beneficiary refusal must write one Denied audit row: %d %v", denied, err)
	}
	// SR5-3: a return reference the platform already holds for the provider is a typed 409.
	held := "a5-held-" + uuid.NewString()[:8]
	if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		pid := "mockpsp"
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: tn.ID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "tombstone:mockpsp:" + held, ProviderID: &pid, ProviderTxID: &held, CorrelationID: uuid.New()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	heldBody := map[string]any{}
	for k, v := range body {
		heldBody[k] = v
	}
	heldBody["return_reference"] = held
	if st, _, e := post(tok(f1, tn, auth.RoleFinance, time.Minute), heldBody); st != http.StatusConflict || e.Code != apierror.CodeConflict {
		t.Fatalf("a held return reference: %d %+v", st, e)
	}
	if n := reverseRows(); n != 0 {
		t.Fatalf("refusals recorded %d decisions", n)
	}

	st, out, e := post(tok(f1, tn, auth.RoleFinance, time.Minute), body)
	if st != http.StatusOK || out["outcome"] != "decision_recorded" {
		t.Fatalf("first decision: %d %v %+v", st, out, e)
	}
	if st, _, e := post(tok(f1, tn, auth.RoleFinance, time.Minute), body); st != http.StatusConflict {
		t.Fatalf("the same decider again: %d %+v", st, e)
	}
	st, out, e = post(tok(f2, tn, auth.RoleFinance, time.Minute), body)
	if st != http.StatusOK || out["outcome"] != "reversed" || out["ledger_transaction_id"] == nil {
		t.Fatalf("second decision: %d %v %+v", st, out, e)
	}
	var state string
	var actions int
	if err := pool.WithTenant(context.Background(), tn.ID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, wr.ID).Scan(&state); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.payout_reversal.http'
			AND target_id = $2 AND ip_address IS NOT NULL AND outcome = 'success'`, tn.ID, wr.ID.String()).Scan(&actions)
	}); err != nil {
		t.Fatal(err)
	}
	if state != "reversed" || actions != 2 {
		t.Fatalf("state %s, http audit rows %d (want reversed, 2)", state, actions)
	}
}
