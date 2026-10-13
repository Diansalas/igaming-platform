//go:build integration

package aml_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// These tests reach the SECOND layer of ADR 0116 s3 (the table triggers and the quorum/authorise primitives) directly, as the table
// OWNER with a forged-but-consistent session. The definer functions normally compute the safe values themselves, so the guards are
// unreachable through the API; they exist for the day a future function, a bug, or an owner-level SQL session writes a row by hand
// (THREAT-MODEL-ARBITRARY-SQL-1). Each case names the guard it pins and the code it must raise.

// ownerAs runs fn as the table owner in a tenant session whose principal is staff (the shape aml_authorize / the guards resolve).
func (c *caseWorld) ownerAs(staff uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return c.Owner.WithTenant(bg, c.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, staff.String()); err != nil {
			return err
		}
		return fn(ctx, tx)
	})
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if got := pgCode(err); got != code {
		t.Fatalf("%s: want SQLSTATE %s, got %q (%v)", what, code, got, err)
	}
}

func TestGuards_DirectInserts(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A, B := c.Q.Officers[0], c.Q.Officers[1]
	caseID := c.open(t)

	t.Run("hold owner scope is forced from the tenant's licensing model", func(t *testing.T) {
		err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_holds (tenant_id, owner_scope, person_id, hold_scope, reason_class, reason_code, placed_by, placed_by_person_id, placed_by_scope)
				VALUES ($1, 'platform', $2, 'all_value_movement', 'manual', 't', $3, $4, 'tenant')`, c.Tenant, c.P.PersonID, A.StaffID, A.PersonID)
			return err
		})
		wantCode(t, "wrong hold owner scope", err, "AM002")
	})
	t.Run("hold actor columns must be the proven session actor", func(t *testing.T) {
		err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_holds (tenant_id, owner_scope, person_id, hold_scope, reason_class, reason_code, placed_by, placed_by_person_id, placed_by_scope)
				VALUES ($1, 'tenant', $2, 'all_value_movement', 'manual', 't', $3, $4, 'tenant')`, c.Tenant, c.P.PersonID, B.StaffID, B.PersonID)
			return err
		})
		wantCode(t, "placed_by is not the session actor", err, "AM001")
	})
	t.Run("case owner scope is forced", func(t *testing.T) {
		err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_cases (tenant_id, owner_scope, person_id, case_kind, opened_by, opened_by_person_id, opened_by_scope)
				VALUES ($1, 'platform', $2, 'manual', $3, $4, 'tenant')`, c.Tenant, c.P.PersonID, A.StaffID, A.PersonID)
			return err
		})
		wantCode(t, "wrong case owner scope", err, "AM002")
	})
	t.Run("note owner scope must be the case's", func(t *testing.T) {
		err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_case_notes (tenant_id, owner_scope, case_id, body, lint_version, created_by, created_by_person_id, created_by_scope)
				VALUES ($1, 'platform', $2, 'x', 'aml-note-lint-v1', $3, $4, 'tenant')`, c.Tenant, caseID, A.StaffID, A.PersonID)
			return err
		})
		wantCode(t, "wrong note owner scope", err, "AM002")
	})

	ev := func(owner string, seq int, typ, from, to string) func(ctx context.Context, tx pgx.Tx) error {
		return func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, from_status, to_status, reason_code, actor_id, actor_person_id, actor_scope)
				VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), $7, 't', $8, $9, 'tenant')`, c.Tenant, owner, caseID, seq, typ, from, to, A.StaffID, A.PersonID)
			return err
		}
	}
	t.Run("event owner scope must be the case's", func(t *testing.T) {
		wantCode(t, "wrong event owner scope", c.ownerAs(A.StaffID, ev("platform", 2, "status_changed", "open", "in_review")), "AM002")
	})
	t.Run("event sequence must be the next one", func(t *testing.T) {
		wantCode(t, "sequence gap", c.ownerAs(A.StaffID, ev("tenant", 3, "status_changed", "open", "in_review")), "AM012")
	})
	t.Run("a case is opened exactly once", func(t *testing.T) {
		wantCode(t, "second opened event", c.ownerAs(A.StaffID, ev("tenant", 2, "opened", "", "open")), "AM010")
	})
	t.Run("a transition must start from the current status", func(t *testing.T) {
		wantCode(t, "from_status is not the current status", c.ownerAs(A.StaffID, ev("tenant", 2, "status_changed", "escalated", "sar_decision_pending")), "AM010")
	})
	t.Run("an unusable request header is evidence-less, not an error", func(t *testing.T) {
		c2 := newCaseWorld(t, "own_licence")
		A2 := c2.Q.Officers[0]
		cid := c2.open(t)
		err := c2.ownerAs(A2.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.aml_ip', '10.0.0.0/8', true), set_config('app.aml_request_id', 'bad id with spaces', true)`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, from_status, to_status, reason_code, actor_id, actor_person_id, actor_scope)
				VALUES ($1, 'tenant', $2, 2, 'status_changed', 'open', 'in_review', 't', $3, $4, 'tenant')`, c2.Tenant, cid, A2.StaffID, A2.PersonID); err != nil {
				return err
			}
			var ipNull, ridNull bool
			if err := tx.QueryRow(ctx, `SELECT ip_address IS NULL, request_id IS NULL FROM aml_case_events WHERE case_id = $1 AND seq = 2`, cid).Scan(&ipNull, &ridNull); err != nil {
				return err
			}
			if !ipNull || !ridNull {
				t.Errorf("a network-shaped address and a malformed request id must be dropped: ipNull=%v ridNull=%v", ipNull, ridNull)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	})
}

func TestGuards_FourEyesTriggerDirect(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A, B := c.Q.Officers[0], c.Q.Officers[1]
	id, seq := c.toSARDecisionPending(t)

	t.Run("a proposal's content hash must match its content", func(t *testing.T) {
		err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, proposal_kind, content_hash, reason_code, actor_id, actor_person_id, actor_scope)
				VALUES ($1, 'tenant', $2, $3, 'proposal', 'close_sar_not_filed', repeat('a', 64), 't', $4, $5, 'tenant')`, c.Tenant, id, seq+1, A.StaffID, A.PersonID)
			return err
		})
		wantCode(t, "forged proposal hash", err, "AM011")
	})

	prop, err := c.Svc.Propose(bg, A.Actor, id, seq, aml.ProposeInput{Kind: aml.ProposeCloseSARNotFiled, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("an approval must bind the proposal's kind as well as its hash", func(t *testing.T) {
		err := c.ownerAs(B.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, from_status, to_status, proposal_kind, proposal_seq, content_hash,
					external_filing_reference, reason_code, actor_id, actor_person_id, actor_scope)
				VALUES ($1, 'tenant', $2, $3, 'approval', 'sar_decision_pending', 'closed_sar_filed', 'close_sar_filed', $4, $5, 'REF-1', 't', $6, $7, 'tenant')`,
				c.Tenant, id, prop.Seq+1, prop.Seq, prop.ContentHash, B.StaffID, B.PersonID)
			return err
		})
		wantCode(t, "approval of a different kind with the same hash", err, "AM012")
	})
	t.Run("the proposer's own account cannot approve (actor)", func(t *testing.T) {
		err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_case_events (tenant_id, owner_scope, case_id, seq, event_type, from_status, to_status, proposal_kind, proposal_seq, content_hash,
					reason_code, actor_id, actor_person_id, actor_scope)
				VALUES ($1, 'tenant', $2, $3, 'approval', 'sar_decision_pending', 'closed_sar_not_filed', 'close_sar_not_filed', $4, $5, 't', $6, $7, 'tenant')`,
				c.Tenant, id, prop.Seq+1, prop.Seq, prop.ContentHash, A.StaffID, A.PersonID)
			return err
		})
		wantCode(t, "self approval", err, "AM011")
	})
}

