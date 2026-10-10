//go:build integration

// ADR 0112 decision LF1, SLICE 2 (migration 0129), HTTP layer: the two casino
// ErrBrandNotActive branches. A NEW casino bet for a player of a suspended brand
// (tenant active) is answered 409 with the generic body on the public webhook
// ("callback rejected") and on the player play route ("request rejected"), with
// exactly one append-only audit row casino_callback.rejected_brand_not_active and
// no ledger row. The webhook preamble checks the tenant only, so a brand refusal
// always happens in the posting transaction. Server on the RUNTIME role pool.
package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// assertGenericConflict requires a 409 whose body is exactly the generic apierror shape
// with the given message: no brand, tenant, status or reference is echoed.
func assertGenericConflict(t *testing.T, resp *http.Response, wantMessage string) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409, got %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var body apierror.Error
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("decode body %q: %v", raw, err)
	}
	if body.Code != apierror.CodeConflict || body.Message != wantMessage {
		t.Fatalf("want generic %s %q, got %+v", apierror.CodeConflict, wantMessage, body)
	}
	low := strings.ToLower(string(raw))
	for _, leak := range []string{"brand", "suspended", "tenant", "status"} {
		if strings.Contains(low, leak) {
			t.Fatalf("the 409 body must be generic, it mentions %q: %s", leak, raw)
		}
	}
}

func TestBrandGate_HTTP_CasinoWebhookAndPlayRoute_409(t *testing.T) {
	owner, issuer := testEnv(t)
	rt := gateRuntimePool(t)
	orchestrator, mock := newMockCasinoOrchestrator()
	srv := newCasinoTestServer(t, rt, issuer, orchestrator)

	tenant := mustCreateTenant(t, owner)
	brand := mustCreateBrand(t, owner, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, owner, tenant.ID, player.ID)
	fundWallet(t, owner, tenant.ID, brand.ID, player.ID, "EUR", 10_000)
	game := mustSeedCasinoGame(t, owner, "mock-casino", "EUR")
	mustEnableCasinoGameForTenant(t, owner, tenant.ID, game.ID)
	mustEnableCasinoCapability(t, srv, owner, tenant)
	launched := mustLaunchCasinoGame(t, srv, player.Tokens.AccessToken, game.ID.String(), "EUR", "real")

	if err := launchfix.TrySetBrandStatus(context.Background(), tenant.ID, brand.ID, "suspended"); err != nil {
		t.Fatalf("suspend brand: %v", err)
	}
	txs0 := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID)
	auditBrand := func() int {
		return gateCount(t, owner, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenant.ID, casino.AuditActionCallbackRejectedBrandNotActive)
	}
	auditTenant := func() int {
		return gateCount(t, owner, tenant.ID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = $2`, tenant.ID, casino.AuditActionCallbackRejectedTenantNotActive)
	}

	t.Run("webhook", func(t *testing.T) {
		payload := mock.CallbackPayload(tenant.ID, casino.CallbackEventBet, "bg-webhook-"+uuid.NewString(), "", "round-bg-webhook", game.ProviderGameID,
			1000, "EUR", casino.OutcomeSucceeded, "", player.ID, uuid.MustParse(launched.SessionID))
		assertGenericConflict(t, rawPostCasinoCallback(t, srv, "/v1/webhooks/casino/"+tenant.Slug+"/mock-casino", payload), "callback rejected")
		if got := auditBrand(); got != 1 {
			t.Fatalf("expected exactly one %s row, got %d", casino.AuditActionCallbackRejectedBrandNotActive, got)
		}
	})
	t.Run("play_route", func(t *testing.T) {
		assertGenericConflict(t, postJSON(t, srv, "/v1/me/casino/sessions/"+launched.SessionID+"/wager", player.Tokens.AccessToken, wagerBody(500)), "request rejected")
		if got := auditBrand(); got != 2 {
			t.Fatalf("expected exactly one more %s row (2 total), got %d", casino.AuditActionCallbackRejectedBrandNotActive, got)
		}
	})
	if got := auditTenant(); got != 0 {
		t.Fatalf("a brand refusal must never be recorded as a tenant refusal, got %d rows", got)
	}
	if got := gateCount(t, owner, tenant.ID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenant.ID); got != txs0 {
		t.Fatalf("refused requests changed the ledger: %d -> %d", txs0, got)
	}
}
