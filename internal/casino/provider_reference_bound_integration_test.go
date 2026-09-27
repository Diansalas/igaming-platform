//go:build integration

// PROVIDER-REF-BOUND-1 (PRH-REF): the platform provider-reference bound at
// the casino verified-callback boundary. An over-bound reference is a
// deterministic ErrProviderReferenceInvalid with NOTHING written (no
// ledger transaction, tombstone, round binding, audit or rejection row);
// an exact-max (255-byte) reference is accepted and idempotent on
// redelivery.
package casino

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerref"
)

type casinoRowCounts struct {
	ledgerTx, rounds, rejections, audit int
}

func countCasinoRows(t *testing.T, pool *db.Pool, f casinoFixture) casinoRowCounts {
	t.Helper()
	var c casinoRowCounts
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&c.ledgerTx); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_provider_rounds WHERE tenant_id = $1`, f.tenantID).Scan(&c.rounds); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections WHERE tenant_id = $1`, f.tenantID).Scan(&c.rejections); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, f.tenantID).Scan(&c.audit)
	})
	if err != nil {
		t.Fatalf("count casino rows: %v", err)
	}
	return c
}

func assertRefRejected(t *testing.T, err error, wantField string, wantReason providerref.Reason) {
	t.Helper()
	if !errors.Is(err, ErrProviderReferenceInvalid) || !errors.Is(err, providerref.ErrInvalid) {
		t.Fatalf("expected ErrProviderReferenceInvalid wrapping providerref.ErrInvalid, got %v", err)
	}
	refErr, ok := providerref.AsError(err)
	if !ok || refErr.Field != wantField || refErr.Reason != wantReason {
		t.Fatalf("expected %s/%s, got %+v", wantField, wantReason, refErr)
	}
	// Not a recorded casino_callback_rejections class (log-only evidence).
	if class, recorded := rejectionClassFor(err); recorded {
		t.Fatalf("an over-bound reference must not map to a rejection class, got %q", class)
	}
	var rej *CallbackRejectedError
	if errors.As(err, &rej) {
		t.Fatalf("an over-bound reference must not be wrapped as a CallbackRejectedError")
	}
}

func TestProviderRefBound_OversizeReferencesRejectedWithNothingWritten(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	// A real bet to roll back / win against, so the win and rollback cases
	// would otherwise succeed and write.
	okBet := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-ref-ok", "", "round-ref-ok", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	if _, err := doReceiveCallback(t, pool, f.tenantID, orch, okBet); err != nil {
		t.Fatalf("seed bet: %v", err)
	}

	over := strings.Repeat("x", providerref.MaxBytes+1)
	overMultibyte := strings.Repeat("é", 128) // 256 bytes, 128 runes
	type tc struct {
		name, field string
		reason      providerref.Reason
		build       func() (CallbackEventType, string, string, string, string, string, uuid.UUID)
	}
	tcs := []tc{
		{"bet provider_tx_id too long", "provider_tx_id", providerref.ReasonTooLong, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventBet, over, "", "round-new", "game-1", "EUR", sessionID
		}},
		{"bet provider_tx_id multibyte too long", "provider_tx_id", providerref.ReasonTooLong, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventBet, overMultibyte, "", "round-new", "game-1", "EUR", sessionID
		}},
		{"bet round_id too long", "round_id", providerref.ReasonTooLong, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventBet, "bet-new", "", over, "game-1", "EUR", sessionID
		}},
		{"bet control char", "provider_tx_id", providerref.ReasonControlChar, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventBet, "bet\nnew", "", "round-new", "game-1", "EUR", sessionID
		}},
		{"win provider_tx_id too long", "provider_tx_id", providerref.ReasonTooLong, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventWin, over, "", "round-ref-ok", "game-1", "EUR", uuid.Nil
		}},
		{"win game id too long", "provider_game_id", providerref.ReasonTooLong, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventWin, "win-new", "", "round-ref-ok", over, "EUR", uuid.Nil
		}},
		{"win asset code too long", "asset_code", providerref.ReasonTooLong, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventWin, "win-new", "", "round-ref-ok", "game-1", over, uuid.Nil
		}},
		{"rollback original too long (would tombstone)", "original_provider_tx_id", providerref.ReasonTooLong, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventRollback, "rb-new", over, "round-ref-ok", "game-1", "EUR", uuid.Nil
		}},
		{"rollback own ref too long", "provider_tx_id", providerref.ReasonTooLong, func() (CallbackEventType, string, string, string, string, string, uuid.UUID) {
			return CallbackEventRollback, over, "bet-ref-ok", "round-ref-ok", "game-1", "EUR", uuid.Nil
		}},
	}

	for _, c := range tcs {
		t.Run(c.name, func(t *testing.T) {
			before := countCasinoRows(t, pool, f)
			balanceBefore := cashBalance(t, pool, f)
			ev, txID, orig, round, game, asset, sess := c.build()
			var amount int64 = 500
			outcome := OutcomeSucceeded
			if ev == CallbackEventRollback {
				amount, outcome = 0, ""
			}
			payload := provider.CallbackPayload(f.tenantID, ev, txID, orig, round, game, amount, asset, outcome, "", f.playerAccountID, sess)
			// Deterministic: the same rejection on every redelivery.
			for i := 0; i < 2; i++ {
				_, err := doReceiveCallback(t, pool, f.tenantID, orch, payload)
				assertRefRejected(t, err, c.field, c.reason)
			}
			if after := countCasinoRows(t, pool, f); after != before {
				t.Fatalf("rows written by a rejected callback: before %+v after %+v", before, after)
			}
			if got := cashBalance(t, pool, f); got != balanceBefore {
				t.Fatalf("balance changed: %d -> %d", balanceBefore, got)
			}
			if d, cr := sumDebitsCredits(t, pool, f.tenantID); d != cr {
				t.Fatalf("ledger unbalanced: debits=%d credits=%d", d, cr)
			}
		})
	}
}

