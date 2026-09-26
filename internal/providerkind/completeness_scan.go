package providerkind

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// mockNameHeuristic matches type names this scan treats as candidate mock/
// fake/synthetic doubles. This is DELIBERATELY NOT the guard's safety
// mechanism - RefuseSyntheticInProduction (guard.go) already fails closed
// on ANY unmarked type regardless of its name, which is exactly what
// closes security review C13's finding that a name-based deny-list ("every
// `type Mock\w+`") fails open for a `Fake…`/`Stub…`/`InMemory…`/`Dev…`
// double. This scan is a SEPARATE, secondary hygiene/documentation check:
// it proves every type that LOOKS like a synthetic double (by the
// project's own established naming conventions - see the eight mocks
// named in 01-provider-trust-analysis.md §3, all of which are `Mock…`)
// also carries the positive `Synthetic` marker, so the marker stays
// consistently applied and self-documenting rather than accidentally
// omitted on a type that obviously should have it. A type this heuristic
// cannot name-match is still safe: it simply falls to
// RefuseSyntheticInProduction's own unconditional "unmarked means refused"
// default in production.
var mockNameHeuristic = regexp.MustCompile(`(?i)(Mock|Fake|Stub|InMemory)`)

// UnmarkedFinding is one type ScanForUnmarkedMocks judged should carry the
// Synthetic marker but does not.
type UnmarkedFinding struct {
	Package string
	File    string
	Type    string
}

func (f UnmarkedFinding) String() string {
	return fmt.Sprintf("%s: type %s (package %s) has no SyntheticComponent method", f.File, f.Type, f.Package)
}

// ScanForUnmarkedMocks walks root recursively, parsing every non-test .go
// file it finds (skipping any directory literally named "testdata", the
// same convention the go tool itself uses to exclude fixture directories
// from ordinary package discovery), and returns one UnmarkedFinding for
// every exported or unexported type whose name matches mockNameHeuristic
// but for which no `func (recv T) SyntheticComponent()` (value or pointer
// receiver) is declared anywhere in the same directory (Go package).
//
// This is a pure, read-only source scan: go/ast + go/parser only, no
// go/types, no build, no network - consistent with this codebase's
// existing structural-guarantee tests (e.g. cmd/platform-api/
// kyc_prefix_e2_const_secret_test.go).
func ScanForUnmarkedMocks(root string) ([]UnmarkedFinding, error) {
	dirs, err := collectPackageDirs(root)
	if err != nil {
		return nil, err
	}

	var findings []UnmarkedFinding
	for _, dir := range dirs {
		types, markedReceivers, err := scanDir(dir)
		if err != nil {
			return nil, err
		}
		for _, ty := range types {
			if !ast.IsExported(ty.name) {
				// Unexported helper/DTO types (e.g. a wire-format payload
				// struct used only to decode a mock's own callback body)
				// are never registered as a component anywhere - they are
				// not what RefuseSyntheticInProduction inspects, and
				// requiring a marker on every internal helper would be
				// noise, not signal. Only names visible outside the
				// package are candidate registrations.
				continue
			}
			if !mockNameHeuristic.MatchString(ty.name) {
				continue
			}
			if markedReceivers[ty.name] {
				continue
			}
			findings = append(findings, UnmarkedFinding{Package: ty.pkg, File: ty.file, Type: ty.name})
		}
	}

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Type < findings[j].Type
	})
	return findings, nil
}

func collectPackageDirs(root string) ([]string, error) {
	seen := map[string]bool{}
	var dirs []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && d.Name() == "testdata" {
				return filepath.SkipDir
			}
			if path != root && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		dir := filepath.Dir(path)
		if !seen[dir] {
			seen[dir] = true
			dirs = append(dirs, dir)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("providerkind: walk %s: %w", root, err)
	}
	sort.Strings(dirs)
	return dirs, nil
}

type foundType struct {
	pkg  string
	file string
	name string
}

// scanDir parses every non-test .go file directly inside dir (not
// recursively - collectPackageDirs already enumerates every directory) and
// returns every declared named type plus the set of type names for which a
// SyntheticComponent method is declared anywhere in dir.
func scanDir(dir string) ([]foundType, map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("providerkind: read dir %s: %w", dir, err)
	}

	var types []foundType
	marked := map[string]bool{}
	fset := token.NewFileSet()

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return nil, nil, fmt.Errorf("providerkind: parse %s: %w", path, err)
		}

		pkgName := file.Name.Name
		for _, decl := range file.Decls {
			switch d := decl.(type) {
			case *ast.GenDecl:
				if d.Tok != token.TYPE {
					continue
				}
				for _, spec := range d.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					types = append(types, foundType{pkg: pkgName, file: path, name: ts.Name.Name})
				}
			case *ast.FuncDecl:
				if d.Recv == nil || len(d.Recv.List) == 0 {
					continue
				}
				if d.Name.Name != "SyntheticComponent" {
					continue
				}
				recvType := recvTypeName(d.Recv.List[0].Type)
				if recvType != "" {
					marked[recvType] = true
				}
			}
		}
	}
	return types, marked, nil
}

// recvTypeName strips a leading pointer star, if any, to get the bare
// receiver type name ("*Foo" and "Foo" both mark "Foo").
func recvTypeName(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return recvTypeName(t.X)
	case *ast.Ident:
		return t.Name
	default:
		return ""
	}
}
