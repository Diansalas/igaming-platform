//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 section 10.3): tenant isolation, the exclusion matrix of
// every outbox policy, the guard trigger (immutability, claim, lease, deferral),
// insert validation and Go/SQL key parity. All as the RUNTIME role (never a
// superuser or BYPASSRLS role, asserted by rtPool); each probe runs in a
// transaction that is rolled back.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

var errProbeRollback = errors.New("probe: roll back")

func pgCode(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

// probeTx runs fn in a transaction scoped to tenant (uuid.Nil = no tenant),
// first applying extra GUCs, and ALWAYS rolls back.
func probeTx(t *testing.T, pool *db.Pool, tenant uuid.UUID, gucs map[string]string, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	run := func(ctx context.Context, tx pgx.Tx) error {
		for k, v := range gucs {
			if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, k, v); err != nil {
				t.Fatalf("set GUC %s: %v", k, err)
			}
		}
		fn(ctx, tx)
		return errProbeRollback
	}
	var err error
	if tenant != uuid.Nil {
		err = pool.WithTenant(context.Background(), tenant, run)
	} else {
		err = pool.WithoutTenant(context.Background(), run)
	}
	if err != nil && !errors.Is(err, errProbeRollback) {
		t.Fatalf("probe transaction: %v", err)
	}
}

// spExec runs one statement inside a savepoint so a refusal does not abort the
// probe transaction (several refusals can be probed in one transaction).
func spExec(ctx context.Context, tx pgx.Tx, sql string, args ...any) (int64, error) {
	if _, err := tx.Exec(ctx, `SAVEPOINT probe_sp`); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		if _, rbErr := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT probe_sp`); rbErr != nil {
			return 0, rbErr
		}
		return 0, err
	}
	if _, relErr := tx.Exec(ctx, `RELEASE SAVEPOINT probe_sp`); relErr != nil {
		return 0, relErr
	}
	return tag.RowsAffected(), nil
}

// probeWorker is probeTx under the real worker identity.
func probeWorker(t *testing.T, pool *db.Pool, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	err := pool.WithPlatformService(context.Background(), db.ServiceKYCSubmissionWorker, func(ctx context.Context, tx pgx.Tx) error {
		fn(ctx, tx)
		return errProbeRollback
	})
	if err != nil && !errors.Is(err, errProbeRollback) {
		t.Fatalf("worker probe transaction: %v", err)
	}
}

// rawOrphan inserts an orphan verification for the fixture's player with no
// outbox row (the target of insert probes).
func rawOrphan(t *testing.T, pool *db.Pool, f fixture) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference)
		                        VALUES ($1, $2, $3, $4, $5, 'unverified', 'mock', NULL)`, id, f.tenantID, f.brandID, f.playerID, f.personID)
		return err
	}); err != nil {
		t.Fatalf("raw orphan: %v", err)
	}
	return id
}

const insertCreateSQL = `INSERT INTO kyc_submission_outbox (tenant_id, verification_id, operation, provider_id, idempotency_key)
	VALUES ($1, $2, 'create', 'mock', $3)`

