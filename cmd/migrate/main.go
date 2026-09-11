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
		return fmt.Errorf("usage: migrate [-dir=migrations] [-steps=N] <up|down|status>")
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
	default:
		return fmt.Errorf("unknown command %q: expected up, down, or status", command)
	}
}
