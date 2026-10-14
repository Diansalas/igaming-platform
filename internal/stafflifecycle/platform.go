package stafflifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

// Request kinds (ADR 0119 s10.4; migration 0139's CHECK).
const (
	KindPlatformInvite        = "platform_invite"
	KindPlatformReactivate    = "platform_reactivate"
	KindPlatformMFAReset      = "platform_mfa_reset"
	KindPlatformPasswordReset = "platform_password_reset" // in the database vocabulary; NOT IMPLEMENTED here
)

// Request is a staff_lifecycle_requests row as the API returns it.
type Request struct {
	ID                uuid.UUID
	Kind              string
	State             string // pending | executed | rejected | cancelled | expired (a pending row past expires_at reads expired)
	TargetStaffID     *uuid.UUID
	InviteEmail       *string
	ReasonCode        *string
	Note              *string
	RequestedBy       uuid.UUID
	RequiredApprovals int
	PayloadHash       string
	ExpiresAt         time.Time
	CreatedAt         time.Time
	DecidedAt         *time.Time
	// InvitePersonID and InvitePersonStaff (review fix SR-3): what the approver must see to judge Person distinctness:
	// the invitee Person and every live (non-offboarded) platform staff account already linked to it (the four-eyes
	// session sees platform rows only).
	InvitePersonID    *uuid.UUID
	InvitePersonStaff []LinkedStaff
}

// LinkedStaff is a live staff account linked to a Person (no credential).
type LinkedStaff struct {
	ID     uuid.UUID
	Email  string
	Status string
}