// disableOutboxGuard disables ONLY the guard row trigger (committed, owner DDL)
// so a test can observe the RLS layer ALONE; it re-enables it on cleanup. The
// guard runs BEFORE the RLS WITH CHECK, so with it enabled a refusal may come
// from either layer: the layered assertions below require the guard-enabled
// refusal to be 42501 or P0001, and the RLS-only run to be exactly the policy.
func disableOutboxGuard(t *testing.T) func() {
	t.Helper()
	conn := func(sql string) {
		url := os.Getenv("TEST_DATABASE_URL")
		c, err := pgx.Connect(context.Background(), url)
		if err != nil {
			t.Fatalf("owner connect: %v", err)
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	conn(`ALTER TABLE kyc_submission_outbox DISABLE TRIGGER kyc_submission_outbox_guard_row`)
	restored := false
	restore := func() {
		if !restored {
			restored = true
			conn(`ALTER TABLE kyc_submission_outbox ENABLE TRIGGER kyc_submission_outbox_guard_row`)
		}
	}
	t.Cleanup(restore)
	return restore
}

// openOutboxPolicies adds a temporary permissive FOR ALL policy (committed owner
// DDL, dropped on cleanup) so the RLS layer admits everything and the guard
// TRIGGER alone decides: the third layer of the defence in depth (RLS, trigger,
// Go CAS), needed to prove each layer independently.
func openOutboxPolicies(t *testing.T) {
	t.Helper()
	conn := func(sql string) {
		c, err := pgx.Connect(context.Background(), os.Getenv("TEST_DATABASE_URL"))
		if err != nil {
			t.Fatalf("owner connect: %v", err)
		}
		defer func() { _ = c.Close(context.Background()) }()
		if _, err := c.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	conn(`CREATE POLICY kso_test_open_all ON kyc_submission_outbox FOR ALL USING (true) WITH CHECK (true)`)
	t.Cleanup(func() { conn(`DROP POLICY IF EXISTS kso_test_open_all ON kyc_submission_outbox`) })
}

// Statements for the exclusion matrix: complete enough to satisfy every CHECK
// constraint with the guard trigger DISABLED, so only the policies decide.
const (
	matrixUpdateSQL = `UPDATE kyc_submission_outbox SET state = 'cancelled', cancel_reason = 'superseded', terminal_at = now(), lease_expires_at = NULL
	                    WHERE id = $1 AND claim_token = $2`
	matrixInsertSQL = `INSERT INTO kyc_submission_outbox (tenant_id, verification_id, player_account_id, operation, provider_id, idempotency_key)
	                    VALUES ($1, $2, $3, 'create', 'mock', $4)`
)

// exclusion-matrix expectations
const (
	wantAll  = "all"  // read, update and insert succeed
	wantRead = "read" // read yes, writes refused
	wantNone = "none" // read 0, writes refused
)

type matrixCase struct {
	name   string
	tenant bool
	gucs   map[string]string
	want   string
}

func exclusionCases() []matrixCase {
	return []matrixCase{
		{"tenant only (positive baseline)", true, nil, wantAll},
		{"staff principal (read yes, write no; M30)", true, map[string]string{"app.principal_id": uuid.NewString()}, wantRead},
		{"player scope", true, map[string]string{"app.player_account_id": uuid.NewString()}, wantNone},
		{"platform admin + tenant", true, map[string]string{"app.platform_admin_principal_id": uuid.NewString()}, wantNone},
		{"acting tenant + tenant", true, map[string]string{"app.acting_tenant_id": uuid.NewString()}, wantNone},
		{"acting platform principal + tenant", true, map[string]string{"app.acting_platform_principal_id": uuid.NewString()}, wantNone},
		{"alert_dispatcher + tenant (mixed)", true, map[string]string{"app.platform_service_id": "alert_dispatcher"}, wantNone},
		{"sportsbook_catalogue_sync + tenant (mixed)", true, map[string]string{"app.platform_service_id": "sportsbook_catalogue_sync"}, wantNone},
		{"kyc worker + tenant (mixed; M10/M11)", true, map[string]string{"app.platform_service_id": "kyc_submission_worker"}, wantNone},
		{"alert_dispatcher only", false, map[string]string{"app.platform_service_id": "alert_dispatcher"}, wantNone},
		{"sportsbook_catalogue_sync only", false, map[string]string{"app.platform_service_id": "sportsbook_catalogue_sync"}, wantNone},
		{"no GUC at all (WithoutTenant)", false, nil, wantNone},
		{"platform admin only", false, map[string]string{"app.platform_admin_principal_id": uuid.NewString()}, wantNone},
	}
}

// runExclusionMatrix runs every case against one claimed row (update probe) and
// one orphan verification (insert probe), all rolled back. rlsOnly (guard
// disabled) requires the refusals to be exactly the policy (42501 / 0 rows).
func runExclusionMatrix(t *testing.T, r *rig, claimed claimedRow, second fixture, insTarget uuid.UUID, rlsOnly bool) {
	t.Helper()
	for _, c := range exclusionCases() {
		t.Run(c.name, func(t *testing.T) {
			tenant := uuid.Nil
			if c.tenant {
				tenant = r.f.tenantID
			}
			probeTx(t, r.pool, tenant, c.gucs, func(ctx context.Context, tx pgx.Tx) {
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM kyc_submission_outbox WHERE id = $1`, claimed.ID).Scan(&n); err != nil {
					t.Fatalf("read: %v", err)
				}
				wantRead := c.want == wantAll || c.want == wantRead
				if wantRead != (n == 1) {
					t.Errorf("read: saw %d row(s), want visible=%v", n, wantRead)
				}
				affected, uerr := spExec(ctx, tx, matrixUpdateSQL, claimed.ID, claimed.ClaimToken)
				switch {
				case c.want == wantAll:
					if uerr != nil || affected != 1 {
						t.Errorf("update must succeed for the baseline: %d %v", affected, uerr)
					}
				case uerr == nil && affected != 0:
					t.Errorf("update must not apply, affected %d", affected)
				case uerr != nil && pgCode(uerr) != "42501" && (rlsOnly || pgCode(uerr) != "P0001"):
					t.Errorf("update refusal must be RLS (42501)%s, got %v", map[bool]string{false: " or the guard (P0001)", true: " alone"}[rlsOnly], uerr)
				}
				_, ierr := spExec(ctx, tx, matrixInsertSQL, second.tenantID, insTarget, second.playerID, "kv:"+insTarget.String())
				switch {
				case c.want == wantAll:
					if ierr != nil {
						t.Errorf("insert must succeed for the baseline: %v", ierr)
					}
				case ierr == nil:
					t.Errorf("insert must be refused")
				case pgCode(ierr) != "42501" && (rlsOnly || pgCode(ierr) != "P0001"):
					t.Errorf("insert refusal must be RLS (42501)%s, got %v", map[bool]string{false: " or the guard (P0001)", true: " alone"}[rlsOnly], ierr)
				}
			})
		})
	}
	// The worker policies: read everything, and ONLY claim (no insert).
	t.Run("kyc worker only", func(t *testing.T) {
		probeWorker(t, r.pool, func(ctx context.Context, tx pgx.Tx) {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM kyc_submission_outbox WHERE id = $1`, claimed.ID).Scan(&n); err != nil || n != 1 {
				t.Errorf("the worker must read the outbox: %d %v", n, err)
			}
			_, ierr := spExec(ctx, tx, matrixInsertSQL, second.tenantID, insTarget, second.playerID, "kv:"+insTarget.String())
			if pgCode(ierr) != "42501" && (rlsOnly || pgCode(ierr) != "P0001") {
				t.Errorf("the worker must not insert: %v", ierr)
			}
			// Not a claim: a non-claimed target state is refused by the worker policy.
			_, uerr := spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'sent', terminal_at = now(), lease_expires_at = NULL WHERE id = $1`, claimed.ID)
			if pgCode(uerr) != "42501" && (rlsOnly || pgCode(uerr) != "P0001") {
				t.Errorf("the worker must not move a row to sent (M9): %v", uerr)
			}
		})
	})
}

// 21. Tenant B cannot read, update or insert tenant A's rows.
func TestOutboxRLS_21_TenantIsolation(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	b := seedFixture(t, r.pool)
	check := func(rlsOnly bool) {
		probeTx(t, r.pool, b.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM kyc_submission_outbox WHERE id = $1`, row.ID).Scan(&n); err != nil || n != 0 {
				t.Errorf("tenant B reads %d of tenant A's rows (err %v)", n, err)
			}
			affected, err := spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'cancelled', cancel_reason = 'superseded', terminal_at = now() WHERE id = $1`, row.ID)
			if err != nil || affected != 0 {
				t.Errorf("tenant B updated tenant A's row: %d %v", affected, err)
			}
			_, err = spExec(ctx, tx, matrixInsertSQL, r.f.tenantID, v.ID, r.f.playerID, "kv:"+v.ID.String())
			if pgCode(err) != "42501" && (rlsOnly || pgCode(err) != "P0001") {
				t.Errorf("tenant B inserting for tenant A must be refused (RLS 42501%s): %v", map[bool]string{false: " or the guard", true: " alone"}[rlsOnly], err)
			}
		})
	}
	check(false)
	t.Run("rls only", func(t *testing.T) {
		disableOutboxGuard(t)
		check(true)
	})
}

// 22. The exclusion matrix: positive baseline first, then every session shape
// that must NOT read or write the outbox through the tenant policies - once
// with the guard enabled and once with it disabled so the POLICIES alone are
// proven (M10, M11, M30).
func TestOutboxRLS_22_ExclusionMatrix(t *testing.T) {
	r := newRig(t)
	v := r.create()
	claimed := claimOne(t, r.w) // a claimed row for the update probes
	if claimed.VerificationID != v.ID {
		t.Fatal("setup: claimed the wrong row")
	}
	second := seedSecondAccount(t, r.pool, r.f)
	insTarget := rawOrphan(t, r.pool, second)
	t.Run("guard enabled", func(t *testing.T) { runExclusionMatrix(t, r, claimed, second, insTarget, false) })
	t.Run("rls only", func(t *testing.T) {
		disableOutboxGuard(t)
		runExclusionMatrix(t, r, claimed, second, insTarget, true)
	})
}

// 23. A tenant cannot claim, cannot touch a pending row except the validated
// verification_not_submitted cancel, cannot change immutable columns; nobody can
// DELETE or TRUNCATE.
func TestOutboxRLS_23_TenantTransitionLimits_NoDeleteNoTruncate(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)

	// A tenant cannot claim: refused by the guard, and by the RLS WITH CHECK alone.
	tenantClaimSQL := `UPDATE kyc_submission_outbox SET state = 'claimed', claim_token = gen_random_uuid(), lease_expires_at = now() + interval '1 minute' WHERE id = $1`
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		_, err := tx.Exec(ctx, tenantClaimSQL, row.ID)
		if pgCode(err) != "42501" && pgCode(err) != "P0001" {
			t.Errorf("a tenant must not claim: %v", err)
		}
	})
	t.Run("claim, rls only", func(t *testing.T) {
		disableOutboxGuard(t)
		probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
			_, err := tx.Exec(ctx, tenantClaimSQL, row.ID)
			if pgCode(err) != "42501" {
				t.Errorf("the RLS WITH CHECK alone must refuse a tenant claim (M8): %v", err)
			}
		})
	})
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		_, err := spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'cancelled', cancel_reason = 'superseded' WHERE id = $1`, row.ID)
		if pgCode(err) != "P0001" {
			t.Errorf("a tenant may cancel a PENDING row only as the validated verification_not_submitted: %v", err)
		}
		_, err = spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'cancelled', cancel_reason = 'verification_not_submitted' WHERE id = $1`, row.ID)
		if pgCode(err) != "P0001" {
			t.Errorf("verification_not_submitted needs a create row that ended terminal, and a SUBMIT row: %v", err)
		}
	})

	claimed := claimOne(t, r.w)
	for _, set := range []string{
		"tenant_id = gen_random_uuid()", "verification_id = gen_random_uuid()", "player_account_id = gen_random_uuid()",
		"operation = 'submit'", "provider_id = 'other'", "document_ids = ARRAY[gen_random_uuid()]",
		"idempotency_key = 'kv:' || gen_random_uuid()::text", "created_at = now() - interval '1 day'", "id = gen_random_uuid()",
	} {
		probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
			_, err := tx.Exec(ctx, `UPDATE kyc_submission_outbox SET `+set+` WHERE id = $1 AND claim_token = $2`, claimed.ID, claimed.ClaimToken)
			if err == nil || (pgCode(err) != "P0001" && pgCode(err) != "42501" && pgCode(err) != "23503" && pgCode(err) != "23514") {
				t.Errorf("changing an immutable column (%s) must be refused, got %v", set, err)
			}
		})
	}
	// Claim columns never change from a tenant session: an attempt is overwritten with OLD.
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		var token uuid.UUID
		var claims int
		err := tx.QueryRow(ctx, `UPDATE kyc_submission_outbox SET state = 'sent', claim_token = gen_random_uuid(), claims = 99, claimed_by_service = 'kyc_submission_worker'
		                         WHERE id = $1 AND claim_token = $2 RETURNING claim_token, claims`, claimed.ID, claimed.ClaimToken).Scan(&token, &claims)
		if err != nil || token != claimed.ClaimToken || claims != 1 {
			t.Errorf("claim columns must be pinned by the trigger: token=%s claims=%d err=%v", token, claims, err)
		}
	})

	// DELETE: there is no DELETE policy (0 rows for the runtime role, the worker
	// and even the owner under FORCE RLS); with RLS lifted for the owner the guard
	// trigger refuses; the runtime role holds neither DELETE nor TRUNCATE.
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		n, err := spExec(ctx, tx, `DELETE FROM kyc_submission_outbox WHERE id = $1`, row.ID)
		if err == nil && n != 0 {
			t.Errorf("DELETE must not apply for a tenant session, affected %d", n)
		}
	})
	probeWorker(t, r.pool, func(ctx context.Context, tx pgx.Tx) {
		n, err := spExec(ctx, tx, `DELETE FROM kyc_submission_outbox WHERE id = $1`, row.ID)
		if err == nil && n != 0 {
			t.Errorf("DELETE must not apply for the worker, affected %d", n)
		}
	})
	ctx := context.Background()
	if err := r.owner.WithTenant(ctx, r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		n, err := spExec(ctx, tx, `DELETE FROM kyc_submission_outbox WHERE id = $1`, row.ID)
		if err == nil && n != 0 {
			t.Errorf("DELETE must not apply even for the owner under FORCE RLS, affected %d", n)
		}
		// RLS lifted for the owner: now only the guard trigger stands.
		if _, err := tx.Exec(ctx, `ALTER TABLE kyc_submission_outbox NO FORCE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		_, err = spExec(ctx, tx, `DELETE FROM kyc_submission_outbox WHERE id = $1`, row.ID)
		if pgCode(err) != "P0001" {
			t.Errorf("the guard trigger must refuse an owner DELETE, got %v", err)
		}
		_, err = spExec(ctx, tx, `TRUNCATE kyc_submission_outbox`)
		if err == nil {
			t.Error("TRUNCATE must be refused")
		}
		return errProbeRollback // restores FORCE RLS
	}); err != nil && !errors.Is(err, errProbeRollback) {
		t.Fatal(err)
	}
	var privs [2]bool
	if err := r.pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', 'kyc_submission_outbox', 'DELETE'), has_table_privilege('igaming_runtime', 'kyc_submission_outbox', 'TRUNCATE')`).Scan(&privs[0], &privs[1])
	}); err != nil {
		t.Fatal(err)
	}
	if privs[0] || privs[1] {
		t.Errorf("the runtime role must hold neither DELETE nor TRUNCATE on the outbox: %v", privs)
	}
}

// 24. Insert validation refusals (the trigger recomputes, never trusts).
func TestOutboxRLS_24_InsertValidation(t *testing.T) {
	r := newRig(t)
	// A document of ANOTHER verification of the same tenant (a second player),
	// created first so no later worker pass touches this test's own rows.
	otherF := seedSecondAccount(t, r.pool, r.f)
	otherV := createViaWorker(t, r.pool, NewMockOutboundResolver(), NewMockKYCProvider(), otherF)
	foreignDoc := seedDocument(t, r.pool, otherF, otherV.ID, DocumentPassport, "f.png")
	vid := r.createSent().ID // a non-orphan, sent verification (it has a reference)
	d1 := seedDocument(t, r.pool, r.f, vid, DocumentPassport, "a.png")
	d2 := seedDocument(t, r.pool, r.f, vid, DocumentSelfie, "b.png")
	rej := seedDocument(t, r.pool, r.f, vid, DocumentOther, "c.png")
	staffID := seedComplianceStaff(t, r.pool, r.f)
	if err := r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ReviewDocument(ctx, tx, ReviewDocumentParams{DocumentID: rej, StaffID: staffID, NewStatus: DocumentRejected, Reason: "bad"})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	orphan := rawOrphan(t, r.pool, seedSecondAccount(t, r.pool, r.f))

	sorted := func(ids ...uuid.UUID) []uuid.UUID {
		docs := make([]SubmittedDocument, len(ids))
		for i, id := range ids {
			docs[i] = SubmittedDocument{DocumentID: id}
		}
		return sortedDocumentIDs(docs)
	}
	asDocs := func(ids ...uuid.UUID) []SubmittedDocument {
		docs := make([]SubmittedDocument, len(ids))
		for i, id := range ids {
			docs[i] = SubmittedDocument{DocumentID: id}
		}
		return docs
	}
	good := sorted(d1, d2)
	goodKey := submissionIdempotencyKey(vid, asDocs(d1, d2))
	// A set whose key is NOT already live (uploads enqueued {d1}, {d1,d2}, {d1,d2,rej}).
	fresh := sorted(d2)
	freshKey := submissionIdempotencyKey(vid, asDocs(d2))
	const submitSQL = `INSERT INTO kyc_submission_outbox (tenant_id, verification_id, operation, provider_id, document_ids, idempotency_key)
	                   VALUES ($1, $2, 'submit', $3, $4::uuid[], $5)`
	type bad struct {
		name string
		sql  string
		args []any
	}
	cases := []bad{
		{"wrong provider", submitSQL, []any{r.f.tenantID, vid, "other", good, goodKey}},
		{"provider id outside the charset", submitSQL, []any{r.f.tenantID, vid, "bad provider!", good, goodKey}},
		{"unsorted ids", submitSQL, []any{r.f.tenantID, vid, "mock", []uuid.UUID{good[1], good[0]}, submissionIdempotencyKey(vid, asDocs(d1, d2))}},
		{"duplicate ids", submitSQL, []any{r.f.tenantID, vid, "mock", []uuid.UUID{good[0], good[0]}, submissionIdempotencyKey(vid, asDocs(d1, d1))}},
		{"a rejected document", submitSQL, []any{r.f.tenantID, vid, "mock", sorted(d1, rej), submissionIdempotencyKey(vid, asDocs(d1, rej))}},
		{"a document of another verification", submitSQL, []any{r.f.tenantID, vid, "mock", sorted(d1, foreignDoc), submissionIdempotencyKey(vid, asDocs(d1, foreignDoc))}},
		{"a document that does not exist", submitSQL, []any{r.f.tenantID, vid, "mock", sorted(d1, uuid.New()), "ks:" + vid.String() + ":" + fmt.Sprintf("%064x", 1)}},
		{"a key that does not match the set (M14)", submitSQL, []any{r.f.tenantID, vid, "mock", good, "ks:" + vid.String() + ":" + fmt.Sprintf("%064x", 7)}},
		{"a key for a different verification", submitSQL, []any{r.f.tenantID, vid, "mock", good, "ks:" + uuid.NewString() + ":" + fmt.Sprintf("%064x", 7)}},
		{"no documents", submitSQL, []any{r.f.tenantID, vid, "mock", []uuid.UUID{}, goodKey}},
		{"create on a non-orphan verification", insertCreateSQL, []any{r.f.tenantID, vid, "kv:" + vid.String()}},
		{"create with the wrong key", insertCreateSQL, []any{r.f.tenantID, orphan, "kv:" + uuid.NewString()}},
		{"a verification of another tenant", insertCreateSQL, []any{r.f.tenantID, uuid.New(), "kv:" + uuid.NewString()}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
				_, err := tx.Exec(ctx, c.sql, c.args...)
				code := pgCode(err)
				if err == nil || (code != "P0001" && code != "23514" && code != "23503") {
					t.Errorf("expected a refusal by the guard or a CHECK, got %v", err)
				}
			})
		})
	}
	// Tenant mismatch is refused by RLS.
	b := seedFixture(t, r.pool)
	probeTx(t, r.pool, b.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		_, err := tx.Exec(ctx, insertCreateSQL, r.f.tenantID, orphan, "kv:"+orphan.String())
		if pgCode(err) != "P0001" && pgCode(err) != "42501" {
			t.Errorf("a tenant_id different from the session must be refused, got %v", err)
		}
	})
	// Positive control + forced columns: a valid submit is accepted, and a
	// forged player_account_id / state / claim columns are overwritten.
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		var state string
		var player uuid.UUID
		var claims int
		var token *uuid.UUID
		err := tx.QueryRow(ctx,
			`INSERT INTO kyc_submission_outbox (tenant_id, verification_id, operation, provider_id, document_ids, idempotency_key, player_account_id, state, claims, claim_token)
			 VALUES ($1, $2, 'submit', 'mock', $3::uuid[], $4, $5, 'sent', 9, gen_random_uuid()) RETURNING state, player_account_id, claims, claim_token`,
			r.f.tenantID, vid, fresh, freshKey, otherF.playerID).Scan(&state, &player, &claims, &token)
		if err != nil {
			t.Fatalf("a valid submit must be accepted: %v", err)
		}
		if state != "pending" || player != r.f.playerID || claims != 0 || token != nil {
			t.Errorf("forced columns not forced: state=%s player=%s (want %s) claims=%d token=%v", state, player, r.f.playerID, claims, token)
		}
	})
}

