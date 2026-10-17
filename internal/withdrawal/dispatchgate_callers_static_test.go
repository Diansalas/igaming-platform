package withdrawal

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestStatic_RecordDispatchGateDecisionCallers pins the production call sites of RecordDispatchGateDecision
// (SF4-2): the function records whatever decision it is handed, so only the payout dispatch paths that obtained
// the decision from the gate evaluation may call it. Test files and testsupport helpers are exempt.
func TestStatic_RecordDispatchGateDecisionCallers(t *testing.T) {
	root := filepath.Join("..", "..")
	want := map[string]int{
		"internal/payments/payout.go":                 2,
		"internal/payments/payout_withdrawal_gate.go": 2,
	}
	got := map[string]int{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", ".claude", "testsupport":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel == "internal/withdrawal/dispatchgate.go" {
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if n := strings.Count(string(b), "RecordDispatchGateDecision("); n > 0 {
			got[rel] = n
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if want[k] != got[k] {
			t.Errorf("unexpected RecordDispatchGateDecision callers in %s: %d (want %d)", k, got[k], want[k])
		}
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("expected %d RecordDispatchGateDecision callers in %s, found %d", n, k, got[k])
		}
	}
}
