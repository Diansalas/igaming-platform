//go:build integration

package alerting

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// TestRLS_TenantRaisesSubjectAlert_AdminSeesAndAcks_OtherTenantSeesNothing
// covers AL-1/AL-2/TI: tenant A raises a platform-owned Kind with subject
// = itself (subject_tenant_raise); tenant A can read it read-only
// (subject_tenant_read) but never ack/resolve it; a platform admin can
// read/ack/resolve it; tenant B sees nothing.
func TestRLS_TenantRaisesSubjectAlert_AdminSeesAndAcks_OtherTenantSeesNothing(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	tenantB := seedTenant(t, pool, admin)

	runner := NewTenantRunner(pool, tenantA)
	pending, err := InTx(context.Background(), runner, func(ctx context.Context, tx pgx.Tx) error {
		return RaiseGuarded(ctx, tx, Alert{
			Kind:            KindPaymentKillSwitchEngaged,
			SubjectTenantID: tenantA,
			Discriminator:   "switch:" + uuid.NewString(),
			Attributes:      map[string]AttrValue{"reason_code": "test"},
		})
	})
	if err != nil {
		t.Fatalf("InTx: %v", err)
	}
	pending.Flush(context.Background())

	var alertID uuid.UUID
	// Tenant A can read it (subject read).
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM alerts WHERE subject_tenant_id = $1`, tenantA).Scan(&alertID)
	}); err != nil {
		t.Fatalf("tenant A read: %v", err)
	}

	// Tenant A cannot ack/resolve it (no UPDATE path at all - RLS filters
	// to zero rows, not an error).
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE alerts SET state = 'acked' WHERE id = $1`, alertID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatal("tenant A must not be able to update the alert's state")
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant A update attempt: %v", err)
	}

	// Tenant B sees nothing.
	if err := pool.WithTenant(context.Background(), tenantB, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE id = $1`, alertID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatal("tenant B must not see tenant A's subject alert")
		}
		return nil
	}); err != nil {
		t.Fatalf("tenant B read: %v", err)
	}

	// Platform admin can ack then resolve it.
	if err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `UPDATE alerts SET state = 'acked' WHERE id = $1`, alertID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE alerts SET state = 'resolved', resolve_reason_code = 'fixed' WHERE id = $1`, alertID)
		return err
	}); err != nil {
		t.Fatalf("platform admin ack/resolve: %v", err)
	}
}

