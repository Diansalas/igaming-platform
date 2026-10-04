//go:build integration

package kyc

// PRH-2 E1 test helpers (ADR 0106 section 10). Conventions:
//   - RLS / guard / worker tests run as the RUNTIME role (TEST_RUNTIME_DATABASE_URL),
//     asserting rolsuper = false AND rolbypassrls = false before anything else.
//   - Fixture manipulation that the production guard rightly forbids (moving a
//     lease or a due time by fixture, T-1; purging leftover rows between tests)
//     goes through the OWNER connection (TEST_DATABASE_URL) in one transaction
//     that disables the guard trigger and lifts FORCE RLS and restores both
//     before commit. No production code path has such a seam.
//   - No test asserts on wall-clock sleeps: time is moved by fixture (T-1).

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// rtPool connects as the runtime role and asserts it is neither superuser nor
// BYPASSRLS (a bypassing role would make every RLS assertion vacuous).
func rtPool(t *testing.T, maxConns int32) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role outbox test")
	}
	pool, err := db.Connect(context.Background(), url, maxConns, 5*time.Second)
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
		t.Fatalf("runtime-role tests must not run as a superuser or BYPASSRLS role (super=%v bypassrls=%v)", super, bypass)
	}
	return pool
}

// ownerTx runs fn in one transaction on the OWNER connection with the outbox
// guard trigger disabled and FORCE RLS lifted; both are restored before commit.
func ownerTx(t *testing.T, fn func(ctx context.Context, tx pgx.Tx)) {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("owner connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("owner begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, stmt := range []string{
		// One cross-package serialisation point for every outbox DDL fixture
		// (taken first, so two fixtures never deadlock on the table lock), and a
		// bounded wait for concurrent writers of other packages.
		`SELECT pg_advisory_xact_lock(1140114)`,
		`SET LOCAL lock_timeout = '30s'`,
		`ALTER TABLE kyc_submission_outbox DISABLE TRIGGER USER`,
		`ALTER TABLE kyc_submission_outbox NO FORCE ROW LEVEL SECURITY`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("owner fixture %q: %v", stmt, err)
		}
	}
	fn(ctx, tx)
	for _, stmt := range []string{
		`ALTER TABLE kyc_submission_outbox FORCE ROW LEVEL SECURITY`,
		`ALTER TABLE kyc_submission_outbox ENABLE TRIGGER USER`,
	} {
		if _, err := tx.Exec(ctx, stmt); err != nil {
			t.Fatalf("owner fixture restore %q: %v", stmt, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("owner commit: %v", err)
	}
}

// ---- parallel-package safety (code review F1) ----
//
// CI runs every package's integration tests in parallel against ONE database.
// No test deletes outbox rows or lifts a guard for rows it does not own, and no
// worker pass claims a row of a tenant the running test did not create:
// OutboxWorker.ClaimScope (nil in production) is set to the tenants the CURRENT
// top-level test seeded. The only DDL left is the short, advisory-lock
// serialised owner fixture in ownerTx (transactional: no other session ever
// sees the trigger disabled).

var testScope struct {
	mu  sync.Mutex
	top string
	ids []uuid.UUID
}

// registerScopeTenant adds a tenant the running test owns to the claim scope.
// A new top-level test starts with an empty scope.
func registerScopeTenant(t *testing.T, id uuid.UUID) {
	t.Helper()
	top := strings.SplitN(t.Name(), "/", 2)[0]
	testScope.mu.Lock()
	defer testScope.mu.Unlock()
	if testScope.top != top {
		testScope.top, testScope.ids = top, nil
	}
	testScope.ids = append(testScope.ids, id)
}

// resetScope starts a fresh claim scope for the running (sub)test: rows an
// earlier subtest left behind are out of scope (no deletion needed).
func resetScope(t *testing.T) {
	t.Helper()
	testScope.mu.Lock()
	defer testScope.mu.Unlock()
	testScope.top, testScope.ids = strings.SplitN(t.Name(), "/", 2)[0], nil
}

// currentScope is the OutboxWorker.ClaimScope of every test worker.
func currentScope() []uuid.UUID {
	testScope.mu.Lock()
	defer testScope.mu.Unlock()
	return append([]uuid.UUID{}, testScope.ids...)
}

// newScopedWorker is NewOutboxWorker limited to the running test's tenants.
func newScopedWorker(pool *db.Pool, orch *Orchestrator, outbound OutboundCredentialResolver) *OutboxWorker {
	w := NewOutboxWorker(pool, orch, outbound)
	w.ClaimScope = currentScope
	return w
}

// outboxFixtureUpdate applies `UPDATE kyc_submission_outbox SET <set> WHERE id = $1`
// by fixture (T-1: move lease/due times without sleeping).
func outboxFixtureUpdate(t *testing.T, id uuid.UUID, set string, args ...any) {
	t.Helper()
	ownerTx(t, func(ctx context.Context, tx pgx.Tx) {
		all := append([]any{id}, args...)
		tag, err := tx.Exec(ctx, `UPDATE kyc_submission_outbox SET `+set+` WHERE id = $1`, all...)
		if err != nil {
			t.Fatalf("outbox fixture update: %v", err)
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("outbox fixture update touched %d rows", tag.RowsAffected())
		}
	})
}

// makeDueNow moves a pending row's next_attempt_at into the past.
func makeDueNow(t *testing.T, id uuid.UUID) {
	t.Helper()
	outboxFixtureUpdate(t, id, `next_attempt_at = now() - interval '1 second'`)
}

// expireLease moves a claimed row's lease into the past.
func expireLease(t *testing.T, id uuid.UUID) {
	t.Helper()
	outboxFixtureUpdate(t, id, `lease_expires_at = now() - interval '1 second'`)
}

// obRow is a read snapshot of one outbox row.
type obRow struct {
	ID             uuid.UUID
	Operation      OutboxOperation
	State          OutboxState
	ProviderID     string
	DocumentIDs    []uuid.UUID
	IdempotencyKey string
	Claims         int
	FailedAttempts int
	LastErrorClass string
	CancelReason   string
	ClaimToken     *uuid.UUID
	PlayerAccount  uuid.UUID
}

func readOutbox(t *testing.T, pool *db.Pool, tenantID, verificationID uuid.UUID) []obRow {
	t.Helper()
	var out []obRow
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT id, operation, state, provider_id, document_ids, idempotency_key, claims, failed_attempts,
			        COALESCE(last_error_class, ''), COALESCE(cancel_reason, ''), claim_token, player_account_id
			   FROM kyc_submission_outbox WHERE verification_id = $1 ORDER BY created_at, id`, verificationID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r obRow
			if err := rows.Scan(&r.ID, &r.Operation, &r.State, &r.ProviderID, &r.DocumentIDs, &r.IdempotencyKey, &r.Claims,
				&r.FailedAttempts, &r.LastErrorClass, &r.CancelReason, &r.ClaimToken, &r.PlayerAccount); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read outbox rows: %v", err)
	}
	return out
}

// onlyRow returns the single outbox row of op for verificationID.
func onlyRow(t *testing.T, pool *db.Pool, tenantID, verificationID uuid.UUID, op OutboxOperation) obRow {
	t.Helper()
	var found []obRow
	for _, r := range readOutbox(t, pool, tenantID, verificationID) {
		if r.Operation == op {
			found = append(found, r)
		}
	}
	if len(found) != 1 {
		t.Fatalf("expected exactly one %s outbox row for %s, got %d", op, verificationID, len(found))
	}
	return found[0]
}

// workerFor builds a worker over pool for one provider (the registered
// orchestrator holds exactly that adapter, so the synthetic fallback selects it
// and the F4 re-check passes).
func workerFor(pool *db.Pool, outbound OutboundCredentialResolver, provider KYCProvider) *OutboxWorker {
	orch := NewOrchestrator(map[string]KYCProvider{provider.ID(): provider}, nil)
	w := newScopedWorker(pool, orch, outbound)
	w.Logger = nil
	return w
}

// requestCreate runs phase A (RequestVerification) in one tenant transaction.
func requestCreate(t *testing.T, pool *db.Pool, f fixture, providerID string) (Verification, bool) {
	t.Helper()
	v, existing, err := tryRequestCreate(pool, f, providerID)
	if err != nil {
		t.Fatalf("RequestVerification: %v", err)
	}
	return v, existing
}

func tryRequestCreate(pool *db.Pool, f fixture, providerID string) (Verification, bool, error) {
	var v Verification
	var existing bool
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		v, existing, err = RequestVerification(ctx, tx, CreateVerificationParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerID, PersonID: f.personID,
		}, providerID)
		return err
	})
	return v, existing, err
}

// passUntilQuiet runs worker passes until one claims nothing (bounded).
func passUntilQuiet(t *testing.T, w *OutboxWorker) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if st := w.RunPass(context.Background()); st.Claimed == 0 {
			return
		}
	}
	t.Fatal("worker did not go quiet within 50 passes")
}

// createViaWorker is the replacement for the removed exported CreateVerification
// in test fixtures: phase A, then worker passes until the create row is sent,
// then the verification as applied. It fails the test if the create did not end
// sent.
func createViaWorker(t *testing.T, pool *db.Pool, outbound OutboundCredentialResolver, provider KYCProvider, f fixture) Verification {
	t.Helper()
	v, _ := requestCreate(t, pool, f, provider.ID())
	w := workerFor(pool, outbound, provider)
	passUntilQuiet(t, w)
	if r := onlyRow(t, pool, f.tenantID, v.ID, OpCreate); r.State != OutboxSent {
		t.Fatalf("create row state = %s (class %q), want sent", r.State, r.LastErrorClass)
	}
	var got Verification
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = GetVerificationByID(ctx, tx, v.ID)
		return err
	}); err != nil {
		t.Fatalf("read applied verification: %v", err)
	}
	return got
}

// submitViaWorker drives worker passes (the document uploads already enqueued
// their submit rows in their own transactions) and returns the verification as
// the worker left it. It replaces the removed exported SubmitVerification in
// test fixtures.
func submitViaWorker(t *testing.T, pool *db.Pool, outbound OutboundCredentialResolver, provider KYCProvider, tenantID, verificationID uuid.UUID) Verification {
	t.Helper()
	passUntilQuiet(t, workerFor(pool, outbound, provider))
	var got Verification
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		got, err = GetVerificationByID(ctx, tx, verificationID)
		return err
	}); err != nil {
		t.Fatalf("read verification: %v", err)
	}
	return got
}

// orphanViaPhaseA seeds a genuine phase-A-only orphan (a pending create row
// that no worker has touched): exactly the shape a crash after phase A leaves.
func orphanViaPhaseA(t *testing.T, pool *db.Pool, f fixture) uuid.UUID {
	t.Helper()
	v, _ := requestCreate(t, pool, f, "mock")
	return v.ID
}

// errIs is errors.Is for table tests.
func errIs(err, target error) bool { return errors.Is(err, target) }
