//go:build integration

// PRH-2 K2 (ADR 0100) over real HTTP, on a private scratch database
// migrated to the latest on-disk migration (the cgAPI harness): security
// K2-G4 / K2-P2's A-1 (role x family x operation) and A-12 (audit content)
// for the K2 operations, plus the policy-change API. Policy values are
// synthetic, test-only (HD-PRH2-3), and scoped to one asset.
package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/capability"
)

type maWorld struct {
	a                          *cgAPI
	tenant, other, brand       uuid.UUID
	wallet, player             uuid.UUID
	f1, f2, fNoGrant, ta, supp uuid.UUID
	comp, otherFin             uuid.UUID
	pa, pb, grantApprover      uuid.UUID
	acting, actingNoGrant      uuid.UUID
}

func (w *maWorld) grant(tenantID, requester, grantee uuid.UUID, platformRequester bool, c capability.Capability) {
	w.a.t.Helper()
	ctx := context.Background()
	var reqID uuid.UUID
	create := func(ctx context.Context, tx pgx.Tx) error {
		in := capability.NewRequestInput{GranteeStaffID: grantee, Capability: c, ReasonCode: "k2-http"}
		if platformRequester {
			until := time.Now().Add(2 * time.Hour)
			in.ValidUntil = &until
		}
		r, err := capability.CreateRequest(ctx, tx, tenantID, in)
		reqID = r.ID
		return err
	}
	var err error
	if platformRequester {
		err = w.a.pool.WithPlatformAdmin(ctx, requester, create)
	} else {
		err = w.a.pool.WithPrincipalScope(ctx, tenantID, requester, create)
	}
	if err != nil {
		w.a.t.Fatalf("grant request: %v", err)
	}
	if err := w.a.pool.WithPlatformAdmin(ctx, w.grantApprover, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID, reqID, "approve", "k2-http")
		return err
	}); err != nil {
		w.a.t.Fatalf("grant approve: %v", err)
	}
}

func newMAWorld(t *testing.T) *maWorld {
	a := newCGAPI(t)
	w := &maWorld{a: a}
	w.tenant, w.other = a.tenant(), a.tenant()
	w.brand, w.player, w.wallet = uuid.New(), uuid.New(), uuid.New()
	person := uuid.New()
	ctx := context.Background()
	if err := a.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.pool.WithTenant(ctx, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, 'b', $3, 'active')`, w.brand, w.tenant, "mab-"+w.brand.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			w.player, w.tenant, w.brand, person, w.player.String()+"@ma.invalid"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'USD')`, w.wallet, w.tenant, w.brand, w.player)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	w.f1, w.f2, w.fNoGrant = a.staff(w.tenant, "finance"), a.staff(w.tenant, "finance"), a.staff(w.tenant, "finance")
	w.ta, w.supp, w.comp = a.staff(w.tenant, "tenant_admin"), a.staff(w.tenant, "support"), a.staff(w.tenant, "compliance")
	w.otherFin = a.staff(w.other, "finance")
	w.pa, w.pb, w.grantApprover = a.staff(uuid.Nil, "platform_admin"), a.staff(uuid.Nil, "platform_admin"), a.staff(uuid.Nil, "platform_admin")
	w.acting, w.actingNoGrant = a.staff(uuid.Nil, "platform_admin"), a.staff(uuid.Nil, "platform_admin")
	for _, f := range []uuid.UUID{w.f1, w.f2} {
		w.grant(w.tenant, w.ta, f, false, capability.CapabilityLedgerAdjustmentInitiate)
		w.grant(w.tenant, w.ta, f, false, capability.CapabilityLedgerAdjustmentApprove)
	}
	w.grant(w.tenant, w.pa, w.acting, true, capability.CapabilityLedgerAdjustmentInitiate)
	w.grant(w.tenant, w.pa, w.acting, true, capability.CapabilityLedgerAdjustmentApprove)

	// The platform baseline, through the policy-change API itself
	// (four-eyes: pa proposes, pb approves). Asset-scoped, synthetic.
	res := a.do("POST", "/v1/admin/financial-policy-changes", a.token(w.pa, uuid.Nil, auth.RolePlatformAdmin), map[string]any{
		"change_kind": "policy", "operation_kind": "ledger_adjustment", "level": "platform", "asset_code": "USD", "base_required_approvals": 1,
	})
	if res.status != http.StatusCreated {
		t.Fatalf("propose baseline: %d %s", res.status, res.body)
	}
	var ch struct {
		ID          string `json:"id"`
		ContentHash string `json:"content_hash"`
	}
	res.decode(t, &ch)
	if res := a.do("POST", "/v1/admin/financial-policy-changes/"+ch.ID+"/approve", a.token(w.pa, uuid.Nil, auth.RolePlatformAdmin),
		map[string]any{"content_hash": ch.ContentHash, "reason_code": "x"}); res.status != http.StatusConflict {
		t.Fatalf("self-approval of a policy change must be 409, got %d %s", res.status, res.body)
	}
	if res := a.do("POST", "/v1/admin/financial-policy-changes/"+ch.ID+"/approve", a.token(w.pb, uuid.Nil, auth.RolePlatformAdmin),
		map[string]any{"content_hash": ch.ContentHash, "reason_code": "x"}); res.status != http.StatusOK {
		t.Fatalf("approve baseline: %d %s", res.status, res.body)
	}
	return w
}

