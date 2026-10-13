package identity

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// ADR 0118 T11 (BRAND-REHOME-HOLD-1): migration 0136 makes the identity columns of player_accounts,
// wallets and kyc_verifications immutable in the database (BR001 / BR002 / BR003). This static pin keeps
// the Go side honest: no non-test Go file may contain an SQL UPDATE (or an upsert's DO UPDATE) of one of
// those tables whose SET list names a pinned column, so a new writer fails here before it fails at
// runtime with a BR code (which is a security signal, ADR 0118 D3). A heuristic over source text, the
// same family as internal/launchgov/status_writer_static_test.go: it reads Go string literals, it does
// not parse SQL, and a statement built by string concatenation of the table name would escape it (the
// dynamic `UPDATE %s` form is checked against every pinned column of every table).

// bindingPinned is ADR 0118 s2's table.
var bindingPinned = map[string][]string{
	"player_accounts":   {"id", "tenant_id", "brand_id", "created_at"},
	"wallets":           {"id", "tenant_id", "brand_id", "player_account_id", "asset_code", "created_at"},
	"kyc_verifications": {"tenant_id", "brand_id", "player_account_id"},
}

var (
	bindingRawString = regexp.MustCompile("(?s)`[^`]*`" + `|"(?:[^"\\\n]|\\.)*"`)
	// UPDATE [ONLY] [public.]table [[AS] alias] SET <list> - the list ends at WHERE / FROM / RETURNING / ; / end.
	bindingUpdateRE = regexp.MustCompile(`(?is)\bupdate\s+(?:only\s+)?(?:"?public"?\.)?"?(%s|%q|%v|player_accounts|wallets|kyc_verifications)"?(?:\s+(?:as\s+)?(\w+))?\s+set\s+(.*?)(?:\bwhere\b|\bfrom\b|\breturning\b|;|$)`)
	// INSERT INTO table ... ON CONFLICT ... DO UPDATE SET <list>.
	bindingUpsertRE = regexp.MustCompile(`(?is)\binsert\s+into\s+(?:"?public"?\.)?"?(player_accounts|wallets|kyc_verifications)"?\b.*?\bdo\s+update\s+set\s+(.*?)(?:\bwhere\b|\breturning\b|;|$)`)
)

// splitTopLevel splits a SET list on commas outside parentheses.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, s[start:i])
				start = i + 1
			}
		}
	}
	return append(out, s[start:])
}

// assignedColumns returns the lower-cased column names a SET list assigns (alias prefixes and quotes
// stripped; the `(a, b) = (...)` tuple form included).
func assignedColumns(setList string) []string {
	var cols []string
	clean := func(c string) string {
		c = strings.Trim(strings.TrimSpace(c), `"`)
		if i := strings.LastIndex(c, "."); i >= 0 {
			c = strings.Trim(c[i+1:], `"`)
		}
		return strings.ToLower(c)
	}
	for _, part := range splitTopLevel(setList) {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "(") {
			end := strings.Index(part, ")")
			if end < 0 {
				continue
			}
			for _, c := range strings.Split(part[1:end], ",") {
				cols = append(cols, clean(c))
			}
			continue
		}
		eq := strings.Index(part, "=")
		if eq < 0 {
			continue
		}
		cols = append(cols, clean(part[:eq]))
	}
	return cols
}

// pinnedWrites returns, for one SQL text, every "table.column" pinned assignment it contains, and the
// number of UPDATE / upsert statements of the three tables it recognised at all.
func pinnedWrites(sql string) (hits []string, statements int) {
	check := func(table, setList string) {
		statements++
		tables := []string{table}
		if strings.HasPrefix(table, "%") { // dynamic table name: any pinned column of any table
			tables = []string{"player_accounts", "wallets", "kyc_verifications"}
		}
		for _, c := range assignedColumns(setList) {
			for _, tb := range tables {
				for _, p := range bindingPinned[tb] {
					if c == p {
						hits = append(hits, tb+"."+c)
					}
				}
			}
		}
	}
	for _, m := range bindingUpdateRE.FindAllStringSubmatch(sql, -1) {
		table := strings.ToLower(m[1])
		if strings.EqualFold(m[2], "set") { // `UPDATE wallets SET ...`: the optional alias group swallowed SET
			continue
		}
		check(table, m[3])
	}
	for _, m := range bindingUpsertRE.FindAllStringSubmatch(sql, -1) {
		check(strings.ToLower(m[1]), m[2])
	}
	return hits, statements
}

