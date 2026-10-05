//go:build integration

// PRH-2 K3 (ADR 0101 24.9 / R-9, T-3, T-12, T-14) over real HTTP, on a private
// scratch database migrated to the latest on-disk migration (the cgAPI harness).
// M1 (a disputed DEPOSIT) is used throughout because it needs no payout
// fixture; the M2 behaviours are covered through the service in
// internal/payments. Policy values are synthetic and test-only.
package httpserver

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

var frClosedTokens = []string{
	payments.TokenForceResolveDisabled, payments.TokenForceResolveNotPermitted, payments.TokenForceResolvePreconditionFail,
	payments.TokenForceResolveReasonNotResolved, payments.TokenForceResolveConflict, payments.TokenForceResolveExpired,
	payments.TokenForceResolveNotFound,
}

type frWorld struct {
	*maWorld
	orch *payments.Orchestrator
	// refusals collects every refusal (>= 400) body seen, for the leakage check.
	refusals [][]byte
}

func (w *frWorld) note(status int, body []byte) {
	if status >= 400 {
		w.refusals = append(w.refusals, body)
	}
}

// noLeakage asserts (non-vacuously) that no collected refusal body carries database
// text or a SQLSTATE-like marker.
func (w *frWorld) noLeakage(t *testing.T, atLeast int) {
	t.Helper()
	if len(w.refusals) < atLeast {
		t.Fatalf("only %d refusal bodies were collected, want >= %d: the leakage check would be vacuous", len(w.refusals), atLeast)
	}
	for _, b := range w.refusals {
		for _, bad := range []string{"SQLSTATE", "violates", "MR0", "MR1", "CG0", "payment_manual_resolutions:", "ERROR:"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("a refusal body leaked database text (%q): %s", bad, b)
			}
		}
	}
}

// deniedRows returns the metadata JSON of the denied audit rows visible in a tenant.
func (w *frWorld) deniedRows(tenantID uuid.UUID) []string {
	var out []string
	if err := w.a.pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata::text FROM audit_log WHERE action = 'payment.manual_resolution_denied'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m string
			if err := rows.Scan(&m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	}); err != nil {
		w.a.t.Fatal(err)
	}
	return out
}

func newFRWorld(t *testing.T, withPolicy bool) *frWorld {
	w := &frWorld{maWorld: newMAWorld(t)}
	orch, mock := newMockOrchestrator()
	w.orch = orch
	mustRegisterCapability(t, w.a.pool, w.tenant, mock)
	for _, f := range []uuid.UUID{w.f1, w.f2} {
		w.grant(w.tenant, w.ta, f, false, capability.CapabilityPaymentForceResolveRequest)
		w.grant(w.tenant, w.ta, f, false, capability.CapabilityPaymentForceResolveApprove)
	}
	w.grant(w.tenant, w.pa, w.acting, true, capability.CapabilityPaymentForceResolveRequest)
	w.grant(w.tenant, w.pa, w.acting, true, capability.CapabilityPaymentForceResolveApprove)
	if withPolicy {
		w.authorPolicy()
	}
	return w
}

func (w *frWorld) authorPolicy() {
	a := w.a
	res := a.do("POST", "/v1/admin/financial-policy-changes", a.token(w.pa, uuid.Nil, auth.RolePlatformAdmin), map[string]any{
		"change_kind": "policy", "operation_kind": "payment_force_resolve", "level": "platform", "asset_code": "USD", "base_required_approvals": 1,
	})
	if res.status != http.StatusCreated {
		a.t.Fatalf("propose force-resolve policy: %d %s", res.status, res.body)
	}
	var ch struct {
		ID          string `json:"id"`
		ContentHash string `json:"content_hash"`
	}
	res.decode(a.t, &ch)
	if res := a.do("POST", "/v1/admin/financial-policy-changes/"+ch.ID+"/approve", a.token(w.pb, uuid.Nil, auth.RolePlatformAdmin),
		map[string]any{"content_hash": ch.ContentHash, "reason_code": "x"}); res.status != http.StatusOK {
		a.t.Fatalf("approve force-resolve policy: %d %s", res.status, res.body)
	}
}

