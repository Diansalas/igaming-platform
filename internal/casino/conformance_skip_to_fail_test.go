package casino

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Stage 10.3 W1a (WH-VENDOR-SCHEME-1; 01-provider-trust-analysis.md §1.2
// "Existing skips become failures"; 04-review-qa.md §3): the conformance
// cases that used to t.Skip for any adapter that is not
// *MockCasinoProvider must now FAIL for such an adapter, exactly as the K3
// tenant-binding case already did.
//
// The suite takes a *testing.T, so the proof re-runs this test binary as a
// child process that executes the suite against a non-mock adapter and
// asserts on the child's -v output: every converted case is FAIL, none is
// SKIP.

const casinoConformanceChildEnv = "W1A_CASINO_CONFORMANCE_CHILD"

// nonMockCasinoAdapter stands in for a future real adapter: it embeds the
// mock, so the non-callback cases still work, but is NOT type-assertable to
// *MockCasinoProvider, which is what the old skips keyed on.
type nonMockCasinoAdapter struct{ *MockCasinoProvider }

// casinoConvertedConformanceCases are the four cases that previously
// skipped (conformance_test.go, formerly lines 164, 187, 270 and 292), in
// -v subtest-name form.
var casinoConvertedConformanceCases = []string{
	"a_simulated_transport_failure_surfaces_as_an_error,_not_a_result",
	"HandleCallback_rejects_a_missing_or_invalid_signature",
	"HandleCallback_rejects_a_malformed_payload",
	"HandleCallback_parses_a_redelivered_payload_identically",
}

// TestCasinoConformance_W1a_NonMockAdapterChild is the child half of
// TestCasinoConformance_W1a_SkipToFail_NonMockAdapterFailsNotSkips. It is
// inert unless the parent set casinoConformanceChildEnv.
func TestCasinoConformance_W1a_NonMockAdapterChild(t *testing.T) {
	if os.Getenv(casinoConformanceChildEnv) != "1" {
		return
	}
	RunProviderConformanceSuite(t, func() CasinoProvider {
		return nonMockCasinoAdapter{NewMockCasinoProvider("real-casino-standin", "EUR", "USD")}
	})
}

func TestCasinoConformance_W1a_SkipToFail_NonMockAdapterFailsNotSkips(t *testing.T) {
	if os.Getenv(casinoConformanceChildEnv) == "1" {
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestCasinoConformance_W1a_NonMockAdapterChild$", "-test.v", "-test.count=1")
	cmd.Env = append(os.Environ(), casinoConformanceChildEnv+"=1")
	out, _ := cmd.CombinedOutput() // the child is EXPECTED to fail
	output := string(out)

	for _, name := range casinoConvertedConformanceCases {
		full := "TestCasinoConformance_W1a_NonMockAdapterChild/" + name
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
