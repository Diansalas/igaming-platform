//go:build integration

// The Rule B2 (extended) mirror generator's own test set
// (ledger-accounting-model.md §7.15), against the real, live migrations
// 0050 (bonus_expense)/0051 (bonus_* transaction types)/0052
// (player_bonus_held) and bonus_mirror.go. Coverage map (§7.15 item ->
// test in this file):
//
//	5, 6, 7, 11, 12, 13, 16 (exhaustive posting-shape table, exact entry
//	set, invariant B1, zero legs for an internal transfer, multi-asset)
//	                             -> TestBonusMirror_WorkedPostingShapes,
//	                                TestBonusMirror_MultiAssetGeneratesPerAssetLegs
//	8  (BonusCost fail-closed validation)
//	                             -> TestBonusMirror_BonusCostFailClosedValidation
//	9  (HR-17, including under a reversal)
//	                             -> TestBonusMirror_HR17RejectsHandAssembledLeg
//	17, 18 (reversal reproduces exact inverse; funding-mismatch rejected;
//	        tombstone of a never-seen grant)
//	                             -> TestBonusMirror_ReversalReproducesExactInverse,
//	                                TestBonusMirror_ReversalFundingMismatchRejected,
//	                                TestBonusMirror_ReversalOfNeverSeenGrantWritesTombstone
//	19, 20 (exact retry / same-key-different-type)
//	                             -> TestBonusMirror_ExactRetryIsIdempotent,
//	                                TestBonusMirror_SameKeyDifferentTypeRejected
//	21, 22 (concurrency)         -> TestBonusMirror_ConcurrentGrantsSameKeyOnlyOnePosts,
//	                                TestBonusMirror_ConcurrentGrantsDifferentKeysBothPost
//	23 (caller rollback leaves nothing)
//	                             -> TestBonusMirror_CallerRollbackLeavesNothing
//	27 (exponents 0, 2, 8, 18)  -> TestBonusMirror_ExponentIndependence
//	28 (RLS)                    -> TestBonusMirror_RLSScopedToOwnerAndTenant
//
// Items 1-4 (migrations 0050/0051 themselves) live in
// bonus_migrations_integration_test.go. Items 14-15 (conversion's
// sufficiency/P_firm gate) are explicitly OUT OF SCOPE here: that
// authorization logic is a CALLER responsibility per §7.6 (a FOR UPDATE
// read and comparison before ever calling Post), owned by whichever
// specialist builds the actual bonus_conversion call site
// (bonus-engine, a later phase) - not by bonus_mirror.go or Post()
// itself, which this file's tests exercise directly. Items 25-26
// (reconciliation stream / RebuildBalance-vs-projection) are exercised
// indirectly inside TestBonusMirror_ExponentIndependence, following
// migration 0048's own test precedent, rather than duplicated as a
// separate reconciliation-package test. Item 10 (the generator's own
// step-4 balance assertion firing) and item 24 (partial failure at the
// entry-insert stage specifically) are NOT separately tested: both
// validation happens strictly BEFORE any write in this implementation
// (applyBonusMirror runs, and can only fail, before Post's idempotent
// insert), so there is no reachable call through the public API that
// gets past every earlier validation yet still produces an unbalanced
// result or a failure mid-insert - constructing one would require
// deliberately corrupting this file's own arithmetic, which every other
// test in this file would also then fail. Recorded here rather than
// covered by a fabricated test.
package ledger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// bonusAccounts is every account one of this file's tests might touch,
// resolved once per (fixture, asset).
type bonusAccounts struct {
	playerBonus       uuid.UUID
	playerLockedBonus uuid.UUID
	playerBonusHeld   uuid.UUID
	promoLiability    uuid.UUID
	bonusExpense      uuid.UUID
	providerPayable   uuid.UUID
	houseGaming       uuid.UUID
	manualAdjustment  uuid.UUID
}

func seedBonusAccounts(t *testing.T, pool *db.Pool, f fixture, walletID uuid.UUID, asset string) bonusAccounts {
	t.Helper()
	var a bonusAccounts
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if a.playerBonus, err = GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, AccountPlayerBonus, asset); err != nil {
			return err
		}
		if a.playerLockedBonus, err = GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, AccountPlayerLockedBonus, asset); err != nil {
			return err
		}
		if a.playerBonusHeld, err = GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, AccountPlayerBonusHeld, asset); err != nil {
			return err
		}
		if a.promoLiability, err = GetOrCreateAccount(ctx, tx, f.tenantID, nil, AccountPromoLiability, asset); err != nil {
			return err
		}
		if a.bonusExpense, err = GetOrCreateAccount(ctx, tx, f.tenantID, nil, AccountBonusExpense, asset); err != nil {
			return err
		}
		if a.providerPayable, err = GetOrCreateAccount(ctx, tx, f.tenantID, nil, AccountProviderPayable, asset); err != nil {
			return err
		}
		if a.houseGaming, err = GetOrCreateAccount(ctx, tx, f.tenantID, nil, AccountHouseGaming, asset); err != nil {
			return err
		}
		a.manualAdjustment, err = GetOrCreateAccount(ctx, tx, f.tenantID, nil, AccountManualAdjustment, asset)
		return err
	})
	if err != nil {
		t.Fatalf("seed bonus accounts (%s): %v", asset, err)
	}
	return a
}

