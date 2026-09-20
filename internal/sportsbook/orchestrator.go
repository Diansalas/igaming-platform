package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/money"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
)

// placeholderRoundingRuleID stands in for the immutable, append-only
// rounding_rules reference table that does not exist in this repository
// yet (confirmed by direct search of migrations/ - no CREATE TABLE
// rounding_rules anywhere). Mirrors internal/bonus's identical, already-
// disclosed placeholder (internal/bonus/types.go) verbatim: a stable,
// well-known id standing in for "the platform default round-half-up
// rounding rule" (ADR 0021 DS-1/DS-2), so every bet this package computes
// a potential_return for at least carries a consistent, auditable,
// non-zero rounding_rule_id pending that table's own migration. NAMED
// GAP, not silently invented as if it were a real reference-table row.
var placeholderRoundingRuleID = uuid.MustParse("00000000-0000-4000-a000-000000000002")

// PlaceBetParams is PlaceBet's input. TenantID/BrandID/PlayerAccountID/
// WalletID MUST be resolved server-side from the authenticated player
// session - never from a request body (identical discipline to
// internal/casino.LaunchGameParams). ExpectedOddsNumerator/Denominator
// are the odds the CLIENT currently believes it is betting on (from its
// last catalogue/event-detail read) - PlaceBet rejects with
// RejectionOddsChanged if the selection's live odds no longer match,
// per docs/architecture/09-sportsbook-architecture.md §3.3 step 1's
// "a price that has since moved is rejected... never silently honored at
// a stale price" rule.
type PlaceBetParams struct {
	TenantID                uuid.UUID
	BrandID                 uuid.UUID
	PlayerAccountID         uuid.UUID
	WalletID                uuid.UUID
	SelectionID             uuid.UUID
	AssetCode               string
	StakeAmount             int64
	ExpectedOddsNumerator   int64
	ExpectedOddsDenominator int64
	IdempotencyKey          string
}

// PlaceBetResult is PlaceBet's output. A rejection (Accepted == false) is
// a business decision, reported as a RESULT field rather than a Go error -
// identical convention to internal/casino's LaunchGameResult/
// ReceiveCallbackResult (see LaunchGameResult's own doc comment for why:
// an RG/risk denial's audit record must commit in the SAME transaction as
// the decision, which a Go error would otherwise roll back). Exactly one
// of the four RejectionXxx constants (types.go) is ever set on a
// rejection, never a generic message alone, so the frontend/bet-slip UI
// can render each distinctly.
type PlaceBetResult struct {
	Bet               Bet
	Accepted          bool
	RejectionCategory string
	RejectionCode     string
	RejectionMessage  string
}

