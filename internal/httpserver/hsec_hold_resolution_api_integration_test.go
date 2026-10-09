//go:build integration

// HSEC-APPROVED-HOLD-RELEASE-1 (ADR 0111 section 6, migration 0124) over real HTTP, on a
// private scratch database migrated to the latest on-disk migration (the cgAPI harness).
// The release authority is migration 0124's; these tests pin the route layer: the static
// permission gate (platform_admin ONLY - every tenant role is denied), the acting-session
// requirement, the closed token set, the ignored body tenant, the denied audit rows and a
// real end-to-end release.
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
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

var hrClosedTokens = []string{
	payments.TokenHoldResolveDisabled, payments.TokenHoldResolveNotPermitted, payments.TokenHoldResolvePrecondition,
	payments.TokenHoldResolveConflict, payments.TokenHoldResolveExpired, payments.TokenHoldResolveNotFound,
}

type hrWorld struct {
	*maWorld
	p1, p2, p3, noGrant uuid.UUID
	refusals            [][]byte
}

func newHRWorld(t *testing.T, withPolicy bool) *hrWorld {
	w := &hrWorld{maWorld: newMAWorld(t)}
	w.p1, w.p2, w.p3, w.noGrant = w.a.staff(uuid.Nil, "platform_admin"), w.a.staff(uuid.Nil, "platform_admin"),
		w.a.staff(uuid.Nil, "platform_admin"), w.a.staff(uuid.Nil, "platform_admin")
	w.grant(w.tenant, w.pa, w.p1, true, capability.CapabilityWithdrawalHoldResolutionRequest)
	w.grant(w.tenant, w.pa, w.p1, true, capability.CapabilityWithdrawalHoldResolutionApprove)
	w.grant(w.tenant, w.pa, w.p2, true, capability.CapabilityWithdrawalHoldResolutionApprove)
	w.grant(w.tenant, w.pa, w.p3, true, capability.CapabilityWithdrawalHoldResolutionApprove)
	// noGrant holds an acting session (a K2 grant) but no hold-resolution capability.
	w.grant(w.tenant, w.pa, w.noGrant, true, capability.CapabilityLedgerAdjustmentInitiate)
	if withPolicy {
		w.authorPolicy(2)
	}
	return w
}

func (w *hrWorld) authorPolicy(base int) {
	a := w.a
	res := a.do("POST", "/v1/admin/financial-policy-changes", a.token(w.pa, uuid.Nil, auth.RolePlatformAdmin), map[string]any{
		"change_kind": "policy", "operation_kind": "withdrawal_hold_resolution", "level": "platform", "asset_code": "USD", "base_required_approvals": base,
	})
	if res.status != http.StatusCreated {
		a.t.Fatalf("propose hold-resolution policy: %d %s", res.status, res.body)
	}
	var ch struct {
		ID          string `json:"id"`
		ContentHash string `json:"content_hash"`
	}
	res.decode(a.t, &ch)
	if res := a.do("POST", "/v1/admin/financial-policy-changes/"+ch.ID+"/approve", a.token(w.pb, uuid.Nil, auth.RolePlatformAdmin),
		map[string]any{"content_hash": ch.ContentHash, "reason_code": "x"}); res.status != http.StatusOK {
		a.t.Fatalf("approve hold-resolution policy: %d %s", res.status, res.body)
	}
}

func (w *hrWorld) note(status int, body []byte) {
	if status >= 400 {
		w.refusals = append(w.refusals, body)
	}
}

func (w *hrWorld) base() string {
	return "/v1/admin/tenants/" + w.tenant.String() + "/withdrawal-hold-resolutions"
}

func (w *hrWorld) tokenFor(id uuid.UUID) string {
	switch id {
	case w.p1, w.p2, w.p3, w.noGrant:
		return w.a.token(id, uuid.Nil, auth.RolePlatformAdmin)
	}
	return w.tok(id)
}

