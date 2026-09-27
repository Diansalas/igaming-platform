//go:build integration

// PRH-I1 step (a): migration 0101 (payment_attempts, payment_provider_events,
// guard triggers, RLS, backfill) - ADR 0095 §13.1, as fixed by the
// ledger-finance re-verification (N2 INSERT guard, N3 T12 sibling
// predicate, N4 backfill columns) and revision 3's T13t. Tests run on
// scratch databases only (DDL never touches the shared test database),
// following the migration_0099_integration_test.go pattern.
//
// migration0101Version is DERIVED from the real on-disk migrations/
// directory rather than hard-coded, because migration 0100 (ADR 0096's
// KYC enforcement migration, PRH-I3) lands on a sibling branch in
// parallel and may or may not be present in a given checkout of this
// branch. This file only ever asserts "the highest version on disk is
// migration 0101's own file", never "the highest version on disk is
// exactly 101".
package payments

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
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

const migration0101Filename = "0101_payment_attempts"

// migration0101Version derives the version number from the real
// migrations/ directory's own filename for 0101, rather than hard-coding
// the integer literal 101 - so a rename or a numbering change is caught
// here instead of silently testing the wrong file.
func migration0101Version(t *testing.T) int64 {
	t.Helper()
	dir := realMigrationsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), migration0101Filename+".up.sql") {
			return 101
		}
	}
	t.Fatalf("no %s.up.sql found under %s", migration0101Filename, dir)
	return 0
}

func realMigrationsDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// migration0101Dir copies every migration file with version <= through
// into a fresh temp dir, so a scratch database can stop exactly at 0101
// (or one migration earlier, at 0101-1, whatever that numerically
// resolves to given a possible sibling-branch gap at 0100).
func migration0101Dir(t *testing.T, through int64) string {
	t.Helper()
	src := realMigrationsDir(t)
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
		if err != nil {
			t.Fatalf("migration filename %q has no numeric version prefix", name)
		}
		if int64(v) > through {
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

// migration0101Scratch migrates a fresh scratch database up to and
// including migration `through` (which must be migration 0101's own
// version - the only exact-match caller in this file), and fails loudly
// if that migration was not actually applied.
func migration0101Scratch(t *testing.T, prefix string, through int64) (*db.Pool, string) {
	t.Helper()
	pool, applied, dir := migration0101ScratchApply(t, prefix, through)
	if len(applied) == 0 || applied[len(applied)-1] != through {
		t.Fatalf("expected %d to be the last applied migration, got %v", through, applied)
	}
	return pool, dir
}

// migration0101ScratchBefore101 migrates a fresh scratch database to
// everything strictly BEFORE migration 0101 - i.e. every on-disk
// migration numbered less than 0101's own version. It does NOT assert an
// exact "last applied" value, because migration 0100 (ADR 0096's KYC
// migration, PRH-I3) may or may not be present on disk in a given
// checkout of this branch (a transient, expected gap - see this file's
// package doc comment); the only invariant that matters here is that
// 0101 itself was NOT applied.
func migration0101ScratchBefore101(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	v := migration0101Version(t)
	pool, applied, dir := migration0101ScratchApply(t, prefix, v-1)
	if len(applied) > 0 && applied[len(applied)-1] >= v {
		t.Fatalf("expected every applied migration to be < %d, got %v", v, applied)
	}
	return pool, dir
}

func migration0101ScratchApply(t *testing.T, prefix string, through int64) (*db.Pool, []int64, string) {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	dir := migration0101Dir(t, through)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through %d: %v", through, err)
	}
	return pool, applied, dir
}

// --- fixtures -------------------------------------------------------------

type m0101Fixture struct {
	tenantID, brandID, playerID, walletID uuid.UUID
}

func seedM0101Fixture(t *testing.T, pool *db.Pool) m0101Fixture {
	t.Helper()
	f := m0101Fixture{tenantID: uuid.New(), brandID: uuid.New(), playerID: uuid.New()}
	personID := uuid.New()

	// Stage 4I Phase E-SECURITY (migration 0077): `tenants` writes require
	// a genuinely platform-admin-scoped transaction (mirrors
	// orchestrator_integration_test.go's seedOrchFixture exactly).
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1,$2,'m0101 tenant','under_platform_licence')`,
			f.tenantID, "m0101-"+f.tenantID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatalf("seed platform rows: %v", err)
	}

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1,$2,$3,'M0101 Brand')`,
			f.brandID, f.tenantID, "b-"+f.brandID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1,$2,$3,$4,$5,'x','active')`,
			f.playerID, f.tenantID, f.brandID, personID, f.playerID.String()+"@example.test"); err != nil {
			return err
		}
		w, err := wallet.GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerID, "EUR")
		if err != nil {
			return err
		}
		f.walletID = w.ID
		return nil
	}); err != nil {
		t.Fatalf("seed brand/player/wallet: %v", err)
	}
	return f
}

func insertDepositIntent(t *testing.T, pool *db.Pool, f m0101Fixture, status string, providerID, providerReference, ledgerTxID *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var ltID any
		if ledgerTxID != nil {
			lt := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1,$2,'deposit',$3,gen_random_uuid())`,
				lt, f.tenantID, "m0101-ltx-"+lt.String()); err != nil {
				return err
			}
			ltID = lt
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key, provider_id, provider_reference, status, ledger_transaction_id)
			 VALUES ($1,$2,$3,$4,$5,'EUR',1000,'card',$6,$7,$8,$9,$10)`,
			id, f.tenantID, f.brandID, f.playerID, f.walletID, "idem-"+id.String(), providerID, providerReference, status, ltID)
		return err
	})
	if err != nil {
		t.Fatalf("insert deposit intent: %v", err)
	}
	return id
}

func insertWithdrawalRequest(t *testing.T, pool *db.Pool, f m0101Fixture, state string, providerID, providerReference *string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key, provider_id, provider_reference)
			 VALUES ($1,$2,$3,$4,$5,'EUR',500,$6,$7,$8,$9)`,
			id, f.tenantID, f.brandID, f.playerID, f.walletID, state, "widem-"+id.String(), providerID, providerReference)
		return err
	})
	if err != nil {
		t.Fatalf("insert withdrawal request: %v", err)
	}
	return id
}

