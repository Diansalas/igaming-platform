package aml

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
)

// CaseKind is how a case came to exist (S3: an officer opened it, with or without alerts).
type CaseKind string

const (
	CaseKindManual CaseKind = "manual"
	CaseKindAlert  CaseKind = "alert"
)

// CaseStatus is a case's projected status (ADR 0116 s3.1).
type CaseStatus string

const (
	StatusOpen                CaseStatus = "open"
	StatusInReview            CaseStatus = "in_review"
	StatusEscalated           CaseStatus = "escalated"
	StatusSARDecisionPending  CaseStatus = "sar_decision_pending"
	StatusClosedNoAction      CaseStatus = "closed_no_action"
	StatusClosedSARNotFiled   CaseStatus = "closed_sar_not_filed"
	StatusClosedSARFiled      CaseStatus = "closed_sar_filed"
	proposalKindCloseNotFiled            = "close_sar_not_filed"
	proposalKindCloseFiled               = "close_sar_filed"
	proposalKindLiftHold                 = "lift_hold"
)

// ProposalKind is what a four-eyes proposal asks the approver to authorise.
type ProposalKind string

const (
	ProposeCloseSARNotFiled ProposalKind = proposalKindCloseNotFiled
	ProposeCloseSARFiled    ProposalKind = proposalKindCloseFiled
	ProposeLiftHold         ProposalKind = proposalKindLiftHold
)

// AlertSeverity of an officer-raised alert.
type AlertSeverity string

const (
	SeverityLow    AlertSeverity = "low"
	SeverityMedium AlertSeverity = "medium"
	SeverityHigh   AlertSeverity = "high"
)

// Case is the projected state of a case.
type Case struct {
	ID         uuid.UUID
	OwnerScope string
	PersonID   uuid.UUID
	Kind       CaseKind
	Status     CaseStatus
	SARFlagged bool
	// LastSeq is the case's last event sequence: the value every state-changing call must present (CAS).
	LastSeq  int
	OpenedAt time.Time
}

// CaseEvent is one row of the case's append-only audit trail.
type CaseEvent struct {
	Seq                     int
	Type                    string
	FromStatus, ToStatus    *string
	ProposalKind            *string
	ProposalSeq             *int
	ContentHash             *string
	SubjectHoldID           *uuid.UUID
	NoteID                  *uuid.UUID
	ExternalFilingReference *string
	ReasonCode              string
	ActorID                 uuid.UUID
	ActorScope              string
	CreatedAt               time.Time
}

// CaseNote is one stored note.
type CaseNote struct {
	ID          uuid.UUID
	Body        string
	LintVersion string
	CreatedBy   uuid.UUID
	CreatedAt   time.Time
}

// QueueItem is one row of the case queue.
type QueueItem struct {
	CaseID     uuid.UUID
	OwnerScope string
	PersonID   uuid.UUID
	Status     CaseStatus
	SARFlagged bool
	LastSeq    int
	OpenedAt   time.Time
}

// Proposal is what Propose returns: the sequence of the proposal event and the content hash the approver must bind.
type Proposal struct {
	Seq         int
	ContentHash string
}

// ProposeInput is the proposal half of a four-eyes action.
type ProposeInput struct {
	Kind ProposalKind
	// HoldID: required for ProposeLiftHold, absent otherwise.
	HoldID uuid.UUID
	// ExternalFilingReference: required for ProposeCloseSARFiled (the human-entered filing reference, linted), absent otherwise.
	ExternalFilingReference string
	ReasonCode              string
}

func (in ProposeInput) validate() error {
	switch in.Kind {
	case ProposeLiftHold:
		if in.HoldID == uuid.Nil || in.ExternalFilingReference != "" {
			return fmt.Errorf("%w: a hold lift proposal names a hold and no filing reference", ErrInvalidInput)
		}
	case ProposeCloseSARFiled:
		if in.HoldID != uuid.Nil || in.ExternalFilingReference == "" {
			return fmt.Errorf("%w: closing as filed needs the external filing reference", ErrInvalidInput)
		}
		if err := LintFilingReference(in.ExternalFilingReference); err != nil {
			return err
		}
	case ProposeCloseSARNotFiled:
		if in.HoldID != uuid.Nil || in.ExternalFilingReference != "" {
			return fmt.Errorf("%w: closing as not filed takes neither a hold nor a filing reference", ErrInvalidInput)
		}
	default:
		return fmt.Errorf("%w: proposal kind %q", ErrInvalidInput, in.Kind)
	}
	if !ValidReasonCode(in.ReasonCode) {
		return fmt.Errorf("%w: reason code", ErrInvalidInput)
	}
	return nil
}