// PlaceBet validates the selection, evaluates RG eligibility and Risk
// policy (in that order, mirroring internal/casino's evaluateAndAudit*
// helpers exactly), locks the player's cash balance and posts the
// sportsbook_bet ledger transaction (Dr player_cash / Cr
// player_locked_cash - docs/decisions/0038 §3, cash-funded case only),
// and writes the sportsbook_bets row - all inside tx, the caller's own
// already-open, tenant-scoped transaction (db.Pool.WithTenant).
//
// Unlike internal/casino's Bet flow, this is a SINGLE synchronous
// request/response: no external provider round-trip occurs at placement
// time (the mock provider is same-process catalogue data, not a live
// wallet-callback partner), so there is no launch-token/session/callback
// split to build here - PlaceBet is called directly by the HTTP handler
// and returns a definite outcome in one call, never "pending".
//
// Idempotent on params.IdempotencyKey (checked BEFORE RG/risk evaluation,
// per findBetByIdempotencyKey's own doc comment): a retried call with the
// same (tenant_id, idempotency_key) returns the original bet, never
// re-evaluates policy or posts a second ledger transaction.
//
// Jurisdiction: DELIBERATELY NOT evaluated here. This stage's directive is
// explicit that wiring internal/jurisdiction.DeterminePlayerJurisdiction
// is out of scope - that function's own doc comment states it has zero
// production callers today and any wiring decision needs human answers to
// HDR-J-7/8/9 first (Stage 4I). This is a recorded, deferred item, not a
// silent gap - but NOTE (Stage 6.1 correction of an earlier, inaccurate
// claim in this comment): unlike casino, which DOES apply a per-game
// jurisdiction_blocklist check at launch time via jurisdiction.Resolve +
// evaluateJurisdictionBlocklist (internal/casino/orchestrator.go), a real
// enforcement point that is wired and tested today, sb_selections/
// sb_events carry NO equivalent blocklist column, so sportsbook has no
// symmetric mechanism to arm even if a future decision populated one.
// This is inert today (casino's own blocklists are all empty - no
// HDR-J item has been answered, so no country is actually blocked
// anywhere on the platform), so it is NOT a live fail-open regression,
// but it IS a real architecture-parity gap: sportsbook must gain an
// equivalent per-selection/per-event blocklist mechanism before any
// second jurisdiction or B2B tenant goes live (docs/governance/
// task-registry.md's Stage 6.1 section tracks this explicitly).
//
// Bonus-funded stakes: OUT OF SCOPE. Every stake this function locks is
// assumed 100% player_cash-funded - docs/decisions/0038 §9 confirms
// bonus-funded sportsbook wagering is BLOCKED platform-wide pending the
// player_locked_cash/player_locked_bonus origin-split joint decision
// (already resolved for casino, migration 0048) being extended to
// sportsbook by a joint architect/ledger-finance/bonus-engine/sportsbook
// decision this stage does not make unilaterally.
func PlaceBet(ctx context.Context, tx pgx.Tx, params PlaceBetParams) (PlaceBetResult, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.WalletID == uuid.Nil || params.SelectionID == uuid.Nil {
		return PlaceBetResult{}, fmt.Errorf("%w: place bet requires fully-populated, server-derived identity fields", ErrInvalidInput)
	}
	if params.AssetCode == "" {
		return PlaceBetResult{}, fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if params.StakeAmount <= 0 {
		return PlaceBetResult{}, fmt.Errorf("%w: stake_amount must be positive", ErrInvalidInput)
	}
	if params.ExpectedOddsNumerator <= 0 || params.ExpectedOddsDenominator <= 0 {
		return PlaceBetResult{}, fmt.Errorf("%w: expected odds numerator/denominator must be positive", ErrInvalidInput)
	}
	if params.IdempotencyKey == "" {
		return PlaceBetResult{}, fmt.Errorf("%w: idempotency_key is required", ErrInvalidInput)
	}

	// Idempotency short-circuit, BEFORE any RG/risk/balance evaluation -
	// see findBetByIdempotencyKey's own doc comment for why (mirrors
	// internal/casino.postBet's identical ordering exactly). Scoped by
	// player_account_id, so a key collision can only ever match THIS
	// player's own prior bet. A match with different selection/stake/
	// asset/odds is a genuine key reuse, not a legitimate retry - reported
	// as an error rather than silently returning someone else's-shaped bet.
	if existing, found, err := findBetByIdempotencyKey(ctx, tx, params.TenantID, params.PlayerAccountID, params.IdempotencyKey); err != nil {
		return PlaceBetResult{}, err
	} else if found {
		if existing.SelectionID != params.SelectionID || existing.StakeAmount != params.StakeAmount || existing.AssetCode != params.AssetCode ||
			existing.OddsNumerator != params.ExpectedOddsNumerator || existing.OddsDenominator != params.ExpectedOddsDenominator {
			return PlaceBetResult{}, fmt.Errorf("%w: existing bet %s", ErrBetIdempotencyKeyReused, existing.ID)
		}
		return PlaceBetResult{Bet: existing, Accepted: true}, nil
	}

	// Structural validation (doc 09 §3.3 step 1, narrowed to this stage's
	// scope): the selection must exist, its event must still be open for
	// betting (not finished/cancelled - live is allowed since this stage
	// builds no in-play-specific logic but also does not forbid it), its
	// market must be open (not suspended/closed), and the odds the client
	// currently sees must still match the selection's live odds.
	sel, err := getSelectionWithContext(ctx, tx, params.SelectionID)
	if err != nil {
		return PlaceBetResult{}, err
	}
	if sel.EventStatus == EventFinished || sel.EventStatus == EventCancelled {
		return PlaceBetResult{Accepted: false, RejectionCategory: RejectionEventNotOpen, RejectionCode: string(sel.EventStatus),
			RejectionMessage: "this event is no longer open for betting"}, nil
	}
	if sel.MarketStatus != MarketOpen {
		return PlaceBetResult{Accepted: false, RejectionCategory: RejectionEventNotOpen, RejectionCode: string(sel.MarketStatus),
			RejectionMessage: "this market is not currently open for betting"}, nil
	}
	if sel.Status != SelectionActive {
		return PlaceBetResult{Accepted: false, RejectionCategory: RejectionEventNotOpen, RejectionCode: string(sel.Status),
			RejectionMessage: "this selection is not currently available"}, nil
	}
	if sel.OddsNumerator != params.ExpectedOddsNumerator || sel.OddsDenominator != params.ExpectedOddsDenominator {
		return PlaceBetResult{Accepted: false, RejectionCategory: RejectionOddsChanged,
			RejectionMessage: "the odds for this selection have changed since you last viewed them"}, nil
	}

	// Stage 4D-RG: the single authoritative "may this player gamble right
	// now" policy boundary, consulted before any ledger effect - identical
	// call and audit shape to internal/casino.evaluateAndAuditEligibility.
	rgDecision, err := evaluateAndAuditEligibility(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID, params.WalletID)
	if err != nil {
		return PlaceBetResult{}, err
	}
	if !rgDecision.Allowed {
		return PlaceBetResult{Accepted: false, RejectionCategory: RejectionRGDenied, RejectionCode: rgDecision.Code,
			RejectionMessage: "gambling is currently restricted for this account: " + rgDecision.Code}, nil
	}

	// Stage 4G: the central Risk & Limits boundary, consulted alongside RG
	// - risk.OperationSportsbookBet already exists in internal/risk's own
	// Operation enum (ADR 0031 §25), so no new Risk mechanism is added
	// here. LicensingMode is resolved fresh, mirroring internal/casino's
	// identical per-call resolution (never cached).
	licensingMode, err := resolveLicensingMode(ctx, tx, params.TenantID)
	if err != nil {
		return PlaceBetResult{}, err
	}
	betID := uuid.New()
	riskDecision, err := evaluateAndAuditRisk(ctx, tx, risk.RiskRequest{
		TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID,
		Operation: risk.OperationSportsbookBet, Product: "sportsbook", AssetCode: params.AssetCode,
		LicensingMode: licensingMode, Amount: params.StakeAmount, CorrelationID: betID,
	})
	if err != nil {
		return PlaceBetResult{}, err
	}
	proceed, err := classifyRiskOutcome(riskDecision.Outcome)
	if err != nil {
		return PlaceBetResult{}, err
	}
	if !proceed {
		return PlaceBetResult{Accepted: false, RejectionCategory: RejectionRiskDenied, RejectionCode: riskDecision.Code,
			RejectionMessage: "this bet was declined by platform risk policy: " + riskDecision.Code}, nil
	}

	cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, params.TenantID, &params.WalletID, ledger.AccountPlayerCash, params.AssetCode)
	if err != nil {
		return PlaceBetResult{}, fmt.Errorf("sportsbook: resolve player_cash account: %w", err)
	}
	lockedAccountID, err := ledger.GetOrCreateAccount(ctx, tx, params.TenantID, &params.WalletID, ledger.AccountPlayerLockedCash, params.AssetCode)
	if err != nil {
		return PlaceBetResult{}, fmt.Errorf("sportsbook: resolve player_locked_cash account: %w", err)
	}

	// Invariant #15: lock and check the balance INSIDE this transaction,
	// immediately before posting - identical pattern and rationale to
	// internal/casino.postBet's own lockCashBalance/insufficient-funds
	// check. This is also the row lock the concurrency test relies on to
	// serialize two concurrent placements against a balance that can only
	// cover one.
	debitTotal, creditTotal, err := lockCashBalance(ctx, tx, cashAccountID)
	if err != nil {
		return PlaceBetResult{}, err
	}
	available := creditTotal - debitTotal
	if available < params.StakeAmount {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: params.TenantID, ActorType: audit.ActorSystem, Action: "sportsbook_bet.declined",
			TargetType: "wallet", TargetID: params.WalletID.String(), Outcome: audit.OutcomeFailure,
			Metadata: map[string]any{"selection_id": params.SelectionID.String(), "amount": params.StakeAmount,
				"asset_code": params.AssetCode, "decline_reason": "insufficient_funds"},
		}); err != nil {
			return PlaceBetResult{}, fmt.Errorf("sportsbook: audit bet decline: %w", err)
		}
		return PlaceBetResult{Accepted: false, RejectionCategory: RejectionInsufficientFunds,
			RejectionMessage: "insufficient available balance for this stake"}, nil
	}

	potentialReturn, err := computePotentialReturn(ctx, tx, params.StakeAmount, sel.OddsNumerator, sel.OddsDenominator, params.AssetCode)
	if err != nil {
		return PlaceBetResult{}, err
	}

	// docs/decisions/0038 §3: cash-funded bet placement is exactly two
	// entries (Dr player_cash / Cr player_locked_cash) - no house_gaming
	// leg at placement time, unlike casino's immediate-settlement shape.
	// provider_id/provider_tx_id stay NULL (§14.6's in-house-mode routing
	// rule: this mock provider is same-process, so there is no external
	// provider reference at all) - idempotency routes through
	// ledger.Post's own IdempotencyKey field, which is namespaced
	// (tenant_id, idempotency_key) GLOBALLY across EVERY transaction type
	// sharing this tenant's ledger (ledger.ErrIdempotencyKeyReused's own
	// doc comment: the same key IS allowed to repeat across different
	// transaction types is exactly the failure mode this guards against -
	// it is REJECTED, not silently treated as a different domain's
	// unrelated key). params.IdempotencyKey alone is a raw, player-chosen
	// string with no player_account_id in it, so passing it through
	// unmodified would let one player's key collide with another player's
	// ledger idempotency slot in the same tenant. Every other domain
	// avoids this by deriving its own server-controlled key (see
	// internal/casino/internal/withdrawal/internal/payments' own
	// IdempotencyKey call sites, all of which prefix a server-side
	// identifier); this does the same, prefixed with BOTH this
	// transaction type's own tag and the player's id - the type tag makes
	// the namespace self-evidently sportsbook's own slice of the flat
	// per-tenant key space (Stage 6.1 hardening: a bare
	// "playerID:clientKey" string, while not colliding with any domain's
	// key format today, relied on every other domain's key format
	// happening to differ rather than on an explicit, structural
	// separation) and the player id makes it impossible for two different
	// players' bets to collide on the same client-chosen string (the
	// Stage 6 fix for the P1 an architect review found).
	ledgerIdempotencyKey := string(ledger.TxSportsbookBet) + ":" + params.PlayerAccountID.String() + ":" + params.IdempotencyKey
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: params.TenantID, TransactionType: ledger.TxSportsbookBet,
		IdempotencyKey: ledgerIdempotencyKey, CorrelationID: betID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: cashAccountID, Direction: ledger.Debit, Amount: params.StakeAmount},
			{LedgerAccountID: lockedAccountID, Direction: ledger.Credit, Amount: params.StakeAmount},
		},
	})
	if err != nil {
		return PlaceBetResult{}, fmt.Errorf("sportsbook: post bet: %w", err)
	}

	bet, err := insertBet(ctx, tx, insertBetParams{
		ID: betID, TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID,
		WalletID: params.WalletID, SelectionID: params.SelectionID, AssetCode: params.AssetCode,
		StakeAmount: params.StakeAmount, OddsNumerator: sel.OddsNumerator, OddsDenominator: sel.OddsDenominator,
		PotentialReturn: potentialReturn, IdempotencyKey: params.IdempotencyKey, LedgerTransactionID: postResult.TransactionID,
	})
	if err != nil {
		return PlaceBetResult{}, err
	}
	// Stage 6.1 hardening (ledger-finance review finding): insertBet's own
	// unique-violation conflict path returns a PRE-EXISTING bet row rather
	// than the one this call tried to insert - which is exactly right for
	// the normal case (a genuine retry with the same idempotency key that
	// ALSO derives the same ledgerIdempotencyKey, so ledger.Post itself
	// already returned the SAME transaction via its own AlreadyPosted
	// short-circuit). But the sportsbook-level key and the ledger-level
	// key are two SEPARATE derivations from the same inputs; if they were
	// ever to disagree (e.g. two application versions with different
	// ledgerIdempotencyKey formats running concurrently during a rolling
	// deploy), THIS call's ledger.Post could have posted a genuinely NEW,
	// now-orphaned ledger transaction (locking a second stake) moments
	// before insertBet's conflict path discarded it in favor of another
	// call's bet row. Cross-checking here turns a silent double-lock into
	// a loud, safely-rolled-back error - this transaction (and its own
	// ledger post from earlier in this same tx) is undone, never the
	// winning transaction.
	if bet.LedgerTransactionID != postResult.TransactionID {
		return PlaceBetResult{}, fmt.Errorf(
			"sportsbook: idempotency key resolved to bet %s (ledger transaction %s) but this call posted ledger transaction %s - refusing to leave an orphaned posting",
			bet.ID, bet.LedgerTransactionID, postResult.TransactionID)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
		Action: "sportsbook_bet.placed", TargetType: "sportsbook_bet", TargetID: bet.ID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"brand_id": params.BrandID.String(), "selection_id": params.SelectionID.String(),
			"stake_amount": params.StakeAmount, "asset_code": params.AssetCode,
			"ledger_transaction_id": postResult.TransactionID.String(), "already_posted": postResult.AlreadyPosted,
		},
	}); err != nil {
		return PlaceBetResult{}, fmt.Errorf("sportsbook: audit bet placed: %w", err)
	}

	return PlaceBetResult{Bet: bet, Accepted: true}, nil
}

