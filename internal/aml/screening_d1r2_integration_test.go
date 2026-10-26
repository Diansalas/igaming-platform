//go:build integration

package aml_test

// d1 round 2 (reviews of 0192): ledger-finance HIGH-1 (shorten then lengthen revives an expired clear), security M-1 / LOW-1 (freshness runs from when the
// screening HAPPENED; refuse a future or ancient screened_at and a finish after the lease), L-1 (the policy named at finish is the one in force at CLAIM
// time), L-2 (only versions that were actually in force count), L-3 (enqueue starvation). TEST FIXTURE VALUES.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

func (tw *trigWorld) policyVersionAt(lists []aml.ListType, interval time.Duration, from time.Time) {
	tw.t.Helper()
	in := aml.SetScreeningPolicyInput{JurisdictionID: tw.jur, Lists: lists, RescreenInterval: interval, EffectiveFrom: from,
		LegalReviewReference: "TEST FIXTURE REF - NOT LEGAL ADVICE", ReasonCode: "fixture_policy"}
	if _, err := ownerSvc(tw.t).SetScreeningPolicy(context.Background(), tw.q.Officers[0].Actor, in); err != nil {
		tw.t.Fatalf("policy version (owner path): %v", err)
	}
}

// claimOne claims one request (lease in seconds) and returns its id and token.
func (tw *trigWorld) claimOne(lease int) (uuid.UUID, uuid.UUID) {
	tw.t.Helper()
	var id, token uuid.UUID
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT o_id, o_claim_token FROM aml_screening_request_claim($1, 1, $2)`, tw.Tenant, lease).Scan(&id, &token)
	}); err != nil {
		tw.t.Fatalf("claim: %v", err)
	}
	return id, token
}

// finishClear ends a claimed request with a clear that names the policy and the given screened_at SQL expression.
func (tw *trigWorld) finishClear(id, token uuid.UUID, policy any, screenedAt string) (applied bool, status string, err error) {
	tw.t.Helper()
	err = tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT o_applied, o_status FROM aml_screening_request_finish($1, $2, $3, 'clear', NULL, NULL, 'mock-screening', 'ref-1', $4, `+screenedAt+`, 0, NULL, $5)`,
			tw.Tenant, id, token, strings.Repeat("c", 64), policy).Scan(&applied, &status)
	})
	return
}

// ---- LF HIGH-1 ------------------------------------------------------------------------------------------------------------------

func TestD1R2_ShortenThenLengthen_NeverRevivesAnExpiredClear_AParkedWithdrawalStaysParked(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	p := tw.player()
	wk := tw.worker()
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a fresh clear releases")
	}
	tw.policyVersion(bothLists, 3*time.Second, 0) // shortened
	time.Sleep(4 * time.Second)
	tw.parkedAt(p, time.Now()) // the withdrawal's payout claim was parked by the gate
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("premise: the clear is stale under the shortened interval")
	}
	tw.policyVersion(bothLists, time.Hour, 0) // lengthened again
	if !tw.heldFor(p, aml.OpWithdrawal) || !tw.gateDenies(p) {
		t.Fatal("LF HIGH-1: shortening and then lengthening the interval revived an expired clear and released a parked withdrawal with no new screening")
	}
	if _, o := tw.parkedOverdue(); o < 0 {
		t.Fatal("unreachable")
	}
	wk.RunPass(ctx) // due: re-screened, and only that releases
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the re-screen releases")
	}
}

// ---- SEC L-2 ----------------------------------------------------------------------------------------------------------------------

func TestD1R2_OnlyVersionsActuallyInForceCount_APendingNarrowingNeutralisedByACounterVersion(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	tw.policyVersionAt([]aml.ListType{aml.ListPEP}, 24*time.Hour, time.Now().Add(3*time.Second)) // P2: pep only, pending
	tw.policyVersion(bothLists, 24*time.Hour, 0)                                                 // P3: both lists NOW, created later, neutralises P2
	p := tw.player()
	tw.worker().RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("cleared")
	}
	time.Sleep(4 * time.Second) // P2's effective time arrives; it was never in force (P3 supersedes it)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("SEC L-2: a pending narrowing version that never took force invalidated everyone's clear when its time arrived")
	}
}

// ---- SEC L-1 ----------------------------------------------------------------------------------------------------------------------

func TestD1R2_FinishMustNameThePolicyInForceAtClaimTime(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	tw.heartbeat()
	p1 := tw.policyID()
	p := tw.player()
	tw.policyVersion([]aml.ListType{aml.ListPEP}, 24*time.Hour, 0) // P2: pep only
	p2 := tw.policyID()
	id, tok := tw.claimOne(60) // claimed UNDER P2
	if _, _, err := tw.finishClear(id, tok, p1, "now()"); pgCode(err) != "AT001" {
		t.Fatalf("SEC L-1: a finish naming the superseded P1 (both lists) after the claim under P2 (pep only) must be refused: %v", err)
	}
	if ok, st, err := tw.finishClear(id, tok, p2, "now()"); err != nil || !ok || st != "done" {
		t.Fatalf("naming the claim-time policy finishes: %v %v %q", err, ok, st)
	}
	// A policy changed AFTER the claim does not matter: the claim-time version is the one to name.
	q := tw.player()
	id2, tok2 := tw.claimOne(60)
	tw.policyVersion(bothLists, 24*time.Hour, 0) // P3
	p3 := tw.policyID()
	if _, _, err := tw.finishClear(id2, tok2, p3, "now()"); pgCode(err) != "AT001" {
		t.Fatalf("a version created after the claim is not the claim-time version: %v", err)
	}
	if ok, _, err := tw.finishClear(id2, tok2, p2, "now()"); err != nil || !ok {
		t.Fatalf("the claim-time version finishes: %v %v", err, ok)
	}
	_, _ = p, q
}