// ProposalContentHash is the hash an approver reviews and binds to (migration 0142 aml_proposal_content_hash). The
// database recomputes it from the stored proposal and refuses a mismatch; this mirror lets an approver or a test check
// it independently.
func ProposalContentHash(tenant, caseID uuid.UUID, seq int, kind ProposalKind, hold *uuid.UUID, ref, reason string, proposer uuid.UUID) string {
	return actorproof.Digest(actorproof.S(tenant.String()), actorproof.S(caseID.String()), actorproof.S(strconv.Itoa(seq)),
		actorproof.S(string(kind)), actorproof.UUIDP(hold), actorproof.SP(ref), actorproof.S(reason), actorproof.S(proposer.String()))
}

func joinSortedUUIDs(ids []uuid.UUID) *string {
	if len(ids) == 0 {
		return nil
	}
	ss := make([]string, len(ids))
	for i, id := range ids {
		ss[i] = id.String()
	}
	sort.Strings(ss)
	j := strings.Join(ss, ",")
	return &j
}

func uuidPtr(id uuid.UUID) *uuid.UUID {
	if id == uuid.Nil {
		return nil
	}
	return &id
}

// RaiseAlert records an officer-raised alert on a person (S3; monitoring and screening alerts are S4 / S2, NOT IMPLEMENTED).
func (s *Service) RaiseAlert(ctx context.Context, a Actor, personID uuid.UUID, severity AlertSeverity, reasonCode string) (uuid.UUID, error) {
	if personID == uuid.Nil || !ValidReasonCode(reasonCode) {
		return uuid.Nil, fmt.Errorf("%w: person id and reason code are required", ErrInvalidInput)
	}
	switch severity {
	case SeverityLow, SeverityMedium, SeverityHigh:
	default:
		return uuid.Nil, fmt.Errorf("%w: severity %q", ErrInvalidInput, severity)
	}
	var id uuid.UUID
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(personID.String()), actorproof.S(string(severity)), actorproof.S(reasonCode))
		if err := s.attachProof(ctx, tx, a, OpProofAlertRaise, actorproof.TargetNew, hash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT aml_alert_raise($1, $2, $3, $4)`, a.TenantID, personID, string(severity), reasonCode).Scan(&id)
	})
	return id, err
}

// OpenCase opens a case on a person. An `alert` case names at least one alert of the same person; a `manual` case none.
func (s *Service) OpenCase(ctx context.Context, a Actor, personID uuid.UUID, kind CaseKind, reasonCode string, alertIDs []uuid.UUID) (uuid.UUID, error) {
	if personID == uuid.Nil || !ValidReasonCode(reasonCode) || (kind != CaseKindManual && kind != CaseKindAlert) {
		return uuid.Nil, fmt.Errorf("%w: person id, case kind and reason code are required", ErrInvalidInput)
	}
	var id uuid.UUID
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(personID.String()), actorproof.S(string(kind)), actorproof.S(reasonCode), joinSortedUUIDs(alertIDs))
		if err := s.attachProof(ctx, tx, a, OpProofCaseOpen, actorproof.TargetNew, hash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT aml_case_open($1, $2, $3, $4, $5)`, a.TenantID, personID, string(kind), reasonCode, alertIDs).Scan(&id)
	})
	return id, err
}

// ReadCase returns the projected state of a case in the actor's scope (ErrNotFound otherwise).
func (s *Service) ReadCase(ctx context.Context, a Actor, caseID uuid.UUID) (Case, error) {
	var c Case
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(caseID.String()), actorproof.S("case"))
		if err := s.attachProof(ctx, tx, a, OpProofCaseRead, caseID.String(), hash); err != nil {
			return err
		}
		var kind, status string
		if err := tx.QueryRow(ctx, `SELECT o_case_id, o_owner_scope, o_person_id, o_case_kind, o_status, o_sar_flagged, o_last_seq, o_opened_at
			FROM aml_case_read($1, $2)`, a.TenantID, caseID).
			Scan(&c.ID, &c.OwnerScope, &c.PersonID, &kind, &status, &c.SARFlagged, &c.LastSeq, &c.OpenedAt); err != nil {
			return err
		}
		c.Kind, c.Status = CaseKind(kind), CaseStatus(status)
		return nil
	})
	return c, err
}

