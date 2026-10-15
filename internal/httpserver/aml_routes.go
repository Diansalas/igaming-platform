// ADR 0116 s6 (A7, S6 minimal): the AML officer HTTP routes. IMPLEMENTED against the local database; all screening and
// monitoring inputs are MOCK or NOT IMPLEMENTED (see docs/plans/a7-aml-record.md). This is software capability only: nothing
// here is a regulatory approval or a statement that any licence condition is met.
//
// Shape (the ledger_adjustment / launch pattern):
//
//   - Every route is registered through staffRoute(deps, class, inner) and listed in staffRouteGolden. Step-up classes follow
//     ADR 0119 s8.1: reads none; placing a hold, opening a case, a transition, a note and a SAR flag only tighten or record
//     (protective) so none; a hold LIFT (loosens a control) and both halves of a four-eyes action (propose / approve) are
//     critical. The signer backstop (actorproof.StepUpOperations) mirrors exactly the same three operations.
//   - The target tenant is the {tenantID} path value ONLY. A tenant-scoped token naming another tenant is 404 (no existence
//     oracle). A platform token acts in the named tenant through the ADR 0099 acting session, which the database refuses
//     unless the principal holds an in-force grant for exactly that tenant. A body or query tenant id does not exist.
//   - The permissions below are only the static route gate. The authority is the in-force aml_officer grant, the signed actor
//     proof and the officer quorum, all re-checked in SQL (migration 0140 aml_authorize): a role alone never reaches AML data.
//   - AML responses carry Cache-Control: no-store, and every error body is a fixed sentence plus a closed check code. Nothing
//     here is reachable by a player token (staffRoute refuses it before any work), and nothing here writes audit_log or any
//     tenant-readable table: the AML trail is the AML-scoped event tables (ADR 0116 s5.2). The player-facing generic cover
//     (account_review / the adjustment `conflict`, ADR 0116 F4 option (b)) is untouched.
//
// NOT IMPLEMENTED here, by design: rejecting a four-eyes proposal (the service has no reject operation: a proposal is
// withdrawn by any later case event, which invalidates it), raising an alert, the SAR export and rule/screening
// configuration routes.
package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// AMLOfficerAPI is the officer surface the routes need (satisfied by *aml.Service). It is an interface so the route layer is
// unit-tested without a database; production wiring passes the real service.
type AMLOfficerAPI interface {
	PlaceHold(ctx context.Context, a aml.Actor, in aml.PlaceHoldInput) (uuid.UUID, error)
	LiftHold(ctx context.Context, a aml.Actor, holdID uuid.UUID, reasonCode string) (uuid.UUID, error)
	ListHolds(ctx context.Context, a aml.Actor, personID uuid.UUID) ([]aml.Hold, error)
	OpenCase(ctx context.Context, a aml.Actor, personID uuid.UUID, kind aml.CaseKind, reasonCode string, alertIDs []uuid.UUID) (uuid.UUID, error)
	ReadCase(ctx context.Context, a aml.Actor, caseID uuid.UUID) (aml.Case, error)
	CaseEvents(ctx context.Context, a aml.Actor, caseID uuid.UUID) ([]aml.CaseEvent, error)
	CaseNotes(ctx context.Context, a aml.Actor, caseID uuid.UUID) ([]aml.CaseNote, error)
	CaseQueue(ctx context.Context, a aml.Actor, limit int) ([]aml.QueueItem, error)
	Transition(ctx context.Context, a aml.Actor, caseID uuid.UUID, expectedSeq int, to aml.CaseStatus, reasonCode string) (int, error)
	AddNote(ctx context.Context, a aml.Actor, caseID uuid.UUID, expectedSeq int, body string) (int, error)
	FlagSAR(ctx context.Context, a aml.Actor, caseID uuid.UUID, expectedSeq int, reasonCode string) (int, error)
	Propose(ctx context.Context, a aml.Actor, caseID uuid.UUID, expectedSeq int, in aml.ProposeInput) (aml.Proposal, error)
	Approve(ctx context.Context, a aml.Actor, caseID uuid.UUID, proposalSeq int, contentHash, reasonCode string) (int, error)
}

var _ AMLOfficerAPI = (*aml.Service)(nil)

