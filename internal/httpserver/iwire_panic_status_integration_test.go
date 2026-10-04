//go:build integration

package httpserver

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// I-wire final review F1 (security F-1, code review F1): a handler PANIC on a
// provider-callback route must reach the provider as a 5xx so it redelivers. A
// response flush placed in a defer turns the panic into an implicit 200 (the
// recover middleware's 500 is then dropped as a superfluous WriteHeader), the
// provider treats it as an acknowledgement and never retries: a lost deposit or
// casino credit. These tests drive the REAL server chain (request id, access
// log, recover) and assert the WIRE status and that the access log agrees.

type logRec struct {
	msg    string
	status int64
	path   string
}

// panicLogHandler records every record and panics when a record's message equals
// panicOn (a panic from inside the handler body, after its defers are
// registered).
type panicLogHandler struct {
	inner   slog.Handler
	mu      *sync.Mutex
	recs    *[]logRec
	panicOn string
}

func newPanicLogger(panicOn string) (*slog.Logger, func() []logRec) {
	var mu sync.Mutex
	var recs []logRec
	h := &panicLogHandler{inner: slog.NewTextHandler(discardWriter{}, &slog.HandlerOptions{Level: slog.LevelDebug}), mu: &mu, recs: &recs, panicOn: panicOn}
	return slog.New(h), func() []logRec { mu.Lock(); defer mu.Unlock(); return append([]logRec(nil), recs...) }
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

func (h *panicLogHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}
func (h *panicLogHandler) Handle(ctx context.Context, r slog.Record) error {
	lr := logRec{msg: r.Message}
	r.Attrs(func(a slog.Attr) bool {
		switch a.Key {
		case "status":
			lr.status = a.Value.Int64()
		case "path":
			lr.path = a.Value.String()
		}
		return true
	})
	h.mu.Lock()
	*h.recs = append(*h.recs, lr)
	h.mu.Unlock()
	if h.panicOn != "" && r.Message == h.panicOn {
		panic("injected panic: " + r.Message)
	}
	return h.inner.Handle(ctx, r)
}
func (h *panicLogHandler) WithAttrs(a []slog.Attr) slog.Handler {
	return &panicLogHandler{inner: h.inner.WithAttrs(a), mu: h.mu, recs: h.recs, panicOn: h.panicOn}
}
func (h *panicLogHandler) WithGroup(n string) slog.Handler {
	return &panicLogHandler{inner: h.inner.WithGroup(n), mu: h.mu, recs: h.recs, panicOn: h.panicOn}
}

func accessLogStatusFor(recs []logRec, pathSub string) (int64, bool) {
	for _, r := range recs {
		if r.msg == "http_request" && strings.Contains(r.path, pathSub) {
			return r.status, true
		}
	}
	return 0, false
}

func hasMsg(recs []logRec, msg string) bool {
	for _, r := range recs {
		if r.msg == msg {
			return true
		}
	}
	return false
}

// panicOnCallbackProvider wraps the mock so that HandleCallback - which
// Orchestrator.ReceiveVerifiedCallback calls INSIDE the alerting.InTx evidence
// transaction, after the handler registered its deferred alert work - panics
// while armed.
type panicOnCallbackProvider struct {
	*payments.MockProvider
	armed atomic.Bool
}

func (p *panicOnCallbackProvider) HandleCallback(ctx context.Context, req payments.InboundCallback, cred payments.WebhookCredential) (payments.CallbackEvent, error) {
	if p.armed.Load() {
		panic("injected panic inside the verified payment callback path")
	}
	return p.MockProvider.HandleCallback(ctx, req, cred)
}

func TestIWire_PaymentWebhookPanic_IsA5xxOnTheWire_AccessLogAgrees_ThenRedeliveryCredits(t *testing.T) {
	pool, issuer := testEnv(t)
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	pp := &panicOnCallbackProvider{MockProvider: mock}
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": pp},
		payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)})
	logger, recs := newPanicLogger("")
	srv := newFinancialTestServerWithLogger(t, pool, issuer, orchestrator, logger)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const amount int64 = 7000
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": "iw-pan-" + uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	ref := providerReferenceFromRedirectURL(intent.RedirectURL)
	cb := func() int {
		r := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mock.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amount, "EUR", "", false))
		defer func() { _ = r.Body.Close() }()
		return r.StatusCode
	}

	pp.armed.Store(true)
	status := cb()
	if status < 500 || status > 599 {
		t.Fatalf("a panic inside the verified payment callback path must be a 5xx on the wire (the provider redelivers), got %d", status)
	}
	if !hasMsg(recs(), "panic_recovered") {
		t.Fatal("the panic must have been recovered by the recover middleware")
	}
	if got, ok := accessLogStatusFor(recs(), "/v1/webhooks/payments"); !ok || got != int64(status) {
		t.Fatalf("the access log must agree with the wire: wire=%d log=%d (found=%v)", status, got, ok)
	}

	// The provider's redelivery (the panic is gone) is processed normally.
	pp.armed.Store(false)
	if s := cb(); s != http.StatusOK {
		t.Fatalf("redelivery after the panic must succeed, got %d", s)
	}
}

func TestIWire_CasinoCallbackPanic_IsA5xxOnTheWire_AccessLogAgrees(t *testing.T) {
	// The orphan win is an integrity rejection; the handler logs
	// casino_webhook_integrity_alert_bet_not_found AFTER registering its deferred
	// alert work. The injected logger panics exactly there.
	logger, recs := newPanicLogger("casino_webhook_integrity_alert_bet_not_found")
	e := newRejEnvHTTPWithLogger(t, logger)
	status := e.send(t, casino.CallbackEventWin, "pan-orph-win", "", "pan-orph-r", 1000, uuid.Nil)
	if status < 500 || status > 599 {
		t.Fatalf("a panic inside the casino callback handler must be a 5xx on the wire, got %d", status)
	}
	if !hasMsg(recs(), "panic_recovered") {
		t.Fatal("the panic must have been recovered by the recover middleware")
	}
	if got, ok := accessLogStatusFor(recs(), "/v1/webhooks/casino"); !ok || got != int64(status) {
		t.Fatalf("the access log must agree with the wire: wire=%d log=%d (found=%v)", status, got, ok)
	}
}
