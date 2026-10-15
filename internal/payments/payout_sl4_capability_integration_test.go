//go:build integration

// PAY-SEC-LAUNCH-1 / S-L4 (2) (ADR 0095 5.2 step (4)): the payout claim (T1p) re-reads the routed capability inside
// its own transaction. A capability disabled, or no longer withdrawal-capable, between the A0 routing and the claim
// commit is NOT claimed: the request stays approved, no attempt, no decision row, no provider call.
package payments

import (
	"context"
	"errors"
	"testing"
	"time"

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

// LF-A11-3: the in-claim capability re-read takes the row FOR SHARE, so a concurrent disable (which takes the row lock)
// waits for the claim instead of slipping in between the re-read and the commit. A racer holding the row FOR UPDATE
// (an uncommitted disable) must block the claim; when it commits the disable, the claim then sees it and refuses.
func TestSL4_CapabilityRereadIsLockedForShare_ConcurrentDisableWaitsAndIsThenSeen(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, mock := fpOrch(t, pool, "mock-sl4lock-"+uuid.NewString()[:8])
	wr := approvedWithdrawal(t, pool, f, 500, "sl4lock-"+uuid.NewString()[:8])

	locked, release := make(chan struct{}), make(chan struct{})
	racer := loStartRacer(t, pool, f.tenantID, "disabler", func(ctx context.Context, tx pgx.Tx) error {
		declared := mock.Capabilities()
		if _, err := WriteCapability(ctx, tx, mock, f.tenantID, nil, CapabilityConfig{
			SupportedFiatCurrencies: declared.SupportedFiatCurrencies, SupportedCryptoAssets: declared.SupportedCryptoAssets,
			SupportedPaymentMethods: declared.SupportedPaymentMethods, SupportsDeposit: declared.SupportsDeposit,
			SupportsWithdrawal: declared.SupportsWithdrawal, SupportsRefundReversal: declared.SupportsRefundReversal,
			AmountLimits: declared.AmountLimits, Priority: 100, Status: CapabilityDisabled,
		}); err != nil {
			return err
		}
		close(locked)
		<-release
		return nil
	})
	<-locked

	claimErr := make(chan error, 1)
	go func() {
		_, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
		claimErr <- err
	}()
	// The claim must be BLOCKED by the racer (not finish, not refuse) while the disable is uncommitted.
	blocked := false
	for i := 0; i < 500 && !blocked; i++ {
		select {
		case err := <-claimErr:
			close(release)
			t.Fatalf("the claim finished while a disable was uncommitted (no lock taken): %v", err)
		default:
		}
		var n int
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE $1 = ANY(pg_blocking_pids(pid))`, racer.pid).Scan(&n)
		}); err != nil {
			close(release)
			t.Fatal(err)
		}
		blocked = n > 0
		if !blocked {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !blocked {
		close(release)
		t.Fatal("the claim never blocked on the capability row")
	}
	close(release)
	if err := racer.wait(); err != nil {
		t.Fatalf("disabler: %v", err)
	}
	if err := <-claimErr; !errors.Is(err, ErrNoRoutableProvider) {
		t.Fatalf("after the disable committed the claim must refuse, got %v", err)
	}
	if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 0 {
		t.Fatalf("no attempt may exist, got %d", n)
	}
}
