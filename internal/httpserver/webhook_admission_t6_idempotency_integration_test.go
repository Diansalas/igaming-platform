//go:build integration

// ADR 0097 T6, per security's exact specification: deposit, win, bet ->
// rollback, reversal/refund, and win-before-bet - each run THROUGH the
// real ADR 0097 admission layer (pre-auth and verified tiers both
// present/enabled, not bypassed), asserting zero rows on a limited
// attempt, exactly one posting on redelivery, an idempotent third
// delivery, and (where applicable) the resulting wallet balance.
package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// b1ReplenishWait is how long these tests sleep, using the REAL clock,
// between exhausting a tier-B1 (VerifiedRate) bucket and redelivering -
// long enough for at least one token to refill at the deliberately fast
// test-only rates configured below (10/sec => ~100ms/token), without
// depending on a virtualized clock seam (T6 exercises admission as a real
// dependency on the request path, not admission's own timing behavior -
// T11 already owns that).
const b1ReplenishWait = 200 * time.Millisecond

// assertLedgerBalancedAndReconciled is security's explicit T6 requirement,
// applied per variant: (1) SUM(debits) == SUM(credits) directly over
// ledger_entries (the ledger's own invariant, CLAUDE.md's "Financial /
// ledger rules"), and (2) the materialized wallet_balance_projection
// equals a full rebuild computed directly from ledger_entries
// (reconciliation.RunLedgerVsProjection - "the projection is rebuilt from
// the ledger, never the reverse", reconciliation-model.md §2.1) - i.e.
// admission's rejections/redeliveries/idempotent-replays in this file
// never leave the ledger unbalanced or the projection out of sync with a
// fresh rebuild.
func assertLedgerBalancedAndReconciled(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	var debits, credits int64
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
			        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
			   FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&debits, &credits)
	})
	if err != nil {
		t.Fatalf("assertLedgerBalancedAndReconciled: sum debits/credits: %v", err)
	}
	if debits != credits {
		t.Fatalf("SUM(debits)=%d != SUM(credits)=%d for tenant %s", debits, credits, tenantID)
	}

	var run reconciliation.Run
	var mismatches []reconciliation.Mismatch
	err = pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var rerr error
		run, mismatches, rerr = reconciliation.RunLedgerVsProjection(ctx, tx, tenantID,
			time.Now().Add(-24*time.Hour), time.Now().Add(time.Hour))
		return rerr
	})
	if err != nil {
		t.Fatalf("assertLedgerBalancedAndReconciled: run ledger-vs-projection: %v", err)
	}
	if run.Status != reconciliation.StatusClean || len(mismatches) != 0 {
		t.Fatalf("expected the projection to equal a fresh rebuild from the ledger (CLEAN, zero mismatches), got status=%q mismatches=%+v", run.Status, mismatches)
	}
}

// t6Settings gives every tier a burst large enough that a handful of
// well-behaved sequential requests in these tests are never themselves
// rejected by admission - the point of T6 is that idempotency/ordering
// stay correct with admission PRESENT, not to re-test admission's own
// rejection behaviour (T1/T2/T14/etc. already do that).
func t6Settings() WebhookAdmissionSettings {
	s := testAdmissionSettings()
	for _, m := range []map[string]WebhookRateBurst{s.PreAuthRate, s.PreAuthUnknownRate, s.VerifiedRate} {
		for k, v := range m {
			v.Burst = 50
			m[k] = v
		}
	}
	return s
}

