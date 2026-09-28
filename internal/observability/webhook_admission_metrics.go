package observability

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// This file is ADR 0097 §8's OTel metrics (PRH-I4-METRICS-1): a counter of
// webhook admission decisions and an in-flight gauge for the A4a bulkhead
// (ADR 0097 §3/§5.1). It mirrors internal/alerting/metrics.go's pattern
// exactly: a package-level meter obtained from the GLOBAL OTel meter
// provider (set up once by observability.InitMetrics in
// cmd/platform-api/main.go), counters/gauges created lazily at package
// init with their creation error discarded (a nil instrument is a
// documented, safe no-op below - see the nil checks in every Record*/
// helper), and every Add call is fire-and-forget: nothing here ever
// returns an error to, or otherwise influences, the caller's control
// flow. That is precisely what admission's own correctness needs: a
// no-op meter (InitMetrics with exporter "none", or this package's
// otel.Meter resolving to the SDK's default no-op provider before
// InitMetrics ever runs) and a failing exporter (an exporter whose
// Export/Collect always errors, discovered only by whatever reads the
// exporter's own output asynchronously) must leave every admission
// decision byte-for-byte identical to a real, healthy meter - see
// internal/httpserver's admission metrics tests for exactly that
// property asserted against real HTTP responses.
//
// HD-PRH-1 (per-tenant webhook path confidentiality) is open, so per ADR
// 0097 §8 there is deliberately NO tenant label anywhere in this file,
// and no raw provider id either (a provider id can itself identify a
// tenant when a tenant self-hosts a bespoke provider integration) - every
// label below is a closed, bounded enum, never a client- or
// database-derived free-form value.
var webhookMeter = otel.Meter("github.com/Diansalas/igaming-platform/internal/httpserver/webhook_admission")

// WebhookAdmissionDecision is ADR 0097 §8's `decision` label: a closed,
// two-value enum. There is no third value - every admission call site
// resolves to exactly one of these before a metric is ever recorded.
type WebhookAdmissionDecision string

const (
	WebhookAdmissionAdmitted WebhookAdmissionDecision = "admitted"
	WebhookAdmissionRejected WebhookAdmissionDecision = "rejected"
)

// WebhookAdmissionReason is ADR 0097 §8's `reason` label: a closed enum
// mirroring the admission `tier` values already used by the ADR 0097 §8
// allow-listed `webhook_admission_rejected` log line (see
// internal/httpserver/webhook_admission.go's logRejected), plus
// "admitted" for a successful decision and "panic" for the admission
// layer's own inner-recover fail-closed path (§6.1/§7/T9).
type WebhookAdmissionReason string

const (
	WebhookAdmissionReasonAdmitted        WebhookAdmissionReason = "admitted"
	WebhookAdmissionReasonIP              WebhookAdmissionReason = "ip"
	WebhookAdmissionReasonPreAuth         WebhookAdmissionReason = "preauth"
	WebhookAdmissionReasonInFlight        WebhookAdmissionReason = "inflight"
	WebhookAdmissionReasonDBGate          WebhookAdmissionReason = "db_gate"
	WebhookAdmissionReasonVerified        WebhookAdmissionReason = "verified"
	WebhookAdmissionReasonDomainBulkhead  WebhookAdmissionReason = "domain_bulkhead"
	WebhookAdmissionReasonDirectoryUnload WebhookAdmissionReason = "directory_unloaded"
	WebhookAdmissionReasonPanic           WebhookAdmissionReason = "panic"
)

// WebhookProviderKind is ADR 0097 §8's `provider_kind` label. It is the
// webhook DOMAIN (payments/casino/kyc), never a raw provider id or
// tenant-identifying value - deliberately a closed, 3(+1)-value enum
// bounded independently of tenant/provider cardinality.
type WebhookProviderKind string

const (
	WebhookProviderKindPayments WebhookProviderKind = "payments"
	WebhookProviderKindCasino   WebhookProviderKind = "casino"
	WebhookProviderKindKYC      WebhookProviderKind = "kyc"
	// WebhookProviderKindUnknown is used only defensively, if a caller
	// somehow supplies a webhook domain outside the three registered
	// today - it keeps the label set closed rather than passing through
	// an arbitrary string.
	WebhookProviderKindUnknown WebhookProviderKind = "_unknown_domain"
)

