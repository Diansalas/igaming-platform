package httpserver

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/apierror"
)

// ADR 0119 slice S4a OpenAPI drift pin (DB-free): the liveness refusal code is in the Error enum, maps to 401, and the
// staff chain is documented on the bearer scheme and on the staff self-service route.
func TestOpenAPI_StaffLiveness_SessionRevokedDocumented(t *testing.T) {
	spec := launchSpec(t)
	start := strings.Index(spec, "    Error:\n")
	if start < 0 {
		t.Fatal("schema Error missing")
	}
	enum := spec[start:]
	if i := strings.Index(enum, "        message:"); i > 0 {
		enum = enum[:i]
	}
	if !strings.Contains(enum, "- "+string(apierror.CodeSessionRevoked)+"\n") {
		t.Error("session_revoked is returned by the API but not in the Error.code enum")
	}
	if apierror.Status(apierror.CodeSessionRevoked) != 401 {
		t.Errorf("session_revoked maps to %d, want 401", apierror.Status(apierror.CodeSessionRevoked))
	}
	src, err := os.ReadFile("../apierror/apierror.go")
	if err != nil {
		t.Fatal(err)
	}
	block := string(src)[strings.Index(string(src), "ADR 0119 slice S4a"):]
	block = block[:strings.Index(block, "\n)\n")]
	if n := len(regexp.MustCompile(`Code = "`).FindAllString(block, -1)); n != 1 {
		t.Fatalf("the ADR 0119 S4a apierror block declares %d codes, this test pins 1", n)
	}
	scheme := spec[strings.Index(spec, "    bearerAuth:\n"):]
	if loc := regexp.MustCompile(`\n    [^ \n]`).FindStringIndex(scheme[1:]); loc != nil {
		scheme = scheme[:loc[0]+1]
	}
	for _, want := range []string{"ADR 0119 S4a", "`session_revoked`", "503 `service_unavailable`", "aal2"} {
		if !strings.Contains(scheme, want) {
			t.Errorf("the bearerAuth scheme must document the staff chain (%q)", want)
		}
	}
	if !strings.Contains(openAPIPathBlock(t, spec, "/v1/staff/me/mfa"), "`session_revoked`") {
		t.Error("/v1/staff/me/mfa must document the session_revoked 401")
	}
}
