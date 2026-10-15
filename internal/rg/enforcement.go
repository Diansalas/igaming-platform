package rg

// ADR 0115 SLICE S4-min (completion audit A6): RG LIMIT ENFORCEMENT inside the money transactions.
//
// Status: deposit, loss and wager limit enforcement, the config-missing / limit-required fail-closed
// answers and the time-out predicate are IMPLEMENTED here (the time-out is in EvaluateEligibility).
// NOT BUILT, by owner direction: session-duration and reality-check enforcement (PROVIDER DEPENDENT /
// future, recorded in docs/plans/a6-rg-enforcement-record.md).
//
// EvaluateAction is the ADR 0115 3.1 boundary. It is called by the SAME five hook points that call
// EvaluateEligibility (casino launch, casino bet, sportsbook placement, deposit T1, deposit T2), inside
// the caller's money transaction, after the status gates and L0.1/L0.2 and before L0.5 / L1-L3 (the
// L0.4 person lock it takes is the one EvaluateEligibility already takes: no new lock class).
//
// THE ENABLE. Limit enforcement is behind a per-tenant / per-brand switch (rg_enforcement_activations,
// migration 0158) that is OFF by default and can only be turned ON (append-only, a one-way ratchet).
// With it off, EvaluateAction is EvaluateEligibility: self-exclusion, time-out, account and wallet
// status are still enforced (they need no configuration), no limit is read and no
// rg_config_missing is raised. With it on, ADR 0115 section 6 applies exactly: no floor in force for
// the kind is rg_config_missing (deposits, new wagers and launches are refused), a mandatory kind with
// no value and no numeric ceiling is rg_limit_required, and a limit is enforced as
// usage + amount <= limit. The read of the switch is part of the evaluation: a transaction that is not
// the request tenant's tenant-scoped session (RLS would answer "no row" = "disabled") is an ERROR, never
// "disabled".
//
// FAIL CLOSED. Every error (a read error, an undeclared ledger leg, a REPEATABLE READ / SERIALIZABLE
// transaction, a lock or statement timeout, a Person-scope session aggregation that cannot be measured)
// is returned as an error wrapping ErrEvaluationUnavailable: the caller's transaction rolls back, nothing
// posts, no intent progresses, no session is minted. An error is never read as zero usage.
//
// SETTLEMENT IS NEVER GATED. Wins, rollbacks, voids, settlements and stake returns do not call this
// function (casino/orchestrator.go postWin / postRollback, sportsbook settlement). Withdrawals have their
// own gate (internal/withdrawal, WD-RG-1) and are never blocked by limits.
//
// EFFECTIVE LIMIT (ADR 0115 2.2), per (kind, asset, period): the MIN of every realized account-scope row
// of the anchor account, every realized tenant_person row of the Person in this tenant (BOTH scopes are
// always read, so a configuration scope change account -> tenant_person cannot drop a limit, S2 hard
// condition (ii)), and the composed numeric ceiling (S2 hard condition (i): a confirm carries the value
// of an earlier request and re-checks no ceiling, so the ceiling clamp is applied HERE at read time).
// INTERPRETATION (recorded, reversible): a numeric ceiling is a hard cap on every period the kind allows,
// whether or not the player set a value for that period ("ceiling: the hard cap", ADR 0115 2.1); an
// attested-none ceiling bounds nothing.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Decision codes of the limit enforcement (ADR 0115 3.1 new codes). session_limit_reached is not
// produced: session enforcement is not built.
const (
	CodeConfigMissing       = "rg_config_missing"
	CodeLimitRequired       = "rg_limit_required"
	CodeDepositLimitReached = "deposit_limit_reached"
	CodeLossLimitReached    = "loss_limit_reached"
	CodeWagerLimitReached   = "wager_limit_reached"
)

// ErrEvaluationUnavailable is the fail-closed error class: the evaluation could not produce a decision.
// Callers must abort their transaction; it is NEVER a policy decision and must not be shown to a player
// as one. It is logged (event rg_evaluation_unavailable) where it is raised; delivery of the ADR 0102
// alert is ALERT-DELIVERY-1 (NOT IMPLEMENTED).
var ErrEvaluationUnavailable = errors.New("rg: evaluation unavailable")

