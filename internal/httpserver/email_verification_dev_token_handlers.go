package httpserver

import (
	"context"
	"net/http"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/tenant"
)

// Stage 9.3: no player account created through the real public
// registration flow can ever reach 'active' status through the real HTTP
// API alone. Every new registration lands in 'pending_verification'
// (internal/identity/player_account.go); the ONLY code path that ever
// transitions an account to 'active' is POST /v1/auth/email-verification/
// confirm (credential_handlers.go), which requires the raw token minted by
// POST /v1/me/email-verification/request. That raw token is handed to
// email.Provider.Send and NOTHING ELSE - email.MockProvider's own doc
// comment is explicit that this is deliberate: there is no real inbox to
// deliver a mock email to, so it correctly refuses to fake one by exposing
// a sent message's body via any API. Net effect: 'active' is the single
// shared gate internal/rg.EvaluateEligibility (CodePlayerAccountNotActive)
// enforces before internal/payments (deposit), internal/casino
// (LaunchGame, postBet), and internal/sportsbook (PlaceBet) allow ANY
// financial/gambling action, and a real, HTTP-only acceptance-test player
// can never pass it.
//
// This file is the identity-domain twin of casino_play_handlers.go and
// payment_deposit_simulation_handlers.go, built to the identical
// precedent, but shaped differently because there is nothing to "simulate"
// here - the real email-verification-CONFIRM endpoint already works
// correctly and is left completely untouched. The only genuine gap is
// "how does a caller with no reachable inbox learn the raw token a mock
// send already minted for them". So this file adds exactly one read-only
// route, GET /v1/me/email-verification/dev-token, which lets an
// authenticated player retrieve THEIR OWN currently-pending
// email-verification raw token - to be fed, by the caller, into the
// UNMODIFIED POST /v1/auth/email-verification/confirm endpoint. This is
// deliberately NOT a second "force-activate" mechanism that skips the real
// confirm flow (this codebase's own "no parallel competing
// implementations" rule) - it closes the "how do I get the token out of a
// mock inbox with no reachable inbox" gap and nothing else. Token hashing,
// expiry, and single-use consumption in the real confirm path are all
// completely unchanged.
//
// Trust boundary: the caller can only ever retrieve THEIR OWN token,
// derived exclusively from the authenticated JWT subject
// (tenant.FromContext's Subject/TenantID) - there is no id path parameter
// at all (a stronger structural guarantee than
// payment_deposit_simulation_handlers.go's resolveOwnDepositIntent, which
// at least accepts a caller-supplied id to check ownership against: here
// there is nothing for a caller to name, so there is no id to name
// SOMEONE ELSE'S token with). No staff/admin principal can reach this
// route at all (auth.RequirePlayerPrincipal, verified by
// TestAccountActivationDevToken_StaffTokenDenied).
//
// Gated behind Deps.AccountActivationTestSupportEnabled (see
// server.go), which cmd/platform-api/main.go sets only outside
// production, exactly like the two precedents above - the route is
// genuinely absent from the mux when the flag is false, not merely
// 404'd inside a handler.
//
// SECURITY-SENSITIVE: this is a new mechanism for retrieving an
// auth-adjacent credential/token. Per CLAUDE.md, it requires explicit
// `security` specialist review before being marked complete.

// devVerificationTokenEntry is one player's most recently issued
// email-verification token, captured at the exact moment
// newRequestEmailVerificationHandler already holds the raw value (right
// before handing it to email.Provider.Send, which never persists or
// exposes it again). Recording the tokenID alongside the raw value is
// what lets newAccountActivationDevTokenHandler re-verify against the
// database that this specific token is STILL live (unconsumed,
// unexpired) before ever handing the raw value back out - a stale entry
// left over from an account that has since confirmed (or requested a
// newer token from another device) must never be served as if it still
// worked.
type devVerificationTokenEntry struct {
	tokenID  uuid.UUID
	rawToken string
}

