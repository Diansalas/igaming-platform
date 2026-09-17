//go:build integration

// Read-side half of migration 0048's non-negotiable phase-2 test set
// (docs/architecture/ledger-accounting-model.md §6.5.8). The write-side
// half (items 1, 2, 5) and the migration mechanics (items 6, 7) live in
// internal/ledger.
//
// Coverage map (§6.5.8 item -> test):
//
//	item 3  (GetSummary combines BOTH locked origins, in either row
//	         order)  -> TestGetSummary_CombinesBothLockedOriginsInEitherRowOrder
//	item 4  (an unhandled player-owned account type errors, never
//	         silently reports zero - the erroring `default` arm, invariant
//	         L1 layer 4)
//	                 -> TestGetSummary_UnhandledPlayerOwnedAccountTypeReturnsError
//	item 8  (the ledger-vs-projection sweep passes unchanged over a
//	         wallet holding the new account types, and still detects
//	         drift on one)
//	                 -> TestReconciliationSweep_UnchangedOverLockedSplitAccounts
//	item 9  (items 3 and 8 at exponent 0 and 18) -> both tests above are
//	         parameterized over the exponent matrix
//	item 10 (RLS re-proved for the new account types)
//	                 -> TestRLS_LockedSplitAccountsScopedToOwnerAndTenant
//
// Why some fixtures seed a projection row directly: HR-9 (§6.5.7)
// deliberately makes every player_locked_bonus posting fail closed until
// the bonus_expense account type and the Rule B2 (extended) mirror
// generator exist, so there is NO way to give a wallet a real
// bonus-origin locked balance at this stage - and inventing one by
// disabling the guard in a test would test a configuration the platform
// never runs. GetSummary is a pure read over wallet_balance_projection,
// so seeding that row directly exercises exactly the code path a future
// real posting will drive. Every test that asserts reconciliation
// cleanliness uses only REAL postings and its own tenant, so these
// hand-seeded rows never mask or fake a reconciliation result.
package wallet

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
)

// Synthetic asset codes for this package's locked-split tests (the
// registry ships nothing at exponent 0 or 18). ensureAssetAtExponent is
// duplicated in internal/ledger's own locked-split test file, in the same
// way testPool/seedFixture already are - but the CODES are deliberately
// distinct from that file's (LCKEXP0/LCKEXP18), suffixed "W" for wallet.
// `go test ./...` runs package binaries CONCURRENTLY against one
// TEST_DATABASE_URL, so sharing codes races on a fresh database: either a
// duplicate-PK insert, or one package's helper cancelling the other's
// still-pending asset-registry four-eyes request (code-reviewer finding
// F3, Stage 4H-B0-R7). Distinct codes remove the shared row entirely, so
// no locking or ON CONFLICT is needed.
const (
	lockedAssetExp0  = "LCKEXP0W"
	lockedAssetExp2  = "EUR"
	lockedAssetExp18 = "LCKEXP18W"
)

func ensureAssetAtExponent(t *testing.T, pool *db.Pool, code, assetType string, exponent int16) {
	t.Helper()
	network := ""
	if assetType == "crypto" {
		network = "wallet-locked-split-test-network"
	}

	var existing *int16
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT decimal_exponent FROM assets WHERE code = $1`, code).Scan(&existing)
	})
	if err == nil && existing != nil {
		if *existing != exponent {
			t.Fatalf("test asset %s exists at exponent %d, expected %d", code, *existing, exponent)
		}
		return
	}

	requester, approver := uuid.New(), uuid.New()
	requestID := uuid.New()
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range []uuid.UUID{requester, approver} {
			personID := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx,
				`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id, status)
				 VALUES ($1, NULL, $2, 'x', 'platform_admin', $3, 'active')`,
				id, id.String()+"@platform.example.com", personID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed platform principals: %v", err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, requester.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx,
			`UPDATE asset_change_requests SET state = 'cancelled' WHERE asset_code = $1 AND state = 'pending'`, code); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO asset_change_requests (id, operation, asset_code, payload, reason_code, requested_by_principal_id)
			 VALUES ($1, 'create', $2, $3::jsonb, 'wallet locked-origin split test asset', $4)`,
			requestID, code, fmt.Sprintf(`{"asset_type":%q,"decimal_exponent":%d,"network":%q}`, assetType, exponent, network), requester)
		return err
	})
	if err != nil {
		t.Fatalf("file asset change request for %s: %v", code, err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, approver.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO asset_change_approvals (request_id, approver_principal_id, decision) VALUES ($1, $2, 'approve')`,
			requestID, approver)
		return err
	})
	if err != nil {
		t.Fatalf("approve asset change request for %s: %v", code, err)
	}

	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, requester.String()); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO assets (code, asset_type, decimal_exponent, display_name, network) VALUES ($1, $2, $3, $4, NULLIF($5, ''))`,
			code, assetType, exponent, "Locked-split exponent-"+fmt.Sprint(exponent)+" test asset", network)
		return err
	})
	if err != nil {
		t.Fatalf("create test asset %s: %v", code, err)
	}
}

