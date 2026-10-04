package alerting

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeTicker struct{ ch chan time.Time }

func (f *fakeTicker) C() <-chan time.Time { return f.ch }
func (f *fakeTicker) Stop()               {}

type fakeRunner struct {
	mu      sync.Mutex
	calls   int
	fn      func(ctx context.Context, call int) error
	started chan int
}

func (f *fakeRunner) RunOnce(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.mu.Unlock()
	if f.started != nil {
		f.started <- n
	}
	if f.fn != nil {
		return f.fn(ctx, n)
	}
	return nil
}

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runLoop starts the loop and returns the tick channel, the results channel
// and a done channel closed when the loop returns.
func runLoop(ctx context.Context, r PassRunner, drain time.Duration) (chan time.Time, chan string, chan struct{}) {
	tick := make(chan time.Time)
	results := make(chan string, 16)
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunDispatcherLoopWithConfig(ctx, r, LoopConfig{
			Interval:     time.Hour,
			DrainTimeout: drain,
			Logger:       quietLogger(),
			NewTicker:    func(time.Duration) Ticker { return &fakeTicker{ch: tick} },
			OnPass:       func(res string) { results <- res },
		})
	}()
	return tick, results, done
}

func TestLoop_ImmediateFirstPassThenOnePerTick(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeRunner{}
	tick, results, done := runLoop(ctx, r, time.Hour)

	if got := <-results; got != "ok" {
		t.Fatalf("first pass result %q, want ok (an immediate pass before any tick)", got)
	}
	tick <- time.Time{}
	<-results
	tick <- time.Time{}
	<-results
	cancel()
	<-done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 3 {
		t.Fatalf("calls = %d, want 3 (immediate + 2 ticks)", r.calls)
	}
}

func TestLoop_PanicIsRecoveredAndLoopContinues(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	r := &fakeRunner{fn: func(_ context.Context, call int) error {
		if call == 1 {
			panic("boom")
		}
		if call == 2 {
			return errors.New("db down")
		}
		return nil
	}}
	tick, results, done := runLoop(ctx, r, time.Hour)
	want := []string{"panic", "error", "ok"}
	for i, w := range want {
		if i > 0 {
			tick <- time.Time{}
		}
		if got := <-results; got != w {
			t.Fatalf("pass %d result %q, want %q", i+1, got, w)
		}
	}
	cancel()
	<-done
}

func TestLoop_ShutdownDrainsInFlightPassAndStartsNoNewOne(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	var passCtxErrAfterCancel error
	r := &fakeRunner{started: make(chan int, 4), fn: func(pc context.Context, _ int) error {
		<-release
		passCtxErrAfterCancel = pc.Err()
		return nil
	}}
	_, results, done := runLoop(ctx, r, time.Hour) // drain bound far away
	<-r.started
	cancel() // shutdown while the pass is in flight
	select {
	case <-done:
		t.Fatal("loop returned while a pass was still in flight (no drain)")
	default:
	}
	close(release)
	<-done
	if passCtxErrAfterCancel != nil {
		t.Fatalf("in-flight pass context was cancelled with the parent (%v); it must drain", passCtxErrAfterCancel)
	}
	if got := <-results; got != "ok" {
		t.Fatalf("drained pass result %q", got)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 1 {
		t.Fatalf("calls = %d, want 1: no new pass after shutdown", r.calls)
	}
}

func TestLoop_DrainTimeoutCancelsAWedgedPass(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	wedged := make(chan struct{})
	r := &fakeRunner{started: make(chan int, 1), fn: func(pc context.Context, _ int) error {
		<-pc.Done() // only the drain timeout can end this pass
		close(wedged)
		return pc.Err()
	}}
	_, results, done := runLoop(ctx, r, time.Millisecond)
	<-r.started
	cancel()
	<-wedged
	<-done
	if got := <-results; got != "error" {
		t.Fatalf("wedged pass result %q, want error", got)
	}
}

func TestLoop_NoPassWhenAlreadyCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := &fakeRunner{}
	_, _, done := runLoop(ctx, r, time.Hour)
	<-done
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.calls != 0 {
		t.Fatalf("calls = %d, want 0 on an already-cancelled context", r.calls)
	}
}
