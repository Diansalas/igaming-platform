package httpserver

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// --- Catalogue browse (public, no auth required - mirrors how a casino
// game catalogue read endpoint would behave if it had no player-specific
// availability join; sportsbook's catalogue this stage has no tenant/
// brand opt-in layer at all - see internal/sportsbook's own package doc
// comment for why that is a deliberate, disclosed scope simplification
// versus internal/casino's two-layer catalogue/availability model) ---

type eventSummaryResponse struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	StartTime string `json:"start_time"`
	Status    string `json:"status"`
	// Available/UnavailableReason (Stage 9.2, ADR 0083 §5.4.1) - Available
	// is always present (never omitted), UnavailableReason only when
	// Available is false. An unannotated/anonymous read leaves Available
	// true and UnavailableReason empty for every event, mirroring
	// sportsbook.EventSummary's own defaults exactly.
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

func toEventSummaryResponse(e sportsbook.EventSummary) eventSummaryResponse {
	return eventSummaryResponse{
		ID: e.ID.String(), Name: e.Name, StartTime: e.StartTime.UTC().Format(rfc3339), Status: string(e.Status),
		Available: e.Available, UnavailableReason: e.UnavailableReason,
	}
}

type competitionCatalogueResponse struct {
	ID     string                 `json:"id"`
	Name   string                 `json:"name"`
	Events []eventSummaryResponse `json:"events"`
}

type sportCatalogueResponse struct {
	ID           string                         `json:"id"`
	Code         string                         `json:"code"`
	Name         string                         `json:"name"`
	Competitions []competitionCatalogueResponse `json:"competitions"`
}

func toSportCatalogueResponse(s sportsbook.SportCatalogue) sportCatalogueResponse {
	resp := sportCatalogueResponse{ID: s.ID.String(), Code: s.Code, Name: s.Name, Competitions: []competitionCatalogueResponse{}}
	for _, c := range s.Competitions {
		cc := competitionCatalogueResponse{ID: c.ID.String(), Name: c.Name, Events: []eventSummaryResponse{}}
		for _, e := range c.Events {
			cc.Events = append(cc.Events, toEventSummaryResponse(e))
		}
		resp.Competitions = append(resp.Competitions, cc)
	}
	return resp
}

// newListSportsHandler returns the full browse tree (sports ->
// competitions -> event summaries). Platform-wide, read-open catalogue
// data - no authentication, no tenant scoping (mirrors the assets
// registry/casino_games precedent: this data carries no RLS).
//
// Stage 9.2 (ADR 0083 §5.4.1): this route is GENUINELY anonymous today -
// no auth middleware is attached to it (sportsbook_routes.go), so there is
// no server-authenticated player/brand identity to build a populated
// AvailabilityContext from. The zero value is passed explicitly (never
// omitted) so the intent is on the record: AnnotateCatalogueAvailability
// is a no-op for it (IsAnonymous()), which is what keeps this endpoint's
// behaviour unchanged. If an authenticated variant of this route is ever
// added, it derives a populated AvailabilityContext the same way
// newPlaceBetHandler derives its own player identity - server-side only,
// never from a client-supplied field - and passes it here instead.
func newListSportsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

		var resp []sportCatalogueResponse
		err := deps.DB.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			sports, err := sportsbook.ListSportsCatalogue(ctx, tx)
			if err != nil {
				return err
			}
			if err := sportsbook.AnnotateCatalogueAvailability(ctx, tx, sports, sportsbook.AvailabilityContext{}); err != nil {
				return err
			}
			resp = make([]sportCatalogueResponse, 0, len(sports))
			for _, s := range sports {
				resp = append(resp, toSportCatalogueResponse(s))
			}
			return nil
		})
		if err != nil {
			logger.Error("list_sports_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list sports catalogue")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type selectionResponse struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	OddsNumerator   int64  `json:"odds_numerator"`
	OddsDenominator int64  `json:"odds_denominator"`
	Status          string `json:"status"`
	// Available/UnavailableReason - see eventSummaryResponse's identical
	// doc comment.
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

func toSelectionResponse(s sportsbook.Selection) selectionResponse {
	return selectionResponse{
		ID: s.ID.String(), Name: s.Name, OddsNumerator: s.OddsNumerator, OddsDenominator: s.OddsDenominator, Status: string(s.Status),
		Available: s.Available, UnavailableReason: s.UnavailableReason,
	}
}

type marketDetailResponse struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Status     string              `json:"status"`
	Selections []selectionResponse `json:"selections"`
	// Available/UnavailableReason - see eventSummaryResponse's identical
	// doc comment.
	Available         bool   `json:"available"`
	UnavailableReason string `json:"unavailable_reason,omitempty"`
}

