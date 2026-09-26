package secretstore_test

// Circuit breaker, cache and fail-closed behaviour of the Fetcher
// (security review 07-w2a-design-review-security.md §5). The memory backend
// is used as the store; its secrets are generated at runtime.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type harness struct {
	t      *testing.T
	mem    *memstore.Store
	f      *secretstore.Fetcher
	clk    *clock
	key    []byte
	tamper atomic.Bool
	logs   *bytes.Buffer
	logMu  sync.Mutex
	tenant uuid.UUID
}

func (h *harness) fp(secret []byte) string {
	mac := hmac.New(sha256.New, h.key)
	mac.Write(secret)
	if h.tamper.Load() {
		mac.Write([]byte{1})
	}
	return "fp1:" + hex.EncodeToString(mac.Sum(nil))
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (w lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, mem: memstore.New(), clk: &clock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}, logs: &bytes.Buffer{}, tenant: uuid.New()}
	h.key = randBytes(t, 32)
	router, err := memstore.NewRouter(h.mem)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(lockedWriter{&h.logMu, h.logs}, nil))
	h.f = secretstore.NewFetcher(router, h.fp, secretstore.WithClock(h.clk.Now), secretstore.WithLogger(logger),
		secretstore.WithSleep(func(context.Context, time.Duration) {}))
	return h
}

func (h *harness) logText() string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.logs.String()
}

func randBytes(t *testing.T, n int) []byte {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

// put stores a fresh secret under a fresh ref and returns the parsed ref
// and its fingerprint.
func (h *harness) put(tenant uuid.UUID) (secretstore.Ref, string, []byte) {
	h.t.Helper()
	raw := fmt.Sprintf("memory://v/provider-creds/%s/casino/acme/n-%s?version=v1", tenant, uuid.NewString()[:8])
	ref, err := secretstore.ParseRef(raw)
	if err != nil {
		h.t.Fatal(err)
	}
	secret := randBytes(h.t, 32)
	h.mem.Put(raw, secret)
	return ref, h.fp(secret), secret
}

func (h *harness) fetch(ref secretstore.Ref, fp string) (secretstore.Secret, error) {
	return h.f.Fetch(context.Background(), h.tenant, ref, fp)
}

func classOf(err error) secretstore.ErrorClass { return secretstore.ClassOf(err) }

// tripBreaker makes three consecutive counting failures on three distinct
// refs (a negative-cache hit would not reach the store).
func (h *harness) tripBreaker() {
	h.t.Helper()
	h.mem.FailAll(secretstore.ClassUnavailable)
	for i := 0; i < secretstore.BreakerTripThreshold; i++ {
		ref, fp, _ := h.put(h.tenant)
		if _, err := h.fetch(ref, fp); classOf(err) != secretstore.ClassUnavailable {
			h.t.Fatalf("expected a counting failure, got %v", err)
		}
	}
}

func TestStoreBreaker_OpensAfterThreeConsecutiveFailures(t *testing.T) {
	h := newHarness(t)
	h.mem.FailAll(secretstore.ClassUnavailable)
	for i := 0; i < secretstore.BreakerTripThreshold-1; i++ {
		ref, fp, _ := h.put(h.tenant)
		_, _ = h.fetch(ref, fp)
		if s := h.f.BreakerState("memory"); s != "closed" {
			t.Fatalf("after %d failures the breaker must still be closed, got %s", i+1, s)
		}
	}
	// One success resets the count.
	h.mem.FailAll(0)
	ref, fp, _ := h.put(h.tenant)
	if _, err := h.fetch(ref, fp); err != nil {
		t.Fatal(err)
	}
	h.tripBreaker()
	if s := h.f.BreakerState("memory"); s != "open" {
		t.Fatalf("three consecutive counting failures must open the breaker, got %s", s)
	}
}

func TestStoreBreaker_FailsFastWhileOpen(t *testing.T) {
	h := newHarness(t)
	h.tripBreaker()
	h.mem.FailAll(0)
	ref, fp, _ := h.put(h.tenant)
	before := h.mem.Calls()
	start := time.Now()
	_, err := h.fetch(ref, fp)
	elapsed := time.Since(start)
	if classOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("open breaker must fail closed, got %v", err)
	}
	if h.mem.Calls() != before {
		t.Fatalf("open breaker made %d store calls", h.mem.Calls()-before)
	}
	if elapsed > 5*time.Millisecond {
		t.Fatalf("open breaker took %s, want < 5ms", elapsed)
	}
}

