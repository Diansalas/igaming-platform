//go:build integration

package aml_test

// d1 round 3 (ledger-finance NEW-M1 on migration 0192): a policy version that comes into force WHILE a screening is in flight (after the claim,
// before the provider answered) must count for the clear, because the clear was produced from the policy snapshot taken at the claim. TEST FIXTURE VALUES.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

func (tw *trigWorld) gatedWorld(interval time.Duration) {
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	tw.heartbeat()
}

// NEW-M1: claim under 1 h; a 3 s version comes into force during the provider call; the answer's screened_at is LATER than that version's start.
func TestD1R3_PolicyChangeDuringInFlightScreening_AnExpiredClearIsNeverRevivedByALaterLengthening(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, time.Hour, 0)
	tw.gatedWorld(time.Hour)
	p := tw.player()
	id, tok := tw.claimOne(120)
	pol1 := tw.policyID()
	tw.policyVersion(bothLists, 3*time.Second, 0) // in force while the provider is working
	time.Sleep(50 * time.Millisecond)
	if ok, st, err := tw.finishClear(id, tok, pol1, "now()"); err != nil || !ok || st != "done" {
		t.Fatalf("the finish naming the claim-time policy is accepted: %v %v %q", err, ok, st)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("premise: fresh right after the answer")
	}
	time.Sleep(4 * time.Second)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("premise: stale under the 3 s version")
	}
	tw.parkedAt(p, time.Now())
	tw.policyVersion(bothLists, time.Hour, 0) // lengthened again
	if !tw.heldFor(p, aml.OpWithdrawal) || !tw.gateDenies(p) {
		t.Fatal("LF NEW-M1: a version that came into force during the in-flight screening was ignored; lengthening revived the expired clear")
	}
	tw.worker().RunPass(ctx) // the scheduler treats the person as due (same predicate), the re-screen releases
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a real re-screen releases")
	}
}

// A version that was already in force BEFORE the claim is part of the snapshot, so it does not count against the clear; one that starts after the
// finish counts as before.
func TestD1R3_VersionsBeforeTheClaimDoNotCount_VersionsAfterTheFinishDo(t *testing.T) {
	tw := newTrigWorld(t, time.Hour, 0)
	tw.gatedWorld(time.Hour)
	p := tw.player()
	tw.policyVersion(bothLists, 2*time.Second, 0) // short, then lengthened, both BEFORE the claim
	tw.policyVersion(bothLists, time.Hour, 0)
	id, tok := tw.claimOne(120)
	pol := tw.policyID()
	if ok, _, err := tw.finishClear(id, tok, pol, "now()"); err != nil || !ok {
		t.Fatalf("finish: %v %v", err, ok)
	}
	time.Sleep(3 * time.Second)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a version that ended before the claim must not invalidate a clear made after it")
	}
	tw.policyVersion(bothLists, 2*time.Second, 0) // AFTER the finish
	time.Sleep(3 * time.Second)
	tw.policyVersion(bothLists, time.Hour, 0)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a version after the finish still expires the clear for good")
	}
}

// The officer-result path is unchanged: timed from LEAST(recorded_at, screened_at); a version authored after it expires it for good.
func TestD1R3_OfficerResultPathUnchanged(t *testing.T) {
	tw := newTrigWorld(t, time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("premise: no screening yet")
	}
	if _, err := tw.svc.RecordScreening(context.Background(), tw.q.Officers[0].Actor, aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: "manual-" + uuid.NewString(),
		Trigger: aml.TriggerManual, ProviderID: "officer-manual", SubjectRef: strings.Repeat("b", 64), Lists: bothLists, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone}); err != nil {
		t.Fatal(err)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("an officer clear covers")
	}
	tw.policyVersion(bothLists, 2*time.Second, 0)
	time.Sleep(3 * time.Second)
	tw.policyVersion(bothLists, time.Hour, 0)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("an officer clear expired under a shorter version is not revived by lengthening")
	}
}
