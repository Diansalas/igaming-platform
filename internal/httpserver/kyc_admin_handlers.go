// Stage 4F: staff/compliance verification and document review. Gated by
// PermVerificationRead (read) / PermVerificationReview (approve/reject) -
// see internal/auth/permission.go's own doc comments for the
// separation-of-duties rationale.
package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// kycCaseResponse is the Stage 5 Back Office operator queue's per-row
// shape - a superset of verificationResponse's fields (this is a new,
// separate response type rather than a reuse of that one, since the
// operator queue reasonably needs BrandID/UpdatedAt/ReviewedBy/SubmittedAt/
// ExpiresAt that the player-facing/single-account verificationResponse
// deliberately does not surface). Every field here already exists on
// kyc.Verification - nothing is invented for this endpoint.
type kycCaseResponse struct {
	ID                   string `json:"id"`
	PlayerAccountID      string `json:"player_account_id"`
	BrandID              string `json:"brand_id"`
	Status               string `json:"status"`
	ProviderID           string `json:"provider_id"`
	ProviderReference    string `json:"provider_reference,omitempty"`
	Reason               string `json:"reason,omitempty"`
	SubmittedAt          string `json:"submitted_at,omitempty"`
	ReviewedAt           string `json:"reviewed_at,omitempty"`
	ReviewedBy           string `json:"reviewed_by,omitempty"`
	ExpiresAt            string `json:"expires_at,omitempty"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
	HasVerifiedResidence bool   `json:"has_verified_residence"`
}

func toKYCCaseResponse(v kyc.Verification) kycCaseResponse {
	resp := kycCaseResponse{
		ID: v.ID.String(), PlayerAccountID: v.PlayerAccountID.String(), BrandID: v.BrandID.String(),
		Status: string(v.Status), ProviderID: v.ProviderID, ProviderReference: v.ProviderReference, Reason: v.Reason,
		CreatedAt: v.CreatedAt.Format(rfc3339), UpdatedAt: v.UpdatedAt.Format(rfc3339),
		HasVerifiedResidence: v.HasVerifiedResidence,
	}
	if v.SubmittedAt != nil {
		resp.SubmittedAt = v.SubmittedAt.Format(rfc3339)
	}
	if v.ReviewedAt != nil {
		resp.ReviewedAt = v.ReviewedAt.Format(rfc3339)
	}
	if v.ReviewedBy != uuid.Nil {
		resp.ReviewedBy = v.ReviewedBy.String()
	}
	if v.ExpiresAt != nil {
		resp.ExpiresAt = v.ExpiresAt.Format(rfc3339)
	}
	return resp
}

// newListKYCCasesHandler is the Stage 5 Back Office operator KYC case
// queue: GET /v1/admin/kyc/cases, tenant-wide (unlike
// newListVerificationsForAccountHandler, which requires an already-known
// player_account_id), paginated via the shared Stage 5 pagination
// convention (internal/httpserver/pagination.go). Gated by
// PermVerificationRead - the same read permission the existing
// player_account_id-scoped admin route uses; this is an additional read
// surface over the identical underlying data, not a new capability.
// Optional ?status= (must be one of kyc's own VerificationStatus values)
// and ?player_account_id= (exact match) filters. tenant scoping is
// entirely RLS's own (kyc_verifications' tenant_isolation policy,
// migration 0040), never a WHERE clause this handler adds.
func newListKYCCasesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		p := parsePageParams(r)
		params := kyc.ListVerificationsForTenantParams{Limit: p.Limit, Offset: p.Offset}

		if statusParam := r.URL.Query().Get("status"); statusParam != "" {
			v := validation.New()
			v.RequireOneOf("status", statusParam,
				string(kyc.StatusUnverified), string(kyc.StatusPending), string(kyc.StatusReviewRequired),
				string(kyc.StatusApproved), string(kyc.StatusRejected), string(kyc.StatusExpired))
			if v.HasErrors() {
				apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
				return
			}
			params.Status = kyc.VerificationStatus(statusParam)
		}
		if accountParam := r.URL.Query().Get("player_account_id"); accountParam != "" {
			targetID, err := uuid.Parse(accountParam)
			if err != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "invalid player_account_id")
				return
			}
			params.PlayerAccountID = targetID
		}

		var (
			verifications []kyc.Verification
			total         int
		)
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			verifications, total, err = kyc.ListVerificationsForTenant(ctx, tx, params)
			return err
		})
		if err != nil {
			logger.Error("list_kyc_cases_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list kyc cases")
			return
		}
		items := make([]kycCaseResponse, 0, len(verifications))
		for _, v := range verifications {
			items = append(items, toKYCCaseResponse(v))
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}

// reviewVerificationRequest.VerifiedResidenceCountry is a plain *string
// via ordinary encoding/json unmarshal (nil when the JSON key is absent OR
// explicitly null) - unlike Phase A's licence_id, there is no meaningful
// "clear" semantic to distinguish from "absent" here: both mean "no
// residence determination this call" (kyc.ReviewVerificationParams' own
// doc comment), so json.RawMessage presence-tracking is not needed.

func newListVerificationsForAccountHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		targetID, err := uuid.Parse(r.URL.Query().Get("player_account_id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "player_account_id query parameter is required")
			return
		}

		var verifications []kyc.Verification
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			verifications, err = kyc.ListVerificationsForAccount(ctx, tx, targetID)
			return err
		})
		if err != nil {
			logger.Error("list_verifications_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list verifications")
			return
		}
		resp := make([]verificationResponse, 0, len(verifications))
		for _, v := range verifications {
			resp = append(resp, toVerificationResponse(v))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type reviewVerificationRequest struct {
	Status                   string  `json:"status"`
	Reason                   string  `json:"reason"`
	VerifiedResidenceCountry *string `json:"verified_residence_country"`
}

func newReviewVerificationHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		verificationID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid verification id")
			return
		}

		var req reviewVerificationRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("status", req.Status, string(kyc.StatusApproved), string(kyc.StatusRejected), string(kyc.StatusReviewRequired))
		if req.VerifiedResidenceCountry != nil {
			v.RequireISO3166Alpha2("verified_residence_country", *req.VerifiedResidenceCountry)
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var result kyc.Verification
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = kyc.ReviewVerification(ctx, tx, kyc.ReviewVerificationParams{
				VerificationID: verificationID, StaffID: staffID, NewStatus: kyc.VerificationStatus(req.Status), Reason: req.Reason,
				VerifiedResidenceCountry: req.VerifiedResidenceCountry,
				IPAddress:                clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if errors.Is(err, kyc.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "verification not found")
			return
		}
		if errors.Is(err, kyc.ErrEvidenceCollectionInactive) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "verified residence evidence collection is not currently enabled for this tenant")
			return
		}
		if errors.Is(err, kyc.ErrInvalidTransition) {
			apierror.Write(w, requestID, apierror.CodeConflict, err.Error())
			return
		}
		if err != nil {
			logger.Error("review_verification_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to review verification")
			return
		}
		writeJSON(w, http.StatusOK, toVerificationResponse(result))
	}
}

func newListDocumentsForAccountHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		targetID, err := uuid.Parse(r.URL.Query().Get("player_account_id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "player_account_id query parameter is required")
			return
		}

		var documents []kyc.Document
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			documents, err = kyc.ListDocumentsForAccount(ctx, tx, targetID)
			return err
		})
		if err != nil {
			logger.Error("list_documents_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list documents")
			return
		}
		resp := make([]documentResponse, 0, len(documents))
		for _, d := range documents {
			resp = append(resp, toDocumentResponse(d))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type reviewDocumentRequest struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func newReviewDocumentHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		documentID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid document id")
			return
		}

		var req reviewDocumentRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("status", req.Status, string(kyc.DocumentApproved), string(kyc.DocumentRejected))
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var result kyc.Document
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = kyc.ReviewDocument(ctx, tx, kyc.ReviewDocumentParams{
				DocumentID: documentID, StaffID: staffID, NewStatus: kyc.DocumentStatus(req.Status), Reason: req.Reason,
			})
			return err
		})
		if errors.Is(err, kyc.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "document not found")
			return
		}
		if errors.Is(err, kyc.ErrInvalidTransition) {
			apierror.Write(w, requestID, apierror.CodeConflict, err.Error())
			return
		}
		if err != nil {
			logger.Error("review_document_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to review document")
			return
		}
		writeJSON(w, http.StatusOK, toDocumentResponse(result))
	}
}

func newGetDocumentContentHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.DocumentStorage == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "document access is temporarily unavailable")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff identity")
			return
		}
		documentID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid document id")
			return
		}

		var contentType string
		var content []byte
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			doc, err := kyc.GetDocumentByID(ctx, tx, documentID)
			if err != nil {
				return err
			}
			contentType, content, err = kyc.GetDocumentContent(ctx, tx, deps.DocumentStorage, doc, audit.ActorStaff, staffID)
			return err
		})
		if errors.Is(err, kyc.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "document not found")
			return
		}
		if err != nil {
			logger.Error("get_document_content_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to retrieve document content")
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", "attachment")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(content)
	}
}

// --- Provider webhook ---

// maxKYCWebhookBodyBytes bounds an inbound KYC provider callback body
// (design §G OpenAPI: "at most 256 KiB").
const maxKYCWebhookBodyBytes = 256 * 1024

// kycWebhookRoute is the KYC domain's parameter set for the shared webhook
// preamble (webhook_preamble.go) - Stage 10.2, ADR 0091, architect ruling
// R2/J4: every webhook domain uses the ONE shared preamble.
var kycWebhookRoute = webhookRoute{
	schemeFor: func(deps Deps, providerID string) (webhookauth.VerificationScheme, bool) {
		if deps.KYCOrchestrator == nil {
			return nil, false
		}
		return deps.KYCOrchestrator.WebhookScheme(providerID)
	},
	maxBody:                 maxKYCWebhookBodyBytes,
	authFailedEvent:         "kyc_webhook_auth_failed",
	tenantLookupFailedEvent: "kyc_webhook_tenant_lookup_failed",
}

// newKYCWebhookHandler receives a provider callback and dispatches it via
// kyc.Orchestrator.ReceiveCallback. There is no bearer-token middleware on
// this route - a provider webhook is not an authenticated platform
// principal. Tenant binding is per docs/decisions/0022 §3 as amended by
// Stage 10.2 (ADR 0091, KYC-WH-1): the tenant slug in the URL is only a
// LOOKUP HINT, selecting one candidate credential, which must then verify
// a signature whose input includes the route-resolved tenant_id/
// provider_id - never any field inside the body. Every pre-verification
// failure gets the IDENTICAL 401 "callback rejected" response, so an
// unauthenticated caller can never enumerate which one is true.
//
// This route is registered ONLY when deps.KYCWebhookEnabled &&
// deps.KYCOrchestrator != nil (registerKYCRoutes) - it is never reachable
// with test support off, so the deps.KYCOrchestrator==nil branch below is
// defense in depth only, mirroring newPaymentWebhookHandler's identical
// belt-and-braces check.
func newKYCWebhookHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.KYCOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "kyc webhooks are not enabled on this deployment")
			return
		}

		// Steps 1-5 (provider_id charset, bounded body read, header format
		// - all before any tenant/DB work - then the platform-wide tenant
		// lookup and active check) are the shared webhook preamble. Every
		// rejection there is the IDENTICAL 401 "callback rejected" with one
		// allow-listed kyc_webhook_auth_failed line.
		t, providerID, body, ok := webhookPreamble(w, r, deps, kycWebhookRoute)
		if !ok {
			return
		}

		var applied bool
		err := deps.DB.WithTenant(r.Context(), t.ID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			_, applied, err = deps.KYCOrchestrator.ReceiveCallback(ctx, tx, t.ID, providerID, webhookauth.Inbound{Header: r.Header, Body: body})
			return err
		})

		var authErr *kyc.CallbackAuthError
		if errors.As(err, &authErr) {
			logWebhookAuthFailure(logger, kycWebhookRoute.authFailedEvent, r, requestID, authErr.Reason, &t.ID, providerID, true, authErr.KeyID, authErr.CredentialFingerprint, len(body))
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}
		if errors.Is(err, kyc.ErrNotFound) {
			// A callback for a provider_reference this platform never
			// created - never a platform failure. Reachable only by a
			// VERIFIED caller (never an enumeration oracle for an
			// unauthenticated one - see ReceiveCallback's own doc
			// comment).
			apierror.Write(w, requestID, apierror.CodeNotFound, "verification not found")
			return
		}
		if errors.Is(err, kyc.ErrCallbackMalformedBody) {
			// A VERIFIED callback (the sender proved knowledge of the
			// resolved credential) whose body is structurally malformed -
			// unparseable JSON, a missing provider_reference, or an
			// outcome outside the closed enum. A real 4xx, not the
			// uniform pre-verification 401 (no audit row).
			apierror.Write(w, requestID, apierror.CodeValidation, "callback rejected")
			return
		}
		if err != nil {
			logger.Error("kyc_webhook_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
			return
		}
		// 204: never echo the verification (id, player_account_id,
		// provider_reference) back to the external caller - design §G
		// fixes the pre-fix 200-with-full-body leak.
		if !applied {
			// K4 (Stage 10.2 final review, M4): a single allow-listed
			// informational line for a verified callback that changed
			// nothing - a replay, anything at or behind the
			// verification's current rank, or an outcome=error against an
			// already-terminal verification (K5). request_id/tenant_id/
			// provider_id only - never the body, headers, signature, or
			// which specific no-op case this was (that detail lives only
			// in the verified provider's own delivery log, never ours).
			logger.Info("kyc_webhook_noop", "request_id", requestID, "tenant_id", t.ID.String(), "provider_id", providerID)
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
