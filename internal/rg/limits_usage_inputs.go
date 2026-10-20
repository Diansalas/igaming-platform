package rg

// ADR 0115 SLICE S3: effective-limit READ INPUTS.
//
// Status: IMPLEMENTED as inputs only. This file reads what S4 needs to decide "usage + amount <=
// limit": the composed configuration in force (ceiling, allowed periods, calendar_tz, week_start,
// count_bonus_funded, loss_may_go_negative, scope, session and TTL fields) and EVERY realized player
// limit row of the key family, from BOTH scopes (the anchor account's rows and the Person's
// tenant_person rows, all periods). It does NOT compute the effective limit and refuses nothing: the
// MIN across scopes and the ceiling (ADR 0115 2.2), the clamp of a confirm after a ceiling was
// lowered, and the legacy-scope combination after a scope change are the S4 HARD CONDITIONS.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// UsageConfig is the composed configuration for one (tenant, brand, kind, asset), as
// rg_limit_config_compose returns it (strictest / UNION / OR). Found is false when no floor is in
// force (rg_config_missing in S4).
type UsageConfig struct {
	Found      bool
	VersionIDs []uuid.UUID
	Kind       ControlKind
	AssetCode  *string

	Enabled, Mandatory bool
	Ceiling            *string // NUMERIC(38,0) text; nil with CeilingNone = an attested "no bound"
	CeilingNone        bool
	AllowedPeriods     []PeriodKind
	CalendarTZ         *string
	WeekStart          *int
	CooldownSeconds    *int64
	Scope              LimitScope

	CountBonusFunded  *bool
	LossMayGoNegative *bool // composed: true only when every row attests it; the floor of 0 applies otherwise

	PostSessionBreakSeconds    *int64
	ConcurrentChainAggregation *bool
	PendingIntentTTLSeconds    *int64 // nil = none attested (or not a deposit row)

	// MissingFloors (lic25) is true for a tenant with an approved operating basis when a jurisdiction it relies on has no floor in
	// force (or cannot be resolved): the configuration read is then partial, and an increase or removal fails closed.
	MissingFloors bool
}

// Windows returns the measurement windows of the allowed periods under this configuration's
// timezone and week start.
func (c UsageConfig) Windows() []WindowSpec {
	out := make([]WindowSpec, 0, len(c.AllowedPeriods))
	for _, p := range c.AllowedPeriods {
		w := WindowSpec{Period: p}
		if c.CalendarTZ != nil {
			w.CalendarTZ = *c.CalendarTZ
		}
		if c.WeekStart != nil {
			w.WeekStart = *c.WeekStart
		}
		out = append(out, w)
	}
	return out
}

// ReadUsageConfig composes the configuration in force at `at` (nil = now) for the tenant: the strictest fold of every jurisdiction the
// tenant relies on (the licence jurisdiction and its approved bases' jurisdictions; lic25), derived server-side, never from a request.
// MissingFloors reports a relied-on jurisdiction without a floor (only for a tenant with an approved basis).
func ReadUsageConfig(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, kind ControlKind, asset *string, at *time.Time) (UsageConfig, error) {
	if err := usageGuard(ctx, tx, tenantID); err != nil {
		return UsageConfig{}, err
	}
	sel, err := tenantReliance(ctx, tx, tenantID)
	if err != nil {
		return UsageConfig{}, err
	}
	return readUsageConfigFor(ctx, tx, sel, tenantID, brandID, kind, asset, at)
}

// readUsageConfigFor is ReadUsageConfig after the scope guard and the jurisdiction selection: EvaluateAction selects the scope's one
// jurisdiction once per action and composes several kinds with the one result (no behavioural difference).
func readUsageConfigFor(ctx context.Context, tx pgx.Tx, sel cfgSel, tenantID, brandID uuid.UUID, kind ControlKind, asset *string, at *time.Time) (UsageConfig, error) {
	cfg := UsageConfig{Kind: kind, AssetCode: asset}
	if len(sel.Codes) == 0 {
		cfg.MissingFloors = sel.Strict && sel.Incomplete
		return cfg, nil
	}
	r, missing, tzConflict, err := readCombined(ctx, tx, sel, tenantID, brandID, kind, asset, at)
	if err != nil {
		return UsageConfig{}, fmt.Errorf("rg: read usage configuration: %w", err)
	}
	if tzConflict {
		return UsageConfig{}, errors.New("rg: relied-on jurisdictions disagree on calendar_tz / week_start; the windows cannot be selected (fail closed)")
	}
	cfg.MissingFloors = missing
	if !r.Found {
		return cfg, nil
	}
	cfg.Found, cfg.VersionIDs = true, r.VersionIDs
	cfg.Enabled, cfg.Mandatory, cfg.CeilingNone = deref(r.Enabled), deref(r.Mandatory), deref(r.CeilingNone)
	cfg.Ceiling = r.Ceiling
	for _, p := range r.Periods {
		cfg.AllowedPeriods = append(cfg.AllowedPeriods, PeriodKind(p))
	}
	cfg.CalendarTZ = r.CalendarTZ
	if r.WeekStart != nil {
		v := int(*r.WeekStart)
		cfg.WeekStart = &v
	}
	cfg.CooldownSeconds = r.CooldownSecs
	if r.Scope != nil {
		cfg.Scope = LimitScope(*r.Scope)
	}
	cfg.CountBonusFunded, cfg.LossMayGoNegative = r.CountBonus, r.LossNeg
	cfg.PostSessionBreakSeconds, cfg.ConcurrentChainAggregation = r.BreakSecs, r.Concurrent
	if !deref(r.TTLNone) {
		cfg.PendingIntentTTLSeconds = r.TTLSecs
	}
	return cfg, nil
}

// EffectiveLimitInputs is everything S4 reads to decide a limit check for one kind and asset.
type EffectiveLimitInputs struct {
	Config UsageConfig
	// Realized holds the realized state of every key of the family visible to the session: the anchor
	// account's account-scope rows and the Person's tenant_person rows, for every period (including a
	// legacy scope after a configuration scope change). HasValue with a nil Value is a removal.
	Realized []LimitState
}

// ReadEffectiveLimitInputs reads the composed configuration and the realized player limits of one
// (kind, asset) for the anchor account. The tx must be a tenant-scoped (staff / system) session of
// the request tenant (usageGuard: a player session, another tenant's session or a non-READ-COMMITTED transaction is refused).
func ReadEffectiveLimitInputs(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID, kind ControlKind, asset *string) (EffectiveLimitInputs, error) {
	// Same scope guard as the usage readers: a player-scoped session sees only its own rows and a session of another
	// tenant sees none, and either would read as "no limit" (ledger-finance F9).
	if err := usageGuard(ctx, tx, tenantID); err != nil {
		return EffectiveLimitInputs{}, err
	}
	acct, err := loadAccount(ctx, tx, tenantID, accountID)
	if err != nil {
		return EffectiveLimitInputs{}, err
	}
	cfg, err := ReadUsageConfig(ctx, tx, tenantID, acct.BrandID, kind, asset, nil)
	if err != nil {
		return EffectiveLimitInputs{}, err
	}
	all, err := CurrentLimits(ctx, tx, tenantID, accountID)
	if err != nil {
		return EffectiveLimitInputs{}, err
	}
	out := EffectiveLimitInputs{Config: cfg}
	for _, st := range all {
		if st.Kind != kind {
			continue
		}
		if (st.AssetCode == nil) != (asset == nil) || (asset != nil && *st.AssetCode != *asset) {
			continue
		}
		out.Realized = append(out.Realized, st)
	}
	return out, nil
}
