package operatingmarket

// This file implements the licence-ceiling write path (ADR 0045 §3.4,
// PLATFORM scope) - the statement of which countries a LICENCE permits at
// all. Every change is a NEW row via close-open-then-insert in ONE
// transaction; there is NO `ON CONFLICT DO UPDATE` anywhere in this file
// (the exact PHASE-B-ARCH-1 provenance defect this package is built to
// avoid).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/validation"
)

// CreateLicenceCountryCeilingVersionParams is
// CreateLicenceCountryCeilingVersion's input. Deliberately absent:
// effective_from (DB-set, migration 0076's stamp-times trigger),
// effective_to (never caller-set), policy_version (compiled-in constant),
// created_by_actor_type (always 'staff').
type CreateLicenceCountryCeilingVersionParams struct {
	LicenceID   uuid.UUID
	CountryCode string // ISO-3166-1 alpha-2, validated, never coerced
	State       State
	Status      Status
	// AuthorizationReference is REQUIRED iff State==StateEnabled &&
	// Status==StatusActive (ocp/ceiling CHECK constraint's own asymmetry -
	// an ENABLE can never be recorded without a named authorization; a
	// DISABLE never needs one).
	AuthorizationReference string
	Actor                  ActorContext
}

// CreateLicenceCountryCeilingVersion appends a new ceiling version,
// closing the currently-open version for (licence, country) in the SAME
// transaction, and writes a PLATFORM-scoped audit_log entry in that same
// transaction.
//
// tx MUST be db.Pool.WithPlatformAdmin - asserted in-function via
// assertPlatformScope AND enforced independently by RLS.
//
// Call AT MOST ONCE per transaction per (licence, country): every close
// and insert in one transaction shares that transaction's single now(),
// so a second call's close lands at effective_to == effective_from and
// trips the CHECK, surfacing as ErrConcurrentPolicyWrite even with no
// concurrent writer (mirrors PHASE-D-CR-P3-5, documented rather than
// papered over).
func CreateLicenceCountryCeilingVersion(ctx context.Context, tx pgx.Tx, p CreateLicenceCountryCeilingVersionParams) (CeilingRecord, error) {
	if err := p.Actor.validate(); err != nil {
		return CeilingRecord{}, err
	}
	if err := assertPlatformScope(ctx, tx); err != nil {
		return CeilingRecord{}, err
	}
	if p.LicenceID == uuid.Nil {
		return CeilingRecord{}, fmt.Errorf("%w: licence_id is required", ErrInvalidInput)
	}
	countryCode := strings.TrimSpace(p.CountryCode)
	if !validation.IsISO3166Alpha2(countryCode) {
		return CeilingRecord{}, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}
	if p.State != StateEnabled && p.State != StateDisabled {
		return CeilingRecord{}, fmt.Errorf("%w: state must be 'enabled' or 'disabled'", ErrInvalidInput)
	}
	if p.Status != StatusActive && p.Status != StatusWithdrawn {
		return CeilingRecord{}, fmt.Errorf("%w: status must be 'active' or 'withdrawn'", ErrInvalidInput)
	}
	authRef := strings.TrimSpace(p.AuthorizationReference)
	if p.Status == StatusActive && p.State == StateEnabled && authRef == "" {
		return CeilingRecord{}, fmt.Errorf("%w: authorization_reference is required to enable a country under a licence ceiling", ErrInvalidInput)
	}
	if p.Status == StatusWithdrawn && p.State != StateDisabled {
		return CeilingRecord{}, fmt.Errorf("%w: a withdrawal must carry state 'disabled'", ErrInvalidInput)
	}

	// Lock the latest version for this key against concurrent writers.
	// This is an OPTIMISATION, not the authoritative control - the
	// authoritative control is the partial unique index
	// uq_licence_country_ceilings_open plus the effective_to >
	// effective_from CHECK, both enforced at INSERT time regardless of
	// this lock's outcome.
	var latestID *uuid.UUID
	var latestEffectiveTo *time.Time
	err := tx.QueryRow(ctx, `
		SELECT id, effective_to FROM licence_country_ceilings
		 WHERE licence_id = $1 AND country_code = $2
		 ORDER BY effective_from DESC LIMIT 1 FOR UPDATE`,
		p.LicenceID, countryCode,
	).Scan(&latestID, &latestEffectiveTo)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CeilingRecord{}, fmt.Errorf("operatingmarket: lock latest ceiling version: %w", err)
	}

	var beforeState *CeilingRecord
	var closedEffectiveTo *time.Time
	if latestID != nil && latestEffectiveTo == nil {
		before, err := readCeilingRecordByID(ctx, tx, *latestID)
		if err != nil {
			return CeilingRecord{}, err
		}
		beforeState = &before

		var closed time.Time
		err = tx.QueryRow(ctx,
			`UPDATE licence_country_ceilings SET effective_to = now() WHERE id = $1 AND effective_to IS NULL RETURNING effective_to`,
			*latestID).Scan(&closed)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23514" {
				return CeilingRecord{}, ErrConcurrentPolicyWrite
			}
			if errors.Is(err, pgx.ErrNoRows) {
				return CeilingRecord{}, ErrConcurrentPolicyWrite
			}
			return CeilingRecord{}, fmt.Errorf("operatingmarket: close prior ceiling version: %w", err)
		}
		closedEffectiveTo = &closed
	}

	var (
		newID         uuid.UUID
		effectiveFrom time.Time
		createdAt     time.Time
	)
	err = tx.QueryRow(ctx, `
		INSERT INTO licence_country_ceilings (
			licence_id, country_code, state, status, authorization_reference,
			reason_code, policy_version, created_by_actor_type, created_by_actor_id
		) VALUES ($1, $2, $3, $4, NULLIF($5, ''), $6, $7, 'staff', $8)
		RETURNING id, effective_from, created_at`,
		p.LicenceID, countryCode, string(p.State), string(p.Status), authRef,
		p.Actor.ReasonCode, PolicyVersion, p.Actor.ActorID,
	).Scan(&newID, &effectiveFrom, &createdAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return CeilingRecord{}, ErrConcurrentPolicyWrite
		}
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return CeilingRecord{}, fmt.Errorf("%w: unknown licence_id %s", ErrInvalidInput, p.LicenceID)
		}
		return CeilingRecord{}, fmt.Errorf("operatingmarket: insert ceiling version: %w", err)
	}

	newState := CeilingRecord{
		ID: newID, LicenceID: p.LicenceID, CountryCode: countryCode,
		State: p.State, Status: p.Status, AuthorizationReference: authRef,
		ReasonCode: p.Actor.ReasonCode, PolicyVersion: PolicyVersion,
		EffectiveFrom: effectiveFrom, CreatedByActorType: "staff",
		CreatedByActorID: p.Actor.ActorID, CreatedAt: createdAt,
	}

	var beforeMetadata any
	if beforeState != nil {
		beforeMetadata = ceilingRecordState(*beforeState)
	}
	var priorEffectiveTo any
	if closedEffectiveTo != nil {
		priorEffectiveTo = closedEffectiveTo.Format(time.RFC3339Nano)
	}

	// ADR 0045 §3.5-A AMENDMENT-2 / §10, the ceiling's own rule: a ceiling
	// withdrawal is fail-closed (a withdrawn/absent ceiling resolves
	// not_permitted_by_licence, never permitted - CeilingReasonCeilingWithdrawn/
	// CeilingReasonNoCeilingRow), so it is NEVER widening_capable, unlike
	// the tenant/brand/operation table's inherit-rung withdrawal case.
	// Computed ONLY from beforeState and this write's own parameters;
	// resolve() is NEVER called here.
	blockedBefore := beforeState != nil && beforeState.Status == StatusActive && beforeState.State == StateDisabled
	blockedAfter := p.Status == StatusActive && p.State == StateDisabled
	rungBlockTransition := "no_block_change"
	switch {
	case blockedBefore && !blockedAfter:
		rungBlockTransition = "removes_block"
	case !blockedBefore && blockedAfter:
		rungBlockTransition = "adds_block"
	}
	wideningCapable := p.Status == StatusActive && p.State == StateEnabled

	if err := recordOperatingMarketAudit(ctx, tx, uuid.Nil, p.Actor, "operating_market.licence_ceiling_version_created", "licence_country_ceilings", newID.String(), map[string]any{
		"before":                  beforeMetadata,
		"after":                   ceilingRecordState(newState),
		"scope":                   "licence",
		"country_code":            countryCode,
		"licence_id":              p.LicenceID.String(),
		"state":                   string(p.State),
		"status":                  string(p.Status),
		"version_id":              newID.String(),
		"effective_from":          effectiveFrom.Format(time.RFC3339Nano),
		"prior_effective_to":      priorEffectiveTo,
		"authorization_reference": authRef,
		"policy_version":          PolicyVersion,
		"rung_block_transition":   rungBlockTransition,
		"widening_capable":        wideningCapable,
	}); err != nil {
		return CeilingRecord{}, err
	}

	return newState, nil
}

func readCeilingRecordByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (CeilingRecord, error) {
	var (
		rec       CeilingRecord
		state     string
		status    string
		actorType string
	)
	err := tx.QueryRow(ctx, `
		SELECT id, licence_id, country_code, state, status, COALESCE(authorization_reference, ''),
		       reason_code, policy_version, effective_from, effective_to,
		       created_by_actor_type, created_by_actor_id, created_at
		  FROM licence_country_ceilings WHERE id = $1`, id,
	).Scan(
		&rec.ID, &rec.LicenceID, &rec.CountryCode, &state, &status, &rec.AuthorizationReference,
		&rec.ReasonCode, &rec.PolicyVersion, &rec.EffectiveFrom, &rec.EffectiveTo,
		&actorType, &rec.CreatedByActorID, &rec.CreatedAt,
	)
	if err != nil {
		return CeilingRecord{}, fmt.Errorf("operatingmarket: read ceiling version %s: %w", id, err)
	}
	parsedState, err := parseState(state)
	if err != nil {
		return CeilingRecord{}, fmt.Errorf("operatingmarket: stored ceiling state is invalid: %w", err)
	}
	parsedStatus, err := parseStatus(status)
	if err != nil {
		return CeilingRecord{}, fmt.Errorf("operatingmarket: stored ceiling status is invalid: %w", err)
	}
	rec.State = parsedState
	rec.Status = parsedStatus
	rec.CreatedByActorType = actorType
	return rec, nil
}

