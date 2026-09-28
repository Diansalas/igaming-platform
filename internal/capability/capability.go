// Package capability is the PRH-2 K1 service layer for ADR 0099's scoped
// financial capability grants: requesting, platform-co-approving,
// rejecting, cancelling, revoking and listing a
// staff_capability_grant_requests / staff_capability_grant_approvals /
// staff_capability_grants row set (migration 0112).
//
// This package is deliberately thin. Every real invariant (R-1..R-13,
// the exact-Person-distinctness rule, the request/approval/grant
// transactional coupling, append-only + one-way-revoke) is enforced by
// migration 0112's own triggers, not here - the two-layer design ADR
// 0099 §2.1 describes ("the static permission ... and an in-force grant
// row read in the action's own transaction") puts the actual authority
// decision in the database, on purpose, so it holds regardless of what
// any future caller of this package does. This package's own job is:
// build the right INSERT/UPDATE, run it inside the right session shape
// (db.WithPrincipalScope for a tenant caller, db.WithPlatformAdmin for a
// platform caller - migration 0112's financial_actor_session() requires
// exactly one of those two shapes for every K1 write), classify the
// resulting Postgres error by its 'CG' SQLSTATE class, and write the
// audit row.
package capability

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Capability is one of the four closed, extensible ADR 0099 §3.1
// financial capabilities.
type Capability string

const (
	CapabilityLedgerAdjustmentInitiate   Capability = "ledger_adjustment:initiate"
	CapabilityLedgerAdjustmentApprove    Capability = "ledger_adjustment:approve"
	CapabilityPaymentForceResolveRequest Capability = "payment_force_resolve:request"
	CapabilityPaymentForceResolveApprove Capability = "payment_force_resolve:approve"
)

// RequestStatus mirrors staff_capability_grant_requests.status.
type RequestStatus string

const (
	RequestPending   RequestStatus = "pending"
	RequestApproved  RequestStatus = "approved"
	RequestRejected  RequestStatus = "rejected"
	RequestCancelled RequestStatus = "cancelled"
	RequestExpired   RequestStatus = "expired"
)

// Request is one staff_capability_grant_requests row.
type Request struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	GranteeStaffID      uuid.UUID
	GranteeScope        string
	Capability          Capability
	ValidFrom           time.Time
	ValidUntil          *time.Time
	ReasonCode          string
	RequestedBy         uuid.UUID
	RequestedByScope    string
	RequestedByPersonID uuid.UUID
	Status              RequestStatus
	CreatedAt           time.Time
	ExpiresAt           time.Time
}

// Grant is one staff_capability_grants row.
type Grant struct {
	ID               uuid.UUID
	TenantID         uuid.UUID
	GranteeStaffID   uuid.UUID
	GranteeScope     string
	Capability       Capability
	RequestID        uuid.UUID
	ApprovalID       uuid.UUID
	ValidFrom        time.Time
	ValidUntil       *time.Time
	GrantedAt        time.Time
	RevokedAt        *time.Time
	RevokedBy        *uuid.UUID
	RevokedByScope   *string
	RevokeReasonCode *string
}

// NewRequestInput is what a caller supplies to CreateRequest. Every
// identity/actor column (requested_by, requested_by_scope,
// requested_by_person_id, status, created_at, expires_at, grantee_scope)
// is forced by the migration 0112 trigger from the DB session - this
// struct intentionally has no field for any of them.
type NewRequestInput struct {
	GranteeStaffID uuid.UUID
	Capability     Capability
	ValidFrom      time.Time
	ValidUntil     *time.Time
	ReasonCode     string
}

// ErrClass classifies a capability-grant database error by its SQLSTATE
// class ('CG', migration 0112's header). Application code branches on
// this, never on message text.
type ErrClass string

const (
	// ErrClassGuardRefusal covers CG010/CG011/CG012/CG099 - an invariant
	// refusal (R-1..R-13, append-only, etc.). Maps to a 409 Conflict at
	// the HTTP layer, the same convention this codebase's kill-switch
	// handlers already use for a trigger refusal.
	ErrClassGuardRefusal ErrClass = "guard_refusal"
	// ErrClassSessionInvalid covers CG001/CG002/CG020 - the actor session
	// itself could not be resolved or is not valid. This should not occur
	// for an HTTP caller that already passed auth.RequirePermission, and
	// is treated as an internal error if it ever does.
	ErrClassSessionInvalid ErrClass = "session_invalid"
	ErrClassOther          ErrClass = "other"
)

// ClassifyError inspects err for a *pgconn.PgError and returns its class.
func ClassifyError(err error) ErrClass {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ErrClassOther
	}
	switch pgErr.Code {
	case "CG010", "CG011", "CG012", "CG099":
		return ErrClassGuardRefusal
	case "CG001", "CG002", "CG020":
		return ErrClassSessionInvalid
	default:
		return ErrClassOther
	}
}

