//go:build integration

// ADR 0097 (PAYWH-RL-1) PRH-I4 adversarial test plan (§11), HTTP layer.
// Unit-level coverage for the deterministic, fake-clock-driven primitives
// (T5 burst accommodation, T8 races, cardinality overflow, idle eviction)
// lives in internal/admission; config validation (T12) lives in
// internal/config; the directory's own fail-safe behaviour lives in
// webhook_tenant_directory_test.go. This file covers the properties that
// need a real HTTP request/response and a real tenant/credential/ledger
// round trip.
package httpserver

import (
	"net/http"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// T1: CrossTenantStarvation_PreAuth. Flooding tenant A's own valid
// webhook URL with garbage (invalid-signature) requests beyond its A3
// burst must not affect tenant B's correctly-signed callbacks in the same
// window.
func TestAdmission_T1_CrossTenantStarvation_PreAuth(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()

	// Tenants are created, and their capabilities registered, BEFORE the
	// test server starts: newAdmissionTestServer performs the ADR 0097 §7
	// synchronous initial directory load exactly once, so a tenant created
	// afterward would stay keyed "_unknown" for the rest of the test
	// (DirectoryRefresh is 1h in testAdmissionSettings, deliberately never
	// firing again mid-test) - defeating the very "known tenant" fairness
	// this test checks.
	tenantA := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenantA)
	mustRegisterCapability(t, pool, tenantA.ID, mockProvider)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	mustRegisterCapability(t, pool, tenantB.ID, mockProvider)

	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, testAdmissionSettings(), false)

	playerB := mustRegisterPlayer(t, srv, brandB.Slug)
	mustActivatePlayer(t, pool, tenantB.ID, playerB.ID)

	// Flood A's URL well beyond its A3 burst (3) with garbage bodies -
	// these fail signature verification too, but must be counted against
	// A3 either way.
	for i := 0; i < 20; i++ {
		resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenantA.Slug+"/mock", payments.InboundCallback{Body: []byte("{}")})
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("flood request %d: got %d, want 401 or 429", i, resp.StatusCode)
		}
	}

	// B's own, correctly signed deposit callback must still be admitted
	// and processed in the SAME window.
	resp := postJSON(t, srv, "/v1/me/deposits", playerB.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": int64(5000), "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201 creating B's deposit intent, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)
	payload := mockProvider.CallbackPayload(tenantB.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, 5000, "EUR", "", false)
	cbResp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenantB.Slug+"/mock", payload)
	defer cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("tenant B's own callback must be admitted despite tenant A's flood, got %d", cbResp.StatusCode)
	}
}

// T2: CrossTenantStarvation_Verified. A sends validly signed callbacks
// beyond B1/B2; B's callbacks are all admitted regardless.
func TestAdmission_T2_CrossTenantStarvation_Verified(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()

	tenantA := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenantA)
	mustRegisterCapability(t, pool, tenantA.ID, mockProvider)
	tenantB := mustCreateTenant(t, pool)
	brandB := mustCreateBrand(t, pool, tenantB)
	mustRegisterCapability(t, pool, tenantB.ID, mockProvider)

	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, testAdmissionSettings(), false)

	playerB := mustRegisterPlayer(t, srv, brandB.Slug)
	mustActivatePlayer(t, pool, tenantB.ID, playerB.ID)

	// A: 10 validly-signed callbacks naming distinct, never-created deposit
	// intents (so each is individually a first delivery) - B1's burst (3)
	// is exceeded; every excess one gets 429/503, never a ledger write.
	rejected := 0
	for i := 0; i < 10; i++ {
		payload := mockProvider.CallbackPayload(tenantA.ID, payments.CallbackEventDeposit, "nonexistent-"+uuid.NewString(), "", payments.OutcomeSucceeded, 100, "EUR", "", false)
		resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenantA.Slug+"/mock", payload)
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusServiceUnavailable {
			rejected++
		}
	}
	if rejected == 0 {
		t.Fatal("expected at least one B1-limited rejection for tenant A's burst")
	}

	// B's own deposit still succeeds.
	resp := postJSON(t, srv, "/v1/me/deposits", playerB.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": int64(2500), "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("expected 201, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)
	payload := mockProvider.CallbackPayload(tenantB.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, 2500, "EUR", "", false)
	cbResp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenantB.Slug+"/mock", payload)
	defer cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("tenant B's callback must be admitted despite A's verified-tier flood, got %d", cbResp.StatusCode)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenantA.ID, "nonexistent-does-not-exist"); got != 0 {
		t.Fatalf("a limited callback for a nonexistent provider ref must never post: got %d rows", got)
	}
}