// withInvitePerson fills the SR-3 view of an invitation request in the caller's (platform) transaction.
func withInvitePerson(ctx context.Context, tx pgx.Tx, r requestRow, out Request) (Request, error) {
	out.InvitePersonID = r.invitePerson
	if r.invitePerson == nil {
		return out, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, email, status FROM staff_users WHERE person_id = $1 AND status <> 'offboarded' ORDER BY id`, *r.invitePerson)
	if err != nil {
		return out, err
	}
	linked, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (LinkedStaff, error) {
		var l LinkedStaff
		err := row.Scan(&l.ID, &l.Email, &l.Status)
		return l, err
	})
	if err != nil {
		return out, err
	}
	out.InvitePersonStaff = linked
	return out, nil
}

// RequestInput is a four-eyes request.
type RequestInput struct {
	Kind          string
	TargetStaffID *uuid.UUID
	Email         string
	PersonID      *uuid.UUID
	Reason        Reason
}

const requestColumns = `id, kind, state, target_staff_id, invite_email, reason_code, note, requested_by, required_approvals,
	payload_hash, expires_at, created_at, decided_at, invite_person_id`

type requestRow struct {
	Request
	invitePerson *uuid.UUID
}

func scanRequest(row pgx.Row) (requestRow, error) {
	var r requestRow
	var approvals int16
	if err := row.Scan(&r.ID, &r.Kind, &r.State, &r.TargetStaffID, &r.InviteEmail, &r.ReasonCode, &r.Note, &r.RequestedBy, &approvals,
		&r.PayloadHash, &r.ExpiresAt, &r.CreatedAt, &r.DecidedAt, &r.invitePerson); err != nil {
		return requestRow{}, err
	}
	r.RequiredApprovals = int(approvals)
	return r, nil
}

func (r requestRow) public(now time.Time) Request {
	out := r.Request
	if out.State == "pending" && !now.Before(out.ExpiresAt) {
		out.State = "expired"
	}
	return out
}

// RequestDigest is the payload digest the database recomputes (staff_lifecycle_request_digest): the request
// content (kind, target, email, Person, reason code, note), k2_canonical encoded.
func RequestDigest(kind string, target *uuid.UUID, email string, person *uuid.UUID, reason, note string) string {
	return actorproof.Digest(actorproof.S(kind), actorproof.UUIDP(target), actorproof.SP(email), actorproof.UUIDP(person),
		actorproof.SP(reason), actorproof.SP(note))
}

func (s *Service) attach(ctx context.Context, tx pgx.Tx, actor Actor, op, target, payload string) error {
	iss := s.issuer()
	if iss == nil {
		return ErrUnavailable
	}
	return iss.Attach(ctx, tx, actorproof.Claims{Actor: actor.StaffID, Scope: actorproof.ScopePlatform, Operation: op,
		Target: target, PayloadHash: payload})
}

// CreateRequest files a platform-staff four-eyes request (platform callers only; the route requires
// platform_staff:govern).
func (s *Service) CreateRequest(ctx context.Context, actor Actor, in RequestInput) (Request, error) {
	if !actor.Platform() {
		return Request{}, ErrForbidden
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	switch in.Kind {
	case KindPlatformInvite:
		if in.TargetStaffID != nil || !validEmail(email) || in.Reason.Code != "" {
			return Request{}, ErrInvalidInput
		}
		if in.PersonID == nil {
			return Request{}, ErrPersonRequired
		}
		if in.Reason.Note != "" {
			if err := ValidateNote(in.Reason.Note); err != nil {
				return Request{}, err
			}
		}
	case KindPlatformReactivate, KindPlatformMFAReset:
		if in.TargetStaffID == nil || email != "" || in.PersonID != nil {
			return Request{}, ErrInvalidInput
		}
		action := ActionReactivate
		if in.Kind == KindPlatformMFAReset {
			action = ActionMFAReset
		}
		if err := ValidateReason(action, in.Reason); err != nil {
			return Request{}, err
		}
		if *in.TargetStaffID == actor.StaffID {
			return Request{}, ErrSelfTarget
		}
	case KindPlatformPasswordReset:
		return Request{}, ErrNotImplemented
	default:
		return Request{}, ErrInvalidInput
	}
	var out Request
	err := s.pool.WithPlatformAdmin(ctx, actor.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		if err := reassertActorOnly(ctx, tx, actor); err != nil {
			return err
		}
		person, err := actorPerson(ctx, tx, actor.StaffID)
		if err != nil {
			return err
		}
		if person == nil {
			return ErrPersonRequired
		}
		switch in.Kind {
		case KindPlatformInvite:
			if *in.PersonID == *person {
				return ErrFourEyes
			}
			// Review fix F2: an offboarded account keeps its email as history; only a live one blocks a re-invite.
			var taken bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM staff_users WHERE tenant_id IS NULL AND email = $1 AND status <> 'offboarded')`, email).Scan(&taken); err != nil {
				return err
			}
			if taken {
				return ErrConflict
			}
			// Review fix SR-3 (SF031 repeats it): the invitee Person may not already be bound to a live platform account.
			inUse, err := personInUse(ctx, tx, *in.PersonID)
			if err != nil {
				return err
			}
			if inUse {
				return ErrPersonInUse
			}
		default:
			target, err := readTarget(ctx, tx, *in.TargetStaffID, uuid.Nil)
			if err != nil {
				return err
			}
			want := "suspended"
			if in.Kind == KindPlatformMFAReset {
				want = "active"
			}
			if target.Status != want {
				return ErrTransition
			}
		}
		digest := RequestDigest(in.Kind, in.TargetStaffID, email, in.PersonID, in.Reason.Code, in.Reason.Note)
		if err := s.attach(ctx, tx, actor, actorproof.OpStaffLifecycleRequest, actorproof.TargetNew, digest); err != nil {
			return err
		}
		r, err := scanRequest(tx.QueryRow(ctx, `INSERT INTO staff_lifecycle_requests
				(kind, target_staff_id, invite_email, invite_person_id, reason_code, note, payload_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+requestColumns,
			in.Kind, in.TargetStaffID, nullable(email), in.PersonID, nullable(in.Reason.Code), nullable(in.Reason.Note), digest))
		if err != nil {
			return mapDBError(err)
		}
		if out, err = withInvitePerson(ctx, tx, r, r.Request); err != nil {
			return err
		}
		return recordRequestAudit(ctx, tx, actor, r, "staff.lifecycle_requested")
	})
	return out, mapDBError(err)
}

// recordRequestAudit writes the platform audit row of a four-eyes request decision (request, reject, cancel; an
// approval is audited by the executed change itself, with the request id).
func recordRequestAudit(ctx context.Context, tx pgx.Tx, actor Actor, r requestRow, action string) error {
	md := map[string]any{"kind": r.Kind, "state": r.State, "payload_hash": r.PayloadHash}
	if r.ReasonCode != nil {
		md["reason_code"] = *r.ReasonCode
	}
	if r.TargetStaffID != nil {
		md["target_staff_id"] = r.TargetStaffID.String()
	}
	return audit.Record(ctx, tx, audit.Entry{ActorType: audit.ActorStaff, ActorID: actor.StaffID, Action: action,
		TargetType: "staff_lifecycle_request", TargetID: r.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: actor.Meta.IP, UserAgent: actor.Meta.UserAgent, RequestID: actor.Meta.RequestID, Metadata: md})
}

// ListRequests lists the four-eyes requests (newest first, at most 200).
func (s *Service) ListRequests(ctx context.Context, actor Actor) ([]Request, error) {
	if !actor.Platform() {
		return nil, ErrForbidden
	}
	var out []Request
	err := s.pool.WithPlatformAdmin(ctx, actor.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM staff_lifecycle_requests ORDER BY created_at DESC, id DESC LIMIT 200`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			r, err := scanRequest(rows)
			if err != nil {
				return err
			}
			pub, err := withInvitePerson(ctx, tx, r, r.public(now))
			if err != nil {
				return err
			}
			out = append(out, pub)
		}
		return rows.Err()
	})
	return out, err
}

