// Stage 4H-B1 Wave 3 Phase 3 (item F, continued): the per-domain
// activate/publish/execute endpoints item E's four-eyes wrappers need a
// caller for, plus the Offer creation surface reconnaissance §2 item 2
// found entirely missing ("zero HTTP handler exists to create an Offer
// at all") and the two-phase manual-grant issue/activate surface
// item E's own IssueManualGrantRequest/ActivateManualGrantWithApproval
// split requires. See bonus_governance_handlers.go's own top-of-file
// doc comment for the conventions every handler in this file follows.
package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/economicop"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// --- campaign_activate ---

func newActivateCampaignHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		campaignID, err := uuid.Parse(r.PathValue("campaignID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid campaign id")
			return
		}

		var resp campaignResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			activated, err := bonus.ActivateCampaign(ctx, tx, tc.TenantID, campaignID, staffID)
			if err != nil {
				return err
			}
			resp = campaignResponse{ID: activated.ID.String(), Status: string(activated.Status), FulfillmentOwner: activated.FulfillmentOwner}
			return nil
		})
		if errors.Is(err, bonus.ErrChangeRequestNotApproved) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "four-eyes approval required and not satisfied")
			return
		}
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "campaign not found")
			return
		}
		if err != nil {
			deps.Logger.Error("activate_campaign_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to activate campaign")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- offer create / version create / publish ---

type createOfferRequest struct {
	BrandID           string `json:"brand_id,omitempty"`
	CampaignID        string `json:"campaign_id"`
	CampaignVersionID string `json:"campaign_version_id"`
	GrantPolicy       string `json:"grant_policy"`
}

type offerResponse struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	GrantPolicy      string `json:"grant_policy"`
	CurrentVersionID string `json:"current_version_id,omitempty"`
}

func toOfferResponse(o bonus.Offer) offerResponse {
	resp := offerResponse{ID: o.ID.String(), Status: string(o.Status), GrantPolicy: string(o.GrantPolicy)}
	if o.CurrentVersionID != nil {
		resp.CurrentVersionID = o.CurrentVersionID.String()
	}
	return resp
}

