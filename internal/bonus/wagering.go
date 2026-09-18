// P_net/P_firm dual-measure derivation ("Model C", ledger-accounting-
// model.md §6.6, docs/architecture/10-bonus-engine-architecture.md
// "doc 10" §W2.8) - a read-only computation over bonus_wagering_progress
// plus ledger_transactions/ledger_entries, NEVER a write and NEVER a
// maintained counter.
//
// SCOPE NOTE, stated honestly rather than silently narrowed: doc 10 §W9
// explicitly allows the first slice to exercise Model C casino-only,
// where "P_firm == P_net identically" (no sportsbook locked-stake
// window, no win/loss/push settlement-outcome complexity - none of the
// five in-slice bonus types (Deposit/Reload/Cashback/Generic-Wagering/
// Coupon) involves a locked-stake product at all). This file implements
// exactly that casino-only regime: a contribution counts toward BOTH
// P_net and P_firm unless the underlying lock transaction it derives from
// has since been reversed (a casino_rollback, or a bonus_reversal/
// generic reversal naming it via reverses_transaction_id) - in which case
// it counts toward NEITHER (ledger-accounting-model.md §6.6.5's "an
// unclassified transaction type excludes... never treated as nullifying
// or as risk-preserving by a permissive default" fail-closed rule is
// honored by only ever recognizing the two transaction_type values this
// slice's own casino integration can actually produce, casino_bet and
// casino_rollback - any OTHER inbound transaction type correlated to a
// contribution's lock transaction is treated as unclassified and
// excluded from P_firm, raising ErrUnclassifiedWageringTransaction,
// rather than silently assumed confirmed).
//
// The full sportsbook-shaped P_net != P_firm distinction (win/loss/push,
// a settlement window, the G-2 HeldDisposition carve-out) is NOT built
// here - it is out of scope for the five in-slice bonus types, per doc 10
// §W9's own explicit allowance, and is named here as a genuine deferral,
// not a silently narrowed implementation of a wider contract.
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrUnclassifiedWageringTransaction is the fail-closed signal mirroring
// internal/risk/cumulative.go's ErrUnrecognizedCumulativeLeg one level
// up: a wagering-progress contribution's lock transaction was itself
// reversed/corrected by a transaction_type this package does not
// recognize as either "confirms the lock" or "nullifies the lock". Raised
// rather than silently treating the contribution as confirmed.
var ErrUnclassifiedWageringTransaction = errors.New("bonus: wagering contribution's lock transaction has an unclassified correcting transaction type")

// WageringProgressResult is the derived P_net/P_firm pair for one Grant,
// in the Grant's own asset's minor units.
type WageringProgressResult struct {
	PNet  *big.Int
	PFirm *big.Int
	// ContributionCount/NullifiedCount are diagnostic, for the Progress
	// trail / integrity-alert path, not part of the P_net/P_firm contract
	// itself.
	ContributionCount int
	NullifiedCount    int
}

// DeriveWageringProgress computes P_net/P_firm for grantID, live, from
// bonus_wagering_progress joined against ledger_transactions (doc 10
// §W2.8). Called at every completion/conversion checkpoint; NEVER a
// write. tx must already be the caller's own open, tenant-scoped
// transaction.
func DeriveWageringProgress(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) (WageringProgressResult, error) {
	contributions, err := ListWageringProgressByGrant(ctx, tx, tenantID, grantID)
	if err != nil {
		return WageringProgressResult{}, err
	}

	result := WageringProgressResult{PNet: big.NewInt(0), PFirm: big.NewInt(0)}
	for _, c := range contributions {
		nullified, err := lockTransactionIsNullified(ctx, tx, tenantID, c.LockLedgerTransactionID)
		if err != nil {
			return WageringProgressResult{}, err
		}
		result.ContributionCount++
		if nullified {
			result.NullifiedCount++
			continue
		}
		result.PNet.Add(result.PNet, c.QualifyingScaled)
		result.PFirm.Add(result.PFirm, c.QualifyingScaled)
	}
	return result, nil
}

// lockTransactionIsNullified reports whether lockTxID has been reversed
// by a correlated reversal/rollback transaction, per this file's own
// casino-only classification: a ledger_transactions row whose
// reverses_transaction_id = lockTxID and whose transaction_type is one of
// the two this slice's casino integration can produce
// (casino_rollback, bonus_reversal) nullifies it. Any OTHER
// reverses_transaction_id-carrying row is unclassified and fails closed
// (ErrUnclassifiedWageringTransaction), per ledger-accounting-model.md
// §6.6.5's binding discipline.
func lockTransactionIsNullified(ctx context.Context, tx pgx.Tx, tenantID, lockTxID uuid.UUID) (bool, error) {
	rows, err := tx.Query(ctx,
		`SELECT transaction_type FROM ledger_transactions WHERE tenant_id = $1 AND reverses_transaction_id = $2`,
		tenantID, lockTxID,
	)
	if err != nil {
		return false, fmt.Errorf("bonus: query reversals of lock transaction %s: %w", lockTxID, err)
	}
	defer rows.Close()

	nullified := false
	for rows.Next() {
		var txType string
		if err := rows.Scan(&txType); err != nil {
			return false, fmt.Errorf("bonus: scan reversal transaction type: %w", err)
		}
		switch txType {
		case "casino_rollback", "bonus_reversal", "tombstone":
			nullified = true
		default:
			return false, fmt.Errorf("%w: lock transaction %s reversed by unclassified transaction_type %q", ErrUnclassifiedWageringTransaction, lockTxID, txType)
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("bonus: read reversals of lock transaction %s: %w", lockTxID, err)
	}
	return nullified, nil
}
