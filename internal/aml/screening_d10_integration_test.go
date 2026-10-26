//go:build integration

package aml_test

// d1 (D10, migration 0192): the freshness and the list binding of a screening clear. TEST FIXTURE VALUES - NOT LEGAL THRESHOLDS: the
// intervals below (seconds, hours) exist to exercise the mechanism; the owner-path setter has no floor, the routed author has the 3600 s floor.
//
// The defect, reproduced first (TestD10_LengtheningTheInterval_...): the gate judged a clear by the interval of the policy in force at
// EVALUATION time and never checked the lists, so a LONGER interval revived an expired clear (releasing a held withdrawal) and a pep-only
// clear still cleared the person once `sanctions` was in force. The fix is only ever stricter: provenance is recorded, a clear covers the person
// only under LEAST(recorded interval, current interval), a recorded-lists superset of the lists in force and no list dropped since; a clear
// without provenance (every row written before 0192) is not trusted and the person is due.

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
)

func (tw *trigWorld) policyID() uuid.UUID {
	tw.t.Helper()
	var id uuid.UUID
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT policy_id FROM aml_screening_policy_effective($1, $2)`, tw.Tenant, tw.jur).Scan(&id)
	}); err != nil {
		tw.t.Fatalf("policy in force: %v", err)
	}
	return id
}

// policyVersion writes a screening policy version through the OWNER-ONLY setter (any interval, loosening allowed): the fixture of "the interval
// was lengthened / a list was removed by a governed loosening or the break-glass".
func (tw *trigWorld) policyVersion(lists []aml.ListType, interval, grace time.Duration) {
	tw.t.Helper()
	in := aml.SetScreeningPolicyInput{JurisdictionID: tw.jur, Lists: lists, RescreenInterval: interval, BackfillGrace: grace, EffectiveFrom: time.Now(),
		LegalReviewReference: "TEST FIXTURE REF - NOT LEGAL ADVICE", ReasonCode: "fixture_policy"}
	if _, err := ownerSvc(tw.t).SetScreeningPolicy(context.Background(), tw.q.Officers[0].Actor, in); err != nil {
		tw.t.Fatalf("policy version (owner path): %v", err)
	}
}

// tighten authors a version through the ROUTE (tighten-only, floor 3600 s).
func (tw *trigWorld) tighten(lists []aml.ListType, interval time.Duration) {
	tw.t.Helper()
	in := aml.SetScreeningPolicyInput{JurisdictionID: tw.jur, Lists: lists, RescreenInterval: interval, BackfillGrace: 0, EffectiveFrom: time.Now(),
		LegalReviewReference: "TEST FIXTURE REF - NOT LEGAL ADVICE", ReasonCode: "fixture_policy"}
	if _, err := tw.svc.AuthorScreeningPolicy(context.Background(), tw.q.Officers[0].Actor, in); err != nil {
		tw.t.Fatalf("tightening through the route: %v", err)
	}
}

var bothLists = []aml.ListType{aml.ListSanctions, aml.ListPEP}

// ---- the defect, then the fix ---------------------------------------------------------------------------------------------

func TestD10_LengtheningTheInterval_NeverRevivesAnExpiredClear_NorReleasesAHeldWithdrawal(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 3*time.Second, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	p := tw.player()
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a new person is held until screened")
	}
	wk := tw.worker()
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a fresh completed clear releases the withdrawal gate")
	}
	time.Sleep(4 * time.Second) // the clear (3 s interval) is now STALE
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a stale clear holds the withdrawal")
	}
	// A governed loosening (or the break-glass) lengthens the re-screen interval to one hour.
	tw.policyVersion(bothLists, time.Hour, 0)
	if !tw.heldFor(p, aml.OpWithdrawal) || !tw.gateDenies(p) {
		t.Fatal("D10 DEFECT: lengthening the re-screen interval revived an EXPIRED clear and released a held withdrawal without any new screening")
	}
	// The held person is DUE: the next pass re-screens, and only that clear (recorded interval: one hour) releases.
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a new completed clear releases the withdrawal")
	}
	var rec int64
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT screened_interval_seconds FROM aml_screening_requests WHERE tenant_id = $1 AND person_id = $2 AND status = 'done' ORDER BY completed_at DESC LIMIT 1`,
			tw.Tenant, p.PersonID).Scan(&rec)
	}); err != nil || rec != 3600 {
		t.Fatalf("the new clear records the interval in force when it was produced (3600): %d %v", rec, err)
	}
}

