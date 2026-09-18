// Package risk implements the Stage 4G central Risk & Limits engine: ONE
// reusable rule/policy model with domain-specific dimensions (product,
// provider, game, asset, payment method, operation), consumed by every
// gambling/financial domain through a single Evaluate boundary. It is a
// SEPARATE domain from internal/rg (Responsible Gaming/self-exclusion) -
// rg.EvaluateEligibility remains the sole authority for self-exclusion;
// this package never duplicates it, and a caller composes both (see
// internal/casino's own evaluateAndAuditRisk, called alongside its
// existing evaluateAndAuditEligibility).
//
// See docs/decisions/0031-risk-and-limits-engine.md for the full design:
// the rule model, deterministic precedence (HARD_LIMIT vs
// CONFIGURABLE_LIMIT vs RISK_SIGNAL), fail-closed behavior, and which
// integration points are actually enforced this stage vs. merely
// designed.
package risk

import (
	"time"

	"github.com/google/uuid"
)

// LimitKind is the mathematical shape of a limit - deliberately a small,
// fully-implemented set (ADR 0031 §4): count/velocity/exposure/loss are
// documented, designed future extensions, never accepted by the database
// CHECK constraint (migration 0041) or this evaluator - a rule this
// package cannot evaluate must never be configurable at all, which is
// stronger than defensively skipping it at evaluation time.
type LimitKind string

const (
	LimitMinAmount        LimitKind = "min_amount"
	LimitMaxAmount        LimitKind = "max_amount"
	LimitCumulativeAmount LimitKind = "cumulative_amount"
)

// isAmountShaped reports whether k compares a monetary amount and
// therefore needs BOTH an asset context on the request and a declared
// threshold denomination on the rule (ADR 0031 §35). Every limit kind
// this stage implements is amount-shaped; the predicate exists so a
// future non-monetary kind (count/velocity - §4/§12) is not forced to
// carry a denomination it has no meaning for, and so "which kinds need an
// asset" is stated in exactly one place instead of being re-derived at
// each call site.
func (k LimitKind) isAmountShaped() bool {
	switch k {
	case LimitMinAmount, LimitMaxAmount, LimitCumulativeAmount:
		return true
	default:
		return false
	}
}

// TimeWindow is the aggregation window a limit applies over.
// 'transaction' is the only valid window for LimitMinAmount/LimitMaxAmount
// (the request's own amount, no aggregation). Every other value is a
// rolling window ending now(), UTC - calendar-aligned windows are a
// documented future extension requiring jurisdiction-configured timezone
// semantics (ADR 0031 §4), not implemented this stage.
type TimeWindow string

const (
	WindowTransaction  TimeWindow = "transaction"
	WindowRollingHour  TimeWindow = "rolling_hour"
	WindowRollingDay   TimeWindow = "rolling_day"
	WindowRollingWeek  TimeWindow = "rolling_week"
	WindowRollingMonth TimeWindow = "rolling_month"
)

// RuleKind distinguishes a non-negotiable ceiling from an ordinary
// commercial limit from a signal that merely contributes to review -
// directive's explicit "do not blindly assume most-restrictive-wins" and
// "the final decision must be deterministic" (ADR 0031 §5).
type RuleKind string

const (
	// RuleHardLimit is enforced regardless of specificity - ALL matching
	// hard limits are evaluated, and a breach of ANY of them denies/
	// reviews, never overridden by a more specific configurable rule.
	// Intended for legal/jurisdiction constraints.
	RuleHardLimit RuleKind = "hard_limit"
	// RuleConfigurableLimit is an ordinary, overridable commercial limit -
	// the single MOST SPECIFIC matching rule per (limit_kind, time_window)
	// wins.
	RuleConfigurableLimit RuleKind = "configurable_limit"
	// RuleRiskSignal contributes to a REVIEW outcome but never DENY by
	// itself.
	RuleRiskSignal RuleKind = "risk_signal"
)

// RuleAction is what happens when a rule is breached.
type RuleAction string

const (
	ActionDeny   RuleAction = "deny"
	ActionReview RuleAction = "review"
)

// Operation is the specific action a rule/request governs - always
// required on both a rule and a request, since "no operation" would make
// deterministic matching impossible. Extensible via an additive
// migration as new integration points are wired; only OperationCasinoLaunch
// and OperationCasinoBet are actually ENFORCED by any caller this stage
// (ADR 0031 §7) - the rest are designed, documented integration points.
type Operation string

