package licensingchange

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// The Go kind registry is exactly the seeds of licensing_change_kinds in migrations 0177 and 0178 (a new kind needs both, plus the
// SQL classify / row-match branches: the extension hook).
func TestKinds_MatchTheMigrationRegistry(t *testing.T) {
	var sqlKinds []string
	approvals := 0
	for _, f := range []string{"0177_licensing_change_approvals.up.sql", "0178_licence_lifecycle_governed_changes.up.sql"} {
		b, err := os.ReadFile("../../migrations/" + f)
		if err != nil {
			t.Fatal(err)
		}
		seed := string(b)[strings.Index(string(b), "INSERT INTO licensing_change_kinds"):]
		seed = seed[:strings.Index(seed, ";")]
		approvals += strings.Count(seed, "2, ARRAY['platform_admin']")
		for _, m := range regexp.MustCompile(`\('([a-z_]+)', (true|false), `).FindAllStringSubmatch(seed, -1) {
			sqlKinds = append(sqlKinds, m[1])
			spec, ok := kinds[Kind(m[1])]
			if !ok {
				t.Errorf("kind %s is seeded but not registered in Go", m[1])
				continue
			}
			if spec.platformOnly != (m[2] == "true") {
				t.Errorf("kind %s: platform_only differs between SQL (%s) and Go (%v)", m[1], m[2], spec.platformOnly)
			}
		}
	}
	var goKinds []string
	for _, k := range Kinds() {
		goKinds = append(goKinds, string(k))
	}
	sort.Strings(sqlKinds)
	sort.Strings(goKinds)
	if strings.Join(sqlKinds, ",") != strings.Join(goKinds, ",") || len(goKinds) != 10 {
		t.Fatalf("Go kinds %v and SQL kinds %v must be the same ten (six from 0177, four lifecycle kinds from 0178)", goKinds, sqlKinds)
	}
	// Every SQL seed row is approved by platform_admin and needs two approvals (the interim HDR-M-1 answer).
	if approvals != 10 {
		t.Fatalf("every kind must seed widening_approvals 2 and approver_roles {platform_admin} (interim HDR-M-1), found %d", approvals)
	}
}

func TestCountApprovals_DistinctPersonsFreshAndNotTheRequester(t *testing.T) {
	now := time.Now()
	reqPerson, p1, p2 := uuid.New(), uuid.New(), uuid.New()
	req := Request{RequestedBy: uuid.New(), RequestedByPersonID: reqPerson, PayloadHash: "h"}
	ap := func(person uuid.UUID, age time.Duration, hash string) Approval {
		return Approval{ApproverID: uuid.New(), ApproverPersonID: person, PayloadHash: hash, DecidedAt: now.Add(-age)}
	}
	cases := []struct {
		name            string
		apps            []Approval
		fresh, distinct int
	}{
		{"two distinct fresh", []Approval{ap(p1, time.Minute, "h"), ap(p2, time.Minute, "h")}, 2, 2},
		{"same Person twice", []Approval{ap(p1, time.Minute, "h"), ap(p1, time.Minute, "h")}, 1, 1},
		{"requester's Person never counts", []Approval{ap(reqPerson, time.Minute, "h"), ap(p1, time.Minute, "h")}, 1, 1},
		{"stale approval", []Approval{ap(p1, 25*time.Hour, "h"), ap(p2, time.Minute, "h")}, 1, 2},
		{"other payload hash", []Approval{ap(p1, time.Minute, "other"), ap(p2, time.Minute, "h")}, 1, 1},
		{"requester's account", []Approval{{ApproverID: req.RequestedBy, ApproverPersonID: p1, PayloadHash: "h", DecidedAt: now}}, 0, 0},
	}
	for _, c := range cases {
		f, d := countApprovals(req, c.apps, now, DefaultApprovalMaxAge)
		if f != c.fresh || d != c.distinct {
			t.Errorf("%s: fresh %d distinct %d, want %d / %d", c.name, f, d, c.fresh, c.distinct)
		}
	}
}

