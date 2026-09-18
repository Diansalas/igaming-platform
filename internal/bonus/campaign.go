package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// ErrNotFound is returned when a row does not exist (or is not visible
// under the caller's current RLS scope).
var ErrNotFound = errors.New("bonus: not found")

// CampaignStatus is bonus_campaigns.status's closed set (doc 10 §W2.1).
type CampaignStatus string

const (
	CampaignDraft    CampaignStatus = "draft"
	CampaignActive   CampaignStatus = "active"
	CampaignPaused   CampaignStatus = "paused"
	CampaignEnded    CampaignStatus = "ended"
	CampaignArchived CampaignStatus = "archived"
)

// Campaign mirrors one bonus_campaigns row (doc 10 §1.1/§W2.1). tenant_id
// is NOT NULL in this schema (security-architecture.md §B1.3's Wave 1
// recommendation - no platform-wide Campaign this Wave), so it is a
// plain uuid.UUID, never nullable, unlike the design doc's own §8
// "platform-wide" language.
type Campaign struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	BrandID            *uuid.UUID
	Status             CampaignStatus
	FulfillmentOwner   string // "internal" | "external:<provider_id>"
	CurrentVersionID   *uuid.UUID
	CreatedByActorType ActorType
	CreatedByActorID   uuid.UUID
	CreatedAt          time.Time
}

// CampaignVersion mirrors one bonus_campaign_versions row (doc 10 §W2.1).
// Structured axis content (display_copy, jurisdiction/asset/product
// restriction, risk rule refs) is opaque JSON to this package - Phase 3
// owns interpreting it.
type CampaignVersion struct {
	ID                      uuid.UUID
	TenantID                uuid.UUID
	CampaignID              uuid.UUID
	VersionNumber           int32
	Name                    string
	DisplayCopy             []byte
	WindowStart             *time.Time
	WindowEnd               *time.Time
	TargetSegment           SegmentReference
	JurisdictionRestriction []string
	AssetRestriction        []string
	ProductRestriction      []string
	BudgetCapAmount         *big.Int // NUMERIC(38,0), nil iff no budget cap set
	BudgetCapAssetCode      *string
	RiskRuleRefs            []byte
	TriggerMechanic         *string
	RewardMechanic          *string
	CompletionMechanic      *string
	CatalogueItem           *string
	TermsVersion            *string
	TermsText               *string
	CreatedByActorType      ActorType
	CreatedByActorID        uuid.UUID
	CreatedAt               time.Time
}

const campaignColumns = `id, tenant_id, brand_id, status, fulfillment_owner, current_version_id, created_by_actor_type, created_by_actor_id, created_at`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCampaign(row rowScanner) (Campaign, error) {
	var (
		c                  Campaign
		status             string
		createdByActorType string
	)
	err := row.Scan(&c.ID, &c.TenantID, &c.BrandID, &status, &c.FulfillmentOwner, &c.CurrentVersionID, &createdByActorType, &c.CreatedByActorID, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Campaign{}, ErrNotFound
	}
	if err != nil {
		return Campaign{}, fmt.Errorf("bonus: scan campaign: %w", err)
	}
	c.Status = CampaignStatus(status)
	c.CreatedByActorType = ActorType(createdByActorType)
	return c, nil
}

