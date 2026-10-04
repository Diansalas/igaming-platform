//go:build integration

// Stage 10.3 W2b (CAS-RECON-1): the casino callback rejection record
// through the REAL webhook handler, one case per recorded rejection class
// (including E3, E10, every G-1 409 class and the C9-extension E9
// distinct-reference case): the response is unchanged, exactly one row of
// the right class is committed (separately, for a rolled-back callback),
// no money moves, and a redelivery adds no row. Plus: I1 (an unverified
// callback writes zero rows), the play-simulation route, a failed record
// write never changing the response, and the read-only admin endpoints
// (authorization, tenant isolation, response shape).
package httpserver

import (
	"bytes"
	"context"
	"fmt"
	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

type rejEnvHTTP struct {
	pool      *db.Pool
	issuer    *auth.Issuer
	srv       *httptest.Server
	mock      *casino.MockCasinoProvider
	tenant    identity.Tenant
	brand     identity.Brand
	player    registeredPlayer
	walletID  uuid.UUID
	game      casino.Game
	sessionID uuid.UUID
}

func newRejEnvHTTP(t *testing.T) *rejEnvHTTP { return newRejEnvHTTPWithLogger(t, nil) }

// newRejEnvHTTPWithLogger is newRejEnvHTTP with an injected server logger (nil =
// the default quiet test logger).
func newRejEnvHTTPWithLogger(t *testing.T, logger *slog.Logger) *rejEnvHTTP {
	t.Helper()
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	if logger != nil {
		srv = newCasinoTestServerWithLogger(t, pool, issuer, orchestrator, logger)
	}
	e := &rejEnvHTTP{pool: pool, issuer: issuer, mock: mock, srv: srv}
	e.tenant = mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, e.srv, pool, e.tenant)
	e.brand = mustCreateBrand(t, pool, e.tenant)
	e.player = mustRegisterPlayer(t, e.srv, e.brand.Slug)
	mustActivatePlayer(t, pool, e.tenant.ID, e.player.ID)
	e.walletID = fundWallet(t, pool, e.tenant.ID, e.brand.ID, e.player.ID, "EUR", 100_000).ID
	e.game = mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, e.tenant.ID, e.game.ID)
	e.sessionID = mustMintCasinoLaunchSessionDirect(t, pool, e.tenant, e.brand.ID, e.player.ID, e.walletID, e.game.ID, "mock-casino", e.game.ProviderGameID, "EUR")
	return e
}

func (e *rejEnvHTTP) send(t *testing.T, ev casino.CallbackEventType, ref, original, round string, amount int64, session uuid.UUID) int {
	t.Helper()
	outcome := casino.OutcomeSucceeded
	if ev == casino.CallbackEventRollback {
		outcome = ""
	}
	payload := e.mock.CallbackPayload(e.tenant.ID, ev, ref, original, round, e.game.ProviderGameID, amount, "EUR", outcome, "", e.player.ID, session)
	resp := rawPostCasinoCallback(t, e.srv, "/v1/webhooks/casino/"+e.tenant.Slug+"/mock-casino", payload)
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode
}

type storedRej struct{ eventType, ref, class string }

func (e *rejEnvHTTP) rows(t *testing.T) []storedRej {
	t.Helper()
	var out []storedRej
	if err := e.pool.WithTenant(context.Background(), e.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		rs, err := tx.Query(ctx, `SELECT event_type, provider_tx_id, reason_class FROM casino_callback_rejections ORDER BY first_seen_at, id`)
		if err != nil {
			return err
		}
		defer rs.Close()
		for rs.Next() {
			var r storedRej
			if err := rs.Scan(&r.eventType, &r.ref, &r.class); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rs.Err()
	}); err != nil {
		t.Fatalf("read rejections: %v", err)
	}
	return out
}

