//go:build integration

package httpserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

// withKillSwitchRoutes serves the same database and token issuer with a payment orchestrator, which
// is what mounts the kill-switch routes (the cgAPI harness has none).
func withKillSwitchRoutes(a *cgAPI) *ksAPI {
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	orch := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock},
		payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)}).WithPayoutDestinations(pitest.Shared())
	srv := httptest.NewServer(New(Deps{
		Logger: slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError})), DB: a.pool, AuthIssuer: a.issuer,
		ServiceName: "pa-test", AccessTokenTTL: time.Hour, RefreshTokenTTL: time.Hour,
		PersonResolver: identityresolution.NewMockPersonResolver(), PaymentOrchestrator: orch,
	}))
	a.t.Cleanup(srv.Close)
	return &ksAPI{t: a.t, pool: a.pool, issuer: a.issuer, srv: srv}
}

// ADR 0114 S0 (security BLOCKER): the platform-actions audit route identifies the platform actor for
// kill-switch actions only (ADR 0104). K1, K2, K3 platform rows carry subject_tenant_id too and must
// show scope only. D-0104-1 (ADR 0114 section 9.5) is the open owner decision on the kill switch.
func TestStaffIdentityHTTP_PlatformActionsAudit_ActorIdentifiedForKillSwitchOnly(t *testing.T) {
	w := newFRWorld(t, true)
	pa, pb, approver, noGrant := w.pa, w.pb, w.grantApprover, w.actingNoGrant
	// K1: platform requester + platform grantee through the API, approved by another platform principal.
	untilT := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	res := w.a.do("POST", "/v1/admin/tenants/"+w.tenant.String()+"/capability-grants/requests", w.tok(pa), map[string]any{
		"grantee_staff_id": noGrant.String(), "capability": "ledger_adjustment:initiate", "reason_code": "audit-test", "valid_until": untilT,
	})
	if res.status != http.StatusCreated {
		t.Fatalf("K1 request: %d %s", res.status, res.body)
	}
	var rq struct {
		ID string `json:"id"`
	}
	res.decode(t, &rq)
	if r := w.a.do("POST", "/v1/admin/tenants/"+w.tenant.String()+"/capability-grants/requests/"+rq.ID+"/approve", w.tok(approver), map[string]any{"reason_code": "audit-test"}); r.status != http.StatusOK {
		t.Fatalf("K1 approve: %d %s", r.status, r.body)
	}
	// K2 and K3 denied rows: a platform principal with no grant for the tenant.
	nobody := w.a.staff(uuid.Nil, "platform_admin")
	if st, _ := w.submit(nobody); st != http.StatusForbidden {
		t.Fatalf("K2 denied seed: %d", st)
	}
	if st, _, _ := w.request(nobody, w.disputedDeposit(), nil); st != http.StatusForbidden {
		t.Fatalf("K3 denied seed: %d", st)
	}
	// kill switch engaged by a platform principal: the one family that stays identified.
	if r := withKillSwitchRoutes(w.a).do("POST", "/v1/admin/tenants/"+w.tenant.String()+"/payments/kill-switches", w.tok(pb),
		map[string]any{"provider_scope": "*", "operation_scope": "deposit", "reason_code": "audit-test"}); r.status != http.StatusOK {
		t.Fatalf("kill switch engage: %d %s", r.status, r.body)
	}
	for name, tok := range map[string]string{"tenant_admin": w.tok(w.ta), "compliance": w.tok(w.comp)} {
		r := w.a.do("GET", "/v1/admin/audit-log/platform-actions?limit=200", tok, nil)
		if r.status != http.StatusOK {
			t.Fatalf("%s: %d %s", name, r.status, r.body)
		}
		sidAbsent(t, name+" platform-actions", r.body, pa, approver, noGrant, nobody, w.acting)
		var page struct {
			Items []struct {
				Action string `json:"action"`
				Actor  struct {
					Scope       string  `json:"scope"`
					StaffID     string  `json:"staff_id"`
					DisplayName *string `json:"display_name"`
				} `json:"actor"`
			} `json:"items"`
		}
		if err := json.Unmarshal(r.body, &page); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, it := range page.Items {
			fam := strings.SplitN(it.Action, ".", 2)[0]
			seen[fam] = true
			if fam == "payments_kill_switch" {
				if it.Actor.StaffID != pb.String() {
					t.Errorf("%s: kill-switch actor must stay identified, got %q", name, it.Actor.StaffID)
				}
				continue
			}
			if it.Actor.StaffID != "" || it.Actor.DisplayName != nil || it.Actor.Scope != "platform" {
				t.Errorf("%s: %s must be scope-only, got %+v", name, it.Action, it.Actor)
			}
		}
		for _, fam := range []string{"capability_grant", "ledger_adjustment", "payments_kill_switch"} {
			if !seen[fam] {
				t.Errorf("%s: fixture non-vacuity: no %s row in %v", name, fam, seen)
			}
		}
	}
}
