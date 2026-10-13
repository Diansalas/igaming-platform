package httpserver

import (
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
)

// ADR 0119 slice S3 OpenAPI drift pins (DB-free): the staff-auth routes mounted == documented, the wire DTOs ==
// the schemas (both directions), the new error codes == the Error enum and the response descriptions, and the
// no-store / rate-limit documentation.

func staffAuthRoutes(t *testing.T) [][]string {
	t.Helper()
	src, err := os.ReadFile("staff_auth_routes.go")
	if err != nil {
		t.Fatal(err)
	}
	return regexp.MustCompile(`mux\.Handle\("(GET|POST|PUT|DELETE) (/v1/[^"]*)"`).FindAllStringSubmatch(string(src), -1)
}

func TestOpenAPI_StaffAuth_RoutesDocumentedBothWays(t *testing.T) {
	spec := launchSpec(t)
	routes := staffAuthRoutes(t)
	// ADR 0119 S4b adds POST /v1/staff/auth/step-up and POST /v1/staff/mfa/recovery-codes/regenerate (7 -> 9).
	if len(routes) != 9 {
		t.Fatalf("expected 9 mounted staff-auth operations, found %d (update the spec and this test together)", len(routes))
	}
	mounted := map[string]bool{}
	for _, r := range routes {
		mounted[strings.ToLower(r[1])+" "+r[2]] = true
		block := openAPIPathBlock(t, spec, r[2])
		if !strings.Contains(block, "    "+strings.ToLower(r[1])+":\n") {
			t.Errorf("%s %s is mounted but not documented", r[1], r[2])
			continue
		}
		if !strings.Contains(block, "tags: [staff-auth]") {
			t.Errorf("%s %s: tag staff-auth missing", r[1], r[2])
		}
		if r[2] == "/v1/staff/me/mfa" {
			if !strings.Contains(block, "bearerAuth") {
				t.Errorf("%s: bearerAuth missing", r[2])
			}
			continue
		}
		// ADR 0119 S4b: the two bearer routes that carry a secret (a new access token; new recovery codes).
		if r[2] == "/v1/staff/auth/step-up" || r[2] == "/v1/staff/mfa/recovery-codes/regenerate" {
			if !strings.Contains(block, "bearerAuth") || !strings.Contains(block, "#/components/headers/NoStore") || !strings.Contains(block, `"503"`) {
				t.Errorf("%s: bearerAuth, no-store and 503 must be documented", r[2])
			}
			if r[2] == "/v1/staff/auth/step-up" && (!strings.Contains(block, "Rate bucket `mfa`") || !strings.Contains(block, `"429"`)) {
				t.Errorf("%s: the rate bucket `mfa` and 429 must be documented", r[2])
			}
			if r[2] == "/v1/staff/mfa/recovery-codes/regenerate" && !strings.Contains(block, "#/components/responses/StepUpRequired") {
				t.Errorf("%s: the 403 step_up_required response must be documented", r[2])
			}
			continue
		}
		if !strings.Contains(block, "security: []") || !strings.Contains(block, "#/components/headers/NoStore") || !strings.Contains(block, `"429"`) || !strings.Contains(block, `"503"`) {
			t.Errorf("%s %s: unauthenticated, no-store, 429 and 503 must be documented", r[1], r[2])
		}
		bucket := "`mfa`"
		switch r[2] {
		case "/v1/staff/auth/login":
			bucket = "`login`"
		case "/v1/staff/auth/enrol/redeem":
			bucket = "`credential`"
		}
		if !strings.Contains(block, "Rate bucket "+bucket) {
			t.Errorf("%s: the rate bucket %s must be named", r[2], bucket)
		}
	}
	// The reverse: nothing documented under the S3 staff-auth paths is unmounted.
	pathRe := regexp.MustCompile(`(?m)^  (/v1/staff/(?:auth|me|mfa)/[^:\s]*):\n((?:    .*\n|\n)*)`)
	opRe := regexp.MustCompile(`(?m)^    (get|post|put|delete):\n`)
	documented := 0
	for _, m := range pathRe.FindAllStringSubmatch(spec, -1) {
		for _, op := range opRe.FindAllStringSubmatch(m[2], -1) {
			documented++
			if !mounted[op[1]+" "+m[1]] {
				t.Errorf("documented but not mounted: %s %s", op[1], m[1])
			}
		}
	}
	if documented != len(routes) {
		t.Errorf("documented %d staff-auth operations, mounted %d", documented, len(routes))
	}
}

