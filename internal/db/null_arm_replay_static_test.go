// A-18 (ADR 0099 §6.2/§6.9 TM-3, security K1-1 future-proofing condition,
// binding per the orchestrator): a permanent static guard that replays
// every CREATE POLICY / DROP POLICY statement across migrations/*.up.sql,
// in migration-version order, to compute the EFFECTIVE policy set, then
// flags any effective, non-SELECT, PERMISSIVE policy that has a bare
// `tenant_id IS NULL` arm (the K1-1 shape: a NULL-tenant row is reachable
// with no other required positive GUC) UNLESS:
//   - it is guarded by a platform-admin or platform-service GUC
//     requirement (the "platform family" and "reference/service" shapes
//     this codebase already uses everywhere outside K1), or
//   - it is guarded by a positive subject-tenant equality requirement (the
//     ADR 0104/0109 `subject_tenant_read` shape and its 0110
//     `alerts_subject_tenant_raise` INSERT counterpart: despite containing
//     the literal substring "tenant_id IS NULL", these require a REAL,
//     non-NULL `app.tenant_id` value to match `subject_tenant_id`, so they
//     are not a free NULL arm at all), or
//   - the table is one of the seven ADR 0099 §6.4 tables migration 0112
//     already restrictively fences (staff_users, audit_log, sessions,
//     login_attempts, persons, player_restrictions, risk_rules) - A-17
//     (migration_0112_integration_test.go) is what proves THEIR fence is
//     actually present; A-18's job is to catch a FUTURE, different table
//     shipping the same exposure unfenced.
//
// This is a pragmatic regex/line-scanner over the migration SQL text, not
// a real SQL parser (matching this codebase's own established convention
// for this class of guard - see internal/txscope's INV-IO-1(c) test and
// internal/ledger's lockorder_static_test.go, both of which state the same
// "lexical, not semantic" trade-off explicitly). It is proven correct by
// planted cases below for every shape enumerated above, both a positive
// (flagged) and a negative (not flagged) case for each guard type, plus a
// DROP-then-recreate-narrower case (the real 0043/0049 precedent ADR 0099
// §6.2 cites) and a DROP-with-no-recreation case.
package db

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// a18FencedTables is the ADR 0099 §6.4 closed list - already covered by
// migration 0112's own restrictive fence (A-17's job to verify), so A-18
// does not re-flag them.
var a18FencedTables = map[string]bool{
	"staff_users":         true,
	"audit_log":           true,
	"sessions":            true,
	"login_attempts":      true,
	"persons":             true,
	"player_restrictions": true,
	"risk_rules":          true,
}

// a18Policy is one effective (CREATE'd, not yet DROP'd) policy.
type a18Policy struct {
	table       string
	name        string
	restrictive bool
	command     string // SELECT/INSERT/UPDATE/DELETE/ALL
	body        string // the full statement text, for the arm/guard regexes
}

// a18CreatePattern/a18DropPattern match a REAL top-level CREATE/DROP POLICY
// statement - anchored to (optional leading whitespace then) the literal
// keywords at the start of a line, which is exactly what every real
// statement in this codebase looks like, and which deliberately does NOT
// match a CREATE POLICY spelled out inside a quoted string being built for
// EXECUTE format(...) (migration 0112's own DO $$ loop): that dynamic
// generation always writes the quote character first (e.g.
// "            'CREATE POLICY acting_fence_select ON %I ...", t)"), so the
// literal keyword is never the first non-whitespace token on its line.
// Every dynamically-generated policy in this codebase is a RESTRICTIVE
// fence (not a permissive NULL-arm exposure this guard cares about) -
// disclosed here rather than silently assumed; TestA18_DynamicPolicyGenerationIsRestrictiveOnly
// pins that fact so a future change violating it fails the build.
var (
	// Identifiers are matched as \w+ (word characters only), never \S+ -
	// \S+ would greedily swallow a trailing ";" (no trailing whitespace
	// before it) into the captured table name, breaking the DROP-side
	// lookup key match against the CREATE side's own (punctuation-free)
	// capture.
	a18CreatePattern = regexp.MustCompile(`(?m)^[ \t]*CREATE POLICY\s+(\w+)\s+ON\s+(\w+)`)
	a18DropPattern   = regexp.MustCompile(`(?m)^[ \t]*DROP POLICY(?:\s+IF EXISTS)?\s+(\w+)\s+ON\s+(\w+)`)
)

