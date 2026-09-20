package jurisdiction

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// This file implements PC-GAP-4's read path (docs/decisions/0043-
// jurisdiction-evaluation-policy-configuration.md Decision 2): resolving
// the ACTIVE jurisdiction_precedence_configs version in force for a
// TENANT's own licensing jurisdiction and operation class, at an explicit
// AsOf. It never reads jurisdiction_resolution_active or
// jurisdiction_evidence_collection_active (ADR 0043 Decision 6 - three
// independent switches, never coupled here).
//
// A nil error from ResolveEvaluationPolicy means a row is in force,
// active, and version-matched - it does NOT mean the policy's fields are
// all decided. A field-level `unset` value (LocationRequirementUnset with
// a nil MaxLocationSignalAge - the zero EvaluationPolicy) is returned WITH
// a nil error BY DESIGN: it must be distinguishable from key-level unset
// (ErrPolicyNotConfigured) and fails closed only downstream, at
// DeterminePlayerJurisdiction, via ErrPolicyUnset. No caller may treat a
// nil error from ResolveEvaluationPolicy as license to skip feeding the
// result through DeterminePlayerJurisdiction. Every OTHER non-active,
// non-configured, mismatched-version, or unknown-licensing-jurisdiction
// condition IS a distinguishable error, and every one of those fails
// closed.

// EvaluationPolicyLookup is ResolveEvaluationPolicy's input. It is
// deliberately TENANT-keyed, not jurisdiction-keyed: the licensing
// jurisdiction is derived server-side from tenants.licence_id ->
// licences.jurisdiction_id (canonical-model §3.4), so no caller - and in
// particular no request body - can ever name the jurisdiction whose
// policy is read.
type EvaluationPolicyLookup struct {
	TenantID       uuid.UUID
	OperationClass OperationClass
	// AsOf selects the effective-dated version and MUST be the same value
	// the caller later passes as DetermineParams.AsOf. This package never
	// calls time.Now() on this path and the selection SQL never calls
	// now().
	AsOf time.Time
}

// EvaluationPolicyProvenance identifies WHICH configuration version
// produced a policy, for the resolution record (canonical-model §5.2's
// config_effective_from) and for incident reconstruction.
//
// It deliberately carries NO jurisdiction id and NO jurisdiction code.
// Nothing that flows out of a configuration read may be mistakable for a
// player's own resolved jurisdiction - that is the structural half of
// HDR-J-1's guarantee on this path (docs/decisions/0043-jurisdiction-
// evaluation-policy-configuration.md Decision 2).
type EvaluationPolicyProvenance struct {
	ConfigID                uuid.UUID
	EffectiveFrom           time.Time
	PrecedencePolicyVersion string
}

var (
	// ErrPolicyNotConfigured means no version is in force at all for this
	// (licensing jurisdiction, operation class) key at AsOf - a key-level
	// unset, distinct from ErrPolicyNotActive (a version exists but its
	// status is not 'active') and from precedence.go's own field-level
	// ErrPolicyUnset.
	ErrPolicyNotConfigured = errors.New("jurisdiction: no evaluation policy version is in force for this licensing jurisdiction and operation class")
	// ErrPolicyNotActive means a version IS in force but its status is
	// 'draft' or 'withdrawn', not 'active'.
	ErrPolicyNotActive = errors.New("jurisdiction: the evaluation policy version in force is not active")
	// ErrPolicyVersionMismatch means the active version in force was
	// authored against a DIFFERENT PrecedencePolicyVersion than the one
	// compiled into this binary - see ResolveEvaluationPolicy's own doc
	// comment for the operational consequence.
	ErrPolicyVersionMismatch = errors.New("jurisdiction: the active evaluation policy was authored against a different precedence policy version and must be re-approved before it may govern")
	// ErrLicensingJurisdictionUnknown means the tenant has no bound
	// licence, or the bound licence does not resolve to a jurisdiction (a
	// registry integrity problem, diagnosable apart from an ordinary data
	// gap - canonical-model §2.2).
	ErrLicensingJurisdictionUnknown = errors.New("jurisdiction: this tenant's licensing jurisdiction is not determinable")
)

