package httpserver

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// clockBoundedReader enforces a read deadline using an injected
// admission.Clock/Timer instead of a real OS-level connection deadline -
// it is the test-only fake armBodyReadDeadline is swapped to, so T11 can
// simulate "a stalled body sender loses its A4a slot at the deadline"
// (ADR 0097 §5.4/T11) driven entirely by FakeClock.Advance, never a real
// sleep (QA review item 1(a)).
type clockBoundedReader struct {
	r        io.Reader
	clock    admission.Clock
	deadline time.Time
}

var errClockDeadlineExceeded = &clockDeadlineError{}

type clockDeadlineError struct{}

func (*clockDeadlineError) Error() string { return "webhookauth: read deadline exceeded" }

func (c *clockBoundedReader) Read(p []byte) (int, error) {
	now := c.clock.Now()
	if !now.Before(c.deadline) {
		return 0, errClockDeadlineExceeded
	}
	type result struct {
		n   int
		err error
	}
	ch := make(chan result, 1)
	go func() {
		n, err := c.r.Read(p)
		ch <- result{n, err}
	}()
	timer := c.clock.NewTimer(c.deadline.Sub(now))
	select {
	case res := <-ch:
		timer.Stop()
		return res.n, res.err
	case <-timer.C():
		return 0, errClockDeadlineExceeded
	}
}

// TestAdmission_T11_SlowBody_ReleasesSlotAtDeadline is ADR 0097's T11,
// resolved per QA review item 1(a): armBodyReadDeadline is swapped for a
// FakeClock-driven fake, so the whole test runs with zero real sleeping
// and stays in the deterministic main lane - never the CI timing lane.
func TestAdmission_T11_SlowBody_ReleasesSlotAtDeadline(t *testing.T) {
	clock := admission.NewFakeClock(time.Unix(0, 0))

	orig := armBodyReadDeadline
	defer func() { armBodyReadDeadline = orig }()
	armBodyReadDeadline = func(w http.ResponseWriter, body io.Reader, deadline time.Time) io.Reader {
		return &clockBoundedReader{r: body, clock: clock, deadline: deadline}
	}

	rb := func(rate float64, burst int) WebhookRateBurst { return WebhookRateBurst{Rate: rate, Burst: burst} }
	settings := WebhookAdmissionSettings{
		Enabled:            true,
		Clock:              clock,
		BodyReadTimeout:    10 * time.Second,
		PreAuthRate:        map[string]WebhookRateBurst{"payments": rb(0.01, 3), "casino": rb(0.01, 3), "kyc": rb(0.01, 3)},
		PreAuthUnknownRate: map[string]WebhookRateBurst{"payments": rb(0.01, 2), "casino": rb(0.01, 2), "kyc": rb(0.01, 2)},
		VerifiedRate:       map[string]WebhookRateBurst{"payments": rb(0.01, 3), "casino": rb(0.01, 3), "kyc": rb(0.01, 3)},
		InFlightGlobal:     100, InFlightPerKey: 50, InFlightUnknown: 50,
		DBGateGlobal: 10, DBGatePerKey: 5, DBGateUnknown: 5, DBGateWait: 100 * time.Millisecond,
		DomainTxPerTenant: 10, DomainWait: 100 * time.Millisecond,
		DirectoryRefresh: time.Hour, DirectoryCap: 100,
		IdleEvict: time.Hour, VerifiedMaxKeys: 100, PerIPMaxKeys: 100,
	}
	rt := newWebhookAdmission(settings, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if rt == nil {
		t.Fatal("expected a non-nil admission runtime")
	}

	mock := payments.NewMockProvider("mock", "EUR", "USD")
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock}, payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)})
	deps := Deps{PaymentOrchestrator: orchestrator, webhookAdmission: rt, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	handler := newPaymentWebhookHandler(deps)

	// A body that never finishes sending (a stalled sender) - Close is
	// deferred so the never-returning real Read call inside
	// clockBoundedReader's goroutine unblocks at test teardown rather than
	// leaking past the test's own lifetime.
	pr, pw := io.Pipe()
	defer func() { _ = pw.Close() }()

	req := httptest.NewRequest(http.MethodPost, "/v1/webhooks/payments/some-tenant/mock", pr)
	req.SetPathValue("tenantSlug", "some-tenant")
	req.SetPathValue("providerID", "mock")
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		handler(rec, req)
		close(done)
	}()

	waitForFakeClockTimer(t, clock)

	// Before the deadline fires, the A4a slot must still be held.
	if global, _ := rt.inflight.InUse("payments|_unknown|_unknown"); global == 0 {
		t.Fatal("the A4a slot must be held while the body read is still pending")
	}

	clock.Advance(10 * time.Second)

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not return after the read deadline elapsed")
	}

	if rec.Code == http.StatusOK {
		t.Fatalf("a stalled body sender must never reach 200, got %d", rec.Code)
	}
	global, _ := rt.inflight.InUse("payments|_unknown|_unknown")
	if global != 0 {
		t.Fatalf("the A4a slot must be released once the handler returns: still %d in use", global)
	}
}

// waitForFakeClockTimer polls (bounded, real-time, generous) until clock
// has at least one pending timer - test synchronization only (mirrors
// internal/admission's own waitForTimerRegistered), never a pass/fail
// timing assertion on the code under test.
func waitForFakeClockTimer(t *testing.T, clock *admission.FakeClock) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if clock.PendingTimers() > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timer never registered")
}
