//go:build integration

// PRH-2 G1 (ADR 0104 §5/§7, KS-AUDIT-TENANT-1): GET /v1/admin/audit-log/
// platform-actions end to end over real HTTP, on the same ksAPI harness
// payments_kill_switch_api_integration_test.go already builds (this file
// deliberately reuses newKSAPI/ksScratchPool/ksMigrationsDir rather than
// re-deriving a parallel fixture).
package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// newAuditAPI mirrors newKSAPI exactly, except it lets a test inject
// deps.AuditPresentationResolver (production wiring never sets this
// field) - the ONLY sanctioned way to exercise "a forced resolver error
// fails closed" over the real HTTP path.
func newAuditAPI(t *testing.T, resolver audit.PresentationResolver) *ksAPI {
	t.Helper()
	pool := ksScratchPool(t, "audit_api_")
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": strings.Repeat("k", 32)})
	if err != nil {
		t.Fatal(err)
	}
	issuer := auth.NewIssuer(keys, "audit-test", "audit-test")
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock},
		payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)})
	logBuf := &syncBuffer{}
	srv := httptest.NewServer(New(Deps{
		Logger:                    slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logBuf), &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                        pool,
		AuthIssuer:                issuer,
		ServiceName:               "audit-test",
		AccessTokenTTL:            time.Hour,
		RefreshTokenTTL:           time.Hour,
		PersonResolver:            identityresolution.NewMockPersonResolver(),
		PaymentOrchestrator:       orchestrator,
		AuditPresentationResolver: resolver,
	}))
	t.Cleanup(srv.Close)
	return &ksAPI{t: t, pool: pool, issuer: issuer, srv: srv, logBuf: logBuf}
}

// fourEyesEngageAndRelease drives a full platform-scope kill-switch
// engage -> request_release -> approve_release cycle over HTTP - the
// ADR 0104 §7 "TI" fixture shape (engage, then release through four-eyes).
// tok1/tok2 must be DISTINCT platform-admin tokens (four-eyes).
func fourEyesEngageAndRelease(t *testing.T, a *ksAPI, tenantID uuid.UUID, tok1, tok2, reasonEngage, reasonRelease string) killSwitchDTO {
	t.Helper()
	base := "/v1/admin/tenants/" + tenantID.String() + "/payments"

	engageResp := a.do("POST", base+"/kill-switches", tok1, map[string]any{
		"provider_scope": "*", "operation_scope": "deposit", "reason_code": reasonEngage,
	})
	if engageResp.status != http.StatusOK {
		t.Fatalf("engage: status=%d body=%s", engageResp.status, engageResp.body)
	}
	var ks killSwitchDTO
	engageResp.decode(t, &ks)

	reqResp := a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tok1, map[string]any{"reason_code": reasonRelease})
	if reqResp.status != http.StatusCreated {
		t.Fatalf("request_release: status=%d body=%s", reqResp.status, reqResp.body)
	}
	var relReq killSwitchReleaseRequestDTO
	reqResp.decode(t, &relReq)

	approveResp := a.do("POST", base+"/kill-switch-release-requests/"+relReq.ID+"/approve", tok2, nil)
	if approveResp.status != http.StatusOK {
		t.Fatalf("approve_release: status=%d body=%s", approveResp.status, approveResp.body)
	}
	return ks
}

type platformActionsPage struct {
	Items []platformActionAuditEntry `json:"items"`
	Total int                        `json:"total"`
}

func getPlatformActions(t *testing.T, a *ksAPI, token string) (platformActionsPage, int) {
	t.Helper()
	resp := a.do("GET", "/v1/admin/audit-log/platform-actions", token, nil)
	var page platformActionsPage
	if resp.status == http.StatusOK {
		resp.decode(t, &page)
	}
	return page, resp.status
}