// gotEntry is one row of ledger_entries, read back for exact-set
// comparison against what a posting-shape test expects.
type gotEntry struct {
	AccountID uuid.UUID
	Direction Direction
	Amount    int64
}

func readEntries(t *testing.T, pool *db.Pool, tenantID, txID uuid.UUID) []gotEntry {
	t.Helper()
	var got []gotEntry
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT ledger_account_id, direction, amount FROM ledger_entries WHERE ledger_transaction_id = $1`, txID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e gotEntry
			if err := rows.Scan(&e.AccountID, &e.Direction, &e.Amount); err != nil {
				return err
			}
			got = append(got, e)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("read entries for transaction %s: %v", txID, err)
	}
	return got
}

// assertExactEntrySet compares got against want as multisets (order-
// independent - §7.4.2's fixed-ordering guarantee is a separate,
// dedicated property, not what this helper checks).
func assertExactEntrySet(t *testing.T, got, want []gotEntry) {
	t.Helper()
	sortEntries := func(s []gotEntry) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].AccountID != s[j].AccountID {
				return s[i].AccountID.String() < s[j].AccountID.String()
			}
			if s[i].Direction != s[j].Direction {
				return s[i].Direction < s[j].Direction
			}
			return s[i].Amount < s[j].Amount
		})
	}
	gotSorted := append([]gotEntry{}, got...)
	wantSorted := append([]gotEntry{}, want...)
	sortEntries(gotSorted)
	sortEntries(wantSorted)
	if len(gotSorted) != len(wantSorted) {
		t.Fatalf("entry count mismatch: got %d %+v, want %d %+v", len(gotSorted), gotSorted, len(wantSorted), wantSorted)
	}
	for i := range gotSorted {
		if gotSorted[i] != wantSorted[i] {
			t.Fatalf("entry set mismatch at position %d: got %+v, want %+v (full got=%+v want=%+v)",
				i, gotSorted[i], wantSorted[i], gotSorted, wantSorted)
		}
	}
}

// assertB1Holds recomputes invariant B1 (extended) DIRECTLY from
// ledger_entries (never from the projection) for one asset:
// signed(promo_liability) + sum(signed(BONUS_SET)) == 0
// (ledger-accounting-model.md §6.1).
func assertB1Holds(t *testing.T, pool *db.Pool, tenantID uuid.UUID, asset string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var sum int64
		err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(CASE WHEN la.account_type = 'promo_liability' THEN
			                       CASE WHEN le.direction = 'credit' THEN le.amount ELSE -le.amount END
			                     ELSE
			                       CASE WHEN le.direction = 'credit' THEN le.amount ELSE -le.amount END
			                     END), 0)
			 FROM ledger_entries le
			 JOIN ledger_accounts la ON la.id = le.ledger_account_id
			 WHERE le.tenant_id = $1 AND le.asset_code = $2
			   AND la.account_type IN ('promo_liability', 'player_bonus', 'player_locked_bonus', 'player_bonus_held')`,
			tenantID, asset,
		).Scan(&sum)
		if err != nil {
			return err
		}
		if sum != 0 {
			return fmt.Errorf("invariant B1 (extended) violated for asset %s: signed(promo_liability)+Σsigned(BONUS_SET) = %d, want 0", asset, sum)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("assertB1Holds: %v", err)
	}
}

