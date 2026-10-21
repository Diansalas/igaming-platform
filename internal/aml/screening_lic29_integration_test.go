//go:build integration

package aml_test

// lic29 (security lic27 F-2/F-3, ledger lic27 N-1/N-2): the keyset-paged parked / overdue read. All values are TEST FIXTURE VALUES.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

func (tw *trigWorld) scan(after *uuid.UUID, limit int) aml.ParkedScan {
	tw.t.Helper()
	var sc aml.ParkedScan
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		sc, e = aml.ScreeningParkedScan(ctx, tx, tw.Tenant, after, limit)
		return e
	}); err != nil {
		tw.t.Fatal(err)
	}
	return sc
}

// Every person is observed exactly once per cycle, in a deterministic order, whatever the batch size.
func TestLic29_ParkedScan_KeysetCycleObservesEveryPersonOnce(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	for i := 0; i < 5; i++ {
		tw.player() // registered BEFORE the gate: no request, denied, gated since the first gating version
	}
	tw.gate(60*time.Second, time.Now().Add(-110*time.Second))
	for _, limit := range []int{1, 2, 3, 5, 100} {
		var after *uuid.UUID
		scanned, overdue, pages := 0, 0, 0
		for {
			sc := tw.scan(after, limit)
			scanned += sc.Scanned
			overdue += sc.Overdue
			pages++
			if pages > 10 {
				t.Fatalf("limit %d: the cycle does not terminate", limit)
			}
			if sc.Next == nil {
				break
			}
			if sc.Scanned != limit {
				t.Fatalf("limit %d: a non-final batch must be full, scanned %d", limit, sc.Scanned)
			}
			after = sc.Next
		}
		if scanned != 5 || overdue != 5 {
			t.Fatalf("limit %d: scanned=%d overdue=%d over %d pages, want 5 and 5", limit, scanned, overdue, pages)
		}
	}
	// A cursor past the end observes nobody and completes the cycle.
	last := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	if sc := tw.scan(&last, 10); sc.Scanned != 0 || sc.Next != nil {
		t.Fatalf("past the end: %+v", sc)
	}
}

// Persons waiting for an OFFICER are counted apart from persons stuck for want of a screening result.
func TestLic29_ParkedScan_OfficerQueueIsCountedApartFromTheDeadWorkerSignal(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.gate(60*time.Second, time.Now().Add(-110*time.Second))
	officer, _ := tw.flagged(aml.MockMarkerPEP + "1") // a match awaiting an officer
	dead := tw.player()                               // a pending request nobody screens
	tw.parkedAt(officer, time.Now().Add(-3*time.Hour))
	tw.parkedAt(dead, time.Now().Add(-3*time.Hour))
	_ = ctx
	sc := tw.scan(nil, 100)
	if sc.Parked != 1 || sc.Overdue != 1 || sc.Officer != 1 || sc.OfficerOverdue != 1 || sc.Scanned != 2 || sc.Next != nil {
		t.Fatalf("%+v, want parked=1 overdue=1 officer=1 officer_overdue=1 scanned=2", sc)
	}
	// The wrapper used by older callers counts the dead-worker signal only.
	if parked, overdue := tw.parkedOverdue(); parked != 1 || overdue != 1 {
		t.Fatalf("parked=%d overdue=%d, want 1 and 1", parked, overdue)
	}
}

func TestLic29_ParkedScan_NoGatingPolicyAndBadArguments(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.player()
	if sc := tw.scan(nil, 10); sc.Scanned != 0 || sc.Parked != 0 || sc.Next != nil {
		t.Fatalf("no gating policy: %+v", sc)
	}
	for _, limit := range []int{0, -1, 5001} {
		err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			_, e := aml.ScreeningParkedScan(ctx, tx, tw.Tenant, nil, limit)
			return e
		})
		if pgCode(err) != "AT001" {
			t.Errorf("limit %d: %v, want AT001", limit, err)
		}
	}
	err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		_, e := aml.ScreeningParkedScan(ctx, tx, uuid.New(), nil, 10)
		return e
	})
	if pgCode(err) != "AT002" {
		t.Errorf("another tenant: %v, want AT002", err)
	}
}

// ---- the tighten-only screening policy author (migration 0189) ----------------------------------------------------------------

