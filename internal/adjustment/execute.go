package adjustment

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// Decision is an approval decision.
type Decision string

const (
	DecisionApprove Decision = "approve"
	DecisionReject  Decision = "reject"
)

// DecisionInput is what a decider supplies: the payload hash it reviewed
// (LF-10: an approval pins exactly the payload it saw) and a reason code.
type DecisionInput struct {
	Decision    Decision
	PayloadHash string
	ReasonCode  string
}

// Outcome is the result of a decision.
type Outcome struct {
	Request    Request
	ApprovalID uuid.UUID
	// Executed is true when this decision was the final approval and the
	// posting was made in its own transaction.
	Executed bool
	// Counted/Required describe the approval count after this decision.
	Counted  int
	Required int
}

// executionStatus is migration 0113's ledger_adjustment_execution_status():
// the ONE counting implementation (§6.2), shared with the -> executing
// guard. There is no Go re-implementation of the rules.
type executionStatus struct {
	Required       int
	Counted        int
	CountedIDs     []uuid.UUID
	InitiatorValid bool
	Contributing   []uuid.UUID
	TenantStatus   *string
	Enabled        bool
}

func readExecutionStatus(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) (executionStatus, error) {
	var st executionStatus
	err := tx.QueryRow(ctx, `SELECT required, counted, counted_approval_ids, initiator_valid, contributing_policy_ids, tenant_status, enabled
		FROM ledger_adjustment_execution_status($1)`, requestID).
		Scan(&st.Required, &st.Counted, &st.CountedIDs, &st.InitiatorValid, &st.Contributing, &st.TenantStatus, &st.Enabled)
	return st, err
}

// testHookAfterShareLocks, when set by an in-package test, runs after the
// L1 staff/grant FOR SHARE locks are held and the recount is done, before
// anything else - so B-7 can race a revoke against a real execution.
var testHookAfterShareLocks func(ctx context.Context, requestID uuid.UUID)

// testHookBeforePost, when set by an in-package test, runs after the
// request is 'executing' and L3 is held, just before ledger.Post - B-15's
// crash-between-steps-7-and-10 injection point.
var testHookBeforePost func(ctx context.Context, requestID uuid.UUID) error

