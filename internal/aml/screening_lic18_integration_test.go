//go:build integration

package aml_test

// lic18 (ledger-finance H-1 and the security addendum S1/S2/L1/L2/L3/L7): real-PostgreSQL tests of the hardened automatic
// screening gate. The worker uses the MOCK provider only. All values are TEST FIXTURE VALUES, not legal thresholds.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// H-1 (b): the table itself refuses a gating version that lacks the scheduled event, and a gate without an alert threshold (the
// constraint definitions are read from the catalog: the table accepts writes only through the governed function).
func TestLic18_TableConstraintsRefuseAGateWithoutScheduledOrAlertThreshold(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	var defs []string
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'aml_screening_trigger_policies'::regclass AND contype = 'c'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var d string
			if err := rows.Scan(&d); err != nil {
				return err
			}
			defs = append(defs, d)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	all := strings.Join(defs, "\n")
	for _, want := range []string{"'scheduled'::text = ANY (events)", "gate_withdrawal = (gate_alert_after_seconds IS NOT NULL)"} {
		if !strings.Contains(all, want) {
			t.Errorf("constraint %q is missing from:\n%s", want, all)
		}
	}
}

// H-1 (d) / L3: a new version never re-opens the backfill grace and never discards a clear.
func TestLic18_NewVersionDoesNotResetGraceOrClears(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 2*time.Second)
	existing := tw.player()
	time.Sleep(1200 * time.Millisecond)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-500*time.Millisecond))
	if tw.heldFor(existing, aml.OpWithdrawal) {
		t.Fatal("an existing person is inside the grace of the FIRST gating version")
	}
	time.Sleep(2500 * time.Millisecond)
	if !tw.heldFor(existing, aml.OpWithdrawal) {
		t.Fatal("the grace is over: an unscreened existing person is held")
	}
	// A second gating version must not hand out a fresh grace.
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-500*time.Millisecond))
	if !tw.heldFor(existing, aml.OpWithdrawal) {
		t.Fatal("a new version re-opened the grace (L3)")
	}
	// The scheduled backfill clears the person; further versions must not discard that clear.
	tw.worker().RunPass(ctx)
	if tw.heldFor(existing, aml.OpWithdrawal) {
		t.Fatal("the scheduled backfill should have cleared the existing person")
	}
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-500*time.Millisecond))
	if tw.heldFor(existing, aml.OpWithdrawal) {
		t.Fatal("a new version discarded a valid clear (clears count from the FIRST gating version)")
	}
}

// H-1 (d): the policy cannot be dated into the past or far into the future.
func TestLic18_EffectiveFromIsBoundedByTheDatabaseToo(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	tw.heartbeat()
	in := aml.SetTriggerPolicyInput{JurisdictionID: tw.jur, Events: []aml.TriggerEvent{aml.EventRegistration}, EffectiveFrom: time.Now().Add(-time.Hour),
		LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}
	if _, err := tw.svc.SetScreeningTriggerPolicy(context.Background(), tw.q.Officers[0].Actor, in); err == nil {
		t.Fatal("a version in the past must be refused")
	}
	in.EffectiveFrom = time.Now().Add(40 * 24 * time.Hour)
	if _, err := tw.svc.SetScreeningTriggerPolicy(context.Background(), tw.q.Officers[0].Actor, in); err == nil {
		t.Fatal("a version beyond 30 days must be refused")
	}
}

