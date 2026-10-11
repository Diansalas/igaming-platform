package launchgov

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Audit actions (ADR 0112 8.1): one append-only audit_log row per event. A platform-scope row
// carries subject_tenant_id (ADR 0104), so the existing tenant read path shows it; a tenant-scope
// session's row carries its own tenant_id.
const (
	auditPrefix = "launch_authorisation."

	// eventSuspended is the audit event of an executed suspension (a constant, so the reviewed
	// status-reader files carry no suspended/closed literal in code).
	eventSuspended = "suspended"

	// AuditCloseRefusedOpenRounds is the separate-transaction refusal row of a closure blocked by
	// open sportsbook bets (GP020). It keeps the name and the counts-only shape of the former
	// tenant.ChangeStatus refusal audit, which the closure tests assert.
	AuditCloseRefusedOpenRounds = "tenant.status_change_refused_open_rounds"
)

func (s *Service) auditEntry(a Actor, action string, tenantID uuid.UUID, outcome audit.Outcome, targetType, targetID string, md map[string]any) audit.Entry {
	e := audit.Entry{
		ActorType: audit.ActorStaff, ActorID: a.StaffID, Action: action,
		TargetType: targetType, TargetID: targetID, Outcome: outcome,
		IPAddress: a.IPAddress, RequestID: a.RequestID, Metadata: md,
	}
	if a.Scope == ScopeTenant {
		e.TenantID = a.TenantID
	} else {
		e.SubjectTenantID = tenantID
	}
	return e
}

func requestMeta(r Request) map[string]any {
	md := map[string]any{
		"request_id": r.ID.String(), "subject_kind": r.SubjectKind, "action": r.Action,
		"from_status": r.FromStatus, "to_status": r.ToStatus, "reason_code": r.ReasonCode,
		"required_approvals": r.RequiredApprovals, "tenant_id": r.TenantID.String(),
		"requested_by": r.RequestedBy.String(), "requested_by_scope": r.RequestedByScope,
	}
	if r.BrandID != nil {
		md["brand_id"] = r.BrandID.String()
	}
	if r.LicensingModel != nil {
		md["licensing_model"] = *r.LicensingModel
	}
	if r.LicensingStatus != nil {
		md["licensing_status"] = *r.LicensingStatus
	}
	return md
}

// auditEvent writes the success row of an event inside the action's own transaction, so the two
// commit or roll back together. Codes and ids only; free text (conditions_note) is never copied.
func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, a Actor, event string, r Request, extra map[string]any) error {
	md := requestMeta(r)
	for k, v := range extra {
		md[k] = v
	}
	return audit.Record(ctx, tx, s.auditEntry(a, auditPrefix+event, r.TenantID, audit.OutcomeSuccess, "launch_authorisation_request", r.ID.String(), md))
}

// denialCode is the closed, machine-readable code of a refusal (never free text).
func denialCode(err error) string {
	var blocked *tenant.CloseBlockedError
	var ge *GuardError
	switch {
	case errors.As(err, &blocked):
		return tenant.CodeCloseBlockedOpenRounds
	case errors.Is(err, ErrUnacknowledged):
		return "LAUNCH_UNACKNOWLEDGED_CHECKS"
	case errors.Is(err, ErrWithdrawalsHolding):
		return "LAUNCH_WITHDRAWALS_HOLDING_FUNDS"
	case errors.Is(err, ErrPreconditionFailed):
		return "LAUNCH_PRECONDITION_FAILED"
	case errors.Is(err, ErrRequestPending):
		return "LAUNCH_REQUEST_PENDING"
	case errors.Is(err, ErrSelfApproval):
		return "LAUNCH_SELF_APPROVAL"
	case errors.Is(err, ErrIllegalTransition):
		return "LAUNCH_ILLEGAL_TRANSITION"
	case errors.Is(err, ErrSubjectClosed):
		return "LAUNCH_SUBJECT_CLOSED"
	case errors.Is(err, ErrRetryableLockTimeout):
		return "LAUNCH_RETRYABLE_LOCK_TIMEOUT"
	case errors.Is(err, ErrPriorReject):
		return "LAUNCH_PRIOR_REJECT"
	case errors.Is(err, ErrPriorRejecter):
		return "LAUNCH_PRIOR_REJECTER"
	case errors.Is(err, ErrNotLegacyBaseline):
		return "LAUNCH_NOT_LEGACY_BASELINE"
	case errors.Is(err, ErrStaleReadiness):
		return "LAUNCH_STALE_READINESS"
	case errors.Is(err, ErrForbidden):
		return "FORBIDDEN"
	case errors.As(err, &ge):
		return "SQLSTATE_" + ge.Code
	}
	return "REFUSED"
}