func ptr(s string) *string { return &s }

func isCheckOrTriggerViolation(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == "23514" || pgErr.Code == "P0001" || pgErr.Code == "23505"
}

// --- up/down/up round trip -------------------------------------------------

func TestMigration0101_UpDownUpRoundTrip(t *testing.T) {
	v := migration0101Version(t)
	pool, dir := migration0101Scratch(t, "m0101rt_", v)

	var tableCount int
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('payment_attempts','payment_provider_events')`).Scan(&tableCount)
	})
	if err != nil || tableCount != 2 {
		t.Fatalf("expected both tables to exist after up: count=%d err=%v", tableCount, err)
	}

	down, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil || len(down) != 1 || down[0] != v {
		t.Fatalf("down must roll back exactly %d: %v %v", v, down, err)
	}
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('payment_attempts','payment_provider_events')`).Scan(&tableCount)
	})
	if err != nil || tableCount != 0 {
		t.Fatalf("expected no orphaned tables after down: count=%d err=%v", tableCount, err)
	}

	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up after down: %v", err)
	}
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name IN ('payment_attempts','payment_provider_events')`).Scan(&tableCount)
	})
	if err != nil || tableCount != 2 {
		t.Fatalf("expected both tables to exist after re-up: count=%d err=%v", tableCount, err)
	}
}

// --- backfill: clean mapping -------------------------------------------------

func TestMigration0101_Backfill_CleanMapping(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101ScratchBefore101(t, "m0101bf_")
	f := seedM0101Fixture(t, pool)

	pendingWithRef := insertDepositIntent(t, pool, f, "pending", ptr("mock-psp"), ptr("ref-pending"), nil)
	pendingNoRef := insertDepositIntent(t, pool, f, "pending", ptr("mock-psp"), nil, nil)
	succeeded := insertDepositIntent(t, pool, f, "succeeded", ptr("mock-psp"), ptr("ref-succeeded"), ptr("x"))
	declinedRouted := insertDepositIntent(t, pool, f, "declined", ptr("mock-psp"), ptr("ref-declined"), nil)
	declinedUnrouted := insertDepositIntent(t, pool, f, "declined", nil, nil, nil)

	wSubmitted := insertWithdrawalRequest(t, pool, f, "submitted", ptr("mock-psp"), ptr("w-ref-submitted"))
	wCompleted := insertWithdrawalRequest(t, pool, f, "completed", ptr("mock-psp"), ptr("w-ref-completed"))
	wFailed := insertWithdrawalRequest(t, pool, f, "failed", ptr("mock-psp"), ptr("w-ref-failed"))
	wReversed := insertWithdrawalRequest(t, pool, f, "reversed", ptr("mock-psp"), ptr("w-ref-reversed"))
	wApproved := insertWithdrawalRequest(t, pool, f, "approved", nil, nil) // never dispatched: no attempt expected

	dir := migration0101Dir(t, v)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up through backfill: %v", err)
	}

	type row struct {
		state, evidence                               string
		legacy, everSent, interactive                 bool
		nextActionSet, firstSubmittedSet, lastSentSet bool
	}
	fetch := func(id uuid.UUID) row {
		var r row
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT state, last_evidence_kind, legacy_backfill, ever_possibly_sent, interactive,
				next_action_at IS NOT NULL, first_submitted_at IS NOT NULL, last_sent_at IS NOT NULL
				FROM payment_attempts WHERE id = $1`, id).
				Scan(&r.state, &r.evidence, &r.legacy, &r.everSent, &r.interactive, &r.nextActionSet, &r.firstSubmittedSet, &r.lastSentSet)
		})
		if err != nil {
			t.Fatalf("fetch attempt %s: %v", id, err)
		}
		return r
	}

	cases := []struct {
		name string
		id   uuid.UUID
		want row
	}{
		{"deposit pending w/ ref -> pending, polled", pendingWithRef, row{"pending", "legacy", true, true, true, true, true, true}},
		{"deposit pending no ref -> ambiguous, polled", pendingNoRef, row{"ambiguous", "legacy", true, true, true, true, true, true}},
		{"deposit succeeded -> succeeded", succeeded, row{"succeeded", "legacy", true, true, true, false, true, true}},
		{"deposit declined routed -> declined", declinedRouted, row{"declined", "legacy", true, true, true, false, true, true}},
		{"deposit declined unrouted -> rejected", declinedUnrouted, row{"rejected", "legacy", true, false, true, false, false, false}},
		{"withdrawal submitted -> pending, polled", wSubmitted, row{"pending", "legacy", true, true, true, true, true, true}},
		{"withdrawal completed -> succeeded", wCompleted, row{"succeeded", "legacy", true, true, true, false, true, true}},
		{"withdrawal failed -> declined", wFailed, row{"declined", "legacy", true, true, true, false, true, true}},
		{"withdrawal reversed -> succeeded", wReversed, row{"succeeded", "legacy", true, true, true, false, true, true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := fetch(c.id)
			if got != c.want {
				t.Fatalf("got %+v, want %+v", got, c.want)
			}
		})
	}

	// wApproved (never dispatched) must have NO attempt row at all.
	var n int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE withdrawal_request_id = $1`, wApproved).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("an approved-but-undispatched withdrawal must get no attempt row: n=%d err=%v", n, err)
	}

	// id = parent id (INV-IO-3 restated for legacy rows).
	var mref string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT merchant_reference FROM payment_attempts WHERE id = $1`, succeeded).Scan(&mref)
	}); err != nil || mref != succeeded.String() {
		t.Fatalf("merchant_reference must equal the parent id: got %q err=%v", mref, err)
	}

	// payout gets the 'legacy_unknown' sentinel payment_method.
	var pm string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payment_method FROM payment_attempts WHERE id = $1`, wCompleted).Scan(&pm)
	}); err != nil || pm != "legacy_unknown" {
		t.Fatalf("backfilled payout payment_method must be legacy_unknown: got %q err=%v", pm, err)
	}
}

// TestMigration0101_Backfill_NonTerminalIntentPickedUpBySweep is the N4
// regression the ledger-finance re-verification asked for: a backfilled
// ambiguous intent must have next_action_at set, or idx_payment_attempts_due
// never surfaces it and the sweeper can never converge it.
func TestMigration0101_Backfill_NonTerminalIntentPickedUpBySweep(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101ScratchBefore101(t, "m0101sweep_")
	f := seedM0101Fixture(t, pool)
	ambiguous := insertDepositIntent(t, pool, f, "ambiguous", ptr("mock-psp"), nil, nil)

	dir := migration0101Dir(t, v)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	var due bool
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM payment_attempts WHERE deposit_intent_id = $1 AND next_action_at IS NOT NULL)`, ambiguous).Scan(&due)
	})
	if err != nil || !due {
		t.Fatalf("backfilled non-terminal attempt must be due for the sweeper: due=%v err=%v", due, err)
	}
}

