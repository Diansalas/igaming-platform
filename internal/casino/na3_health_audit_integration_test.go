//go:build integration

// NA-3: CAS-HEALTH-FAILOPEN-1 (a provider HealthStatus error must fail
// closed, never count as healthy) and CAS-JURIS-AUDIT-1 (a jurisdiction
// denial of LaunchGame writes a casino.launch_denied_by_jurisdiction audit
// row, like the RG and risk denials).
package casino

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func na3CountAudit(t *testing.T, pool *db.Pool, tenantID uuid.UUID, action string) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenantID, action).Scan(&n)
	}); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

func na3CountLedger(t *testing.T, pool *db.Pool, tenantID uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count ledger entries: %v", err)
	}
	return n
}

func na3Launch(t *testing.T, pool *db.Pool, orch *Orchestrator, f casinoFixture, game Game, mode string) (LaunchGameResult, error) {
	t.Helper()
	return orch.LaunchGame(context.Background(), pool, NewMockOutboundResolver(), LaunchGameParams{MarketGuard: testUngatedGuard,
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: game.ID, AssetCode: "EUR", Mode: GameMode(mode),
	})
}

func TestNA3_HealthStatusError_FailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	provider.SetHealthError(errors.New("simulated health probe failure https://x.invalid/?token=SuperSecret"))
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()

	ledgerBefore := na3CountLedger(t, pool, f.tenantID)
	_, err := na3Launch(t, pool, orch, f, game, "real")
	if !errors.Is(err, ErrProviderUnavailable) {
		t.Fatalf("a HealthStatus error must fail closed with ErrProviderUnavailable, got %v", err)
	}
	assertSoleSessionRevoked(t, pool, f)
	if n := na3CountAudit(t, pool, f.tenantID, "casino.launch_failed"); n != 1 {
		t.Fatalf("expected exactly one casino.launch_failed row, got %d", n)
	}
	var meta []byte
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id=$1 AND action='casino.launch_failed'`, f.tenantID).Scan(&meta)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(meta), string(LaunchFailureHealthUnavailable)) || strings.Contains(string(meta), "SuperSecret") {
		t.Fatalf("unexpected audit metadata: %s", meta)
	}
	if got := na3CountLedger(t, pool, f.tenantID); got != ledgerBefore {
		t.Fatalf("health-denied launch posted to the ledger: %d -> %d", ledgerBefore, got)
	}

	// Recovery: once the probe succeeds again a fresh launch works (normal path unchanged).
	provider.SetHealthError(nil)
	res, err := na3Launch(t, pool, orch, f, game, "real")
	if err != nil || res.Denied || res.LaunchURL == "" {
		t.Fatalf("healthy provider launch must succeed: %+v %v", res, err)
	}
}

func TestNA3_HealthStatusError_ConcurrentLaunchesAllFailClosed(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	provider.SetHealthError(errors.New("down"))
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = na3Launch(t, pool, orch, f, game, "real")
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if !errors.Is(e, ErrProviderUnavailable) {
			t.Fatalf("launch %d: expected fail closed, got %v", i, e)
		}
	}
	if got := na3CountAudit(t, pool, f.tenantID, "casino.launch_failed"); got != n {
		t.Fatalf("expected %d launch_failed rows, got %d", n, got)
	}
	var active int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE tenant_id=$1 AND status='active'`, f.tenantID).Scan(&active)
	}); err != nil || active != 0 {
		t.Fatalf("no session may remain active after a fail-closed health denial: %d %v", active, err)
	}
}

