package httpserver

import (
	"context"
	"errors"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// walletSummaryResponse is the player-facing balance view
// (internal/wallet.Summary). Amounts are minor units, matching every
// other financial API in this codebase - never floating point,
// converting to a display amount is a client/UI concern.
type walletSummaryResponse struct {
	WalletID          string `json:"wallet_id"`
	AssetCode         string `json:"asset_code"`
	Status            string `json:"status"`
	CashBalance       int64  `json:"cash_balance"`
	AvailableBalance  int64  `json:"available_balance"`
	HeldForWithdrawal int64  `json:"held_for_withdrawal"`
	LockedBalance     int64  `json:"locked_balance"`
	BonusBalance      int64  `json:"bonus_balance"`
}

func toWalletSummaryResponse(s wallet.Summary) walletSummaryResponse {
	return walletSummaryResponse{
		WalletID: s.Wallet.ID.String(), AssetCode: s.Wallet.AssetCode, Status: string(s.Wallet.Status),
		CashBalance: s.CashBalance, AvailableBalance: s.AvailableBalance,
		HeldForWithdrawal: s.HeldForWithdrawal, LockedBalance: s.LockedBalance, BonusBalance: s.BonusBalance,
	}
}

// newListWalletsHandler returns every wallet the authenticated player
// holds, with its balance summary. Reads run under db.Pool.WithPlayerScope
// so RLS's player_self_scope policy (migrations 0019/0023) is the actual
// isolation mechanism - never solely the WHERE player_account_id = $1
// clause internal/wallet.List already applies (CLAUDE.md: "do not repeat
// the Stage 2 sessions RLS mistake" - defense in depth, not either/or).
func newListWalletsHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

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

		var resp []walletSummaryResponse
		err = deps.DB.WithPlayerScope(r.Context(), tc.TenantID, playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			wallets, err := wallet.List(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			resp = make([]walletSummaryResponse, 0, len(wallets))
			for _, wl := range wallets {
				summary, err := wallet.GetSummary(ctx, tx, wl)
				if err != nil {
					return err
				}
				resp = append(resp, toWalletSummaryResponse(summary))
			}
			return nil
		})
		if err != nil {
			logger.Error("list_wallets_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list wallets")
			return
		}
		if resp == nil {
			resp = []walletSummaryResponse{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

// newGetWalletHandler returns the balance summary for one of the
// player's own wallets, creating it on first access to a new asset
// (mirroring GET-creates-on-first-use for a resource scoped entirely to
// the caller, same as e.g. a per-user settings document) - resolving the
// player's own brand_id via their PlayerAccount, never from client input.
//
// Runs under db.Pool.WithTenant, NOT WithPlayerScope: wallets' RLS
// (migration 0019) deliberately makes player_self_scope SELECT-only -
// only tenant_staff_scope (which requires the player GUC to be UNSET) may
// INSERT - exactly mirroring how the posting engine and every package in
// internal/ledger/internal/withdrawal/internal/payments always write
// under WithTenant, even for a player-initiated action. Isolation here
// comes from playerAccountID being server-derived from the JWT and
// threaded explicitly into wallet.GetOrCreate, never from RLS filtering a
// client-controlled query - the same pattern Stage 2 already uses for
// player_accounts.
func newGetWalletHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

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
		assetCode := r.PathValue("assetCode")
		if assetCode == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "asset code is required")
			return
		}

		var resp walletSummaryResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			wl, err := wallet.GetOrCreate(ctx, tx, tc.TenantID, account.BrandID, playerAccountID, assetCode)
			if err != nil {
				return err
			}
			summary, err := wallet.GetSummary(ctx, tx, wl)
			if err != nil {
				return err
			}
			resp = toWalletSummaryResponse(summary)
			return nil
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "unknown asset code")
			return
		}
		if err != nil {
			logger.Error("get_wallet_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to load wallet")
			return
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
