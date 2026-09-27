// This file implements docs/decisions/0096-kyc-enforcement-boundary.md
// (PRH-I3, KYC-ENFORCE-1): the deterministic "may this money-boundary
// operation proceed given the player's current KYC state" boundary.
//
// EvaluateEnforcement is a pure database read plus in-process comparison.
// It NEVER calls a KYCProvider/vendor - that is a completely separate
// concern (this package's own verification/document orchestration,
// already isolated behind KYCProvider, ADR 0028 §4). It never accepts a
// pre-computed Outcome from a caller (§2.5): the identity fields on
// EnforcementParams MUST be resolved server-side from authenticated/
// tenant context, exactly like rg.EligibilityParams/risk.RiskRequest.
//
// LABEL: IMPLEMENTED for the mechanism described in ADR 0096 §2/§3.2/§3.5
// as wired into internal/withdrawal, internal/casino, internal/sportsbook
// by this same change. The deposit call site (internal/payments.
// InitiateDeposit) is NOT wired by this change - PRH-I1 rewrites that
// function and calls this exported service (task split, task-registry.md
// PRH-I3 row). No production threshold value exists anywhere in this
// file; every numeric value this package ever compares against comes
// from a kyc_enforcement_policies row an operator authors later (ADR
// 0096 §3.7) - zero rows are seeded by migration 0100.
//
// KNOWN GAP (disclosed, not hidden): the edd_amount and registration_tier
// trigger_types are fully modelled in the schema and in the admin write
// path (CreateEnforcementPolicy/ActivateEnforcementPolicy/
// WithdrawEnforcementPolicy below) but EvaluateEnforcement itself does
// not yet CONSULT them - only cumulative_deposit (for EnforcementDeposit)
// and play (for EnforcementCasinoPlay/EnforcementSportsbookPlay) are
// wired into the outcome computation, per the withdrawal structural rule
// (§3.2 point 1) needing no policy lookup at all. Wiring edd_amount and
// registration_tier is deferred, PARTIALLY IMPLEMENTED, pending HD-KYC-2/
// HD-KYC-3's actual content - the mechanism (table, CHECK constraints,
// admin write path) exists so that work is a policy-authoring exercise,
// not a schema/code change, matching ADR 0096's own "mechanism now,
// values later" contract.
package kyc

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// PolicyVersion is EvaluateEnforcement's own compiled-in logic version,
// recorded on every kyc_enforcement_decisions row (mirroring jurisdiction.
// PolicyVersion / risk's own decision-code discipline) so a later change
// to this file's evaluation logic is distinguishable, after the fact,
// from a change to the underlying policy DATA.
const PolicyVersion = "kyc-enforcement-v1"

// EnforcementOperation is the closed set of money-boundary operations
// this ADR's mechanism gates (ADR 0096 §2.2).
type EnforcementOperation string

const (
	EnforcementDeposit          EnforcementOperation = "deposit"
	EnforcementWithdrawalHold   EnforcementOperation = "withdrawal_hold"
	EnforcementWithdrawalPayout EnforcementOperation = "withdrawal_payout"
	EnforcementCasinoPlay       EnforcementOperation = "casino_play"
	EnforcementSportsbookPlay   EnforcementOperation = "sportsbook_play"
)

func validEnforcementOperation(op EnforcementOperation) bool {
	switch op {
	case EnforcementDeposit, EnforcementWithdrawalHold, EnforcementWithdrawalPayout,
		EnforcementCasinoPlay, EnforcementSportsbookPlay:
		return true
	default:
		return false
	}
}

// isWithdrawalOperation reports whether op is one of the two structural,
// always-on withdrawal enforcement points (ADR 0096 §3.2 point 1).
func isWithdrawalOperation(op EnforcementOperation) bool {
	return op == EnforcementWithdrawalHold || op == EnforcementWithdrawalPayout
}

func isPlayOperation(op EnforcementOperation) bool {
	return op == EnforcementCasinoPlay || op == EnforcementSportsbookPlay
}

// EnforcementOutcome is one of exactly five normalized values (ADR 0096
// §2.2) - never a raw kyc_verifications.status string, never a boolean.
type EnforcementOutcome string

