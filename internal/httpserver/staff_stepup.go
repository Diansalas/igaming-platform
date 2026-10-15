package httpserver

// ADR 0119 slice S4b (sections 7.3, 8.2, 9.1): RequireStepUp, the last link of
// the staff route chain, and the named classifiers (the four of section 8.2
// plus alertRouteManageClass, security review finding 1).
//
//	auth.Middleware -> auth.RequireStaffPrincipal -> requireLiveStaffSession -> auth.RequireAAL2 -> requireStepUp(class) -> inner
//
// The freshness instant is the OLDER of the token's mfa_at claim and the
// sealed session row's mfa_at, which liveness has just verified (conditions
// S3-R13 / S4a-R10: never the claims alone; an access token minted before the
// latest step-up does not inherit it). "Now" is the database clock read by the
// same liveness query, the clock mfa_at was written with. A stale or missing
// step-up is 403 step_up_required with max_age and the RFC 9470 header, before
// the handler runs, so nothing was written. A fresh one places an
// auth.StepUpAssertion in the request context for the actor-proof signer
// backstop and the withdrawal policy (sections 9.1, 9.3).
//
// DEVIATION (recorded in ADR 0119 section 26): the gate runs BEFORE the route's
// own permission gate (inner carries auth.RequirePermission / RequireTenantScope,
// S4a D3), so a caller without the permission may be asked to step up first and
// is then refused 403 forbidden. The classifier routes return none for a caller
// their handler refuses anyway, so those handlers' audited denials are kept.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// stepUpClassifier decides a classifier route's class from the request. It may
// read the body only through bufferedBody (the handler then decodes the SAME
// bytes into the SAME type with the same decoder, so the class cannot change
// between the classifier and the handler). A non-nil error is a fault (503).
type stepUpClassifier func(deps Deps, r *http.Request) (auth.StepUpClass, error)

// stepUpClassifiers maps each routeClassifiers name to its function (a static
// test pins that the two sets are equal).
var stepUpClassifiers = map[string]stepUpClassifier{
	"launchActionClass":          launchActionClass,
	"financialPolicyChangeClass": financialPolicyChangeClass,
	"alertResolveClass":          alertResolveClass,
	"rgLimitChangeClass":         rgLimitChangeClass,
	"alertRouteManageClass":      alertRouteManageClass,
	"amlHoldPlaceClass":          amlHoldPlaceClass,
	"amlTransitionClass":         amlTransitionClass,
}

// requireStepUp enforces class. It must run after requireLiveStaffSession (it
// needs the verified sealed assurance) and auth.RequireAAL2.
func requireStepUp(deps Deps, class routeClass) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if class.classifier == "" && class.static == auth.StepUpNone {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := r.Context()
			requestID := observability.RequestIDFromContext(ctx)
			logger := observability.LoggerFromContext(ctx, deps.Logger)
			c := class.static
			if class.classifier != "" {
				fn := stepUpClassifiers[class.classifier]
				if fn == nil {
					// Unreachable: staffRoute refuses an unknown classifier at registration.
					apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
					return
				}
				var err error
				if c, err = fn(deps, r); err != nil {
					logger.Error("staff_step_up_classifier_failed", "classifier", class.classifier, "error", err)
					apierror.Write(w, requestID, apierror.CodeUnavailable, "service temporarily unavailable")
					return
				}
				if !c.Valid() {
					c = auth.StepUpCritical // fail closed to the ceiling
				}
				if c == auth.StepUpNone {
					next.ServeHTTP(w, r)
					return
				}
			}
			m := deps.StaffMFA
			if m == nil {
				writeStaffAuthUnavailable(w, requestID)
				return
			}
			maxAge, ok := m.stepUp.MaxAge(c)
			if !ok {
				apierror.Write(w, requestID, apierror.CodeInternal, "internal error")
				return
			}
			a, ok := stepUpAssertionFor(r)
			if !ok {
				// No verified session assurance in the context: the chain is mis-composed. Fail closed.
				writeSessionRevoked(w, requestID)
				return
			}
			live, _ := liveStaffAssuranceFrom(ctx)
			if !auth.StepUpFresh(a.MFAAt, live.DBNow, maxAge) {
				logger.Info("staff_step_up_required", "class", string(c), "max_age_seconds", int(maxAge/time.Second),
					"route", r.Pattern, "sid", a.SessionID.String())
				auth.WriteStepUpRequired(w, requestID, maxAge)
				return
			}
			a.Class = c
			next.ServeHTTP(w, r.WithContext(auth.ContextWithStepUpAssertion(ctx, a)))
		})
	}
}

