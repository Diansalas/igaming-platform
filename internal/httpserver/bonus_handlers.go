// HTTP handlers for the Bonus Engine (docs/architecture/10-bonus-engine-
// architecture.md, docs/architecture/25-bonus-gamification-api-
// architecture.md) - player-facing (own grants/progress, coupon
// redemption) and staff-facing (campaign/offer authoring, manual grant
// issuance, suggestion review, held-disposition resolution), curated
// projections per role, matching this project's existing conventions
// (internal/httpserver/withdrawal_handlers.go).
//
// SCOPE, stated honestly: this is Stage 4H-B1 Wave 2 Phase 3's first
// HTTP surface for a domain with a very large business-logic surface
// (internal/bonus). It covers the specific endpoints this dispatch names
// - it does NOT expose every internal/bonus capability (e.g. cashback
// settlement-job triggering, deposit/reload bonus triggering, and
// generic-wagering-bonus issuance are event-driven/internal-caller
// surfaces per doc 10 §1.3, not staff HTTP actions, and are not wired
// here).
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

// --- player-facing: view own grants ---

type grantResponse struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	AssetCode        string `json:"asset_code"`
	CreatedAt        string `json:"created_at"`
	ActivatedAt      string `json:"activated_at,omitempty"`
	TerminalAt       string `json:"terminal_at,omitempty"`
	TerminalReason   string `json:"terminal_reason_code,omitempty"`
	RemainingBalance string `json:"remaining_bonus_balance,omitempty"`
}

func toGrantResponse(g bonus.Grant, remaining *big.Int) grantResponse {
	resp := grantResponse{ID: g.ID.String(), Status: string(g.Status), AssetCode: g.AssetCode, CreatedAt: g.CreatedAt.UTC().Format(rfc3339)}
	if g.ActivatedAt != nil {
		resp.ActivatedAt = g.ActivatedAt.UTC().Format(rfc3339)
	}
	if g.TerminalAt != nil {
		resp.TerminalAt = g.TerminalAt.UTC().Format(rfc3339)
	}
	if g.TerminalTriggerReasonCode != nil {
		resp.TerminalReason = *g.TerminalTriggerReasonCode
	}
	if remaining != nil {
		resp.RemainingBalance = remaining.String()
	}
	return resp
}

// newListMyGrantsHandler is the player-facing "view my own grants"
// surface - a plain, RLS-scoped (WithPlayerScope) read, exactly mirroring
// withdrawal_handlers.go's own "list my withdrawal requests" pattern.
func newListMyGrantsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player subject")
			return
		}

		var out []grantResponse
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			grants, err := bonus.ListGrantsByPlayer(ctx, tx, tc.TenantID, playerAccountID)
			if err != nil {
				return err
			}
			for _, g := range grants {
				remaining, err := bonus.RemainingBonusBalance(ctx, tx, tc.TenantID, g.ID)
				if err != nil {
					return err
				}
				out = append(out, toGrantResponse(g, remaining))
			}
			return nil
		})
		if err != nil {
			deps.Logger.Error("list_my_grants_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list grants")
			return
		}
		if out == nil {
			out = []grantResponse{}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// newGetMyGrantProgressHandler is the player-facing "view my own Grant's
// Progress trail" surface (doc 10 §10's own "the exact record a
// disputing player's case is resolved from" requirement) - a curated
// projection (no internal Risk/RG decision detail beyond the stored
// CODE, per §10.1's own "their Code, not their full internal detail"
// rule; this handler passes the stored codes through as-is, since that
// IS the code-only projection).
type progressEntryResponse struct {
	SequenceNumber int64  `json:"sequence_number"`
	TransitionType string `json:"transition_type"`
	TriggerType    string `json:"trigger_type"`
	BeforeStatus   string `json:"before_status,omitempty"`
	AfterStatus    string `json:"after_status,omitempty"`
	ReasonCode     string `json:"reason_code,omitempty"`
	OccurredAt     string `json:"occurred_at"`
}

func newGetMyGrantProgressHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player subject")
			return
		}
		grantID, err := uuid.Parse(r.PathValue("grantID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid grant id")
			return
		}

		var out []progressEntryResponse
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			g, err := bonus.GetGrantByID(ctx, tx, grantID)
			if err != nil {
				return err
			}
			if g.PlayerAccountID != playerAccountID {
				return bonus.ErrNotFound
			}
			entries, err := bonus.ListGrantProgress(ctx, tx, tc.TenantID, grantID)
			if err != nil {
				return err
			}
			for _, e := range entries {
				resp := progressEntryResponse{
					SequenceNumber: e.SequenceNumber, TransitionType: string(e.TransitionType), TriggerType: string(e.TriggerType),
					OccurredAt: e.OccurredAt.UTC().Format(rfc3339),
				}
				if e.BeforeStatus != nil {
					resp.BeforeStatus = *e.BeforeStatus
				}
				if e.AfterStatus != nil {
					resp.AfterStatus = *e.AfterStatus
				}
				if e.ReasonCode != nil {
					resp.ReasonCode = *e.ReasonCode
				}
				out = append(out, resp)
			}
			return nil
		})
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "grant not found")
			return
		}
		if err != nil {
			deps.Logger.Error("get_my_grant_progress_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load grant progress")
			return
		}
		if out == nil {
			out = []progressEntryResponse{}
		}
		writeJSON(w, http.StatusOK, out)
	}
}

