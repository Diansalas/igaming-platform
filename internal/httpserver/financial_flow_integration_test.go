//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/wallet"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// --- Stage 3B financial HTTP-layer test fixtures ---
//
// This file is the direct sibling of identity_flow_integration_test.go,
// covering the wallet/deposit/withdrawal/provider-capability routes wired
// in financial_routes.go. Every test here runs against real Postgres (RLS
// is the actual isolation mechanism under test, never mocked), and follows
// that file's own conventions: fixtures created directly via the internal
// packages rather than through the very HTTP endpoints under test, plain
// individual test functions rather than table-driven, and a bearer token
// minted through the real register/login HTTP flow for player principals.

// newMockOrchestrator builds a payments.Orchestrator with a single mock
// adapter registered under provider_id "mock" - the same shape
// cmd/platform-api/main.go wires up in production, so RouteProvider/
// ReceiveCallback exercise the exact code path a real deployment runs.
// Returns the concrete *payments.MockProvider too, since some tests need
// to build a signed webhook payload via its own CallbackPayload method.
func newMockOrchestrator() (*payments.Orchestrator, *payments.MockProvider) {
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	return payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": mock}, payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)}).WithPayoutDestinations(pitest.Shared()), mock
}

// newFinancialTestServer is newTestServer (server_integration_test.go)
// plus a wired PaymentOrchestrator - every other Deps field matches that
// helper exactly so identity-route behavior is unaffected.
func newFinancialTestServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *payments.Orchestrator) *httptest.Server {
	t.Helper()
	return newFinancialTestServerWith(t, pool, issuer, orchestrator, nil)
}

// newFinancialTestServerWith is newFinancialTestServer with a Deps mutation hook (B13-B tests remove the payout
// instrument service to prove the fail-closed 503).
func newFinancialTestServerWith(t *testing.T, pool *db.Pool, issuer *auth.Issuer, orchestrator *payments.Orchestrator, mutate func(*Deps)) *httptest.Server {
	t.Helper()
	deps := Deps{
		Logger:              slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                  pool,
		AuthIssuer:          issuer,
		ServiceName:         "platform-api-test",
		AccessTokenTTL:      5 * time.Minute,
		RefreshTokenTTL:     time.Hour,
		PaymentOrchestrator: orchestrator, PaymentsOutboundCredentials: payments.MockCredentialResolver{},
		PersonResolver:    identityresolution.NewMockPersonResolver(),
		PayoutInstruments: pitest.Shared(), // B13-B: a withdrawal request binds a verified payout instrument
	}
	if mutate != nil {
		mutate(&deps)
	}
	srv := httptest.NewServer(New(deps))
	t.Cleanup(srv.Close)
	return srv
}

// registeredPlayer bundles a real, HTTP-registered player's identity and
// tokens - id resolved via GET /v1/me rather than trusted from the
// register call itself, so fixtures built directly against internal
// packages (fundWallet, mustCreateDepositIntent, ...) can reference the
// exact same player_account_id the player's own bearer token carries.
type registeredPlayer struct {
	ID     uuid.UUID
	Email  string
	Tokens tokenPairResponse
}

func mustRegisterPlayer(t *testing.T, srv *httptest.Server, brandSlug string) registeredPlayer {
	t.Helper()
	email := "fin-player-" + uuid.NewString() + "@example.com"
	const password = "a-decent-password-1"
	resp := postJSON(t, srv, "/v1/auth/register", "", map[string]string{
		"brand_slug": brandSlug, "email": email, "password": password,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("failed to register player: status %d", resp.StatusCode)
	}
	var tokens tokenPairResponse
	decodeBody(t, resp, &tokens)

	resp = getJSON(t, srv, "/v1/me", tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("failed to read registered player's own profile: status %d", resp.StatusCode)
	}
	var me meResponse
	decodeBody(t, resp, &me)
	id, err := uuid.Parse(me.ID)
	if err != nil {
		t.Fatalf("failed to parse player id: %v", err)
	}
	return registeredPlayer{ID: id, Email: email, Tokens: tokens}
}

// fundWallet posts a plain Flow 1 deposit (debit psp_clearing, credit
// player_cash) directly via internal/ledger, exactly like
// internal/withdrawal's own integration tests fund a fixture wallet -
// letting a test that needs a real available balance skip the full
// deposit-orchestrator/webhook round trip when that round trip isn't
// itself what's under test.
func fundWallet(t *testing.T, pool *db.Pool, tenantID, brandID, playerAccountID uuid.UUID, assetCode string, amount int64) wallet.Wallet {
	t.Helper()
	var wl wallet.Wallet
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wl, err = wallet.GetOrCreate(ctx, tx, tenantID, brandID, playerAccountID, assetCode)
		if err != nil {
			return err
		}
		cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, &wl.ID, ledger.AccountPlayerCash, assetCode)
		if err != nil {
			return err
		}
		clearingAccountID, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, nil, ledger.AccountPSPClearing, assetCode)
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID:        tenantID,
			TransactionType: ledger.TxDeposit,
			IdempotencyKey:  "test-fund-" + uuid.NewString(),
			CorrelationID:   uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearingAccountID, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("fund wallet: %v", err)
	}
	return wl
}

