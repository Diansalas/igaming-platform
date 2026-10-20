//go:build integration

// Package kycpolfix (integration builds ONLY) is the fixture side of migration 0185 (lic19, S4): every KYC enforcement policy
// write is a signed, Person-distinct act, so a fixture needs REAL platform administrators, each linked to its own Person, and a
// process proof issuer. It imports neither kyc (kyc's own tests use it) nor signs anything itself: tests call the kyc functions
// with kycpolicyproof.Signer{}. SYNTHETIC administrators only.
package kycpolfix

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/staffsv"
)

// Install provisions the process proof issuer (skips the test when no database is configured).
func Install(t testing.TB) {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	prooftest.Install(t, u)
}

// Admin creates an active platform_admin staff member linked to a fresh Person and returns its id. Two Admins are always two
// Persons, so they satisfy the Person-distinct four-eyes rule.
func Admin(t testing.TB, pool *db.Pool) uuid.UUID {
	t.Helper()
	id, person := uuid.New(), uuid.New()
	if err := launchfix.OwnerFor(t, pool).WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active', $3)`, id, "kycpol-"+id.String()+"@test.invalid", person)
		return err
	}); err != nil {
		t.Fatalf("kycpolfix: seed platform admin: %v", err)
	}
	return id
}

// AdminSamePerson creates a second platform_admin account linked to the SAME Person as account.
func AdminSamePerson(t testing.TB, pool *db.Pool, account uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := launchfix.OwnerFor(t, pool).WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			SELECT $1, NULL, $2, 'x', 'platform_admin', 'active', person_id FROM staff_users WHERE id = $3`, id, "kycpol-"+id.String()+"@test.invalid", account)
		return err
	}); err != nil {
		t.Fatalf("kycpolfix: seed second account: %v", err)
	}
	return id
}

// AdminNoPerson creates a platform_admin account with NO Person link (to prove the fail-closed refusal).
func AdminNoPerson(t testing.TB, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := launchfix.OwnerFor(t, pool).WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		// SF014 (migration 0139) refuses creating an unlinked platform_admin; staffsv reproduces a LEGACY (pre-0139, not
		// backfilled) one, the sanctioned fixture for the "unlinked principal is refused" defences.
		return staffsv.InsertLegacyUnlinkedStaff(ctx, tx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status)
			VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active')`, id, "kycpol-"+id.String()+"@test.invalid")
	}); err != nil {
		t.Fatalf("kycpolfix: seed unlinked admin: %v", err)
	}
	return id
}