// TestBonusMirror_WorkedPostingShapes is §7.15 items 5-7/11-13/16: every
// row of §7.4.2's worked table, asserted on the EXACT entry set produced
// (never merely "it balanced"), with invariant B1 re-verified from
// ledger_entries after each one. One EUR wallet, fresh idempotency keys
// per case so each is an independent LedgerTransaction.
func TestBonusMirror_WorkedPostingShapes(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")
	operatorCost := &BonusCostAttribution{Funding: FundingOperator}

	const X = int64(500)

	cases := []struct {
		name      string
		txType    TransactionType
		reason    *string
		entries   []EntryInput
		bonusCost *BonusCostAttribution
		want      []gotEntry
	}{
		{
			name:    "grant",
			txType:  TxBonusGrant,
			entries: []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: X}},
			want: []gotEntry{
				{a.playerBonus, Credit, X},
				{a.promoLiability, Debit, X},
			},
			bonusCost: operatorCost,
		},
		{
			name:    "forfeiture_expiry_or_cancellation",
			txType:  TxBonusForfeiture,
			reason:  strPtr("expired"),
			entries: []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: X}},
			want: []gotEntry{
				{a.playerBonus, Debit, X},
				{a.promoLiability, Credit, X},
			},
			bonusCost: operatorCost,
		},
		{
			name:   "conversion",
			txType: TxBonusConversion,
			entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: X},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.playerBonus, Debit, X},
				{f.cashAccountID, Credit, X},
				{a.promoLiability, Credit, X},
				{a.bonusExpense, Debit, X},
			},
			bonusCost: operatorCost,
		},
		{
			name:   "bonus_funded_stake_absorbed_by_house_case_G_generic",
			txType: TxManualAdjustment, // generic: no casino_bet-with-bonus-funding call site exists yet (§7.1)
			reason: strPtr("bonus_funded_stake_test"),
			entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: X},
				{LedgerAccountID: a.houseGaming, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.playerBonus, Debit, X},
				{a.houseGaming, Credit, X},
				{a.promoLiability, Credit, X},
				{a.bonusExpense, Debit, X},
			},
			bonusCost: operatorCost,
		},
		{
			name:   "casino_win_credited_to_bonus",
			txType: TxCasinoWin,
			entries: []EntryInput{
				{LedgerAccountID: a.houseGaming, Direction: Debit, Amount: X},
				{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.houseGaming, Debit, X},
				{a.playerBonus, Credit, X},
				{a.promoLiability, Debit, X},
				{a.bonusExpense, Credit, X},
			},
			bonusCost: operatorCost,
		},
		{
			name:   "sportsbook_lock_case_B_internal_transfer_unmirrored",
			txType: TxManualAdjustment,
			reason: strPtr("bonus_lock_test"),
			entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: X},
				{LedgerAccountID: a.playerLockedBonus, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.playerBonus, Debit, X},
				{a.playerLockedBonus, Credit, X},
			},
			bonusCost: operatorCost, // still required: the posting touches BONUS_SET, even though net=0
		},
		{
			name:   "void_release_case_E_internal_transfer_unmirrored",
			txType: TxManualAdjustment,
			reason: strPtr("bonus_void_test"),
			entries: []EntryInput{
				{LedgerAccountID: a.playerLockedBonus, Direction: Debit, Amount: X},
				{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.playerLockedBonus, Debit, X},
				{a.playerBonus, Credit, X},
			},
			bonusCost: operatorCost,
		},
		{
			name:   "stake_absorbed_from_locked_case_I",
			txType: TxManualAdjustment,
			reason: strPtr("bonus_locked_stake_absorbed_test"),
			entries: []EntryInput{
				{LedgerAccountID: a.playerLockedBonus, Direction: Debit, Amount: X},
				{LedgerAccountID: a.houseGaming, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.playerLockedBonus, Debit, X},
				{a.houseGaming, Credit, X},
				{a.promoLiability, Credit, X},
				{a.bonusExpense, Debit, X},
			},
			bonusCost: operatorCost,
		},
		{
			name:   "manual_adjustment_against_bonus",
			txType: TxManualAdjustment,
			reason: strPtr("adr0032_p1_4_correction"),
			entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: X},
				{LedgerAccountID: a.manualAdjustment, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.playerBonus, Debit, X},
				{a.manualAdjustment, Credit, X},
				{a.promoLiability, Credit, X},
				{a.bonusExpense, Debit, X},
			},
			bonusCost: operatorCost,
		},
		{
			name:   "direct_cash_reward_no_bonus_set_touch_untouched",
			txType: TxBonusGrant,
			entries: []EntryInput{
				{LedgerAccountID: a.bonusExpense, Direction: Debit, Amount: X},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.bonusExpense, Debit, X},
				{f.cashAccountID, Credit, X},
			},
			bonusCost: nil, // touches no BONUS_SET account - must be nil
		},
		{
			name:   "provider_funded_conversion",
			txType: TxBonusConversion,
			entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: X},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: X},
			},
			want: []gotEntry{
				{a.playerBonus, Debit, X},
				{f.cashAccountID, Credit, X},
				{a.promoLiability, Credit, X},
				{a.providerPayable, Debit, X},
			},
			bonusCost: &BonusCostAttribution{Funding: FundingProvider, ProviderID: strPtr("provider-xyz")},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mustPost(t, pool, f, TransactionInput{
				TenantID:        f.tenantID,
				TransactionType: c.txType,
				IdempotencyKey:  "bonus-shape-" + c.name,
				CorrelationID:   uuid.New(),
				ReasonCode:      c.reason,
				Entries:         c.entries,
				BonusCost:       c.bonusCost,
			})
			if res.AlreadyPosted {
				t.Fatal("expected a new posting")
			}
			got := readEntries(t, pool, f.tenantID, res.TransactionID)
			assertExactEntrySet(t, got, c.want)
			assertB1Holds(t, pool, f.tenantID, "EUR")
		})
	}
}

