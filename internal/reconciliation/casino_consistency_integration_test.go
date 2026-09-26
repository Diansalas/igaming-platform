//go:build integration

// Stage 10.3 W2b (CAS-RECON-1): the casino_consistency reconciliation
// stream. A clean world built only through the real casino callback path
// (bets, wins, rollbacks, a tombstone, a multi-bet cash round, an open
// round) reconciles to zero mismatches; for every check C1-C7 a real
// divergence is seeded (a bypassing ledger.Post writer, a direct round
// write, or a rejection-record row - exactly the writers the stream exists
// to catch) and detected as exactly one row of the right kind and key.
// C5, which the unique index makes structurally impossible, is proven on a
// scratch database with that index dropped (paper 02 §2.15, the ADR 0088
// §8.4 precedent). Also: evidence-type dedup, state-type re-detection, the
// advisory lock, live-traffic snapshot safety, cross-tenant isolation, the
// sweep wiring (audit + metrics + P1 log), failure auditing, and the
// no-effect checklist.
package reconciliation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

const casProvider = "mock-casino"

type casWorld struct {
	pool      *db.Pool
	f         fixture
	mock      *casino.MockCasinoProvider
	orch      *casino.Orchestrator
	game      casino.Game
	sessionID uuid.UUID
}

func casSeedPlatformAdmin(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id, personID := uuid.New(), uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status) VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`,
			id, "recon-cas-admin-"+id.String()+"@test.example", personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed platform admin: %v", err)
	}
	return id
}

func newCasWorld(t *testing.T, pool *db.Pool) *casWorld {
	t.Helper()
	w := &casWorld{pool: pool, f: seedFixture(t, pool)}
	sbFund(t, pool, w.f, 1_000_000)
	w.mock = casino.NewMockCasinoProvider(casProvider, "EUR")
	w.orch = casino.NewOrchestrator(map[string]casino.CasinoProvider{casProvider: w.mock}, casino.NewMockWebhookCredentials(w.mock))
	err := pool.WithPlatformAdmin(context.Background(), casSeedPlatformAdmin(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		w.game, err = casino.UpsertGame(ctx, tx, casino.UpsertGameInput{
			ProviderID: casProvider, ProviderGameID: "recon-game-" + uuid.NewString()[:8],
			Name: "Recon Game", GameType: "slot", SupportedAssets: []string{"EUR"}, Status: casino.GameStatusActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed game: %v", err)
	}
	declared := w.mock.Capabilities()
	w.sessionID = w.mintSession(t, w.f.playerAccountID, w.f.walletID)
	err = pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := casino.WriteCapability(ctx, tx, w.mock, w.f.tenantID, nil, casino.CapabilityConfig{
			SupportsCatalogue: declared.SupportsCatalogue, SupportsLaunch: declared.SupportsLaunch, SupportsBalance: declared.SupportsBalance,
			SupportsBet: declared.SupportsBet, SupportsWin: declared.SupportsWin, SupportsRollback: declared.SupportsRollback,
			SupportedAssets: declared.SupportedAssets, SupportedGameTypes: declared.SupportedGameTypes,
			Priority: 100, Status: casino.CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("write capability: %v", err)
	}
	return w
}

func (w *casWorld) mintSession(t *testing.T, playerID, walletID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, _, err := casino.CreateLaunchSession(ctx, tx, casino.CreateLaunchSessionParams{
			TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: playerID, WalletID: walletID,
			GameID: w.game.ID, ProviderID: casProvider, ProviderGameID: w.game.ProviderGameID,
			AssetCode: "EUR", Mode: casino.ModeReal,
		})
		id = s.ID
		return err
	})
	if err != nil {
		t.Fatalf("mint session: %v", err)
	}
	return id
}

// deliver sends one signed MOCK callback through the real
// Orchestrator.ReceiveCallback path; a verified rejection is recorded in a
// separate transaction exactly as the HTTP layer does.
func (w *casWorld) deliver(t *testing.T, ev casino.CallbackEventType, ref, original, round string, amount int64) (casino.ReceiveCallbackResult, error) {
	t.Helper()
	outcome := casino.OutcomeSucceeded
	if ev == casino.CallbackEventRollback {
		outcome = ""
	}
	payload := w.mock.CallbackPayload(w.f.tenantID, ev, ref, original, round, w.game.ProviderGameID, amount, "EUR", outcome, "", w.f.playerAccountID, w.sessionID)
	var res casino.ReceiveCallbackResult
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = w.orch.ReceiveCallback(ctx, tx, w.f.tenantID, casProvider, payload)
		return err
	})
	var rej *casino.CallbackRejectedError
	if asRejected(err, &rej) {
		if werr := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := casino.RecordCallbackRejection(ctx, tx, w.f.tenantID, rej.ProviderID, rej.Rejection, "")
			return err
		}); werr != nil {
			t.Fatalf("record rejection: %v", werr)
		}
	}
	return res, err
}

func asRejected(err error, target **casino.CallbackRejectedError) bool {
	for e := err; e != nil; {
		if r, ok := e.(*casino.CallbackRejectedError); ok {
			*target = r
			return true
		}
		u, ok := e.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		e = u.Unwrap()
	}
	return false
}

func (w *casWorld) mustDeliver(t *testing.T, ev casino.CallbackEventType, ref, original, round string, amount int64) casino.ReceiveCallbackResult {
	t.Helper()
	res, err := w.deliver(t, ev, ref, original, round, amount)
	if err != nil {
		t.Fatalf("deliver %s %s: %v", ev, ref, err)
	}
	return res
}

// buildCleanWorld applies every legitimate casino shape.
func (w *casWorld) buildCleanWorld(t *testing.T) {
	t.Helper()
	w.mustDeliver(t, casino.CallbackEventBet, "b1", "", "r1", 1000)
	w.mustDeliver(t, casino.CallbackEventWin, "w1", "", "r1", 2500)
	w.mustDeliver(t, casino.CallbackEventBet, "b2", "", "r2", 500)
	w.mustDeliver(t, casino.CallbackEventRollback, "rb2", "b2", "r2", 0)
	w.mustDeliver(t, casino.CallbackEventBet, "b3", "", "r3", 700)
	w.mustDeliver(t, casino.CallbackEventWin, "w3", "", "r3", 700)
	w.mustDeliver(t, casino.CallbackEventRollback, "rbw3", "w3", "r3", 0)
	w.mustDeliver(t, casino.CallbackEventBet, "b4a", "", "r4", 100)
	w.mustDeliver(t, casino.CallbackEventBet, "b4b", "", "r4", 200)
	w.mustDeliver(t, casino.CallbackEventWin, "w4", "", "r4", 50)
	if res := w.mustDeliver(t, casino.CallbackEventRollback, "rb5", "never-5", "r5", 0); !res.Tombstoned {
		t.Fatalf("expected rb5 to tombstone, got %+v", res)
	}
	w.mustDeliver(t, casino.CallbackEventBet, "b6", "", "r6", 300) // open round (loss by silence)
}

func (w *casWorld) run(t *testing.T) (Run, []Mismatch, CasinoMetrics) {
	t.Helper()
	var run Run
	var ms []Mismatch
	var m CasinoMetrics
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, ms, m, err = RunCasinoConsistency(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	})
	if err != nil {
		t.Fatalf("RunCasinoConsistency: %v", err)
	}
	return run, ms, m
}

func (w *casWorld) mustRunClean(t *testing.T) CasinoMetrics {
	t.Helper()
	run, ms, m := w.run(t)
	if run.Status != StatusClean || len(ms) != 0 {
		t.Fatalf("expected a clean casino_consistency run, got %s with %d mismatches: %+v", run.Status, len(ms), ms)
	}
	return m
}

func ofKind(ms []Mismatch, kind MismatchKind) []Mismatch {
	var out []Mismatch
	for _, m := range ms {
		if m.MismatchKind == kind {
			out = append(out, m)
		}
	}
	return out
}

func mustOneOfKind(t *testing.T, ms []Mismatch, kind MismatchKind, keyContains ...string) Mismatch {
	t.Helper()
	got := ofKind(ms, kind)
	if len(got) != 1 {
		t.Fatalf("expected exactly 1 %s mismatch, got %d: %+v (all: %+v)", kind, len(got), got, ms)
	}
	for _, k := range keyContains {
		if !strings.Contains(got[0].ReconciliationKey, k) {
			t.Fatalf("%s key %q does not contain %q", kind, got[0].ReconciliationKey, k)
		}
	}
	return got[0]
}

// post writes one ledger transaction directly - the "bypassing writer" the
// stream exists to detect.
func (w *casWorld) post(t *testing.T, in ledger.TransactionInput) uuid.UUID {
	t.Helper()
	in.TenantID = w.f.tenantID
	if in.IdempotencyKey == "" {
		in.IdempotencyKey = "recon-inject-" + uuid.NewString()
	}
	var id uuid.UUID
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, err := ledger.Post(ctx, tx, in)
		id = res.TransactionID
		return err
	})
	if err != nil {
		t.Fatalf("inject ledger transaction: %v", err)
	}
	return id
}

func (w *casWorld) accounts(t *testing.T, walletID uuid.UUID) (cash, house uuid.UUID) {
	t.Helper()
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.f.tenantID,
			ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		cash, house = ids[0], ids[1]
		return nil
	})
	if err != nil {
		t.Fatalf("accounts: %v", err)
	}
	return cash, house
}

func (w *casWorld) secondPlayer(t *testing.T) (playerID, walletID uuid.UUID) {
	t.Helper()
	playerID, personID := uuid.New(), uuid.New()
	if err := w.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerID, w.f.tenantID, w.f.brandID, personID, playerID.String()+"@example.com"); err != nil {
			return err
		}
		wl, err := wallet.GetOrCreate(ctx, tx, w.f.tenantID, w.f.brandID, playerID, "EUR")
		walletID = wl.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed second player: %v", err)
	}
	return playerID, walletID
}

func strp(s string) *string { return &s }

func corr(tenantID uuid.UUID, round string) uuid.UUID {
	return casRoundCorrelationID(tenantID, casProvider, round)
}

// ---------------------------------------------------------------------
// Normal / clean world.

func TestCasinoConsistency_CleanWorldReconcilesToZero(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	m := w.mustRunClean(t)
	if m.TombstonesTotal != 1 || m.RejectionsTotal != 0 || m.RejectionsUnposted != 0 {
		t.Fatalf("unexpected metrics: %+v", m)
	}
	// Loss-by-silence: the open round is younger than the window, and in any
	// case is only ever a metric.
	if m.UnresolvedCashRoundsOlderThanWindow != 0 {
		t.Fatalf("expected no ageing rounds yet, got %+v", m)
	}
	// ledger_vs_projection is unaffected.
	var run Run
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, _, err = RunLedgerVsProjection(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	}); err != nil || run.Status != StatusClean {
		t.Fatalf("ledger_vs_projection must be clean: %s %v", run.Status, err)
	}
}

// A tenant with no casino footprint records a clean run without checks.
func TestCasinoConsistency_NoFootprintIsClean(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	w := &casWorld{pool: pool, f: f}
	w.mustRunClean(t)
}

// ---------------------------------------------------------------------
// C1 round binding.

func TestCasinoConsistency_C1_RoundBindingMismatch(t *testing.T) {
	t.Run("bet_without_round", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		w.mustDeliver(t, casino.CallbackEventBet, "c1a-ok", "", "c1a-r0", 100)
		w.mustRunClean(t)
		cash, house := w.accounts(t, w.f.walletID)
		id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoBet,
			ProviderID: strp(casProvider), ProviderTxID: strp("c1a-bypass"), CorrelationID: corr(w.f.tenantID, "c1a-r1"),
			Entries: []ledger.EntryInput{{LedgerAccountID: cash, Direction: ledger.Debit, Amount: 100}, {LedgerAccountID: house, Direction: ledger.Credit, Amount: 100}}})
		_, ms, _ := w.run(t)
		mustOneOfKind(t, ms, MismatchKindCasRoundBinding, "ledger_transaction="+id.String(), "check=bet_has_round")
	})
	t.Run("round_without_bet", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		w.mustDeliver(t, casino.CallbackEventBet, "c1b-ok", "", "c1b-r0", 100)
		w.mustRunClean(t)
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return casino.BindProviderRound(ctx, tx, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.sessionID, w.game.ID, casProvider, "c1b-orphan-round", nil)
		}); err != nil {
			t.Fatal(err)
		}
		_, ms, _ := w.run(t)
		mustOneOfKind(t, ms, MismatchKindCasRoundBinding, "check=round_has_bet")
	})
	t.Run("round_player", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		p2, w2 := w.secondPlayer(t)
		s2 := w.mintSession(t, p2, w2)
		w.mustRunClean(t)
		// The round belongs to player 2, but a bypassing writer debits
		// player 1's wallet under its correlation.
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return casino.BindProviderRound(ctx, tx, w.f.tenantID, w.f.brandID, p2, s2, w.game.ID, casProvider, "c1c-r", nil)
		}); err != nil {
			t.Fatal(err)
		}
		cash, house := w.accounts(t, w.f.walletID)
		id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoBet,
			ProviderID: strp(casProvider), ProviderTxID: strp("c1c-bet"), CorrelationID: corr(w.f.tenantID, "c1c-r"),
			Entries: []ledger.EntryInput{{LedgerAccountID: cash, Direction: ledger.Debit, Amount: 100}, {LedgerAccountID: house, Direction: ledger.Credit, Amount: 100}}})
		_, ms, _ := w.run(t)
		m := mustOneOfKind(t, ms, MismatchKindCasRoundBinding, "ledger_transaction="+id.String(), "check=round_player")
		if !strings.Contains(m.ExpectedValue, p2.String()) || !strings.Contains(m.ActualValue, w.f.playerAccountID.String()) {
			t.Fatalf("unexpected evidence: %+v", m)
		}
	})
	t.Run("round_correlation", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		w.mustRunClean(t)
		bogus := uuid.New()
		var roundRowID uuid.UUID
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO casino_provider_rounds (tenant_id, brand_id, player_account_id, launch_session_id, game_id, provider_id, provider_round_id, correlation_id)
				VALUES ($1, $2, $3, $4, $5, $6, 'c1d-r', $7) RETURNING id`,
				w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.sessionID, w.game.ID, casProvider, bogus).Scan(&roundRowID)
		}); err != nil {
			t.Fatal(err)
		}
		cash, house := w.accounts(t, w.f.walletID)
		w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoBet,
			ProviderID: strp(casProvider), ProviderTxID: strp("c1d-bet"), CorrelationID: bogus,
			Entries: []ledger.EntryInput{{LedgerAccountID: cash, Direction: ledger.Debit, Amount: 100}, {LedgerAccountID: house, Direction: ledger.Credit, Amount: 100}}})
		_, ms, _ := w.run(t)
		mustOneOfKind(t, ms, MismatchKindCasRoundBinding, "round="+roundRowID.String(), "check=round_correlation")
	})
}