// seedLeg posts one casino ledger transaction directly (the §16.4 abort
// shapes are not producible by the cash-only postBet; this is the
// casino_multibet_win_prefix_test.go seeding technique).
func (e *rejEnvHTTP) seedLeg(t *testing.T, txType ledger.TransactionType, ref, round string, entries func(ctx context.Context, tx pgx.Tx) ([]ledger.EntryInput, error), bonus bool) {
	t.Helper()
	provider := "mock-casino"
	err := e.pool.WithTenant(context.Background(), e.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		es, err := entries(ctx, tx)
		if err != nil {
			return err
		}
		in := ledger.TransactionInput{TenantID: e.tenant.ID, TransactionType: txType,
			IdempotencyKey: provider + ":" + ref, ProviderID: &provider, ProviderTxID: &ref,
			CorrelationID: casinoTestRoundCorrelationID(e.tenant.ID, provider, round), Entries: es}
		if bonus {
			in.BonusCost = &ledger.BonusCostAttribution{Funding: ledger.FundingOperator}
		}
		_, err = ledger.Post(ctx, tx, in)
		return err
	})
	if err != nil {
		t.Fatalf("seed %s %s: %v", txType, ref, err)
	}
}

func (e *rejEnvHTTP) accts(ctx context.Context, tx pgx.Tx, types ...ledger.AccountType) ([]uuid.UUID, error) {
	specs := make([]ledger.AccountSpec, 0, len(types))
	for _, ty := range types {
		spec := ledger.AccountSpec{AccountType: ty, AssetCode: "EUR"}
		if ty != ledger.AccountHouseGaming {
			w := e.walletID
			spec.WalletID = &w
		}
		specs = append(specs, spec)
	}
	return ledger.GetOrCreateAccounts(ctx, tx, e.tenant.ID, specs...)
}

