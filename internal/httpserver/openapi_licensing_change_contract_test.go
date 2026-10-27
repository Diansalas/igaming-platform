package httpserver

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/licensingchange"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// Migration 0177: every mounted licensing change route is documented under the same path and method (both ways); the closed
// LICENSING_CHANGE_* codes in internal/apierror and in the Error.code enum are the same set; the disclaimer is the constant;
// the wire DTOs match their schemas. DB-free.
func TestOpenAPI_LicensingChange_RoutesDocumented(t *testing.T) {
	spec := launchSpec(t)
	routes := mountedRoutes(t, "licensing_change_routes.go")
	if len(routes) != 6 {
		t.Fatalf("expected 6 mounted licensing change operations, found %d (update the spec and this test together)", len(routes))
	}
	mounted := map[string]bool{}
	for _, r := range routes {
		parts := strings.SplitN(r, " ", 2)
		mounted[r] = true
		block := openAPIPathBlock(t, spec, parts[1])
		if !strings.Contains(block, "    "+strings.ToLower(parts[0])+":\n") {
			t.Errorf("%s is mounted but not documented", r)
		}
		if !strings.Contains(block, "licensing_change:") || !strings.Contains(block, "bearerAuth") || !strings.Contains(block, `"401"`) {
			t.Errorf("%s: the entry must name its static permission, security and 401", r)
		}
	}
	for _, op := range documentedOps(spec, "/v1/admin/licensing-changes") {
		if !mounted[op] {
			t.Errorf("documented but not mounted: %s", op)
		}
	}
	if n := len(documentedOps(spec, "/v1/admin/licensing-changes")); n != len(routes) {
		t.Errorf("documented %d licensing change operations, mounted %d", n, len(routes))
	}
	for _, p := range []string{"/v1/admin/licences", "/v1/admin/tenants/{tenantID}/licence"} {
		b := openAPIPathBlock(t, spec, p)
		for _, want := range []string{"migration 0177", `"202"`, "LicensingChangeResponse"} {
			if !strings.Contains(b, want) {
				t.Errorf("%s: OpenAPI entry lacks %q", p, want)
			}
		}
	}
}

func TestOpenAPI_LicensingChange_ErrorCodesMatch(t *testing.T) {
	spec := launchSpec(t)
	src, err := os.ReadFile("../apierror/apierror.go")
	if err != nil {
		t.Fatal(err)
	}
	inCode := map[string]bool{}
	for _, m := range regexp.MustCompile(`Code = "(LICENSING_CHANGE_[A-Z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		inCode[m[1]] = true
	}
	start := strings.Index(spec, "    Error:\n")
	enum := spec[start:]
	enum = enum[:strings.Index(enum, "        message:")]
	inSpec := map[string]bool{}
	for _, m := range regexp.MustCompile(`- (LICENSING_CHANGE_[A-Z_]+)\n`).FindAllStringSubmatch(enum, -1) {
		inSpec[m[1]] = true
	}
	if len(inCode) != 11 || !reflect.DeepEqual(inCode, inSpec) {
		t.Fatalf("apierror LICENSING_CHANGE_* %v and the Error.code enum %v must be the same 11 codes", inCode, inSpec)
	}
	conflict := schemaOrResponseBlock(spec, "    LicensingChangeConflict:\n")
	forbidden := schemaOrResponseBlock(spec, "    LicensingChangeForbidden:\n")
	for c := range inCode {
		switch c {
		case "LICENSING_CHANGE_UNAVAILABLE":
		case "LICENSING_CHANGE_PROOF_REFUSED", "LICENSING_CHANGE_NO_PERSON":
			if !strings.Contains(forbidden, c) {
				t.Errorf("%s must be described in LicensingChangeForbidden", c)
			}
		default:
			if !strings.Contains(conflict, c) {
				t.Errorf("%s must be described in LicensingChangeConflict", c)
			}
		}
	}
	if !strings.Contains(schemaBlock(t, spec, "LicensingChangeDisclaimer"), `- "`+licensingchange.Disclaimer+`"`) {
		t.Fatal("LicensingChangeDisclaimer must be exactly licensingchange.Disclaimer")
	}
}

func schemaOrResponseBlock(spec, key string) string {
	i := strings.Index(spec, key)
	if i < 0 {
		return ""
	}
	rest := spec[i+len(key):]
	if loc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(rest); loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

func TestOpenAPI_LicensingChange_DTOsMatchSchemas(t *testing.T) {
	spec := launchSpec(t)
	for _, c := range []struct {
		dto    any
		schema string
	}{
		{licensingChangeJSON{}, "LicensingChange"},
		{licensingApprovalJSON{}, "LicensingChangeApproval"},
		{licensingChangeResponse{}, "LicensingChangeResponse"},
		{licensingChangeListResponse{}, "LicensingChangeListResponse"},
		{licensingChangeCreateRequest{}, "LicensingChangeCreateRequest"},
		{licensingDecisionRequest{}, "LicensingDecisionRequest"},
		{assignTenantLicenceResponse{}, "TenantLicenceAssignment"},
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
func TestLicensingChangeErrorCode_Mapping(t *testing.T) {
	cases := []struct {
		err  error
		want apierror.Code
	}{
		{licensingchange.ErrNotFound, apierror.CodeNotFound},
		{&licensingchange.DomainError{Err: jurisdiction.ErrNotFound}, apierror.CodeNotFound},
		{licensingchange.ErrNoPerson, apierror.CodeLicensingChangeNoPerson},
		{licensingchange.ErrForbidden, apierror.CodeForbidden},
		{licensingchange.ErrActingSession, apierror.CodeForbidden},
		{licensingchange.ErrInvalidInput, apierror.CodeValidation},
		{&licensingchange.DomainError{Err: jurisdiction.ErrInvalidInput}, apierror.CodeValidation},
		{&licensingchange.DomainError{Err: operatingmarket.ErrInvalidInput}, apierror.CodeValidation},
		{licensingchange.ErrSelfApproval, apierror.CodeLicensingChangeSelfApproval},
		{licensingchange.ErrNotPending, apierror.CodeLicensingChangeNotPending},
		{licensingchange.ErrAlreadyDecided, apierror.CodeLicensingChangeAlreadyDecided},
		{licensingchange.ErrDuplicatePending, apierror.CodeLicensingChangeDuplicatePending},
		{licensingchange.ErrApprovalsStale, apierror.CodeLicensingChangeApprovalsStale},
		{licensingchange.ErrRetryableLockTimeout, apierror.CodeLicensingChangeRetryableLockWait},
		{&licensingchange.DomainError{Err: operatingmarket.ErrCeilingExceeded}, apierror.CodeLicensingChangeRefused},
		{&licensingchange.GuardError{Code: "AP001"}, apierror.CodeLicensingChangeProofRefused},
		{&licensingchange.GuardError{Code: "LC040"}, apierror.CodeLicensingChangeGuardRefused},
		{licensingchange.ErrProofUnavailable, apierror.CodeInternal},
		{errors.New("boom"), apierror.CodeInternal},
	}
	for _, c := range cases {
		if got, _ := licensingChangeErrorCode(c.err); got != c.want {
			t.Errorf("%v: got %s want %s", c.err, got, c.want)
		}
	}
}
