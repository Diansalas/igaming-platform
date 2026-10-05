// Package alertingtest is the TEST channel for the durable alert dispatcher
// (ALERT-DELIVERY-1, ADR 0102 section 18). It is importable from _test.go
// files ONLY: a static test (internal/alerting) fails the build if any
// non-test file imports it, and RecordingChannel carries the
// providerkind.Synthetic marker so MOCK-ADAPTER-PROD-1 would refuse it in
// production anyway. It is never wired into a binary (security S1 ruling).
//
// Nothing here reaches a person. Delivered-to-RecordingChannel is NEVER a
// human notification: HumanNotification() is false.
package alertingtest

import (
	"context"
	"sync"

	"github.com/Diansalas/igaming-platform/internal/alerting"
)

// Result is one scripted outcome.
type Result struct {
	Outcome alerting.Outcome
	Class   alerting.ErrorClass
}

// Sent and Failed build scripted results.
func Sent() Result { return Result{Outcome: alerting.OutcomeSent} }
func Failed(c alerting.ErrorClass) Result {
	return Result{Outcome: alerting.OutcomeFailed, Class: c}
}

// RecordingChannel is a mutex-safe recording channel with scripted outcomes,
// a hang-until-ctx mode, panic-on-demand and a duplicate counter. It dedupes on
// DedupKey like a conforming real adapter must: a second delivery of an
// already-sent DedupKey returns sent WITHOUT counting another notification.
type RecordingChannel struct {
	mu         sync.Mutex
	calls      []alerting.Delivery
	script     []Result
	defaultRes Result
	hang       bool
	panicVal   any
	panicArmed bool
	sentKeys   map[string]bool
	duplicates int
}

// NewRecordingChannel returns a channel whose default outcome is sent.
func NewRecordingChannel() *RecordingChannel {
	return &RecordingChannel{defaultRes: Sent(), sentKeys: map[string]bool{}}
}

// SyntheticComponent marks the channel as a test double for the
// MOCK-ADAPTER-PROD-1 guard.
func (*RecordingChannel) SyntheticComponent() {}

func (*RecordingChannel) ChannelKind() alerting.ChannelKind { return alerting.ChannelMock }

// HumanNotification is false: a recording channel reaches no person.
func (*RecordingChannel) HumanNotification() bool { return false }

// Script queues outcomes consumed one per Deliver call, then the default.
func (c *RecordingChannel) Script(results ...Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.script = append(c.script, results...)
}

// FailWith makes every unscripted delivery fail with the class.
func (c *RecordingChannel) FailWith(class alerting.ErrorClass) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.defaultRes = Failed(class)
}

// BlockUntilCtxDone makes Deliver hang until its context is done (a hung
// vendor); it then returns failed/timeout.
func (c *RecordingChannel) BlockUntilCtxDone() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hang = true
}

// PanicOnce makes the next Deliver panic with v.
func (c *RecordingChannel) PanicOnce(v any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.panicVal, c.panicArmed = v, true
}

// Deliver implements alerting.Sink.
func (c *RecordingChannel) Deliver(ctx context.Context, d alerting.Delivery) (alerting.Outcome, alerting.ErrorClass) {
	c.mu.Lock()
	c.calls = append(c.calls, d)
	if c.panicArmed {
		v := c.panicVal
		c.panicArmed = false
		c.mu.Unlock()
		panic(v)
	}
	if c.hang {
		c.mu.Unlock()
		<-ctx.Done()
		return alerting.OutcomeFailed, alerting.ErrorClassTimeout
	}
	res := c.defaultRes
	if len(c.script) > 0 {
		res, c.script = c.script[0], c.script[1:]
	}
	if res.Outcome == alerting.OutcomeSent {
		if c.sentKeys[d.DedupKey] {
			c.duplicates++
		} else {
			c.sentKeys[d.DedupKey] = true
		}
	}
	c.mu.Unlock()
	return res.Outcome, res.Class
}

// Calls returns a copy of every Delivery passed in, in order.
func (c *RecordingChannel) Calls() []alerting.Delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]alerting.Delivery(nil), c.calls...)
}

// Notifications is the number of DISTINCT DedupKeys successfully sent: what a
// human-visible channel would have shown exactly once each.
func (c *RecordingChannel) Notifications() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.sentKeys)
}

// DuplicateSuppressed counts repeat sends of an already-sent DedupKey that
// the channel suppressed (the dedupe-on-DedupKey contract).
func (c *RecordingChannel) DuplicateSuppressed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.duplicates
}
