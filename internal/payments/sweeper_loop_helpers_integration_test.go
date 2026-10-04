//go:build integration

// PRH-2 H (CP-W1) test helpers: a hook-bearing provider spy, tenant status
// forcing, lease forcing and counters. Everything is deterministic (channels,
// SQL-forced clocks); nothing here sleeps to make an assertion.
package payments

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
)

// loopProvider wraps a MockProvider with call counters and per-call hooks. A hook
// runs BEFORE delegating and while no lock of this spy is held, so a hook may block
// on a channel to hold a sweeper inside the provider call.
type loopProvider struct {
	*MockProvider
	mu                             sync.Mutex
	deposits, withdraws, queries   int
	onDeposit, onWithdraw, onQuery func(call int)
	panicCapabilities              bool
}

func newLoopProvider(id string) *loopProvider {
	return &loopProvider{MockProvider: NewMockProvider(id, "EUR")}
}

func (p *loopProvider) Capabilities() AdapterCapability {
	p.mu.Lock()
	boom := p.panicCapabilities
	p.mu.Unlock()
	if boom {
		panic("loopProvider: injected panic outside the gate")
	}
	return p.MockProvider.Capabilities()
}

func (p *loopProvider) setPanicCapabilities(v bool) {
	p.mu.Lock()
	p.panicCapabilities = v
	p.mu.Unlock()
}

func (p *loopProvider) Deposit(ctx context.Context, req DepositRequest) (DepositResult, error) {
	p.mu.Lock()
	p.deposits++
	n, hook := p.deposits, p.onDeposit
	p.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return p.MockProvider.Deposit(ctx, req)
}

func (p *loopProvider) Withdraw(ctx context.Context, req WithdrawRequest) (WithdrawResult, error) {
	p.mu.Lock()
	p.withdraws++
	n, hook := p.withdraws, p.onWithdraw
	p.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return p.MockProvider.Withdraw(ctx, req)
}

func (p *loopProvider) QueryStatus(ctx context.Context, ref string) (StatusResult, error) {
	p.mu.Lock()
	p.queries++
	n, hook := p.queries, p.onQuery
	p.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	return p.MockProvider.QueryStatus(ctx, ref)
}

func (p *loopProvider) counts() (deposits, withdraws, queries int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.deposits, p.withdraws, p.queries
}

func (p *loopProvider) orchestrator() *Orchestrator {
	id := p.providerID
	return NewOrchestrator(map[string]PaymentProvider{id: p}, MultiWebhookCredentialResolver{id: NewMockWebhookCredentials(p.MockProvider)})
}

// recordingResolver wraps MockCredentialResolver and records the tenant of every
// credential resolution, to prove credentials are resolved only per attempt tenant.
type recordingResolver struct {
	inner MockCredentialResolver
	mu    sync.Mutex
	seen  []uuid.UUID
}

func (r *recordingResolver) Resolve(ctx context.Context, pool providercred.TenantTxRunner, tenantID uuid.UUID, providerID string) (providercred.OutboundCredential, error) {
	r.mu.Lock()
	r.seen = append(r.seen, tenantID)
	r.mu.Unlock()
	return r.inner.Resolve(ctx, pool, tenantID, providerID)
}

func (r *recordingResolver) tenants() map[uuid.UUID]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := map[uuid.UUID]int{}
	for _, id := range r.seen {
		m[id]++
	}
	return m
}

// newLoopSweeper builds a Sweeper like the production wiring does (both KYC gates
// set), with the advisory hint on or off. The deposit KYC gate is the package's
// test-only allow-all (these tests are not about KYC).
func newLoopSweeper(pool *db.Pool, orch *Orchestrator, hint bool, resolver OutboundCredentialResolver) *Sweeper {
	if resolver == nil {
		resolver = MockCredentialResolver{}
	}
	return &Sweeper{
		Pool: pool, Orchestrator: orch, KYCGate: AllowAllDepositKYCGate{},
		PayoutKYCGate: KYCEnforcementPayoutGate{}, CredResolver: resolver,
		Lease: SweeperDefaultLease, TenantAdvisoryHint: hint,
	}
}