// TestAdmission_T6b_PaymentsDepositBacklog_WithinBurst_NoSustained503s is
// the payments deposit "backlog replay" B2 load scenario: a legitimate
// backlog of distinct, already-created deposit callbacks - well within
// every admission tier's configured burst (A2/A3/B1) and the A4b DB
// gate's per-key/global caps - arriving concurrently must all be admitted
// without a SINGLE 503 (the DB gate/bulkhead must not itself become the
// bottleneck for ordinary legitimate concurrency a redeploy, a brief
// upstream PSP retry storm, or a delayed-then-caught-up webhook queue
// would produce), and every one of them posts exactly once.
func TestAdmission_T6b_PaymentsDepositBacklog_WithinBurst_NoSustained503s(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	// t6Settings' burst (50) comfortably exceeds this backlog's size (20),
	// and its DB gate/bulkhead caps (testAdmissionSettings' DBGatePerKey:
	// 50, DBGateGlobal: 100, InFlightPerKey: 50, InFlightGlobal: 100) are
	// likewise never the limiting factor for 20 concurrent legitimate
	// requests - the point under test.
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, t6Settings(), false)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const backlogSize = 20
	const amountPerDeposit = int64(500)

	refs := make([]string, backlogSize)
	for i := 0; i < backlogSize; i++ {
		resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": amountPerDeposit, "payment_method": "card", "idempotency_key": uuid.NewString(),
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 creating backlog deposit %d, got %d", i, resp.StatusCode)
		}
		var intent depositIntentResponse
		decodeBody(t, resp, &intent)
		refs[i] = providerReferenceFromRedirectURL(intent.RedirectURL)
	}

	type outcome struct {
		ref    string
		status int
	}
	results := make(chan outcome, backlogSize)
	for _, ref := range refs {
		go func(ref string) {
			resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
				mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amountPerDeposit, "EUR", "", false))
			resp.Body.Close()
			results <- outcome{ref: ref, status: resp.StatusCode}
		}(ref)
	}

	var got503, gotNon200 int
	for i := 0; i < backlogSize; i++ {
		o := <-results
		if o.status == http.StatusServiceUnavailable {
			got503++
			t.Errorf("backlog delivery for %s got 503 - a legitimate within-burst backlog must never see the DB gate/bulkhead reject it", o.ref)
		}
		if o.status != http.StatusOK {
			gotNon200++
			t.Logf("backlog delivery for %s returned %d (not itself a failure of this test's own assertion unless it was 503)", o.ref, o.status)
		}
	}
	if got503 > 0 {
		t.Fatalf("expected ZERO 503s for a within-burst legitimate backlog, got %d (out of %d)", got503, backlogSize)
	}

	// Every deposit in the backlog posts exactly once.
	var totalRows int
	for _, ref := range refs {
		totalRows += ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref)
	}
	if totalRows != backlogSize {
		t.Fatalf("expected exactly %d total ledger rows (one per backlog deposit), got %d (gotNon200=%d)", backlogSize, totalRows, gotNon200)
	}

	resp := getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var wallet walletSummaryResponse
	decodeBody(t, resp, &wallet)
	if want := amountPerDeposit * backlogSize; wallet.CashBalance != want {
		t.Fatalf("expected balance %d (one credit per backlog deposit), got %d", want, wallet.CashBalance)
	}

	assertLedgerBalancedAndReconciled(t, pool, tenant.ID)
}

