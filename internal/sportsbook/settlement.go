package sportsbook

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// Settlement lifecycle for cash-funded single bets, IN-HOUSE MOCK MODE
// (Stage 10 W1, docs/decisions/0088). This is NOT a real sportsbook
// provider integration: the only driver is the non-production
// test-support staff route (ADR 0088 §9), which simulates what a provider
// would tell the platform. Real provider settlement, webhooks, cashout,
// partial settlement and bonus-funded or mixed-funded settlement are out
// of scope (ADR 0087) and remain PROVIDER DEPENDENT / NOT IMPLEMENTED.
//
// Every operation runs inside the caller's tenant-scoped transaction and
// follows ADR 0088 §5.1 exactly:
//
//	L1  the bet row FOR UPDATE (RLS-scoped)
//	--  history, placement posting, NetLocked; the §4.3 decision table and
//	    the §2.4 payout validation. Rejections return here (no L2/L3).
//	L2  rollback / void-after-settlement: the settlement's
//	    ledger_transactions row FOR UPDATE
//	--  house_gaming account resolution; every TransactionInput built
//	L3  ledger.LockProjectionsForPostings over ALL of the operation's
//	    postings at once
//	L4  ledger.Post for each input, in order (rollback before void)
//	E-4 history row(s), status UPDATE(s), audit - only after every L3
//	    lock is held (INV-LOCK-E4, ADR 0082 Amendment A4)
//
// INV-LOCK-E4 / sole-writer rule: insertSettlementRecord and
// updateBetStatus below are the ONLY writers of sportsbook_bet_settlements
// and sportsbook_bets.status (besides insertBet's INSERT of a new 'open'
// row), and both are reached only through SimulateSettlementEvent's
// ordered path. sportsbook_settlement_sole_writer_test.go enforces this
// statically.
//
// Settlement never reads or locks sb_selections/sb_markets/sb_events
// (INV-SB-SETTLE-6, ADR 0047 §5(c)), and takes no L0 lock: settlement,
// void and rollback are not RG, Risk or exposure checkpoints (ADR 0038
// §13, ADR 0088 §5.4/§6.1).

// SettlementEventType is the simulated provider event kind.
type SettlementEventType string

const (
	SettlementEventSettle   SettlementEventType = "settle"
	SettlementEventRollback SettlementEventType = "rollback"
	SettlementEventVoid     SettlementEventType = "void"
)

// Settlement outcomes (sportsbook_bet_settlements.outcome).
const (
	SettlementOutcomeWon  = "won"
	SettlementOutcomeLost = "lost"
)

// Void reasons (sportsbook_bet_settlements.void_reason CHECK, migration
// 0091). player_self_exclusion is deliberately absent: it belongs to the
// future self-exclusion consumer's own migration (ADR 0034 §14.7).
var validVoidReasons = map[string]bool{"market_cancelled": true, "push": true, "data_error": true}

// Settlement result kinds (ADR 0088 §9.3).
const (
	SettlementResultApplied    = "applied"
	SettlementResultReplayed   = "replayed"
	SettlementResultTombstoned = "tombstoned"
)

// Rejection codes (ADR 0088 §4.3-§4.7, §9.3). A rejection is a RESULT,
// not a Go error, so its audit record commits with no ledger write.
const (
	SettlementRejectBetNotFound         = "NOT_FOUND"
	SettlementRejectPayloadMismatch     = "SETTLEMENT_PAYLOAD_MISMATCH"
	SettlementRejectTombstoned          = "SETTLEMENT_TOMBSTONED"
	SettlementRejectBetVoided           = "BET_VOIDED"
	SettlementRejectBetAlreadySettled   = "BET_ALREADY_SETTLED"
	SettlementRejectGenerationSequence  = "GENERATION_OUT_OF_SEQUENCE"
	SettlementRejectIntegrity           = "SETTLEMENT_INTEGRITY"
	SettlementRejectPayoutInvalid       = "PAYOUT_INVALID"
	SettlementRejectAssetMismatch       = "ASSET_MISMATCH"
	settlementAuditReasonCode           = "test_support_simulation"
	settlementDriver                    = "test_support_simulation"
	settlementMode                      = "in_house_mock"
	settlementAuditTargetType           = "sportsbook_bet"
	settlementAuditActionSettled        = "sportsbook_bet.settled"
	settlementAuditActionRolledBack     = "sportsbook_bet.rolled_back"
	settlementAuditActionVoided         = "sportsbook_bet.voided"
	settlementAuditActionTombstoned     = "sportsbook_bet.rollback_tombstoned"
	settlementAuditActionRejected       = "sportsbook_bet.settlement_rejected"
	settlementHistoryKindSettlement     = "settlement"
	settlementHistoryKindRollback       = "rollback"
	settlementHistoryKindVoid           = "void"
	settlementHistoryKindTombstone      = "tombstone"
	settlementIdempotencyPrefixSettle   = "sportsbook_settlement:"
	settlementIdempotencyPrefixRollback = "sportsbook_rollback:"
	settlementIdempotencyPrefixVoid     = "sportsbook_void:"
)

var (
	// ErrSettlementIntegrity aborts the whole transaction: a stored fact
	// contradicts the settlement contract after a posting may already
	// have been made (ADR 0088 §4.7), so nothing of this call may commit.
	// The HTTP layer alerts and audits it in a SEPARATE transaction.
	ErrSettlementIntegrity = errors.New("sportsbook: settlement integrity failure")
	// ErrSettlementActorNotActive: the staff principal driving the
	// simulation does not exist in this tenant or is not active.
	ErrSettlementActorNotActive = errors.New("sportsbook: settlement actor is not an active staff user of this tenant")
)

// SettlementEvent is one simulated provider event. Every effect-bearing
// value (tenant, player, wallet, accounts, stake, posted amounts) is
// re-derived server-side from the bet row and its placement posting;
// ClaimPayoutAmount/ClaimAssetCode are only the simulated provider's
// statement, validated against the derived values and never posted
// (ADR 0088 §2.4).
type SettlementEvent struct {
	TenantID     uuid.UUID // from the authenticated context only
	BetID        uuid.UUID
	ActorStaffID uuid.UUID // from the authenticated staff principal only
	EventType    SettlementEventType
	// Generation is required for settle and rollback, forbidden for void.
	Generation        int
	Outcome           string // settle only
	ClaimPayoutAmount int64  // settle only
	ClaimAssetCode    string // settle only
	VoidReason        string // void only
	RequestID         string
	IPAddress         string
	RemoteAddr        string
}

