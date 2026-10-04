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

// LF L2: the deterministic sibling of the real-path LF test 9. Every concurrent
// receipt is a contradicting success (T10) and every in-tx raise is swallowed
// (InTxOnly P0001), so each receipt's detached Flush retry runs concurrently.
// Unlike the mixed test, only ONE outcome is possible and it is asserted
// exactly: the attempt is disputed, ZERO postings exist, exactly one park alert
// with the stable key exists, and no receipt errored. A regression that lets one
// concurrent receipt post, lose the alert or duplicate it cannot hide behind
// the "either outcome is consistent" branch of the mixed test.
func TestIWire_LF9_AllContradictingConcurrentReceipts_DisputedZeroPostingsOneAlert(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-lf9d")
	a, ref := e.ambiguousBound(t, "iw-lf9d")
	alertinject.Install(t, pool, e.f.tenantID, alertinject.InTxOnly, "P0001")

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
				_, err := ApplyReceiptEvidence(ctx, tx, e.orch, e.f.tenantID, e.id, ReceiptEvidence{
					EventType: "deposit", ProviderReference: ref, Outcome: OutcomeSucceeded, Amount: 1, AssetCode: "EUR"})
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
			t.Fatalf("receipt %d errored: %v", i, err)
		}
	}
	if got := mustGetAttempt(t, pool, e.f.tenantID, a.ID).State; got != AttemptDisputed {
		t.Fatalf("every receipt contradicts the intent: state = %s, want disputed", got)
	}
	if postings := e.depositTxCount(t); postings != 0 || cashBalance(t, pool, e.f) != 0 {
		t.Fatalf("a disputed attempt never posts: postings=%d balance=%d", postings, cashBalance(t, pool, e.f))
	}
	var park []alertinject.Row
	for _, r := range alertinject.ForSubject(t, pool, e.f.tenantID) {
		if r.Kind == string(alerting.KindPaymentWebhookIntegrity) {
			park = append(park, r)
		}
	}
	want := "attempt:" + a.ID.String() + ":reason:" + TerminalReasonCallbackAmountAssetMismatch
	if len(park) != 1 || park[0].Discriminator != want {
		t.Fatalf("want exactly one park alert %q, got %+v", want, park)
	}
	assertLedgerBalanced(t, pool, e.f.tenantID)
}
