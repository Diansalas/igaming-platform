// Stage 9.2 Part B2 / Wave 3 (ADR 0083 §6.2.3, §9.2): the admin surface
// for sb_exposure_limits - create/disable/list, modelled on
// internal/risk/policy_service.go's shape (audited, reason-coded,
// scope-gated, never a hard delete/edit) EXACTLY, which is the same
// template Wave 2 used for jurisdiction_admin.go (see that file's own doc
// comment) - reused here for within-stage consistency.
//
// UNLIKE sb_jurisdiction_restrictions (Wave 2's platform-admin-write,
// read-open table), sb_exposure_limits IS tenant-owned commercial
// configuration - it carries a tenant_id and its RLS is
// sportsbook_bets' own tenant_staff_scope shape, not casino_games'
// platform-admin shape (migration 0088). The gating permission is
// therefore a TENANT risk_manager-class permission
// (PermSportsbookExposureLimitManage/Read, internal/auth/permission.go),
// never platform-admin-only - mirroring PermRiskConfigRead/
// PermRiskConfigManage's identical tenant-scoped shape and RoleRiskManager
// grant, not PermCasinoCatalogueManage's platform-only one.
//
// A limit is DISABLED, never edited or deleted (ADR 0083 §6.2.3:
// "max_open_potential_payout itself is immutable - changing a ceiling
// means a new row") - the database's own immutability/deny-delete
// triggers (migration 0088) enforce this regardless of what this Go code
// does.
package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// ErrExposureLimitNotFound covers "no such limit" / "already disabled"
// (DisableExposureLimit only ever transitions an 'active' row).
var ErrExposureLimitNotFound = errors.New("sportsbook: exposure limit not found")

