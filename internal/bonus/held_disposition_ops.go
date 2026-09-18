// The G-2 mechanism's operational half (docs/architecture/10-bonus-
// engine-architecture.md "doc 10" N1.4 step 5b/5c, N1.4.2): the two seams
// casino will call in Phase 7 (ResolveTerminalGrantCredit,
// RecheckGrantExposure - their frozen signatures, per doc 08 §16.9/§16.21
// and doc 10 N1.4.2), and disposition-resolution
// (ACTION_REFORFEIT/ACTION_ROUTE_TO_CASH), gated by SEP-1 (REQ-SEP-
// BONUS-4) and four-eyes at the ratified tenant-configurable threshold
// (doc 34 §3.1).
//
// bonus-engine does NOT select G-2 (CLAUDE.md's constraint, doc 10's own
// "no default/timeout/fallback may choose which disposition action
// applies"): ResolveHeldDispositionAction takes `action` as an explicit,
// human-authorized input, supplied by the staff actor who already filed
// and had approved a bonus_change_requests row naming it - never inferred
// or defaulted here.
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// GrantExposureTriggerKind is doc 10 N1.4.2's own enum, adopted verbatim
// from doc 08 §16.21's three named call sites.
type GrantExposureTriggerKind string

const (
	TriggerCasinoRollback          GrantExposureTriggerKind = "casino_rollback"
	TriggerCasinoSettlementTimeout GrantExposureTriggerKind = "casino_settlement_timeout"
	TriggerHeldDispositionResolved GrantExposureTriggerKind = "held_disposition_resolved"
)

// CreditKind names the inbound event kind ResolveTerminalGrantCredit
// captures (doc 10 N1.4 step 5b.i) - informational only (carried onto the
// Progress entry), never branched on by this function.
type CreditKind string

const CreditKindWin CreditKind = "win"

// ResolveTerminalGrantCredit is doc 10 N1.4 step 5b / doc 08 §16.9's named
// seam, adopted verbatim: casino posts the two-leg hold-capture posting
// ITSELF, via its own ledger.Post call (Dr house_gaming payoutAmount /
// Cr player_bonus_held payoutAmount, plus Dr player_locked_bonus
// releasedLockAmount / Cr player_bonus_held releasedLockAmount if the
// stake was locked) BEFORE calling this seam, yielding
// settlementLedgerTransactionID. This seam runs SECOND, inside the SAME
// transaction, and posts NOTHING of its own - it only writes the
// bonus_held_dispositions row (attributing the already-posted settlement
// transaction to the Grant) and appends a Progress entry, returning the
// row's own id for casino's logging/observability only, never branched
// on.
//
// Capture is UNCONDITIONAL: this seam is called, and creates a
// disposition row, for EVERY terminal-Grant win credit, before any of
// G-2's three eventual answers is known or decided - never conditioned on
// ACTION_HOLD_FOR_REVIEW or any other outcome.
//
// Idempotent, keyed on settlementLedgerTransactionID (never
// correlationID, never grantID - LF-22's fix, doc 10 N1.4.1 item 4): a
// redelivered call for the same settlement transaction returns the
// EXISTING disposition's id, never a second row.
func ResolveTerminalGrantCredit(
	ctx context.Context, tx pgx.Tx, grantID, correlationID uuid.UUID, creditKind CreditKind,
	payoutAmount, releasedLockAmount *big.Int, settlementLedgerTransactionID uuid.UUID,
) (uuid.UUID, error) {
	// tenantID is resolved from the Grant row itself (this seam's own
	// signature, per doc 08 §16.9, does not carry a tenantID parameter -
	// casino's own transaction is already tenant-scoped, and the Grant
	// row itself carries its tenant).
	g, err := GetGrantByID(ctx, tx, grantID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("bonus: resolve terminal grant credit: load grant: %w", err)
	}
	if err := AdvisoryLockGrant(ctx, tx, g.TenantID, grantID); err != nil {
		return uuid.Nil, err
	}

	existing, err := GetHeldDispositionBySettlementTransaction(ctx, tx, g.TenantID, settlementLedgerTransactionID)
	if err == nil {
		return existing.ID, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return uuid.Nil, err
	}

	if releasedLockAmount == nil {
		releasedLockAmount = big.NewInt(0)
	}
	disposition, err := CreateHeldDisposition(ctx, tx, HeldDisposition{
		TenantID: g.TenantID, BrandID: g.BrandID, WalletID: g.WalletID, PlayerAccountID: g.PlayerAccountID,
		AssetCode: g.AssetCode, GrantID: grantID, CorrelationID: correlationID,
		SettlementLedgerTransactionID: settlementLedgerTransactionID,
		PayoutAmount:                  payoutAmount, ReleasedLockAmount: releasedLockAmount,
	})
	if err != nil {
		if db.IsUniqueViolation(err) {
			existing, getErr := GetHeldDispositionBySettlementTransaction(ctx, tx, g.TenantID, settlementLedgerTransactionID)
			if getErr != nil {
				return uuid.Nil, getErr
			}
			return existing.ID, nil
		}
		return uuid.Nil, err
	}

	if err := AttributeGrantLedgerTransactionIdempotent(ctx, tx, g.TenantID, grantID, settlementLedgerTransactionID, "bonus_hold_capture"); err != nil {
		return uuid.Nil, err
	}

	total := new(big.Int).Add(payoutAmount, releasedLockAmount)
	assetCode := g.AssetCode
	ledgerTxID := settlementLedgerTransactionID
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: grantID,
		TransitionType: TransitionHeldDispositionCreated, TriggerType: TriggerProviderCallback,
		ActorType: ActorSystem, Amount: total, AssetCode: &assetCode,
		LedgerTransactionID: &ledgerTxID, CorrelationID: &correlationID,
		Detail: []byte(fmt.Sprintf(`{"credit_kind":%q,"held_disposition_id":%q}`, creditKind, disposition.ID)),
	}); err != nil {
		return uuid.Nil, err
	}

	return disposition.ID, nil
}

