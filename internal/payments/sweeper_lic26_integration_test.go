//go:build integration

package payments

// lic26 (ledger-finance N-9, security F-7): the PRODUCTION sweep path (RunPass -> sweepTenant) reads the persons parked at the payout
// gate for an outstanding automatic AML screening, logs COUNTS only and raises the platform-routed aml.screening_gate_trapped alert
// (counts only) when persons are parked past the gating policy's threshold, so a DEAD screening worker is visible. The overdue
// computation itself is tested against a real trigger policy in internal/aml (TestLic26_N9_ParkedOverdue); here the alert threshold
// is injected through the test seam because authoring a trigger policy needs the officer-proof machinery of that package.

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func seedParkedDecisions(t *testing.T, pool *db.Pool, f orchFixture, n int) (personID uuid.UUID) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, f.playerAccountID).Scan(&personID); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if _, err := tx.Exec(ctx, `INSERT INTO aml_enforcement_decisions (tenant_id, player_account_id, person_id, operation, kyc_outcome, hold_outcome, public_outcome, allowed, correlation_id)
				VALUES ($1, $2, $3, 'withdrawal_payout', 'passed', 'held', 'unavailable', false, $4)`, f.tenantID, f.playerAccountID, personID, uuid.New()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("seed parked decisions: %v", err)
	}
	return personID
}

func trappedAlerts(t *testing.T, pool *db.Pool, tenant uuid.UUID) (n int, attrs string) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*), COALESCE(string_agg(attributes::text, ','), '') FROM alerts WHERE kind = 'aml.screening_gate_trapped' AND subject_tenant_id = $1 AND discriminator = 'sweeper:tenant:' || $1::text`, tenant).Scan(&n, &attrs)
	}); err != nil {
		t.Fatalf("read alerts: %v", err)
	}
	return n, attrs
}

// The production read runs in RunPass against the real function: parked persons are counted and logged as counts, with no alert while
// no gating policy sets a threshold.
func TestLic26_N9_ProductionSweepReadsAndLogsParkedCountsOnly(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	person := seedParkedDecisions(t, pool, f, 2)
	spy := newLoopProvider("mock-psp-lic26-parked")
	s := newLoopSweeper(pool, spy.orchestrator(), false, nil)
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	st := s.RunPass(context.Background(), logger, 0)
	// lic27b: only persons the screening gate STILL denies count, and no policy gates withdrawals here, so the stale decision rows
	// count for nothing (pin changed from "one person parked"): the real function ran without error and reported 0 / 0.
	if st.Errors != 0 || st.ScreeningParked != 0 || st.ScreeningParkedOverdue != 0 {
		t.Fatalf("no gating policy: nothing is parked by the screening gate: %+v", st)
	}
	// The count log line (parked > 0) is exercised through the injected read.
	s.testScreeningParked = func(uuid.UUID) (int, int, error) { return 1, 0, nil }
	buf.Reset()
	if st := s.RunPass(context.Background(), logger, 0); st.ScreeningParked != 1 {
		t.Fatalf("%+v", st)
	}
	out := buf.String()
	if !strings.Contains(out, "payouts parked for an outstanding AML screening") || !strings.Contains(out, "parked=1") {
		t.Fatalf("the parked count must be logged: %s", out)
	}
	for _, secret := range []string{person.String(), f.playerAccountID.String()} {
		if strings.Contains(out, secret) {
			t.Fatalf("the log carries counts only, found %s in %s", secret, out)
		}
	}
	if n, _ := trappedAlerts(t, pool, f.tenantID); n != 0 {
		t.Fatalf("no threshold, no alert: %d", n)
	}
}

// Persons parked past the threshold raise the p1 alert with the COUNT as its only attribute; zero overdue raises nothing; a failing
// or panicking read is contained, counted and raises nothing.
func TestLic26_N9_ProductionSweepRaisesTheTrappedAlertWithCountsOnly(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	spy := newLoopProvider("mock-psp-lic26-alert")
	s := newLoopSweeper(pool, spy.orchestrator(), false, nil)

	s.testScreeningParked = func(uuid.UUID) (int, int, error) { return 3, 0, nil }
	if st := s.RunPass(context.Background(), nil, 0); st.Errors != 0 || st.ScreeningParked != 3 {
		t.Fatalf("%+v", st)
	}
	if n, _ := trappedAlerts(t, pool, f.tenantID); n != 0 {
		t.Fatalf("parked but not overdue raises nothing, got %d", n)
	}

	s.testScreeningParked = func(uuid.UUID) (int, int, error) { return 0, 0, errors.New("boom") }
	if st := s.RunPass(context.Background(), nil, 0); st.Errors != 1 {
		t.Fatalf("a failing read is counted: %+v", st)
	}
	s.testScreeningParked = func(uuid.UUID) (int, int, error) { panic("boom") }
	if st := s.RunPass(context.Background(), nil, 0); st.Errors != 1 {
		t.Fatalf("a panicking read is contained and counted: %+v", st)
	}
	if n, _ := trappedAlerts(t, pool, f.tenantID); n != 0 {
		t.Fatalf("a failed read raises nothing, got %d", n)
	}

	s.testScreeningParked = func(uuid.UUID) (int, int, error) { return 4, 2, nil }
	if st := s.RunPass(context.Background(), nil, 0); st.Errors != 0 || st.ScreeningParkedOverdue != 2 {
		t.Fatalf("%+v", st)
	}
	n, attrs := trappedAlerts(t, pool, f.tenantID)
	if n != 1 || !strings.Contains(attrs, `"persons": 2`) && !strings.Contains(attrs, `"persons":2`) {
		t.Fatalf("one open alert with the overdue count: n=%d attributes=%s", n, attrs)
	}
	for _, k := range []string{"person_id", "account", "amount"} {
		if strings.Contains(attrs, k) {
			t.Fatalf("counts only, attributes: %s", attrs)
		}
	}
}