// L1: a match no officer resolved holds the person whatever the policy says now.
func TestLic18_UnresolvedMatchHoldsEvenAfterTheGateIsSwitchedOff(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerSanction+"1")
	tw.worker().RunPass(ctx)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a system-found match must hold")
	}
	tw.setTriggerOwner(nil, false, time.Now().Add(-time.Second)) // the gate is switched off by a newer version (d1 D4: the owner path; the route is tighten-only)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("switching the gate off released an unresolved sanctions match (L1)")
	}
	// Removing the licence binding must not release it either (L2).
	if err := tw.Owner.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = NULL WHERE id = $1`, tw.Tenant)
		return err
	}); err != nil {
		t.Logf("the database refuses to unbind the licence (%v): the L2 scenario cannot be staged, the gate does not depend on it", err)
	} else if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("removing the licence released an unresolved match (L2)")
	}
}

// S1: the request's key is bound to its person and trigger and an error never resolves. A PEP match is used so that, without the
// binding, the officer clear would be ACCEPTED (a sanctions match would be refused for another reason and hide the defect).
func TestLic18_KeyBinding_AndErrorNeverResolves(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p, other := tw.player(), tw.player()
	tw.setVendorRef(p, aml.MockMarkerPEP+"1")
	tw.worker().RunPass(ctx)
	rs := tw.requests(p.PersonID)
	if len(rs) < 1 || rs[0].Status != "needs_officer" {
		t.Fatalf("request: %+v", rs)
	}
	var key string
	for _, r := range rs {
		if r.Status == "needs_officer" {
			key = r.Key
		}
	}
	officer := tw.q.Officers[0].Actor
	rec := func(person uuid.UUID, trigger aml.ScreeningTrigger, out aml.ScreeningOutcome) error {
		in := aml.RecordScreeningInput{PersonID: person, IdempotencyKey: key, Trigger: trigger, ProviderID: aml.MockProviderID,
			SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, Outcome: out, ScreenedAt: time.Now(), Decision: aml.HoldNone}
		if out == aml.OutcomeError {
			in.ErrorCode = aml.ErrCodeUnavailable
		}
		_, err := tw.svc.RecordScreening(ctx, officer, in)
		return err
	}
	if err := rec(other.PersonID, aml.TriggerRegistration, aml.OutcomeClear); err == nil {
		t.Fatal("a key of another person's request must not be usable for a clear (S1)")
	}
	if err := rec(p.PersonID, aml.TriggerManual, aml.OutcomeClear); err == nil {
		t.Fatal("a key of a request of another trigger must be refused (S1)")
	}
	if err := rec(p.PersonID, aml.TriggerRegistration, aml.OutcomeError); err == nil {
		t.Fatal("an error result must never resolve a request (S1)")
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("still held after the refused attempts")
	}
	// The legitimate resolution under the right key, person and trigger works.
	if err := rec(p.PersonID, aml.TriggerRegistration, aml.OutcomeClear); err != nil {
		t.Fatalf("the bound clear: %v", err)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the bound officer clear releases the person")
	}
}

// S1: a sanctions-class match is never cleared by an officer; it is resolved by recording a match (a hold lifted only by four-eyes).
func TestLic18_SanctionsMatchIsNeverClearedByAnOfficer(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerSanction+"1")
	tw.worker().RunPass(ctx)
	var id uuid.UUID
	for _, r := range tw.requests(p.PersonID) {
		if r.Status == "needs_officer" {
			id = uuid.MustParse(r.ID)
		}
	}
	if id == uuid.Nil {
		t.Fatalf("no needs_officer request: %+v", tw.requests(p.PersonID))
	}
	officer := tw.q.Officers[0].Actor
	if _, err := tw.svc.ResolveScreeningRequest(ctx, officer, p.PersonID, id, aml.ResolveScreeningInput{Outcome: aml.OutcomeClear,
		Lists: []aml.ListType{aml.ListSanctions}, ReasonCode: "false_positive"}); err == nil {
		t.Fatal("a single officer must not clear a sanctions match (S1)")
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("still held after the refused clear")
	}
	res, err := tw.svc.ResolveScreeningRequest(ctx, officer, p.PersonID, id, aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: []aml.ListType{aml.ListSanctions}, MatchLists: []aml.ListType{aml.ListSanctions}, ReasonCode: "match_confirmed_by_officer"})
	if err != nil || res.HoldID == nil {
		t.Fatalf("resolve by match: %+v %v", res, err)
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the officer hold keeps the withdrawal held")
	}
	if _, err := tw.svc.ResolveScreeningRequest(ctx, officer, p.PersonID, id, aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: []aml.ListType{aml.ListSanctions}, MatchLists: []aml.ListType{aml.ListSanctions}, ReasonCode: "again"}); !errors.Is(err, aml.ErrIllegalTransition) {
		t.Fatalf("a resolved request cannot be resolved again: %v", err)
	}
}

// H-1 (c): the officer path resolves a failed_terminal request and a PEP match with a clear; the clear is newer than the match.
func TestLic18_OfficerResolvesFailedTerminalAndPEPMatch(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	failed, pep := tw.player(), tw.player()
	tw.setVendorRef(failed, aml.MockMarkerUnavailable)
	tw.setVendorRef(pep, aml.MockMarkerPEP+"1")
	wk := tw.worker()
	wk.Config.MaxAttempts = 1
	for i := 0; i < 3; i++ {
		wk.RunPass(ctx)
		time.Sleep(30 * time.Millisecond)
	}
	officer := tw.q.Officers[0].Actor
	for name, p := range map[string]player{"failed_terminal": failed, "pep needs_officer": pep} {
		if !tw.heldFor(p, aml.OpWithdrawal) {
			t.Fatalf("%s: must be held", name)
		}
		var id string
		for _, r := range tw.requests(p.PersonID) {
			if r.Status == "failed_terminal" || r.Status == "needs_officer" {
				id = r.ID
			}
		}
		if id == "" {
			t.Fatalf("%s: no stuck request: %+v", name, tw.requests(p.PersonID))
		}
		if _, err := tw.svc.ResolveScreeningRequest(ctx, officer, p.PersonID, uuid.MustParse(id), aml.ResolveScreeningInput{Outcome: aml.OutcomeClear, Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, ReasonCode: "officer_checked"}); err != nil {
			t.Fatalf("%s: resolve: %v", name, err)
		}
		if tw.heldFor(p, aml.OpWithdrawal) {
			t.Fatalf("%s: the officer's clear must release the withdrawal gate", name)
		}
	}
}

// L7: a failed_terminal scheduled request does not block the next bucket's re-screen.
func TestLic18_FailedTerminalScheduledRequestIsRequeuedInTheNextBucket(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 3*time.Second, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerUnavailable)
	wk := tw.worker()
	wk.Config.MaxAttempts = 1
	for i := 0; i < 3; i++ {
		wk.RunPass(ctx)
		time.Sleep(30 * time.Millisecond)
	}
	count := func() (n int) {
		for _, r := range tw.requests(p.PersonID) {
			if r.Trigger == "scheduled" {
				n++
			}
		}
		return
	}
	before := count()
	time.Sleep(4 * time.Second)
	wk.RunPass(ctx)
	if after := count(); after <= before {
		t.Fatalf("scheduled requests before %d, after %d: the next bucket must enqueue again", before, after)
	}
}

// H-1 (f): persons held longer than the policy's threshold raise the trapped alert (a count only).
func TestLic18_TrappedPersonRaisesTheAlert(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.heartbeat()
	if _, err := tw.svc.SetScreeningTriggerPolicy(ctx, tw.q.Officers[0].Actor, aml.SetTriggerPolicyInput{JurisdictionID: tw.jur,
		Events: []aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, GateWithdrawal: true, GateAlertAfter: 60 * time.Second,
		EffectiveFrom: time.Now().Add(-110 * time.Second), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}); err != nil {
		t.Fatal(err)
	}
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerUnavailable)
	// An OLD unresolved request: created_at is set explicitly (the fixture stands in for 'two minutes ago').
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO aml_screening_requests (tenant_id, person_id, licensing_jurisdiction_id, trigger, idempotency_key, created_at)
			VALUES ($1, $2, $3, 'scheduled', $4, now() - interval '2 hours')`, tw.Tenant, p.PersonID, tw.jur, "old:"+uuid.NewString())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wk := tw.worker()
	wk.Config.MaxAttempts = 1
	var trapped int
	for i := 0; i < 3; i++ {
		trapped += wk.RunPass(ctx).Trapped
		time.Sleep(30 * time.Millisecond)
	}
	if trapped < 1 {
		t.Fatal("the worker pass must report a trapped person")
	}
	var n int
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'aml.screening_gate_trapped' AND subject_tenant_id = $1
			AND discriminator = 'worker:tenant:' || $1::text AND NOT (attributes ? 'person_id') AND (attributes ->> 'persons')::int >= 1`, tw.Tenant).Scan(&n)
	}); err != nil || n < 1 {
		t.Fatalf("trapped alerts = %d err=%v", n, err)
	}
}

// H-1 (a) / S2: a worker result is accepted only from a provider with a fresh heartbeat, and a clear needs a provider reference.
func TestLic18_FinishRefusesAClearWithoutReferenceOrHeartbeat(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	tw.player()
	var id, token uuid.UUID
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT o_id, o_claim_token FROM aml_screening_request_claim($1, 1, 30)`, tw.Tenant).Scan(&id, &token)
	}); err != nil {
		t.Fatal(err)
	}
	pid := tw.policyID() // d1 D10: a clear names the screening policy version it was made under
	finish := func(provider, ref string) error {
		return tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			var r *string
			if ref != "" {
				r = &ref
			}
			var applied bool
			var status string
			return tx.QueryRow(ctx, `SELECT o_applied, o_status FROM aml_screening_request_finish($1, $2, $3, 'clear', NULL, NULL, $4, $5, $6, now(), 0, NULL, $7)`,
				tw.Tenant, id, token, provider, r, strings.Repeat("c", 64), pid).Scan(&applied, &status)
		})
	}
	tw.heartbeat()
	if err := finish("mock-screening", ""); pgCode(err) != "AT001" {
		t.Fatalf("a clear without a provider reference must be refused (AT001), got %v", err)
	}
	if err := finish("other-provider", "ref-1"); err == nil {
		t.Fatal("a clear from a provider with no heartbeat must be refused")
	}
	if err := finish("mock-screening", "ref-1"); err != nil {
		t.Fatalf("a clear with a reference from the reporting provider: %v", err)
	}
}

