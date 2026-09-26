package secretstore_test

// Per-tenant fairness of the Fetcher (ADR 0094 §4.2 and §9.2; security
// co-sign conditions C8/C9). Wall-clock bounds follow security ruling (6):
// every bound is >= 2x the gap it discriminates ("fail fast" means
// < SlotWait/2), and outcomes are asserted on store-call counts and the
// admission counters first.

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/txscope"
)

const failFast = secretstore.SlotWait / 2

// degrade gives tenant one counting failure on a fresh ref (breaker still
// closed, consecutive = 1: degraded).
func (h *harness) degrade(tenant uuid.UUID) {
	h.t.Helper()
	ref, fp, _ := h.put(tenant)
	h.mem.FailRef(ref.String(), secretstore.ClassUnavailable)
	if _, err := h.f.Fetch(context.Background(), tenant, ref, fp); classOf(err) != secretstore.ClassUnavailable {
		h.t.Fatalf("expected a counting failure, got %v", err)
	}
	if s := h.f.BreakerState("memory", tenant); s != "closed" {
		h.t.Fatalf("one failure must not open the breaker, got %s", s)
	}
}

// waitFor polls cond for up to 2 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFetcher_PerTenantCap(t *testing.T) {
	h := newHarness(t)
	tenant := uuid.New()
	prefix := memstore.Namespace(tenant.String())
	h.mem.TrackMatching(prefix)
	h.mem.BlockMatching(prefix)
	defer h.mem.UnblockMatching(prefix)
	const n = secretstore.MaxConcurrentStoreCallsPerTenant + 1
	var wg sync.WaitGroup
	for i := 0; i < n-1; i++ {
		ref, fp, _ := h.put(tenant)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.f.Fetch(context.Background(), tenant, ref, fp)
		}()
	}
	waitFor(t, "the tenant's first P calls to reach the store", func() bool {
		return h.mem.MaxConcurrentMatching(prefix) == secretstore.MaxConcurrentStoreCallsPerTenant
	})
	ref, fp, _ := h.put(tenant)
	calls := h.mem.Calls()
	start := time.Now()
	_, err := h.f.Fetch(context.Background(), tenant, ref, fp)
	elapsed := time.Since(start)
	if classOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("a tenant over its per-tenant cap must fail closed, got %v", err)
	}
	if h.mem.Calls() != calls {
		t.Fatal("a caller over the per-tenant cap reached the store")
	}
	// A HEALTHY waiter must have waited SlotWait (not failed instantly) and
	// not much longer.
	if elapsed < secretstore.SlotWait-25*time.Millisecond || elapsed > secretstore.SlotWait+50*time.Millisecond {
		t.Fatalf("a healthy caller over the per-tenant cap took %s, want SlotWait (%s) -25ms/+50ms", elapsed, secretstore.SlotWait)
	}
	if m := h.mem.MaxConcurrentMatching(prefix); m > secretstore.MaxConcurrentStoreCallsPerTenant {
		t.Fatalf("tenant reached %d concurrent store calls, per-tenant cap is %d", m, secretstore.MaxConcurrentStoreCallsPerTenant)
	}
	h.mem.UnblockMatching(prefix)
	wg.Wait()
}

