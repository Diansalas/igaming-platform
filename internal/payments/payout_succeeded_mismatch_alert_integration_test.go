//go:build integration

// R-6 (ADR 0095 section 42.8; raise only): a success whose amount/asset is MISMATCHED, arriving by
// callback on an already-SUCCEEDED payout, used to write only
// payments.callback_amount_asset_mismatch_terminal and no page. It now also raises ONE durable P1
// (reason mismatched_success_on_succeeded_payout) as the last statement of the cell, the same
// pattern as R-5. No state change, no release, no settlement, no posting, no delivery.
package payments

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

func alertinjectForOther(t *testing.T, e rbEnv, other uuid.UUID) int {
	t.Helper()
	return len(alertinject.ForSubject(t, e.pool, other))
}

func alertinjectInstall(t *testing.T, e rbEnv, code string, persistent bool) {
	t.Helper()
	mode := alertinject.InTxOnly
	if persistent {
		mode = alertinject.Persistent
	}
	alertinject.Install(t, e.pool, e.f.tenantID, mode, code)
}

// r6Succeeded claims a payout and settles it with a MATCHING success callback (500 EUR, ref).
func (e rbEnv) r6Succeeded(t *testing.T, key, ref string) (PaymentAttempt, b12Out) {
	t.Helper()
	wr, a := e.claim(t, key)
	e.b12Callback(t, a, OutcomeSucceeded, ref, 500)
	if got := mustGetAttempt(t, e.pool, e.f.tenantID, a.ID); got.State != AttemptSucceeded {
		t.Fatalf("setup: want succeeded, got %s", got.State)
	}
	return a, b12Out{wr: wr, a: a, before: e.b12Snapshot(t, wr)}
}

// r6AssertStillSucceeded: nothing but the audit/alert rows changed.
func (e rbEnv) r6AssertStillSucceeded(t *testing.T, o b12Out) {
	t.Helper()
	got := mustGetAttempt(t, e.pool, e.f.tenantID, o.a.ID)
	if got.State != AttemptSucceeded || got.TerminalReason != nil {
		t.Fatalf("R-6 is raise-only: the attempt must stay succeeded with no terminal reason, got %s %v", got.State, got.TerminalReason)
	}
	e.b12AssertNoMoneyMoved(t, o)
}

func TestR6_MismatchedSuccessOnSucceededPayout_RaisesOneP1_NoStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	for i, c := range []struct {
		name   string
		amount int64
		asset  string
	}{
		{"amount_only", 499, "EUR"},
		{"asset_only", 500, "USD"},
		{"both", 1, "USD"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pid := fmt.Sprintf("mock-r6-%d", i)
			f, orch, _ := fpOrch(t, pool, pid)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
			other, _, _ := fpOrch(t, pool, pid+"-other")
			ref := "r6-ref-" + c.name
			a, o := e.r6Succeeded(t, "r6-"+c.name, ref)
			if rows := e.b12Rows(t); len(rows) != 0 {
				t.Fatalf("a matching settlement raises nothing: %+v", rows)
			}

			disp, err := e.r5CallbackDisp(a, ref, c.amount, c.asset)
			if err != nil {
				t.Fatalf("callback: %v", err)
			}
			if disp != DispositionDuplicateEffect {
				t.Fatalf("disposition = %s, want %s (raise only, no effect)", disp, DispositionDuplicateEffect)
			}
			e.r6AssertStillSucceeded(t, o)
			e.b12AssertOneAlert(t, a.ID, alertReasonPayoutMismatchedSuccessOnSucceeded, ref, "USD")
			if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 1 {
				t.Fatalf("terminal mismatch audit = %d, want 1", n)
			}
			if n := e.r8DisputeAudits(t, a.ID); n != 0 {
				t.Fatalf("no dispute audit: nothing is parked, got %d", n)
			}
			if rows := alertinjectForOther(t, e, other.tenantID); rows != 0 {
				t.Fatalf("another tenant must see no alert, got %d rows", rows)
			}
		})
	}
}

// A matching (ordinary) repeated success on a succeeded payout is a plain duplicate: no alert and
// no audit row, however often it is redelivered.
func TestR6_MatchingRepeatedSuccess_DoesNotRaise(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-r6-match")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-r6-match"}
	a, o := e.r6Succeeded(t, "r6-match", "r6-match-ref")
	for i := 0; i < 3; i++ {
		disp, err := e.r5CallbackDisp(a, "r6-match-ref", 500, "EUR")
		if err != nil {
			t.Fatalf("delivery #%d: %v", i, err)
		}
		if disp != DispositionDuplicateEffect {
			t.Fatalf("delivery #%d: disposition = %s, want %s", i, disp, DispositionDuplicateEffect)
		}
	}
	if rows := e.b12Rows(t); len(rows) != 0 {
		t.Fatalf("a matching repeated success must not raise: %+v", rows)
	}
	if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 0 {
		t.Fatalf("a matching repeated success writes no mismatch audit, got %d", n)
	}
	e.r6AssertStillSucceeded(t, o)
}

