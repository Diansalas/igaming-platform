// Stage 4H-B1 Wave 3 Phase 3 (bonus-engine), closing ledger-accounting-
// model.md §7.18.3's own named gaps in full: the per-game-category
// contribution-weight resolver (§7.18.3.3, "bonus-engine's own design
// work, not specified [t]here"), the fail-closed multi-Grant attribution
// posture (§7.18.3.3's own recommendation), and the single entry point
// casino's postBet calls, in the SAME transaction as the bet's own
// player_cash-funded casino_bet posting (HR-10, §7.18.3.3), to both
// record the contribution AND (closing reconnaissance gap-list item 13 -
// "even if issued, [a Generic Wagering Grant's] completion path... has no
// live trigger") drive the Grant toward completion exactly as a real
// wagering-multiplier bonus is supposed to.
package bonus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// ContributionWeightTable is the JSON shape
// OfferVersion.ContributionWeightTable (bonus_offer_versions, migration
// 0055 - stored, confirmed never read by any code path before this
// Wave, reconnaissance gap-list item 15) is unmarshalled into. Every
// weight is basis points (10000 = 100% contribution, 0 = excluded
// entirely). Resolution order, most specific first: an exact
// provider_game_id match, then a game_type match, then the table's own
// "default", then this package's own permissive default (see
// defaultContributionWeightBP's own doc comment).
type ContributionWeightTable struct {
	Default                 *int32           `json:"default,omitempty"`
	ByGameType              map[string]int32 `json:"by_game_type,omitempty"`
	ByProviderGameID        map[string]int32 `json:"by_provider_game_id,omitempty"`
	ExcludedGameTypes       []string         `json:"excluded_game_types,omitempty"`
	ExcludedProviderGameIDs []string         `json:"excluded_provider_game_ids,omitempty"`
}

// defaultContributionWeightBP is the fallback an Offer author who never
// configures this axis at all gets: full (100%) contribution - the
// implicit behavior every wagering-multiplier bonus had before this
// axis existed at all. A misconfigured/unparseable JSON blob is treated
// identically (fail OPEN to full contribution, never a silent zero) -
// this axis exists to let an Offer author narrow which games count
// toward wagering, not to become an accidental denial-of-completion
// vector if it is ever malformed; T.1's own gates remain the only value-
// authorization boundary this package enforces.
const defaultContributionWeightBP int32 = 10000

// ResolveContributionWeightBP resolves a bet's own contribution weight
// against contributionWeightTable (the raw JSONB bytes read from
// OfferVersion.ContributionWeightTable) for a bet against gameType/
// providerGameID (casino's own catalogue classification - this function
// is pure and does no DB access of its own, mirroring doc 10's own
// "casino's own catalogue lookup" ownership split, ledger-accounting-
// model.md §7.18.6's table).
func ResolveContributionWeightBP(contributionWeightTable []byte, gameType, providerGameID string) int32 {
	if len(contributionWeightTable) == 0 {
		return defaultContributionWeightBP
	}
	var t ContributionWeightTable
	if err := json.Unmarshal(contributionWeightTable, &t); err != nil {
		return defaultContributionWeightBP
	}
	for _, excluded := range t.ExcludedProviderGameIDs {
		if excluded == providerGameID {
			return 0
		}
	}
	for _, excluded := range t.ExcludedGameTypes {
		if excluded == gameType {
			return 0
		}
	}
	if providerGameID != "" {
		if w, ok := t.ByProviderGameID[providerGameID]; ok {
			return w
		}
	}
	if gameType != "" {
		if w, ok := t.ByGameType[gameType]; ok {
			return w
		}
	}
	if t.Default != nil {
		return *t.Default
	}
	return defaultContributionWeightBP
}

// ComputeQualifyingScaled is this Wave's own binding scaling convention
// (bonus-engine's own design work, ledger-accounting-model.md §7.18.3.3 -
// "named as bonus-engine's own design work, not specified here"):
// QualifyingScaled = floor(stakedAmount * contributionWeightBP / 10000),
// in the SAME minor-unit domain as stakedAmount, computed symmetrically
// with WageringTargetScaled below so the two are always directly
// comparable (P_firm, the sum of every contribution's QualifyingScaled,
// against a target expressed in the identical domain). Floor, never
// round-up: the conservative direction (can only require a hair MORE
// qualifying stake to complete, never less), matching every other
// value-reducing-favors-the-platform asymmetry already in this package
// (T.5.1). This is a comparison measure, never a ledger posting -
// money.RoundToMinorUnits' DS-1/DS-2 discipline governs a POSTING
// boundary and does not apply here, deliberately not invoked.
func ComputeQualifyingScaled(stakedAmount *big.Int, contributionWeightBP int32) *big.Int {
	if stakedAmount == nil || stakedAmount.Sign() <= 0 || contributionWeightBP <= 0 {
		return big.NewInt(0)
	}
	scaled := new(big.Int).Mul(stakedAmount, big.NewInt(int64(contributionWeightBP)))
	return scaled.Div(scaled, big.NewInt(10000))
}