func policyCount(t *testing.T, tw *trigWorld) int {
	t.Helper()
	var n int
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_policies WHERE tenant_id = $1 AND licensing_jurisdiction_id = $2`, tw.Tenant, tw.jur).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestLic29_PolicyAuthor_TightensButNeverLoosens(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour) // the latest version: both lists, 24 h, grace 1 h, effective now-1h
	A := tw.q.Officers[0].Actor
	base := func() aml.SetScreeningPolicyInput { return policyInput(tw.jur, time.Now()) }
	before := policyCount(t, tw)

	loosen := map[string]func(in *aml.SetScreeningPolicyInput){
		"a list removed":              func(in *aml.SetScreeningPolicyInput) { in.Lists = []aml.ListType{aml.ListPEP} },
		"a longer re-screen interval": func(in *aml.SetScreeningPolicyInput) { in.RescreenInterval = 25 * time.Hour },
		"a longer backfill grace":     func(in *aml.SetScreeningPolicyInput) { in.BackfillGrace = 2 * time.Hour },
		"an earlier effective time":   func(in *aml.SetScreeningPolicyInput) { in.EffectiveFrom = time.Now().Add(-3 * time.Hour) },
		"a shorter interval but a list": func(in *aml.SetScreeningPolicyInput) {
			in.RescreenInterval = time.Hour
			in.Lists = []aml.ListType{aml.ListSanctions}
		},
	}
	for name, mut := range loosen {
		in := base()
		mut(&in)
		_, err := tw.svc.AuthorScreeningPolicy(ctx, A, in)
		if !errors.Is(err, aml.ErrIllegalTransition) || aml.RefusalCode(err) != "policy_loosening_refused" {
			t.Errorf("%s: %v (code %q), want a refused loosening", name, err, aml.RefusalCode(err))
		}
	}
	if policyCount(t, tw) != before {
		t.Fatal("a refused version must write nothing")
	}
	// An equal version and a stricter one are accepted; the latest version is then the bar for the next.
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, base()); err != nil {
		t.Fatalf("an equal version is not less strict: %v", err)
	}
	strict := base()
	strict.RescreenInterval, strict.BackfillGrace = 12*time.Hour, 30*time.Minute
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, strict); err != nil {
		t.Fatalf("a stricter version: %v", err)
	}
	back := base() // the 24 h interval is now looser than the latest (12 h)
	if _, err := tw.svc.AuthorScreeningPolicy(ctx, A, back); aml.RefusalCode(err) != "policy_loosening_refused" {
		t.Fatalf("returning to the old values is a loosening of the latest version: %v", err)
	}
	// The service-level setter (not routed) is unchanged: it still writes any version.
	loose := base()
	loose.RescreenInterval = 48 * time.Hour
	if _, err := tw.svc.SetScreeningPolicy(ctx, A, loose); err != nil {
		t.Fatalf("the unrouted setter keeps its behaviour: %v", err)
	}
}

func TestLic29_PolicyAuthor_FirstVersionAuthorityAndProof(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	j := w.jurisdiction()
	w.bindLicence(j)
	A := q.Officers[0].Actor
	in := policyInput(j, time.Now())
	// The first version of a jurisdiction has nothing to loosen (any stated values are accepted).
	in.RescreenInterval, in.BackfillGrace = 90*24*time.Hour, 30*24*time.Hour
	if _, err := svc.AuthorScreeningPolicy(ctx, A, in); err != nil {
		t.Fatalf("first version: %v", err)
	}
	// A jurisdiction the tenant does not operate in.
	other := policyInput(uuid.New(), time.Now())
	if _, err := svc.AuthorScreeningPolicy(ctx, A, other); aml.RefusalCode(err) != "tenant_not_in_jurisdiction" || !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("unconfigured jurisdiction: %v", err)
	}
	// Quorum: one officer is not enough (checked after the proof, before any write).
	w2 := newWorld(t, "under_platform_licence")
	solo := amlfix.Bootstrap(t, w2.Owner, w2.Tenant, 1)
	svc2 := amlfix.Service(t, w2.RT)
	j2 := w2.jurisdiction()
	if _, err := svc2.AuthorScreeningPolicy(ctx, solo.Officers[0].Actor, policyInput(j2, time.Now())); !errors.Is(err, aml.ErrQuorum) {
		t.Fatalf("one officer: %v, want ErrQuorum", err)
	}
	// A tenant-scope officer (own licence) cannot author screening configuration.
	w3 := newWorld(t, "own_licence")
	q3 := amlfix.Bootstrap(t, w3.Owner, w3.Tenant, 2)
	svc3 := amlfix.Service(t, w3.RT)
	if _, err := svc3.AuthorScreeningPolicy(ctx, q3.Officers[0].Actor, policyInput(w3.jurisdiction(), time.Now())); !errors.Is(err, aml.ErrForbidden) ||
		aml.RefusalCode(err) != "platform_officer_required" {
		t.Fatalf("tenant-scope officer: %v (code %q)", err, aml.RefusalCode(err))
	}
	// No proof at all, raw SQL: the function authorises before anything else.
	err := w.RT.WithPrincipalScope(ctx, w.Tenant, q.Officers[0].StaffID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT aml_screening_policy_author($1, $2, ARRAY['pep'], 60, 0, now(), 'REF', 'r_1')`, w.Tenant, j)
		return e
	})
	if pgCode(err) == "" || pgCode(err)[:2] != "AP" && pgCode(err) != "CG001" && pgCode(err) != "CG020" {
		t.Fatalf("no proof: %v, want an AP/CG refusal", err)
	}
}