func TestStoreBreaker_HalfOpenSingleProbe(t *testing.T) {
	h := newHarness(t)
	h.tripBreaker()
	h.mem.FailAll(0)
	h.clk.Advance(secretstore.BreakerInitialCooldown)
	h.mem.Block()
	refA, fpA, _ := h.put(h.tenant)
	refB, fpB, _ := h.put(h.tenant)
	before := h.mem.Calls()

	var wg sync.WaitGroup
	wg.Add(1)
	var probeErr error
	go func() {
		defer wg.Done()
		_, probeErr = h.fetch(refA, fpA)
	}()
	// Wait until the probe is inside the store.
	deadline := time.Now().Add(2 * time.Second)
	for h.mem.Calls() == before && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if h.mem.Calls() != before+1 {
		t.Fatal("the half-open probe never reached the store")
	}
	if _, err := h.fetch(refB, fpB); classOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("a second caller during the probe must fail fast, got %v", err)
	}
	if h.mem.Calls() != before+1 {
		t.Fatalf("exactly one probe may reach the store, got %d calls", h.mem.Calls()-before)
	}
	h.mem.Unblock()
	wg.Wait()
	if probeErr != nil {
		t.Fatalf("probe: %v", probeErr)
	}
	if s := h.f.BreakerState("memory"); s != "closed" {
		t.Fatalf("a successful probe must close the breaker, got %s", s)
	}
}

func TestStoreBreaker_CooldownBackoffCapped(t *testing.T) {
	h := newHarness(t)
	h.tripBreaker()
	probeFails := func(wait time.Duration) {
		t.Helper()
		h.clk.Advance(wait - time.Second)
		if s := h.f.BreakerState("memory"); s != "open" {
			t.Fatalf("1s before the %s cooldown ends the breaker must be open, got %s", wait, s)
		}
		h.clk.Advance(time.Second)
		ref, fp, _ := h.put(h.tenant)
		if _, err := h.fetch(ref, fp); classOf(err) != secretstore.ClassUnavailable {
			t.Fatalf("probe must fail, got %v", err)
		}
	}
	probeFails(15 * time.Second) // -> 30s
	probeFails(30 * time.Second) // -> 60s
	probeFails(60 * time.Second) // -> capped at 60s
	probeFails(60 * time.Second)
	// A successful probe resets to 15s.
	h.clk.Advance(60 * time.Second)
	h.mem.FailAll(0)
	ref, fp, _ := h.put(h.tenant)
	if _, err := h.fetch(ref, fp); err != nil {
		t.Fatal(err)
	}
	h.tripBreaker()
	h.clk.Advance(15 * time.Second)
	if s := h.f.BreakerState("memory"); s != "half_open" {
		t.Fatalf("after a successful probe the cooldown must reset to 15s, got %s", s)
	}
}

func TestStoreBreaker_PerRefErrorsDoNotTrip(t *testing.T) {
	h := newHarness(t)
	for _, class := range []secretstore.ErrorClass{secretstore.ClassNotFound, secretstore.ClassAccessDenied,
		secretstore.ClassInvalidVersion, secretstore.ClassStoreConfig} {
		for i := 0; i < 5; i++ {
			ref, fp, _ := h.put(h.tenant)
			h.mem.FailRef(ref.String(), class)
			if _, err := h.fetch(ref, fp); classOf(err) != class {
				t.Fatalf("got %v, want %s", err, class)
			}
		}
	}
	// Fingerprint mismatches do not trip it either.
	for i := 0; i < 5; i++ {
		ref, _, _ := h.put(h.tenant)
		if _, err := h.fetch(ref, "fp1:"+hex.EncodeToString(randBytes(t, 32))); classOf(err) != secretstore.ClassIntegrity {
			t.Fatalf("got %v", err)
		}
	}
	if s := h.f.BreakerState("memory"); s != "closed" {
		t.Fatalf("per-ref errors must not trip the breaker, got %s", s)
	}
}

func TestStoreCache_ServesStaleWithinMaxStale(t *testing.T) {
	h := newHarness(t)
	ref, fp, secret := h.put(h.tenant)
	if _, err := h.fetch(ref, fp); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(secretstore.CacheMaxStale - time.Minute)
	h.mem.FailAll(secretstore.ClassUnavailable)
	s, err := h.fetch(ref, fp)
	if err != nil || !bytes.Equal(s.Bytes(), secret) {
		t.Fatalf("a stale entry within max-stale must be served while the store fails: %v", err)
	}
	// Also while the breaker is open.
	h.tripBreaker()
	if s, err := h.fetch(ref, fp); err != nil || !bytes.Equal(s.Bytes(), secret) {
		t.Fatalf("a stale entry within max-stale must be served while the breaker is open: %v", err)
	}
}

