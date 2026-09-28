package adjustment

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Policy levels (ADR 0100 §3.1).
const (
	LevelPlatform     = "platform"
	LevelJurisdiction = "jurisdiction"
	LevelProfile      = "profile"
	LevelTenant       = "tenant"
	LevelBrand        = "brand"
)

// Change kinds.
const (
	ChangeKindPolicy            = "policy"
	ChangeKindProfileAssignment = "profile_assignment"
)

// PolicyChangeInput is a proposed policy row (or tenant -> profile
// assignment). No threshold value is ever supplied by the platform itself
// (HD-PRH2-3): every value here comes from an authorised human author and
// becomes effective only after an independent approval.
type PolicyChangeInput struct {
	ChangeKind                      string
	OperationKind                   string
	Level                           string
	TenantID                        *uuid.UUID
	BrandID                         *uuid.UUID
	JurisdictionID                  *uuid.UUID
	ProfileCode                     *string
	AssetCode                       *string
	BaseRequiredApprovals           *int
	ThresholdMinorUnits             *string // decimal integer string (NUMERIC(38,0)); never a float
	RequiredApprovalsAboveThreshold *int
	EffectiveFrom                   *time.Time
	LegalReviewReference            *string
}

// PolicyChange is one financial_approval_policy_changes row.
type PolicyChange struct {
	ID             uuid.UUID
	ChangeKind     string
	OperationKind  *string
	Level          *string
	TenantID       *uuid.UUID
	ContentHash    string
	RequestedBy    uuid.UUID
	RequestedScope string
	Status         string
	ExpiresAt      time.Time
}

// PolicyCall is the actor of a policy-change call. Policy changes run in
// a TENANT or PLATFORM session (never acting; ADR 0100 §10.4 "No A").
type PolicyCall struct {
	ActorID  uuid.UUID
	TenantID uuid.UUID // uuid.Nil for a platform caller
	Meta     Meta
}

func (c PolicyCall) scope() string {
	if c.TenantID == uuid.Nil {
		return "platform"
	}
	return "tenant"
}

func validThreshold(s *string) error {
	if s == nil {
		return nil
	}
	n, ok := new(big.Int).SetString(*s, 10)
	if !ok || n.Sign() < 0 || len(*s) > 38 {
		return fmt.Errorf("%w: threshold_minor_units must be a non-negative integer (minor units)", ErrInvalidInput)
	}
	return nil
}

// ProposePolicyChangeInTx inserts a change request (actor forced by the
// trigger; content_hash computed by the DB).
func ProposePolicyChangeInTx(ctx context.Context, tx pgx.Tx, call PolicyCall, in PolicyChangeInput) (PolicyChange, error) {
	if in.ChangeKind != ChangeKindPolicy && in.ChangeKind != ChangeKindProfileAssignment {
		return PolicyChange{}, fmt.Errorf("%w: change_kind must be policy or profile_assignment", ErrInvalidInput)
	}
	if err := validThreshold(in.ThresholdMinorUnits); err != nil {
		return PolicyChange{}, err
	}
	// A tenant caller's tenant is always its own session tenant, never a
	// supplied value (the trigger re-checks it too).
	if call.TenantID != uuid.Nil {
		tid := call.TenantID
		in.TenantID = &tid
	}
	var opKind, level any
	if in.OperationKind != "" {
		opKind = in.OperationKind
	}
	if in.Level != "" {
		level = in.Level
	}
	var c PolicyChange
	err := tx.QueryRow(ctx, `
		INSERT INTO financial_approval_policy_changes
			(change_kind, operation_kind, level, tenant_id, brand_id, jurisdiction_id, profile_code, asset_code,
			 base_required_approvals, threshold_minor_units, required_approvals_above_threshold, effective_from,
			 legal_review_reference, content_hash, requested_by, requested_by_scope, requested_by_person_id, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10::numeric, $11, $12, $13, '-', $14, 'platform', $14, now())
		RETURNING id, change_kind, operation_kind, level, tenant_id, content_hash, requested_by, requested_by_scope, status, expires_at`,
		in.ChangeKind, opKind, level, in.TenantID, in.BrandID, in.JurisdictionID, in.ProfileCode, in.AssetCode,
		in.BaseRequiredApprovals, in.ThresholdMinorUnits, in.RequiredApprovalsAboveThreshold, in.EffectiveFrom,
		in.LegalReviewReference, uuid.Nil,
	).Scan(&c.ID, &c.ChangeKind, &c.OperationKind, &c.Level, &c.TenantID, &c.ContentHash, &c.RequestedBy, &c.RequestedScope, &c.Status, &c.ExpiresAt)
	if err != nil {
		return PolicyChange{}, err
	}
	return c, recordPolicyAudit(ctx, tx, call, "financial_policy.change_proposed", c, map[string]any{
		"base_required_approvals":            in.BaseRequiredApprovals,
		"threshold_minor_units":              in.ThresholdMinorUnits,
		"required_approvals_above_threshold": in.RequiredApprovalsAboveThreshold,
		"asset_code":                         in.AssetCode,
		"legal_review_reference":             in.LegalReviewReference,
	})
}

