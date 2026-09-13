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
// than int64 without pulling in math/big for no realistic benefit.
package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// AccountType is one of the Blueprint's ten ledger account types plus the
// Stage 3A architectural addition player_withdrawal_hold. See
// docs/architecture/ledger-accounting-model.md §2.
type AccountType string

const (
	AccountPlayerCash           AccountType = "player_cash"
	AccountPlayerBonus          AccountType = "player_bonus"
	AccountPlayerLocked         AccountType = "player_locked"
	AccountPlayerWithdrawalHold AccountType = "player_withdrawal_hold"
	AccountHouseGaming          AccountType = "house_gaming"
	AccountProviderPayable      AccountType = "provider_payable"
	AccountPSPClearing          AccountType = "psp_clearing"
	AccountPSPReserve           AccountType = "psp_reserve"
	AccountJackpotContribution  AccountType = "jackpot_contribution"
	AccountPromoLiability       AccountType = "promo_liability"
	AccountManualAdjustment     AccountType = "manual_adjustment"
)

// TransactionType is limited to the flows Stage 3B actually implements
// (deposit/withdrawal/manual_adjustment/tombstone) - see migration
// 0021's own comment. Casino/sportsbook/bonus/crypto types are added by
// an additive migration + a new const here when their owning stage
// implements them; they are explicitly BLOCKED this stage
// (CLAUDE.md Stage 3B scope gate).
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
)

// Direction is a ledger entry's debit/credit side. Never a signed amount
// column - see ledger-accounting-model.md §1.3.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// ErrIdempotencyKeyReused is returned when a retried Post call's
// idempotency_key matches an existing transaction whose transaction_type
// differs from the one now requested - a same-key-different-payload
// replay (ADR 0020), rejected rather than silently applied or silently
// treated as a match.
var ErrIdempotencyKeyReused = errors.New("ledger: idempotency key reused with a different transaction type")

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
}

// PostResult is what Post returns.
type PostResult struct {
	TransactionID uuid.UUID
	// AlreadyPosted is true when this call was an idempotent no-op: a
	// transaction with the same (tenant_id, idempotency_key) already
	// existed, so nothing new was posted and TransactionID identifies the
	// ORIGINAL transaction.
	AlreadyPosted bool
}

// Post posts one LedgerTransaction and its entries atomically, inside
// tx - the caller's own already-open, correctly tenant-scoped
// transaction (via db.Pool.WithTenant), exactly like internal/audit's
// Record function. Post never opens its own top-level transaction and
// never commits tx itself.
//
// Idempotency (ADR 0020): a retried call with the same
// (tenant_id, idempotency_key) returns the original result rather than
// erroring or double-posting, via the SAVEPOINT pattern in
// db.IdempotentInsert. A retried call whose transaction_type differs from
// the original's returns ErrIdempotencyKeyReused rather than silently
// preferring either payload.
//
// Balance (invariant #1): entries are validated to balance per asset by
// a deferred database constraint trigger (migration 0022), which Post
// forces to run immediately (rather than at the caller's eventual
// COMMIT) so an unbalanced call fails synchronously, here, with a clear
// error - not silently at some later, unrelated commit.
func Post(ctx context.Context, tx pgx.Tx, in TransactionInput) (PostResult, error) {
	if in.TenantID == uuid.Nil {
		return PostResult{}, fmt.Errorf("%w: tenant id is required", ErrInvalidEntry)
	}
	if in.IdempotencyKey == "" {
		return PostResult{}, fmt.Errorf("%w: idempotency key is required", ErrInvalidEntry)
	}
	if in.CorrelationID == uuid.Nil {
		return PostResult{}, fmt.Errorf("%w: correlation id is required", ErrInvalidEntry)
	}
	if (in.ProviderID == nil) != (in.ProviderTxID == nil) {
		return PostResult{}, fmt.Errorf("%w: provider_id and provider_tx_id must both be set or both be nil", ErrInvalidEntry)
	}
	if in.TransactionType == TxManualAdjustment && (in.ReasonCode == nil || *in.ReasonCode == "") {
		return PostResult{}, fmt.Errorf("%w: manual_adjustment requires a reason code", ErrInvalidEntry)
	}
	for _, e := range in.Entries {
		if e.Amount <= 0 {
			return PostResult{}, fmt.Errorf("%w: entry amount must be positive, got %d", ErrInvalidEntry, e.Amount)
		}
		if e.Direction != Debit && e.Direction != Credit {
			return PostResult{}, fmt.Errorf("%w: entry direction must be debit or credit, got %q", ErrInvalidEntry, e.Direction)
		}
	}

	transactionID := uuid.New()
	conflict, err := db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error {
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
		existingID, existingType, lookupErr := lookupByIdempotencyKey(ctx, tx, in.TenantID, in.IdempotencyKey)
		if lookupErr != nil {
			return PostResult{}, fmt.Errorf("ledger: look up existing transaction for idempotency key: %w", lookupErr)
		}
		if existingType != in.TransactionType {
			return PostResult{}, fmt.Errorf("%w: existing transaction %s has type %q, requested %q",
				ErrIdempotencyKeyReused, existingID, existingType, in.TransactionType)
		}
		return PostResult{TransactionID: existingID, AlreadyPosted: true}, nil
	}

	for _, e := range in.Entries {
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

func lookupByIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, key string) (uuid.UUID, TransactionType, error) {
	var id uuid.UUID
	var txType TransactionType
	err := tx.QueryRow(ctx,
		`SELECT id, transaction_type FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`,
		tenantID, key,
	).Scan(&id, &txType)
	return id, txType, err
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
}

// Signed returns CreditTotal - DebitTotal.
func (b Balance) Signed() int64 { return b.CreditTotal - b.DebitTotal }

// GetProjectedBalance reads the subordinate, materialized balance
// projection - the ordinary read path (docs/decisions/0019 "Balance
// serving"). Returns a zero Balance (not an error) if the account has
// never been posted to.
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
