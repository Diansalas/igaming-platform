//go:build integration

package rg

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// ADR 0115 3.4 / 8 (S3 evidence required BEFORE S4 may merge): EXPLAIN (ANALYZE, BUFFERS) of the usage
// statements showing the migration-0135 indexes in use, the latency of the measurement, and the time the
// L0.4 person lock is held by it. Runs on the scratch database with a synthetic ledger; the figures are
// logged (evidence), the asserted properties are structural (index used, no sequential scan of
// ledger_entries / deposit_intents, bounded buffers) - no SLA is claimed.

// Default scale keeps the suite quick; RG_S3_SCALE=large reproduces the evidence run recorded in
// docs/plans/prh2-hardening-round/adr0115-s3-explain-evidence.txt (about 7 minutes, almost all of it seeding the ledger
// through the real triggers).
var perfTargetHistory, perfFiller = func() (int, int) {
	if os.Getenv("RG_S3_SCALE") == "large" {
		return 20000, 40000
	}
	return 3000, 6000
}()

const (
	perfIndexLedger  = "idx_ledger_entries_tenant_player_asset_created"
	perfIndexDeposit = "idx_deposit_intents_tenant_player_asset_created"
)

type planNode struct {
	NodeType  string     `json:"Node Type"`
	Relation  string     `json:"Relation Name"`
	IndexName string     `json:"Index Name"`
	Plans     []planNode `json:"Plans"`
	SharedHit int64      `json:"Shared Hit Blocks"`
	SharedRd  int64      `json:"Shared Read Blocks"`
}

func (n planNode) walk(fn func(planNode)) {
	fn(n)
	for _, c := range n.Plans {
		c.walk(fn)
	}
}

// bulkLedger inserts n synthetic casino_bet transactions (Dr cash / Cr house, 100) for acct spread
// evenly over [from, to), through the real triggers (nothing disabled).
func (w *s1World) bulkLedger(t *testing.T, tenant, acct uuid.UUID, asset string, n int, from, to time.Time) {
	t.Helper()
	// Batches of 250 transactions: the balance-projection trigger updates one row per entry, and one very large
	// statement degrades superlinearly on that hot row.
	const batch = 250
	span := to.Sub(from)
	for done := 0; done < n; done += batch {
		m := batch
		if n-done < m {
			m = n - done
		}
		step := span / time.Duration(n) // divide first: span*done overflows int64 nanoseconds
		bf := from.Add(step * time.Duration(done))
		bt := from.Add(step * time.Duration(done+m))
		w.bulkLedgerBatch(t, tenant, acct, asset, m, bf, bt)
	}
}

func (w *s1World) bulkLedgerBatch(t *testing.T, tenant, acct uuid.UUID, asset string, n int, from, to time.Time) {
	t.Helper()
	err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		wid, err := s3Wallet(ctx, tx, tenant, acct, asset)
		if err != nil {
			return err
		}
		cash, err := ledger.GetOrCreateAccount(ctx, tx, tenant, &wid, ledger.AccountPlayerCash, asset)
		if err != nil {
			return err
		}
		house, err := ledger.GetOrCreateAccount(ctx, tx, tenant, nil, ledger.AccountHouseGaming, asset)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `
			WITH t AS (
			  INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id, created_at, posted_at)
			  SELECT gen_random_uuid(), $1::uuid, 'casino_bet', 'perf-' || gen_random_uuid()::text, gen_random_uuid(), ts, ts
			    FROM (SELECT $4::timestamptz + (($5::timestamptz - $4::timestamptz) * g / $6::int) AS ts FROM generate_series(0, $6::int - 1) g) s
			  RETURNING id, created_at
			)
			INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount, created_at)
			SELECT id, $2::uuid, $1::uuid, $7::text, 'debit', 100, created_at FROM t
			UNION ALL
			SELECT id, $3::uuid, $1::uuid, $7::text, 'credit', 100, created_at FROM t`,
			tenant, cash, house, from, to, n, asset)
		return err
	})
	if err != nil {
		t.Fatalf("bulk ledger: %v", err)
	}
}

func (w *s1World) bulkIntents(t *testing.T, tenant, acct uuid.UUID, n int, from, to time.Time) {
	t.Helper()
	err := w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		wid, err := s3Wallet(ctx, tx, tenant, acct, "USD")
		if err != nil {
			return err
		}
		var brand uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT brand_id FROM player_accounts WHERE id = $1`, acct).Scan(&brand); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key, status, created_at, updated_at)
			SELECT gen_random_uuid(), $1::uuid, $2::uuid, $3::uuid, $4::uuid, 'USD', 100, 'card', 'perf-' || gen_random_uuid()::text, 'declined', ts, ts
			  FROM (SELECT $5::timestamptz + (($6::timestamptz - $5::timestamptz) * g / $7::int) AS ts FROM generate_series(0, $7::int - 1) g) s`,
			tenant, brand, acct, wid, from, to, n)
		return err
	})
	if err != nil {
		t.Fatalf("bulk intents: %v", err)
	}
}

