package kyc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/kycpolicyproof"
)

// ErrTransactionScope is returned when tx is not a genuinely
// platform-admin-scoped transaction (db.Pool.WithPlatformAdmin) -
// mirrors jurisdiction.ErrTransactionScope exactly.
var ErrTransactionScope = errors.New("kyc: requires a platform-admin-scoped transaction (use db.Pool.WithPlatformAdmin)")

// ErrPolicyInvalidInput is returned for a structurally invalid policy
// write, caught before it ever reaches the database.
var ErrPolicyInvalidInput = errors.New("kyc: invalid enforcement policy input")

func assertPlatformAdminScope(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	var platformAdmin *uuid.UUID
	var scopedTenant *uuid.UUID
	var scopedPlayer *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid,
		        NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&platformAdmin, &scopedTenant, &scopedPlayer); err != nil {
		return uuid.Nil, fmt.Errorf("kyc: read platform admin scope: %w", err)
	}
	if platformAdmin == nil || scopedTenant != nil || scopedPlayer != nil {
		return uuid.Nil, ErrTransactionScope
	}
	return *platformAdmin, nil
}

// CreateEnforcementPolicyParams is CreateEnforcementPolicy's input. Every
// row is created 'draft' - migration 0100's INSERT policy refuses any
// other status, so activation is always a later, second write by a
// DIFFERENT platform-admin principal (four-eyes, security condition 3 /
// re-verification C3).
type CreateEnforcementPolicyParams struct {
	LicensingJurisdictionID uuid.UUID
	TriggerType             string // 'cumulative_deposit' | 'edd_amount' | 'registration_tier' | 'play'
	ThresholdMinorUnits     *string
	AssetCode               *string
	RequiredTier            *string
	PlayOperation           *string
	LegalReviewReference    string
	ReasonCode              string
	// Audit carries the request evidence (IP, user agent, request id) the admin route supplies; nil for library callers.
	Audit *AuditMeta
	// Proof signs the ADR 0110 proof the database requires for a creation (lic19, S4). Required: nil is refused before any write.
	Proof PolicyProof
}

// PolicyContent and PolicyTransition are what the proofs are bound to (defined in the leaf signer package).
type (
	PolicyContent    = kycpolicyproof.PolicyContent
	PolicyTransition = kycpolicyproof.PolicyTransition
)

// PolicyProof signs and attaches the ADR 0110 proof for one policy write (lic19, S4). kyc does not import the issuer: the
// implementation lives in internal/kycpolicyproof (a pinned signer owner). The actor is the platform-admin principal of tx.
type PolicyProof interface {
	AttachCreate(ctx context.Context, tx pgx.Tx, actor uuid.UUID, c PolicyContent) error
	AttachTransition(ctx context.Context, tx pgx.Tx, actor uuid.UUID, t PolicyTransition) error
}

// ErrPolicyProofRequired: a policy write was attempted without a proof signer. The database would refuse it anyway.
var ErrPolicyProofRequired = errors.New("kyc: a signed actor proof is required for a policy write (no proof signer supplied)")

// PolicyWrite carries the proof signer and the reason code of a transition.
type PolicyWrite struct {
	Proof      PolicyProof
	ReasonCode string
}

// AuditMeta is the request evidence an HTTP caller adds to the audit record of a policy write (CLAUDE.md: every mutating
// administrative action records the IP and a reason code). All fields are optional for non-HTTP callers.
type AuditMeta struct {
	ReasonCode string
	IPAddress  string
	UserAgent  string
	RequestID  string
	// Proof signs the proof of a status change (lic19, S4); ReasonCode above is the reason code the proof is bound to.
	Proof PolicyProof
}

// write is the proof-carrying view of the evidence for a transition.
func (m *AuditMeta) write() PolicyWrite {
	if m == nil {
		return PolicyWrite{}
	}
	return PolicyWrite{Proof: m.Proof, ReasonCode: m.ReasonCode}
}

