package assetregistry

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// This file implements ADR 0037 §C.5.1 operations 1-5 (layers 1-3,
// platform-admin only) plus the two four-eyes support operations the
// dual-control requirement (§C.5.3) needs in order to be a real control
// rather than a documented intention.
//
// Every function here requires a transaction opened by
// db.Pool.WithPlatformAdmin. That is not a convention: migration 0044's
// RLS policies on `assets`/`asset_change_requests`/`asset_change_approvals`
// only accept writes when app.platform_admin_principal_id is set AND
// app.tenant_id/app.player_account_id are unset, so a tenant-scoped
// transaction fails at the database regardless of what this Go code does.
//
// Every function here writes an audit record in the SAME transaction
// (actor, tenant = NULL for these platform-scoped mutations, entity,
// before/after state, IP, reason code) per CLAUDE.md's audit rule and ADR
// 0037 §C.3's "no exception" wording.

// ActorContext is the audit-trail context every mutating operation
// carries. ActorID must be the principal resolved from the verified
// token's subject, never a client-supplied identifier.
type ActorContext struct {
	ActorID   uuid.UUID
	IPAddress string
	UserAgent string
	RequestID string
	// ReasonCode is mandatory on every mutation (CLAUDE.md: "actor,
	// tenant, entity, before/after state, IP, reason code").
	ReasonCode string
}

func (a ActorContext) validate() error {
	if a.ActorID == uuid.Nil {
		return fmt.Errorf("%w: actor id is required", ErrInvalidInput)
	}
	if strings.TrimSpace(a.ReasonCode) == "" {
		return fmt.Errorf("%w: reason_code is required on every mutating asset-registry operation", ErrInvalidInput)
	}
	return nil
}

// classifyTriggerError turns migration 0044's raised exceptions into this
// package's own sentinels, so the HTTP layer can answer 409/403 instead
// of a generic 500. The trigger remains the authoritative refusal - this
// only classifies what it said.
func classifyTriggerError(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	msg := pgErr.Message
	switch {
	case strings.Contains(msg, "four-eyes"):
		return fmt.Errorf("%w: %s", ErrDualControlRequired, msg)
	case strings.Contains(msg, "self-approval"):
		return fmt.Errorf("%w: %s", ErrSelfApproval, msg)
	case strings.Contains(msg, "narrow") || strings.Contains(msg, "not platform-authorized"):
		return fmt.Errorf("%w: %s", ErrWidensPlatformAuthorization, msg)
	case strings.Contains(msg, "immutable") ||
		strings.Contains(msg, "never be created active") ||
		strings.Contains(msg, "never be created platform-authorized") ||
		strings.Contains(msg, "does not match the approved request payload") ||
		strings.Contains(msg, "not a platform-scoped staff principal"):
		return fmt.Errorf("%w: %s", ErrInvalidInput, msg)
	}
	return err
}

// --- Four-eyes support operations (ADR 0037 §C.5.3) ---

// FileChangeRequestParams is FileChangeRequest's input. For
// ChangeCreate, AssetType/DecimalExponent/Network are the identity facts
// the approver is approving, and migration 0044 verifies the eventual
// INSERT matches them exactly - so approving "BTC, exponent 8" cannot be
// used to insert "BTC, exponent 2".
type FileChangeRequestParams struct {
	Operation       ChangeOperation
	AssetCode       string
	AssetType       string
	DecimalExponent int16
	Network         string
	Actor           ActorContext
}

