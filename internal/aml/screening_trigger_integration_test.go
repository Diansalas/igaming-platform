//go:build integration

package aml_test

// lic10 (migration 0181, audit G1): real-PostgreSQL tests of the automatic screening triggers, the durable queue, the worker
// (MOCK provider only) and the withdrawal / payout gate. NOT RUN by their author (the database harness was refused in the
// authoring session): the owner runs them. All values are TEST FIXTURE VALUES, not legal thresholds.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

type trigWorld struct {
	*amlWorld
	q    *amlfix.Quorum
	svc  *aml.Service
	jur  uuid.UUID
	prov *countingProvider
}

// countingProvider wraps the MOCK and counts calls that reach it (replays included).
type countingProvider struct {
	inner *aml.MockScreeningProvider
	n     atomic.Int64
}

func (c *countingProvider) ID() string                              { return c.inner.ID() }
func (c *countingProvider) Capabilities() aml.ScreeningCapabilities { return c.inner.Capabilities() }
func (c *countingProvider) Screen(ctx context.Context, in aml.ScreeningRequest) (aml.ScreeningResult, error) {
	c.n.Add(1)
	return c.inner.Screen(ctx, in)
}

// bindLicence gives the tenant a licence in jurisdiction j (the jurisdiction the trigger policy must be for).
func (w *amlWorld) bindLicence(j uuid.UUID) {
	w.t.Helper()
	if err := w.Owner.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		var lic uuid.UUID
		var licensee string
		if err := tx.QueryRow(ctx, `SELECT expected_licensee FROM tenants WHERE id = $1`, w.Tenant).Scan(&licensee); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, $2, $3) RETURNING id`, j, licensee, "TRIG-"+j.String()[:8]).Scan(&lic); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, lic, w.Tenant)
		return err
	}); err != nil {
		w.t.Fatalf("bind licence: %v", err)
	}
}

// newTrigWorld: a platform-licensed tenant with a two-officer quorum, a licence in jurisdiction j and a screening policy.
func newTrigWorld(t *testing.T, rescreen time.Duration, grace time.Duration) *trigWorld {
	t.Helper()
	w, q, svc := platformWorld(t)
	j := w.jurisdiction()
	w.bindLicence(j)
	in := policyInput(j, time.Now().Add(-time.Hour))
	in.RescreenInterval, in.BackfillGrace = rescreen, grace
	if _, err := svc.SetScreeningPolicy(context.Background(), q.Officers[0].Actor, in); err != nil {
		t.Fatalf("screening policy: %v", err)
	}
	return &trigWorld{amlWorld: w, q: q, svc: svc, jur: j, prov: &countingProvider{inner: aml.NewMockScreeningProvider()}}
}

func (tw *trigWorld) setTrigger(events []aml.TriggerEvent, gate bool, from time.Time) {
	tw.t.Helper()
	if _, err := tw.svc.SetScreeningTriggerPolicy(context.Background(), tw.q.Officers[0].Actor, aml.SetTriggerPolicyInput{
		JurisdictionID: tw.jur, Events: events, GateWithdrawal: gate, EffectiveFrom: from,
		LegalReviewReference: "TEST FIXTURE REF - NOT LEGAL ADVICE", ReasonCode: "fixture_trigger"}); err != nil {
		tw.t.Fatalf("trigger policy: %v", err)
	}
}

func (tw *trigWorld) worker() *aml.ScreeningWorker {
	wk := aml.NewScreeningWorker(tw.RT, tw.prov, aml.MockSubjectKeys{})
	wk.TenantScope = func() []uuid.UUID { return []uuid.UUID{tw.Tenant} }
	wk.Config.BackoffBase = 10 * time.Millisecond
	wk.Config.BackoffCap = 20 * time.Millisecond
	return wk
}

// setVendorRef gives the player's person a KYC verification row (status pending: no approval trigger) whose provider reference
// drives the MOCK's marker outcomes.
func (tw *trigWorld) setVendorRef(p player, ref string) {
	tw.t.Helper()
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference)
			VALUES ($1, $2, $3, $4, 'pending', 'mock', $5)`, tw.Tenant, tw.Brand.ID, p.AccountID, p.PersonID, ref)
		return err
	}); err != nil {
		tw.t.Fatalf("seed kyc row: %v", err)
	}
}