func setTenantStatus(t *testing.T, pool *db.Pool, tenantID uuid.UUID, status string) {
	t.Helper()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = $2 WHERE id = $1`, tenantID, status)
		return err
	}); err != nil {
		t.Fatalf("set tenant status %s: %v", status, err)
	}
}

// forceLeaseExpired is the deterministic lease-expiry clock: the attempt's lease
// has lapsed and it is due now.
func forceLeaseExpired(t *testing.T, pool *db.Pool, tenantID, attemptID uuid.UUID) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE payment_attempts SET lease_until = now() - interval '1 second', next_action_at = now() - interval '1 second' WHERE id = $1`, attemptID)
		return err
	}); err != nil {
		t.Fatalf("force lease expiry: %v", err)
	}
}

func ledgerTxCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count ledger transactions: %v", err)
	}
	return n
}

func auditActions(t *testing.T, pool *db.Pool, tenantID uuid.UUID) map[string]int {
	t.Helper()
	m := map[string]int{}
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT action, count(*) FROM audit_log WHERE tenant_id = $1 GROUP BY action`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var a string
			var n int
			if err := rows.Scan(&a, &n); err != nil {
				return err
			}
			m[a] = n
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("audit actions: %v", err)
	}
	return m
}

// dueNow makes a due attempt due again immediately (no sleeping).
func dueNow(t *testing.T, pool *db.Pool, tenantID, attemptID uuid.UUID) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET next_action_at = now() - interval '1 second' WHERE id = $1 AND next_action_at IS NOT NULL`, attemptID)
		return err
	}); err != nil {
		t.Fatalf("make due: %v", err)
	}
}

// pendingDeposit creates a pending deposit attempt at provider p for fixture f and
// returns it plus its provider reference, due now.
func pendingDeposit(t *testing.T, pool *db.Pool, orch *Orchestrator, f orchFixture, key string, amount int64) (PaymentAttempt, string) {
	t.Helper()
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: key,
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.State != AttemptPending || res.Attempt.ProviderReference == nil {
		t.Fatalf("expected a pending attempt with a reference, got %s", res.Attempt.State)
	}
	setNextActionNow(t, pool, f.tenantID, res.Attempt.ID)
	return res.Attempt, *res.Attempt.ProviderReference
}

// idleInTxCount is the S-8 probe: the number of OTHER sessions of this database
// that currently sit inside an open transaction doing nothing, i.e. a transaction
// held across whatever the probing goroutine is doing.
func idleInTxCount(t *testing.T, pool *db.Pool) int {
	t.Helper()
	var n int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid()
			    AND state IN ('idle in transaction', 'idle in transaction (aborted)')`).Scan(&n)
	}); err != nil {
		t.Fatalf("probe idle-in-transaction sessions: %v", err)
	}
	return n
}

func timeNowPlus(d time.Duration) time.Time { return time.Now().Add(d) }

// hangGuard bounds every blocking wait so a broken implementation (or a mutant) fails the test
// instead of hanging it. The bound is a safety net for failure only; no assertion depends on it.
const hangGuard = 60 * time.Second

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hangGuard):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func sendTick(t *testing.T, ch chan<- time.Time) {
	t.Helper()
	select {
	case ch <- time.Now():
	case <-time.After(hangGuard):
		t.Fatal("timed out delivering a tick: the loop is not running")
	}
}

// onceCloser returns an idempotent closer for ch and also runs it at cleanup, so a goroutine held on
// ch can never keep a pooled connection (and so pool.Close) pinned after a failed assertion.
func onceCloser(t *testing.T, ch chan struct{}) func() {
	t.Helper()
	var once sync.Once
	closer := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(closer)
	return closer
}
