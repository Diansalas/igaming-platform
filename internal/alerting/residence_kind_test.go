package alerting

import (
	"testing"

	"github.com/google/uuid"
)

// F5(b): the residence-into-open-market Kind has a closed, ids-only vocabulary. A country (or any key outside it)
// is refused by Go validation before any SQL runs.
func TestKindKYCResidenceIntoOpenMarket_ClosedIDsOnlyVocabulary(t *testing.T) {
	def, ok := Def(KindKYCResidenceIntoOpenMarket)
	if !ok {
		t.Fatal("kind not registered")
	}
	if def.Severity != SeverityP2 || def.Scope != ScopeKindPlatform || !def.RequiresSubject || !def.InTxRaisableByTenant ||
		def.RaiseMode != RaiseModeInTx || def.Simulation {
		t.Fatalf("unexpected definition %+v", def)
	}
	if len(def.AllowedKeys) != 3 {
		t.Fatalf("allowed keys = %v", def.AllowedKeys)
	}
	for _, k := range []string{"verification_id", "brand_id", "request_id"} {
		if _, ok := def.AllowedKeys[k]; !ok {
			t.Fatalf("missing allowed key %q", k)
		}
	}
	tenant := uuid.New()
	good := Alert{Kind: KindKYCResidenceIntoOpenMarket, SubjectTenantID: tenant, Discriminator: "verification:" + uuid.NewString(),
		Attributes: map[string]AttrValue{"verification_id": uuid.NewString(), "brand_id": uuid.NewString(), "request_id": "req-1"}}
	if _, err := good.validate(); err != nil {
		t.Fatalf("a conforming alert must validate: %v", err)
	}
	for _, bad := range []string{"country", "verified_residence_country", "player_account_id", "reviewer", "country_code"} {
		a := good
		a.Attributes = map[string]AttrValue{"verification_id": uuid.NewString(), bad: "MT"}
		if _, err := a.validate(); err == nil {
			t.Fatalf("attribute %q must be refused", bad)
		}
	}
	noSubject := good
	noSubject.SubjectTenantID = uuid.Nil
	if _, err := noSubject.validate(); err == nil {
		t.Fatal("a subject tenant is required")
	}
}
