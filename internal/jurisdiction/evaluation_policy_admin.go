package jurisdiction

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// This file implements PC-GAP-4's write path (docs/decisions/0043-
// jurisdiction-evaluation-policy-configuration.md Decision 3):
// authoring/superseding/withdrawing a jurisdiction_precedence_configs
// VERSION. It deliberately builds the
// authoring and supersession mechanism only - activating a policy
// (status = 'active') is refused at this sanctioned Go write path
// (ErrActivationNotAuthorized): that requires HDR-J-8/HDR-J-9 content, its
// own permission, and a ruling on dual control, none of which exist yet.
// NOTE: migration 0075 imposes no database-level guard against
// status = 'active' (the CHECK constraint and RLS INSERT policy both admit
// it for any platform-admin-scoped writer) - the refusal here is an
// application-layer control, not a structural/schema-level one. A future
// activation phase adding a second writer must not assume the database
// itself blocks this.
//
// Every function here requires a transaction opened by
// db.Pool.WithPlatformAdmin - migration 0075's RLS policies on
// jurisdiction_precedence_configs only accept INSERT/UPDATE when
// app.platform_admin_principal_id is set AND app.tenant_id/
// app.player_account_id are unset, so a tenant-scoped transaction fails
// at the database regardless of what this Go code does. assertPlatformScope
// below is a REAL, in-function control, not defence-in-depth commentary -
// it is what turns a mis-scoped transaction into a diagnosable
// ErrTransactionScope instead of an opaque RLS failure surfacing from deep
// inside a QueryRow call.
//
// There is NO `ON CONFLICT DO UPDATE` anywhere in this file. That is
// deliberate: it is the exact upsert pattern whose provenance defect
// (PHASE-B-ARCH-1, resolution_active.go/evidence_collection_active.go)
// this table's append-only shape is built to avoid - every change of the
// policy in force is a NEW row, never a stamped-over one.

// ErrActivationNotAuthorized is returned when a caller attempts to write
// an ACTIVE evaluation policy version. Stage 4I Phase D deliberately
// builds the authoring and supersession mechanism only: activating a
// policy requires HDR-J-8/HDR-J-9 content, its own permission, and a
// ruling on dual control (HDR-J-2's recorded technical consequence), none
// of which exist. The database representation of 'active' exists so that
// the phase which has those answers adds a writer, not a migration.
var ErrActivationNotAuthorized = errors.New("jurisdiction: writing an active evaluation policy version is not authorized in this phase (HDR-J-8/HDR-J-9 undecided; activation requires its own permission and a dual-control ruling)")

// ErrConcurrentPolicyWrite is returned when another transaction wrote a
// version for the same (licensing jurisdiction, operation class)
// concurrently. The caller may retry; nothing was written.
var ErrConcurrentPolicyWrite = errors.New("jurisdiction: a concurrent evaluation policy write for this licensing jurisdiction and operation class was detected; nothing was written")

// ErrTransactionScope is returned when tx is not a genuinely
// platform-scoped transaction (app.platform_admin_principal_id set AND
// app.tenant_id/app.player_account_id both unset) - a SERVER-side caller
// bug, not a client input error, mirroring
// identity.ErrTransactionScope/PHASE-B-ARCH-1-SEC-5's exact reasoning:
// this is deliberately not wrapped in ErrInvalidInput so the two failure
// classes can be alerted on differently.
var ErrTransactionScope = errors.New("jurisdiction: requires a platform-admin-scoped transaction (use db.Pool.WithPlatformAdmin)")

// assertPlatformScope is the write-side analogue of assertTenantScope
// (resolver.go) and of identity.SetPlayerAccountDeclaredResidence's own
// connection-scope assertion: it reads the three relevant GUCs in ONE
// query and requires app.platform_admin_principal_id to be set AND both
// app.tenant_id and app.player_account_id to be unset. This is a REAL
// control - migration 0075's RLS policies enforce the identical predicate
// independently at the database, so a caller that bypassed this check
// would still fail at the INSERT/UPDATE, but with a much less diagnosable
// error.
func assertPlatformScope(ctx context.Context, tx pgx.Tx) error {
	var platformAdmin *uuid.UUID
	var scopedTenant *uuid.UUID
	var scopedPlayer *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid,
		        NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&platformAdmin, &scopedTenant, &scopedPlayer); err != nil {
		return fmt.Errorf("jurisdiction: read platform admin scope: %w", err)
	}
	if platformAdmin == nil || scopedTenant != nil || scopedPlayer != nil {
		return ErrTransactionScope
	}
	return nil
}

