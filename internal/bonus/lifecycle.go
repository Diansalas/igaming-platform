// Grant lifecycle state-machine enforcement (docs/architecture/10-bonus-
// engine-architecture.md "doc 10" §1.2/§1.3, T.1-T.13, N1.4) - explicit
// valid-predecessor-state checking (illegal transitions are rejected via
// the underlying compare-and-swap primitives in grant.go, never silently
// overwritten), actor/context recording, idempotency, audit (every
// mutating write appends a bonus_grant_progress row per §10.1 AND writes
// an audit.Record per CLAUDE.md), tenant/brand/asset scope enforcement.
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/risk"
)

// AdvisoryLockGrant acquires the (tenant_id, grant_id) advisory lock doc
// 10 §9/N1.5 specify for every transaction that reads or writes anything
// about a Grant - "every transaction that reads or writes anything about
// G" (N1.5) - mirroring internal/casino's own
// hashtextextended(...) pg_advisory_xact_lock pattern
// (orchestrator.go:617). Held for the remainder of the caller's
// transaction (Postgres releases it at COMMIT/ROLLBACK automatically -
// there is no unlock call).
func AdvisoryLockGrant(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID) error {
	_, err := tx.Exec(ctx,
		`SELECT pg_advisory_xact_lock(hashtextextended('bonus_grant:' || $1::text || ':' || $2::text, 0))`,
		tenantID, grantID,
	)
	if err != nil {
		return fmt.Errorf("bonus: acquire grant advisory lock: %w", err)
	}
	return nil
}

// ErrGrantAlreadyExists is returned by IssueGrant when the underlying
// UNIQUE (tenant_id, campaign_id, offer_version_id, player_account_id,
// trigger_reference) constraint (migration 0057) already has a row - the
// ordinary, expected redelivery case (doc 10 §9), never a program error.
// Raised as a distinct sentinel (never a bare db.IsUniqueViolation check
// downstream) because the insert itself runs inside its own savepoint
// (below), so the conflict never poisons the caller's own transaction -
// callers (types.go's issueIdempotent) look the existing Grant up and
// continue in the SAME transaction.
var ErrGrantAlreadyExists = errors.New("bonus: a grant already exists for this (tenant, campaign, offer_version, player, trigger_reference)")

// ErrIllegalTransition is returned when a caller asks for a Grant
// transition doc 10 §1.3's table does not permit from the Grant's actual
// current status - never silently coerced into the closest legal
// transition.
var ErrIllegalTransition = errors.New("bonus: illegal grant state transition")

func denialReasonCode(o GateOutcome) string {
	return fmt.Sprintf("%s_denied:%s", o.DeniedBy, o.Code)
}

// IssueGrantParams is everything (none)->issued needs (doc 10 §1.3/T.2).
type IssueGrantParams struct {
	Grant            Grant
	JurisdictionCode string
}

