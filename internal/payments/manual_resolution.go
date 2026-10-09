// PRH-2 K3 (ADR 0101 revision 4 + section 26): the governed payment
// force-resolution service. M1 records evidence on a disputed DEPOSIT and
// never changes it; M2 declares an ambiguous / allow-listed-disputed PAYOUT
// paid or not paid (withdrawal.Complete / withdrawal.Fail, in the final
// approval's own transaction). Status: IMPLEMENTED against the MOCK stack;
// every real PSP interaction is PROVIDER DEPENDENT.
//
// The authority decisions live in migration 0115's triggers and SQL functions
// (the forced actor, the in-force capability grant, the distinct-Person floor,
// the S-12 beneficiary exclusion, S-2(iii), the closed-tenant actor scope, the
// pinned factual basis, the DB-side recount, the state machine, the
// executing-only ledger fences, the reserved provider-tx namespace). There is
// no second implementation of any of them here: this package builds the
// statements, takes the ADR 0082 Amendment A8 locks in order, calls the
// existing withdrawal and ledger writers, classifies errors by SQLSTATE and
// writes the audit rows. The only Go-side restatement is the closed M2
// allow-list (M2ResolvableDispute), pinned equal to the database's by the C-47
// parity test, so the executor can end a stale resolution refused_at_execution
// instead of failing the transaction.
//
// It is the ONLY non-adjustment caller of db.WithPlatformActingInTenant (ADR
// 0099 6.1; the A-16 static test allows exactly this file), from exactly one
// call site (runSession), whose principal is parsed from the verified token in
// the same function and whose target tenant can only come from
// NewResolutionTarget (the canActOnTenant rule applied to the route's path
// value).
package payments

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// OperationKindForceResolve is the ADR 0100 2 classification key.
const OperationKindForceResolve = "payment_force_resolve"

// DeclaredNotPaidReason is the withdrawal.Fail reason code of an M2 "declare not
// paid" (recorded on the withdrawal.failed audit row).
const DeclaredNotPaidReason = "operator_declared_not_paid"

// Scope values forced by migration 0115 onto every actor column.
const (
	ResolutionScopeTenant         = "tenant"
	ResolutionScopePlatformActing = "platform_acting"
)

// ResolutionKind mirrors payment_manual_resolutions.kind.
type ResolutionKind string

const (
	ResolutionM1DepositEvidence ResolutionKind = "m1_deposit_evidence"
	ResolutionM2DeclarePaid     ResolutionKind = "m2_declare_paid"
	ResolutionM2DeclareNotPaid  ResolutionKind = "m2_declare_not_paid"
	// ResolutionM4EvidencePaid / ResolutionM4EvidenceNotPaid are the
	// evidence-backed resolutions of an UNBOUND payout park (ADR 0111 section 4,
	// PAY-PAYOUT-UNBOUND-RESOLVE-1; migration 0125). Capability:
	// payment_force_resolve (D-9). See manual_resolution_m4.go.
	ResolutionM4EvidencePaid    ResolutionKind = "m4_evidence_paid"
	ResolutionM4EvidenceNotPaid ResolutionKind = "m4_evidence_not_paid"
)

// ResolutionState mirrors payment_manual_resolutions.state.
type ResolutionState string

const (
	ResolutionPending           ResolutionState = "pending"
	ResolutionExecuting         ResolutionState = "executing"
	ResolutionExecuted          ResolutionState = "executed"
	ResolutionRejected          ResolutionState = "rejected"
	ResolutionCancelled         ResolutionState = "cancelled"
	ResolutionExpired           ResolutionState = "expired"
	ResolutionRefusedAtExecute  ResolutionState = "refused_at_execution"
	resolutionRefusedAttempt    string          = "attempt_changed"
	resolutionRefusedPrecond    string          = "precondition_failed"
	resolutionRefusedNoSource   string          = "no_statement_source"
	resolutionRefusedNotAllowed string          = "reason_not_resolvable"
	resolutionRefusedEvidence   string          = "evidence_changed"
	resolutionRefusedUnsealed   string          = "evidence_unsealed"
)

// The closed code vocabularies (ADR 0101 3, 5.1; LF ruling 1). The database's
// payment_manual_resolution_codes table is the authority; these are the Go
// input validators.
var (
	findingCodes = map[string]bool{"awaiting_psp_refund": true, "refund_requested_from_psp": true, "investigated_no_platform_action": true}
	basisCodes   = map[string]bool{"provider_confirmed_out_of_band": true, "reconciliation_exhausted": true}
	contextCodes = map[string]bool{"provider_unqueryable": true, "past_resubmission_horizon": true}
)

// BasisProviderConfirmedOutOfBand is the basis a "declare not paid" after a
// possible dispatch must carry (LF L-3).
const BasisProviderConfirmedOutOfBand = "provider_confirmed_out_of_band"

// M2ResolvableDispute is the closed literal allow-list of ADR 0101 5.1 (F9):
// an M2 admits an `ambiguous` payout, or a `disputed` one whose terminal reason
// is provider_reference_mismatch or success_for_never_sent_attempt. EVERY other
// payout reason is refused for both M2 kinds (amount_asset_mismatch and
// callback_amount_asset_mismatch go to PAYOUT-AMOUNT-DISPUTE-1; the invalid-
// reference parks keep their hold; the tombstone and late_* reasons have no
// path yet). NULL and unknown reasons are refused. The database restates the
// same set in payment_m2_admits and the resolution guard (C-47 pins parity).
func M2ResolvableDispute(state AttemptState, terminalReason *string) bool {
	switch state {
	case AttemptAmbiguous:
		return true
	case AttemptDisputed:
		if terminalReason == nil {
			return false
		}
		switch *terminalReason {
		case "provider_reference_mismatch", "success_for_never_sent_attempt":
			return true
		}
	}
	return false
}

// PayoutDisputeReasons is the single classification table of every payout
// dispute reason written at HEAD (C-5b pins that no payout dispute write site
// uses a reason that is not listed): true = M2-admitted, false = M2-refused.
// invalid_provider_reference:<reason> is expanded over the closed providerref
// reasons by the pin test.
func PayoutDisputeReasons() map[string]bool {
	return map[string]bool{
		"provider_reference_mismatch":         true,
		"success_for_never_sent_attempt":      true,
		"amount_asset_mismatch":               false,
		"callback_amount_asset_mismatch":      false,
		"invalid_provider_reference":          false,
		"provider_reference_conflict":         false, // PAY-PAYOUT-REFBIND-1: hold kept; no M2 path
		"reversal_tombstone_precedes_success": false,
		"late_success_after_terminal":         false,
		"late_decline_after_terminal":         false,
		"late_contradicting_evidence":         false,
		"success_after_payout_declined":       false, // T14: also excluded by the withdrawal-state precondition
		// B13-B (ADR 0111 2.6): a destination echo that differs from the snapshot, or an attempt
		// whose snapshot is missing or inconsistent when evidence tries to settle it. Hold kept;
		// no M2 path. M4 "not paid" is the only exceptional resolution for BOTH reasons
		// (RESOLVE-1 for destination_mismatch; migration 0127 / owner decision 4 for
		// destination_integrity_failure); M4 "paid" is refused for both.
		TerminalReasonDestinationMismatch:         false,
		TerminalReasonDestinationIntegrityFailure: false,
	}
}

// Sentinel errors for conditions this package detects itself.
var (
	ErrResolutionForeignTenant = errors.New("payments: caller may not act on this tenant")
	ErrResolutionNotFound      = errors.New("payments: manual resolution not found")
	ErrResolutionNotPending    = errors.New("payments: manual resolution is not pending")
	ErrResolutionExpired       = errors.New("payments: manual resolution expired")
	ErrResolutionInvalidInput  = errors.New("payments: invalid manual resolution input")
	ErrResolutionNoAuth        = errors.New("payments: no authenticated context")
	ErrResolutionIntegrity     = errors.New("payments: manual resolution integrity violation")
	// ErrResolutionNoStatementSource: the optional security/LF O-4 rule - an
	// m2_declare_not_paid is refused when no payment statement source is
	// registered for the provider, because its double-payout risk is detected
	// only by the payment_statement stream (ADR 0101 12.3).
	ErrResolutionNoStatementSource = errors.New("payments: no payment statement source is registered for the provider")
)

