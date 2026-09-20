package httpserver

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/assetregistry"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// --- Casino round history / Back Office visibility (Stage 7 §15/§16) ---

// roundResponse is one casino.RoundSummary rendered for the player's own
// history view - amount fields are omitted (not zero) when that leg of
// the round never happened, mirroring betResponse's own minimal shape.
type roundResponse struct {
	SessionID            string `json:"session_id"`
	GameID               string `json:"game_id"`
	ProviderID           string `json:"provider_id"`
	ProviderGameID       string `json:"provider_game_id"`
	AssetCode            string `json:"asset_code"`
	Mode                 string `json:"mode"`
	Status               string `json:"status"`
	LaunchedAt           string `json:"launched_at"`
	BetAmount            *int64 `json:"bet_amount,omitempty"`
	WinAmount            *int64 `json:"win_amount,omitempty"`
	RollbackAmount       *int64 `json:"rollback_amount,omitempty"`
	BetProviderTxID      string `json:"bet_provider_tx_id,omitempty"`
	WinProviderTxID      string `json:"win_provider_tx_id,omitempty"`
	RollbackProviderTxID string `json:"rollback_provider_tx_id,omitempty"`
}

func toRoundResponse(r casino.RoundSummary) roundResponse {
	return roundResponse{
		SessionID: r.SessionID.String(), GameID: r.GameID.String(), ProviderID: r.ProviderID,
		ProviderGameID: r.ProviderGameID, AssetCode: r.AssetCode, Mode: string(r.Mode), Status: string(r.Status),
		LaunchedAt: r.LaunchedAt.UTC().Format(rfc3339), BetAmount: r.BetAmount, WinAmount: r.WinAmount,
		RollbackAmount: r.RollbackAmount, BetProviderTxID: r.BetProviderTxID, WinProviderTxID: r.WinProviderTxID,
		RollbackProviderTxID: r.RollbackProviderTxID,
	}
}

// newListMyCasinoRoundsHandler is the player's own paginated casino round
// history (Stage 7 §16). Runs under db.Pool.WithTenant, NOT
// WithPlayerScope - see casino.ListRoundsForPlayer's own doc comment:
// ledger_transactions has no player-scope RLS policy at all, so this
// handler's explicit playerAccountID == tc.Subject binding (never a
// client-supplied player id) is what authorizes the read, mirroring
// newLaunchCasinoGameHandler's identical WithTenant-not-WithPlayerScope
// rationale - not the WithPlayerScope/pagination shape
// newListMyBetsHandler uses (sportsbook_bets, unlike casino rounds, is
// fully self-contained and does carry a player_self_scope policy).
func newListMyCasinoRoundsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.CasinoOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "casino is not enabled on this deployment")
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

		var items []roundResponse
		var total int
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			rounds, count, err := casino.ListRoundsForPlayer(ctx, tx, playerAccountID, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = count
			items = make([]roundResponse, 0, len(rounds))
			for _, rnd := range rounds {
				items = append(items, toRoundResponse(rnd))
			}
			return nil
		})
		if err != nil {
			logger.Error("list_my_casino_rounds_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list casino history")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}

// adminRoundResponse adds staff-only fields to roundResponse - mirrors
// adminBetResponse's identical shape (embedding plus PlayerAccountID/
// BrandID/DecimalExponent), the Back Office minimum-visibility set Stage 7
// §15 asks for (player/brand/game/provider/wager/win/rollback/amount/
// asset/status/timestamps/provider-reference/audit-linkage).
type adminRoundResponse struct {
	roundResponse
	PlayerAccountID      string   `json:"player_account_id"`
	BrandID              string   `json:"brand_id"`
	DecimalExponent      int16    `json:"decimal_exponent"`
	LedgerTransactionIDs []string `json:"ledger_transaction_ids"`
	// ProviderRoundID surfaces the casino_provider_rounds binding (Stage 8,
	// docs/decisions/0080-provider-integration-readiness-without-external-
	// contracts.md Decision 5) for this round's own launch session, when
	// one exists. Empty string ("", never omitted) when no provider has
	// ever bound a round id to this session - the common case today, since
	// no real provider posts callbacks and only Stage 8's own tests
	// populate casino_provider_rounds.
	ProviderRoundID string `json:"provider_round_id"`
}

// newListAdminCasinoRoundsHandler is the Back Office's tenant-wide,
// minimum-visibility casino round queue (Stage 7 §15) - deliberately NOT a
// full casino operations console (no filter/search/export beyond
// pagination). Mirrors newListAdminBetsHandler's identical per-page
// asset-decimal-exponent caching.
func newListAdminCasinoRoundsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.CasinoOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "casino is not enabled on this deployment")
			return
		}

		tc, err := tenant.FromContext(r.Context())
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "no authenticated context")
			return
		}
		p := parsePageParams(r)

		var items []adminRoundResponse
		var total int
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			rounds, count, err := casino.ListRoundsForTenant(ctx, tx, p.Limit, p.Offset)
			if err != nil {
				return err
			}
			total = count

			// One batched casino_provider_rounds lookup for the whole page,
			// not one query per row - mirrors the exponents map's identical
			// per-page-not-per-row caching discipline immediately below.
			sessionIDs := make([]uuid.UUID, 0, len(rounds))
			for _, rnd := range rounds {
				sessionIDs = append(sessionIDs, rnd.SessionID)
			}
			providerRoundIDs, err := casino.LookupProviderRoundIDsBySession(ctx, tx, tc.TenantID, sessionIDs)
			if err != nil {
				return fmt.Errorf("casino admin rounds: look up provider round bindings: %w", err)
			}

			items = make([]adminRoundResponse, 0, len(rounds))
			exponents := make(map[string]int16)
			for _, rnd := range rounds {
				exp, ok := exponents[rnd.AssetCode]
				if !ok {
					a, err := assetregistry.GetAsset(ctx, tx, rnd.AssetCode)
					if err != nil {
						return fmt.Errorf("casino admin rounds: look up asset %q: %w", rnd.AssetCode, err)
					}
					exp = a.DecimalExponent
					exponents[rnd.AssetCode] = exp
				}
				txIDs := make([]string, 0, len(rnd.LedgerTransactionIDs))
				for _, id := range rnd.LedgerTransactionIDs {
					txIDs = append(txIDs, id.String())
				}
				items = append(items, adminRoundResponse{
					roundResponse: toRoundResponse(rnd), PlayerAccountID: rnd.PlayerAccountID.String(),
					BrandID: rnd.BrandID.String(), DecimalExponent: exp, LedgerTransactionIDs: txIDs,
					ProviderRoundID: providerRoundIDs[rnd.SessionID],
				})
			}
			return nil
		})
		if err != nil {
			logger.Error("list_admin_casino_rounds_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list casino rounds")
			return
		}
		writeJSON(w, http.StatusOK, newPagedResponse(items, p, total))
	}
}