// Negative control: the detector finds every pinned assignment shape and ignores every legitimate one.
func TestBrandBindingStaticGuard_DetectorControl(t *testing.T) {
	bad := map[string]string{
		"UPDATE player_accounts SET brand_id = $1 WHERE id = $2":                                           "player_accounts.brand_id",
		"update public.wallets set status = 'frozen', brand_id = $1 where id = $2":                         "wallets.brand_id",
		`UPDATE "wallets" w SET w.player_account_id = $1 WHERE w.id = $2`:                                  "wallets.player_account_id",
		"UPDATE wallets AS w SET asset_code = 'USD' WHERE id = $1":                                         "wallets.asset_code",
		"UPDATE kyc_verifications SET (status, player_account_id) = ('approved', $1) WHERE id = $2":        "kyc_verifications.player_account_id",
		"WITH pa AS (UPDATE player_accounts SET tenant_id = $1 WHERE id = $2 RETURNING id) SELECT 1":       "player_accounts.tenant_id",
		"INSERT INTO wallets (id) VALUES ($1) ON CONFLICT (id) DO UPDATE SET brand_id = EXCLUDED.brand_id": "wallets.brand_id",
		"UPDATE %s SET created_at = now() WHERE id = $1":                                                   "player_accounts.created_at",
		"UPDATE ONLY player_accounts SET id = gen_random_uuid()":                                           "player_accounts.id",
	}
	for sql, want := range bad {
		hits, n := pinnedWrites(sql)
		found := false
		for _, h := range hits {
			if h == want {
				found = true
			}
		}
		if !found || n == 0 {
			t.Errorf("detector missed %q in %q (hits %v, statements %d)", want, sql, hits, n)
		}
	}
	good := []string{
		"UPDATE player_accounts SET status = $1, updated_at = now() WHERE id = $2",
		"UPDATE player_accounts SET password_hash = $1, updated_at = now() WHERE id = $2",
		"UPDATE player_accounts pa SET declared_residence_country = $3, declared_residence_captured_at = now(), updated_at = now() FROM prev WHERE pa.id = $1 AND pa.tenant_id = $2",
		"UPDATE wallets SET status = 'frozen' WHERE player_account_id = $1 AND tenant_id = $2",
		"UPDATE kyc_verifications SET status = $1, reason = $2, provider_reference = $3 WHERE id = $4 AND brand_id = $5",
		"INSERT INTO wallets (tenant_id, brand_id) VALUES ($1, $2) ON CONFLICT (player_account_id, asset_code) DO NOTHING",
		"UPDATE withdrawal_requests SET brand_id = $1",
	}
	for _, sql := range good {
		if hits, _ := pinnedWrites(sql); len(hits) != 0 {
			t.Errorf("false positive %v in %q", hits, sql)
		}
	}
}

func TestBrandBindingStaticGuard_NoGoWriterOfAPinnedColumn(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var violations []string
	statements := map[string]int{}
	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			for _, lit := range bindingRawString.FindAllString(string(b), -1) {
				hits, n := pinnedWrites(lit)
				statements[rel] += n
				for _, h := range hits {
					violations = append(violations, rel+": "+h)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("a non-test Go file writes a column ADR 0118 pins (migration 0136 refuses it with BR001/BR002/BR003; a brand transfer needs its own ADR):\n%s",
			strings.Join(violations, "\n"))
	}
	// Coverage (not vacuous): the scanner recognises the known legitimate writers (ADR 0118 s3).
	for file, min := range map[string]int{
		"internal/identity/player_account.go":  5, // status, password_hash, verified_at, status CAS, declared residence
		"internal/kyc/verification_service.go": 2,
		"internal/kyc/provider.go":             1,
	} {
		if statements[file] < min {
			t.Errorf("%s: the scanner recognised %d UPDATE statement(s) of the pinned tables, want at least %d (detector broken?)", file, statements[file], min)
		}
	}
}
