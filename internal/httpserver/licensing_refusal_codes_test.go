package httpserver

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/licensingchange"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

type nopLog struct{}

func (nopLog) Warn(string, ...any)  {}
func (nopLog) Error(string, ...any) {}

// c1 drift pin (Go half): the OpenAPI enum LicensingRefusalCode is exactly licensingchange.ClosedRefusals. The Back Office half
// (the wording map) is pinned by backoffice/src/features/licensing/licensingRefusal.test.ts, which reads this table and the same enum.
func TestLicensingRefusalCode_OpenAPIEnumEqualsGoTable(t *testing.T) {
	spec := launchSpec(t)
	i := strings.Index(spec, "\n    LicensingRefusalCode:\n")
	if i < 0 {
		t.Fatal("schema LicensingRefusalCode is not documented")
	}
	block := spec[i+1:]
	if j := strings.Index(block, "\n    AMLScreeningRefusalCode:\n"); j > 0 {
		block = block[:j]
	}
	en := block[strings.Index(block, "      enum:\n"):]
	var enum []string
	for _, m := range regexp.MustCompile(`(?m)^        - ([a-z_]+)$`).FindAllStringSubmatch(en, -1) {
		enum = append(enum, m[1])
	}
	var table []string
	for c := range licensingchange.ClosedRefusals {
		table = append(table, c)
		if !strings.Contains(block, "        `"+c+"`: ") {
			t.Errorf("the schema description does not document %s", c)
		}
	}
	sort.Strings(table)
	sorted := append([]string(nil), enum...)
	sort.Strings(sorted)
	if len(enum) < 25 || strings.Join(sorted, ",") != strings.Join(table, ",") {
		t.Fatalf("OpenAPI enum != Go table\nenum:  %v\ntable: %v", sorted, table)
	}
}

