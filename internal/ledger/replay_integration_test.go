//go:build integration

package ledger

// Stage 10 F-7 remediation regression tests (docs/governance/
// stage-10-f7-ledger-replay-audit.md §6.4 L1-L11; ADR 0020 amendment
// 2026-09-25): on an idempotency-key conflict of the same transaction
// type, Post returns AlreadyPosted only for an identical canonical
// payload, and ErrIdempotencyPayloadMismatch - posting nothing - for any
// difference.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func tryPostLedger(pool *db.Pool, f fixture, in TransactionInput) (PostResult, error) {
	var res PostResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = Post(ctx, tx, in)
		return err
	})
	return res, err
}

// ledgerCounts is the tenant's (ledger_transactions, ledger_entries) row
// count - used to prove a rejected replay wrote nothing.
func ledgerCounts(t *testing.T, pool *db.Pool, f fixture) (int, int) {
	t.Helper()
	var txs, entries int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&txs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, f.tenantID).Scan(&entries)
	})
	if err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	return txs, entries
}

func signedBalance(t *testing.T, pool *db.Pool, f fixture, accountID uuid.UUID) int64 {
	t.Helper()
	var b Balance
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		b, err = GetProjectedBalance(ctx, tx, accountID)
		return err
	})
	if err != nil {
		t.Fatalf("read balance: %v", err)
	}
	return b.Signed()
}

// expectMismatch asserts a rejected replay: the typed error naming the
// expected field class, no new ledger rows and unchanged balances of the
// given accounts.
func expectMismatch(t *testing.T, pool *db.Pool, f fixture, in TransactionInput, field string, accounts ...uuid.UUID) {
	t.Helper()
	txsBefore, entriesBefore := ledgerCounts(t, pool, f)
	balances := make([]int64, len(accounts))
	for i, a := range accounts {
		balances[i] = signedBalance(t, pool, f, a)
	}
	res, err := tryPostLedger(pool, f, in)
	if !errors.Is(err, ErrIdempotencyPayloadMismatch) {
		t.Fatalf("want ErrIdempotencyPayloadMismatch (%s), got res=%+v err=%v", field, res, err)
	}
	if errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("payload mismatch must not also match ErrIdempotencyKeyReused: %v", err)
	}
	if !strings.Contains(err.Error(), field) {
		t.Fatalf("want field class %q named in %q", field, err.Error())
	}
	txsAfter, entriesAfter := ledgerCounts(t, pool, f)
	if txsAfter != txsBefore || entriesAfter != entriesBefore {
		t.Fatalf("rejected replay wrote rows: transactions %d->%d, entries %d->%d", txsBefore, txsAfter, entriesBefore, entriesAfter)
	}
	for i, a := range accounts {
		if got := signedBalance(t, pool, f, a); got != balances[i] {
			t.Fatalf("rejected replay changed balance of %s: %d -> %d", a, balances[i], got)
		}
	}
}

func expectAlreadyPosted(t *testing.T, pool *db.Pool, f fixture, in TransactionInput, originalID uuid.UUID) {
	t.Helper()
	txsBefore, entriesBefore := ledgerCounts(t, pool, f)
	res, err := tryPostLedger(pool, f, in)
	if err != nil {
		t.Fatalf("legitimate replay rejected: %v", err)
	}
	if !res.AlreadyPosted || res.TransactionID != originalID {
		t.Fatalf("want AlreadyPosted with %s, got %+v", originalID, res)
	}
	txsAfter, entriesAfter := ledgerCounts(t, pool, f)
	if txsAfter != txsBefore || entriesAfter != entriesBefore {
		t.Fatalf("idempotent replay wrote rows: transactions %d->%d, entries %d->%d", txsBefore, txsAfter, entriesBefore, entriesAfter)
	}
}

// L1 + L4: an identical final entry set, and the same multiset in a
// different order, are both an idempotent replay with the original id.
func TestReplay_IdenticalAndPermutedEntriesAreAlreadyPosted(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	in := depositInput(f, "replay-l1", 500)
	first := mustPost(t, pool, f, in)

	expectAlreadyPosted(t, pool, f, in, first.TransactionID)

	permuted := in
	permuted.Entries = []EntryInput{in.Entries[1], in.Entries[0]}
	expectAlreadyPosted(t, pool, f, permuted, first.TransactionID)

	if got := signedBalance(t, pool, f, f.cashAccountID); got != 500 {
		t.Fatalf("balance after replays = %d, want 500", got)
	}
}

