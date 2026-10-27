package httpserver

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/withdrawalpolicy"
)

// lic11 (migration 0182): every mounted withdrawal restriction policy route is documented (both ways), names its static
// permission, security and 401; the closed WITHDRAWAL_RESTRICTION_POLICY_* codes in internal/apierror and in the Error.code
// enum are the same set; the disclaimer is the constant; the wire DTOs match their schemas. DB-free.
func TestOpenAPI_WithdrawalRestrictionPolicy_RoutesDocumented(t *testing.T) {
	spec := launchSpec(t)
	routes := mountedRoutes(t, "withdrawal_restriction_policy_routes.go")
	if len(routes) != 8 {
		t.Fatalf("expected 8 mounted withdrawal restriction policy operations, found %d (update the spec and this test together)", len(routes))
	}
	mounted := map[string]bool{}
	for _, r := range routes {
		parts := strings.SplitN(r, " ", 2)
		mounted[r] = true
		block := openAPIPathBlock(t, spec, parts[1])
		if !strings.Contains(block, "    "+strings.ToLower(parts[0])+":\n") {
			t.Errorf("%s is mounted but not documented", r)
		}
		if !strings.Contains(block, "withdrawal_restriction_policy:") || !strings.Contains(block, "bearerAuth") || !strings.Contains(block, `"401"`) {
			t.Errorf("%s: the entry must name its static permission, security and 401", r)
		}
	}
	docs := documentedOps(spec, "/v1/admin/withdrawal-restriction-policy-changes", "/v1/admin/tenants/{tenantID}/withdrawal-restriction-policy")
	for _, op := range docs {
		if !mounted[op] {
			t.Errorf("documented but not mounted: %s", op)
		}
	}
	if len(docs) != len(routes) {
		t.Errorf("documented %d operations, mounted %d", len(docs), len(routes))
	}
}

