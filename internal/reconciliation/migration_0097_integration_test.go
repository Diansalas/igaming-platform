//go:build integration

// Stage 10.3 W2b (CAS-RECON-1), migration 0097 - on scratch databases
// only (DDL and trigger/index manipulation never touch the shared test
// database):
//   - up applies on a clean chain; down succeeds on a clean database and
//     round-trips (04-review-qa.md §5, clean-DB case only);
//   - down REFUSES, with the "roll forward" message, once a
//     casino_callback_rejections row or a casino_consistency mismatch
//     exists, and leaves the evidence untouched;
//   - the append-only deny trigger binds even when a row IS visible to an
//     UPDATE/DELETE (a permissive policy added in the fixture) - the
//     trigger, not RLS or the REVOKE, is the binding control; the fixture
//     then drops the trigger and shows the same UPDATE succeeding, so the
//     refusal is proven to come from the trigger;
//   - C5 (tombstone conflict), structurally impossible under the unique
//     index, is detected once that index is dropped.
package reconciliation

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0097Version = int64(97)

// migration0097DirThroughSelf copies the migrations directory up to and
// including 0097 (and nothing later), so "roll back 1 step" always means
// 0097 itself regardless of what lands on top of it later (the
// migration0094DirThroughSelf precedent).
func migration0097DirThroughSelf(t *testing.T) string {
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
		if err != nil || int64(v) > migration0097Version {
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

func migration0097Scratch(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	dir := migration0097DirThroughSelf(t)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through 0097: %v", err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != migration0097Version {
		t.Fatalf("expected 0097 to be the last applied migration, got %v", applied)
	}
	return pool, dir
}

func tableExists(t *testing.T, pool *db.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, "public."+name).Scan(&ok)
	}); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestMigration0097_CleanDatabaseDownThenUpRoundTrip(t *testing.T) {
	pool, dir := migration0097Scratch(t, "m0097rt_")
	if !tableExists(t, pool, "casino_callback_rejections") {
		t.Fatal("0097 up must create casino_callback_rejections")
	}
	down, err := pool.MigrateDown(context.Background(), dir, 1)
	if err != nil || len(down) != 1 || down[0] != migration0097Version {
		t.Fatalf("down on a clean database must roll back exactly 0097: %v %v", down, err)
	}
	if tableExists(t, pool, "casino_callback_rejections") {
		t.Fatal("down must drop casino_callback_rejections")
	}
	// The narrowed kind CHECK is back: a cas_* kind is refused.
	f := seedFixture(t, pool)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return persistRun(ctx, tx, Run{ID: uuid.New(), TenantID: f.tenantID, Stream: StreamCasinoConsistency,
			PeriodStart: time.Now(), PeriodEnd: time.Now(), RunAt: time.Now(), Status: StatusMismatchesFound},
			[]Mismatch{{ID: uuid.New(), TenantID: f.tenantID, ReconciliationKey: "k", ExpectedValue: "e", ActualValue: "a",
				MismatchKind: MismatchKindCasOrphanWin, InvestigationStatus: "open"}})
	})
	if err == nil || !strings.Contains(err.Error(), "mismatch_kind_check") {
		t.Fatalf("after down a cas_* kind must be refused by the restored CHECK, got %v", err)
	}
	up, err := pool.MigrateUp(context.Background(), dir)
	if err != nil || len(up) != 1 || up[0] != migration0097Version {
		t.Fatalf("re-applying 0097: %v %v", up, err)
	}
	if !tableExists(t, pool, "casino_callback_rejections") {
		t.Fatal("round trip must recreate the table")
	}
}

func TestMigration0097_DownRefusesOnceARejectionRowExists(t *testing.T) {
	pool, dir := migration0097Scratch(t, "m0097rej_")
	f := seedFixture(t, pool)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := casino.RecordCallbackRejection(ctx, tx, f.tenantID, "mock-casino",
			casino.CallbackRejection{Class: casino.RejectionBetNotFound, EventType: casino.CallbackEventWin, ProviderTxID: "w", Amount: 1}, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(context.Background(), dir, 1)
	if err == nil || !strings.Contains(err.Error(), "roll forward") || !strings.Contains(err.Error(), "casino_callback_rejections is not empty") {
		t.Fatalf("down must refuse with the roll-forward message, got %v", err)
	}
	var n int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("evidence must survive the refused down: n=%d err=%v", n, err)
	}
}

