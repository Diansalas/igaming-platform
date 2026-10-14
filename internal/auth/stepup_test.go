package auth

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

// ADR 0119 S4b: condition S3-R13 on staff tokens, the step-up policy and the refusal.

// signRaw signs claims with the test issuer's key, bypassing IssueStaff's own checks (what a key holder could mint).
func signRawS4b(t *testing.T, i *Issuer, c Claims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	tok.Header["kid"] = i.keys.ActiveKID()
	s, err := tok.SignedString(i.keys.ActiveSecret())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerify_StaffAssuranceCoherence_S3R13(t *testing.T) {
	i := testIssuer(t)
	now := time.Now()
	base := func() Claims {
		sv := int64(0)
		return Claims{
			RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString(), Issuer: i.issuer, Audience: jwt.ClaimStrings{i.audience},
				IssuedAt: jwt.NewNumericDate(now), ExpiresAt: jwt.NewNumericDate(now.Add(10 * time.Minute)), ID: uuid.NewString()},
			TenantID: uuid.New(), Role: RoleSupport, PrincipalType: PrincipalStaff, SessionID: uuid.NewString(), AAL: StaffAAL,
			AMR: []string{"pwd", "otp"}, AuthTime: jwt.NewNumericDate(now.Add(-time.Hour)), MFAAt: jwt.NewNumericDate(now.Add(-time.Minute)), StaffVersion: &sv,
		}
	}
	if _, err := i.Verify(signRawS4b(t, i, base())); err != nil {
		t.Fatalf("a coherent staff token: %v", err)
	}
	for name, mut := range map[string]func(*Claims){
		"amr pwd only":            func(c *Claims) { c.AMR = []string{"pwd"} },
		"amr unknown method":      func(c *Claims) { c.AMR = []string{"pwd", "sms"} },
		"mfa_at before auth_time": func(c *Claims) { c.MFAAt = jwt.NewNumericDate(now.Add(-2 * time.Hour)) },
		"mfa_at in the future":    func(c *Claims) { c.MFAAt = jwt.NewNumericDate(now.Add(StepUpClockSkew + 2*time.Second)) },
		"auth_time in the future": func(c *Claims) { c.AuthTime = jwt.NewNumericDate(now.Add(time.Minute)); c.MFAAt = c.AuthTime },
		"auth_time older than 12h": func(c *Claims) {
			c.AuthTime = jwt.NewNumericDate(now.Add(-12*time.Hour - time.Minute))
		},
	} {
		c := base()
		mut(&c)
		if _, err := i.Verify(signRawS4b(t, i, c)); !errors.Is(err, ErrInvalidToken) {
			t.Errorf("%s: want ErrInvalidToken, got %v", name, err)
		}
	}
	// otp alone (a future factor combination) and within-skew mfa_at are admitted.
	for name, mut := range map[string]func(*Claims){
		"otp only":           func(c *Claims) { c.AMR = []string{"otp"} },
		"mfa_at within skew": func(c *Claims) { c.MFAAt = jwt.NewNumericDate(now.Add(2 * time.Second)) },
		"auth_time == mfa_at": func(c *Claims) {
			c.MFAAt = c.AuthTime
		},
	} {
		c := base()
		mut(&c)
		if _, err := i.Verify(signRawS4b(t, i, c)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestIssueStaff_RefusesIncoherentAssurance_S3R13(t *testing.T) {
	i := testIssuer(t)
	for name, mut := range map[string]func(*StaffTokenParams){
		"amr without otp":    func(p *StaffTokenParams) { p.AMR = []string{"pwd"} },
		"mfa_at < auth_time": func(p *StaffTokenParams) { p.MFAAt = p.AuthTime.Add(-time.Second) },
	} {
		p := testStaffParams(uuid.New(), uuid.New(), RoleSupport)
		mut(&p)
		if _, err := i.IssueStaff(p, time.Minute); !errors.Is(err, ErrStaffAssuranceRequired) {
			t.Errorf("%s: want ErrStaffAssuranceRequired, got %v", name, err)
		}
	}
}

func TestStepUpPolicy_DefaultsBoundsAndMaxAge(t *testing.T) {
	p := DefaultStepUpPolicy()
	if p.Critical != 5*time.Minute || p.Financial != 10*time.Minute || !p.Valid() {
		t.Fatalf("defaults: %+v", p)
	}
	if d, ok := p.MaxAge(StepUpCritical); !ok || d != 5*time.Minute {
		t.Fatal("critical")
	}
	if d, ok := p.MaxAge(StepUpFinancial); !ok || d != 10*time.Minute {
		t.Fatal("financial")
	}
	if _, ok := p.MaxAge(StepUpNone); ok {
		t.Fatal("none has no max-age")
	}
	if _, ok := p.MaxAge(StepUpClass("bogus")); ok {
		t.Fatal("an unknown class has no max-age")
	}
	for _, bad := range []StepUpPolicy{{Critical: 6 * time.Minute, Financial: 10 * time.Minute}, {Critical: 5 * time.Minute, Financial: 11 * time.Minute},
		{Critical: 30 * time.Second, Financial: 10 * time.Minute}, {}} {
		if bad.Valid() {
			t.Errorf("%+v must be out of bounds", bad)
		}
	}
	if !HasSecondFactorAMR([]string{"pwd", "otp"}) || HasSecondFactorAMR([]string{"pwd"}) || HasSecondFactorAMR(nil) {
		t.Fatal("second-factor amr")
	}
}

func TestWriteStepUpRequired_BodyAndRFC9470Header(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteStepUpRequired(rec, "req-1", 2*time.Minute)
	if rec.Code != 403 {
		t.Fatalf("status %d", rec.Code)
	}
	h := rec.Header().Get("WWW-Authenticate")
	if !strings.HasPrefix(h, `Bearer realm="platform-api", error="insufficient_user_authentication"`) || !strings.HasSuffix(h, `max_age="120"`) {
		t.Fatalf("header %q", h)
	}
	var body struct {
		Code      string `json:"code"`
		MaxAge    *int   `json:"max_age"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Code != "step_up_required" || body.MaxAge == nil || *body.MaxAge != 120 || body.RequestID != "req-1" {
		t.Fatalf("body %s", rec.Body)
	}
}
