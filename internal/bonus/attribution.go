// GrantLedgerAttribution (docs/architecture/10-bonus-engine-architecture.md
// "doc 10" §W2.5), backed by migration 0061's grant_ledger_attributions
// table. Deferred by Phase 2 ("deriving a Grant's remaining bonus balance
// without a maintained counter"), built here.
//
// The one binding rule this file exists to satisfy (CLAUDE.md: "Never
// UPDATE a balance. Balances are projections recomputed from ledger
// entries"; doc 10 §W2.5: "A Grant's remaining bonus balance is always a
// derived read... never a stored figure"): AttributedBonusBalance never
// reads a maintained counter. It reads grant_ledger_attributions (which
// names WHICH ledger transactions belong to this Grant) joined against
// ledger_entries (which is the money) and sums, live, every time it is
// called.
package bonus

import (
	"context"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// GrantLedgerAttribution mirrors one grant_ledger_attributions row.
type GrantLedgerAttribution struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	GrantID             uuid.UUID
	LedgerTransactionID uuid.UUID
	TransactionType     string
}

// AttributeGrantLedgerTransaction inserts one attribution row, in the
// SAME database transaction as the ledger.Post call it attributes (doc 10
// §W2.5's own requirement). DB-unique on (tenant_id, grant_id,
// ledger_transaction_id) - a caller that calls this twice for the same
// (grant, transaction) pair (e.g. because ledger.Post returned
// AlreadyPosted for a retried idempotent call) gets ErrAlreadyAttributed,
// never a silent double-attribution.
var ErrAlreadyAttributed = fmt.Errorf("bonus: this ledger transaction is already attributed to this grant")

func AttributeGrantLedgerTransaction(ctx context.Context, tx pgx.Tx, tenantID, grantID, ledgerTransactionID uuid.UUID, transactionType string) (GrantLedgerAttribution, error) {
	row := tx.QueryRow(ctx, `
		INSERT INTO grant_ledger_attributions (id, tenant_id, grant_id, ledger_transaction_id, transaction_type)
		VALUES (gen_random_uuid(), $1, $2, $3, $4)
		ON CONFLICT (tenant_id, grant_id, ledger_transaction_id) DO NOTHING
		RETURNING id, tenant_id, grant_id, ledger_transaction_id, transaction_type`,
		tenantID, grantID, ledgerTransactionID, transactionType,
	)
	var a GrantLedgerAttribution
	err := row.Scan(&a.ID, &a.TenantID, &a.GrantID, &a.LedgerTransactionID, &a.TransactionType)
	if err == pgx.ErrNoRows {
		return GrantLedgerAttribution{}, ErrAlreadyAttributed
	}
	if err != nil {
		return GrantLedgerAttribution{}, fmt.Errorf("bonus: attribute grant ledger transaction: %w", err)
	}
	return a, nil
}

// AttributeGrantLedgerTransactionIdempotent is
// AttributeGrantLedgerTransaction but treats ErrAlreadyAttributed as
// success (a no-op) - the shape every lifecycle call site actually wants,
// since a retried ledger.Post legitimately returns the SAME
// ledger_transaction_id twice and re-attributing it is not an error.
func AttributeGrantLedgerTransactionIdempotent(ctx context.Context, tx pgx.Tx, tenantID, grantID, ledgerTransactionID uuid.UUID, transactionType string) error {
	_, err := AttributeGrantLedgerTransaction(ctx, tx, tenantID, grantID, ledgerTransactionID, transactionType)
	if err != nil && err != ErrAlreadyAttributed {
		return err
	}
	return nil
}

// AccountBalance is one account's signed net movement over a Grant's
// attributed ledger entries, in that account's own asset's minor units.
// Positive = net credit, negative = net debit (ledger-accounting-
// model.md's credit-positive convention, mirrored from
// internal/ledger/bonus_mirror.go's signedAmount).
type AccountBalance struct {
	AccountType string
	Net         *big.Int
}

// AttributedBalanceByAccountType computes, for a single Grant, the live,
// derived net signed balance per ledger_accounts.account_type, summed
// over every ledger_entries row belonging to a ledger_transaction
// attributed to this Grant via grant_ledger_attributions. This is the
// ONLY way this package ever answers "how much value does this Grant
// currently have in player_bonus / player_locked_bonus / player_bonus_held"
// - never a maintained counter (CLAUDE.md, doc 10 §W2.5).
//
// tx must already be scoped (WithTenant) to tenantID; this function does
// not itself resolve the caller's RLS scope.
func AttributedBalanceByAccountType(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (map[string]*big.Int, error) {
	rows, err := tx.Query(ctx, `
		SELECT la.account_type,
		       SUM(CASE WHEN le.direction = 'credit' THEN le.amount ELSE -le.amount END) AS net
		  FROM grant_ledger_attributions gla
		  JOIN ledger_entries le ON le.ledger_transaction_id = gla.ledger_transaction_id AND le.tenant_id = gla.tenant_id
		  JOIN ledger_accounts la ON la.id = le.ledger_account_id
		 WHERE gla.tenant_id = $1 AND gla.grant_id = $2
		 GROUP BY la.account_type`,
		tenantID, grantID,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: attributed balance by account type: %w", err)
	}
	defer rows.Close()

	out := map[string]*big.Int{}
	for rows.Next() {
		var accountType string
		var net pgtype.Numeric
		if err := rows.Scan(&accountType, &net); err != nil {
			return nil, fmt.Errorf("bonus: scan attributed balance row: %w", err)
		}
		amt, err := numericToBigInt(net)
		if err != nil {
			return nil, err
		}
		if amt == nil {
			amt = big.NewInt(0)
		}
		out[accountType] = amt
	}
	return out, rows.Err()
}

// RemainingBonusBalance is the Grant's own outstanding player_bonus
// exposure - the figure §W2.5 exists to make answerable: the net
// signed balance of the player_bonus account type attributed to this
// Grant. Never includes player_locked_bonus or player_bonus_held (those
// are separate AOE components, never spendable/convertible bonus
// balance in their own right - doc 10 N1.3/N1.4.1 item 3).
func RemainingBonusBalance(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (*big.Int, error) {
	byType, err := AttributedBalanceByAccountType(ctx, tx, tenantID, grantID)
	if err != nil {
		return nil, err
	}
	v, ok := byType["player_bonus"]
	if !ok {
		return big.NewInt(0), nil
	}
	return v, nil
}
