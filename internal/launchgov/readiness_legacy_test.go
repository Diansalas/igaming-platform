package launchgov

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Lic7: pure (DB-free) tests of the two new readiness items. The database halves are in
// readiness_legacy_integration_test.go.

func TestRiskRulesLegacyFrom_PassFailArms(t *testing.T) {
	jur := riskLegacyRule{ID: uuid.New(), Jurisdict: true}
	dep := riskLegacyRule{ID: uuid.New(), DepositOp: true}
	both := riskLegacyRule{ID: uuid.New(), Jurisdict: true, DepositOp: true}
	for _, c := range []struct {
		name          string
		rules         []riskLegacyRule
		result, detal string
		jur, dep      int64
	}{
		{"none", nil, ResultPass, "ok", 0, 0},
		{"jurisdiction only", []riskLegacyRule{jur}, ResultFail, "jurisdiction_scoped_rules_present", 1, 0},
		{"deposit only", []riskLegacyRule{dep}, ResultFail, "deposit_rules_present", 0, 1},
		{"both kinds", []riskLegacyRule{jur, dep}, ResultFail, "both_present", 1, 1},
		{"one rule that is both", []riskLegacyRule{both}, ResultFail, "both_present", 1, 1},
	} {
		r := &Readiness{}
		got := riskRulesLegacyFrom(c.rules, r)
		if got.Code != CheckRiskRulesLegacy || got.Result != c.result || got.DetailCode != c.detal {
			t.Errorf("%s: %+v, want %s/%s", c.name, got, c.result, c.detal)
		}
		if r.Info[InfoRiskLegacyJurisdictionScoped] != c.jur || r.Info[InfoRiskLegacyDepositOperation] != c.dep {
			t.Errorf("%s: info %v", c.name, r.Info)
		}
		if c.result == ResultPass && len(r.Refs) != 0 {
			t.Errorf("%s: a pass carries no rule ids: %v", c.name, r.Refs)
		}
	}
}

func TestRiskRulesLegacyFrom_RefsAreIDsOnlyAndCapped(t *testing.T) {
	var rules []riskLegacyRule
	for i := 0; i < MaxRefsPerKey+7; i++ {
		rules = append(rules, riskLegacyRule{ID: uuid.New(), Jurisdict: true})
	}
	r := &Readiness{}
	got := riskRulesLegacyFrom(rules, r)
	if got.Result != ResultFail {
		t.Fatal(got)
	}
	if r.Info[InfoRiskLegacyJurisdictionScoped] != int64(MaxRefsPerKey+7) {
		t.Fatalf("the count is the FULL count: %v", r.Info)
	}
	ids := r.Refs[RefsRiskLegacyJurisdictionScoped]
	if len(ids) != MaxRefsPerKey {
		t.Fatalf("refs capped at %d, got %d", MaxRefsPerKey, len(ids))
	}
	for _, id := range ids {
		if _, err := uuid.Parse(id); err != nil {
			t.Fatalf("a ref is a bare uuid, got %q", id)
		}
	}
	if _, ok := r.Refs[RefsRiskLegacyDepositOperation]; ok {
		t.Fatal("no deposit key when there is no deposit rule")
	}
	// the detail code is a closed word: it never embeds an id or a count
	if fmt.Sprint(got.DetailCode) != "jurisdiction_scoped_rules_present" {
		t.Fatal(got)
	}
}

func TestKYCPolicyFrom_PassFailArms(t *testing.T) {
	for _, c := range []struct {
		name                string
		lic                 bool
		dep, casino, sports int64
		result, detail      string
	}{
		{"no licence jurisdiction", false, 0, 0, 0, ResultFail, "no_licence_jurisdiction"},
		{"no licence jurisdiction wins over counts", false, 3, 3, 3, ResultFail, "no_licence_jurisdiction"},
		{"zero active policies", true, 0, 0, 0, ResultFail, "no_active_policy"},
		{"deposit only", true, 1, 0, 0, ResultPass, "ok_partial_operations_dormant"},
		{"play only", true, 0, 1, 1, ResultPass, "ok_partial_operations_dormant"},
		{"casino play missing", true, 2, 0, 1, ResultPass, "ok_partial_operations_dormant"},
		{"all three covered", true, 1, 1, 1, ResultPass, "ok"},
	} {
		got := kycPolicyFrom(c.lic, c.dep, c.casino, c.sports)
		if got.Code != CheckKYCPolicy || got.Result != c.result || got.DetailCode != c.detail {
			t.Errorf("%s: %+v, want %s/%s", c.name, got, c.result, c.detail)
		}
	}
}

// Both items are acknowledgeable informational checks: a failing one is in the non-passing set a request must acknowledge
// exactly, neither is a hard-precondition code, and each changes the readiness hash.
func TestLic7Items_AcknowledgeableNotHard(t *testing.T) {
	if CheckRiskRulesLegacy != "R-RISK-RULES-LEGACY" || CheckKYCPolicy != "R-KYC-POLICY" {
		t.Fatal("lic7 check codes drifted (OpenAPI, Back Office labels and the runbook name them)")
	}
	r := Readiness{Version: SnapshotVersion, Environment: "test", Checks: []Check{
		ck(CheckRiskRulesLegacy, ResultFail, "deposit_rules_present"),
		ck(CheckKYCPolicy, ResultFail, "no_active_policy"),
		ck(CheckJurisdictionConfig, ResultPass, "ok"),
	}}
	got := r.NonPassing()
	want := []string{CheckKYCPolicy, CheckRiskRulesLegacy}
	sort.Strings(want)
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("non-passing set %v, want %v", got, want)
	}
	// not hard: the hard-precondition source never names them, and ackErr treats them like every other non-pass.
	for _, f := range []string{"hard.go", "decide.go", "execute.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"CheckRiskRulesLegacy", "CheckKYCPolicy", "R-RISK-RULES-LEGACY", "R-KYC-POLICY"} {
			if strings.Contains(string(b), name) {
				t.Fatalf("%s references %s: the lic7 items must stay acknowledgeable, never a hard precondition", f, name)
			}
		}
	}
	pass := Readiness{Version: SnapshotVersion, Environment: "test", Checks: []Check{
		ck(CheckRiskRulesLegacy, ResultPass, "ok"), ck(CheckKYCPolicy, ResultPass, "ok"), ck(CheckJurisdictionConfig, ResultPass, "ok"),
	}}
	if pass.Hash() == r.Hash() {
		t.Fatal("the items are part of the readiness hash")
	}
	if len(pass.NonPassing()) != 0 {
		t.Fatalf("passing items need no acknowledgement: %v", pass.NonPassing())
	}
}
