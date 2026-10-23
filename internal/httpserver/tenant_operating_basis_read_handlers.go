package httpserver

// c2 (no migration): the TENANT-scoped READ APIs over a tenant's OWN operating bases (migration 0180).
//
//	GET /v1/admin/tenant/operating-bases[?brand_id=][&country_code=][&current=true][&limit=][&offset=]
//	GET /v1/admin/tenant/operating-bases/{basisID}
//
// Authority: PermOperatingBasisTenantRead (tenant_admin, compliance) + RequireTenantScope. The tenant id is the authenticated
// context's and nothing else: there is no tenant path or query parameter (a supplied tenant_id is ignored, never honoured). The
// transaction is db.Pool.WithTenant, so the FORCE-RLS policy operating_bases_tenant_read independently admits only this tenant's
// rows; an unknown id, another tenant's id and another tenant's brand are the same 404. Platform staff use the platform routes
// (/v1/admin/operating-bases), not this one: a platform_admin token has no tenant and is refused here.
//
// Projection (an ALLOW-LIST, see tenantOperatingBasisResponse): scope, kind, status, restricted product codes, review state and
// effective dates only. Deliberately NOT exposed: staff ids (created_by, requester, executor, approvers) and the decision record,
// change_request_id, reason_code, determination / evidence / last-review references, determined_by_role, restrictions_note free
// text (only has_restrictions_note), licence_id and jurisdiction_id (platform registry ids a tenant cannot dereference), and
// tenant_id. The platform marks NOTHING in a basis row as tenant-visible, so every free-text or staff-origin field is withheld.

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

type tenantOperatingBasisResponse struct {
	ID                     string   `json:"id"`
	BrandID                *string  `json:"brand_id"`
	CountryCode            string   `json:"country_code"`
	OperationCode          string   `json:"operation_code"`
	ProductCode            *string  `json:"product_code"`
	BasisKind              string   `json:"basis_kind"`
	RestrictedProductCodes []string `json:"restricted_product_codes"`
	HasRestrictionsNote    bool     `json:"has_restrictions_note"`
	ReviewDueAt            *string  `json:"review_due_at"`
	NeedsRereview          bool     `json:"needs_rereview"`
	Status                 string   `json:"status"`
	IsCurrent              bool     `json:"is_current"`
	PolicyVersion          string   `json:"policy_version"`
	EffectiveFrom          string   `json:"effective_from"`
	EffectiveTo            *string  `json:"effective_to"`
	CreatedAt              string   `json:"created_at"`
	Disclaimer             string   `json:"disclaimer"`
}

func toTenantOperatingBasisResponse(b operatingmarket.BasisRecord) tenantOperatingBasisResponse {
	var due *string
	if b.ReviewDueAt != nil {
		s := b.ReviewDueAt.Format("2006-01-02")
		due = &s
	}
	codes := b.RestrictedProductCodes
	if codes == nil {
		codes = []string{}
	}
	return tenantOperatingBasisResponse{
		ID: b.ID.String(), BrandID: uuidPtrString(b.BrandID), CountryCode: b.CountryCode, OperationCode: b.OperationCode,
		ProductCode: b.ProductCode, BasisKind: string(b.Kind), RestrictedProductCodes: codes, HasRestrictionsNote: b.RestrictionsNote != nil,
		ReviewDueAt: due, NeedsRereview: b.NeedsRereview, Status: string(b.Status), IsCurrent: b.EffectiveTo == nil,
		PolicyVersion: b.PolicyVersion, EffectiveFrom: b.EffectiveFrom.Format(rfc3339), EffectiveTo: timePtrString(b.EffectiveTo),
		CreatedAt: b.CreatedAt.Format(rfc3339), Disclaimer: operatingBasisDisclaimer,
	}
}

// tenantBasisCall is the shared prelude: request id, logger and the AUTHENTICATED tenant (never a client value).
func tenantBasisCall(deps Deps, w http.ResponseWriter, r *http.Request) (requestID string, logger observabilityLogger, tenantID uuid.UUID, ok bool) {
	requestID = observability.RequestIDFromContext(r.Context())
	logger = observability.LoggerFromContext(r.Context(), deps.Logger)
	tc, err := tenant.FromContext(r.Context())
	if err != nil || tc.TenantID == uuid.Nil {
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated tenant context")
		return requestID, logger, uuid.Nil, false
	}
	return requestID, logger, tc.TenantID, true
}

// newListOwnOperatingBasesHandler is GET /v1/admin/tenant/operating-bases.
func newListOwnOperatingBasesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, logger, tenantID, ok := tenantBasisCall(deps, w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		var brand *uuid.UUID
		if v := q.Get("brand_id"); v != "" {
			id, err := uuid.Parse(v)
			if err != nil || id == uuid.Nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "brand_id must be a valid UUID")
				return
			}
			brand = &id
		}
		current, okCur := parseBoolParam(q.Get("current"))
		if !okCur {
			apierror.Write(w, requestID, apierror.CodeValidation, "current must be true or false")
			return
		}
		page := parsePageParams(r)
		var recs []operatingmarket.BasisRecord
		var total int
		err := deps.DB.WithTenant(r.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recs, total, err = operatingmarket.ListOwnOperatingBasesPage(ctx, tx, tenantID, brand, q.Get("country_code"), current, page.Limit, page.Offset)
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "list_own_operating_bases", err)
			return
		}
		items := make([]tenantOperatingBasisResponse, 0, len(recs))
		for _, b := range recs {
			items = append(items, toTenantOperatingBasisResponse(b))
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, page, total))
	}
}

// newGetOwnOperatingBasisHandler is GET /v1/admin/tenant/operating-bases/{basisID}.
func newGetOwnOperatingBasisHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, logger, tenantID, ok := tenantBasisCall(deps, w, r)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("basisID"))
		if err != nil || id == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "basisID must be a valid UUID")
			return
		}
		var rec operatingmarket.BasisRecord
		err = deps.DB.WithTenant(r.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, err = operatingmarket.GetOwnOperatingBasis(ctx, tx, tenantID, id)
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "get_own_operating_basis", err)
			return
		}
		writeJSON(w, http.StatusOK, toTenantOperatingBasisResponse(rec))
	}
}