// Every refusal is answered with the status and the generic API code it had before the closed codes existed, plus the closed
// code in check_codes; internal, authorisation and uncoded refusals carry none; no body ever carries the error text.
func TestWriteLicensingChangeError_CodeInCheckCodesStatusUnchanged(t *testing.T) {
	secret := fmt.Errorf("tenant 8d2f-secret row")
	cases := []struct {
		name   string
		err    error
		status int
		api    string
		checks []string
	}{
		{"ceiling not enabled (409)", &licensingchange.DomainError{Code: "ceiling_not_enabled_for_country", Err: fmt.Errorf("%w: %v", operatingmarket.ErrCeilingExceeded, secret)}, 409, "LICENSING_CHANGE_REFUSED", []string{"ceiling_not_enabled_for_country"}},
		{"no licence or basis (409)", &licensingchange.DomainError{Code: "tenant_without_licence_or_basis", Err: operatingmarket.ErrCeilingExceeded}, 409, "LICENSING_CHANGE_REFUSED", []string{"tenant_without_licence_or_basis"}},
		{"licence unknown (400)", &licensingchange.DomainError{Code: "licence_unknown", Err: fmt.Errorf("%w: %v", jurisdiction.ErrInvalidInput, secret)}, 400, "validation_error", []string{"licence_unknown"}},
		{"brand not owned (404)", &licensingchange.DomainError{Code: "brand_not_owned", Err: fmt.Errorf("%w: %v", jurisdiction.ErrNotFound, secret)}, 404, "not_found", []string{"brand_not_owned"}},
		{"tenant unknown on bind (404)", &licensingchange.DomainError{Code: "tenant_unknown", Err: jurisdiction.ErrNotFound}, 404, "not_found", []string{"tenant_unknown"}},
		{"execute-time sentinel without precheck code (409)", &licensingchange.DomainError{Err: operatingmarket.ErrCeilingExceeded}, 409, "LICENSING_CHANGE_REFUSED", []string{"policy_exceeds_permitted_scope"}},
		{"guard LC024 (409)", &licensingchange.GuardError{Code: "LC024", Message: "licensing_change_requests: the change now widens"}, 409, "LICENSING_CHANGE_GUARD_REFUSED", []string{"change_now_widens"}},
		{"guard LC044 (409)", &licensingchange.GuardError{Code: "LC044", Message: "operating_bases: ..."}, 409, "LICENSING_CHANGE_GUARD_REFUSED", []string{"closed_basis_needs_successor"}},
		{"proof guard AP001 (403) has no refusal code", &licensingchange.GuardError{Code: "AP001"}, 403, "LICENSING_CHANGE_PROOF_REFUSED", nil},
		{"generic domain refusal stays uncoded (400)", &licensingchange.DomainError{Err: jurisdiction.ErrInvalidInput}, 400, "validation_error", nil},
		{"forbidden has none", licensingchange.ErrForbidden, 403, "forbidden", nil},
		{"internal has none", secret, 500, "internal_error", nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeLicensingChangeError(rec, licensingCall{requestID: "req-1", logger: nopLog{}}, "request", c.err)
			if rec.Code != c.status {
				t.Fatalf("status %d want %d: %s", rec.Code, c.status, rec.Body)
			}
			var body struct {
				Code       string   `json:"code"`
				Message    string   `json:"message"`
				CheckCodes []string `json:"check_codes"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if !strings.EqualFold(body.Code, c.api) && !(c.api == "internal_error" && body.Code == string(apierror.CodeInternal)) {
				t.Fatalf("code %q want %q", body.Code, c.api)
			}
			if strings.Join(body.CheckCodes, ",") != strings.Join(c.checks, ",") {
				t.Fatalf("check_codes %v want %v", body.CheckCodes, c.checks)
			}
			if strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "licensing_change_requests") || strings.Contains(rec.Body.String(), "operating_bases") {
				t.Fatalf("the body leaks error text: %s", rec.Body)
			}
		})
	}
}

// The guard map reaches the wire for every state it names, through the real classify.
func TestWriteLicensingChangeError_EveryGuardStateThroughClassify(t *testing.T) {
	for _, state := range []string{"LC001", "LC002", "LC010", "LC012", "LC020", "LC021", "LC023", "LC024", "LC030", "LC040", "LC041", "LC042", "LC043", "LC044", "LC099"} {
		err := licensingchange.ClassifyDBError(&pgconn.PgError{Code: state, Message: "internal detail"})
		rec := httptest.NewRecorder()
		writeLicensingChangeError(rec, licensingCall{requestID: "r", logger: nopLog{}}, "approve", err)
		var body struct {
			CheckCodes []string `json:"check_codes"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusConflict || len(body.CheckCodes) != 1 {
			t.Errorf("%s: %d %v", state, rec.Code, body.CheckCodes)
			continue
		}
		if _, ok := licensingchange.ClosedRefusals[body.CheckCodes[0]]; !ok {
			t.Errorf("%s: %q is not a closed code", state, body.CheckCodes[0])
		}
	}
}

// The duplicate jurisdiction code keeps its 400 validation_error and gains the closed code.
func TestWriteJurisdictionRegistryError_DuplicateCode(t *testing.T) {
	err := fmt.Errorf("%w: jurisdiction code %q already exists%w", jurisdiction.ErrInvalidInput, "KM-ANJ", jurisdiction.ErrDuplicateJurisdictionCode)
	rec := httptest.NewRecorder()
	writeJurisdictionRegistryError(rec, "r", nopLog{}, "create_jurisdiction", err)
	var body struct {
		Code       string   `json:"code"`
		CheckCodes []string `json:"check_codes"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != 400 || body.Code != "validation_error" || strings.Join(body.CheckCodes, ",") != "duplicate_jurisdiction_code" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = httptest.NewRecorder()
	writeJurisdictionRegistryError(rec, "r", nopLog{}, "create_jurisdiction", fmt.Errorf("%w: code and name are required", jurisdiction.ErrInvalidInput))
	if rec.Code != 400 || strings.Contains(rec.Body.String(), "check_codes") {
		t.Fatalf("a plain validation error stays uncoded: %d %s", rec.Code, rec.Body)
	}
}

// c1 LOW-3 end to end through the real classify: a trigger refusal reaching the writer is 409 LICENSING_CHANGE_REFUSED with its closed code
// (it used to be 500 internal_error, nothing written either way).
func TestWriteLicensingChangeError_TriggerRefusalIs409WithCode(t *testing.T) {
	for msg, want := range map[string]string{
		"policy exceeds the licence ceiling":                                 "policy_exceeds_permitted_scope",
		"ocp_inherit_rung_close_requires_successor: close needs a successor": "policy_close_needs_successor",
	} {
		err := licensingchange.ClassifyDBError(&pgconn.PgError{Code: "P0001", Message: msg})
		rec := httptest.NewRecorder()
		writeLicensingChangeError(rec, licensingCall{requestID: "r", logger: nopLog{}}, "approve", err)
		var body struct {
			Code       string   `json:"code"`
			CheckCodes []string `json:"check_codes"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != 409 || body.Code != "LICENSING_CHANGE_REFUSED" || strings.Join(body.CheckCodes, ",") != want || strings.Contains(rec.Body.String(), "ocp_") {
			t.Errorf("%s: %d %s", msg, rec.Code, rec.Body)
		}
	}
}
