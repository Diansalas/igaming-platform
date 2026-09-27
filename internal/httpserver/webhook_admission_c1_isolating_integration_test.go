//go:build integration

// Security re-verification round 3 (rv-prh-i4-security.md §6.3, C1
// required-to-close (a)): an ISOLATING HTTP-level test per domain that
// lets the tenant-slug lookup (and, for payments, ProviderAcceptsWebhook)
// through the A4b gate, then refuses the credential-resolution read
// specifically - proving the C1 fix at the point it actually matters,
// rather than webhook_admission_credential_resolution_integration_test.go's
// tests, which (as that file's own doc comment now discloses) refuse at
// the tenant-lookup gate and never reach the resolver at all.
//
// The A4b gate (*admission.Bulkhead) is a CONCURRENT-HOLDER cap: a
// sequence of non-overlapping acquire/release calls at the same key never
// exhausts it, no matter how low the cap, because each call releases
// before the next one starts. There is therefore no way to make "the
// Nth sequential call on this key fails, the first N-1 succeed" using the
// real Bulkhead's own config knobs alone. This file instead swaps
// webhookAdmissionRuntime.dbGate (a small interface introduced for
// exactly this purpose, dbGateAcquirer in webhook_admission.go) for a
// call-COUNTING fake that admits exactly the first k calls it sees and
// refuses every one after - k=1 for casino/kyc (their only
// pre-verification read is the slug lookup; credential resolution is
// their SECOND, refused, call), k=2 for payments (slug lookup +
// ProviderAcceptsWebhook are calls 1-2; credential resolution is call 3,
// refused). This is a test-only substitution of an interface value on an
// already-built runtime (same-package access to the unexported field,
// exactly like every other test in this package that reaches into
// webhookAdmissionRuntime's internals) - it does not alter, weaken or
// reimplement the real Bulkhead's own concurrency semantics anywhere
// production code runs.
//
// Every test here uses the REAL providercred.Resolver (never MOCK), per
// security's explicit instruction, so a regression that deletes any of
// the four hops C1's fix touches (gatedReader -> Resolver.Resolve ->
// reasonForResolveError -> the handler's AuthError branch) is caught
// here even though webhook_admission_credential_resolution_integration_test.go's
// own tests cannot reach it (N3/N4 in the security review).
package httpserver

import (
	"bytes"
	"context"
	"encoding/hex"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
)

// countingGate admits exactly the first `allowed` Acquire calls it ever
// sees on its own expected key and refuses every call after. Unlike the
// real admission.Bulkhead (a concurrent-holder cap), this counts total
// CALLS, which is what is needed to let an earlier, already-released
// acquisition (the tenant-slug lookup) succeed while a LATER one
// (credential resolution) on the very same key is refused.
//
// Security re-verification #2 (rv-prh-i4-security.md §7.5, Info I6):
// earlier versions of this fake ignored the key entirely, so they would
// not have caught a regression that gated credential resolution on the
// WRONG A4b key (a distinct bug from the one C1 fixes - it would let one
// tenant's saturated gate leak capacity to, or steal capacity from,
// another tenant/provider pair). wantKey pins the exact key every Acquire
// call on this gate must use; a mismatched key fails the test immediately
// via t.Fatalf, rather than silently admitting or refusing based on call
// count alone.
type countingGate struct {
	t       *testing.T
	wantKey string
	allowed int32
	calls   int32
}

func (g *countingGate) Acquire(key string, perKeyCap int, clock admission.Clock, wait time.Duration) (func(), bool) {
	g.t.Helper()
	if key != g.wantKey {
		g.t.Fatalf("countingGate.Acquire: key = %q, want %q (security Info I6: the gate must always be "+
			"consulted on the SAME key for a given tenant+provider+domain, at every hop)", key, g.wantKey)
	}
	n := atomic.AddInt32(&g.calls, 1)
	if n <= g.allowed {
		return func() {}, true
	}
	return nil, false
}

// c1IsolatingRealSubsystem is c1RealSubsystem
// (webhook_admission_credential_resolution_integration_test.go), repeated
// here so this file has no cross-file test-helper dependency.
func c1IsolatingRealSubsystem(t *testing.T) *providercred.Subsystem {
	t.Helper()
	router, err := memstore.NewRouter(memstore.New())
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sub, err := providercred.New(config.Config{ProviderCredentialFingerprintKey: config.NewSecretValue(hex.EncodeToString(key))}, router)
	if err != nil || sub == nil {
		t.Fatalf("providercred.New: %v (nil=%v)", err, sub == nil)
	}
	return sub
}

