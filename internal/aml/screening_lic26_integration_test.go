//go:build integration

package aml_test

// lic26 (ledger-finance NEW-1 / N-9, security F-1..F-5): real-PostgreSQL tests of the sanctions-class resolution rule, the gate's
// evidence rules for a sanctions-class match, the fingerprint outcome / age rule, the replace audit and lease rules and the parked
// summary used by the payments sweeper. The worker uses the MOCK provider only. All values are TEST FIXTURE VALUES.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// flagged makes the worker flag the person with the given mock marker and returns the needs_officer request.
func (tw *trigWorld) flagged(marker string) (player, reqView) {
	tw.t.Helper()
	p := tw.player()
	tw.setVendorRef(p, marker)
	tw.worker().RunPass(context.Background())
	return p, tw.needsOfficer(p)
}

func (tw *trigWorld) needsOfficer(p player) reqView {
	tw.t.Helper()
	var out reqView
	for _, r := range tw.requests(p.PersonID) {
		if r.Status == "needs_officer" {
			out = r
		}
	}
	if out.ID == "" {
		tw.t.Fatalf("no needs_officer request: %+v", tw.requests(p.PersonID))
	}
	return out
}

// gateDenies calls the screening gate itself (owner, tenant session): it reads screening evidence only, not holds.
func (tw *trigWorld) gateDenies(p player) bool {
	tw.t.Helper()
	var denies bool
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_gate_denies($1, $2, 'withdrawal')`, tw.Tenant, p.PersonID).Scan(&denies)
	}); err != nil {
		tw.t.Fatalf("gate: %v", err)
	}
	return denies
}

func (tw *trigWorld) record(a aml.Actor, p player, key string, trig aml.ScreeningTrigger, in aml.RecordScreeningInput) error {
	in.PersonID, in.IdempotencyKey, in.Trigger = p.PersonID, key, trig
	in.ProviderID, in.SubjectRef, in.ScreenedAt = "officer-manual", strings.Repeat("b", 64), time.Now()
	if len(in.Lists) == 0 {
		in.Lists = []aml.ListType{aml.ListSanctions, aml.ListPEP}
	}
	_, err := tw.svc.RecordScreening(context.Background(), a, in)
	return err
}

func (tw *trigWorld) lift(holdID uuid.UUID, by aml.Actor) error {
	_, err := tw.svc.LiftHold(context.Background(), by, holdID, "fixture_lift")
	return err
}

// F-1 / NEW-1: nothing but a SANCTIONS match that placed a SANCTIONS-class hold resolves a sanctions-class request.
func TestLic26_F1_SanctionsRequestIsResolvedOnlyByASanctionsHold(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	A := tw.q.Officers[0].Actor
	p, req := tw.flagged(aml.MockMarkerSanction + "1")
	trig := aml.ScreeningTrigger(req.Trigger)

	t.Run("records that would not place a sanctions-class hold are refused", func(t *testing.T) {
		for name, in := range map[string]aml.RecordScreeningInput{
			"clear": {Outcome: aml.OutcomeClear, Decision: aml.HoldNone},
			// A confirmed outcome naming only the PEP list is a legal record whose hold class is MANUAL (one officer lifts it).
			"confirmed naming only the PEP list": {Outcome: aml.OutcomeConfirmed, MatchLists: []aml.ListType{aml.ListPEP},
				Decision: aml.HoldAllValueMovement, HoldReasonCode: "fixture_hold"},
			"PEP-only potential match": {Outcome: aml.OutcomePotentialMatch, MatchLists: []aml.ListType{aml.ListPEP},
				Decision: aml.HoldWithdrawalPayout, HoldReasonCode: "fixture_hold"},
		} {
			if err := tw.record(A, p, req.Key, trig, in); err == nil {
				t.Errorf("%s: a sanctions-class request must not be resolved by this record", name)
			}
		}
		// No decision at all (the database CHECK and the function both refuse it; the Go validation is skipped to prove it).
		_, err := tw.svc.RecordScreeningSkippingValidation(ctx, A, aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: req.Key, Trigger: trig,
			ProviderID: "officer-manual", SubjectRef: strings.Repeat("b", 64), Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP},
			Outcome: aml.OutcomePotentialMatch, MatchLists: []aml.ListType{aml.ListSanctions}, ScreenedAt: time.Now(), Decision: aml.HoldNone})
		if err == nil {
			t.Error("a sanctions match recorded with decision none must be refused")
		}
	})
	t.Run("the gate still denies after a fresh-key officer clear, with the gate off too", func(t *testing.T) {
		if err := tw.record(A, p, "manual-"+uuid.NewString(), aml.TriggerManual, aml.RecordScreeningInput{Outcome: aml.OutcomeClear, Decision: aml.HoldNone}); err != nil {
			t.Fatalf("an ordinary officer clear under a fresh key: %v", err)
		}
		if !tw.gateDenies(p) || !tw.heldFor(p, aml.OpWithdrawal) {
			t.Fatal("an unresolved sanctions-class request must still deny after a fresh-key officer clear")
		}
		tw.setTrigger(nil, false, time.Now()) // L1: a policy that no longer gates does not release it
		if !tw.heldFor(p, aml.OpWithdrawal) {
			t.Fatal("the always-on hold for an unresolved sanctions match survives a gate-off policy version")
		}
	})
	t.Run("the proper resolution places the hold", func(t *testing.T) {
		rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(req.ID), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
			Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, MatchLists: []aml.ListType{aml.ListSanctions}, ReasonCode: "match_reviewed"})
		if err != nil || rec.HoldID == nil {
			t.Fatalf("resolve: %+v %v", rec, err)
		}
		if !tw.heldFor(p, aml.OpWithdrawal) {
			t.Fatal("held by the sanctions hold")
		}
	})
}

// NEW-1 (b): for a sanctions-class match the gate counts only a worker clear, an auto-reviewed re-flag or the four-eyes lift of a
// SANCTIONS hold; an officer's clear and a single officer's lift of a manual hold do not.
func TestLic26_NEW1_GateIgnoresOfficerClearAndManualLiftForASanctionsMatch(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	A, B := tw.q.Officers[0].Actor, tw.q.Officers[1].Actor
	resolveSanctions := func(p player, r reqView) uuid.UUID {
		t.Helper()
		rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(r.ID), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
			Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, MatchLists: []aml.ListType{aml.ListSanctions}, ReasonCode: "match_reviewed"})
		if err != nil || rec.HoldID == nil {
			t.Fatalf("resolve: %+v %v", rec, err)
		}
		return *rec.HoldID
	}

	t.Run("an officer clear does not satisfy the gate", func(t *testing.T) {
		p, r := tw.flagged(aml.MockMarkerSanction + "1")
		resolveSanctions(p, r)
		if err := tw.record(A, p, "manual-"+uuid.NewString(), aml.TriggerManual, aml.RecordScreeningInput{Outcome: aml.OutcomeClear, Decision: aml.HoldNone}); err != nil {
			t.Fatal(err)
		}
		if !tw.gateDenies(p) {
			t.Fatal("a single officer's clear must not release a sanctions-class match")
		}
	})
	t.Run("a single officer's lift of a MANUAL hold does not satisfy the gate", func(t *testing.T) {
		p, r := tw.flagged(aml.MockMarkerSanction + "2")
		resolveSanctions(p, r)
		key := "manual-" + uuid.NewString()
		rec, err := tw.svc.RecordScreening(ctx, A, aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: key, Trigger: aml.TriggerManual, ProviderID: "officer-manual",
			SubjectRef: strings.Repeat("c", 64), Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, Outcome: aml.OutcomePotentialMatch,
			MatchLists: []aml.ListType{aml.ListPEP}, ScreenedAt: time.Now(), Decision: aml.HoldWithdrawalPayout, HoldReasonCode: "fixture_pep"})
		if err != nil || rec.HoldID == nil {
			t.Fatalf("manual PEP record: %+v %v", rec, err)
		}
		if err := tw.lift(*rec.HoldID, B); err != nil {
			t.Fatalf("a second officer lifts a manual-class hold: %v", err)
		}
		if !tw.gateDenies(p) {
			t.Fatal("lifting a MANUAL hold must not release a person whose latest match is sanctions-class")
		}
	})
	t.Run("PEP path is unchanged: an officer clear satisfies the gate", func(t *testing.T) {
		p, r := tw.flagged(aml.MockMarkerPEP + "1")
		if _, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(r.ID), aml.ResolveScreeningInput{Outcome: aml.OutcomeClear,
			Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, ReasonCode: "false_positive"}); err != nil {
			t.Fatal(err)
		}
		if tw.gateDenies(p) || tw.heldFor(p, aml.OpWithdrawal) {
			t.Fatal("an officer clear of a PEP-only match still satisfies the gate")
		}
	})
	t.Run("the four-eyes lift of a sanctions hold still satisfies the gate", func(t *testing.T) {
		p, r := tw.flagged(aml.MockMarkerSanction + "3")
		h := resolveSanctions(p, r)
		caseID, err := tw.svc.OpenCase(ctx, A, p.PersonID, aml.CaseKindManual, "false_positive_review", nil)
		if err != nil {
			t.Fatal(err)
		}
		seq, err := tw.svc.Transition(ctx, A, caseID, 1, aml.StatusInReview, "false_positive_review")
		if err != nil {
			t.Fatal(err)
		}
		prop, err := tw.svc.Propose(ctx, A, caseID, seq, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: h, ReasonCode: "false_positive"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tw.svc.Approve(ctx, B, caseID, prop.Seq, prop.ContentHash, "false_positive"); err != nil {
			t.Fatal(err)
		}
		if tw.gateDenies(p) {
			t.Fatal("the proof-bound four-eyes lift of a sanctions hold satisfies the gate")
		}
	})
}

// F-2: an officer-cleared match later UPGRADED (potential -> confirmed) is a new finding, and a review expires.
func TestLic26_F2_UpgradedMatchIsNotAutoReviewed(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	A, B := tw.q.Officers[0].Actor, tw.q.Officers[1].Actor
	p, r := tw.flagged(aml.MockMarkerSanction + "1")
	rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(r.ID), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, MatchLists: []aml.ListType{aml.ListSanctions}, ReasonCode: "match_reviewed"})
	if err != nil || rec.HoldID == nil {
		t.Fatalf("resolve: %+v %v", rec, err)
	}
	caseID, _ := tw.svc.OpenCase(ctx, A, p.PersonID, aml.CaseKindManual, "false_positive_review", nil)
	seq, err := tw.svc.Transition(ctx, A, caseID, 1, aml.StatusInReview, "false_positive_review")
	if err != nil {
		t.Fatal(err)
	}
	prop, err := tw.svc.Propose(ctx, A, caseID, seq, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: *rec.HoldID, ReasonCode: "false_positive"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tw.svc.Approve(ctx, B, caseID, prop.Seq, prop.ContentHash, "false_positive"); err != nil {
		t.Fatal(err)
	}
	if tw.gateDenies(p) {
		t.Fatal("reviewed and cleared by four-eyes")
	}
	reviewed := func() (int, int) {
		var rv, total int
		if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE reviewed_from_request_id IS NOT NULL), count(*) FROM aml_screening_requests
				WHERE person_id = $1 AND status = 'needs_officer'`, p.PersonID).Scan(&rv, &total)
		}); err != nil {
			t.Fatal(err)
		}
		return rv, total
	}
	// Control: the SAME match again is auto-reviewed.
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, p.PersonID, nil, "requeue"); err != nil {
		t.Fatal(err)
	}
	tw.worker().RunPass(ctx)
	if rv, total := reviewed(); rv != 1 || total != 2 {
		t.Fatalf("the same hit must be auto-reviewed: reviewed=%d needs_officer=%d", rv, total)
	}
	// The vendor now says CONFIRMED for the same reference: a different outcome is a new finding.
	tw.setVendorRef(p, aml.MockMarkerConfirmed+"1")
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, p.PersonID, nil, "requeue"); err != nil {
		t.Fatal(err)
	}
	tw.worker().RunPass(ctx)
	if rv, total := reviewed(); rv != 1 || total != 3 {
		t.Fatalf("an upgraded (confirmed) match must NOT be auto-reviewed: reviewed=%d needs_officer=%d", rv, total)
	}
	if !tw.gateDenies(p) || !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the upgraded match holds the person for an officer")
	}
}

