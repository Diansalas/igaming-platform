package risk

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrInvalidInput is returned for a structurally invalid RiskRequest
// caught before ever reaching the database - mirrors rg.ErrInvalidInput.
var ErrInvalidInput = errors.New("risk: invalid input")

// ErrConflictingRules is returned when two or more RuleConfigurableLimit
// rules match the same request at the SAME highest specificity for the
// same (LimitKind, TimeWindow) - a genuine configuration error the
// evaluator refuses to silently resolve by table order or any other
// implicit tie-break (directive §16's "the final decision must be
// deterministic," §22's "conflicting rules" fail-closed case). The
// caller MUST treat this exactly like any other Evaluate error - never as
// ALLOW.
var ErrConflictingRules = errors.New("risk: conflicting configurable rules matched at the same specificity")

// ErrMissingAmount is returned when a matched rule needs RiskRequest.Amount
// (a LimitMinAmount/LimitMaxAmount/LimitCumulativeAmount rule) but the
// request supplied zero - fail-closed: an amount-shaped rule can never be
// silently skipped just because the caller forgot to populate Amount.
var ErrMissingAmount = fmt.Errorf("%w: a matched rule requires a non-zero amount", ErrInvalidInput)

// ErrUnknownOperation is returned when req.Operation is not one of the
// Operation values this package recognizes (knownOperations, mirroring
// migration 0041's own CHECK constraint).
//
// Fail-closed for a caller-side typo: an unrecognized operation matches
// no rule at all, so before this check a single misspelled operation
// string resolved to a silent ALLOW for an operation that may well have
// had a HARD_LIMIT configured under its correct name (Stage 4H-B0-R6
// fail-closed audit, ADR 0031 §34).
var ErrUnknownOperation = fmt.Errorf("%w: unrecognized operation", ErrInvalidInput)

// ErrUnknownOutcome is returned if Evaluate ever produced a RiskDecision
// whose Outcome is not one of the three defined values - a self-check on
// its own output, so no internal code path can hand a caller a decision
// it cannot classify. Unreachable today; present because "unclassifiable"
// must resolve to an error, never to the caller's not-ALLOW-so-decline
// branch (ADR 0031 §34's four distinct outcomes).
var ErrUnknownOutcome = errors.New("risk: evaluator produced an unrecognized outcome")

// ErrTenantScopeMismatch is returned when tx's own PostgreSQL tenant
// context (app.tenant_id, set only by db.WithTenant) is absent or does
// not equal req.TenantID.
//
// This is a fail-closed gate on a genuine fail-OPEN: risk_rules' RLS read
// policy (migration 0041) returns a tenant's rules ONLY to a connection
// scoped to that tenant. Evaluated on an unscoped transaction
// (db.WithoutTenant), the same query silently returns platform-wide rules
// alone - every tenant-owned limit, including a tenant's own HARD_LIMIT,
// becomes invisible and the request resolves to ALLOW. Risk must not
// depend on every present and future caller remembering to open the right
// kind of transaction (ADR 0031 §34).
var ErrTenantScopeMismatch = errors.New("risk: transaction is not tenant-scoped to the request's tenant")

// ErrPlayerScopedConnection is returned when tx carries a player scope
// (app.player_account_id, set by db.WithPlayerScope). Every risk_rules
// RLS policy requires that setting to be NULL, so such a connection reads
// ZERO rules - which would resolve every request to ALLOW. risk_rules has
// no legitimate player-facing access path (migration 0041's own RLS
// comment), so this is refused outright rather than silently evaluated
// against an empty rule set.
var ErrPlayerScopedConnection = errors.New("risk: risk evaluation is not permitted on a player-scoped transaction")

// ErrMissingScopeContext is returned when at least one currently-
// EFFECTIVE rule configured for this operation is scoped by a dimension
// the request left empty.
//
// Generalizes the ErrMissingLicensingMode gate (ADR 0031 §10) to every
// optional scope dimension, for the identical reason: empty on the
// REQUEST side is not a wildcard (only empty on the RULE side is), so
// Rule.matches() alone would silently treat such a rule as a non-match -
// letting a caller that simply forgot to populate a field bypass a
// HARD_LIMIT authored for exactly this operation. Deliberately
// operation-wide rather than "only when the rule matches on every other
// dimension too": determining that requires the very matches() logic this
// gate exists to backstop. See ADR 0031 §34.
var ErrMissingScopeContext = fmt.Errorf("%w: a rule scoped by a dimension this request left empty is configured for this operation", ErrInvalidInput)

