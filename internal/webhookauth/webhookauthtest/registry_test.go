package webhookauthtest_test

import (
	"reflect"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
	"github.com/Diansalas/igaming-platform/internal/webhookauth/webhookauthtest"
)

// nonSyntheticFixtures is where every REAL vendor scheme's conformance
// fixture is registered, keyed by its Scheme.Name(). A real scheme can only
// be registered with a domain orchestrator (webhookauth.NewSchemeSet) once
// its name is in webhookauth's conformance manifest, and
// TestEveryRegisteredNonSyntheticSchemeRunsConformance fails for any
// manifest name without an entry here. Empty today: no real vendor scheme
// exists and none is invented (proposal §22).
var nonSyntheticFixtures = map[string]func() webhookauthtest.Fixture{}

// TestEveryRegisteredNonSyntheticSchemeRunsConformance (security C9 point 4,
// ruling R6): every non-synthetic scheme that CAN be registered runs the
// full suite, including the mandatory SC7 timestamp window and a vendor
// known-answer vector. Passes vacuously today (empty manifest); adding a
// manifest entry without a fixture turns it red (see
// TestMissingConformance_FlagsManifestEntryWithoutFixture and the W1a
// mutation record).
func TestEveryRegisteredNonSyntheticSchemeRunsConformance(t *testing.T) {
	manifest := webhookauth.ConformanceManifest()
	if missing := webhookauthtest.MissingConformance(manifest, nonSyntheticFixtures); len(missing) > 0 {
		t.Fatalf("non-synthetic schemes registered in the conformance manifest without a conformance fixture (and known-answer vector): %v", missing)
	}
	for _, name := range manifest {
		f := nonSyntheticFixtures[name]()
		t.Run(name, func(t *testing.T) {
			if f.Scheme == nil || f.Scheme.Name() != name {
				t.Fatalf("fixture registered under %q is for a different scheme", name)
			}
			if f.Scheme.Properties().Synthetic {
				t.Fatalf("fixture %q is for a synthetic scheme; the manifest is for real vendor schemes only", name)
			}
			if len(f.KnownAnswers) == 0 {
				t.Fatalf("fixture %q has no vendor known-answer vector", name)
			}
			webhookauthtest.RunSchemeConformance(t, f)
		})
	}
}

func TestMissingConformance_FlagsManifestEntryWithoutFixture(t *testing.T) {
	fixtures := map[string]func() webhookauthtest.Fixture{
		"vendor-covered-v1": func() webhookauthtest.Fixture { return webhookauthtest.Fixture{} },
		"vendor-nil-v1":     nil,
	}
	got := webhookauthtest.MissingConformance([]string{"vendor-uncovered-v1", "vendor-covered-v1", "vendor-nil-v1"}, fixtures)
	if want := []string{"vendor-nil-v1", "vendor-uncovered-v1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("MissingConformance = %v, want %v", got, want)
	}
	if got := webhookauthtest.MissingConformance(nil, fixtures); len(got) != 0 {
		t.Fatalf("an empty manifest must have nothing missing, got %v", got)
	}
}
