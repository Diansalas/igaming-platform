package launchgov

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerkind"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Pool is the database handle launchgov needs (satisfied by *db.Pool).
type Pool interface {
	WithPlatformAdmin(ctx context.Context, principalID uuid.UUID, fn db.TxFunc) error
	WithPrincipalScope(ctx context.Context, tenantID, principalID uuid.UUID, fn db.TxFunc) error
	WithTenantReadOnly(ctx context.Context, tenantID uuid.UUID, fn db.TxFunc) error
}

// Config wires a Service. Pool is required; everything else has a safe default.
type Config struct {
	Pool Pool
	// Proofs signs the ADR 0110 actor proofs. nil means actorproof.Default(); with neither,
	// every governed write fails closed (ErrProofUnavailable).
	Proofs *actorproof.Issuer
	// Environment is recorded in the readiness snapshot (config.GuardEnvironment()).
	Environment string
	// Components are the process's wired provider components (R-PROVIDER-TIER). Empty means the
	// check is not_evaluable.
	Components []providerkind.Registration
	// LockTimeout is SET LOCAL lock_timeout of every governed transaction (default 5s).
	LockTimeout string
	// SuspendAttempts bounds the executor's retries of a suspension on a lock timeout (default 3).
	SuspendAttempts int
	// Backoff is the wait before retry n (n >= 1). Default 100ms * n.
	Backoff func(n int) time.Duration
	// ApprovalMaxAge bounds how old an approval may be at execution (default DefaultApprovalMaxAge).
	ApprovalMaxAge time.Duration
	// Clock is injectable for tests (default time.Now).
	Clock func() time.Time
}

// Service is the launch authorisation service. It holds no per-request state.
type Service struct {
	cfg Config
}

// New builds a Service.
func New(cfg Config) (*Service, error) {
	if cfg.Pool == nil {
		return nil, errors.New("launchgov: a pool is required")
	}
	if cfg.LockTimeout == "" {
		cfg.LockTimeout = "5s"
	}
	if cfg.SuspendAttempts <= 0 {
		cfg.SuspendAttempts = 3
	}
	if cfg.Backoff == nil {
		cfg.Backoff = func(n int) time.Duration { return time.Duration(n) * 100 * time.Millisecond }
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

// ---- actor / permission checks ------------------------------------------------------------

// authorize checks the static permission, the scope shape and (for a tenant actor) that the
// subject is a brand of the actor's OWN tenant. It is defence in depth behind the route's
// RequirePermission and canActOnTenant, and behind the database's own session checks.
func authorize(a Actor, perm auth.Permission, sub Subject, brandOnlyForTenant bool) error {
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
		if sub.TenantID != a.TenantID {
			// A foreign tenant is indistinguishable from an unknown one.
			return ErrNotFound
		}
		if brandOnlyForTenant && sub.BrandID == nil {
			return fmt.Errorf("%w: a tenant session may act on a brand only", ErrForbidden)
		}
	case actorproof.ScopePlatformActing:
		return ErrActingSession
	default:
		return fmt.Errorf("%w: unknown scope %q", ErrForbidden, a.Scope)
	}
	if !auth.RoleHasPermission(a.Role, perm) {
		return fmt.Errorf("%w: role %q lacks %s", ErrForbidden, a.Role, perm)
	}
	if sub.TenantID == uuid.Nil {
		return fmt.Errorf("%w: tenant id is required", ErrInvalidInput)
	}
	return nil
}

// ---- transactions ---------------------------------------------------------------------------

// runActor opens the actor's session transaction: a platform principal, or a tenant principal
// scoped to its own tenant. It is READ COMMITTED (asserted), with SET LOCAL lock_timeout.
func (s *Service) runActor(ctx context.Context, a Actor, fn db.TxFunc) error {
	wrapped := func(ctx context.Context, tx pgx.Tx) error {
		if err := s.prep(ctx, tx); err != nil {
			return err
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

func (s *Service) prep(ctx context.Context, tx pgx.Tx) error {
	var iso string
	if err := tx.QueryRow(ctx, `SELECT current_setting('transaction_isolation')`).Scan(&iso); err != nil {
		return fmt.Errorf("launchgov: read isolation level: %w", err)
	}
	if iso != "read committed" {
		return ErrIsolationLevel
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, s.cfg.LockTimeout); err != nil {
		return fmt.Errorf("launchgov: set lock_timeout: %w", err)
	}
	return nil
}

// withSubjectTenant runs fn with app.tenant_id bound transaction-locally to the subject's OWN
// tenant (ADR 0112 S6, the migration-0121 technique) for a PLATFORM actor, then restores the
// platform shape (empty tenant) so the decision-record tables see a valid platform session
// again. A tenant actor already holds exactly that tenant, so fn simply runs. The tenant id is a
// server-side value (the locked request row), never a caller value.
func (s *Service) withSubjectTenant(ctx context.Context, tx pgx.Tx, a Actor, tenantID uuid.UUID, fn func() error) error {
	if a.Scope == ScopeTenant {
		return fn()
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return fmt.Errorf("launchgov: bind subject tenant: %w", err)
	}
	if err := fn(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
		return fmt.Errorf("launchgov: restore platform scope: %w", err)
	}
	return nil
}

// attachProof signs and attaches the ADR 0110 proof for the authenticated actor right before
// the governed write it covers.
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

// ---- database error classification ------------------------------------------------------

// GuardError is a database guard refusal (SQLSTATE class LA / AP / GP) the service has no more
// specific sentinel for. Callers branch on Code only.
type GuardError struct {
	Code    string
	Message string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("launchgov: database guard %s: %s", e.Code, e.Message)
}

func pgErr(err error) *pgconn.PgError {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe
	}
	return nil
}

// isRetryableLock reports a lock timeout (55P03) or a deadlock (40P01).
func isRetryableLock(err error) bool {
	pe := pgErr(err)
	return pe != nil && (pe.Code == "55P03" || pe.Code == "40P01")
}

// classify turns a database error into a service error. GP020 becomes tenant.CloseBlockedError;
// a lock timeout becomes ErrRetryableLockTimeout; LA / AP codes become a GuardError; anything
// else is returned unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if isRetryableLock(err) {
		return fmt.Errorf("%w: %v", ErrRetryableLockTimeout, err)
	}
	pe := pgErr(err)
	if pe == nil {
		return err
	}
	if pe.Code == "GP020" {
		return tenant.TranslateStatusChangeError(err)
	}
	if pe.Code == "LA010" {
		// Content and vocabulary are validated in Go before any SQL, and the subject's status was
		// read just before the INSERT, so an LA010 here is the request guard re-reading a subject
		// whose status changed in between (a concurrent governed change): an illegal transition.
		return fmt.Errorf("%w: %s", ErrIllegalTransition, pe.Message)
	}
	if len(pe.Code) == 5 && (pe.Code[:2] == "LA" || pe.Code[:2] == "AP") {
		return &GuardError{Code: pe.Code, Message: pe.Message}
	}
	return err
}

// GuardCode returns the SQLSTATE of a GuardError, or "".
func GuardCode(err error) string {
	var ge *GuardError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ""
}