// ---- SEC M-1 / LF LOW-1 ---------------------------------------------------------------------------------------------------------------

func TestD1R2_ScreenedAt_FutureAndAncientAreRefused_FreshnessRunsFromTheScreening(t *testing.T) {
	tw := newTrigWorld(t, 105*time.Second, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	tw.heartbeat()
	p := tw.player()
	id, tok := tw.claimOne(120)
	pol := tw.policyID()
	for name, at := range map[string]string{"a screened_at an hour in the future": "now() + interval '1 hour'", "a screened_at a day before the claim": "now() - interval '1 day'"} {
		if _, _, err := tw.finishClear(id, tok, pol, at); pgCode(err) != "AT001" {
			t.Errorf("%s must be refused (AT001): %v", name, err)
		}
	}
	// 100 s before the claim is inside the tolerance: accepted, and the clear is only as fresh as the SCREENING (interval 105 s).
	if ok, _, err := tw.finishClear(id, tok, pol, "now() - interval '100 seconds'"); err != nil || !ok {
		t.Fatalf("a screening 100 s old is inside the tolerance: %v %v", err, ok)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("fresh at first (100 s of 105 s)")
	}
	time.Sleep(6 * time.Second)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("SEC M-1: freshness must run from the screening time (106 s old > 105 s), not from the moment the finish was recorded")
	}
}

func TestD1R2_FinishAfterTheLease_IsNotApplied_AndTheRequestIsReclaimed(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	tw.heartbeat()
	tw.player()
	id, tok := tw.claimOne(5)
	pol := tw.policyID()
	time.Sleep(6 * time.Second) // the lease has passed
	ok, st, err := tw.finishClear(id, tok, pol, "now()")
	if err != nil || ok || st != "claimed" {
		t.Fatalf("SEC M-1: a clear finished after its lease must not be applied (a retry follows): applied=%v status=%q err=%v", ok, st, err)
	}
	id2, tok2 := tw.claimOne(60)
	if id2 != id || tok2 == tok {
		t.Fatalf("the request is reclaimed under a new token: %v %v", id2, tok2)
	}
	if ok, _, err := tw.finishClear(id2, tok2, pol, "now()"); err != nil || !ok {
		t.Fatalf("the retry finishes: %v %v", err, ok)
	}
}

// ---- SEC L-3 ----------------------------------------------------------------------------------------------------------------------

func TestD1R2_EnqueueDue_APermanentlyFailedPeriodDoesNotStarveOthers(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventScheduled}, true, time.Now().Add(-time.Second))
	tw.heartbeat()
	var ps []player
	for i := 0; i < 3; i++ {
		ps = append(ps, tw.player())
	}
	enq := func() int {
		var n int
		if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT aml_screening_enqueue_due($1, 1)`, tw.Tenant).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if enq() != 1 {
		t.Fatal("the first pass queues one person")
	}
	id, tok := tw.claimOne(60) // that person's request ends permanently
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var a bool
		var s string
		return tx.QueryRow(ctx, `SELECT o_applied, o_status FROM aml_screening_request_finish($1, $2, $3, 'terminal', NULL, 'unknown', NULL, NULL, NULL, NULL, 0, NULL, NULL)`, tw.Tenant, id, tok).Scan(&a, &s)
	}); err != nil {
		t.Fatal(err)
	}
	if enq() != 1 || enq() != 1 {
		t.Fatal("SEC L-3: a person whose period already failed terminally (same key) starves the others: each later pass must queue the next due person")
	}
	n := 0
	for _, p := range ps {
		n += len(tw.requests(p.PersonID))
	}
	if n != 3 {
		t.Fatalf("every person has exactly one request: %d", n)
	}
}

// SEC M-1 (officer rows): an officer cannot state a screening in the future; an OLD screened_at only shortens freshness.
func TestD1R2_OfficerResult_FutureScreenedAtIsRefused(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	p := tw.player()
	rec := func(at time.Time) error {
		_, err := tw.svc.RecordScreening(context.Background(), tw.q.Officers[0].Actor, aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: "manual-" + uuid.NewString(),
			Trigger: aml.TriggerManual, ProviderID: "officer-manual", SubjectRef: strings.Repeat("b", 64), Lists: bothLists, Outcome: aml.OutcomeClear, ScreenedAt: at, Decision: aml.HoldNone})
		return err
	}
	if err := rec(time.Now().Add(time.Hour)); err == nil {
		t.Fatal("a screening time an hour in the future must be refused")
	}
	if err := rec(time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("an old screening time is allowed (it only shortens freshness): %v", err)
	}
}
