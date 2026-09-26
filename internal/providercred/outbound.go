package providercred

import (
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/txscope"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// PROV-OUTBOUND-CRED-1 (ADR 0093 §5; security review §2, binding
// precisions 1-4).
//
//  1. The outbound_api handle row is read in a short tenant-scoped
//     transaction that has COMMITTED before this package returns, so no
//     outbound HTTP call is ever made while that transaction is held. The
//     in-flight exposure is one call: a revoke that commits between the
//     read and the send.
//  2. A transport retry inside one call's timeout budget may reuse the
//     credential; anything scheduled later re-resolves.
//  3. A credential derived from the secret (an OAuth/bearer token, a session)
//     may be cached only in a DerivedTokenCache, keyed on (tenant, handle,
//     fingerprint) and served only after THIS call's handle read returned
//     that handle active.
//  4. Keep-alive connections are fine for bearer-style auth; mTLS client
//     certificates are PROVIDER DEPENDENT and out of Stage 10.3 scope.
//
// Nothing here is cached in an adapter, an Authenticator, an HTTP client
// or an SDK session: callers build a per-call httpclient.Authenticator from
// the OutboundCredential and drop both when the call returns.

// ErrOutboundCredentialUnavailable is the single, retryable, internal
// failure of an outbound credential resolution. No request may be sent to
// the vendor after it, and there is never a fallback to another credential
// or to an unauthenticated call.
var ErrOutboundCredentialUnavailable = errors.New("providercred: outbound provider credential unavailable (retryable)")

// OutboundCredential is one call's resolved outbound credential. It carries
// the tenant it was resolved for, and redacts its secret on every
// rendering path. It must never be stored in a long-lived value.
type OutboundCredential struct {
	TenantID        uuid.UUID
	Domain          string
	ProviderID      string
	HandleID        uuid.UUID
	KeyID           string
	Fingerprint     string
	VendorAccountID string
	secret          secretstore.Secret
}

// Secret returns a copy of the secret bytes.
func (c OutboundCredential) Secret() []byte { return c.secret.Bytes() }

// String implements fmt.Stringer (never the secret).
func (c OutboundCredential) String() string {
	return fmt.Sprintf("OutboundCredential{TenantID:%s Domain:%s ProviderID:%s HandleID:%s KeyID:%s Fingerprint:%s}",
		c.TenantID, c.Domain, c.ProviderID, c.HandleID, c.KeyID, c.Fingerprint)
}

// GoString implements fmt.GoStringer.
func (c OutboundCredential) GoString() string { return c.String() }

// Format implements fmt.Formatter.
func (c OutboundCredential) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(c.String())) }

// LogValue implements slog.LogValuer.
func (c OutboundCredential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("tenant_id", c.TenantID.String()),
		slog.String("domain", c.Domain),
		slog.String("provider_id", c.ProviderID),
		slog.String("handle_id", c.HandleID.String()),
		slog.String("key_id", c.KeyID),
		slog.String("fingerprint", c.Fingerprint),
	)
}

// MarshalJSON implements json.Marshaler (never the secret).
func (c OutboundCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{
		"tenant_id": c.TenantID.String(), "domain": c.Domain, "provider_id": c.ProviderID,
		"handle_id": c.HandleID.String(), "key_id": c.KeyID, "fingerprint": c.Fingerprint,
	})
}

