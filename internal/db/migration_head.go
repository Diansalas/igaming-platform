package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// MigrationHead is what the runtime role can read of the migration ledger: the highest applied version and how many
// versions are recorded (F6 / SA-3). Nothing else of schema_migrations is read (not the checksum, description or
// time).
type MigrationHead struct {
	HeadVersion  int64
	AppliedCount int64
}

// ReadMigrationHead reads schema_migrations through the runtime role. The runtime role keeps SELECT on the table (the
// blanket grant of deploy/init-app-role.sql; only INSERT, UPDATE and DELETE are revoked) and the table has no row
// level security, so this needs no owner connection and no SECURITY DEFINER function. An empty ledger reports
// {0, 0}. The query is read-only and returns two aggregates.
func (p *Pool) ReadMigrationHead(ctx context.Context) (MigrationHead, error) {
	var h MigrationHead
	err := p.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0), count(*) FROM schema_migrations`).Scan(&h.HeadVersion, &h.AppliedCount)
	})
	if err != nil {
		return MigrationHead{}, fmt.Errorf("db: read migration head: %w", err)
	}
	return h, nil
}
