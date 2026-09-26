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
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// mustMintCasinoLaunchSessionDirect mints a real casino_launch_sessions
// row DIRECTLY via casino.CreateLaunchSession, bypassing LaunchGame's own
// checks entirely (including its OWN capability requirement) - the
// httpserver-package equivalent of internal/casino's own mintSession test
// helper. Stage 10.3 CAS-CAP-ROLLBACK-1 moved postBet's capability gate to
// run AFTER session resolution, so a test proving the S-none/S-off
// capability states at the callback layer needs a genuinely resolvable
// session to reach that gate at all - a fabricated uuid.New() session id
// now fails earlier, at ErrLaunchSessionRequired, which is not what these
// tests are about.
func mustMintCasinoLaunchSessionDirect(t *testing.T, pool *db.Pool, tenant identity.Tenant, brandID, playerID, walletID, gameID uuid.UUID, providerID, providerGameID, assetCode string) uuid.UUID {
	t.Helper()
	var sessionID uuid.UUID
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		session, _, err := casino.CreateLaunchSession(ctx, tx, casino.CreateLaunchSessionParams{
			TenantID: tenant.ID, BrandID: brandID, PlayerAccountID: playerID, WalletID: walletID,
			GameID: gameID, ProviderID: providerID, ProviderGameID: providerGameID,
			AssetCode: assetCode, Mode: casino.ModeReal,
		})
		if err != nil {
			return err
		}
		sessionID = session.ID
		return nil
	})
	if err != nil {
		t.Fatalf("mint casino launch session directly: %v", err)
	}
	return sessionID
}

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

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenantA.ID, tenantB.ID})
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
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenantA.ID, tenantB.ID}, before)
}

// equalSecretCasinoResolver returns the SAME credential secret regardless
// of tenantID - L4 fix (code review, Stage 10.2), ported from KYC's own
// equalSecretResolver (internal/kyc/orchestrator_webhook_integration_
// test.go). Proves the tenant binding lives in the SIGNING INPUT (which
// includes tenant_id, per webhookauth.Scheme.SigningInput), not merely in
// "which secret happens to be looked up" - unlike the ORIGINAL (buggy)
// version of this test, which reused newMockCasinoOrchestrator's own
// resolver (webhookauth.DeriveMockKey, which already derives a genuinely
// DIFFERENT key per tenant) and so was indistinguishable from C3.
type equalSecretCasinoResolver struct {
	secret     []byte
	providerID string
}

func (r equalSecretCasinoResolver) ResolveKey(_ context.Context, tenantID uuid.UUID, providerID, keyID string) (webhookauth.Credential, error) {
	if providerID != r.providerID || keyID != webhookauth.MockKeyID {
		return webhookauth.Credential{}, webhookauth.ErrCredentialUnavailable
	}
	return webhookauth.Credential{TenantID: tenantID, ProviderID: providerID, KeyID: keyID, Secret: r.secret, Fingerprint: webhookauth.Fingerprint(r.secret)}, nil
}

// Resolve adapts ResolveKey to the ADR 0093 §4 resolver signature (a
// single-key test double ignores tx; KeyImplicit fails closed).
func (r equalSecretCasinoResolver) Resolve(ctx context.Context, _ pgx.Tx, tenantID uuid.UUID, providerID, keyID string, sel webhookauth.KeySelection) (webhookauth.CredentialSet, error) {
	return webhookauth.ResolveSingleKey(ctx, r, tenantID, providerID, keyID, sel)
}

