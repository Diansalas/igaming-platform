package aml_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/aml/screeningtest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

// ADR 0116 s4 (A7). Pure / DB-free tests of the screening decision, the orchestrator's fail-closed normalisation and the
// MOCK provider. The database side (record + hold in one transaction, policies, readiness) is screening_integration_test.go.

// ---- Decide ---------------------------------------------------------------------------------------------------------

func TestDecide_PolicyMatrix(t *testing.T) {
	eff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // TEST FIXTURE VALUE - NOT A LEGAL THRESHOLD
	grace := 24 * time.Hour
	pol := &aml.Policy{Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, RescreenInterval: time.Hour, BackfillGrace: grace, EffectiveFrom: eff}
	existing := eff.Add(-time.Hour)
	fresh := eff.Add(time.Hour)
	for _, c := range []struct {
		name       string
		now        time.Time
		pol        *aml.Policy
		registered time.Time
		outcome    aml.ScreeningOutcome
		matches    []aml.ListType
		want       aml.HoldAction
		reason     string
		retry, cas bool
	}{
		{"clear", eff, pol, existing, aml.OutcomeClear, nil, aml.HoldNone, "", false, false},
		{"sanctions potential match", eff, pol, existing, aml.OutcomePotentialMatch, []aml.ListType{aml.ListSanctions}, aml.HoldAllValueMovement, aml.ReasonSanctionsMatch, false, true},
		{"sanctions + pep", eff, pol, existing, aml.OutcomePotentialMatch, []aml.ListType{aml.ListPEP, aml.ListSanctions}, aml.HoldAllValueMovement, aml.ReasonSanctionsMatch, false, true},
		{"pep only", eff, pol, existing, aml.OutcomePotentialMatch, []aml.ListType{aml.ListPEP}, aml.HoldWithdrawalPayout, aml.ReasonPEPMatch, false, true},
		{"confirmed", eff, pol, existing, aml.OutcomeConfirmed, []aml.ListType{aml.ListSanctions}, aml.HoldAllValueMovement, aml.ReasonSanctionsConfirmed, false, true},
		{"a match acts with NO policy", eff, nil, existing, aml.OutcomePotentialMatch, []aml.ListType{aml.ListSanctions}, aml.HoldAllValueMovement, aml.ReasonSanctionsMatch, false, true},
		{"error, no policy: no mandate, retry", eff, nil, fresh, aml.OutcomeError, nil, aml.HoldNone, "", true, false},
		{"error before effective_from: retry only", eff.Add(-time.Nanosecond), pol, fresh, aml.OutcomeError, nil, aml.HoldNone, "", true, false},
		{"error, NEW person at effective_from: held", eff, pol, eff, aml.OutcomeError, nil, aml.HoldWithdrawalPayout, aml.ReasonUnavailable, true, false},
		{"error, person registered 1ns before effective_from, inside grace: no hold", eff, pol, eff.Add(-time.Nanosecond), aml.OutcomeError, nil, aml.HoldNone, "", true, false},
		{"error, existing person one ns before grace ends: no hold", eff.Add(grace - time.Nanosecond), pol, existing, aml.OutcomeError, nil, aml.HoldNone, "", true, false},
		{"error, existing person exactly at grace end: held", eff.Add(grace), pol, existing, aml.OutcomeError, nil, aml.HoldWithdrawalPayout, aml.ReasonUnavailable, true, false},
		{"error, existing person long after grace: held", eff.Add(grace * 10), pol, existing, aml.OutcomeError, nil, aml.HoldWithdrawalPayout, aml.ReasonUnavailable, true, false},
		{"zero grace: existing person held from effective_from", eff, &aml.Policy{Lists: pol.Lists, EffectiveFrom: eff}, existing, aml.OutcomeError, nil, aml.HoldWithdrawalPayout, aml.ReasonUnavailable, true, false},
		// An unknown outcome value is an error, never a clear.
		{"unknown outcome, new person", eff, pol, fresh, aml.ScreeningOutcome("approved"), nil, aml.HoldWithdrawalPayout, aml.ReasonUnavailable, true, false},
		{"empty outcome", eff, pol, fresh, aml.ScreeningOutcome(""), nil, aml.HoldWithdrawalPayout, aml.ReasonUnavailable, true, false},
	} {
		d := aml.Decide(c.now, c.pol, c.registered, c.outcome, c.matches)
		if d.Action != c.want || d.ReasonCode != c.reason || d.Retry != c.retry || d.OpenCase != c.cas {
			t.Errorf("%s: %+v, want action=%s reason=%q retry=%v case=%v", c.name, d, c.want, c.reason, c.retry, c.cas)
		}
	}
}

