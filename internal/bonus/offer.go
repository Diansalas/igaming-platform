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

// OfferStatus is bonus_offers.status's closed set (doc 10 §W2.2).
type OfferStatus string

const (
	OfferDraft   OfferStatus = "draft"
	OfferActive  OfferStatus = "active"
	OfferRetired OfferStatus = "retired"
)

// GrantPolicy selects which canonical trigger mechanic (doc 10 §W3)
// applies - "the one field that distinguishes, e.g., a Coupon Offer
// from a Deposit-bonus Offer" (§W2.2).
type GrantPolicy string

const (
	GrantPolicyAutoIssue              GrantPolicy = "auto_issue"
	GrantPolicyManualApprovalRequired GrantPolicy = "manual_approval_required"
	GrantPolicyCodeRedeemed           GrantPolicy = "code_redeemed"
	GrantPolicyExternalSignal         GrantPolicy = "external_signal"
	GrantPolicyManuallyAssigned       GrantPolicy = "manually_assigned"
)

// RewardKind is BonusReward.reward_kind's closed set (doc 10 §W2.6/§W3).
// R4 is deliberately absent (§W3: "not a reward shape, it is trigger
// mechanic T2 composed with any of R1/R2/R3").
type RewardKind string

const (
	RewardPercentageWithCap RewardKind = "R1"
	RewardFixedValue        RewardKind = "R2"
	RewardFreeRoundOrBet    RewardKind = "R3"
	RewardManuallyAssigned  RewardKind = "R5"
	RewardExternalSignal    RewardKind = "R6"
)

// FulfillmentDestination is BonusReward's own field (ADR 0032 §6(c)).
type FulfillmentDestination string

const (
	FulfillmentIntoPlatformWallet FulfillmentDestination = "into_platform_wallet"
	FulfillmentInsideProvider     FulfillmentDestination = "inside_provider"
)

// PayoutOrdering is the Payout axis's cash/bonus release ordering.
type PayoutOrdering string

const (
	PayoutCashFirst  PayoutOrdering = "cash_first"
	PayoutBonusFirst PayoutOrdering = "bonus_first"
)

// Offer mirrors one bonus_offers row (doc 10 §1.1/§W2.2).
type Offer struct {
	ID                 uuid.UUID
	TenantID           uuid.UUID
	BrandID            *uuid.UUID
	CampaignID         uuid.UUID
	CampaignVersionID  uuid.UUID // immutable pin
	Status             OfferStatus
	GrantPolicy        GrantPolicy
	WindowStart        *time.Time
	WindowEnd          *time.Time
	CurrentVersionID   *uuid.UUID
	CreatedByActorType ActorType
	CreatedByActorID   uuid.UUID
	CreatedAt          time.Time
}

const offerColumns = `id, tenant_id, brand_id, campaign_id, campaign_version_id, status, grant_policy, window_start, window_end, current_version_id, created_by_actor_type, created_by_actor_id, created_at`

func scanOffer(row rowScanner) (Offer, error) {
	var (
		o                  Offer
		status             string
		grantPolicy        string
		createdByActorType string
	)
	err := row.Scan(
		&o.ID, &o.TenantID, &o.BrandID, &o.CampaignID, &o.CampaignVersionID, &status, &grantPolicy, &o.WindowStart, &o.WindowEnd,
		&o.CurrentVersionID, &createdByActorType, &o.CreatedByActorID, &o.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Offer{}, ErrNotFound
	}
	if err != nil {
		return Offer{}, fmt.Errorf("bonus: scan offer: %w", err)
	}
	o.Status = OfferStatus(status)
	o.GrantPolicy = GrantPolicy(grantPolicy)
	o.CreatedByActorType = ActorType(createdByActorType)
	return o, nil
}

