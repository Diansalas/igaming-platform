// Package apierror defines the platform-wide error envelope so every
// service returns errors in the same machine-readable shape. See
// docs/architecture/04-api-architecture.md "Conventions".
package apierror

import (
	"encoding/json"
	"net/http"
)

// Code is a stable, machine-readable error identifier. Callers (frontend,
// back office, partner integrations) branch on Code, never on the human
// message, which may change wording without notice.
type Code string

const (
	CodeValidation     Code = "validation_error"
	CodeUnauthorized   Code = "unauthorized"
	CodeForbidden      Code = "forbidden"
	CodeNotFound       Code = "not_found"
	CodeConflict       Code = "conflict"
	CodeInternal       Code = "internal_error"
	CodeUnavailable    Code = "service_unavailable"
	CodeTenantMismatch Code = "tenant_mismatch"
)

// Error is the wire format for an API error response.
type Error struct {
	Code    Code   `json:"code"`
	Message string `json:"message"`
	// RequestID lets an operator correlate a returned error with server
	// logs/traces without exposing internal details in Message.
	RequestID string `json:"request_id,omitempty"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }

// httpStatus maps an error code to its HTTP status. Kept in one place so
// every handler produces a consistent status for the same error kind.
func httpStatus(c Code) int {
	switch c {
	case CodeValidation:
		return http.StatusBadRequest
	case CodeUnauthorized:
		return http.StatusUnauthorized
	case CodeForbidden, CodeTenantMismatch:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeConflict:
		return http.StatusConflict
	case CodeUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Write sends the standard error envelope. message is always safe to show
// a caller - never pass an internal error's raw Error() string for
// CodeInternal; log the detail server-side and return a generic message
// here instead.
func Write(w http.ResponseWriter, requestID string, code Code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(httpStatus(code))
	_ = json.NewEncoder(w).Encode(Error{
		Code:      code,
		Message:   message,
		RequestID: requestID,
	})
}
