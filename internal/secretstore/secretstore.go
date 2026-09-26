// Package secretstore is the platform's provider-credential secret-store
// boundary (ADR 0093 §4/§6 and its Stage 10.3 W2a amendment; security
// review docs/plans/stage-10.3-planning/07-w2a-design-review-security.md
// §4/§5).
//
// A provider credential is never stored in the database: the
// provider_credential_handles row holds a HANDLE (secret_ref) that pins an
// immutable version in an external store, plus a keyed fingerprint. This
// package owns:
//
//   - Store, the backend interface (one per scheme: awssm, devfile,
//     memory). Backends live in their own packages: devfile
//     (development-only), memstore (test-only; its import is restricted to
//     test code by TestSecretStore_MemstoreImportedOnlyByTests) and awssm
//     (W3b, NOT IMPLEMENTED here).
//   - Ref, the strict parser for a secret_ref, mirroring migration 0096's
//     CHECK constraints, including the tenant namespace rule.
//   - Router, the scheme -> backend map, built only from backends the
//     environment permits (config.ValidateSecretBackendScheme).
//   - Fetcher, the process-wide cache, circuit breaker, concurrency bound
//     and negative cache in front of the Router, with the platform
//     constants of security review §5.
//   - Secret, the only type that carries secret bytes, with redaction on
//     every rendering path (C15).
//
// Nothing here reads the database, and nothing here decides WHETHER a
// credential may be used: that is the per-call handle read in
// internal/providercred, which happens before any Fetch, in every breaker
// state, so revocation is always immediate.
package secretstore

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Secret carries secret bytes. Every rendering path redacts; Bytes returns
// a copy so a caller can never mutate a cached value.
type Secret struct {
	b []byte
}

// NewSecret copies b into a Secret.
func NewSecret(b []byte) Secret {
	c := make([]byte, len(b))
	copy(c, b)
	return Secret{b: c}
}

// Bytes returns a copy of the secret bytes.
func (s Secret) Bytes() []byte {
	c := make([]byte, len(s.b))
	copy(c, s.b)
	return c
}

// Len is the secret length in bytes.
func (s Secret) Len() int { return len(s.b) }

const redacted = "[REDACTED-SECRET]"

// String implements fmt.Stringer.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer.
func (s Secret) GoString() string { return redacted }

// Format implements fmt.Formatter: every verb renders the redacted form.
func (s Secret) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(redacted)) }

// LogValue implements slog.LogValuer.
func (s Secret) LogValue() slog.Value { return slog.StringValue(redacted) }

// MarshalJSON implements json.Marshaler.
func (s Secret) MarshalJSON() ([]byte, error) { return json.Marshal(redacted) }

// ErrorClass is the closed set of store failure classes (security review
// §5). A backend classifies every failure into one of these BEFORE it is
// returned; no store error text, SDK output or path ever leaves a backend.
type ErrorClass int

const (
	// ClassUnavailable: timeout, deadline, network/transport error, 5xx or
	// throttling. The ONLY class that counts toward the circuit breaker.
	ClassUnavailable ErrorClass = iota + 1
	// ClassNotFound: the ref (or its pinned version) does not exist.
	ClassNotFound
	// ClassAccessDenied: the store refused access to the ref.
	ClassAccessDenied
	// ClassInvalidVersion: the ref's version is malformed or not usable.
	ClassInvalidVersion
	// ClassStoreConfig: the backend is misconfigured for this ref (e.g. a
	// devfile permission/owner/size violation).
	ClassStoreConfig
	// ClassIntegrity: the fetched or cached secret's fingerprint differs
	// from the handle row's. P1; the value is never served.
	ClassIntegrity
	// ClassNoBackend: the ref's scheme has no backend in this process's
	// Router (for example a memory:// row outside tests).
	ClassNoBackend
	// ClassInvalidRef: the ref fails the platform's syntax rules.
	ClassInvalidRef
)

func (c ErrorClass) String() string {
	switch c {
	case ClassUnavailable:
		return "store_unavailable"
	case ClassNotFound:
		return "not_found"
	case ClassAccessDenied:
		return "access_denied"
	case ClassInvalidVersion:
		return "invalid_version"
	case ClassStoreConfig:
		return "store_config"
	case ClassIntegrity:
		return "integrity"
	case ClassNoBackend:
		return "no_backend"
	case ClassInvalidRef:
		return "invalid_ref"
	default:
		return "unknown"
	}
}

// CountsTowardBreaker reports whether a failure of this class is a store
// availability failure (security review §5 "Failures that count").
func (c ErrorClass) CountsTowardBreaker() bool { return c == ClassUnavailable }

// Error is the only error type a Store or the Fetcher returns. It carries
// the class and nothing else: never a path, a ref, store text or bytes.
type Error struct {
	Class ErrorClass
}

func (e *Error) Error() string { return "secretstore: " + e.Class.String() }

