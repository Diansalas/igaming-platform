//go:build integration

package httpserver

// U-7 runtime companion to staff_mutation_audit_static_test.go: for one
// representative mutating staff route per route family, a request through the
// REAL router (New(Deps), the full staffRoute chain: auth middleware, staff
// principal, live sealed session, AAL2, step-up class, permission, tenant
// scope) by a test staff principal with a live session must commit exactly one
// success audit_log row naming the acting staff member, the tenant, the entity
// and - where the route requires one - the reason. The static test proves
// reachability for every route; this proves an actual row for each family.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/auth/authtest"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

type u7AuditRow struct {
	actorType, targetType, targetID, outcome string
	actorID                                  *uuid.UUID
	tenantID                                 *uuid.UUID
	metadata                                 map[string]any
}

// u7AuditRows reads every audit_log row of action by actor, in tenant scope
// (tenant-scoped rows are visible there under audit_log's RLS).
func u7AuditRows(t *testing.T, ctx context.Context, txScope func(context.Context, func(context.Context, pgx.Tx) error) error, action string, actor uuid.UUID) []u7AuditRow {
	t.Helper()
	var out []u7AuditRow
	if err := txScope(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT actor_type, actor_id, tenant_id, coalesce(target_type, ''), coalesce(target_id, ''), outcome, metadata
			FROM audit_log WHERE action = $1 AND actor_id = $2`, action, actor)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r u7AuditRow
			var md []byte
			if err := rows.Scan(&r.actorType, &r.actorID, &r.tenantID, &r.targetType, &r.targetID, &r.outcome, &md); err != nil {
				return err
			}
			if err := json.Unmarshal(md, &r.metadata); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read audit_log: %v", err)
	}
	return out
}

func TestStaffMutatingRoutes_WriteAuditRow_PerFamily(t *testing.T) {
	pool, issuer := testEnv(t)
	// newTestServer's Deps plus a MOCK payments orchestrator (the kill-switch
	// routes register only with one, payments_kill_switch_handlers.go).
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	srv := httptest.NewServer(New(Deps{
		Logger:          slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:              pool,
		AuthIssuer:      issuer,
		StaffMFA:        testStaffMFA(),
		ServiceName:     "u7-audit-test",
		AccessTokenTTL:  5 * time.Minute,
		RefreshTokenTTL: time.Hour,
		PersonResolver:  identityresolution.NewMockPersonResolver(), MarketGate: allowAllMarketGate{},
		PaymentOrchestrator: payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock},
			payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)}).WithPayoutDestinations(pitest.Shared()),
	}))
	t.Cleanup(srv.Close)
	ctx := context.Background()
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)

	type staffP struct {
		id    uuid.UUID
		token string
	}
	mk := func(tenantID uuid.UUID, role identity.StaffRole) staffP {
		s := mustCreateStaff(t, pool, tenantID, role, "u7-"+uuid.NewString())
		return staffP{id: s.ID, token: authtest.LiveStaffToken(t, pool, testStaffMFAKeys(), issuer, s.ID, tenantID, auth.Role(role))}
	}
	tenantScope := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
		return pool.WithTenant(ctx, tenant.ID, fn)
	}

	// Bonus family fixture: a generated suggestion to claim (the U-7 fix).
	var suggestionID uuid.UUID
	if err := pool.WithTenant(ctx, tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		mv := "u7-model-v1"
		s, err := bonus.CreateSuggestion(ctx, tx, bonus.Suggestion{TenantID: tenant.ID, OriginatingKind: bonus.OriginatingModel, OriginatingModelVersion: &mv, ProposedConfig: []byte(`{}`)})
		suggestionID = s.ID
		return err
	}); err != nil {
		t.Fatalf("seed suggestion: %v", err)
	}

	cases := []struct {
		family     string
		role       identity.StaffRole
		platform   bool // platform staff acting on the tenant
		method     string
		path       string
		body       any
		wantStatus int
		action     string
		targetType string
		targetID   func(resp []byte, actor uuid.UUID) string
		reasonKey  string // metadata key that must carry the request's reason ("" = none required)
		reason     string
	}{
		{
			family: "players", role: identity.StaffRoleCompliance, method: http.MethodPost,
			path: "/v1/admin/players/" + player.ID.String() + "/suspend", body: map[string]any{"reason": "u7_reason_suspend"},
			wantStatus: http.StatusNoContent, action: "player.suspended", targetType: "player_account",
			targetID: func([]byte, uuid.UUID) string { return player.ID.String() }, reasonKey: "reason", reason: "u7_reason_suspend",
		},
		{
			family: "staff self-service", role: identity.StaffRoleCompliance, method: http.MethodPatch,
			path: "/v1/admin/staff/me/display-name", body: map[string]any{"display_name": "U7 Display"},
			wantStatus: http.StatusNoContent, action: "staff.display_name_changed", targetType: "staff_user",
			targetID: func(_ []byte, actor uuid.UUID) string { return actor.String() },
		},
		{
			family: "rg", role: identity.StaffRoleCompliance, method: http.MethodPost,
			path:       "/v1/admin/rg/restrictions",
			body:       map[string]any{"player_account_id": player.ID.String(), "scope": "tenant", "reason_code": "u7_reason_rg"},
			wantStatus: http.StatusCreated, action: "rg.self_exclusion.created", targetType: "player_restriction",
			targetID: idFromBody, reasonKey: "reason_code", reason: "u7_reason_rg",
		},
		{
			family: "risk", role: identity.StaffRoleRiskManager, method: http.MethodPost,
			path:       "/v1/admin/risk/rules",
			body:       map[string]any{"operation": "casino_bet", "limit_kind": "max_amount", "time_window": "transaction", "threshold": 500, "threshold_exponent": 2},
			wantStatus: http.StatusCreated, action: "risk.rule_created", targetType: "risk_rule", targetID: idFromBody,
		},
		{
			family: "payments", role: identity.StaffRoleTenantAdmin, method: http.MethodPost,
			path:       "/v1/admin/tenants/" + tenant.ID.String() + "/payments/kill-switches",
			body:       map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "u7_reason_ks"},
			wantStatus: http.StatusOK, action: "payments_kill_switch.engage", targetType: "payment_kill_switch",
			targetID: idFromBody, reasonKey: "reason_code", reason: "u7_reason_ks",
		},
		{
			family: "bonus", role: identity.StaffRolePromotionsManager, method: http.MethodPost,
			path:       "/v1/admin/bonus/suggestions/" + suggestionID.String() + "/claim",
			wantStatus: http.StatusOK, action: "bonus_suggestion.claimed", targetType: "bonus_suggestion",
			targetID: func([]byte, uuid.UUID) string { return suggestionID.String() },
		},
		{
			family: "platform tenant provisioning", role: identity.StaffRolePlatformAdmin, platform: true, method: http.MethodPost,
			path:       "/v1/admin/tenants/" + tenant.ID.String() + "/brands",
			body:       map[string]any{"name": "U7 Brand", "slug": "u7-" + uuid.NewString()[:13]},
			wantStatus: http.StatusCreated, action: "brand.created", targetType: "brand", targetID: idFromBody,
		},
	}
	for _, c := range cases {
		t.Run(c.family, func(t *testing.T) {
			staffTenant := tenant.ID
			if c.platform {
				staffTenant = uuid.Nil
			}
			s := mk(staffTenant, c.role)
			before := len(u7AuditRows(t, ctx, tenantScope, c.action, s.id))
			resp := u7Do(t, srv, c.method, c.path, s.token, c.body)
			defer resp.Body.Close()
			var raw json.RawMessage
			_ = json.NewDecoder(resp.Body).Decode(&raw)
			if resp.StatusCode != c.wantStatus {
				t.Fatalf("%s %s: status %d, want %d: %s", c.method, c.path, resp.StatusCode, c.wantStatus, raw)
			}
			rows := u7AuditRows(t, ctx, tenantScope, c.action, s.id)
			if len(rows) != before+1 {
				t.Fatalf("%s: %d %q audit rows by the actor after the request (before %d), want exactly one more", c.path, len(rows), c.action, before)
			}
			r := rows[len(rows)-1]
			if r.actorType != "staff" || r.actorID == nil || *r.actorID != s.id {
				t.Errorf("actor: type %q id %v, want staff %s", r.actorType, r.actorID, s.id)
			}
			if r.tenantID == nil || *r.tenantID != tenant.ID {
				t.Errorf("tenant: %v, want %s", r.tenantID, tenant.ID)
			}
			if r.outcome != "success" {
				t.Errorf("outcome %q, want success", r.outcome)
			}
			if want := c.targetID(raw, s.id); r.targetType != c.targetType || want == "" || r.targetID != want {
				t.Errorf("entity: %s/%s, want %s/%s", r.targetType, r.targetID, c.targetType, want)
			}
			if c.reasonKey != "" && r.metadata[c.reasonKey] != c.reason {
				t.Errorf("reason: metadata[%q] = %v, want %q", c.reasonKey, r.metadata[c.reasonKey], c.reason)
			}
		})
	}
}

func idFromBody(body []byte, _ uuid.UUID) string {
	var v struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(body, &v)
	return v.ID
}

func u7Do(t *testing.T, srv *httptest.Server, method, path, bearer string, body any) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, srv.URL+path, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