// --- player-facing: redeem a coupon (doc 10 §W4) ---

type redeemCouponRequest struct {
	OfferVersionID string `json:"offer_version_id"`
	Code           string `json:"code"`
}

// newRedeemCouponHandler is doc 10 §W4's coupon-redemption endpoint. Per
// §W4's own binding text, this handler does exactly the three things
// named there - validate the code's format, resolve it to an Offer, and
// check the redemption-limit constraint - before handing off to
// bonus.RedeemCoupon's own identical (none)->issued pipeline. It does
// NOT itself validate a code pool/stacking-conflict predicate beyond
// what OfferVersion.RedemptionCode's own equality check provides -
// resolving the code to a specific OfferVersion (offer_version_id) is
// the caller's own lookup responsibility this Phase, since the frozen
// design leaves "a code (or code-pool reference)" as Offer configuration
// this handler reads, not invents.
func newRedeemCouponHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player subject")
			return
		}

		var req redeemCouponRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("offer_version_id", req.OfferVersionID)
		v.RequireNonEmpty("code", req.Code)
		offerVersionID, parseErr := uuid.Parse(req.OfferVersionID)
		if parseErr != nil {
			v.Add("offer_version_id", "must be a valid UUID")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var result grantResponse
		var denied bool
		var denialReason string
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			offerVersion, err := bonus.GetOfferVersionByID(ctx, tx, offerVersionID)
			if err != nil {
				return err
			}
			if offerVersion.RedemptionCode == nil || *offerVersion.RedemptionCode != req.Code {
				denied, denialReason = true, "invalid_code"
				return nil
			}
			offer, err := bonus.GetOfferByID(ctx, tx, offerVersion.OfferID)
			if err != nil {
				return err
			}
			campaign, err := bonus.GetCampaignByID(ctx, tx, offer.CampaignID)
			if err != nil {
				return err
			}
			playerAccount, wallet, err := loadPlayerAccountAndWallet(ctx, tx, tc.TenantID, playerAccountID, offerVersion.RewardAssetCode)
			if err != nil {
				return err
			}

			amount, err := computeCouponRewardAmount(offerVersion)
			if err != nil {
				return err
			}

			g := bonus.Grant{
				TenantID: tc.TenantID, BrandID: *playerAccount.brandID, PlayerAccountID: playerAccountID, WalletID: wallet,
				CampaignID: campaign.ID, CampaignVersionID: *campaign.CurrentVersionID, OfferID: offer.ID, OfferVersionID: offerVersion.ID,
				AssetCode: offerVersion.RewardAssetCode, DecimalExponent: 0,
				FundingSource: offerVersion.FundingSource, FulfillmentDestination: offerVersion.FulfillmentDestination,
				FulfillmentOwner: campaign.FulfillmentOwner, TriggerReference: "coupon:" + req.Code,
			}
			exp, err := lookupDecimalExponent(ctx, tx, offerVersion.RewardAssetCode)
			if err != nil {
				return err
			}
			g.DecimalExponent = exp

			grant, outcome, err := bonus.RedeemCoupon(ctx, tx, bonus.CouponRedemptionParams{Grant: g, Amount: amount, ActorID: playerAccountID})
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
			return nil
		})
		if errors.Is(err, bonus.ErrNotFound) {
			denied, denialReason = true, "invalid_code"
			err = nil
		}
		if err != nil {
			deps.Logger.Error("redeem_coupon_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to redeem coupon")
			return
		}
		if denied {
			apierror.Write(w, requestID, apierror.CodeForbidden, "coupon redemption denied: "+denialReason)
			return
		}
		writeJSON(w, http.StatusCreated, result)
	}
}

