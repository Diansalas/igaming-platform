package casino

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/bonus"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/wallet"
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
}

// NewOrchestrator constructs an Orchestrator over the given adapter
// registry (provider_id -> CasinoProvider implementation).
func NewOrchestrator(providers map[string]CasinoProvider) *Orchestrator {
	return &Orchestrator{providers: providers}
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
	// JurisdictionCode is the tenant's own configured jurisdiction, if
	// known - resolution of a per-player jurisdiction is TODO(jurisdiction),
	// the identical open scope boundary payment-orchestration.md §4
	// carries for payment routing. A nil value skips the jurisdiction
	// check entirely (fails open only in the sense that no jurisdiction
	// context exists yet to check against - never silently ignores an
	// actually-resolved jurisdiction). MUST be resolved server-side from
	// the platform's own configuration/resolver, exactly like every other
	// field on this struct - NEVER from client-supplied input (a request
	// body, header, or geo hint a caller controls) - multi-tenancy review
	// finding: this value is now persisted onto the launch session
	// (migration 0042) and reused as the risk scope for every bet in the
	// round, so a client-influenced value here would let a player pick a
	// jurisdiction that dodges a jurisdiction-scoped HARD_LIMIT for the
	// whole round, not just one request.
	JurisdictionCode *string
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
// provider's own capability and health, mints a single-use launch
// session (ADR 0025 §3), and calls the provider's Launch. Every failure
// mode returns a specific, distinguishable sentinel error (directive
// items C/O/P/Q depend on this - "game doesn't exist" vs. "not enabled
// here" vs. "provider unavailable" vs. "blocked in this jurisdiction"
// are never collapsed into one generic not-found).
func (o *Orchestrator) LaunchGame(ctx context.Context, tx pgx.Tx, params LaunchGameParams) (LaunchGameResult, error) {
	if params.TenantID == uuid.Nil || params.BrandID == uuid.Nil || params.PlayerAccountID == uuid.Nil || params.WalletID == uuid.Nil || params.GameID == uuid.Nil {
		return LaunchGameResult{}, fmt.Errorf("%w: launch requires fully-populated, server-derived identity fields", ErrInvalidInput)
	}
	if params.AssetCode == "" {
		return LaunchGameResult{}, fmt.Errorf("%w: asset_code is required", ErrInvalidInput)
	}
	if params.Mode != ModeReal && params.Mode != ModeDemo {
		return LaunchGameResult{}, fmt.Errorf("%w: mode must be 'real' or 'demo'", ErrInvalidInput)
	}

	game, err := GetGameByID(ctx, tx, params.GameID)
	if err != nil {
		return LaunchGameResult{}, err
	}
	if game.Status != GameStatusActive {
		return LaunchGameResult{}, ErrGameDisabled
	}
	available, err := IsGameAvailable(ctx, tx, params.TenantID, params.BrandID, params.GameID)
	if err != nil {
		return LaunchGameResult{}, err
	}
	if !available {
		return LaunchGameResult{}, ErrGameNotAvailable
	}
	if params.JurisdictionCode != nil && containsString(game.JurisdictionBlocklist, *params.JurisdictionCode) {
		return LaunchGameResult{}, ErrJurisdictionBlocked
	}
	if !containsString(game.SupportedAssets, params.AssetCode) {
		return LaunchGameResult{}, fmt.Errorf("%w: game does not support asset %s", ErrInvalidInput, params.AssetCode)
	}

	// Stage 4D-RG: the single authoritative "may this player gamble right
	// now" policy boundary (ADR 0026 §5/§6), consulted BEFORE a launch
	// session is minted - a prohibited player (suspended, self-excluded
	// anywhere on the platform via their cross-brand Person, or holding a
	// non-active wallet) must never obtain a usable session, regardless of
	// what any provider does or does not enforce on its own end.
	decision, err := evaluateAndAuditEligibility(ctx, tx, params.TenantID, params.BrandID, params.PlayerAccountID, params.WalletID, "casino.launch_denied")
	if err != nil {
		return LaunchGameResult{}, err
	}
	if !decision.Allowed {
		return LaunchGameResult{Denied: true, DenialCode: decision.Code, DenialMessage: decision.Message}, nil
	}

	// Resolved once, used both by the risk check below (real-mode only)
	// and persisted onto the launch session unconditionally just below
	// that (Stage 4G-FINAL Part C) - so a jurisdiction-scoped rule stays
	// reachable from this SAME round's later bets (postBet), not just at
	// launch time. Empty when LaunchGame itself had no resolved
	// jurisdiction to begin with (TODO(jurisdiction) - see
	// LaunchGameParams' own doc comment) - never silently defaulted.
	var jurisdictionCode string
	if params.JurisdictionCode != nil {
		jurisdictionCode = *params.JurisdictionCode
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
			return LaunchGameResult{}, err
		}
		riskDecision, err := evaluateAndAuditRisk(ctx, tx, risk.RiskRequest{
			TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID,
			Operation: risk.OperationCasinoLaunch, Product: "casino", ProviderID: game.ProviderID, GameID: params.GameID,
			AssetCode: params.AssetCode, JurisdictionCode: jurisdictionCode, LicensingMode: licensingMode,
		}, "casino.launch_denied_by_risk_policy")
		if err != nil {
			return LaunchGameResult{}, err
		}
		// ALLOW / REVIEW / DENY / error-or-unavailable are FOUR distinct
		// outcomes, classified in one shared place rather than collapsed
		// into "not allow" at each call site (ADR 0031 §34).
		proceed, err := classifyRiskOutcome(riskDecision.Outcome)
		if err != nil {
			return LaunchGameResult{}, err
		}
		if !proceed {
			return LaunchGameResult{Denied: true, DenialCode: riskDecision.Code, DenialMessage: riskDecision.Message}, nil
		}
	}

	capability, found, err := LoadCapability(ctx, tx, params.TenantID, params.BrandID, game.ProviderID)
	if err != nil {
		return LaunchGameResult{}, err
	}
	if !found || capability.Status != CapabilityActive || !capability.SupportsLaunch {
		return LaunchGameResult{}, ErrProviderUnavailable
	}
	if !containsString(capability.SupportedAssets, params.AssetCode) {
		return LaunchGameResult{}, ErrProviderUnavailable
	}

	provider, registered := o.providers[game.ProviderID]
	if !registered {
		return LaunchGameResult{}, fmt.Errorf("%w: %s", ErrUnknownProvider, game.ProviderID)
	}
	if health, err := provider.HealthStatus(ctx); err == nil && health.CircuitState == CircuitOpen {
		return LaunchGameResult{}, ErrProviderUnavailable
	}

	session, token, err := CreateLaunchSession(ctx, tx, CreateLaunchSessionParams{
		TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID, WalletID: params.WalletID,
		GameID: params.GameID, ProviderID: game.ProviderID, ProviderGameID: game.ProviderGameID,
		AssetCode: params.AssetCode, Mode: params.Mode, JurisdictionCode: jurisdictionCode,
	})
	if err != nil {
		return LaunchGameResult{}, err
	}

	result, err := provider.Launch(ctx, LaunchRequest{
		ProviderGameID: game.ProviderGameID, PlayerAccountID: params.PlayerAccountID,
		AssetCode: params.AssetCode, Mode: params.Mode, LaunchToken: token, SessionID: session.ID,
	})
	if err != nil {
		// The launch call itself failed at the transport level - the
		// session was never actually usable, so revoke it rather than
		// leaving an 'active' row a retried launch attempt could never
		// reach (a fresh LaunchGame call mints its own new session
		// instead of trying to reuse this one).
		_ = RevokeLaunchSession(ctx, tx, session.ID)
		return LaunchGameResult{}, fmt.Errorf("casino: provider launch call failed: %w", err)
	}
	if result.Outcome != OutcomeSucceeded {
		_ = RevokeLaunchSession(ctx, tx, session.ID)
		return LaunchGameResult{}, fmt.Errorf("casino: provider declined launch: %s", result.DeclineReason)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.TenantID, ActorType: audit.ActorPlayer, ActorID: params.PlayerAccountID,
		Action: "casino.launched", TargetType: "casino_launch_session", TargetID: session.ID.String(),
		Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"game_id": params.GameID.String(), "provider_id": game.ProviderID, "provider_game_id": game.ProviderGameID,
			"asset_code": params.AssetCode, "mode": string(params.Mode),
		},
	}); err != nil {
		return LaunchGameResult{}, fmt.Errorf("casino: audit launch: %w", err)
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
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: req.TenantID, ActorType: audit.ActorSystem, Action: auditAction,
		TargetType: "player_account", TargetID: req.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
		Metadata: metadata,
	}); err != nil {
		return risk.RiskDecision{}, fmt.Errorf("casino: audit risk denial: %w", err)
	}
	return decision, nil
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
}