// DecideInTx records a decision and, when it is the final approval that
// brings the counted approvals to the required number, executes the
// request in THIS transaction (ADR 0100 §6.5; LF-13: no approved-but-
// unexecuted window). Lock order is ADR 0082 Amendment A8:
// request (L1, FOR UPDATE) -> staff (L1, FOR SHARE, ascending id) ->
// grants (L1, FOR SHARE, ascending id) -> causation ledger_transactions
// (L2, FOR UPDATE) -> projections (L3, LockProjectionsForPosting) ->
// ledger.Post (L4). The approval insert happens while the L1 request row
// is held and belongs to no lock class.
func DecideInTx(ctx context.Context, tx pgx.Tx, call Call, requestID uuid.UUID, in DecisionInput) (Outcome, error) {
	if in.Decision != DecisionApprove && in.Decision != DecisionReject {
		return Outcome{}, fmt.Errorf("%w: decision must be approve or reject", ErrInvalidInput)
	}
	if !hex64.MatchString(in.PayloadHash) {
		return Outcome{}, fmt.Errorf("%w: payload_hash must be 64 lowercase hex characters", ErrInvalidInput)
	}
	if len(in.ReasonCode) < 1 || len(in.ReasonCode) > 64 {
		return Outcome{}, fmt.Errorf("%w: reason_code must be 1-64 bytes", ErrInvalidInput)
	}

	// Step 1 - L1: the request row, FOR UPDATE. Final approvals serialize
	// here; the idempotency key is the backstop.
	req, err := lockRequest(ctx, tx, call.TenantID, requestID)
	if err != nil {
		return Outcome{}, err
	}
	if req.State != StatePending {
		return Outcome{Request: req}, ErrNotPending
	}
	var expired bool
	if err := tx.QueryRow(ctx, `SELECT now() >= $1::timestamptz`, req.ExpiresAt).Scan(&expired); err != nil {
		return Outcome{}, err
	}
	if expired {
		after, err := setState(ctx, tx, requestID, StateExpired, nil)
		if err != nil {
			return Outcome{}, err
		}
		before := req.State
		return Outcome{Request: after}, recordAudit(ctx, tx, call, "ledger_adjustment.expired", after, &before, nil)
	}

	// Step 2 - the approval insert (migration 0113 triggers: payload hash,
	// in-force grant, distinct Person, beneficiary, S-2(iii), pending).
	var approvalID uuid.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO ledger_adjustment_approvals
			(tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
		VALUES ($1, $2, $3, $4, $5, 'tenant', $5, 0, $6)
		RETURNING id`,
		call.TenantID, requestID, string(in.Decision), in.PayloadHash, uuid.Nil, in.ReasonCode).Scan(&approvalID); err != nil {
		return Outcome{}, err
	}
	out := Outcome{ApprovalID: approvalID}

	if in.Decision == DecisionReject {
		after, err := GetInTx(ctx, tx, call.TenantID, requestID)
		if err != nil {
			return Outcome{}, err
		}
		out.Request = after
		before := req.State
		return out, recordAudit(ctx, tx, call, "ledger_adjustment.rejected", after, &before, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
		})
	}

	// Step 3 - evaluate required (§3.6) and the candidate count.
	st, err := readExecutionStatus(ctx, tx, requestID)
	if err != nil {
		return Outcome{}, err
	}
	out.Counted, out.Required = st.Counted, st.Required
	if !st.InitiatorValid || st.Counted < st.Required {
		out.Request = req
		return out, recordAudit(ctx, tx, call, "ledger_adjustment.approved", req, nil, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
			"counted": st.Counted, "required": st.Required, "initiator_valid": st.InitiatorValid,
		})
	}

	// Step 4 - L1 continued: staff rows (initiator + candidate approvers)
	// then their grants IN FORCE AT now(), both FOR SHARE, ascending id
	// (ADR 0099 §7.4, LF F3, architect I-5 / R-12 ruling: never "the
	// unrevoked grant"). Then recount under the locks.
	if err := lockStaffAndGrants(ctx, tx, req); err != nil {
		return Outcome{}, err
	}
	st, err = readExecutionStatus(ctx, tx, requestID)
	if err != nil {
		return Outcome{}, err
	}
	out.Counted, out.Required = st.Counted, st.Required
	if testHookAfterShareLocks != nil {
		testHookAfterShareLocks(ctx, requestID)
	}
	if !st.InitiatorValid || st.Counted < st.Required {
		out.Request = req
		return out, recordAudit(ctx, tx, call, "ledger_adjustment.approved", req, nil, map[string]any{
			"approval_id": approvalID.String(), "decision_reason_code": in.ReasonCode,
			"counted": st.Counted, "required": st.Required, "initiator_valid": st.InitiatorValid,
		})
	}

	// Step 5 - L2: the causation row, FOR UPDATE (serializes the §5.4
	// cumulative compensation cap across concurrent requests).
	if req.CausationTransactionID != nil {
		var locked uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM ledger_transactions WHERE id = $1 AND tenant_id = $2 FOR UPDATE`,
			*req.CausationTransactionID, req.TenantID).Scan(&locked); err != nil {
			return Outcome{}, fmt.Errorf("adjustment: lock causation transaction: %w", err)
		}
	}

	// Step 6 - the §5.2 execution re-checks (exposure, asset, tenant
	// status, compensation cap), via the same function as submission.
	refusal := ""
	if !st.Enabled {
		refusal = "MA014:policy_disabled"
	} else {
		var r *string
		if err := tx.QueryRow(ctx, `SELECT ledger_adjustment_payload_refusal($1, $2, $3, $4, $5, $6, $7::numeric, $8, $9, $10, $11)`,
			req.ID, req.TenantID, req.WalletID, req.PlayerAccountID, req.AssetCode, string(req.Direction), fmt.Sprint(req.Amount),
			string(req.ReasonCode), req.CausationTransactionID, req.EvidenceRefHash, st.TenantStatus).Scan(&r); err != nil {
			return Outcome{}, err
		}
		if r != nil {
			refusal = *r
		}
	}
	if refusal != "" {
		code := refusalCode(refusal)
		after, err := setState(ctx, tx, requestID, StateRefusedAtExecution, &code)
		if err != nil {
			return Outcome{}, err
		}
		out.Request = after
		before := req.State
		return out, recordAudit(ctx, tx, call, "ledger_adjustment.refused_at_execution", after, &before, map[string]any{
			"approval_id": approvalID.String(), "counted_approval_ids": uuidStrings(st.CountedIDs),
		})
	}

	// Step 7 - executing, executed_txid = txid_current() (forced by the
	// guard, which re-verifies the count and the payload rules).
	if _, err := setState(ctx, tx, requestID, StateExecuting, nil); err != nil {
		return Outcome{}, err
	}

	// Build the exact §4 posting. Account creation precedes L3 (ADR 0082
	// §3.2), in GetOrCreateAccounts' canonical order.
	walletID := req.WalletID
	ids, err := ledger.GetOrCreateAccounts(ctx, tx, req.TenantID,
		ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerCash, AssetCode: req.AssetCode},
		ledger.AccountSpec{WalletID: nil, AccountType: ledger.AccountManualAdjustment, AssetCode: req.AssetCode},
	)
	if err != nil {
		return Outcome{}, err
	}
	playerCash, house := ids[0], ids[1]
	in2 := buildPosting(req, playerCash, house)

	// Step 8 - L3 and the §6.4 sufficiency check, with the SAME input.
	locked, err := ledger.LockProjectionsForPosting(ctx, tx, in2)
	if err != nil {
		return Outcome{}, err
	}
	if req.Direction == DirectionDebitPlayer {
		bal, err := locked.Balance(playerCash)
		if err != nil {
			return Outcome{}, err
		}
		if bal.Signed() < req.Amount {
			after, err := setState(ctx, tx, requestID, StateRefusedInsufficientFunds, nil)
			if err != nil {
				return Outcome{}, err
			}
			out.Request = after
			before := req.State
			return out, recordAudit(ctx, tx, call, "ledger_adjustment.refused_insufficient_funds", after, &before, map[string]any{
				"approval_id": approvalID.String(), "counted_approval_ids": uuidStrings(st.CountedIDs),
			})
		}
	}
	if testHookBeforePost != nil {
		if err := testHookBeforePost(ctx, requestID); err != nil {
			return Outcome{}, err
		}
	}

	// Step 9 - L4.
	res, err := ledger.Post(ctx, tx, in2)
	if err != nil {
		return Outcome{}, err
	}
	if res.AlreadyPosted {
		// A pending request's key can never have been posted: its only
		// poster is this step, which ends the request. Fail closed.
		return Outcome{}, fmt.Errorf("%w: idempotency key %s already posted for a pending request", ErrIntegrity, in2.IdempotencyKey)
	}

	// Step 10 - executed + link (the trigger verifies the §4 shape).
	var after Request
	if err := scanRequest(tx.QueryRow(ctx, `UPDATE ledger_adjustment_requests SET state = 'executed', ledger_transaction_id = $2
		WHERE id = $1 RETURNING `+requestColumns, requestID, res.TransactionID), &after); err != nil {
		return Outcome{}, err
	}
	out.Request = after
	out.Executed = true
	before := req.State
	return out, recordAudit(ctx, tx, call, "ledger_adjustment.executed", after, &before, map[string]any{
		"approval_id": approvalID.String(), "counted_approval_ids": uuidStrings(st.CountedIDs),
		"required_at_execution": st.Required, "contributing_policy_ids_at_execution": uuidStrings(st.Contributing),
	})
}

