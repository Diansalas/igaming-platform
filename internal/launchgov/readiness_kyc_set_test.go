package launchgov

import (
	"testing"

	"github.com/google/uuid"
)

// lic23 SN-1: the aggregate over the evaluated jurisdictions.
func TestKYCPolicyFromSet(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	cases := []struct {
		name     string
		in       []kycJurisdictionCounts
		result   string
		detail   string
		dormant  int
		partial  int
		dormant0 string
	}{
		{"empty set (no licence, no basis)", nil, ResultFail, "no_licence_jurisdiction", 0, 0, ""},
		{"one jurisdiction, all three", []kycJurisdictionCounts{{a, 1, 1, 1}}, ResultPass, "ok", 0, 0, ""},
		{"one jurisdiction, none", []kycJurisdictionCounts{{a, 0, 0, 0}}, ResultFail, "no_active_policy", 1, 0, a.String()},
		{"one jurisdiction, partial", []kycJurisdictionCounts{{a, 1, 0, 0}}, ResultPass, "ok_partial_operations_dormant", 0, 1, ""},
		{"licence complete, basis none", []kycJurisdictionCounts{{a, 1, 1, 1}, {b, 0, 0, 0}}, ResultFail, "no_active_policy", 1, 0, b.String()},
		{"licence none, basis partial", []kycJurisdictionCounts{{a, 0, 0, 0}, {b, 0, 1, 0}}, ResultFail, "no_active_policy", 1, 1, a.String()},
		{"both partial", []kycJurisdictionCounts{{a, 1, 0, 1}, {b, 0, 1, 1}}, ResultPass, "ok_partial_operations_dormant", 0, 2, ""},
		{"both complete", []kycJurisdictionCounts{{a, 1, 1, 1}, {b, 2, 1, 1}}, ResultPass, "ok", 0, 0, ""},
	}
	for _, c := range cases {
		got, dormant, partial := kycPolicyFromSet(c.in)
		if got.Result != c.result || got.DetailCode != c.detail || len(dormant) != c.dormant || len(partial) != c.partial {
			t.Errorf("%s: %+v dormant=%v partial=%v", c.name, got, dormant, partial)
		}
		if c.dormant0 != "" && dormant[0] != c.dormant0 {
			t.Errorf("%s: dormant[0]=%s want %s", c.name, dormant[0], c.dormant0)
		}
	}
}