var (
	a18ForCommandPattern = regexp.MustCompile(`(?i)\bFOR\s+(SELECT|INSERT|UPDATE|DELETE|ALL)\b`)
	// a18BareTenantNullArm matches "tenant_id IS NULL" on the table's OWN
	// column - i.e. NOT prefixed by a dot (which would instead be
	// something like "app.tenant_id ... IS NULL", a GUC-unset check, a
	// completely different and common non-exposing idiom).
	a18BareTenantNullArm    = regexp.MustCompile(`(?i)(^|[^.\w])tenant_id\s+IS\s+NULL`)
	a18PlatformAdminGuard   = regexp.MustCompile(`(?i)platform_admin_principal_id`)
	a18PlatformServiceGuard = regexp.MustCompile(`(?i)platform_service_id`)
	// a18SubjectTenantPositiveGuard matches the subject_tenant_read/
	// alerts_subject_tenant_raise shape: subject_tenant_id required to
	// equal a real value (never merely "IS NULL").
	a18SubjectTenantPositiveGuard = regexp.MustCompile(`(?i)subject_tenant_id\s*=`)
)

// a18ExtractStatement returns the full statement text starting at
// startIdx (the position of "CREATE"/"DROP") up to and including the next
// top-level semicolon. Every CREATE POLICY/DROP POLICY statement in this
// codebase is a single SQL statement with no nested semicolons (no
// PL/pgSQL body), so "up to the next semicolon" is exact, not a heuristic.
func a18ExtractStatement(src string, startIdx int) string {
	rel := strings.IndexByte(src[startIdx:], ';')
	if rel < 0 {
		return src[startIdx:]
	}
	return src[startIdx : startIdx+rel]
}

// a18Command returns the policy's command (defaulting to ALL, Postgres's
// own default when FOR is omitted).
func a18Command(stmt string) string {
	if m := a18ForCommandPattern.FindStringSubmatch(stmt); m != nil {
		return strings.ToUpper(m[1])
	}
	return "ALL"
}

// a18HasTenantNullArm reports whether stmt contains the K1-1 bare
// tenant_id-IS-NULL idiom.
func a18HasTenantNullArm(stmt string) bool {
	return a18BareTenantNullArm.MatchString(stmt)
}

// a18IsGuarded reports whether stmt's NULL arm is closed by a recognized
// guard: a platform-admin GUC requirement, a platform-service GUC
// requirement, or a positive subject-tenant-equality requirement.
func a18IsGuarded(stmt string) bool {
	return a18PlatformAdminGuard.MatchString(stmt) ||
		a18PlatformServiceGuard.MatchString(stmt) ||
		a18SubjectTenantPositiveGuard.MatchString(stmt)
}

// a18Violation is one flagged effective policy.
type a18Violation struct {
	table   string
	name    string
	command string
}

func (v a18Violation) String() string {
	return v.table + "." + v.name + " (" + v.command + ")"
}

