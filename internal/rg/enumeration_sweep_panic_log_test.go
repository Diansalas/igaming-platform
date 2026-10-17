package rg

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// PANIC-LOG-VALUE-1: the enumeration sweep scheduler's recovery logs the
// panic TYPE and a stable code only, never the value; the loop keeps
// running after a panicking sweep (recovery behaviour unchanged).

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

func TestRecoverEnumerationSweepPanic_PanicValueNeverLogged(t *testing.T) {
	cases := []struct {
		name     string
		val      any
		wantType string
	}{
		{"string", "password=" + panicLogFakeSecret, "string"},
		{"error", errors.New("db said " + panicLogFakeSecret), "*errors.errorString"},
		{"url_error", &url.Error{Op: "Get", URL: "https://x.invalid/?k=" + panicLogFakeSecret, Err: errors.New(panicLogFakeSecret)}, "*url.Error"},
		{"struct", struct{ Email string }{panicLogFakeSecret}, "struct { Email string }"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := &panicLogBuf{}
			logger := slog.New(slog.NewJSONHandler(buf, nil))
			func() {
				defer recoverEnumerationSweepPanic(logger)
				panic(tc.val)
			}()
			out := buf.String()
			if strings.Contains(out, panicLogFakeSecret) || strings.Contains(out, "db said") || strings.Contains(out, "x.invalid") {
				t.Fatalf("panic value reached the log: %s", out)
			}
			if !strings.Contains(out, `"panic_code":"rg_enumeration_sweep_panic"`) || !strings.Contains(out, `"panic_type":"`+tc.wantType+`"`) {
				t.Fatalf("want a type-only panic line with type %q, got: %s", tc.wantType, out)
			}
			if strings.Contains(out, "goroutine ") || strings.Contains(out, ".go:") {
				t.Fatalf("stack trace reached the log: %s", out)
			}
		})
	}
	// A nil logger must still swallow the panic (unchanged behaviour).
	func() {
		defer recoverEnumerationSweepPanic(nil)
		panic(panicLogFakeSecret)
	}()
}

// The loop itself survives a panicking sweep: a nil pool makes every sweep
// panic (nil dereference) and the loop must keep ticking until ctx ends.
func TestRunEnumerationReconciliationSchedulerLoop_ContinuesAfterPanic(t *testing.T) {
	buf := &panicLogBuf{}
	logger := slog.New(slog.NewJSONHandler(buf, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunEnumerationReconciliationSchedulerLoop(ctx, nil, logger, time.Millisecond, time.Minute)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for strings.Count(buf.String(), `"panic_code":"rg_enumeration_sweep_panic"`) < 3 {
		if time.Now().After(deadline) {
			t.Fatalf("loop did not survive repeated panics: %s", buf.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("loop did not return after cancel")
	}
	if strings.Contains(buf.String(), "nil pointer") {
		t.Fatalf("panic value (runtime error text) reached the log: %s", buf.String())
	}
}