// ErrMissingJurisdiction is the ErrMissingScopeContext case for
// jurisdiction_code, kept as its own sentinel because it CHANGES a
// previously documented behavior: ADR 0031 §9 recorded that an empty
// JurisdictionCode "matches only jurisdiction-unscoped rules". It now
// fails closed instead, once any jurisdiction-scoped rule is effective
// for the operation - the same fail-open shape §10 already rejected for
// licensing mode. Root cause is unchanged and still open: nothing in this
// codebase resolves a per-player jurisdiction yet (TODO(jurisdiction),
// ADR 0031 §9/§34).
var ErrMissingJurisdiction = fmt.Errorf("%w: a jurisdiction-scoped rule is configured for this operation but the request supplied no jurisdiction_code", ErrMissingScopeContext)

// ErrMissingLicensingMode is returned when at least one rule configured
// for this operation is scoped by LicensingMode but the request supplied
// none (Stage 4G-FINAL Part D, specialist review finding). Unlike other
// optional scope dimensions, an empty RiskRequest.LicensingMode is NEVER
// a legitimate "not yet resolved" value the way JurisdictionCode's empty
// value can be - tenants.licensing_model is NOT NULL, so a real caller
// always has a real value to supply. Adversarial review considered the
// "blast radius" (one platform-wide LicensingMode-scoped rule requires
// EVERY tenant to supply LicensingMode for that operation) and confirmed
// it is the correct, intended behavior: a platform-wide rule is
// deliberately binding on every tenant, so every caller genuinely must
// resolve its own licensing mode once such a rule exists.
var ErrMissingLicensingMode = fmt.Errorf("%w: a licensing_mode-scoped rule is configured for this operation but the request supplied no licensing_mode", ErrMissingScopeContext)

// ErrMissingAsset is returned when an amount-shaped rule matches a
// request that carries no AssetCode, or when an asset-scoped rule is
// configured for this operation and the request left AssetCode empty.
//
// Before this gate, a max_amount/min_amount rule was compared against
// req.Amount with NO asset context whatsoever - i.e. with no way to know
// which asset's minor units either side of the comparison was in (ADR
// 0031 §34/§35). Only the cumulative case checked for an asset at all.
var ErrMissingAsset = fmt.Errorf("%w: an amount-shaped or asset-scoped rule requires the request to carry an asset_code", ErrMissingScopeContext)

// ErrMissingPlayer is returned when req.PlayerAccountID is nil.
//
// Every Operation this package defines is player-scoped, and a
// player-scoped rule (the MOST specific rule shape there is - an
// individual player's own override) is silently skipped by matches() for
// a request with uuid.Nil. Required unconditionally rather than
// per-operation: a future genuinely player-less operation (a retail
// agent-level or house-level check, ADR 0031 §21/§24 - none exists) must
// opt out here deliberately, with its own documented reasoning.
var ErrMissingPlayer = fmt.Errorf("%w: player_account_id is required", ErrInvalidInput)

// windowDuration returns w's rolling duration ending now() - only valid
// for a genuine rolling window (never WindowTransaction, which has no
// duration - callers must not call this for that case).
func windowDuration(w TimeWindow) (time.Duration, error) {
	switch w {
	case WindowRollingHour:
		return time.Hour, nil
	case WindowRollingDay:
		return 24 * time.Hour, nil
	case WindowRollingWeek:
		return 7 * 24 * time.Hour, nil
	case WindowRollingMonth:
		return 30 * 24 * time.Hour, nil
	default:
		return 0, fmt.Errorf("risk: unrecognized rolling time_window %q", w)
	}
}

