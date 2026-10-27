// Stage 4I Phase A
// (docs/plans/stage-4i-jurisdiction-implementation-plan.md Phase A): the
// HTTP write surface for jurisdiction.AssignTenantLicence - the one
// endpoint that binds a tenant's `licence_id` (the FK
// internal/jurisdiction.resolveTenantLicence has always been able to
// read, but that nothing in the application ever wrote before this).
//
// Follows jurisdiction_admin_handlers.go's own conventions exactly: same
// actor-resolution helper (jurisdictionActorFromRequest), same error
// mapper (writeJurisdictionRegistryError). AS OF STAGE 4I PHASE E-SECURITY
// (migration 0077), this handler opens its transaction via
// deps.DB.WithPlatformAdmin(...), exactly like jurisdiction_admin_handlers.go's
// registry writes - NOT deps.DB.WithTenant(tenantID, ...) as it did
// through Phase A/E. `tenants`/`licences` gained row-level security with
// no tenant-scoped write policy of any kind (a tenant-scoped write to its
// own `licence_id` was the exact live-reproduced attack this migration
// closes), and AssignTenantLicence's own audit write moved platform-scoped
// as a consequence (see tenant_licence_admin.go's header comment). This
// handler still applies canActOnTenant (admin_routes.go), the same ADR
// 0011 guard every sibling "act on a target tenant" handler applies - a
// no-op today (only platform_admin holds PermTenantLicenceAssign, and
// canActOnTenant always allows a nil-tenant/platform-scoped caller), but
// it closes a latent risk should this permission ever be additionally
// granted to a tenant-scoped role.
//
// SINCE MIGRATION 0177 (MKT-DUAL-1) this handler no longer calls AssignTenantLicence itself: it files a licensing change
// (internal/licensingchange) whose executor calls it. See newAssignTenantLicenceHandler.
package httpserver

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/licensingchange"
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
	// ChangeRequestID is the executed tenant_licence_unbind licensing change (migration 0177).
	ChangeRequestID string `json:"change_request_id,omitempty"`
}

// newAssignTenantLicenceHandler is PUT /v1/admin/tenants/{tenantID}/licence. Since migration 0177 (MKT-DUAL-1, audit G-2)
// the request body is unchanged but the write is governed:
//   - a licence id (bind / rebind, widening) files a tenant_licence_bind licensing change and answers 202 with the change
//     request (refusals the binding would hit - unknown tenant / licence, non-active licence, licensee vs licensing_model,
//     exclusive BYOL licence already bound - are answered now, as before); two approvals execute it;
//   - null (unbind, narrowing: fail closed) files a tenant_licence_unbind change that executes in the SAME transaction
//     (single actor with a reason code) and answers 200 with the previous response shape.
func newAssignTenantLicenceHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLicensingCall(deps, w, r)
		if !ok {
			return
		}
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		tenantID, err := uuid.Parse(r.PathValue("tenantID"))
		if err != nil || tenantID == uuid.Nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "tenantID must be a valid UUID")
			return
		}
		if !canActOnTenant(tc, tenantID) {
			apierror.Write(w, c.requestID, apierror.CodeForbidden, "cannot act on a different tenant")
			return
		}

		var body assignTenantLicenceRequest
		if err := decodeJSON(r, &body); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("reason_code", body.ReasonCode)
		if v.HasErrors() {
			apierror.Write(w, c.requestID, apierror.CodeValidation, v.Error())
			return
		}

		var licenceID *uuid.UUID
		switch {
		case body.LicenceID == nil:
			apierror.Write(w, c.requestID, apierror.CodeValidation, "licence_id is required; send null to unassign")
			return
		case bytes.Equal(bytes.TrimSpace(body.LicenceID), []byte("null")):
			licenceID = nil
		default:
			var raw string
			if err := json.Unmarshal(body.LicenceID, &raw); err != nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "licence_id must be a UUID string or null")
				return
			}
			parsed, err := uuid.Parse(raw)
			if err != nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "licence_id must be a UUID string or null")
				return
			}
			licenceID = &parsed
		}

		in := licensingchange.RequestInput{ReasonCode: body.ReasonCode}
		if licenceID == nil {
			in.Kind = licensingchange.KindTenantLicenceUnbind
			in.Payload, err = json.Marshal(licensingchange.TenantLicenceUnbindPayload{TenantID: tenantID})
		} else {
			in.Kind = licensingchange.KindTenantLicenceBind
			in.Payload, err = json.Marshal(licensingchange.TenantLicenceBindPayload{TenantID: tenantID, LicenceID: *licenceID})
		}
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		res, err := c.svc.Request(r.Context(), c.actor, in)
		if err != nil {
			writeLicensingChangeError(w, c, "assign_tenant_licence", err)
			return
		}
		if res.Executed {
			writeJSON(w, http.StatusOK, assignTenantLicenceResponse{TenantID: tenantID.String(), LicenceID: nil, ChangeRequestID: res.Request.ID.String()})
			return
		}
		writeJSON(w, http.StatusAccepted, toLicensingChangeResponse(res, c.viewer))
	}
}
