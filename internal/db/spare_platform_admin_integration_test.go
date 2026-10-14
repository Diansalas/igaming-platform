//go:build integration

package db

// ADR 0119 S5 + S6 (migration 0139, SF020): db's internal tests cannot import internal/testsupport/staffsv (it imports
// this package), so the "another active, enrolled platform admin" fixture is repeated here.

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// plantEnrolledPlatformAdmin is staffsv.PlantEnrolledPlatformAdmin.
func plantEnrolledPlatformAdmin(ctx context.Context, tx pgx.Tx) (uuid.UUID, error) {
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