// ---- the MOCK provider: conformance + labelling ----------------------------------------------------------------------

func TestMockScreeningProvider_Conformance(t *testing.T) {
	screeningtest.Run(t, func() aml.ScreeningProvider { return aml.NewMockScreeningProvider() }, screeningtest.Subjects{
		Clear: "SYNTH-CLEAR-1", Sanction: aml.MockMarkerSanction + "1", PEP: aml.MockMarkerPEP + "1", Confirmed: aml.MockMarkerConfirmed + "1",
		Unavailable: aml.MockMarkerUnavailable,
	})
}

func TestMockScreeningProvider_IsSyntheticAndRefusedInProduction(t *testing.T) {
	m := aml.NewMockScreeningProvider()
	if _, ok := any(m).(providerkind.Synthetic); !ok {
		t.Fatal("the mock must implement providerkind.Synthetic")
	}
	if _, ok := any(m).(providerkind.ProductionEligible); ok {
		t.Fatal("the mock must never claim ProductionEligible")
	}
	regs := []providerkind.Registration{{Domain: "aml", Name: "screening_provider", Component: m}}
	if err := providerkind.RefuseSyntheticInProduction("production", regs); err == nil || !strings.Contains(err.Error(), "aml/screening_provider") {
		t.Fatalf("production must refuse the mock, got %v", err)
	}
	if err := providerkind.RefuseSyntheticInProduction("development", regs); err != nil {
		t.Fatalf("a non-production environment allows the mock: %v", err)
	}
	if _, ok := any(aml.MockSubjectKeys{}).(providerkind.Synthetic); !ok {
		t.Fatal("the mock key resolver must be synthetic too")
	}
	if aml.MockProviderID != "mock-screening" || m.ID() != "mock-screening" {
		t.Fatalf("the mock's audit id must say mock: %q", m.ID())
	}
}

// ---- orchestrator ----------------------------------------------------------------------------------------------------

type fakeStore struct {
	mu        sync.Mutex
	policy    *aml.Policy
	policyErr error
	recordErr error
	caseErr   error
	recorded  []aml.RecordScreeningInput
	cases     int
	byKey     map[string]aml.ScreeningRecorded
}

func (f *fakeStore) EffectiveScreeningPolicy(context.Context, aml.Actor, uuid.UUID) (*aml.Policy, error) {
	return f.policy, f.policyErr
}
func (f *fakeStore) RecordScreening(_ context.Context, _ aml.Actor, in aml.RecordScreeningInput) (aml.ScreeningRecorded, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recordErr != nil {
		return aml.ScreeningRecorded{}, f.recordErr
	}
	if err := in.Validate(); err != nil { // the real store validates before any proof is signed
		return aml.ScreeningRecorded{}, err
	}
	if f.byKey == nil {
		f.byKey = map[string]aml.ScreeningRecorded{}
	}
	if prior, ok := f.byKey[in.IdempotencyKey]; ok {
		prior.Replayed = true
		return prior, nil
	}
	rec := aml.ScreeningRecorded{ResultID: uuid.New()}
	if in.Decision != aml.HoldNone {
		h := uuid.New()
		rec.HoldID = &h
	}
	f.byKey[in.IdempotencyKey] = rec
	f.recorded = append(f.recorded, in)
	return rec, nil
}
func (f *fakeStore) OpenCase(context.Context, aml.Actor, uuid.UUID, aml.CaseKind, string, []uuid.UUID) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.caseErr != nil {
		return uuid.Nil, f.caseErr
	}
	f.cases++
	return uuid.New(), nil
}

// scripted is a provider whose behaviour the test controls; it records what it was asked.
type scripted struct {
	res   aml.ScreeningResult
	err   error
	pnc   bool
	hang  bool
	calls int
	got   []aml.ScreeningRequest
}

func (s *scripted) ID() string { return "scripted" }
func (s *scripted) Capabilities() aml.ScreeningCapabilities {
	return aml.ScreeningCapabilities{Sanctions: true, PEP: true, SupportsSubjectReference: true}
}
func (s *scripted) Screen(ctx context.Context, in aml.ScreeningRequest) (aml.ScreeningResult, error) {
	s.calls++
	s.got = append(s.got, in)
	if s.pnc {
		panic("vendor SDK exploded with person 123")
	}
	if s.hang {
		<-ctx.Done()
		return aml.ScreeningResult{}, ctx.Err()
	}
	return s.res, s.err
}

var effFrom = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) // TEST FIXTURE VALUE - NOT A LEGAL THRESHOLD