// SettlementResult is SimulateSettlementEvent's outcome.
type SettlementResult struct {
	BetID uuid.UUID
	// Result is applied, replayed or tombstoned; empty on a rejection.
	Result        string
	RejectionCode string
	// Alert is true when the rejection is an integrity alert (ADR 0088
	// §4.4); the HTTP layer emits
	// sportsbook_settlement_integrity_alert_<reason>.
	Alert                bool
	BetStatus            BetStatus
	Generation           int
	LedgerTransactionIDs []uuid.UUID
	SettlementRecordIDs  []uuid.UUID
}

// Rejected reports whether the event was rejected.
func (r SettlementResult) Rejected() bool { return r.RejectionCode != "" }

// SettlementRecord is one sportsbook_bet_settlements row.
type SettlementRecord struct {
	ID                   uuid.UUID
	TenantID             uuid.UUID
	BetID                uuid.UUID
	EventKind            string
	Generation           *int
	Outcome              *string
	PayoutAmount         *int64
	AssetCode            string
	VoidReason           *string
	ReversesSettlementID *uuid.UUID
	CausationRecordID    *uuid.UUID
	LedgerTransactionID  uuid.UUID
	ActorStaffAccountID  *uuid.UUID
	RequestID            *string
}

// settlementIdempotencyKey etc. compose the reserved in-house keys of ADR
// 0088 §4.2 from a server-resolved bet id and a validated generation. A
// tombstone deliberately shares the settlement key: it occupies the slot.
func settlementIdempotencyKey(betID uuid.UUID, generation int) string {
	return settlementIdempotencyPrefixSettle + betID.String() + "#" + strconv.Itoa(generation)
}

func rollbackIdempotencyKey(betID uuid.UUID, generation int) string {
	return settlementIdempotencyPrefixRollback + betID.String() + "#" + strconv.Itoa(generation)
}

func voidIdempotencyKey(betID uuid.UUID) string {
	return settlementIdempotencyPrefixVoid + betID.String()
}

