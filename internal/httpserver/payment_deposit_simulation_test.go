//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/payments"
)

// --- Stage 9.3 mock-provider deposit-settlement-simulation test fixtures ---
//
// Sibling to financial_flow_integration_test.go's own fixtures - reuses
// newMockOrchestrator/mustRegisterCapability/mustRegisterPlayer/
// mustActivatePlayer/validCapabilityBody/decodeAPIError from that file, and
// follows the same "fixtures via internal packages, real HTTP for what's
// under test" convention. Mirrors casino_play_handlers.go's own adversarial
// test suite in shape: wrong-player denial, disabled-outside-non-production,
// wrong-provider-type denial, already-settled denial, idempotent double-call,
// and one full happy path with ledger verification.

// newFinancialTestServerWithMockSettlement is newFinancialTestServer plus
// PaymentsMockSettlementEnabled: true - mirrors newStage7B2CTestServer's
// identical pattern of adding one simulation flag on top of the base
// financial test server.
func newFinancialTestServerWithMockSettlement(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *payments.Orchestrator) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(New(Deps{
		Logger:                        slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                            pool,
		AuthIssuer:                    issuer,
		ServiceName:                   "platform-api-test",
		AccessTokenTTL:                5 * time.Minute,
		RefreshTokenTTL:               time.Hour,
		PaymentOrchestrator:           orchestrator,
		PaymentsMockSettlementEnabled: true,
		PersonResolver:                identityresolution.NewMockPersonResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv
}

// nonMockPaymentProvider wraps a *payments.MockProvider so a test can
// register a fully-functional (from the orchestrator's point of view)
// adapter under a DIFFERENT concrete Go type than *payments.MockProvider -
// proving requireMockPaymentProvider's type assertion, not merely a
// provider_id string comparison, is what actually gates this endpoint.
// Never a second real adapter implementation - purely a test double,
// mirroring internal/payments' own spyProvider test-only wrapper pattern.
type nonMockPaymentProvider struct{ *payments.MockProvider }

func ledgerTransactionCountForProviderRef(t *testing.T, pool *db.Pool, tenantID uuid.UUID, providerRef string) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_tx_id = $2`, tenantID, providerRef).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count ledger transactions for provider ref %q: %v", providerRef, err)
	}
	return count
}

// providerReferenceFromRedirectURL extracts the mock adapter's own
// provider_reference from InitiateDeposit's own redirect_url shape
// ("https://mock-psp.invalid/pay/<ref>") - mirrors
// TestFinancialHappyPath_EndToEnd's identical technique.
func providerReferenceFromRedirectURL(redirectURL string) string {
	return strings.TrimPrefix(redirectURL, "https://mock-psp.invalid/pay/")
}

// --- 1. Happy path: deposit -> simulate-callback -> ledger + wallet ---

func TestSimulateDepositCallback_HappyPath_SettlesDepositWithLedgerEntries(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 21000
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	if intent.Status != "pending" {
		t.Fatalf("expected deposit intent status 'pending', got %q", intent.Status)
	}
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	// This is the endpoint under test: no real PSP webhook exists in a
	// live, HTTP-only process, so the player's own client drives this
	// simulate-callback endpoint exactly as a real deployment's staging
	// acceptance test would.
	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 200 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 200 simulating the deposit callback, got %d (%s)", resp.StatusCode, body.Message)
	}
	var result map[string]any
	decodeBody(t, resp, &result)
	if result["status"] != "succeeded" {
		t.Fatalf("expected status 'succeeded', got %v", result["status"])
	}
	if result["deposit_intent_id"] != intent.ID {
		t.Fatalf("expected deposit_intent_id %q, got %v", intent.ID, result["deposit_intent_id"])
	}

	// Ledger: exactly one transaction posted under this provider reference.
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, providerRef); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction for provider ref %q, got %d", providerRef, got)
	}

	// Wallet reflects the credited balance - the real HTTP read a Back
	// Office or player client would use to confirm visibility.
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 reading wallet, got %d", resp.StatusCode)
	}
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != depositAmount || walletResp.AvailableBalance != depositAmount {
		t.Fatalf("expected cash/available balance %d after simulated settlement, got cash=%d available=%d",
			depositAmount, walletResp.CashBalance, walletResp.AvailableBalance)
	}

	// Deposit's own status, read back via the ordinary player-facing GET.
	resp = getJSON(t, srv, "/v1/me/deposits/"+intent.ID, player.Tokens.AccessToken)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 reading the settled deposit, got %d", resp.StatusCode)
	}
	var reread depositIntentResponse
	decodeBody(t, resp, &reread)
	if reread.Status != "succeeded" {
		t.Fatalf("expected re-read deposit status 'succeeded', got %q", reread.Status)
	}
}

// --- 2. A different player cannot trigger someone else's deposit's callback ---

func TestSimulateDepositCallback_WrongPlayerDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	victim := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, victim.ID)
	attacker := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, attacker.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", victim.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 8800, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 initiating victim's deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)

	// The attacker, authenticated as themselves, names the victim's own
	// deposit id.
	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", attacker.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for the attacker naming another player's deposit id, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Code != apierror.CodeNotFound {
		t.Errorf("expected not_found code, got %q", apiErr.Code)
	}

	// Confirm the victim's own deposit is genuinely untouched - never
	// inferred from the attacker's failed attempt alone.
	resp = getJSON(t, srv, "/v1/me/deposits/"+intent.ID, victim.Tokens.AccessToken)
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for victim reading their own deposit, got %d", resp.StatusCode)
	}
	var reread depositIntentResponse
	decodeBody(t, resp, &reread)
	if reread.Status != "pending" {
		t.Errorf("expected victim's deposit to remain 'pending' after the attacker's failed attempt, got %q", reread.Status)
	}

	// The victim themselves can still legitimately settle it - proves the
	// 404 above is ownership-scoped, not a broken route.
	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", victim.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 200 {
		t.Errorf("expected 200 for the victim settling their own deposit, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 3. Disabled outside non-production: the route does not exist at all
// when PaymentsMockSettlementEnabled is false ---

func TestSimulateDepositCallback_DisabledWhenFlagOff(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	// newFinancialTestServer (financial_flow_integration_test.go) leaves
	// PaymentsMockSettlementEnabled at its zero value (false) - mirrors
	// cmd/platform-api/main.go's production behavior structurally, not
	// merely by an operator's config choice.
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 6600, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)

	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 (route not registered) when PaymentsMockSettlementEnabled is false, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Confirm the deposit is genuinely untouched.
	resp = getJSON(t, srv, "/v1/me/deposits/"+intent.ID, player.Tokens.AccessToken)
	decodeBody(t, resp, &intent)
	if intent.Status != "pending" {
		t.Errorf("expected deposit to remain 'pending' with the simulation route disabled, got %q", intent.Status)
	}
}

// --- 4. Rejected for a non-mock provider, even though it is otherwise a
// fully-functional, routable adapter ---

func TestSimulateDepositCallback_RejectedForNonMockProvider(t *testing.T) {
	pool, issuer := testEnv(t)
	otherProvider := &nonMockPaymentProvider{MockProvider: payments.NewMockProvider("other-psp", "EUR")}
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{"other-psp": otherProvider}, payments.MultiWebhookCredentialResolver{"other-psp": payments.NewMockWebhookCredentials(otherProvider.MockProvider)})
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, otherProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 7700, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	if intent.ProviderID != "other-psp" {
		t.Fatalf("expected the deposit to route to 'other-psp', got %q", intent.ProviderID)
	}

	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 503 {
		t.Fatalf("expected 503 for a non-mock provider, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me/deposits/"+intent.ID, player.Tokens.AccessToken)
	decodeBody(t, resp, &intent)
	if intent.Status != "pending" {
		t.Errorf("expected the deposit to remain 'pending' after a rejected non-mock simulation attempt, got %q", intent.Status)
	}
}

// --- 5. Rejected for an already-settled deposit, with no second ledger
// effect ---

func TestSimulateDepositCallback_RejectedForAlreadySettledDeposit(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 5500
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200 for the first simulate-callback call, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A second, sequential call against the now-succeeded intent.
	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 for a second simulate-callback call against an already-succeeded deposit, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Exactly one ledger transaction, and the wallet reflects exactly one
	// deposit's worth - the second call had no financial effect at all,
	// which is the property that actually matters (CLAUDE.md's
	// idempotency requirement), regardless of its own HTTP status.
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, providerRef); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction after the rejected second call, got %d", got)
	}
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != depositAmount {
		t.Fatalf("expected cash balance to remain exactly %d after the rejected second call, got %d", depositAmount, walletResp.CashBalance)
	}
}

// --- 6. Idempotent under a genuine concurrent double-call: two
// simultaneous simulate-callback requests against the SAME still-pending
// deposit intent must post exactly one ledger transaction, never two ---

func TestSimulateDepositCallback_ConcurrentDoubleCallIsIdempotent(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 9900
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	const n = 5
	statuses := make([]int, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
			statuses[i] = r.StatusCode
			r.Body.Close()
		}(i)
	}
	wg.Wait()

	for i, code := range statuses {
		if code != 200 && code != 409 {
			t.Errorf("goroutine %d: expected 200 or 409, got %d", i, code)
		}
	}

	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, providerRef); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction after %d concurrent simulate-callback calls, got %d", n, got)
	}

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != depositAmount {
		t.Fatalf("expected exactly one deposit's worth (%d) after %d concurrent calls, got %d", depositAmount, n, walletResp.CashBalance)
	}
}

// --- 7-11. Security-review additions (Stage 9.3 `security` specialist) ---
//
// The six cases above cover the implementer's own threat list. These five
// close the gaps that review identified: cross-TENANT (not merely
// cross-player) isolation, the "the request body is ignored entirely"
// claim asserted rather than merely documented, principal-type
// enforcement, the interaction with a genuine concurrently-delivered
// provider webhook, and the non-`Succeeded` terminal state
// (`declined`) that requireDepositAwaitingCallback exists for - test 5
// only covered the already-`succeeded` case, which postDepositSuccess
// would have short-circuited on its own anyway.

// 7. A player authenticated in tenant B must not be able to settle a
// deposit intent belonging to tenant A - the isolation property
// CLAUDE.md requires of every tenant-owned entity, and a strictly
// stronger statement than test 2's same-tenant wrong-player case.
func TestSimulateDepositCallback_CrossTenantDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	mustRegisterCapability(t, pool, tenantA.ID, mockProvider)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	mustRegisterCapability(t, pool, tenantB.ID, mockProvider)

	victim := mustRegisterPlayer(t, srv, brandA.Slug)
	mustActivatePlayer(t, pool, tenantA.ID, victim.ID)
	attacker := mustRegisterPlayer(t, srv, brandB.Slug)
	mustActivatePlayer(t, pool, tenantB.ID, attacker.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", victim.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 4321, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != 201 {
		t.Fatalf("expected 201 initiating tenant A's deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)

	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", attacker.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 404 {
		t.Fatalf("expected 404 for a tenant B player naming a tenant A deposit id, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me/deposits/"+intent.ID, victim.Tokens.AccessToken)
	var reread depositIntentResponse
	decodeBody(t, resp, &reread)
	if reread.Status != "pending" {
		t.Errorf("expected tenant A's deposit to remain 'pending', got %q", reread.Status)
	}
}

// 8. The request body is genuinely ignored. A caller supplying a larger
// amount, a different asset, another player's provider_reference, a
// different outcome and a reversal event_type must still settle only
// their OWN intent, for its own amount, in its own asset - this is the
// property the handler's whole trust-boundary argument rests on, so it
// is asserted here rather than left to the doc comment.
func TestSimulateDepositCallback_RequestBodyIsIgnored(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	victim := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, victim.ID)
	attacker := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, attacker.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", victim.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 1111111, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var victimIntent depositIntentResponse
	decodeBody(t, resp, &victimIntent)
	victimRef := providerReferenceFromRedirectURL(victimIntent.RedirectURL)

	const attackerAmount int64 = 1000
	resp = postJSON(t, srv, "/v1/me/deposits", attacker.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": attackerAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var attackerIntent depositIntentResponse
	decodeBody(t, resp, &attackerIntent)

	resp = postJSON(t, srv, "/v1/me/deposits/"+attackerIntent.ID+"/simulate-callback", attacker.Tokens.AccessToken, map[string]any{
		"amount":             99999999,
		"asset_code":         "USD",
		"provider_reference": victimRef,
		"outcome":            "declined",
		"event_type":         "deposit_reversal",
		"deposit_id":         victimIntent.ID,
		"player_account_id":  victim.ID.String(),
		"tenant_id":          tenant.ID.String(),
	})
	if resp.StatusCode != 200 {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 200, got %d (%s)", resp.StatusCode, body.Message)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", attacker.Tokens.AccessToken)
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != attackerAmount {
		t.Fatalf("expected exactly the caller's own intent amount (%d) to be credited, got %d", attackerAmount, walletResp.CashBalance)
	}

	resp = getJSON(t, srv, "/v1/me/wallets/USD", attacker.Tokens.AccessToken)
	if resp.StatusCode == 200 {
		var usd walletSummaryResponse
		decodeBody(t, resp, &usd)
		if usd.CashBalance != 0 {
			t.Errorf("body-supplied asset_code credited a USD wallet with %d", usd.CashBalance)
		}
	} else {
		resp.Body.Close()
	}

	resp = getJSON(t, srv, "/v1/me/deposits/"+victimIntent.ID, victim.Tokens.AccessToken)
	var reread depositIntentResponse
	decodeBody(t, resp, &reread)
	if reread.Status != "pending" {
		t.Errorf("body-supplied provider_reference reached another player's intent (now %q)", reread.Status)
	}
}

// 9. A STAFF bearer token must not reach this player-self-service route -
// auth.RequirePlayerPrincipal's guarantee, asserted on this route
// specifically because it mutates financial state.
func TestSimulateDepositCallback_StaffTokenDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 2222, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)

	staffToken, err := issuer.Issue(uuid.NewString(), tenant.ID, auth.RoleTenantAdmin, auth.PrincipalStaff, time.Hour)
	if err != nil {
		t.Fatalf("issue staff token: %v", err)
	}
	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", staffToken, map[string]any{})
	if resp.StatusCode != 403 {
		t.Fatalf("expected 403 for a staff bearer token, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// 10. A genuine, correctly-signed provider webhook delivered CONCURRENTLY
// with this route against the same intent must still post exactly one
// ledger transaction. Test 6 races this route against itself (identical
// payloads); this races it against the OTHER entry point into the same
// pipeline, which is the realistic staging failure mode - an acceptance
// script calling simulate-callback while a retrying webhook lands.
func TestSimulateDepositCallback_RacesRealWebhookWithoutDoubleCredit(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const depositAmount int64 = 7654
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)

	payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "",
		payments.OutcomeSucceeded, depositAmount, "EUR", "", false)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				r := postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
				r.Body.Close()
				return
			}
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/webhooks/payments/"+tenant.Slug+"/mock", bytes.NewReader(payload.Body))
			if err != nil {
				t.Errorf("build webhook request: %v", err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			for k, vs := range payload.Header {
				for _, v := range vs {
					req.Header.Add(k, v)
				}
			}
			r, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("webhook delivery: %v", err)
				return
			}
			r.Body.Close()
		}(i)
	}
	wg.Wait()

	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, providerRef); got != 1 {
		t.Fatalf("expected exactly 1 ledger transaction racing the real webhook, got %d", got)
	}
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != depositAmount {
		t.Fatalf("expected exactly one deposit's worth (%d), got %d", depositAmount, walletResp.CashBalance)
	}
}

// 11. A DECLINED intent must not be resurrectable. This is the case
// requireDepositAwaitingCallback actually exists for: verified during
// Stage 9.3 security review by driving the same Succeeded payload
// straight into Orchestrator.ReceiveCallback with the handler's gate
// bypassed, which DOES credit the ledger and flip the intent to
// 'succeeded' - correct for a genuine provider webhook (the provider is
// authoritative about its own settlement), wrong for a caller-triggered
// simulation. Neither receiveDepositCallback's terminal-state
// short-circuit (it only fires for non-Succeeded outcomes) nor
// postDepositSuccess's idempotency short-circuit (it only fires for
// already-Succeeded intents) covers it, so this handler's own gate is
// the only thing standing between a player and a ledger credit for a
// deposit the platform already told them was declined.
func TestSimulateDepositCallback_RejectedForDeclinedDeposit(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	// MockAmountPlayerDeclineNoCascade drives a synchronous,
	// non-cascadable decline, so the intent reaches 'declined' with a
	// provider_id/provider_reference already assigned.
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": payments.MockAmountPlayerDeclineNoCascade,
		"payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	if intent.Status != "declined" {
		t.Fatalf("expected a 'declined' deposit fixture, got %q", intent.Status)
	}

	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 409 {
		t.Fatalf("expected 409 simulating a settlement callback on a DECLINED deposit, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	if resp.StatusCode == 200 {
		var walletResp walletSummaryResponse
		decodeBody(t, resp, &walletResp)
		if walletResp.CashBalance != 0 {
			t.Fatalf("a declined deposit credited %d to the player's wallet", walletResp.CashBalance)
		}
	} else {
		resp.Body.Close()
	}

	resp = getJSON(t, srv, "/v1/me/deposits/"+intent.ID, player.Tokens.AccessToken)
	var reread depositIntentResponse
	decodeBody(t, resp, &reread)
	if reread.Status != "declined" {
		t.Fatalf("expected the deposit to stay 'declined', got %q", reread.Status)
	}
}

// 12. The player-attributed audit record is written and is forensically
// distinguishable from the ActorSystem 'deposit.posted' record
// receiveDepositCallback writes for a genuine webhook - the whole reason
// recordDepositSimulationAudit exists.
func TestSimulateDepositCallback_WritesDistinguishablePlayerAudit(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServerWithMockSettlement(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": 3333, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	resp = postJSON(t, srv, "/v1/me/deposits/"+intent.ID+"/simulate-callback", player.Tokens.AccessToken, map[string]any{})
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	var actorType, actorID string
	var ip, requestID *string
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT actor_type, actor_id::text, host(ip_address), request_id FROM audit_log
			  WHERE tenant_id = $1 AND target_type = 'deposit_intent' AND target_id = $2
			    AND action = 'payments_simulation.deposit_callback'`,
			tenant.ID, intent.ID).Scan(&actorType, &actorID, &ip, &requestID)
	})
	if err != nil {
		t.Fatalf("read the simulation audit record: %v", err)
	}
	if actorType != "player" || actorID != player.ID.String() {
		t.Errorf("expected the record attributed to the acting player, got actor_type=%q actor_id=%q", actorType, actorID)
	}
	if ip == nil || *ip == "" {
		t.Error("expected the simulation audit record to carry the caller's IP (CLAUDE.md's audit rule)")
	}
	if requestID == nil || *requestID == "" {
		t.Error("expected the simulation audit record to carry the request id, for correlation with request logs")
	}
}
