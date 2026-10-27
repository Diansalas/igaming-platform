package aml

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// lic23 (N-4 / SN-5): the non-forgeable environment guard sits in BOTH database paths that enable a gate or accept a worker result.
// (The helper itself is exercised on scratch databases by the integration test; these call sites need a production-anchored
// database to exercise, so they are pinned here.)
func TestMigration0181_EnvironmentGuardCallSites(t *testing.T) {
	b, err := os.ReadFile("../../migrations/0181_lic10_residence_maker_checker_and_aml_triggers.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, fn := range []string{"CREATE FUNCTION aml_screening_trigger_policy_set(", "CREATE FUNCTION aml_screening_assert_worker_provider("} {
		i := strings.Index(src, fn)
		if i < 0 {
			t.Fatalf("%s not found", fn)
		}
		j := strings.Index(src[i:], "\n$$;")
		body := src[i : i+j]
		if !regexp.MustCompile(`IF NOT public\.aml_screening_gate_environment_ok\(\) THEN`).MatchString(body) {
			t.Errorf("%s must refuse when the database environment anchor is production or unrecorded", fn)
		}
	}
	// The helper reads the anchor: an unrecorded anchor is production (rg_database_environment).
	if !strings.Contains(src, "SELECT public.rg_database_environment() IN ('development', 'staging', 'test')") {
		t.Error("the guard must read the non-forgeable anchor through rg_database_environment()")
	}
}
