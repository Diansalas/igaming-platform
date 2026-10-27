package httpserver

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// lic7 security F1: a tenant-scoped readiness carries neither the legacy risk-rule ids nor their counts
// (risk_config:read is deliberately withheld from tenant compliance/finance); other counts stay.
func TestCoarsenForTenant_DropsRiskRuleRefsAndCounts(t *testing.T) {
	d := launchReadinessDTO{
		Counts: map[string]int64{
			launchgov.InfoRiskLegacyJurisdictionScoped: 2,
			launchgov.InfoRiskLegacyDepositOperation:   1,
			"players_with_balance":                     3,
		},
		Refs: map[string][]string{launchgov.RefsRiskLegacyJurisdictionScoped: {"00000000-0000-0000-0000-000000000001"}},
	}
	coarsenForTenant(&d)
	if d.Refs != nil {
		t.Fatalf("refs leaked to tenant view: %v", d.Refs)
	}
	if _, ok := d.Counts[launchgov.InfoRiskLegacyJurisdictionScoped]; ok {
		t.Fatalf("legacy count leaked: %v", d.Counts)
	}
	if _, ok := d.Counts[launchgov.InfoRiskLegacyDepositOperation]; ok {
		t.Fatalf("legacy count leaked: %v", d.Counts)
	}
	if d.Counts["players_with_balance"] != 3 {
		t.Fatalf("unrelated count removed: %v", d.Counts)
	}
}