// State-type findings are re-detected on every run until resolved
// (intentional, ADR 0023 §4); a re-run never alters earlier evidence.
func TestCasinoConsistency_StateTypeReDetectedEachRun(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	cash, house := w.accounts(t, w.f.walletID)
	w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoBet,
		ProviderID: strp(casProvider), ProviderTxID: strp("st-bypass"), CorrelationID: corr(w.f.tenantID, "st-r"),
		Entries: []ledger.EntryInput{{LedgerAccountID: cash, Direction: ledger.Debit, Amount: 100}, {LedgerAccountID: house, Direction: ledger.Credit, Amount: 100}}})
	run1, ms1, _ := w.run(t)
	run2, ms2, _ := w.run(t)
	if len(ofKind(ms1, MismatchKindCasRoundBinding)) != 1 || len(ofKind(ms2, MismatchKindCasRoundBinding)) != 1 || run1.ID == run2.ID {
		t.Fatalf("state-type finding must re-raise per run: %+v / %+v", ms1, ms2)
	}
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1`, run1.ID).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("first run's evidence must be unchanged: n=%d err=%v", n, err)
	}
}

// ---------------------------------------------------------------------
// C2 posting shape.

func TestCasinoConsistency_C2_PostingShapeMismatch(t *testing.T) {
	t.Run("win_credits_a_wallet_no_round_bet_debited", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		_, w2 := w.secondPlayer(t)
		w.mustDeliver(t, casino.CallbackEventBet, "c2-bet", "", "c2-r", 1000)
		w.mustRunClean(t)
		cash2, house := w.accounts(t, w2)
		id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoWin,
			ProviderID: strp(casProvider), ProviderTxID: strp("c2-win"), CorrelationID: corr(w.f.tenantID, "c2-r"),
			Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 900}, {LedgerAccountID: cash2, Direction: ledger.Credit, Amount: 900}}})
		_, ms, _ := w.run(t)
		m := mustOneOfKind(t, ms, MismatchKindCasPostingShape, "ledger_transaction="+id.String(), "check=win_wallet")
		if !strings.Contains(m.ActualValue, w2.String()) || !strings.Contains(m.ExpectedValue, w.f.walletID.String()) {
			t.Fatalf("unexpected evidence: %+v", m)
		}
	})
	t.Run("bet_touching_two_wallets", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		_, w2 := w.secondPlayer(t)
		w.mustRunClean(t)
		cash1, house := w.accounts(t, w.f.walletID)
		cash2, _ := w.accounts(t, w2)
		sbFundAccount(t, w, cash2, 1000)
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return casino.BindProviderRound(ctx, tx, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.sessionID, w.game.ID, casProvider, "c2b-r", nil)
		}); err != nil {
			t.Fatal(err)
		}
		id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoBet,
			ProviderID: strp(casProvider), ProviderTxID: strp("c2b-bet"), CorrelationID: corr(w.f.tenantID, "c2b-r"),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cash1, Direction: ledger.Debit, Amount: 100}, {LedgerAccountID: cash2, Direction: ledger.Debit, Amount: 100},
				{LedgerAccountID: house, Direction: ledger.Credit, Amount: 200}}})
		_, ms, _ := w.run(t)
		mustOneOfKind(t, ms, MismatchKindCasPostingShape, "ledger_transaction="+id.String(), "check=single_wallet")
	})
}

// sbFundAccount credits a player_cash account via a reason-coded
// manual_adjustment (test fixture funding only).
func sbFundAccount(t *testing.T, w *casWorld, cash uuid.UUID, amount int64) {
	t.Helper()
	reason := "test fixture funding"
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		adj, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fund-" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{{LedgerAccountID: adj, Direction: ledger.Debit, Amount: amount}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: amount}},
		})
		return err
	})
	if err != nil {
		t.Fatalf("fund account: %v", err)
	}
}

// ---------------------------------------------------------------------
// C3 orphan win.

func TestCasinoConsistency_C3_OrphanWin(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	w.mustRunClean(t)
	cash, house := w.accounts(t, w.f.walletID)
	id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoWin,
		ProviderID: strp(casProvider), ProviderTxID: strp("c3-orphan"), CorrelationID: corr(w.f.tenantID, "c3-no-bet-round"),
		Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 400}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 400}}})
	_, ms, _ := w.run(t)
	mustOneOfKind(t, ms, MismatchKindCasOrphanWin, "ledger_transaction="+id.String())
	if len(ms) != 1 {
		t.Fatalf("an orphan win must raise exactly one finding, got %+v", ms)
	}
}

// ---------------------------------------------------------------------
// C4 rollback linkage.

func (w *casWorld) betTxID(t *testing.T, ref string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3`,
			w.f.tenantID, casProvider, ref).Scan(&id)
	}); err != nil {
		t.Fatalf("find %s: %v", ref, err)
	}
	return id
}

