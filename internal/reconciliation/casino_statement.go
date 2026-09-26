package reconciliation

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// Casino statement reconciliation stream (Stage 10.3 W3a, CAS-RECON-STMT-1;
// docs/plans/stage-10.3-planning/02-casino-financial-analysis.md §2.5,
// §2.9, §2.12-§2.15; reconciliation-model.md §2.3).
//
// The counterparty half of the wallet <-> casino provider stream: it
// matches the ledger against a casino statement obtained through the
// provider-neutral statement.CasinoStatementSource, two ways, zero
// tolerance:
//
//	key match     every statement line and every casino_bet/_win/_rollback
//	              carrying a provider reference, by (provider_id,
//	              provider_tx_id): present on both sides with the same kind,
//	              amount, asset, round and (rollback) original reference. A
//	              duplicate statement line is a mismatch. A statement
//	              rollback whose original the ledger holds only as a casino
//	              tombstone is a MATCH (the provider lists its rollback; the
//	              ledger shows "rollback of an unseen original" as the
//	              tombstone) - the one pattern that looks one-sided but is
//	              not a finding.
//	totals match  when the source reports totals: per (provider, asset),
//	              the net house_gaming movement (credits - debits) over the
//	              casino transactions carrying that provider_id equals the
//	              statement's GGR. Every ledger (provider, asset) must have a
//	              statement total; a statement total with no ledger movement
//	              is compared against zero.
//
// HONESTY OF THE RESULT. The only source wired today is
// casino.MockStatementSource, which renders its lines and totals from the
// same casino ledger rows this stream reads. Against it the match is
// TAUTOLOGICAL: a clean run proves the plumbing (the stream, the sweep
// wiring, the advisory lock, the audit, the evidence rows) and nothing
// about agreement with any provider. The matching logic itself is proven
// by the divergent statement sources in casino_statement_integration_test.go,
// which exercise every detection path. Real statement matching is PROVIDER
// DEPENDENT: it needs a contracted provider's statement format, append-only
// statement storage and per-tenant credentials, all NOT IMPLEMENTED.
//
// Period semantics: like sportsbook_settlement, the MOCK source is
// all-time and the stream compares the whole casino population on every
// run; periodStart/periodEnd are recorded on the run row and passed to the
// source, not used as a filter. A real, period-bounded source needs a
// TIMING window (a key on one side only is a mismatch only if also absent
// from the adjacent period's statement) - never an amount tolerance; its
// exact cut-off is PROVIDER DEPENDENT and NOT IMPLEMENTED here.
//
// Detection only. The stream inserts one immutable reconciliation_runs row
// plus zero or more reconciliation_mismatches rows (persistRun) and writes
// nothing else - never the ledger, a projection, a round, a session, a
// capability or the rejection record. Every mismatch is a P1; none is ever
// auto-corrected (paper 02 §2.9: a statement divergence is never "fixed"
// by posting to match the provider). Findings are state-type: re-detected
// every run until the underlying condition is resolved (ADR 0023 §4).
//
// Snapshot: the statement source and the ledger are read in separate
// statements, so the run MUST see one snapshot - otherwise a casino
// posting committed between the two reads would surface as a false P1.
// RunCasinoStatement therefore refuses (fails closed) unless its
// transaction is REPEATABLE READ or SERIALIZABLE; the sweep opens it with
// db.Pool.WithTenantSnapshot.
//
// Deviation from paper 02 §2.5, recorded by its author (ledger-finance):
// the win amount is the house_gaming debit of the win transaction (the
// payout the provider asserted), not the player-side leg sum. The two are
// equal for a direct-cash win, but a locked-cash or locked-bonus win also
// carries the lock-release legs on the player side (payout + released
// stake), which is not the provider's amount. The bet amount is the stake
// (debits on the player's spendable accounts), as specified.

// StreamCasinoStatement is the stream name on reconciliation_runs.
const StreamCasinoStatement Stream = "casino_statement"

