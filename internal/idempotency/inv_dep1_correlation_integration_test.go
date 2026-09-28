//go:build integration

// PAY-DOUBLE-CREDIT-1 / INV-DEP-1 (ADR 0095 §28.2's contract amendment;
// ledger-finance ruling docs/plans/payment-readiness/lf-q1-supersession.md
// §3(ii)/§5 item 4). Companion to
// internal/payments/inv_dep1_matrix_integration_test.go - see that file's
// header for the full mapping table and mutation checklist.
//
// TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse (this
// package's own integration_test.go, lines ~296-330) posts two DISTINCT
// occurrences - a deliberately "coincident payload" case - as two
// TxDeposit transactions sharing ONE correlation id. Once migration 0107
// adds `ledger_transactions_one_deposit_per_intent` (a partial unique
// index on (tenant_id, correlation_id) WHERE transaction_type='deposit'),
// that fixture becomes a genuine collision: for a real deposit,
// correlation_id IS deposit_intents.id (§28.2's binding contract
// amendment), so two distinct TxDeposit postings must never share one
// correlation id, no matter how the idempotency key is composed.
//
// The ledger-finance ruling is explicit that this is a FIXTURE bug, not
// an idempotency-composition one: "Give each occurrence its own
// correlation id. What that test pins is idempotency-key composition,
// not correlation sharing." This file adds the corrected version; it
// does not touch the original (invert, never delete - the implementer
// replaces the original when migration 0107 lands).
//
// This is a plain TxDeposit posting test, not the payments-orchestration
// choke point, so it is NOT expected to fail against HEAD today (there is
// no unique index yet to violate) - it exists so the corrected shape is
// already committed and reviewed before 0107 needs it, and so a future
// `git blame`/review on the ORIGINAL test finds this file immediately.
package idempotency

import (
	"testing"

	"github.com/google/uuid"
)

func TestIntegration_LegitimateSecondOccurrenceDoesNotCollapse_DistinctCorrelationIDs(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	ord1 := OccurrenceOrdinal(1)
	ord2 := OccurrenceOrdinal(2)
	composed1, err := ComposeOccurrenceKeyWithOrdinal("leg-settlement-ref", &ord1)
	if err != nil {
		t.Fatalf("compose 1: %v", err)
	}
	composed2, err := ComposeOccurrenceKeyWithOrdinal("leg-settlement-ref", &ord2)
	if err != nil {
		t.Fatalf("compose 2: %v", err)
	}
	if composed1 == composed2 {
		t.Fatal("two distinct ordinals over the same reference must compose differently")
	}

	// INV-DEP-1 (ADR 0095 §28.2): a real deposit's correlation_id IS the
	// intent id, so two genuinely distinct occurrences get two distinct
	// intent-shaped correlation ids - never the same one, unlike the
	// original fixture this replaces.
	corr1 := uuid.New()
	corr2 := uuid.New()

	// Identical payload (same amount) on purpose - the exact "coincident
	// payload" scenario the original ADR 0038 §14 gap named; only the
	// occurrence key AND the correlation id now distinguish the two.
	first := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, composed1, corr1, 300))
	second := mustPost(t, pool, f.tenantID, externalVehicleInput(f, f.tenantID, "mock-provider", f.cashAccountID, composed2, corr2, 300))

	if first.AlreadyPosted || second.AlreadyPosted {
		t.Fatalf("both occurrences should post as NEW transactions, got AlreadyPosted=%v/%v", first.AlreadyPosted, second.AlreadyPosted)
	}
	if first.TransactionID == second.TransactionID {
		t.Fatal("two distinct occurrences must never share a transaction id")
	}
	// Both amounts posted - 300 + 300 = 600, not absorbed as one.
	assertBalance(t, pool, f.tenantID, f.cashAccountID, 600)
}
