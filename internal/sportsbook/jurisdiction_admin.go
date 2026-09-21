// Stage 9.2 (ADR 0083 §5.2/§9.2): the admin surface for
// sb_jurisdiction_restrictions - create/withdraw/list, modelled EXACTLY on
// internal/risk/policy_service.go's shape (audited, reason-coded,
// scope-gated, never a hard delete) and on internal/casino.UpsertGame's
// assertPlatformScope precedent (this table, like casino_games, is
// platform-admin-write-only - migration 0087's RLS policies are
// byte-identical in shape to casino_games' own). A restriction is
// WITHDRAWN, never deleted (the deny-delete trigger enforces this at the
// database regardless of what this Go code does) - compliance history
// survives every mutation.
package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// ErrTransactionScope is returned when tx is not a genuinely
// platform-admin-scoped transaction - mirrors internal/casino's/
// internal/jurisdiction's identical sentinel and assertPlatformScope
// pattern exactly (each package keeps its own copy rather than sharing
// one, since internal/sportsbook must not import either).
var ErrTransactionScope = errors.New("sportsbook: requires a platform-admin-scoped transaction (use db.Pool.WithPlatformAdmin)")

// ErrRestrictionNotFound covers "no such restriction" / "already
// withdrawn" (WithdrawJurisdictionRestriction only ever transitions an
// 'active' row).
var ErrRestrictionNotFound = errors.New("sportsbook: jurisdiction restriction not found")

