// The five in-slice bonus types (Stage 4H-B0's scope plan §1, re-
// confirmed unchanged by the human directive's own scope section):
// Deposit Bonus, Reload Bonus, Cashback, Generic Wagering Bonus, Coupon.
// Each composes doc 10 §W3's canonical mechanics (Trigger x Reward x
// Completion) with NO new financial mechanism - this file is thin
// orchestration over lifecycle.go's transitions plus each type's own
// amount-computation formula (Dependency Freeze §7's DS-1/DS-2 rounding
// discipline via internal/money.RoundToMinorUnits).
package bonus

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/money"
)

// placeholderRoundingRuleID stands in for Dependency Contract Freeze §7's
// "immutable, append-only rounding_rules reference table", which does not
// exist in this repository as of this stage (confirmed by direct search
// of migrations/ - no CREATE TABLE rounding_rules anywhere; ledger-
// finance's own money.go doc comment: "requested, not claimed... left for
// the Orchestrator to sequence after this dispatch"). money.RoundToMinorUnits
// only structurally requires a non-nil id (it cannot validate existence/
// activeness of a rule against a table that does not exist) - this
// constant is a STABLE, well-known placeholder standing in for "the
// platform default DS-1/DS-2 rounding rule", used consistently by every
// bonus-type computation in this file, so every Grant this Phase creates
// at least carries a consistent, auditable, non-zero rounding_rule_id
// pending that table's own migration. NAMED GAP, not silently invented as
// if it were a real reference-table row.
var placeholderRoundingRuleID = uuid.MustParse("00000000-0000-4000-a000-000000000001")

// computeCappedPercentageReward implements Dependency Freeze §7's binding
// rule for R1 (percentage-of-qualifying-amount-with-cap): "round
// min(exact_amount x rate_%, cap) ONCE, after both the percentage
// multiply and the cap comparison." rateBP is basis points (10000 =
// 100%), never a float.
func computeCappedPercentageReward(qualifyingAmount *big.Int, rateBP int32, cap *big.Int, decimalExponent int32) (*big.Int, error) {
	if qualifyingAmount == nil || qualifyingAmount.Sign() < 0 {
		return nil, fmt.Errorf("bonus: qualifying amount must be non-negative")
	}
	// money.RoundToMinorUnits takes `exact` in the asset's MAJOR
	// denomination (its own doc comment: "exact=10.5, decimalExponent=2
	// -> 1050") and itself multiplies by 10^decimalExponent to produce
	// minor units. Every OTHER amount in this package (qualifyingAmount,
	// cap, the resulting reward) is already minor units, matching this
	// platform's own money convention everywhere else (the ledger,
	// wagering progress, EOI budgets). This function converts qualifying/
	// cap DOWN to major units first, so the percentage multiply/cap
	// comparison happen in the same terms RoundToMinorUnits expects, and
	// its own rescale converts the result back UP to minor units exactly
	// once (DS-2's own "round once, at the final boundary" rule) - never
	// double-scaled.
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimalExponent)), nil)
	scaleRat := new(big.Rat).SetInt(scale)

	exact := new(big.Rat).SetInt(qualifyingAmount)
	exact.Quo(exact, scaleRat)
	exact.Mul(exact, big.NewRat(int64(rateBP), 10000))
	if cap != nil {
		capMajor := new(big.Rat).SetInt(cap)
		capMajor.Quo(capMajor, scaleRat)
		if exact.Cmp(capMajor) > 0 {
			exact = capMajor
		}
	}
	return money.RoundToMinorUnits(exact, decimalExponent, placeholderRoundingRuleID)
}

// lookupExistingGrantByTrigger resolves doc 10 §9's idempotency guarantee
// ("a redelivered deposit.settled event for the same deposit must never
// produce a second Grant") the honest way: CreateGrant relies on the REAL
// DB-enforced UNIQUE (tenant_id, campaign_id, offer_version_id,
// player_account_id, trigger_reference) constraint (migration 0057),
// never check-then-insert; on a unique violation, this looks the existing
// row up and returns it instead of erroring.
func lookupExistingGrantByTrigger(ctx context.Context, tx pgx.Tx, tenantID, campaignID, offerVersionID, playerAccountID uuid.UUID, triggerReference string) (Grant, error) {
	row := tx.QueryRow(ctx, `SELECT `+grantColumns+` FROM bonus_grants
		WHERE tenant_id = $1 AND campaign_id = $2 AND offer_version_id = $3 AND player_account_id = $4 AND trigger_reference = $5`,
		tenantID, campaignID, offerVersionID, playerAccountID, triggerReference,
	)
	return scanGrant(row)
}

