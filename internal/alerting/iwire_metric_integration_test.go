//go:build integration

package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// Security addendum 1 (c)/(e): a REAL swallowed in-tx raise (the alert INSERT
// fails with a swallowable class inside a business transaction that has
// already written) increments alert_raise_failures_total{kind,phase=in_tx},
// leaves the business transaction committed, and the post-commit detached raise
// persists the alert.
func TestIWire_SwallowedInTxRaise_IncrementsMetric_BusinessTxCommits_DetachedPersists(t *testing.T) {
	_ = testMetricReader()
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	alertinject.Install(t, pool, tenantA, alertinject.InTxOnly, "P0001")
	before := raiseFailureCount(t, string(KindPaymentKillSwitchEngaged), "in_tx")

	marker := uuid.New()
	disc := "switch:" + uuid.NewString()
	pending, err := InTx(context.Background(), NewTenantRunner(pool, tenantA), func(ctx context.Context, tx pgx.Tx) error {
		// A business write first (assigns the xid), then the guarded raise.
		if _, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor_type, actor_id, action, target_type, target_id, outcome)
			VALUES ($1, 'system', NULL, 'iwire.test_business_write', 'test', $2, 'success')`, tenantA, marker.String()); err != nil {
			return err
		}
		return RaiseGuarded(ctx, tx, Alert{Kind: KindPaymentKillSwitchEngaged, SubjectTenantID: tenantA, Discriminator: disc,
			Attributes: map[string]AttrValue{"reason_code": "iwire"}})
	})
	if err != nil {
		t.Fatalf("a swallowed raise must never fail the business transaction: %v", err)
	}
	if got := raiseFailureCount(t, string(KindPaymentKillSwitchEngaged), "in_tx"); got != before+1 {
		t.Fatalf("alert_raise_failures_total{in_tx} = %d, want %d", got, before+1)
	}
	// The business write committed.
	var n int
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'iwire.test_business_write' AND target_id = $1`, marker.String()).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("the business write must commit: n=%d err=%v", n, err)
	}
	// Not yet raised: the in-tx raise was rolled back to its savepoint.
	if rows := alertinject.ForSubject(t, pool, tenantA); len(rows) != 0 {
		t.Fatalf("no alert before the post-commit retry: %+v", rows)
	}
	pending.Flush(context.Background())
	rows := alertinject.ForSubject(t, pool, tenantA)
	if len(rows) != 1 || rows[0].Discriminator != disc {
		t.Fatalf("the detached retry must persist the alert: %+v", rows)
	}
}

// raiseFailureCount returns the cumulative value of alert_raise_failures_total for
// (kind, phase).
func raiseFailureCount(t *testing.T, kind, phase string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := testMetricReader().Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var n int64
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
				k, _ := dp.Attributes.Value("kind")
				p, _ := dp.Attributes.Value("phase")
				if k.AsString() == kind && p.AsString() == phase {
					n += dp.Value
				}
			}
		}
	}
	return n
}
