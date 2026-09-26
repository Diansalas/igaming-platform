package webhookauth

import (
	"context"
	"log/slog"
)

// VerifiedKeyLogEvent is the structured event LogVerifiedKey emits.
const VerifiedKeyLogEvent = "webhook_key_verified"

// LogVerifiedKey records which key id verified a callback (security C4;
// ADR 0022 §3 point 2, "The log records which key_id verified"; Stage 10.3
// gate W2 condition W2A-SEC-2). Each domain orchestrator calls it once,
// immediately after VerifyInbound succeeds.
//
// It logs only for a KeyImplicit scheme. That is the only selection where
// the matched key is not already named by the request and where a
// verify_only predecessor can match, so it is the line operators read to
// see whether the predecessor is still in use before revoking it. For a
// KeyFromHeader scheme VerifyInbound has already required the header's key
// id to equal the active key id, so the line would add nothing.
//
// The line carries exactly four attributes: request_id, tenant_id,
// provider_id and key_id. It never carries the secret, the fingerprint,
// the signature, the body or any header. A key id is an operator-chosen,
// charset-bounded label (keyIDPattern), not secret material.
//
// base is used as given - not observability.LoggerFromContext - so
// request_id and tenant_id appear once each. A nil base means slog.Default().
func LogVerifiedKey(ctx context.Context, base *slog.Logger, requestID string, scheme VerificationScheme, in Inbound, verified Credential) {
	if scheme == nil || scheme.Properties().KeySelection != KeyImplicit {
		return
	}
	if base == nil {
		base = slog.Default()
	}
	base.LogAttrs(ctx, slog.LevelInfo, VerifiedKeyLogEvent,
		slog.String("request_id", requestID),
		slog.String("tenant_id", in.TenantID.String()),
		slog.String("provider_id", in.ProviderID),
		slog.String("key_id", verified.KeyID),
	)
}