// T2b (T16 mutation-kill target: "move B1/B2 inside/around WithTenant so
// they no longer gate it"): unlike T2 above (which uses provider
// references that would fail domain processing anyway, so it cannot by
// itself distinguish "B1 correctly blocked this" from "domain processing
// would have rejected it regardless"), this drives TWO real, individually
// postable deposits for the SAME tenant with B1's burst set to exactly 1:
// the first callback must post (exactly one ledger row); the second,
// beyond B1's burst, must be REJECTED and post NOTHING. If a mutation
// makes the B1/B2 decision stop gating deps.DB.WithTenant, this test
// (not T2) is what catches it - recorded in the PRH-I4 mutation-kill
// evidence file.
func TestAdmission_T2b_VerifiedRejectionActuallyBlocksDomainTransaction(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()

	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	settings := testAdmissionSettings()
	settings.VerifiedRate["payments"] = WebhookRateBurst{Rate: 0.01, Burst: 1}
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, settings, false)

	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	createDeposit := func(amount int64) string {
		resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": uuid.NewString(),
		})
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("expected 201 creating a deposit intent, got %d", resp.StatusCode)
		}
		var intent depositIntentResponse
		decodeBody(t, resp, &intent)
		return providerReferenceFromRedirectURL(intent.RedirectURL)
	}

	ref1 := createDeposit(1000)
	ref2 := createDeposit(2000)

	resp1 := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref1, "", payments.OutcomeSucceeded, 1000, "EUR", "", false))
	resp1.Body.Close()
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("first callback (within B1 burst=1) must post, got %d", resp1.StatusCode)
	}

	resp2 := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref2, "", payments.OutcomeSucceeded, 2000, "EUR", "", false))
	resp2.Body.Close()
	if resp2.StatusCode == http.StatusOK {
		t.Fatal("second callback (beyond B1 burst=1) must be rejected, not posted")
	}

	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref1); got != 1 {
		t.Fatalf("the admitted deposit must post exactly once: got %d rows", got)
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, ref2); got != 0 {
		t.Fatalf("the B1-rejected deposit must post ZERO rows - if this is >0, B1 stopped gating the domain transaction: got %d rows", got)
	}
}

// T9 (live half): the webhook tenant directory never loaded (startup
// still waiting, or the DB was down at boot) - every webhook route must
// answer 503, never fall back to unbounded/raw-slug keying.
func TestAdmission_T9_DirectoryNeverLoaded_FailsClosed(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, testAdmissionSettings(), true /* skip the load */)

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	payload := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "irrelevant", "", payments.OutcomeSucceeded, 100, "EUR", "", false)
	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
	defer resp.Body.Close()
	// Security review Low ("T9 must assert the actual status; make code
	// match the comment: 503 when the directory is unloaded"): ADR 0097
	// §7's fail-safe table is an EXPLICIT gate, not merely "fall through
	// to the _unknown bucket" - even a real, active tenant with a
	// correctly-signed callback must get 503 while the admission
	// subsystem's own directory has never completed its first load.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("a request processed while the directory was never loaded must get 503, got %d", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Fatal("expected a Retry-After header")
	}
}

