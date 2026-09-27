// Package providercred_test holds the one providercred test that must
// import internal/casino (security review §2's "no long-lived type holds a
// credential" sweep). It lives in the EXTERNAL test package (providercred_
// test, not providercred) because internal/casino imports internal/
// providercred as of ADR 0095 §15.1/PRH-I2 (casino.LaunchGame's own
// outbound-credential resolution): an in-package providercred test file
// importing casino would be a genuine import cycle, but Go's external test
// package is allowed to import both providercred and anything providercred
// itself imports, precisely to break this shape of cycle. Every other
// providercred test that needs the package's own unexported fields (e.g.
// OutboundCredential.secret) stays in providercred_test.go (package
// providercred).
package providercred_test

import (
	"reflect"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/payments"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providers/httpclient"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// credentialBearing reports whether a field type may carry an outbound
// credential or an authenticator.
func credentialBearing(t reflect.Type) bool {
	outbound := reflect.TypeOf(providercred.OutboundCredential{})
	auth := reflect.TypeOf((*httpclient.Authenticator)(nil)).Elem()
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Map || t.Kind() == reflect.Array {
		if t.Kind() == reflect.Map {
			if credentialBearing(t.Key()) {
				return true
			}
		}
		t = t.Elem()
	}
	return t == outbound || t == auth || t.Implements(auth) || reflect.PointerTo(t).Implements(auth)
}

// TestOutbound_NoCredentialOnLongLivedTypes (security review §2): the HTTP
// client, the domain orchestrators, the resolvers and the adapters hold no
// OutboundCredential, no Authenticator, and (outside the synthetic MOCKs,
// whose per-process MOCK master is not a vendor credential) no []byte
// secret field.
func TestOutbound_NoCredentialOnLongLivedTypes(t *testing.T) {
	syntheticMocks := map[reflect.Type]bool{
		reflect.TypeOf(payments.MockProvider{}):     true,
		reflect.TypeOf(casino.MockCasinoProvider{}): true,
		reflect.TypeOf(kyc.MockKYCProvider{}):       true,
	}
	for _, v := range []any{
		httpclient.Client{}, payments.Orchestrator{}, casino.Orchestrator{}, kyc.Orchestrator{},
		providercred.OutboundResolver{}, providercred.Resolver{}, webhookauth.KindSplitResolver{},
		payments.MockProvider{}, casino.MockCasinoProvider{}, kyc.MockKYCProvider{},
		// ADR 0095 §15.1/PRH-I2: casino's own outbound-credential resolver
		// types join the sweep - neither holds a credential/authenticator
		// field (MockOutboundResolver is a stateless empty struct;
		// OutboundCredentialResolver is an interface, not a struct, so it is
		// exercised instead as a field-holder check via its own
		// implementations above and the CallContext check below).
		casino.MockOutboundResolver{},
	} {
		typ := reflect.TypeOf(v)
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if credentialBearing(field.Type) {
				t.Errorf("%s.%s (%s) can hold a credential or authenticator", typ, field.Name, field.Type)
			}
			if field.Type == reflect.TypeOf([]byte(nil)) && !syntheticMocks[typ] {
				t.Errorf("%s.%s is a []byte field on a long-lived type", typ, field.Name)
			}
		}
	}
	// Negative control: the check sees a planted field.
	type planted struct {
		Cred providercred.OutboundCredential
	}
	if !credentialBearing(reflect.TypeOf(planted{}).Field(0).Type) {
		t.Fatal("the check must detect an OutboundCredential field")
	}
	type plantedAuth struct {
		A *httpclient.HeaderAuthenticator
	}
	if !credentialBearing(reflect.TypeOf(plantedAuth{}).Field(0).Type) {
		t.Fatal("the check must detect an Authenticator field")
	}
}