// CreateOffer inserts a new bonus_offers row.
func CreateOffer(ctx context.Context, tx pgx.Tx, o Offer) (Offer, error) {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	if o.Status == "" {
		o.Status = OfferDraft
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_offers (id, tenant_id, brand_id, campaign_id, campaign_version_id, status, grant_policy, window_start, window_end, created_by_actor_type, created_by_actor_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING `+offerColumns,
		o.ID, o.TenantID, o.BrandID, o.CampaignID, o.CampaignVersionID, string(o.Status), string(o.GrantPolicy), o.WindowStart, o.WindowEnd,
		string(o.CreatedByActorType), o.CreatedByActorID,
	)
	return scanOffer(row)
}

// GetOfferByID looks up an Offer by id.
func GetOfferByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (Offer, error) {
	row := tx.QueryRow(ctx, `SELECT `+offerColumns+` FROM bonus_offers WHERE id = $1`, id)
	return scanOffer(row)
}

// ListOffersByCampaign returns every Offer under a Campaign.
func ListOffersByCampaign(ctx context.Context, tx pgx.Tx, tenantID, campaignID uuid.UUID) ([]Offer, error) {
	rows, err := tx.Query(ctx, `SELECT `+offerColumns+` FROM bonus_offers WHERE tenant_id = $1 AND campaign_id = $2 ORDER BY created_at DESC`, tenantID, campaignID)
	if err != nil {
		return nil, fmt.Errorf("bonus: list offers: %w", err)
	}
	defer rows.Close()
	var out []Offer
	for rows.Next() {
		o, err := scanOffer(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// UpdateOfferStatus performs an unconditional status write - no
// transition-legality/four-eyes enforcement (Phase 3's job).
func UpdateOfferStatus(ctx context.Context, tx pgx.Tx, tenantID, id uuid.UUID, status OfferStatus) (Offer, error) {
	row := tx.QueryRow(ctx, `UPDATE bonus_offers SET status = $3 WHERE tenant_id = $1 AND id = $2 RETURNING `+offerColumns, tenantID, id, string(status))
	return scanOffer(row)
}

// SetOfferCurrentVersion points current_version_id at an OfferVersion.
func SetOfferCurrentVersion(ctx context.Context, tx pgx.Tx, tenantID, offerID, versionID uuid.UUID) (Offer, error) {
	row := tx.QueryRow(ctx, `UPDATE bonus_offers SET current_version_id = $3 WHERE tenant_id = $1 AND id = $2 RETURNING `+offerColumns, tenantID, offerID, versionID)
	return scanOffer(row)
}

// OfferVersion mirrors one bonus_offer_versions row (doc 10 §W2.2, the
// five axes: Eligibility/Reward/Wagering/Payout/Abuse-control). Every
// monetary field is *big.Int; every "structured, not yet CHECK-
// constrained" field is opaque JSON.
type OfferVersion struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	OfferID       uuid.UUID
	VersionNumber int32

	// Eligibility axis
	EligibilitySegment        SegmentReference
	EligibilityJurisdictions  []string
	EligibilityDepositMethods []string
	FirstDepositOnly          bool
	MinQualifyingAmount       *big.Int
	MaxQualifyingAmount       *big.Int
	VIPTierSegment            SegmentReference
	OptInRequired             bool
	KYCRGLevelRequired        *string
	RedemptionCode            *string
	RedemptionCodePoolRef     *string
	RedemptionValidationRule  []byte
	RedemptionLimitPerPlayer  *int32
	RedemptionLimitGlobal     *int32
	StackingConflictPredicate []byte

	// Reward axis
	RewardKind             RewardKind
	RewardAssetCode        string
	RewardCalculation      []byte
	RoundingRuleID         *uuid.UUID
	FulfillmentDestination FulfillmentDestination
	FundingSource          string // "operator" | "provider:<id>"

	// Wagering axis
	WageringMultiplierBP    *int32
	ContributionWeightTable []byte
	MaxBetWhileWagering     *big.Int
	ExcludedGames           []byte
	WageringTimeLimit       *time.Duration

	// Payout axis
	MaxCashoutAmount         *big.Int
	MaxCashoutPercentBP      *int32
	PayoutOrdering           *PayoutOrdering
	PartialReleaseThresholds []byte
	PayoutTimeLimit          *time.Duration

	// Abuse-control axis
	VelocityCapRefs                []byte
	DeviceFingerprintLinkingConfig []byte
	ManualReviewRouting            []byte

	TriggerMechanic        *string
	CompletionMechanic     *string
	TermsAndConditionsText *string

	CreatedByActorType ActorType
	CreatedByActorID   uuid.UUID
	CreatedAt          time.Time
}

const offerVersionColumns = `
	id, tenant_id, offer_id, version_number,
	eligibility_segment_id, eligibility_segment_version_id, eligibility_jurisdiction_list, eligibility_deposit_methods,
	first_deposit_only, min_qualifying_amount, max_qualifying_amount, vip_tier_segment_id, vip_tier_segment_version_id,
	opt_in_required, kyc_rg_level_required, redemption_code, redemption_code_pool_ref, redemption_validation_rule,
	redemption_limit_per_player, redemption_limit_global, stacking_conflict_predicate,
	reward_kind, reward_asset_code, reward_calculation, rounding_rule_id, fulfillment_destination, funding_source,
	wagering_multiplier_bp, contribution_weight_table, max_bet_while_wagering, excluded_games, wagering_time_limit,
	max_cashout_amount, max_cashout_percent_bp, payout_ordering, partial_release_thresholds, payout_time_limit,
	velocity_cap_refs, device_fingerprint_linking_config, manual_review_routing,
	trigger_mechanic, completion_mechanic, terms_and_conditions_text,
	created_by_actor_type, created_by_actor_id, created_at`

func scanOfferVersion(row rowScanner) (OfferVersion, error) {
	var (
		v                   OfferVersion
		minQualifying       pgtype.Numeric
		maxQualifying       pgtype.Numeric
		maxBetWhileWagering pgtype.Numeric
		maxCashoutAmount    pgtype.Numeric
		wageringTimeLimit   pgtype.Interval
		payoutTimeLimit     pgtype.Interval
		rewardKind          string
		fulfillmentDest     string
		payoutOrdering      *string
		createdByActorType  string
	)
	err := row.Scan(
		&v.ID, &v.TenantID, &v.OfferID, &v.VersionNumber,
		&v.EligibilitySegment.SegmentID, &v.EligibilitySegment.SegmentVersionID, &v.EligibilityJurisdictions, &v.EligibilityDepositMethods,
		&v.FirstDepositOnly, &minQualifying, &maxQualifying, &v.VIPTierSegment.SegmentID, &v.VIPTierSegment.SegmentVersionID,
		&v.OptInRequired, &v.KYCRGLevelRequired, &v.RedemptionCode, &v.RedemptionCodePoolRef, &v.RedemptionValidationRule,
		&v.RedemptionLimitPerPlayer, &v.RedemptionLimitGlobal, &v.StackingConflictPredicate,
		&rewardKind, &v.RewardAssetCode, &v.RewardCalculation, &v.RoundingRuleID, &fulfillmentDest, &v.FundingSource,
		&v.WageringMultiplierBP, &v.ContributionWeightTable, &maxBetWhileWagering, &v.ExcludedGames, &wageringTimeLimit,
		&maxCashoutAmount, &v.MaxCashoutPercentBP, &payoutOrdering, &v.PartialReleaseThresholds, &payoutTimeLimit,
		&v.VelocityCapRefs, &v.DeviceFingerprintLinkingConfig, &v.ManualReviewRouting,
		&v.TriggerMechanic, &v.CompletionMechanic, &v.TermsAndConditionsText,
		&createdByActorType, &v.CreatedByActorID, &v.CreatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return OfferVersion{}, ErrNotFound
	}
	if err != nil {
		return OfferVersion{}, fmt.Errorf("bonus: scan offer version: %w", err)
	}
	v.RewardKind = RewardKind(rewardKind)
	v.FulfillmentDestination = FulfillmentDestination(fulfillmentDest)
	v.CreatedByActorType = ActorType(createdByActorType)
	if payoutOrdering != nil {
		po := PayoutOrdering(*payoutOrdering)
		v.PayoutOrdering = &po
	}
	for dst, src := range map[**big.Int]pgtype.Numeric{
		&v.MinQualifyingAmount: minQualifying,
		&v.MaxQualifyingAmount: maxQualifying,
		&v.MaxBetWhileWagering: maxBetWhileWagering,
		&v.MaxCashoutAmount:    maxCashoutAmount,
	} {
		if src.Valid {
			amt, err := numericToBigInt(src)
			if err != nil {
				return OfferVersion{}, err
			}
			*dst = amt
		}
	}
	if wageringTimeLimit.Valid && wageringTimeLimit.Days == 0 && wageringTimeLimit.Months == 0 {
		d := time.Duration(wageringTimeLimit.Microseconds) * time.Microsecond
		v.WageringTimeLimit = &d
	}
	if payoutTimeLimit.Valid && payoutTimeLimit.Days == 0 && payoutTimeLimit.Months == 0 {
		d := time.Duration(payoutTimeLimit.Microseconds) * time.Microsecond
		v.PayoutTimeLimit = &d
	}
	return v, nil
}

func durationToInterval(d *time.Duration) *pgtype.Interval {
	if d == nil {
		return nil
	}
	return &pgtype.Interval{Microseconds: d.Microseconds(), Valid: true}
}

// CreateOfferVersion inserts a new, append-only OfferVersion row. The
// database's own immutability trigger (migration 0055) is the
// enforcement mechanism - there is deliberately no UpdateOfferVersion.
func CreateOfferVersion(ctx context.Context, tx pgx.Tx, v OfferVersion) (OfferVersion, error) {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	row := tx.QueryRow(ctx, `
		INSERT INTO bonus_offer_versions (
			id, tenant_id, offer_id, version_number,
			eligibility_segment_id, eligibility_segment_version_id, eligibility_jurisdiction_list, eligibility_deposit_methods,
			first_deposit_only, min_qualifying_amount, max_qualifying_amount, vip_tier_segment_id, vip_tier_segment_version_id,
			opt_in_required, kyc_rg_level_required, redemption_code, redemption_code_pool_ref, redemption_validation_rule,
			redemption_limit_per_player, redemption_limit_global, stacking_conflict_predicate,
			reward_kind, reward_asset_code, reward_calculation, rounding_rule_id, fulfillment_destination, funding_source,
			wagering_multiplier_bp, contribution_weight_table, max_bet_while_wagering, excluded_games, wagering_time_limit,
			max_cashout_amount, max_cashout_percent_bp, payout_ordering, partial_release_thresholds, payout_time_limit,
			velocity_cap_refs, device_fingerprint_linking_config, manual_review_routing,
			trigger_mechanic, completion_mechanic, terms_and_conditions_text,
			created_by_actor_type, created_by_actor_id
		) VALUES (
			$1,$2,$3,$4,
			$5,$6,$7,$8,
			$9,$10,$11,$12,$13,
			$14,$15,$16,$17,$18,
			$19,$20,$21,
			$22,$23,$24,$25,$26,$27,
			$28,$29,$30,$31,$32,
			$33,$34,$35,$36,$37,
			$38,$39,$40,
			$41,$42,$43,
			$44,$45
		) RETURNING `+offerVersionColumns,
		v.ID, v.TenantID, v.OfferID, v.VersionNumber,
		v.EligibilitySegment.SegmentID, v.EligibilitySegment.SegmentVersionID, nonNilStrings(v.EligibilityJurisdictions), nonNilStrings(v.EligibilityDepositMethods),
		v.FirstDepositOnly, bigIntToNumeric(v.MinQualifyingAmount), bigIntToNumeric(v.MaxQualifyingAmount), v.VIPTierSegment.SegmentID, v.VIPTierSegment.SegmentVersionID,
		v.OptInRequired, v.KYCRGLevelRequired, v.RedemptionCode, v.RedemptionCodePoolRef, nonNilJSON(v.RedemptionValidationRule),
		v.RedemptionLimitPerPlayer, v.RedemptionLimitGlobal, nonNilJSON(v.StackingConflictPredicate),
		string(v.RewardKind), v.RewardAssetCode, nonNilJSON(v.RewardCalculation), v.RoundingRuleID, string(v.FulfillmentDestination), v.FundingSource,
		v.WageringMultiplierBP, nonNilJSON(v.ContributionWeightTable), bigIntToNumeric(v.MaxBetWhileWagering), nonNilJSONArray(v.ExcludedGames), durationToInterval(v.WageringTimeLimit),
		bigIntToNumeric(v.MaxCashoutAmount), v.MaxCashoutPercentBP, payoutOrderingPtr(v.PayoutOrdering), nonNilJSONArray(v.PartialReleaseThresholds), durationToInterval(v.PayoutTimeLimit),
		nonNilJSONArray(v.VelocityCapRefs), nonNilJSON(v.DeviceFingerprintLinkingConfig), nonNilJSON(v.ManualReviewRouting),
		v.TriggerMechanic, v.CompletionMechanic, v.TermsAndConditionsText,
		string(v.CreatedByActorType), v.CreatedByActorID,
	)
	return scanOfferVersion(row)
}

func nonNilJSONArray(b []byte) []byte {
	if b == nil {
		return []byte("[]")
	}
	return b
}

func payoutOrderingPtr(p *PayoutOrdering) *string {
	if p == nil {
		return nil
	}
	s := string(*p)
	return &s
}

// GetOfferVersionByID looks up an OfferVersion by id.
func GetOfferVersionByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (OfferVersion, error) {
	row := tx.QueryRow(ctx, `SELECT `+offerVersionColumns+` FROM bonus_offer_versions WHERE id = $1`, id)
	return scanOfferVersion(row)
}

// ListOfferVersions returns every version of an Offer, oldest first.
func ListOfferVersions(ctx context.Context, tx pgx.Tx, tenantID, offerID uuid.UUID) ([]OfferVersion, error) {
	rows, err := tx.Query(ctx, `SELECT `+offerVersionColumns+` FROM bonus_offer_versions WHERE tenant_id = $1 AND offer_id = $2 ORDER BY version_number ASC`, tenantID, offerID)
	if err != nil {
		return nil, fmt.Errorf("bonus: list offer versions: %w", err)
	}
	defer rows.Close()
	var out []OfferVersion
	for rows.Next() {
		v, err := scanOfferVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