const (
	OutcomeNotRequired EnforcementOutcome = "not_required"
	OutcomePassed      EnforcementOutcome = "passed"
	OutcomePending     EnforcementOutcome = "pending"
	OutcomeFailed      EnforcementOutcome = "failed"
	OutcomeUnavailable EnforcementOutcome = "unavailable"
)

// EnforcementParams is EvaluateEnforcement's input. Every identity field
// MUST be resolved server-side from the authenticated/tenant context -
// never accepted from a request body.
//
// DEVIATION FROM THE ADR 0096 §2.2 SKETCH (disclosed): the sketch listed
// LicensingJurisdictionID as a caller-supplied field ("resolved... exactly
// as jurisdiction.ResolveEvaluationPolicy already does internally"). This
// implementation instead resolves it INSIDE EvaluateEnforcement, from
// TenantID, using the identical tenants.licence_id -> licences.
// jurisdiction_id join jurisdiction.ResolveEvaluationPolicy already
// performs - so every call site (deposit, withdrawal, casino, sportsbook)
// shares one resolution instead of four independent copies, and no call
// site can supply a wrong/stale jurisdiction id even by accident. This is
// a strictly narrower trust surface than the sketch, not a weaker one,
// and is recorded in this ADR's implementation record (§ "Implementation
// record") rather than left as a silent divergence.
type EnforcementParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	// PersonID is REQUIRED for every operation, but is NOT the primary
	// read key (corrected per security re-verification N1, 2026-09-27,
	// which found the original draft self-contradictory - see
	// readLatestVerificationByPlayerAccount's own doc comment for the
	// full reasoning). The primary key for every operation, including
	// withdrawal, is the gated account's own latest row for
	// (tenant_id, brand_id, player_account_id), ordered (created_at DESC,
	// id DESC). PersonID is used ONLY as an additional, deny-only
	// cross-account overlay on the withdrawal structural rule
	// (crossAccountRejectedOverlay): a rejection recorded against a
	// DIFFERENT PlayerAccount of the same Person, in the same tenant,
	// denies a withdrawal from THIS account - never the reverse (an
	// approval on one account can never authorize a withdrawal from
	// another). HD-KYC-6 (cross-tenant/cross-brand reuse) is unaffected:
	// the overlay never widens visibility past the tenant boundary.
	PersonID  uuid.UUID
	Operation EnforcementOperation
	AssetCode string
	// Amount is minor units, player-chosen; never trusted as the sole
	// basis of a cumulative comparison (§2.6(e)) - only as the amount of
	// the transaction being gated right now.
	Amount        int64
	CorrelationID uuid.UUID
}

// EnforcementDecision is EvaluateEnforcement's output.
type EnforcementDecision struct {
	Outcome        EnforcementOutcome
	Allowed        bool
	Code           string
	Message        string
	MatchedTrigger string
	PolicyVersion  string
}

// ErrEnforcementUnavailable is returned alongside a non-nil error from
// EvaluateEnforcement itself is never expected on the happy/deny path -
// deny is signalled through EnforcementDecision.Allowed == false with
// Outcome == OutcomeUnavailable, matching ADR 0096 §2.2's contract that
// "any non-nil error is a DENY at the call site". A non-nil error return
// additionally happens only for a structurally invalid call (missing
// required id), caught before any query runs.
var ErrEnforcementUnavailable = errors.New("kyc: enforcement evaluation unavailable")