// --- backfill: pre-flight aborts -------------------------------------------

func TestMigration0101_Backfill_PreflightAbortsOnAmbiguousIntentWithNoProvider(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101ScratchBefore101(t, "m0101pfa_")
	f := seedM0101Fixture(t, pool)
	insertDepositIntent(t, pool, f, "ambiguous", nil, nil, nil)

	dir := migration0101Dir(t, v)
	_, err := pool.MigrateUp(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "deposit_intents.pending_ambiguous_no_provider") {
		t.Fatalf("expected pre-flight 1 to abort, got %v", err)
	}
	assertMigrationNotRecorded(t, pool, v)
}

func TestMigration0101_Backfill_PreflightAbortsOnSucceededIntentMissingLedgerLink(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101ScratchBefore101(t, "m0101pfb_")
	f := seedM0101Fixture(t, pool)
	insertDepositIntent(t, pool, f, "succeeded", ptr("mock-psp"), ptr("ref"), nil) // no ledger_transaction_id

	dir := migration0101Dir(t, v)
	_, err := pool.MigrateUp(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "deposit_intents.succeeded_missing_link") {
		t.Fatalf("expected pre-flight 2 to abort, got %v", err)
	}
	assertMigrationNotRecorded(t, pool, v)
}

func TestMigration0101_Backfill_PreflightAbortsOnSubmittedWithdrawalMissingReference(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101ScratchBefore101(t, "m0101pfc_")
	f := seedM0101Fixture(t, pool)
	insertWithdrawalRequest(t, pool, f, "submitted", ptr("mock-psp"), nil)

	dir := migration0101Dir(t, v)
	_, err := pool.MigrateUp(context.Background(), dir)
	if err == nil || !strings.Contains(err.Error(), "withdrawal_requests.submitted_no_reference") {
		t.Fatalf("expected pre-flight 3 to abort, got %v", err)
	}
	assertMigrationNotRecorded(t, pool, v)
}

