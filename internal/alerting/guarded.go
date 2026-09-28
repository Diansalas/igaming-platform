package alerting

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// sqlstateClassGoValidation is the sentinel sqlstate_class the §6.3
// fallback uses for a Go-side validation failure (never a real SQLSTATE),
// matching migration 0110's alerting.raise_failed attribute trigger
// exactly ("go_validation").
const sqlstateClassGoValidation = "go_validation"

// sqlstateClassRRDeferred labels a Pending entry that was never even
// attempted in-tx because the transaction was REPEATABLE READ or higher
// (LF C-2/AL-11) - informational only (Flush decides purely from the
// Alert itself via RaiseDetached, never from this label), kept distinct
// from sqlstateClassGoValidation so a future log/metric reader is not
// misled into thinking the Kind's payload was invalid.
const sqlstateClassRRDeferred = "rr_deferred"

// sqlstateClassUnknown is used only when a detached retry's final
// attempt failed with something other than a *pgconn.PgError (e.g. a
// context deadline or a network error) - there is no real two-character
// SQLSTATE to report, so this stand-in satisfies migration 0110's
// `sqlstate_class ~ '^[0-9A-Z]{2}$'` CHECK ("58" is Postgres's own
// System Error class, the closest real meaning available) while staying
// honest that the last observed error was not itself a PG error.
const sqlstateClassUnknown = "58"

// classifySQLState implements the ADR §7.2(4) narrow swallow allowlist:
// classes 22 and 23, or exactly 42501 or P0001. Everything else -
// including 25P02, 40001, 40P01, 55P03, 57014, classes 08/53, context
// cancellation, and any non-PG error - is refused (swallow=false) and
// must propagate.
func classifySQLState(err error) (sqlstateClass string, swallow bool) {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return "", false
	}
	code := pgErr.Code
	if len(code) < 2 {
		return code, false
	}
	class := code[:2]
	switch {
	case class == "22", class == "23":
		return class, true
	case code == "42501":
		return class, true
	case code == "P0001":
		return class, true
	default:
		return class, false
	}
}

// pendingCtxKey is the context key InTx uses to hand the current
// transaction's Pending collector to RaiseGuarded - mirroring
// internal/txscope's own ctx-marker pattern, kept local to this package
// since only RaiseGuarded ever needs to find it.
type pendingCtxKey struct{}

func withPending(ctx context.Context, p *Pending) context.Context {
	return context.WithValue(ctx, pendingCtxKey{}, p)
}

func pendingFromContext(ctx context.Context) *Pending {
	p, _ := ctx.Value(pendingCtxKey{}).(*Pending)
	return p
}

// pendingEntry is one swallowed (or Go-validation-failed) alert awaiting
// its mandatory detached retry.
type pendingEntry struct {
	alert         Alert
	sqlstateClass string // real 2-char class, or sqlstateClassGoValidation
}

// Pending is the ADR §7.3/LF F4 per-transaction collector: exactly one
// Pending is bound to exactly one business transaction (via InTx), and it
// flushes only after that transaction's nil commit. A swallowed alert
// from a rolled-back transaction is discarded with the Pending itself -
// InTx never returns a Pending for a transaction that returned an error,
// so there is no code path that can flush it (AL-7).
type Pending struct {
	runner ScopedRunner
	clock  Clock

	mu      sync.Mutex
	entries []pendingEntry
}

// Option configures InTx/Pending. WithClock overrides the default
// SystemClock (T-1) - tests use it to inject a fake clock instead of
// sleeping.
type Option func(*Pending)

// WithClock overrides the Clock a Pending's Flush uses for its detached-
// retry backoff.
func WithClock(c Clock) Option {
	return func(p *Pending) { p.clock = c }
}

func (p *Pending) add(e pendingEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, e)
}

