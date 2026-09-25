package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/validation"
)

// Stage 7 §6/§7/§9/§10/§11: there is no real, hosted casino-provider game
// client to embed for this vertical slice (directive explicitly bans a
// real provider integration this stage). A real integration's own game
// client is what would call the platform's PUBLIC webhook
// (POST /v1/webhooks/casino/{tenantSlug}/{providerID}, casino_handlers.go)
// with signed bet/win/rollback events as the player plays. These three
// handlers are that same round-trip, driven from the B2C "game/session
// screen" instead of an embedded iframe: each one asks the tenant's own
// registered MockCasinoProvider to construct a correctly-signed callback
// payload (provider.CallbackPayload - the exact helper the existing
// casino test suite already uses to simulate a provider delivery) for the
// player's own launch session, then feeds it through
// Orchestrator.ReceiveCallback - byte-for-byte the SAME parse-verify-post
// pipeline the public webhook uses. Nothing here bypasses HandleCallback's
// signature verification or constructs a CallbackEvent directly; nothing
// in internal/casino's postBet/postWin/postRollback changes.
//
// CRITICAL TRUST-BOUNDARY WARNING (three independent specialist reviews -
// architect, security, ledger-finance - converged on this same finding):
// the mock adapter's signature is minted by THIS PROCESS on the
// AUTHENTICATED PLAYER's own behalf, so unlike a real provider webhook it
// authenticates nothing about which player/round/transaction is named.
// postBet/postWin/postRollback correctly trust a signed payload's
// identifying fields when the signer is an independent, credentialed
// provider - they were never designed to be handed a payload whose
// signer is the platform acting on an untrusted caller's instructions.
// Every field that postRollback in particular resolves authority from
// (session.PlayerAccountID/WalletID for bet, correlation_id-derived
// origin for win, and CRUCIALLY original_provider_tx_id for rollback -
// scoped only to (tenant_id, provider_id, provider_tx_id), sufficient for
// a provider-attested field, NOT sufficient for a player-supplied one)
// must be independently authorized in THIS file before ever reaching
// ReceiveCallback. See requireRollbackTargetOwnedByRound's own doc
// comment for the specific fix. Deliberately restricted to
// *casino.MockCasinoProvider (never any other CasinoProvider
// implementation) - see requireMockCasinoProvider's own doc comment -
// AND gated behind Deps.CasinoPlaySimulationEnabled (see
// casino_routes.go), which cmd/platform-api/main.go sets only outside
// production: a mock provider is, by definition, one that says yes to
// everything, so "the provider type is restricted" is not by itself a
// deployment safety gate.
//
// RoundID convention: every payload constructed here uses the session's
// own id (session.ID.String()) as RoundID - the provider-declared,
// opaque round-correlation string postBet/postWin/postRollback already
// treat as nothing more than a correlation key (CallbackEvent.RoundID's
// own doc comment). This is what lets internal/casino/history.go
// (Stage 7 §15/§16) deterministically recompute a round's own
// correlation_id from its casino_launch_sessions row alone, without any
// new column or table - a real provider's own round id is whatever IT
// declares and is not assumed to follow this convention.
//
// Amounts are capped (maxCasinoPlaySimulationAmount) as defense in depth:
// even gated to non-production, this endpoint lets a player declare their
// own win amount (there is no real game outcome to derive it from), so an
// unbounded value would let a compromised or curious token mint unbounded
// money into a shared dev/staging environment's financial test data.

// maxCasinoPlaySimulationAmount bounds stake_amount/win_amount for the
// play-simulation endpoints - generous enough for realistic manual/
// automated testing, small enough that even an unauthorized or scripted
// abuse of a non-production environment cannot mint an unbounded amount.
const maxCasinoPlaySimulationAmount int64 = 100_000_000 // 1,000,000.00 in a 2-exponent asset

