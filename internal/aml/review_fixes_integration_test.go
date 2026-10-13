//go:build integration

package aml_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// Security review F1 / F3: a grant in force is not enough. The grantee's LIVE staff row must be active, hold the eligible role now,
// and carry the person the grant was requested for. A demoted or suspended grantee neither acts nor counts toward the quorum, in
// BOTH scopes (the platform half goes through the owner-side liveness helper because a platform row is invisible to the session).

func (c *caseWorld) setStaff(t *testing.T, staff uuid.UUID, set string) {
	t.Helper()
	q := `UPDATE staff_users SET ` + set + ` WHERE id = $1`
	var err error
	if c.Scope == "tenant" {
		err = c.Owner.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error { _, e := tx.Exec(ctx, q, staff); return e })
	} else {
		err = c.Owner.WithPlatformAdmin(bg, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error { _, e := tx.Exec(ctx, q, staff); return e })
	}
	if err != nil {
		t.Fatalf("update staff: %v", err)
	}
}

func (c *caseWorld) quorumNow(t *testing.T) int {
	t.Helper()
	var n int
	if err := c.ownerAs(c.Q.Officers[0].StaffID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_officer_quorum($1, $2)`, c.Tenant, c.Scope).Scan(&n)
	}); err != nil {
		// platform scope: the owner tx above is a tenant session; read the quorum from an acting-shaped call instead
		return -1
	}
	return n
}

func TestEligibility_DemotedTenantOfficersAreNoOfficersAndNoQuorum(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	if n := c.quorumNow(t); n != 3 {
		t.Fatalf("three live officers: quorum %d", n)
	}
	// Demote ONE: still acts? No. The demoted officer is refused; the quorum drops to two.
	c.setStaff(t, c.Q.Officers[2].StaffID, `role = 'support'`)
	if _, err := c.Svc.OpenCase(bg, c.Q.Officers[2].Actor, c.P.PersonID, aml.CaseKindManual, "t", nil); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a demoted officer must be refused: %v", err)
	}
	if n := c.quorumNow(t); n != 2 {
		t.Fatalf("one demoted officer leaves quorum 2, got %d", n)
	}
	// Demote the others: nobody can act and there is no quorum.
	c.setStaff(t, c.Q.Officers[0].StaffID, `role = 'support'`)
	c.setStaff(t, c.Q.Officers[1].StaffID, `role = 'support'`)
	for i, o := range c.Q.Officers {
		if _, err := c.Svc.PlaceHold(bg, o.Actor, aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"}); !errors.Is(err, aml.ErrForbidden) {
			t.Fatalf("officer %d demoted to support: want ErrForbidden, got %v", i, err)
		}
	}
	if n := c.quorumNow(t); n != 0 {
		t.Fatalf("no live officer: quorum %d", n)
	}
}

func TestEligibility_PlatformOfficers_SuspendedOrDemotedAreNoOfficersAndNoQuorum(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	A, B, C := c.Q.Officers[0], c.Q.Officers[1], c.Q.Officers[2]
	place := func(by aml.Actor, p player) error {
		_, err := c.Svc.PlaceHold(bg, by, aml.PlaceHoldInput{PersonID: p.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"})
		return err
	}
	if err := place(A.Actor, c.player()); err != nil {
		t.Fatalf("three active platform officers: %v", err)
	}
	// One active + one suspended + one active -> quorum two (the suspended grantee is invisible to the acting session, so this
	// pins the owner-side liveness helper).
	c.setStaff(t, B.StaffID, `status = 'suspended'`)
	if err := place(A.Actor, c.player()); err != nil {
		t.Fatalf("A and C remain: %v", err)
	}
	if _, err := c.Svc.OpenCase(bg, B.Actor, c.P.PersonID, aml.CaseKindManual, "t", nil); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a suspended platform officer must be refused: %v", err)
	}
	// Suspend C as well: only A is live, so NO new hold can be placed.
	c.setStaff(t, C.StaffID, `status = 'suspended'`)
	if err := place(A.Actor, c.player()); !errors.Is(err, aml.ErrQuorum) {
		t.Fatalf("one live platform officer is not a quorum: %v", err)
	}
	// Re-activate B: quorum two again.
	c.setStaff(t, B.StaffID, `status = 'active'`)
	if err := place(A.Actor, c.player()); err != nil {
		t.Fatalf("A and B live: %v", err)
	}
}

// A confirmed-sanctions hold is liftable only by the proof-bound four-eyes approval, and a SUSPENDED platform officer is not a valid
// approver, so the rule is intact when one of the quorum is suspended.
func TestEligibility_SanctionsLiftNeedsTwoLiveOfficers(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	A, B, C := c.Q.Officers[0], c.Q.Officers[1], c.Q.Officers[2]
	hold, err := c.Svc.PlaceHold(bg, A.Actor, aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonConfirmedSanctions, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	id := c.open(t)
	s, err := c.Svc.Transition(bg, A.Actor, id, 1, aml.StatusInReview, "t")
	if err != nil {
		t.Fatal(err)
	}
	prop, err := c.Svc.Propose(bg, A.Actor, id, s, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: hold, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	c.setStaff(t, B.StaffID, `status = 'suspended'`)
	if _, err := c.Svc.Approve(bg, B.Actor, id, prop.Seq, prop.ContentHash, "t"); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a suspended approver must be refused: %v", err)
	}
	if _, err := c.Svc.Approve(bg, A.Actor, id, prop.Seq, prop.ContentHash, "t"); !errors.Is(err, aml.ErrFourEyes) {
		t.Fatalf("the proposer cannot approve: %v", err)
	}
	if _, err := c.Svc.Approve(bg, C.Actor, id, prop.Seq, prop.ContentHash, "t"); err != nil {
		t.Fatalf("a second live officer approves: %v", err)
	}
	if held, _ := c.holdActive(c.P.PersonID, c.P.AccountID, aml.OpDeposit); held {
		t.Fatal("the lift took effect")
	}
}

// Security review F2: an officer never acts on a record about their own Person. Every officer function refuses with the
// four-eyes class, and the queue omits their own subject's rows.
func TestConflictOfInterest_EveryOfficerFunctionRefusesOwnSubject(t *testing.T) {
	for _, model := range []string{"own_licence", "under_platform_licence"} {
		t.Run(model, func(t *testing.T) {
			c := newCaseWorld(t, model)
			A := c.A()
			hold := c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
			caseID := c.open(t)
			other := c.w().player()
			otherCase, err := c.Svc.OpenCase(bg, A, other.PersonID, aml.CaseKindManual, "t", nil)
			if err != nil {
				t.Fatal(err)
			}
			// The subject is ALSO an officer (a second account of the same Person).
			subjStaff := amlfix.NewStaffOfPerson(t, c.Owner, c.Tenant, c.P.PersonID)
			amlfix.GrantOfficer(t, c.Owner, c.Tenant, subjStaff)
			S := aml.Actor{StaffID: subjStaff, TenantID: c.Tenant, Scope: A.Scope}
			refused := func(what string, err error) {
				t.Helper()
				if !errors.Is(err, aml.ErrFourEyes) {
					t.Errorf("%s on one's own Person: want ErrFourEyes, got %v", what, err)
				}
			}
			_, err = c.Svc.PlaceHold(bg, S, aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"})
			refused("PlaceHold", err)
			_, err = c.Svc.LiftHold(bg, S, hold, "t")
			refused("LiftHold", err)
			_, err = c.Svc.ListHolds(bg, S, c.P.PersonID)
			refused("ListHolds", err)
			_, err = c.Svc.RaiseAlert(bg, S, c.P.PersonID, aml.SeverityLow, "t")
			refused("RaiseAlert", err)
			_, err = c.Svc.OpenCase(bg, S, c.P.PersonID, aml.CaseKindManual, "t", nil)
			refused("OpenCase", err)
			_, err = c.Svc.ReadCase(bg, S, caseID)
			refused("ReadCase", err)
			_, err = c.Svc.CaseEvents(bg, S, caseID)
			refused("CaseEvents", err)
			_, err = c.Svc.CaseNotes(bg, S, caseID)
			refused("CaseNotes", err)
			_, err = c.Svc.Transition(bg, S, caseID, 1, aml.StatusInReview, "t")
			refused("Transition", err)
			_, err = c.Svc.AddNote(bg, S, caseID, 1, "a note")
			refused("AddNote", err)
			_, err = c.Svc.FlagSAR(bg, S, caseID, 1, "t")
			refused("FlagSAR", err)
			_, err = c.Svc.Propose(bg, S, caseID, 1, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: hold, ReasonCode: "t"})
			refused("Propose", err)
			_, err = c.Svc.Approve(bg, S, caseID, 2, "00000000000000000000000000000000000000000000000000000000000000aa", "t")
			refused("Approve", err)
			// The queue lists the other subject's case and NOT the officer's own.
			items, err := c.Svc.CaseQueue(bg, S, 50)
			if err != nil {
				t.Fatal(err)
			}
			sawOther := false
			for _, it := range items {
				if it.PersonID == c.P.PersonID {
					t.Errorf("the queue shows the officer's own subject: %+v", it)
				}
				if it.CaseID == otherCase {
					sawOther = true
				}
			}
			if !sawOther {
				t.Error("the queue must still show other persons' cases")
			}
			// Nothing was lifted, flagged or written: the hold is still active and the case untouched.
			if held, _ := c.holdActive(c.P.PersonID, c.P.AccountID, aml.OpDeposit); !held {
				t.Error("the conflicted lift took effect")
			}
			if cs := c.read(t, caseID); cs.LastSeq != 1 || cs.SARFlagged {
				t.Errorf("the conflicted writes took effect: %+v", cs)
			}
		})
	}
}

func (c *caseWorld) w() *amlWorld { return c.amlWorld }

// Security review F5: a bare app.acting_tenant_id (no acting principal) binds nothing; the erasure check is not a runtime function.
func TestSessionTenant_BareActingSettingBindsNothing(t *testing.T) {
	c := newCaseWorld(t, "under_platform_licence")
	c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	err := c.RT.WithoutTenant(bg, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true)`, c.Tenant.String()); err != nil {
			return err
		}
		var b bool
		return tx.QueryRow(ctx, `SELECT aml_hold_active($1, $2, $3, 'deposit')`, c.Tenant, c.P.PersonID, c.P.AccountID).Scan(&b)
	})
	if pgCode(err) != "AM026" {
		t.Fatalf("a forged acting tenant without the acting principal must not bind the session: %v", err)
	}
	err = c.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var b bool
		return tx.QueryRow(ctx, `SELECT aml_person_has_records($1, $2)`, c.Tenant, c.P.PersonID).Scan(&b)
	})
	if pgCode(err) != "42501" {
		t.Fatalf("the runtime role must not execute the erasure check yet: %v", err)
	}
}

