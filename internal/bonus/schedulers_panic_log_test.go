package bonus

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// PANIC-LOG-VALUE-1: runSchedulerLoop's recovery must log the panic TYPE
// and a stable code only - never the panic value - and the loop must keep
// running after a panicking tick (recovery behaviour unchanged).

const panicLogFakeSecret = "SENTINEL-sk_live_7Qx-jane.doe@example.invalid"

type panicLogBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *panicLogBuf) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *panicLogBuf) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.String()
}

func TestRunSchedulerLoop_PanicValueNeverLogged_LoopContinues(t *testing.T) {
	cases := []struct {
		name     string
		val      any
		wantType string
	}{
		{"string", "token=" + panicLogFakeSecret, "string"},
		{"error", errors.New("vendor said " + panicLogFakeSecret), "*errors.errorString"},
		{"url_error", &url.Error{Op: "Get", URL: "https://x.invalid/?k=" + panicLogFakeSecret, Err: errors.New(panicLogFakeSecret)}, "*url.Error"},
		{"struct", struct{ Email string }{panicLogFakeSecret}, "struct { Email string }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &panicLogBuf{}
			logger := slog.New(slog.NewJSONHandler(buf, nil))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var calls atomic.Int32
			done := make(chan struct{})
			go func() {
				defer close(done)
				runSchedulerLoop(ctx, logger, time.Millisecond, "bonus test job", func() error {
					if calls.Add(1) >= 3 {
						cancel()
						return nil
					}
					panic(tc.val)
				})
			}()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("runSchedulerLoop did not return after cancel")
			}
			if calls.Load() < 3 {
				t.Fatalf("loop stopped after a panicking tick: %d calls", calls.Load())
			}
			out := buf.String()
			if strings.Contains(out, panicLogFakeSecret) || strings.Contains(out, "vendor said") || strings.Contains(out, "x.invalid") {
				t.Fatalf("panic value reached the log: %s", out)
			}
			if strings.Count(out, `"panic_code":"bonus_scheduler_panic"`) != 2 || !strings.Contains(out, `"panic_type":"`+tc.wantType+`"`) {
				t.Fatalf("want two type-only panic lines with type %q, got: %s", tc.wantType, out)
			}
			if strings.Contains(out, "goroutine ") || strings.Contains(out, ".go:") {
				t.Fatalf("stack trace reached the log: %s", out)
			}
		})
	}
}
