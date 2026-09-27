//go:build integration

// Stage 9 §20 - targeted load/concurrency re-verification for the four
// areas explicitly OUTSIDE ledger-finance's own financial-concurrency
// re-audit (internal/casino's stage9_concurrency_integration_test.go,
// internal/ledger, internal/wallet, internal/withdrawal, internal/payments):
// concurrent authentication, concurrent sportsbook catalogue reads,
// concurrent casino launch, and concurrent Back Office queue reads.
// Deliberately does NOT re-test concurrent DISTINCT-bet/wallet-race
// scenarios (internal/sportsbook's own TestPlaceBet_ConcurrentPlacements
// OnlySucceeds and internal/casino's Stage 9 suite already own that
// ground) - every test here is a "cheap, clean, read-or-independent-write"
// case: dozens of goroutines hitting a real httptest.Server backed by a
// real Postgres pool, sized to this codebase's actually-configured
// DatabaseMaxConns (internal/config/config.go's own Load() default/
// DATABASE_MAX_CONNS override - see stage9ConfiguredMaxConns's own doc
// comment for why this reads that value directly rather than inventing
// one), asserting correctness, absence of a hang (an explicit bounded
// wait, never relying on `go test`'s own overall timeout to surface a
// deadlock), and a rough wall-clock timing logged for a human to sanity-
// check against "clearly serialized" vs "genuinely concurrent".
//
// Follows this package's own established conventions exactly
// (testEnv/newTestServer/newSportsbookTestServer/newCasinoTestServer/
// postJSON/getJSON/decodeBody/mustCreateTenant/mustCreateBrand/
// mustCreateStaff/mustLoginStaff/mustSeedSportsbookSelection/
// mustSeedCasinoGame/mustEnableCasinoGameForTenant/
// mustEnableCasinoCapability/seedBonusCampaignForHTTP) - only where a
// helper would call t.Fatal/t.Fatalf from inside a spawned goroutine
// (unsafe per the testing package: FailNow must run on the test's own
// goroutine) does this file define a goroutine-safe raw variant instead
// (stage9RawPostJSON/stage9RawGetJSON), collecting results into slices the
// MAIN test goroutine alone asserts against.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
)

// stage9ConfiguredMaxConns mirrors internal/config/config.go's own Load()
// default (10) and its DATABASE_MAX_CONNS override EXACTLY, without
// calling config.Load() itself - Load() also requires DATABASE_URL/
// JWT_SIGNING_SECRET to be set and validates unrelated fields (access/
// refresh TTLs, sweep intervals) that have nothing to do with pool sizing
// and are not guaranteed to be set in this integration-test environment
// (which sets TEST_DATABASE_URL, not DATABASE_URL). Reading the SAME
// env var with the SAME default is "use the actual configured value",
// not "invent a pool size" - it is Load()'s own documented default,
// just resolved without its unrelated preconditions.
func stage9ConfiguredMaxConns(t *testing.T) int32 {
	t.Helper()
	if v := os.Getenv("DATABASE_MAX_CONNS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return int32(n)
		}
	}
	return 10
}