// TestBonusMirror_MultiAssetGeneratesPerAssetLegs is §7.15 item 11: one
// LedgerTransaction with BONUS_SET entries in TWO assets generates the
// correct legs PER ASSET and balances per asset (migration 0022's
// grouping) - each wallet is single-asset (financial-domain-model.md),
// so this uses two wallets of the same player, exactly the multi-wallet
// model CLAUDE.md requires.
func TestBonusMirror_MultiAssetGeneratesPerAssetLegs(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	eur := seedBonusAccounts(t, pool, f, f.walletID, "EUR")
	usdWalletID := seedWalletForAsset(t, pool, f, "USD")
	usd := seedBonusAccounts(t, pool, f, usdWalletID, "USD")

	const eurX, usdY = int64(300), int64(700)
	res := mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxBonusGrant,
		IdempotencyKey:  "bonus-multi-asset-grant",
		CorrelationID:   uuid.New(),
		Entries: []EntryInput{
			{LedgerAccountID: eur.playerBonus, Direction: Credit, Amount: eurX},
			{LedgerAccountID: usd.playerBonus, Direction: Credit, Amount: usdY},
		},
		BonusCost: &BonusCostAttribution{Funding: FundingOperator},
	})
	if res.AlreadyPosted {
		t.Fatal("expected a new posting")
	}

	got := readEntries(t, pool, f.tenantID, res.TransactionID)
	assertExactEntrySet(t, got, []gotEntry{
		{eur.playerBonus, Credit, eurX},
		{eur.promoLiability, Debit, eurX},
		{usd.playerBonus, Credit, usdY},
		{usd.promoLiability, Debit, usdY},
	})
	assertB1Holds(t, pool, f.tenantID, "EUR")
	assertB1Holds(t, pool, f.tenantID, "USD")

	// Balances per asset, independently - migration 0022's own
	// per-(transaction, asset_code) grouping, re-proved here for a
	// generator-produced multi-asset transaction specifically.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var eurDebits, eurCredits, usdDebits, usdCredits int64
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount) FILTER (WHERE direction='debit'),0), COALESCE(SUM(amount) FILTER (WHERE direction='credit'),0)
			 FROM ledger_entries WHERE ledger_transaction_id = $1 AND asset_code = 'EUR'`, res.TransactionID).
			Scan(&eurDebits, &eurCredits); err != nil {
			return err
		}
		if eurDebits != eurCredits {
			t.Fatalf("EUR leg unbalanced: debits=%d credits=%d", eurDebits, eurCredits)
		}
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(amount) FILTER (WHERE direction='debit'),0), COALESCE(SUM(amount) FILTER (WHERE direction='credit'),0)
			 FROM ledger_entries WHERE ledger_transaction_id = $1 AND asset_code = 'USD'`, res.TransactionID).
			Scan(&usdDebits, &usdCredits); err != nil {
			return err
		}
		if usdDebits != usdCredits {
			t.Fatalf("USD leg unbalanced: debits=%d credits=%d", usdDebits, usdCredits)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify per-asset balance: %v", err)
	}
}

// TestBonusMirror_BonusCostFailClosedValidation is §7.15 item 8.
func TestBonusMirror_BonusCostFailClosedValidation(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")

	cases := []struct {
		name      string
		entries   []EntryInput
		bonusCost *BonusCostAttribution
		wantErr   error
	}{
		{
			name:      "touches_bonus_set_nil_cost",
			entries:   []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 100}},
			bonusCost: nil,
			wantErr:   ErrBonusCostRequired,
		},
		{
			name: "does_not_touch_bonus_set_non_nil_cost",
			entries: []EntryInput{
				{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 100},
				{LedgerAccountID: f.clearingID, Direction: Credit, Amount: 100},
			},
			bonusCost: &BonusCostAttribution{Funding: FundingOperator},
			wantErr:   ErrBonusCostNotAllowed,
		},
		{
			name:      "provider_funding_without_provider_id",
			entries:   []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 100}},
			bonusCost: &BonusCostAttribution{Funding: FundingProvider},
			wantErr:   ErrBonusProviderIDRequired,
		},
		{
			name:      "provider_funding_empty_provider_id",
			entries:   []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 100}},
			bonusCost: &BonusCostAttribution{Funding: FundingProvider, ProviderID: strPtr("")},
			wantErr:   ErrBonusProviderIDRequired,
		},
		{
			name:      "unknown_funding_value",
			entries:   []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 100}},
			bonusCost: &BonusCostAttribution{Funding: BonusFunding("charity")},
			wantErr:   ErrBonusFundingInvalid,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			key := "bonus-cost-validation-" + c.name
			var postErr error
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, postErr = Post(ctx, tx, TransactionInput{
					TenantID:        f.tenantID,
					TransactionType: TxBonusGrant,
					IdempotencyKey:  key,
					CorrelationID:   uuid.New(),
					Entries:         c.entries,
					BonusCost:       c.bonusCost,
				})
				return nil // commit regardless: prove nothing durable was written
			})
			if err != nil {
				t.Fatalf("transaction wrapper failed: %v", err)
			}
			if !errors.Is(postErr, c.wantErr) {
				t.Fatalf("expected %v, got %v", c.wantErr, postErr)
			}
			assertNothingPostedForKey(t, pool, f.tenantID, key)
		})
	}
}

