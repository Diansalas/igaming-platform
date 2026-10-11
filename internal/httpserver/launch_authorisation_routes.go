// ADR 0112 slice 4: the launch authorisation HTTP routes (tenant and brand status governance).
//
// What this file is. A thin, authenticating, error-mapping layer over internal/launchgov, which
// owns every rule (state machine, four-eyes, readiness, hard preconditions, signed actor proof,
// audit). Nothing here decides anything about a launch.
//
// What it is NOT. A recorded launch decision is evidence that named people took a decision on
// stated inputs. It is not a legal or regulatory authorisation and it does not determine whether a
// licence is required or whether any jurisdiction permits operation. Every 2xx response carries
// the fixed text launchgov.Disclaimer; no response states a legal conclusion.
//
// Authentication and tenant binding (CLAUDE.md multi-tenancy): every route is
// auth.Middleware + RequireStaffPrincipal + RequirePermission(<static launch_authorisation:*>).
// The tenant is ONLY the {tenantID} path value validated against the authenticated context: a
// tenant-scoped caller naming another tenant gets the same 404 as an unknown id (no existence
// oracle, ADR 0112 8.3); a body "tenant_id" is accepted and IGNORED. The only other ids a caller
// names are references resolved INSIDE the route tenant by a tenant-bound lookup (a brand_id in
// the create body or in a query string, a requestID in the path): a foreign one is a 404.
// The actor (staff id, scope, role) is built from the verified token, never from a body. The
// scope is platform (token without tenant) or tenant (token with tenant); there is no way to ask
// for a platform_acting session here, and the service refuses one anyway (ErrActingSession).
//
// Signed actor proof (ADR 0110): the service signs the proof for the authenticated principal
// right before each governed write (request, cancel, approve/reject, suspension); this layer
// never sees a key. A process with no proof issuer fails closed with a 500 and writes nothing.
//
// Idempotency: the admin four-eyes routes of this repository carry no Idempotency-Key header
// (that convention belongs to player money movement). These routes are idempotent by state:
// a replayed create is LAUNCH_REQUEST_PENDING, a replayed approve/reject by the same Person is
// LAUNCH_ALREADY_DECIDED, a replayed cancel is LAUNCH_REQUEST_NOT_PENDING, a replayed suspension
// is LAUNCH_ILLEGAL_TRANSITION (already suspended); each is a 409 that records nothing.
//
// Errors are the closed LAUNCH_* codes of internal/apierror (message = a fixed sentence, never
// the underlying error text; check_codes = a closed H-/R- vocabulary). Every refusal that rolled
// back a governed write is audited once by the service (separate transaction, codes only).
package httpserver

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

func registerLaunchAuthorisationRoutes(mux *http.ServeMux, deps Deps) {
	staff := func(perm auth.Permission, h http.Handler) http.Handler {
		return auth.Middleware(deps.AuthIssuer)(auth.RequireStaffPrincipal(auth.RequirePermission(perm)(h)))
	}
	// Literal patterns only (the webhook route guard fails closed on any non-literal pattern).
	mux.Handle("GET /v1/admin/tenants/{tenantID}/launch-readiness",
		staff(auth.PermLaunchAuthorisationRead, newLaunchReadinessHandler(deps)))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/launch-authorisations",
		staff(auth.PermLaunchAuthorisationRequest, newLaunchCreateHandler(deps)))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/launch-authorisations",
		staff(auth.PermLaunchAuthorisationRead, newLaunchListHandler(deps)))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}",
		staff(auth.PermLaunchAuthorisationRead, newLaunchGetHandler(deps)))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/approve",
		staff(auth.PermLaunchAuthorisationApprove, newLaunchDecideHandler(deps, launchgov.DecisionApprove)))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/reject",
		staff(auth.PermLaunchAuthorisationApprove, newLaunchDecideHandler(deps, launchgov.DecisionReject)))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/launch-authorisations/{requestID}/cancel",
		staff(auth.PermLaunchAuthorisationRequest, newLaunchCancelHandler(deps)))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/brands/{brandID}/suspend",
		staff(auth.PermLaunchAuthorisationSuspend, newLaunchSuspendHandler(deps, true)))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/suspend",
		staff(auth.PermLaunchAuthorisationSuspend, newLaunchSuspendHandler(deps, false)))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/launch-status-history",
		staff(auth.PermLaunchAuthorisationRead, newLaunchHistoryHandler(deps)))
	// Platform-wide approval queue (platform staff only; a tenant caller is refused 403).
	mux.Handle("GET /v1/admin/launch-authorisations",
		staff(auth.PermLaunchAuthorisationRead, newLaunchQueueHandler(deps)))
}