func TestDecode_StrictShapes(t *testing.T) {
	tid, lid := uuid.New(), uuid.New()
	bad := map[Kind]string{
		KindLicenceCreate:                   `{"jurisdiction_id":"` + uuid.NewString() + `","licensee":"other","licence_number":"X"}`,
		KindTenantLicenceBind:               `{"tenant_id":"` + tid.String() + `","licence_id":"` + lid.String() + `","extra":1}`,
		KindTenantLicenceUnbind:             `{}`,
		KindLicenceCeilingVersion:           `{"licence_id":"` + lid.String() + `","country_code":"MT","state":"enabled","status":"active","authorization_reference":null}`,
		KindOperatingPolicyVersion:          `{"tenant_id":"` + tid.String() + `","scope":"brand","brand_id":null,"operation_code":null,"product_code":null,"country_code":"MT","state":"disabled","status":"active","authorization_reference":null}`,
		KindTenantJurisdictionConfigVersion: `{"tenant_id":"` + tid.String() + `","jurisdiction_id":"` + uuid.NewString() + `","action":"close","kyc_ruleset_id":"k","aml_ruleset_id":null,"rg_ruleset_id":null,"reporting_ruleset_id":null,"allowed_currencies":null,"allowed_payment_methods":null,"geo_block_list":null}`,
	}
	for k, raw := range bad {
		if _, _, err := kinds[k].decode(json.RawMessage(raw)); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s: %s must be refused, got %v", k, raw, err)
		}
	}
	// A withdrawal at the brand rung needs an authorization reference (it widens).
	bid := uuid.New()
	p, _ := json.Marshal(OperatingPolicyPayload{TenantID: tid, Scope: "brand", BrandID: &bid, CountryCode: "MT", State: "disabled", Status: "withdrawn"})
	if _, _, err := kinds[KindOperatingPolicyVersion].decode(p); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("a brand-rung withdrawal without a reference must be refused, got %v", err)
	}
	// The canonical payload carries every key (null where unused).
	good, _ := json.Marshal(TenantJurisdictionConfigPayload{TenantID: tid, JurisdictionID: lid, Action: ConfigActionClose})
	v, subject, err := kinds[KindTenantJurisdictionConfigVersion].decode(good)
	if err != nil || subject != tid {
		t.Fatalf("close: %v %v", err, subject)
	}
	out, _ := json.Marshal(v)
	for _, key := range []string{"kyc_ruleset_id", "allowed_currencies", "geo_block_list", "action"} {
		if !strings.Contains(string(out), `"`+key+`":`) {
			t.Errorf("canonical payload lacks %s: %s", key, out)
		}
	}
}

// Static authority checks happen before any SQL (no pool is touched: a nil pool would panic).
func TestRequest_StaticRefusalsNeedNoDatabase(t *testing.T) {
	s := &Service{cfg: Config{}}
	tid := uuid.New()
	p, _ := json.Marshal(TenantLicenceUnbindPayload{TenantID: tid})
	cases := []struct {
		a    Actor
		want error
	}{
		{Actor{StaffID: uuid.New(), Scope: ScopeTenant, TenantID: tid, Role: string(auth.RoleSupport)}, ErrForbidden},
		{Actor{StaffID: uuid.New(), Scope: ScopeTenant, TenantID: tid, Role: string(auth.RoleTenantAdmin)}, ErrForbidden},
		{Actor{StaffID: uuid.New(), Scope: actorproof.ScopePlatformActing, TenantID: tid, Role: string(auth.RoleTenantAdmin)}, ErrActingSession},
		{Actor{Scope: ScopePlatform, Role: string(auth.RoleTenantAdmin)}, ErrForbidden},
	}
	for i, c := range cases {
		if _, err := s.Request(context.Background(), c.a, RequestInput{Kind: KindTenantLicenceUnbind, Payload: p, ReasonCode: "x"}); !errors.Is(err, c.want) {
			t.Errorf("case %d: got %v want %v", i, err, c.want)
		}
	}
	if _, err := s.Approve(context.Background(), Actor{StaffID: uuid.New(), Scope: ScopeTenant, TenantID: tid, Role: string(auth.RoleCompliance)}, uuid.New(), "x"); !errors.Is(err, ErrForbidden) {
		t.Errorf("tenant approve: %v", err)
	}
}