func lockedExponentCases(t *testing.T, pool *db.Pool) []struct {
	asset    string
	exponent int16
} {
	t.Helper()
	ensureAssetAtExponent(t, pool, lockedAssetExp0, "fiat", 0)
	ensureAssetAtExponent(t, pool, lockedAssetExp18, "crypto", 18)
	return []struct {
		asset    string
		exponent int16
	}{
		{lockedAssetExp0, 0},
		{lockedAssetExp2, 2},
		{lockedAssetExp18, 18},
	}
}

// seedPlayer adds another player_account to f's tenant/brand, so a test
// can hold two wallets of the SAME asset (one player may hold only one
// wallet per asset - migration 0019's UNIQUE constraint).
func seedPlayer(t *testing.T, pool *db.Pool, f fixture) uuid.UUID {
	t.Helper()
	playerID, personID := uuid.New(), uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	})
	if err != nil {
		t.Fatalf("seed person: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status)
			 VALUES ($1, $2, $3, $4, $5, 'x', 'active')`,
			playerID, f.tenantID, f.brandID, personID, playerID.String()+"@example.com")
		return err
	})
	if err != nil {
		t.Fatalf("seed player account: %v", err)
	}
	return playerID
}

// postCashLock funds a wallet and moves `locked` minor units from
// player_cash into player_locked_cash, returning the locked account id.
//
// The transaction type is manual_adjustment, NOT sportsbook_bet: no
// sportsbook_* transaction_type exists at HEAD and migration 0048
// deliberately does not add one (§6.5.2/§6.5.6). This is the generic
// ledger capability, not a sportsbook lock.
func postCashLock(t *testing.T, pool *db.Pool, f fixture, w Wallet, keyPrefix string, deposit, locked int64) uuid.UUID {
	t.Helper()
	var lockedCashID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		cashID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerCash, w.AssetCode)
		if err != nil {
			return err
		}
		clearingID, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountPSPClearing, w.AssetCode)
		if err != nil {
			return err
		}
		lockedCashID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerLockedCash, w.AssetCode)
		if err != nil {
			return err
		}
		provider, providerTx := "mockpsp", keyPrefix+"-dep"
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: keyPrefix + "-dep",
			ProviderID: &provider, ProviderTxID: &providerTx, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: clearingID, Direction: ledger.Debit, Amount: deposit},
				{LedgerAccountID: cashID, Direction: ledger.Credit, Amount: deposit},
			},
		}); err != nil {
			return err
		}
		reason := "locked_origin_split_read_side_test"
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment, IdempotencyKey: keyPrefix + "-lock",
			CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: cashID, Direction: ledger.Debit, Amount: locked},
				{LedgerAccountID: lockedCashID, Direction: ledger.Credit, Amount: locked},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("post cash lock (%s): %v", keyPrefix, err)
	}
	return lockedCashID
}

// seedProjectionRow creates the ledger account of accountType for w and
// hand-writes its balance projection row with the given credit total.
//
// Used ONLY for balances no posting path can legally produce at this
// stage: player_locked_bonus (blocked by HR-9) and the deliberately
// unhandled account type item 4 needs. Every value it writes is an
// integer minor-unit amount - no float is involved anywhere.
func seedProjectionRow(t *testing.T, pool *db.Pool, f fixture, w Wallet, accountType ledger.AccountType, credit int64) uuid.UUID {
	t.Helper()
	var accountID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		accountID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, accountType, w.AssetCode)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO wallet_balance_projection
				(ledger_account_id, tenant_id, wallet_id, player_account_id, asset_code, account_type, debit_total, credit_total)
			 SELECT la.id, la.tenant_id, la.wallet_id, la.player_account_id, la.asset_code, la.account_type, 0, $2
			 FROM ledger_accounts la WHERE la.id = $1`,
			accountID, credit)
		return err
	})
	if err != nil {
		t.Fatalf("seed projection row for %s: %v", accountType, err)
	}
	return accountID
}

