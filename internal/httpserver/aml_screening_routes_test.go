package httpserver

import (
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/stepup"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// lic18 (ledger-finance H-1 c): the four governed screening routes.
var amlScreeningRoutes = map[string]string{
	"GET /v1/admin/tenants/{tenantID}/screening/trigger-policy":                                      "none",
	"POST /v1/admin/tenants/{tenantID}/screening/trigger-policy":                                     "critical",
	"GET /v1/admin/tenants/{tenantID}/screening/policy":                                              "none",
	"POST /v1/admin/tenants/{tenantID}/screening/policy":                                             "critical",
	"GET /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests":                         "none",
	"POST /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests/{requestID}/resolution": "critical",
	"POST /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests/replace":                "critical",
}

func TestAMLScreeningRoutes_GoldenRegistrationAndPermissionsAreExact(t *testing.T) {
	n := 0
	for pat, class := range staffRouteGolden {
		if !strings.Contains(pat, "/screening/") {
			continue
		}
		n++
		if want, ok := amlScreeningRoutes[pat]; !ok || want != class {
			t.Errorf("%s: class %q, want %q (known %v)", pat, class, want, ok)
		}
		if strings.Contains(pat, "/aml/") {
			t.Errorf("%s: the A7 pin counts exactly the eleven /aml/ routes", pat)
		}
	}
	if n != len(amlScreeningRoutes) {
		t.Errorf("golden table has %d screening routes, want %d", n, len(amlScreeningRoutes))
	}
	b, err := os.ReadFile("aml_screening_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if got := len(regexp.MustCompile(`mux\.Handle\("(GET|POST) /v1/`).FindAllString(src, -1)); got != len(amlScreeningRoutes) {
		t.Fatalf("%d registrations, want %d", got, len(amlScreeningRoutes))
	}
	// Every write is step-up critical behind the hold-manage permission; every read is step-up none behind case-read.
	if c := strings.Count(src, "staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermAMLHoldManage)"); c != 4 {
		t.Errorf("%d critical routes behind PermAMLHoldManage, want 4", c)
	}
	if c := strings.Count(src, "staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseRead)"); c != 3 {
		t.Errorf("%d read routes behind PermAMLCaseRead, want 3", c)
	}
}

func TestAMLScreeningRoutes_TheSignerBackstopAgrees(t *testing.T) {
	if got := actorproof.StepUpOperations[actorproof.OpAMLScreeningTriggerPolicySet]; got != stepup.Critical {
		t.Errorf("trigger-policy set is %q in the signer registry, want critical", got)
	}
	// The resolution records through aml_screening:record, which is shared with the officer screening orchestrator and is
	// state-dependent (a match tightens, a clear loosens): the operation-keyed registry cannot say, so it is deliberately absent
	// and the HTTP gate (step-up critical on the route, pinned above) is the control.
	if _, ok := actorproof.StepUpOperations[aml.OpProofScreeningRecord]; ok {
		t.Error("aml_screening:record must stay out of the operation-keyed registry (shared, state-dependent); the route is the gate")
	}
}

func TestOpenAPI_AMLScreeningRoutesDocumented(t *testing.T) {
	spec := readSpec(t)
	for pat := range amlScreeningRoutes {
		parts := strings.SplitN(pat, " ", 2)
		block := openAPIPathBlock(t, spec, parts[1])
		if !strings.Contains(block, "    "+strings.ToLower(parts[0])+":\n") {
			t.Errorf("%s is mounted but not documented", pat)
		}
		if parts[0] == "POST" && !strings.Contains(block, "step-up `critical`") {
			t.Errorf("%s: the description must state the critical step-up", pat)
		}
	}
	for _, op := range documentedOps(spec, "/v1/admin/tenants/{tenantID}/screening/") {
		if _, ok := amlScreeningRoutes[op]; !ok {
			t.Errorf("%s is documented but not mounted", op)
		}
	}
}

// The handlers refuse malformed input before the service is reached and never take the actor or tenant from the body.
func TestAMLScreeningHandlers_RefuseBadInputBeforeTheService(t *testing.T) {
	tid, staff := uuid.New(), uuid.New()
	f := &fakeAML{}
	deps := Deps{AML: f}
	mux := http.NewServeMux()
	tc := tenant.Context{TenantID: tid, Subject: staff.String(), Role: string(auth.RoleCompliance), PrincipalType: string(auth.PrincipalStaff)}
	inject := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(tenant.WithContext(r.Context(), tc)))
		})
	}
	mux.Handle("GET /t/{tenantID}/policy", inject(amlHandler(deps, "p", amlTriggerPolicyRead)))
	mux.Handle("POST /t/{tenantID}/policy", inject(amlHandler(deps, "ps", amlTriggerPolicySet)))
	mux.Handle("POST /t/{tenantID}/persons/{personID}/requests/{requestID}/resolution", inject(amlHandler(deps, "r", amlScreeningResolve)))
	mux.Handle("GET /t/{tenantID}/persons/{personID}/requests", inject(amlHandler(deps, "l", amlScreeningRequests)))
	base := "/t/" + tid.String()
	id := uuid.New().String()
	jur := uuid.New().String()
	cases := []struct{ name, method, path, body string }{
		{"read without jurisdiction", "GET", base + "/policy", ""},
		{"read with a bad jurisdiction", "GET", base + "/policy?jurisdiction_id=x", ""},
		{"set with a bad time", "POST", base + "/policy", fmt.Sprintf(`{"jurisdiction_id":%q,"events":[],"gate_withdrawal":false,"effective_from":"yesterday","legal_review_reference":"R","reason_code":"r_1"}`, jur)},
		{"set with an alert threshold but no gate", "POST", base + "/policy", fmt.Sprintf(`{"jurisdiction_id":%q,"events":[],"gate_withdrawal":false,"gate_alert_after_seconds":600,"effective_from":"2030-01-01T00:00:00Z","legal_review_reference":"R","reason_code":"r_1"}`, jur)},
		{"set with a body tenant", "POST", base + "/policy", fmt.Sprintf(`{"tenant_id":%q,"jurisdiction_id":%q}`, id, jur)},
		{"resolve with a bad request id", "POST", base + "/persons/" + id + "/requests/zz/resolution", `{"outcome":"clear","reason_code":"r_1"}`},
		{"resolve with an unknown field", "POST", base + "/persons/" + id + "/requests/" + id + "/resolution", `{"outcome":"clear","reason_code":"r_1","actor":"x"}`},
		{"list with a bad person id", "GET", base + "/persons/zz/requests", ""},
	}
	for _, c := range cases {
		f.calls = nil
		rec := amlDo(mux, c.method, c.path, c.body)
		if rec.Code != http.StatusBadRequest && rec.Code != http.StatusNotFound {
			t.Errorf("%s: %d %s, want 400/404", c.name, rec.Code, rec.Body)
		}
		if len(f.calls) != 0 {
			t.Errorf("%s: the service was reached: %v", c.name, f.calls)
		}
	}
}