// EvaluateEnforcement is the single authoritative "may this money-
// boundary operation proceed given the player's current KYC state"
// policy boundary (ADR 0096 §2). It never calls a KYCProvider and takes
// no row lock (plain SELECTs only, §5) - it composes safely anywhere
// RG/Risk already compose today, at the position ADR 0096 §2.4 states.
//
// Any non-nil error return is itself a caller bug (missing required
// input) and must be treated as a DENY - but the ordinary "the evaluator
// itself could not determine an outcome" case (a DB error, a malformed
// policy row) is represented as a NIL error with
// EnforcementDecision{Outcome: OutcomeUnavailable, Allowed: false}, per
// §2.6(c): "no rows found" and "the query failed" are different states,
// and a query failure must never be interpreted as not_required.
func EvaluateEnforcement(ctx context.Context, tx pgx.Tx, params EnforcementParams) (EnforcementDecision, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil {
		return EnforcementDecision{}, fmt.Errorf("kyc: tenant/brand/player account ids are required")
	}
	if !validEnforcementOperation(params.Operation) {
		return EnforcementDecision{}, fmt.Errorf("kyc: unknown enforcement operation %q", params.Operation)
	}
	if params.PersonID == uuid.Nil {
		return EnforcementDecision{}, fmt.Errorf("kyc: person id is required")
	}

	// Withdrawal's structural rule (§3.2 point 1) needs NO policy-table
	// lookup and therefore no jurisdiction resolution at all (§5:
	// "withdrawal_hold/withdrawal_payout need no policy-table lookup and
	// no withdrawal_requests history query") - resolved lazily below,
	// only for the two operations that actually consult a policy row, so
	// a tenant with no licence bound (a perfectly ordinary fixture/test
	// state, or an as-yet-unlicensed tenant) never turns every withdrawal
	// into a spurious `unavailable`.
	switch {
	case isWithdrawalOperation(params.Operation):
		return evaluateWithdrawalStructuralRule(ctx, tx, params)
	case params.Operation == EnforcementDeposit:
		licensingJurisdictionID, hasLicence, err := resolveLicensingJurisdictionID(ctx, tx, params.TenantID)
		if err != nil {
			// A genuine query error is a policy-lookup failure, not "no
			// policy configured" - unavailable, never not_required
			// (§2.6(c)).
			return unavailableDecision("jurisdiction_unresolved"), nil
		}
		if !hasLicence {
			// No licence bound means no jurisdiction-keyed policy could
			// possibly have been activated for this tenant (activation is
			// inherently jurisdiction-keyed) - not_required, not
			// unavailable. Distinct from a genuine query error above.
			return notRequiredDecision(params.Operation, "tenant has no licence bound"), nil
		}
		return evaluateDepositThreshold(ctx, tx, params, licensingJurisdictionID)
	case isPlayOperation(params.Operation):
		licensingJurisdictionID, hasLicence, err := resolveLicensingJurisdictionID(ctx, tx, params.TenantID)
		if err != nil {
			return unavailableDecision("jurisdiction_unresolved"), nil
		}
		if !hasLicence {
			return notRequiredDecision(params.Operation, "tenant has no licence bound"), nil
		}
		return evaluatePlayTrigger(ctx, tx, params, licensingJurisdictionID)
	default:
		return unavailableDecision("unhandled_operation"), nil
	}
}

func unavailableDecision(code string) EnforcementDecision {
	return EnforcementDecision{
		Outcome:       OutcomeUnavailable,
		Allowed:       false,
		Code:          "kyc_unavailable:" + code,
		Message:       "kyc enforcement evaluation could not complete",
		PolicyVersion: PolicyVersion,
	}
}

func notRequiredDecision(op EnforcementOperation, message string) EnforcementDecision {
	return EnforcementDecision{
		Outcome:       OutcomeNotRequired,
		Allowed:       true,
		Code:          "kyc_" + string(op) + ":not_required",
		Message:       message,
		PolicyVersion: PolicyVersion,
	}
}

// resolveLicensingJurisdictionID returns (jurisdictionID, true, nil) when
// the tenant has a bound licence resolving to a jurisdiction; (uuid.Nil,
// false, nil) when the tenant simply has no licence bound (a valid,
// non-error state - a not-yet-licensed tenant, or an ordinary test
// fixture); and a non-nil error only for a genuine query failure. Callers
// MUST treat these three cases differently (§2.6(c)): "no licence" is
// not_required, a query failure is unavailable - never conflated.
func resolveLicensingJurisdictionID(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (uuid.UUID, bool, error) {
	var licenceID *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT licence_id FROM tenants WHERE id = $1`, tenantID).Scan(&licenceID); err != nil {
		return uuid.Nil, false, fmt.Errorf("kyc: read tenant licence: %w", err)
	}
	if licenceID == nil {
		return uuid.Nil, false, nil
	}
	var jurisdictionID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT jurisdiction_id FROM licences WHERE id = $1`, *licenceID).Scan(&jurisdictionID); err != nil {
		return uuid.Nil, false, fmt.Errorf("kyc: read licence jurisdiction: %w", err)
	}
	return jurisdictionID, true, nil
}