// mustRegisterCapability writes an active, tenant-wide ProviderCapability
// row mirroring provider's own declared capability exactly, following the
// identical pattern internal/payments' own orchestrator_integration_test.go
// uses (registerCapability) - never a narrower/wider set than what
// NewMockProvider was constructed with.
func mustRegisterCapability(t *testing.T, pool *db.Pool, tenantID uuid.UUID, provider payments.PaymentProvider) {
	t.Helper()
	declared := provider.Capabilities()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := payments.WriteCapability(ctx, tx, provider, tenantID, nil, payments.CapabilityConfig{
			SupportedFiatCurrencies: declared.SupportedFiatCurrencies,
			SupportedCryptoAssets:   declared.SupportedCryptoAssets,
			SupportedPaymentMethods: declared.SupportedPaymentMethods,
			SupportsDeposit:         declared.SupportsDeposit,
			SupportsWithdrawal:      declared.SupportsWithdrawal,
			SupportsRefundReversal:  declared.SupportsRefundReversal,
			AmountLimits:            declared.AmountLimits,
			Priority:                0,
			Status:                  payments.CapabilityActive,
		})
		return err
	})
	if err != nil {
		t.Fatalf("register capability: %v", err)
	}
}

// mustCreateDepositIntent creates a DepositIntent directly via the live
// v2 orchestrator entry point (InitiateDepositAttempt, not through HTTP)
// for a player who is NOT the one under test - e.g. a "victim" fixture in
// a cross-player/cross-tenant isolation test. Requires a routable
// capability already registered for tenantID. Uses the same
// KYCEnforcementDepositGate/MockCredentialResolver pairing the real
// deposit handler wires (deposit_handlers.go) - a fresh test tenant has
// no licence bound, so the KYC gate is a structural not_required pass,
// exactly like every other fixture in this file.
func mustCreateDepositIntent(t *testing.T, pool *db.Pool, orchestrator *payments.Orchestrator, tenantID, brandID, playerAccountID, walletID uuid.UUID, assetCode string, amount int64) payments.DepositIntent {
	t.Helper()
	res, err := orchestrator.InitiateDepositAttempt(context.Background(), pool, payments.KYCEnforcementDepositGate{}, payments.MockCredentialResolver{}, payments.InitiateDepositParams{
		Scope: payments.DepositScope{
			TenantID: tenantID, BrandID: brandID, PlayerAccountID: playerAccountID, WalletID: walletID,
		},
		AssetCode: assetCode, Amount: amount, PaymentMethod: "card", IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatalf("create deposit intent: %v", err)
	}
	return res.Intent
}

// mustCreateWithdrawalRequest creates a WithdrawalRequest directly (not
// through HTTP) for a player who is NOT the one under test. The wallet
// must already have sufficient available balance (see fundWallet).
// mustApproveKYCForWithdrawal seeds an approved kyc_verifications row for
// playerAccountID - ADR 0096 §3.2 point 1 (PRH-I3) requires this before
// ANY withdrawal request can succeed, and this end-to-end HTTP test's own
// registration flow (mustRegisterPlayer) does not itself run a
// verification.
func mustApproveKYCForWithdrawal(t *testing.T, pool *db.Pool, tenantID, brandID, playerAccountID uuid.UUID) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, 'approved', 'mock')`,
			uuid.New(), tenantID, brandID, playerAccountID, account.PersonID)
		return err
	})
	if err != nil {
		t.Fatalf("approve kyc for withdrawal: %v", err)
	}
}

func mustCreateWithdrawalRequest(t *testing.T, pool *db.Pool, tenantID, brandID, playerAccountID, walletID uuid.UUID, assetCode string, amount int64) withdrawal.WithdrawalRequest {
	t.Helper()
	var wr withdrawal.WithdrawalRequest
	instrumentID := pitest.Bind(t, pool, tenantID, playerAccountID, assetCode) // B13-B: a verified MOCK instrument
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
		if err != nil {
			return err
		}
		// ADR 0096 §3.2 point 1 (PRH-I3): every withdrawal now requires a
		// passed, unexpired verification. This helper is used by tests
		// exercising unrelated behavior (cross-player/cross-tenant access
		// control, not KYC), so it seeds one directly rather than making
		// every caller aware of the new gate.
		if _, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, 'approved', 'mock')`,
			uuid.New(), tenantID, brandID, playerAccountID, account.PersonID); err != nil {
			return err
		}
		wr, err = withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: tenantID, BrandID: brandID, PlayerAccountID: playerAccountID, PersonID: account.PersonID, WalletID: walletID,
			AssetCode: assetCode, Amount: amount, IdempotencyKey: uuid.NewString(),
			PayoutInstrumentID: instrumentID, Destinations: pitest.Shared(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("create withdrawal request: %v", err)
	}
	return wr
}