// requireMockCasinoProvider resolves session's own registered provider and
// asserts it is the mock adapter - simulated play only ever exists because
// no real provider integration exists yet this stage (CLAUDE.md's "no
// fake completion": this is explicitly a MOCK simulation seam, never
// something a real integration would also expose). A tenant somehow
// routed to a non-mock adapter gets CodeUnavailable, not a panic or a
// silently-wrong simulation.
func requireMockCasinoProvider(deps Deps, providerID string) (*casino.MockCasinoProvider, bool) {
	provider, ok := deps.CasinoOrchestrator.Provider(providerID)
	if !ok {
		return nil, false
	}
	mock, ok := provider.(*casino.MockCasinoProvider)
	return mock, ok
}

// resolvePlayerOwnedSession loads sessionID and verifies it genuinely
// belongs to playerAccountID under tc's own tenant - casino_launch_
// sessions' tenant_staff_scope RLS policy permits ANY session in the
// tenant to be read/written under db.Pool.WithTenant (it requires
// app.player_account_id be unset, exactly like the withdrawal/deposit
// precedent), so this explicit ownership check is what actually
// authorizes the write here, not row-level security on this path -
// mirrors newLaunchCasinoGameHandler's identical rationale. A mismatch
// returns ErrLaunchSessionNotFound (never a distinguishable "wrong
// player" error) so a caller cannot enumerate another player's session
// ids by status-code alone.
func resolvePlayerOwnedSession(ctx context.Context, tx pgx.Tx, sessionID, playerAccountID uuid.UUID) (casino.LaunchSession, error) {
	session, err := casino.GetLaunchSessionByID(ctx, tx, sessionID)
	if err != nil {
		return casino.LaunchSession{}, err
	}
	if session.PlayerAccountID != playerAccountID {
		return casino.LaunchSession{}, casino.ErrLaunchSessionNotFound
	}
	return session, nil
}

// requireActiveUnexpiredSession is security review finding P2-2: postBet
// itself only rejects a revoked session, never an expired one, and
// postWin/postRollback resolve their accounts via correlation_id, never
// consulting session status/expiry at all. That is a reasonable design
// for postBet/postWin/postRollback's own intended caller (a real
// provider, whose own session/round bookkeeping is authoritative for
// itself), but this file's callers are a player driving the platform's
// OWN session record directly, so this handler layer is the only place
// that can still say "no" to a stale or revoked session before minting a
// financial effect against it.
func requireActiveUnexpiredSession(session casino.LaunchSession) error {
	if session.Status != casino.LaunchSessionActive {
		return &apierror.Error{Code: apierror.CodeValidation, Message: "session is not active"}
	}
	if time.Now().UTC().After(session.ExpiresAt) {
		return &apierror.Error{Code: apierror.CodeValidation, Message: "session has expired"}
	}
	return nil
}

// requireRealMode rejects a demo-mode session before any financial
// effect - a demo round must never post a real financial effect, mirrors
// postBet's own ModeReal check (defense in depth: postWin/postRollback
// never consult Mode themselves, since they resolve accounts via
// correlation_id rather than a session lookup).
func requireRealMode(session casino.LaunchSession) error {
	if session.Mode != casino.ModeReal {
		return &apierror.Error{Code: apierror.CodeValidation, Message: "a demo-mode session cannot post a real financial effect"}
	}
	return nil
}

// deterministicSimulatedProviderTxID derives a stable provider_tx_id from
// (sessionID, action, idempotencyKey) - security/ledger-finance review
// finding: minting a FRESH reference on every call (the previous
// mock.NextProviderTxID() behavior) means a client retry after a lost
// response is, by construction, a SECOND financial transaction, not a
// replay - ledger.Post's own idempotency is keyed on provider_tx_id, so a
// retry with a new one bypasses it entirely. This mirrors
// POST /v1/me/sportsbook/bets' own required idempotency_key contract
// (internal/sportsbook/orchestrator.go) - a retry with the SAME
// idempotency_key now collides on the existing (tenant_id, provider_id,
// provider_tx_id) unique index and ledger.Post returns its already-posted
// result instead of a second effect. Prefixed "sim-" so it can never
// collide with mock.NextProviderTxID()'s own "<providerID>-<seq>" shape
// (used elsewhere, e.g. rollback's own reference below, where a retry is
// already safe via postRollback's own ErrAlreadyRolledBack check - see
// that handler's own comment).
func deterministicSimulatedProviderTxID(sessionID uuid.UUID, action, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(sessionID.String() + ":" + action + ":" + idempotencyKey))
	return "sim-" + hex.EncodeToString(sum[:])[:32]
}

