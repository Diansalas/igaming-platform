package kyc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// ErrNotFound covers "no such verification/document" - mirrors
// identity.ErrNotFound's own role, kept as this package's own sentinel
// rather than importing identity's (a caller distinguishing "player
// account not found" from "verification not found" needs two distinct
// errors).
var ErrNotFound = errors.New("kyc: not found")

// ErrInvalidTransition is returned by ReviewVerification/ReviewDocument
// when the requested status is not a valid review outcome, or the
// current status is not eligible for review.
var ErrInvalidTransition = errors.New("kyc: invalid status transition")

const verificationColumns = `id, tenant_id, brand_id, player_account_id, person_id, status,
	provider_id, provider_reference, reason, submitted_at, reviewed_at, reviewed_by, expires_at, created_at, updated_at`

func scanVerification(row pgx.Row) (Verification, error) {
	var v Verification
	var providerRef, reason *string
	var reviewedBy *uuid.UUID
	err := row.Scan(&v.ID, &v.TenantID, &v.BrandID, &v.PlayerAccountID, &v.PersonID, &v.Status,
		&v.ProviderID, &providerRef, &reason, &v.SubmittedAt, &v.ReviewedAt, &reviewedBy, &v.ExpiresAt, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, ErrNotFound
	}
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: scan verification: %w", err)
	}
	if providerRef != nil {
		v.ProviderReference = *providerRef
	}
	if reason != nil {
		v.Reason = *reason
	}
	if reviewedBy != nil {
		v.ReviewedBy = *reviewedBy
	}
	return v, nil
}

// CreateVerificationParams is CreateVerification's input. TenantID/
// BrandID/PlayerAccountID/PersonID must already be server-resolved from
// the authenticated player's own session (or, for a staff-initiated
// verification, from an already-authorized target account) - never
// client-supplied.
type CreateVerificationParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	PersonID        uuid.UUID
}

