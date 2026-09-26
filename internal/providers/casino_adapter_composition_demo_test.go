// This file is Stage 9's concrete proof for docs/decisions/0080-provider-
// integration-readiness-without-external-contracts.md's "removal/extension
// condition" claim: that a real adapter, once built against an actual
// documented provider contract, composes internal/providers/httpclient.
// Client + internal/providers.LoadProviderConfig, implements
// casino.CasinoProvider, and needs ZERO change to internal/casino itself
// (ledger, session, RG, or otherwise) to be a drop-in.
//
// fakeHTTPCasinoAdapter below is NOT a real vendor adapter, is NOT wired
// into cmd/platform-api/main.go, and does NOT encode any real (or guessed)
// provider's actual contract - its tiny JSON request/response shapes exist
// purely to exercise the "outbound HTTP round trip through httpclient.
// Client, decoded, mapped into casino's canonical types" pattern end to
// end against a local httptest.Server. Per Stage 9's explicit instruction
// ("do NOT integrate undocumented external dummy APIs, do NOT invent
// provider contracts"), this is deliberately scoped as validation of the
// SHARED SCAFFOLDING, not as a step toward any real integration - see the
// package/adapter-level doc comments below for exactly what is and is not
// being claimed.
package providers_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/providers"
	"github.com/Diansalas/igaming-platform/internal/providers/httpclient"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// fakeHTTPCasinoAdapter implements casino.CasinoProvider entirely by
// composing httpclient.Client - proving the interface needs no adapter-
// specific escape hatch to be satisfied by something that actually makes
// network calls (unlike casino.MockCasinoProvider, which is deliberately
// same-process/no-network per its own doc comment). Only the methods this
// demo actually exercises (Catalogue, Launch, Bet, HealthStatus,
// Capabilities) have a real implementation; the rest return a plain "not
// exercised in this demo" error - this is a composition proof, not a
// second production-quality mock, so there is no value in fleshing out
// every method against an invented contract.
type fakeHTTPCasinoAdapter struct {
	client     *httpclient.Client
	capability casino.AdapterCapability
}

func newFakeHTTPCasinoAdapter(cfg providers.ProviderConfig) (*fakeHTTPCasinoAdapter, error) {
	if !cfg.Enabled {
		return nil, errors.New("fakeHTTPCasinoAdapter: provider disabled")
	}
	apiKey, err := cfg.ResolveAPIKey()
	if err != nil {
		return nil, err
	}
	return &fakeHTTPCasinoAdapter{
		client: httpclient.New(httpclient.ClientConfig{
			ProviderName:    "fake-http-casino-demo",
			BaseURL:         cfg.BaseURL,
			Timeout:         cfg.Timeout,
			MaxRetries:      cfg.MaxRetries,
			AuthHeaderName:  "X-Api-Key",
			AuthHeaderValue: apiKey,
		}),
		capability: casino.AdapterCapability{
			ProviderID:        "fake-http-casino-demo",
			SupportsCatalogue: true,
			SupportsLaunch:    true,
			SupportsBalance:   false,
			// Stage 10.3 CAS-CAP-ROLLBACK-1 (M-CAS-1): an adapter declaring
			// SupportsBet must also declare SupportsWin and
			// SupportsRollback - a capability that can open exposure must
			// be able to settle it. Previously false/false here, which
			// ValidateAdapterCapabilityDeclaration/migration 0094's CHECK
			// would now reject; this demo fixture is not itself exercised
			// against either, but is kept conformant so it never becomes a
			// stale, misleading example of a non-conforming declaration.
			SupportsWin:          true,
			SupportsRollback:     true,
			SupportsBet:          true,
			SupportedAssets:      []string{"EUR"},
			SupportedGameTypes:   []string{"slot"},
			CallbackCapabilities: casino.CallbackWebhookOnly,
		},
	}, nil
}

// fakeCatalogueEntryDTO/fakeLaunchResponseDTO/fakeBetResponseDTO/
// fakeHealthResponseDTO are this demo's own made-up wire shapes - never
// assumed to resemble any real provider's actual JSON. They exist only to
// prove the "decode via Response.DecodeJSON, then map into casino's
// canonical types" step of the pattern.
type fakeCatalogueEntryDTO struct {
	GameID string `json:"game_id"`
	Name   string `json:"name"`
}

type fakeLaunchResponseDTO struct {
	Status string `json:"status"`
	URL    string `json:"url"`
}

type fakeBetResponseDTO struct {
	Status string `json:"status"`
}

type fakeHealthResponseDTO struct {
	OK bool `json:"ok"`
}

