//go:build integration

package reconciliation

import (
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// C-34b (ADR 0101 9.2 N1 row, LF F-5): the standing S1 finding discloses the
// approximate attribution: when the evidencing line's own reference is held by
// ANOTHER attempt, the detail names that holder attempt, the import and line.
func TestK3_C34b_StandingDetailNamesTheHolderAttempt(t *testing.T) {
	w := newD2World(t)
	pk, _ := w.parkInvalidRef(t)
	holder := w.deposit(t, d2Amount) // a live attempt that holds its own reference RB
	if holder.ProviderReference == nil {
		t.Fatal("setup: the holder has no reference")
	}
	line := d2Line(*holder.ProviderReference, pk.attempt.MerchantReference, statement.PaymentStatusSucceeded, d2Amount)
	w.d2Run(t, d2Src(line))
	ms := w.d2Run(t, d2PastSrc()) // a later run: only the persisted line remains
	m := d2CUFor(t, ms, pk.attempt.ID)
	for _, want := range []string{"standing: persisted line import=", "line_no=", "is_mock=", "holder_attempt=" + holder.ID.String()} {
		if !strings.Contains(m.ActualValue, want) {
			t.Errorf("the standing detail lacks %q: %s", want, m.ActualValue)
		}
	}
}
