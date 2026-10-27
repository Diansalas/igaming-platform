//go:build integration

package aml_test

// lic27b: the accuracy of the sweeper's parked / overdue read (only persons the gate STILL denies, a window derived from the largest
// alert threshold, persons with no payout decision at all), the replace alert clock, the confirmed-outcome CHECK and migration 0188.
// All values are TEST FIXTURE VALUES.

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// gate authors a gating trigger policy with the given alert threshold and effective time.
func (tw *trigWorld) gate(alert time.Duration, from time.Time) {
	tw.t.Helper()
	tw.heartbeat()
	if _, err := tw.svc.SetScreeningTriggerPolicy(context.Background(), tw.q.Officers[0].Actor, aml.SetTriggerPolicyInput{JurisdictionID: tw.jur,
		Events: []aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, GateWithdrawal: true, GateAlertAfter: alert,
		EffectiveFrom: from, LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}); err != nil {
		tw.t.Fatalf("gate policy: %v", err)
	}
}

func (tw *trigWorld) parkedAt(p player, at time.Time) {
	tw.t.Helper()
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO aml_enforcement_decisions (tenant_id, player_account_id, person_id, operation, kyc_outcome, hold_outcome, public_outcome, allowed, correlation_id, decided_at)
			VALUES ($1, $2, $3, 'withdrawal_payout', 'passed', 'held', 'unavailable', false, $4, $5)`, tw.Tenant, p.AccountID, p.PersonID, uuid.New(), at)
		return e
	}); err != nil {
		tw.t.Fatal(err)
	}
}

func (tw *trigWorld) parkedOverdue() (int, int) {
	tw.t.Helper()
	var parked, overdue int
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		parked, overdue, e = aml.ScreeningParkedOverdue(ctx, tx, tw.Tenant)
		return e
	}); err != nil {
		tw.t.Fatal(err)
	}
	return parked, overdue
}

// LOW-1 (a): once the screening cleared the person, a stale "parked" decision row raises nothing.
func TestLic27b_Low1_ClearedPersonIsNotParkedOrOverdue(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.gate(time.Hour, time.Now().Add(-time.Second))
	p := tw.player() // a pending registration request: the gate denies
	tw.parkedAt(p, time.Now().Add(-3*time.Hour))
	tw.parkedAt(p, time.Now().Add(-time.Minute))
	if parked, overdue := tw.parkedOverdue(); parked != 1 || overdue != 1 {
		t.Fatalf("still denied: parked=%d overdue=%d, want 1 and 1", parked, overdue)
	}
	tw.worker().RunPass(ctx) // clears the person
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("fixture: the worker cleared the person")
	}
	if parked, overdue := tw.parkedOverdue(); parked != 0 || overdue != 0 {
		t.Fatalf("a cleared person must not count although the last decision row says parked: parked=%d overdue=%d", parked, overdue)
	}
}

// LOW-1 (c): an alert threshold longer than the old 7-day window still fires (the look-back is derived from the threshold).
func TestLic27b_Low1_ThresholdLongerThanTheSevenDayWindowStillFires(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.gate(20*24*time.Hour, time.Now().Add(-time.Second))
	p := tw.player()
	tw.parkedAt(p, time.Now().Add(-25*24*time.Hour)) // parked for 25 days with a 20 day threshold
	if parked, overdue := tw.parkedOverdue(); parked != 1 || overdue != 1 {
		t.Fatalf("parked=%d overdue=%d, want 1 and 1 (the 25-day-old run lies outside 7 days)", parked, overdue)
	}
}

// LOW-1 (b): a person the gate denies who never retried a withdrawal (no decision row at all) is overdue once gated longer than the
// threshold, so a dead worker AND no retries is visible.
func TestLic27b_Low1_DeniedPersonWithNoDecisionRowIsOverdue(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	p := tw.player() // registered BEFORE the gate: no registration request is queued
	_ = p
	tw.gate(60*time.Second, time.Now().Add(-110*time.Second))
	if parked, overdue := tw.parkedOverdue(); parked != 0 || overdue != 1 {
		t.Fatalf("parked=%d overdue=%d, want 0 and 1 (denied for 110 s, threshold 60 s, no payout decision rows)", parked, overdue)
	}
	// A recently gated person is not yet overdue.
	tw2 := newTrigWorld(t, 24*time.Hour, 0)
	tw2.player()
	tw2.gate(time.Hour, time.Now().Add(-time.Second))
	if _, overdue := tw2.parkedOverdue(); overdue != 0 {
		t.Fatalf("not yet overdue, got %d", overdue)
	}
}

// LOW-2: queueing a screening for a person with no pending request does not reset the alert clock.
func TestLic27b_Low2_QueueForAPersonWithNoRequestKeepsTheGateAge(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	p := tw.player()
	tw.gate(60*time.Second, time.Now().Add(-110*time.Second))
	res, err := tw.svc.ReplaceScreeningRequest(ctx, tw.q.Officers[0].Actor, p.PersonID, nil, "queue_none")
	if err != nil {
		t.Fatal(err)
	}
	var age time.Duration
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var secs float64
		e := tx.QueryRow(ctx, `SELECT extract(epoch FROM now() - created_at) FROM aml_screening_requests WHERE id = $1`, res.NewRequestID).Scan(&secs)
		age = time.Duration(secs * float64(time.Second))
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if age < 100*time.Second {
		t.Fatalf("the queued request must carry the gate's age (about 110 s), not now: %v", age)
	}
	var overdue int
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_gate_overdue($1, 100)`, tw.Tenant).Scan(&overdue)
	}); err != nil || overdue != 1 {
		t.Fatalf("the trapped-person clock must not restart: overdue=%d err=%v", overdue, err)
	}
}

