package risk

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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

// ErrMissingLicensingMode is returned when at least one rule configured
// for this operation is scoped by LicensingMode but the request supplied
// none (Stage 4G-FINAL Part D, specialist review finding). Unlike every
// other optional scope dimension, an empty RiskRequest.LicensingMode is
// NEVER a legitimate "not yet resolved" value the way JurisdictionCode's
// empty value can be - tenants.licensing_model is NOT NULL, so a real
// caller always has a real value to supply. Rule.matches() alone would
// silently exclude a LicensingMode-scoped rule from ever matching an
// empty-LicensingMode request (empty on the REQUEST side is not a
// wildcard - only empty on the RULE side is), which would let a caller
// that simply forgot to populate the field silently bypass a
// LicensingMode-scoped HARD_LIMIT meant to protect exactly this
// operation. Checked once per Evaluate call, independently of whether
// any individual rule would otherwise match on every other dimension -
// this is DELIBERATELY operation-wide, not per-rule: a narrower check
// (only requiring LicensingMode when a rule ALSO matches every other
// dimension) would itself be a fail-OPEN gap, since determining that
// requires evaluating the same matches() logic this check exists to
// backstop. Adversarial review considered this "blast radius" (one
// platform-wide LicensingMode-scoped rule requires EVERY tenant to
// supply LicensingMode for that operation) and confirmed it is the
// correct, intended behavior, not a defect: a platform-wide rule is
// deliberately visible/binding to every tenant (that is what
// "platform-wide" means), so every caller for that operation genuinely
// must resolve its own licensing mode once such a rule exists - exactly
// the fail-closed contract this whole gate exists to enforce, applied at
// the same scope the rule itself operates at.
//
// as conservative as ErrMissingAmount's own "never silently skip a rule
// this request cannot be safely evaluated against" principle.
var ErrMissingLicensingMode = fmt.Errorf("%w: a licensing_mode-scoped rule is configured for this operation but the request supplied no licensing_mode", ErrInvalidInput)

// operationLedgerTransactionTypes maps a risk Operation to the
// ledger_transactions.transaction_type value(s) a LimitCumulativeAmount
// rule for that operation aggregates over. Only operations this stage
// actually enforces (see ADR 0031 §7) have an entry - a cumulative rule
// configured for an operation with no entry here is a configuration the
// evaluator cannot compute, surfaced as ErrUnsupportedCumulativeOperation
// (fail-closed, never silently treated as "no usage yet").
var operationLedgerTransactionTypes = map[Operation]string{
	OperationCasinoBet: "casino_bet",
}

// operationLedgerRollbackTypes maps an operation's own ledger transaction
// type to its reversal type - a rolled-back operation must not
// permanently consume the player's cumulative capacity (financial
// correctness specialist review finding). Empty string means no reversal
// type exists for that operation.
var operationLedgerRollbackTypes = map[string]string{
	"casino_bet": "casino_rollback",
}

// cumulativeTransactionTypes returns the full set of ledger transaction
// types a cumulative-amount check must net together for txType: the
// operation's own type plus its rollback counterpart, if any.
func cumulativeTransactionTypes(txType string) []string {
	if rollback, ok := operationLedgerRollbackTypes[txType]; ok {
		return []string{txType, rollback}
	}
	return []string{txType}
}

// numericToBigInt converts a scanned NUMERIC(38,0) column to *big.Int,
// never through int64 or float64 - a SUM of many ledger entries for an
// 18-exponent asset can legitimately exceed int64's range (CLAUDE.md's
// own crypto-precision rule), and a silent overflow here would fail OPEN
// exactly where a cumulative limit most needs to fail closed.
func numericToBigInt(n pgtype.Numeric) (*big.Int, error) {
	if !n.Valid {
		return big.NewInt(0), nil
	}
	if n.Exp != 0 {
		return nil, fmt.Errorf("risk: unexpected non-integer NUMERIC exponent %d", n.Exp)
	}
	if n.Int == nil {
		return big.NewInt(0), nil
	}
	return n.Int, nil
}

// ErrUnsupportedCumulativeOperation is returned when a LimitCumulativeAmount
// rule matches a request whose Operation has no known ledger mapping.
var ErrUnsupportedCumulativeOperation = errors.New("risk: cumulative_amount is not supported for this operation")

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

