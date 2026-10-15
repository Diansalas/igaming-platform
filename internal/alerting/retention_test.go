package alerting

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakePurger struct {
	mu      sync.Mutex
	calls   []time.Duration
	batches []int
	results []PurgeResult
	err     error
	panics  bool
}

func (f *fakePurger) PurgeResolved(_ context.Context, older time.Duration, batch int) (PurgeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, older)
	f.batches = append(f.batches, batch)
	if f.panics {
		panic("boom with a secret value")
	}
	if f.err != nil {
		return PurgeResult{}, f.err
	}
	if len(f.results) == 0 {
		return PurgeResult{}, nil
	}
	r := f.results[0]
	f.results = f.results[1:]
	return r, nil
}

type manualTicker struct{ c chan time.Time }

func (m manualTicker) C() <-chan time.Time { return m.c }
func (m manualTicker) Stop()               {}

var lastRetentionResults []string

func runRetention(t *testing.T, p Purger, cfg RetentionConfig, ticks int) (calls func() int, logs func() string) {
	t.Helper()
	lastRetentionResults = nil
	var buf bytes.Buffer
	var bmu sync.Mutex
	cfg.Logger = slog.New(slog.NewTextHandler(writerFn(func(b []byte) (int, error) { bmu.Lock(); defer bmu.Unlock(); return buf.Write(b) }), nil))
	tk := manualTicker{c: make(chan time.Time)}
	cfg.NewTicker = func(time.Duration) Ticker { return tk }
	done := make(chan string, ticks+1)
	cfg.OnTick = func(r string) { lastRetentionResults = append(lastRetentionResults, r); done <- r }
	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	go func() { RunRetentionLoop(ctx, p, cfg); close(finished) }()
	<-done // immediate first tick
	for i := 0; i < ticks; i++ {
		tk.c <- time.Now()
		<-done
	}
	cancel()
	<-finished
	f := p.(*fakePurger)
	return func() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) },
		func() string { bmu.Lock(); defer bmu.Unlock(); return buf.String() }
}

type writerFn func([]byte) (int, error)

func (w writerFn) Write(p []byte) (int, error) { return w(p) }

func TestRetentionLoop_PurgesOnStartAndEveryTickWithTheConfiguredWindow(t *testing.T) {
	f := &fakePurger{}
	calls, _ := runRetention(t, f, RetentionConfig{OlderThan: 400 * 24 * time.Hour, Batch: 123}, 2)
	if calls() != 3 {
		t.Fatalf("purge calls = %d, want 3 (start + 2 ticks)", calls())
	}
	for i, c := range f.calls {
		if c != 400*24*time.Hour || f.batches[i] != 123 {
			t.Fatalf("call %d used window %v batch %d", i, c, f.batches[i])
		}
	}
}

func TestRetentionLoop_ChainsCappedBatchesButOnlyUpToTheBound(t *testing.T) {
	f := &fakePurger{results: []PurgeResult{{Alerts: 5, Capped: true}, {Alerts: 5, Capped: true}, {Alerts: 1}}}
	calls, _ := runRetention(t, f, RetentionConfig{OlderThan: 8 * 24 * time.Hour, MaxBatchesPerTick: 10}, 0)
	if calls() != 3 {
		t.Fatalf("calls = %d, want 3 (two capped batches then a final one)", calls())
	}
	g := &fakePurger{results: []PurgeResult{{Capped: true}, {Capped: true}, {Capped: true}, {Capped: true}, {Capped: true}}}
	calls, _ = runRetention(t, g, RetentionConfig{OlderThan: 8 * 24 * time.Hour, MaxBatchesPerTick: 3}, 0)
	if calls() != 3 {
		t.Fatalf("calls = %d, want exactly the MaxBatchesPerTick bound 3", calls())
	}
}

func TestRetentionLoop_ErrorsAndPanicsAreContainedAndLoggedWithoutValues(t *testing.T) {
	f := &fakePurger{err: errors.New("pq: password=hunter2")}
	calls, logs := runRetention(t, f, RetentionConfig{OlderThan: 8 * 24 * time.Hour}, 2)
	if calls() != 3 {
		t.Fatalf("the loop stopped after an error: %d calls", calls())
	}
	if strings.Contains(logs(), "hunter2") {
		t.Errorf("error text leaked into the log: %s", logs())
	}
	for _, r := range lastRetentionResults {
		if r != "error" {
			t.Errorf("a failing purge tick reported %q, want error", r)
		}
	}
	p := &fakePurger{panics: true}
	calls, logs = runRetention(t, p, RetentionConfig{OlderThan: 8 * 24 * time.Hour}, 1)
	if calls() != 2 {
		t.Fatalf("the loop stopped after a panic: %d calls", calls())
	}
	if strings.Contains(logs(), "secret value") || !strings.Contains(logs(), "alert_retention_panic") {
		t.Errorf("panic log = %s", logs())
	}
	for _, r := range lastRetentionResults {
		if r != "panic" {
			t.Errorf("a panicking purge tick reported %q, want panic", r)
		}
	}
}

func TestRetentionLoop_RefusesAWindowBelowTheFloor(t *testing.T) {
	for _, w := range []time.Duration{0, time.Hour, 6 * 24 * time.Hour} {
		if err := ValidateRetentionConfig(RetentionConfig{OlderThan: w}); err == nil {
			t.Errorf("window %v accepted", w)
		}
		f := &fakePurger{}
		done := make(chan struct{})
		go func() { RunRetentionLoop(context.Background(), f, RetentionConfig{OlderThan: w}); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("the loop ran with an invalid window %v", w)
		}
		if len(f.calls) != 0 {
			t.Errorf("purged with an invalid window %v", w)
		}
	}
	if err := ValidateRetentionConfig(RetentionConfig{OlderThan: 8 * 24 * time.Hour, Batch: 50001}); err == nil {
		t.Error("batch above the database maximum accepted")
	}
	if err := ValidateRetentionConfig(RetentionConfig{OlderThan: 7 * 24 * time.Hour}); err != nil {
		t.Errorf("the 7-day floor itself must be accepted: %v", err)
	}
}
