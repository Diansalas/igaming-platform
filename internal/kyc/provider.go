package kyc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// ProviderOutcome is what a KYCProvider reports - normalized, platform-
// owned values only. A real adapter translates its own vendor-specific
// status strings into exactly one of these before ever returning
// (directive §10: "do not allow provider-specific status strings to leak
// into core business logic").
type ProviderOutcome string

const (
	ProviderApproved       ProviderOutcome = "approved"
	ProviderRejected       ProviderOutcome = "rejected"
	ProviderPending        ProviderOutcome = "pending"
	ProviderReviewRequired ProviderOutcome = "review_required"
	ProviderExpired        ProviderOutcome = "expired"
	// ProviderError signals the provider call itself failed (network,
	// vendor outage, malformed callback) - distinct from a legitimate
	// Rejected verdict. Never applied to a Verification's own status;
	// the orchestrator logs/audits it and leaves the existing status
	// untouched (directive §10: "can KYC provider status corrupt
	// platform state" - an error must never do so).
	ProviderError ProviderOutcome = "error"
)

// Capabilities describes what a registered KYCProvider supports -
// mirrors casino.CasinoProvider/payments.PaymentProvider's identical
// "ask the adapter what it can do" convention, so future callers never
// need a provider-specific type switch.
type Capabilities struct {
	SupportedDocumentTypes []DocumentType
	SupportsCallback       bool
}

// CreateVerificationInput is CreateVerification's argument - carries only
// opaque platform identifiers, never a document or evidence payload
// (those are submitted separately via SubmitVerification once uploaded
// to this platform's own DocumentStorageProvider).
type CreateVerificationInput struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	PersonID        uuid.UUID
}

// SubmittedDocument references a document already stored by this
// platform - a real adapter uses DocumentID/DocumentType to retrieve
// actual bytes from its own separate integration with
// DocumentStorageProvider (out of this stage's scope to wire together;
// see docs/decisions/0028 §5's recorded OPEN CONSIDERATION). This
// interface deliberately never carries raw document bytes itself - doing
// so would hard-code an assumption about how every future vendor wants
// evidence delivered (upload URL vs. direct bytes vs. pull-by-reference
// all differ per vendor).
type SubmittedDocument struct {
	DocumentID   uuid.UUID
	DocumentType DocumentType
}

// ProviderResult is every KYCProvider method's return value - the ONE
// normalized shape core business logic (internal/kyc's own service
// functions, and every caller) ever sees. ProviderReference is required
// on a successful CreateVerification/GetVerification/SubmitVerification
// call and on any HandleCallback result carrying enough information to
// identify which verification it concerns; Reason is a short,
// non-sensitive, machine-readable code (never raw evidence - directive
// §10/§17), mirroring internal/identityresolution.ResolutionResult's
// identical "Reason must never contain evidence" contract (ADR 0027).
//
// KYC-REASON-BOUND-1 (Stage 10.3, security review C16): that intent is
// now an enforced bound, not just a convention. Every adapter's own
// HandleCallback (and any other method populating Reason) MUST run its
// raw vendor status/reason text through NormalizeReason
// (reason_normalize.go) before returning - bounding it to
// MaxReasonBytes (512 bytes) and stripping C0/C1 controls and Unicode
// bidi/format controls. Reason is staff/compliance-only: HD-10.3-3
// (binding human ruling) is that a player never sees ANY provider
// reason text, under any field name - only a closed VerificationStatus.
// The database enforces the length half of this bound independently
// (migration 0095's CHECK on kyc_verifications.reason), but a
// non-conforming adapter must not rely on that as its only defense -
// RunProviderConformanceSuite's mandatory reason-bound case
// (internal/kyc/conformance_test.go) fails, not skips, for any adapter
// that does not normalize.
//
// Gate 10.3-W1 fix round (security S-5, identity-compliance condition 1):
// the PLATFORM also applies NormalizeReason (idempotent) at every write
// site that persists or audits Reason - CreateVerification, the document
// submission path and applyCallbackOutcome - so a non-conforming adapter
// method can no longer store control/bidi text or trip migration 0095's
// CHECK into a 500. ReasonTruncated is how an adapter that already
// normalized reports that ITS normalization truncated (the platform's
// second pass cannot see that); the platform ORs it with its own signal and
// records `reason_truncated: true` in the audit metadata.
type ProviderResult struct {
	ProviderReference string
	Outcome           ProviderOutcome
	Reason            string
	ReasonTruncated   bool
}

