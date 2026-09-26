package httpserver

import (
	"errors"
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// webhookRoute is the per-domain parameter set of the shared webhook
// preamble (Stage 10.2, ADR 0091; design §A "Shared HTTP preamble";
// architect R2 / ruling J4: every webhook domain uses this one preamble -
// never a second, subtly different copy).
type webhookRoute struct {
	// scheme is the domain's platform-defined MOCK wire scheme, whose
	// header names the preamble format-checks.
	scheme webhookauth.Scheme
	// maxBody bounds the raw body read.
	maxBody int
	// authFailedEvent is the allow-listed auth-failure log event, e.g.
	// "payment_webhook_auth_failed".
	authFailedEvent string
	// tenantLookupFailedEvent is logged (with the error) when the
	// platform-wide tenant lookup fails for a reason other than "not
	// found" - a 500, not part of the uniform-401 contract.
	tenantLookupFailedEvent string
}

// webhookPreamble runs the shared, verify-before-parse steps every
// provider-facing webhook route runs BEFORE any tenant-scoped work:
//
//  0. tenantSlug/providerID path values present (else 400 validation);
//  1. provider_id charset;                          } webhookauth
//  2. body read bounded to route.maxBody;           } Scheme.CheckPreamble,
//  3. authentication header format;                 } no tenant/DB work
//  4. platform-wide GetTenantBySlug (unknown -> uniform 401);
//  5. tenant status == "active" (else uniform 401).
//
// Every rejection in 1-5 writes the IDENTICAL 401 "callback rejected"
// response and one allow-listed route.authFailedEvent log line. ok is false
// once a response has been written; the caller must then return.
func webhookPreamble(w http.ResponseWriter, r *http.Request, deps Deps, route webhookRoute) (t identity.Tenant, providerID string, body []byte, ok bool) {
	requestID := observability.RequestIDFromContext(r.Context())
	logger := observability.LoggerFromContext(r.Context(), deps.Logger)

	tenantSlug := r.PathValue("tenantSlug")
	providerID = r.PathValue("providerID")
	if tenantSlug == "" || providerID == "" {
		apierror.Write(w, requestID, apierror.CodeValidation, "tenant slug and provider id are required")
		return identity.Tenant{}, "", nil, false
	}

	pre, ok := route.scheme.CheckPreamble(providerID, r.Header, r.Body, route.maxBody)
	if !ok {
		logWebhookAuthFailure(logger, route.authFailedEvent, r, requestID, pre.Reason, nil, providerID, pre.ProviderIDValid, "", "", pre.BodyLen)
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
		return identity.Tenant{}, "", nil, false
	}
	body = pre.Body

	t, err := identity.GetTenantBySlug(r.Context(), deps.DB, tenantSlug)
	if errors.Is(err, identity.ErrNotFound) {
		logWebhookAuthFailure(logger, route.authFailedEvent, r, requestID, webhookauth.ReasonTenantUnknown, nil, providerID, true, "", "", len(body))
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
		return identity.Tenant{}, "", nil, false
	}
	if err != nil {
		logger.Error(route.tenantLookupFailedEvent, "error", err)
		apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
		return identity.Tenant{}, "", nil, false
	}
	if t.Status != "active" {
		logWebhookAuthFailure(logger, route.authFailedEvent, r, requestID, webhookauth.ReasonTenantInactive, &t.ID, providerID, true, "", "", len(body))
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
		return identity.Tenant{}, "", nil, false
	}
	return t, providerID, body, true
}
