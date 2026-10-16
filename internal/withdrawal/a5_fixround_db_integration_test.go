//go:build integration

// A5 fix round (SR5-1 / LF-A5-1, SR5-6) at the database: WD027 on the tombstone branch and the pinned
// search_path of the replaced governance function.
package withdrawal

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// WD027: a tombstone correlated to a withdrawal request (the reversal-before-completion branch) needs two
// distinct ACTIVE Persons, each holding payment_force_resolve:approve, on one reverse digest. Two finance
// Persons WITHOUT the grant are refused; a deposit-style tombstone (a fresh correlation id) is unaffected.
// (The granted path is tested end to end in internal/payments.)
func TestA5DB_WD027_PayoutTombstoneNeedsForceResolveGrantHolders(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	ref := "a5db-wd027-" + uuid.NewString()[:8]
	wr := a5Walk(t, f, 100, ref, false)
	s1, s2 := mustCreateApprover(t, pool, f.tenantID), mustCreateApprover(t, pool, f.tenantID)
	reason := ReversalReasonPrefix + strings.Repeat("27", 32)
	tomb := func(ctx context.Context, tx pgx.Tx, corr uuid.UUID, r string) error {
		pid := "a5psp"
		_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "tombstone:a5psp:" + r, ProviderID: &pid, ProviderTxID: &r, CorrelationID: corr})
		return err
	}
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := a5Vote(ctx, tx, wr, s1, reason); err != nil {
			return err
		}
		if err := a5Vote(ctx, tx, wr, s2, reason); err != nil {
			return err
		}
		return tomb(ctx, tx, wr.ID, "out-"+ref)
	})
	if got := a5Code(err); got != "WD027" {
		t.Fatalf("a payout tombstone without grant holders: want WD027, got %q (%v)", got, err)
	}
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tomb(ctx, tx, uuid.New(), "a5db-deposit-tomb-"+uuid.NewString())
	})
}

// SR5-6: the replaced withdrawal_approvals_enforce_governance pins its search_path (catalog pin), like every
// other A5 function.
func TestA5DB_FunctionsPinSearchPath(t *testing.T) {
	pool := testPool(t)
	for _, fn := range []string{"withdrawal_approvals_enforce_governance", "ledger_transactions_payout_tombstone_guard",
		"withdrawal_requests_state_transition_guard", "withdrawal_request_ledger_link_violation",
		"ledger_transactions_withdrawal_hold_leg_at_commit"} {
		var c []string
		if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COALESCE(proconfig, '{}') FROM pg_proc WHERE proname = $1`, fn).Scan(&c)
		}); err != nil {
			t.Fatal(err)
		}
		if strings.Join(c, ",") != "search_path=pg_catalog, public, pg_temp" {
			t.Fatalf("%s proconfig = %v, want the pinned search_path", fn, c)
		}
	}
}