// refusalCode strips the SQLSTATE prefix from "<SQLSTATE>:<code>".
func refusalCode(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[i+1:]
		}
	}
	return s
}

// buildPosting is the ONLY TransactionInput this package builds (ADR 0100
// §4, §5.3): exactly two entries, one amount, one asset, one tenant,
// transaction_type manual_adjustment, keyed on the request.
func buildPosting(req Request, playerCash, house uuid.UUID) ledger.TransactionInput {
	reason := string(req.ReasonCode)
	playerDir, houseDir := ledger.Credit, ledger.Debit
	if req.Direction == DirectionDebitPlayer {
		playerDir, houseDir = ledger.Debit, ledger.Credit
	}
	return ledger.TransactionInput{
		TenantID:        req.TenantID,
		TransactionType: ledger.TxManualAdjustment,
		IdempotencyKey:  "manual_adjustment:" + req.ID.String(),
		CorrelationID:   req.ID,
		CausationID:     req.CausationTransactionID,
		ReasonCode:      &reason,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: house, Direction: houseDir, Amount: req.Amount},
			{LedgerAccountID: playerCash, Direction: playerDir, Amount: req.Amount},
		},
	}
}

// lockStaffAndGrants takes the A8 L1 FOR SHARE locks: staff_users rows of
// the initiator and every candidate approver (ascending id), then their
// grants in force at now() (ascending id). A-19 column discipline: only
// id, tenant_id, role, status, person_id are ever selected from
// staff_users. A concurrent revoke either committed first (and is re-read
// as revoked by the recount) or waits for this transaction.
func lockStaffAndGrants(ctx context.Context, tx pgx.Tx, req Request) error {
	var approvers []uuid.UUID
	rows, err := tx.Query(ctx, `SELECT decided_by FROM ledger_adjustment_approvals
		WHERE request_id = $1 AND decision = 'approve' AND payload_hash = $2 ORDER BY decided_by`, req.ID, req.PayloadHash)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		approvers = append(approvers, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	staff := append([]uuid.UUID{req.InitiatedBy}, approvers...)
	if err := drain(tx.Query(ctx, `SELECT id, tenant_id, role, status, person_id FROM staff_users
		WHERE id = ANY($1) ORDER BY id FOR SHARE`, staff)); err != nil {
		return fmt.Errorf("adjustment: lock staff rows: %w", err)
	}
	if err := drain(tx.Query(ctx, `SELECT id FROM staff_capability_grants
		WHERE tenant_id = $1 AND revoked_at IS NULL AND valid_from <= now() AND (valid_until IS NULL OR now() < valid_until)
		  AND ((grantee_staff_id = $2 AND capability = 'ledger_adjustment:initiate')
		    OR (grantee_staff_id = ANY($3) AND capability = 'ledger_adjustment:approve'))
		ORDER BY id FOR SHARE`, req.TenantID, req.InitiatedBy, approvers)); err != nil {
		return fmt.Errorf("adjustment: lock grants: %w", err)
	}
	return nil
}

// drain reads every row of a locking query (so every row lock is taken)
// and discards the values.
func drain(rows pgx.Rows, err error) error {
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
	}
	return rows.Err()
}

// Decide opens the caller's session and decides.
func (s *Service) Decide(ctx context.Context, target Target, requestID uuid.UUID, in DecisionInput, meta Meta) (Outcome, error) {
	var out Outcome
	err := s.runSession(ctx, target, requestID, meta, func(ctx context.Context, tx pgx.Tx, call Call) error {
		var err error
		out, err = DecideInTx(ctx, tx, call, requestID, in)
		return err
	})
	return out, err
}
