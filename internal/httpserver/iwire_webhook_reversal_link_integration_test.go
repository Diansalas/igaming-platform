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
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
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
	wr := uuid.New()
	// B13-B: a LEGACY (NULL-binding) row, as it existed before migration 0126 (the fixture is not about the binding).
	// ADR 0113 (migration 0132): planted as E0 + E0b and walked to `submitted` (E1, E3, E7 with the
	// attempt's provider "mock") through the Go writers as igaming_runtime; it used to be a direct
	// `submitted` INSERT with no hold and a NULL provider_id, which the database now refuses (WD025).
	spec := wdpath.Spec{ID: wr, TenantID: tenant.ID, BrandID: brand.ID, PlayerAccountID: player.ID, WalletID: wallet.ID,
		AssetCode: "EUR", Amount: 5000, IdempotencyKey: "iw-rl-" + wr.String(), ProviderID: "mock"}
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			_, err := wdpath.Plant(ctx, tx, spec)
			return err
		})
	}); err != nil {
		t.Fatalf("seed withdrawal: %v", err)
	}
	wdpath.AdvanceAsRuntime(context.Background(), t, pool, tenant.ID, wr, withdrawal.StateSubmitted, spec)
	if err := pool.WithTenant(context.Background(), tenant.ID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := payments.InsertCreatedAttempt(ctx, tx, payments.NewCreatedAttempt{
			ID: attemptID, TenantID: tenant.ID, Operation: payments.AttemptOperationPayout, WithdrawalRequestID: &wr,
			AttemptNo: 1, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		}); err != nil {
			return err
		}
		// F4 (migration 0169, WD053): the fixture records the allowing re-dispatch decision a payout T2 needs.
		if err := f4RecordDispatchAllowHTTP(ctx, tx, wr, &attemptID); err != nil {
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
