package alerting

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// meter uses the global OpenTelemetry meter provider (set up by
// observability.InitMetrics in cmd/platform-api/main.go), mirroring how
// internal/observability itself obtains a Meter. Every counter here is
// created lazily and safely reused - metric.Meter.Int64Counter is
// idempotent per (name) within one provider.
var meter = otel.Meter("github.com/Diansalas/igaming-platform/internal/alerting")

// alertRaiseFailuresTotal is ADR §7.2(5)/§6.3: alert_raise_failures_total
// {kind, phase}. phase is one of "in_tx", "detached", "fallback". There is
// deliberately NO tenant label anywhere in this package's metrics
// (§6.3: "the metric has no tenant label").
var alertRaiseFailuresTotal, _ = meter.Int64Counter(
	"alert_raise_failures_total",
	metric.WithDescription("Count of alert raises that failed at each phase (ADR 0102 §7.2/§6.3)."),
)

// alertUnroutedTotal is ADR §6.1: alert_unrouted_total{severity},
// incremented only when a new unrouted delivery row was actually inserted
// (RETURNING-gated, LF F9).
var alertUnroutedTotal, _ = meter.Int64Counter(
	"alert_unrouted_total",
	metric.WithDescription("Count of alerts newly marked unrouted per dispatcher pass (ADR 0102 §6.1)."),
)

// alertDeadTotal is ADR §6.1: alert_dead_total{channel_kind}.
var alertDeadTotal, _ = meter.Int64Counter(
	"alert_dead_total",
	metric.WithDescription("Count of alerts whose delivery attempts were exhausted (ADR 0102 §6.1)."),
)

func recordRaiseFailure(ctx context.Context, kind Kind, phase string) {
	if alertRaiseFailuresTotal == nil {
		return
	}
	alertRaiseFailuresTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", string(kind)),
		attribute.String("phase", phase),
	))
}

func recordUnrouted(ctx context.Context, severity Severity) {
	if alertUnroutedTotal == nil {
		return
	}
	alertUnroutedTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("severity", string(severity))))
}

func recordDead(ctx context.Context, channelKind string) {
	if alertDeadTotal == nil {
		return
	}
	alertDeadTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("channel_kind", channelKind)))
}

// alertStaleClaimsTotal (security IC-2 / code review F-1): a 'claimed'
// delivery row whose lease expired before any outcome was recorded - the
// signal that a crash, deploy, OOM or failed record-outcome INSERT
// stranded an in-flight delivery attempt, now reclaimed under a new
// attempt number. No tenant label, matching every other counter here.
var alertStaleClaimsTotal, _ = meter.Int64Counter(
	"alert_stale_claims_total",
	metric.WithDescription("Count of stale 'claimed' delivery rows reclaimed after their lease expired (ADR 0102 §6.1, security IC-2)."),
)

func recordStaleClaim(ctx context.Context) {
	if alertStaleClaimsTotal == nil {
		return
	}
	alertStaleClaimsTotal.Add(ctx, 1)
}

// alertDispatcherPassesTotal counts dispatcher loop passes by result
// ("ok", "error", "panic"). A flat counter is the "alert dispatcher
// stalled" signal (runbook). No tenant label.
var alertDispatcherPassesTotal, _ = meter.Int64Counter(
	"alert_dispatcher_passes_total",
	metric.WithDescription("Count of alert dispatcher loop passes by result (ADR 0102 I-wire)."),
)

func recordDispatcherPass(ctx context.Context, result string) {
	if alertDispatcherPassesTotal == nil {
		return
	}
	alertDispatcherPassesTotal.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}