// Idempotence: three byte-identical mismatched deliveries leave ONE open alert whose occurrences
// grow to 3; the receipt is deduped, the audit row is written once (PAY-PAYOUT-CALLBACK-AUDIT-2), nothing else changes.
func TestR6_Replay_OneAlertRow_OccurrencesGrow_NoStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-r6-replay")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-r6-replay"}
	a, o := e.r6Succeeded(t, "r6-replay", "r6-replay-ref")
	base := e.r8Events(t)
	for i := 0; i < 3; i++ {
		if err := e.r5Callback(a, "r6-replay-ref", 499, "EUR"); err != nil {
			t.Fatalf("delivery #%d: %v", i, err)
		}
		// PAY-PAYOUT-CALLBACK-AUDIT-2 (deliberate change from 1,2,3): one audit row for the first
		// (new) receipt, none per redelivery. The raise stays per delivery (occurrences grow).
		if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 1 {
			t.Fatalf("after delivery #%d: terminal-mismatch audit rows = %d, want 1", i, n)
		}
	}
	rows := e.b12Rows(t)
	if len(rows) != 1 || rows[0].State != "open" || rows[0].Occurrences != 3 ||
		!strings.HasSuffix(rows[0].Discriminator, ":reason:"+alertReasonPayoutMismatchedSuccessOnSucceeded) {
		t.Fatalf("want ONE open alert whose occurrences grew to 3, got %+v", rows)
	}
	if got := e.r8Events(t); got != base+1 {
		t.Fatalf("the mismatched receipt must be deduped to one row: %d -> %d", base, got)
	}
	e.r6AssertStillSucceeded(t, o)
}

// Failure semantics as R-5 (ADR 0102 7.2/7.3).
func TestR6_AlertFailureSemantics(t *testing.T) {
	pool := depositV2ScratchPool(t)
	n := 0
	mk := func(t *testing.T) (rbEnv, PaymentAttempt, b12Out, int) {
		n++
		pid := fmt.Sprintf("mock-r6-f-%d", n)
		f, orch, _ := fpOrch(t, pool, pid)
		e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
		a, o := e.r6Succeeded(t, fmt.Sprintf("r6-f-%d", n), "r6-f-ref")
		return e, a, o, e.r8Events(t)
	}
	t.Run("deterministic", func(t *testing.T) {
		e, a, o, _ := mk(t)
		alertinjectInstall(t, e, "P0001", true)
		if err := e.r5Callback(a, "r6-f-ref", 499, "EUR"); err != nil {
			t.Fatalf("a deterministic alert failure must not fail the delivery: %v", err)
		}
		if e.b12AuditCount(t, r5Terminal, a.ID) != 1 {
			t.Fatalf("the audit row must be committed")
		}
		if rows := e.b12Rows(t); len(rows) != 0 {
			t.Fatalf("no alert is visible after a persistent failure: %+v", rows)
		}
		e.r6AssertStillSucceeded(t, o)
	})
	t.Run("in_tx_failure_detached_retry_persists", func(t *testing.T) {
		e, a, o, _ := mk(t)
		alertinjectInstall(t, e, "P0001", false)
		if err := e.r5Callback(a, "r6-f-ref", 499, "EUR"); err != nil {
			t.Fatalf("callback: %v", err)
		}
		e.b12AssertOneAlert(t, a.ID, alertReasonPayoutMismatchedSuccessOnSucceeded)
		e.r6AssertStillSucceeded(t, o)
	})
	t.Run("transient_rolls_back_then_converges", func(t *testing.T) {
		e, a, o, base := mk(t)
		t.Run("faulty", func(t *testing.T) {
			alertinjectInstall(t, e, "40P01", true)
			if err := e.r5Callback(a, "r6-f-ref", 499, "EUR"); err == nil {
				t.Fatalf("a transient alert failure must propagate")
			}
			if e.b12AuditCount(t, r5Terminal, a.ID) != 0 || e.r8Events(t) != base {
				t.Fatalf("audit row and receipt must roll back with the delivery")
			}
		})
		if err := e.r5Callback(a, "r6-f-ref", 499, "EUR"); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		if e.b12AuditCount(t, r5Terminal, a.ID) != 1 {
			t.Fatalf("one audit row after recovery")
		}
		e.b12AssertOneAlert(t, a.ID, alertReasonPayoutMismatchedSuccessOnSucceeded)
		e.r6AssertStillSucceeded(t, o)
	})
}