// webhookAdmissionDecisionsTotal is ADR 0097 §8:
// webhook_admission_decisions_total{decision,reason,provider_kind}. The
// creation error is intentionally discarded (matches
// internal/alerting/metrics.go): a nil counter is this package's
// documented no-op state, checked by RecordWebhookAdmissionDecision
// before every use.
var webhookAdmissionDecisionsTotal, _ = webhookMeter.Int64Counter(
	"webhook_admission_decisions_total",
	metric.WithDescription("Count of webhook admission decisions by decision/reason/provider_kind (ADR 0097 §8)."),
)

// webhookAdmissionInFlight is ADR 0097 §8's in-flight gauge for the A4a
// bulkhead, modeled as an UpDownCounter (OTel's synchronous gauge
// primitive): +1 when a request acquires an A4a slot, -1 when it is
// released - so at rest (no requests admitted) it reads exactly 0 per
// provider_kind, and never goes negative in normal operation.
var webhookAdmissionInFlight, _ = webhookMeter.Int64UpDownCounter(
	"webhook_admission_inflight",
	metric.WithDescription("Current count of webhook requests holding an A4a in-flight admission slot, by provider_kind (ADR 0097 §8)."),
)

// safelyRecord runs fn and unconditionally swallows any panic from it,
// never letting one escape to the caller. This is deliberate, not merely
// defensive: the DoD for PRH-I4-METRICS-1 is "metrics must never affect
// admission" - not just "an unhealthy exporter never affects admission"
// but genuinely NEVER, even in the case of a hypothetical bug in the
// metrics SDK/an exporter's synchronous aggregation path panicking.
// Without this, such a panic would unwind into the CALLER
// (internal/httpserver's admitPreAuth/admitVerified), landing in THEIR
// OWN admission-layer recover() (ADR 0097 §6.1/§7/T9) and silently
// converting what was already a successful admission decision into a
// rejection - exactly the "let a metric failure change the admission
// outcome" failure mode this package must rule out by construction, not
// by convention. Extracted as its own function (rather than an inline
// `defer func(){ recover() }()` in each caller) so this guarantee itself
// is directly unit-testable independent of the real OTel SDK's own
// behaviour (see webhook_admission_metrics_test.go's
// TestSafelyRecord_SwallowsPanic).
func safelyRecord(fn func()) {
	defer func() { _ = recover() }()
	fn()
}

// RecordWebhookAdmissionDecision records one ADR 0097 §8 admission
// decision. It is always called AFTER the admission decision itself has
// already been made and (for a rejection) the HTTP response already
// written - this function's own body can never affect that decision, and
// a nil counter (no-op meter, or a meter whose Int64Counter call itself
// failed) makes this a true no-op.
func RecordWebhookAdmissionDecision(ctx context.Context, decision WebhookAdmissionDecision, reason WebhookAdmissionReason, kind WebhookProviderKind) {
	if webhookAdmissionDecisionsTotal == nil {
		return
	}
	safelyRecord(func() {
		webhookAdmissionDecisionsTotal.Add(ctx, 1, metric.WithAttributes(
			attribute.String("decision", string(decision)),
			attribute.String("reason", string(reason)),
			attribute.String("provider_kind", string(kind)),
		))
	})
}

// WebhookAdmissionInFlightInc/Dec record ADR 0097 §8's in-flight gauge.
// Callers MUST pair every Inc with exactly one later Dec for the same
// admitted request, on every exit path (including panic recovery) - see
// internal/httpserver/webhook_admission.go's admitPreAuth, where the Inc
// happens at the same point the A4a slot is acquired and the Dec is
// folded into the very release func the caller already must invoke
// exactly once to release that slot.
func WebhookAdmissionInFlightInc(ctx context.Context, kind WebhookProviderKind) {
	if webhookAdmissionInFlight == nil {
		return
	}
	safelyRecord(func() {
		webhookAdmissionInFlight.Add(ctx, 1, metric.WithAttributes(attribute.String("provider_kind", string(kind))))
	})
}

func WebhookAdmissionInFlightDec(ctx context.Context, kind WebhookProviderKind) {
	if webhookAdmissionInFlight == nil {
		return
	}
	safelyRecord(func() {
		webhookAdmissionInFlight.Add(ctx, -1, metric.WithAttributes(attribute.String("provider_kind", string(kind))))
	})
}
