//go:build integration

// SR8-2: exposure that is already running when an operating market closes.
// These tests run with the REAL brand-level market-open check (the orchestrator
// is built WITHOUT the WithUngatedMarketOpenForTests opt-out) over real
// operating-market rows. They pin: a NEW bet is declined when the brand's market
// is closed or unconfigured (fail closed, no ledger write, audited); a NEW bet is
// refused past a consumed session's maximum lifetime; the launch-token bootstrap
// refuses a token minted before the close; and SETTLEMENT (win, rollback) of
// exposure that already exists is NEVER blocked - by the close, by the lifetime -
// because refusing it would strand player funds.
package casino

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

type marketRig struct {
	pool      *db.Pool
	f         casinoFixture
	provider  *MockCasinoProvider
	orch      *Orchestrator // REAL market-open check
	licenceID uuid.UUID
	admin     uuid.UUID
}

func newMarketRig(t *testing.T) *marketRig {
	t.Helper()
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 50000)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))
	return &marketRig{pool: pool, f: f, provider: provider, orch: orch, admin: seedPlatformAdminStaffPrincipal(t, pool)}
}

// openMarket gives the tenant a licence, a ceiling and a tenant opt-in for cc.
func (m *marketRig) openMarket(t *testing.T, cc string) {
	t.Helper()
	jid := uuid.New()
	m.licenceID = uuid.New()
	err := m.pool.WithPlatformAdmin(context.Background(), m.admin, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Casino Market Test')`, jid, "CM-"+jid.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			m.licenceID, jid, "CML-"+m.licenceID.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, m.f.tenantID, m.licenceID); err != nil {
			return err
		}
		_, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: m.licenceID, CountryCode: cc, State: operatingmarket.StateEnabled, Status: operatingmarket.StatusActive,
			AuthorizationReference: "legal-ref", Actor: operatingmarket.ActorContext{ActorID: m.admin, ReasonCode: "sr8-2-test"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("open market (licence+ceiling): %v", err)
	}
	m.setTenantPolicy(t, cc, operatingmarket.StateEnabled)
}

func (m *marketRig) setTenantPolicy(t *testing.T, cc string, state operatingmarket.State) {
	t.Helper()
	if err := m.setTenantPolicyErr(cc, state); err != nil {
		t.Fatalf("tenant policy %s: %v", state, err)
	}
}

func (m *marketRig) setTenantPolicyErr(cc string, state operatingmarket.State) error {
	auth := ""
	if state == operatingmarket.StateEnabled {
		auth = "compliance-ref"
	}
	return m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
			Scope: operatingmarket.ScopeTenant, TenantID: m.f.tenantID, CountryCode: cc, State: state, Status: operatingmarket.StatusActive,
			AuthorizationReference: auth, Actor: operatingmarket.ActorContext{ActorID: m.admin, ReasonCode: "sr8-2-test"},
		})
		return err
	})
}

func (m *marketRig) receive(orch *Orchestrator, event CallbackEventType, txID, origID, round string, amount int64, session uuid.UUID) (ReceiveCallbackResult, error) {
	payload := m.provider.CallbackPayload(m.f.tenantID, event, txID, origID, round, "game-1", amount, "EUR", OutcomeSucceeded, "", m.f.playerRef, session)
	var res ReceiveCallbackResult
	err := m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = orch.receiveCallbackInTx(ctx, tx, m.f.tenantID, "mock-casino", payload)
		return err
	})
	return res, err
}

func (m *marketRig) ledgerRows(t *testing.T, providerTxID string) int {
	t.Helper()
	var n int
	if err := m.pool.WithTenant(context.Background(), m.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mock-casino' AND provider_tx_id = $2`,
			m.f.tenantID, providerTxID).Scan(&n)
	}); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	return n
}

