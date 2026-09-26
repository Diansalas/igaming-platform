//go:build integration

// Stage 10.3 W2b (CAS-RECON-1): the verified-only casino callback
// rejection record at the orchestrator level - the two in-transaction
// write paths (E3's declined bet, E9's distinct second rollback
// reference), the wrapped-error contract the HTTP layer's separately
// committed write relies on (E10 and every other error class), idempotency
// under redelivery and concurrency, I1 (nothing before verification), RLS
// and append-only enforcement. The HTTP-level, every-class coverage is
// internal/httpserver/casino_rejection_record_integration_test.go.
package casino

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

type rejRow struct {
	eventType, providerTxID, reasonClass string
	original, round, asset, amount       *string
}

func loadRejections(t *testing.T, pool *db.Pool, tenantID uuid.UUID) []rejRow {
	t.Helper()
	var out []rejRow
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT event_type, provider_tx_id, reason_class, original_provider_tx_id, round_id, asset_code, amount::text
			  FROM casino_callback_rejections WHERE tenant_id = $1 ORDER BY first_seen_at, id`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r rejRow
			if err := rows.Scan(&r.eventType, &r.providerTxID, &r.reasonClass, &r.original, &r.round, &r.asset, &r.amount); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("load rejections: %v", err)
	}
	return out
}

func auditActionCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenantID, action).Scan(&n)
	}); err != nil {
		t.Fatalf("count audit %s: %v", action, err)
	}
	return n
}

type rejEnv struct {
	pool      *db.Pool
	f         casinoFixture
	provider  *MockCasinoProvider
	orch      *Orchestrator
	sessionID uuid.UUID
}

func newRejEnv(t *testing.T) rejEnv {
	t.Helper()
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 50_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	return rejEnv{pool: pool, f: f, provider: provider, sessionID: sessionID,
		orch: NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))}
}

func (e rejEnv) deliver(t *testing.T, eventType CallbackEventType, ref, original, round string, amount int64) (ReceiveCallbackResult, error) {
	t.Helper()
	payload := e.provider.CallbackPayload(e.f.tenantID, eventType, ref, original, round, "game-1", amount, "EUR", OutcomeSucceeded, "", e.f.playerAccountID, e.sessionID)
	if eventType == CallbackEventRollback {
		payload = e.provider.CallbackPayload(e.f.tenantID, eventType, ref, original, round, "game-1", amount, "EUR", "", "", e.f.playerAccountID, uuid.Nil)
	}
	var result ReceiveCallbackResult
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", payload)
		return err
	})
	return result, err
}

// recordAfterRollback is exactly what the HTTP layer does
// (recordCasinoCallbackRejection): a separate, fresh tenant-scoped
// transaction after the callback's own one rolled back.
func (e rejEnv) recordAfterRollback(t *testing.T, err error) bool {
	t.Helper()
	var rej *CallbackRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("expected a *CallbackRejectedError, got %T: %v", err, err)
	}
	var inserted bool
	if werr := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		inserted, err = RecordCallbackRejection(ctx, tx, e.f.tenantID, rej.ProviderID, rej.Rejection, "req-test")
		return err
	}); werr != nil {
		t.Fatalf("record rejection: %v", werr)
	}
	return inserted
}

// E3: a late bet after its tombstone is DECLINED (commits); its rejection
// row commits in the same transaction, once per key, while the audit row
// stays per-attempt (ADR 0025 Stage 10.3 amendment item 9).
func TestCallbackRejection_E3_LateBetRecordedInSameTransactionOncePerKey(t *testing.T) {
	e := newRejEnv(t)
	if _, err := e.deliver(t, CallbackEventRollback, "rb-e3", "bet-e3", "round-e3", 0); err != nil {
		t.Fatalf("tombstoning rollback: %v", err)
	}
	before := noeffect.CaptureCasino(t, e.pool, []uuid.UUID{e.f.tenantID})
	balance := cashBalance(t, e.pool, e.f)

	for i := 0; i < 2; i++ {
		res, err := e.deliver(t, CallbackEventBet, "bet-e3", "", "round-e3", 1500)
		if err != nil {
			t.Fatalf("E3 delivery %d: %v", i, err)
		}
		if res.Outcome != OutcomeDeclined || res.DeclineReason != "original_rolled_back" {
			t.Fatalf("E3 delivery %d: expected declined/original_rolled_back, got %+v", i, res)
		}
	}
	rows := loadRejections(t, e.pool, e.f.tenantID)
	if len(rows) != 1 {
		t.Fatalf("expected exactly 1 rejection row after 2 deliveries, got %d: %+v", len(rows), rows)
	}
	r := rows[0]
	if r.eventType != "bet" || r.providerTxID != "bet-e3" || r.reasonClass != string(RejectionOriginalTombstoned) ||
		r.original != nil || r.round == nil || *r.round != "round-e3" || r.amount == nil || *r.amount != "1500" || r.asset == nil || *r.asset != "EUR" {
		t.Fatalf("unexpected E3 row: %+v", r)
	}
	if n := auditActionCount(t, e.pool, e.f.tenantID, "casino_bet.rejected_tombstoned"); n != 2 {
		t.Fatalf("expected 2 per-attempt casino_bet.rejected_tombstoned audit rows, got %d", n)
	}
	// No-effect checklist: the only audit delta is the two E3 rows.
	if cashBalance(t, e.pool, e.f) != balance {
		t.Fatal("E3 rejection record must not change any balance")
	}
	after := noeffect.CaptureCasino(t, e.pool, []uuid.UUID{e.f.tenantID})
	if after.LedgerTransactionCount[e.f.tenantID] != before.LedgerTransactionCount[e.f.tenantID] ||
		after.LedgerEntryCount[e.f.tenantID] != before.LedgerEntryCount[e.f.tenantID] ||
		after.CasinoProviderRoundCount[e.f.tenantID] != before.CasinoProviderRoundCount[e.f.tenantID] ||
		after.AuditLogCount[e.f.tenantID] != before.AuditLogCount[e.f.tenantID]+2 {
		t.Fatalf("E3 must post nothing and bind no round: before=%+v after=%+v", before, after)
	}
}

// E10: the callback's transaction rolls back, so the orchestrator hands the
// verified event back in a *CallbackRejectedError; nothing is written on
// the rolled-back transaction, and the separately committed write is
// idempotent.
func TestCallbackRejection_E10_WrappedErrorCarriesVerifiedEventAndRecordsOnce(t *testing.T) {
	e := newRejEnv(t)
	if _, err := e.deliver(t, CallbackEventBet, "bet-e10", "", "round-e10", 1000); err != nil {
		t.Fatalf("bet: %v", err)
	}
	if _, err := e.deliver(t, CallbackEventRollback, "rb-e10", "win-e10", "round-e10", 0); err != nil {
		t.Fatalf("tombstoning rollback of the unseen win: %v", err)
	}
	before := noeffect.CaptureCasino(t, e.pool, []uuid.UUID{e.f.tenantID})

	_, err := e.deliver(t, CallbackEventWin, "win-e10", "", "round-e10", 700)
	if !errors.Is(err, ErrOriginalTombstoned) {
		t.Fatalf("expected ErrOriginalTombstoned, got %v", err)
	}
	var rej *CallbackRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("expected a *CallbackRejectedError, got %T", err)
	}
	if rej.ProviderID != "mock-casino" || rej.Rejection.Class != RejectionOriginalTombstoned || rej.Rejection.EventType != CallbackEventWin ||
		rej.Rejection.ProviderTxID != "win-e10" || rej.Rejection.RoundID != "round-e10" || rej.Rejection.Amount != 700 {
		t.Fatalf("unexpected wrapped rejection: %+v", rej)
	}
	if got := loadRejections(t, e.pool, e.f.tenantID); len(got) != 0 {
		t.Fatalf("the rolled-back callback transaction must not leave a row, got %+v", got)
	}
	noeffect.AssertNoCasinoEffect(t, e.pool, []uuid.UUID{e.f.tenantID}, before)

	if !e.recordAfterRollback(t, err) {
		t.Fatal("first separately-committed write must insert")
	}
	_, err2 := e.deliver(t, CallbackEventWin, "win-e10", "", "round-e10", 700)
	if e.recordAfterRollback(t, err2) {
		t.Fatal("a redelivered rejection must not insert a second row")
	}
	rows := loadRejections(t, e.pool, e.f.tenantID)
	if len(rows) != 1 || rows[0].eventType != "win" || rows[0].reasonClass != "original_tombstoned" || rows[0].amount == nil || *rows[0].amount != "700" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
	noeffect.AssertNoCasinoEffect(t, e.pool, []uuid.UUID{e.f.tenantID}, before)
}

// C9 extension: an E9 rollback naming an already-tombstoned original under
// a DIFFERENT reference is still acknowledged with the idempotent tombstone
// result, and is now recorded (once per reference). A same-reference
// redelivery records nothing.
func TestCallbackRejection_E9_DistinctReferenceRecordedSameReferenceNot(t *testing.T) {
	e := newRejEnv(t)
	first, err := e.deliver(t, CallbackEventRollback, "rb-e9-1", "orig-e9", "round-e9", 0)
	if err != nil || !first.Tombstoned || first.LedgerTransactionID == nil {
		t.Fatalf("first rollback must tombstone: %+v %v", first, err)
	}
	before := noeffect.CaptureCasino(t, e.pool, []uuid.UUID{e.f.tenantID})

	same, err := e.deliver(t, CallbackEventRollback, "rb-e9-1", "orig-e9", "round-e9", 0)
	if err != nil || !same.Tombstoned || *same.LedgerTransactionID != *first.LedgerTransactionID {
		t.Fatalf("same-reference redelivery: %+v %v", same, err)
	}
	if got := loadRejections(t, e.pool, e.f.tenantID); len(got) != 0 {
		t.Fatalf("a same-reference redelivery must record nothing, got %+v", got)
	}

	for i := 0; i < 2; i++ {
		res, err := e.deliver(t, CallbackEventRollback, "rb-e9-2", "orig-e9", "round-e9", 0)
		if err != nil || !res.Tombstoned || *res.LedgerTransactionID != *first.LedgerTransactionID {
			t.Fatalf("distinct reference delivery %d: %+v %v", i, res, err)
		}
	}
	rows := loadRejections(t, e.pool, e.f.tenantID)
	if len(rows) != 1 {
		t.Fatalf("expected 1 row for the distinct reference, got %+v", rows)
	}
	r := rows[0]
	if r.eventType != "rollback" || r.providerTxID != "rb-e9-2" || r.reasonClass != string(RejectionRollbackOfTombstonedOriginal) ||
		r.original == nil || *r.original != "orig-e9" || r.amount != nil {
		t.Fatalf("unexpected E9 row: %+v", r)
	}
	if _, err := e.deliver(t, CallbackEventRollback, "rb-e9-3", "orig-e9", "round-e9", 0); err != nil {
		t.Fatalf("third distinct reference: %v", err)
	}
	if got := loadRejections(t, e.pool, e.f.tenantID); len(got) != 2 {
		t.Fatalf("a second distinct reference is its own row, got %+v", got)
	}
	// No money moved: no new ledger rows, no audit rows of its own.
	noeffect.AssertNoCasinoEffect(t, e.pool, []uuid.UUID{e.f.tenantID}, before)
}

// Concurrency: N distinct rollback references for one unseen original race.
// Exactly one tombstone; every OTHER reference is recorded exactly once.
func TestCallbackRejection_E9_ConcurrentDistinctReferences(t *testing.T) {
	e := newRejEnv(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			res, err := e.deliver(t, CallbackEventRollback, fmt.Sprintf("rb-conc-%d", i), "orig-conc", "round-conc", 0)
			if err == nil && !res.Tombstoned {
				err = fmt.Errorf("delivery %d not tombstoned: %+v", i, res)
			}
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var tombstones int
	if err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'tombstone'`, e.f.tenantID).Scan(&tombstones)
	}); err != nil {
		t.Fatal(err)
	}
	if tombstones != 1 {
		t.Fatalf("expected exactly 1 tombstone, got %d", tombstones)
	}
	if got := loadRejections(t, e.pool, e.f.tenantID); len(got) != n-1 {
		t.Fatalf("expected %d rejection rows (every non-tombstoning reference), got %d", n-1, len(got))
	}
}

