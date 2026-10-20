package withdrawalpolicy

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
)

// configurableCauses mirrors the registry rows with configurable = true (the database is authoritative: WR002).
var configurableCauses = map[string]bool{
	"licence_suspended": true, "licence_expired": true, "licence_not_yet_issued": true, "licence_missing": true,
}

// Request files a policy change for one (tenant, cause). The database classifies it; a narrowing (or equal) change executes
// in the SAME transaction (single actor with a reason code); a widening one waits for two approvals.
func (s *Service) Request(ctx context.Context, a Actor, in RequestInput) (*Result, error) {
	if err := requireChanger(a); err != nil {
		return nil, err
	}
	if in.TenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	if !configurableCauses[in.Cause] {
		return nil, fmt.Errorf("%w: %q", ErrNotConfigurable, in.Cause)
	}
	if !ValidAction(in.Action) {
		return nil, fmt.Errorf("%w: action must be allow, hold_for_review or block", ErrInvalidInput)
	}
	if !ValidReasonCode(in.ReasonCode) {
		return nil, fmt.Errorf("%w: reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]", ErrInvalidInput)
	}
	var note *string
	if strings.TrimSpace(in.Note) != "" {
		if len(in.Note) > 1000 || strings.ContainsFunc(in.Note, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			return nil, fmt.Errorf("%w: note must be 1..1000 bytes without control characters", ErrInvalidInput)
		}
		n := in.Note
		note = &n
	}
	var res Result
	err := s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		res = Result{}
		if err := lockTenantPolicy(ctx, tx, in.TenantID); err != nil {
			return err
		}
		var hash string
		if err := tx.QueryRow(ctx, `SELECT withdrawal_restriction_payload_hash($1, $2, $3, $4)`, in.TenantID, in.Cause, in.Action, in.ReasonCode).Scan(&hash); err != nil {
			return err
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpWithdrawalRestrictionRequest, actorproof.TargetNew, hash); err != nil {
			return err
		}
		var c Change
		if err := scanChange(tx.QueryRow(ctx, `
			INSERT INTO withdrawal_restriction_policy_changes (tenant_id, cause, action, reason_code, note)
			VALUES ($1, $2, $3, $4, $5) RETURNING `+changeColumns,
			in.TenantID, in.Cause, in.Action, in.ReasonCode, note), &c); err != nil {
			return err
		}
		if err := s.auditEvent(ctx, tx, a, "requested", c, nil); err != nil {
			return err
		}
		res.Change = c
		if c.RequiredApprovals > 0 {
			return nil
		}
		return s.execute(ctx, tx, a, c, nil, &res)
	})
	if err != nil {
		return nil, classify(err)
	}
	return &res, nil
}

// Approve records an approval. When it is the final one required, the SAME transaction executes the change.
func (s *Service) Approve(ctx context.Context, a Actor, changeID uuid.UUID, reasonCode string) (*Result, error) {
	if err := requireChanger(a); err != nil {
		return nil, err
	}
	if !ValidReasonCode(reasonCode) {
		return nil, fmt.Errorf("%w: reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]", ErrInvalidInput)
	}
	var res Result
	var expired bool
	err := s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		res, expired = Result{}, false
		// The tenant id is immutable: an unlocked read finds it so the tenant policy lock is taken BEFORE the row lock.
		pre, err := readChange(ctx, tx, changeID, false)
		if err != nil {
			return err
		}
		if err := lockTenantPolicy(ctx, tx, pre.TenantID); err != nil {
			return err
		}
		c, err := readChange(ctx, tx, changeID, true)
		if err != nil {
			return err
		}
		if c.Status != StatusRequested || c.RequiredApprovals == 0 {
			return ErrNotPending
		}
		if !s.now().Before(c.ExpiresAt) {
			expired = true
			return ErrNotPending
		}
		if c.RequestedBy == a.StaffID {
			return ErrSelfApproval
		}
		me, err := personOf(ctx, tx, a.StaffID)
		if err != nil {
			return err
		}
		if c.RequestedByPersonID == uuid.Nil {
			return ErrNoPerson
		}
		if me == c.RequestedByPersonID {
			return ErrSelfApproval
		}
		prior, err := loadApprovals(ctx, tx, c.ID)
		if err != nil {
			return fmt.Errorf("withdrawalpolicy: read approvals (fail closed): %w", err)
		}
		for _, ap := range prior {
			if ap.ApproverPersonID == me || ap.ApproverID == a.StaffID {
				return ErrAlreadyDecided
			}
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpWithdrawalRestrictionApprove, c.ID.String(), c.PayloadHash); err != nil {
			return err
		}
		var ap Approval
		if err := scanApproval(tx.QueryRow(ctx, `INSERT INTO withdrawal_restriction_policy_approvals (change_id, reason_code) VALUES ($1, $2) RETURNING `+approvalColumns,
			c.ID, reasonCode), &ap); err != nil {
			return err
		}
		apps, err := loadApprovals(ctx, tx, c.ID)
		if err != nil {
			return fmt.Errorf("withdrawalpolicy: read approvals (fail closed): %w", err)
		}
		fresh, distinct := countApprovals(c, apps, s.now(), s.cfg.ApprovalMaxAge)
		if err := s.auditEvent(ctx, tx, a, "approved", c, map[string]any{
			"approval_reason_code": reasonCode, "approvals": fresh, "of_required": c.RequiredApprovals,
		}); err != nil {
			return err
		}
		c.Approvals = apps
		res.Change = c
		if fresh >= c.RequiredApprovals {
			return s.execute(ctx, tx, a, c, apps, &res)
		}
		if distinct >= c.RequiredApprovals {
			return ErrApprovalsStale
		}
		return nil
	})
	if expired {
		s.expireBestEffort(ctx, a, changeID)
	}
	if err != nil {
		err = classify(err)
		if errors.Is(err, ErrSelfApproval) || errors.Is(err, ErrNoPerson) || GuardCode(err) != "" {
			s.deniedAudit(ctx, a, "approve", changeID, err)
		}
		return nil, err
	}
	return &res, nil
}