func registerAMLRoutes(mux *http.ServeMux, deps Deps) {
	// Literal patterns only (the webhook route guard fails closed on any non-literal pattern).
	mux.Handle("GET /v1/admin/tenants/{tenantID}/aml/cases",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseRead)(amlHandler(deps, "case_queue", amlCaseQueue))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/aml/cases/{caseID}",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseRead)(amlHandler(deps, "case_read", amlCaseRead))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/aml/persons/{personID}/holds",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseRead)(amlHandler(deps, "hold_list", amlHoldList))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/aml/holds",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLHoldManage)(amlHandler(deps, "hold_place", amlHoldPlace))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/aml/holds/{holdID}/lift",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermAMLHoldManage)(amlHandler(deps, "hold_lift", amlHoldLift))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/aml/cases",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseWork)(amlHandler(deps, "case_open", amlCaseOpen))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/transition",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseWork)(amlHandler(deps, "case_transition", amlCaseTransition))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/notes",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseWork)(amlHandler(deps, "case_note", amlCaseNote))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/sar-flag",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseWork)(amlHandler(deps, "case_flag_sar", amlCaseFlagSAR))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/proposals",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermAMLCaseWork)(amlHandler(deps, "case_propose", amlCasePropose))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/aml/cases/{caseID}/approvals",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermAMLCaseCloseApprove)(amlHandler(deps, "case_approve", amlCaseApprove))))
}

// amlCall is the per-request context: the officer actor, built from the verified token and the validated path tenant only.
type amlCall struct {
	requestID string
	logger    observabilityLogger
	actor     aml.Actor
	svc       AMLOfficerAPI
	viewer    staffViewer // ADR 0114 H1: who is looking, for staff-identity redaction
	r         *http.Request
	w         http.ResponseWriter
}

type amlFunc func(c amlCall)

// amlHandler wraps one AML operation: no-store, service availability, authenticated staff context, tenant scoping. It is
// the ONLY place an aml.Actor is built, so no handler can take an actor, scope or tenant from a body.
func amlHandler(deps Deps, op string, fn amlFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)
		if deps.AML == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "AML officer functions are not available in this process")
			return
		}
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		actor, ok := amlActorFrom(tc, r, requestID)
		if !ok {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		target, err := uuid.Parse(r.PathValue("tenantID"))
		if err != nil || target == uuid.Nil {
			apierror.Write(w, requestID, apierror.CodeInvalidRequest, "invalid request")
			return
		}
		if !canActOnTenant(tc, target) {
			// A foreign tenant is indistinguishable from an unknown one (no existence oracle).
			logger.Warn("aml_denied", "op", op, "reason", "foreign_tenant")
			apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
			return
		}
		if tc.TenantID == uuid.Nil {
			actor.Scope, actor.TenantID = aml.ScopePlatformActing, target
		}
		fn(amlCall{requestID: requestID, logger: logger, actor: actor, svc: deps.AML, viewer: staffViewerOf(tc), r: r, w: w})
	})
}

// amlActorFrom builds the actor from the verified token only: a tenant-scoped token acts in its own tenant, a platform token
// (no tenant) acts as platform_acting in the validated path tenant (set by amlHandler after canActOnTenant).
func amlActorFrom(tc tenant.Context, r *http.Request, requestID string) (aml.Actor, bool) {
	staffID, err := uuid.Parse(tc.Subject)
	if err != nil || staffID == uuid.Nil {
		return aml.Actor{}, false
	}
	return aml.Actor{StaffID: staffID, Scope: aml.ScopeTenant, TenantID: tc.TenantID, IPAddress: clientIP(r), RequestID: requestID}, true
}

// ---- error mapping -------------------------------------------------------------------------------------------------------

