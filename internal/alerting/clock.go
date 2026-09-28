package alerting

import (
	"context"
	"time"
)

// Clock is the injectable time source this package uses everywhere it
// would otherwise call time.Now()/time.Sleep() - the detached-retry
// backoff (§7.3) and the dispatcher's "due work" query (§6.1, T-1: "now
// comes from the injected Clock, passed as a query parameter"). Tests
// inject a fake Clock instead of sleeping or asserting on wall-clock time
// (plan rule T-1/T-2).
type Clock interface {
	Now() time.Time
	Sleep(ctx context.Context, d time.Duration)
}

// SystemClock is the real, production Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// Sleep respects ctx cancellation, unlike a bare time.Sleep - so a
// shutting-down dispatcher or an exhausted detached-retry budget is not
// blocked past its own context's deadline.
func (SystemClock) Sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
	case <-ctx.Done():
	}
}

// detachedCtx mirrors internal/httpserver's deniedAuditCtx pattern
// exactly (payments_kill_switch_handlers.go): it detaches a post-response
// write from the caller's own (possibly already-cancelled) context and
// bounds it with a short timeout, so a client disconnect never silently
// skips the mandatory detached retry (§7.3).
const detachedTimeout = 5 * time.Second

func detachedCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), detachedTimeout)
}

// clockCtxKey lets Pending.Flush hand its (possibly test-injected) Clock
// to RaiseDetached's retry backoff without widening RaiseDetached's own
// signature (which the ADR fixes as (ctx, r, a) error) and without a
// package-level mutable Clock variable that parallel tests would race on.
type clockCtxKey struct{}

// WithClockContext attaches c to ctx for RaiseDetached's retry backoff to
// find. Direct I-wire call sites never need this (SystemClock is the
// correct default); it exists for tests.
func WithClockContext(ctx context.Context, c Clock) context.Context {
	return context.WithValue(ctx, clockCtxKey{}, c)
}

func clockFromContext(ctx context.Context) Clock {
	if c, ok := ctx.Value(clockCtxKey{}).(Clock); ok {
		return c
	}
	return SystemClock{}
}