func TestFetcher_DegradedBudget_HealthyReserve(t *testing.T) {
	h := newHarness(t)
	degraded := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	var prefixes []string
	for _, d := range degraded {
		h.degrade(d)
		p := memstore.Namespace(d.String())
		h.mem.TrackMatching(p)
		h.mem.BlockMatching(p)
		prefixes = append(prefixes, p)
	}
	defer func() {
		for _, p := range prefixes {
			h.mem.UnblockMatching(p)
		}
	}()
	var wg sync.WaitGroup
	for _, d := range degraded {
		for i := 0; i < 2; i++ {
			ref, fp, _ := h.put(d)
			wg.Add(1)
			go func(d uuid.UUID) {
				defer wg.Done()
				_, _ = h.f.Fetch(context.Background(), d, ref, fp)
			}(d)
		}
	}
	waitFor(t, "the degraded budget to fill", func() bool {
		_, deg := h.f.AdmissionSnapshot()
		return deg == secretstore.MaxDegradedStoreCalls
	})
	time.Sleep(20 * time.Millisecond) // let any over-admission show up
	if _, deg := h.f.AdmissionSnapshot(); deg > secretstore.MaxDegradedStoreCalls {
		t.Fatalf("degraded tenants hold %d store calls, budget is %d", deg, secretstore.MaxDegradedStoreCalls)
	}
	var total int64
	for _, p := range prefixes {
		total += h.mem.MaxConcurrentMatching(p)
	}
	if total > int64(secretstore.MaxDegradedStoreCalls) {
		t.Fatalf("degraded tenants reached %d concurrent store calls, budget is %d", total, secretstore.MaxDegradedStoreCalls)
	}
	// A healthy tenant's cold fetch still gets a reserved slot at once.
	healthy := uuid.New()
	ref, fp, secret := h.put(healthy)
	start := time.Now()
	got, err := h.f.Fetch(context.Background(), healthy, ref, fp)
	if err != nil || !bytes.Equal(got.Bytes(), secret) {
		t.Fatalf("a healthy tenant's cold fetch must succeed while degraded tenants are blocked: %v", err)
	}
	if d := time.Since(start); d >= failFast {
		t.Fatalf("the healthy cold fetch took %s, want < %s (a reserved slot, no wait)", d, failFast)
	}
	for _, p := range prefixes {
		h.mem.UnblockMatching(p)
	}
	wg.Wait()
}

func TestFetcher_TenantBreakerIsolated(t *testing.T) {
	h := newHarness(t)
	a, b := h.tenant, uuid.New()
	h.tripBreaker()
	h.mem.FailAll(0)
	if s := h.f.BreakerState("memory", a); s != "open" {
		t.Fatalf("A's breaker = %s, want open", s)
	}
	if s := h.f.BreakerState("memory", b); s != "closed" {
		t.Fatalf("B's breaker = %s, want closed: one tenant's failures must not open another's", s)
	}
	ref, fp, secret := h.put(b)
	calls := h.mem.Calls()
	got, err := h.f.Fetch(context.Background(), b, ref, fp)
	if err != nil || !bytes.Equal(got.Bytes(), secret) || h.mem.Calls() != calls+1 {
		t.Fatalf("B's cold fetch must make one store call and succeed while A's breaker is open: %v calls=%d", err, h.mem.Calls()-calls)
	}
}

func TestFetcher_DegradedProbeNeverStarvesHealthy(t *testing.T) {
	h := newHarness(t)
	a := h.tenant
	h.tripBreaker()
	h.mem.FailAll(0)
	h.clk.Advance(secretstore.BreakerInitialCooldown)
	pa := memstore.Namespace(a.String())
	h.mem.TrackMatching(pa)
	h.mem.BlockMatching(pa)
	defer h.mem.UnblockMatching(pa)
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		ref, fp, _ := h.put(a)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.f.Fetch(context.Background(), a, ref, fp)
		}()
	}
	waitFor(t, "A's half-open probe to reach the store", func() bool { return h.mem.MaxConcurrentMatching(pa) >= 1 })
	time.Sleep(20 * time.Millisecond)
	if m := h.mem.MaxConcurrentMatching(pa); m != 1 {
		t.Fatalf("a half-open tenant burst reached the store %d times, want exactly the 1 probe", m)
	}
	b := uuid.New()
	ref, fp, _ := h.put(b)
	start := time.Now()
	if _, err := h.f.Fetch(context.Background(), b, ref, fp); err != nil {
		t.Fatalf("B's fetch during A's probe: %v", err)
	}
	if d := time.Since(start); d >= failFast {
		t.Fatalf("B waited %s behind A's probe, want < %s", d, failFast)
	}
	h.mem.UnblockMatching(pa)
	wg.Wait()
}