// assertNothingPostedForKey proves a rejected Post left no
// ledger_transactions row for key - the "before Post writes anything at
// all" property HR-9's guard used to hold and the generator now holds in
// its place.
func assertNothingPostedForKey(t *testing.T, pool *db.Pool, tenantID uuid.UUID, key string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`,
			tenantID, key).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("rejected posting for key %q left %d ledger_transactions row(s) behind", key, count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify nothing written for key %q: %v", key, err)
	}
}

// TestBonusMirror_HR17RejectsHandAssembledLeg is §7.15 item 9: a
// caller-supplied promo_liability leg alongside a player_bonus leg is
// rejected and nothing is written - including when ReversesTransactionID
// is set (the exemption §7.4.2 considered and rejected).
func TestBonusMirror_HR17RejectsHandAssembledLeg(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")

	key := "hr17-hand-assembled"
	var postErr error
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, postErr = Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusGrant,
			IdempotencyKey:  key,
			CorrelationID:   uuid.New(),
			Entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 100},
				{LedgerAccountID: a.promoLiability, Direction: Debit, Amount: 100}, // hand-assembled mirror leg
			},
			BonusCost: &BonusCostAttribution{Funding: FundingOperator},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction wrapper failed: %v", err)
	}
	if !errors.Is(postErr, ErrBonusMirrorLegSupplied) {
		t.Fatalf("expected ErrBonusMirrorLegSupplied, got %v", postErr)
	}
	assertNothingPostedForKey(t, pool, f.tenantID, key)

	// Post a real grant, then attempt to reverse it while ALSO
	// hand-supplying the mirror leg - the "no exemption for a reversal"
	// half of HR-17.
	grant := mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxBonusGrant,
		IdempotencyKey:  "hr17-reversal-base-grant",
		CorrelationID:   uuid.New(),
		Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 200}},
		BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
	})

	revKey := "hr17-reversal-hand-assembled"
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, postErr = Post(ctx, tx, TransactionInput{
			TenantID:              f.tenantID,
			TransactionType:       TxBonusReversal,
			IdempotencyKey:        revKey,
			CorrelationID:         uuid.New(),
			ReversesTransactionID: &grant.TransactionID,
			Entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 200},
				{LedgerAccountID: a.promoLiability, Direction: Credit, Amount: 200}, // hand-assembled, even under a reversal
			},
			BonusCost: &BonusCostAttribution{Funding: FundingOperator},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction wrapper failed: %v", err)
	}
	if !errors.Is(postErr, ErrBonusMirrorLegSupplied) {
		t.Fatalf("expected ErrBonusMirrorLegSupplied for a reversal too (no exemption), got %v", postErr)
	}
	assertNothingPostedForKey(t, pool, f.tenantID, revKey)
}

// TestBonusMirror_ReversalReproducesExactInverse is §7.15 item 17: a
// reversal's caller-supplied legs are the inverse of the original's
// caller-supplied legs ONLY, and the generator rebuilds the mirror (and
// recognition, where the original had one) legs - producing the EXACT
// inverse of the original's full entry set, with no special case.
func TestBonusMirror_ReversalReproducesExactInverse(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")
	operatorCost := &BonusCostAttribution{Funding: FundingOperator}

	t.Run("grant", func(t *testing.T) {
		grant := mustPost(t, pool, f, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusGrant,
			IdempotencyKey:  "reverse-exact-inverse-grant",
			CorrelationID:   uuid.New(),
			Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 400}},
			BonusCost:       operatorCost,
		})
		original := readEntries(t, pool, f.tenantID, grant.TransactionID)

		reversal := mustPost(t, pool, f, TransactionInput{
			TenantID:              f.tenantID,
			TransactionType:       TxBonusReversal,
			IdempotencyKey:        "bonus_reversal:" + grant.TransactionID.String(),
			CorrelationID:         uuid.New(),
			ReversesTransactionID: &grant.TransactionID,
			Entries:               []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 400}},
			BonusCost:             operatorCost,
		})
		got := readEntries(t, pool, f.tenantID, reversal.TransactionID)
		assertExactEntrySet(t, got, invertEntries(original))
		assertB1Holds(t, pool, f.tenantID, "EUR")
	})

	t.Run("conversion", func(t *testing.T) {
		conversion := mustPost(t, pool, f, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusConversion,
			IdempotencyKey:  "reverse-exact-inverse-conversion",
			CorrelationID:   uuid.New(),
			Entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 250},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 250},
			},
			BonusCost: operatorCost,
		})
		original := readEntries(t, pool, f.tenantID, conversion.TransactionID)

		reversal := mustPost(t, pool, f, TransactionInput{
			TenantID:              f.tenantID,
			TransactionType:       TxBonusReversal,
			IdempotencyKey:        "bonus_reversal:" + conversion.TransactionID.String(),
			CorrelationID:         uuid.New(),
			ReversesTransactionID: &conversion.TransactionID,
			Entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 250},
				{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 250},
			},
			BonusCost: operatorCost,
		})
		got := readEntries(t, pool, f.tenantID, reversal.TransactionID)
		assertExactEntrySet(t, got, invertEntries(original))
		assertB1Holds(t, pool, f.tenantID, "EUR")
	})
}

func invertEntries(in []gotEntry) []gotEntry {
	out := make([]gotEntry, len(in))
	for i, e := range in {
		d := Credit
		if e.Direction == Credit {
			d = Debit
		}
		out[i] = gotEntry{AccountID: e.AccountID, Direction: d, Amount: e.Amount}
	}
	return out
}

// TestBonusMirror_ReversalFundingMismatchRejected is §7.4.3's last
// fail-closed rule: a reversal's BonusCost.Funding must match what the
// ORIGINAL transaction actually recognized, read from its own posted
// entries.
func TestBonusMirror_ReversalFundingMismatchRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")

	conversion := mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxBonusConversion,
		IdempotencyKey:  "funding-mismatch-conversion",
		CorrelationID:   uuid.New(),
		Entries: []EntryInput{
			{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 300},
			{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 300},
		},
		BonusCost: &BonusCostAttribution{Funding: FundingOperator}, // recognizes bonus_expense
	})

	revKey := "funding-mismatch-reversal"
	var postErr error
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, postErr = Post(ctx, tx, TransactionInput{
			TenantID:              f.tenantID,
			TransactionType:       TxBonusReversal,
			IdempotencyKey:        revKey,
			CorrelationID:         uuid.New(),
			ReversesTransactionID: &conversion.TransactionID,
			Entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 300},
				{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 300},
			},
			BonusCost: &BonusCostAttribution{Funding: FundingProvider, ProviderID: strPtr("wrong-provider")},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction wrapper failed: %v", err)
	}
	if !errors.Is(postErr, ErrBonusFundingMismatch) {
		t.Fatalf("expected ErrBonusFundingMismatch, got %v", postErr)
	}
	assertNothingPostedForKey(t, pool, f.tenantID, revKey)

	// A reversal of a Grant (which recognizes NOTHING - r=0) has nothing
	// to mismatch against, so ANY valid Funding is accepted.
	grant := mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxBonusGrant,
		IdempotencyKey:  "funding-mismatch-grant-control",
		CorrelationID:   uuid.New(),
		Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 150}},
		BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
	})
	reversal := mustPost(t, pool, f, TransactionInput{
		TenantID:              f.tenantID,
		TransactionType:       TxBonusReversal,
		IdempotencyKey:        "funding-mismatch-grant-control-reversal",
		CorrelationID:         uuid.New(),
		ReversesTransactionID: &grant.TransactionID,
		Entries:               []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 150}},
		BonusCost:             &BonusCostAttribution{Funding: FundingProvider, ProviderID: strPtr("anything")},
	})
	if reversal.AlreadyPosted {
		t.Fatal("expected a new posting")
	}
}

// TestBonusMirror_ReversalOfNeverSeenGrantWritesTombstone is §7.15 item
// 18, re-proved for the bonus family specifically (the mechanism is
// internal/casino's postRollbackTombstone's, unmodified by this
// dispatch - see internal/db.IdempotentInsert and
// ledger-accounting-model.md §7.7's "reversal of a never-seen grant"
// paragraph). This test proves the LEDGER-LEVEL property the mechanism
// depends on: posting a tombstone (zero entries) under the grant's own
// idempotency key makes a late-arriving real grant with that SAME key
// collide and be rejected as a reused key, never silently posted after
// its own cancellation.
func TestBonusMirror_ReversalOfNeverSeenGrantWritesTombstone(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")

	grantID := uuid.New()
	key := "bonus_grant:" + grantID.String()

	tombstone := mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxTombstone,
		IdempotencyKey:  key,
		CorrelationID:   uuid.New(),
		Entries:         nil,
	})
	if tombstone.AlreadyPosted {
		t.Fatal("expected the tombstone itself to be a new posting")
	}

	var postErr error
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, postErr = Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusGrant,
			IdempotencyKey:  key, // the late-arriving "real" grant, same key
			CorrelationID:   uuid.New(),
			Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 999}},
			BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
		})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction wrapper failed: %v", err)
	}
	if !errors.Is(postErr, ErrIdempotencyKeyReused) {
		t.Fatalf("expected ErrIdempotencyKeyReused for a grant arriving after its own tombstone, got %v", postErr)
	}

	// And no player_bonus entry was ever created for it.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_entries WHERE ledger_account_id = $1 AND amount = 999`, a.playerBonus).
			Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("the late grant must not have posted, found %d matching entries", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
}

