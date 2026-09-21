package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
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
	// upSeen/downSeen catch a genuine PLAT-MIGDRIFT-1-adjacent duplicate:
	// two DIFFERENT filenames declaring the same version number for the
	// same up/down side (e.g. "0050_a.up.sql" and "0050_b.up.sql"). Without
	// this, the map-keyed-by-version logic below would silently let the
	// second file overwrite the first's UpPath/DownPath, masking a
	// duplicate version number instead of failing loudly.
	upSeen := map[int64]string{}
	downSeen := map[int64]string{}
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
			if prev, exists := upSeen[version]; exists {
				return nil, fmt.Errorf("db: duplicate migration version %d: both %q and %q declare an up file", version, prev, e.Name())
			}
			upSeen[version] = e.Name()
			mig.UpPath = full
			mig.Description = m[2]
		case "down":
			if prev, exists := downSeen[version]; exists {
				return nil, fmt.Errorf("db: duplicate migration version %d: both %q and %q declare a down file", version, prev, e.Name())
			}
			downSeen[version] = e.Name()
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

// findVersionGaps reports every version number missing between the lowest
// and highest version found in migrations - e.g. if versions 1, 2, 4 exist
// but 3 does not. True duplicate version numbers cannot reach this
// function: LoadMigrations itself already rejects two different filenames
// declaring the same version's up (or down) file, which is the only way a
// duplicate could arise from the on-disk migration set.
func findVersionGaps(migrations []migration) []string {
	if len(migrations) == 0 {
		return nil
	}
	versions := make([]int64, len(migrations))
	for i, m := range migrations {
		versions[i] = m.Version
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i] < versions[j] })

	var gaps []string
	for i := 1; i < len(versions); i++ {
		for missing := versions[i-1] + 1; missing < versions[i]; missing++ {
			gaps = append(gaps, fmt.Sprintf("missing migration version %d (gap between %d and %d)", missing, versions[i-1], versions[i]))
		}
	}
	return gaps
}

// hashMigrationSQL is the checksum algorithm schema_migrations.checksum
// stores: SHA-256 of the up-file's raw bytes, hex-encoded. Used both when
// recording a migration at apply time and when verifying it later - the
// two call sites must always agree on the algorithm.
func hashMigrationSQL(content []byte) string {
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}

const ensureSchemaMigrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version     BIGINT PRIMARY KEY,
	description TEXT NOT NULL,
	applied_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
