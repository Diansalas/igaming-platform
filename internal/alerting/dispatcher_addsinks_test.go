package alerting

import (
	"context"
	"testing"
)

type stubSink struct {
	kind  ChannelKind
	human bool
}

func (s stubSink) ChannelKind() ChannelKind                                { return s.kind }
func (s stubSink) HumanNotification() bool                                 { return s.human }
func (s stubSink) Deliver(context.Context, Delivery) (Outcome, ErrorClass) { return OutcomeSent, "" }

func TestAddSinks_RegistersBeforeStartAndRefusesDuplicatesNilAndLateCalls(t *testing.T) {
	d := NewDispatcher(nil, DispatcherConfig{}, LogSink{})
	if err := d.AddSinks(stubSink{kind: ChannelWebhook, human: true}, stubSink{kind: ChannelEmail}); err != nil {
		t.Fatal(err)
	}
	if _, ok := d.sinks[ChannelWebhook]; !ok {
		t.Fatal("webhook sink not registered")
	}
	if err := d.AddSinks(stubSink{kind: ChannelWebhook}); err == nil {
		t.Error("a second sink for the same channel kind replaced the first")
	}
	if err := d.AddSinks(nil); err == nil {
		t.Error("a nil sink was accepted")
	}
	if err := d.AddSinks(stubSink{kind: ChannelLog}); err == nil {
		t.Error("the log sink was replaced")
	}
	// A failed AddSinks registers nothing from that call (all-or-nothing).
	d2 := NewDispatcher(nil, DispatcherConfig{}, LogSink{})
	if err := d2.AddSinks(stubSink{kind: ChannelEmail}, stubSink{kind: ChannelLog}); err == nil {
		t.Fatal("expected a duplicate error")
	}
	if _, ok := d2.sinks[ChannelEmail]; ok {
		t.Error("a partially failed AddSinks left the first sink registered")
	}
	// After the dispatcher started (first pass or first readiness evaluation) the map is closed.
	d.started.Store(true)
	if err := d.AddSinks(stubSink{kind: ChannelMock}); err == nil {
		t.Error("AddSinks was accepted after the dispatcher started")
	}
}
