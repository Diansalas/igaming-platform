package httpserver

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/residenceapproval"
	"github.com/Diansalas/igaming-platform/internal/stepup"
)

var residenceRoutes = map[string]string{
	"GET /v1/admin/tenants/{tenantID}/residence-approval-policy":                       "none",
	"POST /v1/admin/tenants/{tenantID}/residence-approval-policy":                      "none",
	"POST /v1/admin/tenants/{tenantID}/residence-approval-policy/{policyID}/approvals": "critical",
	"GET /v1/admin/kyc/residence-determinations/pending":                               "none",
	"POST /v1/admin/kyc/verifications/{id}/residence-approval":                         "critical",
}

func TestResidenceApprovalRoutes_GoldenAndRegistrationAreExact(t *testing.T) {
	n := 0
	for pat, class := range staffRouteGolden {
		if !strings.Contains(pat, "residence-approval") && !strings.Contains(pat, "residence-determinations") {
			continue
		}
		n++
		if want, ok := residenceRoutes[pat]; !ok || want != class {
			t.Errorf("%s: class %q, want %q (known %v)", pat, class, want, ok)
		}
	}
	if n != len(residenceRoutes) {
		t.Errorf("golden table has %d residence maker-checker routes, want %d", n, len(residenceRoutes))
	}
	src, err := os.ReadFile("residence_approval_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	routes := regexp.MustCompile(`mux\.Handle\("(GET|POST) (/v1/[^"]*)"`).FindAllStringSubmatch(string(src), -1)
	if len(routes) != len(residenceRoutes) {
		t.Fatalf("%d registrations, want %d", len(routes), len(residenceRoutes))
	}
	reg := string(src)
	// The two determination routes are tenant-scoped (a platform token carries no tenant: the verification rows are tenant RLS
	// data); the policy routes are NOT tenant-scope wrapped (a platform token acts through the acting session).
	for _, line := range strings.Split(reg, "\n") {
		if strings.Contains(line, `mux.Handle("`) && strings.Contains(line, "/v1/admin/tenants/{tenantID}/") && strings.Contains(line, "RequireTenantScope") {
			t.Errorf("a policy route must not be tenant-scope wrapped: %s", line)
		}
	}
	if c := strings.Count(reg, "RequireTenantScope"); c != 2 {
		t.Errorf("RequireTenantScope on %d routes, want exactly the two determination routes", c)
	}
	if !strings.Contains(reg, "auth.RequirePermission(auth.PermVerificationReview)(newApproveResidenceDeterminationHandler") {
		t.Error("the determination approval must be gated by verification:review")
	}
}

// The critical routes and the signer backstop agree: the two approval operations are step-up critical in the registry, the
// protective policy filing is absent (a classification drift would turn into a 500 at signing time).
func TestResidenceApprovalRoutes_StepUpClassMatchesSignerBackstop(t *testing.T) {
	ops := map[string]string{
		"POST /v1/admin/tenants/{tenantID}/residence-approval-policy":                      residenceapproval.OpPolicySet,
		"POST /v1/admin/tenants/{tenantID}/residence-approval-policy/{policyID}/approvals": residenceapproval.OpPolicyApprove,
		"POST /v1/admin/kyc/verifications/{id}/residence-approval":                         residenceapproval.OpDeterminationApprove,
	}
	for pat, op := range ops {
		class := staffRouteGolden[pat]
		reg, inRegistry := actorproof.StepUpOperations[op]
		switch class {
		case "none":
			if inRegistry {
				t.Errorf("%s: route class none but %s is step-up classified (%s)", pat, op, reg)
			}
		case "critical":
			if !inRegistry || reg != stepup.Critical {
				t.Errorf("%s: route class critical but %s is registered as %q (in registry %v)", pat, op, reg, inRegistry)
			}
		default:
			t.Errorf("%s: unexpected class %q", pat, class)
		}
	}
}

func TestResidenceErrorCode_ClosedMappingNeverLeaksDatabaseText(t *testing.T) {
	leak := "SQLSTATE RD004 internal detail"
	cases := []struct {
		err  error
		code apierror.Code
	}{
		{residenceapproval.ErrNotFound, apierror.CodeNotFound},
		{residenceapproval.ErrProofUnavailable, apierror.CodeUnavailable},
		{residenceapproval.ErrInvalidInput, apierror.CodeValidation},
		{residenceapproval.ErrFourEyes, apierror.CodeForbidden},
		{residenceapproval.ErrNoPerson, apierror.CodeForbidden},
		{residenceapproval.ErrForbidden, apierror.CodeForbidden},
		{residenceapproval.ErrActorScope, apierror.CodeForbidden},
		{residenceapproval.ErrNotPending, apierror.CodeConflict},
		{residenceapproval.ErrDuplicate, apierror.CodeConflict},
		{errors.New("boom"), apierror.CodeInternal},
	}
	for _, c := range cases {
		wrapped := fmt.Errorf("%w: %s", c.err, leak)
		code, msg := residenceErrorCode(wrapped)
		if code != c.code {
			t.Errorf("%v: code %v, want %v", c.err, code, c.code)
		}
		if strings.Contains(msg, "SQLSTATE") || strings.Contains(msg, "RD0") || msg == "" {
			t.Errorf("%v: message %q must be a fixed sentence", c.err, msg)
		}
	}
}

func TestOpenAPI_ResidenceApprovalRoutesDocumented(t *testing.T) {
	spec := readSpec(t)
	for pat := range residenceRoutes {
		parts := strings.SplitN(pat, " ", 2)
		block := openAPIPathBlock(t, spec, parts[1])
		if !strings.Contains(block, "    "+strings.ToLower(parts[0])+":\n") {
			t.Errorf("%s is mounted but not documented", pat)
		}
	}
	for _, op := range append(documentedOps(spec, "/v1/admin/tenants/{tenantID}/residence-approval-policy"),
		documentedOps(spec, "/v1/admin/kyc/residence-determinations", "/v1/admin/kyc/verifications/{id}/residence-approval")...) {
		if _, ok := residenceRoutes[op]; !ok {
			t.Errorf("%s is documented but not mounted", op)
		}
	}
	for _, pat := range []string{"POST /v1/admin/tenants/{tenantID}/residence-approval-policy/{policyID}/approvals", "POST /v1/admin/kyc/verifications/{id}/residence-approval"} {
		parts := strings.SplitN(pat, " ", 2)
		if !strings.Contains(openAPIPathBlock(t, spec, parts[1]), "step-up `critical`") {
			t.Errorf("%s: the description must state the critical step-up", pat)
		}
	}
}