// A sanctions hold has TWO proposals pending on two cases of the same person: the first approval lifts it; the second is refused
// as STALE (a hold changed since its proposal: the lift counts as a change), never a unique violation leaking from the lifts table.
// (The trigger's own "already lifted" check, AM006, sits behind that invalidation and is only a backstop.)
func TestGuards_SecondApprovalOfAnAlreadyLiftedHold(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A, B := c.A(), c.B()
	hold, err := c.Svc.PlaceHold(bg, A, aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonConfirmedSanctions, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	var cases [2]uuid.UUID
	var props [2]aml.Proposal
	for i := range cases {
		cases[i] = c.open(t)
		s, err := c.Svc.Transition(bg, A, cases[i], 1, aml.StatusInReview, "t")
		if err != nil {
			t.Fatal(err)
		}
		if props[i], err = c.Svc.Propose(bg, A, cases[i], s, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: hold, ReasonCode: "t"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Svc.Approve(bg, B, cases[0], props[0].Seq, props[0].ContentHash, "t"); err != nil {
		t.Fatalf("first approval lifts the hold: %v", err)
	}
	if _, err := c.Svc.Approve(bg, B, cases[1], props[1].Seq, props[1].ContentHash, "t"); !errors.Is(err, aml.ErrStale) {
		t.Fatalf("a second approval of an already-lifted hold must be refused as stale, got %v", err)
	}
}

// The 0142 lift guard, reached directly: an approval event that is not a lift_hold approval of THIS hold authorises nothing, and a
// confirmed-sanctions hold cannot be lifted without one.
func TestGuards_LiftNeedsAMatchingApprovalEvent(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A := c.Q.Officers[0]
	hold, err := c.Svc.PlaceHold(bg, c.A(), aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonConfirmedSanctions, ReasonCode: "t"})
	if err != nil {
		t.Fatal(err)
	}
	caseID := c.open(t)
	lift := func(approval any) error {
		return c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_hold_lifts (hold_id, tenant_id, owner_scope, lifted_by, lifted_by_person_id, lifted_by_scope, reason_code, approval_event_id)
				VALUES ($1, $2, 'tenant', $3, $4, 'tenant', 't', $5)`, hold, c.Tenant, A.StaffID, A.PersonID, approval)
			return err
		})
	}
	wantCode(t, "sanctions lift with no approval", lift(nil), "AM006")
	var opened uuid.UUID
	if err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM aml_case_events WHERE case_id = $1 AND seq = 1`, caseID).Scan(&opened)
	}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "sanctions lift citing an event that is not a lift approval", lift(opened), "AM006")
}