// Concurrency + idempotency: N identical concurrent E10 deliveries, each
// followed by its own separately committed write, produce one row.
func TestCallbackRejection_E10_ConcurrentIdenticalDeliveriesRecordOnce(t *testing.T) {
	e := newRejEnv(t)
	if _, err := e.deliver(t, CallbackEventBet, "bet-c10", "", "round-c10", 1000); err != nil {
		t.Fatal(err)
	}
	if _, err := e.deliver(t, CallbackEventRollback, "rb-c10", "win-c10", "round-c10", 0); err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	inserted := make(chan bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.deliver(t, CallbackEventWin, "win-c10", "", "round-c10", 300)
			var rej *CallbackRejectedError
			if !errors.As(err, &rej) {
				t.Errorf("expected *CallbackRejectedError, got %v", err)
				return
			}
			var ins bool
			if werr := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				ins, err = RecordCallbackRejection(ctx, tx, e.f.tenantID, rej.ProviderID, rej.Rejection, "")
				return err
			}); werr != nil {
				t.Errorf("record: %v", werr)
				return
			}
			inserted <- ins
		}()
	}
	wg.Wait()
	close(inserted)
	var wins int
	for ins := range inserted {
		if ins {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected exactly one insert among %d concurrent writes, got %d", n, wins)
	}
	if got := loadRejections(t, e.pool, e.f.tenantID); len(got) != 1 {
		t.Fatalf("expected 1 row, got %d", len(got))
	}
}

