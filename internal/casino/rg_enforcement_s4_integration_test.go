//go:build integration

// ADR 0115 S4-min (completion audit A6) through the REAL casino code: LaunchGame and postBet (via
// receiveCallbackInTx) with RG limit enforcement switched on, the same win / rollback paths that must never
// be gated, and the fail-closed paths. The rg-level semantics (windows, MIN across scopes, ceilings,
// cooling-off, multi-brand Person aggregation, relink) are proven in internal/rg; this file proves the casino
// WIRING: a limit denial declines the bet with a stable code and a figures-bearing audit row, posts nothing,
// and never blocks settlement of existing exposure.
package casino

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

type rgCasinoWorld struct {
	t        *testing.T
	pool     *db.Pool
	f        casinoFixture
	provider *MockCasinoProvider
	orch     *Orchestrator
	session  uuid.UUID
	staff    uuid.UUID
}

// newRGCasinoWorld builds a funded casino fixture. enforce=true provisions a licence, EUR floors for
// deposit / loss / wager (with over[kind] on top) and switches enforcement on for the tenant.
func newRGCasinoWorld(t *testing.T, enforce bool, over map[string]rgfix.Spec) *rgCasinoWorld {
	t.Helper()
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1_000_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	w := &rgCasinoWorld{t: t, pool: pool, f: f, provider: provider,
		orch:    NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests(),
		session: mintSession(t, pool, f, "mock-casino", "EUR")}
	if enforce {
		w.staff = rgfix.Provision(t, launchfix.OwnerFor(t, pool), f.tenantID, nil, "EUR", over).Staff
	}
	return w
}

func (w *rgCasinoWorld) setLimit(kind rg.ControlKind, period rg.PeriodKind, value string) rg.ChangeResult {
	w.t.Helper()
	var res rg.ChangeResult
	asset := "EUR"
	if err := w.pool.WithPlayerScope(context.Background(), w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = rg.NewLimits(rg.LimitsConfig{}).SetLimit(ctx, tx, rg.SetLimitInput{TenantID: w.f.tenantID, PlayerAccountID: w.f.playerAccountID,
			Limit: rg.LimitInput{Kind: kind, AssetCode: &asset, Period: &period, Value: &value, IdempotencyKey: uuid.NewString()}})
		return err
	}); err != nil {
		w.t.Fatalf("set %s limit: %v", kind, err)
	}
	return res
}

func (w *rgCasinoWorld) deliver(ev CallbackEventType, txID, orig, round string, amount int64) (ReceiveCallbackResult, error) {
	w.t.Helper()
	outcome := OutcomeSucceeded
	if ev == CallbackEventRollback {
		outcome = ""
	}
	sess := w.session
	if ev != CallbackEventBet {
		sess = uuid.Nil
	}
	payload := w.provider.CallbackPayload(w.f.tenantID, ev, txID, orig, round, "game-1", amount, "EUR", outcome, "", w.f.playerRef, sess)
	var res ReceiveCallbackResult
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		res, e = w.orch.receiveCallbackInTx(ctx, tx, w.f.tenantID, "mock-casino", payload)
		return e
	})
	return res, err
}

func (w *rgCasinoWorld) mustDeliver(ev CallbackEventType, txID, orig, round string, amount int64) ReceiveCallbackResult {
	w.t.Helper()
	res, err := w.deliver(ev, txID, orig, round, amount)
	if err != nil {
		w.t.Fatalf("%s %s: %v", ev, txID, err)
	}
	return res
}

func (w *rgCasinoWorld) betsPosted() int {
	w.t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'casino_bet'`, w.f.tenantID).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *rgCasinoWorld) auditMetadata(action string) []map[string]any {
	w.t.Helper()
	var out []map[string]any
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = $2 ORDER BY created_at`, w.f.tenantID, action)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			m := map[string]any{}
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	}); err != nil {
		w.t.Fatal(err)
	}
	return out
}

func (w *rgCasinoWorld) launch(mode GameMode) (LaunchGameResult, error) {
	w.t.Helper()
	game := seedGame(w.t, w.pool, "mock-casino", "EUR")
	enableGameForTenant(w.t, w.pool, w.f, game.ID)
	return w.orch.LaunchGame(context.Background(), w.pool, NewMockOutboundResolver(), LaunchGameParams{
		TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, WalletID: w.f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: mode, MarketGuard: testUngatedGuard,
	})
}