// recordCasinoPlaySimulationAudit adds a PLAYER-attributed audit record
// alongside whatever ActorSystem record postBet/postWin/postRollback
// themselves already wrote (orchestrator.go's own audit calls are correct
// and unchanged for their real intended caller, a provider webhook, and
// are not touched here) - security review finding: without this, a
// player-triggered financial mutation is indistinguishable in audit_log
// from a genuine provider callback, which would make an abuse
// investigation of this simulation seam impossible (CLAUDE.md's "every
// mutating administrative/financial action writes an audit record
// (actor, ...)"). Best-effort: a failure here never blocks or rolls back
// the financial effect that already committed.
func recordCasinoPlaySimulationAudit(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID, sessionID uuid.UUID, action, providerTxID string, result casino.ReceiveCallbackResult) {
	outcome := audit.OutcomeSuccess
	if result.Outcome != casino.OutcomeSucceeded {
		outcome = audit.OutcomeDenied
	}
	metadata := map[string]any{"provider_tx_id": providerTxID, "outcome": string(result.Outcome)}
	if result.LedgerTransactionID != nil {
		metadata["ledger_transaction_id"] = result.LedgerTransactionID.String()
	}
	_ = audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorPlayer, ActorID: playerAccountID,
		Action: action, TargetType: "casino_launch_session", TargetID: sessionID.String(),
		Outcome: outcome, Metadata: metadata,
	})
}

// writeCasinoCallbackResult maps a casino.ReceiveCallbackResult to a
// simulated-play response body, mirroring newCasinoWebhookHandler's own
// response envelope.
func writeCasinoCallbackResult(w http.ResponseWriter, status int, providerTxID string, result casino.ReceiveCallbackResult) {
	resp := map[string]any{"outcome": string(result.Outcome), "provider_tx_id": providerTxID, "tombstoned": result.Tombstoned}
	if result.LedgerTransactionID != nil {
		resp["ledger_transaction_id"] = result.LedgerTransactionID.String()
	}
	if result.DeclineReason != "" {
		resp["decline_reason"] = result.DeclineReason
	}
	writeJSON(w, status, resp)
}

// writeCasinoCallbackError maps ReceiveCallback's sentinel errors to their
// player-facing HTTP response, mirroring newCasinoWebhookHandler's own
// error mapping exactly (this is the SAME pipeline, so the SAME error set
// can occur).
func writeCasinoCallbackError(w http.ResponseWriter, requestID string, logger interface {
	Error(string, ...any)
}, err error, action string) {
	if errors.Is(err, casino.ErrBetNotFound) {
		logger.Error("casino_play_integrity_alert_bet_not_found", "error", err, "action", action)
		apierror.Write(w, requestID, apierror.CodeValidation, "no matching prior bet for this round")
		return
	}
	if errors.Is(err, casino.ErrProviderRoundOwnershipConflict) {
		// Mirrors newCasinoWebhookHandler's own identical branch exactly
		// (casino_handlers.go) - this is the SAME pipeline, so the SAME
		// integrity-alert/enumeration-resistance rationale applies: a 409,
		// logged at elevated severity, with a response message generic
		// enough to never reveal the round id or player identity.
		logger.Error("casino_play_integrity_alert_provider_round_ownership_conflict", "error", err, "action", action)
		apierror.Write(w, requestID, apierror.CodeConflict, "request rejected")
		return
	}
	if errors.Is(err, casino.ErrProviderTxPayloadMismatch) {
		// Mirrors newCasinoWebhookHandler's own identical branch (Stage 10
		// F-7): on this simulation route it is reached by a player reusing
		// an idempotency_key with a different stake - rejected, not
		// silently answered with the original result.
		logger.Error("casino_play_integrity_alert_payload_mismatch", "error", err, "action", action)
		apierror.Write(w, requestID, apierror.CodeConflict, "request rejected")
		return
	}
	if errors.Is(err, casino.ErrAlreadyRolledBack) {
		apierror.Write(w, requestID, apierror.CodeConflict, "original transaction already rolled back")
		return
	}
	if errors.Is(err, casino.ErrLaunchSessionRequired) {
		logger.Error("casino_play_missing_session_binding", "error", err, "action", action)
		apierror.Write(w, requestID, apierror.CodeValidation, "request rejected")
		return
	}
	if errors.Is(err, casino.ErrOutcomeNotSucceeded) {
		apierror.Write(w, requestID, apierror.CodeValidation, "outcome is not succeeded")
		return
	}
	if errors.Is(err, casino.ErrInvalidInput) {
		apierror.Write(w, requestID, apierror.CodeValidation, "request rejected: invalid input")
		return
	}
	logger.Error("casino_play_failed", "error", err, "action", action)
	apierror.Write(w, requestID, apierror.CodeInternal, "failed to process "+action)
}