// launchCall is the per-request context.
type launchCall struct {
	requestID string
	logger    observabilityLogger
	actor     launchgov.Actor
	target    uuid.UUID // the {tenantID} path value (zero for the platform-wide queue)
}

// beginLaunchCall authenticates the caller, builds the launchgov.Actor from the verified token and
// validates the {tenantID} path value against it. withTenant false is the platform-wide queue.
func beginLaunchCall(deps Deps, w http.ResponseWriter, r *http.Request, op string, withTenant bool) (launchCall, bool) {
	c := launchCall{
		requestID: observability.RequestIDFromContext(r.Context()),
		logger:    observability.LoggerFromContext(r.Context(), deps.Logger),
	}
	if deps.LaunchAuthorisation == nil {
		apierror.Write(w, c.requestID, apierror.CodeLaunchUnavailable, "launch authorisation is not available in this process")
		return c, false
	}
	tc, err := tenant.FromContext(r.Context())
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	staffID, err := uuid.Parse(tc.Subject)
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeUnauthorized, "no authenticated context")
		return c, false
	}
	c.actor = launchgov.Actor{
		StaffID: staffID, Scope: launchgov.ScopePlatform, Role: auth.Role(tc.Role),
		IPAddress: clientIP(r), RequestID: c.requestID,
	}
	if tc.TenantID != uuid.Nil {
		c.actor.Scope, c.actor.TenantID = launchgov.ScopeTenant, tc.TenantID
	}
	if !withTenant {
		return c, true
	}
	target, err := uuid.Parse(r.PathValue("tenantID"))
	if err != nil {
		apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
		return c, false
	}
	c.target = target
	if !canActOnTenant(tc, target) {
		// A foreign tenant is indistinguishable from an unknown one (no existence oracle).
		c.logger.Warn("launch_authorisation_denied", "op", op, "reason", "foreign_tenant")
		apierror.Write(w, c.requestID, apierror.CodeNotFound, "not found")
		return c, false
	}
	return c, true
}

// ---- error mapping -----------------------------------------------------------------------

// launchErrorCode maps a launchgov error to the closed API code, the message (a fixed sentence)
// and the check codes. It is a pure function so the whole mapping is unit-tested without a
// database. Order matters: the typed and specific sentinels come before the broad ones.
func launchErrorCode(err error) (apierror.Code, string, []string) {
	var blocked *tenant.CloseBlockedError
	var pre *launchgov.PreconditionError
	var refused *launchgov.RefusedAtExecutionError
	switch {
	case errors.As(err, &blocked):
		return apierror.CodeTenantCloseBlockedOpenRounds, "closure refused: open sportsbook bets exist", nil
	case errors.Is(err, launchgov.ErrNotFound):
		return apierror.CodeNotFound, "not found", nil
	case errors.Is(err, launchgov.ErrActingSession), errors.Is(err, launchgov.ErrForbidden):
		return apierror.CodeForbidden, "forbidden", nil
	case errors.Is(err, launchgov.ErrInvalidInput):
		return apierror.CodeValidation, "invalid request", nil
	case errors.As(err, &refused):
		return apierror.CodeLaunchRefusedAtExecution, "the decision was recorded but execution was refused", []string{refused.Code}
	case errors.Is(err, launchgov.ErrUnacknowledged):
		return apierror.CodeLaunchUnacknowledgedChecks, "every check that is not pass must be acknowledged", preCodes(err, &pre)
	case errors.Is(err, launchgov.ErrWithdrawalsHolding):
		return apierror.CodeLaunchWithdrawalsHolding, "closure refused: withdrawal requests still hold player funds", preCodes(err, &pre)
	case errors.Is(err, launchgov.ErrPreconditionFailed):
		return apierror.CodeLaunchPreconditionFailed, "a hard precondition is not met", preCodes(err, &pre)
	case errors.Is(err, launchgov.ErrRequestPending):
		return apierror.CodeLaunchRequestPending, "a request for this subject is already pending", nil
	case errors.Is(err, launchgov.ErrSelfApproval):
		return apierror.CodeLaunchSelfApproval, "the requester, or the same person, may not decide this request", nil
	case errors.Is(err, launchgov.ErrAlreadyDecided):
		return apierror.CodeLaunchAlreadyDecided, "this person already decided this request", nil
	case errors.Is(err, launchgov.ErrIllegalTransition):
		return apierror.CodeLaunchIllegalTransition, "the action is not legal from the current status", nil
	case errors.Is(err, launchgov.ErrSubjectClosed):
		return apierror.CodeLaunchSubjectClosed, "the subject is closed (terminal)", nil
	case errors.Is(err, launchgov.ErrNotPending):
		return apierror.CodeLaunchRequestNotPending, "the request is not pending", nil
	case errors.Is(err, launchgov.ErrStaleReadiness):
		return apierror.CodeLaunchStaleReadiness, "the readiness seen is stale; re-read it and decide again", nil
	case errors.Is(err, launchgov.ErrPriorReject):
		return apierror.CodeLaunchPriorReject, "an identical request was rejected within the request window", nil
	case errors.Is(err, launchgov.ErrPriorRejecter):
		return apierror.CodeLaunchPriorRejecter, "this approver rejected a variant of this request and it carries no change note", nil
	case errors.Is(err, launchgov.ErrNotLegacyBaseline):
		return apierror.CodeLaunchNotLegacyBaseline, "only a legacy-baseline subject can be ratified", nil
	case errors.Is(err, launchgov.ErrRetryableLockTimeout):
		return apierror.CodeLaunchRetryableLockTimeout, "the subject is busy; retry", nil
	}
	if code := launchgov.GuardCode(err); code != "" {
		switch {
		case code == "LA023":
			return apierror.CodeLaunchWithdrawalsHolding, "closure refused: withdrawal requests still hold player funds", []string{code}
		case code == "LA024":
			return apierror.CodeLaunchOpenBetsBlockClosure, "brand closure refused: open sportsbook bets exist", []string{code}
		case code == "LA001":
			return apierror.CodeForbidden, "forbidden", nil
		case strings.HasPrefix(code, "AP"):
			return apierror.CodeLaunchProofRefused, "the signed actor proof was refused", []string{code}
		default:
			return apierror.CodeLaunchGuardRefused, "refused by a database guard", []string{code}
		}
	}
	return apierror.CodeInternal, "internal error", nil
}