// F-2: an officer review carries forward only for FOUR re-screen intervals of its jurisdiction, however many auto-reviews chain.
func TestLic26_F2_ReviewExpiresAfterFourRescreenIntervals(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 3*time.Second, 0) // review valid for 12 s
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	A, B := tw.q.Officers[0].Actor, tw.q.Officers[1].Actor
	p, r := tw.flagged(aml.MockMarkerPEP + "1")
	reviewedAt := time.Now()
	rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(r.ID), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, MatchLists: []aml.ListType{aml.ListPEP}, ReasonCode: "pep_accepted"})
	if err != nil || rec.HoldID == nil {
		t.Fatalf("resolve: %+v %v", rec, err)
	}
	if err := tw.lift(*rec.HoldID, B); err != nil {
		t.Fatal(err)
	}
	counts := func() (int, int) {
		var rv, total int
		if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE reviewed_from_request_id IS NOT NULL), count(*) FROM aml_screening_requests
				WHERE person_id = $1 AND status = 'needs_officer'`, p.PersonID).Scan(&rv, &total)
		}); err != nil {
			t.Fatal(err)
		}
		return rv, total
	}
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, p.PersonID, nil, "requeue"); err != nil {
		t.Fatal(err)
	}
	tw.worker().RunPass(ctx)
	if rv, total := counts(); rv != 1 || total != 2 {
		t.Fatalf("inside the review age the same hit is auto-reviewed: reviewed=%d needs_officer=%d", rv, total)
	}
	time.Sleep(time.Until(reviewedAt.Add(13 * time.Second)))
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, p.PersonID, nil, "requeue"); err != nil {
		t.Fatal(err)
	}
	tw.worker().RunPass(ctx) // chained from the auto-reviewed re-flag, but the ORIGINAL review is now too old
	if rv, total := counts(); rv != 1 || total != 3 {
		t.Fatalf("past the review age the hit must need an officer again: reviewed=%d needs_officer=%d", rv, total)
	}
}

// F-5: the re-screen interval of a lifted-hold review runs from the SCREENING, not from the lift.
func TestLic26_F5_LiftedHoldIntervalRunsFromTheScreening(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 8*time.Second, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	A, B := tw.q.Officers[0].Actor, tw.q.Officers[1].Actor
	p, r := tw.flagged(aml.MockMarkerPEP + "1")
	start := time.Now()
	rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(r.ID), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, MatchLists: []aml.ListType{aml.ListPEP}, ReasonCode: "pep_accepted"})
	if err != nil || rec.HoldID == nil {
		t.Fatalf("resolve: %+v %v", rec, err)
	}
	time.Sleep(time.Until(start.Add(6 * time.Second)))
	if err := tw.lift(*rec.HoldID, B); err != nil {
		t.Fatal(err)
	}
	if tw.gateDenies(p) {
		t.Fatal("inside the interval the lift satisfies the gate")
	}
	time.Sleep(time.Until(start.Add(9500 * time.Millisecond))) // 9.5 s after the screening, 3.5 s after the lift
	if !tw.gateDenies(p) {
		t.Fatal("the interval runs from the screening (8 s), not from the lift: the person is stale")
	}
}

// F-3 / F-4: replace records who queued the replacement, never cancels a live lease and keeps the original created_at.
func TestLic26_F3F4_ReplaceAuditLeaseAndClock(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	A := tw.q.Officers[0].Actor
	p := tw.player()
	rid := uuid.MustParse(tw.requests(p.PersonID)[0].ID)
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_request_claim($1, 10, 5)`, tw.Tenant).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, p.PersonID, &rid, "stale_request"); err == nil {
		t.Fatal("F-4: a request a worker is processing (live lease) must not be replaced")
	}
	time.Sleep(6 * time.Second)
	res, err := tw.svc.ReplaceScreeningRequest(ctx, A, p.PersonID, &rid, "stale_request")
	if err != nil {
		t.Fatalf("an expired lease may be replaced: %v", err)
	}
	var sameClock bool
	var by, person, reason string
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT n.created_at = o.created_at, n.requested_by::text, n.requested_by_person_id::text, n.request_reason_code
			FROM aml_screening_requests n, aml_screening_requests o WHERE n.id = $1 AND o.id = $2`, res.NewRequestID, rid).Scan(&sameClock, &by, &person, &reason)
	}); err != nil || !sameClock || by != A.StaffID.String() || person == "" || reason != "stale_request" {
		t.Fatalf("replacement row: sameClock=%v by=%q person=%q reason=%q err=%v (F-4 keeps created_at, F-3 records the officer)", sameClock, by, person, reason, err)
	}
	// F-3: the "queue a request for a person with none" path records the officer too.
	tw.worker().RunPass(ctx)
	q := tw.player()
	tw.worker().RunPass(ctx)
	res2, err := tw.svc.ReplaceScreeningRequest(ctx, A, q.PersonID, nil, "queue_none")
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT requested_by::text, request_reason_code FROM aml_screening_requests WHERE id = $1`, res2.NewRequestID).Scan(&by, &reason)
	}); err != nil || by != A.StaffID.String() || reason != "queue_none" {
		t.Fatalf("queued row: by=%q reason=%q err=%v", by, reason, err)
	}
	// The record cannot be rewritten afterwards.
	err = tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE aml_screening_requests SET requested_by = NULL WHERE id = $1`, res2.NewRequestID)
		return e
	})
	if err == nil {
		t.Fatal("requested_by must be immutable")
	}
}

// Replace negatives: another tenant, a missing / wrong proof, the officer as the subject.
func TestLic26_Replace_Negatives(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	A := tw.q.Officers[0].Actor
	p := tw.player()
	tw.worker().RunPass(ctx)

	// Cross-tenant: an officer of THIS tenant, a person and a request of ANOTHER tenant.
	other := newTrigWorld(t, 24*time.Hour, 0)
	other.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	op := other.player()
	orid := uuid.MustParse(other.requests(op.PersonID)[0].ID)
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, op.PersonID, nil, "queue"); err == nil {
		t.Fatal("a person of another tenant must not be given a request")
	}
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, op.PersonID, &orid, "queue"); err == nil {
		t.Fatal("a request of another tenant must not be replaced")
	}
	if n := len(other.requests(op.PersonID)); n != 1 {
		t.Fatalf("the other tenant's queue must be untouched, has %d requests", n)
	}

	// The officer is the SUBJECT: a second account of the person being screened.
	subj := amlfix.NewStaffOfPerson(t, tw.Owner, tw.Tenant, p.PersonID)
	amlfix.GrantOfficer(t, tw.Owner, tw.Tenant, subj)
	sa := aml.Actor{StaffID: subj, TenantID: tw.Tenant, Scope: A.Scope}
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, sa, p.PersonID, nil, "queue"); !errors.Is(err, aml.ErrFourEyes) {
		t.Fatalf("an officer must not queue a screening of themselves: %v, want ErrFourEyes", err)
	}
	if n := len(tw.requests(p.PersonID)); n != 1 {
		t.Fatalf("nothing queued for the subject-officer, the person has %d requests", n)
	}
}

// N-9: the sweeper's read reports the parked persons and how many are past the gating policy's alert threshold.
func TestLic26_N9_ParkedOverdue(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second)) // alert after 1 h
	a, b := tw.player(), tw.player()
	decide := func(p player, at time.Time, kyc string) {
		t.Helper()
		if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, `INSERT INTO aml_enforcement_decisions (tenant_id, player_account_id, person_id, operation, kyc_outcome, hold_outcome, public_outcome, allowed, correlation_id, decided_at)
				VALUES ($1, $2, $3, 'withdrawal_payout', $4, 'held', 'unavailable', false, $5, $6)`, tw.Tenant, p.AccountID, p.PersonID, kyc, uuid.New(), at)
			return e
		}); err != nil {
			t.Fatal(err)
		}
	}
	read := func() (int, int) {
		var parked, overdue int
		if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			var e error
			parked, overdue, e = aml.ScreeningParkedOverdue(ctx, tx, tw.Tenant)
			return e
		}); err != nil {
			t.Fatal(err)
		}
		return parked, overdue
	}
	decide(a, time.Now().Add(-3*time.Hour), "passed") // parked for 3 h: past the 1 h threshold
	decide(a, time.Now().Add(-time.Minute), "passed")
	decide(b, time.Now().Add(-time.Minute), "not_required") // parked for a minute only
	if parked, overdue := read(); parked != 2 || overdue != 1 {
		t.Fatalf("parked=%d overdue=%d, want 2 and 1", parked, overdue)
	}
	// A non-parked decision after the run restarts the clock: a's earlier 3 h run no longer counts.
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO aml_enforcement_decisions (tenant_id, player_account_id, person_id, operation, kyc_outcome, hold_outcome, public_outcome, allowed, correlation_id, decided_at)
			VALUES ($1, $2, $3, 'withdrawal_payout', 'passed', 'none', 'passed', true, $4, now() - interval '30 minutes')`, tw.Tenant, a.AccountID, a.PersonID, uuid.New())
		return e
	}); err != nil {
		t.Fatalf("fixture insert of a non-parked decision: %v", err)
	}
	if parked, overdue := read(); parked != 2 || overdue != 0 {
		t.Fatalf("after a non-parked decision the run restarts: parked=%d overdue=%d, want 2 and 0", parked, overdue)
	}
}