// L2: a different amount under the same key is rejected; nothing posted.
func TestReplay_DifferentAmountRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	mustPost(t, pool, f, depositInput(f, "replay-l2", 500))
	expectMismatch(t, pool, f, depositInput(f, "replay-l2", 501), replayFieldEntries, f.cashAccountID, f.clearingID)
}

// L3: a different account (another wallet) under the same key is rejected.
func TestReplay_DifferentAccountRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	mustPost(t, pool, f, depositInput(f, "replay-l3", 500))

	// A different account of the same asset (the wallet's withdrawal
	// hold), so the replay is balanced in itself and differs only in
	// which account it names.
	var otherCash uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		otherCash, err = GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, AccountPlayerWithdrawalHold, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("seed second account: %v", err)
	}
	in := depositInput(f, "replay-l3", 500)
	in.Entries[1].LedgerAccountID = otherCash
	expectMismatch(t, pool, f, in, replayFieldEntries, f.cashAccountID, otherCash)

	// Direction flip (same accounts and amount) is also a different fact.
	flipped := depositInput(f, "replay-l3", 500)
	flipped.Entries[0].Direction, flipped.Entries[1].Direction = Credit, Debit
	expectMismatch(t, pool, f, flipped, replayFieldEntries, f.cashAccountID)

	// A superset (extra balanced pair) is a different multiset - no netting.
	extra := depositInput(f, "replay-l3", 500)
	extra.Entries = append(extra.Entries,
		EntryInput{LedgerAccountID: f.clearingID, Direction: Debit, Amount: 1},
		EntryInput{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 1})
	expectMismatch(t, pool, f, extra, replayFieldEntries, f.cashAccountID)
}

// L5: a different, or nil-vs-set, reverses_transaction_id is rejected.
func TestReplay_DifferentReversalLinkRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	d1 := mustPost(t, pool, f, depositInput(f, "replay-l5-d1", 300))
	d2 := mustPost(t, pool, f, depositInput(f, "replay-l5-d2", 300))

	reversal := func(reverses *uuid.UUID) TransactionInput {
		provider, ref := "mockpsp", "replay-l5-rev"
		return TransactionInput{
			TenantID: f.tenantID, TransactionType: TxDepositReversal, IdempotencyKey: "replay-l5-rev",
			ProviderID: &provider, ProviderTxID: &ref, CorrelationID: uuid.NewSHA1(f.tenantID, []byte("replay-l5-rev")),
			ReversesTransactionID: reverses,
			Entries: []EntryInput{
				{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 300},
				{LedgerAccountID: f.clearingID, Direction: Credit, Amount: 300},
			},
		}
	}
	d1ID, d2ID := d1.TransactionID, d2.TransactionID
	rev := mustPost(t, pool, f, reversal(&d1ID))

	expectAlreadyPosted(t, pool, f, reversal(&d1ID), rev.TransactionID)
	expectMismatch(t, pool, f, reversal(&d2ID), replayFieldReversalLink, f.cashAccountID)
	expectMismatch(t, pool, f, reversal(nil), replayFieldReversalLink, f.cashAccountID)
}

// L5 (other direction) + provider ref + causation: the stored row has no
// reversal link and the replay sets one; the provider reference and the
// causation id are compared too.
func TestReplay_ProviderRefAndCausationCompared(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	cause1, cause2 := uuid.New(), uuid.New()
	in := depositInput(f, "replay-prov", 200)
	in.CausationID = &cause1
	first := mustPost(t, pool, f, in)
	expectAlreadyPosted(t, pool, f, in, first.TransactionID)

	withLink := in
	withLink.ReversesTransactionID = &first.TransactionID
	expectMismatch(t, pool, f, withLink, replayFieldReversalLink, f.cashAccountID)

	otherRef := in
	ref := "replay-prov-other"
	otherRef.ProviderTxID = &ref
	expectMismatch(t, pool, f, otherRef, replayFieldProviderRef, f.cashAccountID)

	noRef := in
	noRef.ProviderID, noRef.ProviderTxID = nil, nil
	expectMismatch(t, pool, f, noRef, replayFieldProviderRef, f.cashAccountID)

	otherCause := in
	otherCause.CausationID = &cause2
	expectMismatch(t, pool, f, otherCause, replayFieldCausation, f.cashAccountID)

	noCause := in
	noCause.CausationID = nil
	expectMismatch(t, pool, f, noCause, replayFieldCausation, f.cashAccountID)
}