// stepUpAssertionFor builds the request's candidate assertion: subject and
// session from the verified token, MFAAt the older of the token claim and the
// sealed row (verified by liveness). ok is false without both.
func stepUpAssertionFor(r *http.Request) (auth.StepUpAssertion, bool) {
	ctx := r.Context()
	tc, err := tenant.FromContext(ctx)
	if err != nil {
		return auth.StepUpAssertion{}, false
	}
	sub, err := uuid.Parse(tc.Subject)
	if err != nil || sub == uuid.Nil {
		return auth.StepUpAssertion{}, false
	}
	sess, ok := auth.StaffSessionFromContext(ctx)
	if !ok {
		return auth.StepUpAssertion{}, false
	}
	live, ok := liveStaffAssuranceFrom(ctx)
	if !ok || live.SessionID != sess.SessionID || live.DBNow.IsZero() {
		return auth.StepUpAssertion{}, false
	}
	mfaAt := live.MFAAt
	if sess.MFAAt.Before(mfaAt) {
		mfaAt = sess.MFAAt
	}
	return auth.StepUpAssertion{Subject: sub, SessionID: sess.SessionID, MFAAt: mfaAt}, true
}

// ---- the four classifiers (ADR 0119 section 8.2) ------------------------------

// bufferedBody reads the request body (bounded exactly like decodeJSON) and
// replaces it with an identical reader, so the handler decodes the same bytes.
func bufferedBody(r *http.Request) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r.Body, maxRequestBodyBytes+1))
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(b))
	return b, err
}