func assertMigrationNotRecorded(t *testing.T, pool *db.Pool, version int64) {
	t.Helper()
	var applied bool
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version).Scan(&applied)
	}); err != nil || applied {
		t.Fatalf("a failed pre-flight must not record the migration as applied: applied=%v err=%v", applied, err)
	}
	var n int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM information_schema.tables WHERE table_name = 'payment_attempts'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a failed pre-flight must leave no partially-created table: n=%d err=%v", n, err)
	}
}

// --- RLS cross-tenant --------------------------------------------------------

func TestMigration0101_RLS_CrossTenantIsolation(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101Scratch(t, "m0101rls_", v)
	a := seedM0101Fixture(t, pool)
	b := seedM0101Fixture(t, pool)

	intentA := insertDepositIntent(t, pool, a, "pending", ptr("mock-psp"), ptr("rls-ref-a"), nil)
	_ = insertDepositIntent(t, pool, b, "pending", ptr("mock-psp"), ptr("rls-ref-b"), nil)

	// Direct INSERT into payment_attempts as tenant A must never be
	// visible to, or affectable from, tenant B's scope.
	attemptID := uuid.New()
	err := pool.WithTenant(context.Background(), a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
			 VALUES ($1,$2,'deposit',$3,1,'card','EUR',1000,true,$4,$5,'created','platform')`,
			attemptID, a.tenantID, intentA, attemptID.String(), "pa:"+attemptID.String())
		return err
	})
	if err != nil {
		t.Fatalf("insert attempt as tenant A: %v", err)
	}

	var n int
	if err := pool.WithTenant(context.Background(), b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE id = $1`, attemptID).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("tenant B must not see tenant A's payment_attempts row: n=%d err=%v", n, err)
	}

	// Tenant B cannot UPDATE tenant A's row either (RLS WITH CHECK/USING).
	err = pool.WithTenant(context.Background(), b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = 'rejected', terminal_reason='x', last_evidence_kind='platform' WHERE id = $1 AND state='created'`, attemptID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			t.Fatalf("tenant B must not be able to update tenant A's row")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant update attempt: %v", err)
	}

	// payment_provider_events: same isolation.
	evID := uuid.New()
	err = pool.WithTenant(context.Background(), a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_provider_events (id, tenant_id, provider_id, event_type, provider_reference, outcome, event_fingerprint, disposition_at_receipt)
			 VALUES ($1,$2,'mock-psp','deposit','rls-evt-a','pending','\x00','deferred_unresolved')`,
			evID, a.tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("insert receipt as tenant A: %v", err)
	}
	if err := pool.WithTenant(context.Background(), b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_provider_events WHERE id = $1`, evID).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("tenant B must not see tenant A's payment_provider_events row: n=%d err=%v", n, err)
	}
}

// --- trigger: INSERT guard (N2, MX22) ---------------------------------------

func TestMigration0101_InsertGuard_RejectsForbiddenShapes(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101Scratch(t, "m0101ins_", v)
	f := seedM0101Fixture(t, pool)
	intentID := insertDepositIntent(t, pool, f, "pending", ptr("mock-psp"), nil, nil)

	baseCols := `id, tenant_id, operation, deposit_intent_id, attempt_no, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key`
	try := func(extraCols, extraVals string, args ...any) error {
		id := uuid.New()
		// $4/$5/... in extraVals are shifted by the fixed prefix below, so
		// caller-supplied placeholders in the table's cases start at $6.
		full := append([]any{id, f.tenantID, intentID, id.String(), "pa:" + id.String()}, args...)
		placeholders := "$1,$2,'deposit',$3,1,'card','EUR',1000,true,$4,$5"
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO payment_attempts (`+baseCols+extraCols+`) VALUES (`+placeholders+extraVals+`)`, full...)
			return err
		})
	}

	cases := []struct {
		name      string
		extraCols string
		extraVals string
		args      []any
	}{
		{"state=succeeded", ", state, last_evidence_kind, provider_id, provider_reference", ",'succeeded','sync',$6,$7", []any{"mock-psp", "forbidden-succeeded"}},
		{"state=declined", ", state, last_evidence_kind, provider_id", ",'declined','sync',$6", []any{"mock-psp"}},
		{"state=disputed", ", state, last_evidence_kind", ",'disputed','platform'", nil},
		{"legacy_backfill=true", ", state, last_evidence_kind, legacy_backfill", ",'created','platform',true", nil},
		{"last_evidence_kind=legacy", ", state, last_evidence_kind", ",'created','legacy'", nil},
		{"ever_possibly_sent=true", ", state, last_evidence_kind, ever_possibly_sent", ",'created','platform',true", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := try(c.extraCols, c.extraVals, c.args...)
			if !isCheckOrTriggerViolation(err) {
				t.Fatalf("expected the INSERT guard to reject %q, got %v", c.name, err)
			}
		})
	}

	// The two legitimate shapes must still succeed.
	okID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payment_attempts (`+baseCols+`, state, last_evidence_kind) VALUES ($1,$2,'deposit',$3,1,'card','EUR',1000,true,$4,$5,'created','platform')`,
			okID, f.tenantID, intentID, okID.String(), "pa:"+okID.String())
		return err
	}); err != nil {
		t.Fatalf("a legitimate T1 (created) insert must succeed: %v", err)
	}
}

// --- trigger: UPDATE state-pair whitelist -----------------------------------

func TestMigration0101_UpdateGuard_TransitionWhitelist(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101Scratch(t, "m0101upd_", v)
	f := seedM0101Fixture(t, pool)

	// newAttempt only ever INSERTs the two shapes the guard's INSERT half
	// allows (created, or submitting once provider-routed) - reaching any
	// other state is done via legitimate UPDATE transitions below, never
	// by inserting directly into it.
	newAttempt := func(insertState string) uuid.UUID {
		intentID := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)
		id := uuid.New()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			providerID := any(nil)
			if insertState != "created" {
				providerID = "mock-psp"
			}
			_, err := tx.Exec(ctx,
				`INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, provider_id, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
				 VALUES ($1,$2,'deposit',$3,1,$4,'card','EUR',1000,true,$5,$6,$7,'platform')`,
				id, f.tenantID, intentID, providerID, id.String(), "pa:"+id.String(), insertState)
			return err
		})
		if err != nil {
			t.Fatalf("seed attempt in state %s: %v", insertState, err)
		}
		return id
	}

	setState := func(id uuid.UUID, newState, evidence string, extraSet string, extraArgs ...any) error {
		args := append([]any{id, newState, evidence}, extraArgs...)
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = $2, last_evidence_kind = $3, updated_at = now()`+extraSet+` WHERE id = $1`, args...)
			return err
		})
	}

	// Forbidden: succeeded -> anything. Reaching succeeded legitimately
	// (T7) requires a provider_reference and, for a deposit, a ledger
	// link (both CHECK-enforced).
	ltID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1,$2,'deposit',$3,gen_random_uuid())`,
			ltID, f.tenantID, "m0101upd-ltx-"+ltID.String())
		return err
	}); err != nil {
		t.Fatalf("seed ledger transaction: %v", err)
	}
	succ := newAttempt("submitting")
	if err := setState(succ, "succeeded", "sync", ", provider_reference = $4, ledger_transaction_id = $5, resolved_at = now()", "succ-ref", ltID); err != nil {
		t.Fatalf("submitting->succeeded must be allowed: %v", err)
	}
	if err := setState(succ, "disputed", "sync", ""); !isCheckOrTriggerViolation(err) {
		t.Fatalf("succeeded->disputed must be rejected, got %v", err)
	}

	// Forbidden: disputed -> anything (M1/M2 not built). Reaching
	// disputed legitimately from submitting is T10, no extra columns
	// required.
	disp := newAttempt("submitting")
	if err := setState(disp, "disputed", "callback", ", terminal_reason = 'test_dispute'"); err != nil {
		t.Fatalf("submitting->disputed (T10) must be allowed: %v", err)
	}
	if err := setState(disp, "succeeded", "sync", ", provider_reference = 'x'"); !isCheckOrTriggerViolation(err) {
		t.Fatalf("disputed->succeeded must be rejected, got %v", err)
	}

	// Forbidden: rejected -> anything except disputed (T15). Reaching
	// rejected legitimately from created is T3.
	rej := newAttempt("created")
	if err := setState(rej, "rejected", "platform", ", terminal_reason = 'test_reject', resolved_at = now()"); err != nil {
		t.Fatalf("created->rejected (T3) must be allowed: %v", err)
	}
	if err := setState(rej, "created", "platform", ""); !isCheckOrTriggerViolation(err) {
		t.Fatalf("rejected->created must be rejected, got %v", err)
	}

	// INV-IO-7: payout ->declined requires sync/callback/query_status.
	payoutSubmitting := uuid.New()
	wID := insertWithdrawalRequest(t, pool, f, "submitted", ptr("mock-psp"), ptr("payout-guard-ref"))
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_attempts (id, tenant_id, operation, withdrawal_request_id, attempt_no, provider_id, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
			 VALUES ($1,$2,'payout',$3,1,'mock-psp','bank_transfer','EUR',500,true,$4,$5,'submitting','platform')`,
			payoutSubmitting, f.tenantID, wID, payoutSubmitting.String(), "pa:"+payoutSubmitting.String())
		return err
	}); err != nil {
		t.Fatalf("seed payout attempt: %v", err)
	}
	if err := setState(payoutSubmitting, "declined", "operator", ""); !isCheckOrTriggerViolation(err) {
		t.Fatalf("a payout ->declined with last_evidence_kind=operator must be rejected, got %v", err)
	}
	if err := setState(payoutSubmitting, "declined", "sync", ""); err != nil {
		t.Fatalf("a payout ->declined with sync evidence must succeed: %v", err)
	}

	// A deposit ->declined must never carry last_evidence_kind=operator.
	depositSubmitting := newAttempt("submitting")
	if err := setState(depositSubmitting, "declined", "operator", ""); !isCheckOrTriggerViolation(err) {
		t.Fatalf("a deposit ->declined with last_evidence_kind=operator must be rejected, got %v", err)
	}
}

