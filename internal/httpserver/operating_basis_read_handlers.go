package httpserver

// lic9 (migration 0180): the platform READ APIs over the explicit operating bases. PLATFORM scope only
// (PermJurisdictionRegistryRead, platform_admin; not wrapped in RequireTenantScope), step-up none, read-only. Writes are never made
// here: a basis is set, withdrawn or reviewed ONLY by filing a licensing change (POST /v1/admin/licensing-changes with kind
// operating_basis_set / operating_basis_withdraw / operating_basis_review_record), which needs two distinct approvers for anything
// that widens. The target tenant is the platform operator's explicit choice (as for operating-country-policies), never a tenant
// principal's. A stored basis is a recorded business decision, NOT a legal position and NOT a market-permission decision (the
// runtime gate resolves permission).
//
// Decision-makers are derived from the executed change request (requester, executor, approvers); no personal data is stored in the
// basis row itself, and staff ids appear only through the platform viewer.

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

type operatingBasisResponse struct {
	ID                     string    `json:"id"`
	TenantID               string    `json:"tenant_id"`
	BrandID                *string   `json:"brand_id"`
	CountryCode            string    `json:"country_code"`
	OperationCode          string    `json:"operation_code"`
	ProductCode            *string   `json:"product_code"`
	BasisKind              string    `json:"basis_kind"`
	LicenceID              *string   `json:"licence_id"`
	JurisdictionID         *string   `json:"jurisdiction_id"`
	DeterminationReference string    `json:"determination_reference"`
	DeterminedByRole       string    `json:"determined_by_role"`
	EvidenceReference      *string   `json:"basis_evidence_reference"`
	RestrictionsNote       *string   `json:"restrictions_note"`
	RestrictedProductCodes []string  `json:"restricted_product_codes"`
	ReviewDueAt            *string   `json:"review_due_at"`
	NeedsRereview          bool      `json:"needs_rereview"`
	LastReviewReference    *string   `json:"last_review_reference"`
	Status                 string    `json:"status"`
	ReasonCode             string    `json:"reason_code"`
	PolicyVersion          string    `json:"policy_version"`
	EffectiveFrom          string    `json:"effective_from"`
	EffectiveTo            *string   `json:"effective_to"`
	ChangeRequestID        *string   `json:"change_request_id"`
	CreatedBy              *staffRef `json:"created_by"`
	CreatedAt              string    `json:"created_at"`
	Disclaimer             string    `json:"disclaimer"`
}

type operatingBasisApproverJSON struct {
	ApproverID staffRef `json:"approver_id"`
}

type operatingBasisDecisionResponse struct {
	ChangeRequestID string                       `json:"change_request_id"`
	Kind            string                       `json:"kind"`
	ReasonCode      string                       `json:"reason_code"`
	RequestedBy     staffRef                     `json:"requested_by"`
	ExecutedBy      *staffRef                    `json:"executed_by"`
	ExecutedAt      *string                      `json:"executed_at"`
	Approvals       []operatingBasisApproverJSON `json:"approvals"`
}

type operatingBasisDetailResponse struct {
	operatingBasisResponse
	Decision *operatingBasisDecisionResponse `json:"decision"`
}

// operatingBasisDisclaimer rides every basis payload: a recorded basis is a decision of named staff, not a legal position.
const operatingBasisDisclaimer = "An operating basis is a business decision recorded by named staff. It is not a legal or regulatory authorisation and does not by itself permit any market; the runtime gate decides."

func toOperatingBasisResponse(b operatingmarket.BasisRecord) operatingBasisResponse {
	viewer := platformStaffViewer()
	platform := staffScopePlatform
	var due *string
	if b.ReviewDueAt != nil {
		s := b.ReviewDueAt.Format("2006-01-02")
		due = &s
	}
	codes := b.RestrictedProductCodes
	if codes == nil {
		codes = []string{}
	}
	return operatingBasisResponse{
		ID: b.ID.String(), TenantID: b.TenantID.String(), BrandID: uuidPtrString(b.BrandID), CountryCode: b.CountryCode,
		OperationCode: b.OperationCode, ProductCode: b.ProductCode, BasisKind: string(b.Kind), LicenceID: uuidPtrString(b.LicenceID),
		JurisdictionID: uuidPtrString(b.JurisdictionID), DeterminationReference: b.DeterminationReference,
		DeterminedByRole: b.DeterminedByRole, EvidenceReference: b.EvidenceReference, RestrictionsNote: b.RestrictionsNote,
		RestrictedProductCodes: codes, ReviewDueAt: due, NeedsRereview: b.NeedsRereview, LastReviewReference: b.LastReviewReference,
		Status: string(b.Status), ReasonCode: b.ReasonCode, PolicyVersion: b.PolicyVersion, EffectiveFrom: b.EffectiveFrom.Format(rfc3339),
		EffectiveTo: timePtrString(b.EffectiveTo), ChangeRequestID: uuidPtrString(b.ChangeRequestID),
		CreatedBy: viewer.optRef(&platform, b.CreatedByActorID), CreatedAt: b.CreatedAt.Format(rfc3339), Disclaimer: operatingBasisDisclaimer,
	}
}

// newListOperatingBasesHandler is GET /v1/admin/operating-bases?tenant_id=...[&country_code=][&current=true].
func newListOperatingBasesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, logger, actor, ok := readCall(deps, w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		tenantID, err := uuid.Parse(q.Get("tenant_id"))
		if err != nil || tenantID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "tenant_id (a UUID) is required")
			return
		}
		current, okCur := parseBoolParam(q.Get("current"))
		if !okCur {
			apierror.Write(w, requestID, apierror.CodeValidation, "current must be true or false")
			return
		}
		page := parsePageParams(r)
		var recs []operatingmarket.BasisRecord
		var total int
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recs, total, err = operatingmarket.ListOperatingBasesPage(ctx, tx, tenantID, q.Get("country_code"), current, page.Limit, page.Offset)
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "list_operating_bases", err)
			return
		}
		items := make([]operatingBasisResponse, 0, len(recs))
		for _, b := range recs {
			items = append(items, toOperatingBasisResponse(b))
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, page, total))
	}
}

// newGetOperatingBasisHandler is GET /v1/admin/operating-bases/{basisID}.
func newGetOperatingBasisHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, logger, actor, ok := readCall(deps, w, r)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("basisID"))
		if err != nil || id == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "basisID must be a valid UUID")
			return
		}
		var rec operatingmarket.BasisRecord
		var dec *operatingmarket.BasisDecision
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, dec, err = operatingmarket.GetOperatingBasis(ctx, tx, id)
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "get_operating_basis", err)
			return
		}
		out := operatingBasisDetailResponse{operatingBasisResponse: toOperatingBasisResponse(rec)}
		if dec != nil {
			viewer := platformStaffViewer()
			platform := staffScopePlatform
			d := &operatingBasisDecisionResponse{
				ChangeRequestID: dec.RequestID.String(), Kind: dec.Kind, ReasonCode: dec.ReasonCode,
				RequestedBy: viewer.ref(platform, dec.RequestedBy), ExecutedBy: viewer.optRef(&platform, dec.ExecutedBy),
				ExecutedAt: timePtrString(dec.ExecutedAt), Approvals: []operatingBasisApproverJSON{},
			}
			for _, a := range dec.ApproverIDs {
				d.Approvals = append(d.Approvals, operatingBasisApproverJSON{ApproverID: viewer.ref(platform, a)})
			}
			out.Decision = d
		}
		writeJSON(w, http.StatusOK, out)
	}
}
