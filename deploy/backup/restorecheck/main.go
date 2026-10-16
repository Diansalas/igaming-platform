// Command restorecheck is the application-level half of deploy/backup/verify.sh (A12, ADR 0122): it connects to a
// RESTORED database exactly as the application would (the non-owning igaming_runtime role, DATABASE_URL in the
// environment, never argv) and exercises the platform's OWN code against it:
//
//   - the platform's reconciliation stream ledger_vs_projection (internal/reconciliation.RunLedgerVsProjection) for every
//     tenant, which recomputes each account balance from ledger_entries and diffs it against the projection under the
//     real RLS and trigger regime. Any mismatch is drift, which CLAUDE.md defines as a P1 incident. NOTE: a run writes
//     one reconciliation_runs row per tenant (that is what the platform does), so verify.sh runs this AFTER the
//     data comparison;
//
//   - RLS is really enforced for the runtime role in the restored database (not merely flagged): without a tenant scope
//     it sees no tenant rows, and with a tenant scope it sees only that tenant's rows;
//
//   - the startup guard db.VerifyEnvironmentAnchor behaves as documented: a production process is refused on a restored
//     database whose anchor is unrecorded (a restore of a dev/test database, or a fresh cluster), and admitted once the
//     owner has recorded 'production'; a development process is never blocked.
//
//   - the ACTOR-PROOF key state (owner decision 5, ADR 0122 section 10; -actor-proof): a LOGICAL restore carries no actor-proof signing secret,
//     so `unprovisioned` asserts that the platform's own production startup gate (actorproof.VerifyConfiguredInProduction) REFUSES for the
//     given kid(s) and that the SQL verifier answers a well-formed proof with AP002; `provisioned` asserts that the gate admits the NEW kid and
//     refuses the retired ones. This binary never holds, reads or signs with a real key: it only presents a well-formed but unsigned token, so it
//     can prove the fail-closed state but not a successful verification (the drill proves that at the SQL level with a throwaway key).
//
// Output is line oriented ("CHK|name|PASS|detail") and the exit status is non-zero if any check fails.
//
//	DATABASE_URL=postgres://igaming_runtime:...@127.0.0.1:5498/db restorecheck -expect-anchor unrecorded
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

var failed int

func chk(name string, ok bool, detail string, a ...any) {
	st := "PASS"
	if !ok {
		st = "FAIL"
		failed++
	}
	fmt.Printf("CHK|%s|%s|%s\n", name, st, fmt.Sprintf(detail, a...))
}

func main() {
	expect := flag.String("expect-anchor", "unrecorded", "expected database_environment anchor: unrecorded | production | development | test")
	persist := flag.Bool("persist", false, "keep the reconciliation_runs rows (default: roll the run back)")
	apMode := flag.String("actor-proof", "skip", "actor-proof key state of the restored database: unprovisioned | provisioned | skip")
	apKID := flag.String("actor-proof-kid", "", "the NEW actor-proof key id (provisioned mode; optional extra probe in unprovisioned mode)")
	var retired []string
	flag.Func("retired-kid", "an actor-proof key id of the source that must NOT be an active key (repeatable)", func(v string) error {
		retired = append(retired, v)
		return nil
	})
	flag.Parse()
	if err := run(*expect, *persist, actorProofCheck{mode: *apMode, kid: *apKID, retired: retired}); err != nil {
		fmt.Fprintln(os.Stderr, "restorecheck:", err)
		os.Exit(2)
	}
	if failed > 0 {
		fmt.Printf("restorecheck: %d check(s) FAILED\n", failed)
		os.Exit(1)
	}
}

var errRollback = errors.New("restorecheck: deliberate rollback of the reconciliation run")

type actorProofCheck struct {
	mode    string
	kid     string
	retired []string
}

var kidRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