// ErrWageringTargetUnmeasurable is returned by WageringTargetScaled when
// the issuing OfferVersion DOES configure a wagering requirement but the
// Grant carries no granted_amount to measure it against - the target is
// then unknown, which is emphatically NOT the same thing as "no target"
// (DR-4HB1W3-RISK-02, Stage 4H-B1 Wave 3 Phase 4, risk).
var ErrWageringTargetUnmeasurable = errors.New("bonus: wagering target is unmeasurable (offer configures a wagering multiplier but the grant has no granted_amount)")

// WageringTargetScaled computes a Grant's own wagering-completion target
// from its issuing OfferVersion's WageringMultiplierBP (basis points -
// e.g. 350000 = a 35x multiplier) and its own immutable granted_amount
// (migration 0067), in the identical minor-unit domain
// ComputeQualifyingScaled produces, so CheckAndCompleteGrant/ConvertGrant
// can compare the two directly. A nil WageringMultiplierBP (no wagering
// axis configured - Cashback's own permanent case, doc 10 §2) returns a
// nil target, matching CheckAndCompleteGrant's own "nil target = already
// satisfied" contract exactly.
//
// DR-4HB1W3-RISK-02: the "offer HAS a wagering multiplier but the Grant
// has no granted_amount" case is a hard, fail-closed ERROR, never a nil
// target. CheckAndCompleteGrant/ConvertGrant both read a nil target as
// "already satisfied" (lifecycle.go/conversion.go), so returning nil for
// an UNMEASURABLE target silently completes a Grant that has done zero
// qualifying wagering - a missing input resolving to ALLOW, which is the
// one outcome a value-authorizing comparison on this platform may never
// produce. granted_amount is nullable by design (migration 0067: "NULL
// until the Grant actually activates"), and was added only in that
// migration, so every Grant activated before it is live in exactly this
// state today.
func WageringTargetScaled(ov OfferVersion, grantedAmount *big.Int) (*big.Int, error) {
	if ov.WageringMultiplierBP == nil || *ov.WageringMultiplierBP <= 0 {
		return nil, nil
	}
	if grantedAmount == nil || grantedAmount.Sign() < 0 {
		return nil, ErrWageringTargetUnmeasurable
	}
	scaled := new(big.Int).Mul(grantedAmount, big.NewInt(int64(*ov.WageringMultiplierBP)))
	return scaled.Div(scaled, big.NewInt(10000)), nil
}

