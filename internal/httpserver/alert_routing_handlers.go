package httpserver

// ALERT-DELIVERY-1 routing readiness (ADR 0102 section 18): platform-scope
// authoring and visibility of alert routing.
//
//	POST /v1/admin/alerting/routes   alert:route_manage  create or supersede a route version
//	GET  /v1/admin/alerting/routes   alert:route_manage  current (or all) route versions
//	GET  /v1/admin/alerting/status   alert:manage        readiness per severity, exact missing operator input
//	GET  /v1/admin/alerts            alert:manage        platform alerts with derived delivery state (keyset-paged)
//	GET  /v1/admin/alerts/{id}       alert:manage        one platform alert (ADR 0114 S1b)
//
// Every endpoint is platform scope only. Three layers, each pinned by its own
// test: the permission, an explicit tc.TenantID == uuid.Nil check, and the
// database (RLS family + alerting_validated_platform_admin). The permission is
// checked inside the POST handler so a refused attempt writes a denied audit
// row in the caller's own scope (security L-1).
//
// A route is configuration, not a notification. log and mock routes reach no
// person. email and webhook routes can, but the database refuses to enable one
// unless it supersedes a disabled version staged by a DIFFERENT platform admin
// (SR-7 four-eyes, migration 0156); the actual addresses and URL are platform
// configuration, never route data. recipient_ref is an opaque target id; it is
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

func registerAlertRoutingRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("POST /v1/admin/alerting/routes", staffRoute(deps, stepUpByAlertRouteManager, newCreateAlertRouteHandler(deps)))
	mux.Handle("GET /v1/admin/alerting/routes",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAlertRouteManage)(newListAlertRoutesHandler(deps))))
	mux.Handle("GET /v1/admin/alerting/status",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAlertManage)(newAlertStatusHandler(deps))))
	mux.Handle("GET /v1/admin/alerts",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAlertManage)(newListAlertsHandler(deps))))
	mux.Handle("GET /v1/admin/alerts/{alertID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAlertManage)(newGetAlertHandler(deps))))
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

var routeChannelKinds = map[alerting.ChannelKind]bool{
	alerting.ChannelLog: true, alerting.ChannelMock: true, alerting.ChannelEmail: true, alerting.ChannelWebhook: true,
}

type createAlertRouteRequest struct {
	Severity             string   `json:"severity"`
	EscalationStep       int      `json:"escalation_step"`
	ChannelKind          string   `json:"channel_kind"`
	RecipientRef         *string  `json:"recipient_ref"`
	EscalateAfterSeconds *float64 `json:"escalate_after_seconds"`
	Enabled              bool     `json:"enabled"`
	ReasonCode           string   `json:"reason_code"`
	SupersedesID         *string  `json:"supersedes_id"`
	// StageForEnable marks a DISABLED human-notification version as the SR-7 staging record that a different
	// platform admin may later approve (migration 0156 staged_for_enable). A route disabled for any other
	// reason (for example a decommission) leaves it false and can never be used as an approval.
	StageForEnable bool `json:"stage_for_enable"`
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
	d.HumanNotification = alerting.KnownHumanChannelKind(alerting.ChannelKind(d.ChannelKind))
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
		if req.StageForEnable && (req.Enabled || !alerting.ChannelKindIsHumanNotification(kind)) {
			invalid("stage_for_enable", "stage_for_enable is only valid on a disabled email or webhook route")
			return
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
			// SR-7 four-eyes (migration 0156): ENABLING a human-notification route approves the staged
			// (disabled) version it supersedes; the database requires that staging to be by a DIFFERENT
			// platform admin. With no supersedes_id there is nothing to approve and the database refuses (AR001).
			var stagedRouteID *uuid.UUID
			var stagedBy uuid.UUID
			if req.Enabled && alerting.ChannelKindIsHumanNotification(kind) && req.SupersedesID != nil {
				stagedRouteID = &supersedes
				if err := tx.QueryRow(ctx, `SELECT created_by FROM alert_routes WHERE id = $1`, supersedes).Scan(&stagedBy); err != nil {
					return err
				}
			}
			var newID uuid.UUID
			if err := tx.QueryRow(ctx, `INSERT INTO alert_routes (scope, severity, escalation_step, channel_kind, recipient_ref, escalate_after, enabled, reason_code, staged_route_id, staged_for_enable)
				VALUES ('platform', $1, $2, $3, $4, CASE WHEN $5::float8 IS NULL THEN NULL ELSE make_interval(secs => $5::float8) END, $6, $7, $8, $9)
				RETURNING id`,
				req.Severity, req.EscalationStep, req.ChannelKind, req.RecipientRef, req.EscalateAfterSeconds, req.Enabled, req.ReasonCode, stagedRouteID, req.StageForEnable).Scan(&newID); err != nil {
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
			if stagedRouteID != nil {
				// The four-eyes record: who staged the version this actor approved.
				meta["four_eyes_staged_route_id"] = stagedRouteID.String()
				meta["four_eyes_staged_by"] = stagedBy.String()
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
			deny.reason = "sr7_four_eyes_required"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeValidation,
				"SR-7: four-eyes approval required: stage the route disabled with stage_for_enable=true, then a different platform admin enables it by superseding that staged version")
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
	ID               string     `json:"id"`
	Kind             string     `json:"kind"`
	Severity         string     `json:"severity"`
	State            string     `json:"state"`
	SubjectTenantID  *string    `json:"subject_tenant_id,omitempty"`
	FirstSeenAt      time.Time  `json:"first_seen_at"`
	AckedAt          *time.Time `json:"acked_at,omitempty"`
	ResolvedAt       *time.Time `json:"resolved_at,omitempty"`
	AckedBy          *staffRef  `json:"acked_by,omitempty"`
	ResolvedBy       *staffRef  `json:"resolved_by,omitempty"`
	DeliveryState    string     `json:"delivery_state"`
	UnroutedReason   *string    `json:"unrouted_reason,omitempty"`
	NotifiedAPerson  bool       `json:"notified_a_person"`
	EscalationStep   *int       `json:"escalation_step,omitempty"`
	DeliveryChannel  *string    `json:"delivery_channel_kind,omitempty"`
	DeliveryAttempts *int       `json:"attempt_no,omitempty"`
}

// alertListSQL is the shared projection of the list and the single-alert read. The
// alerts guard (migration 0110) forces acked_by/resolved_by from the validated platform
// session, so these columns are always platform actors.
//
// Shape matters (security F1): the keyset filter, ORDER BY and LIMIT are applied to
// `alerts` FIRST, in the inner query, and only those rows (at most limit+1) are joined to
// their latest delivery. A single query with the lateral join beside the ORDER BY made the
// lateral subquery run for every matching alert before the top-N (cost O(all alerts)).
// TestAlertListS1b_QueryCostIsBoundedByLimit pins the loop count with EXPLAIN (ANALYZE).
// innerTail is the WHERE extension, ORDER BY and LIMIT of the inner query on alerts a.
func alertListSQL(innerTail, outerTail string) string {
	return `
	SELECT a.id::text, a.kind, a.severity, a.state, a.subject_tenant_id::text, a.first_seen_at,
	       a.acked_at, a.resolved_at, a.acked_by, a.resolved_by,
	       ld.event, ld.unrouted_reason, ld.channel_kind, ld.escalation_step, ld.attempt_no
	FROM (
		SELECT * FROM alerts a WHERE a.tenant_id IS NULL ` + innerTail + `
	) a
	LEFT JOIN LATERAL (
		SELECT d.event, d.unrouted_reason, d.channel_kind, d.escalation_step, d.attempt_no
		FROM alert_deliveries d WHERE d.alert_id = a.id
		ORDER BY d.recorded_at DESC, d.id DESC LIMIT 1
	) ld ON true ` + outerTail
}

// alertListPageSQL is the list query for one page; cond is the optional keyset condition.
func alertListPageSQL(cond string) string {
	return alertListSQL(alertListInnerTail+cond+alertListOrderLimit, alertListOuterOrder)
}

const (
	alertListInnerTail = `
		AND ($1 = '' OR a.state = $1) AND ($2 = '' OR a.severity = $2) AND ($3 = '' OR a.kind = $3)`
	alertListOrderLimit = `
		ORDER BY a.first_seen_at DESC, a.id DESC
		LIMIT $4`
	alertListOuterOrder = `ORDER BY a.first_seen_at DESC, a.id DESC`
)

// alertActorRefs routes the acting staff ids of an alert through the S0 helper. The database
// guard only ever stores validated PLATFORM administrators there, so the actor scope is
// always platform: a platform viewer sees the ids, any tenant viewer would get null (never
// reachable today: the routes refuse tenant callers first, this is defence in depth).
func alertActorRefs(v staffViewer, ackedBy, resolvedBy *uuid.UUID) (acked, resolved *staffRef) {
	platform := staffScopePlatform
	return v.optRef(&platform, ackedBy), v.optRef(&platform, resolvedBy)
}

// scanAlertListItem maps one alertListSQL row. The viewer decides whether the acting
// staff ids are shown (ADR 0114 S0: every staff-id field goes through the helper); the
// actors of an alert are always platform actors.
func scanAlertListItem(row pgx.Row, v staffViewer) (alertListItemDTO, error) {
	var it alertListItemDTO
	var event, ch *string
	var ackedBy, resolvedBy *uuid.UUID
	if err := row.Scan(&it.ID, &it.Kind, &it.Severity, &it.State, &it.SubjectTenantID, &it.FirstSeenAt,
		&it.AckedAt, &it.ResolvedAt, &ackedBy, &resolvedBy,
		&event, &it.UnroutedReason, &ch, &it.EscalationStep, &it.DeliveryAttempts); err != nil {
		return it, err
	}
	it.AckedBy, it.ResolvedBy = alertActorRefs(v, ackedBy, resolvedBy)
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
	return it, nil
}

func newListAlertsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tc, actor, requestID, ok := platformAlertCaller(w, r)
		if !ok {
			return
		}
		viewer := staffViewerOf(tc)
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		q := r.URL.Query()
		for _, k := range []string{"state", "severity", "kind", "limit"} {
			if len(q[k]) > 1 { // repeated parameters are refused, not first-wins (security F4)
				apierror.Write(w, requestID, apierror.CodeValidation, k+" must be given at most once")
				return
			}
		}
		state, sev, kind := q.Get("state"), q.Get("severity"), q.Get("kind")
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
		if kind != "" {
			if _, known := alerting.Def(alerting.Kind(kind)); !known {
				apierror.Write(w, requestID, apierror.CodeValidation, "kind is not a known alert kind")
				return
			}
		}
		limit := 50
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 || n > 200 || strconv.Itoa(n) != v {
				apierror.Write(w, requestID, apierror.CodeValidation, "limit must be between 1 and 200")
				return
			}
			limit = n
		}
		filter := alertCursorFilter(state, sev, kind)
		var after *alertCursor
		if raw, present := q["cursor"]; present {
			if len(raw) != 1 {
				apierror.Write(w, requestID, apierror.CodeValidation, "cursor is invalid")
				return
			}
			c, err := decodeAlertCursor(raw[0], filter)
			if err != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "cursor is invalid")
				return
			}
			after = &c
		}
		out := []alertListItemDTO{}
		var next *string
		err := deps.DB.WithPlatformAdmin(r.Context(), actor, func(ctx context.Context, tx pgx.Tx) error {
			args := []any{state, sev, kind, limit + 1}
			cond := ""
			if after != nil {
				// Strict keyset on (first_seen_at, id) DESC: rows already served are never
				// repeated and rows inserted later (newer first_seen_at) cannot shift the page.
				cond = ` AND (a.first_seen_at, a.id) < (timestamptz 'epoch' + $5::bigint * interval '1 microsecond', $6::uuid)`
				args = append(args, after.Micros, after.ID)
			}
			rows, err := tx.Query(ctx, alertListPageSQL(cond), args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				it, err := scanAlertListItem(rows, viewer)
				if err != nil {
					return err
				}
				out = append(out, it)
			}
			return rows.Err()
		})
		if err != nil {
			logger.Error("alert_list_failed", "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
			return
		}
		if len(out) > limit {
			out = out[:limit]
			last := out[len(out)-1]
			c := encodeAlertCursor(last.FirstSeenAt, last.ID, filter)
			next = &c
		}
		writeJSON(w, http.StatusOK, map[string]any{"alerts": out, "next_cursor": next})
	}
}

func newGetAlertHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tc, actor, requestID, ok := platformAlertCaller(w, r)
		if !ok {
			return
		}
		rawID := r.PathValue("alertID")
		id, err := uuid.Parse(rawID)
		if err != nil || id.String() != rawID { // canonical lowercase only (security F3), like the cursor
			apierror.Write(w, requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		viewer := staffViewerOf(tc)
		var it alertListItemDTO
		err = deps.DB.WithPlatformAdmin(r.Context(), actor, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			it, e = scanAlertListItem(tx.QueryRow(ctx, alertListSQL(`AND a.id = $1`, ``), id), viewer)
			return e
		})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
		case err != nil:
			observability.LoggerFromContext(r.Context(), deps.Logger).Error("alert_get_failed", "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
		default:
			writeJSON(w, http.StatusOK, map[string]any{"alert": it})
		}
	}
}