// I1: a callback that fails verification is never wrapped and never
// recorded - no event exists before verification.
func TestCallbackRejection_UnverifiedCallbackNeverWrappedNorRecorded(t *testing.T) {
	e := newRejEnv(t)
	if _, err := e.deliver(t, CallbackEventRollback, "rb-i1", "win-i1", "round-i1", 0); err != nil {
		t.Fatal(err)
	}
	// A win that WOULD be E10 if verified - tampered after signing.
	payload := e.provider.CallbackPayload(e.f.tenantID, CallbackEventWin, "win-i1", "", "round-i1", "game-1", 500, "EUR", OutcomeSucceeded, "", e.f.playerAccountID, uuid.Nil)
	tampered := webhookauth.Inbound{Header: payload.Header, Body: append([]byte(nil), payload.Body...)}
	tampered.Body[len(tampered.Body)-2] ^= 0x01
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", tampered)
		return err
	})
	var authErr *webhookauth.AuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected an AuthError for a tampered body, got %v", err)
	}
	var rej *CallbackRejectedError
	if errors.As(err, &rej) {
		t.Fatal("a pre-verification failure must never be wrapped as a rejection")
	}
	if got := loadRejections(t, e.pool, e.f.tenantID); len(got) != 0 {
		t.Fatalf("expected zero rows, got %+v", got)
	}
}

