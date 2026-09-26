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
	"sync"
	"sync/atomic"

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
	block, onCall := s.block, s.onCall
	global, refErr := s.global, s.errs[ref.String()]
	secret, ok := s.secrets[ref.String()]
	s.mu.Unlock()
	if onCall != nil {
		onCall(ref.String())
	}
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
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