// stage9Pool connects with the actually-configured DatabaseMaxConns
// (stage9ConfiguredMaxConns), rather than testEnv's own fixed 5 - this
// file's whole point is observing behavior AT that real pool size under
// more concurrent callers than it has connections, so a smaller invented
// pool would understate real contention and a larger one would overstate
// it.
func stage9Pool(t *testing.T) *db.Pool {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	pool, err := db.Connect(context.Background(), url, stage9ConfiguredMaxConns(t), 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// stage9Server builds one server with every route this file exercises
// enabled (identity/auth, sportsbook catalogue, casino launch, bonus
// admin-read queue) and the Stage 9 §21 per-IP auth rate limiter
// DISABLED (AuthRateLimitPerMinute: -1). That limiter is security's own
// deliberate, reviewed control (ratelimit.go) - out of scope to test or
// tune here - but its very purpose (bounding concurrent unauthenticated
// login/register throughput from one apparent source IP) would otherwise
// directly shape, and confound, this file's login-concurrency
// measurements: httptest.Server's client always presents as 127.0.0.1, so
// every goroutine in this file's login tests shares ONE rate-limit
// bucket key regardless of which of dozens of distinct accounts it logs
// into. Disabling it here isolates what this file actually measures (DB-
// layer contention on login_attempts/sessions), not the separately-owned
// and separately-tested rate limiter.
func stage9Server(t *testing.T, pool *db.Pool, orchestrator *casino.Orchestrator) *httptest.Server {
	t.Helper()
	keys, err := auth.NewKeyRegistry("k1", map[string]string{"k1": testJWTSecret})
	if err != nil {
		t.Fatalf("failed to build key registry: %v", err)
	}
	issuer := auth.NewIssuer(keys, "platform-api-test", "platform-api-test")
	srv := httptest.NewServer(New(Deps{
		Logger:                    slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                        pool,
		AuthIssuer:                issuer,
		ServiceName:               "platform-api-test",
		AccessTokenTTL:            5 * time.Minute,
		RefreshTokenTTL:           time.Hour,
		SportsbookEnabled:         true,
		CasinoOrchestrator:        orchestrator,
		CasinoOutboundCredentials: casino.NewMockOutboundResolver(),
		PersonResolver:            identityresolution.NewMockPersonResolver(),
		AuthRateLimitPerMinute:    -1,
	}))
	t.Cleanup(srv.Close)
	return srv
}

// --- goroutine-safe raw HTTP helpers (never call t.Fatal - see this
// file's own top-of-file doc comment for why) ---

func stage9RawPostJSON(srv *httptest.Server, path, bearerToken string, body any) (*http.Response, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	return http.DefaultClient.Do(req)
}

func stage9RawGetJSON(srv *httptest.Server, path, bearerToken string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		return nil, err
	}
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	return http.DefaultClient.Do(req)
}

// stage9MinCeiling is this file's ORIGINAL fixed hang/deadlock-guard
// ceiling (CI-FLAKE-281). It is preserved as a floor, never lowered:
// stage9Ceiling only ever raises the bound above this value.
const stage9MinCeiling = 30 * time.Second

// stage9MaxCeiling bounds how far stage9Ceiling may scale up, so a
// genuine deadlock/pool-exhaustion still fails in a bounded, CI-reasonable
// time instead of only at `go test`'s own overall timeout.
const stage9MaxCeiling = 3 * time.Minute

// stage9SafetyFactor converts one SERIAL, uncontended calibration hash
// into a per-operation budget that accounts for shared-CPU degradation
// under real concurrency (n goroutines here, plus every other package
// `go test ./...` runs at the same time in CI). Derived from this file's
// own CI-FLAKE-281 investigation stress measurement (docs/plans/
// stage-10.3-planning/13-ci-flake-281-disposition.md): ~0.74s observed per
// login under heavy artificial contention against a typical uncontended
// Argon2id-64MiB hash of tens of milliseconds - roughly an order of
// magnitude, so 15x keeps comfortable margin without being unbounded.
const stage9SafetyFactor = 15

// stage9CalibrateArgon2Cost times ONE real call to auth.HashPassword -
// the exact function, with the exact configured Argon2id parameters
// (internal/auth/password.go's defaultArgon2Params), that every
// register/login in this file actually invokes - immediately before a
// burst, so the derived ceiling reflects the ACTUAL cost on the machine
// running the suite right now, never a hardcoded guess.
func stage9CalibrateArgon2Cost(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	if _, err := auth.HashPassword("stage9-calibration-password-not-a-real-account"); err != nil {
		t.Fatalf("calibrate argon2 cost: %v", err)
	}
	return time.Since(start)
}

// stage9Ceiling derives this file's stage9AwaitAll hang/deadlock-guard
// ceiling for n concurrent operations, scaling with the Argon2id cost
// measured on THIS run (stage9CalibrateArgon2Cost) rather than a fixed
// 30s guess that CI-FLAKE-281 found could be consumed up to ~74% by
// runner CPU contention alone, with no code defect. This changes ONLY the
// wall-clock hang-detection bound - concurrency counts, assertions and
// Argon2 parameters are all untouched (per CI-FLAKE-281 disposition:
// never weaken a threshold or remove concurrency/security coverage to
// make CI green). Floored at stage9MinCeiling (the original fixed value -
// never lowered) and capped at stage9MaxCeiling (so a genuine hang still
// fails in bounded time).
func stage9Ceiling(t *testing.T, n int) time.Duration {
	t.Helper()
	cost := stage9CalibrateArgon2Cost(t)
	scaled := time.Duration(int64(n)) * stage9SafetyFactor * cost
	if scaled < stage9MinCeiling {
		return stage9MinCeiling
	}
	if scaled > stage9MaxCeiling {
		return stage9MaxCeiling
	}
	return scaled
}

// stage9AwaitAll runs n goroutines (bodies indexed 0..n-1) and fails with
// a clear message if they have not ALL finished within timeout - the
// explicit, never-rely-on-go-test's-own-timeout deadlock check every test
// below uses. Returns the wall-clock elapsed for the caller to log.
func stage9AwaitAll(t *testing.T, timeout time.Duration, n int, body func(i int)) time.Duration {
	t.Helper()
	var wg sync.WaitGroup
	start := time.Now()
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			body(i)
		}(i)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return time.Since(start)
	case <-time.After(timeout):
		t.Fatalf("timed out after %s waiting for %d concurrent operations to finish - possible deadlock/pool exhaustion", timeout, n)
		return 0
	}
}