// summaryInForcedRowOrder calls GetSummary with the wallet's two locked
// projection rows FORCED into the given physical order, and returns both
// the summary and the account_type order GetSummary's own query actually
// observed - so the test proves it exercised the order it intended
// instead of assuming it did.
//
// How the order is forced, and why it has to be forced at all: GetSummary's
// query carries no ORDER BY (correctly - the aggregation must not depend
// on order), so row order is whatever the plan yields, and a first
// attempt that merely built two wallets with the rows created in opposite
// orders proved plan-dependent and flaky. Here each row is rewritten to
// the heap tail in the requested sequence (DELETE + re-INSERT of the
// identical totals - wallet_balance_projection is a rebuildable cache,
// not the append-only ledger, and no value is altered), and index/bitmap
// scans are disabled for the transaction so the join reads the heap in
// that order. Everything happens inside ONE transaction, so the rewrite
// and the read cannot be separated by another writer.
func summaryInForcedRowOrder(t *testing.T, pool *db.Pool, f fixture, w Wallet, order []uuid.UUID) (Summary, []string) {
	t.Helper()
	var s Summary
	var observed []string
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, id := range order {
			if _, err := tx.Exec(ctx,
				`WITH moved AS (
					DELETE FROM wallet_balance_projection WHERE ledger_account_id = $1
					RETURNING ledger_account_id, tenant_id, wallet_id, player_account_id,
					          asset_code, account_type, debit_total, credit_total
				 )
				 INSERT INTO wallet_balance_projection
					(ledger_account_id, tenant_id, wallet_id, player_account_id,
					 asset_code, account_type, debit_total, credit_total)
				 SELECT ledger_account_id, tenant_id, wallet_id, player_account_id,
				        asset_code, account_type, debit_total, credit_total
				 FROM moved`, id); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_indexscan = off`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SET LOCAL enable_bitmapscan = off`); err != nil {
			return err
		}
		// Synchronized sequential scans let a seq scan START MID-TABLE and
		// wrap around, which ROTATES the rows a query sees relative to
		// physical order. That is invisible until it isn't: one full
		// `go test -tags=integration ./...` run (packages executing
		// concurrently against one database, so the shared projection table
		// is both larger and being scanned by other sessions) produced
		// exactly a rotation here - intended [player_cash, locked_bonus,
		// locked_cash] observed as [locked_cash, player_cash, locked_bonus]
		// - and this harness correctly refused to report a pass it had not
		// earned. Turning the feature off for this transaction removes the
		// only mechanism in PostgreSQL that reorders a seq scan's output,
		// which is what makes the forced order reproducible rather than
		// usually-right.
		if _, err := tx.Exec(ctx, `SET LOCAL synchronize_seqscans = off`); err != nil {
			return err
		}

		var err error
		if s, err = GetSummary(ctx, tx, w); err != nil {
			return err
		}

		rows, err := tx.Query(ctx,
			`SELECT p.account_type
			 FROM wallet_balance_projection p
			 JOIN ledger_accounts la ON la.id = p.ledger_account_id
			 WHERE la.wallet_id = $1`, w.ID)
		if err != nil {
			return err
		}
		defer rows.Close()
		observed = nil
		for rows.Next() {
			var at string
			if err := rows.Scan(&at); err != nil {
				return err
			}
			observed = append(observed, at)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatalf("summary in forced row order: %v", err)
	}
	return s, observed
}

func indexOf(haystack []string, needle string) int {
	for i, s := range haystack {
		if s == needle {
			return i
		}
	}
	return -1
}