// InTx opens exactly one business transaction (via r.Run) with a fresh
// Pending collector bound to it through ctx, runs fn, and returns the
// Pending ONLY if fn returned nil AND the transaction committed. On any
// error, InTx returns (nil, err): there is no Pending to flush, so a
// swallowed alert from this transaction can never be re-raised later -
// this is what makes "a collector shared across transactions" and "Flush
// called on the error path" both structurally impossible, not merely
// disciplined (LF F4, the ADR §11 mutants).
func InTx(ctx context.Context, r ScopedRunner, fn func(ctx context.Context, tx pgx.Tx) error, opts ...Option) (*Pending, error) {
	p := &Pending{runner: r, clock: SystemClock{}}
	for _, opt := range opts {
		opt(p)
	}
	err := r.Run(ctx, func(innerCtx context.Context, tx pgx.Tx) error {
		return fn(withPending(innerCtx, p), tx)
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

// RaiseGuarded is the in-transaction raise (ADR §7.2): Go validation runs
// first and is NEVER propagated (LF F1/AL-6a) - a validation failure is
// recorded on the transaction's Pending with sqlstate_class
// "go_validation" instead. Otherwise a SAVEPOINT encloses ONLY this
// Raise; on a swallowable SQLSTATE (22/23/42501/P0001, addendum (a)) the
// savepoint is rolled back and the alert is registered on the
// transaction's Pending; every other class - including every transient
// class the ADR names - propagates untouched, exactly as if this call had
// never swallowed anything (Q3).
//
// The caller must be running inside a transaction opened by InTx (so ctx
// carries a Pending) - every sanctioned business call site does. If not,
// RaiseGuarded still never propagates a failure into the business
// transaction (the ADR's core guarantee), but the mandatory detached
// retry cannot be scheduled; this is logged loudly as a caller bug.
func RaiseGuarded(ctx context.Context, tx pgx.Tx, a Alert) error {
	v, err := a.validate()
	if err != nil {
		recordRaiseFailure(ctx, a.Kind, "in_tx")
		slog.Default().Error("alert_raise_invalid", "kind", a.Kind)
		registerPending(ctx, a, sqlstateClassGoValidation)
		return nil
	}
	attrsJSON, err := json.Marshal(v.attrs)
	if err != nil {
		// Unreachable in practice (validate() already round-trips this),
		// but handled as a validation failure for consistency rather than
		// propagating a marshal error into the business transaction.
		recordRaiseFailure(ctx, a.Kind, "in_tx")
		registerPending(ctx, a, sqlstateClassGoValidation)
		return nil
	}

	// LF C-2 / AL-11: RaiseGuarded must never execute an alert statement
	// inside a REPEATABLE READ (or SERIALIZABLE) transaction - an
	// ON CONFLICT DO NOTHING against a row committed after the snapshot
	// raises 40001 there (SR-5), which is exactly the class this
	// function must propagate, not swallow. Checked BEFORE the savepoint
	// is even opened: if the isolation level is not READ COMMITTED, skip
	// the insert entirely and register the alert on Pending so the
	// mandatory post-commit detached raise (§7.3a) picks it up in a
	// fresh READ COMMITTED transaction instead.
	isolation, isoErr := currentTransactionIsolation(ctx, tx)
	if isoErr != nil {
		// Cannot even determine the isolation level: propagate, exactly
		// like any other unclassified failure before the savepoint.
		return fmt.Errorf("alerting: read transaction_isolation: %w", isoErr)
	}
	if isolation != "read committed" {
		slog.Default().Warn("alert_raise_rr_deferred", "kind", a.Kind, "transaction_isolation", isolation)
		registerPending(ctx, a, sqlstateClassRRDeferred)
		return nil
	}

	savepoint, err := tx.Begin(ctx)
	if err != nil {
		// Cannot even open the savepoint: propagate (§7.2 step 2).
		return fmt.Errorf("alerting: open savepoint: %w", err)
	}

	raiseErr := insertOrAttachOccurrence(ctx, savepoint, a, attrsJSON)
	if raiseErr == nil {
		if commitErr := savepoint.Commit(ctx); commitErr != nil {
			return fmt.Errorf("alerting: commit savepoint: %w", commitErr)
		}
		return nil
	}

	sqlstateClass, swallow := classifySQLState(raiseErr)
	if rollbackErr := savepoint.Rollback(ctx); rollbackErr != nil {
		// "If that rollback fails, propagate" (§7.2 step 3).
		return fmt.Errorf("alerting: rollback savepoint after %v: %w", raiseErr, rollbackErr)
	}
	if !swallow {
		return raiseErr
	}

	recordRaiseFailure(ctx, a.Kind, "in_tx")
	slog.Default().Error("alert_raise_failed", "kind", a.Kind, "sqlstate_class", sqlstateClass)
	registerPending(ctx, a, sqlstateClass)
	return nil
}

// currentTransactionIsolation reads the CURRENT transaction's isolation
// level (LF C-2) - `SHOW transaction_isolation` reflects the isolation
// level pgx.TxOptions actually negotiated for this specific transaction,
// never a session default, so this is accurate even if a future caller
// changes the pool's default isolation.
func currentTransactionIsolation(ctx context.Context, tx pgx.Tx) (string, error) {
	var isolation string
	if err := tx.QueryRow(ctx, `SHOW transaction_isolation`).Scan(&isolation); err != nil {
		return "", err
	}
	return isolation, nil
}

func registerPending(ctx context.Context, a Alert, sqlstateClass string) {
	p := pendingFromContext(ctx)
	if p == nil {
		slog.Default().Error("alert_raise_no_pending", "kind", a.Kind, "sqlstate_class", sqlstateClass,
			"detail", "RaiseGuarded called outside alerting.InTx; the mandatory detached retry cannot be scheduled")
		return
	}
	p.add(pendingEntry{alert: a, sqlstateClass: sqlstateClass})
}

// flushBackoff is the technical default backoff schedule for Flush's at-
// most-3 detached-retry attempts (a devops-reviewable constant, not a
// policy value - ADR §14 item 8).
var flushBackoff = []time.Duration{0, 200 * time.Millisecond, 800 * time.Millisecond}

// Flush runs the mandatory detached retry for every alert this
// transaction's Raise calls swallowed (§7.3/§7.5: mandatory for every
// Kind). It must be called ONLY after the response has been written (HTTP
// call sites) or right after the commit (non-HTTP callers) - never on the
// error path (there is nothing to flush there: InTx already returned nil
// for a failed transaction). Each entry gets up to len(flushBackoff)
// attempts in the entry's ORIGINATING scope; if every attempt fails (or
// the failure was itself a Go validation failure, which would only fail
// again identically), the §6.3 terminal fallback persists
// alerting.raise_failed. Flush is safe to call on a nil Pending (a no-op)
// so callers can write `pending.Flush(ctx)` unconditionally after
// checking InTx's error.
func (p *Pending) Flush(ctx context.Context) {
	if p == nil {
		return
	}
	p.mu.Lock()
	entries := p.entries
	p.entries = nil
	p.mu.Unlock()

	if len(entries) == 0 {
		return
	}

	// RaiseDetached itself now detaches its own context (security IC-5/
	// code review F-2/LF C-3) - Flush only needs to make its Clock
	// available on ctx; the caller's own ctx being cancelled (e.g. an
	// HTTP request finishing right after this call, which is exactly
	// when Flush is meant to run) can no longer skip either the retry or
	// the terminal fallback.
	cctx := WithClockContext(ctx, p.clock)

	// LF C-2: always retry through a FRESH READ COMMITTED runner over
	// the same scope, never p.runner directly - see
	// freshReadCommittedRunner's own doc comment.
	retryRunner := freshReadCommittedRunner(p.runner.Pool(), p.runner.Scope())

	for _, e := range entries {
		// RaiseDetached itself performs the mandatory bounded retry and,
		// on exhaustion, the §6.3 terminal fallback (including the
		// go_validation case) - Flush has nothing further to do per
		// entry. The original in-tx sqlstate_class (e.sqlstateClass) was
		// already logged/counted by RaiseGuarded; what matters here is
		// only that every swallowed alert gets its mandatory retry.
		_ = RaiseDetached(cctx, retryRunner, e.alert)
	}
}