func (a actorProofCheck) validate() error {
	switch a.mode {
	case "skip":
		return nil
	case "unprovisioned":
	case "provisioned":
		if !kidRE.MatchString(a.kid) {
			return errors.New("-actor-proof provisioned needs -actor-proof-kid")
		}
	default:
		return fmt.Errorf("-actor-proof must be unprovisioned, provisioned or skip (got %q)", a.mode)
	}
	if a.kid != "" && !kidRE.MatchString(a.kid) {
		return errors.New("-actor-proof-kid is not a valid key id")
	}
	for _, k := range a.retired {
		if !kidRE.MatchString(k) {
			return errors.New("-retired-kid is not a valid key id")
		}
	}
	return nil
}

// startupGate runs the platform's own production startup gate for kid with a throwaway issuer (random key, never stored): the gate only asks
// the database whether the kid is an ACTIVE key, so the key bytes are irrelevant and no real secret is needed.
func startupGate(ctx context.Context, pool *db.Pool, kid string) error {
	key := make([]byte, actorproof.MinKeyBytes)
	if _, err := rand.Read(key); err != nil {
		return err
	}
	iss, err := actorproof.NewIssuer(kid, map[string][]byte{kid: key})
	if err != nil {
		return err
	}
	return actorproof.VerifyConfiguredInProduction(ctx, "production", iss, pool)
}

