// Stage 4I Phase B: the admin HTTP surface for the tenant-scoped
// jurisdiction_evidence_collection_active activation switch (see
// internal/jurisdiction/evidence_collection_active.go's own doc comment
// for the underlying fact this gates). Mirrors
// jurisdiction_admin_handlers.go's own jurisdiction_resolution_active
// block (item B-6) almost exactly, substituting EvidenceType for
// OperationClass - the two differ only in which permission gates them
// (PermJurisdictionEvidenceCollectionActivate, RoleCompliance-only,
// versus PermJurisdictionResolutionActiveWrite, RoleTenantAdmin) per
// that permission's own doc comment in internal/auth/permission.go.
package httpserver

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// --- jurisdiction_evidence_collection_active (Stage 4I Phase B) ---

type evidenceCollectionActiveResponse struct {
	ID           string `json:"id"`
	EvidenceType string `json:"evidence_type"`
	Active       bool   `json:"active"`
}

func toEvidenceCollectionActiveResponse(rec jurisdiction.EvidenceCollectionActiveRecord) evidenceCollectionActiveResponse {
	return evidenceCollectionActiveResponse{ID: rec.ID.String(), EvidenceType: string(rec.EvidenceType), Active: rec.Active}
}

var validJurisdictionEvidenceTypes = []string{
	string(jurisdiction.EvidenceDeclaredResidence), string(jurisdiction.EvidenceVerifiedResidence),
	string(jurisdiction.EvidenceLocationSignal),
}

type setEvidenceCollectionActiveRequest struct {
	Active bool `json:"active"`
	// ReasonCode is required, exactly as it is on every other mutating
	// admin surface in this package's file header convention 2 and on
	// setResolutionActiveRequest's identical field.
	ReasonCode string `json:"reason_code"`
}

// newSetEvidenceCollectionActiveHandler is PUT
// /v1/admin/jurisdiction-evidence-collection/{evidenceType} - a
// tenant-scoped, RoleCompliance-only toggle
// (PermJurisdictionEvidenceCollectionActivate). Unlike
// newSetResolutionActiveHandler's engineering-precondition fact, this one
// gates collection of privacy-sensitive personal data under a
// lawful-basis judgment (HDR-J-3e), which is why it sits with Compliance
// alone rather than RoleTenantAdmin.
func newSetEvidenceCollectionActiveHandler(deps Deps) http.HandlerFunc {
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
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		evidenceType := r.PathValue("evidenceType")
		v := validation.New()
		v.RequireOneOf("evidence_type", evidenceType, validJurisdictionEvidenceTypes...)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		var body setEvidenceCollectionActiveRequest
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v = validation.New()
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var rec jurisdiction.EvidenceCollectionActiveRecord
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, err = jurisdiction.SetEvidenceCollectionActive(ctx, tx, jurisdiction.SetEvidenceCollectionActiveParams{
				TenantID: tc.TenantID, EvidenceType: jurisdiction.EvidenceType(evidenceType), Active: body.Active,
				ActorType: jurisdiction.ActorStaff, ActorID: staffID, ReasonCode: body.ReasonCode,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "set_evidence_collection_active", err)
			return
		}
		writeJSON(w, http.StatusOK, toEvidenceCollectionActiveResponse(rec))
	}
}

func newListEvidenceCollectionActiveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var list []jurisdiction.EvidenceCollectionActiveRecord
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			list, err = jurisdiction.ListEvidenceCollectionActive(ctx, tx, tc.TenantID)
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "list_evidence_collection_active", err)
			return
		}
		resp := make([]evidenceCollectionActiveResponse, 0, len(list))
		for _, rec := range list {
			resp = append(resp, toEvidenceCollectionActiveResponse(rec))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
