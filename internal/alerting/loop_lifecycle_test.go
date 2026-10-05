package alerting

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Lifecycle of the readiness goroutine started by RunDispatcherLoopWithConfig
// (security delta finding 4): panic recovery, shutdown drain, the evaluation
// time bound, and "a plain runner gets no readiness goroutine".

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

type lcRunner struct {
	mode     string
	evals    atomic.Int32
	exited   atomic.Bool
	timedOut atomic.Bool
}

func (r *lcRunner) RunOnce(context.Context) error { return nil }

func (r *lcRunner) EvaluateReadiness(ctx context.Context) error {
	r.evals.Add(1)
	switch r.mode {
	case "panic":
		panic("SECRET-PANIC-VALUE-5c2e")
	case "error":
		return errors.New("postgres://user:SECRET-PW-9f1@host/db failed")
	case "block": // honours ctx; takes a moment to wind down after cancel
		<-ctx.Done()
		time.Sleep(40 * time.Millisecond)
		r.exited.Store(true)
		return ctx.Err()
	case "deadline": // never cancelled by the test: only the evaluation bound can end it
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			r.timedOut.Store(true)
		}
		return ctx.Err()
	}
	return nil
}

func runLoopFor(t *testing.T, r PassRunner, interval time.Duration, d time.Duration) (*syncBuf, time.Duration) {
	t.Helper()
	buf := &syncBuf{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunDispatcherLoopWithConfig(ctx, r, LoopConfig{
			Interval: time.Hour, ReadinessInterval: interval, DrainTimeout: time.Second,
			Logger: slog.New(slog.NewTextHandler(buf, nil)),
		})
	}()
	time.Sleep(d)
	start := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not return after cancel")
	}
	return buf, time.Since(start)
}

func TestLoop_ReadinessEvaluationPanicIsRecoveredAndTheTickerKeepsRunning(t *testing.T) {
	r := &lcRunner{mode: "panic"}
	logs, _ := runLoopFor(t, r, 5*time.Millisecond, 80*time.Millisecond)
	if n := r.evals.Load(); n < 3 {
		t.Fatalf("only %d evaluations: a panic must not stop the ticker", n)
	}
	out := logs.String()
	if strings.Contains(out, "SECRET-PANIC-VALUE") || !strings.Contains(out, "panic_type=string") {
		t.Fatalf("the panic must be logged by TYPE only: %s", out)
	}
}

func TestLoop_ReadinessEvaluationErrorIsLoggedByTypeOnly(t *testing.T) {
	r := &lcRunner{mode: "error"}
	logs, _ := runLoopFor(t, r, 5*time.Millisecond, 40*time.Millisecond)
	out := logs.String()
	if strings.Contains(out, "SECRET-PW") || !strings.Contains(out, "alert_routing_readiness_evaluation_failed") {
		t.Fatalf("readiness errors are logged by type only: %s", out)
	}
}

// Shutdown waits for the readiness goroutine: it has fully exited when the loop returns.
func TestLoop_ShutdownWaitsForTheReadinessGoroutine(t *testing.T) {
	r := &lcRunner{mode: "block"}
	runLoopFor(t, r, 5*time.Millisecond, 30*time.Millisecond)
	if !r.exited.Load() {
		t.Fatal("the loop returned while the readiness evaluation was still winding down (leaked goroutine)")
	}
}

// One evaluation is bounded by readinessEvalTimeout even when nobody cancels the loop.
func TestLoop_ReadinessEvaluationIsTimeBounded(t *testing.T) {
	old := readinessEvalTimeout
	readinessEvalTimeout = 40 * time.Millisecond
	defer func() { readinessEvalTimeout = old }()
	r := &lcRunner{mode: "deadline"}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunDispatcherLoopWithConfig(ctx, r, LoopConfig{Interval: time.Hour, ReadinessInterval: time.Hour, DrainTimeout: time.Second,
			Logger: slog.New(slog.NewTextHandler(&syncBuf{}, nil))})
	}()
	deadline := time.Now().Add(2 * time.Second)
	for !r.timedOut.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	timedOut := r.timedOut.Load()
	cancel()
	<-done
	if !timedOut {
		t.Fatal("a stuck readiness evaluation was not ended by its time bound")
	}
}

type plainBlockRunner struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *plainBlockRunner) RunOnce(ctx context.Context) error {
	p.once.Do(func() { close(p.started) })
	select {
	case <-p.release:
	case <-ctx.Done():
	}
	return nil
}

// A runner that cannot evaluate readiness gets no readiness goroutine.
func TestLoop_PlainRunnerStartsNoReadinessGoroutine(t *testing.T) {
	p := &plainBlockRunner{started: make(chan struct{}), release: make(chan struct{})}
	time.Sleep(20 * time.Millisecond)
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunDispatcherLoopWithConfig(ctx, p, LoopConfig{Interval: time.Hour, DrainTimeout: time.Second, Logger: slog.New(slog.NewTextHandler(&syncBuf{}, nil))})
	}()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("loop never started a pass")
	}
	if extra := runtime.NumGoroutine() - base; extra > 1 {
		t.Fatalf("%d extra goroutines for a plain runner, want only the loop itself", extra)
	}
	close(p.release)
	cancel()
	<-done
}

// Security delta finding 1: an older, slower evaluation never overwrites a newer snapshot.
func TestReadinessTracker_OlderEvaluationCannotOverwriteANewerOne(t *testing.T) {
	now := time.Now()
	tr := &readinessTracker{clock: &manualClockUnit{now: now}, staleAfter: time.Hour}
	lg := &captureLog{}
	notReady := []SeverityReadiness{{Severity: SeverityP1, Required: true, Reason: ReadinessRouteDisabled}}
	ready := []SeverityReadiness{{Severity: SeverityP1, Required: true, Ready: true, Reason: ReadinessReady}}
	tr.update(lg, now.Add(2*time.Second), notReady) // newer evaluation publishes first
	tr.update(lg, now.Add(time.Second), ready)      // older, slower one publishes last
	got := find(tr.effective(), SeverityP1)
	if got.Ready {
		t.Fatal("an older evaluation regressed the snapshot to READY")
	}
	if !tr.snap.EvaluatedAt.Equal(now.Add(2 * time.Second)) {
		t.Fatalf("evaluated_at went backwards: %v", tr.snap.EvaluatedAt)
	}
	if strings.Contains(lg.buf.String(), "alert_routing_ready ") {
		t.Fatalf("a dropped result must not log a transition: %s", lg.buf.String())
	}
	tr.update(lg, now.Add(3*time.Second), ready) // a genuinely newer one is accepted
	if !find(tr.effective(), SeverityP1).Ready {
		t.Fatal("a newer evaluation must be accepted")
	}
}