func newOrch(t *testing.T, p aml.ScreeningProvider, st *fakeStore, now time.Time) *aml.Orchestrator {
	t.Helper()
	o, err := aml.NewOrchestrator(aml.OrchestratorConfig{Provider: p, Store: st, Keys: aml.MockSubjectKeys{}, CallTimeout: 50 * time.Millisecond, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func req(person uuid.UUID, registered time.Time) aml.ScreenRequest {
	return aml.ScreenRequest{PersonID: person, JurisdictionID: uuid.New(), PersonRegisteredAt: registered, VendorSubjectRef: "SYNTH-CLEAR-1",
		Trigger: aml.TriggerManual, IdempotencyKey: "key-" + uuid.NewString()}
}

func actor() aml.Actor {
	return aml.Actor{StaffID: uuid.New(), TenantID: uuid.New(), Scope: aml.ScopeTenant}
}

func bothListsPolicy() *aml.Policy {
	return &aml.Policy{Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, RescreenInterval: time.Hour, BackfillGrace: time.Hour, EffectiveFrom: effFrom}
}

// The only road to a clear is a COMPLETED, self-consistent provider `clear`. Every other way the provider can misbehave is an
// `error` outcome (never clear), and a NEW person with no completed result is held.
func TestOrchestrator_FailClosed_NothingBadBecomesClear(t *testing.T) {
	now := effFrom.Add(time.Hour)
	fresh := effFrom.Add(time.Minute) // registered after effective_from: a missing result holds
	for _, c := range []struct {
		name string
		p    *scripted
		code aml.ScreeningErrorCode
	}{
		{"provider error", &scripted{err: errors.New("503 from vendor, body: John Smith")}, aml.ErrCodeUnknown},
		{"unavailable", &scripted{err: aml.ErrProviderUnavailable}, aml.ErrCodeUnavailable},
		{"wrapped timeout", &scripted{err: errors.Join(errors.New("x"), aml.ErrProviderTimeout)}, aml.ErrCodeTimeout},
		{"rate limited", &scripted{err: aml.ErrProviderRateLimited}, aml.ErrCodeRateLimited},
		{"hung provider hits the deadline", &scripted{hang: true}, aml.ErrCodeTimeout},
		{"panic", &scripted{pnc: true}, aml.ErrCodeUnknown},
		{"unknown outcome value", &scripted{res: aml.ScreeningResult{Outcome: "approved"}}, aml.ErrCodeInvalidResponse},
		{"empty outcome", &scripted{res: aml.ScreeningResult{}}, aml.ErrCodeInvalidResponse},
		{"clear WITH a match", &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomeClear, Matches: []aml.ScreeningMatch{{List: aml.ListSanctions}}}}, aml.ErrCodeInvalidResponse},
		{"clear WITH an error code", &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomeClear, ErrorCode: aml.ErrCodeTimeout}}, aml.ErrCodeInvalidResponse},
		{"match on a list that was not requested", &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomePotentialMatch, Matches: []aml.ScreeningMatch{{List: "watchlist"}}}}, aml.ErrCodeInvalidResponse},
		{"error outcome with a bogus code", &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomeError, ErrorCode: "weird"}}, aml.ErrCodeUnknown},
		{"error outcome with its code", &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomeError, ErrorCode: aml.ErrCodeRateLimited}}, aml.ErrCodeRateLimited},
	} {
		st := &fakeStore{policy: bothListsPolicy()}
		o := newOrch(t, c.p, st, now)
		out, err := o.Screen(context.Background(), actor(), req(uuid.New(), fresh))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if out.Outcome == aml.OutcomeClear || out.Outcome != aml.OutcomeError || out.ErrorCode != c.code {
			t.Errorf("%s: outcome %s / %s, want error / %s", c.name, out.Outcome, out.ErrorCode, c.code)
		}
		if out.Decision.Action != aml.HoldWithdrawalPayout || !out.Decision.Retry || out.Recorded.HoldID == nil {
			t.Errorf("%s: a NEW person with no completed result must be held and retried: %+v hold=%v", c.name, out.Decision, out.Recorded.HoldID)
		}
		if len(st.recorded) != 1 || st.recorded[0].Outcome != aml.OutcomeError || st.recorded[0].Decision != aml.HoldWithdrawalPayout {
			t.Errorf("%s: recorded %+v", c.name, st.recorded)
		}
	}
}

// An existing person (registered before effective_from) is not held for a missing result inside the grace period, and is held
// after it; the outcome is an error either way, never a clear.
func TestOrchestrator_ExistingPerson_GraceThenHold(t *testing.T) {
	p := bothListsPolicy()
	existing := effFrom.Add(-time.Hour)
	for _, c := range []struct {
		now  time.Time
		hold bool
	}{{effFrom.Add(p.BackfillGrace - time.Nanosecond), false}, {effFrom.Add(p.BackfillGrace), true}} {
		st := &fakeStore{policy: p}
		out, err := newOrch(t, &scripted{err: aml.ErrProviderUnavailable}, st, c.now).Screen(context.Background(), actor(), req(uuid.New(), existing))
		if err != nil || out.Outcome != aml.OutcomeError || (out.Recorded.HoldID != nil) != c.hold || !out.Decision.Retry {
			t.Errorf("now=%s: %+v err=%v hold=%v, want hold=%v", c.now, out, err, out.Recorded.HoldID, c.hold)
		}
	}
}

