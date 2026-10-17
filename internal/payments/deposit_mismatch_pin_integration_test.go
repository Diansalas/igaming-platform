//go:build integration

// PAY-DEPOSIT-MISMATCH-ALERT-1 (ADR 0095 section 42.8; RAISE ONLY). This file used to be the guard test
// that PINNED the no-alert behaviour of a mismatched-amount success on an already-SUCCEEDED and on a
// DECLINED deposit (audit row only). It is DELIBERATELY CHANGED: the same scenarios now assert ONE
// durable P1 (payment.webhook_integrity, reason mismatched_success_on_{declined,succeeded}_deposit),
// still with no state change, no ledger effect and exactly one terminal-mismatch audit row. Every other
// assertion of the old pin (state unchanged, terminal reason nil, audit count 1, ledger balanced) is kept.
package payments

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

type dmEnv struct {
	pool *db.Pool
	f    orchFixture
	orch *Orchestrator
	pid  string
	att  PaymentAttempt
	ref  string
}

// dmSetup initiates a deposit of the given amount (a decline sentinel yields a declined attempt) and
// returns the env; the attempt state is left as the mock produced it.
func dmSetup(t *testing.T, pool *db.Pool, name string, amount int64) dmEnv {
	t.Helper()
	pid := "mock-psp-dm-" + name
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider(pid, "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{pid: provider}, MultiWebhookCredentialResolver{pid: NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{MarketGuard: testUngatedGuard,
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: "dm-" + name,
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	ref := "dm-ref-" + name
	if res.Attempt.ProviderReference != nil {
		ref = *res.Attempt.ProviderReference
	}
	return dmEnv{pool: pool, f: f, orch: orch, pid: pid, att: res.Attempt, ref: ref}
}

// deliver applies a provider success callback exactly like the webhook handler (alerting.InTx, flushed
// after the commit).
func (e dmEnv) deliver(t *testing.T, amount int64) ReceiptDisposition {
	t.Helper()
	d, err := e.deliverErr(amount)
	if err != nil {
		t.Fatalf("ApplyReceiptEvidence: %v", err)
	}
	return d
}

func (e dmEnv) deliverErr(amount int64) (ReceiptDisposition, error) {
	var disp ReceiptDisposition
	pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(e.pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		disp, err = ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.pid, ReceiptEvidence{
			EventType: "deposit", ProviderReference: e.ref, MerchantReference: e.att.MerchantReference,
			Outcome: OutcomeSucceeded, Amount: amount, AssetCode: "EUR",
		})
		return err
	})
	if err == nil {
		pending.Flush(context.Background())
	}
	return disp, err
}

func (e dmEnv) ledgerTxCount(t *testing.T) int {
	t.Helper()
	return fpCount(t, e.pool, e.f.tenantID, `SELECT count(*) FROM ledger_transactions WHERE tenant_id=$1`, e.f.tenantID)
}