type reqView struct {
	ID, Key, Status, Trigger string
	Attempts                 int
}

// requests reads the queue as the OWNER under the tenant session (the runtime role has no privilege on the table).
func (tw *trigWorld) requests(person uuid.UUID) []reqView {
	tw.t.Helper()
	var out []reqView
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id::text, idempotency_key, status, trigger, attempts FROM aml_screening_requests WHERE tenant_id = $1 AND person_id = $2 ORDER BY created_at, id`, tw.Tenant, person)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r reqView
			if err := rows.Scan(&r.ID, &r.Key, &r.Status, &r.Trigger, &r.Attempts); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		tw.t.Fatalf("read queue: %v", err)
	}
	return out
}

func (tw *trigWorld) heldFor(p player, op aml.Operation) bool {
	tw.t.Helper()
	held, err := tw.holdActive(p.PersonID, p.AccountID, op)
	if err != nil {
		tw.t.Fatalf("hold evaluation: %v", err)
	}
	return held
}

func TestTriggerPolicy_Authoring_IsGovernedLikeTheScreeningPolicy(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	j := w.jurisdiction()
	a := q.Officers[0].Actor
	in := aml.SetTriggerPolicyInput{JurisdictionID: j, Events: []aml.TriggerEvent{aml.EventRegistration}, GateWithdrawal: true, EffectiveFrom: time.Now().Add(-time.Minute),
		LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture_trigger"}
	// Unconfigured: nil, never a default.
	if p, err := svc.EffectiveScreeningTriggerPolicy(ctx, a, j); err != nil || p != nil {
		t.Fatalf("unconfigured must be nil: %+v %v", p, err)
	}
	// The jurisdiction must be the tenant's LICENCE jurisdiction (the only one the enqueue and the gate resolve), and the screening
	// policy must exist first.
	if _, err := svc.SetScreeningTriggerPolicy(ctx, a, in); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("no licence bound: %v, want ErrInvalidInput (AT004)", err)
	}
	w.bindLicence(j)
	if _, err := svc.SetScreeningTriggerPolicy(ctx, a, in); !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("no screening policy yet: %v, want ErrInvalidInput (AT004)", err)
	}
	if _, err := svc.SetScreeningPolicy(ctx, a, policyInput(j, time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetScreeningTriggerPolicy(ctx, a, in); err != nil {
		t.Fatalf("set: %v", err)
	}
	p, err := svc.EffectiveScreeningTriggerPolicy(ctx, a, j)
	if err != nil || p == nil || !p.GateWithdrawal || len(p.Events) != 1 || p.Events[0] != aml.EventRegistration {
		t.Fatalf("effective: %+v %v", p, err)
	}
	// A NEWER version wins; an explicit empty ungated version is "nothing triggers".
	empty := in
	empty.Events, empty.GateWithdrawal = nil, false
	if _, err := svc.SetScreeningTriggerPolicy(ctx, a, empty); err != nil {
		t.Fatal(err)
	}
	if p, _ := svc.EffectiveScreeningTriggerPolicy(ctx, a, j); p == nil || p.GateWithdrawal || len(p.Events) != 0 {
		t.Fatalf("the newest version must win: %+v", p)
	}
	// The runtime role has no privilege on the table; the reader is session-tenant bound.
	if err := w.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM aml_screening_trigger_policies`)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("no runtime privilege: %v", err)
	}
	if err := w.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var c int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_trigger_policy_effective($1, $2)`, uuid.New(), j).Scan(&c)
	}); pgCode(err) != "AT002" {
		t.Fatalf("a foreign tenant argument must be AT002, got %v", err)
	}
	// Quorum: one officer cannot author; a tenant-scope officer of an own-licence tenant cannot author.
	w2 := newWorld(t, "own_licence")
	svc2 := amlfix.Service(t, w2.RT)
	q2 := amlfix.Bootstrap(t, w2.Owner, w2.Tenant, 2)
	j2 := w2.jurisdiction()
	w2.bindLicence(j2)
	if _, err := svc2.SetScreeningTriggerPolicy(ctx, q2.Officers[0].Actor, aml.SetTriggerPolicyInput{JurisdictionID: j2, Events: []aml.TriggerEvent{aml.EventRegistration},
		EffectiveFrom: time.Now(), LegalReviewReference: "x", ReasonCode: "r"}); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a tenant officer must not author trigger configuration: %v", err)
	}
}

func TestRegistrationTrigger_EnqueuesOnlyWhenConfigured_AndIdempotently(t *testing.T) {
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	// Before any trigger policy: a registration enqueues nothing.
	p0 := tw.player()
	if n := len(tw.requests(p0.PersonID)); n != 0 {
		t.Fatalf("no trigger policy: %d requests, want 0", n)
	}
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	p := tw.player()
	rs := tw.requests(p.PersonID)
	if len(rs) != 1 || rs[0].Status != "pending" || rs[0].Trigger != "registration" || rs[0].Key != "reg:"+p.AccountID.String() {
		t.Fatalf("registration request: %+v", rs)
	}
	// Idempotent: the same event again (the owner-only helper) adds no row; a direct duplicate key is a unique violation.
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var id *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT aml_screening_enqueue($1, $2, 'registration', $3)`, tw.Tenant, p.PersonID, "reg:"+p.AccountID.String()).Scan(&id); err != nil {
			return err
		}
		if id != nil {
			t.Error("a repeated enqueue must return NULL (nothing inserted)")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(tw.requests(p.PersonID)); n != 1 {
		t.Fatalf("requests after replay = %d", n)
	}
	// An event not in the policy (kyc_approval) enqueues nothing.
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference)
			VALUES ($1, $2, $3, $4, 'approved', 'mock', 'ref-x')`, tw.Tenant, tw.Brand.ID, p.AccountID, p.PersonID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n := len(tw.requests(p.PersonID)); n != 1 {
		t.Fatalf("kyc_approval is not in the policy: requests = %d, want still 1", n)
	}
	// With kyc_approval configured an approval enqueues once.
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventKYCApproval}, false, time.Now().Add(-time.Minute))
	p2 := tw.player()
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference)
			VALUES ($1, $2, $3, $4, 'pending', 'mock', 'ref-y')`, tw.Tenant, tw.Brand.ID, p2.AccountID, p2.PersonID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE kyc_verifications SET status = 'approved' WHERE player_account_id = $1`, p2.AccountID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var kycReqs int
	for _, r := range tw.requests(p2.PersonID) {
		if r.Trigger == "kyc_approval" {
			kycReqs++
		}
	}
	if kycReqs != 1 {
		t.Fatalf("kyc_approval requests = %d, want 1", kycReqs)
	}
}

func TestWorker_Outcomes_ClearDone_MatchNeedsOfficer_ErrorRetriesThenTerminal(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	pc, ps, pu := tw.player(), tw.player(), tw.player()
	tw.setVendorRef(ps, aml.MockMarkerSanction+"1")
	tw.setVendorRef(pu, aml.MockMarkerUnavailable)
	wk := tw.worker()
	wk.Config.MaxAttempts = 3

	st := wk.RunPass(ctx)
	if st.Claimed != 3 || st.Done != 1 || st.NeedsOfficer != 1 || st.Retried != 1 {
		t.Fatalf("first pass: %+v", st)
	}
	if r := tw.requests(pc.PersonID); len(r) != 1 || r[0].Status != "done" {
		t.Fatalf("clear person: %+v", r)
	}
	if r := tw.requests(ps.PersonID); len(r) != 1 || r[0].Status != "needs_officer" {
		t.Fatalf("a sanctions match must wait for an officer, never be cleared: %+v", r)
	}
	// The unavailable provider is retried, then ends failed_terminal (an error is NEVER a clear).
	for i := 0; i < 5; i++ {
		wk.RunPass(ctx)
		time.Sleep(50 * time.Millisecond)
	}
	r := tw.requests(pu.PersonID)
	if len(r) != 1 || r[0].Status != "failed_terminal" || r[0].Attempts != 3 {
		t.Fatalf("retry budget: %+v", r)
	}
	// Terminal and done requests are final in the database.
	for _, id := range []string{r[0].ID, tw.requests(pc.PersonID)[0].ID} {
		err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE aml_screening_requests SET status = 'pending', attempts = 0 WHERE id = $1`, id)
			return err
		})
		if pgCode(err) != "AT005" {
			t.Errorf("a final request must be immutable (AT005), got %v", err)
		}
	}
	// Alerts: one match_review and one failed_terminal attention alert, carrying ids only.
	var n int
	if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'aml.screening_attention' AND subject_tenant_id = $1
			AND attributes ->> 'reason' IN ('match_review', 'failed_terminal') AND NOT (attributes ? 'person_id')`, tw.Tenant).Scan(&n)
	}); err != nil || n != 2 {
		t.Fatalf("attention alerts = %d err=%v, want 2", n, err)
	}
	// The mock was called exactly once per attempt: no request was processed twice by one pass.
	if got := tw.prov.n.Load(); got != 1+1+3 {
		t.Fatalf("provider calls = %d, want 5 (clear 1, match 1, unavailable 3 attempts)", got)
	}
}

func TestGate_WithdrawalHeldUntilAFreshCompletedClear_DepositsNeverGated(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 3*time.Second, time.Hour)
	// An EXISTING player (registered before the trigger policy) is not held during the backfill grace.
	existing := tw.player()
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Now().Add(-time.Second))
	if tw.heldFor(existing, aml.OpWithdrawal) {
		t.Fatal("an existing person inside the backfill grace must not be held")
	}
	// A NEW player is held for withdrawal and payout while the screening is pending; deposits and stakes are untouched.
	p := tw.player()
	for _, op := range []aml.Operation{aml.OpWithdrawal, aml.OpPayout} {
		if !tw.heldFor(p, op) {
			t.Errorf("%s must be held while the required screening is pending (fail closed)", op)
		}
	}
	for _, op := range []aml.Operation{aml.OpDeposit, aml.OpCasinoStake, aml.OpSportsbookStake} {
		if tw.heldFor(p, op) {
			t.Errorf("%s must NOT be gated by the screening predicate", op)
		}
	}
	// Completed clear releases; the worker is the only way.
	wk := tw.worker()
	wk.RunPass(ctx)
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a fresh completed clear must release the withdrawal gate")
	}
	// Stale (older than the screening policy's re-screen interval) is held again: unavailable screening is not a pass.
	time.Sleep(4 * time.Second)
	// (the scheduler would enqueue a re-screen; before the worker runs, the person is held)
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a stale clear must hold withdrawal until the re-screen completes")
	}
	wk.RunPass(ctx) // scheduled enqueue + claim + clear
	if tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("after the scheduled re-screen the person is clear again")
	}
	// The existing person was screened by the scheduled backfill too.
	if rs := tw.requests(existing.PersonID); len(rs) == 0 {
		t.Fatal("the scheduled enqueue must backfill existing persons")
	}
}

func TestGate_ProviderUnavailable_FailsClosedForWithdrawal(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerUnavailable)
	wk := tw.worker()
	wk.Config.MaxAttempts = 2
	for i := 0; i < 4; i++ {
		wk.RunPass(ctx)
		time.Sleep(50 * time.Millisecond)
	}
	if r := tw.requests(p.PersonID); len(r) != 1 || r[0].Status != "failed_terminal" {
		t.Fatalf("request: %+v", r)
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a required screening that failed terminally must keep the withdrawal held (fail closed)")
	}
}

func TestGate_MatchNeedsOfficer_HeldUntilTheOfficerRecordsUnderTheSameKey(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, 0)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now().Add(-time.Second))
	p := tw.player()
	tw.setVendorRef(p, aml.MockMarkerPEP+"1")
	tw.worker().RunPass(ctx)
	rs := tw.requests(p.PersonID)
	if len(rs) != 1 || rs[0].Status != "needs_officer" {
		t.Fatalf("request: %+v", rs)
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("a system-found match must hold withdrawal before any officer acts")
	}
	// The officer finds the request (proof-bound read) and records the result under ITS key: that is what resolves it.
	officer := tw.q.Officers[0].Actor
	list, err := tw.svc.ListScreeningRequests(ctx, officer, p.PersonID)
	if err != nil || len(list) != 1 || list[0].Status != aml.RequestNeedsOfficer || list[0].Resolved || list[0].IdempotencyKey != rs[0].Key {
		t.Fatalf("officer list: %+v %v", list, err)
	}
	// A record under a DIFFERENT key does not resolve it.
	other := aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: "other-" + uuid.NewString(), Trigger: aml.TriggerManual, ProviderID: aml.MockProviderID,
		SubjectRef: strings.Repeat("b", 64), Lists: []aml.ListType{aml.ListPEP}, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone}
	if _, err := tw.svc.RecordScreening(ctx, officer, other); err != nil {
		t.Fatal(err)
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("an officer record under another key must not resolve the request")
	}
	// Under the request's key, a PEP potential match places the real hold: still held, by the hold.
	match := aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: rs[0].Key, Trigger: aml.TriggerRegistration, ProviderID: aml.MockProviderID,
		SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{aml.ListPEP}, Outcome: aml.OutcomePotentialMatch, MatchLists: []aml.ListType{aml.ListPEP},
		ScreenedAt: time.Now(), Decision: aml.HoldWithdrawalPayout, HoldReasonCode: aml.ReasonPEPMatch}
	if _, err := tw.svc.RecordScreening(ctx, officer, match); err != nil {
		t.Fatal(err)
	}
	list, _ = tw.svc.ListScreeningRequests(ctx, officer, p.PersonID)
	if len(list) != 1 || !list[0].Resolved {
		t.Fatalf("the request must read as resolved: %+v", list)
	}
	if !tw.heldFor(p, aml.OpWithdrawal) {
		t.Fatal("the officer's hold must keep withdrawal held")
	}
}

func TestWorker_Concurrency_TwoWorkersProcessEachRequestOnce(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	const n = 12
	for i := 0; i < n; i++ {
		tw.player()
	}
	var wg sync.WaitGroup
	var done atomic.Int64
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for pass := 0; pass < 4; pass++ {
				st := tw.worker().RunPass(ctx)
				done.Add(int64(st.Done))
			}
		}()
	}
	wg.Wait()
	if done.Load() != n || tw.prov.n.Load() != n {
		t.Fatalf("done=%d provider calls=%d, want exactly %d each (SKIP LOCKED + claim token)", done.Load(), tw.prov.n.Load(), n)
	}
}

func TestQueue_ClaimTokenCAS_StaleWorkerCannotFinishAReclaimedRequest(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	p := tw.player()
	type claim struct {
		id, token uuid.UUID
		attempts  int
	}
	doClaim := func() []claim {
		var out []claim
		if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT o_id, o_claim_token, o_attempts FROM aml_screening_request_claim($1, 10, 5)`, tw.Tenant)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var c claim
				if err := rows.Scan(&c.id, &c.token, &c.attempts); err != nil {
					return err
				}
				out = append(out, c)
			}
			return rows.Err()
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := doClaim()
	if len(first) != 1 || first[0].attempts != 1 {
		t.Fatalf("first claim: %+v", first)
	}
	if again := doClaim(); len(again) != 0 {
		t.Fatalf("a live lease must not be claimed again: %+v", again)
	}
	time.Sleep(6 * time.Second) // the minimum lease is 5 s
	second := doClaim()
	if len(second) != 1 || second[0].attempts != 2 || second[0].token == first[0].token {
		t.Fatalf("an expired lease is reclaimed under a NEW token: %+v", second)
	}
	finish := func(token uuid.UUID) (bool, string) {
		var applied bool
		var status string
		if err := tw.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT o_applied, o_status FROM aml_screening_request_finish($1, $2, $3, 'clear', NULL, NULL, 'mock-screening', NULL, $4, now(), 0)`,
				tw.Tenant, first[0].id, token, strings.Repeat("c", 64)).Scan(&applied, &status)
		}); err != nil {
			t.Fatal(err)
		}
		return applied, status
	}
	if applied, _ := finish(first[0].token); applied {
		t.Fatal("the stale holder of the OLD token must not finish the reclaimed request")
	}
	if applied, status := finish(second[0].token); !applied || status != "done" {
		t.Fatalf("the current holder finishes: %v %q", applied, status)
	}
	if applied, status := finish(second[0].token); applied || status != "done" {
		t.Fatalf("a finished request is not applied twice: %v %q", applied, status)
	}
	_ = p
	_ = ctx
}

func TestQueue_TenantIsolation_AndNoRuntimePrivilege(t *testing.T) {
	ctx := context.Background()
	a := newTrigWorld(t, 24*time.Hour, time.Hour)
	a.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	a.player()
	b := newTrigWorld(t, 24*time.Hour, time.Hour)
	b.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	// B's session cannot claim, finish or enqueue A's tenant.
	err := b.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var c int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_request_claim($1, 5, 30)`, a.Tenant).Scan(&c)
	})
	if pgCode(err) != "AT002" {
		t.Fatalf("a foreign tenant claim must be AT002, got %v", err)
	}
	if err := b.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var c int
		return tx.QueryRow(ctx, `SELECT aml_screening_enqueue_due($1, 5)`, a.Tenant).Scan(&c)
	}); pgCode(err) != "AT002" {
		t.Fatalf("a foreign tenant enqueue must be AT002, got %v", err)
	}
	// B's worker pass leaves A's requests untouched.
	st := b.worker().RunPass(ctx)
	if st.Done != 0 {
		t.Fatalf("tenant b has no requests: %+v", st)
	}
	var aPending int
	if err := a.Owner.WithTenant(ctx, a.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_requests WHERE status = 'pending'`).Scan(&aPending)
	}); err != nil || aPending != 1 {
		t.Fatalf("tenant a pending = %d err=%v", aPending, err)
	}
	// The runtime role has no privilege on the queue, whatever the session.
	for _, stmt := range []string{`SELECT count(*) FROM aml_screening_requests`, `UPDATE aml_screening_requests SET status = 'done'`, `INSERT INTO aml_screening_requests DEFAULT VALUES`, `DELETE FROM aml_screening_requests`} {
		if err := a.tenantTx(func(ctx context.Context, tx pgx.Tx) error { _, e := tx.Exec(ctx, stmt); return e }); pgCode(err) != "42501" {
			t.Errorf("%q as the runtime role must be 42501, got %v", stmt, err)
		}
	}
}

func TestQueue_IllegalTransitions_RefusedByTheDatabase(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	tw.setTrigger([]aml.TriggerEvent{aml.EventRegistration}, false, time.Now().Add(-time.Minute))
	p := tw.player()
	id := tw.requests(p.PersonID)[0].ID
	for name, stmt := range map[string]string{
		"pending straight to done":          `UPDATE aml_screening_requests SET status = 'done', outcome = 'clear', provider_id = 'mock-screening', screened_at = now(), completed_at = now() WHERE id = $1`,
		"pending straight to needs_officer": `UPDATE aml_screening_requests SET status = 'needs_officer', outcome = 'potential_match', match_lists = ARRAY['pep'], provider_id = 'mock-screening', screened_at = now(), completed_at = now() WHERE id = $1`,
		"identity change":                   `UPDATE aml_screening_requests SET person_id = gen_random_uuid() WHERE id = $1`,
		"attempts reset":                    `UPDATE aml_screening_requests SET attempts = 5 WHERE id = $1`,
	} {
		err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error { _, e := tx.Exec(ctx, stmt, id); return e })
		if err == nil {
			t.Errorf("%s must be refused", name)
		}
	}
	// A match can never be recorded as done / a clear can never carry a match (table CHECKs).
	err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO aml_screening_requests (tenant_id, person_id, licensing_jurisdiction_id, trigger, idempotency_key, status, outcome, match_lists, provider_id, screened_at, completed_at)
			VALUES ($1, $2, $3, 'registration', 'bad-1', 'done', 'potential_match', ARRAY['pep'], 'mock-screening', now(), now())`, tw.Tenant, p.PersonID, tw.jur)
		return e
	})
	if err == nil {
		t.Error("a match recorded as done must be refused")
	}
}
