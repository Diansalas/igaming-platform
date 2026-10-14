//go:build integration

// Package staffsv (integration builds ONLY) lets domain test fixtures that build a verified-token context by hand
// (tenant.Context) carry the actor's CURRENT staff_users.staff_version as the token's `sv` claim (ADR 0119 S5 + S6,
// migration 0139). A fixture registers the pool a staff row lives in when it creates the row; Version reads the row
// at the moment the context is built, exactly like a token minted at that moment.
package staffsv

import (
	"context"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

type entry struct {
	pool   *db.Pool
	tenant uuid.UUID
}

var registry sync.Map // uuid.UUID -> entry

// Register records that staffID lives in pool, in tenantID's scope (uuid.Nil: platform).
func Register(pool *db.Pool, staffID, tenantID uuid.UUID) {
	registry.Store(staffID, entry{pool: pool, tenant: tenantID})
}

// Version returns staffID's current staff_version; 1 (the initial version) for an unregistered or unreadable row.
func Version(staffID uuid.UUID) int64 {
	v, ok := registry.Load(staffID)
	if !ok {
		return 1
	}
	e := v.(entry)
	var sv int64 = 1
	fn := func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE((SELECT staff_version FROM staff_users WHERE id = $1), 1)`, staffID).Scan(&sv)
	}
	var err error
	if e.tenant != uuid.Nil {
		err = e.pool.WithTenant(context.Background(), e.tenant, fn)
	} else {
		err = e.pool.WithoutTenant(context.Background(), fn)
	}
	if err != nil {
		return 1
	}
	return sv
}

// personRequiredRoles mirrors migration 0139's SF014 role set (ADR 0119 section 9.4).
var personRequiredRoles = map[string]bool{"platform_admin": true, "tenant_admin": true, "finance": true, "compliance": true}

// PersonIfRequired creates a fresh Person inside tx and returns its id when role is one SF014 requires a Person for,
// and nil otherwise: the fixture form of "an approve-capable staff member is linked to a Person".
func PersonIfRequired(ctx context.Context, tx pgx.Tx, role string) (*uuid.UUID, error) {
	if !personRequiredRoles[role] {
		return nil, nil
	}
	id := uuid.New()
	if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, id); err != nil {
		return nil, err
	}
	return &id, nil
}

// PlantEnrolledPlatformAdmin creates, inside tx (a platform-scoped session), an active, Person-linked platform_admin
// with an active placeholder factor and returns its id: the "another active, enrolled platform admin" that migration
// 0139's SF020 requires before a test may suspend, offboard or demote a platform admin.
func PlantEnrolledPlatformAdmin(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
	id := uuid.New()
	if _, err := tx.Exec(ctx, `WITH p AS (SELECT gen_random_uuid() AS id), pi AS (INSERT INTO persons (id) SELECT id FROM p)
		INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
		VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active', (SELECT id FROM p))`, id, "spare-admin-"+id.String()+"@fixture.invalid"); err != nil {
		return uuid.Nil, err
	}
	var factor uuid.UUID
	if err := tx.QueryRow(ctx, `INSERT INTO staff_mfa_factors (staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, pending_expires_at)
		VALUES ($1, 'totp', 'pending', 'fixture-placeholder', '\x000000000000000000000000'::bytea, '\x00'::bytea, now() + interval '10 minutes')
		RETURNING id`, id).Scan(&factor); err != nil {
		return uuid.Nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'active', activated_at = now(), pending_expires_at = NULL WHERE id = $1`, factor); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

// InsertLegacyUnlinkedStaff runs insertSQL (a staff_users INSERT of an approve-capable role WITHOUT a person_id) inside
// tx with exactly migration 0139's SF014 trigger disabled for that one statement, then re-enables it - in the SAME
// transaction, so no other session ever observes the trigger disabled (ALTER TABLE is transactional and holds an
// ACCESS EXCLUSIVE lock until commit). It reproduces a LEGACY row created before 0139 (0139 does not backfill: such
// rows can still exist), for the downstream "unlinked principal is refused" defences. tx must belong to the table
// OWNER (the TEST_DATABASE_URL pool); a runtime-role transaction is refused.
func InsertLegacyUnlinkedStaff(ctx context.Context, tx pgx.Tx, insertSQL string, args ...any) error {
	if _, err := tx.Exec(ctx, `ALTER TABLE staff_users DISABLE TRIGGER sf_14_staff_person_required`); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertSQL, args...); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `ALTER TABLE staff_users ENABLE TRIGGER sf_14_staff_person_required`)
	return err
}