// approvedWithdrawal funds the wallet, requests a withdrawal and moves it to `approved`
// (a single state UPDATE: the withdrawal approval workflow itself is not under test), then
// suspends the tenant.
func (w *hrWorld) approvedWithdrawalOnSuspendedTenant(amount int64) uuid.UUID {
	t := w.a.t
	t.Helper()
	ctx := context.Background()
	var person uuid.UUID
	var wrID uuid.UUID
	instrument := pitest.Bind(t, w.a.pool, w.tenant, w.player, "USD") // B13-B: a withdrawal binds a verified payout instrument
	if err := w.a.pool.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, w.player).Scan(&person); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			VALUES ($1, $2, $3, $4, $5, 'approved', 'mock')`, uuid.New(), w.tenant, w.brand, w.player, person); err != nil {
			return err
		}
		cash, err := ledger.GetOrCreateAccount(ctx, tx, w.tenant, &w.wallet, ledger.AccountPlayerCash, "USD")
		if err != nil {
			return err
		}
		clearing, err := ledger.GetOrCreateAccount(ctx, tx, w.tenant, nil, ledger.AccountPSPClearing, "USD")
		if err != nil {
			return err
		}
		pid, ptx := "seed", "seed-"+w.wallet.String()
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.tenant, TransactionType: ledger.TxDeposit, IdempotencyKey: ptx,
			ProviderID: &pid, ProviderTxID: &ptx, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{{LedgerAccountID: clearing, Direction: ledger.Debit, Amount: 100000}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 100000}}}); err != nil {
			return err
		}
		wr, err := withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{TenantID: w.tenant, BrandID: w.brand, PlayerAccountID: w.player, PersonID: person,
			WalletID: w.wallet, AssetCode: "USD", Amount: amount, IdempotencyKey: uuid.NewString(),
			PayoutInstrumentID: instrument, Destinations: pitest.Shared()})
		if err != nil {
			return err
		}
		wrID = wr.ID
		_, err = tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, wr.ID)
		return err
	}); err != nil {
		t.Fatalf("approved withdrawal: %v", err)
	}
	if err := w.a.pool.WithPlatformAdmin(ctx, w.pa, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, w.tenant)
		return err
	}); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}
	return wrID
}

type hrDTO struct {
	ID          string  `json:"id"`
	State       string  `json:"state"`
	PayloadHash string  `json:"payload_hash"`
	LedgerTx    *string `json:"ledger_transaction_id"`
	Refusal     *string `json:"refusal_code"`
}

func (w *hrWorld) request(actor, wrID uuid.UUID, extra map[string]any) (int, []byte, hrDTO) {
	body := map[string]any{
		"withdrawal_request_id": wrID.String(), "kind": "release_hold_to_player", "reason_code": "tenant_suspended_release",
		"evidence_ref_hash": strings.Repeat("ab", 32), "note": "http hsec test",
	}
	for k, v := range extra {
		body[k] = v
	}
	res := w.a.do("POST", w.base(), w.tokenFor(actor), body)
	w.note(res.status, res.body)
	var d hrDTO
	if res.status == http.StatusCreated {
		res.decode(w.a.t, &d)
	}
	return res.status, res.body, d
}

func (w *hrWorld) decide(actor uuid.UUID, id, decision, hash string) (int, []byte) {
	res := w.a.do("POST", w.base()+"/"+id+"/"+decision, w.tokenFor(actor), map[string]any{"payload_hash": hash, "reason_code": "hsec_http_test"})
	w.note(res.status, res.body)
	return res.status, res.body
}

func hrHasClosedToken(body []byte) bool {
	for _, tk := range hrClosedTokens {
		if strings.Contains(string(body), `"`+tk+`"`) {
			return true
		}
	}
	return false
}

func (w *hrWorld) deniedAudits() int {
	var n int
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'withdrawal.hold_resolution_denied'`).Scan(&n)
	}); err != nil {
		w.a.t.Fatal(err)
	}
	return n
}