// TestGetSummary_CombinesBothLockedOriginsInEitherRowOrder is §6.5.8
// item 3 (repeated across item 9's exponent matrix): the defect §6.3.4
// item 1 found - `=` on a field fed by TWO rows, so the reported locked
// balance was whichever row the scan happened to return last - caught by
// a test instead of by a player.
//
// Order-independence is proved by construction, not asserted: the same
// wallet is read twice with the two locked projection rows forced into
// opposite physical orders, and the test FAILS LOUDLY if the query did
// not in fact see them in the intended order, rather than quietly passing
// on one order twice.
func TestGetSummary_CombinesBothLockedOriginsInEitherRowOrder(t *testing.T) {
	pool := testPool(t)
	cases := lockedExponentCases(t, pool)
	f := seedFixture(t, pool)

	const deposit int64 = 1_000_000
	const lockedCash int64 = 7_000
	const lockedBonus int64 = 250

	for _, c := range cases {
		var w Wallet
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			w, err = GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, c.asset)
			return err
		})
		if err != nil {
			t.Fatalf("%s: create wallet: %v", c.asset, err)
		}

		lockedCashID := postCashLock(t, pool, f, w, "order-"+c.asset, deposit, lockedCash)
		lockedBonusID := seedProjectionRow(t, pool, f, w, ledger.AccountPlayerLockedBonus, lockedBonus)

		for _, run := range []struct {
			name  string
			order []uuid.UUID
			first string
		}{
			{"locked_cash row first", []uuid.UUID{lockedCashID, lockedBonusID}, "player_locked_cash"},
			{"locked_bonus row first", []uuid.UUID{lockedBonusID, lockedCashID}, "player_locked_bonus"},
		} {
			// Rewritten rows are appended to the heap tail in the order
			// given, so the slice order IS the intended read order - and
			// the assertion below proves that rather than trusting it.
			// Bounded retry, NOT a tolerance: the assertion below is still
			// a hard failure if the intended order is never achieved. The
			// retry only absorbs a transient plan/scan-order surprise
			// (see summaryInForcedRowOrder's note on synchronized seq
			// scans), because a harness that cannot reproduce its own
			// setup must say so rather than flake a financial test suite.
			const orderAttempts = 3
			var s Summary
			var observed []string
			var gotFirst string
			for attempt := 1; attempt <= orderAttempts; attempt++ {
				s, observed = summaryInForcedRowOrder(t, pool, f, w, run.order)

				cashIdx, bonusIdx := indexOf(observed, "player_locked_cash"), indexOf(observed, "player_locked_bonus")
				if cashIdx < 0 || bonusIdx < 0 {
					t.Fatalf("%s/%s: both locked rows must be visible, observed %v", c.asset, run.name, observed)
				}
				gotFirst = "player_locked_cash"
				if bonusIdx < cashIdx {
					gotFirst = "player_locked_bonus"
				}
				if gotFirst == run.first {
					break
				}
				t.Logf("%s/%s: attempt %d/%d did not achieve the intended row order (wanted %s first, observed %v); retrying",
					c.asset, run.name, attempt, orderAttempts, run.first, observed)
			}
			if gotFirst != run.first {
				t.Fatalf("%s/%s: the harness failed to force the intended row order in %d attempts (wanted %s first, "+
					"observed %v); order-independence is therefore UNPROVEN for this case and must not be reported "+
					"as passing", c.asset, run.name, orderAttempts, run.first, observed)
			}

			if s.LockedCashBalance != lockedCash {
				t.Fatalf("%s/%s: LockedCashBalance = %d, want %d", c.asset, run.name, s.LockedCashBalance, lockedCash)
			}
			if s.LockedBonusBalance != lockedBonus {
				t.Fatalf("%s/%s: LockedBonusBalance = %d, want %d", c.asset, run.name, s.LockedBonusBalance, lockedBonus)
			}
			if s.LockedBalance != lockedCash+lockedBonus {
				t.Fatalf("%s/%s (exponent %d): LockedBalance = %d, want %d (the combined field must be += over BOTH origins, "+
					"not = whichever row arrived last)", c.asset, run.name, c.exponent, s.LockedBalance, lockedCash+lockedBonus)
			}
			if s.CashBalance != deposit-lockedCash {
				t.Fatalf("%s/%s: CashBalance = %d, want %d", c.asset, run.name, s.CashBalance, deposit-lockedCash)
			}
			// AvailableBalance stays CashBalance: locked value has
			// already LEFT player_cash, so nothing is owed a second
			// subtraction (reconciliation-model.md §3).
			if s.AvailableBalance != s.CashBalance {
				t.Fatalf("%s/%s: AvailableBalance = %d, want %d", c.asset, run.name, s.AvailableBalance, s.CashBalance)
			}
			if s.HeldForWithdrawal != 0 || s.BonusBalance != 0 {
				t.Fatalf("%s/%s: expected zero hold/bonus balances, got %+v", c.asset, run.name, s)
			}
		}
	}
}

