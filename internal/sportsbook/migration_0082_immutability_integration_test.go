//go:build integration

// Stage 9 (Production Readiness), migration 0082 section 1.1. Proves
// sportsbook_bets_enforce_immutable_fields actually rejects a direct SQL
// UPDATE to a protected column, and that its two deliberate allowances
// (a mutable `status`, a provider reference settable exactly once) behave
// as the migration documents them.
//
// What the trigger closes: sportsbook_bets' tenant_staff_scope RLS policy
// is FOR ALL, and before migration 0082 the table had no immutability
// trigger at all - so an UPDATE could retroactively rewrite stake_amount,
// the odds frozen at acceptance, or ledger_transaction_id AFTER the stake
// had already been posted to the append-only ledger. The ledger entry
// cannot be edited to match, so the result is a bet record and a ledger
// record that disagree, with nothing in either to show it happened.
//
// Mirrors migration 0080's own trigger test (internal/casino/
// provider_rounds_test.go) and this package's existing fixture helpers.
package sportsbook

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// seedPlacedBet returns the id of one genuinely placed bet.
func seedPlacedBet(t *testing.T, pool *db.Pool, f sbFixture, key string) uuid.UUID {
	t.Helper()
	sel := seedSelection(t, pool, seedSelectionParams{})
	fundWallet(t, pool, f, 100_000)
	result, err := placeBet(t, pool, f, sel, 5_000, key)
	if err != nil {
		t.Fatalf("seed placed bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("seed placed bet was rejected: %s/%s %s", result.RejectionCategory, result.RejectionCode, result.RejectionMessage)
	}
	return result.Bet.ID
}

func TestMigration0082_SportsbookBetsImmutableFields(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	betID := seedPlacedBet(t, pool, f, "mig0082-immutable")

	cases := []struct {
		name string
		set  string
		arg  any
	}{
		// The money columns. A retroactive stake change is the whole
		// reason this trigger exists: the ledger already locked the
		// original stake and cannot be edited to follow.
		{"stake_amount", `stake_amount = 1`, nil},
		{"odds_numerator", `odds_numerator = 9999`, nil},
		{"odds_denominator", `odds_denominator = 1`, nil},
		{"potential_return", `potential_return = 99999999`, nil},
		// Ownership/identity.
		{"tenant_id", `tenant_id = $2`, uuid.New()},
		{"brand_id", `brand_id = $2`, uuid.New()},
		{"player_account_id", `player_account_id = $2`, uuid.New()},
		{"wallet_id", `wallet_id = $2`, uuid.New()},
		{"selection_id", `selection_id = $2`, uuid.New()},
		{"asset_code", `asset_code = 'USD'`, nil},
		// Idempotency and the ledger link - rewriting either would let a
		// replayed bet be accepted a second time, or repoint an accepted
		// bet at someone else's posting.
		{"idempotency_key", `idempotency_key = 'hijacked'`, nil},
		{"ledger_transaction_id", `ledger_transaction_id = $2`, uuid.New()},
		{"placed_at", `placed_at = now() - interval '1 year'`, nil},
		// Stage 9.2 (ADR 0083 §5.2.4/§5.5, migration 0087): the resolved-
		// jurisdiction historical-stability snapshot joined this same
		// immutable-fields IF - INV-SB-JUR-6, TestSportsbookBet_
		// JurisdictionSnapshotIsImmutable's own dedicated regression test
		// covers the end-to-end scenario (a genuinely non-NULL snapshot);
		// this table-driven case proves the trigger's own raw-SQL rejection
		// symmetrically with every other frozen column, on the (today
		// always-NULL) fixture bet this test already seeds.
		{"jurisdiction_code", `jurisdiction_code = 'SOME-CODE'`, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				sql := `UPDATE sportsbook_bets SET ` + tc.set + ` WHERE id = $1`
				if tc.arg != nil {
					_, err := tx.Exec(ctx, sql, betID, tc.arg)
					return err
				}
				_, err := tx.Exec(ctx, sql, betID)
				return err
			})
			if err == nil {
				t.Fatalf("expected sportsbook_bets_immutable_fields to reject %q, got nil error", tc.set)
			}
			if !strings.Contains(err.Error(), "immutable after insert") {
				t.Fatalf("expected the trigger's own immutability message, got: %v", err)
			}
		})
	}

	// Nothing partially applied.
	var stake, oddsNum int64
	var idemKey string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT stake_amount, odds_numerator, idempotency_key FROM sportsbook_bets WHERE id = $1`, betID,
		).Scan(&stake, &oddsNum, &idemKey)
	}); err != nil {
		t.Fatalf("re-read bet: %v", err)
	}
	if stake != 5_000 || idemKey != "mig0082-immutable" {
		t.Fatalf("bet was mutated despite the trigger: stake=%d odds_num=%d key=%q", stake, oddsNum, idemKey)
	}
}

// TestMigration0082_SportsbookBetsStatusMovesOnlyThroughSettlement
// replaces TestMigration0082_SportsbookBetsStatusStaysMutable. Migration
// 0082 deliberately left status writable so a settlement stage could
// exist; migration 0091 (Stage 10 W1, ADR 0088 §3.3) then made status a
// derived cache of sportsbook_bet_settlements, guarded by T-2. So: a raw
// status UPDATE with no history behind it is rejected, and the same
// transition succeeds through the settlement path.
func TestMigration0082_SportsbookBetsStatusMovesOnlyThroughSettlement(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	betID := seedPlacedBet(t, pool, f, "mig0082-status-derived")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET status = 'settled_lost' WHERE id = $1`, betID)
		return err
	})
	if err == nil {
		t.Fatal("expected T-2 to reject a status UPDATE with no settlement history behind it")
	}

	actor := seedRiskManager(t, pool, f.tenantID)
	res := mustSimulate(t, pool, f.tenantID, settleEvent(betID, actor, 1, SettlementOutcomeLost, 0))
	if res.BetStatus != BetStatusSettledLost {
		t.Fatalf("expected settled_lost through the settlement path, got %q", res.BetStatus)
	}
}