// GetRequest reads one request.
func (s *Service) GetRequest(ctx context.Context, actor Actor, id uuid.UUID) (Request, error) {
	if !actor.Platform() {
		return Request{}, ErrForbidden
	}
	var out Request
	err := s.pool.WithPlatformAdmin(ctx, actor.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		r, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM staff_lifecycle_requests WHERE id = $1`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		out, err = withInvitePerson(ctx, tx, r, r.public(now))
		return err
	})
	return out, err
}

// Approve decides a request and executes it in the same transaction (ADR 0119 s10.4, the ADR 0112 S9 pattern):
// L1 the request FOR UPDATE; the approver's own row FOR SHARE (re-assertion) and the target's FOR UPDATE, ascending;
// the approval row (SF032 + proof staff_lifecycle:approve); pending -> executing; the change; executing -> executed;
// the event and the audit row. Emails go after the commit.
func (s *Service) Approve(ctx context.Context, actor Actor, id uuid.UUID, payloadHash string) (Request, Delivery, error) {
	if !actor.Platform() {
		return Request{}, DeliveryNone, ErrForbidden
	}
	if err := s.needMailer(); err != nil {
		return Request{}, DeliveryNone, err
	}
	var out Request
	var mails []mail
	err := s.pool.WithPlatformAdmin(ctx, actor.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		mails = nil
		req, err := s.lockPending(ctx, tx, actor, id, true)
		if err != nil {
			return err
		}
		if payloadHash != req.PayloadHash {
			return ErrPayloadHashMismatch
		}
		var target staffRow
		if req.TargetStaffID != nil {
			if target, err = lockActorAndTarget(ctx, tx, actor, *req.TargetStaffID, uuid.Nil); err != nil {
				return err
			}
			if target.ID == actor.StaffID {
				return ErrFourEyes
			}
		} else if err := reassertActorOnly(ctx, tx, actor); err != nil {
			return err
		}
		if err := s.attach(ctx, tx, actor, actorproof.OpStaffLifecycleApprove, req.ID.String(), req.PayloadHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO staff_lifecycle_approvals (request_id, decision, payload_hash) VALUES ($1, 'approve', $2)`,
			req.ID, req.PayloadHash); err != nil {
			return mapDBError(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE staff_lifecycle_requests SET state = 'executing' WHERE id = $1`, req.ID); err != nil {
			return mapDBError(err)
		}
		reqID := req.ID
		var createdTarget *uuid.UUID
		switch req.Kind {
		case KindPlatformInvite:
			// Review fix SR-3 at execution: the Person may have been bound to another account since the filing.
			inUse, err := personInUse(ctx, tx, *req.invitePerson)
			if err != nil {
				return err
			}
			if inUse {
				return ErrPersonInUse
			}
			newID := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id, invited_by)
				VALUES ($1, NULL, $2, NULL, 'platform_admin', 'invited', $3, $4)`, newID, *req.InviteEmail, req.invitePerson, req.RequestedBy); err != nil {
				return mapDBError(err)
			}
			created, err := readTarget(ctx, tx, newID, uuid.Nil)
			if err != nil {
				return err
			}
			now, err := dbNow(ctx, tx)
			if err != nil {
				return err
			}
			tok, err := s.tokens.IssueEnrolmentToken(ctx, tx, created.ref(), store.EnrolInvite, &req.RequestedBy, &reqID, now)
			if err != nil {
				return err
			}
			mails = append(mails, mail{to: created.Email, purpose: store.EnrolInvite, token: tok})
			if err := record(ctx, tx, eventIn{target: created, event: "invited", actorType: "staff", actor: actor.StaffID,
				after: created.state(), requestID: &reqID, meta: actor.Meta,
				extraMetadata: map[string]any{"requested_by": req.RequestedBy.String()}}); err != nil {
				return err
			}
			createdTarget = &newID
		case KindPlatformReactivate:
			m, err := s.reactivateInTx(ctx, tx, target, "staff", actor.StaffID, reasonOf(req), &reqID, actor.Meta)
			if err != nil {
				return err
			}
			mails = m
		case KindPlatformMFAReset:
			m, err := s.resetMFAInTx(ctx, tx, target, actor.StaffID, reasonOf(req), &reqID, actor.Meta)
			if err != nil {
				return err
			}
			mails = m
		default:
			return ErrNotImplemented
		}
		var r requestRow
		if createdTarget != nil {
			r, err = scanRequest(tx.QueryRow(ctx, `UPDATE staff_lifecycle_requests SET state = 'executed', target_staff_id = $2 WHERE id = $1 RETURNING `+requestColumns,
				req.ID, *createdTarget))
		} else {
			r, err = scanRequest(tx.QueryRow(ctx, `UPDATE staff_lifecycle_requests SET state = 'executed' WHERE id = $1 RETURNING `+requestColumns, req.ID))
		}
		if err != nil {
			return mapDBError(err)
		}
		out, err = withInvitePerson(ctx, tx, r, r.Request)
		return err
	})
	if err != nil {
		return Request{}, DeliveryNone, mapDBError(err)
	}
	return out, s.deliver(ctx, mails), nil
}