// IssueGrant performs doc 10's "(none) -> issued" transition: RG then
// Risk (T.2: AssetAuthorization is explicitly NOT checked at creation -
// "issued is a decision, not a movement"), Operation =
// OperationBonusGrant. On denial, per §1.3's own transition table
// ("Event-bus consumption... after RG and Risk both allow"), NO Grant row
// is created at all - there is nothing to cancel, since issuance never
// happened. Every EOI/idempotency/eligibility-axis-snapshot precondition
// is the CALLER's responsibility (targeting.go) - this function performs
// only the RG/Risk gate and the row insert.
func IssueGrant(ctx context.Context, tx pgx.Tx, p IssueGrantParams) (Grant, GateOutcome, error) {
	g := p.Grant
	jurisdictionID, err := resolveJurisdictionID(ctx, tx, p.JurisdictionCode)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	licensingMode, err := resolveLicensingMode(ctx, tx, g.TenantID)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}

	outcome, err := GateCheckpoint(ctx, tx, GateParams{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, WalletID: g.WalletID,
		JurisdictionID: jurisdictionID, JurisdictionCode: p.JurisdictionCode, LicensingMode: licensingMode,
		AssetCode: g.AssetCode, RiskOperation: risk.OperationBonusGrant, SkipAssetAuthorization: true,
	})
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if !outcome.Allowed {
		return Grant{}, outcome, nil
	}

	// CreateGrant's own insert relies entirely on the schema's unique
	// constraint for idempotency (its own doc comment) - a violation is
	// the EXPECTED, ordinary redelivery case (doc 10 §9), never a
	// program error, so it MUST run inside its own savepoint
	// (db.IdempotentInsert's pattern, mirroring ledger.Post's identical
	// need): a raw INSERT conflict on the caller's own tx would otherwise
	// poison the entire transaction (Postgres aborts every subsequent
	// statement until ROLLBACK), which would break every caller that
	// wants to look up the existing Grant and continue in the SAME
	// transaction (types.go's issueIdempotent).
	var created Grant
	conflict, err := db.IdempotentInsert(ctx, tx, func(spTx pgx.Tx) error {
		var insertErr error
		created, insertErr = CreateGrant(ctx, spTx, g)
		return insertErr
	})
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if conflict {
		return Grant{}, GateOutcome{}, ErrGrantAlreadyExists
	}
	if err := AdvisoryLockGrant(ctx, tx, created.TenantID, created.ID); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	issuedStatus := string(GrantIssued)
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: created.TenantID, BrandID: created.BrandID, PlayerAccountID: created.PlayerAccountID, GrantID: created.ID,
		TransitionType: TransitionIssued, TriggerType: issuanceTriggerType(created.CreatedByActorType),
		TriggerReference: strPtr(created.TriggerReference), AfterStatus: &issuedStatus,
		ActorType: created.CreatedByActorType, ActorID: nonNilActorID(created.CreatedByActorType, created.CreatedByActorID),
	}); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: created.TenantID, ActorType: audit.ActorType(created.CreatedByActorType), ActorID: created.CreatedByActorID,
		Action: "bonus_grant.issued", TargetType: "bonus_grant", TargetID: created.ID.String(), Outcome: audit.OutcomeSuccess,
	}); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	return created, GateOutcome{Allowed: true}, nil
}

func issuanceTriggerType(actor ActorType) TriggerType {
	switch actor {
	case ActorStaff:
		return TriggerStaffAction
	case ActorPlayer:
		return TriggerPlayerAction
	case ActorService:
		return TriggerProviderCallback
	default:
		return TriggerAutomatedRuleEvaluation
	}
}

func strPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNilActorID(actorType ActorType, id uuid.UUID) *uuid.UUID {
	if actorType == ActorSystem {
		return nil
	}
	return &id
}

