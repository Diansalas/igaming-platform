package rg

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func s1RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}

func s1Read(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(s1RepoRoot(t), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const m0135Up = "migrations/0135_rg_limits_schema_and_index.up.sql"

func normPrivs(s string) string {
	var ps []string
	for _, p := range strings.Split(s, ",") {
		ps = append(ps, strings.TrimSpace(p))
	}
	sort.Strings(ps)
	return strings.Join(ps, ",")
}

// deploy/init-app-role.sql re-asserts exactly the runtime grants migration 0135 makes (the
// integration suite's CatalogAndGrants subtest proves the migration's grants against a database).
func TestInitAppRoleSQLMatchesMigration0135Grants(t *testing.T) {
	want := map[string]string{
		"database_environment":            "SELECT",
		"rg_limit_config_change_requests": "INSERT,SELECT",
		"rg_limit_configs":                "INSERT,SELECT",
		"player_rg_limit_changes":         "INSERT,SELECT",
	}
	deploy := s1Read(t, "deploy/init-app-role.sql")
	gotDeploy := map[string]string{}
	for _, m := range regexp.MustCompile(`\('(database_environment|rg_[a-z_]+|player_rg_[a-z_]+)', '([A-Z, ]+)'\)`).FindAllStringSubmatch(deploy, -1) {
		gotDeploy[m[1]] = normPrivs(m[2])
	}
	mig := s1Read(t, m0135Up)
	gotMig := map[string]string{}
	for _, m := range regexp.MustCompile(`'GRANT ([A-Z, ]+) ON (database_environment|rg_[a-z_]+|player_rg_[a-z_]+) TO igaming_runtime'`).FindAllStringSubmatch(mig, -1) {
		gotMig[m[2]] = normPrivs(m[1])
	}
	for tbl, privs := range want {
		if gotDeploy[tbl] != privs {
			t.Errorf("deploy/init-app-role.sql %s = %q, want %q", tbl, gotDeploy[tbl], privs)
		}
		if gotMig[tbl] != privs {
			t.Errorf("migration 0135 %s = %q, want %q", tbl, gotMig[tbl], privs)
		}
	}
	if len(gotDeploy) != len(want) || len(gotMig) != len(want) {
		t.Errorf("unexpected table sets: deploy=%v migration=%v", gotDeploy, gotMig)
	}
	for _, src := range []string{deploy, mig} {
		if !strings.Contains(src, "GRANT UPDATE (status) ON rg_limit_config_change_requests TO igaming_runtime") {
			t.Error("the only runtime UPDATE is the request status column (deploy and migration)")
		}
	}
	// No grant of any write privilege on the environment anchor, anywhere.
	if regexp.MustCompile(`(?im)^[^-\n]*\bGRANT\b[^;'\n]*(INSERT|UPDATE|DELETE|TRUNCATE|ALL)[^;'\n]*ON\s+database_environment`).MatchString(deploy + "\n" + mig) {
		t.Error("database_environment must never be writable by the runtime role")
	}
}

// The Go vocabulary mirrors the migration's CHECK lists exactly.
func TestRGLimitVocabularyMatchesMigration0135(t *testing.T) {
	mig := s1Read(t, m0135Up)
	list := func(re string) []string {
		m := regexp.MustCompile(re).FindStringSubmatch(mig)
		if m == nil {
			t.Fatalf("pattern %q not found in 0135", re)
		}
		var out []string
		for _, s := range regexp.MustCompile(`'([a-z_0-9]+)'`).FindAllStringSubmatch(m[1], -1) {
			out = append(out, s[1])
		}
		return out
	}
	eq := func(name string, got []string, want []string) {
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: migration %v, Go %v", name, got, want)
		}
	}
	var kinds, limitKinds, periods, changes []string
	for _, k := range ConfigControlKinds {
		kinds = append(kinds, string(k))
	}
	for _, k := range PlayerLimitKinds {
		limitKinds = append(limitKinds, string(k))
	}
	for _, p := range PeriodKinds {
		periods = append(periods, string(p))
	}
	for _, c := range ChangeKinds {
		changes = append(changes, string(c))
	}
	eq("rg_limit_configs.control_kind", list(`control_kind\s+TEXT NOT NULL CHECK \(control_kind IN \(([^)]*)\)\),\n    asset_code\s+TEXT NULL REFERENCES assets \(code\) ON DELETE RESTRICT,\n    effective_from`), kinds)
	eq("player_rg_limit_changes.control_kind", list(`control_kind\s+TEXT NOT NULL CHECK \(control_kind IN \(('deposit', 'loss', 'wager', 'session_duration')\)\)`), limitKinds)
	eq("period_kind", list(`period_kind\s+TEXT NULL CHECK \(period_kind IN \(([^)]*)\)\)`), periods)
	eq("change_kind", list(`change_kind\s+TEXT NOT NULL CHECK \(change_kind IN \(([^)]*)\)\)`), changes)
	if !strings.Contains(mig, "CHECK (restriction_type IN ('self_exclusion', 'time_out'))") || RestrictionTimeOut != "time_out" {
		t.Error("player_restrictions.restriction_type must admit exactly self_exclusion and time_out")
	}
	if !strings.Contains(mig, "attestation_ref <> 'synthetic'") || SyntheticAttestationRef != "synthetic" {
		t.Error("the synthetic attestation literal must match the migration")
	}
}

// The migration adds no enforcement (S4) and seeds no configuration row (no legal value).
func TestMigration0135_NoEnforcementNoSeededValues(t *testing.T) {
	mig := s1Read(t, m0135Up)
	for _, forbidden := range []string{"INSERT INTO rg_limit_configs", "INSERT INTO player_rg_limit_changes", "INSERT INTO database_environment",
		"ON ledger_transactions", "ON deposit_intents\n    FOR EACH"} {
		if strings.Contains(mig, forbidden) {
			t.Errorf("migration 0135 must not contain %q (S1 seeds nothing and enforces nothing)", forbidden)
		}
	}
	// The only triggers it creates are on its own tables (no hook on a money path).
	for _, m := range regexp.MustCompile(`CREATE TRIGGER \w+\s+BEFORE [A-Z ]+ ON (\w+)`).FindAllStringSubmatch(mig, -1) {
		switch m[1] {
		case "database_environment", "rg_limit_config_change_requests", "rg_limit_configs", "player_rg_limit_changes",
			"player_restrictions": // security L2: the player-insert id is forced (not a money path)
		default:
			t.Errorf("migration 0135 creates a trigger on %s", m[1])
		}
	}
}