// 25. Go/SQL key parity: for random document sets the trigger's recomputation
// equals Go's submissionIdempotencyKey (the DB refuses on any mismatch).
func TestOutboxRLS_25_GoSQLKeyParity(t *testing.T) {
	pool := rtPool(t, 3)
	for i := 0; i < 200; i++ {
		n := 1 + i%6
		docs := make([]SubmittedDocument, n)
		ids := make([]uuid.UUID, n)
		for j := range docs {
			docs[j] = SubmittedDocument{DocumentID: uuid.New()}
		}
		sortedIDs := sortedDocumentIDs(docs)
		copy(ids, sortedIDs)
		vid := uuid.New()
		goKey := submissionIdempotencyKey(vid, docs)
		var sqlKey string
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT 'ks:' || $1::uuid::text || ':' || encode(sha256(
				(SELECT string_agg(convert_to(d::text, 'UTF8') || '\x00'::bytea, ''::bytea ORDER BY d::text)
				   FROM unnest($2::uuid[]) AS d)), 'hex')`, vid, ids).Scan(&sqlKey)
		}); err != nil {
			t.Fatal(err)
		}
		if sqlKey != goKey {
			t.Fatalf("Go/SQL key mismatch for %v: go=%s sql=%s", ids, goKey, sqlKey)
		}
	}
}

// 26. Worker trigger pinning (F6): a claim cannot change failed_attempts,
// next_attempt_at, last_error_class, cancel_reason or terminal_at (overwritten
// with OLD), cannot change document_ids, cannot re-claim an unexpired lease, and
// the lease is bounded (now, now + 10 minutes].
func TestOutboxRLS_26_WorkerTriggerPinning_F6(t *testing.T) {
	r := newRig(t)
	v := r.create()
	row := onlyRow(t, r.pool, r.f.tenantID, v.ID, OpCreate)
	outboxFixtureUpdate(t, row.ID, `failed_attempts = 3, last_error_class = 'ambiguous'`)

	// (a) every pinned column is overwritten with OLD on a claim (M22).
	probeWorker(t, r.pool, func(ctx context.Context, tx pgx.Tx) {
		var attempts, claims int
		var class *string
		var cancelReason *string
		var terminal *string
		var nextBefore, nextAfter string
		if err := tx.QueryRow(ctx, `SELECT next_attempt_at::text FROM kyc_submission_outbox WHERE id = $1`, row.ID).Scan(&nextBefore); err != nil {
			t.Fatal(err)
		}
		err := tx.QueryRow(ctx,
			`UPDATE kyc_submission_outbox SET state = 'claimed', lease_expires_at = now() + interval '30 seconds',
			        failed_attempts = 0, next_attempt_at = now() + interval '7 days', last_error_class = NULL,
			        cancel_reason = NULL, terminal_at = NULL
			  WHERE id = $1 RETURNING failed_attempts, claims, last_error_class, cancel_reason, terminal_at::text, next_attempt_at::text`, row.ID).
			Scan(&attempts, &claims, &class, &cancelReason, &terminal, &nextAfter)
		if err != nil {
			t.Fatalf("claim: %v", err)
		}
		if attempts != 3 || claims != 1 || class == nil || *class != "ambiguous" || cancelReason != nil || terminal != nil || nextAfter != nextBefore {
			t.Errorf("pinned columns changed on a claim: attempts=%d claims=%d class=%v cancel=%v terminal=%v next=%s (was %s)", attempts, claims, class, cancelReason, terminal, nextAfter, nextBefore)
		}
		// (b) document_ids is immutable even for the worker.
		_, err = tx.Exec(ctx, `UPDATE kyc_submission_outbox SET document_ids = ARRAY[gen_random_uuid()] WHERE id = $1`, row.ID)
		if pgCode(err) != "P0001" {
			t.Errorf("the worker must not change document_ids: %v", err)
		}
	})
	// (c) lease bounds on a claim (M29).
	for name, lease := range map[string]string{"over 10 minutes": "now() + interval '11 minutes'", "not in the future": "now()", "in the past": "now() - interval '1 second'"} {
		probeWorker(t, r.pool, func(ctx context.Context, tx pgx.Tx) {
			_, err := tx.Exec(ctx, `UPDATE kyc_submission_outbox SET state = 'claimed', lease_expires_at = `+lease+` WHERE id = $1`, row.ID)
			if pgCode(err) != "P0001" {
				t.Errorf("a lease %s must be refused by the guard: %v", name, err)
			}
		})
	}
	// (d) an UNEXPIRED lease cannot be re-claimed (M23); an expired one can.
	claimed := claimOne(t, r.w)
	probeWorker(t, r.pool, func(ctx context.Context, tx pgx.Tx) {
		_, err := tx.Exec(ctx, `UPDATE kyc_submission_outbox SET state = 'claimed', lease_expires_at = now() + interval '30 seconds' WHERE id = $1`, claimed.ID)
		if pgCode(err) != "P0001" {
			t.Errorf("re-claiming an unexpired lease must be refused: %v", err)
		}
	})
	expireLease(t, row.ID)
	probeWorker(t, r.pool, func(ctx context.Context, tx pgx.Tx) {
		var attempts int
		var class string
		err := tx.QueryRow(ctx, `UPDATE kyc_submission_outbox SET state = 'claimed', lease_expires_at = now() + interval '30 seconds'
		                         WHERE id = $1 RETURNING failed_attempts, last_error_class`, claimed.ID).Scan(&attempts, &class)
		if err != nil || attempts != claimed.FailedAttempts+1 || class != "lease_expired" {
			t.Errorf("an expired lease re-claims with failed_attempts+1 / lease_expired: attempts=%d class=%q err=%v", attempts, class, err)
		}
	})
	// (e) a not-yet-due pending row cannot be claimed.
	other := seedSecondAccount(t, r.pool, r.f)
	v2, _ := requestCreate(t, r.pool, other, "mock")
	r2 := onlyRow(t, r.pool, other.tenantID, v2.ID, OpCreate)
	outboxFixtureUpdate(t, r2.ID, `next_attempt_at = now() + interval '1 hour'`)
	probeWorker(t, r.pool, func(ctx context.Context, tx pgx.Tx) {
		_, err := tx.Exec(ctx, `UPDATE kyc_submission_outbox SET state = 'claimed', lease_expires_at = now() + interval '30 seconds' WHERE id = $1`, r2.ID)
		if pgCode(err) != "P0001" {
			t.Errorf("a not-yet-due row must not be claimable (M4): %v", err)
		}
	})
}

// 26b. Tenant-side retry/deferral validation: deferral is DB-validated against
// the tenant's real status (M34); classes and next_attempt_at are bounded.
func TestOutboxRLS_26b_RetryAndDeferralValidation_F6(t *testing.T) {
	r := newRig(t)
	v := r.create()
	claimed := claimOne(t, r.w)
	retry := func(class string, next string) (string, error) {
		var err error
		var attempts int
		probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
			err = tx.QueryRow(ctx,
				`UPDATE kyc_submission_outbox SET state = 'pending', last_error_class = $3, next_attempt_at = `+next+`
				  WHERE id = $1 AND claim_token = $2 RETURNING failed_attempts`, claimed.ID, claimed.ClaimToken, class).Scan(&attempts)
		})
		return fmt.Sprint(attempts), err
	}
	if a, err := retry("ambiguous", "now() + interval '30 seconds'"); err != nil || a != "1" {
		t.Fatalf("a valid retry must be accepted with failed_attempts+1: err=%v attempts=%s", err, a)
	}
	if _, err := retry("deferred_tenant_inactive", "now() + interval '30 seconds'"); pgCode(err) != "P0001" {
		t.Errorf("deferral must be refused while the tenant is ACTIVE (M34): %v", err)
	}
	for name, c := range map[string][2]string{
		"unknown class":    {"lease_expired", "now() + interval '30 seconds'"},
		"past deadline":    {"ambiguous", "now() - interval '1 second'"},
		"now deadline":     {"ambiguous", "now()"},
		"beyond one day":   {"ambiguous", "now() + interval '2 days'"},
		"binding mismatch": {"credential_binding_mismatch", "now() + interval '30 seconds'"},
	} {
		if _, err := retry(c[0], c[1]); pgCode(err) != "P0001" {
			t.Errorf("%s: a retry must be refused by the guard, got %v", name, err)
		}
	}
	setTenantStatus(t, r.pool, r.f.tenantID, "suspended")
	t.Cleanup(func() { setTenantStatus(t, r.pool, r.f.tenantID, "active") })
	if a, err := retry("deferred_tenant_inactive", "now() + interval '30 seconds'"); err != nil || a != "0" {
		t.Errorf("deferral for a suspended tenant must be accepted WITHOUT consuming an attempt: err=%v attempts=%s", err, a)
	}
	_ = v
}

// 27. INV-KYC-OB-4 tamper test: P and C run with a tenant id different from the
// claim's RETURNING value: 0 rows, no vendor call, no write.
func TestOutboxRLS_27_TamperedTenantID_NoCallNoWrite_INVKYCOB4(t *testing.T) {
	r := newRig(t)
	v := r.create()
	b := seedFixture(t, r.pool)
	claimed := claimOne(t, r.w)
	before := readOutbox(t, r.pool, r.f.tenantID, v.ID)
	snap := r.snapshot(v.ID)

	tampered := claimed
	tampered.TenantID = b.tenantID // the tenant id taken from anywhere but the claim's RETURNING
	po, err := r.w.prepare(context.Background(), tampered)
	if err != nil || po.kind != prepLost {
		t.Fatalf("P with a different tenant id must lose the claim (0 rows), got %+v err=%v", po, err)
	}
	out := vendorOutcome{kind: outcomeDefinitive, result: ProviderResult{ProviderReference: "tamper-ref", Outcome: ProviderPending}, hasResult: true}
	// prep carries the REAL tenant (an "elsewhere" source a buggy phase C might
	// prefer over the claim's RETURNING value, M25): C must still use the claim's.
	if got := r.w.runPhaseC(context.Background(), tampered, &prepared{verification: Verification{ID: v.ID, TenantID: r.f.tenantID}}, out, false); got != resultClaimLost {
		t.Fatalf("C with a different tenant id must lose the claim, got %q", got)
	}
	if c, s := r.calls(); c != 0 || s != 0 {
		t.Fatalf("no vendor call may happen, got %d/%d", c, s)
	}
	after := readOutbox(t, r.pool, r.f.tenantID, v.ID)
	if len(after) != len(before) || after[0].State != OutboxClaimed || r.snapshot(v.ID) != snap {
		t.Fatalf("no write may happen: rows %+v verification changed=%v", after, r.snapshot(v.ID) != snap)
	}
	// Also an id that never was claimed.
	ghost := claimed
	ghost.ClaimToken = uuid.New()
	if po, _ := r.w.prepare(context.Background(), ghost); po.kind != prepLost {
		t.Fatalf("a wrong claim token must lose the claim, got %+v", po)
	}
}

// 8 / 16b / 30 (trigger layer). With the RLS policies opened wide the guard
// TRIGGER alone must still refuse: a tenant claim, a non-claim worker
// transition, every non-tenant/mixed write shape, a tenant_id different from the
// session, immutable-column changes, a submit claim while its create is live
// (M8, M16b, M30's backstop).
func TestOutboxGuard_TriggerAlone_Refuses_8_16b(t *testing.T) {
	r := newRig(t)
	v := r.create()
	seedDocument(t, r.pool, r.f, v.ID, DocumentPassport, "p.png") // a pending submit behind the live create
	rows := readOutbox(t, r.pool, r.f.tenantID, v.ID)
	var createRow, submitRow obRow
	for _, row := range rows {
		if row.Operation == OpCreate {
			createRow = row
		} else {
			submitRow = row
		}
	}
	second := seedSecondAccount(t, r.pool, r.f)
	insTarget := rawOrphan(t, r.pool, second)
	openOutboxPolicies(t)

	expectGuard := func(name string, err error) {
		t.Helper()
		if pgCode(err) != "P0001" {
			t.Errorf("%s: the guard trigger alone must refuse (P0001), got %v", name, err)
		}
	}
	probeTx(t, r.pool, r.f.tenantID, nil, func(ctx context.Context, tx pgx.Tx) {
		_, err := spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'claimed', claim_token = gen_random_uuid(), lease_expires_at = now() + interval '1 minute' WHERE id = $1`, createRow.ID)
		expectGuard("a tenant claim (M8)", err)
		_, err = spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'cancelled', cancel_reason = 'superseded', terminal_at = now() WHERE id = $1`, createRow.ID)
		expectGuard("a tenant cancel of a pending row", err)
		_, err = spExec(ctx, tx, matrixInsertSQL, uuid.New(), insTarget, second.playerID, "kv:"+insTarget.String())
		expectGuard("a tenant_id different from the session", err)
	})
	for name, gucs := range map[string]map[string]string{
		"principal":        {"app.principal_id": uuid.NewString()},
		"player":           {"app.player_account_id": uuid.NewString()},
		"platform admin":   {"app.platform_admin_principal_id": uuid.NewString()},
		"acting tenant":    {"app.acting_tenant_id": uuid.NewString()},
		"acting principal": {"app.acting_platform_principal_id": uuid.NewString()},
		"dispatcher":       {"app.platform_service_id": "alert_dispatcher"},
		"worker (mixed)":   {"app.platform_service_id": "kyc_submission_worker"},
	} {
		probeTx(t, r.pool, r.f.tenantID, gucs, func(ctx context.Context, tx pgx.Tx) {
			_, err := spExec(ctx, tx, matrixInsertSQL, second.tenantID, insTarget, second.playerID, "kv:"+insTarget.String())
			expectGuard("insert under "+name+" (M30 backstop)", err)
		})
	}
	probeWorker(t, r.pool, func(ctx context.Context, tx pgx.Tx) {
		_, err := spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'sent', terminal_at = now(), lease_expires_at = NULL WHERE id = $1`, createRow.ID)
		expectGuard("a worker non-claim transition (M9 backstop)", err)
		_, err = spExec(ctx, tx, matrixInsertSQL, second.tenantID, insTarget, second.playerID, "kv:"+insTarget.String())
		expectGuard("a worker insert", err)
		// A submit row is not claimable while its create is live (M16b).
		_, err = spExec(ctx, tx, `UPDATE kyc_submission_outbox SET state = 'claimed', lease_expires_at = now() + interval '30 seconds' WHERE id = $1`, submitRow.ID)
		expectGuard("claiming a submit while the create is live (M16b)", err)
	})
}

