package httpserver

// ADR 0119 slice S4a (section 7.3, review N1, S3 conditions S3-R11/R13):
// RequireLiveStaffSession, the per-request liveness check of every staff
// route. One indexed query, in ONE short read-only transaction scoped to the
// token's own tenant (or platform) and principal (migration 0018's
// session_select_own_principal policy), with a 500 ms statement timeout. The
// transaction commits and releases its connection before the handler takes
// its own. A refusal is one 401 session_revoked for every cause; a database
// fault is 503 (fail closed). Every decision is counted by
// staff_liveness_decisions_total{outcome,reason}.

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

const (
	// staffLivenessStatementTimeout is the SET LOCAL statement_timeout of the
	// liveness transaction (section 7.3).
	staffLivenessStatementTimeout = 500 * time.Millisecond
	// staffLivenessDeadline bounds the whole check (pool acquisition included)
	// so a starved pool answers 503 instead of hanging the request.
	staffLivenessDeadline = 2 * time.Second
)

// usableFactorPredicate is the "second factor still usable" rule shared by the
// liveness query and the staff refresh re-check (S3-R11): an ACTIVE factor
// that is not hard-locked. A soft lock (locked_until) does not end a session:
// it throttles guessing, and making it end sessions would let a password
// holder log the victim out.
const usableFactorPredicate = `f.status = 'active' AND f.hard_locked_at IS NULL`

// staffLivenessSQL reads the session row named by the token's sid, joined to
// its staff row, in the token's own scope. The explicit tenant comparisons
// repeat what RLS already enforces, so the check does not depend on RLS alone.
// absolute_expires_at is compared on the DATABASE clock (S3-R8). The staff
// version is not read: staff_users.staff_version arrives with migration 0139
// (S5 + S6); until then every staff member is at staffVersionUnversioned.
const staffLivenessSQL = `SELECT s.revoked_at IS NOT NULL,
       (s.absolute_expires_at IS NULL OR s.absolute_expires_at <= now()),
       su.status, su.role,
       EXISTS (SELECT 1 FROM staff_mfa_factors f WHERE f.staff_user_id = su.id AND ` + usableFactorPredicate + `)
  FROM sessions s
  JOIN staff_users su ON su.id = s.principal_id
 WHERE s.id = $1 AND s.principal_id = $2 AND s.principal_type = 'staff'
   AND s.tenant_id IS NOT DISTINCT FROM $3::uuid
   AND su.tenant_id IS NOT DISTINCT FROM $3::uuid`

// staffLivenessRow is what the liveness query returns.
type staffLivenessRow struct {
	revoked         bool
	absoluteExpired bool
	status          string
	role            string
	staffVersion    int64 // staffVersionUnversioned until migration 0139
	factorUsable    bool
}

// staffLivenessVerdict applies the section 7.3 refusal rules to a found row:
// the session is not revoked and within its absolute lifetime, the staff
// member is active at the token's role and staff version, and the second
// factor is still usable. It does NOT refuse on the idle expires_at (that
// governs refresh only; the 15 min access token is the idle unit).
func staffLivenessVerdict(row staffLivenessRow, tokenRole auth.Role, tokenVersion int64) observability.StaffLivenessReason {
	switch {
	case row.revoked:
		return observability.StaffLivenessReasonRevoked
	case row.absoluteExpired:
		return observability.StaffLivenessReasonAbsoluteExpired
	case row.status != "active":
		return observability.StaffLivenessReasonStaffNotActive
	case auth.Role(row.role) != tokenRole:
		return observability.StaffLivenessReasonRoleMismatch
	case row.staffVersion != tokenVersion:
		return observability.StaffLivenessReasonVersionMismatch
	case !row.factorUsable:
		return observability.StaffLivenessReasonFactorUnusable
	}
	return observability.StaffLivenessReasonOK
}

var errStaffLivenessNoDB = errors.New("httpserver: staff liveness: no database")

// checkStaffLiveness runs the liveness query for the session sess of staff
// member staffID in tenantID's scope (uuid.Nil: platform) and returns the
// verdict. A non-nil error is a fault (fail closed: 503).
func checkStaffLiveness(ctx context.Context, pool *db.Pool, tenantID, staffID uuid.UUID, sess auth.StaffSession, role auth.Role) (observability.StaffLivenessReason, error) {
	if pool == nil {
		return "", errStaffLivenessNoDB
	}
	ctx, cancel := context.WithTimeout(ctx, staffLivenessDeadline)
	defer cancel()
	var tenantArg any
	if tenantID != uuid.Nil {
		tenantArg = tenantID
	}
	row := staffLivenessRow{staffVersion: staffVersionUnversioned}
	err := pool.QueryRowPrincipalScopeReadOnly(ctx, tenantID, staffID, staffLivenessStatementTimeout, staffLivenessSQL,
		[]any{sess.SessionID, staffID, tenantArg}, func(r pgx.Row) error {
			return r.Scan(&row.revoked, &row.absoluteExpired, &row.status, &row.role, &row.factorUsable)
		})
	if errors.Is(err, pgx.ErrNoRows) {
		return observability.StaffLivenessReasonNoSession, nil
	}
	if err != nil {
		return "", err
	}
	return staffLivenessVerdict(row, role, sess.StaffVersion), nil
}

