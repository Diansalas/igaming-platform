//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func migDirThrough(t *testing.T, through int) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || len(name) < 4 {
			continue
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil || v > through {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Ratification (optional, ADR 0112 3.1): a LEGACY subject (a row that existed before migration 0128)
// can record a launch decision without any status change; a subject that was NOT legacy cannot; and a
// ratified subject cannot be ratified twice. Runs on a scratch database migrated through 0127, seeded
// with legacy rows, then through 0130.
func TestRatify_LegacyBaselineSubject_RecordsDecisionWithoutStatusChange(t *testing.T) {
	ctx := context.Background()
	url := scratchdb.New(t, "lgov_ratify_")
	pool, err := db.Connect(ctx, url, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(ctx, migDirThrough(t, 127)); err != nil {
		t.Fatalf("migrate through 0127: %v", err)
	}
	// Legacy rows: a tenant with an active brand and a suspended brand, as the first tenant looks today.
	tid, activeBrand, suspendedBrand := uuid.New(), uuid.New(), uuid.New()
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'legacy', $2, 'under_platform_licence')`, tid, "legacy-"+tid.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		for _, b := range []struct {
			id     uuid.UUID
			status string
		}{{activeBrand, "active"}, {suspendedBrand, "suspended"}} {
			if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, name, slug, status) VALUES ($1, $2, 'b', $3, $4)`, b.id, tid, "lb-"+b.id.String(), b.status); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.MigrateUp(ctx, migDirThrough(t, 130)); err != nil {
		t.Fatalf("migrate through 0130: %v", err)
	}
	prooftest.InstallVia(t, pool.Raw(), t.Name())
	launchfix.SeedOperatorsAt(t, url)
	ops := launchfix.Operators()

	// A jurisdiction configuration for the tenant (the ratification references one).
	jid := uuid.New()
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'ratify')`, jid, "RT-"+jid.String()[:12])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, allowed_currencies, allowed_payment_methods, effective_from)
			VALUES ($1, $2, 'k', 'a', 'r', '["EUR"]', '["card"]', now() - interval '1 hour')`, tid, jid)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	svc, err := launchgov.New(launchgov.Config{Pool: pool, Environment: "test", LockTimeout: "2s"})
	if err != nil {
		t.Fatal(err)
	}
	plat := func(id uuid.UUID) launchgov.Actor {
		return launchgov.Actor{StaffID: id, Scope: launchgov.ScopePlatform, Role: auth.RolePlatformAdmin}
	}
	req, ap1 := plat(ops.Requester), plat(ops.Approver1)

	status := func(b uuid.UUID) string {
		var s string
		if err := pool.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1`, b).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	ratifyInput := func(b uuid.UUID) launchgov.RequestInput {
		in := launchgov.RequestInput{
			Subject: launchgov.Subject{TenantID: tid, BrandID: &b}, Action: launchgov.ActionRatify, ReasonCode: "legacy_ratification",
			Licensing: &launchgov.LicensingRecord{Model: launchgov.LicModelOther, Status: launchgov.LicStatusInForce,
				DeterminationReference: "ext-ref-legacy", DeterminationByRole: "external counsel"},
			ResponsibleOperatorName: "Legacy Operator Ltd", JurisdictionIDs: []uuid.UUID{jid},
		}
		r, err := svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: in.Subject, Action: in.Action, Licensing: in.Licensing, JurisdictionIDs: in.JurisdictionIDs})
		if err != nil {
			t.Fatal(err)
		}
		// Migration 0154 note (A7): this scratch schema is below migration 0154, so the AML reader does not exist. The AML items
		// degrade to not_evaluable (never pass) and the readiness transaction still commits (the read is behind a savepoint).
		for _, c := range r.Checks {
			if (c.Code == launchgov.CheckAMLQuorum || c.Code == launchgov.CheckAMLScreening) && (c.Result != launchgov.ResultNotEvaluable || c.DetailCode != "read_error") {
				t.Fatalf("%s without the reader must be not_evaluable/read_error: %+v", c.Code, c)
			}
		}
		in.AcknowledgedCheckCodes = r.NonPassing()
		return in
	}

	for _, b := range []uuid.UUID{activeBrand, suspendedBrand} {
		before := status(b)
		res, err := svc.Request(ctx, req, ratifyInput(b))
		if err != nil {
			t.Fatalf("ratify request on a legacy %s brand: %v", before, err)
		}
		if res.Request.RequiredApprovals != 1 || res.Request.FromStatus != before || res.Request.ToStatus != before {
			t.Fatalf("ratify request: %+v", res.Request)
		}
		rd, _ := svc.EvaluateReadiness(ctx, launchgov.EvalInputOf(res.Request))
		d, err := svc.Decide(ctx, ap1, tid, res.Request.ID, launchgov.DecideInput{Decision: launchgov.DecisionApprove, ReasonCode: "reviewed_ok", SeenReadinessHash: rd.Hash()})
		if err != nil || !d.Executed {
			t.Fatalf("ratify execution: %+v err=%v", d, err)
		}
		if got := status(b); got != before {
			t.Fatalf("a ratification must not change the status: %s -> %s", before, got)
		}
		// The history: the legacy baseline, then the governed (same-status) decision.
		var kinds []string
		if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT kind || ':' || COALESCE(from_status, '-') || '>' || to_status FROM launch_status_transitions WHERE brand_id = $1 ORDER BY created_at, id`, b)
			if err != nil {
				return err
			}
			kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if len(kinds) != 2 || kinds[0] != "legacy_baseline:->"+before || kinds[1] != "governed:"+before+">"+before {
			t.Fatalf("transition history: %v", kinds)
		}
		// Ratified once: the latest transition is no longer the legacy baseline.
		if _, err := svc.Request(ctx, req, ratifyInput(b)); !errors.Is(err, launchgov.ErrNotLegacyBaseline) {
			t.Fatalf("a second ratification must be refused: %v", err)
		}
	}
	// The audit event is `ratified` (not `executed`).
	var n int
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action = 'launch_authorisation.ratified' AND subject_tenant_id = $1`, tid).Scan(&n)
	}); err != nil || n != 2 {
		t.Fatalf("ratified audit rows: %d err=%v", n, err)
	}
}

// A subject that is not a legacy row (owner-provisioned for a fixture, or created pending and
// activated) cannot be ratified: only a latest legacy_baseline transition admits it.
func TestRatify_NonLegacySubjectRefused(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	brand := w.activeBrand(tid) // owner_provisioned, not legacy_baseline
	in := w.activation(launchgov.Subject{TenantID: tid, BrandID: &brand}, jid)
	in.Action, in.ReasonCode = launchgov.ActionRatify, "legacy_ratification"
	in.AcknowledgedCheckCodes = w.nonPassing(in)
	// Its own error (the Go mirror of the request guard's legacy_baseline rule), not an illegal move.
	_, err := w.svc.Request(ctx, w.req, in)
	if !errors.Is(err, launchgov.ErrNotLegacyBaseline) || errors.Is(err, launchgov.ErrIllegalTransition) {
		t.Fatalf("ratifying an owner-provisioned subject: %v", err)
	}
	if rows := w.audit(tid, "launch_authorisation.request_denied"); len(rows) != 1 || rows[0]["code"] != "LAUNCH_NOT_LEGACY_BASELINE" {
		t.Fatalf("one denied row with the distinct code: %+v", rows)
	}
	// The database says the same on its own (a raw INSERT bypassing Go): LA010 with the legacy_baseline message.
	err = w.asPlatform(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := newSpec(tid, &brand, "ratify").insert(ctx, tx)
		return err
	})
	if pgCode(err) != "LA010" || !strings.Contains(err.Error(), "legacy_baseline") {
		t.Fatalf("the database refuses a non-legacy ratification with LA010: %v", err)
	}
}