// 14b. SKIP LOCKED / FOR UPDATE (M3): a row locked by another transaction is
// SKIPPED, not waited for: the claim returns the OTHER due row immediately.
// Without SKIP LOCKED the claim would block on the locked row (and without FOR
// UPDATE its UPDATE would), so a bounded context turns that into a failure.
func TestOutbox_14b_ClaimSkipsLockedRows_M3(t *testing.T) {
	r := newRig(t)
	other := seedSecondAccount(t, r.pool, r.f)
	v1 := r.create()
	v2, _ := requestCreate(t, r.pool, other, "mock")
	first := onlyRow(t, r.pool, r.f.tenantID, v1.ID, OpCreate)
	second := onlyRow(t, r.pool, r.f.tenantID, v2.ID, OpCreate)

	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- r.pool.WithTenant(context.Background(), r.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var id uuid.UUID
			if err := tx.QueryRow(ctx, `SELECT id FROM kyc_submission_outbox WHERE id = $1 FOR UPDATE`, first.ID).Scan(&id); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	got, err := r.w.claimNext(ctx, nil)
	close(release)
	if werr := <-done; werr != nil {
		t.Fatal(werr)
	}
	if err != nil || got == nil || got.ID != second.ID {
		t.Fatalf("the claim must skip the locked first row and return the second immediately, got %+v err=%v", got, err)
	}
}