// --- staff-facing: campaign / offer authoring ---

type createCampaignRequest struct {
	BrandID          string `json:"brand_id,omitempty"`
	FulfillmentOwner string `json:"fulfillment_owner"`
}

type campaignResponse struct {
	ID               string `json:"id"`
	Status           string `json:"status"`
	FulfillmentOwner string `json:"fulfillment_owner"`
	// BrandID/CurrentVersionID/CreatedAt are additive (Stage 5 Operator
	// Back Office MVP's read surface, newListCampaignsHandler) -
	// omitempty keeps every pre-existing caller's response shape
	// unchanged when these fields are not populated.
	BrandID          string `json:"brand_id,omitempty"`
	CurrentVersionID string `json:"current_version_id,omitempty"`
	CreatedAt        string `json:"created_at,omitempty"`
}

func toCampaignResponse(c bonus.Campaign) campaignResponse {
	resp := campaignResponse{
		ID: c.ID.String(), Status: string(c.Status), FulfillmentOwner: c.FulfillmentOwner,
		CreatedAt: c.CreatedAt.UTC().Format(rfc3339),
	}
	if c.BrandID != nil {
		resp.BrandID = c.BrandID.String()
	}
	if c.CurrentVersionID != nil {
		resp.CurrentVersionID = c.CurrentVersionID.String()
	}
	return resp
}

func newCreateCampaignHandler(deps Deps) http.HandlerFunc {
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
		var req createCampaignRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		if req.FulfillmentOwner == "" {
			req.FulfillmentOwner = "internal"
		}

		var resp campaignResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var brandID *uuid.UUID
			if req.BrandID != "" {
				parsed, perr := uuid.Parse(req.BrandID)
				if perr != nil {
					return perr
				}
				brandID = &parsed
			}
			c, err := bonus.CreateCampaign(ctx, tx, bonus.Campaign{
				TenantID: tc.TenantID, BrandID: brandID, FulfillmentOwner: req.FulfillmentOwner,
				CreatedByActorType: bonus.ActorStaff, CreatedByActorID: staffID,
			})
			if err != nil {
				return err
			}
			resp = toCampaignResponse(c)
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorStaff, ActorID: staffID,
				Action: "bonus_campaign.created", TargetType: "bonus_campaign", TargetID: c.ID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
		})
		if err != nil {
			deps.Logger.Error("create_campaign_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to create campaign")
			return
		}
		writeJSON(w, http.StatusCreated, resp)
	}
}

// --- staff-facing: manual grant issuance (doc 10 N2.4a) ---

