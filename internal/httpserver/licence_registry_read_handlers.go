package httpserver

// lic4 slice 4a: the READ APIs a Back Office needs over the jurisdiction / licence registry and the market configuration.
// PLATFORM scope only (PermJurisdictionRegistryRead, platform_admin; not wrapped in RequireTenantScope), step-up none, read-only.
// Every list that is new here uses the shared pagedResponse envelope (?limit=&offset=). Platform staff ids that appear
// (created_by) go through platformStaffViewer: these routes are unreachable by a tenant principal.
// Nothing here resolves a jurisdiction or decides market permission; these are stored versions.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// licenceDetailResponse is a licence with the tenants whose tenants.licence_id names it.
type licenceDetailResponse struct {
	licenceResponse
	TenantIDs []string `json:"tenant_ids"`
}

func toLicenceDetail(l jurisdiction.Licence, tenants []uuid.UUID) licenceDetailResponse {
	ids := make([]string, 0, len(tenants))
	for _, t := range tenants {
		ids = append(ids, t.String())
	}
	return licenceDetailResponse{licenceResponse: toLicenceResponse(l), TenantIDs: ids}
}

// readCall is the shared prelude of a platform read: request id, logger, platform actor.
func readCall(deps Deps, w http.ResponseWriter, r *http.Request) (requestID string, logger observabilityLogger, actor jurisdiction.ActorContext, ok bool) {
	requestID = observability.RequestIDFromContext(r.Context())
	logger = observability.LoggerFromContext(r.Context(), deps.Logger)
	actor, err := jurisdictionActorFromRequest(r, "read")
	if err != nil {
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
		return requestID, logger, actor, false
	}
	return requestID, logger, actor, true
}

func writeReadError(w http.ResponseWriter, requestID string, logger observabilityLogger, action string, err error) {
	if errorsIsNotFound(err) {
		apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
		return
	}
	if errorsIsInvalid(err) {
		apierror.Write(w, requestID, apierror.CodeValidation, "invalid request")
		return
	}
	logger.Error(action+"_failed", "error", err)
	apierror.Write(w, requestID, apierror.CodeInternal, "failed to "+action)
}

func errorsIsNotFound(err error) bool {
	return errors.Is(err, jurisdiction.ErrNotFound) || errors.Is(err, operatingmarket.ErrNotFound)
}

func errorsIsInvalid(err error) bool {
	return errors.Is(err, jurisdiction.ErrInvalidInput) || errors.Is(err, operatingmarket.ErrInvalidInput)
}

// newGetJurisdictionHandler is GET /v1/admin/jurisdictions/{jurisdictionID}.
func newGetJurisdictionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, logger, actor, ok := readCall(deps, w, r)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("jurisdictionID"))
		if err != nil || id == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "jurisdictionID must be a valid UUID")
			return
		}
		var j jurisdiction.Jurisdiction
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			j, err = jurisdiction.GetJurisdiction(ctx, tx, id)
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "get_jurisdiction", err)
			return
		}
		writeJSON(w, http.StatusOK, toJurisdictionResponse(j))
	}
}

// newGetLicenceHandler is GET /v1/admin/licences/{licenceID}.
func newGetLicenceHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, logger, actor, ok := readCall(deps, w, r)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("licenceID"))
		if err != nil || id == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "licenceID must be a valid UUID")
			return
		}
		var l jurisdiction.Licence
		var bound map[uuid.UUID][]uuid.UUID
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if l, err = jurisdiction.GetLicence(ctx, tx, id); err != nil {
				return err
			}
			bound, err = jurisdiction.LicenceBoundTenantIDs(ctx, tx, []uuid.UUID{id})
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "get_licence", err)
			return
		}
		writeJSON(w, http.StatusOK, toLicenceDetail(l, bound[id]))
	}
}