// a18Replay replays every CREATE/DROP POLICY statement in src (one
// migration file's full text, already known to belong at position
// `version` in the overall ordering) against the running effective set.
// Callers must invoke this for each file in ascending version order.
func a18Replay(effective map[string]map[string]a18Policy, src string) {
	type event struct {
		pos    int
		create bool
	}
	var events []event
	for _, loc := range a18CreatePattern.FindAllStringIndex(src, -1) {
		events = append(events, event{pos: loc[0], create: true})
	}
	for _, loc := range a18DropPattern.FindAllStringIndex(src, -1) {
		events = append(events, event{pos: loc[0], create: false})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].pos < events[j].pos })

	for _, ev := range events {
		if ev.create {
			m := a18CreatePattern.FindStringSubmatch(src[ev.pos:])
			if m == nil {
				continue
			}
			name, table := m[1], m[2]
			stmt := a18ExtractStatement(src, ev.pos)
			pol := a18Policy{
				table:       table,
				name:        name,
				restrictive: regexp.MustCompile(`(?i)\bAS\s+RESTRICTIVE\b`).MatchString(stmt),
				command:     a18Command(stmt),
				body:        stmt,
			}
			if effective[table] == nil {
				effective[table] = map[string]a18Policy{}
			}
			effective[table][name] = pol
		} else {
			m := a18DropPattern.FindStringSubmatch(src[ev.pos:])
			if m == nil {
				continue
			}
			name, table := m[1], m[2]
			if effective[table] != nil {
				delete(effective[table], name)
			}
		}
	}
}

// a18SortedMigrationFiles returns every "<version>_....up.sql" file under
// dir, sorted by numeric version ascending - the exact replay order A-18
// requires.
func a18SortedMigrationFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	type vf struct {
		version int64
		path    string
	}
	var files []vf
	pat := regexp.MustCompile(`^(\d+)_.+\.up\.sql$`)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := pat.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			continue
		}
		files = append(files, vf{version: v, path: filepath.Join(dir, e.Name())})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	out := make([]string, len(files))
	for i, f := range files {
		out[i] = f.path
	}
	if len(out) == 0 {
		t.Fatal("no migration files found - the walk is broken")
	}
	return out
}

// a18ComputeViolations replays every migration file in dir and returns
// every effective violation.
func a18ComputeViolations(t *testing.T, dir string) []a18Violation {
	t.Helper()
	effective := map[string]map[string]a18Policy{}
	for _, path := range a18SortedMigrationFiles(t, dir) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		a18Replay(effective, string(src))
	}

	var violations []a18Violation
	for table, policies := range effective {
		if a18FencedTables[table] {
			continue
		}
		for name, pol := range policies {
			if pol.restrictive {
				continue // a fence itself, not an exposure
			}
			if pol.command == "SELECT" {
				continue // read-only reference data is accepted, §6.2
			}
			if !a18HasTenantNullArm(pol.body) {
				continue
			}
			if a18IsGuarded(pol.body) {
				continue
			}
			violations = append(violations, a18Violation{table: table, name: name, command: pol.command})
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].String() < violations[j].String() })
	return violations
}

// a18RepoRoot mirrors every other static guard's own go.mod-search helper.
func a18RepoRoot(t *testing.T) string {
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

// TestA18_NoUnfencedNullArmTable is the real guard: replays the actual
// repository migrations and fails if any non-SELECT, non-restrictive,
// non-fenced-table policy has an unguarded NULL-tenant arm.
func TestA18_NoUnfencedNullArmTable(t *testing.T) {
	root := a18RepoRoot(t)
	violations := a18ComputeViolations(t, filepath.Join(root, "migrations"))
	if len(violations) > 0 {
		var lines []string
		for _, v := range violations {
			lines = append(lines, v.String())
		}
		t.Fatalf("A-18: unfenced NULL-tenant-arm polic(y/ies) found - add a migration 0112-style restrictive fence or a recognized guard:\n  %s",
			strings.Join(lines, "\n  "))
	}
}

// TestA18_DynamicPolicyGenerationIsRestrictiveOnly pins the one disclosed
// scan limitation named in this file's own doc comment: every
// dynamically-generated (EXECUTE format(...)) CREATE POLICY in this
// codebase must be a RESTRICTIVE fence, never a permissive NULL-arm
// exposure the lexical scanner cannot see at all.
func TestA18_DynamicPolicyGenerationIsRestrictiveOnly(t *testing.T) {
	root := a18RepoRoot(t)
	dynamicCreate := regexp.MustCompile(`(?i)'CREATE POLICY\s+\S+\s+ON\s+%I`)
	files := a18SortedMigrationFiles(t, filepath.Join(root, "migrations"))
	found := 0
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range dynamicCreate.FindAllStringIndex(string(src), -1) {
			found++
			// The dynamically-built statement text runs from the match to
			// the closing quote before the trailing ", t)" - a fixed
			// enough shape in this codebase's own single generator
			// (migration 0112) to just check the same line contains
			// "RESTRICTIVE".
			lineStart := strings.LastIndexByte(string(src)[:loc[0]], '\n') + 1
			lineEnd := strings.IndexByte(string(src)[loc[0]:], '\n')
			if lineEnd < 0 {
				lineEnd = len(src) - loc[0]
			}
			line := string(src)[lineStart : loc[0]+lineEnd]
			if !regexp.MustCompile(`(?i)RESTRICTIVE`).MatchString(line) {
				t.Errorf("%s: dynamically-generated policy is not RESTRICTIVE (lexically invisible to TestA18_NoUnfencedNullArmTable): %s", path, strings.TrimSpace(line))
			}
		}
	}
	if found == 0 {
		t.Fatal("expected to find at least one dynamically-generated CREATE POLICY (migration 0112's own DO loop) - the scan is broken or the source moved")
	}
}