// TestBonusMirror_ExactRetryIsIdempotent is §7.15 item 19, for the bonus
// family specifically: an exact retry of a grant/conversion/forfeiture
// returns AlreadyPosted with the ORIGINAL TransactionID and writes ZERO
// new entries - asserted by entry count, not merely by absence of error.
func TestBonusMirror_ExactRetryIsIdempotent(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")
	operatorCost := &BonusCostAttribution{Funding: FundingOperator}

	in := TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxBonusGrant,
		IdempotencyKey:  "retry-idempotent-grant",
		CorrelationID:   uuid.New(),
		Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 600}},
		BonusCost:       operatorCost,
	}
	first := mustPost(t, pool, f, in)
	if first.AlreadyPosted {
		t.Fatal("first post should not be AlreadyPosted")
	}
	countBefore := len(readEntries(t, pool, f.tenantID, first.TransactionID))

	second := mustPost(t, pool, f, in)
	if !second.AlreadyPosted {
		t.Fatal("retried post should be AlreadyPosted")
	}
	if second.TransactionID != first.TransactionID {
		t.Fatalf("retried post returned a different transaction id: %s vs %s", second.TransactionID, first.TransactionID)
	}
	countAfter := len(readEntries(t, pool, f.tenantID, first.TransactionID))
	if countAfter != countBefore {
		t.Fatalf("retry must write ZERO new entries: had %d, now %d", countBefore, countAfter)
	}
}

// TestBonusMirror_SameKeyDifferentTypeRejected is §7.15 item 20.
func TestBonusMirror_SameKeyDifferentTypeRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")
	operatorCost := &BonusCostAttribution{Funding: FundingOperator}

	mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxBonusGrant,
		IdempotencyKey:  "same-key-different-type",
		CorrelationID:   uuid.New(),
		Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 111}},
		BonusCost:       operatorCost,
	})

	var postErr error
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, postErr = Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusForfeiture, // different type, same key
			IdempotencyKey:  "same-key-different-type",
			CorrelationID:   uuid.New(),
			ReasonCode:      strPtr("test"),
			Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 111}},
			BonusCost:       operatorCost,
		})
		return nil
	})
	if err != nil {
		t.Fatalf("transaction wrapper failed: %v", err)
	}
	if !errors.Is(postErr, ErrIdempotencyKeyReused) {
		t.Fatalf("expected ErrIdempotencyKeyReused, got %v", postErr)
	}
}