func checkActorProof(ctx context.Context, pool *db.Pool, a actorProofCheck, tenants []uuid.UUID) error {
	if a.mode == "skip" {
		return nil
	}
	probe := append([]string{}, a.retired...)
	if a.kid != "" {
		probe = append(probe, a.kid)
	}
	if len(probe) == 0 {
		probe = append(probe, "restorecheck-probe")
	}
	if a.mode == "unprovisioned" {
		for _, k := range probe {
			err := startupGate(ctx, pool, k)
			chk("actor_proof_startup_gate_refuses_"+k, err != nil && strings.Contains(err.Error(), "refusing to start"), "kid=%s err=%v (a production platform-api must refuse to start until a NEW key is provisioned)", k, err)
		}
		if len(tenants) == 0 {
			chk("actor_proof_verifier_fails_closed", false, "no tenant to scope the probe")
			return nil
		}
		// a well-formed proof token (valid shape, random MAC) must be refused with AP002 (unknown/inactive key): no proof can verify
		now := time.Now().Unix()
		mac := make([]byte, 32)
		if _, err := rand.Read(mac); err != nil {
			return err
		}
		actor := uuid.NewString()
		zeros := strings.Repeat("0", 64)
		token := strings.Join([]string{"v1", probe[0], actor, "tenant", tenants[0].String(), "ledger_adjustment:initiate", "new", zeros,
			fmt.Sprint(now), fmt.Sprint(now + 30), "restorecheck-nonce-0001", hex.EncodeToString(mac)}, "|")
		errRB := errors.New("rollback")
		err := pool.WithTenant(ctx, tenants[0], func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, token); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT actor_proof_require($1::uuid, 'tenant', $2::uuid, 'ledger_adjustment:initiate', 'new', $3)`, actor, tenants[0], zeros); err != nil {
				return err
			}
			return errRB
		})
		var pe *pgconn.PgError
		ok := errors.As(err, &pe) && pe.Code == "AP002"
		chk("actor_proof_verifier_fails_closed", ok, "a well-formed proof presented to actor_proof_require is refused with AP002 (got %v)", err)
		return nil
	}
	// provisioned
	err := startupGate(ctx, pool, a.kid)
	chk("actor_proof_startup_gate_admits_new_kid", err == nil, "kid=%s err=%v", a.kid, err)
	for _, k := range a.retired {
		err := startupGate(ctx, pool, k)
		chk("actor_proof_startup_gate_refuses_retired_"+k, err != nil && strings.Contains(err.Error(), "refusing to start"), "kid=%s err=%v", k, err)
	}
	return nil
}

func run(expectAnchor string, persist bool, ap actorProofCheck) error {
	if err := ap.validate(); err != nil {
		return err
	}
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return errors.New("DATABASE_URL (the igaming_runtime credential) is required")
	}
	ctx := context.Background()
	pool, err := db.Connect(ctx, url, 4, 10*time.Second)
	if err != nil {
		return fmt.Errorf("connect as the runtime role: %w", err)
	}
	defer pool.Close()

	var role string
	var super, bypass bool
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_user, r.rolsuper, r.rolbypassrls FROM pg_roles r WHERE r.rolname = current_user`).Scan(&role, &super, &bypass)
	}); err != nil {
		return err
	}
	if role != "igaming_runtime" || super || bypass {
		return fmt.Errorf("DATABASE_URL must be the non-owning igaming_runtime role (got %s super=%v bypassrls=%v)", role, super, bypass)
	}

	// tenants (platform scope read, as the reconciliation scheduler does)
	var tenants []uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id FROM tenants ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			tenants = append(tenants, id)
		}
		return rows.Err()
	}); err != nil {
		return fmt.Errorf("list tenants: %w", err)
	}
	chk("restorecheck_tenants_visible", len(tenants) > 0, "tenants=%d", len(tenants))

	// RLS is enforced for the runtime role: no scope => no tenant rows; a scope => only that tenant's rows
	var unscoped int64
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&unscoped)
	}); err != nil {
		return fmt.Errorf("unscoped read: %w", err)
	}
	chk("runtime_role_unscoped_sees_no_ledger_rows", unscoped == 0, "visible_ledger_entries=%d", unscoped)
	if len(tenants) > 0 {
		var own, foreign int64
		if err := pool.WithTenant(ctx, tenants[0], func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries`).Scan(&own); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id <> $1`, tenants[0]).Scan(&foreign)
		}); err != nil {
			return fmt.Errorf("scoped read: %w", err)
		}
		chk("runtime_role_scoped_sees_only_own_tenant", own > 0 && foreign == 0, "own_rows=%d foreign_rows=%d", own, foreign)
	}

	// the platform's own reconciliation, per tenant, with the runtime role
	now := time.Now().UTC()
	totalMismatch := 0
	for _, tid := range tenants {
		var mism int
		var status reconciliation.Status
		err := pool.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
			run, ms, err := reconciliation.RunLedgerVsProjection(ctx, tx, tid, now.Add(-24*time.Hour), now)
			mism, status = len(ms), run.Status
			if err == nil && !persist {
				return errRollback
			}
			return err
		})
		if errors.Is(err, errRollback) {
			err = nil
		}
		if err != nil {
			chk("recon_ledger_vs_projection_"+tid.String(), false, "run failed: %v", err)
			continue
		}
		totalMismatch += mism
		chk("recon_ledger_vs_projection_"+tid.String(), mism == 0 && status == reconciliation.StatusClean, "status=%s mismatches=%d", status, mism)
	}
	chk("recon_ledger_vs_projection_total_drift", totalMismatch == 0, "tenants=%d drift_mismatches=%d (any non-zero drift is a P1 incident)", len(tenants), totalMismatch)

	// startup guard behaviour (ADR 0115 S1)
	anchor, err := pool.ReadEnvironmentAnchor(ctx)
	if err != nil {
		return fmt.Errorf("read anchor: %w", err)
	}
	got := anchor
	if got == "" {
		got = "unrecorded"
	}
	chk("environment_anchor_state", got == expectAnchor, "anchor=%s expected=%s", got, expectAnchor)
	_, prodErr := db.VerifyEnvironmentAnchor(ctx, "production", pool)
	switch expectAnchor {
	case "production":
		chk("startup_guard_production_process_admitted", prodErr == nil, "err=%v", prodErr)
	case "unrecorded":
		chk("startup_guard_production_process_refused_when_unrecorded", errors.Is(prodErr, db.ErrEnvironmentAnchorUnrecorded), "err=%v (re-provision: INSERT INTO database_environment (environment) VALUES ('production') as the owner, BEFORE starting platform-api in production)", prodErr)
	default:
		chk("startup_guard_production_process_refused_on_dev_or_test_anchor", errors.Is(prodErr, db.ErrEnvironmentAnchorMismatch), "err=%v", prodErr)
	}
	_, devErr := db.VerifyEnvironmentAnchor(ctx, "development", pool)
	chk("startup_guard_development_process_never_blocked", devErr == nil, "err=%v", devErr)

	return checkActorProof(ctx, pool, ap, tenants)
}
