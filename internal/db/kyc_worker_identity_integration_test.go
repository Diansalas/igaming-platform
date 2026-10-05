//go:build integration

// PRH-2 E1 (ADR 0106 section 3.3, security F1/F8; plan T-ID-1..4): the
// dedicated KYC worker identity (kyc_submission_worker) is least privilege.
// With only app.platform_service_id = 'kyc_submission_worker' set (no tenant
// GUC), it can claim an outbox row and read the public reference allowlist,
// and nothing else: the legacy NULL-tenant read AND write arms on nine older
// tables (proven open for any no-tenant session at 3517980) are fenced by the
// 36 kyc_worker_fence_* restrictive policies of migration 0114.
//
// All probes run as the RUNTIME role (never a superuser or BYPASSRLS role),
// asserted before anything else; fixtures use the owner role.
package db

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// kycRuntimePool connects as the runtime role and asserts it is neither a
// superuser nor BYPASSRLS. maxConns 1 forces connection REUSE (the ” form of a
// reset GUC) across probes.
func kycRuntimePool(t *testing.T, maxConns int32) *Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role identity test")
	}
	pool, err := Connect(context.Background(), url, maxConns, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime role: %v", err)
	}
	t.Cleanup(pool.Close)
	var super, bypass bool
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatalf("read role attributes: %v", err)
	}
	if super || bypass {
		t.Fatalf("identity tests must run as a role that is neither superuser nor BYPASSRLS (super=%v bypassrls=%v)", super, bypass)
	}
	return pool
}

// kycSeed is one orphan verification with a pending create outbox row.
type kycSeed struct {
	tenantID, brandID, personID, playerID, verificationID, outboxID uuid.UUID
}

