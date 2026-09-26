package secretstore

import (
	"context"
	"fmt"
	"sort"

	"github.com/Diansalas/igaming-platform/internal/config"
)

// Store is one secret-store backend. Get returns the exact bytes of ref's
// pinned version. Every failure is a *Error with a closed class: a backend
// never returns store text, SDK output or a path. Get must honour ctx.
type Store interface {
	// Scheme is the ref prefix this backend serves (awssm, devfile or
	// memory).
	Scheme() string
	// Get fetches ref's pinned version. ref.Scheme() == Scheme().
	Get(ctx context.Context, ref Ref) (Secret, error)
}

// Router maps a scheme to its one backend. It is built once at startup
// and never mutated.
type Router struct {
	stores    map[string]Store
	validated bool
}

// NewRouter builds the router from stores, calling
// cfg.ValidateSecretBackendScheme for every one (security review §4.1
// point 1): a configured backend the environment does not permit refuses
// startup. memory:// is refused by that rule in every environment, so a
// router built here never serves a memory:// ref.
func NewRouter(cfg config.Config, stores ...Store) (*Router, error) {
	for _, s := range stores {
		if s == nil {
			return nil, fmt.Errorf("secretstore: nil backend")
		}
		if err := cfg.ValidateSecretBackendScheme(s.Scheme()); err != nil {
			return nil, fmt.Errorf("secretstore: backend %q refused: %w", s.Scheme(), err)
		}
	}
	r, err := newRouter(stores)
	if err != nil {
		return nil, err
	}
	r.validated = true
	return r, nil
}

// NewRouterUnvalidated builds a router WITHOUT the environment allow-list.
// TEST SUPPORT ONLY: it exists so tests can route memory:// refs. Its only
// permitted callers are _test.go files and internal/secretstore/memstore
// (itself importable only from tests); TestSecretStore_UnvalidatedRouter
// OnlyFromTestSupport enforces that with an AST check.
func NewRouterUnvalidated(stores ...Store) (*Router, error) {
	for _, s := range stores {
		if s == nil {
			return nil, fmt.Errorf("secretstore: nil backend")
		}
	}
	return newRouter(stores)
}

func newRouter(stores []Store) (*Router, error) {
	r := &Router{stores: make(map[string]Store, len(stores))}
	for _, s := range stores {
		switch s.Scheme() {
		case SchemeAWSSecretsManager, SchemeDevFile, SchemeMemory:
		default:
			return nil, fmt.Errorf("secretstore: unknown backend scheme %q", s.Scheme())
		}
		if _, dup := r.stores[s.Scheme()]; dup {
			return nil, fmt.Errorf("secretstore: duplicate backend for scheme %q", s.Scheme())
		}
		r.stores[s.Scheme()] = s
	}
	return r, nil
}

// Backend returns the backend for scheme. A nil router has none.
func (r *Router) Backend(scheme string) (Store, bool) {
	if r == nil {
		return nil, false
	}
	s, ok := r.stores[scheme]
	return s, ok
}

// Schemes lists the routed schemes, sorted.
func (r *Router) Schemes() []string {
	if r == nil {
		return nil
	}
	out := make([]string, 0, len(r.stores))
	for k := range r.stores {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// GetDirect fetches ref from its backend WITHOUT the Fetcher's cache,
// breaker or negative cache - for registration and apply only, where the
// secret must be read fresh (and never cached before it is approved). It
// is bounded by StoreCallTimeout and panic-safe; every failure is a *Error.
func (r *Router) GetDirect(ctx context.Context, ref Ref) (Secret, error) {
	store, ok := r.Backend(ref.Scheme())
	if !ok {
		return Secret{}, classError(ClassNoBackend)
	}
	callCtx, cancel := context.WithTimeout(ctx, StoreCallTimeout)
	defer cancel()
	return callStore(callCtx, store, ref)
}

// Validated reports whether the router was built through NewRouter (the
// environment allow-list) rather than the test-only NewRouterUnvalidated.
func (r *Router) Validated() bool { return r != nil && r.validated }
