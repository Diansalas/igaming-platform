//go:build integration

// A3 security fix round (SR3-1, SR3-2, SR3-4): the opaque player reference is a
// second binding factor and is enforced fail-closed everywhere it can appear -
// bet, bet replay, win, win replay and rollback. A valid reference of ANOTHER
// player is refused (ErrPlayerRefMismatch, same 400 as an unresolvable value),
// audited (casino_callback.player_ref_mismatch, written by the separately
// committed rejection record) and posts nothing. A rollback carrying a
// reference is refused (nothing written) only on the path that would reverse an
// already POSTED bet/win; for a never-seen or already tombstoned original the
// tombstone is still written (ledger-finance LF-A3-3) and the bad reference is
// kept as an audit-only evidence row. A rollback with no reference stays allowed.
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// a3RecordRejection plays the HTTP layer's part: the domain transaction rolled
// back with a CallbackRejectedError, so the rejection record is written in its
// own committed transaction.
func a3RecordRejection(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerID string, err error) {
	t.Helper()
	var rej *CallbackRejectedError
	if !errors.As(err, &rej) {
		t.Fatalf("expected a *CallbackRejectedError to record, got %T: %v", err, err)
	}
	if werr := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := RecordCallbackRejection(ctx, tx, tenantID, rej.ProviderID, rej.Rejection, "req-a3")
		return e
	}); werr != nil {
		t.Fatalf("record rejection: %v", werr)
	}
}

func a3AuditCount(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenantID, action).Scan(&n)
	}); err != nil {
		t.Fatalf("count audit: %v", err)
	}
	return n
}

// receiveRef is a3Env.receive with an explicit player reference.
func (e *a3Env) receiveRef(t *testing.T, eventType CallbackEventType, txID, origTxID, round string, amount int64, ref, sessionID uuid.UUID) (ReceiveCallbackResult, error) {
	t.Helper()
	outcome := OutcomeSucceeded
	if eventType == CallbackEventRollback {
		outcome = ""
	}
	payload := e.provider.CallbackPayload(e.f.tenantID, eventType, txID, origTxID, round, "game-1", amount, "EUR", outcome, "", ref, sessionID)
	var res ReceiveCallbackResult
	err := e.pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = e.orch.receiveCallbackInTx(ctx, tx, e.f.tenantID, "mock-casino", payload)
		return err
	})
	return res, err
}

func TestA3_RejectionClassForPlayerRefMismatch(t *testing.T) {
	if c, ok := rejectionClassFor(ErrPlayerRefMismatch); !ok || c != RejectionPlayerRefMismatch {
		t.Fatalf("ErrPlayerRefMismatch must map to RejectionPlayerRefMismatch, got %q %v", c, ok)
	}
	// An unresolvable reference is a structural input error, not a recorded rejection.
	if _, ok := rejectionClassFor(ErrPlayerRefUnresolved); ok {
		t.Fatal("ErrPlayerRefUnresolved must not be a recorded rejection class")
	}
}

