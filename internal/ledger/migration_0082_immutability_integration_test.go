//go:build integration

// Stage 9 (Production Readiness), migration 0082 section 1.6 / HR-15.
// Proves ledger_accounts_enforce_immutable_fields actually rejects a
// direct SQL UPDATE to a protected column - not that the Go layer happens
// never to issue one. CLAUDE.md's "Financial / ledger rules" require this
// to be a database fact ("enforced by PostgreSQL ... not by discipline in
// application code"), so the test drives raw SQL through an ordinary
// tenant-scoped connection, exactly like migration 0080's own
// casino_provider_rounds trigger test (internal/casino/
// provider_rounds_test.go) does.
//
// Why this table matters more than its quietness suggests: ledger_entries
// and ledger_transactions have been append-only since migrations 0021/
// 0022, so an attacker or a buggy migration cannot edit a posting - but
// until 0082 they COULD edit the account a posting points at. Flipping a
// ledger_account's account_type or wallet_id retroactively rewrites what
// every historical entry against it MEANS, while SUM(DEBITS) ==
// SUM(CREDITS) keeps holding perfectly and no immutable row is touched.
package ledger

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

func TestMigration0082_LedgerAccountsImmutableFields(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	// Each case is a column the trigger must freeze, with a SET clause
	// that genuinely changes it. cashAccountID is a player-owned account
	// (wallet_id NOT NULL, status NULL); clearingID is a house-level one
	// (wallet_id NULL, status NOT NULL) - both shapes are covered so a
	// future guard that accidentally only fires for one is caught.
	cases := []struct {
		name    string
		account uuid.UUID
		set     string
		arg     any
	}{
		{"account_type on a player account", f.cashAccountID, `account_type = 'house_gaming'`, nil},
		{"account_type on a house account", f.clearingID, `account_type = 'psp_reserve'`, nil},
		{"wallet_id", f.cashAccountID, `wallet_id = $2`, uuid.New()},
		{"wallet_id set to NULL", f.cashAccountID, `wallet_id = NULL`, nil},
		{"asset_code", f.cashAccountID, `asset_code = 'USD'`, nil},
		{"tenant_id", f.cashAccountID, `tenant_id = $2`, uuid.New()},
		{"player_account_id", f.cashAccountID, `player_account_id = $2`, uuid.New()},
		{"created_at", f.cashAccountID, `created_at = now() - interval '1 year'`, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				sql := `UPDATE ledger_accounts SET ` + tc.set + ` WHERE id = $1`
				if tc.arg != nil {
					_, err := tx.Exec(ctx, sql, tc.account, tc.arg)
					return err
				}
				_, err := tx.Exec(ctx, sql, tc.account)
				return err
			})
			if err == nil {
				t.Fatalf("expected ledger_accounts_immutable_fields to reject %q, got nil error", tc.set)
			}
			if !strings.Contains(err.Error(), "immutable after insert") {
				t.Fatalf("expected the trigger's own immutability message, got: %v", err)
			}
		})
	}

	// The row must be exactly as seeded - a trigger that raised but left a
	// partial change behind would be worse than no trigger.
	var acctType, assetCode string
	var walletID, tenantID, playerAccountID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT account_type, asset_code, wallet_id, tenant_id, player_account_id FROM ledger_accounts WHERE id = $1`,
			f.cashAccountID,
		).Scan(&acctType, &assetCode, &walletID, &tenantID, &playerAccountID)
	})
	if err != nil {
		t.Fatalf("re-read seeded account: %v", err)
	}
	if acctType != string(AccountPlayerCash) || walletID != f.walletID || tenantID != f.tenantID || playerAccountID != f.playerAccountID {
		t.Fatalf("seeded account was mutated despite the trigger: type=%s wallet=%s tenant=%s player=%s",
			acctType, walletID, tenantID, playerAccountID)
	}
}

// TestMigration0082_LedgerAccountsStatusStaysMutable is the other half of
// the guard: migration 0082 deliberately leaves `status` writable, because
// migration 0020 defines it as the house-level account's own lifecycle
// (active/frozen/closed) and freezing a compromised house account must
// stay possible. A trigger that froze the whole row would silently remove
// that control, so the allowance is asserted, not assumed.
func TestMigration0082_LedgerAccountsStatusStaysMutable(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE ledger_accounts SET status = 'frozen' WHERE id = $1`, f.clearingID)
		return err
	})
	if err != nil {
		t.Fatalf("expected freezing a house account to remain permitted, got: %v", err)
	}

	var status string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM ledger_accounts WHERE id = $1`, f.clearingID).Scan(&status)
	}); err != nil {
		t.Fatalf("re-read house account: %v", err)
	}
	if status != "frozen" {
		t.Fatalf("expected status 'frozen', got %q", status)
	}
}

// TestMigration0082_LedgerAccountsDenyTruncate proves the statement-level
// half of the guard. It is a SEPARATE trigger for a reason migration 0021
// already records: row-level triggers never fire on TRUNCATE at all, so
// the BEFORE UPDATE guard above would not stop a TRUNCATE from erasing
// every account the (immutable) ledger_entries rows point at.
//
// Runs on an isolated scratch database (internal/testsupport/scratchdb),
// NOT the shared testPool() database every other package's integration
// test also writes against - the same CI-331 lock-contention class
// (docs/plans/stage-10.3-planning/08-ci-331-lock-contention.md) applies
// here, arguably with a larger blast radius: TRUNCATE ledger_accounts
// CASCADE must take ACCESS EXCLUSIVE locks on ledger_accounts,
// ledger_entries AND wallet_balance_projection - tables every financial
// package's integration suite (casino, sportsbook, bonus, payments,
// reconciliation, risk) writes to concurrently. internal/jurisdiction's
// registry_rls_integration_test.go documents this exact mechanism
// (SQLSTATE 40P01, "deadlock detected") already having been observed live
// for its own tenants/licences/jurisdictions TRUNCATE tests, which is why
// those moved to a scratch database first.
func TestMigration0082_LedgerAccountsDenyTruncate(t *testing.T) {
	pool := ledgerImmutabilityScratchPool(t)
	f := seedFixture(t, pool)

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE ledger_accounts CASCADE`)
		return err
	})
	if err == nil {
		t.Fatal("expected ledger_accounts_no_truncate to reject TRUNCATE, got nil error")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected ledger_deny_mutation's own append-only message, got: %v", err)
	}
}

// ledgerImmutabilityScratchPool creates an isolated scratch database with
// the full migration chain applied, for TRUNCATE-class tests in this file
// that must not run against the shared database (see
// TestMigration0082_LedgerAccountsDenyTruncate's comment above).
func ledgerImmutabilityScratchPool(t *testing.T) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, "ledgerimm_truncate_")
	pool, err := db.Connect(context.Background(), url, 5, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), "../../migrations"); err != nil {
		t.Fatalf("migrate scratch database: %v", err)
	}
	return pool
}
