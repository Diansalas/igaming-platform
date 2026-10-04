//go:build integration

package httpserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// Code review C1 (I-wire delta): "the raise happens after the response" must be
// true as seen by the CLIENT. net/http buffers a small response until the
// handler returns, so each handler flushes the response (flushResponse) before
// its queued raises / Pending.Flush. These tests prove it with no wall-clock
// assertion: the detached alert INSERT is made to block on an advisory lock the
// test holds; the client must receive its response while that raise is still
// blocked; only then is the lock released and the alert must appear. If the
// response were not flushed, the request would never complete while the lock is
// held (the failure guard below turns that into a test failure, not a hang).

const flushLockKeyBase int64 = 0x1F1A5000

// within runs f and fails the test if it does not finish (failure guard only;
// never an assertion about how long the happy path takes).
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("%s did not return while the post-response alert work was blocked: the response was not flushed before the alert work", what)
	}
}

func hasAlert(rows []alertinject.Row, kind alerting.Kind, discSuffix string) bool {
	for _, r := range rows {
		if r.Kind == string(kind) && strings.HasSuffix(r.Discriminator, discSuffix) {
			return true
		}
	}
	return false
}

func TestIWire_ResponseFlush_KillSwitchEngage_ClientGetsResponseBeforeTheRaiseCompletes(t *testing.T) {
	a := newKSAPI(t)
	tenant := a.tenant()
	admin := a.tenantStaff(tenant, "tenant_admin")
	tok := a.token(admin, tenant, auth.RoleTenantAdmin, auth.PrincipalStaff)
	key := flushLockKeyBase + 1
	alertinject.InstallBlockDetached(t, a.pool, tenant, key)
	release := alertinject.HoldAdvisoryLock(t, a.pool, key)

	var status int
	within(t, "kill-switch engage", func() {
		// Read the status line and headers only: a.do would ReadAll the body,
		// which (chunked after a flush) ends only when the handler returns.
		req, err := http.NewRequest("POST", a.srv.URL+"/v1/admin/tenants/"+tenant.String()+"/payments/kill-switches",
			strings.NewReader(`{"provider_scope":"*","operation_scope":"deposit","reason_code":"flush_check"}`))
		if err != nil {
			t.Error(err)
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Error(err)
			return
		}
		status = resp.StatusCode
		defer func() { _ = resp.Body.Close() }()
	})
	if status != http.StatusOK {
		t.Fatalf("engage: %d", status)
	}
	if rows := alertinject.ForSubject(t, a.pool, tenant); len(rows) != 0 {
		t.Fatalf("the raise must still be blocked when the client has its response, got %+v", rows)
	}
	release()
	a.srv.Close() // waits for the handler (and its post-response raise) to finish
	if rows := alertinject.Find(alertinject.ForSubject(t, a.pool, tenant), string(alerting.KindPaymentKillSwitchEngaged)); len(rows) != 1 {
		t.Fatalf("after the lock is released the engage alert must exist, got %+v", rows)
	}
}

// Webhook failure path (payload_mismatch): the generic 409 reaches the provider
// before the queued detached raise completes.
func TestIWire_ResponseFlush_WebhookFailurePath_ProviderGetsTheResponseBeforeTheRaiseCompletes(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)

	const amount int64 = 4000
	deposit := func(k string) string {
		resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
			"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": k,
		})
		var intent depositIntentResponse
		decodeBody(t, resp, &intent)
		ref := providerReferenceFromRedirectURL(intent.RedirectURL)
		cb := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amount, "EUR", "", false))
		_ = cb.Body.Close()
		return ref
	}
	d1, d2 := deposit("iw-fl-1"), deposit("iw-fl-2")
	post := func(origin string) int {
		resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal, "iw-fl-rev", origin, payments.OutcomeSucceeded, amount, "EUR", "", false))
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	if s := post(d1); s != http.StatusOK {
		t.Fatalf("first reversal: %d", s)
	}
	key := flushLockKeyBase + 2
	alertinject.InstallBlockDetached(t, pool, tenant.ID, key)
	release := alertinject.HoldAdvisoryLock(t, pool, key)
	var status int
	within(t, "webhook 409", func() { status = post(d2) })
	if status != http.StatusConflict {
		t.Fatalf("expected 409, got %d", status)
	}
	if hasAlert(alertinject.ForSubject(t, pool, tenant.ID), alerting.KindPaymentWebhookIntegrity, ":reason:payload_mismatch") {
		t.Fatal("the failure-path raise must still be blocked when the provider has its response")
	}
	release()
	srv.Close()
	if !hasAlert(alertinject.ForSubject(t, pool, tenant.ID), alerting.KindPaymentWebhookIntegrity, ":reason:payload_mismatch") {
		t.Fatal("after the lock is released the payload_mismatch alert must exist")
	}
}

