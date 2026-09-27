//go:build integration

// Gate 10.3-W1 fix round, security S-5 / code review #4: end to end over
// HTTP, an adapter that returns an unnormalized ~4 KB control/bidi reason
// from CreateVerification and from HandleCallback never produces a 500
// (migration 0095's CHECK is never reached with raw text), and the staff
// view carries only the bounded, cleaned value.
package httpserver

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/identityresolution"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

var httpRawKYCReason = "\r\x1b[31mFAKE ADMIN MESSAGE\x1b[0m\u202Eevil-reversed-text" + strings.Repeat("H", 4096)

// httpRawReasonKYCAdapter wraps the MOCK (real signing/verification) but
// returns httpRawKYCReason, unnormalized, from CreateVerification and
// HandleCallback. It stays synthetic (the MOCK's marker is promoted).
type httpRawReasonKYCAdapter struct{ *kyc.MockKYCProvider }

func (a httpRawReasonKYCAdapter) CreateVerification(ctx context.Context, in kyc.CreateVerificationInput) (kyc.ProviderResult, error) {
	r, err := a.MockKYCProvider.CreateVerification(ctx, in)
	r.Reason, r.ReasonTruncated = httpRawKYCReason, false
	return r, err
}

func (a httpRawReasonKYCAdapter) HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (kyc.ProviderResult, error) {
	r, err := a.MockKYCProvider.HandleCallback(ctx, in, cred)
	if err != nil {
		return r, err
	}
	r.Reason, r.ReasonTruncated = httpRawKYCReason, false
	return r, nil
}

func newRawReasonKYCServer(t *testing.T, pool *db.Pool, issuer *auth.Issuer) (*httptest.Server, httpRawReasonKYCAdapter) {
	t.Helper()
	adapter := httpRawReasonKYCAdapter{kyc.NewMockKYCProvider()}
	srv := httptest.NewServer(New(Deps{
		Logger:                 slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		DB:                     pool,
		AuthIssuer:             issuer,
		ServiceName:            "platform-api-test",
		AccessTokenTTL:         5 * time.Minute,
		RefreshTokenTTL:        time.Hour,
		PersonResolver:         identityresolution.NewMockPersonResolver(),
		KYCOrchestrator:        kyc.NewOrchestrator(map[string]kyc.KYCProvider{"mock": adapter}, kyc.NewMockWebhookCredentials(adapter.MockKYCProvider)),
		KYCWebhookEnabled:      true,
		DocumentStorage:        kyc.NewMockDocumentStorageProvider(),
		MalwareScanner:         kyc.NewMockMalwareScanner(),
		KYCOutboundCredentials: kyc.NewMockOutboundResolver(),
	}))
	t.Cleanup(srv.Close)
	return srv, adapter
}

func TestKYC_UnnormalizedAdapterReason_NoServerError_StaffSeesBounded(t *testing.T) {
	pool, issuer := testEnv(t)
	srv, adapter := newRawReasonKYCServer(t, pool, issuer)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	bounded, _ := kyc.NormalizeReason(httpRawKYCReason)

	createResp := postJSON(t, srv, "/v1/me/kyc/verifications", player.Tokens.AccessToken, map[string]any{})
	createBody := rawResponseBody(t, createResp)
	if createResp.StatusCode != http.StatusCreated {
		t.Fatalf("CreateVerification with an unnormalized adapter reason: want 201, got %d: %s", createResp.StatusCode, createBody)
	}
	var created playerVerificationResponse
	if err := json.Unmarshal(createBody, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}

	compliance := mustCreateStaff(t, pool, tenant.ID, identity.StaffRoleCompliance, "compliance-raw-reason-1")
	tokens := mustLoginStaff(t, srv, tenant.Slug, compliance.Email, "compliance-raw-reason-1")
	assertStaffReason := func(stage string) {
		t.Helper()
		resp := getJSON(t, srv, "/v1/admin/kyc/verifications?player_account_id="+player.ID.String(), tokens.AccessToken)
		body := rawResponseBody(t, resp)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: staff list want 200, got %d: %s", stage, resp.StatusCode, body)
		}
		var rows []struct {
			Reason string `json:"reason"`
		}
		if err := json.Unmarshal(body, &rows); err != nil || len(rows) != 1 {
			t.Fatalf("%s: want a JSON array with one verification, got err=%v: %s", stage, err, body)
		}
		if rows[0].Reason != bounded {
			t.Fatalf("%s: staff reason must be the bounded, cleaned value (%d bytes), got %d bytes: %q", stage, len(bounded), len(rows[0].Reason), rows[0].Reason)
		}
	}
	assertStaffReason("after create")

	ref := mustGetKYCProviderReference(t, pool, tenant.ID, created.ID)
	post := func(stage string, outcome kyc.ProviderOutcome) {
		t.Helper()
		resp := rawPostKYCCallback(t, srv, "/v1/webhooks/kyc/"+tenant.Slug+"/mock", adapter.CallbackPayload(tenant.ID, ref, outcome, "short"))
		body := rawResponseBody(t, resp)
		if resp.StatusCode != http.StatusNoContent {
			t.Fatalf("%s callback with an unnormalized adapter reason: want 204, got %d: %s", stage, resp.StatusCode, body)
		}
	}
	// outcome=error first, while the verification is NOT terminal, so the
	// failure audit row carrying the adapter reason is actually written.
	post("outcome=error", kyc.ProviderError)
	assertStaffReason("after outcome=error callback")
	post("status-update", kyc.ProviderRejected)
	assertStaffReason("after status-update callback")
}
