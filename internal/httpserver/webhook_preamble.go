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
	// schemeFor selects the verification scheme of the adapter registered
	// for providerID in this domain's orchestrator (Stage 10.3 W1a,
	// WH-VENDOR-SCHEME-1): the preamble no longer hard-codes the MOCK
	// header format, so a real adapter's own headers reach its own
	// scheme. Process-global adapter registry only - no tenant input.
	schemeFor func(deps Deps, providerID string) (webhookauth.VerificationScheme, bool)
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
//  2. body read bounded to route.maxBody;           } CheckInboundPreamble,
//  3. scheme lookup by provider id                  } no tenant/DB work
//     (provider_unregistered - Stage 10.3 W1a);     }
//  4. the provider's own scheme.Extract;            }
//  5. platform-wide GetTenantBySlug (unknown -> uniform 401);
//  6. tenant status == "active" (else uniform 401).
//
// Every rejection in 1-6 writes the IDENTICAL 401 "callback rejected"
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

	pre, ok := webhookauth.CheckInboundPreamble(providerID, r.Header, r.Body, route.maxBody, func(id string) (webhookauth.VerificationScheme, bool) {
		if route.schemeFor == nil {
			return nil, false
		}
		return route.schemeFor(deps, id)
	})
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
