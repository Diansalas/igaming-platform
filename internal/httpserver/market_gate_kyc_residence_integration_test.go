//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/marketgate"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// Owner decision 3 end to end: the REAL gate over the REAL KYC-verified-
// residence Locator (no static country anywhere), through the HTTP handlers.

type kycWorld struct {
	*marketWorld
	kycSrv *httptest.Server
	staff  uuid.UUID
	person uuid.UUID
}

func newKYCWorld(t *testing.T) *kycWorld {
	t.Helper()
	w := newMarketWorld(t)
	deps := w.deps
	deps.MarketGate = marketgate.New(marketgate.NewKYCResidenceLocator(w.pool))
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	k := &kycWorld{marketWorld: w, kycSrv: srv}
	k.marketWorld.srv = srv // every helper now talks to the KYC-evidence server
	staff := mustCreateStaff(t, w.pool, w.tenant.ID, identity.StaffRoleCompliance, "kyc-comp-pw-1")
	k.staff = staff.ID
	if err := w.pool.WithTenant(context.Background(), w.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, w.player.ID).Scan(&k.person)
	}); err != nil {
		t.Fatalf("player person: %v", err)
	}
	return k
}

func (k *kycWorld) addKYC(t *testing.T, status, country string, age time.Duration) {
	t.Helper()
	now := time.Now()
	ref := "ref-" + uuid.NewString()
	var c, src *string
	var by *uuid.UUID
	var at *time.Time
	if country != "" {
		src2 := "reviewer_determination"
		c, src, by, at = &country, &src2, &k.staff, &now
	}
	if err := k.pool.WithTenant(context.Background(), k.tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `
			INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id, provider_reference,
			       created_at, updated_at, verified_residence_country, verified_residence_source, verified_residence_set_by, verified_residence_set_at)
			VALUES ($1,$2,$3,$4,$5,$6,'mock',$7,$8,$8,$9,$10,$11,$12)`,
			uuid.New(), k.tenant.ID, k.brand.ID, k.player.ID, k.person, status, ref, now.Add(-age), c, src, by, at)
		return err
	}); err != nil {
		t.Fatalf("seed kyc %s/%s: %v", status, country, err)
	}
}

func statusOf(resp *http.Response) int { resp.Body.Close(); return resp.StatusCode }

// No KYC record: deposit, launch and placement are refused with the ONE closed
// body, and nothing is created (no intent, session, bet, wallet).
func TestMarketGate_HTTP_KYC_NoVerifiedResidenceRefusesMoneyAndGames(t *testing.T) {
	k := newKYCWorld(t)
	_, i0, s0, wl0 := k.counts(t)
	bets0, _ := k.sportsbookRows(t)
	var canonical string
	for name, resp := range map[string]*http.Response{"deposit": k.deposit(t), "launch": k.launch(t), "bet": k.bet(t)} {
		status, body := refusalBody(t, resp)
		if status != http.StatusForbidden {
			t.Fatalf("%s: want 403, got %d (%s)", name, status, body)
		}
		if canonical == "" {
			canonical = body
		} else if body != canonical {
			t.Fatalf("%s: refusal differs (oracle):\n got  %s\n want %s", name, body, canonical)
		}
	}
	if _, i1, s1, wl1 := k.counts(t); i1 != i0 || s1 != s0 || wl1 != wl0 {
		t.Fatalf("refused requests created rows: intents %d->%d sessions %d->%d wallets %d->%d", i0, i1, s0, s1, wl0, wl1)
	}
	if bets1, _ := k.sportsbookRows(t); bets1 != bets0 {
		t.Fatal("a refused placement wrote a bet")
	}
}

// A verified residence in the open market lets all three through; a verified
// residence outside it refuses all three; a later pending re-verification
// withdraws the evidence on the very next request.
func TestMarketGate_HTTP_KYC_VerifiedResidenceDrivesAllowAndRefuse(t *testing.T) {
	k := newKYCWorld(t)
	k.addKYC(t, "approved", "MT", 3*time.Hour)
	if got := statusOf(k.deposit(t)); got != http.StatusCreated {
		t.Fatalf("deposit with verified MT residence: %d", got)
	}
	if got := statusOf(k.launch(t)); got != http.StatusCreated {
		t.Fatalf("launch with verified MT residence: %d", got)
	}
	if got := statusOf(k.bet(t)); got != http.StatusCreated && got != http.StatusOK {
		t.Fatalf("bet with verified MT residence: %d", got)
	}

	// Newer pending re-verification: evidence withdrawn immediately.
	k.addKYC(t, "pending", "", time.Hour)
	for name, resp := range map[string]*http.Response{"deposit": k.deposit(t), "launch": k.launch(t), "bet": k.bet(t)} {
		if got := statusOf(resp); got != http.StatusForbidden {
			t.Fatalf("%s after a pending re-verification: want 403, got %d", name, got)
		}
	}
}