// TestAdmission_T6a_PaymentsDeposit_RetryAfter429_Idempotent: a
// B1-limited deposit callback writes zero rows; redelivered after the
// limiter recovers, it posts exactly once; a third, duplicate delivery is
// idempotent (still exactly one row); the wallet balance reflects exactly
// one credit.
func TestAdmission_T6a_PaymentsDeposit_RetryAfter429_Idempotent(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	settings := t6Settings()
	settings.VerifiedRate["payments"] = WebhookRateBurst{Rate: 10, Burst: 1}
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, settings, false)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	createDeposit := func(amount int64) string {
		resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": uuid.NewString(),
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201, got %d", resp.StatusCode)
		}
		var intent depositIntentResponse
		decodeBody(t, resp, &intent)
		return providerReferenceFromRedirectURL(intent.RedirectURL)
	}

	ref1 := createDeposit(1000)
	ref2 := createDeposit(2000)

	// First deposit consumes B1's single burst token.
	r1 := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref1, "", payments.OutcomeSucceeded, 1000, "EUR", "", false))
	r1.Body.Close()
	if r1.StatusCode != http.StatusOK {
		t.Fatalf("first deposit must post, got %d", r1.StatusCode)
	}

	// Second, distinct deposit is B1-limited - zero rows.
	limited := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref2, "", payments.OutcomeSucceeded, 2000, "EUR", "", false))
	limited.Body.Close()
	if limited.StatusCode == http.StatusOK {
		t.Fatal("the second, B1-limited deposit must not post on its first attempt")
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref2); got != 0 {
		t.Fatalf("a limited deposit must post ZERO rows: got %d", got)
	}

	// Redeliver (a real provider retries on 429/503) - once B1's token has
	// replenished (Rate: 10/sec => a fresh token roughly every 100ms), it
	// now posts exactly once.
	time.Sleep(b1ReplenishWait)
	redelivered := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref2, "", payments.OutcomeSucceeded, 2000, "EUR", "", false))
	redelivered.Body.Close()
	if redelivered.StatusCode != http.StatusOK {
		t.Fatalf("the redelivered deposit must post, got %d", redelivered.StatusCode)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref2); got != 1 {
		t.Fatalf("expected exactly one posting after redelivery: got %d", got)
	}

	// A THIRD, duplicate delivery of the SAME already-posted callback is
	// idempotent - still exactly one row.
	third := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref2, "", payments.OutcomeSucceeded, 2000, "EUR", "", false))
	third.Body.Close()
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref2); got != 1 {
		t.Fatalf("a duplicate redelivery must be idempotent: still expected 1 row, got %d", got)
	}

	// Balance reflects exactly one credit of each deposit (1000 + 2000).
	resp := getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var wallet walletSummaryResponse
	decodeBody(t, resp, &wallet)
	if wallet.CashBalance != 3000 {
		t.Fatalf("expected balance 3000 (one credit each), got %d", wallet.CashBalance)
	}

	assertLedgerBalancedAndReconciled(t, pool, tenant.ID)
}

// TestAdmission_T6c_CasinoBetLimitedThenRollbackReorder: a bet is
// B1-limited (zero rows); a rollback for the SAME provider_tx_id is then
// admitted and writes a tombstone (ADR 0097 §6.4's documented, ledger-
// correct reordering effect); the retried (redelivered) original bet then
// hits the tombstone and is rejected 409 with zero postings.
func TestAdmission_T6c_CasinoBetLimitedThenRollbackReorder(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	settings := t6Settings()
	settings.VerifiedRate["casino"] = WebhookRateBurst{Rate: 10, Burst: 1}
	srv := newAdmissionTestServer(t, pool, issuer, nil, orchestrator, settings, false)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	session := uuid.MustParse(launched.SessionID)

	const betRef = "t6c-bet"
	const round = "t6c-round"

	// Consume B1's single burst token with an unrelated bet first.
	warmup := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "t6c-warmup", "", "t6c-warmup-round", game.ProviderGameID, 100, "EUR", casino.OutcomeSucceeded, "", player.ID, session)
	rw := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", warmup)
	rw.Body.Close()

	// The real bet is now B1-limited.
	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betRef, "", round, game.ProviderGameID, 1000, "EUR", casino.OutcomeSucceeded, "", player.ID, session)
	rb := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	rb.Body.Close()
	if rb.StatusCode == http.StatusOK {
		t.Fatal("the bet must be B1-limited on its first attempt")
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, betRef); got != 0 {
		t.Fatalf("a limited bet must post ZERO rows: got %d", got)
	}

	// A rollback for the SAME provider_tx_id is admitted, once B1's shared
	// tenant+provider bucket has replenished - the documented §6.4 reorder,
	// not a new admission property under test.
	time.Sleep(b1ReplenishWait)
	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, "t6c-rollback", betRef, round, game.ProviderGameID, 1000, "EUR", casino.OutcomeSucceeded, "", player.ID, session)
	rr := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	rr.Body.Close()
	if rr.StatusCode != http.StatusOK {
		t.Fatalf("expected the rollback of a never-posted bet to be accepted as a tombstone, got %d", rr.StatusCode)
	}

	// The retried (redelivered) original bet now hits the tombstone (once
	// B1 has replenished again). Per postBet's own documented convention
	// (a tombstone-rejected bet is a genuine, deterministic DECLINE - not
	// a delivery failure), the webhook ack is still 200 OK; the win is
	// that it posts zero ledger rows, verified below.
	time.Sleep(b1ReplenishWait)
	retry := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	retry.Body.Close()
	if retry.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (ack) for the late bet declined against its own tombstone, got %d", retry.StatusCode)
	}
	// Exactly one row for betRef total: the TOMBSTONE itself (also keyed
	// by provider_tx_id=betRef, per postRollbackTombstone above) - the
	// late bet itself must never add a second one.
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, betRef); got != 1 {
		t.Fatalf("expected exactly one row for betRef (the tombstone, never a real bet posting): got %d", got)
	}

	assertLedgerBalancedAndReconciled(t, pool, tenant.ID)
}