// validateSettlementEvent enforces ADR 0088 §9.2's field matrix. The HTTP
// decoder enforces it too; this is the fail-closed service-level copy.
func validateSettlementEvent(ev SettlementEvent) error {
	if ev.TenantID == uuid.Nil || ev.BetID == uuid.Nil || ev.ActorStaffID == uuid.Nil {
		return fmt.Errorf("%w: settlement requires server-derived tenant, bet and staff actor ids", ErrInvalidInput)
	}
	switch ev.EventType {
	case SettlementEventSettle:
		if ev.Generation < 1 {
			return fmt.Errorf("%w: generation must be >= 1", ErrInvalidInput)
		}
		if ev.Outcome != SettlementOutcomeWon && ev.Outcome != SettlementOutcomeLost {
			return fmt.Errorf("%w: outcome must be won or lost", ErrInvalidInput)
		}
		if ev.ClaimPayoutAmount < 0 {
			return fmt.Errorf("%w: payout_amount must be non-negative", ErrInvalidInput)
		}
		if ev.ClaimAssetCode == "" {
			return fmt.Errorf("%w: asset_code is required for settle", ErrInvalidInput)
		}
		if ev.VoidReason != "" {
			return fmt.Errorf("%w: void_reason is forbidden for settle", ErrInvalidInput)
		}
	case SettlementEventRollback:
		if ev.Generation < 1 {
			return fmt.Errorf("%w: generation must be >= 1", ErrInvalidInput)
		}
		if ev.Outcome != "" || ev.ClaimPayoutAmount != 0 || ev.ClaimAssetCode != "" || ev.VoidReason != "" {
			return fmt.Errorf("%w: rollback carries only a generation", ErrInvalidInput)
		}
	case SettlementEventVoid:
		if ev.Generation != 0 || ev.Outcome != "" || ev.ClaimPayoutAmount != 0 || ev.ClaimAssetCode != "" {
			return fmt.Errorf("%w: void carries only a void_reason", ErrInvalidInput)
		}
		if !validVoidReasons[ev.VoidReason] {
			return fmt.Errorf("%w: void_reason must be market_cancelled, push or data_error", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: event_type must be settle, rollback or void", ErrInvalidInput)
	}
	return nil
}

// betLedgerAccounts are the bet's accounts, derived from its own placement
// posting (ADR 0088 §2.2) - never from the request or a wallet lookup.
type betLedgerAccounts struct {
	Cash, Locked, House uuid.UUID
	Stake               int64
}

// settlementState is everything the decision table needs, read under L1.
type settlementState struct {
	bet      Bet
	history  []SettlementRecord
	accounts betLedgerAccounts
	// maxGeneration is G: the max generation over settlement/tombstone
	// rows, 0 if none.
	maxGeneration int
	// current is the latest un-reversed settlement row, if any.
	current *SettlementRecord
}

func (s settlementState) settlementForGeneration(g int) *SettlementRecord {
	for i := range s.history {
		r := &s.history[i]
		if r.EventKind == settlementHistoryKindSettlement && r.Generation != nil && *r.Generation == g {
			return r
		}
	}
	return nil
}

func (s settlementState) tombstoneForGeneration(g int) *SettlementRecord {
	for i := range s.history {
		r := &s.history[i]
		if r.EventKind == settlementHistoryKindTombstone && r.Generation != nil && *r.Generation == g {
			return r
		}
	}
	return nil
}

func (s settlementState) rollbackOf(settlementID uuid.UUID) *SettlementRecord {
	for i := range s.history {
		r := &s.history[i]
		if r.EventKind == settlementHistoryKindRollback && r.ReversesSettlementID != nil && *r.ReversesSettlementID == settlementID {
			return r
		}
	}
	return nil
}

func (s settlementState) voidRecord() *SettlementRecord {
	for i := range s.history {
		if s.history[i].EventKind == settlementHistoryKindVoid {
			return &s.history[i]
		}
	}
	return nil
}

// SimulateSettlementEvent applies one simulated in-house settlement event
// to one bet, inside tx (db.Pool.WithTenant(ev.TenantID) - never a
// player-scoped or platform-admin transaction). See the file comment for
// the lock sequence. A rejection is returned as a result with its audit
// record written in tx; a Go error means tx must roll back.
func SimulateSettlementEvent(ctx context.Context, tx pgx.Tx, ev SettlementEvent) (SettlementResult, error) {
	if err := validateSettlementEvent(ev); err != nil {
		return SettlementResult{}, err
	}

	// ADR 0088 §9.1 (security S5 recommendation): the staff principal must
	// exist and be active in THIS tenant, checked in the posting
	// transaction, before anything is locked or posted.
	staff, err := identity.GetStaffUserByID(ctx, tx, ev.ActorStaffID)
	if errors.Is(err, identity.ErrNotFound) {
		return SettlementResult{}, ErrSettlementActorNotActive
	}
	if err != nil {
		return SettlementResult{}, fmt.Errorf("sportsbook: resolve settlement actor: %w", err)
	}
	if staff.TenantID != ev.TenantID || staff.Status != "active" {
		return SettlementResult{}, ErrSettlementActorNotActive
	}

	// L1: the bet row, RLS-scoped. Unknown or another tenant's id: 404,
	// no posting, no tombstone (ADR 0088 §4.6).
	bet, err := lockBetForSettlement(ctx, tx, ev.BetID)
	if errors.Is(err, ErrBetNotFound) {
		return rejectSettlement(ctx, tx, ev, nil, SettlementRejectBetNotFound, true)
	}
	if err != nil {
		return SettlementResult{}, err
	}
	if bet.TenantID != ev.TenantID {
		// Unreachable under RLS; fail closed rather than trust it.
		return SettlementResult{}, fmt.Errorf("%w: bet %s is not in tenant %s", ErrSettlementIntegrity, bet.ID, ev.TenantID)
	}

	state, rejection, err := loadSettlementState(ctx, tx, bet)
	if err != nil {
		return SettlementResult{}, err
	}
	if rejection != "" {
		return rejectSettlement(ctx, tx, ev, &bet, rejection, true)
	}

	switch ev.EventType {
	case SettlementEventSettle:
		return settleBet(ctx, tx, ev, state)
	case SettlementEventRollback:
		return rollbackBet(ctx, tx, ev, state)
	default:
		return voidBet(ctx, tx, ev, state)
	}
}

func lockBetForSettlement(ctx context.Context, tx pgx.Tx, betID uuid.UUID) (Bet, error) {
	b, err := scanBet(tx.QueryRow(ctx, `SELECT `+betColumns+` FROM sportsbook_bets WHERE id = $1 FOR UPDATE`, betID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Bet{}, ErrBetNotFound
	}
	if err != nil {
		return Bet{}, fmt.Errorf("sportsbook: lock bet for settlement: %w", err)
	}
	return b, nil
}

const settlementRecordColumns = `id, tenant_id, bet_id, event_kind, generation, outcome, payout_amount, asset_code,
	void_reason, reverses_settlement_id, causation_record_id, ledger_transaction_id, actor_staff_account_id, request_id`

func scanSettlementRecord(row pgx.Row) (SettlementRecord, error) {
	var r SettlementRecord
	var generation *int32
	err := row.Scan(&r.ID, &r.TenantID, &r.BetID, &r.EventKind, &generation, &r.Outcome, &r.PayoutAmount, &r.AssetCode,
		&r.VoidReason, &r.ReversesSettlementID, &r.CausationRecordID, &r.LedgerTransactionID, &r.ActorStaffAccountID, &r.RequestID)
	if err != nil {
		return SettlementRecord{}, err
	}
	if generation != nil {
		g := int(*generation)
		r.Generation = &g
	}
	return r, nil
}

// ListSettlementRecords returns a bet's lifecycle history in insertion
// order, within the current RLS scope (staff tenant scope, or the owning
// player's own scope).
func ListSettlementRecords(ctx context.Context, tx pgx.Tx, betID uuid.UUID) ([]SettlementRecord, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+settlementRecordColumns+` FROM sportsbook_bet_settlements WHERE bet_id = $1 ORDER BY created_at, generation NULLS LAST, id`,
		betID)
	if err != nil {
		return nil, fmt.Errorf("sportsbook: list settlement records: %w", err)
	}
	defer rows.Close()
	var out []SettlementRecord
	for rows.Next() {
		r, err := scanSettlementRecord(rows)
		if err != nil {
			return nil, fmt.Errorf("sportsbook: scan settlement record: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// loadSettlementState reads history and the placement posting under L1
// and performs ADR 0088 §2.2's checks. A §2.2 violation is returned as the
// SETTLEMENT_INTEGRITY rejection code (nothing has been posted yet).
func loadSettlementState(ctx context.Context, tx pgx.Tx, bet Bet) (settlementState, string, error) {
	history, err := ListSettlementRecords(ctx, tx, bet.ID)
	if err != nil {
		return settlementState{}, "", err
	}
	st := settlementState{bet: bet, history: history}
	for i := range history {
		r := &history[i]
		if (r.EventKind == settlementHistoryKindSettlement || r.EventKind == settlementHistoryKindTombstone) &&
			r.Generation != nil && *r.Generation > st.maxGeneration {
			st.maxGeneration = *r.Generation
		}
	}
	for i := range history {
		r := &history[i]
		if r.EventKind == settlementHistoryKindSettlement && st.rollbackOf(r.ID) == nil {
			if st.current != nil {
				return settlementState{}, SettlementRejectIntegrity, nil
			}
			st.current = r
		}
	}

	accounts, ok, err := deriveBetLedgerAccounts(ctx, tx, bet)
	if err != nil {
		return settlementState{}, "", err
	}
	if !ok {
		return settlementState{}, SettlementRejectIntegrity, nil
	}
	st.accounts = accounts

	// Status must agree with history (T-2 keeps it so; checked anyway).
	derived := BetStatusOpen
	switch {
	case st.voidRecord() != nil:
		derived = BetStatusVoid
	case st.current != nil && st.current.Outcome != nil && *st.current.Outcome == SettlementOutcomeWon:
		derived = BetStatusSettledWon
	case st.current != nil:
		derived = BetStatusSettledLost
	}
	if derived != bet.Status {
		return settlementState{}, SettlementRejectIntegrity, nil
	}

	// NetLocked (ADR 0088 §2.2 steps 4-5).
	netLocked, netLockedBonus, err := betNetLocked(ctx, tx, bet.ID, accounts.Locked)
	if err != nil {
		return settlementState{}, "", err
	}
	if netLockedBonus.Sign() != 0 {
		return settlementState{}, SettlementRejectIntegrity, nil
	}
	want := big.NewInt(0)
	if bet.Status == BetStatusOpen {
		want = big.NewInt(accounts.Stake)
	}
	if netLocked.Cmp(want) != 0 {
		return settlementState{}, SettlementRejectIntegrity, nil
	}
	return st, "", nil
}

// deriveBetLedgerAccounts implements ADR 0088 §2.2 steps 1-3. ok=false is
// an integrity failure of the placement posting.
func deriveBetLedgerAccounts(ctx context.Context, tx pgx.Tx, bet Bet) (betLedgerAccounts, bool, error) {
	var txType string
	var correlationID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT transaction_type, correlation_id FROM ledger_transactions WHERE id = $1`, bet.LedgerTransactionID,
	).Scan(&txType, &correlationID)
	if errors.Is(err, pgx.ErrNoRows) {
		return betLedgerAccounts{}, false, nil
	}
	if err != nil {
		return betLedgerAccounts{}, false, fmt.Errorf("sportsbook: load bet placement transaction: %w", err)
	}
	if ledger.TransactionType(txType) != ledger.TxSportsbookBet || correlationID != bet.ID {
		return betLedgerAccounts{}, false, nil
	}

	rows, err := tx.Query(ctx,
		`SELECT e.ledger_account_id, e.direction, e.amount::text, la.account_type, la.asset_code, la.wallet_id
		   FROM ledger_entries e
		   JOIN ledger_accounts la ON la.id = e.ledger_account_id
		  WHERE e.ledger_transaction_id = $1`, bet.LedgerTransactionID)
	if err != nil {
		return betLedgerAccounts{}, false, fmt.Errorf("sportsbook: load bet placement entries: %w", err)
	}
	defer rows.Close()
	var out betLedgerAccounts
	var debitAmount, creditAmount string
	n := 0
	for rows.Next() {
		var accountID uuid.UUID
		var direction, amount, accountType, assetCode string
		var walletID *uuid.UUID
		if err := rows.Scan(&accountID, &direction, &amount, &accountType, &assetCode, &walletID); err != nil {
			return betLedgerAccounts{}, false, fmt.Errorf("sportsbook: scan bet placement entry: %w", err)
		}
		n++
		if assetCode != bet.AssetCode || walletID == nil || *walletID != bet.WalletID {
			return betLedgerAccounts{}, false, nil
		}
		switch {
		case direction == string(ledger.Debit) && accountType == string(ledger.AccountPlayerCash):
			out.Cash, debitAmount = accountID, amount
		case direction == string(ledger.Credit) && accountType == string(ledger.AccountPlayerLockedCash):
			out.Locked, creditAmount = accountID, amount
		default:
			return betLedgerAccounts{}, false, nil
		}
	}
	if err := rows.Err(); err != nil {
		return betLedgerAccounts{}, false, fmt.Errorf("sportsbook: read bet placement entries: %w", err)
	}
	if n != 2 || out.Cash == uuid.Nil || out.Locked == uuid.Nil || debitAmount != creditAmount {
		return betLedgerAccounts{}, false, nil
	}
	stake, err := strconv.ParseInt(debitAmount, 10, 64)
	if err != nil || stake != bet.StakeAmount || stake <= 0 {
		return betLedgerAccounts{}, false, nil
	}
	out.Stake = stake

	// house_gaming is resolved before any L3 lock (ADR 0082 §2.1).
	house, err := ledger.GetOrCreateAccounts(ctx, tx, bet.TenantID,
		ledger.AccountSpec{WalletID: nil, AccountType: ledger.AccountHouseGaming, AssetCode: bet.AssetCode})
	if err != nil {
		return betLedgerAccounts{}, false, fmt.Errorf("sportsbook: resolve house_gaming account: %w", err)
	}
	out.House = house[0]
	return out, true, nil
}

// sportsbookLifecycleTypes are the transaction types whose entries make up
// a bet's lifecycle (ADR 0088 §2.2 step 4, §8.1).
var sportsbookLifecycleTypes = []string{
	string(ledger.TxSportsbookBet), string(ledger.TxSportsbookSettlement),
	string(ledger.TxSportsbookVoid), string(ledger.TxSportsbookRollback),
}

// betNetLocked returns (credit - debit) over the bet's lifecycle entries
// on its locked-cash account, and over any player_locked_bonus account.
// NUMERIC sums are scanned as text into big.Int, never int64.
func betNetLocked(ctx context.Context, tx pgx.Tx, betID, lockedAccountID uuid.UUID) (*big.Int, *big.Int, error) {
	var locked, lockedBonus string
	err := tx.QueryRow(ctx,
		`SELECT
		     COALESCE(SUM(CASE WHEN e.ledger_account_id = $2
		                       THEN CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END END), 0)::text,
		     COALESCE(SUM(CASE WHEN la.account_type = 'player_locked_bonus'
		                       THEN CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END END), 0)::text
		   FROM ledger_entries e
		   JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		   JOIN ledger_accounts la ON la.id = e.ledger_account_id
		  WHERE t.correlation_id = $1 AND t.transaction_type = ANY($3)`,
		betID, lockedAccountID, sportsbookLifecycleTypes,
	).Scan(&locked, &lockedBonus)
	if err != nil {
		return nil, nil, fmt.Errorf("sportsbook: compute bet net locked: %w", err)
	}
	l, ok1 := new(big.Int).SetString(locked, 10)
	lb, ok2 := new(big.Int).SetString(lockedBonus, 10)
	if !ok1 || !ok2 {
		return nil, nil, fmt.Errorf("%w: unparseable net-locked sum", ErrSettlementIntegrity)
	}
	return l, lb, nil
}

// settleBet is ADR 0088 §4.3's settle(g) decision table.
func settleBet(ctx context.Context, tx pgx.Tx, ev SettlementEvent, st settlementState) (SettlementResult, error) {
	bet := st.bet
	g := ev.Generation
	if existing := st.settlementForGeneration(g); existing != nil {
		if existing.Outcome == nil || *existing.Outcome != ev.Outcome || existing.PayoutAmount == nil ||
			*existing.PayoutAmount != ev.ClaimPayoutAmount || existing.AssetCode != ev.ClaimAssetCode {
			return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectPayloadMismatch, true)
		}
		return replaySettlement(ctx, tx, ev, bet, []SettlementRecord{*existing}, settlementAuditActionSettled)
	}
	if st.tombstoneForGeneration(g) != nil {
		return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectTombstoned, true)
	}
	if bet.Status == BetStatusVoid {
		return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectBetVoided, true)
	}
	if bet.Status == BetStatusSettledWon || bet.Status == BetStatusSettledLost {
		return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectBetAlreadySettled, true)
	}
	if g != st.maxGeneration+1 {
		return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectGenerationSequence, false)
	}

	// ADR 0088 §2.4 V-1..V-4 (V-5 is the decoder's and validateSettlementEvent's).
	if ev.ClaimAssetCode != bet.AssetCode {
		return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectAssetMismatch, true)
	}
	var payout int64
	switch ev.Outcome {
	case SettlementOutcomeLost:
		if ev.ClaimPayoutAmount != 0 {
			return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectPayoutInvalid, true)
		}
	case SettlementOutcomeWon:
		if bet.PotentialReturn <= 0 || ev.ClaimPayoutAmount != bet.PotentialReturn {
			return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectPayoutInvalid, true)
		}
		payout = bet.PotentialReturn
	}

	// Re-settlement causation (ADR 0088 §2.1): the rollback or tombstone
	// of generation g-1.
	var causationRecord *SettlementRecord
	if g > 1 {
		if prev := st.settlementForGeneration(g - 1); prev != nil {
			causationRecord = st.rollbackOf(prev.ID)
		} else {
			causationRecord = st.tombstoneForGeneration(g - 1)
		}
		if causationRecord == nil {
			return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectIntegrity, true)
		}
	}

	a := st.accounts
	entries := []ledger.EntryInput{
		{LedgerAccountID: a.Locked, Direction: ledger.Debit, Amount: a.Stake},
		{LedgerAccountID: a.House, Direction: ledger.Credit, Amount: a.Stake},
	}
	if payout > 0 {
		// Full payout, not winnings (Flow 9 trap): Dr house P / Cr cash P.
		entries = append(entries,
			ledger.EntryInput{LedgerAccountID: a.House, Direction: ledger.Debit, Amount: payout},
			ledger.EntryInput{LedgerAccountID: a.Cash, Direction: ledger.Credit, Amount: payout},
		)
	}
	in := ledger.TransactionInput{
		TenantID: bet.TenantID, TransactionType: ledger.TxSportsbookSettlement,
		IdempotencyKey: settlementIdempotencyKey(bet.ID, g), CorrelationID: bet.ID,
		Entries: entries,
	}
	if causationRecord != nil {
		causation := causationRecord.LedgerTransactionID
		in.CausationID = &causation
	}

	txIDs, err := lockAndPost(ctx, tx, in)
	if err != nil {
		return SettlementResult{}, err
	}
	outcome := ev.Outcome
	rec := SettlementRecord{
		TenantID: bet.TenantID, BetID: bet.ID, EventKind: settlementHistoryKindSettlement,
		Generation: &g, Outcome: &outcome, PayoutAmount: &payout, AssetCode: bet.AssetCode,
		LedgerTransactionID: txIDs[0],
	}
	if causationRecord != nil {
		rec.CausationRecordID = &causationRecord.ID
	}
	newStatus := BetStatusSettledLost
	if outcome == SettlementOutcomeWon {
		newStatus = BetStatusSettledWon
	}
	recID, err := writeTransition(ctx, tx, ev, bet.Status, newStatus, rec, settlementAuditActionSettled)
	if err != nil {
		return SettlementResult{}, err
	}
	return SettlementResult{
		BetID: bet.ID, Result: SettlementResultApplied, BetStatus: newStatus, Generation: g,
		LedgerTransactionIDs: txIDs, SettlementRecordIDs: []uuid.UUID{recID},
	}, nil
}

// rollbackBet is ADR 0088 §4.3's rollback(g) decision table.
func rollbackBet(ctx context.Context, tx pgx.Tx, ev SettlementEvent, st settlementState) (SettlementResult, error) {
	bet := st.bet
	g := ev.Generation
	if s := st.settlementForGeneration(g); s != nil {
		if rb := st.rollbackOf(s.ID); rb != nil {
			return replaySettlement(ctx, tx, ev, bet, []SettlementRecord{*rb}, settlementAuditActionRolledBack)
		}
	}
	if tomb := st.tombstoneForGeneration(g); tomb != nil {
		res, err := replaySettlement(ctx, tx, ev, bet, []SettlementRecord{*tomb}, settlementAuditActionTombstoned)
		return res, err
	}
	if st.current != nil && st.current.Generation != nil && *st.current.Generation == g {
		rbIn, err := buildRollbackInput(ctx, tx, st, *st.current)
		if err != nil {
			return SettlementResult{}, err
		}
		txIDs, err := lockAndPost(ctx, tx, rbIn)
		if err != nil {
			return SettlementResult{}, err
		}
		rec := rollbackRecord(bet, *st.current, txIDs[0])
		recID, err := writeTransition(ctx, tx, ev, bet.Status, BetStatusOpen, rec, settlementAuditActionRolledBack)
		if err != nil {
			return SettlementResult{}, err
		}
		return SettlementResult{
			BetID: bet.ID, Result: SettlementResultApplied, BetStatus: BetStatusOpen, Generation: g,
			LedgerTransactionIDs: txIDs, SettlementRecordIDs: []uuid.UUID{recID},
		}, nil
	}
	if bet.Status == BetStatusOpen && g == st.maxGeneration+1 {
		// ADR 0088 §4.5: rollback of a never-seen settlement writes a
		// tombstone occupying the settlement key, so a late settle(g) is
		// rejected. No entries, no L3 lock, status stays open.
		in := ledger.TransactionInput{
			TenantID: bet.TenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: settlementIdempotencyKey(bet.ID, g), CorrelationID: bet.ID,
		}
		txIDs, err := lockAndPost(ctx, tx, in)
		if err != nil {
			return SettlementResult{}, err
		}
		rec := SettlementRecord{
			TenantID: bet.TenantID, BetID: bet.ID, EventKind: settlementHistoryKindTombstone,
			Generation: &g, AssetCode: bet.AssetCode, LedgerTransactionID: txIDs[0],
		}
		recID, err := writeTransition(ctx, tx, ev, bet.Status, bet.Status, rec, settlementAuditActionTombstoned)
		if err != nil {
			return SettlementResult{}, err
		}
		return SettlementResult{
			BetID: bet.ID, Result: SettlementResultTombstoned, BetStatus: bet.Status, Generation: g,
			LedgerTransactionIDs: txIDs, SettlementRecordIDs: []uuid.UUID{recID},
		}, nil
	}
	if bet.Status == BetStatusVoid {
		return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectBetVoided, true)
	}
	return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectGenerationSequence, false)
}

// voidBet is ADR 0088 §4.3's void(void_reason) decision table.
func voidBet(ctx context.Context, tx pgx.Tx, ev SettlementEvent, st settlementState) (SettlementResult, error) {
	bet := st.bet
	if existing := st.voidRecord(); existing != nil {
		if existing.VoidReason == nil || *existing.VoidReason != ev.VoidReason {
			return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectPayloadMismatch, true)
		}
		return replaySettlement(ctx, tx, ev, bet, []SettlementRecord{*existing}, settlementAuditActionVoided)
	}
	a := st.accounts
	voidIn := ledger.TransactionInput{
		TenantID: bet.TenantID, TransactionType: ledger.TxSportsbookVoid,
		IdempotencyKey: voidIdempotencyKey(bet.ID), CorrelationID: bet.ID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: a.Locked, Direction: ledger.Debit, Amount: a.Stake},
			{LedgerAccountID: a.Cash, Direction: ledger.Credit, Amount: a.Stake},
		},
	}
	reason := ev.VoidReason
	voidRec := SettlementRecord{
		TenantID: bet.TenantID, BetID: bet.ID, EventKind: settlementHistoryKindVoid,
		VoidReason: &reason, AssetCode: bet.AssetCode,
	}

	if bet.Status == BetStatusOpen {
		txIDs, err := lockAndPost(ctx, tx, voidIn)
		if err != nil {
			return SettlementResult{}, err
		}
		voidRec.LedgerTransactionID = txIDs[0]
		recID, err := writeTransition(ctx, tx, ev, bet.Status, BetStatusVoid, voidRec, settlementAuditActionVoided)
		if err != nil {
			return SettlementResult{}, err
		}
		return SettlementResult{
			BetID: bet.ID, Result: SettlementResultApplied, BetStatus: BetStatusVoid,
			LedgerTransactionIDs: txIDs, SettlementRecordIDs: []uuid.UUID{recID},
		}, nil
	}

	// Void after settlement: rollback of the current settlement, then the
	// before-settlement-shape void, one DB transaction, both pre-locked
	// together (ADR 0088 §2.3, §5.2).
	if st.current == nil {
		return rejectSettlement(ctx, tx, ev, &bet, SettlementRejectIntegrity, true)
	}
	rbIn, err := buildRollbackInput(ctx, tx, st, *st.current)
	if err != nil {
		return SettlementResult{}, err
	}
	txIDs, err := lockAndPost(ctx, tx, rbIn, voidIn)
	if err != nil {
		return SettlementResult{}, err
	}
	rbRec := rollbackRecord(bet, *st.current, txIDs[0])
	rbEv := ev
	rbEv.EventType = SettlementEventRollback
	rbEv.Generation = *st.current.Generation
	rbEv.VoidReason = ""
	rbRecID, err := writeTransition(ctx, tx, rbEv, bet.Status, BetStatusOpen, rbRec, settlementAuditActionRolledBack)
	if err != nil {
		return SettlementResult{}, err
	}
	voidRec.LedgerTransactionID = txIDs[1]
	voidRec.CausationRecordID = &rbRecID
	voidRecID, err := writeTransition(ctx, tx, ev, BetStatusOpen, BetStatusVoid, voidRec, settlementAuditActionVoided)
	if err != nil {
		return SettlementResult{}, err
	}
	return SettlementResult{
		BetID: bet.ID, Result: SettlementResultApplied, BetStatus: BetStatusVoid,
		LedgerTransactionIDs: txIDs, SettlementRecordIDs: []uuid.UUID{rbRecID, voidRecID},
	}, nil
}

func rollbackRecord(bet Bet, settlement SettlementRecord, ledgerTxID uuid.UUID) SettlementRecord {
	g := *settlement.Generation
	return SettlementRecord{
		TenantID: bet.TenantID, BetID: bet.ID, EventKind: settlementHistoryKindRollback,
		Generation: &g, AssetCode: bet.AssetCode, ReversesSettlementID: &settlement.ID,
		LedgerTransactionID: ledgerTxID,
	}
}

// buildRollbackInput takes the L2 lock on the settlement's ledger
// transaction, confirms it has never been reversed (ADR 0038 §10), and
// returns its exact inverse, loaded from ledger_entries (the
// casino.postRollback inversion pattern).
func buildRollbackInput(ctx context.Context, tx pgx.Tx, st settlementState, settlement SettlementRecord) (ledger.TransactionInput, error) {
	bet := st.bet
	var txType string
	var correlationID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT transaction_type, correlation_id FROM ledger_transactions WHERE id = $1 FOR UPDATE`,
		settlement.LedgerTransactionID,
	).Scan(&txType, &correlationID)
	if err != nil {
		return ledger.TransactionInput{}, fmt.Errorf("%w: lock settlement transaction %s: %w", ErrSettlementIntegrity, settlement.LedgerTransactionID, err)
	}
	if ledger.TransactionType(txType) != ledger.TxSportsbookSettlement || correlationID != bet.ID {
		return ledger.TransactionInput{}, fmt.Errorf("%w: settlement transaction %s has type %q / correlation %s",
			ErrSettlementIntegrity, settlement.LedgerTransactionID, txType, correlationID)
	}
	var reversed bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE reverses_transaction_id = $1)`,
		settlement.LedgerTransactionID,
	).Scan(&reversed); err != nil {
		return ledger.TransactionInput{}, fmt.Errorf("sportsbook: check existing settlement reversal: %w", err)
	}
	if reversed {
		return ledger.TransactionInput{}, fmt.Errorf("%w: settlement transaction %s is already reversed", ErrSettlementIntegrity, settlement.LedgerTransactionID)
	}

	rows, err := tx.Query(ctx,
		`SELECT ledger_account_id, direction, amount::text FROM ledger_entries
		  WHERE ledger_transaction_id = $1 ORDER BY ledger_account_id, direction`,
		settlement.LedgerTransactionID)
	if err != nil {
		return ledger.TransactionInput{}, fmt.Errorf("sportsbook: load settlement entries: %w", err)
	}
	defer rows.Close()
	allowed := map[uuid.UUID]bool{st.accounts.Cash: true, st.accounts.Locked: true, st.accounts.House: true}
	var entries []ledger.EntryInput
	for rows.Next() {
		var accountID uuid.UUID
		var direction, amountText string
		if err := rows.Scan(&accountID, &direction, &amountText); err != nil {
			return ledger.TransactionInput{}, fmt.Errorf("sportsbook: scan settlement entry: %w", err)
		}
		amount, err := strconv.ParseInt(amountText, 10, 64)
		if err != nil || !allowed[accountID] {
			return ledger.TransactionInput{}, fmt.Errorf("%w: settlement transaction %s has an unexpected entry", ErrSettlementIntegrity, settlement.LedgerTransactionID)
		}
		flipped := ledger.Credit
		if ledger.Direction(direction) == ledger.Credit {
			flipped = ledger.Debit
		}
		entries = append(entries, ledger.EntryInput{LedgerAccountID: accountID, Direction: flipped, Amount: amount})
	}
	if err := rows.Err(); err != nil {
		return ledger.TransactionInput{}, fmt.Errorf("sportsbook: read settlement entries: %w", err)
	}
	if len(entries) == 0 {
		return ledger.TransactionInput{}, fmt.Errorf("%w: settlement transaction %s has no entries", ErrSettlementIntegrity, settlement.LedgerTransactionID)
	}
	reverses := settlement.LedgerTransactionID
	return ledger.TransactionInput{
		TenantID: bet.TenantID, TransactionType: ledger.TxSportsbookRollback,
		IdempotencyKey: rollbackIdempotencyKey(bet.ID, *settlement.Generation), CorrelationID: bet.ID,
		ReversesTransactionID: &reverses, Entries: entries,
	}, nil
}

// lockAndPost is L3 then L4 (ADR 0088 §5.1 steps 5-6): one pre-lock over
// every input, then Post in order. When there are two inputs (void after
// settlement) the second is the void, whose CausationID is the first's
// transaction id - not an entry field, so the pre-locked entry set is
// unchanged (§2.1). Every posting here was classified NEW by the decision
// table; a ledger-level replay or key reuse is therefore an integrity
// failure that aborts the transaction (§4.7).
func lockAndPost(ctx context.Context, tx pgx.Tx, ins ...ledger.TransactionInput) ([]uuid.UUID, error) {
	if _, err := ledger.LockProjectionsForPostings(ctx, tx, ins...); err != nil {
		return nil, fmt.Errorf("sportsbook: lock settlement projections: %w", err)
	}
	ids := make([]uuid.UUID, 0, len(ins))
	for i, in := range ins {
		if i > 0 {
			prev := ids[i-1]
			in.CausationID = &prev
		}
		res, err := ledger.Post(ctx, tx, in)
		if errors.Is(err, ledger.ErrIdempotencyKeyReused) {
			return nil, fmt.Errorf("%w: ledger key %s already holds another transaction type: %w", ErrSettlementIntegrity, in.IdempotencyKey, err)
		}
		// BUGFIX (qa, found while writing the W1 acceptance suite):
		// ledger.ErrIdempotencyPayloadMismatch (the F-7 ledger-level fix,
		// ADR 0020 amendment 2026-09-25) is a THIRD replay-shaped outcome
		// Post can now return, alongside ErrIdempotencyKeyReused and
		// AlreadyPosted=true - same key and type, different canonical
		// entries. ADR 0088 §4.7 requires every Post-detected replay for a
		// posting §4.3 classified as NEW to abort as ErrSettlementIntegrity;
		// before this fix it fell through to the generic wrap below and was
		// never classified as an integrity failure at all.
		if errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) {
			return nil, fmt.Errorf("%w: ledger key %s already holds a transaction with different entries: %w", ErrSettlementIntegrity, in.IdempotencyKey, err)
		}
		if err != nil {
			return nil, fmt.Errorf("sportsbook: post %s: %w", in.TransactionType, err)
		}
		if res.AlreadyPosted {
			return nil, fmt.Errorf("%w: ledger key %s was already posted though the settlement history has no record of it", ErrSettlementIntegrity, in.IdempotencyKey)
		}
		ids = append(ids, res.TransactionID)
	}
	if err := settlementAfterPostHook(ctx, tx); err != nil {
		return nil, err
	}
	return ids, nil
}

// writeTransition is ADR 0088 §5.1 step 7 (E-4) for one history row:
// insert the row, then (when the status changes) the status UPDATE, then
// the audit record - all after every L3 lock is held (INV-LOCK-E4).
func writeTransition(ctx context.Context, tx pgx.Tx, ev SettlementEvent, before, after BetStatus, rec SettlementRecord, action string) (uuid.UUID, error) {
	actor := ev.ActorStaffID
	rec.ActorStaffAccountID = &actor
	if ev.RequestID != "" {
		requestID := ev.RequestID
		rec.RequestID = &requestID
	}
	recID, err := insertSettlementRecord(ctx, tx, rec)
	if err != nil {
		return uuid.Nil, err
	}
	if before != after {
		if err := updateBetStatus(ctx, tx, rec.BetID, after); err != nil {
			return uuid.Nil, err
		}
	}
	md := settlementAuditMetadata(ev, before, after)
	md["ledger_transaction_ids"] = []string{rec.LedgerTransactionID.String()}
	md["settlement_record_ids"] = []string{recID.String()}
	md["replayed"] = false
	if rec.Generation != nil {
		md["generation"] = *rec.Generation
	}
	if rec.PayoutAmount != nil {
		md["payout_amount"] = *rec.PayoutAmount
	}
	if err := audit.Record(ctx, tx, settlementAuditEntry(ev, rec.BetID.String(), action, audit.OutcomeSuccess, md)); err != nil {
		return uuid.Nil, fmt.Errorf("sportsbook: audit %s: %w", action, err)
	}
	return recID, nil
}

// insertSettlementRecord is the sole writer of sportsbook_bet_settlements
// (INV-LOCK-E4). T-1 (migration 0091) re-validates the row; a violation
// here means the Go decision table and the database disagree, so it is an
// integrity failure.
func insertSettlementRecord(ctx context.Context, tx pgx.Tx, rec SettlementRecord) (uuid.UUID, error) {
	var generation *int32
	if rec.Generation != nil {
		g := int32(*rec.Generation) //nolint:gosec // G115: generation is validated >= 1 and bounded by history length
		generation = &g
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`INSERT INTO sportsbook_bet_settlements
			(tenant_id, bet_id, event_kind, generation, outcome, payout_amount, asset_code, void_reason,
			 reverses_settlement_id, causation_record_id, ledger_transaction_id, actor_staff_account_id, request_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		 RETURNING id`,
		rec.TenantID, rec.BetID, rec.EventKind, generation, rec.Outcome, rec.PayoutAmount, rec.AssetCode, rec.VoidReason,
		rec.ReversesSettlementID, rec.CausationRecordID, rec.LedgerTransactionID, rec.ActorStaffAccountID, rec.RequestID,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("%w: insert %s history row: %w", ErrSettlementIntegrity, rec.EventKind, err)
	}
	return id, nil
}

// updateBetStatus is the sole writer of sportsbook_bets.status besides
// insertBet's INSERT (INV-LOCK-E4). T-2 (migration 0091) re-derives the
// status from history and rejects anything else.
func updateBetStatus(ctx context.Context, tx pgx.Tx, betID uuid.UUID, status BetStatus) error {
	tag, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET status = $2 WHERE id = $1`, betID, status)
	if err != nil {
		return fmt.Errorf("%w: update bet status: %w", ErrSettlementIntegrity, err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("%w: bet status update affected %d rows", ErrSettlementIntegrity, tag.RowsAffected())
	}
	return nil
}

// replaySettlement answers an exact redelivery: nothing is posted, the
// original records are returned, and an audit record with replayed=true
// keeps retries traceable (ADR 0088 §10).
func replaySettlement(ctx context.Context, tx pgx.Tx, ev SettlementEvent, bet Bet, recs []SettlementRecord, action string) (SettlementResult, error) {
	res := SettlementResult{BetID: bet.ID, Result: SettlementResultReplayed, BetStatus: bet.Status, Generation: ev.Generation}
	var txIDs, recIDs []string
	for _, r := range recs {
		res.LedgerTransactionIDs = append(res.LedgerTransactionIDs, r.LedgerTransactionID)
		res.SettlementRecordIDs = append(res.SettlementRecordIDs, r.ID)
		txIDs = append(txIDs, r.LedgerTransactionID.String())
		recIDs = append(recIDs, r.ID.String())
	}
	md := settlementAuditMetadata(ev, bet.Status, bet.Status)
	md["ledger_transaction_ids"] = txIDs
	md["settlement_record_ids"] = recIDs
	md["replayed"] = true
	if err := audit.Record(ctx, tx, settlementAuditEntry(ev, bet.ID.String(), action, audit.OutcomeSuccess, md)); err != nil {
		return SettlementResult{}, fmt.Errorf("sportsbook: audit settlement replay: %w", err)
	}
	return res, nil
}

// rejectSettlement records a rejection (ADR 0088 §4.4): audit only, no
// ledger write, committed by the caller.
func rejectSettlement(ctx context.Context, tx pgx.Tx, ev SettlementEvent, bet *Bet, code string, alert bool) (SettlementResult, error) {
	res := SettlementResult{BetID: ev.BetID, RejectionCode: code, Alert: alert, Generation: ev.Generation}
	var status BetStatus
	if bet != nil {
		status = bet.Status
		res.BetStatus = bet.Status
	}
	if err := RecordSettlementRejection(ctx, tx, ev, status, code); err != nil {
		return SettlementResult{}, err
	}
	return res, nil
}

// RecordSettlementRejection writes the sportsbook_bet.settlement_rejected
// audit record. Exported so the HTTP layer can record an
// ErrSettlementIntegrity abort in a separate transaction (ADR 0088 §4.7):
// the failed transaction cannot carry it.
func RecordSettlementRejection(ctx context.Context, tx pgx.Tx, ev SettlementEvent, betStatus BetStatus, code string) error {
	md := settlementAuditMetadata(ev, betStatus, betStatus)
	md["rejection_code"] = code
	md["bet_status"] = string(betStatus)
	if ev.Generation != 0 {
		md["generation"] = ev.Generation
	}
	if err := audit.Record(ctx, tx, settlementAuditEntry(ev, ev.BetID.String(), settlementAuditActionRejected, audit.OutcomeFailure, md)); err != nil {
		return fmt.Errorf("sportsbook: audit settlement rejection: %w", err)
	}
	return nil
}

func settlementAuditEntry(ev SettlementEvent, targetID, action string, outcome audit.Outcome, md map[string]any) audit.Entry {
	return audit.Entry{
		TenantID: ev.TenantID, ActorType: audit.ActorStaff, ActorID: ev.ActorStaffID,
		Action: action, TargetType: settlementAuditTargetType, TargetID: targetID, Outcome: outcome,
		IPAddress: ev.IPAddress, RequestID: ev.RequestID, Metadata: md,
	}
}

// settlementAuditMetadata is ADR 0088 §10's metadata set. reason_code is
// test_support_simulation for settle and rollback; void records carry
// void_reason instead.
func settlementAuditMetadata(ev SettlementEvent, before, after BetStatus) map[string]any {
	md := map[string]any{
		"before_status": string(before), "after_status": string(after),
		"event_type": string(ev.EventType), "driver": settlementDriver, "mode": settlementMode,
		"remote_addr": ev.RemoteAddr,
	}
	switch ev.EventType {
	case SettlementEventSettle:
		md["reason_code"] = settlementAuditReasonCode
		md["outcome"] = ev.Outcome
		md["payout_amount"] = ev.ClaimPayoutAmount
		md["asset_code"] = ev.ClaimAssetCode
	case SettlementEventRollback:
		md["reason_code"] = settlementAuditReasonCode
	case SettlementEventVoid:
		md["void_reason"] = ev.VoidReason
	}
	if ev.Generation != 0 {
		md["generation"] = ev.Generation
	}
	return md
}