func TestFetcher_DegradedCallersFailFast(t *testing.T) {
	h := newHarness(t)
	a, b := uuid.New(), uuid.New()
	h.degrade(a)
	h.degrade(b)
	pa, pb := memstore.Namespace(a.String()), memstore.Namespace(b.String())
	h.mem.BlockMatching(pa)
	h.mem.BlockMatching(pb)
	defer h.mem.UnblockMatching(pa)
	defer h.mem.UnblockMatching(pb)
	// Fill the degraded budget: one blocked flight each for A and B.
	refA, fpA, _ := h.put(a)
	refB, fpB, _ := h.put(b)
	var wg sync.WaitGroup
	for _, c := range []struct {
		t   uuid.UUID
		ref secretstore.Ref
		fp  string
	}{{a, refA, fpA}, {b, refB, fpB}} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.f.Fetch(context.Background(), c.t, c.ref, c.fp)
		}()
	}
	waitFor(t, "the degraded budget to fill", func() bool {
		_, deg := h.f.AdmissionSnapshot()
		return deg == secretstore.MaxDegradedStoreCalls
	})
	calls := h.mem.Calls()
	// A degraded OWNER with no degraded budget never waits.
	ref2, fp2, _ := h.put(a)
	start := time.Now()
	if _, err := h.f.Fetch(context.Background(), a, ref2, fp2); classOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("a degraded owner over budget must fail closed, got %v", err)
	}
	if d := time.Since(start); d >= failFast {
		t.Fatalf("a degraded owner waited %s, want < %s", d, failFast)
	}
	// A degraded FOLLOWER of A's in-flight flight never waits either.
	start = time.Now()
	if _, err := h.f.Fetch(context.Background(), a, refA, fpA); classOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("a degraded follower must fail closed, got %v", err)
	}
	if d := time.Since(start); d >= failFast {
		t.Fatalf("a degraded follower waited %s, want < %s", d, failFast)
	}
	if h.mem.Calls() != calls {
		t.Fatalf("fail-fast degraded callers made %d store calls", h.mem.Calls()-calls)
	}
	h.mem.UnblockMatching(pa)
	h.mem.UnblockMatching(pb)
	wg.Wait()
}

func TestFetcher_AdmissionLossNotNegativeCached(t *testing.T) {
	h := newHarness(t)
	// Fill every global slot with blocked calls of other healthy tenants.
	var blockers []string
	var wg sync.WaitGroup
	for i := 0; i < secretstore.MaxConcurrentStoreCalls; i++ {
		other := uuid.New()
		p := memstore.Namespace(other.String())
		h.mem.BlockMatching(p)
		blockers = append(blockers, p)
		ref, fp, _ := h.put(other)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = h.f.Fetch(context.Background(), other, ref, fp)
		}()
	}
	waitFor(t, "every global slot to be taken", func() bool {
		in, _ := h.f.AdmissionSnapshot()
		return in == secretstore.MaxConcurrentStoreCalls
	})
	ref, fp, secret := h.put(h.tenant)
	if _, err := h.fetch(ref, fp); classOf(err) != secretstore.ClassUnavailable {
		t.Fatalf("with every slot taken the fetch must fail after the slot wait, got %v", err)
	}
	for _, p := range blockers {
		h.mem.UnblockMatching(p)
	}
	wg.Wait()
	calls := h.mem.Calls()
	got, err := h.fetch(ref, fp)
	if err != nil || !bytes.Equal(got.Bytes(), secret) || h.mem.Calls() != calls+1 {
		t.Fatalf("an admission loss must not be negative-cached: the next fetch must reach the store and succeed (%v, calls %d)", err, h.mem.Calls()-calls)
	}
}

func TestFetcher_FetchRefusedWithTxHeld(t *testing.T) {
	h := newHarness(t)
	ref, fp, secret := h.put(h.tenant)
	if _, err := h.f.Fetch(txscope.Mark(context.Background()), h.tenant, ref, fp); classOf(err) != secretstore.ClassStoreConfig {
		t.Fatalf("a Fetch with a transaction held must fail closed as store_config, got %v", err)
	}
	if h.mem.Calls() != 0 || h.f.CacheLen() != 0 {
		t.Fatalf("a refused Fetch touched the store (%d) or the cache (%d)", h.mem.Calls(), h.f.CacheLen())
	}
	if in, _ := h.f.AdmissionSnapshot(); in != 0 {
		t.Fatal("a refused Fetch took a slot")
	}
	if !bytes.Contains([]byte(h.logText()), []byte("secret_fetch_with_tx_held")) {
		t.Fatal("a refused Fetch must log secret_fetch_with_tx_held")
	}
	// Nothing was negative-cached and the breaker is untouched.
	got, err := h.fetch(ref, fp)
	if err != nil || !bytes.Equal(got.Bytes(), secret) || h.mem.Calls() != 1 {
		t.Fatalf("after a refused Fetch the next one must be served normally: %v", err)
	}
	if s := h.f.BreakerState("memory", h.tenant); s != "closed" {
		t.Fatalf("breaker = %s", s)
	}
}

