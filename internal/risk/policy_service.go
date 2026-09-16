package risk

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
)

// ErrNotFound covers "no such rule" - mirrors kyc.ErrNotFound/
// rg.ErrInvalidInput's own package-local sentinel convention.
var ErrNotFound = errors.New("risk: rule not found")

const ruleColumns = `id, tenant_id, brand_id, jurisdiction_code, licensing_mode, player_account_id, product, operation,
	provider_id, game_id, asset_code, payment_method, limit_kind, time_window, threshold,
	rule_kind, action, status, effective_from, effective_until, description,
	created_by_actor_type, created_by_actor_id, created_at, updated_at`

func scanRule(row pgx.Row) (Rule, error) {
	var r Rule
	var jurisdictionCode, licensingMode, product, providerID, assetCode, paymentMethod, description *string
	var threshold int64
	err := row.Scan(
		&r.ID, &r.TenantID, &r.BrandID, &jurisdictionCode, &licensingMode, &r.PlayerAccountID, &product, &r.Operation,
		&providerID, &r.GameID, &assetCode, &paymentMethod, &r.LimitKind, &r.TimeWindow, &threshold,
		&r.RuleKind, &r.Action, &r.Status, &r.EffectiveFrom, &r.EffectiveUntil, &description,
		&r.CreatedByActorType, &r.CreatedByActorID, &r.CreatedAt, &r.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Rule{}, ErrNotFound
	}
	if err != nil {
		return Rule{}, fmt.Errorf("risk: scan rule: %w", err)
	}
	r.Threshold = threshold
	if jurisdictionCode != nil {
		r.JurisdictionCode = *jurisdictionCode
	}
	if licensingMode != nil {
		r.LicensingMode = *licensingMode
	}
	if product != nil {
		r.Product = *product
	}
	if providerID != nil {
		r.ProviderID = *providerID
	}
	if assetCode != nil {
		r.AssetCode = *assetCode
	}
	if paymentMethod != nil {
		r.PaymentMethod = *paymentMethod
	}
	if description != nil {
		r.Description = *description
	}
	return r, nil
}

func scanRules(rows pgx.Rows) ([]Rule, error) {
	defer rows.Close()
	var out []Rule
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// listEffectiveRules returns every rule (any status/effective window -
// Evaluate itself filters by isEffective) scoped to operation, visible
// under the current RLS scope (the caller's own tenant plus every
// platform-wide rule - migration 0041's tenant_and_platform_read policy).
func listEffectiveRules(ctx context.Context, tx pgx.Tx, operation Operation) ([]Rule, error) {
	// ORDER BY id: defense in depth for deterministic iteration - Evaluate's
	// own aggregation logic no longer depends on rule order for
	// correctness (specialist review fix), but a stable query order
	// still makes MatchedRule reporting reproducible across identical runs.
	rows, err := tx.Query(ctx, `SELECT `+ruleColumns+` FROM risk_rules WHERE operation = $1 ORDER BY id`, operation)
	if err != nil {
		return nil, fmt.Errorf("risk: list rules: %w", err)
	}
	return scanRules(rows)
}

// CreateRuleParams is CreateRule's input. TenantID nil means a
// platform-wide rule - tx MUST already be scoped consistently (db.
// WithoutTenant for a nil TenantID, db.WithTenant(*TenantID, ...)
// otherwise), since migration 0041's tenant_and_platform_write RLS
// policy requires exactly that to accept the INSERT; CreateRule does not
// itself choose or override the transaction's scope.
type CreateRuleParams struct {
	TenantID           *uuid.UUID
	BrandID            *uuid.UUID
	JurisdictionCode   string
	LicensingMode      string
	PlayerAccountID    *uuid.UUID
	Product            string
	Operation          Operation
	ProviderID         string
	GameID             *uuid.UUID
	AssetCode          string
	PaymentMethod      string
	LimitKind          LimitKind
	TimeWindow         TimeWindow
	Threshold          int64
	RuleKind           RuleKind
	Action             RuleAction
	Description        string
	CreatedByActorType string
	CreatedByActorID   uuid.UUID
	// IPAddress/UserAgent/RequestID are audit-trail context only, never
	// persisted on the risk_rules row itself - CLAUDE.md's "every
	// mutating administrative/financial action writes an audit record
	// (actor, tenant, entity, before/after state, IP, reason code)".
	IPAddress string
	UserAgent string
	RequestID string
}

// CreateRule inserts a new risk_rules row and audits it
// (risk.rule_created) - directive §23's "audit: rule created" minimum.
func CreateRule(ctx context.Context, tx pgx.Tx, params CreateRuleParams) (Rule, error) {
	if params.Operation == "" || params.LimitKind == "" || params.TimeWindow == "" {
		return Rule{}, fmt.Errorf("%w: operation, limit_kind, and time_window are required", ErrInvalidInput)
	}
	if params.Threshold < 0 {
		return Rule{}, fmt.Errorf("%w: threshold must be non-negative", ErrInvalidInput)
	}
	if params.CreatedByActorID == uuid.Nil {
		return Rule{}, fmt.Errorf("%w: created_by_actor_id is required", ErrInvalidInput)
	}
	action := params.Action
	if action == "" {
		action = ActionDeny
	}
	ruleKind := params.RuleKind
	if ruleKind == "" {
		ruleKind = RuleConfigurableLimit
	}
	if ruleKind == RuleRiskSignal {
		action = ActionReview
	}

	r := Rule{
		ID: uuid.New(), TenantID: params.TenantID, BrandID: params.BrandID, JurisdictionCode: params.JurisdictionCode,
		LicensingMode:   params.LicensingMode,
		PlayerAccountID: params.PlayerAccountID, Product: params.Product, Operation: params.Operation,
		ProviderID: params.ProviderID, GameID: params.GameID, AssetCode: params.AssetCode, PaymentMethod: params.PaymentMethod,
		LimitKind: params.LimitKind, TimeWindow: params.TimeWindow, Threshold: params.Threshold,
		RuleKind: ruleKind, Action: action, Status: RuleActive, EffectiveFrom: time.Now().UTC(),
		Description: params.Description, CreatedByActorType: params.CreatedByActorType, CreatedByActorID: params.CreatedByActorID,
	}

	_, err := tx.Exec(ctx,
		`INSERT INTO risk_rules (id, tenant_id, brand_id, jurisdiction_code, licensing_mode, player_account_id, product, operation,
			provider_id, game_id, asset_code, payment_method, limit_kind, time_window, threshold,
			rule_kind, action, status, effective_from, description, created_by_actor_type, created_by_actor_id)
		 VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, NULLIF($7, ''), $8, NULLIF($9, ''), $10, NULLIF($11, ''), NULLIF($12, ''),
			$13, $14, $15, $16, $17, $18, $19, NULLIF($20, ''), $21, $22)`,
		r.ID, r.TenantID, r.BrandID, r.JurisdictionCode, r.LicensingMode, r.PlayerAccountID, r.Product, r.Operation,
		r.ProviderID, r.GameID, r.AssetCode, r.PaymentMethod, r.LimitKind, r.TimeWindow, r.Threshold,
		r.RuleKind, r.Action, r.Status, r.EffectiveFrom, r.Description, r.CreatedByActorType, r.CreatedByActorID,
	)
	if err != nil {
		return Rule{}, fmt.Errorf("risk: insert rule: %w", err)
	}

	auditTenantID := uuid.Nil
	if r.TenantID != nil {
		auditTenantID = *r.TenantID
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: auditTenantID, ActorType: audit.ActorType(params.CreatedByActorType), ActorID: params.CreatedByActorID,
		Action: "risk.rule_created", TargetType: "risk_rule", TargetID: r.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: map[string]any{
			"operation": string(r.Operation), "limit_kind": string(r.LimitKind), "time_window": string(r.TimeWindow),
			"rule_kind": string(r.RuleKind), "action": string(r.Action), "platform_wide": r.TenantID == nil,
			"jurisdiction_code": r.JurisdictionCode, "licensing_mode": r.LicensingMode,
		},
	}); err != nil {
		return Rule{}, fmt.Errorf("risk: audit rule creation: %w", err)
	}
	return r, nil
}

