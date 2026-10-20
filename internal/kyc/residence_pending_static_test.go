package kyc

import (
	"os"
	"strings"
	"testing"
)

// lic10 (migration 0181): the market evidence read must exclude a determination that needs a second Person and has none. The
// behaviour is proven by the integration test (residence_maker_checker_integration_test.go); this static pin keeps the exclusion
// from being edited away without a failing test in environments without a database.
func TestReadMarketResidenceEvidence_ExcludesPendingDeterminations(t *testing.T) {
	b, err := os.ReadFile("market_evidence.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "func ReadMarketResidenceEvidence")
	if i < 0 {
		t.Fatal("function missing")
	}
	body := src[i:]
	for _, need := range []string{
		"residence_determination_checks rc",
		"rc.determined_at = kv.verified_residence_set_at",
		"rc.approval_required AND rc.approved_at IS NULL",
		"rc.tenant_id = $1",
	} {
		if !strings.Contains(body, need) {
			t.Errorf("the evidence read lost its pending-determination exclusion: %q missing", need)
		}
	}
	// The exclusion is inside the candidate subquery (the lateral), not a post-filter of the winner: pending must fall back to
	// an older approved determination, never "win and then be dropped" (which would also hide a still-valid older one).
	lat := strings.Index(body, "JOIN LATERAL")
	exc := strings.Index(body, "residence_determination_checks rc")
	if lat < 0 || exc < lat {
		t.Error("the pending exclusion must sit inside the LATERAL candidate selection")
	}
}

func TestPendingResidenceApprovalList_CarriesNoCountry(t *testing.T) {
	b, err := os.ReadFile("market_evidence.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "func ListPendingResidenceApprovals")
	body := src[i:]
	if strings.Contains(strings.ToLower(body), "verified_residence_country") {
		t.Error("the pending list must not read the residence VALUE (its own permission, HDR-J-3f)")
	}
}