func TestD10_ShorteningTheInterval_StillTightensAtOnce(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.worker().RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a fresh clear releases")
	}
	tw.policyVersion(bothLists, 3*time.Second, 0) // shortened (the owner path has no floor)
	time.Sleep(4 * time.Second)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a clear older than the SHORTENED interval is stale although it was taken under a longer one")
	}
}

// The coverage predicate: recorded-interval / current-interval matrix, lists, gap, missing provenance. Exact, no sleeps (explicit ages).
func TestD10_CoveragePredicate_Matrix(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	type c struct {
		name    string
		ageS    int
		recI    any // recorded interval seconds (nil = no provenance)
		recL    any // recorded lists (nil = none)
		curI    any // current interval seconds (nil = no policy)
		curL    any
		sinceS  any // aml_screening_since: nil = no version came into force since; 0 = a version since lacked a list; n = the smallest interval since
		covered bool
	}
	both, pep := "{pep,sanctions}", "{pep}"
	cases := []c{
		{"fresh under both", 1800, 3600, both, 3600, both, nil, true},
		{"expired under both", 3601, 3600, both, 3600, both, nil, false},
		{"LENGTHENED current never revives an expired clear", 3601, 3600, both, 7200, both, nil, false},
		{"lengthened current, still inside the recorded interval", 1800, 3600, both, 7200, both, nil, true},
		{"SHORTENED current tightens", 3601, 7200, both, 3600, both, nil, false},
		{"shortened current, still inside it", 1800, 7200, both, 3600, both, nil, true},
		{"no current policy: the recorded interval alone", 3601, 3600, both, nil, nil, nil, false},
		{"no current policy, inside", 1800, 3600, both, nil, nil, nil, true},
		{"NO provenance: interval missing", 10, nil, both, 3600, both, nil, false},
		{"NO provenance: lists missing", 10, 3600, nil, 3600, both, nil, false},
		{"pep-only clear, sanctions now in force", 10, 3600, pep, 3600, both, nil, false},
		{"both-lists clear, pep-only now in force", 10, 3600, both, 3600, pep, nil, true},
		{"list order is irrelevant", 10, 3600, "{sanctions,pep}", 3600, "{pep,sanctions}", nil, true},
		{"a list was dropped since the clear (since = 0)", 100, 3600, both, 3600, both, 0, false},
		{"a SHORTER version came into force since (60 s), the clear is older", 100, 7200, both, 7200, both, 60, false},
		{"a shorter version came into force since (200 s), the clear is younger", 100, 7200, both, 7200, both, 200, true},
		{"shorten then lengthen: the shortest interval since still binds", 3601, 7200, both, 7200, both, 3600, false},
	}
	for _, k := range cases {
		var got bool
		if err := w.Owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT aml_screening_covers(clock_timestamp() - make_interval(secs => $1::int), $2::bigint, $3::text[], $4::double precision, $5::text[], $6::bigint)`,
				k.ageS, k.recI, k.recL, k.curI, k.curL, k.sinceS).Scan(&got)
		}); err != nil {
			t.Fatalf("%s: %v", k.name, err)
		}
		if got != k.covered {
			t.Errorf("%s: covered=%v, want %v", k.name, got, k.covered)
		}
	}
}

// ---- lists --------------------------------------------------------------------------------------------------------------------

func TestD10_ListSuperset_PepOnlyClearNoLongerClearsOnceSanctionsIsInForce(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.policyVersion([]aml.ListType{aml.ListPEP}, 24*time.Hour, 0) // pep only is in force
	// An EXISTING person (registered before the gate, no grace) is screened by the SCHEDULED backfill: its idempotency key is bucketed, so a
	// tightened policy inside the same bucket needs the policy version in the key to queue it again.
	existing := tw.player()
	time.Sleep(1500 * time.Millisecond)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-500*time.Millisecond))
	clean, sanc := tw.player(), tw.player()
	tw.setVendorRef(sanc, aml.MockMarkerSanction+"1") // a sanctions-marked SYNTHETIC subject
	wk := tw.worker()
	wk.RunPass(ctx)
	wk.RunPass(ctx) // the scheduled backfill of the existing person
	people := map[string]player{"existing": existing, "clean": clean, "sanctions-marked": sanc}
	for name, p := range people {
		if tw.heldFor(p, aml.OpWithdrawal) {
			t.Fatalf("%s: with pep the only list in force, the pep-only screening clears the person", name)
		}
	}
	// Sanctions is added through the ROUTE (tighten-only).
	tw.tighten(bothLists, 24*time.Hour)
	for name, p := range people {
		if !tw.heldFor(p, aml.OpWithdrawal) || !tw.gateDenies(p) {
			t.Fatalf("D10 DEFECT: %s: a pep-only clear still clears the person once sanctions is in force", name)
		}
	}
	// All are DUE: the scheduler queues them, the worker re-screens against BOTH lists.
	wk.RunPass(ctx)
	for _, name := range []string{"existing", "clean"} {
		if tw.heldFor(people[name], aml.OpWithdrawal) {
			t.Fatalf("%s: cleared by a screening that covered both lists", name)
		}
	}
	if !tw.heldFor(sanc, aml.OpWithdrawal) {
		t.Fatal("the sanctions-marked person is now flagged and held (an unresolved match)")
	}
	if r := tw.needsOfficer(sanc); r.Status != "needs_officer" {
		t.Fatalf("the sanctions-marked person needs an officer: %+v", r)
	}
}

func TestD10_ListRemovedAndReAdded_InvalidatesOldClears(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	p := tw.player()
	wk := tw.worker()
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("cleared against both lists")
	}
	time.Sleep(50 * time.Millisecond)
	tw.policyVersion([]aml.ListType{aml.ListPEP}, 24*time.Hour, 0) // sanctions removed (governed loosening / break-glass)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("while only pep is required, the both-lists clear still covers the person")
	}
	time.Sleep(50 * time.Millisecond)
	tw.tighten(bothLists, 24*time.Hour) // sanctions re-added
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("D10: a list removed and re-added invalidates the clear taken before the removal, whatever lists that clear recorded")
	}
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the re-screen after the re-adding clears the person")
	}
}

// ---- officer results ---------------------------------------------------------------------------------------------------------

func TestD10_OfficerResults_RecordProvenance_AndCoverOnlyTheListsInForce(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	A := tw.q.Officers[0].Actor
	full, pepOnly := tw.player(), tw.player()
	if err := tw.record(A, full, "manual-"+uuid.NewString(), aml.TriggerManual, aml.RecordScreeningInput{Outcome: aml.OutcomeClear, Decision: aml.HoldNone}); err != nil {
		t.Fatal(err)
	}
	if err := tw.record(A, pepOnly, "manual-"+uuid.NewString(), aml.TriggerManual, aml.RecordScreeningInput{Outcome: aml.OutcomeClear, Decision: aml.HoldNone, Lists: []aml.ListType{aml.ListPEP}}); err != nil {
		t.Fatal(err)
	}
	if tw.heldFor(full, aml.OpWithdrawal) {
		t.Fatal("an officer clear that covered both lists in force releases")
	}
	if !tw.heldFor(pepOnly, aml.OpWithdrawal) {
		t.Fatal("an officer clear that covered only pep does not clear a person when both lists are required")
	}
	// The result carries the policy id and the interval in force when it was recorded; the table stays append-only.
	var pol uuid.UUID
	var interval int64
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT screened_policy_id, screened_interval_seconds FROM aml_screening_results WHERE tenant_id = $1 AND person_id = $2`, tw.Tenant, full.PersonID).Scan(&pol, &interval)
	}); err != nil || pol != tw.policyID() || interval != 86400 {
		t.Fatalf("provenance of the officer result: policy %s interval %d (%v), want %s / 86400", pol, interval, err, tw.policyID())
	}
	err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE aml_screening_results SET screened_interval_seconds = 1 WHERE tenant_id = $1`, tw.Tenant)
		return e
	})
	if pgCode(err) != "AM005" {
		t.Fatalf("results stay append-only: %v", err)
	}
	// An officer MATCH whose hold was lifted: covered only when its recorded lists cover the lists in force.
	m1, m2 := tw.player(), tw.player()
	for _, c := range []struct {
		p     player
		lists []aml.ListType
	}{{m1, bothLists}, {m2, []aml.ListType{aml.ListPEP}}} {
		key := "manual-" + uuid.NewString()
		_, err := tw.svc.RecordScreening(ctx, A, aml.RecordScreeningInput{PersonID: c.p.PersonID, IdempotencyKey: key, Trigger: aml.TriggerManual, ProviderID: "officer-manual",
			SubjectRef: strings.Repeat("b", 64), Lists: c.lists, Outcome: aml.OutcomePotentialMatch, MatchLists: []aml.ListType{aml.ListPEP}, ScreenedAt: time.Now(),
			Decision: aml.HoldWithdrawalPayout, HoldReasonCode: "fixture_hold"})
		if err != nil {
			t.Fatalf("record a pep potential match: %v", err)
		}
	}
	lifted := func(p player) {
		t.Helper()
		var hold uuid.UUID
		if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM aml_holds WHERE tenant_id = $1 AND person_id = $2`, tw.Tenant, p.PersonID).Scan(&hold)
		}); err != nil {
			t.Fatal(err)
		}
		if err := tw.lift(hold, tw.q.Officers[1].Actor); err != nil { // another officer lifts a manual-class hold
			t.Fatalf("lift: %v", err)
		}
	}
	lifted(m1)
	lifted(m2)
	if tw.heldFor(m1, aml.OpWithdrawal) {
		t.Fatal("a lifted match recorded against both lists releases (a manual-class hold)")
	}
	if !tw.heldFor(m2, aml.OpWithdrawal) {
		t.Fatal("a lifted match recorded against pep only does not cover both lists in force")
	}
}

