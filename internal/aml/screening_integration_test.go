//go:build integration

package aml_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// ADR 0116 s4 / s7 (A7, migration 0154) against the REAL database as the RUNTIME role: policy versions, the screening record +
// hold in one transaction, idempotency, scope and tenant isolation, the fail-closed table CHECKs and the readiness reader.
// All values are TEST FIXTURE VALUES - NOT LEGAL THRESHOLDS.

// bareJurisdiction seeds a jurisdiction the tenant is NOT configured to operate in.
func (w *amlWorld) bareJurisdiction() uuid.UUID {
	w.t.Helper()
	jid := uuid.New()
	if err := w.Owner.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'aml screening fixture')`, jid, "AS-"+jid.String()[:12])
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction: %v", err)
	}
	return jid
}

// configure makes the tenant operate in jid (an active tenant_jurisdiction_configs row; effectiveTo nil = open ended).
func (w *amlWorld) configure(tenant, jid uuid.UUID, effectiveTo *time.Time) {
	w.t.Helper()
	if err := w.Owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, effective_from, effective_to)
			VALUES ($1, $2, 'kyc1', 'aml1', 'rg1', now() - interval '2 hours', $3)`, tenant, jid, effectiveTo)
		return err
	}); err != nil {
		w.t.Fatalf("seed jurisdiction config: %v", err)
	}
}

// jurisdiction seeds a jurisdiction the tenant operates in (SR7-3: a policy jurisdiction must be one the tenant is configured for).
func (w *amlWorld) jurisdiction() uuid.UUID {
	w.t.Helper()
	jid := w.bareJurisdiction()
	w.configure(w.Tenant, jid, nil)
	return jid
}

func policyInput(j uuid.UUID, from time.Time) aml.SetScreeningPolicyInput {
	return aml.SetScreeningPolicyInput{JurisdictionID: j, Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, RescreenInterval: 24 * time.Hour,
		BackfillGrace: time.Hour, EffectiveFrom: from, LegalReviewReference: "TEST FIXTURE REF - NOT LEGAL ADVICE", ReasonCode: "fixture_policy"}
}

// platformWorld is an under_platform_licence tenant with a two-officer PLATFORM quorum.
func platformWorld(t *testing.T) (*amlWorld, *amlfix.Quorum, *aml.Service) {
	t.Helper()
	w := newWorld(t, "under_platform_licence")
	return w, amlfix.Bootstrap(t, w.Owner, w.Tenant, 2), amlfix.Service(t, w.RT)
}

