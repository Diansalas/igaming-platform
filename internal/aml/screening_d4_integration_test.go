//go:build integration

package aml_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
)

// d1 (D4): the routed trigger policy write is TIGHTEN-ONLY since migration 0191, enforced by the DATABASE function the route calls
// (aml_screening_trigger_policy_author), and the loosening-capable setter (aml_screening_trigger_policy_set, 0181) is owner-only. A loosening
// is the governed two-Person licensing change aml_screening_trigger_policy_loosen (tested in internal/licensingchange).
//
// Loosening, from the schema of aml_screening_trigger_policies: an event removed; gate_withdrawal true -> false; both gate and a LONGER
// gate_alert_after_seconds; an effective_from earlier than the version in force. Tightening: events added, a gate switched ON, a shorter threshold.

const d4Setter = `aml_screening_trigger_policy_set(uuid, uuid, text[], boolean, bigint, timestamptz, text, text)`

func triggerCount(t *testing.T, tw *trigWorld) int {
	t.Helper()
	var n int
	if err := tw.Owner.WithTenant(context.Background(), tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM aml_screening_trigger_policies WHERE tenant_id = $1 AND licensing_jurisdiction_id = $2`, tw.Tenant, tw.jur).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (tw *trigWorld) triggerIn(events []aml.TriggerEvent, gate bool, alert time.Duration, from time.Time) aml.SetTriggerPolicyInput {
	return aml.SetTriggerPolicyInput{JurisdictionID: tw.jur, Events: events, GateWithdrawal: gate, GateAlertAfter: alert, EffectiveFrom: from,
		LegalReviewReference: "TEST FIXTURE REF - NOT LEGAL ADVICE", ReasonCode: "fixture_trigger"}
}

func TestD4_TriggerAuthor_RefusesEveryLooseningDimension_WithTheClosedCode_AndWritesNothing(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	A := tw.q.Officers[0].Actor
	tw.heartbeat()
	all := []aml.TriggerEvent{aml.EventRegistration, aml.EventKYCApproval, aml.EventScheduled}
	base := tw.triggerIn(all, true, time.Hour, time.Now().Add(-time.Minute))
	if _, err := tw.svc.SetScreeningTriggerPolicy(ctx, A, base); err != nil {
		t.Fatalf("first version (a gate, three events, threshold 1 h) through the route: %v", err)
	}
	before := triggerCount(t, tw)
	refused := func(what string, in aml.SetTriggerPolicyInput) {
		t.Helper()
		_, err := tw.svc.SetScreeningTriggerPolicy(ctx, A, in)
		if aml.RefusalCode(err) != "trigger_policy_loosening_refused" {
			t.Errorf("%s: want the closed code trigger_policy_loosening_refused, got %v (code %q)", what, err, aml.RefusalCode(err))
		}
		if n := triggerCount(t, tw); n != before {
			t.Fatalf("%s: a refused loosening wrote a version (%d -> %d)", what, before, n)
		}
	}
	now := time.Now()
	refused("an event removed (kyc_approval)", tw.triggerIn([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, true, time.Hour, now))
	refused("the gate switched OFF, events unchanged", tw.triggerIn(all, false, 0, now))
	refused("the gate switched off and every event removed", tw.triggerIn(nil, false, 0, now))
	refused("a LONGER alert threshold", tw.triggerIn(all, true, 2*time.Hour, now))
	refused("an earlier effective time than the version in force", tw.triggerIn(all, true, time.Hour, now.Add(-90*time.Second)))
	refused("a mix: an event removed AND a shorter threshold", tw.triggerIn([]aml.TriggerEvent{aml.EventScheduled}, true, 30*time.Minute, now))
	// Equal and stricter versions stay on the route.
	if _, err := tw.svc.SetScreeningTriggerPolicy(ctx, A, tw.triggerIn(all, true, time.Hour, time.Now())); err != nil {
		t.Fatalf("an EQUAL version stays on the route: %v", err)
	}
	if _, err := tw.svc.SetScreeningTriggerPolicy(ctx, A, tw.triggerIn(all, true, 30*time.Minute, time.Now())); err != nil {
		t.Fatalf("a SHORTER alert threshold tightens: %v", err)
	}
}

func TestD4_TriggerAuthor_TighteningFromOffStaysOnTheRoute_AndEnablingStillNeedsTheGateGuards(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	A := tw.q.Officers[0].Actor
	// First version: nothing triggers, no gate. A tightening: events added; then the gate ON (heartbeat + environment anchor required).
	if _, err := tw.svc.SetScreeningTriggerPolicy(ctx, A, tw.triggerIn(nil, false, 0, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("a first version with nothing to loosen: %v", err)
	}
	if _, err := tw.svc.SetScreeningTriggerPolicy(ctx, A, tw.triggerIn([]aml.TriggerEvent{aml.EventRegistration}, false, 0, time.Now())); err != nil {
		t.Fatalf("adding an event tightens: %v", err)
	}
	if err := tw.setTriggerErr([]aml.TriggerEvent{aml.EventRegistration}, true, time.Now()); err != nil {
		t.Fatalf("enabling a gate in a test-anchored database with a reporting worker stays on the route: %v", err)
	}
}

func TestD4_TriggerSetter_IsOwnerOnly_RuntimeIs42501_OwnerPathStillWorks(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	A := tw.q.Officers[0].Actor
	r := catalogQuery(t, tw.amlWorld, `SELECT has_function_privilege('igaming_runtime', $1::regprocedure, 'EXECUTE'), has_function_privilege('public', $1::regprocedure, 'EXECUTE')`, d4Setter)[0]
	if r[0].(bool) || r[1].(bool) {
		t.Fatalf("the trigger policy setter must be executable by neither the runtime role nor PUBLIC: %v", r)
	}
	// Raw SQL as the runtime role, and the owner-path service method over the RUNTIME pool: both 42501, nothing written.
	before := triggerCount(t, tw)
	err := tw.RT.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT aml_screening_trigger_policy_set($1, $2, ARRAY[]::text[], false, NULL, now(), 'D4-TEST', 'd4_test')`, tw.Tenant, tw.jur)
		return err
	})
	if pgCode(err) != "42501" {
		t.Fatalf("the runtime role calling the trigger setter must get 42501, got %v", err)
	}
	if _, err := tw.svc.OwnerSetScreeningTriggerPolicy(ctx, A, tw.triggerIn(nil, false, 0, time.Now())); pgCode(err) != "42501" {
		t.Fatalf("OwnerSetScreeningTriggerPolicy over the runtime pool must surface 42501, got %v", err)
	}
	if n := triggerCount(t, tw); n != before {
		t.Fatalf("a refused call wrote a version: %d -> %d", before, n)
	}
	// The owner path keeps working (with the officer controls) and can still do what the route cannot: loosen. That is the documented break-glass.
	own := ownerSvc(t)
	if _, err := own.OwnerSetScreeningTriggerPolicy(ctx, A, tw.triggerIn([]aml.TriggerEvent{aml.EventRegistration, aml.EventScheduled}, false, 0, time.Now().Add(-time.Minute))); err != nil {
		t.Fatalf("the owner path: %v", err)
	}
	if _, err := own.OwnerSetScreeningTriggerPolicy(ctx, A, tw.triggerIn(nil, false, 0, time.Now())); err != nil {
		t.Fatalf("the owner path can remove events (break-glass): %v", err)
	}
	other := A
	other.StaffID = uuid.New()
	if _, err := own.OwnerSetScreeningTriggerPolicy(ctx, other, tw.triggerIn(nil, false, 0, time.Now())); err == nil {
		t.Fatal("an actor without the officer grant must be refused even on the owner path")
	}
}

