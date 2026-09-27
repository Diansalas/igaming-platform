// PRH-I1: the payment kill switch (ADR 0095 §10.2-§10.4, migration 0105).
// Every function here is a thin, tenant/platform-RLS-scoped wrapper around
// the migration 0105 tables. The actor and scope columns are NEVER set by
// this package - they are forced from the database session by the
// migration's own triggers (payment_kill_switch_session()), so this file
// never even attempts to pass one. This mirrors attempt.go's own division
// of labour: the CAS predicate/guard trigger enforce the rule from two
// independent layers, but the ACTOR is single-sourced from the DB alone.
//
// Callers are expected to write an audit.Record entry (actor, tenant,
// entity, before/after, IP, UA, reason code) in the SAME transaction
// (CLAUDE.md's audit rule) and to raise an alert on every successful
// Engage (S95-C7) - neither is this package's job, mirroring every other
// admin-write function in this codebase (e.g. capability.go's
// WriteCapability leaves audit.Record to its caller).
package payments

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// KillSwitchOperationScope mirrors payment_kill_switches.operation_scope.
type KillSwitchOperationScope string

const (
	KillSwitchOperationDeposit KillSwitchOperationScope = "deposit"
	KillSwitchOperationPayout  KillSwitchOperationScope = "payout"
	KillSwitchOperationAny     KillSwitchOperationScope = "*"
)

// KillSwitchSessionScope mirrors both engaged_by_scope/changed_by_scope and
// requested_by_scope/approved_by_scope - always exactly 'tenant' or
// 'platform', forced by the database from the transaction's own session
// GUCs (§10.2.1), never asserted here.
type KillSwitchSessionScope string

const (
	KillSwitchScopeTenant   KillSwitchSessionScope = "tenant"
	KillSwitchScopePlatform KillSwitchSessionScope = "platform"
)

// KillSwitch mirrors one payment_kill_switches row.
type KillSwitch struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	ProviderScope    string // "*" or a provider_id
	OperationScope   KillSwitchOperationScope
	Engaged          bool
	EngagedByScope   *KillSwitchSessionScope
	ReleaseRequestID *uuid.UUID
	ReasonCode       string
	ChangedBy        uuid.UUID
	ChangedByScope   KillSwitchSessionScope
	ChangedAt        time.Time
	Version          int64
	CreatedAt        time.Time
}

// KillSwitchReleaseRequest mirrors one payment_kill_switch_release_requests row.
type KillSwitchReleaseRequest struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	KillSwitchID     uuid.UUID
	ExpectedVersion  int64
	ReasonCode       string
	RequestedBy      uuid.UUID
	RequestedByScope KillSwitchSessionScope
	CreatedAt        time.Time
	ExpiresAt        time.Time
	Status           string
	ApprovedBy       *uuid.UUID
	ApprovedByScope  *KillSwitchSessionScope
	DecidedAt        *time.Time
}

const killSwitchColumns = `id, tenant_id, provider_scope, operation_scope, engaged, engaged_by_scope,
	release_request_id, reason_code, changed_by, changed_by_scope, changed_at, version, created_at`

func scanKillSwitch(row rowScanner) (KillSwitch, error) {
	var ks KillSwitch
	var engagedByScope *string
	var opScope string
	var changedByScope string
	err := row.Scan(
		&ks.ID, &ks.TenantID, &ks.ProviderScope, &opScope, &ks.Engaged, &engagedByScope,
		&ks.ReleaseRequestID, &ks.ReasonCode, &ks.ChangedBy, &changedByScope, &ks.ChangedAt, &ks.Version, &ks.CreatedAt,
	)
	if err != nil {
		return KillSwitch{}, err
	}
	ks.OperationScope = KillSwitchOperationScope(opScope)
	ks.ChangedByScope = KillSwitchSessionScope(changedByScope)
	if engagedByScope != nil {
		s := KillSwitchSessionScope(*engagedByScope)
		ks.EngagedByScope = &s
	}
	return ks, nil
}