type issueManualGrantRequest struct {
	PlayerAccountID   string `json:"player_account_id"`
	CampaignID        string `json:"campaign_id"`
	CampaignVersionID string `json:"campaign_version_id"`
	OfferID           string `json:"offer_id"`
	OfferVersionID    string `json:"offer_version_id"`
	AssetCode         string `json:"asset_code"`
	Amount            string `json:"amount"` // decimal-string minor units, never float (CLAUDE.md)
	FundingSource     string `json:"funding_source"`
	ParentOperationID string `json:"parent_operation_id"` // doc 10 N2.4a: MUST already be a resolvable, approved EOI
	ReasonCode        string `json:"reason_code"`
	// JurisdictionCode is DELETED (Stage 4I JV-2, docs/governance/stage-4i-
	// canonical-model.md §8): a jurisdiction is a RESOLVED FACT about an
	// operation, never a value a staff member may type into a request body
	// (JV-1). httpserver's decodeJSON already calls
	// dec.DisallowUnknownFields() (json.go), so a client that still sends
	// "jurisdiction_code" now gets a clean 400, not a silently-ignored
	// field. bonus.IssueSingleManualGrant resolves the operation's
	// jurisdiction itself, server-side, via internal/jurisdiction.Resolve,
	// from the Grant's own tenant/brand/player - see
	// resolveGrantJurisdiction's own doc comment
	// (internal/bonus/eligibility.go) for the disclosed Stage 4I
	// consequence (every player-scoped resolution is unresolved(no_signal)
	// today, so this denies at the AssetAuthorization gate - the correct,
	// fail-closed, auditable outcome, never worked around here).
}

// newIssueManualGrantHandler is doc 10 N2.4a's single-Grant staff-action
// surface, gated by PermBonusGrantIssue and, per doc 10 N2.4a, requiring
// an already-resolvable parent_operation_id - this handler does not mint
// a fresh EOI root itself (that is a separate "request a manual grant
// authorization" administrative flow this dispatch does not build a
// dedicated endpoint for this Phase; a caller mints one via
// internal/bonus.MintRootOperation server-side, e.g. from a staff
// four-eyes approval flow this Phase's HTTP surface does not yet expose)
// - it is named here explicitly as a curated-but-incomplete surface,
// not silently presented as the full authoring UI.
func newIssueManualGrantHandler(deps Deps) http.HandlerFunc {
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
			{"amount", req.Amount}, {"funding_source", req.FundingSource}, {"parent_operation_id", req.ParentOperationID},
			{"reason_code", req.ReasonCode},
		} {
			v.RequireNonEmpty(f.name, f.val)
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		amount, ok := new(big.Int).SetString(req.Amount, 10)
		if !ok || amount.Sign() < 0 {
			apierror.Write(w, requestID, apierror.CodeValidation, "amount must be a non-negative decimal integer string")
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
			grant, outcome, err := bonus.IssueSingleManualGrant(ctx, tx, g, parentOperationID, staffID, amount)
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
				Action: "bonus_grant.manual_issue", TargetType: "bonus_grant", TargetID: grant.ID.String(), Outcome: audit.OutcomeSuccess,
				IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
				Metadata: map[string]any{"reason_code": req.ReasonCode, "parent_operation_id": req.ParentOperationID},
			})
		})
		if errors.Is(err, economicop.ErrParentOperationNotFound) || errors.Is(err, economicop.ErrParentNotApproved) || errors.Is(err, economicop.ErrParentNotOpenOrExpired) || errors.Is(err, economicop.ErrChildScopeExceedsParent) || errors.Is(err, economicop.ErrBudgetExhausted) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "economic operation authorization check failed: "+err.Error())
			return
		}
		if err != nil {
			deps.Logger.Error("issue_manual_grant_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to issue manual grant")
			return
		}
		if denied {
			apierror.Write(w, requestID, apierror.CodeForbidden, "grant denied: "+denialReason)
			return
		}
		writeJSON(w, http.StatusCreated, result)
	}
}