// parseLocationRequirement maps the DB vocabulary to the Go enum with an
// exhaustive switch and a REJECTING default. A raw conversion would turn
// the DB value "unset" into LocationRequirement("unset"), which is neither
// LocationRequirementUnset nor a recognized value - DeterminePlayerJurisdiction
// would then reject it as ErrInvalidInput instead of ErrPolicyUnset,
// reporting a caller bug where the truth is an unmade policy decision.
func parseLocationRequirement(s string) (LocationRequirement, error) {
	switch s {
	case "unset":
		return LocationRequirementUnset, nil
	case "required":
		return LocationRequired, nil
	case "advisory":
		return LocationAdvisory, nil
	default:
		return "", fmt.Errorf("%w: unrecognized location_requirement %q", ErrInvalidInput, s)
	}
}

// formatLocationRequirement is parseLocationRequirement's inverse, used by
// the write path (evaluation_policy_admin.go). Also exhaustive with a
// rejecting default.
func formatLocationRequirement(r LocationRequirement) (string, error) {
	switch r {
	case LocationRequirementUnset:
		return "unset", nil
	case LocationRequired:
		return "required", nil
	case LocationAdvisory:
		return "advisory", nil
	default:
		return "", fmt.Errorf("%w: unrecognized LocationRequirement %q", ErrInvalidInput, r)
	}
}

// ResolveEvaluationPolicy returns the ACTIVE evaluation policy in force
// at AsOf for the tenant's own licensing jurisdiction and the given
// operation class. Every non-active, non-configured, mismatched-version
// or unknown-jurisdiction condition is a distinguishable error, and every
// one of them fails closed.
//
// A nil error does NOT mean the returned policy's fields are all decided:
// a field-level `unset` value (the zero EvaluationPolicy) is returned WITH
// a nil error by design, to be distinguishable from the key-level
// ErrPolicyNotConfigured, and fails closed only downstream, at
// DeterminePlayerJurisdiction, via ErrPolicyUnset. Callers must always
// feed the result through DeterminePlayerJurisdiction rather than
// inspecting the error alone.
func ResolveEvaluationPolicy(ctx context.Context, q ReadOnlyQuerier, p EvaluationPolicyLookup) (EvaluationPolicy, EvaluationPolicyProvenance, error) {
	if p.TenantID == uuid.Nil {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if !validOperationClass(p.OperationClass) {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: unknown operation_class %q", ErrInvalidInput, p.OperationClass)
	}
	if p.AsOf.IsZero() {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: AsOf is required and must not be the zero time", ErrInvalidInput)
	}

	inScope, err := assertTenantScope(ctx, q, p.TenantID)
	if err != nil {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, err
	}
	if !inScope {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: transaction scope does not match tenant %s", ErrScopeMismatch, p.TenantID)
	}

	var licenceID *uuid.UUID
	err = q.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, p.TenantID).Scan(&licenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: tenant %s has no licence bound", ErrLicensingJurisdictionUnknown, p.TenantID)
	}
	if err != nil {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("jurisdiction: read tenant licence: %w", err)
	}
	if licenceID == nil {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: tenant %s has no licence bound", ErrLicensingJurisdictionUnknown, p.TenantID)
	}

	var licensingJurisdictionID uuid.UUID
	err = q.QueryRow(ctx, `SELECT jurisdiction_id FROM licences WHERE id = $1`, *licenceID).Scan(&licensingJurisdictionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: licence %s does not resolve to a jurisdiction", ErrLicensingJurisdictionUnknown, *licenceID)
	}
	if err != nil {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("jurisdiction: read licence jurisdiction: %w", err)
	}

	var (
		configID              uuid.UUID
		status                string
		locationRequirementDB string
		maxAgeSeconds         *int64
		precedencePolicyVer   string
		effectiveFrom         time.Time
	)
	// ORDER BY + LIMIT 1: the partial unique index and both append-only
	// triggers make more than one matching row a schema-integrity failure
	// that should never occur via any sanctioned write path - but a raw,
	// non-sanctioned platform-admin SQL writer bypassing
	// CreateEvaluationPolicyVersion could construct one (see the
	// migration's own trigger-comment note on this narrow, non-sanctioned
	// residual). Ordering makes resolution deterministic (the
	// most-recently-opened version governs) rather than picking whichever
	// row the query planner happens to return first, in that scenario.
	err = q.QueryRow(ctx, `
		SELECT id, status, location_requirement, max_location_signal_age_seconds,
		       precedence_policy_version, effective_from
		  FROM jurisdiction_precedence_configs
		 WHERE licensing_jurisdiction_id = $1
		   AND operation_class = $2
		   AND effective_from <= $3
		   AND (effective_to IS NULL OR effective_to > $3)
		 ORDER BY effective_from DESC
		 LIMIT 1`,
		licensingJurisdictionID, string(p.OperationClass), p.AsOf,
	).Scan(&configID, &status, &locationRequirementDB, &maxAgeSeconds, &precedencePolicyVer, &effectiveFrom)
	if errors.Is(err, pgx.ErrNoRows) {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, ErrPolicyNotConfigured
	}
	if err != nil {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("jurisdiction: read evaluation policy config: %w", err)
	}

	if status != "active" {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: status is %q", ErrPolicyNotActive, status)
	}
	if precedencePolicyVer != PrecedencePolicyVersion {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("%w: config authored against %q, running %q", ErrPolicyVersionMismatch, precedencePolicyVer, PrecedencePolicyVersion)
	}

	locationRequirement, err := parseLocationRequirement(locationRequirementDB)
	if err != nil {
		return EvaluationPolicy{}, EvaluationPolicyProvenance{}, fmt.Errorf("jurisdiction: stored location_requirement is invalid: %w", err)
	}

	var maxAge *time.Duration
	if maxAgeSeconds != nil {
		d := time.Duration(*maxAgeSeconds) * time.Second
		maxAge = &d
	}

	policy := EvaluationPolicy{
		LocationSignalRequirement: locationRequirement,
		MaxLocationSignalAge:      maxAge,
	}
	provenance := EvaluationPolicyProvenance{
		ConfigID:                configID,
		EffectiveFrom:           effectiveFrom,
		PrecedencePolicyVersion: precedencePolicyVer,
	}
	return policy, provenance, nil
}

