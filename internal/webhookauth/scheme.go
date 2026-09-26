package webhookauth

// Per-adapter verification schemes (Stage 10.3 W1a, WH-VENDOR-SCHEME-1;
// docs/plans/stage-10.3-planning/01-provider-trust-analysis.md §1.1, as
// conditioned by the security review 04-review-security.md C3/C4/C9/C10/
// C11 and proposal §19 rulings R3/R4/R6).
//
// A VerificationScheme is how ONE adapter's vendor authenticates its
// inbound callbacks. The platform keeps ownership of ordering, tenant
// resolution, credential selection and the uniform failure response (ADR
// 0022 §3 points 1-9); the scheme only extracts and verifies. Everything a
// domain orchestrator needs is here, in this order:
//
//	NewSchemeSet          - registration-time validation (a bad declaration
//	                        fails startup);
//	CheckInboundPreamble  - the shared HTTP preamble (provider charset, body
//	                        bound, scheme lookup, Extract), no tenant/DB;
//	ExtractInbound        - Extract, panic-safe, closed reasons;
//	ResolveCredentials    - key selection from Properties().KeySelection
//	                        ONLY (never from an absent key id header);
//	VerifyInbound         - the orchestrator-enforced Verify, closed
//	                        reasons, panic-safe.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"time"

	"github.com/google/uuid"
)

// MaxSkewCap is the platform cap on a scheme's timestamp tolerance (ADR
// 0022 §3 point 10 as conditioned by security C11 / ruling R4: "MaxSkew <=
// 10 minutes"). A larger vendor tolerance needs a recorded security
// sign-off AND a change to this constant in a reviewed commit.
const MaxSkewCap = 10 * time.Minute

// MinSecretBytes is the shortest credential secret any scheme may accept
// (conformance SC10: an empty or short secret is rejected). 16 bytes =
// 128 bits. VerifyInbound enforces it before a scheme is ever called, and
// the conformance suite proves each scheme enforces it on its own too.
const MinSecretBytes = 16

// ErrTimestampOutOfWindow is the ONLY error besides ErrSignatureInvalid a
// VerificationScheme's Verify may return (security C10). It means the MAC
// over the exact bytes is otherwise VALID but the signed timestamp is
// outside Properties().MaxSkew of the platform clock - a genuine replay
// signal, not noise from an unauthenticated caller. A scheme must check the
// MAC first and return ErrSignatureInvalid for any MAC failure, whatever
// the timestamp.
//
// errors.Is(ErrTimestampOutOfWindow, ErrSignatureInvalid) is true, so every
// existing "signature failed" check treats it as a verification failure;
// the distinct identity only selects the log Reason. The HTTP response is
// the same uniform 401.
var ErrTimestampOutOfWindow error = timestampWindowError{}

type timestampWindowError struct{}

func (timestampWindowError) Error() string {
	return "webhookauth: callback timestamp outside the accepted window"
}

func (timestampWindowError) Is(target error) bool { return target == ErrSignatureInvalid }

// ReasonTimestampOutOfWindow is the log reason for ErrTimestampOutOfWindow.
// Declared here (not in webhookauth.go's const block) only to keep the
// W1a diff reviewable; it is part of the single closed Reason enum (see
// AllReasons).
const ReasonTimestampOutOfWindow Reason = "timestamp_out_of_window"

// AllReasons returns every member of the closed Reason enum, in a fixed
// order. The allow-listed auth-failure log line only ever carries one of
// these.
func AllReasons() []Reason {
	return []Reason{
		ReasonTenantUnknown, ReasonTenantInactive, ReasonProviderInvalid, ReasonProviderUnregistered,
		ReasonProviderNotConfigured, ReasonNoResolver, ReasonCredentialUnavailable, ReasonSignatureMissing,
		ReasonSignatureInvalid, ReasonKeyMaterial, ReasonBodyTooLarge, ReasonTimestampOutOfWindow,
	}
}

// TenantBinding declares HOW a scheme binds a callback to one tenant (ADR
// 0022 §3 point 3).
type TenantBinding int

