package casino

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/kyc"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/observability"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/txscope"
	"github.com/Diansalas/igaming-platform/internal/wallet"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// Orchestrator is the casino integration's routing/dispatch layer (ADR
// 0025): it resolves which CasinoProvider serves a game, mints/resolves
// game-launch sessions, and translates verified provider callbacks into
// ledger postings for Flows 5-7. It never posts a ledger entry outside
// of calling ledger.Post with the canonical shapes those flows specify -
// no provider-specific branch exists anywhere in this file
// (docs/decisions/0022 §1's "no provider branch" rule, applied here too).
type Orchestrator struct {
	// providers is the Go-level adapter registry keyed by provider_id -
	// the authority on which CasinoProvider implementation a provider_id
	// actually is, mirroring internal/payments.Orchestrator's identical
	// registry/authority split (docs/decisions/0022 §2.1).
	providers map[string]CasinoProvider
	// webhookCredentialResolver is the single injected inbound-webhook
	// credential resolver (Stage 10.2, CAS-WH-TENANT-1, ADR 0091, design
	// §C1) - the only component that ever sees inbound-callback secret
	// material. A nil resolver fails every callback closed
	// (ReasonNoResolver), never falling back to unauthenticated
	// verification - this is what forces explicit wiring (cmd/platform-
	// api/wiring.go) rather than an accidental always-on mock.
	webhookCredentialResolver webhookauth.Resolver
	// webhookSchemes: every adapter's validated WebhookScheme() (Stage
	// 10.3 W1a; webhook_verify.go).
	webhookSchemes *webhookauth.SchemeSet
	// webhookLogger receives the matched-key_id line after a successful
	// callback verification (webhook_verify.go, W2A-SEC-2), and (security
	// review RV-PRH-I2 C1/F3) LaunchGame's own phase-C-transaction-itself-
	// failed line - the one operator-facing log point this package wires,
	// reused rather than duplicated. Nil means slog.Default().
	webhookLogger *slog.Logger
}

// phaseCTimeout bounds LaunchGame's own phase-C transactions (revoke +
// casino.launch_failed, or the casino.launched audit) - these run on
// context.WithoutCancel(ctx) specifically so a cancelled request context
// can never skip them (security review RV-PRH-I2 C1), so they need their
// OWN bound instead of inheriting the caller's deadline.
const phaseCTimeout = 5 * time.Second

// phaseCLogger is the redaction-safe, operator-only log point for a
// phase-C transaction failure (security review RV-PRH-I2 C1/F3: this must
// never be a silently discarded `_ =`) - reuses webhookLogger rather than
// adding a second logger field, mirroring VerifyCallback's identical
// nil-means-slog.Default() convention.
func (o *Orchestrator) phaseCLogger() *slog.Logger {
	if o.webhookLogger != nil {
		return o.webhookLogger
	}
	return slog.Default()
}

// LaunchFailureReason is a closed, bounded classification of why phase C
// (launchFailed) revoked a launch session - the ONLY thing ever written
// into the append-only "casino.launch_failed" audit record's "reason"
// field (orchestrator review follow-up on RV-PRH-I2: raw error text, e.g.
// a *url.Error's embedded request URL, must never reach append-only
// audit storage - a future real HTTP adapter's transport error can carry
// query-string credentials). The underlying cause's own text is available
// only through LaunchGame's returned Go error (never persisted) and, in
// redacted form, through phaseCLogger's operator-only log line.
type LaunchFailureReason string

const (
	LaunchFailureProviderUnavailable       LaunchFailureReason = "provider_unavailable"
	LaunchFailureDeclined                  LaunchFailureReason = "declined"
	LaunchFailureCircuitOpen               LaunchFailureReason = "circuit_open"
	LaunchFailureCredentialUnavailable     LaunchFailureReason = "credential_unavailable"
	LaunchFailureCredentialBindingMismatch LaunchFailureReason = "credential_binding_mismatch"
	LaunchFailureCtxCancelled              LaunchFailureReason = "ctx_cancelled"
	LaunchFailureInternal                  LaunchFailureReason = "internal"
	// LaunchFailureTxHeld (IO-1B): phase B's own txscope.Held(ctx) refusal
	// fired - see ErrProviderCallRefused's own doc comment (types.go).
	LaunchFailureTxHeld LaunchFailureReason = "tx_held"
)

// redactedLaunchFailureDetail is the ONLY place cause.Error() text is ever
// rendered, and only for phaseCLogger's operator-only log line - never for
// the append-only audit record (see LaunchFailureReason). It never echoes
// arbitrary error text: a *url.Error's embedded request URL (which may
// carry a query-string credential) is reduced to its operation plus a
// timeout/canceled/transport-error classification, exactly like
// internal/payments' own gate.go redactedReason (S95-C8(a)) - duplicated
// rather than imported, since internal/casino must not import
// internal/payments. Anything else that is not one of this package's own
// recognized sentinels is reduced to a fixed, non-identifying label,
// stricter than simply falling back to err.Error().
func redactedLaunchFailureDetail(err error) string {
	if err == nil {
		return ""
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		switch {
		case errors.Is(uerr.Err, context.DeadlineExceeded):
			return fmt.Sprintf("%s: timeout", uerr.Op)
		case errors.Is(uerr.Err, context.Canceled):
			return fmt.Sprintf("%s: canceled", uerr.Op)
		default:
			return fmt.Sprintf("%s: transport error", uerr.Op)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, ErrProviderUnavailable):
		return "provider unavailable"
	default:
		// Never echo unrecognized error text verbatim - it may originate
		// from a real adapter's transport layer or vendor SDK and embed
		// response bytes, headers or a credential-bearing URL that did not
		// happen to be wrapped in a *url.Error.
		return "adapter error (redacted)"
	}
}

// NewOrchestrator constructs an Orchestrator over the given adapter
// registry (provider_id -> CasinoProvider implementation) and the single
// inbound-webhook credential resolver every callback verifies against. A
// nil resolver is valid and deliberate (fails every callback closed) -
// mirrors internal/payments.NewOrchestrator's identical parameter shape
// (Stage 10.2, ruling J14/C1).
//
// Stage 10.3 W1a: every adapter's WebhookScheme() is validated here - a bad
// declaration panics, so the process refuses to start.
func NewOrchestrator(providers map[string]CasinoProvider, resolver webhookauth.Resolver) *Orchestrator {
	// ADR 0097 §6.3/§20 AC6: fail-closed registration for undeclared
	// non-MOCK webhook adapters (see payments.NewOrchestrator's identical
	// comment).
	webhookauth.MustRequireRetrySemantics("casino", providers)
	return &Orchestrator{providers: providers, webhookCredentialResolver: resolver, webhookSchemes: mustCasinoSchemeSet(providers)}
}

// Provider returns the registered adapter for providerID - used by
// admin/config code (e.g. writing a ProviderCapability row or syncing a
// catalogue) that needs the actual adapter instance.
func (o *Orchestrator) Provider(providerID string) (CasinoProvider, bool) {
	p, ok := o.providers[providerID]
	return p, ok
}

// LaunchGameParams is LaunchGame's input. TenantID/BrandID/PlayerAccountID/
// WalletID MUST be resolved server-side from the authenticated player
// session - never from a request body (payment-orchestration.md §3's
// identical rule, restated for casino by ADR 0025 §3). GameID is the
// PLATFORM's own game id (from ListAvailableGames), never a client-
// supplied provider_game_id - resolving through the platform's own
// catalogue is what lets ResolveLaunchEligibility enforce availability/
// jurisdiction/asset checks before ever contacting a provider.
type LaunchGameParams struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
	GameID          uuid.UUID
	AssetCode       string
	Mode            GameMode
	// Jurisdiction is no longer a caller-supplied field (K-3 remediation,
	// docs/governance/stage-4i-canonical-model.md §9): LaunchGame resolves
	// it itself, server-side, via internal/jurisdiction.Resolve, using
	// only TenantID/BrandID/PlayerAccountID already on this struct - never
	// from client-supplied input (a request body, header, or geo hint a
	// caller controls). See LaunchGame's own K-3 doc comment for the full
	// contract; the resolved value is persisted onto the launch session
	// (migration 0042) and reused as the risk scope for every bet in the
	// round, exactly as before this fix.
}

// LaunchGameResult is what LaunchGame returns to its caller (an HTTP
// handler). Denied/DenialCode/DenialMessage report a Stage 4D-RG policy
// denial (internal/rg.EvaluateEligibility) - deliberately a RESULT field,
// never a Go error: the audit record evaluateAndAuditEligibility writes
// for a denial is in the SAME transaction as this call, and db.WithTenant
// rolls back that entire transaction - audit record included - whenever
// its callback returns a non-nil error (see db.Pool.WithTenant's own doc
// comment). Returning the denial as an error was tried and found to
// silently discard its own audit record every time - exactly the "result
// must be... auditable" failure directive §5 exists to prevent - fixed by
// mirroring postBet's already-correct decline-without-error convention
// (see its own identical insufficient-funds case) instead.
type LaunchGameResult struct {
	LaunchURL     string
	SessionID     uuid.UUID
	ExpiresAt     time.Time
	Denied        bool
	DenialCode    string
	DenialMessage string
}

