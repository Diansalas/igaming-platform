package kyc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/providercred"
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

// ErrVerificationStatusConflict is returned by ReviewVerification when the
// verification's status changed between this call's own read and its write
// (the "mirror race" of R1/C1, surfaced to other owners by RV-PRH-I2 KYC
// code re-review: ReviewVerification's write was, until this fix, a blind
// `UPDATE ... WHERE id = $4` with no status predicate - so a concurrent
// SubmitVerification phase C, or a concurrent verified callback, committing
// between this call's read and its write could be silently overwritten,
// including moving a terminal `approved` backward to a staff
// `review_required`). Unlike applyForwardOnlyStatus's own no-op-on-lost-
// race convention (a provider-driven transition, safe to silently defer to
// whichever forward transition won), a STAFF decision is deliberate: on a
// lost race this call does NOT retry and does NOT silently no-op - it
// fails closed with this sentinel, so the caller (an operator) sees an
// explicit conflict and can re-review the verification's own CURRENT
// state rather than believing a decision was recorded when it was not.
var ErrVerificationStatusConflict = errors.New("kyc: verification status changed before this review could be applied")

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

// createVerificationCallTimeout bounds phase B's CreateVerification call.
const createVerificationCallTimeout = defaultProviderCallTimeout

// phaseCTimeout bounds CreateVerification/SubmitVerification's own phase-C
// transactions - mirrors casino.phaseCTimeout exactly, including running on
// context.WithoutCancel(ctx) so a cancelled request context can never
// silently skip the CAS apply (ADR 0095 §15.1.2/security review RV-PRH-I2
// C1, applied here for the same reason: this codebase has exactly one
// documented fix for "phase C must survive a cancelled request ctx", and
// KYC's own phase C is exposed to the identical hazard once a real adapter
// performs I/O).
const phaseCTimeout = 5 * time.Second