func TestScreeningPolicy_VersionsQuorumAndScope(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "under_platform_licence")
	svc := amlfix.Service(t, w.RT)
	j := w.jurisdiction()
	solo := amlfix.Bootstrap(t, w.Owner, w.Tenant, 1)
	a := solo.Officers[0].Actor

	// Unconfigured: nil, never a default.
	if p, err := svc.EffectiveScreeningPolicy(ctx, a, j); err != nil || p != nil {
		t.Fatalf("unconfigured must be nil: %+v %v", p, err)
	}
	// Below quorum nothing can be activated.
	if _, err := svc.SetScreeningPolicy(ctx, a, policyInput(j, time.Now().Add(-time.Hour))); !errors.Is(err, aml.ErrQuorum) {
		t.Fatalf("one officer is not a quorum: %v", err)
	}
	q := &amlfix.Quorum{Tenant: w.Tenant, OwnerScope: w.Scope, Officers: solo.Officers}
	q.Officers = append(q.Officers, amlfix.NewOfficer(t, w.Owner, w.Tenant))

	old := time.Now().Add(-2 * time.Hour)
	id1, err := svc.SetScreeningPolicy(ctx, a, policyInput(j, old))
	if err != nil || id1 == uuid.Nil {
		t.Fatalf("set policy: %v", err)
	}
	p, err := svc.EffectiveScreeningPolicy(ctx, a, j)
	if err != nil || p == nil || len(p.Lists) != 2 || p.RescreenInterval != 24*time.Hour || p.BackfillGrace != time.Hour || !p.EffectiveFrom.Equal(old.UTC().Truncate(time.Microsecond)) {
		t.Fatalf("effective policy: %+v %v", p, err)
	}
	// A FUTURE version is not effective yet; the previous one still is. A newer effective version wins.
	in := policyInput(j, time.Now().Add(time.Hour))
	in.Lists = []aml.ListType{aml.ListSanctions}
	if _, err := svc.SetScreeningPolicy(ctx, a, in); err != nil {
		t.Fatal(err)
	}
	if p, _ := svc.EffectiveScreeningPolicy(ctx, a, j); p == nil || len(p.Lists) != 2 {
		t.Fatalf("a future version must not be effective yet: %+v", p)
	}
	in2 := policyInput(j, time.Now().Add(-time.Minute))
	in2.Lists = []aml.ListType{aml.ListPEP}
	in2.RescreenInterval = 48 * time.Hour
	if _, err := svc.SetScreeningPolicy(ctx, a, in2); err != nil {
		t.Fatal(err)
	}
	if p, _ := svc.EffectiveScreeningPolicy(ctx, a, j); p == nil || len(p.Lists) != 1 || p.Lists[0] != aml.ListPEP || p.RescreenInterval != 48*time.Hour {
		t.Fatalf("the newest effective version must win: %+v", p)
	}
	// Another jurisdiction is unconfigured (policies are per jurisdiction).
	if p, _ := svc.EffectiveScreeningPolicy(ctx, a, w.jurisdiction()); p != nil {
		t.Fatalf("another jurisdiction must be unconfigured: %+v", p)
	}
	// A non-existent jurisdiction is refused by the foreign key (no orphan policy).
	if _, err := svc.SetScreeningPolicy(ctx, a, policyInput(uuid.New(), old)); err == nil {
		t.Fatal("a policy for an unknown jurisdiction must be refused")
	}
	// The policy table is not readable by the runtime role, and the reader is session-tenant bound.
	if err := w.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT count(*) FROM aml_screening_policies`)
		return err
	}); pgCode(err) != "42501" {
		t.Fatalf("the runtime role must have no privilege on the policy table, got %v", err)
	}
	if err := w.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var c int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_policy_effective($1, $2)`, uuid.New(), j).Scan(&c)
	}); pgCode(err) != "AM026" {
		t.Fatalf("a foreign tenant argument must be AM026, got %v", err)
	}
	if err := w.tenantTx(func(ctx context.Context, tx pgx.Tx) error {
		var c int
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_policy_effective($1, $2)`, w.Tenant, j).Scan(&c)
	}); err != nil {
		t.Fatalf("a tenant session reads its own effective policy: %v", err)
	}
}

// Screening configuration is platform-authored: a TENANT-scope officer of an own-licence tenant is refused even with a quorum.
func TestScreeningPolicy_TenantOfficerCannotAuthor(t *testing.T) {
	w := newWorld(t, "own_licence")
	svc := amlfix.Service(t, w.RT)
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	if _, err := svc.SetScreeningPolicy(context.Background(), q.Officers[0].Actor, policyInput(w.jurisdiction(), time.Now().Add(-time.Hour))); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a tenant officer must not author screening configuration: %v", err)
	}
}

func orchestrator(t *testing.T, svc *aml.Service) *aml.Orchestrator {
	t.Helper()
	o, err := aml.NewOrchestrator(aml.OrchestratorConfig{Provider: aml.NewMockScreeningProvider(), Store: svc, Keys: aml.MockSubjectKeys{}, CallTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func (w *amlWorld) screeningReq(p player, j uuid.UUID, vendorRef string) aml.ScreenRequest {
	return aml.ScreenRequest{PersonID: p.PersonID, JurisdictionID: j, PersonRegisteredAt: time.Now(), VendorSubjectRef: vendorRef,
		Trigger: aml.TriggerManual, IdempotencyKey: "it-" + uuid.NewString()}
}

func TestScreening_EndToEnd_OutcomesPlaceTheRequiredHolds(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	j := w.jurisdiction()
	a := q.Officers[0].Actor
	if _, err := svc.SetScreeningPolicy(ctx, a, policyInput(j, time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	o := orchestrator(t, svc)

	// Clear: recorded, no hold, nothing gated.
	pc := w.player()
	out, err := o.Screen(ctx, a, w.screeningReq(pc, j, "SYNTH-CLEAR-1"))
	if err != nil || out.Outcome != aml.OutcomeClear || out.Recorded.HoldID != nil || out.CaseID != nil {
		t.Fatalf("clear: %+v %v", out, err)
	}
	if held, _ := w.holdActive(pc.PersonID, pc.AccountID, aml.OpWithdrawal); held {
		t.Fatal("a clear must not hold")
	}

	// Sanctions potential match: ALL value movement held, a case opened, lifted only by four-eyes (class confirmed_sanctions).
	ps := w.player()
	out, err = o.Screen(ctx, a, w.screeningReq(ps, j, aml.MockMarkerSanction+"1"))
	if err != nil || out.Outcome != aml.OutcomePotentialMatch || out.Recorded.HoldID == nil || out.CaseID == nil || out.CaseError != nil {
		t.Fatalf("sanctions match: %+v %v", out, err)
	}
	for _, op := range allOps() {
		if held, err := w.holdActive(ps.PersonID, ps.AccountID, op); err != nil || !held {
			t.Errorf("sanctions match must gate %s: held=%v err=%v", op, held, err)
		}
	}
	// SR7-10 (CHANGED assertion, reverses the SR7-2 round-2 reading): a POTENTIAL sanctions match gates all value movement by
	// SCOPE and is the sanctions_potential class, which has the SAME four-eyes lift rule as confirmed_sanctions (ADR 0116 D6 /
	// s4.3); a single officer's plain lift is refused. The lift lifecycle itself is proven in TestSanctionsPotential_*.
	if hs, err := svc.ListHolds(ctx, a, ps.PersonID); err != nil || len(hs) != 1 || hs[0].ReasonClass != aml.ReasonSanctionsPotential || hs[0].Scope != aml.ScopeAllValueMovement {
		t.Fatalf("a potential sanctions match must be sanctions_potential with the all-value scope: %+v %v", hs, err)
	}
	if _, err := svc.LiftHold(ctx, a, *out.Recorded.HoldID, "false_positive_sanctions"); !errors.Is(err, aml.ErrHoldRefused) {
		t.Fatalf("a potential-sanctions hold must not be lifted by a single officer: %v", err)
	}

	// PEP potential match: withdrawal and payout only.
	pp := w.player()
	out, err = o.Screen(ctx, a, w.screeningReq(pp, j, aml.MockMarkerPEP+"1"))
	if err != nil || out.Decision.Action != aml.HoldWithdrawalPayout || out.CaseID == nil {
		t.Fatalf("pep match: %+v %v", out, err)
	}
	for op, want := range map[aml.Operation]bool{aml.OpWithdrawal: true, aml.OpPayout: true, aml.OpDeposit: false, aml.OpCasinoStake: false, aml.OpSportsbookStake: false} {
		if held, err := w.holdActive(pp.PersonID, pp.AccountID, op); err != nil || held != want {
			t.Errorf("pep match %s: held=%v err=%v want %v", op, held, err, want)
		}
	}
	// A manual-class screening hold CAN be lifted by one officer (plain officer lift) other than the one who recorded the result
	// (SR7-10, CHANGED: the recorder is refused, see TestScreeningHold_RecorderCannotLiftItWhateverTheClass), and then nothing is gated.
	if _, err := svc.LiftHold(ctx, q.Officers[1].Actor, *out.Recorded.HoldID, "false_positive"); err != nil {
		t.Fatalf("lift pep hold: %v", err)
	}
	if held, _ := w.holdActive(pp.PersonID, pp.AccountID, aml.OpWithdrawal); held {
		t.Fatal("a lifted hold must not gate")
	}

	// Confirmed: all value movement.
	pf := w.player()
	out, err = o.Screen(ctx, a, w.screeningReq(pf, j, aml.MockMarkerConfirmed+"1"))
	if err != nil || out.Outcome != aml.OutcomeConfirmed || out.Decision.Action != aml.HoldAllValueMovement {
		t.Fatalf("confirmed: %+v %v", out, err)
	}
	// The four-eyes class: a CONFIRMED sanctions outcome cannot be lifted by a single officer.
	if hs, err := svc.ListHolds(ctx, a, pf.PersonID); err != nil || len(hs) != 1 || hs[0].ReasonClass != aml.ReasonConfirmedSanctions {
		t.Fatalf("a confirmed sanctions outcome must be the four-eyes class: %+v %v", hs, err)
	}
	if _, err := svc.LiftHold(ctx, a, *out.Recorded.HoldID, "single_officer"); !errors.Is(err, aml.ErrHoldRefused) {
		t.Fatalf("a confirmed-sanctions hold must not be lifted by a single officer: %v", err)
	}

	// Unavailable vendor, person registered AFTER the policy took effect: held for withdrawal/payout, outcome error (never
	// clear), retry flagged. The deposit is not gated.
	pu := w.player()
	out, err = o.Screen(ctx, a, w.screeningReq(pu, j, aml.MockMarkerUnavailable))
	if err != nil || out.Outcome != aml.OutcomeError || out.ErrorCode != aml.ErrCodeUnavailable || !out.Decision.Retry || out.Recorded.HoldID == nil || out.CaseID != nil {
		t.Fatalf("unavailable: %+v %v", out, err)
	}
	if held, _ := w.holdActive(pu.PersonID, pu.AccountID, aml.OpWithdrawal); !held {
		t.Fatal("a new person with no completed screening must be held for withdrawal")
	}
	if held, _ := w.holdActive(pu.PersonID, pu.AccountID, aml.OpDeposit); held {
		t.Fatal("the unavailable-result hold must not gate deposits")
	}

	// A hung vendor times out (CallTimeout) and is the same fail-closed outcome.
	ph := w.player()
	out, err = o.Screen(ctx, a, w.screeningReq(ph, j, aml.MockMarkerSlow+"1"))
	if err != nil || out.Outcome != aml.OutcomeError || out.ErrorCode != aml.ErrCodeTimeout {
		t.Fatalf("timeout: %+v %v", out, err)
	}

	// The audit trail lists every attempt with its decision; no vendor text, no platform id in the stored pseudonym.
	rows, err := svc.ListScreenings(ctx, a, ps.PersonID)
	if err != nil || len(rows) != 1 || rows[0].Outcome != aml.OutcomePotentialMatch || rows[0].Decision != aml.HoldAllValueMovement || rows[0].HoldID == nil ||
		len(rows[0].MatchLists) != 1 || rows[0].MatchLists[0] != aml.ListSanctions || rows[0].ProviderID != aml.MockProviderID {
		t.Fatalf("audit trail: %+v %v", rows, err)
	}
	var ref string
	if err := w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT subject_ref FROM aml_screening_results WHERE person_id = $1`, ps.PersonID).Scan(&ref)
	}); err != nil {
		t.Fatal(err)
	}
	if len(ref) != 64 || strings.Contains(ref, strings.ReplaceAll(ps.PersonID.String(), "-", "")) {
		t.Fatalf("stored subject reference %q", ref)
	}
}

