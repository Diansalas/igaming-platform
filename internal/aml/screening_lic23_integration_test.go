//go:build integration

package aml_test

// lic23 (ledger-finance N-1..N-5, security SN-2 / SN-5): real-PostgreSQL tests of the gate, the reviewed-and-cleared path, the
// environment guard and the officer replace. The worker uses the MOCK provider only. All values are TEST FIXTURE VALUES.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// N-1: a gate can always be switched off, even after the jurisdiction left the tenant's licence / bases.
func TestLic23_N1_GateOffVersionAllowedAfterTheJurisdictionLeavesTheTenantsScope(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the gate holds a new person until screened")
	}
	// The tenant's licence is rebound to ANOTHER jurisdiction: the gated jurisdiction is no longer in its current set.
	j2 := tw.jurisdiction()
	tw.bindLicence(j2)
	// A gate-ON version for the old jurisdiction is refused (unchanged rule) ...
	if err := tw.setTriggerErr([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now()); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("a gate for a jurisdiction outside the current scope: %v, want ErrInvalidInput", err)
	}
	// ... and so is a gate-OFF version that still lists events (only the plain switch-off is relieved) ...
	if err := tw.setTriggerErr([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now()); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("a non-gating version with events outside the current scope: %v, want ErrInvalidInput", err)
	}
	// ... but the switch-off itself (no gate, no events) is allowed.
	if err := tw.setTriggerErr(nil, false, time.Now()); err != nil {
		t.Fatalf("the gate-off version must be allowed: %v", err)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("after the switch-off nothing gates the person any more")
	}
	// A jurisdiction that never had a trigger policy gets no such relief.
	j3 := tw.bareJurisdiction()
	in := aml.SetTriggerPolicyInput{JurisdictionID: j3, EffectiveFrom: time.Now(), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}
	if _, err := tw.svc.SetScreeningTriggerPolicy(ctx, tw.q.Officers[0].Actor, in); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("a switch-off for a jurisdiction with no trigger policy: %v, want ErrInvalidInput", err)
	}
}

// N-3: the scheduled enqueue uses the SAME baseline as the gate (the first GATING version).
func TestLic23_N3_ClearedUnderAnEarlierNonGatingVersionIsRequeuedWhenTheGateWouldHold(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, false, time.Now().Add(-time.Second))
	p := tw.player()
	wk := tw.worker()
	wk.RunPass(ctx) // cleared (done) under the non-gating version
	time.Sleep(1200 * time.Millisecond)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-500*time.Millisecond))
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a clear that predates the first gating version does not satisfy the gate")
	}
	wk.RunPass(ctx) // enqueue_due must queue the person again; the worker clears them
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the person must have been re-enqueued and cleared (the enqueue baseline must be the gating one)")
	}
}

// N-5: a screening policy must be in effect at or before the trigger version's effective time.
func TestLic23_N5_ScreeningPolicyMustBeInEffectAtTheTriggerVersion(t *testing.T) {
	ctx := context.Background()
	rgfix.EnsureTestEnvironment(t, os.Getenv("TEST_DATABASE_URL"))
	w, q, svc := platformWorld(t)
	j := w.jurisdiction()
	w.bindLicence(j)
	A := q.Officers[0].Actor
	if _, err := svc.SetScreeningPolicy(ctx, A, policyInput(j, time.Now().Add(20*time.Minute))); err != nil {
		t.Fatal(err)
	}
	in := aml.SetTriggerPolicyInput{JurisdictionID: j, Events: []aml.TriggerEvent{aml.EventRegistration}, EffectiveFrom: time.Now().Add(time.Minute),
		LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}
	if _, err := svc.SetScreeningTriggerPolicy(ctx, A, in); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("a trigger version effective before the screening policy: %v, want ErrInvalidInput", err)
	}
	in.EffectiveFrom = time.Now().Add(25 * time.Minute)
	if _, err := svc.SetScreeningTriggerPolicy(ctx, A, in); err != nil {
		t.Fatalf("a trigger version effective after the screening policy: %v", err)
	}
}

// N-4 / SN-5, Go side: the production binary refuses a gate before any proof is signed.
func TestLic23_SN5_ServiceRefusesAGateInProduction(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, 0)
	svc, err := aml.New(aml.Config{Pool: tw.RT, Environment: "production"})
	if err != nil {
		t.Fatal(err)
	}
	in := aml.SetTriggerPolicyInput{JurisdictionID: tw.jur, Events: []aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, GateWithdrawal: true,
		GateAlertAfter: time.Hour, EffectiveFrom: time.Now(), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}
	tw.heartbeat()
	if _, err := svc.SetScreeningTriggerPolicy(context.Background(), tw.q.Officers[0].Actor, in); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("production gate: %v, want ErrInvalidInput", err)
	}
}

