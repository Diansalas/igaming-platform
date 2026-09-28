package alerting

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// RaiseDetached opens a fresh transaction in EXACTLY r's originating
// scope and raises a (bounded-retry, then terminal-fallback) alert there
// (ADR §5/§7.3/§7.3a/§7.4). It is the single building block behind:
//   - the mandatory post-swallow retry from Pending.Flush;
//   - the REPEATABLE READ sites' post-commit-only raise (§7.3a);
//   - failure-path P1s whose business transaction rolled back (§7.4);
//   - the kill-switch engage alert (post-commit, detached only, §7.6).
//
// C-102-1/SR-1: the scope comes ONLY from r, never from a - there is no
// code path here that reads a field of Alert to decide which scope to
// reopen. A platform-owned Kind's subject that does not match r's own
// tenant (when r has one) is refused in Go, before any SQL, is logged and
// counted, and is returned immediately with NO retry and NO fallback
// synthesis (a caller/scope-laundering bug must be visible, not papered
// over as a generic P1).
//
// A Go validation failure (bad Kind/attributes/discriminator) is also
// refused before any SQL, but DOES get the §6.3 terminal fallback
// immediately (with sqlstate_class="go_validation") since re-attempting
// would fail identically every time.
//
// Otherwise this makes up to len(flushBackoff) attempts, using the Clock
// found via WithClockContext (SystemClock by default - T-1), with
// backoff between attempts. If every attempt fails, the §6.3 terminal
// fallback persists alerting.raise_failed with the LAST attempt's
// sqlstate_class, and this function still returns the last error (real
// call sites are expected to log-and-continue, never treat it as a
// business failure - see §7.6: "the business outcome never depends on the
// alert row").
//
// Security IC-5 / code review F-2 / LF C-3: RaiseDetached detaches ITS
// OWN context (the deniedAuditCtx pattern, detachedCtx in clock.go) at
// entry, exactly once for the whole call - a caller's context being
// cancelled (an HTTP request finishing, a shutdown signal) must never
// silently skip the mandatory detached retry OR the terminal fallback.
// context.WithoutCancel preserves context VALUES from the parent
// (including a WithClockContext-injected fake Clock, which is what keeps
// this safe for tests that need deterministic time), it only detaches
// cancellation/deadline.
func RaiseDetached(ctx context.Context, r ScopedRunner, a Alert) error {
	dctx, cancel := detachedCtx(ctx)
	defer cancel()

	if err := checkSubjectMatchesScope(r.Scope(), a); err != nil {
		recordRaiseFailure(dctx, a.Kind, "detached")
		slog.Default().Error("alert_raise_scope_mismatch", "kind", a.Kind)
		return err
	}
	if _, err := a.validate(); err != nil {
		recordRaiseFailure(dctx, a.Kind, "detached")
		slog.Default().Error("alert_raise_invalid", "kind", a.Kind)
		raiseFailed(dctx, r.Pool(), a.Kind, sqlstateClassGoValidation)
		return err
	}

	clock := clockFromContext(dctx)
	var lastErr error
	for attempt, wait := range flushBackoff {
		if attempt > 0 {
			clock.Sleep(dctx, wait)
		}
		lastErr = r.Run(dctx, func(ctx context.Context, tx pgx.Tx) error {
			return Raise(ctx, tx, a)
		})
		if lastErr == nil {
			return nil
		}
		var invalid *invalidAlertError
		if errors.As(lastErr, &invalid) {
			// Cannot happen in practice (already validated above), but
			// stop retrying immediately rather than repeat a deterministic
			// failure len(flushBackoff) times.
			break
		}
	}

	class, _ := classifySQLState(lastErr)
	if class == "" {
		class = sqlstateClassUnknown
	}
	recordRaiseFailure(dctx, a.Kind, "detached")
	slog.Default().Error("alert_raise_detached_exhausted", "kind", a.Kind, "sqlstate_class", class, "error", lastErr)
	raiseFailed(dctx, r.Pool(), a.Kind, class)
	return lastErr
}

// RaisePostCommit is a documentation-only alias for RaiseDetached, for
// call sites where the raise mode is specifically "post-commit, detached
// only" (§7.3a REPEATABLE READ sites; the kill-switch engage, §7.6) -
// behaviourally identical to RaiseDetached.
func RaisePostCommit(ctx context.Context, r ScopedRunner, a Alert) error {
	return RaiseDetached(ctx, r, a)
}
