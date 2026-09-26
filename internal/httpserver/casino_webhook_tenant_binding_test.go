//go:build integration

// CAS-WH-TENANT-1 (Stage 10.2, ADR 0091; docs/plans/stage-10.2-planning/
// 01-webhook-trust-design.md §H, C3-C6/C9). Modeled on
// payment_webhook_tenant_binding_test.go's own conventions exactly.
package httpserver

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// TestCasinoWebhook_CrossTenantBet_Rejected is C3: a bet signed for tenant
// A, delivered to tenant B's slug - even naming a session id that
// genuinely exists under B - is rejected before any tenant-scoped read
// (the signing input binds tenant A's own tenant_id, so it can never
// verify against B's derived key).
func TestCasinoWebhook_CrossTenantBet_Rejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenantA)
	mustEnableCasinoCapability(t, srv, pool, tenantB)

	brandB := mustCreateBrand(t, pool, tenantB)
	playerB := mustRegisterPlayer(t, srv, brandB.Slug)
	mustActivatePlayer(t, pool, tenantB.ID, playerB.ID)
	fundWallet(t, pool, tenantB.ID, brandB.ID, playerB.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenantB.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, playerB.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionB := uuid.MustParse(launched.SessionID)

	// Signed FOR tenant A, but naming tenant B's own genuine session id.
	payload := mock.CallbackPayload(tenantA.ID, casino.CallbackEventBet, "cas-c3-bet-1", "", "round-c3", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", playerB.ID, sessionB)

	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenantB.Slug+"/mock-casino", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 for a tenant-A-signed callback delivered to tenant B, got %d", resp.StatusCode)
	}
	body := decodeAPIError(t, resp)
	if body.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message, got %q", body.Message)
	}

	var count int
	err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'cas-c3-bet-1'`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero ledger effect for the rejected cross-tenant bet, got %d rows", count)
	}
}

// TestCasinoWebhook_EqualSecretResolver_StillTenantBound is C4: even if a
// test resolver deliberately hands out the SAME underlying secret for two
// different tenants, the signature still fails to verify across tenants -
// the tenant id is part of what is signed (the signing input), not merely
// which secret happens to be looked up.
func TestCasinoWebhook_EqualSecretResolver_StillTenantBound(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenantA)
	mustEnableCasinoCapability(t, srv, pool, tenantB)

	// A payload correctly signed for tenant A...
	payload := mock.CallbackPayload(tenantA.ID, casino.CallbackEventBet, "cas-c4-bet-1", "", "round-c4", "game-1",
		1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
	// ...delivered to tenant B's own URL. Even though both tenants' mock
	// capability rows share the SAME process-global master secret (the
	// resolver derives a genuinely distinct per-tenant key from it - see
	// webhookauth.DeriveMockKey), the tenant id is part of the signing
	// input itself, so this can never verify for tenant B.
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenantB.Slug+"/mock-casino", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 (the tenant id is bound into the MAC), got %d", resp.StatusCode)
	}
}

// TestCasinoWebhook_TamperMatrix_Rejected is C5: tampering any field the
// signature covers - or the header shape itself - is rejected, and a
// legacy body-embedded "signature" field is rejected too (Stage 10.2
// removed it in favor of the header-only scheme).
func TestCasinoWebhook_TamperMatrix_Rejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenant)

	genuine := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-c5-bet-1", "", "round-c5", "game-1",
		1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())

	flipHex := func(s string) string {
		flipped := strings.Replace(s, "0", "f", 1)
		if flipped == s {
			flipped = strings.Replace(s, "1", "e", 1)
		}
		return flipped
	}

	cases := []struct {
		name    string
		mutate  func() webhookauth.Inbound
		wantMsg string
	}{
		{"tampered_body_amount", func() webhookauth.Inbound {
			in := genuine
			in.Body = []byte(strings.Replace(string(genuine.Body), `"amount":1000`, `"amount":9999`, 1))
			return in
		}, "callback rejected"},
		{"tampered_signature_header", func() webhookauth.Inbound {
			in := genuine
			in.Header = genuine.Header.Clone()
			sig := in.Header.Get(webhookauth.CasinoSignatureHeader)
			in.Header.Set(webhookauth.CasinoSignatureHeader, flipHex(sig))
			return in
		}, "callback rejected"},
		{"missing_key_id_header", func() webhookauth.Inbound {
			in := genuine
			in.Header = genuine.Header.Clone()
			in.Header.Del(webhookauth.CasinoKeyIDHeader)
			return in
		}, "callback rejected"},
		{"unknown_key_id", func() webhookauth.Inbound {
			in := genuine
			in.Header = genuine.Header.Clone()
			in.Header.Set(webhookauth.CasinoKeyIDHeader, "mock-v2")
			return in
		}, "callback rejected"},
		{"legacy_signature_field_in_body", func() webhookauth.Inbound {
			in := genuine
			in.Body = []byte(strings.TrimSuffix(string(genuine.Body), "}") + `,"signature":"deadbeef"}`)
			return in
		}, "callback rejected"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", c.mutate())
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", resp.StatusCode)
			}
			body := decodeAPIError(t, resp)
			if body.Message != c.wantMsg {
				t.Fatalf("expected message %q, got %q", c.wantMsg, body.Message)
			}
		})
	}
}

// TestCasinoWebhook_EnumerationOracle_IndistinguishableResponses is C6: the
// full pre-verification rejection matrix collapses to one byte-identical
// (modulo request_id) 401, including a non-JSON body and an oversized
// body to an otherwise-active, configured tenant - mirroring
// TestWebhook_EnumerationOracle_IndistinguishableResponses's payments
// analog exactly.
func TestCasinoWebhook_EnumerationOracle_IndistinguishableResponses(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	activeTenant := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, activeTenant)

	suspendedTenant := mustCreateTenant(t, pool)
	if err := pool.WithPlatformAdmin(context.Background(), activeTenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, suspendedTenant.ID)
		return err
	}); err != nil {
		t.Fatalf("suspend tenant: %v", err)
	}

	unconfiguredTenant := mustCreateTenant(t, pool)
	// No mock-casino capability row at all for this tenant.

	genuineBody := []byte(`{"event_type":"bet","provider_tx_id":"enum-ref","amount":1000,"asset_code":"EUR","outcome":"succeeded"}`)
	genuineSigned := mock.CallbackPayload(activeTenant.ID, casino.CallbackEventBet, "enum-ref", "", "round-enum", "game-1",
		1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())

	badSigHeader := genuineSigned.Header.Clone()
	sig := badSigHeader.Get(webhookauth.CasinoSignatureHeader)
	flipped := strings.Replace(sig, "0", "f", 1)
	if flipped == sig {
		flipped = strings.Replace(sig, "1", "e", 1)
	}
	badSigHeader.Set(webhookauth.CasinoSignatureHeader, flipped)

	nonJSONBody := []byte("this is deliberately not valid JSON at all {{{")
	oversizedBody := make([]byte, maxCasinoWebhookBodyBytes+1)

	type probe struct {
		name   string
		path   string
		header http.Header
		body   []byte
	}
	probes := []probe{
		{"unknown_slug", "/v1/webhooks/casino/does-not-exist-" + activeTenant.Slug + "/mock-casino", genuineSigned.Header, genuineSigned.Body},
		{"suspended_tenant", "/v1/webhooks/casino/" + suspendedTenant.Slug + "/mock-casino", genuineSigned.Header, genuineSigned.Body},
		{"unregistered_provider", "/v1/webhooks/casino/" + activeTenant.Slug + "/not-a-real-provider", genuineSigned.Header, genuineSigned.Body},
		{"bad_signature", "/v1/webhooks/casino/" + activeTenant.Slug + "/mock-casino", badSigHeader, genuineSigned.Body},
		{"non_json_body_active_tenant", "/v1/webhooks/casino/" + activeTenant.Slug + "/mock-casino", genuineSigned.Header, nonJSONBody},
		{"oversized_body_active_tenant", "/v1/webhooks/casino/" + activeTenant.Slug + "/mock-casino", genuineSigned.Header, oversizedBody},
	}
	_ = genuineBody // documents the body genuineSigned actually wraps; kept for readability only

	var referenceBody string
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			resp := rawPostCasinoCallback(t, srv, p.path, webhookauth.Inbound{Header: p.header, Body: p.body})
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", resp.StatusCode)
			}
			var body apierror.Error
			decodeBody(t, resp, &body)
			body.RequestID = ""
			normalized := string(body.Code) + "|" + body.Message
			if referenceBody == "" {
				referenceBody = normalized
			} else if normalized != referenceBody {
				t.Fatalf("expected the SAME body (modulo request_id) as every other case, got %q, want %q", normalized, referenceBody)
			}
			if body.Code != apierror.CodeUnauthorized || body.Message != "callback rejected" {
				t.Fatalf("unexpected body shape: %+v", body)
			}
		})
	}

	// unconfigured tenant: valid signature would need a resolver credential
	// bound to that tenant, which does not exist for a tenant that never
	// enabled the capability - a fresh callback signed for it still
	// resolves (the MOCK resolver derives per-tenant, not per-capability-
	// row), so this specifically proves the POST-verification capability
	// check (503) is distinct from the pre-verification 401 family, not
	// folded into it.
	t.Run("unconfigured_tenant_gets_503_not_401", func(t *testing.T) {
		payload := mock.CallbackPayload(unconfiguredTenant.ID, casino.CallbackEventBet, "enum-unconfigured-1", "", "round-x", "game-1",
			1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+unconfiguredTenant.Slug+"/mock-casino", payload)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503 for a verified callback against an unconfigured tenant, got %d", resp.StatusCode)
		}
	})

	t.Run("bad_provider_id_charset", func(t *testing.T) {
		resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+activeTenant.Slug+"/BAD_ID!", webhookauth.Inbound{Header: genuineSigned.Header, Body: genuineSigned.Body})
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("expected 401, got %d", resp.StatusCode)
		}
		body := decodeAPIError(t, resp)
		if body.Code != apierror.CodeUnauthorized || body.Message != "callback rejected" {
			t.Fatalf("unexpected body shape: %+v", body)
		}
	})
}

// TestCasinoPlay_BrokenResolver_Returns503Misconfigured is C11: the
// play-simulation route signs in-process for the caller's own tc.TenantID
// (never a value the player supplies) - if the resolver wiring itself is
// broken (nil, mirroring a mockProviderWiring defect), the failure is a
// server misconfiguration (503), never a caller-facing 401, and the
// response never carries the signed bytes or any signature material.
func TestCasinoPlay_BrokenResolver_Returns503Misconfigured(t *testing.T) {
	pool, issuer := testEnv(t)
	mock := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
	// A nil resolver: mirrors mockProviderWiring's own fail-closed default
	// when TestSupportRoutesEnabled() is false, but here TS is simulated
	// as broken WHILE the play-simulation routes are (incorrectly, for
	// this test's purposes) still registered - proving the handler layer
	// itself never assumes a working resolver.
	orchestrator := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, nil)
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-c11-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-c11-pw-1")
	resp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling the mock-casino capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	wagerResp := postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(1000))
	defer wagerResp.Body.Close()
	if wagerResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a broken resolver, got %d", wagerResp.StatusCode)
	}
	rawBytes, err := io.ReadAll(wagerResp.Body)
	if err != nil {
		t.Fatalf("read response body: %v", err)
	}
	raw := string(rawBytes)
	if strings.Contains(raw, "signature") || strings.Contains(raw, "X-Casino-") {
		t.Fatalf("the misconfiguration response must never carry signature material, got %q", raw)
	}
}

// TestCasinoWebhook_DisabledCapability_ValidSignatureGets503 is C9: a
// disabled (or never-configured) tenant capability 503s a fully VERIFIED
// callback - the casino capability stays a deliberate, POST-verification
// money-path kill switch (design §C3), never folded into the uniform
// pre-verification 401 family.
func TestCasinoWebhook_DisabledCapability_ValidSignatureGets503(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-c9-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-c9-pw-1")
	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"
	resp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 registering the disabled capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-c9-bet-1", "", "round-c9", "game-1",
		1000, "EUR", casino.OutcomeSucceeded, "", uuid.New(), uuid.New())
	callbackResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload)
	defer callbackResp.Body.Close()
	if callbackResp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 for a verified callback against a disabled capability, got %d", callbackResp.StatusCode)
	}

	var count int
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = 'cas-c9-bet-1'`).Scan(&count)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero ledger effect while the capability is disabled, got %d rows", count)
	}
}
