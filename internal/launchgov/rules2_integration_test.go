//go:build integration

package launchgov_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
)

// The recorded fields are stored as given; free text is PII-linted before any SQL.
func TestRequest_RecordsLicensingOperatorJurisdictionsAndNote_PIILinted(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	in := w.activation(sub, jid)
	r := w.mustRequest(w.req, in)
	got, _, err := w.svc.GetRequest(ctx, w.ap1, tid, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	acks := append([]string(nil), in.AcknowledgedCheckCodes...)
	sort.Strings(acks)
	if got.LicensingModel == nil || *got.LicensingModel != launchgov.LicModelOther || got.LicensingStatus == nil || *got.LicensingStatus != launchgov.LicStatusConditional ||
		got.DeterminationReference == nil || *got.DeterminationReference != "ext-ref-001" || got.DeterminationByRole == nil || *got.DeterminationByRole != "external counsel" ||
		got.ResponsibleOperatorName == nil || *got.ResponsibleOperatorName != "Operator Ltd" || got.ResponsibleOperatorRegistration != "REG-4711" ||
		len(got.JurisdictionIDs) != 1 || got.JurisdictionIDs[0] != jid || got.ConditionsNote != in.ConditionsNote ||
		strings.Join(got.AcknowledgedCheckCodes, ",") != strings.Join(acks, ",") {
		t.Fatalf("recorded fields: %+v", got)
	}
	if _, err := w.svc.Cancel(ctx, w.req, sub, r.ID, "requester_withdrew"); err != nil {
		t.Fatal(err)
	}

	for name, mut := range map[string]func(*launchgov.RequestInput){
		"email in the note":            func(x *launchgov.RequestInput) { x.ConditionsNote = "ask jane@example.com" },
		"phone in the note":            func(x *launchgov.RequestInput) { x.ConditionsNote = "call +49 30 1234567" },
		"long number in the reference": func(x *launchgov.RequestInput) { x.Licensing.DeterminationReference = "case 123456789012" },
		"wallet in the operator name": func(x *launchgov.RequestInput) {
			x.ResponsibleOperatorName = "0x52908400098527886E0F7030069857D2E4169EE7"
		},
		"card in the registration": func(x *launchgov.RequestInput) { x.ResponsibleOperatorRegistration = "4111 1111 1111 1111" },
		"note over 2000 bytes":     func(x *launchgov.RequestInput) { x.ConditionsNote = strings.Repeat("a", 2001) },
		"unknown reason code":      func(x *launchgov.RequestInput) { x.ReasonCode = "because" },
		"licensing on a close":     func(x *launchgov.RequestInput) { x.Action, x.ReasonCode = launchgov.ActionClose, "offboarding" },
	} {
		bad := in
		lic := *in.Licensing
		bad.Licensing = &lic
		mut(&bad)
		if _, err := w.svc.Request(ctx, w.req, bad); !errors.Is(err, launchgov.ErrInvalidInput) {
			t.Fatalf("%s must be refused as invalid input: %v", name, err)
		}
	}
	if n := w.countRequests(sub); n != 1 { // only the (cancelled) valid one
		t.Fatalf("refused inputs must store nothing: %d requests", n)
	}
	// The suspension's note is linted too.
	act := w.activeBrand(tid)
	if _, err := w.svc.Suspend(ctx, w.req, launchgov.SuspendInput{Subject: launchgov.Subject{TenantID: tid, BrandID: &act}, ReasonCode: "operator_request", ConditionsNote: "mail a@b.co"}); !errors.Is(err, launchgov.ErrInvalidInput) {
		t.Fatalf("PII in a suspension note: %v", err)
	}
}

// The carry-over is for REJECTED requests only: an identical request after a cancellation is fine.
func TestRejectCarryOver_OnlyRejectedRequestsCarryOver(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	jid := w.jurisdiction(tid)
	brand := w.pendingBrand(tid)
	sub := launchgov.Subject{TenantID: tid, BrandID: &brand}
	in := w.activation(sub, jid)
	r1 := w.mustRequest(w.req, in)
	if _, err := w.svc.Cancel(ctx, w.req, sub, r1.ID, "requester_withdrew"); err != nil {
		t.Fatal(err)
	}
	r2, err := w.svc.Request(ctx, w.req, in)
	if err != nil {
		t.Fatalf("an identical request after a cancellation must be accepted: %v", err)
	}
	// ... and after an EXECUTED one the subject has moved on (illegal, not a carry-over).
	if _, err := w.approve(w.ap1, r2.Request); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Request(ctx, w.req, in); !errors.Is(err, launchgov.ErrIllegalTransition) || errors.Is(err, launchgov.ErrPriorReject) {
		t.Fatalf("an identical activation of an active brand: %v", err)
	}
}

// The verifier's launch family, called directly as igaming_runtime (it is EXECUTE-granted): the
// scope / operation matrix of ADR 0112 6.4, independent of the table guards that sit in front of it.
func TestVerifier_LaunchFamily_ScopeOperationMatrix(t *testing.T) {
	w := newWorld(t)
	f := w.forger()
	tid := w.activeTenant()
	actor := w.ops.Approver1
	hash := strings.Repeat("ab", 32)
	target := uuid.NewString()
	call := func(scope string, tenant *uuid.UUID, op, tg string, tokScope string, tokTenant uuid.UUID) error {
		return w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			if err := setProof(ctx, tx, f.Valid(t, actor, tokScope, tokTenant, op, tg, hash)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `SELECT actor_proof_require($1, $2, $3, $4, $5, $6)`, actor, scope, tenant, op, tg, hash)
			return err
		})
	}
	cases := []struct {
		name         string
		scope        string
		tenant       *uuid.UUID
		op, tg       string
		tokScope     string
		tokTenant    uuid.UUID
		wantAccepted bool
	}{
		{"platform request", "platform", nil, "launch_authorisation:request", "new", "platform", uuid.Nil, true},
		{"platform suspend", "platform", nil, "launch_authorisation:suspend", "new", "platform", uuid.Nil, true},
		{"platform cancel", "platform", nil, "launch_authorisation:cancel", target, "platform", uuid.Nil, true},
		{"platform approve", "platform", nil, "launch_authorisation:approve", target, "platform", uuid.Nil, true},
		{"platform reject", "platform", nil, "launch_authorisation:reject", target, "platform", uuid.Nil, true},
		{"tenant request", "tenant", &tid, "launch_authorisation:request", "new", "tenant", tid, true},
		{"tenant suspend", "tenant", &tid, "launch_authorisation:suspend", "new", "tenant", tid, true},
		{"tenant cancel", "tenant", &tid, "launch_authorisation:cancel", target, "tenant", tid, true},
		{"tenant approve (never)", "tenant", &tid, "launch_authorisation:approve", target, "tenant", tid, false},
		{"tenant reject (never)", "tenant", &tid, "launch_authorisation:reject", target, "tenant", tid, false},
		{"platform_acting request (never)", "platform_acting", &tid, "launch_authorisation:request", "new", "platform_acting", tid, false},
		{"platform_acting approve (never)", "platform_acting", &tid, "launch_authorisation:approve", target, "platform_acting", tid, false},
		{"platform with a tenant", "platform", &tid, "launch_authorisation:request", "new", "platform", tid, false},
		{"unknown launch operation", "platform", nil, "launch_authorisation:execute", "new", "platform", uuid.Nil, false},
		{"unknown launch operation 2", "platform", nil, "launch_authorisation:ratify", "new", "platform", uuid.Nil, false},
		{"platform scope for a non-governed family stays refused", "platform", nil, "ledger_adjustment:initiate", "new", "platform", uuid.Nil, false},
		{"hold resolution stays platform_acting only", "platform", nil, "withdrawal_hold_resolution:request", "new", "platform", uuid.Nil, false},
		{"K1 platform family unchanged", "platform", nil, "capability_grant:approve", target, "platform", uuid.Nil, true},
	}
	for _, c := range cases {
		err := call(c.scope, c.tenant, c.op, c.tg, c.tokScope, c.tokTenant)
		if c.wantAccepted && err != nil {
			t.Errorf("%s must be accepted: %v", c.name, err)
		}
		if !c.wantAccepted && pgCode(err) != "AP004" {
			t.Errorf("%s must be refused with AP004, got %v", c.name, err)
		}
	}
}
