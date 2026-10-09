//go:build integration

package httpserver

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// ADR 0102 8 row 10, reason reversal_link: a reversal naming a PAYOUT attempt's
// provider reference is a data-integrity failure (never tombstoned). The domain
// transaction rolls back (generic 500) and the failure-path P1 is raised detached.
func TestIWire_Webhook_ReversalLink_RaisesDetachedP1(t *testing.T) {
	pool, issuer := testEnv(t)
	orchestrator, mockProvider := newMockOrchestrator()
	srv := newFinancialTestServer(t, pool, issuer, orchestrator)
	tenant := mustCreateTenant(t, pool)
	brand := mustCreateBrand(t, pool, tenant)
	mustRegisterCapability(t, pool, tenant.ID, mockProvider)
	player := mustRegisterPlayer(t, srv, brand.Slug)
	mustActivatePlayer(t, pool, tenant.ID, player.ID)
	wallet := fundWallet(t, pool, tenant.ID, brand.ID, player.ID, "EUR", 10_000)

	const payoutRef = "iw-payout-ref-1"
	attemptID := uuid.New()
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		wr := uuid.New()
		// B13-B: a LEGACY (NULL-binding) row, as it existed before migration 0126 (the fixture is not about the binding).
		if err := pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, state, idempotency_key)
			VALUES ($1,$2,$3,$4,$5,'EUR',5000,'submitted',$6)`, wr, tenant.ID, brand.ID, player.ID, wallet.ID, "iw-rl-"+wr.String())
			return err
		}); err != nil {
			return err
		}
		if _, err := payments.InsertCreatedAttempt(ctx, tx, payments.NewCreatedAttempt{
			ID: attemptID, TenantID: tenant.ID, Operation: payments.AttemptOperationPayout, WithdrawalRequestID: &wr,
			AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		}); err != nil {
			return err
		}
		if err := payments.ClaimCreatedForSubmission(ctx, tx, attemptID, "mock", uuid.New(), "iw-rl", time.Now().Add(time.Minute)); err != nil {
			return err
		}
		return payments.MarkAccepted(ctx, tx, attemptID, payments.EvidencePlatform, payoutRef, time.Now().Add(time.Minute))
	}); err != nil {
		t.Fatalf("seed payout attempt: %v", err)
	}

	resp := rawPostCallback(t, srv, "/v1/webhooks/payments/"+tenant.Slug+"/mock",
		mockProvider.CallbackPayload(tenant.ID, payments.CallbackEventDepositReversal, "iw-rl-rev", payoutRef, payments.OutcomeSucceeded, 5000, "EUR", "", false))
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("a reversal naming a payout reference must fail generically (500), got %d", resp.StatusCode)
	}
	rows := alertinject.Find(alertinject.WaitForKind(t, pool, tenant.ID, string(alerting.KindPaymentWebhookIntegrity), 1), string(alerting.KindPaymentWebhookIntegrity))
	if len(rows) != 1 || rows[0].Discriminator != "provider:mock:reason:reversal_link" || rows[0].Severity != "p1" {
		t.Fatalf("expected the reversal_link P1, got %+v", rows)
	}
}
