// Package providercred is the real provider-credential subsystem (Stage
// 10.3 W2a; ADR 0093 and its W2a design-review amendment; normative detail
// docs/plans/stage-10.3-planning/07-w2a-design-review-security.md):
//
//   - the inbound resolver (webhookauth.Resolver) over the FORCE-RLS
//     provider_credential_handles table (migration 0096), including
//     KeyImplicit (active + at most one verify_only predecessor);
//   - the outbound per-call credential resolution (PROV-OUTBOUND-CRED-1);
//   - the four-eyes registration service (file, decide, apply) and the
//     single-actor TransitionHandle (verify_only, shorten, revoke);
//   - the keyed fp1: fingerprint and the transient sha256 confirmation.
//
// The subsystem is constructed only when BOTH a fingerprint key and at
// least one permitted secret-store backend are configured
// (config.Config.ProviderCredentialFingerprintKey, SecretStoreBackends).
// Otherwise New returns nil and every real path fails closed: the real
// resolver is nil (callbacks get 401 no_resolver), non-synthetic outbound
// calls fail, and the request/approve/apply routes are not mounted.
//
// Revocation is immediate by construction: every inbound resolve and every
// outbound call reads the handle row first (one pinned, lock-free,
// read-only, tenant-predicated SELECT), in every cache and breaker state.
// The only material that outlives a request is the secretstore.Fetcher's
// cache, keyed on (tenant, secret_ref, fingerprint), which a revoked handle
// can never reach.
package providercred

import (
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

// Domains a handle may belong to (migration 0096 CHECK).
var Domains = []string{"payments", "kyc", "casino"}

// Purposes (migration 0096 CHECK; R15). Inbound and outbound never share a
// handle.
const (
	PurposeWebhookVerify = "webhook_verify"
	PurposeOutboundAPI   = "outbound_api"
)

// Subsystem bundles the fingerprint key, the secret-store router and the
// process-wide Fetcher. One per process.
type Subsystem struct {
	cfg     config.Config
	key     FingerprintKey
	router  *secretstore.Router
	fetcher *secretstore.Fetcher
	logger  *slog.Logger
}

// Option configures the Subsystem.
type Option func(*options)

type options struct {
	logger      *slog.Logger
	fetcherOpts []secretstore.FetcherOption
}

// WithLogger sets the subsystem logger (P1 alerts, classified failures).
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// WithFetcherOptions passes options to the secretstore.Fetcher (tests
// inject a clock).
func WithFetcherOptions(opts ...secretstore.FetcherOption) Option {
	return func(o *options) { o.fetcherOpts = append(o.fetcherOpts, opts...) }
}

// New builds the subsystem. It returns (nil, nil) - "not constructed", the
// fail-closed state - when the fingerprint key is absent or router routes
// no backend. A present but invalid key is an error (config.Load already
// refuses one, so this is defence in depth).
func New(cfg config.Config, router *secretstore.Router, opts ...Option) (*Subsystem, error) {
	if !cfg.ProviderCredentialFingerprintKey.IsSet() || len(router.Schemes()) == 0 {
		return nil, nil
	}
	key, err := NewFingerprintKey([]byte(cfg.ProviderCredentialFingerprintKey.Reveal()))
	if err != nil {
		return nil, err
	}
	o := options{logger: slog.Default()}
	for _, opt := range opts {
		opt(&o)
	}
	fetcherOpts := append([]secretstore.FetcherOption{secretstore.WithLogger(o.logger)}, o.fetcherOpts...)
	s := &Subsystem{
		cfg:    cfg,
		key:    key,
		router: router,
		logger: o.logger,
	}
	s.fetcher = secretstore.NewFetcher(router, key.Fingerprint, fetcherOpts...)
	return s, nil
}

// MarkProductionEligible implements providerkind.ProductionEligible: the
// real subsystem is the production path. Its backends are registered with
// the ADR 0085 guard separately (devfile carries no eligibility marker and
// memory is synthetic).
func (s *Subsystem) MarkProductionEligible() {}

// Fingerprint returns the keyed fp1: fingerprint of secret.
func (s *Subsystem) Fingerprint(secret []byte) string { return s.key.Fingerprint(secret) }

// Router returns the subsystem's secret-store router.
func (s *Subsystem) Router() *secretstore.Router {
	if s == nil {
		return nil
	}
	return s.router
}

// Fetcher returns the subsystem's Fetcher (tests inspect breaker state).
func (s *Subsystem) Fetcher() *secretstore.Fetcher {
	if s == nil {
		return nil
	}
	return s.fetcher
}

// validDomain reports whether d is a handle domain.
func validDomain(d string) bool {
	for _, x := range Domains {
		if x == d {
			return true
		}
	}
	return false
}

// errNilSubsystem is returned by methods called on a nil Subsystem.
var errNilSubsystem = errors.New("providercred: subsystem not constructed")

// logTxHeld is the ADR 0094 INV-POOL guard's single log line: the entry
// point and the tenant id only.
func (s *Subsystem) logTxHeld(entryPoint string, tenantID uuid.UUID) {
	logger := slog.Default()
	if s != nil && s.logger != nil {
		logger = s.logger
	}
	logger.Error("secret_fetch_with_tx_held", "entry_point", entryPoint, "tenant_id", tenantID.String())
}