// --- T13t (tombstone) ---------------------------------------------------

func TestMigration0101_T13t_TombstonePrecedesSuccess_RequiresNamedTerminalReason(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101Scratch(t, "m0101t13t_", v)
	f := seedM0101Fixture(t, pool)
	intentID := insertDepositIntent(t, pool, f, "declined", ptr("mock-psp"), ptr("t13t-ref"), nil)

	// The INSERT guard (N2) forbids an inserted row from already carrying
	// a provider_reference, so the attempt starts in submitting WITHOUT
	// one, and the reference is attached by the submitting->declined
	// (T8) UPDATE - exactly the shape ApplyDecline produces.
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, provider_id, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
			 VALUES ($1,$2,'deposit',$3,1,'mock-psp','card','EUR',1000,true,$4,$5,'submitting','platform')`,
			id, f.tenantID, intentID, id.String(), "pa:"+id.String())
		return err
	}); err != nil {
		t.Fatalf("seed submitting attempt: %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='declined', last_evidence_kind='sync', decline_stage='after_acceptance', cascadable=false, provider_reference='t13t-ref', resolved_at=now() WHERE id=$1`, id)
		return err
	}); err != nil {
		t.Fatalf("submitting->declined: %v", err)
	}

	// Without the named terminal_reason, declined(deposit)->disputed must
	// be rejected (defense in depth for T13t) - direct SQL, since
	// ApplyDisputeFromNonTerminal only matches submitting/pending/ambiguous.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='disputed', last_evidence_kind='callback', terminal_reason='some_other_reason' WHERE id=$1`, id)
		return err
	}); !isCheckOrTriggerViolation(err) {
		t.Fatalf("T13t without the named terminal_reason must be rejected, got %v", err)
	}

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ApplyTombstonePrecedesSuccess(ctx, tx, id, EvidenceCallback)
	}); err != nil {
		t.Fatalf("ApplyTombstonePrecedesSuccess (T13t) must succeed: %v", err)
	}

	var state, reason string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT state, terminal_reason FROM payment_attempts WHERE id=$1`, id).Scan(&state, &reason)
	}); err != nil || state != "disputed" || reason != TerminalReasonTombstonePrecedesSuccess {
		t.Fatalf("expected disputed/%s, got %s/%s (err=%v)", TerminalReasonTombstonePrecedesSuccess, state, reason, err)
	}
}

// --- T12 legacy_backfill and N3 sibling-succeeded guards --------------------

func TestMigration0101_T12_RefusedOnLegacyBackfillRow(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101ScratchBefore101(t, "m0101t12leg_")
	f := seedM0101Fixture(t, pool)
	intentID := insertDepositIntent(t, pool, f, "ambiguous", ptr("mock-psp"), nil, nil)

	dir := migration0101Dir(t, v)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	var attemptID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM payment_attempts WHERE deposit_intent_id=$1`, intentID).Scan(&attemptID)
	}); err != nil {
		t.Fatalf("find backfilled attempt: %v", err)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ResubmitAmbiguous(ctx, tx, attemptID, uuid.New(), "sweeper-1", time.Now().Add(time.Hour), 0)
	})
	if !errors.Is(err, ErrAttemptStateConflict) {
		t.Fatalf("T12 on a legacy_backfill row must be refused, got %v", err)
	}
}