// EvaluationPolicyStatus mirrors jurisdiction_precedence_configs.status.
type EvaluationPolicyStatus string

const (
	EvaluationPolicyDraft     EvaluationPolicyStatus = "draft"
	EvaluationPolicyActive    EvaluationPolicyStatus = "active"
	EvaluationPolicyWithdrawn EvaluationPolicyStatus = "withdrawn"
)

func validEvaluationPolicyStatus(s EvaluationPolicyStatus) bool {
	switch s {
	case EvaluationPolicyDraft, EvaluationPolicyActive, EvaluationPolicyWithdrawn:
		return true
	default:
		return false
	}
}

// EvaluationPolicyRecord is one jurisdiction_precedence_configs row as
// seen by an administrative/diagnostic reader.
//
// It exposes PrecedenceConfigured (a bool) and NEVER the precedence
// array's content: that content is blocked on HDR-J-2, and a read path
// for content that may not exist yet is a surface a future caller would
// consume half-built.
//
// Diagnostic/admin-only shape: its LocationSignalRequirement and
// MaxLocationSignalAge fields may come from a `draft` or `withdrawn` row,
// not just the one `active` version in force. They must NEVER be lifted
// directly into an EvaluationPolicy by any caller - ResolveEvaluationPolicy
// is the only sanctioned source of an enforcement-usable EvaluationPolicy.
type EvaluationPolicyRecord struct {
	ID                        uuid.UUID
	LicensingJurisdictionID   uuid.UUID
	OperationClass            OperationClass
	Status                    EvaluationPolicyStatus
	LocationSignalRequirement LocationRequirement
	MaxLocationSignalAge      *time.Duration
	PrecedenceConfigured      bool
	ResolverPolicyVersion     string
	PrecedencePolicyVersion   string
	EffectiveFrom             time.Time
	EffectiveTo               *time.Time
	LegalReviewReference      string
	ReasonCode                string
	CreatedByActorType        ActorType
	CreatedByActorID          uuid.UUID
	CreatedAt                 time.Time
}