// ---- legacy data: fail closed, then due ------------------------------------------------------------------------------------

// d10World is a platform-licensed tenant with one registered person, a screening policy (both lists, 2 h, grace 0) and a GATING trigger policy
// on a scratch database migrated through the given migration (the owner pool is the service pool there).
type d10World struct {
	pool   *db.Pool
	tenant uuid.UUID
	person uuid.UUID
	jid    uuid.UUID
	q      *amlfix.Quorum
	svc    *aml.Service
}

func newD10World(t *testing.T, prefix string, through int) *d10World {
	t.Helper()
	ctx := context.Background()
	pool, url := scratch(t, prefix)
	if _, err := pool.MigrateUp(ctx, migDir(t, int64(through))); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO database_environment (environment) VALUES ('test')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tenant, person, jid, q, svc := seedWorld(t, pool, url, prefix)
	A := q.Officers[0].Actor
	if err := pool.WithPlatformAdmin(ctx, q.Officers[0].StaffID, func(ctx context.Context, tx pgx.Tx) error {
		var lic uuid.UUID
		var licensee string
		if err := tx.QueryRow(ctx, `SELECT expected_licensee FROM tenants WHERE id = $1`, tenant).Scan(&licensee); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, $2, $3) RETURNING id`, jid, licensee, "UPG-"+jid.String()[:8]).Scan(&lic); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lic, tenant)
		return err
	}); err != nil {
		t.Fatalf("bind licence: %v", err)
	}
	if _, err := svc.SetScreeningPolicy(ctx, A, aml.SetScreeningPolicyInput{JurisdictionID: jid, Lists: bothLists, RescreenInterval: 2 * time.Hour, BackfillGrace: 0,
		EffectiveFrom: time.Now().Add(-time.Hour), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT screening_worker_heartbeat('mock-screening', true)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetScreeningTriggerPolicy(ctx, A, aml.SetTriggerPolicyInput{JurisdictionID: jid, Events: []aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled},
		GateWithdrawal: true, GateAlertAfter: time.Hour, EffectiveFrom: time.Now().Add(-time.Second), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}); err != nil {
		t.Fatal(err)
	}
	return &d10World{pool: pool, tenant: tenant, person: person, jid: jid, q: q, svc: svc}
}

func (w *d10World) q1(t *testing.T, sql string, args []any, dst ...any) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error { return tx.QueryRow(ctx, sql, args...).Scan(dst...) }); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func TestD10_Upgrade_LegacyClearsAreNotTrusted_SubjectsBecomeDue_AndNothingIsRewritten(t *testing.T) {
	ctx := context.Background()
	w := newD10World(t, "m0192u_", 191)
	pool, tenant, person, jid := w.pool, w.tenant, w.person, w.jid
	// 0191 schema: the scheduler queues the person; the OLD 12-argument finish records a clear WITHOUT provenance.
	q1 := func(sql string, args []any, dst ...any) { t.Helper(); w.q1(t, sql, args, dst...) }
	var enq int
	q1(`SELECT aml_screening_enqueue_due($1, 10)`, []any{tenant}, &enq)
	if enq != 1 {
		t.Fatalf("0191: the person is queued by the scheduler: %d", enq)
	}
	var id, token uuid.UUID
	q1(`SELECT o_id, o_claim_token FROM aml_screening_request_claim($1, 1, 30)`, []any{tenant}, &id, &token)
	var applied bool
	q1(`SELECT o_applied FROM aml_screening_request_finish($1, $2, $3, 'clear', NULL, NULL, 'mock-screening', 'ref-legacy', $4, now(), 0, NULL)`,
		[]any{tenant, id, token, strings.Repeat("c", 64)}, &applied)
	if !applied {
		t.Fatal("0191: the legacy clear was not applied")
	}
	var denies bool
	q1(`SELECT aml_screening_gate_denies($1, $2, 'withdrawal')`, []any{tenant, person}, &denies)
	if denies {
		t.Fatal("premise: under 0191 the legacy clear is fresh (the defect's old behaviour)")
	}
	// ---- upgrade to 0192 with that data in place ----
	if _, err := pool.MigrateUp(ctx, migDir(t, 192)); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	q1(`SELECT aml_screening_gate_denies($1, $2, 'withdrawal')`, []any{tenant, person}, &denies)
	if !denies {
		t.Fatal("a clear with NO recorded provenance must not be trusted: the person is denied after the upgrade")
	}
	var polNull, listsNull, intNull bool
	var status string
	q1(`SELECT screened_policy_id IS NULL, screened_lists IS NULL, screened_interval_seconds IS NULL, status FROM aml_screening_requests WHERE id = $1`, []any{id}, &polNull, &listsNull, &intNull, &status)
	if !polNull || !listsNull || !intNull || status != "done" {
		t.Fatalf("the legacy row is untouched (done, no provenance): %v %v %v %s", polNull, listsNull, intNull, status)
	}
	q1(`SELECT aml_screening_enqueue_due($1, 10)`, []any{tenant}, &enq)
	if enq != 1 {
		t.Fatalf("the person is DUE after the upgrade: %d queued", enq)
	}
	// The new 13-argument finish with the policy version in force records provenance and releases.
	var id2, tok2, pid uuid.UUID
	q1(`SELECT o_id, o_claim_token FROM aml_screening_request_claim($1, 1, 30)`, []any{tenant}, &id2, &tok2)
	q1(`SELECT policy_id FROM aml_screening_policy_effective($1, $2)`, []any{tenant, jid}, &pid)
	q1(`SELECT o_applied FROM aml_screening_request_finish($1, $2, $3, 'clear', NULL, NULL, 'mock-screening', 'ref-new', $4, now(), 0, NULL, $5)`,
		[]any{tenant, id2, tok2, strings.Repeat("d", 64), pid}, &applied)
	if !applied {
		t.Fatal("the re-screen clear was not applied")
	}
	q1(`SELECT aml_screening_gate_denies($1, $2, 'withdrawal')`, []any{tenant, person}, &denies)
	if denies {
		t.Fatal("a clear WITH provenance releases the person")
	}
	var rp uuid.UUID
	var rl []string
	var ri int64
	q1(`SELECT screened_policy_id, screened_lists, screened_interval_seconds FROM aml_screening_requests WHERE id = $1`, []any{id2}, &rp, &rl, &ri)
	if rp != pid || strings.Join(rl, ",") != "pep,sanctions" || ri != 7200 {
		t.Fatalf("recorded provenance: %s %v %d, want %s [pep sanctions] 7200", rp, rl, ri, pid)
	}
}

// ---- the finish function ------------------------------------------------------------------------------------------------------

func TestD10_Finish_RecordsProvenanceFromThePolicyRow_RefusesBadPolicies_IdempotentAndConcurrent(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	tw.heartbeat()
	p := tw.player()
	pid := tw.policyID()
	// a policy version of ANOTHER tenant, one of ANOTHER jurisdiction, and one not yet in effect
	other := newTrigWorld(t, 24*time.Hour, 0)
	otherPol := other.policyID()
	j2 := tw.jurisdiction()
	if _, err := ownerSvc(t).SetScreeningPolicy(ctx, tw.q.Officers[0].Actor, aml.SetScreeningPolicyInput{JurisdictionID: j2, Lists: bothLists, RescreenInterval: time.Hour,
		EffectiveFrom: time.Now().Add(-time.Hour), LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}); err != nil {
		t.Fatal(err)
	}
	var otherJurPol uuid.UUID
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM aml_screening_policies WHERE tenant_id = $1 AND licensing_jurisdiction_id = $2`, tw.Tenant, j2).Scan(&otherJurPol)
	}); err != nil {
		t.Fatal(err)
	}
	future := aml.SetScreeningPolicyInput{JurisdictionID: tw.jur, Lists: bothLists, RescreenInterval: 24 * time.Hour, EffectiveFrom: time.Now().Add(time.Hour),
		LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}
	if _, err := ownerSvc(t).SetScreeningPolicy(ctx, tw.q.Officers[0].Actor, future); err != nil {
		t.Fatal(err)
	}
	var futurePol uuid.UUID
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM aml_screening_policies WHERE tenant_id = $1 AND licensing_jurisdiction_id = $2 ORDER BY created_at DESC LIMIT 1`, tw.Tenant, tw.jur).Scan(&futurePol)
	}); err != nil {
		t.Fatal(err)
	}
	var id, token uuid.UUID
	if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT o_id, o_claim_token FROM aml_screening_request_claim($1, 1, 60)`, tw.Tenant).Scan(&id, &token)
	}); err != nil {
		t.Fatal(err)
	}
	finish := func(policy any) (bool, string, error) {
		var applied bool
		var status string
		err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT o_applied, o_status FROM aml_screening_request_finish($1, $2, $3, 'clear', NULL, NULL, 'mock-screening', 'ref-1', $4, now(), 0, NULL, $5)`,
				tw.Tenant, id, token, strings.Repeat("c", 64), policy).Scan(&applied, &status)
		})
		return applied, status, err
	}
	for name, pol := range map[string]any{"no policy named": nil, "a policy of another tenant": otherPol, "a policy of another jurisdiction": otherJurPol,
		"a policy version not yet in effect": futurePol, "an unknown policy": uuid.New()} {
		if _, _, err := finish(pol); pgCode(err) != "AT001" {
			t.Errorf("%s: want AT001, got %v", name, err)
		}
	}
	for _, r := range tw.requests(p.PersonID) {
		if r.Status != "claimed" {
			t.Fatalf("a refused finish leaves the request claimed: %+v", r)
		}
	}
	// Two concurrent finishes with the same token and the same policy: exactly one is applied.
	var wg sync.WaitGroup
	res := make([]bool, 2)
	errs := make([]error, 2)
	for i := range res {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res[i], _, errs[i] = finish(pid)
		}(i)
	}
	wg.Wait()
	if errs[0] != nil || errs[1] != nil || res[0] == res[1] {
		t.Fatalf("exactly one concurrent finish is applied: %v %v %v", res, errs[0], errs[1])
	}
	var rp uuid.UUID
	var rl []string
	var ri int64
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT screened_policy_id, screened_lists, screened_interval_seconds FROM aml_screening_requests WHERE id = $1`, id).Scan(&rp, &rl, &ri)
	}); err != nil || rp != pid || strings.Join(rl, ",") != "pep,sanctions" || ri != 86400 {
		t.Fatalf("the lists and the interval come from the policy ROW: %s %v %d (%v)", rp, rl, ri, err)
	}
	// A done request is final, also for the owner.
	err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE aml_screening_requests SET screened_interval_seconds = 1 WHERE id = $1`, id)
		return e
	})
	if pgCode(err) != "AT005" {
		t.Fatalf("a terminal request stays final: %v", err)
	}
	// There is exactly one finish function (the 12-argument one is gone: no path can end a request without naming its policy).
	var n int
	if err := tw.Owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'aml_screening_request_finish' AND pronargs = 13`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("the 13-argument finish: %d %v", n, err)
	}
	if err := tw.Owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_proc WHERE proname = 'aml_screening_request_finish' AND pronargs <> 13`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("no other overload of the finish function may exist: %d %v", n, err)
	}
}

// ---- the trapped-funds monitor sees newly stale persons ----------------------------------------------------------------------------

func TestD10_NewlyStalePersons_AreCountedByTheTrappedFundsMonitor_AndLeaveItOnceRescreened(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.policyVersion([]aml.ListType{aml.ListPEP}, 24*time.Hour, 0)
	tw.gate(60*time.Second, time.Now().Add(-110*time.Second)) // the gate took effect 110 s ago; a held person is overdue after 60 s
	p := tw.player()
	wk := tw.worker()
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("cleared against the pep list in force")
	}
	overdue := func() int {
		var n int
		if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT aml_screening_gate_overdue($1, 100)`, tw.Tenant).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if n := overdue(); n != 0 {
		t.Fatalf("nobody is held yet: %d", n)
	}
	tw.tighten(bothLists, 24*time.Hour) // sanctions added: the clear no longer covers the person
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the pep-only clear no longer clears")
	}
	if n := overdue(); n != 1 {
		t.Fatalf("a newly stale person is visible to the trapped-funds monitor (gate overdue): %d", n)
	}
	if _, o := tw.parkedOverdue(); o != 1 {
		t.Fatalf("and to the payments sweeper's read (parked overdue): %d", o)
	}
	wk.RunPass(ctx)
	if n := overdue(); n != 0 {
		t.Fatalf("once re-screened the person leaves the monitor: %d", n)
	}
}