// N-4 / SN-5, database side: the environment guard itself, on scratch databases whose anchor is unrecorded / production / staging.
func scratchThrough181(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	src := filepath.Join("..", "..", "migrations")
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		n, perr := strconv.ParseInt(e.Name()[:4], 10, 64)
		if perr != nil || n > 181 {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(src, e.Name()))
		if err := os.WriteFile(filepath.Join(dir, e.Name()), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pool, err := db.Connect(context.Background(), scratchdb.New(t, prefix), 10, 5_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func envOK(t *testing.T, pool *db.Pool) bool {
	t.Helper()
	var ok bool
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT aml_screening_gate_environment_ok()`).Scan(&ok)
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestLic23_SN5_EnvironmentGuard_UnrecordedAndProductionRefuse_StagingAllows(t *testing.T) {
	ctx := context.Background()
	record := func(pool *db.Pool, env string) {
		t.Helper()
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ($1)`, env)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	a := scratchThrough181(t, "l23ea")
	if envOK(t, a) {
		t.Fatal("an UNRECORDED anchor is production: the guard must refuse")
	}
	record(a, "production")
	if envOK(t, a) {
		t.Fatal("a production anchor must refuse")
	}
	b := scratchThrough181(t, "l23eb")
	record(b, "staging")
	if !envOK(t, b) {
		t.Fatal("a staging anchor allows the MOCK-worker gate")
	}
}

// N-2: the sanctions false-positive remedy. An officer records the match (hold), a DIFFERENT officer pair lifts it by four-eyes, the
// person counts as reviewed-and-cleared from the lift, and the next re-screen flagging the SAME match does not hold them again.
func TestLic23_N2_ReviewedClearedSanctionsMatch_SatisfiesTheGate_AndTheSameHitDoesNotReholdForEver(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 8*time.Second, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerSanction+"1")
	wk := tw.worker()
	wk.RunPass(ctx)
	A, B := tw.q.Officers[0].Actor, tw.q.Officers[1].Actor
	var req string
	for _, r := range tw.requests(p.PersonID) {
		if r.Status == "needs_officer" {
			req = r.ID
		}
	}
	if req == "" {
		t.Fatalf("no needs_officer request: %+v", tw.requests(p.PersonID))
	}
	// A sanctions match cannot be cleared; the officer records it (hold placed).
	rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(req), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, MatchLists: []aml.ListType{aml.ListSanctions}, ReasonCode: "match_reviewed"})
	if err != nil || rec.HoldID == nil {
		t.Fatalf("record: %+v %v", rec, err)
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("held while the hold is in force")
	}
	// The proof-bound four-eyes lift (A proposes, B approves; the recorder A may not be the executing approver).
	caseID, err := tw.svc.OpenCase(ctx, A, p.PersonID, aml.CaseKindManual, "false_positive_review", nil)
	if err != nil {
		t.Fatal(err)
	}
	seq, err := tw.svc.Transition(ctx, A, caseID, 1, aml.StatusInReview, "false_positive_review")
	if err != nil {
		t.Fatal(err)
	}
	prop, err := tw.svc.Propose(ctx, A, caseID, seq, aml.ProposeInput{Kind: aml.ProposeLiftHold, HoldID: *rec.HoldID, ReasonCode: "false_positive"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tw.svc.Approve(ctx, B, caseID, prop.Seq, prop.ContentHash, "false_positive"); err != nil {
		t.Fatalf("four-eyes lift: %v", err)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a reviewed-and-cleared sanctions match (hold lifted by four-eyes) must satisfy the gate")
	}
	// Past the re-screen interval the person is stale and the scheduled re-screen flags the SAME hit again.
	time.Sleep(9 * time.Second)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("stale: held until the re-screen completes")
	}
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the same hit that an officer already reviewed and cleared must not hold the person again for ever")
	}
	var reviewed, total int
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE reviewed_from_request_id IS NOT NULL), count(*) FILTER (WHERE status = 'needs_officer')
			FROM aml_screening_requests WHERE person_id = $1`, p.PersonID).Scan(&reviewed, &total)
	}); err != nil || reviewed != 1 || total != 2 {
		t.Fatalf("auto-reviewed re-flag: reviewed=%d needs_officer=%d err=%v, want 1 and 2", reviewed, total, err)
	}
	var attention int
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'aml.screening_attention' AND subject_tenant_id = $1 AND attributes ->> 'reason' = 'match_review'`, tw.Tenant).Scan(&attention)
	}); err != nil || attention != 1 {
		t.Fatalf("attention alerts = %d err=%v, want only the first match to alert", attention, err)
	}
	// A DIFFERENT hit (another provider-stated identity) is a new finding: it holds the person for an officer.
	tw.setVendorRef(p, aml.MockMarkerSanction+"2")
	time.Sleep(9 * time.Second)
	wk.RunPass(ctx)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a different match must hold the person until an officer reviews it")
	}
}