func assertPlatformScope(ctx context.Context, tx pgx.Tx) error {
	var platformAdmin *uuid.UUID
	var scopedTenant *uuid.UUID
	var scopedPlayer *uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid,
		        NULLIF(current_setting('app.tenant_id', true), '')::uuid,
		        NULLIF(current_setting('app.player_account_id', true), '')::uuid`,
	).Scan(&platformAdmin, &scopedTenant, &scopedPlayer); err != nil {
		return fmt.Errorf("sportsbook: read platform admin scope: %w", err)
	}
	if platformAdmin == nil || scopedTenant != nil || scopedPlayer != nil {
		return ErrTransactionScope
	}
	return nil
}

// JurisdictionRestriction mirrors a sb_jurisdiction_restrictions row.
type JurisdictionRestriction struct {
	ID                     uuid.UUID
	ScopeKind              string
	EventID                *uuid.UUID
	MarketID               *uuid.UUID
	SelectionID            *uuid.UUID
	JurisdictionCode       string
	RestrictionKind        string
	Status                 string
	AuthorizationReference string
	ReasonCode             string
	CreatedByActorType     string
	CreatedByActorID       uuid.UUID
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

const jurisdictionRestrictionColumns = `id, scope_kind, event_id, market_id, selection_id, jurisdiction_code,
	restriction_kind, status, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id,
	created_at, updated_at`

func scanJurisdictionRestriction(row pgx.Row) (JurisdictionRestriction, error) {
	var r JurisdictionRestriction
	err := row.Scan(
		&r.ID, &r.ScopeKind, &r.EventID, &r.MarketID, &r.SelectionID, &r.JurisdictionCode,
		&r.RestrictionKind, &r.Status, &r.AuthorizationReference, &r.ReasonCode, &r.CreatedByActorType, &r.CreatedByActorID,
		&r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return JurisdictionRestriction{}, ErrRestrictionNotFound
	}
	if err != nil {
		return JurisdictionRestriction{}, fmt.Errorf("sportsbook: scan jurisdiction restriction: %w", err)
	}
	return r, nil
}

// CreateJurisdictionRestrictionParams is CreateJurisdictionRestriction's
// input. Exactly one of EventID/MarketID/SelectionID must be set, and it
// must match ScopeKind - the database's own CHECK constraints (migration
// 0087) enforce this independently regardless of what this validation
// does; the validation exists so a caller gets a field-level error rather
// than a raw constraint violation.
type CreateJurisdictionRestrictionParams struct {
	ScopeKind              string
	EventID                *uuid.UUID
	MarketID               *uuid.UUID
	SelectionID            *uuid.UUID
	JurisdictionCode       string
	AuthorizationReference string
	ReasonCode             string
	CreatedByActorID       uuid.UUID
	IPAddress              string
	UserAgent              string
	RequestID              string
}

// CreateJurisdictionRestriction inserts a new, active
// sb_jurisdiction_restrictions row and audits it
// (sportsbook_jurisdiction_restriction.created). tx MUST be a
// platform-admin-scoped transaction (db.Pool.WithPlatformAdmin) -
// asserted here AND independently enforced by migration 0087's RLS write
// policies regardless of what this function does.
func CreateJurisdictionRestriction(ctx context.Context, tx pgx.Tx, params CreateJurisdictionRestrictionParams) (JurisdictionRestriction, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return JurisdictionRestriction{}, err
	}
	switch params.ScopeKind {
	case "event":
		if params.EventID == nil || params.MarketID != nil || params.SelectionID != nil {
			return JurisdictionRestriction{}, fmt.Errorf("%w: scope_kind 'event' requires event_id only", ErrInvalidInput)
		}
	case "market":
		if params.MarketID == nil || params.EventID != nil || params.SelectionID != nil {
			return JurisdictionRestriction{}, fmt.Errorf("%w: scope_kind 'market' requires market_id only", ErrInvalidInput)
		}
	case "selection":
		if params.SelectionID == nil || params.EventID != nil || params.MarketID != nil {
			return JurisdictionRestriction{}, fmt.Errorf("%w: scope_kind 'selection' requires selection_id only", ErrInvalidInput)
		}
	default:
		return JurisdictionRestriction{}, fmt.Errorf("%w: scope_kind must be one of 'event', 'market', 'selection'", ErrInvalidInput)
	}
	if params.JurisdictionCode == "" {
		return JurisdictionRestriction{}, fmt.Errorf("%w: jurisdiction_code is required", ErrInvalidInput)
	}
	// ADR 0045 §4's authorization discipline, carried into this table
	// exactly (ADR 0083 §5.2.2): required on every row, never only on an
	// "enable" - every row here IS a restriction.
	if params.AuthorizationReference == "" || params.ReasonCode == "" {
		return JurisdictionRestriction{}, fmt.Errorf("%w: authorization_reference and reason_code are required", ErrInvalidInput)
	}
	if params.CreatedByActorID == uuid.Nil {
		return JurisdictionRestriction{}, fmt.Errorf("%w: created_by_actor_id is required", ErrInvalidInput)
	}

	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO sb_jurisdiction_restrictions
			(id, scope_kind, event_id, market_id, selection_id, jurisdiction_code,
			 authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'staff', $9)`,
		id, params.ScopeKind, params.EventID, params.MarketID, params.SelectionID, params.JurisdictionCode,
		params.AuthorizationReference, params.ReasonCode, params.CreatedByActorID,
	)
	if err != nil {
		return JurisdictionRestriction{}, fmt.Errorf("sportsbook: insert jurisdiction restriction: %w", err)
	}
	r, err := scanJurisdictionRestriction(tx.QueryRow(ctx, `SELECT `+jurisdictionRestrictionColumns+` FROM sb_jurisdiction_restrictions WHERE id = $1`, id))
	if err != nil {
		return JurisdictionRestriction{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorType: audit.ActorStaff, ActorID: params.CreatedByActorID,
		Action: "sportsbook_jurisdiction_restriction.created", TargetType: "sb_jurisdiction_restriction", TargetID: r.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: map[string]any{
			"scope_kind": r.ScopeKind, "jurisdiction_code": r.JurisdictionCode,
			"event_id": uuidPtrString(r.EventID), "market_id": uuidPtrString(r.MarketID), "selection_id": uuidPtrString(r.SelectionID),
			"authorization_reference": r.AuthorizationReference, "reason_code": r.ReasonCode,
		},
	}); err != nil {
		return JurisdictionRestriction{}, fmt.Errorf("sportsbook: audit jurisdiction restriction creation: %w", err)
	}
	return r, nil
}