func unavailable(reason string, err error) error {
	slog.Error("rg_evaluation_unavailable", "reason", reason, "error", err)
	if err == nil {
		return fmt.Errorf("%w: %s", ErrEvaluationUnavailable, reason)
	}
	return fmt.Errorf("%w: %s: %w", ErrEvaluationUnavailable, reason, err)
}

// Engineering operational settings (ADR 0115 3.3): bounds on how long the evaluation may wait for the
// person lock and how long its reads may run. They are NOT legal values. The statement timeout was sized
// from the heavy-player measurement recorded in docs/plans/a6-rg-enforcement-record.md.
const (
	DefaultActionLockTimeout      = 2 * time.Second
	DefaultActionStatementTimeout = 4 * time.Second
)

// Action is what the player is about to do.
type Action string

const (
	ActionDeposit         Action = "deposit"
	ActionCasinoLaunch    Action = "casino_launch"
	ActionCasinoWager     Action = "casino_wager"
	ActionSportsbookWager Action = "sportsbook_wager"
)

func (a Action) wagers() bool { return a == ActionCasinoWager || a == ActionSportsbookWager }
func (a Action) casino() bool { return a == ActionCasinoWager || a == ActionCasinoLaunch }

// ActionParams is EvaluateAction's input. Every identity field is server-resolved (like
// EligibilityParams). Amount is minor units of AssetCode: the deposit amount, or the stake (for a
// sportsbook bet the stake is the potential loss). Launch carries no amount.
type ActionParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
	Action          Action
	AssetCode       string
	Amount          int64
	// CandidateRef is the deposit intent being evaluated (ADR 0115 3.2): excluded from in-flight usage and
	// added once as Amount.
	CandidateRef *uuid.UUID

	// LockTimeout / StatementTimeout override the defaults (zero = default). A test seam and an operational
	// knob; never request input.
	LockTimeout, StatementTimeout time.Duration
}