type eventDetailResponse struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	StartTime       string                 `json:"start_time"`
	Status          string                 `json:"status"`
	SportCode       string                 `json:"sport_code"`
	SportName       string                 `json:"sport_name"`
	CompetitionName string                 `json:"competition_name"`
	Markets         []marketDetailResponse `json:"markets"`
}

func toEventDetailResponse(d sportsbook.EventDetail) eventDetailResponse {
	resp := eventDetailResponse{
		ID: d.ID.String(), Name: d.Name, StartTime: d.StartTime.UTC().Format(rfc3339), Status: string(d.Status),
		SportCode: d.SportCode, SportName: d.SportName, CompetitionName: d.CompetitionName, Markets: []marketDetailResponse{},
	}
	for _, m := range d.Markets {
		md := marketDetailResponse{
			ID: m.ID.String(), Name: m.Name, Status: string(m.Status), Selections: []selectionResponse{},
			Available: m.Available, UnavailableReason: m.UnavailableReason,
		}
		for _, sel := range m.Selections {
			md.Selections = append(md.Selections, toSelectionResponse(sel))
		}
		resp.Markets = append(resp.Markets, md)
	}
	return resp
}

// newGetEventHandler returns one event's full market/selection tree -
// public, no authentication, mirroring newListSportsHandler (see that
// handler's own Stage 9.2 doc comment for why the zero-value
// AvailabilityContext is passed explicitly here too).
func newGetEventHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

		eventID, err := uuid.Parse(r.PathValue("id"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid event id")
			return
		}

		var detail sportsbook.EventDetail
		err = deps.DB.WithoutTenant(r.Context(), func(ctx context.Context, tx pgx.Tx) error {
			var err error
			detail, err = sportsbook.GetEventDetail(ctx, tx, eventID)
			if err != nil {
				return err
			}
			return sportsbook.AnnotateEventAvailability(ctx, tx, &detail, sportsbook.AvailabilityContext{})
		})
		if errors.Is(err, sportsbook.ErrEventNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "event not found")
			return
		}
		if err != nil {
			logger.Error("get_event_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load event")
			return
		}
		writeJSON(w, http.StatusOK, toEventDetailResponse(detail))
	}
}

// --- Bet placement / history (player-authenticated) ---

// betResponse is shared by the player self-service and Back Office bet
// history endpoints. Outcome/PayoutAmount/SettledAt (ADR 0088 §3.4, Stage
// 10 W1) are read-only additions derived from the bet's own settlement
// history, never written by any request this file handles:
//
//   - Outcome ("won"/"lost", nil when open) and PayoutAmount (nil when
//     open or lost) come from the CURRENT un-reversed settlement row, if
//     any - the same "current" concept internal/sportsbook.CurrentSettlement
//     uses, re-derived here from a plain history slice.
//   - SettledAt is the created_at of the history row that set the bet's
//     CURRENT non-open status: the current settlement row for
//     settled_won/settled_lost, the void row for void; nil when open
//     (including a bet that was settled and then rolled back - it is
//     open again, so SettledAt reverts to nil, matching the bet's own
//     current status exactly).
//
// This struct carries NO staff/request identifiers (ActorStaffAccountID,
// RequestID) - see toBetResponse's own doc comment for why that must never
// change, and TestListMyBets_PlayerSurfaceOmitsStaffAndRequestFields (ADR
// 0088 §14 S8) for the pinning test.
type betResponse struct {
	ID              string  `json:"id"`
	SelectionID     string  `json:"selection_id"`
	AssetCode       string  `json:"asset_code"`
	StakeAmount     int64   `json:"stake_amount"`
	OddsNumerator   int64   `json:"odds_numerator"`
	OddsDenominator int64   `json:"odds_denominator"`
	PotentialReturn int64   `json:"potential_return"`
	Status          string  `json:"status"`
	PlacedAt        string  `json:"placed_at"`
	Outcome         *string `json:"outcome"`
	PayoutAmount    *int64  `json:"payout_amount"`
	SettledAt       *string `json:"settled_at"`
}

