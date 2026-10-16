//go:build stagingacceptance

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B (SHA-1, secret "12345678901234567890"): the 6-digit tail of the published 8-digit codes.
func TestTOTPMatchesRFC6238Vectors(t *testing.T) {
	sec := &totpSecret{key: []byte("12345678901234567890")}
	for _, v := range []struct {
		unix int64
		want string
	}{{59, "287082"}, {1111111109, "081804"}, {1111111111, "050471"}, {1234567890, "005924"}, {2000000000, "279037"}} {
		if got := sec.codeAtStep(stepAt(time.Unix(v.unix, 0))); got != v.want {
			t.Errorf("t=%d: got %s want %s", v.unix, got, v.want)
		}
	}
}

func TestTOTPNeverReusesAStepAndWaitsWhenWindowIsUsedUp(t *testing.T) {
	sec := &totpSecret{key: []byte("12345678901234567890")}
	now := time.Unix(1_700_000_010, 0)
	slept := time.Duration(0)
	clock := func() time.Time { return now }
	sleep := func(d time.Duration) { slept += d; now = now.Add(d) }
	seen := map[string]bool{}
	var last int64
	for i := 0; i < 8; i++ {
		code := sec.next(clock, sleep)
		if seen[code] {
			t.Fatalf("code %s presented twice", code)
		}
		seen[code] = true
		if sec.lastUsed <= last {
			t.Fatalf("step did not advance: %d then %d", last, sec.lastUsed)
		}
		last = sec.lastUsed
		if d := sec.lastUsed - stepAt(now); d < -totpSkew || d > totpSkew {
			t.Fatalf("presented step %d is outside the server window around %d", sec.lastUsed, stepAt(now))
		}
	}
	if slept == 0 {
		t.Fatal("eight uses in two simulated steps must have waited for the clock")
	}
}

func TestTOTPSecretNeverFormats(t *testing.T) {
	sec, err := newTOTPSecretFromBase32("GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{sec.String(), strings.TrimSpace(strings.Join([]string{sec.GoString()}, "")), formatV(sec), formatV(*sec)} {
		if strings.Contains(s, "GEZDG") || strings.Contains(s, "12345") {
			t.Fatalf("secret leaked via formatting: %q", s)
		}
	}
}

func formatV(v any) string { return fmt.Sprintf("%v|%+v|%#v|%s", v, v, v, v) }

func TestScrubberRemovesCredentialsButKeepsEvidence(t *testing.T) {
	s := newScrubber()
	s.add("hunter2-hunter2")
	jwt := "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4eHh4eHh4In0.abcdefghijklmnop"
	in := "pw=hunter2-hunter2 " + jwt + " Authorization: Bearer abcdefghijklmnop12345 otpauth://totp/x?secret=ABC " +
		`{"password":"p@ss","token":"xyz","code":"123456"} opaque=` + strings.Repeat("a1", 20) +
		" id=6fe434ab-428e-4c9d-a827-24206622b1c4 check=auth.staff_password_only_yields_no_token path=/v1/admin/providers/mock-payments/capability code=MARKET_NOT_AVAILABLE"
	out := s.clean(in)
	for _, leak := range []string{"hunter2", jwt, "abcdefghijklmnop12345", "otpauth://", "p@ss", "xyz", "123456", strings.Repeat("a1", 20)} {
		if strings.Contains(out, leak) {
			t.Errorf("scrubbed output still contains %q: %s", leak, out)
		}
	}
	for _, keep := range []string{"6fe434ab-428e-4c9d-a827-24206622b1c4", "auth.staff_password_only_yields_no_token", "/v1/admin/providers/mock-payments/capability", "MARKET_NOT_AVAILABLE"} {
		if !strings.Contains(out, keep) {
			t.Errorf("scrubber destroyed evidence %q: %s", keep, out)
		}
	}
}

func TestResultLinesAreOneLineFourFieldsAndScrubbed(t *testing.T) {
	var out, echo bytes.Buffer
	sc := newScrubber()
	sc.add("topsecret-value")
	rep := newReporter(sc, &out, &echo)
	rep.add(result{Name: "x.y", Status: statusFail, Detail: "multi\nline | pipe topsecret-value"})
	rep.add(result{Name: "x.z", Status: statusSkip, Skip: skipPrereq, Detail: "needs input"})
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if f := strings.Split(l, "|"); len(f) != 4 || f[0] != "RESULT" {
			t.Fatalf("malformed result line %q", l)
		}
		if strings.Contains(l, "topsecret") {
			t.Fatalf("secret in result line %q", l)
		}
	}
	if out.String() != echo.String() {
		t.Fatal("file and stdout must carry identical lines")
	}
	if c := rep.tally(); c.Fail != 1 || c.SkipPrereq != 1 || c.Pass != 0 {
		t.Fatalf("tally %+v", c)
	}
}