// MismatchKindCasMockStatement is a divergence between the casino
// statement source and the ledger (migration 0098). It is named "mock"
// because the only source today is the MOCK; the first real source adds
// its own kind.
const MismatchKindCasMockStatement MismatchKind = "cas_mock_statement_mismatch"

// CasinoStatementInfo describes one run's statement, for the run's audit
// metadata. It is never a mismatch.
type CasinoStatementInfo struct {
	Lines             int
	Totals            int
	TotalsProvided    bool
	TombstonePairings int
}

// AuditMetadata renders i for the run's audit record.
func (i CasinoStatementInfo) AuditMetadata() map[string]any {
	return map[string]any{
		"statement_lines":             i.Lines,
		"statement_totals":            i.Totals,
		"statement_totals_provided":   i.TotalsProvided,
		"rollback_tombstone_pairings": i.TombstonePairings,
	}
}

// RunCasinoStatement runs the casino_statement stream for tenantID inside
// tx (db.Pool.WithTenantSnapshot(tenantID) - REPEATABLE READ is required
// and enforced), recording exactly one Run and its mismatches, atomically -
// the RunLedgerVsProjection contract. A nil source fails the run closed
// rather than skipping the match.
func RunCasinoStatement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, periodStart, periodEnd time.Time, source statement.CasinoStatementSource) (Run, []Mismatch, CasinoStatementInfo, error) {
	if source == nil {
		return Run{}, nil, CasinoStatementInfo{}, fmt.Errorf("reconciliation: %s requires a casino statement source", StreamCasinoStatement)
	}
	if err := requireSnapshotIsolation(ctx, tx); err != nil {
		return Run{}, nil, CasinoStatementInfo{}, err
	}
	run := Run{
		ID: uuid.New(), TenantID: tenantID, Stream: StreamCasinoStatement,
		PeriodStart: periodStart, PeriodEnd: periodEnd, RunAt: time.Now().UTC(),
	}
	r := &sbRecorder{tenantID: tenantID}

	lines, totals, err := source.Statement(ctx, tx, tenantID, periodStart, periodEnd)
	if err != nil {
		return Run{}, nil, CasinoStatementInfo{}, fmt.Errorf("reconciliation: casino statement source %q: %w", source.Label(), err)
	}
	info := CasinoStatementInfo{Lines: len(lines), Totals: len(totals), TotalsProvided: len(totals) > 0}

	footprint, err := casHasFootprint(ctx, tx, tenantID)
	if err != nil {
		return Run{}, nil, CasinoStatementInfo{}, fmt.Errorf("reconciliation: casino footprint: %w", err)
	}
	if footprint || len(lines) > 0 || len(totals) > 0 {
		label := "[" + source.Label() + "] "
		if info.TombstonePairings, err = casStmtMatchKeys(ctx, tx, tenantID, label, lines, r); err != nil {
			return Run{}, nil, CasinoStatementInfo{}, fmt.Errorf("reconciliation: casino statement key match: %w", err)
		}
		if info.TotalsProvided {
			if err := casStmtMatchTotals(ctx, tx, tenantID, label, totals, r); err != nil {
				return Run{}, nil, CasinoStatementInfo{}, fmt.Errorf("reconciliation: casino statement totals match: %w", err)
			}
		}
	}

	mismatches := r.mismatches
	if len(mismatches) > 0 {
		run.Status = StatusMismatchesFound
	} else {
		run.Status = StatusClean
	}
	if err := persistRun(ctx, tx, run, mismatches); err != nil {
		return Run{}, nil, CasinoStatementInfo{}, err
	}
	return run, mismatches, info, nil
}

// requireSnapshotIsolation fails closed unless tx reads one snapshot for
// its whole life (see the file comment's "Snapshot" paragraph).
func requireSnapshotIsolation(ctx context.Context, tx pgx.Tx) error {
	var level string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&level); err != nil {
		return fmt.Errorf("reconciliation: read transaction isolation: %w", err)
	}
	if level != "repeatable read" && level != "serializable" {
		return fmt.Errorf("reconciliation: %s requires a REPEATABLE READ transaction (db.Pool.WithTenantSnapshot), got %q", StreamCasinoStatement, level)
	}
	return nil
}