// KYCProvider is the provider-neutral interface every adapter (real or
// mock) implements - mirrors casino.CasinoProvider/payments.
// PaymentProvider's exact shape (directive §9/§11: "the architecture
// must support Provider A removed, Provider B added" without touching
// Verification/Document/Person/PlayerAccount).
type KYCProvider interface {
	ID() string
	CreateVerification(ctx context.Context, input CreateVerificationInput) (ProviderResult, error)
	GetVerification(ctx context.Context, providerReference string) (ProviderResult, error)
	SubmitVerification(ctx context.Context, providerReference string, documents []SubmittedDocument) (ProviderResult, error)
	// HandleCallback verifies in's authentication against cred (over the
	// raw bytes, BEFORE any parsing - ADR 0022 §3 point 7, extracted into
	// internal/webhookauth by Stage 10.2/ADR 0091), then parses the
	// now-authenticated body and returns the normalized result, including
	// which ProviderReference it concerns. cred is the SINGLE candidate
	// credential the Orchestrator already resolved and equality-checked
	// against in.TenantID/in.ProviderID - never re-resolved here. Returns
	// ErrCallbackSignatureInvalid for any pre-parse authentication
	// failure, and ErrCallbackMalformedBody for any post-verification
	// structural failure (missing provider_reference, an outcome outside
	// the closed enum, unparseable JSON).
	HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (ProviderResult, error)
	// WebhookScheme returns this adapter's inbound-callback verification
	// scheme (Stage 10.3 W1a, WH-VENDOR-SCHEME-1): validated at
	// registration, selected by provider id in the shared HTTP preamble,
	// and run by the Orchestrator ITSELF before HandleCallback.
	WebhookScheme() webhookauth.VerificationScheme
	GetCapabilities() Capabilities
	HealthStatus(ctx context.Context) error
}

var (
	// ErrCallbackSignatureInvalid is returned by a KYCProvider's
	// HandleCallback for any authentication failure over the raw body -
	// an alias of the single shared webhookauth sentinel (Stage 10.2, ADR
	// 0091), so errors.Is/errors.As behave identically whether callers
	// check the KYC-specific name or the shared one.
	ErrCallbackSignatureInvalid = webhookauth.ErrSignatureInvalid

	// ErrCallbackMalformedBody is returned by a KYCProvider adapter's
	// HandleCallback ONLY after its own authentication has already
	// succeeded (never for an authentication failure - that is always
	// ErrCallbackSignatureInvalid) - a distinct sentinel the caller maps
	// to 400, never to the uniform pre-verification 401.
	ErrCallbackMalformedBody = errors.New("kyc: verified callback body is malformed")

	// ErrCallbackAuthFailed is the single sentinel every pre-verification
	// inbound-callback rejection wraps - the same value as
	// webhookauth.ErrAuthFailed, so a caller can use either name with
	// errors.Is/errors.As to *CallbackAuthError.
	ErrCallbackAuthFailed = webhookauth.ErrAuthFailed
)

// CallbackAuthReason is the closed, allow-listed reason enum behind
// ErrCallbackAuthFailed - an alias of the single shared webhookauth.Reason
// enum (Stage 10.2, ADR 0091, architect ruling): KYC does not invent a
// second reason vocabulary.
type CallbackAuthReason = webhookauth.Reason

// CallbackAuthError is ErrCallbackAuthFailed's concrete carrier - an alias
// of webhookauth.AuthError, so errors.As to *CallbackAuthError and to
// *webhookauth.AuthError behave identically.
type CallbackAuthError = webhookauth.AuthError

// Orchestrator holds the process-global provider registry, keyed by
// provider id - mirrors casino.Orchestrator/payments.Orchestrator
// exactly. A tenant's choice of WHICH provider to use for a given
// verification is out of this stage's scope (today: exactly one, the
// mock, is ever registered - see cmd/platform-api/main.go); a future
// per-tenant capability/routing model (mirroring ADR 0022/0025's
// ProviderCapability tables) is a documented extension point, not
// invented here without a real second provider to route between.
//
// webhookCredentialResolver is the SINGLE component that ever sees
// inbound-webhook credential secret material (Stage 10.2, ADR 0091,
// design §B). A nil resolver fails every callback closed with
// ReasonNoResolver - there is no fallback to unauthenticated
// verification, and this is deliberately the default until a real
// resolver exists for KYC (NOT IMPLEMENTED).
type Orchestrator struct {
	providers                 map[string]KYCProvider
	webhookCredentialResolver webhookauth.Resolver
	// webhookSchemes: every adapter's validated WebhookScheme() (Stage
	// 10.3 W1a; webhook_verify.go).
	webhookSchemes *webhookauth.SchemeSet
	// webhookLogger receives the matched-key_id line after a successful
	// callback verification (webhook_verify.go, W2A-SEC-2). Nil means
	// slog.Default().
	webhookLogger *slog.Logger
}