// ---- the auto-reviewed re-flag and the lifted officer match are bound like a clear ------------------------------------------------

func TestD10_Reflag_AutoReviewedReflagAndLiftedMatch_AreBoundLikeAClear(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 8*time.Second, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerPEP+"1")
	wk := tw.worker()
	wk.RunPass(ctx)
	A, B := tw.q.Officers[0].Actor, tw.q.Officers[1].Actor
	req := tw.needsOfficer(p)
	rec, err := tw.svc.ResolveScreeningRequest(ctx, A, p.PersonID, uuid.MustParse(req.ID), aml.ResolveScreeningInput{Outcome: aml.OutcomePotentialMatch,
		Lists: bothLists, MatchLists: []aml.ListType{aml.ListPEP}, ReasonCode: "pep_accepted"})
	if err != nil || rec.HoldID == nil {
		t.Fatalf("record: %+v %v", rec, err)
	}
	if _, err := tw.svc.LiftHold(ctx, B, *rec.HoldID, "pep_accepted"); err != nil {
		t.Fatalf("lift: %v", err)
	}
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("an accepted PEP match satisfies the gate")
	}
	time.Sleep(9 * time.Second) // stale under the 8 s interval it was recorded under
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("stale")
	}
	wk.RunPass(ctx) // the scheduled re-screen flags the SAME hit: auto-reviewed, which satisfies the gate
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the same reviewed hit does not hold the person again")
	}
	time.Sleep(9 * time.Second) // the auto-reviewed re-flag is stale too
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("stale again")
	}
	tw.policyVersion(bothLists, time.Hour, 0) // lengthened
	if !tw.heldFor(p, aml.OpWithdrawal) || !tw.gateDenies(p) {
		t.Fatal("D10: lengthening the interval revived an expired auto-reviewed re-flag / lifted officer match")
	}
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("after the re-screen (the same reviewed hit) the person is satisfied again")
	}
}