// StatementSourceRegistry is the in-process record of the providers that have
// a registered payment statement source (ADR 0101 12.3, LF O-4: decided from
// the process registry, never from payment_statement_imports). A nil registry
// registers nothing (fail closed).
type StatementSourceRegistry struct {
	mu        sync.RWMutex
	providers map[string]bool
}

// Register records that providerID has a statement source in this process.
func (r *StatementSourceRegistry) Register(providerID string) {
	if r == nil || providerID == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.providers == nil {
		r.providers = map[string]bool{}
	}
	r.providers[providerID] = true
}

// Registered reports whether providerID has a registered statement source.
func (r *StatementSourceRegistry) Registered(providerID string) bool {
	if r == nil {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.providers[providerID]
}

// There is deliberately NO package-level registry (PAY-K3-STATEMENT-SOURCE-
// WIRING-1): cmd/platform-api builds one, registers the providers of the SAME
// list it hands to the reconciliation scheduler, and injects it through
// httpserver.Deps.StatementSources. A nil/empty registry refuses every M2
// (fail closed).

// ResolutionTarget is a route-validated target tenant. Its only constructor
// applies the canActOnTenant rule (a tenant caller may name only its own
// tenant; a platform caller may name any tenant and then acts ONLY through an
// ADR 0099 6 acting session that the database refuses without an in-force
// grant for exactly this tenant).
type ResolutionTarget struct{ tenantID uuid.UUID }

// TenantID returns the validated target tenant.
func (t ResolutionTarget) TenantID() uuid.UUID { return t.tenantID }

// NewResolutionTarget validates pathTenantID (the route's {tenantID} path
// value, never a body or query field) against the caller's authenticated
// context.
func NewResolutionTarget(tc tenant.Context, pathTenantID uuid.UUID) (ResolutionTarget, error) {
	if pathTenantID == uuid.Nil {
		return ResolutionTarget{}, fmt.Errorf("%w: nil target tenant", ErrResolutionInvalidInput)
	}
	if tc.TenantID != uuid.Nil && tc.TenantID != pathTenantID {
		return ResolutionTarget{}, ErrResolutionForeignTenant
	}
	return ResolutionTarget{tenantID: pathTenantID}, nil
}

// ResolutionMeta is the non-authoritative request metadata recorded in audit rows.
type ResolutionMeta struct {
	IPAddress string
	UserAgent string
	RequestID string
}

// ResolutionCall identifies the actor of one governed call, derived from the
// verified token and the session actually opened - never from a body.
type ResolutionCall struct {
	ActorID  uuid.UUID
	TenantID uuid.UUID
	Scope    string
	Meta     ResolutionMeta
	// Proofs signs the SIGNED-ACTOR-PROOF (ADR 0110, migration 0120) for this
	// call's governed writes. nil means the process-wide actorproof.Default().
	Proofs *actorproof.Issuer
}

// attachProof signs and attaches the actor proof for ONE governed write, after
// runSession authenticated the principal (verified token subject). The database
// verifies it (migration 0120) and refuses the write without it. Any signing
// failure - a missing issuer or a claim set the database could never admit - is
// returned (a server-side defect, HTTP 500), never silently skipped.
func (c ResolutionCall) attachProof(ctx context.Context, tx pgx.Tx, operation, target, payloadHash string) error {
	iss := c.Proofs
	if iss == nil {
		iss = actorproof.Default()
	}
	return iss.Attach(ctx, tx, actorproof.Claims{
		Actor: c.ActorID, Scope: c.Scope, Tenant: c.TenantID,
		Operation: operation, Target: target, PayloadHash: payloadHash,
	})
}

// ManualResolution is one payment_manual_resolutions row.
type ManualResolution struct {
	ID                          uuid.UUID
	TenantID                    uuid.UUID
	AttemptID                   uuid.UUID
	Operation                   string
	Kind                        ResolutionKind
	TargetState                 *string
	FindingCode                 *string
	BasisCode                   *string
	ContextCode                 *string
	EvidenceRefHash             *string
	Amount                      int64
	AssetCode                   string
	BrandID                     uuid.UUID
	DepositIntentID             *uuid.UUID
	WithdrawalRequestID         *uuid.UUID
	ProviderID                  *string
	ReservedProviderTxID        *string
	ReasonCode                  string
	AttemptStateAtSubmission    string
	TerminalReasonAtSubmission  *string
	EverPossiblySentAtSubmit    bool
	PayloadHash                 string
	RequestedBy                 uuid.UUID
	RequestedByScope            string
	RequestedByPersonID         uuid.UUID
	TenantStatusAtSubmission    string
	TenantStatusAtExecution     *string
	RequiredAtSubmission        int
	State                       ResolutionState
	ExpiresAt                   time.Time
	ExecutedTxID                *int64
	LedgerTransactionID         *uuid.UUID
	RefusalCode                 *string
	CreatedAt                   time.Time
	ClosedAt                    *time.Time
	ContributingPolicyIDsAtSubm []uuid.UUID
	// M4 only (migration 0125; NULL on every other kind). All DB-forced except
	// EvidenceLineID, which must equal the database's deterministic line.
	EvidenceLineID                *uuid.UUID
	EvidenceReference             *string
	EvidenceVerdict               *string
	EvidenceImportIDs             []uuid.UUID
	ProviderReferenceAtSubmission *string
}

const resolutionColumns = `id, tenant_id, attempt_id, operation, kind, target_state, finding_code, basis_code, context_code,
	evidence_ref_hash, amount::text, asset_code, brand_id, deposit_intent_id, withdrawal_request_id, provider_id,
	reserved_provider_tx_id, reason_code, attempt_state_at_submission, terminal_reason_at_submission,
	ever_possibly_sent_at_submission, payload_hash, requested_by, requested_by_scope, requested_by_person_id,
	tenant_status_at_submission, tenant_status_at_execution, required_at_submission, state, expires_at, executed_txid,
	ledger_transaction_id, refusal_code, created_at, closed_at, contributing_policy_ids, ` + m4ResolutionColumns

func scanResolution(row pgx.Row, r *ManualResolution) error {
	var amount string
	if err := row.Scan(&r.ID, &r.TenantID, &r.AttemptID, &r.Operation, &r.Kind, &r.TargetState, &r.FindingCode, &r.BasisCode,
		&r.ContextCode, &r.EvidenceRefHash, &amount, &r.AssetCode, &r.BrandID, &r.DepositIntentID, &r.WithdrawalRequestID,
		&r.ProviderID, &r.ReservedProviderTxID, &r.ReasonCode, &r.AttemptStateAtSubmission, &r.TerminalReasonAtSubmission,
		&r.EverPossiblySentAtSubmit, &r.PayloadHash, &r.RequestedBy, &r.RequestedByScope, &r.RequestedByPersonID,
		&r.TenantStatusAtSubmission, &r.TenantStatusAtExecution, &r.RequiredAtSubmission, &r.State, &r.ExpiresAt, &r.ExecutedTxID,
		&r.LedgerTransactionID, &r.RefusalCode, &r.CreatedAt, &r.ClosedAt, &r.ContributingPolicyIDsAtSubm,
		&r.EvidenceLineID, &r.EvidenceReference, &r.EvidenceVerdict, &r.EvidenceImportIDs, &r.ProviderReferenceAtSubmission); err != nil {
		return err
	}
	n, ok := new(big.Int).SetString(amount, 10)
	if !ok || !n.IsInt64() {
		return fmt.Errorf("%w: amount %q is not an int64", ErrResolutionIntegrity, amount)
	}
	r.Amount = n.Int64()
	return nil
}

var resolutionHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ResolutionRequestInput is everything a caller supplies. Every actor, derived,
// pinned and hash column is forced by migration 0115 - there is no field for
// any of them.
type ResolutionRequestInput struct {
	AttemptID uuid.UUID
	Kind      ResolutionKind
	// FindingCode is M1's finding (awaiting_psp_refund, ...).
	FindingCode string
	// BasisCode / ContextCode are M2's basis and optional secondary context.
	BasisCode   string
	ContextCode string
	// EvidenceRefHash is the lowercase hex SHA-256 of an external evidence
	// reference (no PII ever reaches the platform). Required for M2 (C-101-2),
	// optional for M1.
	EvidenceRefHash string
	ReasonCode      string
	// Note is bounded free text kept on the audit row only.
	Note string
	// EvidenceLineID (M4 only) is the statement line the requester reviewed;
	// the database recomputes the deterministic line and refuses a different one
	// (force_resolve_evidence_mismatch).
	EvidenceLineID uuid.UUID
}

func (in ResolutionRequestInput) validate() error {
	if in.AttemptID == uuid.Nil {
		return fmt.Errorf("%w: attempt_id is required", ErrResolutionInvalidInput)
	}
	if len(in.ReasonCode) < 1 || len(in.ReasonCode) > 64 {
		return fmt.Errorf("%w: reason_code must be 1-64 bytes", ErrResolutionInvalidInput)
	}
	if in.EvidenceRefHash != "" && !resolutionHex64.MatchString(in.EvidenceRefHash) {
		return fmt.Errorf("%w: evidence_ref_hash must be 64 lowercase hex characters", ErrResolutionInvalidInput)
	}
	if len(in.Note) > 1000 {
		return fmt.Errorf("%w: note must be at most 1000 bytes", ErrResolutionInvalidInput)
	}
	switch in.Kind {
	case ResolutionM1DepositEvidence:
		if !findingCodes[in.FindingCode] {
			return fmt.Errorf("%w: finding_code is not in the closed set", ErrResolutionInvalidInput)
		}
		if in.BasisCode != "" || in.ContextCode != "" || in.EvidenceLineID != uuid.Nil {
			return fmt.Errorf("%w: M1 carries a finding only", ErrResolutionInvalidInput)
		}
	case ResolutionM2DeclarePaid, ResolutionM2DeclareNotPaid:
		if !basisCodes[in.BasisCode] {
			return fmt.Errorf("%w: basis_code is not in the closed set", ErrResolutionInvalidInput)
		}
		if in.ContextCode != "" && !contextCodes[in.ContextCode] {
			return fmt.Errorf("%w: context_code is not in the closed set", ErrResolutionInvalidInput)
		}
		if in.FindingCode != "" {
			return fmt.Errorf("%w: M2 carries a basis, not a finding", ErrResolutionInvalidInput)
		}
		if in.EvidenceRefHash == "" {
			return fmt.Errorf("%w: evidence_ref_hash is required for M2", ErrResolutionInvalidInput)
		}
		if in.EvidenceLineID != uuid.Nil {
			return fmt.Errorf("%w: M2 carries no evidence line", ErrResolutionInvalidInput)
		}
	case ResolutionM4EvidencePaid, ResolutionM4EvidenceNotPaid:
		return in.validateM4()
	default:
		return fmt.Errorf("%w: unknown kind", ErrResolutionInvalidInput)
	}
	return nil
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// ResolutionDecision is an approval decision.
type ResolutionDecision string

const (
	ResolutionApprove ResolutionDecision = "approve"
	ResolutionReject  ResolutionDecision = "reject"
)

// ResolutionDecisionInput is what a decider supplies: the payload hash it
// reviewed (an approval pins exactly the payload it saw) and a reason code.
type ResolutionDecisionInput struct {
	Decision    ResolutionDecision
	PayloadHash string
	ReasonCode  string
}

// ResolutionOutcome is the result of a decision.
type ResolutionOutcome struct {
	Resolution ManualResolution
	ApprovalID uuid.UUID
	// Executed is true when this decision was the final approval and the
	// resolution executed in its own transaction.
	Executed bool
	// Refused is true when the final approval found the preconditions no longer
	// held and the resolution ended refused_at_execution (committed, audited).
	Refused  bool
	Counted  int
	Required int
	// Expired is true when the resolution was found past expires_at: the
	// pending -> expired transition and its audit row are committed, no
	// decision is recorded, and Decide returns ErrResolutionExpired.
	Expired bool
}

// ManualResolutionService runs governed calls against a pool.
type ManualResolutionService struct {
	pool    *db.Pool
	sources *StatementSourceRegistry
	proofs  *actorproof.Issuer
	// importKeys verifies the statement-import seals an M4 rests on (ADR 0111
	// 4.3). nil refuses every M4 (force_resolve_evidence_unsealed).
	importKeys *payoutinstrument.Keys
}

// WithProofIssuer sets the issuer this service signs actor proofs with (default:
// the process-wide actorproof.Default()).
func (s *ManualResolutionService) WithProofIssuer(i *actorproof.Issuer) *ManualResolutionService {
	s.proofs = i
	return s
}

// NewManualResolutionService returns a service over pool. sources is the
// statement-source registry the not-paid refusal consults (nil refuses every
// m2_declare_not_paid: fail closed).
func NewManualResolutionService(pool *db.Pool, sources *StatementSourceRegistry) *ManualResolutionService {
	return &ManualResolutionService{pool: pool, sources: sources}
}

// runSession opens the ONE session shape migration 0115 admits for the caller:
// a tenant caller gets db.WithPrincipalScope (family T); a platform caller gets
// db.WithPlatformActingInTenant (family A, ADR 0099 6.1) for the target's
// tenant. The plain platform session has no policy on the resolution tables
// (HD-PRH2-6), so it is never used.
//
// A-16 / K2-G4 call-site pin: principalID is `subject`, parsed from
// tenant.FromContext(ctx).Subject in THIS function; the target tenant is
// `target.tenantID`, set only by NewResolutionTarget.
func (s *ManualResolutionService) runSession(ctx context.Context, target ResolutionTarget, resolutionID uuid.UUID, meta ResolutionMeta, fn func(ctx context.Context, tx pgx.Tx, call ResolutionCall) error) error {
	tc, err := tenant.FromContext(ctx)
	if err != nil {
		return ErrResolutionNoAuth
	}
	subject, err := uuid.Parse(tc.Subject)
	if err != nil || subject == uuid.Nil {
		return ErrResolutionNoAuth
	}
	if target.tenantID == uuid.Nil {
		return fmt.Errorf("%w: unvalidated target", ErrResolutionInvalidInput)
	}
	if tc.TenantID == uuid.Nil {
		return s.pool.WithPlatformActingInTenant(ctx, subject, target.tenantID, resolutionID, OperationKindForceResolve, func(ctx context.Context, tx pgx.Tx) error {
			return fn(ctx, tx, ResolutionCall{ActorID: subject, TenantID: target.tenantID, Scope: ResolutionScopePlatformActing, Meta: meta, Proofs: s.proofs})
		})
	}
	if tc.TenantID != target.tenantID {
		return ErrResolutionForeignTenant
	}
	return s.pool.WithPrincipalScope(ctx, tc.TenantID, subject, func(ctx context.Context, tx pgx.Tx) error {
		return fn(ctx, tx, ResolutionCall{ActorID: subject, TenantID: tc.TenantID, Scope: ResolutionScopeTenant, Meta: meta, Proofs: s.proofs})
	})
}

// --- request -----------------------------------------------------------------

// Request opens the caller's session and submits a resolution.
func (s *ManualResolutionService) Request(ctx context.Context, target ResolutionTarget, in ResolutionRequestInput, meta ResolutionMeta) (ManualResolution, error) {
	if err := in.validate(); err != nil {
		return ManualResolution{}, err
	}
	var out ManualResolution
	err := s.runSession(ctx, target, uuid.Nil, meta, func(ctx context.Context, tx pgx.Tx, call ResolutionCall) error {
		var err error
		out, err = s.requestInTx(ctx, tx, call, in)
		return err
	})
	return out, err
}

func (s *ManualResolutionService) requestInTx(ctx context.Context, tx pgx.Tx, call ResolutionCall, in ResolutionRequestInput) (ManualResolution, error) {
	if err := in.validate(); err != nil {
		return ManualResolution{}, err
	}
	// Every NOT NULL column that migration 0115's guard forces is given a
	// placeholder here (K2's pattern): the BEFORE INSERT trigger overwrites it.
	// SIGNED-ACTOR-PROOF (migration 0120): the id is server-forced by the guard,
	// so the target is 'new'; the digest binds the caller-supplied payload (the
	// same digest the zz_actor_proof_guard trigger recomputes from the row).
	if err := call.attachProof(ctx, tx, actorproof.OpResolutionRequest, actorproof.TargetNew, actorproof.Digest(
		actorproof.S(call.TenantID.String()), actorproof.S(in.AttemptID.String()), actorproof.S(string(in.Kind)),
		actorproof.SP(in.FindingCode), actorproof.SP(in.BasisCode), actorproof.SP(in.ContextCode),
		actorproof.SP(in.EvidenceRefHash), actorproof.S(in.ReasonCode), actorproof.SP(evidenceLineText(in.EvidenceLineID)))); err != nil {
		return ManualResolution{}, err
	}
	// M4 (migration 0125, R-4): the amount and asset are NULL here - the guard
	// refuses any client value and copies the attempt's - and the requested
	// evidence line is supplied. M1/M2 keep the pre-0125 statement (so the
	// service also runs against a pre-0125 schema).
	var r ManualResolution
	insert := `
		INSERT INTO payment_manual_resolutions
			(tenant_id, attempt_id, operation, kind, finding_code, basis_code, context_code, evidence_ref_hash,
			 amount, asset_code, brand_id, reason_code, attempt_state_at_submission, ever_possibly_sent_at_submission,
			 payload_hash, requested_by, requested_by_scope, requested_by_person_id, tenant_status_at_submission,
			 required_at_submission, contributing_policy_ids, expires_at)
		VALUES ($1, $2, 'deposit', $3, $4, $5, $6, $7, 1, '-', $8, $9, '-', false, '-', $8, 'tenant', $8, '-', 1, '{}', now())
		RETURNING ` + resolutionColumns
	args := []any{call.TenantID, in.AttemptID, string(in.Kind), nilIfEmpty(in.FindingCode), nilIfEmpty(in.BasisCode),
		nilIfEmpty(in.ContextCode), nilIfEmpty(in.EvidenceRefHash), uuid.Nil, in.ReasonCode}
	if in.Kind.IsM4() {
		insert = `
		INSERT INTO payment_manual_resolutions
			(tenant_id, attempt_id, operation, kind, finding_code, basis_code, context_code, evidence_ref_hash,
			 amount, asset_code, brand_id, reason_code, attempt_state_at_submission, ever_possibly_sent_at_submission,
			 payload_hash, requested_by, requested_by_scope, requested_by_person_id, tenant_status_at_submission,
			 required_at_submission, contributing_policy_ids, expires_at, evidence_line_id)
		VALUES ($1, $2, 'deposit', $3, $4, $5, $6, $7, NULL, NULL, $8, $9, '-', false, '-', $8, 'tenant', $8, '-', 1, '{}', now(), $10)
		RETURNING ` + resolutionColumns
		args = append(args, in.EvidenceLineID)
	}
	row := tx.QueryRow(ctx, insert, args...)
	if err := scanResolution(row, &r); err != nil {
		return ManualResolution{}, err
	}
	if in.Kind == ResolutionM2DeclareNotPaid || in.Kind == ResolutionM2DeclarePaid || in.Kind.IsM4() {
		// LF O-4 + PAY-K3-STATEMENT-SOURCE-WIRING-1: BOTH M2 kinds are refused at
		// submission unless a statement source is registered for THIS attempt's
		// provider (decided from the process registry). Fail closed in shape: an
		// unreadable attempt, a missing provider id or an unregistered provider all
		// refuse; only a readable attempt with a registered provider proceeds.
		//
		// Ordering (security F-2/F-3): this runs AFTER the INSERT, so migration
		// 0115's guard authorises and validates first (a requester without an
		// in-force grant gets the audited 403 before any attempt-existence or
		// provider-monitoring signal, and an unknown or foreign attempt gets the
		// guard's audited refusal exactly as M1 does). The refusal rolls the
		// uncommitted INSERT back with the transaction.
		att, err := GetAttemptByID(ctx, tx, in.AttemptID)
		if err != nil {
			return ManualResolution{}, fmt.Errorf("payments: manual resolution source check: %w", err)
		}
		if !s.statementSourceRegistered(att.ProviderID) {
			return ManualResolution{}, ErrResolutionNoStatementSource
		}
	}
	extra := map[string]any{"note": in.Note, "required_at_submission": r.RequiredAtSubmission}
	if r.Kind.IsM4() {
		// ADR 0111 4.3: before this request can commit, every import the
		// database's verdict read must carry a seal that verifies over its stored
		// lines (the key never enters the database).
		if err := s.verifyImportSeals(ctx, tx, r.TenantID, r.EvidenceImportIDs); err != nil {
			return ManualResolution{}, err
		}
		extra["import_seals_verified"] = len(r.EvidenceImportIDs)
	}
	if err := recordResolutionAudit(ctx, tx, call, "payment.manual_resolution_requested", r, nil, extra); err != nil {
		return ManualResolution{}, err
	}
	return r, nil
}

// --- reads -------------------------------------------------------------------

// GetResolutionInTx reads one resolution visible to tx's session.
func GetResolutionInTx(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (ManualResolution, error) {
	var r ManualResolution
	err := scanResolution(tx.QueryRow(ctx, `SELECT `+resolutionColumns+` FROM payment_manual_resolutions WHERE id = $1 AND tenant_id = $2`, id, tenantID), &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualResolution{}, ErrResolutionNotFound
	}
	return r, err
}

// Get opens the caller's session and reads one resolution.
func (s *ManualResolutionService) Get(ctx context.Context, target ResolutionTarget, id uuid.UUID, meta ResolutionMeta) (ManualResolution, error) {
	var out ManualResolution
	err := s.runSession(ctx, target, id, meta, func(ctx context.Context, tx pgx.Tx, call ResolutionCall) error {
		var err error
		out, err = GetResolutionInTx(ctx, tx, call.TenantID, id)
		return err
	})
	return out, err
}

// List opens the caller's session and lists a tenant's resolutions, newest
// first (bounded).
func (s *ManualResolutionService) List(ctx context.Context, target ResolutionTarget, limit int, meta ResolutionMeta) ([]ManualResolution, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []ManualResolution
	err := s.runSession(ctx, target, uuid.Nil, meta, func(ctx context.Context, tx pgx.Tx, call ResolutionCall) error {
		rows, err := tx.Query(ctx, `SELECT `+resolutionColumns+` FROM payment_manual_resolutions WHERE tenant_id = $1 ORDER BY created_at DESC, id LIMIT $2`,
			call.TenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r ManualResolution
			if err := scanResolution(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// --- cancel ------------------------------------------------------------------

// Cancel opens the caller's session and cancels a pending resolution
// (requester only, trigger-enforced).
func (s *ManualResolutionService) Cancel(ctx context.Context, target ResolutionTarget, id uuid.UUID, meta ResolutionMeta) (ManualResolution, error) {
	var out ManualResolution
	err := s.runSession(ctx, target, id, meta, func(ctx context.Context, tx pgx.Tx, call ResolutionCall) error {
		before, err := lockResolution(ctx, tx, call.TenantID, id)
		if err != nil {
			return err
		}
		if before.State != ResolutionPending {
			return ErrResolutionNotPending
		}
		if err := call.attachProof(ctx, tx, actorproof.OpResolutionCancel, id.String(), before.PayloadHash); err != nil {
			return err
		}
		out, err = setResolutionState(ctx, tx, id, ResolutionCancelled, nil)
		if err != nil {
			return err
		}
		st := before.State
		return recordResolutionAudit(ctx, tx, call, "payment.manual_resolution_cancelled", out, &st, nil)
	})
	return out, err
}

func lockResolution(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID) (ManualResolution, error) {
	var r ManualResolution
	err := scanResolution(tx.QueryRow(ctx, `SELECT `+resolutionColumns+` FROM payment_manual_resolutions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, id, tenantID), &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return ManualResolution{}, ErrResolutionNotFound
	}
	return r, err
}

// setResolutionState moves the resolution's state; the migration 0115 guard
// decides whether the transition is legal (and forces executed_txid itself).
func setResolutionState(ctx context.Context, tx pgx.Tx, id uuid.UUID, to ResolutionState, refusalCode *string) (ManualResolution, error) {
	var r ManualResolution
	err := scanResolution(tx.QueryRow(ctx, `UPDATE payment_manual_resolutions SET state = $2, refusal_code = $3 WHERE id = $1 RETURNING `+resolutionColumns,
		id, string(to), refusalCode), &r)
	return r, err
}

// --- decide + execute ----------------------------------------------------------

// resolutionExecStatus is migration 0115's
// payment_manual_resolution_execution_status(): the ONE counting
// implementation, shared with the -> executing guard (K3-S2).
type resolutionExecStatus struct {
	Required       int
	Counted        int
	CountedIDs     []uuid.UUID
	RequesterValid bool
	Contributing   []uuid.UUID
	TenantStatus   *string
	Enabled        bool
	// PlatformFloorMet: ADR 0111 S-6 - an M4 needs at least one counted
	// platform_acting approval (computed by the database; true for other kinds).
	PlatformFloorMet bool
}

func readResolutionExecStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID) (resolutionExecStatus, error) {
	var st resolutionExecStatus
	// platform_floor_met is read through to_jsonb so the service also runs on a
	// pre-0125 schema, where no M4 row can exist (the kind CHECK) and the floor
	// is therefore vacuous; on 0125 the function never returns NULL.
	var floor *bool
	err := tx.QueryRow(ctx, `SELECT s.required, s.counted, s.counted_approval_ids, s.requester_valid, s.contributing_policy_ids, s.tenant_status, s.enabled,
		       (to_jsonb(s) ->> 'platform_floor_met')::boolean
		FROM payment_manual_resolution_execution_status($1) s`, id).
		Scan(&st.Required, &st.Counted, &st.CountedIDs, &st.RequesterValid, &st.Contributing, &st.TenantStatus, &st.Enabled, &floor)
	st.PlatformFloorMet = floor == nil || *floor
	return st, err
}

// testHookResolutionAfterShareLocks, when set by an in-package test, runs after
// the L1 staff/grant FOR SHARE locks are held and the recount is done - so
// TestK3_Y07 races a revoke and a staff suspension against a real execution.
var testHookResolutionAfterShareLocks func(ctx context.Context, id uuid.UUID)

// testHookResolutionBeforePost, when set by an in-package test, runs after the
// resolution is 'executing' and the attempt is updated, just before the
// withdrawal posting - C-13's failure-injection point.
var testHookResolutionBeforePost func(ctx context.Context, id uuid.UUID) error

// Decide opens the caller's session and decides.
func (s *ManualResolutionService) Decide(ctx context.Context, target ResolutionTarget, id uuid.UUID, in ResolutionDecisionInput, meta ResolutionMeta) (ResolutionOutcome, error) {
	var out ResolutionOutcome
	err := s.runSession(ctx, target, id, meta, func(ctx context.Context, tx pgx.Tx, call ResolutionCall) error {
		var err error
		out, err = s.decideInTx(ctx, tx, call, id, in)
		return err
	})
	if err == nil && out.Expired {
		return out, ErrResolutionExpired
	}
	return out, err
}

// decideInTx records a decision and, when it is the final approval that brings
// the counted approvals to the required number, executes the resolution in THIS
// transaction (ADR 0101 6.3; no approved-but-unexecuted window). Lock order is
// ADR 0082 Amendment A8 as applied by ADR 0101 6.3 / LF ruling 6:
// parent (L1: withdrawal for M2, deposit intent for M1) -> attempt (L1) ->
// resolution (L1, FOR UPDATE) -> staff, grants (L1, FOR SHARE, ascending id) ->
// projections (L3, inside ledger.Post) -> ledger rows (L4). The approval insert
// happens while the L1 resolution row is held and belongs to no lock class.
func (s *ManualResolutionService) decideInTx(ctx context.Context, tx pgx.Tx, call ResolutionCall, id uuid.UUID, in ResolutionDecisionInput) (ResolutionOutcome, error) {
	if in.Decision != ResolutionApprove && in.Decision != ResolutionReject {
		return ResolutionOutcome{}, fmt.Errorf("%w: decision must be approve or reject", ErrResolutionInvalidInput)
	}
	if !resolutionHex64.MatchString(in.PayloadHash) {
		return ResolutionOutcome{}, fmt.Errorf("%w: payload_hash must be 64 lowercase hex characters", ErrResolutionInvalidInput)
	}
	if len(in.ReasonCode) < 1 || len(in.ReasonCode) > 64 {
		return ResolutionOutcome{}, fmt.Errorf("%w: reason_code must be 1-64 bytes", ErrResolutionInvalidInput)
	}

	// Steps 1-3: the locks, in order. The resolution is read unlocked first to
	// learn its attempt and parent (the lock order requires parent -> attempt ->
	// resolution), then locked and re-read.
	peek, err := GetResolutionInTx(ctx, tx, call.TenantID, id)
	if err != nil {
		return ResolutionOutcome{}, err
	}
	if err := lockResolutionParents(ctx, tx, call.TenantID, peek); err != nil {
		return ResolutionOutcome{}, err
	}
	res, err := lockResolution(ctx, tx, call.TenantID, id)
	if err != nil {
		return ResolutionOutcome{}, err
	}
	if res.State != ResolutionPending {
		return ResolutionOutcome{Resolution: res}, ErrResolutionNotPending
	}
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT now() >= $1::timestamptz`, res.ExpiresAt).Scan(&expired); err != nil {
		return ResolutionOutcome{}, err
	}
	if expired {
		after, err := setResolutionState(ctx, tx, id, ResolutionExpired, nil)
		if err != nil {
			return ResolutionOutcome{}, err
		}
		before := res.State
		return ResolutionOutcome{Resolution: after, Expired: true}, recordResolutionAudit(ctx, tx, call, "payment.manual_resolution_expired", after, &before, nil)
	}

	// Step 4: the approval insert (migration 0115 triggers: payload hash,
	// in-force grant, distinct Person, beneficiary, S-2(iii), closed-tenant
	// scope, pending).
	op := actorproof.OpResolutionApprove
	if in.Decision == ResolutionReject {
		op = actorproof.OpResolutionReject
	}
	if err := call.attachProof(ctx, tx, op, id.String(), in.PayloadHash); err != nil {
		return ResolutionOutcome{}, err
	}
	var approvalID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO payment_manual_resolution_approvals
			(tenant_id, resolution_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
		VALUES ($1, $2, $3, $4, $5, 'tenant', $5, 0, $6)
		RETURNING id`,
		call.TenantID, id, string(in.Decision), in.PayloadHash, uuid.Nil, in.ReasonCode).Scan(&approvalID); err != nil {
		return ResolutionOutcome{}, err
	}
	out := ResolutionOutcome{ApprovalID: approvalID}

	if in.Decision == ResolutionReject {
		after, err := GetResolutionInTx(ctx, tx, call.TenantID, id)
		if err != nil {
			return ResolutionOutcome{}, err
		}
		out.Resolution = after
		before := res.State
		return out, recordResolutionAudit(ctx, tx, call, "payment.manual_resolution_rejected", after, &before, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
		})
	}

	// Step 5: evaluate required and the candidate count.
	st, err := readResolutionExecStatus(ctx, tx, id)
	if err != nil {
		return ResolutionOutcome{}, err
	}
	out.Counted, out.Required = st.Counted, st.Required
	if !st.RequesterValid || st.Counted < st.Required || !st.PlatformFloorMet {
		out.Resolution = res
		return out, recordResolutionAudit(ctx, tx, call, "payment.manual_resolution_approved", res, nil, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
			"counted": st.Counted, "required": st.Required, "requester_valid": st.RequesterValid,
			"platform_floor_met": st.PlatformFloorMet,
		})
	}

	// Step 6: L1 continued - staff rows (requester + candidate approvers) then
	// their grants IN FORCE AT now(), both FOR SHARE, ascending id. Recount under
	// the locks.
	if err := lockResolutionStaffAndGrants(ctx, tx, res); err != nil {
		return ResolutionOutcome{}, err
	}
	st, err = readResolutionExecStatus(ctx, tx, id)
	if err != nil {
		return ResolutionOutcome{}, err
	}
	out.Counted, out.Required = st.Counted, st.Required
	if testHookResolutionAfterShareLocks != nil {
		testHookResolutionAfterShareLocks(ctx, id)
	}
	if !st.RequesterValid || st.Counted < st.Required || !st.PlatformFloorMet {
		out.Resolution = res
		return out, recordResolutionAudit(ctx, tx, call, "payment.manual_resolution_approved", res, nil, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
			"counted": st.Counted, "required": st.Required, "requester_valid": st.RequesterValid,
			"platform_floor_met": st.PlatformFloorMet,
		})
	}

	// Step 7: the executor's own re-check of the ADR 0101 5.1 preconditions and
	// the tenant status (the database re-runs the same checks at -> executing).
	att, err := GetAttemptByID(ctx, tx, res.AttemptID)
	if err != nil {
		return ResolutionOutcome{}, err
	}
	var wr withdrawal.WithdrawalRequest
	if res.WithdrawalRequestID != nil {
		wr, err = withdrawal.GetByID(ctx, tx, *res.WithdrawalRequestID)
		if err != nil {
			return ResolutionOutcome{}, err
		}
	}
	refusal := ""
	switch {
	case !st.Enabled:
		refusal = "policy_disabled"
	default:
		refusal = s.executionRefusal(res, att, wr)
	}
	if refusal == "" && res.Kind.IsM4() {
		// ADR 0111 4.5 steps 6-7 (S-2): re-evaluate the evidence FIRST, then
		// verify the seals of exactly the imports it returned.
		if refusal, err = s.m4EvidenceRefusal(ctx, tx, res); err != nil {
			return ResolutionOutcome{}, err
		}
	}
	if refusal != "" {
		code := refusal
		after, err := setResolutionState(ctx, tx, id, ResolutionRefusedAtExecute, &code)
		if err != nil {
			return ResolutionOutcome{}, err
		}
		out.Resolution = after
		out.Refused = true
		before := res.State
		return out, recordResolutionAudit(ctx, tx, call, "payment.manual_resolution_refused", after, &before, map[string]any{
			"approval_id": approvalID.String(), "counted_approval_ids": resolutionUUIDStrings(st.CountedIDs),
			"attempt_state": string(att.State),
		})
	}

	// Step 8: executing; executed_txid = txid_current() is forced by the guard,
	// which re-verifies the count and the preconditions.
	if _, err := setResolutionState(ctx, tx, id, ResolutionExecuting, nil); err != nil {
		return ResolutionOutcome{}, err
	}

	extra := map[string]any{
		"approval_id": approvalID.String(), "counted_approval_ids": resolutionUUIDStrings(st.CountedIDs),
		"required_at_execution": st.Required, "contributing_policy_ids_at_execution": resolutionUUIDStrings(st.Contributing),
		"attempt_state_before": string(att.State),
	}
	var ledgerTx *uuid.UUID
	if res.Kind.IsM4() {
		// Step 9 (M4): NO attempt update (A-15: the attempt stays disputed);
		// the withdrawal posting only, through the existing writers.
		if testHookResolutionBeforePost != nil {
			if err := testHookResolutionBeforePost(ctx, id); err != nil {
				return ResolutionOutcome{}, err
			}
		}
		var err error
		if ledgerTx, err = postM4(ctx, tx, res, wr); err != nil {
			return ResolutionOutcome{}, err
		}
		extra["withdrawal_request_id"] = wr.ID.String()
		extra["withdrawal_state_before"] = string(wr.State)
		wrAfter, err := withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return ResolutionOutcome{}, err
		}
		extra["withdrawal_state_after"] = string(wrAfter.State)
		extra["attempt_state_after"] = string(att.State)
	} else if res.Kind != ResolutionM1DepositEvidence {
		// Step 9 (M2 only): the attempt UPDATE (operator evidence, admitted by
		// payment_m2_admits), then Complete / Fail (L3/L4 inside ledger.Post).
		target := AttemptSucceeded
		if res.Kind == ResolutionM2DeclareNotPaid {
			target = AttemptDeclined
		}
		if err := applyOperatorResolution(ctx, tx, att, target); err != nil {
			return ResolutionOutcome{}, err
		}
		if testHookResolutionBeforePost != nil {
			if err := testHookResolutionBeforePost(ctx, id); err != nil {
				return ResolutionOutcome{}, err
			}
		}
		if res.Kind == ResolutionM2DeclarePaid {
			if res.ProviderID == nil || res.ReservedProviderTxID == nil {
				return ResolutionOutcome{}, fmt.Errorf("%w: declare-paid resolution has no provider or reserved id", ErrResolutionIntegrity)
			}
			if err := withdrawal.Complete(ctx, tx, wr.ID, *res.ProviderID, *res.ReservedProviderTxID); err != nil {
				return ResolutionOutcome{}, fmt.Errorf("payments: M2 declare paid: %w", err)
			}
		} else {
			if err := withdrawal.Fail(ctx, tx, wr.ID, DeclaredNotPaidReason); err != nil {
				return ResolutionOutcome{}, fmt.Errorf("payments: M2 declare not paid: %w", err)
			}
		}
		wrAfter, err := withdrawal.GetByID(ctx, tx, wr.ID)
		if err != nil {
			return ResolutionOutcome{}, err
		}
		if wrAfter.ReleaseLedgerTransactionID == nil {
			return ResolutionOutcome{}, fmt.Errorf("%w: withdrawal %s has no release transaction after the M2 posting", ErrResolutionIntegrity, wr.ID)
		}
		ledgerTx = wrAfter.ReleaseLedgerTransactionID
		extra["withdrawal_request_id"] = wr.ID.String()
		extra["withdrawal_state_before"] = string(wr.State)
		extra["withdrawal_state_after"] = string(wrAfter.State)
		attAfter, err := GetAttemptByID(ctx, tx, att.ID)
		if err != nil {
			return ResolutionOutcome{}, err
		}
		extra["attempt_state_after"] = string(attAfter.State)
		extra["attempt_evidence_kind_after"] = string(attAfter.LastEvidenceKind)
	} else {
		extra["attempt_state_after"] = string(att.State)
	}

	// Step 10: executed + link (the deferred trigger verifies the shape).
	var after ManualResolution
	if err := scanResolution(tx.QueryRow(ctx, `UPDATE payment_manual_resolutions SET state = 'executed', ledger_transaction_id = $2
		WHERE id = $1 RETURNING `+resolutionColumns, id, ledgerTx), &after); err != nil {
		return ResolutionOutcome{}, err
	}
	out.Resolution = after
	out.Executed = true
	before := res.State
	return out, recordResolutionAudit(ctx, tx, call, "payment.manual_resolution_executed", after, &before, extra)
}

// executionRefusal is the executor's Go-side restatement of the ADR 0101 5.1
// preconditions at execution time: "" when they all hold, else the closed
// refusal code the resolution ends refused_at_execution with.
func (s *ManualResolutionService) executionRefusal(res ManualResolution, att PaymentAttempt, wr withdrawal.WithdrawalRequest) string {
	// R-6: the factual basis the approvers saw must be the one that still holds.
	if string(att.State) != res.AttemptStateAtSubmission || !equalOptString(att.TerminalReason, res.TerminalReasonAtSubmission) {
		return resolutionRefusedAttempt
	}
	if res.Kind.IsM4() {
		return s.m4ExecutionRefusal(res, att, wr)
	}
	if res.Kind == ResolutionM1DepositEvidence {
		if att.Operation != AttemptOperationDeposit || att.State != AttemptDisputed {
			return resolutionRefusedPrecond
		}
		return ""
	}
	if att.Operation != AttemptOperationPayout || att.ProviderID == nil || res.ProviderID == nil || *att.ProviderID != *res.ProviderID {
		return resolutionRefusedPrecond
	}
	if !M2ResolvableDispute(att.State, att.TerminalReason) {
		return resolutionRefusedNotAllowed
	}
	if wr.State != withdrawal.StateSubmitted {
		return resolutionRefusedPrecond
	}
	if res.Kind == ResolutionM2DeclarePaid && (att.ProviderReference == nil || *att.ProviderReference == "") {
		return resolutionRefusedPrecond
	}
	if res.Kind == ResolutionM2DeclareNotPaid {
		if att.EverPossiblySent && (res.BasisCode == nil || *res.BasisCode != BasisProviderConfirmedOutOfBand) {
			return resolutionRefusedPrecond
		}
	}
	// PAY-K3-STATEMENT-SOURCE-WIRING-1: BOTH M2 kinds need a statement source
	// registered for this attempt's provider (the standing kinds
	// pay_declared_paid_unconfirmed and the (c)/(d) clearing exist only when that
	// provider's stream runs). Evaluated PER PROVIDER: the MOCK source unlocks
	// only the mock provider's attempts (orchestrator engineering ruling H-W2, reversible; PRH-2-ROUND2-ENGINEERING-RULINGS).
	if !s.statementSourceRegistered(att.ProviderID) {
		return resolutionRefusedNoSource
	}
	return ""
}

// statementSourceRegistered is true only for a non-nil, non-empty provider id
// that has a registered statement source. Anything else is false (fail closed).
func (s *ManualResolutionService) statementSourceRegistered(providerID *string) bool {
	if s == nil || providerID == nil || *providerID == "" {
		return false
	}
	return s.sources.Registered(*providerID)
}

func equalOptString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// applyOperatorResolution is the M2 attempt UPDATE: operator evidence, the
// target state, and ONLY the columns payment_attempts_operator_column_discipline
// allows (state, last_evidence_kind, resolved_at, next_action_at, updated_at).
// The reserved id is NEVER written to the attempt: it lives only in the ledger
// key.
func applyOperatorResolution(ctx context.Context, tx pgx.Tx, att PaymentAttempt, target AttemptState) error {
	return casUpdate(ctx, tx, "M2 operator resolution",
		`UPDATE payment_attempts
		    SET state = $2, last_evidence_kind = 'operator', resolved_at = now(), next_action_at = NULL, updated_at = now()
		  WHERE id = $1 AND state = $3 AND operation = 'payout'`,
		att.ID, string(target), string(att.State))
}

// lockResolutionParents takes the L1 parent lock and then the attempt lock, in
// ADR 0082 A8 order. M2 locks the withdrawal with no state precondition (a
// reject or a non-final approval of a resolution whose withdrawal already left
// `submitted` must still work; the executor re-checks the state under the
// lock); M1 locks the deposit intent.
func lockResolutionParents(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r ManualResolution) error {
	switch {
	case r.WithdrawalRequestID != nil:
		if _, err := withdrawal.LockForPayoutEvidence(ctx, tx, *r.WithdrawalRequestID); err != nil {
			return fmt.Errorf("payments: lock withdrawal for resolution: %w", err)
		}
	case r.DepositIntentID != nil:
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM deposit_intents WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, *r.DepositIntentID, tenantID).Scan(&locked); err != nil {
			return fmt.Errorf("payments: lock deposit intent for resolution: %w", err)
		}
	}
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE id = $1 AND tenant_id = $2 FOR UPDATE`, r.AttemptID, tenantID).Scan(&locked); err != nil {
		return fmt.Errorf("payments: lock attempt for resolution: %w", err)
	}
	return nil
}

// lockResolutionStaffAndGrants takes the A8 L1 FOR SHARE locks: staff_users rows
// of the requester and every candidate approver (ascending id), then their
// grants in force at now() (ascending id). Only id, tenant_id, role, status,
// person_id are ever selected from staff_users (A-19 column discipline). A
// concurrent revoke either committed first (and is re-read as revoked by the
// recount) or waits for this transaction.
func lockResolutionStaffAndGrants(ctx context.Context, tx pgx.Tx, r ManualResolution) error {
	var approvers []uuid.UUID
	rows, err := tx.Query(ctx, `SELECT decided_by FROM payment_manual_resolution_approvals
		WHERE resolution_id = $1 AND decision = 'approve' AND payload_hash = $2 ORDER BY decided_by`, r.ID, r.PayloadHash)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		approvers = append(approvers, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	staff := append([]uuid.UUID{r.RequestedBy}, approvers...)
	if err := drainRows(tx.Query(ctx, `SELECT id, tenant_id, role, status, person_id FROM staff_users
		WHERE id = ANY($1) ORDER BY id FOR SHARE`, staff)); err != nil {
		return fmt.Errorf("payments: lock staff rows: %w", err)
	}
	if err := drainRows(tx.Query(ctx, `SELECT id FROM staff_capability_grants
		WHERE tenant_id = $1 AND revoked_at IS NULL AND valid_from <= now() AND (valid_until IS NULL OR now() < valid_until)
		  AND ((grantee_staff_id = $2 AND capability = 'payment_force_resolve:request')
		    OR (grantee_staff_id = ANY($3) AND capability = 'payment_force_resolve:approve'))
		ORDER BY id FOR SHARE`, r.TenantID, r.RequestedBy, approvers)); err != nil {
		return fmt.Errorf("payments: lock grants: %w", err)
	}
	return nil
}

// drainRows reads every row of a locking query (so every row lock is taken)
// and discards the values.
func drainRows(rows pgx.Rows, err error) error {
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

func resolutionUUIDStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// recordResolutionAudit writes one audit_log row for a resolution transition in
// the same transaction (ADR 0101 11). Under an acting session migration 0112's
// audit_log_acting_actor trigger re-forces the actor and tenant anyway. The
// evidence hash is recorded, never a free-text evidence reference.
func recordResolutionAudit(ctx context.Context, tx pgx.Tx, call ResolutionCall, action string, r ManualResolution, before *ResolutionState, extra map[string]any) error {
	md := map[string]any{
		"actor_scope":                      call.Scope,
		"resolution_id":                    r.ID.String(),
		"attempt_id":                       r.AttemptID.String(),
		"operation":                        r.Operation,
		"kind":                             string(r.Kind),
		"asset_code":                       r.AssetCode,
		"amount_minor_units":               fmt.Sprint(r.Amount),
		"reason_code":                      r.ReasonCode,
		"payload_hash":                     r.PayloadHash,
		"after_state":                      string(r.State),
		"tenant_status_at_submit":          r.TenantStatusAtSubmission,
		"attempt_state_at_submit":          r.AttemptStateAtSubmission,
		"requested_by_scope":               r.RequestedByScope,
		"required_at_submission":           r.RequiredAtSubmission,
		"ever_possibly_sent_at_submission": r.EverPossiblySentAtSubmit,
	}
	if before != nil {
		md["before_state"] = string(*before)
	}
	if r.TargetState != nil {
		md["target_state"] = *r.TargetState
	}
	if r.FindingCode != nil {
		md["finding_code"] = *r.FindingCode
	}
	if r.BasisCode != nil {
		md["basis_code"] = *r.BasisCode
	}
	if r.ContextCode != nil {
		md["context_code"] = *r.ContextCode
	}
	if r.EvidenceRefHash != nil {
		md["evidence_ref_hash"] = *r.EvidenceRefHash
	}
	if r.TerminalReasonAtSubmission != nil {
		md["terminal_reason_at_submission"] = *r.TerminalReasonAtSubmission
	}
	if r.TenantStatusAtExecution != nil {
		md["tenant_status_at_execution"] = *r.TenantStatusAtExecution
	}
	if r.ProviderID != nil {
		md["provider_id"] = *r.ProviderID
	}
	if r.ReservedProviderTxID != nil {
		md["reserved_provider_tx_id"] = *r.ReservedProviderTxID
	}
	if r.WithdrawalRequestID != nil {
		md["withdrawal_request_id"] = r.WithdrawalRequestID.String()
	}
	if r.DepositIntentID != nil {
		md["deposit_intent_id"] = r.DepositIntentID.String()
	}
	if r.LedgerTransactionID != nil {
		md["ledger_transaction_id"] = r.LedgerTransactionID.String()
	}
	if r.RefusalCode != nil {
		md["refusal_code"] = *r.RefusalCode
	}
	if r.EvidenceLineID != nil {
		md["evidence_line_id"] = r.EvidenceLineID.String()
	}
	if r.EvidenceReference != nil {
		md["evidence_reference"] = *r.EvidenceReference
	}
	if r.EvidenceVerdict != nil {
		md["evidence_verdict"] = *r.EvidenceVerdict
	}
	if r.EvidenceImportIDs != nil {
		md["evidence_import_ids"] = resolutionUUIDStrings(r.EvidenceImportIDs)
	}
	if r.ProviderReferenceAtSubmission != nil {
		md["provider_reference_at_submission"] = *r.ProviderReferenceAtSubmission
	}
	for k, v := range extra {
		md[k] = v
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   call.TenantID,
		ActorType:  audit.ActorStaff,
		ActorID:    call.ActorID,
		Action:     action,
		TargetType: "payment_manual_resolution",
		TargetID:   r.ID.String(),
		Outcome:    audit.OutcomeSuccess,
		IPAddress:  call.Meta.IPAddress,
		UserAgent:  call.Meta.UserAgent,
		RequestID:  call.Meta.RequestID,
		Metadata:   md,
	})
}

// --- error classification --------------------------------------------------------

// ResolutionErrClass classifies a K3 error for the HTTP layer.
type ResolutionErrClass string

const (
	ResolutionErrNone          ResolutionErrClass = ""
	ResolutionErrNotFound      ResolutionErrClass = "not_found"
	ResolutionErrForbidden     ResolutionErrClass = "forbidden"
	ResolutionErrConflict      ResolutionErrClass = "conflict"
	ResolutionErrInvalid       ResolutionErrClass = "invalid"
	ResolutionErrDisabled      ResolutionErrClass = "disabled"
	ResolutionErrPrecondition  ResolutionErrClass = "precondition"
	ResolutionErrNotResolvable ResolutionErrClass = "reason_not_resolvable"
	ResolutionErrExpired       ResolutionErrClass = "expired"
	ResolutionErrRetryable     ResolutionErrClass = "retryable"
	ResolutionErrSession       ResolutionErrClass = "session_invalid"
	ResolutionErrOther         ResolutionErrClass = "other"
	// ADR 0111 4.7: the M4 evidence classes.
	ResolutionErrEvidenceInsufficient ResolutionErrClass = "evidence_insufficient"
	ResolutionErrEvidenceMismatch     ResolutionErrClass = "evidence_mismatch"
	ResolutionErrEvidenceOverflow     ResolutionErrClass = "evidence_overflow"
	ResolutionErrEvidenceUnsealed     ResolutionErrClass = "evidence_unsealed"
)

// ResolutionSQLState returns err's SQLSTATE, or "".
func ResolutionSQLState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// ClassifyResolutionError maps an error to a class, by sentinel or SQLSTATE
// only - never by message text.
func ClassifyResolutionError(err error) ResolutionErrClass {
	switch {
	case err == nil:
		return ResolutionErrNone
	case errors.Is(err, ErrResolutionNotFound), errors.Is(err, pgx.ErrNoRows):
		return ResolutionErrNotFound
	case errors.Is(err, ErrResolutionForeignTenant), errors.Is(err, ErrResolutionNoAuth):
		return ResolutionErrForbidden
	case errors.Is(err, ErrResolutionNotPending):
		return ResolutionErrConflict
	case errors.Is(err, ErrResolutionExpired):
		return ResolutionErrExpired
	case errors.Is(err, ErrResolutionInvalidInput):
		return ResolutionErrInvalid
	case errors.Is(err, ErrResolutionNoStatementSource):
		return ResolutionErrPrecondition
	case errors.Is(err, ErrResolutionEvidenceUnsealed):
		return ResolutionErrEvidenceUnsealed
	case errors.Is(err, withdrawal.ErrStateConflict), errors.Is(err, ErrAttemptStateConflict):
		return ResolutionErrConflict
	}
	code := ResolutionSQLState(err)
	switch {
	case code == "MR014":
		return ResolutionErrDisabled
	case code == "MR060":
		// ADR 0111 4.3: the session cannot see the whole evidence scope (a
		// tenant-staff session never can): an error, never a verdict.
		return ResolutionErrForbidden
	case code == "MR061":
		return ResolutionErrEvidenceMismatch
	case code == "MR062":
		return ResolutionErrEvidenceInsufficient
	case code == "MR063":
		return ResolutionErrEvidenceOverflow
	case code == "MR012":
		return ResolutionErrNotResolvable
	case code == "MR010", code == "MR040", code == "MR041", code == "MR050":
		return ResolutionErrPrecondition
	case code == "40001", code == "40P01":
		return ResolutionErrRetryable
	case code == "MR001", code == "MR003", code == "MR011", code == "MR032", code == "42501":
		return ResolutionErrForbidden
	case len(code) == 5 && code[:2] == "AP":
		// SIGNED-ACTOR-PROOF refusal (migration 0120): fail closed.
		return ResolutionErrForbidden
	case code == "CG001", code == "CG002", code == "CG020", code == "MR002":
		return ResolutionErrSession
	case code == "MR020":
		return ResolutionErrInvalid
	case len(code) == 5 && code[:2] == "MR", code == "CG030", code == "CG031", code == "23505":
		return ResolutionErrConflict
	case code == "23514", code == "22003", code == "23503":
		return ResolutionErrInvalid
	}
	return ResolutionErrOther
}

// The CLOSED token set of ADR 0101 24.9: the only error strings a client ever
// sees. No SQL text and no Person ids.
const (
	TokenForceResolveDisabled          = "force_resolve_disabled"
	TokenForceResolveNotPermitted      = "force_resolve_not_permitted"
	TokenForceResolvePreconditionFail  = "force_resolve_precondition_failed"
	TokenForceResolveReasonNotResolved = "force_resolve_reason_not_resolvable"
	TokenForceResolveConflict          = "force_resolve_conflict"
	TokenForceResolveExpired           = "force_resolve_expired"
	TokenForceResolveNotFound          = "force_resolve_not_found"
	// ADR 0111 4.7 (M4).
	TokenForceResolveEvidenceInsufficient = "force_resolve_evidence_insufficient"
	TokenForceResolveEvidenceMismatch     = "force_resolve_evidence_mismatch"
	TokenForceResolveEvidenceOverflow     = "force_resolve_evidence_overflow"
	TokenForceResolveEvidenceUnsealed     = "force_resolve_evidence_unsealed"
)

// ResolutionToken returns the closed token for a class, or "" for a class that
// maps to a validation (400) or an internal (500) response.
func ResolutionToken(class ResolutionErrClass) string {
	switch class {
	case ResolutionErrDisabled:
		return TokenForceResolveDisabled
	case ResolutionErrForbidden, ResolutionErrSession:
		return TokenForceResolveNotPermitted
	case ResolutionErrPrecondition:
		return TokenForceResolvePreconditionFail
	case ResolutionErrNotResolvable:
		return TokenForceResolveReasonNotResolved
	case ResolutionErrConflict, ResolutionErrRetryable:
		return TokenForceResolveConflict
	case ResolutionErrExpired:
		return TokenForceResolveExpired
	case ResolutionErrNotFound:
		return TokenForceResolveNotFound
	case ResolutionErrEvidenceInsufficient:
		return TokenForceResolveEvidenceInsufficient
	case ResolutionErrEvidenceMismatch:
		return TokenForceResolveEvidenceMismatch
	case ResolutionErrEvidenceOverflow:
		return TokenForceResolveEvidenceOverflow
	case ResolutionErrEvidenceUnsealed:
		return TokenForceResolveEvidenceUnsealed
	}
	return ""
}

// ReservedDeclaredTxID builds the reserved provider-tx id of a declare-paid
// resolution (providerref.ReservedOperatorPrefix + the resolution id), for
// tests and read models; the database forces the same value.
func ReservedDeclaredTxID(resolutionID uuid.UUID) string {
	return providerref.ReservedOperatorPrefix + resolutionID.String()
}