// RecheckGrantExposure is doc 10 N1.4.2 / doc 08 §16.21's named seam,
// adopted verbatim: casino calls this, in the SAME transaction as a
// value-REDUCING closing posting (a plain lock rollback, a settlement-
// timeout sweep, or a held-disposition rollback-void), immediately after
// its own live Grant-status read finds pending_settlement. This function
// recomputes AOE(G, .) live - the IDENTICAL three-component sum
// ComputeAOE already implements, never a second copy of it - and, if now
// empty, flips G.status from pending_settlement to its already-recorded
// terminal_resolution, appending a Progress entry. If AOE remains
// nonzero, G stays at pending_settlement and newStatus reports that
// unchanged value. Acquires no lock beyond the (tenant_id, grant_id)
// advisory lock the caller is expected to already hold for its own
// unrelated reason.
func RecheckGrantExposure(ctx context.Context, tx pgx.Tx, tenantID, grantID, triggeringLedgerTransactionID uuid.UUID, triggerKind GrantExposureTriggerKind) (GrantStatus, error) {
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		return "", err
	}
	if g.Status != GrantPendingSettlement {
		// Nothing to recheck - either already fully terminal (doc 10
		// N1.7's residual case: "no status flip occurs - only the
		// bonus_held_dispositions row and a new Progress entry are
		// appended, exactly as §T.7 already specifies") or not yet
		// deferred at all. This function only ever ACTS on
		// pending_settlement; every other status is returned unchanged.
		return g.Status, nil
	}

	aoe, err := ComputeAOE(ctx, tx, tenantID, grantID)
	if err != nil {
		return "", err
	}
	if !aoe.IsEmpty() {
		return g.Status, nil
	}
	if g.TerminalResolution == nil {
		return "", fmt.Errorf("bonus: grant %s is pending_settlement with no recorded terminal_resolution", grantID)
	}

	now := time.Now().UTC()
	finalized, err := FinalizePendingSettlement(ctx, tx, tenantID, grantID, now)
	if err != nil {
		return "", err
	}
	before, after := string(GrantPendingSettlement), string(finalized.Status)
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: grantID,
		TransitionType: TransitionPendingSettlementFinalized, TriggerType: TriggerProviderCallback,
		BeforeStatus: &before, AfterStatus: &after, ActorType: ActorSystem,
		LedgerTransactionID: &triggeringLedgerTransactionID,
		Detail:              []byte(fmt.Sprintf(`{"trigger_kind":%q}`, triggerKind)),
	}); err != nil {
		return "", err
	}
	return finalized.Status, nil
}

// HeldDispositionAction is G-2's three candidate machine actions (doc 10
// §T.7/N1.4 step 5c) - the human decision this platform has NOT selected.
// This type exists so a caller (staff, via the four-eyes-approved
// bonus_change_requests payload) supplies the answer explicitly; nothing
// in this package infers, defaults, or times out to one of these values.
type HeldDispositionAction string

const (
	ActionReforfeit   HeldDispositionAction = "reforfeit"
	ActionRouteToCash HeldDispositionAction = "route_to_cash"
)

// ErrHeldDispositionNotHeld is returned when the targeted disposition is
// not currently 'held' (already resolved, or voided by a rollback).
var ErrHeldDispositionNotHeld = errors.New("bonus: held disposition is not in 'held' status")