// TestMigration0101_T12_RefusedWhenSiblingSucceeded is the N3 fix
// (RV-0095 ledger, MX23): once a sibling attempt of the same deposit
// intent has succeeded, a T12 resend of a different, still-ambiguous
// sibling must be refused - both by the application CAS predicate in
// ResubmitAmbiguous and, independently, by the guard trigger.
func TestMigration0101_T12_RefusedWhenSiblingSucceeded(t *testing.T) {
	v := migration0101Version(t)
	pool, _ := migration0101Scratch(t, "m0101t12sib_", v)
	f := seedM0101Fixture(t, pool)
	intentID := insertDepositIntent(t, pool, f, "pending", nil, nil, nil)

	ltID := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id) VALUES ($1,$2,'deposit',$3,gen_random_uuid())`,
			ltID, f.tenantID, "m0101-n3-ltx-"+ltID.String())
		return err
	}); err != nil {
		t.Fatalf("seed ledger transaction: %v", err)
	}

	// Sibling 1: succeeded, with its required ledger link. The INSERT
	// guard (N2) forbids a row from carrying a provider_reference at
	// insert time, so it is attached by the submitting->succeeded (T7)
	// UPDATE instead.
	sib1 := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, provider_id, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
			 VALUES ($1,$2,'deposit',$3,1,'mock-psp','card','EUR',1000,true,$4,$5,'submitting','platform')`,
			sib1, f.tenantID, intentID, sib1.String(), "pa:"+sib1.String())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_attempts SET state='succeeded', last_evidence_kind='sync', provider_reference='sib1-ref', ledger_transaction_id=$2, resolved_at=now() WHERE id=$1`, sib1, ltID)
		return err
	}); err != nil {
		t.Fatalf("seed succeeded sibling: %v", err)
	}

	// Sibling 2: still ambiguous.
	sib2 := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, provider_id, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind)
			 VALUES ($1,$2,'deposit',$3,2,'mock-psp2','card','EUR',1000,true,$4,$5,'submitting','platform')`,
			sib2, f.tenantID, intentID, sib2.String(), "pa:"+sib2.String())
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE payment_attempts SET state='ambiguous', last_evidence_kind='sync', ever_possibly_sent=true WHERE id=$1`, sib2)
		return err
	}); err != nil {
		t.Fatalf("seed ambiguous sibling: %v", err)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ResubmitAmbiguous(ctx, tx, sib2, uuid.New(), "sweeper-1", time.Now().Add(time.Hour), 0)
	})
	if !errors.Is(err, ErrAttemptStateConflict) {
		t.Fatalf("T12 on sib2 must be refused once sib1 succeeded, got %v", err)
	}

	// Direct SQL bypassing ResubmitAmbiguous entirely must also be
	// rejected, by the guard trigger's own independent N3 check.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state='submitting', last_evidence_kind='platform', claim_token=gen_random_uuid(), submit_count=submit_count+1, last_sent_at=now() WHERE id=$1`, sib2)
		return err
	})
	if !isCheckOrTriggerViolation(err) {
		t.Fatalf("direct SQL T12 on sib2 must be rejected by the trigger, got %v", err)
	}
}