// countWithdrawalApprovals reads withdrawal_approvals for requestID under
// tenantID's own scope - used to prove a denied/failed approval attempt
// left no row behind, not merely that the HTTP call itself failed.
func countWithdrawalApprovals(t *testing.T, pool *db.Pool, tenantID, requestID uuid.UUID) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COUNT(*) FROM withdrawal_approvals WHERE withdrawal_request_id = $1`, requestID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count withdrawal approvals: %v", err)
	}
	return count
}

// jsonBody marshals v (or, for a []byte, uses it verbatim) into a
// request body reader - shared by capabilityPutRequest (a JSON struct
// body) and rawPostJSON (an already-serialized webhook payload that must
// reach the handler byte-for-byte, signature and all).
func jsonBody(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	if b, ok := v.([]byte); ok {
		return bytes.NewReader(b)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	return bytes.NewReader(b)
}

// rawPostJSON posts an already-serialized body with no bearer token - the
// shape every provider webhook call in this file needs, since
// POST /v1/webhooks/payments/{tenantSlug}/{providerID} has no bearer-auth
// middleware by design (a provider webhook is not an authenticated
// platform principal - see financial_routes.go's own comment).
func rawPostJSON(t *testing.T, srv *httptest.Server, path string, body []byte) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// rawPostCallback posts a payments.InboundCallback's body to the public
// webhook route with its own headers (X-Payments-Signature/X-Payments-
// Key-Id) - PAY-WH-TENANT-1 moved the signature out of the JSON body and
// into these headers, so every webhook test that used to post a bare
// signed []byte via rawPostJSON now posts an InboundCallback via this
// helper instead.
func rawPostCallback(t *testing.T, srv *httptest.Server, path string, inbound payments.InboundCallback) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, bytes.NewReader(inbound.Body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, vs := range inbound.Header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

func decodeAPIError(t *testing.T, resp *http.Response) apierror.Error {
	t.Helper()
	var e apierror.Error
	decodeBody(t, resp, &e)
	return e
}

// --- 1. Cross-player deposit read is a 404, not a data leak ---

func TestGetDeposit_CrossPlayerAccessDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	playerA := mustRegisterPlayer(t, srv, brand.Slug)
	playerB := mustRegisterPlayer(t, srv, brand.Slug)

	var walletB wallet.Wallet
	err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		walletB, err = wallet.GetOrCreate(ctx, tx, tenant.ID, brand.ID, playerB.ID, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("create player B wallet: %v", err)
	}
	intentB := mustCreateDepositIntent(t, pool, orchestrator, tenant.ID, brand.ID, playerB.ID, walletB.ID, "EUR", 5000)

	resp := getJSON(t, srv, "/v1/me/deposits/"+intentB.ID.String(), playerA.Tokens.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for player A reading player B's deposit, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if apiErr.Code != apierror.CodeNotFound {
		t.Errorf("expected not_found code, got %q", apiErr.Code)
	}
	if strings.Contains(apiErr.Message, "5000") || strings.Contains(strings.ToLower(apiErr.Message), "eur") {
		t.Errorf("error response appears to leak deposit details: %q", apiErr.Message)
	}

	// Player B can read their own deposit via the same endpoint shape -
	// proves the 404 above is ownership-scoped, not a broken route.
	resp = getJSON(t, srv, "/v1/me/deposits/"+intentB.ID.String(), playerB.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for player B reading their own deposit, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// --- 2. Cross-player withdrawal read/cancel is a 404, and cancel has no effect ---

func TestWithdrawal_CrossPlayerAccessDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	playerA := mustRegisterPlayer(t, srv, brand.Slug)
	playerB := mustRegisterPlayer(t, srv, brand.Slug)

	walletB := fundWallet(t, pool, tenant.ID, brand.ID, playerB.ID, "EUR", 10000)
	wrB := mustCreateWithdrawalRequest(t, pool, tenant.ID, brand.ID, playerB.ID, walletB.ID, "EUR", 3000)

	// Read denied.
	resp := getJSON(t, srv, "/v1/me/withdrawals/"+wrB.ID.String(), playerA.Tokens.AccessToken)
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for player A reading player B's withdrawal, got %d", resp.StatusCode)
	}
	apiErr := decodeAPIError(t, resp)
	if strings.Contains(apiErr.Message, "3000") {
		t.Errorf("error response appears to leak withdrawal amount: %q", apiErr.Message)
	}

	// Cancel denied.
	resp = postJSON(t, srv, "/v1/me/withdrawals/"+wrB.ID.String()+"/cancel", playerA.Tokens.AccessToken, map[string]string{})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for player A cancelling player B's withdrawal, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Confirm B's request is genuinely unchanged - read it back with B's
	// own token, not just infer it from A's failed attempt.
	resp = getJSON(t, srv, "/v1/me/withdrawals/"+wrB.ID.String(), playerB.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 for player B reading their own withdrawal, got %d", resp.StatusCode)
	}
	var wrResp withdrawalRequestResponse
	decodeBody(t, resp, &wrResp)
	if wrResp.State != string(withdrawal.StateRequested) {
		t.Errorf("expected player B's withdrawal to remain 'requested' after A's failed cancel attempt, got %q", wrResp.State)
	}
}

// --- 3. A role without PermWithdrawalApprove cannot approve ---

func TestWithdrawalApprove_RoleWithoutPermissionDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)

	// support and compliance are two of the three non-finance staff roles
	// defined in internal/auth/permission.go's rolePermissions that do NOT
	// carry PermWithdrawalApprove - Stage 3D removed it from tenant_admin
	// too (business decision #4/#5: a broad admin role must never hold
	// withdrawal authority implicitly), so RoleFinance is now the ONLY
	// role that carries it.
	roles := []identity.StaffRole{identity.StaffRoleSupport, identity.StaffRoleCompliance}
	for _, role := range roles {
		staff := mustCreateStaff(t, pool, tenant.ID, role, "role-pw-1")
		tokens := mustLoginStaff(t, srv, tenant.Slug, staff.Email, "role-pw-1")

		resp := postJSON(t, srv, "/v1/admin/withdrawals/"+uuid.NewString()+"/approve", tokens.AccessToken, map[string]string{})
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("expected 403 for role %q approving a withdrawal, got %d", role, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// --- 4. A player token is denied on every admin financial route ---

func TestPlayerRole_AdminFinancialRoutesDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	token := player.Tokens.AccessToken
	randomID := uuid.NewString()

	assertDenied := func(t *testing.T, resp *http.Response, label string) {
		t.Helper()
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Errorf("%s: expected denial, got 200", label)
		}
		if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: expected 403 or 401, got %d", label, resp.StatusCode)
		}
	}

	assertDenied(t, getJSON(t, srv, "/v1/admin/withdrawals", token), "list pending withdrawals")
	assertDenied(t, postJSON(t, srv, "/v1/admin/withdrawals/"+randomID+"/approve", token, map[string]string{}), "approve withdrawal")
	assertDenied(t, postJSON(t, srv, "/v1/admin/withdrawals/"+randomID+"/reject", token, map[string]string{"reason_code": "x"}), "reject withdrawal")
	assertDenied(t, postJSON(t, srv, "/v1/admin/withdrawals/"+randomID+"/submit", token, map[string]string{"payment_method": "card"}), "submit withdrawal")
	assertDenied(t, capabilityPutRequest(t, srv, "mock", token, validCapabilityBody()), "write provider capability")
}

// --- 5. platform_admin's nil-tenant token is denied on every admin
// financial route by RequireTenantScope, exactly like every other
// tenant-scoped admin route in this codebase ---

func TestPlatformAdmin_AdminFinancialRoutesDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	admin := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, "admin-fin-pw-1")
	tokens := mustLoginStaff(t, srv, "", admin.Email, "admin-fin-pw-1")
	token := tokens.AccessToken
	randomID := uuid.NewString()

	assertForbidden := func(t *testing.T, resp *http.Response, label string) {
		t.Helper()
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s: expected 403 for platform_admin's nil-tenant token, got %d", label, resp.StatusCode)
		}
	}

	assertForbidden(t, getJSON(t, srv, "/v1/admin/withdrawals", token), "list pending withdrawals")
	assertForbidden(t, postJSON(t, srv, "/v1/admin/withdrawals/"+randomID+"/approve", token, map[string]string{}), "approve withdrawal")
	assertForbidden(t, postJSON(t, srv, "/v1/admin/withdrawals/"+randomID+"/reject", token, map[string]string{"reason_code": "x"}), "reject withdrawal")
	assertForbidden(t, postJSON(t, srv, "/v1/admin/withdrawals/"+randomID+"/submit", token, map[string]string{"payment_method": "card"}), "submit withdrawal")
	assertForbidden(t, capabilityPutRequest(t, srv, "mock", token, validCapabilityBody()), "write provider capability")
}

// --- 6. finance lacks PermProviderConfigWrite; only tenant_admin has it ---

func TestProviderCapabilityWrite_FinanceRoleDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)

	if auth.RoleHasPermission(auth.RoleFinance, auth.PermProviderConfigWrite) {
		t.Fatal("test assumption violated: RoleFinance now has PermProviderConfigWrite per internal/auth/permission.go")
	}
	if !auth.RoleHasPermission(auth.RoleTenantAdmin, auth.PermProviderConfigWrite) {
		t.Fatal("test assumption violated: RoleTenantAdmin no longer has PermProviderConfigWrite per internal/auth/permission.go")
	}

	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-pw-1")
	financeTokens := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-pw-1")

	resp := capabilityPutRequest(t, srv, "mock", financeTokens.AccessToken, validCapabilityBody())
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for finance writing provider capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-fin-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-fin-pw-1")
	resp = capabilityPutRequest(t, srv, "mock", tenantAdminTokens.AccessToken, validCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for tenant_admin writing provider capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func validCapabilityBody() map[string]any {
	return map[string]any{
		"supported_fiat_currencies": []string{"EUR"},
		"supported_payment_methods": []string{"card"},
		"supports_deposit":          true,
		"supports_withdrawal":       true,
		"priority":                  0,
		"status":                    "active",
	}
}

func capabilityPutRequest(t *testing.T, srv *httptest.Server, providerID, bearerToken string, body any) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/v1/admin/providers/"+providerID+"/capability", jsonBody(t, body))
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+bearerToken)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	return resp
}

// --- 7. Cross-tenant withdrawal approval is a 404, with no approval row
// created in either tenant ---

func TestWithdrawalApprove_CrossTenantDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, _ := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)

	financeA := mustCreateStaff(t, pool, tenantA.ID, identity.StaffRoleFinance, "finance-a-pw-1")
	financeATokens := mustLoginStaff(t, srv, tenantA.Slug, financeA.Email, "finance-a-pw-1")

	// A real withdrawal belonging entirely to tenant B.
	var playerAccountB uuid.UUID
	err := pool.WithTenant(context.Background(), tenantB.ID, func(ctx context.Context, tx pgx.Tx) error {
		account, err := identity.RegisterPlayer(ctx, tx, brandB, "cross-tenant-wd@example.com", "hash")
		playerAccountB = account.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed player B: %v", err)
	}
	walletB := fundWallet(t, pool, tenantB.ID, brandB.ID, playerAccountB, "EUR", 10000)
	wrB := mustCreateWithdrawalRequest(t, pool, tenantB.ID, brandB.ID, playerAccountB, walletB.ID, "EUR", 4000)

	resp := postJSON(t, srv, "/v1/admin/withdrawals/"+wrB.ID.String()+"/approve", financeATokens.AccessToken, map[string]string{})
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected 404 for tenant A's finance staff approving tenant B's withdrawal, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	if got := countWithdrawalApprovals(t, pool, tenantA.ID, wrB.ID); got != 0 {
		t.Errorf("expected no withdrawal_approvals row visible from tenant A's scope, got %d", got)
	}
	if got := countWithdrawalApprovals(t, pool, tenantB.ID, wrB.ID); got != 0 {
		t.Errorf("expected no withdrawal_approvals row created for tenant B's withdrawal either, got %d", got)
	}
}

// --- 8. Full happy path: register -> deposit -> signed webhook ->
// wallet reflects balance -> withdrawal -> staff list/approve -> hold
// reflected ---

func TestFinancialHappyPath_EndToEnd(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)

	tenantAdmin := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleTenantAdmin, "ta-happy-pw-1")
	tenantAdminTokens := mustLoginStaff(t, srv, tenant.Slug, tenantAdmin.Email, "ta-happy-pw-1")
	resp := capabilityPutRequest(t, srv, "mock", tenantAdminTokens.AccessToken, validCapabilityBody())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 configuring the mock provider's capability, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Register.
	player := mustRegisterPlayer(t, srv, brand.Slug)
	// Stage 9 (identity-compliance): InitiateDeposit now consults
	// internal/rg.EvaluateEligibility, exactly like internal/casino's
	// LaunchGame/postBet have since Stage 4D-RG - which requires
	// PlayerAccountStatus == active, not the pending_verification a fresh
	// registration starts in. mustActivatePlayer stands in for the (out
	// of scope here) real email-verification flow, mirroring the
	// established Stage 6/7 defining-acceptance-test convention
	// (stage6_b2c_sportsbook_acceptance_test.go,
	// stage7_b2c_casino_acceptance_test.go both activate immediately
	// after registration, before any financial action).
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	// Deposit - amount deliberately NOT one of MockProvider's magic
	// synchronous-outcome amounts, so this takes the normal
	// OutcomePending/async-callback path a real hosted-redirect deposit
	// would.
	const depositAmount int64 = 15000
	resp = postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": depositAmount, "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 initiating deposit, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	if intent.Status != "pending" {
		t.Fatalf("expected deposit intent status 'pending', got %q", intent.Status)
	}
	if !strings.HasPrefix(intent.RedirectURL, "https://mock-psp.invalid/pay/") {
		t.Fatalf("expected a mock redirect URL, got %q", intent.RedirectURL)
	}
	providerRef := strings.TrimPrefix(intent.RedirectURL, "https://mock-psp.invalid/pay/")

	// Simulate the provider's webhook callback - signed, as a real
	// delivery would be; HandleCallback rejects anything else (see
	// scenario 10 below).
	payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, depositAmount, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
	if resp.StatusCode != http.StatusOK {
		body := decodeAPIError(t, resp)
		t.Fatalf("expected 200 processing the signed deposit callback, got %d (%s)", resp.StatusCode, body.Message)
	}
	resp.Body.Close()

	// Wallet reflects the credited balance.
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 reading wallet, got %d", resp.StatusCode)
	}
	var walletResp walletSummaryResponse
	decodeBody(t, resp, &walletResp)
	if walletResp.CashBalance != depositAmount || walletResp.AvailableBalance != depositAmount {
		t.Fatalf("expected cash/available balance %d after deposit, got cash=%d available=%d", depositAmount, walletResp.CashBalance, walletResp.AvailableBalance)
	}

	// Withdrawal - ADR 0096 §3.2 point 1 (PRH-I3) requires a passed KYC
	// verification for every withdrawal, from the first one onward.
	mustApproveKYCForWithdrawal(t, pool, tenant.ID, brand.ID, player.ID)
	const withdrawalAmount int64 = 4000
	resp = postJSON(t, srv, "/v1/me/withdrawals", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": withdrawalAmount, "idempotency_key": uuid.NewString(),
		"payout_instrument_id": pitest.Bind(t, pool, tenant.ID, player.ID, "EUR").String(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 requesting withdrawal, got %d", resp.StatusCode)
	}
	var wr withdrawalRequestResponse
	decodeBody(t, resp, &wr)
	if wr.State != string(withdrawal.StateRequested) {
		t.Fatalf("expected withdrawal state 'requested', got %q", wr.State)
	}

	// Staff (finance role - holds PermWithdrawalApprove) lists the queue,
	// which promotes `requested` -> `pending_review`, then approves it.
	finance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleFinance, "finance-happy-pw-1")
	financeTokens := mustLoginStaff(t, srv, tenant.Slug, finance.Email, "finance-happy-pw-1")

	resp = getJSON(t, srv, "/v1/admin/withdrawals", financeTokens.AccessToken)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 listing pending withdrawals, got %d", resp.StatusCode)
	}
	var pending []staffWithdrawalResponse
	decodeBody(t, resp, &pending)
	found := false
	for _, p := range pending {
		if p.ID == wr.ID {
			found = true
			if p.State != string(withdrawal.StatePendingReview) {
				t.Errorf("expected the listed withdrawal to be promoted to 'pending_review', got %q", p.State)
			}
		}
	}
	if !found {
		t.Fatalf("expected withdrawal %s to appear in the staff review queue", wr.ID)
	}

	// The zero-config withdrawal policy fallback fails closed (threshold
	// 0, always requiring 2 approvals - see internal/withdrawal/policy.go)
	// specifically because no tenant has configured a real,
	// asset-appropriate threshold yet. Configure one here so this
	// end-to-end happy path exercises the below-threshold, single-
	// approval branch it's actually testing.
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO withdrawal_policies (tenant_id, asset_code, approval_threshold_minor_units, required_approvals, effective_from)
			 VALUES ($1, 'EUR', 1000000, 2, now() - interval '1 hour')`,
			tenant.ID,
		)
		return err
	}); err != nil {
		t.Fatalf("configure withdrawal policy: %v", err)
	}

	resp = postJSON(t, srv, "/v1/admin/withdrawals/"+wr.ID+"/approve", financeTokens.AccessToken, map[string]string{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 approving withdrawal, got %d", resp.StatusCode)
	}
	var approveResp approveWithdrawalResponse
	decodeBody(t, resp, &approveResp)
	if !approveResp.Approved {
		t.Fatalf("expected withdrawalAmount=%d (below the default four-eyes threshold) to be approved by a single finance approval, got approved=false", withdrawalAmount)
	}

	// Wallet's held_for_withdrawal reflects the hold placed at request
	// time (Flow 3 Step A already ran inside RequestWithdrawal, before
	// any staff action) - cash_balance is reduced by the same amount,
	// available_balance mirrors cash_balance per wallet.Summary's own
	// documented formula (it must never double-subtract the hold).
	resp = getJSON(t, srv, "/v1/me/wallets/EUR", player.Tokens.AccessToken)
	decodeBody(t, resp, &walletResp)
	if walletResp.HeldForWithdrawal != withdrawalAmount {
		t.Errorf("expected held_for_withdrawal=%d, got %d", withdrawalAmount, walletResp.HeldForWithdrawal)
	}
	wantCash := depositAmount - withdrawalAmount
	if walletResp.CashBalance != wantCash || walletResp.AvailableBalance != wantCash {
		t.Errorf("expected cash/available balance %d after the withdrawal hold, got cash=%d available=%d", wantCash, walletResp.CashBalance, walletResp.AvailableBalance)
	}
}

