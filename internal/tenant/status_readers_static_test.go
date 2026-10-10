package tenant

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ADR 0112 section 3.3 / slice 1 "T2": every reader of tenants.status or brands.status
// is inventoried (docs/security/launch-status-readers.md) and classified as
// `= 'active'` / `<> 'active'` (safe for the new value pending_launch: a pending_launch
// subject is refused exactly like a suspended one) or an explicit value list (reviewed).
// Two static pins keep that inventory honest:
//
//  1. no code compares either column with the literal 'suspended' or 'closed' outside a
//     reviewed allow-list (LF7-LF9 of the ledger-finance review, ADR 0112 section 14.1);
//  2. the set of non-test Go files that read the columns by SQL is exactly the reviewed set,
//     so a NEW reader fails this test until it is added to the inventory and classified.
//
// These are heuristics over source text (the same family as no_trigger_disable_test.go):
// they catch the literal forms the codebase actually uses, not every conceivable spelling.

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

// literalCompare matches a comparison of a status expression with 'suspended' / 'closed'
// in SQL (both quote styles of the form used here).
var literalCompare = regexp.MustCompile(
	`(?i)(tenant_status|brand_status|\bt\.status|\bb\.status|\bv_status|\bv_[a-z]*status|tenants\.status|brands\.status|\bNEW\.status|\bOLD\.status|\bstatus)\s*` +
		`(=|<>|!=|IS\s+DISTINCT\s+FROM|IS\s+NOT\s+DISTINCT\s+FROM)\s*'(suspended|closed)'`)

// goLiteralCompare matches the Go spelling for a tenant / brand status variable.
var goLiteralCompare = regexp.MustCompile(
	`(?i)\b(tenantStatus|brandStatus|tenant\.Status|brand\.Status|t\.Status|b\.Status)\s*(==|!=)\s*"(suspended|closed)"`)

var (
	rawString        = regexp.MustCompile("(?s)`[^`]*`")
	tenantBrandTable = regexp.MustCompile(`(?i)\b(from|join|update)\s+(public\.)?(tenants|brands)\b`)
	goSQLCompare     = regexp.MustCompile(`(?i)\bstatus\s*(=|<>|!=|in\s*\()\s*'?(suspended|closed)\b|\bstatus\s+is\s+(not\s+)?distinct\s+from\s+'(suspended|closed)'`)
)

// allowedLiteralCompares is the reviewed allow-list, keyed by "<repo-relative file>:<trimmed line>".
// Anything not listed fails the pin. Each entry says why it is safe for pending_launch.
var allowedLiteralCompares = map[string]string{
	// LF7 (ledger-finance review of ADR 0112, section 14.1): migration 0115's explicit
	// 'closed' comparisons. pending_launch is treated as NOT closed there; tenant-scope
	// force-resolution therefore stays allowed for a pending tenant, which is harmless
	// because a pending tenant has no payments.
	"migrations/0115_payment_force_resolution.up.sql:AND (tenant_status IS DISTINCT FROM 'closed' OR r.requested_by_scope = 'platform_acting')": "LF7",
	"migrations/0115_payment_force_resolution.up.sql:AND (tenant_status IS DISTINCT FROM 'closed' OR a.decided_by_scope = 'platform_acting')":   "LF7",
	"migrations/0115_payment_force_resolution.up.sql:IF v_policy.tenant_status = 'closed' AND v_actor.scope <> 'platform_acting' THEN":          "LF7",
	// 0125 (L-1): the M4 payout-unbound resolution restates the same three 0115 comparisons
	// (closed tenants: platform_acting only). Same LF7 reasoning: pending_launch is not 'closed'.
	"migrations/0125_payout_unbound_resolution_m4.up.sql:AND (tenant_status IS DISTINCT FROM 'closed' OR r.requested_by_scope = 'platform_acting')": "LF7 (0125 restatement)",
	"migrations/0125_payout_unbound_resolution_m4.up.sql:AND (tenant_status IS DISTINCT FROM 'closed' OR a.decided_by_scope = 'platform_acting')":   "LF7 (0125 restatement)",
	"migrations/0125_payout_unbound_resolution_m4.up.sql:IF v_policy.tenant_status = 'closed' AND v_actor.scope <> 'platform_acting' THEN":          "LF7 (0125 restatement)",
	// 0121 item B: the closure gate fires for a transition INTO 'closed'. pending_launch ->
	// closed is such a transition and is counted like any other (a pending tenant has no
	// bets, so the count is 0 and the closure proceeds).
	"migrations/0121_gameplay_stake_return_and_closure_guard.up.sql:IF NEW.status = 'closed' THEN": "0121 closure gate (transition INTO closed)",
	// 0128 (this slice): the closed-is-terminal rule of the status guard.
	"migrations/0128_launch_authorisation_status_governance.up.sql:IF OLD.status = 'closed' THEN": "0128 closed is terminal",
	// 0129 (ADR 0112 slice 2, LF1): CREATE OR REPLACE of the same 0128 guard with the
	// per-brand gameplay lock added; the closed-is-terminal line is 0128's, unchanged.
	"migrations/0129_brand_gameplay_gate.up.sql:IF OLD.status = 'closed' THEN": "0129 restates 0128 closed is terminal",
}