// --- staff-facing: suggestion review ---

type decideSuggestionRequest struct {
	Decision        string  `json:"decision"` // "approved" | "rejected" | "edited"
	Modifications   *string `json:"modifications,omitempty"`
	RejectionReason *string `json:"rejection_reason,omitempty"`
}

func newClaimSuggestionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		reviewerID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		suggestionID, err := uuid.Parse(r.PathValue("suggestionID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid suggestion id")
			return
		}
		var resp Suggestion
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			s, err := bonus.ClaimSuggestionForReview(ctx, tx, tc.TenantID, suggestionID, reviewerID)
			resp = toSuggestionResponse(s)
			return err
		})
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "suggestion not found")
			return
		}
		if errors.Is(err, bonus.ErrIllegalSuggestionTransition) {
			apierror.Write(w, requestID, apierror.CodeConflict, err.Error())
			return
		}
		if err != nil {
			deps.Logger.Error("claim_suggestion_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to claim suggestion")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type Suggestion struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

func toSuggestionResponse(s bonus.Suggestion) Suggestion {
	return Suggestion{ID: s.ID.String(), Status: string(s.Status)}
}

func newDecideSuggestionHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		reviewerID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid staff subject")
			return
		}
		suggestionID, err := uuid.Parse(r.PathValue("suggestionID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid suggestion id")
			return
		}
		var req decideSuggestionRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("decision", req.Decision, "approved", "rejected", "edited")
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var resp Suggestion
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			s, err := bonus.DecideSuggestion(ctx, tx, tc.TenantID, suggestionID, req.Decision, reviewerID, req.Modifications, req.RejectionReason)
			resp = toSuggestionResponse(s)
			return err
		})
		if errors.Is(err, bonus.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "suggestion not found")
			return
		}
		if err != nil {
			deps.Logger.Error("decide_suggestion_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to decide suggestion: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- staff-facing: held-disposition resolution (REQ-SEP-BONUS-4) ---

type resolveHeldDispositionRequest struct {
	Action     string `json:"action"` // "reforfeit" | "route_to_cash"
	ReasonCode string `json:"reason_code"`
	RequestID  string `json:"request_id"` // an already-approved bonus_change_requests id (four-eyes, doc 34 §3.1)
	// RequiredApprovals is DELIBERATELY NOT a field here (SEC-4I-F1 fix,
	// security's Stage 4I Phase 4 design review finding): a client-supplied
	// approvals count, unclamped, previously flowed straight into
	// bonus_change_consume_approved_request, letting a caller lower a
	// tenant's configured N-approver four-eyes threshold to as little as 1
	// for ACTION_ROUTE_TO_CASH (real money leaving player_bonus_held).
	// Every other four-eyes-consuming wrapper in this codebase resolves its
	// required-approvals count SERVER-SIDE, from bonus_approval_policies,
	// via resolveRequiredApprovals (internal/bonus/four_eyes_ops.go) - this
	// endpoint now does the same, inside bonus.ResolveHeldDispositionAction
	// itself, never from request input. Because httpserver's JSON decoding
	// already calls dec.DisallowUnknownFields() (json.go), a client that
	// still sends "required_approvals" in the body now gets a clean 400,
	// not a silently-ignored field.
	//
	// JurisdictionCode is DELETED (Stage 4I JV-2 - see
	// issueManualGrantRequest's own identical doc comment above): a
	// jurisdiction is a resolved fact about an operation, never a staff-
	// typed request field. "route_to_cash"'s own T.1 gate now resolves it
	// server-side via bonus.ResolveHeldDispositionAction (see
	// resolveGrantJurisdiction's doc comment, internal/bonus/
	// eligibility.go). "reforfeit" never ran this gate and is unaffected.
}

func newResolveHeldDispositionHandler(deps Deps) http.HandlerFunc {
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
		dispositionID, err := uuid.Parse(r.PathValue("dispositionID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid disposition id")
			return
		}
		var req resolveHeldDispositionRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireOneOf("action", req.Action, "reforfeit", "route_to_cash")
		v.RequireNonEmpty("reason_code", req.ReasonCode)
		v.RequireNonEmpty("request_id", req.RequestID)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		changeRequestID, err := uuid.Parse(req.RequestID)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "request_id must be a valid UUID")
			return
		}
		var resp struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		}
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			resolved, err := bonus.ResolveHeldDispositionAction(ctx, tx, tc.TenantID, bonus.ResolveHeldDispositionActionParams{
				HeldDispositionID: dispositionID, Action: bonus.HeldDispositionAction(req.Action), ActorID: staffID,
				ReasonCode: req.ReasonCode, RequestID: changeRequestID,
			})
			if err != nil {
				return err
			}
			resp.ID, resp.Status = resolved.ID.String(), string(resolved.Status)
			return nil
		})
		if errors.Is(err, bonus.ErrHeldDispositionNotHeld) {
			apierror.Write(w, requestID, apierror.CodeConflict, "held disposition is not in 'held' status")
			return
		}
		if errors.Is(err, bonus.ErrChangeRequestNotApproved) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "four-eyes approval required and not satisfied")
			return
		}
		if errors.Is(err, bonus.ErrHeldDispositionActionDenied) {
			apierror.Write(w, requestID, apierror.CodeForbidden, "route-to-cash denied: "+err.Error())
			return
		}
		if err != nil {
			deps.Logger.Error("resolve_held_disposition_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to resolve held disposition")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// --- shared helpers ---

type playerAccountInfo struct {
	brandID *uuid.UUID
}

func loadPlayerAccountAndWallet(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, assetCode string) (playerAccountInfo, uuid.UUID, error) {
	var info playerAccountInfo
	var brandID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT brand_id FROM player_accounts WHERE id = $1 AND tenant_id = $2`, playerAccountID, tenantID).Scan(&brandID); err != nil {
		return playerAccountInfo{}, uuid.Nil, bonus.ErrNotFound
	}
	info.brandID = &brandID
	var walletID uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM wallets WHERE tenant_id = $1 AND player_account_id = $2 AND asset_code = $3`,
		tenantID, playerAccountID, assetCode).Scan(&walletID); err != nil {
		return playerAccountInfo{}, uuid.Nil, bonus.ErrNotFound
	}
	return info, walletID, nil
}