type wagerCasinoRoundRequest struct {
	StakeAmount    int64  `json:"stake_amount"`
	IdempotencyKey string `json:"idempotency_key"`
}

// newWagerCasinoRoundHandler simulates the provider-side bet callback for
// the player's own launch session - see this file's own doc comment.
func newWagerCasinoRoundHandler(deps Deps) http.HandlerFunc {
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
		sessionID, err := uuid.Parse(r.PathValue("sessionID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid session id")
			return
		}
		var req wagerCasinoRoundRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("idempotency_key", req.IdempotencyKey)
		if req.StakeAmount <= 0 {
			v.Add("stake_amount", "must be a positive integer (minor units)")
		}
		if req.StakeAmount > maxCasinoPlaySimulationAmount {
			v.Add("stake_amount", "exceeds the maximum simulated play amount")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var result casino.ReceiveCallbackResult
		var providerTxID string
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			session, err := resolvePlayerOwnedSession(ctx, tx, sessionID, playerAccountID)
			if err != nil {
				return err
			}
			if err := requireRealMode(session); err != nil {
				return err
			}
			if err := requireActiveUnexpiredSession(session); err != nil {
				return err
			}
			mock, ok := requireMockCasinoProvider(deps, session.ProviderID)
			if !ok {
				return &apierror.Error{Code: apierror.CodeUnavailable, Message: "simulated play is only available for the mock provider"}
			}
			providerTxID = deterministicSimulatedProviderTxID(session.ID, "wager", req.IdempotencyKey)
			payload := mock.CallbackPayload(casino.CallbackEventBet, providerTxID, "", session.ID.String(), session.ProviderGameID,
				req.StakeAmount, session.AssetCode, casino.OutcomeSucceeded, "", session.PlayerAccountID, session.ID)
			result, err = deps.CasinoOrchestrator.ReceiveCallback(ctx, tx, tc.TenantID, session.ProviderID, payload)
			if err != nil {
				return err
			}
			recordCasinoPlaySimulationAudit(ctx, tx, tc.TenantID, playerAccountID, session.ID, "casino_play_simulation.wager", providerTxID, result)
			return nil
		})
		if errors.Is(err, casino.ErrLaunchSessionNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "session not found")
			return
		}
		var apiErr *apierror.Error
		if errors.As(err, &apiErr) {
			apierror.Write(w, requestID, apiErr.Code, apiErr.Message)
			return
		}
		if err != nil {
			writeCasinoCallbackError(w, requestID, logger, err, "wager")
			return
		}
		writeCasinoCallbackResult(w, http.StatusOK, providerTxID, result)
	}
}

type winCasinoRoundRequest struct {
	WinAmount      int64  `json:"win_amount"`
	IdempotencyKey string `json:"idempotency_key"`
}

