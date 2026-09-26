// Package memstore is the in-memory memory:// secret-store backend.
//
// TEST SUPPORT ONLY (ADR 0093 §6; security review §4.1). It may be
// imported only from _test.go files: TestSecretStore_MemstoreImportedOnlyByTests
// (internal/secretstore) fails the build's test run if any non-test file
// imports it. config.ValidateSecretBackendScheme refuses "memory" in every
// environment, so no configuration can select it either, and a
// secretstore.Router built by NewRouter never routes a memory:// ref.
//
// Test secrets are generated at runtime (crypto/rand) by the tests that
// use this package; nothing here holds a literal secret.
package memstore

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

// Store is the memory:// backend. It is a synthetic component
// (providerkind.Synthetic): the ADR 0085 production guard refuses it.
type Store struct {
	mu      sync.Mutex
	secrets map[string][]byte
	errs    map[string]secretstore.ErrorClass
	global  secretstore.ErrorClass
	block   chan struct{}
	calls   atomic.Int64
	active  atomic.Int64
	maxSeen atomic.Int64
	onCall  func(ref string)
	// ADR 0094 test support: per-substring blocking (one tenant's outage),
	// a context-observing hook, injected latency, and per-substring
	// concurrency high-water marks.
	blockMatch map[string]chan struct{}
	onCallCtx  func(ctx context.Context, ref string)
	latency    time.Duration
	matchers   map[string]*matchCounter
}

type matchCounter struct {
	active  atomic.Int64
	maxSeen atomic.Int64
}

// New returns an empty memory store.
func New() *Store {
	return &Store{secrets: map[string][]byte{}, errs: map[string]secretstore.ErrorClass{}}
}

// SyntheticComponent marks the store as a synthetic (test-only) component.
func (s *Store) SyntheticComponent() {}

// Scheme implements secretstore.Store.
func (s *Store) Scheme() string { return secretstore.SchemeMemory }

// Put stores a copy of secret under ref (the full memory:// ref string).
func (s *Store) Put(ref string, secret []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := make([]byte, len(secret))
	copy(c, secret)
	s.secrets[ref] = c
}

// FailRef makes Get for ref fail with class (0 clears it).
func (s *Store) FailRef(ref string, class secretstore.ErrorClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if class == 0 {
		delete(s.errs, ref)
		return
	}
	s.errs[ref] = class
}

// FailAll makes every Get fail with class (0 clears it).
func (s *Store) FailAll(class secretstore.ErrorClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.global = class
}

// Block makes every Get wait until Unblock (or its context ends).
func (s *Store) Block() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.block == nil {
		s.block = make(chan struct{})
	}
}

// Unblock releases every blocked Get.
func (s *Store) Unblock() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.block != nil {
		close(s.block)
		s.block = nil
	}
}

// BlockMatching makes every Get whose ref contains substr wait until
// UnblockMatching (or its context ends) - one tenant's store outage when
// substr is that tenant's Namespace.
func (s *Store) BlockMatching(substr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.blockMatch == nil {
		s.blockMatch = map[string]chan struct{}{}
	}
	if _, ok := s.blockMatch[substr]; !ok {
		s.blockMatch[substr] = make(chan struct{})
	}
}

// UnblockMatching releases every Get blocked by BlockMatching(substr).
func (s *Store) UnblockMatching(substr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ch, ok := s.blockMatch[substr]; ok {
		close(ch)
		delete(s.blockMatch, substr)
	}
}

// Namespace is the ref substring that every one of tenantID's secrets in
// the provider-credential namespace contains (every domain and provider),
// for BlockMatching / TrackMatching.
func Namespace(tenantID string) string {
	return "/provider-creds/" + tenantID + "/"
}

// SetLatency makes every Get take at least d (0 clears it).
func (s *Store) SetLatency(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.latency = d
}

