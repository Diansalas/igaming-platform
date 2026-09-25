//go:build integration

package payments

// Stage 10 F-7 remediation (ADR 0020 amendment 2026-09-25,
// docs/governance/stage-10-f7-ledger-replay-audit.md §3 sites #19-#21):
// a legitimate reversal/tombstone redelivery still resolves to the
// original ledger transaction; a reversal reference reused for a
// DIFFERENT deposit - which used to return success while that deposit was
// never debited - is now ErrCallbackPayloadMismatch with no ledger effect.

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

type f7PaymentsEnv struct {
	pool     *db.Pool
	f        orchFixture
	provider *MockProvider
	orch     *Orchestrator
}

func newF7PaymentsEnv(t *testing.T) f7PaymentsEnv {
	t.Helper()
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	return f7PaymentsEnv{pool: pool, f: f, provider: provider, orch: NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider})}
}

func (e f7PaymentsEnv) deliver(payload []byte) (ReceiveCallbackResult, error) {
	var res ReceiveCallbackResult
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = e.orch.ReceiveCallback(ctx, tx, e.f.tenantID, "mock-psp", payload)
		return err
	})
	return res, err
}

// succeededDeposit initiates and confirms one deposit, returning its
// provider reference.
func (e f7PaymentsEnv) succeededDeposit(t *testing.T, key string, amount int64) string {
	t.Helper()
	var intent DepositIntent
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = e.orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: e.f.tenantID, BrandID: e.f.brandID, PlayerAccountID: e.f.playerAccountID, WalletID: e.f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: key,
		})
		return err
	}); err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	ref := *intent.ProviderReference
	if _, err := e.deliver(e.provider.CallbackPayload(CallbackEventDeposit, ref, "", OutcomeSucceeded, amount, "EUR", "", false)); err != nil {
		t.Fatalf("deposit success callback: %v", err)
	}
	return ref
}

func (e f7PaymentsEnv) reversal(ref, originalRef string, amount int64) []byte {
	return e.provider.CallbackPayload(CallbackEventDepositReversal, ref, originalRef, OutcomeDeclined, amount, "EUR", "chargeback", false)
}

func (e f7PaymentsEnv) ledgerTxCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, e.f.tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// A sequential redelivery of the SAME reversal reference is idempotent
// success with the original reversal's id. Before the F-7 item it was
// rejected with ErrDepositAlreadyReversed (audit §6.2 observation: the
// already-reversed check did not exclude the reversal's own reference).
func TestF7Payments_SequentialReversalRedeliveryIsIdempotent(t *testing.T) {
	e := newF7PaymentsEnv(t)
	dep := e.succeededDeposit(t, "f7-seq-rev", 4_000)

	first, err := e.deliver(e.reversal("f7-rev-seq", dep, 4_000))
	if err != nil {
		t.Fatalf("first reversal: %v", err)
	}
	count := e.ledgerTxCount(t)
	second, err := e.deliver(e.reversal("f7-rev-seq", dep, 4_000))
	if err != nil {
		t.Fatalf("redelivered reversal: %v", err)
	}
	if *second.LedgerTransactionID != *first.LedgerTransactionID {
		t.Fatalf("redelivery resolved to %s, want %s", *second.LedgerTransactionID, *first.LedgerTransactionID)
	}
	if got := e.ledgerTxCount(t); got != count {
		t.Fatalf("redelivery posted: %d -> %d", count, got)
	}
	if got := cashBalance(t, e.pool, e.f); got != 0 {
		t.Fatalf("player_cash = %d, want 0 (reversed once)", got)
	}
	// A distinct reference for the same deposit is still rejected.
	if _, err := e.deliver(e.reversal("f7-rev-seq-other", dep, 4_000)); !errors.Is(err, ErrDepositAlreadyReversed) {
		t.Fatalf("distinct second reversal: want ErrDepositAlreadyReversed, got %v", err)
	}
}

// Concurrent identical reversal deliveries: one posting, every caller gets
// the same reversal id.
func TestF7Payments_ConcurrentIdenticalReversalRedelivery(t *testing.T) {
	e := newF7PaymentsEnv(t)
	dep := e.succeededDeposit(t, "f7-conc-rev", 2_500)
	const n = 6
	results := make([]ReceiveCallbackResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = e.deliver(e.reversal("f7-rev-conc", dep, 2_500))
		}(i)
	}
	close(start)
	wg.Wait()
	var id uuid.UUID
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if id == uuid.Nil {
			id = *results[i].LedgerTransactionID
		} else if *results[i].LedgerTransactionID != id {
			t.Fatalf("attempt %d resolved to %s, want %s", i, *results[i].LedgerTransactionID, id)
		}
	}
	if got := cashBalance(t, e.pool, e.f); got != 0 {
		t.Fatalf("player_cash = %d, want 0 (reversed once)", got)
	}
}

// Site #21: concurrent reversals naming one never-posted original all
// resolve to the one tombstone (fresh tombstone correlation per call -
// relies on the TxTombstone correlation exemption).
func TestF7Payments_ConcurrentTombstoneRedeliveryIsIdempotent(t *testing.T) {
	e := newF7PaymentsEnv(t)
	const n = 6
	results := make([]ReceiveCallbackResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = e.deliver(e.reversal("f7-rev-tomb", "f7-never-posted", 100))
		}(i)
	}
	close(start)
	wg.Wait()
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("attempt %d: %v", i, errs[i])
		}
		if !results[i].Tombstoned || *results[i].LedgerTransactionID != *results[0].LedgerTransactionID {
			t.Fatalf("attempt %d: %+v, want the one tombstone", i, results[i])
		}
	}
	// A later sequential redelivery too.
	again, err := e.deliver(e.reversal("f7-rev-tomb", "f7-never-posted", 100))
	if err != nil || *again.LedgerTransactionID != *results[0].LedgerTransactionID {
		t.Fatalf("sequential tombstone redelivery: %+v %v", again, err)
	}
}

// Site #20: reversal reference R1, already used to reverse D1, redelivered
// naming a different, unreversed deposit D2.
func TestF7Payments_ReversalRefReusedForDifferentDepositRejected(t *testing.T) {
	e := newF7PaymentsEnv(t)
	d1 := e.succeededDeposit(t, "f7-c20-d1", 1_000)
	d2 := e.succeededDeposit(t, "f7-c20-d2", 1_000)
	if _, err := e.deliver(e.reversal("f7-rev-c20", d1, 1_000)); err != nil {
		t.Fatalf("reverse D1: %v", err)
	}
	if got := cashBalance(t, e.pool, e.f); got != 1_000 {
		t.Fatalf("player_cash = %d, want 1000", got)
	}

	count := e.ledgerTxCount(t)
	res, err := e.deliver(e.reversal("f7-rev-c20", d2, 1_000))
	if !errors.Is(err, ErrCallbackPayloadMismatch) {
		t.Fatalf("want ErrCallbackPayloadMismatch, got res=%+v err=%v", res, err)
	}
	if got := e.ledgerTxCount(t); got != count {
		t.Fatalf("rejected replay posted: %d -> %d", count, got)
	}
	if got := cashBalance(t, e.pool, e.f); got != 1_000 {
		t.Fatalf("player_cash = %d, want 1000 (D2 untouched)", got)
	}
	// D2 can still be genuinely reversed under its own reference.
	if _, err := e.deliver(e.reversal("f7-rev-c20-d2", d2, 1_000)); err != nil {
		t.Fatalf("genuine reversal of D2: %v", err)
	}
	if got := cashBalance(t, e.pool, e.f); got != 0 {
		t.Fatalf("player_cash = %d, want 0", got)
	}
}
