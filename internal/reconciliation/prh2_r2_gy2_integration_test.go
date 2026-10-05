//go:build integration

package reconciliation

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// PAY-RECON-POLL-REF-CLEAR-1 G-Y2 (ledger-finance ruling, REQUIRED for closure):
// when eligible succeeded deposit lines evidence BOTH X and Y they are two
// evidenced captures, so the finding clears only when BOTH are reversed. With
// only one of them evidenced the "X or Y" rule stays (C-36 preserved).

func gy2Rev(offset int, original string) payFixedSource {
	return k3Cov(offset, d2ReversalLine("gy2-rev-"+uuid.NewString(), original, d2Amount))
}

func gy2Succ(ref string) statement.PaymentStatementLine {
	return d2Line(ref, "", statement.PaymentStatusSucceeded, d2Amount)
}

// (a) both evidenced + a reversal on Y only: stands across 3+ runs.
func TestR2_GY2a_BothEvidenced_YOnlyReversalStands(t *testing.T) {
	w := newD2World(t)
	pk, x := w.pollParkWithY(t, "gy2-y-"+uuid.NewString())
	y := w.yOf(t, pk.attempt.ID)
	cu := d2CUFor(t, w.d2Run(t, d2Src(gy2Succ(x), gy2Succ(y))), pk.attempt.ID)
	if got := cu.ActualValue; !strings.Contains(got, "BOTH") {
		t.Fatalf("the finding must name both evidenced references: %s", got)
	}
	d2CUFor(t, w.d2Run(t, gy2Rev(3, y)), pk.attempt.ID)
	for i := 0; i < 4; i++ {
		d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID)
	}
	w.d2AssertNoMoney(t, pk)
	w.d2AssertBalanced(t)
}

// (b) both evidenced + a reversal on X only: stands.
func TestR2_GY2b_BothEvidenced_XOnlyReversalStands(t *testing.T) {
	w := newD2World(t)
	pk, x := w.pollParkWithY(t, "gy2-y-"+uuid.NewString())
	y := w.yOf(t, pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, d2Src(gy2Succ(x), gy2Succ(y))), pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, gy2Rev(3, x)), pk.attempt.ID)
	for i := 0; i < 3; i++ {
		d2CUFor(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID)
	}
	w.d2AssertNoMoney(t, pk)
	w.d2AssertBalanced(t)
}

// (c) both evidenced + reversals on both (in different imports): clears and stays cleared.
func TestR2_GY2c_BothEvidenced_BothReversedClearsAndStays(t *testing.T) {
	w := newD2World(t)
	pk, x := w.pollParkWithY(t, "gy2-y-"+uuid.NewString())
	y := w.yOf(t, pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, d2Src(gy2Succ(x), gy2Succ(y))), pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, gy2Rev(3, y)), pk.attempt.ID)
	d2NoCU(t, w.d2Run(t, gy2Rev(5, x)), "reversals on both X and Y")
	for i := 0; i < 3; i++ {
		d2NoCU(t, w.d2Run(t, d2PastSrc()), "and it stays cleared")
	}
	w.d2AssertNoMoney(t, pk)
	w.d2AssertBalanced(t)
}

// (d) only Y evidenced + a reversal on Y clears.
func TestR2_GY2d_OnlyYEvidenced_YReversalClears(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.pollParkWithY(t, "gy2-y-"+uuid.NewString())
	y := w.yOf(t, pk.attempt.ID)
	d2CUFor(t, w.d2Run(t, d2Src(gy2Succ(y))), pk.attempt.ID)
	d2NoCU(t, w.d2Run(t, gy2Rev(3, y)), "only Y evidenced: a reversal on Y clears")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "and stays cleared")
	w.d2AssertNoMoney(t, pk)
	w.d2AssertBalanced(t)
}

// (e) only X evidenced + a reversal on X clears (and, as in C-36, so does Y).
func TestR2_GY2e_OnlyXEvidenced_XReversalClears(t *testing.T) {
	w := newD2World(t)
	pk, x := w.pollParkWithY(t, "gy2-y-"+uuid.NewString())
	d2CUFor(t, w.d2Run(t, d2Src(gy2Succ(x))), pk.attempt.ID)
	d2NoCU(t, w.d2Run(t, gy2Rev(3, x)), "only X evidenced: a reversal on X clears")
	d2NoCU(t, w.d2Run(t, d2PastSrc()), "and stays cleared")
	w.d2AssertNoMoney(t, pk)
	w.d2AssertBalanced(t)
}

// Security F-1: two parks that recorded the SAME returned reference Y are
// ambiguous: one reversal naming Y must clear neither; each clears only on its
// own X.
func TestR2_F1_SharedYAcrossTwoParks_OneReversalClearsNeither(t *testing.T) {
	w := newD2World(t)
	y := "f1-shared-y-" + uuid.NewString()
	pk1, x1 := w.pollParkWithY(t, y)
	pk2, x2 := w.pollParkWithY(t, y)
	ms := w.d2Run(t, d2Src(gy2Succ(x1), gy2Succ(x2)))
	d2CUFor(t, ms, pk1.attempt.ID)
	d2CUFor(t, ms, pk2.attempt.ID)
	ms = w.d2Run(t, gy2Rev(3, y))
	d2CUFor(t, ms, pk1.attempt.ID)
	d2CUFor(t, ms, pk2.attempt.ID)
	ms = w.d2Run(t, d2PastSrc())
	d2CUFor(t, ms, pk1.attempt.ID)
	d2CUFor(t, ms, pk2.attempt.ID)
	// Each clears only on its own bound reference.
	ms = w.d2Run(t, gy2Rev(5, x1))
	d2NoCUFor(t, ms, pk1.attempt.ID)
	d2CUFor(t, ms, pk2.attempt.ID)
	w.d2AssertNoMoney(t, pk1)
	w.d2AssertNoMoney(t, pk2)
	w.d2AssertBalanced(t)
}

