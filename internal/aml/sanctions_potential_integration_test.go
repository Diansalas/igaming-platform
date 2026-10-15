//go:build integration

// ADR 0116 A7 round 3, SR7-10 (migration 0155): a sanctions POTENTIAL match is a four-eyes-lift hold, and the officer who
// recorded a screening result never lifts the hold it placed. Real database, real step (no mocks of the lift path).
package aml_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

// The live repro: officer 1 records the match, officer 1 lifts => refused; officer 2 alone => refused; the proof-bound
// four-eyes pair => allowed; and the recorder cannot be the approver that executes the lift.
func TestSanctionsPotential_LiftIsFourEyesAndNotByTheRecorder(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	j := w.jurisdiction()
	A, B := q.Officers[0].Actor, q.Officers[1].Actor
	if _, err := svc.SetScreeningPolicy(ctx, A, policyInput(j, time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	o := orchestrator(t, svc)
	ps := w.player()
	out, err := o.Screen(ctx, A, w.screeningReq(ps, j, aml.MockMarkerSanction+"r3"))
	if err != nil || out.Outcome != aml.OutcomePotentialMatch || out.Recorded.HoldID == nil || out.CaseID == nil {
		t.Fatalf("potential sanctions match: %+v %v", out, err)
	}
	hold, caseID := *out.Recorded.HoldID, *out.CaseID
	if hs, err := svc.ListHolds(ctx, A, ps.PersonID); err != nil || len(hs) != 1 || hs[0].ReasonClass != aml.ReasonSanctionsPotential {
		t.Fatalf("class: %+v %v", hs, err)
	}
	// Officer 1 (the recorder) lifts: refused. Officer 2 alone: refused. Neither lifted anything.
	for name, by := range map[string]aml.Actor{"the recorder": A, "a second officer alone": B} {
		if _, err := svc.LiftHold(ctx, by, hold, "false_positive"); !errors.Is(err, aml.ErrHoldRefused) {
			t.Errorf("%s: a single-officer lift of a sanctions_potential hold must be refused: %v", name, err)
		}
	}
	if held, _ := w.holdActive(ps.PersonID, ps.AccountID, aml.OpCasinoStake); !held {
		t.Fatal("the hold must still gate")
	}
	// Drive the case to in_review, then the pair. The recorder may PROPOSE but cannot be the one whose approval executes the lift.
	seq, err := svc.Transition(ctx, A, caseID, 1, aml.StatusInReview, "t")
	if err != nil {
		t.Fatal(err)
	}
	prop, err := svc.Propose(ctx, B, caseID, seq, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: hold, ReasonCode: "false_positive"})
	if err != nil {
		t.Fatalf("propose by officer 2: %v", err)
	}
	if _, err := svc.Approve(ctx, A, caseID, prop.Seq, prop.ContentHash, "false_positive"); !errors.Is(err, aml.ErrFourEyes) {
		t.Fatalf("the recorder's approval must not execute the lift: %v", err)
	}
	if held, _ := w.holdActive(ps.PersonID, ps.AccountID, aml.OpCasinoStake); !held {
		t.Fatal("the refused approval must leave the hold in force")
	}
	// The proof-bound pair: officer 1 proposes, officer 2 approves.
	prop2, err := svc.Propose(ctx, A, caseID, prop.Seq, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: hold, ReasonCode: "false_positive"})
	if err != nil {
		t.Fatalf("propose by officer 1: %v", err)
	}
	if _, err := svc.Approve(ctx, B, caseID, prop2.Seq, prop2.ContentHash, "false_positive"); err != nil {
		t.Fatalf("the four-eyes pair must lift it: %v", err)
	}
	for _, op := range allOps() {
		if held, err := w.holdActive(ps.PersonID, ps.AccountID, op); err != nil || held {
			t.Errorf("after the approved lift %s must not be held: %v %v", op, held, err)
		}
	}
}