func (w *maWorld) tok(id uuid.UUID) string {
	switch id {
	case w.f1, w.f2, w.fNoGrant:
		return w.a.token(id, w.tenant, auth.RoleFinance)
	case w.ta:
		return w.a.token(id, w.tenant, auth.RoleTenantAdmin)
	case w.supp:
		return w.a.token(id, w.tenant, auth.RoleSupport)
	case w.comp:
		return w.a.token(id, w.tenant, auth.RoleCompliance)
	case w.otherFin:
		return w.a.token(id, w.other, auth.RoleFinance)
	default:
		return w.a.token(id, uuid.Nil, auth.RolePlatformAdmin)
	}
}

type maDTO struct {
	ID                  string  `json:"id"`
	PayloadHash         string  `json:"payload_hash"`
	State               string  `json:"state"`
	InitiatedByScope    string  `json:"initiated_by_scope"`
	LedgerTransactionID *string `json:"ledger_transaction_id"`
}

func (w *maWorld) submit(actor uuid.UUID) (int, maDTO) {
	res := w.a.do("POST", "/v1/admin/tenants/"+w.tenant.String()+"/manual-adjustments", w.tok(actor), map[string]any{
		"wallet_id": w.wallet.String(), "asset_code": "USD", "direction": "credit_player", "amount_minor_units": "250",
		"reason_code": "operational_error_correction", "note": "http k2 test",
	})
	var d maDTO
	if res.status == http.StatusCreated {
		res.decode(w.a.t, &d)
	}
	return res.status, d
}

