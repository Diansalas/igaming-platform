package alertingtest_test

import (
	"context"
	"log/slog"
	"sync"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/alerting/alertingtest"
)

func TestConformance_RecordingChannel(t *testing.T) {
	alertingtest.RunChannelConformance(t, func(t *testing.T, _ *slog.Logger) alertingtest.Subject {
		c := alertingtest.NewRecordingChannel()
		return alertingtest.Subject{
			Sink: c, Notifications: c.Notifications, Hang: c.BlockUntilCtxDone, Fail: c.FailWith,
		}
	})
}

func TestConformance_LogSink(t *testing.T) {
	alertingtest.RunChannelConformance(t, func(t *testing.T, l *slog.Logger) alertingtest.Subject {
		return alertingtest.Subject{Sink: alerting.LogSink{Logger: l}}
	})
}

func TestRecordingChannel_IsNeverAHumanNotification(t *testing.T) {
	c := alertingtest.NewRecordingChannel()
	if c.HumanNotification() {
		t.Fatal("RecordingChannel must not claim to notify a person")
	}
	if c.ChannelKind() != alerting.ChannelMock {
		t.Fatalf("kind = %q", c.ChannelKind())
	}
}

func TestRecordingChannel_ScriptedOutcomesThenDefault(t *testing.T) {
	c := alertingtest.NewRecordingChannel()
	c.Script(alertingtest.Failed(alerting.ErrorClassTimeout), alertingtest.Failed(alerting.ErrorClassRejected))
	d := alerting.Delivery{DedupKey: "a:0"}
	if o, k := c.Deliver(context.Background(), d); o != alerting.OutcomeFailed || k != alerting.ErrorClassTimeout {
		t.Fatalf("first: %q/%q", o, k)
	}
	if o, k := c.Deliver(context.Background(), d); o != alerting.OutcomeFailed || k != alerting.ErrorClassRejected {
		t.Fatalf("second: %q/%q", o, k)
	}
	if o, _ := c.Deliver(context.Background(), d); o != alerting.OutcomeSent {
		t.Fatalf("after the script the default is sent, got %q", o)
	}
	if len(c.Calls()) != 3 || c.Notifications() != 1 {
		t.Fatalf("calls=%d notifications=%d", len(c.Calls()), c.Notifications())
	}
}

func TestRecordingChannel_DedupeCountsDuplicates(t *testing.T) {
	c := alertingtest.NewRecordingChannel()
	for i := 0; i < 4; i++ {
		c.Deliver(context.Background(), alerting.Delivery{DedupKey: "same:0"})
	}
	c.Deliver(context.Background(), alerting.Delivery{DedupKey: "other:0"})
	if c.Notifications() != 2 || c.DuplicateSuppressed() != 3 {
		t.Fatalf("notifications=%d duplicates=%d, want 2 and 3", c.Notifications(), c.DuplicateSuppressed())
	}
}

func TestRecordingChannel_PanicOnceThenRecovers(t *testing.T) {
	c := alertingtest.NewRecordingChannel()
	c.PanicOnce("boom")
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Fatalf("recover = %v", r)
			}
		}()
		c.Deliver(context.Background(), alerting.Delivery{DedupKey: "p:0"})
	}()
	if o, _ := c.Deliver(context.Background(), alerting.Delivery{DedupKey: "p:0"}); o != alerting.OutcomeSent {
		t.Fatal("the panic must be one-shot")
	}
}

func TestRecordingChannel_MutexSafeUnderRace(t *testing.T) {
	c := alertingtest.NewRecordingChannel()
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Deliver(context.Background(), alerting.Delivery{DedupKey: "k:" + string(rune('a'+i%4))})
			_ = c.Calls()
			_ = c.Notifications()
		}(i)
	}
	wg.Wait()
	if c.Notifications() != 4 {
		t.Fatalf("4 distinct keys expected, got %d", c.Notifications())
	}
}