// amlErrorCode maps an aml error to the closed API code, a fixed sentence and one closed check code. Pure, so the whole table
// is unit-tested without a database. The database message text is never copied into a response.
func amlErrorCode(err error) (apierror.Code, string, []string) {
	switch {
	case errors.Is(err, aml.ErrNotFound):
		return apierror.CodeNotFound, "not found", nil
	case errors.Is(err, aml.ErrForbidden), errors.Is(err, aml.ErrActorScope):
		return apierror.CodeForbidden, "insufficient permissions for this operation", nil
	case errors.Is(err, aml.ErrProofUnavailable):
		return apierror.CodeUnavailable, "AML officer functions are not available in this process", nil
	case errors.Is(err, aml.ErrInvalidInput):
		return apierror.CodeValidation, "invalid request", nil
	case errors.Is(err, aml.ErrNoteRefused):
		return apierror.CodeValidation, "the note was refused", []string{"note_refused"}
	case errors.Is(err, aml.ErrQuorum):
		return apierror.CodeConflict, "refused", []string{"quorum_not_met"}
	case errors.Is(err, aml.ErrHoldRefused):
		return apierror.CodeConflict, "refused", []string{"hold_refused"}
	case errors.Is(err, aml.ErrFourEyes):
		return apierror.CodeConflict, "refused", []string{"four_eyes_refused"}
	case errors.Is(err, aml.ErrStale):
		return apierror.CodeConflict, "refused", []string{"stale"}
	case errors.Is(err, aml.ErrIllegalTransition):
		return apierror.CodeConflict, "refused", []string{"illegal_transition"}
	}
	if code := aml.GuardCode(err); code != "" {
		if strings.HasPrefix(code, "AP") {
			return apierror.CodeForbidden, "the signed actor proof was refused", nil
		}
		return apierror.CodeConflict, "refused", []string{"guard_refused"}
	}
	return apierror.CodeInternal, "internal error", nil
}

func (c amlCall) fail(op string, err error) {
	code, msg, checks := amlErrorCode(err)
	switch code {
	case apierror.CodeInternal:
		// Only the error class goes to the log; the body is a fixed sentence.
		c.logger.Error("aml_failed", "op", op, "kind", errorKind(err))
	case apierror.CodeNotFound, apierror.CodeValidation:
	default:
		c.logger.Warn("aml_refused", "op", op, "code", string(code), "guard", aml.GuardCode(err))
	}
	apierror.WriteWithCheckCodes(c.w, c.requestID, code, msg, checks)
}

func errorKind(err error) string {
	if err == nil {
		return ""
	}
	if ge := aml.GuardCode(err); ge != "" {
		return ge
	}
	return "unclassified"
}

func (c amlCall) badRequest() {
	apierror.Write(c.w, c.requestID, apierror.CodeValidation, "invalid request")
}

func (c amlCall) pathUUID(name string) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.r.PathValue(name))
	if err != nil || id == uuid.Nil {
		c.badRequest()
		return uuid.Nil, false
	}
	return id, true
}

func (c amlCall) decode(dst any) bool {
	if err := decodeJSON(c.r, dst); err != nil {
		c.badRequest()
		return false
	}
	return true
}

// ---- DTOs ----------------------------------------------------------------------------------------------------------------

type amlHoldDTO struct {
	ID              string  `json:"id"`
	OwnerScope      string  `json:"owner_scope"`
	PlayerAccountID *string `json:"player_account_id,omitempty"`
	Scope           string  `json:"scope"`
	ReasonClass     string  `json:"reason_class"`
	ReasonCode      string  `json:"reason_code"`
	PlacedAt        string  `json:"placed_at"`
	Lifted          bool    `json:"lifted"`
	LiftedAt        *string `json:"lifted_at,omitempty"`
}

func amlTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func toAMLHoldDTO(h aml.Hold) amlHoldDTO {
	d := amlHoldDTO{ID: h.ID.String(), OwnerScope: h.OwnerScope, Scope: string(h.Scope), ReasonClass: string(h.ReasonClass),
		ReasonCode: h.ReasonCode, PlacedAt: amlTime(h.PlacedAt), Lifted: h.Lifted}
	if h.PlayerAccountID != nil {
		s := h.PlayerAccountID.String()
		d.PlayerAccountID = &s
	}
	if h.LiftedAt != nil {
		s := amlTime(*h.LiftedAt)
		d.LiftedAt = &s
	}
	return d
}

type amlCaseDTO struct {
	ID         string `json:"id"`
	OwnerScope string `json:"owner_scope"`
	PersonID   string `json:"person_id"`
	Kind       string `json:"kind"`
	Status     string `json:"status"`
	SARFlagged bool   `json:"sar_flagged"`
	// LastSeq is the value every state-changing call must present as expected_seq (compare-and-set).
	LastSeq  int    `json:"last_seq"`
	OpenedAt string `json:"opened_at"`
}

