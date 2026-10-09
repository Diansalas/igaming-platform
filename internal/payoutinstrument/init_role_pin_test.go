package payoutinstrument

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// deploy/init-app-role.sql re-asserts the 0123 grants on every run; it must
// list exactly the privileges migration 0123 grants (and the grant-pin test
// proves the migration's grants against the database).
func TestInitAppRoleSQLMatchesMigrationGrants(t *testing.T) {
	root := repoRoot(t)
	want := map[string]string{
		"payout_instrument_kinds":                "SELECT",
		"payout_instrument_verification_max_age": "SELECT",
		"payout_instruments":                     "INSERT,SELECT,UPDATE",
		"payout_instrument_verifications":        "INSERT,SELECT",
		"payout_instrument_fingerprint_owners":   "INSERT,SELECT",
		"payout_instrument_blocking_events":      "INSERT,SELECT",
		"payout_attempt_destination_snapshots":   "INSERT,SELECT",
	}
	norm := func(privs string) string {
		var ps []string
		for _, p := range strings.Split(privs, ",") {
			ps = append(ps, strings.TrimSpace(p))
		}
		sort.Strings(ps)
		return strings.Join(ps, ",")
	}
	deploy, err := os.ReadFile(filepath.Join(root, "deploy", "init-app-role.sql"))
	if err != nil {
		t.Fatal(err)
	}
	gotDeploy := map[string]string{}
	for _, m := range regexp.MustCompile(`\('(payout_[a-z_]+)', '([A-Z, ]+)'\)`).FindAllStringSubmatch(string(deploy), -1) {
		gotDeploy[m[1]] = norm(m[2])
	}
	mig, err := os.ReadFile(filepath.Join(root, "migrations", "0123_payout_instruments.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	gotMig := map[string]string{}
	for _, m := range regexp.MustCompile(`GRANT ([A-Z, ]+) ON (payout_[a-z_]+) TO igaming_runtime`).FindAllStringSubmatch(string(mig), -1) {
		gotMig[m[2]] = norm(m[1])
	}
	for tbl, privs := range want {
		if gotDeploy[tbl] != privs {
			t.Errorf("deploy/init-app-role.sql %s = %q, want %q", tbl, gotDeploy[tbl], privs)
		}
		if gotMig[tbl] != privs {
			t.Errorf("migration 0123 %s = %q, want %q", tbl, gotMig[tbl], privs)
		}
	}
	if len(gotDeploy) != len(want) || len(gotMig) != len(want) {
		t.Errorf("unexpected table sets: deploy=%v migration=%v", gotDeploy, gotMig)
	}
}