// ActivateGrantParams is issued->activated's own input (doc 10 T.3).
type ActivateGrantParams struct {
	JurisdictionCode string
	ActorType        ActorType
	ActorID          uuid.UUID
	// Amount is the reward's face value in the Grant's own asset minor
	// units, already computed (and, for a cashback/other computed reward,
	// rounded per money.RoundToMinorUnits) by the caller (the bonus-type-
	// specific business logic, types_*.go) - this function posts exactly
	// this amount, never recomputes it.
	Amount *big.Int
	// WageringTimeLimit, if non-nil and positive, is the issuing Offer
	// version's own configured wagering-completion window
	// (bonus_offer_versions.wagering_time_limit) - Stage 4H-B1 Wave 3's
	// binding decision (docs/governance/wave-3-reconnaissance.md gap-list
	// item 7; migration 0070) for WHEN bonus_grants.expires_at gets
	// populated: at activation, as g.CreatedAt + *WageringTimeLimit,
	// written exactly once via SetGrantExpiryOnce, immediately after
	// granted_amount below. Computed from g.CreatedAt (not time.Now()) so
	// the DB's own `expires_at > created_at` CHECK can never fail under
	// Go/Postgres clock skew. nil (the default for every existing caller -
	// no behavior change) means "this Offer configures no wagering time
	// limit, this Grant never expires", matching migration 0070's own
	// nullable-by-design column.
	//
	// NAMED, DELIBERATE SCOPE BOUNDARY: an OfferVersion's separate
	// PayoutTimeLimit (time to convert/withdraw AFTER completion) is a
	// materially different mechanic - it would need to gate a Grant that
	// is already `completed`, not one still open for new stakes, and
	// bonus_grants.expires_at (migration 0070) is a single write-once
	// column that cannot hold both deadlines. PayoutTimeLimit enforcement
	// is intentionally NOT built by this Phase - named here as a
	// follow-up item, not silently conflated with WageringTimeLimit.
	WageringTimeLimit *time.Duration
	// PostGateHook, if non-nil, runs EXACTLY ONCE, strictly AFTER the T.1
	// gate chain below has allowed this activation and strictly BEFORE
	// the effecting ledger write that follows it - never earlier, never
	// later - all still inside this SAME transaction/advisory lock (doc
	// 34 §5.3 rule 3 / doc 10 N2.4a: "the locking EOI consume happens
	// exactly once, strictly AFTER the gate chain completes, immediately
	// before the effecting write"). This is the seam
	// internal/economicop.ConsumeRootBudget's locking `FOR UPDATE` on the
	// EOI root row is threaded through for the two EOI-gated callers
	// (targeting.go's IssueSingleManualGrant / runBulkGrantJobItem) -
	// DR-4HB1W2-01 (Stage 4H-B1 Wave 2 Phase 10, architect composition
	// finding): the prior code called ConsumeRootBudget BEFORE
	// ActivateGrant even ran, so its locking EOI-root row lock could be
	// acquired before Risk's own pg_advisory_xact_lock (taken inside this
	// function's own GateCheckpoint call below, if a cumulative rule is
	// ever scoped to bonus_grant) - the reverse of doc 34 §5.3 rule 4's
	// canonical "Risk's lock always before the EOI lock" ordering, and a
	// live AB-BA deadlock risk the moment such a rule exists. Threading
	// the consume through THIS hook, instead of leaving it at the call
	// site wrapping the whole function, is what makes "after the gate
	// chain, before the write" achievable at all without duplicating T.1
	// gate logic outside this function.
	//
	// ActivateGrant itself stays completely EOI-agnostic: every non-EOI-
	// gated bonus type (deposit/reload, cashback, generic wagering,
	// coupon redemption) leaves this nil, which is a pure no-op -
	// byte-for-byte the same activation behavior as before this hook
	// existed. Only the two EOI-gated call sites ever set it, each
	// supplying its own economicop.ConsumeRootBudget closure bound to its
	// own root_operation_id/OperationType/subject/amount - the actual
	// EOI-specific decision-making stays in targeting.go, never leaks
	// into this shared function.
	//
	// If the hook returns an error (including economicop.ErrBudgetExhausted),
	// ActivateGrant returns that error immediately, WITHOUT transitioning
	// the Grant to activated (or cancelled) and WITHOUT posting anything
	// - the Grant is left exactly as LockGrantForUpdate found it (status
	// issued), identical in effect to the prior ordering where a budget-
	// exhausted ConsumeRootBudget was discovered before ActivateGrant
	// even started: either way, an over-budget attempt's transaction
	// never commits a completed activation.
	PostGateHook func(ctx context.Context, tx pgx.Tx) error
}