func TestStoreCache_NoStaleBeyondMaxStale(t *testing.T) {
	h := newHarness(t)
	ref, fp, _ := h.put(h.tenant)
	if _, err := h.fetch(ref, fp); err != nil {
		t.Fatal(err)
	}
	h.clk.Advance(secretstore.CacheMaxStale)
	h.mem.FailAll(secretstore.ClassUnavailable)
	if _, err := h.fetch(ref, fp); classOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("beyond max-stale nothing may be served, got %v", err)
	}
}

func TestStoreCache_RefreshNotEvict(t *testing.T) {
	h := newHarness(t)
	ref, fp, _ := h.put(h.tenant)
	if _, err := h.fetch(ref, fp); err != nil {
		t.Fatal(err)
	}
	calls := h.mem.Calls()
	h.clk.Advance(secretstore.CacheTTL - time.Second)
	if _, err := h.fetch(ref, fp); err != nil || h.mem.Calls() != calls {
		t.Fatalf("within the TTL the entry is served from cache: calls %d -> %d, %v", calls, h.mem.Calls(), err)
	}
	h.clk.Advance(time.Second)
	if _, err := h.fetch(ref, fp); err != nil || h.mem.Calls() != calls+1 {
		t.Fatalf("at the TTL the entry is refreshed from the store: calls %d -> %d, %v", calls, h.mem.Calls(), err)
	}
	if h.f.CacheLen() != 1 {
		t.Fatalf("refresh must not evict: cache len %d", h.f.CacheLen())
	}
	// After a failed refresh the entry is still there and served (stale).
	h.clk.Advance(secretstore.CacheTTL)
	h.mem.FailAll(secretstore.ClassUnavailable)
	if _, err := h.fetch(ref, fp); err != nil || h.f.CacheLen() != 1 {
		t.Fatalf("a failed refresh must keep and serve the entry: %v len=%d", err, h.f.CacheLen())
	}
}

// TestStoreCache_FingerprintComparedOnHit (T12): a cached value whose
// keyed fingerprint no longer equals the row's is never served, even
// fresh, and raises the P1 integrity alert.
func TestStoreCache_FingerprintComparedOnHit(t *testing.T) {
	h := newHarness(t)
	ref, fp, _ := h.put(h.tenant)
	if _, err := h.fetch(ref, fp); err != nil {
		t.Fatal(err)
	}
	calls := h.mem.Calls()
	h.tamper.Store(true) // the cached bytes no longer fingerprint to fp
	if _, err := h.fetch(ref, fp); classOf(err) != secretstore.ClassIntegrity {
		t.Fatalf("a cache hit whose fingerprint differs must fail closed as integrity, got %v", err)
	}
	if h.mem.Calls() != calls {
		t.Fatal("the mismatch must be detected on the cache hit itself")
	}
	if !bytes.Contains([]byte(h.logText()), []byte(`"alert":"P1"`)) {
		t.Fatalf("a P1 integrity alert must be logged, got %s", h.logText())
	}
}

func TestStoreCache_NoCrossTenantHit(t *testing.T) {
	h := newHarness(t)
	ref, fp, _ := h.put(h.tenant)
	if _, err := h.fetch(ref, fp); err != nil {
		t.Fatal(err)
	}
	calls := h.mem.Calls()
	other := uuid.New()
	// Same ref and fingerprint, another tenant: never a cache hit.
	h.mem.FailAll(secretstore.ClassNotFound)
	if _, err := h.f.Fetch(context.Background(), other, ref, fp); classOf(err) != secretstore.ClassNotFound || h.mem.Calls() != calls+1 {
		t.Fatalf("another tenant must miss the cache and go to the store: %v calls %d -> %d", err, calls, h.mem.Calls())
	}
}

func TestStoreFetch_SingleflightOneStoreCall(t *testing.T) {
	h := newHarness(t)
	ref, fp, secret := h.put(h.tenant)
	h.mem.Block()
	const n = 20
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s, err := h.fetch(ref, fp)
			if err == nil && !bytes.Equal(s.Bytes(), secret) {
				err = errors.New("wrong secret")
			}
			errs[i] = err
		}(i)
	}
	time.Sleep(20 * time.Millisecond)
	h.mem.Unblock()
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if h.mem.Calls() != 1 {
		t.Fatalf("N concurrent callers for one key must make exactly one store call, got %d", h.mem.Calls())
	}
}

