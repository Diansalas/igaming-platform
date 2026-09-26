// Package ledger implements the platform's append-only, double-entry
// posting engine - the sole authoritative source of financial truth
// (CLAUDE.md, ADR 0001, docs/architecture/ledger-accounting-model.md).
//
// This package owns LedgerAccount/LedgerTransaction/LedgerEntry and the
// Post() posting API. No other package writes to ledger_accounts,
// ledger_transactions, or ledger_entries directly - "every service that
// moves money must go through the wallet/ledger's own API" (ADR 0001).
//
// Money amounts are represented in Go as int64 minor units. This is a
// deliberate implementation choice, not a re-litigation of ADR 0001/0021:
// the database column remains NUMERIC(38,0) for headroom far beyond any
// realistic value (int64 already covers roughly 9.2*10^18 minor units -
// for BTC at 8 decimals alone that is about 9.2*10^10 whole bitcoin,
// several orders of magnitude beyond any value this platform will ever
// hold), and Go has no built-in fixed-point type that would be simpler
// than int64 without pulling in math/big for no realistic benefit. See
// bonus_mirror.go's own doc comment (finding LF-16b) for the one place
// this representation is known to fall short - an 18-exponent asset - and
// why that is disclosed rather than fixed here.
//
// Invariant B1 (bonus mirror, ledger-accounting-model.md §6.1/§7.4) is
// enforced by this package, not by callers: Post invokes bonus_mirror.go's
// Rule B2 (extended) generator unconditionally on every call, so any
// posting touching a BONUS_SET account (player_bonus, player_locked_bonus,
// player_bonus_held) is mirrored automatically. HR-9's fail-closed guard,
// which used to block every such posting until bonus_expense (migration
// 0050) and this generator both existed, is REMOVED as of this same
// dispatch - both preconditions are now met - per §7.4.4's instruction
// that the removal be total, not a feature flag.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// AccountType names one of the ledger account types this platform posts
// to. The Blueprint names ten; player_withdrawal_hold is a Stage 3A
// architectural addition; player_locked_cash/player_locked_bonus replace
// the Blueprint's origin-indeterminate player_locked (migration 0048,
// ledger-accounting-model.md §6.3/§6.4/§6.5, invariant L1); bonus_expense
// (ADR 0032 §2, migration 0050) and player_bonus_held (§7.7.2.4,
// migration 0052) are Stage 4H-B1 Wave 2 additions - the twelfth and
// fourteenth account types, both now migrated. No count is stated here:
// the previous comment's count was already stale and would go stale
// again.
type AccountType string

const (
	AccountPlayerCash  AccountType = "player_cash"
	AccountPlayerBonus AccountType = "player_bonus"
	// The locked-funds family (invariant L1, ledger-accounting-model.md
	// §6.5.4): value held pending the resolution of a wagering event,
	// with its origin determinable from the account_type alone. Bare
	// player_locked is deliberately absent - it is not in migration
	// 0048's CHECK constraint, and its absence here is what makes every
	// stale read-side enumeration a compile error rather than a silent
	// zero (§6.5.3). Membership is explicit and named, never inferred
	// from the player_locked_ prefix.
	AccountPlayerLockedCash     AccountType = "player_locked_cash"
	AccountPlayerLockedBonus    AccountType = "player_locked_bonus"
	AccountPlayerWithdrawalHold AccountType = "player_withdrawal_hold"
	AccountHouseGaming          AccountType = "house_gaming"
	AccountProviderPayable      AccountType = "provider_payable"
	AccountPSPClearing          AccountType = "psp_clearing"
	AccountPSPReserve           AccountType = "psp_reserve"
	AccountJackpotContribution  AccountType = "jackpot_contribution"
	AccountPromoLiability       AccountType = "promo_liability"
	AccountManualAdjustment     AccountType = "manual_adjustment"
	// AccountBonusExpense is ADR 0032 §2's recognized promotional-cost
	// account (migration 0050, ledger-accounting-model.md §7.2):
	// house-level, debit-normal, debited by the Rule B2 (extended) mirror
	// generator's step 3 (bonus_mirror.go) whenever bonus-origin value
	// leaves BONUS_SET for any reason other than forfeiture and the
	// recognition is operator-funded. Never posted to directly by a
	// caller - HR-17 forbids it.
	AccountBonusExpense AccountType = "bonus_expense"
	// AccountPlayerBonusHeld is the third BONUS_SET member
	// (ledger-accounting-model.md §7.7.2, migration 0052): a dedicated,
	// disjoint, player-owned holding account for bonus-origin settlement
	// value (a win payout, and/or a released stake lock) whose
	// disposition is undecided pending gate G-2. Deliberately NOT part of
	// the locked-funds family above - "held" and "locked" are kept
	// disjoint at every read site that enumerates one or the other
	// (§7.7.2.4's explicit, permanent exclusion) - and NOT reachable from
	// any posting this dispatch builds: only casino's future hold-capture
	// posting (§7.7.2.2, not this dispatch's scope) credits it.
	AccountPlayerBonusHeld AccountType = "player_bonus_held"
)