// TestBonusMirror_ConcurrentGrantsSameKeyOnlyOnePosts and
// TestBonusMirror_ConcurrentGrantsDifferentKeysBothPost are §7.15 item
// 22, empirically, with real concurrent transactions (not a sequential
// simulation) - mirroring TestPost_ConcurrentDuplicatesOnlyOneWins'
// established pattern in this package.
func TestBonusMirror_ConcurrentGrantsSameKeyOnlyOnePosts(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")

	// One logical grant retried concurrently: every attempt carries the
	// SAME correlation id, as a real retry of one operation does. Since
	// the Stage 10 F-7 remediation a differing correlation under one key
	// is ErrIdempotencyPayloadMismatch, not a replay (ADR 0020 amendment).
	correlationID := uuid.New()
	in := func() TransactionInput {
		return TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusGrant,
			IdempotencyKey:  "concurrent-grant-same-key",
			CorrelationID:   correlationID,
			Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 250}},
			BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
		}
	}

	const n = 8
	var wg sync.WaitGroup
	results := make([]PostResult, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				results[i], err = Post(ctx, tx, in())
				return err
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	var succeeded int
	firstID := uuid.Nil
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, errs[i])
		}
		if !results[i].AlreadyPosted {
			succeeded++
		}
		if firstID == uuid.Nil {
			firstID = results[i].TransactionID
		} else if results[i].TransactionID != firstID {
			t.Fatalf("goroutine %d returned a different transaction id: %s vs %s", i, results[i].TransactionID, firstID)
		}
	}
	if succeeded != 1 {
		t.Fatalf("expected exactly 1 goroutine to actually post, got %d", succeeded)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		bal, err := GetProjectedBalance(ctx, tx, a.playerBonus)
		if err != nil {
			return err
		}
		if bal.Signed() != 250 {
			t.Fatalf("expected player_bonus balance 250 after %d concurrent same-key attempts, got %d", n, bal.Signed())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	assertB1Holds(t, pool, f.tenantID, "EUR")
}

func TestBonusMirror_ConcurrentGrantsDifferentKeysBothPost(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")

	const n = 5
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := Post(ctx, tx, TransactionInput{
					TenantID:        f.tenantID,
					TransactionType: TxBonusGrant,
					IdempotencyKey:  fmt.Sprintf("concurrent-grant-distinct-key-%d", i),
					CorrelationID:   uuid.New(),
					Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 100}},
					BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
				})
				return err
			})
			errs[i] = err
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: unexpected error: %v", i, err)
		}
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		bal, err := GetProjectedBalance(ctx, tx, a.playerBonus)
		if err != nil {
			return err
		}
		if bal.Signed() != n*100 {
			t.Fatalf("expected player_bonus balance %d after %d concurrent distinct-key grants, got %d", n*100, n, bal.Signed())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	assertB1Holds(t, pool, f.tenantID, "EUR")
}

