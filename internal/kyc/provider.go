package kyc

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
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
type ProviderResult struct {
	ProviderReference string
	Outcome           ProviderOutcome
	Reason            string
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
	// HandleCallback parses a raw, provider-specific webhook payload
	// (never touched or logged by the platform's own HTTP layer -
	// internal/httpserver's webhook handler passes rawPayload straight
	// through) and returns the normalized result, including which
	// ProviderReference it concerns. Provider-specific callback
	// AUTHENTICATION (signature/shared-secret verification) happens
	// HERE, inside the adapter, before any payload field is trusted -
	// identical precedent to casino.CasinoProvider.HandleCallback (ADR
	// 0025 §5/§7).
	HandleCallback(ctx context.Context, rawPayload []byte) (ProviderResult, error)
	GetCapabilities() Capabilities
	HealthStatus(ctx context.Context) error
}

var (
	// ErrUnknownProvider is returned when a caller (an HTTP webhook route,
	// a verification-creation call) names a provider_id the Orchestrator
	// has no adapter registered for.
	ErrUnknownProvider = errors.New("kyc: unknown provider")
)

// Orchestrator holds the process-global provider registry, keyed by
// provider id - mirrors casino.Orchestrator/payments.Orchestrator
// exactly. A tenant's choice of WHICH provider to use for a given
// verification is out of this stage's scope (today: exactly one, the
// mock, is ever registered - see cmd/platform-api/main.go); a future
// per-tenant capability/routing model (mirroring ADR 0022/0025's
// ProviderCapability tables) is a documented extension point, not
// invented here without a real second provider to route between.
type Orchestrator struct {
	providers map[string]KYCProvider
}

func NewOrchestrator(providers map[string]KYCProvider) *Orchestrator {
	return &Orchestrator{providers: providers}
}

func (o *Orchestrator) Provider(id string) (KYCProvider, bool) {
	p, ok := o.providers[id]
	return p, ok
}

// ReceiveCallback dispatches rawPayload to the named provider, then
// applies its normalized result to the matching kyc_verifications row
// (looked up by provider_id+provider_reference within tx's OWN tenant
// scope - migration 0040's tenant_isolation policy is what actually
// prevents this from ever touching a different tenant's row). tx must
// already be tenant-scoped (db.WithTenant) by the caller, which resolves
// the tenant from the webhook URL's tenant slug exactly like
// casino.Orchestrator.ReceiveCallback/payments.Orchestrator.
// ReceiveCallback already do (ADR 0025/0022) - a webhook payload itself
// is never trusted to assert which tenant it belongs to.
//
// Idempotent: a callback for a verification already in a TERMINAL status
// (approved/rejected/expired) is a no-op, mirroring casino/payments'
// identical redelivery-safety precedent - never re-applies or reverses an
// already-final decision. ProviderError never changes status at all -
// see ProviderOutcome's own doc comment.
func (o *Orchestrator) ReceiveCallback(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, rawPayload []byte) (Verification, error) {
	provider, ok := o.providers[providerID]
	if !ok {
		return Verification{}, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}

	result, err := provider.HandleCallback(ctx, rawPayload)
	if err != nil {
		// Never wrap rawPayload's bytes into this error (directive §17).
		return Verification{}, fmt.Errorf("kyc: handle callback: %w", err)
	}

	v, err := getVerificationByProviderReference(ctx, tx, providerID, result.ProviderReference)
	if err != nil {
		return Verification{}, err
	}

	auditOutcome := audit.OutcomeSuccess
	if result.Outcome == ProviderError {
		auditOutcome = audit.OutcomeFailure
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem,
		Action: "kyc.provider_callback", TargetType: "kyc_verification", TargetID: v.ID.String(),
		Outcome:  auditOutcome,
		Metadata: map[string]any{"provider_id": providerID, "provider_outcome": string(result.Outcome), "reason": result.Reason},
	}); err != nil {
		return Verification{}, fmt.Errorf("kyc: audit callback: %w", err)
	}

	if result.Outcome == ProviderError {
		return v, nil
	}
	if isTerminal(v.Status) {
		// Redelivery of a callback for an already-final verification -
		// idempotent no-op, never re-applied.
		return v, nil
	}

	newStatus, ok := statusForOutcome(result.Outcome)
	if !ok {
		return Verification{}, fmt.Errorf("kyc: provider returned an unrecognized outcome %q", result.Outcome)
	}
	return updateVerificationStatus(ctx, tx, v.ID, newStatus, result.Reason)
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
