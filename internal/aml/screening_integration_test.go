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

func (w *amlWorld) jurisdiction() uuid.UUID {
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
	if _, err := svc.LiftHold(ctx, a, *out.Recorded.HoldID, "single_officer"); !errors.Is(err, aml.ErrHoldRefused) {
		t.Fatalf("a sanctions-list hold must not be lifted by a single officer: %v", err)
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
	// A manual-class screening hold CAN be lifted by one officer (plain officer lift), and then nothing is gated.
	if _, err := svc.LiftHold(ctx, a, *out.Recorded.HoldID, "false_positive"); err != nil {
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

// The screening hold does not wait for the officer quorum (ADR 0116 s6), while a MANUAL hold still does.
func TestScreening_HoldIsNotBlockedByAMissingQuorum(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t, "under_platform_licence")
	svc := amlfix.Service(t, w.RT)
	q := amlfix.Bootstrap(t, w.Owner, w.Tenant, 1)
	a := q.Officers[0].Actor
	p := w.player()
	if _, err := svc.PlaceHold(ctx, a, aml.PlaceHoldInput{PersonID: p.PersonID, Scope: aml.ScopeAllValueMovement, ReasonClass: aml.ReasonManual, ReasonCode: "t"}); !errors.Is(err, aml.ErrQuorum) {
		t.Fatalf("control: a manual hold needs the quorum: %v", err)
	}
	r := w.screeningReq(p, w.jurisdiction(), aml.MockMarkerSanction+"1")
	r.Lists = []aml.ListType{aml.ListSanctions}
	out, err := orchestrator(t, svc).Screen(ctx, a, r)
	if err != nil || out.Recorded.HoldID == nil {
		t.Fatalf("a screening match must hold without a quorum: %+v %v", out, err)
	}
	if held, _ := w.holdActive(p.PersonID, p.AccountID, aml.OpCasinoStake); !held {
		t.Fatal("the screening hold must gate")
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
	insert := func(outcome string, match, errCode, decision *string, holdID *uuid.UUID) error {
		err := w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			// The BEFORE INSERT guard compares the actor columns with a PROVEN session actor, which an owner-level fixture
			// insert does not have; it is disabled for this transaction only so that the CHECK constraints are what is tested.
			if _, err := tx.Exec(ctx, `ALTER TABLE aml_screening_results DISABLE TRIGGER aml_screening_results_10_guard`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, subject_ref, lists,
					outcome, match_lists, error_code, screened_at, decision, hold_id, recorded_by, recorded_by_person_id, recorded_by_scope)
				VALUES ($1, 'platform', $2, $3, 'manual', 'mock-screening', $4, ARRAY['sanctions','pep'], $5, $6::text[], $7, now(), $8, $9, $10, $11, 'platform_acting')`,
				w.Tenant, p.PersonID, "chk-"+uuid.NewString(), strings.Repeat("a", 64), outcome, match, errCode, *decision, holdID, q.Officers[0].StaffID, q.Officers[0].PersonID); err != nil {
				return err
			}
			return errRollback // the insert PASSED every constraint; never commit a fixture row
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
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
		"potential match recorded as no action":  {"potential_match", m("{pep}"), nil, none, nil},
		"confirmed under the withdrawal scope":   {"confirmed", m("{sanctions}"), nil, hwp, &hold},
		"sanctions match under the weaker scope": {"potential_match", m("{sanctions}"), nil, hwp, &hold},
		"a match without a list":                 {"potential_match", nil, nil, hall, &hold},
		"an error without its code":              {"error", nil, nil, none, nil},
		"a clear carrying an error code":         {"clear", nil, s("timeout"), none, nil},
		"a clear carrying a match":               {"clear", m("{pep}"), nil, none, nil},
		"a clear carrying a hold":                {"clear", nil, nil, hwp, &hold},
		"an error with a match":                  {"error", m("{pep}"), s("timeout"), none, nil},
		"a hold decision without the hold":       {"error", nil, s("timeout"), hwp, nil},
		"a no-action decision naming a hold":     {"error", nil, s("timeout"), none, &hold},
		"an outcome outside the vocabulary":      {"approved", nil, nil, none, nil},
		"an error code outside the vocabulary":   {"error", nil, s("weird"), none, nil},
		"a list outside the vocabulary":          {"potential_match", m("{watch}"), nil, hall, &hold},
	} {
		err := insert(c.outcome, c.match, c.errCode, c.decision, c.hold)
		if pgCode(err) != "23514" {
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
	} {
		if err := insert(c.outcome, c.match, c.errCode, c.decision, c.hold); err != nil {
			t.Errorf("%s: a lawful shape was refused: %v", name, err)
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