// --- planted-violation proofs (required: "include planted-violation cases") ---

// a18Scenario runs a synthetic sequence of migration file contents (in
// order) through a18Replay and returns the resulting violations.
func a18Scenario(files ...string) []a18Violation {
	effective := map[string]map[string]a18Policy{}
	for _, f := range files {
		a18Replay(effective, f)
	}
	var violations []a18Violation
	for table, policies := range effective {
		if a18FencedTables[table] {
			continue
		}
		for name, pol := range policies {
			if pol.restrictive || pol.command == "SELECT" || !a18HasTenantNullArm(pol.body) || a18IsGuarded(pol.body) {
				continue
			}
			violations = append(violations, a18Violation{table: table, name: name, command: pol.command})
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].String() < violations[j].String() })
	return violations
}

// TestA18_Plant_UnguardedNullArm_Flagged: the base planted violation - a
// FOR ALL policy on a brand-new, non-fenced table with a bare
// tenant_id IS NULL arm and no guard at all must be flagged.
func TestA18_Plant_UnguardedNullArm_Flagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY evil_wide_open ON new_widget_table
    FOR ALL
    USING (
        tenant_id IS NULL
        AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL
    );
`)
	if len(v) != 1 || v[0].table != "new_widget_table" || v[0].name != "evil_wide_open" {
		t.Fatalf("expected exactly one flagged violation on new_widget_table.evil_wide_open, got %v", v)
	}
}

// TestA18_Plant_PlatformAdminGuard_NotFlagged: the same shape, guarded by
// a platform-admin GUC requirement - not flagged.
func TestA18_Plant_PlatformAdminGuard_NotFlagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY platform_only ON new_widget_table
    FOR ALL
    USING (
        tenant_id IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
    );
`)
	if len(v) != 0 {
		t.Fatalf("expected no violations (platform_admin_principal_id guard present), got %v", v)
	}
}

// TestA18_Plant_PlatformServiceGuard_NotFlagged: guarded by a
// platform-service GUC requirement instead - not flagged.
func TestA18_Plant_PlatformServiceGuard_NotFlagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY service_only ON new_widget_table
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') = 'some_worker'
    );
`)
	if len(v) != 0 {
		t.Fatalf("expected no violations (platform_service_id guard present), got %v", v)
	}
}

// TestA18_Plant_SubjectTenantPositiveEquality_NotFlagged: the
// subject_tenant_read / alerts_subject_tenant_raise shape - a bare
// "tenant_id IS NULL" combined with a POSITIVE subject_tenant_id equality
// is not a free NULL arm (it requires a real tenant to match) and must
// not be flagged, on an INSERT (non-SELECT) policy specifically, since
// SELECT is already exempted separately.
func TestA18_Plant_SubjectTenantPositiveEquality_NotFlagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY subject_tenant_raise ON new_widget_table
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL
        AND subject_tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid
    );
`)
	if len(v) != 0 {
		t.Fatalf("expected no violations (positive subject_tenant_id equality guard present), got %v", v)
	}
}

