//go:build integration

package kyc

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

// verificationSkippingKYCProvider models an adapter that FORGOT to verify:
// its HandleCallback re-signs whatever bytes it is handed with the resolved
// credential, then delegates to the mock - so on its own it accepts any
// forged body. Stage 10.3 W1a: the orchestrator must still reject a forgery.
type verificationSkippingKYCProvider struct {
	*MockKYCProvider
	handleCalls int
}

func (p *verificationSkippingKYCProvider) HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (ProviderResult, error) {
	p.handleCalls++
	resigned := in
	resigned.Header = http.Header{}
	kycMockScheme.SetHeaders(resigned.Header, cred.KeyID, kycMockScheme.Sign(cred.Secret, in.TenantID, in.ProviderID, cred.KeyID, in.Body))
	return p.MockKYCProvider.HandleCallback(ctx, resigned, cred)
}

// TestOrchestratorVerify_KYC_EnforcedEvenIfAdapterSkipsIt is the QA plan's
// per-domain mutation-kill test (04-review-qa.md W1a).
func TestOrchestratorVerify_KYC_EnforcedEvenIfAdapterSkipsIt(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	mock := NewMockKYCProvider()
	adapter := &verificationSkippingKYCProvider{MockKYCProvider: mock}
	resolver := NewMockWebhookCredentials(mock)
	orch := NewOrchestrator(map[string]KYCProvider{"mock": adapter}, resolver)

	verificationID := seedVerification(t, pool, f)
	var ref string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	}); err != nil {
		t.Fatalf("read reference: %v", err)
	}
	mock.created[ref] = true

	// A forged "approved" callback: well-formed MOCK headers, attacker key.
	forged := mock.CallbackPayload(f.tenantID, ref, ProviderApproved, "forged")
	forged.Header = http.Header{}
	kycMockScheme.SetHeaders(forged.Header, webhookauth.MockKeyID, kycMockScheme.Sign(webhookauth.NewMockMaster(), f.tenantID, "mock", webhookauth.MockKeyID, forged.Body))

	cred, err := resolver.ResolveKey(context.Background(), f.tenantID, "mock", webhookauth.MockKeyID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	control := forged
	control.TenantID, control.ProviderID = f.tenantID, "mock"
	if _, err := adapter.HandleCallback(context.Background(), control, cred); err != nil {
		t.Fatalf("control: the verification-skipping adapter must accept the forgery on its own, got %v", err)
	}
	adapter.handleCalls = 0

	before := noeffect.Capture(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}})
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", forged)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
		t.Fatalf("the orchestrator must reject a forged callback itself (signature_invalid), got %v", err)
	}
	if adapter.handleCalls != 0 {
		t.Fatalf("HandleCallback ran %d time(s) before verification succeeded", adapter.handleCalls)
	}
	if got := mustGetStatus(t, pool, f.tenantID, verificationID); got != StatusPending {
		t.Fatalf("forged approval changed the verification to %s", got)
	}
	noeffect.AssertNoEffect(t, pool, []uuid.UUID{f.tenantID}, []noeffect.Verification{{TenantID: f.tenantID, ID: verificationID}}, before)

	genuine := mock.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto_approved")
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock", genuine)
		return err
	}); err != nil {
		t.Fatalf("genuine callback rejected: %v", err)
	}
	if adapter.handleCalls != 1 || mustGetStatus(t, pool, f.tenantID, verificationID) != StatusApproved {
		t.Fatalf("genuine callback: handleCalls=%d status=%s", adapter.handleCalls, mustGetStatus(t, pool, f.tenantID, verificationID))
	}
}