func TestRGUsageS3ExplainAndLatency(t *testing.T) {
	w := newS1World(t)
	f := w.newFix(t)
	a := f.acctA1
	ctx := context.Background()
	now := time.Now().UTC()

	// Target player: 12,000 bets over 400 days (a long history - the whole-history scan the ADR warns about) plus a busy
	// last day; two other accounts of the same tenant carry 60,000 more rows so the table is not tiny.
	step := func(what string, fn func()) {
		t0 := time.Now()
		fn()
		t.Logf("seed %s: %v", what, time.Since(t0))
	}
	step("target history", func() {
		w.bulkLedger(t, f.tenantA, a, "USD", perfTargetHistory, now.Add(-400*24*time.Hour), now.Add(-25*time.Hour))
	})
	step("target last day", func() { w.bulkLedger(t, f.tenantA, a, "USD", 300, now.Add(-24*time.Hour), now.Add(-time.Minute)) })
	step("filler X", func() {
		w.bulkLedger(t, f.tenantA, f.acctX, "USD", perfFiller, now.Add(-400*24*time.Hour), now.Add(-time.Minute))
	})
	step("filler A2", func() {
		w.bulkLedger(t, f.tenantA, f.acctA2, "USD", perfFiller, now.Add(-400*24*time.Hour), now.Add(-time.Minute))
	})
	step("intents", func() {
		w.bulkIntents(t, f.tenantA, a, perfFiller/5, now.Add(-400*24*time.Hour), now.Add(-time.Minute))
	})
	step("intents filler", func() {
		w.bulkIntents(t, f.tenantA, f.acctX, perfFiller, now.Add(-400*24*time.Hour), now.Add(-time.Minute))
	})
	wantOK(t, "analyze", func() error {
		for _, tbl := range []string{"ledger_entries", "ledger_transactions", "deposit_intents", "ledger_accounts", "player_accounts"} {
			if _, err := w.owner.Raw().Exec(ctx, "ANALYZE "+tbl); err != nil {
				return err
			}
		}
		return nil
	}())

	// The seed must have produced the intended time distribution (a Duration overflow once clustered it).
	var in30, in7, all int
	wantOK(t, "distribution", w.owner.WithTenant(ctx, f.tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE created_at >= $2::timestamptz - interval '30 days'), count(*) FILTER (WHERE created_at >= $2::timestamptz - interval '7 days'), count(*)
			FROM ledger_entries WHERE player_account_id = $1 AND direction = 'debit'`, a, now).Scan(&in30, &in7, &all)
	}))
	t.Logf("target player debit entries: %d in total, %d in the last 30 days, %d in the last 7 days (rows the 30-day window must read)", all, in30, in7)
	if all != perfTargetHistory+300 || in30 < 300+perfTargetHistory/20 || in30 > 300+perfTargetHistory/8 || in7 < 300 || in7 > in30 {
		t.Fatalf("seed distribution is wrong: total %d, 30d %d, 7d %d", all, in30, in7)
	}

	windows := []WindowSpec{{Period: PeriodRolling24h}, {Period: PeriodRolling7d}, {Period: PeriodRolling30d}, {Period: PeriodCalendarDay, CalendarTZ: "Europe/Berlin"}}
	periods, tzs, wss, err := windowArgs(windows)
	wantOK(t, "args", err)

	explain := func(label, sql string, args ...any) (planNode, string) {
		t.Helper()
		var raw []byte
		var txt strings.Builder
		wantOK(t, label, do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
				return err
			}
			rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					return err
				}
				txt.WriteString(line + "\n")
			}
			return rows.Err()
		}))
		var out []struct {
			Plan planNode `json:"Plan"`
		}
		if err := json.Unmarshal(raw, &out); err != nil || len(out) != 1 {
			t.Fatalf("%s: plan json: %v", label, err)
		}
		return out[0].Plan, txt.String()
	}

	stakeArgs := []any{f.tenantA, a, "USD", nil, periods, tzs, wss, "tenant_person", RGLossTransactionTypes, RGReversalTransactionTypes, RGStakeTransactionTypes}
	plan, text := explain("stake", stakeUsageSQL(len(windows), true), stakeArgs...)
	t.Logf("EXPLAIN (ANALYZE, BUFFERS) stake usage, tenant_person, 4 windows, WITH %s:\n%s", perfIndexLedger, text)
	usesIdx, seq := false, []string{}
	plan.walk(func(n planNode) {
		if n.Relation == "ledger_entries" && n.IndexName == perfIndexLedger {
			usesIdx = true
		}
		if n.NodeType == "Seq Scan" && (n.Relation == "ledger_entries" || n.Relation == "ledger_transactions") {
			seq = append(seq, n.Relation)
		}
	})
	if !usesIdx {
		t.Fatalf("the stake usage statement must use %s:\n%s", perfIndexLedger, text)
	}
	if len(seq) > 0 {
		t.Fatalf("the stake usage statement must not sequentially scan %v:\n%s", seq, text)
	}

	depArgs := []any{f.tenantA, a, "USD", nil, periods, tzs, wss, "tenant_person", nil}
	dplan, dtext := explain("deposit", depositUsageSQL(len(windows), true), depArgs...)
	t.Logf("EXPLAIN (ANALYZE, BUFFERS) deposit usage, tenant_person, 4 windows:\n%s", dtext)
	ledIdx, intIdx, dseq := false, false, []string{}
	dplan.walk(func(n planNode) {
		if n.Relation == "ledger_entries" && n.IndexName == perfIndexLedger {
			ledIdx = true
		}
		if n.Relation == "deposit_intents" && n.IndexName == perfIndexDeposit {
			intIdx = true
		}
		if n.NodeType == "Seq Scan" && (n.Relation == "ledger_entries" || n.Relation == "deposit_intents") {
			dseq = append(dseq, n.Relation)
		}
	})
	if !ledIdx || !intIdx || len(dseq) > 0 {
		t.Fatalf("deposit usage must use %s and %s with no seq scan (ledger=%v intents=%v seq=%v):\n%s", perfIndexLedger, perfIndexDeposit, ledIdx, intIdx, dseq, dtext)
	}

	// Contrast (evidence): the same statement WITHOUT the 0135 ledger index, in this throwaway database only.
	// Dropped and re-created inside one transaction that is rolled back, so the scratch schema is unchanged.
	{
		tx, err := w.owner.Raw().Begin(ctx)
		wantOK(t, "begin", err)
		_, err = tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantA.String())
		wantOK(t, "tenant", err)
		_, err = tx.Exec(ctx, `DROP INDEX `+perfIndexLedger)
		wantOK(t, "drop index (rolled back)", err)
		rows, err := tx.Query(ctx, "EXPLAIN (ANALYZE, BUFFERS) "+stakeUsageSQL(len(windows), true), stakeArgs...)
		wantOK(t, "explain without index", err)
		var b strings.Builder
		for rows.Next() {
			var l string
			wantOK(t, "scan", rows.Scan(&l))
			b.WriteString(l + "\n")
		}
		rows.Close()
		wantOK(t, "rows", rows.Err())
		_ = tx.Rollback(ctx)
		t.Logf("CONTRAST: same statement WITHOUT %s (only the legacy partial index idx_ledger_entries_player_account is left):\n%s", perfIndexLedger, b.String())
	}

	// Latency of the measurement and the time the L0.4 person lock is held by it (lock + both usage reads).
	const runs = 300
	var stake, dep, held []time.Duration
	for i := 0; i < runs; i++ {
		var s, d, h time.Duration
		wantOK(t, "latency run", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
			if err := lockPerson(ctx, tx, f.personA); err != nil {
				return err
			}
			lockedAt := time.Now()
			t0 := time.Now()
			if _, err := MeasureStakeUsage(ctx, tx, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeTenantPerson, AssetCode: "USD", Windows: windows}); err != nil {
				return err
			}
			s = time.Since(t0)
			t0 = time.Now()
			if _, err := MeasureDepositUsage(ctx, tx, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeTenantPerson, AssetCode: "USD", Windows: windows}); err != nil {
				return err
			}
			d = time.Since(t0)
			h = time.Since(lockedAt)
			return nil
		}))
		stake, dep, held = append(stake, s), append(dep, d), append(held, h)
	}
	pct := func(v []time.Duration, p float64) time.Duration {
		c := append([]time.Duration(nil), v...)
		sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
		return c[int(float64(len(c)-1)*p)]
	}
	t.Logf("LATENCY over %d runs (synthetic ledger: %d target-player bets, %d ledger rows in the table): stake usage p50=%v p95=%v p99=%v | deposit usage p50=%v p95=%v p99=%v | L0.4 held for both reads p50=%v p95=%v p99=%v",
		runs, perfTargetHistory+300, 2*(perfTargetHistory+300+2*perfFiller), pct(stake, .5), pct(stake, .95), pct(stake, .99), pct(dep, .5), pct(dep, .95), pct(dep, .99), pct(held, .5), pct(held, .95), pct(held, .99))
	if pct(stake, .95) > 500*time.Millisecond || pct(dep, .95) > 500*time.Millisecond {
		t.Fatalf("usage statements take %v / %v at p95 on a small synthetic ledger; that is a regression against the index design", pct(stake, .95), pct(dep, .95))
	}
	// Correctness at scale: the busy last day (300 bets of 100 in the final 24 h, anchored by the test clock).
	var u StakeUsage
	wantOK(t, "scale correctness", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		u, err = MeasureStakeUsage(ctx, tx, UsageRequest{TenantID: f.tenantA, PlayerAccountID: a, Scope: LimitScopeAccount, AssetCode: "USD", Windows: []WindowSpec{{Period: PeriodRolling24h}}, At: &now})
		return err
	}))
	if got := u.Windows[0].Wager.Account.String(); got != fmt.Sprint(300*100) {
		t.Fatalf("24h wager at scale: %s", got)
	}
}
