package alerting

import (
	"testing"

	"github.com/google/uuid"
)

func TestAlert_Validate_UnknownAttributeKeyRefused(t *testing.T) {
	a := Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: uuid.New(),
		Discriminator:   "switch:abc",
		Attributes:      map[string]AttrValue{"secret": "x"},
	}
	if _, err := a.validate(); err == nil {
		t.Fatal("expected an unknown-key validation error, got nil")
	}
}

func TestAlert_Validate_OversizeAttributesRefused(t *testing.T) {
	big := make([]byte, maxAttributesBytes+1)
	for i := range big {
		big[i] = 'a'
	}
	a := Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: uuid.New(),
		Discriminator:   "switch:abc",
		Attributes:      map[string]AttrValue{"reason_code": string(big)},
	}
	if _, err := a.validate(); err == nil {
		t.Fatal("expected an oversize validation error, got nil")
	}
}

func TestAlert_Validate_NestedAttributeRefused(t *testing.T) {
	a := Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: uuid.New(),
		Discriminator:   "switch:abc",
		Attributes:      map[string]AttrValue{"reason_code": map[string]any{"x": 1}},
	}
	if _, err := a.validate(); err == nil {
		t.Fatal("expected a flat-scalar validation error, got nil")
	}
}

func TestAlert_Validate_DiscriminatorCharsetRefused(t *testing.T) {
	a := Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: uuid.New(),
		Discriminator:   "switch abc\n",
	}
	if _, err := a.validate(); err == nil {
		t.Fatal("expected a discriminator validation error, got nil")
	}
}

func TestAlert_Validate_MissingRequiredSubjectRefused(t *testing.T) {
	a := Alert{
		Kind:          KindPaymentKillSwitchEngaged,
		Discriminator: "switch:abc",
	}
	if _, err := a.validate(); err == nil {
		t.Fatal("expected a missing-subject validation error, got nil")
	}
}

func TestAlert_Validate_MetaKindMustNotCarrySubject(t *testing.T) {
	a := Alert{
		Kind:            KindAlertingUnrouted,
		SubjectTenantID: uuid.New(),
		Discriminator:   "severity:p1",
	}
	if _, err := a.validate(); err == nil {
		t.Fatal("expected a meta-kind-must-not-carry-subject validation error, got nil")
	}
}

func TestAlert_Validate_UnknownKindRefused(t *testing.T) {
	a := Alert{Kind: Kind("no.such.kind"), Discriminator: "x"}
	if _, err := a.validate(); err == nil {
		t.Fatal("expected an unknown-kind validation error, got nil")
	}
}

// TestAlert_Validate_N1_DropsNonConformingRequestID is the security
// confirmation's N-1 test: a caller-controlled request_id that fails the
// charset check ("a b", from X-Request-Id) must be DROPPED and counted,
// never allowed to fail the whole Alert and degrade a specific Kind into
// alerting.raise_failed.
func TestAlert_Validate_N1_DropsNonConformingRequestID(t *testing.T) {
	a := Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: uuid.New(),
		Discriminator:   "switch:abc",
		Attributes:      map[string]AttrValue{"reason_code": "manual", "request_id": "a b"},
	}
	v, err := a.validate()
	if err != nil {
		t.Fatalf("expected the alert to still validate with request_id dropped, got error: %v", err)
	}
	if _, present := v.attrs["request_id"]; present {
		t.Fatal("expected request_id to be dropped from the validated attributes")
	}
	if _, present := v.attrs["reason_code"]; !present {
		t.Fatal("expected the other attribute to survive")
	}
	if len(v.dropped) != 1 || v.dropped[0] != "request_id" {
		t.Fatalf("expected dropped=[request_id], got %v", v.dropped)
	}
}

func TestAlert_Validate_ValidRequestIDKept(t *testing.T) {
	a := Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: uuid.New(),
		Discriminator:   "switch:abc",
		Attributes:      map[string]AttrValue{"request_id": "abc-123_def.456:ghi"},
	}
	v, err := a.validate()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v.attrs["request_id"] != "abc-123_def.456:ghi" {
		t.Fatalf("expected the valid request_id to be kept, got %v", v.attrs["request_id"])
	}
}

func TestAlert_Validate_ErrorValueAttributeRefused(t *testing.T) {
	a := Alert{
		Kind:            KindPaymentKillSwitchEngaged,
		SubjectTenantID: uuid.New(),
		Discriminator:   "switch:abc",
		Attributes:      map[string]AttrValue{"reason_code": errTest{}},
	}
	if _, err := a.validate(); err == nil {
		t.Fatal("expected an error-attribute validation error, got nil")
	}
}

type errTest struct{}

func (errTest) Error() string { return "boom" }

func TestKinds_ScopeSubjectConsistency(t *testing.T) {
	for _, k := range Kinds() {
		def := MustDef(k)
		if IsMetaKind(k) {
			if def.RequiresSubject {
				t.Errorf("meta kind %q must have RequiresSubject=false", k)
			}
			if def.InTxRaisableByTenant {
				t.Errorf("meta kind %q must have InTxRaisableByTenant=false", k)
			}
			continue
		}
		if def.Scope == ScopeKindPlatform && !def.RequiresSubject {
			t.Errorf("platform kind %q (non-meta) must RequireSubject", k)
		}
		if def.Simulation && def.Severity != SeverityP3 {
			t.Errorf("simulation kind %q must be p3, got %s", k, def.Severity)
		}
	}
}