func TestStatusLiteralPin_NoSuspendedOrClosedComparisonOutsideAllowList(t *testing.T) {
	root := repoRoot(t)
	seenAllowed := map[string]bool{}
	check := func(path string, re *regexp.Regexp, subjectOnly func(line string) bool) {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		for i, line := range strings.Split(string(b), "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "--") || strings.HasPrefix(trim, "//") {
				continue
			}
			if !re.MatchString(trim) || (subjectOnly != nil && !subjectOnly(trim)) {
				continue
			}
			key := rel + ":" + trim
			if _, ok := allowedLiteralCompares[key]; ok {
				seenAllowed[key] = true
				continue
			}
			t.Errorf("%s:%d compares a tenant/brand status with 'suspended'/'closed' outside the reviewed allow-list (ADR 0112 LF7-LF9; add it to docs/security/launch-status-readers.md and allowedLiteralCompares only after review): %s", rel, i+1, trim)
		}
	}

	// Migrations: only statements that concern tenant / brand status. A bare `status`
	// matches every table, so for the bare form require a tenants/brands context in the
	// same file section by restricting to the files that define tenant/brand status logic.
	migs, _ := filepath.Glob(filepath.Join(root, "migrations", "*.up.sql"))
	if len(migs) == 0 {
		t.Fatal("no migrations found; the pin would be vacuous")
	}
	tenantBrandLine := func(line string) bool {
		l := strings.ToLower(line)
		// Exclude other tables' own status columns: only the named tenant / brand forms
		// and OLD/NEW.status inside the subject guards qualify.
		return strings.Contains(l, "tenant_status") || strings.Contains(l, "brand_status") ||
			strings.Contains(l, "t.status") || strings.Contains(l, "b.status") ||
			strings.Contains(l, "tenants.status") || strings.Contains(l, "brands.status") ||
			strings.Contains(l, "new.status") || strings.Contains(l, "old.status") || strings.Contains(l, "v_status")
	}
	for _, m := range migs {
		// L-1: EVERY migration is scanned (the line filter below limits matches to tenant /
		// brand status expressions), not a hand-picked file list.
		check(m, literalCompare, tenantBrandLine)
	}

	// Non-test Go.
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			check(path, goLiteralCompare, nil)
			// SQL text inside Go raw strings that reads tenants / brands and compares a status
			// with 'suspended' / 'closed' (the statement spans lines, so scan the literal whole).
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, lit := range rawString.FindAllString(string(b), -1) {
				if tenantBrandTable.MatchString(lit) && goSQLCompare.MatchString(lit) {
					t.Errorf("%s: SQL reading tenants/brands compares a status with 'suspended'/'closed' outside the reviewed allow-list (ADR 0112 LF7-LF9): %.120s", rel, strings.Join(strings.Fields(lit), " "))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for key := range allowedLiteralCompares {
		if !seenAllowed[key] {
			t.Errorf("allow-list entry no longer matches any line (remove it or fix the key): %s", key)
		}
	}
}

// reviewedStatusReaderFiles is the exact set of non-test Go files that read tenants.status or
// brands.status by SQL text (docs/security/launch-status-readers.md, section "Go readers").
// Each is classified there; every one tests = 'active' / <> 'active' / an explicit reviewed list.
var reviewedStatusReaderFiles = []string{
	"internal/bonus/schedulers.go",
	"internal/identity/brand.go",
	"internal/identity/player_account.go",
	"internal/identity/tenant.go",
	"internal/kyc/enforcement_dormancy.go",
	"internal/kyc/outbox_worker.go",
	"internal/payments/sweeper_resolution_only.go",
	"internal/reconciliation/scheduler.go",
	"internal/rg/enumeration_sweep.go",
	"internal/tenant/gameplay_gate.go",
	"internal/tenant/payment_initiation_gate.go",
	"internal/tenant/status.go",
}

// A raw string is a status reader when it names tenants / brands as a table (FROM / JOIN / UPDATE /
// INTO, in any order relative to the status column, so `SELECT t.status ... JOIN tenants t` counts:
// code review C-1) and mentions a status column (bare or alias-qualified).
var (
	statusReaderTable = regexp.MustCompile(`(?i)\b(from|join|update|into)\s+(public\.)?(tenants|brands)\b`)
	statusReaderCol   = regexp.MustCompile(`(?i)\b(\w+\.)?status\b`)
)

func isStatusReaderSource(src string) bool {
	for _, lit := range rawString.FindAllString(src, -1) {
		if statusReaderTable.MatchString(lit) && statusReaderCol.MatchString(lit) {
			return true
		}
	}
	return false
}

// anyStatusLiteral matches a quoted 'suspended' / 'closed' in either quote style: in the REVIEWED
// reader files no code line may mention them at all (a gate is `= 'active'` / `<> 'active'`), whatever
// the variable name, the operator (==, !=, IN, NOT IN, switch/case) or the quote style.
var anyStatusLiteral = regexp.MustCompile(`["'](suspended|closed)["']`)

// statusLiteralLines returns the non-comment lines of a Go source that carry such a literal.
func statusLiteralLines(src string) []string {
	var out []string
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "//") || strings.HasPrefix(trim, "--") || strings.HasPrefix(trim, "*") {
			continue
		}
		if anyStatusLiteral.MatchString(trim) {
			out = append(out, trim)
		}
	}
	return out
}