// toBetResponse builds the base response with no settlement-history
// annotation (Outcome/PayoutAmount/SettledAt all nil) - callers that have
// batch-loaded a page's history call applySettlementHistory afterward.
// Deliberately never given an implicit "look up history itself" path: that
// would silently reintroduce the N+1 query pattern ADR 0088 §3.4 requires
// batch-loading to avoid.
func toBetResponse(b sportsbook.Bet) betResponse {
	return betResponse{
		ID: b.ID.String(), SelectionID: b.SelectionID.String(), AssetCode: b.AssetCode, StakeAmount: b.StakeAmount,
		OddsNumerator: b.OddsNumerator, OddsDenominator: b.OddsDenominator, PotentialReturn: b.PotentialReturn,
		Status: string(b.Status), PlacedAt: b.PlacedAt.UTC().Format(rfc3339),
	}
}

// applySettlementHistory fills in Outcome/PayoutAmount/SettledAt (ADR 0088
// §3.4) from a bet's own settlement history entries (already batch-loaded
// by the caller via internal/sportsbook.ListSettlementRecordsForBets - a
// missing/empty slice for a still-open bet is the normal, expected case,
// not an error).
func applySettlementHistory(resp *betResponse, history []sportsbook.SettlementHistoryEntry) {
	if current := sportsbook.CurrentSettlement(history); current != nil {
		outcome := ""
		if current.Outcome != nil {
			outcome = *current.Outcome
		}
		resp.Outcome = &outcome
		payout := int64(0)
		if current.PayoutAmount != nil {
			payout = *current.PayoutAmount
		}
		resp.PayoutAmount = &payout
		settledAt := current.CreatedAt.UTC().Format(rfc3339)
		resp.SettledAt = &settledAt
		return
	}
	if void := sportsbook.VoidEntry(history); void != nil {
		settledAt := void.CreatedAt.UTC().Format(rfc3339)
		resp.SettledAt = &settledAt
	}
}

type placeBetRequest struct {
	SelectionID             string `json:"selection_id"`
	StakeAmount             int64  `json:"stake_amount"`
	AssetCode               string `json:"asset_code"`
	ExpectedOddsNumerator   int64  `json:"expected_odds_numerator"`
	ExpectedOddsDenominator int64  `json:"expected_odds_denominator"`
	IdempotencyKey          string `json:"idempotency_key"`
}

// placeBetResponse is POST /v1/me/sportsbook/bets' response envelope for
// BOTH outcomes (accepted and rejected) - Accepted is the discriminator a
// client branches on; a rejection is a well-formed business decision, not
// an HTTP error (mirrors internal/casino's newCasinoWebhookHandler's own
// "declined is 200 OK with a decline_reason field" precedent, applied
// here to a player-facing endpoint), so RejectionCategory/RejectionCode/
// RejectionMessage are always populated together and Bet is always nil
// when Accepted is false.
type placeBetResponse struct {
	Accepted          bool         `json:"accepted"`
	Bet               *betResponse `json:"bet,omitempty"`
	RejectionCategory string       `json:"rejection_category,omitempty"`
	RejectionCode     string       `json:"rejection_code,omitempty"`
	RejectionMessage  string       `json:"rejection_message,omitempty"`
}