// --- 1. Concurrent authentication ------------------------------------

// stage9RegisterPlayerWithPassword registers a player with a CALLER-known
// password (unlike mustRegisterPlayer, which generates and discards its
// own) - this file's login tests need to log the same account back in -
// and returns its player_account id (read back via GET /v1/me, exactly
// like mustRegisterPlayer does) so the caller can later read that
// player's own sessions (sessions' own RLS - migration 0018 - has no
// tenant-wide admin SELECT policy at all, only "the token's own row",
// "a principal's own rows", or "one already-resolved internal-op row";
// there is deliberately no way to bulk-count a tenant's sessions via raw
// SQL, only per-principal via auth.ListActiveSessions/WithPrincipalScope,
// exactly like the real GET /v1/me/sessions endpoint does).
func stage9RegisterPlayerWithPassword(t *testing.T, srv *httptest.Server, brandSlug, email, password string) uuid.UUID {
	t.Helper()
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brandSlug, "email": email, "password": password,
	})
	if resp.StatusCode != http.StatusCreated {
		resp.Body.Close()
		t.Fatalf("failed to register player %s: status %d", email, resp.StatusCode)
	}
	var tokens tokenPairResponse
	decodeBody(t, resp, &tokens)

	meResp := getJSON(t, srv, "/v1/me", tokens.AccessToken)
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("failed to read registered player's own profile: status %d", meResp.StatusCode)
	}
	var me meResponse
	decodeBody(t, meResp, &me)
	id, err := uuid.Parse(me.ID)
	if err != nil {
		t.Fatalf("failed to parse player id: %v", err)
	}
	return id
}

