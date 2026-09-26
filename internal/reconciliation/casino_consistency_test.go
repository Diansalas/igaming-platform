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
	src := CasinoMetrics{UnresolvedCashRoundsOlderThanWindow: 1, TombstonesTotal: 2, RejectionsTotal: 3, RejectionsUnposted: 4,
		RejectionsEvidenceOnly: 5, RejectionsByClass: map[string]int64{"payload_mismatch": 5}}
	m := src.AuditMetadata()
	for k, want := range map[string]int64{
		"unresolved_cash_rounds_older_than_window": 1, "tombstones_total": 2,
		"rejections_total": 3, "rejections_unposted": 4, "ageing_metric_window_seconds": 86400,
		"rejections_evidence_only": 5,
	} {
		if got, ok := m[k].(int64); !ok || got != want {
			t.Errorf("metadata[%q] = %v, want %d", k, m[k], want)
		}
	}
	byClass, ok := m["rejections_by_class"].(map[string]int64)
	if !ok || len(byClass) != 1 || byClass["payload_mismatch"] != 5 {
		t.Fatalf("metadata[rejections_by_class] = %v", m["rejections_by_class"])
	}
	// The rendered map is a copy: mutating it never changes the metrics.
	byClass["payload_mismatch"] = 99
	if src.RejectionsByClass["payload_mismatch"] != 5 {
		t.Fatal("AuditMetadata must copy RejectionsByClass")
	}
	// A nil map renders as an empty object, never null.
	if got, ok := (CasinoMetrics{}).AuditMetadata()["rejections_by_class"].(map[string]int64); !ok || got == nil || len(got) != 0 {
		t.Fatalf("empty by-class metric must render as {}: %#v", got)
	}
}

// The C6 ruling's two class sets are disjoint, have no duplicates, and
// return a fresh slice per call (mutating one never narrows C6). The
// partition against migration 0097's CHECK and internal/casino's
// constants is pinned by TestCasinoConsistency_C6_ClassRulingPartitionsEveryRecordedClass.
func TestCasinoC6ClassRuling_DisjointAndImmutable(t *testing.T) {
	seen := map[string]string{}
	for name, set := range map[string][]string{"c6": CasinoUnpostedEventReasonClasses(), "evidence": CasinoEvidenceOnlyReasonClasses()} {
		for _, c := range set {
			if prev, dup := seen[c]; dup {
				t.Fatalf("class %q appears in %s and %s", c, prev, name)
			}
			seen[c] = name
		}
	}
	if len(seen) != 11 {
		t.Fatalf("the ruling must cover exactly the 11 recorded classes, got %d", len(seen))
	}
	a := CasinoUnpostedEventReasonClasses()
	a[0] = "tampered"
	if CasinoUnpostedEventReasonClasses()[0] == "tampered" {
		t.Fatal("CasinoUnpostedEventReasonClasses must return a fresh slice")
	}
}
