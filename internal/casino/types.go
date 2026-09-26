// Package casino implements the casino-provider-agnostic integration
// foundation (Stage 4A): the CasinoProvider adapter interface, the
// Orchestrator that resolves a game's provider, mints/resolves game-
// launch sessions, and translates verified provider callbacks into
// ledger postings for Flows 5-7 (bet/win/rollback), the platform-wide
// game catalogue plus tenant/brand availability, the
// CasinoProviderCapability configuration model, and a MOCK casino
// adapter for development/testing.
//
// Scope for this stage: no real casino provider is integrated (a MOCK
// adapter only), no production credentials exist, no sportsbook/bonus-
// engine/KYC-AML/RG code is implemented here, and bonus-funded stakes
// (player_bonus splits) and jackpot contribution splits are explicitly
// NOT implemented - every Bet/Win in this package assumes a 100%
// player_cash-funded stake, per ADR 0025 §6's documented scope boundary.
//
// See docs/decisions/0025-casino-provider-abstraction-and-game-session-
// model.md and docs/architecture/08-casino-integration-architecture.md
// for the frozen design this package implements against, and
// docs/architecture/financial-transaction-flows.md §5-7 for the exact
// ledger accounts/transaction types/idempotency keys Bet/Win/Rollback
// must use.
package casino

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// Outcome is the canonical result state shared by LaunchResult,
// BetResult, WinResult, RollbackResult, and CallbackEvent - the same
// distinction internal/payments.Outcome draws, applied here: a definite
// OutcomeDeclined is never conflated with an OutcomeAmbiguous
// (timeout/unknown) result, since the two require different handling
// (a decline is final; an ambiguous outcome must never be silently
// treated as either a success or a failure - see Orchestrator.Bet's own
// doc comment).
type Outcome string

const (
	OutcomePending   Outcome = "pending"
	OutcomeSucceeded Outcome = "succeeded"
	OutcomeDeclined  Outcome = "declined"
	OutcomeAmbiguous Outcome = "ambiguous"
)

// GameMode mirrors casino_launch_sessions.mode.
type GameMode string

const (
	ModeReal GameMode = "real"
	ModeDemo GameMode = "demo"
)

// CallbackCapability mirrors casino_provider_capabilities.callback_capabilities
// - the same enum shape docs/decisions/0022 §2 defines for payments,
// redefined locally rather than imported so internal/casino carries no
// dependency on internal/payments (the two integration domains are
// independent adapters over the same architectural pattern - ADR 0025's
// own "Consequences" section).
type CallbackCapability string

const (
	CallbackWebhookOnly CallbackCapability = "webhook"
	CallbackPollingOnly CallbackCapability = "polling_only"
	CallbackBoth        CallbackCapability = "both"
)

// CapabilityStatus mirrors casino_provider_capabilities.status - an
// operator kill-switch independent of health/circuit-breaker state.
type CapabilityStatus string

const (
	CapabilityActive   CapabilityStatus = "active"
	CapabilityDisabled CapabilityStatus = "disabled"
)

// GameStatus mirrors casino_games.status - a PLATFORM-level kill switch
// (e.g. a title's licence is pulled), distinct from a tenant/brand's own
// GameAvailability.Enabled toggle.
type GameStatus string

const (
	GameStatusActive   GameStatus = "active"
	GameStatusDisabled GameStatus = "disabled"
)

// CircuitState mirrors payment-orchestration.md §6's circuit-breaker
// shape, applied identically here (not persisted - see ProviderHealth).
type CircuitState string

const (
	CircuitClosed   CircuitState = "closed"
	CircuitOpen     CircuitState = "open"
	CircuitHalfOpen CircuitState = "half_open"
)

// LaunchSessionStatus mirrors casino_launch_sessions.status.
type LaunchSessionStatus string

const (
	LaunchSessionActive   LaunchSessionStatus = "active"
	LaunchSessionConsumed LaunchSessionStatus = "consumed"
	LaunchSessionExpired  LaunchSessionStatus = "expired"
	LaunchSessionRevoked  LaunchSessionStatus = "revoked"
)

