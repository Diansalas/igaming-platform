package httpserver

// ADR 0102 4.2 / C-102-7 (ALERT-DELIVERY-1, I-wire): acknowledging and
// resolving a platform-owned durable alert. Platform scope only, gated by
// auth.PermAlertManage (granted solely to RolePlatformAdmin); the database's
// own state guard (migration 0110 alerts_guard) independently re-validates the
// acting session as a platform admin and forces acked_by/resolved_by from the
// session, never from this request. There is NO tenant ack endpoint: a tenant
// can never ack, resolve or suppress a platform-owned integrity alert, even one
// it is the subject of (ADR 0102 2(2)).
//
// Every ack and resolve is audited in the SAME platform-admin transaction as
// the state change (audit.Entry with SubjectTenantID for a subject-bearing
// alert, ADR 0104), so an ack with no audit row, or an audit row for an ack
// that never committed, is impossible. The audit metadata carries the Kind,
// the before and after state and the reason code - never alert attributes
// beyond what the alert itself already exposes.
//
// Route authoring lives in alert_routing_handlers.go (ALERT-DELIVERY-1, ADR
// 0102 section 18): configuring a route is not a notification, and no channel
// that reaches a person exists. Until a human configures a real channel
// (HD-PRH2-4-OPS) nothing is delivered to anyone.
//
// Refusals are audited (security L-1): the permission check is made INSIDE the
// handler (not in middleware) so a refused attempt can write its own denied
// audit row in the CALLER'S OWN scope (a tenant caller in its own tenant's
// audit, a platform caller in the platform audit), the K1 pattern.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func registerAlertAdminRoutes(mux *http.ServeMux, deps Deps) {
	staff := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(h))
	}
	// The permission check is inside the handler (see the file comment).
	mux.Handle("POST /v1/admin/alerts/{alertID}/ack", staff(newAlertTransitionHandler(deps, "ack")))
	mux.Handle("POST /v1/admin/alerts/{alertID}/resolve", staff(newAlertTransitionHandler(deps, "resolve")))
	registerAlertRoutingRoutes(mux, deps, staff)
}

// alertDenial describes one refused alert/routing mutation for its denied
// audit row. reason is a closed token; no request value is recorded.
type alertDenial struct {
	action     string
	targetType string
	targetID   string
	reason     string
}

// recordAlertDenied writes the denied audit row in the caller's own scope in
// its own bounded transaction (the main transaction, if any, rolled back).
func recordAlertDenied(ctx context.Context, deps Deps, tc tenant.Context, actor uuid.UUID, d alertDenial, r *http.Request, requestID string) {
	entry := audit.Entry{
		TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: actor,
		Action: d.action, TargetType: d.targetType, TargetID: d.targetID,
		Outcome: audit.OutcomeDenied, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
		Metadata: map[string]any{"denied": d.reason},
	}
	record := func(ctx context.Context, tx pgx.Tx) error { return audit.Record(ctx, tx, entry) }
	dctx, cancel := deniedAuditCtx(ctx)
	defer cancel()
	var err error
	if tc.TenantID == uuid.Nil {
		err = deps.DB.WithPlatformAdmin(dctx, actor, record)
	} else {
		err = deps.DB.WithTenant(dctx, tc.TenantID, record)
	}
	if err != nil {
		observability.LoggerFromContext(ctx, deps.Logger).Error("alert_admin_denied_audit_failed", "action", d.action)
	}
}

// refuseAlert audits the refusal then writes the API error.
func refuseAlert(w http.ResponseWriter, r *http.Request, deps Deps, tc tenant.Context, actor uuid.UUID, requestID string, d alertDenial, code apierror.Code, msg string) {
	recordAlertDenied(r.Context(), deps, tc, actor, d, r, requestID)
	apierror.Write(w, requestID, code, msg)
}

// alertReasonCodePattern bounds the resolve reason code (a closed-shape token,
// never free text): it lands in alerts.resolve_reason_code and the audit row.
var alertReasonCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,63}$`)

type alertTransitionRequest struct {
	ReasonCode string `json:"reason_code"`
}

type alertDTO struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Severity   string     `json:"severity"`
	State      string     `json:"state"`
	AckedAt    *time.Time `json:"acked_at,omitempty"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// alertTransitionLockSQL locks the alert row for an ack/resolve. It MUST be
// FOR NO KEY UPDATE, never FOR UPDATE (security S-1): the alert_occurrences
// foreign-key insert that every in-transaction Raise performs takes FOR KEY
// SHARE on the alert row, which FOR UPDATE conflicts with. With FOR UPDATE an
// ack would block (and be blocked by) a business transaction raising on the
// same alert, and a context expiry there (57014, not swallowed) would roll back
// the dispute, park or posting because of an alert statement. The later UPDATE
// changes no key column, so it takes the same non-key lock. Pinned by
// TestAlertAdmin_AckLockDoesNotBlockInTxRaise.
const alertTransitionLockSQL = `SELECT kind, severity, state, subject_tenant_id FROM alerts WHERE id = $1 AND tenant_id IS NULL FOR NO KEY UPDATE`