func TestRouter_GetDirectRefusedWithTxHeld(t *testing.T) {
	h := newHarness(t)
	ref, _, _ := h.put(h.tenant)
	if _, err := h.f.Router().GetDirect(txscope.Mark(context.Background()), ref); classOf(err) != secretstore.ClassStoreConfig {
		t.Fatalf("GetDirect with a transaction held must fail closed as store_config, got %v", err)
	}
	if h.mem.Calls() != 0 {
		t.Fatal("a refused GetDirect reached the store")
	}
	if _, err := h.f.Router().GetDirect(context.Background(), ref); err != nil {
		t.Fatalf("GetDirect without a transaction: %v", err)
	}
}

// TestFetcher_ColdStartFourRefs_NoRejection is security condition C8: a
// tenant whose 4 distinct refs are all cold at once (after a deploy or a
// rotation), at 150 ms store latency, must see 0 rejections under healthy
// conditions. It fails at P = 1 (the third ref waits > SlotWait) and
// passes at the pre-approved P = 2; SlotWait may not be widened instead.
func TestFetcher_ColdStartFourRefs_NoRejection(t *testing.T) {
	h := newHarness(t)
	h.mem.SetLatency(150 * time.Millisecond)
	tenant := uuid.New()
	var wg sync.WaitGroup
	var rejected atomic.Int64
	for i := 0; i < 4; i++ {
		ref, fp, _ := h.put(tenant)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.f.Fetch(context.Background(), tenant, ref, fp); err != nil {
				rejected.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := rejected.Load(); n != 0 {
		t.Fatalf("%d of 4 cold refs of one healthy tenant were rejected at 150 ms store latency (P=%d), want 0",
			n, secretstore.MaxConcurrentStoreCallsPerTenant)
	}
}

// TestFetcher_GlobalOutageRateBound is security condition C9: with N = 50
// tenants and a store that fails fast, over 120 s of (fake) time, total
// store attempts are bounded by N x 3 x (1 + StoreMaxRetries) to trip every
// tenant's breaker plus the probes each tenant's cooldown schedule allows
// (each probe also up to 1 + StoreMaxRetries attempts). One
// secret_store_multi_tenant_degraded warn line makes the outage visible.
func TestFetcher_GlobalOutageRateBound(t *testing.T) {
	h := newHarness(t)
	const n = 50
	const horizon = 120 * time.Second
	h.mem.FailAll(secretstore.ClassUnavailable)
	tenants := make([]uuid.UUID, n)
	for i := range tenants {
		tenants[i] = uuid.New()
	}
	for tick := time.Duration(0); tick <= horizon; tick += time.Second {
		for _, tn := range tenants {
			ref, fp, _ := h.put(tn) // a fresh ref: the negative cache must not be what bounds this
			_, _ = h.f.Fetch(context.Background(), tn, ref, fp)
		}
		h.clk.Advance(time.Second)
	}
	// Probes per tenant within the horizon: cooldowns 15 s, 30 s, 60 s, 60 s...
	// counted from the (at earliest) t = 0 trip.
	probes := 0
	for at, cd := time.Duration(0), secretstore.BreakerInitialCooldown; ; {
		at += cd
		if at > horizon {
			break
		}
		probes++
		if cd *= 2; cd > secretstore.BreakerMaxCooldown {
			cd = secretstore.BreakerMaxCooldown
		}
	}
	perAttempt := int64(1 + secretstore.StoreMaxRetries)
	bound := int64(n)*int64(secretstore.BreakerTripThreshold)*perAttempt + int64(n)*int64(probes)*perAttempt
	if got := h.mem.Calls(); got > bound {
		t.Fatalf("a global outage made %d store attempts over %s for %d tenants, bound %d", got, horizon, n, bound)
	}
	if !bytes.Contains([]byte(h.logText()), []byte("secret_store_multi_tenant_degraded")) {
		t.Fatal("a multi-tenant outage must raise secret_store_multi_tenant_degraded")
	}
	if c := bytes.Count([]byte(h.logText()), []byte("secret_store_multi_tenant_degraded")); c > int(horizon/secretstore.MultiTenantDegradedWarnInterval)+1 {
		t.Fatalf("the multi-tenant warn line must be rate-limited to 1 per %s, got %d", secretstore.MultiTenantDegradedWarnInterval, c)
	}
	t.Logf("store attempts %d, bound %d", h.mem.Calls(), bound)
}
