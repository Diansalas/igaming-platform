//go:build integration

// R3-GAME-POSTINGS-NONACTIVE-1 (owner decision 2026-10-05, ADR 0095 section
// 40.5): a verified casino bet, win or rollback that would create a NEW
// posting for a suspended or closed tenant is refused inside the posting
// transaction (ErrTenantNotActive): no ledger transaction or entry, no wallet
// projection change, a durable audit record (written the way the HTTP layer
// writes it, through RecordCallbackRejection). A replay of an already-posted
// callback is a read and returns the original outcome. The operations under
// test run on the RUNTIME role pool (asserted NOT rolsuper AND NOT
// rolbypassrls); fixtures are seeded through the owner pool.
package casino

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func runtimePoolForGate(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if url == "" {
		t.Fatal("TEST_RUNTIME_DATABASE_URL must be set: these tests must run as the runtime role")
	}
	rt, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect as runtime role: %v", err)
	}
	t.Cleanup(rt.Close)
	var super, bypass bool
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatalf("read runtime role flags: %v", err)
	}
	if super || bypass {
		t.Fatalf("runtime role must be NOT rolsuper and NOT rolbypassrls, got super=%v bypassrls=%v", super, bypass)
	}
	return rt
}

func setStatusForGate(t *testing.T, owner *db.Pool, tenantID uuid.UUID, status string) {
	t.Helper()
	err := owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET status = $1 WHERE id = $2`, status, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return errors.New("tenant row not updated")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("set tenant status %s: %v", status, err)
	}
}

func ledgerCountsForGate(t *testing.T, owner *db.Pool, tenantID uuid.UUID) (txs, entries int) {
	t.Helper()
	err := owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&txs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&entries)
	})
	if err != nil {
		t.Fatalf("ledger counts: %v", err)
	}
	return txs, entries
}

func rejectionAuditCount(t *testing.T, owner *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	return countAuditRows(t, owner, tenantID, AuditActionCallbackRejectedTenantNotActive)
}

type gateWorld struct {
	owner, rt *db.Pool
	f         casinoFixture
	provider  *MockCasinoProvider
	orch      *Orchestrator
	sessionID uuid.UUID
}

func newGateWorld(t *testing.T, owner, rt *db.Pool) gateWorld {
	t.Helper()
	f := seedCasinoFixture(t, owner)
	fundWallet(t, owner, f, 50_000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, owner, f, provider, 100)
	sessionID := mintSession(t, owner, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))
	return gateWorld{owner: owner, rt: rt, f: f, provider: provider, orch: orch, sessionID: sessionID}
}

// deliver runs one verified callback on the runtime pool, as the HTTP handler
// does (its own transaction, rolled back on error).
func (w gateWorld) deliver(t *testing.T, ev CallbackEventType, ref, original, round string, amount int64) (ReceiveCallbackResult, error) {
	t.Helper()
	sess := uuid.Nil
	if ev == CallbackEventBet {
		sess = w.sessionID
	}
	outcome := OutcomeSucceeded
	if ev == CallbackEventRollback {
		outcome = ""
	}
	payload := w.provider.CallbackPayload(w.f.tenantID, ev, ref, original, round, "game-1", amount, "EUR", outcome, "", w.f.playerAccountID, sess)
	var res ReceiveCallbackResult
	err := w.rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = w.orch.receiveCallbackInTx(ctx, tx, w.f.tenantID, "mock-casino", payload)
		return err
	})
	return res, err
}

// record writes the durable record exactly as recordCasinoCallbackRejection
// does for a *CallbackRejectedError (separate transaction, runtime role).
func (w gateWorld) record(t *testing.T, err error) {
	t.Helper()
	var rejected *CallbackRejectedError
	if !errors.As(err, &rejected) {
		t.Fatalf("a refusal must be a *CallbackRejectedError so the HTTP layer records it, got %T: %v", err, err)
	}
	if rejected.Rejection.Class != RejectionTenantNotActive {
		t.Fatalf("rejection class %q, want %q", rejected.Rejection.Class, RejectionTenantNotActive)
	}
	werr := w.rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := RecordCallbackRejection(ctx, tx, w.f.tenantID, rejected.ProviderID, rejected.Rejection, "req-gate-test")
		return err
	})
	if werr != nil {
		t.Fatalf("record rejection: %v", werr)
	}
}

func TestTenantNotActive_CasinoNewPostingsRefused(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			w := newGateWorld(t, owner, rt)
			// History while ACTIVE: an open bet (round-open), a settled round
			// (bet+win) and a rolled-back bet, so replays and open rounds
			// both exist at closure.
			for _, d := range []struct {
				ev                   CallbackEventType
				ref, original, round string
				amount               int64
			}{
				{CallbackEventBet, "b-open", "", "round-open", 1000},
				{CallbackEventBet, "b-settled", "", "round-settled", 1000},
				{CallbackEventWin, "w-settled", "", "round-settled", 3000},
				{CallbackEventBet, "b-rolled", "", "round-rolled", 500},
				{CallbackEventRollback, "rb-rolled", "b-rolled", "round-rolled", 0},
			} {
				if _, err := w.deliver(t, d.ev, d.ref, d.original, d.round, d.amount); err != nil {
					t.Fatalf("active delivery %s: %v", d.ref, err)
				}
			}
			setStatusForGate(t, owner, w.f.tenantID, status)
			txs0, entries0 := ledgerCountsForGate(t, owner, w.f.tenantID)
			cash0 := cashBalance(t, owner, w.f)

			refused := []struct {
				name                 string
				ev                   CallbackEventType
				ref, original, round string
				amount               int64
			}{
				{"new_bet", CallbackEventBet, "b-new", "", "round-new", 1000},
				{"win_for_open_round", CallbackEventWin, "w-open", "", "round-open", 2000},
				{"rollback_of_posted_bet", CallbackEventRollback, "rb-open", "b-open", "round-open", 0},
				{"rollback_of_posted_win", CallbackEventRollback, "rb-win", "w-settled", "round-settled", 0},
			}
			for _, c := range refused {
				_, err := w.deliver(t, c.ev, c.ref, c.original, c.round, c.amount)
				if !errors.Is(err, ErrTenantNotActive) {
					t.Fatalf("%s: expected ErrTenantNotActive, got %v", c.name, err)
				}
				w.record(t, err)
			}
			txs1, entries1 := ledgerCountsForGate(t, owner, w.f.tenantID)
			if txs1 != txs0 || entries1 != entries0 || cashBalance(t, owner, w.f) != cash0 {
				t.Fatalf("refused callbacks changed the ledger/wallet: tx %d->%d entries %d->%d", txs0, txs1, entries0, entries1)
			}
			if got := rejectionAuditCount(t, owner, w.f.tenantID); got != len(refused) {
				t.Fatalf("expected %d durable refusal audit records, got %d", len(refused), got)
			}
			// No casino_callback_rejections row is written for this class
			// (reconciliation's ruled partition is untouched).
			var rows int
			if err := owner.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections WHERE tenant_id = $1`, w.f.tenantID).Scan(&rows)
			}); err != nil || rows != 0 {
				t.Fatalf("no casino_callback_rejections row expected: n=%d err=%v", rows, err)
			}
			// No tombstone was written for the unseen original.
			var tombs int
			if err := owner.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'tombstone'`, w.f.tenantID).Scan(&tombs)
			}); err != nil || tombs != 0 {
				t.Fatalf("no tombstone may be written: n=%d err=%v", tombs, err)
			}

			// Replays of previously POSTED callbacks: a read, the original
			// outcome, no new movement, evidence intact.
			for _, r := range []struct {
				name                 string
				ev                   CallbackEventType
				ref, original, round string
				amount               int64
			}{
				{"bet_replay", CallbackEventBet, "b-settled", "", "round-settled", 1000},
				{"win_replay", CallbackEventWin, "w-settled", "", "round-settled", 3000},
				{"rollback_replay", CallbackEventRollback, "rb-rolled", "b-rolled", "round-rolled", 0},
			} {
				res, err := w.deliver(t, r.ev, r.ref, r.original, r.round, r.amount)
				if err != nil {
					t.Fatalf("%s on a %s tenant must return the original outcome, got %v", r.name, status, err)
				}
				if !res.Replayed || res.LedgerTransactionID == nil {
					t.Fatalf("%s: expected a replay carrying the original ledger transaction, got %+v", r.name, res)
				}
			}
			txs2, entries2 := ledgerCountsForGate(t, owner, w.f.tenantID)
			if txs2 != txs0 || entries2 != entries0 || cashBalance(t, owner, w.f) != cash0 {
				t.Fatalf("replays changed the ledger/wallet: tx %d->%d", txs0, txs2)
			}

			// Reactivation: an active tenant is unchanged.
			setStatusForGate(t, owner, w.f.tenantID, "active")
			if _, err := w.deliver(t, CallbackEventBet, "b-after", "", "round-after", 100); err != nil {
				t.Fatalf("active tenant must accept a bet: %v", err)
			}
			if _, err := w.deliver(t, CallbackEventWin, "w-open", "", "round-open", 2000); err != nil {
				t.Fatalf("active tenant must settle the open round: %v", err)
			}
		})
	}
}

// TestTenantNotActive_CasinoCrossTenantIsolation: closing tenant A does not
// affect tenant B, and a callback signed for B but sent to A's scope cannot
// post (it is refused or rejected, never posted into either tenant).
func TestTenantNotActive_CasinoCrossTenantIsolation(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	a := newGateWorld(t, owner, rt)
	b := newGateWorld(t, owner, rt)
	setStatusForGate(t, owner, a.f.tenantID, "closed")

	if _, err := a.deliver(t, CallbackEventBet, "iso-a", "", "round-iso-a", 100); !errors.Is(err, ErrTenantNotActive) {
		t.Fatalf("tenant A must be refused: %v", err)
	}
	if _, err := b.deliver(t, CallbackEventBet, "iso-b", "", "round-iso-b", 100); err != nil {
		t.Fatalf("tenant B must be unaffected: %v", err)
	}
	// B's payload (B tenant id and session) delivered into A's route.
	payload := b.provider.CallbackPayload(b.f.tenantID, CallbackEventBet, "iso-cross", "", "round-iso-cross", "game-1", 100, "EUR", OutcomeSucceeded, "", b.f.playerAccountID, b.sessionID)
	txsB0, _ := ledgerCountsForGate(t, owner, b.f.tenantID)
	err := rt.WithTenant(context.Background(), a.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := a.orch.receiveCallbackInTx(ctx, tx, a.f.tenantID, "mock-casino", payload)
		return err
	})
	if err == nil {
		t.Fatal("a callback for tenant B sent through tenant A's scope must not succeed")
	}
	if txsB1, _ := ledgerCountsForGate(t, owner, b.f.tenantID); txsB1 != txsB0 {
		t.Fatalf("cross-tenant delivery posted into tenant B: %d -> %d", txsB0, txsB1)
	}
}

// TestTenantNotActive_CasinoDatabaseBackstop: even a code path that skipped
// the application check cannot post a gameplay ledger type for a closed
// tenant (migration 0118 trigger, SQLSTATE GP010).
func TestTenantNotActive_CasinoDatabaseBackstop(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	w := newGateWorld(t, owner, rt)
	setStatusForGate(t, owner, w.f.tenantID, "suspended")
	txs0, _ := ledgerCountsForGate(t, owner, w.f.tenantID)
	for _, tt := range []string{"casino_bet", "casino_win", "casino_rollback"} {
		err := rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, correlation_id)
				 VALUES ($1, $2, $3, $4)`, w.f.tenantID, tt, "raw-"+tt, uuid.New())
			return err
		})
		assertPgCode(t, err, "GP010")
	}
	if txs1, _ := ledgerCountsForGate(t, owner, w.f.tenantID); txs1 != txs0 {
		t.Fatalf("backstop let a row through: %d -> %d", txs0, txs1)
	}
}

