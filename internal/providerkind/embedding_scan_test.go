package providerkind

import (
	"path/filepath"
	"sort"
	"testing"
)

// TestSyntheticEmbedding_RepoWide is the B13-B repo-wide check (ADR 0111 15.5 L-7): no type in
// internal/** or cmd/** embeds a Synthetic-marked type (or providerkind.Synthetic) unless it declares
// its own SyntheticComponent method. The Synthetic marker is a method, so it is inherited through
// embedding; a real adapter wrapping a mock would otherwise satisfy the payout tiering predicate.
func TestSyntheticEmbedding_RepoWide(t *testing.T) {
	internal := repoInternalDir(t)
	cmd := filepath.Join(filepath.Dir(internal), "cmd")
	findings, err := ScanForSyntheticEmbedding(internal, cmd)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

// TestSyntheticEmbedding_NegativeControl proves the scan is real: the fixtures contain eight
// violations (direct, pointer, transitive, struct-embeds-interface, interface-embeds-marker, interface-embeds-interface,
// struct-embeds-declared-marker interface) and the non-violations.
func TestSyntheticEmbedding_NegativeControl(t *testing.T) {
	findings, err := ScanForSyntheticEmbedding(filepath.Join("testdata", "embedding"))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, f := range findings {
		got = append(got, f.Type)
	}
	sort.Strings(got)
	want := []string{"IfaceEmbedsLocal", "IfaceEmbedsMarker", "RealEmbedsChain", "RealEmbedsInterface", "RealEmbedsMarker", "RealEmbedsPtr", "StructEmbedsDeclared", "StructEmbedsIface"}
	if len(got) != len(want) {
		t.Fatalf("findings = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("findings = %v, want %v", got, want)
		}
	}
}

// The repo-wide walk from this package's own directory must skip testdata (the fixtures would
// otherwise fail the repo-wide check).
func TestSyntheticEmbedding_SkipsTestdata(t *testing.T) {
	findings, err := ScanForSyntheticEmbedding(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("unexpected finding from the package's own tree: %s", f)
	}
}