func preCodes(err error, pre **launchgov.PreconditionError) []string {
	if errors.As(err, pre) {
		return (*pre).Codes
	}
	return nil
}

func writeLaunchError(w http.ResponseWriter, c launchCall, op string, err error) {
	code, msg, checks := launchErrorCode(err)
	switch code {
	case apierror.CodeInternal:
		// The underlying error text stays in the log; the body is a fixed sentence.
		c.logger.Error("launch_authorisation_failed", "op", op, "err", err.Error())
	case apierror.CodeNotFound, apierror.CodeValidation:
	default:
		c.logger.Warn("launch_authorisation_refused", "op", op, "code", string(code))
	}
	apierror.WriteWithCheckCodes(w, c.requestID, code, msg, checks)
}

// ---- DTOs --------------------------------------------------------------------------------

type launchCheckDTO struct {
	Code       string `json:"code"`
	Result     string `json:"result"`
	DetailCode string `json:"detail_code"`
}

type launchReadinessDTO struct {
	Version     int              `json:"version"`
	ComputedAt  string           `json:"computed_at"`
	Environment string           `json:"environment"`
	Checks      []launchCheckDTO `json:"checks"`
	// ReadinessHash is what an approver passes back as seen_readiness_hash.
	ReadinessHash string `json:"readiness_hash"`
	// NonPassingCheckCodes is exactly the set a request must acknowledge (H-5).
	NonPassingCheckCodes []string `json:"non_passing_check_codes"`
	// Counts and Balances are live display data of R-PLAYER-FUNDS (not hashed); Balances are
	// DECIMAL STRINGS per asset (never a JSON number).
	Counts   map[string]int64  `json:"counts,omitempty"`
	Balances map[string]string `json:"balances,omitempty"`
	Notices  []string          `json:"notices,omitempty"`
}

func toLaunchReadinessDTO(r launchgov.Readiness) launchReadinessDTO {
	d := launchReadinessDTO{
		Version: r.Version, ComputedAt: r.ComputedAt.UTC().Format(time.RFC3339), Environment: r.Environment,
		Checks: []launchCheckDTO{}, ReadinessHash: r.Hash(), NonPassingCheckCodes: r.NonPassing(),
		Counts: r.Info, Balances: r.Balances, Notices: r.Notices,
	}
	if d.NonPassingCheckCodes == nil {
		d.NonPassingCheckCodes = []string{}
	}
	for _, c := range r.Checks {
		d.Checks = append(d.Checks, launchCheckDTO{Code: c.Code, Result: c.Result, DetailCode: c.DetailCode})
	}
	return d
}

type launchPriorRejectDTO struct {
	RequestID   string `json:"request_id"`
	DecidedAt   string `json:"decided_at"`
	ReasonCode  string `json:"reason_code"`
	SamePayload bool   `json:"same_payload"`
}