`

// ensureSchemaMigrationsChecksumColumn adds schema_migrations.checksum
// (PLAT-MIGDRIFT-1 - see migration 0083's own doc comment for the full
// motivation) unconditionally, every MigrateUp run, BEFORE anything else
// touches schema_migrations. This is deliberately NOT gated on whether
// migration 0083 itself has already been "applied": schema_migrations is
// bootstrap infrastructure that Go code (this file), not the versioned
// migration chain, has always owned the shape of (see
// ensureSchemaMigrationsTable above, which predates this column by the
// same pattern) - a brand-new, empty database applying migrations 1..N in
// one MigrateUp call needs this column to exist before migration 1's own
// INSERT ever runs, which is before migration 0083 (wherever it sits in
// the chain) has been recorded as applied. Idempotent (IF NOT EXISTS), so
// running it on every call is free once the column exists.
const ensureSchemaMigrationsChecksumColumn = `
ALTER TABLE schema_migrations ADD COLUMN IF NOT EXISTS checksum TEXT;
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
	if _, err := conn.Exec(ctx, ensureSchemaMigrationsChecksumColumn); err != nil {
		return nil, fmt.Errorf("db: ensure schema_migrations checksum column: %w", err)
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
		// checksum is computed from the EXACT bytes just executed above -
		// the same value MigrateVerify recomputes later from the on-disk
		// file, so any subsequent edit to this file is detectable.
		checksum := hashMigrationSQL(sqlBytes)
		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, description, checksum) VALUES ($1, $2, $3)`,
			m.Version, m.Description, checksum,
		); err != nil {
			_ = tx.Rollback(ctx)
			return newlyApplied, fmt.Errorf("db: record migration %d: %w", m.Version, err)
		}
		if err := tx.Commit(ctx); err != nil {
			return newlyApplied, fmt.Errorf("db: commit migration %d: %w", m.Version, err)
		}
		newlyApplied = append(newlyApplied, m.Version)
	}

	// Automatic, transparent backfill (PLAT-MIGDRIFT-1): any row already in
	// schema_migrations from before checksum tracking existed has
	// checksum=NULL. Backfill it from the CURRENT on-disk file content -
	// this is the only content available; it is NOT the content that was
	// actually applied when that migration originally ran, so this cannot
	// retroactively detect an edit that happened before this backfill runs
	// (see docs/architecture/38-deployment-architecture.md's migrate-verify
	// scope note). It DOES mean every legacy migration gets a real
	// baseline going forward: any edit AFTER this backfill point is
	// detected by `migrate verify` exactly like a normal one. Deliberately
	// unconditional and part of every `up` run - no separate manual step.
	if err := backfillMissingChecksums(ctx, conn, migrations); err != nil {
		return newlyApplied, err
	}

	return newlyApplied, nil
}

// backfillMissingChecksums fills in schema_migrations.checksum for every
// already-applied row that predates checksum tracking (checksum IS NULL).
// See MigrateUp's own call-site comment for exactly what this can and
// cannot detect.
func backfillMissingChecksums(ctx context.Context, conn *pgxpool.Conn, migrations []migration) error {
	byVersion := make(map[int64]migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}

	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations WHERE checksum IS NULL`)
	if err != nil {
		return fmt.Errorf("db: query migrations missing a checksum: %w", err)
	}
	var pending []int64
	for rows.Next() {
		var v int64
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return fmt.Errorf("db: scan version pending checksum backfill: %w", err)
		}
		pending = append(pending, v)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return fmt.Errorf("db: read migrations missing a checksum: %w", err)
	}

	for _, v := range pending {
		m, ok := byVersion[v]
		if !ok {
			// Applied historically but its file no longer exists on disk -
			// nothing to backfill from. Left NULL deliberately so
			// VerifyMigrations reports this explicitly (status
			// MigrationCheckUnverifiable) rather than fabricating a value.
			continue
		}
		sqlBytes, err := os.ReadFile(m.UpPath)
		if err != nil {
			return fmt.Errorf("db: read %s for checksum backfill: %w", m.UpPath, err)
		}
		checksum := hashMigrationSQL(sqlBytes)
		if _, err := conn.Exec(ctx, `UPDATE schema_migrations SET checksum = $1 WHERE version = $2`, checksum, v); err != nil {
			return fmt.Errorf("db: backfill checksum for migration %d: %w", v, err)
		}
	}
	return nil
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

// MigrationCheckStatus is the outcome VerifyMigrations reports for one
// already-applied migration.
type MigrationCheckStatus string

const (
	// MigrationCheckOK: the on-disk up-file's current hash matches the
	// checksum recorded when it was applied. No drift detected.
	MigrationCheckOK MigrationCheckStatus = "ok"
	// MigrationCheckMismatch: the on-disk up-file's current hash does NOT
	// match the recorded checksum - the file was edited after being
	// applied to this database (PLAT-MIGDRIFT-1, the exact defect class
	// this feature exists to catch).
	MigrationCheckMismatch MigrationCheckStatus = "mismatch"
	// MigrationCheckUnverifiable: schema_migrations has no checksum
	// recorded for this version (it predates checksum tracking and has
	// not yet been backfilled by a `migrate up` run against this
	// database). This is NOT evidence of drift - it means "no baseline
	// exists to compare against yet" - so callers should surface it
	// distinctly from MigrationCheckMismatch rather than treating it as a
	// detected problem.
	MigrationCheckUnverifiable MigrationCheckStatus = "unverifiable"
	// MigrationCheckMissingFile: schema_migrations records this version as
	// applied, but no on-disk migration file exists for it any more (the
	// file was deleted, or -dir points at the wrong directory).
	MigrationCheckMissingFile MigrationCheckStatus = "missing_file"
)

// MigrationCheckResult is VerifyMigrations' per-migration finding.
type MigrationCheckResult struct {
	Version     int64
	Description string
	Status      MigrationCheckStatus
	Detail      string
}