// TestAdmission_T6d_CasinoWinBeforeBet: a win callback naming a bet that
// has not posted yet gets a 4xx with zero postings (ledger-finance C2's
// documented gap); once the bet posts, the redelivered win posts exactly
// once, unaffected by the earlier rejection.
func TestAdmission_T6d_CasinoWinBeforeBet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	srv := newAdmissionTestServer(t, pool, issuer, nil, orchestrator, t6Settings(), false)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	session := uuid.MustParse(launched.SessionID)

	const betRef = "t6d-bet"
	const winRef = "t6d-win"
	const round = "t6d-round"

	winPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventWin, winRef, "", round, game.ProviderGameID, 500, "EUR", casino.OutcomeSucceeded, "", player.ID, session)
	winFirst := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	winFirst.Body.Close()
	if winFirst.StatusCode == http.StatusOK {
		t.Fatal("a win before its bet must not succeed")
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, winRef); got != 0 {
		t.Fatalf("a win-before-bet must post ZERO rows: got %d", got)
	}

	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, betRef, "", round, game.ProviderGameID, 1000, "EUR", casino.OutcomeSucceeded, "", player.ID, session)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	betResp.Body.Close()
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("the bet must post, got %d", betResp.StatusCode)
	}

	winRetry := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", winPayload)
	winRetry.Body.Close()
	if winRetry.StatusCode != http.StatusOK {
		t.Fatalf("the redelivered win, after its bet posted, must succeed, got %d", winRetry.StatusCode)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, winRef); got != 1 {
		t.Fatalf("expected exactly one win posting, got %d", got)
	}

	assertLedgerBalancedAndReconciled(t, pool, tenant.ID)
}

// TestAdmission_T6e_PaymentsReversal_RetryAfter429_Idempotent: a
// B1-limited deposit-reversal callback writes zero rows; redelivered, it
// posts exactly once; a third delivery is idempotent.
func TestAdmission_T6e_PaymentsReversal_RetryAfter429_Idempotent(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	settings := t6Settings()
	settings.VerifiedRate["payments"] = WebhookRateBurst{Rate: 10, Burst: 2}
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, settings, false)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": int64(5000), "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	depositRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	deposit := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, depositRef, "", payments.OutcomeSucceeded, 5000, "EUR", "", false))
	deposit.Body.Close()
	if deposit.StatusCode != http.StatusOK {
		t.Fatalf("the deposit itself must post, got %d", deposit.StatusCode)
	}

	reversalRef := "t6e-reversal"
	reversalPayload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal, reversalRef, depositRef, payments.OutcomeSucceeded, 5000, "EUR", "", false)

	// Consume B1's remaining token with a throwaway unrelated attempt
	// naming a nonexistent deposit (fails at domain processing, not
	// admission - deliberately, so B1's single remaining slot is spent
	// exactly once before the real reversal below).
	warmup := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal, "t6e-warmup", "nonexistent-ref", payments.OutcomeSucceeded, 100, "EUR", "", false)
	rw := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", warmup)
	rw.Body.Close()

	limited := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", reversalPayload)
	limited.Body.Close()
	if limited.StatusCode == http.StatusOK {
		t.Fatal("the reversal must be B1-limited on its first attempt")
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, reversalRef); got != 0 {
		t.Fatalf("a limited reversal must post ZERO rows: got %d", got)
	}

	time.Sleep(b1ReplenishWait)
	redelivered := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", reversalPayload)
	redelivered.Body.Close()
	if redelivered.StatusCode != http.StatusOK {
		t.Fatalf("the redelivered reversal must post, got %d", redelivered.StatusCode)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, reversalRef); got != 1 {
		t.Fatalf("expected exactly one reversal posting, got %d", got)
	}

	third := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", reversalPayload)
	third.Body.Close()
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, reversalRef); got != 1 {
		t.Fatalf("a duplicate reversal redelivery must be idempotent: still expected 1, got %d", got)
	}

	assertLedgerBalancedAndReconciled(t, pool, tenant.ID)
}