// A-1 (AZ) for K2 operations: role x session family x operation, at HTTP
// and DB level. Every row states the reason for its expected status.
func TestManualAdjustmentAPI_A1_Matrix(t *testing.T) {
	w := newMAWorld(t)
	base := "/v1/admin/tenants/" + w.tenant.String() + "/manual-adjustments"

	// submit
	for _, c := range []struct {
		name  string
		actor uuid.UUID
		want  int
	}{
		{"finance with initiate grant (tenant family)", w.f1, http.StatusCreated},
		{"platform_admin with G-P2 grant (acting family)", w.acting, http.StatusCreated},
		{"finance without grant (DB: MA003)", w.fNoGrant, http.StatusForbidden},
		{"platform_admin without grant (DB: CG020)", w.actingNoGrant, http.StatusForbidden},
		{"tenant_admin (no static permission)", w.ta, http.StatusForbidden},
		{"support (no static permission)", w.supp, http.StatusForbidden},
		{"compliance (read only)", w.comp, http.StatusForbidden},
		{"another tenant's finance (canActOnTenant)", w.otherFin, http.StatusForbidden},
	} {
		if got, _ := w.submit(c.actor); got != c.want {
			t.Errorf("submit / %s: want %d got %d", c.name, c.want, got)
		}
	}

	// read
	st, pending := w.submit(w.f1)
	if st != http.StatusCreated {
		t.Fatalf("seed submit: %d", st)
	}
	for _, c := range []struct {
		name  string
		actor uuid.UUID
		want  int
	}{
		{"finance", w.f2, http.StatusOK},
		{"compliance", w.comp, http.StatusOK},
		{"acting platform_admin with grant", w.acting, http.StatusOK},
		{"platform_admin without grant", w.actingNoGrant, http.StatusForbidden},
		{"tenant_admin", w.ta, http.StatusForbidden},
		{"support", w.supp, http.StatusForbidden},
		{"another tenant's finance", w.otherFin, http.StatusForbidden},
	} {
		if res := w.a.do("GET", base+"/"+pending.ID, w.tok(c.actor), nil); res.status != c.want {
			t.Errorf("get / %s: want %d got %d %s", c.name, c.want, res.status, res.body)
		}
		if res := w.a.do("GET", base, w.tok(c.actor), nil); res.status != c.want {
			t.Errorf("list / %s: want %d got %d", c.name, c.want, res.status)
		}
	}
	// A foreign tenant's request id through a tenant's own path: 404.
	if res := w.a.do("GET", "/v1/admin/tenants/"+w.other.String()+"/manual-adjustments/"+pending.ID, w.tok(w.acting), nil); res.status != http.StatusForbidden {
		t.Errorf("acting principal on a tenant it holds no grant for: want 403 got %d", res.status)
	}

	// decide
	approve := func(actor uuid.UUID, d maDTO) (int, []byte) {
		res := w.a.do("POST", base+"/"+d.ID+"/approve", w.tok(actor), map[string]any{"payload_hash": d.PayloadHash, "reason_code": "ok"})
		return res.status, res.body
	}
	for _, c := range []struct {
		name  string
		actor uuid.UUID
		want  int
	}{
		{"self-approval (DB: MA031)", w.f1, http.StatusConflict},
		{"finance without grant (DB: MA003)", w.fNoGrant, http.StatusForbidden},
		{"platform_admin without grant (CG020)", w.actingNoGrant, http.StatusForbidden},
		{"compliance (no approve permission)", w.comp, http.StatusForbidden},
		{"tenant_admin (no approve permission)", w.ta, http.StatusForbidden},
		{"another tenant's finance", w.otherFin, http.StatusForbidden},
	} {
		if got, body := approve(c.actor, pending); got != c.want {
			t.Errorf("approve / %s: want %d got %d %s", c.name, c.want, got, body)
		}
	}
	if got, body := approve(w.acting, pending); got != http.StatusOK || !strings.Contains(string(body), `"executed":true`) {
		t.Fatalf("acting approval must execute: %d %s", got, body)
	}
	if got, _ := approve(w.f2, pending); got != http.StatusConflict {
		t.Errorf("approving an executed request: want 409 got %d", got)
	}
	// stale payload hash
	_, p2 := w.submit(w.acting)
	res := w.a.do("POST", base+"/"+p2.ID+"/approve", w.tok(w.f2), map[string]any{"payload_hash": strings.Repeat("0", 64), "reason_code": "ok"})
	if res.status != http.StatusConflict {
		t.Errorf("stale payload hash: want 409 got %d", res.status)
	}
	// reject + cancel
	if res := w.a.do("POST", base+"/"+p2.ID+"/reject", w.tok(w.f2), map[string]any{"payload_hash": p2.PayloadHash, "reason_code": "no"}); res.status != http.StatusOK {
		t.Errorf("reject: %d %s", res.status, res.body)
	}
	_, p3 := w.submit(w.f1)
	if res := w.a.do("POST", base+"/"+p3.ID+"/cancel", w.tok(w.f2), nil); res.status != http.StatusConflict {
		t.Errorf("cancel by a non-initiator: want 409 got %d", res.status)
	}
	if res := w.a.do("POST", base+"/"+p3.ID+"/cancel", w.tok(w.f1), nil); res.status != http.StatusOK {
		t.Errorf("cancel by the initiator: %d %s", res.status, res.body)
	}
	// Tenant policy route: tighten by tenant_admin OK; loosening refused.
	tp := "/v1/admin/tenants/" + w.tenant.String() + "/financial-policy-changes"
	if res := w.a.do("POST", tp, w.tok(w.ta), map[string]any{"change_kind": "policy", "operation_kind": "ledger_adjustment", "level": "tenant", "base_required_approvals": 1}); res.status != http.StatusCreated {
		t.Errorf("tenant tighten proposal: %d %s", res.status, res.body)
	}
	if res := w.a.do("POST", "/v1/admin/financial-policy-changes", w.tok(w.ta), map[string]any{"change_kind": "policy", "operation_kind": "ledger_adjustment", "level": "platform", "asset_code": "USD", "base_required_approvals": 1}); res.status != http.StatusForbidden {
		t.Errorf("tenant_admin on the platform policy route: want 403 got %d", res.status)
	}
	if res := w.a.do("POST", "/v1/admin/tenants/"+w.other.String()+"/financial-policy-changes", w.tok(w.ta), map[string]any{"change_kind": "policy", "operation_kind": "ledger_adjustment", "level": "tenant", "base_required_approvals": 2}); res.status != http.StatusForbidden {
		t.Errorf("tenant_admin on another tenant's policy route: want 403 got %d", res.status)
	}
}

