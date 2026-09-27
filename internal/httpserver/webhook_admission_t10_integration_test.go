//go:build integration

// ADR 0097 T10 (Ordering_NoDBNoBodyBeforeAdmission), per security's exact
// specification: a request rejected at A2/A3/A4a issues ZERO statements
// and reads ZERO body bytes, in ALL THREE domains. Must kill S1: admission
// moved to run AFTER the body read or the slug lookup in a handler.
//
// This calls each webhook handler function DIRECTLY with
// httptest.NewRecorder (no real net/http server/connection involved) -
// deliberately, not through a real httptest.Server: net/http's real
// server automatically drains any unread request body after a handler
// returns (to support connection reuse), which would call Read on our
// canary body regardless of whether the HANDLER itself ever touched it,
// producing a false positive. Calling the handler directly is the
// faithful way to observe "did OUR CODE read the body", not "did the Go
// stdlib's connection-reuse machinery".
package httpserver

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

// panicOnReadBody is an io.ReadCloser that fails the test the instant
// anything calls Read - the direct, unambiguous proof that a
// pre-admission-rejected request never reads a single body byte (ORD-2).
type panicOnReadBody struct {
	t *testing.T
}

func (p panicOnReadBody) Read(b []byte) (int, error) {
	p.t.Helper()
	p.t.Error("body was read before admission rejected the request - ORD-2 violated (possible S1 mutation)")
	return 0, io.EOF
}

func (p panicOnReadBody) Close() error { return nil }

// t10AdmittedBody returns a fresh, real, readable body for a request the
// test expects to be ADMITTED (so it must actually be readable).
func t10AdmittedBody() io.ReadCloser {
	return io.NopCloser(bytes.NewReader([]byte("{}")))
}

// TestAdmission_T10_PreAuthRejection_NeverReadsBody drives, for each of
// the three domains, three admitted requests (exhausting A3's burst=3)
// followed by one rejected request whose body panics the test if ever
// read - proving the REAL handler never reads the body once admission has
// already rejected the request.
func TestAdmission_T10_PreAuthRejection_NeverReadsBody(t *testing.T) {
	pool, issuer := testEnv(t)

	quietLogger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("payments", func(t *testing.T) {
		orchestrator, mockProvider := newMockOrchestrator()
		tenant := mustCreateTenant(t, pool)
		mustCreateBrand(t, pool, tenant)
		mustRegisterCapability(t, pool, tenant.ID, mockProvider)
		handler, rt := NewWithAdmission(Deps{
			DB: pool, AuthIssuer: issuer, ServiceName: "t", Logger: quietLogger,
			PaymentOrchestrator: orchestrator, WebhookAdmission: testAdmissionSettings(),
		})
		if err := rt.LoadDirectory(context.Background()); err != nil {
			t.Fatal(err)
		}
		path := "/v1/webhooks/payments/" + tenant.Slug + "/mock"
		for i := 0; i < 3; i++ {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, path, t10AdmittedBody())
			handler.ServeHTTP(rec, r)
		}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, panicOnReadBody{t: t})
		handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 (A3 exhausted), got %d", rec.Code)
		}
	})

	t.Run("casino", func(t *testing.T) {
		mock := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
		orchestrator := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, casino.NewMockWebhookCredentials(mock))
		tenant := mustCreateTenant(t, pool)
		mustCreateBrand(t, pool, tenant)
		handler, rt := NewWithAdmission(Deps{
			DB: pool, AuthIssuer: issuer, ServiceName: "t", Logger: quietLogger,
			CasinoOrchestrator: orchestrator, WebhookAdmission: testAdmissionSettings(),
		})
		if err := rt.LoadDirectory(context.Background()); err != nil {
			t.Fatal(err)
		}
		path := "/v1/webhooks/casino/" + tenant.Slug + "/mock-casino"
		for i := 0; i < 3; i++ {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, path, t10AdmittedBody())
			handler.ServeHTTP(rec, r)
		}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, panicOnReadBody{t: t})
		handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 (A3 exhausted), got %d", rec.Code)
		}
	})

	t.Run("kyc", func(t *testing.T) {
		mock := kyc.NewMockKYCProvider()
		orchestrator := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock-kyc": mock}, kyc.NewMockWebhookCredentials(mock))
		tenant := mustCreateTenant(t, pool)
		mustCreateBrand(t, pool, tenant)
		handler, rt := NewWithAdmission(Deps{
			DB: pool, AuthIssuer: issuer, ServiceName: "t", Logger: quietLogger,
			KYCOrchestrator: orchestrator, KYCWebhookEnabled: true, WebhookAdmission: testAdmissionSettings(),
		})
		if err := rt.LoadDirectory(context.Background()); err != nil {
			t.Fatal(err)
		}
		path := "/v1/webhooks/kyc/" + tenant.Slug + "/mock-kyc"
		for i := 0; i < 3; i++ {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodPost, path, t10AdmittedBody())
			handler.ServeHTTP(rec, r)
		}
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, path, panicOnReadBody{t: t})
		handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("expected 429 (A3 exhausted), got %d", rec.Code)
		}
	})
}
