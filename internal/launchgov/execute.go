package launchgov

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// Refusal codes recorded in launch_authorisation_requests.refusal_code when the executor refuses
// a request that already holds its approvals (status refused_at_execution; durable).
const (
	RefusedApprovalsStale          = "approvals_stale"
	RefusedReadinessChangedSince   = "readiness_changed_since_approval"
	RefusedReadinessUnacknowledged = "readiness_changed_unacknowledged"
)

// execute is the executor (ADR 0112 7.1: the unexported step that absorbs tenant.ChangeStatus).
// The caller holds the request row FOR UPDATE inside a READ COMMITTED platform-or-tenant
// transaction and has just inserted the final approval (or, for a suspension, the request).
// Exactly ONE subject changes status per transaction (security C2): there is no loop here and the
// database refuses a tenant change after a brand change of the same tenant in one transaction
// (migration 0130).
//
// Outcomes:
//   - executed: ("", nil)
//   - refused_at_execution: the request becomes refused_at_execution with a code and the
//     transaction COMMITS (the approvals and the refusal are durable); returns (req, code, nil)
//   - a transient or operator-fixable refusal (lock timeout, GP020 open bets, hold-bearing
//     withdrawals): an error, the transaction ROLLS BACK, the request stays pending
func (s *Service) execute(ctx context.Context, tx pgx.Tx, a Actor, req Request, counted []Approval, as assessment, readinessHash string) (Request, string, error) {
	var out Request
	if err := scanRequest(tx.QueryRow(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1 RETURNING `+requestColumns, req.ID), &out); err != nil {
		return Request{}, "", err
	}
	sub := out.Subject()
	status, err := s.lockSubject(ctx, tx, a, sub)
	if err != nil {
		return Request{}, "", err
	}

	// ---- decision-integrity refusals: durable refused_at_execution ------------------------
	refuse := func(code string) (Request, string, error) {
		var r Request
		if err := scanRequest(tx.QueryRow(ctx, `UPDATE launch_authorisation_requests SET status = 'refused_at_execution', refusal_code = $2 WHERE id = $1 RETURNING `+requestColumns, out.ID, code), &r); err != nil {
			return Request{}, "", err
		}
		if err := s.auditEvent(ctx, tx, a, "refused_at_execution", r, map[string]any{"refusal_code": code, "readiness_snapshot_hash": readinessHash}); err != nil {
			return Request{}, "", err
		}
		return r, code, nil
	}
	if status != out.FromStatus {
		return refuse(H1StatusChanged)
	}
	// ---- H-6 (LA023): no hold-bearing withdrawal request of the subject's players at closure,
	// counted under the subject locks so no new initiation can slip in (a tenant closure holds
	// the tenant status gate key; a brand closure holds the brand row FOR NO KEY UPDATE, which the
	// H-SEC-5/11 FOR SHARE initiation read waits on). It is checked BEFORE the readiness-integrity
	// refusals so the operator sees the fixable cause; it rolls back and the request stays pending
	// (like GP020: resolve the withdrawals, then approve again).
	if out.Action == ActionClose {
		var holds int64
		if err := s.withSubjectTenant(ctx, tx, a, out.TenantID, func() error {
			var err error
			holds, err = holdBearingWithdrawals(ctx, tx, sub)
			return err
		}); err != nil {
			return Request{}, "", err
		}
		if holds > 0 {
			return Request{}, "", &PreconditionError{Base: ErrWithdrawalsHolding, Codes: []string{H6WithdrawalsHolding}}
		}
	}
	for _, f := range as.Failures {
		if f == H6WithdrawalsHolding {
			continue // the authoritative count is the one above, under the locks
		}
		return refuse(f)
	}
	oldest := s.now().Add(-s.cfg.ApprovalMaxAge)
	for _, ap := range counted {
		if ap.DecidedAt.Before(oldest) {
			return refuse(RefusedApprovalsStale)
		}
		if ap.ReadinessHashAtDec != readinessHash {
			return refuse(RefusedReadinessChangedSince)
		}
	}
	acked := map[string]bool{}
	for _, c := range out.AcknowledgedCheckCodes {
		acked[c] = true
	}
	for _, c := range as.Readiness.NonPassing() {
		if !acked[c] {
			return refuse(RefusedReadinessUnacknowledged)
		}
	}

	// ---- the governed transition and the status change ---------------------------------------
	if _, err := tx.Exec(ctx, `
		INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
		VALUES ($1, $2, $3, 'governed', $4, $5, $6)`,
		out.TenantID, out.SubjectKind, out.BrandID, out.FromStatus, out.ToStatus, out.ID); err != nil {
		return Request{}, "", err
	}
	if out.Action != ActionRatify { // ratify records a decision; it changes no status
		if err := s.updateSubjectStatus(ctx, tx, a, sub, out.ToStatus); err != nil {
			return Request{}, "", err
		}
	}
	var done Request
	if err := scanRequest(tx.QueryRow(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1 RETURNING `+requestColumns, out.ID), &done); err != nil {
		return Request{}, "", err
	}
	approvers := make([]string, 0, len(counted))
	for _, ap := range counted {
		approvers = append(approvers, ap.DecidedBy.String())
	}
	event := "executed"
	if done.Action == ActionRatify {
		event = "ratified"
	}
	if err := s.auditEvent(ctx, tx, a, event, done, map[string]any{
		"before": done.FromStatus, "after": done.ToStatus, "approver_ids": approvers, "executed_by": a.StaffID.String(),
		"readiness_snapshot_hash": readinessHash,
	}); err != nil {
		return Request{}, "", err
	}
	return done, "", nil
}

