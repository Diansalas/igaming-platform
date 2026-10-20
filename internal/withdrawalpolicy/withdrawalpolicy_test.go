package withdrawalpolicy

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

func TestIsWidening_Matrix(t *testing.T) {
	cur := func(actions ...string) []StageEffect {
		var out []StageEffect
		for i, a := range actions {
			out = append(out, StageEffect{Stage: Stages[i], Action: a, Source: SourceConfigured})
		}
		return out
	}
	def := func(actions ...string) []StageEffect {
		out := cur(actions...)
		for i := range out {
			out[i].Source = SourceDefault
		}
		return out
	}
	// lic17 F-2 (migration 0184): the FIRST configured allow over the interim default is widening (four-eyes); a tightening
	// over the default stays single actor.
	if !IsWidening(ActionAllow, def("allow", "allow", "allow")) {
		t.Fatal("a first configured allow over the default must be widening")
	}
	if IsWidening(ActionBlock, def("allow", "allow", "allow")) || IsWidening(ActionHoldForReview, def("allow", "allow", "allow")) {
		t.Fatal("a tightening over the default stays single actor")
	}
	mixed := cur("allow", "allow", "allow")
	mixed[2].Source = SourceDefault
	if !IsWidening(ActionAllow, mixed) {
		t.Fatal("a default at ANY stage makes an allow widening")
	}
	for _, c := range []struct {
		next    string
		current []StageEffect
		want    bool
	}{
		{ActionBlock, cur("allow", "allow", "allow"), false},         // tightening: single actor
		{ActionHoldForReview, cur("allow", "allow", "allow"), false}, // tightening
		{ActionAllow, cur("allow", "allow", "allow"), false},         // re-pinning a CONFIGURED allow: single actor
		{ActionAllow, cur("block", "block", "block"), true},          // loosening: two approvers
		{ActionHoldForReview, cur("block", "block", "block"), true},  // loosening
		{ActionAllow, cur("hold_for_review", "hold_for_review", "hold_for_review"), true},
		{ActionBlock, cur("block", "block", "block"), false},
		{ActionHoldForReview, cur("allow", "block", "allow"), true}, // looser at ANY stage widens
	} {
		if got := IsWidening(c.next, c.current); got != c.want {
			t.Fatalf("%s over %+v: want widening=%v, got %v", c.next, c.current, c.want, got)
		}
	}
}

func TestValidation(t *testing.T) {
	for _, ok := range []string{"licence_suspension_hold", "Q12.interim", "a"} {
		if !ValidReasonCode(ok) {
			t.Fatalf("%q must be a valid reason code", ok)
		}
	}
	for _, bad := range []string{"", " x", "free text with spaces", strings.Repeat("a", 65), "-lead"} {
		if ValidReasonCode(bad) {
			t.Fatalf("%q must be refused", bad)
		}
	}
	for _, a := range []string{ActionAllow, ActionHoldForReview, ActionBlock} {
		if !ValidAction(a) {
			t.Fatal(a)
		}
	}
	if ValidAction("override") || ValidAction("") {
		t.Fatal("an unknown action is never valid")
	}
}

// Who may do what (defence in depth behind the database): only a platform platform_admin changes; a tenant reads its own
// tenant only; nobody else.
func TestActorMatrix(t *testing.T) {
	tenant := uuid.New()
	plat := Actor{StaffID: uuid.New(), Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin)}
	if err := requireChanger(plat); err != nil {
		t.Fatalf("platform_admin changes: %v", err)
	}
	for _, a := range []Actor{
		{StaffID: uuid.New(), Scope: ScopeTenant, TenantID: tenant, Role: string(auth.RoleTenantAdmin)},
		{StaffID: uuid.New(), Scope: ScopeTenant, TenantID: tenant, Role: string(auth.RoleCompliance)},
		{StaffID: uuid.New(), Scope: ScopePlatform, Role: string(auth.RoleCompliance)},
		{StaffID: uuid.Nil, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin)},
		{StaffID: uuid.New(), Scope: ScopePlatform, TenantID: tenant, Role: string(auth.RolePlatformAdmin)},
		{StaffID: uuid.New(), Scope: "platform_acting", TenantID: tenant, Role: string(auth.RolePlatformAdmin)},
	} {
		if err := requireChanger(a); !errors.Is(err, ErrForbidden) {
			t.Fatalf("%+v must not change a policy, got %v", a, err)
		}
	}
	reader := Actor{StaffID: uuid.New(), Scope: ScopeTenant, TenantID: tenant, Role: string(auth.RoleTenantAdmin)}
	if err := requireReader(reader, tenant); err != nil {
		t.Fatalf("a tenant reads its own policy: %v", err)
	}
	if err := requireReader(reader, uuid.New()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a foreign tenant is not found, got %v", err)
	}
	if err := requireReader(Actor{StaffID: uuid.New(), Scope: ScopeTenant, TenantID: tenant, Role: string(auth.RoleSupport)}, tenant); !errors.Is(err, ErrForbidden) {
		t.Fatalf("support may not read, got %v", err)
	}
	if err := requireReader(plat, tenant); err != nil {
		t.Fatalf("platform reads any tenant: %v", err)
	}
}

