package webhookauth

// The credential-resolver contract (ADR 0093 §4, Stage 10.3 W2a; ADR 0022
// §3 point 2 and point 9 as amended by Stage 10.3).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TenantReader is what a Resolver needs from the pool: a tenant-scoped
// READ ONLY transaction that commits before WithTenantReadOnly returns
// (ADR 0094 §4.1). *db.Pool satisfies it.
type TenantReader interface {
	WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error
}

// Resolver resolves the single (tenant, provider) credential binding for
// one inbound callback (ADR 0093 §4 as amended by ADR 0094 §4.1):
//
//	Resolve(ctx, r, tenantID, providerID, keyID, sel) (CredentialSet, error)
//	Recheck(ctx, tx, tenantID, cred) error
//
//	  - The domain is fixed when the resolver is constructed.
//	  - sel always comes from the scheme's Properties().KeySelection, never
//	    from the request (ADR 0022 §3, Stage 10.3 amendment). For
//	    KeyFromHeader keyID is the header's key id and the set holds exactly
//	    one credential (Active; Previous nil). For KeyImplicit keyID is ""
//	    and the set holds the active credential plus at most one verify_only
//	    predecessor inside its not_after window.
//	  - Resolve takes NO transaction. The real resolver
//	    (internal/providercred) runs exactly one plain, lock-free, read-only
//	    SELECT on provider_credential_handles in its OWN r.WithTenantReadOnly
//	    transaction - the one pre-verification statement ADR 0022 §3 point
//	    9 allows - which COMMITS before any secret-store fetch, so no pooled
//	    connection is ever held while waiting on the store (INV-POOL). It
//	    must be called with no transaction held (txscope); MOCK resolvers
//	    ignore r (it may be nil for them).
//	  - Recheck runs in the DOMAIN transaction, after verification, and
//	    fails closed unless the handle of the credential that VERIFIED
//	    (Credential.HandleID) is still usable (ADR 0094 §5). A MOCK Recheck
//	    accepts only its own handle-less credential.
//
// A route with no resolver fails closed (ReasonNoResolver) - there is no
// fallback to unauthenticated verification. Errors fold into closed
// reasons: ErrCredentialStoreUnavailable -> credential_store_unavailable,
// ErrCredentialIntegrity -> credential_integrity, ErrNoResolver ->
// no_resolver, anything else -> credential_unavailable.
type Resolver interface {
	Resolve(ctx context.Context, r TenantReader, tenantID uuid.UUID, providerID, keyID string, sel KeySelection) (CredentialSet, error)
	Recheck(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, c Credential) error
}

// KeyResolver is the single-key, transaction-free lookup the platform MOCK
// resolvers (and some test doubles) implement. ResolveSingleKey adapts it
// to Resolver.
type KeyResolver interface {
	ResolveKey(ctx context.Context, tenantID uuid.UUID, providerID, keyID string) (Credential, error)
}

var (
	// ErrCredentialStoreUnavailable: the handle row exists and is usable,
	// but the secret store could not serve its pinned version (and no
	// cached value within max-stale exists). Log reason
	// credential_store_unavailable.
	ErrCredentialStoreUnavailable = errors.New("webhookauth: credential store unavailable")
	// ErrCredentialIntegrity: the stored secret does not match the handle
	// row's fingerprint, or the row's ref is outside its tenant namespace.
	// P1. Log reason credential_integrity.
	ErrCredentialIntegrity = errors.New("webhookauth: credential integrity failure")
	// ErrNoResolver: the resolver that would serve this adapter kind is not
	// constructed in this process. Log reason no_resolver.
	ErrNoResolver = errors.New("webhookauth: no credential resolver for this adapter")
)

// ReasonCredentialStoreUnavailable and ReasonCredentialIntegrity are the
// two closed reasons added by ADR 0093 §4 (see AllReasons).
const (
	ReasonCredentialStoreUnavailable Reason = "credential_store_unavailable"
	ReasonCredentialIntegrity        Reason = "credential_integrity"
)

// reasonForResolveError folds a resolver error into its closed reason.
func reasonForResolveError(err error) Reason {
	switch {
	case errors.Is(err, ErrNoResolver):
		return ReasonNoResolver
	case errors.Is(err, ErrCredentialIntegrity):
		return ReasonCredentialIntegrity
	case errors.Is(err, ErrCredentialStoreUnavailable):
		return ReasonCredentialStoreUnavailable
	default:
		return ReasonCredentialUnavailable
	}
}