func TestConfigValidationNamesTheProblemAndNeverTheSecret(t *testing.T) {
	for _, k := range []string{"BASE_URL", "TENANT", "BRAND", "ADMIN_EMAIL", "ADMIN_PASSWORD_FILE", "ADMIN_TOTP_SECRET_FILE"} {
		t.Setenv(k, "")
	}
	_, err := parseConfig(nil)
	if err == nil {
		t.Fatal("empty configuration accepted")
	}
	for _, want := range []string{"BASE_URL", "TENANT", "BRAND", "ADMIN_EMAIL", "ADMIN_TOTP_SECRET_FILE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
	t.Setenv("BASE_URL", "https://x.cloudfront.net")
	t.Setenv("TENANT", "acme")
	t.Setenv("BRAND", "acme-brand")
	t.Setenv("ADMIN_EMAIL", "a@example.invalid")
	t.Setenv("ADMIN_PASSWORD_FILE", "/nonexistent/pw")
	t.Setenv("ADMIN_TOTP_SECRET_FILE", "/nonexistent/totp")
	c, err := parseConfig(nil)
	if err != nil {
		t.Fatalf("valid configuration refused: %v", err)
	}
	if c.MarketExpect != "open" || !c.Configure || !c.KillSwitchDrill {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	t.Setenv("PLAYER_RESIDENCE_COUNTRY", "mt")
	if _, err := parseConfig(nil); err == nil || !strings.Contains(err.Error(), "ISO-3166") {
		t.Fatalf("lower-case residence accepted: %v", err)
	}
	t.Setenv("PLAYER_RESIDENCE_COUNTRY", "")
	t.Setenv("MARKET_EXPECT", "maybe")
	if _, err := parseConfig(nil); err == nil {
		t.Fatal("bad MARKET_EXPECT accepted")
	}
}

// The platform refuses a payout destination whose maximal digit run is a Luhn-valid 12-19 digit string (a possible card
// number: payoutinstrument.ContainsPAN). A synthetic IBAN that trips it makes a run flaky (observed once in 10% odds).
func TestSyntheticIBANIsValidAndNeverLooksLikeACardNumber(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 3000; i++ {
		iban := syntheticIBAN(randHex(4))
		seen[iban] = true
		// ISO 13616 mod-97 check
		rearr := iban[4:] + iban[:4]
		rem := 0
		for _, c := range rearr {
			if c >= 'A' && c <= 'Z' {
				rem = (rem*100 + int(c-'A') + 10) % 97
			} else {
				rem = (rem*10 + int(c-'0')) % 97
			}
		}
		if rem != 1 || len(iban) != 22 || !strings.HasPrefix(iban, "GB") {
			t.Fatalf("invalid IBAN %q (rem %d)", iban, rem)
		}
		run := ""
		for _, c := range iban {
			if c >= '0' && c <= '9' {
				run += string(c)
				continue
			}
			if len(run) >= 12 && len(run) <= 19 && luhnValid(run) {
				t.Fatalf("IBAN %q contains a Luhn-valid digit run %q", iban, run)
			}
			run = ""
		}
		if len(run) >= 12 && len(run) <= 19 && luhnValid(run) {
			t.Fatalf("IBAN %q ends in a Luhn-valid digit run", iban)
		}
	}
	if len(seen) < 2000 {
		t.Fatalf("only %d distinct IBANs from 3000 run ids", len(seen))
	}
}

func TestSecretFilesMustBePrivate(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "pw")
	if err := os.WriteFile(p, []byte("super-secret-content\n"), 0o644); err != nil { //nolint:gosec // deliberately too open
		t.Fatal(err)
	}
	_, err := readSecretFile(p)
	if err == nil || strings.Contains(err.Error(), "super-secret-content") {
		t.Fatalf("group/other-readable secret file accepted or leaked: %v", err)
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSecretFile(p)
	if err != nil || got != "super-secret-content" {
		t.Fatalf("private file refused: %q %v", got, err)
	}
}

// newStubWorld builds a world whose client talks to srv.
func newStubWorld(t *testing.T, srv *httptest.Server, mutate func(*config)) (*world, *bytes.Buffer) {
	t.Helper()
	cfg := &config{BaseURL: srv.URL, Tenant: "acme", Brand: "acme-brand", RunID: "unit", Timeout: 5 * time.Second, Asset: "EUR", MarketExpect: "open",
		EdgeHeader: "auto", Actors: map[string]actorSpec{}}
	if mutate != nil {
		mutate(cfg)
	}
	var out bytes.Buffer
	sc := newScrubber()
	rep := newReporter(sc, &out, nil)
	c, err := newClient(srv.URL, rep, cfg.RunID, cfg.Timeout, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.hc = srv.Client()
	c.hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return newWorld(cfg, rep, sc, c), &out
}

func stubAPI(extra func(mux *http.ServeMux)) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if id := r.Header.Get("X-Request-Id"); id != "" {
			w.Header().Set("X-Request-Id", id)
		} else {
			w.Header().Set("X-Request-Id", "generated-request-id-1")
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	})
	if extra != nil {
		extra(mux)
	}
	return httptest.NewServer(mux)
}

func resultOf(out *bytes.Buffer, name string) string {
	for _, l := range strings.Split(out.String(), "\n") {
		if strings.HasPrefix(l, "RESULT|"+name+"|") {
			return strings.Split(l, "|")[2]
		}
	}
	return "MISSING"
}

func TestHealthCheckFailsOnAnUnhealthyServerAndDoesNotPassSilently(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", http.StatusInternalServerError) }))
	defer srv.Close()
	w, out := newStubWorld(t, srv, nil)
	w.healthChecks()
	if got := resultOf(out, "health.healthz"); got != "FAIL" {
		t.Fatalf("a 500 /healthz was reported %s", got)
	}
	if got := resultOf(out, "health.readyz"); got != "FAIL" {
		t.Fatalf("a 500 /readyz was reported %s", got)
	}
	if got := resultOf(out, "health.edge_allowlist_403"); got != "SKIP" {
		t.Fatalf("the operator-only edge check must be an explicit SKIP, got %s", got)
	}
}

func TestDependentChecksAreBlockedNotPassedWhenAPrerequisiteFails(t *testing.T) {
	srv := stubAPI(nil)
	defer srv.Close()
	w, out := newStubWorld(t, srv, nil)
	w.check("a.first", func(t *T) { t.fail("broken") })
	w.check("a.second", func(t *T) {
		if !t.need("a.first") {
			return
		}
		t.pass("must not run")
	})
	w.check("a.third", func(t *T) {
		if !t.need("a.second") {
			return
		}
		t.pass("must not run")
	})
	w.check("a.silent", func(t *T) {})
	if resultOf(out, "a.second") != "SKIP" || resultOf(out, "a.third") != "SKIP" {
		t.Fatalf("dependents not skipped:\n%s", out.String())
	}
	if resultOf(out, "a.silent") != "FAIL" {
		t.Fatal("a check that reaches no verdict must FAIL, never pass")
	}
	if !strings.Contains(out.String(), "[blocked-by-fail]") {
		t.Fatal("blocked dependents must be labelled")
	}
}

func TestEdgeSecurityHeadersAreRequiredOverHTTPS(t *testing.T) {
	good := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer good.Close()
	w, out := newStubWorld(t, good, nil)
	w.healthChecks()
	if got := resultOf(out, "health.security_headers"); got != "PASS" {
		t.Fatalf("complete headers reported %s\n%s", got, out.String())
	}
	bad := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"status":"ok"}`)) }))
	defer bad.Close()
	w2, out2 := newStubWorld(t, bad, nil)
	w2.healthChecks()
	if got := resultOf(out2, "health.security_headers"); got != "FAIL" {
		t.Fatalf("missing headers reported %s", got)
	}
}

// stubStaff implements just enough of the staff MFA API to drive actor.signIn through first-time enrolment and then a
// plain MFA login, using the suite's own TOTP generator on both sides.
func stubStaff(secretB32 string, enrolled *bool, lastStep *int64) func(mux *http.ServeMux) {
	sec, _ := newTOTPSecretFromBase32(secretB32)
	check := func(code string) bool {
		for s := stepAt(time.Now()) - 1; s <= stepAt(time.Now())+1; s++ {
			if sec.codeAtStep(s) == code && s > *lastStep {
				*lastStep = s
				return true
			}
		}
		return false
	}
	read := func(r *http.Request) map[string]string {
		var m map[string]string
		_ = json.NewDecoder(r.Body).Decode(&m)
		return m
	}
	write := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	return func(mux *http.ServeMux) {
		mux.HandleFunc("POST /v1/staff/auth/login", func(w http.ResponseWriter, r *http.Request) {
			if !*enrolled {
				write(w, 403, map[string]string{"code": "mfa_enrolment_required"})
				return
			}
			write(w, 200, map[string]any{"challenge": map[string]any{"type": "mfa", "token": "challenge-token-0123456789abcdef", "methods": []string{"totp"}}})
		})
		mux.HandleFunc("POST /v1/staff/auth/enrol/redeem", func(w http.ResponseWriter, r *http.Request) {
			if read(r)["token"] != "bootstrap-token-0123456789abcdef" {
				write(w, 401, map[string]string{"code": "invalid_enrolment_token"})
				return
			}
			write(w, 200, map[string]any{"challenge": map[string]any{"type": "enrol", "token": "enrol-challenge-0123456789abcdef"}})
		})
		mux.HandleFunc("POST /v1/staff/auth/enrol/start", func(w http.ResponseWriter, r *http.Request) {
			write(w, 200, map[string]any{"factor_id": "f1", "secret_base32": secretB32, "otpauth_uri": "otpauth://totp/x?secret=" + secretB32})
		})
		mux.HandleFunc("POST /v1/staff/auth/enrol/confirm", func(w http.ResponseWriter, r *http.Request) {
			if !check(read(r)["code"]) {
				write(w, 401, map[string]string{"code": "invalid_mfa_code"})
				return
			}
			*enrolled = true
			write(w, 200, map[string]any{"recovery_codes": []string{"rc-aaaa-bbbb", "rc-cccc-dddd"}, "token_pair": map[string]string{"access_token": "access-token-enrolled-0123456789", "refresh_token": "refresh-token-0123456789"}})
		})
		mux.HandleFunc("POST /v1/staff/auth/mfa/verify", func(w http.ResponseWriter, r *http.Request) {
			if !check(read(r)["code"]) {
				write(w, 401, map[string]string{"code": "invalid_mfa_code"})
				return
			}
			write(w, 200, map[string]string{"access_token": "access-token-login-0123456789", "refresh_token": "refresh-token-login-0123456789"})
		})
	}
}

func TestStaffEnrolmentThenLoginStoresSecretsPrivatelyAndNeverPrintsThem(t *testing.T) {
	const secret = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	enrolled := false
	var last int64
	srv := stubAPI(stubStaff(secret, &enrolled, &last))
	defer srv.Close()
	dir := t.TempDir()
	write := func(name, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	spec := actorSpec{Role: "ADMIN", Email: "a@example.invalid", PasswordFile: write("pw", "correct-horse-battery\n"), TOTPFile: filepath.Join(dir, "totp"), BootstrapFile: write("boot", "bootstrap-token-0123456789abcdef\n")}
	w, out := newStubWorld(t, srv, func(c *config) { c.Actors["ADMIN"] = spec })
	now := time.Now()
	w.now = func() time.Time { return now }
	w.sleep = func(d time.Duration) { now = now.Add(d) }
	a := &actor{spec: spec, w: w}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	if err := a.signIn(); err != nil {
		t.Fatalf("enrolment sign-in: %v", err)
	}
	if !a.enrolled || a.access == "" {
		t.Fatal("expected a first-time enrolment and an access token")
	}
	for _, f := range []string{spec.TOTPFile, spec.TOTPFile + ".recovery-codes"} {
		st, err := os.Stat(f)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("%s missing or not 0600: %v %v", f, st, err)
		}
	}
	// second run: the secret is read from the file, MFA verify path, no bootstrap token needed
	b := &actor{spec: actorSpec{Role: "ADMIN", Email: spec.Email, PasswordFile: spec.PasswordFile, TOTPFile: spec.TOTPFile}, w: w}
	if err := b.load(); err != nil {
		t.Fatal(err)
	}
	if err := b.signIn(); err != nil {
		t.Fatalf("login with the stored secret: %v", err)
	}
	w.rep.add(result{Name: "stub.enrol", Status: statusPass, Detail: "secret=" + secret + " pw=correct-horse-battery token=bootstrap-token-0123456789abcdef"})
	for _, leak := range []string{secret, "correct-horse-battery", "bootstrap-token-0123456789abcdef", "access-token-enrolled", "rc-aaaa-bbbb"} {
		if strings.Contains(out.String(), leak) {
			t.Fatalf("results contain %q:\n%s", leak, out.String())
		}
	}
}

func TestEnrolmentRefusesToRunWithoutAPlaceToKeepTheSecret(t *testing.T) {
	enrolled := false
	var last int64
	srv := stubAPI(stubStaff("JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP", &enrolled, &last))
	defer srv.Close()
	dir := t.TempDir()
	pw := filepath.Join(dir, "pw")
	boot := filepath.Join(dir, "boot")
	_ = os.WriteFile(pw, []byte("correct-horse-battery"), 0o600)
	_ = os.WriteFile(boot, []byte("bootstrap-token-0123456789abcdef"), 0o600)
	spec := actorSpec{Role: "ADMIN", Email: "a@example.invalid", PasswordFile: pw, BootstrapFile: boot}
	w, _ := newStubWorld(t, srv, nil)
	a := &actor{spec: spec, w: w}
	if err := a.load(); err != nil {
		t.Fatal(err)
	}
	if err := a.signIn(); err == nil {
		t.Fatal("enrolled into memory only: the secret would be lost")
	}
	if enrolled {
		t.Fatal("the factor was confirmed although the secret could not be stored")
	}
}
