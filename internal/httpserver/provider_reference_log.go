package httpserver

import (
	"log/slog"

	"github.com/Diansalas/igaming-platform/internal/providerref"
)

// logProviderReferenceRejected writes the single, allow-listed Warn line
// for a verified callback rejected by the platform provider-reference
// bound (PROVIDER-REF-BOUND-1). Fields: provider_id, tenant_id,
// request_id, and the *providerref.Error's field/reason/byte length/
// SHA-256 prefix. Never the value, and never err's text (a domain error
// wrapping the reference error could, in a future change, gain value
// content) - so an oversize reference can never amplify a log line.
func logProviderReferenceRejected(logger *slog.Logger, event string, err error, providerID, tenantID, requestID string) {
	attrs := []any{"provider_id", providerID, "tenant_id", tenantID, "request_id", requestID}
	if refErr, ok := providerref.AsError(err); ok {
		attrs = append(attrs, refErr.LogAttrs()...)
	}
	logger.Warn(event, attrs...)
}
