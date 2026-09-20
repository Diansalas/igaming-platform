// Package conformance provides reusable httptest.Server-backed test
// scenarios for exercising internal/providers/httpclient.Client's own
// behavior: success, timeout, transport failure, 4xx/5xx status,
// malformed body, flaky-then-succeeds, and duplicate-call observation.
//
// These are provider-BOUNDARY / contract-test helpers for the generic
// httpclient.Client, per docs/decisions/0080-provider-integration-
// readiness-without-external-contracts.md Decision 3. They are NOT
// integration tests of any external "Dummy Sportsbook"/"Dummy Casino" API
// or any other real vendor - no such API's documentation, base URL, or
// credentials exist in this environment (see ADR 0080's Context section).
// Every server here is a local httptest.Server simulating a generic HTTP
// failure/success mode a real provider *might* someday exhibit, not a
// fake of any specific real provider's actual documented contract. When
// a real adapter is eventually built against real documentation, this
// same harness can be pointed at a fake server shaped like that real
// contract to validate the adapter's own mapping - but nothing in this
// package encodes a guessed contract today.
package conformance

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"
)

// CallCounter is a concurrency-safe call counter a test can inspect after
// exercising a server built by this package.
type CallCounter struct {
	mu sync.Mutex
	n  int
}

// Count returns the number of times the counter has been incremented so
// far.
func (c *CallCounter) Count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

func (c *CallCounter) inc() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return c.n
}

// NewSuccessServer returns a server that responds to every request with
// statusCode and body.
func NewSuccessServer(t *testing.T, body []byte, statusCode int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(statusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// NewTimeoutServer returns a server that sleeps for delay before
// responding at all - a caller's Client should be configured with a
// timeout shorter than delay to exercise ErrProviderTimeout.
func NewTimeoutServer(t *testing.T, delay time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// NewTransportFailureServer returns a *httptest.Server that has already
// been closed, so any request against its URL hits a closed connection
// (connection refused) - the standard Go idiom for simulating a
// transport-level failure in tests. Callers must not register an
// additional t.Cleanup(srv.Close) for the returned server; it is already
// closed.
func NewTransportFailureServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	return srv
}

// NewStatusServer returns a server that responds to every request with
// statusCode and body - typically used with a 4xx or 5xx statusCode. Each
// response carries an X-Conformance-Call-Count header (starting at "1")
// so a test can confirm how many times the server was actually called
// without needing a separately-returned counter.
func NewStatusServer(t *testing.T, statusCode int, body []byte) *httptest.Server {
	t.Helper()
	counter := &CallCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := counter.inc()
		w.Header().Set("X-Conformance-Call-Count", strconv.Itoa(n))
		w.WriteHeader(statusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// NewMalformedBodyServer returns a server that responds 200 OK with
// rawBody verbatim - intended to be a body that fails whatever decode
// step the caller applies (e.g. invalid JSON).
func NewMalformedBodyServer(t *testing.T, rawBody []byte) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rawBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// NewFlakyServer returns a server that responds with 503 Service
// Unavailable for the first failCount requests, then thenStatusCode (with
// a small JSON body) for every request after that - for exercising
// retry-then-success. Every response carries an X-Conformance-Call-Count
// header reporting the 1-based request number the server just handled,
// so a test can confirm exactly how many attempts reached the network.
func NewFlakyServer(t *testing.T, failCount int, thenStatusCode int) *httptest.Server {
	t.Helper()
	counter := &CallCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := counter.inc()
		w.Header().Set("X-Conformance-Call-Count", strconv.Itoa(n))
		if n <= failCount {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporarily unavailable"}`))
			return
		}
		w.WriteHeader(thenStatusCode)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// NewDuplicateAckServer returns a server that always acknowledges 200 OK,
// plus a CallCounter the test can inspect. It exists to demonstrate a
// boundary this package deliberately does NOT cover: if a caller retries
// at a layer above httpclient.Client (e.g. because it lost the response
// to a genuinely successful call and cannot tell), this server will
// happily process the "duplicate" call again and ack it a second time -
// proving why domain-level idempotency enforcement (e.g. a unique
// constraint on (provider_id, provider_tx_id), per CLAUDE.md's ledger
// rules) is what actually prevents a double financial effect, not
// anything in httpclient.Client itself, which only relays each attempt
// faithfully.
func NewDuplicateAckServer(t *testing.T) (*httptest.Server, *CallCounter) {
	t.Helper()
	counter := &CallCounter{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := counter.inc()
		w.Header().Set("X-Conformance-Call-Count", strconv.Itoa(n))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ack":"ok"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, counter
}