const (
	_ TenantBinding = iota // the zero value is deliberately invalid (ValidateProperties refuses it)
	// BindingSignedTenant: the platform's tenant id is inside the signed
	// input (only possible for platform-defined schemes, i.e. the MOCK).
	BindingSignedTenant
	// BindingPerMerchantKey: every tenant holds its own vendor key, so a
	// key is tenant-unique.
	BindingPerMerchantKey
	// BindingPerMerchantKeySignedAccount: per-tenant key AND the vendor
	// signs its merchant account id, which must equal
	// Credential.BoundAccountID.
	BindingPerMerchantKeySignedAccount
)

func (b TenantBinding) String() string {
	switch b {
	case BindingSignedTenant:
		return "signed_tenant"
	case BindingPerMerchantKey:
		return "per_merchant_key"
	case BindingPerMerchantKeySignedAccount:
		return "per_merchant_key_signed_account"
	default:
		return fmt.Sprintf("invalid_binding(%d)", int(b))
	}
}

// KeySelection declares how the ONE credential binding is selected (ADR
// 0022 §3 point 2 as clarified by security C3/C4). It is taken from
// Properties() only - never inferred from whether a key-id header happens
// to be present on the request.
type KeySelection int

const (
	_ KeySelection = iota // the zero value is deliberately invalid (ValidateProperties refuses it)
	// KeyFromHeader: the vendor names its key id on every request. An
	// absent key id is ReasonSignatureMissing, never a multi-key trial,
	// and a Previous credential is never passed to Verify.
	KeyFromHeader
	// KeyImplicit: the vendor sends no key id; the active key and at most
	// one verify_only predecessor of the SAME (tenant, provider) may be
	// tried inside a bounded not_after window. Needs a resolver that can
	// return that pair - NOT IMPLEMENTED until W2a (ResolveCredentials
	// fails closed with ReasonCredentialUnavailable meanwhile).
	KeyImplicit
)

func (k KeySelection) String() string {
	switch k {
	case KeyFromHeader:
		return "key_from_header"
	case KeyImplicit:
		return "key_implicit"
	default:
		return fmt.Sprintf("invalid_key_selection(%d)", int(k))
	}
}

// ReplayDefence declares how a scheme's replays are contained.
type ReplayDefence int

const (
	_ ReplayDefence = iota // the zero value is deliberately invalid (ValidateProperties refuses it)
	// ReplayTimestampWindow: a signed timestamp must be within MaxSkew of
	// the platform clock; replays inside the window are absorbed by each
	// domain's database-enforced idempotency (provider_tx_id uniqueness).
	// Mandatory for every non-synthetic scheme (ADR 0022 §3 point 10).
	ReplayTimestampWindow
	// ReplayIdempotencyOnly: no signed timestamp; replay is inert only
	// because every effect is idempotent. Permitted for the platform's
	// synthetic MOCK scheme alone (its bytes are frozen; see mock_scheme.go).
	ReplayIdempotencyOnly
)

func (r ReplayDefence) String() string {
	switch r {
	case ReplayTimestampWindow:
		return "timestamp_window"
	case ReplayIdempotencyOnly:
		return "idempotency_only"
	default:
		return fmt.Sprintf("invalid_replay_defence(%d)", int(r))
	}
}

// SchemeProperties is what a scheme declares about itself. The
// conformance suite (webhookauthtest) proves the declaration is TRUTHFUL;
// ValidateProperties (called at registration) proves it is PERMITTED.
type SchemeProperties struct {
	Binding      TenantBinding
	KeySelection KeySelection
	// SignedTimestamp: the vendor signs a timestamp inside the MAC input.
	SignedTimestamp bool
	// MaxSkew is the accepted |now - signed timestamp|; required in
	// (0, MaxSkewCap] when SignedTimestamp.
	MaxSkew time.Duration
	Replay  ReplayDefence
	// Synthetic marks the platform's own MOCK scheme. Only schemes built
	// by this package (mock_scheme.go) may declare it; NewSchemeSet
	// refuses a Synthetic declaration from any other implementation, so a
	// real adapter cannot opt out of the timestamp rule or the
	// conformance gate by claiming to be a mock.
	Synthetic bool
}

