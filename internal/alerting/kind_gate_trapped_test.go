package alerting

import "testing"

// lic18: the trapped-person Kind carries a count only, never an identifier.
func TestKindAMLScreeningGateTrapped_CountOnly(t *testing.T) {
	d, ok := Def(KindAMLScreeningGateTrapped)
	if !ok {
		t.Fatal("kind not registered")
	}
	if d.Severity != SeverityP1 || !d.RequiresSubject || !d.InTxRaisableByTenant {
		t.Fatalf("definition: %+v", d)
	}
	if _, ok := d.AllowedKeys["persons"]; len(d.AllowedKeys) != 1 || !ok {
		t.Fatalf("allowed keys %v, want only persons (a count)", d.AllowedKeys)
	}
}
