// Stage 4F: player verification (KYC) and document management. See
// docs/decisions/0028-kyc-provider-abstraction.md and
// docs/decisions/0029-document-storage-and-security.md.
//
// Ownership check discipline (directive §7's "a player may access only
// their own permitted documents"): kyc_verifications/kyc_documents' own
// RLS (migration 0040) enforces TENANT isolation only, not per-player
// isolation - a deliberate scope decision (see ADR 0029 §4) rather than
// reintroducing player_restrictions' player-scope GUC machinery for a
// brand-new table pair. Every player self-service handler below
// therefore ALSO explicitly checks the resolved row's PlayerAccountID
// against the caller's own server-resolved account id before returning
// anything - never trusting tenant-level visibility alone for a
// player-facing read. A mismatch reports 404, never 403, so a player can
// never learn that a differently-owned id exists at all.
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

const maxDocumentUploadRequestBytes = kyc.MaxDocumentSizeBytes + 64*1024 // headroom for multipart framing/other fields

type verificationResponse struct {
	ID                string `json:"id"`
	PlayerAccountID   string `json:"player_account_id"`
	Status            string `json:"status"`
	ProviderID        string `json:"provider_id"`
	ProviderReference string `json:"provider_reference,omitempty"`
	Reason            string `json:"reason,omitempty"`
	ReviewedAt        string `json:"reviewed_at,omitempty"`
	CreatedAt         string `json:"created_at"`
}

func toVerificationResponse(v kyc.Verification) verificationResponse {
	resp := verificationResponse{
		ID: v.ID.String(), PlayerAccountID: v.PlayerAccountID.String(), Status: string(v.Status),
		ProviderID: v.ProviderID, ProviderReference: v.ProviderReference, Reason: v.Reason,
		CreatedAt: v.CreatedAt.Format(rfc3339),
	}
	if v.ReviewedAt != nil {
		resp.ReviewedAt = v.ReviewedAt.Format(rfc3339)
	}
	return resp
}

// playerVerificationResponse is the PLAYER-facing shape (Stage 10.2,
// ADR 0091, KYC-WH-1 design §B5) - deliberately omits provider_reference:
// once the webhook is tenant-bound and signature-verified (this stage's
// fix), the reference is no longer a bearer capability an attacker could
// forge a callback with, but a player still has no legitimate need to see
// or echo it, and never trusting a player-supplied provider reference is
// this endpoint's own discipline. The staff-facing shape
// (verificationResponse, kyc_admin_handlers.go) keeps it. Used ONLY by
// newCreateMyVerificationHandler/newListMyVerificationsHandler below.
type playerVerificationResponse struct {
	ID              string `json:"id"`
	PlayerAccountID string `json:"player_account_id"`
	Status          string `json:"status"`
	ProviderID      string `json:"provider_id"`
	Reason          string `json:"reason,omitempty"`
	ReviewedAt      string `json:"reviewed_at,omitempty"`
	CreatedAt       string `json:"created_at"`
}

func toPlayerVerificationResponse(v kyc.Verification) playerVerificationResponse {
	resp := playerVerificationResponse{
		ID: v.ID.String(), PlayerAccountID: v.PlayerAccountID.String(), Status: string(v.Status),
		ProviderID: v.ProviderID, Reason: v.Reason, CreatedAt: v.CreatedAt.Format(rfc3339),
	}
	if v.ReviewedAt != nil {
		resp.ReviewedAt = v.ReviewedAt.Format(rfc3339)
	}
	return resp
}

type documentResponse struct {
	ID               string `json:"id"`
	PlayerAccountID  string `json:"player_account_id"`
	VerificationID   string `json:"verification_id"`
	DocumentType     string `json:"document_type"`
	IssuingCountry   string `json:"issuing_country,omitempty"`
	Version          int    `json:"version"`
	Status           string `json:"status"`
	ContentType      string `json:"content_type"`
	SizeBytes        int64  `json:"size_bytes"`
	OriginalFilename string `json:"original_filename"`
	RejectionReason  string `json:"rejection_reason,omitempty"`
	UploadedAt       string `json:"uploaded_at"`
	ReviewedAt       string `json:"reviewed_at,omitempty"`
}

func toDocumentResponse(d kyc.Document) documentResponse {
	resp := documentResponse{
		ID: d.ID.String(), PlayerAccountID: d.PlayerAccountID.String(), VerificationID: d.VerificationID.String(),
		DocumentType: string(d.DocumentType), IssuingCountry: d.IssuingCountry, Version: d.Version, Status: string(d.Status),
		ContentType: d.ContentType, SizeBytes: d.SizeBytes, OriginalFilename: d.OriginalFilename,
		RejectionReason: d.RejectionReason, UploadedAt: d.UploadedAt.Format(rfc3339),
	}
	if d.ReviewedAt != nil {
		resp.ReviewedAt = d.ReviewedAt.Format(rfc3339)
	}
	return resp
}

// --- Player self-service ---

func newCreateMyVerificationHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.KYCOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "identity verification is temporarily unavailable")
			return
		}
		provider, ok := deps.KYCOrchestrator.Provider("mock")
		if !ok {
			logger.Error("kyc_provider_not_registered")
			apierror.Write(w, requestID, apierror.CodeUnavailable, "identity verification is temporarily unavailable")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		var v kyc.Verification
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			v, err = kyc.CreateVerification(ctx, tx, provider, kyc.CreateVerificationParams{
				TenantID: tc.TenantID, BrandID: account.BrandID, PlayerAccountID: account.ID, PersonID: account.PersonID,
			})
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player not found")
			return
		}
		if err != nil {
			logger.Error("create_verification_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to start identity verification")
			return
		}
		writeJSON(w, http.StatusCreated, toPlayerVerificationResponse(v))
	}
}

func newListMyVerificationsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		var verifications []kyc.Verification
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			verifications, err = kyc.ListVerificationsForAccount(ctx, tx, playerAccountID)
			return err
		})
		if err != nil {
			logger.Error("list_my_verifications_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list verifications")
			return
		}
		resp := make([]playerVerificationResponse, 0, len(verifications))
		for _, v := range verifications {
			resp = append(resp, toPlayerVerificationResponse(v))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func newUploadMyDocumentHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.DocumentStorage == nil || deps.MalwareScanner == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "document upload is temporarily unavailable")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxDocumentUploadRequestBytes)
		if err := r.ParseMultipartForm(kyc.MaxDocumentSizeBytes); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid or oversized multipart form")
			return
		}
		verificationID, err := uuid.Parse(r.FormValue("verification_id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid verification_id")
			return
		}
		documentType := r.FormValue("document_type")
		v := validation.New()
		v.RequireOneOf("document_type", documentType,
			string(kyc.DocumentPassport), string(kyc.DocumentNationalID), string(kyc.DocumentDriversLicense),
			string(kyc.DocumentProofOfAddress), string(kyc.DocumentSelfie), string(kyc.DocumentOther))
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		file, header, err := r.FormFile("file")
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "file is required")
			return
		}
		defer func() { _ = file.Close() }()
		content, err := io.ReadAll(io.LimitReader(file, kyc.MaxDocumentSizeBytes+1))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "failed to read uploaded file")
			return
		}
		if len(content) > kyc.MaxDocumentSizeBytes {
			apierror.Write(w, requestID, apierror.CodeValidation, "uploaded file exceeds the size limit")
			return
		}

		var doc kyc.Document
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			verification, err := kyc.GetVerificationByID(ctx, tx, verificationID)
			if err != nil {
				return err
			}
			if verification.PlayerAccountID != account.ID {
				return kyc.ErrNotFound
			}
			var provider kyc.KYCProvider
			if deps.KYCOrchestrator != nil {
				provider, _ = deps.KYCOrchestrator.Provider(verification.ProviderID)
			}
			doc, err = kyc.UploadDocument(ctx, tx, deps.DocumentStorage, deps.MalwareScanner, provider, kyc.UploadDocumentParams{
				TenantID: tc.TenantID, BrandID: account.BrandID, PlayerAccountID: account.ID, PersonID: account.PersonID,
				VerificationID: verificationID, DocumentType: kyc.DocumentType(documentType),
				IssuingCountry: r.FormValue("issuing_country"), Filename: header.Filename, Content: content,
			})
			return err
		})
		if errors.Is(err, kyc.ErrNotFound) || errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "verification not found")
			return
		}
		if errors.Is(err, kyc.ErrMalwareDetected) {
			apierror.Write(w, requestID, apierror.CodeValidation, "uploaded file failed a security scan")
			return
		}
		if errors.Is(err, kyc.ErrUploadInvalid) || errors.Is(err, kyc.ErrInvalidTransition) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("upload_document_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to upload document")
			return
		}
		writeJSON(w, http.StatusCreated, toDocumentResponse(doc))
	}
}

func newListMyDocumentsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}

		var documents []kyc.Document
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			documents, err = kyc.ListDocumentsForAccount(ctx, tx, playerAccountID)
			return err
		})
		if err != nil {
			logger.Error("list_my_documents_failed", "error", err)
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

func newGetMyDocumentContentHandler(deps Deps) http.HandlerFunc {
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
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid token subject")
			return
		}
		docID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid document id")
			return
		}

		var contentType string
		var content []byte
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			doc, err := kyc.GetDocumentByID(ctx, tx, docID)
			if err != nil {
				return err
			}
			if doc.PlayerAccountID != playerAccountID {
				return kyc.ErrNotFound
			}
			contentType, content, err = kyc.GetDocumentContent(ctx, tx, deps.DocumentStorage, doc, audit.ActorPlayer, playerAccountID)
			return err
		})
		if errors.Is(err, kyc.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "document not found")
			return
		}
		if err != nil {
			logger.Error("get_my_document_content_failed", "error", err)
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