// SR7-2 (CHANGED from "the screening hold does not wait for the officer quorum"): recording a screening result that places a
// hold needs the officer quorum of the owner scope, like any other hold. With a single officer nothing is placed and the
// orchestrator reports the outcome as NOT ENFORCED (fail loud, retry), never as recorded. The ADR 0116 s6 exemption for holds
// placed automatically returns with the F1 system-actor path.
func TestScreening_HoldNeedsTheOfficerQuorum(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "under_platform_licence")
	svc := amlfix.Service(t, w.RT)
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 1)
	a := q.Officers[0].Actor
	p := w.player()
	r := w.screeningReq(p, w.jurisdiction(), aml.MockMarkerSanction+"1")
	r.Lists = []aml.ListType{aml.ListSanctions}
	out, err := orchestrator(t, svc).Screen(ctx, a, r)
	var ne *aml.NotEnforcedError
	if !errors.As(err, &ne) || !errors.Is(err, aml.ErrQuorum) {
		t.Fatalf("a hold-placing record without the quorum must be reported NOT ENFORCED (quorum): %+v %v", out, err)
	}
	if held, _ := w.holdActive(p.PersonID, p.AccountID, aml.OpCasinoStake); held {
		t.Fatal("nothing may have been placed without the quorum")
	}
	if rows, err := svc.ListScreenings(ctx, a, p.PersonID); err != nil || len(rows) != 0 {
		t.Fatalf("no result may be stored without its hold: %+v %v", rows, err)
	}
	// A record that places NO hold (a clear) needs no quorum.
	rc := w.screeningReq(p, w.jurisdiction(), "clean-subject")
	rc.Lists = []aml.ListType{aml.ListSanctions}
	if out, err := orchestrator(t, svc).Screen(ctx, a, rc); err != nil || out.Recorded.ResultID == uuid.Nil {
		t.Fatalf("a clear needs no quorum: %+v %v", out, err)
	}
	// With the quorum the same screening holds and gates.
	q2 := amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	r2 := w.screeningReq(p, w.jurisdiction(), aml.MockMarkerSanction+"1")
	r2.Lists = []aml.ListType{aml.ListSanctions}
	if out, err := orchestrator(t, svc).Screen(ctx, q2.Officers[0].Actor, r2); err != nil || out.Recorded.HoldID == nil {
		t.Fatalf("with the quorum the match must hold: %+v %v", out, err)
	}
	if held, _ := w.holdActive(p.PersonID, p.AccountID, aml.OpCasinoStake); !held {
		t.Fatal("the screening hold must gate")
	}
}

