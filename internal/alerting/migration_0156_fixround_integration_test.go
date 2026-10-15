//go:build integration

package alerting_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertworld"
)

// A9 security-review fix round on migration 0156 (edited in place; not merged anywhere).

// SR9-A9-3: the staged record must carry an explicit staging INTENT (staged_for_enable), set only by the
// staging path. A route disabled for another reason (decommission) cannot be re-enabled by one admin using
// someone else's disabled row, and an enabling version always starts now (effective_from forced).
func TestMigration0156_SR9A93_DecommissionedRowIsNotAStagingRecordAndEffectiveFromIsForced(t *testing.T) {
	w := alertworld.NewWorld(t, "m156_dec")
	a, b := w.SeedAdmin(), w.SeedAdmin()
	ctx := context.Background()
	as := func(admin uuid.UUID, f func(ctx context.Context, tx pgx.Tx) error) error {
		return w.Pool.WithPlatformAdmin(ctx, admin, f)
	}
	// Legitimate flow: A stages (intent flag), B enables -> a live email route.
	var staged, live uuid.UUID
	if err := as(a, func(ctx context.Context, tx pgx.Tx) (err error) {
		staged, err = insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", false, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := as(b, func(ctx context.Context, tx pgx.Tx) (err error) {
		live, err = insertRouteFull(ctx, tx, "p1", 0, "email", "interim-owner", true, &staged)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// B decommissions it: a DISABLED version superseding the live one, for another reason, with no staging intent.
	var decom uuid.UUID
	if err := as(b, func(ctx context.Context, tx pgx.Tx) (err error) {
		decom, err = insertRouteRaw(ctx, tx, "p1", 0, "email", "interim-owner", false, nil, false, "decommission")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE alert_routes SET superseded_at = now(), superseded_by = $2 WHERE id = $1`, live, decom)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// The repro: A alone re-enables the identical configuration against B's decommission row, backdating
	// effective_from by a year. It must be refused.
	err := as(a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code, staged_route_id, effective_from)
			VALUES ('platform', 'p1', 0, 'email', 'interim-owner', true, 'correction', $1, now() - interval '365 days')`, decom)
		return err
	})
	if pgCode(err) != "AR001" || !strings.Contains(err.Error(), "staged for enabling") {
		t.Fatalf("a decommission row must not serve as a four-eyes staging record, got %v", err)
	}

	// The intent flag is only valid on a DISABLED human-notification version.
	for name, c := range map[string]struct {
		kind    string
		enabled bool
	}{"enabled human": {"email", true}, "disabled non-human": {"log", false}, "enabled non-human": {"mock", true}} {
		err := as(a, func(ctx context.Context, tx pgx.Tx) error {
			_, err := insertRouteRaw(ctx, tx, "p2", 3, c.kind, "x", c.enabled, nil, true, "initial_setup")
			return err
		})
		if pgCode(err) != "AR001" {
			t.Errorf("%s: staged_for_enable=true must be refused (AR001), got %v", name, err)
		}
	}

	// effective_from is forced to now() on every INSERT: a legitimate approval with a backdated value starts now.
	var st2 uuid.UUID
	if err := as(a, func(ctx context.Context, tx pgx.Tx) (err error) {
		st2, err = insertRouteFull(ctx, tx, "p2", 1, "webhook", "interim-owner", false, nil)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var recent bool
	if err := as(b, func(ctx context.Context, tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code, staged_route_id, effective_from)
			VALUES ('platform', 'p2', 1, 'webhook', 'interim-owner', true, 'initial_setup', $1, now() - interval '365 days') RETURNING id`, st2).Scan(&id); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT effective_from >= now() - interval '1 minute' AND effective_from <= now() + interval '1 minute' FROM alert_routes WHERE id = $1`, id).Scan(&recent)
	}); err != nil {
		t.Fatalf("approval with a backdated effective_from: %v", err)
	}
	if !recent {
		t.Error("effective_from on an enabling version was not forced to now()")
	}
	// A backdated non-human route is forced as well (uniform rule).
	var recent2 bool
	if err := as(a, func(ctx context.Context, tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, enabled, reason_code, effective_from)
			VALUES ('platform', 'p3', 0, 'log', 'ops', true, 'initial_setup', now() - interval '365 days') RETURNING id`).Scan(&id); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT effective_from >= now() - interval '1 minute' FROM alert_routes WHERE id = $1`, id).Scan(&recent2)
	}); err != nil || !recent2 {
		t.Errorf("non-human effective_from forced = %v, %v", recent2, err)
	}

	// staged_for_enable is immutable like every other column.
	err = as(a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE alert_routes SET staged_for_enable = true WHERE id = $1`, decom)
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "only superseded_at/superseded_by may change") {
		t.Errorf("staged_for_enable must be immutable, got %v", err)
	}
}

// SR9-A9-4: the sweep_failed payload is validated in the database with the raise_failed posture: closed phase
// vocabulary, a 2-character SQLSTATE class, exactly the two keys, string values, and a FORCED discriminator.
// Only the dispatcher identity may raise the Kind (RESTRICTIVE policies); a platform admin may not.
func TestMigration0156_SR9A94_SweepFailedPayloadIsValidatedByTheDatabase(t *testing.T) {
	w := alertworld.NewWorld(t, "m156_sfv")
	admin := w.SeedAdmin()
	ctx := context.Background()
	raise := func(discriminator, attrs string) error {
		return w.Pool.WithPlatformService(ctx, db.ServiceAlertDispatcher, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes)
				VALUES (NULL, NULL, 'reconciliation.sweep_failed', $1, $2::jsonb) RETURNING id`, discriminator, attrs).Scan(&id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, id)
			return err
		})
	}
	// A platform admin must not raise the Kind (the header's claim, enforced by the RESTRICTIVE policy). This runs
	// FIRST, on an empty table, with a valid payload, so the ONLY thing that can refuse it is the policy (an open
	// alert with the same dedup key would refuse it for the wrong reason); the refusal must be the RLS violation.
	err := w.Pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (tenant_id, subject_tenant_id, kind, discriminator, attributes)
			VALUES (NULL, NULL, 'reconciliation.sweep_failed', 'phase:panic', '{"phase":"panic","sqlstate_class":"XX"}'::jsonb)`)
		return err
	})
	if pgCode(err) != "42501" || !strings.Contains(err.Error(), "alerts_sweep_failed_dispatcher_only") {
		t.Fatalf("a platform admin must be refused by the dispatcher-only policy, got %v", err)
	}
	// The free-text repro from the review.
	if err := raise("phase:list_active", `{"phase":"player jane.doe@example.com card 4111111111111111","sqlstate_class":"anything at all"}`); err == nil {
		t.Fatal("free text in phase / sqlstate_class was accepted")
	}
	for name, attrs := range map[string]string{
		"phase outside vocabulary":     `{"phase":"list_everything","sqlstate_class":"08"}`,
		"sqlstate lowercase":           `{"phase":"list_active","sqlstate_class":"ab"}`,
		"sqlstate three chars":         `{"phase":"list_active","sqlstate_class":"08P"}`,
		"sqlstate empty":               `{"phase":"list_active","sqlstate_class":""}`,
		"missing sqlstate":             `{"phase":"list_active"}`,
		"missing phase":                `{"sqlstate_class":"08"}`,
		"empty object":                 `{}`,
		"phase is a number":            `{"phase":1,"sqlstate_class":"08"}`,
		"sqlstate is an object":        `{"phase":"list_active","sqlstate_class":{"a":"08"}}`,
		"phase with trailing newline":  "{\"phase\":\"list_active\\n\",\"sqlstate_class\":\"08\"}",
		"sqlstate with trailing space": `{"phase":"list_active","sqlstate_class":"08 "}`,
		"go_validation is not allowed": `{"phase":"list_active","sqlstate_class":"go_validation"}`,
	} {
		if err := raise("phase:list_active", attrs); err == nil {
			t.Errorf("%s: accepted %s", name, attrs)
		}
	}
	// Every legal phase is accepted, and the discriminator is FORCED from the validated phase: the caller's
	// free-text discriminator is discarded.
	for _, phase := range []string{"list_active", "list_non_active", "panic", "unknown"} {
		if err := raise("free text jane.doe@example.com", `{"phase":"`+phase+`","sqlstate_class":"XX"}`); err != nil {
			t.Fatalf("legal phase %s refused: %v", phase, err)
		}
	}
	var n, bad int
	if err := w.Pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'reconciliation.sweep_failed'`).Scan(&n); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'reconciliation.sweep_failed' AND discriminator <> 'phase:' || (attributes->>'phase')`).Scan(&bad)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 4 || bad != 0 {
		t.Fatalf("stored sweep_failed alerts = %d (want 4), with a non-forced discriminator = %d (want 0)", n, bad)
	}

	// Nor may a platform admin append an occurrence to one (the occurrences table has its own restrictive policy).
	err = w.Pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM alerts WHERE kind = 'reconciliation.sweep_failed' LIMIT 1`).Scan(&id); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO alert_occurrences (alert_id) VALUES ($1)`, id)
		return err
	})
	if pgCode(err) != "42501" || !strings.Contains(err.Error(), "alerts_sweep_failed_dispatcher_only") {
		t.Fatalf("a platform admin must not append an occurrence of the Kind, got %v", err)
	}
	// ...but a platform admin still reads it (the restriction is INSERT only).
	if err := w.Pool.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alerts WHERE kind = 'reconciliation.sweep_failed' AND state = 'open'`).Scan(&n)
	}); err != nil || n != 4 {
		t.Fatalf("platform admin read of sweep_failed alerts = %d, %v", n, err)
	}
}
