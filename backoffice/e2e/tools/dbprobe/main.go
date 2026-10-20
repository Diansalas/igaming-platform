// Command e2e-dbprobe is a TEST-ONLY helper of the Back Office real-stack end-to-end harness (lic21). It is built by
// backoffice/e2e/run.sh into the run directory and is never part of any deployable binary.
//
// It connects with the RUNTIME database role (the role platform-api uses; E2E_RUNTIME_DATABASE_URL) and does the one thing the
// Playwright specs cannot do from TypeScript (call the real Go market-evidence read):
//
//	market-evidence <tenant> <player>
//	    runs the REAL kyc.ReadMarketResidenceEvidence (the only market-evidence consumer) inside a tenant-scoped
//	    transaction and prints "EVIDENCE|true|<country>" or "EVIDENCE|false". It writes nothing (read only, rolled back).
//
// It deliberately imports no signing code (actorproof): the signer-owner static pin must stay untouched. Proofs for the
// below-the-application-layer probes are produced by the TypeScript harness (lib/kyc_proof.ts), which holds the harness key.
// The database password never appears in an argument list (URL from the environment). Synthetic data only.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/kyc"
)

func main() {
	if err := run(context.Background(), os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "e2e-dbprobe:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	url := os.Getenv("E2E_RUNTIME_DATABASE_URL")
	if url == "" {
		return errors.New("E2E_RUNTIME_DATABASE_URL is not set")
	}
	if len(args) == 0 {
		return errors.New("usage: market-evidence <tenant> <player>")
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // always: a probe never commits

	switch args[0] {
	case "market-evidence":
		if len(args) != 3 {
			return errors.New("market-evidence <tenant> <player>")
		}
		tenant, player := uuid.MustParse(args[1]), uuid.MustParse(args[2])
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenant.String()); err != nil {
			return err
		}
		country, ok, err := kyc.ReadMarketResidenceEvidence(ctx, tx, tenant, player)
		if err != nil {
			return err
		}
		if ok {
			fmt.Printf("EVIDENCE|true|%s\n", country)
		} else {
			fmt.Println("EVIDENCE|false")
		}
		return nil

	}
	return fmt.Errorf("unknown command %q", args[0])
}
