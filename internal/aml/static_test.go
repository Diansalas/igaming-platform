package aml_test

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/google/uuid"
)

// Static guards for ADR 0116 (no database): the structural rules that make the dynamic tests meaningful.

func repoRoot(t *testing.T) string {
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

// productionGoFiles returns every non-test, non-hidden Go file under internal/ and cmd/ (relative path -> source).
func productionGoFiles(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	out := map[string]string{}
	for _, top := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			base := d.Name()
			if d.IsDir() {
				if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			out[filepath.ToSlash(rel)] = string(b)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(out) < 200 {
		t.Fatalf("the scan saw only %d files: the walk is broken", len(out))
	}
	return out
}

// B1: no Go code reads or writes an AML table directly; every access is a call of a SECURITY DEFINER function.
var amlTableSQL = regexp.MustCompile(`(?i)\b(from|join|into|update|delete\s+from|truncate(\s+table)?)\s+aml_(holds|hold_lifts|alerts|cases|case_alerts|case_notes|case_events|enforcement_decisions)\b`)

func TestStatic_NoDirectAMLTableAccessInGo(t *testing.T) {
	var offenders []string
	for rel, src := range productionGoFiles(t) {
		if amlTableSQL.MatchString(src) {
			offenders = append(offenders, rel)
		}
	}
	if len(offenders) > 0 {
		sort.Strings(offenders)
		t.Fatalf("Go code touches an AML table directly (ADR 0116 B1: definer functions only): %v", offenders)
	}
	// Non-vacuity.
	if !amlTableSQL.MatchString("SELECT 1 FROM aml_holds") || !amlTableSQL.MatchString("delete from aml_cases") || amlTableSQL.MatchString("SELECT aml_hold_active($1)") {
		t.Fatal("the table-access pattern is broken")
	}
}

// The ONE hold read of the enforcement path lives in hold_eval.go; no other production file queries the gate functions.
func TestStatic_HoldReadsOnlyInHoldEval(t *testing.T) {
	re := regexp.MustCompile(`(?i)select\s+aml_hold_(active|match)\s*\(`)
	for rel, src := range productionGoFiles(t) {
		if re.MatchString(src) && rel != "internal/aml/hold_eval.go" {
			t.Errorf("%s queries the AML hold functions outside internal/aml/hold_eval.go", rel)
		}
	}
	if !re.MatchString(productionGoFiles(t)["internal/aml/hold_eval.go"]) {
		t.Fatal("hold_eval.go no longer queries aml_hold_active: the guard is vacuous")
	}
}

// ADR 0116 s5.2 C4: AML actions are audited in the AML-scoped event tables, NEVER in audit_log (which tenants read), and
// audit.TenantPresentation has no `aml.` entry.
func TestStatic_AMLNeverWritesAuditLog_NoTenantPresentation(t *testing.T) {
	for rel, src := range productionGoFiles(t) {
		if strings.HasPrefix(rel, "internal/aml/") && strings.Contains(src, "internal/audit\"") {
			t.Errorf("%s imports internal/audit: AML must not write audit_log", rel)
		}
	}
	for k := range audit.TenantPresentation {
		if strings.HasPrefix(k, "aml.") || strings.HasPrefix(k, "aml_") {
			t.Errorf("audit.TenantPresentation has the AML entry %q: a tenant would see AML activity", k)
		}
	}
	for rel, src := range productionGoFiles(t) {
		if strings.HasPrefix(rel, "internal/aml/") && regexp.MustCompile(`audit\.Record\(`).MatchString(src) {
			t.Errorf("%s calls audit.Record", rel)
		}
	}
}

// No tipping-off: the INTERNAL held outcome is never rendered. EnforcementDecision.TrueOutcome / OutcomeHeld / Hold may be
// used only inside internal/kyc/enforcement.go (composition and the AML-scoped record).
func TestStatic_TrueOutcomeNeverRendered(t *testing.T) {
	re := regexp.MustCompile(`\.TrueOutcome\b|\bOutcomeHeld\b|\.Hold\.(Held|EvalFailed|HoldID)\b`)
	for rel, src := range productionGoFiles(t) {
		if rel == "internal/kyc/enforcement.go" {
			continue
		}
		if re.MatchString(src) {
			t.Errorf("%s reads the internal AML outcome (TrueOutcome / OutcomeHeld / Hold): it must never reach a player or tenant surface", rel)
		}
	}
	if !re.MatchString(productionGoFiles(t)["internal/kyc/enforcement.go"]) {
		t.Fatal("the guard is vacuous")
	}
}

// evaluateHold (the kyc seam) is assigned only in its declaration; the aml probe variable only in its declaration.
func TestStatic_TestSeamsNotAssignedInProduction(t *testing.T) {
	assignKyc := regexp.MustCompile(`(?m)^\s*evaluateHold\s*=`)
	assignAml := regexp.MustCompile(`(?m)^\s*holdProbe\s*=`)
	for rel, src := range productionGoFiles(t) {
		if strings.HasSuffix(rel, "_integration.go") {
			continue // the integration-tag hooks
		}
		if assignKyc.MatchString(src) {
			t.Errorf("%s assigns kyc.evaluateHold in production code", rel)
		}
		if assignAml.MatchString(src) {
			t.Errorf("%s assigns aml.holdProbe in production code", rel)
		}
	}
}

// The AML proof operations are admitted for scope tenant | platform_acting only: the plain `platform` scope (NULL tenant) is
// not provable for them (mirror of aml_authorize), and a tenant is mandatory.
func TestProofOperations_ScopeAdmission(t *testing.T) {
	iss, err := actorproof.NewIssuer("k1", map[string][]byte{"k1": []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatal(err)
	}
	ops := aml.ProofOperations()
	if len(ops) < 10 {
		t.Fatalf("expected the closed AML operation set, got %v", ops)
	}
	hash := strings.Repeat("a", 64)
	for _, op := range ops {
		if !regexp.MustCompile(`^aml_[a-z_]+:[a-z_]+$`).MatchString(op) {
			t.Errorf("operation %q does not match the database's AML operation shape", op)
		}
		actor, tenant := uuid.New(), uuid.New()
		for _, scope := range []string{actorproof.ScopeTenant, actorproof.ScopePlatformActing} {
			if _, err := iss.Sign(actorproof.Claims{Actor: actor, Scope: scope, Tenant: tenant, Operation: op, Target: actorproof.TargetNew, PayloadHash: hash}); err != nil {
				t.Errorf("%s must be signable for scope %s: %v", op, scope, err)
			}
		}
		if _, err := iss.Sign(actorproof.Claims{Actor: actor, Scope: actorproof.ScopePlatform, Operation: op, Target: actorproof.TargetNew, PayloadHash: hash}); err == nil {
			t.Errorf("%s must NOT be signable for the platform scope", op)
		}
		if _, err := iss.Sign(actorproof.Claims{Actor: actor, Scope: actorproof.ScopeTenant, Operation: op, Target: actorproof.TargetNew, PayloadHash: hash}); err == nil {
			t.Errorf("%s must NOT be signable without a tenant", op)
		}
	}
}

// SQLSTATE class AM belongs to migrations 0140-0142 alone (class selection recorded in migration 0140's header).
func TestStatic_AMClassOnlyInAMLMigrations(t *testing.T) {
	root := repoRoot(t)
	entries, err := os.ReadDir(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`ERRCODE\s*=\s*'AM[0-9]{3}'`)
	seen := 0
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(root, "migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		isAML := strings.HasPrefix(e.Name(), "0140_") || strings.HasPrefix(e.Name(), "0141_") || strings.HasPrefix(e.Name(), "0142_")
		if re.Match(b) {
			if !isAML {
				t.Errorf("%s uses SQLSTATE class AM, which belongs to the AML migrations", e.Name())
			}
			seen++
		}
	}
	if seen < 3 {
		t.Fatalf("expected AM codes in the AML migrations, saw %d files", seen)
	}
}