// The static gate: every tenant role is denied every route (403, before any session or
// database work); unauthenticated is 401; a platform_admin with an acting session but no
// hold-resolution capability is refused by the database (403 not_permitted).
func TestHSECRoutes_TenantRolesDeniedEverywhere_PlatformNeedsTheGrant(t *testing.T) {
	w := newHRWorld(t, true)
	wr := w.approvedWithdrawalOnSuspendedTenant(500)
	someID := uuid.New().String()
	routes := []struct{ method, path string }{
		{"POST", w.base()}, {"GET", w.base()}, {"GET", w.base() + "/" + someID},
		{"POST", w.base() + "/" + someID + "/approve"}, {"POST", w.base() + "/" + someID + "/reject"}, {"POST", w.base() + "/" + someID + "/cancel"},
	}
	for name, actor := range map[string]uuid.UUID{
		"finance with every K3 grant": w.f1, "tenant_admin": w.ta, "compliance": w.comp, "support": w.supp, "other tenant finance": w.otherFin,
	} {
		for _, rt := range routes {
			res := w.a.do(rt.method, rt.path, w.tok(actor), map[string]any{
				"withdrawal_request_id": wr.String(), "kind": "release_hold_to_player", "reason_code": "x_y", "evidence_ref_hash": strings.Repeat("ab", 32),
				"payload_hash": strings.Repeat("ab", 64)[:64], "tenant_id": w.tenant.String(),
			})
			if res.status != http.StatusForbidden {
				t.Errorf("%s %s %s: want 403, got %d %s", name, rt.method, rt.path, res.status, res.body)
			}
		}
	}
	for _, rt := range routes {
		if res := w.a.do(rt.method, rt.path, "", nil); res.status != http.StatusUnauthorized {
			t.Errorf("unauthenticated %s %s: want 401, got %d", rt.method, rt.path, res.status)
		}
	}
	// No release happened.
	if got := w.a.pool; got == nil {
		t.Fatal("pool")
	}
	// A platform_admin with an acting session but without the capability.
	st, body, _ := w.request(w.noGrant, wr, nil)
	if st != http.StatusForbidden || !strings.Contains(string(body), payments.TokenHoldResolveNotPermitted) {
		t.Fatalf("platform_admin without the capability: %d %s", st, body)
	}
	// A platform_admin with NO grant at all for the tenant cannot even open the session.
	st, body, _ = w.request(w.pb, wr, nil)
	if st != http.StatusForbidden || !strings.Contains(string(body), payments.TokenHoldResolveNotPermitted) {
		t.Fatalf("platform_admin without any grant: %d %s", st, body)
	}
	if w.deniedAudits() == 0 {
		t.Fatal("refusals wrote no denied audit rows")
	}
}

func TestHSECRoutes_Inert_WithoutPolicy_And_Validation(t *testing.T) {
	w := newHRWorld(t, false)
	wr := w.approvedWithdrawalOnSuspendedTenant(500)
	st, body, _ := w.request(w.p1, wr, nil)
	if st != http.StatusConflict || !strings.Contains(string(body), payments.TokenHoldResolveDisabled) {
		t.Fatalf("no policy row: want 409 %s, got %d %s", payments.TokenHoldResolveDisabled, st, body)
	}
	for name, extra := range map[string]map[string]any{
		"bad reason code":     {"reason_code": "Bad Code"},
		"upper-case reason":   {"reason_code": "Upper"},
		"unknown kind":        {"kind": "release_everything"},
		"no evidence hash":    {"evidence_ref_hash": ""},
		"non-hex hash":        {"evidence_ref_hash": strings.Repeat("z", 64)},
		"non-uuid withdrawal": {"withdrawal_request_id": "nope"},
	} {
		st, body, _ := w.request(w.p1, wr, extra)
		if st != http.StatusBadRequest && st != http.StatusUnprocessableEntity {
			t.Errorf("%s: want a 4xx validation refusal, got %d %s", name, st, body)
		}
	}
}