// N-2: a PEP match an officer records and another officer lifts is reviewed-and-cleared too (the "accepted PEP" remedy).
func TestLic23_N2_PEPMatchRecordedAndLifted_SatisfiesTheGate(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerPEP+"1")
	tw.worker().RunPass(ctx)
	A, B := tw.q.Officers[0].Actor, tw.q.Officers[1].Actor
	var req string
	for _, r := range tw.requests(p.PersonID) {
		if r.Status == "needs_officer" {
			req = r.ID
		}
	}
	rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(req), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, MatchLists: []aml.ListType{aml.ListPEP}, ReasonCode: "pep_accepted"})
	if err != nil || rec.HoldID == nil {
		t.Fatalf("record: %+v %v", rec, err)
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("held while the PEP hold is in force")
	}
	if _, err := tw.svc.LiftHold(ctx, A, *rec.HoldID, "pep_accepted"); err == nil {
		t.Fatal("the recorder must not lift the hold they placed")
	}
	if _, err := tw.svc.LiftHold(ctx, B, *rec.HoldID, "pep_accepted"); err != nil {
		t.Fatalf("a second officer lifts a manual-class hold: %v", err)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("an accepted PEP match (recorded, then lifted by another officer) satisfies the gate")
	}
}

// N-4: the officer's governed replace of a stale pending request, and the queue for a person with none.
func TestLic23_N4_ReplaceStalePendingRequest_AndQueueForAPersonWithNone(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	A := tw.q.Officers[0].Actor
	// A stale CLAIMED request: a dead worker claimed it with a long lease and will never finish it.
	stale := tw.player()
	var staleID string
	for _, r := range tw.requests(stale.PersonID) {
		staleID = r.ID
	}
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var n int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_request_claim($1, 10, 3600)`, tw.Tenant).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	wk := tw.worker()
	wk.RunPass(ctx)
	if !tw.heldFor(stale, aml.OpWithdrawal) {
		t.Fatal("the stale pending request holds the person")
	}
	// Replace: the request is cancelled (audit columns), a replacement is queued and the worker clears it.
	rid := uuid.MustParse(staleID)
	res, err := tw.svc.ReplaceScreeningRequest(ctx, A, stale.PersonID, &rid, "stale_request")
	if err != nil || res.CancelledRequestID == nil || *res.CancelledRequestID != rid {
		t.Fatalf("replace: %+v %v", res, err)
	}
	var st, by, reason string
	var repl *string
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, cancelled_by::text, cancel_reason_code, replaced_by_request_id::text FROM aml_screening_requests WHERE id = $1`, staleID).Scan(&st, &by, &reason, &repl)
	}); err != nil || st != "cancelled" || by != A.StaffID.String() || reason != "stale_request" || repl == nil || *repl != res.NewRequestID.String() {
		t.Fatalf("cancelled row: %q %q %q %v err=%v", st, by, reason, repl, err)
	}
	wk.RunPass(ctx)
	if tw.heldFor(stale, aml.OpWithdrawal) {
		t.Fatal("the replacement was screened and cleared")
	}
	// A cancelled request is final and a request stuck on a match is resolved, not replaced.
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, stale.PersonID, &rid, "again"); err == nil {
		t.Fatal("a cancelled request cannot be replaced again")
	}
	m := tw.player()
	tw.setVendorRef(m, aml.MockMarkerPEP+"1")
	wk.RunPass(ctx)
	for _, r := range tw.requests(m.PersonID) {
		if r.Status == "needs_officer" {
			id := uuid.MustParse(r.ID)
			if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, m.PersonID, &id, "no"); err == nil {
				t.Fatal("a needs_officer request is resolved, never replaced")
			}
		}
	}
	// A person with a pending request cannot be given a second one without naming it; without any request one is queued.
	fresh := tw.player()
	if _, err := tw.svc.ReplaceScreeningRequest(ctx, A, fresh.PersonID, nil, "queue"); err == nil {
		t.Fatal("the person already has a pending request")
	}
	wk.RunPass(ctx) // clears the registration request
	res2, err := tw.svc.ReplaceScreeningRequest(ctx, A, fresh.PersonID, nil, "queue")
	if err != nil || res2.CancelledRequestID != nil {
		t.Fatalf("queue for a person with no pending request: %+v %v", res2, err)
	}
	wk.RunPass(ctx)
	if n := len(tw.requests(fresh.PersonID)); n != 2 {
		t.Fatalf("requests = %d, want the registration one and the queued one", n)
	}
}

