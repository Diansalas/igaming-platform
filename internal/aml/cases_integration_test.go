//go:build integration

package aml_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

var bg = context.Background()

// caseWorld is a world with a two-officer quorum, a service over the RUNTIME pool and one player under investigation.
type caseWorld struct {
	*amlWorld
	Q   *amlfix.Quorum
	Svc *aml.Service
	P   player
}

func newCaseWorld(t *testing.T, model string) *caseWorld {
	t.Helper()
	w := newWorld(t, model)
	return &caseWorld{amlWorld: w, Q: amlfix.Bootstrap(t, w.Owner, w.Tenant, 3), Svc: amlfix.Service(t, w.RT), P: w.player()}
}

func (c *caseWorld) A() aml.Actor { return c.Q.Officers[0].Actor }
func (c *caseWorld) B() aml.Actor { return c.Q.Officers[1].Actor }

func (c *caseWorld) open(t *testing.T) uuid.UUID {
	t.Helper()
	id, err := c.Svc.OpenCase(bg, c.A(), c.P.PersonID, aml.CaseKindManual, "t", nil)
	if err != nil {
		t.Fatalf("open case: %v", err)
	}
	return id
}

func (c *caseWorld) read(t *testing.T, id uuid.UUID) aml.Case {
	t.Helper()
	cs, err := c.Svc.ReadCase(bg, c.A(), id)
	if err != nil {
		t.Fatalf("read case: %v", err)
	}
	return cs
}

// toSARDecisionPending walks a fresh case to sar_decision_pending and returns the case id and its last seq.
func (c *caseWorld) toSARDecisionPending(t *testing.T) (uuid.UUID, int) {
	t.Helper()
	id := c.open(t)
	seq := 1
	for _, to := range []aml.CaseStatus{aml.StatusInReview, aml.StatusEscalated, aml.StatusSARDecisionPending} {
		var err error
		seq, err = c.Svc.Transition(bg, c.A(), id, seq, to, "t")
		if err != nil {
			t.Fatalf("transition to %s: %v", to, err)
		}
	}
	return id, seq
}

func TestCase_Lifecycle_StateMachine(t *testing.T) {
	for _, model := range []string{"under_platform_licence", "own_licence"} {
		t.Run(model, func(t *testing.T) {
			c := newCaseWorld(t, model)
			id := c.open(t)
			cs := c.read(t, id)
			if cs.Status != aml.StatusOpen || cs.LastSeq != 1 || cs.SARFlagged || cs.PersonID != c.P.PersonID || cs.OwnerScope != c.Scope {
				t.Fatalf("a fresh case: %+v", cs)
			}
			// open -> escalated is not a move.
			if _, err := c.Svc.Transition(bg, c.A(), id, 1, aml.StatusEscalated, "t"); !errors.Is(err, aml.ErrIllegalTransition) {
				t.Fatalf("open -> escalated must be illegal: %v", err)
			}
			seq, err := c.Svc.Transition(bg, c.A(), id, 1, aml.StatusInReview, "t")
			if err != nil || seq != 2 {
				t.Fatalf("open -> in_review: %d %v", seq, err)
			}
			// CAS: presenting the old sequence is stale and writes nothing.
			if _, err := c.Svc.Transition(bg, c.B(), id, 1, aml.StatusClosedNoAction, "t"); !errors.Is(err, aml.ErrStale) {
				t.Fatalf("a stale sequence must be refused: %v", err)
			}
			if seq, err = c.Svc.Transition(bg, c.B(), id, 2, aml.StatusClosedNoAction, "t"); err != nil || seq != 3 {
				t.Fatalf("in_review -> closed_no_action: %d %v", seq, err)
			}
			if got := c.read(t, id); got.Status != aml.StatusClosedNoAction || got.LastSeq != 3 {
				t.Fatalf("after closing: %+v", got)
			}
			// Reopen with a reason.
			if seq, err = c.Svc.Transition(bg, c.A(), id, 3, aml.StatusInReview, "reopened"); err != nil || seq != 4 {
				t.Fatalf("reopen: %d %v", seq, err)
			}
			evs, err := c.Svc.CaseEvents(bg, c.A(), id)
			if err != nil || len(evs) != 4 {
				t.Fatalf("events: %d %v", len(evs), err)
			}
			for i, e := range evs {
				if e.Seq != i+1 {
					t.Fatalf("event %d has seq %d: the trail must be gap-free", i, e.Seq)
				}
			}
			if evs[0].Type != "opened" || evs[3].ReasonCode != "reopened" || evs[3].ActorID != c.Q.Officers[0].StaffID {
				t.Fatalf("trail content: %+v", evs)
			}
		})
	}
}