// TestRLS_CrossTenantSubjectRaiseRefused is TI + C-102-1: a tenant session
// may not raise a platform-owned Kind whose subject is a DIFFERENT
// tenant, refused both in Go (checkSubjectMatchesScope, via RaiseDetached)
// and independently by the database (42501).
func TestRLS_CrossTenantSubjectRaiseRefused(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	tenantB := seedTenant(t, pool, admin)

	// Go-side refusal via RaiseDetached.
	runnerA := NewTenantRunner(pool, tenantA)
	err := RaiseDetached(context.Background(), runnerA, Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: tenantB,
		Discriminator:   "switch:" + uuid.NewString(),
	})
	if err == nil {
		t.Fatal("expected the Go-side subject/scope mismatch refusal, got nil")
	}

	// DB-side refusal (RLS 42501) for a raw insert bypassing Go validation.
	err = pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb)`,
			tenantB, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString())
		return err
	})
	if err == nil {
		t.Fatal("expected the database RLS policy to refuse a cross-tenant subject raise")
	}
}

// TestRLS_MixedGUCSessionSeesNothing is C-102-9: a session carrying BOTH
// app.tenant_id and app.platform_service_id (a shape no production code
// creates today, but the exclusion set must hold regardless) satisfies no
// family and sees nothing.
func TestRLS_MixedGUCSessionSeesNothing(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_service_id', 'alert_dispatcher', true)`); err != nil {
			return err
		}
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts`).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("expected a mixed tenant+platform-service session to see zero alerts, saw %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("mixed-GUC probe: %v", err)
	}
}

// TestRLS_DispatcherCannotUpdateOrInsertNonMetaKind is AL-10/SR-2: the
// dispatcher identity has no UPDATE anywhere, and its INSERT is refused
// for a non-meta Kind on BOTH alerts and alert_occurrences.
func TestRLS_DispatcherCannotUpdateOrInsertNonMetaKind(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	var alertID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
			tenantA, string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString()).Scan(&alertID)
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}

	err := pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE alerts SET state = 'resolved', resolve_reason_code = 'x' WHERE id = $1`, alertID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatal("dispatcher must never be able to UPDATE alerts")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("dispatcher update probe: %v", err)
	}

	err = pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, NULL, $1, $2, '{}'::jsonb)`,
			string(KindPaymentKillSwitchEngaged), "switch:"+uuid.NewString())
		return err
	})
	if err == nil {
		t.Fatal("expected the dispatcher's non-meta INSERT on alerts to be refused")
	}

	err = pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, alertID)
		return err
	})
	if err == nil {
		t.Fatal("expected the dispatcher's non-meta INSERT on alert_occurrences to be refused")
	}
}

// TestRaiseFailed_MalformedInsertsRefused is the security Part 1
// hygiene test: a dispatcher-identity insert of alerting.raise_failed
// with an extra key, a bad kind, or a malformed sqlstate_class is refused
// by the migration 0110 attribute trigger.
func TestRaiseFailed_MalformedInsertsRefused(t *testing.T) {
	pool := testPool(t)

	cases := []struct {
		name  string
		attrs string
	}{
		{"extra key", `{"kind":"payment.kill_switch_engaged","sqlstate_class":"P0","extra":"y"}`},
		{"missing sqlstate_class", `{"kind":"payment.kill_switch_engaged"}`},
		{"nonexistent kind", `{"kind":"no.such.kind","sqlstate_class":"P0"}`},
		{"lowercase sqlstate_class", `{"kind":"payment.kill_switch_engaged","sqlstate_class":"p0"}`},
		{"free-text sqlstate_class", `{"kind":"payment.kill_switch_engaged","sqlstate_class":"oops"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, NULL, $1, 'ignored', $2::jsonb)`,
					string(KindAlertingRaiseFailed), c.attrs)
				return err
			})
			if err == nil {
				t.Fatalf("expected the malformed raise_failed insert to be refused")
			}
		})
	}
}

// TestDedup_DifferentSubjectTenantsGetDifferentAlerts is AL-3: the same
// Kind and discriminator raised for two DIFFERENT subject tenants must
// never collide into one alert - dedup_key embeds the subject tenant.
func TestDedup_DifferentSubjectTenantsGetDifferentAlerts(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)
	tenantB := seedTenant(t, pool, admin)
	discriminator := "switch:" + uuid.NewString()

	for _, tenant := range []uuid.UUID{tenantA, tenantB} {
		err := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb)`,
				tenant, string(KindPaymentKillSwitchEngaged), discriminator)
			return err
		})
		if err != nil {
			t.Fatalf("insert for tenant %s: %v", tenant, err)
		}
	}

	var count int
	err := pool.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE discriminator = $1`, discriminator).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 distinct alerts (one per subject tenant), got %d", count)
	}
}

// TestRLS_SimulationNeverDelivered is AL-9.
func TestRLS_SimulationNeverDelivered(t *testing.T) {
	pool := testPool(t)
	admin := seedPlatformAdmin(t, pool)
	tenantA := seedTenant(t, pool, admin)

	var alertID uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantA, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes) VALUES (NULL, $1, $2, $3, '{}'::jsonb) RETURNING id`,
			tenantA, string(KindSimulationCasinoPlay), "session:s1:reason:x").Scan(&alertID)
	}); err != nil {
		t.Fatalf("seed simulation alert: %v", err)
	}

	err := pool.WithPlatformService(context.Background(), db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_deliveries (alert_id, escalation_step, attempt_no, event) VALUES ($1, 0, 0, 'sent')`, alertID)
		return err
	})
	if err == nil {
		t.Fatal("expected a non-suppressed_simulation delivery row on a simulation alert to be refused")
	}
}
