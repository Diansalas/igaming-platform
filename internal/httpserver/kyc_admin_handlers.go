// Stage 4F: staff/compliance verification and document review. Gated by
// PermVerificationRead (read) / PermVerificationReview (approve/reject) -
// see internal/auth/permission.go's own doc comments for the
// separation-of-duties rationale.
package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

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
	Status string `json:"status"`
	Reason string `json:"reason"`
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
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var result kyc.Verification
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = kyc.ReviewVerification(ctx, tx, kyc.ReviewVerificationParams{
				VerificationID: verificationID, StaffID: staffID, NewStatus: kyc.VerificationStatus(req.Status), Reason: req.Reason,
			})
			return err
		})
		if errors.Is(err, kyc.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "verification not found")
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

func newKYCWebhookHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.KYCOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "kyc webhooks are not enabled on this deployment")
			return
		}

		tenantSlug := r.PathValue("tenantSlug")
		providerID := r.PathValue("providerID")
		if tenantSlug == "" || providerID == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "tenant slug and provider id are required")
			return
		}

		t, err := identity.GetTenantBySlug(r.Context(), deps.DB, tenantSlug)
		if errors.Is(err, identity.ErrNotFound) {
			// Same enumeration-resistance rationale as
			// newCasinoWebhookHandler/newPaymentWebhookHandler: an
			// unrecognized slug and a suspended tenant get the identical
			// not-found response.
			apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
			return
		}
		if err != nil {
			logger.Error("kyc_webhook_tenant_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
			return
		}
		if t.Status != "active" {
			apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxKYCWebhookBodyBytes+1))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "failed to read request body")
			return
		}
		if len(body) > maxKYCWebhookBodyBytes {
			apierror.Write(w, requestID, apierror.CodeValidation, "request body too large")
			return
		}

		var result kyc.Verification
		err = deps.DB.WithTenant(r.Context(), t.ID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = deps.KYCOrchestrator.ReceiveCallback(ctx, tx, t.ID, providerID, body)
			return err
		})
		if errors.Is(err, kyc.ErrUnknownProvider) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
			return
		}
		if errors.Is(err, kyc.ErrNotFound) {
			// A callback for a provider_reference this platform never
			// created - never a platform failure (ADR 0025's identical
			// "provider callback forgery/mismatch" precedent).
			apierror.Write(w, requestID, apierror.CodeNotFound, "verification not found")
			return
		}
		if err != nil {
			// Includes a signature-verification failure surfaced from the
			// adapter's own HandleCallback - never distinguished from any
			// other callback-processing failure at this layer (avoids
			// giving an attacker an oracle for "signature was close").
			logger.Error("kyc_webhook_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeValidation, "failed to process callback")
			return
		}
		writeJSON(w, http.StatusOK, toVerificationResponse(result))
	}
}

const maxKYCWebhookBodyBytes = 256 * 1024
