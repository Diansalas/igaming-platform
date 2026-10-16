//go:build integration

package launchgov_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// A13 fix of audit finding F1: R-ALERTS must not pass on the channel vocabulary. It passes only from the alert
// dispatcher's configuration-only routing readiness: an ENABLED step-0 platform route for p1 and p2 that names a
// recipient, uses a human-notification channel kind, and whose sink is wired in this process and reports a person.
// These tests drive the real database model (alert_routes with the SR-7 four-eyes guard of migration 0156), the real
// dispatcher evaluation, and the real launchgov readiness.

// humanWebhookSink is a wired sink that reports a person, standing in for a configured webhook channel. It never
// delivers anything: only configuration is evaluated.
type humanWebhookSink struct{}

func (humanWebhookSink) ChannelKind() alerting.ChannelKind { return alerting.ChannelWebhook }
func (humanWebhookSink) HumanNotification() bool           { return true }
func (humanWebhookSink) Deliver(context.Context, alerting.Delivery) (alerting.Outcome, alerting.ErrorClass) {
	return alerting.OutcomeSent, alerting.ErrorClassNone
}

// mockEmailSink stands in for the email channel over the MOCK provider: wired, but it notifies nobody.
type mockEmailSink struct{}

func (mockEmailSink) ChannelKind() alerting.ChannelKind { return alerting.ChannelEmail }
func (mockEmailSink) HumanNotification() bool           { return false }
func (mockEmailSink) Deliver(context.Context, alerting.Delivery) (alerting.Outcome, alerting.ErrorClass) {
	return alerting.OutcomeSent, alerting.ErrorClassNone
}

func (w *world) addRoute(admin uuid.UUID, sev, kind string, enabled bool, stagedFor bool, staged *uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.rt.WithPlatformAdmin(context.Background(), admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code, staged_route_id, staged_for_enable)
			VALUES ('platform', $1, 0, $2, 'interim-owner', $3, 'initial_setup', $4, $5) RETURNING id`, sev, kind, enabled, staged, stagedFor).Scan(&id)
	}); err != nil {
		w.t.Fatalf("add %s %s route (enabled=%v): %v", sev, kind, enabled, err)
	}
	return id
}

// stageAndEnable creates a human route the way the API does: one admin stages it disabled, a DIFFERENT admin enables it.
func (w *world) stageAndEnable(sev, kind string) {
	w.t.Helper()
	staged := w.addRoute(w.ops.Requester, sev, kind, false, true, nil)
	w.addRoute(w.ops.Approver1, sev, kind, true, false, &staged)
}

func (w *world) alertsCheckAt(svc *launchgov.Service, tid, jid uuid.UUID) launchgov.Check {
	w.t.Helper()
	r, err := svc.EvaluateReadiness(context.Background(), launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tid}, Action: launchgov.ActionActivate,
		Licensing: &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional,
			DeterminationReference: "ext-ref-001", DeterminationByRole: "external counsel"},
		JurisdictionIDs: []uuid.UUID{jid}})
	if err != nil {
		w.t.Fatalf("readiness: %v", err)
	}
	for _, c := range r.Checks {
		if c.Code == launchgov.CheckAlerts {
			return c
		}
	}
	w.t.Fatal("the readiness has no R-ALERTS check")
	return launchgov.Check{}
}

func wantAlerts(t *testing.T, what string, c launchgov.Check, result, detail string) {
	t.Helper()
	if c.Result != result || c.DetailCode != detail {
		t.Fatalf("%s: R-ALERTS want %s/%s, got %s/%s", what, result, detail, c.Result, c.DetailCode)
	}
}

func TestReadiness_Alerts_PassesOnlyForAnEnabledWiredHumanRoute(t *testing.T) {
	w := newScratchWorld(t) // alert_routes is platform-wide: use a private database
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	ctx := context.Background()

	// the real dispatcher over the real pool, with a human webhook sink and an email sink over the MOCK provider wired
	d := alerting.NewDispatcher(w.rt, alerting.DispatcherConfig{ReadinessStaleAfter: time.Hour}, alerting.LogSink{}, humanWebhookSink{}, mockEmailSink{})
	svc := w.service(launchgov.Config{AlertRouting: d.ReadinessEffective})
	eval := func() {
		t.Helper()
		if err := d.EvaluateReadiness(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// no routing source at all: not evaluable, never a pass
	wantAlerts(t, "no source", w.alertsCheckAt(w.service(launchgov.Config{}), tid, jid), launchgov.ResultNotEvaluable, "no_routing_source")
	// a dispatcher that has not evaluated yet
	wantAlerts(t, "never evaluated", w.alertsCheckAt(svc, tid, jid), launchgov.ResultNotEvaluable, "routing_not_evaluated")

	// no route row: the vocabulary (email, webhook) and the wired sinks exist, and it still fails
	eval()
	wantAlerts(t, "no route", w.alertsCheckAt(svc, tid, jid), launchgov.ResultFail, "p1_no_route_configured")

	// a route that is staged (disabled) is not a route: the second admin has not enabled it
	staged1 := w.addRoute(w.ops.Requester, "p1", "webhook", false, true, nil)
	staged2 := w.addRoute(w.ops.Requester, "p2", "webhook", false, true, nil)
	eval()
	wantAlerts(t, "disabled (staged) routes", w.alertsCheckAt(svc, tid, jid), launchgov.ResultFail, "p1_route_not_enabled")

	// a log route enabled without four-eyes is allowed by the database, but it is not a person
	w.addRoute(w.ops.Requester, "p1", "log", true, false, nil)
	eval()
	wantAlerts(t, "enabled log route", w.alertsCheckAt(svc, tid, jid), launchgov.ResultFail, "p1_channel_not_human_notification")

	// the database refuses to enable a human route alone (SR-7), so there is no way to a pass without a second admin
	if err := w.rt.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code, staged_route_id)
			VALUES ('platform', 'p1', 0, 'webhook', 'interim-owner', true, 'initial_setup', $1)`, staged1)
		return err
	}); pgCode(err) != "AR001" {
		t.Fatalf("the staging admin must not be able to approve their own route: %v", err)
	}

	// the second admin enables p1 only: p2 is still not enabled
	w.addRoute(w.ops.Approver1, "p1", "webhook", true, false, &staged1)
	eval()
	wantAlerts(t, "p1 enabled, p2 only staged", w.alertsCheckAt(svc, tid, jid), launchgov.ResultFail, "p2_route_not_enabled")

	// p2 enabled as well: an enabled, wired, human-notification route for both required severities passes
	w.addRoute(w.ops.Approver1, "p2", "webhook", true, false, &staged2)
	eval()
	wantAlerts(t, "enabled wired human routes", w.alertsCheckAt(svc, tid, jid), launchgov.ResultPass, "ok")

	// a newer DISABLED version (decommission) takes the pass away again
	w.addRoute(w.ops.Approver1, "p1", "webhook", false, false, nil)
	eval()
	wantAlerts(t, "p1 decommissioned", w.alertsCheckAt(svc, tid, jid), launchgov.ResultFail, "p1_route_not_enabled")

	// a stale dispatcher evaluation is not a pass either (fail closed)
	stale := alerting.NewDispatcher(w.rt, alerting.DispatcherConfig{ReadinessStaleAfter: time.Nanosecond}, alerting.LogSink{}, humanWebhookSink{})
	if err := stale.EvaluateReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	wantAlerts(t, "stale evaluation", w.alertsCheckAt(w.service(launchgov.Config{AlertRouting: stale.ReadinessEffective}), tid, jid), launchgov.ResultNotEvaluable, "routing_evaluation_stale")
}

