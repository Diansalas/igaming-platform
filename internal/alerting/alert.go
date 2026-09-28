package alerting

import (
	"encoding/json"
	"fmt"
	"regexp"

	"github.com/google/uuid"
)

// discriminatorPattern mirrors migration 0108's alerts.discriminator
// CHECK exactly (ADR §3.2).
var discriminatorPattern = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,160}$`)

// requestIDPattern mirrors migration 0108's request_id CHECK exactly
// (SR-3, ADR §3.2/§9 item 2).
var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

// maxAttributesBytes mirrors the migration's octet_length(attributes) CHECK.
const maxAttributesBytes = 2048

// AttrValue is a flat JSON scalar - the only shape an alert attribute may
// take (ADR §3.2: "a flat scalar object", no nested object or array, no
// PII/secret/token/raw error text channel).
type AttrValue = any

// Alert is the raise-site's request to record one occurrence of a Kind
// (ADR §5). Scope is deliberately NOT a field here: it is captured by the
// ScopedRunner that is ALREADY OPEN when Raise/RaiseGuarded/RaiseDetached
// is called, never chosen by the Alert payload (SR-1) - this is what
// makes "RaiseDetached chooses its scope from the Alert" (the ADR §11
// mutant) structurally impossible: there is no field to choose from.
type Alert struct {
	Kind            Kind
	SubjectTenantID uuid.UUID // required iff Def(Kind).RequiresSubject
	Discriminator   string    // server-side stable ids only (LF F6, SR-3)
	Attributes      map[string]AttrValue
}

// invalidAlertError is a Go-side validation failure (LF F1/AL-6a). It is
// its own type so RaiseGuarded/RaiseDetached can recognise "this never
// reached SQL" distinctly from a database error, and route it to the
// §6.3 fallback with sqlstate_class="go_validation" without ever
// propagating it into the caller's business transaction.
type invalidAlertError struct {
	kind Kind
	msg  string
}

func (e *invalidAlertError) Error() string {
	return fmt.Sprintf("alerting: invalid alert for kind %q: %s", e.kind, e.msg)
}

// droppedAttributes is returned alongside a validated payload so callers
// (and tests) can observe security Part 1's N-1 requirement: a
// non-conforming request_id is DROPPED and counted, never allowed to
// degrade a specific Kind's alert into a generic raise_failed.
type validated struct {
	attrs   map[string]AttrValue
	dropped []string
}

// validate runs every Go-side check BEFORE any SQL (LF F1): the Kind is
// known, the subject/scope shape matches Def(a.Kind), the discriminator
// charset, the attribute allowlist, the flat-scalar/size shape, and the
// request_id charset - with the N-1 carve-out that a non-conforming
// request_id is dropped (and counted by the caller), not treated as a
// fatal validation error for the whole Alert.
func (a Alert) validate() (validated, error) {
	def, ok := Def(a.Kind)
	if !ok {
		return validated{}, &invalidAlertError{kind: a.Kind, msg: "unknown kind"}
	}
	if def.RequiresSubject && a.SubjectTenantID == uuid.Nil {
		return validated{}, &invalidAlertError{kind: a.Kind, msg: "requires a subject tenant id"}
	}
	if !def.RequiresSubject && a.SubjectTenantID != uuid.Nil {
		return validated{}, &invalidAlertError{kind: a.Kind, msg: "must not carry a subject tenant id"}
	}
	if !discriminatorPattern.MatchString(a.Discriminator) {
		return validated{}, &invalidAlertError{kind: a.Kind, msg: "discriminator fails the charset/length check"}
	}

	out := make(map[string]AttrValue, len(a.Attributes))
	var dropped []string
	for k, v := range a.Attributes {
		if _, allowed := def.AllowedKeys[k]; !allowed {
			return validated{}, &invalidAlertError{kind: a.Kind, msg: fmt.Sprintf("attribute key %q is not in the allowlist for this kind", k)}
		}
		switch v.(type) {
		case map[string]any, []any:
			return validated{}, &invalidAlertError{kind: a.Kind, msg: fmt.Sprintf("attribute %q must be a flat scalar", k)}
		case error:
			// There is no "error" attribute type (ADR §5): never let a Go
			// error value (which may carry a raw message/stack) flow into
			// an attribute.
			return validated{}, &invalidAlertError{kind: a.Kind, msg: fmt.Sprintf("attribute %q may not be an error value", k)}
		}
		if k == "request_id" {
			s, isString := v.(string)
			if !isString || !requestIDPattern.MatchString(s) {
				// N-1 (security confirmation note): DROP, don't fail the
				// whole alert. The caller-controlled X-Request-Id header
				// must never be able to degrade a specific P1 into a
				// generic raise_failed.
				dropped = append(dropped, k)
				continue
			}
		}
		out[k] = v
	}

	b, err := json.Marshal(out)
	if err != nil {
		return validated{}, &invalidAlertError{kind: a.Kind, msg: "attributes are not JSON-serializable"}
	}
	if len(b) > maxAttributesBytes {
		return validated{}, &invalidAlertError{kind: a.Kind, msg: "attributes exceed the size limit"}
	}

	return validated{attrs: out, dropped: dropped}, nil
}

// ProviderRefFingerprint marks a value as already having been reduced to
// a provider-reference fingerprint (ADR §5: "Provider refs appear only as
// fingerprints"). It exists so a raise site cannot accidentally pass a
// raw provider reference string into an attribute that looks like a
// fingerprint - callers MUST go through providerref.Fingerprint first and
// wrap its result here; there is no other way to construct this type,
// and it marshals as a plain JSON string (still a flat scalar).
type ProviderRefFingerprint string

// MarshalJSON makes ProviderRefFingerprint a plain JSON string, so it
// remains a flat scalar attribute value.
func (f ProviderRefFingerprint) MarshalJSON() ([]byte, error) {
	return json.Marshal(string(f))
}
