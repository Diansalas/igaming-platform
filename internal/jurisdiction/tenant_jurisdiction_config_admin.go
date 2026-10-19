package jurisdiction

// Migration 0177 (audit gap G-9, triage L-24): the first application writer of tenant_jurisdiction_configs, the per tenant
// and jurisdiction configuration that launch hard precondition H-3 and the R-JUR-CONFIG / R-RULESETS / R-CURRENCIES /
// R-PAY-METHODS readiness checks read. Before 0177 the table had no writer and a FOR ALL tenant RLS policy, so any tenant
// session could rewrite it.
//
// Every change is a NEW version: the open version of (tenant, jurisdiction) is closed (effective_to = now(), forced by the
// append-only trigger) and the successor is inserted, in ONE platform-scoped transaction, with a platform-scope audit row
// (before / after) projected to the subject tenant (ADR 0104 subject_tenant_id). The database admits these writes only from
// the executor of an executed licensing change request in the same transaction (LC040): callers are internal/licensingchange
// (the governed path) and the table owner's fixtures. No ruleset id, currency or payment method is invented here: the values
// are the operator's, validated for shape only (a currency must be an ACTIVE asset; a geo block entry an ISO-3166-1 alpha-2
// code). Whether a ruleset id names a real ruleset is not checked (HDR-J content; R-RULESETS only proves non-empty).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// TenantJurisdictionConfig mirrors a tenant_jurisdiction_configs row (migration 0002).
type TenantJurisdictionConfig struct {
	ID                    uuid.UUID
	TenantID              uuid.UUID
	JurisdictionID        uuid.UUID
	KYCRulesetID          *string
	AMLRulesetID          *string
	RGRulesetID           *string
	ReportingRulesetID    *string
	AllowedCurrencies     []string
	AllowedPaymentMethods []string
	GeoBlockList          []string
	EffectiveFrom         time.Time
	EffectiveTo           *time.Time
}

// TenantJurisdictionConfigContent is the caller-supplied content of a version.
type TenantJurisdictionConfigContent struct {
	KYCRulesetID          *string
	AMLRulesetID          *string
	RGRulesetID           *string
	ReportingRulesetID    *string
	AllowedCurrencies     []string
	AllowedPaymentMethods []string
	GeoBlockList          []string
}

var (
	rulesetIDRE     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	paymentMethodRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)
	currencyCodeRE  = regexp.MustCompile(`^[A-Z0-9]{2,12}$`)
)

// Normalize validates the content's shape and returns it with nil lists replaced by empty ones. It is pure (no database);
// ValidateTenantJurisdictionConfigContent adds the asset-registry check.
func (c TenantJurisdictionConfigContent) Normalize() (TenantJurisdictionConfigContent, error) {
	for name, v := range map[string]*string{"kyc_ruleset_id": c.KYCRulesetID, "aml_ruleset_id": c.AMLRulesetID,
		"rg_ruleset_id": c.RGRulesetID, "reporting_ruleset_id": c.ReportingRulesetID} {
		if v != nil && !rulesetIDRE.MatchString(*v) {
			return c, fmt.Errorf("%w: %s must be 1..128 characters of [A-Za-z0-9_.:-]", ErrInvalidInput, name)
		}
	}
	check := func(name string, list []string, max int, ok func(string) bool) ([]string, error) {
		if list == nil {
			return []string{}, nil
		}
		if len(list) > max {
			return nil, fmt.Errorf("%w: %s has more than %d entries", ErrInvalidInput, name, max)
		}
		seen := map[string]bool{}
		for _, v := range list {
			if !ok(v) {
				return nil, fmt.Errorf("%w: %s entry %q is not valid", ErrInvalidInput, name, v)
			}
			if seen[v] {
				return nil, fmt.Errorf("%w: %s entry %q is duplicated", ErrInvalidInput, name, v)
			}
			seen[v] = true
		}
		return list, nil
	}
	var err error
	if c.AllowedCurrencies, err = check("allowed_currencies", c.AllowedCurrencies, 64, currencyCodeRE.MatchString); err != nil {
		return c, err
	}
	if c.AllowedPaymentMethods, err = check("allowed_payment_methods", c.AllowedPaymentMethods, 64, paymentMethodRE.MatchString); err != nil {
		return c, err
	}
	if c.GeoBlockList, err = check("geo_block_list", c.GeoBlockList, 300, validation.IsISO3166Alpha2); err != nil {
		return c, err
	}
	return c, nil
}