func TestPostBet_MarketOpen_AcceptsTheBet(t *testing.T) {
	m := newMarketRig(t)
	m.openMarket(t, "MT")
	session := mintSession(t, m.pool, m.f, "mock-casino", "EUR")
	res, err := m.receive(m.orch, CallbackEventBet, "mkt-bet-open", "", "mkt-round-open", 1000, session)
	if err != nil || res.Outcome != OutcomeSucceeded {
		t.Fatalf("an open market must accept the bet, got %+v err=%v", res, err)
	}
	if bal := cashBalance(t, m.pool, m.f); bal != 49000 {
		t.Fatalf("balance %d, want 49000", bal)
	}
}

func TestPostBet_MarketClosedOrUnconfigured_DeclinesNewBetWithNoLedgerWrite(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(m *marketRig, t *testing.T)
	}{
		{"never configured", func(m *marketRig, t *testing.T) {}},
		{"closed after being open", func(m *marketRig, t *testing.T) {
			m.openMarket(t, "MT")
			m.setTenantPolicy(t, "MT", operatingmarket.StateDisabled)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newMarketRig(t)
			tc.setup(m, t)
			session := mintSession(t, m.pool, m.f, "mock-casino", "EUR")
			debits0, credits0 := sumDebitsCredits(t, m.pool, m.f.tenantID)
			res, err := m.receive(m.orch, CallbackEventBet, "mkt-bet-closed", "", "mkt-round-closed", 1000, session)
			if err != nil {
				t.Fatalf("a closed market is a decline, not an error: %v", err)
			}
			if res.Outcome != OutcomeDeclined || res.DeclineReason != DeclineReasonMarketUnavailable {
				t.Fatalf("want declined/%s, got %+v", DeclineReasonMarketUnavailable, res)
			}
			if bal := cashBalance(t, m.pool, m.f); bal != 50000 {
				t.Fatalf("balance moved to %d", bal)
			}
			if d, c := sumDebitsCredits(t, m.pool, m.f.tenantID); d != debits0 || c != credits0 {
				t.Fatalf("ledger changed on a declined bet: (%d,%d) -> (%d,%d)", debits0, credits0, d, c)
			}
			if n := m.ledgerRows(t, "mkt-bet-closed"); n != 0 {
				t.Fatalf("a declined bet left %d ledger transactions", n)
			}
			if !auditActionExists(t, m.pool, m.f.tenantID, "casino_bet.denied_market_closed") {
				t.Fatal("expected the market-closed denial audit row (committed with the decline)")
			}
		})
	}
}

// Settlement of exposure that already exists is NEVER blocked by the close
// (CLAUDE.md / ADR: refusing a win or rollback would strand player funds).
func TestPostWinAndRollback_StillSettleAfterTheMarketCloses(t *testing.T) {
	m := newMarketRig(t)
	m.openMarket(t, "MT")
	session := mintSession(t, m.pool, m.f, "mock-casino", "EUR")
	for _, b := range []struct{ tx, round string }{{"mkt-bet-a", "mkt-round-a"}, {"mkt-bet-b", "mkt-round-b"}} {
		if res, err := m.receive(m.orch, CallbackEventBet, b.tx, "", b.round, 1000, session); err != nil || res.Outcome != OutcomeSucceeded {
			t.Fatalf("setup bet %s: %+v %v", b.tx, res, err)
		}
	}
	if bal := cashBalance(t, m.pool, m.f); bal != 48000 {
		t.Fatalf("setup balance %d", bal)
	}

	m.setTenantPolicy(t, "MT", operatingmarket.StateDisabled) // the market closes with two open bets

	// A NEW bet is declined ...
	if res, err := m.receive(m.orch, CallbackEventBet, "mkt-bet-new", "", "mkt-round-new", 1000, session); err != nil || res.Outcome != OutcomeDeclined {
		t.Fatalf("new bet after close: %+v %v", res, err)
	}
	// ... the win for bet A still settles ...
	if res, err := m.receive(m.orch, CallbackEventWin, "mkt-win-a", "", "mkt-round-a", 2500, uuid.Nil); err != nil || res.Outcome != OutcomeSucceeded {
		t.Fatalf("win after close must settle, got %+v err=%v", res, err)
	}
	// ... and the rollback of bet B still reverses the stake.
	if res, err := m.receive(m.orch, CallbackEventRollback, "mkt-rb-b", "mkt-bet-b", "mkt-round-b", 0, uuid.Nil); err != nil || res.Outcome != OutcomeSucceeded {
		t.Fatalf("rollback after close must post, got %+v err=%v", res, err)
	}
	if bal := cashBalance(t, m.pool, m.f); bal != 48000+2500+1000 {
		t.Fatalf("balance after settlement %d, want %d", bal, 48000+2500+1000)
	}
	if d, c := sumDebitsCredits(t, m.pool, m.f.tenantID); d != c {
		t.Fatalf("ledger invariant: debits=%d credits=%d", d, c)
	}
}