// newCreateOfferHandler closes reconnaissance §2 item 2's own finding -
// "zero HTTP handler exists to create an Offer at all" - a Draft Offer
// itself moves no value (identical posture to newCreateCampaignHandler's
// own Draft-only creation), so this endpoint is NOT four-eyes-gated;
// only offer_publish (below) is.
func newCreateOfferHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		var req createOfferRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireUUID("campaign_id", req.CampaignID)
		v.RequireUUID("campaign_version_id", req.CampaignVersionID)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		if req.GrantPolicy == "" {
			req.GrantPolicy = string(bonus.GrantPolicyManualApprovalRequired)
		}
		v.RequireOneOf("grant_policy", req.GrantPolicy,
			string(bonus.GrantPolicyAutoIssue), string(bonus.GrantPolicyManualApprovalRequired),
			string(bonus.GrantPolicyCodeRedeemed), string(bonus.GrantPolicyExternalSignal), string(bonus.GrantPolicyManuallyAssigned))
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		campaignID, _ := uuid.Parse(req.CampaignID)
		campaignVersionID, _ := uuid.Parse(req.CampaignVersionID)
		var brandID *uuid.UUID
		if req.BrandID != "" {
			parsed, perr := uuid.Parse(req.BrandID)
			if perr != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "brand_id must be a valid UUID")
				return
			}
			brandID = &parsed
		}

		var resp offerResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			o, err := bonus.CreateOffer(ctx, tx, bonus.Offer{
				TenantID: tc.TenantID, BrandID: brandID, CampaignID: campaignID, CampaignVersionID: campaignVersionID,
				GrantPolicy: bonus.GrantPolicy(req.GrantPolicy), CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID,
			})
			if err != nil {
				return err
			}
			resp = toOfferResponse(o)
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
				Action: "bonus_offer.created", TargetType: "bonus_offer", TargetID: o.ID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
		})
		if err != nil {
			deps.Logger.Error("create_offer_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create offer")
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// createOfferVersionRequest carries every OfferVersion field this
// surface exposes. Every monetary field is a decimal-string minor-unit
// amount (CLAUDE.md - never a JSON float).
type createOfferVersionRequest struct {
	MinQualifyingAmount       string   `json:"min_qualifying_amount,omitempty"`
	MaxQualifyingAmount       string   `json:"max_qualifying_amount,omitempty"`
	FirstDepositOnly          bool     `json:"first_deposit_only,omitempty"`
	EligibilityDepositMethods []string `json:"eligibility_deposit_methods,omitempty"`
	RewardKind                string   `json:"reward_kind"`
	RewardAssetCode           string   `json:"reward_asset_code"`
	RewardCalculation         string   `json:"reward_calculation"` // raw JSON object, as a string
	FulfillmentDestination    string   `json:"fulfillment_destination"`
	FundingSource             string   `json:"funding_source"`
	WageringMultiplierBP      int32    `json:"wagering_multiplier_bp,omitempty"`
	ContributionWeightTable   string   `json:"contribution_weight_table,omitempty"` // raw JSON object, as a string
	CompletionMechanic        string   `json:"completion_mechanic,omitempty"`
}

type offerVersionResponse struct {
	ID      string `json:"id"`
	OfferID string `json:"offer_id"`
	Version int32  `json:"version_number"`
}

func newCreateOfferVersionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		offerID, err := uuid.Parse(r.PathValue("offerID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid offer id")
			return
		}
		var req createOfferVersionRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("reward_kind", req.RewardKind,
			string(bonus.RewardPercentageWithCap), string(bonus.RewardFixedValue), string(bonus.RewardFreeRoundOrBet),
			string(bonus.RewardManuallyAssigned), string(bonus.RewardExternalSignal))
		v.RequireNonEmpty("reward_asset_code", req.RewardAssetCode)
		v.RequireOneOf("fulfillment_destination", req.FulfillmentDestination, string(bonus.FulfillmentIntoPlatformWallet), string(bonus.FulfillmentInsideProvider))
		v.RequireNonEmpty("funding_source", req.FundingSource)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		var minQualifying, maxQualifying *big.Int
		for _, pair := range []struct {
			field string
			raw   string
			dst   **big.Int
		}{{"min_qualifying_amount", req.MinQualifyingAmount, &minQualifying}, {"max_qualifying_amount", req.MaxQualifyingAmount, &maxQualifying}} {
			if pair.raw == "" {
				continue
			}
			amt, ok := new(big.Int).SetString(pair.raw, 10)
			if !ok || amt.Sign() < 0 {
				apierror.Write(w, requestID, apierror.CodeValidation, pair.field+" must be a non-negative decimal integer string")
				return
			}
			*pair.dst = amt
		}
		rewardCalculation := []byte(req.RewardCalculation)
		if len(rewardCalculation) == 0 {
			rewardCalculation = []byte("{}")
		} else if !jsonValid(rewardCalculation) {
			apierror.Write(w, requestID, apierror.CodeValidation, "reward_calculation must be valid JSON")
			return
		}
		contributionWeightTable := []byte(req.ContributionWeightTable)
		if len(contributionWeightTable) > 0 && !jsonValid(contributionWeightTable) {
			apierror.Write(w, requestID, apierror.CodeValidation, "contribution_weight_table must be valid JSON")
			return
		}
		var wageringMultiplierBP *int32
		if req.WageringMultiplierBP > 0 {
			wageringMultiplierBP = &req.WageringMultiplierBP
		}
		var completionMechanic *string
		if req.CompletionMechanic != "" {
			completionMechanic = &req.CompletionMechanic
		}

		var resp offerVersionResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			offer, err := bonus.GetOfferByID(ctx, tx, offerID)
			if err != nil {
				return err
			}
			existing, err := bonus.ListOfferVersions(ctx, tx, tc.TenantID, offerID)
			if err != nil {
				return err
			}
			ov, err := bonus.CreateOfferVersion(ctx, tx, bonus.OfferVersion{
				TenantID: tc.TenantID, OfferID: offer.ID, VersionNumber: int32(len(existing)) + 1,
				MinQualifyingAmount: minQualifying, MaxQualifyingAmount: maxQualifying,
				FirstDepositOnly: req.FirstDepositOnly, EligibilityDepositMethods: req.EligibilityDepositMethods,
				RewardKind: bonus.RewardKind(req.RewardKind), RewardAssetCode: req.RewardAssetCode, RewardCalculation: rewardCalculation,
				FulfillmentDestination: bonus.FulfillmentDestination(req.FulfillmentDestination), FundingSource: req.FundingSource,
				WageringMultiplierBP: wageringMultiplierBP, ContributionWeightTable: contributionWeightTable,
				CompletionMechanic: completionMechanic,
				CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID,
			})
			if err != nil {
				return err
			}
			resp = offerVersionResponse{ID: ov.ID.String(), OfferID: ov.OfferID.String(), Version: ov.VersionNumber}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
				Action: "bonus_offer_version.created", TargetType: "bonus_offer_version", TargetID: ov.ID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
		})
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "offer not found")
			return
		}
		if err != nil {
			deps.Logger.Error("create_offer_version_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create offer version")
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

type publishOfferRequest struct {
	OfferVersionID string `json:"offer_version_id"`
}

func newPublishOfferHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		offerID, err := uuid.Parse(r.PathValue("offerID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid offer id")
			return
		}
		var req publishOfferRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireUUID("offer_version_id", req.OfferVersionID)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		offerVersionID, _ := uuid.Parse(req.OfferVersionID)

		var resp offerResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			published, err := bonus.PublishOfferVersion(ctx, tx, tc.TenantID, offerID, offerVersionID, staffID)
			if err != nil {
				return err
			}
			resp = toOfferResponse(published)
			return nil
		})
		if errors.Is(err, bonus.ErrChangeRequestNotApproved) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "four-eyes approval required and not satisfied")
			return
		}
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "offer or offer version not found")
			return
		}
		if err != nil {
			deps.Logger.Error("publish_offer_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to publish offer: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- manual grant: issue (phase 1) / activate-with-approval (phase 2) ---

// newIssueManualGrantRequestHandler is phase 1 of the four-eyes-gated
// manual grant surface (bonus.IssueManualGrantRequest's own doc comment
// explains why issuance is split from activation - migration 0063's SEP-1
// trigger needs a REAL Grant row to exist before anyone approves a
// request naming it). Reuses issueManualGrantRequest's own request shape
// (bonus_handlers.go) minus reason_code (filed separately, on the
// change-request itself, not this step).
func newIssueManualGrantRequestHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		var req issueManualGrantRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		for _, f := range []struct{ name, val string }{
			{"player_account_id", req.PlayerAccountID}, {"campaign_id", req.CampaignID}, {"campaign_version_id", req.CampaignVersionID},
			{"offer_id", req.OfferID}, {"offer_version_id", req.OfferVersionID}, {"asset_code", req.AssetCode},
			{"funding_source", req.FundingSource}, {"parent_operation_id", req.ParentOperationID},
		} {
			v.RequireNonEmpty(f.name, f.val)
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		playerAccountID, err1 := uuid.Parse(req.PlayerAccountID)
		campaignID, err2 := uuid.Parse(req.CampaignID)
		campaignVersionID, err3 := uuid.Parse(req.CampaignVersionID)
		offerID, err4 := uuid.Parse(req.OfferID)
		offerVersionID, err5 := uuid.Parse(req.OfferVersionID)
		parentOperationID, err6 := uuid.Parse(req.ParentOperationID)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || err6 != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "one or more ids are not valid UUIDs")
			return
		}

		var result grantResponse
		var denied bool
		var denialReason string
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			playerAccount, wallet, err := loadPlayerAccountAndWallet(ctx, tx, tc.TenantID, playerAccountID, req.AssetCode)
			if err != nil {
				return err
			}
			exp, err := lookupDecimalExponent(ctx, tx, req.AssetCode)
			if err != nil {
				return err
			}
			g := bonus.Grant{
				TenantID: tc.TenantID, BrandID: *playerAccount.brandID, PlayerAccountID: playerAccountID, WalletID: wallet,
				CampaignID: campaignID, CampaignVersionID: campaignVersionID, OfferID: offerID, OfferVersionID: offerVersionID,
				AssetCode: req.AssetCode, DecimalExponent: exp,
				FundingSource: req.FundingSource, FulfillmentDestination: bonus.FulfillmentIntoPlatformWallet, FulfillmentOwner: "internal",
				TriggerReference: "manual_grant:" + uuid.NewString(),
			}
			grant, outcome, err := bonus.IssueManualGrantRequest(ctx, tx, g, parentOperationID, req.JurisdictionCode, staffID)
			if err != nil {
				return err
			}
			if !outcome.Allowed {
				denied, denialReason = true, outcome.Code
				return nil
			}
			result = toGrantResponse(grant, nil)
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
				Action: "bonus_grant.manual_issue_requested", TargetType: "bonus_grant", TargetID: grant.ID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"parent_operation_id": req.ParentOperationID},
			})
		})
		if errors.Is(err, economicop.ErrParentOperationNotFound) || errors.Is(err, economicop.ErrParentNotApproved) || errors.Is(err, economicop.ErrParentNotOpenOrExpired) || errors.Is(err, economicop.ErrChildScopeExceedsParent) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "economic operation authorization check failed: "+err.Error())
			return
		}
		if err != nil {
			deps.Logger.Error("issue_manual_grant_request_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to issue manual grant request")
			return
		}
		if denied {
			apierror.Write(w, requestID, apierror.CodeForbidden, "grant denied: "+denialReason)
			return
		}
		writeJSON(w, http.StatusCreated, result)
	}
}