func TestCasinoConsistency_C4_RollbackLinkageMismatch(t *testing.T) {
	t.Run("rollback_of_a_non_casino_original", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		w.mustRunClean(t)
		var depID uuid.UUID
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'deposit'`, w.f.tenantID).Scan(&depID)
		}); err != nil {
			t.Fatal(err)
		}
		cash, house := w.accounts(t, w.f.walletID)
		id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoRollback,
			ProviderID: strp(casProvider), ProviderTxID: strp("c4a-rb"), CorrelationID: uuid.New(), ReversesTransactionID: &depID,
			Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 10}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 10}}})
		_, ms, _ := w.run(t)
		mustOneOfKind(t, ms, MismatchKindCasRollbackLinkage, "ledger_transaction="+id.String(), "check=reverses")
	})
	// Same provider, but the "original" is not a bet or win (a casino
	// tombstone) - isolates the type rule from the provider rule.
	t.Run("rollback_of_a_same_provider_tombstone", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		w.mustDeliver(t, casino.CallbackEventRollback, "c4t-rb", "c4t-unseen", "c4t-r", 0)
		w.mustRunClean(t)
		tomb := w.betTxID(t, "c4t-unseen")
		cash, house := w.accounts(t, w.f.walletID)
		id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoRollback,
			ProviderID: strp(casProvider), ProviderTxID: strp("c4t-rb2"), CorrelationID: corr(w.f.tenantID, "c4t-r"), ReversesTransactionID: &tomb,
			Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 10}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 10}}})
		_, ms, _ := w.run(t)
		m := mustOneOfKind(t, ms, MismatchKindCasRollbackLinkage, "ledger_transaction="+id.String(), "check=reverses")
		if !strings.Contains(m.ActualValue, "original_type=tombstone") || !strings.Contains(m.ActualValue, "same_provider=true") {
			t.Fatalf("unexpected evidence: %+v", m)
		}
	})
	// A casino original, but the rollback names a different provider -
	// isolates the provider rule from the type rule.
	t.Run("rollback_under_a_different_provider", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		w.mustDeliver(t, casino.CallbackEventBet, "c4p-bet", "", "c4p-r", 1000)
		w.mustRunClean(t)
		orig := w.betTxID(t, "c4p-bet")
		cash, house := w.accounts(t, w.f.walletID)
		id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoRollback,
			ProviderID: strp("other-casino"), ProviderTxID: strp("c4p-rb"), CorrelationID: corr(w.f.tenantID, "c4p-r"), ReversesTransactionID: &orig,
			Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 1000}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 1000}}})
		_, ms, _ := w.run(t)
		m := mustOneOfKind(t, ms, MismatchKindCasRollbackLinkage, "ledger_transaction="+id.String(), "check=reverses")
		if !strings.Contains(m.ActualValue, "original_type=casino_bet") || !strings.Contains(m.ActualValue, "same_provider=false") {
			t.Fatalf("unexpected evidence: %+v", m)
		}
	})
	t.Run("not_the_exact_inverse", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		w.mustDeliver(t, casino.CallbackEventBet, "c4b-bet", "", "c4b-r", 1000)
		w.mustRunClean(t)
		orig := w.betTxID(t, "c4b-bet")
		cash, house := w.accounts(t, w.f.walletID)
		id := w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoRollback,
			ProviderID: strp(casProvider), ProviderTxID: strp("c4b-rb"), CorrelationID: corr(w.f.tenantID, "c4b-r"), ReversesTransactionID: &orig,
			Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 999}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 999}}})
		_, ms, _ := w.run(t)
		mustOneOfKind(t, ms, MismatchKindCasRollbackLinkage, "ledger_transaction="+id.String(), "check=exact_inverse")
	})
	t.Run("second_reversal_of_one_original", func(t *testing.T) {
		w := newCasWorld(t, testPool(t))
		w.mustDeliver(t, casino.CallbackEventBet, "c4c-bet", "", "c4c-r", 1000)
		w.mustDeliver(t, casino.CallbackEventRollback, "c4c-rb1", "c4c-bet", "c4c-r", 0)
		w.mustRunClean(t)
		orig := w.betTxID(t, "c4c-bet")
		cash, house := w.accounts(t, w.f.walletID)
		w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoRollback,
			ProviderID: strp(casProvider), ProviderTxID: strp("c4c-rb2"), CorrelationID: corr(w.f.tenantID, "c4c-r"), ReversesTransactionID: &orig,
			Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 1000}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 1000}}})
		_, ms, _ := w.run(t)
		mustOneOfKind(t, ms, MismatchKindCasRollbackLinkage, "original="+orig.String(), "check=one_reversal")
	})
}

// ---------------------------------------------------------------------
// C6 unposted provider event, C7 tombstone late original (evidence-type).

func (w *casWorld) recordRejection(t *testing.T, rej casino.CallbackRejection) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := casino.RecordCallbackRejection(ctx, tx, w.f.tenantID, casProvider, rej, "")
		return err
	}); err != nil {
		t.Fatalf("record rejection: %v", err)
	}
}

func TestCasinoConsistency_C6_UnpostedProviderEvent(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	w.mustRunClean(t)
	// A real verified rejection: a win for a round with no bet (E5).
	if _, err := w.deliver(t, casino.CallbackEventWin, "c6-win", "", "c6-no-bet", 800); err == nil {
		t.Fatal("expected the orphan win callback to be rejected")
	}
	// A rejection whose reference the ledger DOES hold (a payload
	// mismatch on a posted bet) is not a C6 finding.
	if _, err := w.deliver(t, casino.CallbackEventBet, "b1", "", "r1", 1001); err == nil {
		t.Fatal("expected the divergent redelivery to be rejected")
	}
	run, ms, m := w.run(t)
	mm := mustOneOfKind(t, ms, MismatchKindCasUnpostedEvent, "provider_tx_id=c6-win", "reason=bet_not_found")
	if len(ms) != 1 || run.Status != StatusMismatchesFound || !strings.Contains(mm.ActualValue, "amount=800") {
		t.Fatalf("unexpected run: %s %+v", run.Status, ms)
	}
	if m.RejectionsTotal != 2 || m.RejectionsUnposted != 1 {
		t.Fatalf("unexpected metrics: %+v", m)
	}
	// Evidence-type: never re-raised.
	w.mustRunClean(t)
	// Settlement: the provider redelivers the bet and then the win through
	// the normal idempotent path; the next run is clean and the old
	// finding stays as evidence until a human resolves it.
	w.mustDeliver(t, casino.CallbackEventBet, "c6-late-bet", "", "c6-no-bet", 100)
	w.mustDeliver(t, casino.CallbackEventWin, "c6-win", "", "c6-no-bet", 800)
	if _, _, m2 := w.run(t); m2.RejectionsUnposted != 0 {
		t.Fatalf("after redelivery the rejection is posted: %+v", m2)
	}
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE mismatch_kind = 'cas_unposted_provider_event' AND investigation_status = 'open'`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("the finding must remain as open evidence: n=%d err=%v", n, err)
	}
}

