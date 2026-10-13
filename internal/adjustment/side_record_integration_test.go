//go:build integration

package adjustment

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// lockedBuf is a goroutine-safe log sink (a late background write must not race the test's read).
type lockedBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuf) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func captureLogs(t *testing.T) *lockedBuf {
	t.Helper()
	buf := &lockedBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return buf
}

// A failed side write is logged with a GENERIC event name and the correlation id (never AML wording or error text), not discarded.
func TestSideRecord_FailureIsLoggedGenericallyWithTheCorrelationID(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	buf := captureLogs(t)
	corr := uuid.New()
	svc := NewService(w.pool)
	// An account that does not belong to the person makes the evidence write fail inside the database.
	svc.recordAMLRefusal(context.Background(), &amlRefusal{
		holdCheck: holdCheck{verdict: aml.HoldVerdict{Held: true}, tenantID: w.Tenant, accountID: uuid.New(), personID: uuid.New()},
		requestID: corr,
	})
	out := buf.String()
	if !strings.Contains(out, "adjustment_side_record_failed") || !strings.Contains(out, corr.String()) || !strings.Contains(out, "step=record") {
		t.Fatalf("the failed side write must be logged with its correlation id: %q", out)
	}
	low := strings.ToLower(out)
	for _, bad := range []string{"aml", "hold", "account does not belong"} {
		if strings.Contains(low, bad) {
			t.Fatalf("the failure log must not carry AML wording or database error text (%q): %q", bad, out)
		}
	}
}

// A starved pool cannot hang the request: the side writes are bounded.
func TestSideRecord_StarvedPoolIsBounded(t *testing.T) {
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	pool, err := db.Connect(context.Background(), url, 1, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	held, err := pool.Raw().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	buf := captureLogs(t)
	svc := NewService(pool)
	start := time.Now()
	svc.recordAMLRefusal(context.Background(), &amlRefusal{
		holdCheck: holdCheck{verdict: aml.HoldVerdict{EvalFailed: true}, tenantID: uuid.New(), accountID: uuid.New(), personID: uuid.New()},
		requestID: uuid.New(),
	})
	// ABSOLUTE bound (not derived from the constant under test): two side writes of 2 s each, plus slack.
	if d := time.Since(start); d > 5500*time.Millisecond {
		t.Fatalf("a starved pool must not hang the refusal: took %s", d)
	}
	if !strings.Contains(buf.String(), "adjustment_side_record_failed") {
		t.Fatalf("the timeout must be logged: %q", buf.String())
	}
}