// latestVerificationState is the §2.6(a)/(b) read: the single latest row,
// deterministically ordered, with expiry folded into the effective
// status independently of the stored column.
type latestVerificationState struct {
	found  bool
	status VerificationStatus
}

// effectiveOutcome maps a latestVerificationState to one of
// passed/pending/failed, per ADR 0096 §2.3 - never itself returning
// not_required or unavailable (those are the caller's own decision,
// depending on whether a check is required at all, and whether the
// lookup itself failed).
func (s latestVerificationState) effectiveOutcome(expired bool) EnforcementOutcome {
	if !s.found {
		return OutcomeFailed
	}
	if expired {
		return OutcomeFailed
	}
	switch s.status {
	case StatusApproved:
		return OutcomePassed
	case StatusPending, StatusReviewRequired:
		return OutcomePending
	case StatusUnverified, StatusRejected, StatusExpired:
		return OutcomeFailed
	default:
		return OutcomeFailed
	}
}

// readLatestVerificationByPlayerAccount is the PRIMARY read key every
// enforcement point uses (security re-verification N1, 2026-09-27,
// resolving the original draft's self-contradiction the same way §2.6(a)/
// HD-KYC-6 already state it): the gated account's own latest row for
// (tenant_id, brand_id, player_account_id), ordered (created_at DESC, id
// DESC). PersonID is NOT used as the primary key anywhere - it is used
// ONLY as an additional, deny-only cross-account overlay for the
// withdrawal structural rule (crossAccountRejectedOverlay below), so a
// newer approval on one brand/account can never mask a rejection on a
// DIFFERENT brand/account of the same Person, without a stale approval on
// account A ever being able to authorize a withdrawal FROM account B.
func readLatestVerificationByPlayerAccount(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerAccountID uuid.UUID) (latestVerificationState, bool, error) {
	var status string
	var expiresAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT status, expires_at FROM kyc_verifications
		 WHERE tenant_id = $1 AND brand_id = $2 AND player_account_id = $3
		 ORDER BY created_at DESC, id DESC LIMIT 1`,
		tenantID, brandID, playerAccountID,
	).Scan(&status, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return latestVerificationState{}, false, nil
	}
	if err != nil {
		return latestVerificationState{}, false, err
	}
	expired := expiresAt != nil && !expiresAt.After(time.Now())
	return latestVerificationState{found: true, status: VerificationStatus(status)}, expired, nil
}

// crossAccountRejectedOverlay implements security re-verification N1's
// exact prescription: "PersonID is used, if at all, only as an
// additional deny-only check: failed if any other PlayerAccount of the
// same Person in the same tenant has a latest row that is rejected."
// Applied ONLY to the withdrawal structural rule (ADR 0096 §3.2 point 1)
// - never to deposit/play, which stay purely per-PlayerAccount per
// §2.6(a)/HD-KYC-6. A "latest row" here means each OTHER account's own
// most-recent verification, not any historical row of theirs.
func crossAccountRejectedOverlay(ctx context.Context, tx pgx.Tx, tenantID, personID, excludePlayerAccountID uuid.UUID) (bool, error) {
	var rejected bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM kyc_verifications v1
			 WHERE v1.tenant_id = $1 AND v1.person_id = $2 AND v1.player_account_id <> $3
			   AND v1.status = 'rejected'
			   AND v1.id = (
			       SELECT v2.id FROM kyc_verifications v2
			        WHERE v2.tenant_id = v1.tenant_id AND v2.player_account_id = v1.player_account_id
			        ORDER BY v2.created_at DESC, v2.id DESC LIMIT 1
			   )
		)`,
		tenantID, personID, excludePlayerAccountID,
	).Scan(&rejected)
	if err != nil {
		return false, err
	}
	return rejected, nil
}

