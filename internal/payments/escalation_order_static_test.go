package payments

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// ADR 0102 7.7 (B6 security review C1): the alert tables are the terminal lock level, so every
// call of the deposit T16 helper must be the LAST work of its branch. The accepted shape is
// exactly
//
//	_, err := s.escalateDepositIfDue(...)   (or `_, err = ...`)
//	return err                              (or `return true, err`)
//
// i.e. the call is a statement of its own (not an `if ...; err != nil` prelude that lets more
// work follow) and the very next statement returns its error. This is a source-order guard:
// the ordering is not observable through database state, so no behavioural test can pin it.
func TestEscalationCallIsLastStatementOfItsBranch_ADR0102_7_7(t *testing.T) {
	call := regexp.MustCompile(`^\s*_, err :?= s\.escalateDepositIfDue\(`)
	next := regexp.MustCompile(`^\s*return (true, )?err$`)
	total := 0
	for _, f := range []string{"sweeper.go", "poll_evidence.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if !strings.Contains(l, "s.escalateDepositIfDue(") || strings.HasPrefix(strings.TrimSpace(l), "//") || strings.HasPrefix(l, "func ") {
				continue
			}
			total++
			if !call.MatchString(l) {
				t.Errorf("%s:%d: escalateDepositIfDue must be a standalone `_, err := ...` statement: %q", f, i+1, strings.TrimSpace(l))
				continue
			}
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j >= len(lines) || !next.MatchString(lines[j]) {
				t.Errorf("%s:%d: escalateDepositIfDue must be followed immediately by `return err`", f, i+1)
			}
		}
	}
	if total != 5 {
		t.Fatalf("expected 5 call sites of s.escalateDepositIfDue (pending, transport, ambiguous, missing, unreferenced), found %d", total)
	}
}