// CreateEvaluationPolicyVersionParams is CreateEvaluationPolicyVersion's
// input. Deliberately absent: effective_from (DB-set, migration 0075's
// stamp-times trigger), effective_to (never caller-set), resolver_policy_
// version and precedence_policy_version (compiled-in constants),
// created_by_actor_type (always 'staff'), and any precedence content
// (HDR-J-2 blocked).
type CreateEvaluationPolicyVersionParams struct {
	LicensingJurisdictionID   uuid.UUID
	OperationClass            OperationClass
	Status                    EvaluationPolicyStatus // draft | withdrawn only
	LocationSignalRequirement LocationRequirement    // "" == unset
	MaxLocationSignalAge      *time.Duration         // nil == unset
	LegalReviewReference      string                 // optional for draft
	Actor                     ActorContext
}

// CreateEvaluationPolicyVersion appends a new version for
// (licensing jurisdiction, operation class), closing the currently-open
// version in the SAME transaction, and writes an audit_log entry in that
// same transaction. tx MUST be a platform-admin-scoped transaction
// (db.Pool.WithPlatformAdmin) - asserted in-function AND enforced by
// migration 0075's INSERT/UPDATE RLS policies.
//
// Call this AT MOST ONCE per transaction for a given key. A second call
// for the same (licensing jurisdiction, operation class) inside the SAME
// transaction will surface as ErrConcurrentPolicyWrite even though no
// concurrent writer exists: every close/insert in one transaction shares
// that transaction's single now(), so the second call's close attempt
// lands at effective_to == effective_from and trips the
// effective_to > effective_from CHECK (security re-verification,
// Stage 4I Phase D fix round) - the same 23514 outcome a genuine
// cross-transaction race also produces (see the close-UPDATE's own
// comment below), which this function cannot distinguish from a caller
// bug without additional bookkeeping. Callers needing to author two
// versions must do so in two separate transactions.
func CreateEvaluationPolicyVersion(ctx context.Context, tx pgx.Tx, p CreateEvaluationPolicyVersionParams) (EvaluationPolicyRecord, error) {
	if err := p.Actor.validate(); err != nil {
		return EvaluationPolicyRecord{}, err
	}
	if err := assertPlatformScope(ctx, tx); err != nil {
		return EvaluationPolicyRecord{}, err
	}
	if p.LicensingJurisdictionID == uuid.Nil {
		return EvaluationPolicyRecord{}, fmt.Errorf("%w: licensing_jurisdiction_id is required", ErrInvalidInput)
	}
	if !validOperationClass(p.OperationClass) {
		return EvaluationPolicyRecord{}, fmt.Errorf("%w: unknown operation_class %q", ErrInvalidInput, p.OperationClass)
	}
	// Activation is refused as its own, distinct, earlier check - this is
	// an authorization decision (HDR-J-8/HDR-J-9 undecided; no permission or
	// dual-control ruling exists yet), not a "is this a recognized status
	// string" question. validEvaluationPolicyStatus below only confirms the
	// latter, so there is exactly one source of truth for "is this a known
	// enum value at all" shared with the read path (evaluation_policy.go).
	if p.Status == EvaluationPolicyActive {
		return EvaluationPolicyRecord{}, ErrActivationNotAuthorized
	}
	if !validEvaluationPolicyStatus(p.Status) {
		return EvaluationPolicyRecord{}, fmt.Errorf("%w: status must be draft or withdrawn", ErrInvalidInput)
	}

	locationRequirementDB, err := formatLocationRequirement(p.LocationSignalRequirement)
	if err != nil {
		return EvaluationPolicyRecord{}, err
	}

	var maxAgeSeconds *int64
	if p.MaxLocationSignalAge != nil {
		d := *p.MaxLocationSignalAge
		if d <= 0 || d%time.Second != 0 {
			return EvaluationPolicyRecord{}, fmt.Errorf("%w: max_location_signal_age must be a positive whole number of seconds", ErrInvalidInput)
		}
		secs := int64(d / time.Second)
		maxAgeSeconds = &secs
	}

	if p.Status == EvaluationPolicyWithdrawn {
		if p.LocationSignalRequirement != LocationRequirementUnset || p.MaxLocationSignalAge != nil {
			return EvaluationPolicyRecord{}, fmt.Errorf("%w: a withdrawal carries no policy content", ErrInvalidInput)
		}
	}

	legalReviewReference := strings.TrimSpace(p.LegalReviewReference)

	// Lock the latest version for this key (if any) against concurrent
	// writers. This is an optimisation, not the authoritative control -
	// the authoritative control is the partial unique index
	// uq_jurisdiction_precedence_configs_open plus the effective_to >
	// effective_from CHECK, both enforced at INSERT time regardless of
	// this lock's outcome.
	var latestID *uuid.UUID
	var latestEffectiveFrom *time.Time
	var latestEffectiveTo *time.Time
	err = tx.QueryRow(ctx, `
		SELECT id, effective_from, effective_to FROM jurisdiction_precedence_configs
		 WHERE licensing_jurisdiction_id = $1 AND operation_class = $2
		 ORDER BY effective_from DESC LIMIT 1 FOR UPDATE`,
		p.LicensingJurisdictionID, string(p.OperationClass),
	).Scan(&latestID, &latestEffectiveFrom, &latestEffectiveTo)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return EvaluationPolicyRecord{}, fmt.Errorf("jurisdiction: lock latest evaluation policy version: %w", err)
	}

	// latestID is the LATEST version regardless of whether it is still
	// open; latestOpen is set only when that latest version has not yet
	// been closed (effective_to IS NULL).
	var latestOpen *uuid.UUID
	if latestID != nil && latestEffectiveTo == nil {
		latestOpen = latestID
	}

	if p.Status == EvaluationPolicyWithdrawn && latestOpen == nil {
		return EvaluationPolicyRecord{}, fmt.Errorf("%w: nothing to withdraw for this licensing jurisdiction and operation class", ErrInvalidInput)
	}

	var beforeState *EvaluationPolicyRecord
	if latestOpen != nil {
		before, err := readEvaluationPolicyRecordByID(ctx, tx, *latestOpen)
		if err != nil {
			return EvaluationPolicyRecord{}, err
		}
		beforeState = &before

		tag, err := tx.Exec(ctx,
			`UPDATE jurisdiction_precedence_configs SET effective_to = now() WHERE id = $1 AND effective_to IS NULL`,
			*latestOpen)
		if err != nil {
			// Under genuine contention, a second transaction's row may have
			// been inserted (effective_from = T_B) after this transaction
			// began (T_A < T_B); this transaction's own close attempt
			// (effective_to = now() ~= T_A) can then violate the
			// effective_to > effective_from CHECK (23514) rather than the
			// 23505 unique-index path below. The outcome is equally safe
			// (nothing written, transaction rolls back) and equally
			// retryable, so it maps to the same ErrConcurrentPolicyWrite
			// signal instead of surfacing as an opaque wrapped error.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" {
				return EvaluationPolicyRecord{}, ErrConcurrentPolicyWrite
			}
			return EvaluationPolicyRecord{}, fmt.Errorf("jurisdiction: close prior evaluation policy version: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return EvaluationPolicyRecord{}, ErrConcurrentPolicyWrite
		}
	}

	var (
		newID         uuid.UUID
		effectiveFrom time.Time
		createdAt     time.Time
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO jurisdiction_precedence_configs (
			licensing_jurisdiction_id, operation_class, precedence, status,
			location_requirement, max_location_signal_age_seconds,
			precedence_status, resolver_policy_version, precedence_policy_version,
			legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id
		) VALUES (
			$1, $2, '[]'::jsonb, $3,
			$4, $5,
			'unset', $6, $7,
			NULLIF($8, ''), $9, 'staff', $10
		)
		RETURNING id, effective_from, created_at`,
		p.LicensingJurisdictionID, string(p.OperationClass), string(p.Status),
		locationRequirementDB, maxAgeSeconds,
		PolicyVersion, PrecedencePolicyVersion,
		legalReviewReference, p.Actor.ReasonCode, p.Actor.ActorID,
	).Scan(&newID, &effectiveFrom, &createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return EvaluationPolicyRecord{}, ErrConcurrentPolicyWrite
		}
		return EvaluationPolicyRecord{}, fmt.Errorf("jurisdiction: insert evaluation policy version: %w", err)
	}

	newState := EvaluationPolicyRecord{
		ID:                        newID,
		LicensingJurisdictionID:   p.LicensingJurisdictionID,
		OperationClass:            p.OperationClass,
		Status:                    p.Status,
		LocationSignalRequirement: p.LocationSignalRequirement,
		MaxLocationSignalAge:      p.MaxLocationSignalAge,
		PrecedenceConfigured:      false,
		ResolverPolicyVersion:     PolicyVersion,
		PrecedencePolicyVersion:   PrecedencePolicyVersion,
		EffectiveFrom:             effectiveFrom,
		LegalReviewReference:      legalReviewReference,
		ReasonCode:                p.Actor.ReasonCode,
		CreatedByActorType:        ActorStaff,
		CreatedByActorID:          p.Actor.ActorID,
		CreatedAt:                 createdAt,
	}

	var beforeMetadata any
	if beforeState != nil {
		beforeMetadata = evaluationPolicyRecordState(*beforeState)
	}

	if err := recordRegistryAudit(ctx, tx, uuid.Nil, p.Actor, "jurisdiction_evaluation_policy.version_created", "jurisdiction_precedence_configs", newID.String(), map[string]any{
		"before":                    beforeMetadata,
		"after":                     evaluationPolicyRecordState(newState),
		"licensing_jurisdiction_id": p.LicensingJurisdictionID.String(),
		"operation_class":           string(p.OperationClass),
	}); err != nil {
		return EvaluationPolicyRecord{}, err
	}

	return newState, nil
}

func readEvaluationPolicyRecordByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (EvaluationPolicyRecord, error) {
	var (
		rec                   EvaluationPolicyRecord
		oc, status            string
		locationRequirementDB string
		maxAgeSeconds         *int64
		precedenceStatus      string
		actorType             string
	)
	err := tx.QueryRow(ctx, `
		SELECT id, licensing_jurisdiction_id, operation_class, status, location_requirement,
		       max_location_signal_age_seconds, precedence_status, resolver_policy_version,
		       precedence_policy_version, effective_from, effective_to,
		       COALESCE(legal_review_reference, ''), reason_code,
		       created_by_actor_type, created_by_actor_id, created_at
		  FROM jurisdiction_precedence_configs WHERE id = $1`, id,
	).Scan(
		&rec.ID, &rec.LicensingJurisdictionID, &oc, &status, &locationRequirementDB,
		&maxAgeSeconds, &precedenceStatus, &rec.ResolverPolicyVersion,
		&rec.PrecedencePolicyVersion, &rec.EffectiveFrom, &rec.EffectiveTo,
		&rec.LegalReviewReference, &rec.ReasonCode,
		&actorType, &rec.CreatedByActorID, &rec.CreatedAt,
	)
	if err != nil {
		return EvaluationPolicyRecord{}, fmt.Errorf("jurisdiction: read evaluation policy version %s: %w", id, err)
	}
	rec.OperationClass = OperationClass(oc)
	rec.Status = EvaluationPolicyStatus(status)
	locationRequirement, err := parseLocationRequirement(locationRequirementDB)
	if err != nil {
		return EvaluationPolicyRecord{}, fmt.Errorf("jurisdiction: stored location_requirement is invalid: %w", err)
	}
	rec.LocationSignalRequirement = locationRequirement
	if maxAgeSeconds != nil {
		d := time.Duration(*maxAgeSeconds) * time.Second
		rec.MaxLocationSignalAge = &d
	}
	rec.PrecedenceConfigured = precedenceStatus == "configured"
	rec.CreatedByActorType = ActorType(actorType)
	return rec, nil
}

// evaluationPolicyRecordState renders an EvaluationPolicyRecord as
// audit-log-friendly before/after state. These are jurisdiction-level
// POLICY VALUES (location_requirement/max_location_signal_age_seconds/
// status/etc.), never player evidence - canonical-model §5.3's "never the
// evidence VALUES" rule governs player-jurisdiction determinations, not
// administrative policy configuration, so before/after here is required
// by CLAUDE.md's audit rule, not excluded by §5.3.
func evaluationPolicyRecordState(r EvaluationPolicyRecord) map[string]any {
	var maxAgeSeconds any
	if r.MaxLocationSignalAge != nil {
		maxAgeSeconds = int64(*r.MaxLocationSignalAge / time.Second)
	}
	var effectiveTo any
	if r.EffectiveTo != nil {
		effectiveTo = r.EffectiveTo.Format(time.RFC3339Nano)
	}
	return map[string]any{
		"id":                          r.ID.String(),
		"status":                      string(r.Status),
		"location_requirement":        string(r.LocationSignalRequirement),
		"max_location_signal_age_sec": maxAgeSeconds,
		"precedence_configured":       r.PrecedenceConfigured,
		"resolver_policy_version":     r.ResolverPolicyVersion,
		"precedence_policy_version":   r.PrecedencePolicyVersion,
		"effective_from":              r.EffectiveFrom.Format(time.RFC3339Nano),
		"effective_to":                effectiveTo,
		"legal_review_reference":      r.LegalReviewReference,
	}
}
