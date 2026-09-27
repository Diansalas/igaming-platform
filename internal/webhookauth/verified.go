package webhookauth

// Two-phase callback verification (ADR 0094 §4.1; security design review
// of ADR 0094, conditions C1-C4).
//
// Phase 1 (a domain's VerifyCallback, NO transaction held): clone the
// inbound, resolve the credential (its handle read runs in its own READ
// ONLY transaction, committed before any secret fetch), run VerifyInbound,
// and seal the result in a VerifiedCallback.
//
// Phase 2 (a domain's ReceiveVerifiedCallback, inside the DOMAIN
// transaction): Redeem the VerifiedCallback - single use, age-bounded,
// bound to (domain, tenant, provider) - and re-check the verified handle
// in that transaction (Resolver.Recheck) before anything is read or
// written.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxVerifiedCallbackAge bounds how long after verification a
// VerifiedCallback may still be redeemed (security condition C2): well
// inside every scheme's MaxSkew (<= MaxSkewCap), so the replay window of
// ADR 0022 §3 point 10 still holds end to end across the two phases.
const MaxVerifiedCallbackAge = 30 * time.Second

// sinceVerified is time.Since (monotonic); tests in this package replace it.
var sinceVerified = time.Since

// VerifiedCallback is the opaque proof that one inbound callback verified
// (ADR 0094 §4.1). It is created only by ResolveAndSeal - which resolves
// the credentials itself through the domain's Resolver and runs
// VerifyInbound - and is consumed by the FIRST Redeem call:
//
//   - single use (C1): the consumed flag is shared by every copy, so a
//     copied value cannot be redeemed twice either;
//   - age-bounded (C2): Redeem refuses it after MaxVerifiedCallbackAge;
//   - bound to the domain, tenant and provider it verified for;
//   - it owns a private copy of the verified inbound (C3): the bytes phase
//     2 hands to HandleCallback are exactly the bytes that verified, and no
//     caller slice aliases them;
//   - every rendering path redacts (C3/C15): it carries the credential
//     secret, which HandleCallback's defence-in-depth re-verify needs.
//
// All fields are unexported; the zero value (and a nil pointer) is
// rejected by Redeem.
type VerifiedCallback struct {
	domain     string
	tenantID   uuid.UUID
	providerID string
	in         Inbound
	cred       Credential
	verifiedAt time.Time
	consumed   *atomic.Bool
}

// CloneInbound returns a deep copy of in: a fresh body slice and cloned
// headers (C3). ResolveAndSeal calls it first - the single chokepoint
// (security review 17, S-1) - and verifies and stores only the copy.
func CloneInbound(in Inbound) Inbound {
	out := Inbound{TenantID: in.TenantID, ProviderID: in.ProviderID}
	if in.Body != nil {
		out.Body = make([]byte, len(in.Body))
		copy(out.Body, in.Body)
	}
	if in.Header != nil {
		out.Header = in.Header.Clone()
	} else {
		out.Header = http.Header{}
	}
	return out
}

// ResolveAndSeal is phase 1's resolve-verify-seal step and the ONLY way to
// obtain a VerifiedCallback (security review 17, S-3 option (a)):
//
//  1. it copies in (CloneInbound - the one chokepoint for C3, S-1): the
//     bytes verified and stored are a private copy no caller slice or
//     header map aliases;
//  2. it resolves the credential set itself, through resolver and r
//     (ResolveCredentials), so a caller cannot hand in a CredentialSet of
//     its choosing;
//  3. it runs VerifyInbound on the copy and seals the result for domain.
//
// in must already carry the route TenantID/ProviderID. It returns the
// credential that verified (Active or the verify_only predecessor), whose
// HandleID phase 2 re-checks (C4). The real resolver's Recheck also binds
// the credential's secret to the fingerprint the handle row pins (S-3
// option (b)), so a sealed token carries only a secret the store holds.
func ResolveAndSeal(ctx context.Context, r TenantReader, domain string, scheme VerificationScheme, resolver Resolver, in Inbound, m AuthMaterial) (*VerifiedCallback, Credential, *AuthError) {
	in = CloneInbound(in)
	creds, authErr := ResolveCredentials(ctx, r, scheme, resolver, in, m)
	if authErr != nil {
		return nil, Credential{}, authErr
	}
	return verifyAndSeal(domain, scheme, creds, in, m, time.Now())
}