// ExposureLimit mirrors a sb_exposure_limits row.
type ExposureLimit struct {
	ID                     uuid.UUID
	TenantID               uuid.UUID
	BrandID                *uuid.UUID
	ScopeKind              string
	AssetCode              string
	MaxOpenPotentialPayout int64
	Status                 string
	AuthorizationReference string
	ReasonCode             string
	CreatedByActorType     string
	CreatedByActorID       uuid.UUID
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

const exposureLimitColumns = `id, tenant_id, brand_id, scope_kind, asset_code, max_open_potential_payout,
	status, authorization_reference, reason_code, created_by_actor_type, created_by_actor_id, created_at, updated_at`

func scanExposureLimit(row pgx.Row) (ExposureLimit, error) {
	var l ExposureLimit
	// max_open_potential_payout is NUMERIC(38,0) in the database - scanned
	// via pgtype.Numeric and converted with THIS package's own
	// numericToBigInt (exposure.go), then narrowed to int64 for the Go
	// struct/JSON response ONLY (mirrors risk.Rule.Threshold's identical
	// int64-in-Go-despite-NUMERIC-in-the-database decision, ADR 0031 §35 -
	// an admin-authored ceiling is expected to fit int64 even for an
	// 18-exponent asset in any realistic commercial configuration; the
	// AGGREGATE evaluate_exposure_limits compares against it stays *big.Int
	// throughout, never narrowed).
	var numeric pgtype.Numeric
	err := row.Scan(
		&l.ID, &l.TenantID, &l.BrandID, &l.ScopeKind, &l.AssetCode, &numeric,
		&l.Status, &l.AuthorizationReference, &l.ReasonCode, &l.CreatedByActorType, &l.CreatedByActorID,
		&l.CreatedAt, &l.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ExposureLimit{}, ErrExposureLimitNotFound
	}
	if err != nil {
		return ExposureLimit{}, fmt.Errorf("sportsbook: scan exposure limit: %w", err)
	}
	thresholdBigInt, err := numericToBigInt(numeric)
	if err != nil {
		return ExposureLimit{}, fmt.Errorf("sportsbook: exposure limit %s: %w", l.ID, err)
	}
	if !thresholdBigInt.IsInt64() {
		return ExposureLimit{}, fmt.Errorf("sportsbook: exposure limit %s: max_open_potential_payout exceeds int64 - not representable in this response shape", l.ID)
	}
	l.MaxOpenPotentialPayout = thresholdBigInt.Int64()
	return l, nil
}

func scanExposureLimits(rows pgx.Rows) ([]ExposureLimit, error) {
	defer rows.Close()
	var out []ExposureLimit
	for rows.Next() {
		l, err := scanExposureLimit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// CreateExposureLimitParams is CreateExposureLimit's input. tx MUST
// already be tenant-scoped (db.Pool.WithTenant) for TenantID - migration
// 0088's tenant_staff_scope RLS policy is what actually restricts the
// INSERT to that tenant, mirroring risk.CreateRule's identical posture
// (this function does not itself choose or override the transaction's
// scope).
type CreateExposureLimitParams struct {
	TenantID uuid.UUID
	// BrandID nil means tenant-wide (every brand in this tenant) - the
	// tenant-wide precedence tier (ADR 0083 §6.2.3).
	BrandID                *uuid.UUID
	ScopeKind              string
	AssetCode              string
	MaxOpenPotentialPayout int64
	AuthorizationReference string
	ReasonCode             string
	CreatedByActorID       uuid.UUID
	IPAddress              string
	UserAgent              string
	RequestID              string
}

// CreateExposureLimit inserts a new, active sb_exposure_limits row and
// audits it (sportsbook.exposure_limit_created). A given (tenant, brand,
// scope_kind, asset) may have at most one ACTIVE row (migration 0088's two
// partial unique indexes) - disable the existing one first to supersede
// it with a new ceiling.
func CreateExposureLimit(ctx context.Context, tx pgx.Tx, params CreateExposureLimitParams) (ExposureLimit, error) {
	switch params.ScopeKind {
	case "event", "market", "selection":
	default:
		return ExposureLimit{}, fmt.Errorf("%w: scope_kind must be one of 'event', 'market', 'selection'", ErrInvalidInput)
	}
	if params.AssetCode == "" {
		return ExposureLimit{}, fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if params.MaxOpenPotentialPayout <= 0 {
		return ExposureLimit{}, fmt.Errorf("%w: max_open_potential_payout must be positive", ErrInvalidInput)
	}
	// ADR 0045 §4's authorization discipline, carried into this table
	// exactly (ADR 0083 §6.2.3): required on every row.
	if params.AuthorizationReference == "" || params.ReasonCode == "" {
		return ExposureLimit{}, fmt.Errorf("%w: authorization_reference and reason_code are required", ErrInvalidInput)
	}
	if params.CreatedByActorID == uuid.Nil {
		return ExposureLimit{}, fmt.Errorf("%w: created_by_actor_id is required", ErrInvalidInput)
	}

	id := uuid.New()
	_, err := tx.Exec(ctx,
		`INSERT INTO sb_exposure_limits
			(id, tenant_id, brand_id, scope_kind, asset_code, max_open_potential_payout,
			 authorization_reference, reason_code, created_by_actor_type, created_by_actor_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'staff', $9)`,
		id, params.TenantID, params.BrandID, params.ScopeKind, params.AssetCode, params.MaxOpenPotentialPayout,
		params.AuthorizationReference, params.ReasonCode, params.CreatedByActorID,
	)
	if err != nil {
		return ExposureLimit{}, fmt.Errorf("sportsbook: insert exposure limit: %w", err)
	}
	l, err := scanExposureLimit(tx.QueryRow(ctx, `SELECT `+exposureLimitColumns+` FROM sb_exposure_limits WHERE id = $1`, id))
	if err != nil {
		return ExposureLimit{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.TenantID, ActorType: audit.ActorStaff, ActorID: params.CreatedByActorID,
		Action: "sportsbook.exposure_limit_created", TargetType: "sb_exposure_limit", TargetID: l.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: map[string]any{
			"scope_kind": l.ScopeKind, "asset_code": l.AssetCode, "brand_id": uuidPtrString(l.BrandID),
			// The threshold and its denomination ARE part of what this row
			// MEANS and belong in the ADMINISTRATIVE audit trail (mirrors
			// risk.CreateRule's identical inclusion) - this is NOT the same
			// as INV-SB-EXP-2, which forbids the threshold reaching a
			// PLAYER-facing response, never the audit log a staff member
			// with PermSportsbookExposureLimitManage already reads.
			"max_open_potential_payout": l.MaxOpenPotentialPayout,
			"authorization_reference":   l.AuthorizationReference, "reason_code": l.ReasonCode,
			// §6.2.2's own disclosure requirement: the admin surface must
			// state which measure the configured number is denominated in,
			// so a human setting it is not guessing.
			"measure": "gross_potential_payout",
		},
	}); err != nil {
		return ExposureLimit{}, fmt.Errorf("sportsbook: audit exposure limit creation: %w", err)
	}
	return l, nil
}

// DisableExposureLimitParams is DisableExposureLimit's input.
type DisableExposureLimitParams struct {
	ID         uuid.UUID
	ReasonCode string
	ActorID    uuid.UUID
	IPAddress  string
	UserAgent  string
	RequestID  string
}

// DisableExposureLimit flips an 'active' row to 'disabled' - the ONLY
// mutation migration 0088's immutability trigger permits besides
// reason_code - and audits it (sportsbook.exposure_limit_disabled). Never
// a delete or an edit of max_open_potential_payout: a limit is disabled
// and superseded by a NEW row, never edited in place.
func DisableExposureLimit(ctx context.Context, tx pgx.Tx, params DisableExposureLimitParams) (ExposureLimit, error) {
	if params.ID == uuid.Nil || params.ActorID == uuid.Nil {
		return ExposureLimit{}, fmt.Errorf("%w: id and actor_id are required", ErrInvalidInput)
	}
	if params.ReasonCode == "" {
		return ExposureLimit{}, fmt.Errorf("%w: reason_code is required", ErrInvalidInput)
	}
	tag, err := tx.Exec(ctx,
		`UPDATE sb_exposure_limits SET status = 'disabled', reason_code = $2 WHERE id = $1 AND status = 'active'`,
		params.ID, params.ReasonCode,
	)
	if err != nil {
		return ExposureLimit{}, fmt.Errorf("sportsbook: disable exposure limit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ExposureLimit{}, ErrExposureLimitNotFound
	}
	l, err := scanExposureLimit(tx.QueryRow(ctx, `SELECT `+exposureLimitColumns+` FROM sb_exposure_limits WHERE id = $1`, params.ID))
	if err != nil {
		return ExposureLimit{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: l.TenantID, ActorType: audit.ActorStaff, ActorID: params.ActorID,
		Action: "sportsbook.exposure_limit_disabled", TargetType: "sb_exposure_limit", TargetID: l.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: map[string]any{"scope_kind": l.ScopeKind, "asset_code": l.AssetCode, "reason_code": l.ReasonCode},
	}); err != nil {
		return ExposureLimit{}, fmt.Errorf("sportsbook: audit exposure limit disable: %w", err)
	}
	return l, nil
}

// ListExposureLimits is the admin read path - every row (active and
// disabled, so the full configuration history is visible), newest first.
// tx must already be tenant-scoped; migration 0088's tenant_staff_scope
// RLS policy restricts the read to that tenant's own rows (no player
// policy exists at all - a limit has no player-facing read path, ADR 0083
// §6.2.3).
func ListExposureLimits(ctx context.Context, tx pgx.Tx) ([]ExposureLimit, error) {
	rows, err := tx.Query(ctx, `SELECT `+exposureLimitColumns+` FROM sb_exposure_limits ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("sportsbook: list exposure limits: %w", err)
	}
	return scanExposureLimits(rows)
}