// reviewedReaderLiteralAllow lists the only code lines in reviewed reader files that carry a
// 'suspended' / 'closed' literal, with the reason each is not a status gate.
var reviewedReaderLiteralAllow = map[string]string{
	"internal/tenant/status.go:case \"active\", \"suspended\", \"closed\":":                                   "ChangeStatus input validation (not a gate)",
	"internal/identity/player_account.go:PlayerStatusSuspended           PlayerAccountStatus = \"suspended\"": "player_accounts status constant, not tenants/brands",
	"internal/identity/player_account.go:PlayerStatusClosed              PlayerAccountStatus = \"closed\"":    "player_accounts status constant, not tenants/brands",
}

func TestStatusLiteralPin_ReviewedReaderFilesCarryNoSuspendedOrClosedLiteral(t *testing.T) {
	root := repoRoot(t)
	seen := map[string]bool{}
	for _, rel := range reviewedStatusReaderFiles {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range statusLiteralLines(string(b)) {
			key := rel + ":" + line
			if _, ok := reviewedReaderLiteralAllow[key]; ok {
				seen[key] = true
				continue
			}
			t.Errorf("%s carries a 'suspended'/'closed' literal in code (ADR 0112 LF7-LF9: gates test = 'active' / <> 'active'): %s", rel, line)
		}
	}
	for key := range reviewedReaderLiteralAllow {
		if !seen[key] {
			t.Errorf("literal allow-list entry no longer matches (remove or fix): %s", key)
		}
	}
}

// Negative tests (code review C-1): the two violating changes the reviewer applied, and the reader
// shape the old inventory regex missed, are now detected.
func TestStatusPins_NegativeCases_ReviewerViolationsAreDetected(t *testing.T) {
	for name, src := range map[string]string{
		"gameplay_gate status == suspended || closed":  "if status == \"suspended\" || status == \"closed\" {\n return err\n}",
		"payment gate NOT IN list":                     "q := `SELECT 1 FROM tenants WHERE id = $1 AND status NOT IN ('suspended', 'closed')`",
		"IN list with other first entry":               "q := `SELECT 1 FROM brands WHERE status IN ('pending_launch', 'closed')`",
		"switch case":                                  "switch s {\ncase \"closed\":\n}",
		"single-quoted compare in SQL under any alias": "WHERE x.status = 'suspended'",
	} {
		if len(statusLiteralLines(src)) == 0 {
			t.Errorf("%s: not detected by the literal pin", name)
		}
	}
	if len(statusLiteralLines("// a suspended tenant is refused (\"closed\" is fine in a comment)\nx := 1")) != 0 {
		t.Error("comments must not trip the pin")
	}
	for name, src := range map[string]string{
		"SELECT t.status ... JOIN tenants": "q := `SELECT b.status, t.status FROM brands b JOIN tenants t ON t.id = b.tenant_id`",
		"status before FROM tenants":       "q := `SELECT status FROM tenants WHERE id = $1`",
		"join only, status in WHERE":       "q := `SELECT 1 FROM x JOIN tenants t ON t.id = x.tenant_id WHERE t.status = 'active'`",
		"update status":                    "q := `UPDATE brands SET status = $2 WHERE id = $1`",
	} {
		if !isStatusReaderSource(src) {
			t.Errorf("%s: not detected by the reader-inventory pin", name)
		}
	}
	if isStatusReaderSource("q := `SELECT status FROM player_accounts`") {
		t.Error("another table's status must not count as a tenants/brands reader")
	}
}

func TestStatusReaderInventory_GoReadersAreExactlyTheReviewedSet(t *testing.T) {
	root := repoRoot(t)
	var got []string
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			if strings.HasPrefix(rel, "internal/testsupport/") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if isStatusReaderSource(string(b)) {
				got = append(got, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(got)
	want := append([]string(nil), reviewedStatusReaderFiles...)
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the set of Go files reading tenants.status / brands.status changed.\n got:  %v\n want: %v\n"+
			"Classify the new reader (= 'active' / <> 'active' / explicit list) in docs/security/launch-status-readers.md and update reviewedStatusReaderFiles.", got, want)
	}
}