func TestCountApprovals(t *testing.T) {
	now := time.Now()
	reqPerson, reqStaff := uuid.New(), uuid.New()
	c := Change{RequestedBy: reqStaff, RequestedByPersonID: reqPerson, PayloadHash: "h"}
	p1, p2 := uuid.New(), uuid.New()
	apps := []Approval{
		{ApproverID: uuid.New(), ApproverPersonID: p1, PayloadHash: "h", DecidedAt: now.Add(-time.Hour)},
		{ApproverID: uuid.New(), ApproverPersonID: p1, PayloadHash: "h", DecidedAt: now},        // same Person twice
		{ApproverID: reqStaff, ApproverPersonID: uuid.New(), PayloadHash: "h", DecidedAt: now},  // the requester's account
		{ApproverID: uuid.New(), ApproverPersonID: reqPerson, PayloadHash: "h", DecidedAt: now}, // the requester's Person
		{ApproverID: uuid.New(), ApproverPersonID: uuid.New(), PayloadHash: "other", DecidedAt: now},
	}
	if fresh, distinct := countApprovals(c, apps, now, DefaultApprovalMaxAge); fresh != 1 || distinct != 1 {
		t.Fatalf("one approver cannot loosen: fresh=%d distinct=%d", fresh, distinct)
	}
	apps = append(apps, Approval{ApproverID: uuid.New(), ApproverPersonID: p2, PayloadHash: "h", DecidedAt: now.Add(-25 * time.Hour)})
	if fresh, distinct := countApprovals(c, apps, now, DefaultApprovalMaxAge); fresh != 1 || distinct != 2 {
		t.Fatalf("a stale approval counts as distinct but not fresh: fresh=%d distinct=%d", fresh, distinct)
	}
}

func TestClassify(t *testing.T) {
	for _, c := range []struct {
		pe   *pgconn.PgError
		want error
	}{
		{&pgconn.PgError{Code: "23505", ConstraintName: "uq_withdrawal_restriction_policy_changes_open"}, ErrDuplicatePending},
		{&pgconn.PgError{Code: "23505", ConstraintName: "uq_withdrawal_restriction_policy_approvals_person"}, ErrAlreadyDecided},
		{&pgconn.PgError{Code: "WR032"}, ErrSelfApproval},
		{&pgconn.PgError{Code: "WR022"}, ErrSelfApproval},
		{&pgconn.PgError{Code: "WR002"}, ErrNotConfigurable},
		{&pgconn.PgError{Code: "WR031"}, ErrNotPending},
		{&pgconn.PgError{Code: "55P03"}, ErrRetryableLockTimeout},
	} {
		if got := classify(c.pe); !errors.Is(got, c.want) {
			t.Fatalf("%s: want %v, got %v", c.pe.Code, c.want, got)
		}
	}
	for _, code := range []string{"WR020", "WR040", "AP001", "LC001"} {
		if GuardCode(classify(&pgconn.PgError{Code: code})) != code {
			t.Fatalf("%s must surface as a guard code", code)
		}
	}
}

func repoRoot() string { return filepath.Join("..", "..") }