// FileChangeRequest records a pending request for one of the three
// dual-controlled operations. It never performs the operation.
func FileChangeRequest(ctx context.Context, tx pgx.Tx, p FileChangeRequestParams) (ChangeRequest, error) {
	if err := p.Actor.validate(); err != nil {
		return ChangeRequest{}, err
	}
	if !validChangeOperation(p.Operation) {
		return ChangeRequest{}, fmt.Errorf("%w: operation must be one of create/activate/platform_authorize (suspend and revoke are deliberately single-actor)", ErrInvalidInput)
	}
	if strings.TrimSpace(p.AssetCode) == "" {
		return ChangeRequest{}, fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}

	payload := map[string]any{}
	if p.Operation == ChangeCreate {
		if p.AssetType != AssetTypeFiat && p.AssetType != AssetTypeCrypto {
			return ChangeRequest{}, fmt.Errorf("%w: asset_type must be 'fiat' or 'crypto'", ErrInvalidInput)
		}
		if p.DecimalExponent < 0 || p.DecimalExponent > 18 {
			return ChangeRequest{}, fmt.Errorf("%w: decimal_exponent must be between 0 and 18", ErrInvalidInput)
		}
		if p.AssetType == AssetTypeCrypto && strings.TrimSpace(p.Network) == "" {
			return ChangeRequest{}, fmt.Errorf("%w: network is required for a crypto asset", ErrInvalidInput)
		}
		payload["asset_type"] = p.AssetType
		payload["decimal_exponent"] = p.DecimalExponent
		if p.Network != "" {
			payload["network"] = p.Network
		}
	}

	req := ChangeRequest{ID: uuid.New(), Operation: p.Operation, AssetCode: p.AssetCode,
		Payload: payload, ReasonCode: p.Actor.ReasonCode, RequestedByPrincipalID: p.Actor.ActorID, State: "pending"}

	if err := tx.QueryRow(ctx,
		`INSERT INTO asset_change_requests (id, operation, asset_code, payload, reason_code, requested_by_principal_id)
		 VALUES ($1, $2, $3, $4, $5, $6) RETURNING requested_at`,
		req.ID, req.Operation, req.AssetCode, payload, req.ReasonCode, req.RequestedByPrincipalID).
		Scan(&req.RequestedAt); err != nil {
		return ChangeRequest{}, classifyTriggerError(fmt.Errorf("assetregistry: insert change request: %w", err))
	}

	if err := recordAudit(ctx, tx, p.Actor, "asset.change_requested", "asset_change_request", req.ID.String(), map[string]any{
		"operation": string(req.Operation), "asset_code": req.AssetCode, "payload": payload,
		"before": nil, "after": map[string]any{"state": "pending"},
	}); err != nil {
		return ChangeRequest{}, err
	}
	return req, nil
}

// DecideChangeRequestParams is DecideChangeRequest's input.
type DecideChangeRequestParams struct {
	RequestID uuid.UUID
	Approve   bool
	Actor     ActorContext
}

// DecideChangeRequest records one approve/reject decision. Migration
// 0044 enforces, at the database: one decision per principal per request
// (UNIQUE), the requester may not decide its own request, the approver
// must be a platform-scoped staff principal, and a decision can never be
// edited afterwards.
func DecideChangeRequest(ctx context.Context, tx pgx.Tx, p DecideChangeRequestParams) (Approval, error) {
	if err := p.Actor.validate(); err != nil {
		return Approval{}, err
	}
	if p.RequestID == uuid.Nil {
		return Approval{}, fmt.Errorf("%w: request_id is required", ErrInvalidInput)
	}

	decision := "reject"
	if p.Approve {
		decision = "approve"
	}
	ap := Approval{ID: uuid.New(), RequestID: p.RequestID, ApproverPrincipalID: p.Actor.ActorID,
		Decision: decision, ReasonCode: p.Actor.ReasonCode}

	if err := tx.QueryRow(ctx,
		`INSERT INTO asset_change_approvals (id, request_id, approver_principal_id, decision, reason_code)
		 VALUES ($1, $2, $3, $4, $5) RETURNING decided_at`,
		ap.ID, ap.RequestID, ap.ApproverPrincipalID, ap.Decision, ap.ReasonCode).Scan(&ap.DecidedAt); err != nil {
		wrapped := fmt.Errorf("assetregistry: insert approval: %w", err)
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Approval{}, fmt.Errorf("%w: this principal has already decided this request", ErrSelfApproval)
		}
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return Approval{}, ErrNotFound
		}
		return Approval{}, classifyTriggerError(wrapped)
	}

	action := "asset.change_rejected"
	if p.Approve {
		action = "asset.change_approved"
	}
	if err := recordAudit(ctx, tx, p.Actor, action, "asset_change_request", p.RequestID.String(), map[string]any{
		"decision": decision, "approval_id": ap.ID.String(),
	}); err != nil {
		return Approval{}, err
	}
	return ap, nil
}

