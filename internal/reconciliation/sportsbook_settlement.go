package reconciliation

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// Sportsbook settlement reconciliation stream (ADR 0088 §8; Stage 10 W1).
//
// Four checks, all read-only, zero tolerance (reconciliation-model §1),
// every NUMERIC sum scanned as text into big.Int and never summed as
// int64 (ADR 0088 §3.6):
//
//	(a) §8.1 per wallet+asset: player_locked_cash net over sportsbook
//	    transaction types only == Σ stake of the wallet's open bets;
//	    player_locked_bonus net over the same types == 0.
//	(b) §8.2 per bet: nets of CASH/LOCKED/HOUSE over the bet's sportsbook
//	    transactions equal §2.3's end state for the bet's status; a
//	    settled_won bet's un-reversed ledger payout equals its current
//	    settlement row's payout_amount.
//	(c) §8.3 two-way orphan check (incl. sportsbook tombstones), causation
//	    consistency, and status == history-derived status.
//	(d) §8.4 statement match against an injectable
//	    statement.SportsbookSettlementSource. The only source today is
//	    sportsbook.MockSettlementStatementSource - MOCK; real provider
//	    statement matching is PROVIDER DEPENDENT (ADR 0038 §12).
//
// Like ledger_vs_projection, the stream recomputes the tenant's whole
// sportsbook population on every run; periodStart/periodEnd are recorded
// on the run row, not used as a filter (a drift is a drift regardless of
// when it was written).

// StreamSportsbookSettlement is ADR 0088 §8's stream.
const StreamSportsbookSettlement Stream = "sportsbook_settlement"

// Mismatch kinds added by migration 0091 (ADR 0088 §8).
const (
	MismatchKindSBLocked        MismatchKind = "sb_locked_mismatch"
	MismatchKindSBBetNet        MismatchKind = "sb_bet_net_mismatch"
	MismatchKindSBOrphanLedger  MismatchKind = "sb_orphan_ledger"
	MismatchKindSBOrphanHistory MismatchKind = "sb_orphan_history"
	MismatchKindSBStatus        MismatchKind = "sb_status_mismatch"
	// MismatchKindSBMockStatement is a divergence between the MOCK
	// statement source and the ledger (ADR 0088 §8.4).
	MismatchKindSBMockStatement MismatchKind = "sb_mock_statement_mismatch"
)

const (
	sbSettlementKeyPrefix = "sportsbook_settlement:"
	sbBetStatusOpen       = "open"
	sbBetStatusWon        = "settled_won"
	sbBetStatusLost       = "settled_lost"
	sbBetStatusVoid       = "void"
)

// sbLifecycleTypes are the transaction types whose entries make up a
// bet's lifecycle (ADR 0088 §2.2 step 4, §8.1). Casino writes
// player_locked_cash too, so every locked-balance sum is scoped to these.
var sbLifecycleTypes = []string{
	string(ledger.TxSportsbookBet), string(ledger.TxSportsbookSettlement),
	string(ledger.TxSportsbookVoid), string(ledger.TxSportsbookRollback),
}

// sbKindForTxType maps a W1 ledger transaction type to the history
// event_kind that must reference it (ADR 0088 §3.3 T-1, §8.3).
var sbKindForTxType = map[string]string{
	string(ledger.TxSportsbookSettlement): "settlement",
	string(ledger.TxSportsbookRollback):   "rollback",
	string(ledger.TxSportsbookVoid):       "void",
	string(ledger.TxTombstone):            "tombstone",
}

func sbTxTypeForKind(kind string) string {
	for t, k := range sbKindForTxType {
		if k == kind {
			return t
		}
	}
	return ""
}

// RunSportsbookSettlement runs the sportsbook_settlement stream for
// tenantID inside tx (db.Pool.WithTenant(tenantID)), recording exactly one
// ReconciliationRun and its mismatches, atomically - the same contract as
// RunLedgerVsProjection. It never writes anything else. source supplies
// the §8.4 statement (in production, cmd/platform-api injects
// sportsbook.MockSettlementStatementSource - MOCK); a nil source fails
// the run closed rather than skipping the statement match.
func RunSportsbookSettlement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time, source statement.SportsbookSettlementSource) (Run, []Mismatch, error) {
	if source == nil {
		return Run{}, nil, fmt.Errorf("reconciliation: %s requires a settlement statement source", StreamSportsbookSettlement)
	}
	run := Run{
		ID:          uuid.New(),
		TenantID:    tenantID,
		Stream:      StreamSportsbookSettlement,
		PeriodStart: periodStart,
		PeriodEnd:   periodEnd,
		RunAt:       time.Now().UTC(),
	}
	r := &sbRecorder{tenantID: tenantID}

	lines, err := source.StatementLines(ctx, tx, tenantID)
	if err != nil {
		return Run{}, nil, fmt.Errorf("reconciliation: statement source %q: %w", source.Label(), err)
	}
	footprint, err := sbHasFootprint(ctx, tx, tenantID)
	if err != nil {
		return Run{}, nil, fmt.Errorf("reconciliation: sportsbook footprint: %w", err)
	}
	if footprint || len(lines) > 0 {
		if err := sbRunChecks(ctx, tx, tenantID, source.Label(), lines, r); err != nil {
			return Run{}, nil, err
		}
	}

	mismatches := r.mismatches
	if len(mismatches) > 0 {
		run.Status = StatusMismatchesFound
	} else {
		run.Status = StatusClean
	}
	if err := persistRun(ctx, tx, run, mismatches); err != nil {
		return Run{}, nil, err
	}
	return run, mismatches, nil
}

