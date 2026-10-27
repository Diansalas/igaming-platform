package aml_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TenantScope is the screening worker's test seam: no production code (cmd or internal, outside _test files and the worker's own
// definition) may set it, and cmd/platform-api must not start the worker in production.
func TestScreeningWorker_TenantScopeSeamIsNeverSetInProduction(t *testing.T) {
	root := repoRoot(t)
	re := regexp.MustCompile(`TenantScope\s*(=|:)[^=]`)
	for _, dir := range []string{"cmd", "internal"} {
		_ = filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
				strings.HasSuffix(path, filepath.Join("aml", "screening_worker.go")) {
				return nil
			}
			src, rerr := os.ReadFile(path)
			if rerr == nil && re.Match(src) && strings.Contains(string(src), "ScreeningWorker") {
				t.Errorf("%s sets ScreeningWorker.TenantScope outside a test", path)
			}
			return nil
		})
	}
}

func TestMain_ScreeningWorkerIsNotStartedInProduction_AndUsesOnlyTheMock(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "cmd", "platform-api", "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "aml.NewScreeningWorker(")
	if i < 0 {
		t.Fatal("the screening worker wiring is missing from main.go")
	}
	guard := strings.LastIndex(src[:i], `cfg.GuardEnvironment() == "production"`)
	if guard < 0 || i-guard > 700 {
		t.Error("the screening worker must be guarded by an explicit production check immediately before it is built")
	}
	if !strings.Contains(src[i:i+200], "providers.AMLScreening") || !strings.Contains(src[i:i+200], "aml.MockSubjectKeys{}") {
		t.Error("the screening worker must be built from the registered MOCK provider and the MOCK subject key resolver only")
	}
}