// CreateVerification starts a new verification attempt: calls the named
// provider's CreateVerification (obtaining a ProviderReference before any
// row exists, mirroring casino.LaunchGame's own "call the provider, then
// persist" ordering), then inserts the kyc_verifications row. tx must
// already be tenant-scoped (db.WithTenant).
func CreateVerification(ctx context.Context, tx pgx.Tx, provider KYCProvider, params CreateVerificationParams) (Verification, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.PersonID == uuid.Nil {
		return Verification{}, fmt.Errorf("%w: tenant_id, brand_id, player_account_id, and person_id are all required", ErrInvalidTransition)
	}

	result, err := provider.CreateVerification(ctx, CreateVerificationInput{
		TenantID: params.TenantID, BrandID: params.BrandID,
		PlayerAccountID: params.PlayerAccountID, PersonID: params.PersonID,
	})
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: create verification with provider: %w", err)
	}

	status, ok := statusForOutcome(result.Outcome)
	if !ok {
		status = StatusPending
	}

	v := Verification{
		ID: uuid.New(), TenantID: params.TenantID, BrandID: params.BrandID,
		PlayerAccountID: params.PlayerAccountID, PersonID: params.PersonID,
		Status: status, ProviderID: provider.ID(), ProviderReference: result.ProviderReference,
		Reason: result.Reason, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference, reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), NULLIF($9, ''))`,
		v.ID, v.TenantID, v.BrandID, v.PlayerAccountID, v.PersonID, v.Status, v.ProviderID, v.ProviderReference, v.Reason,
	)
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: insert verification: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
		Action: "kyc.verification_submitted", TargetType: "kyc_verification", TargetID: v.ID.String(),
		Outcome: audit.OutcomeSuccess, Metadata: map[string]any{"provider_id": v.ProviderID, "status": string(v.Status)},
	}); err != nil {
		return Verification{}, fmt.Errorf("kyc: audit create verification: %w", err)
	}
	return v, nil
}

// GetVerificationByID reads one verification, RLS-scoped to the caller's
// own tenant (or, under WithPlayerScope-style access - not used this
// stage, see ADR 0028's own recorded scope decision - the caller's own
// account). Returns ErrNotFound for a row outside the caller's scope,
// identical in shape to identity.GetPlayerAccountByID's own behavior.
func GetVerificationByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Verification, error) {
	return scanVerification(tx.QueryRow(ctx, `SELECT `+verificationColumns+` FROM kyc_verifications WHERE id = $1`, id))
}

// ListVerificationsForAccount returns every verification for
// playerAccountID, newest first, visible under the current RLS scope.
func ListVerificationsForAccount(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID) ([]Verification, error) {
	rows, err := tx.Query(ctx, `SELECT `+verificationColumns+` FROM kyc_verifications WHERE player_account_id = $1 ORDER BY created_at DESC`, playerAccountID)
	if err != nil {
		return nil, fmt.Errorf("kyc: list verifications: %w", err)
	}
	defer rows.Close()
	var out []Verification
	for rows.Next() {
		v, err := scanVerification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// getVerificationByProviderReference is the Orchestrator's own lookup for
// callback dispatch - unexported, since a caller outside this package
// should never look a verification up by provider reference (only the
// orchestrator, which just received that exact reference FROM the
// provider, has legitimate reason to).
func getVerificationByProviderReference(ctx context.Context, tx pgx.Tx, providerID, providerReference string) (Verification, error) {
	return scanVerification(tx.QueryRow(ctx,
		`SELECT `+verificationColumns+` FROM kyc_verifications WHERE provider_id = $1 AND provider_reference = $2`,
		providerID, providerReference,
	))
}

// updateVerificationStatus applies a new status+reason with no reviewer
// (a provider-driven, not staff-driven, transition) - the Orchestrator's
// own callback-application step. reviewed_at/reviewed_by are deliberately
// left untouched: those two columns record a STAFF review specifically
// (ReviewVerification below), never an automated provider decision - see
// migration 0040's own column comments.
func updateVerificationStatus(ctx context.Context, tx pgx.Tx, id uuid.UUID, status VerificationStatus, reason string) (Verification, error) {
	_, err := tx.Exec(ctx,
		`UPDATE kyc_verifications SET status = $1, reason = NULLIF($2, ''), updated_at = now() WHERE id = $3`,
		status, reason, id,
	)
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: update verification status: %w", err)
	}
	return GetVerificationByID(ctx, tx, id)
}

// ReviewVerificationParams is ReviewVerification's input.
type ReviewVerificationParams struct {
	VerificationID uuid.UUID
	StaffID        uuid.UUID
	NewStatus      VerificationStatus // must be StatusApproved, StatusRejected, or StatusReviewRequired
	Reason         string
}

// ReviewVerification is the STAFF-driven review action
// (PermVerificationReview) - distinct from the Orchestrator's own
// provider-driven updateVerificationStatus: this one always stamps
// reviewed_at/reviewed_by, recording that a HUMAN made this call.
func ReviewVerification(ctx context.Context, tx pgx.Tx, params ReviewVerificationParams) (Verification, error) {
	if params.NewStatus != StatusApproved && params.NewStatus != StatusRejected && params.NewStatus != StatusReviewRequired {
		return Verification{}, fmt.Errorf("%w: status must be approved, rejected, or review_required", ErrInvalidTransition)
	}
	if params.StaffID == uuid.Nil || params.VerificationID == uuid.Nil {
		return Verification{}, fmt.Errorf("%w: staff_id and verification_id are required", ErrInvalidTransition)
	}

	current, err := GetVerificationByID(ctx, tx, params.VerificationID)
	if err != nil {
		return Verification{}, err
	}
	if isTerminal(current.Status) {
		return Verification{}, fmt.Errorf("%w: verification %s is already in a terminal status %q", ErrInvalidTransition, params.VerificationID, current.Status)
	}

	_, err = tx.Exec(ctx,
		`UPDATE kyc_verifications SET status = $1, reason = NULLIF($2, ''), reviewed_at = now(), reviewed_by = $3, updated_at = now() WHERE id = $4`,
		params.NewStatus, params.Reason, params.StaffID, params.VerificationID,
	)
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: review verification: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: current.TenantID, ActorType: audit.ActorStaff, ActorID: params.StaffID,
		Action: "kyc.verification_status_changed", TargetType: "kyc_verification", TargetID: params.VerificationID.String(),
		Outcome:  audit.OutcomeSuccess,
		Metadata: map[string]any{"previous_status": string(current.Status), "new_status": string(params.NewStatus), "reason": params.Reason},
	}); err != nil {
		return Verification{}, fmt.Errorf("kyc: audit review verification: %w", err)
	}
	return GetVerificationByID(ctx, tx, params.VerificationID)
}