// Webhook T10 with a swallowed in-tx raise: the uniform 200 reaches the provider
// before the post-commit detached retry (Pending.Flush) completes.
func TestIWire_ResponseFlush_WebhookT10_ProviderGetsTheUniform200BeforeTheDetachedRetryCompletes(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	const amount int64 = 6000
	resp := postJSON(t, srv, "/v1/me/deposits", player.Tokens.AccessToken, map[string]any{
		"asset_code": "EUR", "amount": amount, "payment_method": "card", "idempotency_key": "iw-fl-t10-" + uuid.NewString(),
	})
	var intent depositIntentResponse
	decodeBody(t, resp, &intent)
	ref := providerReferenceFromRedirectURL(intent.RedirectURL)

	key := flushLockKeyBase + 3
	alertinject.Install(t, pool, tenant.ID, alertinject.InTxOnly, "P0001") // the in-tx raise is swallowed
	alertinject.InstallBlockDetached(t, pool, tenant.ID, key)              // its detached retry blocks
	release := alertinject.HoldAdvisoryLock(t, pool, key)
	var status int
	within(t, "webhook T10 callback", func() {
		r := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
			mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDeposit, ref, "", payments.OutcomeSucceeded, amount-1, "EUR", "", false))
		status = r.StatusCode
		_ = r.Body.Close()
	})
	if status != http.StatusOK {
		t.Fatalf("expected the uniform 200, got %d", status)
	}
	if hasAlert(alertinject.ForSubject(t, pool, tenant.ID), alerting.KindPaymentWebhookIntegrity, ":reason:"+payments.TerminalReasonCallbackAmountAssetMismatch) {
		t.Fatal("the detached retry must still be blocked when the provider has its 200")
	}
	release()
	srv.Close()
	if !hasAlert(alertinject.ForSubject(t, pool, tenant.ID), alerting.KindPaymentWebhookIntegrity, ":reason:"+payments.TerminalReasonCallbackAmountAssetMismatch) {
		t.Fatal("after the lock is released the detached retry must persist the P1")
	}
}

// Casino callback integrity failure path.
func TestIWire_ResponseFlush_CasinoIntegrityFailurePath_ProviderGetsTheResponseBeforeTheRaiseCompletes(t *testing.T) {
	e := newRejEnvHTTP(t)
	key := flushLockKeyBase + 4
	alertinject.InstallBlockDetached(t, e.pool, e.tenant.ID, key)
	release := alertinject.HoldAdvisoryLock(t, e.pool, key)
	var status int
	within(t, "casino orphan win", func() {
		status = e.send(t, casino.CallbackEventWin, "fl-orph-win", "", "fl-orph-r", 1000, uuid.Nil)
	})
	if status != http.StatusBadRequest {
		t.Fatalf("expected the bet_not_found 400, got %d", status)
	}
	if hasAlert(alertinject.ForSubject(t, e.pool, e.tenant.ID), alerting.KindCasinoCallbackIntegrity, ":reason:bet_not_found") {
		t.Fatal("the raise must still be blocked when the provider has its response")
	}
	release()
	e.srv.Close()
	if !hasAlert(alertinject.ForSubject(t, e.pool, e.tenant.ID), alerting.KindCasinoCallbackIntegrity, ":reason:bet_not_found") {
		t.Fatal("after the lock is released the integrity alert must exist")
	}
}
