package launchgov

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// DecideInput is a platform approver's decision.
type DecideInput struct {
	Decision   string // DecisionApprove | DecisionReject
	ReasonCode string // closed catalogue, per decision
	// SeenReadinessHash is the readiness hash the approver was shown (required to approve). If the
	// server's fresh evaluation hashes differently the approval is refused (ErrStaleReadiness) and
	// nothing is recorded: an approver approves what they saw.
	SeenReadinessHash string
}

// EvalInputOf rebuilds the readiness input from a stored request (never from the caller).
func EvalInputOf(r Request) EvalInput {
	in := EvalInput{Subject: r.Subject(), Action: r.Action, JurisdictionIDs: r.JurisdictionIDs}
	if r.LicensingModel != nil {
		rec := &LicensingRecord{Model: *r.LicensingModel, LicenceID: r.LicenceID}
		if r.LicensingStatus != nil {
			rec.Status = *r.LicensingStatus
		}
		if r.DeterminationReference != nil {
			rec.DeterminationReference = *r.DeterminationReference
		}
		if r.DeterminationByRole != nil {
			rec.DeterminationByRole = *r.DeterminationByRole
		}
		in.Licensing = rec
	}
	return in
}

// peek reads a request without locking it (the evaluation inputs live in it).
func (s *Service) peek(ctx context.Context, a Actor, tenantID, id uuid.UUID) (Request, error) {
	var r Request
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM launch_authorisation_requests WHERE id = $1 AND tenant_id = $2`, id, tenantID), &r)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	})
	return r, err
}

// Decide records a platform approver's approve or reject decision on a pending request. The
// approval carries the approver's signed proof (target = the request id, payload = the request's
// payload hash). When an approval is the final one required, the SAME transaction executes the
// request (ADR 0112 6.2 S9): row lock on the request, the approval, pending -> executing, the
// hard-precondition re-checks, the governed transition, the status UPDATE, executing -> executed.
// Tenant staff never approve; the requester and any account of the requester's Person are refused.
func (s *Service) Decide(ctx context.Context, a Actor, tenantID, requestID uuid.UUID, in DecideInput) (*DecisionResult, error) {
	if a.Scope != ScopePlatform {
		return nil, fmt.Errorf("%w: only a platform session decides a request", ErrForbidden)
	}
	if err := authorize(a, auth.PermLaunchAuthorisationApprove, Subject{TenantID: tenantID}, false); err != nil {
		return nil, err
	}
	if in.Decision != DecisionApprove && in.Decision != DecisionReject {
		return nil, fmt.Errorf("%w: decision must be approve or reject", ErrInvalidInput)
	}
	if !reasonAllowed(decisionReasons, in.Decision, in.ReasonCode) {
		return nil, fmt.Errorf("%w: reason code %q is not in the closed catalogue for %s", ErrInvalidInput, in.ReasonCode, in.Decision)
	}
	if in.Decision == DecisionApprove && len(in.SeenReadinessHash) != 64 {
		return nil, fmt.Errorf("%w: the readiness hash the approver saw is required", ErrInvalidInput)
	}

	pre, err := s.peek(ctx, a, tenantID, requestID)
	if err != nil {
		return nil, err
	}
	sub := pre.Subject()
	if pre.Status != ReqPending || !s.now().Before(pre.ExpiresAt) {
		s.sweepBestEffort(ctx, a, tenantID, &requestID)
		return nil, ErrNotPending
	}
	if pre.RequestedBy == a.StaffID {
		return nil, s.denied(ctx, a, "decide", sub, pre.Action, ErrSelfApproval, withRequest(requestID, pre.ReasonCode))
	}
	as, err := s.assess(ctx, EvalInputOf(pre))
	if err != nil {
		return nil, err
	}
	hash := as.Readiness.Hash()
	if in.Decision == DecisionApprove && in.SeenReadinessHash != hash {
		return nil, s.denied(ctx, a, "decide", sub, pre.Action, ErrStaleReadiness, withRequest(requestID, pre.ReasonCode))
	}

	var res DecisionResult
	res.Readiness = as.Readiness
	var refused string
	err = s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		refused = ""
		req, err := lockRequest(ctx, tx, tenantID, requestID)
		if err != nil {
			return err
		}
		if req.Status != ReqPending || !s.now().Before(req.ExpiresAt) {
			return ErrNotPending
		}
		if req.PayloadHash != pre.PayloadHash {
			return ErrNotPending // content is immutable; a mismatch means a different row version: never decide it
		}
		if same, err := samePersonAsRequester(ctx, tx, a.StaffID, req); err != nil {
			return err
		} else if same {
			return ErrSelfApproval
		}
		// A replayed decision (the same Person already decided this request) is a stable,
		// side-effect-free refusal: the database would refuse it too (UNIQUE request + Person, LA012),
		// but with an opaque code.
		meNow, err := personOf(ctx, tx, a.StaffID)
		if err != nil {
			return err
		}
		prior, err := loadApprovals(ctx, tx, req.ID)
		if err != nil {
			return err
		}
		for _, ap := range prior {
			if ap.DecidedByPersonID == meNow {
				return ErrAlreadyDecided
			}
		}
		// Security C2: every reject of the same subject and action within the TTL (any payload)
		// is shown to the approver, and an approver whose own Person rejected one may not
		// approve a request that carries no change note.
		priors, err := loadPriorRejects(ctx, tx, s.now(), req)
		if err != nil {
			return err
		}
		res.PriorRejects = priors
		if in.Decision == DecisionApprove && strings.TrimSpace(req.ConditionsNote) == "" {
			me, err := personOf(ctx, tx, a.StaffID)
			if err != nil {
				return err
			}
			for _, p := range priors {
				if p.DecidedByPersonID == me {
					return ErrPriorRejecter
				}
			}
		}
		op := actorproof.OpLaunchApprove
		if in.Decision == DecisionReject {
			op = actorproof.OpLaunchReject
		}
		if err := s.attachProof(ctx, tx, a, op, req.ID.String(), actorproof.LaunchApprovalDigest(req.PayloadHash, hash, in.ReasonCode)); err != nil {
			return err
		}
		var ap Approval
		if err := scanApproval(tx.QueryRow(ctx, `
			INSERT INTO launch_authorisation_approvals (request_id, decision, payload_hash, readiness_snapshot_hash_at_decision, reason_code)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+approvalColumns,
			req.ID, in.Decision, req.PayloadHash, hash, in.ReasonCode), &ap); err != nil {
			return err
		}
		res.Decision = in.Decision

		if in.Decision == DecisionReject {
			if err := scanRequest(tx.QueryRow(ctx, `UPDATE launch_authorisation_requests SET status = 'rejected' WHERE id = $1 RETURNING `+requestColumns, req.ID), &req); err != nil {
				return err
			}
			res.Request = req
			return s.auditEvent(ctx, tx, a, "rejected", req, map[string]any{"decision_reason_code": in.ReasonCode, "decided_by": a.StaffID.String()})
		}

		apps, err := loadApprovals(ctx, tx, req.ID)
		if err != nil {
			return err
		}
		counted := countedApprovals(req, apps)
		if len(counted) < req.RequiredApprovals {
			res.Request = req
			return s.auditEvent(ctx, tx, a, "approved", req, map[string]any{
				"decision_reason_code": in.ReasonCode, "decided_by": a.StaffID.String(),
				"approvals": len(counted), "of_required": req.RequiredApprovals, "readiness_snapshot_hash_at_decision": hash,
			})
		}
		if err := s.auditEvent(ctx, tx, a, "approved", req, map[string]any{
			"decision_reason_code": in.ReasonCode, "decided_by": a.StaffID.String(),
			"approvals": len(counted), "of_required": req.RequiredApprovals, "readiness_snapshot_hash_at_decision": hash, "final": true,
		}); err != nil {
			return err
		}
		out, code, err := s.execute(ctx, tx, a, req, counted, as, hash)
		if err != nil {
			return err
		}
		res.Request = out
		refused = code
		res.Executed = code == ""
		return nil
	})
	if err != nil {
		err = classify(err)
		if errors.Is(err, ErrNotPending) {
			// The request may have expired by time: materialise (and audit once) the expiry.
			s.sweepBestEffort(ctx, a, tenantID, &requestID)
		}
		if errors.Is(err, ErrRetryableLockTimeout) || errors.Is(err, ErrNotFound) || errors.Is(err, ErrNotPending) || errors.Is(err, ErrIsolationLevel) || errors.Is(err, ErrProofUnavailable) {
			return nil, err
		}
		return nil, s.deniedExec(ctx, a, "decide", sub, pre, err)
	}
	if refused != "" {
		res.Refused = refused
		return &res, &RefusedAtExecutionError{RequestID: requestID, Code: refused}
	}
	return &res, nil
}

