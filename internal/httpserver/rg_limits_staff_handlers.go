package httpserver

// ADR 0115 slice S2: the staff and platform surfaces of the RG limit APIs (see rg_limits_routes.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

// ---- staff: one player's limits ---------------------------------------------------------------

// rgStaffLimitChangeResponse is a limit change as TENANT staff see it. The creator is a staffRef (the
// staff id of the caller's own tenant, else null); it is never a platform staff id.
type rgStaffLimitChangeResponse struct {
	rgLimitChangeResponse
	PlayerAccountID    string   `json:"player_account_id"`
	EvidenceRef        *string  `json:"evidence_ref,omitempty"`
	CreatedByActorType string   `json:"created_by_actor_type"`
	CreatedBy          staffRef `json:"created_by"`
}

func toRGStaffLimitChangeResponse(v staffViewer, c rg.LimitChange, pending bool) rgStaffLimitChangeResponse {
	resp := rgStaffLimitChangeResponse{rgLimitChangeResponse: toRGLimitChangeResponse(c, pending), PlayerAccountID: c.PlayerAccountID.String(),
		EvidenceRef: c.EvidenceRef, CreatedByActorType: c.CreatedByActorType}
	if c.CreatedByActorType == "staff" {
		resp.CreatedBy = v.ref(staffScopeTenant, c.CreatedByActorID) // a tenant-session insert is always a staff user of the tenant
	}
	return resp
}

type rgStaffLimitsResponse struct {
	Limits []rgLimitStateResponse `json:"limits"`
}

type rgStaffHistoryResponse struct {
	History []rgStaffLimitChangeResponse `json:"history"`
}

func playerPathID(w http.ResponseWriter, r *http.Request, c rgCall) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
		return uuid.Nil, false
	}
	return id, true
}

func newStaffGetPlayerRGLimitsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		playerID, ok := playerPathID(w, r, c)
		if !ok {
			return
		}
		var resp rgStaffLimitsResponse
		err := deps.DB.WithTenant(r.Context(), c.tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			states, err := rg.CurrentLimits(ctx, tx, c.tc.TenantID, playerID)
			if err != nil {
				return err
			}
			resp.Limits = make([]rgLimitStateResponse, 0, len(states))
			for _, s := range states {
				resp.Limits = append(resp.Limits, toRGLimitStateResponse(s))
			}
			return nil
		})
		if err != nil {
			_ = writeRGError(w, c, "staff_get_limits", err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func newStaffGetPlayerRGHistoryHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		playerID, ok := playerPathID(w, r, c)
		if !ok {
			return
		}
		limit := 100
		if v := r.URL.Query().Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 500 {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "limit must be 1..500")
				return
			}
			limit = n
		}
		viewer := staffViewerOf(c.tc)
		var resp rgStaffHistoryResponse
		err := deps.DB.WithTenant(r.Context(), c.tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			states, err := rg.CurrentLimits(ctx, tx, c.tc.TenantID, playerID)
			if err != nil {
				return err
			}
			changes, err := rg.ListChanges(ctx, tx, c.tc.TenantID, playerID, limit)
			if err != nil {
				return err
			}
			pending := map[uuid.UUID]bool{}
			for _, s := range states {
				if s.Pending != nil {
					pending[s.Pending.ID] = true
				}
			}
			resp.History = make([]rgStaffLimitChangeResponse, 0, len(changes))
			for _, ch := range changes {
				resp.History = append(resp.History, toRGStaffLimitChangeResponse(viewer, ch, pending[ch.ID]))
			}
			return nil
		})
		if err != nil {
			_ = writeRGError(w, c, "staff_get_history", err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func newStaffSetPlayerRGLimitHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		playerID, ok := playerPathID(w, r, c)
		if !ok {
			return
		}
		key, ok := idempotencyKey(w, r, c.requestID)
		if !ok {
			return
		}
		var req staffSetLimitRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		in, err := req.toLimitInput(key)
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, err.Error())
			return
		}
		svc := rgLimits(deps)
		scope := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
			return deps.DB.WithPrincipalScope(ctx, c.tc.TenantID, c.subject, fn)
		}
		var res rg.ChangeResult
		err = scope(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			res, serr = svc.SetLimitOnPlayerRequest(ctx, tx, rg.StaffSetLimitInput{TenantID: c.tc.TenantID, StaffID: c.subject,
				PlayerAccountID: playerID, Limit: in, ReasonCode: req.ReasonCode, EvidenceRef: req.EvidenceRef, Meta: c.meta})
			return serr
		})
		if err != nil {
			rf := writeRGError(w, c, "staff_set_limit", err)
			auditDenial(r.Context(), deps, c, scope, audit.Entry{TenantID: c.tc.TenantID, ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "rg.limit.change_denied", TargetType: "player_rg_limit_change", Metadata: map[string]any{
					"player_account_id": playerID.String(), "control_kind": auditText(req.Kind), "asset_code": auditTextP(req.AssetCode), "period": auditTextP(req.Period), "value": attemptedValue(req.Value),
					"reason_code": auditText(req.ReasonCode), "source": string(rg.ChangeSourceStaffOnPlayerRequest)}}, rf)
			return
		}
		replayHeader(w, res.Replayed)
		writeJSON(w, http.StatusCreated, toRGStaffLimitChangeResponse(staffViewerOf(c.tc), res.Change, false))
	}
}

