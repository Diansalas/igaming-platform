package auth

import (
	"errors"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ADR 0119 section 7.1 (slice S3): staff tokens carry every assurance claim with aal 2; Issue refuses staff; Verify
// refuses a staff token without the claims (or with another aal) and a non-staff token that carries them.

func TestIssue_RefusesStaffPrincipal(t *testing.T) {
	issuer := testIssuer(t)
	if _, err := issuer.Issue(uuid.NewString(), uuid.New(), RoleSupport, PrincipalStaff, time.Hour); !errors.Is(err, ErrStaffAssuranceRequired) {
		t.Fatalf("Issue must refuse a staff token: %v", err)
	}
}

func TestIssueStaff_RoundTripsTheAssuranceClaims(t *testing.T) {
	issuer := testIssuer(t)
	p := testStaffParams(uuid.New(), uuid.New(), RoleCompliance)
	p.AuthTime = time.Unix(1_800_000_000, 0)
	p.MFAAt = time.Unix(1_800_000_100, 0)
	p.StaffVersion = 7
	tok, err := issuer.IssueStaff(p, 15*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, err := issuer.Verify(tok)
	if err != nil {
		t.Fatal(err)
	}
	if c.Subject != p.StaffID.String() || c.TenantID != p.TenantID || c.Role != RoleCompliance || c.PrincipalType != PrincipalStaff ||
		c.SessionID != p.SessionID.String() || c.AAL != StaffAAL || len(c.AMR) != 2 || c.AMR[0] != "pwd" || c.AMR[1] != "otp" ||
		c.AuthTime.Unix() != 1_800_000_000 || c.MFAAt.Unix() != 1_800_000_100 || c.StaffVersion == nil || *c.StaffVersion != 7 {
		t.Fatalf("claims: %+v", c)
	}
	if got := c.ExpiresAt.Sub(c.IssuedAt.Time); got != 15*time.Minute {
		t.Fatalf("ttl %s", got)
	}
}

func TestIssueStaff_RefusesIncompleteAssurance(t *testing.T) {
	issuer := testIssuer(t)
	for name, mut := range map[string]func(*StaffTokenParams){
		"no staff id":   func(p *StaffTokenParams) { p.StaffID = uuid.Nil },
		"no session id": func(p *StaffTokenParams) { p.SessionID = uuid.Nil },
		"aal 1":         func(p *StaffTokenParams) { p.AAL = 1 },
		"aal 3":         func(p *StaffTokenParams) { p.AAL = 3 },
		"no amr":        func(p *StaffTokenParams) { p.AMR = nil },
		"no auth_time":  func(p *StaffTokenParams) { p.AuthTime = time.Time{} },
		"no mfa_at":     func(p *StaffTokenParams) { p.MFAAt = time.Time{} },
		"negative sv":   func(p *StaffTokenParams) { p.StaffVersion = -1 },
		"no role":       func(p *StaffTokenParams) { p.Role = "" },
	} {
		p := testStaffParams(uuid.New(), uuid.New(), RoleSupport)
		mut(&p)
		if _, err := issuer.IssueStaff(p, time.Hour); !errors.Is(err, ErrStaffAssuranceRequired) {
			t.Errorf("%s: want ErrStaffAssuranceRequired, got %v", name, err)
		}
	}
}

// signRaw signs arbitrary claims with the test issuer's key, bypassing Issue / IssueStaff (a forged-shape token).
func signRaw(t *testing.T, issuer *Issuer, c Claims) string {
	t.Helper()
	now := time.Now()
	c.RegisteredClaims = jwt.RegisteredClaims{Subject: uuid.NewString(), Issuer: issuer.issuer, Audience: jwt.ClaimStrings{issuer.audience},
		IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(time.Hour)), ID: uuid.NewString()}
	tok, err := issuer.sign(c)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestVerify_StaffTokenNeedsEveryAssuranceClaim(t *testing.T) {
	issuer := testIssuer(t)
	sv := int64(0)
	now := jwt.NewNumericDate(time.Now())
	full := Claims{TenantID: uuid.New(), Role: RoleSupport, PrincipalType: PrincipalStaff, SessionID: uuid.NewString(), AAL: 2,
		AMR: []string{"pwd", "otp"}, AuthTime: now, MFAAt: now, StaffVersion: &sv}
	if _, err := issuer.Verify(signRaw(t, issuer, full)); err != nil {
		t.Fatalf("a complete staff token must verify: %v", err)
	}
	for name, mut := range map[string]func(*Claims){
		"pre-S3 shape (no assurance)": func(c *Claims) {
			c.SessionID, c.AAL, c.AMR, c.AuthTime, c.MFAAt, c.StaffVersion = "", 0, nil, nil, nil, nil
		},
		"no sid":       func(c *Claims) { c.SessionID = "" },
		"sid not uuid": func(c *Claims) { c.SessionID = "abc" },
		"nil sid":      func(c *Claims) { c.SessionID = uuid.Nil.String() },
		"aal 1":        func(c *Claims) { c.AAL = 1 },
		"aal 3":        func(c *Claims) { c.AAL = 3 },
		"no amr":       func(c *Claims) { c.AMR = nil },
		"no auth_time": func(c *Claims) { c.AuthTime = nil },
		"no mfa_at":    func(c *Claims) { c.MFAAt = nil },
		"no sv":        func(c *Claims) { c.StaffVersion = nil },
		"negative sv":  func(c *Claims) { n := int64(-1); c.StaffVersion = &n },
	} {
		c := full
		mut(&c)
		if _, err := issuer.Verify(signRaw(t, issuer, c)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: want ErrInvalidToken, got %v", name, err)
		}
	}
	// A player or service token carrying any assurance claim is refused too.
	for _, pt := range []PrincipalType{PrincipalPlayer, PrincipalService} {
		c := full
		c.PrincipalType = pt
		if _, err := issuer.Verify(signRaw(t, issuer, c)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s token with assurance claims: want ErrInvalidToken, got %v", pt, err)
		}
		c = Claims{TenantID: uuid.New(), Role: RolePlayer, PrincipalType: pt, AAL: 2}
		if _, err := issuer.Verify(signRaw(t, issuer, c)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s token with an aal claim: want ErrInvalidToken, got %v", pt, err)
		}
	}
}
