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
	// ErrJurisdictionBlocked is returned when the game's own
	// jurisdiction_blocklist contains the tenant's configured jurisdiction.
	ErrJurisdictionBlocked = errors.New("casino: game is blocked in this jurisdiction")
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
	// ErrCallbackSignatureInvalid is returned by a CasinoProvider's
	// HandleCallback when the payload's authentication does not verify.
	// Never wrapped with the raw payload or any field from it.
	ErrCallbackSignatureInvalid = errors.New("casino: callback signature verification failed")

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
// UpsertGamesFromCatalogue (catalogue.go) maps this into a casino_games
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
	// HandleCallback verifies an inbound provider callback's signature
	// and parses it into a canonical CallbackEvent - the orchestrator's
	// actual entry point (ADR 0025 §1), regardless of whether a given
	// provider's own transport shape is push (webhook) or the
	// Bet/Win/Rollback methods above being called synchronously by the
	// provider's own game server.
	HandleCallback(ctx context.Context, rawPayload []byte) (CallbackEvent, error)
	// Capabilities returns this adapter's own declared, static layer only
	// - never tenant/brand/priority/status (ADR 0025 §4).
	Capabilities() AdapterCapability
	HealthStatus(ctx context.Context) (ProviderHealth, error)
}