// ValidateProperties reports whether p is a PERMITTED declaration (security
// C11). It does not know which concrete type declared p; NewSchemeSet adds
// the "Synthetic only for the platform MOCK" rule.
func ValidateProperties(p SchemeProperties) error {
	switch p.Binding {
	case BindingSignedTenant, BindingPerMerchantKey, BindingPerMerchantKeySignedAccount:
	default:
		return fmt.Errorf("webhookauth: scheme declares unknown tenant binding %s", p.Binding)
	}
	switch p.KeySelection {
	case KeyFromHeader, KeyImplicit:
	default:
		return fmt.Errorf("webhookauth: scheme declares unknown key selection %s", p.KeySelection)
	}
	switch p.Replay {
	case ReplayTimestampWindow, ReplayIdempotencyOnly:
	default:
		return fmt.Errorf("webhookauth: scheme declares unknown replay defence %s", p.Replay)
	}
	if p.SignedTimestamp {
		if p.MaxSkew <= 0 {
			return fmt.Errorf("webhookauth: scheme signs a timestamp but declares MaxSkew %s <= 0", p.MaxSkew)
		}
		if p.MaxSkew > MaxSkewCap {
			return fmt.Errorf("webhookauth: scheme MaxSkew %s exceeds the platform cap %s (security sign-off required)", p.MaxSkew, MaxSkewCap)
		}
		if p.Replay != ReplayTimestampWindow {
			return fmt.Errorf("webhookauth: scheme signs a timestamp but declares replay defence %s", p.Replay)
		}
	} else {
		if p.MaxSkew != 0 {
			return fmt.Errorf("webhookauth: scheme declares MaxSkew %s without a signed timestamp", p.MaxSkew)
		}
		if p.Replay == ReplayTimestampWindow {
			return errors.New("webhookauth: scheme declares a timestamp-window replay defence without a signed timestamp")
		}
	}
	if !p.Synthetic {
		// ADR 0022 §3 point 10: every real scheme signs a timestamp.
		if !p.SignedTimestamp {
			return errors.New("webhookauth: a non-synthetic scheme must sign a timestamp (ADR 0022 §3 point 10)")
		}
		// Only the platform can put ITS tenant id in the signed input.
		if p.Binding == BindingSignedTenant {
			return errors.New("webhookauth: a non-synthetic scheme cannot declare signed-tenant binding (a vendor never signs the platform's tenant id)")
		}
	} else if p.Replay == ReplayIdempotencyOnly && p.SignedTimestamp {
		return errors.New("webhookauth: inconsistent synthetic replay declaration")
	}
	return nil
}

// AuthMaterial is what a scheme's Extract parsed and format-checked from
// the request. It is NOT verified. KeyID selects the credential for
// KeyFromHeader schemes and is "" for KeyImplicit ones; everything else
// (signature bytes, signed timestamp, account id) is scheme-private.
type AuthMaterial struct {
	KeyID   string
	private any
}

// NewAuthMaterial builds an AuthMaterial. private is opaque to the platform
// and handed back to the same scheme's Verify.
func NewAuthMaterial(keyID string, private any) AuthMaterial {
	return AuthMaterial{KeyID: keyID, private: private}
}

// Private returns the scheme-private part set by NewAuthMaterial.
func (m AuthMaterial) Private() any { return m.private }

// CredentialSet is the single (tenant, provider) credential binding handed
// to Verify: Active, plus - for KeyImplicit schemes only, during a bounded
// rotation window - at most one Previous (verify_only) credential of the
// same tenant and provider (security C4). Previous is always nil for a
// KeyFromHeader scheme.
type CredentialSet struct {
	Active   Credential
	Previous *Credential
}

// VerificationScheme is one adapter's inbound-callback authentication.
type VerificationScheme interface {
	// Name is a stable, loggable identifier, e.g.
	// "platform-mock:igaming.payments.webhook.v1".
	Name() string
	// Extract parses and format-validates the auth headers only. It is
	// pure: no DB, no secret, no clock, no body parsing, and it never uses
	// in.TenantID (unknown when the HTTP preamble calls it). On failure it
	// returns ReasonSignatureMissing (a required header is absent) or
	// ReasonSignatureInvalid (present but malformed), never any other
	// reason. A KeyFromHeader scheme MUST treat an absent key id as
	// ReasonSignatureMissing.
	Extract(in Inbound) (AuthMaterial, Reason, bool)
	// Verify checks in against creds over the RAW body, with a
	// constant-time MAC comparison (hmac.Equal), BEFORE any parsing. It
	// requires cred.TenantID/ProviderID == in.TenantID/ProviderID, a signed
	// account id (if any) == cred.BoundAccountID, a secret of at least
	// MinSecretBytes and - when Properties().SignedTimestamp - a signed
	// timestamp within MaxSkew of now (the platform clock; the only "now").
	// It returns the KeyID of the credential that verified, or exactly
	// ErrSignatureInvalid / ErrTimestampOutOfWindow (C10) with no wrapping
	// and no body, secret or fingerprint in the text (SC11).
	Verify(creds CredentialSet, in Inbound, m AuthMaterial, now time.Time) (matchedKeyID string, err error)
	// Properties declares what the conformance suite must prove.
	Properties() SchemeProperties
}

