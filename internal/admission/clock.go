// Package admission implements ADR 0097's transport-layer webhook
// admission and rate-limiting primitives: a keyed GCRA token bucket
// (gcra.go), a keyed counting-semaphore bulkhead (bulkhead.go) and a
// bounded log-suppression window (suppress.go).
//
// This package is a stdlib-only leaf (architect review, ADR 0097 §20
// AC1): it imports nothing under internal/ or any third-party module, and
// it must never be imported by a domain package (webhookauth, payments,
// casino, kyc, ledger, db, identity, config) - only internal/httpserver
// (and its own tests) may import it. import_guard_test.go pins both
// directions.
//
// Every time-driven primitive here takes the Clock interface instead of
// calling time.Now()/time.Sleep() directly, so tests can use FakeClock
// and stay in the deterministic main lane (ADR 0097 §5.1, §11 QA review
// item 7).
package admission

import "time"

// Clock is the time source every admission primitive uses instead of the
// time package directly (ADR 0097 §5.1). *RealClock is used in
// production; *FakeClock drives deterministic tests.
type Clock interface {
	Now() time.Time
	NewTimer(d time.Duration) Timer
}

// Timer is the minimal timer surface a Clock produces - satisfied by
// *time.Timer via realTimer, and by FakeClock's own virtual timer.
type Timer interface {
	// C returns the channel that fires when the timer expires.
	C() <-chan time.Time
	// Stop prevents the timer from firing, if it hasn't already. Safe to
	// call more than once.
	Stop() bool
}

// realClock is the production Clock: real wall time, real timers.
type realClock struct{}

// RealClock returns the production Clock backed by the time package.
func RealClock() Clock { return realClock{} }

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer {
	return &realTimer{t: time.NewTimer(d)}
}

type realTimer struct{ t *time.Timer }

func (r *realTimer) C() <-chan time.Time { return r.t.C }
func (r *realTimer) Stop() bool          { return r.t.Stop() }
