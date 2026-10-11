package launchgov

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
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

// Queue paging bounds (ADR 0112 slice 6 (c)).
const (
	QueueDefaultLimit = 50
	QueueMaxLimit     = 200
)

// QueueCursor is the keyset position of the last row of a page: the queue is ordered by
// (created_at DESC, id DESC) and the next page is every row strictly after that pair. It is a
// position, not a capability: the platform-only authorisation of the queue is applied on every call.
type QueueCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

// ErrBadCursor is a cursor that is not exactly what EncodeCursor produces.
var ErrBadCursor = errors.New("launchgov: malformed queue cursor")

// EncodeCursor renders a cursor as an opaque URL-safe token: v1.<unix microseconds>.<uuid>, base64url.
// Microseconds are the precision of timestamptz, so the round trip is exact.
func EncodeCursor(c QueueCursor) string {
	raw := "v1." + strconv.FormatInt(c.CreatedAt.UTC().UnixMicro(), 10) + "." + c.ID.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor is the strict inverse of EncodeCursor: any other input is ErrBadCursor.
func DecodeCursor(tok string) (QueueCursor, error) {
	if tok == "" || len(tok) > 128 {
		return QueueCursor{}, ErrBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return QueueCursor{}, ErrBadCursor
	}
	parts := strings.Split(string(raw), ".")
	if len(parts) != 3 || parts[0] != "v1" {
		return QueueCursor{}, ErrBadCursor
	}
	us, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || us <= 0 || strconv.FormatInt(us, 10) != parts[1] {
		return QueueCursor{}, ErrBadCursor
	}
	id, err := uuid.Parse(parts[2])
	if err != nil || id == uuid.Nil || id.String() != parts[2] {
		return QueueCursor{}, ErrBadCursor
	}
	return QueueCursor{CreatedAt: time.UnixMicro(us).UTC(), ID: id}, nil
}

// QueuePage is one page of the platform-wide queue. Next is nil on the last page.
type QueuePage struct {
	Items []Request
	Next  *QueueCursor
}

// ListRequestsAcrossTenants is the platform-wide approval queue: a PLATFORM actor only, every
// tenant, newest first by (created_at DESC, id DESC) with keyset paging (after = the last row of the
// previous page; nil = first page). limit defaults to QueueDefaultLimit and is capped at
// QueueMaxLimit. Same expiry-on-access rule as ListRequests (a pending row past expires_at is
// reported as expired). Rows carry tenant and brand names (display only) and the caller's
// approvability. A tenant actor is refused (ErrForbidden): its view is its own tenant.
func (s *Service) ListRequestsAcrossTenants(ctx context.Context, a Actor, statuses []string, limit int, after *QueueCursor) (QueuePage, error) {
	if a.Scope == actorproof.ScopePlatformActing {
		return QueuePage{}, ErrActingSession
	}
	if a.Scope != ScopePlatform {
		return QueuePage{}, fmt.Errorf("%w: the platform-wide queue is for platform staff", ErrForbidden)
	}
	if err := authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: uuid.New()}, false); err != nil {
		return QueuePage{}, err
	}
	if limit <= 0 {
		limit = QueueDefaultLimit
	}
	if limit > QueueMaxLimit {
		limit = QueueMaxLimit
	}
	if statuses == nil {
		statuses = []string{}
	}
	var afterTS *time.Time
	var afterID *uuid.UUID
	if after != nil {
		afterTS, afterID = &after.CreatedAt, &after.ID
	}
	now := s.now()
	var page QueuePage
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		page = QueuePage{}
		rows, err := tx.Query(ctx, `SELECT `+prefixed("r", requestColumns)+`, t.name, b.name
			FROM launch_authorisation_requests r
			LEFT JOIN tenants t ON t.id = r.tenant_id
			LEFT JOIN brands b ON b.id = r.brand_id AND b.tenant_id = r.tenant_id
			WHERE (cardinality($1::text[]) = 0
			       OR (CASE WHEN r.status = 'pending' AND r.expires_at <= $3 THEN 'expired' ELSE r.status END) = ANY($1))
			  AND ($4::timestamptz IS NULL OR (r.created_at, r.id) < ($4::timestamptz, $5::uuid))
			ORDER BY r.created_at DESC, r.id DESC LIMIT $2`, statuses, limit+1, now, afterTS, afterID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r Request
			if err := scanRequestWithNames(rows, &r); err != nil {
				return err
			}
			if r.Status == ReqPending && !now.Before(r.ExpiresAt) {
				r.Status = ReqExpired
			}
			page.Items = append(page.Items, r)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		if len(page.Items) > limit {
			page.Items = page.Items[:limit]
			last := page.Items[limit-1]
			page.Next = &QueueCursor{CreatedAt: last.CreatedAt, ID: last.ID}
		}
		return s.annotateApprovability(ctx, tx, a, now, page.Items)
	})
	return page, err
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
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		return s.annotateApprovability(ctx, tx, a, now, out)
	})
	return out, err
}

// LatestBrandKinds returns, for each given brand of the tenant, the kind of its newest status
// transition (governed | legacy_baseline | owner_provisioned). A brand with no transition is
// absent from the map (the API renders null). ADR 0112 slice 6 (e): a display flag for lists, read
// with the same ordering the history endpoint uses ((created_at, id) newest first). A tenant actor
// reads only its own tenant (authorize + RLS); a role without launch_authorisation:read gets
// ErrForbidden and the caller renders null.
func (s *Service) LatestBrandKinds(ctx context.Context, a Actor, tenantID uuid.UUID, brandIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: tenantID}, false); err != nil {
		return nil, err
	}
	return s.latestKinds(ctx, a, `SELECT DISTINCT ON (brand_id) brand_id, kind FROM launch_status_transitions
		WHERE subject_kind = 'brand' AND tenant_id = $1 AND brand_id = ANY($2)
		ORDER BY brand_id, created_at DESC, id DESC`, tenantID, nonNilIDs(brandIDs))
}

// LatestTenantKinds is LatestBrandKinds for tenant subjects. Platform actors only (the tenant list
// is platform-only).
func (s *Service) LatestTenantKinds(ctx context.Context, a Actor, tenantIDs []uuid.UUID) (map[uuid.UUID]string, error) {
	if a.Scope != ScopePlatform {
		return nil, fmt.Errorf("%w: tenant launch kinds are for platform staff", ErrForbidden)
	}
	if err := authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: uuid.New()}, false); err != nil {
		return nil, err
	}
	return s.latestKinds(ctx, a, `SELECT DISTINCT ON (tenant_id) tenant_id, kind FROM launch_status_transitions
		WHERE subject_kind = 'tenant' AND tenant_id = ANY($1)
		ORDER BY tenant_id, created_at DESC, id DESC`, nonNilIDs(tenantIDs))
}

// nonNilIDs: a nil slice would bind as SQL NULL and ANY(NULL) matches nothing silently.
func nonNilIDs(ids []uuid.UUID) []uuid.UUID {
	if ids == nil {
		return []uuid.UUID{}
	}
	return ids
}

func (s *Service) latestKinds(ctx context.Context, a Actor, q string, args ...any) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			var kind string
			if err := rows.Scan(&id, &kind); err != nil {
				return err
			}
			out[id] = kind
		}
		return rows.Err()
	})
	return out, err
}