func TestD4_OnlyAllowListedRuntimeFunctionsWriteTheTriggerPolicies(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	got := sortedKeys(tableWriters(t, w, "aml_screening_trigger_policies"))
	want := []string{"aml_screening_trigger_policy_author", "screening_trigger_loosen_apply"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("runtime-executable writers of aml_screening_trigger_policies = %v, want exactly %v (the tighten-only author and the governed executor)", got, want)
	}
	// Direct writes by the runtime role are denied by privilege.
	ctx := context.Background()
	for _, stmt := range []string{
		`INSERT INTO aml_screening_trigger_policies (tenant_id, licensing_jurisdiction_id, events, gate_withdrawal, effective_from, legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope)
			VALUES (gen_random_uuid(), gen_random_uuid(), ARRAY[]::text[], false, now(), 'x', 'x', gen_random_uuid(), gen_random_uuid(), 'platform_acting')`,
		`UPDATE aml_screening_trigger_policies SET gate_withdrawal = false`, `DELETE FROM aml_screening_trigger_policies`,
	} {
		err := w.RT.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error { _, err := tx.Exec(ctx, stmt); return err })
		if pgCode(err) != "42501" {
			t.Errorf("runtime %.40q: want 42501, got %v", stmt, err)
		}
	}
	// The executor is runtime-executable but refuses without an executed request of this transaction.
	err := w.RT.WithTenant(ctx, w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT screening_trigger_loosen_apply($1)`, uuid.New())
		return err
	})
	if err == nil {
		t.Fatal("screening_trigger_loosen_apply must refuse a request that was not executed in this transaction")
	}
}

// The comparison function is the one definition of "less strict": a truth table over every dimension.
func TestD4_StrictnessComparison_TruthTable(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	type row struct {
		pe     string
		pg     bool
		pa     any
		e      string
		g      bool
		a      any
		looser bool
		strict bool
	}
	rows := []row{
		{"{registration,scheduled}", true, 3600, "{registration,scheduled}", true, 3600, false, false}, // equal
		{"{registration,scheduled}", true, 3600, "{registration}", false, nil, true, false},            // gate off + event removed
		{"{registration,scheduled}", true, 3600, "{registration,scheduled}", false, nil, true, false},  // gate off only
		{"{registration,scheduled}", true, 3600, "{registration,scheduled}", true, 7200, true, false},  // longer threshold
		{"{registration,scheduled}", true, 3600, "{registration,scheduled}", true, 1800, false, true},  // shorter threshold
		{"{registration,scheduled}", true, 3600, "{registration,kyc_approval,scheduled}", true, 3600, false, true},
		{"{registration}", false, nil, "{registration,scheduled}", true, 3600, false, true}, // enabling a gate (and adding an event)
		{"{registration}", false, nil, "{}", false, nil, true, false},                       // the last event removed
		{"{registration,scheduled}", true, 3600, "{scheduled}", true, 1800, true, true},     // event removed AND shorter threshold = a mix
		{"{}", false, nil, "{}", false, nil, false, false},
	}
	for i, r := range rows {
		var looser, strict bool
		var pa, a any = r.pa, r.a
		if err := w.Owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT o_looser, o_stricter FROM screening_trigger_cmp($1::text[], $2, $3::bigint, $4::text[], $5, $6::bigint)`, r.pe, r.pg, pa, r.e, r.g, a).Scan(&looser, &strict)
		}); err != nil {
			t.Fatalf("row %d: %v", i, err)
		}
		if looser != r.looser || strict != r.strict {
			t.Errorf("row %d (%v -> %v): looser=%v stricter=%v, want %v / %v", i, r, r.e, looser, strict, r.looser, r.strict)
		}
	}
}
