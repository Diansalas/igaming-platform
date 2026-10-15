package httpserver

// ADR 0115 S4-min (completion audit A6): the staff surface of the RG LIMIT ENFORCEMENT switch
// (migration 0158, internal/rg/enforcement_activation.go).
//
//	GET  /v1/admin/rg/enforcement   PermRGConfigRead   the tenant's activation rows (oldest first)
//	POST /v1/admin/rg/enforcement   PermRGConfigWrite   switch enforcement ON for the tenant or one brand
//
// The switch is OFF by default and ONE-WAY: there is no disable operation anywhere (the table is
// append-only and holds enabling rows only). The POST is therefore a PROTECTIVE write (ADR 0119 rule 8.1:
// no step-up) and is idempotent by state: a repeat for the same scope answers 200 with the stored row and
// writes nothing. The tenant and the actor come from the verified token only; the database additionally
// refuses any writer that is not an active staff principal of that tenant. A refusal is audited in its
// own transaction (rg.enforcement.activation_denied).

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

func registerRGEnforcementRoutes(mux *http.ServeMux, deps Deps) {
	// Player: the server-owned choices of the RG screen (read-only; rg_options_handlers.go).
	mux.Handle("GET /v1/me/rg/options", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newGetMyRGOptionsHandler(deps))))
	mux.Handle("GET /v1/admin/rg/enforcement", staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGConfigRead)(newListRGEnforcementHandler(deps)))))
	mux.Handle("POST /v1/admin/rg/enforcement", staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGConfigWrite)(newActivateRGEnforcementHandler(deps)))))
}

type rgEnforcementResponse struct {
	ID             string   `json:"id"`
	Level          string   `json:"level"` // tenant | brand
	BrandID        *string  `json:"brand_id,omitempty"`
	ReasonCode     string   `json:"reason_code"`
	AttestationRef string   `json:"attestation_ref"`
	CreatedBy      staffRef `json:"created_by"`
	CreatedAt      string   `json:"created_at"`
}

func toRGEnforcementResponse(v staffViewer, a rg.EnforcementActivation) rgEnforcementResponse {
	resp := rgEnforcementResponse{ID: a.ID.String(), Level: "tenant", ReasonCode: a.ReasonCode, AttestationRef: a.AttestationRef,
		CreatedBy: v.ref(staffScopeTenant, a.CreatedByStaff), CreatedAt: a.CreatedAt.UTC().Format(rfc3339)}
	if a.BrandID != nil {
		resp.Level = "brand"
		s := a.BrandID.String()
		resp.BrandID = &s
	}
	return resp
}

type activateRGEnforcementRequest struct {
	BrandID        *uuid.UUID `json:"brand_id,omitempty"`
	ReasonCode     string     `json:"reason_code"`
	AttestationRef string     `json:"attestation_ref"`
}

func newListRGEnforcementHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		viewer := staffViewerOf(c.tc)
		items := []rgEnforcementResponse{}
		err := deps.DB.WithTenant(r.Context(), c.tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := rg.ListEnforcementActivations(ctx, tx)
			if err != nil {
				return err
			}
			for _, a := range rows {
				items = append(items, toRGEnforcementResponse(viewer, a))
			}
			return nil
		})
		if err != nil {
			_ = writeRGError(w, c, "list_enforcement", err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items, "enforcement_enabled": len(items) > 0})
	}
}

func newActivateRGEnforcementHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		var req activateRGEnforcementRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		svc := rgLimits(deps)
		scope := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
			return deps.DB.WithPrincipalScope(ctx, c.tc.TenantID, c.subject, fn)
		}
		var row rg.EnforcementActivation
		var created bool
		err := scope(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			row, created, serr = svc.ActivateEnforcement(ctx, tx, rg.ActivateEnforcementInput{TenantID: c.tc.TenantID, StaffID: c.subject,
				BrandID: req.BrandID, ReasonCode: req.ReasonCode, AttestationRef: req.AttestationRef, Meta: c.meta})
			return serr
		})
		if err != nil {
			rf := writeRGError(w, c, "activate_enforcement", err)
			auditDenial(r.Context(), deps, c, scope, audit.Entry{TenantID: c.tc.TenantID, ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "rg.enforcement.activation_denied", TargetType: "rg_enforcement_activation", Metadata: map[string]any{
					"brand_id": req.BrandID, "reason_code": auditText(req.ReasonCode), "attestation_ref": auditText(req.AttestationRef)}}, rf)
			return
		}
		status := http.StatusCreated
		if !created {
			status = http.StatusOK
		}
		writeJSON(w, status, toRGEnforcementResponse(staffViewerOf(c.tc), row))
	}
}
