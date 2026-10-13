//go:build integration

package httpserver

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// behindChain invokes a route's INNER handler directly, with tc injected as the authenticated context, as if the ADR
// 0119 S4a staff chain had admitted the request. Since S4a no live staff session can carry claims that disagree with
// the database (a role the staff row does not hold, a tenant claim for a platform row, a role at a scope the
// staff_users CHECK forbids), so the in-handler authorization layers behind the chain are now defence in depth that no
// HTTP request can reach with such claims. Tests that pin those layers on their own (a mutant that drops one must
// still fail) call the handler this way; the HTTP half of the same test asserts the chain's 401 session_revoked.
func behindChain(t *testing.T, h http.Handler, method, target string, pathValues map[string]string, tc tenant.Context, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader = http.NoBody
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, target, rdr)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range pathValues {
		req.SetPathValue(k, v)
	}
	req = req.WithContext(tenant.WithContext(req.Context(), tc))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}
