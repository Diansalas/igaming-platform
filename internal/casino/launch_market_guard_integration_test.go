//go:build integration

// A8: LaunchGame's operating-market guard seam (LaunchGameParams.MarketGuard).
// The decision logic is internal/marketgate's; these tests pin WHERE LaunchGame
// evaluates it (first statement of phase A, before any catalogue read and
// before CreateLaunchSession can mint a session or token) and what it does
// with each verdict (a refusal is a Denied RESULT carrying the generic
// DenialCodeMarketNotAvailable so the guard's audit row commits; an error
// fails the launch closed).
package casino

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
)

func marketLaunchRig(t *testing.T) (*Orchestrator, casinoFixture, Game, countFn) {
	t.Helper()
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	game := seedGame(t, pool, "mock-casino", "EUR")
	enableGameForTenant(t, pool, f, game.ID)
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))
	sessions := func() int {
		var n int
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE tenant_id = $1`, f.tenantID).Scan(&n)
		}); err != nil {
			t.Fatalf("count sessions: %v", err)
		}
		return n
	}
	return orch, f, game, sessions
}

type countFn func() int

func launchWith(t *testing.T, orch *Orchestrator, f casinoFixture, gameID [16]byte, mode GameMode, guard marketgate.Guard) (LaunchGameResult, error) {
	t.Helper()
	return orch.LaunchGame(context.Background(), testPool(t), NewMockOutboundResolver(), LaunchGameParams{
		TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		GameID: gameID, AssetCode: "EUR", Mode: mode, MarketGuard: guard,
	})
}

func TestLaunchGame_MarketGuardRefusalIsADeniedResultWithNoSession(t *testing.T) {
	orch, f, game, sessions := marketLaunchRig(t)
	for _, mode := range []GameMode{ModeReal, ModeDemo} {
		calls := 0
		guard := func(context.Context, pgx.Tx) (marketgate.Decision, error) {
			calls++
			return marketgate.Decision{Allowed: false, Code: marketgate.ReasonNotPermitted}, nil
		}
		res, err := launchWith(t, orch, f, game.ID, mode, guard)
		if err != nil {
			t.Fatalf("%s: a refusal is a result, not an error: %v", mode, err)
		}
		if !res.Denied || res.DenialCode != DenialCodeMarketNotAvailable || res.LaunchURL != "" || res.SessionID.String() != "00000000-0000-0000-0000-000000000000" {
			t.Fatalf("%s: want a generic market denial with no session, got %+v", mode, res)
		}
		if calls != 1 {
			t.Fatalf("%s: guard ran %d times, want 1", mode, calls)
		}
	}
	if n := sessions(); n != 0 {
		t.Fatalf("a refused launch minted %d launch sessions/tokens", n)
	}
}

// The refusal precedes every catalogue read: an unknown game id gets the
// SAME refusal (not ErrGameNotFound), so a refused market learns nothing
// about which games exist.
func TestLaunchGame_MarketGuardRefusalPrecedesCatalogueChecks(t *testing.T) {
	orch, f, _, sessions := marketLaunchRig(t)
	guard := func(context.Context, pgx.Tx) (marketgate.Decision, error) {
		return marketgate.Decision{Allowed: false, Code: marketgate.ReasonEvidenceUnavailable}, nil
	}
	var unknownGame [16]byte
	unknownGame[0] = 0xAB
	res, err := launchWith(t, orch, f, unknownGame, ModeReal, guard)
	if err != nil || !res.Denied || res.DenialCode != DenialCodeMarketNotAvailable {
		t.Fatalf("want the generic market denial for an unknown game id, got %+v err=%v", res, err)
	}
	if sessions() != 0 {
		t.Fatal("no session may exist")
	}
}

func TestLaunchGame_MarketGuardErrorFailsClosed(t *testing.T) {
	orch, f, game, sessions := marketLaunchRig(t)
	boom := errors.New("policy read failed")
	guard := func(context.Context, pgx.Tx) (marketgate.Decision, error) {
		return marketgate.Decision{Allowed: true}, boom
	}
	res, err := launchWith(t, orch, f, game.ID, ModeReal, guard)
	if err == nil || !errors.Is(err, boom) {
		t.Fatalf("a guard error must fail the launch, got %+v err=%v", res, err)
	}
	if res.Denied || res.LaunchURL != "" {
		t.Fatalf("unexpected result %+v", res)
	}
	if sessions() != 0 {
		t.Fatal("a failed gate minted a session")
	}
}

func TestLaunchGame_MarketGuardAllowProceedsToMintTheSession(t *testing.T) {
	orch, f, game, sessions := marketLaunchRig(t)
	sawSessions := -1
	guard := func(ctx context.Context, tx pgx.Tx) (marketgate.Decision, error) {
		// First statement of phase A, inside the tenant transaction: no
		// session exists yet.
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM casino_launch_sessions WHERE tenant_id = $1`, f.tenantID).Scan(&sawSessions); err != nil {
			return marketgate.Decision{}, err
		}
		return marketgate.Decision{Allowed: true}, nil
	}
	res, err := launchWith(t, orch, f, game.ID, ModeReal, guard)
	if err != nil || res.Denied || res.LaunchURL == "" {
		t.Fatalf("want a minted launch, got %+v err=%v", res, err)
	}
	if sawSessions != 0 {
		t.Fatalf("the guard must run before any session exists, saw %d", sawSessions)
	}
	if sessions() != 1 {
		t.Fatalf("want exactly 1 session, got %d", sessions())
	}
}

// SR8-3: an empty guard REFUSES; the named sentinel is the only ungated path.
func TestLaunchGame_EmptyMarketGuardRefuses(t *testing.T) {
	orch, f, game, sessions := marketLaunchRig(t)
	res, err := launchWith(t, orch, f, game.ID, ModeDemo, nil)
	if !errors.Is(err, marketgate.ErrGuardRequired) || res.LaunchURL != "" {
		t.Fatalf("empty guard: want ErrGuardRequired, got %+v err=%v", res, err)
	}
	if sessions() != 0 {
		t.Fatal("an empty guard minted a session")
	}
}

func TestLaunchGame_NamedUngatedSentinelProceeds(t *testing.T) {
	orch, f, game, sessions := marketLaunchRig(t)
	res, err := launchWith(t, orch, f, game.ID, ModeDemo, marketgate.UngatedForTests)
	if err != nil || res.Denied || sessions() != 1 {
		t.Fatalf("named sentinel: want normal processing, got %+v err=%v sessions=%d", res, err, sessions())
	}
}