// CreateEnforcementPolicy authors a new DRAFT kyc_enforcement_policies
// row and writes an audit.Record in the same transaction. tx MUST be a
// platform-admin-scoped transaction.
func CreateEnforcementPolicy(ctx context.Context, tx pgx.Tx, p CreateEnforcementPolicyParams) (uuid.UUID, error) {
	principal, err := assertPlatformAdminScope(ctx, tx)
	if err != nil {
		return uuid.Nil, err
	}
	if p.LicensingJurisdictionID == uuid.Nil {
		return uuid.Nil, fmt.Errorf("%w: licensing_jurisdiction_id is required", ErrPolicyInvalidInput)
	}
	if strings.TrimSpace(p.ReasonCode) == "" {
		return uuid.Nil, fmt.Errorf("%w: reason_code is required", ErrPolicyInvalidInput)
	}
	if p.Proof == nil {
		return uuid.Nil, ErrPolicyProofRequired
	}

	// The proof is attached right before the write it covers; the database trigger consumes it in this transaction.
	if err := p.Proof.AttachCreate(ctx, tx, principal, PolicyContent{LicensingJurisdictionID: p.LicensingJurisdictionID, TriggerType: p.TriggerType,
		ThresholdMinorUnits: p.ThresholdMinorUnits, AssetCode: p.AssetCode, RequiredTier: p.RequiredTier, PlayOperation: p.PlayOperation,
		LegalReviewReference: p.LegalReviewReference, ReasonCode: p.ReasonCode}); err != nil {
		return uuid.Nil, fmt.Errorf("kyc: attach proof: %w", err)
	}
	var id uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO kyc_enforcement_policies
			(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code,
			 required_tier, play_operation, legal_review_reference, reason_code,
			 created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, 'draft', $3, $4, $5, $6, NULLIF($7, ''), $8, 'staff', $9)
		RETURNING id`,
		p.LicensingJurisdictionID, p.TriggerType, p.ThresholdMinorUnits, p.AssetCode,
		p.RequiredTier, p.PlayOperation, p.LegalReviewReference, p.ReasonCode, principal,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("kyc: insert enforcement policy: %w", err)
	}

	entry := audit.Entry{
		TenantID:   uuid.Nil,
		ActorType:  audit.ActorStaff,
		ActorID:    principal,
		Action:     "kyc_enforcement_policy.created",
		TargetType: "kyc_enforcement_policy",
		TargetID:   id.String(),
		Outcome:    audit.OutcomeSuccess,
		Metadata: map[string]any{
			"licensing_jurisdiction_id": p.LicensingJurisdictionID.String(),
			"trigger_type":              p.TriggerType,
			"reason_code":               p.ReasonCode,
			// lic18 L6: the AFTER state of the row (what was authored), not only who and why.
			"after": policyAfterState(p.TriggerType, "draft", p.ThresholdMinorUnits, p.AssetCode, p.RequiredTier, p.PlayOperation, p.LegalReviewReference != "", nil),
		},
	}
	if m := p.Audit; m != nil {
		entry.IPAddress, entry.UserAgent, entry.RequestID = m.IPAddress, m.UserAgent, m.RequestID
	}
	if err := audit.Record(ctx, tx, entry); err != nil {
		return uuid.Nil, fmt.Errorf("kyc: audit: %w", err)
	}
	return id, nil
}

// transitionEnforcementPolicy is Activate/Withdraw's shared shape: an
// UPDATE ... WHERE id = $1 AND status = $2, relying entirely on migration
// 0100/0103's lifecycle trigger (draft/active/withdrawn legality, the
// four-eyes creator-vs-activator check, and - since migration 0103 -
// the supersession requirement on active->withdrawn) to reject anything
// illegal - this function adds no duplicate business logic, only the
// audit record. successorID is NULL for every transition except
// active->withdrawn done via SupersedeEnforcementPolicy.
func transitionEnforcementPolicy(ctx context.Context, tx pgx.Tx, id uuid.UUID, fromStatus, toStatus, action string, successorID *uuid.UUID, w PolicyWrite) error {
	return transitionEnforcementPolicyMeta(ctx, tx, id, fromStatus, toStatus, action, successorID, nil, w)
}

// transitionEnforcementPolicyMeta is transitionEnforcementPolicy with optional request evidence (the admin routes).
func transitionEnforcementPolicyMeta(ctx context.Context, tx pgx.Tx, id uuid.UUID, fromStatus, toStatus, action string, successorID *uuid.UUID, meta *AuditMeta, w PolicyWrite) error {
	principal, err := assertPlatformAdminScope(ctx, tx)
	if err != nil {
		return err
	}
	if w.Proof == nil {
		return ErrPolicyProofRequired
	}
	if !ValidPolicyReasonCode(w.ReasonCode) {
		return fmt.Errorf("%w: reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]", ErrPolicyInvalidInput)
	}
	// The reason code travels in a single-use transaction-local setting the trigger folds into the verified hash.
	if _, err := tx.Exec(ctx, `SELECT set_config('app.kyc_policy_reason_code', $1, true)`, w.ReasonCode); err != nil {
		return fmt.Errorf("kyc: set reason code: %w", err)
	}
	if err := w.Proof.AttachTransition(ctx, tx, principal, PolicyTransition{ID: id, From: fromStatus, To: toStatus, Successor: successorID, ReasonCode: w.ReasonCode}); err != nil {
		return fmt.Errorf("kyc: attach proof: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = $1, superseded_by_policy_id = $4 WHERE id = $2 AND status = $3`, toStatus, id, fromStatus, successorID)
	if err != nil {
		return fmt.Errorf("kyc: transition enforcement policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: policy %s is not in state %q", ErrPolicyInvalidInput, id, fromStatus)
	}
	entry := audit.Entry{
		TenantID:   uuid.Nil,
		ActorType:  audit.ActorStaff,
		ActorID:    principal,
		Action:     action,
		TargetType: "kyc_enforcement_policy",
		TargetID:   id.String(),
		Outcome:    audit.OutcomeSuccess,
	}
	if meta != nil {
		entry.IPAddress, entry.UserAgent, entry.RequestID = meta.IPAddress, meta.UserAgent, meta.RequestID
		md := map[string]any{"from_status": fromStatus, "to_status": toStatus, "reason_code": meta.ReasonCode}
		if successorID != nil {
			md["successor_policy_id"] = successorID.String()
		}
		// lic18 L6: the AFTER state of the row, read in the same transaction after the update.
		if after, aerr := readPolicyAfterState(ctx, tx, id); aerr == nil {
			md["after"] = after
		} else {
			return fmt.Errorf("kyc: audit after-state: %w", aerr)
		}
		entry.Metadata = md
	}
	return audit.Record(ctx, tx, entry)
}