// ErrAlreadyGranted is returned by an idempotent issuance wrapper when
// the underlying (tenant, campaign, offer_version, player, trigger_reference)
// tuple already has a Grant - the caller should treat this exactly like
// a successful, already-completed issuance (doc 10 §9), never retry with
// a different trigger_reference to "force" a second grant.
var ErrAlreadyGranted = errors.New("bonus: a grant already exists for this (campaign, offer version, player, trigger) tuple")

func issueIdempotent(ctx context.Context, tx pgx.Tx, p IssueGrantParams) (Grant, GateOutcome, error) {
	created, outcome, err := IssueGrant(ctx, tx, p)
	if err != nil {
		if errors.Is(err, ErrGrantAlreadyExists) {
			existing, lookupErr := lookupExistingGrantByTrigger(ctx, tx, p.Grant.TenantID, p.Grant.CampaignID, p.Grant.OfferVersionID, p.Grant.PlayerAccountID, p.Grant.TriggerReference)
			if lookupErr != nil {
				return Grant{}, GateOutcome{}, lookupErr
			}
			return existing, GateOutcome{Allowed: true}, ErrAlreadyGranted
		}
		return Grant{}, GateOutcome{}, err
	}
	return created, outcome, nil
}

// DepositBonusParams covers BOTH Deposit Bonus and Reload Bonus (doc 10
// §2: "Reload bonus - identical Offer shape to deposit bonus, eligibility
// axis only distinguishes it" - no new lifecycle concept, one function).
type DepositBonusParams struct {
	Grant         Grant // caller has already populated every T.2-frozen field except computed amount
	DepositAmount *big.Int
	RateBP        int32
	CapAmount     *big.Int // nil = uncapped
	MinQualifying *big.Int // nil = no minimum
	MaxQualifying *big.Int // nil = no maximum
	ActorType     ActorType
	ActorID       uuid.UUID
	// WageringTimeLimit, if set, is threaded through to
	// ActivateGrantParams (see its own doc comment) - the issuing Offer
	// version's configured wagering-completion window, nil meaning "never
	// expires". Stage 4H-B1 Wave 3.
	WageringTimeLimit *time.Duration
}

// IssueAndActivateDepositBonus implements the Deposit/Reload Bonus type:
// idempotent trigger on a deposit event (doc 10 §9's own idempotency key,
// enforced at the DB), tenant/brand/asset-safe (every field flows through
// the ordinary Grant/gate machinery, never a bonus-type-specific
// shortcut), Risk/RG/campaign-eligibility-gated before posting (T.1's
// full gate at activation), never grants twice for the same deposit (the
// real DB-enforced idempotency key - db.IsUniqueViolation, never check-
// then-insert). Per ADR 0032 §3.1's "one posting, not two" rule for a
// no-opt-in, deposit-triggered Offer, issuance and activation collapse
// into one caller-visible operation.
func IssueAndActivateDepositBonus(ctx context.Context, tx pgx.Tx, p DepositBonusParams) (Grant, GateOutcome, error) {
	return issueAndActivateDepositBonus(ctx, tx, p, activateGrantProd)
}

// issueAndActivateDepositBonus is IssueAndActivateDepositBonus's shared
// implementation, parameterized on the activateGrantFunc seam (see that
// type's own doc comment, targeting.go).
//
// DR-4I-BONUS-01 CLOSURE (`architect`, Stage 4I final cross-domain
// certification): this is the third and last instance of the hand-copy
// drift class SEC-4I-F6 (`security`) and SEC-4I-F7 (`qa`) each closed
// elsewhere in this package. `qa` disclosed it as a residual rather than
// fixing it; it is closed here because of WHAT the copy was hiding, which
// is materially more than test hygiene: this function's clamp-then-cap-
// then-round ordering IS the financial invariant ADR 0021's DS-2 records
// (round once, at the final monetary boundary, after the cap comparison),
// and the platform's own ledger-finance certification suite
// (wave3_ledger_finance_certification_integration_test.go) reaches it ONLY
// through the former hand-copy. A regression in the production ordering
// would therefore not have been caught by the very tests that certify that
// ordering. Nothing about the invariant is changed here - only which code
// the certification actually exercises.
func issueAndActivateDepositBonus(ctx context.Context, tx pgx.Tx, p DepositBonusParams, activate activateGrantFunc) (Grant, GateOutcome, error) {
	if p.MinQualifying != nil && p.DepositAmount.Cmp(p.MinQualifying) < 0 {
		return Grant{}, GateOutcome{Allowed: false, DeniedBy: "eligibility_axis", Code: "below_min_qualifying_amount"}, nil
	}
	qualifying := p.DepositAmount
	if p.MaxQualifying != nil && qualifying.Cmp(p.MaxQualifying) > 0 {
		qualifying = p.MaxQualifying
	}
	amount, err := computeCappedPercentageReward(qualifying, p.RateBP, p.CapAmount, p.Grant.DecimalExponent)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}

	g, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: p.Grant})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		return Grant{}, GateOutcome{}, err
	}
	if errors.Is(err, ErrAlreadyGranted) {
		return g, issueOutcome, nil
	}
	if !issueOutcome.Allowed {
		return g, issueOutcome, nil
	}

	activated, activateOutcome, err := activate(ctx, tx, g.TenantID, g.ID, ActivateGrantParams{
		ActorType: TriggerActorForAutomated(p.ActorType), ActorID: p.ActorID, Amount: amount,
		WageringTimeLimit: p.WageringTimeLimit,
	})
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	return activated, activateOutcome, nil
}

