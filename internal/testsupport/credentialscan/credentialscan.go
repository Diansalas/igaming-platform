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
package credentialscan

import (
	"fmt"
	"reflect"

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

// Scan walks v (and everything reachable from it - pointers, structs,
// slices, arrays, maps and interfaces) and returns one violation string per
// field found holding a forbidden type, a non-nil httpclient.Authenticator,
// or a non-nil, non-allow-listed func value. An empty result means v is
// clean. Cycles are guarded by a visited-pointer set; a violation report
// stops descending into that field (the finding itself is what matters, not
// what it in turn contains).
func Scan(v any) []string {
	var violations []string
	visited := map[uintptr]bool{}
	var walk func(rv reflect.Value, path string)
	walk = func(rv reflect.Value, path string) {
		if !rv.IsValid() {
			return
		}
		t := rv.Type()
		for _, ft := range forbiddenTypes {
			if t == ft {
				violations = append(violations, fmt.Sprintf("%s: holds forbidden type %s", path, t.String()))
				return
			}
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
		}
	}
	walk(reflect.ValueOf(v), "root")
	return violations
}
