//go:build integration

package aml_test

// d1 round 4 (ledger-finance R3-L1 on 0193): a policy version whose transaction COMMITS LATE (created_at fixed at insert, before the claim; visible
// only after the screening finished) must still count against a worker clear, ordered by version, not by clock. Also the bound of the auto-reviewed
// re-flag route of the gate (admitted gap of round 3). TEST FIXTURE VALUES.

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// holdOpenPool wraps the owner pool: the function body runs, then the transaction is held OPEN (not committed) until release is closed.
type holdOpenPool struct {
	inner    *db.Pool
	inserted chan struct{}
	release  chan struct{}
}

func (s *holdOpenPool) WithPrincipalScope(ctx context.Context, tenantID, principalID uuid.UUID, fn db.TxFunc) error {
	return s.inner.WithPrincipalScope(ctx, tenantID, principalID, s.wrap(fn))
}

func (s *holdOpenPool) WithPlatformActingInTenant(ctx context.Context, principalID, targetTenantID, requestID uuid.UUID, op string, fn db.TxFunc) error {
	return s.inner.WithPlatformActingInTenant(ctx, principalID, targetTenantID, requestID, op, s.wrap(fn))
}

func (s *holdOpenPool) wrap(fn db.TxFunc) db.TxFunc {
	return func(ctx context.Context, tx pgx.Tx) error {
		err := fn(ctx, tx)
		if err == nil {
			close(s.inserted)
			<-s.release
		}
		return err
	}
}

// lateVersion starts authoring a 2 s version whose transaction stays open; commit() releases it and waits for the commit.
func (tw *trigWorld) lateVersion(interval time.Duration) (commit func()) {
	tw.t.Helper()
	sp := &holdOpenPool{inner: ownerPool(tw.t), inserted: make(chan struct{}), release: make(chan struct{})}
	svc := amlfix.Service(tw.t, sp)
	done := make(chan error, 1)
	go func() {
		_, err := svc.SetScreeningPolicy(context.Background(), tw.q.Officers[0].Actor, aml.SetScreeningPolicyInput{JurisdictionID: tw.jur, Lists: bothLists,
			RescreenInterval: interval, EffectiveFrom: time.Now(), LegalReviewReference: "TEST FIXTURE REF - NOT LEGAL ADVICE", ReasonCode: "fixture_policy"})
		done <- err
	}()
	<-sp.inserted
	time.Sleep(50 * time.Millisecond) // the claim below is strictly later than the insert
	return func() {
		close(sp.release)
		if err := <-done; err != nil {
			tw.t.Fatalf("late committing version: %v", err)
		}
	}
}

// inflightProv authors a version at a chosen point of the provider call.
type inflightProv struct {
	inner *aml.MockScreeningProvider
	fire  func()
}

func (r *inflightProv) ID() string                              { return r.inner.ID() }
func (r *inflightProv) Capabilities() aml.ScreeningCapabilities { return r.inner.Capabilities() }
func (r *inflightProv) Screen(ctx context.Context, in aml.ScreeningRequest) (aml.ScreeningResult, error) {
	if f := r.fire; f != nil {
		r.fire = nil
		f()
		time.Sleep(20 * time.Millisecond)
	}
	return r.inner.Screen(ctx, in)
}

func (tw *trigWorld) workerWith(p aml.ScreeningProvider) *aml.ScreeningWorker {
	wk := aml.NewScreeningWorker(tw.RT, p, aml.MockSubjectKeys{})
	wk.TenantScope = func() []uuid.UUID { return []uuid.UUID{tw.Tenant} }
	wk.Config.BackoffBase = 10 * time.Millisecond
	wk.Config.BackoffCap = 20 * time.Millisecond
	return wk
}