// CaseEvents returns the case's audit trail in sequence order.
func (s *Service) CaseEvents(ctx context.Context, a Actor, caseID uuid.UUID) ([]CaseEvent, error) {
	var out []CaseEvent
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(caseID.String()), actorproof.S("events"))
		if err := s.attachProof(ctx, tx, a, OpProofCaseRead, caseID.String(), hash); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT o_seq, o_event_type, o_from_status, o_to_status, o_proposal_kind, o_proposal_seq, o_content_hash,
			o_subject_hold_id, o_note_id, o_external_filing_reference, o_reason_code, o_actor_id, o_actor_scope, o_created_at
			FROM aml_case_events_list($1, $2)`, a.TenantID, caseID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e CaseEvent
			if err := rows.Scan(&e.Seq, &e.Type, &e.FromStatus, &e.ToStatus, &e.ProposalKind, &e.ProposalSeq, &e.ContentHash, &e.SubjectHoldID,
				&e.NoteID, &e.ExternalFilingReference, &e.ReasonCode, &e.ActorID, &e.ActorScope, &e.CreatedAt); err != nil {
				return err
			}
			out = append(out, e)
		}
		return rows.Err()
	})
	return out, err
}

// CaseNotes returns the case's notes in creation order.
func (s *Service) CaseNotes(ctx context.Context, a Actor, caseID uuid.UUID) ([]CaseNote, error) {
	var out []CaseNote
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(caseID.String()), actorproof.S("notes"))
		if err := s.attachProof(ctx, tx, a, OpProofCaseRead, caseID.String(), hash); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT o_note_id, o_body, o_lint_version, o_created_by, o_created_at FROM aml_case_notes_list($1, $2)`, a.TenantID, caseID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n CaseNote
			if err := rows.Scan(&n.ID, &n.Body, &n.LintVersion, &n.CreatedBy, &n.CreatedAt); err != nil {
				return err
			}
			out = append(out, n)
		}
		return rows.Err()
	})
	return out, err
}

