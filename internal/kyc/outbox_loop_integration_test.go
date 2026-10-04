//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 section 7.1/7.4, test 58): the worker LOOP - an immediate
// first pass, tick-driven passes (injectable ticker, T-1: no sleeps), per-item
// panic recovery, refusal to start an invalid worker, and the shutdown drain of
// the item in flight.

import (
	"context"
	"sync"
	"testing"
	"time"
)

type passRecorder struct {
	mu    sync.Mutex
	stats []PassStats
	ch    chan PassStats
}

func newPassRecorder() *passRecorder { return &passRecorder{ch: make(chan PassStats, 64)} }

func (p *passRecorder) record(s PassStats) {
	p.mu.Lock()
	p.stats = append(p.stats, s)
	p.mu.Unlock()
	p.ch <- s
}

// next blocks (bounded, as a failure guard only - never as an assertion on
// timing) until the next pass completes.
func (p *passRecorder) next(t *testing.T) PassStats {
	t.Helper()
	select {
	case s := <-p.ch:
		return s
	case <-time.After(30 * time.Second):
		t.Fatal("the loop did not complete a pass")
		return PassStats{}
	}
}

func TestOutboxLoop_ImmediateFirstPass_ThenTickDriven_StopsOnCancel(t *testing.T) {
	r := newRig(t)
	v1 := r.create()
	ticks := make(chan time.Time)
	rec := newPassRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutboxWorkerLoop(ctx, r.w, nil, ticks, rec.record)
	}()

	first := rec.next(t) // no tick has been sent: the first pass is immediate
	if first.Claimed != 1 || first.Results[resultSent] != 1 {
		t.Fatalf("the immediate first pass must send the waiting row, got %+v", first)
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, v1.ID, OpCreate); got.State != OutboxSent {
		t.Fatalf("row = %s", got.State)
	}

	second := seedSecondAccount(t, r.pool, r.f)
	v2, _ := requestCreate(t, r.pool, second, "mock")
	ticks <- time.Now()
	if got := rec.next(t); got.Claimed != 1 {
		t.Fatalf("a tick must run another pass, got %+v", got)
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, v2.ID, OpCreate); got.State != OutboxSent {
		t.Fatalf("row = %s", got.State)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the loop must return after ctx cancellation")
	}
}

func TestOutboxLoop_PanicInOneItemIsRecovered_LoopContinues(t *testing.T) {
	r := newRig(t)
	r.createImpl = func(context.Context, CreateVerificationInput) (ProviderResult, error) { panic("adapter panic") }
	v := r.create()
	ticks := make(chan time.Time)
	rec := newPassRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutboxWorkerLoop(ctx, r.w, nil, ticks, rec.record)
	}()
	first := rec.next(t)
	if first.Results[resultPanic] != 1 {
		t.Fatalf("expected the panic to be recovered per item, got %+v", first.Results)
	}
	if got := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate); got.State != OutboxClaimed {
		t.Fatalf("a panicked item stays claimed (the lease path recovers it), got %s", got.State)
	}
	// The loop is alive: the next tick runs a pass.
	ticks <- time.Now()
	rec.next(t)
	cancel()
	<-done
}

func TestOutboxLoop_InvalidWorkerIsRefusedAndNeverRuns(t *testing.T) {
	r := newRig(t)
	r.create()
	r.w.Config.Lease = time.Second // below the F9 floor
	rec := newPassRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutboxWorkerLoop(context.Background(), r.w, nil, make(chan time.Time), rec.record)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("an invalid worker must be refused immediately")
	}
	if len(rec.stats) != 0 {
		t.Fatal("an invalid worker must run no pass")
	}
	if c, _ := r.calls(); c != 0 {
		t.Fatalf("no vendor call, got %d", c)
	}
}

// Shutdown drain: the context is cancelled while an item is in flight; the item
// finishes (its phase C applies on a detached context), no further row is
// claimed, and the loop returns.
func TestOutboxLoop_ShutdownDrainsTheItemInFlight(t *testing.T) {
	r := newRig(t)
	second := seedSecondAccount(t, r.pool, r.f)
	v1 := r.create()
	v2, _ := requestCreate(t, r.pool, second, "mock")
	ctx, cancel := context.WithCancel(context.Background())
	base := r.createImpl
	r.createImpl = func(c context.Context, in CreateVerificationInput) (ProviderResult, error) {
		cancel() // shutdown arrives mid-call
		return base(c, in)
	}
	rec := newPassRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runOutboxWorkerLoop(ctx, r.w, nil, make(chan time.Time), rec.record)
	}()
	first := rec.next(t)
	<-done
	if first.Claimed != 1 || first.Results[resultSent] != 1 {
		t.Fatalf("the in-flight item must finish and no new row be claimed after cancel, got %+v", first)
	}
	states := map[OutboxState]int{}
	for _, v := range []Verification{v1, v2} {
		states[onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate).State]++
	}
	if states[OutboxSent] != 1 || states[OutboxPending] != 1 {
		t.Fatalf("expected one sent (drained) and one still pending, got %v", states)
	}
}
