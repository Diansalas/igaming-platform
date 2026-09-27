package httpserver

import (
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// armBodyReadDeadline enforces A5's per-request body read deadline
// (ADR 0097 §5.4). Production leaves body untouched and relies on the
// real OS-level connection deadline (http.NewResponseController,
// best-effort - ignored on a ResponseWriter that doesn't support it, e.g.
// httptest.ResponseRecorder in unit tests). Tests may replace this
// package-level var with a fake driven by an injected admission.Clock
// instead of real time (QA review item 1(a): "parameterize BodyReadTimeout
// behind the same clock/timer abstraction... keeping T11 fully
// deterministic and in the main lane" - see webhook_admission_deadline_
// test.go's clockBoundedReader). A test that swaps this var MUST restore
// it before returning (defer) - it is not safe under t.Parallel with
// another test that also swaps it.
var armBodyReadDeadline = func(w http.ResponseWriter, body io.Reader, deadline time.Time) io.Reader {
	_ = http.NewResponseController(w).SetReadDeadline(deadline)
	return body
}

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
	// domain is this route's ADR 0097 admission domain tag ("payments",
	// "casino" or "kyc") - fixed by the route, never derived from input.
	domain webhookDomain
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

	// ADR 0097 A5: a declared Content-Length over the route's own cap is
	// rejected WITHOUT reading any byte of the body (T10 ORD-2) - the
	// IDENTICAL uniform 401/body_too_large response as today, so this is
	// not a new oracle. A best-effort per-request read deadline is set
	// regardless (BodyReadTimeout defaults conservatively when admission
	// is disabled) so a slow/stalled sender cannot hold the goroutine (and,
	// when admission is enabled, its A4a slot) indefinitely; ignored on a
	// ResponseWriter that doesn't support it (e.g. httptest.ResponseRecorder
	// in unit tests) - SetReadDeadline is best-effort exactly like every
	// other caller of http.NewResponseController in this codebase.
	if r.ContentLength > 0 && r.ContentLength > int64(route.maxBody) {
		logWebhookAuthFailure(logger, route.authFailedEvent, r, requestID, webhookauth.ReasonBodyTooLarge, nil, providerID, webhookauth.ValidProviderID(providerID), "", "", 0)
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
		return identity.Tenant{}, "", nil, false
	}
	bodyReadTimeout := 10 * time.Second
	now := time.Now()
	if deps.webhookAdmission != nil {
		bodyReadTimeout = deps.webhookAdmission.settings.BodyReadTimeout
		now = deps.webhookAdmission.clock.Now()
	}
	bodyReader := armBodyReadDeadline(w, r.Body, now.Add(bodyReadTimeout))

	pre, ok := webhookauth.CheckInboundPreamble(providerID, r.Header, bodyReader, route.maxBody, func(id string) (webhookauth.VerificationScheme, bool) {
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

	// ADR 0097 A4b/§5.3: GetTenantBySlug is gated by the pre-verification
	// DB bulkhead, keyed exactly like the A3 preKey. A gate timeout maps to
	// 503 (capacity), never the uniform 401 (auth) - errDBGateUnavailable is
	// distinguished below.
	tenantKey, providerKey := unknownComponent, unknownComponent
	if deps.webhookAdmission != nil {
		tenantKey, providerKey = deps.webhookAdmission.preAuthKeys(tenantSlug, providerID, func(id string) bool {
			if route.schemeFor == nil {
				return false
			}
			_, ok := route.schemeFor(deps, id)
			return ok
		})
	}
	t, err := deps.webhookAdmission.gatedTenantLookup(r.Context(), deps.DB, route.domain, tenantKey, providerKey, tenantSlug)
	if errors.Is(err, errDBGateUnavailable) {
		apierror.Write(w, requestID, apierror.CodeUnavailable, "service temporarily unavailable; retry later")
		return identity.Tenant{}, "", nil, false
	}
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
