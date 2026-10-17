package httpserver

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// PANIC-LOG-VALUE-1: every recover() site in this package logs the panic
// TYPE and a stable code only - never the panic value, which can carry a
// credential, a token, a URL with a query-string secret or player PII -
// and recovery behaviour (500 / 503 / continue) is unchanged.

const panicLogFakeSecret = "SENTINEL-sk_live_7Qx-jane.doe@example.invalid"

var panicLogCases = []struct {
	name     string
	val      any
	wantType string
}{
	{"string", "Authorization: Bearer " + panicLogFakeSecret, "string"},
	{"error", errors.New("vendor said " + panicLogFakeSecret), "*errors.errorString"},
	{"url_error", &url.Error{Op: "Post", URL: "https://psp.invalid/?api_key=" + panicLogFakeSecret, Err: errors.New(panicLogFakeSecret)}, "*url.Error"},
	{"struct", struct{ Email string }{panicLogFakeSecret}, "struct { Email string }"},
}

func assertTypeOnlyPanicLog(t *testing.T, out, code, wantType string) {
	t.Helper()
	if strings.Contains(out, panicLogFakeSecret) || strings.Contains(out, "vendor said") || strings.Contains(out, "psp.invalid") || strings.Contains(out, "Bearer") {
		t.Fatalf("panic value reached the log: %s", out)
	}
	if !strings.Contains(out, `"panic_code":"`+code+`"`) || !strings.Contains(out, `"panic_type":"`+wantType+`"`) {
		t.Fatalf("want a type-only panic line (code %q, type %q), got: %s", code, wantType, out)
	}
	if strings.Contains(out, `"panic":`) || strings.Contains(out, "goroutine ") || strings.Contains(out, ".go:") {
		t.Fatalf("panic value key or stack trace in the log: %s", out)
	}
}

func TestRecoverMiddleware_PanicValueNeverLogged_Still500(t *testing.T) {
	for _, tc := range panicLogCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			h := chain(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(tc.val) }),
				requestIDMiddleware, recoverMiddleware(logger))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/me", nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want 500", rec.Code)
			}
			if strings.Contains(rec.Body.String(), panicLogFakeSecret) {
				t.Fatalf("panic value reached the response: %s", rec.Body.String())
			}
			out := buf.String()
			if !strings.Contains(out, `"panic_recovered"`) || !strings.Contains(out, `"path":"/v1/me"`) {
				t.Fatalf("panic_recovered line missing message/path: %s", out)
			}
			assertTypeOnlyPanicLog(t, out, "http_handler_panic", tc.wantType)
		})
	}
}

func TestWebhookAdmission_PanicValueNeverLogged_Still503(t *testing.T) {
	for _, tc := range panicLogCases {
		t.Run("preauth_"+tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			rt := newMetricsTestRuntime(t)
			rt.logger = slog.New(slog.NewJSONHandler(&buf, nil))
			w := httptest.NewRecorder()
			r := newWebhookMetricsRequest("/v1/webhooks/payments/p1/mock", "p1", "mock")
			rel, ok := rt.admitPreAuth(w, r, domainPayments, 0, func(string) bool { panic(tc.val) }, nil)
			if ok || rel != nil {
				t.Fatalf("panic must reject with no release func: ok=%v", ok)
			}
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			if strings.Contains(w.Body.String(), panicLogFakeSecret) {
				t.Fatalf("panic value reached the response: %s", w.Body.String())
			}
			out := buf.String()
			if !strings.Contains(out, "webhook_admission_panic_recovered") || !strings.Contains(out, `"domain":"payments"`) {
				t.Fatalf("admission panic line missing: %s", out)
			}
			assertTypeOnlyPanicLog(t, out, "webhook_admission_preauth_panic", tc.wantType)
		})
		t.Run("verified_"+tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			rt := newMetricsTestRuntime(t)
			rt.logger = slog.New(slog.NewJSONHandler(&buf, nil))
			tenantID := uuid.New()
			// Exhaust B1 first so the retries429 callback (which panics) runs.
			w1 := httptest.NewRecorder()
			r1 := newWebhookMetricsRequest("/v1/webhooks/kyc/v1/mock", "v1", "mock")
			rel1, ok1 := rt.admitVerified(w1, r1, domainKYC, tenantID, "mock", nil)
			if !ok1 {
				t.Fatal("expected the first verified call to be admitted")
			}
			rel1()
			w := httptest.NewRecorder()
			r := newWebhookMetricsRequest("/v1/webhooks/kyc/v1/mock", "v1", "mock")
			rel, ok := rt.admitVerified(w, r, domainKYC, tenantID, "mock", func(string) (bool, bool) { panic(tc.val) })
			if ok || rel != nil {
				t.Fatalf("panic must reject with no release func: ok=%v", ok)
			}
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503", w.Code)
			}
			out := buf.String()
			if !strings.Contains(out, "webhook_admission_panic_recovered") || !strings.Contains(out, `"domain":"kyc"`) {
				t.Fatalf("admission panic line missing: %s", out)
			}
			assertTypeOnlyPanicLog(t, out, "webhook_admission_verified_panic", tc.wantType)
		})
	}
}

func TestRecoverIntegrityRecordPanic_PanicValueNeverLogged(t *testing.T) {
	for _, tc := range panicLogCases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&buf, nil))
			func() {
				defer recoverIntegrityRecordPanic(logger)
				panic(tc.val)
			}()
			out := buf.String()
			if !strings.Contains(out, "request_withdrawal_integrity_record_panic") {
				t.Fatalf("panic line missing: %s", out)
			}
			assertTypeOnlyPanicLog(t, out, "withdrawal_integrity_record_panic", tc.wantType)
		})
	}
}