func TestNA3_JurisdictionDenial_WritesOneAuditRow(t *testing.T) {
	for _, mode := range []string{"real", "demo"} {
		t.Run(mode, func(t *testing.T) {
			pool := testPool(t)
			f := seedCasinoFixture(t, pool)
			provider := NewMockCasinoProvider("mock-casino", "EUR")
			game := seedGameWithBlocklist(t, pool, "mock-casino", []string{"ANY-CODE"}, "EUR")
			enableGameForTenant(t, pool, f, game.ID)
			registerCasinoCapability(t, pool, f, provider, 100)
			orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()

			ledgerBefore := na3CountLedger(t, pool, f.tenantID)
			res, err := na3Launch(t, pool, orch, f, game, mode)
			if err != nil || !res.Denied || res.DenialCode != DenialCodeJurisdictionUnresolved {
				t.Fatalf("expected unresolved-jurisdiction denial, got %+v %v", res, err)
			}
			const action = "casino.launch_denied_by_jurisdiction"
			if n := na3CountAudit(t, pool, f.tenantID, action); n != 1 {
				t.Fatalf("expected exactly one %s row, got %d", action, n)
			}
			var (
				actorType, targetType, targetID, outcome string
				actorID                                  *uuid.UUID
				ip, ua                                   *string
				meta                                     []byte
			)
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT actor_type, actor_id, target_type, target_id, outcome, metadata, ip_address::text, user_agent
					FROM audit_log WHERE tenant_id=$1 AND action=$2`, f.tenantID, action).
					Scan(&actorType, &actorID, &targetType, &targetID, &outcome, &meta, &ip, &ua)
			}); err != nil {
				t.Fatal(err)
			}
			if actorType != "system" || actorID != nil || targetType != "player_account" || targetID != f.playerAccountID.String() || outcome != "denied" {
				t.Fatalf("unexpected audit shape: %s %v %s %s %s", actorType, actorID, targetType, targetID, outcome)
			}
			var m map[string]any
			if err := json.Unmarshal(meta, &m); err != nil {
				t.Fatal(err)
			}
			if m["reason_code"] != DenialCodeJurisdictionUnresolved || m["game_id"] != game.ID.String() || m["mode"] != mode || m["brand_id"] != f.brandID.String() {
				t.Fatalf("unexpected metadata: %s", meta)
			}
			for k := range m {
				switch k {
				case "reason_code", "brand_id", "provider_id", "game_id", "asset_code", "mode", "jurisdiction_code":
				default:
					t.Fatalf("unexpected metadata key %q (no-PII allowlist): %s", k, meta)
				}
			}
			if got := na3CountLedger(t, pool, f.tenantID); got != ledgerBefore {
				t.Fatalf("jurisdiction denial posted to the ledger")
			}
			// No session minted for a denied launch.
			var sessions int
			_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE tenant_id=$1`, f.tenantID).Scan(&sessions)
			})
			if sessions != 0 {
				t.Fatalf("denied launch minted %d sessions", sessions)
			}

			// RLS: another tenant cannot see this row.
			other := seedCasinoFixture(t, pool)
			var seen int
			if err := pool.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE action=$1 AND target_id=$2`, action, f.playerAccountID.String()).Scan(&seen)
			}); err != nil || seen != 0 {
				t.Fatalf("audit row visible across tenants: %d %v", seen, err)
			}
		})
	}
}

// A retried launch is a fresh attempt (same as the RG/risk denials): it is
// denied identically, mints nothing, and writes exactly one more row; N
// concurrent attempts write exactly N rows (no lost or merged rows).
func TestNA3_JurisdictionDenial_RetryAndConcurrency(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGameWithBlocklist(t, pool, "mock-casino", []string{"ANY-CODE"}, "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider)).WithUngatedMarketOpenForTests()
	const action = "casino.launch_denied_by_jurisdiction"

	for i := 1; i <= 2; i++ {
		res, err := na3Launch(t, pool, orch, f, game, "real")
		if err != nil || !res.Denied || res.DenialCode != DenialCodeJurisdictionUnresolved {
			t.Fatalf("retry %d: %+v %v", i, res, err)
		}
		if n := na3CountAudit(t, pool, f.tenantID, action); n != i {
			t.Fatalf("after attempt %d expected %d rows, got %d", i, i, n)
		}
	}
	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := na3Launch(t, pool, orch, f, game, "real")
			if err != nil || !res.Denied {
				t.Errorf("concurrent: %+v %v", res, err)
			}
		}()
	}
	wg.Wait()
	if got := na3CountAudit(t, pool, f.tenantID, action); got != 2+n {
		t.Fatalf("expected %d rows, got %d", 2+n, got)
	}
}
