//go:build integration

// R-5 (ADR 0095 section 42.5; raise only): a success whose amount/asset is MISMATCHED, arriving by
// callback on an already-DECLINED payout (the hold is already released: a double-payout candidate),
// used to write only payments.callback_amount_asset_mismatch_terminal and no page. It now also
// raises ONE durable P1 (reason mismatched_success_on_declined_payout) as the last statement of
// the cell. No state change, no release, no settlement, no posting, no delivery.
package payments

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

const r5Terminal = "payments.callback_amount_asset_mismatch_terminal"

// r5Callback applies a payout success callback with an explicit amount and asset, exactly like the
// webhook handler does (inside alerting.InTx, flushed after the commit).
func (e rbEnv) r5Callback(a PaymentAttempt, ref string, amount int64, asset string) error {
	_, err := e.r5CallbackDisp(a, ref, amount, asset)
	return err
}

func (e rbEnv) r5CallbackDisp(a PaymentAttempt, ref string, amount int64, asset string) (ReceiptDisposition, error) {
	var disp ReceiptDisposition
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(e.pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disp, err = ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.pid, ReceiptEvidence{
			EventType: "payout", ProviderReference: ref, MerchantReference: a.MerchantReference,
			Outcome: OutcomeSucceeded, Amount: amount, AssetCode: asset,
		})
		return err
	})
	if err == nil {
		pending.Flush(context.Background())
	}
	return disp, err
}

func (e rbEnv) r5Declined(t *testing.T, key string) (PaymentAttempt, b12Before, b12Out) {
	t.Helper()
	wr, a := e.claim(t, key)
	e.b12Decline(t, wr, a)
	before := e.b12Snapshot(t, wr)
	return a, before, b12Out{wr: wr, a: a, before: before}
}

// r5AssertStillDeclined: nothing but the audit/alert rows changed.
func (e rbEnv) r5AssertStillDeclined(t *testing.T, o b12Out) {
	t.Helper()
	got := mustGetAttempt(t, e.pool, e.f.tenantID, o.a.ID)
	if got.State != AttemptDeclined || got.TerminalReason != nil {
		t.Fatalf("R-5 is raise-only: the attempt must stay declined with no terminal reason, got %s %v", got.State, got.TerminalReason)
	}
	e.b12AssertNoMoneyMoved(t, o)
}

func TestR5_MismatchedSuccessOnDeclinedPayout_RaisesOneP1_NoStateChange(t *testing.T) {
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
			pid := fmt.Sprintf("mock-r5-%d", i)
			f, orch, _ := fpOrch(t, pool, pid)
			e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
			other, _, _ := fpOrch(t, pool, pid+"-other")
			a, _, o := e.r5Declined(t, "r5-"+c.name)
			ref := "r5-ref-" + c.name

			disp, err := e.r5CallbackDisp(a, ref, c.amount, c.asset)
			if err != nil {
				t.Fatalf("callback: %v", err)
			}
			// Unchanged: the cell reports "no effect" (no state change), as before R-5.
			if disp != DispositionDuplicateEffect {
				t.Fatalf("disposition = %s, want %s (raise only, no effect)", disp, DispositionDuplicateEffect)
			}
			e.r5AssertStillDeclined(t, o)
			e.b12AssertOneAlert(t, a.ID, alertReasonPayoutMismatchedSuccessOnDeclined, ref, "USD")
			// The pre-existing terminal-mismatch audit is unchanged (one row, amounts in audit only);
			// the dispute audit of the parking cells is NOT written (nothing was parked).
			if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 1 {
				t.Fatalf("terminal mismatch audit = %d, want 1", n)
			}
			if n := e.r8DisputeAudits(t, a.ID); n != 0 {
				t.Fatalf("no dispute audit: nothing is parked, got %d", n)
			}
			if rows := alertinject.ForSubject(t, pool, other.tenantID); len(rows) != 0 {
				t.Fatalf("another tenant must see no alert: %+v", rows)
			}
		})
	}
}

// Idempotence: the byte-identical redelivery is deduped at the receipt (no second receipt row) and
// the cell is the same no-state-change cell again; the alert stays ONE open row (the discriminator
// is the attempt and the closed reason), its occurrence count growing exactly as for any repeated
// B12 raise. No state change, no money, no extra alert row.
func TestR5_Replay_OneAlertRow_OccurrencesGrow_NoStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-r5-replay")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-r5-replay"}
	a, _, o := e.r5Declined(t, "r5-replay")
	for i := 0; i < 3; i++ {
		if err := e.r5Callback(a, "r5-replay-ref", 499, "EUR"); err != nil {
			t.Fatalf("delivery #%d: %v", i, err)
		}
	}
	rows := e.b12Rows(t)
	if len(rows) != 1 || rows[0].State != "open" || rows[0].Occurrences != 3 ||
		!strings.HasSuffix(rows[0].Discriminator, ":reason:"+alertReasonPayoutMismatchedSuccessOnDeclined) {
		t.Fatalf("want ONE open alert whose occurrences grew to 3, got %+v", rows)
	}
	if e.r8Events(t) != 1 {
		t.Fatalf("the receipt must be deduped, got %d rows", e.r8Events(t))
	}
	e.r5AssertStillDeclined(t, o)
}