// H-1 (e): the hold verdict says ScreeningOnly when the gate (and no officer hold) holds the person, so a payout claim PARKS; a real
// officer hold is not ScreeningOnly and still rejects.
func TestLic18_HoldVerdictScreeningOnly_VersusOfficerHold(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	waiting, held := tw.player(), tw.player() // both have a pending registration screening (no worker pass yet)
	verdict := func(p player) (v aml.HoldVerdict) {
		t.Helper()
		if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			var err error
			v, err = aml.EvaluateHold(ctx, tx, tw.Tenant, p.PersonID, p.AccountID, aml.OpPayout)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := verdict(waiting); !v.Held || !v.ScreeningOnly || v.EvalFailed {
		t.Fatalf("an outstanding screening alone: %+v, want Held and ScreeningOnly", v)
	}
	// An officer-placed hold (quorum of two officers) is a finding, not a waiting state.
	if _, err := tw.svc.PlaceHold(ctx, tw.q.Officers[0].Actor, aml.PlaceHoldInput{PersonID: held.PersonID, Scope: aml.ScopeWithdrawalAndPayout, ReasonClass: aml.ReasonManual, ReasonCode: "officer_hold"}); err != nil {
		t.Fatal(err)
	}
	if v := verdict(held); !v.Held || v.ScreeningOnly {
		t.Fatalf("an officer hold: %+v, want Held and NOT ScreeningOnly", v)
	}
	// After the worker clears the waiting person nothing holds them.
	tw.worker().RunPass(ctx)
	if v := verdict(waiting); v.Held || v.ScreeningOnly {
		t.Fatalf("after the clear: %+v", v)
	}
}
