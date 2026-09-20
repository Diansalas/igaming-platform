package httpserver

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

// maxCasinoWebhookBodyBytes mirrors maxWebhookBodyBytes's identical
// rationale in deposit_handlers.go - a casino provider callback has no
// session/auth to rate-limit by.
const maxCasinoWebhookBodyBytes = 1 << 20 // 1 MiB

type casinoGameResponse struct {
	ID              string   `json:"id"`
	ProviderID      string   `json:"provider_id"`
	Name            string   `json:"name"`
	GameType        string   `json:"game_type"`
	RTPVariant      string   `json:"rtp_variant,omitempty"`
	Volatility      string   `json:"volatility,omitempty"`
	FeatureFlags    []string `json:"feature_flags"`
	SupportedAssets []string `json:"supported_assets"`
	MobileSupported bool     `json:"mobile_supported"`
	DemoSupported   bool     `json:"demo_supported"`
}

func toCasinoGameResponse(g casino.Game) casinoGameResponse {
	return casinoGameResponse{
		ID: g.ID.String(), ProviderID: g.ProviderID, Name: g.Name, GameType: g.GameType,
		RTPVariant: g.RTPVariant, Volatility: g.Volatility, FeatureFlags: g.FeatureFlags,
		SupportedAssets: g.SupportedAssets, MobileSupported: g.MobileSupported, DemoSupported: g.DemoSupported,
	}
}

// newListCasinoGamesHandler returns the player's own tenant/brand's
// resolved game catalogue (ADR 0025 §2 - platform catalogue joined against
// the tenant's own opt-in layer). Runs under db.Pool.WithTenant, NOT
// WithPlayerScope: casino_game_availability (migration 0035) carries only
// a tenant_isolation policy, no player_self_scope SELECT policy - the
// player's own view is this server-side join, never a table the player
// reads directly, mirroring newInitiateDepositHandler's identical
// WithTenant-not-WithPlayerScope rationale in deposit_handlers.go.
func newListCasinoGamesHandler(deps Deps) http.HandlerFunc {
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

		var resp []casinoGameResponse
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			games, err := casino.ListAvailableGames(ctx, tx, tc.TenantID, account.BrandID)
			if err != nil {
				return err
			}
			resp = make([]casinoGameResponse, 0, len(games))
			for _, g := range games {
				resp = append(resp, toCasinoGameResponse(g))
			}
			return nil
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if err != nil {
			logger.Error("list_casino_games_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to list games")
			return
		}
		if resp == nil {
			resp = []casinoGameResponse{}
		}
		writeJSON(w, http.StatusOK, resp)
	}
}

type launchCasinoGameRequest struct {
	AssetCode string `json:"asset_code"`
	Mode      string `json:"mode"`
}

type launchCasinoGameResponse struct {
	LaunchURL string `json:"launch_url"`
	SessionID string `json:"session_id"`
	ExpiresAt string `json:"expires_at"`
}

// newLaunchCasinoGameHandler resolves the player's own identity/wallet
// server-side and drives Orchestrator.LaunchGame. Per ADR 0025 §3/§6, the
// player's platform JWT is NEVER passed to the provider - only the
// short-lived, single-use launch token LaunchGame itself mints. GameID
// names the PLATFORM's own game id (from GET /v1/me/casino/games), never a
// client-supplied provider_game_id.
//
// Runs under db.Pool.WithTenant, NOT WithPlayerScope - casino_launch_sessions'
// tenant_staff_scope policy (migration 0035) requires app.player_account_id
// be unset for any write, exactly like withdrawal_requests; the player's
// own authorization is this handler's explicit playerAccountID == tc.Subject
// check, not row-level security on this write path (the same split
// deposit_handlers.go already uses).
func newLaunchCasinoGameHandler(deps Deps) http.HandlerFunc {
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
		gameID, err := uuid.Parse(r.PathValue("gameID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid game id")
			return
		}

		var req launchCasinoGameRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("asset_code", req.AssetCode)
		v.RequireOneOf("mode", req.Mode, "real", "demo")
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var result casino.LaunchGameResult
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			account, err := identity.GetPlayerAccountByID(ctx, tx, playerAccountID)
			if err != nil {
				return err
			}
			wl, err := wallet.GetOrCreate(ctx, tx, tc.TenantID, account.BrandID, playerAccountID, req.AssetCode)
			if err != nil {
				return err
			}
			result, err = deps.CasinoOrchestrator.LaunchGame(ctx, tx, casino.LaunchGameParams{
				TenantID: tc.TenantID, BrandID: account.BrandID, PlayerAccountID: playerAccountID, WalletID: wl.ID,
				GameID: gameID, AssetCode: req.AssetCode, Mode: casino.GameMode(req.Mode),
			})
			return err
		})
		if errors.Is(err, identity.ErrNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "player account not found")
			return
		}
		if db.IsForeignKeyViolation(err) {
			apierror.Write(w, requestID, apierror.CodeValidation, "unknown asset code")
			return
		}
		if errors.Is(err, casino.ErrGameNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "game not found")
			return
		}
		if errors.Is(err, casino.ErrGameDisabled) || errors.Is(err, casino.ErrGameNotAvailable) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "game not available")
			return
		}
		if errors.Is(err, casino.ErrProviderUnavailable) {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "game provider is temporarily unavailable")
			return
		}
		if errors.Is(err, casino.ErrUnknownProvider) {
			logger.Error("launch_casino_game_unknown_provider", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to launch game")
			return
		}
		if errors.Is(err, casino.ErrInvalidInput) {
			apierror.Write(w, requestID, apierror.CodeValidation, err.Error())
			return
		}
		if err != nil {
			logger.Error("launch_casino_game_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to launch game")
			return
		}
		if result.Denied {
			writeCasinoLaunchDenial(w, requestID, result)
			return
		}

		writeJSON(w, http.StatusCreated, launchCasinoGameResponse{
			LaunchURL: result.LaunchURL, SessionID: result.SessionID.String(), ExpiresAt: result.ExpiresAt.Format(rfc3339),
		})
	}
}

