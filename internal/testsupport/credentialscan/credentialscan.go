// Package credentialscan is the shared implementation of ADR 0095 §16.2
// item 15 (PROV-OUTBOUND-CRED-1, "no credential held by an adapter",
// S95-C8(c)): a small, generic reflection walk over an already-constructed
// value (a registered adapter, resolver, or an orchestrator's whole
// provider map) that fails if it finds a field of a known credential/secret
// type, or a non-nil func-typed field. Test-support code only - never
// imported by production code - so it may safely import the credential
// types it checks for (providercred, secretstore, httpclient) without
// creating a production import edge those packages must avoid (the static
// half of this same ADR condition, checked separately per package by each
// domain's own AdapterPackageImports test).
//
// RV-PRH-I1 security review M5 fix round: this walker additionally handles
// atomic.Pointer[T] (the type parameter is visible at the reflect.Type
// level even when the pointer itself is nil - it is smuggled in through
// the zero-length phantom array field every atomic.Pointer[T] carries),
// chan T (checked structurally, since there is no live value to inspect
// without receiving from the channel, which this package must never do),
// unsafe.Pointer (flagged unconditionally - its target type is not
// recoverable via reflection at all) and pointer-receiver Authenticator
// implementations (reflect.PointerTo(t).Implements(authenticatorType) for
// any addressable value, not just t.Implements(authenticatorType)
// directly - a type whose Authenticate/RedactionValues methods have
// pointer receivers does not itself satisfy the interface, only *T does).
package credentialscan

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providers/httpclient"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

var forbiddenTypes = []reflect.Type{
	reflect.TypeOf(providercred.OutboundCredential{}),
	reflect.TypeOf(secretstore.Secret{}),
	reflect.TypeOf(providercred.DerivedTokenCache{}),
}

var authenticatorType = reflect.TypeOf((*httpclient.Authenticator)(nil)).Elem()

// AllowedFuncFields is a package-qualified allow-list of "Type.FieldName"
// entries permitted to hold a non-nil func value - §11's "func-typed fields
// are forbidden unless allow-listed". Empty by default; a caller that
// legitimately needs one (there is none known in this codebase today) adds
// it here, reviewed, rather than this package silently ignoring every func
// field.
var AllowedFuncFields = map[string]bool{}

// isForbiddenType reports whether t is (or, for a pointer/array element,
// wraps) one of the forbidden credential/secret types.
func isForbiddenType(t reflect.Type) bool {
	for _, ft := range forbiddenTypes {
		if t == ft {
			return true
		}
	}
	return false
}

// atomicPointerElem returns the T in atomic.Pointer[T] for t, or nil if t is
// not an atomic.Pointer instantiation. atomic.Pointer[T]'s own struct layout
// (sync/atomic, Go's standard library) carries an unexported zero-length
// array field of type [0]*T specifically so the type parameter survives
// even when no value was ever stored - this walks that field's element type
// rather than reading any actual pointer value (which may be nil).
func atomicPointerElem(t reflect.Type) reflect.Type {
	if t.Kind() != reflect.Struct || t.PkgPath() != "sync/atomic" || !strings.HasPrefix(t.Name(), "Pointer[") {
		return nil
	}
	for i := 0; i < t.NumField(); i++ {
		ft := t.Field(i).Type
		if ft.Kind() == reflect.Array && ft.Len() == 0 && ft.Elem().Kind() == reflect.Ptr {
			return ft.Elem().Elem()
		}
	}
	return nil
}

