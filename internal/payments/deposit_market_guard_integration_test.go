//go:build integration

// A8: InitiateDepositAttempt's operating-market guard seam
// (InitiateDepositParams.MarketGuard). The guard's own decision logic is
// internal/marketgate's; these tests pin WHERE the orchestrator evaluates it
// and WHAT it does with each verdict: first statement of phase A (before any
// row exists), a refusal creates no intent/attempt and makes no provider call
// (replay included), the refusal is a RESULT (so the guard's audit row
// commits) and a guard error fails the request closed.
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
)

type marketDepositRig struct {
	pool     *db.Pool
	f        orchFixture
	orch     *Orchestrator
	provider *txscopeCapturingProvider
}

func newMarketDepositRig(t *testing.T, id string) marketDepositRig {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	inner := NewMockProvider(id, "EUR")
	provider := &txscopeCapturingProvider{MockProvider: inner}
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{id: provider}, MultiWebhookCredentialResolver{id: NewMockWebhookCredentials(inner)})
	return marketDepositRig{pool: pool, f: f, orch: orch, provider: provider}
}

func (r marketDepositRig) initiate(t *testing.T, key string, guard marketgate.Guard) (InitiateDepositAttemptResult, error) {
	t.Helper()
	return r.orch.InitiateDepositAttempt(context.Background(), r.pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: r.f.tenantID, BrandID: r.f.brandID, PlayerAccountID: r.f.playerAccountID, WalletID: r.f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: key, MarketGuard: guard,
	})
}

func (r marketDepositRig) intentCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM deposit_intents WHERE tenant_id = $1`, r.f.tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count intents: %v", err)
	}
	return n
}

func (r marketDepositRig) providerCalls() int {
	r.provider.mu.Lock()
	defer r.provider.mu.Unlock()
	return len(r.provider.heldSeen)
}

func staticGuard(d marketgate.Decision, err error, calls *int) marketgate.Guard {
	return func(context.Context, pgx.Tx) (marketgate.Decision, error) {
		*calls++
		return d, err
	}
}

func TestInitiateDepositAttempt_MarketGuardRefusalCreatesNothingAndCallsNoProvider(t *testing.T) {
	r := newMarketDepositRig(t, "mock-psp-mkt-a")
	calls := 0
	res, err := r.initiate(t, "mkt-deny", staticGuard(marketgate.Decision{Allowed: false, Code: marketgate.ReasonNotPermitted}, nil, &calls))
	if err != nil {
		t.Fatalf("a refusal is a result, not an error: %v", err)
	}
	if !res.MarketDenied || res.AttemptCreated || res.Intent.ID.String() != "00000000-0000-0000-0000-000000000000" {
		t.Fatalf("want MarketDenied with a zero intent, got %+v", res)
	}
	if calls != 1 {
		t.Fatalf("guard must run exactly once, ran %d", calls)
	}
	if n := r.intentCount(t); n != 0 {
		t.Fatalf("a refused deposit created %d intents", n)
	}
	if n := r.providerCalls(); n != 0 {
		t.Fatalf("a refused deposit reached the provider %d times", n)
	}
}

// The refusal must not consume the idempotency key: the same key, once the
// market is open, is a normal first request.
func TestInitiateDepositAttempt_MarketGuardRefusalDoesNotConsumeTheIdempotencyKey(t *testing.T) {
	r := newMarketDepositRig(t, "mock-psp-mkt-b")
	calls := 0
	if res, err := r.initiate(t, "mkt-key", staticGuard(marketgate.Decision{Code: marketgate.ReasonNotPermitted}, nil, &calls)); err != nil || !res.MarketDenied {
		t.Fatalf("want refusal, got %+v err=%v", res, err)
	}
	res, err := r.initiate(t, "mkt-key", staticGuard(marketgate.Decision{Allowed: true}, nil, &calls))
	if err != nil || res.MarketDenied || !res.AttemptCreated {
		t.Fatalf("the key must be reusable after a refusal, got %+v err=%v", res, err)
	}
	if n := r.intentCount(t); n != 1 {
		t.Fatalf("want 1 intent, got %d", n)
	}
}

// A replay of an already-created intent is refused too: a request from
// outside the operating market learns nothing about earlier intents.
func TestInitiateDepositAttempt_MarketGuardRefusesReplayOfAnExistingIntent(t *testing.T) {
	r := newMarketDepositRig(t, "mock-psp-mkt-c")
	calls := 0
	first, err := r.initiate(t, "mkt-replay", staticGuard(marketgate.Decision{Allowed: true}, nil, &calls))
	if err != nil || !first.AttemptCreated {
		t.Fatalf("first request: %+v err=%v", first, err)
	}
	replay, err := r.initiate(t, "mkt-replay", staticGuard(marketgate.Decision{Code: marketgate.ReasonEvidenceUnavailable}, nil, &calls))
	if err != nil || !replay.MarketDenied || replay.Intent.ID == first.Intent.ID {
		t.Fatalf("a refused replay must not return the intent, got %+v err=%v", replay, err)
	}
	if n := r.intentCount(t); n != 1 {
		t.Fatalf("want the original intent only, got %d", n)
	}
}

func TestInitiateDepositAttempt_MarketGuardErrorFailsClosed(t *testing.T) {
	r := newMarketDepositRig(t, "mock-psp-mkt-d")
	calls := 0
	boom := errors.New("policy read failed")
	res, err := r.initiate(t, "mkt-err", staticGuard(marketgate.Decision{Allowed: true}, boom, &calls))
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("a guard error must fail the request, got %+v err=%v", res, err)
	}
	if res.AttemptCreated || res.MarketDenied {
		t.Fatalf("unexpected result %+v", res)
	}
	if n := r.intentCount(t); n != 0 {
		t.Fatalf("a failed gate created %d intents", n)
	}
	if n := r.providerCalls(); n != 0 {
		t.Fatalf("a failed gate reached the provider %d times", n)
	}
}

// The guard is the FIRST statement of phase A: when it runs, the intent
// row for this request does not exist yet, and it runs inside the
// tenant-scoped transaction (it can read tenant data).
func TestInitiateDepositAttempt_MarketGuardRunsBeforeAnyRowExistsInsideTheTransaction(t *testing.T) {
	r := newMarketDepositRig(t, "mock-psp-mkt-e")
	var seen = -1
	guard := func(ctx context.Context, tx pgx.Tx) (marketgate.Decision, error) {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM deposit_intents WHERE tenant_id = $1`, r.f.tenantID).Scan(&seen); err != nil {
			return marketgate.Decision{}, err
		}
		return marketgate.Decision{Allowed: true}, nil
	}
	if res, err := r.initiate(t, "mkt-first", guard); err != nil || !res.AttemptCreated {
		t.Fatalf("want an allowed request, got %+v err=%v", res, err)
	}
	if seen != 0 {
		t.Fatalf("the guard must run before the intent insert (saw %d rows)", seen)
	}
}

// nil means "no market gate" (library/test callers only); the HTTP handler
// always sets it. Pinned so the semantics are explicit, not accidental.
func TestInitiateDepositAttempt_NilMarketGuardIsTheUngatedLibraryPath(t *testing.T) {
	r := newMarketDepositRig(t, "mock-psp-mkt-f")
	res, err := r.initiate(t, "mkt-nil", nil)
	if err != nil || res.MarketDenied || !res.AttemptCreated {
		t.Fatalf("nil guard: want normal processing, got %+v err=%v", res, err)
	}
}