// The case trail records the request metadata (IP, request id) the officer service carried.
func TestCase_Trail_RecordsRequestMetadata(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	a := c.A()
	a.IPAddress, a.RequestID = "203.0.113.9", "req-aml-1"
	id, err := c.Svc.OpenCase(bg, a, c.P.PersonID, aml.CaseKindManual, "t", nil)
	if err != nil {
		t.Fatal(err)
	}
	var ip, rid *string
	if err := c.Owner.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT host(ip_address), request_id FROM aml_case_events WHERE case_id = $1 AND seq = 1`, id).Scan(&ip, &rid)
	}); err != nil {
		t.Fatal(err)
	}
	if ip == nil || *ip != "203.0.113.9" || rid == nil || *rid != "req-aml-1" {
		t.Fatalf("trail metadata: ip=%v request_id=%v", ip, rid)
	}
	// A hostile value is dropped, never an error and never stored.
	a.IPAddress, a.RequestID = "not an ip; DROP", "bad id with spaces"
	if _, err := c.Svc.Transition(bg, a, id, 1, aml.StatusInReview, "t"); err != nil {
		t.Fatalf("hostile metadata must not fail the call: %v", err)
	}
}

func TestCase_Concurrent_OneWinnerPerSequence(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	id := c.open(t)
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.Svc.Transition(bg, c.Q.Officers[i%3].Actor, id, 1, aml.StatusInReview, "t"); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			} else if !errors.Is(err, aml.ErrStale) && !errors.Is(err, aml.ErrIllegalTransition) {
				t.Errorf("a loser must see stale/illegal, got %v", err)
			}
		}(i)
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("exactly one transition may win sequence 1, got %d", won)
	}
	if got := c.read(t, id); got.LastSeq != 2 {
		t.Fatalf("no gap, no fork: %+v", got)
	}
}

func TestCase_SARFlag_IsIrreversible_AndForbidsNoAction(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	id := c.open(t)
	if _, err := c.Svc.FlagSAR(bg, c.A(), id, 1, "t"); !errors.Is(err, aml.ErrIllegalTransition) {
		t.Fatalf("an unworked case cannot be flagged: %v", err)
	}
	seq, _ := c.Svc.Transition(bg, c.A(), id, 1, aml.StatusInReview, "t")
	if seq, err := c.Svc.FlagSAR(bg, c.A(), id, seq, "suspicion"); err != nil || seq != 3 {
		t.Fatalf("flag: %d %v", seq, err)
	}
	if _, err := c.Svc.FlagSAR(bg, c.A(), id, 3, "again"); !errors.Is(err, aml.ErrIllegalTransition) {
		t.Fatalf("the flag is set once: %v", err)
	}
	if _, err := c.Svc.Transition(bg, c.A(), id, 3, aml.StatusClosedNoAction, "t"); !errors.Is(err, aml.ErrIllegalTransition) {
		t.Fatalf("a SAR-flagged case cannot close as no action: %v", err)
	}
	if !c.read(t, id).SARFlagged {
		t.Fatal("the flag must show in the projection")
	}
	// Even a reopen cycle cannot clear it: there is no event that un-flags.
	seq, _ = c.Svc.Transition(bg, c.A(), id, 3, aml.StatusEscalated, "t")
	_, _ = c.Svc.Transition(bg, c.A(), id, seq, aml.StatusSARDecisionPending, "t")
	if !c.read(t, id).SARFlagged {
		t.Fatal("the flag is irreversible")
	}
}

func TestCase_Notes_LintedStoredAndInvalidatePendingProposals(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	id, seq := c.toSARDecisionPending(t)
	// A hostile note is refused BEFORE any proof exists or any database call is made.
	if _, err := c.Svc.AddNote(bg, c.A(), id, seq, "call me on +44 7700 900123"); !errors.Is(err, aml.ErrNoteRefused) {
		t.Fatalf("a phone number must be refused: %v", err)
	}
	if got := c.read(t, id); got.LastSeq != seq {
		t.Fatal("a refused note must write nothing")
	}
	seq2, err := c.Svc.AddNote(bg, c.A(), id, seq, "reviewed the deposit pattern, nothing further")
	if err != nil || seq2 != seq+1 {
		t.Fatalf("note: %d %v", seq2, err)
	}
	notes, err := c.Svc.CaseNotes(bg, c.A(), id)
	if err != nil || len(notes) != 1 || notes[0].LintVersion != aml.LintVersion || !strings.Contains(notes[0].Body, "deposit pattern") {
		t.Fatalf("stored notes: %+v %v", notes, err)
	}
	// A proposal followed by a note: the approval is stale.
	p, err := c.Svc.Propose(bg, c.A(), id, seq2, aml.ProposeInput{Kind: aml.ProposeCloseSARNotFiled, ReasonCode: "t"})
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	if _, err := c.Svc.AddNote(bg, c.B(), id, p.Seq, "second thoughts"); err != nil {
		t.Fatalf("note after proposal: %v", err)
	}
	if _, err := c.Svc.Approve(bg, c.B(), id, p.Seq, p.ContentHash, "t"); !errors.Is(err, aml.ErrStale) {
		t.Fatalf("a note after the proposal must invalidate it: %v", err)
	}
}

func TestFourEyes_SARClosure_FiledAndNotFiled(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	for _, kind := range []aml.ProposalKind{aml.ProposeCloseSARFiled, aml.ProposeCloseSARNotFiled} {
		id, seq := c.toSARDecisionPending(t)
		// A plain transition into a SAR closure is refused: only an approval can do it.
		if _, err := c.Svc.Transition(bg, c.A(), id, seq, aml.StatusClosedSARFiled, "t"); !errors.Is(err, aml.ErrFourEyes) {
			t.Fatalf("a SAR closure by plain transition must be refused: %v", err)
		}
		in := aml.ProposeInput{Kind: kind, ReasonCode: "t"}
		if kind == aml.ProposeCloseSARFiled {
			in.ExternalFilingReference = "FIU-2026/000123"
		}
		p, err := c.Svc.Propose(bg, c.A(), id, seq, in)
		if err != nil {
			t.Fatalf("%s propose: %v", kind, err)
		}
		var ref *string
		if in.ExternalFilingReference != "" {
			ref = &in.ExternalFilingReference
		}
		want := aml.ProposalContentHash(c.Tenant, id, p.Seq, kind, nil, deref(ref), "t", c.Q.Officers[0].StaffID)
		if p.ContentHash != want {
			t.Fatalf("the content hash must be reproducible by the approver: %s vs %s", p.ContentHash, want)
		}
		if got := c.read(t, id); got.Status != aml.StatusSARDecisionPending {
			t.Fatalf("a proposal changes nothing: %+v", got)
		}
		// The proposer cannot approve their own proposal.
		if _, err := c.Svc.Approve(bg, c.A(), id, p.Seq, p.ContentHash, "t"); !errors.Is(err, aml.ErrFourEyes) {
			t.Fatalf("self-approval must be refused: %v", err)
		}
		// A wrong hash is refused (the approver binds to what they saw).
		if _, err := c.Svc.Approve(bg, c.B(), id, p.Seq, strings.Repeat("0", 64), "t"); !errors.Is(err, aml.ErrStale) {
			t.Fatalf("a hash that does not match the proposal must be refused: %v", err)
		}
		n, err := c.Svc.Approve(bg, c.B(), id, p.Seq, p.ContentHash, "t")
		if err != nil || n != p.Seq+1 {
			t.Fatalf("approve: %d %v", n, err)
		}
		wantStatus := aml.StatusClosedSARNotFiled
		if kind == aml.ProposeCloseSARFiled {
			wantStatus = aml.StatusClosedSARFiled
		}
		if got := c.read(t, id); got.Status != wantStatus {
			t.Fatalf("after approval: %+v want %s", got, wantStatus)
		}
		// Replaying the approval fails: the proposal is no longer the last event.
		if _, err := c.Svc.Approve(bg, c.B(), id, p.Seq, p.ContentHash, "t"); !errors.Is(err, aml.ErrStale) {
			t.Fatalf("an approval cannot be replayed: %v", err)
		}
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// A reopen between the proposal and the approval invalidates the approval (seq no longer current).
func TestFourEyes_ApprovalAfterReopen_Refused(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	id, seq := c.toSARDecisionPending(t)
	p, err := c.Svc.Propose(bg, c.A(), id, seq, aml.ProposeInput{Kind: aml.ProposeCloseSARNotFiled, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// Any event after the proposal (here a SAR flag) ends it.
	if _, err := c.Svc.FlagSAR(bg, c.B(), id, p.Seq, "t"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.Approve(bg, c.B(), id, p.Seq, p.ContentHash, "t"); !errors.Is(err, aml.ErrStale) {
		t.Fatalf("approval after a later event: %v", err)
	}
	// A fresh proposal after the intervening event works.
	p2, err := c.Svc.Propose(bg, c.A(), id, p.Seq+1, aml.ProposeInput{Kind: aml.ProposeCloseSARNotFiled, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.Approve(bg, c.B(), id, p.Seq, p.ContentHash, "t"); !errors.Is(err, aml.ErrStale) {
		t.Fatalf("the OLD proposal stays dead: %v", err)
	}
	if _, err := c.Svc.Approve(bg, c.B(), id, p2.Seq, p2.ContentHash, "t"); err != nil {
		t.Fatalf("the fresh proposal can be approved: %v", err)
	}
}

// B2: the approver must differ from the proposer by actor id AND by Person, and neither may be the case subject.
func TestFourEyes_SamePerson_SubjectPerson_Refused(t *testing.T) {
	for _, model := range []string{"under_platform_licence", "own_licence"} {
		t.Run(model, func(t *testing.T) {
			c := newCaseWorld(t, model)
			scope := c.Q.Officers[0].Actor.Scope

			// (a) A second ACCOUNT of the proposer's Person, granted officer: still the same human.
			twin := amlfix.NewStaffOfPerson(t, c.Owner, c.Tenant, c.Q.Officers[0].PersonID)
			amlfix.GrantOfficer(t, c.Owner, c.Tenant, twin)
			twinActor := aml.Actor{StaffID: twin, TenantID: c.Tenant, Scope: scope}
			id, seq := c.toSARDecisionPending(t)
			p, err := c.Svc.Propose(bg, c.A(), id, seq, aml.ProposeInput{Kind: aml.ProposeCloseSARNotFiled, ReasonCode: "t"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.Svc.Approve(bg, twinActor, id, p.Seq, p.ContentHash, "t"); !errors.Is(err, aml.ErrFourEyes) {
				t.Fatalf("a second account of the proposer's Person must be refused: %v", err)
			}

			// (b) The case SUBJECT is also staff and an officer: they cannot approve (or propose on) their own case.
			subjectStaff := amlfix.NewStaffOfPerson(t, c.Owner, c.Tenant, c.P.PersonID)
			amlfix.GrantOfficer(t, c.Owner, c.Tenant, subjectStaff)
			subj := aml.Actor{StaffID: subjectStaff, TenantID: c.Tenant, Scope: scope}
			if _, err := c.Svc.Approve(bg, subj, id, p.Seq, p.ContentHash, "t"); !errors.Is(err, aml.ErrFourEyes) {
				t.Fatalf("the case subject must not approve: %v", err)
			}
			id2, seq2 := c.toSARDecisionPending(t)
			if _, err := c.Svc.Propose(bg, subj, id2, seq2, aml.ProposeInput{Kind: aml.ProposeCloseSARNotFiled, ReasonCode: "t"}); !errors.Is(err, aml.ErrFourEyes) {
				t.Fatalf("the case subject must not propose: %v", err)
			}
			// (c) A proper second officer still can.
			if _, err := c.Svc.Approve(bg, c.B(), id, p.Seq, p.ContentHash, "t"); err != nil {
				t.Fatalf("a distinct officer approves: %v", err)
			}
		})
	}
}

func TestFourEyes_ProposalShapeAndStatusRules(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	id := c.open(t)
	// SAR closure only from sar_decision_pending.
	seq, _ := c.Svc.Transition(bg, c.A(), id, 1, aml.StatusInReview, "t")
	if _, err := c.Svc.Propose(bg, c.A(), id, seq, aml.ProposeInput{Kind: aml.ProposeCloseSARNotFiled, ReasonCode: "t"}); !errors.Is(err, aml.ErrIllegalTransition) {
		t.Fatalf("a SAR closure is proposed from sar_decision_pending only: %v", err)
	}
	// Input shape.
	bad := []aml.ProposeInput{
		{Kind: aml.ProposeCloseSARFiled, ReasonCode: "t"},                                              // filed without a reference
		{Kind: aml.ProposeCloseSARFiled, ExternalFilingReference: "a@b.co", ReasonCode: "t"},           // reference with an at-sign
		{Kind: aml.ProposeCloseSARNotFiled, ExternalFilingReference: "REF-1", ReasonCode: "t"},         // not-filed with a reference
		{Kind: aml.ProposeLiftHold, ReasonCode: "t"},                                                   // lift without a hold
		{Kind: aml.ProposeCloseSARNotFiled, HoldID: uuid.New(), ReasonCode: "t"},                       // close with a hold
		{Kind: "nonsense", ReasonCode: "t"},                                                            // unknown kind
		{Kind: aml.ProposeCloseSARNotFiled, ReasonCode: "Not A Token"},                                 // free-text reason
		{Kind: aml.ProposeCloseSARFiled, ExternalFilingReference: "4111111111111111", ReasonCode: "t"}, // PAN-shaped reference
	}
	for i, in := range bad {
		if _, err := c.Svc.Propose(bg, c.A(), id, seq, in); !errors.Is(err, aml.ErrInvalidInput) {
			t.Errorf("bad proposal %d must be refused as invalid input: %v", i, err)
		}
	}
}

// A confirmed-sanctions hold is lifted only by the four-eyes approval of a case about that person; the approval executes the
// lift in the same transaction. A hold change since the proposal invalidates it.
func TestFourEyes_SanctionsHoldLift(t *testing.T) {
	for _, model := range []string{"under_platform_licence", "own_licence"} {
		t.Run(model, func(t *testing.T) {
			c := newCaseWorld(t, model)
			hold := c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonConfirmedSanctions)
			if held, _ := c.holdActive(c.P.PersonID, c.P.AccountID, aml.OpCasinoStake); !held {
				t.Fatal("the sanctions hold must gate")
			}
			id := c.open(t)
			seq, _ := c.Svc.Transition(bg, c.A(), id, 1, aml.StatusInReview, "t")
			p, err := c.Svc.Propose(bg, c.A(), id, seq, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: hold, ReasonCode: "false_positive"})
			if err != nil {
				t.Fatalf("propose lift: %v", err)
			}
			if held, _ := c.holdActive(c.P.PersonID, c.P.AccountID, aml.OpCasinoStake); !held {
				t.Fatal("a proposal must not lift anything")
			}
			if _, err := c.Svc.Approve(bg, c.A(), id, p.Seq, p.ContentHash, "t"); !errors.Is(err, aml.ErrFourEyes) {
				t.Fatalf("the proposer cannot approve the lift: %v", err)
			}
			if _, err := c.Svc.Approve(bg, c.B(), id, p.Seq, p.ContentHash, "false_positive"); err != nil {
				t.Fatalf("approve lift: %v", err)
			}
			for _, op := range allOps() {
				if held, err := c.holdActive(c.P.PersonID, c.P.AccountID, op); err != nil || held {
					t.Fatalf("after the approved lift %s must not be held: %v %v", op, held, err)
				}
			}
			hs, err := c.Svc.ListHolds(bg, c.A(), c.P.PersonID)
			if err != nil || len(hs) != 1 || !hs[0].Lifted {
				t.Fatalf("hold listing after the lift: %+v %v", hs, err)
			}

			// A hold change between proposal and approval invalidates the proposal.
			p2 := c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonConfirmedSanctions)
			id2 := c.open(t)
			seq2, _ := c.Svc.Transition(bg, c.A(), id2, 1, aml.StatusInReview, "t")
			prop, err := c.Svc.Propose(bg, c.A(), id2, seq2, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: p2, ReasonCode: "false_positive"})
			if err != nil {
				t.Fatal(err)
			}
			c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeWithdrawalAndPayout, aml.ReasonManual)
			if _, err := c.Svc.Approve(bg, c.B(), id2, prop.Seq, prop.ContentHash, "t"); !errors.Is(err, aml.ErrStale) {
				t.Fatalf("a hold placed after the proposal must invalidate it: %v", err)
			}
			if held, _ := c.holdActive(c.P.PersonID, c.P.AccountID, aml.OpDeposit); !held {
				t.Fatal("the invalidated lift must leave the hold in force")
			}
		})
	}
}

// A lift proposal for a hold of ANOTHER person is not found (the hold must belong to the case subject).
func TestFourEyes_LiftProposal_WrongPersonOrAlreadyLifted(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	other := c.player()
	otherHold := c.Q.PlaceHold(t, c.Svc, other.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonConfirmedSanctions)
	id := c.open(t)
	seq, _ := c.Svc.Transition(bg, c.A(), id, 1, aml.StatusInReview, "t")
	if _, err := c.Svc.Propose(bg, c.A(), id, seq, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: otherHold, ReasonCode: "t"}); !errors.Is(err, aml.ErrNotFound) {
		t.Fatalf("a hold of another person is not found: %v", err)
	}
	manual := c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	c.Q.LiftHold(t, c.Svc, manual)
	if _, err := c.Svc.Propose(bg, c.A(), id, seq, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: manual, ReasonCode: "t"}); !errors.Is(err, aml.ErrHoldRefused) {
		t.Fatalf("an already lifted hold: %v", err)
	}
}

func TestAlerts_RaiseAndOpenAlertCase(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	a1, err := c.Svc.RaiseAlert(bg, c.A(), c.P.PersonID, aml.SeverityHigh, "velocity_review")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.OpenCase(bg, c.A(), c.P.PersonID, aml.CaseKindAlert, "t", nil); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("an alert case names an alert: %v", err)
	}
	if _, err := c.Svc.OpenCase(bg, c.A(), c.P.PersonID, aml.CaseKindManual, "t", []uuid.UUID{a1}); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("a manual case links no alert: %v", err)
	}
	other := c.player()
	if _, err := c.Svc.OpenCase(bg, c.A(), other.PersonID, aml.CaseKindAlert, "t", []uuid.UUID{a1}); err == nil {
		t.Fatal("an alert of another person cannot be linked to this person's case")
	}
	id, err := c.Svc.OpenCase(bg, c.A(), c.P.PersonID, aml.CaseKindAlert, "from_alert", []uuid.UUID{a1})
	if err != nil {
		t.Fatalf("alert case: %v", err)
	}
	if cs := c.read(t, id); cs.Kind != aml.CaseKindAlert {
		t.Fatalf("kind: %+v", cs)
	}
	q, err := c.Svc.CaseQueue(bg, c.A(), 10)
	if err != nil || len(q) != 1 || q[0].CaseID != id {
		t.Fatalf("queue: %+v %v", q, err)
	}
	if _, err := c.Svc.CaseQueue(bg, c.A(), 0); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("queue limit: %v", err)
	}
	if _, err := c.Svc.CaseQueue(bg, c.A(), 101); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("queue limit: %v", err)
	}
}

// No tipping-off, and tenant isolation: an officer of another tenant gets NOT FOUND (not forbidden) for a real case id, and a
// tenant-scope officer inside a platform-licensed tenant cannot see, list or work platform-owned data.
func TestCase_Isolation_ForeignTenantAndOwnerScope(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	id := c.open(t)
	hold := c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)

	// A valid officer of ANOTHER tenant, presenting tenant-A ids against its own tenant: not found.
	other := newCaseWorld(t, "under_platform_licence")
	foreign := other.A()
	if _, err := other.Svc.ReadCase(bg, foreign, id); !errors.Is(err, aml.ErrNotFound) {
		t.Fatalf("a foreign tenant's case id must be not found: %v", err)
	}
	if _, err := other.Svc.Transition(bg, foreign, id, 1, aml.StatusInReview, "t"); !errors.Is(err, aml.ErrNotFound) {
		t.Fatalf("a foreign tenant's case id cannot be transitioned: %v", err)
	}
	if _, err := other.Svc.LiftHold(bg, foreign, hold, "t"); !errors.Is(err, aml.ErrNotFound) {
		t.Fatalf("a foreign tenant's hold id cannot be lifted: %v", err)
	}
	// An officer cannot name another tenant in the Actor: the session tenant must be the requested tenant.
	crossed := c.A()
	crossed.TenantID = other.Tenant
	if _, err := c.Svc.ReadCase(bg, crossed, id); err == nil {
		t.Fatal("an actor pointed at a tenant it holds no grant in must be refused")
	}

	// A tenant-scope officer of the SAME platform-licensed tenant: the case is platform-owned.
	tstaff, _ := amlfix.NewTenantComplianceStaff(t, c.Owner, c.Tenant)
	amlfix.GrantOfficerAs(t, c.Owner, c.Tenant, tstaff, "tenant")
	tenantOfficer := aml.Actor{StaffID: tstaff, TenantID: c.Tenant, Scope: aml.ScopeTenant}
	if _, err := c.Svc.ReadCase(bg, tenantOfficer, id); !errors.Is(err, aml.ErrNotFound) {
		t.Fatalf("a platform-owned case must be NOT FOUND for a tenant officer: %v", err)
	}
	if evs, err := c.Svc.CaseEvents(bg, tenantOfficer, id); !errors.Is(err, aml.ErrNotFound) || evs != nil {
		t.Fatalf("events of a platform-owned case: %v %v", evs, err)
	}
	if q, err := c.Svc.CaseQueue(bg, tenantOfficer, 50); err != nil || len(q) != 0 {
		t.Fatalf("the tenant officer's queue must not list platform-owned cases: %+v %v", q, err)
	}
	if hs, err := c.Svc.ListHolds(bg, tenantOfficer, c.P.PersonID); err != nil || len(hs) != 0 {
		t.Fatalf("the tenant officer must not see platform-owned holds: %+v %v", hs, err)
	}
	if _, err := c.Svc.Transition(bg, tenantOfficer, id, 1, aml.StatusInReview, "t"); !errors.Is(err, aml.ErrNotFound) {
		t.Fatalf("a tenant officer cannot work a platform-owned case: %v", err)
	}
	if _, err := c.Svc.LiftHold(bg, tenantOfficer, hold, "t"); !errors.Is(err, aml.ErrNotFound) {
		t.Fatalf("a tenant officer cannot lift a platform-owned hold: %v", err)
	}
	if _, err := c.Svc.OpenCase(bg, tenantOfficer, c.P.PersonID, aml.CaseKindManual, "t", nil); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a tenant officer cannot open a case in a platform-licensed tenant: %v", err)
	}
	if _, err := c.Svc.PlaceHold(bg, tenantOfficer, aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"}); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a tenant officer cannot place a hold in a platform-licensed tenant: %v", err)
	}
}

// ADR 0116 s7: the erasure check and the RESTRICT foreign keys.
func TestErasure_FailsClosedAgainstAMLRecords(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	clean := c.player()
	has := func(person uuid.UUID) bool {
		var b bool
		if err := c.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT aml_person_has_records($1, $2)`, c.Tenant, person).Scan(&b)
		}); err != nil {
			t.Fatal(err)
		}
		return b
	}
	if has(c.P.PersonID) || has(clean.PersonID) {
		t.Fatal("no records yet")
	}
	c.open(t)
	if !has(c.P.PersonID) || has(clean.PersonID) {
		t.Fatal("a person with a case has AML records; another does not")
	}
	// A wrong tenant is refused, never silently false.
	if err := c.RT.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var b bool
		return tx.QueryRow(ctx, `SELECT aml_person_has_records($1, $2)`, uuid.New(), c.P.PersonID).Scan(&b)
	}); pgCode(err) != "AM026" {
		t.Fatalf("a foreign tenant must raise AM026: %v", err)
	}
	// The RESTRICT FKs: a hard delete of the person or the account is refused even by the table owner.
	for _, q := range []string{`DELETE FROM player_accounts WHERE id = $1`, `DELETE FROM persons WHERE id = $2`} {
		err := c.Owner.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, q, c.P.AccountID, c.P.PersonID)
			return err
		})
		if err == nil {
			t.Fatalf("%q must be refused while AML records exist", q)
		}
	}
}
