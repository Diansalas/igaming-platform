package kyc

// PRH-2 E1 (ADR 0106 section 7.2, identity-compliance C1/R1/R2): the
// STAFF-visible, read-only, derived submission state of a verification. It
// answers "where is this verification's vendor submission?" for the compliance
// console, so a stranded submission (failed_terminal, or a submit cancelled
// because the provider was de-configured) is visible. It is NOT part of the
// Verification struct, is computed by functions defined only here, and is
// called only by staff handlers: enforcement, payments and withdrawal never
// call it (INV-KYC-OB-5, a static test pins this). It exposes no vendor
// reference, document data or counters: only a closed state and, when failed or
// cancelled, the closed error class or cancel reason.

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// SubmissionStateValue is the derived staff-visible state.
type SubmissionStateValue string

const (
	SubmissionNone      SubmissionStateValue = "none"
	SubmissionQueued    SubmissionStateValue = "queued"
	SubmissionSent      SubmissionStateValue = "sent"
	SubmissionFailed    SubmissionStateValue = "failed"
	SubmissionCancelled SubmissionStateValue = "cancelled"
)

// SubmissionState is the derived state plus, for failed or cancelled, the
// closed class or reason.
type SubmissionState struct {
	State          SubmissionStateValue
	LastErrorClass string
	CancelReason   string
}

// latestRow is a verification's latest create or submit outbox row.
type latestRow struct {
	state          OutboxState
	lastErrorClass string
	cancelReason   string
}

// benignCancel reports whether a latest-row cancellation is benign (no stranded
// submission): superseded, no_documents, and document_set_changed when a
// replacement (any live or sent submit row of the verification) exists.
func benignCancel(reason string, replacementExists bool) bool {
	switch OutboxCancelReason(reason) {
	case CancelSuperseded, CancelNoDocuments:
		return true
	case CancelDocumentSetChanged:
		return replacementExists
	}
	return false
}

// deriveSubmissionState applies the ADR 0106 section 7.2 precedence with the
// identity-compliance R2 correction, over the verification's latest create row
// and latest submit row (by (created_at, id)):
//
//  1. failed      if either latest row is failed_terminal;
//  2. cancelled   if either latest row is cancelled for a NON-benign reason
//     (the stranded case, e.g. create sent and the latest submit cancelled
//     provider_deconfigured), shown with its cancel_reason;
//  3. queued      if either latest row is pending or claimed;
//  4. sent        if either latest row is sent;
//  5. cancelled   if only benign cancellations exist;
//  6. none        if no row exists.
func deriveSubmissionState(create, submit *latestRow, replacementExists bool) SubmissionState {
	rows := []*latestRow{create, submit}
	for _, r := range rows {
		if r != nil && r.state == OutboxFailedTerminal {
			return SubmissionState{State: SubmissionFailed, LastErrorClass: r.lastErrorClass}
		}
	}
	for _, r := range rows {
		if r != nil && r.state == OutboxCancelled && !benignCancel(r.cancelReason, replacementExists) {
			return SubmissionState{State: SubmissionCancelled, CancelReason: r.cancelReason}
		}
	}
	for _, r := range rows {
		if r != nil && (r.state == OutboxPending || r.state == OutboxClaimed) {
			return SubmissionState{State: SubmissionQueued}
		}
	}
	for _, r := range rows {
		if r != nil && r.state == OutboxSent {
			return SubmissionState{State: SubmissionSent}
		}
	}
	for _, r := range rows {
		if r != nil && r.state == OutboxCancelled {
			return SubmissionState{State: SubmissionCancelled, CancelReason: r.cancelReason}
		}
	}
	return SubmissionState{State: SubmissionNone}
}

// StaffSubmissionStates returns the derived submission state of each
// verification id, read under the staff principal's tenant RLS
// (kso_tenant_select; a staff-principal session may read but never write the
// outbox). STAFF HANDLERS ONLY: no enforcement, payments or withdrawal code may
// call it. Verifications with no outbox row map to state none.
func StaffSubmissionStates(ctx context.Context, tx pgx.Tx, verificationIDs []uuid.UUID) (map[uuid.UUID]SubmissionState, error) {
	out := make(map[uuid.UUID]SubmissionState, len(verificationIDs))
	for _, id := range verificationIDs {
		out[id] = SubmissionState{State: SubmissionNone}
	}
	if len(verificationIDs) == 0 {
		return out, nil
	}
	rows, err := tx.Query(ctx,
		`SELECT DISTINCT ON (verification_id, operation)
		        verification_id, operation, state, COALESCE(last_error_class, ''), COALESCE(cancel_reason, '')
		   FROM kyc_submission_outbox
		  WHERE verification_id = ANY ($1::uuid[])
		  ORDER BY verification_id, operation, created_at DESC, id DESC`,
		verificationIDs)
	if err != nil {
		return nil, fmt.Errorf("kyc: read submission state: %w", err)
	}
	type pair struct{ create, submit *latestRow }
	latest := map[uuid.UUID]*pair{}
	for rows.Next() {
		var vid uuid.UUID
		var op, state, class, reason string
		if err := rows.Scan(&vid, &op, &state, &class, &reason); err != nil {
			rows.Close()
			return nil, fmt.Errorf("kyc: scan submission state: %w", err)
		}
		p := latest[vid]
		if p == nil {
			p = &pair{}
			latest[vid] = p
		}
		lr := &latestRow{state: OutboxState(state), lastErrorClass: class, cancelReason: reason}
		if OutboxOperation(op) == OpCreate {
			p.create = lr
		} else {
			p.submit = lr
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("kyc: read submission state: %w", err)
	}
	// "document_set_changed with a replacement enqueued": any live or sent
	// submit row of the verification.
	replacement := map[uuid.UUID]bool{}
	rrows, err := tx.Query(ctx,
		`SELECT DISTINCT verification_id FROM kyc_submission_outbox
		  WHERE verification_id = ANY ($1::uuid[]) AND operation = 'submit' AND state IN ('pending', 'claimed', 'sent')`,
		verificationIDs)
	if err != nil {
		return nil, fmt.Errorf("kyc: read submission replacement: %w", err)
	}
	for rrows.Next() {
		var vid uuid.UUID
		if err := rrows.Scan(&vid); err != nil {
			rrows.Close()
			return nil, fmt.Errorf("kyc: scan submission replacement: %w", err)
		}
		replacement[vid] = true
	}
	rrows.Close()
	if err := rrows.Err(); err != nil {
		return nil, fmt.Errorf("kyc: read submission replacement: %w", err)
	}
	for vid, p := range latest {
		out[vid] = deriveSubmissionState(p.create, p.submit, replacement[vid])
	}
	return out, nil
}

// StaffSubmissionState is StaffSubmissionStates for one verification. STAFF
// HANDLERS ONLY.
func StaffSubmissionState(ctx context.Context, tx pgx.Tx, verificationID uuid.UUID) (SubmissionState, error) {
	m, err := StaffSubmissionStates(ctx, tx, []uuid.UUID{verificationID})
	if err != nil {
		return SubmissionState{}, err
	}
	return m[verificationID], nil
}
