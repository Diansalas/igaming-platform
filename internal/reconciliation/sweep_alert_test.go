package reconciliation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
)

type sweepFailedCall struct{ phase, class string }

type raiseRecorder struct {
	mu    sync.Mutex
	calls []sweepFailedCall
	err   error
}

func (r *raiseRecorder) install(t *testing.T) {
	t.Helper()
	old := raiseSweepFailed
	t.Cleanup(func() { raiseSweepFailed = old })
	raiseSweepFailed = func(_ context.Context, _ *db.Pool, phase, class string) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls = append(r.calls, sweepFailedCall{phase, class})
		return r.err
	}
}

func logger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

func TestHandleSweepError_ListingFailuresBecomeDurableAlerts(t *testing.T) {
	for _, c := range []struct {
		name      string
		err       error
		wantPhase string
		wantClass string
	}{
		{"active listing, db error", fmt.Errorf("%w: %w", ErrListActiveTenants, &pgconn.PgError{Code: "08006"}), alerting.SweepPhaseListActive, "08"},
		{"active listing, non-pg error", fmt.Errorf("%w: %w", ErrListActiveTenants, errors.New("dial tcp")), alerting.SweepPhaseListActive, "58"},
		{"non-active listing", fmt.Errorf("%w: %w", ErrListNonActiveTenants, &pgconn.PgError{Code: "57014"}), alerting.SweepPhaseListNonActive, "57"},
		{"unclassified sweep error", errors.New("something else"), alerting.SweepPhaseUnknown, "58"},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := &raiseRecorder{}
			rec.install(t)
			l, buf := logger()
			handleSweepError(context.Background(), nil, l, c.err)
			if len(rec.calls) != 1 || rec.calls[0].phase != c.wantPhase || rec.calls[0].class != c.wantClass {
				t.Fatalf("raise calls = %+v, want one {%s %s}", rec.calls, c.wantPhase, c.wantClass)
			}
			if strings.Contains(buf.String(), "LOG-ONLY") {
				t.Errorf("the log line still claims the failure is log-only: %s", buf.String())
			}
		})
	}
}

func TestHandleSweepError_SuccessRaisesNothing(t *testing.T) {
	rec := &raiseRecorder{}
	rec.install(t)
	l, _ := logger()
	handleSweepError(context.Background(), nil, l, nil)
	if len(rec.calls) != 0 {
		t.Fatalf("a clean sweep raised %+v", rec.calls)
	}
}

func TestHandleSweepError_ShutdownCancellationIsNotAFailure(t *testing.T) {
	err := fmt.Errorf("%w: %w", ErrListActiveTenants, context.Canceled)
	rec := &raiseRecorder{}
	rec.install(t)
	l, _ := logger()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	handleSweepError(ctx, nil, l, err)
	if len(rec.calls) != 0 {
		t.Fatalf("graceful shutdown raised a false P1: %+v", rec.calls)
	}
	// ...but a cancellation while the process is still running is a real failure (deadline/driver cancel).
	handleSweepError(context.Background(), nil, l, err)
	if len(rec.calls) != 1 {
		t.Fatalf("a cancellation with a live context must alert, got %+v", rec.calls)
	}
}

func TestRecoverSweepPanic_AlertsAndNeverLogsThePanicValue(t *testing.T) {
	rec := &raiseRecorder{}
	rec.install(t)
	l, buf := logger()
	func() {
		defer recoverSweepPanic(context.Background(), nil, l)
		panic("boom: tenant-1234 secret")
	}()
	if len(rec.calls) != 1 || rec.calls[0].phase != alerting.SweepPhasePanic {
		t.Fatalf("panic raise calls = %+v", rec.calls)
	}
	logs := buf.String()
	if strings.Contains(logs, "tenant-1234") || strings.Contains(logs, "secret") {
		t.Errorf("the panic VALUE leaked into the log: %s", logs)
	}
	if !strings.Contains(logs, "recovered from panic") {
		t.Errorf("the recovered-panic line is missing: %s", logs)
	}
	// no panic, no alert
	func() { defer recoverSweepPanic(context.Background(), nil, l) }()
	if len(rec.calls) != 1 {
		t.Fatalf("a normal return raised: %+v", rec.calls)
	}
}

func TestHandleSweepError_ARaiseFailureIsLoggedAndNeverPanics(t *testing.T) {
	rec := &raiseRecorder{err: errors.New("db down")}
	rec.install(t)
	l, buf := logger()
	handleSweepError(context.Background(), nil, l, fmt.Errorf("%w: x", ErrListActiveTenants))
	if !strings.Contains(buf.String(), "sweep_failed_alert_not_written") {
		t.Errorf("a failed alert write must be logged: %s", buf.String())
	}
}
