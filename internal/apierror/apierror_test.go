package apierror

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWrite_StatusCodeMapping(t *testing.T) {
	cases := []struct {
		code       Code
		wantStatus int
	}{
		{CodeValidation, http.StatusBadRequest},
		{CodeUnauthorized, http.StatusUnauthorized},
		{CodeForbidden, http.StatusForbidden},
		{CodeTenantMismatch, http.StatusForbidden},
		{CodeNotFound, http.StatusNotFound},
		{CodeConflict, http.StatusConflict},
		{CodeUnavailable, http.StatusServiceUnavailable},
		{CodeInternal, http.StatusInternalServerError},
		{Code("something_unrecognized"), http.StatusInternalServerError}, // safe default
	}

	for _, tc := range cases {
		t.Run(string(tc.code), func(t *testing.T) {
			rec := httptest.NewRecorder()
			Write(rec, "req-123", tc.code, "a message")

			if rec.Code != tc.wantStatus {
				t.Errorf("code %q: expected status %d, got %d", tc.code, tc.wantStatus, rec.Code)
			}

			var body Error
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("failed to decode response body: %v", err)
			}
			if body.Code != tc.code {
				t.Errorf("expected body code %q, got %q", tc.code, body.Code)
			}
			if body.RequestID != "req-123" {
				t.Errorf("expected request id 'req-123', got %q", body.RequestID)
			}
		})
	}
}

func TestWrite_SetsJSONContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, "req-1", CodeValidation, "bad input")

	ct := rec.Header().Get("Content-Type")
	if ct != "application/json; charset=utf-8" {
		t.Errorf("expected JSON content type, got %q", ct)
	}
}