func toAMLCaseDTO(c aml.Case) amlCaseDTO {
	return amlCaseDTO{ID: c.ID.String(), OwnerScope: c.OwnerScope, PersonID: c.PersonID.String(), Kind: string(c.Kind),
		Status: string(c.Status), SARFlagged: c.SARFlagged, LastSeq: c.LastSeq, OpenedAt: amlTime(c.OpenedAt)}
}

type amlQueueItemDTO struct {
	CaseID     string `json:"case_id"`
	OwnerScope string `json:"owner_scope"`
	PersonID   string `json:"person_id"`
	Status     string `json:"status"`
	SARFlagged bool   `json:"sar_flagged"`
	LastSeq    int    `json:"last_seq"`
	OpenedAt   string `json:"opened_at"`
}

type amlEventDTO struct {
	Seq                     int     `json:"seq"`
	Type                    string  `json:"type"`
	FromStatus              *string `json:"from_status,omitempty"`
	ToStatus                *string `json:"to_status,omitempty"`
	ProposalKind            *string `json:"proposal_kind,omitempty"`
	ProposalSeq             *int    `json:"proposal_seq,omitempty"`
	ContentHash             *string `json:"content_hash,omitempty"`
	SubjectHoldID           *string `json:"subject_hold_id,omitempty"`
	NoteID                  *string `json:"note_id,omitempty"`
	ExternalFilingReference *string `json:"external_filing_reference,omitempty"`
	ReasonCode              string  `json:"reason_code"`
	// ActorID is the acting officer's staff id, redacted by the ADR 0114 H1 helper: a tenant viewer sees only tenant-scope actors.
	ActorID    staffRef `json:"actor_id"`
	ActorScope string   `json:"actor_scope"`
	CreatedAt  string   `json:"created_at"`
}

func toAMLEventDTO(v staffViewer, e aml.CaseEvent) amlEventDTO {
	d := amlEventDTO{Seq: e.Seq, Type: e.Type, FromStatus: e.FromStatus, ToStatus: e.ToStatus, ProposalKind: e.ProposalKind,
		ProposalSeq: e.ProposalSeq, ContentHash: e.ContentHash, ExternalFilingReference: e.ExternalFilingReference,
		ReasonCode: e.ReasonCode, ActorID: v.ref(e.ActorScope, e.ActorID), ActorScope: e.ActorScope, CreatedAt: amlTime(e.CreatedAt)}
	if e.SubjectHoldID != nil {
		s := e.SubjectHoldID.String()
		d.SubjectHoldID = &s
	}
	if e.NoteID != nil {
		s := e.NoteID.String()
		d.NoteID = &s
	}
	return d
}

type amlNoteDTO struct {
	ID          string `json:"id"`
	Body        string `json:"body"`
	LintVersion string `json:"lint_version"`
	// CreatedBy is redacted like the events' actor (ADR 0114 H1).
	CreatedBy staffRef `json:"created_by"`
	CreatedAt string   `json:"created_at"`
}

// ---- handlers ------------------------------------------------------------------------------------------------------------

func amlCaseQueue(c amlCall) {
	limit := 50
	if v := c.r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			c.badRequest()
			return
		}
		limit = n
	}
	items, err := c.svc.CaseQueue(c.r.Context(), c.actor, limit)
	if err != nil {
		c.fail("case_queue", err)
		return
	}
	out := make([]amlQueueItemDTO, 0, len(items))
	for _, q := range items {
		out = append(out, amlQueueItemDTO{CaseID: q.CaseID.String(), OwnerScope: q.OwnerScope, PersonID: q.PersonID.String(),
			Status: string(q.Status), SARFlagged: q.SARFlagged, LastSeq: q.LastSeq, OpenedAt: amlTime(q.OpenedAt)})
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"items": out})
}