// LaunchGame resolves a game's eligibility (platform status, tenant/
// brand availability, jurisdiction, asset support), resolves its
// provider's own capability, mints a single-use launch session (ADR 0025
// §3), and calls the provider's Launch. Every failure mode returns a
// specific, distinguishable sentinel error (directive items C/O/P/Q depend
// on this - "game doesn't exist" vs. "not enabled here" vs. "provider
// unavailable" vs. "blocked in this jurisdiction" are never collapsed into
// one generic not-found).
//
// ADR 0095 §15.1 (PRH-I2, two-phase launch split): LaunchGame owns its own
// transaction boundaries instead of receiving one from its caller. Phase A
// - every eligibility/capability check plus CreateLaunchSession plus the
// "casino.launch_requested" audit record - runs in ONE tenant-scoped
// transaction (pool.WithTenant) that commits before this function ever
// calls a provider adapter method that can perform real I/O (HealthStatus,
// Launch). No transaction, and therefore no pooled connection, is held
// across either call. Phase B resolves the per-call outbound credential
// (PROV-OUTBOUND-CRED-1) and calls Launch. Phase C is a second short
// transaction: success audits "casino.launched"; a failed or ambiguous
// outcome revokes the session (CAS on status='active') and audits
// "casino.launch_failed" - so a launch token is never left usable for a
// round the platform can't account for. Phase C runs on a request-ctx-
// independent, bounded timeout (context.WithoutCancel plus a short
// deadline, not the caller's own ctx) specifically so a cancelled request
// context (a client disconnect mid-Launch, the ordinary real-world trigger
// once a real adapter does I/O) can never silently skip the revoke and the
// audit (security review RV-PRH-I2 C1 / code review R1) - a phase-C
// transaction failure is logged (never discarded) and the success path
// attempts a revoke too if its own audit write fails, rather than leaving
// a vendor-accepted launch un-auditable.
//
// A crash on either side of the vendor call (a genuine process crash, not
// a ctx cancellation, which phase C's own timeout now survives) leaves the
// session 'active' and unconsumed. This is harmless SPECIFICALLY because
// postBet itself rejects a bet placed against a non-active/non-consumed OR
// expired session (ErrLaunchSessionRequired) - the platform's own
// expires_at bound, not merely "no legitimate vendor would call back",
// closes the exposure window to at most DefaultLaunchTokenTTL for a
// session that stays 'active' (never consumed). A session the vendor has
// already 'consumed' is NOT time-bounded (CAS-SESSION-EXPIRY-1, ADR 0095
// §15.1.5): its revocation after a failed launch is the open
// CAS-REVOKE-CONSUMED-1 (security review RV-PRH-I2, casino/ledger-finance
// follow-up on §15.1's original wording, which incorrectly relied on
// expiry never being checked at all). Retries are the existing behaviour
// (no automatic retry; a player retry mints a brand-new session/token) -
// there is no code path that mints a second token for the same session.
//
// pool is the tenant-scoped transaction runner (*db.Pool satisfies it,
// mirroring providercred.TenantTxRunner's identical shape); outbound is the
// casino domain's outbound-credential resolver (a MOCK for a synthetic
// adapter, providercred's real *(*Subsystem).Outbound("casino") for a real
// one). Both nil fail every launch closed (never a silent fallback), the
// same nil-resolver convention SetWebhookLogger/the inbound resolver field
// already use.
func (o *Orchestrator) LaunchGame(ctx context.Context, pool providercred.TenantTxRunner, outbound OutboundCredentialResolver, params LaunchGameParams) (LaunchGameResult, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.WalletID == uuid.Nil || params.GameID == uuid.Nil {
		return LaunchGameResult{}, fmt.Errorf("%w: launch requires fully-populated, server-derived identity fields", ErrInvalidInput)
	}
	if params.AssetCode == "" {
		return LaunchGameResult{}, fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if params.Mode != ModeReal && params.Mode != ModeDemo {
		return LaunchGameResult{}, fmt.Errorf("%w: mode must be 'real' or 'demo'", ErrInvalidInput)
	}
	if pool == nil {
		return LaunchGameResult{}, fmt.Errorf("%w: casino launch has no transaction runner configured", ErrProviderUnavailable)
	}

	// Phase A: one committed transaction for every check plus the session
	// mint plus its own audit record. denied captures a policy decision
	// (RG/risk/jurisdiction) that must still COMMIT its own audit row
	// (evaluateAndAuditEligibility/evaluateAndAuditRisk/
	// evaluateJurisdictionBlocklist already wrote it) without being treated
	// as a transaction-aborting error - mirroring the pre-split code's
	// identical "Denied is a result, never a Go error" contract (see
	// LaunchGameResult's own doc comment).
	var (
		session        LaunchSession
		token          string
		providerID     string
		providerGameID string
		denied         *LaunchGameResult
	)
	txErr := pool.WithTenant(ctx, params.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		game, err := GetGameByID(ctx, tx, params.GameID)
		if err != nil {
			return err
		}
		if game.Status != GameStatusActive {
			return ErrGameDisabled
		}
		available, err := IsGameAvailable(ctx, tx, params.TenantID, params.BrandID, params.GameID)
		if err != nil {
			return err
		}
		if !available {
			return ErrGameNotAvailable
		}

		// K-3 remediation (canonical-model §9): resolve the player's
		// jurisdiction ONCE, here, and feed all three consumers from this one
		// value (K3-3) - the blocklist check immediately below, the
		// RiskRequest further down (real-mode only), and CreateLaunchSession's
		// persisted snapshot. This closes the defect where this line used to
		// dereference params.JurisdictionCode directly while a SEPARATELY-
		// derived local fed Risk/the session - two dereferences of what should
		// always have been one value.
		//
		// K3-4 (demo mode decided EXPLICITLY, canonical-model §9.6): this
		// resolution, and the blocklist check it feeds, run for BOTH real AND
		// demo launches - never conditioned on params.Mode. This is
		// architect's deliberate ruling that demo launches are catalogue-
		// availability-bearing BY DEFAULT: "may this title be offered in this
		// market" is a question about the CATALOGUE, not about whether real
		// money is at stake, so the platform's answer must not depend on which
		// endpoint is asked. This is a stated, commented choice - not an
		// accident of this code sitting above the params.Mode == ModeReal
		// branch below (which gates Risk, a genuinely money-only concern).
		//
		// Resolve is a cheap, read-only, no-external-network-dependency
		// Postgres read (canonical-model §9.4) - there is no cost reason to
		// skip it for an unarmed game, and every player-scoped resolution
		// resolves unresolved(no_signal) today regardless (HDR-J-3 is
		// unanswered, canonical-model §11.3), so running it unconditionally
		// changes nothing observable for a game carrying no blocklist.
		jurisdictionResolution, err := jurisdiction.Resolve(ctx, tx, jurisdiction.Params{
			TenantID: params.TenantID, BrandID: &params.BrandID, PlayerAccountID: &params.PlayerAccountID,
			OperationClass:       jurisdiction.OperationPlay,
			RequestedByActorType: jurisdiction.ActorPlayer, RequestedByActorID: &params.PlayerAccountID,
		})
		if err != nil {
			return fmt.Errorf("casino: resolve jurisdiction: %w", err)
		}

		// K3-1 - the one correction the canonical model makes to RISK's/
		// security's own K-3 recommendations (canonical-model §9.1's full
		// reasoning is not re-derived here): the control is only ARMED for a
		// game whose OWN jurisdiction_blocklist is non-empty. This is a clean,
		// statically-determinable test (no jurisdiction resolution needed at
		// all) - a game with an empty blocklist has no jurisdiction-dependent
		// policy in force, so it must not start denying launches the moment
		// jurisdiction resolution exists in the codebase. Applying RISK's
		// original "remove the params.JurisdictionCode != nil guard
		// unconditionally" recommendation literally would deny 100% of casino
		// launches in Stage 4I, since every player-scoped resolution is
		// unresolved(no_signal) today (HDR-J-3 unanswered).
		// canonical-model §4.4 Layer 2: every consuming gate re-asserts the
		// resolution's own tenant/brand/player binding against its OWN
		// authenticated context before ever calling Code() - a resolution for
		// a different player (or a tenant-subject resolution with no player at
		// all) is structurally unusable here. Resolve derived this SAME
		// resolution from these SAME params two lines above, so this is
		// defense-in-depth rather than something that can genuinely diverge in
		// production - but it is exactly the check that keeps a future
		// refactor from ever laundering a mismatched resolution through this
		// gate, and it is what makes evaluateJurisdictionBlocklist below safe
		// to feed a resolution obtained from anywhere.
		if err := jurisdictionResolution.AssertScope(params.TenantID, &params.BrandID, &params.PlayerAccountID); err != nil {
			return fmt.Errorf("casino: jurisdiction resolution scope: %w", err)
		}
		if blocked, denialCode, err := evaluateJurisdictionBlocklist(game.JurisdictionBlocklist, jurisdictionResolution); err != nil {
			return err
		} else if blocked {
			denied = &LaunchGameResult{Denied: true, DenialCode: denialCode}
			return nil
		}
		if !containsString(game.SupportedAssets, params.AssetCode) {
			return fmt.Errorf("%w: game does not support asset %s", ErrInvalidInput, params.AssetCode)
		}

		// Stage 4D-RG: the single authoritative "may this player gamble right
		// now" policy boundary (ADR 0026 §5/§6), consulted BEFORE a launch
		// session is minted - a prohibited player (suspended, self-excluded
		// anywhere on the platform via their cross-brand Person, or holding a
		// non-active wallet) must never obtain a usable session, regardless of
		// what any provider does or does not enforce on its own end.
		decision, err := evaluateAndAuditEligibility(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID, params.WalletID, "casino.launch_denied")
		if err != nil {
			return err
		}
		if !decision.Allowed {
			denied = &LaunchGameResult{Denied: true, DenialCode: decision.Code, DenialMessage: decision.Message}
			return nil
		}

		// K3-3 (continued): the SAME jurisdictionResolution computed above
		// feeds the risk check below and the launch session's persisted
		// snapshot just below that (Stage 4G-FINAL Part C) - so a
		// jurisdiction-scoped rule stays reachable from this SAME round's
		// later bets (postBet), not just at launch time. Empty only when the
		// resolution did not resolve - never silently defaulted
		// (jurisdiction.Resolution.Code() is structurally unreachable for a
		// non-Resolved outcome, canonical-model §2.3 property 2).
		var jurisdictionCode string
		if jurisdictionResolution.Outcome() == jurisdiction.Resolved {
			jurisdictionCode, _ = jurisdictionResolution.Code() // err impossible: Outcome() == Resolved, just checked
		}

		// Stage 4G: the central Risk & Limits boundary, consulted alongside
		// (never instead of) RG eligibility above - a separate domain, per
		// ADR 0031 §1. Skipped for demo-mode launches: no real financial
		// exposure exists yet to gate. A REVIEW outcome is treated identically
		// to DENY at this integration point (ADR 0031 §6 - no
		// compliance-review workflow exists yet for casino launch, so a
		// review-flagged launch fails safe by blocking rather than proceeding
		// provisionally).
		if params.Mode == ModeReal {
			// Stage 4G-FINAL Part D: resolved server-side from the tenant's
			// OWN tenants.licensing_model, never assumed - risk.Evaluate
			// itself never looks this up (see RiskRequest.LicensingMode's doc
			// comment). Lets a platform-wide HARD_LIMIT expressing the
			// PLATFORM's own licence's legal ceiling be scoped so it does not
			// also bind a future bring-your-own-licence tenant.
			licensingMode, err := resolveLicensingMode(ctx, tx, params.TenantID)
			if err != nil {
				return err
			}
			riskDecision, err := evaluateAndAuditRisk(ctx, tx, risk.RiskRequest{
				TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID,
				Operation: risk.OperationCasinoLaunch, Product: "casino", ProviderID: game.ProviderID, GameID: params.GameID,
				AssetCode: params.AssetCode, JurisdictionCode: jurisdictionCode, LicensingMode: licensingMode,
			}, "casino.launch_denied_by_risk_policy")
			if err != nil {
				return err
			}
			// ALLOW / REVIEW / DENY / error-or-unavailable are FOUR distinct
			// outcomes, classified in one shared place rather than collapsed
			// into "not allow" at each call site (ADR 0031 §34).
			proceed, err := classifyRiskOutcome(riskDecision.Outcome)
			if err != nil {
				return err
			}
			if !proceed {
				denied = &LaunchGameResult{Denied: true, DenialCode: riskDecision.Code, DenialMessage: riskDecision.Message}
				return nil
			}
		}

		capability, found, err := LoadCapability(ctx, tx, params.TenantID, params.BrandID, game.ProviderID)
		if err != nil {
			return err
		}
		if !found || capability.Status != CapabilityActive || !capability.SupportsLaunch {
			return ErrProviderUnavailable
		}
		if !containsString(capability.SupportedAssets, params.AssetCode) {
			return ErrProviderUnavailable
		}
		// Stage 10.3 CAS-CAP-ROLLBACK-1 (§1.4 step 7, launch coherence): a
		// real-money launch must also require supports_bet - otherwise a
		// player could obtain a usable real-money session in which every bet
		// callback then 503s at postBet's own capability gate. Demo launches
		// are unaffected (no financial exposure to gate); this has no ledger
		// effect either way.
		if params.Mode == ModeReal && !capability.SupportsBet {
			return ErrProviderUnavailable
		}

		if _, registered := o.providers[game.ProviderID]; !registered {
			return fmt.Errorf("%w: %s", ErrUnknownProvider, game.ProviderID)
		}

		session, token, err = CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
			TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID, WalletID: params.WalletID,
			GameID: params.GameID, ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID,
			AssetCode: params.AssetCode, Mode: params.Mode, JurisdictionCode: jurisdictionCode,
		})
		if err != nil {
			return err
		}
		providerID, providerGameID = game.ProviderID, game.ProviderGameID

		return audit.Record(ctx, tx, audit.Entry{
			TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
			Action: "casino.launch_requested", TargetType: "casino_launch_session", TargetID: session.ID.String(),
			Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"game_id": params.GameID.String(), "provider_id": game.ProviderID, "provider_game_id": game.ProviderGameID,
				"asset_code": params.AssetCode, "mode": string(params.Mode),
			},
		})
	})
	if txErr != nil {
		return LaunchGameResult{}, txErr
	}
	if denied != nil {
		return *denied, nil
	}

	// launchFailed runs phase C's failure path - RevokeLaunchSession's CAS
	// on status='active', plus the "casino.launch_failed" audit record -
	// and returns the caller-facing error.
	//
	// Security review RV-PRH-I2 C1 / code review R1: this MUST NOT run on
	// the request's own ctx. If the request context is already cancelled
	// (a client disconnect mid-Launch, the ordinary real-world trigger once
	// a real adapter does I/O), pool.WithTenant(ctx, ...) would fail to
	// even begin a transaction, silently skipping BOTH the revoke and the
	// audit - leaving the session 'active' and bet-eligible while the
	// player was told the launch failed. context.WithoutCancel detaches
	// from the request's own cancellation/deadline; the bounded timeout
	// this function adds back is phaseCTimeout, never the caller's.
	//
	// A failure of phase C itself (not the triggering cause) is logged at
	// error level - never discarded - since it can leave a live, bet-
	// eligible session with no trace in the audit table (F3).
	// launchFailed's "reason" parameter is the ONLY thing ever written into
	// the append-only casino.launch_failed audit record - a closed,
	// bounded LaunchFailureReason, never cause.Error() (orchestrator
	// review follow-up on RV-PRH-I2: a real adapter's transport error can
	// carry a full request URL, including query-string credentials, and
	// append-only audit storage must never receive that). cause's own text
	// reaches an operator ONLY through phaseCLogger's log line, and only
	// in redactedLaunchFailureDetail's redacted form - never verbatim,
	// never into audit.
	launchFailed := func(reason LaunchFailureReason, cause error, logFields ...any) (LaunchGameResult, error) {
		phaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), phaseCTimeout)
		defer cancel()
		var revoked bool
		var priorStatus LaunchSessionStatus
		phaseErr := pool.WithTenant(phaseCtx, params.TenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			priorStatus, revoked, err = RevokeLaunchSession(ctx, tx, session.ID)
			if err != nil {
				return err
			}
			return audit.Record(ctx, tx, audit.Entry{
				TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
				Action: "casino.launch_failed", TargetType: "casino_launch_session", TargetID: session.ID.String(),
				Outcome: audit.OutcomeFailure,
				Metadata: map[string]any{
					"game_id": params.GameID.String(), "provider_id": providerID, "provider_game_id": providerGameID,
					"reason": string(reason), "revoked": revoked, "prior_status": string(priorStatus),
				},
			})
		})
		// Operator-only, redaction-safe detail line - every launch failure,
		// not only a phase-C transaction failure, so the actual cause is
		// still discoverable somewhere without ever touching audit.
		// logFields carries only pre-vetted, structured, non-error-text
		// values a call site explicitly chose to attach (e.g. the
		// provider's own declared decline_reason) - never a raw error.
		fields := append([]any{
			"tenant_id", params.TenantID.String(), "session_id", session.ID.String(),
			"provider_id", providerID, "reason", string(reason), "detail", redactedLaunchFailureDetail(cause),
		}, logFields...)
		o.phaseCLogger().Warn("casino_launch_failed", fields...)
		if phaseErr != nil {
			o.phaseCLogger().Error("casino_launch_phase_c_failed", "tenant_id", params.TenantID.String(),
				"session_id", session.ID.String(), "action", "casino.launch_failed", "error", phaseErr.Error())
		}
		return LaunchGameResult{}, fmt.Errorf("casino: %w", cause)
	}

	// Phase A committed (this released every advisory lock RG/risk took):
	// the provider is now resolved from the SAME registry Phase A already
	// verified holds this provider id, under no transaction.
	provider, registered := o.providers[providerID]
	if !registered {
		// Unreachable in practice (Phase A just verified registration under
		// the same process-lifetime registry), but never assume: fail the
		// same way an actually-missing adapter would, without dereferencing
		// a nil CasinoProvider.
		return launchFailed(LaunchFailureInternal, fmt.Errorf("%w: provider %q no longer registered", ErrProviderUnavailable, providerID))
	}

	// Health snapshot outside any transaction (ADR 0095 §9.6/§15.1):
	// HealthStatus is contractually in-memory-only and returns promptly, so
	// this never performs I/O while (or without) a pooled connection held.
	if health, err := provider.HealthStatus(ctx); err == nil && health.CircuitState == CircuitOpen {
		return launchFailed(LaunchFailureCircuitOpen, fmt.Errorf("%w: circuit open", ErrProviderUnavailable))
	}

	// Phase B: resolve the per-call outbound credential (PROV-OUTBOUND-
	// CRED-1) OUTSIDE any transaction, then call Launch - the one call in
	// this function that may perform real provider I/O, with no pooled
	// connection held across it.
	if outbound == nil {
		return launchFailed(LaunchFailureCredentialUnavailable, fmt.Errorf("%w: no outbound credential resolver configured", ErrProviderUnavailable))
	}
	cred, err := outbound.Resolve(ctx, pool, params.TenantID, providerID)
	if err != nil {
		// Code review F2.1: a credential-resolution failure is "provider
		// unavailable" to the player (an unconfigured or rotated vendor
		// credential is not something the player caused or can retry
		// around any differently), so it maps to 503 exactly like every
		// other phase-B/phase-A-registry failure - not a generic 500.
		return launchFailed(LaunchFailureCredentialUnavailable, fmt.Errorf("%w: resolve outbound credential: %v", ErrProviderUnavailable, err))
	}
	// Defense in depth (ADR 0095 §9.1/S95-C8(b)): the credential this call
	// just resolved must bind to the SAME tenant/provider/domain LaunchGame
	// is launching for. A resolver bug or a future real adapter's own
	// mismatch is caught here rather than silently used.
	if cred.TenantID != params.TenantID || cred.ProviderID != providerID || cred.Domain != "casino" {
		return launchFailed(LaunchFailureCredentialBindingMismatch, fmt.Errorf("%w: outbound credential binding mismatch", ErrProviderUnavailable))
	}

	call := CallContext{
		TenantID: params.TenantID, ProviderID: providerID, Credential: cred,
		IdempotencyKey: "cas:" + session.ID.String(), Deadline: time.Now().Add(defaultLaunchCallTimeout),
	}
	// IO-1B (architect review, INV-IO-1(b)): defence in depth behind the
	// primary API-shape control (no function that can reach
	// CasinoProvider.Launch takes a pgx.Tx) - refuse the adapter outbound
	// call itself if ctx is, despite that, marked as holding a pooled
	// database transaction. See ErrProviderCallRefused's own doc comment
	// (types.go) for why this exists as a SECOND control, not the primary
	// one.
	if txscope.Held(ctx) {
		return launchFailed(LaunchFailureTxHeld, ErrProviderCallRefused)
	}
	result, err := provider.Launch(ctx, LaunchRequest{
		ProviderGameID: providerGameID, PlayerAccountID: params.PlayerAccountID,
		AssetCode: params.AssetCode, Mode: params.Mode, LaunchToken: token, SessionID: session.ID,
		Call: call,
	})
	if err != nil {
		// The launch call itself failed at the transport level - the
		// session was never actually usable, so revoke it rather than
		// leaving an 'active' row a retried launch attempt could never
		// reach (a fresh LaunchGame call mints its own new session instead
		// of trying to reuse this one). Distinguishes a ctx-cancellation-
		// induced transport failure (a client disconnect mid-Launch) from
		// any other transport failure - both revoke and audit identically,
		// but the closed reason code says which, without ever storing the
		// adapter's own error text.
		reason := LaunchFailureProviderUnavailable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			reason = LaunchFailureCtxCancelled
		}
		return launchFailed(reason, fmt.Errorf("provider launch call failed: %w", err))
	}
	if result.Outcome != OutcomeSucceeded {
		return launchFailed(LaunchFailureDeclined, fmt.Errorf("provider declined launch: %s", result.DeclineReason),
			"decline_reason", result.DeclineReason)
	}

	// Phase C success: a second short transaction for the "casino.launched"
	// audit record only - still no transaction held during, or across, the
	// Launch call above. Same ctx-independence as launchFailed (security
	// review RV-PRH-I2 C1): a cancelled request ctx must not silently skip
	// this audit either.
	successCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), phaseCTimeout)
	defer cancel()
	if err := pool.WithTenant(successCtx, params.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		return audit.Record(ctx, tx, audit.Entry{
			TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
			Action: "casino.launched", TargetType: "casino_launch_session", TargetID: session.ID.String(),
			Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"game_id": params.GameID.String(), "provider_id": providerID, "provider_game_id": providerGameID,
				"asset_code": params.AssetCode, "mode": string(params.Mode),
			},
		})
	}); err != nil {
		// The vendor already accepted the launch, so this is NOT a harmless
		// expiry case - the platform cannot prove it audited the accepted
		// launch. Security review RV-PRH-I2 C1 / code review R1: attempt a
		// revoke too (never leave the session silently active with an
		// unaudited "launched" that the caller was just told failed),
		// through the SAME ctx-independent, logged path launchFailed uses.
		return launchFailed(LaunchFailureInternal, fmt.Errorf("%w: audit launch: %v", ErrProviderUnavailable, err))
	}

	return LaunchGameResult{LaunchURL: result.LaunchURL, SessionID: session.ID, ExpiresAt: session.ExpiresAt}, nil
}