// TrackMatching starts counting concurrent Gets for refs containing substr;
// MaxConcurrentMatching reports the high-water mark.
func (s *Store) TrackMatching(substr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.matchers == nil {
		s.matchers = map[string]*matchCounter{}
	}
	if _, ok := s.matchers[substr]; !ok {
		s.matchers[substr] = &matchCounter{}
	}
}

// MaxConcurrentMatching is the highest number of concurrent Gets observed for
// refs containing a substring registered with TrackMatching (0 if not
// tracked).
func (s *Store) MaxConcurrentMatching(substr string) int64 {
	s.mu.Lock()
	c := s.matchers[substr]
	s.mu.Unlock()
	if c == nil {
		return 0
	}
	return c.maxSeen.Load()
}

// OnCallCtx registers a hook invoked at the start of every Get with its
// context (e.g. to assert txscope.Held(ctx) is false).
func (s *Store) OnCallCtx(fn func(ctx context.Context, ref string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCallCtx = fn
}

// OnCall registers a hook invoked at the start of every Get.
func (s *Store) OnCall(fn func(ref string)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onCall = fn
}

// Calls is the number of Get calls so far.
func (s *Store) Calls() int64 { return s.calls.Load() }

// MaxConcurrent is the highest number of concurrent Get calls observed.
func (s *Store) MaxConcurrent() int64 { return s.maxSeen.Load() }

// Get implements secretstore.Store.
func (s *Store) Get(ctx context.Context, ref secretstore.Ref) (secretstore.Secret, error) {
	s.calls.Add(1)
	n := s.active.Add(1)
	defer s.active.Add(-1)
	for {
		m := s.maxSeen.Load()
		if n <= m || s.maxSeen.CompareAndSwap(m, n) {
			break
		}
	}
	s.mu.Lock()
	block, onCall, onCallCtx, latency := s.block, s.onCall, s.onCallCtx, s.latency
	global, refErr := s.global, s.errs[ref.String()]
	secret, ok := s.secrets[ref.String()]
	var matchBlocks []chan struct{}
	for p, ch := range s.blockMatch {
		if strings.Contains(ref.String(), p) {
			matchBlocks = append(matchBlocks, ch)
		}
	}
	var counters []*matchCounter
	for p, c := range s.matchers {
		if strings.Contains(ref.String(), p) {
			counters = append(counters, c)
		}
	}
	s.mu.Unlock()
	for _, c := range counters {
		n := c.active.Add(1)
		defer c.active.Add(-1)
		for {
			m := c.maxSeen.Load()
			if n <= m || c.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
	}
	if onCall != nil {
		onCall(ref.String())
	}
	if onCallCtx != nil {
		onCallCtx(ctx, ref.String())
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return secretstore.Secret{}, secretstore.NewError(secretstore.ClassUnavailable)
		}
	}
	for _, ch := range matchBlocks {
		select {
		case <-ch:
		case <-ctx.Done():
			return secretstore.Secret{}, secretstore.NewError(secretstore.ClassUnavailable)
		}
	}
	if latency > 0 {
		t := time.NewTimer(latency)
		select {
		case <-t.C:
		case <-ctx.Done():
			t.Stop()
			return secretstore.Secret{}, secretstore.NewError(secretstore.ClassUnavailable)
		}
	}
	if global != 0 {
		return secretstore.Secret{}, secretstore.NewError(global)
	}
	if refErr != 0 {
		return secretstore.Secret{}, secretstore.NewError(refErr)
	}
	if !ok {
		return secretstore.Secret{}, secretstore.NewError(secretstore.ClassNotFound)
	}
	return secretstore.NewSecret(secret), nil
}

// NewRouter returns a router serving only the given memory stores (and
// any other test backends), bypassing the environment allow-list, which
// refuses memory:// everywhere. Test support only.
func NewRouter(stores ...secretstore.Store) (*secretstore.Router, error) {
	return secretstore.NewRouterUnvalidated(stores...)
}
