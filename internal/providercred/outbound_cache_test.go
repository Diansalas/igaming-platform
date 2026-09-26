package providercred

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDerivedTokenCache_BoundEvictsOldest proves CODE-HYGIENE-10.3-1 item
// 2: the cache does not grow without bound. Once maxSize is exceeded, the
// oldest entry is evicted (and its bytes zeroed) rather than the cache
// growing forever, and the order list never grows past the same bound as
// the entries map (an orphaned list element would silently shrink the
// effective bound over time - code review F-3).
func TestDerivedTokenCache_BoundEvictsOldest(t *testing.T) {
	now := time.Now()
	cache := NewDerivedTokenCache(func() time.Time { return now })
	cache.maxSize = 3 // white-box: keep the test fast, bound behaviour is size-agnostic

	tenant := uuid.New()
	creds := make([]OutboundCredential, 0, 4)
	for i := 0; i < 4; i++ {
		creds = append(creds, OutboundCredential{
			TenantID: tenant, HandleID: uuid.New(), Fingerprint: "fp",
		})
	}

	for i, c := range creds {
		cache.Put(c, []byte{byte(i), byte(i), byte(i)}, now.Add(time.Minute))
	}

	if got := len(cache.entries); got != 3 {
		t.Fatalf("expected the cache to stay at its bound of 3 entries, got %d", got)
	}
	if got := cache.order.Len(); got != len(cache.entries) {
		t.Fatalf("expected the order list to track the entries map exactly (no orphaned elements), order.Len()=%d entries=%d", got, len(cache.entries))
	}

	// The oldest (creds[0]) must have been evicted.
	if _, ok := cache.Get(creds[0]); ok {
		t.Fatal("expected the oldest entry to be evicted once the bound was exceeded")
	}
	// The three most recent must still be present.
	for i := 1; i < 4; i++ {
		if _, ok := cache.Get(creds[i]); !ok {
			t.Fatalf("expected entry %d to still be present", i)
		}
	}
}

// TestDerivedTokenCache_BoundEvictionZeroesTokenBytes proves zeroing
// through the ACTUAL bound-eviction path in Put (not just a direct call to
// removeElementLocked, which a mutation of Put's own eviction loop - e.g.
// replacing d.removeElementLocked(d.order.Front()) with a bare
// d.order.Remove/delete pair - would not be caught by; code review F-3).
func TestDerivedTokenCache_BoundEvictionZeroesTokenBytes(t *testing.T) {
	now := time.Now()
	cache := NewDerivedTokenCache(func() time.Time { return now })
	cache.maxSize = 1

	tenant := uuid.New()
	first := OutboundCredential{TenantID: tenant, HandleID: uuid.New(), Fingerprint: "fp"}
	second := OutboundCredential{TenantID: tenant, HandleID: uuid.New(), Fingerprint: "fp"}

	cache.Put(first, []byte("first-secret-bytes"), now.Add(time.Hour))
	el, ok := cache.entries[keyFor(first)]
	if !ok {
		t.Fatal("expected the first entry to be present before the bound is exceeded")
	}
	firstEntry := el.Value.(*derivedEntry) // captured BEFORE the evicting Put

	cache.Put(second, []byte("second-secret-bytes"), now.Add(time.Hour)) // exceeds maxSize=1, evicts first

	if _, ok := cache.entries[keyFor(first)]; ok {
		t.Fatal("expected the first entry to be evicted by the bound")
	}
	if got := cache.order.Len(); got != len(cache.entries) {
		t.Fatalf("expected the order list to track the entries map exactly after a bound eviction, order.Len()=%d entries=%d", got, len(cache.entries))
	}
	for i, b := range firstEntry.token {
		if b != 0 {
			t.Fatalf("expected the bound-evicted entry's token bytes to be zeroed via Put's own eviction path, byte %d = %x", i, b)
		}
	}
}