// NewOrchestrator constructs an Orchestrator. resolver may be nil (every
// callback then fails closed as ReasonNoResolver) - callers wire a real
// resolver only where cfg.TestSupportRoutesEnabled() (ADR 0085), see
// cmd/platform-api/wiring.go's mockProviderWiring.
//
// Stage 10.3 W1a: every adapter's WebhookScheme() is validated here - a bad
// declaration panics, so the process refuses to start.
func NewOrchestrator(providers map[string]KYCProvider, resolver webhookauth.Resolver) *Orchestrator {
	// ADR 0097 §6.3/§20 AC6: fail-closed registration for undeclared
	// non-MOCK webhook adapters (see payments.NewOrchestrator's identical
	// comment).
	webhookauth.MustRequireRetrySemantics("kyc", providers)
	return &Orchestrator{providers: providers, webhookCredentialResolver: resolver, webhookSchemes: mustKYCSchemeSet(providers)}
}

func (o *Orchestrator) Provider(id string) (KYCProvider, bool) {
	p, ok := o.providers[id]
	return p, ok
}

// ReceiveCallback dispatches a verified provider callback (Stage 10.2,
// ADR 0091, design §B6/§B7; architect rulings R3-R5/J5-J7).
//
// tenantID/providerID are the ROUTE-resolved values (an HTTP handler's
// per-tenant webhook path segments, resolved and the tenant's active
// status checked BEFORE calling this function) - in.TenantID/in.ProviderID
// are OVERWRITTEN from these parameters FIRST, never trusted from
// whatever the caller happened to set on in (architect R3): one tenant id
// flows from the route, through RLS (tx must already be tenant-scoped via
// db.Pool.WithTenant), through the resolver, into the signing input that
// is verified, and into every write this function makes.
//
// Order, strictly BEFORE any tenant-scoped read, lock, write, or audit row
// (architect §2/J8, strict I1 - the only permitted statement before
// verification succeeds is the caller's own GetTenantBySlug, platform-wide,
// plus WithTenant's own set_config):
//
//	(a) the adapter must be registered, else ReasonProviderUnregistered;
//	(b) a resolver must exist (else ReasonNoResolver) and must resolve a
//	    single candidate credential for (tenantID, providerID, keyID) (else
//	    ReasonCredentialUnavailable), which must itself be bound to exactly
//	    this (tenantID, providerID) (else ReasonCredentialUnavailable,
//	    defense in depth - no conforming resolver should ever return a
//	    mismatched credential);
//	(c) this Orchestrator runs the adapter's WebhookScheme().Verify over
//	    the raw bytes against that credential (Stage 10.3 W1a: the
//	    orchestrator, not the adapter, is the mandatory verifier;
//	    ReasonSignatureInvalid / ReasonTimestampOutOfWindow on failure),
//	    then provider.HandleCallback parses the now-authenticated body (ErrCallbackMalformedBody on
//	    a structural failure - a DIFFERENT, POST-verification error class
//	    the caller maps to 400, never the uniform 401);
//	(d) the verification is looked up by an EXPLICIT
//	    tenant_id = $1 AND provider_id = $2 AND provider_reference = $3
//	    predicate (architect R4/J6, on top of kyc_verifications' own
//	    tenant_isolation RLS and migration 0040's identically-shaped
//	    UNIQUE index) - not found is ErrNotFound, reachable only by a
//	    VERIFIED caller (never an enumeration oracle for an unauthenticated
//	    one);
//	(e) the forward-only rank transition and its audit row (B7 below).
//
// Every failure before (c) succeeds returns a *CallbackAuthError wrapping
// ErrCallbackAuthFailed with a closed, allow-listed reason - the caller
// (an HTTP handler) maps every one of them to the SAME uniform 401
// response, so an unauthenticated caller can never distinguish "unknown
// provider" from "bad signature" by status code or body.
//
// applied (Stage 10.2 final review, K4) reports whether this call actually
// wrote anything: true for a forward status transition (with its one
// success audit row) and for a non-terminal outcome=error (with its one
// failure audit row, B7/K5); false for every no-op - a replay, anything
// arriving at or behind the verification's current rank (including after a
// terminal status or a staff decision), and an outcome=error delivered
// against an ALREADY-TERMINAL verification (K5: no audit row in that one
// case, since replaying it must stay inert like every other replay). The
// caller (an HTTP handler) uses applied==false to log a single allow-listed
// kyc_webhook_noop info line - never to change the response itself, which
// stays 204 either way.
func (o *Orchestrator) ReceiveVerifiedCallback(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, v *VerifiedCallback) (result Verification, applied bool, err error) {
	// (a)+(b)+verify ran in phase 1 (VerifyCallback, no transaction held;
	// ADR 0094 §4.1) - no statement of any kind ran in THIS transaction
	// before it. Redeem + Recheck is the first statement here (ADR 0094
	// §5): a revoked/expired/rotated-away handle, a reused or stale token,
	// or a tenant/provider/domain mismatch is the uniform
	// credential_unavailable with nothing read or written.
	provider, in, cred, err := o.redeemVerified(ctx, tx, tenantID, providerID, v)
	if err != nil {
		return Verification{}, false, err
	}
	keyID := cred.KeyID

	// (c) the adapter's HandleCallback parses the now-VERIFIED bytes (it
	// may re-verify as defence in depth). A re-verification failure is
	// ErrCallbackSignatureInvalid; every post-verification failure is
	// ErrCallbackMalformedBody - never confused with one another, since
	// that would break the uniform-401 contract (point 7).
	providerResult, err := provider.HandleCallback(ctx, in, cred)
	if errors.Is(err, ErrCallbackSignatureInvalid) {
		return Verification{}, false, &CallbackAuthError{Reason: webhookauth.ReasonSignatureInvalid, KeyID: keyID, CredentialFingerprint: cred.Fingerprint}
	}
	if err != nil {
		// Covers ErrCallbackMalformedBody and any other post-verification
		// structural failure. Never wrap in.Body's bytes into this error
		// (directive §17) - only reachable once verification has already
		// succeeded, so the sender is authenticated, just wrong.
		return Verification{}, false, err
	}

	// (d) explicit tenant-scoped lookup (architect R4/J6) - reachable only
	// by a VERIFIED caller.
	verification, err := getVerificationByProviderReference(ctx, tx, tenantID, providerID, providerResult.ProviderReference)
	if err != nil {
		return Verification{}, false, err
	}

	// (e) forward-only rank transition + audit, all in this same
	// transaction (B7/J6/J11).
	return applyCallbackOutcome(ctx, tx, tenantID, verification, providerResult)
}

