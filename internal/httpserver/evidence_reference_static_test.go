package httpserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ADR 0114 S0 (security BLOCKER 2): the M4 evidence reference text R is never exposed through the API
// or tenant-visible audit metadata while evidence_ref_hash is OPEN. The server still reads R from the
// database (internal/payments/manual_resolution_m4.go); only these two exposures are pinned shut.
func TestEvidenceReferenceText_NotExposedByAPIOrAuditMetadata(t *testing.T) {
	for name, src := range readPackageSources(t) {
		if strings.Contains(src, `json:"evidence_reference"`) || strings.Contains(src, `json:"evidence_reference,`) {
			t.Errorf("%s declares a JSON field evidence_reference: the reference text must not be returned (use evidence_reference_present)", name)
		}
	}
	paths, err := filepath.Glob(filepath.Join("..", "payments", "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob payments: %v %d", err, len(paths))
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), `md["evidence_reference"]`) || strings.Contains(string(b), `"evidence_reference":`) {
			t.Errorf("%s writes the reference text into audit metadata (key evidence_reference)", p)
		}
	}
}
