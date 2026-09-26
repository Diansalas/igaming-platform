// Stage 4H-B1 Wave 2 Phase 7: wires the G-2 casino/bonus integration this
// package's own frozen design already specifies
// (docs/architecture/08-casino-integration-architecture.md §16 in full)
// into real postWin/postRollback code, calling into
// internal/bonus.ResolveTerminalGrantCredit and
// internal/bonus.RecheckGrantExposure - the two seams Phase 3 built and
// froze from bonus-engine's own side.
//
// Nothing here selects G-2 (the human decision, docs/decisions/0039-*.md
// Decision 2) - it only implements the destination-resolution query
// (§16.4/§16.4a, closing LF-18), the unconditional hold-capture posting
// (§16.9/§16.14), the held-win rollback transition (§16.15), and the
// plain-lock-rollback/held-win-rollback RecheckGrantExposure call sites
// (§16.21). The three G-2 disposition actions themselves
// (ACTION_REFORFEIT/ACTION_ROUTE_TO_CASH/ACTION_HOLD_FOR_REVIEW) are
// entirely internal/bonus's own resolution flow (held_disposition_ops.go,
// ResolveHeldDispositionAction) - this package never decides among them.
package casino

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
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// --- §16.4/§16.4a: the destination-resolution query, LF-18's fix ---

// originRow is one row of §16.4's Step 1 (locked-credit-leg identity) or
// Step 2 (direct-debit-leg identity) query result.
type originRow struct {
	BetTransactionID uuid.UUID
	AccountType      ledger.AccountType
	WalletID         uuid.UUID
	AssetCode        string
}

// winOrigin is §16.4's fully-resolved origin: which account a round's
// stake actually came from (never from event.PlayerAccountID or any
// other payload field - §16.3/§16.18's standing invariant), and, for a
// locked origin, Step 1b's live, currently-outstanding amount (LF-18) -
// never Step 1's possibly-stale bet-time figure.
type winOrigin struct {
	BetTransactionID     uuid.UUID
	AccountType          ledger.AccountType
	WalletID             uuid.UUID
	AssetCode            string
	Locked               bool
	NetOutstandingLocked int64 // meaningful only when Locked
}

var lockedOriginAccountTypes = []ledger.AccountType{ledger.AccountPlayerLockedCash, ledger.AccountPlayerLockedBonus}
var directOriginAccountTypes = []ledger.AccountType{ledger.AccountPlayerCash, ledger.AccountPlayerBonus}

func accountTypeStrings(types []ledger.AccountType) []string {
	out := make([]string, len(types))
	for i, t := range types {
		out[i] = string(t)
	}
	return out
}

