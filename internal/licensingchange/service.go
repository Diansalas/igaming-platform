package licensingchange

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// Pool is the database handle the service needs (satisfied by *db.Pool).
type Pool interface {
	WithPlatformAdmin(ctx context.Context, principalID uuid.UUID, fn db.TxFunc) error
	WithPrincipalScope(ctx context.Context, tenantID, principalID uuid.UUID, fn db.TxFunc) error
}

// Config wires a Service. Pool is required; everything else has a safe default.
type Config struct {
	Pool Pool
	// Proofs signs the ADR 0110 actor proofs. nil means actorproof.Default(); with neither, every governed write fails closed
	// (ErrProofUnavailable).
	Proofs *actorproof.Issuer
	// LockTimeout is SET LOCAL lock_timeout of every governed transaction (default 5s).
	LockTimeout string
	// ApprovalMaxAge bounds how old an approval may be at execution (default DefaultApprovalMaxAge; the database caps it at
	// 24 h whatever this says, so a larger value only ever produces an LC020 refusal).
	ApprovalMaxAge time.Duration
	// Clock is injectable for tests (default time.Now).
	Clock func() time.Time
}

// Service is the licensing change service. It holds no per-request state.
type Service struct {
	cfg Config
	// loadApprovals reads a request's approvals. A read error fails the decision CLOSED (nothing is executed, the
	// transaction rolls back). Replaceable only from this package's own tests.
	loadApprovals func(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) ([]Approval, error)
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("licensingchange: a pool is required")
	}
	if cfg.LockTimeout == "" {
		cfg.LockTimeout = "5s"
	}
	if cfg.ApprovalMaxAge <= 0 {
		cfg.ApprovalMaxAge = DefaultApprovalMaxAge
	}
	if cfg.Clock == nil {
		cfg.Clock = time.Now
	}
	return &Service{cfg: cfg, loadApprovals: loadApprovals}, nil
}

func (s *Service) now() time.Time { return s.cfg.Clock() }

// validateActor checks the scope shape (defence in depth behind the routes and the database's own session checks).
func validateActor(a Actor) error {
	if a.StaffID == uuid.Nil {
		return fmt.Errorf("%w: no actor", ErrForbidden)
	}
	switch a.Scope {
	case ScopePlatform:
		if a.TenantID != uuid.Nil {
			return fmt.Errorf("%w: a platform actor carries no tenant", ErrForbidden)
		}
	case ScopeTenant:
		if a.TenantID == uuid.Nil {
			return fmt.Errorf("%w: a tenant actor needs its tenant", ErrForbidden)
		}
	case actorproof.ScopePlatformActing:
		return ErrActingSession
	default:
		return fmt.Errorf("%w: unknown scope %q", ErrForbidden, a.Scope)
	}
	return nil
}

func requirePerm(a Actor, p auth.Permission) error {
	if !auth.RoleHasPermission(auth.Role(a.Role), p) {
		return fmt.Errorf("%w: role %q lacks %s", ErrForbidden, a.Role, p)
	}
	return nil
}

// runActor opens the actor's session transaction (a platform principal, or a tenant principal scoped to its own tenant),
// READ COMMITTED (asserted) with SET LOCAL lock_timeout.
func (s *Service) runActor(ctx context.Context, a Actor, fn db.TxFunc) error {
	wrapped := func(ctx context.Context, tx pgx.Tx) error {
		var iso string
		if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
			return fmt.Errorf("licensingchange: read isolation level: %w", err)
		}
		if iso != "read committed" {
			return ErrIsolationLevel
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, s.cfg.LockTimeout); err != nil {
			return fmt.Errorf("licensingchange: set lock_timeout: %w", err)
		}
		return fn(ctx, tx)
	}
	switch a.Scope {
	case ScopePlatform:
		return s.cfg.Pool.WithPlatformAdmin(ctx, a.StaffID, wrapped)
	case ScopeTenant:
		return s.cfg.Pool.WithPrincipalScope(ctx, a.TenantID, a.StaffID, wrapped)
	}
	return ErrForbidden
}

// attachProof signs and attaches the ADR 0110 proof for the authenticated actor right before the governed write it covers.
func (s *Service) attachProof(ctx context.Context, tx pgx.Tx, a Actor, op, target, payloadHash string) error {
	iss := s.cfg.Proofs
	if iss == nil {
		iss = actorproof.Default()
	}
	if iss == nil {
		return ErrProofUnavailable
	}
	tenantID := uuid.Nil
	if a.Scope == ScopeTenant {
		tenantID = a.TenantID
	}
	return iss.Attach(ctx, tx, actorproof.Claims{
		Actor: a.StaffID, Scope: a.Scope, Tenant: tenantID, Operation: op, Target: target, PayloadHash: payloadHash,
	})
}

// ---- rows ----------------------------------------------------------------------------------------------------------

