package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// Security addendum 1 (c): a swallowed or failed raise increments
// alert_raise_failures_total{kind,phase} with BOUNDED labels and NO tenant label
// (ADR 0102 6.3/7.2). The instrument is created from the package's global
// meter, which delegates to the provider installed here.
func TestMetric_AlertRaiseFailuresTotal_BoundedLabelsNoTenant(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))

	ctx := context.Background()
	// Go-validation failure path of RaiseGuarded: no SQL, so no tx is needed. It
	// must never propagate, and must count one in_tx failure for the Kind.
	if err := RaiseGuarded(ctx, nil, Alert{Kind: KindPaymentWebhookIntegrity, SubjectTenantID: uuid.New(), Discriminator: "bad discriminator!"}); err != nil {
		t.Fatalf("RaiseGuarded must never propagate a validation failure: %v", err)
	}
	// An unregistered (caller-supplied) Kind must be reported as "unknown",
	// never as its own label value.
	_ = RaiseGuarded(ctx, nil, Alert{Kind: Kind("x." + uuid.NewString())})

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(ctx, &rm); err != nil {
		t.Fatal(err)
	}
	var found, foundUnknown bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "alert_raise_failures_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("unexpected data type %T", m.Data)
			}
			for _, dp := range sum.DataPoints {
				set := dp.Attributes
				if set.Len() != 2 {
					t.Fatalf("labels must be exactly {kind, phase}, got %v", set.ToSlice())
				}
				if _, has := set.Value("tenant_id"); has {
					t.Fatal("the metric must carry no tenant label")
				}
				kind, _ := set.Value("kind")
				phase, _ := set.Value("phase")
				if kind.AsString() == string(KindPaymentWebhookIntegrity) && phase.AsString() == "in_tx" && dp.Value >= 1 {
					found = true
				}
				if kind.AsString() == "unknown" {
					foundUnknown = true
				}
				if _, registered := Def(Kind(kind.AsString())); !registered && kind.AsString() != "unknown" {
					t.Fatalf("unbounded kind label %q", kind.AsString())
				}
			}
		}
	}
	if !found {
		t.Fatal("alert_raise_failures_total{kind=payment.webhook_integrity,phase=in_tx} did not increment")
	}
	if !foundUnknown {
		t.Fatal("an unregistered kind must be counted under kind=unknown")
	}
}