const (
	OperationCasinoLaunch  Operation = "casino_launch"
	OperationCasinoBet     Operation = "casino_bet"
	OperationDeposit       Operation = "deposit"
	OperationWithdrawal    Operation = "withdrawal"
	OperationSportsbookBet Operation = "sportsbook_bet"
	OperationBonusGrant    Operation = "bonus_grant"
	// OperationBonusConversion is the Bonus Engine's `completed` ->
	// `converted` release of bonus value as withdrawable cash (ADR 0031
	// §15a-ii/§16/§16a/§38(d)/§40 - naming fixed, not re-openable:
	// `bonus_conversion`, superseding doc 10 §4's placeholder
	// `bonus_convert`; `bonus_activate` remains rejected, activation
	// reuses OperationBonusGrant). Landed together with migration 0065's
	// CHECK widening, the HTTP allowlist, the OpenAPI enum, the
	// operationCumulativeSpecs entry, and internal/bonus/conversion.go's
	// already-written call site, per §16's own six-steps-in-one-change
	// discipline.
	OperationBonusConversion Operation = "bonus_conversion"
)

// knownOperations is the closed set of Operation values migration 0041's
// (as widened by migration 0065) own CHECK constraint accepts. Evaluate
// validates req.Operation against it (ErrUnknownOperation) rather than
// querying for rules and finding none: an unknown or misspelled
// operation would otherwise match zero rules and resolve to a silent
// ALLOW - a fail-OPEN triggered by a single typo at a caller (Stage
// 4H-B0-R6 fail-closed audit, ADR 0031 §34).
var knownOperations = map[Operation]struct{}{
	OperationCasinoLaunch:    {},
	OperationCasinoBet:       {},
	OperationDeposit:         {},
	OperationWithdrawal:      {},
	OperationSportsbookBet:   {},
	OperationBonusGrant:      {},
	OperationBonusConversion: {},
}

// IsKnownOperation reports whether o is one of the Operation values this
// package (and migration 0041's CHECK constraint) recognizes. Exported so
// a caller/handler can reject an operation before ever opening a
// transaction, using the same single source of truth Evaluate enforces.
func IsKnownOperation(o Operation) bool {
	_, ok := knownOperations[o]
	return ok
}

// RuleStatus is a rule's own lifecycle state - a rule is disabled, never
// deleted (migration 0041's append-only trigger enforces this at the
// database).
type RuleStatus string

const (
	RuleActive   RuleStatus = "active"
	RuleDisabled RuleStatus = "disabled"
)