// getGrantedAmount reads bonus_grants.granted_amount (migration 0067) -
// an immutable computation input this package's own Grant struct/
// grantColumns deliberately does not carry (every OTHER existing reader
// of this column, economicop.ConsumeRootBudget, reads it via its own raw
// SQL rather than through this package's Grant type; this function
// mirrors that same narrow-read shape rather than widening grantColumns/
// scanGrant for every existing caller).
func getGrantedAmount(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (*big.Int, error) {
	var amt pgtype.Numeric
	if err := tx.QueryRow(ctx, `SELECT granted_amount FROM bonus_grants WHERE tenant_id = $1 AND id = $2`, tenantID, grantID).Scan(&amt); err != nil {
		return nil, fmt.Errorf("bonus: read granted_amount for grant %s: %w", grantID, err)
	}
	return numericToBigInt(amt)
}

// CashFundedBetContributionParams is casino postBet's own call-through
// input (ledger-accounting-model.md §7.18.3.3), supplied from the SAME
// posting casino just made - never re-derived, never a caller's own
// separately-computed figure (§7.18.3.2's binding "read from the
// funding account's own debit" rule; StakeAmount here IS that debit
// amount, since it is the exact value casino itself just posted as the
// player_cash debit on BetLedgerTransactionID).
type CashFundedBetContributionParams struct {
	TenantID               uuid.UUID
	PlayerAccountID        uuid.UUID
	BetLedgerTransactionID uuid.UUID
	CorrelationID          uuid.UUID
	AssetCode              string
	StakeAmount            int64 // minor units, > 0
	GameType               string
	ProviderGameID         string
}

// RecordCashFundedWageringContribution is the single entry point
// internal/casino's postBet calls, in the SAME transaction as the bet's
// own posting (HR-10), immediately after it and before postBet returns
// (§7.18.3.3). Safe to call unconditionally (it re-checks
// HasActiveWageringGrant itself), though callers are expected to run
// that check first per §7.18.3.3's own text so the cost to the ordinary,
// no-active-bonus case is exactly one indexed EXISTS before this
// function is even entered.
//
// Multi-Grant attribution (§7.18.3.3's own named gap): until a
// qualifying-wager attribution rule across concurrent Grants is
// specified, a player holding MORE than one Grant simultaneously open
// for new stakes is a fail-closed no-op here - no contribution is
// recorded for ANY of them, an audit entry names the ambiguity, and
// every such Grant is left exactly as it was (never silently attributed
// to an arbitrarily-chosen one). This is conservative, not a data-loss
// risk: an Offer that authors more than one concurrently-active
// wagering-type Grant per player is a misconfiguration this function
// refuses to guess through, not a real player experience this platform's
// Offer-authoring surface is expected to produce today.
func RecordCashFundedWageringContribution(ctx context.Context, tx pgx.Tx, p CashFundedBetContributionParams) error {
	if p.StakeAmount <= 0 {
		return nil
	}
	// DR-4HB1W3-RISK-01: an unidentified asset makes every downstream
	// comparison in this function meaningless (see listActiveWageringGrants'
	// own doc comment). Fail closed rather than fall back to "any asset",
	// mirroring risk.ErrMissingAsset's own posture exactly.
	if p.AssetCode == "" {
		return fmt.Errorf("bonus: cash-funded wagering contribution requires an asset_code (bet %s)", p.BetLedgerTransactionID)
	}
	grants, err := listActiveWageringGrants(ctx, tx, p.TenantID, p.PlayerAccountID, p.AssetCode)
	if err != nil {
		return err
	}
	if len(grants) == 0 {
		return nil
	}
	if len(grants) > 1 {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: p.TenantID, ActorType: audit.ActorSystem,
			Action: "bonus_wagering_contribution.ambiguous_multi_grant_skipped", TargetType: "player_account",
			TargetID: p.PlayerAccountID.String(), Outcome: audit.OutcomeFailure,
			Metadata: map[string]any{
				"grant_count": len(grants), "bet_ledger_transaction_id": p.BetLedgerTransactionID.String(),
				"correlation_id": p.CorrelationID.String(),
			},
		})
	}
	grant := grants[0]

	ov, err := GetOfferVersionByID(ctx, tx, grant.OfferVersionID)
	if err != nil {
		return fmt.Errorf("bonus: load offer version for cash-funded wagering contribution: %w", err)
	}

	weightBP := ResolveContributionWeightBP(ov.ContributionWeightTable, p.GameType, p.ProviderGameID)
	if weightBP <= 0 {
		// An excluded game contributes nothing - not an error, and not a
		// reason to skip the completion re-check below either (a Grant
		// already at its target from a PRIOR contribution should still be
		// found completed, even if THIS particular bet excludes).
		return checkWageringCompletion(ctx, tx, p.TenantID, grant)
	}

	staked := big.NewInt(p.StakeAmount)
	qualifying := ComputeQualifyingScaled(staked, weightBP)
	if err := RecordWageringContribution(ctx, tx, p.TenantID, grant.ID, RecordWageringContributionParams{
		OfferVersionID: grant.OfferVersionID, LockLedgerTransactionID: p.BetLedgerTransactionID, CorrelationID: p.CorrelationID,
		AssetCode: p.AssetCode, StakedBonusAmount: staked, ContributionWeightBP: weightBP, QualifyingScaled: qualifying,
	}); err != nil {
		return fmt.Errorf("bonus: record cash-funded wagering contribution: %w", err)
	}

	return checkWageringCompletion(ctx, tx, p.TenantID, grant)
}

// checkWageringCompletion re-derives grant's own wagering target from its
// OfferVersion/granted_amount and calls CheckAndCompleteGrant - closing
// reconnaissance gap-list item 13 ("even if issued, its completion
// path... has no live trigger") for the cash-funded mechanic this Wave
// authorizes. grant is re-loaded fresh (not the caller's own stale copy)
// since RecordWageringContribution above may have transitioned its
// status.
func checkWageringCompletion(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, grant Grant) error {
	ov, err := GetOfferVersionByID(ctx, tx, grant.OfferVersionID)
	if err != nil {
		return fmt.Errorf("bonus: load offer version for wagering completion check: %w", err)
	}
	grantedAmount, err := getGrantedAmount(ctx, tx, tenantID, grant.ID)
	if err != nil {
		return err
	}
	target, err := WageringTargetScaled(ov, grantedAmount)
	if errors.Is(err, ErrWageringTargetUnmeasurable) {
		// DR-4HB1W3-RISK-02, fail-closed WITHOUT collateral damage: the
		// Grant is NOT completed (the only outcome that would have been
		// unsafe), the ambiguity is audited for an operator exactly as the
		// multi-Grant case above is, and the player's own cash bet - which
		// has nothing to do with this data defect - still posts. Rolling
		// the bet back instead would convert a dormant Grant-data problem
		// into a live betting outage for that player. Note the CONVERSION
		// call site must NOT adopt this posture: WageringTargetScaled
		// returns the error precisely so a money-moving caller can refuse
		// outright.
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem,
			Action: "bonus_wagering_contribution.unmeasurable_target_completion_skipped", TargetType: "bonus_grant",
			TargetID: grant.ID.String(), Outcome: audit.OutcomeFailure,
			Metadata: map[string]any{"offer_version_id": grant.OfferVersionID.String(), "reason": err.Error()},
		})
	}
	if err != nil {
		return fmt.Errorf("bonus: wagering completion check for grant %s: %w", grant.ID, err)
	}
	if _, _, err := CheckAndCompleteGrant(ctx, tx, tenantID, grant.ID, target); err != nil {
		return fmt.Errorf("bonus: check wagering completion: %w", err)
	}
	return nil
}