// casStmtView is one side's view of one (provider_id, provider_tx_id) key.
type casStmtView struct {
	kind     string
	amount   *big.Int
	asset    string
	original string
	// round is rendered for the statement side; the ledger side compares
	// the round through its correlation id.
	round string
}

func (v casStmtView) render() string {
	s := fmt.Sprintf("kind=%s amount=%s asset=%s", v.kind, v.amount, v.asset)
	if v.original != "" {
		s += " original=" + v.original
	}
	if v.round != "" {
		s += " round=" + v.round
	}
	return s
}

type casStmtLedgerRow struct {
	view        casStmtView
	correlation uuid.UUID
}

func casStmtKey(provider, ref string) string {
	return "provider=" + provider + " provider_tx_id=" + ref
}

// casStmtLedgerView is the platform side of the key match: every
// casino_bet/_win/_rollback carrying a provider reference (one without is
// C2's cas_posting_shape_mismatch and cannot be keyed). Amount: a bet's
// stake (debits on the player's spendable accounts), a win's payout (its
// house_gaming debit), a rollback's original's amount; an unlinked
// rollback's amount is left nil so it never matches.
func casStmtLedgerView(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, r *sbRecorder) (map[string]casStmtLedgerRow, error) {
	rows, err := tx.Query(ctx, `
		WITH t AS (
		    SELECT t.id, t.provider_id, t.provider_tx_id, t.transaction_type, t.correlation_id, t.reverses_transaction_id,
		           COALESCE((SELECT SUM(e.amount) FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		                      WHERE e.ledger_transaction_id = t.id AND e.direction = 'debit' AND la.wallet_id IS NOT NULL
		                        AND la.account_type IN ('player_cash', 'player_bonus')), 0) AS stake,
		           COALESCE((SELECT SUM(e.amount) FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		                      WHERE e.ledger_transaction_id = t.id AND e.direction = 'debit'
		                        AND la.account_type = 'house_gaming'), 0) AS payout,
		           (SELECT CASE WHEN count(DISTINCT la.asset_code) = 1 THEN min(la.asset_code)
		                        WHEN count(DISTINCT la.asset_code) = 0 THEN '<none>' ELSE '<multiple>' END
		              FROM ledger_entries e JOIN ledger_accounts la ON la.id = e.ledger_account_id
		             WHERE e.ledger_transaction_id = t.id) AS asset
		      FROM ledger_transactions t
		     WHERE t.tenant_id = $1 AND t.transaction_type = ANY($2)
		       AND t.provider_id IS NOT NULL AND t.provider_tx_id IS NOT NULL)
		SELECT t.provider_id, t.provider_tx_id, t.transaction_type, t.correlation_id, t.asset,
		       t.stake::text, t.payout::text,
		       COALESCE(o.provider_tx_id, ''), COALESCE(o.transaction_type, ''),
		       COALESCE(o.stake, 0)::text, COALESCE(o.payout, 0)::text
		  FROM t
		  LEFT JOIN t o ON o.id = t.reverses_transaction_id
		 ORDER BY t.provider_id, t.provider_tx_id, t.id`, tenantID, casinoTxTypes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]casStmtLedgerRow{}
	for rows.Next() {
		var provider, ref, txType, asset, stake, payout, origRef, origType, origStake, origPayout string
		var corr uuid.UUID
		if err := rows.Scan(&provider, &ref, &txType, &corr, &asset, &stake, &payout, &origRef, &origType, &origStake, &origPayout); err != nil {
			return nil, err
		}
		v := casStmtView{asset: asset}
		amountOf := func(ty, st, po string) (*big.Int, error) {
			switch ty {
			case "casino_bet":
				return parseBig(st)
			case "casino_win":
				return parseBig(po)
			}
			return nil, nil
		}
		switch txType {
		case "casino_bet":
			v.kind = statement.CasinoLineBet
		case "casino_win":
			v.kind = statement.CasinoLineWin
		default:
			v.kind = statement.CasinoLineRollback
			v.original = origRef
			if origRef == "" {
				v.original = "<unlinked>"
			}
		}
		if txType == "casino_rollback" {
			v.amount, err = amountOf(origType, origStake, origPayout)
		} else {
			v.amount, err = amountOf(txType, stake, payout)
		}
		if err != nil {
			return nil, err
		}
		key := casStmtKey(provider, ref)
		if _, dup := out[key]; dup {
			// Structurally impossible under the (tenant, provider_id,
			// provider_tx_id) unique index; a backstop, never silent.
			r.add(MismatchKindCasMockStatement, "ledger: "+key+" check=key",
				"one ledger posting per provider reference", "duplicate ledger-side casino posting")
			continue
		}
		out[key] = casStmtLedgerRow{view: v, correlation: corr}
	}
	return out, rows.Err()
}

