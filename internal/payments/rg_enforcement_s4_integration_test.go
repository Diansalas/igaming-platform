//go:build integration

// ADR 0115 S4-min (completion audit A6) through the REAL deposit initiation (T1) and the sweeper's cascade
// re-check (T2): the deposit limit is evaluated inside the transaction that creates / re-checks the intent,
// counts in-flight intents (the candidate itself only once), declines the intent IN THE SAME TRANSACTION with
// a figures-bearing audit row, and is enforced under concurrency. The rg-level semantics are proven in
// internal/rg; this file proves the payments WIRING.
package payments

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

func rgSetDepositLimit(t *testing.T, pool *db.Pool, f orchFixture, value string) {
	t.Helper()
	asset, period := "EUR", rg.PeriodRolling24h
	if err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.NewLimits(rg.LimitsConfig{}).SetLimit(ctx, tx, rg.SetLimitInput{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID,
			Limit: rg.LimitInput{Kind: rg.ControlDeposit, AssetCode: &asset, Period: &period, Value: &value, IdempotencyKey: uuid.NewString()}})
		return err
	}); err != nil {
		t.Fatalf("set deposit limit: %v", err)
	}
}

func rgDenials(t *testing.T, pool *db.Pool, f orchFixture) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'payments.deposit_denied_by_rg' ORDER BY created_at`, f.tenantID)
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
		t.Fatal(err)
	}
	return out
}

func rgDepositWorld(t *testing.T, enforce bool) (*db.Pool, orchFixture, *Orchestrator, *MockProvider) {
	t.Helper()
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("rg-s4-p1", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"rg-s4-p1": provider}, MultiWebhookCredentialResolver{"rg-s4-p1": NewMockWebhookCredentials(provider)}).
		WithPayoutDestinations(pitest.Shared())
	if enforce {
		rgfix.Provision(t, launchfix.OwnerFor(t, pool), f.tenantID, nil, "EUR", nil)
	}
	return pool, f, orch, provider
}

func rgDeposit(t *testing.T, pool *db.Pool, orch *Orchestrator, f orchFixture, amount int64, key string) InitiateDepositAttemptResult {
	t.Helper()
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt %s: %v", key, err)
	}
	return res
}

func TestRGEnforceS4_Deposit_LimitDeclinesTheIntentInTheSameTransactionWithFigures(t *testing.T) {
	pool, f, orch, _ := rgDepositWorld(t, true)
	rgSetDepositLimit(t, pool, f, "1000")
	// 600 goes to the provider and stays pending (in flight): it counts at the requested amount.
	first := rgDeposit(t, pool, orch, f, 600, "rg-d1-"+uuid.NewString())
	if first.Intent.Status == DepositIntentDeclined {
		t.Fatalf("a deposit within the limit must not be declined: %+v", first.Intent)
	}
	// 600 in flight + 401 > 1000
	over := rgDeposit(t, pool, orch, f, 401, "rg-d2-"+uuid.NewString())
	if over.Intent.Status != DepositIntentDeclined || over.AttemptCreated {
		t.Fatalf("a deposit over the limit must be declined with no attempt and no provider call, got status=%s attempt=%v", over.Intent.Status, over.AttemptCreated)
	}
	rows := rgDenials(t, pool, f)
	if len(rows) != 1 {
		t.Fatalf("want exactly 1 denial audit row, got %d", len(rows))
	}
	m := rows[0]
	for k, want := range map[string]string{"reason_code": rg.CodeDepositLimitReached, "limit": "1000", "usage": "600", "attempted_amount": "401",
		"period": "rolling_24h", "kind": "deposit", "asset_code": "EUR", "deposit_intent_id": over.Intent.ID.String(), "candidate_ref": over.Intent.ID.String()} {
		if m[k] != want {
			t.Fatalf("denial audit %s = %v, want %q (%v)", k, m[k], want, m)
		}
	}
	if m["window_start"] == nil || m["as_of"] == nil || m["config_version_ids"] == nil || m["usage_inputs"] == nil {
		t.Fatalf("the denial audit must carry the computed figures: %v", m)
	}
	// exactly at the limit is allowed (inclusive): 600 + 400
	if at := rgDeposit(t, pool, orch, f, 400, "rg-d3-"+uuid.NewString()); at.Intent.Status == DepositIntentDeclined {
		t.Fatalf("600 + 400 == 1000 must be allowed, got %+v", at.Intent)
	}
	// An idempotent replay of the DECLINED request returns the same declined intent (no re-evaluation, no second row).
	key := "rg-replay-" + uuid.NewString()
	d1 := rgDeposit(t, pool, orch, f, 1, key)
	d2 := rgDeposit(t, pool, orch, f, 1, key)
	if d1.Intent.ID != d2.Intent.ID || d2.Intent.Status != DepositIntentDeclined {
		t.Fatalf("a replay must return the original declined intent: %+v vs %+v", d1.Intent, d2.Intent)
	}
}

func TestRGEnforceS4_Deposit_SwitchOffByDefault_TimeOutAlwaysEnforced_ConfigMissingFailsClosed(t *testing.T) {
	pool, f, orch, _ := rgDepositWorld(t, false)
	rgSetDepositLimit(t, pool, f, "100") // recorded, not enforced: no activation
	if r := rgDeposit(t, pool, orch, f, 5000, "off-"+uuid.NewString()); r.Intent.Status == DepositIntentDeclined {
		t.Fatalf("enforcement off: the deposit proceeds, got %+v", r.Intent)
	}
	if err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := rg.NewLimits(rg.LimitsConfig{}).CreateTimeOut(ctx, tx, rg.TimeOutInput{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID,
			DurationHours: 24, IdempotencyKey: uuid.NewString()})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := rgDeposit(t, pool, orch, f, 10, "to-"+uuid.NewString())
	if r.Intent.Status != DepositIntentDeclined {
		t.Fatalf("a deposit during a time-out must be declined, got %+v", r.Intent)
	}
	if rows := rgDenials(t, pool, f); len(rows) != 1 || rows[0]["reason_code"] != rg.CodeTimedOut {
		t.Fatalf("want one timed_out denial, got %v", rows)
	}

	// enforcement on, no floor: fail closed
	pool2 := pool
	g := seedOrchFixture(t, pool2)
	registerCapability(t, pool2, g, NewMockProvider("rg-s4-p2", "EUR"), 100)
	staff := rgfix.Staff(t, launchfix.OwnerFor(t, pool2), g.tenantID)
	rgfix.Activate(t, launchfix.OwnerFor(t, pool2), g.tenantID, nil, staff)
	p2 := NewMockProvider("rg-s4-p2", "EUR")
	orch2 := NewOrchestrator(map[string]PaymentProvider{"rg-s4-p2": p2}, MultiWebhookCredentialResolver{"rg-s4-p2": NewMockWebhookCredentials(p2)}).WithPayoutDestinations(pitest.Shared())
	if c := rgDeposit(t, pool2, orch2, g, 10, "cm-"+uuid.NewString()); c.Intent.Status != DepositIntentDeclined {
		t.Fatalf("enforcement on with no floor must decline the deposit (rg_config_missing), got %+v", c.Intent)
	}
	if rows := rgDenials(t, pool2, g); len(rows) != 1 || rows[0]["reason_code"] != rg.CodeConfigMissing {
		t.Fatalf("want one rg_config_missing denial, got %v", rows)
	}
}

func TestRGEnforceS4_Deposit_ConcurrentInitiationsRacingTheLimit_InFlightIntentsCount(t *testing.T) {
	pool, f, orch, _ := rgDepositWorld(t, true)
	rgSetDepositLimit(t, pool, f, "500")
	const n = 8
	var mu sync.Mutex
	proceeded, declined := 0, 0
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
				Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
				AssetCode: "EUR", Amount: 200, PaymentMethod: "card", IdempotencyKey: fmt.Sprintf("race-%d-%s", i, uuid.NewString()),
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil:
				t.Errorf("initiation %d: %v", i, err)
			case res.Intent.Status == DepositIntentDeclined:
				declined++
			default:
				proceeded++
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if proceeded != 2 || declined != n-2 {
		t.Fatalf("limit 500 / deposit 200: exactly 2 of %d concurrent deposits may proceed, got %d proceeded %d declined", n, proceeded, declined)
	}
}

// T2 (drive.go): the sweeper's cascade re-check evaluates the SAME boundary. A cascade child created before the
// player lowered the limit is declined at its re-check, in the same transaction, and the audit names the attempt.
func TestRGEnforceS4_Deposit_T2CascadeRecheckDeclinesUnderALoweredLimit(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	providerA := NewMockProvider("rg-s4-t2a", "EUR")
	providerB := NewMockProvider("rg-s4-t2b", "EUR")
	providerB.AcceptAllAmounts = true
	registerCapability(t, pool, f, providerA, 100)
	registerCapability(t, pool, f, providerB, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"rg-s4-t2a": providerA, "rg-s4-t2b": providerB},
		MultiWebhookCredentialResolver{"rg-s4-t2a": NewMockWebhookCredentials(providerA), "rg-s4-t2b": NewMockWebhookCredentials(providerB)}).WithPayoutDestinations(pitest.Shared())
	rgfix.Provision(t, launchfix.OwnerFor(t, pool), f.tenantID, nil, "EUR", nil)
	rgSetDepositLimit(t, pool, f, "1000")

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 600, PaymentMethod: "card", IdempotencyKey: "rg-t2-a-" + uuid.NewString(),
	})
	if err != nil || res.Intent.Status == DepositIntentDeclined {
		t.Fatalf("the first deposit must proceed: %+v err=%v", res.Intent, err)
	}
	// A second pending deposit of 300 is in flight too (600 + 300 <= 1000 at its T1).
	rgDeposit2 := func(amount int64, key string) {
		r, e := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: key,
		})
		if e != nil || r.Intent.Status == DepositIntentDeclined {
			t.Fatalf("deposit %d must proceed: %+v err=%v", amount, r.Intent, e)
		}
	}
	rgDeposit2(300, "rg-t2-b-"+uuid.NewString())

	ref := *res.Attempt.ProviderReference
	setNextActionNow(t, pool, f.tenantID, res.Attempt.ID)
	providerA.Resolve(ref, OutcomeDeclined, "provider_unavailable", true) // cascadable
	sweeper := NewSweeper(pool, orch, AllowAllDepositKYCGate{}, MockCredentialResolver{})
	// Lower the limit BEFORE the sweep: 300 (other intent) + 600 (this intent, once) = 900 > 800 at the T2 re-check.
	rgSetDepositLimit(t, pool, f, "800")
	for i := 0; i < 3; i++ {
		if stats := sweeper.RunOnce(context.Background(), []uuid.UUID{f.tenantID}); len(stats.Errors) != 0 {
			t.Fatalf("sweep %d: %v", i, stats.Errors)
		}
	}
	var status string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id = $1`, res.Intent.ID).Scan(&status)
	}); err != nil {
		t.Fatal(err)
	}
	if status != string(DepositIntentDeclined) {
		t.Fatalf("the T2 re-check under a lowered limit must decline the intent, status=%s", status)
	}
	var found bool
	for _, m := range rgDenials(t, pool, f) {
		if m["deposit_intent_id"] == res.Intent.ID.String() && m["attempt_id"] != nil && m["reason_code"] == rg.CodeDepositLimitReached &&
			m["usage"] == "300" && m["limit"] == "800" && m["attempted_amount"] == "600" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the T2 denial audit (attempt id + figures usage 300 / limit 800 / attempted 600) is missing: %v", rgDenials(t, pool, f))
	}
}
