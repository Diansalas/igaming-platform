package httpserver

// ADR 0115 S4-min (completion audit A6): the staff surface of the RG LIMIT ENFORCEMENT switch
// (migration 0158, internal/rg/enforcement_activation.go).
//
//	GET  /v1/admin/rg/enforcement   PermRGConfigRead   the tenant's activation rows (oldest first)
//	POST /v1/admin/rg/enforcement   PermRGConfigWrite   switch enforcement ON for the tenant or one brand
//
// The switch is OFF by default and ONE-WAY: there is no disable operation anywhere (the table is
// append-only and holds enabling rows only). Because it is irreversible and, with no compliance floor in force,
// halts every deposit, real launch and wager of the scope (rg_config_missing), the POST is step-up CRITICAL
// (security review SR6-1; ADR 0119 treats protective writes that are outage vectors as critical) and it is
// REFUSED (409) unless every jurisdiction in effect for the tenant has a compliance floor for the deposit, loss
// and wager kinds in every allowed currency (the floor part of the R-RG-CONFIG readiness check). It is
// idempotent by state: a repeat for an already switched-on scope answers 200 with the stored row and
// writes nothing (the precondition is not re-evaluated for a repeat). The tenant and the actor come from the verified token only; the database additionally
// refuses any writer that is not an active staff principal of that tenant. A refusal is audited in its
// own transaction (rg.enforcement.activation_denied).

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

func registerRGEnforcementRoutes(mux *http.ServeMux, deps Deps) {
	// Player: the server-owned choices of the RG screen (read-only; rg_options_handlers.go).
	mux.Handle("GET /v1/me/rg/options", auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(newGetMyRGOptionsHandler(deps))))
	mux.Handle("GET /v1/admin/rg/enforcement", staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGConfigRead)(newListRGEnforcementHandler(deps)))))
	mux.Handle("POST /v1/admin/rg/enforcement", staffRoute(deps, stepUpCritical, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGConfigWrite)(newActivateRGEnforcementHandler(deps)))))
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

// errRGActivationNotReady is the SR6-1 precondition failure (not a database error).
type errRGActivationNotReady struct{ result, detail string }

func (e *errRGActivationNotReady) Error() string {
	return "rg enforcement activation precondition: R-RG-CONFIG " + e.result + " " + e.detail
}

// rgRefusedActivationNotReady is the audit-only code of that refusal.
const rgRefusedActivationNotReady rg.RefusalCode = "activation_not_ready"

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
			// SR6-1 guard: refuse unless the floors are in force (a repeat of an existing scope is not re-checked).
			existing, lerr := rg.ListEnforcementActivations(ctx, tx)
			if lerr != nil {
				return lerr
			}
			already := false
			for _, a := range existing {
				if (a.BrandID == nil && req.BrandID == nil) || (a.BrandID != nil && req.BrandID != nil && *a.BrandID == *req.BrandID) {
					already = true
				}
			}
			if !already {
				if chk := launchgov.RGActivationReadiness(ctx, tx, c.tc.TenantID, req.BrandID, time.Now()); chk.Result != launchgov.ResultPass {
					return &errRGActivationNotReady{result: chk.Result, detail: chk.DetailCode}
				}
			}
			var serr error
			row, created, serr = svc.ActivateEnforcement(ctx, tx, rg.ActivateEnforcementInput{TenantID: c.tc.TenantID, StaffID: c.subject,
				BrandID: req.BrandID, ReasonCode: req.ReasonCode, AttestationRef: req.AttestationRef, Meta: c.meta})
			return serr
		})
		var notReady *errRGActivationNotReady
		if errors.As(err, &notReady) {
			rf := &rg.Refusal{Code: rgRefusedActivationNotReady}
			apierror.Write(w, c.requestID, apierror.CodeConflict, "RG limit enforcement cannot be switched on yet: readiness check R-RG-CONFIG is "+notReady.result+" ("+notReady.detail+")")
			auditDenial(r.Context(), deps, c, scope, audit.Entry{TenantID: c.tc.TenantID, ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "rg.enforcement.activation_denied", TargetType: "rg_enforcement_activation", Metadata: map[string]any{
					"brand_id": req.BrandID, "readiness_result": notReady.result, "readiness_detail": notReady.detail}}, rf)
			return
		}
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