// ActivateEnforcementPolicy moves a draft row to active. Must be called
// by a DIFFERENT platform-admin principal than the one who created the
// row (migration 0100's lifecycle trigger enforces this at the
// database).
func ActivateEnforcementPolicy(ctx context.Context, tx pgx.Tx, id uuid.UUID, w PolicyWrite) error {
	return transitionEnforcementPolicy(ctx, tx, id, "draft", "active", "kyc_enforcement_policy.activated", nil, w)
}

// WithdrawEnforcementPolicy withdraws a draft or active row, WITHOUT
// naming a successor.
//
// SECURITY F3 (rv-prh-i3-security.md, 2026-09-27; closed at the DB level
// by migration 0103, ADR 0096 §17.3): withdrawing a DRAFT row (never
// having been enforced) works exactly as before. Withdrawing an ACTIVE
// row through this function is now REFUSED BY THE DATABASE ITSELF -
// migration 0103's extended lifecycle trigger requires
// superseded_by_policy_id to be set on every active->withdrawn
// transition, and this function never sets it. There is no longer any
// standalone way to withdraw an active policy and leave the key with no
// active successor; SupersedeEnforcementPolicy below is the only path
// the database accepts for that transition.
func WithdrawEnforcementPolicy(ctx context.Context, tx pgx.Tx, id uuid.UUID, fromStatus string, w PolicyWrite) error {
	return transitionEnforcementPolicy(ctx, tx, id, fromStatus, "withdrawn", "kyc_enforcement_policy.withdrawn", nil, w)
}

// withdrawEnforcementPolicyWithSuccessor is SupersedeEnforcementPolicy's
// own withdrawal step: it names successorID in the SAME UPDATE statement
// that transitions the row to 'withdrawn'. Migration 0103's deferred
// constraint trigger confirms, at commit time, that successorID is by
// then 'active' and shares the withdrawn row's enforcement key.
func withdrawEnforcementPolicyWithSuccessor(ctx context.Context, tx pgx.Tx, id uuid.UUID, fromStatus string, successorID uuid.UUID, w PolicyWrite) error {
	return transitionEnforcementPolicy(ctx, tx, id, fromStatus, "withdrawn", "kyc_enforcement_policy.withdrawn", &successorID, w)
}

// SupersedeEnforcementPolicyParams is SupersedeEnforcementPolicy's input.
type SupersedeEnforcementPolicyParams struct {
	// WithdrawID/WithdrawFromStatus identify the currently-active (or
	// draft) row being replaced.
	WithdrawID         uuid.UUID
	WithdrawFromStatus string
	// WithdrawnBy is the platform-admin principal performing the
	// withdrawal step - must differ from WithdrawID's own
	// created_by_actor_id (migration 0100's trigger enforces this).
	WithdrawnBy uuid.UUID
	// NewPolicy is the replacement row's content. CreatedBy is the
	// principal authoring it.
	NewPolicy CreateEnforcementPolicyParams
	CreatedBy uuid.UUID
	// ActivatedBy is the principal activating the replacement - must
	// differ from CreatedBy (migration 0100's trigger enforces this).
	ActivatedBy uuid.UUID
	// Proof signs the three steps (lic19, S4); ReasonCode is the reason of the withdrawal and the activation.
	Proof      PolicyProof
	ReasonCode string
}