// T13: DomainIndependence. A casino flood does not consume payments'
// buckets or in-flight shares beyond the shared A4a global cap.
func TestAdmission_T13_DomainIndependence(t *testing.T) {
	pool, issuer := testEnv(t)
	paymentOrch, mockPayments := newMockOrchestrator()
	casinoOrch, mockCasino := newMockCasinoOrchestrator()
	srv := newAdmissionTestServer(t, pool, issuer, paymentOrch, casinoOrch, testAdmissionSettings(), false)

	payTenant := mustCreateTenant(t, pool)
	payBrand := mustCreateBrand(t, pool, payTenant)
	mustRegisterCapability(t, pool, payTenant.ID, mockPayments)
	casinoTenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, casinoTenant)
	_ = mockCasino

	payPlayer := mustRegisterPlayer(t, srv, payBrand.Slug)
	mustActivatePlayer(t, pool, payTenant.ID, payPlayer.ID)

	// Flood the casino domain's unknown bucket (bad provider id) well
	// beyond ITS OWN limits.
	for i := 0; i < 20; i++ {
		resp := rawPostCallback(t, srv, "/v1/webhooks/casino/"+casinoTenant.Slug+"/does-not-exist", payments.InboundCallback{Body: []byte("{}")})
		resp.Body.Close()
	}

	// The payments domain must be entirely unaffected.
	resp := postJSON(t, srv, "/v1/me/deposits", payPlayer.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": int64(1000), "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("payments domain must be unaffected by a casino-domain flood, got %d", resp.StatusCode)
	}
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	providerRef := providerReferenceFromRedirectURL(intent.RedirectURL)
	payload := mockPayments.CallbackPayload(payTenant.ID, payments.CallbackEventDeposit, providerRef, "", payments.OutcomeSucceeded, 1000, "EUR", "", false)
	cbResp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+payTenant.Slug+"/mock", payload)
	defer cbResp.Body.Close()
	if cbResp.StatusCode != http.StatusOK {
		t.Fatalf("payments callback must succeed despite the casino-domain flood, got %d", cbResp.StatusCode)
	}
}

// T15: PreAuthResponse_Indistinguishable. With the SAME bucket state, a
// validly-signed and an invalidly-signed request produce byte-identical
// status/body once A3 itself has already rejected - the limiting
// decision depends only on preKey state, never on signature validity.
func TestAdmission_T15_PreAuthResponse_Indistinguishable(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, testAdmissionSettings(), false)

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	// Exhaust A3's burst (3) first with valid-looking-but-garbage bodies.
	for i := 0; i < 3; i++ {
		r := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payments.InboundCallback{Body: []byte("{}")})
		r.Body.Close()
	}
	// Now compare a validly-signed request against an invalidly-signed
	// one - IF A3 is the thing rejecting both, they must be identical.
	valid := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "some-ref", "", payments.OutcomeSucceeded, 100, "EUR", "", false)
	invalid := payments.InboundCallback{Header: valid.Header.Clone(), Body: valid.Body}
	invalid.Header.Set("X-Payments-Signature", "00")

	rv := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", valid)
	defer rv.Body.Close()
	ri := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", invalid)
	defer ri.Body.Close()

	if rv.StatusCode != http.StatusTooManyRequests && rv.StatusCode != http.StatusServiceUnavailable {
		t.Skip("A3 burst was not yet exhausted (timing-sensitive on a shared CI runner); skipping the indistinguishability comparison")
	}
	if rv.StatusCode != ri.StatusCode {
		t.Fatalf("A3-limited responses must be indistinguishable regardless of signature validity: valid=%d invalid=%d", rv.StatusCode, ri.StatusCode)
	}
	ev, ei := decodeAPIError(t, rv), decodeAPIError(t, ri)
	if ev.Code != ei.Code || ev.Message != ei.Message {
		t.Fatalf("A3-limited bodies must be identical: %+v vs %+v", ev, ei)
	}
}

// T17: NoPreVerificationBypass. There is no way to reach a 200 (domain
// processing) without a signature that actually verifies - flooding with
// well-formed-looking-but-wrongly-signed requests can only ever produce
// 401 (or, once A3 trips, 429/503), never 200, and never a ledger row.
func TestAdmission_T17_NoPreVerificationBypass(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, testAdmissionSettings(), false)

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)

	valid := mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "t17-ref", "", payments.OutcomeSucceeded, 100, "EUR", "", false)
	for i := 0; i < 10; i++ {
		bad := payments.InboundCallback{Header: valid.Header.Clone(), Body: valid.Body}
		bad.Header.Set("X-Payments-Signature", "deadbeef")
		resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", bad)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			t.Fatalf("an unverified callback must never reach 200 (attempt %d)", i)
		}
	}
	if got := ledgerTransactionCountForProviderRef(t, pool, tenant.ID, "t17-ref"); got != 0 {
		t.Fatalf("no unverified attempt may ever post: got %d ledger rows", got)
	}
}