// Replace: a missing or wrong proof is refused before anything is read or written (raw SQL in the officer's real session shape).
func TestLic26_Replace_MissingOrWrongProofIsRefused(t *testing.T) {
	w := newWorld(t, "own_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	o0, o1 := q.Officers[0].StaffID, q.Officers[1].StaffID
	p := w.player()
	const sql = `SELECT aml_screening_request_replace($1, $2, NULL, 'queue')`
	hash := actorproof.Digest(actorproof.S(w.Tenant.String()), actorproof.S(p.PersonID.String()), actorproof.SP(""), actorproof.S("queue"))
	f := prooftest.ProcessForger(t, ownerURL(t))
	valid := func(actor uuid.UUID, op, payload string) string {
		return f.Valid(t, actor, "tenant", w.Tenant, op, "new", payload)
	}
	for _, c := range []struct{ name, token, want string }{
		{"no proof", "", "AP001"},
		{"proof of another officer", valid(o1, "aml_screening_request:replace", hash), "AP004"},
		{"proof for another reason code", valid(o0, "aml_screening_request:replace",
			actorproof.Digest(actorproof.S(w.Tenant.String()), actorproof.S(p.PersonID.String()), actorproof.SP(""), actorproof.S("other"))), "AP004"},
		{"proof for another person", valid(o0, "aml_screening_request:replace",
			actorproof.Digest(actorproof.S(w.Tenant.String()), actorproof.S(uuid.NewString()), actorproof.SP(""), actorproof.S("queue"))), "AP004"},
		{"proof for another operation", valid(o0, "aml_case:open", hash), "AP004"},
		// Positive control: a correctly bound proof is accepted, so the refusals above are about the proof. The tenant has no gating
		// trigger policy, hence AT004 AFTER authorisation.
		{"correct proof (control)", valid(o0, "aml_screening_request:replace", hash), "AT004"},
	} {
		err := rawCall(w, o0, c.token, sql, w.Tenant, p.PersonID)
		if pgCode(err) != c.want {
			t.Errorf("%s: want %s, got %v", c.name, c.want, err)
		}
	}
}