// newPlaceBetHandler resolves the player's own identity/wallet server-side
// and drives sportsbook.PlaceBet. Mirrors newLaunchCasinoGameHandler's
// WithTenant-not-WithPlayerScope rationale exactly: sportsbook_bets'
// tenant_staff_scope policy (migration 0078) requires app.player_account_id
// be unset for any write, so this handler's own explicit
// playerAccountID == tc.Subject binding (never a client-supplied player id)
// is what authorizes the write, not row-level security on this path.
func newPlaceBetHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}

		var req placeBetRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("selection_id", req.SelectionID)
		v.RequireNonEmpty("asset_code", req.AssetCode)
		v.RequireNonEmpty("idempotency_key", req.IdempotencyKey)
		if req.StakeAmount <= 0 {
			v.Add("stake_amount", "must be a positive integer (minor units)")
		}
		if req.ExpectedOddsNumerator <= 0 || req.ExpectedOddsDenominator <= 0 {
			v.Add("expected_odds_numerator", "expected_odds_numerator/expected_odds_denominator must both be positive")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}
		selectionID, err := uuid.Parse(req.SelectionID)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid selection id")
			return
		}

		var result sportsbook.PlaceBetResult
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			wl, err := wallet.GetOrCreate(ctx, tx, tc.TenantID, account.BrandID, playerAccountID, req.AssetCode)
			if err != nil {
				return err
			}
			result, err = sportsbook.PlaceBet(ctx, tx, sportsbook.PlaceBetParams{
				TenantID: tc.TenantID, BrandID: account.BrandID, PlayerAccountID: playerAccountID, WalletID: wl.ID,
				SelectionID: selectionID, AssetCode: req.AssetCode, StakeAmount: req.StakeAmount,
				ExpectedOddsNumerator: req.ExpectedOddsNumerator, ExpectedOddsDenominator: req.ExpectedOddsDenominator,
				IdempotencyKey: req.IdempotencyKey,
			})
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if errors.Is(err, sportsbook.ErrSelectionNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "selection not found")
			return
		}
		if errors.Is(err, sportsbook.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if errors.Is(err, sportsbook.ErrBetIdempotencyKeyReused) {
			apierror.Write(w, requestID, apierror.CodeConflict, "idempotency key already used with different bet parameters")
			return
		}
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "unknown asset code")
			return
		}
		if err != nil {
			logger.Error("place_bet_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to place bet")
			return
		}

		if !result.Accepted {
			// Stage 9 §16 observability gap closure: an RG/self-exclusion or
			// Risk & Limits policy block was, until now, visible ONLY in the
			// audit_log table (sportsbook.PlaceBet's own audit.Record calls
			// for RejectionRGDenied/RejectionRiskDenied) - nothing reached
			// the structured request log, so no log-based alert rule could
			// ever page on a spike in either without polling the audit
			// table. Every OTHER decline reason here (odds changed, event/
			// market/selection no longer open, insufficient funds) is an
			// ordinary, expected commercial outcome, not a control firing,
			// so this deliberately does NOT log those - logging every
			// decline would bury the two categories that actually matter
			// for compliance/fraud alerting in routine noise. One event
			// name for both, distinguished by the policy field (mirrors
			// this file's own "one rejection response shape, distinguished
			// by RejectionCategory" convention) rather than two near-
			// duplicate event names.
			if result.RejectionCategory == sportsbook.RejectionRGDenied || result.RejectionCategory == sportsbook.RejectionRiskDenied || result.RejectionCategory == sportsbook.RejectionKYCDenied {
				policy := "rg"
				switch result.RejectionCategory {
				case sportsbook.RejectionRiskDenied:
					policy = "risk"
				case sportsbook.RejectionKYCDenied:
					policy = "kyc"
				}
				logger.Warn("sportsbook_bet_policy_blocked", "policy", policy, "reason_code", result.RejectionCode)
			}
			writeJSON(w, http.StatusOK, toPlaceBetRejectionResponse(result))
			return
		}
		betResp := toBetResponse(result.Bet)
		writeJSON(w, http.StatusCreated, placeBetResponse{Accepted: true, Bet: &betResp})
	}
}

