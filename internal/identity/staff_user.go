package identity

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// StaffRole mirrors auth.Role's staff-relevant values. Kept as a
// separate string type here (rather than importing internal/auth) to
// avoid a dependency cycle - internal/httpserver is what bridges the two
// when issuing a token for a loaded StaffUser.
type StaffRole string

const (
	StaffRolePlatformAdmin StaffRole = "platform_admin"
	StaffRoleTenantAdmin   StaffRole = "tenant_admin"
	StaffRoleSupport       StaffRole = "support"
	StaffRoleCompliance    StaffRole = "compliance"
	StaffRoleFinance       StaffRole = "finance"
)

// StaffUser operates the platform/back office - distinct from
// PlayerAccount. TenantID is uuid.Nil for a platform-wide
// platform_admin; every other role belongs to exactly one tenant (see
// migration 0011's CHECK constraint, which enforces this pairing at the
// database level, not just here).
type StaffUser struct {
	ID           uuid.UUID
	TenantID     uuid.UUID // uuid.Nil for platform_admin
	Email        string
	PasswordHash string
	Role         StaffRole
	Status       string
	// PersonID is nil for the overwhelming majority of staff accounts,
	// which have no corresponding player account. It is set only for
	// the rare, deliberately identified case of a real person who is
	// both a platform/tenant staff member and a player at some brand -
	// see migration 0029's own doc comment. It is what
	// internal/withdrawal's BeneficiaryCheck (wired in
	// internal/httpserver) compares against a withdrawal's own player
	// account to authoritatively prevent self-approval.
	PersonID *uuid.UUID
}

// GetStaffUserByEmail looks up a staff user. If tenantID is uuid.Nil, tx
// must come from db.WithoutTenant (platform_admin lookup); otherwise tx
// must come from db.WithTenant(tenantID) - this mirrors the dual-scope
// RLS policy on staff_users exactly, so the caller picks the right one
// based on whether a tenant_slug was supplied at the login endpoint.
func GetStaffUserByEmail(ctx context.Context, tx pgx.Tx, email string) (StaffUser, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var s StaffUser
	var tenantID *uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, email, password_hash, role, status, person_id FROM staff_users WHERE email = $1`,
		email,
	).Scan(&s.ID, &tenantID, &s.Email, &s.PasswordHash, &s.Role, &s.Status, &s.PersonID)
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffUser{}, ErrNotFound
	}
	if err != nil {
		return StaffUser{}, fmt.Errorf("identity: get staff user: %w", err)
	}
	if tenantID != nil {
		s.TenantID = *tenantID
	}
	return s, nil
}

// GetStaffUserByID looks up a staff user by id within the current scope
// (tx from db.WithTenant for a tenant-scoped staff user, or
// db.WithoutTenant for a platform-wide one) - used by the refresh
// handler to re-check a staff principal's CURRENT role and status before
// minting a new access token, rather than trusting whatever the previous
// token claimed.
func GetStaffUserByID(ctx context.Context, tx pgx.Tx, id uuid.UUID) (StaffUser, error) {
	var s StaffUser
	var tenantID *uuid.UUID
	err := tx.QueryRow(ctx,
		`SELECT id, tenant_id, email, password_hash, role, status, person_id FROM staff_users WHERE id = $1`,
		id,
	).Scan(&s.ID, &tenantID, &s.Email, &s.PasswordHash, &s.Role, &s.Status, &s.PersonID)
	if errors.Is(err, pgx.ErrNoRows) {
		return StaffUser{}, ErrNotFound
	}
	if err != nil {
		return StaffUser{}, fmt.Errorf("identity: get staff user by id: %w", err)
	}
	if tenantID != nil {
		s.TenantID = *tenantID
	}
	return s, nil
}

// CreateStaffUser creates a staff user within tx. Platform-admin creation
// (tenantID == uuid.Nil) requires tx from db.WithoutTenant; tenant-scoped
// roles require tx from db.WithTenant(tenantID) - enforced by the
// dual-scope RLS policy regardless of what this function is given, so a
// mismatch fails at the database, not silently. Callers (cmd/seed-admin,
// the admin staff-creation endpoint) write the corresponding audit
// record in the same tx.
//
// personID is nil for the overwhelming majority of staff accounts - see
// StaffUser.PersonID's doc comment. Pass a non-nil value only when this
// staff member is a KNOWN, deliberately identified dual-role individual
// (also a player at some brand); the caller is responsible for having
// actually verified that, since this function does not and cannot
// (there is no reliable automatic way to detect it - see migration
// 0029). An unknown/invalid person id fails with the same foreign-key
// error Postgres would give for any other bad reference.
func CreateStaffUser(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, email, passwordHash string, role StaffRole, personID *uuid.UUID) (StaffUser, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	s := StaffUser{ID: uuid.New(), TenantID: tenantID, Email: email, PasswordHash: passwordHash, Role: role, Status: "active", PersonID: personID}

	var tid *uuid.UUID
	if tenantID != uuid.Nil {
		tid = &tenantID
	}
	_, err := tx.Exec(ctx,
		`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		s.ID, tid, s.Email, s.PasswordHash, s.Role, s.Status, s.PersonID,
	)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return StaffUser{}, ErrEmailTaken
		}
		return StaffUser{}, fmt.Errorf("identity: create staff user: %w", err)
	}
	return s, nil
}
