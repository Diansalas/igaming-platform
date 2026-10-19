//go:build integration

package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

// Migration 0177: POST /v1/admin/licences and PUT /v1/admin/tenants/{tenantID}/licence file licensing changes (202) that two
// approvals by two OTHER Persons execute. These helpers drive that governed path over HTTP for the existing licence tests.

// ensureLicensingProofs installs the process actor-proof issuer for the test database (the governed writes fail closed with
// 500 without one).
func ensureLicensingProofs(t *testing.T) {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	prooftest.Install(t, u)
}

// licensingApproverTokens creates two platform admins (two distinct Persons) and logs them in.
func licensingApproverTokens(t *testing.T, srv *httptest.Server, pool *db.Pool) (string, string) {
	t.Helper()
	pw := "pa-lc-approver-pw"
	a1 := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, pw)
	a2 := mustCreateStaff(t, pool, uuid.Nil, identity.StaffRolePlatformAdmin, pw)
	return mustLoginStaff(t, srv, "", a1.Email, pw).AccessToken, mustLoginStaff(t, srv, "", a2.Email, pw).AccessToken
}

// executeFiledChange takes the 202 response of a governed route, approves the change with two other Persons and returns the
// final (executed) response.
func executeFiledChange(t *testing.T, srv *httptest.Server, pool *db.Pool, filed *http.Response) licensingChangeResponseIT {
	t.Helper()
	if filed.StatusCode != http.StatusAccepted && filed.StatusCode != http.StatusCreated {
		t.Fatalf("expected 202/201 filing the licensing change, got %d", filed.StatusCode)
	}
	var change licensingChangeResponseIT
	decodeBody(t, filed, &change)
	if change.ChangeRequest.Status != "requested" || change.Executed || change.ChangeRequest.RequiredApprovals != 2 {
		t.Fatalf("a widening change is filed, not executed, and needs two approvals: %+v", change)
	}
	tok1, tok2 := licensingApproverTokens(t, srv, pool)
	r1 := sendAssetJSON(t, srv.URL+"/v1/admin/licensing-changes/"+change.ChangeRequest.ID+"/approve", http.MethodPost, tok1, map[string]any{"reason_code": "reviewed_ok"})
	var first licensingChangeResponseIT
	decodeBody(t, r1, &first)
	if r1.StatusCode != http.StatusOK || first.Executed {
		t.Fatalf("the first approval must be recorded without executing: %d %+v", r1.StatusCode, first)
	}
	r2 := sendAssetJSON(t, srv.URL+"/v1/admin/licensing-changes/"+change.ChangeRequest.ID+"/approve", http.MethodPost, tok2, map[string]any{"reason_code": "reviewed_ok"})
	var done licensingChangeResponseIT
	decodeBody(t, r2, &done)
	if r2.StatusCode != http.StatusOK || !done.Executed || done.ChangeRequest.Status != "executed" {
		t.Fatalf("the second approval must execute the change: %d %+v", r2.StatusCode, done)
	}
	return done
}

type licensingChangeResponseIT struct {
	ChangeRequest struct {
		ID                string  `json:"id"`
		Kind              string  `json:"kind"`
		Status            string  `json:"status"`
		Widening          bool    `json:"widening"`
		RequiredApprovals int     `json:"required_approvals"`
		RequestedBy       *string `json:"requested_by"`
		RequestedScope    string  `json:"requested_scope"`
		ExecutedBy        *string `json:"executed_by"`
		SubjectTenantID   *string `json:"subject_tenant_id"`
		Approvals         []struct {
			ApproverID *string `json:"approver_id"`
		} `json:"approvals"`
	} `json:"change_request"`
	Executed   bool           `json:"executed"`
	Outcome    map[string]any `json:"outcome"`
	Disclaimer string         `json:"disclaimer"`
}