type launchApprovalDTO struct {
	ID                      string `json:"id"`
	Decision                string `json:"decision"`
	DecidedBy               string `json:"decided_by"`
	ReasonCode              string `json:"reason_code"`
	ReadinessHashAtDecision string `json:"readiness_hash_at_decision"`
	DecidedAt               string `json:"decided_at"`
}

type launchRequestDTO struct {
	ID          string  `json:"id"`
	TenantID    string  `json:"tenant_id"`
	SubjectKind string  `json:"subject_kind"`
	BrandID     *string `json:"brand_id,omitempty"`
	Action      string  `json:"action"`
	FromStatus  string  `json:"from_status"`
	ToStatus    string  `json:"to_status"`
	Status      string  `json:"status"`

	LicensingModel                  *string  `json:"licensing_model,omitempty"`
	LicensingStatus                 *string  `json:"licensing_status,omitempty"`
	LicenceID                       *string  `json:"licence_id,omitempty"`
	DeterminationReference          *string  `json:"determination_reference,omitempty"`
	DeterminationByRole             *string  `json:"determination_by_role,omitempty"`
	ResponsibleOperatorName         *string  `json:"responsible_operator_name,omitempty"`
	ResponsibleOperatorRegistration string   `json:"responsible_operator_registration,omitempty"`
	JurisdictionIDs                 []string `json:"jurisdiction_ids"`
	ConditionsNote                  string   `json:"conditions_note,omitempty"`

	ReadinessSnapshotHash  string   `json:"readiness_snapshot_hash"`
	AcknowledgedCheckCodes []string `json:"acknowledged_check_codes"`
	ReasonCode             string   `json:"reason_code"`

	RequestedBy       string  `json:"requested_by"`
	RequestedByScope  string  `json:"requested_by_scope"`
	RequiredApprovals int     `json:"required_approvals"`
	PayloadHash       string  `json:"payload_hash"`
	CreatedAt         string  `json:"created_at"`
	ExpiresAt         string  `json:"expires_at"`
	DecidedAt         *string `json:"decided_at,omitempty"`
	RefusalCode       *string `json:"refusal_code,omitempty"`

	PriorRejects []launchPriorRejectDTO `json:"prior_rejects,omitempty"`
	Approvals    []launchApprovalDTO    `json:"approvals,omitempty"`
}

func strp(u *uuid.UUID) *string {
	if u == nil {
		return nil
	}
	s := u.String()
	return &s
}

func toLaunchRequestDTO(r launchgov.Request) launchRequestDTO {
	d := launchRequestDTO{
		ID: r.ID.String(), TenantID: r.TenantID.String(), SubjectKind: r.SubjectKind, BrandID: strp(r.BrandID),
		Action: r.Action, FromStatus: r.FromStatus, ToStatus: r.ToStatus, Status: r.Status,
		LicensingModel: r.LicensingModel, LicensingStatus: r.LicensingStatus, LicenceID: strp(r.LicenceID),
		DeterminationReference: r.DeterminationReference, DeterminationByRole: r.DeterminationByRole,
		ResponsibleOperatorName: r.ResponsibleOperatorName, ResponsibleOperatorRegistration: r.ResponsibleOperatorRegistration,
		JurisdictionIDs: []string{}, ConditionsNote: r.ConditionsNote,
		ReadinessSnapshotHash: r.ReadinessSnapshotHash, AcknowledgedCheckCodes: r.AcknowledgedCheckCodes, ReasonCode: r.ReasonCode,
		RequestedBy: r.RequestedBy.String(), RequestedByScope: r.RequestedByScope, RequiredApprovals: r.RequiredApprovals,
		PayloadHash: r.PayloadHash, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339), ExpiresAt: r.ExpiresAt.UTC().Format(time.RFC3339),
		RefusalCode: r.RefusalCode,
	}
	if d.AcknowledgedCheckCodes == nil {
		d.AcknowledgedCheckCodes = []string{}
	}
	for _, j := range r.JurisdictionIDs {
		d.JurisdictionIDs = append(d.JurisdictionIDs, j.String())
	}
	if r.DecidedAt != nil {
		s := r.DecidedAt.UTC().Format(time.RFC3339)
		d.DecidedAt = &s
	}
	for _, p := range r.PriorRejects {
		d.PriorRejects = append(d.PriorRejects, launchPriorRejectDTO{
			RequestID: p.RequestID.String(), DecidedAt: p.DecidedAt.UTC().Format(time.RFC3339), ReasonCode: p.ReasonCode, SamePayload: p.SamePayload,
		})
	}
	return d
}

