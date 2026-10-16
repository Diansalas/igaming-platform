package main_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// This file deliberately carries NO build tag, so `go test ./...` (and CI) run it. It pins the property the rest of the
// directory depends on: every other Go file here is behind the `stagingacceptance` build tag, so the black-box staging
// suite can never be compiled into a binary or run by CI, and can never make a plain `go build ./...` fail.
func TestEveryAcceptanceSuiteFileIsBehindTheBuildTag(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, f := range files {
		if f == "tag_guard_test.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		first, _, _ := strings.Cut(string(b), "\n")
		if strings.TrimSpace(first) != "//go:build stagingacceptance" {
			t.Errorf("%s must start with //go:build stagingacceptance (found %q)", f, first)
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no suite files found: the guard would pass vacuously")
	}
}