func TestMarketGate_HTTP_KYC_VerifiedResidenceOutsideTheMarketIsRefused(t *testing.T) {
	k := newKYCWorld(t)
	k.addKYC(t, "approved", "DE", time.Hour) // DE: not licensed / not opted in
	for name, resp := range map[string]*http.Response{"deposit": k.deposit(t), "launch": k.launch(t), "bet": k.bet(t)} {
		if got := statusOf(resp); got != http.StatusForbidden {
			t.Fatalf("%s with verified DE residence: want 403, got %d", name, got)
		}
	}
}

// Client-supplied headers cannot supply or move the evidence: with no KYC they
// still refuse; with verified MT residence a header claiming DE does not refuse.
func TestMarketGate_HTTP_KYC_ClientHeadersAreNotEvidence(t *testing.T) {
	k := newKYCWorld(t)
	spoof := func(country string) int {
		req, _ := http.NewRequest(http.MethodPost, k.kycSrv.URL+"/v1/me/deposits", jsonBody(t, map[string]any{
			"asset_code": "EUR", "amount": 15000, "payment_method": "card", "idempotency_key": uuid.NewString()}))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+k.player.Tokens.AccessToken)
		for h, v := range map[string]string{"X-Country": country, "CF-IPCountry": country, "X-Forwarded-For": "203.0.113.7", "Accept-Language": country + "-" + country} {
			req.Header.Set(h, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return statusOf(resp)
	}
	if got := spoof("MT"); got != http.StatusForbidden {
		t.Fatalf("no KYC + spoofed MT headers: want 403, got %d", got)
	}
	k.addKYC(t, "approved", "MT", time.Hour)
	if got := spoof("DE"); got != http.StatusCreated {
		t.Fatalf("verified MT + spoofed DE headers: the KYC evidence must govern, got %d", got)
	}
}

// Registration (before KYC can exist): brand-level, no evidence, no client value.
func TestMarketGate_HTTP_KYC_RegistrationIsBrandLevel(t *testing.T) {
	k := newKYCWorld(t)
	// Open brand: a registration succeeds although no KYC/evidence exists for the new account.
	if got := statusOf(k.register(t)); got != http.StatusCreated {
		t.Fatalf("open brand registration: want 201, got %d", got)
	}
	// ... and that fresh account (no KYC) can neither deposit nor launch.
	// Brand closed: registration refused with the same closed 403.
	k.setTenant(operatingmarket.StateDisabled)
	resp := k.register(t)
	if status, _ := refusalBody(t, resp); status != http.StatusForbidden {
		t.Fatalf("closed brand registration: want 403, got %d", status)
	}
}

// An account registered through the brand-level rule has no residence evidence,
// so it can reach neither money nor games until KYC verifies a residence.
func TestMarketGate_HTTP_KYC_FreshRegistrationCannotReachMoneyOrGames(t *testing.T) {
	k := newKYCWorld(t)
	fresh := mustRegisterPlayer(t, k.kycSrv, k.brand.Slug)
	mustActivatePlayer(t, k.pool, k.tenant.ID, fresh.ID)
	dep := postJSON(t, k.kycSrv, "/v1/me/deposits", fresh.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 15000, "payment_method": "card", "idempotency_key": uuid.NewString()})
	if got := statusOf(dep); got != http.StatusForbidden {
		t.Fatalf("fresh account deposit: want 403, got %d", got)
	}
	la := postJSON(t, k.kycSrv, "/v1/me/casino/games/"+k.gameID.String()+"/launch", fresh.Tokens.AccessToken,
		map[string]string{"asset_code": "EUR", "mode": "demo"})
	if got := statusOf(la); got != http.StatusForbidden {
		t.Fatalf("fresh account launch: want 403, got %d", got)
	}
}

// Tenant isolation through HTTP: tenant B's open market and a B player token
// never read tenant A's verified residence for A's player.
func TestMarketGate_HTTP_KYC_AnotherTenantsVerifiedResidenceIsNeverRead(t *testing.T) {
	k := newKYCWorld(t)
	k.addKYC(t, "approved", "MT", time.Hour) // tenant A's player is verified

	b := mustCreateTenant(t, k.pool)
	bBrand := mustCreateBrand(t, k.pool, b)
	openMarketFor(t, k.pool, b.ID, "MT")
	bPlayer := mustRegisterPlayer(t, k.kycSrv, bBrand.Slug)
	mustActivatePlayer(t, k.pool, b.ID, bPlayer.ID)
	dep := postJSON(t, k.kycSrv, "/v1/me/deposits", bPlayer.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 15000, "payment_method": "card", "idempotency_key": uuid.NewString()})
	if got := statusOf(dep); got != http.StatusForbidden {
		t.Fatalf("tenant B player has no KYC of its own: want 403, got %d", got)
	}
}