func readRepo(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoRoot(), rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Migration 0182 static pins: it ships NO policy row and NO legal value; the configurable causes' defaults are exactly the
// behaviour before 0182 (allow at every stage); the tenant / brand / market causes are NOT configurable; the verifier is
// the 0177 body plus the marked block (and the down restores the 0177 body byte for byte); no SECURITY DEFINER is added.
func TestMigration0182_Static(t *testing.T) {
	up := readRepo(t, "migrations/0182_withdrawal_restriction_policy.up.sql")
	down := readRepo(t, "migrations/0182_withdrawal_restriction_policy.down.sql")
	m0177 := readRepo(t, "migrations/0177_licensing_change_approvals.up.sql")

	code := stripSQLComments(up)
	if regexp.MustCompile(`(?i)INSERT\s+INTO\s+withdrawal_restriction_policies`).MatchString(code) {
		t.Fatal("0182 must ship no policy row")
	}
	if regexp.MustCompile(`(?i)INSERT\s+INTO\s+withdrawal_restriction_policy_changes`).MatchString(code) {
		t.Fatal("0182 must ship no change request")
	}
	for _, cause := range []string{"licence_suspended", "licence_expired", "licence_not_yet_issued", "licence_missing"} {
		re := regexp.MustCompile(`\('` + cause + `', 'licence', true, true, 'allow', 'allow', 'allow',`)
		if !re.MatchString(up) {
			t.Fatalf("%s must be configurable with the interim default allow / allow / allow (parity with pre-0182)", cause)
		}
	}
	for _, cause := range []string{"tenant_suspended", "tenant_closed", "brand_suspended", "brand_closed", "market_closed"} {
		re := regexp.MustCompile(`\('` + cause + `', '[a-z_]+', false, false, NULL, NULL, NULL,`)
		if !re.MatchString(up) {
			t.Fatalf("%s must be listed NOT configurable with no default", cause)
		}
	}
	// The verifier: 0177's body (lines between CREATE OR REPLACE FUNCTION actor_proof_require( and the SECURITY DEFINER
	// trailer) with exactly the 0182 block and allow-list term added; the down carries the 0177 body verbatim.
	v0177 := verifierBody(t, m0177)
	vUp := verifierBody(t, up)
	vDown := verifierBody(t, down)
	if vDown != v0177 {
		t.Fatal("the down must restore the 0177 verifier body byte for byte")
	}
	stripped := strings.Replace(vUp, "\n                                         OR p_operation LIKE 'withdrawal\\_restriction\\_policy:%') THEN", ") THEN", 1)
	i := strings.Index(stripped, "    -- 0182 (withdrawal restriction policy, lic11)")
	j := strings.Index(stripped, "    -- The NULL-tenant encoding")
	if i < 0 || j < 0 || i > j {
		t.Fatal("the 0182 verifier block must sit right before the NULL-tenant rule")
	}
	if stripped[:i]+stripped[j:] != v0177 {
		t.Fatal("the 0182 verifier must equal the 0177 body plus the marked block and allow-list term only")
	}
	if strings.Count(code, "SECURITY DEFINER") != 1 {
		t.Fatalf("only the replaced verifier may be SECURITY DEFINER, found %d", strings.Count(code, "SECURITY DEFINER"))
	}
	// Every CREATE FUNCTION pins search_path.
	for _, fn := range regexp.MustCompile(`(?s)CREATE (?:OR REPLACE )?FUNCTION .*?\$\$;`).FindAllString(code, -1) {
		if !strings.Contains(fn, "SET search_path = pg_catalog, public, pg_temp") {
			t.Fatalf("a function does not pin search_path: %.120s", fn)
		}
	}
	// Existing money-path guards are NOT replaced by 0182.
	for _, g := range []string{"withdrawal_requests_subject_active_guard", "withdrawal_requests_approved_hold_freeze", "withdrawal_dispatch_decision_ok",
		"withdrawal_gate_decisions_actor_proof_guard", "licensing_change_classify", "licensing_change_row_matches"} {
		if strings.Contains(code, g) {
			t.Fatalf("0182 must not touch %s", g)
		}
	}
	if regexp.MustCompile(`(?i)INSERT\s+INTO\s+licensing_change_kinds`).MatchString(code) {
		t.Fatal("0182 must not touch licensing_change_kinds")
	}
}

func verifierBody(t *testing.T, src string) string {
	t.Helper()
	i := strings.Index(src, "CREATE OR REPLACE FUNCTION actor_proof_require(")
	if i < 0 {
		t.Fatal("no verifier")
	}
	rest := src[i:]
	end := "$$ LANGUAGE plpgsql SECURITY DEFINER\n    SET search_path = pg_catalog, public, pg_temp;"
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatal("no verifier trailer")
	}
	return rest[:j+len(end)]
}

func stripSQLComments(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(s, "\n") {
		if k := strings.Index(l, "--"); k >= 0 {
			l = l[:k]
		}
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String()
}

// Only this package writes the policy, change and approval tables (production code; tests and the migration excluded).
func TestStatic_OnlyThisPackageWritesThePolicy(t *testing.T) {
	needles := []*regexp.Regexp{
		regexp.MustCompile(`(?i)INSERT\s+INTO\s+withdrawal_restriction_polic`),
		regexp.MustCompile(`(?i)UPDATE\s+withdrawal_restriction_polic`),
	}
	err := filepath.WalkDir(repoRoot(), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".claude", "migrations":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(repoRoot(), p)
		rel = filepath.ToSlash(rel)
		if strings.HasPrefix(rel, "internal/withdrawalpolicy/") {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		for _, n := range needles {
			if n.Match(b) {
				t.Errorf("%s writes a withdrawal restriction policy table outside internal/withdrawalpolicy", rel)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