// A MATCHING success after a decline is still T14 (disputed + its own alert reason); R-5 never
// fires for it. A mismatched success on a payout that is not declined keeps its existing cell.
func TestR5_MatchingSuccessAfterDecline_IsStillT14_NotR5(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f, orch, _ := fpOrch(t, pool, "mock-r5-t14")
	e := rbEnv{pool: pool, f: f, orch: orch, pid: "mock-r5-t14"}
	a, _, _ := e.r5Declined(t, "r5-t14")
	if err := e.r5Callback(a, "r5-t14-ref", 500, "EUR"); err != nil {
		t.Fatalf("callback: %v", err)
	}
	if got := mustGetAttempt(t, pool, f.tenantID, a.ID); got.State != AttemptDisputed {
		t.Fatalf("a matching success after a decline is T14 (disputed), got %s", got.State)
	}
	e.b12AssertOneAlert(t, a.ID, "success_after_payout_declined")
	if n := e.b12AuditCount(t, r5Terminal, a.ID); n != 0 {
		t.Fatalf("T14 does not write the terminal-mismatch audit, got %d", n)
	}
}

// Failure semantics (ADR 0102 7.2/7.3, as B12): a deterministic alert failure keeps the audit row and
// the delivery succeeds; the in-tx failure is recovered by the detached post-commit retry; a
// transient failure propagates and rolls back the audit row and the receipt, and the redelivery
// converges to one audit row and one alert.
func TestR5_AlertFailureSemantics(t *testing.T) {
	pool := depositV2ScratchPool(t)
	n := 0
	mk := func(t *testing.T) (rbEnv, PaymentAttempt, b12Out) {
		n++
		pid := fmt.Sprintf("mock-r5-f-%d", n)
		f, orch, _ := fpOrch(t, pool, pid)
		e := rbEnv{pool: pool, f: f, orch: orch, pid: pid}
		a, _, o := e.r5Declined(t, fmt.Sprintf("r5-f-%d", n))
		return e, a, o
	}
	t.Run("deterministic", func(t *testing.T) {
		e, a, o := mk(t)
		alertinject.Install(t, pool, e.f.tenantID, alertinject.Persistent, "P0001")
		if err := e.r5Callback(a, "r5-f-ref", 499, "EUR"); err != nil {
			t.Fatalf("a deterministic alert failure must not fail the delivery: %v", err)
		}
		if e.b12AuditCount(t, r5Terminal, a.ID) != 1 {
			t.Fatalf("the audit row must be committed")
		}
		if rows := e.b12Rows(t); len(rows) != 0 {
			t.Fatalf("no alert is visible after a persistent failure: %+v", rows)
		}
		e.r5AssertStillDeclined(t, o)
	})
	t.Run("in_tx_failure_detached_retry_persists", func(t *testing.T) {
		e, a, o := mk(t)
		alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")
		if err := e.r5Callback(a, "r5-f-ref", 499, "EUR"); err != nil {
			t.Fatalf("callback: %v", err)
		}
		e.b12AssertOneAlert(t, a.ID, alertReasonPayoutMismatchedSuccessOnDeclined)
		e.r5AssertStillDeclined(t, o)
	})
	t.Run("transient_rolls_back_then_converges", func(t *testing.T) {
		e, a, o := mk(t)
		t.Run("faulty", func(t *testing.T) {
			alertinject.Install(t, pool, e.f.tenantID, alertinject.Persistent, "40P01")
			if err := e.r5Callback(a, "r5-f-ref", 499, "EUR"); err == nil {
				t.Fatalf("a transient alert failure must propagate")
			}
			if e.b12AuditCount(t, r5Terminal, a.ID) != 0 || e.r8Events(t) != 0 {
				t.Fatalf("audit row and receipt must roll back with the delivery")
			}
		})
		if err := e.r5Callback(a, "r5-f-ref", 499, "EUR"); err != nil {
			t.Fatalf("redelivery: %v", err)
		}
		if e.b12AuditCount(t, r5Terminal, a.ID) != 1 {
			t.Fatalf("one audit row after recovery")
		}
		e.b12AssertOneAlert(t, a.ID, alertReasonPayoutMismatchedSuccessOnDeclined)
		e.r5AssertStillDeclined(t, o)
	})
}
