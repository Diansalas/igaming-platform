package actorproof

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Migration 0177 (MKT-DUAL-1): licensing_change:* is provable for scope platform (NULL tenant on the wire) or, for request
// and cancel only, scope tenant; approve / reject / execute for scope platform only; never platform_acting or system; the
// operation set is closed. The Go signer refuses every other shape before the database does.
func TestSign_LicensingChange_ScopeMatrix(t *testing.T) {
	i := newTestIssuer(t)
	tenant := uuid.New()
	hash := strings.Repeat("cd", 32)
	target := uuid.NewString()
	cases := []struct {
		op     string
		scope  string
		tenant uuid.UUID
		ok     bool
	}{
		{OpLicensingChangeRequest, ScopePlatform, uuid.Nil, true},
		{OpLicensingChangeRequest, ScopeTenant, tenant, true},
		{OpLicensingChangeCancel, ScopePlatform, uuid.Nil, true},
		{OpLicensingChangeCancel, ScopeTenant, tenant, true},
		{OpLicensingChangeApprove, ScopePlatform, uuid.Nil, true},
		{OpLicensingChangeReject, ScopePlatform, uuid.Nil, true},
		{OpLicensingChangeExecute, ScopePlatform, uuid.Nil, true},
		// decisions and execution are never tenant
		{OpLicensingChangeApprove, ScopeTenant, tenant, false},
		{OpLicensingChangeReject, ScopeTenant, tenant, false},
		{OpLicensingChangeExecute, ScopeTenant, tenant, false},
		// never platform_acting, never system
		{OpLicensingChangeRequest, ScopePlatformActing, tenant, false},
		{OpLicensingChangeApprove, ScopePlatformActing, tenant, false},
		{OpLicensingChangeExecute, ScopeSystem, tenant, false},
		// platform scope carries no tenant; tenant scope needs one
		{OpLicensingChangeRequest, ScopePlatform, tenant, false},
		{OpLicensingChangeRequest, ScopeTenant, uuid.Nil, false},
		// the operation set is closed
		{"licensing_change:apply", ScopePlatform, uuid.Nil, false},
		{"licensing_change:suspend", ScopePlatform, uuid.Nil, false},
	}
	for n, c := range cases {
		tg := target
		if c.op == OpLicensingChangeRequest {
			tg = TargetNew
		}
		_, err := i.Sign(Claims{Actor: uuid.New(), Scope: c.scope, Tenant: c.tenant, Operation: c.op, Target: tg, PayloadHash: hash})
		if c.ok && err != nil {
			t.Fatalf("case %d %+v must sign: %v", n, c, err)
		}
		if !c.ok && !errors.Is(err, ErrInvalidClaims) {
			t.Fatalf("case %d %+v must be refused, got %v", n, c, err)
		}
	}
	if OpLicensingChangeRequest != "licensing_change:request" || OpLicensingChangeApprove != "licensing_change:approve" ||
		OpLicensingChangeReject != "licensing_change:reject" || OpLicensingChangeCancel != "licensing_change:cancel" ||
		OpLicensingChangeExecute != "licensing_change:execute" {
		t.Fatal("operation strings drifted from migration 0177")
	}
}