// TestPlatformActionsAuditAPI_TIHeadline is ADR 0104 §7's headline test.
func TestPlatformActionsAuditAPI_TIHeadline(t *testing.T) {
	a := newAuditAPI(t, nil)
	tenantA := a.tenant()
	tenantB := a.tenant()
	platX := a.platformAdmin()
	platY := a.platformAdmin()
	tokX := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	tokY := a.token(platY, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)

	// Give the platform admin actor a display name, exercising §5.4's
	// name lookup end to end.
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET display_name = 'Platform Ops' WHERE id = $1`, platX)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	fourEyesEngageAndRelease(t, a, tenantA, tokX, tokY, "incident-a", "resolved-a")
	fourEyesEngageAndRelease(t, a, tenantB, tokX, tokY, "incident-b", "resolved-b")

	tenantAAdmin := a.tenantStaff(tenantA, "tenant_admin")
	tenantATok := a.token(tenantAAdmin, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)
	tenantBAdmin := a.tenantStaff(tenantB, "tenant_admin")
	tenantBTok := a.token(tenantBAdmin, tenantB, auth.RoleTenantAdmin, auth.PrincipalStaff)

	pageA, status := getPlatformActions(t, a, tenantATok)
	if status != http.StatusOK {
		t.Fatalf("tenant A read: status=%d", status)
	}
	if pageA.Total != 3 {
		t.Fatalf("expected tenant A to see exactly its 3 rows (engage+request_release+approve_release), got total=%d items=%+v", pageA.Total, pageA.Items)
	}
	for _, e := range pageA.Items {
		if e.Actor.Scope != "platform" || e.Actor.StaffID == "" {
			t.Errorf("expected an identified platform actor, got %+v", e.Actor)
		}
		if strings.Contains(e.Action, "incident-b") || strings.Contains(e.ReasonCode, "incident-b") {
			t.Errorf("tenant A must never see tenant B's rows: %+v", e)
		}
	}
	// The engage row must carry reason_code and the display name; the
	// approve_release row must carry the switch's before/after.
	var sawEngage, sawApprove bool
	for _, e := range pageA.Items {
		switch e.Action {
		case "payments_kill_switch.engage":
			sawEngage = true
			if e.ReasonCode != "incident-a" {
				t.Errorf("expected reason_code=incident-a, got %q", e.ReasonCode)
			}
			if e.Actor.StaffID != platX.String() {
				t.Errorf("expected actor.staff_id=%s, got %s", platX, e.Actor.StaffID)
			}
			if e.Actor.DisplayName == nil || *e.Actor.DisplayName != "Platform Ops" {
				t.Errorf("expected actor.display_name=Platform Ops, got %v", e.Actor.DisplayName)
			}
		case "payments_kill_switch.approve_release":
			sawApprove = true
			if e.Before == nil || e.After == nil {
				t.Errorf("expected approve_release to carry before/after, got before=%v after=%v", e.Before, e.After)
			}
		}
	}
	if !sawEngage || !sawApprove {
		t.Fatalf("expected both engage and approve_release rows, got %+v", pageA.Items)
	}
	// AT-6: no ip_address/user_agent/request_id/non-allowlisted metadata
	// key anywhere in the raw JSON.
	raw, _ := json.Marshal(pageA)
	for _, forbidden := range []string{"ip_address", "user_agent", "request_id"} {
		if strings.Contains(string(raw), forbidden) {
			t.Errorf("AT-6: response must never contain %q, got: %s", forbidden, raw)
		}
	}

	pageB, status := getPlatformActions(t, a, tenantBTok)
	if status != http.StatusOK {
		t.Fatalf("tenant B read: status=%d", status)
	}
	if pageB.Total != 3 {
		t.Fatalf("expected tenant B to see exactly its 3 rows, got total=%d", pageB.Total)
	}
	for _, e := range pageB.Items {
		if e.ReasonCode == "incident-a" {
			t.Errorf("tenant B must never see tenant A's rows: %+v", e)
		}
	}

	// Raw SELECT under WithTenant(A) never returns B's rows (defence in
	// depth, independent of the HTTP layer).
	var crossCount int
	if err := a.pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id IS NULL AND subject_tenant_id = $1`, tenantB).Scan(&crossCount)
	}); err != nil {
		t.Fatal(err)
	}
	if crossCount != 0 {
		t.Fatalf("expected a raw SELECT under WithTenant(A) to see 0 of B's subject rows, got %d", crossCount)
	}
}

