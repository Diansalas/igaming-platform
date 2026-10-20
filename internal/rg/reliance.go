package rg

// lic25 (migration 0187; F1 of lic14, SN-3 of lic23): WHICH jurisdiction's responsible-gaming rulebook governs.
//
// Before lic25 every RG read selected the jurisdiction of the tenant's LICENCE (tenants.licence_id), even for a scope governed by an
// operating basis whose jurisdiction differs, and refused a tenant that had an approved basis but no licence. The selection now
// follows the same rule as KYC (internal/kyc/enforcement.go resolveEnforcementJurisdiction, internal/operatingmarket/rulebook.go):
//
//   - A tenant with NO approved, active, in-force operating basis resolves exactly as before (licence jurisdiction, or none).
//   - EXPOSURE CHECKS (EvaluateAction: deposit, wager, launch) have a scope: the player's verified residence country (read in the same
//     transaction), the brand, the operation and the product. scopedSelection picks ONE jurisdiction: the governing basis row's, or the
//     tenant licence's when the scope has no row. A basis tenant whose scope cannot be resolved (no verified residence, a basis that does
//     not permit, no row and no licence) is an error: the caller denies. It is never read as "no configuration".
//   - ACCOUNT-LEVEL calls have no operation or product (limit setting, options, time-out, operator bounds). They use tenantReliance: the
//     set of EVERY jurisdiction the tenant relies on (licence plus approved bases) and the strictest fold of their configurations
//     (foldCompose: the fold rg_limit_config_compose applies to its own rows; nothing is invented). A relied-on jurisdiction with no
//     floor in force makes an INCREASE or a REMOVAL fail closed; an initial value, a decrease, a cancel, a time-out and a
//     self-exclusion are never blocked by it.
//
// The player scope of a session (app.player_account_id) is cleared for the tenant-scoped reads and restored afterwards, as the 0121 /
// 0130 / 0135 technique and the KYC selection do; a restore failure is an error.

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// ErrRulebookUnavailable is wrapped by every "the governing jurisdiction cannot be selected" refusal of the scoped selection.
var ErrRulebookUnavailable = errors.New("rg: the responsible-gaming rulebook cannot be selected for this scope")

// cfgSel is the selection a configuration read uses: the jurisdictions to compose. Codes is sorted and distinct. Strict is true for a
// tenant with an approved operating basis; only then does a jurisdiction without a floor count as MissingFloors (a tenant with no basis
// keeps "no floor = no configuration" exactly). Incomplete: an approved basis whose jurisdiction could not be resolved, or bases and no
// jurisdiction at all.
type cfgSel struct {
	Codes      []string
	Strict     bool
	Incomplete bool
}

func singleSel(code *string) cfgSel {
	if code == nil {
		return cfgSel{}
	}
	return cfgSel{Codes: []string{*code}}
}

// withPlayerScopeCleared runs fn with app.player_account_id cleared and restores it afterwards (a restore failure is returned).
func withPlayerScopeCleared(ctx context.Context, tx pgx.Tx, fn func() error) error {
	var player string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.player_account_id', true), '')`).Scan(&player); err != nil {
		return fmt.Errorf("rg: read player scope: %w", err)
	}
	if player != "" {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.player_account_id', '', true)`); err != nil {
			return fmt.Errorf("rg: clear player scope: %w", err)
		}
	}
	err := fn()
	if player != "" {
		if _, rerr := tx.Exec(ctx, `SELECT set_config('app.player_account_id', $1, true)`, player); rerr != nil {
			return fmt.Errorf("rg: restore player scope: %w", rerr)
		}
	}
	return err
}