var errAlertNotFound = errors.New("alert not found")
var errAlertBadTransition = errors.New("invalid alert state transition")

func newAlertTransitionHandler(deps Deps, op string) http.HandlerFunc {
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
		deny := alertDenial{action: "alerts." + op, targetType: "alert", targetID: r.PathValue("alertID")}
		// Layer 1: the permission. Inside the handler so a refusal is audited.
		if !auth.RoleHasPermission(auth.Role(tc.Role), auth.PermAlertManage) {
			deny.reason = "permission"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeForbidden, "insufficient permissions for this operation")
			return
		}
		// Layer 2, platform scope only: a tenant-scoped session never reaches
		// the state change (the permission already excludes every tenant role;
		// this is the explicit second gate).
		if tc.TenantID != uuid.Nil {
			deny.reason = "platform_scope_required"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeForbidden, "insufficient permissions for this operation")
			return
		}
		alertID, err := uuid.Parse(r.PathValue("alertID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		deny.targetID = alertID.String()
		var body alertTransitionRequest
		if op == "resolve" {
			if err := decodeJSON(r, &body); err != nil {
				deny.reason = "invalid_body"
				refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeInvalidRequest, "invalid request")
				return
			}
			if !alertReasonCodePattern.MatchString(body.ReasonCode) {
				deny.reason = "reason_code_required"
				refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeValidation, "reason_code is required (lowercase token, at most 64 characters)")
				return
			}
		} else if r.ContentLength != 0 {
			// ack takes an OPTIONAL reason_code (audit metadata only).
			if err := decodeJSON(r, &body); err != nil && !errors.Is(err, io.EOF) {
				deny.reason = "invalid_body"
				refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeInvalidRequest, "invalid request")
				return
			}
			if body.ReasonCode != "" && !alertReasonCodePattern.MatchString(body.ReasonCode) {
				deny.reason = "reason_code_invalid"
				refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeValidation, "reason_code must be a lowercase token of at most 64 characters")
				return
			}
		}

		var out alertDTO
		err = deps.DB.WithPlatformAdmin(r.Context(), actor, func(ctx context.Context, tx pgx.Tx) error {
			var (
				kind, severity, state string
				subject               *uuid.UUID
			)
			if err := tx.QueryRow(ctx, alertTransitionLockSQL, alertID).Scan(&kind, &severity, &state, &subject); err != nil {
				if errors.Is(err, pgx.ErrNoRows) {
					return errAlertNotFound
				}
				return err
			}
			var newState string
			switch {
			case op == "ack" && state == "open":
				newState = "acked"
			case op == "resolve" && (state == "open" || state == "acked"):
				newState = "resolved"
			default:
				return errAlertBadTransition
			}
			var reason *string
			if op == "resolve" {
				reason = &body.ReasonCode
			}
			auditReason := body.ReasonCode
			// acked_by/resolved_by and the timestamps are forced by the
			// database guard from the validated session, not from here.
			if err := tx.QueryRow(ctx,
				`UPDATE alerts SET state = $2, resolve_reason_code = COALESCE($3, resolve_reason_code) WHERE id = $1
				 RETURNING id, kind, severity, state, acked_at, resolved_at`,
				alertID, newState, reason,
			).Scan(&out.ID, &out.Kind, &out.Severity, &out.State, &out.AckedAt, &out.ResolvedAt); err != nil {
				return err
			}
			meta := map[string]any{"kind": kind, "severity": severity, "before_state": state, "after_state": newState}
			if auditReason != "" {
				meta["reason_code"] = auditReason
			}
			entry := audit.Entry{
				TenantID: uuid.Nil, ActorType: audit.ActorStaff, ActorID: actor,
				Action: "alerts." + op, TargetType: "alert", TargetID: alertID.String(),
				Outcome: audit.OutcomeSuccess, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: meta,
			}
			if subject != nil {
				entry.SubjectTenantID = *subject // from the stored alert row, never from the request
			}
			return audit.Record(ctx, tx, entry)
		})
		switch {
		case errors.Is(err, errAlertNotFound):
			deny.reason = "not_found"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeNotFound, "not found")
		case errors.Is(err, errAlertBadTransition):
			deny.reason = "invalid_transition"
			refuseAlert(w, r, deps, tc, actor, requestID, deny, apierror.CodeConflict, "alert state does not allow this operation")
		case err != nil:
			// Class only (never the error text): enough to tell a lock/timeout
			// from a refusal without leaking statement detail.
			logger.Error("alert_admin_transition_failed", "op", op, "alert_id", alertID.String(), "sqlstate_class", alertAdminSQLStateClass(err))
			apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
		default:
			writeJSON(w, http.StatusOK, out)
		}
	}
}

// alertAdminSQLStateClass returns the two-character SQLSTATE class of err, or
// "non_pg".
func alertAdminSQLStateClass(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && len(pgErr.Code) >= 2 {
		return pgErr.Code[:2]
	}
	return "non_pg"
}