// schemeNamePattern bounds a scheme name (it is logged and used as a map
// key in the conformance manifest).
var schemeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.:_-]{0,95}$`)

// conformanceManifest lists every NON-synthetic scheme name that may be
// registered. Adding a name here is the only way to register a real
// vendor scheme, and the registry-driven test in
// internal/webhookauth/webhookauthtest fails for any name here that has no
// conformance fixture (with a vendor known-answer vector) - so a real
// scheme can never be registered without running the conformance suite
// (security C9, ruling R6). Empty today: no real vendor exists, and none
// is invented (proposal §22).
var conformanceManifest = map[string]struct{}{}

// ConformanceManifest returns the sorted names of every non-synthetic
// scheme that may be registered.
func ConformanceManifest() []string {
	names := make([]string, 0, len(conformanceManifest))
	for n := range conformanceManifest {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ValidateScheme is the registration-time check for one scheme: a
// well-formed Name, a PERMITTED Properties() declaration (C11), Synthetic
// only for this package's own MOCK scheme, and - for a non-synthetic
// scheme - presence in the conformance manifest (C9).
func ValidateScheme(s VerificationScheme) error {
	if s == nil {
		return errors.New("webhookauth: nil verification scheme")
	}
	name := s.Name()
	if !schemeNamePattern.MatchString(name) {
		return fmt.Errorf("webhookauth: invalid scheme name %q", name)
	}
	p := s.Properties()
	if err := ValidateProperties(p); err != nil {
		return fmt.Errorf("scheme %q: %w", name, err)
	}
	_, isPlatformMock := s.(mockVerificationScheme)
	if p.Synthetic && !isPlatformMock {
		return fmt.Errorf("webhookauth: scheme %q declares Synthetic but is not the platform MOCK scheme", name)
	}
	if !p.Synthetic {
		if _, listed := conformanceManifest[name]; !listed {
			return fmt.Errorf("webhookauth: non-synthetic scheme %q is not in the conformance manifest (it must pass webhookauthtest.RunSchemeConformance with a vendor known-answer vector first)", name)
		}
	}
	return nil
}

// SchemeSet is one domain's validated provider-id -> scheme registry,
// built once from that domain's process-global adapter registry. It holds
// no tenant input.
type SchemeSet struct {
	domain  string
	schemes map[string]VerificationScheme
}

// NewSchemeSet validates every (providerID, scheme) pair (ValidProviderID,
// ValidateScheme). Any error means the process must refuse to start.
func NewSchemeSet(domain string, schemes map[string]VerificationScheme) (*SchemeSet, error) {
	set := &SchemeSet{domain: domain, schemes: make(map[string]VerificationScheme, len(schemes))}
	ids := make([]string, 0, len(schemes))
	for id := range schemes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !ValidProviderID(id) {
			return nil, fmt.Errorf("webhookauth: %s provider id %q fails the webhook provider-id charset", domain, id)
		}
		if err := ValidateScheme(schemes[id]); err != nil {
			return nil, fmt.Errorf("webhookauth: %s provider %q: %w", domain, id, err)
		}
		set.schemes[id] = schemes[id]
	}
	return set, nil
}

// MustSchemeSet is NewSchemeSet for orchestrator constructors, which run at
// process start: a bad declaration panics, so startup fails loudly rather
// than serving with an unvalidated scheme.
func MustSchemeSet(domain string, schemes map[string]VerificationScheme) *SchemeSet {
	set, err := NewSchemeSet(domain, schemes)
	if err != nil {
		panic(err.Error())
	}
	return set
}

// Lookup returns the scheme registered for providerID. A nil set has none.
func (s *SchemeSet) Lookup(providerID string) (VerificationScheme, bool) {
	if s == nil {
		return nil, false
	}
	v, ok := s.schemes[providerID]
	return v, ok
}

// CheckInboundPreamble is the shared, tenant-independent webhook preamble
// (Stage 10.3 order, 01-provider-trust-analysis.md §1.1):
//
//  1. provider_id charset (ReasonProviderInvalid);
//  2. body read bounded to maxBody (ReasonBodyTooLarge);
//  3. scheme lookup by provider id (ReasonProviderUnregistered) - now
//     BEFORE the tenant lookup;
//  4. scheme.Extract (ReasonSignatureMissing / ReasonSignatureInvalid).
//
// It never parses the body and never touches the database.
func CheckInboundPreamble(providerID string, h http.Header, body io.Reader, maxBody int, lookup func(providerID string) (VerificationScheme, bool)) (PreambleResult, bool) {
	if !ValidProviderID(providerID) {
		return PreambleResult{Reason: ReasonProviderInvalid}, false
	}
	raw, err := io.ReadAll(io.LimitReader(body, int64(maxBody)+1))
	if err != nil {
		return PreambleResult{ProviderIDValid: true, Reason: ReasonBodyTooLarge}, false
	}
	if len(raw) > maxBody {
		return PreambleResult{ProviderIDValid: true, BodyLen: len(raw), Reason: ReasonBodyTooLarge}, false
	}
	var scheme VerificationScheme
	ok := false
	if lookup != nil {
		scheme, ok = lookup(providerID)
	}
	if !ok || scheme == nil {
		return PreambleResult{ProviderIDValid: true, BodyLen: len(raw), Reason: ReasonProviderUnregistered}, false
	}
	if _, authErr := ExtractInbound(scheme, Inbound{ProviderID: providerID, Header: h, Body: raw}); authErr != nil {
		return PreambleResult{ProviderIDValid: true, BodyLen: len(raw), Reason: authErr.Reason}, false
	}
	return PreambleResult{Body: raw, BodyLen: len(raw), ProviderIDValid: true}, true
}

// ExtractInbound runs scheme.Extract with the platform's guarantees: a
// panic is ReasonSignatureInvalid (fail closed), a reason outside {missing,
// invalid} is folded to ReasonSignatureInvalid, and a KeyFromHeader scheme
// that returns an empty or non-charset key id is rejected (security C3:
// never a key-id-less trial) - so KeyID on a returned AuthError is always
// safe to log.
func ExtractInbound(scheme VerificationScheme, in Inbound) (m AuthMaterial, authErr *AuthError) {
	defer func() {
		if recover() != nil {
			m, authErr = AuthMaterial{}, &AuthError{Reason: ReasonSignatureInvalid}
		}
	}()
	m, reason, ok := scheme.Extract(in)
	if !ok {
		if reason != ReasonSignatureMissing {
			reason = ReasonSignatureInvalid
		}
		return AuthMaterial{}, &AuthError{Reason: reason}
	}
	switch scheme.Properties().KeySelection {
	case KeyFromHeader:
		if m.KeyID == "" {
			return AuthMaterial{}, &AuthError{Reason: ReasonSignatureMissing}
		}
		if !keyIDPattern.MatchString(m.KeyID) {
			return AuthMaterial{}, &AuthError{Reason: ReasonSignatureInvalid}
		}
	case KeyImplicit:
		if m.KeyID != "" {
			// A KeyImplicit scheme never selects by key id; a non-empty one
			// is a scheme bug, never trusted.
			return AuthMaterial{}, &AuthError{Reason: ReasonSignatureInvalid}
		}
	default:
		return AuthMaterial{}, &AuthError{Reason: ReasonSignatureInvalid}
	}
	return m, nil
}

// ResolveCredentials selects the single credential binding for (in.TenantID,
// in.ProviderID). The mode comes from scheme.Properties().KeySelection ONLY
// (security C3), never from whether m.KeyID is empty:
//
//   - KeyFromHeader: exactly Resolve(tenant, provider, m.KeyID); an empty
//     key id is ReasonSignatureMissing (defence in depth over
//     ExtractInbound); Previous is always nil.
//   - KeyImplicit: needs a resolver able to return the active key and at
//     most one verify_only predecessor - NOT IMPLEMENTED until W2a, so this
//     fails closed with ReasonCredentialUnavailable.
//
// A nil resolver is ReasonNoResolver. A resolved credential bound to a
// different tenant or provider is ReasonCredentialUnavailable.
func ResolveCredentials(ctx context.Context, scheme VerificationScheme, resolver Resolver, in Inbound, m AuthMaterial) (CredentialSet, *AuthError) {
	switch scheme.Properties().KeySelection {
	case KeyFromHeader:
		if m.KeyID == "" {
			return CredentialSet{}, &AuthError{Reason: ReasonSignatureMissing}
		}
		if resolver == nil {
			return CredentialSet{}, &AuthError{Reason: ReasonNoResolver, KeyID: m.KeyID}
		}
		cred, err := resolver.Resolve(ctx, in.TenantID, in.ProviderID, m.KeyID)
		if err != nil {
			return CredentialSet{}, &AuthError{Reason: ReasonCredentialUnavailable, KeyID: m.KeyID}
		}
		if cred.TenantID != in.TenantID || cred.ProviderID != in.ProviderID || cred.KeyID != m.KeyID {
			// Defence in depth only: no conforming resolver returns a
			// credential bound to anything other than what it was asked for.
			return CredentialSet{}, &AuthError{Reason: ReasonCredentialUnavailable, KeyID: m.KeyID}
		}
		return CredentialSet{Active: cred}, nil
	case KeyImplicit:
		if resolver == nil {
			return CredentialSet{}, &AuthError{Reason: ReasonNoResolver}
		}
		return CredentialSet{}, &AuthError{Reason: ReasonCredentialUnavailable}
	default:
		return CredentialSet{}, &AuthError{Reason: ReasonCredentialUnavailable}
	}
}

// VerifyInbound is the orchestrator-enforced verification step (ADR 0022
// §3 points 4-7 as amended by Stage 10.3: the orchestrator, not the
// adapter, is the mandatory verifier). Every domain's ReceiveCallback
// calls it after ResolveCredentials and BEFORE the adapter's
// HandleCallback, so no adapter can skip verification.
//
// Platform guarantees layered on top of the scheme: a Previous credential
// is dropped for KeyFromHeader schemes and outside its not_after window; a
// credential bound to another tenant/provider or with a secret shorter than
// MinSecretBytes never reaches the scheme; a panic is a verification
// failure; any error other than ErrTimestampOutOfWindow is
// ReasonSignatureInvalid; the matched key id must belong to the set. It
// returns the credential that verified.
func VerifyInbound(scheme VerificationScheme, creds CredentialSet, in Inbound, m AuthMaterial, now time.Time) (verified Credential, authErr *AuthError) {
	fail := func(reason Reason) (Credential, *AuthError) {
		e := &AuthError{Reason: reason, KeyID: m.KeyID}
		if reason == ReasonSignatureInvalid {
			e.CredentialFingerprint = creds.Active.Fingerprint
		}
		return Credential{}, e
	}
	if !credentialUsable(creds.Active, in) {
		return fail(ReasonSignatureInvalid)
	}
	props := scheme.Properties()
	if props.KeySelection == KeyFromHeader && (m.KeyID == "" || m.KeyID != creds.Active.KeyID) {
		return fail(ReasonSignatureInvalid)
	}
	if props.KeySelection != KeyImplicit || creds.Previous == nil ||
		!credentialUsable(*creds.Previous, in) || creds.Previous.NotAfter.IsZero() || !now.Before(creds.Previous.NotAfter) {
		creds.Previous = nil
	}

	var matched string
	var err error
	func() {
		defer func() {
			if recover() != nil {
				matched, err = "", ErrSignatureInvalid
			}
		}()
		matched, err = scheme.Verify(creds, in, m, now)
	}()
	if err != nil {
		// Identity, not errors.Is: the closed contract (C10) is the exact
		// sentinel; a wrapped value is folded to signature_invalid.
		if err == ErrTimestampOutOfWindow {
			return fail(ReasonTimestampOutOfWindow)
		}
		return fail(ReasonSignatureInvalid)
	}
	switch {
	case matched != "" && matched == creds.Active.KeyID:
		return creds.Active, nil
	case creds.Previous != nil && matched != "" && matched == creds.Previous.KeyID:
		return *creds.Previous, nil
	default:
		return fail(ReasonSignatureInvalid)
	}
}

func credentialUsable(c Credential, in Inbound) bool {
	return c.TenantID != uuid.Nil && c.TenantID == in.TenantID && c.ProviderID != "" &&
		c.ProviderID == in.ProviderID && len(c.Secret) >= MinSecretBytes
}