func TestCasinoRejectionRecord_EveryClassViaWebhook(t *testing.T) {
	type tc struct {
		name       string
		setup      func(t *testing.T, e *rejEnvHTTP)
		deliver    func(t *testing.T, e *rejEnvHTTP) int
		wantStatus int
		want       storedRej
		// committing: the callback itself commits (E3's audit row), so the
		// audit delta is +1 per delivery instead of 0.
		auditPerDelivery int
	}
	win := func(ref, round string) func(t *testing.T, e *rejEnvHTTP) int {
		return func(t *testing.T, e *rejEnvHTTP) int {
			return e.send(t, casino.CallbackEventWin, ref, "", round, 1000, uuid.Nil)
		}
	}
	cases := []tc{
		{
			name: "E10_late_win_after_tombstone",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				if s := e.send(t, casino.CallbackEventBet, "e10-bet", "", "e10-r", 1000, e.sessionID); s != http.StatusOK {
					t.Fatalf("bet: %d", s)
				}
				if s := e.send(t, casino.CallbackEventRollback, "e10-rb", "e10-win", "e10-r", 0, uuid.Nil); s != http.StatusOK {
					t.Fatalf("tombstoning rollback: %d", s)
				}
			},
			deliver: win("e10-win", "e10-r"), wantStatus: http.StatusConflict,
			want: storedRej{"win", "e10-win", "original_tombstoned"},
		},
		{
			name: "E3_late_bet_after_tombstone",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				if s := e.send(t, casino.CallbackEventRollback, "e3-rb", "e3-bet", "e3-r", 0, uuid.Nil); s != http.StatusOK {
					t.Fatalf("tombstoning rollback: %d", s)
				}
			},
			deliver: func(t *testing.T, e *rejEnvHTTP) int {
				return e.send(t, casino.CallbackEventBet, "e3-bet", "", "e3-r", 1000, e.sessionID)
			},
			wantStatus: http.StatusOK, want: storedRej{"bet", "e3-bet", "original_tombstoned"}, auditPerDelivery: 1,
		},
		{
			name: "G1_ambiguous_round",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				seedDirectCasinoBetLeg(t, e.pool, e.tenant.ID, "mock-casino", "amb-1", "amb-r", e.walletID, "EUR", ledger.AccountPlayerBonus, 500)
				seedDirectCasinoBetLeg(t, e.pool, e.tenant.ID, "mock-casino", "amb-2", "amb-r", e.walletID, "EUR", ledger.AccountPlayerBonus, 500)
			},
			deliver: win("amb-win", "amb-r"), wantStatus: http.StatusConflict,
			want: storedRej{"win", "amb-win", "ambiguous_round"},
		},
		{
			name: "G1_wallet_collision",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				p2 := mustRegisterPlayer(t, e.srv, e.brand.Slug)
				mustActivatePlayer(t, e.pool, e.tenant.ID, p2.ID)
				w2 := fundWallet(t, e.pool, e.tenant.ID, e.brand.ID, p2.ID, "EUR", 10_000)
				seedDirectCasinoBetLeg(t, e.pool, e.tenant.ID, "mock-casino", "col-a", "col-r", e.walletID, "EUR", ledger.AccountPlayerCash, 100)
				seedDirectCasinoBetLeg(t, e.pool, e.tenant.ID, "mock-casino", "col-b", "col-r", w2.ID, "EUR", ledger.AccountPlayerCash, 100)
			},
			deliver: win("col-win", "col-r"), wantStatus: http.StatusConflict,
			want: storedRej{"win", "col-win", "wallet_collision"},
		},
		{
			name: "G1_mixed_funding",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				seedDirectCasinoBetLeg(t, e.pool, e.tenant.ID, "mock-casino", "mix-c", "mix-r", e.walletID, "EUR", ledger.AccountPlayerCash, 100)
				seedDirectCasinoBetLeg(t, e.pool, e.tenant.ID, "mock-casino", "mix-b", "mix-r", e.walletID, "EUR", ledger.AccountPlayerBonus, 100)
			},
			deliver: win("mix-win", "mix-r"), wantStatus: http.StatusConflict,
			want: storedRej{"win", "mix-win", "mixed_funding"},
		},
		{
			name: "G1_lock_already_released",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				e.seedLeg(t, ledger.TxCasinoBet, "lk-bet", "lk-r", func(ctx context.Context, tx pgx.Tx) ([]ledger.EntryInput, error) {
					a, err := e.accts(ctx, tx, ledger.AccountPlayerCash, ledger.AccountPlayerLockedCash)
					return []ledger.EntryInput{{LedgerAccountID: a[0], Direction: ledger.Debit, Amount: 300}, {LedgerAccountID: a[1], Direction: ledger.Credit, Amount: 300}}, err
				}, false)
				// A release of the whole lock under the same round.
				e.seedLeg(t, ledger.TxCasinoWin, "lk-release", "lk-r", func(ctx context.Context, tx pgx.Tx) ([]ledger.EntryInput, error) {
					a, err := e.accts(ctx, tx, ledger.AccountPlayerLockedCash, ledger.AccountPlayerCash)
					return []ledger.EntryInput{{LedgerAccountID: a[0], Direction: ledger.Debit, Amount: 300}, {LedgerAccountID: a[1], Direction: ledger.Credit, Amount: 300}}, err
				}, false)
			},
			deliver: win("lk-win", "lk-r"), wantStatus: http.StatusConflict,
			want: storedRej{"win", "lk-win", "lock_already_released"},
		},
		{
			name: "G1_bonus_bet_not_locked",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				seedDirectCasinoBetLeg(t, e.pool, e.tenant.ID, "mock-casino", "bnl-bet", "bnl-r", e.walletID, "EUR", ledger.AccountPlayerBonus, 200)
			},
			deliver: win("bnl-win", "bnl-r"), wantStatus: http.StatusConflict,
			want: storedRej{"win", "bnl-win", "bonus_bet_not_locked"},
		},
		{
			name:    "bet_not_found_orphan_win",
			setup:   func(t *testing.T, e *rejEnvHTTP) {},
			deliver: win("orph-win", "orph-r"), wantStatus: http.StatusBadRequest,
			want: storedRej{"win", "orph-win", "bet_not_found"},
		},
		{
			name: "already_rolled_back_second_distinct_reference",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				if s := e.send(t, casino.CallbackEventBet, "arb-bet", "", "arb-r", 1000, e.sessionID); s != http.StatusOK {
					t.Fatalf("bet: %d", s)
				}
				if s := e.send(t, casino.CallbackEventRollback, "arb-rb1", "arb-bet", "arb-r", 0, uuid.Nil); s != http.StatusOK {
					t.Fatalf("first rollback: %d", s)
				}
			},
			deliver: func(t *testing.T, e *rejEnvHTTP) int {
				return e.send(t, casino.CallbackEventRollback, "arb-rb2", "arb-bet", "arb-r", 0, uuid.Nil)
			},
			wantStatus: http.StatusConflict, want: storedRej{"rollback", "arb-rb2", "already_rolled_back"},
		},
		{
			name: "payload_mismatch_divergent_redelivery",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				if s := e.send(t, casino.CallbackEventBet, "pm-bet", "", "pm-r", 1000, e.sessionID); s != http.StatusOK {
					t.Fatalf("bet: %d", s)
				}
			},
			deliver: func(t *testing.T, e *rejEnvHTTP) int {
				return e.send(t, casino.CallbackEventBet, "pm-bet", "", "pm-r", 1500, e.sessionID)
			},
			wantStatus: http.StatusConflict, want: storedRej{"bet", "pm-bet", "payload_mismatch"},
		},
		{
			name: "round_ownership_conflict",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				p2 := mustRegisterPlayer(t, e.srv, e.brand.Slug)
				mustActivatePlayer(t, e.pool, e.tenant.ID, p2.ID)
				w2 := fundWallet(t, e.pool, e.tenant.ID, e.brand.ID, p2.ID, "EUR", 10_000)
				s2 := mustMintCasinoLaunchSessionDirect(t, e.pool, e.tenant, e.brand.ID, p2.ID, w2.ID, e.game.ID, "mock-casino", e.game.ProviderGameID, "EUR")
				payload := e.mock.CallbackPayload(e.tenant.ID, casino.CallbackEventBet, "own-p2", "", "own-r", e.game.ProviderGameID, 100, "EUR", casino.OutcomeSucceeded, "", p2.ID, s2)
				resp := rawPostCasinoCallback(t, e.srv, "/v1/webhooks/casino/"+e.tenant.Slug+"/mock-casino", payload)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					t.Fatalf("player 2 bet: %d", resp.StatusCode)
				}
			},
			deliver: func(t *testing.T, e *rejEnvHTTP) int {
				return e.send(t, casino.CallbackEventBet, "own-p1", "", "own-r", 100, e.sessionID)
			},
			wantStatus: http.StatusConflict, want: storedRej{"bet", "own-p1", "round_ownership_conflict"},
		},
		{
			name: "E9_distinct_reference_for_tombstoned_original",
			setup: func(t *testing.T, e *rejEnvHTTP) {
				if s := e.send(t, casino.CallbackEventRollback, "e9-rb1", "e9-orig", "e9-r", 0, uuid.Nil); s != http.StatusOK {
					t.Fatalf("tombstoning rollback: %d", s)
				}
			},
			deliver: func(t *testing.T, e *rejEnvHTTP) int {
				return e.send(t, casino.CallbackEventRollback, "e9-rb2", "e9-orig", "e9-r", 0, uuid.Nil)
			},
			wantStatus: http.StatusOK, want: storedRej{"rollback", "e9-rb2", "rollback_of_tombstoned_original"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRejEnvHTTP(t)
			c.setup(t, e)
			if got := e.rows(t); len(got) != 0 {
				t.Fatalf("setup must not record rejections, got %+v", got)
			}
			before := noeffect.CaptureCasino(t, e.pool, []uuid.UUID{e.tenant.ID})
			for i := 0; i < 2; i++ {
				if s := c.deliver(t, e); s != c.wantStatus {
					t.Fatalf("delivery %d: expected %d, got %d", i, c.wantStatus, s)
				}
				got := e.rows(t)
				if len(got) != 1 || got[0] != c.want {
					t.Fatalf("delivery %d: expected exactly one row %+v, got %+v", i, c.want, got)
				}
			}
			// ADR 0102 8 row 9 (failure-path P1, detached): an integrity rejection
			// answered with an error status raises ONE open casino.callback_integrity
			// alert per (provider, reason) - two deliveries are two occurrences of the
			// same alert (stable key) - and a non-error disposition raises none.
			assertCasinoIntegrityAlert(t, e, c.want.class, c.wantStatus)
			after := noeffect.CaptureCasino(t, e.pool, []uuid.UUID{e.tenant.ID})
			id := e.tenant.ID
			if after.LedgerTransactionCount[id] != before.LedgerTransactionCount[id] || after.LedgerEntryCount[id] != before.LedgerEntryCount[id] ||
				after.TombstoneCount[id] != before.TombstoneCount[id] || after.CasinoProviderRoundCount[id] != before.CasinoProviderRoundCount[id] ||
				after.AuditLogCount[id] != before.AuditLogCount[id]+2*c.auditPerDelivery {
				t.Fatalf("a rejection must have no financial effect: before=%+v after=%+v", before, after)
			}
			if c.auditPerDelivery == 0 {
				noeffect.AssertNoCasinoEffect(t, e.pool, []uuid.UUID{e.tenant.ID}, before)
			}
		})
	}
}

