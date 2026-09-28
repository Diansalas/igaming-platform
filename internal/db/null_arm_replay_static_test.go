// A-18 (ADR 0099 §6.2/§6.9 TM-3, security K1-1 future-proofing condition;
// hardened by security's K1 re-check hard gate K2-G1): a permanent static
// guard that replays every CREATE POLICY / DROP POLICY statement across
// migrations/*.up.sql, in migration-version order, to compute the
// EFFECTIVE policy set, then flags any effective PERMISSIVE policy whose
// predicate is reachable by an acting session without a positive guard:
//
//   - an EXPOSURE is either a bare `tenant_id IS NULL` arm on the table's
//     own column (the K1-1 shape) or a literal `USING (true)` /
//     `WITH CHECK (true)` arm (K2-G1 false negative (b));
//   - a GUARD must be POSITIVE (K2-G1 false negative (a)): a named
//     non-tenant GUC required to be NOT NULL or equal to something
//     (`NULLIF(current_setting('app.platform_admin_principal_id', true),
//     ”) IS NOT NULL`, `... = 'alert_dispatcher'`, `col = NULLIF(
//     current_setting('app.player_account_id', true), ”)::uuid`), a
//     positive `subject_tenant_id =` equality, or one of the acting-family
//     functions (financial_acting_session_valid / financial_acting_gucs_exact).
//     A NEGATIVE mention (`... platform_admin_principal_id ... IS NULL`)
//     is NOT a guard - that is exactly what an acting session satisfies;
//   - a SELECT exposure is accepted ONLY on the closed ADR 0099 §6.2
//     reference-table allowlist (a18SelectAllowlist) - K2-G1 false
//     negative (c): every other SELECT NULL/true arm is flagged too;
//   - any non-SELECT exposure is always flagged;
//   - the seven ADR 0099 §6.4 tables migration 0112 restrictively fences
//     (A-17 proves their fence) are skipped, as are tables carrying a
//     migration-0113 acting restrictive SELECT fence
//     (a18AdditionallyFencedTables).
//
// This is a lexical scanner over the migration SQL text, not a SQL parser
// (the same stated trade-off as internal/txscope's INV-IO-1(c) test and
// internal/ledger's lockorder_static_test.go). Its known limit - a positive
// guard that sits in a different OR branch from the NULL arm is still
// counted - is covered by the DYNAMIC complement
// (acting_visibility_dynamic_integration_test.go, K2-G1): a real, valid
// acting session counts the rows it can see on every public table and
// compares against the same allowlist. Planted cases below prove each of
// security's three false negatives is now caught, plus every negative
// (not-flagged) shape.
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

// a18AdditionallyFencedTables are tables whose SELECT NULL arm was found
// by the hardened A-18 (K2-G1) and closed by migration 0113 with an
// AS RESTRICTIVE acting SELECT fence (ADR 0100 §20 implementation
// record): 0045's asset_operation_eligibility.tenant_and_platform_read and
// 0043's open_bet_self_exclusion_policies.tenant_and_platform_read both
// admit NULL-tenant rows to any player-unset session, which an acting
// session is. ADR 0099 §6.2 listed both as "not exposed" on the strength
// of their WRITE policies only. Only their SELECT exposure is fenced, so
// only SELECT exposures are skipped for these tables; a non-SELECT
// exposure on them is still flagged.
var a18AdditionallyFencedTables = map[string]bool{
	"asset_operation_eligibility":      true,
	"open_bet_self_exclusion_policies": true,
}

