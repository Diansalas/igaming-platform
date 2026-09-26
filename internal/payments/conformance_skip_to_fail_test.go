package payments

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Stage 10.3 W1a (WH-VENDOR-SCHEME-1; 01-provider-trust-analysis.md §1.2
// "Existing skips become failures"; 04-review-qa.md §3): the conformance
// cases that used to t.Skip for any adapter that is not *MockProvider must
// now FAIL for such an adapter, exactly as the K3 tenant-binding case
// already did. Otherwise a real adapter "passes" ADR 0022 §6 by skipping.
//
// The suite takes a *testing.T, so a failing run cannot be observed from
// inside the same test without failing it. The proof therefore re-runs this
// test binary as a child process that executes the suite against a
// non-mock adapter, and asserts on the child's -v output: every converted
// case must be reported as FAIL and none may be reported as SKIP.

const conformanceChildEnv = "W1A_PAYMENTS_CONFORMANCE_CHILD"

// nonMockPaymentsAdapter stands in for a future real adapter. It embeds
// the mock so every non-callback case still works, but it is NOT
// type-assertable to *MockProvider, which is what the old skips keyed on.
type nonMockPaymentsAdapter struct{ *MockProvider }

// paymentsConvertedConformanceCases are the three cases that previously
// skipped (conformance_test.go, formerly lines 85, 114 and 221), in -v
// subtest-name form.
var paymentsConvertedConformanceCases = []string{
	"HandleCallback_surfaces_decline_and_ambiguous_distinguishably",
	"redelivered_callback_is_idempotent",
	"inbound_key_material_is_rejected,_not_stored_or_logged",
}

// TestPaymentsConformance_W1a_NonMockAdapterChild is the child half of
// TestPaymentsConformance_W1a_SkipToFail_NonMockAdapterFailsNotSkips. It is
// inert (returns immediately) unless the parent set conformanceChildEnv.
func TestPaymentsConformance_W1a_NonMockAdapterChild(t *testing.T) {
	if os.Getenv(conformanceChildEnv) != "1" {
		return
	}
	RunProviderConformanceSuite(t, func() PaymentProvider {
		return nonMockPaymentsAdapter{NewMockProvider("real-psp-standin")}
	})
}

func TestPaymentsConformance_W1a_SkipToFail_NonMockAdapterFailsNotSkips(t *testing.T) {
	if os.Getenv(conformanceChildEnv) == "1" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestPaymentsConformance_W1a_NonMockAdapterChild$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), conformanceChildEnv+"=1")
	out, _ := cmd.CombinedOutput() // the child is EXPECTED to fail
	output := string(out)

	for _, name := range paymentsConvertedConformanceCases {
		full := "TestPaymentsConformance_W1a_NonMockAdapterChild/" + name
		if strings.Contains(output, "--- SKIP: "+full) {
			t.Errorf("conformance case %q still SKIPs for a non-mock adapter; it must fail (ADR 0022 §6)", name)
		}
		if !strings.Contains(output, "--- FAIL: "+full) {
			t.Errorf("conformance case %q did not FAIL for a non-mock adapter", name)
		}
	}
	if t.Failed() {
		t.Logf("child output:\n%s", output)
	}
}