// licenceCode is the tenant licence's jurisdiction code, nil when the tenant has no licence, an error when the chain is unusable
// (the pre-lic25 behaviour of tenantJurisdiction). The player scope must already be cleared.
func licenceCode(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (*string, error) {
	chain, err := jurisdiction.ResolveLicensingChain(ctx, tx, tenantID)
	if err != nil {
		return nil, fmt.Errorf("rg: tenant jurisdiction: %w", err)
	}
	switch chain.Outcome {
	case jurisdiction.LicensingResolved:
		code := chain.JurisdictionCode
		return &code, nil
	case jurisdiction.LicensingNoLicence:
		// Unchanged: no licence = no configuration (nil). With enforcement ON this is rg_config_missing (fail closed); with it off
		// players set limits with no ceiling (G2 in docs/plans/licensing-audit-enforcement.md, readiness R-RG-CONFIG covers activation).
		return nil, nil
	default:
		// licence_invalid (tenant not visible / dangling licence): an error, so the caller refuses.
		return nil, fmt.Errorf("rg: tenant licensing chain unusable: %s", chain.InvalidReason)
	}
}

// licenceCodeOf is licenceCode for a tx whose player scope is not cleared yet.
func licenceCodeOf(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (code *string, err error) {
	err = withPlayerScopeCleared(ctx, tx, func() error {
		var e error
		code, e = licenceCode(ctx, tx, tenantID)
		return e
	})
	return code, err
}

// tenantReliance returns the set of every jurisdiction the tenant relies on: the licence jurisdiction plus the jurisdictions of its
// approved, active, in-force operating bases. The tenant id is the session's own (asserted by every read).
func tenantReliance(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (cfgSel, error) {
	var sel cfgSel
	err := withPlayerScopeCleared(ctx, tx, func() error {
		lic, err := licenceCode(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		var asOf time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&asOf); err != nil {
			return fmt.Errorf("rg: read clock: %w", err)
		}
		has, err := operatingmarket.TenantHasApprovedBasis(ctx, tx, tenantID, asOf)
		if err != nil {
			return fmt.Errorf("rg: %w", err)
		}
		if !has {
			sel = singleSel(lic)
			return nil
		}
		var complete bool
		var codes []string
		if err := tx.QueryRow(ctx, `SELECT complete, codes FROM rg_basis_jurisdictions($1, $2)`, tenantID, asOf).Scan(&complete, &codes); err != nil {
			return fmt.Errorf("rg: read basis jurisdictions: %w", err)
		}
		if lic != nil {
			codes = append(codes, *lic)
		}
		sel = cfgSel{Codes: sortedDistinct(codes), Strict: true}
		sel.Incomplete = !complete || len(sel.Codes) == 0
		return nil
	})
	if err != nil {
		return cfgSel{}, err
	}
	return sel, nil
}

func sortedDistinct(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// scopeOf maps an enforcement action onto the operating-market operation and product (the KYC mapping).
func scopeOf(a Action) (operation string, product *string) {
	switch a {
	case ActionDeposit:
		return "deposit", nil
	case ActionSportsbookWager:
		p := "sportsbook"
		return "wagering", &p
	default: // casino wager and casino launch
		p := "casino"
		return "wagering", &p
	}
}

// scopedSelection selects the ONE jurisdiction whose rulebook governs an exposure action of the player. See the file comment.
func scopedSelection(ctx context.Context, tx pgx.Tx, p ActionParams) (cfgSel, error) {
	var sel cfgSel
	err := withPlayerScopeCleared(ctx, tx, func() error {
		var asOf time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&asOf); err != nil {
			return fmt.Errorf("rg: read clock: %w", err)
		}
		has, err := operatingmarket.TenantHasApprovedBasis(ctx, tx, p.TenantID, asOf)
		if err != nil {
			return fmt.Errorf("rg: %w", err)
		}
		lic, err := licenceCode(ctx, tx, p.TenantID)
		if err != nil {
			return err
		}
		if !has {
			sel = singleSel(lic)
			return nil
		}
		country, ok, err := kyc.ReadMarketResidenceEvidence(ctx, tx, p.TenantID, p.PlayerAccountID)
		if err != nil {
			return fmt.Errorf("rg: %w", err)
		}
		if !ok {
			return fmt.Errorf("%w: no verified residence country (basis_scope_unavailable)", ErrRulebookUnavailable)
		}
		op, product := scopeOf(p.Action)
		q := operatingmarket.RulebookQuery{TenantID: p.TenantID, CountryCode: country, OperationCode: op, ProductCode: product, AsOf: asOf}
		if p.BrandID != uuid.Nil {
			b := p.BrandID
			q.BrandID = &b
		}
		rb, err := operatingmarket.ResolveRulebookJurisdiction(ctx, tx, q)
		if err != nil {
			return fmt.Errorf("rg: %w", err)
		}
		switch rb.State {
		case operatingmarket.RulebookBasis:
			var code string
			if err := tx.QueryRow(ctx, `SELECT code FROM jurisdictions WHERE id = $1`, rb.JurisdictionID).Scan(&code); err != nil {
				return fmt.Errorf("%w: basis jurisdiction unreadable: %v", ErrRulebookUnavailable, err)
			}
			sel = cfgSel{Codes: []string{code}, Strict: true}
		case operatingmarket.RulebookLegacy:
			// The scope has no row: the tenant's own licence governs, and a tenant with bases but no licence is never default-allow.
			if lic == nil {
				return fmt.Errorf("%w: the scope has no basis row and the tenant has no licence (basis_scope_unavailable)", ErrRulebookUnavailable)
			}
			sel = cfgSel{Codes: []string{*lic}, Strict: true}
		default:
			return fmt.Errorf("%w: %s", ErrRulebookUnavailable, rb.Reason)
		}
		return nil
	})
	if err != nil {
		return cfgSel{}, err
	}
	return sel, nil
}

// ---- the configuration read and the strictest fold -------------------------------------------------------------------------

// rawCompose is one row of rg_limit_config_compose, with intervals as whole seconds.
type rawCompose struct {
	Found                                                  bool
	VersionIDs                                             []uuid.UUID
	Enabled, Mandatory, CeilingNone                        *bool
	Ceiling                                                *string
	Periods                                                []string
	CalendarTZ                                             *string
	WeekStart                                              *int16
	CooldownSecs                                           *int64
	Scope                                                  *string
	CountBonus, LossNeg, Concurrent, TTLNone, DurationsAny *bool
	BreakSecs, TTLSecs                                     *int64
	DurationsSecs                                          []int64
}

func readRaw(ctx context.Context, tx pgx.Tx, jur string, tenantID, brandID uuid.UUID, kind ControlKind, asset *string, at *time.Time) (rawCompose, error) {
	var r rawCompose
	err := tx.QueryRow(ctx, `SELECT found, version_ids, enabled, mandatory, ceiling::text, ceiling_none, allowed_periods,
			calendar_tz, week_start, extract(epoch FROM increase_cooldown)::bigint, scope, count_bonus_funded,
			loss_may_go_negative, extract(epoch FROM post_session_break)::bigint, concurrent_chain_aggregation,
			extract(epoch FROM pending_intent_ttl)::bigint, pending_intent_ttl_none, allowed_durations_any,
			(SELECT array_agg(extract(epoch FROM x)::bigint ORDER BY x) FROM unnest(allowed_durations) AS x)
		FROM rg_limit_config_compose($1, $2, $3, $4, $5, COALESCE($6::timestamptz, clock_timestamp()))`,
		jur, tenantID, brandID, string(kind), asset, at,
	).Scan(&r.Found, &r.VersionIDs, &r.Enabled, &r.Mandatory, &r.Ceiling, &r.CeilingNone, &r.Periods, &r.CalendarTZ, &r.WeekStart,
		&r.CooldownSecs, &r.Scope, &r.CountBonus, &r.LossNeg, &r.BreakSecs, &r.Concurrent, &r.TTLSecs, &r.TTLNone, &r.DurationsAny, &r.DurationsSecs)
	if err != nil {
		return rawCompose{}, fmt.Errorf("rg: compose configuration: %w", err)
	}
	return r, nil
}

// readCombined composes every jurisdiction of the selection and folds the ones that have a floor. missing is true when the tenant is
// strict (has an approved basis) and the selection is incomplete or a selected jurisdiction has no floor in force. An error means the
// fold could not be decided; tzConflict is reported separately (it matters only to a caller that measures windows).
func readCombined(ctx context.Context, tx pgx.Tx, sel cfgSel, tenantID, brandID uuid.UUID, kind ControlKind, asset *string, at *time.Time) (r rawCompose, missing, tzConflict bool, err error) {
	raws := make([]rawCompose, 0, len(sel.Codes))
	for _, code := range sel.Codes {
		x, e := readRaw(ctx, tx, code, tenantID, brandID, kind, asset, at)
		if e != nil {
			return rawCompose{}, false, false, e
		}
		raws = append(raws, x)
	}
	r, anyMissing, tzConflict := foldCompose(raws)
	return r, sel.Strict && (sel.Incomplete || anyMissing), tzConflict, nil
}

// foldCompose folds per-jurisdiction compositions with the rules of rg_limit_config_compose (migration 0135): the numeric ceiling is the
// minimum, "ceiling none" only when every member attests none, the cooling-off and post-session break the maximum, enabled / mandatory /
// reconfirmation / count-bonus / concurrent-chain by OR, a loss may go negative only when every member attests it, option sets (periods,
// durations) by UNION (an option set can only be widened by a stricter rule, never narrowed: the 0135 write trigger enforces the same
// direction inside a jurisdiction), the scope the broadest, the pending-intent ttl none when any member says none. calendar_tz /
// week_start are not comparable: when members disagree tzConflict is true and the value of the first member is returned. Members
// without a floor in force are skipped and reported through anyMissing. A one-member fold returns that member unchanged.
func foldCompose(raws []rawCompose) (out rawCompose, anyMissing, tzConflict bool) {
	var found []rawCompose
	for _, x := range raws {
		if x.Found {
			found = append(found, x)
		} else {
			anyMissing = true
		}
	}
	switch len(found) {
	case 0:
		return rawCompose{}, anyMissing, false
	case 1:
		return found[0], anyMissing, false
	}
	out.Found = true
	ids := map[uuid.UUID]bool{}
	periods := map[string]bool{}
	durs := map[int64]bool{}
	var ceilings []*big.Int
	for i, x := range found {
		for _, id := range x.VersionIDs {
			if !ids[id] {
				ids[id] = true
				out.VersionIDs = append(out.VersionIDs, id)
			}
		}
		out.Enabled = orBool(out.Enabled, x.Enabled)
		out.Mandatory = orBool(out.Mandatory, x.Mandatory)
		out.CountBonus = orBool(out.CountBonus, x.CountBonus)
		out.Concurrent = orBool(out.Concurrent, x.Concurrent)
		out.DurationsAny = orBool(out.DurationsAny, x.DurationsAny)
		out.TTLNone = orBool(out.TTLNone, x.TTLNone)
		out.CeilingNone = andBool(out.CeilingNone, x.CeilingNone, i == 0)
		out.LossNeg = andBool(out.LossNeg, x.LossNeg, i == 0)
		out.CooldownSecs = maxInt(out.CooldownSecs, x.CooldownSecs)
		out.BreakSecs = maxInt(out.BreakSecs, x.BreakSecs)
		out.TTLSecs = maxInt(out.TTLSecs, x.TTLSecs)
		for _, p := range x.Periods {
			periods[p] = true
		}
		for _, d := range x.DurationsSecs {
			durs[d] = true
		}
		if x.Ceiling != nil {
			if c, ok := new(big.Int).SetString(*x.Ceiling, 10); ok {
				ceilings = append(ceilings, c)
			} else {
				// An unparsable ceiling cannot be folded: keep the text so the caller's own parse fails closed.
				out.Ceiling = x.Ceiling
			}
		}
		if x.Scope != nil && (out.Scope == nil || scopeRank(*x.Scope) > scopeRank(*out.Scope)) {
			s := *x.Scope
			out.Scope = &s
		}
		if x.CalendarTZ != nil {
			if out.CalendarTZ == nil {
				out.CalendarTZ = x.CalendarTZ
			} else if *out.CalendarTZ != *x.CalendarTZ {
				tzConflict = true
			}
		}
		if x.WeekStart != nil {
			if out.WeekStart == nil {
				out.WeekStart = x.WeekStart
			} else if *out.WeekStart != *x.WeekStart {
				tzConflict = true
			}
		}
	}
	if out.Ceiling == nil && len(ceilings) > 0 {
		m := ceilings[0]
		for _, c := range ceilings[1:] {
			if c.Cmp(m) < 0 {
				m = c
			}
		}
		s := m.String()
		out.Ceiling = &s
	}
	for p := range periods {
		out.Periods = append(out.Periods, p)
	}
	sort.Strings(out.Periods)
	if out.DurationsAny == nil || !*out.DurationsAny {
		for d := range durs {
			out.DurationsSecs = append(out.DurationsSecs, d)
		}
		sort.Slice(out.DurationsSecs, func(i, j int) bool { return out.DurationsSecs[i] < out.DurationsSecs[j] })
	}
	// A none-attested pending-intent ttl dominates (compose: NULL when any member says none).
	if out.TTLNone != nil && *out.TTLNone {
		out.TTLSecs = nil
	}
	sort.Slice(out.VersionIDs, func(i, j int) bool { return out.VersionIDs[i].String() < out.VersionIDs[j].String() })
	return out, anyMissing, tzConflict
}

func scopeRank(s string) int {
	switch s {
	case "account":
		return 0
	case "tenant_person":
		return 1
	case "platform_person":
		return 2
	}
	return -1
}

func orBool(a, b *bool) *bool {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	v := *a || *b
	return &v
}

// andBool is the AND over the non-nil members (first is true for the first member, which seeds the accumulator).
func andBool(a, b *bool, first bool) *bool {
	if first || a == nil {
		return b
	}
	if b == nil {
		return a
	}
	v := *a && *b
	return &v
}

func maxInt(a, b *int64) *int64 {
	if a == nil {
		return b
	}
	if b == nil {
		return a
	}
	if *b > *a {
		return b
	}
	return a
}
