//go:build integration

package aml_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

func pgCode(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// holdActive calls the enforcement read as the RUNTIME role in a plain tenant session (the shape a gate runs in).
func (w *amlWorld) holdActive(person, account uuid.UUID, op aml.Operation) (bool, error) {
	var held bool
	err := w.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_hold_active($1, $2, $3, $4)`, w.Tenant, person, account, string(op)).Scan(&held)
	})
	return held, err
}

func allOps() []aml.Operation {
	return []aml.Operation{aml.OpDeposit, aml.OpWithdrawal, aml.OpPayout, aml.OpCasinoStake, aml.OpSportsbookStake, aml.OpManualAdjustment}
}

// ADR 0116 s6 / s7: a hold cannot be ACTIVATED for a scope without the officer quorum (two distinct Persons). Lifting is
// never blocked by a lost quorum (it can only release).
func TestPlaceHold_RequiresQuorum_LiftDoesNot(t *testing.T) {
	for _, model := range []string{"under_platform_licence", "own_licence"} {
		t.Run(model, func(t *testing.T) {
			w := newWorld(t, model)
			p := w.player()
			svc := amlfix.Service(t, w.RT)
			q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 1)
			_, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, aml.PlaceHoldInput{PersonID: p.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"})
			if !errors.Is(err, aml.ErrQuorum) {
				t.Fatalf("one officer must not be a quorum: %v", err)
			}
			if held, err := w.holdActive(p.PersonID, p.AccountID, aml.OpDeposit); err != nil || held {
				t.Fatalf("a refused placement must create no hold: held=%v err=%v", held, err)
			}
			q.Officers = append(q.Officers, amlfix.NewOfficer(t, w.Owner, w.Tenant))
			id, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, aml.PlaceHoldInput{PersonID: p.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"})
			if err != nil {
				t.Fatalf("two distinct officers are a quorum: %v", err)
			}
			if held, err := w.holdActive(p.PersonID, p.AccountID, aml.OpDeposit); err != nil || !held {
				t.Fatalf("the placed hold must gate: held=%v err=%v", held, err)
			}
			// Revoke one officer's grant: the quorum is lost, so no NEW hold may be placed, but the existing hold can still be
			// lifted by the remaining officer (a lost quorum never traps a customer), and the revoked officer can no longer act.
			amlfix.RevokeOfficer(t, w.Owner, w.Tenant, q.Officers[1].StaffID)
			p2 := w.player()
			if _, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, aml.PlaceHoldInput{PersonID: p2.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"}); !errors.Is(err, aml.ErrQuorum) {
				t.Fatalf("a lost quorum must block a NEW hold: %v", err)
			}
			if _, err := svc.LiftHold(context.Background(), q.Officers[1].Actor, id, "t"); !errors.Is(err, aml.ErrForbidden) {
				t.Fatalf("a revoked officer must be refused immediately: %v", err)
			}
			if _, err := svc.LiftHold(context.Background(), q.Officers[0].Actor, id, "t"); err != nil {
				t.Fatalf("lift after a lost quorum: %v", err)
			}
		})
	}
}

// What each hold scope gates, per operation (ADR 0116 s5.1, D9): all_value_movement gates every operation;
// withdrawal_and_payout gates withdrawal and payout; ANY hold gates a manual adjustment.
func TestHoldActive_ScopeOperationMatrix(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	svc := amlfix.Service(t, w.RT)
	cases := []struct {
		scope aml.HoldScope
		want  map[aml.Operation]bool
	}{
		{aml.ScopeAllValueMovement, map[aml.Operation]bool{aml.OpDeposit: true, aml.OpWithdrawal: true, aml.OpPayout: true, aml.OpCasinoStake: true, aml.OpSportsbookStake: true, aml.OpManualAdjustment: true}},
		{aml.ScopeWithdrawalAndPayout, map[aml.Operation]bool{aml.OpDeposit: false, aml.OpWithdrawal: true, aml.OpPayout: true, aml.OpCasinoStake: false, aml.OpSportsbookStake: false, aml.OpManualAdjustment: true}},
	}
	for _, c := range cases {
		p := w.player()
		if held, _ := w.holdActive(p.PersonID, p.AccountID, aml.OpDeposit); held {
			t.Fatal("a player with no hold must not be held")
		}
		q.PlaceHold(t, svc, p.PersonID, nil, c.scope, aml.ReasonManual)
		for _, op := range allOps() {
			held, err := w.holdActive(p.PersonID, p.AccountID, op)
			if err != nil {
				t.Fatalf("%s/%s: %v", c.scope, op, err)
			}
			if held != c.want[op] {
				t.Errorf("scope %s op %s: held=%v want %v", c.scope, op, held, c.want[op])
			}
		}
	}
}

// An account-level hold gates that account only; a person-level hold gates every account of the person in the tenant.
func TestHoldActive_AccountVersusPersonLevel(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	svc := amlfix.Service(t, w.RT)
	p := w.player()
	other := w.secondBrandAccount(p.PersonID)
	q.PlaceHold(t, svc, p.PersonID, &p.AccountID, aml.ScopeAllValueMovement, aml.ReasonManual)
	if held, err := w.holdActive(p.PersonID, p.AccountID, aml.OpCasinoStake); err != nil || !held {
		t.Fatalf("the named account must be held: %v %v", held, err)
	}
	if held, err := w.holdActive(p.PersonID, other, aml.OpCasinoStake); err != nil || held {
		t.Fatalf("an account-level hold must not gate the person's other account: %v %v", held, err)
	}
	p2 := w.player()
	p2other := w.secondBrandAccount(p2.PersonID)
	q.PlaceHold(t, svc, p2.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	for _, acct := range []uuid.UUID{p2.AccountID, p2other} {
		if held, err := w.holdActive(p2.PersonID, acct, aml.OpDeposit); err != nil || !held {
			t.Fatalf("a person-level hold must gate every account of the person: %v %v", held, err)
		}
	}
}

// C2: the person must be the named account's person, and a person-level hold needs an account in the tenant.
func TestPlaceHold_C2_PersonBinding(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	svc := amlfix.Service(t, w.RT)
	p1, p2 := w.player(), w.player()
	in := func(person uuid.UUID, acct *uuid.UUID) aml.PlaceHoldInput {
		return aml.PlaceHoldInput{PersonID: person, PlayerAccountID: acct, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"}
	}
	if _, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, in(p1.PersonID, &p2.AccountID)); !errors.Is(err, aml.ErrHoldRefused) {
		t.Fatalf("an account of another person must be refused (C2): %v", err)
	}
	if _, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, in(uuid.New(), nil)); err == nil {
		t.Fatal("a person with no account in the tenant (or no persons row) must be refused")
	}
	// A person that exists but has an account only in ANOTHER tenant.
	w2 := newWorld(t, "under_platform_licence")
	foreign := w2.player()
	if _, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, in(foreign.PersonID, nil)); err == nil {
		t.Fatal("a person with accounts only in another tenant must be refused")
	}
}

// Lifting: single officer for a manual hold; once only; a sanctions hold is refused outside the four-eyes path.
func TestLiftHold_Rules(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	svc := amlfix.Service(t, w.RT)
	p := w.player()
	h := q.PlaceHold(t, svc, p.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	if _, err := svc.LiftHold(context.Background(), q.Officers[1].Actor, h, "t"); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if held, _ := w.holdActive(p.PersonID, p.AccountID, aml.OpDeposit); held {
		t.Fatal("a lifted hold must not gate")
	}
	if _, err := svc.LiftHold(context.Background(), q.Officers[1].Actor, h, "t"); err == nil {
		t.Fatal("a hold lifts exactly once")
	}
	s := q.PlaceHold(t, svc, p.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonConfirmedSanctions)
	if _, err := svc.LiftHold(context.Background(), q.Officers[1].Actor, s, "t"); !errors.Is(err, aml.ErrHoldRefused) {
		t.Fatalf("a confirmed sanctions hold must not be lifted by one officer: %v", err)
	}
	if held, _ := w.holdActive(p.PersonID, p.AccountID, aml.OpDeposit); !held {
		t.Fatal("the sanctions hold must still gate")
	}
	if _, err := svc.LiftHold(context.Background(), q.Officers[1].Actor, uuid.New(), "t"); !errors.Is(err, aml.ErrNotFound) {
		t.Fatalf("an unknown hold is not found: %v", err)
	}
}

// Two officers lifting the same hold at once: exactly one wins (UNIQUE hold_id), nobody sees a half state.
func TestLiftHold_Concurrent_ExactlyOneWins(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	svc := amlfix.Service(t, w.RT)
	p := w.player()
	h := q.PlaceHold(t, svc, p.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := svc.LiftHold(context.Background(), q.Officers[i%2].Actor, h, "t"); err == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("exactly one concurrent lift must succeed, got %d", ok)
	}
}

// Owner-scope rules: a platform officer places holds only in platform-licensed tenants, a tenant officer only in own-licence
// tenants, and a tenant officer cannot touch a platform-owned hold (reported as not found).
func TestHolds_OwnerScopeRules(t *testing.T) {
	wp := newWorld(t, "under_platform_licence")
	wt := newWorld(t, "own_licence")
	qp := amlfix.Bootstrap(t, wp.Owner, wp.Tenant, 2)
	qt := amlfix.Bootstrap(t, wt.Owner, wt.Tenant, 2)
	svcP, svcT := amlfix.Service(t, wp.RT), amlfix.Service(t, wt.RT)
	pp, pt := wp.player(), wt.player()
	hp := qp.PlaceHold(t, svcP, pp.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	ht := qt.PlaceHold(t, svcT, pt.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)

	// A tenant officer of the OWN-licence tenant cannot reach the platform tenant at all.
	if _, err := svcP.LiftHold(context.Background(), qt.Officers[0].Actor, hp, "t"); err == nil {
		t.Fatal("an officer of another tenant must not lift a hold")
	}
	if _, err := svcT.ListHolds(context.Background(), qt.Officers[0].Actor, pt.PersonID); err != nil {
		t.Fatalf("a tenant officer reads its own tenant's holds: %v", err)
	}
	hs, err := svcP.ListHolds(context.Background(), qp.Officers[0].Actor, pp.PersonID)
	if err != nil || len(hs) != 1 || hs[0].ID != hp || hs[0].OwnerScope != "platform" {
		t.Fatalf("platform officer list: %+v %v", hs, err)
	}
	hts, err := svcT.ListHolds(context.Background(), qt.Officers[0].Actor, pt.PersonID)
	if err != nil || len(hts) != 1 || hts[0].ID != ht || hts[0].OwnerScope != "tenant" {
		t.Fatalf("tenant officer list: %+v %v", hts, err)
	}
}

// A staff member WITHOUT an aml_officer grant cannot use any AML function, even with a valid proof for themselves.
func TestNonOfficer_Refused(t *testing.T) {
	w := newWorld(t, "own_licence")
	svc := amlfix.Service(t, w.RT)
	p := w.player()
	staff, _ := amlfix.NewStaff(t, w.Owner, w.Tenant) // compliance staff of the tenant, no grant
	a := aml.Actor{StaffID: staff, TenantID: w.Tenant, Scope: aml.ScopeTenant}
	_, err := svc.PlaceHold(context.Background(), a, aml.PlaceHoldInput{PersonID: p.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"})
	if !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a staff member without the grant must be refused: %v", err)
	}
	if _, err := svc.ListHolds(context.Background(), a, p.PersonID); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("reads are refused too: %v", err)
	}
}

// The aml_officer grant is four-eyes: a tenant admin can REQUEST it for a tenant compliance user but can never decide it
// (only a distinct PLATFORM approver does, R-7), and until it is decided nothing is granted.
func TestOfficerGrant_TenantAdminCannotSelfApprove(t *testing.T) {
	w := newWorld(t, "own_licence")
	admin, _ := amlfix.NewStaff(t, w.Owner, w.Tenant)
	grantee, _ := amlfix.NewStaff(t, w.Owner, w.Tenant)
	var reqID uuid.UUID
	if err := w.Owner.WithPrincipalScope(context.Background(), w.Tenant, admin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, err = requestAMLGrantAsSessionActorID(ctx, tx, w.Tenant, grantee)
		return err
	}); err != nil {
		t.Fatalf("a tenant principal may REQUEST the grant: %v", err)
	}
	// Another tenant principal (a different Person) tries to approve: only a platform session may decide a grant request.
	approver, _ := amlfix.NewStaff(t, w.Owner, w.Tenant)
	err := w.Owner.WithPrincipalScope(context.Background(), w.Tenant, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, w.Tenant, reqID, "approve", "t")
		return err
	})
	if err == nil {
		t.Fatal("a tenant principal must never be able to approve an aml_officer grant")
	}
	// No grant exists: the grantee cannot act.
	svc := amlfix.Service(t, w.RT)
	p := w.player()
	a := aml.Actor{StaffID: grantee, TenantID: w.Tenant, Scope: aml.ScopeTenant}
	if _, err := svc.ListHolds(context.Background(), a, p.PersonID); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("an undecided request grants nothing: %v", err)
	}
}

// A suspended officer is not an officer: they cannot act (the session refuses them) and must not count toward the quorum.
func TestPlaceHold_SuspendedOfficerDoesNotCountTowardQuorum(t *testing.T) {
	w := newWorld(t, "own_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	svc := amlfix.Service(t, w.RT)
	if err := w.Owner.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, q.Officers[1].StaffID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p := w.player()
	third := amlfix.NewOfficer(t, w.Owner, w.Tenant)
	q.Officers = append(q.Officers, third)
	// Officers 0 and 2 are active: quorum met. Suspend officer 2 as well: only officer 0 is active, quorum lost.
	if _, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, aml.PlaceHoldInput{PersonID: p.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"}); err != nil {
		t.Fatalf("two ACTIVE officers are a quorum (the suspended one is ignored): %v", err)
	}
	if err := w.Owner.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, third.StaffID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	p2 := w.player()
	if _, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, aml.PlaceHoldInput{PersonID: p2.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"}); !errors.Is(err, aml.ErrQuorum) {
		t.Fatalf("one active officer is not a quorum although three grants are in force: %v", err)
	}
}