// ListLicenceCountryCeilingVersions returns the full, append-only version
// history for one (licence, country), newest first. Diagnostics/admin
// surface only.
//
// tx MUST be a platform-admin-scoped transaction (db.Pool.WithPlatformAdmin)
// - asserted in-function via assertPlatformScope, as its FIRST statement
// (PHASE-D-SEC-P3-1's own lesson: a list function taking a raw pgx.Tx plus
// a caller-supplied id with no scope assertion is a real cross-tenant/
// cross-scope read gap, not a defence-in-depth nicety).
func ListLicenceCountryCeilingVersions(ctx context.Context, tx pgx.Tx, licenceID uuid.UUID, countryCode string) ([]CeilingRecord, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return nil, err
	}
	if licenceID == uuid.Nil {
		return nil, fmt.Errorf("%w: licence_id is required", ErrInvalidInput)
	}
	countryCode = strings.TrimSpace(countryCode)
	if !validation.IsISO3166Alpha2(countryCode) {
		return nil, fmt.Errorf("%w: country_code must be a valid, uppercase ISO-3166-1 alpha-2 code", ErrInvalidInput)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, licence_id, country_code, state, status, COALESCE(authorization_reference, ''),
		       reason_code, policy_version, effective_from, effective_to,
		       created_by_actor_type, created_by_actor_id, created_at
		  FROM licence_country_ceilings
		 WHERE licence_id = $1 AND country_code = $2
		 ORDER BY effective_from DESC`,
		licenceID, countryCode)
	if err != nil {
		return nil, fmt.Errorf("operatingmarket: list ceiling versions: %w", err)
	}
	defer rows.Close()

	var out []CeilingRecord
	for rows.Next() {
		var (
			rec       CeilingRecord
			state     string
			status    string
			actorType string
		)
		if err := rows.Scan(
			&rec.ID, &rec.LicenceID, &rec.CountryCode, &state, &status, &rec.AuthorizationReference,
			&rec.ReasonCode, &rec.PolicyVersion, &rec.EffectiveFrom, &rec.EffectiveTo,
			&actorType, &rec.CreatedByActorID, &rec.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("operatingmarket: scan ceiling version: %w", err)
		}
		parsedState, err := parseState(state)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: stored ceiling state is invalid: %w", err)
		}
		parsedStatus, err := parseStatus(status)
		if err != nil {
			return nil, fmt.Errorf("operatingmarket: stored ceiling status is invalid: %w", err)
		}
		rec.State = parsedState
		rec.Status = parsedStatus
		rec.CreatedByActorType = actorType
		out = append(out, rec)
	}
	return out, rows.Err()
}