// TestStage9_ConcurrentLogins_SameAccount_NoDeadlockAndCorrectSessionCount
// races N simultaneous logins for the exact SAME player account/identifier
// - the "same account" half of §20's auth requirement. Unlike a financial
// balance, a player account has no single mutable row a login serializes
// on (login_attempts is append-only INSERT, sessions has no per-account
// uniqueness constraint - the platform deliberately allows multiple
// concurrent sessions), so the expected result is EVERY concurrent login
// succeeding independently: no lock contention beyond ordinary pool-
// connection queuing, no lost session, no corrupted lockout count.
func TestStage9_ConcurrentLogins_SameAccount_NoDeadlockAndCorrectSessionCount(t *testing.T) {
	pool := stage9Pool(t)
	srv := stage9Server(t, pool, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	const email = "stage9-same-account@example.com"
	const password = "a-decent-password-1"
	playerID := stage9RegisterPlayerWithPassword(t, srv, brand.Slug, email, password)

	const n = 30
	statuses := make([]int, n)
	elapsed := stage9AwaitAll(t, stage9Ceiling(t, n), n, func(i int) {
		resp, err := stage9RawPostJSON(srv, "/v1/auth/login", "", map[string]string{
			"brand_slug": brand.Slug, "email": email, "password": password,
		})
		if err != nil {
			statuses[i] = -1
			return
		}
		defer resp.Body.Close()
		statuses[i] = resp.StatusCode
	})
	t.Logf("stage9 same-account concurrent logins: n=%d elapsed=%s", n, elapsed)

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("login %d: expected 200, got %d", i, s)
		}
	}

	var sessionCount int
	if err := pool.WithPrincipalScope(context.Background(), tenant.ID, playerID, func(ctx context.Context, tx pgx.Tx) error {
		sessions, err := auth.ListActiveSessions(ctx, tx, auth.PrincipalPlayer, playerID)
		sessionCount = len(sessions)
		return err
	}); err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	var succeededAttempts int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM login_attempts WHERE tenant_id = $1 AND succeeded = true`, tenant.ID,
		).Scan(&succeededAttempts)
	}); err != nil {
		t.Fatalf("verify login_attempts count: %v", err)
	}
	// n concurrent logins, plus the ONE session registration itself
	// already minted (POST /v1/auth/register returns a real token pair,
	// exactly like login does) - stage9RegisterPlayerWithPassword's own
	// GET /v1/me call reuses that session's access token rather than
	// minting a fresh one.
	if want := n + 1; sessionCount != want {
		t.Fatalf("expected exactly %d sessions (the registration's own plus one per concurrent login, none lost/duplicated), got %d", want, sessionCount)
	}
	if succeededAttempts != n {
		t.Fatalf("expected exactly %d succeeded login_attempts rows, got %d", n, succeededAttempts)
	}
}

// TestStage9_ConcurrentLogins_DifferentAccounts_NoPoolExhaustionOrHang is
// the "different accounts" half: N distinct players logging in at once,
// each its own identifier/lockout row, exercising the pool at ~3x its own
// configured size (stage9ConfiguredMaxConns) so real acquire-queuing
// happens, without any shared row for two callers to contend on.
func TestStage9_ConcurrentLogins_DifferentAccounts_NoPoolExhaustionOrHang(t *testing.T) {
	pool := stage9Pool(t)
	srv := stage9Server(t, pool, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	n := int(stage9ConfiguredMaxConns(t)) * 3
	if n < 24 {
		n = 24
	}
	const password = "a-decent-password-2"
	emails := make([]string, n)
	for i := 0; i < n; i++ {
		emails[i] = fmt.Sprintf("stage9-diff-account-%d-%s@example.com", i, uuid.NewString()[:8])
		stage9RegisterPlayerWithPassword(t, srv, brand.Slug, emails[i], password)
	}

	statuses := make([]int, n)
	elapsed := stage9AwaitAll(t, stage9Ceiling(t, n), n, func(i int) {
		resp, err := stage9RawPostJSON(srv, "/v1/auth/login", "", map[string]string{
			"brand_slug": brand.Slug, "email": emails[i], "password": password,
		})
		if err != nil {
			statuses[i] = -1
			return
		}
		defer resp.Body.Close()
		statuses[i] = resp.StatusCode
	})
	t.Logf("stage9 distinct-account concurrent logins: n=%d configuredMaxConns=%d elapsed=%s", n, stage9ConfiguredMaxConns(t), elapsed)

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("login %d (%s): expected 200, got %d", i, emails[i], s)
		}
	}
}

// --- 2. Concurrent sportsbook catalogue reads (cheap, clean case) ----

// TestStage9_ConcurrentCatalogueReads_ReadHeavyNoContention drives many
// concurrent, unauthenticated GET /v1/sportsbook/sports and GET
// /v1/sportsbook/events/{id} calls against one seeded catalogue -
// sb_sports/sb_competitions/sb_events/sb_markets/sb_selections carry no
// row lock on a plain SELECT, so this is the "should show zero
// contention" case §20 calls out explicitly: every read is independent,
// nothing here shares a row with anything else in this test, and the
// only expected serialization is ordinary connection-pool queuing once
// concurrency exceeds stage9ConfiguredMaxConns.
func TestStage9_ConcurrentCatalogueReads_ReadHeavyNoContention(t *testing.T) {
	pool := stage9Pool(t)
	srv := stage9Server(t, pool, nil)

	var eventID string
	selectionID := mustSeedSportsbookSelection(t, pool, 250, 100)
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT e.id::text FROM sb_events e
			JOIN sb_markets m ON m.event_id = e.id
			JOIN sb_selections sel ON sel.market_id = m.id
			WHERE sel.id = $1`, selectionID).Scan(&eventID)
	}); err != nil {
		t.Fatalf("resolve seeded event id: %v", err)
	}

	n := int(stage9ConfiguredMaxConns(t)) * 6
	if n < 60 {
		n = 60
	}
	statuses := make([]int, n)
	elapsed := stage9AwaitAll(t, stage9Ceiling(t, n), n, func(i int) {
		var resp *http.Response
		var err error
		if i%2 == 0 {
			resp, err = stage9RawGetJSON(srv, "/v1/sportsbook/sports", "")
		} else {
			resp, err = stage9RawGetJSON(srv, "/v1/sportsbook/events/"+eventID, "")
		}
		if err != nil {
			statuses[i] = -1
			return
		}
		defer resp.Body.Close()
		statuses[i] = resp.StatusCode
	})
	t.Logf("stage9 concurrent catalogue reads: n=%d elapsed=%s (%.2fms/op average)", n, elapsed, float64(elapsed.Milliseconds())/float64(n))

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("catalogue read %d: expected 200, got %d", i, s)
		}
	}
}

