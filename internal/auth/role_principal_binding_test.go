package auth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ADR 0119 S3 security review C1: permissions are looked up by role alone, so a staff role on a player, service or
// unknown principal must never be minted (Issue) nor accepted (Verify), including a hand-signed token with a valid
// key. Before the fix a player-principal token with role platform_admin passed every admin permission check.

var everyStaffRole = []Role{RolePlatformAdmin, RoleTenantAdmin, RoleSupport, RoleCompliance, RoleFinance, RoleRiskManager,
	RolePromotionsManager, RoleBonusOperations}

func TestIssue_RefusesStaffRoleOnNonStaffPrincipal(t *testing.T) {
	issuer := testIssuer(t)
	for _, role := range append(append([]Role(nil), everyStaffRole...), "", "made_up") {
		for _, pt := range []PrincipalType{PrincipalPlayer, PrincipalService, "", "admin"} {
			if _, err := issuer.Issue(uuid.NewString(), uuid.New(), role, pt, time.Hour); !errors.Is(err, ErrRolePrincipalMismatch) {
				t.Errorf("Issue(%q, %q) must be refused, got %v", role, pt, err)
			}
		}
	}
	// The legitimate non-staff tokens still issue.
	for _, pt := range []PrincipalType{PrincipalPlayer, PrincipalService} {
		if _, err := issuer.Issue(uuid.NewString(), uuid.New(), RolePlayer, pt, time.Hour); err != nil {
			t.Errorf("Issue(player, %q): %v", pt, err)
		}
	}
	// IssueStaff refuses the player role.
	if _, err := issuer.IssueStaff(testStaffParams(uuid.New(), uuid.New(), RolePlayer), time.Hour); !errors.Is(err, ErrRolePrincipalMismatch) {
		t.Errorf("IssueStaff(player role) must be refused, got %v", err)
	}
}

// handSigned signs arbitrary claims with the issuer's real key: the attacker model is a token minted by any code path
// (or person) holding the signing key without going through Issue / IssueStaff.
func handSigned(t *testing.T, issuer *Issuer, c Claims) string {
	t.Helper()
	tok, err := issuer.sign(c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestVerify_RefusesHandSignedRolePrincipalMismatch(t *testing.T) {
	issuer := testIssuer(t)
	staffClaims := func(role Role) Claims {
		c := issuer.baseClaims(uuid.NewString(), uuid.Nil, role, PrincipalStaff, time.Hour)
		sv := int64(0)
		c.SessionID, c.AAL, c.AMR = uuid.NewString(), StaffAAL, []string{"pwd", "otp"}
		c.AuthTime, c.MFAAt, c.StaffVersion = jwt.NewNumericDate(time.Now()), jwt.NewNumericDate(time.Now()), &sv
		return c
	}
	cases := map[string]Claims{
		"player principal, platform_admin": issuer.baseClaims(uuid.NewString(), uuid.Nil, RolePlatformAdmin, PrincipalPlayer, time.Hour),
		"empty principal, platform_admin":  issuer.baseClaims(uuid.NewString(), uuid.Nil, RolePlatformAdmin, "", time.Hour),
		"service principal, finance":       issuer.baseClaims(uuid.NewString(), uuid.New(), RoleFinance, PrincipalService, time.Hour),
		"unknown principal, player role":   issuer.baseClaims(uuid.NewString(), uuid.New(), RolePlayer, "admin", time.Hour),
		"player principal, empty role":     issuer.baseClaims(uuid.NewString(), uuid.New(), "", PrincipalPlayer, time.Hour),
		"staff principal, player role":     staffClaims(RolePlayer),
		"staff principal, empty role":      staffClaims(""),
	}
	for _, role := range everyStaffRole {
		cases["player principal, "+string(role)] = issuer.baseClaims(uuid.NewString(), uuid.New(), role, PrincipalPlayer, time.Hour)
	}
	for name, c := range cases {
		tok := handSigned(t, issuer, c)
		if _, err := issuer.Verify(tok); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: Verify must refuse, got %v", name, err)
		}
		// Through the middleware the answer is 401 and the handler is never reached.
		reached := false
		h := Middleware(issuer)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))
		req := httptest.NewRequest(http.MethodGet, "/v1/admin/tenants", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized || reached {
			t.Errorf("%s: middleware answered %d (reached=%v), want 401", name, rec.Code, reached)
		}
	}
	// Control: a well-formed hand-signed staff token verifies (the refusals above are the binding, not the signing).
	if _, err := issuer.Verify(handSigned(t, issuer, staffClaims(RolePlatformAdmin))); err != nil {
		t.Fatalf("control staff token: %v", err)
	}
}
