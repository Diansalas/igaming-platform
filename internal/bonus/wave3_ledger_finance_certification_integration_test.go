//go:build integration

// Stage 4H-B1 Wave 3 Phase 11 (`ledger-finance`, final financial
// certification) — the B1 proof `qa`'s own Phase 9 §4 explicitly
// disclosed as MISSING:
//
//	"No test anywhere, including this phase's own new ones, ever reaches
//	a successful bonus_grant posting via either sweep — so Invariant B1
//	(extended) holds TRIVIALLY in every one of these tests (zero postings
//	occur), not because it was proven under a genuine successful posting."
//
// Certifying `SUM(DEBITS) == SUM(CREDITS)` on a mechanism that never
// posts is certifying nothing. This file closes that gap the only way
// that is honest: it drives the deposit sweep's and the cashback
// scheduler's OWN candidate-resolution and amount-derivation chains —
// the real, unmodified production functions (listUnprocessedDeposits,
// listDepositMatchableOfferVersions, matchesDepositEligibility,
// parsePercentageRewardCalculation, lookupDecimalExponent,
// listCashbackMatchableCampaigns, cashbackWindowSeconds,
// cashbackCandidatesAndNetLoss, resolvePlayerWallet, playerBrandID) —
// and then calls the same IssueAndActivateDepositBonus /
// IssueAndActivateCashback entry point the sweeps call, with exactly the
// parameter struct the sweeps build.
//
// THE ONE SUBSTITUTION, STATED PLAINLY RATHER THAN HIDDEN, UPDATED FOR
// STAGE 4I: this file originally substituted the fixture's own real,
// fully dual-control-authorized jurisdiction code for the sweeps' own
// JurisdictionCode: "" (RunDepositSweepForTenant's disclosed limitation),
// so AssetAuthorization would genuinely ALLOW and this file's own B1
// proof could reach a real posting. Stage 4I's jurisdiction resolver
// (internal/jurisdiction, docs/governance/stage-4i-canonical-model.md)
// makes that substitution structurally impossible now: bonus_grants and
// bonus_grant activation are ALWAYS player-scoped operations, and the
// resolver is, by design, incapable of resolving ANY jurisdiction for a
// player-scoped operation today (HDR-J-1/HDR-J-3 unanswered - see
// resolveGrantJurisdiction's own doc comment, eligibility.go) - there is
// no longer any code path, test or production, that can make
// AssetAuthorization ALLOW a Bonus activation. This file now uses
// forceIssueAndActivateDepositBonusForTest/forceIssueAndActivateCashbackForTest
// (lifecycle_integration_test.go) to reach the SAME real posting this
// file's own B1 proof needs, skipping ONLY T.1's gate call - every other
// line (candidate resolution, amount derivation, the ledger posting
// itself) is the real, unmodified production code this file's own header
// above already documents driving directly. What was already true before
// Stage 4I remains true after it: what is not proven here is "a sweep
// supplies a real jurisdiction" (now provably impossible, not merely
// unproven) - everything downstream of activation, where the arithmetic
// and the double-entry live, IS proven.
package bonus