// I1: a callback that fails verification (tampered body) is the uniform
// 401 and writes zero rows - an unverified caller can never create one.
func TestCasinoRejectionRecord_UnverifiedCallbackWritesZeroRows(t *testing.T) {
	e := newRejEnvHTTP(t)
	if s := e.send(t, casino.CallbackEventRollback, "i1-rb", "i1-win", "i1-r", 0, uuid.Nil); s != http.StatusOK {
		t.Fatalf("tombstoning rollback: %d", s)
	}
	payload := e.mock.CallbackPayload(e.tenant.ID, casino.CallbackEventWin, "i1-win", "", "i1-r", e.game.ProviderGameID, 500, "EUR", casino.OutcomeSucceeded, "", e.player.ID, uuid.Nil)
	tampered := webhookauth.Inbound{Header: payload.Header, Body: bytes.Replace(payload.Body, []byte(`"amount":500`), []byte(`"amount":501`), 1)}
	if bytes.Equal(tampered.Body, payload.Body) {
		t.Fatal("fixture: tamper did not change the body")
	}
	resp := rawPostCasinoCallback(t, e.srv, "/v1/webhooks/casino/"+e.tenant.Slug+"/mock-casino", tampered)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the uniform 401, got %d", resp.StatusCode)
	}
	// Signed for another tenant, delivered to this one: also 401, no row.
	other := mustCreateTenant(t, e.pool)
	foreign := e.mock.CallbackPayload(other.ID, casino.CallbackEventWin, "i1-win", "", "i1-r", e.game.ProviderGameID, 500, "EUR", casino.OutcomeSucceeded, "", e.player.ID, uuid.Nil)
	resp = rawPostCasinoCallback(t, e.srv, "/v1/webhooks/casino/"+e.tenant.Slug+"/mock-casino", foreign)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the uniform 401 for a cross-tenant signature, got %d", resp.StatusCode)
	}
	if got := e.rows(t); len(got) != 0 {
		t.Fatalf("an unverified callback must write zero rows, got %+v", got)
	}
}

