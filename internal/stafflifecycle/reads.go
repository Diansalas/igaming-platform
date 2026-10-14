package stafflifecycle

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxDirectory is the directory page cap (Truncated beyond it).
const MaxDirectory = 500

// MaxEvents is the lifecycle history cap.
const MaxEvents = 200

// staffSelect is the directory projection. mfa: an ACTIVE factor exists / it is hard-locked. last_login_at: the
// newest staff.login_succeeded audit row of the staff member that is visible in the scope (ADR 0119 s14.4 derives
// it from the sessions' auth_time; sessions are visible only to their own principal under RLS, so the audit row of
// the same event - "a session was issued", s14.4 N9 - is used instead: a recorded deviation).
const staffSelect = `SELECT su.id, su.tenant_id, su.email, su.display_name, su.role, su.status, su.staff_version, su.person_id IS NOT NULL,
       EXISTS (SELECT 1 FROM staff_mfa_factors f WHERE f.staff_user_id = su.id AND f.status = 'active'),
       EXISTS (SELECT 1 FROM staff_mfa_factors f WHERE f.staff_user_id = su.id AND f.status = 'active' AND f.hard_locked_at IS NOT NULL),
       (SELECT max(a.created_at) FROM audit_log a WHERE a.actor_type = 'staff' AND a.actor_id = su.id AND a.action = 'staff.login_succeeded'),
       su.created_at, su.status_changed_at
  FROM staff_users su`

func scanStaff(row pgx.Row) (Staff, error) {
	var s Staff
	var tenant *uuid.UUID
	var last *time.Time
	if err := row.Scan(&s.ID, &tenant, &s.Email, &s.DisplayName, &s.Role, &s.Status, &s.StaffVersion, &s.PersonLinked,
		&s.MFAEnrolled, &s.MFAHardLocked, &last, &s.CreatedAt, &s.StatusChangedAt); err != nil {
		return Staff{}, err
	}
	if tenant != nil {
		s.TenantID = *tenant
	}
	s.LastLoginAt = last
	return s, nil
}

func loadStaff(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Staff, error) {
	s, err := scanStaff(tx.QueryRow(ctx, staffSelect+` WHERE su.id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Staff{}, ErrNotFound
	}
	return s, err
}

// readScope runs a read in the directory scope: a tenant (WithTenant) or the platform (WithoutTenant). A tenant
// caller may read only its own tenant; a platform staff read needs a platform caller.
func (s *Service) readScope(ctx context.Context, viewer Actor, tenantID uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	switch {
	case tenantID == uuid.Nil:
		if !viewer.Platform() {
			return ErrForbidden
		}
		return s.pool.WithoutTenant(ctx, fn)
	case viewer.Platform() || viewer.TenantID == tenantID:
		return s.pool.WithTenant(ctx, tenantID, fn)
	default:
		return ErrNotFound
	}
}

// DirectoryFilter narrows the directory (closed vocabularies; validated by the caller).
type DirectoryFilter struct {
	Status string
	Role   string
}

// ListStaff is the directory of one tenant (tenantID) or of the platform staff (uuid.Nil). The scope's RLS also
// guarantees that a tenant listing never contains a platform row.
func (s *Service) ListStaff(ctx context.Context, viewer Actor, tenantID uuid.UUID, f DirectoryFilter) ([]Staff, bool, error) {
	var out []Staff
	err := s.readScope(ctx, viewer, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, staffSelect+`
			WHERE su.tenant_id IS NOT DISTINCT FROM $1::uuid
			  AND ($2 = '' OR su.status = $2) AND ($3 = '' OR su.role = $3)
			ORDER BY su.email, su.id LIMIT $4`, nilIfZero(tenantID), f.Status, f.Role, MaxDirectory+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			st, err := scanStaff(rows)
			if err != nil {
				return err
			}
			out = append(out, st)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, false, err
	}
	truncated := len(out) > MaxDirectory
	if truncated {
		out = out[:MaxDirectory]
	}
	return out, truncated, nil
}

// GetStaff reads one staff member of tenantID (uuid.Nil: platform). Another scope's row is ErrNotFound.
func (s *Service) GetStaff(ctx context.Context, viewer Actor, tenantID, staffID uuid.UUID) (Staff, error) {
	var out Staff
	err := s.readScope(ctx, viewer, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		st, err := loadStaff(ctx, tx, staffID)
		if err != nil {
			return err
		}
		if st.TenantID != tenantID {
			return ErrNotFound
		}
		out = st
		return nil
	})
	return out, err
}

// ListEvents is a staff member's lifecycle history (newest first). ActorIsTenantStaff is set when the actor is a
// staff member of the target's tenant (visible in this scope): a tenant viewer sees only those actor ids.
func (s *Service) ListEvents(ctx context.Context, viewer Actor, tenantID, staffID uuid.UUID) ([]Event, error) {
	var out []Event
	err := s.readScope(ctx, viewer, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		st, err := loadStaff(ctx, tx, staffID)
		if err != nil {
			return err
		}
		if st.TenantID != tenantID {
			return ErrNotFound
		}
		rows, err := tx.Query(ctx, `SELECT e.id, e.staff_user_id, e.event, e.actor_type, e.actor_staff_id,
				(a.id IS NOT NULL AND a.tenant_id IS NOT NULL),
				e.before, e.after, e.reason_code, e.note, e.lifecycle_request_id, e.created_at
			  FROM staff_lifecycle_events e
			  LEFT JOIN staff_users a ON a.id = e.actor_staff_id
			 WHERE e.staff_user_id = $1
			 ORDER BY e.created_at DESC, e.id DESC LIMIT $2`, staffID, MaxEvents)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var ev Event
			if err := rows.Scan(&ev.ID, &ev.StaffUserID, &ev.Event, &ev.ActorType, &ev.ActorStaffID, &ev.ActorIsTenantStaff,
				&ev.Before, &ev.After, &ev.ReasonCode, &ev.Note, &ev.LifecycleRequestID, &ev.CreatedAt); err != nil {
				return err
			}
			out = append(out, ev)
		}
		return rows.Err()
	})
	return out, err
}

func nilIfZero(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}