// CreateCampaign inserts a new bonus_campaigns row. TenantID must come
// from authenticated server-side context only (CLAUDE.md). No fulfillment
// eligibility/four-eyes/activation logic is performed - that is Phase 3.
func CreateCampaign(ctx context.Context, tx pgx.Tx, c Campaign) (Campaign, error) {
	if c.ID == uuid.Nil {
		c.ID = uuid.New()
	}
	if c.Status == "" {
		c.Status = CampaignDraft
	}
	if c.FulfillmentOwner == "" {
		c.FulfillmentOwner = "internal"
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_campaigns (id, tenant_id, brand_id, status, fulfillment_owner, created_by_actor_type, created_by_actor_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING `+campaignColumns,
		c.ID, c.TenantID, c.BrandID, string(c.Status), c.FulfillmentOwner, string(c.CreatedByActorType), c.CreatedByActorID,
	)
	return scanCampaign(row)
}

// GetCampaignByID looks up a Campaign by id within the caller's current
// RLS scope.
func GetCampaignByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Campaign, error) {
	row := tx.QueryRow(ctx, `SELECT `+campaignColumns+` FROM bonus_campaigns WHERE id = $1`, id)
	return scanCampaign(row)
}

// ListCampaigns returns every Campaign visible under the caller's
// current RLS scope, most recently created first.
func ListCampaigns(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]Campaign, error) {
	rows, err := tx.Query(ctx, `SELECT `+campaignColumns+` FROM bonus_campaigns WHERE tenant_id = $1 ORDER BY created_at DESC`, tenantID)
	if err != nil {
		return nil, fmt.Errorf("bonus: list campaigns: %w", err)
	}
	defer rows.Close()
	var out []Campaign
	for rows.Next() {
		c, err := scanCampaign(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// UpdateCampaignStatus performs the one allowed-transition update this
// package provides: an unconditional status write. It enforces no
// transition-legality rule (doc 10's activate/suspend asymmetry, four-eyes
// above a cost tier, etc.) - that is Phase 3's state-machine layer.
func UpdateCampaignStatus(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, status CampaignStatus) (Campaign, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bonus_campaigns SET status = $3 WHERE tenant_id = $1 AND id = $2
		RETURNING `+campaignColumns,
		tenantID, id, string(status),
	)
	return scanCampaign(row)
}

// SetCampaignCurrentVersion points current_version_id at a
// CampaignVersion belonging to the same Campaign/tenant (the FK
// constraint enforces the tenant match; this function does not itself
// verify campaign_id matches, leaving that to the caller/FK).
func SetCampaignCurrentVersion(ctx context.Context, tx pgx.Tx, tenantID, campaignID, versionID uuid.UUID) (Campaign, error) {
	row := tx.QueryRow(ctx, `
		UPDATE bonus_campaigns SET current_version_id = $3 WHERE tenant_id = $1 AND id = $2
		RETURNING `+campaignColumns,
		tenantID, campaignID, versionID,
	)
	return scanCampaign(row)
}

const campaignVersionColumns = `
	id, tenant_id, campaign_id, version_number, name, display_copy, window_start, window_end,
	target_segment_id, target_segment_version_id, jurisdiction_restriction, asset_restriction, product_restriction,
	budget_cap_amount, budget_cap_asset_code, risk_rule_refs,
	trigger_mechanic, reward_mechanic, completion_mechanic, catalogue_item, terms_version, terms_text,
	created_by_actor_type, created_by_actor_id, created_at`

func scanCampaignVersion(row rowScanner) (CampaignVersion, error) {
	var (
		v                  CampaignVersion
		budgetCap          pgtype.Numeric
		createdByActorType string
	)
	err := row.Scan(
		&v.ID, &v.TenantID, &v.CampaignID, &v.VersionNumber, &v.Name, &v.DisplayCopy, &v.WindowStart, &v.WindowEnd,
		&v.TargetSegment.SegmentID, &v.TargetSegment.SegmentVersionID, &v.JurisdictionRestriction, &v.AssetRestriction, &v.ProductRestriction,
		&budgetCap, &v.BudgetCapAssetCode, &v.RiskRuleRefs,
		&v.TriggerMechanic, &v.RewardMechanic, &v.CompletionMechanic, &v.CatalogueItem, &v.TermsVersion, &v.TermsText,
		&createdByActorType, &v.CreatedByActorID, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CampaignVersion{}, ErrNotFound
	}
	if err != nil {
		return CampaignVersion{}, fmt.Errorf("bonus: scan campaign version: %w", err)
	}
	v.CreatedByActorType = ActorType(createdByActorType)
	if budgetCap.Valid {
		amt, err := numericToBigInt(budgetCap)
		if err != nil {
			return CampaignVersion{}, err
		}
		v.BudgetCapAmount = amt
	}
	return v, nil
}

// CreateCampaignVersion inserts a new, append-only CampaignVersion row.
// The database's own immutability trigger (migration 0054) is the
// enforcement mechanism, not this function - there is deliberately no
// UpdateCampaignVersion in this package.
func CreateCampaignVersion(ctx context.Context, tx pgx.Tx, v CampaignVersion) (CampaignVersion, error) {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_campaign_versions (
			id, tenant_id, campaign_id, version_number, name, display_copy, window_start, window_end,
			target_segment_id, target_segment_version_id, jurisdiction_restriction, asset_restriction, product_restriction,
			budget_cap_amount, budget_cap_asset_code, risk_rule_refs,
			trigger_mechanic, reward_mechanic, completion_mechanic, catalogue_item, terms_version, terms_text,
			created_by_actor_type, created_by_actor_id
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24)
		RETURNING `+campaignVersionColumns,
		v.ID, v.TenantID, v.CampaignID, v.VersionNumber, v.Name, nonNilJSON(v.DisplayCopy), v.WindowStart, v.WindowEnd,
		v.TargetSegment.SegmentID, v.TargetSegment.SegmentVersionID, nonNilStrings(v.JurisdictionRestriction), nonNilStrings(v.AssetRestriction), nonNilStrings(v.ProductRestriction),
		bigIntToNumeric(v.BudgetCapAmount), v.BudgetCapAssetCode, nonNilJSON(v.RiskRuleRefs),
		v.TriggerMechanic, v.RewardMechanic, v.CompletionMechanic, v.CatalogueItem, v.TermsVersion, v.TermsText,
		string(v.CreatedByActorType), v.CreatedByActorID,
	)
	return scanCampaignVersion(row)
}

// GetCampaignVersionByID looks up a CampaignVersion by id.
func GetCampaignVersionByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (CampaignVersion, error) {
	row := tx.QueryRow(ctx, `SELECT `+campaignVersionColumns+` FROM bonus_campaign_versions WHERE id = $1`, id)
	return scanCampaignVersion(row)
}

// ListCampaignVersions returns every version of a Campaign, oldest first.
func ListCampaignVersions(ctx context.Context, tx pgx.Tx, tenantID, campaignID uuid.UUID) ([]CampaignVersion, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+campaignVersionColumns+` FROM bonus_campaign_versions WHERE tenant_id = $1 AND campaign_id = $2 ORDER BY version_number ASC`,
		tenantID, campaignID,
	)
	if err != nil {
		return nil, fmt.Errorf("bonus: list campaign versions: %w", err)
	}
	defer rows.Close()
	var out []CampaignVersion
	for rows.Next() {
		v, err := scanCampaignVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