// WithdrawJurisdictionRestrictionParams is WithdrawJurisdictionRestriction's
// input.
type WithdrawJurisdictionRestrictionParams struct {
	ID uuid.UUID
	// ReasonCode is REQUIRED and REPLACES the row's own reason_code - the
	// one other field migration 0087's immutability trigger permits to
	// change alongside status, so the withdrawal's own justification is
	// recorded on the row itself, not only in the audit trail.
	ReasonCode string
	ActorID    uuid.UUID
	IPAddress  string
	UserAgent  string
	RequestID  string
}

// WithdrawJurisdictionRestriction flips an 'active' row to 'withdrawn' -
// the ONLY mutation migration 0087's immutability trigger permits besides
// reason_code - and audits it
// (sportsbook_jurisdiction_restriction.withdrawn). Never a delete: the
// deny-delete trigger refuses one regardless, and compliance history must
// survive.
func WithdrawJurisdictionRestriction(ctx context.Context, tx pgx.Tx, params WithdrawJurisdictionRestrictionParams) (JurisdictionRestriction, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return JurisdictionRestriction{}, err
	}
	if params.ID == uuid.Nil || params.ActorID == uuid.Nil {
		return JurisdictionRestriction{}, fmt.Errorf("%w: id and actor_id are required", ErrInvalidInput)
	}
	if params.ReasonCode == "" {
		return JurisdictionRestriction{}, fmt.Errorf("%w: reason_code is required", ErrInvalidInput)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE sb_jurisdiction_restrictions SET status = 'withdrawn', reason_code = $2 WHERE id = $1 AND status = 'active'`,
		params.ID, params.ReasonCode,
	)
	if err != nil {
		return JurisdictionRestriction{}, fmt.Errorf("sportsbook: withdraw jurisdiction restriction: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return JurisdictionRestriction{}, ErrRestrictionNotFound
	}
	r, err := scanJurisdictionRestriction(tx.QueryRow(ctx, `SELECT `+jurisdictionRestrictionColumns+` FROM sb_jurisdiction_restrictions WHERE id = $1`, params.ID))
	if err != nil {
		return JurisdictionRestriction{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorType: audit.ActorStaff, ActorID: params.ActorID,
		Action: "sportsbook_jurisdiction_restriction.withdrawn", TargetType: "sb_jurisdiction_restriction", TargetID: r.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: map[string]any{"scope_kind": r.ScopeKind, "jurisdiction_code": r.JurisdictionCode, "reason_code": r.ReasonCode},
	}); err != nil {
		return JurisdictionRestriction{}, fmt.Errorf("sportsbook: audit jurisdiction restriction withdrawal: %w", err)
	}
	return r, nil
}

// ListJurisdictionRestrictions is the admin read path - every row
// (active and withdrawn, so the full compliance history is visible),
// newest first. The underlying table's own RLS read policy is
// platform-uniform read-open (migration 0087, byte-identical to
// casino_games_read) - this function itself performs no scope assertion
// beyond that, mirroring risk.ListRulesForOperation's identical posture;
// the HTTP handler's own permission gate (PermSportsbookJurisdictionRestrictionRead)
// is what restricts who reaches this call at all.
func ListJurisdictionRestrictions(ctx context.Context, tx pgx.Tx) ([]JurisdictionRestriction, error) {
	rows, err := tx.Query(ctx, `SELECT `+jurisdictionRestrictionColumns+` FROM sb_jurisdiction_restrictions ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sportsbook: list jurisdiction restrictions: %w", err)
	}
	defer rows.Close()
	var out []JurisdictionRestriction
	for rows.Next() {
		r, err := scanJurisdictionRestriction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func uuidPtrString(id *uuid.UUID) string {
	if id == nil {
		return ""
	}
	return id.String()
}