// LF C-2: Y equal to an ATTEMPT-LESS legacy deposit posting key is not
// attributable (the deposit ledger arm is the only protection).
func TestR2_GY1d_YIsLegacyAttemptlessDepositKey(t *testing.T) {
	w := newD2World(t)
	y := w.legacyDeposit(t, 4000)
	pk, x := w.pollParkWithY(t, y)
	d2CUFor(t, w.d2Run(t, d2Src(gy2Succ(x))), pk.attempt.ID)
	yNote(t, w.d2Run(t, gy2Rev(3, y)), pk.attempt.ID, y)
	yNote(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID, y)
}

// LF C-2: Y equal to an ATTEMPT-LESS legacy withdrawal_completed key.
func TestR2_GY1e_YIsLegacyAttemptlessWithdrawalCompletedKey(t *testing.T) {
	w := newD2World(t)
	y := "gy1e-legsettle-" + uuid.NewString()[:8]
	w.legacyWithdrawalCompleted(t, payProvA, y, 1000)
	pk, x := w.pollParkWithY(t, y)
	d2CUFor(t, w.d2Run(t, d2Src(gy2Succ(x))), pk.attempt.ID)
	yNote(t, w.d2Run(t, gy2Rev(3, y)), pk.attempt.ID, y)
	yNote(t, w.d2Run(t, d2PastSrc()), pk.attempt.ID, y)
}

// yOf reads the typed Y evidence row of an attempt.
func (w *d2World) yOf(t *testing.T, attempt uuid.UUID) string {
	t.Helper()
	var y string
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT reference FROM payment_attempt_reference_evidence WHERE attempt_id = $1`, attempt).Scan(&y)
	}); err != nil {
		t.Fatalf("read Y evidence: %v", err)
	}
	return y
}

// d2NoCUFor asserts no pay_captured_unposted for one attempt.
func d2NoCUFor(t *testing.T, ms []Mismatch, attempt uuid.UUID) {
	t.Helper()
	for _, m := range ms {
		if m.MismatchKind == d2KindCU && strings.Contains(m.ReconciliationKey, "attempt="+attempt.String()) {
			t.Fatalf("unexpected pay_captured_unposted for attempt %s:\n%s", attempt, renderMismatches(ms))
		}
	}
}

// d2RealSource is a NON-synthetic fixed source (a stand-in for a real PSP
// statement: is_mock=false imports), so a MOCK line becomes ineligible (D-4/RC-3).
type d2RealSource struct{ inner payFixedSource }

func (d2RealSource) Label() string        { return "real-fixed test statement" }
func (s d2RealSource) ProviderID() string { return s.inner.ProviderID() }
func (s d2RealSource) Fetch(ctx context.Context, r statement.PaymentFetchRequest) (statement.PaymentStatement, error) {
	return s.inner.Fetch(ctx, r)
}

// G-Y2 uses ELIGIBLE evidence only (D-4/RC-3): once a real import exists, a
// succeeded line from a MOCK import does not make Y an evidenced capture, so the
// "X or Y" rule applies and a (real) reversal on Y clears.
func TestR2_GY2f_MockEvidenceIsIneligibleOnceARealImportExists(t *testing.T) {
	w := newD2World(t)
	pk, x := w.pollParkWithY(t, "gy2-y-"+uuid.NewString())
	y := w.yOf(t, pk.attempt.ID)
	real := func(offset int, lines ...statement.PaymentStatementLine) d2RealSource {
		return d2RealSource{k3Cov(offset, lines...)}
	}
	d2CUFor(t, w.d2Run(t, real(3, gy2Succ(x))), pk.attempt.ID)  // real import: X evidenced
	d2CUFor(t, w.d2Run(t, k3Cov(4, gy2Succ(y))), pk.attempt.ID) // MOCK import: Y line, ineligible
	d2NoCU(t, w.d2Run(t, real(5, d2ReversalLine("gy2-rev-"+uuid.NewString(), y, d2Amount))), "Y evidenced only by ineligible MOCK evidence: X-or-Y rule, a real reversal on Y clears")
	w.d2AssertNoMoney(t, pk)
}

// LF D-LF-3: G-Y2 counts only SUCCEEDED lines as evidence. X succeeded and Y only
// pending or declined: Y is NOT an evidenced capture, so the "X or Y" rule applies
// and a reversal on X alone clears.
func TestR2_GY2g_YOnlyPendingOrDeclined_XReversalClears(t *testing.T) {
	for _, st := range []string{statement.PaymentStatusPending, statement.PaymentStatusDeclined} {
		w := newD2World(t)
		pk, x := w.pollParkWithY(t, "gy2-y-"+uuid.NewString())
		y := w.yOf(t, pk.attempt.ID)
		d2CUFor(t, w.d2Run(t, d2Src(gy2Succ(x), d2Line(y, "", st, d2Amount))), pk.attempt.ID)
		d2NoCU(t, w.d2Run(t, gy2Rev(3, x)), "Y only "+st+": a reversal on X clears")
		d2NoCU(t, w.d2Run(t, d2PastSrc()), "and stays cleared")
		w.d2AssertNoMoney(t, pk)
	}
}
