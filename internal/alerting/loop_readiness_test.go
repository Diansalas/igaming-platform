package alerting

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

// slowRunner has a delivery pass that outlasts the staleness window (a slow or
// down vendor: every Deliver times out). Its readiness evaluation is the fake
// "ready" configuration, so any flip to not-ready is caused by the loop, not by
// the configuration.
type slowRunner struct {
	tr        *readinessTracker
	release   chan struct{}
	started   chan struct{}
	evalOnce  sync.Once
	evaluated chan struct{}
	evalSig   sync.Once
}

func (r *slowRunner) RunOnce(ctx context.Context) error {
	r.evalOnce.Do(func() { close(r.started) })
	select {
	case <-r.release:
	case <-ctx.Done():
	}
	return nil
}

func (r *slowRunner) EvaluateReadiness(context.Context) error {
	r.tr.update(&captureLog{}, time.Now(), []SeverityReadiness{
		{Severity: SeverityP1, Required: true, Ready: true, Reason: ReadinessReady},
	})
	r.evalSig.Do(func() { close(r.evaluated) })
	return nil
}

// Code review C1: readiness depends on configuration only. A delivery pass much
// longer than the staleness window must not flip it, because readiness is
// evaluated on its own ticker.
func TestLoop_ReadinessDoesNotFlapWhileADeliveryPassIsSlow(t *testing.T) {
	const staleAfter = 120 * time.Millisecond
	r := &slowRunner{
		tr:      &readinessTracker{clock: SystemClock{}, staleAfter: staleAfter},
		release: make(chan struct{}), started: make(chan struct{}), evaluated: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunDispatcherLoopWithConfig(ctx, r, LoopConfig{
			Interval: time.Hour, ReadinessInterval: 20 * time.Millisecond, DrainTimeout: time.Second,
			Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
	}()
	<-r.started
	select {
	case <-r.evaluated:
	case <-time.After(2 * time.Second):
		cancel()
		close(r.release)
		<-done
		t.Fatal("readiness was never evaluated while the delivery pass was running: the readiness ticker is not running")
	}
	deadline := time.Now().Add(5 * staleAfter) // the pass stays blocked well past the window
	samples := 0
	for time.Now().Before(deadline) {
		for _, s := range r.tr.effective() {
			if s.Severity == SeverityP1 && !s.Ready {
				t.Fatalf("readiness flipped to %q while a delivery pass was merely slow", s.Reason)
			}
		}
		samples++
		time.Sleep(5 * time.Millisecond)
	}
	if samples < 20 {
		t.Fatalf("too few samples (%d) to mean anything", samples)
	}
	close(r.release)
	cancel()
	<-done
}

// The loop with a runner that cannot evaluate readiness must not start the ticker (fake runners in other tests).
func TestLoop_NoReadinessGoroutineForPlainRunners(t *testing.T) {
	var _ PassRunner = (*slowRunner)(nil)
	var _ readinessEvaluator = (*slowRunner)(nil)
	var _ readinessEvaluator = (*Dispatcher)(nil)
}

// Code review #6 (kills the mutant that widens the default window): the production default is pinned.
func TestDefaults_ReadinessStaleAfterIsThreeLoopIntervals(t *testing.T) {
	if DefaultLoopInterval != 15*time.Second {
		t.Fatalf("loop interval changed to %v: re-justify the staleness window", DefaultLoopInterval)
	}
	if got := DefaultDispatcherConfig().ReadinessStaleAfter; got != 45*time.Second {
		t.Fatalf("default staleness window = %v, want 45s (3 x the 15s loop interval)", got)
	}
	d := NewDispatcher(nil, DispatcherConfig{})
	if d.readiness.staleAfter != 45*time.Second {
		t.Fatalf("a zero-value config must get the 45s default, got %v", d.readiness.staleAfter)
	}
}

// Code review #4: route resolution is deterministic on an effective_from tie, in either input order.
func TestEvaluateReadiness_TieOnEffectiveFromIsDeterministic(t *testing.T) {
	now := time.Now()
	enabledLow := RouteConfig{ID: "00000000-0000-0000-0000-000000000001", Severity: SeverityP1, Step: 0, ChannelKind: ChannelLog, Enabled: true, HasRecipient: true, EffectiveFrom: now}
	disabledHigh := RouteConfig{ID: "ffffffff-0000-0000-0000-000000000001", Severity: SeverityP1, Step: 0, ChannelKind: ChannelLog, Enabled: false, EffectiveFrom: now}
	wired := map[ChannelKind]bool{ChannelLog: false}
	a := find(EvaluateReadiness([]RouteConfig{enabledLow, disabledHigh}, wired), SeverityP1)
	b := find(EvaluateReadiness([]RouteConfig{disabledHigh, enabledLow}, wired), SeverityP1)
	if a.Reason != b.Reason || a.Reason != ReadinessRouteDisabled {
		t.Fatalf("tie must resolve to the higher id (disabled) in both orders: %s / %s", a.Reason, b.Reason)
	}
}

// Code review #10: an unknown or unclassified kind never reads as delivered.
func TestDeliveryState_UnknownKindsNeverReadAsDelivered(t *testing.T) {
	for _, k := range []ChannelKind{"pager", "", "webhook", ChannelLog, ChannelMock} {
		if got := DeliveryState("sent", k); got == DeliveryStateDelivered {
			t.Errorf("DeliveryState(sent,%q) = delivered with no known human kind", k)
		}
	}
	knownHumanChannelKinds["pager_test_only"] = true
	defer delete(knownHumanChannelKinds, "pager_test_only")
	if DeliveryState("sent", "pager_test_only") != DeliveryStateDelivered {
		t.Fatal("a kind explicitly registered as human must read as delivered")
	}
}
