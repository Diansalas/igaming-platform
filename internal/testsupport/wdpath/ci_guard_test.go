//go:build integration

package wdpath

import "testing"

// ledger-finance review C2: the tamper-plant evidence can never be skipped on a CI database.
func TestPersistentDecision_NeverSkipsSilentlyInCI(t *testing.T) {
	for _, tc := range []struct {
		name, extra string
		ci          bool
		skip, fail  bool
	}{
		{"igaming_platform_ci_local", "", false, true, false},                 // persistent shared: skip
		{"igaming_platform_dev", "", false, true, false},                      // persistent dev: skip
		{"priv_x", "", false, false, false},                                   // private: run
		{"igaming_platform_test", "", true, false, false},                     // CI database: run
		{"igaming_platform_test", "igaming_platform_test", false, true, true}, // CI db made persistent: FAIL
		{"igaming_platform_ci_local", "", true, true, true},                   // skip decision while CI=true: FAIL
		{"priv_x", "priv_x", true, true, true},                                // extended list in CI: FAIL
	} {
		skip, fail := persistentDecision(tc.name, tc.extra, tc.ci)
		if skip != tc.skip || (fail != "") != tc.fail {
			t.Errorf("%+v: got skip=%v fail=%q", tc, skip, fail)
		}
	}
}