// TriggerActorForAutomated normalizes an issuance's own actor type into
// the actor that performs an automated, no-opt-in activation - system,
// unless the caller is explicitly a staff member driving a manual grant
// (the same actor performs both halves of the collapsed issue+activate
// operation).
func TriggerActorForAutomated(actor ActorType) ActorType {
	if actor == ActorStaff {
		return ActorStaff
	}
	return ActorSystem
}

// CashbackParams is one cashback-window settlement job's own input (doc
// 10 §2's Cashback row: "completion is time/window-based... the
// settlement job's own comparison against 'now' to decide the window has
// elapsed MUST read clock_timestamp(), never now()").
type CashbackParams struct {
	Grant         Grant
	NetLossAmount *big.Int // this window's net loss, already computed by the caller from ledger reads - never invented here
	RateBP        int32
	CapAmount     *big.Int // the Offer's declared maximum (also the EOI value-budget conservative-maximum, doc 34 §3.4 RK-W15P2-5)
	ActorType     ActorType
	ActorID       uuid.UUID
}

// IssueAndActivateCashback implements the Cashback bonus type:
// independent per-period rounding (doc 10 Dependency Freeze §7's
// "cashback residual" rule - "each cashback calculation rounds
// independently and immediately, round-half-up, with no accumulation;
// the discarded sub-minor-unit fraction is not owed to the player and
// never becomes a ledger fact" - this function is called ONCE per window,
// computes ONE rounded amount from THIS window's own NetLossAmount only,
// and carries no state into the next call), capped by CapAmount BEFORE
// rounding (Dependency Freeze §7's own binding per-bonus-type rule).
// Completion mechanic C2 (time-window settlement) means this function
// IS the completion trigger, not merely the reward computation - a fresh
// Grant per window, immediately completed since a cashback Offer's
// Wagering axis is a no-op by design (doc 10 §2).
func IssueAndActivateCashback(ctx context.Context, tx pgx.Tx, p CashbackParams) (Grant, GateOutcome, error) {
	return issueAndActivateCashback(ctx, tx, p, activateGrantProd)
}

// issueAndActivateCashback is IssueAndActivateCashback's shared
// implementation, parameterized on the activateGrantFunc seam - see
// issueAndActivateDepositBonus's own doc comment (above) for the full
// DR-4I-BONUS-01 reasoning. Cashback's case is the sharper one: the
// per-window, no-accumulation rounding rule this function implements is
// itself a recorded decision (ADR 0021's cashback-residual rule, restated
// in this function's own doc comment), and it was likewise certified only
// through a hand-copy.
func issueAndActivateCashback(ctx context.Context, tx pgx.Tx, p CashbackParams, activate activateGrantFunc) (Grant, GateOutcome, error) {
	amount, err := computeCappedPercentageReward(p.NetLossAmount, p.RateBP, p.CapAmount, p.Grant.DecimalExponent)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}

	g, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: p.Grant})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		return Grant{}, GateOutcome{}, err
	}
	if errors.Is(err, ErrAlreadyGranted) {
		return g, issueOutcome, nil
	}
	if !issueOutcome.Allowed {
		return g, issueOutcome, nil
	}

	activated, activateOutcome, err := activate(ctx, tx, g.TenantID, g.ID, ActivateGrantParams{
		ActorType: ActorSystem, ActorID: p.ActorID, Amount: amount,
	})
	if err != nil || !activateOutcome.Allowed {
		return activated, activateOutcome, err
	}

	// C3-equivalent completion: a cashback Offer's Wagering axis is a
	// no-op (doc 10 §2), so completion follows activation immediately
	// with no wagering-multiplier target (nil target = "already
	// satisfied").
	completed, _, err := CheckAndCompleteGrant(ctx, tx, g.TenantID, g.ID, nil)
	if err != nil {
		return Grant{}, GateOutcome{}, err
	}
	return completed, GateOutcome{Allowed: true}, nil
}