// ---- operator configuration -----------------------------------------------------------------------

// rgConfigResponse is one configuration row. created_by is a staffRef: the floor rows are written by
// PLATFORM staff, whose id a tenant caller never sees.
type rgConfigResponse struct {
	ID               string           `json:"id"`
	Level            string           `json:"level"` // floor | tenant | brand
	JurisdictionCode string           `json:"jurisdiction_code"`
	BrandID          *string          `json:"brand_id,omitempty"`
	Config           rg.ConfigContent `json:"config"`
	CreatedBy        staffRef         `json:"created_by"`
	CreatedAt        string           `json:"created_at"`
}

func toRGConfigResponse(v staffViewer, r rg.ConfigRow) rgConfigResponse {
	level, actorScope := "floor", staffScopePlatform
	if r.TenantID != nil {
		level, actorScope = "tenant", staffScopeTenant
		if r.BrandID != nil {
			level = "brand"
		}
	}
	resp := rgConfigResponse{ID: r.ID.String(), Level: level, JurisdictionCode: r.JurisdictionCode, Config: r.Content,
		CreatedBy: v.ref(actorScope, r.CreatedByActorID), CreatedAt: r.CreatedAt.UTC().Format(rfc3339)}
	if r.BrandID != nil {
		s := r.BrandID.String()
		resp.BrandID = &s
	}
	return resp
}

type writeRGOperatorConfigRequest struct {
	BrandID *uuid.UUID       `json:"brand_id,omitempty"`
	Config  rg.ConfigContent `json:"config"`
}

func newListRGLimitConfigsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		var kind *rg.ControlKind
		if v := r.URL.Query().Get("control_kind"); v != "" {
			k := rg.ControlKind(v)
			valid := false
			for _, x := range rg.ConfigControlKinds {
				if x == k {
					valid = true
				}
			}
			if !valid {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "control_kind is not a known control kind")
				return
			}
			kind = &k
		}
		p := parsePageParams(r)
		viewer := staffViewerOf(c.tc)
		var items []rgConfigResponse
		var total int
		err := deps.DB.WithTenant(r.Context(), c.tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			rows, t, err := rg.ListConfigs(ctx, tx, kind, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = t
			for _, row := range rows {
				items = append(items, toRGConfigResponse(viewer, row))
			}
			return nil
		})
		if err != nil {
			_ = writeRGError(w, c, "list_configs", err)
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}

func newWriteRGOperatorConfigHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		var req writeRGOperatorConfigRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		svc := rgLimits(deps)
		scope := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
			return deps.DB.WithPrincipalScope(ctx, c.tc.TenantID, c.subject, fn)
		}
		var row rg.ConfigRow
		err := scope(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			row, serr = svc.WriteOperatorConfig(ctx, tx, rg.OperatorConfigInput{TenantID: c.tc.TenantID, StaffID: c.subject,
				BrandID: req.BrandID, Content: req.Config, Meta: c.meta})
			return serr
		})
		if err != nil {
			rf := writeRGError(w, c, "operator_config", err)
			auditDenial(r.Context(), deps, c, scope, audit.Entry{TenantID: c.tc.TenantID, ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "rg.limit_config.operator_denied", TargetType: "rg_limit_config", Metadata: map[string]any{
					"control_kind": auditText(string(req.Config.ControlKind)), "asset_code": auditTextP(req.Config.AssetCode), "brand_id": req.BrandID,
					"reason_code": auditText(req.Config.ReasonCode), "attestation_ref": auditText(req.Config.AttestationRef)}}, rf)
			return
		}
		writeJSON(w, http.StatusCreated, toRGConfigResponse(staffViewerOf(c.tc), row))
	}
}

// ---- jurisdiction floor change requests (platform) -------------------------------------------------

