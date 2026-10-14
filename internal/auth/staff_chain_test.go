package auth

// ADR 0119 slice S4a: the closed staff role set (S3 re-review condition S3-R12), the staff session context and
// RequireAAL2.

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// The closed set equals identity.StaffRole's values and the staff_users.role CHECK of the latest migration that
// redefines it. A role added in one place only fails here.
func TestStaffRoles_ClosedSet_ParityWithIdentityAndDatabase(t *testing.T) {
	var got []string
	for _, r := range StaffRoles() {
		got = append(got, string(r))
	}
	sort.Strings(got)
	ident := []string{
		string(identity.StaffRolePlatformAdmin), string(identity.StaffRoleTenantAdmin), string(identity.StaffRoleSupport),
		string(identity.StaffRoleCompliance), string(identity.StaffRoleFinance), string(identity.StaffRoleRiskManager),
		string(identity.StaffRolePromotionsManager), string(identity.StaffRoleBonusOperations),
	}
	sort.Strings(ident)
	if strings.Join(got, ",") != strings.Join(ident, ",") {
		t.Errorf("auth staff roles %v, identity staff roles %v", got, ident)
	}
	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.up.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	sort.Strings(files)
	checkRe := regexp.MustCompile(`(?s)staff_users_role_check\s+CHECK\s*\(\s*role\s+IN\s*\(([^)]*)\)`)
	var db []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if m := checkRe.FindAllStringSubmatch(string(b), -1); len(m) > 0 {
			db = nil
			for _, v := range strings.Split(m[len(m)-1][1], ",") {
				db = append(db, strings.Trim(strings.TrimSpace(v), "'"))
			}
		}
	}
	sort.Strings(db)
	if strings.Join(got, ",") != strings.Join(db, ",") {
		t.Errorf("auth staff roles %v, latest staff_users_role_check %v", got, db)
	}
	if StaffRoles()[0] = "tampered"; IsStaffRole("tampered") {
		t.Error("StaffRoles must return a copy")
	}
	if IsStaffRole(RolePlayer) || IsStaffRole("") || IsStaffRole("Platform_Admin") {
		t.Error("player, empty and case variants are not staff roles")
	}
}

// S3-R12: IssueStaff, Issue and Verify bind a staff principal to the closed set; any other role string is refused at
// mint time and at verification time (not merely permission-less).
func TestStaffPrincipal_RoleOutsideClosedSetRefused(t *testing.T) {
	issuer := testIssuer(t)
	for _, role := range StaffRoles() {
		if _, err := issuer.IssueStaff(testStaffParams(uuid.New(), uuid.New(), role), time.Hour); err != nil {
			t.Errorf("IssueStaff(%s): %v", role, err)
		}
	}
	sv := int64(0)
	now := jwt.NewNumericDate(time.Now())
	for _, role := range []Role{"made_up_role", "PLATFORM_ADMIN", "platform_admin ", "admin", RolePlayer} {
		if _, err := issuer.IssueStaff(testStaffParams(uuid.New(), uuid.New(), role), time.Hour); !errors.Is(err, ErrRolePrincipalMismatch) {
			t.Errorf("IssueStaff(%q) must be refused with ErrRolePrincipalMismatch, got %v", role, err)
		}
		tok := signRaw(t, issuer, Claims{TenantID: uuid.New(), Role: role, PrincipalType: PrincipalStaff, SessionID: uuid.NewString(),
			AAL: StaffAAL, AMR: []string{"pwd", "otp"}, AuthTime: now, MFAAt: now, StaffVersion: &sv})
		if _, err := issuer.Verify(tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("Verify of a hand-signed staff token with role %q must be refused, got %v", role, err)
		}
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		Middleware(issuer)(okHandler()).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("role %q: the middleware answered %d, want 401", role, rec.Code)
		}
	}
}