func toLaunchPriorRejects(ps []launchgov.PriorReject) []launchPriorRejectDTO {
	out := []launchPriorRejectDTO{}
	for _, p := range ps {
		out = append(out, launchPriorRejectDTO{
			RequestID: p.RequestID.String(), DecidedAt: p.DecidedAt.UTC().Format(time.RFC3339), ReasonCode: p.ReasonCode, SamePayload: p.SamePayload,
		})
	}
	return out
}

// ---- request parsing ---------------------------------------------------------------------

// optUUID parses an optional uuid query/body value: absent is nil, malformed is an error.
func optUUID(s string) (*uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	u, err := uuid.Parse(s)
	if err != nil || u == uuid.Nil {
		return nil, errors.New("invalid uuid")
	}
	return &u, nil
}

func parseUUIDList(s string) ([]uuid.UUID, error) {
	if s == "" {
		return nil, nil
	}
	var out []uuid.UUID
	for _, p := range strings.Split(s, ",") {
		u, err := uuid.Parse(strings.TrimSpace(p))
		if err != nil || u == uuid.Nil {
			return nil, errors.New("invalid uuid")
		}
		out = append(out, u)
	}
	return out, nil
}

var launchRequestStatuses = map[string]bool{
	launchgov.ReqPending: true, launchgov.ReqExecuting: true, launchgov.ReqExecuted: true, launchgov.ReqRefusedAtExec: true,
	launchgov.ReqRejected: true, launchgov.ReqCancelled: true, launchgov.ReqExpired: true, launchgov.ReqSuperseded: true,
}

func parseLaunchStatuses(q string) ([]string, bool) {
	if q == "" {
		return nil, true
	}
	var out []string
	for _, s := range strings.Split(q, ",") {
		s = strings.TrimSpace(s)
		if !launchRequestStatuses[s] {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

func parseLaunchLimit(q string) (int, bool) {
	if q == "" {
		return 0, true
	}
	n, err := strconv.Atoi(q)
	if err != nil || n < 1 || n > 500 {
		return 0, false
	}
	return n, true
}

func (c launchCall) badRequest(w http.ResponseWriter) {
	apierror.Write(w, c.requestID, apierror.CodeInvalidRequest, "invalid request")
}

// withDisclaimer wraps a payload under the fixed envelope every 2xx launch response uses.
type launchEnvelope struct {
	Disclaimer string `json:"disclaimer"`
}

// ---- handlers ----------------------------------------------------------------------------

// GET .../launch-readiness?action=&brand_id=&jurisdiction_ids=&licensing_model=&licensing_status=&licence_id=
// Read-only: writes nothing. The query never carries free text.
func newLaunchReadinessHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, "readiness", true)
		if !ok {
			return
		}
		q := r.URL.Query()
		brandID, err := optUUID(q.Get("brand_id"))
		if err != nil {
			c.badRequest(w)
			return
		}
		juris, err := parseUUIDList(q.Get("jurisdiction_ids"))
		if err != nil {
			c.badRequest(w)
			return
		}
		licID, err := optUUID(q.Get("licence_id"))
		if err != nil {
			c.badRequest(w)
			return
		}
		in := launchgov.EvalInput{
			Subject: launchgov.Subject{TenantID: c.target, BrandID: brandID}, Action: q.Get("action"), JurisdictionIDs: juris,
		}
		if q.Get("licensing_model") != "" || q.Get("licensing_status") != "" || licID != nil {
			in.Licensing = &launchgov.LicensingRecord{Model: q.Get("licensing_model"), Status: q.Get("licensing_status"), LicenceID: licID}
		}
		rd, err := deps.LaunchAuthorisation.ReadReadiness(r.Context(), c.actor, in)
		if err != nil {
			writeLaunchError(w, c, "readiness", err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			launchEnvelope
			Readiness launchReadinessDTO `json:"readiness"`
		}{launchEnvelope{launchgov.Disclaimer}, toLaunchReadinessDTO(rd)})
	}
}

type launchLicensingBody struct {
	Model                  string  `json:"model"`
	Status                 string  `json:"status"`
	LicenceID              *string `json:"licence_id,omitempty"`
	DeterminationReference string  `json:"determination_reference,omitempty"`
	DeterminationByRole    string  `json:"determination_by_role,omitempty"`
}

