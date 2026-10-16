//go:build stagingacceptance

package main

import (
	"regexp"
	"sort"
	"strings"
)

// items returns the rows of a list response: a bare array, or the "items" member of a paginated envelope.
func items(r *response) []any {
	if a := r.arr(); a != nil {
		return a
	}
	if m := r.obj(); m != nil {
		if a, ok := m["items"].([]any); ok {
			return a
		}
	}
	return nil
}

var reRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{8,128}$`)

func (w *world) healthChecks() {
	w.check("health.healthz", func(t *T) {
		r := t.call("GET", "/healthz", "", nil)
		if !t.expect(r, 200, "", "GET /healthz") {
			return
		}
		if s := pickStr(r.obj(), "status"); s != "ok" {
			t.fail("/healthz body status=%q, want ok", s)
			return
		}
		t.pass("200 status=ok in %dms", r.Dur.Milliseconds())
	})
	w.check("health.readyz", func(t *T) {
		r := t.call("GET", "/readyz", "", nil)
		if !t.expect(r, 200, "", "GET /readyz (database reachable and webhook tenant directory loaded)") {
			return
		}
		if s := pickStr(r.obj(), "status"); s != "ok" {
			t.fail("/readyz body status=%q, want ok", s)
			return
		}
		t.pass("200 status=ok in %dms", r.Dur.Milliseconds())
	})
	w.check("health.request_id", func(t *T) {
		r1 := t.call("GET", "/healthz", "", nil, withHeader("X-Request-Id", ""))
		if r1 == nil {
			return
		}
		gen := r1.Header.Get("X-Request-Id")
		if !reRequestID.MatchString(gen) {
			t.fail("no usable X-Request-Id on a request that carried none: %q", gen)
			return
		}
		want := "acc-" + w.cfg.RunID + "-echo"
		r2 := t.call("GET", "/healthz", "", nil, withHeader("X-Request-Id", want))
		if r2 == nil {
			return
		}
		if got := r2.Header.Get("X-Request-Id"); got != want {
			t.fail("a client-supplied X-Request-Id was not echoed (want %q, got %q): CloudWatch correlation by request id is not possible", want, got)
			return
		}
		t.pass("generated id present; client id %s echoed", want)
	})
	w.check("health.security_headers", func(t *T) {
		mode := w.cfg.EdgeHeader
		https := strings.HasPrefix(w.c.base.Scheme, "https")
		if mode == "skip" || (mode == "auto" && !https) {
			t.skipPrereq("BASE_URL is not https (or EDGE_HEADERS=skip): the security headers are added by the CloudFront response-headers policy, not by platform-api")
			return
		}
		r := t.call("GET", "/healthz", "", nil)
		if r == nil {
			return
		}
		var missing []string
		if !strings.Contains(strings.ToLower(r.Header.Get("Strict-Transport-Security")), "max-age=") {
			missing = append(missing, "Strict-Transport-Security")
		}
		if !strings.EqualFold(r.Header.Get("X-Content-Type-Options"), "nosniff") {
			missing = append(missing, "X-Content-Type-Options=nosniff")
		}
		if r.Header.Get("X-Frame-Options") == "" {
			missing = append(missing, "X-Frame-Options")
		}
		if r.Header.Get("Referrer-Policy") == "" {
			missing = append(missing, "Referrer-Policy")
		}
		if len(missing) > 0 {
			t.fail("missing edge security headers: %s", strings.Join(missing, ", "))
			return
		}
		t.pass("HSTS, nosniff, X-Frame-Options and Referrer-Policy present")
	})
	w.check("health.cors", func(t *T) {
		origin := w.cfg.CORSOrigin
		if origin == "" {
			t.skipPrereq("CORS_ORIGIN not supplied: pass an origin that CORS_ALLOWED_ORIGINS allows to test the allow-list")
			return
		}
		pre := func(o string) *response {
			return t.call("OPTIONS", "/v1/auth/login", "", nil,
				withHeader("Origin", o), withHeader("Access-Control-Request-Method", "POST"),
				withHeader("Access-Control-Request-Headers", "Content-Type, Authorization"))
		}
		ok := pre(origin)
		if ok == nil {
			return
		}
		if got := ok.Header.Get("Access-Control-Allow-Origin"); got != origin {
			t.fail("preflight from the allowed origin got Access-Control-Allow-Origin=%q (HTTP %d), want %q", got, ok.Status, origin)
			return
		}
		bad := pre("https://not-allowed.acceptance.invalid")
		if bad == nil {
			return
		}
		if got := bad.Header.Get("Access-Control-Allow-Origin"); got != "" {
			t.fail("a non-allow-listed origin was granted Access-Control-Allow-Origin=%q", got)
			return
		}
		if ok.Header.Get("Access-Control-Allow-Origin") == "*" {
			t.fail("wildcard CORS")
			return
		}
		t.pass("allowed origin echoed, foreign origin refused, no wildcard")
	})
	w.check("health.unauthenticated_rejected", func(t *T) {
		for _, p := range []string{"/v1/admin/tenants", "/v1/me/wallets", "/v1/admin/alerts"} {
			r := t.call("GET", p, "", nil)
			if !t.expect(r, 401, "", "GET "+p+" without credentials") {
				return
			}
		}
		r := t.call("GET", "/v1/admin/tenants", "not-a-real-token", nil)
		if !t.expect(r, 401, "", "GET /v1/admin/tenants with a garbage bearer token") {
			return
		}
		t.pass("admin and player routes answer 401 without or with an invalid token")
	})
	w.check("health.metrics_not_public", func(t *T) {
		r := t.call("GET", "/metrics", "", nil)
		if r == nil {
			return
		}
		if r.Status == 200 && strings.Contains(string(r.Body), "# HELP") {
			t.fail("/metrics exposes a Prometheus exposition on the public API (it must stay on loopback METRICS_ADDR)")
			return
		}
		t.pass("/metrics is not served publicly (HTTP %d)", r.Status)
	})
	w.check("health.edge_allowlist_403", func(t *T) {
		t.skipManual("proving that a NON-allow-listed network gets 403 needs a vantage point outside staging_access_cidrs; the operator does it once (README section 'Operator-only evidence')")
	})
}

func sortedKeys(m map[string]string) []string {
	k := make([]string, 0, len(m))
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}
