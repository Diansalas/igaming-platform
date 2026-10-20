package withdrawal

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
)

// lic11 / lic17 (M-2): every licence validity maps to exactly one cause, none (valid) or UNKNOWN (the row is not visible / not
// bound: a readability statement, never licence_missing). CHANGED ASSERTION (lic17 M-2, intentional): LicenceNotFound and the
// LicensingInvalid chain were licence_missing before; they are now "unknown" (fail closed with a policy, allow+unknown without).
func TestLic11_ValidityCause_Mapping(t *testing.T) {
	for _, c := range []struct {
		validity jurisdiction.LicenceValidity
		want     RestrictionCause
		unknown  bool
	}{
		{jurisdiction.LicenceValid, "", false},
		{jurisdiction.LicenceSuspended, CauseLicenceSuspended, false},
		{jurisdiction.LicenceStatusExpired, CauseLicenceExpired, false},
		{jurisdiction.LicenceDateExpired, CauseLicenceExpired, false},
		{jurisdiction.LicenceNotYetIssued, CauseLicenceNotYetIssued, false},
		{jurisdiction.LicenceNotFound, "", true},
		{jurisdiction.LicenceNotBound, "", true},
	} {
		got, unk, err := validityCause(c.validity)
		if err != nil || got != c.want || unk != c.unknown {
			t.Fatalf("%s: want %q unknown=%v, got %q unknown=%v err=%v", c.validity, c.want, c.unknown, got, unk, err)
		}
	}
	if _, _, err := validityCause("revoked"); !errors.Is(err, ErrRestrictionUnknownLicenceState) {
		t.Fatalf("an unknown licence state must fail closed, got %v", err)
	}
}

// lic17 F-4 static pin: validityCause covers EVERY jurisdiction.LicenceValidity constant declared in licence_validity.go, and the
// licences.status vocabulary the validity evaluation maps (active / suspended / expired) is the only one any migration declares.
func TestLic17_Static_ValidityCauseCoversEveryValidityAndStatus(t *testing.T) {
	src := readSrc(t, "internal/jurisdiction/licence_validity.go")
	consts := regexp.MustCompile(`(?m)^\s+Licence\w+\s+LicenceValidity = "([a-z_]+)"`).FindAllStringSubmatch(src, -1)
	if len(consts) < 7 {
		t.Fatalf("expected the LicenceValidity constants, found %d", len(consts))
	}
	for _, m := range consts {
		if _, _, err := validityCause(jurisdiction.LicenceValidity(m[1])); err != nil {
			t.Errorf("validityCause does not cover %q: %v", m[1], err)
		}
	}
	for _, st := range []string{`case "suspended":`, `case "expired":`, `case "active":`} {
		if !strings.Contains(src, st) {
			t.Errorf("EvaluateLicenceValidity no longer maps %s", st)
		}
	}
	ents, err := os.ReadDir(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	statusDecl := regexp.MustCompile(`(?is)(CREATE TABLE licences\b.*?status[^,]*CHECK \(status IN \(([^)]*)\)\))|(ALTER TABLE licences[^;]*status[^;]*CHECK[^;]*)`)
	found := 0
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		b, err := os.ReadFile(filepath.Join("..", "..", "migrations", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range statusDecl.FindAllStringSubmatch(string(b), -1) {
			found++
			if m[2] != "" && strings.ReplaceAll(m[2], " ", "") != "'active','suspended','expired'" {
				t.Errorf("%s declares a licences.status vocabulary %s the restriction policy does not map", e.Name(), m[2])
			}
			if m[3] != "" {
				t.Errorf("%s alters the licences.status CHECK: re-check validityCause and EvaluateLicenceValidity", e.Name())
			}
		}
	}
	if found == 0 {
		t.Fatal("the licences.status declaration was not found (the scan is vacuous)")
	}
}

// lic17 F-1: the reliance -> causes mapping. A tenant with NO licence and NO basis keeps licence_missing; a documented
// no-licence basis alone contributes nothing; every relied licence counts (a suspended basis licence with a valid tenant
// licence yields licence_suspended); an unreadable licence makes the whole reliance unknown.
func TestLic17_ReliedCauses(t *testing.T) {
	id := func() uuid.UUID { return uuid.New() }
	causes, unk, err := reliedCauses(licenceReliance{})
	if err != nil || unk || len(causes) != 1 || causes[CauseLicenceMissing] == "" {
		t.Fatalf("no licence, no basis: licence_missing, got %v %v %v", causes, unk, err)
	}
	causes, unk, err = reliedCauses(licenceReliance{hasBasis: true})
	if err != nil || unk || len(causes) != 0 {
		t.Fatalf("documented basis only: no cause, got %v %v %v", causes, unk, err)
	}
	causes, unk, err = reliedCauses(licenceReliance{hasBasis: true, licences: []reliedLicence{
		{id(), jurisdiction.LicenceValid}, {id(), jurisdiction.LicenceSuspended}, {id(), jurisdiction.LicenceDateExpired}}})
	if err != nil || unk || len(causes) != 2 || causes[CauseLicenceSuspended] != "suspended" || causes[CauseLicenceExpired] != "date_expired" {
		t.Fatalf("every relied licence counts, got %v %v %v", causes, unk, err)
	}
	if _, unk, _ = reliedCauses(licenceReliance{licences: []reliedLicence{{id(), jurisdiction.LicenceNotFound}}}); !unk {
		t.Fatal("an unreadable licence is unknown")
	}
	if _, unk, _ = reliedCauses(licenceReliance{unknown: true}); !unk {
		t.Fatal("an unreadable tenant licence binding is unknown (never licence_missing)")
	}
	if _, _, err = reliedCauses(licenceReliance{licences: []reliedLicence{{id(), "mystery"}}}); err == nil {
		t.Fatal("an unmapped validity is an error")
	}
	// Strictest-wins happens in combine, over causes evaluated in the fixed ConfigurableCauses order.
	d := combine(RestrictionStageDispatch, 3, "valid", []effective{
		{cause: CauseLicenceSuspended, state: "suspended", action: RestrictionHoldForReview, source: RestrictionSourceConfigured},
		{cause: CauseLicenceExpired, state: "date_expired", action: RestrictionBlock, source: RestrictionSourceConfigured},
	})
	if d.Action != RestrictionBlock || d.DecidingCause != CauseLicenceExpired || d.LicenceState != "date_expired" {
		t.Fatalf("strictest wins with its own licence state: %+v", d)
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
