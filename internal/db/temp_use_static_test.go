package db

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoRuntimeTempObjectsInProductionCode is the static half of the PRH-2 R2
// invariant "the runtime role cannot create temporary objects" (migration 0116,
// ADR 0108, TRIGGER-SEARCH-PATH-1). The database REVOKEs TEMPORARY from PUBLIC
// and the runtime role, so any production SQL that creates a TEMP object would
// fail at run time with 42501. This test fails earlier and louder: no non-test
// .go file under cmd/ or internal/ may contain CREATE [GLOBAL|LOCAL] TEMP[ORARY],
// SELECT ... INTO TEMP[ORARY], or an object qualified into the pg_temp schema.
// Anyone who needs a TEMP object in production code must first revisit that
// invariant (ADR 0108) - the unpinned 0026..0113 functions are exploitable
// through exactly this capability. A bare "pg_temp" at the END of a pinned
// search_path is not an object reference and is allowed.
func TestNoRuntimeTempObjectsInProductionCode(t *testing.T) {
	pat := regexp.MustCompile(`(?i)(create\s+(?:global\s+|local\s+)?temp(?:orary)?\b|into\s+(?:global\s+|local\s+)?temp(?:orary)?\b|pg_temp\s*\.)`)
	for _, f := range []string{
		"CREATE TEMP TABLE x (a int)", "create temporary table x (a int)", "CREATE GLOBAL TEMP TABLE x (a int)",
		"SELECT 1 INTO TEMP x", "select 1 into temporary table x", "INSERT INTO pg_temp.x VALUES (1)", "CREATE FUNCTION pg_temp.f()",
	} {
		if !pat.MatchString(f) {
			t.Fatalf("self-check: pattern must flag %q", f)
		}
	}
	for _, ok := range []string{"SET search_path = pg_catalog, public, pg_temp", "temperature", "INTO template_x", "a temp value"} {
		if pat.MatchString(ok) {
			t.Fatalf("self-check: pattern must not flag %q", ok)
		}
	}

	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	var scanned int
	var offenders []string
	for _, top := range []string{"cmd", "internal"} {
		err := filepath.WalkDir(filepath.Join(root, top), func(path string, d fs.DirEntry, err error) error {
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
			scanned++
			if loc := pat.FindString(string(b)); loc != "" {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel+": "+loc)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if scanned < 100 {
		t.Fatalf("scanned only %d production .go files; the walk is broken", scanned)
	}
	if len(offenders) > 0 {
		t.Fatalf("production code creates TEMP objects, which the runtime role cannot do after migration 0116 (ADR 0108); revisit that invariant first: %v", offenders)
	}
}