// End to end over HTTP: request, a self-approval refusal, two distinct approvers, the
// release, the read-backs, the closed token set and no database text in any refusal.
func TestHSECRoutes_EndToEnd_Release(t *testing.T) {
	w := newHRWorld(t, true)
	wr := w.approvedWithdrawalOnSuspendedTenant(750)

	// A body tenant_id of another tenant is accepted and IGNORED (the path decides).
	st, body, r := w.request(w.p1, wr, map[string]any{"tenant_id": w.other.String()})
	if st != http.StatusCreated || r.State != "pending" {
		t.Fatalf("request: %d %s", st, body)
	}
	// The requester cannot approve (four-eyes) ...
	if st, body := w.decide(w.p1, r.ID, "approve", r.PayloadHash); st != http.StatusForbidden && st != http.StatusConflict || !hrHasClosedToken(body) {
		t.Fatalf("self-approval: %d %s", st, body)
	}
	// ... a stale payload hash is refused ...
	if st, body := w.decide(w.p2, r.ID, "approve", strings.Repeat("0", 64)); st != http.StatusConflict || !hrHasClosedToken(body) {
		t.Fatalf("wrong payload hash: %d %s", st, body)
	}
	// ... one of two approvals does not execute.
	st, body = w.decide(w.p2, r.ID, "approve", r.PayloadHash)
	if st != http.StatusOK || !strings.Contains(string(body), `"executed":false`) || !strings.Contains(string(body), `"counted_approvals":1`) {
		t.Fatalf("first approval: %d %s", st, body)
	}
	if got := w.state(wr); got != "approved" {
		t.Fatalf("withdrawal state after one approval: %s", got)
	}
	st, body = w.decide(w.p3, r.ID, "approve", r.PayloadHash)
	if st != http.StatusOK || !strings.Contains(string(body), `"executed":true`) {
		t.Fatalf("final approval: %d %s", st, body)
	}
	if got := w.state(wr); got != "rejected" {
		t.Fatalf("withdrawal state after the release: %s", got)
	}
	// Read-backs.
	res := w.a.do("GET", w.base()+"/"+r.ID, w.tokenFor(w.p1), nil)
	var got hrDTO
	res.decode(t, &got)
	if res.status != http.StatusOK || got.State != "executed" || got.LedgerTx == nil {
		t.Fatalf("get: %d %s", res.status, res.body)
	}
	if res := w.a.do("GET", w.base(), w.tokenFor(w.p2), nil); res.status != http.StatusOK || !strings.Contains(string(res.body), r.ID) {
		t.Fatalf("list: %d %s", res.status, res.body)
	}
	// A replay is a clean conflict and posts nothing.
	if st, body := w.decide(w.p3, r.ID, "approve", r.PayloadHash); st != http.StatusConflict || !strings.Contains(string(body), payments.TokenHoldResolveConflict) {
		t.Fatalf("replay: %d %s", st, body)
	}
	// Closed token set, no database text.
	if len(w.refusals) < 3 {
		t.Fatalf("only %d refusal bodies collected: the leakage check would be vacuous", len(w.refusals))
	}
	for _, b := range w.refusals {
		for _, bad := range []string{"SQLSTATE", "violates", "HR0", "CG0", "AP0", "withdrawal_hold_resolutions:", "ERROR:"} {
			if strings.Contains(string(b), bad) {
				t.Errorf("a refusal body leaked database text (%q): %s", bad, b)
			}
		}
	}
	if w.deniedAudits() == 0 {
		t.Fatal("refusals wrote no denied audit rows")
	}
}

func (w *hrWorld) state(wr uuid.UUID) string {
	var s string
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state FROM withdrawal_requests WHERE id = $1`, wr).Scan(&s)
	}); err != nil {
		w.a.t.Fatal(err)
	}
	return s
}

// Cancel is requester-only; reject by a distinct approver ends the resolution.
func TestHSECRoutes_Cancel_And_Reject(t *testing.T) {
	w := newHRWorld(t, true)
	wr := w.approvedWithdrawalOnSuspendedTenant(300)
	_, _, r := w.request(w.p1, wr, nil)
	if res := w.a.do("POST", w.base()+"/"+r.ID+"/cancel", w.tokenFor(w.p2), nil); res.status != http.StatusConflict && res.status != http.StatusForbidden {
		t.Fatalf("cancel by a non-requester: %d %s", res.status, res.body)
	}
	if res := w.a.do("POST", w.base()+"/"+r.ID+"/cancel", w.tokenFor(w.p1), nil); res.status != http.StatusOK || !strings.Contains(string(res.body), `"state":"cancelled"`) {
		t.Fatalf("cancel by the requester: %d %s", res.status, res.body)
	}
	_, _, r2 := w.request(w.p1, wr, nil)
	if st, body := w.decide(w.p2, r2.ID, "reject", r2.PayloadHash); st != http.StatusOK || !strings.Contains(string(body), `"state":"rejected"`) {
		t.Fatalf("reject: %d %s", st, body)
	}
	if got := w.state(wr); got != "approved" {
		t.Fatalf("the hold must remain after a reject/cancel: %s", got)
	}
}