// lockSubject locks the subject row FOR NO KEY UPDATE (the lock the status UPDATE itself takes: it conflicts with
// the H-SEC-5/11 FOR SHARE readers and with other status writers, but NOT with the FOR KEY SHARE locks every
// foreign-key check on tenants / brands takes, so a gameplay placement that holds the brand gate key and then
// inserts a bet row can never deadlock with it) and returns its current status. A tenant subject
// is read in the platform session (tenants_platform_admin_update, as the former ChangeStatus did)
// and additionally takes the tenant status gate key exclusively NOW (the 0118/0121 trigger takes
// the same key at the UPDATE; taking it earlier lets the withdrawal count run under it). A brand
// subject is read through the S6 technique (app.tenant_id bound to the subject's own tenant) so
// the row lock is real; the brand gate key is taken by the status guard at the UPDATE.
func (s *Service) lockSubject(ctx context.Context, tx pgx.Tx, a Actor, sub Subject) (string, error) {
	var status string
	if sub.BrandID == nil {
		err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR NO KEY UPDATE`, sub.TenantID).Scan(&status)
		if errors.Is(err, pgx.ErrNoRows) {
			return "", ErrNotFound
		}
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(public.tenant_status_gate_key($1))`, sub.TenantID); err != nil {
			return "", err
		}
		return status, nil
	}
	err := s.withSubjectTenant(ctx, tx, a, sub.TenantID, func() error {
		return tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1 AND tenant_id = $2 FOR NO KEY UPDATE`, *sub.BrandID, sub.TenantID).Scan(&status)
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	return status, err
}

// lockSubjectShared locks the subject row FOR SHARE (see Request): it conflicts with the
// executor's FOR NO KEY UPDATE and with nothing the gameplay foreign-key checks take. The status
// is read only to take the lock; it is not used.
func (s *Service) lockSubjectShared(ctx context.Context, tx pgx.Tx, a Actor, sub Subject) error {
	var status string
	var err error
	if sub.BrandID == nil {
		err = tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1 FOR SHARE`, sub.TenantID).Scan(&status)
	} else {
		err = s.withSubjectTenant(ctx, tx, a, sub.TenantID, func() error {
			return tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1 AND tenant_id = $2 FOR SHARE`, *sub.BrandID, sub.TenantID).Scan(&status)
		})
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

// updateSubjectStatus is THE status write: the only place in the non-test code base that updates
// tenants.status or brands.status (pinned by a static test). The database refuses it unless the
// same transaction holds a governed transition of an executing request (zz_launch_status_governed).
func (s *Service) updateSubjectStatus(ctx context.Context, tx pgx.Tx, a Actor, sub Subject, to string) error {
	if sub.BrandID == nil {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = $1, updated_at = now() WHERE id = $2`, to, sub.TenantID)
		return err
	}
	return s.withSubjectTenant(ctx, tx, a, sub.TenantID, func() error {
		_, err := tx.Exec(ctx, `UPDATE brands SET status = $1, updated_at = now() WHERE id = $2 AND tenant_id = $3`, to, *sub.BrandID, sub.TenantID)
		return err
	})
}