// CaseQueue returns the cases the actor's scope may read, newest first (limit 1..100).
func (s *Service) CaseQueue(ctx context.Context, a Actor, limit int) ([]QueueItem, error) {
	if limit < 1 || limit > 100 {
		return nil, fmt.Errorf("%w: queue limit is 1..100", ErrInvalidInput)
	}
	var out []QueueItem
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(strconv.Itoa(limit)), actorproof.S("queue"))
		if err := s.attachProof(ctx, tx, a, OpProofCaseRead, actorproof.TargetNew, hash); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT o_case_id, o_owner_scope, o_person_id, o_status, o_sar_flagged, o_last_seq, o_opened_at FROM aml_case_queue($1, $2)`, a.TenantID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var q QueueItem
			var st string
			if err := rows.Scan(&q.CaseID, &q.OwnerScope, &q.PersonID, &st, &q.SARFlagged, &q.LastSeq, &q.OpenedAt); err != nil {
				return err
			}
			q.Status = CaseStatus(st)
			out = append(out, q)
		}
		return rows.Err()
	})
	return out, err
}

// writeCall runs one sequence-checked case write and returns the new last sequence.
func (s *Service) writeCall(ctx context.Context, a Actor, op string, caseID uuid.UUID, hash, sql string, args ...any) (int, error) {
	var seq int
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		if err := s.attachProof(ctx, tx, a, op, caseID.String(), hash); err != nil {
			return err
		}
		return tx.QueryRow(ctx, sql, args...).Scan(&seq)
	})
	return seq, err
}

// Transition moves a case along the plain state machine (SAR closures go through Propose / Approve). expectedSeq is the
// case's LastSeq as the caller read it: a case that moved on is ErrStale.
func (s *Service) Transition(ctx context.Context, a Actor, caseID uuid.UUID, expectedSeq int, to CaseStatus, reasonCode string) (int, error) {
	if caseID == uuid.Nil || expectedSeq < 1 || !ValidReasonCode(reasonCode) {
		return 0, fmt.Errorf("%w: case id, expected sequence and reason code are required", ErrInvalidInput)
	}
	hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(caseID.String()), actorproof.S(strconv.Itoa(expectedSeq)), actorproof.S(string(to)), actorproof.S(reasonCode))
	return s.writeCall(ctx, a, OpProofCaseTransition, caseID, hash,
		`SELECT aml_case_transition($1, $2, $3, $4, $5)`, a.TenantID, caseID, expectedSeq, string(to), reasonCode)
}

// AddNote lints the body server-side (LintNote) BEFORE the proof is signed and the database call is made, so a proof can
// never exist for an unlinted note, then stores it with the lint version.
func (s *Service) AddNote(ctx context.Context, a Actor, caseID uuid.UUID, expectedSeq int, body string) (int, error) {
	if caseID == uuid.Nil || expectedSeq < 1 {
		return 0, fmt.Errorf("%w: case id and expected sequence are required", ErrInvalidInput)
	}
	if err := LintNote(body); err != nil {
		return 0, err
	}
	hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(caseID.String()), actorproof.S(strconv.Itoa(expectedSeq)), actorproof.S(body), actorproof.S(LintVersion))
	return s.writeCall(ctx, a, OpProofCaseNote, caseID, hash,
		`SELECT aml_case_add_note($1, $2, $3, $4, $5)`, a.TenantID, caseID, expectedSeq, body, LintVersion)
}

// FlagSAR marks the case SAR-flagged. The flag is irreversible and forbids closing the case as no action.
func (s *Service) FlagSAR(ctx context.Context, a Actor, caseID uuid.UUID, expectedSeq int, reasonCode string) (int, error) {
	if caseID == uuid.Nil || expectedSeq < 1 || !ValidReasonCode(reasonCode) {
		return 0, fmt.Errorf("%w: case id, expected sequence and reason code are required", ErrInvalidInput)
	}
	hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(caseID.String()), actorproof.S(strconv.Itoa(expectedSeq)), actorproof.S(reasonCode))
	return s.writeCall(ctx, a, OpProofCaseFlagSAR, caseID, hash,
		`SELECT aml_case_flag_sar($1, $2, $3, $4)`, a.TenantID, caseID, expectedSeq, reasonCode)
}

// Propose records the proposal half of a four-eyes action (a SAR closure or a confirmed-sanctions hold lift). The returned
// ContentHash is what the approver must bind: Approve refuses any other hash, and any later event invalidates the proposal.
func (s *Service) Propose(ctx context.Context, a Actor, caseID uuid.UUID, expectedSeq int, in ProposeInput) (Proposal, error) {
	if caseID == uuid.Nil || expectedSeq < 1 {
		return Proposal{}, fmt.Errorf("%w: case id and expected sequence are required", ErrInvalidInput)
	}
	if err := in.validate(); err != nil {
		return Proposal{}, err
	}
	var p Proposal
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(caseID.String()), actorproof.S(strconv.Itoa(expectedSeq)), actorproof.S(string(in.Kind)),
			actorproof.UUIDP(uuidPtr(in.HoldID)), actorproof.SP(in.ExternalFilingReference), actorproof.S(in.ReasonCode))
		if err := s.attachProof(ctx, tx, a, OpProofCasePropose, caseID.String(), hash); err != nil {
			return err
		}
		var ref *string
		if in.ExternalFilingReference != "" {
			ref = &in.ExternalFilingReference
		}
		return tx.QueryRow(ctx, `SELECT o_seq, o_content_hash FROM aml_case_propose($1, $2, $3, $4, $5, $6, $7)`,
			a.TenantID, caseID, expectedSeq, string(in.Kind), uuidPtr(in.HoldID), ref, in.ReasonCode).Scan(&p.Seq, &p.ContentHash)
	})
	return p, err
}

// Approve records the approval half. proposalSeq must be the case's LAST event (nothing may have happened since the
// proposal) and contentHash the hash the approver reviewed. A SAR closure moves the case to its closed_sar_* status; a
// hold-lift proposal lifts the hold in the same transaction. The database refuses an approver who is the proposer, the
// proposer's Person, or the case subject.
func (s *Service) Approve(ctx context.Context, a Actor, caseID uuid.UUID, proposalSeq int, contentHash, reasonCode string) (int, error) {
	if caseID == uuid.Nil || proposalSeq < 2 || len(contentHash) != 64 || !ValidReasonCode(reasonCode) {
		return 0, fmt.Errorf("%w: case id, proposal sequence, content hash and reason code are required", ErrInvalidInput)
	}
	hash := actorproof.Digest(tenantStr(a.TenantID), actorproof.S(caseID.String()), actorproof.S(strconv.Itoa(proposalSeq)), actorproof.S(contentHash), actorproof.S(reasonCode))
	return s.writeCall(ctx, a, OpProofCaseApprove, caseID, hash,
		`SELECT aml_case_approve($1, $2, $3, $4, $5)`, a.TenantID, caseID, proposalSeq, contentHash, reasonCode)
}
