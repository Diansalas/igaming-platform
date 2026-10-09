package providerkind

// B13-B (ADR 0111 2.5 / 15.5 L-7): the Synthetic marker is a Go METHOD, so it is inherited through
// struct embedding. A real (non-mock) type that embeds a Synthetic type silently becomes
// Synthetic and would satisfy the payout tiering predicate (payoutinstrument.IsSyntheticComponent)
// while the production guard (RefuseSyntheticInProduction) would also see it as a mock - or, worse,
// a real adapter wrapping a mock would pass for synthetic. This scan makes that structurally
// impossible to introduce unnoticed: no type may embed a Synthetic-marked type (or the
// providerkind.Synthetic interface) unless it declares its OWN SyntheticComponent method, i.e. it
// is deliberately and explicitly a mock itself.
//
// Pure read-only source scan (go/ast + go/parser, no go/types), like ScanForUnmarkedMocks. Types are
// matched by (package name, type name) - conservative across packages that share a name.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// EmbeddingFinding is one struct that embeds a Synthetic-marked type without declaring its own marker.
type EmbeddingFinding struct {
	Package  string
	File     string
	Type     string
	Embedded string
}

func (f EmbeddingFinding) String() string {
	return fmt.Sprintf("%s: type %s (package %s) embeds Synthetic-marked %s without declaring its own SyntheticComponent method", f.File, f.Type, f.Package, f.Embedded)
}

type structInfo struct {
	pkg, file, name string
	embedded        []string // "pkg.Type" or "Type" resolved to the same package
}

// ScanForSyntheticEmbedding walks the given roots and reports every struct type that embeds a
// Synthetic-marked type (transitively) without declaring its own SyntheticComponent method.
func ScanForSyntheticEmbedding(roots ...string) ([]EmbeddingFinding, error) {
	synthetic := map[string]bool{"providerkind.Synthetic": true} // the interface itself
	var structs []structInfo
	for _, root := range roots {
		dirs, err := collectPackageDirs(root)
		if err != nil {
			return nil, err
		}
		for _, dir := range dirs {
			ss, marked, pkg, err := scanStructs(dir)
			if err != nil {
				return nil, err
			}
			structs = append(structs, ss...)
			for name := range marked {
				synthetic[pkg+"."+name] = true
			}
		}
	}
	// Own-marker set, per (pkg, type), for the exemption.
	own := map[string]bool{}
	for k := range synthetic {
		own[k] = true
	}
	var findings []EmbeddingFinding
	// Fixpoint: a type that embeds a Synthetic type inherits the marker, so a type embedding THAT
	// type is flagged too.
	inherited := map[string]bool{}
	for changed := true; changed; {
		changed = false
		for _, st := range structs {
			key := st.pkg + "." + st.name
			if own[key] || inherited[key] {
				continue
			}
			for _, e := range st.embedded {
				if synthetic[e] || inherited[e] {
					inherited[key] = true
					changed = true
					findings = append(findings, EmbeddingFinding{Package: st.pkg, File: st.file, Type: st.name, Embedded: e})
					break
				}
			}
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

// scanStructs returns every struct type of the package in dir with its embedded fields, plus the
// set of type names that declare a SyntheticComponent method.
func scanStructs(dir string) ([]structInfo, map[string]bool, string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, "", fmt.Errorf("providerkind: read dir %s: %w", dir, err)
	}
	pkgName := filepath.Base(dir)
	var out []structInfo
	marked := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, nil, "", fmt.Errorf("providerkind: parse %s: %w", path, err)
		}
		pkg := file.Name.Name
		pkgName = pkg
		// import name (alias or the last path element) -> imported package name
		aliases := map[string]string{}
		for _, imp := range file.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			real := p[strings.LastIndex(p, "/")+1:]
			name := real
			if imp.Name != nil {
				name = imp.Name.Name
			}
			aliases[name] = real
		}
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
					if it, isIface := ts.Type.(*ast.InterfaceType); isIface && it.Methods != nil {
						// Security L-3: interface embedding. An interface that lists SyntheticComponent itself is a
						// marker declaration (like providerkind.Synthetic): embedding it inherits the marker, so it joins
						// the marked set. An interface that EMBEDS a marked type/interface (and lists no marker of its
						// own) is recorded like a struct embedding and is a finding.
						info := structInfo{pkg: pkg, file: path, name: ts.Name.Name}
						for _, f := range it.Methods.List {
							if len(f.Names) == 0 {
								if key := embeddedKey(f.Type, pkg, aliases); key != "" {
									info.embedded = append(info.embedded, key)
								}
							} else if f.Names[0].Name == "SyntheticComponent" {
								marked[ts.Name.Name] = true
							}
						}
						out = append(out, info)
						continue
					}
					st, ok := ts.Type.(*ast.StructType)
					if !ok || st.Fields == nil {
						continue
					}
					info := structInfo{pkg: pkg, file: path, name: ts.Name.Name}
					for _, f := range st.Fields.List {
						if len(f.Names) != 0 {
							continue // a named field is composition, not embedding
						}
						if key := embeddedKey(f.Type, pkg, aliases); key != "" {
							info.embedded = append(info.embedded, key)
						}
					}
					out = append(out, info)
				}
			case *ast.FuncDecl:
				if d.Recv != nil && len(d.Recv.List) > 0 && d.Name.Name == "SyntheticComponent" {
					if n := recvTypeName(d.Recv.List[0].Type); n != "" {
						marked[n] = true
					}
				}
			}
		}
	}
	return out, marked, pkgName, nil
}

func embeddedKey(expr ast.Expr, pkg string, aliases map[string]string) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return embeddedKey(t.X, pkg, aliases)
	case *ast.Ident:
		return pkg + "." + t.Name
	case *ast.SelectorExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			if real, ok := aliases[id.Name]; ok {
				return real + "." + t.Sel.Name
			}
			return id.Name + "." + t.Sel.Name
		}
	case *ast.IndexExpr: // generic instantiation
		return embeddedKey(t.X, pkg, aliases)
	}
	return ""
}