// TransactionType is limited to the flows implemented so far
// (deposit/withdrawal/manual_adjustment/tombstone, Stage 4A's casino
// bet/win/rollback, and Stage 4H-B1's four bonus types) - see migration
// 0021's own comment. Sportsbook/crypto types remain added by an
// additive migration + a new const here when their owning stage
// implements them; they are explicitly BLOCKED until then (CLAUDE.md's
// stage scope gate).
type TransactionType string

const (
	TxDeposit             TransactionType = "deposit"
	TxDepositReversal     TransactionType = "deposit_reversal"
	TxWithdrawalRequested TransactionType = "withdrawal_requested"
	TxWithdrawalCompleted TransactionType = "withdrawal_completed"
	TxWithdrawalRejected  TransactionType = "withdrawal_rejected"
	TxWithdrawalFailed    TransactionType = "withdrawal_failed"
	TxWithdrawalReversed  TransactionType = "withdrawal_reversed"
	TxManualAdjustment    TransactionType = "manual_adjustment"
	TxTombstone           TransactionType = "tombstone"
	// TxCasinoBet/Win/Rollback implement financial-transaction-flows.md
	// Flows 5-7 (Stage 4A, migration 0035's additive CHECK-constraint
	// values). See internal/casino's own package doc comment for the
	// owning package.
	TxCasinoBet      TransactionType = "casino_bet"
	TxCasinoWin      TransactionType = "casino_win"
	TxCasinoRollback TransactionType = "casino_rollback"
	// TxBonusGrant/Conversion/Forfeiture/Reversal implement ADR 0032's
	// bonus posting shapes (migration 0051, ledger-accounting-model.md
	// §7.3). TxBonusForfeiture covers expiry AND cancellation-after-
	// activation too - ADR 0032 §3.1 is binding that these are
	// distinguished from each other and from an ordinary forfeiture only
	// by reason_code (required on this type, migration 0051's widened
	// ledger_transactions_check1), never by a separate type. Every
	// posting of any of these four touching a BONUS_SET account is
	// mirrored automatically by bonus_mirror.go's Rule B2 (extended)
	// generator; none of the four is itself special-cased there (ADR
	// 0032 §2: "Rule B2 admits no exception by transaction type").
	TxBonusGrant      TransactionType = "bonus_grant"
	TxBonusConversion TransactionType = "bonus_conversion"
	TxBonusForfeiture TransactionType = "bonus_forfeiture"
	TxBonusReversal   TransactionType = "bonus_reversal"
	// TxSportsbookBet implements docs/decisions/0038 §3's cash-funded bet-
	// placement posting (Stage 6, migration 0078's additive CHECK-
	// constraint value): Dr player_cash / Cr player_locked_cash, no
	// house_gaming leg at placement time (the stake is not recognized as
	// revenue until settlement).
	TxSportsbookBet TransactionType = "sportsbook_bet"
	// TxSportsbookSettlement/Void/Rollback implement ADR 0038 §5/§8.1/§10
	// for cash-funded singles in in-house mode (Stage 10 W1, ADR 0088,
	// migration 0091): settlement Dr player_locked_cash S / Cr house_gaming
	// S (+ Dr house_gaming P / Cr player_cash P when won); void Dr
	// player_locked_cash S / Cr player_cash S; rollback the exact inverse of
	// a settlement. Written only by internal/sportsbook's settlement
	// functions under the bet's L1 row lock (INV-LOCK-E4). Partial
	// settlement and cashout remain NOT IMPLEMENTED.
	TxSportsbookSettlement TransactionType = "sportsbook_settlement"
	TxSportsbookVoid       TransactionType = "sportsbook_void"
	TxSportsbookRollback   TransactionType = "sportsbook_rollback"
)

