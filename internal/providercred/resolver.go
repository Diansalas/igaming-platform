package providercred

import (
	"context"
	"crypto/hmac"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/txscope"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// HandleReadSQL is THE handle read (ADR 0022 §3 point 9 as amended; ADR
// 0093 §4; ADR 0094 §4.1): one plain, lock-free, read-only SELECT with an
// explicit tenant_id = $1 predicate in addition to RLS. No FOR
// UPDATE/SHARE, no advisory lock, no write. It is the only
// pre-verification statement a KYC or casino callback may run; it runs in
// its own READ ONLY transaction that commits before any secret fetch, and
// the statement-capture tests
// (TestPointNineCapture_<Domain>_AllowsExactlyOneHandleRead) pin this exact
// text. The not_before/not_after window is evaluated by the database
// clock. $5 = ” selects every usable row of the binding (KeyImplicit and
// outbound); otherwise exactly the named key id.
const HandleReadSQL = `SELECT id, key_id, secret_ref, fingerprint, vendor_account_id, status, not_after ` +
	`FROM provider_credential_handles ` +
	`WHERE tenant_id = $1 AND domain = $2 AND provider_id = $3 AND purpose = $4 ` +
	`AND status IN ('active', 'verify_only') ` +
	`AND not_before <= now() AND (not_after IS NULL OR not_after > now()) ` +
	`AND ($5::text = '' OR key_id = $5::text) ` +
	`ORDER BY status, key_id LIMIT 3`

// HandleRecheckSQL is the post-verification re-check (ADR 0094 §5): the
// FIRST statement of the domain transaction after set_config. It is
// lock-free (no FOR SHARE: security accepted the narrower [re-check,
// commit] window over lock traffic on the bet path) and tenant-predicated,
// and binds the exact handle, fingerprint, domain, provider and purpose of
// the credential that VERIFIED. Zero rows (revoked, expired, rotated away)
// fails the callback closed; the statement-capture tests pin this text.
const HandleRecheckSQL = `SELECT 1 FROM provider_credential_handles ` +
	`WHERE tenant_id = $1 AND id = $2 AND fingerprint = $3 ` +
	`AND domain = $4 AND provider_id = $5 AND purpose = $6 ` +
	`AND status IN ('active', 'verify_only') ` +
	`AND not_before <= now() AND (not_after IS NULL OR not_after > now())`

type handleRow struct {
	id              uuid.UUID
	keyID           string
	secretRef       string
	fingerprint     string
	vendorAccountID *string
	status          string
	notAfter        *time.Time
}

// readHandles runs HandleReadSQL in tx. Any DB error is returned as is;
// callers fold it into a reason that does not depend on whether a handle
// exists (C5).
func readHandles(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, domain, providerID, purpose, keyID string) ([]handleRow, error) {
	rows, err := tx.Query(ctx, HandleReadSQL, tenantID, domain, providerID, purpose, keyID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []handleRow
	for rows.Next() {
		var h handleRow
		if err := rows.Scan(&h.id, &h.keyID, &h.secretRef, &h.fingerprint, &h.vendorAccountID, &h.status, &h.notAfter); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Resolver is the real inbound webhook credential resolver for one domain
// (webhookauth.Resolver). It holds no credential: every call reads the
// handle rows in its OWN short READ ONLY transaction, which commits, and
// only then fetches the pinned secret through the process-wide Fetcher -
// no pooled connection is ever held while waiting on the store (ADR 0094
// INV-POOL).
type Resolver struct {
	sub    *Subsystem
	domain string
}

// Resolver returns the real resolver for domain, or a TRUE nil interface
// when the subsystem is not constructed (callbacks then fail closed as
// no_resolver) or the domain is unknown.
func (s *Subsystem) Resolver(domain string) webhookauth.Resolver {
	if s == nil || !validDomain(domain) {
		return nil
	}
	return &Resolver{sub: s, domain: domain}
}

// MarkProductionEligible implements providerkind.ProductionEligible.
func (r *Resolver) MarkProductionEligible() {}

// Resolve implements webhookauth.Resolver (ADR 0093 §4 as amended by ADR
// 0094 §4.1). It must be called with NO transaction held: a ctx marked by
// txscope fails closed (credential_unavailable, no read, no store call,
// one secret_fetch_with_tx_held log line).
//
// Row counts: KeyFromHeader needs exactly one usable row for the named key
// id; KeyImplicit needs exactly one active row plus at most one
// verify_only row, both inside their windows. Any other count, any other
// selection, and any DB error fail closed as credential_unavailable - the
// same reason whether or not a handle exists (C5). A ref outside the
// tenant namespace or a fingerprint mismatch is credential_integrity (P1);
// a store failure without a cached value within max-stale is
// credential_store_unavailable.
func (r *Resolver) Resolve(ctx context.Context, reader webhookauth.TenantReader, tenantID uuid.UUID, providerID, keyID string, sel webhookauth.KeySelection) (webhookauth.CredentialSet, error) {
	if r == nil || r.sub == nil || reader == nil || tenantID == uuid.Nil || !webhookauth.ValidProviderID(providerID) {
		return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
	}
	if txscope.Held(ctx) {
		r.sub.logTxHeld("providercred.Resolver.Resolve", tenantID)
		return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
	}
	switch sel {
	case webhookauth.KeyFromHeader:
		if keyID == "" {
			return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
		}
	case webhookauth.KeyImplicit:
		if keyID != "" {
			return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
		}
	default:
		return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
	}

	var rows []handleRow
	err := reader.WithTenantReadOnly(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		rows, err = readHandles(ctx, tx, tenantID, r.domain, providerID, PurposeWebhookVerify, keyID)
		return err
	})
	if err != nil {
		// Security review C1 of PRH-I4 (HIGH): a capacity rejection from
		// the ADR 0097 pre-verification DB admission gate must NEVER be
		// folded into ErrCredentialUnavailable (which the uniform-401
		// contract treats identically to "no credential exists") - checked
		// FIRST, with errors.Is, and propagated distinctly so the eventual
		// caller answers a retryable 503, never the uniform 401. Every
		// OTHER WithTenantReadOnly error (a genuine DB failure) still folds
		// into ErrCredentialUnavailable exactly as before (C5's "any other
		// DB error fails closed as credential_unavailable" rule is
		// unchanged for real errors).
		if errors.Is(err, webhookauth.ErrTenantReaderUnavailable) {
			return webhookauth.CredentialSet{}, webhookauth.ErrTenantReaderUnavailable
		}
		return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
	}
	// The read-only transaction has committed: from here on no pooled
	// connection is held, and ctx is the caller's unmarked context.

	var active, previous *handleRow
	switch sel {
	case webhookauth.KeyFromHeader:
		if len(rows) != 1 {
			return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
		}
		active = &rows[0]
	case webhookauth.KeyImplicit:
		for i := range rows {
			switch rows[i].status {
			case "active":
				if active != nil {
					return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
				}
				active = &rows[i]
			case "verify_only":
				if previous != nil || rows[i].notAfter == nil {
					return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
				}
				previous = &rows[i]
			default:
				return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
			}
		}
		if active == nil {
			return webhookauth.CredentialSet{}, webhookauth.ErrCredentialUnavailable
		}
	}

	set := webhookauth.CredentialSet{}
	cred, err := r.sub.credentialFor(ctx, tenantID, r.domain, providerID, *active)
	if err != nil {
		return webhookauth.CredentialSet{}, err
	}
	if sel == webhookauth.KeyImplicit {
		// The active key of a KeyImplicit pair is "active" for Verify
		// (zero NotAfter); only the predecessor carries its window.
		cred.NotAfter = time.Time{}
	}
	set.Active = cred
	if previous != nil {
		p, err := r.sub.credentialFor(ctx, tenantID, r.domain, providerID, *previous)
		if err != nil {
			return webhookauth.CredentialSet{}, err
		}
		p.NotAfter = *previous.notAfter
		set.Previous = &p
	}
	return set, nil
}

// Recheck implements webhookauth.Resolver (ADR 0094 §5; security
// condition C4): inside the DOMAIN transaction tx, after verification, the
// handle of the credential that VERIFIED (Active or the verify_only
// predecessor - c.HandleID) must still be usable, with the same
// fingerprint, for this domain, provider and purpose, on the database
// clock. Zero rows, a zero HandleID, a tenant mismatch or any DB error is
// ErrCredentialUnavailable. It runs under tx's RLS as well as its explicit
// tenant predicate, so a credential for tenant A re-checked inside a
// tenant-B transaction finds nothing.
func (r *Resolver) Recheck(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, c webhookauth.Credential) error {
	if r == nil || tx == nil || tenantID == uuid.Nil || c.HandleID == uuid.Nil || c.TenantID != tenantID ||
		!webhookauth.ValidProviderID(c.ProviderID) || !ValidFingerprint(c.Fingerprint) {
		return webhookauth.ErrCredentialUnavailable
	}
	// The credential's secret must be the one its fingerprint - which
	// HandleRecheckSQL binds to the row - names (security review 17, S-3
	// option (b)): a VerifiedCallback can then carry only a secret the
	// store holds for this handle, never one chosen by in-process code.
	if r.sub == nil || !hmac.Equal([]byte(r.sub.key.Fingerprint(c.Secret)), []byte(c.Fingerprint)) {
		return webhookauth.ErrCredentialUnavailable
	}
	var one int
	if err := tx.QueryRow(ctx, HandleRecheckSQL, tenantID, c.HandleID, c.Fingerprint, r.domain, c.ProviderID, PurposeWebhookVerify).Scan(&one); err != nil {
		return webhookauth.ErrCredentialUnavailable
	}
	return nil
}

// credentialFor turns a handle row into a verified Credential: the ref
// must parse and sit inside the row's own tenant namespace (C1, resolver
// side), its scheme must have a backend in this process, and the fetched
// secret's keyed fingerprint must equal the row's (on every resolve,
// cached or not).
func (s *Subsystem) credentialFor(ctx context.Context, tenantID uuid.UUID, domain, providerID string, h handleRow) (webhookauth.Credential, error) {
	secret, err := s.secretFor(ctx, tenantID, domain, providerID, h)
	if err != nil {
		return webhookauth.Credential{}, err
	}
	c := webhookauth.Credential{
		TenantID:    tenantID,
		ProviderID:  providerID,
		KeyID:       h.keyID,
		Secret:      secret.Bytes(),
		Fingerprint: h.fingerprint,
		HandleID:    h.id,
	}
	if h.vendorAccountID != nil {
		c.BoundAccountID = *h.vendorAccountID
	}
	if h.notAfter != nil {
		c.NotAfter = *h.notAfter
	}
	return c, nil
}

// secretFor fetches and integrity-checks a handle row's secret. Errors are
// webhookauth sentinels.
func (s *Subsystem) secretFor(ctx context.Context, tenantID uuid.UUID, domain, providerID string, h handleRow) (secretstore.Secret, error) {
	ref, err := secretstore.ParseRef(h.secretRef)
	if err != nil || !ref.InNamespace(tenantID, domain, providerID) || !ValidFingerprint(h.fingerprint) {
		s.logger.Error("provider_credential_integrity_failure",
			"alert", "P1", "reason", string(webhookauth.ReasonCredentialIntegrity),
			"class", "ref_outside_namespace", "tenant_id", tenantID.String(), "handle_id", h.id.String())
		return secretstore.Secret{}, webhookauth.ErrCredentialIntegrity
	}
	secret, err := s.fetcher.Fetch(ctx, tenantID, ref, h.fingerprint)
	if err != nil {
		return secretstore.Secret{}, foldStoreError(err)
	}
	return secret, nil
}

// foldStoreError maps a secretstore class onto the resolver sentinels.
func foldStoreError(err error) error {
	var se *secretstore.Error
	if !errors.As(err, &se) {
		return webhookauth.ErrCredentialStoreUnavailable
	}
	switch se.Class {
	case secretstore.ClassIntegrity, secretstore.ClassInvalidRef:
		return webhookauth.ErrCredentialIntegrity
	case secretstore.ClassNoBackend:
		// The row's scheme has no backend in this process (for example a
		// memory:// row outside tests): security review §4.1 point 3.
		return webhookauth.ErrCredentialUnavailable
	default:
		return webhookauth.ErrCredentialStoreUnavailable
	}
}
