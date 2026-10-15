//go:build integration

// PAY-SEC-LAUNCH-1 / S-L4 (2) (ADR 0095 5.2 step (4)): the payout claim (T1p) re-reads the routed capability inside
// its own transaction. A capability disabled, or no longer withdrawal-capable, between the A0 routing and the claim
// commit is NOT claimed: the request stays approved, no attempt, no decision row, no provider call.
package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"

	"github.com/jackc/pgx/v5"
)

func sl4RewriteCapability(t *testing.T, pool *db.Pool, f payoutFixture, p *MockProvider, status CapabilityStatus, withdrawals bool) {
	t.Helper()
	declared := p.Capabilities()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := WriteCapability(ctx, tx, p, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: declared.SupportedFiatCurrencies, SupportedCryptoAssets: declared.SupportedCryptoAssets,
			SupportedPaymentMethods: declared.SupportedPaymentMethods, SupportsDeposit: declared.SupportsDeposit,
			SupportsWithdrawal: withdrawals, SupportsRefundReversal: declared.SupportsRefundReversal,
			AmountLimits: declared.AmountLimits, Priority: 100, Status: status,
		})
		return err
	}); err != nil {
		t.Fatalf("rewrite capability: %v", err)
	}
}

func TestSL4_ClaimRereadsTheCapabilityInsideTheClaimTransaction(t *testing.T) {
	cases := map[string]struct {
		status      CapabilityStatus
		withdrawals bool
		wantRefused bool
	}{
		"disabled between routing and claim":                 {CapabilityDisabled, true, true},
		"no longer withdrawal-capable between routing/claim": {CapabilityActive, false, true},
		"unchanged (control): the claim goes through":        {CapabilityActive, true, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f, orch, mock := fpOrch(t, pool, "mock-sl4-"+uuid.NewString()[:8])
			wr := approvedWithdrawal(t, pool, f, 500, "sl4-"+uuid.NewString()[:8])
			ledgerBefore := fpLedgerTx(t, pool, f)
			payoutClaimAfterRoutingHook = func() { sl4RewriteCapability(t, pool, f, mock, tc.status, tc.withdrawals) }
			t.Cleanup(func() { payoutClaimAfterRoutingHook = nil })

			_, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
			if !tc.wantRefused {
				if err != nil {
					t.Fatalf("control claim failed: %v", err)
				}
				if countAttempts(t, pool, f.tenantID, wr.ID) != 1 {
					t.Fatal("control: the claim must create the attempt")
				}
				return
			}
			if !errors.Is(err, ErrNoRoutableProvider) {
				t.Fatalf("a capability changed since routing must refuse the claim with ErrNoRoutableProvider, got %v", err)
			}
			if got := fpReqState(t, pool, f, wr.ID); got.State != withdrawal.StateApproved {
				t.Fatalf("the request must stay approved, got %s", got.State)
			}
			if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 0 {
				t.Fatalf("no attempt may be created, got %d", n)
			}
			if n := fpDecisions(t, pool, f, "withdrawal_payout", "passed"); n != 0 {
				t.Fatalf("the allow decision row must roll back with the refused claim, got %d", n)
			}
			if got := fpLedgerTx(t, pool, f); got != ledgerBefore {
				t.Fatalf("a refused claim posts nothing (%d -> %d ledger transactions)", ledgerBefore, got)
			}
		})
	}
}