type activateManualGrantRequest struct {
	ParentOperationID string `json:"parent_operation_id"`
	JurisdictionCode  string `json:"jurisdiction_code,omitempty"`
	Amount            string `json:"amount"`
}

func newActivateManualGrantHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		grantID, err := uuid.Parse(r.PathValue("grantID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid grant id")
			return
		}
		var req activateManualGrantRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("parent_operation_id", req.ParentOperationID)
		v.RequireNonEmpty("amount", req.Amount)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		parentOperationID, perr := uuid.Parse(req.ParentOperationID)
		if perr != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "parent_operation_id must be a valid UUID")
			return
		}
		amount, ok := new(big.Int).SetString(req.Amount, 10)
		if !ok || amount.Sign() < 0 {
			apierror.Write(w, requestID, apierror.CodeValidation, "amount must be a non-negative decimal integer string")
			return
		}

		var result grantResponse
		var denied bool
		var denialReason string
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			grant, outcome, err := bonus.ActivateManualGrantWithApproval(ctx, tx, tc.TenantID, grantID, parentOperationID, req.JurisdictionCode, staffID, amount)
			if err != nil {
				return err
			}
			if !outcome.Allowed {
				denied, denialReason = true, outcome.Code
				return nil
			}
			remaining, err := bonus.RemainingBonusBalance(ctx, tx, tc.TenantID, grant.ID)
			if err != nil {
				return err
			}
			result = toGrantResponse(grant, remaining)
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
				Action: "bonus_grant.manual_issue_activated", TargetType: "bonus_grant", TargetID: grant.ID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
		})
		if errors.Is(err, bonus.ErrChangeRequestNotApproved) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "four-eyes approval required and not satisfied")
			return
		}
		if errors.Is(err, economicop.ErrParentOperationNotFound) || errors.Is(err, economicop.ErrParentNotApproved) || errors.Is(err, economicop.ErrParentNotOpenOrExpired) || errors.Is(err, economicop.ErrChildScopeExceedsParent) || errors.Is(err, economicop.ErrBudgetExhausted) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "economic operation authorization check failed: "+err.Error())
			return
		}
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "grant not found")
			return
		}
		if errors.Is(err, bonus.ErrIllegalTransition) {
			apierror.Write(w, requestID, apierror.CodeConflict, "grant is not in a state that can be activated")
			return
		}
		if err != nil {
			deps.Logger.Error("activate_manual_grant_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to activate manual grant")
			return
		}
		if denied {
			apierror.Write(w, requestID, apierror.CodeForbidden, "grant activation denied: "+denialReason)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// --- bulk grant jobs: create / execute ---

type createBulkGrantJobRequest struct {
	CampaignID      string   `json:"campaign_id"`
	OfferVersionID  string   `json:"offer_version_id"`
	TargetKind      string   `json:"target_kind"` // "single_player" | "player_list" (segment/segment_set out of Wave 3 scope)
	TargetPlayerIDs []string `json:"target_player_account_ids"`
	IdempotencyKey  string   `json:"idempotency_key"`
}

type bulkGrantJobResponse struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	ApprovalState string `json:"approval_state"`
}

