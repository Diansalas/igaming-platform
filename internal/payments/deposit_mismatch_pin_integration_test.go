//go:build integration

// Guard test (R-6 review, PAY-DEPOSIT-MISMATCH-ALERT-1 not yet built): pins TODAY's DEPOSIT behaviour
// for a mismatched-amount success arriving on an already-SUCCEEDED and on a DECLINED deposit. The
// shared receipt cells write the terminal-mismatch audit row only: no alert, no payout-reason alert,
// no state change. When PAY-DEPOSIT-MISMATCH-ALERT-1 lands this test must be changed deliberately.
package payments

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

func TestDepositMismatchedSuccess_OnSucceededAndDeclined_AuditOnly_NoAlert_NoStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	for _, c := range []struct {
		name   string
		amount int64
		want   AttemptState
	}{
		{"succeeded", 5000, AttemptSucceeded},
		{"declined", MockAmountPlayerDeclineNoCascade, AttemptDeclined},
	} {
		t.Run(c.name, func(t *testing.T) {
			pid := "mock-psp-dm-" + c.name
			f := seedOrchFixture(t, pool)
			provider := NewMockProvider(pid, "EUR")
			registerCapability(t, pool, f, provider, 100)
			orch := NewOrchestrator(map[string]PaymentProvider{pid: provider}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())
			res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
				Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
				AssetCode: "EUR", Amount: c.amount, PaymentMethod: "card", IdempotencyKey: "dm-" + c.name,
			})
			if err != nil {
				t.Fatalf("InitiateDepositAttempt: %v", err)
			}
			ref := "dm-ref-" + c.name
			if res.Attempt.ProviderReference != nil {
				ref = *res.Attempt.ProviderReference
			}
			deliver := func(amount int64) ReceiptDisposition {
				var disp ReceiptDisposition
				pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
					var err error
					disp, err = ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, pid, ReceiptEvidence{
						EventType: "deposit", ProviderReference: ref, MerchantReference: res.Attempt.MerchantReference,
						Outcome: OutcomeSucceeded, Amount: amount, AssetCode: "EUR",
					})
					return err
				})
				if err != nil {
					t.Fatalf("ApplyReceiptEvidence: %v", err)
				}
				pending.Flush(context.Background())
				return disp
			}
			if c.want == AttemptSucceeded {
				if d := deliver(c.amount); d != DispositionApplied {
					t.Fatalf("setup: matching success disposition = %s, want applied", d)
				}
			}
			before := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
			if before.State != c.want {
				t.Fatalf("setup: state = %s, want %s", before.State, c.want)
			}
			deliver(c.amount + 1)

			after := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
			if after.State != c.want || after.TerminalReason != nil {
				t.Fatalf("no state change expected: %s %v", after.State, after.TerminalReason)
			}
			if rows := alertinject.ForSubject(t, pool, f.tenantID); len(rows) != 0 {
				t.Fatalf("today a deposit mismatch raises NO alert (PAY-DEPOSIT-MISMATCH-ALERT-1), got %+v", rows)
			}
			n := fpCount(t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2 AND target_id=$3`,
				f.tenantID, r5Terminal, res.Attempt.ID.String())
			if n != 1 {
				t.Fatalf("terminal mismatch audit rows = %d, want 1", n)
			}
			assertLedgerBalanced(t, pool, f.tenantID)
		})
	}
}