// Is lets errors.Is(err, &Error{Class: c}) match by class.
func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Class == e.Class
}

// Errorf-free constructor.
func classError(c ErrorClass) error { return &Error{Class: c} }

// NewError returns a classified error. Backends use it.
func NewError(c ErrorClass) error { return classError(c) }

// ClassOf returns err's class. An error that is not a *Error (a backend
// bug) is treated as ClassUnavailable: fail closed, and count it.
func ClassOf(err error) ErrorClass {
	if err == nil {
		return 0
	}
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return ClassUnavailable
}

// Scheme names (the backend prefixes a secret_ref may carry).
const (
	SchemeAWSSecretsManager = "awssm"
	SchemeDevFile           = "devfile"
	SchemeMemory            = "memory"
)

// Ref is a parsed, syntactically valid secret_ref. It mirrors migration
// 0096's CHECK constraints exactly (and is stricter in one place: the
// memory scheme accepts at most a version query, like devfile).
type Ref struct {
	raw      string
	scheme   string
	path     string
	version  string
	fragment string
}

// MaxRefBytes is the longest accepted secret_ref.
const MaxRefBytes = 512

var (
	refControlOrSpace = regexp.MustCompile(`[[:cntrl:][:space:]]`)
	awssmRef          = regexp.MustCompile(`^awssm://([^?#]+)\?versionId=([A-Za-z0-9-]{32,64})(?:#([A-Za-z0-9._-]{1,64}))?$`)
	devfileRef        = regexp.MustCompile(`^devfile://([^?#]+)\?version=([A-Za-z0-9._-]{1,64})$`)
	memoryRef         = regexp.MustCompile(`^memory://([^?#]+)(?:\?version=([A-Za-z0-9._-]{1,64}))?$`)
	refNameSegment    = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9._-]{0,127}$`)
)

// ParseRef parses raw. Any violation is ClassInvalidRef.
func ParseRef(raw string) (Ref, error) {
	if raw == "" || len(raw) > MaxRefBytes || refControlOrSpace.MatchString(raw) {
		return Ref{}, classError(ClassInvalidRef)
	}
	var m []string
	var scheme string
	switch {
	case strings.HasPrefix(raw, SchemeAWSSecretsManager+"://"):
		scheme, m = SchemeAWSSecretsManager, awssmRef.FindStringSubmatch(raw)
	case strings.HasPrefix(raw, SchemeDevFile+"://"):
		scheme, m = SchemeDevFile, devfileRef.FindStringSubmatch(raw)
	case strings.HasPrefix(raw, SchemeMemory+"://"):
		scheme, m = SchemeMemory, memoryRef.FindStringSubmatch(raw)
	default:
		return Ref{}, classError(ClassInvalidRef)
	}
	if m == nil {
		return Ref{}, classError(ClassInvalidRef)
	}
	r := Ref{raw: raw, scheme: scheme, path: m[1], version: m[2]}
	if len(m) > 3 {
		r.fragment = m[3]
	}
	return r, nil
}

// String returns the raw ref. A ref is NOT secret (it is staff-visible),
// but it is never logged at the public webhook boundary.
func (r Ref) String() string { return r.raw }

// Scheme is the backend scheme.
func (r Ref) Scheme() string { return r.scheme }

// Path is everything between "<scheme>://" and the query string.
func (r Ref) Path() string { return r.path }

// Version is the pinned version (awssm versionId, devfile/memory version).
func (r Ref) Version() string { return r.version }

// Fragment is the awssm JSON key, if any.
func (r Ref) Fragment() string { return r.fragment }

// namespaceMarker is the segment every provider-credential ref contains
// exactly once.
const namespaceMarker = "/provider-creds/"

// InNamespace reports whether the ref lives in the tenant's namespace for
// (domain, provider): exactly the migration 0096 function
// provider_credential_ref_in_namespace, evaluated over "<scheme>://<path>"
// (the query string and fragment are never part of the namespace).
func (r Ref) InNamespace(tenantID uuid.UUID, domain, providerID string) bool {
	segs, ok := r.namespaceSegments()
	if !ok {
		return false
	}
	return segs[0] == tenantID.String() && segs[1] == domain && segs[2] == providerID
}

// NamespaceName is the fourth namespace segment (the secret's name).
func (r Ref) NamespaceName() (string, bool) {
	segs, ok := r.namespaceSegments()
	if !ok {
		return "", false
	}
	return segs[3], true
}

func (r Ref) namespaceSegments() ([]string, bool) {
	full := r.scheme + "://" + r.path
	pos := strings.Index(full, namespaceMarker)
	if pos < 0 {
		return nil, false
	}
	if strings.Contains(full[pos+1:], namespaceMarker) {
		return nil, false
	}
	segs := strings.Split(full[pos+len(namespaceMarker):], "/")
	if len(segs) != 4 || !refNameSegment.MatchString(segs[3]) {
		return nil, false
	}
	return segs, true
}