// ActivateGrant performs doc 10's "issued -> activated" transition: the
// full T.1 three-way gate (AssetAuthorization -> RG -> Risk), and, on
// allow, the SOLE bonus_grant posting (ADR 0032 §3.1: "Dr promo_liability
// · Cr player_bonus", transaction_type bonus_grant), attributed to the
// Grant, in the SAME transaction as the status transition. On denial, the
// Grant is recorded as cancelled (doc 10 §5/T.3), never silently left
// issued.
func ActivateGrant(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, p ActivateGrantParams) (Grant, GateOutcome, error) {
	if err := AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if g.Status != GrantIssued {
		return Grant{}, GateOutcome{}, fmt.Errorf("%w: activate requires status issued, got %s", ErrIllegalTransition, g.Status)
	}

	jurisdictionID, err := resolveJurisdictionID(ctx, tx, p.JurisdictionCode)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	licensingMode, err := resolveLicensingMode(ctx, tx, g.TenantID)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}

	amountMinor, err := amountToInt64(p.Amount)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}

	outcome, err := GateCheckpoint(ctx, tx, GateParams{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, WalletID: g.WalletID,
		JurisdictionID: jurisdictionID, JurisdictionCode: p.JurisdictionCode, LicensingMode: licensingMode,
		AssetCode: g.AssetCode, Amount: amountMinor, RiskOperation: risk.OperationBonusGrant,
	})
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	now := time.Now().UTC()
	if !outcome.Allowed {
		reason := denialReasonCode(outcome)
		cancelled, err := UpdateGrantStatus(ctx, tx, tenantID, grantID, GrantIssued, GrantCancelled, now)
		if err != nil {
			return Grant{}, GateOutcome{}, err
		}
		before, after := string(GrantIssued), string(GrantCancelled)
		if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
			TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: g.ID,
			TransitionType: TransitionCancelled, TriggerType: TriggerAutomatedRuleEvaluation,
			BeforeStatus: &before, AfterStatus: &after, ActorType: ActorSystem, ReasonCode: &reason,
			RiskDecisionCode: gateDecisionCode(outcome, "risk"), RGDecisionCode: gateDecisionCode(outcome, "rg"),
			AssetAuthorizationReasonCode: gateDecisionCode(outcome, "asset_authorization"),
		}); err != nil {
			return Grant{}, GateOutcome{}, err
		}
		return cancelled, outcome, nil
	}

	// doc 34 §5.3 rule 3/4, doc 10 N2.4a: strictly after the gate chain
	// above (including Risk's advisory lock, if taken), strictly before
	// the effecting ledger write below. See PostGateHook's own doc
	// comment (DR-4HB1W2-01).
	if p.PostGateHook != nil {
		if err := p.PostGateHook(ctx, tx); err != nil {
			return Grant{}, GateOutcome{}, err
		}
	}

	fundingKind, providerID, err := parseFundingSource(g.FundingSource)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	walletID := g.WalletID
	playerBonusAccount, err := ledger.GetOrCreateAccount(ctx, tx, g.TenantID, &walletID, ledger.AccountPlayerBonus, g.AssetCode)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	idempotencyKey := fmt.Sprintf("bonus_grant:%s", g.ID)
	postResult, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: g.TenantID, TransactionType: ledger.TxBonusGrant, IdempotencyKey: idempotencyKey,
		CorrelationID: g.ID,
		Entries:       []ledger.EntryInput{{LedgerAccountID: playerBonusAccount, Direction: ledger.Credit, Amount: amountMinor}},
		BonusCost:     &ledger.BonusCostAttribution{Funding: fundingKind, ProviderID: providerID},
	})
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if err := AttributeGrantLedgerTransactionIdempotent(ctx, tx, g.TenantID, g.ID, postResult.TransactionID, string(ledger.TxBonusGrant)); err != nil {
		return Grant{}, GateOutcome{}, err
	}

	activated, err := UpdateGrantStatus(ctx, tx, tenantID, grantID, GrantIssued, GrantActivated, now)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	// LF-Phase-11 (migration 0067): record the Grant's granted_amount -
	// an immutable computation input (doc 29 BI-3), never a balance, never
	// re-derived - exactly once, in this same transaction, immediately
	// after the sole bonus_grant posting above. This is the ONLY point in
	// the Grant lifecycle that ever writes this column (migration 0067's
	// trigger extension rejects any later change), which is why the
	// `granted_amount IS NULL` guard below is a belt-and-suspenders
	// no-op-on-replay rather than a real branch: UpdateGrantStatus's own
	// CAS above already guarantees this statement runs at most once per
	// Grant (a redelivered activation attempt fails the CAS first). Closes
	// DR-4HB1W2-02's disclosed gap: internal/economicop.ConsumeRootBudget's
	// value-budget query sums exactly this column for
	// OperationBonusManualGrant/OperationAPIInitiatedGrant, which had no
	// column to read before this migration.
	if _, err := tx.Exec(ctx,
		`UPDATE bonus_grants SET granted_amount = $3 WHERE tenant_id = $1 AND id = $2 AND granted_amount IS NULL`,
		tenantID, grantID, amountMinor,
	); err != nil {
		return Grant{}, GateOutcome{}, fmt.Errorf("bonus: record granted_amount: %w", err)
	}
	if p.WageringTimeLimit != nil && *p.WageringTimeLimit > 0 {
		if _, err := SetGrantExpiryOnce(ctx, tx, tenantID, grantID, g.CreatedAt.Add(*p.WageringTimeLimit)); err != nil {
			return Grant{}, GateOutcome{}, err
		}
	}
	before, after := string(GrantIssued), string(GrantActivated)
	ledgerTxID := postResult.TransactionID
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: g.ID,
		TransitionType: TransitionActivated, TriggerType: activationTriggerType(p.ActorType),
		BeforeStatus: &before, AfterStatus: &after, ActorType: p.ActorType, ActorID: nonNilActorID(p.ActorType, p.ActorID),
		Amount: p.Amount, AssetCode: &g.AssetCode, LedgerTransactionID: &ledgerTxID,
	}); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: g.TenantID, ActorType: audit.ActorType(p.ActorType), ActorID: p.ActorID,
		Action: "bonus_grant.activated", TargetType: "bonus_grant", TargetID: g.ID.String(), Outcome: audit.OutcomeSuccess,
	}); err != nil {
		return Grant{}, GateOutcome{}, err
	}
	return activated, GateOutcome{Allowed: true}, nil
}