// queryOriginRows implements §16.4's Step 1 (direction=credit,
// lockedOriginAccountTypes) and Step 2 (direction=debit,
// directOriginAccountTypes) with one shared shape, parameterized only by
// which leg/account-type family the caller wants - exactly the two SQL
// blocks the document specifies, reused verbatim (the NOT EXISTS
// (reverses_transaction_id) exclusion is §16.1's existing, unmodified
// clause). tenantID/correlationID are always server-derived
// (roundCorrelationID); nothing here ever reads a payload field.
func queryOriginRows(ctx context.Context, tx pgx.Tx, tenantID, correlationID uuid.UUID, direction ledger.Direction, accountTypes []ledger.AccountType) ([]originRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.id, la.account_type, la.wallet_id, la.asset_code
		  FROM ledger_entries      e
		  JOIN ledger_accounts     la ON la.id = e.ledger_account_id
		  JOIN ledger_transactions t  ON t.id  = e.ledger_transaction_id
		 WHERE t.tenant_id = $1
		   AND t.correlation_id = $2
		   AND t.transaction_type = $3
		   AND e.direction = $4
		   AND la.account_type = ANY($5)
		   AND NOT EXISTS (SELECT 1 FROM ledger_transactions r WHERE r.reverses_transaction_id = t.id)
		 GROUP BY t.id, la.account_type, la.wallet_id, la.asset_code`,
		tenantID, correlationID, ledger.TxCasinoBet, direction, accountTypeStrings(accountTypes),
	)
	if err != nil {
		return nil, fmt.Errorf("casino: resolve win origin rows: %w", err)
	}
	defer rows.Close()

	var out []originRow
	for rows.Next() {
		var r originRow
		var acctType string
		if err := rows.Scan(&r.BetTransactionID, &acctType, &r.WalletID, &r.AssetCode); err != nil {
			return nil, fmt.Errorf("casino: scan win origin row: %w", err)
		}
		r.AccountType = ledger.AccountType(acctType)
		out = append(out, r)
	}
	return out, rows.Err()
}

// classifyOriginRows applies §16.4's outcomes 2/3/4, shared verbatim by
// Step 1 (locked) and Step 2 (direct) result sets. rows must be
// non-empty. LF-7's wallet-collision check is applied identically to
// Step 2's result set too - a disclosed, symmetric extension of §16.4's
// own reasoning ("one account_type naming two different players' wallets
// under the same correlation_id cannot be legitimate") rather than a
// deviation from it: the document's literal text only worked the locked
// case, but nothing about the reasoning is locked-case-specific.
func classifyOriginRows(rows []originRow) (originRow, error) {
	wallets := map[uuid.UUID]bool{}
	txs := map[uuid.UUID]bool{}
	for _, r := range rows {
		wallets[r.WalletID] = true
		txs[r.BetTransactionID] = true
	}
	if len(wallets) > 1 {
		return originRow{}, ErrCorrelationWalletCollision
	}
	if len(txs) > 1 {
		return originRow{}, ErrAmbiguousMultiOriginRound
	}
	if len(rows) > 1 {
		// One bet_transaction_id, one wallet_id, yet the GROUP BY key
		// still produced more than one row - the only remaining dimension
		// that can differ is account_type: a single posting instruction
		// spanning two funding origins (outcome 4).
		return originRow{}, ErrMixedFundingUnsupported
	}
	return rows[0], nil
}

// queryNetOutstandingLocked implements §16.4 Step 1b - LF-18's fix
// (ledger-accounting-model.md §6.3.3.1 "variant 2"): the live, currently
// outstanding net over EVERY transaction sharing this correlation_id
// against this exact (account_type, wallet_id, asset_code) triple - no
// transaction_type filter, so it reflects a settlement-timeout sweep, a
// prior win, or a prior rollback the instant any of them commits. Never
// the stale, bet-time-only Step 1 figure.
func queryNetOutstandingLocked(ctx context.Context, tx pgx.Tx, tenantID, correlationID uuid.UUID, accountType ledger.AccountType, walletID uuid.UUID, assetCode string) (int64, error) {
	var net int64
	err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END), 0)
		  FROM ledger_entries      e
		  JOIN ledger_accounts     la ON la.id = e.ledger_account_id
		  JOIN ledger_transactions t  ON t.id  = e.ledger_transaction_id
		 WHERE t.tenant_id      = $1
		   AND t.correlation_id = $2
		   AND la.account_type  = $3
		   AND la.wallet_id     = $4
		   AND la.asset_code    = $5`,
		tenantID, correlationID, string(accountType), walletID, assetCode,
	).Scan(&net)
	if err != nil {
		return 0, fmt.Errorf("casino: resolve net outstanding locked amount: %w", err)
	}
	return net, nil
}

// classifyDirectOriginRows is classifyOriginRows' G-1 counterpart for the
// DIRECT (cash/bonus-debit) branch only (Stage 10.3, docs/plans/
// stage-10.3-planning/02-casino-financial-analysis.md §3 G-1; F9).
// `classifyOriginRows` treated ANY correlation id resolving more than one
// un-reversed casino_bet as ErrAmbiguousMultiOriginRound - including two
// or more plain player_cash bets on one wallet, which is a normal
// multi-debit round (side bets, multi-hand table games, feature buys,
// re-bets), not an integrity problem: postWinDirectCash never uses
// BetTransactionID, so which specific bet row this function returns has
// NO financial effect for a cash origin. LF-8's original restriction (08
// §16.4a) was scoped to bonus-funded wagering; applying it to cash was
// unjustified.
//
// Every other outcome is UNCHANGED from classifyOriginRows: a wallet
// collision is still ErrCorrelationWalletCollision, entries spanning more
// than one funding origin (account_type) are still
// ErrMixedFundingUnsupported, and any other multi-row shape (for example,
// more than one bare player_bonus debit leg with no lock - itself already
// a structural inconsistency §16.10.1 says should never exist) still
// falls back to ErrAmbiguousMultiOriginRound rather than guessing. rows
// must be non-empty (the same precondition as classifyOriginRows) - the
// caller (resolveWinOrigin) already returns ErrBetNotFound for an empty
// result set before ever reaching here.
func classifyDirectOriginRows(rows []originRow) (originRow, error) {
	wallets := map[uuid.UUID]bool{}
	accountTypes := map[ledger.AccountType]bool{}
	for _, r := range rows {
		wallets[r.WalletID] = true
		accountTypes[r.AccountType] = true
	}
	if len(wallets) > 1 {
		return originRow{}, ErrCorrelationWalletCollision
	}
	if len(accountTypes) > 1 {
		return originRow{}, ErrMixedFundingUnsupported
	}
	if len(rows) == 1 {
		return rows[0], nil
	}
	// len(rows) > 1, one wallet, one account_type: a genuine multi-bet
	// round. Safe to resolve arbitrarily (rows[0]) ONLY for the
	// player_cash account type - postWinDirectCash never dereferences
	// BetTransactionID, so no financial decision depends on which row is
	// returned. Any other account_type sharing this exact shape (e.g. two
	// un-locked player_bonus debits) stays ambiguous - §16.10.1 says a
	// bonus-funded bet must always lock, so this branch is not this
	// stage's cash-round fix and must not silently start resolving it too.
	if rows[0].AccountType == ledger.AccountPlayerCash {
		return rows[0], nil
	}
	return originRow{}, ErrAmbiguousMultiOriginRound
}