// sbHasFootprint reports whether the tenant holds ANY input of any check:
// a bet, a sportsbook-typed ledger transaction (the four lifecycle types),
// a tombstone in the sportsbook_settlement: key namespace, or a history
// row. When it holds none and the statement has no lines, every check
// below is over empty sets and provably yields zero mismatches, so the
// run is recorded clean without them - a cost guard for the many tenants
// with no sportsbook activity, not a tolerance.
func sbHasFootprint(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM sportsbook_bets WHERE tenant_id = $1)
		    OR EXISTS (SELECT 1 FROM sportsbook_bet_settlements WHERE tenant_id = $1)
		    OR EXISTS (SELECT 1 FROM ledger_transactions
		                WHERE tenant_id = $1
		                  AND (transaction_type = ANY($2)
		                       OR (transaction_type = 'tombstone' AND idempotency_key LIKE 'sportsbook\_settlement:%')))`,
		tenantID, sbLifecycleTypes).Scan(&found)
	return found, err
}

func sbRunChecks(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, label string, lines []statement.SportsbookSettlementLine, r *sbRecorder) error {
	if err := sbCheckLocked(ctx, tx, tenantID, r); err != nil {
		return fmt.Errorf("reconciliation: sportsbook locked-vs-open check: %w", err)
	}
	if err := sbCheckBetNets(ctx, tx, tenantID, r); err != nil {
		return fmt.Errorf("reconciliation: sportsbook per-bet net check: %w", err)
	}
	if err := sbCheckOrphanLedger(ctx, tx, tenantID, r); err != nil {
		return fmt.Errorf("reconciliation: sportsbook ledger orphan check: %w", err)
	}
	if err := sbCheckOrphanHistory(ctx, tx, tenantID, r); err != nil {
		return fmt.Errorf("reconciliation: sportsbook history orphan check: %w", err)
	}
	if err := sbCheckStatus(ctx, tx, tenantID, r); err != nil {
		return fmt.Errorf("reconciliation: sportsbook status check: %w", err)
	}
	if err := sbCheckStatement(ctx, tx, tenantID, label, lines, r); err != nil {
		return fmt.Errorf("reconciliation: sportsbook statement match: %w", err)
	}
	return nil
}

type sbRecorder struct {
	tenantID   uuid.UUID
	mismatches []Mismatch
}

func (r *sbRecorder) add(kind MismatchKind, key, expected, actual string) {
	r.mismatches = append(r.mismatches, Mismatch{
		ID:                  uuid.New(),
		TenantID:            r.tenantID,
		ReconciliationKey:   key,
		ExpectedValue:       expected,
		ActualValue:         actual,
		MismatchKind:        kind,
		InvestigationStatus: investigationStatusOpen,
	})
}

func parseBig(s string) (*big.Int, error) {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		return nil, fmt.Errorf("unparseable NUMERIC %q", s)
	}
	return v, nil
}

// ---------------------------------------------------------------------
// (a) §8.1 locked balance vs open stakes, per wallet and asset.

func sbCheckLocked(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	rows, err := tx.Query(ctx, `
		WITH locked AS (
		    SELECT la.wallet_id, la.asset_code,
		           SUM(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END) AS net
		      FROM ledger_entries e
		      JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		      JOIN ledger_accounts la ON la.id = e.ledger_account_id
		     WHERE t.tenant_id = $1 AND t.transaction_type = ANY($2)
		       AND la.account_type = 'player_locked_cash'
		     GROUP BY la.wallet_id, la.asset_code),
		open_stakes AS (
		    SELECT wallet_id, asset_code, SUM(stake_amount::numeric) AS total
		      FROM sportsbook_bets
		     WHERE tenant_id = $1 AND status = 'open'
		     GROUP BY wallet_id, asset_code)
		SELECT COALESCE(l.wallet_id, o.wallet_id)::text, COALESCE(l.asset_code, o.asset_code),
		       COALESCE(o.total, 0)::text, COALESCE(l.net, 0)::text
		  FROM locked l
		  FULL OUTER JOIN open_stakes o ON o.wallet_id = l.wallet_id AND o.asset_code = l.asset_code
		 ORDER BY 1, 2`, tenantID, sbLifecycleTypes)
	if err != nil {
		return err
	}
	type lockedRow struct{ wallet, asset, open, net string }
	var cash []lockedRow
	for rows.Next() {
		var lr lockedRow
		var wallet *string
		if err := rows.Scan(&wallet, &lr.asset, &lr.open, &lr.net); err != nil {
			rows.Close()
			return err
		}
		if wallet != nil {
			lr.wallet = *wallet
		}
		cash = append(cash, lr)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, lr := range cash {
		open, err := parseBig(lr.open)
		if err != nil {
			return err
		}
		net, err := parseBig(lr.net)
		if err != nil {
			return err
		}
		if open.Cmp(net) != 0 {
			r.add(MismatchKindSBLocked,
				fmt.Sprintf("wallet=%s asset=%s account_type=player_locked_cash", lr.wallet, lr.asset),
				"open_bet_stakes="+open.String(), "ledger_locked_net="+net.String())
		}
	}

	// player_locked_bonus over sportsbook types must be 0 (never the
	// wallet's whole player_locked_bonus balance - casino bonus bets use
	// it; ADR 0088 §8.1, OI-2).
	rows, err = tx.Query(ctx, `
		SELECT la.wallet_id::text, la.asset_code,
		       SUM(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END)::text
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		  JOIN ledger_accounts la ON la.id = e.ledger_account_id
		 WHERE t.tenant_id = $1 AND t.transaction_type = ANY($2)
		   AND la.account_type = 'player_locked_bonus'
		 GROUP BY la.wallet_id, la.asset_code
		 ORDER BY 1, 2`, tenantID, sbLifecycleTypes)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var wallet *string
		var asset, netText string
		if err := rows.Scan(&wallet, &asset, &netText); err != nil {
			return err
		}
		net, err := parseBig(netText)
		if err != nil {
			return err
		}
		if net.Sign() != 0 {
			w := ""
			if wallet != nil {
				w = *wallet
			}
			r.add(MismatchKindSBLocked,
				fmt.Sprintf("wallet=%s asset=%s account_type=player_locked_bonus", w, asset),
				"sportsbook_locked_bonus_net=0", "sportsbook_locked_bonus_net="+net.String())
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------------
// (b) §8.2 per-bet netting.

type sbBet struct {
	id        uuid.UUID
	status    string
	stake     *big.Int
	asset     string
	walletID  uuid.UUID
	placement uuid.UUID
}

func sbLoadBets(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]sbBet, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, status, stake_amount::text, asset_code, wallet_id, ledger_transaction_id
		  FROM sportsbook_bets WHERE tenant_id = $1 ORDER BY id`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sbBet
	for rows.Next() {
		var b sbBet
		var stake string
		if err := rows.Scan(&b.id, &b.status, &stake, &b.asset, &b.walletID, &b.placement); err != nil {
			return nil, err
		}
		if b.stake, err = parseBig(stake); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

type sbPlacementEntry struct {
	accountID   uuid.UUID
	direction   string
	amount      *big.Int
	accountType string
	walletID    *uuid.UUID
	asset       string
}

type sbPlacement struct {
	txType      string
	correlation *uuid.UUID
	entries     []sbPlacementEntry
}

func sbLoadPlacements(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[uuid.UUID]*sbPlacement, error) {
	rows, err := tx.Query(ctx, `
		SELECT b.id, t.transaction_type, t.correlation_id,
		       e.ledger_account_id, e.direction, e.amount::text, la.account_type, la.wallet_id, la.asset_code
		  FROM sportsbook_bets b
		  JOIN ledger_transactions t ON t.id = b.ledger_transaction_id
		  LEFT JOIN ledger_entries e ON e.ledger_transaction_id = t.id
		  LEFT JOIN ledger_accounts la ON la.id = e.ledger_account_id
		 WHERE b.tenant_id = $1`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]*sbPlacement{}
	for rows.Next() {
		var betID uuid.UUID
		var txType string
		var corr, accountID, walletID *uuid.UUID
		var direction, amount, accountType, asset *string
		if err := rows.Scan(&betID, &txType, &corr, &accountID, &direction, &amount, &accountType, &walletID, &asset); err != nil {
			return nil, err
		}
		p := out[betID]
		if p == nil {
			p = &sbPlacement{txType: txType, correlation: corr}
			out[betID] = p
		}
		if accountID == nil || direction == nil || amount == nil || accountType == nil || asset == nil {
			continue
		}
		amt, err := parseBig(*amount)
		if err != nil {
			return nil, err
		}
		p.entries = append(p.entries, sbPlacementEntry{
			accountID: *accountID, direction: *direction, amount: amt,
			accountType: *accountType, walletID: walletID, asset: *asset,
		})
	}
	return out, rows.Err()
}

// sbPlacementAccounts is ADR 0088 §2.2 steps 1-2: CASH/LOCKED derived from
// the bet's own placement posting. ok=false (with a reason) means the
// placement itself is malformed.
func sbPlacementAccounts(b sbBet, p *sbPlacement) (cash, locked uuid.UUID, reason string) {
	if p == nil {
		return uuid.Nil, uuid.Nil, "placement ledger transaction not found"
	}
	if p.txType != string(ledger.TxSportsbookBet) || p.correlation == nil || *p.correlation != b.id {
		return uuid.Nil, uuid.Nil, fmt.Sprintf("placement transaction type=%s correlation mismatch", p.txType)
	}
	if len(p.entries) != 2 {
		return uuid.Nil, uuid.Nil, fmt.Sprintf("placement has %d entries, want 2", len(p.entries))
	}
	for _, e := range p.entries {
		if e.asset != b.asset || e.walletID == nil || *e.walletID != b.walletID || e.amount.Cmp(b.stake) != 0 {
			return uuid.Nil, uuid.Nil, "placement entry asset/wallet/amount does not match the bet"
		}
		switch {
		case e.direction == string(ledger.Debit) && e.accountType == string(ledger.AccountPlayerCash):
			cash = e.accountID
		case e.direction == string(ledger.Credit) && e.accountType == string(ledger.AccountPlayerLockedCash):
			locked = e.accountID
		}
	}
	if cash == uuid.Nil || locked == uuid.Nil {
		return uuid.Nil, uuid.Nil, "placement is not Dr player_cash / Cr player_locked_cash"
	}
	return cash, locked, ""
}

type sbNetRow struct {
	accountID   uuid.UUID
	accountType string
	asset       string
	houseScoped bool // wallet_id IS NULL
	net         *big.Int
}

func sbLoadBetNets(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[uuid.UUID][]sbNetRow, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.correlation_id, e.ledger_account_id, la.account_type, la.asset_code, la.wallet_id IS NULL,
		       SUM(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END)::text
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		  JOIN ledger_accounts la ON la.id = e.ledger_account_id
		 WHERE t.tenant_id = $1 AND t.transaction_type = ANY($2)
		   AND t.correlation_id IN (SELECT id FROM sportsbook_bets WHERE tenant_id = $1)
		 GROUP BY t.correlation_id, e.ledger_account_id, la.account_type, la.asset_code, la.wallet_id IS NULL`,
		tenantID, sbLifecycleTypes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]sbNetRow{}
	for rows.Next() {
		var betID uuid.UUID
		var n sbNetRow
		var net string
		if err := rows.Scan(&betID, &n.accountID, &n.accountType, &n.asset, &n.houseScoped, &net); err != nil {
			return nil, err
		}
		if n.net, err = parseBig(net); err != nil {
			return nil, err
		}
		out[betID] = append(out[betID], n)
	}
	return out, rows.Err()
}

// sbCurrentSettlement is a bet's latest un-reversed settlement history row
// and the CASH payout actually posted by its ledger transaction.
type sbCurrentSettlement struct {
	count        int
	outcome      string
	payout       *big.Int
	ledgerTxID   uuid.UUID
	ledgerPayout *big.Int // Σ credit on the bet's CASH in that transaction
	reversed     bool     // the ledger holds a rollback reversing it
}

func sbLoadCurrentSettlements(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[uuid.UUID]*sbCurrentSettlement, error) {
	rows, err := tx.Query(ctx, `
		SELECT s.bet_id, s.outcome, s.payout_amount::text, s.ledger_transaction_id,
		       COALESCE((SELECT SUM(e.amount) FROM ledger_entries e
		                  JOIN ledger_entries pe ON pe.ledger_account_id = e.ledger_account_id
		                                        AND pe.direction = 'debit'
		                  JOIN sportsbook_bets b ON b.ledger_transaction_id = pe.ledger_transaction_id
		                 WHERE e.ledger_transaction_id = s.ledger_transaction_id
		                   AND e.direction = 'credit' AND b.id = s.bet_id), 0)::text,
		       EXISTS (SELECT 1 FROM ledger_transactions rt
		                WHERE rt.reverses_transaction_id = s.ledger_transaction_id)
		  FROM sportsbook_bet_settlements s
		 WHERE s.tenant_id = $1 AND s.event_kind = 'settlement'
		   AND NOT EXISTS (SELECT 1 FROM sportsbook_bet_settlements r
		                    WHERE r.event_kind = 'rollback' AND r.reverses_settlement_id = s.id)`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]*sbCurrentSettlement{}
	for rows.Next() {
		var betID, ltx uuid.UUID
		var outcome *string
		var payout *string
		var ledgerPayout string
		var reversed bool
		if err := rows.Scan(&betID, &outcome, &payout, &ltx, &ledgerPayout, &reversed); err != nil {
			return nil, err
		}
		c := out[betID]
		if c == nil {
			c = &sbCurrentSettlement{}
			out[betID] = c
		}
		c.count++
		c.ledgerTxID, c.reversed = ltx, reversed
		if outcome != nil {
			c.outcome = *outcome
		}
		c.payout = new(big.Int)
		if payout != nil {
			if c.payout, err = parseBig(*payout); err != nil {
				return nil, err
			}
		}
		if c.ledgerPayout, err = parseBig(ledgerPayout); err != nil {
			return nil, err
		}
	}
	return out, rows.Err()
}

func sbCheckBetNets(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	bets, err := sbLoadBets(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	placements, err := sbLoadPlacements(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	nets, err := sbLoadBetNets(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	current, err := sbLoadCurrentSettlements(ctx, tx, tenantID)
	if err != nil {
		return err
	}

	for _, b := range bets {
		key := "bet=" + b.id.String()
		cashID, lockedID, reason := sbPlacementAccounts(b, placements[b.id])
		if reason != "" {
			r.add(MismatchKindSBBetNet, key+" check=placement", "well-formed placement (ADR 0088 §2.2)", reason)
			continue
		}

		cash, locked, house, other := new(big.Int), new(big.Int), new(big.Int), new(big.Int)
		var otherAccounts []string
		for _, n := range nets[b.id] {
			switch {
			case n.accountID == cashID:
				cash.Add(cash, n.net)
			case n.accountID == lockedID:
				locked.Add(locked, n.net)
			case n.accountType == string(ledger.AccountHouseGaming) && n.houseScoped && n.asset == b.asset:
				house.Add(house, n.net)
			default:
				if n.net.Sign() != 0 {
					other.Add(other, new(big.Int).Abs(n.net))
					otherAccounts = append(otherAccounts, n.accountID.String()+"("+n.accountType+")="+n.net.String())
				}
			}
		}
		if other.Sign() != 0 {
			sort.Strings(otherAccounts)
			r.add(MismatchKindSBBetNet, key+" check=foreign_accounts",
				"no net on accounts outside CASH/LOCKED/HOUSE", strings.Join(otherAccounts, ","))
		}

		s := b.stake
		var wantCash, wantLocked, wantHouse *big.Int
		cur := current[b.id]
		switch b.status {
		case sbBetStatusOpen:
			wantCash, wantLocked, wantHouse = new(big.Int).Neg(s), new(big.Int).Set(s), new(big.Int)
		case sbBetStatusLost:
			wantCash, wantLocked, wantHouse = new(big.Int).Neg(s), new(big.Int), new(big.Int).Set(s)
		case sbBetStatusWon:
			if cur == nil || cur.count != 1 || cur.outcome != "won" {
				r.add(MismatchKindSBBetNet, key+" check=payout",
					"exactly one un-reversed won settlement row", fmt.Sprintf("current settlement rows=%d", sbCount(cur)))
				continue
			}
			p := cur.payout
			wantCash = new(big.Int).Sub(p, s)
			wantLocked = new(big.Int)
			wantHouse = new(big.Int).Sub(s, p)
			if cur.reversed || cur.ledgerPayout.Cmp(p) != 0 {
				r.add(MismatchKindSBBetNet, key+" check=payout",
					fmt.Sprintf("un-reversed ledger payout=%s (settlement row payout_amount)", p),
					fmt.Sprintf("ledger payout=%s reversed_in_ledger=%t (tx %s)", cur.ledgerPayout, cur.reversed, cur.ledgerTxID))
			}
		case sbBetStatusVoid:
			wantCash, wantLocked, wantHouse = new(big.Int), new(big.Int), new(big.Int)
		default:
			r.add(MismatchKindSBBetNet, key+" check=status", "a known bet status", b.status)
			continue
		}
		if cash.Cmp(wantCash) != 0 || locked.Cmp(wantLocked) != 0 || house.Cmp(wantHouse) != 0 {
			r.add(MismatchKindSBBetNet, key+" check=end_state status="+b.status,
				fmt.Sprintf("cash=%s locked=%s house=%s", wantCash, wantLocked, wantHouse),
				fmt.Sprintf("cash=%s locked=%s house=%s", cash, locked, house))
		}
	}
	return nil
}

func sbCount(c *sbCurrentSettlement) int {
	if c == nil {
		return 0
	}
	return c.count
}

// ---------------------------------------------------------------------
// (c) §8.3 two-way orphan check, causation, status.

// sbCheckOrphanLedger: every W1 ledger transaction (and every tombstone
// in the sportsbook_settlement: key namespace) has exactly one history
// row referencing it, whose bet_id = correlation_id and whose kind
// matches the type.
func sbCheckOrphanLedger(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	rows, err := tx.Query(ctx, `
		SELECT t.id, t.transaction_type, t.correlation_id,
		       count(h.id), min(h.bet_id::text), min(h.event_kind)
		  FROM ledger_transactions t
		  LEFT JOIN sportsbook_bet_settlements h ON h.ledger_transaction_id = t.id
		 WHERE t.tenant_id = $1
		   AND (t.transaction_type IN ('sportsbook_settlement', 'sportsbook_void', 'sportsbook_rollback')
		        OR (t.transaction_type = 'tombstone' AND t.idempotency_key LIKE 'sportsbook\_settlement:%'))
		 GROUP BY t.id, t.transaction_type, t.correlation_id
		 ORDER BY t.id`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, corr uuid.UUID
		var txType string
		var n int
		var betID, kind *string
		if err := rows.Scan(&id, &txType, &corr, &n, &betID, &kind); err != nil {
			return err
		}
		key := "ledger_transaction=" + id.String()
		want := fmt.Sprintf("one history row kind=%s bet=%s", sbKindForTxType[txType], corr)
		switch {
		case n != 1:
			r.add(MismatchKindSBOrphanLedger, key, want, fmt.Sprintf("%d history rows (type=%s)", n, txType))
		case betID == nil || *betID != corr.String() || kind == nil || *kind != sbKindForTxType[txType]:
			r.add(MismatchKindSBOrphanLedger, key, want,
				fmt.Sprintf("history row kind=%s bet=%s", deref(kind), deref(betID)))
		}
	}
	return rows.Err()
}

func deref(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

// sbCheckOrphanHistory: every history row's ledger transaction exists
// with the matching type and correlation_id; its causation follows ADR
// 0088 §2.1; a rollback's ledger transaction reverses its settlement's.
func sbCheckOrphanHistory(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	rows, err := tx.Query(ctx, `
		SELECT h.id, h.bet_id, h.event_kind, h.generation, h.ledger_transaction_id,
		       t.id IS NOT NULL, t.transaction_type, t.correlation_id, t.causation_id, t.reverses_transaction_id,
		       h.causation_record_id, c.bet_id, c.event_kind, c.generation, c.ledger_transaction_id,
		       h.reverses_settlement_id, rs.bet_id, rs.event_kind, rs.generation, rs.ledger_transaction_id
		  FROM sportsbook_bet_settlements h
		  LEFT JOIN ledger_transactions t ON t.id = h.ledger_transaction_id
		  LEFT JOIN sportsbook_bet_settlements c ON c.id = h.causation_record_id
		  LEFT JOIN sportsbook_bet_settlements rs ON rs.id = h.reverses_settlement_id
		 WHERE h.tenant_id = $1
		 ORDER BY h.id`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id, betID, ltxID             uuid.UUID
			kind                         string
			gen                          *int32
			txFound                      bool
			txType                       *string
			txCorr, txCause, txReverses  *uuid.UUID
			causeRec, causeBet, causeLtx *uuid.UUID
			causeKind                    *string
			causeGen                     *int32
			revRec, revBet, revLtx       *uuid.UUID
			revKind                      *string
			revGen                       *int32
		)
		if err := rows.Scan(&id, &betID, &kind, &gen, &ltxID,
			&txFound, &txType, &txCorr, &txCause, &txReverses,
			&causeRec, &causeBet, &causeKind, &causeGen, &causeLtx,
			&revRec, &revBet, &revKind, &revGen, &revLtx); err != nil {
			return err
		}
		key := "history=" + id.String() + " bet=" + betID.String()

		wantType := sbTxTypeForKind(kind)
		if !txFound || txType == nil || *txType != wantType || txCorr == nil || *txCorr != betID {
			r.add(MismatchKindSBOrphanHistory, key+" check=ledger_transaction",
				fmt.Sprintf("ledger transaction %s type=%s correlation=%s", ltxID, wantType, betID),
				fmt.Sprintf("found=%t type=%s correlation=%s", txFound, deref(txType), uuidOrNull(txCorr)))
			continue
		}

		// Causation (ADR 0088 §2.1, §8.3).
		var wantCause *uuid.UUID
		causeProblem := ""
		switch {
		case kind == "settlement" && gen != nil && *gen > 1:
			if causeRec == nil || causeBet == nil || *causeBet != betID || causeKind == nil ||
				(*causeKind != "rollback" && *causeKind != "tombstone") || causeGen == nil || *causeGen != *gen-1 {
				causeProblem = fmt.Sprintf("re-settlement g=%d must cite this bet's g-1 rollback/tombstone row", *gen)
			} else {
				wantCause = causeLtx
			}
		case kind == "void" && causeRec != nil:
			if causeBet == nil || *causeBet != betID || causeKind == nil || *causeKind != "rollback" {
				causeProblem = "a composed void must cite this bet's rollback row"
			} else {
				wantCause = causeLtx
			}
		case causeRec != nil:
			causeProblem = "causation_record_id not permitted for this event"
		}
		if causeProblem != "" {
			r.add(MismatchKindSBOrphanHistory, key+" check=causation_record", causeProblem,
				fmt.Sprintf("causation_record_id=%s", uuidOrNull(causeRec)))
		} else if !uuidPtrEqual(wantCause, txCause) {
			r.add(MismatchKindSBOrphanHistory, key+" check=causation",
				"ledger causation_id="+uuidOrNull(wantCause), "ledger causation_id="+uuidOrNull(txCause))
		}

		// A rollback's ledger transaction reverses its settlement's.
		if kind == "rollback" {
			if revRec == nil || revBet == nil || *revBet != betID || revKind == nil || *revKind != "settlement" ||
				revGen == nil || gen == nil || *revGen != *gen || !uuidPtrEqual(revLtx, txReverses) {
				r.add(MismatchKindSBOrphanHistory, key+" check=reverses",
					"ledger reverses_transaction_id="+uuidOrNull(revLtx)+" (the reversed settlement's)",
					"ledger reverses_transaction_id="+uuidOrNull(txReverses))
			}
		} else if txReverses != nil {
			r.add(MismatchKindSBOrphanHistory, key+" check=reverses",
				"ledger reverses_transaction_id=<null>", "ledger reverses_transaction_id="+txReverses.String())
		}
	}
	return rows.Err()
}

func uuidOrNull(u *uuid.UUID) string {
	if u == nil {
		return "<null>"
	}
	return u.String()
}

func uuidPtrEqual(a, b *uuid.UUID) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// sbCheckStatus: sportsbook_bets.status equals the status derived from
// history exactly as trigger T-2 derives it (INV-SB-SETTLE-3).
func sbCheckStatus(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) error {
	rows, err := tx.Query(ctx, `
		SELECT b.id, b.status,
		       CASE WHEN EXISTS (SELECT 1 FROM sportsbook_bet_settlements v
		                          WHERE v.bet_id = b.id AND v.event_kind = 'void') THEN 'void'
		            ELSE COALESCE((SELECT 'settled_' || s.outcome
		                             FROM sportsbook_bet_settlements s
		                            WHERE s.bet_id = b.id AND s.event_kind = 'settlement'
		                              AND NOT EXISTS (SELECT 1 FROM sportsbook_bet_settlements rb
		                                               WHERE rb.event_kind = 'rollback' AND rb.reverses_settlement_id = s.id)
		                            ORDER BY s.generation DESC LIMIT 1), 'open')
		       END
		  FROM sportsbook_bets b
		 WHERE b.tenant_id = $1
		 ORDER BY b.id`, tenantID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var status, derived string
		if err := rows.Scan(&id, &status, &derived); err != nil {
			return err
		}
		if status != derived {
			r.add(MismatchKindSBStatus, "bet="+id.String(), "history_derived_status="+derived, "status="+status)
		}
	}
	return rows.Err()
}

// ---------------------------------------------------------------------
// (d) §8.4 statement match - MOCK source today.

type sbStatementView struct {
	outcome string
	payout  *big.Int
	asset   string
}

func sbStatementKey(betID uuid.UUID, void bool, generation *int) string {
	if void {
		return "bet=" + betID.String() + " line=void"
	}
	if generation == nil {
		return "bet=" + betID.String() + " line=settlement#<null>"
	}
	return "bet=" + betID.String() + " line=settlement#" + strconv.Itoa(*generation)
}

// sbLedgerStatementView is the platform side of the match, derived from
// the LEDGER (not the history table the mock renders from): every
// un-reversed sportsbook_settlement (generation from its server-composed
// idempotency key, payout = Σ credits to player_cash) and every
// sportsbook_void, per bet.
func sbLedgerStatementView(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[string]sbStatementView, error) {
	rows, err := tx.Query(ctx, `
		SELECT t.correlation_id, t.transaction_type, t.idempotency_key, COALESCE(b.asset_code, ''),
		       COALESCE((SELECT SUM(e.amount) FROM ledger_entries e
		                   JOIN ledger_accounts la ON la.id = e.ledger_account_id
		                  WHERE e.ledger_transaction_id = t.id AND e.direction = 'credit'
		                    AND la.account_type = 'player_cash'), 0)::text
		  FROM ledger_transactions t
		  LEFT JOIN sportsbook_bets b ON b.id = t.correlation_id
		 WHERE t.tenant_id = $1
		   AND ((t.transaction_type = 'sportsbook_settlement'
		         AND NOT EXISTS (SELECT 1 FROM ledger_transactions rt
		                          WHERE rt.reverses_transaction_id = t.id AND rt.transaction_type = 'sportsbook_rollback'))
		        OR t.transaction_type = 'sportsbook_void')`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]sbStatementView{}
	for rows.Next() {
		var betID uuid.UUID
		var txType, idemKey, asset, payoutText string
		if err := rows.Scan(&betID, &txType, &idemKey, &asset, &payoutText); err != nil {
			return nil, err
		}
		payout, err := parseBig(payoutText)
		if err != nil {
			return nil, err
		}
		if txType == string(ledger.TxSportsbookVoid) {
			out[sbStatementKey(betID, true, nil)] = sbStatementView{payout: new(big.Int), asset: asset}
			continue
		}
		var gen *int
		if g, ok := sbGenerationFromKey(idemKey, betID); ok {
			gen = &g
		}
		outcome := "lost"
		if payout.Sign() > 0 {
			outcome = "won"
		}
		out[sbStatementKey(betID, false, gen)] = sbStatementView{outcome: outcome, payout: payout, asset: asset}
	}
	return out, rows.Err()
}

// sbGenerationFromKey parses g from "sportsbook_settlement:<bet_id>#<g>"
// (ADR 0088 §4.2).
func sbGenerationFromKey(key string, betID uuid.UUID) (int, bool) {
	prefix := sbSettlementKeyPrefix + betID.String() + "#"
	if !strings.HasPrefix(key, prefix) {
		return 0, false
	}
	g, err := strconv.Atoi(strings.TrimPrefix(key, prefix))
	if err != nil || g < 1 {
		return 0, false
	}
	return g, true
}

func sbCheckStatement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, sourceLabel string, lines []statement.SportsbookSettlementLine, r *sbRecorder) error {
	ledgerView, err := sbLedgerStatementView(ctx, tx, tenantID)
	if err != nil {
		return err
	}
	label := "[" + sourceLabel + "] "

	stated := map[string]sbStatementView{}
	for _, l := range lines {
		key := sbStatementKey(l.BetID, l.Void, l.Generation)
		if _, dup := stated[key]; dup {
			r.add(MismatchKindSBMockStatement, "statement: "+key, "one statement line", label+"duplicate statement line")
			continue
		}
		v := sbStatementView{outcome: l.Outcome, payout: big.NewInt(l.PayoutAmount), asset: l.AssetCode}
		if l.Void {
			v.outcome = ""
		}
		stated[key] = v
	}

	keys := make([]string, 0, len(stated)+len(ledgerView))
	for k := range stated {
		keys = append(keys, k)
	}
	for k := range ledgerView {
		if _, ok := stated[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, inStatement := stated[k]
		l, inLedger := ledgerView[k]
		switch {
		case !inLedger:
			r.add(MismatchKindSBMockStatement, "statement: "+k, "ledger: no matching posting", label+s.render())
		case !inStatement:
			r.add(MismatchKindSBMockStatement, "statement: "+k, "ledger: "+l.render(), label+"no statement line")
		case s.outcome != l.outcome || s.payout.Cmp(l.payout) != 0 || s.asset != l.asset:
			r.add(MismatchKindSBMockStatement, "statement: "+k, "ledger: "+l.render(), label+s.render())
		}
	}
	return nil
}

func (v sbStatementView) render() string {
	return fmt.Sprintf("outcome=%s payout=%s asset=%s", v.outcome, v.payout, v.asset)
}