// writeSessionRevoked is the single liveness refusal.
func writeSessionRevoked(w http.ResponseWriter, requestID string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="platform-api", error="invalid_token"`)
	apierror.Write(w, requestID, apierror.CodeSessionRevoked, "the staff session is no longer valid; sign in again")
}

// isStaffPrincipal reports whether the authenticated principal is staff.
func isStaffPrincipal(r *http.Request) bool {
	tc, err := tenant.FromContext(r.Context())
	return err == nil && tc.PrincipalType == string(auth.PrincipalStaff)
}

// requireLiveStaffSession is RequireLiveStaffSession (ADR 0119 section 7.3).
// It runs after auth.Middleware and auth.RequireStaffPrincipal, so the
// principal is a verified staff token; it refuses any request whose staff
// session is no longer live.
//
// Every decision log line carries what an investigation needs (S4a security
// review R3(a), code review CR 4): the staff id, the token's tenant scope, the
// session id, the method, the route pattern and the client IP - never a token.
// request_id and the logging tenant come from the request logger. A request
// whose client went away while the check ran is recorded as client_cancelled
// at Info (code review CR 3), never as a database error.
func requireLiveStaffSession(deps Deps) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			requestID := observability.RequestIDFromContext(ctx)
			tc, tcErr := tenant.FromContext(ctx)
			sess, ok := auth.StaffSessionFromContext(ctx)
			staffID, idErr := uuid.Parse(tc.Subject)
			scope := "platform"
			if tc.TenantID != uuid.Nil {
				scope = tc.TenantID.String()
			}
			logger := observability.LoggerFromContext(ctx, deps.Logger).With(
				"staff_id", tc.Subject, "token_tenant", scope, "sid", sess.SessionID.String(),
				"method", r.Method, "route", r.Pattern, "client_ip", trustedProxyClientIP(r, deps.TrustedProxyCount))
			if tcErr != nil || !ok || tc.PrincipalType != string(auth.PrincipalStaff) || idErr != nil || staffID == uuid.Nil {
				observability.RecordStaffLivenessDecision(ctx, observability.StaffLivenessRefused, observability.StaffLivenessReasonNoAssurance)
				logger.Warn("staff_liveness_refused", "reason", string(observability.StaffLivenessReasonNoAssurance))
				writeSessionRevoked(w, requestID)
				return
			}
			reason, err := checkStaffLiveness(ctx, deps.DB, tc.TenantID, staffID, sess, auth.Role(tc.Role))
			if err != nil {
				if ctx.Err() != nil {
					// The client went away (or the server is shutting the request down): not a database fault.
					observability.RecordStaffLivenessDecision(ctx, observability.StaffLivenessError, observability.StaffLivenessReasonClientCancelled)
					logger.Info("staff_liveness_cancelled", "reason", string(observability.StaffLivenessReasonClientCancelled))
				} else {
					observability.RecordStaffLivenessDecision(ctx, observability.StaffLivenessError, observability.StaffLivenessReasonDBError)
					logger.Error("staff_liveness_failed", "reason", string(observability.StaffLivenessReasonDBError), "error", err)
				}
				apierror.Write(w, requestID, apierror.CodeUnavailable, "service temporarily unavailable")
				return
			}
			if reason != observability.StaffLivenessReasonOK {
				observability.RecordStaffLivenessDecision(ctx, observability.StaffLivenessRefused, reason)
				logger.Warn("staff_liveness_refused", "reason", string(reason))
				writeSessionRevoked(w, requestID)
				return
			}
			observability.RecordStaffLivenessDecision(ctx, observability.StaffLivenessAllowed, observability.StaffLivenessReasonOK)
			next.ServeHTTP(w, r)
		})
	}
}

// staffFactorUsable reports whether staffID still has a usable second factor
// (usableFactorPredicate), inside the caller's scoped transaction: the staff
// refresh re-check (S3-R11).
func staffFactorUsable(ctx context.Context, tx pgx.Tx, staffID uuid.UUID) (bool, error) {
	var usable bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM staff_mfa_factors f WHERE f.staff_user_id = $1 AND `+usableFactorPredicate+`)`, staffID,
	).Scan(&usable)
	return usable, err
}