// TestProviderRefBound_ExactMaxAcceptedAndIdempotent: a 255-byte
// provider_tx_id / round id (ASCII and multibyte) posts once and a
// redelivery is a replay, not a second posting.
func TestProviderRefBound_ExactMaxAcceptedAndIdempotent(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	betRef := strings.Repeat("b", providerref.MaxBytes)
	roundRef := strings.Repeat("é", 127) + "r" // 255 bytes
	if len(betRef) != 255 || len(roundRef) != 255 {
		t.Fatalf("fixture lengths %d/%d", len(betRef), len(roundRef))
	}
	bet := provider.CallbackPayload(f.tenantID, CallbackEventBet, betRef, "", roundRef, "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	first, err := doReceiveCallback(t, pool, f.tenantID, orch, bet)
	if err != nil || first.Outcome != OutcomeSucceeded || first.Replayed {
		t.Fatalf("exact-max bet must post: %+v %v", first, err)
	}
	afterFirst := countCasinoRows(t, pool, f)
	second, err := doReceiveCallback(t, pool, f.tenantID, orch, bet)
	if err != nil || !second.Replayed {
		t.Fatalf("redelivered exact-max bet must be a replay: %+v %v", second, err)
	}
	if after := countCasinoRows(t, pool, f); after.ledgerTx != afterFirst.ledgerTx || after.rounds != afterFirst.rounds {
		t.Fatalf("redelivery wrote rows: %+v -> %+v", afterFirst, after)
	}

	winRef := strings.Repeat("w", 252) + "€" // 255 bytes
	win := provider.CallbackPayload(f.tenantID, CallbackEventWin, winRef, "", roundRef, "game-1", 3000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.Nil)
	for i := 0; i < 2; i++ {
		if _, err := doReceiveCallback(t, pool, f.tenantID, orch, win); err != nil {
			t.Fatalf("exact-max win delivery %d: %v", i, err)
		}
	}
	rbRef := strings.Repeat("r", providerref.MaxBytes)
	rb := provider.CallbackPayload(f.tenantID, CallbackEventRollback, rbRef, winRef, roundRef, "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	for i := 0; i < 2; i++ {
		if _, err := doReceiveCallback(t, pool, f.tenantID, orch, rb); err != nil {
			t.Fatalf("exact-max rollback delivery %d: %v", i, err)
		}
	}
	if got := cashBalance(t, pool, f); got != 4000 {
		t.Fatalf("balance after bet 1000, win 3000 (rolled back): want 4000, got %d", got)
	}
	if d, cr := sumDebitsCredits(t, pool, f.tenantID); d != cr {
		t.Fatalf("ledger unbalanced: debits=%d credits=%d", d, cr)
	}
	// The stored references are byte-identical (never truncated).
	var stored []string
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT provider_tx_id FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id IS NOT NULL ORDER BY posted_at, provider_tx_id`, f.tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			stored = append(stored, s)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read refs: %v", err)
	}
	want := map[string]bool{betRef: false, winRef: false, rbRef: false}
	for _, s := range stored {
		if _, ok := want[s]; ok {
			want[s] = true
		}
	}
	for ref, seen := range want {
		if !seen {
			t.Fatalf("reference of %d bytes not stored verbatim (stored: %d refs)", len(ref), len(stored))
		}
	}
}

// TestProviderRefBound_TombstoneForExactMaxOriginal: a rollback naming a
// never-seen 255-byte original writes its tombstone, and the late
// original is then rejected (the CLAUDE.md late-arrival guarantee holds at
// the boundary length).
func TestProviderRefBound_TombstoneForExactMaxOriginal(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	orig := strings.Repeat("o", providerref.MaxBytes)
	rb := provider.CallbackPayload(f.tenantID, CallbackEventRollback, "rb-tomb", orig, "round-tomb", "game-1", 0, "EUR", "", "", f.playerAccountID, uuid.Nil)
	res, err := doReceiveCallback(t, pool, f.tenantID, orch, rb)
	if err != nil || !res.Tombstoned {
		t.Fatalf("expected a tombstone for a never-seen exact-max original: %+v %v", res, err)
	}
	late := provider.CallbackPayload(f.tenantID, CallbackEventBet, orig, "", "round-tomb", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	// E3: a late bet after its tombstone is acknowledged as declined, with
	// its rejection row recorded (the exact-max reference fits the
	// bounded casino_callback_rejections columns).
	lateRes, err := doReceiveCallback(t, pool, f.tenantID, orch, late)
	if err != nil || lateRes.Outcome != OutcomeDeclined {
		t.Fatalf("late exact-max original must be declined (E3), got %+v %v", lateRes, err)
	}
	var rejections int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections WHERE provider_tx_id = $1 AND reason_class = 'original_tombstoned'`, orig).Scan(&rejections)
	}); err != nil || rejections != 1 {
		t.Fatalf("expected one original_tombstoned rejection row for the exact-max reference, got %d (%v)", rejections, err)
	}
	if got := cashBalance(t, pool, f); got != 5000 {
		t.Fatalf("balance must be untouched, got %d", got)
	}
}