// --- Operation 1: create asset (dual-controlled) ---

// CreateAssetParams is CreateAsset's input. There is deliberately no
// `active` or `platform_authorized` field: ADR 0037 §C.5.5 forbids
// creation from carrying any authorization, and migration 0044 refuses an
// INSERT that tries.
type CreateAssetParams struct {
	Code            string
	AssetType       string
	DecimalExponent int16
	DisplayName     string
	Network         string
	Actor           ActorContext
}

// CreateAsset inserts a layer-1 row. It requires a pending
// ChangeCreate request for this code, approved by a different platform
// principal, whose payload matches these identity facts exactly - all
// enforced by migration 0044's trigger, which also consumes the request
// so one approval authorizes one creation, once.
func CreateAsset(ctx context.Context, tx pgx.Tx, p CreateAssetParams) (Asset, error) {
	if err := p.Actor.validate(); err != nil {
		return Asset{}, err
	}
	if strings.TrimSpace(p.Code) == "" || strings.TrimSpace(p.DisplayName) == "" {
		return Asset{}, fmt.Errorf("%w: code and display_name are required", ErrInvalidInput)
	}
	if p.AssetType != AssetTypeFiat && p.AssetType != AssetTypeCrypto {
		return Asset{}, fmt.Errorf("%w: asset_type must be 'fiat' or 'crypto'", ErrInvalidInput)
	}
	if p.DecimalExponent < 0 || p.DecimalExponent > 18 {
		return Asset{}, fmt.Errorf("%w: decimal_exponent must be between 0 and 18", ErrInvalidInput)
	}

	var network *string
	if strings.TrimSpace(p.Network) != "" {
		n := p.Network
		network = &n
	}

	if _, err := tx.Exec(ctx,
		`INSERT INTO assets (code, asset_type, decimal_exponent, display_name, network, active, platform_authorized)
		 VALUES ($1, $2, $3, $4, $5, false, false)`,
		p.Code, p.AssetType, p.DecimalExponent, p.DisplayName, network); err != nil {
		return Asset{}, classifyTriggerError(fmt.Errorf("assetregistry: insert asset: %w", err))
	}

	created, err := GetAsset(ctx, tx, p.Code)
	if err != nil {
		return Asset{}, err
	}
	if err := recordAudit(ctx, tx, p.Actor, "asset.created", "asset", created.Code, map[string]any{
		"before": nil, "after": assetState(created),
	}); err != nil {
		return Asset{}, err
	}
	return created, nil
}

// --- Operation 2: update metadata (not dual-controlled) ---

// UpdateAssetMetadata changes ONLY mutable display metadata (ADR 0037
// §C.5.4). code/decimal_exponent/asset_type/network are absent from this
// function's inputs by design, and migration 0044's
// assets_immutable_identity trigger refuses them at the database even if
// some future code path tries.
func UpdateAssetMetadata(ctx context.Context, tx pgx.Tx, code, displayName string, actor ActorContext) (Asset, error) {
	if err := actor.validate(); err != nil {
		return Asset{}, err
	}
	if strings.TrimSpace(displayName) == "" {
		return Asset{}, fmt.Errorf("%w: display_name is required", ErrInvalidInput)
	}
	before, err := GetAsset(ctx, tx, code)
	if err != nil {
		return Asset{}, err
	}
	tag, err := tx.Exec(ctx, `UPDATE assets SET display_name = $2 WHERE code = $1`, code, displayName)
	if err != nil {
		return Asset{}, classifyTriggerError(fmt.Errorf("assetregistry: update asset metadata: %w", err))
	}
	if tag.RowsAffected() == 0 {
		// Zero rows under RLS means this transaction has no platform-admin
		// write visibility on `assets` at all - a silent no-op would be
		// far worse than a loud failure.
		return Asset{}, fmt.Errorf("%w: no asset row was updated (is this transaction platform-admin-scoped?)", ErrNotFound)
	}
	after, err := GetAsset(ctx, tx, code)
	if err != nil {
		return Asset{}, err
	}
	if err := recordAudit(ctx, tx, actor, "asset.metadata_updated", "asset", code, map[string]any{
		"before": assetState(before), "after": assetState(after),
	}); err != nil {
		return Asset{}, err
	}
	return after, nil
}