func TestCasinoConsistency_C6_EveryRecordedClassWithoutAPosting(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.mustRunClean(t)
	classes := []casino.RejectionClass{
		casino.RejectionAmbiguousRound, casino.RejectionWalletCollision, casino.RejectionMixedFunding,
		casino.RejectionLockAlreadyReleased, casino.RejectionBonusBetNotLocked, casino.RejectionBetNotFound,
		casino.RejectionAlreadyRolledBack, casino.RejectionRoundOwnershipConflict, casino.RejectionRollbackOfTombstonedOriginal,
	}
	for i, c := range classes {
		rej := casino.CallbackRejection{Class: c, EventType: casino.CallbackEventWin, ProviderTxID: fmt.Sprintf("c6all-%d", i), RoundID: "r", AssetCode: "EUR", Amount: 5}
		if c == casino.RejectionAlreadyRolledBack || c == casino.RejectionRollbackOfTombstonedOriginal {
			rej.EventType, rej.OriginalProviderTxID, rej.Amount = casino.CallbackEventRollback, "o", 0
		}
		if c == casino.RejectionRoundOwnershipConflict {
			rej.EventType = casino.CallbackEventBet
		}
		w.recordRejection(t, rej)
	}
	_, ms, _ := w.run(t)
	if got := ofKind(ms, MismatchKindCasUnpostedEvent); len(got) != len(classes) {
		t.Fatalf("expected %d C6 findings, got %d: %+v", len(classes), len(got), ms)
	}
}