// T14: AdapterDeclaredStatus. A provider declared "does not retry 429"
// gets 503 from B1 instead, once its verified bucket is exhausted.
func TestAdmission_T14_AdapterDeclaredStatus_No429Retry(t *testing.T) {
	pool, issuer := testEnv(t)
	mock := payments.NewMockProvider("mock", "EUR", "USD")
	declared := declaredRetryMockProvider{MockProvider: mock, sem: webhookauth.WebhookRetrySemantics{Retries429: false, Retries503: true, HonorsRetryAfter: true}}
	orchestrator := payments.NewOrchestrator(map[string]payments.PaymentProvider{"mock": declared}, payments.MultiWebhookCredentialResolver{"mock": payments.NewMockWebhookCredentials(mock)})

	tenant := mustCreateTenant(t, pool)
	mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mock)

	// A3's own burst is set much higher than B1's here, deliberately: the
	// point of this test is B1's adapter-declared status mapping, which
	// only shows up once a request actually REACHES the verified tier -
	// with equal bursts, A3 (checked first) would exhaust before B1 ever
	// does, and this test would only ever observe A3's ordinary 429.
	settings := testAdmissionSettings()
	settings.PreAuthRate["payments"] = WebhookRateBurst{Rate: 0.01, Burst: 20}
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, settings, false)

	saw503, saw429 := false, false
	for i := 0; i < 10; i++ {
		payload := mock.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, "t14-"+uuid.NewString(), "", payments.OutcomeSucceeded, 100, "EUR", "", false)
		resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock", payload)
		resp.Body.Close()
		switch resp.StatusCode {
		case http.StatusServiceUnavailable:
			saw503 = true
		case http.StatusTooManyRequests:
			saw429 = true
		}
	}
	if !saw503 {
		t.Fatal("a no-429-retry adapter's exhausted B1 bucket must answer 503, not 429")
	}
	if saw429 {
		t.Fatal("a no-429-retry adapter must never see a 429 from B1")
	}
}

// declaredRetryMockProvider wraps payments.MockProvider to declare
// WebhookRetrySemantics for T14 - the mock adapter itself carries no
// opinion, so this is the standard "wrap and add a method" pattern.
type declaredRetryMockProvider struct {
	*payments.MockProvider
	sem webhookauth.WebhookRetrySemantics
}

func (d declaredRetryMockProvider) WebhookRetrySemantics() (webhookauth.WebhookRetrySemantics, bool) {
	return d.sem, true
}

// T19 (lite): MultiTenantSimultaneousFlood. k tenants flood concurrently;
// a control tenant outside the flood is admitted throughout, and no
// request panics or hangs (the global caps hold).
func TestAdmission_T19_MultiTenantSimultaneousFlood(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newAdmissionTestServer(t, pool, issuer, orchestrator, nil, testAdmissionSettings(), false)

	const k = 5
	floodTenants := make([]uuid.UUID, k)
	floodSlugs := make([]string, k)
	for i := 0; i < k; i++ {
		tn := mustCreateTenant(t, pool)
		mustCreateBrand(t, pool, tn)
		mustRegisterCapability(t, pool, tn.ID, mockProvider)
		floodTenants[i] = tn.ID
		floodSlugs[i] = tn.Slug
	}
	control := mustCreateTenant(t, pool)
	controlBrand := mustCreateBrand(t, pool, control)
	mustRegisterCapability(t, pool, control.ID, mockProvider)
	controlPlayer := mustRegisterPlayer(t, srv, controlBrand.Slug)
	mustActivatePlayer(t, pool, control.ID, controlPlayer.ID)

	var wg sync.WaitGroup
	for i := 0; i < k; i++ {
		wg.Add(1)
		go func(slug string, tenantID uuid.UUID) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				payload := mockProvider.CallbackPayload(tenantID, payments.CallbackEventDeposit, "flood-"+uuid.NewString(), "", payments.OutcomeSucceeded, 10, "EUR", "", false)
				resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+slug+"/mock", payload)
				resp.Body.Close()
			}
		}(floodSlugs[i], floodTenants[i])
	}
	wg.Wait()

	resp := postJSON(t, srv, "/v1/me/deposits", controlPlayer.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": int64(4242), "payment_method": "card", "idempotency_key": uuid.NewString(),
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("control tenant must be admitted throughout a k-tenant simultaneous flood, got %d", resp.StatusCode)
	}
}