// amlCaseRead returns the case, its append-only events and its notes. The three reads are three separate proof-bound
// officer transactions (each re-authorised); a case that moves between them is visible as a larger last_seq than the events.
func amlCaseRead(c amlCall) {
	id, ok := c.pathUUID("caseID")
	if !ok {
		return
	}
	cs, err := c.svc.ReadCase(c.r.Context(), c.actor, id)
	if err != nil {
		c.fail("case_read", err)
		return
	}
	evs, err := c.svc.CaseEvents(c.r.Context(), c.actor, id)
	if err != nil {
		c.fail("case_read", err)
		return
	}
	notes, err := c.svc.CaseNotes(c.r.Context(), c.actor, id)
	if err != nil {
		c.fail("case_read", err)
		return
	}
	events := make([]amlEventDTO, 0, len(evs))
	for _, e := range evs {
		events = append(events, toAMLEventDTO(c.viewer, e))
	}
	ns := make([]amlNoteDTO, 0, len(notes))
	for _, n := range notes {
		// A note row carries no scope column; its author's scope is the actor_scope of the case event that recorded it (the event
		// names the note id). No matching event means the scope is unknown, and unknown is hidden from a tenant viewer.
		noteScope := ""
		for _, e := range evs {
			if e.NoteID != nil && *e.NoteID == n.ID {
				noteScope = e.ActorScope
				if noteScope == aml.ScopePlatformActing {
					noteScope = staffScopePlatform
				}
			}
		}
		ns = append(ns, amlNoteDTO{ID: n.ID.String(), Body: n.Body, LintVersion: n.LintVersion, CreatedBy: c.viewer.ref(noteScope, n.CreatedBy), CreatedAt: amlTime(n.CreatedAt)})
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"case": toAMLCaseDTO(cs), "events": events, "notes": ns})
}

func amlHoldList(c amlCall) {
	id, ok := c.pathUUID("personID")
	if !ok {
		return
	}
	hs, err := c.svc.ListHolds(c.r.Context(), c.actor, id)
	if err != nil {
		c.fail("hold_list", err)
		return
	}
	out := make([]amlHoldDTO, 0, len(hs))
	for _, h := range hs {
		out = append(out, toAMLHoldDTO(h))
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"items": out})
}

type amlHoldPlaceRequest struct {
	PersonID        string  `json:"person_id"`
	PlayerAccountID *string `json:"player_account_id"`
	Scope           string  `json:"scope"`
	ReasonClass     string  `json:"reason_class"`
	ReasonCode      string  `json:"reason_code"`
}

func amlHoldPlace(c amlCall) {
	var req amlHoldPlaceRequest
	if !c.decode(&req) {
		return
	}
	person, err := uuid.Parse(req.PersonID)
	if err != nil || person == uuid.Nil {
		c.badRequest()
		return
	}
	in := aml.PlaceHoldInput{PersonID: person, Scope: aml.HoldScope(req.Scope), ReasonClass: aml.ReasonClass(req.ReasonClass), ReasonCode: req.ReasonCode}
	if req.PlayerAccountID != nil {
		acct, err := uuid.Parse(*req.PlayerAccountID)
		if err != nil || acct == uuid.Nil {
			c.badRequest()
			return
		}
		in.PlayerAccountID = &acct
	}
	id, err := c.svc.PlaceHold(c.r.Context(), c.actor, in)
	if err != nil {
		c.fail("hold_place", err)
		return
	}
	writeJSON(c.w, http.StatusCreated, map[string]any{"hold_id": id.String()})
}

type amlReasonRequest struct {
	ReasonCode string `json:"reason_code"`
}

func amlHoldLift(c amlCall) {
	id, ok := c.pathUUID("holdID")
	if !ok {
		return
	}
	var req amlReasonRequest
	if !c.decode(&req) {
		return
	}
	liftID, err := c.svc.LiftHold(c.r.Context(), c.actor, id, req.ReasonCode)
	if err != nil {
		c.fail("hold_lift", err)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"lift_id": liftID.String()})
}

type amlCaseOpenRequest struct {
	PersonID   string   `json:"person_id"`
	Kind       string   `json:"kind"`
	ReasonCode string   `json:"reason_code"`
	AlertIDs   []string `json:"alert_ids"`
}