func TestCasinoConsistency_C7_TombstoneLaterMatchedByOriginal(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	w.mustRunClean(t)
	// E3: the original bet arrives after its rollback tombstoned it.
	res := w.mustDeliver(t, casino.CallbackEventBet, "never-5", "", "r5", 900)
	if res.Outcome != casino.OutcomeDeclined {
		t.Fatalf("expected E3 decline, got %+v", res)
	}
	// E10 for a win reference in the same shape.
	w.mustDeliver(t, casino.CallbackEventRollback, "rb-c7w", "c7-win", "r1", 0)
	if _, err := w.deliver(t, casino.CallbackEventWin, "c7-win", "", "r1", 100); err == nil {
		t.Fatal("expected E10")
	}
	_, ms, _ := w.run(t)
	c7 := ofKind(ms, MismatchKindCasTombstoneLateOrigin)
	if len(c7) != 2 || len(ofKind(ms, MismatchKindCasUnpostedEvent)) != 0 || len(ms) != 2 {
		t.Fatalf("expected exactly two C7 findings and nothing else, got %+v", ms)
	}
	keys := c7[0].ReconciliationKey + "|" + c7[1].ReconciliationKey
	if !strings.Contains(keys, "provider_tx_id=never-5") || !strings.Contains(keys, "provider_tx_id=c7-win") {
		t.Fatalf("unexpected C7 keys: %s", keys)
	}
	// Once per tombstone: a redelivered late original adds nothing.
	w.mustDeliver(t, casino.CallbackEventBet, "never-5", "", "r5", 900)
	w.mustRunClean(t)
}