// a18SelectAllowlist is the closed ADR 0099 §6.2 "read-only reference data
// an acting session can still read (accepted)" list, plus the family-R
// reference tables migrations 0112 (K1) and 0113 (K2) create themselves
// (ADR 0099 §10.2, ADR 0100 §10.1): platform reference data, no PII, no
// credentials. A SELECT NULL/true arm on any table NOT listed here is
// flagged (K2-G1 (c)). The dynamic complement uses this same list.
var a18SelectAllowlist = map[string]bool{
	"assets":                          true,
	"brands":                          true,
	"casino_games":                    true,
	"jurisdictions":                   true,
	"jurisdiction_precedence_configs": true,
	"kyc_enforcement_policies":        true,
	"platform_operations":             true,
	"platform_products":               true,
	"sb_sports":                       true,
	"sb_competitions":                 true,
	"sb_events":                       true,
	"sb_markets":                      true,
	"sb_selections":                   true,
	"sb_jurisdiction_restrictions":    true,
	"alert_kinds":                     true,
	"tenants":                         true,
	"licences":                        true,
	"licence_country_ceilings":        true,
	// K1 (0112) family-R reference tables.
	"financial_capability_catalogue":   true,
	"financial_governance_permissions": true,
	"financial_capability_settings":    true,
	// K2 (0113) family-R reference tables.
	"financial_control_classifications": true,
	"ledger_adjustment_reason_codes":    true,
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
	// column - i.e. NOT prefixed by a dot, quote or word character (which
	// would instead be something like "app.tenant_id ... IS NULL" or
	// "subject_tenant_id IS NULL", a different idiom).
	a18BareTenantNullArm = regexp.MustCompile(`(?i)(^|[^.\w'])tenant_id\s+IS\s+NULL`)
	// a18TrueArm matches a literal always-true predicate arm (K2-G1 (b)).
	a18TrueArm = regexp.MustCompile(`(?i)\b(USING|WITH\s+CHECK)\s*\(\s*true\s*\)`)
	// a18PositiveGUCGuard matches a non-tenant GUC required to be present:
	// "NULLIF(current_setting('app.X', true), '')[)][::type][)] IS NOT
	// NULL" or "... = <something>". app.tenant_id is deliberately NOT a
	// guard GUC: it is the tenant arm itself, not a second requirement.
	a18PositiveGUCGuard = regexp.MustCompile(`(?i)NULLIF\(\s*current_setting\(\s*'app\.(platform_admin_principal_id|platform_service_id|principal_id|player_account_id|session_lookup_hash|session_internal_op_id|credential_token_lookup_hash)'\s*,\s*true\s*\)\s*,\s*''\s*\)\s*\)?\s*(::\s*\w+)?\s*\)?\s*(IS\s+NOT\s+NULL|=)`)
	// a18PositiveGUCEqualityRHS matches "col = NULLIF(current_setting(
	// 'app.X'...", the same positive requirement with the GUC on the right.
	a18PositiveGUCEqualityRHS = regexp.MustCompile(`(?i)=\s*\(?\s*NULLIF\(\s*current_setting\(\s*'app\.(platform_admin_principal_id|platform_service_id|principal_id|player_account_id|session_lookup_hash|session_internal_op_id|credential_token_lookup_hash)'`)
	// a18SubjectTenantPositiveGuard matches the subject_tenant_read/
	// alerts_subject_tenant_raise shape: subject_tenant_id required to
	// equal a real value (never merely "IS NULL").
	a18SubjectTenantPositiveGuard = regexp.MustCompile(`(?i)subject_tenant_id\s*=`)
	// a18ActingFamilyGuard matches a deliberate acting-family permissive
	// policy (ADR 0099 §6.3/§6.5): these ARE the acting surface, bounded
	// by the dynamic complement, not an accidental NULL arm.
	a18ActingFamilyGuard = regexp.MustCompile(`(?i)financial_acting_session_valid\(\)|financial_acting_gucs_exact\(\)`)
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

// a18HasExposure reports whether stmt contains a K1-1 bare
// tenant_id-IS-NULL arm or a literal true arm.
func a18HasExposure(stmt string) bool {
	return a18BareTenantNullArm.MatchString(stmt) || a18TrueArm.MatchString(stmt)
}

// a18IsGuarded reports whether stmt carries a recognized POSITIVE guard.
func a18IsGuarded(stmt string) bool {
	return a18PositiveGUCGuard.MatchString(stmt) ||
		a18PositiveGUCEqualityRHS.MatchString(stmt) ||
		a18SubjectTenantPositiveGuard.MatchString(stmt) ||
		a18ActingFamilyGuard.MatchString(stmt)
}

// a18Flagged is the single classification rule both the real guard and
// the planted scenarios use.
func a18Flagged(pol a18Policy) bool { return a18FlaggedWith(pol, true) }

// a18FlaggedWith is a18Flagged with the 0113 additionally-fenced SELECT
// exemption switchable, so a test can prove those entries are real findings.
func a18FlaggedWith(pol a18Policy, exemptAdditional bool) bool {
	if a18FencedTables[pol.table] {
		return false
	}
	if pol.restrictive {
		return false // a fence itself, not an exposure
	}
	if !a18HasExposure(pol.body) || a18IsGuarded(pol.body) {
		return false
	}
	if pol.command == "SELECT" && (a18SelectAllowlist[pol.table] || (exemptAdditional && a18AdditionallyFencedTables[pol.table])) {
		return false
	}
	return true
}

// a18Collect applies a18Flagged to every effective policy, sorted.
func a18Collect(effective map[string]map[string]a18Policy) []a18Violation {
	var violations []a18Violation
	for _, policies := range effective {
		for _, pol := range policies {
			if a18Flagged(pol) {
				violations = append(violations, a18Violation{table: pol.table, name: pol.name, command: pol.command})
			}
		}
	}
	sort.Slice(violations, func(i, j int) bool { return violations[i].String() < violations[j].String() })
	return violations
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

	violations := a18Collect(effective)
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

// TestA18_AdditionallyFencedTablesAreRealFindings proves the two
// a18AdditionallyFencedTables entries are genuine findings of the hardened
// scanner (not stale allowlist padding): with their exemption switched
// off, replaying the real migrations flags EXACTLY their SELECT NULL arms
// and nothing else. Migration 0113's restrictive acting SELECT fence on
// both is what makes the exemption legitimate; the dynamic complement
// (TestK2G1_ActingSessionRowVisibilityMatchesAllowlist) proves the fence
// actually holds at runtime.
func TestA18_AdditionallyFencedTablesAreRealFindings(t *testing.T) {
	root := a18RepoRoot(t)
	effective := map[string]map[string]a18Policy{}
	for _, path := range a18SortedMigrationFiles(t, filepath.Join(root, "migrations")) {
		src, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		a18Replay(effective, string(src))
	}
	var got []string
	for _, policies := range effective {
		for _, pol := range policies {
			if a18FlaggedWith(pol, false) {
				got = append(got, pol.table+"."+pol.name+"("+pol.command+")")
			}
		}
	}
	sort.Strings(got)
	want := []string{
		"asset_operation_eligibility.tenant_and_platform_read(SELECT)",
		"open_bet_self_exclusion_policies.tenant_and_platform_read(SELECT)",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("expected exactly %v flagged without the 0113 exemption, got %v", want, got)
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
	return a18Collect(effective)
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

// TestA18_Plant_SelectOnlyOnAllowlistedReferenceTable_NotFlagged: a bare
// NULL arm (or a USING (true) arm) on a SELECT-only policy of a table on
// the closed ADR 0099 §6.2 reference allowlist is accepted.
func TestA18_Plant_SelectOnlyOnAllowlistedReferenceTable_NotFlagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY read_only_reference ON assets
    FOR SELECT
    USING (tenant_id IS NULL);
CREATE POLICY read_all ON alert_kinds FOR SELECT USING (true);
`)
	if len(v) != 0 {
		t.Fatalf("expected no violations (SELECT on an allowlisted reference table is accepted), got %v", v)
	}
}

// TestA18_K2G1a_NegativeIsNullMentionIsNotAGuard_Flagged is security's
// planted false negative (a): a policy whose only "guard" is a NEGATIVE
// mention of the platform GUC (`... IS NULL`) - exactly what an acting
// session satisfies - must be flagged. The old scanner accepted any
// textual mention of platform_admin_principal_id as a guard.
func TestA18_K2G1a_NegativeIsNullMentionIsNotAGuard_Flagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY negative_only ON new_widget_table
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '') IS NULL
        AND NULLIF(current_setting('app.platform_service_id', true), '') IS NULL
    );
`)
	if len(v) != 1 || v[0].name != "negative_only" {
		t.Fatalf("expected the negative-only 'guard' to be flagged, got %v", v)
	}
	// And the positive form of the very same GUC is still accepted.
	ok := a18Scenario(`
CREATE POLICY positive ON new_widget_table
    FOR INSERT
    WITH CHECK (
        tenant_id IS NULL
        AND NULLIF(current_setting('app.platform_admin_principal_id', true), '')::uuid IS NOT NULL
    );
`)
	if len(ok) != 0 {
		t.Fatalf("expected the positive guard to be accepted, got %v", ok)
	}
}

// TestA18_K2G1b_TrueArms_Flagged is security's planted false negative (b):
// `FOR ALL USING (true) WITH CHECK (true)` has no tenant_id text at all and
// was invisible to the old scanner. A lone WITH CHECK (true) INSERT is
// flagged too.
func TestA18_K2G1b_TrueArms_Flagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY wide_open ON new_widget_table FOR ALL USING (true) WITH CHECK (true);
CREATE POLICY insert_anything ON other_widget_table FOR INSERT WITH CHECK ( true );
`)
	if len(v) != 2 {
		t.Fatalf("expected both true-arm policies flagged, got %v", v)
	}
}

// TestA18_K2G1c_SelectNullArmOutsideAllowlist_Flagged is security's
// planted false negative (c): the old scanner blanket-exempted every SELECT
// policy. A SELECT NULL arm (or USING (true)) on a table NOT on the ADR
// 0099 §6.2 allowlist must be flagged.
func TestA18_K2G1c_SelectNullArmOutsideAllowlist_Flagged(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY read_null_rows ON new_widget_table
    FOR SELECT
    USING (tenant_id IS NULL OR tenant_id = NULLIF(current_setting('app.tenant_id', true), '')::uuid);
CREATE POLICY read_everything ON other_widget_table FOR SELECT USING (true);
`)
	if len(v) != 2 {
		t.Fatalf("expected both SELECT exposures outside the allowlist to be flagged, got %v", v)
	}
}

// TestA18_AdditionallyFencedTables_SelectOnlyExempt pins that the two
// tables migration 0113 fences (asset_operation_eligibility,
// open_bet_self_exclusion_policies) are exempt for SELECT only - a
// non-SELECT exposure on them is still flagged.
func TestA18_AdditionallyFencedTables_SelectOnlyExempt(t *testing.T) {
	v := a18Scenario(`
CREATE POLICY r ON asset_operation_eligibility FOR SELECT USING (tenant_id IS NULL);
CREATE POLICY w ON asset_operation_eligibility FOR INSERT WITH CHECK (tenant_id IS NULL);
`)
	if len(v) != 1 || v[0].name != "w" {
		t.Fatalf("expected only the INSERT exposure flagged, got %v", v)
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