// Direction is a ledger entry's debit/credit side. Never a signed amount
// column - see ledger-accounting-model.md §1.3.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// ErrIdempotencyKeyReused is returned when a Post call's idempotency_key
// matches an existing transaction whose transaction_type differs from the
// one now requested. It covers a differing TYPE only; every other
// payload difference under the same key (entries, reversal link,
// provider reference, reason code, causation, correlation) is
// ErrIdempotencyPayloadMismatch (ADR 0020 amendment 2026-09-25, Stage 10
// F-7). The type check runs first and keeps this sentinel, because
// callers map it specifically - ADR 0088 §4.7 maps it (and
// ErrIdempotencyPayloadMismatch) to sportsbook's own ErrSettlementIntegrity
// as the ledger-level backstop for the settlement lifecycle; there is no
// ErrSettlementTombstoned type. The sportsbook decision table's
// SETTLEMENT_TOMBSTONED rejection code (ADR 0088 §4.3) is a separate,
// earlier-evaluated result, not a mapping of this error. The two ledger
// sentinels are deliberately distinct and must not be merged.
var ErrIdempotencyKeyReused = errors.New("ledger: idempotency key reused with a different transaction type")

// ErrIdempotencyPayloadMismatch is returned when a Post call's
// idempotency_key matches an existing transaction of the SAME
// transaction_type whose canonical payload differs from the request: the
// final entry multiset (caller entries plus Rule B2 mirror/recognition
// legs, compared order-insensitively as (ledger_account_id, direction,
// amount)), reverses_transaction_id, provider_id/provider_tx_id,
// reason_code, causation_id, or correlation_id (correlation is not
// compared for TxTombstone - see replay.go). Nothing new is posted. The
// wrapped detail names the existing transaction id and the differing
// field classes only - never amounts, accounts or references.
//
// Before Stage 10 F-7 such a call silently returned the ORIGINAL
// transaction with AlreadyPosted = true (docs/governance/
// stage-10-f7-ledger-replay-audit.md §1). It is an integrity failure,
// never a retry signal: a caller that receives it must not treat the
// operation as done, and must not retry with the same key.
var ErrIdempotencyPayloadMismatch = errors.New("ledger: idempotency key reused with a different payload")

// reversalOneDepositReversalConstraint is the name of migration 0092's
// partial unique index (tenant_id, reverses_transaction_id) WHERE
// transaction_type = 'deposit_reversal' - INV-PAY-REV-1
// (docs/plans/stage-10.1-planning-gate-proposal.md §F): at most one
// deposit_reversal transaction per original deposit per tenant.
const reversalOneDepositReversalConstraint = "ledger_transactions_one_deposit_reversal"

// ErrReversalAlreadyExists is returned by Post when the INSERT into
// ledger_transactions violates the migration-0092 partial unique index
// rather than the ordinary (tenant_id, idempotency_key) idempotency
// constraint. Stage 10.1 PAY-REV-1 (ADR 0090): internal/payments'
// deposit-reversal callback path takes an ADR 0082 class-L2 lock on the
// original deposit's ledger_transactions row and re-checks "already
// reversed?" AFTER acquiring it (the primary control, docs/plans/
// stage-10.1-planning-gate-proposal.md §E); this index is the BACKSTOP
// against a future writer that skips that lock. Routing on the
// constraint name (rather than treating this like any other unique
// violation) matters because db.IdempotentInsert's ordinary conflict path
// would otherwise look this INSERT up by idempotency_key and find NO
// ROW - the reversal's own idempotency key was never actually written -
// which used to surface as a confusing "conflict but nothing found"
// internal error instead of the true cause. ledger-finance pre-approved
// exactly this change (Stage 10.1 review P2-1).
var ErrReversalAlreadyExists = errors.New("ledger: a deposit_reversal transaction already exists for this original deposit")

