//go:build integration

package aml_test

// lic30 (ledger-finance M-1 / M-3 / LOW-2, security L-1 / L-2): the monitor still sees the officer queue when no policy gates withdrawals,
// the re-screen interval floor, the bounded effective time, strictness against the version in force and the latest, and the author lock.
// All values are TEST FIXTURE VALUES.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// M-1: with NO gating policy a person with an unresolved needs_officer request is still denied, so the monitor still counts them. The
// threshold is the LAST gating version's (60 s here); the dead-worker buckets stay empty.
func TestLic30_M1_OfficerQueueIsStillScannedWhenNoPolicyGatesWithdrawals(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.gate(60*time.Second, time.Now().Add(-110*time.Second)) // the last gating version: alert after 60 s
	p, _ := tw.flagged(aml.MockMarkerPEP + "1")               // needs_officer, unresolved
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now())
	if !tw.gateDenies(p) {
		t.Fatal("fixture: an unresolved needs_officer request denies whatever the policy says now")
	}
	sc := tw.scan(nil, 100)
	if sc.Officer != 1 || sc.Parked != 0 || sc.Overdue != 0 || sc.Scanned != 1 {
		t.Fatalf("no gating policy, officer queue must still be scanned: %+v", sc)
	}
	time.Sleep(62 * time.Second) // older than the last gating version's threshold
	sc = tw.scan(nil, 100)
	if sc.Officer != 1 || sc.OfficerOverdue != 1 || sc.Overdue != 0 {
		t.Fatalf("overdue officer queue without a gating policy: %+v", sc)
	}
	var n int
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_gate_overdue($1, 100)`, tw.Tenant).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("the worker's trapped count must not return 0 without a gating policy: n=%d err=%v", n, err)
	}
	_ = ctx
}

// failed_terminal needs an officer too: it counts in the officer bucket, not the dead-worker one.
func TestLic30_FailedTerminalRequestCountsWithTheOfficerQueue(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.gate(60*time.Second, time.Now().Add(-110*time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerUnavailable)
	wk := tw.worker()
	wk.Config.MaxAttempts = 1
	wk.RunPass(ctx)
	var st string
	for _, r := range tw.requests(p.PersonID) {
		st = r.Status
	}
	if st != "failed_terminal" {
		t.Fatalf("fixture: the request must end failed_terminal, is %q", st)
	}
	if sc := tw.scan(nil, 100); sc.Officer != 1 || sc.Parked != 0 || sc.Overdue != 0 {
		t.Fatalf("failed_terminal belongs to the officer bucket: %+v", sc)
	}
}

// M-3: the re-screen interval floor (an hour; and twice the gate alert threshold of the jurisdiction), in Go AND in the database.
func TestLic30_M3_RescreenIntervalFloor(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	A := tw.q.Officers[0].Actor
	in := func(d time.Duration) aml.SetScreeningPolicyInput {
		x := policyInput(tw.jur, time.Now())
		x.RescreenInterval, x.BackfillGrace = d, 0
		return x
	}
	for _, d := range []time.Duration{time.Second, 60 * time.Second, 59 * time.Minute, time.Hour - time.Second} {
		if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, in(d)); aml.RefusalCode(err) != "rescreen_interval_below_floor" || !errors.Is(err, aml.ErrInvalidInput) {
			t.Errorf("Go: %v: %v (code %q)", d, err, aml.RefusalCode(err))
		}
		if _, err := tw.svc.AuthorScreeningPolicySkippingGoChecks(ctx, A, in(d)); aml.RefusalCode(err) != "rescreen_interval_below_floor" {
			t.Errorf("database: %v: %v (code %q)", d, err, aml.RefusalCode(err))
		}
	}
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, in(time.Hour)); err != nil {
		t.Fatalf("exactly the floor is accepted (no gating policy yet): %v", err)
	}
	// With a gating policy (alert after 1 h) the interval must be at least twice that.
	tw.gate(time.Hour, time.Now().Add(-110*time.Second))
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, in(time.Hour+30*time.Minute)); aml.RefusalCode(err) != "rescreen_interval_below_alert_threshold" {
		t.Fatalf("1.5 h with a 1 h alert threshold: %v (code %q)", err, aml.RefusalCode(err))
	}
	// A longer interval is the loosening rule's business (the latest is 1 h); equal to twice the threshold is not above 1 h, so use the
	// unrouted setter to move the latest version to 10 h first.
	if _, err := tw.svc.SetScreeningPolicy(ctx, A, func() aml.SetScreeningPolicyInput { x := in(10 * time.Hour); return x }()); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, in(2*time.Hour)); err != nil {
		t.Fatalf("exactly twice the alert threshold: %v", err)
	}
}

// L-2: the effective time is bounded to [now-2min, now+30d] in Go and in the database.
func TestLic30_L2_EffectiveFromIsBounded(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	j := w.jurisdiction()
	w.bindLicence(j)
	A := q.Officers[0].Actor
	at := func(d time.Duration) aml.SetScreeningPolicyInput { return policyInput(j, time.Now().Add(d)) }
	for _, d := range []time.Duration{-3 * time.Minute, -time.Hour, 31 * 24 * time.Hour} {
		if _, err := svc.AuthorScreeningPolicy(ctx, A, at(d)); aml.RefusalCode(err) != "policy_effective_from_out_of_range" {
			t.Errorf("Go %v: %v (code %q)", d, err, aml.RefusalCode(err))
		}
		if _, err := svc.AuthorScreeningPolicySkippingGoChecks(ctx, A, at(d)); aml.RefusalCode(err) != "policy_effective_from_out_of_range" {
			t.Errorf("database %v: %v (code %q)", d, err, aml.RefusalCode(err))
		}
	}
	for _, d := range []time.Duration{-90 * time.Second, 0, 29 * 24 * time.Hour} {
		x := at(d)
		if _, err := svc.AuthorScreeningPolicy(ctx, A, x); err != nil {
			t.Errorf("%v is inside the bound: %v", d, err)
		}
	}
}

// LOW-2: strictness is checked against the version in force AND the latest, so a version at least as strict as both may take effect
// earlier than a pending, future-dated one; but never looser than either.
func TestLic30_Low2_StrictnessAgainstTheVersionInForceAndTheLatest(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour) // in force: both lists, 24 h, grace 1 h, effective now-1h
	A := tw.q.Officers[0].Actor
	v := func(interval, grace time.Duration, from time.Duration) aml.SetScreeningPolicyInput {
		x := policyInput(tw.jur, time.Now().Add(from))
		x.RescreenInterval, x.BackfillGrace = interval, grace
		return x
	}
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, v(12*time.Hour, 30*time.Minute, time.Hour)); err != nil { // pending: effective in an hour
		t.Fatalf("a pending stricter version: %v", err)
	}
	// Tighter than the version in force (24 h) but looser than the PENDING latest (12 h): refused (only the latest-version comparison sees it).
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, v(18*time.Hour, 30*time.Minute, 0)); aml.RefusalCode(err) != "policy_loosening_refused" {
		t.Fatalf("looser than the pending latest: %v", err)
	}
	// Stricter than BOTH the pending and the version in force, taking effect now (earlier than the pending one): allowed.
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, v(6*time.Hour, 10*time.Minute, 0)); err != nil {
		t.Fatalf("at least as strict as both, earlier than the pending one: %v", err)
	}
	// Tighter than the version in force (24 h) but looser than the latest (6 h): refused.
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, v(8*time.Hour, 10*time.Minute, 0)); aml.RefusalCode(err) != "policy_loosening_refused" {
		t.Fatalf("looser than the latest: %v", err)
	}
	// Never earlier than the version in force (now the 6 h version, which took effect just now).
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, v(6*time.Hour, 10*time.Minute, -90*time.Second)); aml.RefusalCode(err) != "policy_loosening_refused" {
		t.Fatalf("earlier than the version in force: %v", err)
	}
}

// L-1: authors of the same tenant and jurisdiction are serialised by an advisory lock taken before the previous version is read.
func TestLic30_L1_AuthorTakesTheJurisdictionLock(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	A := tw.q.Officers[0].Actor
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeRelease) // a failing assertion must not leave the lock transaction open (the pool would never close)
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1::text || $2::text, 0))`, tw.Tenant, tw.jur); err != nil {
				return err
			}
			close(held)
			<-release
			return nil
		})
	}()
	<-held
	x := policyInput(tw.jur, time.Now())
	x.RescreenInterval = 12 * time.Hour
	done := make(chan error, 1)
	go func() { _, err := tw.svc.AuthorScreeningPolicy(ctx, A, x); done <- err }()
	select {
	case err := <-done:
		t.Fatalf("the author must wait for the jurisdiction lock, returned %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	closeRelease()
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("after the lock is released the author completes: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the author never completed after the lock was released")
	}
	// A DIFFERENT jurisdiction's lock does not block it.
	other := policyInput(tw.jur, time.Now())
	other.RescreenInterval = 6 * time.Hour
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, other); err != nil {
		t.Fatal(err)
	}
}
