package httpserver

// lic18 (ledger-finance H-1 c; migration 0181): the HTTP surface of the automatic AML screening gate.
//
//	GET  /v1/admin/tenants/{tenantID}/screening/trigger-policy?jurisdiction_id=              the policy in force (none)
//	POST /v1/admin/tenants/{tenantID}/screening/trigger-policy                               author the next VERSION (critical)
//	GET  /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests                  a person's automatic screening requests (none)
//	POST /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests/{requestID}/resolution   the officer's resolution (critical)
//
// The paths deliberately do NOT sit under /aml/ (the A7 route pins count exactly the eleven /aml/ officer routes). Authority is the
// AML officer model, unchanged: the static permission is only the route gate; the DATABASE re-checks the in-force aml_officer grant,
// the signed actor proof and the officer quorum (migration 0140 aml_authorize). Authoring the policy is platform_acting only (the
// same rule as the screening policy); resolving a request is the owner-scope officer's, like any officer screening record, and a
// SANCTIONS-class match can only be resolved by recording a match (a hold lifted by four-eyes), never by a clear.
// Both writes are step-up critical: the policy can switch a withdrawal gate off, a resolution can release a held person.

import (
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

func registerAMLScreeningRoutes(mux *http.ServeMux, deps Deps) {
	mux.Handle("GET /v1/admin/tenants/{tenantID}/screening/trigger-policy",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseRead)(amlHandler(deps, "trigger_policy_read", amlTriggerPolicyRead))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/screening/trigger-policy",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermAMLHoldManage)(amlHandler(deps, "trigger_policy_set", amlTriggerPolicySet))))
	mux.Handle("GET /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests",
		staffRoute(deps, stepUpNone, auth.RequirePermission(auth.PermAMLCaseRead)(amlHandler(deps, "screening_requests", amlScreeningRequests))))
	mux.Handle("POST /v1/admin/tenants/{tenantID}/screening/persons/{personID}/requests/{requestID}/resolution",
		staffRoute(deps, stepUpCritical, auth.RequirePermission(auth.PermAMLHoldManage)(amlHandler(deps, "screening_resolve", amlScreeningResolve))))
}

type amlTriggerPolicyDTO struct {
	Configured     bool     `json:"configured"`
	Events         []string `json:"events"`
	GateWithdrawal bool     `json:"gate_withdrawal"`
	// GateAlertAfterSeconds is set iff gate_withdrawal.
	GateAlertAfterSeconds *int64  `json:"gate_alert_after_seconds"`
	EffectiveFrom         *string `json:"effective_from"`
}

func amlTriggerPolicyRead(c amlCall) {
	jur, err := uuid.Parse(c.r.URL.Query().Get("jurisdiction_id"))
	if err != nil || jur == uuid.Nil {
		c.badRequest()
		return
	}
	p, err := c.svc.EffectiveScreeningTriggerPolicy(c.r.Context(), c.actor, jur)
	if err != nil {
		c.fail("trigger_policy_read", err)
		return
	}
	out := amlTriggerPolicyDTO{Events: []string{}}
	if p != nil {
		out.Configured, out.GateWithdrawal = true, p.GateWithdrawal
		for _, e := range p.Events {
			out.Events = append(out.Events, string(e))
		}
		if p.GateWithdrawal {
			n := int64(p.GateAlertAfter / time.Second)
			out.GateAlertAfterSeconds = &n
		}
		t := amlTime(p.EffectiveFrom)
		out.EffectiveFrom = &t
	}
	writeJSON(c.w, http.StatusOK, out)
}

type amlTriggerPolicyRequest struct {
	JurisdictionID        string   `json:"jurisdiction_id"`
	Events                []string `json:"events"`
	GateWithdrawal        bool     `json:"gate_withdrawal"`
	GateAlertAfterSeconds int64    `json:"gate_alert_after_seconds"`
	EffectiveFrom         string   `json:"effective_from"`
	LegalReviewReference  string   `json:"legal_review_reference"`
	ReasonCode            string   `json:"reason_code"`
}

func amlTriggerPolicySet(c amlCall) {
	var req amlTriggerPolicyRequest
	if !c.decode(&req) {
		return
	}
	jur, err := uuid.Parse(req.JurisdictionID)
	if err != nil || jur == uuid.Nil {
		c.badRequest()
		return
	}
	from, err := time.Parse(time.RFC3339, req.EffectiveFrom)
	if err != nil {
		c.badRequest()
		return
	}
	in := aml.SetTriggerPolicyInput{JurisdictionID: jur, GateWithdrawal: req.GateWithdrawal, EffectiveFrom: from,
		LegalReviewReference: req.LegalReviewReference, ReasonCode: req.ReasonCode}
	for _, e := range req.Events {
		in.Events = append(in.Events, aml.TriggerEvent(e))
	}
	if req.GateWithdrawal {
		in.GateAlertAfter = time.Duration(req.GateAlertAfterSeconds) * time.Second
	} else if req.GateAlertAfterSeconds != 0 {
		c.badRequest()
		return
	}
	id, err := c.svc.SetScreeningTriggerPolicy(c.r.Context(), c.actor, in)
	if err != nil {
		c.fail("trigger_policy_set", err)
		return
	}
	writeJSON(c.w, http.StatusCreated, map[string]any{"policy_id": id.String()})
}

type amlScreeningRequestDTO struct {
	ID             string   `json:"id"`
	Trigger        string   `json:"trigger"`
	IdempotencyKey string   `json:"idempotency_key"`
	Status         string   `json:"status"`
	Attempts       int      `json:"attempts"`
	Outcome        string   `json:"outcome,omitempty"`
	MatchLists     []string `json:"match_lists,omitempty"`
	LastErrorCode  string   `json:"last_error_code,omitempty"`
	CompletedAt    *string  `json:"completed_at,omitempty"`
	Resolved       bool     `json:"resolved"`
	CreatedAt      string   `json:"created_at"`
}

func amlScreeningRequests(c amlCall) {
	pid, ok := c.pathUUID("personID")
	if !ok {
		return
	}
	rows, err := c.svc.ListScreeningRequests(c.r.Context(), c.actor, pid)
	if err != nil {
		c.fail("screening_requests", err)
		return
	}
	out := make([]amlScreeningRequestDTO, 0, len(rows))
	for _, r := range rows {
		d := amlScreeningRequestDTO{ID: r.ID.String(), Trigger: string(r.Trigger), IdempotencyKey: r.IdempotencyKey, Status: string(r.Status),
			Attempts: r.Attempts, Outcome: string(r.Outcome), LastErrorCode: string(r.LastErrorCode), Resolved: r.Resolved, CreatedAt: amlTime(r.CreatedAt)}
		for _, l := range r.MatchLists {
			d.MatchLists = append(d.MatchLists, string(l))
		}
		if r.CompletedAt != nil {
			t := amlTime(*r.CompletedAt)
			d.CompletedAt = &t
		}
		out = append(out, d)
	}
	writeJSON(c.w, http.StatusOK, map[string]any{"items": out})
}

type amlScreeningResolveRequest struct {
	Outcome    string   `json:"outcome"`
	Lists      []string `json:"lists"`
	MatchLists []string `json:"match_lists"`
	ReasonCode string   `json:"reason_code"`
}

func amlScreeningResolve(c amlCall) {
	pid, ok := c.pathUUID("personID")
	if !ok {
		return
	}
	rid, ok := c.pathUUID("requestID")
	if !ok {
		return
	}
	var req amlScreeningResolveRequest
	if !c.decode(&req) {
		return
	}
	in := aml.ResolveScreeningInput{Outcome: aml.ScreeningOutcome(req.Outcome), ReasonCode: req.ReasonCode}
	for _, l := range req.Lists {
		in.Lists = append(in.Lists, aml.ListType(l))
	}
	for _, l := range req.MatchLists {
		in.MatchLists = append(in.MatchLists, aml.ListType(l))
	}
	res, err := c.svc.ResolveScreeningRequest(c.r.Context(), c.actor, pid, rid, in)
	if err != nil {
		c.fail("screening_resolve", err)
		return
	}
	out := map[string]any{"result_id": res.ResultID.String(), "replayed": res.Replayed}
	if res.HoldID != nil {
		out["hold_id"] = res.HoldID.String()
	}
	writeJSON(c.w, http.StatusCreated, out)
}