// ValidateTenantJurisdictionConfigContent normalizes c and checks every allowed currency is an ACTIVE asset (the platform
// asset registry is readable from any session).
func ValidateTenantJurisdictionConfigContent(ctx context.Context, q pgx.Tx, c TenantJurisdictionConfigContent) (TenantJurisdictionConfigContent, error) {
	c, err := c.Normalize()
	if err != nil {
		return c, err
	}
	if len(c.AllowedCurrencies) == 0 {
		return c, nil
	}
	var active int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM assets WHERE code = ANY($1) AND active`, c.AllowedCurrencies).Scan(&active); err != nil {
		return c, fmt.Errorf("jurisdiction: read assets: %w", err)
	}
	if active != len(c.AllowedCurrencies) {
		return c, fmt.Errorf("%w: every allowed currency must be an active asset", ErrInvalidInput)
	}
	return c, nil
}

// SetTenantJurisdictionConfigVersionParams is SetTenantJurisdictionConfigVersion's input.
type SetTenantJurisdictionConfigVersionParams struct {
	TenantID       uuid.UUID
	JurisdictionID uuid.UUID
	Content        TenantJurisdictionConfigContent
	Actor          ActorContext
	// ChangeRequestID is recorded in the audit metadata (the governed path always sets it).
	ChangeRequestID uuid.UUID
}

const tjcColumns = `id, tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id, reporting_ruleset_id,
	allowed_currencies, allowed_payment_methods, geo_block_list, effective_from, effective_to`

func scanTJC(row pgx.Row) (TenantJurisdictionConfig, error) {
	var c TenantJurisdictionConfig
	var cur, pm, geo []byte
	if err := row.Scan(&c.ID, &c.TenantID, &c.JurisdictionID, &c.KYCRulesetID, &c.AMLRulesetID, &c.RGRulesetID, &c.ReportingRulesetID,
		&cur, &pm, &geo, &c.EffectiveFrom, &c.EffectiveTo); err != nil {
		return c, err
	}
	for _, x := range []struct {
		raw []byte
		dst *[]string
	}{{cur, &c.AllowedCurrencies}, {pm, &c.AllowedPaymentMethods}, {geo, &c.GeoBlockList}} {
		*x.dst = []string{}
		if len(x.raw) > 0 {
			if err := json.Unmarshal(x.raw, x.dst); err != nil {
				return c, fmt.Errorf("jurisdiction: stored configuration list is not a string array: %w", err)
			}
		}
	}
	return c, nil
}

// lockOpenTJC locks and returns the open version of (tenant, jurisdiction), or nil.
func lockOpenTJC(ctx context.Context, tx pgx.Tx, tenantID, jurisdictionID uuid.UUID) (*TenantJurisdictionConfig, error) {
	c, err := scanTJC(tx.QueryRow(ctx, `SELECT `+tjcColumns+` FROM tenant_jurisdiction_configs
		WHERE tenant_id = $1 AND jurisdiction_id = $2 AND effective_to IS NULL
		ORDER BY effective_from DESC LIMIT 1 FOR UPDATE`, tenantID, jurisdictionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: lock open configuration: %w", err)
	}
	return &c, nil
}

func closeTJC(ctx context.Context, tx pgx.Tx, id uuid.UUID) (time.Time, error) {
	var closed time.Time
	err := tx.QueryRow(ctx, `UPDATE tenant_jurisdiction_configs SET effective_to = now() WHERE id = $1 AND effective_to IS NULL RETURNING effective_to`, id).Scan(&closed)
	if errors.Is(err, pgx.ErrNoRows) {
		return closed, fmt.Errorf("%w: the open configuration changed concurrently", ErrInvalidInput)
	}
	return closed, err
}

// SetTenantJurisdictionConfigVersion closes the open version of (tenant, jurisdiction), if any, and appends the new one, with a
// platform-scope audit row (subject tenant) in the same transaction. tx MUST be platform-scoped (asserted).
func SetTenantJurisdictionConfigVersion(ctx context.Context, tx pgx.Tx, p SetTenantJurisdictionConfigVersionParams) (TenantJurisdictionConfig, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return TenantJurisdictionConfig{}, err
	}
	if err := p.Actor.validate(); err != nil {
		return TenantJurisdictionConfig{}, err
	}
	if p.TenantID == uuid.Nil || p.JurisdictionID == uuid.Nil {
		return TenantJurisdictionConfig{}, fmt.Errorf("%w: tenant_id and jurisdiction_id are required", ErrInvalidInput)
	}
	content, err := ValidateTenantJurisdictionConfigContent(ctx, tx, p.Content)
	if err != nil {
		return TenantJurisdictionConfig{}, err
	}
	before, err := lockOpenTJC(ctx, tx, p.TenantID, p.JurisdictionID)
	if err != nil {
		return TenantJurisdictionConfig{}, err
	}
	if before != nil {
		if _, err := closeTJC(ctx, tx, before.ID); err != nil {
			return TenantJurisdictionConfig{}, err
		}
	}
	cur, _ := json.Marshal(content.AllowedCurrencies)
	pm, _ := json.Marshal(content.AllowedPaymentMethods)
	geo, _ := json.Marshal(content.GeoBlockList)
	after, err := scanTJC(tx.QueryRow(ctx, `
		INSERT INTO tenant_jurisdiction_configs (tenant_id, jurisdiction_id, kyc_ruleset_id, aml_ruleset_id, rg_ruleset_id,
			reporting_ruleset_id, allowed_currencies, allowed_payment_methods, geo_block_list)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+tjcColumns,
		p.TenantID, p.JurisdictionID, content.KYCRulesetID, content.AMLRulesetID, content.RGRulesetID, content.ReportingRulesetID,
		cur, pm, geo))
	if err != nil {
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23503" {
			return TenantJurisdictionConfig{}, fmt.Errorf("%w: unknown tenant or jurisdiction", ErrInvalidInput)
		}
		if errors.As(err, &pe) && pe.Code == "23505" {
			return TenantJurisdictionConfig{}, fmt.Errorf("%w: a concurrent configuration version was written", ErrInvalidInput)
		}
		return TenantJurisdictionConfig{}, fmt.Errorf("jurisdiction: insert configuration version: %w", err)
	}
	if err := recordTJCAudit(ctx, tx, p.Actor, "jurisdiction_registry.tenant_jurisdiction_config_version_set", p.TenantID, after.ID,
		before, &after, p.ChangeRequestID); err != nil {
		return TenantJurisdictionConfig{}, err
	}
	return after, nil
}

