//go:build integration

package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// The pool the process opens (db.ConnectWithRuntimeParams with cfg.DatabaseRuntimeParams()) puts
// idle_in_transaction_session_timeout on every connection it opens, as a startup parameter: the
// role itself carries no such setting (no ALTER ROLE), and an idle transaction is terminated.
func TestProductionPoolWiring_IdleInTransactionTimeoutPerConnection(t *testing.T) {
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL is required (a skip is not evidence)")
	}
	ctx := context.Background()
	show := func(p *db.Pool) string {
		var got string
		if err := p.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SHOW idle_in_transaction_session_timeout`).Scan(&got)
		}); err != nil {
			t.Fatal(err)
		}
		return got
	}

	// The default configuration: 30s on a pool of several connections (every one of them).
	def := config.Config{}
	p, err := db.ConnectWithRuntimeParams(ctx, u, 4, 5*time.Second, def.DatabaseRuntimeParams())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for i := 0; i < 4; i++ {
		// Hold several connections at once so the pool really opens more than one.
		if got := show(p); got != "30s" {
			t.Fatalf("connection %d: idle_in_transaction_session_timeout = %q, want 30s", i, got)
		}
	}
	// The role carries no setting of its own: the value above came from the startup packet.
	var roleSettings int
	if err := p.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM pg_db_role_setting s JOIN pg_roles r ON r.oid = s.setrole
			WHERE r.rolname = current_user AND array_to_string(s.setconfig, ',') LIKE '%idle_in_transaction_session_timeout%'`).Scan(&roleSettings)
	}); err != nil {
		t.Fatal(err)
	}
	if roleSettings != 0 {
		t.Fatalf("the setting must be per connection, not on the role (found %d role-level settings)", roleSettings)
	}

	// A configured override reaches the session, and an idle transaction is terminated.
	cfg := config.Config{DatabaseIdleInTxTimeout: 800 * time.Millisecond}
	q, err := db.ConnectWithRuntimeParams(ctx, u, 2, 5*time.Second, cfg.DatabaseRuntimeParams())
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if got := show(q); got != "800ms" {
		t.Fatalf("override: %q", got)
	}
	err = q.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
			return err
		}
		time.Sleep(1800 * time.Millisecond)
		_, err := tx.Exec(ctx, `SELECT 1`)
		return err
	})
	if err == nil {
		t.Fatal("an idle transaction must be terminated by the configured timeout")
	}
}
