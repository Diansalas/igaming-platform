//go:build integration

// KYC-ENF-OUTAGE-1 end-to-end test (code review rv-prh-i3-code-review.md
// N1/MB1, "Re-review (FH-7)"'s own fault-injection probe: "a second
// connection holds LOCK TABLE kyc_verifications IN ACCESS EXCLUSIVE MODE;
// request tx lock_timeout = '300ms'"). Reproduces the exact scenario
// through the REAL HTTP handler, not a hand-constructed error value.
//
// Before the savepoint fix (internal/kyc.EvaluateEnforcement), a genuine
// Postgres error inside the KYC read aborted the caller's transaction, so
// this exact scenario reached a generic non-retryable 500
// (withdrawal_handlers.go's bottom `apierror.CodeInternal` branch), never
// the 503 branch the handler already had written for Outcome=unavailable
// (MB1: that branch existed in source but no test could ever make a real
// DB failure reach it). This test kills MB1 by disabling that 503 branch
// in source and confirming this test fails (verified manually during
// development; the branch itself is the fix under test).
//
// The HTTP layer's own connection pool has no per-request lock_timeout
// knob (the `SET LOCAL lock_timeout` internal/withdrawal's own
// fault-injection test uses is set INSIDE the transaction the withdrawal
// package opens, which an external HTTP client cannot reach) - so this
// test builds a SEPARATE, test-only *db.Pool whose every connection is
// opened with `lock_timeout=300ms` as a session default via the
// connection string's own `options` parameter (a per-connection startup
// GUC, not a role/database-level change - nothing shared is touched, and
// this pool is closed at the end of this test only).
package httpserver

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