func TestOpenAPI_StaffAuth_DTOsMatchSchemas(t *testing.T) {
	spec := launchSpec(t)
	for _, c := range []struct {
		dto    any
		schema string
	}{
		{staffLoginRequest{}, "StaffLoginRequest"},
		{staffChallengeWireBody{}, "StaffAuthChallenge"},
		{staffChallengeWire{}, "StaffLoginChallenge"},
		{mfaVerifyRequest{}, "MfaVerifyRequest"},
		{mfaRecoverRequest{}, "MfaRecoverRequest"},
		{enrolStartRequest{}, "EnrolStartRequest"},
		{enrolStartWire{}, "EnrolStartResponse"},
		{enrolConfirmRequest{}, "EnrolConfirmRequest"},
		{enrolConfirmWire{}, "EnrolConfirmResponse"},
		{enrolRedeemRequest{}, "EnrolRedeemRequest"},
		{staffMFAStatusResponse{}, "StaffMfaStatus"},
		{tokenPairResponse{}, "TokenPair"},
		{stepUpRequest{}, "StepUpRequest"},
		{stepUpWire{}, "StepUpResponse"},
		{recoveryCodesWire{}, "RecoveryCodesRegenerateResponse"},
	} {
		props := staffAuthSchemaProps(schemaBlock(t, spec, c.schema))
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
	for _, sensitive := range []string{"StaffAuthChallenge", "StaffLoginChallenge", "EnrolStartResponse", "EnrolConfirmResponse",
		"StepUpResponse", "RecoveryCodesRegenerateResponse"} {
		if !strings.Contains(schemaBlock(t, spec, sensitive), "x-sensitive: true") {
			t.Errorf("%s must be marked x-sensitive", sensitive)
		}
	}
}

// staffAuthSchemaProps is launchSchemaProps admitting digits in a property name (secret_base32).
func staffAuthSchemaProps(block string) map[string]bool {
	out := map[string]bool{}
	in := false
	re := regexp.MustCompile(`^        ([a-z0-9_]+):`)
	for _, line := range strings.Split(block, "\n") {
		if strings.HasPrefix(line, "      properties:") {
			in = true
			continue
		}
		if in && strings.HasPrefix(line, "      ") && !strings.HasPrefix(line, "        ") {
			in = false
		}
		if in {
			if m := re.FindStringSubmatch(line); m != nil {
				out[m[1]] = true
			}
		}
	}
	return out
}

func TestOpenAPI_StaffAuth_ErrorCodes(t *testing.T) {
	spec := launchSpec(t)
	start := strings.Index(spec, "    Error:\n")
	if start < 0 {
		t.Fatal("schema Error missing")
	}
	enum := spec[start:]
	if i := strings.Index(enum, "        message:"); i > 0 {
		enum = enum[:i]
	}
	codes := map[apierror.Code]int{
		apierror.CodeMFAEnrolmentRequired: 403, apierror.CodeMFALocked: 403, apierror.CodeInvalidMFACode: 401,
		apierror.CodeInvalidChallenge: 401, apierror.CodeInvalidEnrolmentToken: 401, apierror.CodeMFAEnrolmentExpired: 409,
	}
	src, err := os.ReadFile("../apierror/apierror.go")
	if err != nil {
		t.Fatal(err)
	}
	block := string(src)[strings.Index(string(src), "ADR 0119 slice S3"):]
	block = block[:strings.Index(block, "\n)\n")]
	if n := len(regexp.MustCompile(`Code = "`).FindAllString(block, -1)); n != len(codes) {
		t.Fatalf("the ADR 0119 apierror block declares %d codes, this test pins %d", n, len(codes))
	}
	for c, status := range codes {
		if !strings.Contains(enum, "- "+string(c)+"\n") {
			t.Errorf("%s is returned by the API but not in the Error.code enum", c)
		}
		if apierror.Status(c) != status {
			t.Errorf("%s maps to HTTP %d, want %d", c, apierror.Status(c), status)
		}
		if strings.Count(spec, string(c)) < 2 {
			t.Errorf("%s is not described in any staff-auth response", c)
		}
	}
	login := openAPIPathBlock(t, spec, "/v1/staff/auth/login")
	if !strings.Contains(login, "never a token pair") && !strings.Contains(login, "NEVER yields a token pair") && !strings.Contains(login, "never yields a token pair") {
		t.Error("the login entry must state the breaking change (no token pair)")
	}
	refresh := openAPIPathBlock(t, spec, "/v1/auth/refresh")
	for _, want := range []string{"STAFF sessions", "assurance seal", "absolute lifetime", "idle"} {
		if !strings.Contains(refresh, want) {
			t.Errorf("the refresh entry must document the staff rules (%q)", want)
		}
	}
}