func TestStoreFetch_ConcurrencyBoundedAtFour(t *testing.T) {
	h := newHarness(t)
	h.mem.Block()
	const n = 12
	var wg sync.WaitGroup
	var failedFast atomic.Int64
	for i := 0; i < n; i++ {
		ref, fp, _ := h.put(h.tenant)
		wg.Add(1)
		go func() {
			defer wg.Done()
			start := time.Now()
			_, err := h.fetch(ref, fp)
			if err != nil && time.Since(start) < time.Second {
				failedFast.Add(1)
			}
		}()
	}
	time.Sleep(500 * time.Millisecond)
	if m := h.mem.MaxConcurrent(); m > secretstore.MaxConcurrentStoreCalls {
		t.Fatalf("at most %d store calls may be in flight, saw %d", secretstore.MaxConcurrentStoreCalls, m)
	}
	h.mem.Unblock()
	wg.Wait()
	if failedFast.Load() < n-secretstore.MaxConcurrentStoreCalls {
		t.Fatalf("callers beyond the slot limit must fail fast after the slot wait, only %d did", failedFast.Load())
	}
}

func TestStoreFetch_NegativeCache(t *testing.T) {
	h := newHarness(t)
	// Counting failure: negative-cached for 5s.
	ref, fp, _ := h.put(h.tenant)
	h.mem.FailRef(ref.String(), secretstore.ClassUnavailable)
	_, _ = h.fetch(ref, fp)
	calls := h.mem.Calls()
	h.mem.FailRef(ref.String(), 0)
	h.clk.Advance(secretstore.NegativeTTLCounting - time.Millisecond)
	if _, err := h.fetch(ref, fp); classOf(err) != secretstore.ClassUnavailable || h.mem.Calls() != calls {
		t.Fatalf("within 5s the counting failure is negative-cached: %v", err)
	}
	h.clk.Advance(time.Millisecond)
	if _, err := h.fetch(ref, fp); err != nil {
		t.Fatalf("after 5s the store is asked again: %v", err)
	}
	// Per-ref failure: negative-cached for 30s.
	ref2, fp2, _ := h.put(h.tenant)
	h.mem.FailRef(ref2.String(), secretstore.ClassNotFound)
	_, _ = h.fetch(ref2, fp2)
	h.mem.FailRef(ref2.String(), 0)
	calls = h.mem.Calls()
	h.clk.Advance(secretstore.NegativeTTLPerRef - time.Millisecond)
	if _, err := h.fetch(ref2, fp2); classOf(err) != secretstore.ClassNotFound || h.mem.Calls() != calls {
		t.Fatalf("within 30s the per-ref failure is negative-cached: %v", err)
	}
	h.clk.Advance(time.Millisecond)
	if _, err := h.fetch(ref2, fp2); err != nil {
		t.Fatalf("after 30s the store is asked again: %v", err)
	}
}

func TestStoreFetch_IntegrityAlertRateLimited(t *testing.T) {
	h := newHarness(t)
	ref, _, _ := h.put(h.tenant)
	wrong := "fp1:" + hex.EncodeToString(randBytes(t, 32))
	count := func() int { return bytes.Count([]byte(h.logText()), []byte(`provider_credential_integrity_failure`)) }
	_, _ = h.fetch(ref, wrong)
	h.clk.Advance(secretstore.NegativeTTLPerRef)
	_, _ = h.fetch(ref, wrong)
	if count() != 1 {
		t.Fatalf("the P1 alert must be rate-limited to 1 per ref per 5 min, got %d", count())
	}
	h.clk.Advance(secretstore.IntegrityAlertInterval)
	_, _ = h.fetch(ref, wrong)
	if count() != 2 {
		t.Fatalf("after 5 min a new alert is allowed, got %d", count())
	}
	if bytes.Contains([]byte(h.logText()), []byte(hex.EncodeToString(h.key))) {
		t.Fatal("the log carries key material")
	}
}

func TestStoreFetch_NoBackendFailsClosed(t *testing.T) {
	h := newHarness(t)
	ref, err := secretstore.ParseRef(fmt.Sprintf("devfile://provider-creds/%s/casino/acme/n?version=1", h.tenant))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.fetch(ref, "fp1:"+hex.EncodeToString(randBytes(t, 32))); classOf(err) != secretstore.ClassNoBackend {
		t.Fatalf("a scheme with no backend must be no_backend, got %v", err)
	}
}