// TestAdmission_C1a_PaymentsCredentialResolutionGateRejection_Isolated
// lets calls 1-2 (slug lookup, ProviderAcceptsWebhook) through and refuses
// call 3 (credential resolution) - payments' SECOND VerifyCallback read,
// the exact hop C1's HIGH finding was about.
func TestAdmission_C1a_PaymentsCredentialResolutionGateRejection_Isolated(t *testing.T) {
	pool, issuer := testEnv(t)
	sub := c1IsolatingRealSubsystem(t)
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock}, sub.Resolver("payments"))

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler, rt := NewWithAdmission(Deps{
		DB: pool, AuthIssuer: issuer, ServiceName: "platform-api-test", Logger: logger,
		PaymentOrchestrator: orchestrator, WebhookAdmission: testAdmissionSettings(),
	})
	if err := rt.LoadDirectory(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt.rt.dbGate = &countingGate{t: t, wantKey: string(domainPayments) + "|" + tenant.Slug + "|" + "mock", allowed: 2}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	payload := mock.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "c1a-ref", "", payments.OutcomeSucceeded, 100, "EUR", "", false)
	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
	defer resp.Body.Close()

	assertC1Isolated(t, resp, pool, tenant.ID, "c1a-ref", buf.String())
}

// TestAdmission_C1a_CasinoCredentialResolutionGateRejection_Isolated lets
// call 1 (slug lookup) through and refuses call 2 (casino's ONLY
// VerifyCallback read, credential resolution).
func TestAdmission_C1a_CasinoCredentialResolutionGateRejection_Isolated(t *testing.T) {
	pool, issuer := testEnv(t)
	sub := c1IsolatingRealSubsystem(t)
	mock := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
	orchestrator := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, sub.Resolver("casino"))

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	handler, rt := NewWithAdmission(Deps{
		DB: pool, AuthIssuer: issuer, ServiceName: "platform-api-test", Logger: logger,
		CasinoOrchestrator: orchestrator, WebhookAdmission: testAdmissionSettings(),
	})
	if err := rt.LoadDirectory(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt.rt.dbGate = &countingGate{t: t, wantKey: string(domainCasino) + "|" + tenant.Slug + "|" + "mock-casino", allowed: 1}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "c1a-casino-ref", "", "round-c1a", "game-c1a", 100, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
	resp := rawPostCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payments.InboundCallback{Header: payload.Header, Body: payload.Body})
	defer resp.Body.Close()

	assertC1Isolated(t, resp, pool, tenant.ID, "c1a-casino-ref", buf.String())
}

// TestAdmission_C1a_KYCCredentialResolutionGateRejection_Isolated mirrors
// casino's shape (k=1: slug lookup admitted, credential resolution
// refused).
func TestAdmission_C1a_KYCCredentialResolutionGateRejection_Isolated(t *testing.T) {
	pool, issuer := testEnv(t)
	sub := c1IsolatingRealSubsystem(t)
	mock := kyc.NewMockKYCProvider()
	orchestrator := kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock-kyc": mock}, sub.Resolver("kyc"))

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)

	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	settings := testAdmissionSettings()
	handler, rt := NewWithAdmission(Deps{
		DB: pool, AuthIssuer: issuer, ServiceName: "platform-api-test", Logger: logger,
		KYCOrchestrator: orchestrator, KYCWebhookEnabled: true, WebhookAdmission: settings,
	})
	if err := rt.LoadDirectory(context.Background()); err != nil {
		t.Fatal(err)
	}
	rt.rt.dbGate = &countingGate{t: t, wantKey: string(domainKYC) + "|" + tenant.Slug + "|" + "mock-kyc", allowed: 1}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	payload := mock.CallbackPayload(tenant.ID, "c1a-kyc-ref", kyc.ProviderApproved, "")
	resp := rawPostCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock-kyc", payments.InboundCallback{Header: payload.Header, Body: payload.Body})
	defer resp.Body.Close()

	assertC1Isolated(t, resp, pool, tenant.ID, "c1a-kyc-ref", buf.String())
}

// assertC1Isolated is the shared assertion for every domain in this file:
// 503 (never 401), a Retry-After header, a db_gate log line, and zero
// ledger rows for the callback's own reference (proof it never reached
// domain processing).
func assertC1Isolated(t *testing.T, resp *http.Response, pool *db.Pool, tenantID uuid.UUID, ref string, logOutput string) {
	t.Helper()
	if resp.StatusCode == http.StatusUnauthorized {
		t.Fatal("a DB-gate rejection during credential resolution must NEVER surface as the uniform 401 (security review C1)")
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header (C2)")
	}
	if !strings.Contains(logOutput, `"tier":"db_gate"`) {
		t.Fatalf("expected a webhook_admission_rejected tier=db_gate log line, got: %s", logOutput)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenantID, ref); got != 0 {
		t.Fatalf("a DB-gate-rejected callback must post ZERO rows: got %d", got)
	}
}
