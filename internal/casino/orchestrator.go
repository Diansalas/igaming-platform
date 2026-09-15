package casino

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
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
	// actually-resolved jurisdiction).
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

	// Stage 4G: the central Risk & Limits boundary, consulted alongside
	// (never instead of) RG eligibility above - a separate domain, per
	// ADR 0031 §1. Skipped for demo-mode launches: no real financial
	// exposure exists yet to gate. A REVIEW outcome is treated identically
	// to DENY at this integration point (ADR 0031 §6 - no
	// compliance-review workflow exists yet for casino launch, so a
	// review-flagged launch fails safe by blocking rather than proceeding
	// provisionally).
	if params.Mode == ModeReal {
		var jurisdictionCode string
		if params.JurisdictionCode != nil {
			jurisdictionCode = *params.JurisdictionCode
		}
		riskDecision, err := evaluateAndAuditRisk(ctx, tx, risk.RiskRequest{
			TenantID: params.TenantID, BrandID: params.BrandID, PlayerAccountID: params.PlayerAccountID,
			Operation: risk.OperationCasinoLaunch, Product: "casino", ProviderID: game.ProviderID, GameID: params.GameID,
			AssetCode: params.AssetCode, JurisdictionCode: jurisdictionCode,
		}, "casino.launch_denied_by_risk_policy")
		if err != nil {
			return LaunchGameResult{}, err
		}
		if riskDecision.Outcome != risk.OutcomeAllow {
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
		AssetCode: params.AssetCode, Mode: params.Mode,
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
	// JurisdictionCode is deliberately left empty here (multi-tenancy/
	// architecture specialist review, disclosed - ADR 0031 §8): unlike
	// LaunchGameParams, casino_launch_sessions does not persist the
	// jurisdiction resolved at launch time, and no other source of a
	// per-bet jurisdiction exists in this codebase today (the same
	// already-documented "TODO(jurisdiction)" gap LaunchGameParams'
	// own doc comment names). A jurisdiction-scoped risk rule is
	// therefore NEVER reachable from postBet, only from LaunchGame -
	// express a legal/jurisdiction constraint that must also bind
	// bet-time as a tenant-scoped (or platform-wide) rule instead until
	// jurisdiction is persisted on the launch session.
	riskDecision, err := evaluateAndAuditRisk(ctx, tx, risk.RiskRequest{
		TenantID: tenantID, BrandID: session.BrandID, PlayerAccountID: session.PlayerAccountID,
		Operation: risk.OperationCasinoBet, Product: "casino", ProviderID: providerID, GameID: session.GameID, AssetCode: event.AssetCode,
		Amount: event.Amount, CorrelationID: roundCorrelationID(tenantID, providerID, event.RoundID),
	}, "casino_bet.denied_by_risk_policy")
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if riskDecision.Outcome != risk.OutcomeAllow {
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

// postWin implements Flow 6 (financial-transaction-flows.md §6): debit
// house_gaming, credit player_cash. A win naming a round with no matching,
// still-valid (never rolled back) prior bet is an integrity alert (a
// provider protocol violation), not a routine failure - logged/audited at
// elevated severity by the HTTP handler, which maps ErrBetNotFound
// distinctly from an ordinary not-found.
//
// The wallet a win credits is resolved from the round's OWN bet
// transaction's own ledger entries - the SAME player_cash account that
// bet actually debited - never from event.PlayerAccountID (specialist
// review finding, empirically reproduced during review: a win naming a
// DIFFERENT player_account_id than the one who placed the round's bet was
// previously credited to that different player in full). Deriving from
// the bet's own ledger-truth entries is a stronger anchor than a session
// lookup here: it is impossible for a win to be misdirected to any wallet
// other than the one the round's own bet is already proven to have used.
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

	var betWalletID uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT le.wallet_id
		 FROM ledger_transactions lt
		 JOIN ledger_entries le ON le.ledger_transaction_id = lt.id AND le.direction = 'debit'
		 WHERE lt.tenant_id = $1 AND lt.correlation_id = $2 AND lt.transaction_type = $3
		   AND NOT EXISTS (SELECT 1 FROM ledger_transactions r WHERE r.reverses_transaction_id = lt.id)
		 LIMIT 1`,
		tenantID, roundCorrelationID(tenantID, providerID, event.RoundID), ledger.TxCasinoBet,
	).Scan(&betWalletID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Covers both "no bet was ever posted for this round" and "the
		// round's bet was already rolled back" (a win on a voided round is
		// the same class of integrity violation as an orphan win).
		return ReceiveCallbackResult{}, fmt.Errorf("%w: round=%s provider=%s", ErrBetNotFound, event.RoundID, providerID)
	}
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: check prior bet: %w", err)
	}

	wl, err := wallet.GetByID(ctx, tx, betWalletID)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve wallet: %w", err)
	}
	if wl.AssetCode != event.AssetCode {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: asset_code does not match the round's own bet", ErrInvalidInput)
	}

	cashAccountID, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, &wl.ID, ledger.AccountPlayerCash, event.AssetCode)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve player_cash account: %w", err)
	}
	houseAccountID, err := ledger.GetOrCreateAccount(ctx, tx, tenantID, nil, ledger.AccountHouseGaming, event.AssetCode)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: resolve house_gaming account: %w", err)
	}

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxCasinoWin,
		IdempotencyKey: providerID + ":" + event.ProviderTxID,
		ProviderID:     &providerID, ProviderTxID: &event.ProviderTxID,
		CorrelationID: roundCorrelationID(tenantID, providerID, event.RoundID),
		Entries: []ledger.EntryInput{
			{LedgerAccountID: houseAccountID, Direction: ledger.Debit, Amount: event.Amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: event.Amount},
		},
	})
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: post win: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: "casino_win.posted",
		TargetType: "ledger_transaction", TargetID: postResult.TransactionID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"provider_id": providerID, "provider_tx_id": event.ProviderTxID, "round_id": event.RoundID,
			"amount": event.Amount, "asset_code": event.AssetCode, "already_posted": postResult.AlreadyPosted,
		},
	}); err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: audit win posted: %w", err)
	}

	return ReceiveCallbackResult{Outcome: OutcomeSucceeded, LedgerTransactionID: &postResult.TransactionID}, nil
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

	rollbackTxType := ledger.TxCasinoRollback
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: rollbackTxType,
		IdempotencyKey:        providerID + ":" + event.ProviderTxID,
		ProviderID:            &providerID,
		ProviderTxID:          &event.ProviderTxID,
		CorrelationID:         roundCorrelationID(tenantID, providerID, event.RoundID),
		ReversesTransactionID: &originalID,
		Entries:               inverted,
	})
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("casino: post rollback: %w", err)
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
}

func loadEntries(ctx context.Context, tx pgx.Tx, transactionID uuid.UUID) ([]ledgerEntry, error) {
	rows, err := tx.Query(ctx,
		`SELECT ledger_account_id, direction, amount FROM ledger_entries WHERE ledger_transaction_id = $1`,
		transactionID,
	)
	if err != nil {
		return nil, fmt.Errorf("casino: load entries: %w", err)
	}
	defer rows.Close()

	var out []ledgerEntry
	for rows.Next() {
		var e ledgerEntry
		if err := rows.Scan(&e.LedgerAccountID, &e.Direction, &e.Amount); err != nil {
			return nil, fmt.Errorf("casino: scan entry: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