// SN-2: a record under the key of a request that is not awaiting an officer, or under a reserved key prefix, is refused.
func TestLic23_SN2_RecordUnderANonWaitingOrReservedKeyIsRefused(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player() // a pending registration request
	rs := tw.requests(p.PersonID)
	if len(rs) != 1 || rs[0].Status != "pending" {
		t.Fatalf("requests: %+v", rs)
	}
	A := tw.q.Officers[0].Actor
	rec := func(key string, trig aml.ScreeningTrigger) error {
		_, err := tw.svc.RecordScreening(ctx, A, aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: key, Trigger: trig, ProviderID: aml.MockProviderID,
			SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone})
		return err
	}
	if err := rec(rs[0].Key, aml.TriggerRegistration); err == nil {
		t.Fatal("a record under the key of a PENDING request would pre-resolve it: must be refused")
	}
	for _, key := range []string{"sched:" + p.PersonID.String() + ":999999", "reg:" + uuid.NewString(), "kyc:" + uuid.NewString(), "repl:" + uuid.NewString()} {
		if err := rec(key, aml.TriggerScheduled); err == nil {
			t.Fatalf("the reserved key %q must be refused", key)
		}
	}
	// A manual key still works.
	if err := rec("manual-"+uuid.NewString(), aml.TriggerManual); err != nil {
		t.Fatalf("an ordinary officer key: %v", err)
	}
}

// N-4: a single officer is not a quorum; the replace is refused before anything is read or written.
func TestLic23_N4_ReplaceNeedsTheOfficerQuorum(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 1)
	svc := amlfix.Service(t, w.RT)
	p := w.player()
	if _, err := svc.ReplaceScreeningRequest(context.Background(), q.Officers[0].Actor, p.PersonID, nil, "queue"); !errors.Is(err, aml.ErrQuorum) {
		t.Fatalf("one officer: %v, want ErrQuorum", err)
	}
}

// N-9: the parked summary counts persons whose LATEST payout decision was a screening-only hold, and is readable without the worker.
func TestLic23_N9_ScreeningParkedSummary(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	a, b, c := tw.player(), tw.player(), tw.player()
	rec := func(p player, hold bool, kycOutcome string) {
		t.Helper()
		var holdID uuid.UUID
		if hold {
			holdID = tw.q.PlaceHold(t, tw.svc, p.PersonID, nil, aml.ScopeWithdrawalAndPayout, aml.ReasonManual)
		}
		if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			return aml.RecordEnforcementDecision(ctx, tx, aml.EnforcementRecord{TenantID: tw.Tenant, PlayerAccountID: p.AccountID, PersonID: p.PersonID,
				Operation: "withdrawal_payout", KYCOutcome: kycOutcome, HoldOutcome: aml.HoldOutcomeHeld, PublicOutcome: "unavailable", Allowed: false, HoldID: holdID, CorrelationID: uuid.New()})
		}); err != nil {
			t.Fatal(err)
		}
	}
	rec(a, false, "passed")       // parked by the screening gate only
	rec(a, false, "not_required") // a second evaluation of the same person
	rec(b, true, "passed")        // an officer hold: a finding, not a waiting state
	rec(c, false, "pending")      // KYC pending: not a screening park
	var persons, evals int
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var err error
		persons, evals, err = aml.ScreeningParkedSummary(ctx, tx, tw.Tenant)
		return err
	}); err != nil || persons != 1 || evals != 2 {
		t.Fatalf("persons=%d evaluations=%d err=%v, want 1 and 2", persons, evals, err)
	}
	// The worker pass reports it too (counts only).
	if st := tw.worker().RunPass(ctx); st.ParkedScreening != 1 {
		t.Fatalf("pass summary parked = %d, want 1", st.ParkedScreening)
	}
	// A foreign tenant argument is refused.
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := aml.ScreeningParkedSummary(ctx, tx, uuid.New())
		return err
	}); pgCode(err) != "AT002" {
		t.Fatalf("foreign tenant: %v, want AT002", err)
	}
}
