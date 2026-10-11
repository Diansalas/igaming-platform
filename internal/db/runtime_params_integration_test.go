//go:build integration

package db

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// ADR 0112 security C4: idle_in_transaction_session_timeout is set per CONNECTION from the
// application's pool configuration (startup parameters), not by ALTER ROLE. A transaction left
// idle then ends instead of holding a gate key / subject row lock forever.
func TestConnectWithRuntimeParams_IdleInTransactionTimeoutIsPerConnection(t *testing.T) {
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL is required (a skip is not evidence)")
	}
	ctx := context.Background()
	p, err := ConnectWithRuntimeParams(ctx, u, 4, 5*time.Second, map[string]string{"idle_in_transaction_session_timeout": "700ms"})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var got string
	if err := p.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SHOW idle_in_transaction_session_timeout`).Scan(&got)
	}); err != nil {
		t.Fatal(err)
	}
	if got != "700ms" {
		t.Fatalf("the per-connection setting must reach the session, got %q", got)
	}
	// An idle transaction is terminated by the server.
	err = p.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
			return err
		}
		time.Sleep(1500 * time.Millisecond)
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	})
	if err == nil {
		t.Fatal("an idle transaction must be terminated by idle_in_transaction_session_timeout")
	}
	// A plain Connect leaves the server default (the setting is opt-in per pool).
	q, err := Connect(ctx, u, 2, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SHOW idle_in_transaction_session_timeout`).Scan(&got)
	}); err != nil {
		t.Fatal(err)
	}
	if got == "700ms" {
		t.Fatal("Connect must not inherit another pool's setting")
	}
	// Only the closed set of names is accepted.
	if _, err := ConnectWithRuntimeParams(ctx, u, 2, 5*time.Second, map[string]string{"session_replication_role": "replica"}); err == nil ||
		!strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("an unlisted runtime parameter must be refused: %v", err)
	}
}
