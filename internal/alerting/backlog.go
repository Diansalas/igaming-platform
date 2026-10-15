package alerting

import (
	"context"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Undelivered-alert backlog (A9, pending-signals.yml alert dispatcher backlog/age). An alert is UNDELIVERED
// when it is open (not acknowledged), not a simulation, and nobody has been notified of it: it has no delivery
// row yet, or its latest row is unrouted / claimed / failed / dead, or it was "sent" only through a channel
// that reaches no person (log, mock, an email channel over the MOCK provider). Acknowledged and resolved
// alerts are excluded: a human already has them.
//
// The tally is computed from the rows the dispatcher already reads each pass (readDueWork), so it costs no
// extra query; it is therefore as fresh as the last completed pass, and alert_dispatcher_passes_total is the
// signal that the passes are still happening. Labels: severity only (p1|p2|p3), never an alert id or tenant.

var severityIndex = map[Severity]int{SeverityP1: 0, SeverityP2: 1, SeverityP3: 2}

// backlogTally accumulates one pass.
type backlogTally struct {
	count  [3]int64
	oldest [3]time.Duration
}

func (b *backlogTally) add(sev Severity, age time.Duration) {
	i, ok := severityIndex[sev]
	if !ok {
		return
	}
	b.count[i]++
	if age > b.oldest[i] {
		b.oldest[i] = age
	}
}

var (
	backlogPublished   atomic.Bool
	backlogCount       [3]atomic.Int64
	backlogOldestAgeSc [3]atomic.Int64
)

// publishBacklog makes a completed pass's tally visible to the gauges.
func publishBacklog(b backlogTally) {
	for i := range b.count {
		backlogCount[i].Store(b.count[i])
		backlogOldestAgeSc[i].Store(int64(b.oldest[i] / time.Second))
	}
	backlogPublished.Store(true)
}

var severities = [3]Severity{SeverityP1, SeverityP2, SeverityP3}

func init() { registerBacklogGauges() }

// registerBacklogGauges is split from init so a test can register the same gauges on a private meter.
func registerBacklogGauges() {
	_, _ = meter.Int64ObservableGauge("alert_undelivered_count",
		metric.WithDescription("Open, unacknowledged, non-simulation alerts nobody has been notified of, by severity (as of the last completed dispatcher pass)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if !backlogPublished.Load() {
				return nil
			}
			for i, sev := range severities {
				o.Observe(backlogCount[i].Load(), metric.WithAttributes(attribute.String("severity", string(sev))))
			}
			return nil
		}))
	_, _ = meter.Int64ObservableGauge("alert_undelivered_oldest_age_seconds",
		metric.WithDescription("Age in seconds of the oldest undelivered alert, by severity; 0 when none (as of the last completed dispatcher pass)."),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			if !backlogPublished.Load() {
				return nil
			}
			for i, sev := range severities {
				o.Observe(backlogOldestAgeSc[i].Load(), metric.WithAttributes(attribute.String("severity", string(sev))))
			}
			return nil
		}))
}

// undelivered classifies an alert's latest delivery event (nil: none yet) and that row's channel kind.
func (d *Dispatcher) undelivered(event, channelKind *string) bool {
	if event == nil {
		return true
	}
	switch *event {
	case "unrouted", "claimed", "failed", "dead":
		return true
	case "sent":
		if channelKind == nil {
			return true
		}
		return !d.channelIsHuman(ChannelKind(*channelKind))
	default: // suppressed_simulation or an unknown future event: not counted
		return false
	}
}
