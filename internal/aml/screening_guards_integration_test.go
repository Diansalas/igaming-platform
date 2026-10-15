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
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// A replayed key is compared on EVERY identifying field: the same key with a different person, outcome, decision or pseudonym is
// refused (never silently re-bound), while an identical replay is accepted.
func TestScreening_Replay_EveryFieldIsCompared(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	a := q.Officers[0].Actor
	p1, p2 := w.player(), w.player()
	ref := strings.Repeat("c", 64)
	at := time.Now().UTC().Truncate(time.Microsecond) // fixed: a replay compares the screened time too
	base := func(key string, f func(in *aml.RecordScreeningInput)) aml.RecordScreeningInput {
		in := aml.RecordScreeningInput{PersonID: p1.PersonID, IdempotencyKey: key, Trigger: aml.TriggerManual, ProviderID: aml.MockProviderID, SubjectRef: ref,
			Lists: []aml.ListType{aml.ListSanctions, aml.ListPEP}, Outcome: aml.OutcomePotentialMatch, MatchLists: []aml.ListType{aml.ListPEP},
			ScreenedAt: at, Decision: aml.HoldWithdrawalPayout, HoldReasonCode: aml.ReasonPEPMatch}
		if f != nil {
			f(&in)
		}
		return in
	}
	k1, k2 := "rp-"+uuid.NewString(), "rp-"+uuid.NewString()
	first, err := svc.RecordScreening(ctx, a, base(k1, nil))
	if err != nil || first.Replayed || first.HoldID == nil {
		t.Fatalf("first: %+v %v", first, err)
	}
	if again, err := svc.RecordScreening(ctx, a, base(k1, nil)); err != nil || !again.Replayed || again.ResultID != first.ResultID {
		t.Fatalf("an identical replay is accepted: %+v %v", again, err)
	}
	// decision only (stricter scope, same outcome, same pseudonym)
	if _, err := svc.RecordScreening(ctx, a, base(k1, func(in *aml.RecordScreeningInput) { in.Decision = aml.HoldAllValueMovement })); !errors.Is(err, aml.ErrInvalidInput) {
		t.Errorf("decision differs: %v", err)
	}
	// pseudonym only
	if _, err := svc.RecordScreening(ctx, a, base(k1, func(in *aml.RecordScreeningInput) { in.SubjectRef = strings.Repeat("d", 64) })); !errors.Is(err, aml.ErrInvalidInput) {
		t.Errorf("subject reference differs: %v", err)
	}
	// person only
	if _, err := svc.RecordScreening(ctx, a, base(k1, func(in *aml.RecordScreeningInput) { in.PersonID = p2.PersonID })); !errors.Is(err, aml.ErrInvalidInput) {
		t.Errorf("person differs: %v", err)
	}
	// outcome only: a sanctions potential match, then the same content as a confirmed outcome (same scope, same pseudonym)
	sanc := func(o aml.ScreeningOutcome) aml.RecordScreeningInput {
		return base(k2, func(in *aml.RecordScreeningInput) {
			in.Outcome, in.MatchLists, in.Decision, in.HoldReasonCode = o, []aml.ListType{aml.ListSanctions}, aml.HoldAllValueMovement, aml.ReasonSanctionsMatch
			in.PersonID = p2.PersonID
		})
	}
	if _, err := svc.RecordScreening(ctx, a, sanc(aml.OutcomePotentialMatch)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordScreening(ctx, a, sanc(aml.OutcomeConfirmed)); !errors.Is(err, aml.ErrInvalidInput) {
		t.Errorf("outcome differs: %v", err)
	}
	// SR7-7: EVERY remaining field is compared on replay, one at a time (each variant is otherwise valid, so the refusal is the
	// replay comparison and not a shape rule): trigger, provider, provider reference, the requested lists, the screened time and
	// the hold reason code.
	for name, f := range map[string]func(in *aml.RecordScreeningInput){
		"trigger":            func(in *aml.RecordScreeningInput) { in.Trigger = aml.TriggerScheduled },
		"provider id":        func(in *aml.RecordScreeningInput) { in.ProviderID = "other-provider" },
		"provider reference": func(in *aml.RecordScreeningInput) { in.ProviderReference = "ref-2" },
		"requested lists":    func(in *aml.RecordScreeningInput) { in.Lists = []aml.ListType{aml.ListPEP} },
		"screened time":      func(in *aml.RecordScreeningInput) { in.ScreenedAt = at.Add(time.Second) },
		"hold reason code":   func(in *aml.RecordScreeningInput) { in.HoldReasonCode = aml.ReasonUnavailable },
	} {
		if _, err := svc.RecordScreening(ctx, a, base(k1, f)); !errors.Is(err, aml.ErrInvalidInput) {
			t.Errorf("%s differs: want the idempotency refusal, got %v", name, err)
		}
	}
	// match lists only (same sanctions-grade decision), and error code only (an error record, same decision and reason).
	if _, err := svc.RecordScreening(ctx, a, sanc(aml.OutcomePotentialMatch)); err != nil {
		t.Fatalf("identical sanctions replay: %v", err)
	}
	if _, err := svc.RecordScreening(ctx, a, base(k2, func(in *aml.RecordScreeningInput) {
		in.PersonID = p2.PersonID
		in.Outcome, in.MatchLists, in.Decision, in.HoldReasonCode = aml.OutcomePotentialMatch, []aml.ListType{aml.ListSanctions, aml.ListPEP}, aml.HoldAllValueMovement, aml.ReasonSanctionsMatch
	})); !errors.Is(err, aml.ErrInvalidInput) {
		t.Errorf("match lists differ: %v", err)
	}
	k3 := "rp-" + uuid.NewString()
	errRec := func(code aml.ScreeningErrorCode) aml.RecordScreeningInput {
		return base(k3, func(in *aml.RecordScreeningInput) {
			in.PersonID = p2.PersonID
			in.Outcome, in.MatchLists, in.ErrorCode, in.HoldReasonCode = aml.OutcomeError, nil, code, aml.ReasonUnavailable
		})
	}
	if _, err := svc.RecordScreening(ctx, a, errRec(aml.ErrCodeTimeout)); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordScreening(ctx, a, errRec(aml.ErrCodeRateLimited)); !errors.Is(err, aml.ErrInvalidInput) {
		t.Errorf("error code differs: %v", err)
	}
	// none of the refused replays placed anything extra
	if hs, err := svc.ListHolds(ctx, a, p1.PersonID); err != nil || len(hs) != 1 {
		t.Fatalf("p1 holds: %+v %v", hs, err)
	}
	if hs, err := svc.ListHolds(ctx, a, p2.PersonID); err != nil || len(hs) != 2 {
		t.Fatalf("p2 holds: %+v %v", hs, err)
	}
}

// The BEFORE INSERT guards are an independent second check behind the definer functions: an owner-level insert that skips the
// function still cannot write a result for another owner scope, for a person with no account in the tenant, or naming another
// person's hold; and neither table accepts a row whose actor columns are not a proven session actor.
func TestScreening_Guards_AreIndependentOfTheDefinerFunctions(t *testing.T) {
	ctx := context.Background()
	w, q, svc := platformWorld(t)
	p, p2 := w.player(), w.player()
	otherHold := q.PlaceHold(t, svc, p2.PersonID, nil, aml.ScopeWithdrawalAndPayout, aml.ReasonManual)
	var ghost uuid.UUID // a Person with NO account in this tenant
	if err := w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		pp, err := identity.CreatePerson(ctx, tx)
		ghost = pp.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ins := func(owner string, person uuid.UUID, hold *uuid.UUID) error {
		decision := "none"
		if hold != nil {
			decision = "hold_withdrawal_and_payout"
		}
		return w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, subject_ref, lists,
					outcome, error_code, screened_at, decision, hold_id, hold_reason_class, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope)
				VALUES ($1, $2, $3, $4, 'manual', 'mock-screening', $5, ARRAY['pep'], 'error', 'timeout', now(), $6, $7,
					CASE WHEN $6::text = 'none' THEN NULL ELSE 'manual' END, $10, $8, $9, 'platform_acting')`,
				w.Tenant, owner, person, "g-"+uuid.NewString(), strings.Repeat("a", 64), decision, hold, q.Officers[0].StaffID, q.Officers[0].PersonID,
				strings.Repeat("b", 64))
			return err
		})
	}
	// owner_scope 'tenant' disagrees with the recording scope (platform_acting): the CHECK or the owner-scope guard, whichever
	// the engine raises first (the guard fires first: it is a BEFORE trigger).
	if err := ins("tenant", p.PersonID, nil); pgCode(err) != "AM002" {
		t.Errorf("a tenant-owner row in a platform-owned tenant: %v", err)
	}
	if err := ins("platform", ghost, nil); pgCode(err) != "AM007" {
		t.Errorf("a person with no account in the tenant: %v", err)
	}
	if err := ins("platform", p.PersonID, &otherHold); pgCode(err) != "AM007" {
		t.Errorf("another person's hold: %v", err)
	}
	// SR7-2: the named hold must also carry the class and the scope the result's decision derives. Own person's manual
	// withdrawal-scope hold: a result claiming the four-eyes class (confirmed sanctions) or the all-value scope is refused AM007.
	ownHold := q.PlaceHold(t, svc, p.PersonID, nil, aml.ScopeWithdrawalAndPayout, aml.ReasonManual)
	claim := func(hold uuid.UUID, outcome, match, decision, cls string) error {
		return w.Owner.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, subject_ref, lists,
					outcome, match_lists, screened_at, decision, hold_id, hold_reason_class, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope)
				VALUES ($1, 'platform', $2, $3, 'manual', 'mock-screening', $4, ARRAY['sanctions','pep'], $5, $6::text[], now(), $7, $8, $9, $10, $11, $12, 'platform_acting')`,
				w.Tenant, p.PersonID, "gc-"+uuid.NewString(), strings.Repeat("a", 64), outcome, match, decision, hold, cls, strings.Repeat("b", 64),
				q.Officers[0].StaffID, q.Officers[0].PersonID)
			return err
		})
	}
	if err := claim(ownHold, "confirmed", "{sanctions}", "hold_all_value_movement", "confirmed_sanctions"); pgCode(err) != "AM007" {
		t.Errorf("a hold of the wrong class and scope behind a confirmed result: %v", err)
	}
	if err := claim(ownHold, "potential_match", "{sanctions}", "hold_all_value_movement", "sanctions_potential"); pgCode(err) != "AM007" {
		t.Errorf("a withdrawal-scope hold behind an all-value decision: %v", err)
	}
	// The SCOPE matches (all value movement) but the CLASS does not: a manual all-value hold behind a confirmed-sanctions result.
	manualAll := q.PlaceHold(t, svc, p.PersonID, nil, aml.ScopeAllValueMovement, aml.ReasonManual)
	if err := claim(manualAll, "confirmed", "{sanctions}", "hold_all_value_movement", "confirmed_sanctions"); pgCode(err) != "AM007" {
		t.Errorf("a manual-class hold behind a confirmed-sanctions result (same scope): %v", err)
	}
	// SR7-10: a potential sanctions result behind a manual all-value hold (right scope, wrong class) is refused too.
	if err := claim(manualAll, "potential_match", "{sanctions}", "hold_all_value_movement", "sanctions_potential"); pgCode(err) != "AM007" {
		t.Errorf("a manual-class hold behind a sanctions_potential result (same scope): %v", err)
	}
	// With everything lawful but no PROVEN session actor at all the insert is refused.
	if err := ins("platform", p.PersonID, nil); err == nil {
		t.Error("a row with no proven session actor must be refused")
	}

	// Inside a genuine acting session (the officer's own, validated by the database) the actor-column comparison itself is
	// what is tested: the proven actor's ids insert; any other staff id, person id or scope is refused AM001.
	j := w.jurisdiction()
	errRollback := errors.New("rolled back on purpose")
	officer := q.Officers[0]
	asOfficer := func(fn func(ctx context.Context, tx pgx.Tx) error) error {
		err := w.Owner.WithPlatformActingInTenant(ctx, officer.StaffID, w.Tenant, uuid.Nil, "compliance", func(ctx context.Context, tx pgx.Tx) error {
			if err := fn(ctx, tx); err != nil {
				return err
			}
			return errRollback
		})
		if errors.Is(err, errRollback) {
			return nil
		}
		return err
	}
	insResult := func(by, byPerson uuid.UUID, scope string) error {
		return asOfficer(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_screening_results (tenant_id, owner_scope, person_id, idempotency_key, trigger, provider_id, subject_ref, lists,
					outcome, error_code, screened_at, decision, payload_hash, recorded_by, recorded_by_person_id, recorded_by_scope)
				VALUES ($1, 'platform', $2, $3, 'manual', 'mock-screening', $4, ARRAY['pep'], 'error', 'timeout', now(), 'none', $8, $5, $6, $7)`,
				w.Tenant, p.PersonID, "ga-"+uuid.NewString(), strings.Repeat("a", 64), by, byPerson, scope, strings.Repeat("b", 64))
			return err
		})
	}
	if err := insResult(officer.StaffID, officer.PersonID, "platform_acting"); err != nil {
		t.Fatalf("control: the proven actor's own columns must insert: %v", err)
	}
	for name, c := range map[string]struct {
		by, person uuid.UUID
		scope      string
	}{
		"another staff id":  {uuid.New(), officer.PersonID, "platform_acting"},
		"another person id": {officer.StaffID, uuid.New(), "platform_acting"},
		"another scope":     {officer.StaffID, officer.PersonID, "tenant"},
	} {
		if err := insResult(c.by, c.person, c.scope); pgCode(err) != "AM001" {
			t.Errorf("results guard, %s: want AM001, got %v", name, err)
		}
	}
	insPolicy := func(by, byPerson uuid.UUID, scope string) error {
		return asOfficer(func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds, effective_from,
					legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
				VALUES ($1, $2, ARRAY['pep'], 3600, 0, now(), 'TEST FIXTURE REF', 'fixture', $3, $4, $5)`, w.Tenant, j, by, byPerson, scope)
			return err
		})
	}
	if err := insPolicy(officer.StaffID, officer.PersonID, "platform_acting"); err != nil {
		t.Fatalf("control: the proven actor's own columns must insert a policy: %v", err)
	}
	for name, c := range map[string]struct {
		by, person uuid.UUID
		scope      string
	}{
		"another staff id":  {uuid.New(), officer.PersonID, "platform_acting"},
		"another person id": {officer.StaffID, uuid.New(), "platform_acting"},
	} {
		if err := insPolicy(c.by, c.person, c.scope); pgCode(err) != "AM001" {
			t.Errorf("policies guard, %s: want AM001, got %v", name, err)
		}
	}
	// The policy table only ever records a platform_acting author (CHECK), whatever the guard says.
	if err := insPolicy(officer.StaffID, officer.PersonID, "tenant"); pgCode(err) != "AM001" && pgCode(err) != "23514" {
		t.Errorf("a tenant-scope policy author: %v", err)
	}
}