// ErrInvalidEntry is returned for a structurally invalid entry (e.g. a
// non-positive amount) caught before ever reaching the database.
var ErrInvalidEntry = errors.New("ledger: invalid entry")

// EntryInput is one leg of a transaction to post.
type EntryInput struct {
	LedgerAccountID uuid.UUID
	Direction       Direction
	// Amount is strictly positive minor units - CHECK (amount > 0) at the
	// database level too (ledger-accounting-model.md §1.3).
	Amount int64
}

// TransactionInput is everything Post needs to post one atomic,
// idempotent, balanced financial fact.
type TransactionInput struct {
	TenantID              uuid.UUID
	TransactionType       TransactionType
	IdempotencyKey        string
	ProviderID            *string
	ProviderTxID          *string
	CorrelationID         uuid.UUID
	CausationID           *uuid.UUID
	ReversesTransactionID *uuid.UUID
	ReasonCode            *string
	// Entries is empty ONLY for TxTombstone (a rollback of a transaction
	// never seen posts no money movement - CLAUDE.md's rollback rule,
	// ledger-accounting-model.md §1.4).
	Entries []EntryInput
	// BonusCost is required if, and only if, Entries touches a BONUS_SET
	// account (player_bonus, player_locked_bonus, player_bonus_held) -
	// bonus_mirror.go's Rule B2 (extended) generator validates this
	// fail-closed, with NO default (ledger-accounting-model.md §7.4.3).
	// Nil for every non-bonus posting (deposit, withdrawal, casino,
	// cash-only manual_adjustment, ...).
	BonusCost *BonusCostAttribution
}

// PostResult is what Post returns.
type PostResult struct {
	TransactionID uuid.UUID
	// AlreadyPosted is true when this call was an idempotent no-op: a
	// transaction with the same (tenant_id, idempotency_key) already
	// existed, so nothing new was posted and TransactionID identifies the
	// ORIGINAL transaction. Since the Stage 10 F-7 remediation it is only
	// ever true when the original's canonical payload equals this
	// request's (see Post and ErrIdempotencyPayloadMismatch).
	AlreadyPosted bool
}

// Post posts one LedgerTransaction and its entries atomically, inside
// tx - the caller's own already-open, correctly tenant-scoped
// transaction (via db.Pool.WithTenant), exactly like internal/audit's
// Record function. Post never opens its own top-level transaction and
// never commits tx itself.
//
// Idempotency (ADR 0020, as amended 2026-09-25 by Stage 10 F-7): a call
// whose (tenant_id, idempotency_key) already exists is resolved via the
// SAVEPOINT pattern in db.IdempotentInsert and never double-posts. It
// then returns exactly one of:
//
//   - ErrIdempotencyKeyReused if the stored transaction_type differs
//     (checked first; ADR 0088 §4.5 depends on this sentinel);
//   - ErrIdempotencyPayloadMismatch if the type matches but the canonical
//     payload differs - the final entry multiset (after Rule B2 legs,
//     order-insensitive), reverses_transaction_id, provider_id/
//     provider_tx_id, reason_code, causation_id or correlation_id
//     (correlation exempt for TxTombstone; replay.go);
//   - PostResult{TransactionID: original, AlreadyPosted: true} only when
//     every one of those is equal - so AlreadyPosted now proves the
//     request describes the very fact already on the ledger.
//
// A caller whose legitimate retries can differ in any compared field
// (e.g. a per-attempt correlation id) must make it deterministic or
// resolve the retry before calling Post. A unique violation on the
// (tenant_id, provider_id, provider_tx_id) index alone (a NEW key whose
// provider reference is already taken, e.g. by a tombstone) is not a
// replay: it returns an untyped wrapped error and posts nothing.
//
// A violation of migration 0092's partial unique index
// (ledger_transactions_one_deposit_reversal) is a THIRD, distinct case,
// never confused with an idempotency-key replay: it returns
// ErrReversalAlreadyExists and posts nothing (Stage 10.1 PAY-REV-1).
//
// Rule B2 (extended) mirror generator (ledger-accounting-model.md §7.4,
// bonus_mirror.go): a posting any of whose entries resolves to a
// BONUS_SET account (player_bonus, player_locked_bonus, player_bonus_held)
// is mirrored automatically here, before anything is written - the
// caller supplies only the economically real legs plus BonusCost, and
// Post appends the promo_liability/bonus_expense/provider_payable legs
// invariant B1 requires. A caller may never hand-assemble one of those
// legs itself (HR-17); doing so is rejected before any write, same as
// every other validation failure below.
//