func TestOrchestrator_Matches_HoldAndCase(t *testing.T) {
	now := effFrom.Add(time.Hour)
	existing := effFrom.Add(-time.Hour)
	for _, c := range []struct {
		name    string
		res     aml.ScreeningResult
		outcome aml.ScreeningOutcome
		lists   []aml.ListType
		action  aml.HoldAction
		reason  string
	}{
		{"sanctions", aml.ScreeningResult{Outcome: aml.OutcomePotentialMatch, Matches: []aml.ScreeningMatch{{List: aml.ListSanctions, VendorMatchRef: "opaque"}}}, aml.OutcomePotentialMatch, []aml.ListType{aml.ListSanctions}, aml.HoldAllValueMovement, aml.ReasonSanctionsMatch},
		{"pep only", aml.ScreeningResult{Outcome: aml.OutcomePotentialMatch, Matches: []aml.ScreeningMatch{{List: aml.ListPEP}}}, aml.OutcomePotentialMatch, []aml.ListType{aml.ListPEP}, aml.HoldWithdrawalPayout, aml.ReasonPEPMatch},
		{"confirmed without naming the list", aml.ScreeningResult{Outcome: aml.OutcomeConfirmed}, aml.OutcomeConfirmed, []aml.ListType{aml.ListSanctions, aml.ListPEP}, aml.HoldAllValueMovement, aml.ReasonSanctionsConfirmed},
		// An ambiguous potential_match (no list named) is a match on EVERY requested list: the conservative reading.
		{"ambiguous potential match", aml.ScreeningResult{Outcome: aml.OutcomePotentialMatch}, aml.OutcomePotentialMatch, []aml.ListType{aml.ListSanctions, aml.ListPEP}, aml.HoldAllValueMovement, aml.ReasonSanctionsMatch},
	} {
		st := &fakeStore{policy: bothListsPolicy()}
		out, err := newOrch(t, &scripted{res: c.res}, st, now).Screen(context.Background(), actor(), req(uuid.New(), existing))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if out.Outcome != c.outcome || out.Decision.Action != c.action || out.Decision.ReasonCode != c.reason || out.Recorded.HoldID == nil || out.CaseID == nil || st.cases != 1 {
			t.Errorf("%s: %+v hold=%v case=%v cases=%d", c.name, out, out.Recorded.HoldID, out.CaseID, st.cases)
		}
		if len(st.recorded) != 1 || len(st.recorded[0].MatchLists) == 0 {
			t.Errorf("%s: a match must be recorded with its list(s): %+v", c.name, st.recorded)
		}
	}
	// A confirmed answer when sanctions was not even requested is not a conforming answer: error, not a silent clear.
	st := &fakeStore{policy: &aml.Policy{Lists: []aml.ListType{aml.ListPEP}, RescreenInterval: time.Hour, EffectiveFrom: effFrom}}
	out, err := newOrch(t, &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomeConfirmed}}, st, now).Screen(context.Background(), actor(), req(uuid.New(), effFrom.Add(time.Minute)))
	if err != nil || out.Outcome != aml.OutcomeError || out.ErrorCode != aml.ErrCodeInvalidResponse {
		t.Errorf("confirmed without sanctions requested: %+v %v", out, err)
	}
}

// A match is enforced even with no policy configured (the policy governs the error rule and the lists, not whether a match acts).
func TestOrchestrator_NoPolicy_MatchStillActsAndErrorDoesNotHold(t *testing.T) {
	now := effFrom.Add(time.Hour)
	r := req(uuid.New(), effFrom.Add(time.Minute))
	r.Lists = []aml.ListType{aml.ListSanctions}
	st := &fakeStore{}
	out, err := newOrch(t, &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomePotentialMatch, Matches: []aml.ScreeningMatch{{List: aml.ListSanctions}}}}, st, now).Screen(context.Background(), actor(), r)
	if err != nil || out.Decision.Action != aml.HoldAllValueMovement {
		t.Fatalf("match without policy: %+v %v", out, err)
	}
	r2 := req(uuid.New(), effFrom.Add(time.Minute))
	r2.Lists = []aml.ListType{aml.ListSanctions}
	st2 := &fakeStore{}
	out, err = newOrch(t, &scripted{err: aml.ErrProviderUnavailable}, st2, now).Screen(context.Background(), actor(), r2)
	if err != nil || out.Outcome != aml.OutcomeError || out.Decision.Action != aml.HoldNone || !out.Decision.Retry {
		t.Fatalf("error without policy is retry-only, never clear: %+v %v", out, err)
	}
	// No policy and no explicit lists: nothing to screen against; refused before any provider call.
	p := &scripted{}
	if _, err := newOrch(t, p, &fakeStore{}, now).Screen(context.Background(), actor(), req(uuid.New(), effFrom)); !errors.Is(err, aml.ErrInvalidInput) || p.calls != 0 {
		t.Fatalf("no policy, no lists: %v calls=%d", err, p.calls)
	}
}