// SR3-1: bet and win replays with another player's valid reference are refused
// as payload mismatches (409) and move nothing.
func TestA3_Replays_ForeignPlayerRefIsAPayloadMismatch(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	otherID, _ := seedSecondPlayerWallet(t, e.pool, e.f)
	otherRef := playerRefFor(t, e.pool, e.f.tenantID, "mock-casino", otherID)

	if _, err := e.receive(t, CallbackEventBet, "sr3-bet", "", "sr3-round", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	if _, err := e.receive(t, CallbackEventWin, "sr3-win", "", "sr3-round", 1500, uuid.Nil); err != nil {
		t.Fatalf("win: %v", err)
	}
	before := cashBalance(t, e.pool, e.f)

	if _, err := e.receiveRef(t, CallbackEventBet, "sr3-bet", "", "sr3-round", 1000, otherRef, e.sessionID); !errors.Is(err, ErrProviderTxPayloadMismatch) {
		t.Fatalf("a bet replay naming another player must be ErrProviderTxPayloadMismatch, got %v", err)
	}
	if _, err := e.receiveRef(t, CallbackEventWin, "sr3-win", "", "sr3-round", 1500, otherRef, uuid.Nil); !errors.Is(err, ErrProviderTxPayloadMismatch) {
		t.Fatalf("a win replay naming another player must be ErrProviderTxPayloadMismatch, got %v", err)
	}
	// The true owner's reference still replays cleanly.
	if r, err := e.receive(t, CallbackEventBet, "sr3-bet", "", "sr3-round", 1000, e.sessionID); err != nil || !r.Replayed {
		t.Fatalf("own-reference bet replay: %+v %v", r, err)
	}
	if r, err := e.receive(t, CallbackEventWin, "sr3-win", "", "sr3-round", 1500, uuid.Nil); err != nil || !r.Replayed {
		t.Fatalf("own-reference win replay: %+v %v", r, err)
	}
	e.requireBalance(t, before)
}

// SR3-1 for a win: the mismatch is refused against the bettor the round's own
// bet determines, on locked-funding rounds too (exercised via the cash path
// here; the comparison is on the resolved wallet's owner).
func TestA3_Win_ForeignPlayerRefIsRefusedAndAudited(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	otherID, otherWallet := seedSecondPlayerWallet(t, e.pool, e.f)
	otherRef := playerRefFor(t, e.pool, e.f.tenantID, "mock-casino", otherID)
	if _, err := e.receive(t, CallbackEventBet, "sr3w-bet", "", "sr3w-round", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	_, err := e.receiveRef(t, CallbackEventWin, "sr3w-win", "", "sr3w-round", 1500, otherRef, uuid.Nil)
	if !errors.Is(err, ErrPlayerRefMismatch) {
		t.Fatalf("expected ErrPlayerRefMismatch, got %v", err)
	}
	a3RecordRejection(t, e.pool, e.f.tenantID, "mock-casino", err)
	if n := a3AuditCount(t, e.pool, e.f.tenantID, AuditActionCallbackPlayerRefMismatch); n != 1 {
		t.Fatalf("expected one mismatch audit row, got %d", n)
	}
	if c := e.countTx(t, "sr3w-win"); c != 0 {
		t.Fatalf("nothing may post, got %d", c)
	}
	if b := fmWalletCashBalance(t, e.pool, e.f.tenantID, otherWallet); b != 0 {
		t.Fatalf("the named player must be untouched, got %d", b)
	}
	e.requireBalance(t, 9000)
}

// SR3-2: the ordinary rollback path resolves a PRESENT player_ref.
func TestA3_Rollback_PlayerRefIsResolvedAndMatched(t *testing.T) {
	e := a3SetupConsumed(t, 10000)
	otherID, _ := seedSecondPlayerWallet(t, e.pool, e.f)
	otherRef := playerRefFor(t, e.pool, e.f.tenantID, "mock-casino", otherID)
	foreignTenant := seedCasinoFixture(t, e.pool)
	sameTenantOtherProvider := playerRefFor(t, e.pool, e.f.tenantID, "mock-other", e.f.playerAccountID)

	if _, err := e.receive(t, CallbackEventBet, "sr3r-bet", "", "sr3r-round", 1000, e.sessionID); err != nil {
		t.Fatalf("bet: %v", err)
	}
	e.requireBalance(t, 9000)

	for name, ref := range map[string]uuid.UUID{
		"raw account id":               e.f.playerAccountID,
		"another tenant's reference":   foreignTenant.playerRef,
		"another provider's reference": sameTenantOtherProvider,
		"a random reference":           uuid.New(),
	} {
		_, err := e.receiveRef(t, CallbackEventRollback, "sr3r-rb-"+name, "sr3r-bet", "sr3r-round", 0, ref, uuid.Nil)
		if !errors.Is(err, ErrPlayerRefUnresolved) || errors.Is(err, ErrPlayerRefMismatch) {
			t.Fatalf("%s: expected a plain ErrPlayerRefUnresolved, got %v", name, err)
		}
		e.requireBalance(t, 9000)
		if c := e.countTx(t, "sr3r-rb-"+name); c != 0 {
			t.Fatalf("%s: no reversal may post, got %d", name, c)
		}
	}

	// LF-A3-3 (replaces the earlier "refused, NO tombstone" expectation): an
	// unresolvable reference on a rollback whose original was NEVER seen still
	// writes the tombstone (a late original must be rejected) and keeps the bad
	// reference as an audit-only record; only the posted-original path refuses.
	for i, ref := range []uuid.UUID{uuid.New(), e.f.playerAccountID} {
		r, err := e.receiveRef(t, CallbackEventRollback, "sr3r-rb-ghost", "sr3r-ghost-original", "sr3r-ghost-round", 0, ref, uuid.Nil)
		if err != nil || !r.Tombstoned {
			t.Fatalf("an unresolvable reference on an unseen original must still tombstone: %+v %v", r, err)
		}
		if n := a3AuditCount(t, e.pool, e.f.tenantID, AuditActionCallbackPlayerRefMismatch); n != i+1 {
			t.Fatalf("expected %d evidence rows after delivery %d, got %d", i+1, i+1, n)
		}
	}
	if c := e.countTx(t, "sr3r-ghost-original"); c != 1 {
		t.Fatalf("the tombstone must exist exactly once under the original's reference, found %d", c)
	}
	// The late original is then rejected (that is what the tombstone is for).
	if _, err := e.receive(t, CallbackEventBet, "sr3r-ghost-original", "", "sr3r-ghost-round", 100, e.sessionID); err != nil {
		var rej *CallbackRejectedError
		if !errors.As(err, &rej) && !errors.Is(err, ErrOriginalTombstoned) {
			t.Fatalf("unexpected error for a late original: %v", err)
		}
	}
	if c := e.countTx(t, "sr3r-ghost-original"); c != 1 {
		t.Fatalf("a late original must not post over the tombstone, found %d rows", c)
	}
	e.requireBalance(t, 9000)
	base := a3AuditCount(t, e.pool, e.f.tenantID, AuditActionCallbackPlayerRefMismatch)

	// A valid reference of ANOTHER player than the original's: mismatch, audited, nothing posted.
	_, err := e.receiveRef(t, CallbackEventRollback, "sr3r-rb-other", "sr3r-bet", "sr3r-round", 0, otherRef, uuid.Nil)
	if !errors.Is(err, ErrPlayerRefMismatch) {
		t.Fatalf("expected ErrPlayerRefMismatch, got %v", err)
	}
	a3RecordRejection(t, e.pool, e.f.tenantID, "mock-casino", err)
	if n := a3AuditCount(t, e.pool, e.f.tenantID, AuditActionCallbackPlayerRefMismatch); n != base+1 {
		t.Fatalf("expected one more mismatch audit row (%d), got %d", base+1, n)
	}
	e.requireBalance(t, 9000)

	// A valid reference of a player for a never-seen original is fine (resolvability only): tombstone.
	if r, err := e.receiveRef(t, CallbackEventRollback, "sr3r-rb-tomb", "sr3r-unseen", "sr3r-round2", 0, otherRef, uuid.Nil); err != nil || !r.Tombstoned {
		t.Fatalf("a resolvable reference with an unseen original must tombstone: %+v %v", r, err)
	}
	// A missing reference stays allowed: a rollback must always be possible.
	if r, err := e.receiveRef(t, CallbackEventRollback, "sr3r-rb-noref", "sr3r-bet", "sr3r-round", 0, uuid.Nil, uuid.Nil); err != nil || r.Outcome != OutcomeSucceeded {
		t.Fatalf("a rollback with no reference must still post: %+v %v", r, err)
	}
	e.requireBalance(t, 10000)
}

// SR3-4: the mock-play consume CAS carries a positive provider predicate and
// needs the mock provider value, so a session of any other provider can never
// be consumed through it.
func TestA3_MockPlayConsume_IsBoundToTheMockProvider(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 1000)
	mock := NewMockCasinoProvider("mock-casino", "EUR")
	sessionID := mintActiveSession(t, pool, f, "mock-casino", "EUR")
	status := func() LaunchSessionStatus {
		var s LaunchSession
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			s, err = GetLaunchSessionByID(ctx, tx, sessionID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return s.Status
	}
	consume := func(m *MockCasinoProvider) error {
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return ConsumeLaunchSessionForMockPlay(ctx, tx, sessionID, m)
		})
	}
	if err := consume(nil); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("no mock provider value must be refused, got %v", err)
	}
	// SR3B-1 repro: a mock built for ANOTHER provider id must not consume this
	// mock-casino session, and the mock must not consume a real vendor's session.
	if err := consume(NewMockCasinoProvider("some-other-provider", "EUR")); !errors.Is(err, ErrLaunchSessionNotActive) {
		t.Fatalf("a mock of a different provider id must not consume this session, got %v", err)
	}
	if got := status(); got != LaunchSessionActive {
		t.Fatalf("the refused consume must leave the session active, got %s", got)
	}
	vendorSession := mintActiveSession(t, pool, f, "real-vendor-x", "EUR")
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ConsumeLaunchSessionForMockPlay(ctx, tx, vendorSession, mock)
	}); !errors.Is(err, ErrLaunchSessionNotActive) {
		t.Fatalf("the mock-casino mock must not consume a 'real-vendor-x' session, got %v", err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := GetLaunchSessionByID(ctx, tx, vendorSession)
		if err == nil && s.Status != LaunchSessionActive {
			t.Fatalf("the real vendor's session must stay active, got %s", s.Status)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := consume(mock); err != nil {
		t.Fatalf("the mock provider's own session must be consumable: %v", err)
	}
	if got := status(); got != LaunchSessionConsumed {
		t.Fatalf("expected consumed, got %s", got)
	}
}
