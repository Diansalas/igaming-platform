package webhookauth

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// TestReasonForResolveError_AdmissionUnavailable is security
// re-verification round 3's C1(b): a table-driven unit test on
// reasonForResolveError itself - the cheapest possible pin for N4 (the
// review's own mutation: deleting the ErrTenantReaderUnavailable case so
// it falls through to the generic credential_unavailable default,
// silently recreating the High-severity uniform-401 defect).
func TestReasonForResolveError_AdmissionUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want Reason
	}{
		{"admission capacity sentinel maps to its own reason", ErrTenantReaderUnavailable, ReasonAdmissionUnavailable},
		{"a wrapped admission capacity sentinel still maps (errors.Is, not ==)", wrapErr{ErrTenantReaderUnavailable}, ReasonAdmissionUnavailable},
		{"a genuine, unrelated DB error still folds into credential_unavailable", errFake("boom"), ReasonCredentialUnavailable},
		{"no resolver still maps to its own reason, not admission_unavailable", ErrNoResolver, ReasonNoResolver},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := reasonForResolveError(tc.err); got != tc.want {
				t.Fatalf("reasonForResolveError(%v) = %s, want %s", tc.err, got, tc.want)
			}
		})
	}
}

type wrapErr struct{ err error }

func (w wrapErr) Error() string { return "wrapped: " + w.err.Error() }
func (w wrapErr) Unwrap() error { return w.err }

type errFake string

func (e errFake) Error() string { return string(e) }

// TestResolveCredentials_AdmissionUnavailable_BothKeySelections is C1(b)'s
// second half: ResolveCredentials, in BOTH KeyFromHeader and KeyImplicit
// modes, must turn a resolver-returned ErrTenantReaderUnavailable into
// &AuthError{Reason: ReasonAdmissionUnavailable} - never
// ReasonCredentialUnavailable (the uniform 401) - independent of which
// key-selection mode the scheme declares. This is the same N4 mutation,
// pinned one layer up (ResolveCredentials, not just the bare
// reasonForResolveError helper), so a future refactor that inlines or
// reorders reasonForResolveError's callers is caught here too.
func TestResolveCredentials_AdmissionUnavailable_BothKeySelections(t *testing.T) {
	tenant := uuid.New()
	in := Inbound{TenantID: tenant, ProviderID: "vendor-a"}

	cases := []struct {
		name string
		sel  KeySelection
		m    AuthMaterial
	}{
		{"KeyFromHeader", KeyFromHeader, AuthMaterial{KeyID: "k-1"}},
		{"KeyImplicit", KeyImplicit, AuthMaterial{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := spyScheme{name: "x", props: SchemeProperties{Binding: BindingPerMerchantKey, KeySelection: tc.sel, SignedTimestamp: true, MaxSkew: MaxSkewCap, Replay: ReplayTimestampWindow}}
			resolver := &countingResolver{err: ErrTenantReaderUnavailable}
			_, authErr := ResolveCredentials(context.Background(), nil, scheme, resolver, in, tc.m)
			if authErr == nil {
				t.Fatal("expected a non-nil AuthError")
			}
			if authErr.Reason != ReasonAdmissionUnavailable {
				t.Fatalf("Reason = %s, want %s (this is N4: a DB-gate rejection must never surface as the uniform credential_unavailable 401)", authErr.Reason, ReasonAdmissionUnavailable)
			}
		})
	}
}