// resolveWinOrigin is §16.4 in full: Step 1 (locked identity), Step 1b
// (LF-18's outstanding-amount fix, run only once identity is
// unambiguous), Step 2 (direct-absorb fallback when no lock exists), and
// the six-outcome classification (§16.4's table). Returns the ONE
// resolved origin a win may credit, or a named, fail-closed sentinel
// error - never a guess, and never anything derived from the provider's
// own payload.
func resolveWinOrigin(ctx context.Context, tx pgx.Tx, tenantID, correlationID uuid.UUID) (winOrigin, error) {
	lockedRows, err := queryOriginRows(ctx, tx, tenantID, correlationID, ledger.Credit, lockedOriginAccountTypes)
	if err != nil {
		return winOrigin{}, err
	}
	if len(lockedRows) > 0 {
		row, err := classifyOriginRows(lockedRows)
		if err != nil {
			return winOrigin{}, err
		}
		net, err := queryNetOutstandingLocked(ctx, tx, tenantID, correlationID, row.AccountType, row.WalletID, row.AssetCode)
		if err != nil {
			return winOrigin{}, err
		}
		if net <= 0 {
			// Outcome 6 (LF-18): identity found, nothing genuinely left
			// locked. Abort - never release a second time.
			return winOrigin{}, ErrLockAlreadyReleased
		}
		return winOrigin{
			BetTransactionID: row.BetTransactionID, AccountType: row.AccountType,
			WalletID: row.WalletID, AssetCode: row.AssetCode,
			Locked: true, NetOutstandingLocked: net,
		}, nil
	}

	directRows, err := queryOriginRows(ctx, tx, tenantID, correlationID, ledger.Debit, directOriginAccountTypes)
	if err != nil {
		return winOrigin{}, err
	}
	if len(directRows) == 0 {
		return winOrigin{}, ErrBetNotFound
	}
	// G-1 (Stage 10.3): the direct/cash branch uses its OWN classifier -
	// classifyDirectOriginRows, not classifyOriginRows - because a
	// multi-bet CASH round is normal here (see its own doc comment); the
	// locked branch above is untouched and keeps classifyOriginRows'
	// stricter, unmodified behavior.
	row, err := classifyDirectOriginRows(directRows)
	if err != nil {
		return winOrigin{}, err
	}
	if row.AccountType == ledger.AccountPlayerBonus {
		// Under §16.10.1's mandated shape a bonus-funded bet must ALWAYS
		// lock - a bare player_bonus debit leg with no Step-1 lock is a
		// structural inconsistency, never a second legitimate origin.
		return winOrigin{}, ErrBonusBetNotLocked
	}
	return winOrigin{BetTransactionID: row.BetTransactionID, AccountType: row.AccountType, WalletID: row.WalletID, AssetCode: row.AssetCode}, nil
}

// --- Grant/funding lookups casino needs to reach the two named seams ---

