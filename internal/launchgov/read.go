package launchgov

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

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