// Security review F7: a decision row never names another person's hold.
func TestRecordDecision_HoldMustBelongToThePerson(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	hold := c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	other := c.w().player()
	call := func(person player, holdID uuid.UUID) error {
		return c.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			return tx.QueryRow(ctx, `SELECT aml_record_enforcement_decision($1, $2, $3, 'deposit', 'passed', 'held', 'review', false, $4, $5)`,
				c.Tenant, person.AccountID, person.PersonID, holdID, uuid.New()).Scan(&id)
		})
	}
	if err := call(other, hold); pgCode(err) != "AM007" {
		t.Fatalf("another person's hold must be refused: %v", err)
	}
	if err := call(c.P, hold); err != nil {
		t.Fatalf("the person's own hold is recorded: %v", err)
	}
}

// Security review F8: a hold placed after the proposal invalidates the approval by the monotonic change mark even when its
// timestamp is EARLIER than the proposal (a transaction that started before the proposal and committed after it).
func TestFourEyes_BackdatedHoldChangeStillInvalidatesTheProposal(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A, B := c.Q.Officers[0], c.Q.Officers[1]
	hold, err := c.Svc.PlaceHold(bg, A.Actor, aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonConfirmedSanctions, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	id := c.open(t)
	s, err := c.Svc.Transition(bg, A.Actor, id, 1, aml.StatusInReview, "t")
	if err != nil {
		t.Fatal(err)
	}
	prop, err := c.Svc.Propose(bg, A.Actor, id, s, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: hold, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	// A second hold appears with a placed_at a day in the PAST (the sequence, not the clock, orders it after the proposal).
	if err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO aml_holds (tenant_id, owner_scope, person_id, hold_scope, reason_class, reason_code, placed_by, placed_by_person_id, placed_by_scope, placed_at)
			VALUES ($1, 'tenant', $2, 'withdrawal_and_payout', 'manual', 't', $3, $4, 'tenant', now() - interval '1 day')`, c.Tenant, c.P.PersonID, A.StaffID, A.PersonID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Svc.Approve(bg, B.Actor, id, prop.Seq, prop.ContentHash, "t"); !errors.Is(err, aml.ErrStale) {
		t.Fatalf("a hold change since the proposal (even a backdated row) invalidates it: %v", err)
	}
}