// computePotentialReturn computes stake * decimal_odds in the asset's
// minor-unit exponent, entirely via *big.Rat/*big.Int arithmetic - never
// float64 (CLAUDE.md). Mirrors internal/bonus's
// computeCappedPercentageReward scaling discipline exactly: convert the
// minor-unit stake down to the asset's major denomination, multiply by
// the odds ratio, then round back up to minor units ONCE, at the final
// boundary (ADR 0021 DS-2), via internal/money.RoundToMinorUnits.
func computePotentialReturn(ctx context.Context, tx pgx.Tx, stakeAmount, oddsNumerator, oddsDenominator int64, assetCode string) (int64, error) {
	asset, err := assetregistry.GetAsset(ctx, tx, assetCode)
	if err != nil {
		return 0, fmt.Errorf("sportsbook: resolve asset decimal exponent: %w", err)
	}
	exponent := asset.DecimalExponent
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(exponent)), nil)
	scaleRat := new(big.Rat).SetInt(scale)

	stakeMajor := new(big.Rat).SetInt64(stakeAmount)
	stakeMajor.Quo(stakeMajor, scaleRat)

	oddsRatio := big.NewRat(oddsNumerator, oddsDenominator)
	exact := new(big.Rat).Mul(stakeMajor, oddsRatio)

	rounded, err := money.RoundToMinorUnits(exact, int32(exponent), placeholderRoundingRuleID)
	if err != nil {
		return 0, fmt.Errorf("sportsbook: compute potential return: %w", err)
	}
	return money.ToInt64(rounded)
}