// toPlaceBetRejectionResponse maps a rejected sportsbook.PlaceBetResult to
// its player-facing response. Factored out of newPlaceBetHandler so the
// K3-6 collapse below is independently unit-testable, mirroring
// writeCasinoLaunchDenial's identical structure/rationale
// (internal/httpserver/casino_handlers.go) exactly, applied here to
// sportsbook.RejectionJurisdictionDenied (ADR 0083 Part C, §7.1 step 7):
// sportsbook.DenialCodeJurisdictionUnresolved and
// sportsbook.DenialCodeJurisdictionBlocked MUST produce a BYTE-IDENTICAL
// player-facing rejection_code/rejection_message - same values, every
// time - even though they stay fully distinguishable everywhere internal
// to this point (PlaceBetResult.RejectionCode itself, PlaceBet's own
// sportsbook_bet.denied_by_jurisdiction_policy audit record). The
// distinction between "your jurisdiction could not be determined" and
// "you are blocked in your jurisdiction" is exactly the signal that would
// tell an attacker whether a manipulation attempt registered - collapsed
// here, in this ONE place, before any other rejection category (every
// other category DOES intentionally disclose its own RejectionCode - a
// player declined for insufficient funds or a stale price is legitimately
// owed that specific reason, unlike a jurisdiction determination).
//
// SEC-S92-6 fix round (security review of ADR 0083 Part B2): a second,
// distinct collapse for sportsbook.RejectionExposureLimit. INV-SB-EXP-2
// (no amount/threshold/scope/limit-id ever reaches a player-facing
// payload) already held before this fix and is untouched here - the gap
// this closes is different: RejectionCategory being the DISTINCT,
// never-otherwise-used literal "exposure_limit" is itself an oracle. A
// client that binary-searches stake amounts against one selection can
// read the exact point the response category flips to "exposure_limit"
// and reconstruct the book's remaining open capacity under a configured
// ceiling, purely from WHICH category comes back - no number is ever
// literally returned, but the category name alone leaks the fact that
// exposure (as opposed to any other reason) is why this particular stake
// was declined.
//
// Fix: an exposure-limit decline is reported to the player in the exact
// same shape sportsbook.RejectionRiskDenied already uses for a genuine
// risk.CodeLimitBreach decline (internal/risk's own real, non-exposure
// "a configured limit was breached" outcome) - same RejectionCategory,
// same RejectionCode, same RejectionMessage, byte-for-byte identical to
// what orchestrator.go's own risk-denial branch produces when
// riskDecision.Code == risk.CodeLimitBreach ("this bet was declined by
// platform risk policy: " + code). This is deliberately NOT a brand-new,
// exposure-only opaque value (unlike jurisdiction's "jurisdiction_
// unavailable", which only has to hide which of two internal jurisdiction
// sub-reasons applied, not that a jurisdiction gate fired at all): a new
// exposure-only literal would just relocate the oracle instead of closing
// it. Reusing a REAL, already-possible, stake-and-limit-shaped risk
// outcome means a client cannot tell "the book's cross-player exposure
// ceiling was breached" apart from "my own applicable risk/limit policy
// declined this bet" - both are stake-amount-sensitive, non-player-
// verifiable (unlike insufficient_funds, which a player can independently
// falsify against their own wallet balance read), plausible causes for
// the identical response. The internal sportsbook.RejectionExposureLimit
// category, PlaceBetResult.RejectionCode/RejectionMessage (already empty/
// generic per INV-SB-EXP-2) and the sportsbook_bet.denied_by_exposure_
// policy audit record are completely unchanged by this - only this one
// player-facing mapping function changes.
func toPlaceBetRejectionResponse(result sportsbook.PlaceBetResult) placeBetResponse {
	if result.RejectionCategory == sportsbook.RejectionJurisdictionDenied {
		return placeBetResponse{
			Accepted: false, RejectionCategory: sportsbook.RejectionJurisdictionDenied,
			RejectionCode: "jurisdiction_unavailable", RejectionMessage: "this bet is not available in your jurisdiction",
		}
	}
	if result.RejectionCategory == sportsbook.RejectionExposureLimit {
		return placeBetResponse{
			Accepted: false, RejectionCategory: sportsbook.RejectionRiskDenied,
			RejectionCode:    risk.CodeLimitBreach,
			RejectionMessage: "this bet was declined by platform risk policy: " + risk.CodeLimitBreach,
		}
	}
	// F7 (security review rv-prh-i3-security.md): players see status
	// only, never the internal kyc.EnforcementDecision.Code
	// ("kyc_sportsbook_play:pending" etc. - matched_trigger/policy_version
	// are never even reached this call site, but the raw outcome string
	// still is, unless collapsed here) and never a distinct "unavailable"
	// signal - a transient evaluator failure is a generic, retryable
	// message, exactly like the withdrawal handler's own 503 mapping
	// (B1). "verification_required" is this route's own closed-enum
	// code - a player-facing UI can render one static "please verify
	// your identity" flow from it, never anything jurisdiction/threshold-
	// specific.
	if result.RejectionCategory == sportsbook.RejectionKYCDenied {
		return placeBetResponse{
			Accepted: false, RejectionCategory: sportsbook.RejectionKYCDenied,
			RejectionCode: "verification_required", RejectionMessage: "this bet requires identity verification",
		}
	}
	return placeBetResponse{
		Accepted: false, RejectionCategory: result.RejectionCategory,
		RejectionCode: result.RejectionCode, RejectionMessage: result.RejectionMessage,
	}
}

