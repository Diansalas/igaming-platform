//go:build integration

// PAY-PAYOUT-CALLBACK-AUDIT-2 (ADR 0095 section 42.8; security F-2, ledger-finance L-3): the
// no-state-change terminal mismatch audit rows (payments.callback_amount_asset_mismatch_terminal on a
// DECLINED and on a SUCCEEDED attempt, and the M-1 foreign-reference row) are written once per NEW
// receipt, never again per redelivery (the L-e precedent). The raise stays unconditional (the alert
// dedupes). These tests add the two-goroutine concurrency cover for the audited cells and R-5/R-6/M-1.
package payments

import (
	"fmt"
	"sync"
	"testing"
)

// runTwo runs f(0) and f(1) concurrently (released together) and fails on any error.
func runTwo(t *testing.T, f func(i int) error) {
	t.Helper()
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = f(i)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent delivery #%d: %v", i, err)
		}
	}
}

// R-5 (declined), R-6 (succeeded) and M-1 (foreign reference): two goroutines deliver the SAME
// mismatched event. Exactly ONE audit row (the first, new receipt), exactly ONE alert row (the
// raise is unconditional, so its occurrences count reaches 2), one receipt row, no state change.
func TestCallbackAudit2_Concurrent_IdenticalDeliveries_OneAuditRow_OneAlertRow_SignalCells(t *testing.T) {
	pool := depositV2ScratchPool(t)
	type cell struct {
		name   string
		action string
		reason string
		setup  func(t *testing.T, e rbEnv, key string) (PaymentAttempt, b12Out)
		ref    func(key string) string
		amount int64
		still  func(e rbEnv, t *testing.T, o b12Out)
	}
	cells := []cell{
		{"R5_declined_mismatch", r5Terminal, alertReasonPayoutMismatchedSuccessOnDeclined,
			func(t *testing.T, e rbEnv, key string) (PaymentAttempt, b12Out) {
				a, _, o := e.r5Declined(t, key)
				return a, o
			}, func(k string) string { return k + "-ref" }, 499, rbEnv.r5AssertStillDeclined},
		{"R6_succeeded_mismatch", r5Terminal, alertReasonPayoutMismatchedSuccessOnSucceeded,
			func(t *testing.T, e rbEnv, key string) (PaymentAttempt, b12Out) {
				return e.r6Succeeded(t, key, key+"-ref")
			},
			func(k string) string { return k + "-ref" }, 499, rbEnv.r6AssertStillSucceeded},
		{"M1_succeeded_foreign_ref", auditActionPayoutSucceededForeignRef, alertReasonPayoutForeignRefSuccessOnSucceeded,
			func(t *testing.T, e rbEnv, key string) (PaymentAttempt, b12Out) {
				return e.r6Succeeded(t, key, key+"-ref")
			},
			func(k string) string { return k + "-foreign" }, 500, rbEnv.r6AssertStillSucceeded},
	}
	n := 0
	for _, c := range cells {
		t.Run(c.name, func(t *testing.T) {
			for rep := 0; rep < 3; rep++ {
				n++
				pid := fmt.Sprintf("mock-ca2-sig-%d", n)
				f, orch, _ := fpOrch(t, pool, pid)
				e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
				key := fmt.Sprintf("ca2-sig-%d", n)
				a, o := c.setup(t, e, key)
				base := e.r8Events(t)
				runTwo(t, func(int) error { return e.r5Callback(a, c.ref(key), c.amount, "EUR") })

				if got := e.b12AuditCount(t, c.action, a.ID); got != 1 {
					t.Fatalf("rep %d: audit rows = %d, want exactly 1 for two identical concurrent deliveries", rep, got)
				}
				rows := e.b12Rows(t)
				if len(rows) != 1 || rows[0].State != "open" || rows[0].Occurrences != 2 {
					t.Fatalf("rep %d: want ONE open alert row (raise is unconditional: occurrences 2), got %+v", rep, rows)
				}
				if want := "payout_attempt:" + a.ID.String() + ":reason:" + c.reason; rows[0].Discriminator != want {
					t.Fatalf("rep %d: discriminator %s, want %s", rep, rows[0].Discriminator, want)
				}
				if got := e.r8Events(t); got != base+1 {
					t.Fatalf("rep %d: the receipt must be deduped to one row: %d -> %d", rep, base, got)
				}
				c.still(e, t, o)
			}
		})
	}
}

// The audited dispute cells (T14, T15, callback amount/asset mismatch, tombstone): two goroutines
// deliver the same event, and (where the cell is reference-agnostic) two CONFLICTING events naming
// different references. Exactly one dispute, one dispute audit row and one alert row; the loser is a
// no-op cell. No money moves.
func TestCallbackAudit2_Concurrent_DisputeCells_OneAuditRow_OneAlertRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	for i, c := range r8Cells() {
		for _, conflicting := range []bool{false, true} {
			if conflicting && c.name == "tombstone_precedes_success" {
				continue // the tombstone is keyed on the one reference
			}
			t.Run(fmt.Sprintf("%s/conflicting=%v", c.name, conflicting), func(t *testing.T) {
				pid := fmt.Sprintf("mock-ca2-d-%d-%v", i, conflicting)
				f, orch, _ := fpOrch(t, pool, pid)
				e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
				wr, a := c.setup(t, e)
				snap := e.b12Snapshot(t, wr)
				runTwo(t, func(g int) error {
					ref, amount := c.ref, c.amount
					if conflicting && g == 1 {
						ref += "-other"
						if c.amount != 500 {
							amount = c.amount - 1 // still a mismatch for the mismatch cell
						}
					}
					return e.b12CallbackErr(a, OutcomeSucceeded, ref, amount)
				})
				got := mustGetAttempt(t, pool, f.tenantID, a.ID)
				if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != c.reason {
					t.Fatalf("want disputed/%s, got %s %v", c.reason, got.State, got.TerminalReason)
				}
				if n := e.r8DisputeAudits(t, a.ID); n != 1 {
					t.Fatalf("dispute audit rows = %d, want exactly 1", n)
				}
				e.b12AssertOneAlert(t, a.ID, c.reason)
				e.b12AssertNoMoneyMoved(t, b12Out{wr: wr, a: a, before: snap})
			})
		}
	}
}