// SR7-2 repro: the database (function AND table CHECKs), not only the Go mirror, refuses to derive the four-eyes hold class from
// anything but a CONFIRMED sanctions-list outcome. The inputs below are ones Validate refuses, presented to the database directly.
func TestScreening_ClassDerivationIsStrict_InTheDatabase(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t) // a two-officer quorum: the quorum is NOT what refuses these
	a := q.Officers[0].Actor
	p := w.player()
	ref := strings.Repeat("e", 64)
	rec := func(key string, f func(in *aml.RecordScreeningInput)) error {
		in := aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: key, Trigger: aml.TriggerManual, ProviderID: aml.MockProviderID, SubjectRef: ref,
			Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, ScreenedAt: time.Now(), Decision: aml.HoldAllValueMovement, HoldReasonCode: "repro"}
		f(&in)
		if err := in.Validate(); err == nil {
			t.Errorf("%s: the Go mirror must refuse this input as well", key)
		}
		_, err := svc.RecordScreeningSkippingValidation(ctx, a, in)
		return err
	}
	cases := map[string]func(in *aml.RecordScreeningInput){
		// single officer, outcome error + an all-value decision: used to place a confirmed_sanctions hold
		"error with an all-value decision": func(in *aml.RecordScreeningInput) {
			in.Outcome, in.ErrorCode = aml.OutcomeError, aml.ErrCodeTimeout
		},
		// PEP-only potential match with an all-value decision
		"pep-only potential match with an all-value decision": func(in *aml.RecordScreeningInput) {
			in.Outcome, in.MatchLists = aml.OutcomePotentialMatch, []aml.ListType{aml.ListPEP}
		},
		// a clear with a hold
		"clear with an all-value decision": func(in *aml.RecordScreeningInput) { in.Outcome = aml.OutcomeClear },
	}
	for name, f := range cases {
		// The FUNCTION refuses it first (AM004 -> ErrInvalidInput); the table CHECK (23514) is the independent second line, proven
		// by TestScreeningResults_ChecksFailClosedInTheDatabase.
		if err := rec("sr72-"+uuid.NewString(), f); !errors.Is(err, aml.ErrInvalidInput) {
			t.Errorf("%s: the database function must refuse it (AM004), got %v", name, err)
		}
	}
	if hs, err := svc.ListHolds(ctx, a, p.PersonID); err != nil || len(hs) != 0 {
		t.Fatalf("none of the refused records may have left a hold: %+v %v", hs, err)
	}
	if rows, err := svc.ListScreenings(ctx, a, p.PersonID); err != nil || len(rows) != 0 {
		t.Fatalf("none of the refused records may have left a result: %+v %v", rows, err)
	}
	// The lawful derivations, and the class each one places.
	for name, c := range map[string]struct {
		outcome aml.ScreeningOutcome
		match   []aml.ListType
		want    aml.ReasonClass
		scope   aml.HoldScope
	}{
		"confirmed sanctions":         {aml.OutcomeConfirmed, []aml.ListType{aml.ListSanctions}, aml.ReasonConfirmedSanctions, aml.ScopeAllValueMovement},
		"potential sanctions":         {aml.OutcomePotentialMatch, []aml.ListType{aml.ListSanctions}, aml.ReasonSanctionsPotential, aml.ScopeAllValueMovement},
		"confirmed naming only a pep": {aml.OutcomeConfirmed, []aml.ListType{aml.ListPEP}, aml.ReasonManual, aml.ScopeAllValueMovement},
	} {
		pp := w.player()
		in := aml.RecordScreeningInput{PersonID: pp.PersonID, IdempotencyKey: "sr72-ok-" + uuid.NewString(), Trigger: aml.TriggerManual, ProviderID: aml.MockProviderID,
			SubjectRef: ref, Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, Outcome: c.outcome, MatchLists: c.match, ScreenedAt: time.Now(),
			Decision: aml.HoldAllValueMovement, HoldReasonCode: "repro_ok"}
		if got := aml.HoldReasonClassFor(c.outcome, c.match); got != c.want {
			t.Errorf("%s: Go class %s, want %s", name, got, c.want)
		}
		if _, err := svc.RecordScreening(ctx, a, in); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		hs, err := svc.ListHolds(ctx, a, pp.PersonID)
		if err != nil || len(hs) != 1 || hs[0].ReasonClass != c.want || hs[0].Scope != c.scope {
			t.Errorf("%s: placed %+v %v, want class %s scope %s", name, hs, err, c.want, c.scope)
		}
	}
	// PEP-only / unavailable: the manual class with the NARROWER scope.
	pp := w.player()
	if _, err := svc.RecordScreening(ctx, a, aml.RecordScreeningInput{PersonID: pp.PersonID, IdempotencyKey: "sr72-pep-" + uuid.NewString(), Trigger: aml.TriggerManual,
		ProviderID: aml.MockProviderID, SubjectRef: ref, Lists: []aml.ListType{aml.ListPEP}, Outcome: aml.OutcomePotentialMatch, MatchLists: []aml.ListType{aml.ListPEP},
		ScreenedAt: time.Now(), Decision: aml.HoldWithdrawalPayout, HoldReasonCode: aml.ReasonPEPMatch}); err != nil {
		t.Fatal(err)
	}
	if hs, err := svc.ListHolds(ctx, a, pp.PersonID); err != nil || len(hs) != 1 || hs[0].ReasonClass != aml.ReasonManual || hs[0].Scope != aml.ScopeWithdrawalAndPayout {
		t.Fatalf("pep-only hold: %+v %v", hs, err)
	}
}