type launchCreateBody struct {
	// TenantID is accepted and IGNORED: the tenant is the validated path value.
	TenantID *string `json:"tenant_id,omitempty"`
	// BrandID selects a brand OF THE ROUTE TENANT (looked up tenant-bound; a foreign brand is a 404).
	// Absent means the tenant itself is the subject.
	BrandID                         *string              `json:"brand_id,omitempty"`
	Action                          string               `json:"action"`
	ReasonCode                      string               `json:"reason_code"`
	Licensing                       *launchLicensingBody `json:"licensing,omitempty"`
	ResponsibleOperatorName         string               `json:"responsible_operator_name,omitempty"`
	ResponsibleOperatorRegistration string               `json:"responsible_operator_registration,omitempty"`
	JurisdictionIDs                 []string             `json:"jurisdiction_ids,omitempty"`
	ConditionsNote                  string               `json:"conditions_note,omitempty"`
	AcknowledgedCheckCodes          []string             `json:"acknowledged_check_codes,omitempty"`
}

type launchResultResponse struct {
	launchEnvelope
	Request    launchRequestDTO   `json:"request"`
	Readiness  launchReadinessDTO `json:"readiness"`
	Executed   bool               `json:"executed"`
	Superseded []string           `json:"superseded_request_ids,omitempty"`
}

func toLaunchResult(res *launchgov.RequestResult) launchResultResponse {
	out := launchResultResponse{
		launchEnvelope: launchEnvelope{launchgov.Disclaimer},
		Request:        toLaunchRequestDTO(res.Request), Readiness: toLaunchReadinessDTO(res.Readiness), Executed: res.Executed,
	}
	for _, id := range res.Superseded {
		out.Superseded = append(out.Superseded, id.String())
	}
	return out
}

func newLaunchCreateHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, "request", true)
		if !ok {
			return
		}
		var body launchCreateBody
		if err := decodeJSON(r, &body); err != nil {
			c.badRequest(w)
			return
		}
		brandID := (*uuid.UUID)(nil)
		if body.BrandID != nil {
			u, err := optUUID(*body.BrandID)
			if err != nil || u == nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
				return
			}
			brandID = u
		}
		sub := launchgov.Subject{TenantID: c.target, BrandID: brandID}
		if body.Action == launchgov.ActionSuspend {
			// A suspension is requested AND executed in one transaction by one authorised person;
			// the service checks launch_authorisation:suspend itself.
			res, err := deps.LaunchAuthorisation.Suspend(r.Context(), c.actor, launchgov.SuspendInput{Subject: sub, ReasonCode: body.ReasonCode, ConditionsNote: body.ConditionsNote})
			if err != nil {
				writeLaunchError(w, c, "suspend", err)
				return
			}
			writeJSON(w, http.StatusCreated, toLaunchResult(res))
			return
		}
		in := launchgov.RequestInput{
			Subject: sub, Action: body.Action, ReasonCode: body.ReasonCode,
			ResponsibleOperatorName: body.ResponsibleOperatorName, ResponsibleOperatorRegistration: body.ResponsibleOperatorRegistration,
			ConditionsNote: body.ConditionsNote, AcknowledgedCheckCodes: body.AcknowledgedCheckCodes,
		}
		for _, j := range body.JurisdictionIDs {
			u, err := uuid.Parse(j)
			if err != nil || u == uuid.Nil {
				apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
				return
			}
			in.JurisdictionIDs = append(in.JurisdictionIDs, u)
		}
		if body.Licensing != nil {
			rec := &launchgov.LicensingRecord{
				Model: body.Licensing.Model, Status: body.Licensing.Status,
				DeterminationReference: body.Licensing.DeterminationReference, DeterminationByRole: body.Licensing.DeterminationByRole,
			}
			if body.Licensing.LicenceID != nil {
				u, err := optUUID(*body.Licensing.LicenceID)
				if err != nil || u == nil {
					apierror.Write(w, c.requestID, apierror.CodeValidation, "invalid request")
					return
				}
				rec.LicenceID = u
			}
			in.Licensing = rec
		}
		res, err := deps.LaunchAuthorisation.Request(r.Context(), c.actor, in)
		if err != nil {
			writeLaunchError(w, c, "request", err)
			return
		}
		writeJSON(w, http.StatusCreated, toLaunchResult(res))
	}
}

func newLaunchListHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, "list", true)
		if !ok {
			return
		}
		q := r.URL.Query()
		brandID, err := optUUID(q.Get("brand_id"))
		statuses, sok := parseLaunchStatuses(q.Get("status"))
		limit, lok := parseLaunchLimit(q.Get("limit"))
		if err != nil || !sok || !lok {
			c.badRequest(w)
			return
		}
		rs, err := deps.LaunchAuthorisation.ListRequests(r.Context(), c.actor, c.target, launchgov.ListFilter{BrandID: brandID, Statuses: statuses, Limit: limit})
		if err != nil {
			writeLaunchError(w, c, "list", err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			launchEnvelope
			Items []launchRequestDTO `json:"items"`
		}{launchEnvelope{launchgov.Disclaimer}, toLaunchRequestDTOs(rs)})
	}
}