// TestDerivedTokenCache_RotationEvictsStaleFingerprint proves that once a
// handle's credential fingerprint rotates, the old fingerprint's entry is
// proactively evicted (not just unreachable) as soon as the new one is
// stored - it does not linger until it happens to expire or the cache
// happens to fill up.
func TestDerivedTokenCache_RotationEvictsStaleFingerprint(t *testing.T) {
	now := time.Now()
	cache := NewDerivedTokenCache(func() time.Time { return now })

	tenant := uuid.New()
	handle := uuid.New()
	oldCred := OutboundCredential{TenantID: tenant, HandleID: handle, Fingerprint: "old-fp"}
	newCred := OutboundCredential{TenantID: tenant, HandleID: handle, Fingerprint: "new-fp"}

	cache.Put(oldCred, []byte("old-token-bytes"), now.Add(time.Hour))
	if _, ok := cache.Get(oldCred); !ok {
		t.Fatal("expected the old fingerprint's entry to be present before rotation")
	}
	if got := len(cache.entries); got != 1 {
		t.Fatalf("expected exactly 1 entry before rotation, got %d", got)
	}

	cache.Put(newCred, []byte("new-token-bytes"), now.Add(time.Hour))

	if _, ok := cache.Get(oldCred); ok {
		t.Fatal("expected the old fingerprint's entry to be evicted by the rotation, not merely unreachable")
	}
	if got, ok := cache.Get(newCred); !ok || string(got) != "new-token-bytes" {
		t.Fatal("expected the new fingerprint's entry to be served")
	}
	if got := len(cache.entries); got != 1 {
		t.Fatalf("expected exactly 1 entry after rotation (old evicted, new inserted), got %d", got)
	}
	if got := cache.order.Len(); got != len(cache.entries) {
		t.Fatalf("expected the order list to track the entries map exactly after a rotation eviction (no orphaned elements), order.Len()=%d entries=%d", got, len(cache.entries))
	}
}

// TestDerivedTokenCache_RotationZeroesEvictedTokenBytes proves zeroing
// through the ACTUAL rotation-eviction path in Put (not just a direct call
// to removeElementLocked). Without this, a mutation of the rotation loop
// that does e.g. delete(d.entries, k)/d.order.Remove(el) directly instead
// of calling d.removeElementLocked(el) would survive every other test in
// this file (code review F-3's exact failure scenario).
func TestDerivedTokenCache_RotationZeroesEvictedTokenBytes(t *testing.T) {
	now := time.Now()
	cache := NewDerivedTokenCache(func() time.Time { return now })

	tenant := uuid.New()
	handle := uuid.New()
	oldCred := OutboundCredential{TenantID: tenant, HandleID: handle, Fingerprint: "old-fp"}
	newCred := OutboundCredential{TenantID: tenant, HandleID: handle, Fingerprint: "new-fp"}

	cache.Put(oldCred, []byte("old-token-bytes"), now.Add(time.Hour))
	el, ok := cache.entries[keyFor(oldCred)]
	if !ok {
		t.Fatal("expected the old fingerprint's entry to be present before rotation")
	}
	oldEntry := el.Value.(*derivedEntry) // captured BEFORE the rotating Put

	cache.Put(newCred, []byte("new-token-bytes"), now.Add(time.Hour))

	for i, b := range oldEntry.token {
		if b != 0 {
			t.Fatalf("expected the rotation-evicted entry's token bytes to be zeroed via Put's own rotation path, byte %d = %x", i, b)
		}
	}
}

