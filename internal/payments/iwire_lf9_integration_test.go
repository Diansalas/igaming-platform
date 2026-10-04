//go:build integration

package payments

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// LF test 9 (the real-path version, ledger-finance I-wire F1): concurrent
// evidence receipts on ONE live attempt/intent through the real
// ApplyReceiptEvidence path, each in its own alerting.InTx transaction with a
// post-commit Flush, while an in-tx raise failure is injected
// (InTxOnly P0001) so a swallowed raise's DETACHED retry runs concurrently with
// the other receipts. Half the receipts are contradicting successes (T10) and
// half are valid successes racing them. The two possible final states are both
// consistent, and nothing else is:
//
//   - the valid success won: the attempt is succeeded, exactly one deposit
//     posting exists, and no park alert exists (a later contradicting success
//     on a succeeded attempt is audit-only);
//   - the contradicting success won: the attempt is disputed, ZERO postings
//     exist (a disputed attempt never posts), and exactly one park alert exists.
//
// No receipt errors, no deadlock, never more than one posting. Run with
// -race -count=50.
func TestIWire_LF9_ConcurrentT10ReceiptsWithDetachedFlush_NoDoublePostingOneAlert(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-lf9")
	a, ref := e.ambiguousBound(t, "iw-lf9")
	alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			amount := int64(5000) // a valid success
			if i%2 == 0 {
				amount = 1 // a contradicting success (T10)
			}
			<-start
			pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
				_, err := ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.id, ReceiptEvidence{
					EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: amount, AssetCode: "EUR"})
				return err
			})
			if err == nil {
				pending.Flush(context.Background())
			}
			errs[i] = err
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("receipt %d errored (no alert or lock outcome may fail a receipt): %v", i, err)
		}
	}

	postings := e.depositTxCount(t)
	final := mustGetAttempt(t, pool, e.f.tenantID, a.ID)
	var parkAlerts []alertinject.Row
	for _, r := range alertinject.ForSubject(t, pool, e.f.tenantID) {
		if r.Kind == string(alerting.KindPaymentWebhookIntegrity) {
			parkAlerts = append(parkAlerts, r)
		}
	}
	t.Logf("LF9 outcome: state=%s postings=%d park_alerts=%d", final.State, postings, len(parkAlerts))
	switch final.State {
	case AttemptSucceeded:
		if postings != 1 || cashBalance(t, pool, e.f) != 5000 {
			t.Fatalf("succeeded: postings=%d balance=%d, want exactly one posting of 5000", postings, cashBalance(t, pool, e.f))
		}
		if len(parkAlerts) != 0 {
			t.Fatalf("succeeded attempt must have no park alert, got %+v", parkAlerts)
		}
	case AttemptDisputed:
		if postings != 0 || cashBalance(t, pool, e.f) != 0 {
			t.Fatalf("disputed: postings=%d balance=%d, want zero (a disputed second capture stays unposted)", postings, cashBalance(t, pool, e.f))
		}
		want := "attempt:" + a.ID.String() + ":reason:" + TerminalReasonCallbackAmountAssetMismatch
		if len(parkAlerts) != 1 || parkAlerts[0].Discriminator != want || parkAlerts[0].Occurrences < 1 {
			t.Fatalf("disputed: want exactly one park alert %q, got %+v", want, parkAlerts)
		}
	default:
		t.Fatalf("final state %s: want succeeded or disputed", final.State)
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}
