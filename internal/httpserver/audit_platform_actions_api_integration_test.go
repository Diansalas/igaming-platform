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
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
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
		payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)}).WithPayoutDestinations(pitest.Shared())
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
	return getPlatformActionsPath(t, a, token, "/v1/admin/audit-log/platform-actions")
}

func getPlatformActionsPath(t *testing.T, a *ksAPI, token, path string) (platformActionsPage, int) {
	t.Helper()
	resp := a.do("GET", path, token, nil)
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
// chainHasAction reports whether any entry in chain has the given action -
// a small helper so every F-3 assertion below reads the same way.
func chainHasAction(chain []platformActionAuditChainEntry, action string) bool {
	for _, c := range chain {
		if c.Action == action {
			return true
		}
	}
	return false
}

// findByAction returns the FIRST item in items whose Action matches -
// t.Fatal's if none is found, since every caller below requires exactly
// one such row to exist.
func findByAction(t *testing.T, items []platformActionAuditEntry, action string) platformActionAuditEntry {
	t.Helper()
	for _, e := range items {
		if e.Action == action {
			return e
		}
	}
	t.Fatalf("expected an item with action=%s, got %+v", action, items)
	return platformActionAuditEntry{}
}

// TestPlatformActionsAuditAPI_ApprovalChainLinksReleaseToEngage is F-3
// (code review): the chain link that depends ENTIRELY on metadata's own
// "kill_switch_id" key (request_release has no other way to be linked -
// its own target_type is payment_kill_switch_release_request, never
// payment_kill_switch, so killSwitchChainKey's target_id fallback can
// never key it) was previously asserted only one-directionally (engage's
// chain contains approve_release) - a mutant disabling the metadata
// branch entirely (M2) SURVIVED that assertion, because engage and
// approve_release are BOTH keyable via their own target_id
// (payment_kill_switch), so their mutual link survives even with the
// metadata branch gone; only request_release's OWN presence in the chain
// actually proves that branch is alive. This test now asserts
// request_release's membership from all three sides.
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

	engage := findByAction(t, page.Items, "payments_kill_switch.engage")
	requestRelease := findByAction(t, page.Items, "payments_kill_switch.request_release")
	approveRelease := findByAction(t, page.Items, "payments_kill_switch.approve_release")

	if !chainHasAction(engage.ApprovalChain, "payments_kill_switch.request_release") {
		t.Errorf("F-3: expected the engage row's chain to include request_release, got %+v", engage.ApprovalChain)
	}
	if !chainHasAction(engage.ApprovalChain, "payments_kill_switch.approve_release") {
		t.Errorf("expected the engage row's chain to include approve_release, got %+v", engage.ApprovalChain)
	}
	if !chainHasAction(approveRelease.ApprovalChain, "payments_kill_switch.request_release") {
		t.Errorf("F-3: expected the approve_release row's chain to include request_release, got %+v", approveRelease.ApprovalChain)
	}
	if !chainHasAction(approveRelease.ApprovalChain, "payments_kill_switch.engage") {
		t.Errorf("expected the approve_release row's chain to include engage, got %+v", approveRelease.ApprovalChain)
	}
	if !chainHasAction(requestRelease.ApprovalChain, "payments_kill_switch.engage") {
		t.Errorf("expected the request_release row's own chain to include engage, got %+v", requestRelease.ApprovalChain)
	}
	if !chainHasAction(requestRelease.ApprovalChain, "payments_kill_switch.approve_release") {
		t.Errorf("expected the request_release row's own chain to include approve_release, got %+v", requestRelease.ApprovalChain)
	}
}