// Scan walks v (and everything reachable from it - pointers, structs,
// slices, arrays, maps, interfaces, channels and atomic.Pointer[T]) and
// returns one violation string per field found holding a forbidden type, a
// non-nil httpclient.Authenticator (including a pointer-receiver
// implementation on an addressable value), a raw unsafe.Pointer, or a
// non-nil, non-allow-listed func value. An empty result means v is clean.
// Cycles are guarded by a visited-pointer set; a violation report stops
// descending into that field (the finding itself is what matters, not what
// it in turn contains).
func Scan(v any) []string {
	var violations []string
	visited := map[uintptr]bool{}
	var walk func(rv reflect.Value, path string)
	walk = func(rv reflect.Value, path string) {
		if !rv.IsValid() {
			return
		}
		t := rv.Type()

		if isForbiddenType(t) {
			violations = append(violations, fmt.Sprintf("%s: holds forbidden type %s", path, t.String()))
			return
		}
		if elem := atomicPointerElem(t); elem != nil && isForbiddenType(elem) {
			violations = append(violations, fmt.Sprintf("%s: holds atomic.Pointer[%s], a forbidden type", path, elem.String()))
			return
		}
		if t.Kind() != reflect.Interface && t.Implements(authenticatorType) {
			// A non-nil value (pointer or otherwise) satisfying
			// Authenticator - the zero value of most such types is a
			// legitimate "not yet built" state, so only flag when the
			// underlying value is actually populated.
			if !rv.IsZero() {
				violations = append(violations, fmt.Sprintf("%s: holds a non-nil httpclient.Authenticator (%s)", path, t.String()))
				return
			}
		} else if t.Kind() != reflect.Interface && t.Kind() != reflect.Ptr && reflect.PointerTo(t).Implements(authenticatorType) {
			// Pointer-receiver implementation: T itself does not satisfy
			// Authenticator, only *T does. reflect.PointerTo(t).Implements
			// needs no addressability - it is a pure type-level check - so
			// this must never be gated on rv.CanAddr(). RV-PRH-I1 security
			// review M5 residual #1: a value-typed adapter stored in an
			// interface or map (exactly the shape paymentsAdapters()
			// returns) is NOT addressable, so the old rv.CanAddr() gate let
			// a pointer-receiver Authenticator held by value inside a
			// map[string]any or interface{} slip through undetected. Flag
			// when populated - a real adapter could store this as a value
			// field specifically to dodge a naive t.Implements check.
			if !rv.IsZero() {
				violations = append(violations, fmt.Sprintf("%s: holds a value whose pointer type implements httpclient.Authenticator (%s, pointer-receiver)", path, t.String()))
				return
			}
		}

		switch rv.Kind() {
		case reflect.Ptr:
			if rv.IsNil() {
				return
			}
			addr := rv.Pointer()
			if visited[addr] {
				return
			}
			visited[addr] = true
			walk(rv.Elem(), path)
		case reflect.Interface:
			if rv.IsNil() {
				return
			}
			walk(rv.Elem(), path)
		case reflect.Struct:
			for i := 0; i < t.NumField(); i++ {
				walk(rv.Field(i), path+"."+t.Field(i).Name)
			}
		case reflect.Slice, reflect.Array:
			for i := 0; i < rv.Len(); i++ {
				walk(rv.Index(i), fmt.Sprintf("%s[%d]", path, i))
			}
		case reflect.Map:
			// Never call k.Interface() here: a map value reached through an
			// unexported field is read-only-flagged by reflect, and
			// Interface() panics on such a value. An index-based label is
			// enough for a violation report to be actionable.
			for i, k := range rv.MapKeys() {
				walk(rv.MapIndex(k), fmt.Sprintf("%s[map entry %d]", path, i))
			}
		case reflect.Func:
			if !rv.IsNil() && !AllowedFuncFields[path] {
				violations = append(violations, fmt.Sprintf("%s: holds a non-nil func value (forbidden unless allow-listed)", path))
			}
		case reflect.Chan:
			// No live value can be inspected without receiving from the
			// channel (which this package must never do - it could consume
			// a real message a production goroutine is waiting to send).
			// Checked structurally instead: a channel typed to carry a
			// forbidden credential type end to end is refused regardless
			// of whether anything is currently queued on it.
			elem := t.Elem()
			if isForbiddenType(elem) || reflect.PointerTo(elem).Implements(authenticatorType) || elem.Implements(authenticatorType) {
				violations = append(violations, fmt.Sprintf("%s: holds a chan of forbidden type %s", path, elem.String()))
			}
		case reflect.UnsafePointer:
			// The pointee's type is not recoverable via reflection at all,
			// so an unsafe.Pointer field in an adapter is flagged
			// unconditionally - it cannot be proven safe, only proven
			// absent.
			violations = append(violations, fmt.Sprintf("%s: holds an unsafe.Pointer field (cannot be verified safe)", path))
		}
	}
	walk(reflect.ValueOf(v), "root")
	return violations
}

// ForbiddenPackageImports is the static half of the ADR §11 condition:
// import paths an adapter PACKAGE may never depend on directly. The secret
// store's own Fetcher (internal/secretstore/fetcher.go) lives in this same
// package, so forbidding the secretstore import path covers it too - there
// is no separate importable path for the Fetcher alone.
var ForbiddenPackageImports = []string{
	`"github.com/Diansalas/igaming-platform/internal/secretstore"`,
}

// CheckPackageImports is the RV-PRH-I1 security review M5 fix: PACKAGE-WIDE
// (every non-test .go file in dir), not just one hand-picked adapter file.
// Returns one violation string per forbidden import found, naming the file.
func CheckPackageImports(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, forbidden := range ForbiddenPackageImports {
			if strings.Contains(string(src), forbidden) {
				violations = append(violations, fmt.Sprintf("%s: imports %s", path, forbidden))
			}
		}
	}
	return violations, nil
}

// CheckPackageLevelVars is the RV-PRH-I1 security review M5 fix: no
// package-level (file-scope) `var` declaration anywhere in dir's non-test
// .go files may have a forbidden credential/secret type - a package-level
// var is a far worse hazard than a struct field (it lives for the whole
// process and is trivially reachable from anywhere in the package, not just
// from a specific constructed value this package's reflection Scan can
// reach). AST-based, not reflection - there is no running value to reflect
// on for a var declared but perhaps never assigned to an exported
// accessor.
func CheckPackageLevelVars(dir string) ([]string, error) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, err
		}
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.VAR {
				continue
			}
			for _, spec := range gd.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || vs.Type == nil {
					continue
				}
				typeName := exprTypeName(vs.Type)
				for _, forbiddenName := range []string{"OutboundCredential", "Secret", "DerivedTokenCache"} {
					if strings.Contains(typeName, forbiddenName) {
						for _, id := range vs.Names {
							violations = append(violations, fmt.Sprintf("%s: package-level var %s has forbidden type %s", path, id.Name, typeName))
						}
					}
				}
			}
		}
	}
	return violations, nil
}

// exprTypeName renders a type expression's name well enough to substring-
// match against the forbidden type name list - not a full type checker,
// deliberately: this is a fast, dependency-free syntactic check, not a
// replacement for the reflective Scan above.
func exprTypeName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprTypeName(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + exprTypeName(t.X)
	case *ast.ArrayType:
		return "[]" + exprTypeName(t.Elt)
	case *ast.MapType:
		return "map[" + exprTypeName(t.Key) + "]" + exprTypeName(t.Value)
	case *ast.IndexExpr:
		return exprTypeName(t.X) + "[" + exprTypeName(t.Index) + "]"
	default:
		return ""
	}
}
