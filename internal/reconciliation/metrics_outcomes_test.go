package reconciliation

import (
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

type fakePaySource struct {
	statement.PaymentStatementSource
	id string
}

func (f fakePaySource) ProviderID() string { return f.id }

// recordOutcomes must record exactly the streams that RAN: an observation-only (suspended/closed) tenant ran
// only ledger_vs_projection and payment_statement, so its zero-valued sportsbook/casino outcomes must not appear
// as runs (they would count runs that never happened and, worse, a fake "success" for a stream that is off).
func TestRecordOutcomes_ObservationOnlyTenantsRecordOnlyTheirTwoStreams(t *testing.T) {
	tr, reader, _ := newTestTracker(t)
	active, closed := uuid.New(), uuid.New()
	gen := tr.beginSweep()
	tr.recordOutcomes(gen, []SweepOutcome{
		{TenantID: active, Run: Run{Status: StatusClean},
			Sportsbook: StreamOutcome{Run: Run{Status: StatusClean}}, Casino: StreamOutcome{Run: Run{Status: StatusClean}},
			CasinoStatement: StreamOutcome{Run: Run{Status: StatusClean}},
			PaymentStatement: []StreamOutcome{
				{Run: Run{Status: StatusClean}},
				{Err: errors.New("provider b down")},
			}},
		{TenantID: closed, ObservationOnly: true, TenantStatus: "closed", Run: Run{Status: StatusClean},
			PaymentStatement: []StreamOutcome{{Run: Run{Status: StatusClean}}, {Run: Run{Status: StatusClean}}}},
	}, []statement.PaymentStatementSource{fakePaySource{id: "prov-a"}, fakePaySource{id: "prov-b"}})
	tr.endSweep(gen, true)

	runs := observed(t, reader)["reconciliation_runs_total"]
	want := map[string]float64{
		"ledger_vs_projection":  2, // both tenants
		"sportsbook_settlement": 1, // the active tenant only
		"casino_consistency":    1,
		"casino_statement":      1,
		"payment_statement":     4, // 2 sources x 2 tenants
	}
	for stream, n := range want {
		if runs[stream] != n {
			t.Errorf("runs_total{stream=%s} = %v, want %v", stream, runs[stream], n)
		}
	}

	// Two sources of one tenant are tracked separately: prov-b failing must keep payment_statement's oldest
	// success at 0 even though prov-a succeeded.
	if got := observed(t, reader)["reconciliation_last_success_unix_seconds"]["payment_statement"]; got != 0 {
		t.Errorf("payment_statement last_success = %v, want 0 (one source never succeeded for the active tenant)", got)
	}
	// ...while a stream where everything succeeded reports a real time.
	if got := observed(t, reader)["reconciliation_last_success_unix_seconds"]["casino_statement"]; got == 0 {
		t.Errorf("casino_statement last_success = 0 after a success")
	}
}

// A source index beyond the configured sources (a defensive case) must not panic and falls back to the shared key.
func TestRecordOutcomes_MoreOutcomesThanSourcesDoesNotPanic(t *testing.T) {
	tr, _, _ := newTestTracker(t)
	gen := tr.beginSweep()
	tr.recordOutcomes(gen, []SweepOutcome{{TenantID: uuid.New(), PaymentStatement: []StreamOutcome{{}, {}}}}, nil)
}