// DenialCode values LaunchGame reports on LaunchGameResult (K-3 remediation,
// docs/governance/stage-4i-canonical-model.md §9.2/§9.3 K3-2/K3-5): a
// jurisdiction-dependent denial is reported as a RESULT, not a Go error
// (LaunchGameResult.Denied/DenialCode - same shape the RG/Risk gates
// already use, per K3-5's return-shape-consistency requirement), and the
// two possible jurisdiction denial reasons are DELIBERATELY
// distinguishable INTERNALLY (this constant, the audit trail, operator
// logs) even though K3-6 (canonical-model §6.3, the HTTP-boundary oracle
// rule) requires them to collapse into ONE identical player-facing
// response. That collapse happens ONLY at the HTTP boundary
// (internal/httpserver/casino_handlers.go) - never here.
const (
	// DenialCodeJurisdictionUnresolved is reported when a game's own
	// jurisdiction_blocklist is non-empty (the control is "armed" - see
	// LaunchGame's own K-3 doc comment) but the platform could not
	// determine the player's jurisdiction (jurisdiction.Resolve did not
	// return Resolved). This is NOT "blocked in this jurisdiction" - the
	// player's jurisdiction is unknown, not known-and-disallowed - and
	// must never be reported as DenialCodeJurisdictionBlocked (K3-2).
	DenialCodeJurisdictionUnresolved = "jurisdiction_unresolved"
	// DenialCodeJurisdictionBlocked is reported when the player's
	// resolved jurisdiction code appears in the game's own
	// jurisdiction_blocklist.
	DenialCodeJurisdictionBlocked = "jurisdiction_blocked"
)