// SuspendInput files an immediate suspension (the fail-closed direction, ADR 0112 6.3).
type SuspendInput struct {
	Subject    Subject
	ReasonCode string // closed catalogue for suspend; mandatory
	// ConditionsNote is an optional bounded record (PII-linted); nothing in it is enforced.
	ConditionsNote string
}

// Suspend suspends a subject at once: one authorised person, a mandatory reason code and a signed
// proof; the request is inserted and executed in ONE transaction (required_approvals = 0). It
// supersedes every other pending request of the subject in that transaction (the pending rows are
// locked first, in id order, so a suspension and an approval racing on the same subject take row
// locks in the same order), audits the superseded request ids, and retries on a lock timeout a
// bounded number of times (the suspension waits for in-flight gameplay and payment lock holders by
// design). Reactivation is always four-eyes: a suspender can never reactivate alone.
func (s *Service) Suspend(ctx context.Context, a Actor, in SuspendInput) (*RequestResult, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationSuspend, in.Subject, true); err != nil {
		return nil, err
	}
	if !reasonAllowed(requestReasons, ActionSuspend, in.ReasonCode) {
		return nil, fmt.Errorf("%w: reason code %q is not in the closed catalogue for suspend", ErrInvalidInput, in.ReasonCode)
	}
	if err := LintFreeText("conditions_note", in.ConditionsNote, maxConditionsNote); err != nil {
		return nil, err
	}
	if err := otherDocumentedNeedsNote(in.ReasonCode, in.ConditionsNote); err != nil {
		return nil, err
	}
	// Informational readiness (R-PLAYER-FUNDS). A failed evaluation must never delay a
	// suspension: it degrades to a not_evaluable check.
	as, err := s.assess(ctx, EvalInput{Subject: in.Subject, Action: ActionSuspend})
	if err != nil {
		if errors.Is(err, ErrNotFound) || errors.Is(err, ErrInvalidInput) {
			return nil, err
		}
		as = assessment{Readiness: Readiness{Version: SnapshotVersion, ComputedAt: s.now(), Environment: s.cfg.Environment,
			Checks: []Check{ck(CheckPlayerFunds, ResultNotEvaluable, "read_error")}, Info: map[string]int64{}}}
	}
	for _, f := range as.Failures {
		if f == H1SubjectClosed {
			return nil, s.denied(ctx, a, "suspend", in.Subject, ActionSuspend, ErrSubjectClosed)
		}
		if f == H1IllegalTransition {
			return nil, s.denied(ctx, a, "suspend", in.Subject, ActionSuspend, ErrIllegalTransition)
		}
	}
	snap, err := as.Readiness.SnapshotJSON()
	if err != nil {
		return nil, err
	}
	snapHash := as.Readiness.Hash()
	ri := RequestInput{Subject: in.Subject, Action: ActionSuspend, ReasonCode: in.ReasonCode, ConditionsNote: in.ConditionsNote}
	digest := digestFor(in.Subject, ri, snapHash, nil, "").Digest()

	var lastErr error
	attempts := 0
	for attempt := 1; attempt <= s.cfg.SuspendAttempts; attempt++ {
		attempts = attempt
		res, err := s.suspendOnce(ctx, a, in, ri, string(snap), snapHash, digest)
		if err == nil {
			res.Readiness = as.Readiness
			return res, nil
		}
		lastErr = err
		// A lock timeout / deadlock, or the C-3(b) commit check LA030 (a request committed by a
		// concurrent transaction between this suspension's pending-request read and its commit: the
		// next attempt reads it and supersedes it), is retried against a fresh transaction.
		if !isRetryableLock(err) && !isStaleCommitCheck(err) {
			break
		}
		if attempt < s.cfg.SuspendAttempts {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(s.cfg.Backoff(attempt)):
			}
		}
	}
	err = classify(lastErr)
	if errors.Is(err, ErrProofUnavailable) || errors.Is(err, ErrIsolationLevel) || errors.Is(err, ErrNotFound) {
		return nil, err
	}
	if isRetryableLock(lastErr) || isStaleCommitCheck(lastErr) {
		// All attempts were consumed by lock contention: a suspension (the fail-closed direction)
		// did not happen. This is a P1 condition (security C4), raised on its own detached path.
		s.raiseSuspensionExhausted(ctx, a, in.Subject, attempts)
	}
	return nil, s.denied(ctx, a, "suspend", in.Subject, ActionSuspend, err, withExtra(map[string]any{"attempts": attempts}))
}