func (p ActionParams) validate() error {
	if p.TenantID == uuid.Nil || p.BrandID == uuid.Nil || p.PlayerAccountID == uuid.Nil {
		return fmt.Errorf("%w: tenant_id, brand_id and player_account_id are required", ErrInvalidInput)
	}
	switch p.Action {
	case ActionDeposit, ActionCasinoWager, ActionSportsbookWager:
		if p.Amount <= 0 {
			return fmt.Errorf("%w: amount must be positive for %s", ErrInvalidInput, p.Action)
		}
	case ActionCasinoLaunch:
		if p.Amount != 0 {
			return fmt.Errorf("%w: a launch carries no amount", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: unknown action %q", ErrInvalidInput, p.Action)
	}
	if p.AssetCode == "" || len(p.AssetCode) > 32 {
		return fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	return nil
}

// EnforcementEnabled reports whether limit enforcement is switched on for the brand (a tenant-wide
// activation covers every brand). tx must be the request tenant's tenant-scoped session; anything else is
// an error (never "disabled").
func EnforcementEnabled(ctx context.Context, tx pgx.Tx, tenantID, brandID uuid.UUID) (bool, error) {
	var session string
	var on bool
	if err := tx.QueryRow(ctx, `SELECT COALESCE(current_setting('app.tenant_id', true), ''),
			EXISTS (SELECT 1 FROM rg_enforcement_activations WHERE tenant_id = $1 AND (brand_id IS NULL OR brand_id = $2))`,
		tenantID, brandID).Scan(&session, &on); err != nil {
		return false, fmt.Errorf("rg: read enforcement activation: %w", err)
	}
	if session != tenantID.String() {
		return false, ErrUsageScope
	}
	return on, nil
}

// EvaluateAction is the ADR 0115 3.1 enforcement boundary. See the file comment.
func EvaluateAction(ctx context.Context, tx pgx.Tx, p ActionParams) (Decision, error) {
	if err := p.validate(); err != nil {
		return Decision{}, err
	}
	elig := EligibilityParams{TenantID: p.TenantID, BrandID: p.BrandID, PlayerAccountID: p.PlayerAccountID, WalletID: p.WalletID}

	on, err := EnforcementEnabled(ctx, tx, p.TenantID, p.BrandID)
	if err != nil {
		return Decision{}, unavailable("enforcement switch", err)
	}
	if !on {
		// Limit enforcement is off for this brand: eligibility only (self-exclusion, time-out, status).
		return EvaluateEligibility(ctx, tx, elig)
	}

	// Isolation, scope and the READ COMMITTED requirement of the post-lock usage read (ADR 0115 3.1).
	if err := usageGuard(ctx, tx, p.TenantID); err != nil {
		return Decision{}, unavailable("scope or isolation guard", err)
	}
	restore, err := applyLocalTimeouts(ctx, tx, p)
	if err != nil {
		return Decision{}, unavailable("set timeouts", err)
	}

	dec, err := EvaluateEligibility(ctx, tx, elig)
	if err != nil {
		return Decision{}, unavailable("eligibility", err)
	}
	if !dec.Allowed {
		if rerr := restore(ctx); rerr != nil {
			return Decision{}, unavailable("restore timeouts", rerr)
		}
		return dec, nil
	}

	out, err := evaluateLimits(ctx, tx, p, dec)
	if err != nil {
		return Decision{}, unavailable("limits", err)
	}
	if rerr := restore(ctx); rerr != nil {
		return Decision{}, unavailable("restore timeouts", rerr)
	}
	return out, nil
}

// applyLocalTimeouts sets a transaction-local lock_timeout and statement_timeout for the evaluation and
// returns the function that puts the previous values back: the rest of the caller's transaction (the
// ledger posting, its row locks) must keep the timeouts it had.
func applyLocalTimeouts(ctx context.Context, tx pgx.Tx, p ActionParams) (func(context.Context) error, error) {
	lock, stmt := p.LockTimeout, p.StatementTimeout
	if lock <= 0 {
		lock = DefaultActionLockTimeout
	}
	if stmt <= 0 {
		stmt = DefaultActionStatementTimeout
	}
	var prevLock, prevStmt string
	if err := tx.QueryRow(ctx, `SELECT current_setting('lock_timeout'), current_setting('statement_timeout')`).Scan(&prevLock, &prevStmt); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)`,
		fmt.Sprintf("%dms", lock.Milliseconds()), fmt.Sprintf("%dms", stmt.Milliseconds())); err != nil {
		return nil, err
	}
	return func(ctx context.Context) error {
		_, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true), set_config('statement_timeout', $2, true)`, prevLock, prevStmt)
		return err
	}, nil
}

// limitDef is one effective limit of one (kind, period, scope): the minimum of its sources.
type limitDef struct {
	kind   ControlKind
	period PeriodKind
	scope  LimitScope
	value  *big.Int
	source string // the source of the minimum: player_account | player_tenant_person | ceiling
	window WindowSpec
}

func kindCode(k ControlKind) string {
	switch k {
	case ControlDeposit:
		return CodeDepositLimitReached
	case ControlLoss:
		return CodeLossLimitReached
	default:
		return CodeWagerLimitReached
	}
}

func periodRank(p PeriodKind) int {
	for i, x := range PeriodKinds {
		if x == p {
			return i
		}
	}
	return len(PeriodKinds)
}

func kindRank(k ControlKind) int {
	switch k {
	case ControlWager:
		return 0
	case ControlLoss:
		return 1
	default:
		return 2
	}
}

// kindPlan is the resolved configuration and effective limits of one kind.
type kindPlan struct {
	kind     ControlKind
	cfg      UsageConfig
	defs     []limitDef
	hasValue bool // the player holds at least one realized (non-removed) value for this kind and asset
}

