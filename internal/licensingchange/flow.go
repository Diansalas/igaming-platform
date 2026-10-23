package licensingchange

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// reasonCodeRE is the reason-code format the database checks too (a code, never free text).
var reasonCodeRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,63}$`)

// ValidReasonCode reports whether s is a well-formed reason code.
func ValidReasonCode(s string) bool { return reasonCodeRE.MatchString(s) }

// Request files a change. The database classifies it (widening or narrowing), derives the approvals it needs and binds the
// requester's proof. A narrowing change filed by a platform admin (0 approvals) executes in the SAME transaction.
func (s *Service) Request(ctx context.Context, a Actor, in RequestInput) (*Result, error) {
	if err := validateActor(a); err != nil {
		return nil, err
	}
	if err := requirePerm(a, auth.PermLicensingChangeRequest); err != nil {
		return nil, err
	}
	spec, ok := kinds[in.Kind]
	if !ok {
		return nil, fmt.Errorf("%w: unknown kind %q", ErrInvalidInput, in.Kind)
	}
	if !ValidReasonCode(in.ReasonCode) {
		return nil, fmt.Errorf("%w: reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]", ErrInvalidInput)
	}
	var note *string
	if strings.TrimSpace(in.Note) != "" {
		n, err := optText(&in.Note, 1000)
		if err != nil {
			return nil, err
		}
		note = n
	}
	payload, subject, err := spec.decode(in.Payload)
	if err != nil {
		return nil, err
	}
	switch a.Scope {
	case ScopeTenant:
		if spec.platformOnly {
			return nil, fmt.Errorf("%w: tenant staff may not request %s", ErrForbidden, in.Kind)
		}
		if subject != a.TenantID {
			return nil, ErrNotFound // a foreign tenant is indistinguishable from an unknown one
		}
		perm := spec.tenantPerm(payload)
		if perm == "" {
			return nil, fmt.Errorf("%w: tenant staff may not request %s", ErrForbidden, in.Kind)
		}
		if err := requirePerm(a, perm); err != nil {
			return nil, err
		}
	case ScopePlatform:
		if spec.platformPerm != "" {
			if err := requirePerm(a, spec.platformPerm); err != nil {
				return nil, err
			}
		}
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("licensingchange: encode payload: %w", err)
	}

	var res Result
	err = s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		res = Result{}
		if spec.precheck != nil {
			if err := spec.precheck(ctx, tx, payload); err != nil {
				return err
			}
		}
		var hash string
		if err := tx.QueryRow(ctx, `SELECT licensing_change_payload_hash($1, $2::jsonb, $3)`, string(in.Kind), canonical, in.ReasonCode).Scan(&hash); err != nil {
			return err
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpLicensingChangeRequest, actorproof.TargetNew, hash); err != nil {
			return err
		}
		var req Request
		if err := scanRequest(tx.QueryRow(ctx, `
			INSERT INTO licensing_change_requests (kind, payload, payload_hash, reason_code, note)
			VALUES ($1, $2::jsonb, $3, $4, $5) RETURNING `+requestColumns,
			string(in.Kind), canonical, hash, in.ReasonCode, note), &req); err != nil {
			return err
		}
		if err := s.auditEvent(ctx, tx, a, "requested", req, nil); err != nil {
			return err
		}
		res.Request = req
		if req.RequiredApprovals > 0 {
			return nil
		}
		// Single-actor (narrowing, platform requester): execute now, in this transaction.
		return s.execute(ctx, tx, a, req, nil, &res)
	})
	if err != nil {
		return nil, classify(err)
	}
	return &res, nil
}

// Approve records a platform approver's approval. When it is the final one required, the SAME transaction executes the
// change (row lock on the request, the approval, requested -> approved -> executed, the governed write).
func (s *Service) Approve(ctx context.Context, a Actor, requestID uuid.UUID, reasonCode string) (*Result, error) {
	if err := validateActor(a); err != nil {
		return nil, err
	}
	if a.Scope != ScopePlatform {
		return nil, fmt.Errorf("%w: only a platform session approves", ErrForbidden)
	}
	if err := requirePerm(a, auth.PermLicensingChangeApprove); err != nil {
		return nil, err
	}
	if !ValidReasonCode(reasonCode) {
		return nil, fmt.Errorf("%w: reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]", ErrInvalidInput)
	}
	var res Result
	var expired bool
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		res, expired = Result{}, false
		req, err := lockRequest(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if req.Status != StatusRequested || req.RequiredApprovals == 0 {
			return ErrNotPending
		}
		if !s.now().Before(req.ExpiresAt) {
			expired = true
			return ErrNotPending
		}
		if req.RequestedBy == a.StaffID {
			return ErrSelfApproval
		}
		me, err := personOf(ctx, tx, a.StaffID)
		if err != nil {
			return err
		}
		if req.RequestedByPersonID == uuid.Nil {
			return ErrNoPerson
		}
		if me == req.RequestedByPersonID {
			return ErrSelfApproval
		}
		prior, err := s.loadApprovals(ctx, tx, req.ID)
		if err != nil {
			return fmt.Errorf("licensingchange: read approvals (fail closed): %w", err)
		}
		for _, ap := range prior {
			if ap.ApproverPersonID == me || ap.ApproverID == a.StaffID {
				return ErrAlreadyDecided
			}
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpLicensingChangeApprove, req.ID.String(), req.PayloadHash); err != nil {
			return err
		}
		var ap Approval
		if err := scanApproval(tx.QueryRow(ctx, `INSERT INTO licensing_change_approvals (request_id, reason_code) VALUES ($1, $2) RETURNING `+approvalColumns,
			req.ID, reasonCode), &ap); err != nil {
			return err
		}
		apps, err := s.loadApprovals(ctx, tx, req.ID)
		if err != nil {
			return fmt.Errorf("licensingchange: read approvals (fail closed): %w", err)
		}
		fresh, distinct := countApprovals(req, apps, s.now(), s.cfg.ApprovalMaxAge)
		if err := s.auditEvent(ctx, tx, a, "approved", req, map[string]any{
			"approval_reason_code": reasonCode, "approvals": fresh, "of_required": req.RequiredApprovals,
		}); err != nil {
			return err
		}
		req.Approvals = apps
		res.Request = req
		if fresh >= req.RequiredApprovals {
			return s.execute(ctx, tx, a, req, apps, &res)
		}
		if distinct >= req.RequiredApprovals {
			// Enough Persons approved, but not within the approval window: refuse (the approval just inserted rolls
			// back). The request cannot reach execution any more; the requester cancels it and files a new one.
			return ErrApprovalsStale
		}
		return nil
	})
	if expired {
		s.expireBestEffort(ctx, a, requestID)
	}
	if err != nil {
		err = classify(err)
		if errors.Is(err, ErrSelfApproval) || errors.Is(err, ErrNoPerson) || GuardCode(err) != "" {
			s.deniedAudit(ctx, a, "approve", requestID, err)
		}
		return nil, err
	}
	return &res, nil
}

// Reject closes an open request (an eligible platform approver who is not the requester's Person).
func (s *Service) Reject(ctx context.Context, a Actor, requestID uuid.UUID, reasonCode string) (*Request, error) {
	if err := validateActor(a); err != nil {
		return nil, err
	}
	if a.Scope != ScopePlatform {
		return nil, fmt.Errorf("%w: only a platform session rejects", ErrForbidden)
	}
	if err := requirePerm(a, auth.PermLicensingChangeApprove); err != nil {
		return nil, err
	}
	if !ValidReasonCode(reasonCode) {
		return nil, fmt.Errorf("%w: reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]", ErrInvalidInput)
	}
	var out Request
	var expired bool
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		expired = false
		req, err := lockRequest(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if req.Status != StatusRequested {
			return ErrNotPending
		}
		if !s.now().Before(req.ExpiresAt) {
			expired = true
			return ErrNotPending
		}
		me, err := personOf(ctx, tx, a.StaffID)
		if err != nil {
			return err
		}
		if req.RequestedBy == a.StaffID || me == req.RequestedByPersonID {
			return ErrSelfApproval
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpLicensingChangeReject, req.ID.String(), req.PayloadHash); err != nil {
			return err
		}
		if err := scanRequest(tx.QueryRow(ctx, `UPDATE licensing_change_requests SET status = 'rejected', decision_reason_code = $2 WHERE id = $1 RETURNING `+requestColumns,
			req.ID, reasonCode), &out); err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, a, "rejected", out, map[string]any{"decision_reason_code": reasonCode})
	})
	if expired {
		s.expireBestEffort(ctx, a, requestID)
	}
	if err != nil {
		err = classify(err)
		if errors.Is(err, ErrSelfApproval) || GuardCode(err) != "" {
			s.deniedAudit(ctx, a, "reject", requestID, err)
		}
		return nil, err
	}
	return &out, nil
}

// Cancel withdraws an open request (its requester only, platform or tenant).
func (s *Service) Cancel(ctx context.Context, a Actor, requestID uuid.UUID) (*Request, error) {
	if err := validateActor(a); err != nil {
		return nil, err
	}
	if err := requirePerm(a, auth.PermLicensingChangeRequest); err != nil {
		return nil, err
	}
	var out Request
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		req, err := lockRequest(ctx, tx, requestID)
		if err != nil {
			return err
		}
		if req.Status != StatusRequested {
			return ErrNotPending
		}
		if req.RequestedBy != a.StaffID {
			return fmt.Errorf("%w: only the requester may cancel", ErrForbidden)
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpLicensingChangeCancel, req.ID.String(), req.PayloadHash); err != nil {
			return err
		}
		if err := scanRequest(tx.QueryRow(ctx, `UPDATE licensing_change_requests SET status = 'cancelled' WHERE id = $1 RETURNING `+requestColumns, req.ID), &out); err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, a, "cancelled", out, nil)
	})
	if err != nil {
		return nil, classify(err)
	}
	return &out, nil
}

// execute runs the change: (approved,) proof-bound executed, the event audit, then the domain write LAST (a kind writing a
// tenant-RLS table binds the subject tenant for the rest of the transaction). The database re-checks everything: approvals
// (count, freshness, Person, role), expiry, the requester / transaction rule of a single-actor change, re-classification.
func (s *Service) execute(ctx context.Context, tx pgx.Tx, a Actor, req Request, apps []Approval, res *Result) error {
	spec, ok := kinds[req.Kind]
	if !ok {
		return fmt.Errorf("%w: kind %q has no executor", ErrInvalidInput, req.Kind)
	}
	payload, _, err := spec.decode(req.Payload)
	if err != nil {
		return fmt.Errorf("licensingchange: stored payload does not decode: %w", err)
	}
	if req.RequiredApprovals > 0 {
		if _, err := tx.Exec(ctx, `UPDATE licensing_change_requests SET status = 'approved' WHERE id = $1`, req.ID); err != nil {
			return err
		}
	}
	if err := s.attachProof(ctx, tx, a, actorproof.OpLicensingChangeExecute, req.ID.String(), req.PayloadHash); err != nil {
		return err
	}
	var done Request
	if err := scanRequest(tx.QueryRow(ctx, `UPDATE licensing_change_requests SET status = 'executed' WHERE id = $1 RETURNING `+requestColumns, req.ID), &done); err != nil {
		return err
	}
	done.Approvals = apps
	approvers := make([]string, 0, len(apps))
	for _, ap := range apps {
		approvers = append(approvers, ap.ApproverID.String())
	}
	if err := s.auditEvent(ctx, tx, a, "executed", done, map[string]any{"approvers": approvers}); err != nil {
		return err
	}
	out, err := spec.execute(ctx, tx, execContext{actor: a, req: done}, payload)
	if err != nil {
		var de *DomainError
		if errors.As(err, &de) {
			return err
		}
		if pgErr(err) != nil {
			return triggerRefusal(err)
		}
		return &DomainError{Err: err}
	}
	res.Request = done
	res.Executed = true
	res.Outcome = out
	return nil
}

// expireBestEffort materialises the expiry of a request found expired (separate transaction; failure is ignored, the
// request stays undecidable either way).
func (s *Service) expireBestEffort(ctx context.Context, a Actor, requestID uuid.UUID) {
	_ = s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		var r Request
		err := scanRequest(tx.QueryRow(ctx, `UPDATE licensing_change_requests SET status = 'expired'
			WHERE id = $1 AND status = 'requested' AND expires_at <= clock_timestamp() RETURNING `+requestColumns, requestID), &r)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, a, "expired", r, nil)
	})
}

// ---- audit ---------------------------------------------------------------------------------------------------------

// auditEvent writes one audit row per event in the SAME transaction. A platform actor's row is platform-scoped with the
// subject tenant projection (ADR 0104); a tenant actor's row is tenant-scoped.
func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, a Actor, event string, r Request, extra map[string]any) error {
	md := map[string]any{
		"change_request_id": r.ID.String(), "kind": string(r.Kind), "status": r.Status, "widening": r.Widening,
		"required_approvals": r.RequiredApprovals, "payload_hash": r.PayloadHash, "reason_code": r.ReasonCode,
		"requested_by": r.RequestedBy.String(), "payload": json.RawMessage(r.Payload),
	}
	for k, v := range extra {
		md[k] = v
	}
	e := audit.Entry{
		ActorType: audit.ActorStaff, ActorID: a.StaffID, Action: "licensing_change." + event,
		TargetType: "licensing_change_request", TargetID: r.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: a.IPAddress, UserAgent: a.UserAgent, RequestID: a.RequestID, Metadata: md,
	}
	if a.Scope == ScopeTenant {
		e.TenantID = a.TenantID
	} else if r.SubjectTenantID != nil {
		e.SubjectTenantID = *r.SubjectTenantID
	}
	if err := audit.Record(ctx, tx, e); err != nil {
		return fmt.Errorf("licensingchange: audit %s: %w", event, err)
	}
	return nil
}

// deniedAudit records a refused decision once, in its own transaction (codes only; nothing about the request's content).
func (s *Service) deniedAudit(ctx context.Context, a Actor, op string, requestID uuid.UUID, cause error) {
	code := GuardCode(cause)
	switch {
	case errors.Is(cause, ErrSelfApproval):
		code = "self_approval"
	case errors.Is(cause, ErrNoPerson):
		code = "no_person"
	}
	_ = s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		e := audit.Entry{
			ActorType: audit.ActorStaff, ActorID: a.StaffID, Action: "licensing_change." + op + "_denied",
			TargetType: "licensing_change_request", TargetID: requestID.String(), Outcome: audit.OutcomeDenied,
			IPAddress: a.IPAddress, UserAgent: a.UserAgent, RequestID: a.RequestID, Metadata: map[string]any{"refusal": code},
		}
		if a.Scope == ScopeTenant {
			e.TenantID = a.TenantID
		}
		return audit.Record(ctx, tx, e)
	})
}

// ---- reads ---------------------------------------------------------------------------------------------------------

// Get returns one request with its approvals. A tenant actor sees only requests about its own tenant (RLS); anything else is
// ErrNotFound.
func (s *Service) Get(ctx context.Context, a Actor, requestID uuid.UUID) (*Request, error) {
	if err := validateActor(a); err != nil {
		return nil, err
	}
	if err := requirePerm(a, auth.PermLicensingChangeRead); err != nil {
		return nil, err
	}
	var out Request
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM licensing_change_requests WHERE id = $1`, requestID), &out)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		out.Approvals, err = loadApprovals(ctx, tx, requestID)
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return &out, nil
}

// List returns requests, newest first (a tenant actor: the ones about its own tenant).
func (s *Service) List(ctx context.Context, a Actor, f ListFilter) ([]Request, error) {
	if err := validateActor(a); err != nil {
		return nil, err
	}
	if err := requirePerm(a, auth.PermLicensingChangeRead); err != nil {
		return nil, err
	}
	if f.Status != "" {
		switch f.Status {
		case StatusRequested, StatusApproved, StatusExecuted, StatusRejected, StatusCancelled, StatusExpired:
		default:
			return nil, fmt.Errorf("%w: unknown status filter", ErrInvalidInput)
		}
	}
	if f.Kind != "" {
		if _, ok := kinds[f.Kind]; !ok {
			return nil, fmt.Errorf("%w: unknown kind filter", ErrInvalidInput)
		}
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 100
	}
	var out []Request
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM licensing_change_requests
			WHERE ($1 = '' OR status = $1) AND ($2 = '' OR kind = $2)
			ORDER BY created_at DESC, id LIMIT $3`, f.Status, string(f.Kind), f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Request
			if err := scanRequest(rows, &r); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}