// A-12 (AU) for K2: the audit content of submit / approve+execute /
// reject / cancel, including the acting actor forced by trigger
// (actor_scope platform_acting, tenant_id = X) and denied rows for
// refusals.
func TestManualAdjustmentAPI_A12_AuditContent(t *testing.T) {
	w := newMAWorld(t)
	base := "/v1/admin/tenants/" + w.tenant.String() + "/manual-adjustments"
	_, d := w.submit(w.f1)
	res := w.a.do("POST", base+"/"+d.ID+"/approve", w.tok(w.acting), map[string]any{"payload_hash": d.PayloadHash, "reason_code": "ok"})
	if res.status != http.StatusOK {
		t.Fatalf("approve: %d %s", res.status, res.body)
	}
	type row struct {
		action, actor, scope, state, reqID, payload string
		tenant                                      *uuid.UUID
		ledgerTx                                    *string
	}
	var rows []row
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		r, err := tx.Query(ctx, `SELECT action, actor_id::text, metadata->>'actor_scope', metadata->>'after_state', request_id, metadata->>'payload_hash',
			tenant_id, metadata->>'ledger_transaction_id' FROM audit_log WHERE target_id = $1 ORDER BY created_at, action`, d.ID)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var x row
			var reqID *string
			if err := r.Scan(&x.action, &x.actor, &x.scope, &x.state, &reqID, &x.payload, &x.tenant, &x.ledgerTx); err != nil {
				return err
			}
			if reqID != nil {
				x.reqID = *reqID
			}
			rows = append(rows, x)
		}
		return r.Err()
	}); err != nil {
		t.Fatal(err)
	}
	// One row per transition: submitted, then executed (the final
	// approval's own row IS the execution row).
	if len(rows) != 2 {
		t.Fatalf("expected exactly the submitted and executed rows, got %+v", rows)
	}
	byAction := map[string]row{}
	for _, r := range rows {
		byAction[r.action] = r
	}
	sub, ok := byAction["ledger_adjustment.submitted"]
	if !ok || sub.actor != w.f1.String() || sub.scope != "tenant" || sub.payload != d.PayloadHash || sub.tenant == nil || *sub.tenant != w.tenant {
		t.Fatalf("submission audit row wrong: %+v", sub)
	}
	ex, ok := byAction["ledger_adjustment.executed"]
	if !ok || ex.actor != w.acting.String() || ex.scope != "platform_acting" || ex.state != "executed" || ex.ledgerTx == nil || ex.tenant == nil || *ex.tenant != w.tenant {
		t.Fatalf("execution audit row wrong (acting actor must be forced, tenant X): %+v", ex)
	}
	{
		// The setter's own open row is keyed on the request id in metadata.
		var n int
		if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'financial.acting_session_opened' AND metadata->>'request_id' = $1
				AND metadata->>'operation' = 'ledger_adjustment' AND actor_id = $2`, d.ID, w.acting).Scan(&n)
		}); err != nil || n != 1 {
			t.Fatalf("acting open audit row for the approval: n=%d err=%v", n, err)
		}
	}
	// Denied: a platform principal without a grant -> platform-scope denied
	// row with the route target as subject tenant.
	res = w.a.do("POST", base, w.tok(w.actingNoGrant), map[string]any{
		"wallet_id": w.wallet.String(), "asset_code": "USD", "direction": "credit_player", "amount_minor_units": "1",
		"reason_code": "operational_error_correction", "note": "x"})
	if res.status != http.StatusForbidden {
		t.Fatalf("no-grant submit: %d", res.status)
	}
	var denied int
	if err := w.a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'ledger_adjustment.submit' AND outcome = 'denied' AND actor_id = $1
			AND tenant_id IS NULL AND subject_tenant_id = $2`, w.actingNoGrant, w.tenant).Scan(&denied)
	}); err != nil || denied != 1 {
		t.Fatalf("denied audit row for the no-grant platform principal: n=%d err=%v", denied, err)
	}
}