// disputedDeposit drives a real deposit to `disputed` (a callback whose amount
// contradicts the attempt) and returns the attempt id.
func (w *frWorld) disputedDeposit() uuid.UUID {
	t := w.a.t
	t.Helper()
	res, err := w.orch.InitiateDepositAttempt(context.Background(), w.a.pool, payments.KYCEnforcementDepositGate{}, payments.MockCredentialResolver{}, payments.InitiateDepositParams{
		Scope:     payments.DepositScope{TenantID: w.tenant, BrandID: w.brand, PlayerAccountID: w.player, WalletID: w.wallet},
		AssetCode: "USD", Amount: 5000, PaymentMethod: "card", IdempotencyKey: uuid.NewString(),
	})
	if err != nil || res.Attempt.ProviderReference == nil {
		t.Fatalf("initiate deposit: %v (ref %v)", err, res.Attempt.ProviderReference)
	}
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := payments.ApplyReceiptEvidence(ctx, tx, w.orch, w.tenant, "mock", payments.ReceiptEvidence{
			EventType: "deposit", ProviderReference: *res.Attempt.ProviderReference,
			Outcome: payments.OutcomeSucceeded, Amount: 4999, AssetCode: "USD",
		})
		return err
	}); err != nil {
		t.Fatalf("mismatching receipt: %v", err)
	}
	return res.Attempt.ID
}

func (w *frWorld) base() string {
	return "/v1/admin/tenants/" + w.tenant.String() + "/payment-force-resolutions"
}

func (w *frWorld) request(actor, attempt uuid.UUID, extra map[string]any) (int, []byte, frDTO) {
	body := map[string]any{
		"attempt_id": attempt.String(), "kind": "m1_deposit_evidence", "finding_code": "awaiting_psp_refund",
		"reason_code": "http-k3", "note": "http k3 test",
	}
	for k, v := range extra {
		body[k] = v
	}
	res := w.a.do("POST", w.base(), w.tok(actor), body)
	var d frDTO
	if res.status == http.StatusCreated {
		res.decode(w.a.t, &d)
	}
	return res.status, res.body, d
}

type frDTO struct {
	ID          string  `json:"id"`
	TenantID    string  `json:"tenant_id"`
	AttemptID   string  `json:"attempt_id"`
	PayloadHash string  `json:"payload_hash"`
	State       string  `json:"state"`
	LedgerTx    *string `json:"ledger_transaction_id"`
}

func frHasClosedToken(body []byte) bool {
	for _, tk := range frClosedTokens {
		if strings.Contains(string(body), `"`+tk+`"`) {
			return true
		}
	}
	return false
}