// The play-simulation route runs the same verify-then-post pipeline and
// records the same way.
func TestCasinoRejectionRecord_PlaySimulationRouteRecords(t *testing.T) {
	e := newRejEnvHTTP(t)
	launched := mustLaunchCasinoGame(t, e.srv, e.player.Tokens.AccessToken, e.game.ID.String(), "EUR", "real")
	key := uuid.NewString()
	if resp := postJSON(t, e.srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", e.player.Tokens.AccessToken,
		map[string]any{"stake_amount": 100, "idempotency_key": key}); resp.StatusCode != http.StatusOK {
		t.Fatalf("wager: %d", resp.StatusCode)
	}
	resp := postJSON(t, e.srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", e.player.Tokens.AccessToken,
		map[string]any{"stake_amount": 200, "idempotency_key": key})
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("divergent wager reuse must be 409, got %d", resp.StatusCode)
	}
	got := e.rows(t)
	if len(got) != 1 || got[0].class != "payload_mismatch" || got[0].eventType != "bet" {
		t.Fatalf("expected one payload_mismatch row, got %+v", got)
	}
}

// Security review R-1 (gate 10.3-W2/W3): the rejection row is
// reconciliation evidence, so a provider that disconnects (its request
// context cancelled, or past its deadline, right after the callback
// transaction rolled back) must not erase it. Before the fix this test
// asserted the opposite (a cancelled context gave no row).
func TestCasinoRejectionRecord_CommitsWhenRequestContextIsCancelled(t *testing.T) {
	pool, _ := testEnv(t)
	tenant := mustCreateTenant(t, pool)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	for i, ctx := range []context.Context{cancelled, expired} {
		err := &casino.CallbackRejectedError{ProviderID: "mock-casino",
			Rejection: casino.CallbackRejection{Class: casino.RejectionBetNotFound, EventType: casino.CallbackEventWin, ProviderTxID: fmt.Sprintf("r1-ref-%d", i), Amount: 5},
			Err:       casino.ErrBetNotFound}
		recordCasinoCallbackRejection(ctx, Deps{DB: pool}, logger, tenant.ID, "req-1", err)
	}
	if buf.Len() != 0 {
		t.Fatalf("the detached write must succeed, got a failure line: %q", buf.String())
	}
	var n int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections WHERE reason_class = 'bet_not_found'`).Scan(&n)
	}); err != nil || n != 2 {
		t.Fatalf("both rows must commit despite the cancelled/expired request context: n=%d err=%v", n, err)
	}
}

// A failed record write is logged (allow-listed fields only) and never
// changes the response: the helper swallows it. The failure is induced by
// the write's own bound (an already-expired timeout), which also proves the
// detached write is bounded rather than unbounded.
func TestCasinoRejectionRecord_WriteFailureIsLoggedNotPropagated(t *testing.T) {
	pool, _ := testEnv(t)
	tenant := mustCreateTenant(t, pool)
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	err := &casino.CallbackRejectedError{ProviderID: "mock-casino",
		Rejection: casino.CallbackRejection{Class: casino.RejectionBetNotFound, EventType: casino.CallbackEventWin, ProviderTxID: "secret-ref-123", Amount: 5},
		Err:       casino.ErrBetNotFound}
	recordCasinoCallbackRejectionWithin(context.Background(), time.Nanosecond, Deps{DB: pool}, logger, tenant.ID, "req-1", err)
	out := buf.String()
	if !strings.Contains(out, "casino_callback_rejection_record_failed") || !strings.Contains(out, `"reason_class":"bet_not_found"`) {
		t.Fatalf("expected the allow-listed failure line, got %q", out)
	}
	if strings.Contains(out, "secret-ref-123") {
		t.Fatal("the failure line must not echo the provider reference")
	}
	var n int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("no row expected: n=%d err=%v", n, err)
	}
	// Any other error is a no-op (nothing logged, nothing written).
	buf.Reset()
	recordCasinoCallbackRejection(context.Background(), Deps{DB: pool}, logger, tenant.ID, "req-2", casino.ErrBetNotFound)
	recordCasinoCallbackRejection(context.Background(), Deps{DB: pool}, logger, tenant.ID, "req-3", &webhookauth.AuthError{Reason: webhookauth.ReasonSignatureInvalid})
	if buf.Len() != 0 {
		t.Fatalf("non-rejection errors must be a silent no-op, got %q", buf.String())
	}
}

// ---------------------------------------------------------------------
// Read-only admin endpoints.

func runCasinoConsistencyForTest(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, err := reconciliation.RunCasinoConsistency(ctx, tx, tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	}); err != nil {
		t.Fatalf("run casino_consistency: %v", err)
	}
}

func TestCasinoReconciliationAdmin_AuthorizationIsolationAndShape(t *testing.T) {
	a := newRejEnvHTTP(t)
	b := newRejEnvHTTP(t)
	// Tenant A: one orphan-win rejection, and a casino_consistency run with
	// its C6 finding.
	if s := a.send(t, casino.CallbackEventWin, "adm-win", "", "adm-r", 750, uuid.Nil); s != http.StatusBadRequest {
		t.Fatalf("orphan win: %d", s)
	}
	runCasinoConsistencyForTest(t, a.pool, a.tenant.ID)

	adminA := mustCreateStaff(t, a.pool, a.tenant.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokA := mustLoginStaff(t, a.srv, a.tenant.Slug, adminA.Email, "a-decent-password-1")

	var rej pagedResponse[casinoCallbackRejectionResponse]
	resp := getJSON(t, a.srv, "/v1/admin/casino/callback-rejections", tokA.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rejections: %d", resp.StatusCode)
	}
	decodeBody(t, resp, &rej)
	if rej.Total != 1 || rej.Items[0].ProviderTxID != "adm-win" || rej.Items[0].ReasonClass != "bet_not_found" ||
		rej.Items[0].Amount == nil || *rej.Items[0].Amount != "750" || rej.Items[0].OriginalProviderTxID != nil {
		t.Fatalf("unexpected rejection page: %+v", rej)
	}
	var runs pagedResponse[casinoReconciliationRunResponse]
	resp = getJSON(t, a.srv, "/v1/admin/casino/reconciliation/runs", tokA.AccessToken)
	decodeBody(t, resp, &runs)
	if runs.Total != 1 || runs.Items[0].Stream != "casino_consistency" || runs.Items[0].Status != "mismatches_found" {
		t.Fatalf("unexpected runs page: %+v", runs)
	}
	var mms pagedResponse[casinoReconciliationMismatchResponse]
	resp = getJSON(t, a.srv, "/v1/admin/casino/reconciliation/mismatches?status=open", tokA.AccessToken)
	decodeBody(t, resp, &mms)
	if mms.Total != 1 || mms.Items[0].MismatchKind != "cas_unposted_provider_event" || mms.Items[0].RunID != runs.Items[0].ID || mms.Items[0].ResolvedAt != nil {
		t.Fatalf("unexpected mismatches page: %+v", mms)
	}
	if resp := getJSON(t, a.srv, "/v1/admin/casino/reconciliation/mismatches?status=bogus", tokA.AccessToken); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown status filter must be 400, got %d", resp.StatusCode)
	}

	// Finance and compliance may read; support, risk_manager and a player may not.
	for _, role := range []identity.StaffRole{identity.StaffRoleFinance, identity.StaffRoleCompliance} {
		s := mustCreateStaff(t, a.pool, a.tenant.ID, role, "a-decent-password-1")
		tok := mustLoginStaff(t, a.srv, a.tenant.Slug, s.Email, "a-decent-password-1")
		if resp := getJSON(t, a.srv, "/v1/admin/casino/reconciliation/runs", tok.AccessToken); resp.StatusCode != http.StatusOK {
			t.Fatalf("%s must be allowed, got %d", role, resp.StatusCode)
		}
	}
	for _, role := range []identity.StaffRole{identity.StaffRoleSupport, identity.StaffRoleRiskManager} {
		s := mustCreateStaff(t, a.pool, a.tenant.ID, role, "a-decent-password-1")
		tok := mustLoginStaff(t, a.srv, a.tenant.Slug, s.Email, "a-decent-password-1")
		for _, p := range []string{"/v1/admin/casino/reconciliation/runs", "/v1/admin/casino/reconciliation/mismatches", "/v1/admin/casino/callback-rejections"} {
			if resp := getJSON(t, a.srv, p, tok.AccessToken); resp.StatusCode != http.StatusForbidden {
				t.Fatalf("%s must be 403 on %s, got %d", role, p, resp.StatusCode)
			}
		}
	}
	for _, p := range []string{"/v1/admin/casino/reconciliation/runs", "/v1/admin/casino/callback-rejections"} {
		if resp := getJSON(t, a.srv, p, a.player.Tokens.AccessToken); resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("a player must be refused on %s, got %d", p, resp.StatusCode)
		}
		if resp := getJSON(t, a.srv, p, ""); resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("no token must be 401 on %s, got %d", p, resp.StatusCode)
		}
	}
	// Read-only: no write verb is routed.
	if resp := postJSON(t, a.srv, "/v1/admin/casino/reconciliation/mismatches", tokA.AccessToken, map[string]any{}); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST must not be routed, got %d", resp.StatusCode)
	}

	// Tenant B's admin sees none of A's evidence.
	adminB := mustCreateStaff(t, b.pool, b.tenant.ID, identity.StaffRoleTenantAdmin, "a-decent-password-1")
	tokB := mustLoginStaff(t, b.srv, b.tenant.Slug, adminB.Email, "a-decent-password-1")
	for _, p := range []string{"/v1/admin/casino/reconciliation/runs", "/v1/admin/casino/reconciliation/mismatches", "/v1/admin/casino/callback-rejections"} {
		var page pagedResponse[map[string]any]
		resp := getJSON(t, b.srv, p, tokB.AccessToken)
		decodeBody(t, resp, &page)
		if page.Total != 0 || len(page.Items) != 0 {
			t.Fatalf("tenant B must see none of A's rows on %s, got %+v", p, page)
		}
	}
}

// casinoAlertReasonByRejectionClass maps a stored rejection class to the
// integrity-alert reason casino_handlers.go raises for it (and nothing for the
// classes that map to a plain conflict/ok response).
var casinoAlertReasonByRejectionClass = map[string]string{
	"bet_not_found":            "bet_not_found",
	"payload_mismatch":         "payload_mismatch",
	"round_ownership_conflict": "provider_round_ownership_conflict",
	"original_tombstoned":      "original_tombstoned",
	"ambiguous_round":          "win_origin",
	"wallet_collision":         "win_origin",
	"mixed_funding":            "win_origin",
	"lock_already_released":    "win_origin",
	"bonus_bet_not_locked":     "win_origin",
}

func assertCasinoIntegrityAlert(t *testing.T, e *rejEnvHTTP, class string, status int) {
	t.Helper()
	reason, mapped := casinoAlertReasonByRejectionClass[class]
	var got []alertinject.Row
	rows := alertinject.ForSubject(t, e.pool, e.tenant.ID)
	if mapped && status >= 400 {
		// the alert is raised by post-response work (ADR 0102 7.4); wait (bounded)
		// for both deliveries' occurrences
		rows = alertinject.WaitUpTo(t, e.pool, e.tenant.ID, 10*time.Second, func(rs []alertinject.Row) bool {
			k := alertinject.Find(rs, string(alerting.KindCasinoCallbackIntegrity))
			return len(k) == 1 && k[0].Occurrences >= 2
		})
	}
	for _, r := range rows {
		if r.Kind == string(alerting.KindCasinoCallbackIntegrity) {
			got = append(got, r)
		}
	}
	if !mapped || status < 400 {
		if len(got) != 0 {
			t.Fatalf("class %s (status %d) must raise no integrity alert, got %+v", class, status, got)
		}
		return
	}
	if len(got) != 1 {
		t.Fatalf("class %s: want exactly one integrity alert, got %+v", class, got)
	}
	r := got[0]
	if r.Discriminator != "provider:mock-casino:reason:"+reason || r.Severity != "p1" || r.Occurrences != 2 || r.Attributes["provider_id"] != "mock-casino" {
		t.Fatalf("class %s: alert %+v, want discriminator provider:mock-casino:reason:%s, p1, 2 occurrences", class, r, reason)
	}
}
