//go:build integration

// Licensing enforcement slice 3, G4: LaunchGame and the launch-token bootstrap
// are gated by tenant and brand status, like postBet. A launch on a tenant or
// brand that is not 'active' mints no session or token, and (via the HTTP
// handler, see internal/httpserver) creates no wallet. Demo mode is gated too
// (it still mints a wallet-bound session and the handler creates the wallet).
package casino

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func allowGuardCounting(calls *int) marketgate.Guard {
	return func(context.Context, pgx.Tx) (marketgate.Decision, error) {
		*calls++
		return marketgate.Decision{Allowed: true}, nil
	}
}

func statusDenialAudits(t *testing.T, f casinoFixture) int {
	t.Helper()
	return countAuditRows(t, testPool(t), f.tenantID, "casino.launch_denied_by_status")
}

func TestLaunchGame_NonActiveTenantOrBrandMintsNothing(t *testing.T) {
	type setter func(t *testing.T, f casinoFixture, status string)
	setTenant := func(t *testing.T, f casinoFixture, status string) {
		if status == "pending_launch" {
			if err := launchfix.ForcePending(context.Background(), t, testPool(t), f.tenantID, nil); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := launchfix.TrySetTenantStatus(context.Background(), f.tenantID, status); err != nil {
			t.Fatal(err)
		}
	}
	setBrand := func(t *testing.T, f casinoFixture, status string) {
		if status == "pending_launch" {
			if err := launchfix.ForcePending(context.Background(), t, testPool(t), f.tenantID, &f.brandID); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := launchfix.TrySetBrandStatus(context.Background(), f.tenantID, f.brandID, status); err != nil {
			t.Fatal(err)
		}
	}
	for scope, set := range map[string]setter{"tenant": setTenant, "brand": setBrand} {
		for _, status := range []string{"pending_launch", "suspended", "closed"} {
			for _, mode := range []GameMode{ModeReal, ModeDemo} {
				t.Run(scope+"/"+status+"/"+string(mode), func(t *testing.T) {
					orch, f, game, sessions := marketLaunchRig(t)
					set(t, f, status)
					calls := 0
					res, err := launchWith(t, orch, f, game.ID, mode, allowGuardCounting(&calls))
					if err != nil {
						t.Fatalf("a status denial is a result, not an error: %v", err)
					}
					if !res.Denied || res.DenialCode != DenialCodeTenantOrBrandNotActive || res.LaunchURL != "" {
						t.Fatalf("want tenant_or_brand_not_active denial, got %+v", res)
					}
					if calls != 0 {
						t.Fatalf("status gate must precede the market guard, guard ran %d times", calls)
					}
					if n := sessions(); n != 0 {
						t.Fatalf("a refused launch minted %d sessions", n)
					}
					if n := statusDenialAudits(t, f); n != 1 {
						t.Fatalf("want exactly 1 casino.launch_denied_by_status audit row, got %d", n)
					}
				})
			}
		}
	}
}

func TestLaunchGame_ActiveTenantAndBrandStillLaunch(t *testing.T) {
	orch, f, game, sessions := marketLaunchRig(t)
	calls := 0
	res, err := launchWith(t, orch, f, game.ID, ModeDemo, allowGuardCounting(&calls))
	if err != nil || res.Denied || sessions() != 1 || calls != 1 {
		t.Fatalf("active launch must work: %+v err=%v sessions=%d calls=%d", res, err, sessions(), calls)
	}
}

// A brand suspension racing a launch: the launch waits for the in-flight
// suspension (shared status lock) and is then refused.
func TestLaunchGame_BrandSuspensionRacingLaunchWinsAndNothingIsMinted(t *testing.T) {
	for i := 0; i < 5; i++ {
		orch, f, game, sessions := marketLaunchRig(t)
		held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
		var once sync.Once
		releaseOnce := func() { once.Do(func() { close(release) }) }
		t.Cleanup(releaseOnce)
		go func() {
			done <- launchfix.TrySetBrandStatusHolding(context.Background(), f.tenantID, f.brandID, "suspended", func() {
				close(held)
				<-release
			})
		}()
		select {
		case <-held:
		case err := <-done:
			t.Fatalf("suspension ended before the hold: %v", err)
		case <-time.After(20 * time.Second):
			t.Fatal("suspension never reached the hold")
		}
		type out struct {
			res LaunchGameResult
			err error
		}
		launched := make(chan out, 1)
		go func() {
			calls := 0
			r, err := launchWith(t, orch, f, game.ID, ModeReal, allowGuardCounting(&calls))
			launched <- out{r, err}
		}()
		select {
		case o := <-launched:
			t.Fatalf("the launch must wait for the in-flight suspension, finished early: %+v err=%v", o.res, o.err)
		case <-time.After(300 * time.Millisecond):
		}
		releaseOnce()
		if err := <-done; err != nil {
			t.Fatalf("suspension: %v", err)
		}
		o := <-launched
		if o.err != nil || !o.res.Denied || o.res.DenialCode != DenialCodeTenantOrBrandNotActive {
			t.Fatalf("launch must see the committed suspension: %+v err=%v", o.res, o.err)
		}
		if sessions() != 0 {
			t.Fatal("session minted for a suspended brand")
		}
	}
}

func TestBootstrapLaunch_TokenMintedBeforeSuspensionIsRefusedAfterIt(t *testing.T) {
	for _, scope := range []string{"tenant", "brand"} {
		t.Run(scope, func(t *testing.T) {
			m := newMarketRig(t)
			m.openMarket(t, "MT")
			game := seedGame(t, m.pool, "mock-casino", "EUR")
			enableGameForTenant(t, m.pool, m.f, game.ID)
			session, token := mintSessionForBootstrap(t, m.pool, m.f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
			want := scope + "_not_active"
			if scope == "tenant" {
				if err := launchfix.TrySetTenantStatus(context.Background(), m.f.tenantID, "suspended"); err != nil {
					t.Fatal(err)
				}
			} else if err := launchfix.TrySetBrandStatus(context.Background(), m.f.tenantID, m.f.brandID, "suspended"); err != nil {
				t.Fatal(err)
			}
			in := m.provider.BootstrapPayload(m.f.tenantID, token, "req-status-1", game.ProviderGameID, "EUR", "real")
			v := bootstrapVerified(t, m.orch, m.pool, m.f.tenantID, "mock-casino", in)
			result, err := m.orch.BootstrapLaunch(context.Background(), m.pool, m.f.tenantID, "mock-casino", v)
			if err != nil {
				t.Fatalf("a status denial is a result: %v", err)
			}
			if !result.Denied || result.DeniedReason != want {
				t.Fatalf("want Denied/%s, got %+v", want, result)
			}
			assertGateDenied(t, m.pool, m.f, session.ID, want)
		})
	}
}
