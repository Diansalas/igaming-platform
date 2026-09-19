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
// THE ONE SUBSTITUTION, STATED PLAINLY RATHER THAN HIDDEN: the sweeps
// pass JurisdictionCode: "" (RunDepositSweepForTenant's own disclosed
// limitation — this platform has no per-player jurisdiction resolver
// anywhere, the same pre-existing gap already disclosed against casino's
// real-money bet path), which AssetAuthorization correctly treats as an
// immediate denial (ADR 0037 §C.2). These tests pass the fixture's OWN
// real, fully dual-control-authorized jurisdiction code instead
// (seedLifecycleFixture → authorizeFreshAssetForBonusWagering: a real
// `jurisdictions` row plus a real ScopeTenant/ScopeJurisdiction
// `assetregistry.AuthorizeScope` chain). NOTHING about the gate itself
// is stubbed, skipped, short-circuited or weakened: the full T.1 chain
// (AssetAuthorization → RG → Risk) runs unmodified and must genuinely
// ALLOW for these tests to reach a posting at all. What is not proven
// here — and is not provable until the platform-wide jurisdiction-
// resolution gap closes — is the single link "a sweep supplies a real
// jurisdiction code". Everything downstream of that link, which is where
// all of the arithmetic and all of the double-entry lives, IS proven.
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
func runDepositSweepChainWithJurisdiction(t *testing.T, pool *db.Pool, f lifecycleFixture, jurisdictionCode string, ignoreCursor bool) (Grant, GateOutcome) {
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
				grant, outcome, err = IssueAndActivateDepositBonus(ctx, tx, DepositBonusParams{
					Grant: g, DepositAmount: event.Amount, RateBP: rateBP, CapAmount: capAmount,
					MinQualifying: c.OfferVersion.MinQualifyingAmount, MaxQualifying: c.OfferVersion.MaxQualifyingAmount,
					JurisdictionCode: jurisdictionCode, ActorType: ActorSystem, ActorID: uuid.Nil,
					WageringTimeLimit: c.OfferVersion.WageringTimeLimit,
				})
				if err != nil {
					return err
				}
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

	grant, outcome := runDepositSweepChainWithJurisdiction(t, pool, f, f.jurisdictionCode, false)
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
	replayGrant, _ := runDepositSweepChainWithJurisdiction(t, pool, f, f.jurisdictionCode, true)
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
		grant, outcome, err = IssueAndActivateCashback(ctx, tx, CashbackParams{
			Grant: g, NetLossAmount: netLoss, RateBP: rateBP, CapAmount: capAmount,
			JurisdictionCode: f.jurisdictionCode, ActorType: ActorSystem, ActorID: uuid.Nil,
		})
		return err
	})
	if err != nil {
		t.Fatalf("cashback scheduler chain: %v", err)
	}
	if issued != 1 {
		t.Fatalf("expected exactly one issuance attempt, got %d", issued)
	}
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
}