// evaluateWithdrawalStructuralRule implements ADR 0096 §3.2 point 1: every
// withdrawal request/payout dispatch requires the gated account's CURRENT
// latest verification to be passed and unexpired - full stop, no policy
// row, no "already withdrawn once" exemption, no jurisdiction override -
// PLUS the deny-only cross-account overlay (crossAccountRejectedOverlay)
// so a rejection recorded against a DIFFERENT PlayerAccount of the same
// Person cannot be sidestepped by withdrawing from this one.
func evaluateWithdrawalStructuralRule(ctx context.Context, tx pgx.Tx, params EnforcementParams) (EnforcementDecision, error) {
	state, expired, err := readLatestVerificationByPlayerAccount(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID)
	if err != nil {
		return unavailableDecision("verification_lookup_failed"), nil
	}
	outcome := state.effectiveOutcome(expired)
	if outcome == OutcomePassed {
		crossRejected, err := crossAccountRejectedOverlay(ctx, tx, params.TenantID, params.PersonID, params.PlayerAccountID)
		if err != nil {
			return unavailableDecision("cross_account_overlay_failed"), nil
		}
		if crossRejected {
			outcome = OutcomeFailed
		}
	}
	return EnforcementDecision{
		Outcome:        outcome,
		Allowed:        outcome == OutcomePassed,
		Code:           "kyc_" + string(params.Operation) + ":" + string(outcome),
		Message:        "withdrawal requires a passed, unexpired verification (ADR 0096 §3.2)",
		MatchedTrigger: "structural:withdrawal_requires_passed",
		PolicyVersion:  PolicyVersion,
	}, nil
}

// activePolicyRow is the subset of a kyc_enforcement_policies row
// EvaluateEnforcement needs.
type activePolicyRow struct {
	id                  uuid.UUID
	thresholdMinorUnits *string // NUMERIC read as text to preserve exact precision
	assetCode           *string
}