// insertOrphanVerification is phase A: a kyc_verifications row with
// status='unverified' and provider_reference NULL, plus the
// "kyc.verification_requested" audit record (ADR 0095 §15.2) - committed
// BEFORE any provider call. This row has no enforcement effect on its own:
// NOT because "EvaluateEnforcement only allows on 'passed'" (that claim is
// false in the deny direction - a naive "latest row" read would still map
// an unverified row to `failed` and could mask an existing approval/
// mislead an overlay, ADR 0096 §19/RV-PRH-I2 KYC review F1, R2-4 comment
// correction) but because ADR 0096's own read semantics explicitly
// EXCLUDE this exact shape (status='unverified' AND provider_reference IS
// NULL, orphanRowExclusionSQL) from "latest row" selection in BOTH
// readLatestVerificationByPlayerAccount and crossAccountRejectedOverlay's
// finalStatusesSQL-scoped subquery (ADR 0096 §2.6(g), §20.2) - so an
// orphan left behind by a phase B/C failure is harmless by construction,
// via that specific exclusion, not via any general "only passed matters"
// property.
func insertOrphanVerification(ctx context.Context, tx pgx.Tx, params CreateVerificationParams, providerID string) (Verification, error) {
	v := Verification{
		ID: uuid.New(), TenantID: params.TenantID, BrandID: params.BrandID,
		PlayerAccountID: params.PlayerAccountID, PersonID: params.PersonID,
		Status: StatusUnverified, ProviderID: providerID,
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference, reason)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, NULL, NULL)`,
		v.ID, v.TenantID, v.BrandID, v.PlayerAccountID, v.PersonID, v.Status, v.ProviderID,
	)
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: insert verification: %w", err)
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
		Action: "kyc.verification_requested", TargetType: "kyc_verification", TargetID: v.ID.String(),
		Outcome: audit.OutcomeSuccess, Metadata: map[string]any{"provider_id": v.ProviderID},
	}); err != nil {
		return Verification{}, fmt.Errorf("kyc: audit create verification: %w", err)
	}
	return v, nil
}

// applyCreateVerificationResult is phase C: a CAS UPDATE onto the phase-A
// row (ADR 0095 §15.2) - it only ever applies to the SAME orphan row this
// call's own phase A inserted (id AND provider_reference IS NULL AND
// status='unverified' in the WHERE clause; not a forward-only rank
// transition like the callback path, since this is the row's very first
// transition), plus the "kyc.verification_submitted" audit record (the
// existing action name, moved here from the old single-transaction
// CreateVerification per ADR 0095 §15.2's own table).
func applyCreateVerificationResult(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, v Verification, result ProviderResult) (Verification, error) {
	result, reasonTruncated := normalizeProviderResult(result)
	status, ok := statusForOutcome(result.Outcome)
	if !ok {
		if result.ProviderReference == "" {
			// R2 (RV-PRH-I2 KYC code re-review 2, "pre-existing, low: create-
			// side ProviderError/empty reference", 2026-09-27): an
			// unrecognized outcome (e.g. ProviderError returned WITHOUT a Go
			// error - PROVIDER DEPENDENT, the mock never does this) with NO
			// reference must NEVER be written as a `pending` row. Doing so
			// would create a row that (a) counts as "decided" under ADR
			// 0096's primary read (§2.6(a) - `pending` is non-final but
			// still supersedes an existing approval, unlike an excluded
			// orphan), turning a transient vendor ambiguity into a spurious
			// DENY for an already-approved player, and (b) is a PERMANENT
			// dead end under N5 - a `pending` row with no reference can
			// never be uploaded to or submitted (ErrVerificationNotSubmitted
			// fires on every attempt, and no reference will ever arrive to
			// change that). This mirrors SubmitVerification's own IC
			// condition 2 ("an ambiguous/timeout result leaves status
			// unchanged", ADR 0095 §15.3): the phase-A orphan row is left
			// EXACTLY as committed (still 'unverified', still NULL
			// reference) rather than accepting a worse-than-orphan
			// "pending-but-never-resolvable" state - fail closed in the ONE
			// direction that matters here (never let a transient vendor
			// ambiguity turn an existing allow into a deny, nor a deny into
			// an allow; leaving the row exactly as phase A committed it
			// does neither). The caller (CreateVerification) treats this
			// identically to a phase-B failure - ErrProviderUnavailable,
			// 503, no state change, no retry - see ADR 0096 §20.5.
			return Verification{}, fmt.Errorf("%w: provider returned an unrecognized outcome %q with no reference", ErrProviderUnavailable, result.Outcome)
		}
		status = StatusPending
	}
	tag, err := tx.Exec(ctx,
		`UPDATE kyc_verifications SET provider_reference = NULLIF($1, ''), status = $2, reason = NULLIF($3, ''), updated_at = now()
		 WHERE id = $4 AND tenant_id = $5 AND provider_reference IS NULL AND status = $6`,
		result.ProviderReference, status, result.Reason, v.ID, tenantID, StatusUnverified,
	)
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: apply create verification result: %w", err)
	}
	if tag.RowsAffected() != 1 {
		// Unreachable in this codebase's own call pattern (this function is
		// only ever invoked once, immediately after phase A committed
		// exactly this row, under this same verification id) - but never
		// assumed: a future caller violating that must fail closed rather
		// than silently double-apply or apply to the wrong row.
		return Verification{}, fmt.Errorf("kyc: apply create verification result: verification %s was not in the expected pre-reference state", v.ID)
	}
	updated, err := GetVerificationByID(ctx, tx, v.ID)
	if err != nil {
		return Verification{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorPlayer, ActorID: v.PlayerAccountID,
		Action: "kyc.verification_submitted", TargetType: "kyc_verification", TargetID: v.ID.String(),
		Outcome: audit.OutcomeSuccess, Metadata: withReasonTruncated(map[string]any{"provider_id": updated.ProviderID, "status": string(updated.Status)}, reasonTruncated),
	}); err != nil {
		return Verification{}, fmt.Errorf("kyc: audit create verification: %w", err)
	}
	return updated, nil
}

// CreateVerification starts a new verification attempt (ADR 0095 §15.2,
// PRH-I2): phase A commits the intent (an orphan 'unverified' row with no
// provider_reference, plus its own audit record) BEFORE any provider call;
// phase B calls the provider with NO database transaction/pooled connection
// held (the per-call outbound credential is resolved via outbound,
// PROV-OUTBOUND-CRED-1, immediately before the call); phase C applies the
// result under a CAS on a short, bounded transaction.
//
// pool is the tenant-scoped transaction runner (*db.Pool satisfies it,
// mirroring casino.LaunchGame's identical parameter); outbound is the KYC
// domain's outbound-credential resolver (a MOCK for a synthetic adapter,
// providercred's real *(*Subsystem).Outbound("kyc") for a real one). Both
// nil fail closed with ErrProviderUnavailable, the same nil-resolver
// convention LaunchGame's own parameters use - never a silent fallback.
//
// A phase B failure (no resolver, credential resolution failure, a
// credential binding mismatch, or the provider call itself failing) leaves
// the phase-A orphan row exactly as phase A committed it: 'unverified',
// provider_reference NULL. This orphan has no enforcement effect - not
// because "EvaluateEnforcement only allows on 'passed'" (R2-4 correction:
// that framing is false in the deny direction and was the exact defect ADR
// 0096 §19/RV-PRH-I2 KYC review F1 fixed) but because ADR 0096's own
// "latest row" reads explicitly exclude this shape outright
// (orphanRowExclusionSQL, §2.6(g); the cross-account overlay's own
// finalStatusesSQL-scoped subquery excludes it too, §20.2) - it is
// harmless by construction via that specific exclusion, exactly like a
// genuine crash in the same window (ADR 0095 §15.2 "Failure recovery").
// There is no automatic retry; a player retry calls CreateVerification
// again and gets a brand-new row (existing behaviour, unchanged by this
// split).
//
// CreateVerification vendor idempotency is PROVIDER DEPENDENT: the vendor
// idempotency key this call's phase B passes via CallContext.IdempotencyKey
// ("kv:" + the new row's own id) is honored only if the vendor supports
// idempotency keys at all. Where it does not, a player retry issued before
// the orphan from a prior attempt is known creates a second, unrelated
// vendor-side verification under a second platform row - not a financial or
// enforcement hazard (each row is evaluated independently on its own
// merits), but never silently assumed deduplicated; this must be confirmed
// and recorded at real-vendor intake (ADR 0095 §25 identity-compliance
// review).
func CreateVerification(ctx context.Context, pool providercred.TenantTxRunner, outbound OutboundCredentialResolver, provider KYCProvider, params CreateVerificationParams) (Verification, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.PersonID == uuid.Nil {
		return Verification{}, fmt.Errorf("%w: tenant_id, brand_id, player_account_id, and person_id are all required", ErrInvalidTransition)
	}
	if provider == nil {
		return Verification{}, fmt.Errorf("%w: no KYC provider configured", ErrProviderUnavailable)
	}
	if pool == nil {
		return Verification{}, fmt.Errorf("%w: KYC verification has no transaction runner configured", ErrProviderUnavailable)
	}
	providerID := provider.ID()

	// Phase A: one committed transaction for the orphan row plus its own
	// audit record - this releases the pooled connection before phase B
	// ever runs.
	var v Verification
	txErr := pool.WithTenant(ctx, params.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, err = insertOrphanVerification(ctx, tx, params, providerID)
		return err
	})
	if txErr != nil {
		return Verification{}, txErr
	}

	// Phase B: resolve the per-call outbound credential (PROV-OUTBOUND-
	// CRED-1) OUTSIDE any transaction, then call CreateVerification - the
	// one call in this function that may perform real provider I/O, with
	// no pooled connection held across it. A failure here leaves v exactly
	// as phase A committed it (see doc comment above) - it is never
	// retried automatically and never rolled back (there is nothing to
	// roll back: the orphan row is the intended, harmless artifact of this
	// failure mode).
	if outbound == nil {
		return Verification{}, fmt.Errorf("%w: no outbound credential resolver configured", ErrProviderUnavailable)
	}
	cred, err := outbound.Resolve(ctx, pool, params.TenantID, providerID)
	if err != nil {
		slog.Default().Warn("kyc_create_verification_credential_unavailable",
			"tenant_id", params.TenantID.String(), "verification_id", v.ID.String(), "provider_id", providerID)
		return Verification{}, fmt.Errorf("%w: resolve outbound credential: %v", ErrProviderUnavailable, err)
	}
	// Defense in depth (ADR 0095 §9.1/S95-C8(b), mirrors casino.LaunchGame's
	// identical check): the credential this call just resolved must bind to
	// the SAME tenant/provider/domain this verification belongs to.
	if cred.TenantID != params.TenantID || cred.ProviderID != providerID || cred.Domain != "kyc" {
		return Verification{}, fmt.Errorf("%w: outbound credential binding mismatch", ErrProviderUnavailable)
	}

	call := CallContext{
		TenantID: params.TenantID, ProviderID: providerID, Credential: cred,
		IdempotencyKey: "kv:" + v.ID.String(), Deadline: time.Now().Add(createVerificationCallTimeout),
	}
	// An explicit field-by-field literal, not a type conversion, so a field
	// later added to CreateVerificationParams is never passed to the
	// provider interface by accident.
	result, err := provider.CreateVerification(ctx, CreateVerificationInput{ //nolint:staticcheck // S1016: see comment above
		TenantID: params.TenantID, BrandID: params.BrandID,
		PlayerAccountID: params.PlayerAccountID, PersonID: params.PersonID, Call: call,
	})
	if err != nil {
		slog.Default().Warn("kyc_create_verification_provider_call_failed",
			"tenant_id", params.TenantID.String(), "verification_id", v.ID.String(), "provider_id", providerID)
		return Verification{}, fmt.Errorf("%w: create verification with provider: %v", ErrProviderUnavailable, err)
	}

	// Phase C: a short, bounded transaction applies the result under CAS.
	// context.WithoutCancel(ctx) plus phaseCTimeout, never the caller's own
	// ctx, mirroring casino.LaunchGame's identical phase-C treatment
	// (security review RV-PRH-I2 C1): the provider has already accepted
	// this verification by the time we reach here, so a cancelled request
	// context must never silently skip recording that.
	phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), phaseCTimeout)
	defer cancel()
	var applied Verification
	if err := pool.WithTenant(phaseCtx, params.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		applied, err = applyCreateVerificationResult(ctx, tx, params.TenantID, v, result)
		return err
	}); err != nil {
		slog.Default().Error("kyc_create_verification_phase_c_failed",
			"tenant_id", params.TenantID.String(), "verification_id", v.ID.String(), "error", err.Error())
		return Verification{}, fmt.Errorf("kyc: create verification: apply result: %w", err)
	}
	return applied, nil
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

// applyForwardOnlyStatus (provider.go) is this package's own provider-
// driven, not staff-driven, status writer - both the Orchestrator's
// callback-application step (via applyCallbackOutcome) and
// SubmitVerification's own phase C (RV-PRH-I2 KYC R1 fix) apply a status
// under its forward-only CAS rule; reviewed_at/reviewed_by are deliberately
// left untouched by both - those two columns record a STAFF review
// specifically (ReviewVerification below), never an automated provider
// decision - see migration 0040's own column comments. There is no longer a
// blind, non-CAS status writer in this package (the R1 finding: a blind
// UPDATE here is exactly what let a concurrent staff decision or callback
// be silently overwritten during SubmitVerification's own provider
// round-trip).

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

// reviewVerificationTestRaceHook is a TEST-ONLY seam (always nil in
// production - never set outside test code) - see its call site inside
// ReviewVerification for what it is for.
var reviewVerificationTestRaceHook func(verificationID uuid.UUID)

// ReviewVerification is the STAFF-driven review action
// (PermVerificationReview) - distinct from the Orchestrator's own
// provider-driven applyForwardOnlyStatus: this one always stamps
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
	var reasonTruncated bool
	params.Reason, reasonTruncated = NormalizeReason(params.Reason)

	current, err := GetVerificationByID(ctx, tx, params.VerificationID)
	if err != nil {
		return Verification{}, err
	}
	if isTerminal(current.Status) {
		return Verification{}, fmt.Errorf("%w: verification %s is already in a terminal status %q", ErrInvalidTransition, params.VerificationID, current.Status)
	}
	// reviewVerificationTestRaceHook: TEST-ONLY (nil in production, never
	// set outside test code). Lets a test commit a concurrent transition -
	// a SubmitVerification phase C, or a verified callback - in the exact
	// window between this call's own read (current, above) and its CAS
	// write below, reproducing the mirror-race finding directly (RV-PRH-I2
	// KYC code review), the same way the R1 P1/P2 tests use
	// spyKYCProvider.onSubmitVerification to commit a concurrent decision
	// mid-provider-call.
	if reviewVerificationTestRaceHook != nil {
		reviewVerificationTestRaceHook(params.VerificationID)
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

	// Mirror race fix (RV-PRH-I2 KYC code review, surfaced finding): this
	// write is now a CAS on `current.Status`, the status THIS call's own
	// read above just saw - never a blind, unconditional UPDATE. A
	// concurrent SubmitVerification phase C or verified callback committing
	// between that read and this write makes the WHERE clause miss (zero
	// rows, surfaced by QueryRow as pgx.ErrNoRows below), and this call
	// fails closed with ErrVerificationStatusConflict - it never retries
	// and never silently no-ops, because a staff decision is deliberate,
	// not a replayable provider transition (see ErrVerificationStatusConflict's
	// own doc comment).
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
		 WHERE kyc_verifications.id = $4 AND kyc_verifications.status = $6
		RETURNING (prev.before_value IS NOT NULL) AS had_previous,
		          (prev.before_value IS DISTINCT FROM COALESCE($5, prev.before_value)) AS changed,
		          kyc_verifications.verified_residence_set_at`,
		params.NewStatus, params.Reason, params.StaffID, params.VerificationID, params.VerifiedResidenceCountry, current.Status,
	).Scan(&hadPreviousResidence, &residenceChanged, &newSetAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, fmt.Errorf("%w: verification %s (expected status %q)", ErrVerificationStatusConflict, params.VerificationID, current.Status)
	}
	if err != nil {
		return Verification{}, fmt.Errorf("kyc: review verification: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: current.TenantID, ActorType: audit.ActorStaff, ActorID: params.StaffID,
		Action: "kyc.verification_status_changed", TargetType: "kyc_verification", TargetID: params.VerificationID.String(),
		Outcome:   audit.OutcomeSuccess,
		IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: withReasonTruncated(map[string]any{"previous_status": string(current.Status), "new_status": string(params.NewStatus), "reason": params.Reason}, reasonTruncated),
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
