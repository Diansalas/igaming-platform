//go:build integration

package alerting_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertworld"
)

func TestRaiseSweepFailed_PersistsADeduplicatedP1PlatformAlert(t *testing.T) {
	w := alertworld.NewWorld(t, "swf")
	admin := w.SeedAdmin()
	ctx := context.Background()

	if err := alerting.RaiseSweepFailed(ctx, w.Pool, alerting.SweepPhaseListActive, "08"); err != nil {
		t.Fatalf("first raise: %v", err)
	}
	if err := alerting.RaiseSweepFailed(ctx, w.Pool, alerting.SweepPhaseListActive, "57"); err != nil {
		t.Fatalf("second raise: %v", err)
	}
	if err := alerting.RaiseSweepFailed(ctx, w.Pool, alerting.SweepPhaseListNonActive, "08"); err != nil {
		t.Fatalf("other phase raise: %v", err)
	}

	type row struct {
		id       uuid.UUID
		severity string
		subject  *uuid.UUID
		disc     string
		attrs    string
		occ      int
	}
	var rows []row
	if err := w.Pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		q, err := tx.Query(ctx, `SELECT a.id, a.severity, a.subject_tenant_id, a.discriminator, a.attributes::text,
			(SELECT count(*) FROM alert_occurrences o WHERE o.alert_id = a.id)
			FROM alerts a WHERE a.kind = 'reconciliation.sweep_failed' ORDER BY a.discriminator`)
		if err != nil {
			return err
		}
		defer q.Close()
		for q.Next() {
			var r row
			if err := q.Scan(&r.id, &r.severity, &r.subject, &r.disc, &r.attrs, &r.occ); err != nil {
				return err
			}
			rows = append(rows, r)
		}
		return q.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("alerts = %d, want 2 (one per phase, repeated failures dedupe)", len(rows))
	}
	for _, r := range rows {
		if r.severity != "p1" || r.subject != nil {
			t.Errorf("alert %s severity=%s subject=%v, want p1 and no subject", r.disc, r.severity, r.subject)
		}
	}
	if rows[0].disc != "phase:list_active" || rows[0].occ != 2 {
		t.Errorf("list_active alert = %+v, want 2 occurrences on one alert", rows[0])
	}
	if rows[1].disc != "phase:list_non_active" || rows[1].occ != 1 {
		t.Errorf("list_non_active alert = %+v", rows[1])
	}
	// No free text anywhere: attributes are exactly the closed pair.
	if rows[0].attrs != `{"phase": "list_active", "sqlstate_class": "08"}` && rows[0].attrs != `{"phase": "list_active", "sqlstate_class": "57"}` {
		t.Errorf("attributes = %s", rows[0].attrs)
	}
}

func TestRaiseSweepFailed_RefusesAnythingOutsideTheClosedVocabulary(t *testing.T) {
	w := alertworld.NewWorld(t, "swf_bad")
	admin := w.SeedAdmin()
	ctx := context.Background()
	for _, c := range []struct{ phase, class string }{
		{"list_active; DROP TABLE alerts", "08"},
		{"", "08"},
		{"tenant-1234", "08"},
		{alerting.SweepPhaseListActive, "boom: connection refused to 10.0.0.1"},
		{alerting.SweepPhaseListActive, ""},
		{alerting.SweepPhaseListActive, "8"},
		{alerting.SweepPhaseListActive, "abc"},
		{alerting.SweepPhaseListActive, "0a"}, // lower case is not a SQLSTATE class
	} {
		if err := alerting.RaiseSweepFailed(ctx, w.Pool, c.phase, c.class); err == nil {
			t.Errorf("RaiseSweepFailed(%q, %q) accepted free text", c.phase, c.class)
		}
	}
	var n int
	if err := w.Pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'reconciliation.sweep_failed'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("alerts written for refused input: %d (%v)", n, err)
	}
	if err := alerting.RaiseSweepFailed(ctx, nil, alerting.SweepPhaseListActive, "08"); err == nil {
		t.Error("a nil pool must be an error, not a silent no-op")
	}
}

func TestRaiseSweepFailed_IsDeliveredLikeAnyOtherP1(t *testing.T) {
	w := alertworld.NewWorld(t, "swf_disp")
	admin := w.SeedAdmin()
	ctx := context.Background()
	if err := alerting.RaiseSweepFailed(ctx, w.Pool, alerting.SweepPhasePanic, "58"); err != nil {
		t.Fatal(err)
	}
	var id uuid.UUID
	if err := w.Pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM alerts WHERE kind = 'reconciliation.sweep_failed'`).Scan(&id)
	}); err != nil {
		t.Fatal(err)
	}
	// A subjectless alert flows through the dispatcher (no route configured => unrouted, visible).
	d := alerting.NewDispatcher(w.Pool, alerting.DefaultDispatcherConfig(), alerting.LogSink{})
	if err := d.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if ev, reason := w.LatestEvent(admin, id); ev != "unrouted" || reason != "no_route" {
		t.Fatalf("latest event = %q/%q, want unrouted/no_route", ev, reason)
	}
}
