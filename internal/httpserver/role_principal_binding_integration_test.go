//go:build integration

package httpserver

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// handSignedToken signs arbitrary claims with the test server's real signing key (kid k1): a token that never went
// through auth.Issue / auth.IssueStaff.
func handSignedToken(t *testing.T, principalType auth.PrincipalType, role auth.Role, tenantID uuid.UUID) string {
	t.Helper()
	now := time.Now()
	c := auth.Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject: uuid.NewString(), Issuer: "platform-api-test", Audience: jwt.ClaimStrings{"platform-api-test"},
			IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), ID: uuid.NewString(),
		},
		TenantID: tenantID, Role: role, PrincipalType: principalType,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString([]byte(testJWTSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// ADR 0119 S3 security review C1 (executed by the reviewer: 200 on GET /v1/admin/tenants): a staff role on a player,
// service or empty principal is refused at Issue and, hand-signed with the real key, answered 401 by every
// authenticated route, here the platform-admin tenant list.
func TestRolePrincipalBinding_StaffRoleOnNonStaffPrincipal_401(t *testing.T) {
	pool, issuer := testEnv(t)
	srv := newTestServer(t, pool, issuer)
	for _, pt := range []auth.PrincipalType{auth.PrincipalPlayer, auth.PrincipalService, ""} {
		if _, err := issuer.Issue(uuid.NewString(), uuid.Nil, auth.RolePlatformAdmin, pt, time.Hour); !errors.Is(err, auth.ErrRolePrincipalMismatch) {
			t.Fatalf("Issue(platform_admin, %q) must be refused, got %v", pt, err)
		}
		resp := getJSON(t, srv, "/v1/admin/tenants", handSignedToken(t, pt, auth.RolePlatformAdmin, uuid.Nil))
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("hand-signed %q principal with platform_admin: GET /v1/admin/tenants answered %d, want 401", pt, resp.StatusCode)
		}
	}
	// Control: the same route with a real aal2 platform-admin staff token is 200, so the 401s are the binding.
	admin := mustCreateStaff(t, pool, uuid.Nil, "platform_admin", "admin-password-1")
	tokens := mustLoginStaff(t, srv, "", admin.Email, "admin-password-1")
	resp := getJSON(t, srv, "/v1/admin/tenants", tokens.AccessToken)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control: platform admin GET /v1/admin/tenants answered %d", resp.StatusCode)
	}
}
