package withdrawal

import (
	"os"
	"strings"
	"testing"
)

// ADR 0112 slice 4 (migration 0131, residual 14(b2)): the deploy grant script, both CI grant steps and
// the runtime-role document all revoke DELETE and TRUNCATE on withdrawal_requests from igaming_runtime.
// (The WD012 trigger is the binding control; this pins the defence in depth, DB-free.)
func TestDeleteTruncateRevokePinned(t *testing.T) {
	for _, f := range []string{
		"../../deploy/init-app-role.sql",
		"../../.github/workflows/ci.yml",
		"../../.github/workflows/ci-selfhosted.yml",
		"../../docs/security/runtime-role-separation.md",
	} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "REVOKE DELETE, TRUNCATE ON withdrawal_requests FROM igaming_runtime") {
			t.Errorf("%s must revoke DELETE and TRUNCATE on withdrawal_requests from igaming_runtime", f)
		}
	}
}
