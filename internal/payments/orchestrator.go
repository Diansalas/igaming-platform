package payments

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/rg"
)

// defaultMaxCascadeDepth bounds how many distinct providers a single
// deposit attempt may cascade through (payment-orchestration.md §5:
// "up to a configurable cascade depth"). Configurable per Orchestrator via
// MaxCascadeDepth, not a compile-time constant tenants are stuck with.
const defaultMaxCascadeDepth = 3

// Orchestrator is the PaymentOrchestrator (payment-orchestration.md §3):
// it resolves which PaymentProvider handles a deposit and translates
// verified provider callbacks into ledger postings. It never posts a
// ledger entry itself outside of calling ledger.Post with the canonical
// Flow 1/Flow 2 shapes - no provider-specific branch exists anywhere in
// this file (docs/decisions/0022 §1).
type Orchestrator struct {
	// providers is the Go-level adapter registry keyed by provider_id -
	// the authority on which PaymentProvider implementation a
	// provider_id actually is (docs/decisions/0022 §2.1: "a capability
	// row describes an adapter; it never promotes one"). A
	// provider_capabilities row naming a provider_id absent from this map
	// is a data/deploy mismatch, defensively skipped by RouteProvider
	// rather than treated as a routable candidate.
	providers map[string]PaymentProvider
	// MaxCascadeDepth bounds cascade-on-decline (payment-orchestration.md
	// §5). Defaults to defaultMaxCascadeDepth when <= 0.
	MaxCascadeDepth int
}

// NewOrchestrator constructs an Orchestrator over the given adapter
// registry (provider_id -> PaymentProvider implementation).
func NewOrchestrator(providers map[string]PaymentProvider) *Orchestrator {
	return &Orchestrator{providers: providers, MaxCascadeDepth: defaultMaxCascadeDepth}
}

// Provider returns the registered adapter for providerID, per
// docs/decisions/0022 §2.1's "the adapter registry is the authority" -
// used by admin/config code (e.g. writing a ProviderCapability row) that
// needs the actual adapter instance rather than routing through it.
func (o *Orchestrator) Provider(providerID string) (PaymentProvider, bool) {
	p, ok := o.providers[providerID]
	return p, ok
}

func (o *Orchestrator) maxCascadeDepth() int {
	if o.MaxCascadeDepth <= 0 {
		return defaultMaxCascadeDepth
	}
	return o.MaxCascadeDepth
}

// OperationKind distinguishes deposit-direction from withdrawal-direction
// routing, since a ProviderCapability's supports_deposit/
// supports_withdrawal flags differ independently
// (docs/decisions/0022 §2). Withdrawal routing is not exercised by any
// orchestrator method this stage (withdrawal orchestration is NOT
// IMPLEMENTED - see the package doc comment) but RouteProvider is
// intentionally shared, unmodified, for whichever stage implements it
// next, per payment-orchestration.md §4's own statement that "this
// routing order is written for deposits and is applied to withdrawals
// as-is."
type OperationKind string

const (
	OperationDeposit    OperationKind = "deposit"
	OperationWithdrawal OperationKind = "withdrawal"
)

// RoutingRequest is RouteProvider's input - every filterable dimension
// payment-orchestration.md §4 requires MINUS jurisdiction/country
// (TODO(jurisdiction) below).
type RoutingRequest struct {
	TenantID      uuid.UUID
	BrandID       uuid.UUID
	AssetCode     string
	PaymentMethod string
	Amount        int64
	Operation     OperationKind
	// ExcludeProviderIDs holds provider_ids already attempted within this
	// deposit's own cascade chain (payment-orchestration.md §5), so a
	// repeated RouteProvider call during cascade never re-selects a
	// provider that already produced a cascadable decline for this same
	// business operation.
	//
	// TODO(jurisdiction): country/jurisdiction-based filtering
	// (payment-orchestration.md §4 step 2, "drops providers not
	// licensed/permitted to serve that country") requires
	// tenant_jurisdiction_configs integration (Stage 1) and is out of
	// scope for Stage 3B, per the task's own explicit scope note.
	//
	// Stage 4I note (docs/governance/stage-4i-payments-model.md): when
	// this dimension is finally built, it is a regulatory gate resolved
	// via internal/jurisdiction.Resolve against the platform's own
	// jurisdiction/licence model - it is a SEPARATE mechanism from
	// AdapterCapability.SupportedCountries (a provider's own declared
	// market/rail coverage, ISO-3166 code space) and must not reuse that
	// field's "empty = unrestricted" default as its own absent-value
	// contract; see the longer note on SupportedCountries in types.go.
	ExcludeProviderIDs []string
}

