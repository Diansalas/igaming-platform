package adjustment

// MANUAL-ADJ-LINK-1 (migration 0153, ADR 0100 section 21): static pins for the owner-only fixture exemption.
//
// The exemption (internal/testsupport/adjlinkfix, used directly or through adjlinkpost / the per-package
// mustPostFixture helpers / B-12's withoutLinkFence) is the existing ADR 0110 s9.4 owner residual - the table owner
// disabling the two 0153 triggers inside its own transaction. It is not a production bypass, but every use of it is a
// place where a test does NOT exercise the fence, so the set of callers is an inventory: a new caller file fails
// this test until it is added below with a one-line justification (and reviewed).

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var linkFenceExemptionCall = regexp.MustCompile(`\badjlinkfix\.Without\(|\badjlinkpost\.Post\(|\bmustPostFixture\(|\bwithoutLinkFence\(`)

// linkFenceExemptionInventory: every file that calls the exemption, and why. Paths are repo-relative, slash form.
var linkFenceExemptionInventory = map[string]string{
	"internal/testsupport/adjlinkpost/adjlinkpost.go":                    "the exemption wrapped around one ledger.Post (adjlinkfix itself cannot import ledger)",
	"internal/adjustment/b11_b17_integration_test.go":                    "B-12: proves the 0113 link trigger (MA040) still refuses forged links with the 0153 fence absent",
	"internal/adjustment/b23_b25_integration_test.go":                    "B-25: plants a rogue posting through the owner residual so the detective recon kind is still proven",
	"internal/adjustment/k2_security_conditions_integration_test.go":     "K2-R1: plants a governed-key posting for a pending request to prove the MA040 non-executed-exit backstop",
	"internal/casino/bonus_settlement_integration_test.go":               "fixture: drains a round lock / funds a wallet with a generic posting",
	"internal/casino/orchestrator_integration_test.go":                   "fixture: funds the wallet",
	"internal/idempotency/integration_test.go":                           "fixture: FIRST post of the in-house vehicle; every replay runs with the fence enabled",
	"internal/launchgov/funds_integration_test.go":                       "fixture: funds / nets a wallet",
	"internal/ledger/ledger_integration_test.go":                         "defines mustPostFixture (generic manual_adjustment postings of this package's tests)",
	"internal/ledger/bonus_mirror_integration_test.go":                   "fixture: Rule B2 worked shapes posted generically as manual_adjustment",
	"internal/ledger/locked_origin_split_integration_test.go":            "fixture: bonus-set and cash-lock shapes posted generically as manual_adjustment",
	"internal/ledger/replay_integration_test.go":                         "fixture: the L7 original; its replays run with the fence enabled",
	"internal/reconciliation/casino_consistency_integration_test.go":     "fixture: funds the wallet with a non-casino posting",
	"internal/reconciliation/sportsbook_settlement_integration_test.go":  "fixture: funds the wallet with a non-sportsbook posting",
	"internal/risk/cumulative_leg_integration_test.go":                   "fixture: funds the wallet",
	"internal/risk/cumulative_sportsbook_settlement_integration_test.go": "fixture: funds the wallet with a non-sportsbook posting",
	"internal/sportsbook/orchestrator_integration_test.go":               "fixture: funds the wallet",
	"internal/sportsbook/settlement_assertions_integration_test.go":      "fixture: funds the wallet",
	"internal/wallet/locked_origin_summary_integration_test.go":          "fixture: posts the cash lock shape",
}

func TestLinkFence0153_ExemptionCallersInventoried(t *testing.T) {
	root := repoRoot(t)
	found := map[string]bool{}
	walkGo(t, root, func(path string, src []byte) {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		rel = filepath.ToSlash(rel)
		if rel == "internal/adjustment/link_fence_static_test.go" || !linkFenceExemptionCall.Match(src) {
			return // this file holds the planted negative controls
		}
		found[rel] = true
	})
	var unknown, stale []string
	for f := range found {
		if _, ok := linkFenceExemptionInventory[f]; !ok {
			unknown = append(unknown, f)
		}
	}
	for f := range linkFenceExemptionInventory {
		if !found[f] {
			stale = append(stale, f)
		}
	}
	sort.Strings(unknown)
	sort.Strings(stale)
	if len(unknown) > 0 {
		t.Errorf("new caller(s) of the 0153 owner-only fixture exemption; add each to the inventory with a justification:\n  %s",
			strings.Join(unknown, "\n  "))
	}
	if len(stale) > 0 {
		t.Errorf("inventory entries with no caller left; remove them:\n  %s", strings.Join(stale, "\n  "))
	}
	// The exemption helpers exist only in integration builds, never in a production binary.
	for _, f := range []string{"internal/testsupport/adjlinkfix/adjlinkfix.go", "internal/testsupport/adjlinkpost/adjlinkpost.go"} {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(b), "//go:build integration\n") {
			t.Errorf("%s must be integration-build only", f)
		}
	}
}

func TestLinkFence0153_ExemptionCallRegexCatchesPlants(t *testing.T) {
	for _, plant := range []string{`adjlinkfix.Without(ctx, tx, fn)`, `adjlinkpost.Post(ctx, tx, in)`, `mustPostFixture(t, pool, f, in)`, `withoutLinkFence(build)`} {
		if !linkFenceExemptionCall.MatchString(plant) {
			t.Fatalf("plant not detected: %s", plant)
		}
	}
	if linkFenceExemptionCall.MatchString(`// adjlinkfix.Without is documented here`) {
		t.Fatal("a prose mention was flagged")
	}
}

// The trigger names adjlinkfix disables are exactly the ones migration 0153 creates (and drops in its down).
func TestLinkFence0153_ExemptionNamesMatchMigration(t *testing.T) {
	root := repoRoot(t)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	fix := read("internal/testsupport/adjlinkfix/adjlinkfix.go")
	up := read("migrations/0153_manual_adjustment_link_fence.up.sql")
	down := read("migrations/0153_manual_adjustment_link_fence.down.sql")
	for _, c := range []struct{ name, table string }{
		{"ledger_transactions_manual_adjustment_link", "ledger_transactions"},
		{"ledger_entries_manual_adjustment_link", "ledger_entries"},
	} {
		if !strings.Contains(fix, `"`+c.name+`"`) {
			t.Errorf("adjlinkfix does not name %s", c.name)
		}
		if !strings.Contains(up, "CREATE CONSTRAINT TRIGGER "+c.name+"\n    AFTER INSERT ON "+c.table+"\n    NOT DEFERRABLE INITIALLY IMMEDIATE") {
			t.Errorf("0153 up does not create %s as a NOT DEFERRABLE AFTER INSERT constraint trigger on %s", c.name, c.table)
		}
		if !strings.Contains(down, "DROP TRIGGER IF EXISTS "+c.name+" ON "+c.table+";") {
			t.Errorf("0153 down does not drop %s", c.name)
		}
	}
	// No owner / GUC / environment bypass in the fence functions (CLAUDE.md, registry: "no test-bypass GUC").
	var code []string // the SQL with its -- comment lines removed (the header explains why there is no definer)
	for _, l := range strings.Split(up, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "--") {
			code = append(code, l)
		}
	}
	sql := strings.ToUpper(strings.Join(code, "\n"))
	for _, forbidden := range []string{"CURRENT_USER", "SESSION_USER", "PG_HAS_ROLE", "CURRENT_SETTING", "SECURITY DEFINER", "RG_DATABASE_ENVIRONMENT"} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("0153 up must not reference %s", forbidden)
		}
	}
}