// deniedExec audits a rolled-back execution refusal. A closure blocked by open sportsbook bets
// (GP020) keeps the counts-only row of the former ChangeStatus audit.
func (s *Service) deniedExec(ctx context.Context, a Actor, op string, sub Subject, req Request, err error) error {
	return s.denied(ctx, a, op, sub, req.Action, err, withRequest(req.ID, req.ReasonCode))
}

// ErrNoPerson: a staff row is unreadable or carries no Person, so "same Person" cannot be
// decided. The check fails CLOSED with an error (the database refuses a NULL person_id too:
// LA012 / LA001), never with "not the same Person".
var ErrNoPerson = errors.New("launchgov: a staff account has no readable Person; the four-eyes check cannot be evaluated")

// personOf returns the Person of a staff account, or ErrNoPerson when the row is unreadable or
// has none.
func personOf(ctx context.Context, tx pgx.Tx, staff uuid.UUID) (uuid.UUID, error) {
	var p *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, staff).Scan(&p)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && p == nil) {
		return uuid.Nil, ErrNoPerson
	}
	if err != nil {
		return uuid.Nil, err
	}
	return *p, nil
}

// samePersonAsRequester reports whether the approver's staff account is linked to the SAME Person
// as the request's requester. The requester's Person is the one the database recorded on the
// request (requested_by_person_id; a tenant requester's own staff row is not readable from a
// platform session), the approver's is read from its own row. An unreadable approver row, a
// NULL person_id on either side, or a nil recorded Person fails CLOSED with ErrNoPerson: the
// answer "not the same Person" is never inferred from missing data (security L3).
func samePersonAsRequester(ctx context.Context, tx pgx.Tx, approver uuid.UUID, req Request) (bool, error) {
	me, err := personOf(ctx, tx, approver)
	if err != nil {
		return false, err
	}
	if req.RequestedByPersonID == uuid.Nil {
		return false, ErrNoPerson
	}
	return me == req.RequestedByPersonID, nil
}

func loadApprovals(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) ([]Approval, error) {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM launch_authorisation_approvals WHERE request_id = $1 ORDER BY decided_at, id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var ap Approval
		if err := scanApproval(rows, &ap); err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

// countedApprovals are the approve decisions that count toward the four-eyes requirement:
// payload hash matching, by distinct Persons other than the requester (the database counts the
// same way before it lets the request reach 'executing').
func countedApprovals(req Request, apps []Approval) []Approval {
	seen := map[uuid.UUID]bool{req.RequestedByPersonID: true}
	var out []Approval
	for _, ap := range apps {
		if ap.Decision != DecisionApprove || ap.PayloadHash != req.PayloadHash || ap.DecidedBy == req.RequestedBy {
			continue
		}
		if seen[ap.DecidedByPersonID] {
			continue
		}
		seen[ap.DecidedByPersonID] = true
		out = append(out, ap)
	}
	return out
}
