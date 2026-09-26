package reconciliation

import (
	"testing"

	"github.com/google/uuid"
)

// casRoundCorrelationID is a deliberate copy of internal/casino's
// roundCorrelationID (this package must not import casino). Both are
// pinned to the SAME literal (internal/casino/rejections_test.go's
// roundCorrelationIDKnownVector), so a divergence fails one of the two.
func TestCasRoundCorrelationID_MatchesCasinoKnownVector(t *testing.T) {
	tenant := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	if got := casRoundCorrelationID(tenant, "mock-casino", "round-1").String(); got != "a304a518-6bb1-53f4-8cd1-bdfa3a1a376b" {
		t.Fatalf("casRoundCorrelationID diverged from casino.roundCorrelationID: %s", got)
	}
}

func TestCasinoMetrics_AuditMetadataCarriesEveryMetric(t *testing.T) {
	m := CasinoMetrics{UnresolvedCashRoundsOlderThanWindow: 1, TombstonesTotal: 2, RejectionsTotal: 3, RejectionsUnposted: 4}.AuditMetadata()
	for k, want := range map[string]int64{
		"unresolved_cash_rounds_older_than_window": 1, "tombstones_total": 2,
		"rejections_total": 3, "rejections_unposted": 4, "ageing_metric_window_seconds": 86400,
	} {
		if got, ok := m[k].(int64); !ok || got != want {
			t.Errorf("metadata[%q] = %v, want %d", k, m[k], want)
		}
	}
}