func toLaunchRequestDTOs(rs []launchgov.Request) []launchRequestDTO {
	out := make([]launchRequestDTO, 0, len(rs))
	for _, x := range rs {
		out = append(out, toLaunchRequestDTO(x))
	}
	return out
}

// newLaunchQueueHandler is the platform-wide approval queue (platform staff only).
func newLaunchQueueHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, "queue", false)
		if !ok {
			return
		}
		statuses, sok := parseLaunchStatuses(r.URL.Query().Get("status"))
		limit, lok := parseLaunchLimit(r.URL.Query().Get("limit"))
		if !sok || !lok {
			c.badRequest(w)
			return
		}
		rs, err := deps.LaunchAuthorisation.ListRequestsAcrossTenants(r.Context(), c.actor, statuses, limit)
		if err != nil {
			writeLaunchError(w, c, "queue", err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			launchEnvelope
			Items []launchRequestDTO `json:"items"`
		}{launchEnvelope{launchgov.Disclaimer}, toLaunchRequestDTOs(rs)})
	}
}

func newLaunchGetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, "get", true)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			c.badRequest(w)
			return
		}
		req, apps, err := deps.LaunchAuthorisation.GetRequest(r.Context(), c.actor, c.target, id)
		if err != nil {
			writeLaunchError(w, c, "get", err)
			return
		}
		d := toLaunchRequestDTO(*req)
		for _, ap := range apps {
			d.Approvals = append(d.Approvals, launchApprovalDTO{
				ID: ap.ID.String(), Decision: ap.Decision, DecidedBy: ap.DecidedBy.String(), ReasonCode: ap.ReasonCode,
				ReadinessHashAtDecision: ap.ReadinessHashAtDec, DecidedAt: ap.DecidedAt.UTC().Format(time.RFC3339),
			})
		}
		out := struct {
			launchEnvelope
			Request launchRequestDTO `json:"request"`
			// Readiness is the CURRENT server-side evaluation of a pending request's inputs (never
			// client-supplied): its readiness_hash is what an approver passes as seen_readiness_hash.
			Readiness *launchReadinessDTO `json:"readiness,omitempty"`
		}{launchEnvelope: launchEnvelope{launchgov.Disclaimer}, Request: d}
		if req.Status == launchgov.ReqPending {
			rd, err := deps.LaunchAuthorisation.ReadReadiness(r.Context(), c.actor, launchgov.EvalInputOf(*req))
			if err != nil {
				writeLaunchError(w, c, "get", err)
				return
			}
			dto := toLaunchReadinessDTO(rd)
			out.Readiness = &dto
		}
		writeJSON(w, http.StatusOK, out)
	}
}

type launchDecideBody struct {
	TenantID   *string `json:"tenant_id,omitempty"` // accepted and IGNORED
	ReasonCode string  `json:"reason_code"`
	// SeenReadinessHash is the readiness_hash the approver was shown (required to approve).
	SeenReadinessHash string `json:"seen_readiness_hash,omitempty"`
}

type launchDecisionResponse struct {
	launchEnvelope
	Request      launchRequestDTO       `json:"request"`
	Decision     string                 `json:"decision"`
	Executed     bool                   `json:"executed"`
	Readiness    launchReadinessDTO     `json:"readiness"`
	PriorRejects []launchPriorRejectDTO `json:"prior_rejects"`
}

func newLaunchDecideHandler(deps Deps, decision string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, decision, true)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			c.badRequest(w)
			return
		}
		var body launchDecideBody
		if err := decodeJSON(r, &body); err != nil {
			c.badRequest(w)
			return
		}
		res, err := deps.LaunchAuthorisation.Decide(r.Context(), c.actor, c.target, id, launchgov.DecideInput{
			Decision: decision, ReasonCode: body.ReasonCode, SeenReadinessHash: body.SeenReadinessHash,
		})
		if err != nil {
			writeLaunchError(w, c, decision, err)
			return
		}
		writeJSON(w, http.StatusOK, launchDecisionResponse{
			launchEnvelope: launchEnvelope{launchgov.Disclaimer},
			Request:        toLaunchRequestDTO(res.Request), Decision: res.Decision, Executed: res.Executed,
			Readiness: toLaunchReadinessDTO(res.Readiness), PriorRejects: toLaunchPriorRejects(res.PriorRejects),
		})
	}
}