// CloseTenantJurisdictionConfig closes the open version of (tenant, jurisdiction) with no successor (narrowing: with no
// configuration in effect H-3 refuses activation). tx MUST be platform-scoped (asserted).
func CloseTenantJurisdictionConfig(ctx context.Context, tx pgx.Tx, tenantID, jurisdictionID uuid.UUID, actor ActorContext, changeRequestID uuid.UUID) (TenantJurisdictionConfig, error) {
	if err := assertPlatformScope(ctx, tx); err != nil {
		return TenantJurisdictionConfig{}, err
	}
	if err := actor.validate(); err != nil {
		return TenantJurisdictionConfig{}, err
	}
	before, err := lockOpenTJC(ctx, tx, tenantID, jurisdictionID)
	if err != nil {
		return TenantJurisdictionConfig{}, err
	}
	if before == nil {
		return TenantJurisdictionConfig{}, fmt.Errorf("%w: there is no open configuration to close", ErrNotFound)
	}
	closed, err := closeTJC(ctx, tx, before.ID)
	if err != nil {
		return TenantJurisdictionConfig{}, err
	}
	after := *before
	after.EffectiveTo = &closed
	if err := recordTJCAudit(ctx, tx, actor, "jurisdiction_registry.tenant_jurisdiction_config_closed", tenantID, before.ID,
		before, &after, changeRequestID); err != nil {
		return TenantJurisdictionConfig{}, err
	}
	return after, nil
}

// OpenTenantJurisdictionConfig returns the open version of (tenant, jurisdiction) without locking it, or nil.
func OpenTenantJurisdictionConfig(ctx context.Context, tx pgx.Tx, tenantID, jurisdictionID uuid.UUID) (*TenantJurisdictionConfig, error) {
	c, err := scanTJC(tx.QueryRow(ctx, `SELECT `+tjcColumns+` FROM tenant_jurisdiction_configs
		WHERE tenant_id = $1 AND jurisdiction_id = $2 AND effective_to IS NULL ORDER BY effective_from DESC LIMIT 1`, tenantID, jurisdictionID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("jurisdiction: read open configuration: %w", err)
	}
	return &c, nil
}

func tjcState(c *TenantJurisdictionConfig) any {
	if c == nil {
		return nil
	}
	var to any
	if c.EffectiveTo != nil {
		to = c.EffectiveTo.UTC().Format(time.RFC3339Nano)
	}
	return map[string]any{
		"id": c.ID.String(), "tenant_id": c.TenantID.String(), "jurisdiction_id": c.JurisdictionID.String(),
		"kyc_ruleset_id": c.KYCRulesetID, "aml_ruleset_id": c.AMLRulesetID, "rg_ruleset_id": c.RGRulesetID,
		"reporting_ruleset_id": c.ReportingRulesetID, "allowed_currencies": c.AllowedCurrencies,
		"allowed_payment_methods": c.AllowedPaymentMethods, "geo_block_list": c.GeoBlockList,
		"effective_from": c.EffectiveFrom.UTC().Format(time.RFC3339Nano), "effective_to": to,
	}
}

// recordTJCAudit writes the platform-scope audit row of a configuration change, projected to the subject tenant (ADR 0104).
func recordTJCAudit(ctx context.Context, tx pgx.Tx, actor ActorContext, action string, tenantID, targetID uuid.UUID,
	before, after *TenantJurisdictionConfig, changeRequestID uuid.UUID) error {
	md := map[string]any{"before": tjcState(before), "after": tjcState(after), "reason_code": actor.ReasonCode}
	if changeRequestID != uuid.Nil {
		md["change_request_id"] = changeRequestID.String()
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		ActorType: audit.ActorStaff, ActorID: actor.ActorID, SubjectTenantID: tenantID,
		Action: action, TargetType: "tenant_jurisdiction_config", TargetID: targetID.String(),
		Outcome: audit.OutcomeSuccess, IPAddress: actor.IPAddress, UserAgent: strings.TrimSpace(actor.UserAgent),
		RequestID: actor.RequestID, Metadata: md,
	}); err != nil {
		return fmt.Errorf("jurisdiction: audit %s: %w", action, err)
	}
	return nil
}
