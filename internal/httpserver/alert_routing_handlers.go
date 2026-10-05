package httpserver

// ALERT-DELIVERY-1 routing readiness (ADR 0102 section 18): platform-scope
// authoring and visibility of alert routing.
//
//	POST /v1/admin/alerting/routes   alert:route_manage  create or supersede a route version
//	GET  /v1/admin/alerting/routes   alert:route_manage  current (or all) route versions
//	GET  /v1/admin/alerting/status   alert:manage        readiness per severity, exact missing operator input
//	GET  /v1/admin/alerts            alert:manage        platform alerts with derived delivery state
//
// Every endpoint is platform scope only. Three layers, each pinned by its own
// test: the permission, an explicit tc.TenantID == uuid.Nil check, and the
// database (RLS family + alerting_validated_platform_admin). The permission is
// checked inside the POST handler so a refused attempt writes a denied audit
// row in the caller's own scope (security L-1).
//
// A route is configuration, not a notification. No route here reaches a
// person: only the log and mock channel kinds exist, both non-human, and the
// database refuses to enable a route for any human-notification kind (SR-7:
// four-eyes not built). recipient_ref is an opaque vendor target id; it is
// never recorded verbatim in the audit log (only "recipient_ref_present"), and
// the database row is the sole place it lives.

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// AlertRoutingSource is what the status endpoint reads: the dispatcher's
// cached, staleness-applied readiness evaluation. nil (no dispatcher in this
// process) reports every severity not ready.
type AlertRoutingSource interface {
	ReadinessEffective() []alerting.SeverityReadiness
	ReadinessEvaluatedAt() time.Time
}

func registerAlertRoutingRoutes(mux *http.ServeMux, deps Deps, staff func(http.Handler) http.Handler) {
	mux.Handle("POST /v1/admin/alerting/routes", staff(newCreateAlertRouteHandler(deps)))
	mux.Handle("GET /v1/admin/alerting/routes",
		staff(auth.RequirePermission(auth.PermAlertRouteManage)(newListAlertRoutesHandler(deps))))
	mux.Handle("GET /v1/admin/alerting/status",
		staff(auth.RequirePermission(auth.PermAlertManage)(newAlertStatusHandler(deps))))
	mux.Handle("GET /v1/admin/alerts",
		staff(auth.RequirePermission(auth.PermAlertManage)(newListAlertsHandler(deps))))
}

// platformAlertCaller authenticates and applies the explicit platform-scope
// gate used by every read endpoint here.
func platformAlertCaller(w http.ResponseWriter, r *http.Request) (tc tenant.Context, actor uuid.UUID, requestID string, ok bool) {
	requestID = observability.RequestIDFromContext(r.Context())
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
		return tc, actor, requestID, false
	}
	actor, err = uuid.Parse(tc.Subject)
	if err != nil {
		apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
		return tc, actor, requestID, false
	}
	if tc.TenantID != uuid.Nil {
		apierror.Write(w, requestID, apierror.CodeForbidden, "insufficient permissions for this operation")
		return tc, actor, requestID, false
	}
	return tc, actor, requestID, true
}

var routeReasonCodes = map[string]bool{
	"initial_setup": true, "on_call_change": true, "escalation_change": true,
	"correction": true, "decommission": true, "drill": true,
}

var routeChannelKinds = map[alerting.ChannelKind]bool{alerting.ChannelLog: true, alerting.ChannelMock: true}

type createAlertRouteRequest struct {
	Severity             string   `json:"severity"`
	EscalationStep       int      `json:"escalation_step"`
	ChannelKind          string   `json:"channel_kind"`
	RecipientRef         *string  `json:"recipient_ref"`
	EscalateAfterSeconds *float64 `json:"escalate_after_seconds"`
	Enabled              bool     `json:"enabled"`
	ReasonCode           string   `json:"reason_code"`
	SupersedesID         *string  `json:"supersedes_id"`
}

