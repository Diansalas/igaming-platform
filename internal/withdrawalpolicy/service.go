package withdrawalpolicy

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

// Config wires a Service. Pool is required.
type Config struct {
	Pool Pool
	// Proofs signs the ADR 0110 proofs; nil means actorproof.Default(); with neither, every governed write fails closed.
	Proofs *actorproof.Issuer
	// LockTimeout is SET LOCAL lock_timeout of every transaction (default 5s).
	LockTimeout string
	// ApprovalMaxAge bounds how old a counted approval may be (default 24 h; the database caps it at 24 h).
	ApprovalMaxAge time.Duration
	// Clock is injectable for tests (default time.Now).
	Clock func() time.Time
}

// Service is the withdrawal restriction policy service. It holds no per-request state.
type Service struct{ cfg Config }

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("withdrawalpolicy: a pool is required")
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
	return &Service{cfg: cfg}, nil
}

func (s *Service) now() time.Time { return s.cfg.Clock() }

// validateActor checks the scope shape (defence in depth behind the database's own session checks).
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
	default:
		return fmt.Errorf("%w: unknown scope %q", ErrForbidden, a.Scope)
	}
	return nil
}

// requireChanger: a platform actor with the change permission (platform_admin only).
func requireChanger(a Actor) error {
	if err := validateActor(a); err != nil {
		return err
	}
	if a.Scope != ScopePlatform {
		return fmt.Errorf("%w: only a platform session changes a withdrawal restriction policy", ErrForbidden)
	}
	if !auth.RoleHasPermission(auth.Role(a.Role), auth.PermWithdrawalRestrictionPolicyChange) {
		return fmt.Errorf("%w: role %q lacks %s", ErrForbidden, a.Role, auth.PermWithdrawalRestrictionPolicyChange)
	}
	return nil
}

// requireReader: any actor with the read permission; a tenant actor reads its own tenant only.
func requireReader(a Actor, tenantID uuid.UUID) error {
	if err := validateActor(a); err != nil {
		return err
	}
	if !auth.RoleHasPermission(auth.Role(a.Role), auth.PermWithdrawalRestrictionPolicyRead) {
		return fmt.Errorf("%w: role %q lacks %s", ErrForbidden, a.Role, auth.PermWithdrawalRestrictionPolicyRead)
	}
	if a.Scope == ScopeTenant && tenantID != a.TenantID {
		return ErrNotFound // a foreign tenant is indistinguishable from an unknown one
	}
	return nil
}