type ceilingVersionResponse struct {
	ID                     string    `json:"id"`
	LicenceID              string    `json:"licence_id"`
	CountryCode            string    `json:"country_code"`
	State                  string    `json:"state"`
	Status                 string    `json:"status"`
	AuthorizationReference *string   `json:"authorization_reference"`
	ReasonCode             string    `json:"reason_code"`
	PolicyVersion          string    `json:"policy_version"`
	EffectiveFrom          string    `json:"effective_from"`
	EffectiveTo            *string   `json:"effective_to"`
	CreatedByActorType     string    `json:"created_by_actor_type"`
	CreatedBy              *staffRef `json:"created_by"`
	CreatedAt              string    `json:"created_at"`
}

func optString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// newListLicenceCountryCeilingsHandler is GET /v1/admin/licence-country-ceilings?licence_id=...[&country_code=][&current=true].
func newListLicenceCountryCeilingsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, logger, actor, ok := readCall(deps, w, r)
		if !ok {
			return
		}
		q := r.URL.Query()
		licenceID, err := uuid.Parse(q.Get("licence_id"))
		if err != nil || licenceID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "licence_id (a UUID) is required")
			return
		}
		current, okCur := parseBoolParam(q.Get("current"))
		if !okCur {
			apierror.Write(w, requestID, apierror.CodeValidation, "current must be true or false")
			return
		}
		page := parsePageParams(r)
		var recs []operatingmarket.CeilingRecord
		var total int
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recs, total, err = operatingmarket.ListLicenceCeilingVersionsPage(ctx, tx, licenceID, q.Get("country_code"), current, page.Limit, page.Offset)
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "list_licence_country_ceilings", err)
			return
		}
		items := make([]ceilingVersionResponse, 0, len(recs))
		platform := staffScopePlatform
		viewer := platformStaffViewer()
		for _, c := range recs {
			id := c.CreatedByActorID
			items = append(items, ceilingVersionResponse{
				ID: c.ID.String(), LicenceID: c.LicenceID.String(), CountryCode: c.CountryCode, State: string(c.State), Status: string(c.Status),
				AuthorizationReference: optString(c.AuthorizationReference), ReasonCode: c.ReasonCode, PolicyVersion: c.PolicyVersion,
				EffectiveFrom: c.EffectiveFrom.Format(rfc3339), EffectiveTo: timePtrString(c.EffectiveTo),
				CreatedByActorType: c.CreatedByActorType, CreatedBy: viewer.optRef(&platform, &id), CreatedAt: c.CreatedAt.Format(rfc3339),
			})
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, page, total))
	}
}

type policyVersionResponse struct {
	ID                     string    `json:"id"`
	TenantID               string    `json:"tenant_id"`
	Scope                  string    `json:"scope"`
	BrandID                *string   `json:"brand_id"`
	OperationCode          *string   `json:"operation_code"`
	ProductCode            *string   `json:"product_code"`
	CountryCode            string    `json:"country_code"`
	State                  string    `json:"state"`
	Status                 string    `json:"status"`
	AuthorizationReference *string   `json:"authorization_reference"`
	ReasonCode             string    `json:"reason_code"`
	PolicyVersion          string    `json:"policy_version"`
	EffectiveFrom          string    `json:"effective_from"`
	EffectiveTo            *string   `json:"effective_to"`
	CreatedByActorType     string    `json:"created_by_actor_type"`
	CreatedBy              *staffRef `json:"created_by"`
	CreatedAt              string    `json:"created_at"`
}

