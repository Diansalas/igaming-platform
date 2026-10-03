//go:build integration

// PRH-2 D (PAY-POLL-AMOUNT-1 / FH7-06, defence in depth): ledger.Post refuses
// an EMPTY provider_tx_id with the typed ErrInvalidEntry, before anything is
// locked or written. Without the guard a poll path that keyed the posting on an
// empty echo would post under the degenerate key "<provider>:" (and only the
// untyped 0099 CHECK would stop it, as a constraint error that loops).
package ledger

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestPost_EmptyProviderTxID_IsRefusedAsInvalidEntry_NothingWritten(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	in := depositInput(f, "empty-ptx-1", 5_000)
	empty := ""
	in.ProviderTxID = &empty
	in.IdempotencyKey = "mockpsp:" // what the unguarded poll path would have derived

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := Post(ctx, tx, in)
		return err
	})
	if !errors.Is(err, ErrInvalidEntry) {
		t.Fatalf("Post with an empty provider_tx_id = %v, want ErrInvalidEntry", err)
	}

	var n int64
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND provider_id = 'mockpsp'`, f.tenantID).Scan(&n)
	}); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("a refused post left %d ledger_transactions row(s)", n)
	}

	// A non-empty reference still posts (the guard is exactly "empty").
	mustPost(t, pool, f, depositInput(f, "empty-ptx-ok", 5_000))
}