// evaluateAndAuditEligibility mirrors internal/casino's identical helper
// verbatim (rg.EvaluateEligibility, audit the denial before returning) -
// duplicated rather than imported because internal/casino does not export
// it and this package must not import internal/casino (a product-adapter
// package importing a sibling product-adapter package would be exactly
// the cross-domain coupling this codebase's package-boundary discipline
// forbids - docs/architecture/09-sportsbook-architecture.md §3.1's
// "package/module boundaries, not merely conceptual layers" rule, applied
// here between casino and sportsbook rather than within sportsbook's own
// layers).
func evaluateAndAuditEligibility(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerAccountID, walletID uuid.UUID) (rg.Decision, error) {
	decision, err := rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{
		TenantID: tenantID, BrandID: brandID, PlayerAccountID: playerAccountID, WalletID: walletID,
	})
	if err != nil {
		return rg.Decision{}, fmt.Errorf("sportsbook: evaluate rg eligibility: %w", err)
	}
	if decision.Allowed {
		return decision, nil
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: "sportsbook_bet.denied_by_rg_policy",
		TargetType: "player_account", TargetID: playerAccountID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{"reason_code": decision.Code, "person_id": decision.PersonID.String(), "brand_id": brandID.String()},
	}); err != nil {
		return rg.Decision{}, fmt.Errorf("sportsbook: audit rg denial: %w", err)
	}
	return decision, nil
}

