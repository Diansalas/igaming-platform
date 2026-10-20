package withdrawalpolicy

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Effective is the operator read API: for one tenant, every registry cause (configurable or not), the effective action
// per stage of each configurable cause with its source (configured / default_unconfigured), the policy set version, and
// the UNCONFIGURED notice wherever the interim default is in force. A tenant actor reads its own tenant only (RLS and the
// actor check); a platform actor any tenant.
func (s *Service) Effective(ctx context.Context, a Actor, tenantID uuid.UUID) (*EffectiveView, error) {
	if err := requireReader(a, tenantID); err != nil {
		return nil, err
	}
	if tenantID == uuid.Nil {
		return nil, fmt.Errorf("%w: tenant_id is required", ErrInvalidInput)
	}
	var out EffectiveView
	err := s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		out = EffectiveView{TenantID: tenantID, Disclaimer: Disclaimer}
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants WHERE id = $1)`, tenantID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
		if err := tx.QueryRow(ctx, `SELECT withdrawal_restriction_set_version($1)`, tenantID).Scan(&out.SetVersion); err != nil {
			return err
		}
		out.Configured = out.SetVersion > 0
		rows, err := tx.Query(ctx, `SELECT cause, cause_class, configurable, evaluated, governing_reference, current_behaviour
			FROM withdrawal_restriction_causes ORDER BY configurable DESC, cause_class, cause`)
		if err != nil {
			return err
		}
		var causes []CauseView
		for rows.Next() {
			var cv CauseView
			if err := rows.Scan(&cv.Cause, &cv.Class, &cv.Configurable, &cv.Evaluated, &cv.GoverningReference, &cv.CurrentBehaviour); err != nil {
				rows.Close()
				return err
			}
			causes = append(causes, cv)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range causes {
			if !causes[i].Configurable {
				continue
			}
			st, err := currentEffects(ctx, tx, tenantID, causes[i].Cause)
			if err != nil {
				return err
			}
			causes[i].Stages = st
			causes[i].Unconfigured = true
			for _, e := range st {
				if e.Source == SourceConfigured {
					causes[i].Unconfigured = false
				}
			}
			if causes[i].Unconfigured {
				causes[i].Notice = UnconfiguredNotice
			}
		}
		out.Causes = causes
		out.ActionSemantics = ActionSemantics
		if a.Scope != ScopeTenant {
			out.Parked = ParkedSummary{Note: ParkedPlatformNote}
			return nil
		}
		p, err := parkedSummary(ctx, tx, tenantID)
		out.Parked = p
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return &out, nil
}

// History returns the tenant's policy versions, newest first (append-only; a tenant actor reads its own).
func (s *Service) History(ctx context.Context, a Actor, tenantID uuid.UUID) ([]Policy, error) {
	if err := requireReader(a, tenantID); err != nil {
		return nil, err
	}
	var out []Policy
	err := s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, `SELECT `+policyColumns+` FROM withdrawal_restriction_policies WHERE tenant_id = $1 ORDER BY set_version DESC`, tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p Policy
			if err := scanPolicy(rows, &p); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, classify(err)
	}
	return out, nil
}

// GetChange returns one change with its approvals (platform actors only: change requests are platform records).
func (s *Service) GetChange(ctx context.Context, a Actor, changeID uuid.UUID) (*Change, error) {
	if err := validateActor(a); err != nil {
		return nil, err
	}
	if a.Scope != ScopePlatform {
		return nil, ErrNotFound
	}
	if err := requireReader(a, uuid.Nil); err != nil {
		return nil, err
	}
	var out Change
	err := s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		c, err := readChange(ctx, tx, changeID, false)
		if err != nil {
			return err
		}
		c.Approvals, err = loadApprovals(ctx, tx, c.ID)
		out = c
		return err
	})
	if err != nil {
		return nil, classify(err)
	}
	return &out, nil
}

// ListChanges returns changes, newest first (platform actors only).
func (s *Service) ListChanges(ctx context.Context, a Actor, f ListFilter) ([]Change, error) {
	if err := validateActor(a); err != nil {
		return nil, err
	}
	if a.Scope != ScopePlatform {
		return nil, fmt.Errorf("%w: change requests are platform records", ErrForbidden)
	}
	if err := requireReader(a, uuid.Nil); err != nil {
		return nil, err
	}
	switch f.Status {
	case "", StatusRequested, StatusApproved, StatusExecuted, StatusRejected, StatusCancelled, StatusExpired:
	default:
		return nil, fmt.Errorf("%w: unknown status filter", ErrInvalidInput)
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 100
	}
	var out []Change
	err := s.run(ctx, a, func(ctx context.Context, tx pgx.Tx) error {
		out = nil
		rows, err := tx.Query(ctx, `SELECT `+changeColumns+` FROM withdrawal_restriction_policy_changes
			WHERE ($1::uuid IS NULL OR tenant_id = $1) AND ($2 = '' OR status = $2)
			ORDER BY created_at DESC, id LIMIT $3`, nullableUUID(f.TenantID), f.Status, f.Limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c Change
			if err := scanChange(rows, &c); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, err
		}
		return nil, classify(err)
	}
	return out, nil
}

func nullableUUID(u uuid.UUID) *uuid.UUID {
	if u == uuid.Nil {
		return nil
	}
	return &u
}

// parkedSummary reads the tenant's open withdrawals whose latest dispatch gate decision is a WD056 refusal (lic17 M-1), in the
// tenant's own session. "Parked since" is the earliest WD056 refusal recorded for the request.
func parkedSummary(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (ParkedSummary, error) {
	out := ParkedSummary{Visible: true, Note: ParkedTenantNote}
	err := tx.QueryRow(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (d.withdrawal_request_id) d.withdrawal_request_id, d.allowed, d.gate_code
			  FROM withdrawal_gate_decisions d
			 WHERE d.tenant_id = $1
			 ORDER BY d.withdrawal_request_id, d.decided_xact DESC, d.decided_at DESC
		), parked AS (
			SELECT (SELECT min(d2.decided_at) FROM withdrawal_gate_decisions d2
			         WHERE d2.withdrawal_request_id = l.withdrawal_request_id AND d2.tenant_id = $1
			           AND NOT d2.allowed AND d2.gate_code = 'WD056') AS since
			  FROM latest l
			  JOIN withdrawal_requests wr ON wr.id = l.withdrawal_request_id AND wr.tenant_id = $1
			 WHERE NOT l.allowed AND l.gate_code = 'WD056' AND wr.state IN ('approved', 'submitted')
		)
		SELECT count(*), min(since), floor(EXTRACT(EPOCH FROM clock_timestamp() - min(since)))::bigint FROM parked`, tenantID).
		Scan(&out.Count, &out.OldestParkedSince, &out.OldestAgeSeconds)
	if err != nil {
		return ParkedSummary{}, fmt.Errorf("withdrawalpolicy: read parked withdrawals: %w", err)
	}
	return out, nil
}
