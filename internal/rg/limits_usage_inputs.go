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

// ReadUsageConfig composes the configuration in force at `at` (nil = now) for the tenant's licence
// jurisdiction (derived server-side, never from a request).
func ReadUsageConfig(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID, kind ControlKind, asset *string, at *time.Time) (UsageConfig, error) {
	jur, err := tenantJurisdiction(ctx, tx, tenantID)
	if err != nil {
		return UsageConfig{}, err
	}
	cfg := UsageConfig{Kind: kind, AssetCode: asset}
	if jur == nil {
		return cfg, nil
	}
	var enabled, mandatory, none, lossNeg, conc, ttlNone *bool
	var scope *string
	var periods []string
	var tz *string
	var ws *int16
	var cooldown, brk, ttl *int64
	var countBonus *bool
	err = tx.QueryRow(ctx, `SELECT found, version_ids, enabled, mandatory, ceiling::text, ceiling_none, allowed_periods,
			calendar_tz, week_start, extract(epoch FROM increase_cooldown)::bigint, scope, count_bonus_funded,
			loss_may_go_negative, extract(epoch FROM post_session_break)::bigint, concurrent_chain_aggregation,
			extract(epoch FROM pending_intent_ttl)::bigint, pending_intent_ttl_none
		FROM rg_limit_config_compose($1, $2, $3, $4, $5, COALESCE($6::timestamptz, clock_timestamp()))`,
		*jur, tenantID, brandID, string(kind), asset, at,
	).Scan(&cfg.Found, &cfg.VersionIDs, &enabled, &mandatory, &cfg.Ceiling, &none, &periods, &tz, &ws, &cooldown,
		&scope, &countBonus, &lossNeg, &brk, &conc, &ttl, &ttlNone)
	if err != nil {
		return UsageConfig{}, fmt.Errorf("rg: read usage configuration: %w", err)
	}
	if !cfg.Found {
		return UsageConfig{Kind: kind, AssetCode: asset}, nil
	}
	cfg.Enabled, cfg.Mandatory, cfg.CeilingNone = deref(enabled), deref(mandatory), deref(none)
	for _, p := range periods {
		cfg.AllowedPeriods = append(cfg.AllowedPeriods, PeriodKind(p))
	}
	cfg.CalendarTZ = tz
	if ws != nil {
		v := int(*ws)
		cfg.WeekStart = &v
	}
	cfg.CooldownSeconds = cooldown
	if scope != nil {
		cfg.Scope = LimitScope(*scope)
	}
	cfg.CountBonusFunded, cfg.LossMayGoNegative = countBonus, lossNeg
	cfg.PostSessionBreakSeconds, cfg.ConcurrentChainAggregation = brk, conc
	if !deref(ttlNone) {
		cfg.PendingIntentTTLSeconds = ttl
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
// (kind, asset) for the anchor account. The tx must be a tenant-scoped (staff / system) session;
// a player session sees only its own rows.
func ReadEffectiveLimitInputs(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID, kind ControlKind, asset *string) (EffectiveLimitInputs, error) {
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
