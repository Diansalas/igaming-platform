// Bonus Conversion (docs/architecture/10-bonus-engine-architecture.md
// "doc 10" §W2.9, T.12, N1.4 Path A): atomic, idempotent, ledger-backed
// (via ledger.Post, which auto-invokes Rule B2), Risk/RG/AssetAuthorization-
// gated, actor/subject-protected, tenant/player-scoped, auditable.
//
// NAMED DEPENDENCY GAP, per the human directive's explicit instruction to
// check internal/risk/types.go's Operation enum before wiring the
// conversion checkpoint: risk.Operation currently has NO
// "bonus_conversion" value (confirmed by direct read of
// internal/risk/types.go - only OperationBonusGrant exists; ADR 0031
// §15a-ii/§16 name bonus_conversion's exact shape but "zero of the six
// required extension-process steps are complete", doc 10 Genuine Gap 4,
// re-confirmed unchanged by this dispatch). This function calls
// risk.Evaluate with Operation("bonus_conversion") anyway, exactly as the
// human directive instructs ("fail closed and document the exact
// dependency gap rather than skipping the check") - risk.Evaluate itself
// already fails closed on an unrecognized Operation
// (ErrUnknownOperation, internal/risk/types.go's knownOperations map),
// and doc 10 §5/T.12's own frozen rule is that ANY Risk error at
// conversion leaves the Grant in `completed` (non-terminal, retryable),
// never forfeits. The practical consequence, stated plainly: NO Grant can
// successfully convert on this platform until `risk` lands the
// bonus_conversion Operation value - conversion is not bypassed, broken,
// or silently disabled; it fails exactly as safely as an intentional
// block would, and will start succeeding the moment that one dependency
// closes, with no code change required here.
package bonus

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/risk"
)

// OperationBonusConversion is the Risk Operation value ADR 0031 §15a-ii/
// §16 specify but has not yet landed in internal/risk/types.go - see this
// file's own doc comment. Declared here, NOT in internal/risk (this
// package must never invent or pre-empt another domain's enum), purely
// so every call site in this file names the same string literal instead
// of repeating a magic value.
const OperationBonusConversion risk.Operation = "bonus_conversion"

// ConvertGrantParams is completed->converted's own input (doc 10 T.12/
// N1.4 Path A).
type ConvertGrantParams struct {
	JurisdictionCode string
	ActorType        ActorType
	ActorID          uuid.UUID
	// MaxCashoutAmount is the Offer's own payout-axis ceiling (nil = no
	// cap). The decided amount is min(remaining bonus balance,
	// MaxCashoutAmount).
	MaxCashoutAmount *big.Int
}

// ConvertGrantResult reports what happened, distinguishing "converted"
// from every non-terminal block per doc 10 §5/T.12's own frozen rule.
type ConvertGrantResult struct {
	Grant         Grant
	Converted     bool
	BlockedReason string // "open_exposure_outstanding" (N1.4 Path A) | "asset_authorization" | "rg" | "risk" - empty iff Converted
	BlockedCode   string
}