// Balance (invariant #1): entries are validated to balance per asset by
// a deferred database constraint trigger (migration 0022), which Post
// forces to run immediately (rather than at the caller's eventual
// COMMIT) so an unbalanced call fails synchronously, here, with a clear
// error - not silently at some later, unrelated commit.
//
// Lock ordering (docs/decisions/0082, lockorder.go). Post takes ALL of
// this posting's wallet_balance_projection locks (class L3) in one step,
// in canonical ascending-ledger_account_id order, over the COMPLETE final
// entry set - caller entries plus the Rule B2 mirror/recognition legs
// generated below - and it does so BEFORE db.IdempotentInsert's
// ledger_transactions key insert (class L4, ADR 0082 R5, reversing the
// order these two steps used to run in). Two consequences the caller must
// know:
//
//   - A caller may NOT take a wallet_balance_projection lock itself (R4);
//     if it needs a balance before deciding whether to post, it calls
//     LockProjectionsForPosting with the SAME TransactionInput it will
//     later hand to Post, and reads the balance from that result.
//     Re-locking here is then a no-op.
//   - A caller may NOT take any L0 (advisory), L1 (domain state row) or
//     L2 (ledger_transactions FOR UPDATE) lock AFTER calling
//     LockProjectionsForPosting or Post (R8), with the single named and
//     tested exception E-1 (ADR 0082 §5.1).
//
// Entry insertion order into ledger_entries is deliberately unchanged
// (R7): §7.4.2's byte-identical-entry-rows property is an auditability
// property, and once the pre-lock step exists, entry order has no locking
// meaning left to exploit.
func Post(ctx context.Context, tx pgx.Tx, in TransactionInput) (PostResult, error) {
	// Validation + Rule B2 (extended) mirror generation + §7.4.2's fixed-
	// order concatenation, extracted so LockProjectionsForPosting computes
	// the identical final entry set from the identical input. Runs BEFORE
	// anything is locked or written, so a validation failure leaves no
	// ledger_transactions row, no ledger_entries row and no projection
	// row.
	entriesToPost, err := prepareEntries(ctx, tx, in)
	if err != nil {
		return PostResult{}, err
	}

	// ADR 0082 class L3, before L4 (rule R5). Every projection row this
	// posting's ledger_entries inserts would otherwise lock implicitly -
	// via migration 0023's AFTER INSERT trigger, one exclusive row lock
	// per entry in slice order - is materialised and locked here instead,
	// in one canonical ascending order, so the trigger below acquires
	// nothing new. Also closes finding L-a: a transaction can no longer
	// hold the (tenant_id, idempotency_key) index entry while waiting on a
	// projection row, because two deliveries of one key necessarily target
	// the same projection set and therefore serialize here, at L3.
	//
	// Accepted consequence, stated in the ADR: an idempotent replay now
	// takes its projection locks before discovering it is a no-op.
	if _, err := ensureAndLockProjectionsInOrder(ctx, tx, in.TenantID,
		canonicalAccountOrder(entryAccountIDs(entriesToPost))); err != nil {
		return PostResult{}, err
	}

	transactionID := uuid.New()
	conflict, conflictConstraint, err := db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error {
		_, err := spTx.Exec(ctx,
			`INSERT INTO ledger_transactions
				(id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id,
				 correlation_id, causation_id, reverses_transaction_id, reason_code)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			transactionID, in.TenantID, in.TransactionType, in.IdempotencyKey, in.ProviderID, in.ProviderTxID,
			in.CorrelationID, in.CausationID, in.ReversesTransactionID, in.ReasonCode,
		)
		return err
	})
	if err != nil {
		return PostResult{}, fmt.Errorf("ledger: insert transaction: %w", err)
	}

	if conflict {
		// No new lock is needed for the comparison below: this call
		// already holds every L3 projection lock of ITS entry set, and
		// ledger rows are immutable (migration 0082), so the stored
		// transaction read here cannot change underneath it.
		//
		// ledger-finance P2-A (Stage 10.1 review): Postgres reports
		// whichever violated unique index it happens to check FIRST when
		// an INSERT violates more than one at once, and it checks in
		// INDEX-OID order - not a fixed, semantically meaningful order.
		// A same-key `deposit_reversal` retry violates BOTH the ordinary
		// (tenant_id, idempotency_key) index AND migration 0092's
		// (tenant_id, reverses_transaction_id) partial index simultaneously.
		// Today the idempotency index happens to be older (lower OID), so
		// conflictConstraint reports it and this call already takes the
		// idempotency-key replay path below. But a routine
		// `REINDEX INDEX CONCURRENTLY` (or any migration that recreates
		// either constraint) can flip which index Postgres reports FIRST,
		// which used to make conflictConstraint report the 0092 index
		// instead - and the OLD code below trusted that name blindly,
		// returning ErrReversalAlreadyExists (-> HTTP 409, a false
		// integrity alert, a false denial audit) for a retry that in fact
		// posted successfully the first time. Fail-closed in the sense
		// that nothing double-posts either way, but it silently broke the
		// idempotency contract for a legitimate retry under an ordinary
		// DBA action.
		//
		// The fix does not trust conflictConstraint's identity at all for
		// classification: it ALWAYS looks up the idempotency key first,
		// regardless of which constraint Postgres happened to name. If a
		// transaction already exists under this exact key, this is
		// unconditionally a retry/replay (whether the reported constraint
		// was the idempotency index, the 0092 index, or both at once), and
		// falls through to the ordinary AlreadyPosted / payload-mismatch
		// comparison unchanged. ErrReversalAlreadyExists is returned only
		// when the 0092 index fired AND no row exists for this
		// idempotency key at all - i.e. this INSERT's own idempotency_key
		// was genuinely never written, so the conflict can only be a
		// distinct, second reversal of the same original deposit.
		existing, lookupErr := lookupByIdempotencyKey(ctx, tx, in.TenantID, in.IdempotencyKey)
		if lookupErr != nil {
			if errors.Is(lookupErr, pgx.ErrNoRows) && conflictConstraint == reversalOneDepositReversalConstraint {
				return PostResult{}, fmt.Errorf("%w: reverses_transaction_id=%v", ErrReversalAlreadyExists, in.ReversesTransactionID)
			}
			return PostResult{}, fmt.Errorf("ledger: look up existing transaction for idempotency key: %w", lookupErr)
		}
		if existing.TransactionType != in.TransactionType {
			return PostResult{}, fmt.Errorf("%w: existing transaction %s has type %q, requested %q",
				ErrIdempotencyKeyReused, existing.ID, existing.TransactionType, in.TransactionType)
		}
		diffs, cmpErr := replayPayloadDifferences(ctx, tx, existing, in, entriesToPost)
		if cmpErr != nil {
			return PostResult{}, cmpErr
		}
		if len(diffs) > 0 {
			return PostResult{}, fmt.Errorf("%w: existing transaction %s differs in [%s]",
				ErrIdempotencyPayloadMismatch, existing.ID, formatReplayDiffs(diffs))
		}
		return PostResult{TransactionID: existing.ID, AlreadyPosted: true}, nil
	}

	for _, e := range entriesToPost {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
			 VALUES ($1, $2, $3, (SELECT asset_code FROM ledger_accounts WHERE id = $2), $4, $5)`,
			transactionID, e.LedgerAccountID, in.TenantID, e.Direction, e.Amount,
		); err != nil {
			return PostResult{}, fmt.Errorf("ledger: insert entry for account %s: %w", e.LedgerAccountID, err)
		}
	}

	// Force the deferred balance-check trigger (migration 0022) to run
	// now rather than at the caller's eventual COMMIT, so an unbalanced
	// Post call fails synchronously with a clear error here.
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ledger_entries_balanced IMMEDIATE`); err != nil {
		return PostResult{}, fmt.Errorf("ledger: balance check failed: %w", err)
	}
	// Postgres's SET CONSTRAINTS mode change lasts until end of
	// transaction, not end of statement - restore DEFERRED so a SECOND
	// Post call in the same caller-supplied transaction still gets the
	// deferred-until-explicitly-checked semantics migration 0022 requires,
	// instead of having its first entry INSERT fail immediately because
	// the transaction's balance hasn't been completed yet.
	if _, err := tx.Exec(ctx, `SET CONSTRAINTS ledger_entries_balanced DEFERRED`); err != nil {
		return PostResult{}, fmt.Errorf("ledger: restore deferred constraint mode: %w", err)
	}

	return PostResult{TransactionID: transactionID}, nil
}

// GetOrCreateAccount returns the id of the LedgerAccount for
// (walletID, accountType, assetCode) if walletID is non-nil (a
// player-owned account), or (tenantID, accountType, assetCode) if
// walletID is nil (a house-level account) - creating it on first use.
// Race-free under concurrent first postings via INSERT ... ON CONFLICT
// DO NOTHING against the partial unique index, never check-then-insert
// (ledger-accounting-model.md §1.1).
func GetOrCreateAccount(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, walletID *uuid.UUID, accountType AccountType, assetCode string) (uuid.UUID, error) {
	newID := uuid.New()
	if walletID != nil {
		if _, err := tx.Exec(ctx,
			`INSERT INTO ledger_accounts (id, tenant_id, wallet_id, account_type, asset_code)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (wallet_id, account_type, asset_code) WHERE wallet_id IS NOT NULL DO NOTHING`,
			newID, tenantID, *walletID, accountType, assetCode,
		); err != nil {
			return uuid.Nil, fmt.Errorf("ledger: get or create player account: %w", err)
		}
		var id uuid.UUID
		err := tx.QueryRow(ctx,
			`SELECT id FROM ledger_accounts WHERE wallet_id = $1 AND account_type = $2 AND asset_code = $3`,
			*walletID, accountType, assetCode,
		).Scan(&id)
		if err != nil {
			return uuid.Nil, fmt.Errorf("ledger: fetch player account: %w", err)
		}
		return id, nil
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO ledger_accounts (id, tenant_id, wallet_id, account_type, asset_code, status)
		 VALUES ($1, $2, NULL, $3, $4, 'active')
		 ON CONFLICT (tenant_id, account_type, asset_code) WHERE wallet_id IS NULL DO NOTHING`,
		newID, tenantID, accountType, assetCode,
	); err != nil {
		return uuid.Nil, fmt.Errorf("ledger: get or create house account: %w", err)
	}
	var id uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id FROM ledger_accounts WHERE tenant_id = $1 AND account_type = $2 AND asset_code = $3 AND wallet_id IS NULL`,
		tenantID, accountType, assetCode,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("ledger: fetch house account: %w", err)
	}
	return id, nil
}

// Balance is a LedgerAccount's balance, credit-positive for every
// account type without exception (ledger-accounting-model.md §5) -
// which sign is "healthy" for a given account_type is a presentation-
// layer/reporting concern, never encoded into this struct.
type Balance struct {
	LedgerAccountID uuid.UUID
	AssetCode       string
	AccountType     AccountType
	DebitTotal      int64
	CreditTotal     int64
	// Found is true only when GetProjectedBalance actually read a
	// wallet_balance_projection row. A zero-valued Balance with
	// Found == false means the row is genuinely absent, which the
	// Stage 3C reconciliation sweep (internal/reconciliation) must
	// distinguish from "a row exists whose totals happen to be zero" -
	// the two are different failure modes (a missing projection can mean
	// the projection trigger never ran; a present zero-balance row is
	// entirely ordinary for an account with no activity yet). Unused by
	// every pre-Stage-3C caller, which only ever read the totals.
	Found bool
}

// Signed returns CreditTotal - DebitTotal.
func (b Balance) Signed() int64 { return b.CreditTotal - b.DebitTotal }

// GetProjectedBalance reads the subordinate, materialized balance
// projection - the ordinary read path (docs/decisions/0019 "Balance
// serving"). Returns a zero Balance (not an error) if the account has
// never been posted to - check Found to tell that apart from a present
// row whose totals are zero.
func GetProjectedBalance(ctx context.Context, tx pgx.Tx, ledgerAccountID uuid.UUID) (Balance, error) {
	var b Balance
	b.LedgerAccountID = ledgerAccountID
	err := tx.QueryRow(ctx,
		`SELECT asset_code, account_type, debit_total, credit_total
		 FROM wallet_balance_projection WHERE ledger_account_id = $1`,
		ledgerAccountID,
	).Scan(&b.AssetCode, &b.AccountType, &b.DebitTotal, &b.CreditTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return Balance{LedgerAccountID: ledgerAccountID}, nil
	}
	if err != nil {
		return Balance{}, fmt.Errorf("ledger: get projected balance: %w", err)
	}
	b.Found = true
	return b, nil
}

// RebuildBalance recomputes a LedgerAccount's balance directly from
// ledger_entries - the authoritative source, bypassing the projection
// entirely (docs/decisions/0019 "Balance serving": "the projection can
// never diverge... but the rebuild procedure must still exist and be
// exercised"). Used by the reconciliation job and by
// RebuildProjectionRow, and directly by tests proving the projection is
// always reproducible from the ledger alone (invariant #9).
func RebuildBalance(ctx context.Context, tx pgx.Tx, ledgerAccountID uuid.UUID) (Balance, error) {
	var b Balance
	b.LedgerAccountID = ledgerAccountID
	err := tx.QueryRow(ctx,
		`SELECT
			la.asset_code, la.account_type,
			COALESCE(SUM(le.amount) FILTER (WHERE le.direction = 'debit'), 0),
			COALESCE(SUM(le.amount) FILTER (WHERE le.direction = 'credit'), 0)
		 FROM ledger_accounts la
		 LEFT JOIN ledger_entries le ON le.ledger_account_id = la.id
		 WHERE la.id = $1
		 GROUP BY la.asset_code, la.account_type`,
		ledgerAccountID,
	).Scan(&b.AssetCode, &b.AccountType, &b.DebitTotal, &b.CreditTotal)
	if err != nil {
		return Balance{}, fmt.Errorf("ledger: rebuild balance: %w", err)
	}
	return b, nil
}

// RebuildProjectionRow drops and recomputes the projection row for
// ledgerAccountID from ledger_entries alone, then overwrites
// wallet_balance_projection with that authoritative recomputation. This
// is the exercised disaster-recovery path docs/decisions/0019 requires
// exist, not merely a theoretical property: a corrupted or manually
// deleted projection row is always fully recoverable.
//
// Lock ordering (ADR 0082 class L3): this takes an exclusive row lock on
// the projection row via its ON CONFLICT DO UPDATE. Callers iterating
// MULTIPLE accounts must iterate in ascending ledger_account_id order -
// use canonicalAccountOrder - or two concurrent repairs over overlapping
// account sets can deadlock. It has no production caller today (DR and
// tests only), which is why it is not routed through
// ensureAndLockProjectionsInOrder; that is a fact about today's callers,
// not a licence for a future one to iterate unordered.
func RebuildProjectionRow(ctx context.Context, tx pgx.Tx, ledgerAccountID uuid.UUID) (Balance, error) {
	b, err := RebuildBalance(ctx, tx, ledgerAccountID)
	if err != nil {
		return Balance{}, err
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO wallet_balance_projection
			(ledger_account_id, tenant_id, wallet_id, player_account_id, asset_code, account_type, debit_total, credit_total, updated_at)
		 SELECT la.id, la.tenant_id, la.wallet_id, la.player_account_id, $2, $3, $4, $5, now()
		 FROM ledger_accounts la WHERE la.id = $1
		 ON CONFLICT (ledger_account_id) DO UPDATE SET
			debit_total = EXCLUDED.debit_total,
			credit_total = EXCLUDED.credit_total,
			updated_at = now()`,
		ledgerAccountID, b.AssetCode, b.AccountType, b.DebitTotal, b.CreditTotal,
	)
	if err != nil {
		return Balance{}, fmt.Errorf("ledger: rebuild projection row: %w", err)
	}
	return b, nil
}