// Rule mirrors a risk_rules row. TenantID nil means platform-wide.
// Every other scope field (BrandID/JurisdictionCode/PlayerAccountID/
// Product/ProviderID/GameID/AssetCode/PaymentMethod) nil/empty means
// "applies regardless of that dimension" - a rule matches a request when
// every ONE of its non-nil/non-empty scope fields equals the request's
// corresponding field exactly (see evaluator.go's matches method).
type Rule struct {
	ID               uuid.UUID
	TenantID         *uuid.UUID
	BrandID          *uuid.UUID
	JurisdictionCode string
	// LicensingMode scopes a rule to tenants operating under a specific
	// licensing arrangement - "under_platform_licence" or "own_licence"
	// (Stage 4G-FINAL Part D), mirroring tenants.licensing_model exactly
	// (never a new taxonomy). Empty means "applies regardless of
	// licensing mode." A platform-wide (TenantID nil) HARD_LIMIT
	// expressing the PLATFORM's own licence's legal ceiling MUST set this
	// explicitly to "under_platform_licence" - left empty, it would
	// otherwise also bind a future bring-your-own-licence tenant
	// operating under a different licence's own legal regime, which is
	// exactly the gap this field exists to close. See
	// docs/decisions/0031-risk-and-limits-engine.md §10.
	LicensingMode   string
	PlayerAccountID *uuid.UUID
	Product         string
	Operation       Operation
	ProviderID      string
	GameID          *uuid.UUID
	AssetCode       string
	PaymentMethod   string
	LimitKind       LimitKind
	TimeWindow      TimeWindow
	// Threshold is minor units, denominated per ThresholdExponent/
	// AssetCode below - never floating point (CLAUDE.md's financial
	// rules). Deliberately int64, matching ledger.EntryInput.Amount and
	// RiskRequest.Amount (the values it is compared against) even though
	// the column is NUMERIC(38,0): widening Risk alone would create a
	// second money width inside one comparison. Consequence, disclosed in
	// ADR 0031 §35: for an 18-exponent asset the largest AUTHORABLE
	// threshold is int64's maximum (~9.22 major units), and a larger
	// NUMERIC value written by direct SQL makes the row unscannable,
	// which fails CLOSED as an Evaluate error rather than wrapping.
	Threshold int64
	// ThresholdExponent is the decimal exponent Threshold's minor units
	// are expressed in, for an ASSET-AGNOSTIC amount rule (AssetCode
	// empty). Such a rule binds every asset of that exponent and fails
	// closed (ErrThresholdDenominationMismatch) for an asset of any
	// other exponent - never silently re-denominated (ADR 0031 §35,
	// closing §32(d)'s fail-open).
	//
	// nil for an asset-scoped rule, whose denomination IS its own
	// AssetCode: the exponent is then read from the `assets` registry at
	// evaluation time and never copied onto the rule, so there is exactly
	// one source of exponent truth (CLAUDE.md's "per-currency exponent
	// looked up from the Asset registry").
	//
	// nil for an asset-agnostic amount rule only in a row created BEFORE
	// migration 0046 (whose CHECK is NOT VALID precisely so such rows are
	// grandfathered as data rather than guessed at): Evaluate refuses to
	// evaluate one (ErrMissingThresholdDenomination).
	ThresholdExponent  *int16
	RuleKind           RuleKind
	Action             RuleAction
	Status             RuleStatus
	EffectiveFrom      time.Time
	EffectiveUntil     *time.Time
	Description        string
	CreatedByActorType string
	CreatedByActorID   uuid.UUID
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// ThresholdExponentOf returns a pointer to e, for populating
// CreateRuleParams.ThresholdExponent (an asset-agnostic amount rule's
// required threshold denomination - ADR 0031 §35) without every caller
// declaring its own local variable.
func ThresholdExponentOf(e int16) *int16 { return &e }

// specificity ranks how narrowly Rule r is scoped, used ONLY to resolve
// precedence among RuleConfigurableLimit rules that otherwise match the
// same request for the same (LimitKind, TimeWindow) - the single highest
// SCORE decides; a genuine tie (two rules with the IDENTICAL set of scope
// dimensions present, e.g. both scoped by the same GameID and nothing
// else) is a configuration conflict, never silently broken by table order
// (see evaluator.go's ErrConflictingRules).
//
// Scored as a bitmask, one bit per scope dimension, ordered most to least
// specific: player > game > provider > asset > payment_method > product >
// brand > tenant > jurisdiction > licensing_mode - directive §16's own
// suggested ordering, extended to cover EVERY optional scope field.
// licensing_mode ranks below jurisdiction deliberately (Stage 4G-FINAL
// Part D): it is a broader, binary categorization (platform-operated vs.
// bring-your-own-licence) rather than a specific legal jurisdiction, so a
// rule narrowed by an actual jurisdiction is treated as more specific
// than one narrowed only by licensing mode. Each bit strictly outweighs
// the sum of every lower bit combined (a plain property of binary place
// value), so a single player-scoped rule always beats any combination of
// lower-ranked dimensions, while a rule that ALSO narrows by an
// additional dimension (e.g. tenant+asset) is correctly scored as MORE
// specific than a rule matching on tenant alone - closing a P1 found in
// specialist review: the previous single-highest-dimension-only scheme
// treated "tenant only" and "tenant+asset" as an ARTIFICIAL TIE, which
// forced ErrConflictingRules (and therefore a fail-closed outage of the
// entire operation for that tenant) for two rules that were never
// actually in conflict.
func (r Rule) specificity() int {
	score := 0
	if r.PlayerAccountID != nil {
		score |= 1 << 9
	}
	if r.GameID != nil {
		score |= 1 << 8
	}
	if r.ProviderID != "" {
		score |= 1 << 7
	}
	if r.AssetCode != "" {
		score |= 1 << 6
	}
	if r.PaymentMethod != "" {
		score |= 1 << 5
	}
	if r.Product != "" {
		score |= 1 << 4
	}
	if r.BrandID != nil {
		score |= 1 << 3
	}
	if r.TenantID != nil {
		score |= 1 << 2
	}
	if r.JurisdictionCode != "" {
		score |= 1 << 1
	}
	if r.LicensingMode != "" {
		score |= 1 << 0
	}
	return score
}

// RiskRequest is Evaluate's input. TenantID/BrandID/PlayerAccountID MUST
// be resolved server-side, never client-supplied - identical discipline
// to every other financial/gambling boundary in this codebase
// (rg.EligibilityParams, ledger.TransactionInput). Amount is required
// only for operations a LimitMinAmount/LimitMaxAmount/LimitCumulativeAmount
// rule could apply to (casino_bet today) - zero is a valid "no amount"
// value for an amount-less operation (casino_launch), and Evaluate never
// applies an amount-shaped rule when Amount is zero and no matching rule
// happens to require it, but a configured amount rule for an amount-less
// operation is a configuration error the evaluator surfaces as
// ErrMissingAmount rather than silently comparing against zero.
type RiskRequest struct {
	TenantID         uuid.UUID
	BrandID          uuid.UUID
	PlayerAccountID  uuid.UUID
	JurisdictionCode string
	// LicensingMode is the requesting tenant's OWN licensing_model
	// (tenants.licensing_model - "under_platform_licence" or
	// "own_licence"), resolved server-side by the caller exactly like
	// every other identity field on this struct - risk.Evaluate never
	// looks it up itself. Required for a request to ever match a rule
	// scoped by LicensingMode; left empty only matches rules that leave
	// LicensingMode unscoped too. See Rule.LicensingMode's doc comment.
	LicensingMode string
	Product       string
	Operation     Operation
	ProviderID    string
	GameID        uuid.UUID
	AssetCode     string
	PaymentMethod string
	// Amount is minor units for THIS operation (e.g. the stake being
	// placed) - required whenever a matching rule needs it.
	Amount int64
	// CorrelationID lets a caller tie a RiskDecision back to the specific
	// operation it gated (e.g. the casino bet's own round/provider_tx_id-
	// derived correlation id) - purely a reporting/audit convenience,
	// never used for matching.
	CorrelationID uuid.UUID
}

// Outcome is RiskDecision's own normalized result - never a
// provider-specific or ad-hoc string (mirrors identityresolution.
// ResolutionResult/kyc.ProviderResult's identical normalization
// discipline).
type Outcome string

const (
	OutcomeAllow  Outcome = "allow"
	OutcomeDeny   Outcome = "deny"
	OutcomeReview Outcome = "review"
)

// IsKnown reports whether o is one of the three defined outcomes.
//
// Exists so an enforcement point can keep ALLOW / REVIEW / DENY /
// "error-or-unavailable" as FOUR semantically distinct outcomes (ADR 0031
// §34) instead of collapsing everything that is not ALLOW into a decline:
// an Outcome outside this set is not a business decision at all and must
// be handled like an evaluator error (fail closed as an error, never a
// decline carrying a reason code, and never ALLOW). Evaluate self-checks
// its own output with this too, so a future internal code path cannot
// return an unclassified decision.
func (o Outcome) IsKnown() bool {
	switch o {
	case OutcomeAllow, OutcomeDeny, OutcomeReview:
		return true
	default:
		return false
	}
}

// MatchedRule is one rule that contributed to a RiskDecision - reported
// for explainability (directive §23's "risk decisions must be
// explainable"), never exposing more than the rule's own identity and
// outcome contribution.
type MatchedRule struct {
	RuleID   uuid.UUID
	RuleKind RuleKind
	Action   RuleAction
	Breached bool
}

// RiskDecision is Evaluate's deterministic, auditable output - the ONLY
// shape a caller ever sees, regardless of how many rules were evaluated
// internally. Code is a stable, machine-readable reason, mirroring
// rg.Decision's identical "specific, distinguishable sentinel" contract.
type RiskDecision struct {
	Outcome       Outcome
	Code          string
	Message       string
	MatchedRules  []MatchedRule
	CorrelationID uuid.UUID
}

const (
	CodeAllowed         = "allowed"
	CodeHardLimitBreach = "hard_limit_breached"
	CodeLimitBreach     = "limit_breached"
	CodeRiskSignalFlag  = "risk_signal_flagged"
)