// TestGetSummary_UnhandledPlayerOwnedAccountTypeReturnsError is §6.5.8
// item 4 and invariant L1 layer 4: the erroring `default` arm, which is
// the root-cause fix (§6.5.5). The pre-0048 defect was not "this switch
// lacks two cases" but "this switch silently drops what it does not
// recognize" - so the class, not just the instance, has to be tested.
//
// The unhandled row is a wallet-owned promo_liability account, inserted
// as raw SQL because no code path produces one: the live schema permits
// it (ADR 0035's proposed ledger_accounts_owner_family CHECK, which would
// forbid it, is unauthorized and absent - see §6.5.11), while
// GetSummary's switch has no arm for it. That is exactly the shape of a
// future migration adding a player-owned type without updating this
// switch (HR-7).
func TestGetSummary_UnhandledPlayerOwnedAccountTypeReturnsError(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var w Wallet
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		w, err = GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("create wallet: %v", err)
	}
	postCashLock(t, pool, f, w, "default-arm", 5_000, 1_000)

	// Control: the same wallet summarizes cleanly before the unhandled
	// row exists, so the failure below is attributable to that row alone.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := GetSummary(ctx, tx, w)
		if err != nil {
			return err
		}
		if s.LockedCashBalance != 1_000 || s.LockedBalance != 1_000 {
			t.Fatalf("control summary wrong: %+v", s)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("control GetSummary: %v", err)
	}

	seedProjectionRow(t, pool, f, w, ledger.AccountPromoLiability, 4_242)

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := GetSummary(ctx, tx, w)
		if err == nil {
			t.Fatalf("GetSummary silently accepted an unhandled player-owned account type and returned %+v; "+
				"a plausible-looking wrong balance is the worse outcome (L1 layer 4)", s)
		}
		if !strings.Contains(err.Error(), "promo_liability") || !strings.Contains(err.Error(), "unhandled") {
			t.Fatalf("error must name the unhandled account type, got: %v", err)
		}
		if s != (Summary{}) {
			t.Fatalf("a failed GetSummary must return a zero Summary, not a partially populated one: %+v", s)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify default arm: %v", err)
	}
}

