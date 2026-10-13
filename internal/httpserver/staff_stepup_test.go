package httpserver

// ADR 0119 S4b (DB-free): the classifier set, the classifiers' decisions and body discipline, the liveness seal /
// coherence verdict (S3-R10 / S3-R13 / S4a-R10) and the single production setter of the step-up assertion.

import (
	"bytes"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func TestStepUpClassifiers_ParityWithRouteClassifiers(t *testing.T) {
	var a, b []string
	for k := range routeClassifiers {
		a = append(a, k)
	}
	for k, fn := range stepUpClassifiers {
		if fn == nil {
			t.Errorf("%s has no function", k)
		}
		b = append(b, k)
	}
	sort.Strings(a)
	sort.Strings(b)
	if strings.Join(a, ",") != strings.Join(b, ",") || len(a) != 4 {
		t.Fatalf("routeClassifiers %v vs stepUpClassifiers %v (ADR 0119 section 8.2: four classifiers)", a, b)
	}
}

func classify(t *testing.T, fn stepUpClassifier, role auth.Role, tenantID uuid.UUID, body string) (auth.StepUpClass, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/x", strings.NewReader(body))
	r = r.WithContext(tenant.WithContext(r.Context(), tenant.Context{TenantID: tenantID, Subject: uuid.NewString(), Role: string(role), PrincipalType: "staff"}))
	c, err := fn(Deps{}, r)
	if err != nil {
		t.Fatalf("classifier error: %v", err)
	}
	rest, _ := io.ReadAll(r.Body)
	return c, rest
}

func TestClassifiers_DecisionsAndBodyDiscipline(t *testing.T) {
	tn := uuid.New()
	for _, c := range []struct {
		name string
		fn   stepUpClassifier
		role auth.Role
		body string
		want auth.StepUpClass
	}{
		{"launch suspend", launchActionClass, auth.RolePlatformAdmin, `{"action":"suspend","reason_code":"x"}`, auth.StepUpNone},
		{"launch activate", launchActionClass, auth.RolePlatformAdmin, `{"action":"activate","reason_code":"x"}`, auth.StepUpCritical},
		{"launch ratify", launchActionClass, auth.RolePlatformAdmin, `{"action":"ratify"}`, auth.StepUpCritical},
		{"launch duplicate key, last wins", launchActionClass, auth.RolePlatformAdmin, `{"action":"suspend","action":"activate"}`, auth.StepUpCritical},
		{"launch case-folded key", launchActionClass, auth.RolePlatformAdmin, `{"action":"suspend","Action":"close"}`, auth.StepUpCritical},
		{"launch unknown field", launchActionClass, auth.RolePlatformAdmin, `{"action":"suspend","bogus":1}`, auth.StepUpCritical},
		{"launch malformed", launchActionClass, auth.RolePlatformAdmin, `{`, auth.StepUpCritical},
		{"launch empty", launchActionClass, auth.RolePlatformAdmin, ``, auth.StepUpCritical},
		{"policy author", financialPolicyChangeClass, auth.RolePlatformAdmin, `{}`, auth.StepUpCritical},
		{"policy tighten-only", financialPolicyChangeClass, auth.RoleTenantAdmin, `{}`, auth.StepUpNone},
		{"rg value", rgLimitChangeClass, auth.RoleSupport, `{"kind":"deposit","value":"100"}`, auth.StepUpNone},
		{"rg removal", rgLimitChangeClass, auth.RoleSupport, `{"kind":"deposit","value":null}`, auth.StepUpCritical},
		{"rg value missing", rgLimitChangeClass, auth.RoleSupport, `{"kind":"deposit"}`, auth.StepUpCritical},
		{"rg duplicate value, last wins", rgLimitChangeClass, auth.RoleSupport, `{"kind":"deposit","value":"5","value":null}`, auth.StepUpCritical},
		{"rg malformed", rgLimitChangeClass, auth.RoleSupport, `nope`, auth.StepUpCritical},
		{"alert tenant caller (handler refuses)", alertResolveClass, auth.RoleTenantAdmin, `{}`, auth.StepUpNone},
	} {
		tenantID := uuid.Nil
		if c.role != auth.RolePlatformAdmin {
			tenantID = tn
		}
		got, rest := classify(t, c.fn, c.role, tenantID, c.body)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if string(rest) != c.body {
			t.Errorf("%s: the handler must read the SAME bytes, got %q", c.name, rest)
		}
	}
	if !alertNeedsCriticalStepUp("staff.mfa_recovery_used", "p3") || !alertNeedsCriticalStepUp("reconciliation.run_failed", "p1") ||
		alertNeedsCriticalStepUp("alerting.unrouted", "p2") || alertNeedsCriticalStepUp("simulation.casino_play", "p3") {
		t.Fatal("the pinned alert rule")
	}
	// A body above the handler's bound is classified at the ceiling and handed on byte-identical (the handler refuses it).
	big := `{"action":"suspend","x":"` + strings.Repeat("a", int(maxRequestBodyBytes)) + `"}`
	got, rest := classify(t, launchActionClass, auth.RolePlatformAdmin, uuid.Nil, big)
	if got != auth.StepUpCritical || !bytes.HasPrefix([]byte(big), rest) {
		t.Fatal("oversized body")
	}
}

// staffAssuranceVerdict: a valid v2 seal and a coherent row pass; a v1 (legacy) seal, a tampered seal, an amr without a
// second factor, auth_time after mfa_at, a future mfa_at and an auth_time older than the absolute TTL are refused.
func TestStaffAssuranceVerdict(t *testing.T) {
	keys := testStaffMFAKeys()
	sid, staff, tn := uuid.New(), uuid.New(), uuid.New()
	now := staffmfa.SessionTime(time.Now())
	mk := func(mut func(*staffmfa.SessionAssurance)) (staffLivenessRow, staffmfa.SessionAssurance) {
		a := staffmfa.SessionAssurance{SessionID: sid, PrincipalType: "staff", PrincipalID: staff, TenantID: tn, AAL: 2, AMR: []string{"pwd", "otp"},
			AuthTime: now.Add(-time.Hour), MFAAt: now.Add(-time.Minute), AbsoluteExpiresAt: now.Add(11 * time.Hour),
			RefreshTokenHash: strings.Repeat("ab", 32), ExpiresAt: now.Add(20 * time.Minute)}
		if mut != nil {
			mut(&a)
		}
		seal, err := keys.SealSession(a)
		if err != nil {
			t.Fatal(err)
		}
		aal := int16(a.AAL)
		return staffLivenessRow{dbNow: now, aal: &aal, amr: a.AMR, authTime: &a.AuthTime, mfaAt: &a.MFAAt, absoluteExpiry: &a.AbsoluteExpiresAt,
			idleExp: &a.ExpiresAt, refreshHash: a.RefreshTokenHash, kid: &seal.KID, seal: seal.MAC}, a
	}
	row, _ := mk(nil)
	if r, live := staffAssuranceVerdict(row, keys, 12*time.Hour, sid, staff, tn); r != observability.StaffLivenessReasonOK || !live.MFAAt.Equal(*row.mfaAt) ||
		!live.DBNow.Equal(now) || live.SessionID != sid {
		t.Fatalf("valid row: %s %+v", r, live)
	}
	for name, c := range map[string]struct {
		mut  func(*staffmfa.SessionAssurance)
		want observability.StaffLivenessReason
	}{
		"amr pwd only":        {func(a *staffmfa.SessionAssurance) { a.AMR = []string{"pwd"} }, observability.StaffLivenessReasonAssuranceIncoherent},
		"mfa_at < auth_time":  {func(a *staffmfa.SessionAssurance) { a.MFAAt = a.AuthTime.Add(-time.Second) }, observability.StaffLivenessReasonAssuranceIncoherent},
		"mfa_at future":       {func(a *staffmfa.SessionAssurance) { a.MFAAt = now.Add(time.Minute) }, observability.StaffLivenessReasonAssuranceIncoherent},
		"auth_time too old":   {func(a *staffmfa.SessionAssurance) { a.AuthTime = now.Add(-13 * time.Hour) }, observability.StaffLivenessReasonAssuranceIncoherent},
		"aal 3 (sealed)":      {func(a *staffmfa.SessionAssurance) { a.AAL = 3 }, observability.StaffLivenessReasonAssuranceIncoherent},
		"auth_time past 1 h ": {func(a *staffmfa.SessionAssurance) { a.AuthTime = now.Add(-90 * time.Minute) }, ""},
	} {
		r, _ := mk(c.mut)
		ttl := 12 * time.Hour
		want := c.want
		if want == "" {
			ttl, want = time.Hour, observability.StaffLivenessReasonAssuranceIncoherent
		}
		if got, _ := staffAssuranceVerdict(r, keys, ttl, sid, staff, tn); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	// A row whose bound columns differ from what was sealed: seal_invalid.
	for name, mut := range map[string]func(*staffLivenessRow){
		"refresh hash": func(r *staffLivenessRow) { r.refreshHash = strings.Repeat("cd", 32) },
		"idle expiry":  func(r *staffLivenessRow) { e := r.idleExp.Add(time.Minute); r.idleExp = &e },
		"mfa_at":       func(r *staffLivenessRow) { m := r.mfaAt.Add(time.Second); r.mfaAt = &m },
		"seal bit":     func(r *staffLivenessRow) { r.seal = append([]byte(nil), r.seal...); r.seal[0] ^= 1 },
		"null seal":    func(r *staffLivenessRow) { r.seal = nil },
		"other sid":    func(r *staffLivenessRow) {},
	} {
		r, _ := mk(nil)
		mut(&r)
		s := sid
		if name == "other sid" {
			s = uuid.New()
		}
		if got, _ := staffAssuranceVerdict(r, keys, 12*time.Hour, s, staff, tn); got != observability.StaffLivenessReasonSealInvalid {
			t.Errorf("%s: %s, want seal_invalid", name, got)
		}
	}
	if got, _ := staffAssuranceVerdict(row, nil, 12*time.Hour, sid, staff, tn); got != observability.StaffLivenessReasonSealInvalid {
		t.Error("no keys must never pass")
	}
	// A genuine S3 (v1) seal: seal_legacy.
	legacy, a := mk(nil)
	master := sha256.Sum256([]byte("adr0119-s3-httpserver-test|mac-1"))
	sub, err := hkdf.Key(sha256.New, master[:], nil, "staff-session-assurance-v1|m1", 32)
	if err != nil {
		t.Fatal(err)
	}
	us := func(t time.Time) string { return strconv.FormatInt(t.UTC().UnixMicro(), 10) }
	m := hmac.New(sha256.New, sub)
	m.Write([]byte("ss-v1|" + sid.String() + "|staff|" + staff.String() + "|" + tn.String() + "|2|pwd,otp|" + us(a.AuthTime) + "|" + us(a.MFAAt) + "|" + us(a.AbsoluteExpiresAt)))
	legacy.seal = m.Sum(nil)
	if got, _ := staffAssuranceVerdict(legacy, keys, 12*time.Hour, sid, staff, tn); got != observability.StaffLivenessReasonSealLegacy {
		t.Fatalf("legacy v1 seal: %s", got)
	}
}

// The step-up gate without a verified session assurance in its context fails closed (a mis-composed chain).
func TestRequireStepUp_FailsClosedWithoutLiveAssurance(t *testing.T) {
	reached := false
	h := requireStepUp(Deps{StaffMFA: testStaffMFA()}, stepUpCritical)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	r := httptest.NewRequest(http.MethodPost, "/x", nil)
	r = r.WithContext(tenant.WithContext(context.Background(), tenant.Context{Subject: uuid.NewString(), PrincipalType: "staff", Role: "platform_admin"}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if reached || rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "session_revoked") {
		t.Fatalf("%d %s reached=%v", rec.Code, rec.Body, reached)
	}
	// Class none is a pass-through (no assertion placed, nothing refused).
	reached = false
	requireStepUp(Deps{}, stepUpNone)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })).ServeHTTP(httptest.NewRecorder(), r)
	if !reached {
		t.Fatal("none must pass through")
	}
}

// Exactly one production caller of auth.ContextWithStepUpAssertion: the gate.
func TestStatic_StepUpAssertionSetOnlyByTheGate(t *testing.T) {
	root := filepath.Join("..", "..")
	var callers []string
	if err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := info.Name()
			if path != root && (strings.HasPrefix(base, ".") || base == "node_modules" || base == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), "auth.ContextWithStepUpAssertion(") {
			rel, _ := filepath.Rel(root, path)
			callers = append(callers, filepath.ToSlash(rel))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(callers, ",") != "internal/httpserver/staff_stepup.go" {
		t.Fatalf("auth.ContextWithStepUpAssertion production callers: %v", callers)
	}
}