func lookupDecimalExponent(ctx context.Context, tx pgx.Tx, assetCode string) (int32, error) {
	var exp int32
	if err := tx.QueryRow(ctx, `SELECT decimal_exponent FROM assets WHERE code = $1`, assetCode).Scan(&exp); err != nil {
		return 0, bonus.ErrNotFound
	}
	return exp, nil
}

// computeCouponRewardAmount reads the OfferVersion's own already-computed
// reward configuration for a FIXED-VALUE coupon reward (R2, doc 10 §W3) -
// a percentage-with-cap (R1) coupon would need a qualifying amount this
// endpoint does not have (a coupon has no deposit/loss to compute a
// percentage against); wiring R1-shaped coupons is left to a future
// surface, and this function fails closed rather than guessing.
func computeCouponRewardAmount(ov bonus.OfferVersion) (*big.Int, error) {
	if ov.RewardKind != bonus.RewardFixedValue {
		return nil, errors.New("bonus: this endpoint supports fixed-value (R2) coupon rewards only")
	}
	var payload struct {
		Amount string `json:"amount"`
	}
	if err := json.Unmarshal(ov.RewardCalculation, &payload); err != nil || payload.Amount == "" {
		return nil, errors.New("bonus: offer version's reward_calculation does not carry a fixed amount")
	}
	amount, ok := new(big.Int).SetString(payload.Amount, 10)
	if !ok {
		return nil, errors.New("bonus: offer version's reward_calculation amount is not a valid integer")
	}
	return amount, nil
}
