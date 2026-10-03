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

// LF tests 7/9: N concurrent raisers of ONE condition (the same intent's
// multiple-success) produce exactly one open alert with N occurrences, each
// through its own alerting.InTx transaction, under -race.
func TestIWire_MultipleSuccess_ConcurrentRaisersDedupToOneAlert(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-dd")
	a, _ := e.ambiguousBound(t, "iw-dd")
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			pending, err := alerting.InTx(context.Background(), alerting.NewTenantRunner(pool, e.f.tenantID), func(ctx context.Context, tx pgx.Tx) error {
				return raiseMultipleSuccessAlert(ctx, tx, a, e.id, EvidenceCallback, i%2 == 0)
			})
			if err == nil {
				pending.Flush(context.Background())
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("concurrent raise: %v", err)
		}
	}
	rows := alertinject.ForSubject(t, pool, e.f.tenantID)
	ms := alertinject.Find(rows, string(alerting.KindPaymentMultipleSuccessForIntent))
	bs := alertinject.Find(rows, string(alerting.KindPaymentDepositIntentIndexBackstop))
	if len(ms) != 1 || ms[0].Occurrences != n {
		t.Fatalf("multiple-success: want 1 alert with %d occurrences, got %+v", n, ms)
	}
	if len(bs) != 1 || bs[0].Occurrences != n/2 {
		t.Fatalf("backstop: want 1 alert with %d occurrences, got %+v", n/2, bs)
	}
}