func TestOpenAPI_WithdrawalRestrictionPolicy_ErrorCodesMatch(t *testing.T) {
	spec := launchSpec(t)
	src, err := os.ReadFile("../apierror/apierror.go")
	if err != nil {
		t.Fatal(err)
	}
	inCode := map[string]bool{}
	for _, m := range regexp.MustCompile(`Code = "(WITHDRAWAL_RESTRICTION_POLICY_[A-Z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		inCode[m[1]] = true
	}
	start := strings.Index(spec, "    Error:\n")
	enum := spec[start:]
	enum = enum[:strings.Index(enum, "        message:")]
	inSpec := map[string]bool{}
	for _, m := range regexp.MustCompile(`- (WITHDRAWAL_RESTRICTION_POLICY_[A-Z_]+)\n`).FindAllStringSubmatch(enum, -1) {
		inSpec[m[1]] = true
	}
	if len(inCode) != 11 || !reflect.DeepEqual(inCode, inSpec) {
		t.Fatalf("apierror WITHDRAWAL_RESTRICTION_POLICY_* %v and the Error.code enum %v must be the same 11 codes", inCode, inSpec)
	}
	conflict := schemaOrResponseBlock(spec, "    WithdrawalRestrictionPolicyConflict:\n")
	forbidden := schemaOrResponseBlock(spec, "    WithdrawalRestrictionPolicyForbidden:\n")
	for c := range inCode {
		switch c {
		case "WITHDRAWAL_RESTRICTION_POLICY_UNAVAILABLE":
		case "WITHDRAWAL_RESTRICTION_POLICY_PROOF_REFUSED", "WITHDRAWAL_RESTRICTION_POLICY_NO_PERSON":
			if !strings.Contains(forbidden, c) {
				t.Errorf("%s must be described in WithdrawalRestrictionPolicyForbidden", c)
			}
		default:
			if !strings.Contains(conflict, c) {
				t.Errorf("%s must be described in WithdrawalRestrictionPolicyConflict", c)
			}
		}
	}
	if !strings.Contains(schemaBlock(t, spec, "WithdrawalRestrictionPolicyDisclaimer"), `- "`+withdrawalpolicy.Disclaimer+`"`) {
		t.Fatal("WithdrawalRestrictionPolicyDisclaimer must be exactly withdrawalpolicy.Disclaimer")
	}
}

func TestOpenAPI_WithdrawalRestrictionPolicy_DTOsMatchSchemas(t *testing.T) {
	spec := launchSpec(t)
	for _, c := range []struct {
		dto    any
		schema string
	}{
		{wrpStageJSON{}, "WithdrawalRestrictionPolicyStage"},
		{wrpCauseJSON{}, "WithdrawalRestrictionPolicyCause"},
		{wrpEffectiveResponse{}, "WithdrawalRestrictionPolicyEffective"},
		{wrpVersionJSON{}, "WithdrawalRestrictionPolicyVersion"},
		{wrpVersionsResponse{}, "WithdrawalRestrictionPolicyVersions"},
		{wrpCreateRequest{}, "WithdrawalRestrictionPolicyChangeCreateRequest"},
		{wrpDecisionRequest{}, "WithdrawalRestrictionPolicyDecisionRequest"},
		{wrpApprovalJSON{}, "WithdrawalRestrictionPolicyApproval"},
		{wrpChangeJSON{}, "WithdrawalRestrictionPolicyChange"},
		{wrpChangeResponse{}, "WithdrawalRestrictionPolicyChangeResponse"},
		{wrpChangeListResponse{}, "WithdrawalRestrictionPolicyChangeListResponse"},
	} {
		props := launchSchemaProps(schemaBlock(t, spec, c.schema))
		seen := map[string]bool{}
		for _, f := range jsonFields(reflect.TypeOf(c.dto)) {
			seen[f] = true
			if !props[f] {
				t.Errorf("%s: the field %q is on the wire but not in the schema", c.schema, f)
			}
		}
		for p := range props {
			if !seen[p] {
				t.Errorf("%s: the schema property %q is not produced by the DTO", c.schema, p)
			}
		}
	}
}

// The error mapping is closed and specific-before-broad (pure; no database).
func TestWRPErrorCode_Mapping(t *testing.T) {
	cases := []struct {
		err  error
		want apierror.Code
	}{
		{withdrawalpolicy.ErrNotFound, apierror.CodeNotFound},
		{withdrawalpolicy.ErrNoPerson, apierror.CodeWRPNoPerson},
		{withdrawalpolicy.ErrForbidden, apierror.CodeForbidden},
		{withdrawalpolicy.ErrInvalidInput, apierror.CodeValidation},
		{withdrawalpolicy.ErrNotConfigurable, apierror.CodeWRPNotConfigurable},
		{withdrawalpolicy.ErrSelfApproval, apierror.CodeWRPSelfApproval},
		{withdrawalpolicy.ErrNotPending, apierror.CodeWRPNotPending},
		{withdrawalpolicy.ErrAlreadyDecided, apierror.CodeWRPAlreadyDecided},
		{withdrawalpolicy.ErrDuplicatePending, apierror.CodeWRPDuplicatePending},
		{withdrawalpolicy.ErrApprovalsStale, apierror.CodeWRPApprovalsStale},
		{withdrawalpolicy.ErrRetryableLockTimeout, apierror.CodeWRPRetryableLockWait},
		{&withdrawalpolicy.GuardError{Code: "AP001"}, apierror.CodeWRPProofRefused},
		{&withdrawalpolicy.GuardError{Code: "WR040"}, apierror.CodeWRPGuardRefused},
		{withdrawalpolicy.ErrProofUnavailable, apierror.CodeInternal},
		{errors.New("boom"), apierror.CodeInternal},
	}
	for _, c := range cases {
		if got, _ := wrpErrorCode(c.err); got != c.want {
			t.Errorf("%v: got %s want %s", c.err, got, c.want)
		}
	}
}