// L6: a different correlation id is rejected on a non-tombstone, and
// accepted on a tombstone (whose other fields are still compared).
func TestReplay_CorrelationComparedExceptTombstone(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	in := depositInput(f, "replay-l6", 400)
	mustPost(t, pool, f, in)
	otherCorr := in
	otherCorr.CorrelationID = uuid.New()
	expectMismatch(t, pool, f, otherCorr, replayFieldCorrelation, f.cashAccountID)

	provider, ref := "mockpsp", "never-seen-original"
	tomb := func(correlation uuid.UUID, providerTxID string) TransactionInput {
		r := providerTxID
		return TransactionInput{
			TenantID: f.tenantID, TransactionType: TxTombstone, IdempotencyKey: "tombstone:mockpsp:never-seen-original",
			ProviderID: &provider, ProviderTxID: &r, CorrelationID: correlation,
		}
	}
	first := mustPost(t, pool, f, tomb(uuid.New(), ref))
	// Existing tombstone writers mint a fresh correlation per call.
	expectAlreadyPosted(t, pool, f, tomb(uuid.New(), ref), first.TransactionID)
	// The provider reference is still compared on a tombstone.
	expectMismatch(t, pool, f, tomb(uuid.New(), "some-other-original"), replayFieldProviderRef)
}

// L7: a different reason_code is rejected.
func TestReplay_DifferentReasonCodeRejected(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")
	adj := func(reason string) TransactionInput {
		return TransactionInput{
			TenantID: f.tenantID, TransactionType: TxManualAdjustment, IdempotencyKey: "replay-l7",
			CorrelationID: uuid.NewSHA1(f.tenantID, []byte("replay-l7")), ReasonCode: &reason,
			Entries: []EntryInput{
				{LedgerAccountID: a.manualAdjustment, Direction: Debit, Amount: 50},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 50},
			},
		}
	}
	first := mustPost(t, pool, f, adj("goodwill"))
	expectAlreadyPosted(t, pool, f, adj("goodwill"), first.TransactionID)
	expectMismatch(t, pool, f, adj("error_correction"), replayFieldReasonCode, f.cashAccountID)
}

// L8: a bonus posting replayed with identical caller entries and
// BonusCost regenerates identical Rule B2 legs and is AlreadyPosted; the
// same entries with a different BonusCost.Funding land on a different
// recognition account (bonus_expense vs provider_payable) and are
// rejected. A conversion (Dr player_bonus / Cr player_cash) is used
// because it produces BOTH a promo_liability mirror leg and a
// recognition leg; a plain grant nets to zero after the mirror and has no
// Funding-dependent leg at all.
func TestReplay_BonusMirrorLegsCompared(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	a := seedBonusAccounts(t, pool, f, f.walletID, "EUR")
	mustPost(t, pool, f, TransactionInput{
		TenantID: f.tenantID, TransactionType: TxBonusGrant, IdempotencyKey: "replay-l8-grant", CorrelationID: uuid.New(),
		Entries:   []EntryInput{{LedgerAccountID: a.playerBonus, Direction: Credit, Amount: 250}},
		BonusCost: &BonusCostAttribution{Funding: FundingOperator},
	})
	corr := uuid.New()
	conversion := func(cost *BonusCostAttribution) TransactionInput {
		return TransactionInput{
			TenantID: f.tenantID, TransactionType: TxBonusConversion, IdempotencyKey: "replay-l8",
			CorrelationID: corr,
			Entries: []EntryInput{
				{LedgerAccountID: a.playerBonus, Direction: Debit, Amount: 100},
				{LedgerAccountID: f.cashAccountID, Direction: Credit, Amount: 100},
			},
			BonusCost: cost,
		}
	}
	first := mustPost(t, pool, f, conversion(&BonusCostAttribution{Funding: FundingOperator}))
	expectAlreadyPosted(t, pool, f, conversion(&BonusCostAttribution{Funding: FundingOperator}), first.TransactionID)

	providerID := "mock-casino"
	expectMismatch(t, pool, f, conversion(&BonusCostAttribution{Funding: FundingProvider, ProviderID: &providerID}),
		replayFieldEntries, a.playerBonus, f.cashAccountID, a.bonusExpense, a.providerPayable, a.promoLiability)
}

