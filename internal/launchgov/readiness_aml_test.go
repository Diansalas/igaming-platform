package launchgov

import (
	"errors"
	"sort"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/providerkind"
)

// ADR 0116 s6 / S7 (A7): the AML readiness checks. Pure (DB-free) tests of amlChecksFrom; the database half (aml_readiness) is
// internal/launchgov/readiness_aml_integration_test.go.

type synthScreening struct{}

func (synthScreening) SyntheticComponent() {}

type eligibleScreening struct{}

func (eligibleScreening) MarkProductionEligible() {}

type unmarkedScreening struct{}

func reg(c any) []providerkind.Registration {
	return []providerkind.Registration{{Domain: "payments", Name: "provider", Component: synthScreening{}}, {Domain: "aml", Name: "screening_provider", Component: c}}
}

func byCode(cs []Check) map[string]Check {
	m := map[string]Check{}
	for _, c := range cs {
		m[c.Code] = c
	}
	return m
}

func TestAMLChecks_ThreeChecksAlwaysPresent_MonitoringAlwaysFails(t *testing.T) {
	ready := amlData{OwnerScope: "platform", QuorumMet: true, Total: 1, WithPolicy: 1}
	for name, cs := range map[string][]Check{
		"all configured": amlChecksFrom(ready, nil, 1, reg(eligibleScreening{}), "production"),
		"read error":     amlChecksFrom(amlData{}, errors.New("db"), 1, reg(eligibleScreening{}), "test"),
		"nothing":        amlChecksFrom(amlData{}, nil, 0, nil, ""),
		"mock, non-prod": amlChecksFrom(ready, nil, 1, reg(synthScreening{}), "development"),
		"mock, prod":     amlChecksFrom(ready, nil, 1, reg(synthScreening{}), "production"),
	} {
		m := byCode(cs)
		if len(cs) != 3 || len(m) != 3 {
			t.Fatalf("%s: want exactly the three R-AML-* checks, got %+v", name, cs)
		}
		mon := m[CheckAMLMonitoring]
		if mon.Result != ResultFail || mon.DetailCode != "monitoring_not_implemented" {
			t.Errorf("%s: transaction monitoring is NOT built and must always be an explicit failing item, got %+v", name, mon)
		}
	}
}

func TestAMLChecks_Quorum(t *testing.T) {
	for _, c := range []struct {
		met    bool
		result string
		detail string
	}{{true, ResultPass, "ok"}, {false, ResultFail, "quorum_not_met"}} {
		got := byCode(amlChecksFrom(amlData{QuorumMet: c.met, Total: 1, WithPolicy: 1}, nil, 1, reg(eligibleScreening{}), "production"))[CheckAMLQuorum]
		if got.Result != c.result || got.DetailCode != c.detail {
			t.Errorf("quorum met=%v: %+v", c.met, got)
		}
	}
	// A read failure is never a pass.
	got := byCode(amlChecksFrom(amlData{QuorumMet: true}, errors.New("boom"), 1, reg(eligibleScreening{}), "test"))
	if got[CheckAMLQuorum].Result != ResultNotEvaluable || got[CheckAMLScreening].Result != ResultNotEvaluable {
		t.Errorf("a read error must be not_evaluable for the database-derived checks: %+v", got)
	}
}

