package httpserver

// ADR 0119 slice S4a: the liveness verdict (pure) and the shape of the liveness query (DB-free).

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func TestStaffLivenessVerdict_EachRuleAlone(t *testing.T) {
	live := staffLivenessRow{status: "active", role: string(auth.RoleTenantAdmin), staffVersion: 0, factorUsable: true}
	if got := staffLivenessVerdict(live, auth.RoleTenantAdmin, 0); got != observability.StaffLivenessReasonOK {
		t.Fatalf("a live row must pass, got %s", got)
	}
	for _, tc := range []struct {
		name   string
		mut    func(*staffLivenessRow)
		role   auth.Role
		sv     int64
		reason observability.StaffLivenessReason
	}{
		{"revoked", func(r *staffLivenessRow) { r.revoked = true }, auth.RoleTenantAdmin, 0, observability.StaffLivenessReasonRevoked},
		{"absolute expiry", func(r *staffLivenessRow) { r.absoluteExpired = true }, auth.RoleTenantAdmin, 0, observability.StaffLivenessReasonAbsoluteExpired},
		{"suspended", func(r *staffLivenessRow) { r.status = "suspended" }, auth.RoleTenantAdmin, 0, observability.StaffLivenessReasonStaffNotActive},
		{"offboarded", func(r *staffLivenessRow) { r.status = "offboarded" }, auth.RoleTenantAdmin, 0, observability.StaffLivenessReasonStaffNotActive},
		{"status case", func(r *staffLivenessRow) { r.status = "Active" }, auth.RoleTenantAdmin, 0, observability.StaffLivenessReasonStaffNotActive},
		{"role changed", func(r *staffLivenessRow) { r.role = string(auth.RoleSupport) }, auth.RoleTenantAdmin, 0, observability.StaffLivenessReasonRoleMismatch},
		{"token role differs", func(*staffLivenessRow) {}, auth.RolePlatformAdmin, 0, observability.StaffLivenessReasonRoleMismatch},
		{"token sv ahead", func(*staffLivenessRow) {}, auth.RoleTenantAdmin, 1, observability.StaffLivenessReasonVersionMismatch},
		{"row sv ahead", func(r *staffLivenessRow) { r.staffVersion = 2 }, auth.RoleTenantAdmin, 0, observability.StaffLivenessReasonVersionMismatch},
		{"factor unusable", func(r *staffLivenessRow) { r.factorUsable = false }, auth.RoleTenantAdmin, 0, observability.StaffLivenessReasonFactorUnusable},
	} {
		row := live
		tc.mut(&row)
		if got := staffLivenessVerdict(row, tc.role, tc.sv); got != tc.reason {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.reason)
		}
	}
}

// The query reads the session by id AND principal (never by id alone), only staff rows, in the token's own tenant
// (explicitly, not only through RLS), the database clock for the absolute expiry, the shared usable-factor rule, and
// never the idle expires_at.
func TestStaffLivenessSQL_Shape(t *testing.T) {
	q := staffLivenessSQL
	for _, want := range []string{
		"s.id = $1 AND s.principal_id = $2 AND s.principal_type = 'staff'",
		"s.tenant_id IS NOT DISTINCT FROM $3::uuid",
		"su.tenant_id IS NOT DISTINCT FROM $3::uuid",
		"s.absolute_expires_at <= now()",
		"s.revoked_at IS NOT NULL",
		usableFactorPredicate,
		"JOIN staff_users su ON su.id = s.principal_id",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("liveness query lacks %q", want)
		}
	}
	if strings.Contains(q, "expires_at,") || strings.Contains(q, "s.expires_at") {
		t.Error("liveness must not read the idle expires_at")
	}
	if usableFactorPredicate != `f.status = 'active' AND f.hard_locked_at IS NULL` {
		t.Error("the usable-factor rule changed")
	}
	if staffLivenessStatementTimeout.Milliseconds() != 500 {
		t.Errorf("statement timeout %s, ADR 0119 section 7.3 says 500 ms", staffLivenessStatementTimeout)
	}
}

// The liveness middleware on its own (DB-free): a request without a verified staff session in its context - whatever
// the principal claims - is the single 401 session_revoked, decided before any database access (so a nil pool is
// never reached: a fault would be 503). The handler never runs.
func TestRequireLiveStaffSession_NoAssuranceIsRefusedWithoutDB(t *testing.T) {
	reached := false
	h := requireLiveStaffSession(Deps{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	staffTC := tenant.Context{TenantID: uuid.New(), Role: string(auth.RoleSupport), PrincipalType: string(auth.PrincipalStaff), Subject: uuid.NewString()}
	for name, tc := range map[string]*tenant.Context{
		"no context":                  nil,
		"staff principal, no session": &staffTC,
		"player principal":            {TenantID: uuid.New(), Role: string(auth.RolePlayer), PrincipalType: string(auth.PrincipalPlayer), Subject: uuid.NewString()},
		"staff, non-uuid subject":     {Role: string(auth.RoleSupport), PrincipalType: string(auth.PrincipalStaff), Subject: "admin-1"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/staff/me/mfa", nil)
		if tc != nil {
			req = req.WithContext(tenant.WithContext(req.Context(), *tc))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), `"code":"session_revoked"`) {
			t.Errorf("%s: %d %s, want 401 session_revoked", name, rec.Code, rec.Body)
		}
	}
	if reached {
		t.Fatal("the handler ran")
	}
}
