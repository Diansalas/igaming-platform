//go:build integration

// ADR 0113 slice 1 (C4): the casino bet/win latency gate for the ledger_entries hold-account guard
// (B-i, BEFORE INSERT) and its deferred companion (B-ii, at COMMIT) added by migration 0132.
//
// Budget (stated before measuring, ledger-finance): the p95 latency of a casino bet callback and of a
// casino win callback, each measured from the first statement of the callback to the end of COMMIT
// (B-ii fires at COMMIT, so the commit is inside the timed window), may regress by at most 10% with
// the two triggers enabled versus disabled. Method: one funded player, one launch session, real
// orchestrator callbacks (receiveCallbackInTx: bet then win on a fresh round per sample), samples
// INTERLEAVED one-for-one between the two modes so drift on a shared machine hits both equally. Each
// sample's transaction opens with an untimed ALTER TABLE ledger_entries {DISABLE|ENABLE} TRIGGER of
// both triggers, so the two modes take the identical lock profile and differ ONLY in whether B-i /
// B-ii run; the disabled state never outlives its own transaction (transactional DDL), so nothing
// else on the database ever sees the triggers off. The table owner runs it (as every casino test does).
//
// OPT-IN (ledger-finance review C3): the gate toggles a trigger on ledger_entries ~1600 times (each an
// ACCESS EXCLUSIVE lock for the toggling transaction), so it runs only when ADR0113_LATENCY_GATE=1 AND
// ADR0113_LATENCY_DB names the connected database (an explicit "this is my private database"); it
// skips otherwise and is never run on a shared database. The always-on evidence is
// TestADR0113_HoldGuardTriggerCost_ExplainAnalyze (per-trigger cost from EXPLAIN ANALYZE, no DDL).
//
// ADR0113_LATENCY_SAMPLES overrides the per-mode sample count (default 400).
package casino

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func TestADR0113_CasinoBetWinLatencyGate(t *testing.T) {
	if os.Getenv("ADR0113_LATENCY_GATE") != "1" {
		t.Skip("opt-in: toggles a trigger on ledger_entries ~1600 times (exclusive table lock); set ADR0113_LATENCY_GATE=1 and ADR0113_LATENCY_DB=<private database> to run it. Always-on evidence: TestADR0113_HoldGuardTriggerCost_ExplainAnalyze")
	}
	pool := testPool(t)
	var dbName string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_database()::text`).Scan(&dbName)
	}); err != nil {
		t.Fatal(err)
	}
	if want := os.Getenv("ADR0113_LATENCY_DB"); want == "" || want != dbName {
		t.Fatalf("the latency gate runs only on the private database named by ADR0113_LATENCY_DB (connected to %q)", dbName)
	}
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1_000_000_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	n := 400
	if v, err := strconv.Atoi(os.Getenv("ADR0113_LATENCY_SAMPLES")); err == nil && v > 0 {
		n = v
	}
	const budget = 0.10

	ctx := context.Background()
	toggle := func(enabled bool) string {
		verb := "DISABLE"
		if enabled {
			verb = "ENABLE"
		}
		return fmt.Sprintf(`ALTER TABLE ledger_entries %[1]s TRIGGER ledger_entries_withdrawal_hold_guard;
			ALTER TABLE ledger_entries %[1]s TRIGGER ledger_entries_withdrawal_hold_link_at_commit`, verb)
	}
	// one callback in its own transaction; returns the time from the first callback statement to the
	// end of COMMIT.
	run := func(enabled bool, payload webhookauth.Inbound) time.Duration {
		t.Helper()
		tx, err := pool.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, toggle(enabled)); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		res, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		if err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("callback: %v", err)
		}
		if res.Outcome != OutcomeSucceeded {
			_ = tx.Rollback(ctx)
			t.Fatalf("callback outcome %q (%s)", res.Outcome, res.DeclineReason)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		return time.Since(start)
	}

	samples := map[string][]time.Duration{}
	// warm-up (both modes), not recorded
	for i := 0; i < 20; i++ {
		for _, enabled := range []bool{true, false} {
			round := "lat-warm-" + uuid.NewString()
			run(enabled, provider.CallbackPayload(f.tenantID, CallbackEventBet, "b-"+round, "", round, "game-1", 10, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID))
			run(enabled, provider.CallbackPayload(f.tenantID, CallbackEventWin, "w-"+round, "", round, "game-1", 5, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil))
		}
	}
	for i := 0; i < n; i++ {
		modes := []bool{true, false}
		if i%2 == 1 {
			modes = []bool{false, true}
		}
		for _, enabled := range modes {
			mode := "disabled"
			if enabled {
				mode = "enabled"
			}
			round := "lat-" + uuid.NewString()
			samples["bet/"+mode] = append(samples["bet/"+mode],
				run(enabled, provider.CallbackPayload(f.tenantID, CallbackEventBet, "b-"+round, "", round, "game-1", 10, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)))
			samples["win/"+mode] = append(samples["win/"+mode],
				run(enabled, provider.CallbackPayload(f.tenantID, CallbackEventWin, "w-"+round, "", round, "game-1", 5, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)))
		}
	}
	pct := func(d []time.Duration, p float64) time.Duration {
		s := append([]time.Duration(nil), d...)
		sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
		idx := int(p*float64(len(s)-1) + 0.5)
		return s[idx]
	}
	failed := false
	for _, kind := range []string{"bet", "win"} {
		on, off := samples[kind+"/enabled"], samples[kind+"/disabled"]
		p50on, p50off, p95on, p95off := pct(on, 0.50), pct(off, 0.50), pct(on, 0.95), pct(off, 0.95)
		reg := float64(p95on)/float64(p95off) - 1
		t.Logf("ADR0113-LATENCY %s n=%d p50 disabled=%v enabled=%v (%+.1f%%) | p95 disabled=%v enabled=%v (%+.1f%%) | budget p95 <= +%.0f%%",
			kind, len(on), p50off, p50on, (float64(p50on)/float64(p50off)-1)*100, p95off, p95on, reg*100, budget*100)
		if reg > budget {
			failed = true
		}
	}
	if failed {
		t.Fatalf("casino bet/win p95 regression above the %.0f%% budget with the 0132 hold-account guards enabled", budget*100)
	}
}

// TestADR0113_HoldGuardTriggerCost_ExplainAnalyze is the always-on cost evidence for C4 (no DDL, no
// lock beyond an ordinary insert, rolled back): EXPLAIN ANALYZE of a two-entry player posting reports
// the time and call count of every trigger, B-ii fired at statement end (SET CONSTRAINTS ... IMMEDIATE
// for that trigger only). The bound is generous (1 ms per call; reviewers measured 0.01-0.04 ms) so
// the test pins an order of magnitude, not a benchmark.
func TestADR0113_HoldGuardTriggerCost_ExplainAnalyze(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	ctx := context.Background()
	type trig struct {
		Name  string  `json:"Trigger Name"`
		Time  float64 `json:"Time"`
		Calls int     `json:"Calls"`
	}
	per := map[string][]float64{}
	for i := 0; i < 30; i++ {
		tx, err := pool.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		func() {
			defer func() { _ = tx.Rollback(ctx) }()
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
				t.Fatal(err)
			}
			cash, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerCash, "EUR")
			if err != nil {
				t.Fatal(err)
			}
			clearing, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, "EUR")
			if err != nil {
				t.Fatal(err)
			}
			txID, ref := uuid.New(), "a13-cost-"+uuid.NewString()
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
				VALUES ($1, $2, 'deposit', $3, 'a13-cost', $3, gen_random_uuid())`, txID, f.tenantID, ref); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(ctx, `SET CONSTRAINTS ledger_entries_withdrawal_hold_link_at_commit IMMEDIATE`); err != nil {
				t.Fatal(err)
			}
			var plan []byte
			if err := tx.QueryRow(ctx, `EXPLAIN (ANALYZE, FORMAT JSON) INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
				VALUES ($1, $2, $4, 'EUR', 'debit', 5), ($1, $3, $4, 'EUR', 'credit', 5)`, txID, clearing, cash, f.tenantID).Scan(&plan); err != nil {
				t.Fatal(err)
			}
			var out []struct {
				Triggers []trig `json:"Triggers"`
			}
			if err := json.Unmarshal(plan, &out); err != nil || len(out) != 1 {
				t.Fatalf("plan: %v %s", err, plan)
			}
			for _, tr := range out[0].Triggers {
				if tr.Calls > 0 {
					per[tr.Name] = append(per[tr.Name], tr.Time/float64(tr.Calls))
				}
			}
		}()
	}
	med := func(v []float64) float64 {
		s := append([]float64(nil), v...)
		sort.Float64s(s)
		return s[len(s)/2]
	}
	for _, name := range []string{"ledger_entries_populate_from_account_trigger", "ledger_entries_withdrawal_hold_guard", "ledger_entries_withdrawal_hold_link_at_commit"} {
		v := per[name]
		if len(v) == 0 {
			t.Fatalf("EXPLAIN ANALYZE reported no calls of %s: %v", name, per)
		}
		t.Logf("ADR0113-TRIGGER-COST %s median %.4f ms/entry over %d runs", name, med(v), len(v))
		if name != "ledger_entries_populate_from_account_trigger" && med(v) > 1.0 {
			t.Fatalf("%s costs %.4f ms per entry (bound 1 ms)", name, med(v))
		}
	}
}