func (tw *trigWorld) dueNow() int {
	tw.t.Helper()
	var n int
	if err := tw.RT.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_enqueue_due($1, 100)`, tw.Tenant).Scan(&n)
	}); err != nil {
		tw.t.Fatal(err)
	}
	return n
}

// staleThenLengthened: stale under the short version, still held after a lengthening, and the scheduler agrees (identical predicate).
func (tw *trigWorld) staleThenLengthened(p player, label string) {
	tw.t.Helper()
	time.Sleep(3 * time.Second)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		tw.t.Fatalf("%s: premise: stale under the short version", label)
	}
	tw.policyVersion(bothLists, time.Hour, 0)
	if !tw.heldFor(p, aml.OpWithdrawal) || !tw.gateDenies(p) {
		tw.t.Fatalf("%s: the expired clear was REVIVED by a lengthening", label)
	}
	pending := 0
	for _, r := range tw.requests(p.PersonID) {
		if r.Status == "pending" || r.Status == "claimed" {
			pending++
		}
	}
	if n := tw.dueNow(); n+pending != 1 {
		tw.t.Fatalf("%s: the gate holds but the scheduler does not consider the person due (due=%d pending=%d): predicate mismatch", label, n, pending)
	}
}

func r4World(t *testing.T) (*trigWorld, player) {
	tw := newTrigWorld(t, time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	return tw, tw.player()
}

func TestD1R4_LateCommittingVersion_StillCountsAgainstAWorkerClear(t *testing.T) {
	ctx := context.Background()
	tw, p := r4World(t)
	commit := tw.lateVersion(2 * time.Second)
	tw.worker().RunPass(ctx) // claim, read the old 1 h policy, screen and finish while the 2 s version is inserted but NOT committed
	if rs := tw.requests(p.PersonID); len(rs) == 0 || rs[0].Status != "done" || tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatalf("premise: the clear was applied under the old policy: %+v", rs)
	}
	commit()
	tw.staleThenLengthened(p, "late committing version, worker clear")
}

// The same for a version that commits in the middle of a re-screen whose bound is the claim (not the late-commit rule): kept as the control.
func TestD1R4_VersionOrderIsNotClockOrder_AVersionCreatedBeforeTheClaimPolicyDoesNotCount(t *testing.T) {
	ctx := context.Background()
	tw, p := r4World(t)
	tw.policyVersion(bothLists, 2*time.Second, 0) // created BEFORE the claim policy below
	tw.policyVersion(bothLists, time.Hour, 0)     // the claim policy
	tw.worker().RunPass(ctx)
	time.Sleep(3 * time.Second)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a version created before the claim policy is part of the snapshot and must not expire the clear")
	}
}

// Auto-reviewed re-flag route of the gate (reviewed_from_request_id IS NOT NULL), both bounds: a version in force DURING the re-flag screening, and one that
// commits late. mode is "inflight" or "late".
func reflagWorld(t *testing.T, mode string) {
	ctx := context.Background()
	tw, p := r4World(t)
	tw.setVendorRef(p, aml.MockMarkerPEP+"1")
	tw.worker().RunPass(ctx)
	A, B := tw.q.Officers[0].Actor, tw.q.Officers[1].Actor
	req := tw.needsOfficer(p)
	rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(req.ID), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: bothLists, MatchLists: []aml.ListType{aml.ListPEP}, ReasonCode: "pep_accepted"})
	if err != nil || rec.HoldID == nil {
		t.Fatalf("record: %+v %v", rec, err)
	}
	if _, err := tw.svc.LiftHold(ctx, B, *rec.HoldID, "pep_accepted"); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("premise: an accepted PEP match satisfies the gate")
	}
	// make the officer's lifted match stale so the person is due for a scheduled re-screen, then lengthen again
	tw.policyVersion(bothLists, 2*time.Second, 0)
	time.Sleep(3 * time.Second)
	tw.policyVersion(bothLists, time.Hour, 0)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("premise: the officer-lifted match is stale and was not revived")
	}
	commit := func() {}
	switch mode {
	case "inflight":
		tw.workerWith(&inflightProv{inner: aml.NewMockScreeningProvider(), fire: func() { tw.policyVersion(bothLists, 2*time.Second, 0) }}).RunPass(ctx)
	case "late":
		commit = tw.lateVersion(2 * time.Second)
		tw.worker().RunPass(ctx)
	}
	var reviewed int
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_requests WHERE tenant_id=$1 AND person_id=$2 AND status='needs_officer' AND reviewed_from_request_id IS NOT NULL`, tw.Tenant, p.PersonID).Scan(&reviewed)
	}); err != nil {
		t.Fatal(err)
	}
	if reviewed == 0 {
		t.Fatal("the probe did not produce an auto-reviewed re-flag: the route is not exercised")
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("premise: the fresh auto-reviewed re-flag satisfies the gate")
	}
	commit()
	tw.staleThenLengthened(p, "auto-reviewed re-flag, "+mode)
}

func TestD1R4_AutoReviewedReflagRoute_VersionInForceDuringTheScreeningCounts(t *testing.T) {
	reflagWorld(t, "inflight")
}

func TestD1R4_AutoReviewedReflagRoute_LateCommittingVersionCounts(t *testing.T) {
	reflagWorld(t, "late")
}
