// Package sql holds the RDS role-init script and its static regression test (no database needed).
package sql

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// SRSP-1 (security review of STAGING-PREP-1): the script runs before migrate on EVERY deploy and
// rotation, so any blanket table/sequence GRANT that is not first-install-only silently widens the
// narrowed per-table privileges the migrations set (alert_routes, ledger_adjustment_*, aml_*, ...).
// The invariant pinned here is that every "ON ALL TABLES|SEQUENCES" grant sits inside the block
// guarded by "schema_migrations IS NULL", i.e. it only runs on a schema with no migrated tables.
func TestRoleInitBlanketGrantsAreFirstInstallOnly(t *testing.T) {
	b, err := os.ReadFile("init-runtime-role.rds.sql")
	if err != nil {
		t.Fatal(err)
	}
	var code []string
	for _, l := range strings.Split(string(b), "\n") {
		if i := strings.Index(l, "--"); i >= 0 {
			l = l[:i]
		}
		code = append(code, l)
	}
	src := strings.Join(code, "\n")
	blanket := regexp.MustCompile(`(?is)\bGRANT\b[^;]*\bON\s+ALL\s+(TABLES|SEQUENCES|FUNCTIONS)\b[^;]*;`)
	guard := regexp.MustCompile(`(?is)IF\s+to_regclass\('public\.schema_migrations'\)\s+IS\s+NULL\s+THEN(.*?)END\s+IF;`)
	guarded := ""
	for _, m := range guard.FindAllStringSubmatch(src, -1) {
		guarded += m[1]
	}
	all := blanket.FindAllString(src, -1)
	if len(all) == 0 {
		t.Fatal("expected the first-install blanket grants to exist")
	}
	for _, g := range all {
		if !strings.Contains(guarded, g) {
			t.Errorf("blanket grant outside the first-install guard (would widen a migrated database on re-run): %s", strings.TrimSpace(g))
		}
	}
}

// The authoritative narrowings the script re-asserts must not be dropped.
func TestRoleInitKeepsExplicitRevokes(t *testing.T) {
	b, err := os.ReadFile("init-runtime-role.rds.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"REVOKE INSERT, UPDATE, DELETE ON schema_migrations FROM igaming_runtime",
		"REVOKE ALL ON actor_proof_keys FROM igaming_runtime",
		"REVOKE ALL ON actor_proof_nonces FROM igaming_runtime",
		"REVOKE TEMPORARY ON DATABASE %I FROM igaming_runtime",
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("init-runtime-role.rds.sql lost: %s", want)
		}
	}
}