// Every screening-derived hold, whatever its class, is never lifted by the officer who recorded the result: a PEP hold (manual
// class, single-officer lift) is refused for the recorder (AM011) and allowed for another officer.
func TestScreeningHold_RecorderCannotLiftItWhateverTheClass(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	j := w.jurisdiction()
	A, B := q.Officers[0].Actor, q.Officers[1].Actor
	if _, err := svc.SetScreeningPolicy(ctx, A, policyInput(j, time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	o := orchestrator(t, svc)
	pp := w.player()
	out, err := o.Screen(ctx, A, w.screeningReq(pp, j, aml.MockMarkerPEP+"r3"))
	if err != nil || out.Recorded.HoldID == nil {
		t.Fatalf("pep match: %+v %v", out, err)
	}
	if hs, err := svc.ListHolds(ctx, A, pp.PersonID); err != nil || len(hs) != 1 || hs[0].ReasonClass != aml.ReasonManual {
		t.Fatalf("a PEP hold is manual class: %+v %v", hs, err)
	}
	if _, err := svc.LiftHold(ctx, A, *out.Recorded.HoldID, "false_positive"); !errors.Is(err, aml.ErrFourEyes) {
		t.Fatalf("the recorder lifting its own screening hold must be refused as a four-eyes violation: %v", err)
	}
	if held, _ := w.holdActive(pp.PersonID, pp.AccountID, aml.OpWithdrawal); !held {
		t.Fatal("the refused lift must leave the hold in force")
	}
	if _, err := svc.LiftHold(ctx, B, *out.Recorded.HoldID, "false_positive"); err != nil {
		t.Fatalf("another officer may lift a manual-class screening hold: %v", err)
	}
	// A hold an officer placed BY HAND is unaffected: the placing officer may lift their own manual hold (no screening result).
	pm := w.player()
	h := q.PlaceHold(t, svc, pm.PersonID, nil, aml.ScopeWithdrawalAndPayout, aml.ReasonManual)
	if _, err := svc.LiftHold(ctx, A, h, "t"); err != nil {
		t.Fatalf("a hand-placed hold is not a screening hold: %v", err)
	}
}

// The 0155 lift guard, reached directly (independent of the definer functions): a sanctions_potential hold cannot be lifted
// without an approval event, an approval event is not valid for a manual hold, and the officer who recorded the screening result
// that placed a hold cannot insert its lift, whatever the class.
func TestGuards_SanctionsPotentialAndRecorderRules_InTheDatabase(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A, B := c.Q.Officers[0], c.Q.Officers[1]
	record := func(person uuid.UUID, key string, outcome aml.ScreeningOutcome, list aml.ListType, decision aml.HoldAction, code string) uuid.UUID {
		t.Helper()
		r, err := c.Svc.RecordScreening(bg, A.Actor, aml.RecordScreeningInput{PersonID: person, IdempotencyKey: key, Trigger: aml.TriggerManual, ProviderID: aml.MockProviderID,
			SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{list}, Outcome: outcome, MatchLists: []aml.ListType{list}, ScreenedAt: time.Now(),
			Decision: decision, HoldReasonCode: code})
		if err != nil || r.HoldID == nil {
			t.Fatalf("record %s: %+v %v", key, r, err)
		}
		return *r.HoldID
	}
	lift := func(by amlfix.Officer, hold uuid.UUID, approval any) error {
		return c.ownerAs(by.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_hold_lifts (hold_id, tenant_id, owner_scope, lifted_by, lifted_by_person_id, lifted_by_scope, reason_code, approval_event_id)
				VALUES ($1, $2, 'tenant', $3, $4, 'tenant', 't', $5)`, hold, c.Tenant, by.StaffID, by.PersonID, approval)
			return err
		})
	}
	potential := record(c.P.PersonID, "g155-1", aml.OutcomePotentialMatch, aml.ListSanctions, aml.HoldAllValueMovement, "sanctions_potential_match")
	wantCode(t, "sanctions_potential lift with no approval (another officer)", lift(B, potential, nil), "AM006")
	wantCode(t, "sanctions_potential lift with no approval (the recorder)", lift(A, potential, nil), "AM006")
	// A manual-class screening hold: an approval event is not valid for it; the recorder is refused; another officer is allowed.
	p2 := c.w().player()
	pep := record(p2.PersonID, "g155-2", aml.OutcomePotentialMatch, aml.ListPEP, aml.HoldWithdrawalPayout, aml.ReasonPEPMatch)
	wantCode(t, "the recorder lifting its own screening hold", lift(A, pep, nil), "AM011")
	// The rule is about the PERSON, not the account: a second staff account of the recorder's Person is the recorder too.
	second := amlfix.NewStaffOfPerson(t, c.Owner, c.Tenant, A.PersonID)
	amlfix.GrantOfficer(t, c.Owner, c.Tenant, second)
	wantCode(t, "a second account of the recorder's Person", lift(amlfix.Officer{StaffID: second, PersonID: A.PersonID}, pep, nil), "AM011")
	// An approval event is valid for a sanctions lift only: citing the case-opened event on a manual hold is refused (AM006).
	var opened uuid.UUID
	caseID := c.open(t)
	if err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM aml_case_events WHERE case_id = $1 AND seq = 1`, caseID).Scan(&opened)
	}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "an approval event on a manual-class hold", lift(B, pep, opened), "AM006")
	if err := lift(B, pep, nil); err != nil {
		t.Fatalf("another officer may lift a manual-class screening hold: %v", err)
	}
}

// sanctions_potential is derived from a screening record only: PlaceHold (the officer path) refuses it.
func TestSanctionsPotential_NotPlaceableByHand(t *testing.T) {
	_, q, svc := platformWorld(t)
	_, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, aml.PlaceHoldInput{PersonID: uuid.New(), Scope: aml.ScopeAllValueMovement,
		ReasonClass: aml.ReasonSanctionsPotential, ReasonCode: "t"})
	if !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("PlaceHold must refuse the screening-derived class: %v", err)
	}
}