func (a *fakeHTTPCasinoAdapter) Catalogue(ctx context.Context) ([]casino.CatalogueEntry, error) {
	resp, err := a.client.Do(ctx, httpclient.Request{Method: http.MethodGet, Path: "/catalogue", Operation: "catalogue", Idempotent: true})
	if err != nil {
		return nil, err
	}
	var dtos []fakeCatalogueEntryDTO
	if err := resp.DecodeJSON(&dtos); err != nil {
		return nil, err
	}
	out := make([]casino.CatalogueEntry, 0, len(dtos))
	for _, d := range dtos {
		out = append(out, casino.CatalogueEntry{
			ProviderGameID:  d.GameID,
			Name:            d.Name,
			GameType:        "slot",
			SupportedAssets: []string{"EUR"},
		})
	}
	return out, nil
}

func (a *fakeHTTPCasinoAdapter) Launch(ctx context.Context, req casino.LaunchRequest) (casino.LaunchResult, error) {
	body, _ := json.Marshal(map[string]string{
		"game_id":      req.ProviderGameID,
		"launch_token": req.LaunchToken,
		"session_id":   req.SessionID.String(),
	})
	resp, err := a.client.Do(ctx, httpclient.Request{
		Method: http.MethodPost, Path: "/launch", Operation: "launch", Body: body,
	})
	if err != nil {
		return casino.LaunchResult{}, err
	}
	var dto fakeLaunchResponseDTO
	if err := resp.DecodeJSON(&dto); err != nil {
		return casino.LaunchResult{}, err
	}
	if dto.Status != "ok" {
		return casino.LaunchResult{Outcome: casino.OutcomeDeclined, DeclineReason: dto.Status}, nil
	}
	return casino.LaunchResult{Outcome: casino.OutcomeSucceeded, LaunchURL: dto.URL}, nil
}

func (a *fakeHTTPCasinoAdapter) Balance(ctx context.Context, req casino.BalanceRequest) (casino.BalanceResult, error) {
	return casino.BalanceResult{}, errors.New("fakeHTTPCasinoAdapter: Balance not exercised in this demo")
}

func (a *fakeHTTPCasinoAdapter) Bet(ctx context.Context, req casino.BetRequest) (casino.BetResult, error) {
	body, _ := json.Marshal(map[string]any{
		"provider_tx_id": req.ProviderTxID,
		"round_id":       req.RoundID,
		"amount":         req.Amount,
		"asset_code":     req.AssetCode,
	})
	// A bet is never idempotent-retried in this demo, mirroring
	// httpclient.Request.Idempotent's own documented default ("assume
	// unsafe to retry") - deliberately not overridden here, since this
	// demo has no way to know whether a real provider's bet endpoint is
	// safe to replay.
	resp, err := a.client.Do(ctx, httpclient.Request{Method: http.MethodPost, Path: "/bet", Operation: "bet", Body: body})
	if err != nil {
		return casino.BetResult{}, err
	}
	var dto fakeBetResponseDTO
	if err := resp.DecodeJSON(&dto); err != nil {
		return casino.BetResult{}, err
	}
	if dto.Status != "accepted" {
		return casino.BetResult{Outcome: casino.OutcomeDeclined, DeclineReason: dto.Status}, nil
	}
	return casino.BetResult{Outcome: casino.OutcomeSucceeded}, nil
}

func (a *fakeHTTPCasinoAdapter) Win(ctx context.Context, req casino.WinRequest) (casino.WinResult, error) {
	return casino.WinResult{}, errors.New("fakeHTTPCasinoAdapter: Win not exercised in this demo")
}

func (a *fakeHTTPCasinoAdapter) Rollback(ctx context.Context, req casino.RollbackRequest) (casino.RollbackResult, error) {
	return casino.RollbackResult{}, errors.New("fakeHTTPCasinoAdapter: Rollback not exercised in this demo")
}

func (a *fakeHTTPCasinoAdapter) HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (casino.CallbackEvent, error) {
	return casino.CallbackEvent{}, errors.New("fakeHTTPCasinoAdapter: HandleCallback not exercised in this demo")
}

// WebhookScheme (Stage 10.3 W1a): this demo never exercises inbound
// callbacks (see HandleCallback), so it reuses the casino platform MOCK
// scheme only to satisfy the interface. A real adapter implements its own
// vendor's documented scheme and must pass webhookauthtest conformance.
func (a *fakeHTTPCasinoAdapter) WebhookScheme() webhookauth.VerificationScheme {
	return casino.WebhookScheme().VerificationScheme()
}

