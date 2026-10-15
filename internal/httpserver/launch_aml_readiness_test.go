package httpserver

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// ADR 0116 A7: for a tenant-scoped viewer the PROCESS half of R-AML-SCREENING (which provider the process runs) collapses to
// see_platform exactly like R-PROVIDER-TIER, while the tenant-visible half (the policy per jurisdiction), the quorum and the
// monitoring gap stay readable. Results never change.
func TestCoarsenForTenant_AMLChecks(t *testing.T) {
	for _, c := range []struct {
		code, in, want string
	}{
		{launchgov.CheckAMLScreening, "no_screening_provider", "see_platform"},
		{launchgov.CheckAMLScreening, "synthetic_provider_in_production", "see_platform"},
		{launchgov.CheckAMLScreening, "ok_mock_provider", "see_platform"},
		{launchgov.CheckAMLScreening, "no_component_registry", "see_platform"},
		{launchgov.CheckAMLScreening, "policy_missing_for_jurisdiction", "policy_missing_for_jurisdiction"},
		{launchgov.CheckAMLScreening, "no_jurisdiction_referenced", "no_jurisdiction_referenced"},
		{launchgov.CheckAMLScreening, "ok", "ok"},
		{launchgov.CheckAMLQuorum, "quorum_not_met", "quorum_not_met"},
		{launchgov.CheckAMLQuorum, "ok", "ok"},
		// SR7-6: the PLATFORM officer quorum of a platform-licensed tenant is platform process detail.
		{launchgov.CheckAMLQuorum, "quorum_not_met" + launchgov.AMLPlatformScopeSuffix, "see_platform"},
		{launchgov.CheckAMLQuorum, "ok" + launchgov.AMLPlatformScopeSuffix, "see_platform"},
		{launchgov.CheckAMLMonitoring, "monitoring_not_implemented", "monitoring_not_implemented"},
	} {
		d := launchReadinessDTO{Environment: "production", Checks: []launchCheckDTO{{Code: c.code, Result: "fail", DetailCode: c.in}}}
		coarsenForTenant(&d)
		if d.Checks[0].DetailCode != c.want || d.Checks[0].Result != "fail" || d.Environment != "redacted" {
			t.Errorf("%s/%s: %+v, want detail %s", c.code, c.in, d.Checks[0], c.want)
		}
	}
}
