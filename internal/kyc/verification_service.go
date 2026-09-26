package kyc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/validation"
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

// ErrEvidenceCollectionInactive is returned by ReviewVerification when a
// caller attempts to record a verified-residence determination
// (ReviewVerificationParams.VerifiedResidenceCountry != nil) while
// jurisdiction_evidence_collection_active is OFF for (tenant,
// verified_residence) - Stage 4I Phase B's activation boundary. Distinct
// from ErrInvalidTransition so the HTTP layer maps it to 403, not 400/409.
// When this is returned, the ENTIRE review call - including any status
// transition requested in the same call - is rolled back; a
// partially-applied review (status changed, residence determination
// silently dropped) is never allowed to happen.
var ErrEvidenceCollectionInactive = errors.New("kyc: verified-residence evidence collection is not active for this tenant")

const verificationColumns = `id, tenant_id, brand_id, player_account_id, person_id, status,
	provider_id, provider_reference, reason, submitted_at, reviewed_at, reviewed_by, expires_at, created_at, updated_at,
	(verified_residence_country IS NOT NULL), verified_residence_set_at, verified_residence_set_by`

func scanVerification(row pgx.Row) (Verification, error) {
	var v Verification
	var providerRef, reason *string
	var reviewedBy *uuid.UUID
	err := row.Scan(&v.ID, &v.TenantID, &v.BrandID, &v.PlayerAccountID, &v.PersonID, &v.Status,
		&v.ProviderID, &providerRef, &reason, &v.SubmittedAt, &v.ReviewedAt, &reviewedBy, &v.ExpiresAt, &v.CreatedAt, &v.UpdatedAt,
		&v.HasVerifiedResidence, &v.VerifiedResidenceSetAt, &v.VerifiedResidenceSetBy)
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

	// An explicit field-by-field literal, not a type conversion, so a field
	// later added to CreateVerificationParams is never passed to the
	// provider interface by accident.
	result, err := provider.CreateVerification(ctx, CreateVerificationInput{ //nolint:staticcheck // S1016: see comment above
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

// ListVerificationsForTenantParams is ListVerificationsForTenant's input.
// Status == "" and PlayerAccountID == uuid.Nil each mean "no filter on
// that dimension" - a genuinely tenant-wide, unfiltered operator queue
// read relies entirely on kyc_verifications' own tenant_isolation RLS
// policy (migration 0040) for tenant scoping, never a predicate this
// function adds itself (mirrors ListVerificationsForAccount's identical
// reliance on ambient RLS). Limit/Offset are trusted, already-validated
// pagination inputs (see internal/httpserver.parsePageParams, the Stage 5
// Back Office pagination convention) - this function does not
// independently re-validate their bounds.
type ListVerificationsForTenantParams struct {
	Status          VerificationStatus
	PlayerAccountID uuid.UUID
	Limit           int
	Offset          int
}

// scanVerificationWithTotal is scanVerification's counterpart for a query
// that additionally projects a `count(*) OVER()` window column - kept
// separate from scanVerification (whose column list is exactly
// verificationColumns, reused verbatim by callers with no extra column)
// rather than complicating that helper's signature for its one caller
// that needs a running total.
func scanVerificationWithTotal(row pgx.Row) (Verification, int, error) {
	var v Verification
	var providerRef, reason *string
	var reviewedBy *uuid.UUID
	var total int
	err := row.Scan(&v.ID, &v.TenantID, &v.BrandID, &v.PlayerAccountID, &v.PersonID, &v.Status,
		&v.ProviderID, &providerRef, &reason, &v.SubmittedAt, &v.ReviewedAt, &reviewedBy, &v.ExpiresAt, &v.CreatedAt, &v.UpdatedAt,
		&v.HasVerifiedResidence, &v.VerifiedResidenceSetAt, &v.VerifiedResidenceSetBy, &total)
	if err != nil {
		return Verification{}, 0, fmt.Errorf("kyc: scan verification with total: %w", err)
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
	return v, total, nil
}

// ListVerificationsForTenant is the Stage 5 Back Office operator queue
// read: every verification visible under the current RLS scope, optionally
// narrowed by Status and/or PlayerAccountID, newest first, paginated, along
// with the total matching row count (ignoring Limit/Offset) so a caller can
// render pagination controls. Distinct from ListVerificationsForAccount,
// which is unpaginated and always scoped to one already-known
// playerAccountID - this is the additional, tenant-wide "operator queue"
// counterpart that view was never meant to serve on its own; neither
// function's behavior changes because the other exists.
func ListVerificationsForTenant(ctx context.Context, tx pgx.Tx, params ListVerificationsForTenantParams) ([]Verification, int, error) {
	var playerFilter *uuid.UUID
	if params.PlayerAccountID != uuid.Nil {
		v := params.PlayerAccountID
		playerFilter = &v
	}
	rows, err := tx.Query(ctx,
		`SELECT `+verificationColumns+`, count(*) OVER() AS total_count
		 FROM kyc_verifications
		 WHERE ($1 = '' OR status = $1)
		   AND ($2::uuid IS NULL OR player_account_id = $2)
		 ORDER BY created_at DESC
		 LIMIT $3 OFFSET $4`,
		string(params.Status), playerFilter, params.Limit, params.Offset,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("kyc: list verifications for tenant: %w", err)
	}
	defer rows.Close()
	var out []Verification
	total := 0
	for rows.Next() {
		v, t, err := scanVerificationWithTotal(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, v)
		total = t
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("kyc: list verifications for tenant: %w", err)
	}
	return out, total, nil
}

// getVerificationByProviderReference is the Orchestrator's own lookup for
// callback dispatch - unexported, since a caller outside this package
// should never look a verification up by provider reference (only the
// orchestrator, which just received that exact reference FROM the
// provider, has legitimate reason to). tenantID is an EXPLICIT predicate
// (architect R4/J6), on top of - never instead of - kyc_verifications' own
// tenant_isolation RLS/FORCE ROW LEVEL SECURITY and migration 0040's
// identically-shaped UNIQUE (tenant_id, provider_id, provider_reference)
// index: a callback reachable only via a tenant-bound verified signature
// must never resolve to a DIFFERENT tenant's row even if RLS were somehow
// misconfigured for this connection.
func getVerificationByProviderReference(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, providerReference string) (Verification, error) {
	return scanVerification(tx.QueryRow(ctx,
		`SELECT `+verificationColumns+` FROM kyc_verifications WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3`,
		tenantID, providerID, providerReference,
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

	// IPAddress/UserAgent/RequestID are recorded on the audit entries this
	// call writes (CLAUDE.md's audit rule names IP explicitly for every
	// mutating administrative action). Optional only in the sense that a
	// non-HTTP caller may have none to supply; the HTTP handler always
	// populates all three.
	IPAddress string
	UserAgent string
	RequestID string

	// VerifiedResidenceCountry, when non-nil, records THIS reviewer's
	// explicit residence determination (HDR-J-3c) as part of this same
	// review act. nil means "this review makes no residence
	// determination" and leaves any existing value on the row untouched.
	// A non-nil pointer to "" is a caller error (ErrInvalidTransition),
	// never treated as "clear the value" - there is no clear/revoke
	// operation in this phase.
	VerifiedResidenceCountry *string
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
	if params.VerifiedResidenceCountry != nil && !validation.IsISO3166Alpha2(*params.VerifiedResidenceCountry) {
		return Verification{}, fmt.Errorf("%w: verified_residence_country must be a valid ISO-3166-1 alpha-2 code", ErrInvalidTransition)
	}

	// KYC-REASON-BOUND-1 (Stage 10.3, W0 architect code-check addendum):
	// a staff reviewer's own free-text reason writes to the SAME
	// kyc_verifications.reason column migration 0095's CHECK
	// (octet_length(reason) <= 512) now bounds - this is this write
	// path's own ingestion boundary (mirroring MockKYCProvider.
	// HandleCallback's identical call for the provider-callback path),
	// so an oversized staff reason is normalized here, once, rather than
	// ever reaching the database as a raw CHECK-constraint violation
	// (an unhelpful 500, not a validation error). Every caller of this
	// function - the staff HTTP handler and any internal/kyc-package
	// caller alike - gets this bound for free.
	params.Reason, _ = NormalizeReason(params.Reason)

	current, err := GetVerificationByID(ctx, tx, params.VerificationID)
	if err != nil {
		return Verification{}, err
	}
	if isTerminal(current.Status) {
		return Verification{}, fmt.Errorf("%w: verification %s is already in a terminal status %q", ErrInvalidTransition, params.VerificationID, current.Status)
	}

	// Stage 4I Phase B's activation boundary: only checked (and only
	// capable of failing the WHOLE call) when a residence determination is
	// actually being attempted. Checked BEFORE the UPDATE below, inside
	// this same transaction, so a failure here leaves the status
	// transition unapplied too - one transaction, all-or-nothing.
	if params.VerifiedResidenceCountry != nil {
		active, err := jurisdiction.IsEvidenceCollectionActive(ctx, tx, current.TenantID, jurisdiction.EvidenceVerifiedResidence)
		if err != nil {
			return Verification{}, fmt.Errorf("kyc: check verified-residence evidence collection active: %w", err)
		}
		if !active {
			return Verification{}, ErrEvidenceCollectionInactive
		}
	}

	var hadPreviousResidence, residenceChanged bool
	var newSetAt *time.Time
	err = tx.QueryRow(ctx, `
		WITH prev AS (SELECT verified_residence_country AS before_value FROM kyc_verifications WHERE id = $4)
		UPDATE kyc_verifications
		   SET status      = $1,
		       reason      = NULLIF($2, ''),
		       reviewed_at = now(),
		       reviewed_by = $3,
		       verified_residence_country = COALESCE($5, verified_residence_country),
		       verified_residence_source  = CASE WHEN $5 IS NULL THEN verified_residence_source ELSE 'reviewer_determination' END,
		       verified_residence_set_by  = CASE WHEN $5 IS NULL THEN verified_residence_set_by  ELSE $3    END,
		       verified_residence_set_at  = CASE WHEN $5 IS NULL THEN verified_residence_set_at  ELSE now() END,
		       updated_at  = now()
		  FROM prev
		 WHERE kyc_verifications.id = $4
		RETURNING (prev.before_value IS NOT NULL) AS had_previous,
		          (prev.before_value IS DISTINCT FROM COALESCE($5, prev.before_value)) AS changed,
		          kyc_verifications.verified_residence_set_at`,
		params.NewStatus, params.Reason, params.StaffID, params.VerificationID, params.VerifiedResidenceCountry,
	).Scan(&hadPreviousResidence, &residenceChanged, &newSetAt)
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: review verification: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: current.TenantID, ActorType: audit.ActorStaff, ActorID: params.StaffID,
		Action: "kyc.verification_status_changed", TargetType: "kyc_verification", TargetID: params.VerificationID.String(),
		Outcome:   audit.OutcomeSuccess,
		IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: map[string]any{"previous_status": string(current.Status), "new_status": string(params.NewStatus), "reason": params.Reason},
	}); err != nil {
		return Verification{}, fmt.Errorf("kyc: audit review verification: %w", err)
	}

	if params.VerifiedResidenceCountry != nil {
		// CRITICAL: the country VALUE (or any hash of it) must never
		// appear in this metadata - not even indirectly via a free-text
		// field. params.Reason is deliberately NOT included here (it
		// already rides on the kyc.verification_status_changed entry
		// above): a reviewer's free-text reason is exactly the kind of
		// uncontrolled string that could contain the country the
		// activation-gate mechanism exists to keep out of this record.
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: current.TenantID, ActorType: audit.ActorStaff, ActorID: params.StaffID,
			Action: "kyc.verified_residence_determined", TargetType: "kyc_verification", TargetID: params.VerificationID.String(),
			Outcome:   audit.OutcomeSuccess,
			IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
			Metadata: map[string]any{
				"provenance":         "reviewer_determination",
				"had_previous_value": hadPreviousResidence,
				"value_changed":      residenceChanged,
				"player_account_id":  current.PlayerAccountID.String(),
				"determined_at":      newSetAt.UTC().Format(time.RFC3339), // non-nil: this block only runs when a determination was made this call
			},
		}); err != nil {
			return Verification{}, fmt.Errorf("kyc: audit verified residence determination: %w", err)
		}
	}

	return GetVerificationByID(ctx, tx, params.VerificationID)
}

// GetVerifiedResidence returns the country from the most recent APPROVED
// verification carrying a residence determination for this player. This
// selection rule (most recent approved) is a narrow, documented choice
// that a future precedence phase may need to revisit if a player has more
// than one approved verification with a determination.
//
// Explicitly distinguishes "no tenant scope at all on this connection"
// (an error) from "no row visible/matching under this scope" (ok=false) -
// mirrors identity.GetDeclaredResidence's own discipline, for the same
// silent-wrong-answer reason. Deliberately takes no tenantID parameter -
// unlike an earlier revision of this function, which took tenantID as a
// plain argument and used it in the WHERE clause alongside RLS. That
// shape let a caller-supplied tenantID silently diverge from the
// connection's actual RLS scope (harmless only because RLS still
// filtered correctly, but a trap for a future caller). This function
// instead relies on RLS alone for tenant scoping, exactly like
// identity.GetDeclaredResidence.
func GetVerifiedResidence(ctx context.Context, q RowQuerier, playerAccountID uuid.UUID) (country string, setAt time.Time, verificationID uuid.UUID, ok bool, err error) {
	var scoped *uuid.UUID
	if err := q.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), '')::uuid`).Scan(&scoped); err != nil {
		return "", time.Time{}, uuid.Nil, false, fmt.Errorf("kyc: get verified residence: read tenant scope: %w", err)
	}
	if scoped == nil {
		return "", time.Time{}, uuid.Nil, false, fmt.Errorf("kyc: get verified residence: transaction has no tenant scope (use db.Pool.WithTenant)")
	}

	var countryVal string
	var setAtVal time.Time
	var idVal uuid.UUID
	err = q.QueryRow(ctx, `
		SELECT verified_residence_country, verified_residence_set_at, id
		  FROM kyc_verifications
		 WHERE player_account_id = $1 AND verified_residence_country IS NOT NULL AND status = $2
		 ORDER BY verified_residence_set_at DESC LIMIT 1`,
		playerAccountID, StatusApproved,
	).Scan(&countryVal, &setAtVal, &idVal)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", time.Time{}, uuid.Nil, false, nil
	}
	if err != nil {
		return "", time.Time{}, uuid.Nil, false, fmt.Errorf("kyc: get verified residence: %w", err)
	}
	return countryVal, setAtVal, idVal, true, nil
}

// RowQuerier is the minimal read handle GetVerifiedResidence needs -
// satisfied by pgx.Tx today. Declared locally, mirroring
// identity.RowQuerier's own minimal-interface convention, rather than
// importing that package's (this package already has no dependency on
// internal/identity and should not gain one just for this interface).
type RowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}