// Reject closes an open change (a platform_admin who is not the requester's Person).
func (s *Service) Reject(ctx context.Context, a Actor, changeID uuid.UUID, reasonCode string) (*Change, error) {
	if err := requireChanger(a); err != nil {
		return nil, err
	}
	if !ValidReasonCode(reasonCode) {
		return nil, fmt.Errorf("%w: reason_code must be a code of 1..64 characters [A-Za-z0-9_.:-]", ErrInvalidInput)
	}
	var out Change
	var expired bool
	err := s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		expired = false
		c, err := readChange(ctx, tx, changeID, true)
		if err != nil {
			return err
		}
		if c.Status != StatusRequested {
			return ErrNotPending
		}
		if !s.now().Before(c.ExpiresAt) {
			expired = true
			return ErrNotPending
		}
		me, err := personOf(ctx, tx, a.StaffID)
		if err != nil {
			return err
		}
		if c.RequestedBy == a.StaffID || me == c.RequestedByPersonID {
			return ErrSelfApproval
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpWithdrawalRestrictionReject, c.ID.String(), c.PayloadHash); err != nil {
			return err
		}
		if err := scanChange(tx.QueryRow(ctx, `UPDATE withdrawal_restriction_policy_changes SET status = 'rejected', decision_reason_code = $2 WHERE id = $1 RETURNING `+changeColumns,
			c.ID, reasonCode), &out); err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, a, "rejected", out, map[string]any{"decision_reason_code": reasonCode})
	})
	if expired {
		s.expireBestEffort(ctx, a, changeID)
	}
	if err != nil {
		err = classify(err)
		if errors.Is(err, ErrSelfApproval) || GuardCode(err) != "" {
			s.deniedAudit(ctx, a, "reject", changeID, err)
		}
		return nil, err
	}
	return &out, nil
}

// Cancel withdraws an open change (its requester only).
func (s *Service) Cancel(ctx context.Context, a Actor, changeID uuid.UUID) (*Change, error) {
	if err := requireChanger(a); err != nil {
		return nil, err
	}
	var out Change
	err := s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		c, err := readChange(ctx, tx, changeID, true)
		if err != nil {
			return err
		}
		if c.Status != StatusRequested {
			return ErrNotPending
		}
		if c.RequestedBy != a.StaffID {
			return fmt.Errorf("%w: only the requester may cancel", ErrForbidden)
		}
		if err := s.attachProof(ctx, tx, a, actorproof.OpWithdrawalRestrictionCancel, c.ID.String(), c.PayloadHash); err != nil {
			return err
		}
		if err := scanChange(tx.QueryRow(ctx, `UPDATE withdrawal_restriction_policy_changes SET status = 'cancelled' WHERE id = $1 RETURNING `+changeColumns, c.ID), &out); err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, a, "cancelled", out, nil)
	})
	if err != nil {
		return nil, classify(err)
	}
	return &out, nil
}

