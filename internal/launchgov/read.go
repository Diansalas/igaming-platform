package launchgov

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
)

// ReadReadiness is EvaluateReadiness behind the read permission and the actor's scope: the entry
// point for an operator (the HTTP route). It is read-only; a tenant actor can evaluate only its
// own tenant (a foreign tenant is ErrNotFound), and a platform actor any tenant.
func (s *Service) ReadReadiness(ctx context.Context, a Actor, in EvalInput) (Readiness, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRead, in.Subject, false); err != nil {
		return Readiness{}, err
	}
	return s.EvaluateReadiness(ctx, in)
}

// CancelByID cancels a pending request named only by its id within the (route-validated) tenant:
// the subject is read from the stored request, never from the caller. An unknown or foreign id is
// ErrNotFound. Everything else is Cancel.
func (s *Service) CancelByID(ctx context.Context, a Actor, tenantID, requestID uuid.UUID, reasonCode string) (*Request, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRequest, Subject{TenantID: tenantID}, false); err != nil {
		return nil, err
	}
	pre, err := s.peek(ctx, a, tenantID, requestID)
	if err != nil {
		return nil, err
	}
	return s.Cancel(ctx, a, pre.Subject(), requestID, reasonCode)
}

// Transition is one launch_status_transitions row (the append-only status history). Codes and ids
// only; no natural person's data.
type Transition struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	SubjectKind string
	BrandID     *uuid.UUID
	Kind        string // governed | legacy_baseline | owner_provisioned
	FromStatus  *string
	ToStatus    string
	RequestID   *uuid.UUID
	ExecutedBy  *uuid.UUID
	ApproverIDs []uuid.UUID
	ActorType   string
	CreatedAt   time.Time
}

// ListTransitions reads the status history of the tenant (brandID nil = every subject of the
// tenant, tenant and brands), newest first. Read-only; a foreign tenant is ErrNotFound for a
// tenant actor (authorize), and RLS binds a tenant session to its own tenant regardless.
func (s *Service) ListTransitions(ctx context.Context, a Actor, tenantID uuid.UUID, brandID *uuid.UUID, limit int) ([]Transition, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: tenantID}, false); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Transition
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT id, tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id,
				executed_by, approver_ids, actor_type, created_at
			  FROM launch_status_transitions
			 WHERE tenant_id = $1 AND ($2::uuid IS NULL OR brand_id = $2)
			 ORDER BY created_at DESC, id DESC LIMIT $3`, tenantID, brandID, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var t Transition
			if err := rows.Scan(&t.ID, &t.TenantID, &t.SubjectKind, &t.BrandID, &t.Kind, &t.FromStatus, &t.ToStatus, &t.RequestID,
				&t.ExecutedBy, &t.ApproverIDs, &t.ActorType, &t.CreatedAt); err != nil {
				return err
			}
			out = append(out, t)
		}
		return rows.Err()
	})
	return out, err
}

// ListRequestsAcrossTenants is the platform-wide approval queue: a PLATFORM actor only, every
// tenant, newest first. Same expiry-on-access rule as ListRequests (a pending row past expires_at
// is reported as expired). A tenant actor is refused (ErrForbidden): its view is its own tenant.
func (s *Service) ListRequestsAcrossTenants(ctx context.Context, a Actor, statuses []string, limit int) ([]Request, error) {
	if a.Scope == actorproof.ScopePlatformActing {
		return nil, ErrActingSession
	}
	if a.Scope != ScopePlatform {
		return nil, fmt.Errorf("%w: the platform-wide queue is for platform staff", ErrForbidden)
	}
	if err := authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: uuid.New()}, false); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if statuses == nil {
		statuses = []string{}
	}
	now := s.now()
	var out []Request
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM launch_authorisation_requests
			WHERE (cardinality($1::text[]) = 0
			       OR (CASE WHEN status = 'pending' AND expires_at <= $3 THEN 'expired' ELSE status END) = ANY($1))
			ORDER BY created_at DESC, id DESC LIMIT $2`, statuses, limit, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Request
			if err := scanRequest(rows, &r); err != nil {
				return err
			}
			if r.Status == ReqPending && !now.Before(r.ExpiresAt) {
				r.Status = ReqExpired
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}

// ListFilter narrows ListRequests. All fields are optional; the tenant is the route-validated one.
type ListFilter struct {
	BrandID  *uuid.UUID // nil = every subject of the tenant
	Statuses []string   // empty = every status
	Limit    int        // default 100, max 500
}

// ListRequests lists the tenant's launch requests, newest first. A tenant actor reads only its own
// tenant (RLS) and a foreign tenant id is ErrNotFound-equivalent (authorize). Expiry is computed
// on access: a pending request past its expires_at is reported with Status ReqExpired and is
// never executable (the database refuses an approval on it).
func (s *Service) ListRequests(ctx context.Context, a Actor, tenantID uuid.UUID, f ListFilter) ([]Request, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: tenantID}, false); err != nil {
		return nil, err
	}
	limit := f.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var out []Request
	now := s.now()
	statuses := f.Statuses
	if statuses == nil {
		statuses = []string{}
	}
	// Expiry observed here is materialised (and audited once) before the read; the filter below
	// still treats a pending row past its expires_at as expired on its own, so the result is right
	// even when the materialising write could not run.
	s.sweepBestEffort(ctx, a, tenantID, nil)
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT `+requestColumns+` FROM launch_authorisation_requests
			WHERE tenant_id = $1 AND ($2::uuid IS NULL OR brand_id = $2)
			  AND (cardinality($3::text[]) = 0
			       OR (CASE WHEN status = 'pending' AND expires_at <= $5 THEN 'expired' ELSE status END) = ANY($3))
			ORDER BY created_at DESC, id DESC LIMIT $4`, tenantID, f.BrandID, statuses, limit, now)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Request
			if err := scanRequest(rows, &r); err != nil {
				return err
			}
			if r.Status == ReqPending && !now.Before(r.ExpiresAt) {
				r.Status = ReqExpired
			}
			out = append(out, r)
		}
		return rows.Err()
	})
	return out, err
}