// --- Operations 3/4: activate (dual-controlled) and suspend (single-actor) ---

// SetActive flips layer 2. Granting (active=true) requires an approved
// ChangeActivate request by a different principal; revoking
// (active=false) deliberately does not - ADR 0037 §C.5.3's asymmetry: a
// kill-switch used mid-incident must not wait for a second approver.
func SetActive(ctx context.Context, tx pgx.Tx, code string, active bool, actor ActorContext) (Asset, error) {
	return setAssetFlag(ctx, tx, code, "active", active, actor)
}

// --- Operation 5: configure platform authorization ---

// SetPlatformAuthorized flips layer 3 (ADR 0037 §C.5.1 op 5 - the
// "configure capabilities" operation, which is about the ASSET's platform
// clearance and must never be confused with ProviderCapability, ADR
// 0022). Granting requires dual control; revoking does not.
func SetPlatformAuthorized(ctx context.Context, tx pgx.Tx, code string, authorized bool, actor ActorContext) (Asset, error) {
	return setAssetFlag(ctx, tx, code, "platform_authorized", authorized, actor)
}

func setAssetFlag(ctx context.Context, tx pgx.Tx, code, column string, value bool, actor ActorContext) (Asset, error) {
	if err := actor.validate(); err != nil {
		return Asset{}, err
	}
	// column is never caller-supplied: SetActive/SetPlatformAuthorized are
	// the only callers and each passes a literal. Guarded anyway so a
	// future caller cannot turn this into a SQL-injection surface.
	var sql string
	switch column {
	case "active":
		sql = `UPDATE assets SET active = $2 WHERE code = $1`
	case "platform_authorized":
		sql = `UPDATE assets SET platform_authorized = $2 WHERE code = $1`
	default:
		return Asset{}, fmt.Errorf("%w: unsupported asset flag %q", ErrInvalidInput, column)
	}

	before, err := GetAsset(ctx, tx, code)
	if err != nil {
		return Asset{}, err
	}
	tag, err := tx.Exec(ctx, sql, code, value)
	if err != nil {
		return Asset{}, classifyTriggerError(fmt.Errorf("assetregistry: set %s: %w", column, err))
	}
	if tag.RowsAffected() == 0 {
		return Asset{}, fmt.Errorf("%w: no asset row was updated (is this transaction platform-admin-scoped?)", ErrNotFound)
	}
	after, err := GetAsset(ctx, tx, code)
	if err != nil {
		return Asset{}, err
	}

	action := "asset." + column + "_revoked"
	if value {
		action = "asset." + column + "_granted"
	}
	if err := recordAudit(ctx, tx, actor, action, "asset", code, map[string]any{
		"before": assetState(before), "after": assetState(after),
	}); err != nil {
		return Asset{}, err
	}
	return after, nil
}

func assetState(a Asset) map[string]any {
	return map[string]any{
		"code": a.Code, "asset_type": a.AssetType, "decimal_exponent": a.DecimalExponent,
		"display_name": a.DisplayName, "network": a.Network,
		"active": a.Active, "platform_authorized": a.PlatformAuthorized,
	}
}

// recordAudit writes the mandatory audit record for a PLATFORM-scoped
// mutation (tenant_id NULL - audit_log's dual-scope RLS, ADR 0013), in
// the same transaction as the mutation itself so the two commit or roll
// back together.
func recordAudit(ctx context.Context, tx pgx.Tx, actor ActorContext, action, targetType, targetID string, metadata map[string]any) error {
	if metadata == nil {
		metadata = map[string]any{}
	}
	metadata["reason_code"] = actor.ReasonCode
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: uuid.Nil, ActorType: audit.ActorStaff, ActorID: actor.ActorID,
		Action: action, TargetType: targetType, TargetID: targetID,
		Outcome: audit.OutcomeSuccess, IPAddress: actor.IPAddress, UserAgent: actor.UserAgent,
		RequestID: actor.RequestID, Metadata: metadata,
	}); err != nil {
		return fmt.Errorf("assetregistry: audit %s: %w", action, err)
	}
	return nil
}
