package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"
)

type fakeDirectoryPool struct {
	mu    sync.Mutex
	slugs []string
	err   error
	calls int
}

func (f *fakeDirectoryPool) ListActiveTenantSlugs(ctx context.Context, limit int) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return f.slugs, nil
}

func TestWebhookTenantDirectory_LoadAndContains(t *testing.T) {
	pool := &fakeDirectoryPool{slugs: []string{"acme", "beta"}}
	d := newWebhookTenantDirectory(pool, 100, slog.Default())
	if d.Loaded() {
		t.Fatal("must not be Loaded() before the first Load")
	}
	if err := d.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !d.Loaded() {
		t.Fatal("must be Loaded() after a successful Load")
	}
	if !d.Contains("acme") || d.Contains("unknown-tenant") {
		t.Fatal("Contains must reflect the loaded snapshot exactly")
	}
	if d.Size() != 2 {
		t.Fatalf("Size() = %d, want 2", d.Size())
	}
}

// TestWebhookTenantDirectory_RefreshFailureKeepsLastSnapshot is T9's
// fail-safe case for the directory: a refresh error never wipes a good
// snapshot (ADR 0097 §7).
func TestWebhookTenantDirectory_RefreshFailureKeepsLastSnapshot(t *testing.T) {
	pool := &fakeDirectoryPool{slugs: []string{"acme"}}
	d := newWebhookTenantDirectory(pool, 100, slog.Default())
	if err := d.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	pool.err = errors.New("db down")
	if err := d.Load(context.Background()); err == nil {
		t.Fatal("expected the injected error")
	}
	if !d.Contains("acme") {
		t.Fatal("a failed refresh must keep the last good snapshot")
	}
}

func TestWebhookTenantDirectory_TruncationLogged(t *testing.T) {
	pool := &fakeDirectoryPool{slugs: []string{"a", "b", "c"}}
	d := newWebhookTenantDirectory(pool, 2, slog.Default()) // cap smaller than slug count
	if err := d.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Not asserting the log line's exact text here (that's covered by
	// T7's allow-list scope); asserting only that a smaller cap doesn't
	// crash and the directory still holds a bounded, non-empty snapshot.
	if d.Size() == 0 {
		t.Fatal("directory must still hold entries after a truncated load")
	}
}

// TestWebhookTenantDirectory_RunExitsOnShutdown is devops condition 2's
// explicit requirement: the background refresher goroutine exits when its
// context is cancelled, and does not fire another refresh afterward.
func TestWebhookTenantDirectory_RunExitsOnShutdown(t *testing.T) {
	pool := &fakeDirectoryPool{slugs: []string{"acme"}}
	d := newWebhookTenantDirectory(pool, 100, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		d.Run(ctx, time.Millisecond)
		close(done)
	}()

	// Let it refresh a few times, then cancel and confirm Run returns
	// promptly (bounded, generous real-time wait - this is test
	// synchronization for a goroutine lifecycle, not a timing assertion
	// on the admission mechanisms themselves, exactly like
	// waitForTimerRegistered in internal/admission's own tests).
	deadline := time.Now().Add(2 * time.Second)
	for {
		pool.mu.Lock()
		calls := pool.calls
		pool.mu.Unlock()
		if calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Run never called ListActiveTenantSlugs")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not exit after its context was cancelled")
	}

	pool.mu.Lock()
	callsAtCancel := pool.calls
	pool.mu.Unlock()
	time.Sleep(20 * time.Millisecond)
	pool.mu.Lock()
	callsAfter := pool.calls
	pool.mu.Unlock()
	if callsAfter != callsAtCancel {
		t.Fatalf("Run kept refreshing after shutdown: %d calls before, %d after waiting", callsAtCancel, callsAfter)
	}
}