func (w *frWorld) deniedAudits() int {
	var n int
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'payment.manual_resolution_denied'`).Scan(&n)
	}); err != nil {
		w.a.t.Fatal(err)
	}
	return n
}

// T-12: role x session family x operation. Every row states why.
func TestForceResolutionAPI_T12_Matrix(t *testing.T) {
	w := newFRWorld(t, true)
	for _, c := range []struct {
		name  string
		actor uuid.UUID
		want  int
	}{
		{"finance with request grant (tenant family)", w.f1, http.StatusCreated},
		{"platform_admin with G-P2 grant (acting family)", w.acting, http.StatusCreated},
		{"finance without grant (database refuses)", w.fNoGrant, http.StatusForbidden},
		{"platform_admin without grant (CG020)", w.actingNoGrant, http.StatusForbidden},
		{"tenant_admin (no static permission)", w.ta, http.StatusForbidden},
		{"support (no static permission)", w.supp, http.StatusForbidden},
		{"compliance (read only)", w.comp, http.StatusForbidden},
		{"another tenant's finance (canActOnTenant)", w.otherFin, http.StatusForbidden},
	} {
		got, body, _ := w.request(c.actor, w.disputedDeposit(), nil)
		w.note(got, body)
		if got != c.want {
			t.Errorf("request / %s: want %d got %d %s", c.name, c.want, got, body)
		}
		if got == http.StatusForbidden && c.actor != w.ta && c.actor != w.supp && c.actor != w.comp && !frHasClosedToken(body) {
			// the static-permission refusals use the generic envelope; every
			// service-level refusal must carry a closed token.
			t.Errorf("request / %s: 403 body is not a closed token: %s", c.name, body)
		}
	}
	for _, c := range []struct {
		name  string
		actor uuid.UUID
		want  int
	}{
		{"finance reads", w.f1, http.StatusOK},
		{"compliance reads", w.comp, http.StatusOK},
		{"tenant_admin has no force-resolve permission", w.ta, http.StatusForbidden},
		{"support has no force-resolve permission", w.supp, http.StatusForbidden},
		{"another tenant's finance", w.otherFin, http.StatusForbidden},
	} {
		if res := w.a.do("GET", w.base(), w.tok(c.actor), nil); res.status != c.want {
			t.Errorf("list / %s: want %d got %d %s", c.name, c.want, res.status, res.body)
		}
	}
	// Z30/Z31: every service-level 403 class and the foreign-tenant refusal wrote a
	// denied audit row carrying the closed token (and, for guard refusals, the SQLSTATE
	// CODE only).
	if rows := w.deniedRows(w.other); len(rows) == 0 || !strings.Contains(strings.Join(rows, "\n"), `"denied_token": "`+payments.TokenForceResolveNotPermitted+`"`) {
		t.Errorf("the foreign-tenant refusal wrote no denied audit row with the closed token: %v", rows)
	}
	own := strings.Join(w.deniedRows(w.tenant), "\n")
	if !strings.Contains(own, `"sqlstate": "MR003"`) {
		t.Errorf("the no-grant refusal's denied audit row lacks sqlstate MR003: %s", own)
	}
	if strings.Contains(own, "grant") || strings.Contains(own, "ERROR") {
		t.Errorf("a denied audit row carries message text: %s", own)
	}
	// No token at all, and an anonymous caller.
	if res := w.a.do("GET", w.base(), "", nil); res.status != http.StatusUnauthorized {
		t.Errorf("anonymous list: want 401 got %d", res.status)
	}
	// Approve/reject need the approve permission: compliance, support, tenant_admin are refused.
	_, _, d := w.request(w.f1, w.disputedDeposit(), nil)
	for _, actor := range []uuid.UUID{w.comp, w.supp, w.ta, w.otherFin} {
		res := w.a.do("POST", w.base()+"/"+d.ID+"/approve", w.tok(actor), map[string]any{"payload_hash": d.PayloadHash, "reason_code": "x"})
		if res.status != http.StatusForbidden {
			t.Errorf("approve by a role without the permission: want 403 got %d %s", res.status, res.body)
		}
	}
	// Cancel needs the REQUEST permission: only the requester may cancel.
	if res := w.a.do("POST", w.base()+"/"+d.ID+"/cancel", w.tok(w.f2), nil); res.status == http.StatusOK {
		t.Errorf("a non-requester cancelled the request: %s", res.body)
	}
	if res := w.a.do("POST", w.base()+"/"+d.ID+"/cancel", w.tok(w.f1), nil); res.status != http.StatusOK {
		t.Errorf("the requester must be able to cancel: %d %s", res.status, res.body)
	}
}

// T-14: the closed token set, the ignored body tenant, the denial audit, the
// whole request -> approve -> executed path, and the refused re-approval.
func TestForceResolutionAPI_T14_FlowTokensAndAudit(t *testing.T) {
	w := newFRWorld(t, false)
	attempt := w.disputedDeposit()

	// Disabled until the platform authors a baseline.
	before := w.deniedAudits()
	st, body, _ := w.request(w.f1, attempt, nil)
	w.note(st, body)
	if st != http.StatusConflict || !strings.Contains(string(body), payments.TokenForceResolveDisabled) {
		t.Fatalf("no policy: want 409 %s, got %d %s", payments.TokenForceResolveDisabled, st, body)
	}
	if w.deniedAudits() <= before {
		t.Error("a refused request must write a payment.manual_resolution_denied audit row")
	}
	w.authorPolicy()

	// A body tenant_id naming another tenant is ignored: the path wins.
	st, body, d := w.request(w.f1, attempt, map[string]any{"tenant_id": w.other.String()})
	if st != http.StatusCreated || d.TenantID != w.tenant.String() {
		t.Fatalf("request: %d %s (tenant %q)", st, body, d.TenantID)
	}

	// A closed-set refusal for each of: unknown attempt, self-approval, stale hash.
	st, body, _ = w.request(w.f1, uuid.New(), nil)
	w.note(st, body)
	if st < 400 || !frHasClosedToken(body) && st != http.StatusBadRequest {
		t.Errorf("unknown attempt: want a closed-token refusal, got %d %s", st, body)
	}
	res := w.a.do("POST", w.base()+"/"+d.ID+"/approve", w.tok(w.f1), map[string]any{"payload_hash": d.PayloadHash, "reason_code": "x"})
	w.note(res.status, res.body)
	if res.status != http.StatusForbidden && res.status != http.StatusConflict || !frHasClosedToken(res.body) {
		t.Errorf("self-approval: want a closed-token refusal, got %d %s", res.status, res.body)
	}
	res = w.a.do("POST", w.base()+"/"+d.ID+"/approve", w.tok(w.f2), map[string]any{"payload_hash": strings.Repeat("0", 64), "reason_code": "x"})
	w.note(res.status, res.body)
	if res.status < 400 || res.status == http.StatusInternalServerError || !frHasClosedToken(res.body) {
		t.Errorf("stale payload hash: want a closed-token refusal, got %d %s", res.status, res.body)
	}
	nf := w.a.do("GET", w.base()+"/"+uuid.NewString(), w.tok(w.f1), nil)
	w.note(nf.status, nf.body)
	if res := nf; res.status != http.StatusNotFound || !strings.Contains(string(res.body), payments.TokenForceResolveNotFound) {
		t.Errorf("unknown resolution: want 404 %s, got %d %s", payments.TokenForceResolveNotFound, res.status, res.body)
	}

	// Approve executes M1: the resolution is executed, the attempt stays
	// disputed, nothing is linked to the ledger.
	res = w.a.do("POST", w.base()+"/"+d.ID+"/approve", w.tok(w.f2), map[string]any{"payload_hash": d.PayloadHash, "reason_code": "x"})
	if res.status != http.StatusOK {
		t.Fatalf("approve: %d %s", res.status, res.body)
	}
	var dec struct {
		Resolution frDTO `json:"resolution"`
		Executed   bool  `json:"executed"`
	}
	res.decode(t, &dec)
	got := dec.Resolution
	if got.State != "executed" || got.LedgerTx != nil || !dec.Executed {
		t.Fatalf("executed M1: state=%q ledger link=%v", got.State, got.LedgerTx)
	}
	var attemptState string
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM payment_attempts WHERE id = $1`, attempt).Scan(&attemptState)
	}); err != nil || attemptState != "disputed" {
		t.Fatalf("M1 must leave the attempt disputed: %q %v", attemptState, err)
	}
	// Re-approving an executed resolution is a conflict.
	res = w.a.do("POST", w.base()+"/"+d.ID+"/approve", w.tok(w.f2), map[string]any{"payload_hash": d.PayloadHash, "reason_code": "x"})
	w.note(res.status, res.body)
	if res.status != http.StatusConflict || !frHasClosedToken(res.body) {
		t.Errorf("re-approval: want 409 closed token, got %d %s", res.status, res.body)
	}
	// Reject: a second request is rejected by one decision.
	attempt2 := w.disputedDeposit()
	_, _, d2 := w.request(w.f1, attempt2, nil)
	res = w.a.do("POST", w.base()+"/"+d2.ID+"/reject", w.tok(w.f2), map[string]any{"payload_hash": d2.PayloadHash, "reason_code": "no"})
	if res.status != http.StatusOK {
		t.Fatalf("reject: %d %s", res.status, res.body)
	}
	res.decode(t, &dec)
	if dec.Resolution.State != "rejected" {
		t.Errorf("reject state = %q", dec.Resolution.State)
	}
	// No refusal body ever carries SQL text or a SQLSTATE (non-vacuous: every refusal
	// collected above is checked, and there must be several).
	w.noLeakage(t, 6)
}