// TestReconciliationSweep_UnchangedOverLockedSplitAccounts is §6.5.8
// item 8 (repeated across item 9's exponent matrix): §6.4.6 item 3's
// claim that internal/reconciliation treats account_type as opaque,
// proved by execution rather than asserted.
//
// It lives in this package because internal/reconciliation imports
// internal/ledger, so a test in that package cannot import it back
// (import cycle), while internal/wallet is imported by neither.
//
// Every balance here comes from a REAL posting - no hand-seeded
// projection row - because the whole point is that the sweep agrees with
// the ledger.
//
// This test deliberately creates NO player_locked_bonus account: an
// account with no entries at all trips a PRE-EXISTING false positive in
// internal/reconciliation that has nothing to do with migration 0048
// (finding LF-0048-1, characterized by the next test). Mixing the two
// would make this test's result unreadable.
func TestReconciliationSweep_UnchangedOverLockedSplitAccounts(t *testing.T) {
	pool := testPool(t)
	cases := lockedExponentCases(t, pool)
	f := seedFixture(t, pool)

	var driftAccountID uuid.UUID
	for i, c := range cases {
		player := seedPlayer(t, pool, f)
		var w Wallet
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			w, err = GetOrCreate(ctx, tx, f.tenantID, f.brandID, player, c.asset)
			return err
		})
		if err != nil {
			t.Fatalf("%s: seed wallet: %v", c.asset, err)
		}
		locked := postCashLock(t, pool, f, w, "sweep-"+c.asset, 900_000, 123_456)
		if i == 0 {
			driftAccountID = locked
		}
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, mismatches, err := reconciliation.RunLedgerVsProjection(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
		if err != nil {
			return err
		}
		if run.Status != reconciliation.StatusClean || len(mismatches) != 0 {
			t.Fatalf("sweep over locked-split accounts must be clean, got status=%s mismatches=%+v", run.Status, mismatches)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("clean sweep: %v", err)
	}

	// And drift on a player_locked_cash projection row is still DETECTED:
	// opacity must not mean blindness.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE wallet_balance_projection SET credit_total = credit_total + 1 WHERE ledger_account_id = $1`,
			driftAccountID)
		return err
	})
	if err != nil {
		t.Fatalf("inject drift: %v", err)
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, mismatches, err := reconciliation.RunLedgerVsProjection(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
		if err != nil {
			return err
		}
		if run.Status != reconciliation.StatusMismatchesFound || len(mismatches) != 1 {
			t.Fatalf("expected exactly one mismatch on the drifted player_locked_cash account, got status=%s mismatches=%+v",
				run.Status, mismatches)
		}
		if mismatches[0].ReconciliationKey != driftAccountID.String() {
			t.Fatalf("mismatch keyed to %s, expected the drifted account %s", mismatches[0].ReconciliationKey, driftAccountID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("drift sweep: %v", err)
	}
}

// TestReconciliationSweep_EntrylessAccountIsAFalsePositive_KNOWNDEFECT
// is a CHARACTERIZATION test, not an approval: it pins behavior this
// dispatch found and is NOT authorized to fix, so the defect cannot be
// lost and cannot be silently "fixed" without a deliberate test update.
//
// FINDING LF-0048-1 (found while implementing §6.5.8 item 8; owner:
// `ledger-finance`; PRE-EXISTING, not introduced by migration 0048).
// internal/reconciliation's ledger-vs-projection sweep reports a spurious
// `balance_mismatch` for ANY ledger account that has no entries and
// therefore no wallet_balance_projection row:
//
//   - `GetProjectedBalance` returns a zero-valued Balance with Found=false
//     and EMPTY AssetCode/AccountType when the row is absent (by design);
//   - the sweep's first arm only fires when the rebuilt totals are
//     non-zero, so an entry-less account falls through to the second arm;
//   - that arm compares AssetCode/AccountType unconditionally, so
//     "EUR"/"player_withdrawal_hold" vs ""/"" is a mismatch - even though
//     the first arm's own comment states that an account which has never
//     been posted to "is ordinary and not a mismatch".
//
// Verified independent of this change by reproducing it with
// `player_withdrawal_hold`, a type migration 0048 does not touch. It is a
// false-POSITIVE (a phantom P1 drift alert, per CLAUDE.md's zero-drift
// rule), never a missed real drift, which is why it is reported rather
// than treated as a blocker for migration 0048.
//
// Migration 0048 makes it slightly easier to reach: HR-9 rejects the
// posting AFTER a caller may already have created a player_locked_bonus
// account, so a committed-but-never-posted-to account can now linger.
// The fix belongs in internal/reconciliation (gate the second arm on
// projected.Found, i.e. treat "no projection row AND zero rebuilt totals"
// as ordinary), which this dispatch is explicitly scoped out of touching.
//
// WHEN THAT FIX LANDS THIS TEST MUST FAIL, and its expectation must be
// inverted to "clean" rather than deleted.
func TestReconciliationSweep_EntrylessAccountIsAFalsePositive_KNOWNDEFECT(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var lockedBonusID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := GetOrCreate(ctx, tx, f.tenantID, f.brandID, f.playerAccountID, "EUR")
		if err != nil {
			return err
		}
		// Created, never posted to: HR-9 blocks every posting against it.
		lockedBonusID, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &w.ID, ledger.AccountPlayerLockedBonus, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("seed entry-less locked-bonus account: %v", err)
	}

	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, mismatches, err := reconciliation.RunLedgerVsProjection(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
		if err != nil {
			return err
		}
		if run.Status == reconciliation.StatusClean {
			t.Fatalf("finding LF-0048-1 appears to be FIXED (sweep is clean over an entry-less account). " +
				"That is the desired behavior: update this test to assert StatusClean and zero mismatches, " +
				"and remove the characterization note above")
		}
		if len(mismatches) != 1 || mismatches[0].ReconciliationKey != lockedBonusID.String() {
			t.Fatalf("expected exactly the entry-less account to be (spuriously) flagged, got %+v", mismatches)
		}
		if mismatches[0].MismatchKind != reconciliation.MismatchKindBalanceMismatch {
			t.Fatalf("expected kind %q, got %q", reconciliation.MismatchKindBalanceMismatch, mismatches[0].MismatchKind)
		}
		// The shape of the false positive: zero totals on both sides,
		// differing only in the absent row's empty asset/type strings.
		if !strings.Contains(mismatches[0].ActualValue, "asset= type=") {
			t.Fatalf("finding LF-0048-1's signature is an ABSENT projection row (empty asset/type); got actual=%q - "+
				"this may be a different, real mismatch and must be investigated, not assumed benign",
				mismatches[0].ActualValue)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("characterize LF-0048-1: %v", err)
	}
}

// TestRLS_LockedSplitAccountsScopedToOwnerAndTenant is §6.5.8 item 10:
// ledger_accounts' existing two-policy RLS shape (migration 0020,
// untouched by 0048 - which is the point) re-proved for both new account
// types. A new account_type inherits isolation because the policies key on
// tenant_id/player_account_id and never on account_type; "inherits" is
// cheap to claim and is proved here instead.
func TestRLS_LockedSplitAccountsScopedToOwnerAndTenant(t *testing.T) {
	pool := testPool(t)
	owner := seedFixture(t, pool)
	otherTenant := seedFixture(t, pool)
	otherPlayer := seedPlayer(t, pool, owner)

	var accountIDs []uuid.UUID
	err := pool.WithTenant(context.Background(), owner.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		w, err := GetOrCreate(ctx, tx, owner.tenantID, owner.brandID, owner.playerAccountID, "EUR")
		if err != nil {
			return err
		}
		for _, at := range []ledger.AccountType{ledger.AccountPlayerLockedCash, ledger.AccountPlayerLockedBonus} {
			id, err := ledger.GetOrCreateAccount(ctx, tx, owner.tenantID, &w.ID, at, "EUR")
			if err != nil {
				return err
			}
			accountIDs = append(accountIDs, id)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed locked accounts: %v", err)
	}

	countVisible := func(name string, run func(context.Context, func(context.Context, pgx.Tx) error) error) int {
		t.Helper()
		var count int
		err := run(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM ledger_accounts
				 WHERE id = ANY($1) AND account_type IN ('player_locked_cash', 'player_locked_bonus')`,
				accountIDs).Scan(&count)
		})
		if err != nil {
			t.Fatalf("%s: query: %v", name, err)
		}
		return count
	}

	if got := countVisible("owner player scope", func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
		return pool.WithPlayerScope(ctx, owner.tenantID, owner.playerAccountID, fn)
	}); got != 2 {
		t.Fatalf("the owning player must see both of its own locked accounts, saw %d", got)
	}
	if got := countVisible("other player, same tenant", func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
		return pool.WithPlayerScope(ctx, owner.tenantID, otherPlayer, fn)
	}); got != 0 {
		t.Fatalf("another player in the same tenant must see none of them, saw %d", got)
	}
	if got := countVisible("other tenant staff", func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
		return pool.WithTenant(ctx, otherTenant.tenantID, fn)
	}); got != 0 {
		t.Fatalf("another tenant's staff scope must see none of them, saw %d", got)
	}
	if got := countVisible("owning tenant staff", func(ctx context.Context, fn func(context.Context, pgx.Tx) error) error {
		return pool.WithTenant(ctx, owner.tenantID, fn)
	}); got != 2 {
		t.Fatalf("the owning tenant's staff scope must see both, saw %d", got)
	}
}
