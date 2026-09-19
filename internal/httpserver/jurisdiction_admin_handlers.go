// Stage 4I item B-1 (docs/governance/stage-4i-canonical-model.md §11.1):
// the admin API for the platform-wide `jurisdictions`/`licences`
// registries, plus item B-6's tenant-scoped jurisdiction_resolution_active
// toggle. Mirrors internal/httpserver/asset_registry_handlers.go's own
// two conventions exactly:
//
//  1. Registry (jurisdictions/licences) handlers run under
//     deps.DB.WithPlatformAdmin - platform-wide reference data, no
//     tenant scope. Resolution-active handlers run under
//     deps.DB.WithTenant with the tenant from the verified token.
//     Neither ever reads a tenant identifier out of a request body.
//  2. Every mutating registry handler passes a reason_code through to
//     the service, which requires it (CLAUDE.md: "actor, tenant, entity,
//     before/after state, IP, reason code").
package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// --- shared helpers ---

func jurisdictionActorFromRequest(r *http.Request, reasonCode string) (jurisdiction.ActorContext, error) {
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		return jurisdiction.ActorContext{}, err
	}
	actorID, err := uuid.Parse(tc.Subject)
	if err != nil {
		return jurisdiction.ActorContext{}, err
	}
	return jurisdiction.ActorContext{
		ActorID: actorID, IPAddress: clientIP(r), UserAgent: r.UserAgent(),
		RequestID: observability.RequestIDFromContext(r.Context()), ReasonCode: reasonCode,
	}, nil
}

func writeJurisdictionRegistryError(w http.ResponseWriter, requestID string, logger interface {
	Error(msg string, args ...any)
}, action string, err error) {
	switch {
	case errors.Is(err, jurisdiction.ErrInvalidInput):
		apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
	case errors.Is(err, jurisdiction.ErrNotFound):
		apierror.Write(w, requestID, apierror.CodeNotFound, err.Error())
	default:
		logger.Error(action+"_failed", "error", err)
		apierror.Write(w, requestID, apierror.CodeInternal, "failed to "+action)
	}
}

// --- jurisdictions ---

type jurisdictionResponse struct {
	ID             string `json:"id"`
	Code           string `json:"code"`
	Name           string `json:"name"`
	RegulatoryBody string `json:"regulatory_body,omitempty"`
	Notes          string `json:"notes,omitempty"`
	CreatedAt      string `json:"created_at"`
}

func toJurisdictionResponse(j jurisdiction.Jurisdiction) jurisdictionResponse {
	return jurisdictionResponse{
		ID: j.ID.String(), Code: j.Code, Name: j.Name,
		RegulatoryBody: j.RegulatoryBody, Notes: j.Notes,
		CreatedAt: j.CreatedAt.Format(rfc3339),
	}
}

type createJurisdictionRequest struct {
	Code           string `json:"code"`
	Name           string `json:"name"`
	RegulatoryBody string `json:"regulatory_body,omitempty"`
	Notes          string `json:"notes,omitempty"`
	ReasonCode     string `json:"reason_code"`
}

func newCreateJurisdictionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var body createJurisdictionRequest
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("code", body.Code)
		v.RequireNonEmpty("name", body.Name)
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		actor, err := jurisdictionActorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var created jurisdiction.Jurisdiction
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = jurisdiction.CreateJurisdiction(ctx, tx, jurisdiction.CreateJurisdictionParams{
				Code: body.Code, Name: body.Name, RegulatoryBody: body.RegulatoryBody, Notes: body.Notes, Actor: actor,
			})
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "create_jurisdiction", err)
			return
		}
		writeJSON(w, http.StatusCreated, toJurisdictionResponse(created))
	}
}

func newListJurisdictionsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		actor, err := jurisdictionActorFromRequest(r, "list")
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var list []jurisdiction.Jurisdiction
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			list, err = jurisdiction.ListJurisdictions(ctx, tx)
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "list_jurisdictions", err)
			return
		}
		resp := make([]jurisdictionResponse, 0, len(list))
		for _, j := range list {
			resp = append(resp, toJurisdictionResponse(j))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- licences ---

type licenceResponse struct {
	ID                string   `json:"id"`
	JurisdictionID    string   `json:"jurisdiction_id"`
	Licensee          string   `json:"licensee"`
	LicenceNumber     string   `json:"licence_number"`
	Status            string   `json:"status"`
	PermittedProducts []string `json:"permitted_products"`
	PermittedMarkets  []string `json:"permitted_markets"`
	CreatedAt         string   `json:"created_at"`
}

func toLicenceResponse(l jurisdiction.Licence) licenceResponse {
	return licenceResponse{
		ID: l.ID.String(), JurisdictionID: l.JurisdictionID.String(), Licensee: l.Licensee,
		LicenceNumber: l.LicenceNumber, Status: l.Status,
		PermittedProducts: l.PermittedProducts, PermittedMarkets: l.PermittedMarkets,
		CreatedAt: l.CreatedAt.Format(rfc3339),
	}
}

type createLicenceRequest struct {
	JurisdictionID    string   `json:"jurisdiction_id"`
	Licensee          string   `json:"licensee"`
	LicenceNumber     string   `json:"licence_number"`
	PermittedProducts []string `json:"permitted_products,omitempty"`
	PermittedMarkets  []string `json:"permitted_markets,omitempty"`
	ReasonCode        string   `json:"reason_code"`
}

func newCreateLicenceHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		var body createLicenceRequest
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireUUID("jurisdiction_id", body.JurisdictionID)
		v.RequireOneOf("licensee", body.Licensee, "platform", "tenant")
		v.RequireNonEmpty("licence_number", body.LicenceNumber)
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		jurisdictionID, err := uuid.Parse(body.JurisdictionID)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "jurisdiction_id must be a valid UUID")
			return
		}
		actor, err := jurisdictionActorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var created jurisdiction.Licence
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			created, err = jurisdiction.CreateLicence(ctx, tx, jurisdiction.CreateLicenceParams{
				JurisdictionID: jurisdictionID, Licensee: body.Licensee, LicenceNumber: body.LicenceNumber,
				PermittedProducts: body.PermittedProducts, PermittedMarkets: body.PermittedMarkets, Actor: actor,
			})
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "create_licence", err)
			return
		}
		writeJSON(w, http.StatusCreated, toLicenceResponse(created))
	}
}

func newListLicencesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		actor, err := jurisdictionActorFromRequest(r, "list")
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var list []jurisdiction.Licence
		err = deps.DB.WithPlatformAdmin(r.Context(), actor.ActorID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			list, err = jurisdiction.ListLicences(ctx, tx)
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "list_licences", err)
			return
		}
		resp := make([]licenceResponse, 0, len(list))
		for _, l := range list {
			resp = append(resp, toLicenceResponse(l))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- jurisdiction_resolution_active (item B-6) ---

type resolutionActiveResponse struct {
	ID             string `json:"id"`
	OperationClass string `json:"operation_class"`
	Active         bool   `json:"active"`
}

func toResolutionActiveResponse(rec jurisdiction.ActiveRecord) resolutionActiveResponse {
	return resolutionActiveResponse{ID: rec.ID.String(), OperationClass: string(rec.OperationClass), Active: rec.Active}
}

var validJurisdictionOperationClasses = []string{
	string(jurisdiction.OperationPlay), string(jurisdiction.OperationCatalogueAvailability),
	string(jurisdiction.OperationBonusIssuance), string(jurisdiction.OperationBonusConversion),
}

type setResolutionActiveRequest struct {
	Active bool `json:"active"`
}

// newSetResolutionActiveHandler is PUT
// /v1/admin/jurisdiction-resolution-active/{operationClass} - a
// tenant-scoped, RoleTenantAdmin-only toggle (PermJurisdictionResolutionActiveWrite).
// Deliberately single-actor (no four-eyes): this records an ENGINEERING
// precondition fact, not a financial or player-facing authorization
// decision, and RISK's own future consumer (R-2b) treats "not recorded"
// as inactive/fail-closed regardless of who flips it.
func newSetResolutionActiveHandler(deps Deps) http.HandlerFunc {
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
		operationClass := r.PathValue("operationClass")
		v := validation.New()
		v.RequireOneOf("operation_class", operationClass, validJurisdictionOperationClasses...)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		var body setResolutionActiveRequest
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}

		var rec jurisdiction.ActiveRecord
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			rec, err = jurisdiction.SetResolutionActive(ctx, tx, jurisdiction.SetResolutionActiveParams{
				TenantID: tc.TenantID, OperationClass: jurisdiction.OperationClass(operationClass), Active: body.Active,
				ActorType: jurisdiction.ActorStaff, ActorID: staffID,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "set_resolution_active", err)
			return
		}
		writeJSON(w, http.StatusOK, toResolutionActiveResponse(rec))
	}
}

func newListResolutionActiveHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		var list []jurisdiction.ActiveRecord
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			list, err = jurisdiction.ListResolutionActive(ctx, tx, tc.TenantID)
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "list_resolution_active", err)
			return
		}
		resp := make([]resolutionActiveResponse, 0, len(list))
		for _, rec := range list {
			resp = append(resp, toResolutionActiveResponse(rec))
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