// Sentinel errors.
var (
	// ErrGameNotFound is returned when a provider_game_id/game id names
	// no row in casino_games.
	ErrGameNotFound = errors.New("casino: game not found")
	// ErrGameDisabled is returned when the platform-level casino_games.status
	// is 'disabled' - distinct from ErrGameNotAvailable (a tenant/brand's
	// own opt-out), so a caller can tell "the platform pulled this title"
	// from "this tenant never enabled it" (directive items O/P).
	ErrGameDisabled = errors.New("casino: game is disabled at the platform level")
	// ErrGameNotAvailable is returned when the caller's (tenant, brand)
	// has no enabled casino_game_availability row for this game.
	ErrGameNotAvailable = errors.New("casino: game is not available for this tenant/brand")
	// ErrUnknownProvider is returned when a capability row or routing
	// decision names a provider_id the orchestrator has no adapter
	// registered for.
	ErrUnknownProvider = errors.New("casino: unknown provider_id")
	// ErrProviderUnavailable is returned when the game's own provider has
	// no active, launch-capable casino_provider_capabilities row for this
	// tenant/brand/asset, or is unhealthy (circuit open) - directive item Q.
	ErrProviderUnavailable = errors.New("casino: provider is not available for this tenant/brand")
	// ErrCapabilityWidensAdapter is returned by WriteCapability when a
	// tenant-configured row would assert more than the adapter's own
	// declared capability (mirrors docs/decisions/0022 §2.1).
	ErrCapabilityWidensAdapter = errors.New("casino: capability configuration widens beyond the adapter's declared capability")
	// ErrRiskOutcomeUnrecognized is returned when risk.Evaluate returns a
	// RiskDecision whose Outcome is none of allow/deny/review. Unreachable
	// through risk.Evaluate itself (which self-checks its own output), and
	// kept anyway: this enforcement point must classify the FOURTH
	// outcome - "the platform could not decide" - separately from a real
	// DENY/REVIEW business decision, so an unrecognized state can never be
	// reported to a provider or player as a policy decline, nor (far
	// worse) fall through to ALLOW. See ADR 0031 §34.
	ErrRiskOutcomeUnrecognized = errors.New("casino: risk evaluation returned an unrecognized outcome")
	// ErrCallbackSignatureInvalid is returned by a CasinoProvider's
	// HandleCallback when the payload's authentication does not verify.
	// Never wrapped with the raw payload or any field from it. Stage 10.2
	// (CAS-WH-TENANT-1, ADR 0091, design §C1): the same sentinel value as
	// internal/webhookauth and internal/payments, so errors.Is behaves
	// identically across every webhook domain.
	ErrCallbackSignatureInvalid = webhookauth.ErrSignatureInvalid
	// ErrCallbackMalformedBody is returned by a CasinoProvider's
	// HandleCallback for a VERIFIED callback (the sender proved knowledge
	// of the shared credential) whose body is structurally malformed -
	// missing a required field, an unrecognized event_type/outcome, or an
	// unparseable player_account_id/session_id UUID. A DIFFERENT sentinel
	// class from ErrCallbackSignatureInvalid/webhookauth.ErrAuthFailed:
	// only reachable once verification has already succeeded, so the HTTP
	// layer maps it to 400, never the uniform pre-verification 401
	// (design §C2 point 2; mirrors payments.ErrCallbackMalformedBody).
	ErrCallbackMalformedBody = errors.New("casino: malformed callback body")

	// ErrLaunchSessionNotFound is returned when a launch token resolves to
	// no casino_launch_sessions row at all.
	ErrLaunchSessionNotFound = errors.New("casino: launch session not found")
	// ErrLaunchSessionNotActive is returned when a launch token resolves
	// to a real row that is not in 'active' status - already consumed,
	// expired, or revoked. Single-use enforcement (ADR 0025 §3): a second
	// resolution attempt for the same token, even the exact same
	// millisecond, must never succeed twice.
	ErrLaunchSessionNotActive = errors.New("casino: launch session is not active (already consumed, expired, or revoked)")

	// ErrInsufficientFunds is returned when a bet's wallet balance,
	// locked and read inside the same transaction as the prospective
	// debit, does not cover the requested stake (invariant #15, same
	// pattern as internal/withdrawal.ErrInsufficientFunds).
	ErrInsufficientFunds = errors.New("casino: insufficient player_cash balance")
	// ErrBetNotFound is returned when a win or rollback callback names a
	// round/provider_tx_id with no matching prior bet transaction -
	// financial-transaction-flows.md §6's "integrity alert... a provider
	// protocol violation, not a normal failure path" for Win, and the
	// tombstone trigger for Rollback (see ErrRollbackOriginalNotFound,
	// which is NOT an error - a tombstone is the correct, successful
	// outcome for that case).
	ErrBetNotFound = errors.New("casino: no matching prior bet transaction for this round")
	// ErrAlreadyRolledBack is returned when a rollback names an original
	// provider_tx_id that already has a reversal posted against it, under
	// a NEW rollback reference of its own (a redelivery of the SAME
	// rollback reference is instead an idempotent no-op via ledger.Post's
	// own idempotency - this error is only for a distinct new reference
	// naming an already-reversed original, mirroring
	// payments.ErrDepositAlreadyReversed exactly).
	ErrAlreadyRolledBack = errors.New("casino: original transaction already has a rollback posted against it")
	// ErrProviderTxPayloadMismatch is returned when a callback reuses a
	// provider_tx_id that is already posted, but with a payload that
	// differs from the posted fact (Stage 10 F-7 remediation, ADR 0020
	// amendment 2026-09-25): a bet whose amount, asset, round or session
	// wallet differs from the posted bet (checked by postBet before its
	// short-circuit), or a win/rollback whose ledger posting would differ
	// from the one already stored under the same key
	// (ledger.ErrIdempotencyPayloadMismatch, wrapped). Before F-7 these
	// silently returned the ORIGINAL result as success. It is an
	// integrity alert (a provider protocol violation or a compromised
	// signing key), never a retry signal; nothing is posted.
	ErrProviderTxPayloadMismatch = errors.New("casino: provider transaction reference already posted with a different payload")

	// ErrInvalidInput is returned for a structurally invalid call (missing
	// required id, non-positive amount, empty reference) caught before
	// ever reaching the database.
	ErrInvalidInput = errors.New("casino: invalid input")

	// ErrOutcomeNotSucceeded is returned by postBet/postWin when a
	// CallbackEvent's own Outcome field is declared "declined" or
	// "ambiguous" rather than "succeeded" - specialist review (qa)
	// finding: the field was parsed from the signed payload but never
	// enforced, so a provider-declared decline/ambiguous outcome was
	// silently posted as a real financial effect regardless. A provider
	// that calls the bet/win callback boundary at all is instructing the
	// platform to move money for that event; if its own payload disagrees
	// with that ("outcome": "declined"/"ambiguous"), the callback is
	// malformed/self-contradictory and is rejected rather than trusted
	// either way.
	ErrOutcomeNotSucceeded = errors.New("casino: callback outcome is not 'succeeded'")

	// ErrLaunchSessionRequired is returned by postBet when a bet callback
	// carries no session_id, or names one that does not resolve to a
	// known, non-revoked, real-money (never demo) launch session issued
	// by THIS provider (ADR 0025 §3/§6 - specialist review finding,
	// independently raised by security/multi-tenancy/architect: a bet
	// callback with no session binding lets a validly-signed provider
	// debit an arbitrary player's wallet with no record that a launch
	// ever occurred, and cannot distinguish a demo round from a real-
	// money one).
	ErrLaunchSessionRequired = errors.New("casino: bet callback requires a valid real-money launch session")

	// --- Stage 4H-B1 Wave 2 Phase 7 (G-2 casino integration,
	// docs/architecture/08-casino-integration-architecture.md §16.4's
	// classification table). Each is a named, fail-closed abort - never a
	// guess - raised as a loud integrity/ops alert exactly like
	// ErrBetNotFound, per §16.4's own discipline. ---

	// ErrCorrelationWalletCollision is §16.4's outcome 2 (LF-7): a single
	// correlation_id resolved more than one distinct wallet_id at the
	// SAME account_type - either a roundCorrelationID hash collision or a
	// posting-layer defect, never guessed. Checked before outcome 3
	// (ErrAmbiguousMultiOriginRound) since a cross-wallet collision is the
	// more severe integrity condition.
	ErrCorrelationWalletCollision = errors.New("casino: correlation_id resolved more than one player wallet")
	// ErrAmbiguousMultiOriginRound is §16.4's outcome 3 / §16.4a (LF-8): a
	// correlation_id resolved more than one distinct bet_transaction_id
	// (a legitimate multi-bet round, re-bet, or side bet) that
	// WinRequest/CallbackEvent cannot yet disambiguate per-bet
	// (OriginatingProviderTxID does not exist on the win protocol today -
	// confirmed against types.go). Routed to manual reconciliation, never
	// guessed by amount-matching (CLAUDE.md's fail-closed financial-write
	// rule) - NOT a permanent abort-forever condition.
	ErrAmbiguousMultiOriginRound = errors.New("casino: correlation_id resolved more than one bet transaction; win cannot be attributed to a specific bet without OriginatingProviderTxID (routed to manual reconciliation)")
	// ErrMixedFundingUnsupported is §16.4's outcome 4: a single
	// bet_transaction_id's own credit/debit legs span more than one
	// distinct account_type - the exact HR-2 single-posting-instruction
	// mixed-origin shape HR-2 is meant to make unreachable. Kept distinct
	// from ErrAmbiguousMultiOriginRound so an operator can tell "this
	// looks like an HR-2 regression" apart from "this is an expected,
	// unhandled multi-bet round."
	ErrMixedFundingUnsupported = errors.New("casino: bet transaction's own entries span more than one funding origin (possible HR-2 regression)")
	// ErrLockAlreadyReleased is §16.4's outcome 6 (LF-18's fix): a
	// casino_bet credit leg genuinely exists for this correlation_id, but
	// the net signed sum over EVERY later transaction sharing it
	// (Step 1b, ledger-accounting-model.md §6.3.3.1 "variant 2") is
	// already <= 0 - some earlier transaction (an ordinary prior win/
	// rollback, or a settlement-timeout sweep) has already resolved this
	// lock. Never released a second time.
	ErrLockAlreadyReleased = errors.New("casino: this round's locked stake has already been released by an earlier transaction")
	// ErrBonusBetNotLocked is §16.4's Step-2 "player_bonus, no Step-1
	// lock found" row: under §16.10.1's mandated shape, a bonus-funded
	// casino bet must ALWAYS lock (case B). A bare player_bonus debit leg
	// with no matching Step-1 lock is a structural inconsistency (the
	// disallowed immediate-absorb shape was posted somehow), never a
	// second legitimate origin - never silently credited.
	ErrBonusBetNotLocked = errors.New("casino: bet's own debit leg is player_bonus with no corresponding lock; the disallowed immediate-absorb shape may have been posted")
	// ErrLockedBonusGrantMissing is a casino-side integrity finding (not
	// itself named by doc 08 §16, which assumes a bonus-funded postBet
	// that attributes the Grant already exists): a player_locked_bonus
	// origin was resolved, but no grant_ledger_attributions row names
	// which Grant it belongs to. Under the mandated case-B lock shape,
	// every locked-bonus bet MUST be Grant-attributed at bet time - this
	// is never guessed (which Grant's advisory lock/live status read
	// would even be correct is unknowable), only raised as a loud
	// integrity alert.
	ErrLockedBonusGrantMissing = errors.New("casino: player_locked_bonus origin has no grant_ledger_attributions row naming its Grant")
	// ErrHeldDispositionAlreadyVoided is returned by postRollback when a
	// genuinely distinct new rollback reference names a
	// bonus_held_dispositions record that a DIFFERENT, earlier rollback
	// already transitioned to voided_by_rollback (08 §16.15's "two
	// genuinely distinct rollback attempts" proof, the race loser's
	// branch) - never a second disposition of the same held value.
	ErrHeldDispositionAlreadyVoided = errors.New("casino: this held disposition was already voided by an earlier rollback")
	// ErrHeldDispositionRollbackUnsupported is LF-10 (08 §16.20, still
	// open, ledger-finance's decision, not casino's): a rollback names a
	// bonus_held_dispositions record that has already moved to a terminal
	// disposition (resolved_reforfeit/resolved_route_to_cash) - the held
	// value has already left player_bonus_held entirely, potentially
	// already spent or converted. This document/package does not
	// pre-select "permit a negative balance as a clawback," "route the
	// shortfall to a receivable," or "reject-and-alert" - it only fails
	// closed, posting nothing, never guessing, never silently reusing
	// §16.15's still-held mechanism for this structurally different case.
	ErrHeldDispositionRollbackUnsupported = errors.New("casino: rollback of an already-resolved held disposition is LF-10 (ledger-finance's open decision); refusing to guess a resolution")

	// --- Stage 8 (docs/decisions/0080-provider-integration-readiness-
	// without-external-contracts.md, Decision 1) - casino_provider_rounds
	// binding. ---

	// ErrProviderRoundOwnershipConflict is returned by BindProviderRound
	// when a provider_round_id already bound to a DIFFERENT
	// launch_session_id/player_account_id/brand_id (same tenant, same
	// provider) is bet on again - the exact cross-player/cross-tenant/
	// cross-brand round-id collision Stage 8 requires be rejected outright
	// rather than silently overwritten (a provider's round-id namespace is
	// never assumed to distinguish players/sessions on its own - see the
	// ADR's own "Uniqueness scope" discussion).
	ErrProviderRoundOwnershipConflict = errors.New("casino: provider_round_id is already bound to a different session/player/brand")
	// ErrProviderRoundNotFound is returned by LookupProviderRound when no
	// casino_provider_rounds row exists for the given
	// (tenant_id, provider_id, provider_round_id) - e.g. no bet has ever
	// been posted for that round yet.
	ErrProviderRoundNotFound = errors.New("casino: provider round not found")

	// --- Migration 0084 / ADR 0081 (ARCH-DB-2) ---

	// ErrTransactionScope is returned by UpsertGame when tx is not a
	// genuinely platform-admin-scoped transaction
	// (app.platform_admin_principal_id set AND app.tenant_id/
	// app.player_account_id both unset) - a SERVER-side caller bug, not a
	// client input error, mirroring internal/jurisdiction's own
	// ErrTransactionScope/assertPlatformScope exactly: this is a REAL,
	// in-function control, not defence-in-depth commentary, since migration
	// 0084's casino_games_platform_admin_insert/update RLS policies enforce
	// the identical predicate independently at the database - a caller that
	// bypassed this check would still fail at the INSERT/UPDATE, but with a
	// much less diagnosable error.
	ErrTransactionScope = errors.New("casino: requires a platform-admin-scoped transaction (use db.Pool.WithPlatformAdmin)")

	// --- Stage 9.2 (ADR 0081 §5/§5.2, ARCH-DB-2 Phase 2) - four-eyes
	// governance for casino_games.jurisdiction_blocklist removals and
	// status 'disabled'->'active' re-enablement (migration 0086). ---

	// ErrDualControlRequired is returned when a jurisdiction_unblock/
	// status_activate mutation has no independently-approved
	// casino_catalogue_change_requests row behind it. The authoritative
	// refusal is migration 0086's casino_games_dual_control trigger; this
	// sentinel lets the HTTP layer answer 409 rather than 500.
	ErrDualControlRequired = errors.New("casino: operation requires an approved catalogue change request by a different platform principal (four-eyes, ADR 0081 §5)")
	// ErrSelfApproval is returned when a principal tries to approve or
	// reject its own catalogue change request - including through a second
	// staff account resolving to the same Person.
	ErrSelfApproval = errors.New("casino: a principal may not approve its own catalogue change request")
	// ErrDuplicateDecision is returned when a principal that has ALREADY
	// decided a request submits a second decision on it - the only
	// reachable unique-constraint violation on
	// casino_catalogue_change_approvals is
	// UNIQUE (request_id, approver_principal_id), mirrors
	// assetregistry.ErrDuplicateDecision's identical rationale: a retry is
	// not the same thing as self-dealing.
	ErrDuplicateDecision = errors.New("casino: this principal has already decided this catalogue change request")
	// ErrChangeRequestNotFound covers "no such casino_catalogue_change_requests row".
	ErrChangeRequestNotFound = errors.New("casino: catalogue change request not found")
	// ErrChangeRequestNotPending is returned by DecideChangeRequest when
	// the named request has already left the 'pending' state (applied,
	// rejected, or cancelled) - a decision on a request that is no longer
	// pending is never silently accepted, mirroring
	// internal/withdrawal.Approve's own lockRequestForUpdate/
	// ErrStateConflict pattern.
	ErrChangeRequestNotPending = errors.New("casino: catalogue change request is no longer pending")
	// ErrPrincipalNotEligible is returned when the requesting or deciding
	// principal does not resolve to a platform-scoped (tenant_id IS NULL)
	// staff_users row.
	ErrPrincipalNotEligible = errors.New("casino: principal is not eligible to request or decide a catalogue change (must be a platform-scoped staff principal)")
)