func assertPgCode(t *testing.T, err error, code string) {
	t.Helper()
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != code {
		t.Fatalf("expected SQLSTATE %s, got %v", code, err)
	}
}

// RLS (FORCE, bound to app.tenant_id, player-excluded) and append-only.
func TestCallbackRejections_RLSAndAppendOnly(t *testing.T) {
	pool := testPool(t)
	fA := seedCasinoFixture(t, pool)
	fB := seedCasinoFixture(t, pool)
	rej := CallbackRejection{Class: RejectionBetNotFound, EventType: CallbackEventWin, ProviderTxID: "rls-win", RoundID: "r", AssetCode: "EUR", Amount: 5}
	if err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordCallbackRejection(ctx, tx, fA.tenantID, "mock-casino", rej, "")
		return err
	}); err != nil {
		t.Fatalf("record under A: %v", err)
	}

	// Tenant B sees nothing of A.
	var n int
	if err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections WHERE provider_tx_id = 'rls-win'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("tenant B must see zero of A's rows: n=%d err=%v", n, err)
	}
	// Tenant B cannot forge a row for A.
	err := pool.WithTenant(context.Background(), fB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordCallbackRejection(ctx, tx, fA.tenantID, "mock-casino", rej, "")
		return err
	})
	assertRLSViolation(t, err)
	// A player-scoped connection neither reads nor writes.
	if err := pool.WithPlayerScope(context.Background(), fA.tenantID, fA.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("player scope must see zero rows: n=%d err=%v", n, err)
	}
	err = pool.WithPlayerScope(context.Background(), fA.tenantID, fA.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordCallbackRejection(ctx, tx, fA.tenantID, "mock-casino",
			CallbackRejection{Class: RejectionBetNotFound, EventType: CallbackEventWin, ProviderTxID: "rls-win-2", Amount: 5}, "")
		return err
	})
	assertRLSViolation(t, err)

	// Append-only, layer 1 (RLS): no UPDATE/DELETE policy exists, so under
	// FORCE RLS even the table owner's tenant-scoped UPDATE/DELETE matches
	// zero rows. Layer 2 (the deny trigger, which binds even when a row IS
	// visible) is proven on a scratch database in internal/reconciliation's
	// TestMigration0097_DenyTriggerBindsEvenWithAPermissiveUpdatePolicy.
	for _, stmt := range []string{
		`UPDATE casino_callback_rejections SET reason_class = 'payload_mismatch' WHERE provider_tx_id = 'rls-win'`,
		`DELETE FROM casino_callback_rejections WHERE provider_tx_id = 'rls-win'`,
	} {
		var affected int64
		err := pool.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, stmt)
			affected = tag.RowsAffected()
			return err
		})
		if err == nil && affected != 0 {
			t.Fatalf("expected %q to affect no row, affected %d", stmt, affected)
		}
	}
	if got := loadRejections(t, pool, fA.tenantID); len(got) != 1 || got[0].reasonClass != "bet_not_found" {
		t.Fatalf("row must be unchanged, got %+v", got)
	}

	if url := os.Getenv("TEST_RUNTIME_DATABASE_URL"); url != "" {
		rt, err := db.Connect(context.Background(), url, 2, 5_000_000_000)
		if err != nil {
			t.Fatalf("connect as runtime role: %v", err)
		}
		defer rt.Close()
		err = rt.WithTenant(context.Background(), fA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE casino_callback_rejections SET reason_class = 'payload_mismatch' WHERE provider_tx_id = 'rls-win'`)
			return err
		})
		assertPgCode(t, err, "42501")
	}
}