func TestAMLChecks_Screening_Matrix(t *testing.T) {
	ok := amlData{QuorumMet: true, Total: 2, WithPolicy: 2}
	for _, c := range []struct {
		name   string
		d      amlData
		refs   int
		comps  []providerkind.Registration
		env    string
		result string
		detail string
	}{
		{"production eligible provider, every policy", ok, 2, reg(eligibleScreening{}), "production", ResultPass, "ok"},
		{"non-production eligible provider", ok, 2, reg(eligibleScreening{}), "development", ResultPass, "ok"},
		{"MOCK allowed outside production", ok, 2, reg(synthScreening{}), "development", ResultPass, "ok_mock_provider"},
		{"MOCK allowed in the test environment", ok, 2, reg(synthScreening{}), "test", ResultPass, "ok_mock_provider"},
		{"MOCK is NOT configured in production", ok, 2, reg(synthScreening{}), "production", ResultFail, "synthetic_provider_in_production"},
		{"an unmarked provider is not production-eligible", ok, 2, reg(unmarkedScreening{}), "production", ResultFail, "synthetic_provider_in_production"},
		{"no provider registered", ok, 2, []providerkind.Registration{{Domain: "payments", Name: "provider", Component: synthScreening{}}}, "development", ResultFail, "no_screening_provider"},
		{"a nil provider component is no provider", ok, 2, reg(nil), "development", ResultFail, "no_screening_provider"},
		{"provider registered under the wrong name", ok, 2, []providerkind.Registration{{Domain: "aml", Name: "monitoring", Component: eligibleScreening{}}}, "development", ResultFail, "no_screening_provider"},
		{"no component registry at all", ok, 2, nil, "development", ResultNotEvaluable, "no_component_registry"},
		{"one jurisdiction lacks a policy", amlData{QuorumMet: true, Total: 2, WithPolicy: 1}, 2, reg(eligibleScreening{}), "production", ResultFail, "policy_missing_for_jurisdiction"},
		{"no policy at all", amlData{QuorumMet: true, Total: 1, WithPolicy: 0}, 1, reg(eligibleScreening{}), "production", ResultFail, "policy_missing_for_jurisdiction"},
		{"no jurisdiction referenced", amlData{QuorumMet: true}, 0, reg(eligibleScreening{}), "production", ResultFail, "no_jurisdiction_referenced"},
		{"referenced but the reader counted none", amlData{QuorumMet: true}, 1, reg(eligibleScreening{}), "production", ResultFail, "no_jurisdiction_referenced"},
		// A mock with an incomplete policy set is still a fail: the mock only satisfies the PROVIDER half.
		{"MOCK allowed but policy missing", amlData{QuorumMet: true, Total: 1, WithPolicy: 0}, 1, reg(synthScreening{}), "development", ResultFail, "policy_missing_for_jurisdiction"},
	} {
		got := byCode(amlChecksFrom(c.d, nil, c.refs, c.comps, c.env))[CheckAMLScreening]
		if got.Result != c.result || got.DetailCode != c.detail {
			t.Errorf("%s: %+v, want %s/%s", c.name, got, c.result, c.detail)
		}
	}
}

// The check codes are part of the contract (OpenAPI, Back Office labels): pin them, and require them in the non-passing set an
// approver must acknowledge when they are not pass.
func TestAMLChecks_CodesPinnedAndAcknowledged(t *testing.T) {
	if CheckAMLQuorum != "R-AML-QUORUM" || CheckAMLScreening != "R-AML-SCREENING" || CheckAMLMonitoring != "R-AML-MONITORING" {
		t.Fatal("AML readiness check codes drifted")
	}
	if SnapshotVersion != 2 {
		t.Fatalf("the check set changed with A7, so the snapshot version is 2, got %d", SnapshotVersion)
	}
	r := Readiness{Version: SnapshotVersion, Environment: "test", Checks: amlChecksFrom(amlData{}, nil, 0, nil, "test")}
	got := r.NonPassing()
	want := []string{CheckAMLMonitoring, CheckAMLQuorum, CheckAMLScreening}
	sort.Strings(want)
	if len(got) != 3 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("non-passing set %v, want %v (not_evaluable counts as not pass)", got, want)
	}
	// The monitoring item stays in the acknowledged set even when everything else is configured: it cannot be configured away.
	full := Readiness{Version: SnapshotVersion, Environment: "production", Checks: amlChecksFrom(amlData{QuorumMet: true, Total: 1, WithPolicy: 1}, nil, 1, reg(eligibleScreening{}), "production")}
	if np := full.NonPassing(); len(np) != 1 || np[0] != CheckAMLMonitoring {
		t.Fatalf("with everything configured only monitoring remains: %v", np)
	}
	// The readiness hash changes with the check set (so a request filed against the old set goes stale).
	a := Readiness{Version: 1, Environment: "test"}
	b := Readiness{Version: SnapshotVersion, Environment: "test", Checks: amlChecksFrom(amlData{}, nil, 0, nil, "test")}
	if a.Hash() == b.Hash() {
		t.Fatal("adding checks must change the readiness hash")
	}
}