// breach reports whether req breaches r, querying cumulative usage from
// the ledger when r.LimitKind is LimitCumulativeAmount. tx must already
// be tenant-scoped.
func (r Rule) breach(ctx context.Context, tx pgx.Tx, req RiskRequest) (bool, error) {
	switch r.LimitKind {
	case LimitMinAmount:
		if req.Amount == 0 {
			return false, ErrMissingAmount
		}
		return req.Amount < r.Threshold, nil
	case LimitMaxAmount:
		if req.Amount == 0 {
			return false, ErrMissingAmount
		}
		return req.Amount > r.Threshold, nil
	case LimitCumulativeAmount:
		if req.Amount == 0 {
			return false, ErrMissingAmount
		}
		txType, ok := operationLedgerTransactionTypes[req.Operation]
		if !ok {
			return false, fmt.Errorf("%w: operation %q", ErrUnsupportedCumulativeOperation, req.Operation)
		}
		if req.PlayerAccountID == uuid.Nil || req.AssetCode == "" {
			return false, fmt.Errorf("%w: player_account_id and asset_code are required for a cumulative_amount rule", ErrInvalidInput)
		}
		dur, err := windowDuration(r.TimeWindow)
		if err != nil {
			return false, err
		}

		// Serializes concurrent evaluations of the SAME (tenant, player,
		// operation, limit_kind, asset) cumulative rule so two racing
		// requests can never both read a stale "under threshold" snapshot
		// and both proceed - identical pattern to rg.lockPerson/
		// internal/casino.lockCashBalance's own transaction-scoped
		// advisory/row locking, released automatically at this
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
		// Net of debits minus credits across BOTH the operation's own
		// transaction type and its rollback counterpart (financial
		// correctness specialist review finding): a rolled-back bet's
		// original debit must not permanently consume the player's
		// cumulative capacity - postRollback posts an inverted (credit)
		// entry against the same player_cash account, so summing
		// (debit - credit) across both types nets a voided round to zero.
		// Scanned as NUMERIC via pgtype.Numeric, never int64 - the SUM of
		// many NUMERIC(38,0) entries can legitimately exceed int64's range
		// for an 18-exponent asset (CLAUDE.md's own crypto-precision
		// rule), and a naive int64 SUM/addition would silently wrap and
		// fail OPEN exactly where this rule most needs to fail closed
		// (financial + security specialist review finding).
		var existingNumeric pgtype.Numeric
		err = tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(CASE WHEN le.direction = 'debit' THEN le.amount ELSE -le.amount END), 0)
			 FROM ledger_entries le
			 JOIN ledger_transactions lt ON lt.id = le.ledger_transaction_id
			 WHERE le.tenant_id = $1
			   AND le.player_account_id = $2
			   AND le.asset_code = $3
			   AND lt.transaction_type = ANY($4)
			   AND le.created_at >= $5`,
			req.TenantID, req.PlayerAccountID, req.AssetCode, cumulativeTransactionTypes(txType), windowStart,
		).Scan(&existingNumeric)
		if err != nil {
			return false, fmt.Errorf("risk: query cumulative usage: %w", err)
		}
		existing, err := numericToBigInt(existingNumeric)
		if err != nil {
			return false, fmt.Errorf("risk: convert cumulative usage: %w", err)
		}
		total := new(big.Int).Add(existing, big.NewInt(req.Amount))
		return total.Cmp(big.NewInt(r.Threshold)) > 0, nil
	default:
		// Unreachable given migration 0041's CHECK constraint, but the
		// evaluator must never silently ALLOW a limit_kind it doesn't
		// recognize (fail-closed for a malformed rule - directive §22).
		return false, fmt.Errorf("risk: unrecognized limit_kind %q", r.LimitKind)
	}
}

// Evaluate is the single authoritative Risk & Limits decision boundary
// (ADR 0031 §2) - every caller (today: internal/casino) consults this
// instead of implementing its own limit comparison. tx must already be
// tenant-scoped (db.WithTenant) for req.TenantID; the RLS policy on
// risk_rules (migration 0041) is what actually restricts the SELECT
// below to req's own tenant's rules plus every platform-wide rule.
//
// FAIL-CLOSED CONTRACT: any non-nil error MUST be treated by the caller
// exactly like a DENY - never as ALLOW. This includes a database error,
// an unrecognized limit_kind, a conflicting-rule configuration
// (ErrConflictingRules), and a rule requiring data the request didn't
// supply (ErrMissingAmount, ErrUnsupportedCumulativeOperation). Evaluate
// itself never returns a "soft" error meant to be interpreted as ALLOW -
// there is no such thing in this API.
func Evaluate(ctx context.Context, tx pgx.Tx, req RiskRequest) (RiskDecision, error) {
	if req.TenantID == uuid.Nil || req.BrandID == uuid.Nil || req.Operation == "" {
		return RiskDecision{}, fmt.Errorf("%w: tenant_id, brand_id, and operation are required", ErrInvalidInput)
	}

	rules, err := listEffectiveRules(ctx, tx, req.Operation)
	if err != nil {
		return RiskDecision{}, err
	}

	// ErrMissingLicensingMode fail-closed gate (Stage 4G-FINAL, specialist
	// review finding): checked against every rule CONFIGURED for this
	// operation, not just ones that would otherwise match on every other
	// dimension - conservative by design, mirroring ErrMissingAmount's own
	// "never silently skip a rule this request cannot be safely evaluated
	// against" principle. Rule.matches() alone cannot catch this: an empty
	// req.LicensingMode against a LicensingMode-scoped rule simply returns
	// false (a non-match), which would silently exclude that rule from
	// ever applying rather than erroring - exactly the fail-open gap a
	// forgetful future caller (Evaluate has only one caller, internal/
	// casino, today) could otherwise introduce. Only a currently
	// EFFECTIVE rule (isEffective(now), i.e. active and within its
	// effective window) triggers this gate - a disabled or not-yet-
	// effective rule is not a live policy and must never force every
	// unrelated request for this operation to supply LicensingMode.
	now := time.Now().UTC()
	if req.LicensingMode == "" {
		for _, r := range rules {
			if r.LicensingMode != "" && r.isEffective(now) {
				return RiskDecision{}, ErrMissingLicensingMode
			}
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
			breached, err := r.breach(ctx, tx, req)
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
			breached, err := r.breach(ctx, tx, req)
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
		breached, err := best.breach(ctx, tx, req)
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

	return decision, nil
}