func evaluateLimits(ctx context.Context, tx pgx.Tx, p ActionParams, dec Decision) (Decision, error) {
	jur, err := tenantJurisdiction(ctx, tx, p.TenantID)
	if err != nil {
		return Decision{}, err
	}
	asset := p.AssetCode

	kinds := []ControlKind{ControlDeposit}
	if p.Action.wagers() || p.Action == ActionCasinoLaunch {
		kinds = []ControlKind{ControlWager, ControlLoss}
	}

	// S3 hard condition (c): a Person-scope session aggregation cannot be measured (the sessions of the
	// Person's other accounts are not readable by one account's measurement). It fails closed rather than
	// silently under-counting. Session-duration limits themselves are not enforced (see the file comment).
	sess, err := readUsageConfigFor(ctx, tx, jur, p.TenantID, p.BrandID, ControlSessionDuration, nil, nil)
	if err != nil {
		return Decision{}, err
	}
	if sess.Found && sess.Scope == LimitScopeTenantPerson && deref(sess.ConcurrentChainAggregation) {
		return Decision{}, errors.New("session_duration concurrent_chain_aggregation at tenant_person scope cannot be measured (S3 Residual 2)")
	}

	// Phase 1: configuration per kind. No floor in force for a kind is rg_config_missing (ADR 0115 section 6).
	plans := make([]*kindPlan, 0, len(kinds))
	for _, k := range kinds {
		cfg, err := readUsageConfigFor(ctx, tx, jur, p.TenantID, p.BrandID, k, &asset, nil)
		if err != nil {
			return Decision{}, err
		}
		if !cfg.Found {
			return denial(dec, CodeConfigMissing, "no responsible-gaming configuration is in force for "+string(k), map[string]any{
				"kind": string(k), "asset_code": asset, "action": string(p.Action),
			}), nil
		}
		if p.Action.casino() && deref(cfg.CountBonusFunded) {
			// S3 condition (a): no window-safe bonus spec exists, so a casino path with count_bonus_funded
			// must not be enabled. The measurement fails closed on a locked-bonus shape anyway; refuse up front.
			return Decision{}, fmt.Errorf("count_bonus_funded=true for %s is not supported on the casino path (S3 condition (a))", k)
		}
		plans = append(plans, &kindPlan{kind: k, cfg: cfg})
	}

	// Phase 2: the player's realized values, both scopes (CurrentLimits returns the anchor account's rows and
	// the Person's tenant_person rows). A removal (HasValue with a nil Value) bounds nothing.
	states, err := CurrentLimits(ctx, tx, p.TenantID, p.PlayerAccountID)
	if err != nil {
		return Decision{}, err
	}
	for _, pl := range plans {
		if err := pl.build(states, asset); err != nil {
			return Decision{}, err
		}
		// mandatory with no value and no numeric ceiling: rg_limit_required.
		if pl.cfg.Mandatory && !pl.hasValue && pl.cfg.Ceiling == nil {
			return denial(dec, CodeLimitRequired, "a "+string(pl.kind)+" limit is mandatory and none is set", map[string]any{
				"kind": string(pl.kind), "asset_code": asset, "action": string(p.Action),
				"config_version_ids": uuidStrings(pl.cfg.VersionIDs),
			}), nil
		}
	}

	if p.Action == ActionCasinoLaunch {
		return dec, nil // config and mandatory only: a launch moves no money and measures no usage
	}

	var defs []limitDef
	for _, pl := range plans {
		defs = append(defs, pl.defs...)
	}
	if len(defs) == 0 {
		return dec, nil // no limit of any source applies: nothing to measure
	}
	sort.SliceStable(defs, func(i, j int) bool {
		if a, b := kindRank(defs[i].kind), kindRank(defs[j].kind); a != b {
			return a < b
		}
		if a, b := periodRank(defs[i].period), periodRank(defs[j].period); a != b {
			return a < b
		}
		return defs[i].scope < defs[j].scope
	})

	amount := big.NewInt(p.Amount)
	if p.Action == ActionDeposit {
		return checkDeposit(ctx, tx, p, dec, plans[0], defs, amount)
	}
	return checkStake(ctx, tx, p, dec, plans, defs, amount)
}

func denial(dec Decision, code, msg string, figures map[string]any) Decision {
	figures["code"] = code
	return Decision{Allowed: false, Code: code, Message: msg, PersonID: dec.PersonID, Figures: figures}
}