// ---------------------------------------------------------------------
// Concurrency.

func TestCasinoConsistency_ConcurrentRunsSerializedByAdvisoryLock(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	hold, firstIn := make(chan struct{}), make(chan bool, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, _, _, acquired, err := TryRunCasinoConsistencyForTenant(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
			firstIn <- acquired
			<-hold
			return err
		})
	}()
	if !<-firstIn {
		close(hold)
		wg.Wait()
		t.Fatal("first run must acquire the lock")
	}
	var second bool
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, _, _, second, err = TryRunCasinoConsistencyForTenant(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	})
	close(hold)
	wg.Wait()
	if err != nil || second {
		t.Fatalf("second concurrent run must skip: acquired=%t err=%v", second, err)
	}
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE stream = 'casino_consistency'`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("expected exactly one casino_consistency run, got %d (%v)", n, err)
	}
	// A different stream's lock never contends with this one.
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, acq, err := TryRunCasinoConsistencyForTenant(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		if err == nil && !acq {
			err = fmt.Errorf("lock must be free after the holder committed")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// A run concurrent with live bet/win/rollback traffic sees each check's
// single-statement snapshot and never reports a false mismatch.
func TestCasinoConsistency_LiveTrafficProducesNoFalseMismatch(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var delivered atomic.Int64
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				round := fmt.Sprintf("live-%d-%d", g, i)
				if _, err := w.deliver(t, casino.CallbackEventBet, round+"-b", "", round, 10); err != nil {
					t.Errorf("live bet: %v", err)
					return
				}
				delivered.Add(1)
				switch i % 3 {
				case 0:
					_, _ = w.deliver(t, casino.CallbackEventWin, round+"-w", "", round, 15)
				case 1:
					_, _ = w.deliver(t, casino.CallbackEventRollback, round+"-rb", round+"-b", round, 0)
				default:
					_, _ = w.deliver(t, casino.CallbackEventRollback, round+"-tomb", round+"-unseen", round, 0)
				}
			}
		}(g)
	}
	// At least 15 runs, and keep running until the traffic has genuinely
	// interleaved with them (>= 45 live rounds).
	for i := 0; i < 15 || delivered.Load() < 45; i++ {
		if i > 2000 {
			close(stop)
			wg.Wait()
			t.Fatal("live traffic did not progress")
		}
		run, ms, _ := w.run(t)
		if run.Status != StatusClean {
			close(stop)
			wg.Wait()
			t.Fatalf("run %d under live traffic reported %+v", i, ms)
		}
	}
	close(stop)
	wg.Wait()
	w.mustRunClean(t)
}

// ---------------------------------------------------------------------
// Authorization / tenant isolation.

func TestCasinoConsistency_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	a := newCasWorld(t, pool)
	b := newCasWorld(t, pool)
	a.buildCleanWorld(t)
	b.buildCleanWorld(t)
	// Tenant A: an orphan win and an unposted rejection for reference "x-ref".
	cash, house := a.accounts(t, a.f.walletID)
	a.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoWin,
		ProviderID: strp(casProvider), ProviderTxID: strp("x-orphan"), CorrelationID: corr(a.f.tenantID, "x-none"),
		Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 5}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 5}}})
	a.recordRejection(t, casino.CallbackRejection{Class: casino.RejectionBetNotFound, EventType: casino.CallbackEventWin, ProviderTxID: "x-ref", Amount: 5})
	// Tenant B legitimately posts a bet under the SAME provider reference;
	// it must not resolve A's rejection, and B sees none of A's drift.
	b.mustDeliver(t, casino.CallbackEventBet, "x-ref", "", "x-round", 10)

	_, msB, mB := b.run(t)
	if len(msB) != 0 || mB.RejectionsTotal != 0 {
		t.Fatalf("tenant B must see none of A's drift or rejections: %+v %+v", msB, mB)
	}
	_, msA, _ := a.run(t)
	mustOneOfKind(t, msA, MismatchKindCasOrphanWin)
	mustOneOfKind(t, msA, MismatchKindCasUnpostedEvent, "provider_tx_id=x-ref")
	// B's connection reads zero of A's mismatch/run rows.
	var n int
	if err := pool.WithTenant(context.Background(), b.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE tenant_id = $1`, a.f.tenantID).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("tenant B read %d of A's mismatches (%v)", n, err)
	}
}

