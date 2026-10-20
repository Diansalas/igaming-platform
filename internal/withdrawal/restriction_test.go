package withdrawal

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// lic11: every licence state maps to exactly one cause (or none for a valid licence); an unknown state fails closed.
func TestLic11_LicenceCause_Mapping(t *testing.T) {
	for _, c := range []struct {
		chain    jurisdiction.LicensingOutcome
		validity jurisdiction.LicenceValidity
		want     RestrictionCause
	}{
		{jurisdiction.LicensingResolved, jurisdiction.LicenceValid, ""},
		{jurisdiction.LicensingResolved, jurisdiction.LicenceSuspended, CauseLicenceSuspended},
		{jurisdiction.LicensingResolved, jurisdiction.LicenceStatusExpired, CauseLicenceExpired},
		{jurisdiction.LicensingResolved, jurisdiction.LicenceDateExpired, CauseLicenceExpired},
		{jurisdiction.LicensingResolved, jurisdiction.LicenceNotYetIssued, CauseLicenceNotYetIssued},
		{jurisdiction.LicensingResolved, jurisdiction.LicenceNotFound, CauseLicenceMissing},
		{jurisdiction.LicensingResolved, jurisdiction.LicenceNotBound, CauseLicenceMissing},
		{jurisdiction.LicensingNoLicence, "", CauseLicenceMissing},
		{jurisdiction.LicensingInvalid, "", CauseLicenceMissing},
	} {
		got, state, err := licenceCause(c.chain, c.validity)
		if err != nil || got != c.want || state == "" {
			t.Fatalf("%s/%s: want %q, got %q (state %q, err %v)", c.chain, c.validity, c.want, got, state, err)
		}
	}
	if _, _, err := licenceCause(jurisdiction.LicensingResolved, "revoked"); !errors.Is(err, ErrRestrictionUnknownLicenceState) {
		t.Fatalf("an unknown licence state must fail closed, got %v", err)
	}
	if _, _, err := licenceCause("mystery", jurisdiction.LicenceValid); !errors.Is(err, ErrRestrictionUnknownLicenceState) {
		t.Fatalf("an unknown chain outcome must fail closed, got %v", err)
	}
}

func TestLic11_ActionRank_IsTotalAndStrict(t *testing.T) {
	if !(RestrictionAllow.Rank() < RestrictionHoldForReview.Rank() && RestrictionHoldForReview.Rank() < RestrictionBlock.Rank()) {
		t.Fatal("allow < hold_for_review < block")
	}
	if RestrictionAction("override").Valid() || RestrictionAction("").Valid() {
		t.Fatal("an unknown action is never valid")
	}
}

// combine: no cause -> allow / none; the strictest wins; a configured source wins a tie (so a configured refusal is the one
// recorded); the set version is always carried.
func TestLic11_Combine(t *testing.T) {
	d := combine(RestrictionStageRequest, 0, "valid", nil)
	if d.Action != RestrictionAllow || d.Source != RestrictionSourceNone || d.DecidingCause != "" || d.Refuses() {
		t.Fatalf("no cause: %+v", d)
	}
	pid := uuid.New()
	d = combine(RestrictionStageDispatch, 7, "suspended", []effective{
		{cause: CauseLicenceSuspended, action: RestrictionAllow, source: RestrictionSourceDefault},
		{cause: CauseLicenceExpired, action: RestrictionBlock, source: RestrictionSourceConfigured, policyID: &pid, version: 7},
	})
	if d.Action != RestrictionBlock || d.DecidingCause != CauseLicenceExpired || d.Source != RestrictionSourceConfigured || d.SetVersion != 7 || !d.Refuses() {
		t.Fatalf("strictest must win: %+v", d)
	}
	d = combine(RestrictionStageDispatch, 3, "x", []effective{
		{cause: CauseLicenceSuspended, action: RestrictionAllow, source: RestrictionSourceDefault},
		{cause: CauseLicenceMissing, action: RestrictionAllow, source: RestrictionSourceConfigured, policyID: &pid, version: 3},
	})
	if d.DecidingCause != CauseLicenceMissing || d.Source != RestrictionSourceConfigured || d.Refuses() {
		t.Fatalf("a configured source wins a tie: %+v", d)
	}
	if len(d.Causes) != 2 {
		t.Fatalf("every cause in force is listed: %v", d.Causes)
	}
}

