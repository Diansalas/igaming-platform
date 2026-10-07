//go:build integration

package reconciliation

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
)

// R3-TEST-GAPS-1 (LF N-5; mutant LF3 survived): the observation flag marks ONLY
// a non-active tenant's run. An ACTIVE tenant's ledger_vs_projection audit row
// must carry neither non_active_tenant_observation nor tenant_status, or staff
// would read an ordinary hourly run as "tenant was not active". The suspended
// tenant is the control: its row carries both, so the absence check is not vacuous.
func TestR3_ActiveTenantLedgerAuditCarriesNoObservationFlag(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	active := seedFixture(t, pool)
	suspended := seedFixture(t, pool)
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return execOne(ctx, tx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, suspended.tenantID)
	}); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	outcomes, err := RunSweepTenants(ctx, pool, nil, []uuid.UUID{active.tenantID, suspended.tenantID},
		time.Now().Add(-time.Hour), time.Now(), sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweepTenants: %v", err)
	}
	if o := findOutcome(t, outcomes, active); o.ObservationOnly || o.Err != nil {
		t.Fatalf("active outcome: %+v", o)
	}

	meta := func(f fixture) map[string]any {
		t.Helper()
		var rows []map[string]any
		if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			r, err := tx.Query(ctx, `SELECT metadata FROM audit_log WHERE tenant_id=$1 AND action='reconciliation.sweep_run' AND metadata->>'stream'='ledger_vs_projection'`, f.tenantID)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				var raw []byte
				if err := r.Scan(&raw); err != nil {
					return err
				}
				m := map[string]any{}
				if err := json.Unmarshal(raw, &m); err != nil {
					return err
				}
				rows = append(rows, m)
			}
			return r.Err()
		}); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		if len(rows) != 1 {
			t.Fatalf("expected exactly one ledger_vs_projection sweep_run audit row for %s, got %d", f.tenantID, len(rows))
		}
		return rows[0]
	}

	am := meta(active)
	if _, has := am["non_active_tenant_observation"]; has {
		t.Fatalf("active tenant audit must not carry non_active_tenant_observation: %v", am)
	}
	if _, has := am["tenant_status"]; has {
		t.Fatalf("active tenant audit must not carry tenant_status: %v", am)
	}
	// Control: the non-active tenant's row is flagged.
	sm := meta(suspended)
	if sm["non_active_tenant_observation"] != true || sm["tenant_status"] != "suspended" {
		t.Fatalf("suspended tenant audit must carry the observation flag and status: %v", sm)
	}
}
