//go:build integration

// Migration 0048's locked-origin split (player_locked ->
// player_locked_cash / player_locked_bonus) and HR-9's fail-closed bonus
// posting guard - the ledger-side half of the non-negotiable phase-2 test
// set in docs/architecture/ledger-accounting-model.md §6.5.8.
//
// Coverage map (§6.5.8 item -> test):
//
//	item 1  (bare player_locked rejected by the CHECK, invariant L1
//	         layer 1)                 -> TestLedgerAccounts_BarePlayerLockedRejectedByCheckConstraint
//	item 2  (GetOrCreateAccount for the new types, DB-level idempotent)
//	                                  -> TestGetOrCreateAccount_LockedSplitTypesIdempotentAtEveryExponent
//	item 5  (HR-9 rejects a BONUS_SET posting and writes nothing)
//	                                  -> TestPost_HR9BonusSetPostingsRejectedAndNothingWritten
//	item 9  (items 2/8 at exponent 0 and 18)
//	                                  -> the two tests above plus
//	                                     TestPost_CashOriginLockPostsAndRebuildsAtEveryExponent
//
// Items 3, 4, 8 and 10 are read-side/projection/RLS cases and live in
// internal/wallet/locked_origin_summary_integration_test.go; items 6 and
// 7 are migration-mechanics cases and live in
// migration_0048_integration_test.go.
//
// Invariant L1 layer 2 (the type system) is deliberately NOT a runtime
// test: AccountPlayerLocked no longer exists, so any stale use of it is a
// compile error and this file's existence as a compiling test binary is
// the proof (§6.5.4 layer 5's second clause).
package ledger

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// pgCheckViolation is PostgreSQL's CHECK-constraint violation SQLSTATE -
// what invariant L1 layer 1 must produce for a bare player_locked
// insert, proving the rejection comes from the DATABASE and not from
// application-side discipline (CLAUDE.md).
const pgCheckViolation = "23514"

// accountTypeConstraint is the constraint migration 0048 swaps. Named
// here so a rejection can be attributed to the RIGHT constraint rather
// than to any CHECK on the table.
const accountTypeConstraint = "ledger_accounts_account_type_check"

// The Asset registry ships nothing at exponent 0 or 18 (migration 0003:
// EUR/USD/GBP/BRL/MXN at 2, USDT at 6, BTC at 8), so those two are
// created once as synthetic test assets with fixed codes - `assets` is
// append-only and dual-controlled, so a test must not mint a fresh code
// per run. The codes are unique to THIS package: internal/wallet's
// locked-split test file has a helper of the same shape and uses
// "...W"-suffixed codes, because `go test ./...` runs package binaries
// concurrently against one TEST_DATABASE_URL and a shared code races
// (code-reviewer finding F3, Stage 4H-B0-R7).
const (
	lockedAssetExp0  = "LCKEXP0"
	lockedAssetExp2  = "EUR"
	lockedAssetExp18 = "LCKEXP18"
)

// ensureAssetAtExponent guarantees an `assets` row for code at exponent,
// walking migration 0044/0047's four-eyes path in raw SQL (requester
// principal -> pending change request -> approval by a DIFFERENT platform
// principal linked to a DIFFERENT person -> insert). Modeled on
// internal/risk's helper of the same shape; it deliberately does not
// import internal/assetregistry, so that if that path changes this fails
// loudly rather than silently diverging. internal/ledger's production
// code never creates an asset.
func ensureAssetAtExponent(t *testing.T, pool *db.Pool, code, assetType string, exponent int16) {
	t.Helper()
	network := ""
	if assetType == "crypto" {
		network = "ledger-locked-split-test-network"
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
			// Migration 0047: a requester/approver must be linked to a
			// DISTINCT Person and be active, otherwise the same-person
			// half of four-eyes cannot be evaluated.
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
		// A previous abandoned run may have left a pending request for
		// this code; the apply-time consume picks the OLDEST pending
		// request, so a stale one would be matched against this insert.
		if _, err := tx.Exec(ctx,
			`UPDATE asset_change_requests SET state = 'cancelled' WHERE asset_code = $1 AND state = 'pending'`, code); err != nil {
			return err
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO asset_change_requests (id, operation, asset_code, payload, reason_code, requested_by_principal_id)
			 VALUES ($1, 'create', $2, $3::jsonb, 'ledger locked-origin split test asset', $4)`,
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

// lockedExponentCases is the exponent matrix §6.5.8 item 9 requires:
// exponent 0 (no minor units at all), the ordinary fiat exponent 2, and
// exponent 18 (the widest crypto case). §6.4.6 item 5 argues the locked
// split is exponent-independent; these cases prove it by execution.
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

// seedWalletForAsset creates one more wallet for the fixture's player in
// assetCode and returns its id (seedFixture itself only creates EUR).
func seedWalletForAsset(t *testing.T, pool *db.Pool, f fixture, assetCode string) uuid.UUID {
	t.Helper()
	if assetCode == "EUR" {
		return f.walletID
	}
	walletID := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, $5)`,
			walletID, f.tenantID, f.brandID, f.playerAccountID, assetCode)
		return err
	})
	if err != nil {
		t.Fatalf("seed %s wallet: %v", assetCode, err)
	}
	return walletID
}