// --- 9. Forged webhook: a provider_reference from a DIFFERENT tenant's
// deposit is rejected as not-found, with no ledger effect in either tenant ---

func TestPaymentWebhook_ForgedCrossTenantProviderReferenceDenied(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenantA := mustCreateTenant(t, pool)
	brandA := mustCreateBrand(t, pool, tenantA)
	tenantB := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenantB) // tenant B just needs to exist/be active for the webhook route lookup

	mustRegisterCapability(t, pool, tenantA.ID, mockProvider)

	playerA := mustRegisterPlayer(t, srv, brandA.Slug)
	// Stage 9 (identity-compliance): see TestFinancialHappyPath_EndToEnd's
	// identical comment - InitiateDeposit now requires an active (not
	// pending_verification) player account.
	mustActivatePlayer(t, pool, tenantA.ID, playerA.ID)
	var walletA wallet.Wallet
	err := pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		walletA, err = wallet.GetOrCreate(ctx, tx, tenantA.ID, brandA.ID, playerA.ID, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("create player A wallet: %v", err)
	}
	intentA := mustCreateDepositIntent(t, pool, orchestrator, tenantA.ID, brandA.ID, playerA.ID, walletA.ID, "EUR", 7500)
	if intentA.ProviderReference == nil {
		t.Fatalf("expected tenant A's deposit intent to have a provider reference")
	}

	// A caller who somehow learned tenant A's own provider_reference (they
	// are sequential and not secret - see mock.go's own comment) posts it,
	// signed FOR TENANT A, to TENANT B's webhook endpoint instead.
	// PAY-WH-TENANT-1 (ADR 0090): this now fails at signature verification
	// (401), never even reaching a "not found" lookup under B's scope -
	// the tenant is bound into what is verified, not just an RLS-scoped
	// read after the fact.
	forgedPayload := mockProvider.CallbackPayload(tenantA.ID, payments.CallbackEventDeposit, *intentA.ProviderReference, "", payments.OutcomeSucceeded, 7500, "EUR", "", false)
	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenantB.Slug+"/mock", forgedPayload)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 for a cross-tenant forged provider_reference (tenant-bound signature verification fails), got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// No ledger effect anywhere: tenant A's own intent is untouched.
	var reread payments.DepositIntent
	err = pool.WithTenant(context.Background(), tenantA.ID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reread, err = payments.GetDepositIntentByID(ctx, tx, intentA.ID)
		return err
	})
	if err != nil {
		t.Fatalf("reread tenant A's deposit intent: %v", err)
	}
	if reread.Status != payments.DepositIntentPending {
		t.Errorf("expected tenant A's deposit intent to remain 'pending' after the forged cross-tenant callback, got %q", reread.Status)
	}
	if reread.LedgerTransactionID != nil {
		t.Errorf("expected no ledger transaction to have been posted for tenant A's deposit, got %s", *reread.LedgerTransactionID)
	}
}

