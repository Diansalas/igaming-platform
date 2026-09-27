package httpserver

import (
	"context"
	"errors"
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
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// maxCasinoWebhookBodyBytes mirrors maxWebhookBodyBytes's identical
// rationale in deposit_handlers.go - a casino provider callback has no
// session/auth to rate-limit by.
const maxCasinoWebhookBodyBytes = 1 << 20 // 1 MiB

// casinoWebhookRoute is the casino domain's parameter set for the shared
// webhook preamble (webhook_preamble.go). Stage 10.2, CAS-WH-TENANT-1, ADR
// 0091, design §C6.
var casinoWebhookRoute = webhookRoute{
	schemeFor: func(deps Deps, providerID string) (webhookauth.VerificationScheme, bool) {
		if deps.CasinoOrchestrator == nil {
			return nil, false
		}
		return deps.CasinoOrchestrator.WebhookScheme(providerID)
	},
	maxBody:                 maxCasinoWebhookBodyBytes,
	domain:                  domainCasino,
	authFailedEvent:         "casino_webhook_auth_failed",
	tenantLookupFailedEvent: "casino_webhook_tenant_lookup_failed",
}

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
			// Stage 9 §16 observability gap closure: an RG/self-exclusion or
			// Risk & Limits policy block was, until now, visible ONLY in the
			// audit_log table (evaluateAndAuditEligibility/evaluateAndAuditRisk's
			// own audit.Record calls) - despite this function's own sibling
			// doc comment on writeCasinoLaunchDenial already claiming
			// "operator-facing logs" as one of the places this distinction
			// is kept, which was not actually true until this line. Excludes
			// the two jurisdiction denial codes deliberately: K3-6 (see
			// writeCasinoLaunchDenial below) requires those two to stay
			// byte-identical at the HTTP boundary specifically so a
			// response-timing/shape difference can't become the oracle;
			// logging them under their own real, distinguishable codes here
			// would be harmless to a PLAYER (this log is operator-only,
			// never returned in any response), but jurisdiction availability
			// is a catalogue/geo-blocking concern, not the RG/Risk policy
			// category this event exists to make alertable - see
			// docs/runbooks/observability-and-alerting.md.
			if result.DenialCode != casino.DenialCodeJurisdictionUnresolved && result.DenialCode != casino.DenialCodeJurisdictionBlocked {
				logger.Warn("casino_launch_policy_blocked", "reason_code", result.DenialCode)
			}
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
// rationale.
//
// Stage 10.2 (CAS-WH-TENANT-1, ADR 0091, design §C6): tenant binding is
// per docs/decisions/0022 §3 as amended - the tenant slug in the URL is
// only a LOOKUP HINT, selecting one candidate credential, which must then
// verify a signature whose input includes the route-resolved tenant_id/
// provider_id, never any field inside the body. Every pre-verification
// failure (unknown tenant, inactive tenant, bad provider_id, unregistered
// provider, no resolver, no credential, bad signature) gets the IDENTICAL
// 401 "callback rejected" response via the shared webhookPreamble, so an
// unauthenticated caller can never enumerate which of those is true. The
// route STAYS REGISTERED even with test support off/in production - the
// resolver is then nil, so every callback fails closed with 401
// (reason no_resolver); no real aggregator exists yet (design §C7).
func newCasinoWebhookHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.CasinoOrchestrator == nil {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "casino webhooks are not enabled on this deployment")
			return
		}

		// ADR 0097 A2/A3/A4a: the FIRST thing any webhook route does - no
		// DB, no body read (ORD-1/ORD-2).
		releaseInflight, admitted := deps.webhookAdmission.admitPreAuth(w, r, domainCasino, deps.TrustedProxyCount, func(id string) bool {
			if deps.CasinoOrchestrator == nil {
				return false
			}
			_, ok := deps.CasinoOrchestrator.WebhookScheme(id)
			return ok
		})
		if !admitted {
			return
		}
		defer releaseInflight()

		// Steps 1-5 (provider_id charset, bounded body read, header format
		// - all before any tenant/DB work - then the platform-wide tenant
		// lookup and active check) are the shared webhook preamble every
		// webhook domain uses (Stage 10.2, ADR 0091, architect R2 / ruling
		// J4). Every rejection there is the IDENTICAL 401
		// "callback rejected" with one allow-listed
		// casino_webhook_auth_failed line. ADR 0097 A4b gates the
		// platform-wide tenant lookup this now performs.
		t, providerID, body, ok := webhookPreamble(w, r, deps, casinoWebhookRoute)
		if !ok {
			return
		}

		// ADR 0094 §4.1: phase 1 (verification) holds NO transaction - its
		// handle read runs in a short READ ONLY transaction that commits
		// before any secret-store fetch; the domain transaction opens only
		// after it succeeded, and re-checks the verified handle first. ADR
		// 0097 §5.3: gatedReader gates VerifyCallback's own
		// WithTenantReadOnly calls under the same A4b bulkhead.
		var reader webhookauth.TenantReader = deps.DB
		if deps.webhookAdmission != nil {
			tenantKey, providerKey := deps.webhookAdmission.preAuthKeys(t.Slug, providerID, func(id string) bool {
				_, ok := deps.CasinoOrchestrator.WebhookScheme(id)
				return ok
			})
			reader = deps.webhookAdmission.newGatedReader(deps.DB, domainCasino, tenantKey, providerKey)
		}
		var result casino.ReceiveCallbackResult
		verified, err := deps.CasinoOrchestrator.VerifyCallback(r.Context(), reader, t.ID, providerID, webhookauth.Inbound{Header: r.Header, Body: body})
		if errors.Is(err, errDBGateUnavailable) {
			apierror.Write(w, requestID, apierror.CodeUnavailable, "service temporarily unavailable; retry later")
			return
		}
		// ADR 0097 B1/B2 (ORD-3/ORD-4): admitted ONLY off the just-verified
		// (tenant_id, provider_id), strictly before deps.DB.WithTenant. The
		// B2 release is held (ledger-finance C1) through
		// recordCasinoCallbackRejection below, released exactly once at the
		// end of this handler - so no 429/503 can ever follow a commit.
		var releaseDomainTx func()
		if err == nil {
			var admittedVerified bool
			releaseDomainTx, admittedVerified = deps.webhookAdmission.admitVerified(w, r, domainCasino, t.ID, providerID, func(providerID string) (allows, declared bool) {
				sem, ok := deps.CasinoOrchestrator.WebhookRetrySemantics(providerID)
				if !ok {
					return true, false
				}
				return sem.Retries429, true
			})
			if !admittedVerified {
				return
			}
			defer func() {
				if releaseDomainTx != nil {
					releaseDomainTx()
				}
			}()
		}
		if err == nil {
			err = deps.DB.WithTenant(r.Context(), t.ID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				result, err = deps.CasinoOrchestrator.ReceiveVerifiedCallback(ctx, tx, t.ID, providerID, verified)
				return err
			})
		}

		// Stage 10.3 W2b (CAS-RECON-1): a verified-but-rejected callback
		// gets its durable rejection record here, in a separately
		// committed transaction (the callback's own one has rolled back).
		// A no-op for every error that is not a *casino.CallbackRejectedError,
		// including every pre-verification AuthError (I1). Never changes
		// the response below.
		recordCasinoCallbackRejection(r.Context(), deps, logger, t.ID, requestID, err)

		var authErr *webhookauth.AuthError
		if errors.As(err, &authErr) {
			logWebhookAuthFailure(logger, casinoWebhookRoute.authFailedEvent, r, requestID, authErr.Reason, &t.ID, providerID, true, authErr.KeyID, authErr.CredentialFingerprint, len(body))
			apierror.Write(w, requestID, apierror.CodeUnauthorized, "callback rejected")
			return
		}
		if errors.Is(err, casino.ErrCallbackMalformedBody) {
			// A VERIFIED callback (the sender proved knowledge of the
			// shared credential) whose body is structurally malformed -
			// a real 4xx, not the uniform pre-verification 401 (design
			// §C2 point 2).
			logger.Warn("casino_webhook_malformed_body_after_verification", "provider_id", providerID, "tenant_id", t.ID.String(), "request_id", requestID)
			apierror.Write(w, requestID, apierror.CodeValidation, "callback rejected")
			return
		}
		if errors.Is(err, casino.ErrProviderReferenceInvalid) {
			// PROVIDER-REF-BOUND-1: a VERIFIED callback whose provider
			// reference breaks the platform bound (internal/providerref).
			// Deterministic and non-retryable: the same 400 class as a
			// malformed verified body; nothing was read or written. No
			// casino_callback_rejections row (the value cannot be stored in
			// its bounded columns) - this log line is the evidence. It
			// carries field, reason, byte length and a hash prefix only,
			// never the value (no log amplification).
			logProviderReferenceRejected(logger, "casino_webhook_provider_reference_rejected", err, providerID, t.ID.String(), requestID)
			apierror.Write(w, requestID, apierror.CodeValidation, "callback rejected")
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
		if errors.Is(err, casino.ErrProviderTxPayloadMismatch) {
			// Stage 10 F-7 remediation: a provider_tx_id already posted,
			// redelivered with a different payload (amount, round, session,
			// or - for a rollback - a different original). Before F-7 this
			// was silently answered with the ORIGINAL result as success.
			// Nothing was posted; an integrity alert (provider protocol
			// violation or compromised signing key), and a 409 with a
			// generic body that never echoes the reference or amounts.
			logger.Error("casino_webhook_integrity_alert_payload_mismatch", "error", err, "provider_id", providerID, "tenant_id", t.ID.String())
			apierror.Write(w, requestID, apierror.CodeConflict, "callback rejected")
			return
		}
		if errors.Is(err, casino.ErrAlreadyRolledBack) {
			apierror.Write(w, requestID, apierror.CodeConflict, "original transaction already rolled back")
			return
		}
		if errors.Is(err, casino.ErrOriginalTombstoned) {
			// Stage 10.3 CAS-CAP-ROLLBACK-1, E10: a win naming a
			// provider_tx_id a tombstone already covers (its own rollback
			// was accepted before it was ever posted). Deterministic,
			// never retryable, nothing posted - a 409 with a generic body,
			// never echoing the reference.
			logger.Error("casino_webhook_integrity_alert_original_tombstoned", "error", err, "provider_id", providerID, "tenant_id", t.ID.String())
			apierror.Write(w, requestID, apierror.CodeConflict, "callback rejected")
			return
		}
		if errors.Is(err, casino.ErrAmbiguousMultiOriginRound) || errors.Is(err, casino.ErrCorrelationWalletCollision) ||
			errors.Is(err, casino.ErrLockAlreadyReleased) || errors.Is(err, casino.ErrMixedFundingUnsupported) ||
			errors.Is(err, casino.ErrBonusBetNotLocked) {
			// Stage 10.3 G-1 (docs/plans/stage-10.3-planning/
			// 02-casino-financial-analysis.md §3): these §16.4 abort
			// outcomes are integrity alerts (a provider protocol violation
			// or a platform posting-layer defect), never a routine
			// failure - previously fell through to the generic 500
			// branch below, which invites endless provider retries for a
			// condition a retry can never resolve. A 409 with a generic
			// body, never echoing the round id or any player identity.
			// G-1 itself (a genuine multi-cash-bet round) no longer
			// reaches here at all - resolveWinOrigin resolves it to the
			// shared wallet instead (see classifyDirectOriginRows); this
			// branch is now reached only for a GENUINELY ambiguous
			// bonus/mixed-origin round or a wallet collision.
			logger.Error("casino_webhook_integrity_alert_win_origin", "error", err, "provider_id", providerID, "tenant_id", t.ID.String())
			apierror.Write(w, requestID, apierror.CodeConflict, "callback rejected")
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

		// R1 (ledger-finance re-verification after fix round A, gate
		// 10.3-W1): a structured, allow-listed Info line marking every
		// replay short-circuit (postBet's E2 idempotency short-circuit, the
		// E9 tombstone replay, the postWin/postRollback AlreadyPosted gate,
		// and postRollbackHeldWin's voided-by-same-reference short-circuit -
		// see casino.ReceiveCallbackResult.Replayed's own doc comment for
		// the full list). Deliberately carries only request_id, tenant_id,
		// provider_id and event_type - never provider_tx_id or any other
		// caller-supplied value, matching this handler's existing "never
		// echo caller-supplied identifiers" discipline for every other log
		// line above. Lets a first delivery be told apart from a
		// redelivery without a database join; it is not itself a durable
		// record (W2b's callback/rejection record is the durable answer).
		if result.Replayed {
			logger.Info("casino_callback_replayed",
				"request_id", requestID, "tenant_id", t.ID.String(), "provider_id", providerID,
				"event_type", string(result.EventType))
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
