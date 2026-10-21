// Command e2e-mcprobe is a TEST-ONLY helper of the Back Office real-stack end-to-end harness (lic32). It is built by
// backoffice/e2e/run.sh into the run directory and is never part of any deployable binary.
//
// It connects with the RUNTIME database role (the role platform-api uses; E2E_RUNTIME_DATABASE_URL) and calls the REAL market
// resolver, which the Playwright specs cannot call from TypeScript (no HTTP endpoint exposes it):
//
//	resolve <tenant> <country> <operation>
//	    runs operatingmarket.ResolveOperatingCountryPolicy (the single implementation of the decision, the one marketgate calls)
//	    inside a tenant-scoped, rolled-back, read-only transaction at the DATABASE clock and prints "RESOLVE|<outcome>".
//
// Nothing is written and nothing is signed. The database password never appears in an argument list. Synthetic data only.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "e2e-mcprobe:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	url := os.Getenv("E2E_RUNTIME_DATABASE_URL")
	if url == "" {
		return errors.New("E2E_RUNTIME_DATABASE_URL is not set")
	}
	if len(args) != 4 || args[0] != "resolve" {
		return errors.New("usage: resolve <tenant> <country> <operation>")
	}
	tenant, err := uuid.Parse(args[1])
	if err != nil {
		return err
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // always: a probe never commits
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
		return err
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&now); err != nil {
		return err
	}
	res, err := operatingmarket.ResolveOperatingCountryPolicy(ctx, tx, operatingmarket.Query{
		TenantID: tenant, CountryCode: args[2], OperationCode: args[3], AsOf: now,
	})
	if err != nil {
		return err
	}
	fmt.Printf("RESOLVE|%s\n", res.Outcome())
	return nil
}