// decodeLikeHandler decodes b with decodeJSON's exact decoder settings.
func decodeLikeHandler(b []byte, dst any) error {
	if int64(len(b)) > maxRequestBodyBytes {
		return errors.New("body too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// launchActionClass: none for action = suspend (ADR 0112 section 6.3
// asymmetry; this route then refuses it with 400, suspensions have their own
// routes), critical for every other action and for a body the handler will
// refuse (fail closed to the ceiling).
func launchActionClass(_ Deps, r *http.Request) (auth.StepUpClass, error) {
	b, err := bufferedBody(r)
	if err != nil {
		return auth.StepUpCritical, nil
	}
	var body launchCreateBody
	if err := decodeLikeHandler(b, &body); err != nil {
		return auth.StepUpCritical, nil
	}
	if body.Action == launchgov.ActionSuspend {
		return auth.StepUpNone, nil
	}
	return auth.StepUpCritical, nil
}

// financialPolicyChangeClass: critical when the caller holds
// financial_policy:author (it authors policy, platform_admin); none for a caller
// that holds only financial_policy:tighten (migration 0113 refuses anything but
// a tightening from a tenant session: a protective action, rule 8.1) and for a
// caller the route's permission gate refuses anyway.
func financialPolicyChangeClass(_ Deps, r *http.Request) (auth.StepUpClass, error) {
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		return auth.StepUpCritical, nil
	}
	if auth.RoleHasPermission(auth.Role(tc.Role), auth.PermFinancialPolicyAuthor) {
		return auth.StepUpCritical, nil
	}
	return auth.StepUpNone, nil
}

// alertResolveCriticalKindPrefixes and the p1 severity pin which alerts need a
// critical step-up to resolve (ADR 0119 section 8.2 (f): resolving a security
// or P1 signal from a stolen session would hide it). Every staff.* kind (the
// MFA, session-integrity, hard-lock, recovery-used and failure-pattern signals
// and their platform variants) and every p1 alert (integrity, reconciliation,
// alerting.raise_failed, ...).
var alertResolveCriticalKindPrefixes = []string{"staff."}

const alertResolveCriticalSeverity = "p1"

func alertNeedsCriticalStepUp(kind, severity string) bool {
	if severity == alertResolveCriticalSeverity {
		return true
	}
	for _, p := range alertResolveCriticalKindPrefixes {
		if strings.HasPrefix(kind, p) {
			return true
		}
	}
	return false
}

// alertResolveClass: critical for a pinned security / P1 alert, and for an
// alert that cannot be read (unknown id: fail closed); none otherwise. none too
// for a caller the handler refuses anyway (no alert:manage, or a tenant
// session), so the handler's audited denial is kept.
func alertResolveClass(deps Deps, r *http.Request) (auth.StepUpClass, error) {
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		return auth.StepUpCritical, nil
	}
	if !auth.RoleHasPermission(auth.Role(tc.Role), auth.PermAlertManage) || tc.TenantID != uuid.Nil {
		return auth.StepUpNone, nil
	}
	actor, err := uuid.Parse(tc.Subject)
	if err != nil {
		return auth.StepUpCritical, nil
	}
	alertID, err := uuid.Parse(r.PathValue("alertID"))
	if err != nil {
		return auth.StepUpNone, nil // the handler answers 400 and writes nothing
	}
	if deps.DB == nil {
		return "", errors.New("no database")
	}
	var kind, severity string
	err = deps.DB.WithPlatformAdmin(r.Context(), actor, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT kind, severity FROM alerts WHERE id = $1 AND tenant_id IS NULL`, alertID).Scan(&kind, &severity)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return auth.StepUpCritical, nil
	}
	if err != nil {
		return "", err
	}
	if alertNeedsCriticalStepUp(kind, severity) {
		return auth.StepUpCritical, nil
	}
	return auth.StepUpNone, nil
}

// alertRouteManageClass (security review of S4b, finding 1): POST
// /v1/admin/alerting/routes checks its permission INSIDE the handler so that a
// refusal is audited (alert_route.create denied). A static critical class would
// answer an unauthorised caller with step_up_required before that audit row is
// written. So: none for a caller the handler refuses anyway (no
// alert:route_manage, or a tenant session) - the handler then writes its
// audited 403 - and critical for every caller the handler admits. No body read.
func alertRouteManageClass(_ Deps, r *http.Request) (auth.StepUpClass, error) {
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		return auth.StepUpCritical, nil
	}
	if !auth.RoleHasPermission(auth.Role(tc.Role), auth.PermAlertRouteManage) || tc.TenantID != uuid.Nil {
		return auth.StepUpNone, nil
	}
	return auth.StepUpCritical, nil
}

// rgLimitChangeClass: critical for a removal (value null) and for a body the
// handler will refuse; none for a value. ADR 0115: a staff INCREASE is refused
// by the RG domain (RG_STEP_UP_REQUIRED) whatever the step-up state, because
// S4b does NOT enable staff increases; ADR 0115's own classifier slice must
// move the increase decision into the domain transaction (the domain knows the
// current limit under its person lock; this route cannot) before it does.
func rgLimitChangeClass(_ Deps, r *http.Request) (auth.StepUpClass, error) {
	b, err := bufferedBody(r)
	if err != nil {
		return auth.StepUpCritical, nil
	}
	var body staffSetLimitRequest
	if err := decodeLikeHandler(b, &body); err != nil {
		return auth.StepUpCritical, nil
	}
	v := bytes.TrimSpace(body.Value)
	if len(v) == 0 || bytes.Equal(v, []byte("null")) {
		return auth.StepUpCritical, nil
	}
	return auth.StepUpNone, nil
}

// amlHoldPlaceClass (ADR 0116 A7, security SR7-1): a `confirmed_sanctions` hold gates ALL value movement and only the four-eyes
// case workflow can lift it, so placing one from a stale or stolen session is a denial-of-service lever (ADR 0119 section 8.1:
// a protective action that is irreversible by one person is not exempt). critical for it; none for a `manual` hold (the
// officer's own single-officer lift exists). Anything else, including a body the handler will refuse, is classified at the
// ceiling. The handler decodes the SAME buffered bytes.
func amlHoldPlaceClass(_ Deps, r *http.Request) (auth.StepUpClass, error) {
	b, err := bufferedBody(r)
	if err != nil {
		return auth.StepUpCritical, nil
	}
	var body amlHoldPlaceRequest
	if err := decodeLikeHandler(b, &body); err != nil {
		return auth.StepUpCritical, nil
	}
	if aml.ReasonClass(body.ReasonClass) == aml.ReasonManual {
		return auth.StepUpNone, nil
	}
	return auth.StepUpCritical, nil
}

// amlTransitionClass (ADR 0116 A7, security SR7-5): closing an investigation with no action loosens a regulatory control (rule
// 8.1 (e)), so a move to closed_no_action is critical; the other moves only progress or reopen a case (none). An unknown or
// missing target, or a body the handler will refuse, is classified at the ceiling.
func amlTransitionClass(_ Deps, r *http.Request) (auth.StepUpClass, error) {
	b, err := bufferedBody(r)
	if err != nil {
		return auth.StepUpCritical, nil
	}
	var body amlTransitionRequest
	if err := decodeLikeHandler(b, &body); err != nil {
		return auth.StepUpCritical, nil
	}
	switch aml.CaseStatus(body.To) {
	case aml.StatusInReview, aml.StatusEscalated, aml.StatusSARDecisionPending:
		return auth.StepUpNone, nil
	}
	return auth.StepUpCritical, nil
}
