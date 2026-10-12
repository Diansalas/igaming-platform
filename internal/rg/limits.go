package rg

// ADR 0115 SLICE S1: the RG limit vocabulary and the two read primitives slices S2-S4 build on.
//
// Status: schema IMPLEMENTED (migration 0135), enforcement NOT IMPLEMENTED (S4), write APIs NOT
// IMPLEMENTED (S2). Nothing in this file refuses a deposit, a wager, a launch or a login.
//
// The classification of a limit change and the realization rule each have exactly ONE
// implementation, in SQL (rg_limit_change_classify / rg_limit_realized, migration 0135). The
// database's BEFORE INSERT trigger on player_rg_limit_changes calls them, and Go calls them through
// ClassifyLimitChange / RealizedLimit below; Go never re-implements either (security condition 3).

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RestrictionTimeOut is the second player_restrictions type (ADR 0115 2.3; migration 0135 extends
// the CHECK and the player_self_insert policy). S1 writes no time-out and EvaluateEligibility does
// not read one yet (S4).
const RestrictionTimeOut RestrictionType = "time_out"

// ControlKind mirrors rg_limit_configs.control_kind.
type ControlKind string

const (
	ControlDeposit              ControlKind = "deposit"
	ControlLoss                 ControlKind = "loss"
	ControlWager                ControlKind = "wager"
	ControlSessionDuration      ControlKind = "session_duration"
	ControlRealityCheckInterval ControlKind = "reality_check_interval"
	ControlTimeOut              ControlKind = "time_out"
)

// ConfigControlKinds is the closed set rg_limit_configs admits.
var ConfigControlKinds = []ControlKind{ControlDeposit, ControlLoss, ControlWager, ControlSessionDuration, ControlRealityCheckInterval, ControlTimeOut}

// PlayerLimitKinds is the closed set player_rg_limit_changes admits (the player-set limits).
var PlayerLimitKinds = []ControlKind{ControlDeposit, ControlLoss, ControlWager, ControlSessionDuration}

// IsAmountKind reports whether the kind is measured in asset minor units (per asset, per period).
func (k ControlKind) IsAmountKind() bool {
	return k == ControlDeposit || k == ControlLoss || k == ControlWager
}

// PeriodKind mirrors the allowed period values (ADR 0115 1.2).
type PeriodKind string

const (
	PeriodRolling24h    PeriodKind = "rolling_24h"
	PeriodRolling7d     PeriodKind = "rolling_7d"
	PeriodRolling30d    PeriodKind = "rolling_30d"
	PeriodCalendarDay   PeriodKind = "calendar_day"
	PeriodCalendarWeek  PeriodKind = "calendar_week"
	PeriodCalendarMonth PeriodKind = "calendar_month"
)

// PeriodKinds is the closed set.
var PeriodKinds = []PeriodKind{PeriodRolling24h, PeriodRolling7d, PeriodRolling30d, PeriodCalendarDay, PeriodCalendarWeek, PeriodCalendarMonth}

// LimitScope mirrors rg_limit_configs.scope / player_rg_limit_changes.scope.
type LimitScope string

const (
	LimitScopeAccount        LimitScope = "account"
	LimitScopeTenantPerson   LimitScope = "tenant_person"
	LimitScopePlatformPerson LimitScope = "platform_person" // time-out only
)

// ChangeKind mirrors player_rg_limit_changes.change_kind. It is always decided by the database.
type ChangeKind string

const (
	ChangeInitial       ChangeKind = "initial"
	ChangeDecrease      ChangeKind = "decrease"
	ChangeIncrease      ChangeKind = "increase"
	ChangeRemoval       ChangeKind = "removal"
	ChangeCancelPending ChangeKind = "cancel_pending"
	ChangeConfirm       ChangeKind = "confirm"
)

// ChangeKinds is the closed set.
var ChangeKinds = []ChangeKind{ChangeInitial, ChangeDecrease, ChangeIncrease, ChangeRemoval, ChangeCancelPending, ChangeConfirm}

// ChangeSource mirrors player_rg_limit_changes.source.
type ChangeSource string

const (
	ChangeSourcePlayerSelfService    ChangeSource = "player_self_service"
	ChangeSourceStaffOnPlayerRequest ChangeSource = "staff_on_player_request"
)

// SyntheticAttestationRef labels SYNTHETIC-NOT-LEGAL configuration (test fixtures only). The
// database refuses such a row unless database_environment AND app.environment are development or
// test, and rg_limit_config_compose treats it as absent otherwise.
const SyntheticAttestationRef = "synthetic"

// SQLSTATEs of migration 0135 (class RL).
const (
	SQLStateRGActorSession       = "RL001"
	SQLStateRGBackdated          = "RL003"
	SQLStateRGOperatorLoosens    = "RL004"
	SQLStateRGNoFloorInForce     = "RL005"
	SQLStateRGFloorNoApproval    = "RL006"
	SQLStateRGFourEyesBinding    = "RL007"
	SQLStateRGSyntheticRefused   = "RL008"
	SQLStateRGAppendOnly         = "RL009"
	SQLStateRGRequestTransition  = "RL010"
	SQLStateRGCalendarTZ         = "RL011"
	SQLStateRGClientAssertedTime = "RL020"
	SQLStateRGNoChange           = "RL021"
	SQLStateRGSupersedeInvalid   = "RL022"
	SQLStateRGConfigMissing      = "RL023"
	SQLStateRGIsolation          = "RL024"
	SQLStateRGTenantActor        = "RL025"
)

// ErrRGDatabase wraps a refusal of the RG limit triggers (inspect the SQLSTATE with SQLStateOf).
var ErrRGDatabase = errors.New("rg: limit change refused by the database")

// SQLStateOf returns the SQLSTATE of a database error ("" otherwise).
func SQLStateOf(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// LimitKey names one player limit: the anchor account (account scope) or the Person within the
// tenant (tenant_person scope), the kind, and for amount kinds the asset and period.
type LimitKey struct {
	TenantID        uuid.UUID
	PlayerAccountID uuid.UUID
	PersonID        uuid.UUID
	Scope           LimitScope
	Kind            ControlKind
	AssetCode       *string
	Period          *PeriodKind
}

func (k LimitKey) period() *string {
	if k.Period == nil {
		return nil
	}
	s := string(*k.Period)
	return &s
}

// Realized is the latest realized row of a key (ADR 0115 2.2). HasRow is false when no row was
// ever realized; Value nil with HasRow true means the limit was removed.
type Realized struct {
	HasRow   bool
	Value    *string // NUMERIC(38,0) as a decimal string (never a float)
	ChangeID *uuid.UUID
}

// RealizedLimit evaluates the SQL realization rule at the database instant `at` (pass nil for the
// current clock_timestamp()). It runs with the caller's row visibility.
func RealizedLimit(ctx context.Context, tx pgx.Tx, k LimitKey, at *time.Time) (Realized, error) {
	var r Realized
	err := tx.QueryRow(ctx, `SELECT has_row, value::text, change_id
		FROM rg_limit_realized($1, $2, $3, $4, $5, $6, $7, COALESCE($8::timestamptz, clock_timestamp()))`,
		k.TenantID, k.PlayerAccountID, k.PersonID, string(k.Scope), string(k.Kind), k.AssetCode, k.period(), at,
	).Scan(&r.HasRow, &r.Value, &r.ChangeID)
	if err != nil {
		return Realized{}, fmt.Errorf("rg: realized limit: %w", err)
	}
	return r, nil
}

// ClassifyParams is the input of ClassifyLimitChange. Value nil means a removal; Supersedes and
// Intent name a cancel_pending / confirm.
type ClassifyParams struct {
	Key              LimitKey
	BrandID          uuid.UUID
	JurisdictionCode *string
	Value            *string
	Supersedes       *uuid.UUID
	Intent           *ChangeKind
	Source           ChangeSource
	At               *time.Time
}

// Classification is what the database decides for a change at an instant.
type Classification struct {
	ChangeKind           ChangeKind
	EffectiveAt          time.Time
	RequiresConfirmation bool
	Value                *string
	ConfigVersionIDs     []uuid.UUID
}

// ClassifyLimitChange evaluates the ONE SQL classification (rg_limit_change_classify) - for a
// preview ("takes effect at"); the authoritative classification is the trigger's, at insert time.
// A refusal is returned as the database error (SQLStateOf: RL021 / RL022 / RL023).
func ClassifyLimitChange(ctx context.Context, tx pgx.Tx, p ClassifyParams) (Classification, error) {
	var c Classification
	var kind string
	var intent *string
	if p.Intent != nil {
		s := string(*p.Intent)
		intent = &s
	}
	err := tx.QueryRow(ctx, `SELECT change_kind, effective_at, requires_confirmation, value::text, config_version_ids
		FROM rg_limit_change_classify($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11, $12, $13,
		                              COALESCE($14::timestamptz, clock_timestamp()))`,
		p.Key.TenantID, p.Key.PlayerAccountID, p.Key.PersonID, p.BrandID, p.JurisdictionCode,
		string(p.Key.Scope), string(p.Key.Kind), p.Key.AssetCode, p.Key.period(), p.Value,
		p.Supersedes, intent, string(p.Source), p.At,
	).Scan(&kind, &c.EffectiveAt, &c.RequiresConfirmation, &c.Value, &c.ConfigVersionIDs)
	if err != nil {
		return Classification{}, fmt.Errorf("%w: %w", ErrRGDatabase, err)
	}
	c.ChangeKind = ChangeKind(kind)
	return c, nil
}