// L9: a type mismatch is still ErrIdempotencyKeyReused, never the new
// sentinel - ADR 0088 §4.5 maps it to ErrSettlementTombstoned.
func TestReplay_TypeMismatchStillErrIdempotencyKeyReused(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	provider, ref := "mockpsp", "replay-l9"
	mustPost(t, pool, f, TransactionInput{
		TenantID: f.tenantID, TransactionType: TxTombstone, IdempotencyKey: "replay-l9",
		ProviderID: &provider, ProviderTxID: &ref, CorrelationID: uuid.New(),
	})
	// The same key, now as a deposit with a different payload as well: the
	// type check runs first and wins.
	_, err := tryPostLedger(pool, f, depositInput(f, "replay-l9", 10))
	if !errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("want ErrIdempotencyKeyReused, got %v", err)
	}
	if errors.Is(err, ErrIdempotencyPayloadMismatch) {
		t.Fatalf("type mismatch must not match ErrIdempotencyPayloadMismatch: %v", err)
	}
}

// L10: concurrent deliveries of one key, half with the original payload
// and half with a different amount. Exactly one posts; every equal-
// payload loser gets AlreadyPosted with the winner's id; every different-
// payload call gets the mismatch; the balance reflects one posting only.
func TestReplay_ConcurrentMixedPayloadsOneEffect(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	const n = 10
	var wg sync.WaitGroup
	results := make([]PostResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			amount := int64(700)
			if i%2 == 1 {
				amount = 701
			}
			results[i], errs[i] = tryPostLedger(pool, f, depositInput(f, "replay-l10", amount))
		}(i)
	}
	close(start)
	wg.Wait()

	var posted, replayed, mismatched int
	var winnerID uuid.UUID
	winnerAmount := int64(0)
	for i := 0; i < n; i++ {
		switch {
		case errs[i] == nil && !results[i].AlreadyPosted:
			posted++
			winnerID = results[i].TransactionID
			winnerAmount = 700
			if i%2 == 1 {
				winnerAmount = 701
			}
		case errs[i] == nil:
			replayed++
		case errors.Is(errs[i], ErrIdempotencyPayloadMismatch):
			mismatched++
		default:
			t.Fatalf("goroutine %d: unexpected error %v", i, errs[i])
		}
	}
	if posted != 1 {
		t.Fatalf("want exactly one posting, got %d (replayed=%d mismatched=%d)", posted, replayed, mismatched)
	}
	for i := 0; i < n; i++ {
		amount := int64(700)
		if i%2 == 1 {
			amount = 701
		}
		if errs[i] == nil && results[i].TransactionID != winnerID {
			t.Fatalf("goroutine %d returned %s, want winner %s", i, results[i].TransactionID, winnerID)
		}
		if amount == winnerAmount && errs[i] != nil {
			t.Fatalf("goroutine %d had the winner's payload but got %v", i, errs[i])
		}
		if amount != winnerAmount && !errors.Is(errs[i], ErrIdempotencyPayloadMismatch) {
			t.Fatalf("goroutine %d had a different payload but got res=%+v err=%v", i, results[i], errs[i])
		}
	}
	if replayed != n/2-1 || mismatched != n/2 {
		t.Fatalf("want %d replays and %d mismatches, got %d and %d", n/2-1, n/2, replayed, mismatched)
	}
	if got := signedBalance(t, pool, f, f.cashAccountID); got != winnerAmount {
		t.Fatalf("balance %d, want %d (one posting)", got, winnerAmount)
	}
	if got := signedBalance(t, pool, f, f.clearingID); got != -winnerAmount {
		t.Fatalf("clearing balance %d, want %d", got, -winnerAmount)
	}
}

// L11: a NEW idempotency key whose (provider_id, provider_tx_id) is
// already taken is not a replay. It stays an untyped, fail-closed error
// (neither sentinel), and posts nothing.
func TestReplay_ProviderIndexOnlyConflictIsUntypedAndPostsNothing(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	mustPost(t, pool, f, depositInput(f, "replay-l11", 90))

	in := depositInput(f, "replay-l11-new-key", 90)
	ref := "replay-l11" // the first posting's provider_tx_id
	in.ProviderTxID = &ref
	txsBefore, entriesBefore := ledgerCounts(t, pool, f)
	_, err := tryPostLedger(pool, f, in)
	if err == nil {
		t.Fatal("want an error for a taken provider reference under a new key")
	}
	if errors.Is(err, ErrIdempotencyPayloadMismatch) || errors.Is(err, ErrIdempotencyKeyReused) {
		t.Fatalf("provider-index-only conflict must stay untyped, got %v", err)
	}
	txsAfter, entriesAfter := ledgerCounts(t, pool, f)
	if txsAfter != txsBefore || entriesAfter != entriesBefore {
		t.Fatal("provider-index-only conflict wrote rows")
	}
	if got := signedBalance(t, pool, f, f.cashAccountID); got != 90 {
		t.Fatalf("balance %d, want 90", got)
	}
}