// readActivePolicies returns EVERY active row for
// (licensingJurisdictionID, triggerType[, playOperation]) - one per
// asset_code, per the asset-aware unique index (security re-verification
// N2, 2026-09-27: kyc_enforcement_policies_one_active now includes
// asset_code, so more than one active row for the same trigger_type can
// legitimately coexist, one per asset).
func readActivePolicies(ctx context.Context, tx pgx.Tx, licensingJurisdictionID uuid.UUID, triggerType string, playOperation *string) ([]activePolicyRow, error) {
	var rows pgx.Rows
	var err error
	if playOperation != nil {
		rows, err = tx.Query(ctx, `SELECT id, threshold_minor_units::text, asset_code FROM kyc_enforcement_policies
		           WHERE licensing_jurisdiction_id = $1 AND trigger_type = $2 AND status = 'active' AND play_operation = $3`,
			licensingJurisdictionID, triggerType, *playOperation)
	} else {
		rows, err = tx.Query(ctx, `SELECT id, threshold_minor_units::text, asset_code FROM kyc_enforcement_policies
		           WHERE licensing_jurisdiction_id = $1 AND trigger_type = $2 AND status = 'active'`,
			licensingJurisdictionID, triggerType)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []activePolicyRow
	for rows.Next() {
		var row activePolicyRow
		if err := rows.Scan(&row.id, &row.thresholdMinorUnits, &row.assetCode); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// readActivePolicy is readActivePolicies' single-row convenience wrapper,
// used only where asset scoping does not apply (the 'play' trigger,
// which carries no asset_code at all).
func readActivePolicy(ctx context.Context, tx pgx.Tx, licensingJurisdictionID uuid.UUID, triggerType string, playOperation *string) (activePolicyRow, bool, error) {
	rows, err := readActivePolicies(ctx, tx, licensingJurisdictionID, triggerType, playOperation)
	if err != nil {
		return activePolicyRow{}, false, err
	}
	if len(rows) == 0 {
		return activePolicyRow{}, false, nil
	}
	return rows[0], true, nil
}

// evaluateDepositThreshold implements ADR 0096 §3.2 point 2's deposit
// leg: dormant (no active cumulative_deposit row for this jurisdiction,
// any asset) resolves to not_required/allow. If at least one active row
// exists for this jurisdiction but NONE matches the requested asset, the
// outcome is `unavailable` (fail closed) - security re-verification N2:
// no FX/cross-asset aggregation exists (that is KYC-FX-AGG-1, a separate,
// registered, human-decision-gated follow-up: it needs a rate source
// this platform does not have), so a jurisdiction that has decided
// cumulative-deposit KYC matters at all must not silently exempt an
// asset it has no configured threshold for. An active row matching the
// requested asset governs the comparison exactly as before.
func evaluateDepositThreshold(ctx context.Context, tx pgx.Tx, params EnforcementParams, licensingJurisdictionID uuid.UUID) (EnforcementDecision, error) {
	policies, err := readActivePolicies(ctx, tx, licensingJurisdictionID, "cumulative_deposit", nil)
	if err != nil {
		return unavailableDecision("policy_lookup_failed"), nil
	}
	if len(policies) == 0 {
		return EnforcementDecision{
			Outcome:       OutcomeNotRequired,
			Allowed:       true,
			Code:          "kyc_deposit:not_required",
			Message:       "no active cumulative_deposit policy for this jurisdiction",
			PolicyVersion: PolicyVersion,
		}, nil
	}
	var policy activePolicyRow
	var matched bool
	for _, p := range policies {
		if p.assetCode != nil && *p.assetCode == params.AssetCode {
			policy = p
			matched = true
			break
		}
	}
	if !matched {
		// N2: at least one active cumulative_deposit policy exists for
		// this jurisdiction, but none for the requested asset - fail
		// closed rather than silently exempting an unconfigured asset.
		return unavailableDecision("asset_not_covered_by_active_policy"), nil
	}

	cumulative, err := sumSettledDeposits(ctx, tx, params.TenantID, params.PlayerAccountID, params.AssetCode)
	if err != nil {
		return unavailableDecision("cumulative_lookup_failed"), nil
	}

	threshold, ok := new(big.Int).SetString(strings.TrimSpace(derefStr(policy.thresholdMinorUnits)), 10)
	if !ok {
		return unavailableDecision("malformed_threshold"), nil
	}
	total := new(big.Int).Add(cumulative, big.NewInt(params.Amount))
	if total.Cmp(threshold) < 0 {
		return EnforcementDecision{
			Outcome:        OutcomeNotRequired,
			Allowed:        true,
			Code:           "kyc_deposit:not_required",
			Message:        "cumulative deposit total below the active threshold",
			MatchedTrigger: policy.id.String(),
			PolicyVersion:  PolicyVersion,
		}, nil
	}

	state, expired, err := readLatestVerificationByPlayerAccount(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID)
	if err != nil {
		return unavailableDecision("verification_lookup_failed"), nil
	}
	outcome := state.effectiveOutcome(expired)
	return EnforcementDecision{
		Outcome:        outcome,
		Allowed:        outcome == OutcomePassed,
		Code:           "kyc_deposit:" + string(outcome),
		Message:        "cumulative deposit threshold reached; verification required",
		MatchedTrigger: policy.id.String(),
		PolicyVersion:  PolicyVersion,
	}, nil
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// sumSettledDeposits sums SETTLED deposit credits (ledger TxDeposit) to
// the player's player_cash account for assetCode - never
// deposit_intents.amount, which includes declined/pending intents (ADR
// 0096 §2.6(e)/ledger-finance C6). KNOWN GAP: in-flight (initiated, not
// yet settled) deposits are NOT included here - the exact in-flight
// handling rule is HD-KYC-1's own content, not decided by this
// mechanism (ADR 0096 §2.6(e)).
func sumSettledDeposits(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, assetCode string) (*big.Int, error) {
	var totalText *string
	err := tx.QueryRow(ctx, `
		SELECT SUM(e.amount)::text
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		  JOIN ledger_accounts a ON a.id = e.ledger_account_id
		  JOIN wallets w ON w.id = a.wallet_id
		 WHERE t.tenant_id = $1
		   AND w.player_account_id = $2
		   AND a.asset_code = $3
		   AND a.account_type = 'player_cash'
		   AND t.transaction_type = 'deposit'
		   AND e.direction = 'credit'`,
		tenantID, playerAccountID, assetCode,
	).Scan(&totalText)
	if err != nil {
		return nil, fmt.Errorf("kyc: sum settled deposits: %w", err)
	}
	if totalText == nil {
		return big.NewInt(0), nil
	}
	total, ok := new(big.Int).SetString(strings.TrimSpace(*totalText), 10)
	if !ok {
		return nil, fmt.Errorf("kyc: malformed settled-deposit sum %q", *totalText)
	}
	return total, nil
}

// evaluatePlayTrigger implements ADR 0096 §3.5: dormant (no active 'play'
// row for this jurisdiction/operation) resolves to not_required/allow. An
// active row requires the player's current verification to be passed,
// with the ordinary §2.3 mapping applied with no exception.
func evaluatePlayTrigger(ctx context.Context, tx pgx.Tx, params EnforcementParams, licensingJurisdictionID uuid.UUID) (EnforcementDecision, error) {
	op := string(params.Operation)
	policy, found, err := readActivePolicy(ctx, tx, licensingJurisdictionID, "play", &op)
	if err != nil {
		return unavailableDecision("policy_lookup_failed"), nil
	}
	if !found {
		return EnforcementDecision{
			Outcome:       OutcomeNotRequired,
			Allowed:       true,
			Code:          "kyc_" + op + ":not_required",
			Message:       "no active play policy for this jurisdiction/operation",
			PolicyVersion: PolicyVersion,
		}, nil
	}

	state, expired, err := readLatestVerificationByPlayerAccount(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID)
	if err != nil {
		return unavailableDecision("verification_lookup_failed"), nil
	}
	outcome := state.effectiveOutcome(expired)
	return EnforcementDecision{
		Outcome:        outcome,
		Allowed:        outcome == OutcomePassed,
		Code:           "kyc_" + op + ":" + string(outcome),
		Message:        "jurisdiction requires a passed verification before play",
		MatchedTrigger: policy.id.String(),
		PolicyVersion:  PolicyVersion,
	}, nil
}

// RecordDecision writes one kyc_enforcement_decisions row plus one
// audit.Record call (ADR 0096 §3.6/§7.6) - PII-free, no document
// content, no raw provider reason. Callers are responsible for §3.6's
// commit discipline (allow: same transaction as the domain effect; deny:
// a transaction containing only this row and no domain effect) - this
// function itself only performs the write, inside whatever transaction
// the caller passes.
func RecordDecision(ctx context.Context, tx pgx.Tx, params EnforcementParams, decision EnforcementDecision) error {
	var matchedTrigger *string
	if decision.MatchedTrigger != "" {
		matchedTrigger = &decision.MatchedTrigger
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO kyc_enforcement_decisions
			(tenant_id, brand_id, player_account_id, operation, outcome, allowed, matched_trigger, policy_version, correlation_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		params.TenantID, params.BrandID, params.PlayerAccountID, string(params.Operation),
		string(decision.Outcome), decision.Allowed, matchedTrigger, decision.PolicyVersion, params.CorrelationID,
	); err != nil {
		return fmt.Errorf("kyc: record enforcement decision: %w", err)
	}

	action := "kyc.enforcement_allowed"
	outcome := audit.OutcomeSuccess
	if !decision.Allowed {
		action = "kyc.enforcement_denied"
		outcome = audit.OutcomeFailure
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID:   params.TenantID,
		ActorType:  audit.ActorSystem,
		Action:     action,
		TargetType: "player_account",
		TargetID:   params.PlayerAccountID.String(),
		Outcome:    outcome,
		Metadata: map[string]any{
			"operation":      string(params.Operation),
			"kyc_outcome":    string(decision.Outcome),
			"policy_version": decision.PolicyVersion,
			"correlation_id": params.CorrelationID.String(),
		},
	})
}
