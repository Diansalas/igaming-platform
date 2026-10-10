package tenant

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deploy/init-app-role.sql re-asserts, on every run, exactly the grants migration 0128 gives
// igaming_runtime on the three launch tables (ADR 0112 4.6). This static pin compares the GRANT
// statements of the two files; the integration twin
// (TestLaunchGov_InitAppRoleSQL_ReassertsLaunchTableGrants) proves the effect on a database.
func TestLaunchGrants_InitAppRoleSQLMatchesMigration(t *testing.T) {
	root := repoRoot(t)
	mig, err := os.ReadFile(filepath.Join(root, "migrations", "0128_launch_authorisation_status_governance.up.sql"))
	if err != nil {
		t.Fatal(err)
	}
	deploy, err := os.ReadFile(filepath.Join(root, "deploy", "init-app-role.sql"))
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{
		"GRANT SELECT, INSERT ON launch_authorisation_requests TO igaming_runtime",
		"GRANT UPDATE (status, decided_at, refusal_code, executing_txid) ON launch_authorisation_requests TO igaming_runtime",
		"GRANT SELECT, INSERT ON launch_authorisation_approvals TO igaming_runtime",
		"GRANT SELECT, INSERT ON launch_status_transitions TO igaming_runtime",
	} {
		if !strings.Contains(string(mig), g) {
			t.Errorf("migration 0128 lacks %q", g)
		}
		if !strings.Contains(string(deploy), g) {
			t.Errorf("deploy/init-app-role.sql lacks %q", g)
		}
	}
	// Neither file may grant DELETE / TRUNCATE / table-level UPDATE on the launch tables.
	for name, b := range map[string][]byte{"migration 0128": mig, "deploy/init-app-role.sql": deploy} {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "GRANT") && strings.Contains(line, "launch_") &&
				(strings.Contains(line, "DELETE") || strings.Contains(line, "TRUNCATE") || strings.Contains(line, "GRANT ALL")) {
				t.Errorf("%s widens the launch-table grants: %s", name, strings.TrimSpace(line))
			}
		}
	}
}