// devVerificationTokenStore is an in-memory, non-persisted map from
// playerAccountID to that player's most recently issued
// email-verification token. Deliberately NEVER written to the database -
// the real player_credential_tokens table already stores only a SHA-256
// hash of the raw token (auth/credential_token.go), and this store must
// not weaken that: it exists purely so ONE additional in-process reader
// (this file's own handler) can learn a value the request-handler already
// held transiently on the same running instance, mirroring exactly how
// this repository's own existing Go integration test suite reaches a mock
// provider's in-process state (payment_deposit_simulation_handlers.go's
// own doc comment) - the difference here is that a real, standalone
// HTTP-only staging process needs an HTTP-reachable equivalent of that
// same seam, which is exactly what this store, plus the route below,
// provides.
//
// Entries are silently overwritten (never appended) - IssueCredentialToken
// itself supersedes any prior outstanding token of the same purpose for
// an account, so at most one entry per player is ever meaningful, and a
// fresh request naturally replaces whatever was recorded before.
// Unbounded for the lifetime of a process instance: acceptable ONLY
// because this store is allocated at all solely when
// AccountActivationTestSupportEnabled is true (never in production - see
// server.go), i.e. a non-production, test/staging-only instance whose
// player population and lifetime are both operator-controlled.
type devVerificationTokenStore struct {
	mu      sync.Mutex
	entries map[uuid.UUID]devVerificationTokenEntry
}

func newDevVerificationTokenStore() *devVerificationTokenStore {
	return &devVerificationTokenStore{entries: make(map[uuid.UUID]devVerificationTokenEntry)}
}

func (s *devVerificationTokenStore) record(playerAccountID, tokenID uuid.UUID, rawToken string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[playerAccountID] = devVerificationTokenEntry{tokenID: tokenID, rawToken: rawToken}
}

func (s *devVerificationTokenStore) lookup(playerAccountID uuid.UUID) (devVerificationTokenEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.entries[playerAccountID]
	return entry, ok
}

func (s *devVerificationTokenStore) delete(playerAccountID uuid.UUID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, playerAccountID)
}

// newAccountActivationDevTokenHandler serves GET /v1/me/email-verification/
// dev-token - see this file's own doc comment for the full rationale and
// trust-boundary argument.
func newAccountActivationDevTokenHandler(deps Deps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		requestID := observability.RequestIDFromContext(r.Context())
		logger := observability.LoggerFromContext(r.Context(), deps.Logger)

		if deps.accountActivationDevTokens == nil {
			// Defensive only: registerCredentialRoutes never registers this
			// route unless AccountActivationTestSupportEnabled is true, and
			// New() only leaves accountActivationDevTokens nil when that
			// same flag is false - this branch should be unreachable.
			apierror.Write(w, requestID, apierror.CodeUnavailable, "account-activation test support is not enabled on this deployment")
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

		entry, ok := deps.accountActivationDevTokens.lookup(playerAccountID)
		if !ok {
			apierror.Write(w, requestID, apierror.CodeNotFound,
				"no pending email-verification token - call POST /v1/me/email-verification/request first")
			return
		}

		// Re-verify against the database, inside the caller's own tenant
		// scope, that this EXACT token id is still live (unconsumed,
		// unexpired) for this EXACT player account - never trust the
		// in-memory copy alone. This is what makes an already-confirmed
		// account (or one that requested a newer token elsewhere,
		// superseding this one) return a clean 404 instead of handing back
		// a raw value that would only fail later at the real confirm
		// endpoint - and it is a read of player_credential_tokens' own
		// authoritative state, not a second source of truth.
		var stillLive bool
		err = deps.DB.WithTenant(r.Context(), tc.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			if scanErr := tx.QueryRow(ctx,
				`SELECT EXISTS (
					SELECT 1 FROM player_credential_tokens
					 WHERE id = $1 AND player_account_id = $2 AND purpose = $3
					   AND consumed_at IS NULL AND expires_at > now()
				)`,
				entry.tokenID, playerAccountID, auth.PurposeEmailVerification,
			).Scan(&stillLive); scanErr != nil {
				return scanErr
			}
			outcome := audit.OutcomeSuccess
			if !stillLive {
				outcome = audit.OutcomeDenied
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: tc.TenantID, ActorType: audit.ActorPlayer, ActorID: playerAccountID,
				Action: "account_activation_test_support.dev_token_retrieved", TargetType: "player_account", TargetID: playerAccountID.String(),
				Outcome: outcome, IPAddress: clientIP(r), UserAgent: r.UserAgent(), RequestID: requestID,
			})
		})
		if err != nil {
			logger.Error("account_activation_dev_token_lookup_failed", "error", err)
			apierror.Write(w, requestID, apierror.CodeInternal, "failed to resolve verification token")
			return
		}
		if !stillLive {
			deps.accountActivationDevTokens.delete(playerAccountID)
			apierror.Write(w, requestID, apierror.CodeNotFound,
				"no pending email-verification token - call POST /v1/me/email-verification/request first")
			return
		}

		writeJSON(w, http.StatusOK, map[string]any{"token": entry.rawToken})
	}
}
