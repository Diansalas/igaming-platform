package httpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/payments"
)

// F2 (A10b), DB-free: the K3 and HSEC DTOs carry the asset exponent as given by the registry lookup, for the
// exponents the platform uses (2 fiat, 8 BTC, 18 ETH-class), and JSON null (never a guess, never 0) when it could
// not be resolved.
func TestResolutionDTOs_CarryRegistryExponent(t *testing.T) {
	for _, tc := range []struct {
		asset string
		exp   int16
	}{{"USD", 2}, {"BTC", 8}, {"ETH", 18}, {"JPY", 0}} {
		exps := map[string]int16{tc.asset: tc.exp}
		k3 := toResolutionDTO(staffViewer{}, payments.ManualResolution{ID: uuid.New(), AssetCode: tc.asset, Amount: 1}, exps)
		hs := toHoldResolutionDTO(staffViewer{}, payments.HoldResolution{ID: uuid.New(), AssetCode: tc.asset, Amount: 1}, exps)
		for name, v := range map[string]any{"k3": k3, "hold": hs} {
			b, err := json.Marshal(v)
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(b, &m); err != nil {
				t.Fatal(err)
			}
			got, present := m["exponent"]
			if !present || got != float64(tc.exp) {
				t.Errorf("%s %s: exponent = %v (present %v), want %d", name, tc.asset, got, present, tc.exp)
			}
		}
	}
}

func TestResolutionDTOs_UnknownAssetIsNullExponent(t *testing.T) {
	k3 := toResolutionDTO(staffViewer{}, payments.ManualResolution{ID: uuid.New(), AssetCode: "ZZZ"}, map[string]int16{"USD": 2})
	hs := toHoldResolutionDTO(staffViewer{}, payments.HoldResolution{ID: uuid.New(), AssetCode: "ZZZ"}, nil)
	for name, v := range map[string]any{"k3": k3, "hold": hs} {
		b, _ := json.Marshal(v)
		if !strings.Contains(string(b), `"exponent":null`) {
			t.Errorf("%s: an unresolved asset must serialise exponent:null, got %s", name, b)
		}
	}
	if exponentRef(nil, "USD") != nil {
		t.Error("exponentRef on a missing code must be nil")
	}
}