// matches reports whether r applies to req - every non-nil/non-empty
// scope field on r must equal req's corresponding field exactly; a
// nil/empty field on r means "applies regardless" of that dimension.
//
// NOTE: matches() is intentionally NOT the fail-closed boundary. An empty
// dimension on the REQUEST side returning false here is exactly the
// silent-skip fail-open that Evaluate's own missing-scope gate
// (ErrMissingScopeContext and friends) exists to catch BEFORE this
// function's result can be acted on.
func (r Rule) matches(req RiskRequest) bool {
	if r.Operation != req.Operation {
		return false
	}
	if r.TenantID != nil && *r.TenantID != req.TenantID {
		return false
	}
	if r.BrandID != nil && *r.BrandID != req.BrandID {
		return false
	}
	if r.JurisdictionCode != "" && r.JurisdictionCode != req.JurisdictionCode {
		return false
	}
	if r.LicensingMode != "" && r.LicensingMode != req.LicensingMode {
		return false
	}
	if r.PlayerAccountID != nil && *r.PlayerAccountID != req.PlayerAccountID {
		return false
	}
	if r.Product != "" && r.Product != req.Product {
		return false
	}
	if r.ProviderID != "" && r.ProviderID != req.ProviderID {
		return false
	}
	if r.GameID != nil && (req.GameID == uuid.Nil || *r.GameID != req.GameID) {
		return false
	}
	if r.AssetCode != "" && r.AssetCode != req.AssetCode {
		return false
	}
	if r.PaymentMethod != "" && r.PaymentMethod != req.PaymentMethod {
		return false
	}
	return true
}

// isEffective reports whether r is currently active and within its
// effective window at instant t.
func (r Rule) isEffective(t time.Time) bool {
	if r.Status != RuleActive {
		return false
	}
	if t.Before(r.EffectiveFrom) {
		return false
	}
	return r.EffectiveUntil == nil || t.Before(*r.EffectiveUntil)
}

// missingScopeContext reports the fail-closed error for the first scope
// dimension that r narrows and req leaves empty, or nil if r can be
// safely evaluated against req.
//
// TenantID/BrandID/PlayerAccountID are absent from this list because
// Evaluate already requires all three on every request. GameID is
// included: a game-scoped rule cannot be proven inapplicable by a request
// that carries no game.
func (r Rule) missingScopeContext(req RiskRequest) error {
	switch {
	case r.JurisdictionCode != "" && req.JurisdictionCode == "":
		return fmt.Errorf("%w (rule %s)", ErrMissingJurisdiction, r.ID)
	case r.LicensingMode != "" && req.LicensingMode == "":
		return fmt.Errorf("%w (rule %s)", ErrMissingLicensingMode, r.ID)
	case r.AssetCode != "" && req.AssetCode == "":
		return fmt.Errorf("%w (rule %s is scoped to asset %s)", ErrMissingAsset, r.ID, r.AssetCode)
	case r.Product != "" && req.Product == "":
		return fmt.Errorf("%w: product (rule %s)", ErrMissingScopeContext, r.ID)
	case r.ProviderID != "" && req.ProviderID == "":
		return fmt.Errorf("%w: provider_id (rule %s)", ErrMissingScopeContext, r.ID)
	case r.PaymentMethod != "" && req.PaymentMethod == "":
		return fmt.Errorf("%w: payment_method (rule %s)", ErrMissingScopeContext, r.ID)
	case r.GameID != nil && req.GameID == uuid.Nil:
		return fmt.Errorf("%w: game_id (rule %s)", ErrMissingScopeContext, r.ID)
	}
	return nil
}

// verifyConnectionScope proves tx is scoped the way risk_rules' RLS
// policies require BEFORE any rule is read, so an incorrectly-scoped
// transaction can never produce a silently-empty or silently-partial rule
// set (see ErrTenantScopeMismatch/ErrPlayerScopedConnection).
//
// Reads the same two connection-level settings migration 0041's policies
// themselves read; deliberately local to this package rather than a new
// internal/db helper, since promoting it to shared infrastructure is an
// architect-owned change (docs/governance/change-control.md).
func verifyConnectionScope(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	var scopedTenant, scopedPlayer *string
	err := tx.QueryRow(ctx,
		`SELECT NULLIF(current_setting('app.tenant_id', true), ''), NULLIF(current_setting('app.player_account_id', true), '')`,
	).Scan(&scopedTenant, &scopedPlayer)
	if err != nil {
		return fmt.Errorf("risk: read connection scope: %w", err)
	}
	if scopedPlayer != nil {
		return fmt.Errorf("%w: app.player_account_id is set", ErrPlayerScopedConnection)
	}
	if scopedTenant == nil {
		return fmt.Errorf("%w: app.tenant_id is not set on this transaction", ErrTenantScopeMismatch)
	}
	parsed, err := uuid.Parse(*scopedTenant)
	if err != nil {
		return fmt.Errorf("%w: app.tenant_id %q is not a uuid", ErrTenantScopeMismatch, *scopedTenant)
	}
	if parsed != tenantID {
		return fmt.Errorf("%w: transaction is scoped to %s, request is for %s", ErrTenantScopeMismatch, parsed, tenantID)
	}
	return nil
}

