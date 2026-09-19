// Stage 4I Phase A
// (docs/plans/stage-4i-jurisdiction-implementation-plan.md Phase A): the
// HTTP write surface for jurisdiction.AssignTenantLicence - the one
// endpoint that binds a tenant's `licence_id` (the FK
// internal/jurisdiction.resolveTenantLicence has always been able to
// read, but that nothing in the application ever wrote before this).
//
// Follows jurisdiction_admin_handlers.go's own conventions exactly: same
// actor-resolution helper (jurisdictionActorFromRequest), same error
// mapper (writeJurisdictionRegistryError). Unlike the jurisdictions/
// licences registry writes in that file, this handler opens its
// transaction via deps.DB.WithTenant(tenantID, ...) rather than
// WithPlatformAdmin - AssignTenantLicence's own audit write is
// TENANT-scoped (this operation's subject is a specific tenant, not a
// platform-wide reference row), mirroring newCreateBrandHandler/
// newCreateStaffHandler's exact convention (admin_routes.go) for
// "platform_admin acts on a target tenant" writes. `tenants`/`licences`
// still carry no row-level security, so nothing about the read/update
// logic depends on which GUC is set - only the audit write's scope does.
// This handler also applies canActOnTenant (admin_routes.go), the same
// ADR 0011 guard every sibling "act on a target tenant" handler applies -
// a no-op today (only platform_admin holds PermTenantLicenceAssign, and
// canActOnTenant always allows a nil-tenant/platform-scoped caller), but
// it closes a latent risk should this permission ever be additionally
// granted to a tenant-scoped role.
package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

type assignTenantLicenceRequest struct {
	// LicenceID is a required JSON key: a JSON `null` unassigns, a UUID
	// string assigns. Presence vs. absence of the key is itself
	// significant (a caller that omits it entirely is almost certainly a
	// mistake, not an intent to unassign), which is why this is
	// json.RawMessage rather than *string.
	LicenceID  json.RawMessage `json:"licence_id"`
	ReasonCode string          `json:"reason_code"`
}

type assignTenantLicenceResponse struct {
	TenantID  string  `json:"tenant_id"`
	LicenceID *string `json:"licence_id"`
}

// newAssignTenantLicenceHandler is PUT /v1/admin/tenants/{tenantID}/licence.
func newAssignTenantLicenceHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		// The uuid.Nil check (beyond a bare parse failure) matters here in
		// a way it didn't before this fix round: db.Pool.WithTenant itself
		// rejects a nil tenant id with a raw internal error, which would
		// otherwise surface as a 500 instead of a controlled 400 - unlike
		// the old WithPlatformAdmin wrapper, AssignTenantLicence's own
		// "tenant_id is required" check never got a chance to run.
		tenantID, err := uuid.Parse(r.PathValue("tenantID"))
		if err != nil || tenantID == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "tenantID must be a valid UUID")
			return
		}
		if !canActOnTenant(tc, tenantID) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "cannot act on a different tenant")
			return
		}

		var body assignTenantLicenceRequest
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var licenceID *uuid.UUID
		switch {
		case body.LicenceID == nil:
			apierror.Write(w, requestID, apierror.CodeValidation, "licence_id is required; send null to unassign")
			return
		case bytes.Equal(bytes.TrimSpace(body.LicenceID), []byte("null")):
			licenceID = nil
		default:
			var raw string
			if err := json.Unmarshal(body.LicenceID, &raw); err != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "licence_id must be a UUID string or null")
				return
			}
			parsed, err := uuid.Parse(raw)
			if err != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "licence_id must be a UUID string or null")
				return
			}
			licenceID = &parsed
		}

		actor, err := jurisdictionActorFromRequest(r, body.ReasonCode)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}

		var state jurisdiction.TenantLicenceState
		err = deps.DB.WithTenant(r.Context(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			state, err = jurisdiction.AssignTenantLicence(ctx, tx, jurisdiction.AssignTenantLicenceParams{
				TenantID: tenantID, LicenceID: licenceID, Actor: actor,
			})
			return err
		})
		if err != nil {
			writeJurisdictionRegistryError(w, requestID, logger, "assign_tenant_licence", err)
			return
		}

		resp := assignTenantLicenceResponse{TenantID: state.TenantID.String()}
		if state.LicenceID != nil {
			s := state.LicenceID.String()
			resp.LicenceID = &s
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