// TestDerivedTokenCache_EvictionZeroesTokenBytes proves the shared
// eviction primitive itself (removeElementLocked) zeroes a token, in
// isolation from Put's two calling paths (bound and rotation), which have
// their own dedicated tests above.
func TestDerivedTokenCache_EvictionZeroesTokenBytes(t *testing.T) {
	now := time.Now()
	cache := NewDerivedTokenCache(func() time.Time { return now })

	tenant := uuid.New()
	handle := uuid.New()
	cred := OutboundCredential{TenantID: tenant, HandleID: handle, Fingerprint: "fp"}
	cache.Put(cred, []byte("super-secret-bytes"), now.Add(time.Hour))

	el, ok := cache.entries[keyFor(cred)]
	if !ok {
		t.Fatal("expected entry to be present")
	}
	entry := el.Value.(*derivedEntry)

	cache.removeElementLocked(el)

	for i, b := range entry.token {
		if b != 0 {
			t.Fatalf("expected evicted token bytes to be zeroed, byte %d = %x", i, b)
		}
	}
}

// TestDerivedTokenCache_FormattingRedactsToken proves derivedTokenBytes
// (and therefore any derivedEntry holding one) never prints its bytes
// through fmt's standard verbs - the exposure code review F-2 flagged once
// derivedEntry.token stopped being a secretstore.Secret. This is what
// makes a future debug helper, a failed assertion's t.Logf("%+v", ...), or
// any other formatting of a *derivedEntry safe.
func TestDerivedTokenCache_FormattingRedactsToken(t *testing.T) {
	const plaintext = "super-secret-derived-token-bytes"
	tok := newDerivedTokenBytes([]byte(plaintext))
	entry := &derivedEntry{
		key:     derivedKey{tenant: uuid.New(), handle: uuid.New(), fingerprint: "fp"},
		token:   tok,
		expires: time.Now().Add(time.Hour),
	}

	renderings := []string{
		fmt.Sprintf("%v", tok),
		fmt.Sprintf("%+v", tok),
		fmt.Sprintf("%#v", tok),
		fmt.Sprintf("%v", *entry),
		fmt.Sprintf("%+v", *entry),
		fmt.Sprintf("%#v", *entry),
		fmt.Sprintf("%v", entry),
		fmt.Sprintf("%+v", entry),
	}
	for _, r := range renderings {
		if strings.Contains(r, plaintext) {
			t.Fatalf("formatting leaked the derived token's plaintext bytes: %q", r)
		}
		if !strings.Contains(r, redactedDerivedToken) {
			t.Fatalf("expected the redaction marker %q in formatted output, got: %q", redactedDerivedToken, r)
		}
	}
}

// TestDerivedTokenCache_ConcurrentPutGetRotate exercises Put/Get/rotation
// from many goroutines against a shared cache under -race, so the claim
// that every access to entries/order happens under d.mu (code review's own
// "Verified correct" note on item 2, and F-3's request for a test that
// actually exercises the locking) is backed by a race-detector run rather
// than code inspection alone. It asserts no panics/races; it does not
// assert which goroutine's write "wins" a given key, since concurrent
// writers to the same (tenant, handle) key race by design (the last
// commit wins, exactly like two concurrent real resolutions would).
func TestDerivedTokenCache_ConcurrentPutGetRotate(t *testing.T) {
	now := time.Now()
	var mu sync.Mutex // guards `now` itself, read/written by the test's own goroutines below
	cache := NewDerivedTokenCache(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	})

	const goroutines = 16
	const opsPerGoroutine = 200
	tenant := uuid.New()
	handles := make([]uuid.UUID, 4)
	for i := range handles {
		handles[i] = uuid.New()
	}

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < opsPerGoroutine; i++ {
				h := handles[i%len(handles)]
				cred := OutboundCredential{
					TenantID: tenant, HandleID: h,
					Fingerprint: fmt.Sprintf("fp-%d-%d", g, i%3), // rotates across a small set of fingerprints per handle
				}
				mu.Lock()
				expiry := now.Add(time.Minute)
				mu.Unlock()
				cache.Put(cred, []byte{byte(g), byte(i)}, expiry)
				cache.Get(cred)
			}
		}(g)
	}
	wg.Wait()

	if got := cache.order.Len(); got != len(cache.entries) {
		t.Fatalf("expected the order list to track the entries map exactly after concurrent use, order.Len()=%d entries=%d", got, len(cache.entries))
	}
}