type alertRouteDTO struct {
	ID                   string     `json:"id"`
	Severity             string     `json:"severity"`
	EscalationStep       int        `json:"escalation_step"`
	ChannelKind          string     `json:"channel_kind"`
	RecipientRef         *string    `json:"recipient_ref,omitempty"`
	EscalateAfterSeconds *float64   `json:"escalate_after_seconds,omitempty"`
	Enabled              bool       `json:"enabled"`
	ReasonCode           *string    `json:"reason_code,omitempty"`
	HumanNotification    bool       `json:"human_notification"`
	EffectiveFrom        time.Time  `json:"effective_from"`
	SupersededAt         *time.Time `json:"superseded_at,omitempty"`
	SupersededBy         *string    `json:"superseded_by,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
}

const alertRouteColumns = `id::text, severity, escalation_step, channel_kind, recipient_ref,
	EXTRACT(EPOCH FROM escalate_after)::float8, enabled, reason_code, effective_from, superseded_at, superseded_by::text, created_at`

func scanAlertRoute(row pgx.Row) (alertRouteDTO, error) {
	var d alertRouteDTO
	err := row.Scan(&d.ID, &d.Severity, &d.EscalationStep, &d.ChannelKind, &d.RecipientRef,
		&d.EscalateAfterSeconds, &d.Enabled, &d.ReasonCode, &d.EffectiveFrom, &d.SupersededAt, &d.SupersededBy, &d.CreatedAt)
	d.HumanNotification = alerting.KnownHumanChannelKinds[alerting.ChannelKind(d.ChannelKind)]
	return d, err
}

var (
	errRouteNotFound        = errors.New("route to supersede not found")
	errRouteSupersedeMatch  = errors.New("supersedes a route for a different severity or step")
	errRouteCurrentExists   = errors.New("a current route exists for this severity and step")
	routeWriteAdvisoryKey   = int64(0x0a1e7_0117) // serialises route writers; not a tenant-isolation mechanism
	maxEscalateAfterSeconds = float64(7 * 24 * 3600)
)

func newCreateAlertRouteHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		actor, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		deny := alertDenial{action: "alert_route.create", targetType: "alert_route"}
		// Layer 1: permission (inside the handler so the refusal is audited).
		if !auth.RoleHasPermission(auth.Role(tc.Role), auth.PermAlertRouteManage) {
			deny.reason = "permission"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeForbidden, "insufficient permissions for this operation")
			return
		}
		// Layer 2: explicit platform scope.
		if tc.TenantID != uuid.Nil {
			deny.reason = "platform_scope_required"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeForbidden, "insufficient permissions for this operation")
			return
		}
		var req createAlertRouteRequest
		if err := decodeJSON(r, &req); err != nil {
			deny.reason = "invalid_body"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		invalid := func(reason, msg string) {
			deny.reason = reason
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeValidation, msg)
		}
		sev := alerting.Severity(req.Severity)
		switch sev {
		case alerting.SeverityP1, alerting.SeverityP2, alerting.SeverityP3:
		default:
			invalid("severity", "severity must be p1, p2 or p3")
			return
		}
		if req.EscalationStep < 0 || req.EscalationStep > 9 {
			invalid("escalation_step", "escalation_step must be between 0 and 9")
			return
		}
		kind := alerting.ChannelKind(req.ChannelKind)
		if !routeChannelKinds[kind] {
			invalid("channel_kind", "channel_kind is not a known channel kind")
			return
		}
		if !routeReasonCodes[req.ReasonCode] {
			invalid("reason_code", "reason_code is required and must be from the closed vocabulary")
			return
		}
		if req.RecipientRef != nil {
			if err := alerting.ValidateRecipientRef(*req.RecipientRef); err != nil {
				invalid("recipient_ref", "recipient_ref must be an opaque target id (no email, phone, address or key)")
				return
			}
		}
		if req.Enabled && req.RecipientRef == nil {
			invalid("enabled_requires_recipient", "an enabled route requires a recipient_ref")
			return
		}
		if req.EscalateAfterSeconds != nil && (*req.EscalateAfterSeconds <= 0 || *req.EscalateAfterSeconds > maxEscalateAfterSeconds) {
			invalid("escalate_after", "escalate_after_seconds must be positive and at most 7 days")
			return
		}
		// Security M-3: in production a p1/p2 route on a channel that reaches
		// no person would hide the unrouted signal behind a "sent" row.
		if deps.AlertRoutingProduction && req.Enabled && sev != alerting.SeverityP3 && !alerting.ChannelKindIsHumanNotification(kind) {
			invalid("non_human_p1_p2_in_production", "a p1/p2 route cannot be enabled on a channel that does not notify a person in production")
			return
		}
		var supersedes uuid.UUID
		if req.SupersedesID != nil {
			supersedes, err = uuid.Parse(*req.SupersedesID)
			if err != nil {
				invalid("supersedes_id", "supersedes_id must be a route id")
				return
			}
			deny.targetID = supersedes.String()
		}

		var out alertRouteDTO
		err = deps.DB.WithPlatformAdmin(r.Context(), actor, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, routeWriteAdvisoryKey); err != nil {
				return err
			}
			var before map[string]any
			var recipientChanged bool
			if req.SupersedesID != nil {
				var oldSev string
				var oldStep int
				var oldEnabled bool
				var oldKind string
				var oldRecipient *string
				err := tx.QueryRow(ctx, `SELECT severity, escalation_step, enabled, channel_kind, recipient_ref FROM alert_routes
					WHERE id = $1 AND scope = 'platform' AND superseded_at IS NULL FOR UPDATE`, supersedes).Scan(&oldSev, &oldStep, &oldEnabled, &oldKind, &oldRecipient)
				if errors.Is(err, pgx.ErrNoRows) {
					return errRouteNotFound
				}
				if err != nil {
					return err
				}
				if oldSev != req.Severity || oldStep != req.EscalationStep {
					return errRouteSupersedeMatch
				}
				before = map[string]any{"route_id": supersedes.String(), "enabled": oldEnabled, "channel_kind": oldKind}
				// Shows a redirection without recording either value (security F5).
				recipientChanged = (oldRecipient == nil) != (req.RecipientRef == nil) ||
					(oldRecipient != nil && req.RecipientRef != nil && *oldRecipient != *req.RecipientRef)
			} else {
				var exists bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alert_routes WHERE scope = 'platform' AND severity = $1 AND escalation_step = $2 AND superseded_at IS NULL)`,
					req.Severity, req.EscalationStep).Scan(&exists); err != nil {
					return err
				}
				if exists {
					return errRouteCurrentExists
				}
			}
			var newID uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, escalate_after, enabled, reason_code)
				VALUES ('platform', $1, $2, $3, $4, CASE WHEN $5::float8 IS NULL THEN NULL ELSE make_interval(secs => $5::float8) END, $6, $7)
				RETURNING id`,
				req.Severity, req.EscalationStep, req.ChannelKind, req.RecipientRef, req.EscalateAfterSeconds, req.Enabled, req.ReasonCode).Scan(&newID); err != nil {
				return err
			}
			if req.SupersedesID != nil {
				if _, err := tx.Exec(ctx, `UPDATE alert_routes SET superseded_at = now(), superseded_by = $2 WHERE id = $1`, supersedes, newID); err != nil {
					return err
				}
			}
			row := tx.QueryRow(ctx, `SELECT `+alertRouteColumns+` FROM alert_routes WHERE id = $1`, newID)
			d, err := scanAlertRoute(row)
			if err != nil {
				return err
			}
			out = d
			meta := map[string]any{
				"severity": req.Severity, "escalation_step": req.EscalationStep, "channel_kind": req.ChannelKind,
				"enabled": req.Enabled, "reason_code": req.ReasonCode,
				// The value is never audited (it lives only in alert_routes).
				"recipient_ref_present": req.RecipientRef != nil,
				"recipient_changed":     recipientChanged,
				"after":                 map[string]any{"route_id": newID.String(), "enabled": req.Enabled, "channel_kind": req.ChannelKind},
			}
			if before != nil {
				meta["before"] = before
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: uuid.Nil, ActorType: audit.ActorStaff, ActorID: actor,
				Action: "alert_route.create", TargetType: "alert_route", TargetID: newID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: meta,
			})
		})
		var pgErr *pgconn.PgError
		switch {
		case err == nil:
			writeJSON(w, http.StatusCreated, out)
		case errors.Is(err, errRouteNotFound):
			deny.reason = "supersede_target_not_found"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeNotFound, "not found")
		case errors.Is(err, errRouteSupersedeMatch):
			deny.reason = "supersede_mismatch"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeConflict, "supersedes_id names a route for a different severity or step")
		case errors.Is(err, errRouteCurrentExists):
			deny.reason = "current_route_exists"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeConflict, "a current route exists for this severity and step; supply supersedes_id")
		case errors.As(err, &pgErr) && pgErr.Code == "AR001":
			deny.reason = "sr7_four_eyes_not_built"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeValidation, "SR-7: four-eyes approval required; not built")
		case errors.As(err, &pgErr) && pgErr.Code == "23514":
			deny.reason = "routing_rule_violation"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeValidation, "the route violates a routing rule")
		default:
			logger.Error("alert_route_create_failed", "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
		}
	}
}

func newListAlertRoutesHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, actor, requestID, ok := platformAlertCaller(w, r)
		if !ok {
			return
		}
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		includeSuperseded := r.URL.Query().Get("include_superseded") == "true"
		out := []alertRouteDTO{}
		err := deps.DB.WithPlatformAdmin(r.Context(), actor, func(ctx context.Context, tx pgx.Tx) error {
			q := `SELECT ` + alertRouteColumns + ` FROM alert_routes WHERE scope = 'platform'`
			if !includeSuperseded {
				q += ` AND superseded_at IS NULL`
			}
			q += ` ORDER BY severity, escalation_step, created_at DESC, id LIMIT 200`
			rows, err := tx.Query(ctx, q)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				d, err := scanAlertRoute(rows)
				if err != nil {
					return err
				}
				out = append(out, d)
			}
			return rows.Err()
		})
		if err != nil {
			logger.Error("alert_route_list_failed", "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"routes": out})
	}
}

type alertStatusSeverityDTO struct {
	Severity             string `json:"severity"`
	RequiredForReadiness bool   `json:"required_for_readiness"`
	RoutesConfigured     int    `json:"routes_configured"`
	RoutesEnabled        int    `json:"routes_enabled"`
	HumanNotifyRoutes    int    `json:"human_notification_routes"`
	Ready                bool   `json:"ready"`
	Reason               string `json:"reason"`
	MissingOperatorInput string `json:"missing_operator_input,omitempty"`
}

func newAlertStatusHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _, _, ok := platformAlertCaller(w, r)
		if !ok {
			return
		}
		eff := alerting.NotEvaluated()
		var evaluatedAt time.Time
		if deps.AlertRouting != nil {
			eff = deps.AlertRouting.ReadinessEffective()
			evaluatedAt = deps.AlertRouting.ReadinessEvaluatedAt()
		}
		sevs := make([]alertStatusSeverityDTO, 0, len(eff))
		allReady := true
		for _, e := range eff {
			sevs = append(sevs, alertStatusSeverityDTO{
				Severity: string(e.Severity), RequiredForReadiness: e.Required,
				RoutesConfigured: e.RoutesConfigured, RoutesEnabled: e.RoutesEnabled, HumanNotifyRoutes: e.HumanRoutesEnabled,
				Ready: e.Ready, Reason: string(e.Reason), MissingOperatorInput: alerting.MissingOperatorInput(e.Reason),
			})
			if e.Required && !e.Ready {
				allReady = false
			}
		}
		resp := map[string]any{
			"ready":      allReady,
			"severities": sevs,
			"note":       "Readiness is computed over configuration only. A log or mock route never notifies a person and never counts. /readyz is report-only for routing.",
		}
		if !evaluatedAt.IsZero() {
			resp["evaluated_at"] = evaluatedAt
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type alertListItemDTO struct {
	ID               string    `json:"id"`
	Kind             string    `json:"kind"`
	Severity         string    `json:"severity"`
	State            string    `json:"state"`
	SubjectTenantID  *string   `json:"subject_tenant_id,omitempty"`
	FirstSeenAt      time.Time `json:"first_seen_at"`
	DeliveryState    string    `json:"delivery_state"`
	UnroutedReason   *string   `json:"unrouted_reason,omitempty"`
	NotifiedAPerson  bool      `json:"notified_a_person"`
	EscalationStep   *int      `json:"escalation_step,omitempty"`
	DeliveryChannel  *string   `json:"delivery_channel_kind,omitempty"`
	DeliveryAttempts *int      `json:"attempt_no,omitempty"`
}

func newListAlertsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, actor, requestID, ok := platformAlertCaller(w, r)
		if !ok {
			return
		}
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		q := r.URL.Query()
		state, sev := q.Get("state"), q.Get("severity")
		switch state {
		case "", "open", "acked", "resolved":
		default:
			apierror.Write(w, requestID, apierror.CodeValidation, "state must be open, acked or resolved")
			return
		}
		switch sev {
		case "", "p1", "p2", "p3":
		default:
			apierror.Write(w, requestID, apierror.CodeValidation, "severity must be p1, p2 or p3")
			return
		}
		limit := 50
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 {
				apierror.Write(w, requestID, apierror.CodeValidation, "limit must be between 1 and 200")
				return
			}
			limit = n
		}
		out := []alertListItemDTO{}
		err := deps.DB.WithPlatformAdmin(r.Context(), actor, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `
				SELECT a.id::text, a.kind, a.severity, a.state, a.subject_tenant_id::text, a.first_seen_at,
				       ld.event, ld.unrouted_reason, ld.channel_kind, ld.escalation_step, ld.attempt_no
				FROM alerts a
				LEFT JOIN LATERAL (
					SELECT d.event, d.unrouted_reason, d.channel_kind, d.escalation_step, d.attempt_no
					FROM alert_deliveries d WHERE d.alert_id = a.id
					ORDER BY d.recorded_at DESC, d.id DESC LIMIT 1
				) ld ON true
				WHERE a.tenant_id IS NULL
				  AND ($1 = '' OR a.state = $1) AND ($2 = '' OR a.severity = $2)
				ORDER BY a.first_seen_at DESC, a.id DESC
				LIMIT $3`, state, sev, limit)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var it alertListItemDTO
				var event, ch *string
				if err := rows.Scan(&it.ID, &it.Kind, &it.Severity, &it.State, &it.SubjectTenantID, &it.FirstSeenAt,
					&event, &it.UnroutedReason, &ch, &it.EscalationStep, &it.DeliveryAttempts); err != nil {
					return err
				}
				ev, kind := "", alerting.ChannelKind("")
				if event != nil {
					ev = *event
				}
				if ch != nil {
					kind = alerting.ChannelKind(*ch)
					it.DeliveryChannel = ch
				}
				it.DeliveryState = alerting.DeliveryState(ev, kind)
				it.NotifiedAPerson = it.DeliveryState == alerting.DeliveryStateDelivered
				out = append(out, it)
			}
			return rows.Err()
		})
		if err != nil {
			logger.Error("alert_list_failed", "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"alerts": out})
	}
}