// TestAdmission_T6f_PaymentsReversal_BeforeDeposit_ThenDepositArrives is
// the distinct "reversal arrives before its own deposit exists" ordering
// security asked for (T6e above is "reversal after the deposit, but
// itself B1-limited" - a different property). Per the orchestrator's own
// documented Flow 2 tombstone semantics (internal/payments/
// orchestrator.go's receiveDepositReversalCallback/
// postDepositReversalTombstone - the payments-domain analogue of ADR
// 0097 §6.4's casino bet/rollback reorder), a reversal naming a deposit
// reference that has never posted is ACCEPTED and writes a tombstone
// ledger_transactions row keyed by the ORIGINAL deposit's provider_tx_id
// (not the reversal's own) - so the late-arriving original deposit is
// then permanently rejected against that same (provider_id,
// provider_tx_id), never posting.
func TestAdmission_T6f_PaymentsReversal_BeforeDeposit_ThenDepositArrives(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, t6Settings(), false)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": int64(4000), "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	depositRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	const reversalRef = "t6f-reversal"
	reversalPayload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal, reversalRef, depositRef, payments.OutcomeSucceeded, 4000, "EUR", "", false)

	// The reversal arrives BEFORE its own deposit has ever posted - the
	// tombstone branch, keyed by the ORIGINAL deposit reference.
	early := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", reversalPayload)
	early.Body.Close()
	if early.StatusCode != http.StatusOK {
		t.Fatalf("expected the reversal of a never-posted deposit to be accepted as a tombstone, got %d", early.StatusCode)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, depositRef); got != 1 {
		t.Fatalf("expected exactly one tombstone row keyed by the original deposit reference: got %d", got)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, reversalRef); got != 0 {
		t.Fatalf("the tombstone is keyed by the ORIGINAL reference, not the reversal's own - expected 0 rows for the reversal reference, got %d", got)
	}

	// The late-arriving deposit now hits its own tombstone and must never
	// post. PRH-payments-callback-cutover (ADR 0095 §4.4 tombstone cell,
	// LF95-C6(d), §6.2 "anomaly" row): old->new, this used to roll back to
	// a non-200 (409/500); it now COMMITS the attempt terminally as
	// `disputed` (terminal_reason "reversal_tombstone_precedes_success")
	// together with its receipt, and returns the SAME uniform 200 body as
	// every other disposition (LF95-C3: never rolled back to produce an
	// error code). The invariant this test protects - no SECOND posting -
	// still holds and is asserted below; only the status code and the new
	// `disputed` assertion are new.
	time.Sleep(b1ReplenishWait)
	deposit := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, depositRef, "", payments.OutcomeSucceeded, 4000, "EUR", "", false))
	deposit.Body.Close()
	if deposit.StatusCode != http.StatusOK {
		t.Fatalf("expected the uniform 200 (the tombstone collision is committed as disputed, not rolled back to an error), got %d", deposit.StatusCode)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, depositRef); got != 1 {
		t.Fatalf("expected still exactly one row (the tombstone, never a second deposit posting): got %d", got)
	}
	var state, terminalReason string
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT state, terminal_reason FROM payment_attempts WHERE provider_id = 'mock' AND provider_reference = $1`,
			depositRef,
		).Scan(&state, &terminalReason)
	}); err != nil {
		t.Fatalf("query attempt state: %v", err)
	}
	if state != "disputed" || terminalReason != "reversal_tombstone_precedes_success" {
		t.Fatalf("expected the attempt to be disputed with terminal_reason 'reversal_tombstone_precedes_success', got state=%q terminal_reason=%q", state, terminalReason)
	}

	assertLedgerBalancedAndReconciled(t, pool, tenant.ID)
}