// newWinCasinoRoundHandler simulates the provider-side win callback for
// the player's own launch session - see this file's own doc comment.
func newWinCasinoRoundHandler(deps Deps) http.HandlerFunc {
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
		sessionID, err := uuid.Parse(r.PathValue("sessionID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid session id")
			return
		}
		var req winCasinoRoundRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("idempotency_key", req.IdempotencyKey)
		if req.WinAmount <= 0 {
			v.Add("win_amount", "must be a positive integer (minor units)")
		}
		if req.WinAmount > maxCasinoPlaySimulationAmount {
			v.Add("win_amount", "exceeds the maximum simulated play amount")
		}
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var result casino.ReceiveCallbackResult
		var providerTxID string
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			session, err := resolvePlayerOwnedSession(ctx, tx, sessionID, playerAccountID)
			if err != nil {
				return err
			}
			if err := requireRealMode(session); err != nil {
				return err
			}
			if err := requireActiveUnexpiredSession(session); err != nil {
				return err
			}
			mock, ok := requireMockCasinoProvider(deps, session.ProviderID)
			if !ok {
				return &apierror.Error{Code: apierror.CodeUnavailable, Message: "simulated play is only available for the mock provider"}
			}
			providerTxID = deterministicSimulatedProviderTxID(session.ID, "win", req.IdempotencyKey)
			payload := mock.CallbackPayload(casino.CallbackEventWin, providerTxID, "", session.ID.String(), session.ProviderGameID,
				req.WinAmount, session.AssetCode, casino.OutcomeSucceeded, "", session.PlayerAccountID, uuid.Nil)
			result, err = deps.CasinoOrchestrator.ReceiveCallback(ctx, tx, tc.TenantID, session.ProviderID, payload)
			if err != nil {
				return err
			}
			recordCasinoPlaySimulationAudit(ctx, tx, tc.TenantID, playerAccountID, session.ID, "casino_play_simulation.win", providerTxID, result)
			return nil
		})
		if errors.Is(err, casino.ErrLaunchSessionNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "session not found")
			return
		}
		var apiErr *apierror.Error
		if errors.As(err, &apiErr) {
			apierror.Write(w, requestID, apiErr.Code, apiErr.Message)
			return
		}
		if err != nil {
			writeCasinoCallbackError(w, requestID, logger, err, "win")
			return
		}
		writeCasinoCallbackResult(w, http.StatusOK, providerTxID, result)
	}
}

type rollbackCasinoRoundRequest struct {
	OriginalProviderTxID string `json:"original_provider_tx_id"`
}

// requireRollbackTargetOwnedByRound is the fix for the P0 three
// independent specialist reviews (architect, security, ledger-finance)
// converged on: postRollback's own lookup of original_provider_tx_id is
// scoped to (tenant_id, provider_id, provider_tx_id) ONLY - correct for a
// signature-verified provider webhook (where that triple is fully
// provider-attested) and NOT sufficient for this endpoint, where the
// signature is self-issued by the platform on the player's own behalf and
// therefore authenticates nothing about which transaction they name.
// Without this check a player could, using their OWN launch session (so
// resolvePlayerOwnedSession's own check passes): (1) reverse a DIFFERENT
// player's bet or win, crediting/debiting that player's wallet; (2)
// reverse their own bet or win from an UNRELATED earlier round, making a
// round's own history/Back-Office view report a reversal it did not
// experience; or (3) name a never-issued provider_tx_id and have
// postRollback write a permanent tombstone in the shared, append-only
// (tenant_id, provider_id, provider_tx_id) namespace, later making a
// genuine bet/win that happens to mint that exact reference fail outright
// - a tenant-wide denial-of-service with no possible remediation short of
// re-registering the provider under a new provider_id. Requiring the
// named transaction to belong to THIS round (same correlation_id) AND
// THIS wallet closes all three: it is exactly the set of transactions a
// legitimate rollback of "my own current round" would ever need to name.
func requireRollbackTargetOwnedByRound(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, session casino.LaunchSession, originalProviderTxID string) error {
	owned, err := casino.TransactionBelongsToRound(ctx, tx, tenantID, session.ProviderID, session.ID.String(), session.WalletID, originalProviderTxID)
	if err != nil {
		return err
	}
	if !owned {
		// Enumeration-resistant: identical to "session not found" rather
		// than a distinguishable "wrong transaction" error, so a caller
		// cannot use this endpoint to probe for the existence of another
		// player's/round's provider_tx_id.
		return casino.ErrLaunchSessionNotFound
	}
	return nil
}