// ConvertGrant performs doc 10's "completed -> converted" transition.
// Order, per T.12/N1.4 Path A, both already-frozen rules composed
// together (neither this function nor any Human Decision Register item
// selects an order between them - AOE is checked first purely because it
// requires no external call, cheapest first, mirroring T.1's own
// cost-ordering justification):
//  1. AOE(G, t) != empty -> block, Grant stays `completed`, reason
//     "open_exposure_outstanding" (N1.4 Path A - identical consequence
//     shape to an RG/Risk/AssetAuthorization denial at this checkpoint).
//  2. P_firm re-derived and re-checked against the wagering target
//     (HR-12: re-checked inside the conversion's own transaction) - a
//     caller that reaches this function with a stale/incorrect
//     wageringTargetScaled gets rejected here, not silently trusted.
//  3. The full T.1 three-way gate, Operation = OperationBonusConversion
//     (see this file's own doc comment for the current, expected-to-fail
//     outcome).
//  4. On allow: the sole bonus_conversion posting (ADR 0032 §3.1: Dr
//     player_bonus · Cr player_cash - Rule B2's own mirror generator adds
//     the promo_liability/bonus_expense legs automatically), attributed,
//     Grant -> converted.
func ConvertGrant(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, wageringTargetScaled *big.Int, p ConvertGrantParams) (ConvertGrantResult, error) {
	if err := AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		return ConvertGrantResult{}, err
	}
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	if g.Status != GrantCompleted {
		return ConvertGrantResult{}, fmt.Errorf("%w: convert requires status completed, got %s", ErrIllegalTransition, g.Status)
	}

	aoe, err := ComputeAOE(ctx, tx, tenantID, grantID)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	if !aoe.IsEmpty() {
		if err := recordConversionBlock(ctx, tx, g, "open_exposure_outstanding", "open_exposure_outstanding"); err != nil {
			return ConvertGrantResult{}, err
		}
		return ConvertGrantResult{Grant: g, Converted: false, BlockedReason: "open_exposure_outstanding", BlockedCode: "open_exposure_outstanding"}, nil
	}

	progress, err := DeriveWageringProgress(ctx, tx, tenantID, grantID)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	if wageringTargetScaled != nil && progress.PFirm.Cmp(wageringTargetScaled) < 0 {
		if err := recordConversionBlock(ctx, tx, g, "wagering_requirement_not_met", "wagering_requirement_not_met"); err != nil {
			return ConvertGrantResult{}, err
		}
		return ConvertGrantResult{Grant: g, Converted: false, BlockedReason: "wagering_requirement_not_met", BlockedCode: "wagering_requirement_not_met"}, nil
	}

	jurisdictionID, err := resolveJurisdictionID(ctx, tx, p.JurisdictionCode)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	licensingMode, err := resolveLicensingMode(ctx, tx, g.TenantID)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	remaining, err := RemainingBonusBalance(ctx, tx, tenantID, grantID)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	decided := new(big.Int).Set(remaining)
	if p.MaxCashoutAmount != nil && decided.Cmp(p.MaxCashoutAmount) > 0 {
		decided = new(big.Int).Set(p.MaxCashoutAmount)
	}
	amountMinor, err := amountToInt64(decided)
	if err != nil {
		return ConvertGrantResult{}, err
	}

	outcome, err := GateCheckpoint(ctx, tx, GateParams{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, WalletID: g.WalletID,
		JurisdictionID: jurisdictionID, JurisdictionCode: p.JurisdictionCode, LicensingMode: licensingMode,
		AssetCode: g.AssetCode, Amount: amountMinor, RiskOperation: OperationBonusConversion,
	})
	if err != nil {
		return ConvertGrantResult{}, err
	}
	if !outcome.Allowed {
		reason := denialReasonCode(outcome)
		if err := recordConversionBlock(ctx, tx, g, outcome.DeniedBy, reason); err != nil {
			return ConvertGrantResult{}, err
		}
		return ConvertGrantResult{Grant: g, Converted: false, BlockedReason: outcome.DeniedBy, BlockedCode: outcome.Code}, nil
	}

	if amountMinor == 0 {
		// Nothing left to convert (e.g. already fully written down by an
		// interleaved adjustment) - still a legitimate terminal
		// conversion of zero value, per ADR 0032/§7.6's "one atomic,
		// all-or-nothing posting per Grant" (a zero-amount posting is not
		// meaningful to the ledger - CHECK(amount > 0) - so this
		// transitions status without a ledger posting at all, the
		// identical "issued has no ledger effect" shape §3.1's own table
		// already uses for a decision-only transition).
		now := time.Now().UTC()
		converted, err := UpdateGrantStatus(ctx, tx, tenantID, grantID, GrantCompleted, GrantConverted, now)
		if err != nil {
			return ConvertGrantResult{}, err
		}
		before, after := string(GrantCompleted), string(GrantConverted)
		if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
			TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: g.ID,
			TransitionType: TransitionConverted, TriggerType: conversionTriggerType(p.ActorType),
			BeforeStatus: &before, AfterStatus: &after, ActorType: p.ActorType, ActorID: nonNilActorID(p.ActorType, p.ActorID),
			Amount: big.NewInt(0), AssetCode: &g.AssetCode,
		}); err != nil {
			return ConvertGrantResult{}, err
		}
		return ConvertGrantResult{Grant: converted, Converted: true}, nil
	}

	fundingKind, providerID, err := parseFundingSource(g.FundingSource)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	walletID := g.WalletID
	playerBonusAccount, err := ledger.GetOrCreateAccount(ctx, tx, g.TenantID, &walletID, ledger.AccountPlayerBonus, g.AssetCode)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	playerCashAccount, err := ledger.GetOrCreateAccount(ctx, tx, g.TenantID, &walletID, ledger.AccountPlayerCash, g.AssetCode)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	idempotencyKey := fmt.Sprintf("bonus_conversion:%s", g.ID)
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: g.TenantID, TransactionType: ledger.TxBonusConversion, IdempotencyKey: idempotencyKey,
		CorrelationID: g.ID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: playerBonusAccount, Direction: ledger.Debit, Amount: amountMinor},
			{LedgerAccountID: playerCashAccount, Direction: ledger.Credit, Amount: amountMinor},
		},
		BonusCost: &ledger.BonusCostAttribution{Funding: fundingKind, ProviderID: providerID},
	})
	if err != nil {
		return ConvertGrantResult{}, err
	}
	if err := AttributeGrantLedgerTransactionIdempotent(ctx, tx, g.TenantID, g.ID, postResult.TransactionID, string(ledger.TxBonusConversion)); err != nil {
		return ConvertGrantResult{}, err
	}

	now := time.Now().UTC()
	converted, err := UpdateGrantStatus(ctx, tx, tenantID, grantID, GrantCompleted, GrantConverted, now)
	if err != nil {
		return ConvertGrantResult{}, err
	}
	before, after := string(GrantCompleted), string(GrantConverted)
	ledgerTxID := postResult.TransactionID
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: g.ID,
		TransitionType: TransitionConverted, TriggerType: conversionTriggerType(p.ActorType),
		BeforeStatus: &before, AfterStatus: &after, ActorType: p.ActorType, ActorID: nonNilActorID(p.ActorType, p.ActorID),
		Amount: decided, AssetCode: &g.AssetCode, LedgerTransactionID: &ledgerTxID,
	}); err != nil {
		return ConvertGrantResult{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: g.TenantID, ActorType: audit.ActorType(p.ActorType), ActorID: p.ActorID,
		Action: "bonus_grant.converted", TargetType: "bonus_grant", TargetID: g.ID.String(), Outcome: audit.OutcomeSuccess,
	}); err != nil {
		return ConvertGrantResult{}, err
	}
	return ConvertGrantResult{Grant: converted, Converted: true}, nil
}

func conversionTriggerType(actor ActorType) TriggerType {
	if actor == ActorStaff {
		return TriggerStaffAction
	}
	return TriggerAutomatedRuleEvaluation
}

func recordConversionBlock(ctx context.Context, tx pgx.Tx, g Grant, deniedBy, reason string) error {
	_, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: g.ID,
		TransitionType: TransitionConversionBlocked, TriggerType: TriggerAutomatedRuleEvaluation,
		ActorType: ActorSystem, ReasonCode: &reason,
		RiskDecisionCode:             strPtrIf(deniedBy == "risk", reason),
		RGDecisionCode:               strPtrIf(deniedBy == "rg", reason),
		AssetAuthorizationReasonCode: strPtrIf(deniedBy == "asset_authorization", reason),
	})
	return err
}

func strPtrIf(cond bool, s string) *string {
	if !cond {
		return nil
	}
	return &s
}