// evaluateAndAuditEligibility consults the single authoritative
// rg.EvaluateEligibility policy boundary and, on denial, writes an audit
// record BEFORE returning - so a denial is always visible in the audit
// trail even though (by design, per the directive's §7) it produces no
// ledger effect at all. Shared verbatim by LaunchGame and postBet so
// casino never grows two independent copies of "is this player allowed to
// gamble right now" (ADR 0026 §5's explicit "do not duplicate independent
// RG checks throughout handlers" rule).
//
// Callers decide FOR THEMSELVES how to surface a non-Allowed Decision -
// LaunchGame reports it via LaunchGameResult.Denied/DenialCode (a result
// field, never a Go error - see LaunchGameResult's own doc comment for why
// an earlier error-based attempt was found, by test, to silently roll back
// its own audit record); postBet already has an established decline-
// without-error convention (the identical insufficient-funds case just
// above it) and reports the SAME way, so an RG-declined bet is exactly as
// provider-protocol-normal as an insufficient-funds decline, never a
// transport-level error.
func evaluateAndAuditEligibility(ctx context.Context, tx pgx.Tx, tenantID, brandID, playerAccountID, walletID uuid.UUID, auditAction string) (rg.Decision, error) {
	decision, err := rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{
		TenantID: tenantID, BrandID: brandID, PlayerAccountID: playerAccountID, WalletID: walletID,
	})
	if err != nil {
		return rg.Decision{}, fmt.Errorf("casino: evaluate rg eligibility: %w", err)
	}
	if decision.Allowed {
		return decision, nil
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: auditAction,
		TargetType: "player_account", TargetID: playerAccountID.String(), Outcome: audit.OutcomeDenied,
		Metadata: map[string]any{
			"reason_code": decision.Code, "person_id": decision.PersonID.String(), "brand_id": brandID.String(),
		},
	}); err != nil {
		return rg.Decision{}, fmt.Errorf("casino: audit rg denial: %w", err)
	}
	return decision, nil
}

// resolveLicensingMode resolves tenantID's OWN tenants.licensing_model
// (Stage 4G-FINAL Part D) for a risk.RiskRequest's LicensingMode field -
// risk.Evaluate never looks this up itself (see RiskRequest.LicensingMode's
// doc comment), so every caller resolves it server-side exactly like
// every other identity field already on RiskRequest. Shared by LaunchGame
// and postBet so casino never grows two independent copies of this
// lookup (the same "one shared helper, not duplicated per call site"
// discipline as evaluateAndAuditEligibility/evaluateAndAuditRisk).
func resolveLicensingMode(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (string, error) {
	t, err := identity.GetTenantByID(ctx, tx, tenantID)
	if err != nil {
		return "", fmt.Errorf("casino: resolve tenant licensing mode: %w", err)
	}
	return t.LicensingModel, nil
}

// classifyRiskOutcome maps a RiskDecision.Outcome to what THIS
// enforcement point does with it, keeping ALLOW / REVIEW / DENY /
// error-or-unavailable as four semantically distinct outcomes (ADR 0031
// §34) instead of one "not allow" catch-all:
//
//   - ALLOW -> proceed.
//   - DENY -> a business decision; the caller declines without a Go
//     error, exactly like an insufficient-funds decline.
//   - REVIEW -> also blocks HERE, and that is still the deliberate,
//     DISCLOSED SIMPLIFICATION ADR 0031 §6/§8/§11 records rather than a
//     resolved product decision: no compliance-review queue exists yet to
//     route a review-flagged operation to, so it fails safe by blocking.
//     REVIEW and DENY remain semantically different (§11) and are
//     distinguishable in the audit record's own `outcome` metadata field;
//     a future stage that adds a review queue changes THIS function, not
//     internal/risk's signature or model.
//   - anything else -> not a decision at all. Returns an error so the
//     whole transaction aborts (no ledger effect), and the caller never
//     reports a policy decline for what is really "the platform could not
//     decide". Unreachable through risk.Evaluate, which self-checks its
//     own output; kept because the fourth outcome must have an explicit
//     home rather than falling into DENY's branch by default.
func classifyRiskOutcome(o risk.Outcome) (proceed bool, err error) {
	switch o {
	case risk.OutcomeAllow:
		return true, nil
	case risk.OutcomeDeny, risk.OutcomeReview:
		return false, nil
	default:
		return false, fmt.Errorf("casino: %w: %q", ErrRiskOutcomeUnrecognized, o)
	}
}

// evaluateAndAuditRisk consults the central risk.Evaluate boundary
// (Stage 4G, ADR 0031 §2) and, on a non-ALLOW outcome, writes an audit
// record BEFORE returning - identical shape to evaluateAndAuditEligibility
// immediately above, so casino never grows two independent conventions
// for "consult a policy boundary, audit a denial, let the caller decide
// how to surface it." A non-nil error from risk.Evaluate is NEVER treated
// as ALLOW - it propagates up, aborting the whole transaction (fail-
// closed, ADR 0031 §6), unlike a clean non-ALLOW RiskDecision (a genuine
// business decision, reported the same decline-without-error way
// LaunchGame/postBet already report an RG denial).
func evaluateAndAuditRisk(ctx context.Context, tx pgx.Tx, req risk.RiskRequest, auditAction string) (risk.RiskDecision, error) {
	decision, err := risk.Evaluate(ctx, tx, req)
	if err != nil {
		return risk.RiskDecision{}, fmt.Errorf("casino: evaluate risk policy: %w", err)
	}
	if decision.Outcome == risk.OutcomeAllow {
		return decision, nil
	}
	// Metadata includes enough to investigate WHICH game/provider/stake
	// triggered the denial (casino integration specialist review finding:
	// an earlier version omitted these, unlike the sibling
	// casino_bet.declined insufficient-funds audit record) - never raw
	// rule contents beyond the already-opaque reason code.
	//
	// jurisdiction_code/licensing_mode (canonical-model §9.3's "plus one
	// item", correct today independent of the resolver, RISK §3.2): before
	// this, a denial by a jurisdiction-scoped HARD_LIMIT produced an audit
	// record from which the jurisdiction that actually triggered it could
	// not be recovered. Both fields come straight off req - the SAME
	// values risk.Evaluate itself matched against - never re-derived.
	metadata := map[string]any{
		"reason_code": decision.Code, "outcome": string(decision.Outcome), "operation": string(req.Operation),
		"brand_id": req.BrandID.String(), "provider_id": req.ProviderID, "asset_code": req.AssetCode,
	}
	if req.GameID != uuid.Nil {
		metadata["game_id"] = req.GameID.String()
	}
	if req.Amount != 0 {
		metadata["amount"] = req.Amount
	}
	if req.JurisdictionCode != "" {
		metadata["jurisdiction_code"] = req.JurisdictionCode
	}
	if req.LicensingMode != "" {
		metadata["licensing_mode"] = req.LicensingMode
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: req.TenantID, ActorType: audit.ActorSystem, Action: auditAction,
		TargetType: "player_account", TargetID: req.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
		Metadata: metadata,
	}); err != nil {
		return risk.RiskDecision{}, fmt.Errorf("casino: audit risk denial: %w", err)
	}
	return decision, nil
}

// evaluateJurisdictionBlocklist implements K-3's per-game blocklist
// decision (canonical-model §9.2/§9.3) given an ALREADY-RESOLVED
// jurisdiction.Resolution - factored out of LaunchGame so it can be
// exercised directly, in unit tests, against a GENUINELY resolved
// Resolution obtained via the resolver's own producible tenant_licence
// basis (jurisdiction_blocklist_test.go), since no player-side
// jurisdiction producer exists anywhere in this codebase yet (HDR-J-3
// unanswered, canonical-model §11.3) and Resolution is deliberately
// non-forgeable (no exported constructor exists to fabricate one).
//
// K3-1: blocklist is nil/empty -> the control is not ARMED for this game;
// never denies, regardless of res's outcome (a game with no
// jurisdiction-dependent policy in force must not start denying launches
// the moment jurisdiction resolution exists in the codebase - see
// LaunchGame's own K3-1 doc comment for the full reasoning).
//
// K3-2: armed and res did not resolve -> denies with
// DenialCodeJurisdictionUnresolved, NEVER DenialCodeJurisdictionBlocked -
// these are distinguishable INTERNAL outcomes; K3-6 collapses them into
// one player-facing response only at the HTTP boundary, never here.
func evaluateJurisdictionBlocklist(blocklist []string, res jurisdiction.Resolution) (denied bool, denialCode string, err error) {
	if len(blocklist) == 0 {
		return false, "", nil
	}
	if res.Outcome() != jurisdiction.Resolved {
		return true, DenialCodeJurisdictionUnresolved, nil
	}
	code, err := res.Code()
	if err != nil {
		// Unreachable: Code() is only unreachable for a non-Resolved
		// outcome (jurisdiction.ErrNotResolved's own doc comment), and the
		// check immediately above already confirmed Resolved. Kept as a
		// hard stop rather than a silent fallthrough if it is ever reached.
		return false, "", fmt.Errorf("casino: resolved jurisdiction code: %w", err)
	}
	if containsString(blocklist, code) {
		return true, DenialCodeJurisdictionBlocked, nil
	}
	return false, "", nil
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// roundCorrelationID deterministically derives a stable UUID from
// (tenantID, providerID, roundID) so a bet and its later win, posted by
// two entirely separate ReceiveCallback calls, land under the SAME
// ledger_transactions.correlation_id without requiring a lookup table of
// provider round ids - directive item 13's "sufficient correlation
// context to trace... provider transaction ID". Deterministic (SHA1/v5,
// never randomized) so the same round always maps to the same
// correlation id, computed independently by whichever call happens to
// see it first.
func roundCorrelationID(tenantID uuid.UUID, providerID, roundID string) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(tenantID.String()+":"+providerID+":"+roundID))
}

// acquireProviderTxDeliveryLock takes the L0.1 casino bet-delivery
// advisory lock, keyed on (tenantID, providerID, ref). Shared by postBet
// (ref = its own provider_tx_id), postWin (ref = its own provider_tx_id -
// ADR 0082 Amendment A6's E10 extension, closing the race between a win
// and a concurrent rollback of that SAME still-unseen reference), and
// postRollback (ref = the ORIGINAL provider_tx_id being rolled back - ADR
// 0082 Amendment A6 itself). Factored out so every caller uses the
// IDENTICAL key string postBet originally defined - see postBet's own
// original doc comment (still on the postBet call site) for the full
// deadlock/collision rationale (hashtextextended over the tenant-
// qualified key; a tenant component is required, never optional, because
// provider_tx_id uniqueness is only guaranteed WITHIN one tenant).
//
// Deadlock analysis (ADR 0082 Amendment A6): two transactions contend on
// this lock only for the SAME (tenant, provider, ref) triple, and neither
// holds any other lock when it requests it (it is always the first lock
// each of postBet/postWin/postRollback takes) - so no cycle through this
// lock is possible. It is taken at most once per transaction.
func acquireProviderTxDeliveryLock(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, ref string) error {
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('casino_bet_delivery:' || $1::text || ':' || $2 || ':' || $3, 0))`,
		tenantID, providerID, ref,
	); err != nil {
		return fmt.Errorf("casino: acquire provider-tx delivery lock: %w", err)
	}
	return nil
}

// isProviderTxTombstoned reports whether a tombstone already exists for
// (tenantID, providerID, providerTxID) - the shared E3 (postBet)/E10
// (postWin) check (§1.3). A plain SELECT; the caller is expected to have
// already taken acquireProviderTxDeliveryLock on the SAME ref, which is
// what makes this read race-free against a concurrent postRollback
// writing that exact tombstone (ADR 0082 Amendment A6).
func isProviderTxTombstoned(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, providerTxID string) (bool, error) {
	var found bool
	err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3 AND transaction_type = $4)`,
		tenantID, providerID, providerTxID, ledger.TxTombstone,
	).Scan(&found)
	if err != nil {
		return false, fmt.Errorf("casino: check provider_tx_id tombstone: %w", err)
	}
	return found, nil
}