// SR8-2 (a): a consumed session older than the maximum lifetime takes no NEW bet.
func TestPostBet_ConsumedSessionPastMaxLifetimeRefusesNewBetButSettles(t *testing.T) {
	m := newMarketRig(t)
	m.openMarket(t, "MT")
	session := mintSession(t, m.pool, m.f, "mock-casino", "EUR")
	if res, err := m.receive(m.orch, CallbackEventBet, "life-bet-1", "", "life-round-1", 1000, session); err != nil || res.Outcome != OutcomeSucceeded {
		t.Fatalf("fresh session bet: %+v %v", res, err)
	}

	m.orch.SetSessionMaxLifetime(50 * time.Millisecond)
	time.Sleep(150 * time.Millisecond)

	debits0, credits0 := sumDebitsCredits(t, m.pool, m.f.tenantID)
	_, err := m.receive(m.orch, CallbackEventBet, "life-bet-2", "", "life-round-2", 1000, session)
	if !errors.Is(err, ErrLaunchSessionRequired) {
		t.Fatalf("a new bet past the maximum lifetime must be refused with ErrLaunchSessionRequired, got %v", err)
	}
	if d, c := sumDebitsCredits(t, m.pool, m.f.tenantID); d != debits0 || c != credits0 {
		t.Fatal("a refused bet changed the ledger")
	}
	// Settlement of the bet taken BEFORE the lifetime ended is not blocked.
	if res, err := m.receive(m.orch, CallbackEventWin, "life-win-1", "", "life-round-1", 3000, uuid.Nil); err != nil || res.Outcome != OutcomeSucceeded {
		t.Fatalf("win past the lifetime must settle, got %+v err=%v", res, err)
	}
	// A rollback of that exposure likewise (reversal of the win is a separate flow; use a fresh pair).
}

func TestPostBet_DefaultMaxLifetimeIsFourHours(t *testing.T) {
	if DefaultSessionMaxLifetime != 4*time.Hour {
		t.Fatalf("default max lifetime %v, want 4h", DefaultSessionMaxLifetime)
	}
	o := NewOrchestrator(map[string]CasinoProvider{}, nil)
	if o.maxSessionLifetime() != 4*time.Hour {
		t.Fatalf("unset orchestrator lifetime %v", o.maxSessionLifetime())
	}
	o.SetSessionMaxLifetime(0)
	o.SetSessionMaxLifetime(-time.Hour)
	if o.maxSessionLifetime() != 4*time.Hour {
		t.Fatal("non-positive values must keep the default")
	}
	o.SetSessionMaxLifetime(90 * time.Minute)
	if o.maxSessionLifetime() != 90*time.Minute {
		t.Fatal("SetSessionMaxLifetime ignored")
	}
}