type rgFloorRequestResponse struct {
	ID               string          `json:"id"`
	JurisdictionCode string          `json:"jurisdiction_code"`
	ControlKind      string          `json:"control_kind"`
	AssetCode        *string         `json:"asset_code,omitempty"`
	ProposedConfig   json.RawMessage `json:"proposed_config"`
	PayloadHash      string          `json:"payload_hash"`
	ReasonCode       string          `json:"reason_code"`
	RequestedBy      staffRef        `json:"requested_by"`
	Status           string          `json:"status"`
	DecidedBy        *staffRef       `json:"decided_by,omitempty"`
	DecidedAt        *string         `json:"decided_at,omitempty"`
	CreatedAt        string          `json:"created_at"`
	ExpiresAt        string          `json:"expires_at"`
	AppliedConfigID  *string         `json:"applied_config_id,omitempty"`
}

func toRGFloorRequestResponse(f rg.FloorRequest) rgFloorRequestResponse {
	v, platform := platformStaffViewer(), staffScopePlatform
	resp := rgFloorRequestResponse{ID: f.ID.String(), JurisdictionCode: f.JurisdictionCode, ControlKind: string(f.ControlKind), AssetCode: f.AssetCode,
		ProposedConfig: f.ProposedConfig, PayloadHash: f.PayloadHash, ReasonCode: f.ReasonCode, RequestedBy: v.ref(platform, f.RequestedBy),
		Status: f.Status, DecidedBy: v.optRef(&platform, f.DecidedBy), CreatedAt: f.CreatedAt.UTC().Format(rfc3339), ExpiresAt: f.ExpiresAt.UTC().Format(rfc3339)}
	if f.DecidedAt != nil {
		s := f.DecidedAt.UTC().Format(rfc3339)
		resp.DecidedAt = &s
	}
	if f.AppliedConfigID != nil {
		s := f.AppliedConfigID.String()
		resp.AppliedConfigID = &s
	}
	return resp
}

type requestRGFloorRequest struct {
	JurisdictionCode string           `json:"jurisdiction_code"`
	Config           rg.ConfigContent `json:"config"`
}

func newRequestRGFloorHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		var req requestRGFloorRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		svc := rgLimits(deps)
		scope := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
			return deps.DB.WithPlatformAdmin(ctx, c.subject, fn)
		}
		var fr rg.FloorRequest
		err := scope(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			fr, serr = svc.RequestFloor(ctx, tx, rg.FloorRequestInput{StaffID: c.subject, JurisdictionCode: req.JurisdictionCode, Content: req.Config, Meta: c.meta})
			return serr
		})
		if err != nil {
			rf := writeRGError(w, c, "floor_request", err)
			auditDenial(r.Context(), deps, c, scope, audit.Entry{ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "rg.limit_config.floor_denied", TargetType: "rg_limit_config_change_request", Metadata: map[string]any{
					"jurisdiction_code": auditText(req.JurisdictionCode), "control_kind": auditText(string(req.Config.ControlKind)), "op": "request"}}, rf)
			return
		}
		writeJSON(w, http.StatusCreated, toRGFloorRequestResponse(fr))
	}
}

func newDecideRGFloorHandler(deps Deps, decision rg.FloorDecision) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		svc := rgLimits(deps)
		scope := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
			return deps.DB.WithPlatformAdmin(ctx, c.subject, fn)
		}
		var fr rg.FloorRequest
		err = scope(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			fr, serr = svc.DecideFloor(ctx, tx, rg.DecideFloorInput{StaffID: c.subject, RequestID: id, Decision: decision, Meta: c.meta})
			return serr
		})
		if err != nil {
			rf := writeRGError(w, c, "floor_"+string(decision), err)
			auditDenial(r.Context(), deps, c, scope, audit.Entry{ActorType: audit.ActorStaff, ActorID: c.subject,
				Action: "rg.limit_config.floor_denied", TargetType: "rg_limit_config_change_request", TargetID: id.String(),
				Metadata: map[string]any{"op": string(decision)}}, rf)
			return
		}
		writeJSON(w, http.StatusOK, toRGFloorRequestResponse(fr))
	}
}

func newListRGFloorRequestsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		status := r.URL.Query().Get("status")
		switch status {
		case "", "pending", "approved", "rejected", "cancelled":
		default:
			apierror.Write(w, c.requestID, apierror.CodeValidation, "status must be pending, approved, rejected or cancelled")
			return
		}
		p := parsePageParams(r)
		var items []rgFloorRequestResponse
		var total int
		err := deps.DB.WithPlatformAdmin(r.Context(), c.subject, func(ctx context.Context, tx pgx.Tx) error {
			rows, t, err := rg.ListFloorRequests(ctx, tx, status, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = t
			for _, f := range rows {
				items = append(items, toRGFloorRequestResponse(f))
			}
			return nil
		})
		if err != nil {
			_ = writeRGError(w, c, "list_floor_requests", err)
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}