// TestPlatformActionsAuditAPI_ApprovalChainIncludesCancelRelease is F-3's
// second required scenario: engage -> request_release -> cancel_release
// (instead of approve), asserting the cancel is linked into the same
// chain as engage and request_release - cancel_release's own
// kill_switch_id metadata key (added alongside G1) is what makes this
// possible, since its target_type (payment_kill_switch_release_request)
// can never be keyed via the target_id fallback either.
func TestPlatformActionsAuditAPI_ApprovalChainIncludesCancelRelease(t *testing.T) {
	a := newAuditAPI(t, nil)
	tenantA := a.tenant()
	platX := a.platformAdmin()
	tokX := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenantA.String() + "/payments"

	engageResp := a.do("POST", base+"/kill-switches", tokX, map[string]any{
		"provider_scope": "*", "operation_scope": "deposit", "reason_code": "cancel-chain-incident",
	})
	if engageResp.status != http.StatusOK {
		t.Fatalf("engage: status=%d body=%s", engageResp.status, engageResp.body)
	}
	var ks killSwitchDTO
	engageResp.decode(t, &ks)

	reqResp := a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tokX, map[string]any{"reason_code": "cancel-chain-release"})
	if reqResp.status != http.StatusCreated {
		t.Fatalf("request_release: status=%d body=%s", reqResp.status, reqResp.body)
	}
	var relReq killSwitchReleaseRequestDTO
	reqResp.decode(t, &relReq)

	cancelResp := a.do("POST", base+"/kill-switch-release-requests/"+relReq.ID+"/cancel", tokX, nil)
	if cancelResp.status != http.StatusNoContent {
		t.Fatalf("cancel_release: status=%d body=%s", cancelResp.status, cancelResp.body)
	}

	tenantAAdmin := a.tenantStaff(tenantA, "tenant_admin")
	tenantATok := a.token(tenantAAdmin, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)
	page, status := getPlatformActions(t, a, tenantATok)
	if status != http.StatusOK {
		t.Fatalf("read: status=%d", status)
	}

	engage := findByAction(t, page.Items, "payments_kill_switch.engage")
	requestRelease := findByAction(t, page.Items, "payments_kill_switch.request_release")
	cancelRelease := findByAction(t, page.Items, "payments_kill_switch.cancel_release")

	if !chainHasAction(engage.ApprovalChain, "payments_kill_switch.cancel_release") {
		t.Errorf("F-3: expected the engage row's chain to include cancel_release, got %+v", engage.ApprovalChain)
	}
	if !chainHasAction(requestRelease.ApprovalChain, "payments_kill_switch.cancel_release") {
		t.Errorf("F-3: expected the request_release row's chain to include cancel_release, got %+v", requestRelease.ApprovalChain)
	}
	if !chainHasAction(cancelRelease.ApprovalChain, "payments_kill_switch.engage") {
		t.Errorf("expected the cancel_release row's own chain to include engage, got %+v", cancelRelease.ApprovalChain)
	}
	if !chainHasAction(cancelRelease.ApprovalChain, "payments_kill_switch.request_release") {
		t.Errorf("expected the cancel_release row's own chain to include request_release, got %+v", cancelRelease.ApprovalChain)
	}
}

// TestPlatformActionsAuditAPI_ChainIncludesTenantOwnRows is F-5 (code
// review; orchestrator decision: conform to ADR §5.5 - "the tenant's own
// rows; subject rows for that tenant"): a TENANT-requested release,
// approved by a PLATFORM admin, must show the tenant's own engage/
// request_release rows (ordinary tenant-scope audit rows, tenant_id =
// tenantA - never subject rows) in the resulting subject
// (approve_release) row's approval_chain. Before this fix, the chain was
// built only from subject rows, so the tenant's own engage/request_release
// rows - which the tenant can already read via GET /v1/admin/audit-log
// today - were invisible from the platform-actions chain specifically.
func TestPlatformActionsAuditAPI_ChainIncludesTenantOwnRows(t *testing.T) {
	a := newAuditAPI(t, nil)
	tenantA := a.tenant()
	tenantAdmin := a.tenantStaff(tenantA, "tenant_admin")
	tenantTok := a.token(tenantAdmin, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)
	platX := a.platformAdmin()
	platTok := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenantA.String() + "/payments"

	// (1) TENANT engage.
	engageResp := a.do("POST", base+"/kill-switches", tenantTok, map[string]any{
		"provider_scope": "*", "operation_scope": "deposit", "reason_code": "f5-tenant-incident",
	})
	if engageResp.status != http.StatusOK {
		t.Fatalf("tenant engage: status=%d body=%s", engageResp.status, engageResp.body)
	}
	var ks killSwitchDTO
	engageResp.decode(t, &ks)
	if ks.ChangedByScope != "tenant" {
		t.Fatalf("setup: expected a tenant-scope engage, got %+v", ks)
	}

	// (2) TENANT request_release.
	reqResp := a.do("POST", base+"/kill-switches/"+ks.ID+"/release-requests", tenantTok, map[string]any{"reason_code": "f5-tenant-release"})
	if reqResp.status != http.StatusCreated {
		t.Fatalf("tenant request_release: status=%d body=%s", reqResp.status, reqResp.body)
	}
	var relReq killSwitchReleaseRequestDTO
	reqResp.decode(t, &relReq)

	// (3) PLATFORM approve_release - a distinct principal from the tenant
	// requester (four-eyes), and a genuinely platform-scope caller acting
	// on tenantA per canActOnTenant - this is what writes the SUBJECT row.
	approveResp := a.do("POST", base+"/kill-switch-release-requests/"+relReq.ID+"/approve", platTok, nil)
	if approveResp.status != http.StatusOK {
		t.Fatalf("platform approve_release: status=%d body=%s", approveResp.status, approveResp.body)
	}

	page, status := getPlatformActions(t, a, tenantTok)
	if status != http.StatusOK {
		t.Fatalf("read: status=%d", status)
	}
	// Only the platform approve_release is a SUBJECT row - ADR §5.1's own
	// item list scope (tenant-own engage/request_release are NOT platform
	// actions and stay out of this endpoint's own item list, per AT-7 -
	// they are reachable via the existing GET /v1/admin/audit-log).
	if len(page.Items) != 1 {
		t.Fatalf("expected exactly 1 subject (approve_release) row as an ITEM, got %+v", page.Items)
	}
	approveRelease := page.Items[0]
	if approveRelease.Action != "payments_kill_switch.approve_release" {
		t.Fatalf("expected the one item to be approve_release, got %+v", approveRelease)
	}
	if !chainHasAction(approveRelease.ApprovalChain, "payments_kill_switch.engage") {
		t.Errorf("F-5: expected the approve_release row's chain to include the TENANT's own engage row, got %+v", approveRelease.ApprovalChain)
	}
	if !chainHasAction(approveRelease.ApprovalChain, "payments_kill_switch.request_release") {
		t.Errorf("F-5: expected the approve_release row's chain to include the TENANT's own request_release row, got %+v", approveRelease.ApprovalChain)
	}
}