// casStmtTombstones is the set of (provider_id, provider_tx_id) keys the
// ledger holds as a CASINO tombstone (casinoProvidersCTE: the key format
// alone cannot tell a casino tombstone from a payments one, G-4).
func casStmtTombstones(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (map[string]bool, error) {
	rows, err := tx.Query(ctx, `
		WITH `+casinoProvidersCTE+`
		SELECT ts.provider_id, ts.provider_tx_id
		  FROM ledger_transactions ts
		 WHERE ts.tenant_id = $1 AND ts.transaction_type = 'tombstone'
		   AND ts.provider_id IN (SELECT provider_id FROM casino_providers)
		   AND ts.provider_tx_id IS NOT NULL`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var provider, ref string
		if err := rows.Scan(&provider, &ref); err != nil {
			return nil, err
		}
		out[casStmtKey(provider, ref)] = true
	}
	return out, rows.Err()
}

func casStmtMatchKeys(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, label string, lines []statement.CasinoStatementLine, r *sbRecorder) (int, error) {
	ledgerView, err := casStmtLedgerView(ctx, tx, tenantID, r)
	if err != nil {
		return 0, err
	}
	tombstones, err := casStmtTombstones(ctx, tx, tenantID)
	if err != nil {
		return 0, err
	}

	type stated struct {
		view     casStmtView
		provider string
	}
	statedView := map[string]stated{}
	for _, l := range lines {
		key := casStmtKey(l.ProviderID, l.ProviderTxID)
		if _, dup := statedView[key]; dup {
			r.add(MismatchKindCasMockStatement, "statement: "+key+" check=key", "one statement line per provider reference",
				label+"duplicate statement line")
			continue
		}
		statedView[key] = stated{provider: l.ProviderID, view: casStmtView{
			kind: l.Kind, amount: big.NewInt(l.Amount), asset: l.AssetCode,
			original: l.OriginalProviderTxID, round: l.RoundID,
		}}
	}

	keys := make([]string, 0, len(statedView)+len(ledgerView))
	for k := range statedView {
		keys = append(keys, k)
	}
	for k := range ledgerView {
		if _, ok := statedView[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	pairings := 0
	for _, k := range keys {
		s, inStatement := statedView[k]
		l, inLedger := ledgerView[k]
		switch {
		case !inLedger:
			// The one one-sided pattern that is NOT a finding: a provider
			// rollback whose original the ledger holds only as a casino
			// tombstone (rollback of an original never seen).
			if s.view.kind == statement.CasinoLineRollback && s.view.original != "" &&
				tombstones[casStmtKey(s.provider, s.view.original)] {
				pairings++
				continue
			}
			expected := "ledger: no matching casino posting"
			if tombstones[k] {
				// C7's counterparty confirmation: the provider still
				// counts an original the platform tombstoned.
				expected = "ledger: no posting (the reference is held only as a tombstone; late original counted by the provider)"
			}
			r.add(MismatchKindCasMockStatement, "statement: "+k+" check=key", expected, label+s.view.render())
		case !inStatement:
			r.add(MismatchKindCasMockStatement, "statement: "+k+" check=key", "ledger: "+l.view.render(), label+"no statement line")
		default:
			var diffs []string
			if s.view.kind != l.view.kind {
				diffs = append(diffs, "kind")
			}
			if l.view.amount == nil || s.view.amount.Cmp(l.view.amount) != 0 {
				diffs = append(diffs, "amount")
			}
			if s.view.asset != l.view.asset {
				diffs = append(diffs, "asset")
			}
			if s.view.original != l.view.original {
				diffs = append(diffs, "original")
			}
			if casRoundCorrelationID(tenantID, s.provider, s.view.round) != l.correlation {
				diffs = append(diffs, "round")
			}
			if len(diffs) > 0 {
				r.add(MismatchKindCasMockStatement, "statement: "+k+" check=key fields="+strings.Join(diffs, ","),
					"ledger: "+l.view.render()+" correlation="+l.correlation.String(), label+s.view.render())
			}
		}
	}
	return pairings, nil
}

func casStmtTotalKey(provider, asset string) string {
	return "provider=" + provider + " asset=" + asset
}

// casStmtMatchTotals: per (provider, asset), the net house_gaming movement
// of the provider's casino transactions equals the statement's GGR.
func casStmtMatchTotals(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, label string, totals []statement.CasinoStatementTotal, r *sbRecorder) error {
	rows, err := tx.Query(ctx, `
		SELECT t.provider_id, la.asset_code,
		       SUM(CASE WHEN e.direction = 'credit' THEN e.amount ELSE -e.amount END)::text
		  FROM ledger_entries e
		  JOIN ledger_transactions t ON t.id = e.ledger_transaction_id
		  JOIN ledger_accounts la ON la.id = e.ledger_account_id
		 WHERE t.tenant_id = $1 AND t.transaction_type = ANY($2)
		   AND t.provider_id IS NOT NULL AND la.account_type = 'house_gaming'
		 GROUP BY t.provider_id, la.asset_code`, tenantID, casinoTxTypes)
	if err != nil {
		return err
	}
	ledgerNet := map[string]*big.Int{}
	for rows.Next() {
		var provider, asset, net string
		if err := rows.Scan(&provider, &asset, &net); err != nil {
			rows.Close()
			return err
		}
		v, err := parseBig(net)
		if err != nil {
			rows.Close()
			return err
		}
		ledgerNet[casStmtTotalKey(provider, asset)] = v
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	stated := map[string]*big.Int{}
	for _, t := range totals {
		key := casStmtTotalKey(t.ProviderID, t.AssetCode)
		if _, dup := stated[key]; dup {
			r.add(MismatchKindCasMockStatement, "total: "+key+" check=total", "one statement total per provider and asset",
				label+"duplicate statement total")
			continue
		}
		if t.GGR == nil {
			r.add(MismatchKindCasMockStatement, "total: "+key+" check=total", "a stated GGR", label+"GGR=<nil>")
			stated[key] = nil
			continue
		}
		stated[key] = new(big.Int).Set(t.GGR)
	}

	keys := make([]string, 0, len(stated)+len(ledgerNet))
	for k := range stated {
		keys = append(keys, k)
	}
	for k := range ledgerNet {
		if _, ok := stated[k]; !ok {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, inStatement := stated[k]
		l, inLedger := ledgerNet[k]
		if !inLedger {
			l = new(big.Int)
		}
		switch {
		case !inStatement:
			r.add(MismatchKindCasMockStatement, "total: "+k+" check=total", "ledger: house_gaming_net="+l.String(), label+"no statement total")
		case s == nil:
			// Already recorded above.
		case s.Cmp(l) != 0:
			r.add(MismatchKindCasMockStatement, "total: "+k+" check=total", "ledger: house_gaming_net="+l.String(), label+"GGR="+s.String())
		}
	}
	return nil
}
