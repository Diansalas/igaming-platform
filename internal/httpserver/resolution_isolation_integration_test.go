//go:build integration

// ADR 0094 §9.3 - the adversarial / concurrency suite for secret-resolution
// resource isolation (F-POOL-1), end to end through the real public
// webhook handlers with the REAL provider-credential resolver over the
// memory store, at the production pool size (10 connections, asserted by
// phasecapture.Pool10 - QA §11 item 1). Run with -race (QA §11 item 2).
//
// Kept in its own file so its CI cost can be timed separately (QA §11
// item 3). The tests marked [lane] in their doc comment run ALONE in the
// isolated CI step (security ruling (6)); the rest run in the main
// integration step.
//
// Every scenario ends with the global post-conditions: SUM(debits) ==
// SUM(credits) per tenant, every ledger account's projection equals its
// rebuild from ledger_entries (point-in-time reconciliation after the
// scenario - QA §11 item 7: this is NOT the scheduled hourly drift job,
// which ledger-finance's own suite owns), and 0 ledger/audit rows for any
// rejected callback's tenant.
package httpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providercred/providercredtest"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
	"github.com/Diansalas/igaming-platform/internal/txscope"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// isoBound is the reviewed "< 500 ms" bound (security review §5): it
// measures pool admission and is never widened.
const isoBound = 500 * time.Millisecond

// isoLongSlack is TestStoreOutage_DoesNotPinPool's longSlack (400 ms
// around the reviewed 250 ms): no pooled transaction of the pool under
// test may stay open longer than this while callers wait on the store.
const isoLongSlack = 400 * time.Millisecond

// isoClock is an optionally frozen clock for the Fetcher (Recovery test).
type isoClock struct {
	mu     sync.Mutex
	frozen bool
	now    time.Time
}

func (c *isoClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.frozen {
		return time.Now()
	}
	return c.now
}

func (c *isoClock) Freeze() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.frozen, c.now = true, time.Now()
}