func (a *fakeHTTPCasinoAdapter) Capabilities() casino.AdapterCapability {
	return a.capability
}

func (a *fakeHTTPCasinoAdapter) HealthStatus(ctx context.Context) (casino.ProviderHealth, error) {
	resp, err := a.client.Do(ctx, httpclient.Request{Method: http.MethodGet, Path: "/health", Operation: "health_status", Idempotent: true})
	if err != nil {
		return casino.ProviderHealth{}, err
	}
	var dto fakeHealthResponseDTO
	if err := resp.DecodeJSON(&dto); err != nil {
		return casino.ProviderHealth{}, err
	}
	state := casino.CircuitClosed
	if !dto.OK {
		state = casino.CircuitState("open")
	}
	return casino.ProviderHealth{ProviderID: "fake-http-casino-demo", CircuitState: state, LastUpdated: time.Now()}, nil
}

// compile-time proof: fakeHTTPCasinoAdapter satisfies casino.CasinoProvider
// with zero change to that interface or to internal/casino.
var _ casino.CasinoProvider = (*fakeHTTPCasinoAdapter)(nil)

// TestFakeHTTPCasinoAdapter_ComposesHTTPClientAgainstRealServer is the
// concrete evidence for ADR 0080's "removal/extension condition" claim,
// per Stage 9's task: a full Catalogue -> Launch -> Bet -> HealthStatus
// round trip through actual net/http (httptest.Server, real sockets, real
// JSON encode/decode) via an adapter that is composed ENTIRELY from
// internal/providers/httpclient.Client + internal/providers.
// LoadProviderConfig/ProviderConfig and implements casino.CasinoProvider
// with no interface change, no orchestrator change, no ledger/wallet/
// identity/tenant/brand/RG code touched at all. Wiring this adapter into
// Orchestrator.LaunchGame/ReceiveCallback (internal/casino/orchestrator.go)
// would require passing this same value wherever casino.MockCasinoProvider
// is passed today - nothing else.
func TestFakeHTTPCasinoAdapter_ComposesHTTPClientAgainstRealServer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Api-Key"); got != "demo-secret-value" {
			t.Errorf("server received X-Api-Key = %q, want the configured credential", got)
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/catalogue":
			_, _ = w.Write([]byte(`[{"game_id":"demo-slot-1","name":"Demo Slot"}]`))
		case "/launch":
			_, _ = w.Write([]byte(`{"status":"ok","url":"https://fake-demo.invalid/play/abc"}`))
		case "/bet":
			_, _ = w.Write([]byte(`{"status":"accepted"}`))
		case "/health":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	t.Setenv("FAKE_DEMO_CASINO_API_KEY", "demo-secret-value")

	cfg := providers.ProviderConfig{
		Enabled:      true,
		BaseURL:      srv.URL,
		APIKeyEnvVar: "FAKE_DEMO_CASINO_API_KEY",
		Timeout:      time.Second,
		MaxRetries:   1,
	}

	adapter, err := newFakeHTTPCasinoAdapter(cfg)
	if err != nil {
		t.Fatalf("newFakeHTTPCasinoAdapter: %v", err)
	}

	ctx := context.Background()

	entries, err := adapter.Catalogue(ctx)
	if err != nil {
		t.Fatalf("Catalogue: %v", err)
	}
	if len(entries) != 1 || entries[0].ProviderGameID != "demo-slot-1" {
		t.Fatalf("Catalogue() = %+v, want one demo-slot-1 entry", entries)
	}

	launchRes, err := adapter.Launch(ctx, casino.LaunchRequest{
		ProviderGameID: "demo-slot-1",
		LaunchToken:    "launch-token-xyz",
		SessionID:      uuid.New(),
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if launchRes.Outcome != casino.OutcomeSucceeded || launchRes.LaunchURL == "" {
		t.Fatalf("Launch() = %+v, want OutcomeSucceeded with a URL", launchRes)
	}

	betRes, err := adapter.Bet(ctx, casino.BetRequest{
		ProviderTxID: "tx-1", RoundID: "round-1", Amount: 100, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("Bet: %v", err)
	}
	if betRes.Outcome != casino.OutcomeSucceeded {
		t.Fatalf("Bet() = %+v, want OutcomeSucceeded", betRes)
	}

	health, err := adapter.HealthStatus(ctx)
	if err != nil {
		t.Fatalf("HealthStatus: %v", err)
	}
	if health.CircuitState != casino.CircuitClosed {
		t.Fatalf("HealthStatus().CircuitState = %v, want CircuitClosed", health.CircuitState)
	}
}