// GenericWageringBonusParams is the Generic Wagering Bonus type's own
// input (doc 10 §2's "wagering bonus (generic multiplier bonus... no
// deposit trigger)" row - the general-purpose Offer shape for any
// operator-authored promotion whose eligibility is deposit/segment/
// coupon-based rather than mission/tournament-triggered). This is also
// the shape a Coupon redemption (types below) or a manual grant ends up
// using once issued - the reward may be a fixed amount (R2) or a
// percentage-with-cap (R1); the caller supplies the already-computed
// Amount either way, since this type's own defining property is its
// COMPLETION mechanic (C1, wagering-multiplier), not its reward formula.
type GenericWageringBonusParams struct {
	Grant             Grant
	Amount            *big.Int
	ActorType         ActorType
	ActorID           uuid.UUID
	WageringTimeLimit *time.Duration // see DepositBonusParams' identical field
}

// IssueAndActivateGenericWageringBonus issues and activates a Grant whose
// completion mechanic is C1 (wagering-multiplier) - the general-purpose
// in-slice Offer shape (doc 10 §2/§W3). Completion itself is driven by
// RecordWageringContribution + CheckAndCompleteGrant as bets settle, not
// by this function.
func IssueAndActivateGenericWageringBonus(ctx context.Context, tx pgx.Tx, p GenericWageringBonusParams) (Grant, GateOutcome, error) {
	g, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: p.Grant})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		return Grant{}, GateOutcome{}, err
	}
	if errors.Is(err, ErrAlreadyGranted) {
		return g, issueOutcome, nil
	}
	if !issueOutcome.Allowed {
		return g, issueOutcome, nil
	}
	return ActivateGrant(ctx, tx, g.TenantID, g.ID, ActivateGrantParams{
		ActorType: p.ActorType, ActorID: p.ActorID, Amount: p.Amount,
		WageringTimeLimit: p.WageringTimeLimit,
	})
}

// CouponRedemptionParams is a Coupon redemption's own input (doc 10 §2's
// Coupon row / §W4: "the only lifecycle novelty is the trigger... being a
// player-supplied code validated against an Offer, otherwise identical to
// a deposit/cash bonus" - trigger mechanic T2 composed with any reward/
// completion mechanic, never a parallel object or a sixth reward shape).
type CouponRedemptionParams struct {
	Grant             Grant // TriggerReference MUST be set to the redeemed code (or a per-attempt id) by the caller - the per-player redemption-limit uniqueness IS the (campaign, offer_version, player, trigger_reference) constraint already enforced at grant.go's own DB level (doc 10 §W4: "a per-player limit is enforced as an ordinary DB uniqueness constraint on the redemption attempt")
	Amount            *big.Int
	ActorID           uuid.UUID      // the redeeming player's own principal
	WageringTimeLimit *time.Duration // see DepositBonusParams' identical field
}

// RedeemCoupon validates NOTHING about the code's format/pool/stacking-
// conflict predicate itself (doc 10 §W4: "the code-redemption endpoint
// does exactly three things - validate the code's format, resolve it to
// an Offer, and check the redemption-limit constraint - before handing
// off to the IDENTICAL (none) -> issued pipeline every other trigger
// uses" - those three things are the CALLER's job, e.g. the HTTP handler
// resolving the code to an OfferVersion before ever constructing Grant).
// This function IS that identical pipeline: AssetAuthorization -> RG ->
// Risk (T.1), the eligibility-axis snapshot (already baked into
// p.Grant.EligibilitySnapshot by the caller), and §9's idempotency key -
// a code is only a different trigger_reference value, never a shortcut.
func RedeemCoupon(ctx context.Context, tx pgx.Tx, p CouponRedemptionParams) (Grant, GateOutcome, error) {
	p.Grant.CreatedByActorType = ActorPlayer
	p.Grant.CreatedByActorID = p.ActorID
	g, issueOutcome, err := issueIdempotent(ctx, tx, IssueGrantParams{Grant: p.Grant})
	if err != nil && !errors.Is(err, ErrAlreadyGranted) {
		return Grant{}, GateOutcome{}, err
	}
	if errors.Is(err, ErrAlreadyGranted) {
		return g, issueOutcome, nil
	}
	if !issueOutcome.Allowed {
		return g, issueOutcome, nil
	}
	return ActivateGrant(ctx, tx, g.TenantID, g.ID, ActivateGrantParams{
		ActorType: ActorPlayer, ActorID: p.ActorID, Amount: p.Amount,
		WageringTimeLimit: p.WageringTimeLimit,
	})
}