// An enabled email route whose sink is the MOCK provider (this repository's only email adapter) reaches nobody, so the
// check fails even though the route is enabled, approved and the channel kind is a human-notification kind.
func TestReadiness_Alerts_EnabledEmailRouteOverTheMockProviderDoesNotPass(t *testing.T) {
	w := newScratchWorld(t)
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	d := alerting.NewDispatcher(w.rt, alerting.DispatcherConfig{ReadinessStaleAfter: time.Hour}, alerting.LogSink{}, mockEmailSink{})
	svc := w.service(launchgov.Config{AlertRouting: d.ReadinessEffective})
	w.stageAndEnable("p1", "email")
	w.stageAndEnable("p2", "email")
	if err := d.EvaluateReadiness(context.Background()); err != nil {
		t.Fatal(err)
	}
	wantAlerts(t, "enabled email over MOCK", w.alertsCheckAt(svc, tid, jid), launchgov.ResultFail, "p1_channel_not_human_notification")
}

// The snapshot hash binds the R-ALERTS result: configuring a route changes the readiness hash, so a request filed
// while the check failed goes stale instead of being approved on an outdated readiness.
func TestReadiness_Alerts_ResultIsPartOfTheReadinessHash(t *testing.T) {
	w := newScratchWorld(t)
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	ctx := context.Background()
	d := alerting.NewDispatcher(w.rt, alerting.DispatcherConfig{ReadinessStaleAfter: time.Hour}, alerting.LogSink{}, humanWebhookSink{})
	svc := w.service(launchgov.Config{AlertRouting: d.ReadinessEffective})
	in := launchgov.EvalInput{Subject: launchgov.Subject{TenantID: tid}, Action: launchgov.ActionActivate,
		Licensing: &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusConditional,
			DeterminationReference: "ext-ref-001", DeterminationByRole: "external counsel"},
		JurisdictionIDs: []uuid.UUID{jid}}
	if err := d.EvaluateReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	before, err := svc.EvaluateReadiness(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	w.stageAndEnable("p1", "webhook")
	w.stageAndEnable("p2", "webhook")
	if err := d.EvaluateReadiness(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := svc.EvaluateReadiness(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	if before.Version != launchgov.SnapshotVersion || before.Hash() == after.Hash() {
		t.Fatalf("a changed R-ALERTS result must change the readiness hash (version %d)", before.Version)
	}
}
