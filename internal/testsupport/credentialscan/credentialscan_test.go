package credentialscan

import (
	"net/http"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/secretstore"
)

func TestScan_CatchesOutboundCredentialField(t *testing.T) {
	type badAdapter struct {
		cred providercred.OutboundCredential
	}
	v := &badAdapter{cred: providercred.NewMockOutboundCredential(uuid.Nil, "payments", "p")}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a field holding providercred.OutboundCredential, got none")
	}
}

func TestScan_CatchesSecretField(t *testing.T) {
	type badAdapter struct {
		secret secretstore.Secret
	}
	v := &badAdapter{}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a field holding secretstore.Secret, got none")
	}
	_ = v.secret // the field is deliberately never read - its mere TYPE is what Scan must catch
}

func TestScan_CatchesNestedCredentialThroughPointerAndSlice(t *testing.T) {
	type inner struct {
		cred providercred.OutboundCredential
	}
	type badAdapter struct {
		items []*inner
	}
	v := &badAdapter{items: []*inner{{cred: providercred.NewMockOutboundCredential(uuid.Nil, "casino", "p")}}}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a credential nested through a pointer inside a slice, got none")
	}
}

func TestScan_CatchesNonNilFuncField(t *testing.T) {
	type badAdapter struct {
		fn func()
	}
	v := &badAdapter{fn: func() {}}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a non-nil, non-allow-listed func field, got none")
	}
}

// TestScan_CatchesAtomicPointerToCredential is the RV-PRH-I1 security
// review M5 evasion #1: a credential held behind an atomic.Pointer[T] must
// be caught even when the pointer is nil - the type parameter alone is
// enough.
func TestScan_CatchesAtomicPointerToCredential(t *testing.T) {
	type badAdapter struct {
		cred atomic.Pointer[providercred.OutboundCredential]
	}
	v := &badAdapter{}
	_ = v.cred.Load() // never set - the type parameter alone must still be caught
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for atomic.Pointer[OutboundCredential] even when nil, got none")
	}
}

// TestScan_CatchesChanOfCredential is the RV-PRH-I1 security review M5
// evasion #2.
func TestScan_CatchesChanOfCredential(t *testing.T) {
	type badAdapter struct {
		ch chan providercred.OutboundCredential
	}
	v := &badAdapter{ch: make(chan providercred.OutboundCredential, 1)}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a chan of OutboundCredential, got none")
	}
}

// TestScan_CatchesPointerReceiverAuthenticator is the RV-PRH-I1 security
// review M5 evasion #3: a type whose Authenticate/RedactionValues methods
// have POINTER receivers does not itself satisfy httpclient.Authenticator
// (only *T does) - a naive t.Implements(authenticatorType) check misses it
// entirely for a value-typed field.
func TestScan_CatchesPointerReceiverAuthenticator(t *testing.T) {
	type badAdapter struct {
		auth fakePointerReceiverAuthenticator
	}
	v := &badAdapter{auth: fakePointerReceiverAuthenticator{apiKey: "x"}}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for a pointer-receiver Authenticator held by value, got none")
	}
}

type fakePointerReceiverAuthenticator struct{ apiKey string }

func (a *fakePointerReceiverAuthenticator) Authenticate(*http.Request) error { return nil }
func (a *fakePointerReceiverAuthenticator) RedactionValues() []string        { return nil }

// TestScan_CatchesPointerReceiverAuthenticatorInMap is the RV-PRH-I1
// security re-verification 1 M5 residual: the pointer-receiver check used
// to be gated on rv.CanAddr(), but reflect.PointerTo(t).Implements needs no
// addressability at all - it is a pure type-level check. A value-typed
// adapter stored in a map[string]any or interface{} is NOT addressable
// (exactly the shape paymentsAdapters() returns, per the review), so the
// old gate let a pointer-receiver Authenticator held by VALUE inside such a
// container slip through undetected. This is the review's own probe:
// map[string]any{"p": valueAdapter{...}} used to report 0 violations; the
// same value behind a pointer already reported 1 (TestScan_
// CatchesPointerReceiverAuthenticator, above).
func TestScan_CatchesPointerReceiverAuthenticatorInMap(t *testing.T) {
	type valueAdapter struct {
		auth fakePointerReceiverAuthenticator
	}
	bundle := map[string]any{"p": valueAdapter{auth: fakePointerReceiverAuthenticator{apiKey: "x"}}}
	if got := Scan(bundle); len(got) == 0 {
		t.Fatal("expected a violation for a pointer-receiver Authenticator held by value inside a map, got none")
	}
}

// TestScan_CatchesUnsafePointer is the RV-PRH-I1 security review M5
// evasion note: unsafe.Pointer is not walked by a naive type-based scan at
// all - its target type is unrecoverable via reflection, so it is flagged
// unconditionally.
func TestScan_CatchesUnsafePointer(t *testing.T) {
	type badAdapter struct {
		p unsafe.Pointer
	}
	x := 1
	v := &badAdapter{p: unsafe.Pointer(&x)}
	if got := Scan(v); len(got) == 0 {
		t.Fatal("expected a violation for an unsafe.Pointer field, got none")
	}
}

func TestScan_CleanValueProducesNoViolations(t *testing.T) {
	type cleanAdapter struct {
		ID     string
		Amount int64
		Tags   []string
	}
	v := &cleanAdapter{ID: "p", Amount: 1, Tags: []string{"a", "b"}}
	if got := Scan(v); len(got) != 0 {
		t.Fatalf("expected no violations for a clean value, got %v", got)
	}
}