func activationTriggerType(actor ActorType) TriggerType {
	switch actor {
	case ActorStaff:
		return TriggerStaffAction
	case ActorPlayer:
		return TriggerPlayerAction
	default:
		return TriggerAutomatedRuleEvaluation
	}
}

func gateDecisionCode(o GateOutcome, deniedBy string) *string {
	if o.DeniedBy != deniedBy {
		return nil
	}
	c := o.Code
	return &c
}

func parseFundingSource(fundingSource string) (ledger.BonusFunding, *string, error) {
	if fundingSource == "operator" {
		return ledger.FundingOperator, nil, nil
	}
	const providerPrefix = "provider:"
	if len(fundingSource) > len(providerPrefix) && fundingSource[:len(providerPrefix)] == providerPrefix {
		id := fundingSource[len(providerPrefix):]
		return ledger.FundingProvider, &id, nil
	}
	return "", nil, fmt.Errorf("bonus: unrecognized funding_source %q", fundingSource)
}

func amountToInt64(v *big.Int) (int64, error) {
	if v == nil {
		return 0, nil
	}
	if !v.IsInt64() {
		return 0, fmt.Errorf("bonus: amount %s does not fit in int64 minor units", v.String())
	}
	n := v.Int64()
	if n < 0 {
		return 0, fmt.Errorf("bonus: amount must not be negative, got %s", v.String())
	}
	return n, nil
}

// RecordWageringContributionParams is one lock-transaction's contribution
// (doc 10 §W2.8/ledger-accounting-model.md §6.6.4, generalized by §7.18.3.2).
type RecordWageringContributionParams struct {
	OfferVersionID          uuid.UUID
	LockLedgerTransactionID uuid.UUID
	CorrelationID           uuid.UUID
	AssetCode               string
	// StakedBonusAmount is the qualifying stake amount, read from the
	// funding account's OWN posted debit on LockLedgerTransactionID -
	// player_bonus/player_locked_bonus for a bonus-funded lock, player_cash
	// for a cash-funded wagering-contribution Grant (§7.18.3.2's
	// generalization) - NEVER a caller-supplied/computed number. The field
	// name predates the cash-funded generalization and is left unchanged
	// (a documentation clarification only, not a schema/behavioral change).
	StakedBonusAmount    *big.Int
	ContributionWeightBP int32
	QualifyingScaled     *big.Int
	RoundingRuleID       *uuid.UUID
}