// ReceiveCallback dispatches a verified provider callback: parses it via
// the named adapter's HandleCallback, then posts Flow 5 (bet), Flow 6
// (win), or Flow 7 (rollback) via ledger.Post.
//
// tenantID is resolved by the caller (an HTTP handler, from a per-tenant
// webhook path) BEFORE opening tx and BEFORE calling this function -
// never from rawPayload, mirroring internal/payments.Orchestrator.
// ReceiveCallback's identical, already-reviewed signature choice and
// rationale (payment-orchestration.md §3).
func (o *Orchestrator) ReceiveCallback(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, rawPayload []byte) (ReceiveCallbackResult, error) {
	provider, ok := o.providers[providerID]
	if !ok {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}

	event, err := provider.HandleCallback(ctx, rawPayload)
	if err != nil {
		// Never wrap rawPayload's bytes into this error.
		return ReceiveCallbackResult{}, fmt.Errorf("casino: handle callback: %w", err)
	}

	// Enforce the tenant's own CasinoProviderCapability as an actual kill
	// switch on the money path, not just at launch time (specialist review
	// finding: a tenant disabling this provider's capability, or never
	// configuring one at all, previously had NO effect here - the process-
	// global adapter registry alone decided whether a callback was
	// accepted). Checked tenant-wide (brand_id NULL) - the same capability
	// row LaunchGame itself resolves for a tenant-wide route.
	capability, found, err := LoadCapability(ctx, tx, tenantID, uuid.Nil, providerID)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if !found || capability.Status != CapabilityActive {
		return ReceiveCallbackResult{}, ErrProviderUnavailable
	}

	switch event.EventType {
	case CallbackEventBet:
		if !capability.SupportsBet {
			return ReceiveCallbackResult{}, ErrProviderUnavailable
		}
		return o.postBet(ctx, tx, tenantID, providerID, event)
	case CallbackEventWin:
		if !capability.SupportsWin {
			return ReceiveCallbackResult{}, ErrProviderUnavailable
		}
		return o.postWin(ctx, tx, tenantID, providerID, event)
	case CallbackEventRollback:
		if !capability.SupportsRollback {
			return ReceiveCallbackResult{}, ErrProviderUnavailable
		}
		return o.postRollback(ctx, tx, tenantID, providerID, event)
	default:
		return ReceiveCallbackResult{}, fmt.Errorf("casino: unsupported callback event type %q", event.EventType)
	}
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

// lockCashBalance takes a row lock on the wallet's player_cash
// wallet_balance_projection row and returns its raw debit/credit
// totals - identical pattern and rationale to
// internal/withdrawal.lockCashBalanceForUpdate (invariant #15: the
// balance read and the prospective debit happen inside the same
// transaction, never a stale check-then-post).
func lockCashBalance(ctx context.Context, tx pgx.Tx, ledgerAccountID uuid.UUID) (debitTotal, creditTotal int64, err error) {
	err = tx.QueryRow(ctx,
		`SELECT debit_total, credit_total FROM wallet_balance_projection WHERE ledger_account_id = $1 FOR UPDATE`,
		ledgerAccountID,
	).Scan(&debitTotal, &creditTotal)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("casino: lock cash balance: %w", err)
	}
	return debitTotal, creditTotal, nil
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
	if _, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('casino_bet_delivery:' || $1::text || ':' || $2 || ':' || $3, 0))`,
		tenantID, providerID, event.ProviderTxID,
	); err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: acquire bet delivery lock: %w", err)
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
		return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &existingID}, nil
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
	if session.Status == LaunchSessionRevoked {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: session has been revoked", ErrLaunchSessionRequired)
	}
	if session.Mode != ModeReal {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: session is demo-mode, cannot post a real financial effect", ErrLaunchSessionRequired)
	}
	if session.AssetCode != event.AssetCode {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: asset_code does not match the launch session", ErrInvalidInput)
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

	cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, &wl.ID, ledger.AccountPlayerCash, event.AssetCode)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve player_cash account: %w", err)
	}
	houseAccountID, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, nil, ledger.AccountHouseGaming, event.AssetCode)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve house_gaming account: %w", err)
	}

	// Invariant #15: lock and check the balance INSIDE this transaction,
	// immediately before posting - a concurrent bet against the same
	// wallet cannot both observe "sufficient" (financial-transaction-
	// flows.md §5's "insufficient funds -> rejected before posting,
	// checked in the same DB transaction that would post it").
	debitTotal, creditTotal, err := lockCashBalance(ctx, tx, cashAccountID)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	available := creditTotal - debitTotal
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

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
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
	})
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: post bet: %w", err)
	}

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

	return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID}, nil
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

	correlationID := roundCorrelationID(tenantID, providerID, event.RoundID)
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
		return ReceiveCallbackResult{Tombstoned: true, LedgerTransactionID: &originalID}, nil
	}
	if originalType != ledger.TxCasinoBet && originalType != ledger.TxCasinoWin {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: original transaction %s has type %q, expected casino_bet or casino_win",
			ErrInvalidInput, originalID, originalType)
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

	return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID}, nil
}

func postRollbackTombstone(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent) (uuid.UUID, error) {
	// ReasonCode left nil - ledger_transactions' CHECK constraint
	// (migration 0021) forbids it for any type other than
	// manual_adjustment. The human-readable reason lives in this
	// function's caller's audit record instead.
	result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxTombstone,
		IdempotencyKey: fmt.Sprintf("tombstone:%s:%s", providerID, event.OriginalProviderTxID),
		ProviderID:     &providerID, ProviderTxID: &event.OriginalProviderTxID,
		CorrelationID: uuid.New(),
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
	rows, err := tx.Query(ctx,
		`SELECT e.ledger_account_id, e.direction, e.amount, la.account_type
		   FROM ledger_entries e
		   JOIN ledger_accounts la ON la.id = e.ledger_account_id
		  WHERE e.ledger_transaction_id = $1`,
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
