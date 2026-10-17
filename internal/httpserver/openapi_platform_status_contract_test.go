package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// The route is registered through staffRoute with step-up class none and the dedicated permission as the inner gate
// (pinned on the source, because the layered tenant check would otherwise hide a dropped permission).
func TestPlatformStatusRoute_RegisteredWithTheDedicatedPermissionAndClassNone(t *testing.T) {
	src, err := os.ReadFile("platform_status_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	want := `staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermPlatformStatusRead)(newPlatformStatusHandler(deps)))`
	if !strings.Contains(string(src), want) {
		t.Fatalf("the route must be registered exactly as %s", want)
	}
}

// The handler's own platform-scope gate: a tenant-scoped principal is refused 403 even if it somehow held the permission
// (the explicit tc.TenantID == uuid.Nil layer), a platform principal passes, and a malformed subject is 401.
func TestPlatformAlertCaller_RefusesATenantScopedPrincipal(t *testing.T) {
	call := func(tc tenant.Context) (*httptest.ResponseRecorder, bool) {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/platform/status", nil)
		req = req.WithContext(tenant.WithContext(req.Context(), tc))
		rec := httptest.NewRecorder()
		_, _, _, ok := platformAlertCaller(rec, req)
		return rec, ok
	}
	platform := tenant.Context{TenantID: uuid.Nil, Role: "platform_admin", PrincipalType: "staff", Subject: uuid.NewString()}
	if rec, ok := call(platform); !ok || rec.Code != http.StatusOK {
		t.Fatalf("a platform principal must pass: ok=%v code=%d", ok, rec.Code)
	}
	scoped := platform
	scoped.TenantID = uuid.New()
	if rec, ok := call(scoped); ok || rec.Code != http.StatusForbidden {
		t.Fatalf("a tenant-scoped principal must be refused 403: ok=%v code=%d", ok, rec.Code)
	}
	bad := platform
	bad.Subject = "not-a-uuid"
	if rec, ok := call(bad); ok || rec.Code != http.StatusUnauthorized {
		t.Fatalf("a malformed subject must be 401: ok=%v code=%d", ok, rec.Code)
	}
}

// F6 / SA-3: the platform status route is mounted and documented both ways, with the golden step-up class none and
// the response DTO field for field equal to the PlatformStatus schema. DB-free.

func TestOpenAPI_PlatformStatus_RouteDocumentedBothWays(t *testing.T) {
	spec := launchSpec(t)
	src, err := os.ReadFile("platform_status_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|PATCH|DELETE) (/v1/[^"]*)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) != 1 || routes[0][1] != "GET" || routes[0][2] != "/v1/admin/platform/status" {
		t.Fatalf("expected exactly GET /v1/admin/platform/status mounted, got %v", routes)
	}
	block := openAPIPathBlock(t, spec, "/v1/admin/platform/status")
	if !strings.HasPrefix(block, "    get:\n") || strings.Contains(block, "\n    post:") || strings.Contains(block, "\n    put:") {
		t.Fatalf("only get may be documented for the status path:\n%s", block)
	}
	for _, want := range []string{"bearerAuth", `"401"`, `"403"`, "platform_status:read", "PlatformStatus"} {
		if !strings.Contains(block, want) {
			t.Errorf("OpenAPI block missing %q", want)
		}
	}
	if got := staffRouteGolden["GET /v1/admin/platform/status"]; got != "none" {
		t.Fatalf("golden class = %q, a read is none", got)
	}
}

func TestOpenAPI_PlatformStatus_DTOMatchesSchema(t *testing.T) {
	spec := launchSpec(t)
	top := rgSchemaBlock(t, spec, "PlatformStatus")
	raw, err := json.Marshal(platformStatusResponse{})
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	// Top-level properties, then each nested object's properties, each documented under the schema.
	for _, key := range []string{"build", "migrations", "database_environment"} {
		if !regexp.MustCompile(`(?m)^        ` + key + `:`).MatchString(top) {
			t.Errorf("schema lacks top-level property %q", key)
		}
		if _, ok := m[key]; !ok {
			t.Errorf("DTO lacks %q", key)
		}
	}
	if len(m) != 3 {
		t.Errorf("DTO has %d top-level fields, the schema documents 3", len(m))
	}
	for key, fields := range map[string][]string{
		"build": {"commit"}, "migrations": {"head_version", "applied_count"}, "database_environment": {"anchor", "recorded"},
	} {
		if len(m[key]) != len(fields) {
			t.Errorf("%s: DTO has %d fields, want %v", key, len(m[key]), fields)
		}
		for _, f := range fields {
			if _, ok := m[key][f]; !ok {
				t.Errorf("%s: DTO lacks %q", key, f)
			}
			if !strings.Contains(top, f+":") {
				t.Errorf("%s: schema lacks %q", key, f)
			}
		}
	}
	// The anchor enum in the schema is exactly the closed token set of the handler.
	enum := regexp.MustCompile(`anchor: \{ type: string, enum: \[([^\]]*)\]`).FindStringSubmatch(top)
	if enum == nil {
		t.Fatal("anchor enum missing from the schema")
	}
	got := map[string]bool{}
	for _, v := range strings.Split(enum[1], ",") {
		got[strings.TrimSpace(v)] = true
	}
	for _, in := range []string{"", "production", "staging", "development", "test", "anything-else"} {
		tok := platformStatusAnchor(in).Anchor
		if !got[tok] {
			t.Errorf("handler token %q (input %q) is not in the schema enum %v", tok, in, got)
		}
	}
	if len(got) != 6 {
		t.Errorf("schema enum has %d values, want the 6 handler tokens", len(got))
	}
}