// ---------------------------------------------------------------------
// No-effect: detection never corrects and never touches money.

func TestCasinoConsistency_DetectionHasNoFinancialEffect(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	cash, house := w.accounts(t, w.f.walletID)
	w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoWin,
		ProviderID: strp(casProvider), ProviderTxID: strp("ne-orphan"), CorrelationID: corr(w.f.tenantID, "ne-none"),
		Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 5}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 5}}})
	w.recordRejection(t, casino.CallbackRejection{Class: casino.RejectionBetNotFound, EventType: casino.CallbackEventWin, ProviderTxID: "ne-rej", Amount: 5})
	before := noeffect.CaptureCasino(t, w.pool, []uuid.UUID{w.f.tenantID})
	var rejBefore int
	_ = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&rejBefore)
	})
	for i := 0; i < 2; i++ {
		if run, _, _ := w.run(t); run.Status != StatusMismatchesFound && i == 0 {
			t.Fatalf("expected findings, got %s", run.Status)
		}
	}
	noeffect.AssertNoCasinoEffect(t, w.pool, []uuid.UUID{w.f.tenantID}, before)
	var rejAfter int
	_ = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&rejAfter)
	})
	if rejAfter != rejBefore {
		t.Fatalf("the stream must never write the rejection record: %d -> %d", rejBefore, rejAfter)
	}
}