func TestScreening_IdempotentReplayAndKeyReuse(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	a := q.Officers[0].Actor
	j := w.jurisdiction()
	o := orchestrator(t, svc)
	p := w.player()
	r := w.screeningReq(p, j, aml.MockMarkerSanction+"1")
	r.Lists = []aml.ListType{aml.ListSanctions}
	first, err := o.Screen(ctx, a, r)
	if err != nil || first.Recorded.Replayed {
		t.Fatalf("first: %+v %v", first, err)
	}
	second, err := o.Screen(ctx, a, r)
	if err != nil || !second.Recorded.Replayed || second.Recorded.ResultID != first.Recorded.ResultID || *second.Recorded.HoldID != *first.Recorded.HoldID || second.CaseID != nil {
		t.Fatalf("replay: %+v %v", second, err)
	}
	holds, err := svc.ListHolds(ctx, a, p.PersonID)
	if err != nil || len(holds) != 1 {
		t.Fatalf("a replay must place no second hold: %+v %v", holds, err)
	}
	// The same key for a DIFFERENT outcome is refused by the database (never silently re-bound).
	_, err = svc.RecordScreening(ctx, a, aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: r.IdempotencyKey, Trigger: aml.TriggerManual,
		ProviderID: aml.MockProviderID, SubjectRef: strings.Repeat("b", 64), Lists: r.Lists, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone})
	if !errors.Is(err, aml.ErrInvalidInput) {
		t.Fatalf("key reuse with different content: %v", err)
	}
}

// An officer cannot screen (and so cannot clear or hold) their OWN person: conflict of interest, the four-eyes class.
func TestScreening_OfficerCannotScreenOwnPerson(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	_ = w
	_, err := svc.RecordScreening(ctx, q.Officers[0].Actor, aml.RecordScreeningInput{PersonID: q.Officers[0].PersonID, IdempotencyKey: "own-1", Trigger: aml.TriggerManual,
		ProviderID: aml.MockProviderID, SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{aml.ListSanctions}, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone})
	if !errors.Is(err, aml.ErrFourEyes) {
		t.Fatalf("own person: %v", err)
	}
	// ... nor read the screening history of their own person.
	if _, err := svc.ListScreenings(ctx, q.Officers[0].Actor, q.Officers[0].PersonID); !errors.Is(err, aml.ErrFourEyes) {
		t.Fatalf("own person (read): %v", err)
	}
}

