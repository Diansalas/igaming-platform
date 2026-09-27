package kyc

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
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

	if err := audit.Record(ctx, tx, audit.Entry{
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
		},
	}); err != nil {
		return uuid.Nil, fmt.Errorf("kyc: audit: %w", err)
	}
	return id, nil
}

// transitionEnforcementPolicy is Activate/Withdraw's shared shape: an
// UPDATE ... WHERE id = $1 AND status = $2, relying entirely on migration
// 0100's lifecycle trigger (draft/active/withdrawn legality AND the
// four-eyes creator-vs-activator check) to reject anything illegal - this
// function adds no duplicate business logic, only the audit record.
func transitionEnforcementPolicy(ctx context.Context, tx pgx.Tx, id uuid.UUID, fromStatus, toStatus, action string) error {
	principal, err := assertPlatformAdminScope(ctx, tx)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = $1 WHERE id = $2 AND status = $3`, toStatus, id, fromStatus)
	if err != nil {
		return fmt.Errorf("kyc: transition enforcement policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: policy %s is not in state %q", ErrPolicyInvalidInput, id, fromStatus)
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   uuid.Nil,
		ActorType:  audit.ActorStaff,
		ActorID:    principal,
		Action:     action,
		TargetType: "kyc_enforcement_policy",
		TargetID:   id.String(),
		Outcome:    audit.OutcomeSuccess,
	})
}

// ActivateEnforcementPolicy moves a draft row to active. Must be called
// by a DIFFERENT platform-admin principal than the one who created the
// row (migration 0100's lifecycle trigger enforces this at the
// database).
func ActivateEnforcementPolicy(ctx context.Context, tx pgx.Tx, id uuid.UUID) error {
	return transitionEnforcementPolicy(ctx, tx, id, "draft", "active", "kyc_enforcement_policy.activated")
}

// WithdrawEnforcementPolicy withdraws a draft or active row. Withdrawing
// an active row is a relaxation of enforcement and requires the same
// four-eyes (different-principal) DB check as activation.
func WithdrawEnforcementPolicy(ctx context.Context, tx pgx.Tx, id uuid.UUID, fromStatus string) error {
	return transitionEnforcementPolicy(ctx, tx, id, fromStatus, "withdrawn", "kyc_enforcement_policy.withdrawn")
}
