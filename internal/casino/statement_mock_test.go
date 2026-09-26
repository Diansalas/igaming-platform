package casino

import (
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
)

// Stage 10.3 W3a (CAS-RECON-STMT-1): the MOCK casino statement source is a
// synthetic component (so MOCK-ADAPTER-PROD-1's production guard refuses a
// binary that wires it), satisfies the provider-neutral contract, and
// labels itself MOCK and PROVIDER DEPENDENT in every record it produces.
func TestMockStatementSource_IsSyntheticAndLabelledMock(t *testing.T) {
	var src statement.CasinoStatementSource = MockStatementSource{}
	if _, ok := src.(providerkind.Synthetic); !ok {
		t.Fatal("MockStatementSource must implement providerkind.Synthetic")
	}
	if _, ok := src.(providerkind.ProductionEligible); ok {
		t.Fatal("MockStatementSource must never be ProductionEligible")
	}
	for _, want := range []string{"MOCK", "PROVIDER DEPENDENT", "tautological"} {
		if !strings.Contains(src.Label(), want) {
			t.Errorf("label %q must contain %q", src.Label(), want)
		}
	}
}
