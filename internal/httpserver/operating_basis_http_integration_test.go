//go:build integration

package httpserver

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// lic9 (migration 0180): an operating basis is filed through the EXISTING licensing change route (no new write route), executed by
// two other Persons, and read back through the platform read APIs with its decision record. No value is shipped: the test files
// its own throwaway basis for a throwaway tenant.
func TestOperatingBasisHTTP_FileExecuteRead(t *testing.T) {
	w := newReadsWorld(t, "pa-basis-pw-1")
	due := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	payload := map[string]any{
		"tenant_id": w.tenantB.String(), "brand_id": nil, "country_code": "MT", "operation_code": "deposit", "product_code": nil,
		"basis_kind": "no_licence_documented", "licence_id": nil, "jurisdiction_id": w.jurID,
		"determination_reference": "DET-HTTP-1", "determined_by_role": "head_of_compliance", "basis_evidence_reference": nil,
		"restrictions_note": nil, "restricted_product_codes": []string{}, "review_due_at": due,
	}
	filed := w.do(http.MethodPost, "/v1/admin/licensing-changes", map[string]any{"kind": "operating_basis_set", "payload": payload, "reason_code": "documented_basis"})
	done := executeFiledChange(t, w.srv, w.pool, filed)
	if done.ChangeRequest.Kind != "operating_basis_set" || !done.ChangeRequest.Widening || done.Outcome["operating_basis_id"] == nil {
		t.Fatalf("a basis set is widening and returns the basis id: %+v", done)
	}

	var pg pageIT
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/operating-bases?tenant_id="+w.tenantB.String()+"&current=true", nil), &pg)
	if pg.Total != 1 || len(pg.Items) != 1 {
		t.Fatalf("one open basis: %+v", pg)
	}
	it := pg.Items[0]
	if it["basis_kind"] != "no_licence_documented" || it["status"] != "active" || it["review_due_at"] != due || it["change_request_id"] != done.ChangeRequest.ID ||
		it["determined_by_role"] != "head_of_compliance" || it["disclaimer"] == "" {
		t.Fatalf("basis item %+v", it)
	}
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/operating-bases?tenant_id="+w.tenantA.String(), nil), &pg)
	if pg.Total != 0 || pg.Items == nil {
		t.Fatalf("tenant A has no basis (and the list is an array, never null): %+v", pg)
	}

	var detail struct {
		ID       string `json:"id"`
		Decision *struct {
			ChangeRequestID string `json:"change_request_id"`
			Kind            string `json:"kind"`
			Approvals       []struct {
				ApproverID *string `json:"approver_id"`
			} `json:"approvals"`
		} `json:"decision"`
	}
	decodeBody(t, w.do(http.MethodGet, "/v1/admin/operating-bases/"+it["id"].(string), nil), &detail)
	if detail.Decision == nil || detail.Decision.ChangeRequestID != done.ChangeRequest.ID || detail.Decision.Kind != "operating_basis_set" || len(detail.Decision.Approvals) != 2 {
		t.Fatalf("the decision record must name the change and its two approvers: %+v", detail)
	}
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/operating-bases/"+uuid.NewString(), nil), http.StatusNotFound, "unknown basis")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/operating-bases/nope", nil), http.StatusBadRequest, "malformed basis id")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/operating-bases", nil), http.StatusBadRequest, "tenant_id required")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/operating-bases?tenant_id="+uuid.NewString(), nil), http.StatusNotFound, "unknown tenant")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/operating-bases?tenant_id="+w.tenantB.String()+"&country_code=mt", nil), http.StatusBadRequest, "lowercase country")
	expectStatus(t, w.do(http.MethodGet, "/v1/admin/operating-bases?tenant_id="+w.tenantB.String()+"&current=maybe", nil), http.StatusBadRequest, "bad current")

	// A malformed basis is refused at filing with a 400 and writes nothing.
	bad := map[string]any{}
	for k, v := range payload {
		bad[k] = v
	}
	bad["determined_by_role"] = "Jane Doe"
	bad["country_code"] = "DE"
	r := w.do(http.MethodPost, "/v1/admin/licensing-changes", map[string]any{"kind": "operating_basis_set", "payload": bad, "reason_code": "documented_basis"})
	expectStatus(t, r, http.StatusBadRequest, "a person's name as the responsible role")
}