// build derives the effective limits of the kind from the configuration and the realized player rows.
func (pl *kindPlan) build(states []LimitState, asset string) error {
	type key struct {
		period PeriodKind
		scope  LimitScope
	}
	best := map[key]*limitDef{}
	consider := func(k key, v *big.Int, source string) {
		if cur, ok := best[k]; !ok || v.Cmp(cur.value) < 0 {
			best[k] = &limitDef{kind: pl.kind, period: k.period, scope: k.scope, value: new(big.Int).Set(v), source: source}
		}
	}
	periods := map[PeriodKind]bool{}
	for _, p := range pl.cfg.AllowedPeriods {
		periods[p] = true
	}
	for _, st := range states {
		if st.Kind != pl.kind || st.AssetCode == nil || *st.AssetCode != asset || st.Period == nil {
			continue
		}
		if !st.HasValue || st.Value == nil {
			continue // never realized, or removed
		}
		v, ok := new(big.Int).SetString(*st.Value, 10)
		// (Cmp, not the sign accessor: the signer-pin tripwire of limits_s2_review_test.go counts textual proof-signer calls.)
		if !ok || v.Cmp(new(big.Int)) < 0 {
			return fmt.Errorf("rg: unparsable realized limit value for %s", pl.kind)
		}
		pl.hasValue = true
		periods[*st.Period] = true
		switch st.Scope {
		case LimitScopeAccount:
			consider(key{*st.Period, LimitScopeAccount}, v, "player_account")
		case LimitScopeTenantPerson:
			consider(key{*st.Period, LimitScopeTenantPerson}, v, "player_tenant_person")
		default:
			return fmt.Errorf("rg: unexpected limit scope %q", st.Scope)
		}
	}
	if pl.cfg.Ceiling != nil {
		c, ok := new(big.Int).SetString(*pl.cfg.Ceiling, 10)
		if !ok {
			return errors.New("rg: unparsable ceiling")
		}
		scope := pl.cfg.Scope
		if scope != LimitScopeAccount && scope != LimitScopeTenantPerson {
			return fmt.Errorf("rg: unexpected configured scope %q", scope)
		}
		for p := range periods {
			consider(key{p, scope}, c, "ceiling")
		}
	}
	for k, d := range best {
		w := WindowSpec{Period: k.period}
		if pl.cfg.CalendarTZ != nil {
			w.CalendarTZ = *pl.cfg.CalendarTZ
		}
		if pl.cfg.WeekStart != nil {
			w.WeekStart = *pl.cfg.WeekStart
		}
		d.window = w
		pl.defs = append(pl.defs, *d)
	}
	return nil
}

// windowSet dedupes the windows of the defs into the measurement's window list.
func windowSet(defs []limitDef) ([]WindowSpec, map[WindowSpec]int, error) {
	idx := map[WindowSpec]int{}
	var specs []WindowSpec
	for _, d := range defs {
		if _, ok := idx[d.window]; !ok {
			idx[d.window] = len(specs)
			specs = append(specs, d.window)
		}
	}
	if len(specs) > MaxUsageWindows {
		return nil, nil, fmt.Errorf("rg: %d distinct windows exceed the bound %d", len(specs), MaxUsageWindows)
	}
	return specs, idx, nil
}

func measureScope(defs []limitDef) LimitScope {
	for _, d := range defs {
		if d.scope == LimitScopeTenantPerson {
			return LimitScopeTenantPerson
		}
	}
	return LimitScopeAccount
}

func pick(a Amounts, scope LimitScope) (*big.Int, error) {
	if scope == LimitScopeTenantPerson {
		if a.Person == nil {
			return nil, errors.New("rg: person-scope figure missing")
		}
		return a.Person, nil
	}
	return a.Account, nil
}