// run opens the actor's session transaction (platform principal, or tenant principal scoped to its own tenant), READ
// COMMITTED (asserted), SET LOCAL lock_timeout.
func (s *Service) run(ctx context.Context, a Actor, fn db.TxFunc) error {
	wrapped := func(ctx context.Context, tx pgx.Tx) error {
		var iso string
		if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
			return fmt.Errorf("withdrawalpolicy: read isolation level: %w", err)
		}
		if iso != "read committed" {
			return ErrIsolationLevel
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, s.cfg.LockTimeout); err != nil {
			return fmt.Errorf("withdrawalpolicy: set lock_timeout: %w", err)
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

// attachProof signs and attaches the ADR 0110 proof (platform scope, NULL tenant) right before the governed write it covers.
func (s *Service) attachProof(ctx context.Context, tx pgx.Tx, a Actor, op, target, payloadHash string) error {
	iss := s.cfg.Proofs
	if iss == nil {
		iss = actorproof.Default()
	}
	if iss == nil {
		return ErrProofUnavailable
	}
	return iss.Attach(ctx, tx, actorproof.Claims{
		Actor: a.StaffID, Scope: actorproof.ScopePlatform, Operation: op, Target: target, PayloadHash: payloadHash,
	})
}

// lockTenantPolicy takes the tenant status gate key EXCLUSIVE (ADR 0082 R8: the advisory lock before any row lock of the
// transaction). Every withdrawal decision reads the policy under the same key SHARED (request, T1p), so a change and a
// decision serialise; the 0182 guards take it again (re-entrant).
func lockTenantPolicy(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(tenant_status_gate_key($1))`, tenantID); err != nil {
		return fmt.Errorf("withdrawalpolicy: take the tenant policy lock: %w", err)
	}
	return nil
}

// ---- rows ----------------------------------------------------------------------------------------------------------

const changeColumns = `id, tenant_id, cause, action, reason_code, note, payload_hash, widening, required_approvals, base_set_version,
	requested_by, requested_by_person_id, status, created_at, expires_at, decided_by, decided_at, decision_reason_code,
	executed_by, executed_at`

func scanChange(row pgx.Row, c *Change) error {
	return row.Scan(&c.ID, &c.TenantID, &c.Cause, &c.Action, &c.ReasonCode, &c.Note, &c.PayloadHash, &c.Widening,
		&c.RequiredApprovals, &c.BaseSetVersion, &c.RequestedBy, &c.RequestedByPersonID, &c.Status, &c.CreatedAt, &c.ExpiresAt,
		&c.DecidedBy, &c.DecidedAt, &c.DecisionReasonCode, &c.ExecutedBy, &c.ExecutedAt)
}

const approvalColumns = `id, change_id, approver_id, approver_person_id, payload_hash, reason_code, decided_at`

func scanApproval(row pgx.Row, ap *Approval) error {
	return row.Scan(&ap.ID, &ap.ChangeID, &ap.ApproverID, &ap.ApproverPersonID, &ap.PayloadHash, &ap.ReasonCode, &ap.DecidedAt)
}

const policyColumns = `id, tenant_id, cause, action, set_version, change_id, reason_code, created_by, created_at`

func scanPolicy(row pgx.Row, p *Policy) error {
	return row.Scan(&p.ID, &p.TenantID, &p.Cause, &p.Action, &p.SetVersion, &p.ChangeID, &p.ReasonCode, &p.CreatedBy, &p.CreatedAt)
}

func loadApprovals(ctx context.Context, tx pgx.Tx, changeID uuid.UUID) ([]Approval, error) {
	rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM withdrawal_restriction_policy_approvals WHERE change_id = $1 ORDER BY decided_at, id`, changeID)
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

func readChange(ctx context.Context, tx pgx.Tx, id uuid.UUID, forUpdate bool) (Change, error) {
	q := `SELECT ` + changeColumns + ` FROM withdrawal_restriction_policy_changes WHERE id = $1`
	if forUpdate {
		q += ` FOR UPDATE`
	}
	var c Change
	err := scanChange(tx.QueryRow(ctx, q, id), &c)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// personOf returns the Person of a staff account, or ErrNoPerson (fail closed: "not the same Person" is never inferred from
// missing data).
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

// countApprovals mirrors withdrawal_restriction_fresh_approvals (the database stays authoritative).
func countApprovals(c Change, apps []Approval, now time.Time, maxAge time.Duration) (fresh, distinct int) {
	seen := map[uuid.UUID]bool{c.RequestedByPersonID: true}
	freshSeen := map[uuid.UUID]bool{}
	oldest := now.Add(-maxAge)
	for _, ap := range apps {
		if ap.PayloadHash != c.PayloadHash || ap.ApproverID == c.RequestedBy || ap.ApproverPersonID == c.RequestedByPersonID {
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
	case pe.Code == "23505" && pe.ConstraintName == "uq_withdrawal_restriction_policy_changes_open":
		return ErrDuplicatePending
	case pe.Code == "23505" && strings.HasPrefix(pe.ConstraintName, "uq_withdrawal_restriction_policy_approvals_"):
		return ErrAlreadyDecided
	case pe.Code == "23503":
		return ErrNotFound
	case pe.Code == "WR002":
		return ErrNotConfigurable
	case pe.Code == "WR032" || pe.Code == "WR022":
		return ErrSelfApproval
	case pe.Code == "WR031" || pe.Code == "WR013":
		return fmt.Errorf("%w: %s", ErrNotPending, pe.Message)
	case len(pe.Code) == 5 && (pe.Code[:2] == "WR" || pe.Code[:2] == "LC" || pe.Code[:2] == "AP"):
		return &GuardError{Code: pe.Code, Message: pe.Message}
	}
	return err
}