// ResolveHeldDispositionActionParams is ResolveHeldDispositionAction's
// input. RequestID must name an ALREADY-FILED, ALREADY-APPROVED
// bonus_change_requests row (operation = held_disposition_resolve) whose
// payload names this exact HeldDispositionID/Action pair - this function
// consumes that approval atomically as part of applying the action; it
// never applies an action that was not itself the subject of a real,
// dual-controlled, SEP-1-checked approval (REQ-SEP-BONUS-4).
type ResolveHeldDispositionActionParams struct {
	HeldDispositionID uuid.UUID
	Action            HeldDispositionAction
	ActorID           uuid.UUID
	ReasonCode        string
	RequestID         uuid.UUID
	RequiredApprovals int32
}

// ResolveHeldDispositionAction applies a G-2 disposition answer, gated by
// REQ-SEP-BONUS-4 (already enforced on the bonus_change_approvals row
// this function's own ConsumeApprovedChangeRequest call requires to
// exist and be approved - migration 0063's SEP-1 trigger already ran at
// approval time) and doc 34 §3.1's bonus_held_disposition_resolution
// four-eyes control (consumed here, atomically, immediately before the
// effecting write - doc 34 §5.3's canonical ordering).
//
// Lock order per HR-25 (ledger-accounting-model.md §7.7.2.9): (1)
// (tenant_id, grant_id) advisory lock; (2) SELECT ... FOR UPDATE on the
// bonus_held_dispositions row itself. Neither action ever acquires a
// player_bonus projection FOR UPDATE (HR-25's own correction: both debit
// player_bonus_held directly, never via player_bonus).
func ResolveHeldDispositionAction(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, p ResolveHeldDispositionActionParams) (HeldDisposition, error) {
	disposition, err := GetHeldDispositionByID(ctx, tx, p.HeldDispositionID)
	if err != nil {
		return HeldDisposition{}, err
	}
	if err := AdvisoryLockGrant(ctx, tx, tenantID, disposition.GrantID); err != nil {
		return HeldDisposition{}, err
	}
	disposition, err = LockHeldDispositionForUpdate(ctx, tx, p.HeldDispositionID)
	if err != nil {
		return HeldDisposition{}, err
	}
	if disposition.Status != HeldDispositionHeld {
		return HeldDisposition{}, ErrHeldDispositionNotHeld
	}

	payloadMatch := []byte(fmt.Sprintf(`{"action":%q}`, p.Action))
	if _, err := ConsumeApprovedChangeRequest(ctx, tx, tenantID, ChangeOpHeldDispositionResolve, p.HeldDispositionID, payloadMatch, p.RequiredApprovals, p.ActorID); err != nil {
		return HeldDisposition{}, err
	}

	g, err := GetGrantByID(ctx, tx, disposition.GrantID)
	if err != nil {
		return HeldDisposition{}, err
	}
	fundingKind, providerID, err := parseFundingSource(g.FundingSource)
	if err != nil {
		return HeldDisposition{}, err
	}
	totalAmount := new(big.Int).Add(disposition.PayoutAmount, disposition.ReleasedLockAmount)
	amountMinor, err := amountToInt64(totalAmount)
	if err != nil {
		return HeldDisposition{}, err
	}
	walletID := g.WalletID
	heldAccount, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, &walletID, ledger.AccountPlayerBonusHeld, g.AssetCode)
	if err != nil {
		return HeldDisposition{}, err
	}

	var newStatus HeldDispositionStatus
	var txType ledger.TransactionType
	var entries []ledger.EntryInput
	var reasonCode *string

	switch p.Action {
	case ActionReforfeit:
		newStatus = HeldDispositionResolvedReforfeit
		txType = ledger.TxBonusForfeiture
		rc := "terminal_grant_reforfeit:" + p.ReasonCode
		reasonCode = &rc
		// N1.4 step 5c: "Dr player_bonus_held payout+released_lock_amount
		// / Cr promo_liability" - the caller supplies ONLY the
		// player_bonus_held debit; Rule B2's own mirror generator
		// (bonus_mirror.go, frozen, never hand-assembled - HR-17) adds
		// the promo_liability credit automatically, producing exactly
		// this two-leg shape (no residual, no recognition leg, since the
		// value never touches player_cash).
		entries = []ledger.EntryInput{{LedgerAccountID: heldAccount, Direction: ledger.Debit, Amount: amountMinor}}
	case ActionRouteToCash:
		newStatus = HeldDispositionResolvedRouteToCash
		txType = ledger.TxBonusConversion
		rc := "terminal_grant_cash_route:" + p.ReasonCode
		reasonCode = &rc
		playerCash, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, &walletID, ledger.AccountPlayerCash, g.AssetCode)
		if err != nil {
			return HeldDisposition{}, err
		}
		// NAMED FINDING (not silently worked around - see this
		// function's own package doc comment and the Phase 3 report):
		// doc 10 N1.4 step 5c's prose describes this posting as
		// "Dr player_bonus_held.../Cr player_cash..." with "no
		// promo_liability/bonus_expense legs at all". That description
		// does not survive contact with the ALREADY-FROZEN, ledger-
		// finance-owned Rule B2 mirror generator (internal/ledger/
		// bonus_mirror.go), which "admits no exception by transaction
		// type" and fires on ANY posting touching a BONUS_SET account -
		// player_bonus_held IS a BONUS_SET member (doc 10 N1.4.1 item 2;
		// ledger-accounting-model.md §7.7.2.3). Supplying these two
		// caller entries and letting Post run unmodified (never hand-
		// assembling a mirror leg - HR-17) produces FOUR entries: this
		// debit/credit pair PLUS an automatic Cr promo_liability and Dr
		// bonus_expense (or provider_payable) recognition leg - which is
		// economically correct (routing held bonus value irrevocably to
		// real spendable cash is a genuine cost, exactly like an ordinary
		// bonus_conversion) and is what CLAUDE.md's own "never hand-
		// construct a mirror" rule requires this implementation to defer
		// to, rather than attempting to force doc 10's simplified 2-leg
		// description through a hand-built posting HR-17 would reject
		// outright. Flagged for the Orchestrator/architect/ledger-finance
		// to reconcile doc 10's prose against Rule B2's own supremacy;
		// not resolved unilaterally here.
		entries = []ledger.EntryInput{
			{LedgerAccountID: heldAccount, Direction: ledger.Debit, Amount: amountMinor},
			{LedgerAccountID: playerCash, Direction: ledger.Credit, Amount: amountMinor},
		}
	default:
		return HeldDisposition{}, fmt.Errorf("bonus: unrecognized held-disposition action %q", p.Action)
	}

	idempotencyKey := fmt.Sprintf("bonus_held_disposition_resolve:%s", disposition.ID)
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: txType, IdempotencyKey: idempotencyKey,
		CorrelationID: disposition.CorrelationID, ReasonCode: reasonCode,
		Entries:   entries,
		BonusCost: &ledger.BonusCostAttribution{Funding: fundingKind, ProviderID: providerID},
	})
	if err != nil {
		return HeldDisposition{}, err
	}
	if err := AttributeGrantLedgerTransactionIdempotent(ctx, tx, tenantID, disposition.GrantID, postResult.TransactionID, string(txType)); err != nil {
		return HeldDisposition{}, err
	}

	resolved, err := ResolveHeldDisposition(ctx, tx, tenantID, disposition.ID, newStatus, &p.ActorID, &p.ReasonCode, postResult.TransactionID, time.Now().UTC())
	if err != nil {
		return HeldDisposition{}, err
	}

	afterStatus := string(newStatus)
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: g.ID,
		TransitionType: TransitionHeldDispositionResolved, TriggerType: TriggerStaffAction,
		AfterStatus: &afterStatus, ActorType: ActorStaff, ActorID: &p.ActorID, ReasonCode: &p.ReasonCode,
		Amount: totalAmount, AssetCode: &g.AssetCode, LedgerTransactionID: &postResult.TransactionID,
		Detail: []byte(fmt.Sprintf(`{"action":%q,"held_disposition_id":%q}`, p.Action, disposition.ID)),
	}); err != nil {
		return HeldDisposition{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorStaff, ActorID: p.ActorID,
		Action: "bonus_held_disposition.resolved", TargetType: "bonus_held_disposition", TargetID: disposition.ID.String(),
		Outcome: audit.OutcomeSuccess, Metadata: map[string]any{"action": string(p.Action), "reason_code": p.ReasonCode},
	}); err != nil {
		return HeldDisposition{}, err
	}

	// If this was the last outstanding AOE component for G, flip
	// pending_settlement -> terminal_resolution directly (doc 10 N1.4
	// step 5c: "Bonus Engine already owns both the disposition write and
	// the Grant-status write in that one transaction... no seam is needed
	// here" - unlike the voided_by_rollback sub-case, which is casino's
	// own transaction and therefore goes through RecheckGrantExposure).
	if g.Status == GrantPendingSettlement {
		if _, err := RecheckGrantExposure(ctx, tx, tenantID, g.ID, postResult.TransactionID, TriggerHeldDispositionResolved); err != nil {
			return HeldDisposition{}, err
		}
	}

	return resolved, nil
}
