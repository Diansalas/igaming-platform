package httpserver

import (
	"errors"
	"net/http"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// newCasinoBootstrapHandler is CAS-PLAY-BOOTSTRAP-1 (ADR 0103): the vendor
// launch-token bootstrap/consume endpoint. It shares the SAME preamble,
// admission bulkhead and phase-1 verification as the bet/win/rollback
// callback route (newCasinoWebhookHandler) - same credential, same
// scheme, same domain tag - but dispatches to casino.Orchestrator.
// BootstrapLaunch instead of ReceiveVerifiedCallback, and has a much
// smaller error surface (ADR 0103 §3.6: exactly one uniform 401 for every
// step 1-3 refusal and every non-matching replay, one constant 403 for a
// step-4 gate denial, everything else 5xx).
func newCasinoBootstrapHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		markWebhookRouteForLogging(r)

		if deps.CasinoOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "casino webhooks are not enabled on this deployment")
			return
		}

		retries429 := func(providerID string) (allows, declared bool) {
			if deps.CasinoOrchestrator == nil {
				return true, false
			}
			sem, ok := deps.CasinoOrchestrator.WebhookRetrySemantics(providerID)
			if !ok {
				return true, false
			}
			return sem.Retries429, true
		}

		// ADR 0097 A2/A3/A4a: the FIRST thing this route does.
		releaseInflight, admitted := deps.webhookAdmission.admitPreAuth(w, r, domainCasino, deps.TrustedProxyCount, func(id string) bool {
			if deps.CasinoOrchestrator == nil {
				return false
			}
			_, ok := deps.CasinoOrchestrator.WebhookScheme(id)
			return ok
		}, retries429)
		if !admitted {
			return
		}
		defer releaseInflight()

		// The shared preamble: provider_id charset, bounded body read,
		// header format, then the platform-wide tenant lookup and active
		// check - identical to every other casino webhook route.
		t, providerID, body, ok := webhookPreamble(w, r, deps, casinoWebhookRoute)
		if !ok {
			return
		}

		// Phase 1 (ADR 0094 §4.1): no transaction held; its own handle
		// read commits before any secret-store fetch.
		var reader webhookauth.TenantReader = deps.DB
		if deps.webhookAdmission != nil {
			tenantKey, providerKey := deps.webhookAdmission.preAuthKeys(t.Slug, providerID, func(id string) bool {
				_, ok := deps.CasinoOrchestrator.WebhookScheme(id)
				return ok
			})
			reader = deps.webhookAdmission.newGatedReader(deps.DB, domainCasino, tenantKey, providerKey)
		}
		verified, err := deps.CasinoOrchestrator.VerifyCallback(r.Context(), reader, t.ID, providerID, webhookauth.Inbound{Header: r.Header, Body: body})
		if errors.Is(err, errDBGateUnavailable) {
			deps.webhookAdmission.writeAdmissionUnavailableAuthError(w, r, domainCasino, t.ID, providerID)
			return
		}

		// ADR 0097 B1/B2: admitted ONLY off the just-verified
		// (tenant_id, provider_id), strictly before deps.DB.WithTenant.
		var releaseDomainTx func()
		if err == nil {
			var admittedVerified bool
			releaseDomainTx, admittedVerified = deps.webhookAdmission.admitVerified(w, r, domainCasino, t.ID, providerID, retries429)
			if !admittedVerified {
				return
			}
			defer func() {
				if releaseDomainTx != nil {
					releaseDomainTx()
				}
			}()
		}

		var result casino.BootstrapResult
		if err == nil {
			result, err = deps.CasinoOrchestrator.BootstrapLaunch(r.Context(), deps.DB, t.ID, providerID, verified)
		}

		var authErr *webhookauth.AuthError
		if errors.As(err, &authErr) {
			if authErr.Reason == webhookauth.ReasonAdmissionUnavailable {
				deps.webhookAdmission.writeAdmissionUnavailableAuthError(w, r, domainCasino, t.ID, providerID)
				return
			}
			logWebhookAuthFailure(logger, "casino_bootstrap_auth_failed", r, requestID, authErr.Reason, &t.ID, providerID, true, authErr.KeyID, authErr.CredentialFingerprint, len(body))
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}
		var refused *casino.BootstrapRefusedError
		if errors.As(err, &refused) {
			// ADR 0103 §3.6 (BS-8): the SAME uniform 401 as every
			// pre-verification failure - the reason is server-side log
			// only, never sent to the vendor.
			logger.Warn("casino_bootstrap_refused", "reason", string(refused.Reason), "provider_id", providerID, "tenant_id", t.ID.String(), "request_id", requestID)
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}
		if errors.Is(err, casino.ErrBootstrapInvariantBroken) {
			logger.Error("casino_bootstrap_invariant_broken", "error", err, "provider_id", providerID, "tenant_id", t.ID.String())
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process bootstrap")
			return
		}
		if err != nil {
			logger.Error("casino_bootstrap_failed", "error", err, "provider_id", providerID)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process bootstrap")
			return
		}

		if result.Denied {
			// ADR 0103 §3.3: the 403 body is CONSTANT - the reason is
			// never disclosed to the vendor (RG status is player-
			// sensitive); it is server-side audit/log only.
			logger.Warn("casino_bootstrap_denied", "reason", result.DeniedReason, "provider_id", providerID, "tenant_id", t.ID.String(), "session_id", result.SessionID.String())
			apierror.Write(w, requestID, apierror.CodeForbidden, "launch not permitted")
			return
		}

		if result.Replayed {
			logger.Info("casino_bootstrap_replayed", "request_id", requestID, "tenant_id", t.ID.String(), "provider_id", providerID)
		}

		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(result.ResponseJSON)
	}
}