func reasonOf(r requestRow) Reason {
	var out Reason
	if r.ReasonCode != nil {
		out.Code = *r.ReasonCode
	}
	if r.Note != nil {
		out.Note = *r.Note
	}
	return out
}

// lockPending locks the request (L1) and applies the Go four-eyes checks the database repeats (SF032): pending and
// unexpired; the decider is neither the requester (staff id or Person) nor the target (staff id or Person) nor the
// invitee's Person; the decider has a Person; for an approval (review fix SR-5) the requester is still active.
func (s *Service) lockPending(ctx context.Context, tx pgx.Tx, actor Actor, id uuid.UUID, approve bool) (requestRow, error) {
	req, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM staff_lifecycle_requests WHERE id = $1 FOR UPDATE`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return requestRow{}, ErrNotFound
	}
	if err != nil {
		return requestRow{}, err
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return requestRow{}, err
	}
	if req.State != "pending" || !now.Before(req.ExpiresAt) {
		return requestRow{}, ErrTransition
	}
	person, err := actorPerson(ctx, tx, actor.StaffID)
	if err != nil {
		return requestRow{}, err
	}
	if person == nil || req.RequestedBy == actor.StaffID {
		return requestRow{}, ErrFourEyes
	}
	var requesterPerson uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT requested_by_person FROM staff_lifecycle_requests WHERE id = $1`, id).Scan(&requesterPerson); err != nil {
		return requestRow{}, err
	}
	if requesterPerson == *person || (req.invitePerson != nil && *req.invitePerson == *person) {
		return requestRow{}, ErrFourEyes
	}
	if approve {
		var requesterStatus string
		err := tx.QueryRow(ctx, `SELECT status FROM staff_users WHERE id = $1`, req.RequestedBy).Scan(&requesterStatus)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return requestRow{}, err
		}
		if requesterStatus != "active" {
			return requestRow{}, fmt.Errorf("%w: the requester is no longer an active staff member", ErrTransition)
		}
	}
	if req.TargetStaffID != nil {
		if *req.TargetStaffID == actor.StaffID {
			return requestRow{}, ErrFourEyes
		}
		var tp *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, *req.TargetStaffID).Scan(&tp); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return requestRow{}, err
		}
		if tp != nil && *tp == *person {
			return requestRow{}, ErrFourEyes
		}
	}
	return req, nil
}

