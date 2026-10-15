//go:build integration

// A8: end-to-end tests of operating-market blocking at the three wired
// points - POST /v1/auth/register, POST /v1/me/deposits and
// POST /v1/me/casino/games/{gameID}/launch - with the REAL marketgate.Gate
// over a static Locator and REAL operating-market configuration rows (the
// harness' allowAllMarketGate is used everywhere else). Every refusal must
// be ONE status/code/message regardless of cause, must create nothing, and
// must leave a refusal audit row carrying ids and closed codes only.
package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/geolocation"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// bindLicence gives the tenant its own licence (the ceiling's anchor).
func bindLicence(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (licenceID, adminID uuid.UUID) {
	t.Helper()
	licenceID = uuid.New()
	adminID = mustSeedPlatformAdminStaffPrincipal(t, pool)
	jid := uuid.New()
	err := launchfix.OwnerFor(t, pool).WithPlatformAdmin(context.Background(), adminID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Market Gate Test')`, jid, "MG-"+jid.String()[:8]); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO licences (id, jurisdiction_id, licensee, licence_number) VALUES ($1, $2, 'platform', $3)`,
			licenceID, jid, "MGL-"+licenceID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $2 WHERE id = $1`, tenantID, licenceID)
		return err
	})
	if err != nil {
		t.Fatalf("bind licence: %v", err)
	}
	return licenceID, adminID
}

func openMarketFor(t *testing.T, pool *db.Pool, tenantID uuid.UUID, cc string) (setTenant func(operatingmarket.State)) {
	t.Helper()
	licenceID, adminID := bindLicence(t, pool, tenantID)
	err := launchfix.OwnerFor(t, pool).WithPlatformAdmin(context.Background(), adminID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := operatingmarket.CreateLicenceCountryCeilingVersion(ctx, tx, operatingmarket.CreateLicenceCountryCeilingVersionParams{
			LicenceID: licenceID, CountryCode: cc, State: operatingmarket.StateEnabled, Status: operatingmarket.StatusActive,
			AuthorizationReference: "legal-ref", Actor: operatingmarket.ActorContext{ActorID: adminID, ReasonCode: "a8-test"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("enable ceiling: %v", err)
	}
	set := func(state operatingmarket.State) {
		t.Helper()
		auth := ""
		if state == operatingmarket.StateEnabled {
			auth = "compliance-ref"
		}
		err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := operatingmarket.CreateOperatingCountryPolicyVersion(ctx, tx, operatingmarket.CreateOperatingCountryPolicyVersionParams{
				Scope: operatingmarket.ScopeTenant, TenantID: tenantID, CountryCode: cc, State: state, Status: operatingmarket.StatusActive,
				AuthorizationReference: auth, Actor: operatingmarket.ActorContext{ActorID: adminID, ReasonCode: "a8-test"},
			})
			return err
		})
		if err != nil {
			t.Fatalf("tenant policy %s: %v", state, err)
		}
	}
	set(operatingmarket.StateEnabled)
	return set
}

func countTenantRows(t *testing.T, pool *db.Pool, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

// refusalBody normalises a refusal for byte comparison: request_id is the
// only legitimately varying field.
func refusalBody(t *testing.T, resp *http.Response) (int, string) {
	t.Helper()
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("refusal body is not JSON: %s", raw)
	}
	delete(m, "request_id")
	norm, _ := json.Marshal(m)
	return resp.StatusCode, string(norm)
}

type marketWorld struct {
	pool       *db.Pool
	srv        *httptest.Server
	locator    *geolocation.StaticLocator
	tenant     identity.Tenant
	brand      identity.Brand
	setTenant  func(operatingmarket.State)
	gameID     uuid.UUID
	player     registeredPlayer
	regCounter int
	deps       Deps
	selection  uuid.UUID // a sportsbook selection (SR8-1)
}

func newMarketWorld(t *testing.T) *marketWorld {
	t.Helper()
	pool, issuer := testEnv(t)
	payOrch, payMock := newMockOrchestrator()
	casOrch, _ := newMockCasinoOrchestrator()
	locator := geolocation.NewStaticLocator("MT")
	deps := financialTestDeps(pool, issuer, payOrch)
	deps.CasinoOrchestrator = casOrch
	deps.CasinoOutboundCredentials = casino.NewMockOutboundResolver()
	deps.MarketGate = marketgate.New(locator) // the REAL gate
	deps.SportsbookEnabled = true
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)

	w := &marketWorld{pool: pool, srv: srv, locator: locator, deps: deps}
	w.tenant = mustCreateTenant(t, pool)
	w.brand = mustCreateBrand(t, pool, w.tenant)
	w.setTenant = openMarketFor(t, pool, w.tenant.ID, "MT")

	staff := mustCreateStaff(t, pool, w.tenant.ID, identity.StaffRoleTenantAdmin, "mg-ta-pw-1")
	tokens := mustLoginStaff(t, srv, w.tenant.Slug, staff.Email, "mg-ta-pw-1")
	if r := capabilityPutRequest(t, srv, "mock", tokens.AccessToken, validCapabilityBody()); r.StatusCode != http.StatusOK {
		t.Fatalf("payments capability: %d", r.StatusCode)
	}
	if r := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody()); r.StatusCode != http.StatusOK {
		t.Fatalf("casino capability: %d", r.StatusCode)
	}
	_ = payMock
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, w.tenant.ID, game.ID)
	w.gameID = game.ID

	// A player registered while the market is open (locator MT).
	w.player = mustRegisterPlayer(t, srv, w.brand.Slug)
	mustActivatePlayer(t, pool, w.tenant.ID, w.player.ID)
	w.selection = mustSeedSportsbookSelection(t, pool, 250, 100)
	fundWallet(t, pool, w.tenant.ID, w.brand.ID, w.player.ID, "EUR", 1_000_000)
	return w
}

func (w *marketWorld) register(t *testing.T) *http.Response {
	w.regCounter++
	return postJSON(t, w.srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": w.brand.Slug, "email": "mg-" + uuid.NewString() + "@example.com", "password": "a-decent-password-1",
	})
}

func (w *marketWorld) deposit(t *testing.T) *http.Response {
	return postJSON(t, w.srv, "/v1/me/deposits", w.player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 15000, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
}

func (w *marketWorld) launch(t *testing.T) *http.Response {
	return postJSON(t, w.srv, "/v1/me/casino/games/"+w.gameID.String()+"/launch", w.player.Tokens.AccessToken,
		map[string]string{"asset_code": "EUR", "mode": "demo"})
}

func (w *marketWorld) bet(t *testing.T) *http.Response {
	return postJSON(t, w.srv, "/v1/me/sportsbook/bets", w.player.Tokens.AccessToken,
		placeBetRequestBody(w.selection, 1000, 250, 100, uuid.NewString()))
}

// sportsbookRows counts the rows a placement writes: bets and sportsbook_bet ledger transactions.
func (w *marketWorld) sportsbookRows(t *testing.T) (bets, ledgerTx int) {
	return countTenantRows(t, w.pool, w.tenant.ID, `SELECT count(*) FROM sportsbook_bets WHERE tenant_id = $1`, w.tenant.ID),
		countTenantRows(t, w.pool, w.tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'sportsbook_bet'`, w.tenant.ID)
}

func (w *marketWorld) counts(t *testing.T) (players, intents, sessions, wallets int) {
	return countTenantRows(t, w.pool, w.tenant.ID, `SELECT count(*) FROM player_accounts WHERE tenant_id = $1`, w.tenant.ID),
		countTenantRows(t, w.pool, w.tenant.ID, `SELECT count(*) FROM deposit_intents WHERE tenant_id = $1`, w.tenant.ID),
		countTenantRows(t, w.pool, w.tenant.ID, `SELECT count(*) FROM casino_launch_sessions WHERE tenant_id = $1`, w.tenant.ID),
		countTenantRows(t, w.pool, w.tenant.ID, `SELECT count(*) FROM wallets WHERE tenant_id = $1`, w.tenant.ID)
}

func (w *marketWorld) refusalAudits(t *testing.T, op string) int {
	return countTenantRows(t, w.pool, w.tenant.ID,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2 AND metadata->>'operation' = $3`,
		w.tenant.ID, marketgate.AuditActionRefused, op)
}