// TestPlatformActionsAuditAPI_ChainSpansPages is F-5's second required
// case: with ?limit=1, only the most recent row (approve_release) appears
// as a page ITEM, but its approval_chain must still include the earlier
// engage/request_release rows, which are NOT on this page at all - the
// chain source query (platformActionsChainSourceQuery) has no LIMIT/
// OFFSET, unlike the page query.
func TestPlatformActionsAuditAPI_ChainSpansPages(t *testing.T) {
	a := newAuditAPI(t, nil)
	tenantA := a.tenant()
	platX := a.platformAdmin()
	platY := a.platformAdmin()
	tokX := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	tokY := a.token(platY, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	fourEyesEngageAndRelease(t, a, tenantA, tokX, tokY, "f5-page-incident", "f5-page-release")

	tenantAAdmin := a.tenantStaff(tenantA, "tenant_admin")
	tenantATok := a.token(tenantAAdmin, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)

	// Sanity: without pagination, all 3 rows appear.
	full, status := getPlatformActions(t, a, tenantATok)
	if status != http.StatusOK || full.Total != 3 {
		t.Fatalf("setup: expected 3 total subject rows, got status=%d total=%d", status, full.Total)
	}

	// limit=1: exactly one ITEM (the most recent, approve_release, per
	// ORDER BY created_at DESC), but its OWN chain must still name the
	// engage and request_release rows that are NOT on this page.
	page, status := getPlatformActionsPath(t, a, tenantATok, "/v1/admin/audit-log/platform-actions?limit=1&offset=0")
	if status != http.StatusOK {
		t.Fatalf("paginated read: status=%d", status)
	}
	if len(page.Items) != 1 {
		t.Fatalf("expected exactly 1 item with limit=1, got %+v", page.Items)
	}
	if page.Total != 3 {
		t.Fatalf("expected total=3 regardless of limit, got %d", page.Total)
	}
	item := page.Items[0]
	if item.Action != "payments_kill_switch.approve_release" {
		t.Fatalf("expected the single (most recent) item to be approve_release, got %+v", item)
	}
	if !chainHasAction(item.ApprovalChain, "payments_kill_switch.engage") {
		t.Errorf("F-5: expected the chain to include engage even though it is on a DIFFERENT page, got %+v", item.ApprovalChain)
	}
	if !chainHasAction(item.ApprovalChain, "payments_kill_switch.request_release") {
		t.Errorf("F-5: expected the chain to include request_release even though it is on a DIFFERENT page, got %+v", item.ApprovalChain)
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
// TestExistingAuditLogAPI_UnaffectedBySubjectRows is F-4 (code review):
// ADR 0104 §7's "Readers unchanged (R)" for GET /v1/admin/audit-log -
// with a genuine subject row present for the SAME tenant (via a real
// platform kill-switch engage), the existing tenant-scoped reader
// (queryAuditLog's own `tenant_id = $1` filter) must never return it,
// since a subject row's tenant_id is always NULL (migration 0109's
// platform-only CHECK).
func TestExistingAuditLogAPI_UnaffectedBySubjectRows(t *testing.T) {
	a := newAuditAPI(t, nil)
	tenantA := a.tenant()
	platX := a.platformAdmin()
	platY := a.platformAdmin()
	tokX := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	tokY := a.token(platY, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	fourEyesEngageAndRelease(t, a, tenantA, tokX, tokY, "f4-existing-reader-incident", "f4-existing-reader-released")

	// Sanity: the subject rows really exist and are visible through the
	// NEW platform-actions endpoint.
	tenantAAdmin := a.tenantStaff(tenantA, "tenant_admin")
	tenantATok := a.token(tenantAAdmin, tenantA, auth.RoleTenantAdmin, auth.PrincipalStaff)
	page, status := getPlatformActions(t, a, tenantATok)
	if status != http.StatusOK || page.Total == 0 {
		t.Fatalf("setup: expected the platform-actions endpoint to see the subject rows, status=%d total=%d", status, page.Total)
	}

	// The EXISTING /v1/admin/audit-log endpoint must never see them.
	resp := a.do("GET", "/v1/admin/audit-log", tenantATok, nil)
	if resp.status != http.StatusOK {
		t.Fatalf("existing audit-log read: status=%d body=%s", resp.status, resp.body)
	}
	var existing struct {
		Items []struct {
			Action string `json:"action"`
		} `json:"items"`
		Total int `json:"total"`
	}
	resp.decode(t, &existing)
	for _, e := range existing.Items {
		if strings.HasPrefix(e.Action, "payments_kill_switch.") {
			t.Fatalf("F-4: expected the existing tenant audit-log reader to never see a platform-scope subject row, got %+v", e)
		}
	}
}

func TestPlatformActionsAuditAPI_NameLookupNeverSelectsEmail(t *testing.T) {
	if strings.Contains(strings.ToLower(staffDisplayNameLookupQuery), "email") {
		t.Fatalf("the staff display-name lookup query must never select email, got: %s", staffDisplayNameLookupQuery)
	}
	if !strings.Contains(staffDisplayNameLookupQuery, "display_name") || !strings.Contains(staffDisplayNameLookupQuery, "staff_users") {
		t.Fatalf("expected the query to select id, display_name from staff_users, got: %s", staffDisplayNameLookupQuery)
	}
}

// TestPlatformActionsAuditAPI_RouteTarget is F-9 (code review): a
// dedicated "route target" test for AT-5/ADR §4 - the stored
// subject_tenant_id must equal the PATH target (never a client-supplied
// body value), and a body that tries to carry its own tenant_id is
// refused (decodeJSON's unknown-field rejection, the same convention
// payments_kill_switch_api_integration_test.go's own
// TestPaymentsKillSwitchAPI_UnknownFieldsRefused already relies on).
func TestPlatformActionsAuditAPI_RouteTarget(t *testing.T) {
	a := newAuditAPI(t, nil)
	tenantA := a.tenant()
	platX := a.platformAdmin()
	tokX := a.token(platX, uuid.Nil, auth.RolePlatformAdmin, auth.PrincipalStaff)
	base := "/v1/admin/tenants/" + tenantA.String() + "/payments"

	// A body carrying its own "tenant_id" is refused outright (unknown
	// field) - the target can ONLY ever come from the path.
	badBody := a.do("POST", base+"/kill-switches", tokX, map[string]any{
		"provider_scope": "*", "operation_scope": "deposit", "reason_code": "f9-route-target", "tenant_id": tenantA.String(),
	})
	if badBody.status != http.StatusBadRequest {
		t.Fatalf("expected a body carrying tenant_id to be refused with 400, got status=%d body=%s", badBody.status, badBody.body)
	}

	// The success path: the STORED subject_tenant_id equals the path
	// target exactly.
	engageResp := a.do("POST", base+"/kill-switches", tokX, map[string]any{
		"provider_scope": "*", "operation_scope": "deposit", "reason_code": "f9-route-target-ok",
	})
	if engageResp.status != http.StatusOK {
		t.Fatalf("engage: status=%d body=%s", engageResp.status, engageResp.body)
	}

	var storedSubjectTenant uuid.UUID
	if err := a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT subject_tenant_id FROM audit_log WHERE tenant_id IS NULL AND action = 'payments_kill_switch.engage' AND actor_id = $1 ORDER BY created_at DESC LIMIT 1`, platX).Scan(&storedSubjectTenant)
	}); err != nil {
		t.Fatalf("read stored subject_tenant_id: %v", err)
	}
	if storedSubjectTenant != tenantA {
		t.Fatalf("F-9: expected the stored subject_tenant_id to equal the path target %s, got %s", tenantA, storedSubjectTenant)
	}
}