// TenantTxRunner is what OutboundResolver needs from the pool: a
// tenant-scoped transaction that commits before WithTenant returns.
// *db.Pool satisfies it.
type TenantTxRunner interface {
	WithTenant(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error
}

// OutboundResolver resolves a domain's outbound_api credential per call.
type OutboundResolver struct {
	sub    *Subsystem
	domain string
}

// Outbound returns the domain's outbound resolver, or nil when the
// subsystem is not constructed (non-synthetic outbound calls then fail
// closed).
func (s *Subsystem) Outbound(domain string) *OutboundResolver {
	if s == nil || !validDomain(domain) {
		return nil
	}
	return &OutboundResolver{sub: s, domain: domain}
}

// MarkProductionEligible implements providerkind.ProductionEligible.
func (o *OutboundResolver) MarkProductionEligible() {}

// Resolve reads the tenant's single active outbound_api handle for
// providerID in its OWN short tenant-scoped transaction (committed before
// the secret fetch and before the caller's HTTP call), then fetches and
// integrity-checks the pinned secret. The caller must not hold a DB
// transaction across the HTTP call that uses the result. A nil receiver,
// a missing, expired or revoked handle, a store failure or an integrity
// failure are all ErrOutboundCredentialUnavailable.
func (o *OutboundResolver) Resolve(ctx context.Context, pool TenantTxRunner, tenantID uuid.UUID, providerID string) (OutboundCredential, error) {
	if o == nil || o.sub == nil || pool == nil || tenantID == uuid.Nil || !webhookauth.ValidProviderID(providerID) {
		return OutboundCredential{}, ErrOutboundCredentialUnavailable
	}
	if txscope.Held(ctx) {
		// ADR 0094 INV-POOL: resolving inside a caller's transaction would
		// hold that connection across a nested acquisition and the store.
		o.sub.logTxHeld("providercred.OutboundResolver.Resolve", tenantID)
		return OutboundCredential{}, ErrOutboundCredentialUnavailable
	}
	var rows []handleRow
	err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = readHandles(ctx, tx, tenantID, o.domain, providerID, PurposeOutboundAPI, "")
		return err
	})
	if err != nil || len(rows) != 1 || rows[0].status != "active" {
		return OutboundCredential{}, ErrOutboundCredentialUnavailable
	}
	h := rows[0]
	secret, err := o.sub.secretFor(ctx, tenantID, o.domain, providerID, h)
	if err != nil {
		return OutboundCredential{}, ErrOutboundCredentialUnavailable
	}
	c := OutboundCredential{
		TenantID: tenantID, Domain: o.domain, ProviderID: providerID,
		HandleID: h.id, KeyID: h.keyID, Fingerprint: h.fingerprint, secret: secret,
	}
	if h.vendorAccountID != nil {
		c.VendorAccountID = *h.vendorAccountID
	}
	return c, nil
}

// derivedTokenCacheMaxEntries bounds DerivedTokenCache so an unbounded
// number of rotations/tenants cannot grow it forever. It is generous
// relative to any realistic per-process count of (tenant, handle,
// fingerprint) triples live at once; eviction is FIFO-by-insertion once
// the bound is hit (CODE-HYGIENE-10.3-1 item 2).
const derivedTokenCacheMaxEntries = 4096

// DerivedTokenCache holds credentials DERIVED from an outbound secret
// (security review §2 precision 3). An entry is keyed on (tenant, handle
// id, fingerprint) and is served only for an OutboundCredential that THIS
// call's handle read just returned, so a revoked or rotated handle can
// never reach its token. Its TTL never exceeds the vendor's own expiry.
//
// The cache is bounded (derivedTokenCacheMaxEntries) with FIFO-by-
// insertion eviction once full, and a Put for a rotated fingerprint
// proactively evicts every other entry sharing the same (tenant, handle)
// pair - a stale fingerprint can never be served again (the key no longer
// matches an OutboundCredential a live read would return), so there is no
// reason to keep its derived bytes in memory until it ages out or the
// cache happens to fill up. Evicted entries have their token bytes
// zeroed before being dropped, so no derived secret material is retained
// past eviction.
type DerivedTokenCache struct {
	mu      sync.Mutex
	now     func() time.Time
	maxSize int
	order   *list.List // list of *derivedEntry; front = oldest
	entries map[derivedKey]*list.Element
}

type derivedKey struct {
	tenant      uuid.UUID
	handle      uuid.UUID
	fingerprint string
}

// redactedDerivedToken is what derivedTokenBytes prints instead of its
// bytes, on every formatting path.
const redactedDerivedToken = "[REDACTED-DERIVED-TOKEN]"

// derivedTokenBytes holds one derived token's own copy of its raw bytes and
// redacts them on every rendering path (String/GoString/Format), mirroring
// secretstore.Secret's redaction contract for the outbound secret itself -
// so a derivedEntry accidentally formatted with %v/%+v/%#v (a debug helper,
// a failed assertion's t.Logf, a future slog call) prints
// "[REDACTED-DERIVED-TOKEN]", never the token (code review F-2 on
// CODE-HYGIENE-10.3-1 item 2). It also exposes an in-place zero() for
// eviction, which secretstore.Secret does not: this stays a small,
// cache-local type - rather than adding that operation to
// secretstore.Secret - because internal/secretstore is being changed
// elsewhere (F-POOL-1) and must not be touched here.
type derivedTokenBytes []byte

// newDerivedTokenBytes copies b so the cache's own copy can never be
// mutated by a caller that still holds the slice it passed to Put.
func newDerivedTokenBytes(b []byte) derivedTokenBytes {
	out := make(derivedTokenBytes, len(b))
	copy(out, b)
	return out
}