// Code review R-2: a decision on a request already past expires_at commits
// the pending -> expired transition and its audit row, records NO decision,
// and answers 409 "request expired" (never 200 {executed:false}).
func TestManualAdjustmentAPI_K2R2_ExpiredDecisionIs409(t *testing.T) {
	w := newMAWorld(t)
	base := "/v1/admin/tenants/" + w.tenant.String() + "/manual-adjustments"
	st, d := w.submit(w.f1)
	if st != http.StatusCreated {
		t.Fatalf("submit: %d", st)
	}
	// Scratch DB only: move expires_at into the past with the immutability
	// guard disabled inside ONE transaction (ALTER TABLE is transactional,
	// so no other session ever sees the guard off).
	if err := w.a.pool.WithPrincipalScope(context.Background(), w.tenant, w.f1, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE ledger_adjustment_requests DISABLE TRIGGER ledger_adjustment_requests_guard`); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET expires_at = now() - interval '1 minute' WHERE id = $1`, d.ID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("backdate touched %d rows", tag.RowsAffected())
		}
		if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `ALTER TABLE ledger_adjustment_requests ENABLE TRIGGER ledger_adjustment_requests_guard`)
		return err
	}); err != nil {
		t.Fatalf("backdate expires_at (scratch): %v", err)
	}
	res := w.a.do("POST", base+"/"+d.ID+"/approve", w.tok(w.f2), map[string]any{"payload_hash": d.PayloadHash, "reason_code": "ok"})
	if res.status != http.StatusConflict || !strings.Contains(string(res.body), "request expired") {
		t.Fatalf("decision on an expired request: want 409 \"request expired\", got %d %s", res.status, res.body)
	}
	var state string
	var expiredRows, approvals int
	if err := w.a.pool.WithPrincipalScope(context.Background(), w.tenant, w.f1, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT state FROM ledger_adjustment_requests WHERE id = $1`, d.ID).Scan(&state); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE target_id = $1 AND action = 'ledger_adjustment.expired'`, d.ID).Scan(&expiredRows); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_approvals WHERE request_id = $1`, d.ID).Scan(&approvals)
	}); err != nil {
		t.Fatal(err)
	}
	if state != "expired" || expiredRows != 1 || approvals != 0 {
		t.Fatalf("after expiry: state=%s expired-audit-rows=%d approvals=%d (want expired, 1, 0)", state, expiredRows, approvals)
	}
}

// Code review R-3: a DB payload-rule refusal (MA022 here: goodwill_credit
// as a debit) is a 400 carrying only the closed refusal token, with a
// denied audit row; never a 409 "conflict" and never DB message text.
func TestManualAdjustmentAPI_K2R3_PayloadRuleRefusalIs400WithToken(t *testing.T) {
	w := newMAWorld(t)
	res := w.a.do("POST", "/v1/admin/tenants/"+w.tenant.String()+"/manual-adjustments", w.tok(w.f1), map[string]any{
		"wallet_id": w.wallet.String(), "asset_code": "USD", "direction": "debit_player", "amount_minor_units": "5",
		"reason_code": "goodwill_credit", "note": "r3",
	})
	if res.status != http.StatusBadRequest || !strings.Contains(string(res.body), "direction_not_allowed_for_reason") {
		t.Fatalf("goodwill debit: want 400 with the closed token, got %d %s", res.status, res.body)
	}
	if strings.Contains(string(res.body), "ledger_adjustment_requests") {
		t.Fatalf("DB message text leaked: %s", res.body)
	}
	var denied int
	if err := w.a.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'ledger_adjustment.submit' AND outcome = 'denied' AND actor_id = $1`, w.f1).Scan(&denied)
	}); err != nil || denied != 1 {
		t.Fatalf("denied audit row: n=%d err=%v", denied, err)
	}
}