// VerifyReport is VerifyMigrations' full result: one MigrationCheckResult
// per row in schema_migrations, plus any gap found in the on-disk version
// sequence (LoadMigrations already rejects true duplicate version numbers
// outright, so VerifyReport itself only needs to carry gaps).
type VerifyReport struct {
	Results     []MigrationCheckResult
	VersionGaps []string
}

// OK reports whether verification found no actionable problem: every
// result is MigrationCheckOK (MigrationCheckUnverifiable does NOT fail
// this - see its own doc comment) and there are no version gaps.
func (r *VerifyReport) OK() bool {
	if len(r.VersionGaps) > 0 {
		return false
	}
	for _, res := range r.Results {
		if res.Status == MigrationCheckMismatch || res.Status == MigrationCheckMissingFile {
			return false
		}
	}
	return true
}

// VerifyMigrations is PLAT-MIGDRIFT-1's actual detection mechanism (see
// docs/architecture/38-deployment-architecture.md's migrate-verify scope
// note for the precise, exhaustive statement of what this does and does
// not check). In short, for every row in schema_migrations it recomputes
// the current on-disk up-file's hash and compares it against the checksum
// recorded when that migration was applied, and separately checks the
// full on-disk migration set for gaps in the version sequence.
//
// It does NOT check: down-files (a `.down.sql` being edited after the
// fact is invisible to this - only the up-file that was actually run is
// hashed), the live database schema against what the migration SQL
// claims to produce (a checksum match only proves the FILE is unchanged,
// not that the schema itself was never separately hand-altered outside
// the migration chain), or anything for a migration version that predates
// checksum tracking and has never had `migrate up` run against this
// database since (MigrationCheckUnverifiable, not a failure).
func (p *Pool) VerifyMigrations(ctx context.Context, dir string) (*VerifyReport, error) {
	migrations, err := LoadMigrations(dir)
	if err != nil {
		return nil, err
	}

	report := &VerifyReport{VersionGaps: findVersionGaps(migrations)}

	byVersion := make(map[int64]migration, len(migrations))
	for _, m := range migrations {
		byVersion[m.Version] = m
	}

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("db: acquire verify connection: %w", err)
	}
	defer conn.Release()

	var hasChecksumColumn bool
	if err := conn.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name = 'schema_migrations' AND column_name = 'checksum')`,
	).Scan(&hasChecksumColumn); err != nil {
		return nil, fmt.Errorf("db: check schema_migrations checksum column: %w", err)
	}
	if !hasChecksumColumn {
		return nil, fmt.Errorf("db: schema_migrations has no checksum column yet - run `migrate up` first; it adds the column and backfills existing rows automatically")
	}

	rows, err := conn.Query(ctx, `SELECT version, description, checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("db: query schema_migrations: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var version int64
		var description string
		var checksum *string
		if err := rows.Scan(&version, &description, &checksum); err != nil {
			return nil, fmt.Errorf("db: scan schema_migrations row: %w", err)
		}

		m, ok := byVersion[version]
		if !ok {
			report.Results = append(report.Results, MigrationCheckResult{
				Version: version, Description: description, Status: MigrationCheckMissingFile,
				Detail: "recorded as applied in schema_migrations but no on-disk migration file exists for this version",
			})
			continue
		}
		if checksum == nil {
			report.Results = append(report.Results, MigrationCheckResult{
				Version: version, Description: description, Status: MigrationCheckUnverifiable,
				Detail: "no checksum recorded (applied before checksum tracking existed; the next `migrate up` run backfills this automatically)",
			})
			continue
		}
		sqlBytes, err := os.ReadFile(m.UpPath)
		if err != nil {
			return nil, fmt.Errorf("db: read %s: %w", m.UpPath, err)
		}
		actual := hashMigrationSQL(sqlBytes)
		if actual != *checksum {
			report.Results = append(report.Results, MigrationCheckResult{
				Version: version, Description: description, Status: MigrationCheckMismatch,
				Detail: fmt.Sprintf("%s no longer matches the content recorded when this migration was applied (recorded checksum %s, current checksum %s) - the file was edited after being applied to this database", m.UpPath, *checksum, actual),
			})
			continue
		}
		report.Results = append(report.Results, MigrationCheckResult{Version: version, Description: description, Status: MigrationCheckOK})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("db: read schema_migrations: %w", err)
	}

	return report, nil
}
