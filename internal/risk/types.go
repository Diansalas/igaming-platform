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
)

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
	PlayerAccountID  *uuid.UUID
	Product          string
	Operation        Operation
	ProviderID       string
	GameID           *uuid.UUID
	AssetCode        string
	PaymentMethod    string
	LimitKind        LimitKind
	TimeWindow       TimeWindow
	// Threshold is minor units, matching the asset's own registered
	// exponent - never floating point (CLAUDE.md's financial rules).
	Threshold          int64
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
// brand > tenant > jurisdiction - directive §16's own suggested ordering,
// extended to cover EVERY optional scope field. Each bit strictly
// outweighs the sum of every lower bit combined (a plain property of
// binary place value), so a single player-scoped rule always beats any
// combination of lower-ranked dimensions, while a rule that ALSO narrows
// by an additional dimension (e.g. tenant+asset) is correctly scored as
// MORE specific than a rule matching on tenant alone - closing a P1 found
// in specialist review: the previous single-highest-dimension-only scheme
// treated "tenant only" and "tenant+asset" as an ARTIFICIAL TIE, which
// forced ErrConflictingRules (and therefore a fail-closed outage of the
// entire operation for that tenant) for two rules that were never
// actually in conflict.
func (r Rule) specificity() int {
	score := 0
	if r.PlayerAccountID != nil {
		score |= 1 << 8
	}
	if r.GameID != nil {
		score |= 1 << 7
	}
	if r.ProviderID != "" {
		score |= 1 << 6
	}
	if r.AssetCode != "" {
		score |= 1 << 5
	}
	if r.PaymentMethod != "" {
		score |= 1 << 4
	}
	if r.Product != "" {
		score |= 1 << 3
	}
	if r.BrandID != nil {
		score |= 1 << 2
	}
	if r.TenantID != nil {
		score |= 1 << 1
	}
	if r.JurisdictionCode != "" {
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
	Product          string
	Operation        Operation
	ProviderID       string
	GameID           uuid.UUID
	AssetCode        string
	PaymentMethod    string
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