// Middleware attaches the verified staff session to staff requests only.
func TestMiddleware_AttachesStaffSessionForStaffTokensOnly(t *testing.T) {
	issuer := testIssuer(t)
	p := testStaffParams(uuid.New(), uuid.New(), RoleFinance)
	p.StaffVersion = 3
	// S4b (S3-R13): coherent times at verification.
	base := time.Now().Add(-time.Hour).Truncate(time.Second)
	p.AuthTime = base
	p.MFAAt = base.Add(60 * time.Second)
	staff, err := issuer.IssueStaff(p, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	player, err := issuer.Issue(uuid.NewString(), uuid.New(), RolePlayer, PrincipalPlayer, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	var got StaffSession
	var ok bool
	h := Middleware(issuer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok = StaffSessionFromContext(r.Context())
	}))
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+staff)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !ok || got.SessionID != p.SessionID || got.AAL != StaffAAL || got.StaffVersion != 3 || got.AuthTime.Unix() != base.Unix() ||
		got.MFAAt.Unix() != base.Unix()+60 || len(got.AMR) != 2 {
		t.Fatalf("staff session: %+v %v", got, ok)
	}
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+player)
	h.ServeHTTP(httptest.NewRecorder(), req)
	if ok {
		t.Fatal("a player token must carry no staff session")
	}
	if _, ok := staffSessionFromClaims(&Claims{PrincipalType: PrincipalStaff, SessionID: "not-a-uuid"}); ok {
		t.Fatal("incomplete claims must not yield a staff session")
	}
	// Only a STAFF principal yields a staff session, even if every assurance claim is present (defence in depth
	// behind Verify, which refuses assurance claims on a non-staff token).
	sv, at := int64(0), jwt.NewNumericDate(time.Now())
	for _, pt := range []PrincipalType{PrincipalPlayer, PrincipalService, ""} {
		if _, ok := staffSessionFromClaims(&Claims{PrincipalType: pt, SessionID: uuid.NewString(), AAL: StaffAAL,
			AMR: []string{"pwd", "otp"}, AuthTime: at, MFAAt: at, StaffVersion: &sv}); ok {
			t.Fatalf("a %q principal must not yield a staff session", pt)
		}
	}
}

// RequireAAL2 refuses (401) anything but a staff principal with an aal2 session in its context.
func TestRequireAAL2(t *testing.T) {
	staffCtx := tenant.Context{TenantID: uuid.New(), Role: string(RoleSupport), PrincipalType: string(PrincipalStaff), Subject: uuid.NewString()}
	for _, tc := range []struct {
		name   string
		tc     *tenant.Context
		sess   *StaffSession
		status int
	}{
		{"staff aal2", &staffCtx, &StaffSession{SessionID: uuid.New(), AAL: StaffAAL}, http.StatusOK},
		{"staff aal1", &staffCtx, &StaffSession{SessionID: uuid.New(), AAL: 1}, http.StatusUnauthorized},
		{"staff aal3", &staffCtx, &StaffSession{SessionID: uuid.New(), AAL: 3}, http.StatusUnauthorized},
		{"staff without a session", &staffCtx, nil, http.StatusUnauthorized},
		{"no tenant context", nil, &StaffSession{SessionID: uuid.New(), AAL: StaffAAL}, http.StatusUnauthorized},
		{"player with a staff session value", &tenant.Context{Role: string(RolePlayer), PrincipalType: string(PrincipalPlayer)},
			&StaffSession{SessionID: uuid.New(), AAL: StaffAAL}, http.StatusUnauthorized},
	} {
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/x", nil)
		ctx := req.Context()
		if tc.tc != nil {
			ctx = tenant.WithContext(ctx, *tc.tc)
		}
		if tc.sess != nil {
			ctx = withStaffSession(ctx, *tc.sess)
		}
		rec := httptest.NewRecorder()
		RequireAAL2(okHandler()).ServeHTTP(rec, req.WithContext(ctx))
		if rec.Code != tc.status {
			t.Errorf("%s: %d, want %d", tc.name, rec.Code, tc.status)
		}
		if tc.status == http.StatusUnauthorized && !strings.Contains(rec.Header().Get("WWW-Authenticate"), "insufficient_user_authentication") {
			t.Errorf("%s: the refusal must carry the RFC 9470 error", tc.name)
		}
	}
}
