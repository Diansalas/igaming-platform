//go:build integration

// ADR 0112 slice 3, migration 0130 on THROWAWAY scratch databases: a whole-schema up/down/up snapshot
// (the down restores 0129's status guard and 0124's actor_proof_require byte for byte), the S13-style
// down refusal while a request is in flight, and the new objects. Requires TEST_DATABASE_URL and
// TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package tenant

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

const migration0130Version = 130

func TestMigration0130_UpDownUp_WholeSchema_RestoresPriorBodiesVerbatim(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0130s_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0130Version-1)); err != nil {
		t.Fatal(err)
	}
	snap129 := lgSnapshot(t, pool)
	full := lgMigDir(t, migration0130Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	snap130 := lgSnapshot(t, pool)
	if snap129 == snap130 {
		t.Fatal("0130 changed nothing in the schema snapshot (vacuous)")
	}
	// The new objects.
	var n int
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT
			(SELECT count(*) FROM pg_trigger WHERE tgname = 'zz_actor_proof_guard' AND tgrelid IN ('launch_authorisation_requests'::regclass, 'launch_authorisation_approvals'::regclass))
			+ (SELECT count(*) FROM pg_trigger WHERE tgname = 'launch_requests_reject_carryover' AND tgrelid = 'launch_authorisation_requests'::regclass)
			+ (SELECT count(*) FROM pg_trigger WHERE tgname = 'launch_suspension_no_stale_pending_at_commit' AND tgrelid = 'launch_authorisation_requests'::regclass)
			+ (SELECT count(*) FROM pg_proc WHERE proname = 'launch_request_content_digest')`).Scan(&n)
	}); err != nil || n != 5 {
		t.Fatalf("0130 objects present: %d (want 5) err=%v", n, err)
	}
	// The verifier admits the launch family (and only through the platform / tenant scopes).
	var src string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT prosrc FROM pg_proc WHERE proname = 'actor_proof_require'`).Scan(&src)
	}); err != nil || !strings.Contains(src, `launch\_authorisation:%`) {
		t.Fatalf("the verifier must carry the launch_authorisation family: err=%v", err)
	}

	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down with no request in flight: %v", err)
	}
	if got := lgSnapshot(t, pool); got != snap129 {
		t.Fatalf("down did not restore the 0129 schema byte for byte:\n%s", lgSnapDiff(snap129, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := lgSnapshot(t, pool); got != snap130 {
		t.Fatalf("up after down differs from the first up:\n%s", lgSnapDiff(snap130, got))
	}
}

// S13 style: the down refuses, BEFORE dropping anything, while a launch request is pending or executing.
func TestMigration0130_Down_RefusesWhileARequestIsInFlight(t *testing.T) {
	ctx := context.Background()
	pool, url := lgScratch(t, "m0130d_")
	full := lgMigDir(t, migration0130Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	prooftest.InstallVia(t, pool.Raw(), t.Name())
	launchfix.SeedOperatorsAt(t, url)
	ops := launchfix.Operators()

	var tenantID, reqID uuid.UUID
	var hash string
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0130", "m0130-"+uuid.NewString(), "under_platform_licence")
		if err != nil {
			return err
		}
		tenantID = tn.ID
		d := actorproof.LaunchRequest{TenantID: tenantID, SubjectKind: "tenant", Action: "close",
			ReadinessSnapshotHash: strings.Repeat("0", 64), ReasonCode: "offboarding"}
		if err := prooftest.AttachForSession(ctx, tx, actorproof.OpLaunchRequest, actorproof.TargetNew, d.Digest()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO launch_authorisation_requests
			(tenant_id, subject_kind, action, reason_code, readiness_snapshot, readiness_snapshot_hash)
			VALUES ($1, 'tenant', 'close', 'offboarding', '{}'::jsonb, repeat('0', 64)) RETURNING id, payload_hash`, tenantID).Scan(&reqID, &hash)
	}); err != nil {
		t.Fatalf("file a request with a real proof: %v", err)
	}
	_, err := pool.MigrateDown(ctx, full, 1)
	if pgCodeOf(err) != "LA099" {
		t.Fatalf("down must refuse with LA099 while a request is pending, got %v", err)
	}
	var n int
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname IN ('zz_actor_proof_guard', 'launch_requests_reject_carryover', 'launch_suspension_no_stale_pending_at_commit')
			AND tgrelid IN ('launch_authorisation_requests'::regclass, 'launch_authorisation_approvals'::regclass) AND NOT tgisinternal`).Scan(&n)
	}); err != nil || n != 4 {
		t.Fatalf("a refused down dropped something: %d err=%v", n, err)
	}
	// Terminal history does not block: cancel (with the requester's proof) and the down runs.
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, actorproof.OpLaunchCancel, reqID.String(), hash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1`, reqID)
		return err
	}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down with only terminal history: %v", err)
	}
}