// verifyAndSeal runs VerifyInbound on in (already the private copy) and,
// only on success, seals it for domain.
func verifyAndSeal(domain string, scheme VerificationScheme, creds CredentialSet, in Inbound, m AuthMaterial, now time.Time) (*VerifiedCallback, Credential, *AuthError) {
	cred, authErr := VerifyInbound(scheme, creds, in, m, now)
	if authErr != nil {
		return nil, Credential{}, authErr
	}
	if domain == "" || in.TenantID == uuid.Nil || in.ProviderID == "" {
		return nil, Credential{}, &AuthError{Reason: ReasonCredentialUnavailable, KeyID: m.KeyID}
	}
	return &VerifiedCallback{
		domain:     domain,
		tenantID:   in.TenantID,
		providerID: in.ProviderID,
		in:         in,
		cred:       cred,
		verifiedAt: time.Now(),
		consumed:   new(atomic.Bool),
	}, cred, nil
}

// Redeem is phase 2's entry (ADR 0094 §4.1/§5). In order, any failure
// being the uniform credential_unavailable:
//
//  1. v must be a sealed VerifiedCallback not yet redeemed - the first
//     call consumes it atomically, whatever its outcome (C1);
//  2. it must be for domain, tenantID and providerID (the route values
//     the domain transaction runs under);
//  3. it must be at most MaxVerifiedCallbackAge old (C2);
//  4. resolver.Recheck(tx) must confirm, inside the domain transaction,
//     that the handle of the credential that VERIFIED is still usable
//     (revoked, expired or rotated-away fails; a DB error fails).
//
// It returns the private inbound copy and the verified credential for
// HandleCallback. Nothing is read or written by the caller before this
// succeeds.
func (v *VerifiedCallback) Redeem(ctx context.Context, tx pgx.Tx, domain string, tenantID uuid.UUID, providerID string, resolver Resolver) (Inbound, Credential, *AuthError) {
	fail := func(keyID string) (Inbound, Credential, *AuthError) {
		return Inbound{}, Credential{}, &AuthError{Reason: ReasonCredentialUnavailable, KeyID: keyID}
	}
	if v == nil || v.consumed == nil {
		return fail("")
	}
	if !v.consumed.CompareAndSwap(false, true) {
		return fail(v.cred.KeyID)
	}
	if v.domain != domain || domain == "" || v.tenantID != tenantID || tenantID == uuid.Nil ||
		v.providerID != providerID || v.in.TenantID != tenantID || v.in.ProviderID != providerID {
		return fail(v.cred.KeyID)
	}
	if age := sinceVerified(v.verifiedAt); age < 0 || age > MaxVerifiedCallbackAge {
		return fail(v.cred.KeyID)
	}
	if resolver == nil || tx == nil {
		return fail(v.cred.KeyID)
	}
	if err := resolver.Recheck(ctx, tx, tenantID, v.cred); err != nil {
		return fail(v.cred.KeyID)
	}
	return v.in, v.cred, nil
}

// String implements fmt.Stringer: never the secret, the body or headers.
func (v VerifiedCallback) String() string {
	return fmt.Sprintf("VerifiedCallback{Domain:%s TenantID:%s ProviderID:%s KeyID:%s}",
		v.domain, v.tenantID, v.providerID, v.cred.KeyID)
}

// GoString implements fmt.GoStringer.
func (v VerifiedCallback) GoString() string { return v.String() }

// Format implements fmt.Formatter: every verb renders the redacted form.
func (v VerifiedCallback) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(v.String())) }

// LogValue implements slog.LogValuer.
func (v VerifiedCallback) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("domain", v.domain),
		slog.String("tenant_id", v.tenantID.String()),
		slog.String("provider_id", v.providerID),
		slog.String("key_id", v.cred.KeyID),
	)
}

// MarshalJSON implements json.Marshaler (never the secret, body or
// headers).
func (v VerifiedCallback) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"domain": v.domain, "tenant_id": v.tenantID.String(), "provider_id": v.providerID, "key_id": v.cred.KeyID,
	})
}