func checkDeposit(ctx context.Context, tx pgx.Tx, p ActionParams, dec Decision, pl *kindPlan, defs []limitDef, amount *big.Int) (Decision, error) {
	specs, idx, err := windowSet(defs)
	if err != nil {
		return Decision{}, err
	}
	u, err := MeasureDepositUsage(ctx, tx, UsageRequest{
		TenantID: p.TenantID, PlayerAccountID: p.PlayerAccountID, Scope: measureScope(defs), AssetCode: p.AssetCode,
		Windows: specs, ExcludeIntentID: p.CandidateRef,
	})
	if err != nil {
		return Decision{}, err
	}
	for _, d := range defs {
		w := u.Windows[idx[d.window]]
		usage, err := pick(w.Total, d.scope)
		if err != nil {
			return Decision{}, err
		}
		if new(big.Int).Add(usage, amount).Cmp(d.value) > 0 {
			return denial(dec, kindCode(d.kind), "the deposit would exceed the "+string(d.period)+" deposit limit",
				limitFigures(p, d, usage, amount, w.Start, u.AsOf, pl.cfg.VersionIDs, u.AuditInputs())), nil
		}
	}
	return dec, nil
}

func checkStake(ctx context.Context, tx pgx.Tx, p ActionParams, dec Decision, plans []*kindPlan, defs []limitDef, amount *big.Int) (Decision, error) {
	specs, idx, err := windowSet(defs)
	if err != nil {
		return Decision{}, err
	}
	req := UsageRequest{TenantID: p.TenantID, PlayerAccountID: p.PlayerAccountID, Scope: measureScope(defs), AssetCode: p.AssetCode, Windows: specs}
	versions := map[ControlKind][]uuid.UUID{}
	for _, pl := range plans {
		versions[pl.kind] = pl.cfg.VersionIDs
		switch pl.kind {
		case ControlWager:
			req.WagerCountsBonus = deref(pl.cfg.CountBonusFunded)
		case ControlLoss:
			req.LossCountsBonus = deref(pl.cfg.CountBonusFunded)
			req.LossMayGoNegative = deref(pl.cfg.LossMayGoNegative)
		}
	}
	u, err := MeasureStakeUsage(ctx, tx, req)
	if err != nil {
		return Decision{}, err
	}
	for _, d := range defs {
		w := u.Windows[idx[d.window]]
		var usage *big.Int
		var perr error
		if d.kind == ControlWager {
			usage, perr = pick(w.Wager, d.scope)
		} else {
			usage, perr = pick(w.Loss, d.scope)
		}
		if perr != nil {
			return Decision{}, perr
		}
		if new(big.Int).Add(usage, amount).Cmp(d.value) > 0 {
			f := limitFigures(p, d, usage, amount, w.Start, u.AsOf, versions[d.kind], u.AuditInputs())
			if d.kind == ControlLoss {
				f["loss_raw_usage"] = pickOrEmpty(w.LossRaw, d.scope)
			}
			return denial(dec, kindCode(d.kind), "the stake would exceed the "+string(d.period)+" "+string(d.kind)+" limit", f), nil
		}
	}
	return dec, nil
}

func pickOrEmpty(a Amounts, scope LimitScope) string {
	v, err := pick(a, scope)
	if err != nil || v == nil {
		return ""
	}
	return v.String()
}

// limitFigures are the computed figures of a limit denial (ADR 0115 section 4; S3 condition (e)).
func limitFigures(p ActionParams, d limitDef, usage, amount *big.Int, windowStart, asOf time.Time, versions []uuid.UUID, inputs map[string]any) map[string]any {
	f := map[string]any{
		"kind": string(d.kind), "period": string(d.period), "scope": string(d.scope), "limit": d.value.String(), "limit_source": d.source,
		"usage": usage.String(), "attempted_amount": amount.String(), "asset_code": p.AssetCode, "action": string(p.Action),
		"window_start": windowStart.UTC().Format(time.RFC3339Nano), "as_of": asOf.UTC().Format(time.RFC3339Nano),
		"calendar_tz": d.window.CalendarTZ, "config_version_ids": uuidStrings(versions), "usage_inputs": inputs,
	}
	if p.CandidateRef != nil {
		f["candidate_ref"] = p.CandidateRef.String()
	}
	return f
}