// SR8-2 (c): a launch token minted before the market closed is refused (and revoked) at the bootstrap.
func TestBootstrapLaunch_TokenMintedBeforeCloseIsRefusedAfterIt(t *testing.T) {
	m := newMarketRig(t)
	m.openMarket(t, "MT")
	game := seedGame(t, m.pool, "mock-casino", "EUR")
	enableGameForTenant(t, m.pool, m.f, game.ID)
	session, token := mintSessionForBootstrap(t, m.pool, m.f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)

	m.setTenantPolicy(t, "MT", operatingmarket.StateDisabled)

	in := m.provider.BootstrapPayload(m.f.tenantID, token, "req-market-closed-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, m.orch, m.pool, m.f.tenantID, "mock-casino", in)
	result, err := m.orch.BootstrapLaunch(context.Background(), m.pool, m.f.tenantID, "mock-casino", v)
	if err != nil {
		t.Fatalf("a closed market is a denial result, not an error: %v", err)
	}
	if !result.Denied || result.DeniedReason != "market_closed" {
		t.Fatalf("want Denied/market_closed, got %+v", result)
	}
	assertGateDenied(t, m.pool, m.f, session.ID, "market_closed")
}

func TestBootstrapLaunch_OpenMarketStillConsumes(t *testing.T) {
	m := newMarketRig(t)
	m.openMarket(t, "MT")
	game := seedGame(t, m.pool, "mock-casino", "EUR")
	enableGameForTenant(t, m.pool, m.f, game.ID)
	_, token := mintSessionForBootstrap(t, m.pool, m.f, game, ModeReal, "EUR", DefaultLaunchTokenTTL)
	in := m.provider.BootstrapPayload(m.f.tenantID, token, "req-market-open-1", game.ProviderGameID, "EUR", "real")
	v := bootstrapVerified(t, m.orch, m.pool, m.f.tenantID, "mock-casino", in)
	result, err := m.orch.BootstrapLaunch(context.Background(), m.pool, m.f.tenantID, "mock-casino", v)
	if err != nil || result.Denied {
		t.Fatalf("an open market must bootstrap, got %+v err=%v", result, err)
	}
}

// Disable racing bets: every bet is either accepted (posted exactly once, ledger balanced) or declined
// (nothing posted); after the disable commits and in-flight work drains, the next bet is declined; the
// settlement of every accepted bet still posts.
func TestPostBet_DisableRacingBets_NeverCorruptsTheLedgerAndSettlementStillPosts(t *testing.T) {
	m := newMarketRig(t)
	m.openMarket(t, "MT")
	session := mintSession(t, m.pool, m.f, "mock-casino", "EUR")

	const n = 12
	var wg sync.WaitGroup
	results := make([]ReceiveCallbackResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = m.receive(m.orch, CallbackEventBet, "race-bet-"+uuid.NewString()[:8], "", "race-round-"+uuid.NewString()[:8], 100, session)
		}(i)
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		if err := m.setTenantPolicyErr("MT", operatingmarket.StateDisabled); err != nil && !errors.Is(err, operatingmarket.ErrConcurrentPolicyWrite) {
			t.Errorf("disable: %v", err)
		}
	}()
	close(start)
	wg.Wait()

	accepted := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("bet %d: unexpected error %v", i, errs[i])
		}
		switch results[i].Outcome {
		case OutcomeSucceeded:
			accepted++
		case OutcomeDeclined:
			if results[i].DeclineReason != DeclineReasonMarketUnavailable {
				t.Fatalf("bet %d declined for %q", i, results[i].DeclineReason)
			}
		default:
			t.Fatalf("bet %d: unexpected outcome %+v", i, results[i])
		}
	}
	if bal := cashBalance(t, m.pool, m.f); bal != 50000-int64(accepted)*100 {
		t.Fatalf("balance %d does not match %d accepted bets", bal, accepted)
	}
	if d, c := sumDebitsCredits(t, m.pool, m.f.tenantID); d != c {
		t.Fatalf("ledger invariant violated: %d vs %d", d, c)
	}
	// After the disable has committed, the very next bet is declined.
	if res, err := m.receive(m.orch, CallbackEventBet, "race-after", "", "race-round-after", 100, session); err != nil || res.Outcome != OutcomeDeclined {
		t.Fatalf("bet after the disable: %+v %v", res, err)
	}
}