// newRollbackCasinoRoundHandler simulates the provider-side rollback
// callback for the player's own launch session, reversing the named prior
// bet or win via the existing compensating-entry model (postRollback) -
// see this file's own doc comment and requireRollbackTargetOwnedByRound's
// own doc comment for the authorization boundary this endpoint adds on
// top of postRollback's own (correct, for its real intended caller)
// tenant+provider-scoped lookup.
//
// No idempotency-key-derived provider_tx_id is needed here (unlike
// wager/win): a retry naming the same original_provider_tx_id is already
// safe regardless of which reference THIS rollback itself mints -
// postRollback's own check (orchestrator.go: "a prior reversal against
// originalID is only a conflict if it was posted under a DIFFERENT
// rollback reference") means a genuine second rollback attempt against an
// already-reversed original returns ErrAlreadyRolledBack (409), never a
// second reversal.
func newRollbackCasinoRoundHandler(deps Deps) http.HandlerFunc {
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
		sessionID, err := uuid.Parse(r.PathValue("sessionID"))
		if err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid session id")
			return
		}
		var req rollbackCasinoRoundRequest
		if err := decodeJSON(r, &req); err != nil {
			apierror.Write(w, requestID, apierror.CodeValidation, "invalid request body")
			return
		}
		v := validation.New()
		v.RequireNonEmpty("original_provider_tx_id", req.OriginalProviderTxID)
		if v.HasErrors() {
			apierror.Write(w, requestID, apierror.CodeValidation, v.Error())
			return
		}

		var result casino.ReceiveCallbackResult
		var providerTxID string
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			session, err := resolvePlayerOwnedSession(ctx, tx, sessionID, playerAccountID)
			if err != nil {
				return err
			}
			if err := requireRealMode(session); err != nil {
				return err
			}
			if err := requireActiveUnexpiredSession(session); err != nil {
				return err
			}
			if err := requireRollbackTargetOwnedByRound(ctx, tx, tc.TenantID, session, req.OriginalProviderTxID); err != nil {
				return err
			}
			mock, ok := requireMockCasinoProvider(deps, session.ProviderID)
			if !ok {
				return &apierror.Error{Code: apierror.CodeUnavailable, Message: "simulated play is only available for the mock provider"}
			}
			providerTxID = mock.NextProviderTxID()
			payload := mock.CallbackPayload(casino.CallbackEventRollback, providerTxID, req.OriginalProviderTxID, session.ID.String(),
				session.ProviderGameID, 0, session.AssetCode, "", "", session.PlayerAccountID, uuid.Nil)
			result, err = deps.CasinoOrchestrator.ReceiveCallback(ctx, tx, tc.TenantID, session.ProviderID, payload)
			if err != nil {
				return err
			}
			recordCasinoPlaySimulationAudit(ctx, tx, tc.TenantID, playerAccountID, session.ID, "casino_play_simulation.rollback", providerTxID, result)
			return nil
		})
		if errors.Is(err, casino.ErrLaunchSessionNotFound) {
			apierror.Write(w, requestID, apierror.CodeNotFound, "session not found")
			return
		}
		var apiErr *apierror.Error
		if errors.As(err, &apiErr) {
			apierror.Write(w, requestID, apiErr.Code, apiErr.Message)
			return
		}
		if err != nil {
			writeCasinoCallbackError(w, requestID, logger, err, "rollback")
			return
		}
		writeCasinoCallbackResult(w, http.StatusOK, providerTxID, result)
	}
}
