//go:build integration

package payments

// lic29 (security lic27 F-2, ledger lic27 N-1/N-2): the sweeper walks the persons in keyset batches, alerts the dead-worker signal and the
// officer queue APART, and alerts when its own read fails several passes in a row. The batch itself is tested in internal/aml.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
)

func alertsByDisc(t *testing.T, pool *db.Pool, tenant uuid.UUID, kind string) map[string]string {
	t.Helper()
	out := map[string]string{}
	if err := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT discriminator, attributes::text FROM alerts WHERE kind = $1 AND subject_tenant_id = $2`, kind, tenant)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d, a string
			if err := rows.Scan(&d, &a); err != nil {
				return err
			}
			out[d] = a
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestLic29_SweeperWalksTheKeysetCycleAndAlertsApart(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	s := newLoopSweeper(pool, newLoopProvider("mock-psp-lic29").orchestrator(), false, nil)
	c1, c2 := uuid.New(), uuid.New()
	var seen []*uuid.UUID
	s.testScreeningScan = func(_ uuid.UUID, after *uuid.UUID) (aml.ParkedScan, error) {
		seen = append(seen, after)
		switch len(seen) {
		case 1:
			return aml.ParkedScan{Parked: 2, Overdue: 2, Officer: 1, OfficerOverdue: 1, Scanned: 500, Next: &c1}, nil
		case 2:
			return aml.ParkedScan{Parked: 1, Scanned: 500, Next: &c2}, nil
		}
		return aml.ParkedScan{Scanned: 3}, nil
	}
	st := s.RunPass(context.Background(), nil, 0)
	if st.Errors != 0 || st.ScreeningParked != 2 || st.ScreeningParkedOverdue != 2 || st.ScreeningParkedOfficer != 1 ||
		st.ScreeningParkedOfficerOverdue != 1 || st.ScreeningParkedScanned != 500 || !st.ScreeningParkedPartial {
		t.Fatalf("pass 1: %+v", st)
	}
	st = s.RunPass(context.Background(), nil, 0)
	if !st.ScreeningParkedPartial {
		t.Fatalf("pass 2 continues the cycle: %+v", st)
	}
	st = s.RunPass(context.Background(), nil, 0)
	if st.ScreeningParkedPartial || st.ScreeningParkedScanned != 3 {
		t.Fatalf("pass 3 completes the cycle: %+v", st)
	}
	st = s.RunPass(context.Background(), nil, 0) // the next cycle starts again from the start
	if len(seen) != 4 || seen[0] != nil || seen[1] == nil || *seen[1] != c1 || seen[2] == nil || *seen[2] != c2 || seen[3] != nil {
		t.Fatalf("the cursor must be carried from batch to batch and reset after the end: %v", seen)
	}
	got := alertsByDisc(t, pool, f.tenantID, "aml.screening_gate_trapped")
	w, o := got["sweeper:tenant:"+f.tenantID.String()], got["sweeper_officer:tenant:"+f.tenantID.String()]
	if len(got) != 2 || !strings.Contains(w, "2") || !strings.Contains(o, "1") {
		t.Fatalf("the dead-worker signal and the officer queue alert apart, counts only: %v", got)
	}
}

func TestLic29_SweeperAlertsWhenItsOwnReadFailsConsecutively(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	s := newLoopSweeper(pool, newLoopProvider("mock-psp-lic29b").orchestrator(), false, nil)
	fail := true
	s.testScreeningScan = func(uuid.UUID, *uuid.UUID) (aml.ParkedScan, error) {
		if fail {
			return aml.ParkedScan{}, errors.New("boom")
		}
		return aml.ParkedScan{}, nil
	}
	count := func() int { return len(alertsByDisc(t, pool, f.tenantID, "aml.screening_attention")) }
	for i := 0; i < 2; i++ {
		if st := s.RunPass(context.Background(), nil, 0); st.ScreeningParkedReadFailures != 1 || st.Errors != 1 {
			t.Fatalf("%+v", st)
		}
	}
	if count() != 0 {
		t.Fatal("fewer than N consecutive failures raise nothing")
	}
	fail = false
	s.RunPass(context.Background(), nil, 0) // a success resets the counter
	fail = true
	for i := 0; i < 2; i++ {
		s.RunPass(context.Background(), nil, 0)
	}
	if count() != 0 {
		t.Fatal("a success in between resets the consecutive count")
	}
	s.RunPass(context.Background(), nil, 0) // the Nth consecutive failure
	got := alertsByDisc(t, pool, f.tenantID, "aml.screening_attention")
	a := got["monitor:tenant:"+f.tenantID.String()]
	if len(got) != 1 || !strings.Contains(a, "monitor_failing") || strings.Contains(a, "person") || strings.Contains(a, f.playerAccountID.String()) {
		t.Fatalf("one platform alert, reason only: %v", got)
	}
}