// bytes returns a fresh copy, so a caller of Get can never mutate what the
// cache holds, and zeroing the cache's copy later can never corrupt a copy
// a caller is still using.
func (b derivedTokenBytes) bytes() []byte {
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// zero overwrites b's bytes in place (used on eviction), so evicted
// derived-secret material does not linger in the Go heap for an
// indeterminate time before GC reclaims it.
func (b derivedTokenBytes) zero() {
	for i := range b {
		b[i] = 0
	}
}

func (derivedTokenBytes) String() string { return redactedDerivedToken }

func (derivedTokenBytes) GoString() string { return redactedDerivedToken }

func (derivedTokenBytes) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redactedDerivedToken)) }

type derivedEntry struct {
	key     derivedKey
	token   derivedTokenBytes
	expires time.Time
}

// String/GoString/Format on derivedEntry itself (not just on
// derivedTokenBytes) matter because Go's fmt package only consults a
// FIELD's own String/GoString/Format methods when that field is exported;
// derivedEntry.token is unexported, so formatting a *derivedEntry or
// derivedEntry directly (e.g. a debug helper, or a failed assertion's
// t.Logf("%+v", *el.Value.(*derivedEntry))) would otherwise fall through to
// fmt's default reflection-based struct dump, which DOES print an
// unexported []byte-kind field's raw byte values (as a list of numbers) -
// fully reconstructable plaintext, not just an unredacted display. Defining
// these directly on derivedEntry closes that gap (code review F-2 on
// CODE-HYGIENE-10.3-1 item 2).
func (e derivedEntry) String() string {
	return fmt.Sprintf("derivedEntry{tenant:%s handle:%s fingerprint:%s expires:%s token:%s}",
		e.key.tenant, e.key.handle, e.key.fingerprint, e.expires, redactedDerivedToken)
}

func (e derivedEntry) GoString() string { return e.String() }

func (e derivedEntry) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(e.String())) }

// NewDerivedTokenCache builds an empty, bounded cache (now nil means
// time.Now).
func NewDerivedTokenCache(now func() time.Time) *DerivedTokenCache {
	if now == nil {
		now = time.Now
	}
	return &DerivedTokenCache{
		now:     now,
		maxSize: derivedTokenCacheMaxEntries,
		order:   list.New(),
		entries: map[derivedKey]*list.Element{},
	}
}

func keyFor(c OutboundCredential) derivedKey {
	return derivedKey{tenant: c.TenantID, handle: c.HandleID, fingerprint: c.Fingerprint}
}

// removeElementLocked drops el from both the entries map and the order
// list, zeroing its token bytes first. Callers must hold d.mu.
func (d *DerivedTokenCache) removeElementLocked(el *list.Element) {
	e := el.Value.(*derivedEntry)
	e.token.zero()
	delete(d.entries, e.key)
	d.order.Remove(el)
}

// Get returns the token derived from exactly this credential, if still
// within its expiry.
func (d *DerivedTokenCache) Get(c OutboundCredential) ([]byte, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	el, ok := d.entries[keyFor(c)]
	if !ok {
		return nil, false
	}
	e := el.Value.(*derivedEntry)
	if !d.now().Before(e.expires) {
		d.removeElementLocked(el)
		return nil, false
	}
	return e.token.bytes(), true
}

// Put stores token derived from c until vendorExpiry (the vendor's own
// expiry is the maximum TTL). Any existing entry for a different
// fingerprint of the same (tenant, handle) pair - i.e. a rotation - is
// evicted first, and the cache is trimmed back to its bound afterwards.
func (d *DerivedTokenCache) Put(c OutboundCredential, token []byte, vendorExpiry time.Time) {
	if c.HandleID == uuid.Nil || c.TenantID == uuid.Nil || !d.now().Before(vendorExpiry) {
		return
	}
	key := keyFor(c)

	d.mu.Lock()
	defer d.mu.Unlock()

	for k, el := range d.entries {
		if k.tenant == key.tenant && k.handle == key.handle && k.fingerprint != key.fingerprint {
			d.removeElementLocked(el)
		}
	}
	if el, ok := d.entries[key]; ok {
		d.removeElementLocked(el)
	}

	el := d.order.PushBack(&derivedEntry{key: key, token: newDerivedTokenBytes(token), expires: vendorExpiry})
	d.entries[key] = el

	for d.order.Len() > d.maxSize {
		d.removeElementLocked(d.order.Front())
	}
}
