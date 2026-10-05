//go:build integration

package reconciliation

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PAY-RECON-POLL-REF-CLEAR-1 (ledger-finance G-Y1..G-Y3). A poll_reference_mismatch
// park's typed Y evidence may CLEAR the standing finding only when Y is
// attributable to the parked capture: not held by another deposit/payout attempt,
// not the key of a deposit, withdrawal_completed or deposit_reversal posting.
// Otherwise only X clears. G-Y2 (both X and Y evidenced: both must be reversed)
// is IMPLEMENTED per the ledger-finance ruling; its tests are in
// prh2_r2_gy2_integration_test.go. The shared-Y rule (security F-1) is there too.

// pollParkWithY parks a fresh deposit (reference X) as poll_reference_mismatch
// with typed Y evidence for y, through the real sweeper poll path.
func (w *d2World) pollParkWithY(t *testing.T, y string) (d2Parked, string) {
	t.Helper()
	a := w.deposit(t, d2Amount)
	if a.ProviderReference == nil {
		t.Fatalf("setup: no reference (state %s)", a.State)
	}
	x := *a.ProviderReference
	w.p.setStatus(x, payments.StatusResult{ProviderReference: y, Outcome: payments.OutcomeSucceeded, Amount: d2Amount, AssetCode: "EUR"})
	w.d2PollOnce(t, a.ID)
	pk := d2Parked{attempt: w.mustParked(t, a.ID, payments.TerminalReasonPollReferenceMismatch, false), pspRef: x}
	var rows int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempt_reference_evidence WHERE attempt_id = $1 AND reference = $2`, a.ID, y).Scan(&rows)
	}); err != nil || rows != 1 {
		t.Fatalf("setup: want one typed Y row for %q, got %d (%v)", y, rows, err)
	}
	return pk, x
}

func yNote(t *testing.T, ms []Mismatch, attempt uuid.UUID, y string) {
	t.Helper()
	cu := d2CUFor(t, ms, attempt)
	if !strings.Contains(cu.ActualValue, "poll_returned_reference="+y) || !strings.Contains(cu.ActualValue, "not used for clearing") {
		t.Fatalf("finding must name the unattributable Y: %s", cu.ActualValue)
	}
}

// G-Y1a: Y is B's bound reference and B is succeeded AND posted. A reversal
// naming Y (B's own refund) must not clear A's park on X; a reversal on X does.
func TestR2_GY1a_YHeldByPostedAttemptDoesNotClear(t *testing.T) {
	w := newD2World(t)
	b := w.deposit(t, d2Amount)
	w.succeed(t, w.mockA, payProvA, b)
	b = w.attempt(t, b.ID)
	if b.LedgerTransactionID == nil {
		t.Fatal("setup: B must be posted")
	}
	y := *b.ProviderReference
	pk, x := w.pollParkWithY(t, y)

	yNote(t, w.d2Run(t, d2Src(d2Line(x, "", statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID, y)
	// B's own refund line, naming Y: does NOT clear A (in-run and later).
	yNote(t, w.d2Run(t, k3Cov(3, d2ReversalLine("r2-rev-"+uuid.NewString(), y, d2Amount))), pk.attempt.ID, y)
	yNote(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID, y)
	// A reversal on X still clears.
	d2NoCU(t, w.d2Run(t, k3Cov(5, d2ReversalLine("r2-rev-"+uuid.NewString(), x, d2Amount))), "a reversal on X clears")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "and stays cleared")
	w.d2AssertNoMoney(t, pk)
	w.d2AssertBalanced(t)
}

// G-Y1a': Y held by another attempt that is NOT posted (pending, bound reference).
func TestR2_GY1_YHeldByUnpostedAttemptDoesNotClear(t *testing.T) {
	w := newD2World(t)
	b := w.deposit(t, d2Amount) // pending, bound reference, no posting
	if b.ProviderReference == nil || b.LedgerTransactionID != nil {
		t.Fatalf("setup: want a pending bound unposted B, got %s", b.State)
	}
	y := *b.ProviderReference
	pk, x := w.pollParkWithY(t, y)
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(x, "", statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID)
	yNote(t, w.d2Run(t, k3Cov(3, d2ReversalLine("r2-rev-"+uuid.NewString(), y, d2Amount))), pk.attempt.ID, y)
	d2NoCU(t, w.d2Run(t, k3Cov(5, d2ReversalLine("r2-rev-"+uuid.NewString(), x, d2Amount))), "X clears")
	w.d2AssertNoMoney(t, pk)
}

// G-Y1b: Y is a withdrawal_completed ledger key (a payout's settlement
// reference) - not attributable; the payout attempt/posting holds it.
func TestR2_GY1b_YIsWithdrawalCompletedKeyDoesNotClear(t *testing.T) {
	w := newD2World(t)
	y := "r2-wc-" + uuid.NewString()
	w.payoutFixture(t, payProvA, "r2-instr-"+uuid.NewString()[:8], y, 3000, true)
	pk, x := w.pollParkWithY(t, y)
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(x, "", statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID)
	yNote(t, w.d2Run(t, k3Cov(3, d2ReversalLine("r2-rev-"+uuid.NewString(), y, d2Amount))), pk.attempt.ID, y)
	d2NoCU(t, w.d2Run(t, k3Cov(5, d2ReversalLine("r2-rev-"+uuid.NewString(), x, d2Amount))), "X clears")
}

// G-Y1c: Y is a deposit_reversal ledger key (a posted reversal's own reference):
// not attributable.
func TestR2_GY1c_YIsDepositReversalKeyDoesNotClear(t *testing.T) {
	w := newD2World(t)
	b := w.deposit(t, d2Amount)
	w.succeed(t, w.mockA, payProvA, b)
	revRef := "r2-revkey-" + uuid.NewString()[:8]
	w.deliverReversal(t, revRef, *b.ProviderReference, d2Amount) // posts deposit_reversal keyed revRef
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE transaction_type = 'deposit_reversal' AND provider_tx_id = $1`, revRef).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("setup: want one deposit_reversal keyed %q, got %d (%v)", revRef, n, err)
	}
	pk, x := w.pollParkWithY(t, revRef)
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(x, "", statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID)
	yNote(t, w.d2Run(t, k3Cov(3, d2ReversalLine("r2-rev-"+uuid.NewString(), revRef, d2Amount))), pk.attempt.ID, revRef)
	d2NoCU(t, w.d2Run(t, k3Cov(5, d2ReversalLine("r2-rev-"+uuid.NewString(), x, d2Amount))), "X clears")
}