// requireActiveTenantForNewPosting is the casino entry point of the owner
// decision R3-GAME-POSTINGS-NONACTIVE-1 (2026-10-05, ADR 0095 section 40.5):
// a NEW gameplay financial posting (bet, win, rollback or rollback
// tombstone) for a suspended or closed tenant is refused with
// ErrTenantNotActive. It runs in the posting transaction itself, via the
// shared race-free primitive tenant.RequireActiveForGameplay, AFTER the
// caller's own replay short-circuits and BEFORE its first ledger write.
//
// A replay is a read, not a new movement: when the callback's own
// (provider, provider_tx_id) already exists in the ledger, nothing can be
// posted under it (the database's unique key refuses a second row), so the
// refusal is skipped and the caller's existing replay handling returns the
// original outcome. ownRef is that reference ("" disables the exemption).
func requireActiveTenantForNewPosting(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, ownRef string) error {
	err := tenant.RequireActiveForGameplay(ctx, tx, tenantID)
	if err == nil {
		return nil
	}
	if !errors.Is(err, tenant.ErrNotActiveForGameplay) {
		return fmt.Errorf("casino: check tenant status: %w", err)
	}
	if ownRef != "" {
		var exists bool
		if qerr := tx.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3)`,
			tenantID, providerID, ownRef,
		).Scan(&exists); qerr != nil {
			return fmt.Errorf("casino: check replay of own reference: %w", qerr)
		}
		if exists {
			return nil
		}
	}
	return fmt.Errorf("%w: %w", ErrTenantNotActive, err)
}

// ReceiveCallbackResult is what ReceiveCallback returns for a caller
// (an HTTP handler) that needs to know the outcome without exposing the
// full ledger internals - mirrors internal/payments.ReceiveCallbackResult.
type ReceiveCallbackResult struct {
	Outcome             Outcome
	LedgerTransactionID *uuid.UUID
	DeclineReason       string
	// Tombstoned is true only for a rollback whose original bet/win was
	// never seen (financial-transaction-flows.md §7's tombstone case).
	Tombstoned bool
	// EventType is the dispatched CallbackEvent's own EventType (bet/win/
	// rollback), set once, in one place, by ReceiveCallback's own dispatch
	// switch below - never by postBet/postWin/postRollback themselves, so
	// every one of their return sites (including every short-circuit) gets
	// it uniformly. Exists solely so the webhook handler's
	// "casino_callback_replayed" log line (Replayed, below) can name which
	// kind of callback replayed without re-deriving it from the response
	// shape.
	EventType CallbackEventType
	// Replayed is true whenever THIS delivery short-circuited on an
	// already-recorded fact rather than newly posting/writing one -
	// ledger-finance's R1 recommendation (gate 10.3-W1 re-verification):
	// postBet's E2 idempotency short-circuit, the E9 tombstone
	// short-circuit (both the first-tombstone-write and a later replay of
	// it), postWin/postRollback's ledger.Post AlreadyPosted gate, and
	// postRollbackHeldWin's voided-by-same-reference short-circuit all set
	// it. It never affects outcome, response shape, or any ledger/audit
	// effect - it exists only so the webhook handler can log
	// "casino_callback_replayed" and a first delivery can be told apart
	// from a redelivery without a database join (see webhook_verify.go's
	// caller in internal/httpserver).
	Replayed bool
}

// ReceiveCallback dispatches a verified provider callback: verifies it via
// the named adapter's HandleCallback, then posts Flow 5 (bet), Flow 6
// (win), or Flow 7 (rollback) via ledger.Post.
//
// tenantID/providerID are resolved by the caller (an HTTP handler, from a
// per-tenant webhook path) BEFORE opening tx and BEFORE calling this
// function - never from in.Body. This function OVERWRITES
// in.TenantID/in.ProviderID from its own parameters first (architect
// ruling J5/R3: one tenant id flows from route -> RLS -> resolver ->
// signing input -> credential check -> writes), mirroring
// internal/payments.Orchestrator.ReceiveCallback's identical, already-
// reviewed signature choice and rationale (payment-orchestration.md §3).
//
// Verification order (Stage 10.2, CAS-WH-TENANT-1, ADR 0091, design §C3),
// strictly BEFORE any ledger/session/round read, lock, write, tombstone,
// or audit row (invariant I1 - no tenant-scoped statement runs here before
// (c) succeeds):
//
//	(a) the adapter must be registered;
//	(b) resolve the single candidate credential for
//	    (tenantID, providerID, keyID), then re-check cred.TenantID/
//	    cred.ProviderID equal what was just resolved for - a mismatch
//	    fails closed, never falling back to any other credential;
//	(c) HMAC/signature verification over the raw bytes BEFORE any parsing
//	    (point 7), run by THIS orchestrator via the adapter's
//	    WebhookScheme() (Stage 10.3 W1a, WH-VENDOR-SCHEME-1: the
//	    orchestrator, not the adapter, is the mandatory verifier;
//	    webhook_verify.go), followed by the adapter's HandleCallback
//	    parsing the verified bytes.
//
// Only AFTER (c) succeeds does dispatch happen at all - see (d) below for
// the Stage 10.3 CAS-CAP-ROLLBACK-1 change to what "dispatch" now means:
// the capability check that used to run HERE, for every event type,
// before dispatch, was deleted and replaced with a NEW-BET-ONLY gate
// inside postBet, resolved for the session's own brand (ADR 0025 Stage
// 10.3 amendment). The casino capability remains a deliberate money-path
// kill switch on NEW EXPOSURE (design §C3); it is no longer a kill switch
// on settling exposure that already exists.
//
// Every failure before (c) succeeds returns a *webhookauth.AuthError
// wrapping webhookauth.ErrAuthFailed with a closed, allow-listed reason -
// the caller (an HTTP handler) maps every one of them to the SAME uniform
// 401 response, so an unauthenticated caller can never distinguish
// "unknown provider" from "bad signature" by status code or body.
func (o *Orchestrator) ReceiveVerifiedCallback(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, v *VerifiedCallback) (ReceiveCallbackResult, error) {
	// (a)+(b)+verify ran in phase 1 (VerifyCallback, no transaction held;
	// ADR 0094 §4.1) - no statement of any kind ran in THIS transaction
	// before it. Redeem + Recheck is the first statement here (ADR 0094
	// §5): a revoked/expired/rotated-away handle, a reused or stale token,
	// or a tenant/provider/domain mismatch is the uniform
	// credential_unavailable with nothing read or written.
	provider, in, cred, err := o.redeemVerified(ctx, tx, tenantID, providerID, v)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	keyID := cred.KeyID

	// (c) the adapter's HandleCallback parses the now-VERIFIED bytes (it
	// may re-verify as defence in depth). A re-verification failure is
	// ErrCallbackSignatureInvalid (== webhookauth.ErrSignatureInvalid) -
	// never any other error type - so this switch is exhaustive for
	// pre-verification failures. ErrCallbackMalformedBody (checked last,
	// falling into the generic branch) is a POST-verification, distinct
	// error class the HTTP layer maps to a 4xx, not the uniform 401.
	event, err := provider.HandleCallback(ctx, in, cred)
	if errors.Is(err, ErrCallbackSignatureInvalid) {
		return ReceiveCallbackResult{}, &webhookauth.AuthError{Reason: webhookauth.ReasonSignatureInvalid, KeyID: keyID, CredentialFingerprint: cred.Fingerprint}
	}
	if err != nil {
		// Covers ErrCallbackMalformedBody and any other post-verification
		// structural failure. Never wrap in.Body's bytes into THIS error.
		return ReceiveCallbackResult{}, fmt.Errorf("casino: handle callback: %w", err)
	}

	// PROVIDER-REF-BOUND-1: every provider-supplied reference is bounded
	// BEFORE any domain statement (lock, read, write, tombstone, audit).
	// Deterministic, non-retryable; never truncated.
	if err := validateCallbackReferences(event); err != nil {
		return ReceiveCallbackResult{}, err
	}

	// (d) Stage 10.3 CAS-CAP-ROLLBACK-1 (docs/plans/stage-10.3-planning/
	// 02-casino-financial-analysis.md §1.3/§1.4, ADR 0025 Stage 10.3
	// amendment): the tenant-wide (brand_id NULL) pre-dispatch capability/
	// status check that used to run HERE, for every event type, is
	// DELETED. It made the capability a kill switch on SETTLEMENT as well
	// as on new exposure: disabling it 503'd a verified win (withholding
	// winnings on an already-taken stake), 503'd a verified rollback
	// (stranding that stake), and - worst - 503'd a rollback of an unseen
	// original before it could ever write its tombstone, breaking
	// CLAUDE.md's late-arrival guarantee purely as a function of tenant
	// configuration (F1-F4). The ledger-finance ruling (§1.3): capability
	// and status gate NEW EXPOSURE ONLY; settlement of existing exposure
	// is NEVER blocked by capability, status, a missing flag, or a missing
	// row.
	//
	// A verified win/rollback therefore now dispatches directly,
	// UNCONDITIONALLY with respect to capability - postWin and postRollback
	// each still resolve/act on the round's own ledger truth, never a
	// capability row. The ONLY event this package still gates by
	// capability is a NEW bet (E1), and that gate moved to postBet itself
	// (see its own doc comment), where it can resolve the capability for
	// the SESSION's OWN brand - fixing F4 (a tenant-wide, brandID=uuid.Nil
	// lookup could never match a brand-only row) by resolving it exactly
	// the way LaunchGame already does.
	//
	// Stage 10.3 W2b (CAS-RECON-1): every dispatch result passes through
	// wrapRejection (rejections.go). A verified callback rejected with one
	// of the recorded financial rejection classes comes back as a
	// *CallbackRejectedError carrying the verified event's identifiers, so
	// the caller can write the rejection record in a separately-committed
	// transaction after this one rolls back. errors.Is on the wrapped
	// error is unchanged. This is the only place the wrapper is applied,
	// and it is reached only after verification succeeded (I1).
	switch event.EventType {
	case CallbackEventBet:
		result, err := wrapRejection(providerID, event)(mapReplayPayloadMismatch(o.postBet(ctx, tx, tenantID, providerID, event)))
		result.EventType = event.EventType
		return result, err
	case CallbackEventWin:
		result, err := wrapRejection(providerID, event)(mapReplayPayloadMismatch(o.postWin(ctx, tx, tenantID, providerID, event)))
		result.EventType = event.EventType
		return result, err
	case CallbackEventRollback:
		result, err := wrapRejection(providerID, event)(mapReplayPayloadMismatch(o.postRollback(ctx, tx, tenantID, providerID, event)))
		result.EventType = event.EventType
		return result, err
	default:
		// Stage 10.2 final review (K10/L6): an unknown event type reaching
		// this point is a verified-but-malformed callback (the adapter's own
		// parsing is expected to reject it first, as MockCasinoProvider's
		// does - this is defense in depth for a future adapter that doesn't).
		// It must map to ErrCallbackMalformedBody -> 400 like every other
		// post-verification structural failure, never an unwrapped error
		// that falls through to a 500 for a verified caller.
		return ReceiveCallbackResult{}, fmt.Errorf("%w: unsupported callback event type %q", ErrCallbackMalformedBody, event.EventType)
	}
}

// validateCallbackReferences applies the platform provider-reference bound
// (internal/providerref) to every provider-supplied identifier of a
// verified casino callback. Required-ness stays the adapter's/postX's own
// decision (an absent optional field is accepted here); only a PRESENT
// value must satisfy the bound. The returned error wraps both
// ErrProviderReferenceInvalid and the *providerref.Error (never the
// value).
func validateCallbackReferences(event CallbackEvent) error {
	err := providerref.ValidateAll(
		providerref.Field{Name: "provider_tx_id", Value: event.ProviderTxID, Required: true},
		providerref.Field{Name: "original_provider_tx_id", Value: event.OriginalProviderTxID},
		providerref.Field{Name: "round_id", Value: event.RoundID},
		providerref.Field{Name: "provider_game_id", Value: event.ProviderGameID},
		providerref.Field{Name: "asset_code", Value: event.AssetCode},
	)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrProviderReferenceInvalid, err)
	}
	return nil
}

// mapReplayPayloadMismatch turns ledger.ErrIdempotencyPayloadMismatch from
// any casino posting (a win or rollback whose provider_tx_id is already
// posted with a different entry set, reversal link or round - audit sites
// #7, #9-#13) into this package's ErrProviderTxPayloadMismatch, so the HTTP
// layer maps every such case to one integrity alert and a 409. The ledger
// error stays wrapped for logs; nothing was posted.
func mapReplayPayloadMismatch(result ReceiveCallbackResult, err error) (ReceiveCallbackResult, error) {
	if errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) && !errors.Is(err, ErrProviderTxPayloadMismatch) {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: %w", ErrProviderTxPayloadMismatch, err)
	}
	return result, err
}

// verifyPostedBetMatchesEvent is postBet's caller-level replay comparison
// (Stage 10 F-7 remediation, audit site #6): the idempotency short-circuit
// never reaches ledger.Post, so ledger.Post's own payload comparison
// cannot protect it. A redelivery is accepted as a replay only if it
// describes the posted bet: the same stake (the sum of the posting's
// debit legs on player-owned accounts), the same asset on every player-
// owned leg, the same round (correlation_id) and the same wallet (the one
// event.SessionID resolves to). Anything else is
// ErrProviderTxPayloadMismatch, naming only the differing field classes.
// Plain SELECTs; takes no lock.
func verifyPostedBetMatchesEvent(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, postedID uuid.UUID, event CallbackEvent) error {
	var diffs []string

	var correlationID uuid.UUID
	if err := tx.QueryRow(ctx,
		`SELECT correlation_id FROM ledger_transactions WHERE tenant_id = $1 AND id = $2`, tenantID, postedID,
	).Scan(&correlationID); err != nil {
		return fmt.Errorf("casino: load posted bet for replay comparison: %w", err)
	}
	if correlationID != roundCorrelationID(tenantID, providerID, event.RoundID) {
		diffs = append(diffs, "round")
	}

	rows, err := tx.Query(ctx,
		`SELECT la.wallet_id, la.asset_code, le.direction, le.amount::text
		   FROM ledger_entries le JOIN ledger_accounts la ON la.id = le.ledger_account_id
		  WHERE le.ledger_transaction_id = $1 AND la.wallet_id IS NOT NULL`, postedID)
	if err != nil {
		return fmt.Errorf("casino: load posted bet entries for replay comparison: %w", err)
	}
	defer rows.Close()
	var postedWallet uuid.UUID
	walletConsistent, assetMatches := true, true
	stake := new(big.Int)
	for rows.Next() {
		var walletID uuid.UUID
		var asset, amountText string
		var direction ledger.Direction
		if err := rows.Scan(&walletID, &asset, &direction, &amountText); err != nil {
			return fmt.Errorf("casino: scan posted bet entry for replay comparison: %w", err)
		}
		if postedWallet == uuid.Nil {
			postedWallet = walletID
		} else if walletID != postedWallet {
			walletConsistent = false
		}
		if asset != event.AssetCode {
			assetMatches = false
		}
		if direction == ledger.Debit {
			amount, ok := new(big.Int).SetString(amountText, 10)
			if !ok {
				return fmt.Errorf("casino: unparseable posted bet amount for replay comparison")
			}
			stake.Add(stake, amount)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("casino: read posted bet entries for replay comparison: %w", err)
	}
	if stake.Cmp(big.NewInt(event.Amount)) != 0 {
		diffs = append(diffs, "amount")
	}
	if !assetMatches {
		diffs = append(diffs, "asset")
	}

	sessionMatches := walletConsistent && postedWallet != uuid.Nil && event.SessionID != uuid.Nil
	if sessionMatches {
		session, err := GetLaunchSessionByID(ctx, tx, event.SessionID)
		switch {
		case errors.Is(err, ErrLaunchSessionNotFound):
			sessionMatches = false
		case err != nil:
			return err
		default:
			sessionMatches = session.ProviderID == providerID && session.WalletID == postedWallet
		}
	}
	if !sessionMatches {
		diffs = append(diffs, "session")
	}

	if len(diffs) > 0 {
		return fmt.Errorf("%w: posted bet %s differs in [%s]", ErrProviderTxPayloadMismatch, postedID, strings.Join(diffs, ","))
	}
	return nil
}

// validateCallbackEvent is shared by postBet/postWin (postRollback has its
// own, narrower validation - a rollback carries no Outcome of its own).
func validateCallbackEvent(event CallbackEvent) error {
	if event.ProviderTxID == "" {
		return fmt.Errorf("%w: provider_tx_id is required", ErrInvalidInput)
	}
	if event.PlayerAccountID == uuid.Nil {
		return fmt.Errorf("%w: player_account_id is required", ErrInvalidInput)
	}
	if event.AssetCode == "" {
		return fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if event.Amount <= 0 {
		return fmt.Errorf("%w: amount must be positive, got %d", ErrInvalidInput, event.Amount)
	}
	// Specialist review (qa) finding: this field was parsed from the
	// signed payload but never enforced, so a provider-declared decline/
	// ambiguous outcome was silently posted as a real financial effect
	// regardless. A bet/win callback whose own outcome disagrees with
	// "this is a real financial effect" is rejected outright, never
	// posted and never silently treated as a success.
	if event.Outcome != OutcomeSucceeded {
		return fmt.Errorf("%w: got %q", ErrOutcomeNotSucceeded, event.Outcome)
	}
	return nil
}

// findPostedBetTransaction looks up whether a casino_bet transaction has
// already been posted for (providerID, providerTxID) under tenantID - see
// postBet's own idempotency-short-circuit doc comment for why this check
// exists ahead of RG/balance evaluation, not merely inside ledger.Post's
// own conflict handling. A DECLINED bet (RG-denied or insufficient-funds)
// intentionally posts no row at all (Flow 5's own design), so it is
// correctly NOT found here and a redelivery of a genuinely-declined
// attempt is re-evaluated fresh each time against current state - only a
// SUCCEEDED post is idempotent-replayed verbatim.
func findPostedBetTransaction(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID, providerTxID string) (id uuid.UUID, found bool, err error) {
	err = tx.QueryRow(ctx,
		`SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = $2 AND provider_id = $3 AND provider_tx_id = $4`,
		tenantID, ledger.TxCasinoBet, providerID, providerTxID,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, fmt.Errorf("casino: check existing bet transaction: %w", err)
	}
	return id, true, nil
}

// postBet implements Flow 5 (financial-transaction-flows.md §5): debit
// player_cash, credit house_gaming. Player_bonus-funded stakes are
// explicitly out of scope this stage (ADR 0025 §6) - every bet here is
// assumed 100% player_cash-funded.
//
// The wallet a bet debits is resolved from the platform's OWN
// casino_launch_sessions row (event.SessionID), never from
// event.PlayerAccountID directly - specialist review finding (security/
// multi-tenancy/architect, independently): a payload-supplied player id
// with no session binding lets a validly-signed provider debit an
// arbitrary player in the tenant with no record a launch ever happened,
// and cannot distinguish a demo round from a real-money one. A session
// resolved here that is revoked, or was minted in demo mode, is rejected
// outright - a demo round must never post a real financial effect.
func (o *Orchestrator) postBet(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent) (ReceiveCallbackResult, error) {
	if err := validateCallbackEvent(event); err != nil {
		return ReceiveCallbackResult{}, err
	}
	// Explicit, postBet-level requirement (Stage 8 review finding): a bet
	// is bound to a provider round later in this function (BindProviderRound),
	// which has its own round-id validation - but that requirement must be
	// visible and enforced here too, not only surface implicitly the first
	// time BindProviderRound happens to be reached. round_id is otherwise
	// not part of validateCallbackEvent (postWin/postRollback have their own
	// distinct round_id requirements and share that function).
	if event.RoundID == "" {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: round_id is required to post a bet", ErrInvalidInput)
	}

	// Stage 4G-FINAL flake investigation (TestConcurrent_DuplicateBetDeliveryDuringSelfExclusion):
	// the idempotency short-circuit immediately below only serializes
	// SEQUENTIAL redeliveries (one delivery's transaction commits before
	// the next one's find-query runs). Two GENUINELY CONCURRENT deliveries
	// of the identical provider_tx_id can each start before the other
	// commits, so both see "not yet posted" and each independently
	// re-evaluates live RG/Risk state below - if that state changes
	// between the two evaluations (e.g. a self-exclusion becomes
	// effective mid-race), the two deliveries can return DIFFERENT
	// outcomes for what is, from the provider's perspective, the exact
	// same bet - even though ledger.Post's own idempotency key still
	// guarantees at most one financial effect is ever posted. A provider
	// that treats "declined" as "no stake was taken" would then disagree
	// with the ledger's own truth about whether this round's stake was
	// collected. This lock forces a second, truly-concurrent delivery to
	// wait for the first's transaction to fully commit (or roll back)
	// before proceeding - by the time it re-reads below, the first
	// delivery's outcome (posted or not) is settled and visible, so the
	// two deliveries' results can never diverge. hashtextextended (not
	// hashtext) for the full 64-bit lock-key space - see
	// internal/reconciliation.TryRunLedgerVsProjectionForTenant's
	// identical rationale for why a single 32-bit hashtext component is
	// an unacceptable collision risk for a shared advisory-lock
	// namespace. Scoped to (tenantID, providerID, provider_tx_id) - the
	// tenant component is required, not optional: provider_tx_id
	// uniqueness is only ever guaranteed WITHIN one tenant (migration
	// 0021's own "a platform-global unique key on a tenant-partitioned,
	// RLS-protected table is a cross-tenant collision risk" rule, applied
	// identically to a lock key - adversarial review finding), so two
	// different tenants sharing a provider with overlapping
	// provider_tx_id values would otherwise serialize against each
	// other. Otherwise scoped narrowly enough (one specific bet, one
	// specific tenant) that it adds no contention beyond the exact case
	// it exists to fix.
	if err := acquireProviderTxDeliveryLock(ctx, tx, tenantID, providerID, event.ProviderTxID); err != nil {
		return ReceiveCallbackResult{}, err
	}

	// Idempotency short-circuit, BEFORE session/RG/balance evaluation:
	// financial-transaction-flows.md §5 requires an exact retry (same
	// provider_tx_id) of an ALREADY-POSTED bet to be "an idempotent no-op
	// returning the original result" - never re-evaluated against
	// whatever the platform's live state happens to be at redelivery
	// time. Without this check, a bet that legitimately succeeded, then
	// redelivered after the player later self-excluded (self-exclusion is
	// effectively permanent - EVERY future redelivery would be affected,
	// not just a narrow timing window) or after the wallet balance
	// changed, would incorrectly report "declined" for a stake that was
	// already taken - a provider that treats "declined" as "stake never
	// taken" could then void the round and never deliver its own win
	// callback for a round the platform already debited (financial
	// correctness specialist review finding, empirically reproduced).
	// ledger.Post's OWN idempotency key would also no-op a redelivery
	// that reaches it - this check exists so a redelivery never even
	// reaches the RG/balance checks, which must only ever evaluate a
	// bet's FIRST delivery.
	if existingID, found, err := findPostedBetTransaction(ctx, tx, tenantID, providerID, event.ProviderTxID); err != nil {
		return ReceiveCallbackResult{}, err
	} else if found {
		// Stage 10 F-7 (audit site #6): a redelivery is only a replay if
		// it describes the bet already posted - see
		// verifyPostedBetMatchesEvent.
		if err := verifyPostedBetMatchesEvent(ctx, tx, tenantID, providerID, existingID, event); err != nil {
			return ReceiveCallbackResult{}, err
		}
		return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &existingID, Replayed: true}, nil
	}

	// R3-GAME-POSTINGS-NONACTIVE-1: past the replay short-circuit this bet
	// would be a NEW stake. Refused for a suspended/closed tenant, in this
	// transaction, before any session/RG/risk evaluation or write.
	if err := requireActiveTenantForNewPosting(ctx, tx, tenantID, providerID, ""); err != nil {
		return ReceiveCallbackResult{}, err
	}

	// Stage 10.3 CAS-CAP-ROLLBACK-1, E3 (§1.3/§1.4 step 2): a rollback for
	// THIS exact provider_tx_id was already accepted while this reference
	// itself had never been posted (F8's late-original-after-tombstone
	// defect - previously an untyped unique-violation error surfacing as
	// an unhelpful, endlessly-retried 500). L0.1 above is taken on this
	// SAME reference, so this plain SELECT cannot race a concurrent
	// postRollback's own tombstone-write path for the identical reference
	// (ADR 0082 Amendment A6) - either this bet's transaction commits
	// first and the rollback later finds a real casino_bet to reverse
	// (E7), or the tombstone commits first and is visible here.
	//
	// Reported as a DECLINE, never a Go error (postBet's own established
	// insufficient-funds/RG-denial convention just below) - not "the
	// platform could not decide", but a genuine, deterministic, non-
	// retryable business outcome: the stake was never taken because this
	// exact reference is already known to have been rolled back. Using the
	// decline path (rather than a returned error) lets its own audit
	// record commit in THIS transaction, closing F12's disclosed gap for
	// this one case (note B, §1.3) - a returned error would roll the whole
	// transaction back, including its own audit row.
	if tombstoned, err := isProviderTxTombstoned(ctx, tx, tenantID, providerID, event.ProviderTxID); err != nil {
		return ReceiveCallbackResult{}, err
	} else if tombstoned {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_bet.rejected_tombstoned",
			// TargetType/TargetID name what this rejection is ABOUT - the
			// provider's own bet reference - not a ledger_transaction row,
			// since this decline path posts NOTHING (no transaction id
			// exists to point at). Previously mislabelled TargetType as
			// "ledger_transaction" while TargetID actually held a provider
			// reference, not a ledger_transactions.id (cleanup item, gate
			// 10.3-W1 code review #cosmetic, ledger-finance F-7).
			TargetType: "casino_provider_tx", TargetID: providerID + ":" + event.ProviderTxID, Outcome: audit.OutcomeFailure,
			Metadata: map[string]any{
				"provider_id": providerID, "provider_tx_id": event.ProviderTxID, "round_id": event.RoundID,
				"reason": "original_rolled_back",
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: audit bet rejected tombstoned: %w", err)
		}
		// Stage 10.3 W2b (CAS-RECON-1): the durable rejection record, in
		// THIS committing transaction (the decline commits, so the row
		// commits with it). Once per key - a redelivered E3 adds a second
		// audit row (per-attempt, by rule) but never a second rejection
		// row. A failure here rolls the decline back too, exactly like a
		// failure of the audit write above.
		if _, err := RecordCallbackRejection(ctx, tx, tenantID, providerID,
			newCallbackRejection(RejectionOriginalTombstoned, event), observability.RequestIDFromContext(ctx)); err != nil {
			return ReceiveCallbackResult{}, err
		}
		return ReceiveCallbackResult{Outcome: OutcomeDeclined, DeclineReason: "original_rolled_back"}, nil
	}

	if event.SessionID == uuid.Nil {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: session_id is required", ErrLaunchSessionRequired)
	}
	session, err := GetLaunchSessionByID(ctx, tx, event.SessionID)
	if errors.Is(err, ErrLaunchSessionNotFound) {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: session_id does not resolve to a known launch session", ErrLaunchSessionRequired)
	}
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if session.ProviderID != providerID {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: session belongs to a different provider", ErrLaunchSessionRequired)
	}
	// Security review RV-PRH-I2 (I3/item 2, casino/ledger-finance
	// follow-up on ADR 0095 §15.1's "expires, harmless" argument), CORRECTED
	// per code-reviewer FH-7 re-review (CAS-SESSION-EXPIRY-1, 2026-09-28):
	// a NEW bet is only ever accepted against a session that is 'active' or
	// 'consumed' (the two states a normal, in-progress round can be in -
	// 'consumed' is the ordinary post-token-bootstrap state, not an edge
	// case). An allow-list (rather than "reject only 'revoked'") fails
	// closed on 'expired' and on any future status this package doesn't
	// know about yet, not just the ones named today.
	//
	// `expires_at` bounds only the UN-CONSUMED launch TOKEN's own
	// resolvability window (DefaultLaunchTokenTTL, launch.go) - it is set
	// once at mint and is immutable (migration 0036/0042). It was never
	// meant to bound how long an in-play (consumed) round may keep
	// betting: the original item-2 fix applied it to BOTH statuses, which
	// meant every real-money round stopped accepting bets ~
	// DefaultLaunchTokenTTL (2 minutes) after launch, regardless of actual
	// play. Fixed: the expiry check applies ONLY to a session that was
	// NEVER consumed - this is what makes §15.1's "an orphaned/never-
	// resolved active session is harmless" claim actually true, without
	// also time-boxing genuine in-play sessions. A 'consumed' session
	// remains bet-eligible for as long as it stays 'consumed': it is NOT
	// time-bounded, and RevokeLaunchSession (launchFailed below) matches only
	// status='active', so a failed launch does not revoke a consumed session
	// either - that gap is CAS-REVOKE-CONSUMED-1 (blocked by the 0036/0042
	// immutability trigger). The status allow-list still rejects 'revoked'
	// and 'expired'.
	//
	// postWin/postRollback are UNAFFECTED either way - they resolve their
	// accounts from the ledger's own prior entries (correlation_id), never
	// from this session lookup, so a bet placed BEFORE expiry still
	// settles (win/rollback) after the session has since expired.
	if session.Status != LaunchSessionActive && session.Status != LaunchSessionConsumed {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: session is not eligible to accept a new bet (status=%s)", ErrLaunchSessionRequired, session.Status)
	}
	if session.Status == LaunchSessionActive && time.Now().UTC().After(session.ExpiresAt) {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: session has expired", ErrLaunchSessionRequired)
	}
	if session.Mode != ModeReal {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: session is demo-mode, cannot post a real financial effect", ErrLaunchSessionRequired)
	}
	if session.AssetCode != event.AssetCode {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: asset_code does not match the launch session", ErrInvalidInput)
	}

	// Stage 10.3 CAS-CAP-ROLLBACK-1 (§1.3/§1.4 step 2.5): the ONLY capability
	// gate a callback ever passes through, and it applies to a NEW BET
	// only - never to a win, rollback, replay (E2, handled above), or
	// tombstone (E3, handled above). Resolved for the SESSION's OWN brand
	// (session.BrandID), never brandID=uuid.Nil - this is what fixes F4: a
	// tenant-wide lookup could never match a brand-only capability row,
	// while LaunchGame itself always resolved the real brand. A plain
	// SELECT (LoadCapability takes no lock class), positioned BEFORE the
	// L0.2 advisory lock below, so a rejected bet never takes the player
	// lock - the ADR 0082 order is unchanged.
	//
	// Capability states, resolved once here (§1.3):
	//   - S-none (no row at all for tenant/brand/provider): reject. A
	//     brand with no capability row configured is treated identically
	//     to an explicitly disabled one - "no configuration" is never
	//     silently "allowed" for a money-moving gate.
	//   - S-off (status=disabled): reject.
	//   - S-nobet (status=active, supports_bet=false): reject.
	//   - S-on (status=active, supports_bet=true; migration 0094's CHECK
	//     guarantees this implies supports_win AND supports_rollback):
	//     proceed to the normal Flow 5 checks below.
	//   - Additionally: the session's own asset_code must still be in the
	//     capability's supported_assets - the capability may have narrowed
	//     since this session was launched.
	//
	// The rejection shape is UNCHANGED (ErrProviderUnavailable -> 503):
	// financially a 503 and a definitive decline are equivalent (neither
	// posts), and keeping 503 here means no API/OpenAPI change for the bet
	// path itself (§1.3 note A). A provider that retries after re-enable is
	// safe (E2 idempotency); a provider that cancels instead sends a
	// rollback, which hits E8 (always tombstones, regardless of capability
	// state - see postRollback).
	capability, found, err := LoadCapability(ctx, tx, tenantID, session.BrandID, providerID)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if !found || capability.Status != CapabilityActive || !capability.SupportsBet || !containsString(capability.SupportedAssets, event.AssetCode) {
		return ReceiveCallbackResult{}, ErrProviderUnavailable
	}

	// ADR 0082 §3.3/§4.2, class L0.2 - closes finding LOCK-1d. This bet
	// takes its wallet_balance_projection locks below and THEN, after
	// posting, acquires a grant advisory lock for the cash-funded
	// wagering contribution; bonus.ConvertGrant does the exact reverse
	// (grant advisory lock, then player_cash inside ledger.Post). That is
	// a genuine ABBA across a row lock and an advisory lock, and no
	// amount of projection-row sorting touches it. Serializing both
	// behind this one player-scoped advisory lock, taken FIRST, removes
	// the cycle while leaving the contribution inside the bet's own
	// transaction where HR-10 requires it.
	//
	// Positioned immediately after the L0.1 delivery lock's own session
	// resolution and validation (it needs session.PlayerAccountID) and
	// before RG/Risk, so the whole remainder of this function runs under
	// it - including the post-Post contribution block, which is then
	// simply reentrant under a lock this transaction already holds.
	if err := bonus.AdvisoryLockPlayerBonusScope(ctx, tx, tenantID, session.PlayerAccountID); err != nil {
		return ReceiveCallbackResult{}, err
	}

	wl, err := wallet.GetByID(ctx, tx, session.WalletID)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve wallet: %w", err)
	}

	// Stage 4D-RG: re-evaluate eligibility HERE, independently of whatever
	// LaunchGame decided when the session was minted - a session can span an
	// arbitrarily long round, and a self-exclusion/suspension applied
	// mid-round must still stop the NEXT bet from posting (directive §7/§8:
	// "a player cannot place a new bet when platform policy prohibits
	// gambling... checked BEFORE the financial debit is committed"). No
	// ledger entry has been touched yet at this point - a denial here
	// produces zero financial effect, never a partial one.
	decision, err := evaluateAndAuditEligibility(ctx, tx, tenantID, session.BrandID, session.PlayerAccountID, wl.ID, "casino_bet.denied_by_rg_policy")
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if !decision.Allowed {
		return ReceiveCallbackResult{Outcome: OutcomeDeclined, DeclineReason: decision.Code}, nil
	}

	// Stage 4G: the central Risk & Limits boundary - re-evaluated on
	// every bet, exactly like RG eligibility just above, since a limit
	// (or the cumulative usage it aggregates) can change between one bet
	// and the next within the same launch session. No ledger entry has
	// been touched yet at this point - a denial here produces zero
	// financial effect. This call sits AFTER the idempotency short-
	// circuit at the top of this function (financial-transaction-
	// flows.md's own requirement, directive §27): a redelivered
	// already-posted bet returns there and never reaches risk evaluation
	// a second time, so a cumulative rule's aggregate state changing
	// between the original delivery and a retry can never flip an
	// already-succeeded bet's own outcome.
	//
	// JurisdictionCode now comes from THIS SAME session's own persisted
	// value (migration 0042, Stage 4G-FINAL Part C) - the exact
	// jurisdiction LaunchGame itself resolved and evaluated risk against
	// when the round began, denormalized onto casino_launch_sessions
	// exactly like ProviderGameID/AssetCode already were. Empty only when
	// LaunchGame itself had no resolved jurisdiction to persist
	// (TODO(jurisdiction) still applies at its root cause - no per-player
	// jurisdiction resolution exists yet anywhere in this codebase) -
	// this closes the previously-disclosed "reachable from LaunchGame but
	// never from postBet" gap (ADR 0031 §8/§9) without inventing a new,
	// independent per-bet jurisdiction source.
	//
	// LicensingMode is resolved fresh here (Stage 4G-FINAL Part D),
	// exactly like at LaunchGame - never cached on the session, since a
	// tenant's own licensing_model is a slow-changing platform-registry
	// fact, not a round-specific one.
	licensingMode, err := resolveLicensingMode(ctx, tx, tenantID)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	riskDecision, err := evaluateAndAuditRisk(ctx, tx, risk.RiskRequest{
		TenantID: tenantID, BrandID: session.BrandID, PlayerAccountID: session.PlayerAccountID,
		Operation: risk.OperationCasinoBet, Product: "casino", ProviderID: providerID, GameID: session.GameID, AssetCode: event.AssetCode,
		JurisdictionCode: session.JurisdictionCode, LicensingMode: licensingMode,
		Amount: event.Amount, CorrelationID: roundCorrelationID(tenantID, providerID, event.RoundID),
	}, "casino_bet.denied_by_risk_policy")
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	// Four distinct outcomes, same shared classification as LaunchGame
	// above (ADR 0031 §34): DENY/REVIEW are business decisions reported
	// through this package's established decline-without-error
	// convention; an unrecognized outcome is not a decision at all and
	// fails closed as an error, so a provider is never told "the player's
	// limits declined this" when the truth is "the platform could not
	// decide".
	proceed, err := classifyRiskOutcome(riskDecision.Outcome)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if !proceed {
		return ReceiveCallbackResult{Outcome: OutcomeDeclined, DeclineReason: riskDecision.Code}, nil
	}

	// ADR 0096 §2.4/§3.5 (PRH-I3): KYC "play" trigger, appended as a
	// third, final read-only step after RG and Risk, before the balance
	// lock/ledger post - the exact position ADR 0031 §7 already documents
	// RG/Risk occupying. Default (no active 'play' policy for this
	// jurisdiction) is not_required/allow, so this is a no-op read on
	// every path until a jurisdiction authors one (HD-KYC-8). A deny here
	// reuses the SAME OutcomeDeclined/DeclineReason shape RG/Risk/
	// insufficient-funds/tombstone denials already use uniformly at this
	// call site - not a new provider-visible outcome variant (casino
	// review condition 1, ADR 0096 §11).
	kycParams := kyc.EnforcementParams{
		TenantID: tenantID, BrandID: session.BrandID, PlayerAccountID: session.PlayerAccountID,
		PersonID: decision.PersonID, Operation: kyc.EnforcementCasinoPlay, AssetCode: event.AssetCode,
		CorrelationID: roundCorrelationID(tenantID, providerID, event.RoundID),
	}
	kycDecision, err := kyc.EvaluateEnforcement(ctx, tx, kycParams)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: evaluate kyc enforcement: %w", err)
	}
	if !kycDecision.Allowed {
		if err := kyc.RecordDecision(ctx, tx, kycParams, kycDecision); err != nil {
			return ReceiveCallbackResult{}, err
		}
		return ReceiveCallbackResult{Outcome: OutcomeDeclined, DeclineReason: kycDecision.Code}, nil
	}
	// A decision row is written for every REAL evaluation (a jurisdiction
	// actually has an active 'play' policy) but deliberately NOT for the
	// overwhelmingly common not_required/dormant path (no active policy
	// anywhere) - §7.7's +2ms p95 hot-path budget is stated against the
	// SELECT-only cost; an unconditional per-bet INSERT into
	// kyc_enforcement_decisions for a fact that never varies (no policy
	// configured) would be a disclosed, avoidable regression this
	// implementation does not accept without profiling evidence. This is
	// a deliberate, disclosed narrowing of §7.6's "every EvaluateEnforcement
	// call writes exactly one decision row" requirement for the play
	// surface only - flagged for qa/security re-review, not silently
	// applied.
	if kycDecision.Outcome != kyc.OutcomeNotRequired {
		if err := kyc.RecordDecision(ctx, tx, kycParams, kycDecision); err != nil {
			return ReceiveCallbackResult{}, err
		}
	}

	// ADR 0082 §3.2/§4.2: resolved through GetOrCreateAccounts so the
	// ledger_accounts unique-index insertion waits happen in canonical
	// (wallet, account_type, asset) order, never this call site's
	// argument order.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: &wl.ID, AccountType: ledger.AccountPlayerCash, AssetCode: event.AssetCode},
		ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: event.AssetCode},
	)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve bet ledger accounts: %w", err)
	}
	cashAccountID, houseAccountID := accounts[0], accounts[1]

	// The COMPLETE, final posting this bet will make, built BEFORE the
	// balance check so the pre-lock below covers every account it touches
	// (ADR 0082 R3: pre-locking a subset - which is exactly what the
	// deleted lockCashBalance did, locking only player_cash out of
	// {player_cash, house_gaming} - is the LOCK-1 bug itself). The very
	// same value is handed to ledger.Post below; it is never rebuilt.
	betInput := ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxCasinoBet,
		// Namespaced by providerID - see the identical rationale on
		// internal/payments' deposit-posting idempotency key: nothing in
		// the CasinoProvider contract guarantees provider_tx_id
		// uniqueness ACROSS providers.
		IdempotencyKey: providerID + ":" + event.ProviderTxID,
		ProviderID:     &providerID, ProviderTxID: &event.ProviderTxID,
		CorrelationID: roundCorrelationID(tenantID, providerID, event.RoundID),
		Entries: []ledger.EntryInput{
			{LedgerAccountID: cashAccountID, Direction: ledger.Debit, Amount: event.Amount},
			{LedgerAccountID: houseAccountID, Direction: ledger.Credit, Amount: event.Amount},
		},
	}

	// Invariant #15: lock and check the balance INSIDE this transaction,
	// immediately before posting - a concurrent bet against the same
	// wallet cannot both observe "sufficient" (financial-transaction-
	// flows.md §5's "insufficient funds -> rejected before posting,
	// checked in the same DB transaction that would post it").
	//
	// ADR 0082 R1/R4: the lock is taken by internal/ledger, over ALL of
	// this posting's projection rows at once, in canonical ascending
	// ledger_account_id order - not by a projection FOR UPDATE of this
	// package's own, which R4 forbids and which was the LOCK-1 cycle's
	// first edge. Kept in exactly its previous position (after RG and
	// Risk, before BindProviderRound) so R8 and the existing "no ledger
	// entry touched yet at decline time" property both still hold: the
	// pre-lock writes no entry, and the zero-totals projection row R6
	// materialises rolls back with a declined transaction and is in any
	// case an account this bet was about to use.
	locked, err := ledger.LockProjectionsForPosting(ctx, tx, betInput)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: lock bet projections: %w", err)
	}
	cashBalance, err := locked.Balance(cashAccountID)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: read locked player_cash balance: %w", err)
	}
	available := cashBalance.Signed()
	if available < event.Amount {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_bet.declined",
			TargetType: "wallet", TargetID: wl.ID.String(), Outcome: audit.OutcomeFailure,
			Metadata: map[string]any{
				"provider_id": providerID, "provider_tx_id": event.ProviderTxID, "amount": event.Amount,
				"asset_code": event.AssetCode, "decline_reason": "insufficient_funds",
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: audit bet decline: %w", err)
		}
		return ReceiveCallbackResult{Outcome: OutcomeDeclined, DeclineReason: "insufficient_funds"}, nil
	}

	// Stage 8 (docs/decisions/0080-provider-integration-readiness-without-
	// external-contracts.md, Decision 1): durably bind this provider-
	// declared round id to the resolved session's own brand/player/game
	// identity, the first time this round is bet on (idempotent on every
	// later bet of the same round - see BindProviderRound's own doc
	// comment). No separate "provider session id" concept exists on
	// CallbackEvent yet, so that field is passed nil - honest about what is
	// not knowable from this callback shape today, never guessed. A
	// conflict (this provider_round_id already bound to a DIFFERENT
	// player/brand in this tenant) aborts the whole callback with no
	// ledger effect - the exact cross-player/cross-brand round-id
	// collision this stage requires be rejected, not silently overwritten -
	// by returning the error as-is so the enclosing db.Pool.WithTenant
	// transaction rolls back everything.
	//
	// Deliberately positioned HERE - immediately before the financial
	// posting below, AFTER RG eligibility, Risk, and the insufficient-funds
	// check have all already passed - and NOT earlier alongside the
	// session/asset-match checks. All three of those declines return
	// (OutcomeDeclined, nil error): a bet this transaction is never actually
	// going to post must never bind a round, or a DECLINED delivery would
	// still durably claim the round id, potentially pre-empting the
	// legitimate bet that (re)tries it (specialist review finding - qa).
	//
	// ADR 0082 named exception E-2 (§5.1a): casino_provider_rounds is a
	// class L1 domain state row, and its lock is therefore taken here
	// AFTER the class L3 pre-lock above - an inversion of the canonical
	// order, kept deliberately for the reason immediately above. It is
	// safe only because postBet is the SOLE writer of that table
	// (INV-LOCK-E2), so no transaction anywhere holds a
	// casino_provider_rounds row lock and then waits on a projection lock,
	// and two concurrent postBets take L3-then-L1 in the same order as
	// each other. A second writer of casino_provider_rounds must resolve
	// E-2 first - see §5.1a for what resolving it requires.
	if err := BindProviderRound(ctx, tx, tenantID, session.BrandID, session.PlayerAccountID, session.ID, session.GameID,
		providerID, event.RoundID, nil); err != nil {
		return ReceiveCallbackResult{}, err
	}

	// The SAME betInput the pre-lock above was computed from - never a
	// rebuilt one, or the pre-lock would be a lock over a different
	// account set than the one actually posted (ADR 0082 R3).
	postResult, err := ledger.Post(ctx, tx, betInput)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: post bet: %w", err)
	}

	// R2 (ledger-finance re-verification after fix round A, gate 10.3-W1):
	// gated on !AlreadyPosted for structural consistency with postWin/
	// postRollback's identical guard, even though AlreadyPosted is
	// unreachable here today - L0.1 (acquireProviderTxDeliveryLock) plus
	// the E2 idempotency short-circuit above both return long before this
	// line on any redelivery, so ledger.Post never sees a second attempt
	// at the same provider_tx_id. Gating anyway makes "postings are
	// audited once per fact" a property of every posting site, not one
	// that depends on an upstream short-circuit never being removed or
	// reordered by a future change.
	if !postResult.AlreadyPosted {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_bet.posted",
			TargetType: "ledger_transaction", TargetID: postResult.TransactionID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": providerID, "provider_tx_id": event.ProviderTxID, "round_id": event.RoundID,
				"amount": event.Amount, "asset_code": event.AssetCode, "already_posted": postResult.AlreadyPosted,
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: audit bet posted: %w", err)
		}
	}

	// Stage 4H-B1 Wave 3 Phase 3 (ledger-accounting-model.md §7.18.3.3):
	// the cash-funded wagering-contribution trigger, in the SAME
	// transaction as the bet posting above (HR-10) - so a rejected/rolled-
	// back bet is structurally incapable of producing a phantom
	// contribution. HasActiveWageringGrant is the cheap, indexed
	// pre-check every bet pays regardless of whether the player holds an
	// active Grant; the weighting/attribution/completion work below only
	// runs for the (rare) case it returns true. Never gates the bet
	// itself - a failure here is a genuine internal error (returned,
	// which rolls back this bet's own posting too, exactly like every
	// other post-posting step in this function), never a silent
	// swallow that would leave a contribution permanently unrecorded.
	active, err := bonus.HasActiveWageringGrant(ctx, tx, tenantID, session.PlayerAccountID)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: check active wagering grant: %w", err)
	}
	if active {
		game, err := GetGameByID(ctx, tx, session.GameID)
		if err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve game for wagering contribution: %w", err)
		}
		if err := bonus.RecordCashFundedWageringContribution(ctx, tx, bonus.CashFundedBetContributionParams{
			TenantID: tenantID, PlayerAccountID: session.PlayerAccountID,
			BetLedgerTransactionID: postResult.TransactionID, CorrelationID: roundCorrelationID(tenantID, providerID, event.RoundID),
			AssetCode: event.AssetCode, StakeAmount: event.Amount,
			GameType: game.GameType, ProviderGameID: event.ProviderGameID,
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: record cash-funded wagering contribution: %w", err)
		}
	}

	return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID, Replayed: postResult.AlreadyPosted}, nil
}

// postWin implements Flow 6 (financial-transaction-flows.md §6): resolves
// the round's true funding origin (§16.4 in full, Stage 4H-B1 Wave 2
// Phase 7 - bonus_settlement.go) and credits it accordingly. A win naming
// a round with no matching, still-valid (never rolled back) prior bet is
// an integrity alert (a provider protocol violation), not a routine
// failure - logged/audited at elevated severity by the HTTP handler,
// which maps ErrBetNotFound distinctly from an ordinary not-found.
//
// The wallet a win credits is resolved from the round's OWN bet
// transaction's own ledger entries - never from event.PlayerAccountID
// (specialist review finding, empirically reproduced during review: a
// win naming a DIFFERENT player_account_id than the one who placed the
// round's bet was previously credited to that different player in full).
// Deriving from the bet's own ledger-truth entries is a stronger anchor
// than a session lookup here: it is impossible for a win to be
// misdirected to any wallet other than the one the round's own bet is
// already proven to have used. The account TYPE the win credits (cash vs.
// bonus, locked vs. direct) is likewise derived exclusively from ledger
// truth via resolveWinOrigin - never from event.Amount, event.AssetCode
// (used only as a cross-check below, never a resolution input), or any
// other payload field (§16.3/§16.18's standing invariant).
//
// Deliberately does NOT call evaluateAndAuditEligibility (Stage 4D-RG,
// ADR 0026's own "Specialist review findings and fixes" - financial
// correctness review): a win settles a bet that was already legitimate
// when placed (postBet's own RG check already gated it). Blocking the
// settlement of an already-placed bet because the player's status changed
// AFTER the bet would strand the stake in house_gaming with no
// compensating entry - the opposite of player protection, not an
// enforcement of it.
func (o *Orchestrator) postWin(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent) (ReceiveCallbackResult, error) {
	if err := validateCallbackEvent(event); err != nil {
		return ReceiveCallbackResult{}, err
	}

	// ADR 0082 Amendment A6 extension (Stage 10.3 W1c, architect-directed
	// W0 code check): L0.1 on THIS WIN's OWN provider_tx_id, taken first -
	// before anything else in this function, including the tombstone check
	// immediately below. Closes E10's own race: a win and a rollback that
	// names this SAME win's provider_tx_id as its OriginalProviderTxID (a
	// rollback of a win the ledger has not posted yet) can arrive
	// concurrently. postRollback's own L0.1 (Amendment A6) is keyed on the
	// SAME ref string for its original reference, so the two calls
	// serialize deterministically on this exact key: whichever transaction
	// commits first decides the outcome for the other - either this win
	// posts first and the rollback later finds a real casino_win to
	// reverse (E7), or the tombstone commits first and is visible to the
	// isProviderTxTombstoned check below (E10). Without this lock, both
	// could observe "no tombstone yet" concurrently and each proceed,
	// letting a win post AFTER its own rollback already tombstoned that
	// exact reference.
	//
	// No new lock class, no new exception, R8 unaffected: this is the
	// SAME L0.1 class postBet/postRollback already take, just a new call
	// site sharing the identical key format (acquireProviderTxDeliveryLock).
	if err := acquireProviderTxDeliveryLock(ctx, tx, tenantID, providerID, event.ProviderTxID); err != nil {
		return ReceiveCallbackResult{}, err
	}

	// Stage 10.3 CAS-CAP-ROLLBACK-1, E10 (§1.3 table): a rollback for THIS
	// exact provider_tx_id already committed a tombstone before this win
	// was ever posted (a rollback of a win the ledger has not seen yet).
	// Rejected with a named, typed error (ErrOriginalTombstoned -> 409) -
	// postWin has no established decline-without-error convention (unlike
	// postBet), so this is reported as an error, aborting the transaction
	// with zero writes; F12's rejection-record gap for this specific case
	// is disclosed, not fixed here (Item 2/W2b's rejection record covers
	// it later).
	if tombstoned, err := isProviderTxTombstoned(ctx, tx, tenantID, providerID, event.ProviderTxID); err != nil {
		return ReceiveCallbackResult{}, err
	} else if tombstoned {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: provider_tx_id=%s", ErrOriginalTombstoned, event.ProviderTxID)
	}

	// R3-GAME-POSTINGS-NONACTIVE-1: a win for a suspended/closed tenant is a
	// NEW movement and is refused, in this transaction, before the round is
	// resolved or anything is locked/written. An exact replay of an already-
	// posted win (its own reference exists) is exempt and falls through to
	// the existing AlreadyPosted handling, which returns the original.
	if err := requireActiveTenantForNewPosting(ctx, tx, tenantID, providerID, event.ProviderTxID); err != nil {
		return ReceiveCallbackResult{}, err
	}

	correlationID := roundCorrelationID(tenantID, providerID, event.RoundID)

	// Stage 9 §10 (adversarial concurrency re-audit, ledger-finance):
	// resolveWinOrigin below is a check-then-act read - "is this round's
	// bet still unreversed" (the ErrBetNotFound guard) and "is any of its
	// stake still locked" (LF-18's ErrLockAlreadyReleased guard, §16.4
	// outcome 6). Both guards were unlocked reads, so two genuinely
	// concurrent settlements of one round each observed the pre-race
	// answer and each acted on it: empirically reproduced (Stage 9,
	// TestStage9_ConcurrentDistinctWinsOnLockedRound_ReleasesLockExactly-
	// Once) as two distinct win callbacks for one locked round EACH
	// posting the full stake release - crediting the player the same
	// stake twice and driving player_locked_cash negative. Every
	// individual posting balanced, so SUM(debits) == SUM(credits) did not
	// catch it; the guard simply never ran against committed state.
	//
	// Locking the round's own casino_bet transaction row(s) first makes
	// every settlement of one round serialize on the SAME row
	// postRollback already locks for the identical reason (see its own
	// FOR UPDATE and rationale) - so by the time the second caller
	// re-reads below, the first's effect (a release, or a reversal) is
	// committed and visible, and its guard fires. A row lock on an
	// append-only table is not a mutation and does not trip
	// ledger_deny_mutation(). Ordered by id so several bet rows under one
	// correlation id are always taken in the same order. Scoped to one
	// round, so it adds no contention beyond the case it exists to fix;
	// a round with no bet at all locks nothing and still falls through to
	// resolveWinOrigin's own ErrBetNotFound.
	if _, err := tx.Exec(ctx,
		`SELECT 1 FROM ledger_transactions
		  WHERE tenant_id = $1 AND correlation_id = $2 AND transaction_type = $3
		  ORDER BY id FOR UPDATE`,
		tenantID, correlationID, ledger.TxCasinoBet,
	); err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: lock round bet transactions: %w", err)
	}

	origin, err := resolveWinOrigin(ctx, tx, tenantID, correlationID)
	if err != nil {
		// Covers "no bet was ever posted for this round", "the round's bet
		// was already rolled back" (ErrBetNotFound - the same class of
		// integrity violation as an orphan win), and every other §16.4
		// abort outcome (ErrCorrelationWalletCollision,
		// ErrAmbiguousMultiOriginRound, ErrMixedFundingUnsupported,
		// ErrLockAlreadyReleased, ErrBonusBetNotLocked) - each decorated
		// identically with round/provider context for ops visibility.
		return ReceiveCallbackResult{}, fmt.Errorf("%w: round=%s provider=%s", err, event.RoundID, providerID)
	}

	wl, err := wallet.GetByID(ctx, tx, origin.WalletID)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve wallet: %w", err)
	}
	if wl.AssetCode != event.AssetCode {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: asset_code does not match the round's own bet", ErrInvalidInput)
	}

	switch origin.AccountType {
	case ledger.AccountPlayerCash:
		return o.postWinDirectCash(ctx, tx, tenantID, providerID, event, origin, correlationID)
	case ledger.AccountPlayerLockedCash:
		return o.postWinLockedCash(ctx, tx, tenantID, providerID, event, origin, correlationID)
	case ledger.AccountPlayerLockedBonus:
		return o.postWinLockedBonus(ctx, tx, tenantID, providerID, event, origin, correlationID)
	default:
		// Unreachable given resolveWinOrigin's own exhaustive
		// classification (ledger-accounting-model.md §6.6.5's discipline:
		// an allowlist-with-silent-fallback fails open) - kept as a hard
		// stop rather than a silent guess if it is ever reached.
		return ReceiveCallbackResult{}, fmt.Errorf("casino: unhandled win origin account_type %q", origin.AccountType)
	}
}

// postRollback implements Flow 7 (financial-transaction-flows.md §7): a
// new LedgerTransaction with reverses_transaction_id pointing at the
// SPECIFIC original bet or win transaction named by
// event.OriginalProviderTxID - exact inverse of whichever flow actually
// posted. A rollback naming a provider_tx_id the ledger never posted a
// bet OR win for writes a tombstone (CLAUDE.md's rollback rule), mirroring
// internal/payments.postDepositReversalTombstone exactly.
//
// Also deliberately does NOT call evaluateAndAuditEligibility (see
// postWin's identical doc comment) - a rollback is a CORRECTION to
// history, not a new stake; gating corrections on the player's current
// status would make the ledger un-correctable for exactly the players
// most likely to need a correction.
func (o *Orchestrator) postRollback(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent) (ReceiveCallbackResult, error) {
	if event.ProviderTxID == "" {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: provider_tx_id is required", ErrInvalidInput)
	}
	if event.OriginalProviderTxID == "" {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: original_provider_tx_id is required for a rollback", ErrInvalidInput)
	}

	// ADR 0082 Amendment A6 (Stage 10.3 W1c, CAS-CAP-ROLLBACK-1): L0.1 on
	// the ORIGINAL reference (event.OriginalProviderTxID), taken first -
	// before the FOR UPDATE lookup below, and before anything else in this
	// function. This is the SAME key string postBet takes on its own
	// reference and postWin now also takes on its own reference - so a
	// late-arriving original bet/win for this exact reference, racing this
	// rollback, serializes deterministically: either the original commits
	// first and this rollback finds a real casino_bet/casino_win to
	// reverse below (E7), or this rollback's tombstone commits first and
	// the late original is rejected by postBet's E3 check (or postWin's
	// E10 check). Before this lock, the loser of that race got an untyped
	// unique-violation error surfacing as an unhelpful 500 (F8) - never a
	// financial correctness issue (nothing double-posts either way), but a
	// named, deterministic outcome now replaces it.
	if err := acquireProviderTxDeliveryLock(ctx, tx, tenantID, providerID, event.OriginalProviderTxID); err != nil {
		return ReceiveCallbackResult{}, err
	}

	// FOR UPDATE: two concurrent rollback requests naming the SAME
	// original must serialize on this row, not both observe "not yet
	// reversed" and both post a reversal (specialist review finding,
	// empirically reproduced: two distinct concurrent rollback references
	// for one bet both succeeded, doubling the reversal credit).
	// Permitted on this append-only table - a row lock is not itself a
	// mutation and does not trigger ledger_deny_mutation().
	var originalID uuid.UUID
	var originalType ledger.TransactionType
	err := tx.QueryRow(ctx,
		`SELECT id, transaction_type FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = $2 AND provider_tx_id = $3 FOR UPDATE`,
		tenantID, providerID, event.OriginalProviderTxID,
	).Scan(&originalID, &originalType)
	if errors.Is(err, pgx.ErrNoRows) {
		// R3-GAME-POSTINGS-NONACTIVE-1: writing a tombstone is a NEW ledger
		// write; refused for a suspended/closed tenant (the late original
		// would be refused by the same check, so nothing is left unguarded).
		if err := requireActiveTenantForNewPosting(ctx, tx, tenantID, providerID, ""); err != nil {
			return ReceiveCallbackResult{}, err
		}
		txID, tombErr := postRollbackTombstone(ctx, tx, tenantID, providerID, event)
		if tombErr != nil {
			return ReceiveCallbackResult{}, tombErr
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_rollback.tombstoned",
			TargetType: "ledger_transaction", TargetID: txID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": providerID, "original_provider_tx_id": event.OriginalProviderTxID,
				"rollback_provider_tx_id": event.ProviderTxID,
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: audit rollback tombstone: %w", err)
		}
		return ReceiveCallbackResult{Tombstoned: true, LedgerTransactionID: &txID}, nil
	}
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: look up original transaction: %w", err)
	}
	if originalType == ledger.TxTombstone {
		// A second rollback event (a redelivery of the same reference, or
		// a genuinely distinct new reference) naming an original that
		// STILL does not exist - the first such delivery already wrote
		// this tombstone (postRollbackTombstone's own idempotency key is
		// deterministic per OriginalProviderTxID, mirroring
		// internal/payments' identical "multiple reversal attempts
		// against one never-posted original collapse to one tombstone"
		// rule). Report the same idempotent tombstone result again,
		// rather than the generic "expected casino_bet or casino_win"
		// error this used to fall through to.
		//
		// Stage 10.3 W2b (CAS-RECON-1; gate 10.3-W1 ledger-finance C9
		// extension): when THIS delivery's own rollback reference differs
		// from the one that wrote the tombstone, it is a second, distinct
		// rollback reference for an original that was never posted - still
		// acknowledged with the idempotent tombstone result (unchanged), but
		// now durably recorded in this committing transaction. The first
		// reference is read from the tombstone's own
		// casino_rollback.tombstoned audit row (same transaction as the
		// tombstone). If that row is not visible the reference cannot be
		// proven equal, and the delivery is recorded rather than silently
		// dropped (fail toward evidence; a human resolves it). A
		// same-reference redelivery records nothing.
		firstRef, known, err := firstTombstoningRollbackReference(ctx, tx, tenantID, originalID)
		if err != nil {
			return ReceiveCallbackResult{}, err
		}
		if !known || firstRef != event.ProviderTxID {
			if _, err := RecordCallbackRejection(ctx, tx, tenantID, providerID,
				newCallbackRejection(RejectionRollbackOfTombstonedOriginal, event), observability.RequestIDFromContext(ctx)); err != nil {
				return ReceiveCallbackResult{}, err
			}
		}
		return ReceiveCallbackResult{Tombstoned: true, LedgerTransactionID: &originalID, Replayed: true}, nil
	}
	if originalType != ledger.TxCasinoBet && originalType != ledger.TxCasinoWin {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: original transaction %s has type %q, expected casino_bet or casino_win",
			ErrInvalidInput, originalID, originalType)
	}

	// R3-GAME-POSTINGS-NONACTIVE-1: a rollback/refund of an already-posted
	// bet or win is a NEW reversing posting; refused for a suspended/closed
	// tenant (owner decision: fail closed, the callback is kept as durable
	// evidence for staff resolution). An exact redelivery of an already-
	// posted rollback (its own reference exists) is exempt and returns the
	// original through ledger.Post's AlreadyPosted gate below.
	if err := requireActiveTenantForNewPosting(ctx, tx, tenantID, providerID, event.ProviderTxID); err != nil {
		return ReceiveCallbackResult{}, err
	}

	// A prior reversal against originalID is only a conflict if it was
	// posted under a DIFFERENT rollback reference - a REDELIVERY of this
	// exact rollback (same event.ProviderTxID) must fall through to
	// ledger.Post below and return its own idempotent no-op result, never
	// ErrAlreadyRolledBack (directive items G/H/I: redelivery/replay of a
	// financial callback must never be indistinguishable from a genuine
	// second, conflicting rollback attempt).
	var existingReversalProviderTxID *string
	err = tx.QueryRow(ctx,
		`SELECT provider_tx_id FROM ledger_transactions WHERE reverses_transaction_id = $1`, originalID,
	).Scan(&existingReversalProviderTxID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: check existing rollback: %w", err)
	}
	if existingReversalProviderTxID != nil && *existingReversalProviderTxID != event.ProviderTxID {
		return ReceiveCallbackResult{}, ErrAlreadyRolledBack
	}

	// §16.15/§16.21 site 3 (Stage 4H-B1 Wave 2 Phase 7, bonus_settlement.go):
	// a rollback naming a WIN that is still parked (or was ever parked) in
	// bonus_held_dispositions is handled entirely by its own dedicated
	// transition - never the generic entry-inversion below, which would
	// (per §16.15's own reasoning) wrongly resurrect a lock that already,
	// correctly, closed to zero. handled=false for every ordinary win (no
	// disposition row at all) falls through unaffected.
	if originalType == ledger.TxCasinoWin {
		if handled, result, err := o.postRollbackHeldWin(ctx, tx, tenantID, providerID, event, originalID); handled {
			return result, err
		}
	}

	entries, err := loadEntries(ctx, tx, originalID)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if len(entries) == 0 {
		// Fail closed rather than post a zero-entry "rollback" that would
		// mark originalID reversed (blocking any future genuine rollback)
		// without actually moving any money back - not reachable today
		// (originalType already proved a real casino_bet/casino_win
		// exists, and Post never leaves a non-tombstone transaction
		// entryless), but a silent no-op here would be worse than a loud
		// failure if that invariant is ever violated.
		return ReceiveCallbackResult{}, fmt.Errorf("casino: original transaction %s has no ledger entries to reverse", originalID)
	}

	inverted := make([]ledger.EntryInput, 0, len(entries))
	for _, e := range entries {
		dir := ledger.Credit
		if e.Direction == ledger.Credit {
			dir = ledger.Debit
		}
		inverted = append(inverted, ledger.EntryInput{LedgerAccountID: e.LedgerAccountID, Direction: dir, Amount: e.Amount})
	}

	// §16.10.3/§16.21 site 1 (Stage 4H-B1 Wave 2 Phase 7): a plain
	// lock-rollback of a bonus-funded BET (before any win/loss is known)
	// touches BONUS_SET (player_bonus/player_locked_bonus), which
	// bonus_mirror.go's Rule B2 generator REQUIRES a BonusCost for -
	// resolved from the Grant this bet's own lock is attributed to
	// (grant_ledger_attributions), never guessed. Scoped to originalType
	// == casino_bet only: an ORDINARY win's rollback (a resolved,
	// non-held player_bonus credit later swept by an unrelated
	// forfeiture) deliberately gets NO BonusCost here, leaving today's
	// existing nil behavior unchanged - LF-10's general case (§16.20,
	// still open, ledger-finance's decision) is not silently re-enabled
	// by this dispatch; it continues to fail at ledger.Post's own
	// ErrBonusCostRequired validation exactly as it does today, rather
	// than this package inventing a sufficiency-check/compensating-entry
	// mechanism ledger-finance has not yet designed.
	var bonusCost *ledger.BonusCostAttribution
	var betGrantID uuid.UUID
	var betGrantFound bool
	if originalType == ledger.TxCasinoBet && entriesTouchBonusSet(entries) {
		betGrantID, betGrantFound, err = lookupGrantForLedgerTransaction(ctx, tx, tenantID, originalID)
		if err != nil {
			return ReceiveCallbackResult{}, err
		}
		if !betGrantFound {
			return ReceiveCallbackResult{}, fmt.Errorf("%w: original_transaction=%s", ErrLockedBonusGrantMissing, originalID)
		}
		if err := bonus.AdvisoryLockGrant(ctx, tx, tenantID, betGrantID); err != nil {
			return ReceiveCallbackResult{}, err
		}
		grant, err := bonus.GetGrantByID(ctx, tx, betGrantID)
		if err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: load grant for bet rollback: %w", err)
		}
		bonusCost, err = grantBonusCost(grant.FundingSource)
		if err != nil {
			return ReceiveCallbackResult{}, err
		}
	}

	rollbackTxType := ledger.TxCasinoRollback
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: rollbackTxType,
		IdempotencyKey:        providerID + ":" + event.ProviderTxID,
		ProviderID:            &providerID,
		ProviderTxID:          &event.ProviderTxID,
		CorrelationID:         roundCorrelationID(tenantID, providerID, event.RoundID),
		ReversesTransactionID: &originalID,
		Entries:               inverted,
		BonusCost:             bonusCost,
	})
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: post rollback: %w", err)
	}

	if betGrantFound && !postResult.AlreadyPosted {
		// Attribute the reversal itself to the Grant BEFORE rechecking
		// exposure - ComputeAOE's Component 1 (lockedExposure, aoe.go) is a
		// LIVE read of player_locked_bonus summed ONLY over entries
		// attributed via grant_ledger_attributions. The original bet's own
		// lock credit was (or, once a bonus-funded postBet ships, will be)
		// attributed at bet time; without attributing this reversal too,
		// that component would never net back to zero.
		if err := bonus.AttributeGrantLedgerTransactionIdempotent(ctx, tx, tenantID, betGrantID, postResult.TransactionID, string(rollbackTxType)); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: attribute bet rollback to grant: %w", err)
		}
		if err := recheckGrantAfterBonusTouchingRollback(ctx, tx, tenantID, betGrantID, postResult.TransactionID); err != nil {
			return ReceiveCallbackResult{}, err
		}
	}

	// gate 10.3-W1 QA condition 1 / ledger-finance C4 (same defect as
	// postWin's identical guard, see bonus_settlement.go's
	// postWinDirectCash comment): a redelivery of an already-posted
	// rollback (this generic entry-inversion path - the tombstone path a
	// few lines above already short-circuits before ever reaching here)
	// must not write a second audit row.
	if !postResult.AlreadyPosted {
		auditAction := "casino_bet.rolled_back"
		if originalType == ledger.TxCasinoWin {
			auditAction = "casino_win.rolled_back"
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: auditAction,
			TargetType: "ledger_transaction", TargetID: postResult.TransactionID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": providerID, "rollback_provider_tx_id": event.ProviderTxID,
				"original_provider_tx_id": event.OriginalProviderTxID, "original_transaction_id": originalID.String(),
				"already_posted": postResult.AlreadyPosted,
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("casino: audit rollback: %w", err)
		}
	}

	return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID, Replayed: postResult.AlreadyPosted}, nil
}

func postRollbackTombstone(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent) (uuid.UUID, error) {
	// Stage 10.3 CAS-CAP-ROLLBACK-1 (§1.4 step 5, F7): the tombstone's own
	// CorrelationID is now DETERMINISTIC, not uuid.New() - the SAME
	// roundCorrelationID a bet/win of this round would use, when the
	// rollback event names a RoundID (as every one of the platform's own
	// test/mock payloads and every real casino round does). This is
	// replay-safe because tombstones are exempt from correlation
	// comparison entirely (replay.go's own comment on this) - it exists
	// only so Item 2's reconciliation stream can later JOIN a tombstone to
	// its round. When no RoundID is present (a rollback that never names
	// one), fall back to a deterministic v5 UUID of the tombstone's own
	// idempotency key - still never a random uuid.New(), so two identical
	// deliveries of a rollback with no RoundID always compute the SAME
	// value (ledger.Post's own idempotency key is what actually guards
	// against a duplicate post either way; this is only about which
	// correlation id a first delivery picks). Existing rows written before
	// this change keep their original (random) CorrelationID - untouched.
	idempotencyKey := fmt.Sprintf("tombstone:%s:%s", providerID, event.OriginalProviderTxID)
	// Tenant-qualified (gate 10.3-W1 code review #13; ledger-finance
	// informational note, §1's "Deterministic tombstone correlation" row):
	// the idempotency key alone is only unique WITHIN one tenant
	// (provider_tx_id uniqueness is a per-tenant property throughout this
	// package - see acquireProviderTxDeliveryLock's identical rationale),
	// so two different tenants' rollbacks of a same-named, never-seen
	// original would otherwise compute the identical correlation id. This
	// changes the fallback's VALUE for callers with no RoundID; it is still
	// replay-safe because tombstones remain exempt from correlation
	// comparison (ledger/replay.go), so the change cannot break a redelivery
	// of an existing tombstone - confirmed against
	// TestReplay_CorrelationComparedExceptTombstone.
	correlationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(tenantID.String()+":"+idempotencyKey))
	if event.RoundID != "" {
		correlationID = roundCorrelationID(tenantID, providerID, event.RoundID)
	}
	// ReasonCode left nil - ledger_transactions' CHECK constraint
	// (migration 0021) forbids it for any type other than
	// manual_adjustment. The human-readable reason lives in this
	// function's caller's audit record instead.
	result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxTombstone,
		IdempotencyKey: idempotencyKey,
		ProviderID:     &providerID, ProviderTxID: &event.OriginalProviderTxID,
		CorrelationID: correlationID,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("casino: post rollback tombstone: %w", err)
	}
	return result.TransactionID, nil
}

type ledgerEntry struct {
	LedgerAccountID uuid.UUID
	Direction       ledger.Direction
	Amount          int64
	// AccountType is resolved alongside the entry (Stage 4H-B1 Wave 2
	// Phase 7) so postRollback's generic entry-inversion path can detect
	// whether the original transaction touched a BONUS_SET account
	// (player_bonus/player_locked_bonus/player_bonus_held) without a
	// second query - bonus_mirror.go's Rule B2 generator requires a
	// BonusCost on any posting that does.
	AccountType ledger.AccountType
}

func loadEntries(ctx context.Context, tx pgx.Tx, transactionID uuid.UUID) ([]ledgerEntry, error) {
	// ORDER BY e.ledger_account_id is NO LONGER the lock-ordering
	// mechanism, and this comment no longer claims it is (ADR 0082 §4.2).
	//
	// It was introduced as the Stage 9 LOCK-2 fix, on the reasoning that
	// postRollback feeds these rows straight into ledger.Post in the
	// order returned, every ledger_entries INSERT fires migration 0023's
	// AFTER trigger, and that trigger's ON CONFLICT DO UPDATE takes a ROW
	// LOCK - so this SELECT's row order WAS the projection-lock
	// acquisition order, and an unordered read (planner-dependent: seq
	// scan, bitmap heap scan and index scan can each return a different
	// order for the same rows) let two concurrent rollbacks over the same
	// account pair deadlock.
	//
	// That reasoning was correct but provably incomplete: ledger.Post
	// APPENDS the Rule B2 mirror/recognition legs AFTER the caller's
	// entries (§7.4.2's fixed order), so a bonus-touching rollback's
	// final lock sequence was "sorted caller entries, then unsorted
	// generated legs" - not sorted at all. ADR 0082's pre-lock step
	// inside Post now covers the complete final account set, generated
	// legs included, which is the case an ORDER BY here could never
	// reach. This clause is kept because a deterministic read order is
	// correct and documents intent, not because anything depends on it
	// for locking.
	rows, err := tx.Query(ctx,
		`SELECT e.ledger_account_id, e.direction, e.amount, la.account_type
		   FROM ledger_entries e
		   JOIN ledger_accounts la ON la.id = e.ledger_account_id
		  WHERE e.ledger_transaction_id = $1
		  ORDER BY e.ledger_account_id`,
		transactionID,
	)
	if err != nil {
		return nil, fmt.Errorf("casino: load entries: %w", err)
	}
	defer rows.Close()

	var out []ledgerEntry
	for rows.Next() {
		var e ledgerEntry
		var acctType string
		if err := rows.Scan(&e.LedgerAccountID, &e.Direction, &e.Amount, &acctType); err != nil {
			return nil, fmt.Errorf("casino: scan entry: %w", err)
		}
		e.AccountType = ledger.AccountType(acctType)
		out = append(out, e)
	}
	return out, rows.Err()
}

// entriesTouchBonusSet reports whether any of entries resolves to a
// BONUS_SET account (ledger-accounting-model.md §7.7.2.3) - mirrors
// internal/ledger's own bonusSetAccountTypes list (unexported; casino
// only needs the membership test, not the generator itself).
func entriesTouchBonusSet(entries []ledgerEntry) bool {
	for _, e := range entries {
		switch e.AccountType {
		case ledger.AccountPlayerBonus, ledger.AccountPlayerLockedBonus, ledger.AccountPlayerBonusHeld:
			return true
		}
	}
	return false
}