import (
	"context"
	"fmt"
	"math/big"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// postingSums is one ledger transaction's own debit/credit totals plus
// the per-account-type breakdown B1 and Rule B2 are asserted against.
type postingSums struct {
	transactionID uuid.UUID
	debitTotal    int64
	creditTotal   int64
	byAccountType map[string]int64 // signed: +credit, -debit
	entryCount    int
}

// readPostingForGrant loads the single `bonus_grant` ledger transaction
// ActivateGrant posts for grantID (CorrelationID = the Grant's own id,
// lifecycle.go) and totals its entries. Fails the test if the count is
// anything other than exactly one — a second posting for the same Grant
// would itself be the defect.
func readPostingForGrant(t *testing.T, pool *db.Pool, tenantID, grantID uuid.UUID, txType ledger.TransactionType) postingSums {
	t.Helper()
	var s postingSums
	s.byAccountType = map[string]int64{}
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND correlation_id = $2 AND transaction_type = $3`,
			tenantID, grantID, string(txType),
		).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("expected exactly one %s transaction for grant %s, found %d", txType, grantID, count)
		}
		if err := tx.QueryRow(ctx,
			`SELECT id FROM ledger_transactions WHERE tenant_id = $1 AND correlation_id = $2 AND transaction_type = $3`,
			tenantID, grantID, string(txType),
		).Scan(&s.transactionID); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `
			SELECT la.account_type, le.direction, le.amount
			  FROM ledger_entries le
			  JOIN ledger_accounts la ON la.id = le.ledger_account_id AND la.tenant_id = le.tenant_id
			 WHERE le.tenant_id = $1 AND le.ledger_transaction_id = $2`,
			tenantID, s.transactionID,
		)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var accountType, direction string
			var amount int64
			if err := rows.Scan(&accountType, &direction, &amount); err != nil {
				return err
			}
			s.entryCount++
			if direction == string(ledger.Debit) {
				s.debitTotal += amount
				s.byAccountType[accountType] -= amount
			} else {
				s.creditTotal += amount
				s.byAccountType[accountType] += amount
			}
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read posting for grant %s: %v", grantID, err)
	}
	return s
}

// assertBalanced is the certification's core assertion: Invariant B1,
// CLAUDE.md's own "SUM(DEBITS) == SUM(CREDITS) always holds", checked on
// a REAL posting rather than on the empty set.
func assertBalanced(t *testing.T, s postingSums, label string) {
	t.Helper()
	if s.entryCount == 0 {
		t.Fatalf("%s: no ledger entries at all — B1 would hold trivially, which proves nothing", label)
	}
	if s.debitTotal != s.creditTotal {
		t.Fatalf("%s: B1 VIOLATED — SUM(debits)=%d != SUM(credits)=%d on transaction %s",
			label, s.debitTotal, s.creditTotal, s.transactionID)
	}
	if s.debitTotal == 0 {
		t.Fatalf("%s: both totals are zero — nothing was actually posted", label)
	}
}

// assertReconciliationClean recomputes every ledger account's balance
// from ledger_entries and diffs it against wallet_balance_projection for
// this tenant (the SAME generic sweep the hourly reconciliation job
// runs). Proves item 6 of this phase's scope — that the Wave's new
// posting-adjacent paths participate in the existing reconciliation
// lineage automatically, because they post through internal/ledger.Post
// rather than inventing a parallel path.
func assertReconciliationClean(t *testing.T, pool *db.Pool, tenantID uuid.UUID, label string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, mismatches, err := reconciliation.RunLedgerVsProjection(ctx, tx, tenantID,
			time.Now().UTC().Add(-24*time.Hour), time.Now().UTC())
		if err != nil {
			return err
		}
		if len(mismatches) != 0 {
			return fmt.Errorf("%s: reconciliation found %d ledger-vs-projection mismatch(es), first=%+v", label, len(mismatches), mismatches[0])
		}
		if run.Status != reconciliation.StatusClean {
			return fmt.Errorf("%s: reconciliation run status %q, want %q", label, run.Status, reconciliation.StatusClean)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// runDepositSweepChainWithJurisdiction replays RunDepositSweepForTenant's
// OWN body against the real production helpers, substituting only the
// jurisdiction code (see this file's header). Returns the Grant and the
// gate outcome for the single expected match.
// ignoreCursor=true replays every deposit from the beginning regardless
// of the tenant's stored watermark, so the REPLAY assertion below
// exercises the DB-enforced `bonus_grants` unique constraint itself
// rather than the cursor that would ordinarily stop the sweep earlier.
// (The cursor's own idempotency is separately covered by Phase 3's
// TestRunDepositSweepForTenant_MatchesAndAdvancesWatermark.)
func runDepositSweepChain(t *testing.T, pool *db.Pool, f lifecycleFixture, ignoreCursor bool) (Grant, GateOutcome) {
	t.Helper()
	var grant Grant
	var outcome GateOutcome
	matches := 0
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var w depositSweepWatermark
		var err error
		if !ignoreCursor {
			w, err = getDepositSweepWatermark(ctx, tx, f.tenantID, DepositSweepConsumerName)
			if err != nil {
				return err
			}
		}
		events, err := listUnprocessedDeposits(ctx, tx, f.tenantID, w)
		if err != nil {
			return err
		}
		for _, event := range events {
			candidates, err := listDepositMatchableOfferVersions(ctx, tx, f.tenantID, event.BrandID, event.AssetCode, event.PostedAt)
			if err != nil {
				return err
			}
			for _, c := range candidates {
				matched, err := matchesDepositEligibility(ctx, tx, f.tenantID, event, c.OfferVersion)
				if err != nil {
					return err
				}
				if !matched {
					continue
				}
				rateBP, capAmount, err := parsePercentageRewardCalculation(c.OfferVersion.RewardCalculation)
				if err != nil {
					return err
				}
				exp, err := lookupDecimalExponent(ctx, tx, c.OfferVersion.RewardAssetCode)
				if err != nil {
					return err
				}
				g := Grant{
					TenantID: f.tenantID, BrandID: event.BrandID, PlayerAccountID: event.PlayerAccountID, WalletID: event.WalletID,
					CampaignID: c.CampaignID, CampaignVersionID: c.CampaignVersionID, OfferID: c.OfferID, OfferVersionID: c.OfferVersion.ID,
					AssetCode: c.OfferVersion.RewardAssetCode, DecimalExponent: exp,
					FundingSource: c.OfferVersion.FundingSource, FulfillmentDestination: c.OfferVersion.FulfillmentDestination,
					FulfillmentOwner: c.FulfillmentOwner, TriggerReference: event.LedgerTransactionID.String(),
					EligibilitySnapshot: []byte(fmt.Sprintf(`{"deposit_ledger_transaction_id":%q,"payment_method":%q}`, event.LedgerTransactionID, event.PaymentMethod)),
					CreatedByActorType:  ActorSystem,
				}
				matches++
				grant, outcome = forceIssueAndActivateDepositBonusForTest(t, ctx, tx, DepositBonusParams{
					Grant: g, DepositAmount: event.Amount, RateBP: rateBP, CapAmount: capAmount,
					MinQualifying: c.OfferVersion.MinQualifyingAmount, MaxQualifying: c.OfferVersion.MaxQualifyingAmount,
					ActorType: ActorSystem, ActorID: uuid.Nil,
					WageringTimeLimit: c.OfferVersion.WageringTimeLimit,
				})
			}
			if err := advanceDepositSweepWatermark(ctx, tx, f.tenantID, DepositSweepConsumerName, event.LedgerTransactionID, event.PostedAt); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("deposit sweep chain: %v", err)
	}
	if matches != 1 {
		t.Fatalf("expected exactly one matched deposit/offer pair, got %d", matches)
	}
	return grant, outcome
}

// TestLFCert_DepositSweepPosting_B1AndRoundingAndReconciliation is the
// deposit half of this phase's B1 proof. The deposit amount (333 minor
// units at a 50% rate) is chosen deliberately: 333 × 5000bp = 166.5
// minor units EXACTLY, a rounding tie — so the posted amount also proves
// money.RoundToMinorUnits' DS-1 ties-away-from-zero rule and DS-2's
// round-ONCE-at-the-final-boundary rule survive all the way to the
// ledger, rather than being lost to a premature truncation somewhere in
// the sweep's own derivation chain.
func TestLFCert_DepositSweepPosting_B1AndRoundingAndReconciliation(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedDepositMatchableOffer(t, pool, f, false, nil) // rate_bp 5000, cap_amount 100000
	depositTxID := seedRawDeposit(t, pool, f, 333, "card")

	grant, outcome := runDepositSweepChain(t, pool, f, false)
	if !outcome.Allowed {
		t.Fatalf("the full T.1 gate chain denied the deposit-bonus activation (denied_by=%s code=%s) — "+
			"this test cannot certify B1 without a real posting", outcome.DeniedBy, outcome.Code)
	}
	if grant.Status != GrantActivated {
		t.Fatalf("grant status = %s, want %s", grant.Status, GrantActivated)
	}
	if grant.TriggerReference != depositTxID.String() {
		t.Fatalf("trigger_reference = %q, want the deposit's own ledger transaction id %q", grant.TriggerReference, depositTxID)
	}

	const wantReward = int64(167) // 333 * 0.50 = 166.5 -> DS-1 ties away from zero -> 167

	// The immutable computation input (migration 0067) must equal the
	// posted amount exactly - never a separately-derived number.
	var grantedAmount *big.Int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		grantedAmount, err = getGrantedAmount(ctx, tx, f.tenantID, grant.ID)
		return err
	})
	if err != nil {
		t.Fatalf("read granted_amount: %v", err)
	}
	if grantedAmount == nil || grantedAmount.Int64() != wantReward {
		t.Fatalf("granted_amount = %v, want %d", grantedAmount, wantReward)
	}

	s := readPostingForGrant(t, pool, f.tenantID, grant.ID, ledger.TxBonusGrant)
	assertBalanced(t, s, "deposit-sweep bonus_grant posting")

	if got := s.byAccountType[string(ledger.AccountPlayerBonus)]; got != wantReward {
		t.Fatalf("player_bonus net credit = %d, want %d (the rounded reward, posted once)", got, wantReward)
	}
	// Rule B2 (extended) mirror: the caller supplies only the
	// Cr player_bonus leg; the generator must have supplied the balancing
	// Dr promo_liability leg. If it had not, B1 above would already have
	// failed - this asserts WHICH account balanced it, so a future change
	// that balances against the wrong account is caught here rather than
	// silently satisfying B1.
	if got := s.byAccountType[string(ledger.AccountPromoLiability)]; got != -wantReward {
		t.Fatalf("promo_liability net = %d, want %d (the Rule B2 mirror debit)", got, -wantReward)
	}
	if s.entryCount != 2 {
		t.Fatalf("entry count = %d, want 2 (Cr player_bonus + Dr promo_liability)", s.entryCount)
	}

	assertReconciliationClean(t, pool, f.tenantID, "after deposit-sweep bonus_grant posting")

	// DUPLICATE / REPLAY: re-running the sweep chain for the SAME deposit,
	// with the watermark deliberately ignored, must resolve to the
	// existing Grant (ErrAlreadyGranted, raised by the DB-enforced
	// UNIQUE (tenant_id, campaign_id, offer_version_id, player_account_id,
	// trigger_reference) constraint and handled inside
	// IssueAndActivateDepositBonus) and must post NOTHING new.
	replayGrant, _ := runDepositSweepChain(t, pool, f, true)
	if replayGrant.ID != grant.ID {
		t.Fatalf("replay produced a DIFFERENT grant %s (original %s) — idempotency violated", replayGrant.ID, grant.ID)
	}
	replaySums := readPostingForGrant(t, pool, f.tenantID, grant.ID, ledger.TxBonusGrant)
	if replaySums.transactionID != s.transactionID || replaySums.creditTotal != s.creditTotal {
		t.Fatalf("replay changed the posting: tx %s->%s, credits %d->%d",
			s.transactionID, replaySums.transactionID, s.creditTotal, replaySums.creditTotal)
	}
	assertReconciliationClean(t, pool, f.tenantID, "after deposit-sweep replay")
}

// runCashbackChainWithJurisdiction replays
// RunCashbackSchedulerForTenant's OWN body for the campaign's first
// elapsed window against the real production helpers, substituting only
// the jurisdiction code (see this file's header). Returns the Grant, the
// gate outcome and the NetLossAmount the real ledger read produced.
func runCashbackChain(t *testing.T, pool *db.Pool, f lifecycleFixture, asOf time.Time) (Grant, GateOutcome, *big.Int) {
	t.Helper()
	var grant Grant
	var outcome GateOutcome
	var observedNetLoss *big.Int
	issued := 0
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		campaigns, err := listCashbackMatchableCampaigns(ctx, tx, f.tenantID)
		if err != nil {
			return err
		}
		if len(campaigns) != 1 {
			return fmt.Errorf("expected exactly one cashback campaign, got %d", len(campaigns))
		}
		c := campaigns[0]
		exp, err := lookupDecimalExponent(ctx, tx, c.OfferVersion.RewardAssetCode)
		if err != nil {
			return err
		}
		windowStart := c.WindowAnchor
		windowEnd := windowStart.Add(time.Duration(c.WindowSeconds) * time.Second)
		if !windowEnd.Before(asOf) {
			return fmt.Errorf("fixture error: window has not elapsed against asOf")
		}
		candidates, err := cashbackCandidatesAndNetLoss(ctx, tx, f.tenantID, c.BrandID, c.OfferVersion.RewardAssetCode, windowStart, windowEnd)
		if err != nil {
			return err
		}
		netLoss, ok := candidates[f.playerID]
		if !ok {
			return fmt.Errorf("the net-loss query did not find the seeded player at all")
		}
		observedNetLoss = netLoss

		rateBP, capAmount, err := parsePercentageRewardCalculation(c.OfferVersion.RewardCalculation)
		if err != nil {
			return err
		}
		walletID, err := resolvePlayerWallet(ctx, tx, f.tenantID, f.playerID, c.OfferVersion.RewardAssetCode)
		if err != nil {
			return err
		}
		brandID, err := playerBrandID(ctx, tx, f.tenantID, f.playerID)
		if err != nil {
			return err
		}
		triggerRef := fmt.Sprintf("cashback:%s:%s:%s:%s", c.CampaignID, f.playerID,
			windowStart.UTC().Format(time.RFC3339), windowEnd.UTC().Format(time.RFC3339))
		g := Grant{
			TenantID: f.tenantID, BrandID: brandID, PlayerAccountID: f.playerID, WalletID: walletID,
			CampaignID: c.CampaignID, CampaignVersionID: c.CampaignVersionID, OfferID: c.OfferID, OfferVersionID: c.OfferVersion.ID,
			AssetCode: c.OfferVersion.RewardAssetCode, DecimalExponent: exp,
			FundingSource: c.OfferVersion.FundingSource, FulfillmentDestination: c.OfferVersion.FulfillmentDestination,
			FulfillmentOwner: c.FulfillmentOwner, TriggerReference: triggerRef,
			CreatedByActorType: ActorSystem,
		}
		issued++
		grant, outcome = forceIssueAndActivateCashbackForTest(t, ctx, tx, CashbackParams{
			Grant: g, NetLossAmount: netLoss, RateBP: rateBP, CapAmount: capAmount,
			ActorType: ActorSystem, ActorID: uuid.Nil,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("cashback scheduler chain: %v", err)
	}
	if issued != 1 {
		t.Fatalf("expected exactly one issuance attempt, got %d", issued)
	}
	return grant, outcome, observedNetLoss
}

// TestLFCert_CashbackSchedulerPosting_B1AndNetLossArithmetic is the
// cashback half of the same proof, driving the scheduler's own
// campaign-resolution and NetLossAmount ledger read (the exact query
// ledger-accounting-model.md §7.18.4 item 4 specifies) before posting.
func TestLFCert_CashbackSchedulerPosting_B1AndNetLossArithmetic(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	seedCashbackOffer(t, pool, f, 1000, 86400) // 10% cashback, 1-day window
	seedCasinoBetForCashback(t, pool, f, 1000) // 1000 staked, no win -> net loss 1000

	asOf := time.Now().UTC().Add(48 * time.Hour)
	grant, outcome, observedNetLoss := runCashbackChain(t, pool, f, asOf)

	if !outcome.Allowed {
		t.Fatalf("the full T.1 gate chain denied the cashback activation (denied_by=%s code=%s)", outcome.DeniedBy, outcome.Code)
	}
	if observedNetLoss == nil || observedNetLoss.Int64() != 1000 {
		t.Fatalf("NetLossAmount from the real ledger read = %v, want 1000", observedNetLoss)
	}
	// Cashback self-completes (C2): the Grant must already be completed.
	if grant.Status != GrantCompleted {
		t.Fatalf("grant status = %s, want %s", grant.Status, GrantCompleted)
	}

	const wantReward = int64(100) // 1000 net loss * 10% = 100, exact, no rounding tie

	s := readPostingForGrant(t, pool, f.tenantID, grant.ID, ledger.TxBonusGrant)
	assertBalanced(t, s, "cashback bonus_grant posting")
	if got := s.byAccountType[string(ledger.AccountPlayerBonus)]; got != wantReward {
		t.Fatalf("player_bonus net credit = %d, want %d", got, wantReward)
	}
	if got := s.byAccountType[string(ledger.AccountPromoLiability)]; got != -wantReward {
		t.Fatalf("promo_liability net = %d, want %d (the Rule B2 mirror debit)", got, -wantReward)
	}

	assertReconciliationClean(t, pool, f.tenantID, "after cashback bonus_grant posting")

	// DUPLICATE / REPLAY — the coverage-floor category CLAUDE.md names
	// FIRST for financial functionality, and the one no test in this
	// Wave covered for the cashback scheduler specifically (the deposit
	// sweep had TestRunDepositSweepForTenant_MatchesAndAdvancesWatermark;
	// the cashback scheduler had no equivalent). Re-running the identical
	// window recomputes the identical deterministic trigger_reference,
	// which the DB-enforced UNIQUE (tenant_id, campaign_id,
	// offer_version_id, player_account_id, trigger_reference) constraint
	// must resolve to the existing Grant — never a second cashback payout
	// for the same window.
	replayGrant, _, replayNetLoss := runCashbackChain(t, pool, f, asOf)
	if replayGrant.ID != grant.ID {
		t.Fatalf("replaying the same cashback window produced a DIFFERENT grant %s (original %s) — a double payout", replayGrant.ID, grant.ID)
	}
	if replayNetLoss == nil || replayNetLoss.Int64() != 1000 {
		t.Fatalf("replay NetLossAmount = %v, want the same 1000 (the window's ledger read must be deterministic)", replayNetLoss)
	}
	replaySums := readPostingForGrant(t, pool, f.tenantID, grant.ID, ledger.TxBonusGrant)
	if replaySums.transactionID != s.transactionID || replaySums.creditTotal != s.creditTotal {
		t.Fatalf("replay changed the posting: tx %s->%s, credits %d->%d",
			s.transactionID, replaySums.transactionID, s.creditTotal, replaySums.creditTotal)
	}
	assertReconciliationClean(t, pool, f.tenantID, "after cashback replay")
}

// TestLFCert_DepositSweepMalformedRewardCalculation_SkipsAuditsAndContinues
// covers the PARTIAL-FAILURE category of CLAUDE.md's financial test
// floor for the deposit sweep: a single Offer whose reward_calculation
// does not parse must not abort the tenant's whole tick (which would
// roll back every other Offer's issuance and, worse, the watermark
// advance — permanently stalling that tenant's sweep behind one
// mistyped Offer). It must be skipped, audited, and the tick must
// continue and still advance its cursor.
func TestLFCert_DepositSweepMalformedRewardCalculation_SkipsAuditsAndContinues(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	// Authored malformed from the start: bonus_offer_versions rows are
	// immutable by design, so the defect this models is an OfferVersion
	// PUBLISHED with a reward_calculation that parses as JSON (the
	// column's own jsonb check passes) but carries no positive rate_bp —
	// exactly the authoring-time hole ADR 0040 D3 item 1 routes to
	// `bonus-engine` to close at the write path. This asserts the
	// runtime fail-safe underneath it.
	co := seedDepositMatchableOfferWithReward(t, pool, f, `{"not_a_rate":1}`)
	depositTxID := seedRawDeposit(t, pool, f, 10000, "card")

	var outcome DepositSweepOutcome
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var runErr error
		outcome, runErr = RunDepositSweepForTenant(ctx, tx, f.tenantID, uuid.Nil)
		return runErr
	}); err != nil {
		t.Fatalf("the sweep aborted on a malformed reward_calculation instead of skipping it: %v", err)
	}
	if outcome.EventsProcessed != 1 {
		t.Fatalf("events processed = %d, want 1 (the tick must not abort)", outcome.EventsProcessed)
	}
	if outcome.GrantsIssued != 0 || outcome.GrantsDenied != 0 {
		t.Fatalf("no grant may be issued or denied from an unparseable reward: %+v", outcome)
	}
	if len(outcome.Skipped) != 1 {
		t.Fatalf("skipped reasons = %v, want exactly one", outcome.Skipped)
	}

	// The skip must be VISIBLE to an operator, not silent.
	var auditCount int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'bonus_deposit_sweep.malformed_reward_calculation_skipped' AND target_id = $2`,
			f.tenantID, co.offerVersionID.String(),
		).Scan(&auditCount)
	}); err != nil {
		t.Fatalf("read audit log: %v", err)
	}
	if auditCount != 1 {
		t.Fatalf("expected exactly one malformed_reward_calculation_skipped audit row, got %d", auditCount)
	}

	// The watermark must still have advanced past the deposit, so one
	// mistyped Offer cannot stall the tenant's sweep forever.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := getDepositSweepWatermark(ctx, tx, f.tenantID, DepositSweepConsumerName)
		if err != nil {
			return err
		}
		if w.LastProcessedLedgerTransactionID == nil || *w.LastProcessedLedgerTransactionID != depositTxID {
			return fmt.Errorf("watermark = %v, want it advanced to the processed deposit %s", w.LastProcessedLedgerTransactionID, depositTxID)
		}
		return nil
	}); err != nil {
		t.Fatalf("%v", err)
	}

	// And nothing at all was posted.
	var postings int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'bonus_grant'`, f.tenantID,
		).Scan(&postings)
	}); err != nil {
		t.Fatalf("count bonus_grant postings: %v", err)
	}
	if postings != 0 {
		t.Fatalf("expected zero bonus_grant postings from an unparseable reward, got %d", postings)
	}
}

// TestLFCert_SchedulerAsOfComesFromTheDatabaseClock guards
// DR-4HB1W3-LF-01 (Stage 4H-B1 Wave 3 Phase 11): the instant the
// cashback scheduler and the expiry sweep compare their windows against
// must be the DATABASE's own clock_timestamp(), read inside the same
// transaction, never Go's time.Now() and never now().
//
// The half this test CAN prove deterministically is the "never now()"
// half, and it is a real regression guard: now() is pinned to
// transaction start, so a future change replacing clock_timestamp() with
// now() makes the strict inequality below fail. The "never time.Now()"
// half is not testable on a single-host rig (the Go process and Postgres
// share one clock there, so both implementations agree exactly) — see
// dbClockTimestamp's own doc comment, which states that limitation
// rather than implying a proof that does not exist.
func TestLFCert_SchedulerAsOfComesFromTheDatabaseClock(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var txStart time.Time
		if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&txStart); err != nil {
			return err
		}
		// Force measurable wall-clock progress WITHIN the transaction, so
		// a now()-based implementation is distinguishable from a
		// clock_timestamp()-based one.
		if _, err := tx.Exec(ctx, `SELECT pg_sleep(0.05)`); err != nil {
			return err
		}
		asOf, err := dbClockTimestamp(ctx, tx)
		if err != nil {
			return err
		}
		if !asOf.After(txStart) {
			return fmt.Errorf("asOf %s is not strictly after the transaction's own now() %s — "+
				"the scheduler clock is pinned to transaction start (now()), which doc 10 §2 forbids", asOf, txStart)
		}
		var txEnd time.Time
		if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&txEnd); err != nil {
			return err
		}
		if asOf.After(txEnd.UTC()) {
			return fmt.Errorf("asOf %s is after a later clock_timestamp() read %s — not a database clock read at all", asOf, txEnd)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("%v", err)
	}
}

// TestLFCert_ExpirySweepWithDatabaseClock_TerminatesExpiredGrant proves
// the production composition DR-4HB1W3-LF-01 introduces — asOf sourced
// from dbClockTimestamp, inside the same tenant transaction as
// RunExpirySweepForTenant — still terminates an already-expired Grant.
// (RunExpirySweep/RunCashbackSweep, the pool-level entry points this
// phase changed, are deliberately NOT called directly: they iterate
// EVERY active tenant in the shared dev database and would mutate other
// suites' Grants. This asserts the same composition, tenant-scoped.)
func TestLFCert_ExpirySweepWithDatabaseClock_TerminatesExpiredGrant(t *testing.T) {
	pool := testPool(t)
	f := seedLifecycleFixture(t, pool)
	co := seedCampaignOffer(t, pool, f.tenantID, f.brandID, f.staffID)

	var grantID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		g := newTestOfferGrant(f, co, "lfcert-expiry-dbclock")
		result := forceIssueAndActivateGrantForTest(t, ctx, tx, g, ActivateGrantParams{
			Amount: big.NewInt(1000), ActorType: ActorSystem,
			WageringTimeLimit: durationPtr(10 * time.Millisecond),
		})
		grantID = result.ID
		return nil
	})
	if err != nil {
		t.Fatalf("seed expiring grant: %v", err)
	}
	time.Sleep(40 * time.Millisecond)

	var outcome ExpirySweepOutcome
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		asOf, err := dbClockTimestamp(ctx, tx)
		if err != nil {
			return err
		}
		outcome, err = RunExpirySweepForTenant(ctx, tx, f.tenantID, uuid.Nil, asOf)
		return err
	})
	if err != nil {
		t.Fatalf("expiry sweep with database clock: %v", err)
	}
	if outcome.GrantsTerminated != 1 {
		t.Fatalf("expected exactly one terminated grant, got %+v", outcome)
	}
	if status := readGrantStatus(t, pool, f.tenantID, grantID); status != GrantExpired {
		t.Fatalf("grant status = %s, want %s", status, GrantExpired)
	}
	// A terminal write-down of a still-positive player_bonus balance is
	// itself a posting: assert B1 on it too, so the expiry path is
	// certified on a real posting rather than by inspection.
	s := readPostingForGrant(t, pool, f.tenantID, grantID, ledger.TxBonusForfeiture)
	assertBalanced(t, s, "expiry-sweep terminal write-down posting")
	if got := s.byAccountType[string(ledger.AccountPlayerBonus)]; got != -1000 {
		t.Fatalf("player_bonus net = %d, want -1000 (the full granted amount written down)", got)
	}
	if got := s.byAccountType[string(ledger.AccountPromoLiability)]; got != 1000 {
		t.Fatalf("promo_liability net = %d, want 1000 (the Rule B2 mirror credit)", got)
	}
	assertReconciliationClean(t, pool, f.tenantID, "after expiry-sweep terminal write-down")
}

func durationPtr(d time.Duration) *time.Duration { return &d }

// seedDepositMatchableOfferWithReward mirrors seedDepositMatchableOffer
// exactly but lets the caller author the OfferVersion's own
// reward_calculation blob (bonus_offer_versions rows are immutable once
// written, so a malformed one can only be modelled by authoring it that
// way).
func seedDepositMatchableOfferWithReward(t *testing.T, pool *db.Pool, f lifecycleFixture, rewardCalculation string) campaignOffer {
	t.Helper()
	var co campaignOffer
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		c, err := CreateCampaign(ctx, tx, Campaign{TenantID: f.tenantID, BrandID: &f.brandID, Status: CampaignActive, CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID})
		if err != nil {
			return err
		}
		co.campaignID = c.ID
		v, err := CreateCampaignVersion(ctx, tx, CampaignVersion{TenantID: f.tenantID, CampaignID: c.ID, VersionNumber: 1, Name: "V1", CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID})
		if err != nil {
			return err
		}
		co.campaignVersionID = v.ID
		if _, err := SetCampaignCurrentVersion(ctx, tx, f.tenantID, c.ID, v.ID); err != nil {
			return err
		}
		o, err := CreateOffer(ctx, tx, Offer{
			TenantID: f.tenantID, BrandID: &f.brandID, CampaignID: c.ID, CampaignVersionID: v.ID,
			GrantPolicy: GrantPolicyAutoIssue, Status: OfferActive, CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID,
		})
		if err != nil {
			return err
		}
		co.offerID = o.ID
		ov, err := CreateOfferVersion(ctx, tx, OfferVersion{
			TenantID: f.tenantID, OfferID: o.ID, VersionNumber: 1, RewardKind: RewardPercentageWithCap, RewardAssetCode: f.assetCode,
			RewardCalculation:      []byte(rewardCalculation),
			FulfillmentDestination: FulfillmentIntoPlatformWallet, FundingSource: "operator",
			CreatedByActorType: ActorStaff, CreatedByActorID: f.staffID,
		})
		if err != nil {
			return err
		}
		co.offerVersionID = ov.ID
		_, err = SetOfferCurrentVersion(ctx, tx, f.tenantID, o.ID, ov.ID)
		return err
	})
	if err != nil {
		t.Fatalf("seed deposit-matchable offer with custom reward: %v", err)
	}
	return co
}