// Stage 4D-RG's LaunchGame/postBet eligibility denial (internal/
// rg.EvaluateEligibility) is deliberately NOT a sentinel error here -
// unlike every other failure mode above, a denial must still let its own
// audit record commit (see LaunchGameResult's own doc comment for why a
// Go error was tried and rejected: it silently rolled back the audit
// write along with everything else). LaunchGame reports it via
// LaunchGameResult.Denied/DenialCode; postBet via
// ReceiveCallbackResult.Outcome == OutcomeDeclined (mirroring the
// existing insufficient-funds decline shape exactly).

// AmountLimit is one (asset_code, min, max) row - present in the schema
// for a future stake-limit feature but not read or enforced anywhere in
// Stage 4A (responsible-gaming stake limits are explicitly out of scope -
// see ADR 0025 §4). Kept out of AdapterCapability/ProviderCapability
// entirely rather than added as a dead field, per CLAUDE.md's
// "no fake completion" rule: a struct field nothing reads is exactly the
// "false sense of enforcement" specialist review has repeatedly flagged
// elsewhere in this codebase (docs/decisions/0022 §2's
// required_approver_roles precedent).

// AdapterCapability is the adapter-declared layer only - "layer (a)" of
// ADR 0025 §4's two-layer split: static properties of the integration,
// returned verbatim by a CasinoProvider's Capabilities() method. Excludes
// tenant_id/brand_id/priority/status, which are tenant-config data
// (layer (b)) an adapter never asserts about itself.
type AdapterCapability struct {
	ProviderID           string
	SupportsCatalogue    bool
	SupportsLaunch       bool
	SupportsBalance      bool
	SupportsBet          bool
	SupportsWin          bool
	SupportsRollback     bool
	SupportedAssets      []string
	SupportedGameTypes   []string
	CallbackCapabilities CallbackCapability
}

