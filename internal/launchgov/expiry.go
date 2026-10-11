package launchgov

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

// expireStale moves every pending request that is past its expires_at to 'expired' and writes ONE
// launch_authorisation.expired audit row per request, in the caller's transaction (so a request
// can never be expired without its audit row, and a request that another session already
// expired is not audited twice: the UPDATE returns only the rows this statement changed). It
// narrows to one subject (sub non-nil) or one request id (id non-nil) or the whole tenant. The
// rows are locked in id order first, so concurrent sweeps and decisions never form a lock cycle.
// The expiry time is the database's clock (the clock the 0128 guard uses for the same
// transition); observed names the call site for the audit row.
func (s *Service) expireStale(ctx context.Context, tx pgx.Tx, a Actor, tenantID uuid.UUID, sub *Subject, id *uuid.UUID, observed string) ([]Request, error) {
	var kind *string
	var brand *uuid.UUID
	if sub != nil {
		k := sub.Kind()
		kind, brand = &k, sub.BrandID
	}
	rows, err := tx.Query(ctx, `
		UPDATE launch_authorisation_requests SET status = 'expired'
		 WHERE id IN (SELECT r.id FROM launch_authorisation_requests r
		               WHERE r.tenant_id = $1 AND r.status = 'pending' AND r.expires_at <= now()
		                 AND ($2::uuid IS NULL OR r.id = $2)
		                 AND ($3::text IS NULL OR (r.subject_kind = $3 AND r.brand_id IS NOT DISTINCT FROM $4))
		               ORDER BY r.id FOR UPDATE)
		RETURNING `+requestColumns, tenantID, id, kind, brand)
	if err != nil {
		return nil, err
	}
	var expired []Request
	for rows.Next() {
		var r Request
		if err := scanRequest(rows, &r); err != nil {
			rows.Close()
			return nil, err
		}
		expired = append(expired, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, r := range expired {
		if err := s.auditEvent(ctx, tx, a, "expired", r, map[string]any{
			"expires_at": r.ExpiresAt.UTC().Format(time.RFC3339), "observed_by": a.StaffID.String(), "observed_at_operation": observed,
		}); err != nil {
			return nil, err
		}
	}
	return expired, nil
}

// SweepExpired materialises time-expired pending requests of the tenant (all of them, or the
// subject's) as 'expired' with one audit row each, and returns how many it expired. It is safe
// to call at any time and from any number of workers: a request is expired and audited exactly
// once. The service also calls it (best effort) when expiry is observed on a read or a decision.
func (s *Service) SweepExpired(ctx context.Context, a Actor, tenantID uuid.UUID) (int, error) {
	if err := authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: tenantID}, false); err != nil {
		return 0, err
	}
	n := 0
	err := s.runActor(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		got, err := s.expireStale(ctx, tx, a, tenantID, nil, nil, "sweep")
		n = len(got)
		return err
	})
	return n, classifyOrNil(err)
}

func classifyOrNil(err error) error {
	if err == nil {
		return nil
	}
	return classify(err)
}

// sweepBestEffort runs the expiry sweep for one request (id non-nil) or a tenant; a failure is
// ignored (the caller's own outcome never depends on it; the next observation retries).
func (s *Service) sweepBestEffort(ctx context.Context, a Actor, tenantID uuid.UUID, id *uuid.UUID) {
	if authorize(a, auth.PermLaunchAuthorisationRead, Subject{TenantID: tenantID}, false) != nil {
		return
	}
	_ = s.runActor(context.WithoutCancel(ctx), a, func(ctx context.Context, tx pgx.Tx) error {
		_, err := s.expireStale(ctx, tx, a, tenantID, nil, id, "observed")
		return err
	})
}

// loadPriorRejects returns every reject decision, within the request TTL, on another request of
// the same subject and action, whatever its payload (security C2): the carry-over rule (LA014)
// is exact on the payload hash and so is defeated by editing one character; showing the
// variants to the approver and refusing the prior rejecter's approval are the other half.
func loadPriorRejects(ctx context.Context, tx pgx.Tx, now time.Time, req Request) ([]PriorReject, error) {
	rows, err := tx.Query(ctx, `
		SELECT r.id, ap.decided_at, ap.decided_by, ap.decided_by_person_id, ap.reason_code, r.payload_hash = $5
		  FROM launch_authorisation_approvals ap
		  JOIN launch_authorisation_requests r ON r.id = ap.request_id
		 WHERE r.tenant_id = $1 AND r.subject_kind = $2 AND r.brand_id IS NOT DISTINCT FROM $3 AND r.action = $4
		   AND r.id <> $6 AND ap.decision = 'reject' AND ap.decided_at > $7
		 ORDER BY ap.decided_at DESC, ap.id`,
		req.TenantID, req.SubjectKind, req.BrandID, req.Action, req.PayloadHash, req.ID, now.Add(-RequestTTL))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PriorReject
	for rows.Next() {
		var p PriorReject
		if err := rows.Scan(&p.RequestID, &p.DecidedAt, &p.DecidedBy, &p.DecidedByPersonID, &p.ReasonCode, &p.SamePayload); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