// TestA18_Plant_SelectOnly_NotFlagged: a bare NULL arm on a SELECT-only
// policy (read-only reference data, accepted per §6.2) is not flagged.
func TestA18_Plant_SelectOnly_NotFlagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY read_only_reference ON new_widget_table
    FOR SELECT
    USING (tenant_id IS NULL);
`)
	if len(v) != 0 {
		t.Fatalf("expected no violations (SELECT-only is accepted), got %v", v)
	}
}

// TestA18_Plant_FencedTable_NotFlagged: the exact same unguarded shape on
// one of the seven ADR 0099 §6.4 tables is not flagged (A-17 is what
// proves those seven actually carry the restrictive fence).
func TestA18_Plant_FencedTable_NotFlagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY dual_scope_isolation ON staff_users
    FOR ALL
    USING (
        (tenant_id IS NOT NULL AND tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid)
        OR (tenant_id IS NULL AND NULLIF(current_setting('app.tenant_id', true), '') IS NULL)
    );
`)
	if len(v) != 0 {
		t.Fatalf("expected no violations (staff_users is one of the seven fenced tables), got %v", v)
	}
}

// TestA18_Plant_RestrictivePolicy_NotFlagged: a RESTRICTIVE policy is a
// fence, never an exposure, even with the same textual NULL arm.
func TestA18_Plant_RestrictivePolicy_NotFlagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY some_fence ON new_widget_table AS RESTRICTIVE FOR ALL
    USING (tenant_id IS NULL);
`)
	if len(v) != 0 {
		t.Fatalf("expected no violations (RESTRICTIVE policies are fences, not exposures), got %v", v)
	}
}

// TestA18_Plant_DropThenNoRecreate_NotFlagged: a policy created then
// dropped, with nothing recreated in its place, must not appear in the
// effective set at all.
func TestA18_Plant_DropThenNoRecreate_NotFlagged(t *testing.T) {
	v := a18Scenario(
		`CREATE POLICY temp_hole ON new_widget_table FOR ALL USING (tenant_id IS NULL);`,
		`DROP POLICY temp_hole ON new_widget_table;`,
	)
	if len(v) != 0 {
		t.Fatalf("expected no violations (policy was dropped with nothing recreated), got %v", v)
	}
}

// TestA18_Plant_DropThenRecreateNarrower_NotFlagged: the real 0043/0049
// precedent ADR 0099 §6.2 cites - an exposed policy is later DROPped and
// a narrower (guarded) one is CREATEd under the same name. The replay
// must reflect the LATEST version only.
func TestA18_Plant_DropThenRecreateNarrower_NotFlagged(t *testing.T) {
	v := a18Scenario(
		`CREATE POLICY narrowed_over_time ON new_widget_table FOR ALL USING (tenant_id IS NULL);`,
		`DROP POLICY narrowed_over_time ON new_widget_table;
CREATE POLICY narrowed_over_time ON new_widget_table
    FOR ALL
    USING (
        tenant_id IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
    );`,
	)
	if len(v) != 0 {
		t.Fatalf("expected no violations (the later, narrower recreation is what's effective), got %v", v)
	}
}

// TestA18_Plant_DropThenRecreateStillOpen_Flagged: the mirror image - a
// GUARDED policy dropped and recreated WITHOUT its guard must be caught
// (proves the replay tracks the LATEST state, not just "was it ever
// guarded").
func TestA18_Plant_DropThenRecreateStillOpen_Flagged(t *testing.T) {
	v := a18Scenario(
		`CREATE POLICY regressed ON new_widget_table
    FOR ALL
    USING (
        tenant_id IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NOT NULL
    );`,
		`DROP POLICY regressed ON new_widget_table;
CREATE POLICY regressed ON new_widget_table FOR ALL USING (tenant_id IS NULL);`,
	)
	if len(v) != 1 || v[0].name != "regressed" {
		t.Fatalf("expected exactly one flagged violation (the guard was regressed away), got %v", v)
	}
}