// execute: (approved,) proof-bound executed, then the policy version row LAST, then the audit with before / after. The
// database re-checks everything (approvals, freshness, Person, expiry, the single-actor rule, re-classification under the
// tenant policy lock) and assigns the set version.
func (s *Service) execute(ctx context.Context, tx pgx.Tx, a Actor, c Change, apps []Approval, res *Result) error {
	before, err := currentEffects(ctx, tx, c.TenantID, c.Cause)
	if err != nil {
		return err
	}
	if c.RequiredApprovals > 0 {
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_restriction_policy_changes SET status = 'approved' WHERE id = $1`, c.ID); err != nil {
			return err
		}
	}
	if err := s.attachProof(ctx, tx, a, actorproof.OpWithdrawalRestrictionExecute, c.ID.String(), c.PayloadHash); err != nil {
		return err
	}
	var done Change
	if err := scanChange(tx.QueryRow(ctx, `UPDATE withdrawal_restriction_policy_changes SET status = 'executed' WHERE id = $1 RETURNING `+changeColumns, c.ID), &done); err != nil {
		return err
	}
	var p Policy
	if err := scanPolicy(tx.QueryRow(ctx, `
		INSERT INTO withdrawal_restriction_policies (tenant_id, cause, action, change_id, reason_code)
		VALUES ($1, $2, $3, $4, $5) RETURNING `+policyColumns,
		done.TenantID, done.Cause, done.Action, done.ID, done.ReasonCode), &p); err != nil {
		return err
	}
	done.Approvals = apps
	approvers := make([]string, 0, len(apps))
	for _, ap := range apps {
		approvers = append(approvers, ap.ApproverID.String())
	}
	beforeMD := make([]map[string]any, 0, len(before))
	for _, st := range before {
		beforeMD = append(beforeMD, map[string]any{"stage": st.Stage, "action": st.Action, "source": st.Source})
	}
	if err := s.auditEvent(ctx, tx, a, "executed", done, map[string]any{
		"approvers": approvers,
		"before":    beforeMD,
		"after":     map[string]any{"action": p.Action, "source": SourceConfigured, "policy_id": p.ID.String(), "set_version": p.SetVersion},
	}); err != nil {
		return err
	}
	res.Change = done
	res.Executed = true
	res.Policy = &p
	return nil
}

// currentEffects reads the effective action of (tenant, cause) at every stage (the database function, under the session's RLS).
func currentEffects(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, cause string) ([]StageEffect, error) {
	out := make([]StageEffect, 0, len(Stages))
	for _, st := range Stages {
		e := StageEffect{Stage: st}
		if err := tx.QueryRow(ctx, `SELECT action, source, policy_id, set_version FROM withdrawal_restriction_effective($1, $2, $3)`,
			tenantID, cause, st).Scan(&e.Action, &e.Source, &e.PolicyID, &e.SetVersion); err != nil {
			return nil, fmt.Errorf("withdrawalpolicy: read the effective policy: %w", err)
		}
		out = append(out, e)
	}
	return out, nil
}

// expireBestEffort materialises the expiry of a change found expired (separate transaction; failure ignored).
func (s *Service) expireBestEffort(ctx context.Context, a Actor, changeID uuid.UUID) {
	_ = s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		var c Change
		err := scanChange(tx.QueryRow(ctx, `UPDATE withdrawal_restriction_policy_changes SET status = 'expired'
			WHERE id = $1 AND status = 'requested' AND expires_at <= clock_timestamp() RETURNING `+changeColumns, changeID), &c)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		return s.auditEvent(ctx, tx, a, "expired", c, nil)
	})
}

// auditEvent writes one platform-scoped audit row per event in the SAME transaction, projected to the subject tenant
// (ADR 0104: the tenant reads the history of its own policy).
func (s *Service) auditEvent(ctx context.Context, tx pgx.Tx, a Actor, event string, c Change, extra map[string]any) error {
	md := map[string]any{
		"change_id": c.ID.String(), "cause": c.Cause, "action": c.Action, "status": c.Status, "widening": c.Widening,
		"required_approvals": c.RequiredApprovals, "payload_hash": c.PayloadHash, "reason_code": c.ReasonCode,
		"requested_by": c.RequestedBy.String(), "base_set_version": c.BaseSetVersion,
	}
	for k, v := range extra {
		md[k] = v
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorType: audit.ActorStaff, ActorID: a.StaffID, Action: "withdrawal_restriction_policy." + event,
		TargetType: "withdrawal_restriction_policy_change", TargetID: c.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: a.IPAddress, UserAgent: a.UserAgent, RequestID: a.RequestID, SubjectTenantID: c.TenantID, Metadata: md,
	}); err != nil {
		return fmt.Errorf("withdrawalpolicy: audit %s: %w", event, err)
	}
	return nil
}

// deniedAudit records a refused decision once, in its own transaction (codes only).
func (s *Service) deniedAudit(ctx context.Context, a Actor, op string, changeID uuid.UUID, cause error) {
	code := GuardCode(cause)
	switch {
	case errors.Is(cause, ErrSelfApproval):
		code = "self_approval"
	case errors.Is(cause, ErrNoPerson):
		code = "no_person"
	}
	_ = s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			ActorType: audit.ActorStaff, ActorID: a.StaffID, Action: "withdrawal_restriction_policy." + op + "_denied",
			TargetType: "withdrawal_restriction_policy_change", TargetID: changeID.String(), Outcome: audit.OutcomeDenied,
			IPAddress: a.IPAddress, UserAgent: a.UserAgent, RequestID: a.RequestID, Metadata: map[string]any{"refusal": code},
		})
	})
}