// TestLedgerAccounts_BarePlayerLockedRejectedByCheckConstraint is §6.5.8
// item 1 and invariant L1 layer 1: after migration 0048, an
// origin-indeterminate locked account cannot be created at all - not
// "is not created by any current code path", but cannot be created, by
// the database, with SQLSTATE 23514. The insert is issued as raw SQL
// precisely because no Go code path can express it any more (L1 layer 2).
func TestLedgerAccounts_BarePlayerLockedRejectedByCheckConstraint(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_accounts (id, tenant_id, wallet_id, account_type, asset_code)
			 VALUES ($1, $2, $3, 'player_locked', 'EUR')`,
			uuid.New(), f.tenantID, f.walletID)
		return err
	})
	if err == nil {
		t.Fatal("bare player_locked was accepted; migration 0048's CHECK constraint is not in force (invariant L1 layer 1)")
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("expected a PostgreSQL error, got %T: %v", err, err)
	}
	if pgErr.Code != pgCheckViolation {
		t.Fatalf("expected SQLSTATE %s (check violation), got %s: %v", pgCheckViolation, pgErr.Code, err)
	}
	if pgErr.ConstraintName != accountTypeConstraint {
		t.Fatalf("rejection came from constraint %q, expected %q - the right constraint must be the one refusing this",
			pgErr.ConstraintName, accountTypeConstraint)
	}

	// Control, same statement shape: both replacement values ARE
	// accepted, so the test above proves a targeted removal rather than a
	// broken insert.
	walletID := seedWalletForAsset(t, pool, f, "USD")
	for _, accountType := range []AccountType{AccountPlayerLockedCash, AccountPlayerLockedBonus} {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO ledger_accounts (id, tenant_id, wallet_id, account_type, asset_code)
				 VALUES ($1, $2, $3, $4, 'USD')`,
				uuid.New(), f.tenantID, walletID, accountType)
			return err
		})
		if err != nil {
			t.Fatalf("account type %q must be accepted after migration 0048: %v", accountType, err)
		}
	}
}

// TestGetOrCreateAccount_LockedSplitTypesIdempotentAtEveryExponent is
// §6.5.8 item 2, repeated across item 9's exponent matrix. The partial
// unique index idx_ledger_accounts_wallet_type_asset (migration 0020,
// unchanged by 0048) is re-proved for both new values: a second call in a
// SEPARATE transaction returns the same account id and leaves exactly one
// row, with no check-then-insert anywhere.
func TestGetOrCreateAccount_LockedSplitTypesIdempotentAtEveryExponent(t *testing.T) {
	pool := testPool(t)
	cases := lockedExponentCases(t, pool)
	f := seedFixture(t, pool)

	for _, c := range cases {
		walletID := seedWalletForAsset(t, pool, f, c.asset)
		for _, accountType := range []AccountType{AccountPlayerLockedCash, AccountPlayerLockedBonus} {
			var first, second uuid.UUID
			for i, target := range []*uuid.UUID{&first, &second} {
				err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					id, err := GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, accountType, c.asset)
					*target = id
					return err
				})
				if err != nil {
					t.Fatalf("%s/%s call %d: %v", c.asset, accountType, i+1, err)
				}
			}
			if first != second {
				t.Fatalf("%s/%s (exponent %d): second GetOrCreateAccount returned a different id (%s vs %s)",
					c.asset, accountType, c.exponent, first, second)
			}
			var count int
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx,
					`SELECT count(*) FROM ledger_accounts WHERE wallet_id = $1 AND account_type = $2 AND asset_code = $3`,
					walletID, accountType, c.asset).Scan(&count)
			})
			if err != nil {
				t.Fatalf("count %s/%s rows: %v", c.asset, accountType, err)
			}
			if count != 1 {
				t.Fatalf("%s/%s (exponent %d): expected exactly 1 ledger_accounts row, got %d", c.asset, accountType, c.exponent, count)
			}
		}
	}
}