// --- 10. An unsigned/garbage-signature webhook payload is rejected ---

func TestPaymentWebhook_UnsignedPayloadRejected(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	// Entirely unsigned: no X-Payments-Signature/X-Payments-Key-Id headers
	// at all (PAY-WH-TENANT-1 moved the signature out of the JSON body).
	unsignedBody := []byte(`{"event_type":"deposit","provider_reference":"mock-does-not-matter","outcome":"succeeded","amount":1000,"asset_code":"EUR"}`)
	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payments.InboundCallback{Body: unsignedBody})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 rejecting an unsigned webhook payload, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A garbage/incorrect signature is rejected identically - not merely
	// "missing header" handling.
	garbageHeader := make(http.Header)
	garbageHeader.Set("X-Payments-Signature", "v1="+strings.Repeat("a", 64))
	garbageHeader.Set("X-Payments-Key-Id", "mock-v1")
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payments.InboundCallback{Header: garbageHeader, Body: unsignedBody})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 rejecting a garbage-signature webhook payload, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Confirm this genuinely reached the signature check rather than
	// failing earlier for an unrelated reason (e.g. tenant lookup) - the
	// SAME provider_reference, correctly signed, is accepted past
	// authentication. PRH-payments-callback-cutover (ADR 0095 §6.1 step 5/
	// §6.2, S95-C4): a callback naming no resolvable attempt is no longer
	// ErrDepositIntentNotFound/404 - it is durably receipted as
	// `deferred_unresolved` and answered with the SAME uniform 200 body as
	// every other disposition, so a verified sender can never learn
	// whether the reference exists. This still proves the signature gate
	// itself is not what blocked a well-formed request: an unsigned/
	// garbage-signature request above got 401, and this identical payload,
	// correctly signed, gets 200.
	signedButUnknown := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "mock-does-not-matter", "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
	resp = rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", signedButUnknown)
	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected a correctly-signed callback for an unresolvable reference to be durably deferred (200), not rejected; got %d", resp.StatusCode)
	}
	resp.Body.Close()
}
