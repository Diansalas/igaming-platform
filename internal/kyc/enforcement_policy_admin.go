package kyc

// lic10 (L-20): the READ side and the route-shaped write helpers of the KYC enforcement policy authoring surface. The WRITE
// rules stay where they always were: migration 0100/0103 (draft-only insert, four-eyes creator versus activator, supersession
// of an active row, no activation of a trigger the evaluator does not consult) are the authority; nothing here duplicates
// them. NO threshold value, asset or tier is defined here: every value is supplied by an authorised administrator.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// EnforcementPolicy is one kyc_enforcement_policies row as the admin API reads it.
type EnforcementPolicy struct {
	ID                      uuid.UUID
	LicensingJurisdictionID uuid.UUID
	TriggerType             string
	Status                  string
	ThresholdMinorUnits     *string
	AssetCode               *string
	RequiredTier            *string
	PlayOperation           *string
	EffectiveFrom           time.Time
	LegalReviewReference    *string
	ReasonCode              string
	CreatedAt               time.Time
	CreatedByActorID        uuid.UUID
	SupersededByPolicyID    *uuid.UUID
}

const enforcementPolicyColumns = `id, licensing_jurisdiction_id, trigger_type, status, threshold_minor_units::text, asset_code, required_tier,
	play_operation, effective_from, legal_review_reference, reason_code, created_at, created_by_actor_id, superseded_by_policy_id`

func scanEnforcementPolicy(row pgx.Row) (EnforcementPolicy, error) {
	var p EnforcementPolicy
	err := row.Scan(&p.ID, &p.LicensingJurisdictionID, &p.TriggerType, &p.Status, &p.ThresholdMinorUnits, &p.AssetCode, &p.RequiredTier,
		&p.PlayOperation, &p.EffectiveFrom, &p.LegalReviewReference, &p.ReasonCode, &p.CreatedAt, &p.CreatedByActorID, &p.SupersededByPolicyID)
	return p, err
}

// ListEnforcementPoliciesFilter narrows a list. A zero field means "any".
type ListEnforcementPoliciesFilter struct {
	LicensingJurisdictionID uuid.UUID
	Status                  string
}

// MaxEnforcementPolicyList bounds one list (the table is small platform configuration; the bound is a safety net).
const MaxEnforcementPolicyList = 500