// TestPlatformActionsAuditAPI_ApprovalChainLinksReleaseToEngage confirms
// §5.5: the approval chain links a kill-switch's engage/request/approve
// rows together, built only from the tenant's own RLS-filtered read.
func TestPlatformActionsAuditAPI_ApprovalChainLinksReleaseToEngage(t *testing.T) {
	a := newAuditAPI(t, nil)
	tenantA := a.tenant()
	platX := a.platformAdmin()
	platY := a.platformAdmin()
	tokX := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	tokY := a.token(platY, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	fourEyesEngageAndRelease(t, a, tenantA, tokX, tokY, "chain-incident", "chain-resolved")

	tenantAAdmin := a.tenantStaff(tenantA, "tenant_admin")
	tenantATok := a.token(tenantAAdmin, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)
	page, status := getPlatformActions(t, a, tenantATok)
	if status != http.StatusOK {
		t.Fatalf("read: status=%d", status)
	}
	var engage platformActionAuditEntry
	for _, e := range page.Items {
		if e.Action == "payments_kill_switch.engage" {
			engage = e
		}
	}
	if len(engage.ApprovalChain) == 0 {
		t.Fatalf("expected the engage row's approval_chain to link its later request_release/approve_release siblings, got %+v", page.Items)
	}
	var sawChainedApprove bool
	for _, c := range engage.ApprovalChain {
		if c.Action == "payments_kill_switch.approve_release" {
			sawChainedApprove = true
		}
	}
	if !sawChainedApprove {
		t.Errorf("expected the chain to include the approve_release sibling, got %+v", engage.ApprovalChain)
	}
}

// TestPlatformActionsAuditAPI_ExcludedSessions is ADR 0104 §7's "excluded
// sessions" list, over the real HTTP surface: a player token and a
// platform-scoped token must both fail to reach any subject row (the
// platform token fails RequireTenantScope entirely; there is no
// production HTTP path today that sets app.platform_service_id or
// app.acting_* on an admin request - those are covered at the RLS layer
// directly in internal/audit's integration tests).
func TestPlatformActionsAuditAPI_ExcludedSessions(t *testing.T) {
	a := newAuditAPI(t, nil)
	tenantA := a.tenant()
	platX := a.platformAdmin()
	platY := a.platformAdmin()
	tokX := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	tokY := a.token(platY, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	fourEyesEngageAndRelease(t, a, tenantA, tokX, tokY, "excl-incident", "excl-resolved")

	// A player token: RequirePermission(PermAuditRead) refuses it (players
	// hold no staff permission at all).
	playerTok := a.token(a.player(tenantA), tenantA, auth.RolePlayer, auth.PrincipalPlayer)
	if _, status := getPlatformActions(t, a, playerTok); status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Errorf("expected a player token to be refused, got status=%d", status)
	}

	// A platform-scoped token: RequireTenantScope refuses a nil-tenant
	// caller before the handler's own defence-in-depth check even runs.
	if _, status := getPlatformActions(t, a, tokX); status != http.StatusForbidden && status != http.StatusUnauthorized {
		t.Errorf("expected a platform-scoped token to be refused (no tenant to project into), got status=%d", status)
	}
}

// TestPlatformActionsAuditAPI_ForcedResolverErrorFailsClosed is the §7
// "a forced resolver error fails closed" test, over the real HTTP path:
// a resolver that would (if honored) widen disclosure (ShowFreeFormMetadata:
// true - surfacing every non-allowlisted metadata key, including
// denied_class) must never have that effect once it also returns an
// error.
func TestPlatformActionsAuditAPI_ForcedResolverErrorFailsClosed(t *testing.T) {
	widerButErroring := func(ctx context.Context, tenantID uuid.UUID) (audit.Presentation, error) {
		return audit.Presentation{ActorPresentation: audit.ActorPresentationIdentified, ShowNetworkMetadata: true, ShowFreeFormMetadata: true},
			context.Canceled // any non-nil error
	}
	a := newAuditAPI(t, widerButErroring)
	tenantA := a.tenant()
	platX := a.platformAdmin()
	platY := a.platformAdmin()
	tokX := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	tokY := a.token(platY, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	fourEyesEngageAndRelease(t, a, tenantA, tokX, tokY, "resolver-fail-incident", "resolver-fail-resolved")

	tenantAAdmin := a.tenantStaff(tenantA, "tenant_admin")
	tenantATok := a.token(tenantAAdmin, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)
	resp := a.do("GET", "/v1/admin/audit-log/platform-actions", tenantATok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("expected the request to still succeed (fail closed to the restrictive presentation, not fail the whole request), got status=%d body=%s", resp.status, resp.body)
	}
	if strings.Contains(string(resp.body), "extra_metadata") || strings.Contains(string(resp.body), "denied_class") {
		t.Fatalf("expected the forced resolver error to fail closed (no extra_metadata/denied_class surfaced despite the resolver's own wider value), got: %s", resp.body)
	}
}

// TestPlatformActionsAuditAPI_NameLookupNeverSelectsEmail is C-104-3/AT-8:
// an assertion on the exact SQL text of the name-lookup query.
func TestPlatformActionsAuditAPI_NameLookupNeverSelectsEmail(t *testing.T) {
	if strings.Contains(strings.ToLower(staffDisplayNameLookupQuery), "email") {
		t.Fatalf("the staff display-name lookup query must never select email, got: %s", staffDisplayNameLookupQuery)
	}
	if !strings.Contains(staffDisplayNameLookupQuery, "display_name") || !strings.Contains(staffDisplayNameLookupQuery, "staff_users") {
		t.Fatalf("expected the query to select id, display_name from staff_users, got: %s", staffDisplayNameLookupQuery)
	}
}