// DecidePolicyChangeInTx approves (inserting the policy/profile row in the
// same transaction, by trigger) or rejects a change. contentHash is the
// hash the approver reviewed; a mismatch is refused (MA012).
func DecidePolicyChangeInTx(ctx context.Context, tx pgx.Tx, call PolicyCall, changeID uuid.UUID, decision Decision, contentHash, reasonCode string) (PolicyChange, error) {
	if decision != DecisionApprove && decision != DecisionReject {
		return PolicyChange{}, fmt.Errorf("%w: decision must be approve or reject", ErrInvalidInput)
	}
	if len(reasonCode) < 1 || len(reasonCode) > 64 {
		return PolicyChange{}, fmt.Errorf("%w: reason_code must be 1-64 bytes", ErrInvalidInput)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO financial_approval_policy_change_approvals
			(change_id, decision, content_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
		VALUES ($1, $2, $3, $4, 'platform', $4, 0, $5)`,
		changeID, string(decision), contentHash, uuid.Nil, reasonCode); err != nil {
		return PolicyChange{}, err
	}
	c, err := getPolicyChange(ctx, tx, changeID)
	if err != nil {
		return PolicyChange{}, err
	}
	action := "financial_policy.change_approved"
	if decision == DecisionReject {
		action = "financial_policy.change_rejected"
	}
	extra := map[string]any{"decision_reason_code": reasonCode}
	if decision == DecisionApprove {
		var policyID *uuid.UUID
		_ = tx.QueryRow(ctx, `SELECT id FROM financial_approval_policies WHERE change_id = $1`, changeID).Scan(&policyID)
		if policyID != nil {
			extra["policy_id"] = policyID.String()
		}
	}
	return c, recordPolicyAudit(ctx, tx, call, action, c, extra)
}

// CancelPolicyChangeInTx cancels a pending change (requester only).
func CancelPolicyChangeInTx(ctx context.Context, tx pgx.Tx, call PolicyCall, changeID uuid.UUID) (PolicyChange, error) {
	tag, err := tx.Exec(ctx, `UPDATE financial_approval_policy_changes SET status = 'cancelled' WHERE id = $1`, changeID)
	if err != nil {
		return PolicyChange{}, err
	}
	if tag.RowsAffected() != 1 {
		return PolicyChange{}, ErrNotFound
	}
	c, err := getPolicyChange(ctx, tx, changeID)
	if err != nil {
		return PolicyChange{}, err
	}
	return c, recordPolicyAudit(ctx, tx, call, "financial_policy.change_cancelled", c, nil)
}

func getPolicyChange(ctx context.Context, tx pgx.Tx, changeID uuid.UUID) (PolicyChange, error) {
	var c PolicyChange
	err := tx.QueryRow(ctx, `SELECT id, change_kind, operation_kind, level, tenant_id, content_hash, requested_by, requested_by_scope, status, expires_at
		FROM financial_approval_policy_changes WHERE id = $1`, changeID).
		Scan(&c.ID, &c.ChangeKind, &c.OperationKind, &c.Level, &c.TenantID, &c.ContentHash, &c.RequestedBy, &c.RequestedScope, &c.Status, &c.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyChange{}, ErrNotFound
	}
	return c, err
}

func recordPolicyAudit(ctx context.Context, tx pgx.Tx, call PolicyCall, action string, c PolicyChange, extra map[string]any) error {
	md := map[string]any{
		"actor_scope":  call.scope(),
		"change_id":    c.ID.String(),
		"change_kind":  c.ChangeKind,
		"content_hash": c.ContentHash,
		"after_state":  c.Status,
	}
	if c.OperationKind != nil {
		md["operation_kind"] = *c.OperationKind
	}
	if c.Level != nil {
		md["level"] = *c.Level
	}
	for k, v := range extra {
		md[k] = v
	}
	entry := audit.Entry{
		ActorType:  audit.ActorStaff,
		ActorID:    call.ActorID,
		Action:     action,
		TargetType: "financial_approval_policy_change",
		TargetID:   c.ID.String(),
		Outcome:    audit.OutcomeSuccess,
		IPAddress:  call.Meta.IPAddress,
		UserAgent:  call.Meta.UserAgent,
		RequestID:  call.Meta.RequestID,
		Metadata:   md,
	}
	if call.TenantID != uuid.Nil {
		entry.TenantID = call.TenantID
	} else if c.TenantID != nil {
		// Platform action on tenant X's rows: tenant-visible via
		// subject_tenant_id (ADR 0104 / migration 0109).
		entry.SubjectTenantID = *c.TenantID
	}
	return audit.Record(ctx, tx, entry)
}

// Policy is one financial_approval_policies row (read model).
type Policy struct {
	ID                              uuid.UUID
	OperationKind                   string
	Level                           string
	TenantID                        *uuid.UUID
	BrandID                         *uuid.UUID
	JurisdictionID                  *uuid.UUID
	ProfileCode                     *string
	AssetCode                       *string
	BaseRequiredApprovals           int
	ThresholdMinorUnits             *string
	RequiredApprovalsAboveThreshold *int
	EffectiveFrom                   time.Time
	SupersedesID                    *uuid.UUID
	ChangeID                        uuid.UUID
}

// ListPoliciesInTx lists the policy rows visible to the session.
func ListPoliciesInTx(ctx context.Context, tx pgx.Tx, operationKind string) ([]Policy, error) {
	rows, err := tx.Query(ctx, `SELECT id, operation_kind, level, tenant_id, brand_id, jurisdiction_id, profile_code, asset_code,
		base_required_approvals, threshold_minor_units::text, required_approvals_above_threshold, effective_from, supersedes_id, change_id
		FROM financial_approval_policies WHERE operation_kind = $1 ORDER BY effective_from DESC, id LIMIT 500`, operationKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Policy
	for rows.Next() {
		var p Policy
		if err := rows.Scan(&p.ID, &p.OperationKind, &p.Level, &p.TenantID, &p.BrandID, &p.JurisdictionID, &p.ProfileCode, &p.AssetCode,
			&p.BaseRequiredApprovals, &p.ThresholdMinorUnits, &p.RequiredApprovalsAboveThreshold, &p.EffectiveFrom, &p.SupersedesID, &p.ChangeID); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Evaluation is financial_policy_required_approvals()'s result.
type Evaluation struct {
	Enabled               bool
	Required              int
	ContributingPolicyIDs []uuid.UUID
	TenantStatus          *string
}

// EvaluateInTx calls the ONE policy evaluator (no Go re-implementation).
func EvaluateInTx(ctx context.Context, tx pgx.Tx, operationKind string, tenantID uuid.UUID, brandID *uuid.UUID, assetCode string, amount *int64) (Evaluation, error) {
	var ev Evaluation
	var amt any
	if amount != nil {
		amt = fmt.Sprint(*amount)
	}
	err := tx.QueryRow(ctx, `SELECT enabled, required, contributing_policy_ids, tenant_status
		FROM financial_policy_required_approvals($1, $2, $3, $4, $5::numeric, now())`,
		operationKind, tenantID, brandID, assetCode, amt).Scan(&ev.Enabled, &ev.Required, &ev.ContributingPolicyIDs, &ev.TenantStatus)
	return ev, err
}

// runPolicySession opens a tenant (WithPrincipalScope) or platform
// (WithPlatformAdmin) session - never an acting session.
func (s *Service) runPolicySession(ctx context.Context, fn func(ctx context.Context, tx pgx.Tx, call PolicyCall) error, meta Meta) error {
	tc, err := tenant.FromContext(ctx)
	if err != nil {
		return ErrNoAuthContext
	}
	subject, err := uuid.Parse(tc.Subject)
	if err != nil || subject == uuid.Nil {
		return ErrNoAuthContext
	}
	call := PolicyCall{ActorID: subject, TenantID: tc.TenantID, Meta: meta}
	if tc.TenantID == uuid.Nil {
		return s.pool.WithPlatformAdmin(ctx, subject, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx, call) })
	}
	return s.pool.WithPrincipalScope(ctx, tc.TenantID, subject, func(ctx context.Context, tx pgx.Tx) error { return fn(ctx, tx, call) })
}

// ProposePolicyChange proposes a change. A tenant caller's tenant id is
// always its own (a body value is overwritten here and re-checked by the
// trigger).
func (s *Service) ProposePolicyChange(ctx context.Context, in PolicyChangeInput, meta Meta) (PolicyChange, error) {
	var out PolicyChange
	err := s.runPolicySession(ctx, func(ctx context.Context, tx pgx.Tx, call PolicyCall) error {
		var err error
		out, err = ProposePolicyChangeInTx(ctx, tx, call, in)
		return err
	}, meta)
	return out, err
}

// DecidePolicyChange decides a change.
func (s *Service) DecidePolicyChange(ctx context.Context, changeID uuid.UUID, decision Decision, contentHash, reasonCode string, meta Meta) (PolicyChange, error) {
	var out PolicyChange
	err := s.runPolicySession(ctx, func(ctx context.Context, tx pgx.Tx, call PolicyCall) error {
		var err error
		out, err = DecidePolicyChangeInTx(ctx, tx, call, changeID, decision, contentHash, reasonCode)
		return err
	}, meta)
	return out, err
}

// CancelPolicyChange cancels a change.
func (s *Service) CancelPolicyChange(ctx context.Context, changeID uuid.UUID, meta Meta) (PolicyChange, error) {
	var out PolicyChange
	err := s.runPolicySession(ctx, func(ctx context.Context, tx pgx.Tx, call PolicyCall) error {
		var err error
		out, err = CancelPolicyChangeInTx(ctx, tx, call, changeID)
		return err
	}, meta)
	return out, err
}

// ListPolicies lists visible policy rows for operationKind.
func (s *Service) ListPolicies(ctx context.Context, operationKind string, meta Meta) ([]Policy, error) {
	var out []Policy
	err := s.runPolicySession(ctx, func(ctx context.Context, tx pgx.Tx, call PolicyCall) error {
		var err error
		out, err = ListPoliciesInTx(ctx, tx, operationKind)
		return err
	}, meta)
	return out, err
}