// TestCasinoWebhook_EqualSecretResolver_StillTenantBound is C4: even with
// a resolver that hands out the SAME secret for every tenant (unlike C3,
// where each tenant's own per-tenant-derived key already differs), an
// A-signed callback delivered to B is still rejected, because the tenant
// id is bound into the signing input itself, not merely a side effect of
// which secret got looked up.
//
// Mutation-kill demonstration (recorded in docs/plans/stage-10.2-planning/
// 08-webhook-test-traceability.md, not committed as code): with tenant_id
// dropped from webhookauth.Scheme.SigningInput (leaving provider_id/key_id/
// body only), this test goes red - a B-delivered, A-signed callback under
// this SAME equal-secret resolver now verifies and posts.
func TestCasinoWebhook_EqualSecretResolver_StillTenantBound(t *testing.T) {
	pool, issuer := testEnv(t)
	mock := casino.NewMockCasinoProvider("mock-casino", "EUR", "USD")
	secret := []byte("equal-secret-shared-by-every-casino-tenant-32b!")
	resolver := equalSecretCasinoResolver{secret: secret, providerID: "mock-casino"}
	orchestrator := casino.NewOrchestrator(map[string]casino.CasinoProvider{"mock-casino": mock}, resolver)
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	mustEnableCasinoCapability(t, srv, pool, tenantA)
	mustEnableCasinoCapability(t, srv, pool, tenantB)

	brandA := mustCreateBrand(t, pool, tenantA)
	playerA := mustRegisterPlayer(t, srv, brandA.Slug)
	mustActivatePlayer(t, pool, tenantA.ID, playerA.ID)
	fundWallet(t, pool, tenantA.ID, brandA.ID, playerA.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenantA.ID, game.ID)
	launchedA := mustLaunchCasinoGame(t, srv, playerA.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionA := uuid.MustParse(launchedA.SessionID)

	scheme := webhookauth.CasinoScheme()
	body := []byte(`{"event_type":"bet","provider_tx_id":"cas-c4-bet-1","round_id":"round-c4","provider_game_id":"` + game.ProviderGameID + `","amount":1000,"asset_code":"EUR","outcome":"succeeded","player_account_id":"` + playerA.ID.String() + `","session_id":"` + sessionA.String() + `"}`)
	sigForA := scheme.Sign(secret, tenantA.ID, "mock-casino", webhookauth.MockKeyID, body)
	header := make(http.Header)
	scheme.SetHeaders(header, webhookauth.MockKeyID, sigForA)
	payload := webhookauth.Inbound{TenantID: tenantA.ID, ProviderID: "mock-casino", Header: header, Body: body}

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenantA.ID, tenantB.ID})
	// Delivered to B: the header/body bytes are byte-identical to what
	// verified for A under this SAME shared secret, but B's own tenant id
	// gets substituted into the signing input the orchestrator recomputes -
	// it will not match.
	resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenantB.Slug+"/mock-casino", payload)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 under an equal-secret resolver (the tenant id is bound into the MAC), got %d", resp.StatusCode)
	}
	body2 := decodeAPIError(t, resp)
	if body2.Message != "callback rejected" {
		t.Fatalf("expected the uniform 'callback rejected' message, got %q", body2.Message)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenantA.ID, tenantB.ID}, before)

	// Sanity check that the SAME payload correctly verifies for its OWN
	// tenant (A) under this equal-secret resolver - proving the 401 above
	// is genuinely about the tenant mismatch, not merely a malformed
	// request this resolver could never accept from anyone.
	resp2 := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenantA.Slug+"/mock-casino", payload)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for the SAME payload delivered to its own tenant A, got %d", resp2.StatusCode)
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
			// M1 fix (code review, Stage 10.2): the legacy-shaped body must
			// be GENUINELY, correctly signed with the real derived key -
			// otherwise this case never reaches HandleCallback's own
			// hasLegacySignatureField guard at all (it would already be
			// rejected earlier, by Verify, for the unrelated reason that
			// the body changed after signing). mock.SignRawBody signs
			// EXACTLY the bytes handed to it, so this is the one payload in
			// the matrix whose signature is valid over a body containing
			// a "signature" field - the guard this case exists to prove is
			// the ONLY thing standing between it and acceptance (see the
			// mutation-kill record in docs/plans/stage-10.2-planning/
			// 08-webhook-test-traceability.md).
			legacyBody := []byte(strings.TrimSuffix(string(genuine.Body), "}") + `,"signature":"deadbeef"}`)
			return mock.SignRawBody(tenant.ID, legacyBody)
		}, "callback rejected"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
			resp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", c.mutate())
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", resp.StatusCode)
			}
			body := decodeAPIError(t, resp)
			if body.Message != c.wantMsg {
				t.Fatalf("expected message %q, got %q", c.wantMsg, body.Message)
			}
			noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
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
	unconfiguredBrand := mustCreateBrand(t, pool, unconfiguredTenant)
	unconfiguredPlayer := mustRegisterPlayer(t, srv, unconfiguredBrand.Slug)
	mustActivatePlayer(t, pool, unconfiguredTenant.ID, unconfiguredPlayer.ID)
	unconfiguredWallet := fundWallet(t, pool, unconfiguredTenant.ID, unconfiguredBrand.ID, unconfiguredPlayer.ID, "EUR", 10_000)
	unconfiguredGame := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	unconfiguredSessionID := mustMintCasinoLaunchSessionDirect(t, pool, unconfiguredTenant, unconfiguredBrand.ID, unconfiguredPlayer.ID, unconfiguredWallet.ID, unconfiguredGame.ID, "mock-casino", unconfiguredGame.ProviderGameID, "EUR")

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
		payload := mock.CallbackPayload(unconfiguredTenant.ID, casino.CallbackEventBet, "enum-unconfigured-1", "", "round-x", unconfiguredGame.ProviderGameID,
			1000, "EUR", casino.OutcomeSucceeded, "", unconfiguredPlayer.ID, unconfiguredSessionID)
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

	// A genuinely resolvable session is required to reach postBet's own
	// capability gate at all (Stage 10.3 CAS-CAP-ROLLBACK-1 moved that gate
	// to run AFTER session resolution) - minted directly since LaunchGame
	// itself would refuse while the capability is disabled.
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	wl := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	sessionID := mustMintCasinoLaunchSessionDirect(t, pool, tenant, brand.ID, player.ID, wl.ID, game.ID, "mock-casino", game.ProviderGameID, "EUR")

	payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "cas-c9-bet-1", "", "round-c9", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{tenant.ID})
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
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{tenant.ID}, before)
}

