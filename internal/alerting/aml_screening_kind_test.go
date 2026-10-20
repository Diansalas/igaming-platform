package alerting

import (
	"testing"

	"github.com/google/uuid"
)

// lic10 (migration 0181): the automatic screening attention Kind has a closed vocabulary of ids and codes: never the person,
// the list, the match or vendor text.
func TestKindAMLScreeningAttention_ClosedVocabulary(t *testing.T) {
	def, ok := Def(KindAMLScreeningAttention)
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
	good := Alert{Kind: KindAMLScreeningAttention, SubjectTenantID: uuid.New(), Discriminator: "request:" + uuid.NewString(),
		Attributes: map[string]AttrValue{"request_id": uuid.NewString(), "reason": "match_review", "trigger": "registration"}}
	if _, err := good.validate(); err != nil {
		t.Fatalf("a conforming alert must validate: %v", err)
	}
	for _, bad := range []string{"person_id", "list", "match_lists", "outcome", "provider_reference", "subject_ref", "error"} {
		a := good
		a.Attributes = map[string]AttrValue{"request_id": uuid.NewString(), bad: "x"}
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
