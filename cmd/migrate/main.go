// Command migrate applies or rolls back platform-api's SQL migrations
// under /migrations. It is deliberately a small, dependency-light tool
// (using the platform's own internal/db package) rather than a pull of a
// heavier third-party migration framework, per the Stage 1 instruction to
// prefer the simplest architecture that credibly does the job.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "migrate: fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	dir := flag.String("dir", "migrations", "path to migrations directory")
	steps := flag.Int("steps", 1, "number of migrations to roll back (down command only)")
	flag.Parse()

	if flag.NArg() != 1 {
		return fmt.Errorf("usage: migrate [-dir=migrations] [-steps=N] <up|down|status|verify>")
	}
	command := flag.Arg(0)

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx := context.Background()
	pool, err := db.Connect(ctx, databaseURL, 2, 5*time.Second)
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}
	defer pool.Close()

	switch command {
	case "up":
		applied, err := pool.MigrateUp(ctx, *dir)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("migrate: nothing to apply, already up to date")
			return nil
		}
		fmt.Printf("migrate: applied %d migration(s): %v\n", len(applied), applied)
		return nil
	case "down":
		rolledBack, err := pool.MigrateDown(ctx, *dir, *steps)
		if err != nil {
			return err
		}
		fmt.Printf("migrate: rolled back %d migration(s): %v\n", len(rolledBack), rolledBack)
		return nil
	case "status":
		migrations, err := db.LoadMigrations(*dir)
		if err != nil {
			return err
		}
		fmt.Printf("migrate: %d migration(s) found in %s: %s\n", len(migrations), *dir, db.DescribeMigrations(migrations))
		return nil
	case "verify":
		// PLAT-MIGDRIFT-1 (docs/architecture/38-deployment-architecture.md
		// §3/§4): checksum-verifies every already-applied migration's
		// up-file against what was recorded when it was applied, and
		// checks the on-disk migration set for version-number gaps. See
		// db.VerifyMigrations' own doc comment for the exact, non-
		// aspirational scope of what this does and does not check.
		report, err := pool.VerifyMigrations(ctx, *dir)
		if err != nil {
			return err
		}
		for _, res := range report.Results {
			label := fmt.Sprintf("%04d_%s", res.Version, res.Description)
			switch res.Status {
			case db.MigrationCheckOK:
				fmt.Printf("migrate verify: OK           %s\n", label)
			case db.MigrationCheckUnverifiable:
				fmt.Printf("migrate verify: UNVERIFIABLE %s: %s\n", label, res.Detail)
			default:
				fmt.Printf("migrate verify: %-12s %s: %s\n", strings.ToUpper(string(res.Status)), label, res.Detail)
			}
		}
		for _, gap := range report.VersionGaps {
			fmt.Printf("migrate verify: GAP          %s\n", gap)
		}
		if !report.OK() {
			return fmt.Errorf("migrate verify: FAILED - see above (mismatch, missing file, or version gap detected)")
		}
		fmt.Println("migrate verify: all applied migrations verified clean, no version gaps")
		return nil
	default:
		return fmt.Errorf("unknown command %q: expected up, down, status, or verify", command)
	}
}