// The readiness reader sets a placeholder principal around ONE count; it must restore the session setting exactly (a changed
// setting would leak into the rest of the caller's transaction).
func TestAMLReadiness_RestoresTheSessionSettings(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "under_platform_licence")
	amlfix.Bootstrap(t, w.Owner, w.Tenant, 2)
	if err := w.RT.WithTenantReadOnly(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var before, after *string
		if err := tx.QueryRow(ctx, `SELECT current_setting('app.principal_id', true)`).Scan(&before); err != nil {
			return err
		}
		var met bool
		if err := tx.QueryRow(ctx, `SELECT o_quorum_met FROM aml_readiness($1, NULL)`, w.Tenant).Scan(&met); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT current_setting('app.principal_id', true)`).Scan(&after); err != nil {
			return err
		}
		if !met {
			t.Error("the quorum must be seen")
		}
		norm := func(s *string) string { // an unset setting and an empty one are the same to every policy (NULLIF(..., ''))
			if s == nil {
				return ""
			}
			return *s
		}
		if norm(before) != norm(after) {
			t.Errorf("app.principal_id before %q, after %q: the setting must be restored", norm(before), norm(after))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Scope and tenant isolation: another tenant's officer cannot read or write; a tenant-scope officer of a platform-licensed
// tenant sees no platform-owned result; a non-officer is forbidden.
func TestScreening_ScopeAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	a := q.Officers[0].Actor
	p := w.player()
	r := w.screeningReq(p, w.jurisdiction(), aml.MockMarkerSanction+"1")
	r.Lists = []aml.ListType{aml.ListSanctions}
	if _, err := orchestrator(t, svc).Screen(ctx, a, r); err != nil {
		t.Fatal(err)
	}
	// Another tenant's officer.
	w2, q2, _ := platformWorld(t)
	_ = w2
	foreign := q2.Officers[0].Actor
	foreign.TenantID = w.Tenant // the actor names a tenant it has no grant in
	if _, err := svc.ListScreenings(ctx, foreign, p.PersonID); err == nil {
		t.Fatal("an officer of another tenant must be refused")
	}
	own2 := q2.Officers[0].Actor
	if rows, err := svc.ListScreenings(ctx, own2, p.PersonID); err != nil || len(rows) != 0 {
		t.Fatalf("another tenant's officer sees nothing of this person: %+v %v", rows, err)
	}
	// A tenant-scope officer inside the platform-licensed tenant: platform-owned rows are invisible.
	staff, _ := amlfix.NewTenantComplianceStaff(t, w.Owner, w.Tenant)
	amlfix.GrantOfficerAs(t, w.Owner, w.Tenant, staff, "tenant")
	tenantOfficer := aml.Actor{StaffID: staff, TenantID: w.Tenant, Scope: aml.ScopeTenant}
	if rows, err := svc.ListScreenings(ctx, tenantOfficer, p.PersonID); err != nil || len(rows) != 0 {
		t.Fatalf("a tenant officer must not see platform-owned screening results: %+v %v", rows, err)
	}
	// ... and cannot write one for a platform-owned scope.
	_, err := svc.RecordScreening(ctx, tenantOfficer, aml.RecordScreeningInput{PersonID: p.PersonID, IdempotencyKey: "tenant-write", Trigger: aml.TriggerManual,
		ProviderID: aml.MockProviderID, SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{aml.ListSanctions}, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone})
	if !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a tenant officer must not record into the platform scope: %v", err)
	}
	// A non-officer (compliance staff without the grant) is forbidden.
	plain, _ := amlfix.NewTenantComplianceStaff(t, w.Owner, w.Tenant)
	if _, err := svc.ListScreenings(ctx, aml.Actor{StaffID: plain, TenantID: w.Tenant, Scope: aml.ScopeTenant}, p.PersonID); !errors.Is(err, aml.ErrForbidden) {
		t.Fatalf("a non-officer: %v", err)
	}
	// A person with no account in the tenant cannot be screened into it.
	if _, err := svc.RecordScreening(ctx, a, aml.RecordScreeningInput{PersonID: uuid.New(), IdempotencyKey: "ghost", Trigger: aml.TriggerManual,
		ProviderID: aml.MockProviderID, SubjectRef: strings.Repeat("a", 64), Lists: []aml.ListType{aml.ListSanctions}, Outcome: aml.OutcomeClear, ScreenedAt: time.Now(), Decision: aml.HoldNone}); err == nil {
		t.Fatal("an unknown person must be refused")
	}
}

// The table CHECKs are the last line: even an OWNER-level insert cannot record a match as "no action", an error as a clear, or a
// sanctions match under the weaker hold scope.
func TestScreeningResults_ChecksFailClosedInTheDatabase(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	p := w.player()
	// A valid hold to point decisions at.
	hold := q.PlaceHold(t, svc, p.PersonID, nil, aml.ScopeWithdrawalAndPayout, aml.ReasonManual)
	errRollback := errors.New("rolled back on purpose")
	// cls overrides the derived hold class (SR7-2) when a test needs to forge one; "" means "derive it as the function does".
	insertCls := func(cls string, outcome string, match, errCode, decision *string, holdID *uuid.UUID) error {
		err := w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			// The BEFORE INSERT guard compares the actor columns with a PROVEN session actor, which an owner-level fixture
			// insert does not have; it is disabled for this transaction only so that the CHECK constraints are what is tested.
			if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_results DISABLE TRIGGER aml_screening_results_10_guard`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, subject_ref, lists,
					outcome, match_lists, error_code, screened_at, decision, hold_id, hold_reason_class, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope)
				VALUES ($1, 'platform', $2, $3, 'manual', 'mock-screening', $4, ARRAY['sanctions','pep'], $5, $6::text[], $7, now(), $8, $9,
					CASE WHEN $12::text <> '' THEN $12::text WHEN $8::text = 'none' THEN NULL
					     WHEN COALESCE('sanctions' = ANY ($6::text[]), false) THEN CASE WHEN $5::text = 'confirmed' THEN 'confirmed_sanctions' ELSE 'sanctions_potential' END
					     ELSE 'manual' END,
					$13, $10, $11, 'platform_acting')`,
				w.Tenant, p.PersonID, "chk-"+uuid.NewString(), strings.Repeat("a", 64), outcome, match, errCode, *decision, holdID, q.Officers[0].StaffID, q.Officers[0].PersonID,
				cls, strings.Repeat("b", 64)); err != nil {
				return err
			}
			return errRollback // the insert PASSED every constraint; never commit a fixture row
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	insert := func(outcome string, match, errCode, decision *string, holdID *uuid.UUID) error {
		return insertCls("", outcome, match, errCode, decision, holdID)
	}
	s := func(v string) *string { return &v }
	m := func(v string) *string { return &v }
	none, hwp, hall := s("none"), s("hold_withdrawal_and_payout"), s("hold_all_value_movement")
	for name, c := range map[string]struct {
		outcome        string
		match, errCode *string
		decision       *string
		hold           *uuid.UUID
	}{
		"potential match recorded as no action":                         {"potential_match", m("{pep}"), nil, none, nil},
		"confirmed under the withdrawal scope":                          {"confirmed", m("{sanctions}"), nil, hwp, &hold},
		"sanctions match under the weaker scope":                        {"potential_match", m("{sanctions}"), nil, hwp, &hold},
		"confirmed naming only the PEP list under the withdrawal scope": {"confirmed", m("{pep}"), nil, hwp, &hold},
		"a decision outside the vocabulary":                             {"error", nil, s("timeout"), s("freeze"), &hold},
		"a match without a list":                                        {"potential_match", nil, nil, hall, &hold},
		"an error without its code":                                     {"error", nil, nil, none, nil},
		"a clear carrying an error code":                                {"clear", nil, s("timeout"), none, nil},
		"a clear carrying a match":                                      {"clear", m("{pep}"), nil, none, nil},
		"a clear carrying a hold":                                       {"clear", nil, nil, hwp, &hold},
		"an error with a match":                                         {"error", m("{pep}"), s("timeout"), none, nil},
		"a hold decision without the hold":                              {"error", nil, s("timeout"), hwp, nil},
		"a no-action decision naming a hold":                            {"error", nil, s("timeout"), none, &hold},
		"an outcome outside the vocabulary":                             {"approved", nil, nil, none, nil},
		"an error code outside the vocabulary":                          {"error", nil, s("weird"), none, nil},
		"a match list outside the vocabulary":                           {"potential_match", m("{watch}"), nil, hwp, &hold}, // hwp: no other CHECK may refuse it
		// SR7-2: all-value-movement is for a confirmed outcome or a sanctions-list match only.
		"an error under the all-value scope":                   {"error", nil, s("timeout"), hall, &hold},
		"a PEP-only potential match under the all-value scope": {"potential_match", m("{pep}"), nil, hall, &hold},
	} {
		err := insert(c.outcome, c.match, c.errCode, c.decision, c.hold)
		if pgCode(err) != "23514" {
			t.Errorf("%s: want a CHECK violation, got %v", name, err)
		}
	}
	// SR7-2 / SR7-8: the stored hold class is derived strictly from outcome and list. A forged class is a CHECK violation: the
	// four-eyes class for a potential match / a PEP-only confirmed outcome / an error, and the manual class for a confirmed
	// sanctions outcome, and a class without a hold or a hold without a class.
	for name, c := range map[string]struct {
		cls, outcome   string
		match, errCode *string
		decision       *string
		hold           *uuid.UUID
	}{
		"confirmed class on a potential sanctions match": {"confirmed_sanctions", "potential_match", m("{sanctions}"), nil, hall, &hold},
		"manual class on a potential sanctions match":    {"manual", "potential_match", m("{sanctions}"), nil, hall, &hold},
		"potential class on a confirmed sanctions":       {"sanctions_potential", "confirmed", m("{sanctions}"), nil, hall, &hold},
		"potential class on a PEP-only potential match":  {"sanctions_potential", "potential_match", m("{pep}"), nil, hwp, &hold},
		"potential class on an error":                    {"sanctions_potential", "error", nil, s("timeout"), hwp, &hold},
		"four-eyes class on a PEP-only confirmed":        {"confirmed_sanctions", "confirmed", m("{pep}"), nil, hall, &hold},
		"four-eyes class on an error":                    {"confirmed_sanctions", "error", nil, s("timeout"), hwp, &hold},
		"manual class on a confirmed sanctions outcome":  {"manual", "confirmed", m("{sanctions}"), nil, hall, &hold},
		"a class on a no-action decision":                {"manual", "error", nil, s("timeout"), none, nil},
		"an unknown class":                               {"quorum", "error", nil, s("timeout"), hwp, &hold},
	} {
		if err := insertCls(c.cls, c.outcome, c.match, c.errCode, c.decision, c.hold); pgCode(err) != "23514" {
			t.Errorf("%s: want a CHECK violation, got %v", name, err)
		}
	}
	// Controls: the lawful shapes pass the CHECKs (with the actor guard disabled).
	for name, c := range map[string]struct {
		outcome        string
		match, errCode *string
		decision       *string
		hold           *uuid.UUID
	}{
		"error with no action (grace)": {"error", nil, s("timeout"), none, nil},
		"clear":                        {"clear", nil, nil, none, nil},
		"pep match":                    {"potential_match", m("{pep}"), nil, hwp, &hold},
		"potential sanctions (4-eyes)": {"potential_match", m("{sanctions}"), nil, hall, &hold},
		"confirmed sanctions (4-eyes)": {"confirmed", m("{sanctions}"), nil, hall, &hold},
		"confirmed pep only (manual)":  {"confirmed", m("{pep}"), nil, hall, &hold},
	} {
		if err := insert(c.outcome, c.match, c.errCode, c.decision, c.hold); err != nil {
			t.Errorf("%s: a lawful shape was refused: %v", name, err)
		}
	}
	// Shape CHECKs of the identifying columns: a platform-id-shaped pseudonym, a malformed key / provider / reference / trigger, an
	// out-of-vocabulary list, an owner scope that disagrees with the recording scope.
	raw := func(col, val string) error {
		cols := map[string]string{"trigger": "manual", "idempotency_key": "shape-" + uuid.NewString(), "provider_id": "mock-screening",
			"provider_reference": "ok-1", "subject_ref": strings.Repeat("a", 64), "owner_scope": "platform", "recorded_by_scope": "platform_acting", "lists": "{sanctions}", "payload_hash": strings.Repeat("b", 64)}
		cols[col] = val
		err := w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_results DISABLE TRIGGER aml_screening_results_10_guard`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, provider_reference, subject_ref, lists,
					outcome, screened_at, decision, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $12::text[], 'clear', now(), 'none', $13, $9, $10, $11)`,
				w.Tenant, cols["owner_scope"], p.PersonID, cols["idempotency_key"], cols["trigger"], cols["provider_id"], cols["provider_reference"], cols["subject_ref"],
				q.Officers[0].StaffID, q.Officers[0].PersonID, cols["recorded_by_scope"], cols["lists"], cols["payload_hash"]); err != nil {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	if err := raw("trigger", "manual"); err != nil {
		t.Fatalf("control (lawful shape): %v", err)
	}
	for name, c := range map[string][2]string{
		"unknown trigger":                 {"trigger", "cron"},
		"key with a space":                {"idempotency_key", "has space"},
		"empty key":                       {"idempotency_key", ""},
		"uuid-shaped pseudonym":           {"subject_ref", uuid.NewString()},
		"uppercase pseudonym":             {"subject_ref", strings.ToUpper(strings.Repeat("a", 64))},
		"short pseudonym":                 {"subject_ref", "abc"},
		"provider id with a space":        {"provider_id", "Mock Screening"},
		"provider reference with a space": {"provider_reference", "a b"},
		"owner scope outside the set":     {"owner_scope", "everyone"},
		"scope disagreeing with owner":    {"recorded_by_scope", "tenant"},
		"recording scope outside the set": {"recorded_by_scope", "everyone"},
		"a list outside the vocabulary":   {"lists", "{watch}"},
		"a short payload digest":          {"payload_hash", "abc"},
		"an uppercase payload digest":     {"payload_hash", strings.ToUpper(strings.Repeat("b", 64))},
		"three lists":                     {"lists", "{sanctions,pep,adverse}"},
		"no list":                         {"lists", "{}"},
	} {
		if err := raw(c[0], c[1]); pgCode(err) != "23514" {
			t.Errorf("%s: want a CHECK violation, got %v", name, err)
		}
	}
	// Append-only, even for the owner.
	if err := w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE aml_screening_results SET outcome = outcome`)
		return err
	}); err != nil && pgCode(err) != "AM005" && pgCode(err) != "42501" {
		t.Fatalf("update must be refused: %v", err)
	}
}