// ---- migration 0192 on a throwaway database -----------------------------------------------------------------------------------------

func TestMigration0192_UpDownUp_WholeSchema_DownRefusedOnceProvenanceExists(t *testing.T) {
	ctx := context.Background()
	pool, _ := scratch(t, "m0192s_")
	if _, err := pool.MigrateUp(ctx, migDir(t, versionBefore(t, 192))); err != nil {
		t.Fatal(err)
	}
	base := snapshot(t, pool)
	full := migDir(t, 192)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("up: %v", err)
	}
	up := snapshot(t, pool)
	for _, want := range []string{"screened_policy_id", "screened_lists", "screened_interval_seconds", "aml_screening_covers", "aml_screening_requests_provenance_required"} {
		if !strings.Contains(up, want) {
			t.Errorf("the up snapshot lacks %q", want)
		}
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	wantSame(t, "after down 0192", base, snapshot(t, pool))
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("second up: %v", err)
	}
	wantSame(t, "second up vs first up", up, snapshot(t, pool))

	// Down is refused (AT095) once a request carries provenance: a world on the current schema produces one clear with the new finish.
	w := newD10World(t, "m0192d_", 192)
	var id, token, pid uuid.UUID
	var enq int
	w.q1(t, `SELECT aml_screening_enqueue_due($1, 10)`, []any{w.tenant}, &enq)
	w.q1(t, `SELECT o_id, o_claim_token FROM aml_screening_request_claim($1, 1, 30)`, []any{w.tenant}, &id, &token)
	w.q1(t, `SELECT policy_id FROM aml_screening_policy_effective($1, $2)`, []any{w.tenant, w.jid}, &pid)
	var applied bool
	w.q1(t, `SELECT o_applied FROM aml_screening_request_finish($1, $2, $3, 'clear', NULL, NULL, 'mock-screening', 'ref-d', $4, now(), 0, NULL, $5)`,
		[]any{w.tenant, id, token, strings.Repeat("d", 64), pid}, &applied)
	if enq != 1 || !applied {
		t.Fatalf("premise: one request queued and cleared with provenance: %d %v", enq, applied)
	}
	before := snapshot(t, w.pool)
	if _, err := w.pool.MigrateDown(ctx, migDir(t, 192), 1); err == nil || !strings.Contains(err.Error(), "AT095") {
		t.Fatalf("0192 down must refuse AT095 once provenance exists: %v", err)
	}
	wantSame(t, "a refused down changes nothing", before, snapshot(t, w.pool))
}
