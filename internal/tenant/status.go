package tenant

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// CodeCloseBlockedOpenRounds is the stable machine-readable code of a refused
// tenant closure (owner decision Q-GP-1, 2026-10-06, ADR 0095 section 40.6).
// An HTTP surface answers it 409 with apierror.CodeTenantCloseBlockedOpenRounds.
const CodeCloseBlockedOpenRounds = "TENANT_CLOSE_BLOCKED_OPEN_ROUNDS"

// sqlStateCloseBlocked is raised by the tenants_status_change_gate trigger
// (migration 0121) for a transition INTO 'closed' while open gaming rounds
// exist. The trigger is the authority: it runs inside the status-changing
// transaction under the same advisory lock pair the gameplay gate uses, so
// there is no check-then-act window, and it covers every writer of
// tenants.status, not just ChangeStatus.
const sqlStateCloseBlocked = "GP020"

// ErrCloseBlockedOpenRounds is the sentinel for a refused closure.
var ErrCloseBlockedOpenRounds = errors.New("tenant: closure refused: open gaming rounds exist")

// CloseBlockedError carries the COUNTS (only) behind a refused closure. The
// counts are the closing tenant's own (the database function counts under that
// tenant's scope); no round, bet or player identifier is ever included.
//
// Casino: a casino round has no representable open state in the data model
// (see ADR 0095 section 40.6, Q-GP-6), so only sportsbook open bets are
// counted today.
type CloseBlockedError struct {
	SportsbookOpenBets int64
}

func (e *CloseBlockedError) Error() string {
	return fmt.Sprintf("%s: sportsbook_open_bets=%d", ErrCloseBlockedOpenRounds, e.SportsbookOpenBets)
}

// Is makes errors.Is(err, ErrCloseBlockedOpenRounds) true for every
// *CloseBlockedError.
func (e *CloseBlockedError) Is(target error) bool { return target == ErrCloseBlockedOpenRounds }

// TranslateStatusChangeError maps the closure trigger's SQLSTATE GP020 (from
// ANY writer of tenants.status) into a *CloseBlockedError. Every other error
// is returned unchanged. A GP020 whose DETAIL cannot be parsed still maps to
// the typed error (counts zero): the refusal itself is never lost.
func TranslateStatusChangeError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != sqlStateCloseBlocked {
		return err
	}
	out := &CloseBlockedError{}
	for _, kv := range strings.Split(pgErr.Detail, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		n, perr := strconv.ParseInt(v, 10, 64)
		if perr != nil {
			continue
		}
		if k == "sportsbook_open_bets" {
			out.SportsbookOpenBets = n
		}
	}
	return out
}

// StatusRunner runs fn in a platform-admin-scoped transaction (satisfied by
// *db.Pool). Changing tenants.status is a platform operation: the
// tenants_platform_admin_update policy admits no tenant-scoped writer.
type StatusRunner interface {
	WithPlatformAdmin(ctx context.Context, principalID uuid.UUID, fn db.TxFunc) error
}

// ChangeStatusParams is ChangeStatus's input. ActorID must be the
// authenticated platform-scoped staff principal resolved server-side, never a
// client value. ReasonCode is required (CLAUDE.md audit rule).
type ChangeStatusParams struct {
	TenantID   uuid.UUID
	NewStatus  string
	ActorID    uuid.UUID
	ReasonCode string
	IPAddress  string
	RequestID  string
}

// ErrInvalidStatusChange marks a malformed ChangeStatus request.
var ErrInvalidStatusChange = errors.New("tenant: invalid status change")

// ErrUnknownTenant is returned when the tenant row does not exist.
var ErrUnknownTenant = errors.New("tenant: unknown tenant")

const (
	auditActionStatusChanged       = "tenant.status_changed"
	auditActionCloseRefusedOpenRds = "tenant.status_change_refused_open_rounds"
)

// ChangeStatus moves a tenant between active, suspended and closed, audited.
//
// The transaction takes SET LOCAL lock_timeout (the status change waits for
// in-flight gameplay postings: runbook, ADR 0095 section 40.5) and updates
// tenants.status. The closure refusal is NOT decided here: the database
// trigger decides it atomically under the advisory lock; this function only
// translates GP020 into *CloseBlockedError and then writes a durable audit
// row for the REFUSAL in a SEPARATE transaction (the refused transaction
// rolled back, and with it anything written inside it). The refusal audit row
// carries counts only. Suspension and reactivation are not subject to the
// closure check. The operator's approved sequence to close a tenant with open
// bets is to resolve them first (void, or settle) while the tenant is still
// active (or suspended, for a void), then close.
func ChangeStatus(ctx context.Context, runner StatusRunner, p ChangeStatusParams) error {
	switch p.NewStatus {
	case "active", "suspended", "closed":
	default:
		return fmt.Errorf("%w: status %q", ErrInvalidStatusChange, p.NewStatus)
	}
	if p.TenantID == uuid.Nil || p.ActorID == uuid.Nil {
		return fmt.Errorf("%w: tenant id and actor id are required", ErrInvalidStatusChange)
	}
	if strings.TrimSpace(p.ReasonCode) == "" {
		return fmt.Errorf("%w: reason_code is required", ErrInvalidStatusChange)
	}

	err := runner.WithPlatformAdmin(ctx, p.ActorID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '5s'`); err != nil {
			return fmt.Errorf("tenant: set lock_timeout: %w", err)
		}
		var before string
		err := tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, p.TenantID).Scan(&before)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrUnknownTenant
		}
		if err != nil {
			return fmt.Errorf("tenant: read status: %w", err)
		}
		if before == p.NewStatus {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE tenants SET status = $1, updated_at = now() WHERE id = $2`, p.NewStatus, p.TenantID); err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			ActorType: audit.ActorStaff, ActorID: p.ActorID,
			Action: auditActionStatusChanged, TargetType: "tenant", TargetID: p.TenantID.String(),
			Outcome: audit.OutcomeSuccess, IPAddress: p.IPAddress, RequestID: p.RequestID,
			Metadata: map[string]any{"before": before, "after": p.NewStatus, "reason_code": p.ReasonCode},
		})
	})
	if err == nil {
		return nil
	}
	translated := TranslateStatusChangeError(err)
	var blocked *CloseBlockedError
	if !errors.As(translated, &blocked) {
		return translated
	}
	auditErr := runner.WithPlatformAdmin(ctx, p.ActorID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			ActorType: audit.ActorStaff, ActorID: p.ActorID,
			Action: auditActionCloseRefusedOpenRds, TargetType: "tenant", TargetID: p.TenantID.String(),
			Outcome: audit.OutcomeDenied, IPAddress: p.IPAddress, RequestID: p.RequestID,
			Metadata: map[string]any{
				"requested_status": p.NewStatus, "reason_code": p.ReasonCode,
				"code": CodeCloseBlockedOpenRounds, "sportsbook_open_bets": blocked.SportsbookOpenBets,
			},
		})
	})
	if auditErr != nil {
		// The refusal stands; the failed audit write is reported with it.
		return fmt.Errorf("%w (audit of the refusal failed: %v)", blocked, auditErr)
	}
	return blocked
}