func TestRGEnforceS4_Casino_WagerLimitDeclinesTheBetWithFiguresAndNeverBlocksSettlement(t *testing.T) {
	w := newRGCasinoWorld(t, true, nil)
	w.setLimit(rg.ControlWager, rg.PeriodRolling24h, "500")
	for i := 0; i < 5; i++ {
		if r := w.mustDeliver(CallbackEventBet, fmt.Sprintf("b%d", i), "", fmt.Sprintf("r%d", i), 100); r.Outcome != OutcomeSucceeded {
			t.Fatalf("bet %d within the limit must post, got %+v", i, r)
		}
	}
	before := w.betsPosted()
	r := w.mustDeliver(CallbackEventBet, "b-over", "", "r-over", 1)
	if r.Outcome != OutcomeDeclined || r.DeclineReason != rg.CodeWagerLimitReached {
		t.Fatalf("a bet over the wager limit must be declined with %q, got %+v", rg.CodeWagerLimitReached, r)
	}
	if w.betsPosted() != before {
		t.Fatal("a declined bet must post nothing")
	}
	// A redelivery of a bet that DID post is an idempotent replay, never re-evaluated against the (now exhausted) limit.
	if r := w.mustDeliver(CallbackEventBet, "b0", "", "r0", 100); r.Outcome != OutcomeSucceeded {
		t.Fatalf("a replay of a posted bet must still succeed with the limit exhausted, got %+v", r)
	}

	// The denial audit carries the COMPUTED figures (ADR 0115 section 4; S3 condition (e)).
	rows := w.auditMetadata("casino_bet.denied_by_rg_policy")
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 denial audit row, got %d", len(rows))
	}
	m := rows[0]
	for k, want := range map[string]string{"reason_code": rg.CodeWagerLimitReached, "limit": "500", "usage": "500", "attempted_amount": "1",
		"period": "rolling_24h", "kind": "wager", "asset_code": "EUR", "limit_source": "player_account"} {
		if m[k] != want {
			t.Fatalf("denial audit %s = %v, want %q (%v)", k, m[k], want, m)
		}
	}
	for _, k := range []string{"window_start", "as_of", "config_version_ids", "usage_inputs", "person_id"} {
		if m[k] == nil {
			t.Fatalf("denial audit lacks %q: %v", k, m)
		}
	}

	// Settlement / win / rollback of EXISTING exposure is never gated, even with the limit exhausted and then lowered.
	w.setLimit(rg.ControlWager, rg.PeriodRolling24h, "100")
	if r := w.mustDeliver(CallbackEventWin, "w0", "", "r0", 900); r.Outcome != OutcomeSucceeded {
		t.Fatalf("a win must post with the limit exhausted, got %+v", r)
	}
	if r := w.mustDeliver(CallbackEventRollback, "rb1", "b1", "r1", 0); r.Outcome != OutcomeSucceeded {
		t.Fatalf("a rollback of an existing bet must post with the limit exhausted, got %+v", r)
	}
	// ...and the rolled-back stake no longer counts: usage is 400, the lowered limit is 100, still refused.
	if r := w.mustDeliver(CallbackEventBet, "b-after", "", "r-after", 1); r.DeclineReason != rg.CodeWagerLimitReached {
		t.Fatalf("a new bet is still refused under the lowered limit, got %+v", r)
	}
}

func TestRGEnforceS4_Casino_LossLimitIsNetOfWins(t *testing.T) {
	w := newRGCasinoWorld(t, true, nil)
	w.setLimit(rg.ControlLoss, rg.PeriodRolling24h, "300")
	if r := w.mustDeliver(CallbackEventBet, "b1", "", "r1", 300); r.Outcome != OutcomeSucceeded { // 0 + 300 == 300
		t.Fatalf("a stake equal to the loss limit must post, got %+v", r)
	}
	// net loss 300 + 1 > 300 -> a further bet is refused
	if r := w.mustDeliver(CallbackEventBet, "b2", "", "r2", 1); r.DeclineReason != rg.CodeLossLimitReached {
		t.Fatalf("net loss 300 over a 300 limit must refuse, got %+v", r)
	}
	w.mustDeliver(CallbackEventWin, "w1", "", "r1", 250) // net loss 50
	if r := w.mustDeliver(CallbackEventBet, "b3", "", "r3", 250); r.Outcome != OutcomeSucceeded {
		t.Fatalf("net loss 50 + 250 == 300 must be allowed, got %+v", r)
	}
	if r := w.mustDeliver(CallbackEventBet, "b4", "", "r4", 1); r.DeclineReason != rg.CodeLossLimitReached {
		t.Fatalf("net loss 300 + 1 must be refused, got %+v", r)
	}
}

