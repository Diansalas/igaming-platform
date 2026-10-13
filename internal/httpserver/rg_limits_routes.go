// ADR 0115 slice S2: the Responsible Gaming limit, time-out and RG-configuration WRITE APIs.
//
// STATUS. API only. Nothing here enforces a limit on a deposit, a wager, a launch or a login (S4) and
// no usage is measured (S3); no UI (S5). A recorded limit is a stored preference with a server-decided
// effective time, not (yet) a gate.
//
// What this layer does. Authenticate, derive every identity from the verified token (never a body,
// query or path value for the tenant), validate shape, call internal/rg, map the closed refusal
// vocabulary onto the closed RG_* error codes, audit every refusal in a separate transaction, and
// redact staff identities on every response (ADR 0114 S0 staffViewer). The rules live in internal/rg
// and in the migration-0135 triggers.
//
// Authentication and authorization:
//   - /v1/me/rg/*: auth.RequirePlayerPrincipal. The player's own account id and tenant come from the
//     token; db.WithPlayerScope pins RLS. A body "tenant_id" (or any unknown field) is a 400.
//   - /v1/admin/players/{id}/rg/*, /v1/admin/rg/limit-configs: RequireStaffPrincipal + RequireTenantScope
//   - the static permission. {id} is resolved INSIDE the caller's tenant by a tenant-scoped read: a
//     player of another tenant is the same 404 as an unknown id.
//   - /v1/admin/platform/rg/*: RequireStaffPrincipal + a platform-token check (tenant id absent) + the
//     static permission; floor four-eyes (distinct staff AND Person) and the signed actor proofs are
//     the database's.
//
// Idempotency: every limit / time-out write requires an Idempotency-Key header (1-120 printable ASCII).
// The same key with the same request replays the stored result (header Idempotent-Replayed: true);
// the same key with a different request is 409 RG_IDEMPOTENCY_MISMATCH. For limit changes this is the
// S1 UNIQUE (tenant_id, player_account_id, idempotency_key) constraint; for time-outs see
// internal/rg/limits_timeout.go (person-lock serialised, audit-keyed, not a database constraint).
package httpserver

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/admission"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Per-player write limiter (ADR 0115 3.5): burst 10, refilling one write per 2 seconds. Refused
// attempts consume budget. Engineering bounds, not legal values.
const (
	rgWriteRate  = 0.5
	rgWriteBurst = 10
)

// NewRGWriteLimiter builds the production limiter (tests inject a FakeClock one through Deps).
func NewRGWriteLimiter(clock admission.Clock) *admission.GCRALimiter {
	if clock == nil {
		clock = admission.RealClock()
	}
	return admission.NewGCRALimiter(rgWriteRate, rgWriteBurst, 100000, time.Hour, "overflow", clock)
}

var (
	defaultRGLimitsOnce sync.Once
	defaultRGLimits     *rg.Limits
)

func rgLimits(deps Deps) *rg.Limits {
	if deps.RGLimits != nil {
		return deps.RGLimits
	}
	defaultRGLimitsOnce.Do(func() { defaultRGLimits = rg.NewLimits(rg.LimitsConfig{}) })
	return defaultRGLimits
}