// newListMyBetsHandler is the player's own paginated bet history.
func newListMyBetsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		playerAccountID, err := uuid.Parse(tc.Subject)
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "invalid player identity")
			return
		}
		p := parsePageParams(r)

		var items []betResponse
		var total int
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			bets, count, err := sportsbook.ListBetsForPlayer(ctx, tx, playerAccountID, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = count
			// ADR 0088 §3.4: one batch query for this page's settlement
			// history, never one query per bet.
			betIDs := make([]uuid.UUID, len(bets))
			for i, b := range bets {
				betIDs[i] = b.ID
			}
			history, err := sportsbook.ListSettlementRecordsForBets(ctx, tx, betIDs)
			if err != nil {
				return err
			}
			items = make([]betResponse, 0, len(bets))
			for _, b := range bets {
				resp := toBetResponse(b)
				applySettlementHistory(&resp, history[b.ID])
				items = append(items, resp)
			}
			return nil
		})
		if err != nil {
			logger.Error("list_my_bets_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list bets")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}

// --- Back Office (staff, tenant-wide, read-only) ---

type adminBetResponse struct {
	betResponse
	PlayerAccountID string `json:"player_account_id"`
	BrandID         string `json:"brand_id"`
	// DecimalExponent lets the Back Office render stake/potential-return as
	// a real decimal amount instead of raw minor units, mirroring the
	// Stage 5 admin withdrawal fix (staffWithdrawalResponse.DecimalExponent)
	// for the same reason: a staff-facing money display without it
	// misrepresents the real amount.
	DecimalExponent int16 `json:"decimal_exponent"`
	// ProviderID/ProviderBetReference pass through sportsbook.Bet's identical,
	// always-nil-today pointer fields (Stage 8, docs/decisions/0080-
	// provider-integration-readiness-without-external-contracts.md
	// Decision 5/Decision 2) as empty strings ("", never omitted) when nil
	// - no real sportsbook provider adapter exists yet, so every bet
	// placed via the existing PlaceBet flow has both NULL.
	ProviderID           string `json:"provider_id"`
	ProviderBetReference string `json:"provider_bet_reference"`
	// CorrelationID (ADR 0088 §3.4) is the bet id itself - every W1 ledger
	// posting for this bet carries it as ledger_transactions.correlation_id
	// (ADR 0088 §2.1/§2.2), so surfacing it lets Back Office staff pivot
	// straight from a bet to its ledger transactions without a separate
	// lookup. Deliberately just b.ID.String() again under a different,
	// ledger-shaped name - not a new fact, a named alias for an existing
	// one.
	CorrelationID string `json:"correlation_id"`
	// Lifecycle (ADR 0088 §3.4) is this bet's full settlement history in
	// insertion order - staff-only (never on betResponse/the player
	// surface). Deliberately the exact field set §3.4 names (id,
	// event_kind, generation, outcome, payout_amount, void_reason,
	// ledger_transaction_id, created_at) - NEITHER this nor betResponse
	// ever surfaces actor_staff_account_id/request_id (the underlying
	// sportsbook.SettlementHistoryEntry/SettlementRecord carry both, but
	// settlementLifecycleEntryResponse below deliberately omits them), so
	// TestListMyBets_PlayerSurfaceOmitsStaffAndRequestFields (§14 S8)
	// pins their absence from the player response specifically.
	Lifecycle []settlementLifecycleEntryResponse `json:"lifecycle"`
}