func TestRGEnforceS4_Casino_SwitchOffByDefault_RecordedLimitIsNotEnforced(t *testing.T) {
	w := newRGCasinoWorld(t, false, nil)
	w.setLimit(rg.ControlWager, rg.PeriodRolling24h, "100") // recorded, no activation
	if r := w.mustDeliver(CallbackEventBet, "b1", "", "r1", 5000); r.Outcome != OutcomeSucceeded {
		t.Fatalf("with enforcement off the bet must post, got %+v", r)
	}
	if res, err := w.launch(ModeReal); err != nil || res.Denied {
		t.Fatalf("with enforcement off a launch needs no RG configuration, got %+v err=%v", res, err)
	}
}

func TestRGEnforceS4_Casino_TimeOutBlocksLaunchAndBetsEvenWithTheSwitchOff_ButNotSettlement(t *testing.T) {
	w := newRGCasinoWorld(t, false, nil)
	w.mustDeliver(CallbackEventBet, "b1", "", "r1", 100) // existing exposure
	if err := w.pool.WithPlayerScope(context.Background(), w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.NewLimits(rg.LimitsConfig{}).CreateTimeOut(ctx, tx, rg.TimeOutInput{TenantID: w.f.tenantID, PlayerAccountID: w.f.playerAccountID,
			DurationHours: 24, IdempotencyKey: uuid.NewString()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := w.mustDeliver(CallbackEventBet, "b2", "", "r2", 100); r.Outcome != OutcomeDeclined || r.DeclineReason != rg.CodeTimedOut {
		t.Fatalf("a bet during a time-out must be declined timed_out, got %+v", r)
	}
	res, err := w.launch(ModeReal)
	if err != nil || !res.Denied || res.DenialCode != rg.CodeTimedOut {
		t.Fatalf("a launch during a time-out must be denied timed_out, got %+v err=%v", res, err)
	}
	if res, err := w.launch(ModeDemo); err != nil || !res.Denied || res.DenialCode != rg.CodeTimedOut {
		t.Fatalf("a demo launch is refused during a time-out as it is during a self-exclusion, got %+v err=%v", res, err)
	}
	// existing exposure settles
	if r := w.mustDeliver(CallbackEventWin, "w1", "", "r1", 500); r.Outcome != OutcomeSucceeded {
		t.Fatalf("a win during a time-out must post, got %+v", r)
	}
	if r := w.mustDeliver(CallbackEventRollback, "rb1", "b1", "r1", 0); r.Outcome != OutcomeSucceeded {
		t.Fatalf("a rollback during a time-out must post, got %+v", r)
	}
}

func TestRGEnforceS4_Casino_Launch_ConfigMissingAndMandatory_FailClosed(t *testing.T) {
	// switch on, tenant has no licence / floors: rg_config_missing for a REAL launch and a bet; demo launches have no money at stake
	w := newRGCasinoWorld(t, false, nil)
	staff := rgfix.Staff(t, launchfix.OwnerFor(t, w.pool), w.f.tenantID)
	rgfix.Activate(t, launchfix.OwnerFor(t, w.pool), w.f.tenantID, nil, staff)
	res, err := w.launch(ModeReal)
	if err != nil || !res.Denied || res.DenialCode != rg.CodeConfigMissing {
		t.Fatalf("a real launch with enforcement on and no floor must be denied rg_config_missing, got %+v err=%v", res, err)
	}
	if r := w.mustDeliver(CallbackEventBet, "b1", "", "r1", 10); r.Outcome != OutcomeDeclined || r.DeclineReason != rg.CodeConfigMissing {
		t.Fatalf("a bet with no floor must be declined rg_config_missing, got %+v", r)
	}
	if res, err := w.launch(ModeDemo); err != nil || res.Denied {
		t.Fatalf("a demo launch moves no money and needs no RG configuration, got %+v err=%v", res, err)
	}

	m := newRGCasinoWorld(t, true, map[string]rgfix.Spec{"wager": {"mandatory": true}})
	res, err = m.launch(ModeReal)
	if err != nil || !res.Denied || res.DenialCode != rg.CodeLimitRequired {
		t.Fatalf("a mandatory wager limit with no value must refuse a launch with rg_limit_required, got %+v err=%v", res, err)
	}
	if r := m.mustDeliver(CallbackEventBet, "b1", "", "r1", 10); r.DeclineReason != rg.CodeLimitRequired {
		t.Fatalf("...and a bet, got %+v", r)
	}
	m.setLimit(rg.ControlWager, rg.PeriodRolling24h, "1000")
	if res, err := m.launch(ModeReal); err != nil || res.Denied {
		t.Fatalf("once the mandatory limit is set the launch proceeds, got %+v err=%v", res, err)
	}
}

func TestRGEnforceS4_Casino_EvaluationFailure_AbortsTheCallback_PostsNothing(t *testing.T) {
	// count_bonus_funded=true on the casino path has no window-safe spec (S3 condition (a)): the evaluation cannot decide.
	w := newRGCasinoWorld(t, true, map[string]rgfix.Spec{"wager": {"count_bonus_funded": true}})
	w.setLimit(rg.ControlWager, rg.PeriodRolling24h, "1000")
	before := w.betsPosted()
	_, err := w.deliver(CallbackEventBet, "b1", "", "r1", 10)
	if err == nil || !errors.Is(err, rg.ErrEvaluationUnavailable) {
		t.Fatalf("an evaluation that cannot decide must ABORT the callback (fail closed), got %v", err)
	}
	if w.betsPosted() != before {
		t.Fatal("nothing may post when the RG evaluation cannot decide")
	}
	if rows := w.auditMetadata("casino_bet.denied_by_rg_policy"); len(rows) != 0 {
		t.Fatalf("an aborted evaluation is not a policy denial and writes no denial row, got %d", len(rows))
	}
	if _, err := w.launch(ModeReal); err == nil || !errors.Is(err, rg.ErrEvaluationUnavailable) {
		t.Fatalf("a launch whose evaluation cannot decide must fail closed too, got %v", err)
	}
}

func TestRGEnforceS4_Casino_ConcurrentBetsRacingTheLimit_ExactlyTheAllowedOnesPost(t *testing.T) {
	w := newRGCasinoWorld(t, true, nil)
	w.setLimit(rg.ControlWager, rg.PeriodRolling24h, "500")
	const n = 12
	var mu sync.Mutex
	succeeded, declined := 0, 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r, err := w.deliver(CallbackEventBet, fmt.Sprintf("race-%d", i), "", fmt.Sprintf("race-r-%d", i), 100)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("bet %d: %v", i, err)
			case r.Outcome == OutcomeSucceeded:
				succeeded++
			case r.DeclineReason == rg.CodeWagerLimitReached:
				declined++
			default:
				t.Errorf("bet %d: unexpected outcome %+v", i, r)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if succeeded != 5 || declined != n-5 {
		t.Fatalf("limit 500 / stake 100: exactly 5 of %d concurrent bets may post, got %d posted, %d declined", n, succeeded, declined)
	}
	if got := w.betsPosted(); got != 5 {
		t.Fatalf("exactly 5 casino_bet transactions must exist, got %d", got)
	}
}

// Evidence (ADR 0115 S3 conditions: re-measure the request-path p95 delta and the L0.4 hold time of a whole bet
// when S4 is wired). It logs figures and asserts only that every bet posted; no SLA is claimed. The tenant has a
// short ledger history (about 100 bets) and four enforced windows, so the figure is the FIXED per-bet cost of
// the enforcement path (about 20 small statements plus the usage scan); the scan itself grows with the player's
// window volume (see the heavy-player run, docs/plans/a6-rg-enforcement-record.md). The whole callback
// transaction duration is an upper bound of the L0.4 hold time: the person lock is taken early in it and held to
// the commit.
func TestRGEnforceS4_Evidence_CasinoBetCallbackLatency_OffVersusOn(t *testing.T) {
	measure := func(enforce bool) (p50, p95 time.Duration) {
		w := newRGCasinoWorld(t, enforce, nil)
		if enforce {
			for _, p := range []rg.PeriodKind{rg.PeriodRolling24h, rg.PeriodRolling7d, rg.PeriodRolling30d, rg.PeriodCalendarMonth} {
				w.setLimit(rg.ControlWager, p, "100000000")
				w.setLimit(rg.ControlLoss, p, "100000000")
			}
		}
		for i := 0; i < 100; i++ { // history
			w.mustDeliver(CallbackEventBet, fmt.Sprintf("h%d", i), "", fmt.Sprintf("hr%d", i), 10)
		}
		const runs = 100
		durs := make([]time.Duration, 0, runs)
		for i := 0; i < runs; i++ {
			begin := time.Now()
			if r := w.mustDeliver(CallbackEventBet, fmt.Sprintf("m%d", i), "", fmt.Sprintf("mr%d", i), 10); r.Outcome != OutcomeSucceeded {
				t.Fatalf("bet %d: %+v", i, r)
			}
			durs = append(durs, time.Since(begin))
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		return durs[runs/2], durs[(runs*95)/100]
	}
	off50, off95 := measure(false)
	on50, on95 := measure(true)
	t.Logf("EVIDENCE casino bet callback transaction (upper bound of the L0.4 hold): enforcement OFF p50=%s p95=%s; ON (4 windows x wager+loss) p50=%s p95=%s; delta p50=%s p95=%s",
		off50, off95, on50, on95, on50-off50, on95-off95)
}