// statusRank is the forward-only monotonic order B7/J11 require:
// unverified(0) < pending(1) < review_required(2) < terminal(3). A
// callback can only move a verification FORWARD in this order (a
// compare-and-set UPDATE guards this at the database level, updateVerification
// StatusCAS below) - it never resurrects a terminal verification, and it
// never moves a verification backward (e.g. review_required -> pending).
func statusRank(s VerificationStatus) int {
	switch s {
	case StatusUnverified:
		return 0
	case StatusPending:
		return 1
	case StatusReviewRequired:
		return 2
	case StatusApproved, StatusRejected, StatusExpired:
		return 3
	default:
		return -1
	}
}

func isTerminal(s VerificationStatus) bool {
	return s == StatusApproved || s == StatusRejected || s == StatusExpired
}

func statusForOutcome(o ProviderOutcome) (VerificationStatus, bool) {
	switch o {
	case ProviderApproved:
		return StatusApproved, true
	case ProviderRejected:
		return StatusRejected, true
	case ProviderPending:
		return StatusPending, true
	case ProviderReviewRequired:
		return StatusReviewRequired, true
	case ProviderExpired:
		return StatusExpired, true
	default:
		return "", false
	}
}

// applyCallbackOutcome is B7's forward-only replay/idempotency rule,
// applied to an already-verified, already-tenant-scoped-looked-up
// verification v. The returned bool reports whether this call actually
// wrote anything (K4/K5, Stage 10.2 final review) - the caller uses it only
// to decide whether to log an informational no-op line, never to change the
// response.
//
//   - outcome "error" against an ALREADY-TERMINAL verification (K5, L2/F-5,
//     Stage 10.2 final review): no state change and NO audit row - an
//     outcome=error callback is otherwise the one exception to "a replay
//     writes no audit row" (see the non-terminal case below), and closing
//     THAT exception for the terminal case keeps §E's "replay is inert"
//     true without bound: a captured error callback replayed after the
//     verification has already reached a terminal status must not be able
//     to grow audit_log without limit.
//   - outcome "error" against a NON-terminal verification: no state change,
//     but ONE failure audit row is written (a verified sender reporting a
//     provider-side failure is still an auditable event) - directive §10's
//     "an error must never corrupt platform state". This one still writes
//     an audit row per delivery (bounded by the verification's own life:
//     once it reaches a terminal status, the case above takes over and it
//     stops) - a disclosed, narrower exception than before, not a new one.
//   - rank(new) > rank(current): a compare-and-set UPDATE (tenant_id AND
//     id AND status=current in the WHERE clause) applies the transition
//     and writes exactly one success audit row, in this same transaction.
//     A lost race (another writer applied a DIFFERENT, also-forward
//     transition first) re-reads and re-evaluates, up to 3 times total,
//     then returns an error rather than looping forever.
//   - rank(new) <= rank(current) - including every replay of an
//     already-applied transition, anything arriving after a terminal
//     status (J11: a callback never resurrects a terminal verification -
//     only a new CreateVerification row starts a new attempt), and
//     anything arriving after a staff decision that already reached or
//     exceeded that rank: no state change and NO audit row (a 204 no-op
//     at the HTTP layer).
func applyCallbackOutcome(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, v Verification, result ProviderResult) (Verification, bool, error) {
	// Security S-5: the platform bounds the adapter's reason itself before
	// either audit row or the status UPDATE below (idempotent over an
	// adapter that already normalized in HandleCallback).
	result, reasonTruncated := normalizeProviderResult(result)
	if result.Outcome == ProviderError {
		if isTerminal(v.Status) {
			// K5: the verification already reached a terminal status -
			// never write a second (or Nth) failure audit row for a
			// replayed or late-arriving error callback against it.
			return v, false, nil
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem,
			Action: "kyc.provider_callback", TargetType: "kyc_verification", TargetID: v.ID.String(),
			Outcome:  audit.OutcomeFailure,
			Metadata: withReasonTruncated(map[string]any{"provider_id": v.ProviderID, "provider_outcome": string(result.Outcome), "reason": result.Reason}, reasonTruncated),
		}); err != nil {
			return Verification{}, false, fmt.Errorf("kyc: audit callback error outcome: %w", err)
		}
		return v, true, nil
	}

	newStatus, ok := statusForOutcome(result.Outcome)
	if !ok {
		return Verification{}, false, fmt.Errorf("%w: unrecognized outcome %q", ErrCallbackMalformedBody, result.Outcome)
	}
	newRank := statusRank(newStatus)

	current := v
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if newRank <= statusRank(current.Status) {
			// Equal or backward - a no-op, no audit row (B7). Covers every
			// replay, and every callback arriving after a terminal status
			// or after a staff decision that already reached/exceeded this
			// rank (J11).
			return current, false, nil
		}
		tag, err := tx.Exec(ctx,
			`UPDATE kyc_verifications SET status = $1, reason = NULLIF($2, ''), updated_at = now()
			 WHERE id = $3 AND tenant_id = $4 AND status = $5`,
			newStatus, result.Reason, current.ID, tenantID, current.Status,
		)
		if err != nil {
			return Verification{}, false, fmt.Errorf("kyc: update verification status: %w", err)
		}
		if tag.RowsAffected() == 1 {
			updated, err := GetVerificationByID(ctx, tx, current.ID)
			if err != nil {
				return Verification{}, false, err
			}
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem,
				Action: "kyc.provider_callback", TargetType: "kyc_verification", TargetID: current.ID.String(),
				Outcome:  audit.OutcomeSuccess,
				Metadata: withReasonTruncated(map[string]any{"provider_id": updated.ProviderID, "provider_outcome": string(result.Outcome), "reason": result.Reason}, reasonTruncated),
			}); err != nil {
				return Verification{}, false, fmt.Errorf("kyc: audit callback success: %w", err)
			}
			return updated, true, nil
		}
		// Lost race: re-read the row (RLS-scoped, mirroring
		// updateVerificationStatus's own post-update re-read) and
		// re-evaluate on the next iteration.
		reread, err := GetVerificationByID(ctx, tx, current.ID)
		if err != nil {
			return Verification{}, false, err
		}
		current = reread
	}
	return Verification{}, false, fmt.Errorf("kyc: exhausted retries applying callback status transition for verification %s", v.ID)
}