// RecordWageringContribution is the Bonus-side half of a qualifying
// bet's own posting transaction - called in the SAME database
// transaction as the posting itself (HR-10). For a Grant whose wagering
// mechanic is bonus-funded/locked-stake (the shape this function was
// originally written for), that posting is casino's own lock transaction
// (a player_bonus/player_locked_bonus debit). For a Grant whose wagering
// mechanic is the cash-funded mechanic Stage 4H-B1 Wave 3 authorizes
// (ledger-accounting-model.md §7.18.3.2's generalization), that posting
// is instead an ordinary, 100% player_cash-funded casino_bet transaction
// - StakedBonusAmount (the field name predates this generalization and is
// NOT renamed at the column/struct level, doc 29 BI-3-style stability) is
// then read from that transaction's own posted player_cash debit, never
// from the caller's own claim, exactly mirroring how the bonus-funded
// case reads a player_bonus/player_locked_bonus debit - "the qualifying
// stake amount, read from the funding account's own debit", never a
// caller-supplied number, regardless of which account funded the stake.
// The two funding sources are mutually exclusive per Grant (a Grant's own
// Offer fixes its mechanic at authoring time) and are never mixed on one
// contribution row.
//
// It writes the append-only WageringProgress row (idempotently - a
// redelivered/re-entrant call for the SAME (grant, lock transaction) pair
// is a no-op, per §7.18.3.5's hardening item), attributes the lock/bet
// transaction to the Grant, appends a Progress entry, and - if the Grant
// is still Activated (its very first contribution) - transitions it to
// InProgress. A redelivered contribution is skipped entirely (no second
// Progress entry, no redundant status transition) once the underlying
// WageringProgress row already exists.
func RecordWageringContribution(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, p RecordWageringContributionParams) error {
	if err := AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		return err
	}
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		return err
	}
	if ComputeNewStakeEligibility(g.Status) != StakeEligibilityOpen {
		return fmt.Errorf("%w: grant %s is not open for new stakes (status %s)", ErrIllegalTransition, grantID, g.Status)
	}

	_, created, err := CreateWageringProgressIdempotent(ctx, tx, WageringProgress{
		TenantID: tenantID, GrantID: grantID, PlayerAccountID: g.PlayerAccountID, OfferVersionID: p.OfferVersionID,
		LockLedgerTransactionID: p.LockLedgerTransactionID, CorrelationID: p.CorrelationID, AssetCode: p.AssetCode,
		StakedBonusAmount: p.StakedBonusAmount, ContributionWeightBP: p.ContributionWeightBP,
		QualifyingScaled: p.QualifyingScaled, RoundingRuleID: p.RoundingRuleID,
	})
	if err != nil {
		return err
	}
	if !created {
		// Already recorded (a redelivery/re-entrant call for the SAME
		// lock/bet transaction) - the Grant's status/Progress trail was
		// already updated the first time this was reached; doing so again
		// would double-append a Progress entry for the exact same stake.
		return nil
	}
	if err := AttributeGrantLedgerTransactionIdempotent(ctx, tx, tenantID, grantID, p.LockLedgerTransactionID, "casino_bet"); err != nil {
		return err
	}

	before := string(g.Status)
	after := before
	if g.Status == GrantActivated {
		if _, err := UpdateGrantStatus(ctx, tx, tenantID, grantID, GrantActivated, GrantInProgress, time.Now().UTC()); err != nil {
			return err
		}
		after = string(GrantInProgress)
	}
	amount := p.StakedBonusAmount
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: tenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: grantID,
		TransitionType: TransitionInProgressContribution, TriggerType: TriggerAutomatedRuleEvaluation,
		BeforeStatus: &before, AfterStatus: &after, ActorType: ActorSystem,
		Amount: amount, AssetCode: &p.AssetCode, LedgerTransactionID: &p.LockLedgerTransactionID, CorrelationID: &p.CorrelationID,
	}); err != nil {
		return err
	}
	return nil
}

