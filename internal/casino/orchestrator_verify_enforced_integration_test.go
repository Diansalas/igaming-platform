//go:build integration

package casino

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// verificationSkippingCasinoProvider models an adapter that FORGOT to
// verify: its HandleCallback re-signs whatever bytes it is handed with the
// resolved credential, then delegates to the mock - so on its own it accepts
// any forged body. Stage 10.3 W1a: the orchestrator must still reject a
// forgery.
type verificationSkippingCasinoProvider struct {
	*MockCasinoProvider
	handleCalls int
}

func (p *verificationSkippingCasinoProvider) HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (CallbackEvent, error) {
	p.handleCalls++
	resigned := in
	resigned.Header = http.Header{}
	casinoScheme.SetHeaders(resigned.Header, cred.KeyID, casinoScheme.Sign(cred.Secret, in.TenantID, in.ProviderID, cred.KeyID, in.Body))
	return p.MockCasinoProvider.HandleCallback(ctx, resigned, cred)
}

// TestOrchestratorVerify_Casino_EnforcedEvenIfAdapterSkipsIt is the QA
// plan's per-domain mutation-kill test (04-review-qa.md W1a). The forgery is
// a rollback of a never-seen original, which - were it accepted - writes a
// tombstone (a tenant-scoped ledger effect).
func TestOrchestratorVerify_Casino_EnforcedEvenIfAdapterSkipsIt(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	mock := NewMockCasinoProvider("mock-casino", "EUR")
	adapter := &verificationSkippingCasinoProvider{MockCasinoProvider: mock}
	registerCasinoCapability(t, pool, f, adapter, 100)
	resolver := NewMockWebhookCredentials(mock)
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": adapter}, resolver)

	payload := func() webhookauth.Inbound {
		return mock.CallbackPayload(f.tenantID, CallbackEventRollback, "w1a-forged-rb-1", "w1a-never-seen-original", "round-w1a", "game-1",
			1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())
	}
	forged := payload()
	forged.Header = http.Header{}
	casinoScheme.SetHeaders(forged.Header, webhookauth.MockKeyID, casinoScheme.Sign(webhookauth.NewMockMaster(), f.tenantID, "mock-casino", webhookauth.MockKeyID, forged.Body))

	cred, err := resolver.ResolveKey(context.Background(), f.tenantID, "mock-casino", webhookauth.MockKeyID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	control := forged
	control.TenantID, control.ProviderID = f.tenantID, "mock-casino"
	if _, err := adapter.HandleCallback(context.Background(), control, cred); err != nil {
		t.Fatalf("control: the verification-skipping adapter must accept the forgery on its own, got %v", err)
	}
	adapter.handleCalls = 0

	before := noeffect.CaptureCasino(t, pool, []uuid.UUID{f.tenantID})
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", forged)
		return err
	})
	var authErr *webhookauth.AuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
		t.Fatalf("the orchestrator must reject a forged callback itself (signature_invalid), got %v", err)
	}
	if adapter.handleCalls != 0 {
		t.Fatalf("HandleCallback ran %d time(s) before verification succeeded", adapter.handleCalls)
	}
	noeffect.AssertNoCasinoEffect(t, pool, []uuid.UUID{f.tenantID}, before)

	// The genuinely-signed equivalent passes verification and reaches the
	// adapter (its posting outcome is casino's concern, not this test's).
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", payload())
		return err
	})
	if errors.As(err, &authErr) {
		t.Fatalf("a genuinely-signed callback must pass orchestrator verification, got %v", err)
	}
	if adapter.handleCalls != 1 {
		t.Fatalf("expected HandleCallback exactly once for the genuine callback, got %d", adapter.handleCalls)
	}
}