// CreateRequest inserts a staff_capability_grant_requests row. tenantID
// must be the request's own tenant (the tenant a tenant caller belongs
// to, or the platform grantee's target tenant for a platform caller) -
// migration 0112's guard trigger (R-5/R-6) independently refuses a
// mismatched value, this is not the authoritative check.
func CreateRequest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, in NewRequestInput) (Request, error) {
	var req Request
	row := tx.QueryRow(ctx, `
		INSERT INTO staff_capability_grant_requests
			(tenant_id, grantee_staff_id, capability, valid_from, valid_until, reason_code)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, tenant_id, grantee_staff_id, grantee_scope, capability, valid_from, valid_until,
			reason_code, requested_by, requested_by_scope, requested_by_person_id, status, created_at, expires_at
	`, tenantID, in.GranteeStaffID, string(in.Capability), in.ValidFrom, in.ValidUntil, in.ReasonCode)
	if err := scanRequest(row, &req); err != nil {
		return Request{}, err
	}
	return req, nil
}

// CancelRequest moves a pending request to 'cancelled'. Only the
// request's own tenant/platform scope may do this (RLS), and only while
// pending (the trigger refuses otherwise).
func CancelRequest(ctx context.Context, tx pgx.Tx, requestID uuid.UUID) error {
	tag, err := tx.Exec(ctx, `UPDATE staff_capability_grant_requests SET status = 'cancelled' WHERE id = $1`, requestID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// DecideAndGrant records a platform decision on requestID. For
// decision == "approve" it ALSO inserts the corresponding
// staff_capability_grants row in the same transaction, exactly as
// migration 0112's deferred constraint trigger requires. For
// decision == "reject" no grant row is inserted.
func DecideAndGrant(ctx context.Context, tx pgx.Tx, requestID uuid.UUID, decision, reasonCode string) (approvalID uuid.UUID, grant *Grant, err error) {
	if err := tx.QueryRow(ctx, `
		INSERT INTO staff_capability_grant_approvals (request_id, decision, reason_code)
		VALUES ($1, $2, $3)
		RETURNING id
	`, requestID, decision, reasonCode).Scan(&approvalID); err != nil {
		return uuid.Nil, nil, err
	}

	if decision != "approve" {
		return approvalID, nil, nil
	}

	var g Grant
	row := tx.QueryRow(ctx, `
		INSERT INTO staff_capability_grants (request_id, approval_id)
		VALUES ($1, $2)
		RETURNING id, tenant_id, grantee_staff_id, grantee_scope, capability, request_id, approval_id,
			valid_from, valid_until, granted_at, revoked_at, revoked_by, revoked_by_scope, revoke_reason_code
	`, requestID, approvalID)
	if err := scanGrant(row, &g); err != nil {
		return uuid.Nil, nil, err
	}
	return approvalID, &g, nil
}

// RevokeGrant sets revoked_at/revoked_by/revoked_by_scope/
// revoke_reason_code on an in-force grant (the emergency stop, ADR 0099
// §8.1). revoked_by/revoked_by_scope are forced by the trigger from the
// DB session, never trusted from a caller - this function does not even
// accept them as parameters.
func RevokeGrant(ctx context.Context, tx pgx.Tx, grantID uuid.UUID, reasonCode string) error {
	tag, err := tx.Exec(ctx, `
		UPDATE staff_capability_grants
		   SET revoked_at = now(), revoke_reason_code = $2
		 WHERE id = $1
	`, grantID, reasonCode)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ListRequests returns every staff_capability_grant_requests row visible
// under the caller's own RLS scope, most recent first.
func ListRequests(ctx context.Context, tx pgx.Tx) ([]Request, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, grantee_staff_id, grantee_scope, capability, valid_from, valid_until,
			reason_code, requested_by, requested_by_scope, requested_by_person_id, status, created_at, expires_at
		FROM staff_capability_grant_requests
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var req Request
		if err := scanRequestRow(rows, &req); err != nil {
			return nil, err
		}
		out = append(out, req)
	}
	return out, rows.Err()
}

// ListGrants returns every staff_capability_grants row visible under the
// caller's own RLS scope, most recently granted first.
func ListGrants(ctx context.Context, tx pgx.Tx) ([]Grant, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, tenant_id, grantee_staff_id, grantee_scope, capability, request_id, approval_id,
			valid_from, valid_until, granted_at, revoked_at, revoked_by, revoked_by_scope, revoke_reason_code
		FROM staff_capability_grants
		ORDER BY granted_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := scanGrantRow(rows, &g); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

type scannable interface {
	Scan(dest ...any) error
}

func scanRequest(row scannable, req *Request) error {
	return scanRequestRow(row, req)
}

func scanRequestRow(row scannable, req *Request) error {
	var capability, status string
	if err := row.Scan(&req.ID, &req.TenantID, &req.GranteeStaffID, &req.GranteeScope, &capability,
		&req.ValidFrom, &req.ValidUntil, &req.ReasonCode, &req.RequestedBy, &req.RequestedByScope,
		&req.RequestedByPersonID, &status, &req.CreatedAt, &req.ExpiresAt,
	); err != nil {
		return err
	}
	req.Capability = Capability(capability)
	req.Status = RequestStatus(status)
	return nil
}

func scanGrant(row scannable, g *Grant) error {
	var capability string
	if err := row.Scan(&g.ID, &g.TenantID, &g.GranteeStaffID, &g.GranteeScope, &capability, &g.RequestID,
		&g.ApprovalID, &g.ValidFrom, &g.ValidUntil, &g.GrantedAt, &g.RevokedAt, &g.RevokedBy,
		&g.RevokedByScope, &g.RevokeReasonCode,
	); err != nil {
		return err
	}
	g.Capability = Capability(capability)
	return nil
}

func scanGrantRow(row scannable, g *Grant) error {
	return scanGrant(row, g)
}