// If the policy cannot be read the orchestrator refuses to run (it cannot apply the error rule): no provider call, no record.
func TestOrchestrator_PolicyReadFailure_RefusesBeforeTheProvider(t *testing.T) {
	p := &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomeClear}}
	st := &fakeStore{policyErr: errors.New("db down")}
	r := req(uuid.New(), effFrom)
	r.Lists = []aml.ListType{aml.ListSanctions} // a complete request: only the unreadable policy may stop it
	if _, err := newOrch(t, p, st, effFrom).Screen(context.Background(), actor(), r); err == nil || p.calls != 0 || len(st.recorded) != 0 {
		t.Fatalf("policy failure: err=%v calls=%d recorded=%d", err, p.calls, len(st.recorded))
	}
}

// If the record cannot be written, the caller gets a NotEnforcedError that names what must still be enforced; it is never a
// success, and a match is not lost silently.
func TestOrchestrator_RecordFailure_IsNotEnforcedError(t *testing.T) {
	now := effFrom.Add(time.Hour)
	st := &fakeStore{policy: bothListsPolicy(), recordErr: errors.New("write failed")}
	_, err := newOrch(t, &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomePotentialMatch, Matches: []aml.ScreeningMatch{{List: aml.ListSanctions}}}}, st, now).
		Screen(context.Background(), actor(), req(uuid.New(), effFrom.Add(-time.Hour)))
	var ne *aml.NotEnforcedError
	if !errors.As(err, &ne) || ne.Outcome != aml.OutcomePotentialMatch || ne.Decision.Action != aml.HoldAllValueMovement {
		t.Fatalf("want NotEnforcedError carrying the match, got %v", err)
	}
	// The same for an error outcome on a new person.
	st2 := &fakeStore{policy: bothListsPolicy(), recordErr: errors.New("write failed")}
	_, err = newOrch(t, &scripted{err: aml.ErrProviderUnavailable}, st2, now).Screen(context.Background(), actor(), req(uuid.New(), effFrom.Add(time.Minute)))
	if !errors.As(err, &ne) || ne.Outcome != aml.OutcomeError || ne.Decision.Action != aml.HoldWithdrawalPayout {
		t.Fatalf("want NotEnforcedError for the error outcome, got %v", err)
	}
}

// The hold is the enforcement; a case that cannot be opened is reported and never undoes or skips the hold.
func TestOrchestrator_CaseFailure_DoesNotLoseTheHold(t *testing.T) {
	st := &fakeStore{policy: bothListsPolicy(), caseErr: errors.New("quorum")}
	out, err := newOrch(t, &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomePotentialMatch, Matches: []aml.ScreeningMatch{{List: aml.ListSanctions}}}}, st, effFrom.Add(time.Hour)).
		Screen(context.Background(), actor(), req(uuid.New(), effFrom.Add(-time.Hour)))
	if err != nil || out.Recorded.HoldID == nil || out.CaseID != nil || out.CaseError == nil {
		t.Fatalf("hold stands, case error reported: %+v %v", out, err)
	}
}