// RouteProvider implements payment-orchestration.md §4's routing
// dimensions 1 (tenant/brand, via ListRoutingCandidates being scoped to
// tenant/brand already), 3 (currency/asset), 4 (payment method), 5
// (amount), and 6 (provider health) - dimension 2 (jurisdiction) is
// TODO(jurisdiction) per RoutingRequest's doc comment. Candidates are
// filtered down to those active, registered, healthy (circuit not open),
// and matching every dimension, then ranked by health (higher rolling
// success rate first, then lower p99 latency), with Priority breaking
// ties among "otherwise-equal" candidates exactly as
// docs/decisions/0022 §2 describes it - Priority is a tie-breaker, not
// the primary ranking key, because §6 of payment-orchestration.md states
// health ranking picks "the healthiest" among survivors.
func (o *Orchestrator) RouteProvider(ctx context.Context, tx pgx.Tx, req RoutingRequest) (PaymentProvider, ProviderCapability, error) {
	candidates, err := ListRoutingCandidates(ctx, tx, req.TenantID, req.BrandID)
	if err != nil {
		return nil, ProviderCapability{}, err
	}

	excluded := make(map[string]struct{}, len(req.ExcludeProviderIDs))
	for _, id := range req.ExcludeProviderIDs {
		excluded[id] = struct{}{}
	}

	type scoredCandidate struct {
		capability ProviderCapability
		provider   PaymentProvider
		health     ProviderHealth
	}
	var eligible []scoredCandidate

	for _, candidate := range candidates {
		if candidate.Status != CapabilityActive {
			continue
		}
		if _, isExcluded := excluded[candidate.ProviderID]; isExcluded {
			continue
		}
		if req.Operation == OperationWithdrawal {
			if !candidate.SupportsWithdrawal {
				continue
			}
		} else if !candidate.SupportsDeposit {
			continue
		}
		if !assetSupported(candidate, req.AssetCode) {
			continue
		}
		if !containsString(candidate.SupportedPaymentMethods, req.PaymentMethod) {
			continue
		}
		if !amountWithinLimits(candidate, req.AssetCode, req.Amount) {
			continue
		}

		provider, registered := o.providers[candidate.ProviderID]
		if !registered {
			continue
		}
		health, err := provider.HealthStatus(ctx)
		if err != nil {
			// Cannot confirm health this call - treated as unroutable
			// rather than optimistically routable; a genuinely healthy
			// provider whose health check itself failed will be
			// reconsidered on the next call.
			continue
		}
		if health.CircuitState == CircuitOpen {
			continue
		}

		eligible = append(eligible, scoredCandidate{capability: candidate, provider: provider, health: health})
	}

	if len(eligible) == 0 {
		return nil, ProviderCapability{}, ErrNoRoutableProvider
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		a, b := eligible[i], eligible[j]
		if a.health.RollingSuccessRate != b.health.RollingSuccessRate {
			return a.health.RollingSuccessRate > b.health.RollingSuccessRate
		}
		if a.health.RollingLatencyP99Ms != b.health.RollingLatencyP99Ms {
			return a.health.RollingLatencyP99Ms < b.health.RollingLatencyP99Ms
		}
		if a.capability.Priority != b.capability.Priority {
			return a.capability.Priority < b.capability.Priority
		}
		return a.capability.ProviderID < b.capability.ProviderID
	})

	best := eligible[0]
	return best.provider, best.capability, nil
}

func assetSupported(capability ProviderCapability, assetCode string) bool {
	return containsString(capability.SupportedFiatCurrencies, assetCode) || containsString(capability.SupportedCryptoAssets, assetCode)
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// amountWithinLimits returns true (no restriction) when the capability
// declares no amount_limits row for assetCode at all - a provider
// claiming support for an asset without declaring a limit for it is a
// configuration gap, not something RouteProvider silently treats as
// "unroutable"; WriteCapability's own validation is the place that
// governs what limits get configured in the first place.
func amountWithinLimits(capability ProviderCapability, assetCode string, amount int64) bool {
	for _, lim := range capability.AmountLimits {
		if lim.AssetCode == assetCode {
			return amount >= lim.MinAmount && amount <= lim.MaxAmount
		}
	}
	return true
}

// DepositScope carries only server-derived identifiers
// (payment-orchestration.md §3): tenant_id, brand_id, player_account_id,
// and wallet_id must all be resolved from the caller's own authenticated
// session/context before InitiateDeposit is called - never populated from
// client-supplied request fields. Carrying them in their own struct
// (rather than as bare InitiateDepositParams fields) is deliberate, per
// that section's own instruction, "precisely so that a future caller
// cannot pass a client-supplied value positionally without it being
// obvious in review."
type DepositScope struct {
	TenantID        uuid.UUID
	BrandID         uuid.UUID
	PlayerAccountID uuid.UUID
	WalletID        uuid.UUID
}

// InitiateDepositParams is InitiateDeposit's input. AssetCode/Amount/
// PaymentMethod/IdempotencyKey are the only caller-controlled fields -
// everything identifying WHO is depositing comes from Scope.
type InitiateDepositParams struct {
	Scope          DepositScope
	AssetCode      string
	Amount         int64
	PaymentMethod  string
	IdempotencyKey string
}

// DepositIntentStatus mirrors deposit_intents.status (migration 0025).
type DepositIntentStatus string

const (
	DepositIntentPending   DepositIntentStatus = "pending"
	DepositIntentSucceeded DepositIntentStatus = "succeeded"
	DepositIntentDeclined  DepositIntentStatus = "declined"
	DepositIntentAmbiguous DepositIntentStatus = "ambiguous"
	DepositIntentFailed    DepositIntentStatus = "failed"
)

// DepositIntent is the orchestrator's own workflow row
// (payment-orchestration.md §3, migration 0025) - distinct from any
// LedgerTransaction, which does not exist until a deposit actually
// succeeds (ledger-accounting-model.md §4).
type DepositIntent struct {
	ID                  uuid.UUID
	TenantID            uuid.UUID
	BrandID             uuid.UUID
	PlayerAccountID     uuid.UUID
	WalletID            uuid.UUID
	AssetCode           string
	Amount              int64
	PaymentMethod       string
	IdempotencyKey      string
	ProviderID          *string
	ProviderReference   *string
	Status              DepositIntentStatus
	LedgerTransactionID *uuid.UUID

	// RedirectURL/HostedFieldToken are transient - migration 0025 has no
	// column for either, since they are only meaningful once, at
	// initiation time (a caller redirects the player or mounts hosted
	// fields immediately using the value InitiateDeposit itself returns).
	// A later lookup of the same intent (e.g. a player polling their own
	// deposit's status) will not repopulate these fields - that is by
	// design, not a bug.
	RedirectURL      string
	HostedFieldToken string
}

const depositIntentColumns = `id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key, provider_id, provider_reference, status, ledger_transaction_id`

func scanDepositIntent(row rowScanner) (DepositIntent, error) {
	var d DepositIntent
	err := row.Scan(
		&d.ID, &d.TenantID, &d.BrandID, &d.PlayerAccountID, &d.WalletID, &d.AssetCode, &d.Amount, &d.PaymentMethod,
		&d.IdempotencyKey, &d.ProviderID, &d.ProviderReference, &d.Status, &d.LedgerTransactionID,
	)
	return d, err
}

// GetDepositIntentByID looks up a DepositIntent by id within the caller's
// current RLS scope - tenant staff scope (db.Pool.WithTenant) for back-
// office/system callers, or player scope (db.Pool.WithPlayerScope) for a
// player checking their own deposit's status, exactly like
// internal/withdrawal.GetByID's identical contract for withdrawal
// requests.
func GetDepositIntentByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (DepositIntent, error) {
	row := tx.QueryRow(ctx, `SELECT `+depositIntentColumns+` FROM deposit_intents WHERE id = $1`, id)
	d, err := scanDepositIntent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return DepositIntent{}, ErrDepositIntentNotFound
	}
	if err != nil {
		return DepositIntent{}, fmt.Errorf("payments: get deposit intent by id: %w", err)
	}
	return d, nil
}

