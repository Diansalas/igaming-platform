package payments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// TestWebhookAuthAliases_ErrorsIsAs is ruling J2 (backend review, Stage
// 10.2): the internal/payments aliases and sentinels still match values
// produced by internal/webhookauth, and vice versa, through errors.Is and
// errors.As (including through %w wrapping).
func TestWebhookAuthAliases_ErrorsIsAs(t *testing.T) {
	// Sentinel identity (pointer-identical, not merely equal text).
	if ErrCallbackAuthFailed != webhookauth.ErrAuthFailed || //nolint:errorlint // identity is the property under test
		ErrWebhookCredentialUnavailable != webhookauth.ErrCredentialUnavailable || //nolint:errorlint
		ErrCallbackSignatureInvalid != webhookauth.ErrSignatureInvalid { //nolint:errorlint
		t.Fatal("payments sentinels must be the webhookauth sentinel values")
	}

	t.Run("webhookauth-produced AuthError via payments names", func(t *testing.T) {
		err := fmt.Errorf("ctx: %w", &webhookauth.AuthError{Reason: webhookauth.ReasonNoResolver, KeyID: "mock-v1"})
		if !errors.Is(err, ErrCallbackAuthFailed) {
			t.Fatal("errors.Is(webhookauth AuthError, payments.ErrCallbackAuthFailed) failed")
		}
		var ae *CallbackAuthError
		if !errors.As(err, &ae) || ae.Reason != ReasonNoResolver || ae.KeyID != "mock-v1" {
			t.Fatal("errors.As(webhookauth AuthError, *payments.CallbackAuthError) failed")
		}
	})

	t.Run("payments-produced CallbackAuthError via webhookauth names", func(t *testing.T) {
		// A real payments-produced error: ReceiveCallback's header check
		// fails before any tx use, so a nil tx is never touched. Stage 10.3
		// W1a: the provider must be REGISTERED for the header check to run
		// at all - an unregistered provider has no scheme and is now
		// provider_unregistered before any header is examined.
		orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": NewMockProvider("mock-psp")}, nil)
		_, err := orch.receiveCallbackInTx(context.Background(), nil, uuid.New(), "mock-psp", InboundCallback{Header: http.Header{}})
		if !errors.Is(err, webhookauth.ErrAuthFailed) {
			t.Fatalf("errors.Is(payments error, webhookauth.ErrAuthFailed) failed: %v", err)
		}
		var ae *webhookauth.AuthError
		if !errors.As(err, &ae) || ae.Reason != webhookauth.ReasonSignatureMissing {
			t.Fatalf("errors.As(payments error, *webhookauth.AuthError) failed: %v", err)
		}
	})

	t.Run("signature sentinel both directions", func(t *testing.T) {
		provider := NewMockProvider("mock-psp")
		tenantID := uuid.New()
		cred, err := NewMockWebhookCredentials(provider).ResolveKey(context.Background(), tenantID, "mock-psp", mockWebhookKeyID)
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		in := provider.CallbackPayload(tenantID, CallbackEventDeposit, "ref-j2", "", OutcomeSucceeded, 1000, "EUR", "", false)
		in.Body = append(in.Body, ' ')

		_, handleErr := provider.HandleCallback(context.Background(), in, cred)
		if !errors.Is(handleErr, webhookauth.ErrSignatureInvalid) {
			t.Fatalf("payments HandleCallback error must match webhookauth.ErrSignatureInvalid, got %v", handleErr)
		}
		verifyErr := paymentsScheme.Verify(cred, in)
		if !errors.Is(fmt.Errorf("w: %w", verifyErr), ErrCallbackSignatureInvalid) {
			t.Fatalf("webhookauth Scheme.Verify error must match payments.ErrCallbackSignatureInvalid, got %v", verifyErr)
		}
	})

	t.Run("credential-unavailable sentinel both directions", func(t *testing.T) {
		_, err := webhookauth.MockResolver{Master: webhookauth.NewMockMaster(), Label: webhookauth.PaymentsMockKeyLabel, ProviderID: "a"}.ResolveKey(context.Background(), uuid.New(), "b", mockWebhookKeyID)
		if !errors.Is(err, ErrWebhookCredentialUnavailable) {
			t.Fatalf("webhookauth resolver error must match payments sentinel, got %v", err)
		}
		_, err = NewMockWebhookCredentials(NewMockProvider("a")).ResolveKey(context.Background(), uuid.New(), "b", mockWebhookKeyID)
		if !errors.Is(err, webhookauth.ErrCredentialUnavailable) {
			t.Fatalf("payments resolver error must match webhookauth sentinel, got %v", err)
		}
	})

	t.Run("payments scheme parameters unchanged", func(t *testing.T) {
		s := paymentsScheme
		if s.Prefix != SigningInputPrefix || s.SignatureHeader != HeaderSignature || s.KeyIDHeader != HeaderKeyID ||
			SigningInputPrefix != "igaming.payments.webhook.v1" || HeaderSignature != "X-Payments-Signature" || HeaderKeyID != "X-Payments-Key-Id" ||
			mockWebhookKeyID != "mock-v1" {
			t.Fatal("payments webhook scheme parameters changed")
		}
		// Stage 10.2 final review (K9/L5): payments.ProviderIDPattern was
		// deleted as dead code (zero callers besides this assertion); the
		// provider_id charset itself is webhookauth's, unexported, and
		// exercised here through the public ValidProviderID function so the
		// charset boundary stays pinned without reaching into an internal
		// package-level regexp.
		if !webhookauth.ValidProviderID("mock-casino-1") {
			t.Fatal("payments webhook provider_id charset changed: expected a lowercase alphanumeric-with-hyphens id to remain valid")
		}
		if webhookauth.ValidProviderID("Mock_Casino") {
			t.Fatal("payments webhook provider_id charset changed: expected an uppercase/underscore id to remain invalid")
		}
	})
}
