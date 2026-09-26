package casino

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// StatementLine, StatementTotal and StatementSource are the casino
// reconciliation statement contract (internal/reconciliation/statement, a
// dependency-free leaf so neither package imports the other), aliased here
// for the casino domain's own use - the sportsbook.SettlementStatement*
// precedent.
type (
	StatementLine   = statement.CasinoStatementLine
	StatementTotal  = statement.CasinoStatementTotal
	StatementSource = statement.CasinoStatementSource
)

// MockStatementLabel is the label every record, audit entry and log line of
// the MOCK casino statement match carries.
const MockStatementLabel = "MOCK in-house casino statement (rendered from the platform's own casino ledger rows; tautological by construction - proves the matching plumbing only; real provider statement matching is PROVIDER DEPENDENT)"

// MockStatementSource is the casino_statement stream's statement source
// (Stage 10.3 W3a, CAS-RECON-STMT-1; docs/plans/stage-10.3-planning/
// 02-casino-financial-analysis.md §2.5), injected into the reconciliation
// sweep by cmd/platform-api.
//
// MOCK: no contracted casino aggregator exists, so there is no provider
// statement. This source renders a "statement" from the platform's OWN
// casino ledger rows (casino_bet, casino_win, casino_rollback) plus the
// casino_provider_rounds binding for the round id. Because it derives from
// the very ledger the stream matches it against, a clean run on
// uncorrupted data is TAUTOLOGICAL: it proves only that the stream, its
// sweep wiring, its advisory lock, its audit and its evidence rows work end
// to end - never that the platform agrees with any provider. What proves
// the matching logic itself is the divergent statement sources in the
// stream's integration tests, which exercise every detection path. Real
// statement ingestion (provider API or file, stored append-only, per-tenant
// credentials from the secret store) and real matching are PROVIDER
// DEPENDENT and NOT IMPLEMENTED.
//
// Rendering rule (all-time, like sportsbook.MockSettlementStatementSource;
// periodStart/periodEnd are ignored):
//   - one line per casino_bet/casino_win/casino_rollback that carries a
//     provider reference, keyed by (provider_id, provider_tx_id);
//   - bet amount = the stake (debits on the player's spendable accounts),
//     win amount = the payout (the house_gaming debit), rollback amount =
//     its original's amount, rollback original = its original's reference;
//   - round id from casino_provider_rounds under the transaction's own
//     correlation id ("" when no binding row exists);
//   - tombstones are not lines (a provider lists its rollback; this mock
//     cannot, since the ledger keeps only the original's reference on a
//     tombstone);
//   - one total per (provider, asset): the net house_gaming movement of
//     that provider's casino transactions.
type MockStatementSource struct{}

var _ StatementSource = MockStatementSource{}

// Label implements StatementSource.
func (MockStatementSource) Label() string { return MockStatementLabel }

// SyntheticComponent implements providerkind.Synthetic (Stage 10.3,
// MOCK-ADAPTER-PROD-1) - a structural marker only, satisfied without this
// package importing internal/providerkind, so the production startup guard
// refuses a binary that wires this source.
func (MockStatementSource) SyntheticComponent() {}

// Statement implements StatementSource. Read-only.
func (MockStatementSource) Statement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, _, _ time.Time) ([]StatementLine, []StatementTotal, error) {
	rows, err := tx.Query(ctx, `
		WITH amounts AS (
		    SELECT t.id, t.provider_id, t.provider_tx_id, t.transaction_type, t.correlation_id,
		           t.reverses_transaction_id,
		           COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit' AND la.wallet_id IS NOT NULL
		                                          AND la.account_type IN ('player_cash', 'player_bonus')), 0) AS stake,
		           COALESCE(SUM(e.amount) FILTER (WHERE e.direction = 'debit' AND la.account_type = 'house_gaming'), 0) AS payout,
		           COALESCE(min(la.asset_code), '') AS asset
		      FROM ledger_transactions t
		      LEFT JOIN ledger_entries e ON e.ledger_transaction_id = t.id
		      LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
		     WHERE t.tenant_id = $1 AND t.transaction_type IN ('casino_bet', 'casino_win', 'casino_rollback')
		       AND t.provider_id IS NOT NULL AND t.provider_tx_id IS NOT NULL
		     GROUP BY t.id)
		SELECT a.provider_id, a.provider_tx_id, a.transaction_type,
		       COALESCE(o.provider_tx_id, ''),
		       COALESCE(pr.provider_round_id, ''),
		       a.asset,
		       (CASE a.transaction_type
		            WHEN 'casino_bet' THEN a.stake
		            WHEN 'casino_win' THEN a.payout
		            ELSE CASE o.transaction_type WHEN 'casino_bet' THEN o.stake WHEN 'casino_win' THEN o.payout ELSE 0 END
		        END)::text
		  FROM amounts a
		  LEFT JOIN amounts o ON o.id = a.reverses_transaction_id
		  LEFT JOIN casino_provider_rounds pr
		    ON pr.tenant_id = $1 AND pr.provider_id = a.provider_id AND pr.correlation_id = a.correlation_id
		 ORDER BY a.provider_id, a.provider_tx_id`, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("casino: MOCK statement lines: %w", err)
	}
	var lines []StatementLine
	for rows.Next() {
		var l StatementLine
		var txType, amount string
		if err := rows.Scan(&l.ProviderID, &l.ProviderTxID, &txType, &l.OriginalProviderTxID, &l.RoundID, &l.AssetCode, &amount); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("casino: MOCK statement lines: scan: %w", err)
		}
		switch txType {
		case "casino_bet":
			l.Kind = statement.CasinoLineBet
		case "casino_win":
			l.Kind = statement.CasinoLineWin
		default:
			l.Kind = statement.CasinoLineRollback
		}
		// Minor units never exceed int64 on a casino posting (EntryInput
		// amounts are int64); a value that does not fit fails the render
		// closed rather than wrapping.
		if l.Amount, err = strconv.ParseInt(amount, 10, 64); err != nil {
			rows.Close()
			return nil, nil, fmt.Errorf("casino: MOCK statement lines: amount %q: %w", amount, err)
		}
		lines = append(lines, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("casino: MOCK statement lines: %w", err)
	}

	rows, err = tx.Query(ctx, `
		SELECT t.provider_id, la.asset_code,
		       SUM(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END)::text
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		  JOIN ledger_accounts la ON la.id = e.ledger_account_id
		 WHERE t.tenant_id = $1 AND t.transaction_type IN ('casino_bet', 'casino_win', 'casino_rollback')
		   AND t.provider_id IS NOT NULL AND la.account_type = 'house_gaming'
		 GROUP BY t.provider_id, la.asset_code
		 ORDER BY 1, 2`, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("casino: MOCK statement totals: %w", err)
	}
	defer rows.Close()
	var totals []StatementTotal
	for rows.Next() {
		var tot StatementTotal
		var ggr string
		if err := rows.Scan(&tot.ProviderID, &tot.AssetCode, &ggr); err != nil {
			return nil, nil, fmt.Errorf("casino: MOCK statement totals: scan: %w", err)
		}
		v, ok := new(big.Int).SetString(ggr, 10)
		if !ok {
			return nil, nil, fmt.Errorf("casino: MOCK statement totals: unparseable GGR %q", ggr)
		}
		tot.GGR = v
		totals = append(totals, tot)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("casino: MOCK statement totals: %w", err)
	}
	return lines, totals, nil
}