// ---- closed refusal codes through the real service --------------------------------------------------------------------------

func TestLic29_RefusalCodes_ThroughTheRealService(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	A := tw.q.Officers[0].Actor
	rec := func(p player, key string, trig aml.ScreeningTrigger, in aml.RecordScreeningInput) error {
		return tw.record(A, p, key, trig, in)
	}
	check := func(name string, err error, sentinel error, code string) {
		t.Helper()
		if err == nil || !errors.Is(err, sentinel) || aml.RefusalCode(err) != code {
			t.Errorf("%s: %v (code %q), want %v / %q", name, err, aml.RefusalCode(err), sentinel, code)
		}
	}
	// no gating policy yet
	p0 := tw.player()
	_, err := tw.svc.ReplaceScreeningRequest(ctx, A, p0.PersonID, nil, "queue")
	check("no gating policy", err, aml.ErrInvalidInput, "no_gating_policy")

	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	// pending request
	pend := tw.player()
	pr := tw.requests(pend.PersonID)[0]
	_, err = tw.svc.ReplaceScreeningRequest(ctx, A, pend.PersonID, nil, "queue")
	check("person has a pending request", err, aml.ErrIllegalTransition, "person_has_pending_request")
	check("record under a pending request's key", rec(pend, pr.Key, aml.TriggerRegistration, aml.RecordScreeningInput{Outcome: aml.OutcomeClear, Decision: aml.HoldNone}),
		aml.ErrInvalidInput, "request_not_awaiting_officer")
	check("reserved key prefix", rec(pend, "reg:"+uuid.NewString(), aml.TriggerScheduled, aml.RecordScreeningInput{Outcome: aml.OutcomeClear, Decision: aml.HoldNone}),
		aml.ErrInvalidInput, "key_prefix_reserved")
	// live lease
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_request_claim($1, 10, 60)`, tw.Tenant).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	rid := uuid.MustParse(pr.ID)
	_, err = tw.svc.ReplaceScreeningRequest(ctx, A, pend.PersonID, &rid, "stale")
	check("live lease", err, aml.ErrIllegalTransition, "request_lease_live")

	// sanctions-class request
	sp, sr := tw.flagged(aml.MockMarkerSanction + "9")
	trig := aml.ScreeningTrigger(sr.Trigger)
	check("sanctions clear", rec(sp, sr.Key, trig, aml.RecordScreeningInput{Outcome: aml.OutcomeClear, Decision: aml.HoldNone}), aml.ErrHoldRefused, "sanctions_clear_refused")
	check("confirmed naming only PEP", rec(sp, sr.Key, trig, aml.RecordScreeningInput{Outcome: aml.OutcomeConfirmed, MatchLists: []aml.ListType{aml.ListPEP},
		Decision: aml.HoldAllValueMovement, HoldReasonCode: "fixture_hold"}), aml.ErrHoldRefused, "sanctions_match_needs_hold")
	check("error result", rec(sp, sr.Key, trig, aml.RecordScreeningInput{Outcome: aml.OutcomeError, ErrorCode: aml.ErrCodeUnknown, Decision: aml.HoldNone}),
		aml.ErrInvalidInput, "error_cannot_resolve")
	check("another person's request", rec(pend, sr.Key, trig, aml.RecordScreeningInput{Outcome: aml.OutcomeClear, Decision: aml.HoldNone}), aml.ErrInvalidInput, "request_key_mismatch")
	sid := uuid.MustParse(sr.ID)
	_, err = tw.svc.ReplaceScreeningRequest(ctx, A, sp.PersonID, &sid, "no")
	check("a match is resolved, not replaced", err, aml.ErrIllegalTransition, "request_not_replaceable")

	// trigger policy refusals the database owns
	in := aml.SetTriggerPolicyInput{JurisdictionID: tw.jur, Events: []aml.TriggerEvent{aml.EventRegistration}, EffectiveFrom: time.Now().Add(-time.Hour),
		LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}
	_, err = tw.svc.SetScreeningTriggerPolicy(ctx, A, in)
	check("effective time in the past", err, aml.ErrInvalidInput, "effective_from_out_of_range")
	in2 := aml.SetTriggerPolicyInput{JurisdictionID: uuid.New(), Events: []aml.TriggerEvent{aml.EventRegistration}, EffectiveFrom: time.Now(),
		LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}
	_, err = tw.svc.SetScreeningTriggerPolicy(ctx, A, in2)
	check("jurisdiction outside the tenant's scope", err, aml.ErrInvalidInput, "jurisdiction_not_in_scope")
}