func TestMigration0097_DownRefusesOnceACasinoMismatchExists(t *testing.T) {
	pool, dir := migration0097Scratch(t, "m0097mm_")
	f := seedFixture(t, pool)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return persistRun(ctx, tx, Run{ID: uuid.New(), TenantID: f.tenantID, Stream: StreamCasinoConsistency,
			PeriodStart: time.Now(), PeriodEnd: time.Now(), RunAt: time.Now(), Status: StatusMismatchesFound},
			[]Mismatch{{ID: uuid.New(), TenantID: f.tenantID, ReconciliationKey: "k", ExpectedValue: "e", ActualValue: "a",
				MismatchKind: MismatchKindCasOrphanWin, InvestigationStatus: "open"}})
	}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(context.Background(), dir, 1)
	if err == nil || !strings.Contains(err.Error(), "roll forward") || !strings.Contains(err.Error(), "casino_consistency mismatch") {
		t.Fatalf("down must refuse with the roll-forward message, got %v", err)
	}
	if !tableExists(t, pool, "casino_callback_rejections") {
		t.Fatal("a refused down must drop nothing")
	}
}

// QA 04 §W2b: "the binding control is the trigger, REVOKE is defence in
// depth". A permissive UPDATE/DELETE policy is added (simulating a future
// mistake that makes rows visible to an UPDATE); the owner - who holds
// every privilege - is still refused by the trigger. Then, as the kill
// control, the trigger is dropped and the same UPDATE succeeds.
func TestMigration0097_DenyTriggerBindsEvenWithAPermissiveUpdatePolicy(t *testing.T) {
	pool, _ := migration0097Scratch(t, "m0097trg_")
	f := seedFixture(t, pool)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := casino.RecordCallbackRejection(ctx, tx, f.tenantID, "mock-casino",
			casino.CallbackRejection{Class: casino.RejectionBetNotFound, EventType: casino.CallbackEventWin, ProviderTxID: "w", Amount: 1}, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	exec := func(sql string) error {
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
	}
	if err := exec(`CREATE POLICY test_permissive_mutate ON casino_callback_rejections FOR ALL USING (true) WITH CHECK (true)`); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE casino_callback_rejections SET reason_class = 'payload_mismatch'`,
		`DELETE FROM casino_callback_rejections`,
		`TRUNCATE casino_callback_rejections`,
	} {
		if err := exec(stmt); err == nil || !strings.Contains(err.Error(), "append-only") && !strings.Contains(err.Error(), "not permitted") {
			t.Fatalf("%q must be refused by the deny trigger, got %v", stmt, err)
		}
	}
	// Kill control: without the row trigger the same UPDATE goes through.
	if err := exec(`DROP TRIGGER casino_callback_rejections_immutable ON casino_callback_rejections`); err != nil {
		t.Fatal(err)
	}
	if err := exec(`UPDATE casino_callback_rejections SET reason_class = 'payload_mismatch'`); err != nil {
		t.Fatalf("control: with the trigger dropped the UPDATE must succeed, got %v", err)
	}
}

// C5: a non-tombstone transaction under a casino tombstone's reference.
// The unique index makes this impossible; on a scratch database it is
// dropped to prove the backstop detects it.
func TestCasinoConsistency_C5_TombstoneConflict(t *testing.T) {
	pool, _ := migration0097Scratch(t, "recon_c5_")
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	w.mustRunClean(t)
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `DROP INDEX idx_ledger_transactions_tenant_provider_tx`)
		return err
	}); err != nil {
		t.Fatalf("drop provider-tx unique index on scratch: %v", err)
	}
	var tombstoneID uuid.UUID
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'tombstone'`, w.f.tenantID).Scan(&tombstoneID)
	}); err != nil {
		t.Fatal(err)
	}
	cash, house := w.accounts(t, w.f.walletID)
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := casino.BindProviderRound(ctx, tx, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.sessionID, w.game.ID, casProvider, "r5", nil); err != nil {
			return err
		}
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxCasinoBet,
			IdempotencyKey: casProvider + ":never-5", ProviderID: strp(casProvider), ProviderTxID: strp("never-5"),
			CorrelationID: corr(w.f.tenantID, "r5"),
			Entries:       []ledger.EntryInput{{LedgerAccountID: cash, Direction: ledger.Debit, Amount: 100}, {LedgerAccountID: house, Direction: ledger.Credit, Amount: 100}}})
		return err
	}); err != nil {
		t.Fatalf("post the conflicting original: %v", err)
	}
	_, ms, _ := w.run(t)
	mustOneOfKind(t, ms, MismatchKindCasTombstoneConflict, "tombstone="+tombstoneID.String(), "provider_tx_id=never-5")
	if len(ms) != 1 {
		t.Fatalf("the conflict must be the only finding, got %+v", ms)
	}
}
