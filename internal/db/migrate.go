package db

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// migrationFilePattern matches "<version>_<description>.<up|down>.sql",
// e.g. "0001_create_tenants.up.sql". Version is a zero-padded integer so
// lexical and numeric sort agree.
var migrationFilePattern = regexp.MustCompile(`^(\d+)_(.+)\.(up|down)\.sql$`)

type migration struct {
	Version     int64
	Description string
	UpPath      string
	DownPath    string
}

// LoadMigrations reads and pairs up/down SQL files from dir. It fails
// loudly if an up file has no matching down file (or vice versa) - a
// migration platform must always be able to roll back a change, per the
// project's engineering discipline around reproducible, reversible
// schema changes (Blueprint §8 certification posture).
func LoadMigrations(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("db: read migrations dir %q: %w", dir, err)
	}

	byVersion := map[int64]*migration{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationFilePattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		version, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("db: migration %q has invalid version: %w", e.Name(), err)
		}
		mig, ok := byVersion[version]
		if !ok {
			mig = &migration{Version: version, Description: m[2]}
			byVersion[version] = mig
		}
		full := filepath.Join(dir, e.Name())
		switch m[3] {
		case "up":
			mig.UpPath = full
		case "down":
			mig.DownPath = full
		}
	}

	migrations := make([]migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.UpPath == "" || m.DownPath == "" {
			return nil, fmt.Errorf("db: migration version %d (%s) is missing its up or down file", m.Version, m.Description)
		}
		migrations = append(migrations, *m)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

const ensureSchemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version     BIGINT PRIMARY KEY,
	description TEXT NOT NULL,
	applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// migrationLockKey is an arbitrary, fixed advisory-lock key shared by
// every platform-api process that might run migrations concurrently
// (e.g. two deploys racing, or a developer running `make migrate-up`
// while CI is doing the same against a shared environment). Without it,
// two concurrent MigrateUp calls both read the same "already applied"
// set and can both attempt - and partially apply - the same new
// migration. The value itself is meaningless; it only needs to be
// stable and not collide with an unrelated advisory lock elsewhere in
// the platform.
var migrationLockKey int64 = 727_272_001

// MigrateUp applies every migration in dir whose version has not yet been
// recorded in schema_migrations, in ascending order, each in its own
// transaction. It is idempotent: running it again with nothing new to
// apply is a no-op. The whole operation is guarded by a session-level
// advisory lock so two processes cannot apply migrations concurrently
// against the same database.
func (p *Pool) MigrateUp(ctx context.Context, dir string) ([]int64, error) {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("db: acquire migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey) }()

	if _, err := conn.Exec(ctx, ensureSchemaMigrationsTable); err != nil {
		return nil, fmt.Errorf("db: ensure schema_migrations table: %w", err)
	}

	migrations, err := LoadMigrations(dir)
	if err != nil {
		return nil, err
	}

	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("db: query schema_migrations: %w", err)
	}
	applied := map[int64]bool{}
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: scan version: %w", err)
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: read schema_migrations: %w", err)
	}

	var newlyApplied []int64
	for _, m := range migrations {
		if applied[m.Version] {
			continue
		}
		sqlBytes, err := os.ReadFile(m.UpPath)
		if err != nil {
			return newlyApplied, fmt.Errorf("db: read %s: %w", m.UpPath, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return newlyApplied, fmt.Errorf("db: begin migration tx: %w", err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return newlyApplied, fmt.Errorf("db: apply migration %d (%s): %w", m.Version, m.Description, err)
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, description) VALUES ($1, $2)`,
			m.Version, m.Description,
		); err != nil {
			_ = tx.Rollback(ctx)
			return newlyApplied, fmt.Errorf("db: record migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return newlyApplied, fmt.Errorf("db: commit migration %d: %w", m.Version, err)
		}
		newlyApplied = append(newlyApplied, m.Version)
	}
	return newlyApplied, nil
}

// MigrateDown rolls back the `steps` most recently applied migrations
// (ordered by when they were actually applied - applied_at - not by
// version number; the two coincide under normal forward-only operation,
// but applied_at is the true record of order and is what a rollback must
// honor), most-recent first. Guarded by the same advisory lock as
// MigrateUp.
func (p *Pool) MigrateDown(ctx context.Context, dir string, steps int) ([]int64, error) {
	if steps <= 0 {
		return nil, fmt.Errorf("db: steps must be a positive number, got %d", steps)
	}

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: acquire migration connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("db: acquire migration lock: %w", err)
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey) }()

	migrations, err := LoadMigrations(dir)
	if err != nil {
		return nil, err
	}
	byVersion := map[int64]migration{}
	for _, m := range migrations {
		byVersion[m.Version] = m
	}

	rows, err := conn.Query(ctx,
		`SELECT version FROM schema_migrations ORDER BY applied_at DESC, version DESC LIMIT $1`,
		steps,
	)
	if err != nil {
		return nil, fmt.Errorf("db: query applied migrations: %w", err)
	}
	var versions []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, fmt.Errorf("db: scan applied version: %w", err)
		}
		versions = append(versions, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: read schema_migrations: %w", err)
	}

	var rolledBack []int64
	for _, v := range versions {
		m, ok := byVersion[v]
		if !ok {
			return rolledBack, fmt.Errorf("db: applied migration %d has no local file to roll back with", v)
		}
		sqlBytes, err := os.ReadFile(m.DownPath)
		if err != nil {
			return rolledBack, fmt.Errorf("db: read %s: %w", m.DownPath, err)
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return rolledBack, fmt.Errorf("db: begin rollback tx: %w", err)
		}
		if _, err := tx.Exec(ctx, string(sqlBytes)); err != nil {
			_ = tx.Rollback(ctx)
			return rolledBack, fmt.Errorf("db: roll back migration %d (%s): %w", m.Version, m.Description, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, v); err != nil {
			_ = tx.Rollback(ctx)
			return rolledBack, fmt.Errorf("db: unrecord migration %d: %w", v, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return rolledBack, fmt.Errorf("db: commit rollback %d: %w", v, err)
		}
		rolledBack = append(rolledBack, v)
	}
	return rolledBack, nil
}

// DescribeMigrations is a small debug helper used by the CLI to print
// what would run; kept here to stay next to the parsing logic it depends
// on.
func DescribeMigrations(migrations []migration) string {
	names := make([]string, 0, len(migrations))
	for _, m := range migrations {
		names = append(names, fmt.Sprintf("%04d_%s", m.Version, m.Description))
	}
	return strings.Join(names, ", ")
}