// TestPost_HR9BonusSetPostingsRejectedAndNothingWritten is §6.5.8 item 5
// and HR-9 (§6.5.7). Both BONUS_SET members are required to be blocked:
// player_locked_bonus (which migration 0048 creates) and player_bonus
// (promoted from recommended to required at Stage 4H-B0-R7).
//
// The surrounding transaction is deliberately COMMITTED after the
// rejected Post, so this proves "nothing was written" rather than merely
// "the caller's rollback undid it" - a guard that wrote a
// ledger_transactions row and then relied on the caller rolling back
// would pass the weaker test and fail this one.
func TestPost_HR9BonusSetPostingsRejectedAndNothingWritten(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// Account CREATION is unaffected by HR-9 - only posting is blocked.
	// A minted-but-never-posted-to account holds no value.
	var bonusID, lockedBonusID, lockedCashID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if bonusID, err = GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, AccountPlayerBonus, "EUR"); err != nil {
			return err
		}
		if lockedBonusID, err = GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, AccountPlayerLockedBonus, "EUR"); err != nil {
			return err
		}
		lockedCashID, err = GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, AccountPlayerLockedCash, "EUR")
		return err
	})
	if err != nil {
		t.Fatalf("create bonus-family accounts: %v", err)
	}

	// Fund the wallet so the rejected postings are otherwise entirely
	// well-formed (balanced, positive, idempotency-keyed): the ONLY
	// reason each must fail is HR-9.
	mustPost(t, pool, f, depositInput(f, "hr9-funding", 10_000))

	for _, c := range []struct {
		name      string
		accountID uuid.UUID
		wantType  string
	}{
		{"player_locked_bonus", lockedBonusID, "player_locked_bonus"},
		{"player_bonus", bonusID, "player_bonus"},
	} {
		key := "hr9-" + c.name
		var postErr error
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, postErr = Post(ctx, tx, TransactionInput{
				TenantID:        f.tenantID,
				TransactionType: TxManualAdjustment,
				IdempotencyKey:  key,
				CorrelationID:   uuid.New(),
				ReasonCode:      strPtr("hr9_guard_test"),
				Entries: []EntryInput{
					{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 500},
					{LedgerAccountID: c.accountID, Direction: Credit, Amount: 500},
				},
			})
			return nil // commit, so the assertions below are about durable state
		})
		if err != nil {
			t.Fatalf("%s: transaction wrapper failed: %v", c.name, err)
		}
		if !errors.Is(postErr, ErrBonusPostingBlocked) {
			t.Fatalf("%s: expected ErrBonusPostingBlocked, got %v", c.name, postErr)
		}
		// HR-9's error must NAME its own precondition, so a Bonus
		// developer who hits it is told what to build (§6.5.7).
		for _, want := range []string{c.wantType, "bonus_expense", "mirror generator"} {
			if !strings.Contains(postErr.Error(), want) {
				t.Fatalf("%s: HR-9 error must mention %q, got: %v", c.name, want, postErr)
			}
		}

		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var txCount, entryCount int
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND idempotency_key = $2`,
				f.tenantID, key).Scan(&txCount); err != nil {
				return err
			}
			if txCount != 0 {
				t.Fatalf("%s: HR-9 rejection left %d ledger_transactions row(s) behind", c.name, txCount)
			}
			if err := tx.QueryRow(ctx,
				`SELECT count(*) FROM ledger_entries WHERE ledger_account_id = $1`, c.accountID).Scan(&entryCount); err != nil {
				return err
			}
			if entryCount != 0 {
				t.Fatalf("%s: HR-9 rejection left %d ledger_entries row(s) behind", c.name, entryCount)
			}
			projected, err := GetProjectedBalance(ctx, tx, c.accountID)
			if err != nil {
				return err
			}
			if projected.Found {
				t.Fatalf("%s: HR-9 rejection created a balance projection row (%+v)", c.name, projected)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s: verify nothing written: %v", c.name, err)
		}
	}

	// The cash-origin half of the same family is NOT blocked: the guard
	// must be exactly as wide as BONUS_SET and no wider, or it would
	// silently disable the capability migration 0048 exists to add.
	mustPost(t, pool, f, TransactionInput{
		TenantID:        f.tenantID,
		TransactionType: TxManualAdjustment,
		IdempotencyKey:  "hr9-control-cash-lock",
		CorrelationID:   uuid.New(),
		ReasonCode:      strPtr("hr9_guard_control"),
		Entries: []EntryInput{
			{LedgerAccountID: f.cashAccountID, Direction: Debit, Amount: 500},
			{LedgerAccountID: lockedCashID, Direction: Credit, Amount: 500},
		},
	})
}

// TestPost_CashOriginLockPostsAndRebuildsAtEveryExponent proves the one
// behavioral capability migration 0048 adds - a cash-origin lock is
// postable and accounted exactly - across §6.5.8 item 9's exponent
// matrix, and that the projection and a from-ledger recomputation agree
// field-by-field for the new account type (the account-level form of
// item 8's "opaque pass-through" claim; the tenant-wide sweep itself is
// in internal/wallet's file, which is where the reconciliation import
// can live without an import cycle).
//
// NOTE ON TRANSACTION TYPE: this posts manual_adjustment, not
// sportsbook_bet. There is no sportsbook_* transaction_type at HEAD and
// migration 0048 deliberately does not add one (§6.5.2/§6.5.6); cases
// A/D/F/H/J/K remain NOT IMPLEMENTED as postings. This test exercises the
// generic ledger-layer capability only, and must not be read as a
// sportsbook lock/unlock implementation.
func TestPost_CashOriginLockPostsAndRebuildsAtEveryExponent(t *testing.T) {
	pool := testPool(t)
	cases := lockedExponentCases(t, pool)
	f := seedFixture(t, pool)

	// 9007199254740993 is 2^53+1, the smallest positive integer float64
	// cannot represent: any float anywhere on this path would return
	// ...992 instead (CLAUDE.md's no-floating-point rule).
	const deposit int64 = 9007199254740993
	const locked int64 = 4503599627370497

	for _, c := range cases {
		walletID := seedWalletForAsset(t, pool, f, c.asset)
		var cashID, clearingID, lockedCashID uuid.UUID
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			if cashID, err = GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, AccountPlayerCash, c.asset); err != nil {
				return err
			}
			if clearingID, err = GetOrCreateAccount(ctx, tx, f.tenantID, nil, AccountPSPClearing, c.asset); err != nil {
				return err
			}
			lockedCashID, err = GetOrCreateAccount(ctx, tx, f.tenantID, &walletID, AccountPlayerLockedCash, c.asset)
			return err
		})
		if err != nil {
			t.Fatalf("%s: seed accounts: %v", c.asset, err)
		}

		mustPost(t, pool, f, assetDepositInput(f, "lock-dep-"+c.asset, clearingID, cashID, deposit))
		mustPost(t, pool, f, TransactionInput{
			TenantID:        f.tenantID,
			TransactionType: TxManualAdjustment,
			IdempotencyKey:  "lock-move-" + c.asset,
			CorrelationID:   uuid.New(),
			ReasonCode:      strPtr("locked_origin_split_capability_test"),
			Entries: []EntryInput{
				{LedgerAccountID: cashID, Direction: Debit, Amount: locked},
				{LedgerAccountID: lockedCashID, Direction: Credit, Amount: locked},
			},
		})

		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			for _, want := range []struct {
				name      string
				accountID uuid.UUID
				signed    int64
				accountT  AccountType
			}{
				{"cash", cashID, deposit - locked, AccountPlayerCash},
				{"locked_cash", lockedCashID, locked, AccountPlayerLockedCash},
			} {
				projected, err := GetProjectedBalance(ctx, tx, want.accountID)
				if err != nil {
					return err
				}
				if !projected.Found {
					t.Fatalf("%s/%s: no projection row", c.asset, want.name)
				}
				if projected.Signed() != want.signed {
					t.Fatalf("%s/%s (exponent %d): expected balance %d, got %d",
						c.asset, want.name, c.exponent, want.signed, projected.Signed())
				}
				if projected.AccountType != want.accountT {
					t.Fatalf("%s/%s: projection carries account_type %q, expected %q",
						c.asset, want.name, projected.AccountType, want.accountT)
				}
				rebuilt, err := RebuildBalance(ctx, tx, want.accountID)
				if err != nil {
					return err
				}
				if rebuilt.AssetCode != projected.AssetCode || rebuilt.AccountType != projected.AccountType ||
					rebuilt.DebitTotal != projected.DebitTotal || rebuilt.CreditTotal != projected.CreditTotal {
					t.Fatalf("%s/%s: ledger-derived balance %+v differs from projection %+v",
						c.asset, want.name, rebuilt, projected)
				}
			}

			// SUM(DEBITS) == SUM(CREDITS) for this asset, including the
			// new account type, read straight from the entries
			// (CLAUDE.md's standing invariant).
			var debits, credits int64
			if err := tx.QueryRow(ctx,
				`SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
				        COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
				 FROM ledger_entries WHERE tenant_id = $1 AND asset_code = $2`,
				f.tenantID, c.asset).Scan(&debits, &credits); err != nil {
				return err
			}
			if debits != credits {
				t.Fatalf("%s: SUM(DEBITS)=%d != SUM(CREDITS)=%d", c.asset, debits, credits)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("%s: verify: %v", c.asset, err)
		}
	}
}