// settlementLifecycleEntryResponse is one sportsbook_bet_settlements row,
// staff-only (ADR 0088 §3.4).
type settlementLifecycleEntryResponse struct {
	ID                  string  `json:"id"`
	EventKind           string  `json:"event_kind"`
	Generation          *int    `json:"generation"`
	Outcome             *string `json:"outcome"`
	PayoutAmount        *int64  `json:"payout_amount"`
	VoidReason          *string `json:"void_reason"`
	LedgerTransactionID string  `json:"ledger_transaction_id"`
	CreatedAt           string  `json:"created_at"`
}

func toSettlementLifecycleEntryResponse(e sportsbook.SettlementHistoryEntry) settlementLifecycleEntryResponse {
	return settlementLifecycleEntryResponse{
		ID: e.ID.String(), EventKind: e.EventKind, Generation: e.Generation, Outcome: e.Outcome,
		PayoutAmount: e.PayoutAmount, VoidReason: e.VoidReason,
		LedgerTransactionID: e.LedgerTransactionID.String(), CreatedAt: e.CreatedAt.UTC().Format(rfc3339),
	}
}

// stringOrEmpty dereferences an optional *string, returning "" for nil -
// the same nil-tolerant flattening convention this response layer already
// uses for every other nullable-in-the-database field it surfaces.
func stringOrEmpty(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// newListAdminBetsHandler is the Stage 6 Back Office sportsbook bet
// visibility view: tenant-wide (not player-restricted), paginated per the
// exact shared Stage 5 pageParams/pagedResponse convention, mirroring
// newListAdminWithdrawalsHandler's identical shape (same pagination
// envelope, same tenant-scoping-not-player-scoping, same "pure read, no
// side effect" discipline).
func newListAdminBetsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if !deps.SportsbookEnabled {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "sportsbook is not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		p := parsePageParams(r)

		var items []adminBetResponse
		var total int
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			bets, count, err := sportsbook.ListBetsForTenant(ctx, tx, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = count
			// ADR 0088 §3.4: one batch query for this page's settlement
			// history, never one query per bet.
			betIDs := make([]uuid.UUID, len(bets))
			for i, b := range bets {
				betIDs[i] = b.ID
			}
			history, err := sportsbook.ListSettlementRecordsForBets(ctx, tx, betIDs)
			if err != nil {
				return err
			}
			items = make([]adminBetResponse, 0, len(bets))
			// One asset-registry lookup per distinct asset code in this page,
			// not per row - mirrors newListAdminWithdrawalsHandler's identical
			// precedent.
			exponents := make(map[string]int16)
			for _, b := range bets {
				exp, ok := exponents[b.AssetCode]
				if !ok {
					a, err := assetregistry.GetAsset(ctx, tx, b.AssetCode)
					if err != nil {
						return fmt.Errorf("sportsbook admin list: look up asset %q: %w", b.AssetCode, err)
					}
					exp = a.DecimalExponent
					exponents[b.AssetCode] = exp
				}
				base := toBetResponse(b)
				betHistory := history[b.ID]
				applySettlementHistory(&base, betHistory)
				lifecycle := make([]settlementLifecycleEntryResponse, 0, len(betHistory))
				for _, e := range betHistory {
					lifecycle = append(lifecycle, toSettlementLifecycleEntryResponse(e))
				}
				items = append(items, adminBetResponse{
					betResponse: base, PlayerAccountID: b.PlayerAccountID.String(), BrandID: b.BrandID.String(), DecimalExponent: exp,
					ProviderID: stringOrEmpty(b.ProviderID), ProviderBetReference: stringOrEmpty(b.ProviderBetReference),
					CorrelationID: b.ID.String(), Lifecycle: lifecycle,
				})
			}
			return nil
		})
		if err != nil {
			logger.Error("list_admin_bets_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list bets")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}