func registerRGLimitRoutes(mux *http.ServeMux, deps Deps) {
	limiter := deps.RGWriteLimiter
	if limiter == nil {
		limiter = NewRGWriteLimiter(nil)
	}
	player := func(h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequirePlayerPrincipal(h))
	}
	// Literal patterns only (the webhook route guard fails closed on any non-literal pattern).
	mux.Handle("GET /v1/me/rg/limits", player(newGetMyRGLimitsHandler(deps)))
	mux.Handle("POST /v1/me/rg/limits", player(newSetMyRGLimitHandler(deps, limiter)))
	mux.Handle("POST /v1/me/rg/limits/changes/{id}/confirm", player(newResolveMyRGLimitHandler(deps, limiter, rg.ChangeConfirm)))
	mux.Handle("POST /v1/me/rg/limits/changes/{id}/cancel", player(newResolveMyRGLimitHandler(deps, limiter, rg.ChangeCancelPending)))
	mux.Handle("POST /v1/me/rg/time-out", player(newCreateMyTimeOutHandler(deps, limiter)))

	mux.Handle("GET /v1/admin/players/{id}/rg/limits", staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGLimitRead)(newStaffGetPlayerRGLimitsHandler(deps)))))
	mux.Handle("GET /v1/admin/players/{id}/rg/history", staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGLimitRead)(newStaffGetPlayerRGHistoryHandler(deps)))))
	mux.Handle("POST /v1/admin/players/{id}/rg/limits", staffRoute(deps, stepUpByRGLimitDirection, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGLimitWriteOnPlayerRequest)(newStaffSetPlayerRGLimitHandler(deps)))))

	mux.Handle("GET /v1/admin/rg/limit-configs", staffRoute(deps, stepUpNone, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGConfigRead)(newListRGLimitConfigsHandler(deps)))))
	mux.Handle("POST /v1/admin/rg/limit-configs", staffRoute(deps, stepUpCritical, auth.RequireTenantScope(auth.RequirePermission(auth.PermRGConfigWrite)(newWriteRGOperatorConfigHandler(deps)))))

	mux.Handle("GET /v1/admin/platform/rg/limit-config-change-requests", staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermRGFloorRead)(requirePlatformToken(newListRGFloorRequestsHandler(deps)))))
	mux.Handle("POST /v1/admin/platform/rg/limit-config-change-requests", staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermRGFloorRequest)(requirePlatformToken(newRequestRGFloorHandler(deps)))))
	mux.Handle("POST /v1/admin/platform/rg/limit-config-change-requests/{id}/approve",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermRGFloorDecide)(requirePlatformToken(newDecideRGFloorHandler(deps, rg.FloorApprove)))))
	mux.Handle("POST /v1/admin/platform/rg/limit-config-change-requests/{id}/reject",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermRGFloorDecide)(requirePlatformToken(newDecideRGFloorHandler(deps, rg.FloorReject)))))
	mux.Handle("POST /v1/admin/platform/rg/limit-config-change-requests/{id}/cancel",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermRGFloorRequest)(requirePlatformToken(newDecideRGFloorHandler(deps, rg.FloorCancel)))))
}

// requirePlatformToken admits only a PLATFORM-scoped token (no tenant id in the verified claims).
func requirePlatformToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		if tc.TenantID != uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeForbidden, "this operation requires a platform-scoped identity")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// rgCall is the per-request context: the verified identity and the audit metadata.
type rgCall struct {
	requestID string
	logger    observabilityLogger
	tc        tenant.Context
	subject   uuid.UUID
	meta      rg.RequestMeta
}

func beginRGCall(deps Deps, w http.ResponseWriter, r *http.Request) (rgCall, bool) {
	c := rgCall{
		requestID: observability.RequestIDFromContext(r.Context()),
		logger:    observability.LoggerFromContext(r.Context(), deps.Logger),
	}
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	sub, err := uuid.Parse(tc.Subject)
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "invalid identity")
		return c, false
	}
	c.tc, c.subject = tc, sub
	c.meta = rg.RequestMeta{IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: c.requestID}
	return c, true
}

// admitRGWrite applies the per-player write budget (refused attempts consume budget).
func admitRGWrite(w http.ResponseWriter, c rgCall, limiter *admission.GCRALimiter) bool {
	if admitted, retry := limiter.Allow(c.tc.TenantID.String() + ":" + c.subject.String()); !admitted {
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
		apierror.Write(w, c.requestID, apierror.CodeRateLimited, "too many responsible gaming changes, retry later")
		return false
	}
	return true
}

func idempotencyKey(w http.ResponseWriter, r *http.Request, requestID string) (string, bool) {
	k := r.Header.Get("Idempotency-Key")
	if k == "" {
		apierror.Write(w, requestID, apierror.CodeValidation, "the Idempotency-Key header is required")
		return "", false
	}
	return k, true
}