// aml_hold_match answers for the ACTIVE hold only: a lifted hold is gone.
func TestGuards_HoldMatchIgnoresLiftedHolds(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	hold := c.Q.PlaceHold(t, c.Svc, c.P.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	match := func() (*uuid.UUID, error) {
		var id *uuid.UUID
		err := c.ownerAs(c.Q.Officers[0].StaffID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT aml_hold_match($1, $2, $3, 'deposit')`, c.Tenant, c.P.PersonID, c.P.AccountID).Scan(&id)
		})
		return id, err
	}
	if id, err := match(); err != nil || id == nil || *id != hold {
		t.Fatalf("an active hold must match: %v %v", id, err)
	}
	c.Q.LiftHold(t, c.Svc, hold)
	if id, err := match(); err != nil || id != nil {
		t.Fatalf("a lifted hold must not match: %v %v", id, err)
	}
}

// aml_authorize, reached directly: the operation-name shape and the session shape are refused before any grant or proof is looked at.
func TestAuthorize_DirectCalls(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	A := c.Q.Officers[0]
	err := c.ownerAs(A.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM aml_authorize($1, 'not an aml op', 'x', 'y')`, c.Tenant)
		return err
	})
	wantCode(t, "malformed operation name", err, "AM001")
	err = c.Owner.WithPlatformAdmin(bg, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT * FROM aml_authorize($1, 'aml_hold:place', 'new', 'y')`, c.Tenant)
		return err
	})
	wantCode(t, "a plain platform session may not use AML functions", err, "AM001")
}

// Quorum: distinct PERSONS in force of the OWNER's grantee scope. Three grants held by one human, an expired grant, and a grant of
// the other scope are each not a second officer.
func TestQuorum_CountsDistinctInForceOfTheOwnerScope(t *testing.T) {
	place := func(c *caseWorld, by aml.Actor) error {
		_, err := c.Svc.PlaceHold(bg, by, aml.PlaceHoldInput{PersonID: c.P.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"})
		return err
	}
	newOne := func(t *testing.T) (*caseWorld, amlfix.Officer) {
		w := newWorld(t, "own_licence")
		c := &caseWorld{amlWorld: w, Svc: amlfix.Service(t, w.RT), P: w.player()}
		o := amlfix.NewOfficer(t, w.Owner, w.Tenant)
		c.Q = &amlfix.Quorum{Tenant: w.Tenant, OwnerScope: "tenant", Officers: []amlfix.Officer{o}}
		return c, o
	}
	t.Run("two accounts of one Person are one officer", func(t *testing.T) {
		c, o := newOne(t)
		second := amlfix.NewStaffOfPerson(t, c.Owner, c.Tenant, o.PersonID)
		amlfix.GrantOfficer(t, c.Owner, c.Tenant, second)
		if err := place(c, o.Actor); !errors.Is(err, aml.ErrQuorum) {
			t.Fatalf("one Person with two grants is not a quorum: %v", err)
		}
	})
	t.Run("an expired grant is not an officer", func(t *testing.T) {
		c, o := newOne(t)
		staff, _ := amlfix.NewStaff(t, c.Owner, c.Tenant)
		amlfix.GrantOfficerUntil(t, c.Owner, c.Tenant, staff, "tenant", time.Now().Add(4*time.Second))
		if err := place(c, o.Actor); err != nil {
			t.Fatalf("while the second grant is in force there is a quorum: %v", err)
		}
		time.Sleep(5 * time.Second)
		c.P = c.player()
		if err := place(c, o.Actor); !errors.Is(err, aml.ErrQuorum) {
			t.Fatalf("an expired grant must stop counting: %v", err)
		}
	})
	t.Run("a platform-scope grant is not a tenant-owner officer", func(t *testing.T) {
		c, o := newOne(t)
		staff, _ := amlfix.NewPlatformStaff(t, c.Owner)
		amlfix.GrantOfficerAs(t, c.Owner, c.Tenant, staff, "platform")
		if err := place(c, o.Actor); !errors.Is(err, aml.ErrQuorum) {
			t.Fatalf("a platform grant must not count toward a tenant-owned scope: %v", err)
		}
	})
}

// A platform officer acting in a tenant-owned tenant reaches the tenant-owned rows too (ADR 0116 s3.1 scope matrix).
func TestScope_PlatformActingReachesTenantOwnedCases(t *testing.T) {
	c := newCaseWorld(t, "own_licence")
	caseID := c.open(t)
	staff, person := amlfix.NewPlatformStaff(t, c.Owner)
	amlfix.GrantOfficerAs(t, c.Owner, c.Tenant, staff, "platform")
	pa := aml.Actor{StaffID: staff, TenantID: c.Tenant, Scope: aml.ScopePlatformActing}
	_ = person
	cs, err := c.Svc.ReadCase(bg, pa, caseID)
	if err != nil || cs.ID != caseID {
		t.Fatalf("a platform officer acting in the tenant reads its tenant-owned case: %+v %v", cs, err)
	}
}