// writeCasinoLaunchDenial maps a casino.LaunchGameResult.Denied outcome to
// its player-facing HTTP response. Factored out of
// newLaunchCasinoGameHandler so the K3-6 collapse below is independently
// unit-testable (casino_handlers_test.go) via httptest.ResponseRecorder,
// asserting byte-identical output for the two jurisdiction denial codes
// without needing a full LaunchGame/database round trip (K3-6 requires
// "denies on a genuinely blocked jurisdiction" to be UNREACHABLE via HTTP
// in Stage 4I - no player-side jurisdiction resolution exists yet - so
// this is the only way to test the collapse against both codes directly).
//
// K3-6 / canonical-model §6.3 (the HTTP-boundary oracle rule, generalized
// platform-wide by security/architect beyond casino):
// DenialCodeJurisdictionUnresolved and DenialCodeJurisdictionBlocked MUST
// produce a BYTE-IDENTICAL player-facing response - same status code,
// same message, same body - even though they are kept fully
// distinguishable everywhere internal to this point
// (LaunchGameResult.DenialCode itself, casino.evaluateAndAuditRisk's/
// evaluateAndAuditEligibility's audit records, operator-facing logs). The
// distinction between "your jurisdiction could not be determined" and
// "you are blocked in your jurisdiction" is EXACTLY the signal that would
// tell an attacker whether a manipulation attempt (a VPN, a proxy, a
// changed declared residence) registered - handled here, in this ONE
// place, deliberately BEFORE the generic RG/Risk denial branch (which
// DOES intentionally disclose result.DenialCode - a player denied by
// their own account status is legitimately owed that specific reason,
// unlike a jurisdiction determination).
func writeCasinoLaunchDenial(w http.ResponseWriter, requestID string, result casino.LaunchGameResult) {
	if result.DenialCode == casino.DenialCodeJurisdictionUnresolved || result.DenialCode == casino.DenialCodeJurisdictionBlocked {
		apierror.Write(w, requestID, apierror.CodeForbidden, "game is not available in your jurisdiction")
		return
	}
	// Stage 4D-RG: told to the player directly (never a generic
	// "forbidden") - a player denied by their own account status or
	// self-exclusion is legitimately owed that specific reason, the same
	// way a real-world RG self-exclusion page always names itself rather
	// than presenting a bare access-denied screen.
	apierror.Write(w, requestID, apierror.CodeForbidden, "gambling is currently restricted for this account: "+result.DenialCode)
}

