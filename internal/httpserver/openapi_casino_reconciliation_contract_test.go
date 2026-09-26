// Stage 10.3 W2b (CAS-RECON-1): contract test for the three read-only
// casino reconciliation admin routes - modeled on
// openapi_casinowebhook_contract_test.go (a structural check against the
// plain-text spec), plus a field-set check that the documented schema
// properties are EXACTLY the handler's JSON tags, so a response field
// added or removed on one side and not the other fails here.
package httpserver

import (
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func casReconSpec(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("../../docs/api/openapi/platform-api.yaml")
	if err != nil {
		t.Fatalf("read openapi spec: %v", err)
	}
	return string(raw)
}

func casReconPathBlock(t *testing.T, content, path string) string {
	t.Helper()
	key := "  " + path + ":"
	start := strings.Index(content, key+"\n")
	if start < 0 {
		t.Fatalf("expected the OpenAPI spec to document %q", path)
	}
	rest := content[start+len(key):]
	loc := regexp.MustCompile(`(?m)^  \S`).FindStringIndex(rest)
	if loc != nil {
		rest = rest[:loc[0]]
	}
	return rest
}

func jsonTags(v any) []string {
	var out []string
	rt := reflect.TypeOf(v)
	for i := 0; i < rt.NumField(); i++ {
		tag := strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]
		if tag != "" && tag != "-" {
			out = append(out, tag)
		}
	}
	sort.Strings(out)
	return out
}

// schemaProps extracts a components.schemas entry's property names and
// its single-line required list from the plain-text spec (no YAML
// dependency, matching the other contract tests in this package).
func schemaProps(t *testing.T, content, name string) (props, required []string) {
	t.Helper()
	key := "\n    " + name + ":\n"
	start := strings.Index(content, key)
	if start < 0 {
		t.Fatalf("schema %s not documented", name)
	}
	block := content[start+len(key):]
	if loc := regexp.MustCompile(`(?m)^    \S`).FindStringIndex(block); loc != nil {
		block = block[:loc[0]]
	}
	for _, m := range regexp.MustCompile(`(?m)^        ([a-z_]+):`).FindAllStringSubmatch(block, -1) {
		props = append(props, m[1])
	}
	rm := regexp.MustCompile(`(?m)^      required: \[([^\]]*)\]`).FindStringSubmatch(block)
	if rm == nil {
		t.Fatalf("schema %s has no single-line required list", name)
	}
	for _, f := range strings.Split(rm[1], ",") {
		required = append(required, strings.TrimSpace(f))
	}
	sort.Strings(props)
	sort.Strings(required)
	return props, required
}

func TestOpenAPI_CasinoReconciliationAdmin_ContractMatchesHandlers(t *testing.T) {
	content := casReconSpec(t)

	routes := []struct {
		path   string
		codes  []string
		schema string
	}{
		{"/v1/admin/casino/reconciliation/runs", []string{`"200":`, `"401":`, `"403":`, `"500":`}, "CasinoReconciliationRun"},
		{"/v1/admin/casino/reconciliation/mismatches", []string{`"200":`, `"400":`, `"401":`, `"403":`, `"500":`}, "CasinoReconciliationMismatch"},
		{"/v1/admin/casino/callback-rejections", []string{`"200":`, `"401":`, `"403":`, `"500":`}, "CasinoCallbackRejection"},
	}
	for _, rt := range routes {
		block := casReconPathBlock(t, content, rt.path)
		if !strings.Contains(block, "    get:") {
			t.Errorf("%s: expected a GET operation", rt.path)
		}
		for _, verb := range []string{"    post:", "    put:", "    patch:", "    delete:"} {
			if strings.Contains(block, verb) {
				t.Errorf("%s: read-only route must document no %s operation", rt.path, strings.TrimSpace(verb))
			}
		}
		for _, want := range []string{"bearerAuth", "casino_reconciliation:read", "PageMeta", rt.schema} {
			if !strings.Contains(block, want) {
				t.Errorf("%s: OpenAPI entry missing %q", rt.path, want)
			}
		}
		for _, code := range rt.codes {
			if !strings.Contains(block, code) {
				t.Errorf("%s: OpenAPI entry missing response code %s", rt.path, code)
			}
		}
	}

	for name, v := range map[string]any{
		"CasinoReconciliationRun":      casinoReconciliationRunResponse{},
		"CasinoReconciliationMismatch": casinoReconciliationMismatchResponse{},
		"CasinoCallbackRejection":      casinoCallbackRejectionResponse{},
	} {
		props, required := schemaProps(t, content, name)
		tags := jsonTags(v)
		if !reflect.DeepEqual(props, tags) {
			t.Errorf("%s: documented properties %v != handler JSON fields %v", name, props, tags)
		}
		if !reflect.DeepEqual(required, tags) {
			t.Errorf("%s: required %v != handler JSON fields %v (every field is always present)", name, required, tags)
		}
	}
}