// S7 readiness reader: coarse, session-tenant bound, and it follows the quorum and the policies.
func TestAMLReadiness_QuorumAndPolicyCounts(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "under_platform_licence")
	svc := amlfix.Service(t, w.RT)
	j1, j2 := w.jurisdiction(), w.jurisdiction()
	read := func() (scope string, quorum bool, total, with int) {
		t.Helper()
		if err := w.RT.WithTenantReadOnly(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT o_owner_scope, o_quorum_met, o_jurisdictions_total, o_jurisdictions_with_policy FROM aml_readiness($1, $2)`,
				w.Tenant, []uuid.UUID{j1, j2, j1}).Scan(&scope, &quorum, &total, &with)
		}); err != nil {
			t.Fatalf("readiness: %v", err)
		}
		return
	}
	if s, q, tot, with := read(); s != "platform" || q || tot != 2 || with != 0 {
		t.Fatalf("empty: %s %v %d %d", s, q, tot, with)
	}
	one := amlfix.Bootstrap(t, w.Owner, w.Tenant, 1)
	if _, q, _, _ := read(); q {
		t.Fatal("one officer is not a quorum")
	}
	two := append(one.Officers, amlfix.NewOfficer(t, w.Owner, w.Tenant))
	if _, q, _, _ := read(); !q {
		t.Fatal("two distinct officers are a quorum")
	}
	// A TENANT-scope officer in a platform-licensed tenant is not a member of the PLATFORM quorum.
	if _, err := svc.SetScreeningPolicy(ctx, two[0].Actor, policyInput(j1, time.Now().Add(-time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SetScreeningPolicy(ctx, two[0].Actor, policyInput(j2, time.Now().Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, q, tot, with := read(); !q || tot != 2 || with != 1 {
		t.Fatalf("one effective policy of two: q=%v total=%d with=%d", q, tot, with)
	}
	amlfix.RevokeOfficer(t, w.Owner, w.Tenant, two[1].StaffID)
	if _, q, _, _ := read(); q {
		t.Fatal("a revoked officer leaves the quorum")
	}
	// Session-tenant bound: another tenant id is refused, never a silently filtered answer.
	err := w.RT.WithTenantReadOnly(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var s string
		return tx.QueryRow(ctx, `SELECT o_owner_scope FROM aml_readiness($1, NULL)`, uuid.New()).Scan(&s)
	})
	if pgCode(err) != "AM026" {
		t.Fatalf("a foreign tenant must be AM026, got %v", err)
	}
	// A NULL jurisdiction list reads zero of zero (no division, no crash).
	if err := w.RT.WithTenantReadOnly(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		var tot, with int
		if err := tx.QueryRow(ctx, `SELECT o_jurisdictions_total, o_jurisdictions_with_policy FROM aml_readiness($1, NULL)`, w.Tenant).Scan(&tot, &with); err != nil {
			return err
		}
		if tot != 0 || with != 0 {
			t.Errorf("null list: %d %d", tot, with)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Only officers of the tenant's OWNER scope count toward its readiness quorum: tenant-scope officers do not make a
// platform-licensed tenant ready, and platform officers do not make an own-licence tenant ready.
func TestAMLReadiness_OwnerScopeDecidesWhichOfficersCount(t *testing.T) {
	ctx := context.Background()
	quorum := func(w *amlWorld) bool {
		t.Helper()
		var q bool
		if err := w.RT.WithTenantReadOnly(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT o_quorum_met FROM aml_readiness($1, NULL)`, w.Tenant).Scan(&q)
		}); err != nil {
			t.Fatalf("readiness: %v", err)
		}
		return q
	}
	// Platform-licensed: one platform officer + one tenant-scope officer is NOT a platform quorum.
	pw := newWorld(t, "under_platform_licence")
	amlfix.Bootstrap(t, pw.Owner, pw.Tenant, 1)
	ts, _ := amlfix.NewTenantComplianceStaff(t, pw.Owner, pw.Tenant)
	amlfix.GrantOfficerAs(t, pw.Owner, pw.Tenant, ts, "tenant")
	if quorum(pw) {
		t.Fatal("a tenant-scope officer must not count toward a platform-owned tenant's quorum")
	}
	amlfix.NewOfficer(t, pw.Owner, pw.Tenant)
	if !quorum(pw) {
		t.Fatal("two platform officers are a platform quorum")
	}
	// Own-licence: tenant officers count; platform officers do not.
	ow := newWorld(t, "own_licence")
	amlfix.Bootstrap(t, ow.Owner, ow.Tenant, 1)
	ps, _ := amlfix.NewPlatformStaff(t, ow.Owner)
	amlfix.GrantOfficerAs(t, ow.Owner, ow.Tenant, ps, "platform")
	if quorum(ow) {
		t.Fatal("a platform officer must not count toward an own-licence tenant's quorum")
	}
	amlfix.NewOfficer(t, ow.Owner, ow.Tenant)
	if !quorum(ow) {
		t.Fatal("two tenant officers are a tenant quorum")
	}
}

