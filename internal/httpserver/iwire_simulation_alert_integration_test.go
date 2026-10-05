//go:build integration

package httpserver

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// Row 11: the simulation alert is Kind-tagged simulation (p3), and is NEVER
// delivered or paged as a production P1 - even with a p3 route configured, the
// dispatcher records suppressed_simulation and the sink sees nothing.
func TestIWire_SimulationPayloadMismatchAlert_IsP3SimulationNeverDelivered(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	deps := Deps{DB: a.pool}
	raiseSimulationPayloadMismatchAlert(context.Background(), deps, tenant, "mock", "req-sim-1")

	rows := alertinject.Find(alertinject.ForSubject(t, a.pool, tenant), string(alerting.KindSimulationPaymentPayloadMismatch))
	if len(rows) != 1 || rows[0].Severity != "p3" || rows[0].Discriminator != "provider:mock" || rows[0].Attributes["request_id"] != "req-sim-1" {
		t.Fatalf("simulation alert: %+v", rows)
	}

	admin := a.platformAdmin()
	if err := a.pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		for _, sev := range []string{"p1", "p2", "p3"} {
			if _, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code) VALUES ('platform', $1, 0, 'mock', $2, true, 'initial_setup')`, sev, "mock:"+sev); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("routes: %v", err)
	}
	sink := &alerting.MockSink{}
	disp := alerting.NewDispatcher(a.pool, alerting.DispatcherConfig{}, sink)
	if err := disp.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, d := range sink.Attempts {
		if d.Kind == alerting.KindSimulationPaymentPayloadMismatch {
			t.Fatalf("a simulation alert must never be delivered: %+v", d)
		}
	}
	var event string
	if err := a.pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT d.event FROM alert_deliveries d JOIN alerts al ON al.id = d.alert_id
			WHERE al.kind = $1 AND al.subject_tenant_id = $2 ORDER BY d.recorded_at DESC LIMIT 1`,
			string(alerting.KindSimulationPaymentPayloadMismatch), tenant).Scan(&event)
	}); err != nil {
		t.Fatalf("read delivery: %v", err)
	}
	if event != "suppressed_simulation" {
		t.Fatalf("delivery event %q, want suppressed_simulation", event)
	}
	_ = uuid.Nil
}