// --- 3. Concurrent casino launch --------------------------------------

// TestStage9_ConcurrentCasinoLaunch_DistinctPlayers_NoContention launches
// N distinct players' first-ever session for the same game at once. Each
// call's wallet.GetOrCreate/CreateLaunchSession touches only that
// player's OWN rows (a fresh wallet row keyed by that player_account_id,
// a fresh casino_launch_sessions row) - deliberately NOT the "same
// player, same wallet" race, which is wallet/ledger concurrency territory
// (ledger-finance's, exercised by internal/wallet's own concurrent
// GetOrCreate tests and this stage's casino money-path re-audit) and out
// of this file's scope.
func TestStage9_ConcurrentCasinoLaunch_DistinctPlayers_NoContention(t *testing.T) {
	pool := stage9Pool(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := stage9Server(t, pool, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	n := int(stage9ConfiguredMaxConns(t)) * 3
	if n < 24 {
		n = 24
	}
	tokens := make([]string, n)
	for i := 0; i < n; i++ {
		player := mustRegisterPlayer(t, srv, brand.Slug)
		mustActivatePlayer(t, pool, tenant.ID, player.ID)
		tokens[i] = player.Tokens.AccessToken
	}

	type launchResult struct {
		status    int
		sessionID string
	}
	results := make([]launchResult, n)
	elapsed := stage9AwaitAll(t, stage9Ceiling(t, n), n, func(i int) {
		resp, err := stage9RawPostJSON(srv, "/v1/me/casino/games/"+game.ID.String()+"/launch", tokens[i], map[string]string{
			"asset_code": "EUR", "mode": "real",
		})
		if err != nil {
			results[i] = launchResult{status: -1}
			return
		}
		defer resp.Body.Close()
		var out launchCasinoGameResponse
		if resp.StatusCode == http.StatusCreated {
			_ = json.NewDecoder(resp.Body).Decode(&out)
		}
		results[i] = launchResult{status: resp.StatusCode, sessionID: out.SessionID}
	})
	t.Logf("stage9 concurrent distinct-player casino launches: n=%d elapsed=%s", n, elapsed)

	seen := make(map[string]bool, n)
	for i, r := range results {
		if r.status != http.StatusCreated {
			t.Fatalf("launch %d: expected 201, got %d", i, r.status)
		}
		if r.sessionID == "" {
			t.Fatalf("launch %d: expected a non-empty session id", i)
		}
		if seen[r.sessionID] {
			t.Fatalf("launch %d: session id %s was minted more than once (duplicate/lost-update)", i, r.sessionID)
		}
		seen[r.sessionID] = true
	}

	var sessionRows int
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE tenant_id = $1`, tenant.ID).Scan(&sessionRows)
	}); err != nil {
		t.Fatalf("count casino_launch_sessions: %v", err)
	}
	if sessionRows != n {
		t.Fatalf("expected exactly %d casino_launch_sessions rows, got %d", n, sessionRows)
	}
}

// TestStage9_ConcurrentCasinoLaunch_SamePlayerManySessions launches N
// sessions concurrently for ONE already-wallet-funded player - the
// "many simultaneous session launches" case read literally, and the one
// genuine shared-state question in this file's casino-launch coverage
// that is NOT wallet-balance concurrency: does minting N independent,
// unrelated casino_launch_sessions rows for the same player serialize on
// anything (e.g. an accidental table-level or player-scoped lock), or do
// they proceed independently like the distinct-player case above. The
// wallet is pre-funded (via fundWallet, run once, sequentially, BEFORE
// the concurrent burst) specifically so wallet.GetOrCreate resolves an
// EXISTING row on every concurrent call instead of racing to CREATE one -
// that creation race is explicitly out of scope here (see the sibling
// test's own doc comment).
func TestStage9_ConcurrentCasinoLaunch_SamePlayerManySessions(t *testing.T) {
	pool := stage9Pool(t)
	orchestrator, _ := newMockCasinoOrchestrator()
	srv := stage9Server(t, pool, orchestrator)

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	n := int(stage9ConfiguredMaxConns(t)) * 2
	if n < 20 {
		n = 20
	}
	type launchResult struct {
		status    int
		sessionID string
	}
	results := make([]launchResult, n)
	elapsed := stage9AwaitAll(t, stage9Ceiling(t, n), n, func(i int) {
		resp, err := stage9RawPostJSON(srv, "/v1/me/casino/games/"+game.ID.String()+"/launch", player.Tokens.AccessToken, map[string]string{
			"asset_code": "EUR", "mode": "real",
		})
		if err != nil {
			results[i] = launchResult{status: -1}
			return
		}
		defer resp.Body.Close()
		var out launchCasinoGameResponse
		if resp.StatusCode == http.StatusCreated {
			_ = json.NewDecoder(resp.Body).Decode(&out)
		}
		results[i] = launchResult{status: resp.StatusCode, sessionID: out.SessionID}
	})
	t.Logf("stage9 concurrent same-player casino launches: n=%d elapsed=%s", n, elapsed)

	seen := make(map[string]bool, n)
	for i, r := range results {
		if r.status != http.StatusCreated {
			t.Fatalf("launch %d: expected 201, got %d", i, r.status)
		}
		if seen[r.sessionID] {
			t.Fatalf("launch %d: session id %s was minted more than once", i, r.sessionID)
		}
		seen[r.sessionID] = true
	}
}

// --- 4. Back Office queue reads under concurrent staff access --------

// TestStage9_ConcurrentBackOfficeQueueReads_PureReadsNoContention drives
// many concurrent staff GET /v1/admin/bonus/change-requests calls (the
// literal Back Office "change-request queue" - backoffice/src/features/
// bonus/ChangeRequestQueuePage.tsx's own backing endpoint) - a pure,
// tenant-scoped, permission-gated read with no write in this test at all
// once the fixture is seeded, so - like the catalogue case above - the
// only expected serialization is ordinary pool-connection queuing, never
// row-level contention.
func TestStage9_ConcurrentBackOfficeQueueReads_PureReadsNoContention(t *testing.T) {
	pool := stage9Pool(t)
	srv := stage9Server(t, pool, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	filer := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, "stage9-queue-filer-1")
	filerToken := mustLoginStaff(t, srv, tenant.Slug, filer.Email, "stage9-queue-filer-1")
	campaignID := seedBonusCampaignForHTTP(t, pool, tenant.ID, brand.ID, filer.ID)
	fileResp := postJSON(t, srv, "/v1/admin/bonus/change-requests", filerToken.AccessToken, map[string]any{
		"operation": "campaign_activate", "target_type": "bonus_campaigns", "target_id": campaignID.String(),
		"payload": map[string]string{"action": "activate"}, "reason_code": "stage9-load-test",
	})
	if fileResp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 filing the fixture change request, got %d", fileResp.StatusCode)
	}
	fileResp.Body.Close()

	const staffCount = 5
	const readsPerStaff = 12
	n := staffCount * readsPerStaff
	tokens := make([]string, staffCount)
	for i := 0; i < staffCount; i++ {
		staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRolePromotionsManager, fmt.Sprintf("stage9-queue-reader-%d", i))
		tokens[i] = mustLoginStaff(t, srv, tenant.Slug, staff.Email, fmt.Sprintf("stage9-queue-reader-%d", i)).AccessToken
	}

	statuses := make([]int, n)
	elapsed := stage9AwaitAll(t, stage9Ceiling(t, n), n, func(i int) {
		resp, err := stage9RawGetJSON(srv, "/v1/admin/bonus/change-requests", tokens[i%staffCount])
		if err != nil {
			statuses[i] = -1
			return
		}
		defer resp.Body.Close()
		statuses[i] = resp.StatusCode
	})
	t.Logf("stage9 concurrent Back Office queue reads: staff=%d readsPerStaff=%d n=%d elapsed=%s", staffCount, readsPerStaff, n, elapsed)

	for i, s := range statuses {
		if s != http.StatusOK {
			t.Fatalf("queue read %d: expected 200, got %d", i, s)
		}
	}
}