// isStaleCommitCheck reports the C-3(b) deferred commit check (LA030).
func isStaleCommitCheck(err error) bool {
	pe := pgErr(err)
	return pe != nil && pe.Code == "LA030"
}

func (s *Service) suspendOnce(ctx context.Context, a Actor, in SuspendInput, ri RequestInput, snap, snapHash, digest string) (*RequestResult, error) {
	var out Request
	var superseded []uuid.UUID
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		superseded = nil
		if err := s.attachProof(ctx, tx, a, actorproof.OpLaunchSuspend, "new", digest); err != nil {
			return err
		}
		var req Request
		if err := scanRequest(tx.QueryRow(ctx, `
			INSERT INTO launch_authorisation_requests
			    (tenant_id, subject_kind, brand_id, action, reason_code, readiness_snapshot, readiness_snapshot_hash, conditions_note)
			VALUES ($1, $2, $3, 'suspend', $4, $5::jsonb, $6, $7)
			RETURNING `+requestColumns,
			in.Subject.TenantID, in.Subject.Kind(), in.Subject.BrandID, in.ReasonCode, snap, snapHash, in.ConditionsNote), &req); err != nil {
			return err
		}
		// Lock the other pending requests of the subject FIRST (id order), then the subject: the
		// same order an approval takes (its request row, then the subject), so no cycle.
		rows, err := tx.Query(ctx, `SELECT id FROM launch_authorisation_requests
			WHERE tenant_id = $1 AND subject_kind = $2 AND brand_id IS NOT DISTINCT FROM $3 AND status = 'pending' AND id <> $4
			ORDER BY id FOR UPDATE`, req.TenantID, req.SubjectKind, req.BrandID, req.ID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
		if err != nil {
			return err
		}
		if err := scanRequest(tx.QueryRow(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1 RETURNING `+requestColumns, req.ID), &req); err != nil {
			return err
		}
		status, err := s.lockSubject(ctx, tx, a, req.Subject())
		if err != nil {
			return err
		}
		if status != req.FromStatus {
			return ErrIllegalTransition
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
			VALUES ($1, $2, $3, 'governed', $4, $5, $6)`,
			req.TenantID, req.SubjectKind, req.BrandID, req.FromStatus, req.ToStatus, req.ID); err != nil {
			return err
		}
		if err := s.updateSubjectStatus(ctx, tx, a, req.Subject(), req.ToStatus); err != nil {
			return err
		}
		if err := scanRequest(tx.QueryRow(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1 RETURNING `+requestColumns, req.ID), &out); err != nil {
			return err
		}
		// S3 / C-3(b): every other pending request of the subject is superseded in this transaction;
		// the deferred commit check (migration 0130) fails the COMMIT if one survives.
		for _, id := range ids {
			var sup Request
			if err := scanRequest(tx.QueryRow(ctx, `UPDATE launch_authorisation_requests SET status = 'superseded' WHERE id = $1 RETURNING `+requestColumns, id), &sup); err != nil {
				return err
			}
			superseded = append(superseded, id)
			if err := s.auditEvent(ctx, tx, a, "superseded", sup, map[string]any{
				"superseded_by_request_id": out.ID.String(), "superseded_by_action": ActionSuspend, "superseded_by": a.StaffID.String(),
				"superseded_by_scope": a.Scope,
			}); err != nil {
				return err
			}
		}
		supIDs := make([]string, 0, len(superseded))
		for _, id := range superseded {
			supIDs = append(supIDs, id.String())
		}
		return s.auditEvent(ctx, tx, a, eventSuspended, out, map[string]any{
			"before": out.FromStatus, "after": out.ToStatus, "executed_by": a.StaffID.String(),
			"superseded_request_ids": supIDs, "readiness_snapshot_hash": snapHash,
		})
	})
	if err != nil {
		return nil, err
	}
	return &RequestResult{Request: out, Executed: true, Superseded: superseded}, nil
}