type launchCancelBody struct {
	TenantID   *string `json:"tenant_id,omitempty"` // accepted and IGNORED
	ReasonCode string  `json:"reason_code"`
}

func newLaunchCancelHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, "cancel", true)
		if !ok {
			return
		}
		id, err := uuid.Parse(r.PathValue("requestID"))
		if err != nil {
			c.badRequest(w)
			return
		}
		var body launchCancelBody
		if err := decodeJSON(r, &body); err != nil {
			c.badRequest(w)
			return
		}
		// The cancel is bound to the request's own subject by the service (tenant-bound lock); the
		// Subject passed here only carries the validated tenant (a tenant actor may cancel its
		// brand requests; a tenant-subject request is the platform's own).
		req, err := deps.LaunchAuthorisation.CancelByID(r.Context(), c.actor, c.target, id, body.ReasonCode)
		if err != nil {
			writeLaunchError(w, c, "cancel", err)
			return
		}
		writeJSON(w, http.StatusOK, struct {
			launchEnvelope
			Request launchRequestDTO `json:"request"`
		}{launchEnvelope{launchgov.Disclaimer}, toLaunchRequestDTO(*req)})
	}
}

type launchSuspendBody struct {
	TenantID       *string `json:"tenant_id,omitempty"` // accepted and IGNORED
	BrandID        *string `json:"brand_id,omitempty"`  // accepted and IGNORED: the brand is the path value
	ReasonCode     string  `json:"reason_code"`
	ConditionsNote string  `json:"conditions_note,omitempty"`
}

func newLaunchSuspendHandler(deps Deps, brand bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, "suspend", true)
		if !ok {
			return
		}
		sub := launchgov.Subject{TenantID: c.target}
		if brand {
			b, err := uuid.Parse(r.PathValue("brandID"))
			if err != nil {
				c.badRequest(w)
				return
			}
			sub.BrandID = &b
		}
		var body launchSuspendBody
		if err := decodeJSON(r, &body); err != nil {
			c.badRequest(w)
			return
		}
		res, err := deps.LaunchAuthorisation.Suspend(r.Context(), c.actor, launchgov.SuspendInput{Subject: sub, ReasonCode: body.ReasonCode, ConditionsNote: body.ConditionsNote})
		if err != nil {
			writeLaunchError(w, c, "suspend", err)
			return
		}
		writeJSON(w, http.StatusOK, toLaunchResult(res))
	}
}

type launchTransitionDTO struct {
	ID          string   `json:"id"`
	SubjectKind string   `json:"subject_kind"`
	BrandID     *string  `json:"brand_id,omitempty"`
	Kind        string   `json:"kind"`
	FromStatus  *string  `json:"from_status,omitempty"`
	ToStatus    string   `json:"to_status"`
	RequestID   *string  `json:"request_id,omitempty"`
	ExecutedBy  *string  `json:"executed_by,omitempty"`
	ApproverIDs []string `json:"approver_ids"`
	ActorType   string   `json:"actor_type"`
	CreatedAt   string   `json:"created_at"`
}

func newLaunchHistoryHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, ok := beginLaunchCall(deps, w, r, "history", true)
		if !ok {
			return
		}
		brandID, err := optUUID(r.URL.Query().Get("brand_id"))
		limit, lok := parseLaunchLimit(r.URL.Query().Get("limit"))
		if err != nil || !lok {
			c.badRequest(w)
			return
		}
		ts, err := deps.LaunchAuthorisation.ListTransitions(r.Context(), c.actor, c.target, brandID, limit)
		if err != nil {
			writeLaunchError(w, c, "history", err)
			return
		}
		items := make([]launchTransitionDTO, 0, len(ts))
		for _, t := range ts {
			d := launchTransitionDTO{
				ID: t.ID.String(), SubjectKind: t.SubjectKind, BrandID: strp(t.BrandID), Kind: t.Kind, FromStatus: t.FromStatus,
				ToStatus: t.ToStatus, RequestID: strp(t.RequestID), ExecutedBy: strp(t.ExecutedBy), ApproverIDs: []string{},
				ActorType: t.ActorType, CreatedAt: t.CreatedAt.UTC().Format(time.RFC3339),
			}
			for _, a := range t.ApproverIDs {
				d.ApproverIDs = append(d.ApproverIDs, a.String())
			}
			items = append(items, d)
		}
		writeJSON(w, http.StatusOK, struct {
			launchEnvelope
			Items []launchTransitionDTO `json:"items"`
		}{launchEnvelope{launchgov.Disclaimer}, items})
	}
}