// evaluateAndAuditRisk mirrors internal/casino's identical helper - see
// evaluateAndAuditEligibility's doc comment for why this is duplicated
// rather than shared.
func evaluateAndAuditRisk(ctx context.Context, tx pgx.Tx, req risk.RiskRequest) (risk.RiskDecision, error) {
	decision, err := risk.Evaluate(ctx, tx, req)
	if err != nil {
		return risk.RiskDecision{}, fmt.Errorf("sportsbook: evaluate risk policy: %w", err)
	}
	if decision.Outcome == risk.OutcomeAllow {
		return decision, nil
	}
	metadata := map[string]any{
		"reason_code": decision.Code, "outcome": string(decision.Outcome), "operation": string(req.Operation),
		"brand_id": req.BrandID.String(), "asset_code": req.AssetCode,
	}
	if req.Amount != 0 {
		metadata["amount"] = req.Amount
	}
	if req.LicensingMode != "" {
		metadata["licensing_mode"] = req.LicensingMode
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: req.TenantID, ActorType: audit.ActorSystem, Action: "sportsbook_bet.denied_by_risk_policy",
		TargetType: "player_account", TargetID: req.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
		Metadata: metadata,
	}); err != nil {
		return risk.RiskDecision{}, fmt.Errorf("sportsbook: audit risk denial: %w", err)
	}
	return decision, nil
}