// EngageKillSwitch performs the single-actor engage (ADR 0095 §10.4). tx
// must already be tenant-scoped (db.Pool.WithPrincipalScope, tenant staff
// route) or platform-scoped (db.Pool.WithPlatformAdmin, platform route) -
// the actor/scope columns come from that session alone. Idempotent on
// (tenantID, providerScope, operationScope): engaging an already-engaged
// row updates its reason_code and bumps version (a re-engage/reason
// update), rather than erroring, which is deliberate - a second incident
// responder reaching for the same switch should never see a conflict
// error for "the exact thing they were trying to do already happened".
func EngageKillSwitch(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerScope string, operationScope KillSwitchOperationScope, reasonCode string) (KillSwitch, error) {
	if reasonCode == "" {
		return KillSwitch{}, fmt.Errorf("payments: EngageKillSwitch: reason_code is required")
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO payment_kill_switches (id, tenant_id, provider_scope, operation_scope, engaged, reason_code)
		 VALUES (gen_random_uuid(), $1, $2, $3, true, $4)
		 ON CONFLICT (tenant_id, provider_scope, operation_scope)
		 DO UPDATE SET engaged = true, reason_code = EXCLUDED.reason_code
		 RETURNING `+killSwitchColumns,
		tenantID, providerScope, string(operationScope), reasonCode,
	)
	return scanKillSwitch(row)
}

// GetKillSwitch reads one switch row by its natural key, under the
// caller's own tenant or platform RLS context.
func GetKillSwitch(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerScope string, operationScope KillSwitchOperationScope) (KillSwitch, bool, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+killSwitchColumns+` FROM payment_kill_switches WHERE tenant_id=$1 AND provider_scope=$2 AND operation_scope=$3`,
		tenantID, providerScope, string(operationScope),
	)
	ks, err := scanKillSwitch(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return KillSwitch{}, false, nil
	}
	if err != nil {
		return KillSwitch{}, false, fmt.Errorf("payments: get kill switch: %w", err)
	}
	return ks, true, nil
}

// ListKillSwitches returns every switch row visible to the caller's
// current RLS context for tenantID (tenant staff see only their own
// tenant's rows, including platform-engaged ones read-only per §10.5;
// a platform session filters explicitly on tenantID since the platform
// RLS family has no tenant predicate of its own).
func ListKillSwitches(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]KillSwitch, error) {
	rows, err := tx.Query(ctx, `SELECT `+killSwitchColumns+` FROM payment_kill_switches WHERE tenant_id=$1 ORDER BY provider_scope, operation_scope`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("payments: list kill switches: %w", err)
	}
	defer rows.Close()
	var out []KillSwitch
	for rows.Next() {
		ks, err := scanKillSwitch(rows)
		if err != nil {
			return nil, fmt.Errorf("payments: scan kill switch: %w", err)
		}
		out = append(out, ks)
	}
	return out, rows.Err()
}

// RequestKillSwitchRelease files the release request half of four-eyes
// release (§10.4). ttl bounds expires_at; the trigger independently forces
// it to at most now()+24h regardless of what is passed here (defense in
// depth - this parameter only ever narrows, never widens, that ceiling).
func RequestKillSwitchRelease(ctx context.Context, tx pgx.Tx, tenantID, killSwitchID uuid.UUID, expectedVersion int64, reasonCode string, ttl time.Duration) (KillSwitchReleaseRequest, error) {
	if reasonCode == "" {
		return KillSwitchReleaseRequest{}, fmt.Errorf("payments: RequestKillSwitchRelease: reason_code is required")
	}
	if ttl <= 0 || ttl > 24*time.Hour {
		ttl = 24 * time.Hour
	}
	row := tx.QueryRow(ctx,
		`INSERT INTO payment_kill_switch_release_requests (id, tenant_id, kill_switch_id, expected_version, reason_code, expires_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, now() + $5::interval)
		 RETURNING id, tenant_id, kill_switch_id, expected_version, reason_code, requested_by, requested_by_scope, created_at, expires_at, status`,
		tenantID, killSwitchID, expectedVersion, reasonCode, ttl.String(),
	)
	var req KillSwitchReleaseRequest
	var scope string
	if err := row.Scan(&req.ID, &req.TenantID, &req.KillSwitchID, &req.ExpectedVersion, &req.ReasonCode,
		&req.RequestedBy, &scope, &req.CreatedAt, &req.ExpiresAt, &req.Status); err != nil {
		return KillSwitchReleaseRequest{}, fmt.Errorf("payments: request kill switch release: %w", err)
	}
	req.RequestedByScope = KillSwitchSessionScope(scope)
	return req, nil
}

// ApproveAndReleaseKillSwitch performs the approval and the release UPDATE
// in ONE transaction (§10.2.2 point 6's "decided_txid = txid_current()"
// requirement), so a request approved in any earlier transaction can never
// release. tx must be a DIFFERENT principal's session from the one that
// created requestID - the database itself refuses a same-principal
// approval (four-eyes), this function does not re-check it.
func ApproveAndReleaseKillSwitch(ctx context.Context, tx pgx.Tx, killSwitchID, requestID uuid.UUID) (KillSwitch, error) {
	if _, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='approved' WHERE id=$1`, requestID); err != nil {
		return KillSwitch{}, fmt.Errorf("payments: approve kill switch release request: %w", err)
	}
	row := tx.QueryRow(ctx,
		`UPDATE payment_kill_switches SET engaged=false, release_request_id=$2 WHERE id=$1 RETURNING `+killSwitchColumns,
		killSwitchID, requestID,
	)
	ks, err := scanKillSwitch(row)
	if err != nil {
		return KillSwitch{}, fmt.Errorf("payments: release kill switch: %w", err)
	}
	return ks, nil
}

// CancelKillSwitchRelease withdraws an open request before it is approved.
func CancelKillSwitchRelease(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE payment_kill_switch_release_requests SET status='cancelled' WHERE id=$1 AND status='open'`, requestID)
	if err != nil {
		return fmt.Errorf("payments: cancel kill switch release request: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("payments: cancel kill switch release request: %w", ErrAttemptStateConflict)
	}
	return nil
}

// KillSwitchEngaged is a non-atomic, read-only check for orchestration
// code that needs to CHOOSE a terminal reason (e.g. T3's 'kill_switch'
// decline reason) after a claim statement has already refused atomically
// for the real safety property (§10.3) - never a substitute for the
// in-statement predicate, which is what actually prevents the call.
func KillSwitchEngaged(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, operation AttemptOperation) (bool, error) {
	var engaged bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM payment_kill_switches k
			WHERE k.tenant_id = $1 AND k.engaged
			  AND k.provider_scope IN ('*', $2)
			  AND k.operation_scope IN ('*', $3)
		)`,
		tenantID, providerID, string(operation),
	).Scan(&engaged)
	if err != nil {
		return false, fmt.Errorf("payments: check kill switch engagement: %w", err)
	}
	return engaged, nil
}