// ---------------------------------------------------------------------
// Sweep wiring: audit (with metrics), P1 log line, failure audit.

type logCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logCapture) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logCapture) lines() []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(l.buf.String(), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func TestRunSweep_CasinoStreamWiredAuditedAndP1Logged(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	cash, house := w.accounts(t, w.f.walletID)
	w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoWin,
		ProviderID: strp(casProvider), ProviderTxID: strp("sw-orphan"), CorrelationID: corr(w.f.tenantID, "sw-none"),
		Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 5}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 5}}})

	lc := &logCapture{}
	logger := slog.New(slog.NewJSONHandler(lc, nil))
	outcomes, err := RunSweep(context.Background(), pool, logger, time.Now().Add(-time.Hour), time.Now(), sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	o := findOutcome(t, outcomes, w.f)
	if o.Casino.Err != nil || o.Casino.Skipped || o.Casino.Run.Status != StatusMismatchesFound || o.Casino.Run.Stream != StreamCasinoConsistency {
		t.Fatalf("unexpected casino outcome: %+v", o.Casino)
	}
	var p1 bool
	for _, l := range lc.lines() {
		if l["msg"] == "reconciliation sweep: MISMATCH FOUND" && l["stream"] == "casino_consistency" && l["tenant_id"] == w.f.tenantID.String() {
			p1 = true
		}
	}
	if !p1 {
		t.Fatal("expected a P1 'MISMATCH FOUND' log line for the casino_consistency stream")
	}
	var meta []byte
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run'
			AND metadata->>'stream' = 'casino_consistency' AND target_id = $2`, w.f.tenantID, o.Casino.Run.ID.String()).Scan(&meta)
	}); err != nil {
		t.Fatalf("casino sweep audit row: %v", err)
	}
	for _, k := range []string{"tombstones_total", "rejections_total", "rejections_unposted", "unresolved_cash_rounds_older_than_window", "ageing_metric_window_seconds"} {
		if !strings.Contains(string(meta), `"`+k+`"`) {
			t.Fatalf("audit metadata missing metric %s: %s", k, meta)
		}
	}
}

func TestRunSweep_CasinoStreamFailureIsAuditedInFreshTransaction(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	now := time.Now()
	outcomes, err := RunSweep(context.Background(), pool, nil, now, now.Add(-time.Hour), sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatal(err)
	}
	if o := findOutcome(t, outcomes, w.f); o.Casino.Err == nil {
		t.Fatal("expected the inverted-period casino run to fail")
	}
	var runs, failed int
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE stream = 'casino_consistency'`).Scan(&runs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
			AND metadata->>'stream' = 'casino_consistency'`, w.f.tenantID).Scan(&failed)
	}); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || failed != 1 {
		t.Fatalf("expected no partial run and one failure audit, got runs=%d failed=%d", runs, failed)
	}
}