// ListEnforcementPolicies lists policies newest first. tx must be a platform-admin-scoped transaction (the table is
// readable by anyone; the scope assertion keeps the admin surface from being driven from a tenant session by mistake).
func ListEnforcementPolicies(ctx context.Context, tx pgx.Tx, f ListEnforcementPoliciesFilter) ([]EnforcementPolicy, error) {
	if _, err := assertPlatformAdminScope(ctx, tx); err != nil {
		return nil, err
	}
	if f.Status != "" && f.Status != "draft" && f.Status != "active" && f.Status != "withdrawn" {
		return nil, fmt.Errorf("%w: status filter", ErrPolicyInvalidInput)
	}
	var jur *uuid.UUID
	if f.LicensingJurisdictionID != uuid.Nil {
		jur = &f.LicensingJurisdictionID
	}
	var status *string
	if f.Status != "" {
		status = &f.Status
	}
	rows, err := tx.Query(ctx, `SELECT `+enforcementPolicyColumns+` FROM kyc_enforcement_policies
		WHERE ($1::uuid IS NULL OR licensing_jurisdiction_id = $1) AND ($2::text IS NULL OR status = $2)
		ORDER BY created_at DESC, id DESC LIMIT $3`, jur, status, MaxEnforcementPolicyList)
	if err != nil {
		return nil, fmt.Errorf("kyc: list enforcement policies: %w", err)
	}
	defer rows.Close()
	var out []EnforcementPolicy
	for rows.Next() {
		p, err := scanEnforcementPolicy(rows)
		if err != nil {
			return nil, fmt.Errorf("kyc: scan enforcement policy: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetEnforcementPolicy reads one policy; ErrNotFound when absent.
func GetEnforcementPolicy(ctx context.Context, tx pgx.Tx, id uuid.UUID) (EnforcementPolicy, error) {
	if _, err := assertPlatformAdminScope(ctx, tx); err != nil {
		return EnforcementPolicy{}, err
	}
	p, err := scanEnforcementPolicy(tx.QueryRow(ctx, `SELECT `+enforcementPolicyColumns+` FROM kyc_enforcement_policies WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return EnforcementPolicy{}, ErrNotFound
	}
	if err != nil {
		return EnforcementPolicy{}, fmt.Errorf("kyc: get enforcement policy: %w", err)
	}
	return p, nil
}

// SupersedeEnforcementPolicyWithDraft withdraws the ACTIVE policy oldID naming the DRAFT successorID as its successor and
// activates the successor, in the caller's transaction, all as ONE acting principal. The database is the authority on every
// rule: the acting principal must differ from BOTH rows' creators (migration 0100 four-eyes), the successor must share the
// enforcement key (0103 deferred check), and the successor must be activatable. There is deliberately NO way to switch an
// active policy off without a successor (migration 0103, security F3): enforcement is never silently disabled.
func SupersedeEnforcementPolicyWithDraft(ctx context.Context, tx pgx.Tx, oldID, successorID uuid.UUID, meta *AuditMeta) error {
	if oldID == uuid.Nil || successorID == uuid.Nil || oldID == successorID {
		return fmt.Errorf("%w: a distinct active policy and draft successor are required", ErrPolicyInvalidInput)
	}
	if err := transitionEnforcementPolicyMeta(ctx, tx, oldID, "active", "withdrawn", "kyc_enforcement_policy.withdrawn", &successorID, meta); err != nil {
		return err
	}
	return transitionEnforcementPolicyMeta(ctx, tx, successorID, "draft", "active", "kyc_enforcement_policy.activated", nil, meta)
}

// ActivateEnforcementPolicyAudited is ActivateEnforcementPolicy with request evidence on the audit record.
func ActivateEnforcementPolicyAudited(ctx context.Context, tx pgx.Tx, id uuid.UUID, meta *AuditMeta) error {
	return transitionEnforcementPolicyMeta(ctx, tx, id, "draft", "active", "kyc_enforcement_policy.activated", nil, meta)
}

// WithdrawDraftEnforcementPolicyAudited withdraws a policy that was never activated (a draft). An ACTIVE policy cannot be
// withdrawn this way: the database refuses it (migration 0103); use SupersedeEnforcementPolicyWithDraft.
func WithdrawDraftEnforcementPolicyAudited(ctx context.Context, tx pgx.Tx, id uuid.UUID, meta *AuditMeta) error {
	return transitionEnforcementPolicyMeta(ctx, tx, id, "draft", "withdrawn", "kyc_enforcement_policy.withdrawn", nil, meta)
}

// PolicyFailure is the closed classification of a database refusal of a policy write.
type PolicyFailure string

const (
	PolicyFailureNone         PolicyFailure = ""
	PolicyFailureFourEyes     PolicyFailure = "four_eyes_refused"
	PolicyFailureNotWired     PolicyFailure = "trigger_not_wired"
	PolicyFailureNeedsSuccess PolicyFailure = "supersession_required"
	PolicyFailureIllegalMove  PolicyFailure = "illegal_transition"
	PolicyFailureDuplicate    PolicyFailure = "active_policy_exists"
	PolicyFailureUnknownRef   PolicyFailure = "unknown_reference"
	PolicyFailureInvalid      PolicyFailure = "invalid_policy"
	PolicyFailureOther        PolicyFailure = "other"
)

// ClassifyPolicyError maps a database refusal of a policy write to a closed code (the route turns it into a fixed sentence;
// the database text is never returned to a caller). The lifecycle triggers raise plain exceptions (P0001) whose text is the
// only discriminator, so this matches on documented phrases of migrations 0100/0103; an unrecognised refusal is
// PolicyFailureOther and is surfaced as a conflict, never as success.
func ClassifyPolicyError(err error) PolicyFailure {
	if err == nil {
		return PolicyFailureNone
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return PolicyFailureOther
	}
	switch pe.Code {
	case "23505":
		return PolicyFailureDuplicate
	case "23503":
		return PolicyFailureUnknownRef
	case "23514", "22P02", "22003":
		return PolicyFailureInvalid
	case "42501":
		return PolicyFailureOther
	}
	m := pe.Message
	switch {
	case strings.Contains(m, "four-eyes"):
		return PolicyFailureFourEyes
	case strings.Contains(m, "not yet evaluator-wired"):
		return PolicyFailureNotWired
	case strings.Contains(m, "requires supersession"), strings.Contains(m, "superseding policy"):
		return PolicyFailureNeedsSuccess
	case strings.Contains(m, "illegal status transition"), strings.Contains(m, "only status"):
		return PolicyFailureIllegalMove
	}
	return PolicyFailureOther
}

// ValidPolicyReasonCode reports whether s is a well-formed reason code (a code, never free text).
func ValidPolicyReasonCode(s string) bool { return policyReason.MatchString(s) }

// ---- Route-level validation (pure, so it is unit-tested without a database) ----

// WiredEnforcementTriggerTypes are the trigger types the evaluator consults and the database lets be activated (migration
// 0100 N3). The admin route authors ONLY these: authoring a draft of an unwired type would suggest a control that governs
// nothing. edd_amount and registration_tier stay NOT IMPLEMENTED until the evaluator consults them.
var WiredEnforcementTriggerTypes = []string{"cumulative_deposit", "play"}

var (
	thresholdRE  = regexp.MustCompile(`^[1-9][0-9]{0,37}$`)
	assetCodeRE  = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	policyReason = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)
)

// ValidatePolicyInput checks a create request's shape for the two wired trigger types. It invents no value: it only refuses
// shapes the table's CHECKs would refuse anyway, with a readable reason, before any write.
func ValidatePolicyInput(p CreateEnforcementPolicyParams) error {
	if p.LicensingJurisdictionID == uuid.Nil {
		return fmt.Errorf("%w: licensing_jurisdiction_id is required", ErrPolicyInvalidInput)
	}
	if !policyReason.MatchString(p.ReasonCode) {
		return fmt.Errorf("%w: reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]", ErrPolicyInvalidInput)
	}
	ref := p.LegalReviewReference
	if strings.TrimSpace(ref) == "" || strings.TrimSpace(ref) != ref || len(ref) > 200 {
		return fmt.Errorf("%w: legal_review_reference is required (1..200 characters, no surrounding space)", ErrPolicyInvalidInput)
	}
	for _, r := range ref {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return fmt.Errorf("%w: legal_review_reference contains a control character", ErrPolicyInvalidInput)
		}
	}
	switch p.TriggerType {
	case "cumulative_deposit":
		if p.ThresholdMinorUnits == nil || !thresholdRE.MatchString(*p.ThresholdMinorUnits) {
			return fmt.Errorf("%w: threshold_minor_units must be a positive integer string (minor units)", ErrPolicyInvalidInput)
		}
		if p.AssetCode == nil || !assetCodeRE.MatchString(*p.AssetCode) {
			return fmt.Errorf("%w: asset_code is required", ErrPolicyInvalidInput)
		}
		if p.RequiredTier != nil || p.PlayOperation != nil {
			return fmt.Errorf("%w: required_tier and play_operation do not apply to cumulative_deposit", ErrPolicyInvalidInput)
		}
	case "play":
		if p.PlayOperation == nil || (*p.PlayOperation != "casino_play" && *p.PlayOperation != "sportsbook_play") {
			return fmt.Errorf("%w: play_operation must be casino_play or sportsbook_play", ErrPolicyInvalidInput)
		}
		if p.ThresholdMinorUnits != nil || p.AssetCode != nil || p.RequiredTier != nil {
			return fmt.Errorf("%w: threshold_minor_units, asset_code and required_tier do not apply to play", ErrPolicyInvalidInput)
		}
	default:
		return fmt.Errorf("%w: trigger_type must be one of %v (edd_amount and registration_tier are not evaluator-wired)", ErrPolicyInvalidInput, WiredEnforcementTriggerTypes)
	}
	return nil
}