// seedKYCOutbox seeds a tenant, a player account, an orphan verification and
// its pending create row, via the owner pool (raw SQL; the guard trigger
// validates the row exactly as in production).
func seedKYCOutbox(t *testing.T, owner *Pool) kycSeed {
	t.Helper()
	s := kycSeed{tenantID: createTestTenant(t, owner)}
	ctx := context.Background()
	s.personID = uuid.New()
	if err := owner.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id, status) VALUES ($1, 'unverified')`, s.personID)
		return err
	}); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	err := owner.WithTenant(ctx, s.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s.brandID, s.playerID, s.verificationID = uuid.New(), uuid.New(), uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'KYC Test Brand')`,
			s.brandID, s.tenantID, "b-"+s.brandID.String()[:8]); err != nil {
			return fmt.Errorf("brand: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			s.playerID, s.tenantID, s.brandID, s.personID, s.playerID.String()+"@example.com"); err != nil {
			return fmt.Errorf("player: %w", err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference)
			 VALUES ($1, $2, $3, $4, $5, 'unverified', 'mock', NULL)`,
			s.verificationID, s.tenantID, s.brandID, s.playerID, s.personID); err != nil {
			return fmt.Errorf("verification: %w", err)
		}
		return tx.QueryRow(ctx,
			`INSERT INTO kyc_submission_outbox (tenant_id, verification_id, operation, provider_id, idempotency_key)
			 VALUES ($1, $2, 'create', 'mock', $3) RETURNING id`,
			s.tenantID, s.verificationID, "kv:"+s.verificationID.String()).Scan(&s.outboxID)
	})
	if err != nil {
		t.Fatalf("seed KYC outbox row: %v", err)
	}
	return s
}

func workerTx(pool *Pool, fn TxFunc) error {
	return pool.WithPlatformService(context.Background(), ServiceKYCSubmissionWorker, fn)
}

func isCode(err error, codes ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	for _, c := range codes {
		if pgErr.Code == c {
			return true
		}
	}
	return false
}

// seedFencedRows creates at least one NULL-tenant (platform-level) row in each
// of the nine fenced tables via a session that legitimately may write them
// (WithoutTenant / platform admin), so a worker count of 0 is the fence and not
// an empty seed. Returns the table -> platform-row id map for the denial probes.
func seedFencedRows(t *testing.T, owner *Pool) map[string]uuid.UUID {
	t.Helper()
	ctx := context.Background()
	ids := map[string]uuid.UUID{}
	admin := uuid.New()
	must := func(name string, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("seed fenced table %s: %v", name, err)
		}
	}
	// staff_users, audit_log, login_attempts: dual_scope NULL arm (WithoutTenant).
	staffID := uuid.New()
	must("staff_users", owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, 'x', 'platform_admin')`,
			staffID, "kycfence-"+staffID.String()+"@platform.test")
		return err
	}))
	ids["staff_users"] = staffID
	must("audit_log", owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO audit_log (tenant_id, actor_type, action, outcome) VALUES (NULL, 'system', 'kyc.fence_probe_seed', 'success') RETURNING id`).Scan(new(uuid.UUID))
	}))
	must("login_attempts", owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO login_attempts (tenant_id, principal_type, identifier, succeeded) VALUES (NULL, 'staff', 'kycfence@platform.test', false)`)
		return err
	}))
	must("sessions", owner.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO sessions (principal_type, principal_id, tenant_id, refresh_token_hash, expires_at)
		                         VALUES ('staff', $1, NULL, $2, now() + interval '1 hour')`, staffID, "kycfence-"+uuid.NewString())
		return err
	}))
	must("persons", owner.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		id := uuid.New()
		ids["persons"] = id
		_, err := tx.Exec(ctx, `INSERT INTO persons (id, status) VALUES ($1, 'unverified')`, id)
		return err
	}))
	must("risk_rules", owner.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO risk_rules (tenant_id, operation, limit_kind, time_window, threshold, threshold_exponent, created_by_actor_type, created_by_actor_id)
			 VALUES (NULL, 'deposit', 'max_amount', 'transaction', 100, 2, 'staff', $1) RETURNING id`, admin).Scan(new(uuid.UUID))
	}))
	must("player_restrictions", owner.WithPlatformAdmin(ctx, admin, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO player_restrictions (person_id, tenant_id, restriction_type, source, created_by_actor_type, created_by_actor_id)
			 VALUES ($1, NULL, 'self_exclusion', 'staff', 'staff', $2) RETURNING id`, ids["persons"], admin).Scan(new(uuid.UUID))
	}))
	k2SeedNullTenantFencedRows(t, owner) // asset_operation_eligibility + open_bet_self_exclusion_policies
	return ids
}

// fencedTables are the nine tables of ADR 0106 section 3.3.
var fencedTables = []string{
	"staff_users", "audit_log", "sessions", "login_attempts", "risk_rules",
	"player_restrictions", "persons", "asset_operation_eligibility", "open_bet_self_exclusion_policies",
}

// workerReferenceAllowlist is ADR 0106 section 3.3's public reference allowlist
// (ADR 0099 section 6.2's reviewed a18SelectAllowlist restricted to what the
// worker can see, plus schema_migrations).
var workerReferenceAllowlist = map[string]bool{
	"tenants": true, "alert_kinds": true, "assets": true, "brands": true, "casino_games": true,
	"financial_capability_catalogue": true, "financial_capability_settings": true,
	"financial_control_classifications": true, "financial_governance_permissions": true,
	"jurisdiction_precedence_configs": true, "jurisdictions": true, "kyc_enforcement_policies": true,
	"ledger_adjustment_reason_codes": true, "platform_operations": true, "platform_products": true,
	"sb_competitions": true, "sb_events": true, "sb_jurisdiction_restrictions": true, "sb_markets": true,
	"sb_selections": true, "sb_sports": true, "schema_migrations": true,
}

// T-ID-1: the worker sees every tenant's outbox rows, claims a due row, and the
// trigger forces the claim columns.
func TestKYCWorkerIdentity_T_ID_1_SeesAllOutboxRowsAndClaims(t *testing.T) {
	owner := testPool(t)
	rt := kycRuntimePool(t, 5)
	a, b := seedKYCOutbox(t, owner), seedKYCOutbox(t, owner)

	var seen int
	if err := workerTx(rt, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_submission_outbox WHERE id = ANY ($1)`, []uuid.UUID{a.outboxID, b.outboxID}).Scan(&seen)
	}); err != nil {
		t.Fatal(err)
	}
	if seen != 2 {
		t.Fatalf("the worker must see both tenants' outbox rows, saw %d", seen)
	}

	var token *uuid.UUID
	var state, by string
	var claims int
	var lease *time.Time
	if err := workerTx(rt, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`UPDATE kyc_submission_outbox SET state = 'claimed', lease_expires_at = now() + interval '60 seconds'
			  WHERE id = $1 RETURNING state, claim_token, claimed_by_service, claims, lease_expires_at`, a.outboxID).
			Scan(&state, &token, &by, &claims, &lease)
	}); err != nil {
		t.Fatalf("worker claim: %v", err)
	}
	if state != "claimed" || token == nil || by != "kyc_submission_worker" || claims != 1 || lease == nil {
		t.Fatalf("claim columns not forced: state=%s token=%v by=%q claims=%d lease=%v", state, token, by, claims, lease)
	}
}