// TestMigration0082_SportsbookBetsProviderReferenceWriteOnce covers the
// second allowance: provider_id/provider_bet_reference are NULL on every
// row today (migration 0081), so a future adapter must be able to record
// them once - but must never be able to repoint an already-accepted bet
// at a different provider reference. Same "mutable until first set, then
// frozen" idiom migration 0080 uses for provider_session_id.
func TestMigration0082_SportsbookBetsProviderReferenceWriteOnce(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	betID := seedPlacedBet(t, pool, f, "mig0082-provider-ref")

	// First write: permitted (migration 0081's symmetric-null CHECK means
	// the pair moves together).
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE sportsbook_bets SET provider_id = 'mock-sb', provider_bet_reference = 'ref-1' WHERE id = $1`, betID)
		return err
	}); err != nil {
		t.Fatalf("expected the first provider-reference write to be permitted, got: %v", err)
	}

	// Second, different write: rejected.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE sportsbook_bets SET provider_bet_reference = 'ref-2' WHERE id = $1`, betID)
		return err
	})
	if err == nil {
		t.Fatal("expected the trigger to reject repointing an already-set provider reference, got nil error")
	}
	if !strings.Contains(err.Error(), "immutable once set") {
		t.Fatalf("expected the trigger's own once-set message, got: %v", err)
	}

	// Re-writing the SAME value must still pass - an idempotent replay is
	// not a hijack, and the guard uses IS DISTINCT FROM precisely so it
	// does not turn a retry into a failure.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE sportsbook_bets SET provider_id = 'mock-sb', provider_bet_reference = 'ref-1' WHERE id = $1`, betID)
		return err
	}); err != nil {
		t.Fatalf("expected an identical-value rewrite (idempotent replay) to be permitted, got: %v", err)
	}
}

func TestMigration0082_SportsbookBetsDenyTruncate(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	seedPlacedBet(t, pool, f, "mig0082-truncate")

	// Since migration 0091, sportsbook_bet_settlements references
	// sportsbook_bets, so a plain TRUNCATE is refused by PostgreSQL's FK
	// check before any trigger runs. CASCADE gets past that check and
	// proves the deny trigger itself still fires.
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE sportsbook_bets`)
		return err
	})
	if err == nil {
		t.Fatal("expected a plain TRUNCATE of sportsbook_bets to be refused, got nil error")
	}
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE sportsbook_bets CASCADE`)
		return err
	})
	if err == nil {
		t.Fatal("expected sportsbook_bets_no_truncate to reject TRUNCATE ... CASCADE, got nil error")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected ledger_deny_mutation's own append-only message, got: %v", err)
	}
}