// lookupGrantForLedgerTransaction resolves which Grant, if any, migration
// 0061's grant_ledger_attributions names for a ledger transaction - the
// mechanism a locked-bonus bet posting (once a future bonus-funded
// postBet exists, §16.10.1) attributes at bet time, and this package's
// own new postings (win-release, hold-capture) attribute themselves
// (below). At most one row is expected per transaction for casino's own
// single-Grant-per-bet shape; LIMIT 1 is a defensive bound, not a claim
// the table permits more.
func lookupGrantForLedgerTransaction(ctx context.Context, tx pgx.Tx, tenantID, ledgerTransactionID uuid.UUID) (uuid.UUID, bool, error) {
	var grantID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT grant_id FROM grant_ledger_attributions WHERE tenant_id = $1 AND ledger_transaction_id = $2 LIMIT 1`,
		tenantID, ledgerTransactionID,
	).Scan(&grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("casino: lookup grant for ledger transaction: %w", err)
	}
	return grantID, true, nil
}

// grantBonusCost mirrors internal/bonus's own unexported
// parseFundingSource contract exactly (Grant.FundingSource is
// "operator" | "provider:<id>", fixed at grant time, immutable
// thereafter) - a small, disclosed duplication so casino can build the
// ledger.BonusCostAttribution every posting touching a BONUS_SET account
// requires (bonus_mirror.go's Rule B2 generator, ledger-accounting-
// model.md §7.4.3). casino is TOLD the funding source via the Grant row,
// never asked to infer it.
func grantBonusCost(fundingSource string) (*ledger.BonusCostAttribution, error) {
	if fundingSource == "operator" {
		return &ledger.BonusCostAttribution{Funding: ledger.FundingOperator}, nil
	}
	const prefix = "provider:"
	if strings.HasPrefix(fundingSource, prefix) && len(fundingSource) > len(prefix) {
		id := fundingSource[len(prefix):]
		return &ledger.BonusCostAttribution{Funding: ledger.FundingProvider, ProviderID: &id}, nil
	}
	return nil, fmt.Errorf("casino: unrecognized grant funding_source %q", fundingSource)
}

// isGrantTerminalForG2 is §16.8 bullet 1's live-status classification:
// the statuses at which a value-creating win credit must route through
// §16.9's seam rather than crediting player_bonus directly.
// pending_settlement/expired/cancelled/forfeited are named explicitly by
// §16.8/§16.10.2 (INV-TG). GrantReversed is not discussed anywhere in
// doc 08 §16 - included here defensively (never credit a fully-reversed
// Grant's stake directly into a spendable balance) as a disclosed,
// conservative extension, not a Human Decision Register selection:
// capturing into player_bonus_held never itself creates spendable value
// (§16.16), so erring toward capture for an undiscussed status can never
// be the exploit this section closes.
func isGrantTerminalForG2(status bonus.GrantStatus) bool {
	switch status {
	case bonus.GrantPendingSettlement, bonus.GrantExpired, bonus.GrantCancelled, bonus.GrantForfeited, bonus.GrantReversed:
		return true
	default:
		return false
	}
}

// lookupTransactionProviderTxID resolves a ledger transaction's own
// provider_tx_id (nil for a system-initiated transaction, e.g. one with
// no provider_id/provider_tx_id pair at all).
func lookupTransactionProviderTxID(ctx context.Context, tx pgx.Tx, tenantID, ledgerTransactionID uuid.UUID) (*string, error) {
	var providerTxID *string
	err := tx.QueryRow(ctx,
		`SELECT provider_tx_id FROM ledger_transactions WHERE tenant_id = $1 AND id = $2`,
		tenantID, ledgerTransactionID,
	).Scan(&providerTxID)
	if err != nil {
		return nil, fmt.Errorf("casino: lookup transaction provider_tx_id: %w", err)
	}
	return providerTxID, nil
}

func bigIntToInt64(v *big.Int) (int64, error) {
	if v == nil {
		return 0, nil
	}
	if !v.IsInt64() {
		return 0, fmt.Errorf("casino: amount %s does not fit in int64 minor units", v.String())
	}
	n := v.Int64()
	if n < 0 {
		return 0, fmt.Errorf("casino: amount must not be negative, got %s", v.String())
	}
	return n, nil
}

// --- postWin's per-origin posting branches ---

// postWinDirectCash is today's UNCHANGED behavior (§16.4's Step-2/
// player_cash row): Dr house_gaming W / Cr player_cash W. Extracted
// verbatim from the pre-Phase-7 postWin body so the pure-cash path (the
// only reachable path in production today, ADR 0025 §6) is provably
// byte-for-byte unaffected by this dispatch.
func (o *Orchestrator) postWinDirectCash(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent, origin winOrigin, correlationID uuid.UUID) (ReceiveCallbackResult, error) {
	// ADR 0082 §4.3: no LOCKING change is needed here - this path posts
	// through ledger.Post and takes no projection lock of its own, so
	// Post's internal L3 pre-lock now orders it. Only the account
	// resolution moves to GetOrCreateAccounts, for canonical
	// ledger_accounts creation order.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: &origin.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: event.AssetCode},
		ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: event.AssetCode},
	)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve direct-cash win ledger accounts: %w", err)
	}
	cashAccountID, houseAccountID := accounts[0], accounts[1]

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxCasinoWin,
		IdempotencyKey: providerID + ":" + event.ProviderTxID,
		ProviderID:     &providerID, ProviderTxID: &event.ProviderTxID,
		CorrelationID: correlationID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: houseAccountID, Direction: ledger.Debit, Amount: event.Amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: event.Amount},
		},
	})
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: post win: %w", err)
	}

	// gate 10.3-W1 QA condition 1 / ledger-finance C4 (discovered during
	// that condition's own test-writing): a redelivery of an
	// ALREADY-POSTED win (sequential, or a concurrent delivery that lost
	// the L0.1 race and only then re-reached ledger.Post, which is itself
	// correctly idempotent) must NOT write a second audit row - exactly
	// postBet's own established convention, where the idempotency
	// short-circuit at the top of the function never reaches its audit
	// call a second time. Before this guard, N redeliveries/concurrent
	// deliveries of one win produced N `casino_win.posted` rows despite
	// posting exactly once, an audit-log-bloat defect with no financial
	// effect (the ledger stayed correctly idempotent throughout).
	if !postResult.AlreadyPosted {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_win.posted",
			TargetType: "ledger_transaction", TargetID: postResult.TransactionID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": providerID, "provider_tx_id": event.ProviderTxID, "round_id": event.RoundID,
				"amount": event.Amount, "asset_code": event.AssetCode, "already_posted": postResult.AlreadyPosted,
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: audit win posted: %w", err)
		}
	}

	return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID}, nil
}

// postWinLockedCash is §16.4's destination-map row for a
// player_locked_cash origin: the win payout credits player_cash, and the
// same posting bundles the lock release (Dr player_locked_cash X / Cr
// player_cash X). No Grant dimension exists for a cash lock - never G-2.
func (o *Orchestrator) postWinLockedCash(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent, origin winOrigin, correlationID uuid.UUID) (ReceiveCallbackResult, error) {
	// ADR 0082 §4.3: account resolution only - no locking change (this
	// path posts through ledger.Post, whose internal L3 pre-lock now
	// orders it).
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: &origin.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: event.AssetCode},
		ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: event.AssetCode},
		ledger.AccountSpec{WalletID: &origin.WalletID, AccountType: ledger.AccountPlayerLockedCash, AssetCode: event.AssetCode},
	)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve locked-cash win ledger accounts: %w", err)
	}
	cashAccountID, houseAccountID, lockedCashAccountID := accounts[0], accounts[1], accounts[2]

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxCasinoWin,
		IdempotencyKey: providerID + ":" + event.ProviderTxID,
		ProviderID:     &providerID, ProviderTxID: &event.ProviderTxID,
		CorrelationID: correlationID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: houseAccountID, Direction: ledger.Debit, Amount: event.Amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: event.Amount},
			{LedgerAccountID: lockedCashAccountID, Direction: ledger.Debit, Amount: origin.NetOutstandingLocked},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: origin.NetOutstandingLocked},
		},
	})
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: post win (locked cash release): %w", err)
	}

	// See postWinDirectCash's identical guard/comment (gate 10.3-W1
	// condition C4): never audit a redelivery of an already-posted win a
	// second time.
	if !postResult.AlreadyPosted {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_win.posted",
			TargetType: "ledger_transaction", TargetID: postResult.TransactionID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": providerID, "provider_tx_id": event.ProviderTxID, "round_id": event.RoundID,
				"amount": event.Amount, "asset_code": event.AssetCode, "already_posted": postResult.AlreadyPosted,
				"origin_account_type": string(origin.AccountType), "released_lock_amount": origin.NetOutstandingLocked,
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: audit win posted: %w", err)
		}
	}

	return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID}, nil
}

// postWinLockedBonus is §16.4's destination-map rows for a
// player_locked_bonus origin: resolve the Grant that funded the lock
// (grant_ledger_attributions), take the live status read under doc10
// §9's advisory lock, and branch exactly as §16.5/§16.7/§16.9 specify -
// ordinary crediting for a non-terminal Grant, or the unconditional
// two-leg hold-capture posting (§16.14) plus ResolveTerminalGrantCredit
// (§16.9) for a terminal one. No disposition among ACTION_REFORFEIT/
// ACTION_ROUTE_TO_CASH/ACTION_HOLD_FOR_REVIEW is decided anywhere in this
// function (§16.9's own corrected framing) - capture is technical,
// unconditional, decided before any of the three is known.
func (o *Orchestrator) postWinLockedBonus(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent, origin winOrigin, correlationID uuid.UUID) (ReceiveCallbackResult, error) {
	grantID, found, err := lookupGrantForLedgerTransaction(ctx, tx, tenantID, origin.BetTransactionID)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if !found {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: bet_transaction=%s", ErrLockedBonusGrantMissing, origin.BetTransactionID)
	}

	// doc10 §9's (tenant_id, grant_id) advisory lock, acquired by casino
	// BEFORE its own live status read (§16.9) - the identical lock
	// ResolveTerminalGrantCredit itself re-acquires further down
	// (pg_advisory_xact_lock is reentrant within one session/transaction,
	// §16.9's own "no new lock participant" finding).
	if err := bonus.AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		return ReceiveCallbackResult{}, err
	}
	grant, err := bonus.GetGrantByID(ctx, tx, grantID)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: load grant for locked-bonus win: %w", err)
	}
	if grant.TenantID != tenantID {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: grant %s does not belong to this tenant", grantID)
	}
	bonusCost, err := grantBonusCost(grant.FundingSource)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}

	// ADR 0082 §4.3: account resolution only - neither branch takes a
	// projection lock of its own, so both are ordered by ledger.Post's
	// own internal L3 pre-lock, which (unlike anything this file could
	// do) also covers the Rule B2 mirror/recognition legs Post generates
	// for these BONUS_SET postings. Each branch resolves its COMPLETE
	// account set in one GetOrCreateAccounts call so ledger_accounts
	// creation is canonically ordered across all three, not just the two
	// that used to be resolved before the branch.
	walletID := origin.WalletID

	if !isGrantTerminalForG2(grant.Status) {
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: event.AssetCode},
			ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerLockedBonus, AssetCode: event.AssetCode},
			ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerBonus, AssetCode: event.AssetCode},
		)
		if err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve locked-bonus win ledger accounts: %w", err)
		}
		houseAccountID, lockedBonusAccountID, playerBonusAccountID := accounts[0], accounts[1], accounts[2]

		postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: tenantID, TransactionType: ledger.TxCasinoWin,
			IdempotencyKey: providerID + ":" + event.ProviderTxID,
			ProviderID:     &providerID, ProviderTxID: &event.ProviderTxID,
			CorrelationID: correlationID,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: houseAccountID, Direction: ledger.Debit, Amount: event.Amount},
				{LedgerAccountID: playerBonusAccountID, Direction: ledger.Credit, Amount: event.Amount},
				{LedgerAccountID: lockedBonusAccountID, Direction: ledger.Debit, Amount: origin.NetOutstandingLocked},
				{LedgerAccountID: playerBonusAccountID, Direction: ledger.Credit, Amount: origin.NetOutstandingLocked},
			},
			BonusCost: bonusCost,
		})
		if err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: post win (locked bonus, non-terminal grant): %w", err)
		}
		if err := bonus.AttributeGrantLedgerTransactionIdempotent(ctx, tx, tenantID, grantID, postResult.TransactionID, string(ledger.TxCasinoWin)); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: attribute win to grant: %w", err)
		}

		// See postWinDirectCash's identical guard/comment (gate 10.3-W1
		// condition C4): never audit a redelivery of an already-posted win
		// a second time.
		if !postResult.AlreadyPosted {
			if err := audit.Record(ctx, tx, audit.Entry{
				TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_win.posted",
				TargetType: "ledger_transaction", TargetID: postResult.TransactionID.String(), Outcome: audit.OutcomeSuccess,
				Metadata: map[string]any{
					"provider_id": providerID, "provider_tx_id": event.ProviderTxID, "round_id": event.RoundID,
					"amount": event.Amount, "asset_code": event.AssetCode, "already_posted": postResult.AlreadyPosted,
					"origin_account_type": string(origin.AccountType), "released_lock_amount": origin.NetOutstandingLocked,
					"grant_id": grantID.String(), "grant_status": string(grant.Status),
				},
			}); err != nil {
				return ReceiveCallbackResult{}, fmt.Errorf("casino: audit win posted: %w", err)
			}
		}

		return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID}, nil
	}

	// Terminal/pending_settlement (§16.5/§16.7 sub-branch 2, §16.9's
	// corrected call order, §16.14's exact two-leg posting): capture is
	// UNCONDITIONAL, decided before any of G-2's three eventual actions is
	// known.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: event.AssetCode},
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerLockedBonus, AssetCode: event.AssetCode},
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerBonusHeld, AssetCode: event.AssetCode},
	)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve terminal hold-capture ledger accounts: %w", err)
	}
	houseAccountID, lockedBonusAccountID, playerBonusHeldAccountID := accounts[0], accounts[1], accounts[2]

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxCasinoWin,
		IdempotencyKey: providerID + ":" + event.ProviderTxID,
		ProviderID:     &providerID, ProviderTxID: &event.ProviderTxID,
		CorrelationID: correlationID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: houseAccountID, Direction: ledger.Debit, Amount: event.Amount},
			{LedgerAccountID: playerBonusHeldAccountID, Direction: ledger.Credit, Amount: event.Amount},
			{LedgerAccountID: lockedBonusAccountID, Direction: ledger.Debit, Amount: origin.NetOutstandingLocked},
			{LedgerAccountID: playerBonusHeldAccountID, Direction: ledger.Credit, Amount: origin.NetOutstandingLocked},
		},
		BonusCost: bonusCost,
	})
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: post win (terminal-grant hold-capture): %w", err)
	}

	heldDispositionID, err := bonus.ResolveTerminalGrantCredit(ctx, tx, grantID, correlationID, bonus.CreditKindWin,
		big.NewInt(event.Amount), big.NewInt(origin.NetOutstandingLocked), postResult.TransactionID)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve terminal grant credit: %w", err)
	}

	// See postWinDirectCash's identical guard/comment (gate 10.3-W1
	// condition C4): never audit a redelivery of an already-posted/
	// already-captured win a second time. ResolveTerminalGrantCredit
	// itself is still called unconditionally above (its own idempotency,
	// not this package's, and not touched by this cleanup).
	if !postResult.AlreadyPosted {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_win.captured_pending_g2",
			TargetType: "ledger_transaction", TargetID: postResult.TransactionID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": providerID, "provider_tx_id": event.ProviderTxID, "round_id": event.RoundID,
				"amount": event.Amount, "asset_code": event.AssetCode, "already_posted": postResult.AlreadyPosted,
				"released_lock_amount": origin.NetOutstandingLocked, "grant_id": grantID.String(),
				"grant_status": string(grant.Status), "held_disposition_id": heldDispositionID.String(),
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: audit win captured: %w", err)
		}
	}

	return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID}, nil
}

// --- postRollback's two new §16.21 call sites ---

// recheckGrantAfterBonusTouchingRollback is §16.21 site 1: after posting
// the plain, generic rollback of a bet whose lock touched
// player_bonus/player_locked_bonus, recheck the attributed Grant's
// exposure. A no-op (nil error, no lock taken) when the original
// transaction was never Grant-attributed (an ordinary cash-funded bet -
// today's only reachable production shape).
func recheckGrantAfterBonusTouchingRollback(ctx context.Context, tx pgx.Tx, tenantID, grantID, reversalTransactionID uuid.UUID) error {
	if err := bonus.AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		return err
	}
	if _, err := bonus.RecheckGrantExposure(ctx, tx, tenantID, grantID, reversalTransactionID, bonus.TriggerCasinoRollback); err != nil {
		return fmt.Errorf("casino: recheck grant exposure after rollback: %w", err)
	}
	return nil
}

// postRollbackHeldWin is §16.15 (adopting ledger-finance §7.7.2.7): a
// rollback naming a hold-capture win transaction still parked in
// bonus_held_dispositions. handled=false means no disposition row exists
// for originalID at all - the caller falls through to the ordinary
// generic-inversion path, unaffected.
//
// LF-10 (§16.20, still open, ledger-finance's decision): if the
// disposition has already moved past 'held' (resolved_reforfeit,
// resolved_route_to_cash, or - a distinct, genuinely-new rollback
// reference racing an earlier one - voided_by_rollback already), this
// function fails closed with a named sentinel error and posts nothing -
// it never silently reuses this held-shape mechanism for a structurally
// different, already-resolved case (§16.15's own explicit boundary).
func (o *Orchestrator) postRollbackHeldWin(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent, originalID uuid.UUID) (handled bool, result ReceiveCallbackResult, err error) {
	disposition, err := bonus.GetHeldDispositionBySettlementTransaction(ctx, tx, tenantID, originalID)
	if errors.Is(err, bonus.ErrNotFound) {
		return false, ReceiveCallbackResult{}, nil
	}
	if err != nil {
		return true, ReceiveCallbackResult{}, fmt.Errorf("casino: look up held disposition for rollback: %w", err)
	}

	// HR-25 lock order (ledger-accounting-model.md §7.7.2.9): (1) the
	// (tenant_id, grant_id) advisory lock, THEN (2) the disposition row's
	// own FOR UPDATE lock.
	if err := bonus.AdvisoryLockGrant(ctx, tx, tenantID, disposition.GrantID); err != nil {
		return true, ReceiveCallbackResult{}, err
	}
	disposition, err = bonus.LockHeldDispositionForUpdate(ctx, tx, disposition.ID)
	if err != nil {
		return true, ReceiveCallbackResult{}, fmt.Errorf("casino: lock held disposition for update: %w", err)
	}

	switch disposition.Status {
	case bonus.HeldDispositionHeld:
		// proceeds below.
	case bonus.HeldDispositionVoidedByRollback:
		// A REDELIVERY of the exact rollback that already voided this
		// record (same provider_tx_id) is an idempotent no-op - it must
		// return the same success result, never an error (§16.15's own
		// "duplicate rollback of the same held record" proof: "the
		// redelivered rollback's own reversal transaction hits
		// ledger.Post's idempotency key first"). This check exists because
		// THIS function returns before ever reaching ledger.Post's own
		// idempotency key, unlike the ordinary generic-inversion path.
		// Only a GENUINELY DISTINCT new rollback reference naming an
		// already-voided record (the race-loser case, §16.15's third
		// proof bullet) is rejected.
		if disposition.ResolutionLedgerTransactionID != nil {
			resolutionProviderTxID, err := lookupTransactionProviderTxID(ctx, tx, tenantID, *disposition.ResolutionLedgerTransactionID)
			if err != nil {
				return true, ReceiveCallbackResult{}, err
			}
			if resolutionProviderTxID != nil && *resolutionProviderTxID == event.ProviderTxID {
				resolutionTxID := *disposition.ResolutionLedgerTransactionID
				return true, ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &resolutionTxID}, nil
			}
		}
		return true, ReceiveCallbackResult{}, fmt.Errorf("%w: disposition=%s", ErrHeldDispositionAlreadyVoided, disposition.ID)
	case bonus.HeldDispositionResolvedReforfeit, bonus.HeldDispositionResolvedRouteToCash:
		return true, ReceiveCallbackResult{}, fmt.Errorf("%w: disposition=%s status=%s", ErrHeldDispositionRollbackUnsupported, disposition.ID, disposition.Status)
	default:
		return true, ReceiveCallbackResult{}, fmt.Errorf("casino: held disposition %s has unrecognized status %q", disposition.ID, disposition.Status)
	}

	grant, err := bonus.GetGrantByID(ctx, tx, disposition.GrantID)
	if err != nil {
		return true, ReceiveCallbackResult{}, fmt.Errorf("casino: load grant for held-win rollback: %w", err)
	}
	bonusCost, err := grantBonusCost(grant.FundingSource)
	if err != nil {
		return true, ReceiveCallbackResult{}, err
	}

	// ADR 0082 §4.3: account resolution only - no locking change (this
	// path posts through ledger.Post, whose internal L3 pre-lock now
	// orders it, generated mirror legs included).
	walletID := disposition.WalletID
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: disposition.AssetCode},
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerBonusHeld, AssetCode: disposition.AssetCode},
	)
	if err != nil {
		return true, ReceiveCallbackResult{}, fmt.Errorf("casino: resolve held-win rollback ledger accounts: %w", err)
	}
	houseAccountID, playerBonusHeldAccountID := accounts[0], accounts[1]

	payoutAmount, err := bigIntToInt64(disposition.PayoutAmount)
	if err != nil {
		return true, ReceiveCallbackResult{}, err
	}
	releasedLockAmount, err := bigIntToInt64(disposition.ReleasedLockAmount)
	if err != nil {
		return true, ReceiveCallbackResult{}, err
	}

	// §16.15/§16.18 Part B step 3, walked and doubly-confirmed there (see
	// this dispatch's own report for the drafting inconsistency this
	// resolves against §16.15's own prose): BOTH legs reverse straight to
	// house_gaming. Restoring player_locked_bonus is deliberately NEVER
	// attempted - L(G) already closed to zero at hold-capture time.
	entries := make([]ledger.EntryInput, 0, 4)
	if payoutAmount > 0 {
		entries = append(entries,
			ledger.EntryInput{LedgerAccountID: playerBonusHeldAccountID, Direction: ledger.Debit, Amount: payoutAmount},
			ledger.EntryInput{LedgerAccountID: houseAccountID, Direction: ledger.Credit, Amount: payoutAmount},
		)
	}
	if releasedLockAmount > 0 {
		entries = append(entries,
			ledger.EntryInput{LedgerAccountID: playerBonusHeldAccountID, Direction: ledger.Debit, Amount: releasedLockAmount},
			ledger.EntryInput{LedgerAccountID: houseAccountID, Direction: ledger.Credit, Amount: releasedLockAmount},
		)
	}
	if len(entries) == 0 {
		return true, ReceiveCallbackResult{}, fmt.Errorf("casino: held disposition %s has no positive payout or released-lock amount to reverse", disposition.ID)
	}

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxCasinoRollback,
		IdempotencyKey:        providerID + ":" + event.ProviderTxID,
		ProviderID:            &providerID,
		ProviderTxID:          &event.ProviderTxID,
		CorrelationID:         disposition.CorrelationID,
		ReversesTransactionID: &originalID,
		Entries:               entries,
		BonusCost:             bonusCost,
	})
	if err != nil {
		return true, ReceiveCallbackResult{}, fmt.Errorf("casino: post held-win rollback: %w", err)
	}

	if !postResult.AlreadyPosted {
		// Attribute the reversal itself to the Grant BEFORE rechecking
		// exposure - ComputeAOE's Component 3 (heldDispositionExposure,
		// aoe.go) is a LIVE read of player_bonus_held summed ONLY over
		// entries attributed via grant_ledger_attributions. The original
		// hold-capture posting was attributed by ResolveTerminalGrantCredit
		// itself (held_disposition_ops.go); without attributing this
		// reversal too, that component would never net back to zero and
		// RecheckGrantExposure could never observe AOE empty for this
		// disposition's own contribution.
		if err := bonus.AttributeGrantLedgerTransactionIdempotent(ctx, tx, tenantID, disposition.GrantID, postResult.TransactionID, string(ledger.TxCasinoRollback)); err != nil {
			return true, ReceiveCallbackResult{}, fmt.Errorf("casino: attribute held-win rollback to grant: %w", err)
		}
		now := time.Now().UTC()
		if _, err := bonus.ResolveHeldDisposition(ctx, tx, tenantID, disposition.ID, bonus.HeldDispositionVoidedByRollback, nil, nil, postResult.TransactionID, now); err != nil {
			return true, ReceiveCallbackResult{}, fmt.Errorf("casino: void held disposition on rollback: %w", err)
		}
		if _, err := bonus.RecheckGrantExposure(ctx, tx, tenantID, disposition.GrantID, postResult.TransactionID, bonus.TriggerHeldDispositionResolved); err != nil {
			return true, ReceiveCallbackResult{}, fmt.Errorf("casino: recheck grant exposure after held-win rollback: %w", err)
		}
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_win.rolled_back",
		TargetType: "ledger_transaction", TargetID: postResult.TransactionID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"provider_id": providerID, "rollback_provider_tx_id": event.ProviderTxID,
			"original_provider_tx_id": event.OriginalProviderTxID, "original_transaction_id": originalID.String(),
			"already_posted": postResult.AlreadyPosted, "held_disposition_id": disposition.ID.String(), "grant_id": disposition.GrantID.String(),
		},
	}); err != nil {
		return true, ReceiveCallbackResult{}, fmt.Errorf("casino: audit held-win rollback: %w", err)
	}

	return true, ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID}, nil
}
