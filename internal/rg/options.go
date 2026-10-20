package rg

// ADR 0115 S4-min / S5-min: the SERVER-OWNED choices the player's RG screen offers, so no client hard-codes a
// period, a duration or a ceiling. Read-only; it decides nothing and enforces nothing.
//
// What is configuration and what is an engineering default:
//   - limit kinds, allowed periods, the ceiling, the scope and the increase cooling-off come from the composed
//     rg_limit_configs of the player's brand and the asset asked about (compliance values, OQ-2). With no
//     configuration in force, ConfigInForce is false and every period of the closed vocabulary is offered
//     (a protective initial value or decrease is accepted without configuration, ADR 0115 3.1).
//   - time-out durations come from the configured allowed_durations when the configuration lists them (only
//     whole hours are reachable through the API; others are omitted). With `any` or no configuration the
//     ENGINEERING DEFAULT list below is offered; the API accepts any whole number of hours up to
//     MaxTimeOutHours.
//   - self-exclusion has no configuration at all (ADR 0026): the list below is an ENGINEERING DEFAULT of
//     convenient choices, not a legal value; the API accepts any positive number of days, and an indefinite
//     exclusion.

import (
	"context"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DefaultTimeOutHours and DefaultSelfExclusionDays are engineering defaults (NOT legal values).
var (
	DefaultTimeOutHours      = []int{1, 24, 72, 168, 720}
	DefaultSelfExclusionDays = []int{7, 30, 90, 180, 365}
)

// LimitOption is what the player may set for one amount kind and the asset.
type LimitOption struct {
	Kind          ControlKind
	ConfigInForce bool
	Mandatory     bool
	// AllowedPeriods is the composed allowed_periods, or the whole vocabulary when no configuration is in force.
	AllowedPeriods []PeriodKind
	// Ceiling is the composed numeric maximum (NUMERIC(38,0) text); nil = none attested or no configuration.
	Ceiling                 *string
	Scope                   LimitScope
	IncreaseCooldownSeconds *int64
}

// Options is everything the player's RG screen needs from the server for one asset.
type Options struct {
	AssetCode string
	// EnforcementEnabled reports whether limit enforcement is switched on for the player's brand. Informational:
	// self-exclusion and time-out are always enforced.
	EnforcementEnabled bool
	Limits             []LimitOption
	TimeOutHours       []int
	SelfExclusionDays  []int
}

// ReadOptions composes the options for the account and asset. tx MUST be a tenant-scoped, non-player-scoped
// READ COMMITTED session of the account's tenant (db.Pool.WithTenant): the configuration is not player data,
// and the usage guards refuse a player-scoped session.
func ReadOptions(ctx context.Context, tx pgx.Tx, tenantID, accountID uuid.UUID, asset string) (Options, error) {
	if asset == "" || !assetRe.MatchString(asset) {
		return Options{}, invalidf("asset_code is required")
	}
	acct, err := loadAccount(ctx, tx, tenantID, accountID)
	if err != nil {
		return Options{}, err
	}
	on, err := EnforcementEnabled(ctx, tx, tenantID, acct.BrandID)
	if err != nil {
		return Options{}, err
	}
	sel, err := tenantReliance(ctx, tx, tenantID)
	if err != nil {
		return Options{}, err
	}
	out := Options{AssetCode: asset, EnforcementEnabled: on, SelfExclusionDays: append([]int(nil), DefaultSelfExclusionDays...)}
	for _, k := range []ControlKind{ControlDeposit, ControlLoss, ControlWager} {
		cfg, err := readUsageConfigFor(ctx, tx, sel, tenantID, acct.BrandID, k, &asset, nil)
		if err != nil {
			return Options{}, err
		}
		if cfg.MissingFloors {
			// lic25: a tenant with an approved basis must not be shown a partial truth (the options of the jurisdictions that do have a
			// floor) while another jurisdiction it relies on has none: refused, never "no configuration". Limit setting, time-outs and
			// self-exclusion do not depend on this read.
			return Options{}, refuse(RefusedConfigMissing, "a jurisdiction the tenant relies on has no "+string(k)+" floor in force")
		}
		o := LimitOption{Kind: k, ConfigInForce: cfg.Found, Mandatory: cfg.Mandatory, Ceiling: cfg.Ceiling, Scope: cfg.Scope,
			IncreaseCooldownSeconds: cfg.CooldownSeconds, AllowedPeriods: cfg.AllowedPeriods}
		if !cfg.Found {
			o.AllowedPeriods = append([]PeriodKind(nil), PeriodKinds...)
		}
		out.Limits = append(out.Limits, o)
	}
	to, err := composeConfig(ctx, tx, sel, tenantID, acct.BrandID, ControlTimeOut, nil)
	if err != nil {
		return Options{}, err
	}
	out.TimeOutHours = timeOutHours(to)
	return out, nil
}

func timeOutHours(c ComposedConfig) []int {
	if !c.Found || c.AllowedDurationsAny || len(c.AllowedDurationsSecs) == 0 {
		return append([]int(nil), DefaultTimeOutHours...)
	}
	var hours []int
	for _, s := range c.AllowedDurationsSecs {
		if s > 0 && s%3600 == 0 && s/3600 <= int64(MaxTimeOutHours) {
			hours = append(hours, int(s/3600))
		}
	}
	sort.Ints(hours)
	if len(hours) == 0 {
		// Configured, but only sub-hour / fractional-hour durations: none is reachable through a whole-hours API.
		return []int{}
	}
	return hours
}