// breach reports whether req breaches r, querying cumulative usage from
// the ledger when r.LimitKind is LimitCumulativeAmount. tx must already
// be tenant-scoped (verified by Evaluate). exponents is Evaluate's own
// per-call asset-exponent resolver - the single source of decimal
// exponent truth for the comparison (ADR 0031 §35).
func (r Rule) breach(ctx context.Context, tx pgx.Tx, req RiskRequest, exponents *assetExponents) (bool, error) {
	if !r.LimitKind.isAmountShaped() {
		// Unreachable given migration 0041's CHECK constraint, but the
		// evaluator must never silently ALLOW a limit_kind it doesn't
		// recognize (fail-closed for a malformed rule - directive §22).
		return false, fmt.Errorf("risk: unrecognized limit_kind %q", r.LimitKind)
	}

	// Every amount-shaped comparison needs BOTH sides denominated in the
	// same asset's minor units. Before this stage only the cumulative
	// case checked for an asset at all, so a min/max rule could be
	// compared with no asset context whatsoever (ADR 0031 §34/§35).
	if req.Amount == 0 {
		return false, ErrMissingAmount
	}
	if req.AssetCode == "" {
		return false, fmt.Errorf("%w (rule %s, limit_kind=%s)", ErrMissingAsset, r.ID, r.LimitKind)
	}
	reqExponent, err := exponents.forRequest(ctx, req.AssetCode)
	if err != nil {
		return false, err
	}
	if _, err := r.thresholdExponent(reqExponent); err != nil {
		return false, err
	}

	switch r.LimitKind {
	case LimitMinAmount:
		return req.Amount < r.Threshold, nil
	case LimitMaxAmount:
		return req.Amount > r.Threshold, nil
	case LimitCumulativeAmount:
		if req.PlayerAccountID == uuid.Nil {
			return false, ErrMissingPlayer
		}
		spec, ok := operationCumulativeSpecs[req.Operation]
		if !ok {
			return false, fmt.Errorf("%w: operation %q", ErrUnsupportedCumulativeOperation, req.Operation)
		}
		if err := spec.validate(); err != nil {
			return false, fmt.Errorf("%w (operation %q)", err, req.Operation)
		}
		dur, err := windowDuration(r.TimeWindow)
		if err != nil {
			return false, err
		}

		// Serializes concurrent evaluations of the SAME (tenant, player,
		// operation, limit_kind, asset) cumulative rule so two racing
		// requests can never both read a stale "under threshold" snapshot
		// and both proceed - identical pattern to rg.lockPerson's own
		// transaction-scoped advisory locking (and to the class L3
		// wallet_balance_projection row locks
		// ledger.LockProjectionsForPosting takes on the posting path),
		// released automatically at this
		// transaction's commit or rollback. Held until the CALLER's own
		// transaction commits (this function does not commit anything
		// itself), which is what actually closes the race: the caller
		// posts its own ledger effect inside the SAME transaction that
		// evaluated this Decision.
		lockKey := fmt.Sprintf("%s:%s:%s:%s:%s", req.TenantID, req.PlayerAccountID, req.Operation, r.LimitKind, req.AssetCode)
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext('risk_cumulative'), hashtext($1))`, lockKey); err != nil {
			return false, fmt.Errorf("risk: lock cumulative usage: %w", err)
		}

		windowStart := time.Now().UTC().Add(-dur)
		existing, err := cumulativeUsage(ctx, tx, spec, req, windowStart)
		if err != nil {
			return false, err
		}
		total := new(big.Int).Add(existing, big.NewInt(req.Amount))
		return total.Cmp(big.NewInt(r.Threshold)) > 0, nil
	default:
		return false, fmt.Errorf("risk: unrecognized limit_kind %q", r.LimitKind)
	}
}

// Evaluate is the single authoritative Risk & Limits decision boundary
// (ADR 0031 §2) - every caller (today: internal/casino's LaunchGame and
// postBet, the only two call sites in the repository) consults this
// instead of implementing its own limit comparison. tx must already be
// tenant-scoped (db.WithTenant) for req.TenantID; the RLS policy on
// risk_rules (migration 0041) is what actually restricts the SELECT below
// to req's own tenant's rules plus every platform-wide rule - and
// verifyConnectionScope proves that scope rather than trusting it.
//
// FAIL-CLOSED CONTRACT: any non-nil error MUST be treated by the caller
// exactly like a DENY - never as ALLOW, and never as a business decline
// carrying a reason code (an error means "the platform could not decide",
// not "the player's limits rejected this"). This includes a database
// error or timeout, an unrecognized limit_kind/rule_kind/operation, a
// conflicting-rule configuration (ErrConflictingRules), a wrongly-scoped
// transaction (ErrTenantScopeMismatch/ErrPlayerScopedConnection), missing
// request context (ErrMissingAmount, ErrMissingPlayer, ErrMissingAsset,
// ErrMissingJurisdiction, ErrMissingLicensingMode,
// ErrMissingScopeContext), an undeclared threshold denomination or an
// exponent mismatch (ErrMissingThresholdDenomination,
// ErrThresholdDenominationMismatch), and an unmeasurable cumulative rule
// (ErrUnsupportedCumulativeOperation, ErrInvalidCumulativeSpec,
// ErrUnrecognizedCumulativeLeg). Evaluate never returns a "soft" error
// meant to be interpreted as ALLOW - there is no such thing in this API.
//
// ALLOW / REVIEW / DENY / error are FOUR distinct outcomes, and an
// enforcement point must keep them distinct (ADR 0031 §34): REVIEW is
// semantically not DENY even where today's only consumer blocks on both.
func Evaluate(ctx context.Context, tx pgx.Tx, req RiskRequest) (RiskDecision, error) {
	if req.TenantID == uuid.Nil || req.BrandID == uuid.Nil || req.Operation == "" {
		return RiskDecision{}, fmt.Errorf("%w: tenant_id, brand_id, and operation are required", ErrInvalidInput)
	}
	if !IsKnownOperation(req.Operation) {
		return RiskDecision{}, fmt.Errorf("%w: %q", ErrUnknownOperation, req.Operation)
	}
	if req.PlayerAccountID == uuid.Nil {
		return RiskDecision{}, ErrMissingPlayer
	}
	if err := verifyConnectionScope(ctx, tx, req.TenantID); err != nil {
		return RiskDecision{}, err
	}

	rules, err := listEffectiveRules(ctx, tx, req.Operation)
	if err != nil {
		return RiskDecision{}, err
	}

	now := time.Now().UTC()
	exponents := &assetExponents{tx: tx}

	// Missing-scope fail-closed gate (ADR 0031 §34, generalizing §10's
	// licensing-mode gate to every optional dimension): checked against
	// every rule CONFIGURED and currently EFFECTIVE for this operation,
	// not just ones that would otherwise match on every other dimension.
	// Conservative by design - Rule.matches() alone cannot catch this,
	// because an empty request-side dimension against a rule that scopes
	// it simply returns false (a non-match), silently excluding the rule
	// from ever applying rather than erroring. A disabled or not-yet-
	// effective rule is not a live policy and must never force every
	// unrelated request for this operation to supply a dimension.
	for _, r := range rules {
		if !r.isEffective(now) {
			continue
		}
		if err := r.missingScopeContext(req); err != nil {
			return RiskDecision{}, err
		}
	}

	var matched []MatchedRule
	var hardBreachAction RuleAction
	hardBreached := false
	var configBreachAction RuleAction
	configBreached := false
	riskSignalBreached := false

	// Group configurable candidates by (LimitKind, TimeWindow) so
	// precedence/conflict resolution happens independently per limit
	// shape - a player-specific max_amount override must not suppress a
	// completely separate cumulative_amount rule.
	type configKey struct {
		kind   LimitKind
		window TimeWindow
	}
	configCandidates := map[configKey][]Rule{}

	for _, r := range rules {
		if !r.isEffective(now) || !r.matches(req) {
			continue
		}
		switch r.RuleKind {
		case RuleHardLimit:
			breached, err := r.breach(ctx, tx, req, exponents)
			if err != nil {
				return RiskDecision{}, err
			}
			matched = append(matched, MatchedRule{RuleID: r.ID, RuleKind: r.RuleKind, Action: r.Action, Breached: breached})
			if breached {
				hardBreached = true
				if hardBreachAction == "" || r.Action == ActionDeny {
					hardBreachAction = r.Action
				}
			}
		case RuleConfigurableLimit:
			configCandidates[configKey{r.LimitKind, r.TimeWindow}] = append(configCandidates[configKey{r.LimitKind, r.TimeWindow}], r)
		case RuleRiskSignal:
			breached, err := r.breach(ctx, tx, req, exponents)
			if err != nil {
				return RiskDecision{}, err
			}
			matched = append(matched, MatchedRule{RuleID: r.ID, RuleKind: r.RuleKind, Action: r.Action, Breached: breached})
			if breached {
				riskSignalBreached = true
			}
		default:
			return RiskDecision{}, fmt.Errorf("risk: unrecognized rule_kind %q on rule %s", r.RuleKind, r.ID)
		}
	}

	// Resolve exactly one CONFIGURABLE rule per (limit_kind, time_window):
	// the single highest-specificity match. Two candidates tied at the
	// top specificity is a genuine configuration conflict - fail closed,
	// never a coin flip or table-order pick.
	for _, candidates := range configCandidates {
		best := candidates[0]
		tie := false
		for _, c := range candidates[1:] {
			if c.specificity() > best.specificity() {
				best, tie = c, false
			} else if c.specificity() == best.specificity() {
				tie = true
			}
		}
		if tie {
			return RiskDecision{}, fmt.Errorf("%w: operation=%s limit_kind=%s time_window=%s", ErrConflictingRules, req.Operation, best.LimitKind, best.TimeWindow)
		}
		breached, err := best.breach(ctx, tx, req, exponents)
		if err != nil {
			return RiskDecision{}, err
		}
		matched = append(matched, MatchedRule{RuleID: best.ID, RuleKind: best.RuleKind, Action: best.Action, Breached: breached})
		if breached {
			configBreached = true
			// Deny-priority merge across every (limit_kind, time_window)
			// group, order-independent regardless of Go's randomized map
			// iteration order over configCandidates above - mirrors
			// hardBreachAction's own already-correct pattern exactly.
			// Specialist review finding: this previously overwrote
			// unconditionally on every group, so a later REVIEW-action
			// breach could silently DOWNGRADE an earlier DENY-action
			// breach purely depending on map iteration order - the exact
			// non-determinism directive §16 requires never exist.
			if configBreachAction == "" || best.Action == ActionDeny {
				configBreachAction = best.Action
			}
		}
	}

	decision := RiskDecision{Outcome: OutcomeAllow, Code: CodeAllowed, MatchedRules: matched, CorrelationID: req.CorrelationID}

	// Precedence: DENY (hard or configurable) beats REVIEW (from either
	// a review-action breach or any risk signal) beats ALLOW - directive
	// §16's "the final decision must be deterministic," applied as one
	// fixed, documented priority order rather than left implicit.
	switch {
	case hardBreached && hardBreachAction == ActionDeny:
		decision.Outcome, decision.Code, decision.Message = OutcomeDeny, CodeHardLimitBreach, "a platform hard limit was breached"
	case configBreached && configBreachAction == ActionDeny:
		decision.Outcome, decision.Code, decision.Message = OutcomeDeny, CodeLimitBreach, "a configured limit was breached"
	case hardBreached:
		decision.Outcome, decision.Code, decision.Message = OutcomeReview, CodeHardLimitBreach, "a platform hard limit flagged this for review"
	case configBreached:
		decision.Outcome, decision.Code, decision.Message = OutcomeReview, CodeLimitBreach, "a configured limit flagged this for review"
	case riskSignalBreached:
		decision.Outcome, decision.Code, decision.Message = OutcomeReview, CodeRiskSignalFlag, "a risk signal was flagged"
	}

	// Self-check on our own output: a decision a caller cannot classify
	// must surface as an error, never as "not ALLOW, so decline".
	if !decision.Outcome.IsKnown() {
		return RiskDecision{}, fmt.Errorf("%w: %q", ErrUnknownOutcome, decision.Outcome)
	}
	return decision, nil
}