// ListDepositIntentsForPlayer returns every DepositIntent belonging to
// playerAccountID, most recent first - intended to be called inside
// db.Pool.WithPlayerScope so RLS's player_self_scope policy (migration
// 0025) is what actually restricts visibility.
func ListDepositIntentsForPlayer(ctx context.Context, tx pgx.Tx, playerAccountID uuid.UUID) ([]DepositIntent, error) {
	rows, err := tx.Query(ctx,
		`SELECT `+depositIntentColumns+` FROM deposit_intents WHERE player_account_id = $1 ORDER BY created_at DESC`,
		playerAccountID,
	)
	if err != nil {
		return nil, fmt.Errorf("payments: list deposit intents for player: %w", err)
	}
	defer rows.Close()

	var out []DepositIntent
	for rows.Next() {
		d, err := scanDepositIntent(rows)
		if err != nil {
			return nil, fmt.Errorf("payments: scan deposit intent: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func loadDepositIntentByIdempotencyKey(ctx context.Context, tx pgx.Tx, tenantID, playerAccountID uuid.UUID, key string) (DepositIntent, bool, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+depositIntentColumns+` FROM deposit_intents WHERE tenant_id = $1 AND player_account_id = $2 AND idempotency_key = $3`,
		tenantID, playerAccountID, key,
	)
	d, err := scanDepositIntent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return DepositIntent{}, false, nil
	}
	if err != nil {
		return DepositIntent{}, false, fmt.Errorf("payments: load deposit intent by idempotency key: %w", err)
	}
	return d, true, nil
}

// loadDepositIntentByProviderRef resolves a deposit_intents row by
// (provider_id, provider_reference) - the ONLY lookup ReceiveCallback
// uses to resolve tenant/player/wallet, never a payload-carried
// identifier (payment-orchestration.md §10). tx's tenant scope (set by
// the caller before calling ReceiveCallback) is what makes this
// cross-tenant-safe: RLS silently returns zero rows for another tenant's
// matching reference rather than this function ever seeing it.
func loadDepositIntentByProviderRef(ctx context.Context, tx pgx.Tx, providerID, providerReference string) (DepositIntent, bool, error) {
	row := tx.QueryRow(ctx,
		`SELECT `+depositIntentColumns+` FROM deposit_intents WHERE provider_id = $1 AND provider_reference = $2`,
		providerID, providerReference,
	)
	d, err := scanDepositIntent(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return DepositIntent{}, false, nil
	}
	if err != nil {
		return DepositIntent{}, false, fmt.Errorf("payments: load deposit intent by provider reference: %w", err)
	}
	return d, true, nil
}

func setIntentAttempt(ctx context.Context, tx pgx.Tx, intentID uuid.UUID, providerID, providerReference *string, status DepositIntentStatus) error {
	_, err := tx.Exec(ctx,
		`UPDATE deposit_intents SET provider_id = $2, provider_reference = $3, status = $4, updated_at = now() WHERE id = $1`,
		intentID, providerID, providerReference, status,
	)
	if err != nil {
		return fmt.Errorf("payments: update deposit intent attempt: %w", err)
	}
	return nil
}

func validateInitiateDepositParams(p InitiateDepositParams) error {
	if p.Scope.TenantID == uuid.Nil || p.Scope.BrandID == uuid.Nil || p.Scope.PlayerAccountID == uuid.Nil || p.Scope.WalletID == uuid.Nil {
		return fmt.Errorf("payments: InitiateDeposit requires a fully-populated, server-derived DepositScope")
	}
	if p.Amount <= 0 {
		return fmt.Errorf("payments: amount must be positive, got %d", p.Amount)
	}
	if p.AssetCode == "" {
		return fmt.Errorf("payments: asset_code is required")
	}
	if p.PaymentMethod == "" {
		return fmt.Errorf("payments: payment_method is required")
	}
	if p.IdempotencyKey == "" {
		return fmt.Errorf("payments: idempotency_key is required")
	}
	return nil
}

// InitiateDeposit creates (or, on a client-side retry, returns) a
// DepositIntent and drives it through RouteProvider and the chosen
// adapter's synchronous Deposit call, including cascade-on-decline and
// ambiguous-outcome resolution (payment-orchestration.md §5, §7). tx must
// already be tenant-scoped via db.Pool.WithTenant(params.Scope.TenantID,
// ...).
//
// Idempotency (payment-orchestration.md §8): a retried call with the same
// (tenant_id, player_account_id, idempotency_key) returns the ORIGINAL
// intent as-is - never a second call to any provider - via the same
// SAVEPOINT pattern ledger.Post uses (db.IdempotentInsert), against
// deposit_intents' own UNIQUE(tenant_id, player_account_id,
// idempotency_key) constraint (migration 0025).
func (o *Orchestrator) InitiateDeposit(ctx context.Context, tx pgx.Tx, params InitiateDepositParams) (DepositIntent, error) {
	if err := validateInitiateDepositParams(params); err != nil {
		return DepositIntent{}, err
	}

	intentID := uuid.New()
	conflict, err := db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error {
		_, err := spTx.Exec(ctx,
			`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			intentID, params.Scope.TenantID, params.Scope.BrandID, params.Scope.PlayerAccountID, params.Scope.WalletID,
			params.AssetCode, params.Amount, params.PaymentMethod, params.IdempotencyKey,
		)
		return err
	})
	if err != nil {
		return DepositIntent{}, fmt.Errorf("payments: create deposit intent: %w", err)
	}

	if conflict {
		existing, found, err := loadDepositIntentByIdempotencyKey(ctx, tx, params.Scope.TenantID, params.Scope.PlayerAccountID, params.IdempotencyKey)
		if err != nil {
			return DepositIntent{}, err
		}
		if !found {
			return DepositIntent{}, fmt.Errorf("payments: idempotency conflict on deposit intent but no existing row found")
		}
		// A retry must reuse the SAME deposit parameters as the original -
		// never be silently accepted against a different payload (see
		// ErrIdempotencyKeyReused's doc comment; mirrors
		// internal/ledger.Post and internal/withdrawal.RequestWithdrawal's
		// identical check on their own idempotency conflict path).
		if existing.BrandID != params.Scope.BrandID || existing.WalletID != params.Scope.WalletID ||
			existing.AssetCode != params.AssetCode || existing.Amount != params.Amount || existing.PaymentMethod != params.PaymentMethod {
			return DepositIntent{}, fmt.Errorf("%w: existing intent %s", ErrIdempotencyKeyReused, existing.ID)
		}
		return existing, nil
	}

	intent := DepositIntent{
		ID: intentID, TenantID: params.Scope.TenantID, BrandID: params.Scope.BrandID,
		PlayerAccountID: params.Scope.PlayerAccountID, WalletID: params.Scope.WalletID,
		AssetCode: params.AssetCode, Amount: params.Amount, PaymentMethod: params.PaymentMethod,
		IdempotencyKey: params.IdempotencyKey, Status: DepositIntentPending,
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: params.Scope.TenantID, ActorType: audit.ActorPlayer, ActorID: params.Scope.PlayerAccountID,
		Action: "deposit.requested", TargetType: "deposit_intent", TargetID: intentID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{"amount": params.Amount, "asset_code": params.AssetCode, "payment_method": params.PaymentMethod},
	}); err != nil {
		return DepositIntent{}, fmt.Errorf("payments: audit deposit request: %w", err)
	}

	// Stage 9 production-readiness fix (identity-compliance): internal/
	// rg.EvaluateEligibility - the single authoritative "may this player
	// perform a gambling-adjacent financial action right now" boundary
	// (ADR 0026 §5), consulted by internal/casino's LaunchGame/postBet
	// since Stage 4D-RG - was never consulted anywhere on the deposit
	// path. An active platform-wide self-exclusion (or a suspended
	// player account, or a non-active wallet) had NO effect on whether a
	// deposit could be initiated: a self-excluded player could fund a
	// wallet indefinitely even though they could never launch a game or
	// place a bet with the resulting balance. Checked here, inside the
	// SAME transaction that created intent above, and strictly BEFORE
	// RouteProvider/provider.Deposit ever run - a denied player must
	// never reach a real payment provider at all.
	//
	// Deliberately surfaced as a DepositIntentDeclined RESULT via
	// finalizeDeclined, never as a Go error: a non-nil error returned
	// from inside this same db.Pool.WithTenant callback rolls back the
	// whole transaction, which would silently discard the very audit
	// record this check exists to create - the identical class of bug
	// internal/casino.evaluateAndAuditEligibility's own doc comment
	// already records finding and fixing once before on the casino
	// launch/bet path ("an earlier error-based attempt was found, by
	// test, to silently roll back its own audit record").
	eligibility, err := rg.EvaluateEligibility(ctx, tx, rg.EligibilityParams{
		TenantID: intent.TenantID, BrandID: intent.BrandID, PlayerAccountID: intent.PlayerAccountID, WalletID: intent.WalletID,
	})
	if err != nil {
		return DepositIntent{}, fmt.Errorf("payments: evaluate rg eligibility: %w", err)
	}
	if !eligibility.Allowed {
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "payments.deposit_denied_by_rg",
			TargetType: "player_account", TargetID: intent.PlayerAccountID.String(), Outcome: audit.OutcomeDenied,
			Metadata: map[string]any{
				"reason_code": eligibility.Code, "person_id": eligibility.PersonID.String(),
				"deposit_intent_id": intent.ID.String(), "brand_id": intent.BrandID.String(),
			},
		}); err != nil {
			return DepositIntent{}, fmt.Errorf("payments: audit rg denial: %w", err)
		}
		return o.finalizeDeclined(ctx, tx, intent, nil, nil, "rg_ineligible:"+eligibility.Code)
	}

	return o.attemptDeposit(ctx, tx, intent, nil)
}

// attemptDeposit routes and attempts one provider Deposit call for
// intent, recursing (via handleDecline) on a cascadable decline up to
// MaxCascadeDepth, and resolving an ambiguous synchronous outcome via
// QueryStatus before ever cascading (payment-orchestration.md §5).
// excluded is every provider_id already attempted for this intent so far
// - its length IS the current cascade depth, so no separate depth
// parameter is threaded through.
func (o *Orchestrator) attemptDeposit(ctx context.Context, tx pgx.Tx, intent DepositIntent, excluded []string) (DepositIntent, error) {
	provider, capability, err := o.RouteProvider(ctx, tx, RoutingRequest{
		TenantID: intent.TenantID, BrandID: intent.BrandID, AssetCode: intent.AssetCode,
		PaymentMethod: intent.PaymentMethod, Amount: intent.Amount, Operation: OperationDeposit,
		ExcludeProviderIDs: excluded,
	})
	if errors.Is(err, ErrNoRoutableProvider) {
		return o.finalizeDeclined(ctx, tx, intent, nil, nil, "no_routable_provider")
	}
	if err != nil {
		return intent, fmt.Errorf("payments: route provider: %w", err)
	}
	providerID := capability.ProviderID

	result, err := provider.Deposit(ctx, DepositRequest{
		MerchantReference: intent.ID.String(), Amount: intent.Amount, AssetCode: intent.AssetCode, PaymentMethod: intent.PaymentMethod,
	})
	if err != nil {
		// Transport-level failure while initiating: the platform cannot
		// tell whether the provider actually received the request, so
		// this is OutcomeAmbiguous by policy - never a decline (which
		// could silently discard a request the provider did receive) and
		// never cascaded without a reference to query
		// (payment-orchestration.md §5).
		return o.finalizeAmbiguous(ctx, tx, intent, &providerID, nil, "initiation_transport_error")
	}

	switch result.Outcome {
	case OutcomePending:
		if result.ProviderReference == "" {
			return intent, fmt.Errorf("payments: adapter %s returned OutcomePending with no provider reference", providerID)
		}
		ref := result.ProviderReference
		if err := setIntentAttempt(ctx, tx, intent.ID, &providerID, &ref, DepositIntentPending); err != nil {
			return intent, err
		}
		intent.ProviderID, intent.ProviderReference, intent.Status = &providerID, &ref, DepositIntentPending
		intent.RedirectURL, intent.HostedFieldToken = result.RedirectURL, result.HostedFieldToken
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "deposit.initiated",
			TargetType: "deposit_intent", TargetID: intent.ID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{"provider_id": providerID, "provider_reference": ref},
		}); err != nil {
			return intent, fmt.Errorf("payments: audit deposit initiated: %w", err)
		}
		return intent, nil

	case OutcomeDeclined:
		return o.handleDecline(ctx, tx, intent, providerID, result.ProviderReference, result.Cascadable, result.DeclineReason, excluded)

	case OutcomeAmbiguous:
		return o.resolveAmbiguous(ctx, tx, intent, providerID, provider, result.ProviderReference, excluded)

	default:
		return intent, fmt.Errorf("payments: adapter %s returned invalid synchronous Deposit outcome %q", providerID, result.Outcome)
	}
}

// handleDecline records a definite decline and either cascades to the
// next-ranked candidate (if the adapter declared it cascadable and the
// cascade depth budget allows) or finalizes the intent as declined
// (payment-orchestration.md §5).
func (o *Orchestrator) handleDecline(ctx context.Context, tx pgx.Tx, intent DepositIntent, providerID, providerReference string, cascadable bool, reason string, excluded []string) (DepositIntent, error) {
	var refPtr *string
	if providerReference != "" {
		refPtr = &providerReference
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "deposit.attempt_declined",
		TargetType: "deposit_intent", TargetID: intent.ID.String(), Outcome: audit.OutcomeFailure,
		Metadata: map[string]any{"provider_id": providerID, "provider_reference": providerReference, "decline_reason": reason, "cascadable": cascadable},
	}); err != nil {
		return intent, fmt.Errorf("payments: audit attempt decline: %w", err)
	}

	attemptsSoFar := len(excluded) + 1
	if cascadable && attemptsSoFar < o.maxCascadeDepth() {
		nextExcluded := make([]string, 0, len(excluded)+1)
		nextExcluded = append(nextExcluded, excluded...)
		nextExcluded = append(nextExcluded, providerID)
		return o.attemptDeposit(ctx, tx, intent, nextExcluded)
	}
	return o.finalizeDeclined(ctx, tx, intent, &providerID, refPtr, reason)
}

// resolveAmbiguous implements payment-orchestration.md §5's binding rule:
// "the orchestrator must not cascade on an ambiguous/timeout outcome; it
// must call the provider's QueryStatus to resolve the ambiguity first...
// and only cascade once that provider has returned a definite decline."
// If QueryStatus itself cannot resolve it (error, or still
// pending/ambiguous), the intent is left in DepositIntentAmbiguous -
// "left open pending manual/reconciliation resolution", never cascaded
// and never silently marked failed.
func (o *Orchestrator) resolveAmbiguous(ctx context.Context, tx pgx.Tx, intent DepositIntent, providerID string, provider PaymentProvider, providerReference string, excluded []string) (DepositIntent, error) {
	if providerReference == "" {
		return o.finalizeAmbiguous(ctx, tx, intent, &providerID, nil, "no_provider_reference_to_query")
	}
	status, err := provider.QueryStatus(ctx, providerReference)
	if err != nil {
		return o.finalizeAmbiguous(ctx, tx, intent, &providerID, &providerReference, "query_status_failed")
	}

	switch status.Outcome {
	case OutcomeSucceeded:
		return o.postDepositSuccess(ctx, tx, intent, providerID, providerReference, status.Amount, status.AssetCode)
	case OutcomeDeclined:
		return o.handleDecline(ctx, tx, intent, providerID, providerReference, status.Cascadable, status.DeclineReason, excluded)
	default: // OutcomeAmbiguous or OutcomePending - still unresolved
		return o.finalizeAmbiguous(ctx, tx, intent, &providerID, &providerReference, "still_unresolved_after_query_status")
	}
}

func (o *Orchestrator) finalizeDeclined(ctx context.Context, tx pgx.Tx, intent DepositIntent, providerID, providerReference *string, reason string) (DepositIntent, error) {
	if err := setIntentAttempt(ctx, tx, intent.ID, providerID, providerReference, DepositIntentDeclined); err != nil {
		return intent, err
	}
	intent.ProviderID, intent.ProviderReference, intent.Status = providerID, providerReference, DepositIntentDeclined
	meta := map[string]any{"decline_reason": reason}
	if providerID != nil {
		meta["provider_id"] = *providerID
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "deposit.declined",
		TargetType: "deposit_intent", TargetID: intent.ID.String(), Outcome: audit.OutcomeFailure, Metadata: meta,
	}); err != nil {
		return intent, fmt.Errorf("payments: audit deposit declined: %w", err)
	}
	return intent, nil
}

func (o *Orchestrator) finalizeAmbiguous(ctx context.Context, tx pgx.Tx, intent DepositIntent, providerID, providerReference *string, reason string) (DepositIntent, error) {
	if err := setIntentAttempt(ctx, tx, intent.ID, providerID, providerReference, DepositIntentAmbiguous); err != nil {
		return intent, err
	}
	intent.ProviderID, intent.ProviderReference, intent.Status = providerID, providerReference, DepositIntentAmbiguous
	meta := map[string]any{"reason": reason}
	if providerID != nil {
		meta["provider_id"] = *providerID
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "deposit.ambiguous",
		TargetType: "deposit_intent", TargetID: intent.ID.String(), Outcome: audit.OutcomeFailure, Metadata: meta,
	}); err != nil {
		return intent, fmt.Errorf("payments: audit deposit ambiguous: %w", err)
	}
	return intent, nil
}

// postDepositSuccess posts financial-transaction-flows.md Flow 1: debit
// psp_clearing, credit player_cash, idempotent on the provider's own
// reference (used as both the ledger's provider_tx_id and its
// idempotency_key). A redelivered success callback for an
// already-succeeded intent is a no-op here - the short-circuit below,
// combined with ledger.Post's own (tenant_id, provider_id, provider_tx_id)
// uniqueness as a second, independent backstop
// (payment-orchestration.md §8).
func (o *Orchestrator) postDepositSuccess(ctx context.Context, tx pgx.Tx, intent DepositIntent, providerID, providerReference string, amount int64, assetCode string) (DepositIntent, error) {
	if intent.Status == DepositIntentSucceeded {
		return intent, nil
	}
	if amount != intent.Amount || assetCode != intent.AssetCode {
		return intent, fmt.Errorf("%w: intent %s expected %d %s, provider confirmed %d %s",
			ErrCallbackProviderMismatch, intent.ID, intent.Amount, intent.AssetCode, amount, assetCode)
	}

	// ADR 0082 §4.5: this package needs NO locking change - finding
	// LOCK-1b (a deposit and a reversal of a DIFFERENT deposit on the same
	// wallet taking (psp_clearing, player_cash) in opposite orders) is
	// closed entirely by ledger.Post's own internal L3 pre-lock, because
	// neither path takes any lock outside Post. The switch to
	// GetOrCreateAccounts is the uniform, defensive half: canonical
	// (wallet, account_type, asset) creation order for the
	// ledger_accounts unique-index insertion waits.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, intent.TenantID,
		ledger.AccountSpec{WalletID: &intent.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: intent.AssetCode},
		ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: intent.AssetCode},
	)
	if err != nil {
		return intent, fmt.Errorf("payments: resolve deposit ledger accounts: %w", err)
	}
	cashAccountID, clearingAccountID := accounts[0], accounts[1]

	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		// IdempotencyKey is namespaced by providerID, not the bare
		// providerReference: nothing in the PaymentProvider contract
		// requires reference strings to be unique ACROSS providers
		// (types.go's own doc comment defines it only as "the provider's
		// own reference for this attempt"), so with a second real PSP
		// configured, provider B's reference could collide with one
		// provider A already used - which would silently attribute B's
		// deposit to A's ledger transaction via ledger.Post's idempotency-
		// conflict path instead of posting it. The (tenant_id, provider_id,
		// provider_tx_id) unique index below already scopes correctly per
		// provider; this makes the OTHER idempotency key agree with it.
		TenantID: intent.TenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: providerID + ":" + providerReference,
		ProviderID: &providerID, ProviderTxID: &providerReference, CorrelationID: intent.ID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: clearingAccountID, Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: cashAccountID, Direction: ledger.Credit, Amount: amount},
		},
	})
	if errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) {
		// Unreachable through the intent compare above today (audit
		// site #19 is class B), kept as the typed backstop.
		return intent, fmt.Errorf("%w: post deposit: %w", ErrCallbackPayloadMismatch, err)
	}
	if err != nil {
		return intent, fmt.Errorf("payments: post deposit: %w", err)
	}

	if _, err := tx.Exec(ctx,
		`UPDATE deposit_intents SET provider_id = $2, provider_reference = $3, status = $4, ledger_transaction_id = $5, updated_at = now() WHERE id = $1`,
		intent.ID, providerID, providerReference, DepositIntentSucceeded, postResult.TransactionID,
	); err != nil {
		return intent, fmt.Errorf("payments: mark deposit intent succeeded: %w", err)
	}
	intent.ProviderID, intent.ProviderReference = &providerID, &providerReference
	intent.Status = DepositIntentSucceeded
	intent.LedgerTransactionID = &postResult.TransactionID

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: intent.TenantID, ActorType: audit.ActorSystem, Action: "deposit.posted",
		TargetType: "deposit_intent", TargetID: intent.ID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"provider_id": providerID, "provider_reference": providerReference, "amount": amount, "asset_code": assetCode,
			"ledger_transaction_id": postResult.TransactionID.String(), "already_posted": postResult.AlreadyPosted,
		},
	}); err != nil {
		return intent, fmt.Errorf("payments: audit deposit posted: %w", err)
	}
	return intent, nil
}

// ReceiveCallbackResult is what ReceiveCallback returns for a caller
// (e.g. an HTTP handler) that needs to know the outcome, without exposing
// the full DepositIntent/ledger internals.
type ReceiveCallbackResult struct {
	// DepositIntentID is uuid.Nil only for a tombstoned reversal (the
	// original deposit was never seen - Tombstoned is true in that case).
	DepositIntentID     uuid.UUID
	Status              DepositIntentStatus
	LedgerTransactionID *uuid.UUID
	Tombstoned          bool
}

// ReceiveCallback dispatches a verified provider callback: parses it via
// the named adapter's HandleCallback, resolves the deposit_intents row it
// refers to, and posts Flow 1 (deposit) or Flow 2 (deposit reversal) via
// ledger.Post.
//
// Signature note - a deliberate, documented deviation from
// payment-orchestration.md §3's abstract pseudocode
// "ReceiveCallback(ctx, provider_id, rawPayload) error", which omits both
// tx and tenantID:
//
//   - tx: every other DB-touching function in this codebase
//     (ledger.Post, wallet.GetOrCreate, audit.Record) takes an
//     already-scoped pgx.Tx rather than owning its own transaction -
//     this function follows that established convention, and the task's
//     own InitiateDeposit/RouteProvider signatures already do the same.
//   - tenantID: docs/decisions/0022 §3 records as an OPEN DECISION *how*
//     a webhook selects the right tenant/verification key before any
//     tenant is known from the payload alone, and names a per-tenant
//     webhook endpoint/path as one candidate resolution. deposit_intents'
//     RLS (migration 0025) has no platform-wide/dual-scope read policy -
//     only tenant_staff_scope, which requires app.tenant_id to already be
//     set - so looking up a row by (provider_id, provider_reference) is
//     structurally impossible without tenantID already being known. This
//     function therefore accepts tenantID as a parameter, sourced by the
//     caller (an HTTP handler, resolving it from a per-tenant webhook
//     path/credential) BEFORE opening tx via db.Pool.WithTenant and
//     BEFORE calling this function - never from rawPayload. This is the
//     most conservative choice consistent with both that OPEN DECISION's
//     own first candidate and the existing RLS shape, and it still
//     satisfies payment-orchestration.md §10's binding rule that "the
//     tenant comes from the key that verified the signature, never from
//     the body." The narrower question §3 leaves open - exactly how a
//     real adapter picks its per-tenant verification key when one
//     provider_id serves many tenants - remains unresolved by this
//     function and is out of scope for the mock, which has no per-tenant
//     credential concept at all.
func (o *Orchestrator) ReceiveCallback(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, rawPayload []byte) (ReceiveCallbackResult, error) {
	provider, ok := o.providers[providerID]
	if !ok {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: %s", ErrUnknownProvider, providerID)
	}

	event, err := provider.HandleCallback(ctx, rawPayload)
	if err != nil {
		// Never wrap rawPayload's bytes into this error - ErrInboundKeyMaterial
		// and any parse error must carry no payload content
		// (docs/decisions/0022 §4.1).
		return ReceiveCallbackResult{}, fmt.Errorf("payments: handle callback: %w", err)
	}

	switch event.EventType {
	case CallbackEventDeposit:
		return o.receiveDepositCallback(ctx, tx, tenantID, providerID, event)
	case CallbackEventDepositReversal:
		return o.receiveDepositReversalCallback(ctx, tx, tenantID, providerID, event)
	default:
		return ReceiveCallbackResult{}, fmt.Errorf("payments: unsupported callback event type %q", event.EventType)
	}
}

// providerExclusionSoFar approximates the cascade-exclusion set for a
// callback-triggered decline/ambiguity. Because deposit_intents (migration
// 0025) stores only the LATEST attempted (provider_id, provider_reference)
// pair, not a full attempt history, this can only exclude the single
// most-recently-tried provider, not every provider ever tried across this
// intent's lifetime. Documented limitation, not a silent gap: a richer
// per-attempt history table is the natural follow-up if cascade chains
// need to survive across callback boundaries with full history, but nothing
// in Stage 3B's scope requires it (a single synchronous InitiateDeposit
// call already cascades with full history via attemptDeposit's own
// `excluded` slice; only a callback arriving asynchronously after
// InitiateDeposit already returned loses that in-memory history).
func providerExclusionSoFar(intent DepositIntent) []string {
	if intent.ProviderID != nil {
		return []string{*intent.ProviderID}
	}
	return nil
}

func (o *Orchestrator) receiveDepositCallback(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent) (ReceiveCallbackResult, error) {
	intent, found, err := loadDepositIntentByProviderRef(ctx, tx, providerID, event.ProviderReference)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}
	if !found {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: provider=%s reference=%s", ErrDepositIntentNotFound, providerID, event.ProviderReference)
	}
	if intent.TenantID != tenantID {
		// Defense-in-depth only: RLS under tx's tenant scope already
		// guarantees the SELECT above could not have returned another
		// tenant's row - this branch should be unreachable.
		return ReceiveCallbackResult{}, ErrDepositIntentNotFound
	}

	// A callback for an intent already in a TERMINAL state (succeeded/
	// declined/failed) is a late, out-of-order, or replayed delivery -
	// e.g. a decline arriving after an earlier cascade attempt's success
	// callback already posted, or any callback redelivered after the
	// intent's own routing was exhausted. postDepositSuccess has its own
	// redelivered-success short-circuit (line ~652) for the one case that
	// is a legitimate, expected redelivery; every other combination here
	// must be a safe no-op, never a second mutation of an intent that
	// already has a final, ledger-backed outcome - handleDecline in
	// particular can re-enter the cascade (a fresh provider.Deposit call
	// at a DIFFERENT PSP) if allowed to run against an intent that has
	// already succeeded.
	if event.Outcome != OutcomeSucceeded &&
		(intent.Status == DepositIntentSucceeded || intent.Status == DepositIntentDeclined || intent.Status == DepositIntentFailed) {
		return ReceiveCallbackResult{DepositIntentID: intent.ID, Status: intent.Status, LedgerTransactionID: intent.LedgerTransactionID}, nil
	}

	switch event.Outcome {
	case OutcomeSucceeded:
		intent, err = o.postDepositSuccess(ctx, tx, intent, providerID, event.ProviderReference, event.Amount, event.AssetCode)
	case OutcomeDeclined:
		intent, err = o.handleDecline(ctx, tx, intent, providerID, event.ProviderReference, event.Cascadable, event.DeclineReason, providerExclusionSoFar(intent))
	case OutcomeAmbiguous:
		intent, err = o.resolveAmbiguous(ctx, tx, intent, providerID, o.providers[providerID], event.ProviderReference, providerExclusionSoFar(intent))
	case OutcomePending:
		// Informational only - no state transition required.
	default:
		return ReceiveCallbackResult{}, fmt.Errorf("payments: callback has invalid outcome %q", event.Outcome)
	}
	if err != nil {
		return ReceiveCallbackResult{}, err
	}

	return ReceiveCallbackResult{DepositIntentID: intent.ID, Status: intent.Status, LedgerTransactionID: intent.LedgerTransactionID}, nil
}

// receiveDepositReversalCallback implements financial-transaction-flows.md
// Flow 2, including its tombstone case: if the original deposit was never
// posted to the ledger (no matching deposit_intents row, or a matching row
// with no ledger_transaction_id yet), a TxTombstone is written instead so
// a late-arriving original deposit callback for that reference is
// rejected by the ledger's own (tenant_id, provider_id, provider_tx_id)
// uniqueness rather than posted after the fact
// (ledger-accounting-model.md §1.4, CLAUDE.md's rollback rule).
func (o *Orchestrator) receiveDepositReversalCallback(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent) (ReceiveCallbackResult, error) {
	original, found, err := loadDepositIntentByProviderRef(ctx, tx, providerID, event.OriginalProviderReference)
	if err != nil {
		return ReceiveCallbackResult{}, err
	}

	if !found || original.LedgerTransactionID == nil {
		txID, err := postDepositReversalTombstone(ctx, tx, tenantID, providerID, event)
		if err != nil {
			return ReceiveCallbackResult{}, err
		}
		if err := audit.Record(ctx, tx, audit.Entry{
			TenantID: tenantID, ActorType: audit.ActorSystem, Action: "deposit.reversal_tombstoned",
			TargetType: "ledger_transaction", TargetID: txID.String(), Outcome: audit.OutcomeSuccess,
			Metadata: map[string]any{
				"provider_id": providerID, "original_provider_reference": event.OriginalProviderReference,
				"reversal_provider_reference": event.ProviderReference,
			},
		}); err != nil {
			return ReceiveCallbackResult{}, fmt.Errorf("payments: audit reversal tombstone: %w", err)
		}
		return ReceiveCallbackResult{LedgerTransactionID: &txID, Tombstoned: true}, nil
	}

	// A reversal callback's amount/asset are payload-controlled facts
	// about a debit the platform is about to post - CLAUDE.md's
	// authorization rule ("never trusted from the client") applies here
	// exactly as it does to a tenant_id: the callback is verified as
	// AUTHENTIC (HandleCallback's signature check), which is not the same
	// as verified as CORRECT. Stage 3B implements only whole-deposit
	// reversal, never a partial refund, so the only value that can ever
	// be legitimate is the original's own amount/asset - anything else is
	// rejected rather than silently capped or coerced.
	if event.Amount > 0 && event.Amount != original.Amount {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: reversal amount %d does not match original deposit amount %d",
			ErrCallbackProviderMismatch, event.Amount, original.Amount)
	}
	if event.AssetCode != "" && event.AssetCode != original.AssetCode {
		return ReceiveCallbackResult{}, fmt.Errorf("%w: reversal asset %q does not match original deposit asset %q",
			ErrCallbackProviderMismatch, event.AssetCode, original.AssetCode)
	}
	amount := original.Amount

	// Reject a second reversal of the same original deposit under a NEW
	// provider_reference of its own - see ErrDepositAlreadyReversed's doc
	// comment. A REDELIVERY of the same reversal (same provider_reference)
	// is excluded here and falls through to ledger.Post, whose idempotency
	// check returns the original reversal (AlreadyPosted) - this check is
	// only for a distinct reference naming an already-reversed original.
	// Stage 10 F-7 remediation: the exclusion of this reversal's OWN
	// reference is new. Before it, a sequential same-reference redelivery
	// was rejected here with ErrDepositAlreadyReversed, contradicting this
	// very comment (audit §6.2 observation); IS DISTINCT FROM so a
	// reversal row with a NULL provider reference is still counted.
	var alreadyReversed bool
	if err := tx.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM ledger_transactions WHERE reverses_transaction_id = $1
		                  AND (provider_id IS DISTINCT FROM $2 OR provider_tx_id IS DISTINCT FROM $3))`,
		original.LedgerTransactionID, providerID, event.ProviderReference,
	).Scan(&alreadyReversed); err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("payments: check existing reversal: %w", err)
	}
	if alreadyReversed {
		return ReceiveCallbackResult{}, ErrDepositAlreadyReversed
	}

	// ADR 0082 §4.5: account resolution only - see the identical comment
	// on the deposit-success posting above for why the reversal path
	// (the other half of finding LOCK-1b) needs no locking change.
	accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
		ledger.AccountSpec{WalletID: &original.WalletID, AccountType: ledger.AccountPlayerCash, AssetCode: original.AssetCode},
		ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: original.AssetCode},
	)
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("payments: resolve deposit reversal ledger accounts: %w", err)
	}
	cashAccountID, clearingAccountID := accounts[0], accounts[1]

	reversalRef := event.ProviderReference
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		// See the identical provider-namespacing comment on the deposit
		// success posting above.
		TenantID: tenantID, TransactionType: ledger.TxDepositReversal, IdempotencyKey: providerID + ":" + reversalRef,
		ProviderID: &providerID, ProviderTxID: &reversalRef, CorrelationID: original.ID,
		ReversesTransactionID: original.LedgerTransactionID,
		Entries: []ledger.EntryInput{
			{LedgerAccountID: cashAccountID, Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: clearingAccountID, Direction: ledger.Credit, Amount: amount},
		},
	})
	if errors.Is(err, ledger.ErrIdempotencyPayloadMismatch) {
		// Audit site #20: this reversal reference is already posted with a
		// different payload (typically: it reversed a DIFFERENT deposit).
		// Before F-7 this returned the other reversal as success and
		// reported THIS deposit reversed; now nothing is posted and the
		// caller gets an integrity failure.
		return ReceiveCallbackResult{}, fmt.Errorf("%w: post deposit reversal: %w", ErrCallbackPayloadMismatch, err)
	}
	if err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("payments: post deposit reversal: %w", err)
	}

	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: tenantID, ActorType: audit.ActorSystem, Action: "deposit.reversed",
		TargetType: "deposit_intent", TargetID: original.ID.String(), Outcome: audit.OutcomeSuccess,
		Metadata: map[string]any{
			"provider_id": providerID, "reversal_provider_reference": reversalRef,
			"original_ledger_transaction_id": original.LedgerTransactionID.String(),
			"reversal_ledger_transaction_id": postResult.TransactionID.String(), "amount": amount,
		},
	}); err != nil {
		return ReceiveCallbackResult{}, fmt.Errorf("payments: audit deposit reversal: %w", err)
	}

	return ReceiveCallbackResult{DepositIntentID: original.ID, Status: original.Status, LedgerTransactionID: &postResult.TransactionID}, nil
}

func postDepositReversalTombstone(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, providerID string, event CallbackEvent) (uuid.UUID, error) {
	originalRef := event.OriginalProviderReference
	// ReasonCode is deliberately left nil: ledger_transactions' own CHECK
	// constraint (migration 0021) requires reason_code IS NOT NULL if and
	// only if transaction_type = 'manual_adjustment', and forbids it for
	// every other type including 'tombstone'. The human-readable reason
	// ("deposit_reversal_for_unseen_original") is carried in this
	// function's caller's audit record instead, not on the ledger row.
	result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenantID, TransactionType: ledger.TxTombstone,
		IdempotencyKey: fmt.Sprintf("tombstone:%s:%s", providerID, originalRef),
		ProviderID:     &providerID, ProviderTxID: &originalRef,
		CorrelationID: uuid.New(),
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("payments: post reversal tombstone: %w", err)
	}
	return result.TransactionID, nil
}