// Reject records a reject decision (protective; no step-up) and closes the request.
func (s *Service) Reject(ctx context.Context, actor Actor, id uuid.UUID) (Request, error) {
	if !actor.Platform() {
		return Request{}, ErrForbidden
	}
	var out Request
	err := s.pool.WithPlatformAdmin(ctx, actor.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		req, err := s.lockPending(ctx, tx, actor, id, false)
		if err != nil {
			return err
		}
		// Review fix SR-8: the decider's own row FOR SHARE against the verified token (role, staff_version).
		if err := reassertActorOnly(ctx, tx, actor); err != nil {
			return err
		}
		if err := s.attach(ctx, tx, actor, actorproof.OpStaffLifecycleReject, req.ID.String(), req.PayloadHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO staff_lifecycle_approvals (request_id, decision, payload_hash) VALUES ($1, 'reject', $2)`,
			req.ID, req.PayloadHash); err != nil {
			return mapDBError(err)
		}
		r, err := scanRequest(tx.QueryRow(ctx, `UPDATE staff_lifecycle_requests SET state = 'rejected' WHERE id = $1 RETURNING `+requestColumns, req.ID))
		if err != nil {
			return mapDBError(err)
		}
		out = r.Request
		return recordRequestAudit(ctx, tx, actor, r, "staff.lifecycle_request_rejected")
	})
	return out, mapDBError(err)
}

// Cancel withdraws one's own pending request.
func (s *Service) Cancel(ctx context.Context, actor Actor, id uuid.UUID) (Request, error) {
	if !actor.Platform() {
		return Request{}, ErrForbidden
	}
	var out Request
	err := s.pool.WithPlatformAdmin(ctx, actor.StaffID, func(ctx context.Context, tx pgx.Tx) error {
		req, err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM staff_lifecycle_requests WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if req.RequestedBy != actor.StaffID {
			return ErrForbidden
		}
		if req.State != "pending" {
			return ErrTransition
		}
		// Review fix SR-8: the requester's own row FOR SHARE against the verified token.
		if err := reassertActorOnly(ctx, tx, actor); err != nil {
			return err
		}
		if err := s.attach(ctx, tx, actor, actorproof.OpStaffLifecycleCancel, req.ID.String(), req.PayloadHash); err != nil {
			return err
		}
		r, err := scanRequest(tx.QueryRow(ctx, `UPDATE staff_lifecycle_requests SET state = 'cancelled' WHERE id = $1 RETURNING `+requestColumns, req.ID))
		if err != nil {
			return mapDBError(err)
		}
		out = r.Request
		return recordRequestAudit(ctx, tx, actor, r, "staff.lifecycle_request_cancelled")
	})
	return out, mapDBError(err)
}
