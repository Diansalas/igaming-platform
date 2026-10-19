package licensingchange

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func decodeKind(t *testing.T, k Kind, body string) (any, error) {
	t.Helper()
	spec, ok := kinds[k]
	if !ok {
		t.Fatalf("kind %s is not registered", k)
	}
	p, subject, err := spec.decode(json.RawMessage(body))
	if err == nil && subject != uuid.Nil {
		t.Errorf("%s: a licence lifecycle kind has no subject tenant", k)
	}
	return p, err
}

func TestLifecycleKinds_AreRegisteredPlatformOnly(t *testing.T) {
	for _, k := range []Kind{KindLicenceStatusChange, KindLicenceValidityDatesChange, KindLicenceReviewDueChange, KindLicenceEvidenceReferenceChange} {
		spec, ok := kinds[k]
		if !ok || !spec.platformOnly || spec.precheck == nil {
			t.Errorf("%s: must be registered, platform only, with a precheck", k)
		}
	}
}

func TestLifecycleKinds_DecodeRefusals(t *testing.T) {
	id := uuid.New().String()
	cases := []struct {
		kind Kind
		body string
	}{
		{KindLicenceStatusChange, `{"licence_id":"` + id + `","status":"revoked"}`},
		{KindLicenceStatusChange, `{"licence_id":"` + id + `"}`},
		{KindLicenceStatusChange, `{"licence_id":"` + id + `","status":"active","tenant_id":"` + id + `"}`},
		{KindLicenceStatusChange, `{"licence_id":"00000000-0000-0000-0000-000000000000","status":"active"}`},
		{KindLicenceStatusChange, ``},
		// A missing date key must never be read as "clear".
		{KindLicenceValidityDatesChange, `{"licence_id":"` + id + `","issued_at":null}`},
		{KindLicenceValidityDatesChange, `{"licence_id":"` + id + `","expires_at":null}`},
		{KindLicenceValidityDatesChange, `{"licence_id":"` + id + `","issued_at":"2026-13-01","expires_at":null}`},
		{KindLicenceValidityDatesChange, `{"licence_id":"` + id + `","issued_at":"2026-1-1","expires_at":null}`},
		{KindLicenceValidityDatesChange, `{"licence_id":"` + id + `","issued_at":"2026-05-01","expires_at":"2026-05-01"}`},
		{KindLicenceValidityDatesChange, `{"licence_id":"` + id + `","issued_at":"2026-05-01","expires_at":"2026-04-01"}`},
		{KindLicenceReviewDueChange, `{"licence_id":"` + id + `"}`},
		{KindLicenceReviewDueChange, `{"licence_id":"` + id + `","review_due_at":"tomorrow"}`},
		{KindLicenceEvidenceReferenceChange, `{"licence_id":"` + id + `"}`},
		{KindLicenceEvidenceReferenceChange, `{"licence_id":"` + id + `","licence_evidence_reference":""}`},
		{KindLicenceEvidenceReferenceChange, `{"licence_id":"` + id + `","licence_evidence_reference":" x"}`},
		{KindLicenceEvidenceReferenceChange, `{"licence_id":"` + id + `","licence_evidence_reference":"a@b.example"}`},
		{KindLicenceEvidenceReferenceChange, `{"licence_id":"` + id + `","licence_evidence_reference":"a\tb"}`},
		{KindLicenceEvidenceReferenceChange, `{"licence_id":"` + id + `","licence_evidence_reference":"` + strings.Repeat("x", 201) + `"}`},
	}
	for i, c := range cases {
		if _, err := decodeKind(t, c.kind, c.body); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("case %d (%s %s): err = %v, want ErrInvalidInput", i, c.kind, c.body, err)
		}
	}
}

func TestLifecycleKinds_DecodeAcceptsAndCanonicalises(t *testing.T) {
	id := uuid.New().String()
	ok := []struct {
		kind Kind
		body string
		want string
	}{
		{KindLicenceStatusChange, `{"status":"active","licence_id":"` + id + `"}`, `{"licence_id":"` + id + `","status":"active"}`},
		{KindLicenceValidityDatesChange, `{"licence_id":"` + id + `","issued_at":null,"expires_at":"2030-01-31"}`, `{"licence_id":"` + id + `","issued_at":null,"expires_at":"2030-01-31"}`},
		{KindLicenceValidityDatesChange, `{"licence_id":"` + id + `","issued_at":null,"expires_at":null}`, `{"licence_id":"` + id + `","issued_at":null,"expires_at":null}`},
		{KindLicenceReviewDueChange, `{"licence_id":"` + id + `","review_due_at":null}`, `{"licence_id":"` + id + `","review_due_at":null}`},
		{KindLicenceEvidenceReferenceChange, `{"licence_id":"` + id + `","licence_evidence_reference":"REF-1"}`, `{"licence_id":"` + id + `","licence_evidence_reference":"REF-1"}`},
		{KindLicenceEvidenceReferenceChange, `{"licence_id":"` + id + `","licence_evidence_reference":null}`, `{"licence_id":"` + id + `","licence_evidence_reference":null}`},
	}
	for i, c := range ok {
		p, err := decodeKind(t, c.kind, c.body)
		if err != nil {
			t.Errorf("case %d: %v", i, err)
			continue
		}
		b, _ := json.Marshal(p)
		if string(b) != c.want {
			t.Errorf("case %d: canonical payload %s, want %s", i, b, c.want)
		}
	}
}