// S-4: a confirmed request that does not name the sanctions list cannot exist.
func TestLic27b_S4_ConfirmedRequestMustNameTheSanctionsList(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	id := tw.requests(p.PersonID)[0].ID
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_request_claim($1, 10, 60)`, tw.Tenant).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	pid := tw.policyID()
	upd := func(lists string) error {
		return tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			// d1 D10 (0192): a request that ends needs_officer / done must carry its screening provenance (a new CHECK), so the fixture states it;
			// otherwise the PEP-only case below would fail on THAT check and no longer prove the sanctions rule.
			_, e := tx.Exec(ctx, `UPDATE aml_screening_requests SET status = 'needs_officer', outcome = 'confirmed', match_lists = `+lists+`,
				provider_id = 'mock', screened_at = now(), completed_at = now(), claim_token = NULL, lease_until = NULL,
				screened_policy_id = $2, screened_lists = ARRAY['pep', 'sanctions'], screened_interval_seconds = 86400 WHERE id = $1`, id, pid)
			return e
		})
	}
	if err := upd(`ARRAY['pep']::text[]`); err == nil || !strings.Contains(err.Error(), "23514") {
		t.Fatalf("a confirmed outcome naming only the PEP list must violate the CHECK: %v", err)
	}
	if err := upd(`ARRAY['sanctions']::text[]`); err != nil {
		t.Fatalf("a confirmed outcome naming the sanctions list is legal: %v", err)
	}
}

// 0188: the parked index exists with the expected definition, and a differently defined same-named index is NOT silently kept.
func TestMigration0188_ParkedIndex_DefinedAndNeverSilentlyKept(t *testing.T) {
	ctx := context.Background()
	pool, dir := scratchThrough(t, "m188_", 187)
	src := filepath.Join("..", "..", "migrations")
	entries, _ := os.ReadDir(src)
	for _, e := range entries {
		if n, err := strconv.ParseInt(e.Name()[:4], 10, 64); err == nil && n == 188 {
			b, _ := os.ReadFile(filepath.Join(src, e.Name()))
			if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `CREATE INDEX aml_enforcement_decisions_tenant_op_decided ON aml_enforcement_decisions (tenant_id)`)
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MigrateUp(ctx, dir); err == nil {
		t.Fatal("0188 must fail when a differently defined index of the same name exists")
	}
	var def string
	pool2, _ := scratchThrough(t, "m188b_", 188)
	if err := pool2.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'aml_enforcement_decisions_tenant_op_decided'`).Scan(&def)
	}); err != nil || !strings.Contains(def, "(tenant_id, operation, decided_at DESC)") {
		t.Fatalf("index definition: %q err=%v", def, err)
	}
}

var _ = db.Pool{}