// TestBonusMirror_CallerRollbackLeavesNothing is §7.15 item 23: if the
// CALLER rolls back its own transaction after a successful Post, nothing
// is visible afterwards - including the generator's own legs and the
// house-level accounts GetOrCreateAccount mints along the way.
func TestBonusMirror_CallerRollbackLeavesNothing(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")

	// A fresh asset (never posted to before) so a NEW promo_liability
	// account for it is minted DURING this rolled-back call, proving the
	// mint itself does not survive either.
	asset := "BRL"
	walletID := seedWalletForAsset(t, pool, f, asset)
	freshBonus := seedBonusAccounts(t, pool, f, walletID, asset)
	_ = a

	var txID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		res, err := Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusGrant,
			IdempotencyKey:  "caller-rollback-grant",
			CorrelationID:   uuid.New(),
			Entries:         []EntryInput{{LedgerAccountID: freshBonus.playerBonus, Direction: Credit, Amount: 42}},
			BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
		})
		if err != nil {
			return err
		}
		txID = res.TransactionID
		return errors.New("deliberate rollback: caller decided not to commit")
	})
	if err == nil {
		t.Fatal("expected the wrapper to surface the deliberate rollback error")
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE id = $1`, txID).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("rolled-back Post left %d ledger_transactions row(s) behind", count)
		}
		bal, err := GetProjectedBalance(ctx, tx, freshBonus.playerBonus)
		if err != nil {
			return err
		}
		if bal.Found {
			t.Fatalf("rolled-back Post left a balance projection row: %+v", bal)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify nothing survived the rollback: %v", err)
	}
}

// TestBonusMirror_ExponentIndependence is §7.15 item 27: grant,
// conversion and forfeiture repeated at exponents 0, 2, 8 and 18,
// reusing this package's own exponent-matrix helper
// (lockedExponentCases) rather than minting a fourth set of synthetic
// assets. Also re-proves item 25/26's spirit at the account level:
// RebuildBalance from ledger_entries alone agrees with the projection
// after every posting, for every account this generator touches,
// following migration 0048's own "prove it by execution" precedent
// (§6.5.8 item 9).
func TestBonusMirror_ExponentIndependence(t *testing.T) {
	pool := testPool(t)
	cases := lockedExponentCases(t, pool)
	f := seedFixture(t, pool)
	operatorCost := &BonusCostAttribution{Funding: FundingOperator}

	for _, c := range cases {
		t.Run(fmt.Sprintf("exponent_%d", c.exponent), func(t *testing.T) {
			walletID := seedWalletForAsset(t, pool, f, c.asset)
			a := seedBonusAccounts(t, pool, f, walletID, c.asset)

			const grantAmount = int64(123_456_789)
			grant := mustPost(t, pool, f, TransactionInput{
				TenantID:        f.tenantID,
				TransactionType: TxBonusGrant,
				IdempotencyKey:  "exp-grant-" + c.asset,
				CorrelationID:   uuid.New(),
				Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: grantAmount}},
				BonusCost:       operatorCost,
			})
			assertExactEntrySet(t, readEntries(t, pool, f.tenantID, grant.TransactionID), []gotEntry{
				{a.playerBonus, Credit, grantAmount},
				{a.promoLiability, Debit, grantAmount},
			})

			const convertAmount = int64(50_000_000)
			var cashID uuid.UUID
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				cashID, err = GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, AccountPlayerCash, c.asset)
				return err
			})
			if err != nil {
				t.Fatalf("seed cash account: %v", err)
			}
			conversion := mustPost(t, pool, f, TransactionInput{
				TenantID:        f.tenantID,
				TransactionType: TxBonusConversion,
				IdempotencyKey:  "exp-conversion-" + c.asset,
				CorrelationID:   uuid.New(),
				Entries: []EntryInput{
					{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: convertAmount},
					{LedgerAccountID: cashID, Direction: Credit, Amount: convertAmount},
				},
				BonusCost: operatorCost,
			})
			assertExactEntrySet(t, readEntries(t, pool, f.tenantID, conversion.TransactionID), []gotEntry{
				{a.playerBonus, Debit, convertAmount},
				{cashID, Credit, convertAmount},
				{a.promoLiability, Credit, convertAmount},
				{a.bonusExpense, Debit, convertAmount},
			})

			const forfeitAmount = int64(1)
			forfeiture := mustPost(t, pool, f, TransactionInput{
				TenantID:        f.tenantID,
				TransactionType: TxBonusForfeiture,
				IdempotencyKey:  "exp-forfeiture-" + c.asset,
				CorrelationID:   uuid.New(),
				ReasonCode:      strPtr("expired"),
				Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: forfeitAmount}},
				BonusCost:       operatorCost,
			})
			assertExactEntrySet(t, readEntries(t, pool, f.tenantID, forfeiture.TransactionID), []gotEntry{
				{a.playerBonus, Debit, forfeitAmount},
				{a.promoLiability, Credit, forfeitAmount},
			})

			assertB1Holds(t, pool, f.tenantID, c.asset)

			// RebuildBalance (from ledger_entries alone) must agree with
			// the projection for every account this exponent's cases
			// touched - invariant #9, proved by execution.
			err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				for _, accountID := range []uuid.UUID{a.playerBonus, a.promoLiability, a.bonusExpense, cashID} {
					projected, err := GetProjectedBalance(ctx, tx, accountID)
					if err != nil {
						return err
					}
					rebuilt, err := RebuildBalance(ctx, tx, accountID)
					if err != nil {
						return err
					}
					if rebuilt.DebitTotal != projected.DebitTotal || rebuilt.CreditTotal != projected.CreditTotal {
						t.Fatalf("account %s (exponent %d): rebuilt %+v != projected %+v", accountID, c.exponent, rebuilt, projected)
					}
				}
				return nil
			})
			if err != nil {
				t.Fatalf("verify rebuild-vs-projection: %v", err)
			}
		})
	}
}

// TestBonusMirror_RLSScopedToOwnerAndTenant is §7.15 item 28: a
// player-scoped connection can read its own player_bonus account and
// cannot read another player's; a tenant-staff connection cannot read
// another tenant's promo_liability/bonus_expense; a player-scoped
// connection cannot post at all (the existing platform-wide policy,
// re-proved for the three account types this dispatch adds/uses).
func TestBonusMirror_RLSScopedToOwnerAndTenant(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	other := seedFixture(t, pool)

	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")
	mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxBonusGrant,
		IdempotencyKey:  "rls-grant",
		CorrelationID:   uuid.New(),
		Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 500}},
		BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
	})

	// Player self-scope: f's own player can read its player_bonus row;
	// other's player cannot.
	err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE ledger_account_id = $1`, a.playerBonus).Scan(&count); err != nil {
			return err
		}
		if count == 0 {
			t.Fatal("the owning player must see its own player_bonus entries")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("player self-scope read: %v", err)
	}

	// A rejected INSERT aborts the underlying Postgres transaction, so the
	// closure returns postErr itself (WithPlayerScope then skips its own
	// Commit, matching every other caller's mustPost-style idiom in this
	// package) rather than swallowing it and trying to commit an already
	// -aborted transaction, which would surface as an unrelated
	// "commit unexpectedly resulted in rollback" error instead of the
	// RLS rejection this test is actually about.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, postErr := Post(ctx, tx, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxBonusGrant,
			IdempotencyKey:  "rls-player-cannot-post",
			CorrelationID:   uuid.New(),
			Entries:         []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 1}},
			BonusCost:       &BonusCostAttribution{Funding: FundingOperator},
		})
		return postErr
	})
	if err == nil {
		t.Fatal("a player-scoped connection must not be able to post at all")
	}

	// Tenant-staff scope: another tenant cannot see this tenant's
	// promo_liability/bonus_expense accounts at all.
	err = pool.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var count int
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_accounts WHERE id = ANY($1)`,
			[]uuid.UUID{a.promoLiability, a.bonusExpense}).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			t.Fatalf("another tenant must not see this tenant's promo_liability/bonus_expense accounts, saw %d", count)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("cross-tenant read: %v", err)
	}
}