// rgErrorCode maps a refusal onto the closed RG_* / generic codes and a fixed message.
func rgErrorCode(rf *rg.Refusal) (apierror.Code, string) {
	switch rf.Code {
	case rg.RefusedInvalid:
		return apierror.CodeValidation, rf.Detail
	case rg.RefusedNotFound:
		return apierror.CodeNotFound, "not found"
	case rg.RefusedForbidden:
		return apierror.CodeForbidden, "not permitted"
	case rg.RefusedAboveCeiling:
		return apierror.CodeRGLimitAboveCeiling, "the value is above the maximum the configuration allows"
	case rg.RefusedPeriodNotAllowed:
		return apierror.CodeRGPeriodNotAllowed, "the period is not allowed by the configuration"
	case rg.RefusedDurationNotAllowed:
		return apierror.CodeRGDurationNotAllowed, "the duration is not allowed by the configuration"
	case rg.RefusedNotOffered:
		return apierror.CodeRGNotOffered, "the configuration does not offer this change"
	case rg.RefusedBackdated:
		return apierror.CodeRGBackdated, "effective_from is in the past; a configuration change takes effect now or later"
	case rg.RefusedSyntheticRefused:
		return apierror.CodeRGSyntheticRefused, "synthetic configuration is refused in this environment"
	case rg.RefusedNoChange:
		return apierror.CodeRGNoChange, "the request changes nothing"
	case rg.RefusedChangeNotPending:
		return apierror.CodeRGChangeNotPending, "the change is not pending, is not the latest request of its limit, or needs no confirmation"
	case rg.RefusedCoolingOff:
		return apierror.CodeRGCoolingOff, "the cooling-off period has not yet run; read effective_at from the pending change"
	case rg.RefusedConfigMissing:
		return apierror.CodeRGConfigMissing, "no configuration is in force for this limit; an increase or removal is refused"
	case rg.RefusedPendingCap:
		return apierror.CodeRGPendingCap, "too many open pending changes"
	case rg.RefusedIdempotencyMismatch:
		return apierror.CodeRGIdempotencyMismatch, "the Idempotency-Key was used with a different request"
	case rg.RefusedOperatorLoosens:
		msg := "an operator row may only tighten restrictive fields and widen option sets"
		if rf.Detail != "" {
			msg += " (field " + rf.Detail + ")"
		}
		return apierror.CodeRGOperatorLoosens, msg
	case rg.RefusedNoFloor:
		return apierror.CodeRGNoFloor, "no jurisdiction floor is in force for this limit; there is nothing to tighten"
	case rg.RefusedSelfApproval:
		return apierror.CodeRGSelfApproval, "the requesting Person may not decide this request"
	case rg.RefusedRequestNotPending:
		return apierror.CodeRGRequestNotPending, "the request is not pending"
	case rg.RefusedGuard:
		return apierror.CodeRGGuardRefused, "refused by a database guard"
	case rg.RefusedStepUpRequired:
		return apierror.CodeRGStepUpRequired, "a staff-originated increase or removal needs step-up authentication, which is not implemented"
	case rg.RefusedSelfTarget:
		return apierror.CodeRGSelfTarget, "staff may not change their own Person's limits"
	case rg.RefusedIdentityReview:
		return apierror.CodeRGIdentityReview, "an account under identity review may not loosen a person-level limit"
	case rg.RefusedProof:
		return apierror.CodeRGProofRefused, "the signed actor proof was refused"
	case rg.RefusedUnavailable:
		return apierror.CodeRGUnavailable, "temporarily unavailable, retry"
	}
	return apierror.CodeInternal, "internal error"
}

// writeRGError writes the response for err. A refusal is a closed code; anything else is internal
// and reveals nothing. It always returns the refusal that was written; an internal fault is the
// refusal code "internal" so the caller can still audit it.
func writeRGError(w http.ResponseWriter, c rgCall, op string, err error) *rg.Refusal {
	if rf, ok := rg.IsRefusal(rg.MapDBError(err)); ok {
		code, msg := rgErrorCode(rf)
		if rf.Code == rg.RefusedUnavailable {
			w.Header().Set("Retry-After", "1")
		}
		apierror.Write(w, c.requestID, code, msg)
		return rf
	}
	c.logger.Error("rg_write_failed", "op", op, "error_class", "internal")
	apierror.Write(w, c.requestID, apierror.CodeInternal, "internal error")
	return &rg.Refusal{Code: rgRefusedInternal}
}

// rgRefusedInternal is the audit-only code of a refusal that ended as an internal error (500).
const rgRefusedInternal rg.RefusalCode = "internal"

// auditDenial records a refusal in its own transaction (the refused write rolled back). A failure of
// the denial record itself is logged; the request stays refused either way.
func auditDenial(ctx context.Context, deps Deps, c rgCall, scope func(context.Context, func(context.Context, pgx.Tx) error) error,
	e audit.Entry, rf *rg.Refusal) {
	if rf == nil {
		return
	}
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	e.Metadata["code"] = string(rf.Code)
	if len(rf.ConfigVersionIDs) > 0 {
		e.Metadata["config_version_ids"] = rf.ConfigVersionIDs
	}
	if rf.Code == rg.RefusedOperatorLoosens {
		e.Metadata["field"] = rf.Detail
	}
	e.IPAddress, e.UserAgent, e.RequestID = c.meta.IPAddress, c.meta.UserAgent, c.meta.RequestID
	if err := scope(ctx, func(ctx context.Context, tx pgx.Tx) error { return rgLimits(deps).RecordDenial(ctx, tx, e) }); err != nil {
		c.logger.Error("rg_denial_audit_failed", "error_class", "internal")
	}
}