// TestCasinoWebhook_CAS_CAP_ROLLBACK_1_DisabledCapabilityStillAllowsRollbackOfAlreadyPostedBet
// is CAS-CAP-ROLLBACK-1's own resolution (Stage 10.3,
// docs/plans/stage-10.3-planning/02-casino-financial-analysis.md §1.3),
// flipped from a pre-fix CHARACTERIZATION to a REQUIREMENT (04-review-qa.md
// §3's binding instruction: "flip ... from a characterization to a
// requirement"). It used to record that once a bet had genuinely posted
// (capability enabled at the time), disabling the tenant's WHOLE
// casino_provider_capabilities row afterward made even a fully verified,
// otherwise-legitimate ROLLBACK of that SAME already-posted bet 503
// ("provider unavailable") - stranding the stake with no way to correct
// it. The ledger-finance ruling closes that: capability/status gate NEW
// EXPOSURE ONLY; settlement of existing exposure (a rollback of an
// already-posted bet) is NEVER blocked by a disabled/missing capability.
func TestCasinoWebhook_CAS_CAP_ROLLBACK_1_DisabledCapabilityStillAllowsRollbackOfAlreadyPostedBet(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	staff := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "cas-caprb1-pw-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "cas-caprb1-pw-1")

	// Enable the capability, post a genuine bet (a real ledger effect),
	// THEN disable the capability - mirroring a tenant turning a provider
	// off mid-operation, with an already-open round.
	enableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, validCasinoCapabilityBody())
	if enableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 enabling the mock-casino capability, got %d", enableResp.StatusCode)
	}
	enableResp.Body.Close()

	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, pool, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, pool, tenant.ID, game.ID)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")
	sessionID := uuid.MustParse(launched.SessionID)

	const originalTxID = "cas-caprb1-original-bet"
	betPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, originalTxID, "", "round-caprb1", game.ProviderGameID,
		1000, "EUR", casino.OutcomeSucceeded, "", player.ID, sessionID)
	betResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", betPayload)
	if betResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 posting the original bet while the capability is enabled, got %d", betResp.StatusCode)
	}
	betResp.Body.Close()

	var ledgerCountAfterBet int
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id = $1`, originalTxID).Scan(&ledgerCountAfterBet)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions after bet: %v", err)
	}
	if ledgerCountAfterBet != 1 {
		t.Fatalf("expected the original bet to have genuinely posted (1 ledger_transactions row), got %d", ledgerCountAfterBet)
	}

	disabledBody := validCasinoCapabilityBody()
	disabledBody["status"] = "disabled"
	disableResp := putJSON(t, srv, "/v1/admin/casino/providers/mock-casino/capability", tokens.AccessToken, disabledBody)
	if disableResp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 disabling the mock-casino capability, got %d", disableResp.StatusCode)
	}
	disableResp.Body.Close()

	rollbackPayload := mock.CallbackPayload(tenant.ID, casino.CallbackEventRollback, "cas-caprb1-rollback", originalTxID, "", "",
		0, "EUR", "", "", player.ID, uuid.Nil)
	rollbackResp := rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", rollbackPayload)
	defer rollbackResp.Body.Close()
	// REQUIREMENT (Stage 10.3 CAS-CAP-ROLLBACK-1): a rollback of an
	// already-posted bet settles (200) even though the capability is
	// disabled - settlement of existing exposure is never gated by
	// capability/status.
	if rollbackResp.StatusCode != http.StatusOK {
		t.Fatalf("Stage 10.3 CAS-CAP-ROLLBACK-1: expected 200 for a rollback of an already-posted bet EVEN THOUGH the capability is disabled, got %d", rollbackResp.StatusCode)
	}

	// The rollback genuinely reversed the original bet: two rows now exist
	// (the original bet plus its reversal), and the reversal names the
	// original.
	var ledgerCountAfterRollback int
	err = pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE provider_id = 'mock-casino' AND provider_tx_id IN ($1, $2)`, originalTxID, "cas-caprb1-rollback").Scan(&ledgerCountAfterRollback)
	})
	if err != nil {
		t.Fatalf("query ledger_transactions after rollback: %v", err)
	}
	if ledgerCountAfterRollback != 2 {
		t.Fatalf("expected exactly 2 ledger_transactions rows (the original bet plus its reversal) after the rollback settled, got %d", ledgerCountAfterRollback)
	}
	balance := walletCashBalance(t, srv, player.Tokens.AccessToken)
	if balance != 10_000 {
		t.Fatalf("expected the stake to be returned by the rollback (balance back to 10000), got %d", balance)
	}
}