func amlCaseOpen(c amlCall) {
	var req amlCaseOpenRequest
	if !c.decode(&req) {
		return
	}
	person, err := uuid.Parse(req.PersonID)
	if err != nil || person == uuid.Nil || len(req.AlertIDs) > 100 {
		c.badRequest()
		return
	}
	alerts := make([]uuid.UUID, 0, len(req.AlertIDs))
	for _, s := range req.AlertIDs {
		a, err := uuid.Parse(s)
		if err != nil || a == uuid.Nil {
			c.badRequest()
			return
		}
		alerts = append(alerts, a)
	}
	id, err := c.svc.OpenCase(c.r.Context(), c.actor, person, aml.CaseKind(req.Kind), req.ReasonCode, alerts)
	if err != nil {
		c.fail("case_open", err)
		return
	}
	writeJSON(c.w, http.StatusCreated, map[string]any{"case_id": id.String()})
}

type amlTransitionRequest struct {
	ExpectedSeq int    `json:"expected_seq"`
	To          string `json:"to"`
	ReasonCode  string `json:"reason_code"`
}

func amlCaseTransition(c amlCall) {
	id, ok := c.pathUUID("caseID")
	if !ok {
		return
	}
	var req amlTransitionRequest
	if !c.decode(&req) {
		return
	}
	seq, err := c.svc.Transition(c.r.Context(), c.actor, id, req.ExpectedSeq, aml.CaseStatus(req.To), req.ReasonCode)
	if err != nil {
		c.fail("case_transition", err)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"last_seq": seq})
}

type amlNoteRequest struct {
	ExpectedSeq int    `json:"expected_seq"`
	Body        string `json:"body"`
}

func amlCaseNote(c amlCall) {
	id, ok := c.pathUUID("caseID")
	if !ok {
		return
	}
	var req amlNoteRequest
	if !c.decode(&req) {
		return
	}
	seq, err := c.svc.AddNote(c.r.Context(), c.actor, id, req.ExpectedSeq, req.Body)
	if err != nil {
		c.fail("case_note", err)
		return
	}
	writeJSON(c.w, http.StatusCreated, map[string]any{"last_seq": seq})
}

type amlSeqReasonRequest struct {
	ExpectedSeq int    `json:"expected_seq"`
	ReasonCode  string `json:"reason_code"`
}

func amlCaseFlagSAR(c amlCall) {
	id, ok := c.pathUUID("caseID")
	if !ok {
		return
	}
	var req amlSeqReasonRequest
	if !c.decode(&req) {
		return
	}
	seq, err := c.svc.FlagSAR(c.r.Context(), c.actor, id, req.ExpectedSeq, req.ReasonCode)
	if err != nil {
		c.fail("case_flag_sar", err)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"last_seq": seq})
}

type amlProposeRequest struct {
	ExpectedSeq             int    `json:"expected_seq"`
	Kind                    string `json:"kind"`
	HoldID                  string `json:"hold_id"`
	ExternalFilingReference string `json:"external_filing_reference"`
	ReasonCode              string `json:"reason_code"`
}

func amlCasePropose(c amlCall) {
	id, ok := c.pathUUID("caseID")
	if !ok {
		return
	}
	var req amlProposeRequest
	if !c.decode(&req) {
		return
	}
	in := aml.ProposeInput{Kind: aml.ProposalKind(req.Kind), ExternalFilingReference: req.ExternalFilingReference, ReasonCode: req.ReasonCode}
	if req.HoldID != "" {
		h, err := uuid.Parse(req.HoldID)
		if err != nil || h == uuid.Nil {
			c.badRequest()
			return
		}
		in.HoldID = h
	}
	p, err := c.svc.Propose(c.r.Context(), c.actor, id, req.ExpectedSeq, in)
	if err != nil {
		c.fail("case_propose", err)
		return
	}
	writeJSON(c.w, http.StatusCreated, map[string]any{"proposal_seq": p.Seq, "content_hash": p.ContentHash})
}

type amlApproveRequest struct {
	ProposalSeq int    `json:"proposal_seq"`
	ContentHash string `json:"content_hash"`
	ReasonCode  string `json:"reason_code"`
}

func amlCaseApprove(c amlCall) {
	id, ok := c.pathUUID("caseID")
	if !ok {
		return
	}
	var req amlApproveRequest
	if !c.decode(&req) {
		return
	}
	seq, err := c.svc.Approve(c.r.Context(), c.actor, id, req.ProposalSeq, req.ContentHash, req.ReasonCode)
	if err != nil {
		c.fail("case_approve", err)
		return
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"last_seq": seq})
}