// The WD056 dispatch denial carries the cause as its reason (the closed alert vocabulary) and the provenance.
func TestLic11_DispatchDenial_Shape(t *testing.T) {
	pid := uuid.New()
	d := combine(RestrictionStageRedispatch, 2, "suspended", []effective{
		{cause: CauseLicenceSuspended, action: RestrictionHoldForReview, source: RestrictionSourceConfigured, policyID: &pid, version: 2},
	})
	g := d.DispatchDenial()
	if g.Allowed || g.GateCode != withdrawalgate.CodeRestriction || withdrawalgate.CodeRestriction != "WD056" || g.Reason != string(CauseLicenceSuspended) {
		t.Fatalf("denial shape: %+v", g)
	}
	if p := g.Restriction; p == nil || p.Source != RestrictionSourceConfigured || p.SetVersion != 2 || p.Cause != string(CauseLicenceSuspended) || p.Action != string(RestrictionHoldForReview) {
		t.Fatalf("provenance: %+v", g.Restriction)
	}
	if !decisionReasonShape.MatchString(g.Reason) {
		t.Fatal("the reason must satisfy the decision-row reason shape")
	}
	md := d.AuditMetadata()
	for _, k := range []string{"restriction_stage", "restriction_action", "restriction_policy_source", "restriction_policy_set_version", "restriction_cause", "restriction_policy_id", "restriction_policy_version", "restriction_licence_state"} {
		if _, ok := md[k]; !ok {
			t.Fatalf("audit metadata lacks %s: %v", k, md)
		}
	}
	// Never anything destination- or person-shaped (the B13-B audit regex of withdrawal.requested).
	for k := range md {
		for _, bad := range []string{"fingerprint", "iban", "mask", "label"} {
			if strings.Contains(k, bad) {
				t.Fatalf("audit key %q contains %q", k, bad)
			}
		}
	}
}

func TestLic11_ConfigurableCauses_Closed(t *testing.T) {
	if len(ConfigurableCauses) != 4 {
		t.Fatalf("exactly the four licence causes are configurable, got %v", ConfigurableCauses)
	}
	for _, c := range []RestrictionCause{"tenant_suspended", "tenant_closed", "brand_suspended", "brand_closed", "market_closed", ""} {
		if IsConfigurableCause(c) {
			t.Fatalf("%q must not be configurable (owner decision / not evaluable)", c)
		}
	}
}

func readSrc(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Placement pins (ADR 0082 / H-SEC-11 precedence): in RequestWithdrawal the policy runs AFTER the replay lookup and the
// H-SEC-11 status gate (an unchanged refusal keeps its precedence) and BEFORE the WD-RG-1 gate; in EvaluateDispatchGate it
// runs after the nil-gate check and before gate.Evaluate, and a refusal returns before the gate is consulted.
func TestLic11_Static_Placement(t *testing.T) {
	src := readSrc(t, "internal/withdrawal/withdrawal.go")
	body := src[strings.Index(src, "func RequestWithdrawal("):]
	body = body[:strings.Index(body, "\nfunc ")]
	replay := strings.Index(body, "getByTenantPlayerIdempotencyKey(")
	status := strings.Index(body, "tenant.RequireActiveForPaymentInitiation(")
	policy := strings.Index(body, "evaluateRequestRestriction(")
	rg := strings.Index(body, "evaluateRGRiskGate(")
	insert := strings.Index(body, "INSERT INTO withdrawal_requests")
	if replay < 0 || status < 0 || policy < 0 || rg < 0 || !(replay < status && status < policy && policy < rg && rg < insert) {
		t.Fatalf("RequestWithdrawal order must be replay(%d) < H-SEC-11(%d) < restriction policy(%d) < WD-RG-1(%d) < insert(%d)", replay, status, policy, rg, insert)
	}
	dg := readSrc(t, "internal/withdrawal/dispatchgate.go")
	fn := dg[strings.Index(dg, "func EvaluateDispatchGate("):]
	fn = fn[:strings.Index(fn, "\n}\n")]
	nilCheck := strings.Index(fn, "if gate == nil")
	eval := strings.Index(fn, "EvaluateRestriction(")
	refuse := strings.Index(fn, "return rd.DispatchDenial(), nil")
	gate := strings.Index(fn, "gate.Evaluate(")
	if nilCheck < 0 || eval < 0 || refuse < 0 || gate < 0 || !(nilCheck < eval && eval < refuse && refuse < gate) {
		t.Fatalf("EvaluateDispatchGate order must be nil gate(%d) < policy(%d) < refusal return(%d) < gate(%d)", nilCheck, eval, refuse, gate)
	}
	// The provenance is set by us after the gate (a Gate implementation can never forge it).
	if strings.Index(fn, "d.Restriction = rd.Provenance()") < gate {
		t.Fatal("the restriction provenance must be assigned after the gate returns")
	}
}