// testEnvWithLockTimeout is testEnv (server_integration_test.go) with
// every connection opened with the given `lock_timeout` as a session
// default, via the connection string's `options=-c lock_timeout=...`
// parameter - a per-connection GUC set at startup by the Postgres wire
// protocol itself, not an ALTER ROLE/ALTER DATABASE that would affect any
// other connection, test, or session.
func testEnvWithLockTimeout(t *testing.T, lockTimeout string) *db.Pool {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse TEST_DATABASE_URL: %v", err)
	}
	// url.Values.Encode() escapes a space as "+", which pgx's own DSN
	// parser does not decode back to a space for the libpq `options`
	// parameter (it arrived at the server as the literal string
	// "+lock_timeout=...", an unrecognized GUC name) - and pgx's DSN
	// parser separately refuses an unescaped "=" inside a query value. Both
	// "=" and " " are percent-encoded explicitly (QueryEscape's "+" for
	// space is replaced with "%20") so the value survives both parses
	// intact.
	optionsValue := strings.ReplaceAll(url.QueryEscape("-c lock_timeout="+lockTimeout), "+", "%20")
	if u.RawQuery != "" {
		u.RawQuery += "&"
	}
	u.RawQuery += "options=" + optionsValue

	pool, err := db.Connect(context.Background(), u.String(), 5, 5*time.Second)
	if err != nil {
		t.Fatalf("failed to connect to test database with lock_timeout=%s: %v", lockTimeout, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestRequestWithdrawalHandler_KYCStoreOutageReturns503 proves the real
// HTTP handler maps a genuine KYC-store outage to a retryable 503, with
// exactly one committed `unavailable` decision row, one audit row, and no
// withdrawal_requests row or ledger posting.
func TestRequestWithdrawalHandler_KYCStoreOutageReturns503(t *testing.T) {
	// FK-3 (code review f-kyc-code-review.md, 2026-09-28): every fixture
	// below is seeded through the ORDINARY pool, never the lock_timeout
	// one. The lock_timeout pool is reserved for newFinancialTestServer
	// alone - the one thing this test actually needs a short lock_timeout
	// on is the HANDLER's own transaction, which is the only place the
	// KYC-store outage is being probed. Seeding fixtures through that same
	// pool would risk a fixture statement waiting behind an unrelated
	// lock (e.g. another package's own TRUNCATE on a shared CI database)
	// for longer than 300ms and failing with an unrelated 55P03, which
	// has nothing to do with what this test is actually proving.
	pool, issuer := testEnv(t)
	lockTimeoutPool := testEnvWithLockTimeout(t, "300ms")
	srv := newFinancialTestServer(t, lockTimeoutPool, issuer, nil)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	decisionsBefore := countRows(t, pool, tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`, tenant.ID, player.ID)
	auditBefore := countRows(t, pool, tenant.ID, `SELECT count(*) FROM audit_log WHERE action = 'kyc.enforcement_denied'`)

	instrument := pitest.Bind(t, pool, tenant.ID, player.ID, "EUR").String() // before the table lock
	release := lockKYCVerificationsTable(t, pool)
	defer release()

	resp := postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 500, "idempotency_key": uuid.NewString(), "payout_instrument_id": instrument,
	})
	release()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a genuine KYC-store outage, got %d", resp.StatusCode)
	}

	decisionsAfter := countRows(t, pool, tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2`, tenant.ID, player.ID)
	if decisionsAfter != decisionsBefore+1 {
		t.Fatalf("expected exactly 1 new kyc_enforcement_decisions row, got %d new", decisionsAfter-decisionsBefore)
	}
	unavailableCount := countRows(t, pool, tenant.ID, `SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2 AND outcome = 'unavailable'`, tenant.ID, player.ID)
	if unavailableCount != 1 {
		t.Fatalf("expected exactly 1 kyc_enforcement_decisions row with outcome='unavailable', got %d", unavailableCount)
	}
	// FK-4 (code review f-kyc-code-review.md): NOT asserted here on
	// purpose. kyc_enforcement_decisions has no `code` column at all
	// (migration 0100 stores outcome/allowed/matched_trigger/
	// policy_version/correlation_id only - EnforcementDecision.Code is an
	// in-memory value, never persisted), and the player-facing HTTP
	// response deliberately never exposes it either (security condition
	// 8: "player-facing surface: status only, never matched_trigger/
	// policy_version/an amount", withdrawal_handlers.go's own comment) -
	// there is no observable surface at this layer to assert Code
	// against. The code IS asserted directly, at the layer where it is
	// actually observable, by
	// TestRequestWithdrawal_KYCStoreOutage_FailsClosedWithOneUnavailableDecision
	// (internal/withdrawal) and the casino/sportsbook play-path outage
	// tests (§23.5), all three of which hold the real
	// EnforcementDecision/DeclineReason/RejectionCode value directly.
	auditAfter := countRows(t, pool, tenant.ID, `SELECT count(*) FROM audit_log WHERE action = 'kyc.enforcement_denied'`)
	if auditAfter != auditBefore+1 {
		t.Fatalf("expected exactly 1 new audit_log row, got %d new", auditAfter-auditBefore)
	}
	reqCount := countRows(t, pool, tenant.ID, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND player_account_id = $2`, tenant.ID, player.ID)
	if reqCount != 0 {
		t.Fatalf("expected 0 withdrawal_requests rows after a KYC-unavailable outcome, got %d", reqCount)
	}
	ledgerCount := countRows(t, pool, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'withdrawal_requested'`, tenant.ID)
	if ledgerCount != 0 {
		t.Fatalf("expected 0 withdrawal_requested ledger postings after a KYC-unavailable outcome, got %d", ledgerCount)
	}
}

// lockKYCVerificationsTable starts a blocker transaction on a SEPARATE
// pool connection that takes `LOCK TABLE kyc_verifications IN ACCESS
// EXCLUSIVE MODE` and holds it until the returned release() is called.
// Returning from the goroutine's LOCK TABLE statement itself proves the
// lock is held - there is no contention to wait out (nothing else holds
// it yet), so no polling is needed.
func lockKYCVerificationsTable(t *testing.T, pool *db.Pool) (release func()) {
	t.Helper()
	ready := make(chan error, 1)
	proceed := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `LOCK TABLE kyc_verifications IN ACCESS EXCLUSIVE MODE`); err != nil {
				ready <- err
				return err
			}
			ready <- nil
			<-proceed
			return nil
		})
	}()

	if err := <-ready; err != nil {
		t.Fatalf("blocker failed to acquire ACCESS EXCLUSIVE lock on kyc_verifications: %v", err)
	}

	var released bool
	return func() {
		if released {
			return
		}
		released = true
		close(proceed)
		if err := <-done; err != nil {
			t.Fatalf("blocker transaction failed: %v", err)
		}
	}
}