// newListOperatingCountryPoliciesHandler is GET /v1/admin/operating-country-policies?tenant_id=...[&country_code=][&current=true].
// The target tenant is the platform operator's explicit choice (as the tenant id in PUT /v1/admin/tenants/{tenantID}/licence);
// it is never taken from a tenant principal's request.
func newListOperatingCountryPoliciesHandler(deps Deps) http.HandlerFunc {
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
		var recs []operatingmarket.PolicyRecord
		var total int
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recs, total, err = operatingmarket.ListTenantPolicyVersionsPage(ctx, tx, tenantID, q.Get("country_code"), current, page.Limit, page.Offset)
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "list_operating_country_policies", err)
			return
		}
		items := make([]policyVersionResponse, 0, len(recs))
		viewer := platformStaffViewer()
		for _, p := range recs {
			id := p.CreatedByActorID
			var brand *string
			if p.BrandID != nil {
				s := p.BrandID.String()
				brand = &s
			}
			// The author may be tenant or platform staff; this route is platform-only, so the platform viewer sees the id.
			scope := staffScopePlatform
			items = append(items, policyVersionResponse{
				ID: p.ID.String(), TenantID: p.TenantID.String(), Scope: string(p.Scope), BrandID: brand, OperationCode: p.OperationCode,
				ProductCode: p.ProductCode, CountryCode: p.CountryCode, State: string(p.State), Status: string(p.Status),
				AuthorizationReference: optString(p.AuthorizationReference), ReasonCode: p.ReasonCode, PolicyVersion: p.PolicyVersion,
				EffectiveFrom: p.EffectiveFrom.Format(rfc3339), EffectiveTo: timePtrString(p.EffectiveTo),
				CreatedByActorType: p.CreatedByActorType, CreatedBy: viewer.optRef(&scope, &id), CreatedAt: p.CreatedAt.Format(rfc3339),
			})
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, page, total))
	}
}

type tenantJurisdictionConfigResponse struct {
	ID                    string   `json:"id"`
	TenantID              string   `json:"tenant_id"`
	JurisdictionID        string   `json:"jurisdiction_id"`
	KYCRulesetID          *string  `json:"kyc_ruleset_id"`
	AMLRulesetID          *string  `json:"aml_ruleset_id"`
	RGRulesetID           *string  `json:"rg_ruleset_id"`
	ReportingRulesetID    *string  `json:"reporting_ruleset_id"`
	AllowedCurrencies     []string `json:"allowed_currencies"`
	AllowedPaymentMethods []string `json:"allowed_payment_methods"`
	GeoBlockList          []string `json:"geo_block_list"`
	EffectiveFrom         string   `json:"effective_from"`
	EffectiveTo           *string  `json:"effective_to"`
}

// newListTenantJurisdictionConfigsHandler is GET /v1/admin/tenants/{tenantID}/jurisdiction-configs.
func newListTenantJurisdictionConfigsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID, logger, actor, ok := readCall(deps, w, r)
		if !ok {
			return
		}
		tenantID, err := uuid.Parse(r.PathValue("tenantID"))
		if err != nil || tenantID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "tenantID must be a valid UUID")
			return
		}
		page := parsePageParams(r)
		var recs []jurisdiction.TenantJurisdictionConfig
		var total int
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			recs, total, err = jurisdiction.ListTenantJurisdictionConfigs(ctx, tx, tenantID, page.Limit, page.Offset)
			return err
		})
		if err != nil {
			writeReadError(w, requestID, logger, "list_tenant_jurisdiction_configs", err)
			return
		}
		items := make([]tenantJurisdictionConfigResponse, 0, len(recs))
		for _, c := range recs {
			items = append(items, tenantJurisdictionConfigResponse{
				ID: c.ID.String(), TenantID: c.TenantID.String(), JurisdictionID: c.JurisdictionID.String(),
				KYCRulesetID: c.KYCRulesetID, AMLRulesetID: c.AMLRulesetID, RGRulesetID: c.RGRulesetID, ReportingRulesetID: c.ReportingRulesetID,
				AllowedCurrencies: nonNil(c.AllowedCurrencies), AllowedPaymentMethods: nonNil(c.AllowedPaymentMethods), GeoBlockList: nonNil(c.GeoBlockList),
				EffectiveFrom: c.EffectiveFrom.Format(rfc3339), EffectiveTo: timePtrString(c.EffectiveTo),
			})
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, page, total))
	}
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func parseBoolParam(v string) (value, ok bool) {
	switch strings.ToLower(v) {
	case "":
		return false, true
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}