const requestColumns = `id, kind, payload, payload_hash, reason_code, note, widening, required_approvals, subject_tenant_id,
	subject_licence_id, requested_by, requested_by_person_id, requested_scope, requested_tenant_id, status, created_at, expires_at,
	decided_by, decided_at, decision_reason_code, executed_by, executed_at`

func scanRequest(row pgx.Row, r *Request) error {
	var kind string
	var payload []byte
	if err := row.Scan(&r.ID, &kind, &payload, &r.PayloadHash, &r.ReasonCode, &r.Note, &r.Widening, &r.RequiredApprovals,
		&r.SubjectTenantID, &r.SubjectLicenceID, &r.RequestedBy, &r.RequestedByPersonID, &r.RequestedScope, &r.RequestedTenantID,
		&r.Status, &r.CreatedAt, &r.ExpiresAt, &r.DecidedBy, &r.DecidedAt, &r.DecisionReasonCode, &r.ExecutedBy, &r.ExecutedAt); err != nil {
		return err
	}
	r.Kind = Kind(kind)
	r.Payload = append(r.Payload[:0], payload...)
	return nil
}

const approvalColumns = `id, request_id, approver_id, approver_person_id, payload_hash, reason_code, decided_at`

func scanApproval(row pgx.Row, ap *Approval) error {
	return row.Scan(&ap.ID, &ap.RequestID, &ap.ApproverID, &ap.ApproverPersonID, &ap.PayloadHash, &ap.ReasonCode, &ap.DecidedAt)
}

func loadApprovals(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) ([]Approval, error) {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM licensing_change_approvals WHERE request_id = $1 ORDER BY decided_at, id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Approval
	for rows.Next() {
		var ap Approval
		if err := scanApproval(rows, &ap); err != nil {
			return nil, err
		}
		out = append(out, ap)
	}
	return out, rows.Err()
}

func lockRequest(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Request, error) {
	var r Request
	err := scanRequest(tx.QueryRow(ctx, `SELECT `+requestColumns+` FROM licensing_change_requests WHERE id = $1 FOR UPDATE`, id), &r)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// personOf returns the Person of a staff account, or ErrNoPerson when the row is unreadable or has none (fail closed: "not
// the same Person" is never inferred from missing data).
func personOf(ctx context.Context, tx pgx.Tx, staff uuid.UUID) (uuid.UUID, error) {
	var p *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, staff).Scan(&p)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && p == nil) {
		return uuid.Nil, ErrNoPerson
	}
	if err != nil {
		return uuid.Nil, err
	}
	return *p, nil
}

// countApprovals mirrors licensing_change_fresh_approvals (the database stays authoritative): distinct Persons other than the
// requester's, on the request's payload hash, not by the requester's account. fresh counts those decided within maxAge.
func countApprovals(req Request, apps []Approval, now time.Time, maxAge time.Duration) (fresh, distinct int) {
	seen := map[uuid.UUID]bool{req.RequestedByPersonID: true}
	freshSeen := map[uuid.UUID]bool{}
	oldest := now.Add(-maxAge)
	for _, ap := range apps {
		if ap.PayloadHash != req.PayloadHash || ap.ApproverID == req.RequestedBy || ap.ApproverPersonID == req.RequestedByPersonID {
			continue
		}
		if !seen[ap.ApproverPersonID] {
			seen[ap.ApproverPersonID] = true
			distinct++
		}
		if ap.DecidedAt.After(oldest) && !freshSeen[ap.ApproverPersonID] {
			freshSeen[ap.ApproverPersonID] = true
			fresh++
		}
	}
	return fresh, distinct
}

// ---- database error classification --------------------------------------------------------------------------------

func pgErr(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// classify turns a database error into a service error.
func classify(err error) error {
	if err == nil {
		return nil
	}
	pe := pgErr(err)
	if pe == nil {
		return err
	}
	switch {
	case pe.Code == "55P03" || pe.Code == "40P01":
		return fmt.Errorf("%w: %v", ErrRetryableLockTimeout, err)
	case pe.Code == "23505" && pe.ConstraintName == "uq_licensing_change_requests_open_payload":
		return ErrDuplicatePending
	case pe.Code == "23505" && strings.HasPrefix(pe.ConstraintName, "uq_licensing_change_approvals_"):
		return ErrAlreadyDecided
	case pe.Code == "LC032" || pe.Code == "LC022":
		return ErrSelfApproval
	case pe.Code == "LC031" || pe.Code == "LC013":
		return fmt.Errorf("%w: %s", ErrNotPending, pe.Message)
	case len(pe.Code) == 5 && (pe.Code[:2] == "LC" || pe.Code[:2] == "AP"):
		g := &GuardError{Code: pe.Code, Message: pe.Message}
		if pe.Code == "LC050" {
			g.Hint = pe.Hint
		}
		return g
	}
	return err
}
