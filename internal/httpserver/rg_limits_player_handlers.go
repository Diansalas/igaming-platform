package httpserver

// ADR 0115 slice S2: the player surface of the RG limit APIs (see rg_limits_routes.go for the model).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

// rgLimitChangeResponse is one stored limit change as the PLAYER sees it. It carries no staff
// identity at all: a staff-originated change is marked by source staff_on_player_request with its reason code only.
type rgLimitChangeResponse struct {
	ID                   string  `json:"id"`
	Kind                 string  `json:"kind"`
	Scope                string  `json:"scope"`
	AssetCode            *string `json:"asset_code,omitempty"`
	Period               *string `json:"period,omitempty"`
	Value                *string `json:"value"` // null = a removal
	ChangeKind           string  `json:"change_kind"`
	Pending              bool    `json:"pending"`
	RequiresConfirmation bool    `json:"requires_confirmation"`
	RequestedAt          string  `json:"requested_at"`
	EffectiveAt          string  `json:"effective_at"`
	SupersedesChangeID   *string `json:"supersedes_change_id,omitempty"`
	Source               string  `json:"source"`
	ReasonCode           string  `json:"reason_code"`
}

func toRGLimitChangeResponse(c rg.LimitChange, pending bool) rgLimitChangeResponse {
	resp := rgLimitChangeResponse{
		ID: c.ID.String(), Kind: string(c.Kind), Scope: string(c.Scope), AssetCode: c.AssetCode, Value: c.Value,
		ChangeKind: string(c.ChangeKind), Pending: pending, RequiresConfirmation: c.RequiresConfirmation,
		RequestedAt: c.RequestedAt.UTC().Format(time.RFC3339Nano), EffectiveAt: c.EffectiveAt.UTC().Format(time.RFC3339Nano),
		Source: string(c.Source), ReasonCode: c.ReasonCode,
	}
	if c.Period != nil {
		p := string(*c.Period)
		resp.Period = &p
	}
	if c.SupersedesChangeID != nil {
		s := c.SupersedesChangeID.String()
		resp.SupersedesChangeID = &s
	}
	return resp
}

type rgLimitStateResponse struct {
	Kind      string                 `json:"kind"`
	Scope     string                 `json:"scope"`
	AssetCode *string                `json:"asset_code,omitempty"`
	Period    *string                `json:"period,omitempty"`
	HasValue  bool                   `json:"has_value"`
	Value     *string                `json:"value"` // the realized value of this key; null = none / removed
	Pending   *rgLimitChangeResponse `json:"pending,omitempty"`
}

func toRGLimitStateResponse(s rg.LimitState) rgLimitStateResponse {
	r := rgLimitStateResponse{Kind: string(s.Kind), Scope: string(s.Scope), AssetCode: s.AssetCode, HasValue: s.HasValue, Value: s.Value}
	if s.Period != nil {
		p := string(*s.Period)
		r.Period = &p
	}
	if s.Pending != nil {
		p := toRGLimitChangeResponse(*s.Pending, true)
		r.Pending = &p
	}
	return r
}

type rgMyLimitsResponse struct {
	Limits  []rgLimitStateResponse  `json:"limits"`
	History []rgLimitChangeResponse `json:"history"`
}

func historyResponses(changes []rg.LimitChange, states []rg.LimitState) []rgLimitChangeResponse {
	pending := map[uuid.UUID]bool{}
	for _, s := range states {
		if s.Pending != nil {
			pending[s.Pending.ID] = true
		}
	}
	out := make([]rgLimitChangeResponse, 0, len(changes))
	for _, c := range changes {
		out = append(out, toRGLimitChangeResponse(c, pending[c.ID]))
	}
	return out
}

// setLimitRequest is the body of a limit write. `value` is REQUIRED: a decimal string (or integer)
// of minor units / seconds, or null to remove the limit. There is no tenant, person, scope, kind of
// change or time field: those are server-decided, and an unknown field is a 400.
type setLimitRequest struct {
	Kind      string          `json:"kind"`
	AssetCode *string         `json:"asset_code,omitempty"`
	Period    *string         `json:"period,omitempty"`
	Value     json.RawMessage `json:"value"`
}

type staffSetLimitRequest struct {
	setLimitRequest
	ReasonCode  string `json:"reason_code"`
	EvidenceRef string `json:"evidence_ref"`
}

func (q setLimitRequest) toLimitInput(key string) (rg.LimitInput, error) {
	raw := bytes.TrimSpace(q.Value)
	if len(raw) == 0 {
		return rg.LimitInput{}, errors.New("value is required (null removes the limit)")
	}
	in := rg.LimitInput{Kind: rg.ControlKind(q.Kind), AssetCode: q.AssetCode, IdempotencyKey: key}
	if q.Period != nil {
		p := rg.PeriodKind(*q.Period)
		in.Period = &p
	}
	switch {
	case string(raw) == "null":
	case raw[0] == '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return rg.LimitInput{}, errors.New("value is malformed")
		}
		in.Value = &s
	default:
		s := string(raw) // a JSON number token; the service accepts only a positive integer literal
		in.Value = &s
	}
	return in, nil
}

// auditText bounds and cleans a caller-supplied string before it is echoed into an append-only audit row:
// at most 64 bytes of printable ASCII, else a fixed marker. An unvalidated field is never copied verbatim.
func auditText(s string) string {
	if len(s) > 64 {
		return "<invalid>"
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return "<invalid>"
		}
	}
	return s
}

func auditTextP(p *string) any {
	if p == nil {
		return nil
	}
	return auditText(*p)
}

// attemptedValue is the value the caller asked for, as audit text (bounded, printable only).
func attemptedValue(raw json.RawMessage) string {
	s := strings.Trim(string(bytes.TrimSpace(raw)), `"`)
	if len(s) > 64 {
		s = s[:64]
	}
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r > 0x7e {
			return '?'
		}
		return r
	}, s)
}