// CheckAndCompleteGrant re-derives P_firm and, if it now meets or exceeds
// wageringTargetScaled, transitions activated/in_progress -> completed
// (doc 10 §1.3; W2.8: "only P_firm may authorize completed -> converted").
// A no-op (returns the Grant unchanged, completed=false) if the target is
// not yet met, or if the Grant is already completed/terminal - callers
// (a bet-settlement/contribution-processing loop) are expected to call
// this after every contribution without needing to track state
// themselves.
func CheckAndCompleteGrant(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, wageringTargetScaled *big.Int) (Grant, bool, error) {
	if err := AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		return Grant{}, false, err
	}
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		return Grant{}, false, err
	}
	if g.Status != GrantActivated && g.Status != GrantInProgress {
		return g, false, nil
	}

	progress, err := DeriveWageringProgress(ctx, tx, tenantID, grantID)
	if err != nil {
		return Grant{}, false, err
	}
	if wageringTargetScaled != nil && progress.PFirm.Cmp(wageringTargetScaled) < 0 {
		return g, false, nil
	}

	now := time.Now().UTC()
	completed, err := UpdateGrantStatus(ctx, tx, tenantID, grantID, g.Status, GrantCompleted, now)
	if err != nil {
		return Grant{}, false, err
	}
	before, after := string(g.Status), string(GrantCompleted)
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: tenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: grantID,
		TransitionType: TransitionCompleted, TriggerType: TriggerAutomatedRuleEvaluation,
		BeforeStatus: &before, AfterStatus: &after, ActorType: ActorSystem,
	}); err != nil {
		return Grant{}, false, err
	}
	return completed, true, nil
}

// terminalWriteDown posts the ordinary bonus_forfeiture write-down (ADR
// 0032 §5: "Dr player_bonus / Cr promo_liability on the outstanding
// balance") for whatever player_bonus balance is currently attributed to
// the Grant and free (never gated by AssetAuthorization/RG/Risk per doc
// 10 §T.5.1's value-reducing asymmetry - "cancellation only ever reduces
// exposure - there is nothing for a value-creating gate to protect").
// A zero outstanding balance posts nothing (no vacuous write-down).
func terminalWriteDown(ctx context.Context, tx pgx.Tx, g Grant, reasonCode string) (*uuid.UUID, error) {
	outstanding, err := RemainingBonusBalance(ctx, tx, g.TenantID, g.ID)
	if err != nil {
		return nil, err
	}
	if outstanding.Sign() <= 0 {
		return nil, nil
	}
	amountMinor, err := amountToInt64(outstanding)
	if err != nil {
		return nil, err
	}
	fundingKind, providerID, err := parseFundingSource(g.FundingSource)
	if err != nil {
		return nil, err
	}
	walletID := g.WalletID
	playerBonusAccount, err := ledger.GetOrCreateAccount(ctx, tx, g.TenantID, &walletID, ledger.AccountPlayerBonus, g.AssetCode)
	if err != nil {
		return nil, err
	}
	idempotencyKey := fmt.Sprintf("bonus_forfeiture:%s:%s", g.ID, reasonCode)
	result, err := ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: g.TenantID, TransactionType: ledger.TxBonusForfeiture, IdempotencyKey: idempotencyKey,
		CorrelationID: g.ID, ReasonCode: &reasonCode,
		Entries:   []ledger.EntryInput{{LedgerAccountID: playerBonusAccount, Direction: ledger.Debit, Amount: amountMinor}},
		BonusCost: &ledger.BonusCostAttribution{Funding: fundingKind, ProviderID: providerID},
	})
	if err != nil {
		return nil, err
	}
	if err := AttributeGrantLedgerTransactionIdempotent(ctx, tx, g.TenantID, g.ID, result.TransactionID, string(ledger.TxBonusForfeiture)); err != nil {
		return nil, err
	}
	return &result.TransactionID, nil
}

// TerminateGrantParams is the shared input for Expire/Cancel/Forfeit
// (doc 10 §T.9/T.10, N1.4).
type TerminateGrantParams struct {
	Resolution    TerminalResolution
	ReasonCode    string
	ActorType     ActorType
	ActorID       uuid.UUID
	TriggerType   TriggerType
	CorrelationID *uuid.UUID
}

