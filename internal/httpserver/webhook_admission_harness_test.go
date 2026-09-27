//go:build integration

package httpserver

import (
	"context"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// newAdmissionTestServer is newFinancialTestServer/newCasinoTestServer's
// shared ADR 0097 twin: it wires WebhookAdmission and, unless
// skipDirectoryLoad, performs the same synchronous initial directory load
// cmd/platform-api/main.go performs at real startup - a test that omits
// this deliberately (T9's "directory never loaded" fail-safe case) passes
// skipDirectoryLoad=true instead.
func newAdmissionTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, paymentOrch *payments.Orchestrator, casinoOrch *casino.Orchestrator, settings WebhookAdmissionSettings, skipDirectoryLoad bool) *httptest.Server {
	t.Helper()
	handler, rt := NewWithAdmission(Deps{
		Logger:              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                  pool,
		AuthIssuer:          issuer,
		ServiceName:         "platform-api-test",
		AccessTokenTTL:      5 * time.Minute,
		RefreshTokenTTL:     time.Hour,
		PaymentOrchestrator: paymentOrch, PaymentsOutboundCredentials: payments.MockCredentialResolver{},
		CasinoOrchestrator: casinoOrch,
		PersonResolver:     identityresolution.NewMockPersonResolver(),
		WebhookAdmission:   settings,
	})
	if !skipDirectoryLoad {
		if err := rt.LoadDirectory(context.Background()); err != nil {
			t.Fatalf("initial directory load failed: %v", err)
		}
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// testAdmissionSettings returns generous-but-bounded settings suitable
// for driving a deliberate flood in a test with a handful of requests:
// small A3/B1 bursts so a few requests exhausts them, and A4a/A4b/B2 caps
// large enough that they are never the limiting factor in these tests
// (they have their own dedicated coverage in internal/admission and in
// the fake-clock-driven unit tests elsewhere in this package) - so no
// test here ever exercises Bulkhead's bounded WAIT path, which means real
// time (the default RealClock) introduces no flakiness: nothing here
// asserts a Retry-After VALUE or an elapsed duration, only status codes
// and row counts.
func testAdmissionSettings() WebhookAdmissionSettings {
	// A deliberately TINY rate (not the fast 1000/s a real deployment
	// would use): the emission interval T=1/rate must stay far larger
	// than the real wall-clock time a test's own HTTP round trip and
	// domain processing take between successive requests, or the bucket
	// silently refills between calls and burst is never actually
	// exceeded. 0.01/s (T=100s) makes that impossible within any test's
	// real-time budget while still admitting exactly `burst` requests
	// immediately (GCRA's burst tolerance is independent of rate).
	rb := func(burst int) WebhookRateBurst { return WebhookRateBurst{Rate: 0.01, Burst: burst} }
	return WebhookAdmissionSettings{
		Enabled: true,
		PreAuthRate: map[string]WebhookRateBurst{
			"payments": rb(3), "casino": rb(3), "kyc": rb(3),
		},
		PreAuthUnknownRate: map[string]WebhookRateBurst{
			"payments": rb(2), "casino": rb(2), "kyc": rb(2),
		},
		VerifiedRate: map[string]WebhookRateBurst{
			"payments": rb(3), "casino": rb(3), "kyc": rb(3),
		},
		InFlightGlobal: 100, InFlightPerKey: 50, InFlightUnknown: 50,
		DBGateGlobal: 100, DBGatePerKey: 50, DBGateUnknown: 50, DBGateWait: 200 * time.Millisecond,
		DomainTxPerTenant: 100, DomainWait: 200 * time.Millisecond,
		BodyReadTimeout:  10 * time.Second,
		DirectoryRefresh: time.Hour,
		// The shared CI database (docs: "the shared DB may be used by
		// another agent") can hold tens of thousands of tenants
		// accumulated across concurrent test runs - a small cap would
		// truncate OUR own newly-created test tenants out of the
		// snapshot, silently collapsing them into the shared "_unknown"
		// bucket and defeating the very fairness these tests check. A
		// generous cap keeps every test tenant in its own known bucket
		// regardless of how many other tenants exist.
		DirectoryCap: 200000,
		IdleEvict:    time.Hour, VerifiedMaxKeys: 10000, PerIPMaxKeys: 10000,
	}
}