// classifyRiskOutcome mirrors internal/casino's identical function - see
// that function's own doc comment for the full ALLOW/DENY/REVIEW/
// error-or-unavailable rationale (ADR 0031 §34).
func classifyRiskOutcome(o risk.Outcome) (proceed bool, err error) {
	switch o {
	case risk.OutcomeAllow:
		return true, nil
	case risk.OutcomeDeny, risk.OutcomeReview:
		return false, nil
	default:
		return false, fmt.Errorf("sportsbook: %w: %q", errUnrecognizedRiskOutcome, o)
	}
}

var errUnrecognizedRiskOutcome = errors.New("unrecognized risk outcome")

// resolveLicensingMode mirrors internal/casino's identical helper.
func resolveLicensingMode(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (string, error) {
	t, err := identity.GetTenantByID(ctx, tx, tenantID)
	if err != nil {
		return "", fmt.Errorf("sportsbook: resolve tenant licensing mode: %w", err)
	}
	return t.LicensingModel, nil
}

// lockCashBalance mirrors internal/casino's identical helper (invariant
// #15: the balance read and the prospective debit happen inside the same
// transaction, via a row lock on wallet_balance_projection).
func lockCashBalance(ctx context.Context, tx pgx.Tx, ledgerAccountID uuid.UUID) (debitTotal, creditTotal int64, err error) {
	err = tx.QueryRow(ctx,
		`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
		ledgerAccountID,
	).Scan(&debitTotal, &creditTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("sportsbook: lock cash balance: %w", err)
	}
	return debitTotal, creditTotal, nil
}
