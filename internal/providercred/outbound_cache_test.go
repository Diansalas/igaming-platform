package providercred

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestDerivedTokenCache_BoundEvictsOldest proves CODE-HYGIENE-10.3-1 item
// 2: the cache does not grow without bound. Once maxSize is exceeded, the
// oldest entry is evicted (and its bytes zeroed) rather than the cache
// growing forever.
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
}

// TestDerivedTokenCache_EvictionZeroesTokenBytes proves that eviction (via
// bound or rotation) overwrites the retained token bytes rather than just
// unlinking them, so no derived secret material is retained in the byte
// slice past eviction.
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