func replayHeader(w http.ResponseWriter, replayed bool) {
	if replayed {
		w.Header().Set("Idempotent-Replayed", "true")
	}
}

func newSetMyRGLimitHandler(deps Deps, limiter *admission.GCRALimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		if !admitRGWrite(w, c, limiter) {
			return
		}
		key, ok := idempotencyKey(w, r, c.requestID)
		if !ok {
			return
		}
		var req setLimitRequest
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
			return deps.DB.WithPlayerScope(ctx, c.tc.TenantID, c.subject, fn)
		}
		var res rg.ChangeResult
		err = scope(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			res, serr = svc.SetLimit(ctx, tx, rg.SetLimitInput{TenantID: c.tc.TenantID, PlayerAccountID: c.subject, Limit: in, Meta: c.meta})
			return serr
		})
		if err != nil {
			rf := writeRGError(w, c, "set_limit", err)
			auditDenial(r.Context(), deps, c, scope, audit.Entry{TenantID: c.tc.TenantID, ActorType: audit.ActorPlayer, ActorID: c.subject,
				Action: "rg.limit.change_denied", TargetType: "player_rg_limit_change", Metadata: map[string]any{
					"control_kind": auditText(req.Kind), "asset_code": auditTextP(req.AssetCode), "period": auditTextP(req.Period), "value": attemptedValue(req.Value)}}, rf)
			return
		}
		replayHeader(w, res.Replayed)
		status := http.StatusCreated
		if res.Change.Pending() {
			status = http.StatusAccepted
		}
		writeJSON(w, status, toRGLimitChangeResponse(res.Change, res.Change.Pending()))
	}
}

func newResolveMyRGLimitHandler(deps Deps, limiter *admission.GCRALimiter, intent rg.ChangeKind) http.HandlerFunc {
	denied := "rg.limit.change_denied"
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		if !admitRGWrite(w, c, limiter) {
			return
		}
		key, ok := idempotencyKey(w, r, c.requestID)
		if !ok {
			return
		}
		changeID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
			return
		}
		svc := rgLimits(deps)
		scope := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
			return deps.DB.WithPlayerScope(ctx, c.tc.TenantID, c.subject, fn)
		}
		var res rg.ChangeResult
		err = scope(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			res, serr = svc.ResolvePending(ctx, tx, rg.ResolveInput{TenantID: c.tc.TenantID, PlayerAccountID: c.subject,
				ChangeID: changeID, Intent: intent, IdempotencyKey: key, Meta: c.meta})
			return serr
		})
		if err != nil {
			rf := writeRGError(w, c, string(intent), err)
			auditDenial(r.Context(), deps, c, scope, audit.Entry{TenantID: c.tc.TenantID, ActorType: audit.ActorPlayer, ActorID: c.subject,
				Action: denied, TargetType: "player_rg_limit_change", TargetID: changeID.String(),
				Metadata: map[string]any{"intent": string(intent)}}, rf)
			return
		}
		replayHeader(w, res.Replayed)
		writeJSON(w, http.StatusOK, toRGLimitChangeResponse(res.Change, false))
	}
}

type createTimeOutRequest struct {
	DurationHours int `json:"duration_hours"`
}

func newCreateMyTimeOutHandler(deps Deps, limiter *admission.GCRALimiter) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		if !admitRGWrite(w, c, limiter) {
			return
		}
		key, ok := idempotencyKey(w, r, c.requestID)
		if !ok {
			return
		}
		var req createTimeOutRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		svc := rgLimits(deps)
		scope := func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
			return deps.DB.WithPlayerScope(ctx, c.tc.TenantID, c.subject, fn)
		}
		var res rg.TimeOutResult
		err := scope(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var serr error
			res, serr = svc.CreateTimeOut(ctx, tx, rg.TimeOutInput{TenantID: c.tc.TenantID, PlayerAccountID: c.subject,
				DurationHours: req.DurationHours, IdempotencyKey: key, Meta: c.meta})
			return serr
		})
		if err != nil {
			rf := writeRGError(w, c, "time_out", err)
			auditDenial(r.Context(), deps, c, scope, audit.Entry{TenantID: c.tc.TenantID, ActorType: audit.ActorPlayer, ActorID: c.subject,
				Action: "rg.time_out.denied", TargetType: "player_restriction",
				Metadata: map[string]any{"duration_hours": req.DurationHours}}, rf)
			return
		}
		replayHeader(w, res.Replayed)
		status := http.StatusCreated
		if res.Replayed {
			status = http.StatusOK
		}
		writeJSON(w, status, toRestrictionResponse(res.Restriction))
	}
}

func newGetMyRGLimitsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginRGCall(deps, w, r)
		if !ok {
			return
		}
		limit := 50
		if v := r.URL.Query().Get("history_limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "history_limit must be 1..200")
				return
			}
			limit = n
		}
		var resp rgMyLimitsResponse
		err := deps.DB.WithPlayerScope(r.Context(), c.tc.TenantID, c.subject, func(ctx context.Context, tx pgx.Tx) error {
			states, err := rg.CurrentLimits(ctx, tx, c.tc.TenantID, c.subject)
			if err != nil {
				return err
			}
			changes, err := rg.ListChanges(ctx, tx, c.tc.TenantID, c.subject, limit)
			if err != nil {
				return err
			}
			resp.Limits = make([]rgLimitStateResponse, 0, len(states))
			for _, s := range states {
				resp.Limits = append(resp.Limits, toRGLimitStateResponse(s))
			}
			resp.History = historyResponses(changes, states)
			return nil
		})
		if err != nil {
			_ = writeRGError(w, c, "get_limits", err)
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