// ListRulesForOperation is the admin read path - visible under RLS
// exactly like listEffectiveRules (this package's own internal use),
// exported for the HTTP handler.
func ListRulesForOperation(ctx context.Context, tx pgx.Tx, operation Operation) ([]Rule, error) {
	return listEffectiveRules(ctx, tx, operation)
}

// DisableRuleParams is DisableRule's input.
type DisableRuleParams struct {
	RuleID    uuid.UUID
	ActorType string
	ActorID   uuid.UUID
	IPAddress string
	UserAgent string
	RequestID string
}

// DisableRule flips a rule's status to 'disabled' - the ONLY mutation
// migration 0041's immutability trigger permits besides description/
// effective_until, and audits it (risk.rule_disabled). A rule is never
// deleted (directive §17/§23 "audit: rule disabled... deleted where
// permitted" - this stage permits disable only, never delete, since a
// past RiskDecision must remain explainable against the exact rule that
// produced it).
func DisableRule(ctx context.Context, tx pgx.Tx, params DisableRuleParams) (Rule, error) {
	if params.RuleID == uuid.Nil || params.ActorID == uuid.Nil {
		return Rule{}, fmt.Errorf("%w: rule_id and actor_id are required", ErrInvalidInput)
	}
	tag, err := tx.Exec(ctx, `UPDATE risk_rules SET status = 'disabled' WHERE id = $1 AND status = 'active'`, params.RuleID)
	if err != nil {
		return Rule{}, fmt.Errorf("risk: disable rule: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return Rule{}, ErrNotFound
	}
	r, err := scanRule(tx.QueryRow(ctx, `SELECT `+ruleColumns+` FROM risk_rules WHERE id = $1`, params.RuleID))
	if err != nil {
		return Rule{}, err
	}
	auditTenantID := uuid.Nil
	if r.TenantID != nil {
		auditTenantID = *r.TenantID
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: auditTenantID, ActorType: audit.ActorType(params.ActorType), ActorID: params.ActorID,
		Action: "risk.rule_disabled", TargetType: "risk_rule", TargetID: r.ID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: params.IPAddress, UserAgent: params.UserAgent, RequestID: params.RequestID,
		Metadata: map[string]any{"operation": string(r.Operation)},
	}); err != nil {
		return Rule{}, fmt.Errorf("risk: audit rule disable: %w", err)
	}
	return r, nil
}
