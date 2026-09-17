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
	personID := flag.String("person-id", "", "optional: link this platform_admin to an existing person id (mutually exclusive with -create-person)")
	createPerson := flag.Bool("create-person", false, "optional: create a new person cluster and link this platform_admin to it (mutually exclusive with -person-id)")
	flag.Parse()

	if *email == "" {
		return fmt.Errorf("usage: seed-admin -email=admin@example.com (password read from SEED_ADMIN_PASSWORD)")
	}
	if *personID != "" && *createPerson {
		return fmt.Errorf("-person-id and -create-person are mutually exclusive")
	}
	var explicitPersonID uuid.UUID
	if *personID != "" {
		var err error
		explicitPersonID, err = uuid.Parse(*personID)
		if err != nil {
			return fmt.Errorf("-person-id must be a valid UUID: %w", err)
		}
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
	var linkedPerson identity.Person
	err = pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		var resolvedPersonID *uuid.UUID
		switch {
		case *createPerson:
			// Stage 4H-B0-R6 fix: born person-linked, rather than always
			// nil. A fresh person cluster in the same transaction as the
			// staff row, so the bootstrap never leaves an orphaned
			// person if staff creation later fails (e.g. email taken).
			var err error
			linkedPerson, err = identity.CreatePerson(ctx, tx)
			if err != nil {
				return err
			}
			resolvedPersonID = &linkedPerson.ID
		case *personID != "":
			resolvedPersonID = &explicitPersonID
		}

		var err error
		staff, err = identity.CreateStaffUser(ctx, tx, uuid.Nil, *email, passwordHash, identity.StaffRolePlatformAdmin, resolvedPersonID)
		if err != nil {
			return err
		}
		metadata := map[string]any{"tool": "cmd/seed-admin"}
		if resolvedPersonID != nil {
			metadata["person_id"] = resolvedPersonID.String()
			metadata["person_created"] = *createPerson
		}
		return audit.Record(ctx, tx, audit.Entry{
			ActorType:  audit.ActorSystem,
			Action:     "staff.platform_admin_bootstrapped",
			TargetType: "staff_user", TargetID: staff.ID.String(),
			Outcome:  audit.OutcomeSuccess,
			Metadata: metadata,
		})
	})
	if err != nil {
		return fmt.Errorf("create platform admin: %w", err)
	}

	if staff.PersonID != nil {
		fmt.Printf("seed-admin: created platform_admin %s (id: %s), linked to person %s\n", staff.Email, staff.ID, staff.PersonID)
	} else {
		fmt.Printf("seed-admin: created platform_admin %s (id: %s) - WARNING: no person_id set; this account cannot act as a four-eyes requester/approver on any control requiring resolved person linkage (e.g. Asset Registry) until remediated via POST /v1/admin/platform-staff/{staffID}/person-link\n", staff.Email, staff.ID)
	}
	return nil
}
