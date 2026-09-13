// Command seed-admin creates the platform's first platform_admin staff
// user. This is a one-time bootstrap operation, deliberately NOT exposed
// through the HTTP API (there is no "create the first admin" endpoint,
// since any such endpoint would either need to be open to the internet
// pre-auth - a standing privilege-escalation risk - or itself require an
// existing admin, which is exactly the chicken-and-egg problem this tool
// exists to break). It connects to the database directly, the same way
// cmd/migrate does - see docs/decisions/0014-service-identity-pattern.md
// for why that's the platform's standard shape for a platform-level job,
// not a special case invented here.
//
// The password is read from an environment variable, never a CLI flag
// (flags are visible in process listings/shell history; an env var, set
// via a secrets manager or a one-off `SEED_ADMIN_PASSWORD=... go run
// ./cmd/seed-admin`, is not).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed-admin: fatal:", err)
		os.Exit(1)
	}
}

func run() error {
	email := flag.String("email", "", "platform admin email (required)")
	flag.Parse()

	if *email == "" {
		return fmt.Errorf("usage: seed-admin -email=admin@example.com (password read from SEED_ADMIN_PASSWORD)")
	}
	password := os.Getenv("SEED_ADMIN_PASSWORD")
	if password == "" {
		return fmt.Errorf("SEED_ADMIN_PASSWORD environment variable is required")
	}
	if len(password) < 12 {
		return fmt.Errorf("SEED_ADMIN_PASSWORD must be at least 12 characters for a platform_admin account")
	}

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

	passwordHash, err := auth.HashPassword(password)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}

	var staff identity.StaffUser
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		staff, err = identity.CreateStaffUser(ctx, tx, uuid.Nil, *email, passwordHash, identity.StaffRolePlatformAdmin, nil)
		if err != nil {
			return err
		}
		return audit.Record(ctx, tx, audit.Entry{
			ActorType:  audit.ActorSystem,
			Action:     "staff.platform_admin_bootstrapped",
			TargetType: "staff_user", TargetID: staff.ID.String(),
			Outcome:  audit.OutcomeSuccess,
			Metadata: map[string]any{"tool": "cmd/seed-admin"},
		})
	})
	if err != nil {
		return fmt.Errorf("create platform admin: %w", err)
	}

	fmt.Printf("seed-admin: created platform_admin %s (id: %s)\n", staff.Email, staff.ID)
	return nil
}
