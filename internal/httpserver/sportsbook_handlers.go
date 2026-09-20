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
}

func toEventSummaryResponse(e sportsbook.EventSummary) eventSummaryResponse {
	return eventSummaryResponse{ID: e.ID.String(), Name: e.Name, StartTime: e.StartTime.UTC().Format(rfc3339), Status: string(e.Status)}
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
}

func toSelectionResponse(s sportsbook.Selection) selectionResponse {
	return selectionResponse{ID: s.ID.String(), Name: s.Name, OddsNumerator: s.OddsNumerator, OddsDenominator: s.OddsDenominator, Status: string(s.Status)}
}

type marketDetailResponse struct {
	ID         string              `json:"id"`
	Name       string              `json:"name"`
	Status     string              `json:"status"`
	Selections []selectionResponse `json:"selections"`
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
		md := marketDetailResponse{ID: m.ID.String(), Name: m.Name, Status: string(m.Status), Selections: []selectionResponse{}}
		for _, sel := range m.Selections {
			md.Selections = append(md.Selections, toSelectionResponse(sel))
		}
		resp.Markets = append(resp.Markets, md)
	}
	return resp
}

// newGetEventHandler returns one event's full market/selection tree -
// public, no authentication, mirroring newListSportsHandler.
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
			return err
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

type betResponse struct {
	ID              string `json:"id"`
	SelectionID     string `json:"selection_id"`
	AssetCode       string `json:"asset_code"`
	StakeAmount     int64  `json:"stake_amount"`
	OddsNumerator   int64  `json:"odds_numerator"`
	OddsDenominator int64  `json:"odds_denominator"`
	PotentialReturn int64  `json:"potential_return"`
	Status          string `json:"status"`
	PlacedAt        string `json:"placed_at"`
}

func toBetResponse(b sportsbook.Bet) betResponse {
	return betResponse{
		ID: b.ID.String(), SelectionID: b.SelectionID.String(), AssetCode: b.AssetCode, StakeAmount: b.StakeAmount,
		OddsNumerator: b.OddsNumerator, OddsDenominator: b.OddsDenominator, PotentialReturn: b.PotentialReturn,
		Status: string(b.Status), PlacedAt: b.PlacedAt.UTC().Format(rfc3339),
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
			writeJSON(w, http.StatusOK, placeBetResponse{
				Accepted: false, RejectionCategory: result.RejectionCategory,
				RejectionCode: result.RejectionCode, RejectionMessage: result.RejectionMessage,
			})
			return
		}
		betResp := toBetResponse(result.Bet)
		writeJSON(w, http.StatusCreated, placeBetResponse{Accepted: true, Bet: &betResp})
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
			items = make([]betResponse, 0, len(bets))
			for _, b := range bets {
				items = append(items, toBetResponse(b))
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
				items = append(items, adminBetResponse{
					betResponse: toBetResponse(b), PlayerAccountID: b.PlayerAccountID.String(), BrandID: b.BrandID.String(), DecimalExponent: exp,
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