// ProviderCapability is the full effective capability row: layer (a) plus
// layer (b), the operator-configured tenant-scoped fields (ADR 0025 §4).
// Mirrors casino_provider_capabilities (migration 0035). Resolved by
// whole-row replacement, never a per-field merge - see LoadCapability.
type ProviderCapability struct {
	AdapterCapability
	ID uuid.UUID
	// TenantID is always set. BrandID is nil for a tenant-wide row.
	TenantID uuid.UUID
	BrandID  *uuid.UUID
	Priority int
	Status   CapabilityStatus
}

// ProviderHealth mirrors payment-orchestration.md §6's health/circuit-
// breaker shape - an in-memory HealthStatus() return value, never a
// persisted table (identical rationale to internal/payments.ProviderHealth).
type ProviderHealth struct {
	ProviderID          string
	RollingSuccessRate  float64
	RollingLatencyP99Ms int64
	CircuitState        CircuitState
	LastUpdated         time.Time
}

// CatalogueEntry is one title a CasinoProvider's Catalogue() call
// returns - the provider's own declared facts about a game it offers.
// UpsertGame (catalogue.go) maps this into a casino_games
// row, keyed on (ProviderID, ProviderGameID) - the platform's own game id
// is assigned once, at first sync, and never re-derived from this shape
// again (ADR 0025 §2: "provider game identifier... never promoted to
// platform identity").
type CatalogueEntry struct {
	ProviderGameID  string
	Name            string
	GameType        string
	RTPVariant      string
	Volatility      string
	FeatureFlags    []string
	SupportedAssets []string
	MobileSupported bool
	DemoSupported   bool
}