// Idempotency end to end: the same key twice calls the provider once, records once, opens ONE case, and the second answer
// is flagged as a replay.
func TestOrchestrator_Idempotent_SameKeyOneProviderCallOneCase(t *testing.T) {
	m := aml.NewMockScreeningProvider()
	st := &fakeStore{policy: bothListsPolicy()}
	o := newOrch(t, m, st, effFrom.Add(time.Hour))
	r := req(uuid.New(), effFrom.Add(-time.Hour))
	r.VendorSubjectRef = aml.MockMarkerSanction + "7"
	a := actor()
	first, err := o.Screen(context.Background(), a, r)
	if err != nil || first.Recorded.Replayed || first.CaseID == nil {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := o.Screen(context.Background(), a, r)
	if err != nil || !second.Recorded.Replayed || second.CaseID != nil || second.Outcome != first.Outcome {
		t.Fatalf("second: %+v %v", second, err)
	}
	if m.Calls() != 1 || len(st.recorded) != 1 || st.cases != 1 {
		t.Fatalf("provider calls %d, records %d, cases %d, want 1/1/1", m.Calls(), len(st.recorded), st.cases)
	}
	// The same key with a different subject is refused by the provider (error outcome), never silently re-bound.
	r2 := r
	r2.VendorSubjectRef = aml.MockMarkerPEP + "9"
	out, err := o.Screen(context.Background(), a, r2)
	if err != nil || out.Outcome != aml.OutcomeError {
		t.Fatalf("key reuse with different content must not produce a normal outcome: %+v %v", out, err)
	}
}

// C7: the provider never sees a platform id, the stored reference is the same pseudonym, it is per tenant, and it is stable.
func TestOrchestrator_VendorNeverSeesPlatformIds(t *testing.T) {
	person := uuid.New()
	a := actor()
	p := &scripted{res: aml.ScreeningResult{Outcome: aml.OutcomeClear}}
	st := &fakeStore{policy: bothListsPolicy()}
	r := req(person, effFrom.Add(-time.Hour))
	if _, err := newOrch(t, p, st, effFrom.Add(time.Hour)).Screen(context.Background(), a, r); err != nil {
		t.Fatal(err)
	}
	got := p.got[0]
	for _, secret := range []string{person.String(), a.TenantID.String(), a.StaffID.String(), r.JurisdictionID.String(), strings.ReplaceAll(person.String(), "-", "")} {
		if strings.Contains(got.SubjectRef, secret) || strings.Contains(got.Subject.VendorSubjectRef, secret) || strings.Contains(got.IdempotencyKey, secret) {
			t.Errorf("the provider request leaks %s: %+v", secret, got)
		}
	}
	if st.recorded[0].SubjectRef != got.SubjectRef {
		t.Error("the stored subject reference must be the one the vendor saw")
	}
	k1, _ := aml.MockSubjectKeys{}.Key(context.Background(), a.TenantID)
	k2, _ := aml.MockSubjectKeys{}.Key(context.Background(), uuid.New())
	r1, _ := aml.SubjectRef(k1, person)
	r1b, _ := aml.SubjectRef(k1, person)
	r2, _ := aml.SubjectRef(k2, person)
	rOther, _ := aml.SubjectRef(k1, uuid.New())
	if r1 != r1b || r1 == r2 || r1 == rOther || len(r1) != 64 {
		t.Errorf("subject reference must be stable, per tenant and per person: %s %s %s %s", r1, r1b, r2, rOther)
	}
	if _, err := aml.SubjectRef([]byte("short"), person); err == nil {
		t.Error("a short key must be refused")
	}
}

func TestOrchestrator_RejectsMalformedRequestsBeforeTheProvider(t *testing.T) {
	now := effFrom.Add(time.Hour)
	for name, mut := range map[string]func(r *aml.ScreenRequest){
		"nil person":       func(r *aml.ScreenRequest) { r.PersonID = uuid.Nil },
		"nil jurisdiction": func(r *aml.ScreenRequest) { r.JurisdictionID = uuid.Nil },
		"bad trigger":      func(r *aml.ScreenRequest) { r.Trigger = "cron" },
		"no registered at": func(r *aml.ScreenRequest) { r.PersonRegisteredAt = time.Time{} },
		"bad key":          func(r *aml.ScreenRequest) { r.IdempotencyKey = "has space" },
		"empty key":        func(r *aml.ScreenRequest) { r.IdempotencyKey = "" },
	} {
		p := &scripted{}
		r := req(uuid.New(), effFrom)
		mut(&r)
		if _, err := newOrch(t, p, &fakeStore{policy: bothListsPolicy()}, now).Screen(context.Background(), actor(), r); !errors.Is(err, aml.ErrInvalidInput) || p.calls != 0 {
			t.Errorf("%s: err=%v calls=%d", name, err, p.calls)
		}
	}
	if _, err := aml.NewOrchestrator(aml.OrchestratorConfig{}); err == nil {
		t.Error("an unconfigured orchestrator must not build")
	}
}

// RecordScreeningInput.Validate mirrors the table CHECKs: a match can never be recorded as no action, a clear carries nothing,
// an error is never a clear.
func TestRecordScreeningInput_Validate_MirrorsTheChecks(t *testing.T) {
	good := aml.RecordScreeningInput{
		PersonID: uuid.New(), IdempotencyKey: "k1", Trigger: aml.TriggerManual, ProviderID: "mock-screening", SubjectRef: strings.Repeat("a", 64),
		Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone,
	}
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	with := func(f func(in *aml.RecordScreeningInput)) aml.RecordScreeningInput { in := good; f(&in); return in }
	for name, in := range map[string]aml.RecordScreeningInput{
		"clear with a hold":        with(func(in *aml.RecordScreeningInput) { in.Decision, in.HoldReasonCode = aml.HoldWithdrawalPayout, "x" }),
		"clear with a match":       with(func(in *aml.RecordScreeningInput) { in.MatchLists = []aml.ListType{aml.ListPEP} }),
		"clear with an error code": with(func(in *aml.RecordScreeningInput) { in.ErrorCode = aml.ErrCodeTimeout }),
		"error without a code":     with(func(in *aml.RecordScreeningInput) { in.Outcome = aml.OutcomeError }),
		"error with a bogus code":  with(func(in *aml.RecordScreeningInput) { in.Outcome, in.ErrorCode = aml.OutcomeError, "boom" }),
		"error with a match": with(func(in *aml.RecordScreeningInput) {
			in.Outcome, in.ErrorCode, in.MatchLists = aml.OutcomeError, aml.ErrCodeTimeout, []aml.ListType{aml.ListPEP}
		}),
		"match as no action": with(func(in *aml.RecordScreeningInput) {
			in.Outcome, in.MatchLists = aml.OutcomePotentialMatch, []aml.ListType{aml.ListPEP}
		}),
		"match without a list": with(func(in *aml.RecordScreeningInput) {
			in.Outcome, in.Decision, in.HoldReasonCode = aml.OutcomePotentialMatch, aml.HoldWithdrawalPayout, "x"
		}),
		"sanctions match with PEP scope": with(func(in *aml.RecordScreeningInput) {
			in.Outcome, in.MatchLists, in.Decision, in.HoldReasonCode = aml.OutcomePotentialMatch, []aml.ListType{aml.ListSanctions}, aml.HoldWithdrawalPayout, "x"
		}),
		"confirmed with PEP scope": with(func(in *aml.RecordScreeningInput) {
			in.Outcome, in.MatchLists, in.Decision, in.HoldReasonCode = aml.OutcomeConfirmed, []aml.ListType{aml.ListPEP}, aml.HoldWithdrawalPayout, "x"
		}),
		"hold decision without a reason": with(func(in *aml.RecordScreeningInput) {
			in.Outcome, in.ErrorCode, in.Decision = aml.OutcomeError, aml.ErrCodeTimeout, aml.HoldWithdrawalPayout
		}),
		"reason without a hold":   with(func(in *aml.RecordScreeningInput) { in.HoldReasonCode = "x" }),
		"unknown outcome":         with(func(in *aml.RecordScreeningInput) { in.Outcome = "approved" }),
		"uuid-shaped subject ref": with(func(in *aml.RecordScreeningInput) { in.SubjectRef = uuid.NewString() }),
		"unknown trigger":         with(func(in *aml.RecordScreeningInput) { in.Trigger = "cron" }),
		"zero screened time":      with(func(in *aml.RecordScreeningInput) { in.ScreenedAt = time.Time{} }),
		"nil person":              with(func(in *aml.RecordScreeningInput) { in.PersonID = uuid.Nil }),
		"unknown decision":        with(func(in *aml.RecordScreeningInput) { in.Decision = "freeze" }),
		// SR7-2: all-value-movement belongs to a confirmed outcome or a sanctions-list match only.
		"error with an all-value decision": with(func(in *aml.RecordScreeningInput) {
			in.Outcome, in.ErrorCode, in.Decision, in.HoldReasonCode = aml.OutcomeError, aml.ErrCodeTimeout, aml.HoldAllValueMovement, "x"
		}),
		"pep-only potential match with an all-value decision": with(func(in *aml.RecordScreeningInput) {
			in.Outcome, in.MatchLists, in.Decision, in.HoldReasonCode = aml.OutcomePotentialMatch, []aml.ListType{aml.ListPEP}, aml.HoldAllValueMovement, "x"
		}),
		"clear with an all-value decision": with(func(in *aml.RecordScreeningInput) { in.Decision, in.HoldReasonCode = aml.HoldAllValueMovement, "x" }),
	} {
		if err := in.Validate(); !errors.Is(err, aml.ErrInvalidInput) {
			t.Errorf("%s: want ErrInvalidInput, got %v", name, err)
		}
	}
}

// SR7-2 / SR7-8 / SR7-10: the hold class is derived strictly from the outcome and the list. ANY sanctions-list match is a
// four-eyes-lift class (confirmed_sanctions for a confirmed outcome, sanctions_potential otherwise); a PEP-only match, an error
// and a clear are manual.
func TestHoldReasonClassFor_AnySanctionsMatchIsAFourEyesClass(t *testing.T) {
	s, p := []aml.ListType{aml.ListSanctions}, []aml.ListType{aml.ListPEP}
	both := []aml.ListType{aml.ListPEP, aml.ListSanctions}
	for name, c := range map[string]struct {
		o    aml.ScreeningOutcome
		m    []aml.ListType
		want aml.ReasonClass
	}{
		"confirmed sanctions":          {aml.OutcomeConfirmed, s, aml.ReasonConfirmedSanctions},
		"confirmed sanctions and pep":  {aml.OutcomeConfirmed, both, aml.ReasonConfirmedSanctions},
		"confirmed pep only":           {aml.OutcomeConfirmed, p, aml.ReasonManual},
		"confirmed without a list":     {aml.OutcomeConfirmed, nil, aml.ReasonManual},
		"potential sanctions":          {aml.OutcomePotentialMatch, s, aml.ReasonSanctionsPotential},
		"potential sanctions and pep":  {aml.OutcomePotentialMatch, both, aml.ReasonSanctionsPotential},
		"potential pep":                {aml.OutcomePotentialMatch, p, aml.ReasonManual},
		"error":                        {aml.OutcomeError, nil, aml.ReasonManual},
		"error with a (bogus) match":   {aml.OutcomeError, s, aml.ReasonSanctionsPotential},
		"clear":                        {aml.OutcomeClear, nil, aml.ReasonManual},
		"unknown outcome with a match": {"approved", s, aml.ReasonSanctionsPotential},
	} {
		if got := aml.HoldReasonClassFor(c.o, c.m); got != c.want {
			t.Errorf("%s: %s, want %s", name, got, c.want)
		}
	}
}

// nopPool is a Pool that must never be reached (validation refuses first).
type nopPool struct{}

func (nopPool) WithPrincipalScope(context.Context, uuid.UUID, uuid.UUID, db.TxFunc) error {
	return errors.New("nop pool reached")
}
func (nopPool) WithPlatformActingInTenant(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, db.TxFunc) error {
	return errors.New("nop pool reached")
}

func TestSetScreeningPolicyInput_NoDefaultsAndLegalReferenceRequired(t *testing.T) {
	svc, err := aml.New(aml.Config{Pool: nopPool{}})
	if err != nil {
		t.Fatal(err)
	}
	ok := aml.SetScreeningPolicyInput{JurisdictionID: uuid.New(), Lists: []aml.ListType{aml.ListSanctions}, RescreenInterval: time.Hour, BackfillGrace: time.Minute,
		EffectiveFrom: effFrom, LegalReviewReference: "TEST FIXTURE REF", ReasonCode: "fixture"}
	a := aml.Actor{StaffID: uuid.New(), TenantID: uuid.New(), Scope: aml.ScopePlatformActing}
	mut := map[string]func(in *aml.SetScreeningPolicyInput){
		"no lists":         func(in *aml.SetScreeningPolicyInput) { in.Lists = nil },
		"three lists":      func(in *aml.SetScreeningPolicyInput) { in.Lists = []aml.ListType{"sanctions", "pep", "adverse"} },
		"duplicate list":   func(in *aml.SetScreeningPolicyInput) { in.Lists = []aml.ListType{"pep", "pep"} },
		"unknown list":     func(in *aml.SetScreeningPolicyInput) { in.Lists = []aml.ListType{"watch"} },
		"zero interval":    func(in *aml.SetScreeningPolicyInput) { in.RescreenInterval = 0 },
		"sub-second":       func(in *aml.SetScreeningPolicyInput) { in.RescreenInterval = 1500 * time.Millisecond },
		"negative grace":   func(in *aml.SetScreeningPolicyInput) { in.BackfillGrace = -time.Second },
		"no legal ref":     func(in *aml.SetScreeningPolicyInput) { in.LegalReviewReference = "" },
		"blank legal ref":  func(in *aml.SetScreeningPolicyInput) { in.LegalReviewReference = "   " },
		"padded legal ref": func(in *aml.SetScreeningPolicyInput) { in.LegalReviewReference = " ref" },
		"control char ref": func(in *aml.SetScreeningPolicyInput) { in.LegalReviewReference = "a\u202eb" },
		"no effective":     func(in *aml.SetScreeningPolicyInput) { in.EffectiveFrom = time.Time{} },
		"bad reason":       func(in *aml.SetScreeningPolicyInput) { in.ReasonCode = "Not A Token" },
		"no jurisdiction":  func(in *aml.SetScreeningPolicyInput) { in.JurisdictionID = uuid.Nil },
	}
	for name, f := range mut {
		in := ok
		f(&in)
		if _, err := svc.SetScreeningPolicy(context.Background(), a, in); !errors.Is(err, aml.ErrInvalidInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