// SR7-3: the policy jurisdiction must be one the tenant operates in. An existing jurisdiction the tenant has no ACTIVE
// configuration for (never configured, configuration ended, configured for ANOTHER tenant) is refused and nothing is stored;
// the configured one is accepted.
func TestScreeningPolicy_JurisdictionMustBeOneTheTenantOperatesIn(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	a := q.Officers[0].Actor
	other := newWorld(t, "under_platform_licence")
	past := time.Now().Add(-time.Hour)

	never := w.bareJurisdiction()
	ended := w.bareJurisdiction()
	w.configure(w.Tenant, ended, &past)
	foreign := w.bareJurisdiction()
	w.configure(other.Tenant, foreign, nil)
	for name, j := range map[string]uuid.UUID{"never configured": never, "configuration ended": ended, "configured for another tenant": foreign} {
		if _, err := svc.SetScreeningPolicy(ctx, a, policyInput(j, time.Now().Add(-time.Hour))); !errors.Is(err, aml.ErrInvalidInput) {
			t.Errorf("%s: want ErrInvalidInput, got %v", name, err)
		}
		if p, err := svc.EffectiveScreeningPolicy(ctx, a, j); err != nil || p != nil {
			t.Errorf("%s: nothing may be stored: %+v %v", name, p, err)
		}
	}
	// The session tenant setting of the caller is restored around the check: the configured jurisdiction is accepted afterwards.
	ok := w.jurisdiction()
	if _, err := svc.SetScreeningPolicy(ctx, a, policyInput(ok, time.Now().Add(-time.Hour))); err != nil {
		t.Fatalf("a configured jurisdiction must be accepted: %v", err)
	}
	if p, err := svc.EffectiveScreeningPolicy(ctx, a, ok); err != nil || p == nil {
		t.Fatalf("the accepted policy must be readable: %+v %v", p, err)
	}
}