// G-Y3: a TOMBSTONE on an unheld Y clears (a reversal callback naming Y with no
// posted original writes a tombstone on Y), including with no statement line.
func TestR2_GY3_TombstoneOnYClears(t *testing.T) {
	w := newD2World(t)
	y := "r2-echo-" + uuid.NewString()
	pk, x := w.pollParkWithY(t, y)
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(x, "", statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID)
	w.deliverReversal(t, "r2-tomb-"+uuid.NewString()[:8], y, d2Amount)
	var n int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE transaction_type = 'tombstone' AND provider_tx_id = $1`, y).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("setup: want one tombstone on Y, got %d (%v)", n, err)
	}
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "a tombstone on an unheld Y clears")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "and stays cleared")
	w.d2AssertNoMoney(t, pk)
	w.d2AssertBalanced(t)
}

// Cross-tenant: tenant 2 holds a posted deposit whose reference equals tenant
// 1's Y, plus a reversal line naming it. Neither makes Y unattributable for
// tenant 1 (no foreign holder is visible: the Y row and the clearing both stay
// in tenant 1's own data) nor clears tenant 1's park by itself.
func TestR2_GY1_CrossTenantYHolderIsInvisibleAndClearsNothing(t *testing.T) {
	w := newD2World(t)
	y := "r2-xt-" + uuid.NewString()
	pk, x := w.pollParkWithY(t, y)
	// Tenant 2: a deposit attempt of the same provider BOUND TO THE SAME
	// reference string as tenant 1's Y.
	f2 := seedFixture(t, w.pool)
	w2 := &payWorld{pool: w.pool, f: f2, mockA: w.mockA, orch: w.orch, srcA: w.srcA}
	w2.registerCapability(t, w.p, 10)
	w.p.setScript(d2Pending(y))
	b2 := w2.deposit(t, d2Amount)
	w.p.setScript(nil)
	if b2.ProviderReference == nil || *b2.ProviderReference != y {
		t.Fatalf("setup: tenant 2's attempt must hold the same reference string, got %v", b2.ProviderReference)
	}
	// Tenant 1's finding stands, with Y still attributable (no foreign holder seen).
	d2CUFor(t, w.d2Run(t, d2Src(d2Line(x, "", statement.PaymentStatusSucceeded, d2Amount))), pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID)
	// A reversal on Y in tenant 1 clears (Y is unheld in tenant 1) - proves the
	// foreign data neither blocked nor pre-cleared it.
	d2NoCU(t, w.d2Run(t, k3Cov(3, d2ReversalLine("r2-rev-"+uuid.NewString(), y, d2Amount))), "unheld Y in this tenant clears")
}