// SupersedeEnforcementPolicy withdraws an existing policy row and
// authors+activates its replacement, ALL IN THE SAME database
// transaction (tx) - so a caller using this sanctioned path can never
// leave an active policy withdrawn with no successor also committed in
// that same atomic unit. tx must already be a platform-admin-scoped
// transaction (db.Pool.WithPlatformAdmin); this function itself changes
// the acting-principal GUC between steps (CreatedBy -> WithdrawnBy ->
// ActivatedBy) so each step's own four-eyes check is evaluated against
// the RIGHT principal, not whichever one opened the transaction.
//
// Order matters and is NOT arbitrary: the replacement is authored as
// 'draft' FIRST (the unique partial index
// kyc_enforcement_policies_one_active forbids two simultaneously-active
// rows for the same key, so the replacement cannot already be 'active'
// while the original still is), THEN the original is withdrawn NAMING
// the replacement's id as its successor, THEN the replacement is
// activated. Migration 0103's deferred (commit-time) constraint trigger
// confirms the named successor really did reach 'active', for the same
// enforcement key, by the time this transaction commits - closing
// security F3 at the database level, not merely by convention (ADR 0096
// §17.3).
func SupersedeEnforcementPolicy(ctx context.Context, tx pgx.Tx, p SupersedeEnforcementPolicyParams) (uuid.UUID, error) {
	if err := setActingPrincipal(ctx, tx, p.CreatedBy); err != nil {
		return uuid.Nil, err
	}
	if p.NewPolicy.Proof == nil {
		p.NewPolicy.Proof = p.Proof
	}
	newID, err := CreateEnforcementPolicy(ctx, tx, p.NewPolicy)
	if err != nil {
		return uuid.Nil, err
	}

	if err := setActingPrincipal(ctx, tx, p.WithdrawnBy); err != nil {
		return uuid.Nil, err
	}
	if err := withdrawEnforcementPolicyWithSuccessor(ctx, tx, p.WithdrawID, p.WithdrawFromStatus, newID, PolicyWrite{Proof: p.Proof, ReasonCode: p.ReasonCode}); err != nil {
		return uuid.Nil, err
	}

	if err := setActingPrincipal(ctx, tx, p.ActivatedBy); err != nil {
		return uuid.Nil, err
	}
	if err := ActivateEnforcementPolicy(ctx, tx, newID, PolicyWrite{Proof: p.Proof, ReasonCode: p.ReasonCode}); err != nil {
		return uuid.Nil, err
	}
	return newID, nil
}

// setActingPrincipal changes app.platform_admin_principal_id for the
// remainder of THIS transaction - tx must already be platform-admin
// scoped (the tenant/player GUCs stay unset throughout).
func setActingPrincipal(ctx context.Context, tx pgx.Tx, principalID uuid.UUID) error {
	if principalID == uuid.Nil {
		return fmt.Errorf("%w: acting principal id is required", ErrPolicyInvalidInput)
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, principalID.String()); err != nil {
		return fmt.Errorf("kyc: set acting principal: %w", err)
	}
	return nil
}

// policyAfterState is the audit "after" object of a policy row: the rule's values, never free text.
func policyAfterState(trigger, status string, threshold, asset, tier, play *string, hasLegalRef bool, successor *string) map[string]any {
	m := map[string]any{"trigger_type": trigger, "status": status, "has_legal_review_reference": hasLegalRef}
	for k, v := range map[string]*string{"threshold_minor_units": threshold, "asset_code": asset, "required_tier": tier, "play_operation": play, "successor_policy_id": successor} {
		if v != nil {
			m[k] = *v
		}
	}
	return m
}

func readPolicyAfterState(ctx context.Context, tx pgx.Tx, id uuid.UUID) (map[string]any, error) {
	var trigger, status string
	var threshold, asset, tier, play, legal, successor *string
	if err := tx.QueryRow(ctx, `SELECT trigger_type, status, threshold_minor_units::text, asset_code, required_tier, play_operation,
			legal_review_reference, superseded_by_policy_id::text FROM kyc_enforcement_policies WHERE id = $1`, id).
		Scan(&trigger, &status, &threshold, &asset, &tier, &play, &legal, &successor); err != nil {
		return nil, err
	}
	return policyAfterState(trigger, status, threshold, asset, tier, play, legal != nil && *legal != "", successor), nil
}
