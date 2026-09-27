//go:build integration

package payments

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// verificationSkippingPaymentsAdapter models an adapter that FORGOT to
// verify: its HandleCallback re-signs whatever bytes it is handed with the
// credential the orchestrator resolved, then delegates to the mock - so on
// its own it accepts any forged body. Stage 10.3 W1a (WH-VENDOR-SCHEME-1):
// the orchestrator must reject a forged callback anyway, because it runs the
// adapter's WebhookScheme().Verify ITSELF before HandleCallback.
type verificationSkippingPaymentsAdapter struct {
	*MockProvider
	handleCalls int
}

func (a *verificationSkippingPaymentsAdapter) HandleCallback(ctx context.Context, in InboundCallback, cred WebhookCredential) (CallbackEvent, error) {
	a.handleCalls++
	resigned := in
	resigned.Header = http.Header{}
	paymentsScheme.SetHeaders(resigned.Header, cred.KeyID, paymentsScheme.Sign(cred.Secret, in.TenantID, in.ProviderID, cred.KeyID, in.Body))
	return a.MockProvider.HandleCallback(ctx, resigned, cred)
}

// TestOrchestratorVerify_Payments_EnforcedEvenIfAdapterSkipsIt is the QA
// plan's per-domain mutation-kill test (04-review-qa.md W1a): deleting the
// orchestrator's verification call must turn it red.
func TestOrchestratorVerify_Payments_EnforcedEvenIfAdapterSkipsIt(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	mock := NewMockProvider("mock-psp", "EUR")
	adapter := &verificationSkippingPaymentsAdapter{MockProvider: mock}
	registerCapability(t, pool, f, mock, 100)
	resolver := MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(mock)}
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": adapter}, resolver)

	const ref = "w1a-forged-deposit-1"
	var intentID uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, status, provider_id, provider_reference, idempotency_key)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, 'EUR', 1000, 'card', 'pending', 'mock-psp', $5, 'w1a-forged-idem')
			 RETURNING id`,
			f.tenantID, f.brandID, f.playerAccountID, f.walletID, ref).Scan(&intentID); err != nil {
			return err
		}
		// PRH-payments-callback-cutover (ADR 0095 §6.1, INV-IO-14): the
		// genuine callback below must resolve through payment_attempts, not
		// deposit_intents directly - a matching 'pending' attempt (already
		// accepted, with this SAME provider_reference) is the on-disk shape
		// InitiateDepositAttempt's own phase A/B would have produced.
		attempt, err := InsertSubmittingAttempt(ctx, tx, NewSubmittingAttempt{
			ID: uuid.New(), TenantID: f.tenantID, Operation: AttemptOperationDeposit,
			DepositIntentID: &intentID, ProviderID: "mock-psp", PaymentMethod: "card",
			AssetCode: "EUR", Amount: 1000, Interactive: false,
			ClaimToken: uuid.New(), LeaseOwner: "test-backfill", LeaseUntil: time.Now().Add(time.Minute),
		})
		if err != nil {
			return err
		}
		return MarkAccepted(ctx, tx, attempt.ID, EvidenceSync, ref, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("seed pending intent: %v", err)
	}

	// A forged "deposit succeeded" callback: well-formed MOCK headers, but
	// signed with an attacker's key, not the tenant's credential.
	forged := mock.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 1000, "EUR", "", false)
	forged.Header = http.Header{}
	paymentsScheme.SetHeaders(forged.Header, mockWebhookKeyID, paymentsScheme.Sign(webhookauth.NewMockMaster(), f.tenantID, "mock-psp", mockWebhookKeyID, forged.Body))

	// Control: the adapter on its own really does accept the forgery.
	cred, err := resolver.ResolveKey(context.Background(), f.tenantID, "mock-psp", mockWebhookKeyID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	control := forged
	control.TenantID, control.ProviderID = f.tenantID, "mock-psp"
	if _, err := adapter.HandleCallback(context.Background(), control, cred); err != nil {
		t.Fatalf("control: the verification-skipping adapter must accept the forgery on its own, got %v", err)
	}
	adapter.handleCalls = 0

	ledgerBefore, auditBefore := countLedgerAndAudit(t, pool, f.tenantID)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", forged)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != ReasonSignatureInvalid {
		t.Fatalf("the orchestrator must reject a forged callback itself (signature_invalid), got %v", err)
	}
	if adapter.handleCalls != 0 {
		t.Fatalf("HandleCallback ran %d time(s) before verification succeeded", adapter.handleCalls)
	}
	assertNoFinancialEffect(t, pool, f.tenantID, f.walletID, ledgerBefore, auditBefore, 1, "mock-psp", ref)

	// And the genuine callback still flows end to end through the same
	// adapter (the orchestrator's verification is not over-rejecting) -
	// asserted all the way to a real posting (disposition `applied`, cash
	// credited), not merely that HandleCallback ran, so this test still
	// proves the SAME "flows end to end" claim its own comment always
	// made.
	genuine := mock.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 1000, "EUR", "", false)
	var genuineResult ReceiveCallbackResult
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		genuineResult, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", genuine)
		return err
	}); err != nil {
		t.Fatalf("genuine callback rejected: %v", err)
	}
	if adapter.handleCalls != 1 {
		t.Fatalf("expected HandleCallback exactly once for the genuine callback, got %d", adapter.handleCalls)
	}
	if genuineResult.Disposition != DispositionApplied {
		t.Fatalf("expected the genuine callback to be applied, got disposition %q", genuineResult.Disposition)
	}
	if b := cashBalance(t, pool, f); b != 1000 {
		t.Fatalf("expected the genuine callback to actually post (cash_balance=1000), got %d", b)
	}
}