type denyCfg struct {
	requestID *uuid.UUID
	reason    string
	extra     map[string]any
}

type denyOpt func(*denyCfg)

// withRequest names the request a refusal concerns and its reason code (the GP020 refusal row
// carries the reason code, as the former ChangeStatus audit did).
func withRequest(id uuid.UUID, reason string) denyOpt {
	return func(c *denyCfg) { c.requestID, c.reason = &id, reason }
}

func withExtra(m map[string]any) denyOpt { return func(c *denyCfg) { c.extra = m } }

// denied audits a refusal that rolled back, ONCE per call, in a separate transaction (the
// refused transaction's own writes are gone), with codes and counts only, and returns the
// original error. Validation errors, unknown ids and not-pending are not audited (nothing was
// attempted that a reviewer needs to see; a probing actor still leaves the route's request log).
func (s *Service) denied(ctx context.Context, a Actor, op string, sub Subject, action string, cause error, opts ...denyOpt) error {
	if cause == nil {
		return nil
	}
	if errors.Is(cause, ErrInvalidInput) || errors.Is(cause, ErrNotFound) || errors.Is(cause, ErrNotPending) ||
		errors.Is(cause, ErrActingSession) || errors.Is(cause, ErrIsolationLevel) || errors.Is(cause, ErrProofUnavailable) {
		return cause
	}
	var cfg denyCfg
	for _, o := range opts {
		o(&cfg)
	}
	md := map[string]any{"code": denialCode(cause), "operation": op, "subject_kind": sub.Kind(), "tenant_id": sub.TenantID.String()}
	if sub.BrandID != nil {
		md["brand_id"] = sub.BrandID.String()
	}
	if action != "" {
		md["action"] = action
	}
	for k, v := range cfg.extra {
		md[k] = v
	}
	if cfg.requestID != nil {
		md["request_id"] = cfg.requestID.String()
	}
	var pre *PreconditionError
	if errors.As(cause, &pre) {
		md["check_codes"] = pre.Codes
	}
	var prior *PriorRejectError
	if errors.As(cause, &prior) {
		md["prior_request_id"] = prior.RequestID.String()
	}
	if g := GuardCode(cause); g != "" {
		md["sqlstate"] = g
	}
	target, targetID := "launch_authorisation_request", ""
	if cfg.requestID != nil {
		targetID = cfg.requestID.String()
	} else {
		target, targetID = sub.Kind(), sub.TenantID.String()
		if sub.BrandID != nil {
			targetID = sub.BrandID.String()
		}
	}
	entry := s.auditEntry(a, auditPrefix+op+"_denied", sub.TenantID, audit.OutcomeDenied, target, targetID, md)
	var blocked *tenant.CloseBlockedError
	if errors.As(cause, &blocked) {
		// Counts only, under the action name the closure tests assert (the former ChangeStatus audit).
		entry.Action = AuditCloseRefusedOpenRounds
		entry.TargetType, entry.TargetID = "tenant", sub.TenantID.String()
		entry.Metadata = map[string]any{
			"requested_status": StatusClosed, "reason_code": cfg.reason,
			"code": tenant.CodeCloseBlockedOpenRounds, "sportsbook_open_bets": blocked.SportsbookOpenBets,
		}
	}
	aerr := s.runActor(context.WithoutCancel(ctx), a, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, entry)
	})
	if aerr != nil {
		// The refusal stands; the failed audit write is reported with it.
		return &auditFailedError{cause: cause, audit: aerr}
	}
	return cause
}

// auditFailedError wraps a refusal whose audit write failed: errors.Is / As see the cause.
type auditFailedError struct {
	cause error
	audit error
}

func (e *auditFailedError) Error() string {
	return e.cause.Error() + " (audit of the refusal failed: " + e.audit.Error() + ")"
}
func (e *auditFailedError) Unwrap() error { return e.cause }