func (c *isoClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type isoWorld struct {
	t          *testing.T
	pool       *db.Pool
	appName    string
	mem        *memstore.Store
	sub        *providercred.Subsystem
	clock      *isoClock
	casinoMock *casino.MockCasinoProvider
	payMock    *payments.MockProvider
	payOrch    *payments.Orchestrator
	srv        *httptest.Server
	logs       *syncBuffer
	principals providercredtest.Principals
	client     *http.Client

	storeCallsWithTx atomic.Int64
}

// isoPlayer is one funded player with its own wallet and casino session.
type isoPlayer struct {
	id, walletID, sessionID uuid.UUID
}

type isoTenant struct {
	tenant    identity.Tenant
	brandID   uuid.UUID
	player    registeredPlayer
	walletID  uuid.UUID
	sessionID uuid.UUID
	game      casino.Game
	casino    []byte // casino webhook secret
	pay       []byte // payments webhook secret
}

func newIsoWorld(t *testing.T) *isoWorld {
	t.Helper()
	pool, appName := phasecapture.Pool10Named(t, "TEST_DATABASE_URL")
	w := &isoWorld{t: t, pool: pool, appName: appName, mem: memstore.New(), clock: &isoClock{}, logs: &syncBuffer{},
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}}
	logger := slog.New(slog.NewJSONHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	router, err := memstore.NewRouter(w.mem)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	w.sub, err = providercred.New(config.Config{ProviderCredentialFingerprintKey: config.NewSecretValue(hex.EncodeToString(key))},
		router, providercred.WithLogger(logger), providercred.WithFetcherOptions(secretstore.WithClock(w.clock.Now)))
	if err != nil || w.sub == nil {
		t.Fatalf("subsystem: %v", err)
	}
	w.mem.OnCallCtx(func(ctx context.Context, _ string) {
		if txscope.Held(ctx) {
			w.storeCallsWithTx.Add(1)
		}
	})
	w.casinoMock = casino.NewMockCasinoProvider("mock-casino", "EUR")
	w.payMock = payments.NewMockProvider("mock-psp", "EUR")
	casinoOrch := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": w.casinoMock}, w.sub.Resolver("casino"))
	w.payOrch = payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock-psp": w.payMock}, w.sub.Resolver("payments"))
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": testJWTSecret})
	if err != nil {
		t.Fatal(err)
	}
	w.srv = httptest.NewServer(New(Deps{
		Logger: logger, DB: pool, AuthIssuer: auth.NewIssuer(keys, "platform-api-test", "platform-api-test"),
		ServiceName: "platform-api-test", AccessTokenTTL: 5 * time.Minute, RefreshTokenTTL: time.Hour,
		CasinoOrchestrator: casinoOrch, PaymentOrchestrator: w.payOrch, PaymentsOutboundCredentials: payments.MockCredentialResolver{},
		PersonResolver: identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(w.srv.Close)
	t.Cleanup(w.client.CloseIdleConnections)
	w.principals = providercredtest.SeedPrincipals(t, pool)
	return w
}

// newTenant seeds a tenant with an active, funded player, a casino session
// and both webhook credentials (casino + payments), all cold in the cache.
func (w *isoWorld) newTenant(fund int64) *isoTenant {
	t := w.t
	t.Helper()
	it := &isoTenant{tenant: mustCreateTenant(t, w.pool)}
	brand := mustCreateBrand(t, w.pool, it.tenant)
	it.brandID = brand.ID
	it.player = mustRegisterPlayer(t, w.srv, brand.Slug)
	mustActivatePlayer(t, w.pool, it.tenant.ID, it.player.ID)
	it.walletID = fundWallet(t, w.pool, it.tenant.ID, it.brandID, it.player.ID, "EUR", fund).ID
	declared := w.casinoMock.Capabilities()
	if err := w.pool.WithTenant(context.Background(), it.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := casino.WriteCapability(ctx, tx, w.casinoMock, it.tenant.ID, nil, casino.CapabilityConfig{
			SupportsCatalogue: declared.SupportsCatalogue, SupportsLaunch: declared.SupportsLaunch, SupportsBalance: declared.SupportsBalance,
			SupportsBet: declared.SupportsBet, SupportsWin: declared.SupportsWin, SupportsRollback: declared.SupportsRollback,
			SupportedAssets: declared.SupportedAssets, SupportedGameTypes: declared.SupportedGameTypes,
			Priority: 100, Status: casino.CapabilityActive,
		})
		return err
	}); err != nil {
		t.Fatalf("casino capability: %v", err)
	}
	mustRegisterCapability(t, w.pool, it.tenant.ID, w.payMock)
	it.game = mustSeedCasinoGame(t, w.pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, w.pool, it.tenant.ID, it.game.ID)
	it.sessionID = mustMintCasinoLaunchSessionDirect(t, w.pool, it.tenant, it.brandID, it.player.ID, it.walletID, it.game.ID, "mock-casino", it.game.ProviderGameID, "EUR")
	_, it.casino = providercredtest.Register(t, w.pool, w.sub, w.mem.Put, w.principals, providercredtest.Spec{
		TenantID: it.tenant.ID, Domain: "casino", ProviderID: "mock-casino", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	_, it.pay = providercredtest.Register(t, w.pool, w.sub, w.mem.Put, w.principals, providercredtest.Spec{
		TenantID: it.tenant.ID, Domain: "payments", ProviderID: "mock-psp", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	return it
}

func (w *isoWorld) ns(it *isoTenant) string { return memstore.Namespace(it.tenant.ID.String()) }

// main is it's registered player.
func (it *isoTenant) main() isoPlayer {
	return isoPlayer{id: it.player.ID, walletID: it.walletID, sessionID: it.sessionID}
}

// extraPlayer seeds another active, funded player of it directly in the
// database (no HTTP registration), with its own wallet and casino
// session, so concurrent callbacks need not serialize on one wallet.
func (w *isoWorld) extraPlayer(it *isoTenant, fund int64) isoPlayer {
	t := w.t
	t.Helper()
	p := isoPlayer{id: uuid.New()}
	person := uuid.New()
	if err := w.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person)
		return err
	}); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if err := w.pool.WithTenant(context.Background(), it.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			VALUES ($1, $2, $3, $4, $5, 'x', 'active')`, p.id, it.tenant.ID, it.brandID, person, p.id.String()+"@iso.example")
		return err
	}); err != nil {
		t.Fatalf("seed player: %v", err)
	}
	p.walletID = fundWallet(t, w.pool, it.tenant.ID, it.brandID, p.id, "EUR", fund).ID
	p.sessionID = mustMintCasinoLaunchSessionDirect(t, w.pool, it.tenant, it.brandID, p.id, p.walletID, it.game.ID, "mock-casino", it.game.ProviderGameID, "EUR")
	return p
}

// casinoEvent builds a casino callback signed with key (it's own key
// unless the test says otherwise).
func (w *isoWorld) casinoEvent(it *isoTenant, key []byte, ev casino.CallbackEventType, ref, original, round string, amount int64) webhookauth.Inbound {
	return w.casinoEventFor(it, it.main(), key, ev, ref, original, round, amount)
}

func (w *isoWorld) casinoEventFor(it *isoTenant, p isoPlayer, key []byte, ev casino.CallbackEventType, ref, original, round string, amount int64) webhookauth.Inbound {
	outcome := casino.OutcomeSucceeded
	in := w.casinoMock.CallbackPayload(it.tenant.ID, ev, ref, original, round, it.game.ProviderGameID, amount, "EUR", outcome, "", p.id, p.sessionID)
	in.Header = in.Header.Clone()
	webhookauth.CasinoScheme().SetHeaders(in.Header, webhookauth.MockKeyID,
		webhookauth.CasinoScheme().Sign(key, it.tenant.ID, "mock-casino", webhookauth.MockKeyID, in.Body))
	return in
}

// deposit creates a pending intent (with a matching payment_attempts row -
// PRH-payments-callback-cutover, ADR 0095 §6.1: the callback path now
// resolves every deposit event through payment_attempts, INV-IO-14) and
// returns its signed success callback.
func (w *isoWorld) deposit(it *isoTenant, amount int64) payments.InboundCallback {
	w.t.Helper()
	return w.depositFor(it, it.main(), amount)
}

func (w *isoWorld) depositFor(it *isoTenant, p isoPlayer, amount int64) payments.InboundCallback {
	w.t.Helper()
	// PROV-OUTBOUND-CRED-1-LEGACY-PATH (E2): the legacy InitiateDeposit/
	// attemptDeposit chain is gone. InitiateDepositAttempt (the live v2
	// entry point) creates the matching payment_attempts row itself as
	// part of its own T1+T2/T4 transitions - no need to hand-drive
	// InsertSubmittingAttempt/MarkAccepted separately any more. A fresh
	// isoTenant has no licence bound, so KYCEnforcementDepositGate is a
	// structural not_required pass here, exactly like the real deposit
	// handler wires it (deposit_handlers.go) - this test is about
	// pool/tenant isolation under the resolver, not about the deposit
	// state machine itself.
	res, err := w.payOrch.InitiateDepositAttempt(context.Background(), w.pool, payments.KYCEnforcementDepositGate{}, payments.MockCredentialResolver{}, payments.InitiateDepositParams{
		Scope:     payments.DepositScope{TenantID: it.tenant.ID, BrandID: it.brandID, PlayerAccountID: p.id, WalletID: p.walletID},
		AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: "iso-" + uuid.NewString(),
	})
	if err != nil {
		w.t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.Attempt.ProviderReference == nil || *res.Attempt.ProviderReference == "" {
		w.t.Fatalf("expected InitiateDepositAttempt to set a provider_reference, got %+v", res.Attempt)
	}
	providerRef := *res.Attempt.ProviderReference
	in := w.payMock.CallbackPayload(it.tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, amount, "EUR", "", false)
	in.Header = in.Header.Clone()
	webhookauth.PaymentsScheme().SetHeaders(in.Header, webhookauth.MockKeyID,
		webhookauth.PaymentsScheme().Sign(it.pay, it.tenant.ID, "mock-psp", webhookauth.MockKeyID, in.Body))
	return in
}

type isoResult struct {
	status  int
	latency time.Duration
	err     error
}

// post delivers in to the public webhook route; safe from any goroutine.
func (w *isoWorld) post(domain string, it *isoTenant, provider string, in webhookauth.Inbound) isoResult {
	req, err := http.NewRequest(http.MethodPost, w.srv.URL+"/v1/webhooks/"+domain+"/"+it.tenant.Slug+"/"+provider, bytes.NewReader(in.Body))
	if err != nil {
		return isoResult{err: err}
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range in.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	start := time.Now()
	resp, err := w.client.Do(req)
	if err != nil {
		return isoResult{err: err, latency: time.Since(start)}
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return isoResult{status: resp.StatusCode, latency: time.Since(start)}
}

func (w *isoWorld) bet(it *isoTenant, ref string, amount int64) isoResult {
	return w.post("casino", it, "mock-casino", w.casinoEvent(it, it.casino, casino.CallbackEventBet, ref, "", "round-"+ref, amount))
}

func okStatus(s int) bool { return s >= 200 && s < 300 }

func (w *isoWorld) wantOK(what string, r isoResult) {
	w.t.Helper()
	if r.err != nil || !okStatus(r.status) {
		w.t.Fatalf("%s: status %d err %v", what, r.status, r.err)
	}
	if r.latency >= isoBound {
		w.t.Fatalf("%s took %s, want < %s", what, r.latency, isoBound)
	}
}

// wantStatusOK checks outcome only - no latency bound. Main-lane tests use
// it: wall-clock bounds are admitted only in the isolated timing lane
// (security ruling (6); code review 18, R-1).
func (w *isoWorld) wantStatusOK(what string, r isoResult) {
	w.t.Helper()
	if r.err != nil || !okStatus(r.status) {
		w.t.Fatalf("%s: status %d err %v", what, r.status, r.err)
	}
}

// unrelatedQuery times an unrelated tenant's WithTenant + SELECT 1.
func (w *isoWorld) unrelatedQuery(tenant uuid.UUID) time.Duration {
	start := time.Now()
	if err := w.pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		var one int
		return tx.QueryRow(ctx, `SELECT 1`).Scan(&one)
	}); err != nil {
		w.t.Fatal(err)
	}
	return time.Since(start)
}

// watchTx starts sampling the oldest open transaction of this world's pool
// (pg_stat_activity, application_name): ADR 0094's "0 transactions longer
// than longSlack" criterion on the real handler path.
func (w *isoWorld) watchTx() *phasecapture.XactAgeSampler {
	return phasecapture.StartXactAgeSampler(w.t, "TEST_DATABASE_URL", w.appName)
}

// assertNoLongTx fails if any transaction of the pool stayed open longer
// than isoLongSlack while it was watched.
func (w *isoWorld) assertNoLongTx(s *phasecapture.XactAgeSampler) {
	w.t.Helper()
	s.Stop()
	age, err := s.MaxAge()
	if err != nil {
		w.t.Fatalf("pg_stat_activity sampler: %v", err)
	}
	if age > isoLongSlack {
		w.t.Fatalf("a pooled transaction stayed open %s (> %s) while callers waited on the store: a connection is held across the store wait (INV-POOL)", age, isoLongSlack)
	}
	w.t.Logf("oldest pooled transaction observed: %s", age)
}

func (w *isoWorld) reasonCount(reason webhookauth.Reason) int {
	return strings.Count(w.logs.String(), `"reason":"`+string(reason)+`"`)
}

func (w *isoWorld) ledgerTxCount(it *isoTenant) int {
	w.t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), it.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id IN ('mock-casino', 'mock-psp')`).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

func (w *isoWorld) auditCount(it *isoTenant) int {
	w.t.Helper()
	var n int
	if err := w.pool.WithTenant(context.Background(), it.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action LIKE 'casino_%' OR action LIKE 'payments.%'`).Scan(&n)
	}); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// postConditions is the suite's global post-condition set (see the file
// comment), plus INV-POOL: no store call ever ran with a transaction held.
func (w *isoWorld) postConditions(tenants ...*isoTenant) {
	w.t.Helper()
	if n := w.storeCallsWithTx.Load(); n != 0 {
		w.t.Fatalf("%d secret-store calls ran with a pooled transaction held (INV-POOL)", n)
	}
	if n := w.pool.Raw().Stat().AcquiredConns(); n > phasecapture.PoolSize {
		w.t.Fatalf("%d acquired connections > pool size", n)
	}
	for _, it := range tenants {
		if err := w.pool.WithTenant(context.Background(), it.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			var debits, credits int64
			if err := tx.QueryRow(ctx, `SELECT
				COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
				COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
				FROM ledger_entries WHERE tenant_id = $1`, it.tenant.ID).Scan(&debits, &credits); err != nil {
				return err
			}
			if debits != credits {
				return fmt.Errorf("tenant %s: SUM(debits)=%d != SUM(credits)=%d", it.tenant.Slug, debits, credits)
			}
			rows, err := tx.Query(ctx, `SELECT id FROM ledger_accounts WHERE tenant_id = $1`, it.tenant.ID)
			if err != nil {
				return err
			}
			var ids []uuid.UUID
			for rows.Next() {
				var id uuid.UUID
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return err
				}
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				proj, err := ledger.GetProjectedBalance(ctx, tx, id)
				if err != nil {
					return err
				}
				rebuilt, err := ledger.RebuildBalance(ctx, tx, id)
				if err != nil {
					return err
				}
				if proj.DebitTotal != rebuilt.DebitTotal || proj.CreditTotal != rebuilt.CreditTotal {
					return fmt.Errorf("tenant %s account %s: projection %d/%d != rebuild %d/%d",
						it.tenant.Slug, id, proj.DebitTotal, proj.CreditTotal, rebuilt.DebitTotal, rebuilt.CreditTotal)
				}
			}
			return nil
		}); err != nil {
			w.t.Fatal(err)
		}
	}
}

// burst sends n casino bets for it from conc goroutines and returns the
// results.
func (w *isoWorld) burst(it *isoTenant, n, conc int, prefix string) []isoResult {
	out := make([]isoResult, n)
	var next atomic.Int64
	var wg sync.WaitGroup
	for g := 0; g < conc; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= n {
					return
				}
				out[i] = w.bet(it, fmt.Sprintf("%s-%d", prefix, i), 10)
			}
		}()
	}
	wg.Wait()
	return out
}

func allRejected(t *testing.T, what string, rs []isoResult) {
	t.Helper()
	for i, r := range rs {
		if r.err != nil || r.status != http.StatusUnauthorized {
			t.Fatalf("%s[%d]: status %d err %v, want the uniform 401", what, i, r.status, r.err)
		}
	}
}

// TestResolutionIsolation_NormalOperation [lane] is ADR 0094 §9.3 test 1:
// 3 tenants x 30 concurrent signed callbacks (per tenant 10 payments
// deposits and 10 casino bet->win pairs, 10 in flight per tenant), cold
// then warm: all verified and posted exactly once, each in < 500 ms, at
// most one store call per (tenant, ref); redeliveries afterwards post
// nothing. Extended by
// security condition C8: one tenant with 4 distinct cold refs requested
// concurrently at 150 ms store latency sees 0 rejections.
func TestResolutionIsolation_NormalOperation(t *testing.T) {
	w := newIsoWorld(t)
	tenants := []*isoTenant{w.newTenant(100_000), w.newTenant(100_000), w.newTenant(100_000)}
	calls := w.mem.Calls()
	// Each (tenant, i) uses its own player and wallet: this test measures
	// pool admission, not the (intended) serialization of postings on one
	// wallet's projection lock.
	players := map[*isoTenant][]isoPlayer{}
	deposits := map[*isoTenant][]payments.InboundCallback{}
	for _, it := range tenants {
		for i := 0; i < 10; i++ {
			p := w.extraPlayer(it, 1_000)
			players[it] = append(players[it], p)
			deposits[it] = append(deposits[it], w.depositFor(it, p, 100))
		}
	}
	var mu sync.Mutex
	var failures []string
	var worst time.Duration
	note := func(r isoResult) {
		mu.Lock()
		if r.latency > worst {
			worst = r.latency
		}
		mu.Unlock()
	}
	// 30 callbacks per tenant (10 bet->win pairs + 10 deposits), at most
	// 10 in flight per tenant (30 across the 3 tenants). Measured: with all
	// 60 chains in flight at once, -race p100 was 400-590 ms at pool 10 -
	// pure pool throughput (about 4 short transactions per callback), not a
	// resolution effect - so the reviewed 500 ms bound would measure CPU
	// saturation instead of admission (ADR 0094 implementation record).
	var wg sync.WaitGroup
	for _, it := range tenants {
		inflight := make(chan struct{}, 10)
		for i := 0; i < 10; i++ {
			wg.Add(2)
			go func() {
				defer wg.Done()
				inflight <- struct{}{}
				defer func() { <-inflight }()
				betRef := fmt.Sprintf("n-bet-%s-%d", it.tenant.Slug, i)
				p := players[it][i]
				r := w.post("casino", it, "mock-casino", w.casinoEventFor(it, p, it.casino, casino.CallbackEventBet, betRef, "", "round-"+betRef, 10))
				note(r)
				if r.err == nil && okStatus(r.status) {
					r = w.post("casino", it, "mock-casino", w.casinoEventFor(it, p, it.casino, casino.CallbackEventWin,
						fmt.Sprintf("n-win-%s-%d", it.tenant.Slug, i), "", "round-"+betRef, 20))
					note(r)
				}
				if r.err != nil || !okStatus(r.status) || r.latency >= isoBound {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("casino %s %d: %d %v %s", it.tenant.Slug, i, r.status, r.err, r.latency))
					mu.Unlock()
				}
			}()
			go func() {
				defer wg.Done()
				inflight <- struct{}{}
				defer func() { <-inflight }()
				r := w.post("payments", it, "mock-psp", deposits[it][i])
				note(r)
				if r.err != nil || !okStatus(r.status) || r.latency >= isoBound {
					mu.Lock()
					failures = append(failures, fmt.Sprintf("deposit %s %d: %d %v %s", it.tenant.Slug, i, r.status, r.err, r.latency))
					mu.Unlock()
				}
			}()
		}
	}
	wg.Wait()
	if len(failures) > 0 {
		t.Fatalf("normal operation failures: %v", failures)
	}
	t.Logf("worst callback latency under normal operation: %s", worst)
	// Redeliveries (warm, after the timed concurrent phase): idempotent.
	for _, it := range tenants {
		for i := 0; i < 10; i++ {
			if r := w.post("payments", it, "mock-psp", deposits[it][i]); r.err != nil || !okStatus(r.status) {
				t.Fatalf("deposit redelivery %s %d: %d %v", it.tenant.Slug, i, r.status, r.err)
			}
		}
	}
	if got := w.mem.Calls() - calls; got > int64(len(tenants)*2) {
		t.Fatalf("%d store calls for %d (tenant, ref) pairs: single-flight/cache must give at most one each", got, len(tenants)*2)
	}
	for _, it := range tenants {
		// 10 bets + 10 wins + 10 deposits, each posted exactly once (the
		// fixture funding is not a provider posting).
		if n := w.ledgerTxCount(it); n != 30 {
			t.Fatalf("tenant %s: %d provider ledger transactions, want exactly 30 (redeliveries must not post)", it.tenant.Slug, n)
		}
	}
	w.postConditions(tenants...)

	t.Run("C8: 4 distinct cold refs of one tenant at 150ms store latency", func(t *testing.T) {
		it := w.newTenant(1)
		var providers []string
		for i := 0; i < 4; i++ {
			p := fmt.Sprintf("c8-vendor-%d", i)
			providercredtest.Register(t, w.pool, w.sub, w.mem.Put, w.principals, providercredtest.Spec{
				TenantID: it.tenant.ID, Domain: "casino", ProviderID: p, Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
			})
			providers = append(providers, p)
		}
		w.mem.SetLatency(150 * time.Millisecond)
		defer w.mem.SetLatency(0)
		var rejected atomic.Int64
		var wg sync.WaitGroup
		for _, p := range providers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := w.sub.Resolver("casino").Resolve(context.Background(), w.pool, it.tenant.ID, p, webhookauth.MockKeyID, webhookauth.KeyFromHeader); err != nil {
					rejected.Add(1)
				}
			}()
		}
		wg.Wait()
		if n := rejected.Load(); n != 0 {
			t.Fatalf("%d of 4 cold refs rejected under healthy 150ms latency (P=%d)", n, secretstore.MaxConcurrentStoreCallsPerTenant)
		}
	})
}

// TestResolutionIsolation_OneTenantStoreOutage [lane] is ADR 0094 §9.3
// test 2: tenant A's store hangs; 50 A callbacks; concurrently B (cold)
// and C (warm) callbacks and an unrelated query all complete in < 500 ms,
// A holds at most P store calls, and no pooled connection is held while
// waiting on the store.
func TestResolutionIsolation_OneTenantStoreOutage(t *testing.T) {
	w := newIsoWorld(t)
	a, b, c := w.newTenant(100_000), w.newTenant(100_000), w.newTenant(100_000)
	unrelated := mustCreateTenant(t, w.pool)
	w.wantOK("C warm-up", w.bet(c, "o-c-warm", 10))
	w.mem.TrackMatching(w.ns(a))
	w.mem.BlockMatching(w.ns(a))
	defer w.mem.UnblockMatching(w.ns(a))
	aDeposits := []payments.InboundCallback{w.deposit(a, 100), w.deposit(a, 100)}
	before := w.reasonCount(webhookauth.ReasonCredentialStoreUnavailable)
	aLedger, aAudit := w.ledgerTxCount(a), w.auditCount(a)

	watch := w.watchTx()
	var aRes []isoResult
	var aDep [2]isoResult
	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); aRes = w.burst(a, 50, 50, "o-a") }()
	go func() { defer wg.Done(); aDep[0] = w.post("payments", a, "mock-psp", aDeposits[0]) }()
	go func() { defer wg.Done(); aDep[1] = w.post("payments", a, "mock-psp", aDeposits[1]) }()
	time.Sleep(300 * time.Millisecond)
	if d := w.unrelatedQuery(unrelated.ID); d >= isoBound {
		t.Fatalf("an unrelated tenant query took %s during A's outage, want < %s", d, isoBound)
	}
	w.wantOK("B cold bet during A's outage", w.bet(b, "o-b-1", 10))
	w.wantOK("B warm bet during A's outage", w.bet(b, "o-b-2", 10))
	w.wantOK("C warm bet during A's outage", w.bet(c, "o-c-1", 10))
	w.wantOK("B cold deposit during A's outage", w.post("payments", b, "mock-psp", w.deposit(b, 100)))
	wg.Wait()
	w.assertNoLongTx(watch)

	allRejected(t, "A casino", aRes)
	allRejected(t, "A payments", aDep[:])
	if got := w.reasonCount(webhookauth.ReasonCredentialStoreUnavailable) - before; got != 52 {
		t.Fatalf("%d credential_store_unavailable rejections, want exactly A's 52", got)
	}
	if m := w.mem.MaxConcurrentMatching(w.ns(a)); m > 2 {
		t.Fatalf("tenant A held %d concurrent store calls, per-tenant cap is 2", m)
	}
	if s := w.sub.Fetcher().BreakerState("memory", b.tenant.ID); s != "closed" {
		t.Fatalf("B's breaker = %s during A's outage", s)
	}
	if w.ledgerTxCount(a) != aLedger || w.auditCount(a) != aAudit {
		t.Fatal("A's rejected callbacks wrote ledger or audit rows")
	}
	w.postConditions(a, b, c)
}

// degrade gives it one counting store failure on its PAYMENTS ref (so its
// casino ref is not negative-cached): it becomes degraded.
func (w *isoWorld) degrade(it *isoTenant) {
	w.t.Helper()
	ref := w.handleRef(it, "payments")
	w.mem.FailRef(ref, secretstore.ClassUnavailable)
	if r := w.post("payments", it, "mock-psp", w.deposit(it, 100)); r.status != http.StatusUnauthorized {
		w.t.Fatalf("degrading %s: status %d", it.tenant.Slug, r.status)
	}
	w.mem.FailRef(ref, 0)
}

func (w *isoWorld) handleRef(it *isoTenant, domain string) string {
	w.t.Helper()
	var ref string
	if err := w.pool.WithTenant(context.Background(), it.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT secret_ref FROM provider_credential_handles WHERE tenant_id = $1 AND domain = $2 AND status = 'active'`,
			it.tenant.ID, domain).Scan(&ref)
	}); err != nil {
		w.t.Fatal(err)
	}
	return ref
}

// TestResolutionIsolation_MultipleTenantsOutage [lane] is ADR 0094 §9.3
// test 3: five degraded tenants, each blocked and bursting 50 callbacks,
// hold at most D = 2 store calls together; a healthy tenant's COLD
// callback and an unrelated query still complete in < 500 ms.
func TestResolutionIsolation_MultipleTenantsOutage(t *testing.T) {
	w := newIsoWorld(t)
	var as []*isoTenant
	for i := 0; i < 5; i++ {
		as = append(as, w.newTenant(100_000))
	}
	h := w.newTenant(100_000)
	unrelated := mustCreateTenant(t, w.pool)
	for _, a := range as {
		w.degrade(a)
		w.mem.TrackMatching(w.ns(a))
		w.mem.BlockMatching(w.ns(a))
	}
	defer func() {
		for _, a := range as {
			w.mem.UnblockMatching(w.ns(a))
		}
	}()
	watch := w.watchTx()
	var wg sync.WaitGroup
	res := make([][]isoResult, len(as))
	for i, a := range as {
		wg.Add(1)
		go func() { defer wg.Done(); res[i] = w.burst(a, 50, 25, fmt.Sprintf("m-a%d", i)) }()
	}
	time.Sleep(300 * time.Millisecond)
	w.wantOK("healthy cold bet during 5 degraded tenants' outage", w.bet(h, "m-h-1", 10))
	if d := w.unrelatedQuery(unrelated.ID); d >= isoBound {
		t.Fatalf("an unrelated tenant query took %s, want < %s", d, isoBound)
	}
	wg.Wait()
	w.assertNoLongTx(watch)
	var total int64
	for i, a := range as {
		allRejected(t, fmt.Sprintf("A%d", i), res[i])
		total += w.mem.MaxConcurrentMatching(w.ns(a))
	}
	if total > 2 {
		t.Fatalf("degraded tenants reached %d concurrent store calls in total (sum of per-tenant peaks), budget D is 2", total)
	}
	w.postConditions(append(as, h)...)
}

// TestResolutionIsolation_SimultaneousOnset_Bounded [lane] is ADR 0094
// §9.3 test 3b: five never-observed outages start at the same instant.
// At +50 ms a healthy tenant's cold callback either succeeds or fails
// (credential_store_unavailable - the disclosed onset residual), and
// either way within SlotWait + 150 ms, holding no connection; at +2.3 s it
// succeeds; a warm healthy callback always succeeds.
func TestResolutionIsolation_SimultaneousOnset_Bounded(t *testing.T) {
	w := newIsoWorld(t)
	var as []*isoTenant
	for i := 0; i < 5; i++ {
		as = append(as, w.newTenant(100_000))
	}
	h := w.newTenant(100_000)
	w.wantOK("H warm-up", w.bet(h, "s-h-warm", 10))
	for _, a := range as {
		w.mem.BlockMatching(w.ns(a))
	}
	defer func() {
		for _, a := range as {
			w.mem.UnblockMatching(w.ns(a))
		}
	}()
	var wg sync.WaitGroup
	for i, a := range as {
		wg.Add(1)
		go func() { defer wg.Done(); _ = w.burst(a, 10, 10, fmt.Sprintf("s-a%d", i)) }()
	}
	time.Sleep(50 * time.Millisecond)
	early := w.post("payments", h, "mock-psp", w.deposit(h, 100))
	if early.err != nil || (!okStatus(early.status) && early.status != http.StatusUnauthorized) {
		t.Fatalf("H's cold callback at onset: status %d err %v", early.status, early.err)
	}
	// Either outcome must be FAST: a healthy caller never waits longer than
	// SlotWait for admission, so it succeeds or fails within SlotWait+150ms.
	if early.latency > secretstore.SlotWait+150*time.Millisecond {
		t.Fatalf("H's cold callback at onset took %s (status %d), want <= SlotWait+150ms whatever the outcome", early.latency, early.status)
	}
	w.wantOK("H warm bet during the onset", w.bet(h, "s-h-1", 10))
	time.Sleep(2300*time.Millisecond - 50*time.Millisecond - early.latency)
	w.wantOK("H cold deposit after the onset window", w.post("payments", h, "mock-psp", w.deposit(h, 100)))
	wg.Wait()
	w.postConditions(append(as, h)...)
}

// TestResolutionIsolation_Recovery is ADR 0094 §9.3 test 4 (main lane;
// fake clock, no wall-clock bound): A's store fails, A's breaker opens,
// the store recovers, after the cooldown A's single probe succeeds and
// closes the breaker, A's next callback posts once, and a redelivery of a
// callback that failed during the outage posts exactly once. B is
// unaffected at every phase.
func TestResolutionIsolation_Recovery(t *testing.T) {
	w := newIsoWorld(t)
	a, b := w.newTenant(100_000), w.newTenant(100_000)
	w.clock.Freeze()
	ref := w.handleRef(a, "casino")
	w.mem.FailRef(ref, secretstore.ClassUnavailable)
	failed := w.casinoEvent(a, a.casino, casino.CallbackEventBet, "r-a-failed", "", "round-r-a-failed", 10)
	for i := 0; i < secretstore.BreakerTripThreshold; i++ {
		if r := w.post("casino", a, "mock-casino", failed); r.status != http.StatusUnauthorized {
			t.Fatalf("A during the outage: status %d", r.status)
		}
		w.clock.Advance(secretstore.NegativeTTLCounting + time.Second)
		if r := w.bet(b, fmt.Sprintf("r-b-%d", i), 10); r.err != nil || !okStatus(r.status) {
			t.Fatalf("B during A's outage: %d %v", r.status, r.err)
		}
	}
	if s := w.sub.Fetcher().BreakerState("memory", a.tenant.ID); s != "open" {
		t.Fatalf("A's breaker = %s after 3 counting failures, want open", s)
	}
	if s := w.sub.Fetcher().BreakerState("memory", b.tenant.ID); s != "closed" {
		t.Fatalf("B's breaker = %s", s)
	}
	w.mem.FailRef(ref, 0) // the store recovers
	calls := w.mem.Calls()
	if r := w.post("casino", a, "mock-casino", failed); r.status != http.StatusUnauthorized || w.mem.Calls() != calls {
		t.Fatalf("while A's breaker is open: status %d, %d store calls", r.status, w.mem.Calls()-calls)
	}
	w.clock.Advance(secretstore.BreakerInitialCooldown)
	if r := w.bet(a, "r-a-after", 10); r.err != nil || !okStatus(r.status) {
		t.Fatalf("A's first callback after the cooldown (the probe): %d %v", r.status, r.err)
	}
	if s := w.sub.Fetcher().BreakerState("memory", a.tenant.ID); s != "closed" {
		t.Fatalf("a successful probe must close A's breaker, got %s", s)
	}
	for i := 0; i < 2; i++ {
		if r := w.post("casino", a, "mock-casino", failed); r.err != nil || !okStatus(r.status) {
			t.Fatalf("redelivery %d of the callback that failed during the outage: %d %v", i, r.status, r.err)
		}
	}
	var n int
	if err := w.pool.WithTenant(context.Background(), a.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'r-a-failed'`).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("the redelivered callback posted %d times, want exactly once", n)
	}
	if r := w.bet(b, "r-b-final", 10); r.err != nil || !okStatus(r.status) {
		t.Fatalf("B after A's recovery: %d %v", r.status, r.err)
	}
	w.postConditions(a, b)
}

// TestResolutionIsolation_ConnectionExhaustion [lane] is ADR 0094 §9.3
// test 5: during A's outage, 500 A callbacks from 100 goroutines plus B
// traffic: B's p100 < 500 ms with no acquire error, no connection held
// across the store wait, and goroutines return to baseline afterwards.
func TestResolutionIsolation_ConnectionExhaustion(t *testing.T) {
	w := newIsoWorld(t)
	a, b := w.newTenant(100_000), w.newTenant(100_000)
	w.wantOK("B warm-up", w.bet(b, "x-b-warm", 10))
	w.client.CloseIdleConnections()
	baseline := runtime.NumGoroutine()
	w.mem.BlockMatching(w.ns(a))
	defer w.mem.UnblockMatching(w.ns(a))
	watch := w.watchTx()
	var wg sync.WaitGroup
	var aRes []isoResult
	wg.Add(1)
	go func() { defer wg.Done(); aRes = w.burst(a, 500, 100, "x-a") }()
	time.Sleep(100 * time.Millisecond)
	var worst time.Duration
	for i := 0; i < 20; i++ {
		r := w.bet(b, fmt.Sprintf("x-b-%d", i), 10)
		w.wantOK(fmt.Sprintf("B bet %d during A's 500-callback burst", i), r)
		if r.latency > worst {
			worst = r.latency
		}
		time.Sleep(50 * time.Millisecond)
	}
	wg.Wait()
	w.assertNoLongTx(watch)
	allRejected(t, "A", aRes)
	w.client.CloseIdleConnections()
	deadline := time.Now().Add(5 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if g := runtime.NumGoroutine(); g > baseline+5 {
		t.Fatalf("goroutines %d after the burst, baseline %d: a leak", g, baseline)
	}
	t.Logf("B worst latency %s", worst)
	w.postConditions(a, b)
}

// TestResolutionIsolation_CrossTenant is ADR 0094 §9.3 test 6 (main
// lane; outcome assertions only, no wall-clock bound - code review 18,
// R-1): during A's outage, a body signed with B's warm key delivered to
// A's route, and A's key delivered to B's route, are both the uniform 401
// with 0 writes; A and B share provider id and key id with different
// secrets and each verifies only under its own tenant; B's traffic never
// reads or updates A's breaker or reaches A's store namespace.
func TestResolutionIsolation_CrossTenant(t *testing.T) {
	w := newIsoWorld(t)
	a, b := w.newTenant(100_000), w.newTenant(100_000)
	// (c) before the outage: equal (provider, key id), different secrets,
	// each verifies only for its own tenant.
	w.wantStatusOK("A own key", w.bet(a, "ct-a-own", 10))
	w.wantStatusOK("B own key", w.bet(b, "ct-b-own", 10))
	aLedger, bLedger := w.ledgerTxCount(a), w.ledgerTxCount(b)

	// Open A's own breaker: three counting failures on three fresh refs
	// of A (a negative-cache hit would not reach the store).
	for i := 0; i < secretstore.BreakerTripThreshold; i++ {
		p := fmt.Sprintf("ct-trip-%d", i)
		h, _ := providercredtest.Register(t, w.pool, w.sub, w.mem.Put, w.principals, providercredtest.Spec{
			TenantID: a.tenant.ID, Domain: "casino", ProviderID: p, Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
		})
		w.mem.FailRef(h.SecretRef, secretstore.ClassUnavailable)
		if _, err := w.sub.Resolver("casino").Resolve(context.Background(), w.pool, a.tenant.ID, p, webhookauth.MockKeyID, webhookauth.KeyFromHeader); !errors.Is(err, webhookauth.ErrCredentialStoreUnavailable) {
			t.Fatalf("tripping A's breaker: %v", err)
		}
	}
	w.mem.TrackMatching(w.ns(a))
	w.mem.BlockMatching(w.ns(a))
	defer w.mem.UnblockMatching(w.ns(a))
	aState := w.sub.Fetcher().BreakerState("memory", a.tenant.ID)
	if aState != "open" {
		t.Fatalf("A's breaker = %s, want open", aState)
	}
	aStoreCalls := w.mem.MaxConcurrentMatching(w.ns(a))

	// (a) A's route, body signed with B's key.
	in := w.casinoEvent(a, b.casino, casino.CallbackEventBet, "ct-a-with-b-key", "", "round-ct-x", 10)
	if r := w.post("casino", a, "mock-casino", in); r.status != http.StatusUnauthorized {
		t.Fatalf("A's route with B's key: status %d", r.status)
	}
	// (b) B's route, body signed with A's key (for B's tenant).
	in = w.casinoEvent(b, a.casino, casino.CallbackEventBet, "ct-b-with-a-key", "", "round-ct-y", 10)
	if r := w.post("casino", b, "mock-casino", in); r.status != http.StatusUnauthorized {
		t.Fatalf("B's route with A's key: status %d", r.status)
	}
	// B's own traffic - including a COLD fetch (B's payments ref) while
	// A's breaker is open - keeps working and never touches A's state.
	w.wantStatusOK("B cold deposit while A's breaker is open", w.post("payments", b, "mock-psp", w.deposit(b, 100)))
	for i := 0; i < 10; i++ {
		w.wantStatusOK("B during A's outage", w.bet(b, fmt.Sprintf("ct-b-%d", i), 10))
	}
	if s := w.sub.Fetcher().BreakerState("memory", a.tenant.ID); s != aState {
		t.Fatalf("B's requests changed A's breaker %s -> %s", aState, s)
	}
	if w.mem.MaxConcurrentMatching(w.ns(a)) != aStoreCalls {
		t.Fatal("B's requests reached A's store namespace")
	}
	if w.ledgerTxCount(a) != aLedger {
		t.Fatal("a cross-tenant callback wrote to A")
	}
	if n := w.ledgerTxCount(b); n != bLedger+11 {
		t.Fatalf("B has %d provider postings, want %d (its own 11 only)", n, bLedger+11)
	}
	w.postConditions(a, b)
}

// TestResolutionIsolation_FinancialDuringOutage [lane] is ADR 0094 §9.3
// test 7: while A's store is down and A is bursting, B's casino bet then
// win (cold, then warm), a B payment deposit, idempotent redelivery of
// each, and a B player wallet read all succeed in < 500 ms and post
// exactly once; A has no ledger or audit rows.
func TestResolutionIsolation_FinancialDuringOutage(t *testing.T) {
	w := newIsoWorld(t)
	a, b := w.newTenant(100_000), w.newTenant(100_000)
	w.mem.BlockMatching(w.ns(a))
	defer w.mem.UnblockMatching(w.ns(a))
	aLedger, aAudit := w.ledgerTxCount(a), w.auditCount(a)
	watch := w.watchTx()
	var wg sync.WaitGroup
	var aRes []isoResult
	wg.Add(1)
	go func() { defer wg.Done(); aRes = w.burst(a, 50, 50, "f-a") }()
	time.Sleep(200 * time.Millisecond)

	bet := w.casinoEvent(b, b.casino, casino.CallbackEventBet, "f-b-bet", "", "round-f-b", 1000)
	win := w.casinoEvent(b, b.casino, casino.CallbackEventWin, "f-b-win", "", "round-f-b", 2500)
	dep := w.deposit(b, 4000)
	w.wantOK("B cold bet", w.post("casino", b, "mock-casino", bet))
	w.wantOK("B warm win", w.post("casino", b, "mock-casino", win))
	w.wantOK("B cold deposit", w.post("payments", b, "mock-psp", dep))
	w.wantOK("B bet redelivery", w.post("casino", b, "mock-casino", bet))
	w.wantOK("B win redelivery", w.post("casino", b, "mock-casino", win))
	w.wantOK("B deposit redelivery", w.post("payments", b, "mock-psp", dep))
	start := time.Now()
	resp := getJSON(t, w.srv, "/v1/me/wallets/EUR", b.player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("B's wallet read: %d", resp.StatusCode)
	}
	var summary walletSummaryResponse
	decodeBody(t, resp, &summary)
	if d := time.Since(start); d >= isoBound {
		t.Fatalf("B's wallet read took %s, want < %s", d, isoBound)
	}
	if want := int64(100_000 - 1000 + 2500 + 4000); summary.CashBalance != want {
		t.Fatalf("B's cash balance %d, want %d (each posting exactly once)", summary.CashBalance, want)
	}
	if n := w.ledgerTxCount(b); n != 3 {
		t.Fatalf("B has %d provider postings, want exactly 3 (bet, win, deposit - redeliveries post nothing)", n)
	}
	wg.Wait()
	w.assertNoLongTx(watch)
	allRejected(t, "A", aRes)
	if w.ledgerTxCount(a) != aLedger || w.auditCount(a) != aAudit {
		t.Fatal("A's rejected callbacks wrote ledger or audit rows")
	}
	w.postConditions(a, b)
}

// TestSimulationHandlers_SessionRevalidatedInDomainTx is ADR 0094 §9.3
// test 11: the casino play simulation re-validates the session in the
// domain transaction (step 3), so a session that expires between
// verification and the domain transaction posts nothing.
func TestSimulationHandlers_SessionRevalidatedInDomainTx(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	simulationBetweenPhasesHook = func(sessionID uuid.UUID) {
		if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
			// status 'expired' (expires_at itself is immutable, migration
			// 0036): postBet rejects only a REVOKED session, so only the
			// step-3 re-validation can refuse this one.
			_, err := tx.Exec(ctx, `UPDATE casino_launch_sessions SET status = 'expired' WHERE id = $1`, sessionID)
			return err
		}); err != nil {
			t.Errorf("expire session between phases: %v", err)
		}
	}
	defer func() { simulationBetweenPhasesHook = nil }()
	resp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(500))
	defer func() { _ = resp.Body.Close() }()
	if okStatus(resp.StatusCode) {
		t.Fatalf("a session that expired between verification and the domain transaction must not post (status %d)", resp.StatusCode)
	}
	if n := countLedgerTransactions(t, pool, tenant.ID, ""); n != 0 {
		t.Fatalf("%d ledger transactions for a wager on a session that expired between the phases", n)
	}
}