// ResolveSingleKey adapts a KeyResolver to the Resolver contract: for
// KeyFromHeader it returns exactly the one credential; KeyImplicit (and any
// other selection) fails closed, because a single-key MOCK resolver has no
// notion of a verify_only predecessor.
func ResolveSingleKey(ctx context.Context, r KeyResolver, tenantID uuid.UUID, providerID, keyID string, sel KeySelection) (CredentialSet, error) {
	if r == nil {
		return CredentialSet{}, ErrNoResolver
	}
	if sel != KeyFromHeader {
		return CredentialSet{}, ErrCredentialUnavailable
	}
	cred, err := r.ResolveKey(ctx, tenantID, providerID, keyID)
	if err != nil {
		return CredentialSet{}, err
	}
	return CredentialSet{Active: cred}, nil
}

// syntheticAdapter is providerkind.Synthetic restated structurally.
type syntheticAdapter interface{ SyntheticComponent() }

// KindSplitResolver is the ADR 0093 §4 wiring rule: the real resolver
// serves non-synthetic adapters and the MOCK resolver serves synthetic
// adapters. The split is a two-way choice by ADAPTER KIND (ADR 0085 §1,
// Stage 10.3 amendment), computed once from the domain's adapter registry;
// it is never a map keyed by vendor. A provider id that is not registered
// fails closed.
type KindSplitResolver struct {
	synthetic map[string]bool
	mock      Resolver
	real      Resolver
}

// NewKindSplitResolver builds the split over adapters. mock serves the
// synthetic adapters (nil means none is wired: those callbacks get
// no_resolver); real serves every other adapter (nil likewise). It returns
// a TRUE nil interface when both are nil, so an orchestrator's own
// nil-resolver branch still applies.
func NewKindSplitResolver[A any](adapters map[string]A, mock, real Resolver) Resolver {
	if mock == nil && real == nil {
		return nil
	}
	s := &KindSplitResolver{synthetic: map[string]bool{}, mock: mock, real: real}
	for id, a := range adapters {
		_, isSynthetic := any(a).(syntheticAdapter)
		s.synthetic[id] = isSynthetic
	}
	return s
}

// Resolve implements Resolver.
func (s *KindSplitResolver) Resolve(ctx context.Context, r TenantReader, tenantID uuid.UUID, providerID, keyID string, sel KeySelection) (CredentialSet, error) {
	target, err := s.target(providerID)
	if err != nil {
		return CredentialSet{}, err
	}
	return target.Resolve(ctx, r, tenantID, providerID, keyID, sel)
}

// Recheck implements Resolver: the same two-way split by adapter kind as
// Resolve, so the MOCK Recheck is reachable only for a synthetic adapter
// and an unregistered provider id fails closed (security condition C4).
func (s *KindSplitResolver) Recheck(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, c Credential) error {
	target, err := s.target(c.ProviderID)
	if err != nil {
		return err
	}
	return target.Recheck(ctx, tx, tenantID, c)
}

func (s *KindSplitResolver) target(providerID string) (Resolver, error) {
	isSynthetic, registered := s.synthetic[providerID]
	if !registered {
		return nil, ErrCredentialUnavailable
	}
	target := s.real
	if isSynthetic {
		target = s.mock
	}
	if target == nil {
		return nil, ErrNoResolver
	}
	return target, nil
}

// MarshalJSON implements json.Marshaler for Credential (security C15): the
// same allow-listed fields as LogValue, never Secret.
func (c Credential) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"tenant_id":   c.TenantID.String(),
		"provider_id": c.ProviderID,
		"key_id":      c.KeyID,
		"fingerprint": c.Fingerprint,
	})
}

// Format implements fmt.Formatter so every verb (including %x/%s on a
// Credential value) renders the redacted form.
func (c Credential) Format(f fmt.State, verb rune) {
	if verb == 'v' && f.Flag('#') {
		_, _ = f.Write([]byte(c.GoString()))
		return
	}
	_, _ = f.Write([]byte(c.String()))
}

// String implements fmt.Stringer for CredentialSet: never a secret.
func (s CredentialSet) String() string {
	if s.Previous == nil {
		return fmt.Sprintf("CredentialSet{Active:%s}", s.Active.String())
	}
	return fmt.Sprintf("CredentialSet{Active:%s Previous:%s}", s.Active.String(), s.Previous.String())
}

// GoString implements fmt.GoStringer for CredentialSet.
func (s CredentialSet) GoString() string { return s.String() }

// Format implements fmt.Formatter for CredentialSet.
func (s CredentialSet) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(s.String())) }

// LogValue implements slog.LogValuer for CredentialSet.
func (s CredentialSet) LogValue() slog.Value {
	attrs := []slog.Attr{slog.Any("active", s.Active.LogValue())}
	if s.Previous != nil {
		attrs = append(attrs, slog.Any("previous", s.Previous.LogValue()))
	}
	return slog.GroupValue(attrs...)
}

// MarshalJSON implements json.Marshaler for CredentialSet.
func (s CredentialSet) MarshalJSON() ([]byte, error) {
	out := map[string]any{"active": s.Active}
	if s.Previous != nil {
		out["previous"] = *s.Previous
	}
	return json.Marshal(out)
}
