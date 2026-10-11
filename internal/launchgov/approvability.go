package launchgov

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// ADR 0112 slice 6 (d): a per-request, server-computed answer to "may THIS caller approve this
// request right now", with a reason from a CLOSED catalogue. It is a read-side convenience for the
// Back Office, never an authority: Decide re-checks everything under the row lock and the database
// (LA012 and friends) stays authoritative, so a stale or wrong answer here can only mis-label a
// button, never permit a decision.
//
// A reason is a code only. It never names or hints at another staff member: "self_approval" and
// "same_person" describe the CALLER's own accounts, "already_decided" the caller's own Person, and
// "prior_reject_requires_change_note" the caller's own earlier reject.
const (
	CannotNotPlatformApprover = "not_platform_approver"
	CannotNotPending          = "not_pending"
	CannotExpired             = "expired"
	CannotSelfApproval        = "self_approval"
	CannotSamePerson          = "same_person"
	CannotNoPerson            = "no_person"
	CannotAlreadyDecided      = "already_decided"
	CannotPriorReject         = "prior_reject_requires_change_note"
)

// CannotApproveReasons is the closed catalogue, in the order the rule evaluates them.
var CannotApproveReasons = []string{
	CannotNotPlatformApprover, CannotNotPending, CannotExpired, CannotSelfApproval,
	CannotSamePerson, CannotNoPerson, CannotAlreadyDecided, CannotPriorReject,
}

// Approvability is the answer for one (caller, request) pair. Reason is empty iff CanApprove.
type Approvability struct {
	CanApprove bool
	Reason     string
}

// approvalFacts are the caller-specific facts the pure rule needs; the loader fills them.
type approvalFacts struct {
	MyPerson              uuid.UUID // Nil when the caller's staff row has no readable Person
	DecidedByMyPerson     bool      // my Person already recorded a decision on this request
	PriorRejectByMyPerson bool      // my Person rejected a request of the same subject and action within the TTL
}

// callerCanApproveAtAll is the static part: a platform session whose role holds :approve.
func callerCanApproveAtAll(a Actor) bool {
	return a.Scope == ScopePlatform && a.TenantID == uuid.Nil && auth.RoleHasPermission(a.Role, auth.PermLaunchAuthorisationApprove)
}

// evalApprovability is the pure rule. It mirrors Decide's refusal order exactly (see decide.go).
func evalApprovability(a Actor, r Request, now time.Time, f approvalFacts) Approvability {
	no := func(reason string) Approvability { return Approvability{Reason: reason} }
	if !callerCanApproveAtAll(a) {
		return no(CannotNotPlatformApprover)
	}
	if r.Status == ReqExpired || (r.Status == ReqPending && !now.Before(r.ExpiresAt)) {
		return no(CannotExpired)
	}
	if r.Status != ReqPending {
		return no(CannotNotPending)
	}
	if r.RequestedBy == a.StaffID {
		return no(CannotSelfApproval)
	}
	if f.MyPerson == uuid.Nil || r.RequestedByPersonID == uuid.Nil {
		return no(CannotNoPerson)
	}
	if f.MyPerson == r.RequestedByPersonID {
		return no(CannotSamePerson)
	}
	if f.DecidedByMyPerson {
		return no(CannotAlreadyDecided)
	}
	if f.PriorRejectByMyPerson && strings.TrimSpace(r.ConditionsNote) == "" {
		return no(CannotPriorReject)
	}
	return Approvability{CanApprove: true}
}

type subjectKey struct {
	tenant uuid.UUID
	kind   string
	brand  uuid.UUID // Nil for a tenant subject
	action string
}

func keyOf(r Request) subjectKey {
	k := subjectKey{tenant: r.TenantID, kind: r.SubjectKind, action: r.Action}
	if r.BrandID != nil {
		k.brand = *r.BrandID
	}
	return k
}

// annotateApprovability fills Request.Approvability for every request, inside the caller's read
// transaction. A caller that can never approve costs no query at all. Otherwise three bounded
// reads: the caller's Person, the caller's Person's decisions on these requests, and the caller's
// Person's rejects within the request TTL. A missing Person is reported (no_person), not guessed.
func (s *Service) annotateApprovability(ctx context.Context, tx pgx.Tx, a Actor, now time.Time, rs []Request) error {
	if len(rs) == 0 {
		return nil
	}
	if !callerCanApproveAtAll(a) {
		for i := range rs {
			v := evalApprovability(a, rs[i], now, approvalFacts{})
			rs[i].Approvability = &v
		}
		return nil
	}
	me, err := personOf(ctx, tx, a.StaffID)
	if err != nil && !errors.Is(err, ErrNoPerson) {
		return err
	}
	if errors.Is(err, ErrNoPerson) {
		me = uuid.Nil
	}
	decided := map[uuid.UUID]bool{}
	rejected := map[subjectKey]bool{}
	if me != uuid.Nil {
		ids := make([]uuid.UUID, 0, len(rs))
		for _, r := range rs {
			ids = append(ids, r.ID)
		}
		rows, err := tx.Query(ctx, `SELECT request_id FROM launch_authorisation_approvals WHERE request_id = ANY($1) AND decided_by_person_id = $2`, ids, me)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			decided[id] = true
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		prows, err := tx.Query(ctx, `
			SELECT DISTINCT r.tenant_id, r.subject_kind, r.brand_id, r.action
			  FROM launch_authorisation_approvals ap
			  JOIN launch_authorisation_requests r ON r.id = ap.request_id
			 WHERE ap.decision = 'reject' AND ap.decided_by_person_id = $1 AND ap.decided_at > $2`, me, now.Add(-RequestTTL))
		if err != nil {
			return err
		}
		for prows.Next() {
			var k subjectKey
			var brand *uuid.UUID
			if err := prows.Scan(&k.tenant, &k.kind, &brand, &k.action); err != nil {
				prows.Close()
				return err
			}
			if brand != nil {
				k.brand = *brand
			}
			rejected[k] = true
		}
		prows.Close()
		if err := prows.Err(); err != nil {
			return err
		}
	}
	for i := range rs {
		// A request that is itself rejected by my Person is not "pending", so the same-request
		// reject never reaches the prior-reject rule (Decide excludes r.id <> request).
		v := evalApprovability(a, rs[i], now, approvalFacts{
			MyPerson: me, DecidedByMyPerson: decided[rs[i].ID], PriorRejectByMyPerson: rejected[keyOf(rs[i])],
		})
		rs[i].Approvability = &v
	}
	return nil
}