// LaunchRequest is the canonical, adapter-agnostic shape for asking a
// CasinoProvider to produce a launch URL. Per ADR 0025 §1, no free-form
// passthrough field exists - every field is explicitly enumerated.
// PlayerAccountID is the platform's own opaque id, never an email/name/
// PII (ADR 0025 §3: "minimizing exposed player data").
type LaunchRequest struct {
	ProviderGameID  string
	PlayerAccountID uuid.UUID
	AssetCode       string
	Mode            GameMode
	// LaunchToken is the platform's own opaque, single-use launch
	// credential (ADR 0025 §3) - passed through so the adapter can embed
	// it in the provider's own launch URL for the provider to present
	// back on its own wallet-callback calls. Never the player's JWT.
	LaunchToken string
	// SessionID is the platform's own casino_launch_sessions row id for
	// this round - distinct from LaunchToken (which is single-use and
	// consumed once at bootstrap). The adapter is expected to have its
	// provider echo THIS identifier back on every subsequent bet/win/
	// rollback callback, so ReceiveCallback can resolve player/wallet/
	// asset/mode from the platform's own session record instead of
	// trusting a payload-supplied player_account_id (specialist review
	// finding, security/multi-tenancy/architect reviews converging
	// independently: a callback with no session binding lets a valid,
	// correctly-signed provider misdirect funds to an arbitrary player
	// within the tenant, and cannot distinguish a demo-mode round from a
	// real-money one).
	SessionID uuid.UUID
}