// T-ID-2: denials, including every NULL-tenant variant security proved open for
// a no-tenant session at 3517980. Each statement runs under ONLY the worker GUC.
func TestKYCWorkerIdentity_T_ID_2_NullTenantWritesAndReadsDenied(t *testing.T) {
	owner := testPool(t)
	rt := kycRuntimePool(t, 5)
	seedFencedRows(t, owner)
	seed := seedKYCOutbox(t, owner)

	type probe struct {
		name string
		sql  string
		args []any
	}
	// Writes: must be refused (42501) or affect zero rows.
	writes := []probe{
		{"INSERT platform staff_users platform_admin (security F1 proof)", `INSERT INTO staff_users (tenant_id, email, password_hash, role) VALUES (NULL, 'forged-admin@platform.test', 'x', 'platform_admin')`, nil},
		{"INSERT platform audit_log (security F1 proof)", `INSERT INTO audit_log (tenant_id, actor_type, action, outcome) VALUES (NULL, 'system', 'forged', 'success')`, nil},
		{"UPDATE staff_users", `UPDATE staff_users SET status = 'suspended' WHERE tenant_id IS NULL`, nil},
		{"DELETE staff_users", `DELETE FROM staff_users WHERE tenant_id IS NULL`, nil},
		{"risk_rules INSERT", `INSERT INTO risk_rules (tenant_id, operation, limit_kind, time_window, threshold, threshold_exponent, created_by_actor_type, created_by_actor_id) VALUES (NULL, 'deposit', 'max_amount', 'transaction', 1, 2, 'staff', gen_random_uuid())`, nil},
		{"risk_rules UPDATE", `UPDATE risk_rules SET threshold = 1 WHERE tenant_id IS NULL`, nil},
		{"risk_rules DELETE", `DELETE FROM risk_rules WHERE tenant_id IS NULL`, nil},
		{"player_restrictions staff insert", `INSERT INTO player_restrictions (person_id, tenant_id, restriction_type, source, created_by_actor_type, created_by_actor_id) VALUES (gen_random_uuid(), NULL, 'self_exclusion', 'staff', 'staff', gen_random_uuid())`, nil},
		{"sessions insert", `INSERT INTO sessions (principal_type, principal_id, tenant_id, refresh_token_hash, expires_at) VALUES ('staff', gen_random_uuid(), NULL, 'forged', now() + interval '1 hour')`, nil},
		{"sessions update", `UPDATE sessions SET revoked_at = now() WHERE tenant_id IS NULL`, nil},
		{"login_attempts insert", `INSERT INTO login_attempts (tenant_id, principal_type, identifier, succeeded) VALUES (NULL, 'staff', 'x', true)`, nil},
		{"login_attempts delete", `DELETE FROM login_attempts WHERE tenant_id IS NULL`, nil},
		{"persons INSERT", `INSERT INTO persons (id, status) VALUES (gen_random_uuid(), 'unverified')`, nil},
		{"persons UPDATE (clearing excluded)", `UPDATE persons SET status = 'unverified'`, nil},
		{"persons DELETE", `DELETE FROM persons WHERE id NOT IN (SELECT person_id FROM player_accounts)`, nil},
		{"alerts INSERT for the KYC Kind", `INSERT INTO alerts (kind, severity, discriminator, subject_tenant_id) VALUES ('kyc.submission_failed_terminal', 'p2', 'create:mock', $1)`, []any{seed.tenantID}},
		{"alert_deliveries INSERT", `INSERT INTO alert_deliveries (alert_id) VALUES (gen_random_uuid())`, nil},
		{"outbox INSERT", `INSERT INTO kyc_submission_outbox (tenant_id, verification_id, operation, provider_id, idempotency_key) VALUES ($1, $2, 'create', 'mock', $3)`, []any{seed.tenantID, seed.verificationID, "kv:" + seed.verificationID.String()}},
		{"outbox non-claim transition (sent)", `UPDATE kyc_submission_outbox SET state = 'sent' WHERE id = $1`, []any{seed.outboxID}},
		{"outbox DELETE", `DELETE FROM kyc_submission_outbox WHERE id = $1`, []any{seed.outboxID}},
	}
	for _, p := range writes {
		var affected int64
		var execErr error
		err := workerTx(rt, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, p.sql, p.args...)
			affected, execErr = tag.RowsAffected(), err
			return errors.New("rollback") // never commit a probe
		})
		_ = err
		if execErr == nil && affected != 0 {
			t.Errorf("T-ID-2 %s: the worker session changed %d row(s)", p.name, affected)
		}
		if execErr != nil && !isCode(execErr, "42501", "P0001") {
			t.Errorf("T-ID-2 %s: expected 42501 (RLS) or P0001 (guard), got %v", p.name, execErr)
		}
	}

	// Reads: zero rows on every fenced table and on the tenant tables.
	for _, tbl := range append(append([]string{}, fencedTables...),
		"kyc_verifications", "kyc_documents", "ledger_transactions", "ledger_entries", "payment_attempts",
		"withdrawal_requests", "staff_capability_grants", "ledger_adjustment_requests",
		"alerts", "alert_occurrences", "alert_deliveries") {
		var n int64
		if err := workerTx(rt, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{tbl}.Sanitize()).Scan(&n)
		}); err != nil {
			t.Errorf("T-ID-2 read %s: %v", tbl, err)
			continue
		}
		if n != 0 {
			t.Errorf("T-ID-2: the worker session sees %d row(s) of %s", n, tbl)
		}
	}
	// password_hash of a platform staff row is unreachable.
	var n int64
	if err := workerTx(rt, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(password_hash) FROM staff_users`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("password_hash of staff rows reachable by the worker session: %d", n)
	}

	// alerting_session_scope() refuses the worker identity (never edited by E1).
	err := workerTx(rt, func(ctx context.Context, tx pgx.Tx) error {
		var s string
		return tx.QueryRow(ctx, `SELECT alerting_session_scope()`).Scan(&s)
	})
	if err == nil {
		t.Fatal("alerting_session_scope() must refuse the kyc_submission_worker identity")
	}
}

// T-ID-3: the visibility-0 allowlist probe (the K2-G1 shape).
func TestKYCWorkerIdentity_T_ID_3_VisibilityZeroExceptOutboxAndAllowlist(t *testing.T) {
	owner := testPool(t)
	rt := kycRuntimePool(t, 5)
	seedFencedRows(t, owner)
	seedKYCOutbox(t, owner)
	assertWorkerVisibility(t, rt)
}

func assertWorkerVisibility(t *testing.T, rt *Pool) {
	t.Helper()
	ctx := context.Background()
	var tables []string
	if err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = 'public' ORDER BY tablename`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			tables = append(tables, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("list tables: %v", err)
	}
	if len(tables) < 100 {
		t.Fatalf("expected the full public schema (>100 tables), got %d - the enumeration is broken", len(tables))
	}

	count := func(run func(context.Context, TxFunc) error, table string) (int64, bool) {
		var n int64
		failed := false
		_ = run(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{table}.Sanitize()).Scan(&n); err != nil {
				failed = true
			}
			return errors.New("rollback")
		})
		return n, !failed
	}
	workerRun := func(c context.Context, fn TxFunc) error {
		return rt.WithPlatformService(c, ServiceKYCSubmissionWorker, fn)
	}
	noTenantRun := func(c context.Context, fn TxFunc) error { return rt.WithoutTenant(c, fn) }

	var unexpected, exceeds []string
	visible := map[string]int64{}
	for _, tbl := range tables {
		w, ok := count(workerRun, tbl)
		if !ok {
			continue // not readable at all: not visible
		}
		wt, wtOK := count(noTenantRun, tbl)
		// The outbox is the one table the worker legitimately sees more of.
		if wtOK && w > wt && tbl != "kyc_submission_outbox" {
			exceeds = append(exceeds, fmt.Sprintf("%s (worker %d > WithoutTenant %d)", tbl, w, wt))
		}
		if w == 0 {
			continue
		}
		visible[tbl] = w
		if tbl != "kyc_submission_outbox" && !workerReferenceAllowlist[tbl] {
			unexpected = append(unexpected, fmt.Sprintf("%s (%d rows)", tbl, w))
		}
	}
	sort.Strings(unexpected)
	sort.Strings(exceeds)
	if len(unexpected) > 0 {
		t.Errorf("T-ID-3: the worker session sees rows in table(s) on neither the outbox nor the public reference allowlist:\n  %s", strings.Join(unexpected, "\n  "))
	}
	if len(exceeds) > 0 {
		t.Errorf("T-ID-3: the worker session sees MORE than a WithoutTenant session:\n  %s", strings.Join(exceeds, "\n  "))
	}
	// Non-vacuity: the worker does see the outbox; and every fenced table has
	// rows a WithoutTenant (or platform) session CAN see, so the worker's 0 is
	// the fence, not an empty seed.
	if visible["kyc_submission_outbox"] < 1 {
		t.Fatal("the worker must see the outbox (probe is vacuous otherwise)")
	}
	// (sessions' SELECT policies carry no NULL arm - its exposure was INSERT and
	// UPDATE, covered by T-ID-2's write probes - so it is not in this list.)
	for _, tbl := range []string{"staff_users", "audit_log", "persons", "login_attempts", "risk_rules", "player_restrictions"} {
		wt, ok := count(noTenantRun, tbl)
		if !ok || wt < 1 {
			t.Errorf("non-vacuity: a WithoutTenant session must see seeded rows in %s (saw %d, readable=%v)", tbl, wt, ok)
		}
	}
	for _, tbl := range []string{"asset_operation_eligibility", "open_bet_self_exclusion_policies"} {
		var n int64
		if err := rt.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM `+pgx.Identifier{tbl}.Sanitize()+` WHERE tenant_id IS NULL`).Scan(&n)
		}); err != nil || n < 1 {
			t.Errorf("non-vacuity: a platform session must see seeded NULL-tenant rows in %s (saw %d, err=%v)", tbl, n, err)
		}
	}
}

// T-ID-4: the identity probes on a REUSED pooled connection (the ” form of a
// reset transaction-local GUC) as well as a fresh one. A single-connection pool
// is dirtied by a tenant transaction first.
func TestKYCWorkerIdentity_T_ID_4_FreshAndReusedConnection(t *testing.T) {
	owner := testPool(t)
	seedFencedRows(t, owner)
	seed := seedKYCOutbox(t, owner)
	one := kycRuntimePool(t, 1)

	probe := func(label string) {
		t.Helper()
		var staff, audit, persons, outbox int64
		if err := workerTx(one, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_users`).Scan(&staff); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&audit); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM persons`).Scan(&persons); err != nil {
				return err
			}
			return tx.QueryRow(ctx, `SELECT count(*) FROM kyc_submission_outbox WHERE id = $1`, seed.outboxID).Scan(&outbox)
		}); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if staff != 0 || audit != 0 || persons != 0 || outbox != 1 {
			t.Fatalf("%s: worker saw staff=%d audit=%d persons=%d (want 0) outbox=%d (want 1)", label, staff, audit, persons, outbox)
		}
		var execErr error
		_ = workerTx(one, func(ctx context.Context, tx pgx.Tx) error {
			_, execErr = tx.Exec(ctx, `INSERT INTO staff_users (tenant_id, email, password_hash, role) VALUES (NULL, 'forged@platform.test', 'x', 'platform_admin')`)
			return errors.New("rollback")
		})
		if !isCode(execErr, "42501") {
			t.Fatalf("%s: the platform_admin insert must be refused with 42501, got %v", label, execErr)
		}
	}
	probe("fresh connection")
	// Dirty the only connection with a tenant transaction (leaves app.tenant_id
	// as '' on reuse), then probe again on that very connection.
	if err := one.WithTenant(context.Background(), seed.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	probe("reused connection after a tenant transaction")
	var form string
	if err := one.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT coalesce(current_setting('app.platform_service_id', true), 'NULL')`).Scan(&form)
	}); err != nil {
		t.Fatal(err)
	}
	t.Logf("GUC form observed on the reused connection after commit: %q (NULL and '' are both handled by NULLIF)", form)
}