// ListEvaluationPolicyVersions returns the full, append-only version
// history for one (licensing jurisdiction, operation class), newest
// first. Diagnostics / future admin surface only - never an enforcement
// path (enforcement uses ResolveEvaluationPolicy for exactly one
// in-force version), mirroring ListResolutionActive's own doc comment.
//
// tx MUST be a platform-admin-scoped transaction (db.Pool.WithPlatformAdmin)
// - asserted in-function via assertPlatformScope. Without this assertion,
// a tenant- or player-scoped Go CALLER OF THIS FUNCTION could enumerate
// another licensing jurisdiction's full version history (reason_code,
// legal_review_reference, created_by_actor_id/type included), which
// contradicts ADR 0043 Decision 2's "the caller never handles a licensing
// jurisdiction id at all" principle. This assertion closes that path for
// Go callers of this function ONLY - it does NOT narrow migration 0075's
// own deliberately permissive `FOR SELECT USING (true)` RLS policy on
// jurisdiction_precedence_configs, which still permits any tenant- or
// player-scoped CONNECTION to read this table directly via raw SQL
// (code-reviewer's Phase D finding). Whether that read policy should be
// narrowed (e.g. to the in-force row only, or excluding provenance
// columns) is a `security`-owned decision, not decided by this function.
//
// Returned records may carry LocationSignalRequirement/
// MaxLocationSignalAge from a `draft` or `withdrawn` row - see
// EvaluationPolicyRecord's own doc comment: this function is diagnostic/
// admin-only and its results must never be lifted directly into an
// EvaluationPolicy.
func ListEvaluationPolicyVersions(ctx context.Context, tx pgx.Tx, licensingJurisdictionID uuid.UUID, operationClass OperationClass) ([]EvaluationPolicyRecord, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return nil, err
	}
	if licensingJurisdictionID == uuid.Nil {
		return nil, fmt.Errorf("%w: licensing_jurisdiction_id is required", ErrInvalidInput)
	}
	if !validOperationClass(operationClass) {
		return nil, fmt.Errorf("%w: unknown operation_class %q", ErrInvalidInput, operationClass)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, licensing_jurisdiction_id, operation_class, status, location_requirement,
		       max_location_signal_age_seconds, precedence_status, resolver_policy_version,
		       precedence_policy_version, effective_from, effective_to,
		       COALESCE(legal_review_reference, ''), reason_code,
		       created_by_actor_type, created_by_actor_id, created_at
		  FROM jurisdiction_precedence_configs
		 WHERE licensing_jurisdiction_id = $1 AND operation_class = $2
		 ORDER BY effective_from DESC`,
		licensingJurisdictionID, string(operationClass))
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: list evaluation policy versions: %w", err)
	}
	defer rows.Close()

	var out []EvaluationPolicyRecord
	for rows.Next() {
		var (
			rec                   EvaluationPolicyRecord
			oc, status            string
			locationRequirementDB string
			maxAgeSeconds         *int64
			precedenceStatus      string
			actorType             string
		)
		if err := rows.Scan(
			&rec.ID, &rec.LicensingJurisdictionID, &oc, &status, &locationRequirementDB,
			&maxAgeSeconds, &precedenceStatus, &rec.ResolverPolicyVersion,
			&rec.PrecedencePolicyVersion, &rec.EffectiveFrom, &rec.EffectiveTo,
			&rec.LegalReviewReference, &rec.ReasonCode,
			&actorType, &rec.CreatedByActorID, &rec.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("jurisdiction: scan evaluation policy version: %w", err)
		}
		rec.OperationClass = OperationClass(oc)
		rec.Status = EvaluationPolicyStatus(status)
		locationRequirement, err := parseLocationRequirement(locationRequirementDB)
		if err != nil {
			return nil, fmt.Errorf("jurisdiction: stored location_requirement is invalid: %w", err)
		}
		rec.LocationSignalRequirement = locationRequirement
		if maxAgeSeconds != nil {
			d := time.Duration(*maxAgeSeconds) * time.Second
			rec.MaxLocationSignalAge = &d
		}
		rec.PrecedenceConfigured = precedenceStatus == "configured"
		rec.CreatedByActorType = ActorType(actorType)
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("jurisdiction: iterate evaluation policy versions: %w", err)
	}
	return out, nil
}