// LaunchResult is what a CasinoProvider adapter returns from Launch.
type LaunchResult struct {
	Outcome       Outcome
	LaunchURL     string
	DeclineReason string
}

// BalanceRequest/BalanceResult let a provider (or, in Stage 4A, the mock)
// query the platform-resolved balance for a game session - read-only,
// posts nothing. AssetCode/Amount are int64 minor units, matching
// internal/ledger's convention - never floating point.
type BalanceRequest struct {
	ProviderGameID  string
	PlayerAccountID uuid.UUID
	AssetCode       string
}

type BalanceResult struct {
	Amount    int64
	AssetCode string
}

// BetRequest/BetResult, WinRequest/WinResult, RollbackRequest/
// RollbackResult are the canonical shapes an adapter's own HandleCallback
// implementation translates a provider's specific wallet-callback payload
// into internally, if that provider's transport shape is "the provider
// calls the platform per-operation" (ADR 0025 §1). The orchestrator
// itself never calls these directly - it consumes the adapter's own
// HandleCallback output (CallbackEvent) uniformly regardless of adapter
// transport shape.
type BetRequest struct {
	ProviderTxID string
	RoundID      string
	Amount       int64
	AssetCode    string
}

type BetResult struct {
	Outcome       Outcome
	DeclineReason string
}

type WinRequest struct {
	ProviderTxID string
	RoundID      string
	Amount       int64
	AssetCode    string
}

type WinResult struct {
	Outcome       Outcome
	DeclineReason string
}

type RollbackRequest struct {
	ProviderTxID         string
	OriginalProviderTxID string
}

type RollbackResult struct {
	Outcome       Outcome
	DeclineReason string
}