// newCasinoWebhookHandler receives a provider callback (bet/win/rollback)
// and dispatches it via Orchestrator.ReceiveCallback. No bearer-token
// middleware - a provider webhook is not an authenticated platform
// principal, exactly mirroring newPaymentWebhookHandler's identical
// rationale: tenant resolution is this handler's own responsibility, via
// the URL's tenant slug, never any field inside rawPayload; payload
// signature verification happens inside the named adapter's own
// HandleCallback, before any payload field is used (ADR 0025 §5/§7).
func newCasinoWebhookHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.CasinoOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "casino webhooks are not enabled on this deployment")
			return
		}

		tenantSlug := r.PathValue("tenantSlug")
		providerID := r.PathValue("providerID")
		if tenantSlug == "" || providerID == "" {
			apierror.Write(w, requestID, apierror.CodeValidation, "tenant slug and provider id are required")
			return
		}

		t, err := identity.GetTenantBySlug(r.Context(), deps.DB, tenantSlug)
		if errors.Is(err, identity.ErrNotFound) {
			// Same enumeration-resistance rationale as
			// newPaymentWebhookHandler: an unrecognized slug and a
			// suspended tenant get the identical not-found response.
			apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
			return
		}
		if err != nil {
			logger.Error("casino_webhook_tenant_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
			return
		}
		if t.Status != "active" {
			apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
			return
		}

		body, err := io.ReadAll(io.LimitReader(r.Body, maxCasinoWebhookBodyBytes+1))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "failed to read request body")
			return
		}
		if len(body) > maxCasinoWebhookBodyBytes {
			apierror.Write(w, requestID, apierror.CodeValidation, "request body too large")
			return
		}

		var result casino.ReceiveCallbackResult
		err = deps.DB.WithTenant(r.Context(), t.ID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			result, err = deps.CasinoOrchestrator.ReceiveCallback(ctx, tx, t.ID, providerID, body)
			return err
		})
		if errors.Is(err, casino.ErrCallbackSignatureInvalid) {
			// A 4xx, not a 500 - an unsigned/mis-signed callback is a
			// caller/authentication error, not a platform failure (ADR
			// 0025 §12: "provider callback forgery").
			logger.Error("casino_webhook_signature_invalid", "provider_id", providerID, "tenant_id", t.ID.String())
			apierror.Write(w, requestID, apierror.CodeValidation, "callback rejected")
			return
		}
		if errors.Is(err, casino.ErrUnknownProvider) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "not found")
			return
		}
		if errors.Is(err, casino.ErrProviderUnavailable) {
			// The tenant's own CasinoProviderCapability is disabled (or
			// never configured) - a kill switch, not a platform failure.
			apierror.Write(w, requestID, apierror.CodeUnavailable, "provider is not enabled for this tenant")
			return
		}
		if errors.Is(err, casino.ErrBetNotFound) {
			// A win/rollback naming a round with no matching prior bet is
			// an integrity alert - a provider protocol violation, not a
			// routine failure (financial-transaction-flows.md §6) -
			// logged at elevated severity, never silently 200'd.
			logger.Error("casino_webhook_integrity_alert_bet_not_found", "error", err, "provider_id", providerID, "tenant_id", t.ID.String())
			apierror.Write(w, requestID, apierror.CodeValidation, "no matching prior bet for this round")
			return
		}
		if errors.Is(err, casino.ErrProviderRoundOwnershipConflict) {
			// A provider_round_id already bound to a DIFFERENT player/brand
			// is a protocol violation exactly like ErrBetNotFound above -
			// logged at elevated severity, never silently retried as a
			// routine 500 (a bare 500 would tell a well-behaved-looking
			// caller retrying makes sense, when it never will). A 409, not a
			// 5xx: this is a caller-supplied-data conflict, not a platform
			// failure (Stage 8 review finding - P1). The response message is
			// as generic as ErrBetNotFound's own - it never echoes the round
			// id or any player identity, which would let a well-behaved-
			// looking caller enumerate which round ids are already claimed.
			logger.Error("casino_webhook_integrity_alert_provider_round_ownership_conflict", "error", err, "provider_id", providerID, "tenant_id", t.ID.String())
			apierror.Write(w, requestID, apierror.CodeConflict, "callback rejected")
			return
		}
		if errors.Is(err, casino.ErrAlreadyRolledBack) {
			apierror.Write(w, requestID, apierror.CodeConflict, "original transaction already rolled back")
			return
		}
		if errors.Is(err, casino.ErrLaunchSessionRequired) {
			logger.Error("casino_webhook_missing_session_binding", "provider_id", providerID, "tenant_id", t.ID.String())
			apierror.Write(w, requestID, apierror.CodeValidation, "callback rejected")
			return
		}
		if errors.Is(err, casino.ErrOutcomeNotSucceeded) {
			apierror.Write(w, requestID, apierror.CodeValidation, "callback outcome is not succeeded")
			return
		}
		if errors.Is(err, casino.ErrInvalidInput) {
			// Never echo err.Error() to an unauthenticated caller - it may
			// include submitted field values (security specialist review
			// finding). A fixed, generic message only.
			apierror.Write(w, requestID, apierror.CodeValidation, "callback rejected: invalid input")
			return
		}
		if err != nil {
			logger.Error("casino_webhook_failed", "error", err, "provider_id", providerID)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to process callback")
			return
		}

		resp := map[string]any{"outcome": string(result.Outcome), "tombstoned": result.Tombstoned}
		if result.LedgerTransactionID != nil {
			resp["ledger_transaction_id"] = result.LedgerTransactionID.String()
		}
		if result.DeclineReason != "" {
			resp["decline_reason"] = result.DeclineReason
		}
		writeJSON(w, http.StatusOK, resp)
	}
}