// TerminateGrant implements doc 10's expiry/cancellation/forfeiture
// transitions UNDER N1.4's pending_settlement mechanism, exactly:
//  1. NewStakeEligibility(G, .) flips to closed immediately (mechanical -
//     enforced by every wagering-authorization call site reading
//     G.status, not by this function; this function only records the
//     terminal trigger).
//  2. AOE(G, .) is computed live, inside this transaction, under the
//     advisory lock (N1.4 step 2).
//  3. If AOE = empty: the terminal status is written directly (N1.4 step
//     3 - "the ordinary case today, fully unmodified").
//  4. If AOE != empty: G.status becomes pending_settlement instead (N1.4
//     step 4), the terminal_resolution/reason/trigger fields are
//     recorded, and whatever portion of the write-down is already free
//     right now still posts now.
//
// Never gated by AssetAuthorization/RG/Risk (§T.5.1/§T.10's asymmetry -
// a write-down only ever reduces exposure).
func TerminateGrant(ctx context.Context, tx pgx.Tx, tenantID, grantID uuid.UUID, p TerminateGrantParams) (Grant, error) {
	if err := AdvisoryLockGrant(ctx, tx, tenantID, grantID); err != nil {
		return Grant{}, err
	}
	g, err := LockGrantForUpdate(ctx, tx, grantID)
	if err != nil {
		return Grant{}, err
	}
	if ComputeNewStakeEligibility(g.Status) != StakeEligibilityOpen {
		return Grant{}, fmt.Errorf("%w: terminate requires an open (issued/activated/in_progress) grant, got %s", ErrIllegalTransition, g.Status)
	}

	now := time.Now().UTC()
	ledgerTxID, err := terminalWriteDown(ctx, tx, g, p.ReasonCode)
	if err != nil {
		return Grant{}, err
	}

	aoe, err := ComputeAOE(ctx, tx, tenantID, grantID)
	if err != nil {
		return Grant{}, err
	}

	before := string(g.Status)
	var result Grant
	var transitionType TransitionType
	var afterStatus string

	if aoe.IsEmpty() {
		terminalStatus := terminalResolutionToStatus(p.Resolution)
		result, err = UpdateGrantStatus(ctx, tx, tenantID, grantID, g.Status, terminalStatus, now)
		if err != nil {
			return Grant{}, err
		}
		transitionType = terminalResolutionToTransitionType(p.Resolution)
		afterStatus = string(terminalStatus)
	} else {
		correlationID := grantID
		if p.CorrelationID != nil {
			correlationID = *p.CorrelationID
		}
		result, err = SetGrantPendingSettlement(ctx, tx, tenantID, grantID, g.Status, p.Resolution, p.ReasonCode, now, correlationID)
		if err != nil {
			return Grant{}, err
		}
		transitionType = TransitionPendingSettlementDeferred
		afterStatus = string(GrantPendingSettlement)
	}

	reasonCode := p.ReasonCode
	if _, err := AppendGrantProgress(ctx, tx, GrantProgressEntry{
		TenantID: g.TenantID, BrandID: g.BrandID, PlayerAccountID: g.PlayerAccountID, GrantID: g.ID,
		TransitionType: transitionType, TriggerType: p.TriggerType,
		BeforeStatus: &before, AfterStatus: &afterStatus, ActorType: p.ActorType, ActorID: nonNilActorID(p.ActorType, p.ActorID),
		ReasonCode: &reasonCode, LedgerTransactionID: ledgerTxID, CorrelationID: p.CorrelationID,
	}); err != nil {
		return Grant{}, err
	}
	if err := audit.Record(ctx, tx, audit.Entry{
		TenantID: g.TenantID, ActorType: audit.ActorType(p.ActorType), ActorID: p.ActorID,
		Action: "bonus_grant." + string(transitionType), TargetType: "bonus_grant", TargetID: g.ID.String(),
		Outcome: audit.OutcomeSuccess, Metadata: map[string]any{"reason_code": p.ReasonCode},
	}); err != nil {
		return Grant{}, err
	}
	return result, nil
}

func terminalResolutionToStatus(r TerminalResolution) GrantStatus {
	switch r {
	case TerminalResolutionExpired:
		return GrantExpired
	case TerminalResolutionCancelled:
		return GrantCancelled
	case TerminalResolutionForfeited:
		return GrantForfeited
	}
	return GrantCancelled
}

func terminalResolutionToTransitionType(r TerminalResolution) TransitionType {
	switch r {
	case TerminalResolutionExpired:
		return TransitionExpired
	case TerminalResolutionCancelled:
		return TransitionCancelled
	case TerminalResolutionForfeited:
		return TransitionForfeited
	}
	return TransitionCancelled
}