// CallbackEventType distinguishes which of Flows 5-7 a parsed callback
// corresponds to.
type CallbackEventType string

const (
	CallbackEventBet      CallbackEventType = "bet"
	CallbackEventWin      CallbackEventType = "win"
	CallbackEventRollback CallbackEventType = "rollback"
)

// CallbackEvent is HandleCallback's canonical, parsed-and-verified
// output - no free-form passthrough field, per ADR 0025 §1. Signature
// verification happens inside HandleCallback BEFORE any payload field is
// used to construct this value (ADR 0025 §5) - an adapter that cannot
// verify the payload returns an error, never a CallbackEvent.
type CallbackEvent struct {
	EventType CallbackEventType
	// ProviderTxID is THIS event's own provider reference - for a
	// rollback this is the rollback's own reference, distinct from the
	// bet's/win's being rolled back.
	ProviderTxID string
	// OriginalProviderTxID is set only for EventType ==
	// CallbackEventRollback: the reference of the SPECIFIC bet or win
	// transaction being rolled back. A round with both a bet and a win to
	// roll back requires two separate rollback events, one per original
	// reference - financial-transaction-flows.md §7's "two independent
	// reversal postings, one per original" (ADR 0025 §6), mirroring
	// exactly how internal/payments' deposit-reversal handles one
	// original per reversal event.
	OriginalProviderTxID string
	// RoundID correlates a bet and its later win under the same
	// provider-declared round/spin, used only as the ledger transaction's
	// CorrelationID for traceability (directive item 13's "correlation
	// context to trace... provider transaction ID") - never used to
	// resolve amounts/accounts, which always come from the event's own
	// fields.
	RoundID         string
	ProviderGameID  string
	Amount          int64
	AssetCode       string
	Outcome         Outcome
	DeclineReason   string
	PlayerAccountID uuid.UUID
	// SessionID is the LaunchRequest.SessionID the platform handed this
	// provider at launch time, echoed back on this callback - required
	// for CallbackEventBet (postBet resolves the actual wallet/player/
	// mode from the platform's own casino_launch_sessions row this names,
	// never from PlayerAccountID above, which is payload-supplied and
	// therefore never authoritative for a financial write). Win/Rollback
	// derive their own accounts from the ledger's own prior entries
	// instead (see postWin/postRollback), so this field is not consulted
	// for those event types.
	SessionID uuid.UUID
}

// CasinoProvider is the interface every adapter (real aggregator or
// mock) implements identically - ADR 0025 §1. No provider SDK type or
// vendor-specific shape ever appears in this interface's method
// signatures; provider-specific behavior lives entirely inside the
// adapter's own implementation.
type CasinoProvider interface {
	Catalogue(ctx context.Context) ([]CatalogueEntry, error)
	Launch(ctx context.Context, req LaunchRequest) (LaunchResult, error)
	Balance(ctx context.Context, req BalanceRequest) (BalanceResult, error)
	Bet(ctx context.Context, req BetRequest) (BetResult, error)
	Win(ctx context.Context, req WinRequest) (WinResult, error)
	Rollback(ctx context.Context, req RollbackRequest) (RollbackResult, error)
	// HandleCallback verifies an inbound provider callback's signature -
	// over the raw bytes, strictly BEFORE any parsing (ADR 0022 §3 point
	// 7 / Stage 10.2 CAS-WH-TENANT-1, design §C1) - and only then parses
	// it into a canonical CallbackEvent. The orchestrator's actual entry
	// point (ADR 0025 §1), regardless of whether a given provider's own
	// transport shape is push (webhook) or the Bet/Win/Rollback methods
	// above being called synchronously by the provider's own game server.
	// cred is the single candidate credential the orchestrator already
	// resolved for (in.TenantID, in.ProviderID, key id) - never a
	// cross-tenant trial. A verification failure returns exactly
	// ErrCallbackSignatureInvalid; a post-verification structural failure
	// returns ErrCallbackMalformedBody.
	HandleCallback(ctx context.Context, in webhookauth.Inbound, cred webhookauth.Credential) (CallbackEvent, error)
	// Capabilities returns this adapter's own declared, static layer only
	// - never tenant/brand/priority/status (ADR 0025 §4).
	Capabilities() AdapterCapability
	HealthStatus(ctx context.Context) (ProviderHealth, error)
}