func (e dmEnv) auditCount(t *testing.T) int {
	t.Helper()
	return fpCount(t, e.pool, e.f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id=$1 AND action=$2 AND target_id=$3`,
		e.f.tenantID, r5Terminal, e.att.ID.String())
}

func TestDepositMismatchedSuccess_OnSucceededAndDeclined_AuditAndOneP1_NoStateChange(t *testing.T) {
	pool := depositV2ScratchPool(t)
	for _, c := range []struct {
		name   string
		amount int64
		want   AttemptState
		reason string
	}{
		{"succeeded", 5000, AttemptSucceeded, alertReasonDepositMismatchedSuccessOnSucceeded},
		{"declined", MockAmountPlayerDeclineNoCascade, AttemptDeclined, alertReasonDepositMismatchedSuccessOnDeclined},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := dmSetup(t, pool, c.name, c.amount)
			if c.want == AttemptSucceeded {
				if d := e.deliver(t, c.amount); d != DispositionApplied {
					t.Fatalf("setup: matching success disposition = %s, want applied", d)
				}
			}
			if before := mustGetAttempt(t, pool, e.f.tenantID, e.att.ID); before.State != c.want {
				t.Fatalf("setup: state = %s, want %s", before.State, c.want)
			}
			if rows := alertinject.ForSubject(t, pool, e.f.tenantID); len(rows) != 0 {
				t.Fatalf("normal path (matching success / decline) must raise nothing, got %+v", rows)
			}
			txBefore := e.ledgerTxCount(t)

			e.deliver(t, c.amount+1)

			after := mustGetAttempt(t, pool, e.f.tenantID, e.att.ID)
			if after.State != c.want || after.TerminalReason != nil {
				t.Fatalf("no state change expected: %s %v", after.State, after.TerminalReason)
			}
			rows := alertinject.ForSubject(t, pool, e.f.tenantID)
			if len(rows) != 1 {
				t.Fatalf("want exactly ONE alert (PAY-DEPOSIT-MISMATCH-ALERT-1), got %+v", rows)
			}
			r := rows[0]
			if r.Kind != string(alerting.KindPaymentWebhookIntegrity) || r.Severity != "p1" || r.State != "open" || r.Occurrences != 1 {
				t.Fatalf("alert shape: %+v", r)
			}
			if want := "attempt:" + e.att.ID.String() + ":reason:" + c.reason; r.Discriminator != want {
				t.Fatalf("discriminator = %q, want %q", r.Discriminator, want)
			}
			// Codes/ids only: the sole attribute is the registered provider id; no amount, reference or PII.
			if len(r.Attributes) != 1 || r.Attributes["provider_id"] != e.pid {
				t.Fatalf("attributes must be provider_id only, got %+v", r.Attributes)
			}
			if n := e.auditCount(t); n != 1 {
				t.Fatalf("terminal mismatch audit rows = %d, want 1", n)
			}
			if got := e.ledgerTxCount(t); got != txBefore {
				t.Fatalf("raise-only: ledger transactions %d -> %d", txBefore, got)
			}
			assertLedgerBalanced(t, pool, e.f.tenantID)
		})
	}
}

// Duplicates: a byte-identical redelivery is deduped at the receipt (audit stays 1), the alert stays ONE
// open row (idempotent discriminator) with growing occurrences; a different mismatched amount for the
// same attempt and reason lands on the same row too.
func TestDepositMismatchedSuccess_Replays_OneAlertRow_OccurrencesGrow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	e := dmSetup(t, pool, "replay", MockAmountPlayerDeclineNoCascade)
	for i := 0; i < 3; i++ {
		e.deliver(t, MockAmountPlayerDeclineNoCascade+1)
	}
	e.deliver(t, MockAmountPlayerDeclineNoCascade+2)
	rows := alertinject.ForSubject(t, pool, e.f.tenantID)
	if len(rows) != 1 || rows[0].Occurrences != 4 || rows[0].State != "open" ||
		!strings.HasSuffix(rows[0].Discriminator, ":reason:"+alertReasonDepositMismatchedSuccessOnDeclined) {
		t.Fatalf("want ONE open alert with 4 occurrences, got %+v", rows)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, e.att.ID); got.State != AttemptDeclined {
		t.Fatalf("state = %s", got.State)
	}
}

// Benign duplicates raise nothing: a byte-identical redelivery of the MATCHING success on a succeeded
// deposit, and a redelivered decline on a declined deposit.
func TestDepositBenignDuplicates_RaiseNoAlert(t *testing.T) {
	pool := depositV2ScratchPool(t)
	e := dmSetup(t, pool, "benign", 5000)
	e.deliver(t, 5000)
	if d := e.deliver(t, 5000); d != DispositionApplied && d != DispositionDuplicateEffect {
		t.Fatalf("matching redelivery disposition = %s", d)
	}
	if rows := alertinject.ForSubject(t, pool, e.f.tenantID); len(rows) != 0 {
		t.Fatalf("a benign duplicate success must raise nothing, got %+v", rows)
	}
	d := dmSetup(t, pool, "benign-declined", MockAmountPlayerDeclineNoCascade)
	if rows := alertinject.ForSubject(t, pool, d.f.tenantID); len(rows) != 0 {
		t.Fatalf("a plain declined deposit must raise nothing, got %+v", rows)
	}
}

// Concurrency: parallel mismatched deliveries (distinct amounts so none is receipt-deduped) leave ONE
// open alert row whose occurrences equal the deliveries, one state, no posting.
func TestDepositMismatchedSuccess_Concurrent_OneAlertRow(t *testing.T) {
	pool := depositV2ScratchPool(t)
	e := dmSetup(t, pool, "conc", MockAmountPlayerDeclineNoCascade)
	const n = 6
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := e.deliverErr(MockAmountPlayerDeclineNoCascade + int64(i) + 1)
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent delivery: %v", err)
		}
	}
	rows := alertinject.ForSubject(t, pool, e.f.tenantID)
	if len(rows) != 1 || rows[0].Occurrences != n {
		t.Fatalf("want ONE alert with %d occurrences, got %+v", n, rows)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, e.att.ID); got.State != AttemptDeclined {
		t.Fatalf("state = %s", got.State)
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}

// Retry, transient class: a transient alert-statement failure rolls the whole delivery back (error to
// the caller so the provider redelivers; no audit row committed, no alert, no state change).
func TestDepositMismatchedSuccess_TransientRaiseFailure_RollsBackDelivery(t *testing.T) {
	pool := depositV2ScratchPool(t)
	e := dmSetup(t, pool, "retry-t", MockAmountPlayerDeclineNoCascade)
	alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "40001")
	if _, err := e.deliverErr(MockAmountPlayerDeclineNoCascade + 1); err == nil {
		t.Fatalf("a transient alert-statement failure must propagate and roll the delivery back")
	}
	if n := e.auditCount(t); n != 0 {
		t.Fatalf("the rolled-back delivery must leave no audit row, got %d", n)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, e.att.ID); got.State != AttemptDeclined || got.TerminalReason != nil {
		t.Fatalf("state changed: %s %v", got.State, got.TerminalReason)
	}
}

// Retry, deterministic class: an in-tx alert failure (P0001) is swallowed by RaiseGuarded, the delivery
// commits (audit row, no state change), and the post-commit detached retry (Pending.Flush) persists the
// ONE P1.
func TestDepositMismatchedSuccess_DeterministicRaiseFailure_DetachedRetryRaises(t *testing.T) {
	pool := depositV2ScratchPool(t)
	e := dmSetup(t, pool, "retry-d", 5000)
	e.deliver(t, 5000)
	alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")
	e.deliver(t, 5001)
	if n := e.auditCount(t); n != 1 {
		t.Fatalf("audit rows = %d, want 1 (delivery must commit)", n)
	}
	rows := alertinject.WaitUpTo(t, pool, e.f.tenantID, 5*time.Second, func(rs []alertinject.Row) bool { return len(rs) > 0 })
	if len(rows) != 1 || !strings.HasSuffix(rows[0].Discriminator, ":reason:"+alertReasonDepositMismatchedSuccessOnSucceeded) {
		t.Fatalf("want the detached retry to persist ONE P1, got %+v", rows)
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, e.att.ID); got.State != AttemptSucceeded {
		t.Fatalf("state = %s", got.State)
	}
}