// newCreateBulkGrantJobHandler creates a BulkGrantJob targeting a static,
// caller-supplied player-id list ONLY (single_player/player_list) - the
// existing, deliberate Wave-scope boundary (targeting.go's own top-of-
// file doc comment: "live dynamic segment evaluation is NOT implemented").
func newCreateBulkGrantJobHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		var req createBulkGrantJobRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireUUID("campaign_id", req.CampaignID)
		v.RequireUUID("offer_version_id", req.OfferVersionID)
		v.RequireOneOf("target_kind", req.TargetKind, string(bonus.TargetSinglePlayer), string(bonus.TargetPlayerList))
		v.RequireNonEmpty("idempotency_key", req.IdempotencyKey)
		if len(req.TargetPlayerIDs) == 0 {
			v.Add("target_player_account_ids", "must contain at least one player id")
		}
		if req.TargetKind == string(bonus.TargetSinglePlayer) && len(req.TargetPlayerIDs) != 1 {
			v.Add("target_player_account_ids", "must contain exactly one player id for target_kind=single_player")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		campaignID, _ := uuid.Parse(req.CampaignID)
		offerVersionID, _ := uuid.Parse(req.OfferVersionID)
		playerIDs := make([]uuid.UUID, 0, len(req.TargetPlayerIDs))
		for _, s := range req.TargetPlayerIDs {
			id, perr := uuid.Parse(s)
			if perr != nil {
				apierror.Write(w, requestID, apierror.CodeValidation, "target_player_account_ids must all be valid UUIDs")
				return
			}
			playerIDs = append(playerIDs, id)
		}

		var resp bulkGrantJobResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			if existing, getErr := bonus.GetBulkGrantJobByIdempotencyKey(ctx, tx, tc.TenantID, req.IdempotencyKey); getErr == nil {
				resp = bulkGrantJobResponse{ID: existing.ID.String(), Status: string(existing.Status), ApprovalState: string(existing.ApprovalState)}
				return nil
			} else if !errors.Is(getErr, bonus.ErrNotFound) {
				return getErr
			}
			ov, err := bonus.GetOfferVersionByID(ctx, tx, offerVersionID)
			if err != nil {
				return err
			}
			campaign, err := bonus.GetCampaignByID(ctx, tx, campaignID)
			if err != nil {
				return err
			}
			job := bonus.BulkGrantJob{
				TenantID: tc.TenantID, CampaignID: campaignID, OfferVersionID: offerVersionID,
				TargetKind:             bonus.BulkGrantJobTargetKind(req.TargetKind),
				RequestedByPrincipalID: staffID, IdempotencyKey: req.IdempotencyKey,
			}
			if campaign.BrandID != nil {
				job.BrandID = *campaign.BrandID
			}
			// Exactly one target representation may be populated per
			// target_kind (migration 0060's own CHECK constraint) - never
			// both, even though this handler already validated
			// target_player_account_ids' own length above.
			if req.TargetKind == string(bonus.TargetSinglePlayer) {
				job.TargetPlayerAccountID = &playerIDs[0]
			} else {
				job.TargetPlayerList = playerIDs
			}
			_ = ov // asset/reward shape validated at execution time (RunStaticBulkGrantJob's own read)
			created, err := bonus.CreateBulkGrantJob(ctx, tx, job)
			if err != nil {
				return err
			}
			resp = bulkGrantJobResponse{ID: created.ID.String(), Status: string(created.Status), ApprovalState: string(created.ApprovalState)}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
				Action: "bulk_grant_job.created", TargetType: "bulk_grant_job", TargetID: created.ID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"target_kind": req.TargetKind, "player_count": len(playerIDs)},
			})
		})
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "campaign or offer version not found")
			return
		}
		if err != nil {
			deps.Logger.Error("create_bulk_grant_job_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create bulk grant job")
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

type executeBulkGrantJobRequest struct {
	JurisdictionCode string `json:"jurisdiction_code,omitempty"`
	Amount           string `json:"amount"`
}

// newExecuteBulkGrantJobHandler consumes the bulk_job_execute approval
// and runs the job SYNCHRONOUSLY (ExecuteBulkGrantJobWithApproval's own
// doc comment names this a disclosed, reversible engineering decision,
// not a Human Decision Register item) - a caller invoking this endpoint
// for a large player list should expect a correspondingly long request,
// bounded only by RunStaticBulkGrantJob's own per-item work.
func newExecuteBulkGrantJobHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		staffID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		jobID, err := uuid.Parse(r.PathValue("jobID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid job id")
			return
		}
		var req executeBulkGrantJobRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("amount", req.Amount)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		amount, ok := new(big.Int).SetString(req.Amount, 10)
		if !ok || amount.Sign() < 0 {
			apierror.Write(w, requestID, apierror.CodeValidation, "amount must be a non-negative decimal integer string")
			return
		}

		var resp bulkGrantJobResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			job, err := bonus.GetBulkGrantJobByID(ctx, tx, jobID)
			if err != nil {
				return err
			}
			ov, err := bonus.GetOfferVersionByID(ctx, tx, job.OfferVersionID)
			if err != nil {
				return err
			}
			campaign, err := bonus.GetCampaignByID(ctx, tx, job.CampaignID)
			if err != nil {
				return err
			}
			var campaignVersionID uuid.UUID
			if campaign.CurrentVersionID != nil {
				campaignVersionID = *campaign.CurrentVersionID
			}
			var target bonus.StaticPlayerListTarget
			switch job.TargetKind {
			case bonus.TargetSinglePlayer:
				if job.TargetPlayerAccountID == nil {
					return errors.New("bonus: single_player job has no target_player_account_id")
				}
				target = bonus.StaticPlayerListTarget{PlayerAccountIDs: []uuid.UUID{*job.TargetPlayerAccountID}}
			case bonus.TargetPlayerList:
				target = bonus.StaticPlayerListTarget{PlayerAccountIDs: job.TargetPlayerList}
			default:
				return bonus.ErrDynamicSegmentTargetingNotSupported
			}
			template := bonus.Grant{
				TenantID: tc.TenantID, BrandID: job.BrandID, CampaignID: job.CampaignID, OfferVersionID: job.OfferVersionID,
				OfferID: ov.OfferID, CampaignVersionID: campaignVersionID,
				AssetCode: ov.RewardAssetCode, FundingSource: ov.FundingSource, FulfillmentDestination: ov.FulfillmentDestination,
				FulfillmentOwner: "internal",
			}
			exp, err := lookupDecimalExponent(ctx, tx, ov.RewardAssetCode)
			if err != nil {
				return err
			}
			template.DecimalExponent = exp

			result, err := bonus.ExecuteBulkGrantJobWithApproval(ctx, tx, tc.TenantID, jobID, staffID, target, template, req.JurisdictionCode, staffID, amount)
			if err != nil {
				return err
			}
			resp = bulkGrantJobResponse{ID: result.Job.ID.String(), Status: string(result.Status), ApprovalState: string(result.Job.ApprovalState)}
			return nil
		})
		if errors.Is(err, bonus.ErrChangeRequestNotApproved) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "four-eyes approval required and not satisfied")
			return
		}
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "bulk grant job or offer version not found")
			return
		}
		if errors.Is(err, bonus.ErrDynamicSegmentTargetingNotSupported) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			deps.Logger.Error("execute_bulk_grant_job_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to execute bulk grant job: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

func jsonValid(b []byte) bool {
	return json.Valid(b)
}
