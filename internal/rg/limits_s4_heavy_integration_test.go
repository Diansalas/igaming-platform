//go:build integration

package rg

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// ADR 0115 S3 hard condition (b), measured at S4: a HEAVY player (at least 300,000 bet+win ledger rows in 30 days)
// under the 30-day and calendar-month wager / loss windows, with the production statement timeout. This is an
// EVIDENCE run, off by default (seeding 300,000 ledger transactions through the real triggers takes tens of
// minutes): RG_S4_HEAVY=1 enables it; RG_S4_HEAVY_ROWS overrides the number of bet transactions (and of win
// transactions) - the default 150,000 + 150,000 = 300,000 rows. A skip is NOT evidence. The recorded figures
// and the decision they support are in docs/plans/a6-rg-enforcement-record.md.
func s4_HeavyPlayerMeasurement(t *testing.T, w *s1World) {
	if os.Getenv("RG_S4_HEAVY") != "1" {
		t.Skip("RG_S4_HEAVY=1 not set: heavy-player evidence run skipped (a skip is not evidence)")
	}
	n := 150000
	if v := os.Getenv("RG_S4_HEAVY_ROWS"); v != "" {
		x, err := strconv.Atoi(v)
		if err != nil || x < 1 {
			t.Fatalf("RG_S4_HEAVY_ROWS: %v", err)
		}
		n = x
	}
	f := w.s4Fix(t, nil)
	w.activate(t, f, nil)
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling30d, strp("100000000000"))
	w.setLimit(t, f.tenantA, f.acctA1, ControlLoss, PeriodRolling30d, strp("100000000000"))
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodCalendarMonth, strp("100000000000"))
	w.setLimit(t, f.tenantA, f.acctA1, ControlWager, PeriodRolling24h, strp("100000000000"))

	start := time.Now()
	now := time.Now()
	w.bulkTyped(t, f.tenantA, f.acctA1, "casino_bet", n, now.Add(-29*24*time.Hour), now.Add(-time.Minute))
	w.bulkTyped(t, f.tenantA, f.acctA1, "casino_win", n, now.Add(-29*24*time.Hour), now.Add(-time.Minute))
	t.Logf("seeded %d bet + %d win ledger transactions in %s", n, n, time.Since(start).Round(time.Second))

	// ANALYZE so the planner sees the real distribution (the production autovacuum would).
	if _, err := w.owner.Raw().Exec(context.Background(), `ANALYZE ledger_entries; ANALYZE ledger_transactions; ANALYZE ledger_accounts`); err != nil {
		t.Fatal(err)
	}

	// MEASURE with a generous statement timeout (60 s) so the figure is the real cost of the scan, not a
	// timeout artefact; then report whether the PRODUCTION default would have held. A heavy player beyond the
	// default fails CLOSED in production (asserted below), which is safe but is an availability cost: the
	// rollup decision in docs/plans/a6-rg-enforcement-record.md rests on these figures.
	const runs = 7
	var durs []time.Duration
	for i := 0; i < runs; i++ {
		p := f.action(f.acctA1, ActionCasinoWager, 100)
		p.StatementTimeout = 60 * time.Second
		begin := time.Now()
		d, err := w.eval(f, f.tenantA, p)
		el := time.Since(begin)
		if err != nil {
			t.Fatalf("run %d: %v after %s", i, err, el)
		}
		if !d.Allowed {
			t.Fatalf("run %d: %s %v", i, d.Code, d.Figures)
		}
		durs = append(durs, el)
	}
	sorted := append([]time.Duration(nil), durs...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	p50, mx := sorted[runs/2], sorted[runs-1]
	t.Logf("HEAVY-PLAYER EvaluateAction(casino_wager), %d bets + %d wins in 30 days, windows rolling_24h / rolling_30d / calendar_month, "+
		"account scope: runs=%v p50=%s max=%s (default statement timeout %s, lock timeout %s); production default holds: %v",
		n, n, durs, p50, mx, DefaultActionStatementTimeout, DefaultActionLockTimeout, mx < DefaultActionStatementTimeout)

	// The size of what the scan reads, for the record.
	var rowsInWindow int64
	if err := do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1 AND player_account_id = $2 AND asset_code = 'USD'
			AND created_at >= clock_timestamp() - interval '720 hours'`, f.tenantA, f.acctA1).Scan(&rowsInWindow)
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("ledger_entries rows of the player in the 30-day window: %d", rowsInWindow)

	// A production timeout shorter than the heavy scan fails CLOSED, never open.
	p := f.action(f.acctA1, ActionCasinoWager, 100)
	p.StatementTimeout = 5 * time.Millisecond
	if _, err := w.eval(f, f.tenantA, p); err == nil {
		t.Fatal("a statement timeout shorter than the heavy scan must fail closed")
	}
}

// bulkTyped inserts n synthetic transactions of one stake / win type for acct spread evenly over [from, to), in
// batches, through the real triggers (nothing disabled).
func (w *s1World) bulkTyped(t *testing.T, tenant, acct uuid.UUID, txType string, n int, from, to time.Time) {
	t.Helper()
	const batch = 250
	step := to.Sub(from) / time.Duration(n)
	for done := 0; done < n; done += batch {
		m := batch
		if n-done < m {
			m = n - done
		}
		bf, bt := from.Add(step*time.Duration(done)), from.Add(step*time.Duration(done+m))
		err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
			wid, err := s3Wallet(ctx, tx, tenant, acct, "USD")
			if err != nil {
				return err
			}
			cash, err := ledger.GetOrCreateAccount(ctx, tx, tenant, &wid, ledger.AccountPlayerCash, "USD")
			if err != nil {
				return err
			}
			house, err := ledger.GetOrCreateAccount(ctx, tx, tenant, nil, ledger.AccountHouseGaming, "USD")
			if err != nil {
				return err
			}
			cashDir, houseDir := "debit", "credit"
			if txType == "casino_win" {
				cashDir, houseDir = "credit", "debit"
			}
			_, err = tx.Exec(ctx, fmt.Sprintf(`
				WITH t AS (
				  INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id, created_at, posted_at)
				  SELECT gen_random_uuid(), $1::uuid, $7::text, 'perf-' || gen_random_uuid()::text, gen_random_uuid(), ts, ts
				    FROM (SELECT $4::timestamptz + (($5::timestamptz - $4::timestamptz) * g / $6::int) AS ts FROM generate_series(0, $6::int - 1) g) s
				  RETURNING id, created_at
				)
				INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount, created_at)
				SELECT id, $2::uuid, $1::uuid, 'USD', '%s', 100, created_at FROM t
				UNION ALL
				SELECT id, $3::uuid, $1::uuid, 'USD', '%s', 100, created_at FROM t`, cashDir, houseDir),
				tenant, cash, house, bf, bt, m, txType)
			return err
		})
		if err != nil {
			t.Fatalf("bulk %s: %v", txType, err)
		}
	}
}
