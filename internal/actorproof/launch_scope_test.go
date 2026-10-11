package actorproof

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// ADR 0112 6.4 (migration 0130): launch_authorisation:* is provable for scope tenant or platform
// (NULL tenant on the wire) only; approve / reject for scope platform only; never platform_acting;
// the operation set is closed. The Go signer refuses every other shape before the database does.
func TestSign_LaunchAuthorisation_ScopeMatrix(t *testing.T) {
	i := newTestIssuer(t)
	tenant := uuid.New()
	hash := strings.Repeat("ab", 32)
	target := uuid.NewString()
	type tc struct {
		op     string
		scope  string
		tenant uuid.UUID
		ok     bool
	}
	cases := []tc{
		{OpLaunchRequest, ScopePlatform, uuid.Nil, true},
		{OpLaunchRequest, ScopeTenant, tenant, true},
		{OpLaunchSuspend, ScopePlatform, uuid.Nil, true},
		{OpLaunchSuspend, ScopeTenant, tenant, true},
		{OpLaunchCancel, ScopePlatform, uuid.Nil, true},
		{OpLaunchCancel, ScopeTenant, tenant, true},
		{OpLaunchApprove, ScopePlatform, uuid.Nil, true},
		{OpLaunchReject, ScopePlatform, uuid.Nil, true},
		// approve / reject are never tenant
		{OpLaunchApprove, ScopeTenant, tenant, false},
		{OpLaunchReject, ScopeTenant, tenant, false},
		// never platform_acting
		{OpLaunchRequest, ScopePlatformActing, tenant, false},
		{OpLaunchSuspend, ScopePlatformActing, tenant, false},
		{OpLaunchApprove, ScopePlatformActing, tenant, false},
		// platform scope carries no tenant; tenant scope needs one
		{OpLaunchRequest, ScopePlatform, tenant, false},
		{OpLaunchRequest, ScopeTenant, uuid.Nil, false},
		// the operation set is closed
		{"launch_authorisation:execute", ScopePlatform, uuid.Nil, false},
		{"launch_authorisation:ratify", ScopePlatform, uuid.Nil, false},
	}
	for n, c := range cases {
		tg := target
		if c.op == OpLaunchRequest || c.op == OpLaunchSuspend {
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
	if OpLaunchRequest != "launch_authorisation:request" || OpLaunchSuspend != "launch_authorisation:suspend" || OpLaunchCancel != "launch_authorisation:cancel" ||
		OpLaunchApprove != "launch_authorisation:approve" || OpLaunchReject != "launch_authorisation:reject" {
		t.Fatal("operation strings drifted from migration 0130")
	}
}

func TestLaunchRequestDigest_BindsEveryField_OrderIndependentArrays(t *testing.T) {
	brand := uuid.New()
	tenant := uuid.New()
	j1, j2 := uuid.New(), uuid.New()
	base := func() LaunchRequest {
		m, st, op := "other_manually_approved", "conditional", "operator"
		ref, role := "ref-1", "external counsel"
		return LaunchRequest{TenantID: tenant, SubjectKind: "brand", BrandID: &brand, Action: "activate",
			LicensingModel: &m, LicensingStatus: &st, DeterminationReference: &ref, DeterminationByRole: &role,
			ResponsibleOperatorName: &op, ResponsibleOperatorRegistration: "reg", JurisdictionIDs: []uuid.UUID{j1, j2},
			ConditionsNote: "note", ReadinessSnapshotHash: strings.Repeat("0", 64), AcknowledgedCheckCodes: []string{"R-B", "R-A"}, ReasonCode: "r"}
	}
	a := base()
	b := base()
	b.JurisdictionIDs = []uuid.UUID{j2, j1}
	b.AcknowledgedCheckCodes = []string{"R-A", "R-B"}
	if a.Digest() != b.Digest() {
		t.Fatal("array order must not change the digest")
	}
	mut := map[string]func(r *LaunchRequest){
		"tenant":  func(r *LaunchRequest) { r.TenantID = uuid.New() },
		"kind":    func(r *LaunchRequest) { r.SubjectKind = "tenant" },
		"brand":   func(r *LaunchRequest) { x := uuid.New(); r.BrandID = &x },
		"action":  func(r *LaunchRequest) { r.Action = "reactivate" },
		"reason":  func(r *LaunchRequest) { r.ReasonCode = "r2" },
		"note":    func(r *LaunchRequest) { r.ConditionsNote = "note2" },
		"snap":    func(r *LaunchRequest) { r.ReadinessSnapshotHash = strings.Repeat("1", 64) },
		"ack":     func(r *LaunchRequest) { r.AcknowledgedCheckCodes = []string{"R-A"} },
		"juris":   func(r *LaunchRequest) { r.JurisdictionIDs = []uuid.UUID{j1} },
		"reg":     func(r *LaunchRequest) { r.ResponsibleOperatorRegistration = "reg2" },
		"model":   func(r *LaunchRequest) { m := "tenant_licence"; r.LicensingModel = &m },
		"licence": func(r *LaunchRequest) { x := uuid.New(); r.LicenceID = &x },
	}
	for name, f := range mut {
		c := base()
		f(&c)
		if c.Digest() == a.Digest() {
			t.Fatalf("digest does not bind %s", name)
		}
	}
}