// TestTenantNotActive_CasinoStatusChangeVsPostingRace proves no check-then-act
// window, in either order, using two real connections.
func TestTenantNotActive_CasinoStatusChangeVsPostingRace(t *testing.T) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)

	t.Run("status_change_first_then_bet_refused", func(t *testing.T) {
		w := newGateWorld(t, owner, rt)
		updated, release, closerDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var releaseOnceGuard sync.Once
		releaseOnce := func() { releaseOnceGuard.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce) // never leave a held transaction behind on a failed assertion
		go func() {
			closerDone <- owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `UPDATE tenants SET status = 'closed' WHERE id = $1`, w.f.tenantID); err != nil {
					return err
				}
				close(updated)
				<-release
				return nil
			})
		}()
		<-updated
		type out struct {
			res ReceiveCallbackResult
			err error
		}
		posted := make(chan out, 1)
		go func() {
			r, err := w.deliver(t, CallbackEventBet, "race-1", "", "round-race-1", 100)
			posted <- out{r, err}
		}()
		select {
		case o := <-posted:
			t.Fatalf("the bet must wait for the in-flight status change, finished early: %+v err=%v", o.res, o.err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-closerDone; err != nil {
			t.Fatalf("status change: %v", err)
		}
		if o := <-posted; !errors.Is(o.err, ErrTenantNotActive) {
			t.Fatalf("the bet must see the committed closure and be refused: %+v err=%v", o.res, o.err)
		}
	})

	t.Run("bet_first_then_status_change_waits", func(t *testing.T) {
		w := newGateWorld(t, owner, rt)
		inTx, release := make(chan struct{}), make(chan struct{})
		var releaseOnceGuard sync.Once
		releaseOnce := func() { releaseOnceGuard.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce) // never leave a held transaction behind on a failed assertion
		payload := w.provider.CallbackPayload(w.f.tenantID, CallbackEventBet, "race-2", "", "round-race-2", "game-1", 100, "EUR", OutcomeSucceeded, "", w.f.playerAccountID, w.sessionID)
		posted := make(chan error, 1)
		go func() {
			posted <- rt.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := w.orch.receiveCallbackInTx(ctx, tx, w.f.tenantID, "mock-casino", payload); err != nil {
					return err
				}
				close(inTx)
				<-release // hold the posting transaction open after the check and the post
				return nil
			})
		}()
		<-inTx
		closed := make(chan error, 1)
		go func() {
			closed <- owner.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'closed' WHERE id = $1`, w.f.tenantID)
				return err
			})
		}()
		select {
		case err := <-closed:
			t.Fatalf("the status change must wait for the in-flight posting, finished early: %v", err)
		case <-time.After(500 * time.Millisecond):
		}
		releaseOnce()
		if err := <-posted; err != nil {
			t.Fatalf("posting: %v", err)
		}
		if err := <-closed; err != nil {
			t.Fatalf("status change: %v", err)
		}
		if _, err := w.deliver(t, CallbackEventWin, "race-2-win", "", "round-race-2", 100); !errors.Is(err, ErrTenantNotActive) {
			t.Fatalf("after the closure the next posting is refused: %v", err)
		}
	})
}

// TestTenantNotActive_CasinoTombstonesAlwaysWrittenAndLateOriginalRecorded
// (ledger-finance C1 and C3): a rollback of a never-seen original writes its
// tombstone on a suspended tenant (no money, always written) and is
// idempotent; a late original bet is then declined from the E3 check BEFORE
// the non-active gate, so casino_callback_rejections records it as
// original_tombstoned (the C7 cas_tombstone_late_original input), also after
// reactivation; a late original win is refused as tombstoned.
func TestTenantNotActive_CasinoTombstonesAlwaysWrittenAndLateOriginalRecorded(t *testing.T) {
	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) { casinoTombstonesAlwaysWritten(t, status) })
	}
}

func casinoTombstonesAlwaysWritten(t *testing.T, status string) {
	owner := testPool(t)
	rt := runtimePoolForGate(t)
	w := newGateWorld(t, owner, rt)
	setStatusForGate(t, owner, w.f.tenantID, status)
	txs0, entries0 := ledgerCountsForGate(t, owner, w.f.tenantID)

	res, err := w.deliver(t, CallbackEventRollback, "rb-late", "b-late", "round-late", 0)
	if err != nil || !res.Tombstoned {
		t.Fatalf("a tombstone must be written on a suspended tenant: %+v err=%v", res, err)
	}
	txs1, entries1 := ledgerCountsForGate(t, owner, w.f.tenantID)
	if txs1 != txs0+1 || entries1 != entries0 {
		t.Fatalf("expected one tombstone and no entries: tx %d->%d entries %d->%d", txs0, txs1, entries0, entries1)
	}
	if again, err := w.deliver(t, CallbackEventRollback, "rb-late", "b-late", "round-late", 0); err != nil || !again.Tombstoned {
		t.Fatalf("tombstone replay must be idempotent: %+v err=%v", again, err)
	}
	if txs2, _ := ledgerCountsForGate(t, owner, w.f.tenantID); txs2 != txs1 {
		t.Fatalf("replay wrote a row: %d -> %d", txs1, txs2)
	}

	lateRows := func() int {
		var n int
		if err := owner.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections WHERE tenant_id = $1 AND reason_class = 'original_tombstoned' AND provider_tx_id = 'b-late'`, w.f.tenantID).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for _, phase := range []string{"suspended", "reactivated"} {
		if phase == "reactivated" {
			setStatusForGate(t, owner, w.f.tenantID, "active")
		}
		bet, err := w.deliver(t, CallbackEventBet, "b-late", "", "round-late", 100)
		if err != nil || bet.Outcome != OutcomeDeclined || bet.DeclineReason != "original_rolled_back" {
			t.Fatalf("%s: late original bet must be declined as rolled back: %+v err=%v", phase, bet, err)
		}
		if lateRows() != 1 {
			t.Fatalf("%s: the late original must be recorded once as original_tombstoned", phase)
		}
		if _, err := w.deliver(t, CallbackEventWin, "b-late", "", "round-late", 100); !errors.Is(err, ErrOriginalTombstoned) {
			t.Fatalf("%s: late original win must be refused as tombstoned, got %v", phase, err)
		}
	}
}
