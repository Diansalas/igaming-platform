//go:build integration

// Real-PostgreSQL tests for migration 0076's jurisdictions.country_code
// column and its Go write surface (CreateJurisdiction/
// SetJurisdictionCountryCode) - ADR 0045 §1.1.
package jurisdiction

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// TestJurisdictionCountryCode_RejectsNonISOAndLowercase covers the shape
// CHECK (DB level), the Go ISO-3166 assignment check, and the
// trim-only/never-uppercase normalization rule - on both CreateJurisdiction
// and SetJurisdictionCountryCode.
func TestJurisdictionCountryCode_RejectsNonISOAndLowercase(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	cases := []struct {
		name string
		code string
	}{
		{"lowercase rejected, never coerced", "mt"},
		{"not a currently-assigned code", "ZZ"},
		{"wrong shape - three letters", "MTA"},
		{"wrong shape - digits", "M1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
				_, err := CreateJurisdiction(ctx, tx, CreateJurisdictionParams{
					Code: "CC-" + tc.code, Name: "Test", CountryCode: tc.code,
					Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"},
				})
				return err
			})
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("expected ErrInvalidInput for country_code %q, got %v", tc.code, err)
			}
		})
	}

	// A valid, uppercase, currently-assigned code is accepted.
	var j Jurisdiction
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		j, err = CreateJurisdiction(ctx, tx, CreateJurisdictionParams{
			Code: "CC-VALID-" + uuid.New().String()[:8], Name: "Test Valid", CountryCode: "MT",
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("CreateJurisdiction with a valid country_code: %v", err)
	}
	if j.CountryCode != "MT" {
		t.Fatalf("expected country_code MT, got %q", j.CountryCode)
	}

	// SetJurisdictionCountryCode: same rejection rules (lowercase is
	// rejected, never coerced).
	if err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetJurisdictionCountryCode(ctx, tx, j.ID, "co", ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"})
		return err
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("expected ErrInvalidInput for lowercase SetJurisdictionCountryCode, got %v", err)
	}

	var updated Jurisdiction
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = SetJurisdictionCountryCode(ctx, tx, j.ID, "CO", ActorContext{ActorID: f.platformAdmin, ReasonCode: "test-update"})
		return err
	})
	if err != nil {
		t.Fatalf("SetJurisdictionCountryCode with a valid code: %v", err)
	}
	if updated.CountryCode != "CO" {
		t.Fatalf("expected country_code CO after update, got %q", updated.CountryCode)
	}

	// Clearing it back to NULL.
	var cleared Jurisdiction
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		cleared, err = SetJurisdictionCountryCode(ctx, tx, j.ID, "", ActorContext{ActorID: f.platformAdmin, ReasonCode: "test-clear"})
		return err
	})
	if err != nil {
		t.Fatalf("SetJurisdictionCountryCode clearing to empty: %v", err)
	}
	if cleared.CountryCode != "" {
		t.Fatalf("expected empty country_code after clearing, got %q", cleared.CountryCode)
	}

	// Raw SQL: the DB-level shape CHECK independently rejects a malformed
	// value, proving the Go validator is not the only line of defense.
	// Stage 4I Phase E-SECURITY (migration 0077): this UPDATE must run
	// under a genuinely platform-admin-scoped transaction - a WithoutTenant
	// attempt would now be denied by RLS as a silent zero-row no-op
	// (raising no error at all), which would make this test observe nil
	// instead of the CHECK violation it exists to prove.
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE jurisdictions SET country_code = 'zz9' WHERE id = $1`, j.ID)
		return err
	})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgCheckViolation {
		t.Fatalf("expected a CHECK violation (%s) from the raw SQL update, got %v", pgCheckViolation, err)
	}
}

// TestSetJurisdictionCountryCode_RequiresPlatformScope is Fix 2 (Phase E
// fix round): SetJurisdictionCountryCode asserts platform scope as its
// FIRST statement. Through Stage 4I Phase E, `jurisdictions` carried ZERO
// row-level security, so this Go-level check was the ONLY control;
// Stage 4I Phase E-SECURITY (migration 0077) additionally gave
// `jurisdictions` a database-level RLS backstop for writes (this Go-level
// check remains the first, more diagnosable line of defense). Tenant-scoped
// and player-scoped transactions must both be refused with
// ErrTransactionScope and leave the row unmodified; a genuine
// platform-admin transaction succeeds.
func TestSetJurisdictionCountryCode_RequiresPlatformScope(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)

	var j Jurisdiction
	err := pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		j, err = CreateJurisdiction(ctx, tx, CreateJurisdictionParams{
			Code: "SJC-" + uuid.New().String()[:8], Name: "Scope Test Jurisdiction",
			Actor: ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction: %v", err)
	}

	readCountryCode := func() *string {
		t.Helper()
		var v *string
		err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT country_code FROM jurisdictions WHERE id = $1`, j.ID).Scan(&v)
		})
		if err != nil {
			t.Fatalf("read raw country_code: %v", err)
		}
		return v
	}
	if v := readCountryCode(); v != nil {
		t.Fatalf("sanity: expected NULL country_code before any write, got %q", *v)
	}

	// Tenant-scoped: refused.
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetJurisdictionCountryCode(ctx, tx, j.ID, "MT", ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope for a tenant-scoped transaction, got %v", err)
	}
	if v := readCountryCode(); v != nil {
		t.Fatalf("expected the refused tenant-scoped attempt to leave country_code unmodified, got %q", *v)
	}

	// Player-scoped: refused.
	err = pool.WithPlayerScope(context.Background(), f.tenantID, f.playerID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := SetJurisdictionCountryCode(ctx, tx, j.ID, "MT", ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"})
		return err
	})
	if !errors.Is(err, ErrTransactionScope) {
		t.Fatalf("expected ErrTransactionScope for a player-scoped transaction, got %v", err)
	}
	if v := readCountryCode(); v != nil {
		t.Fatalf("expected the refused player-scoped attempt to leave country_code unmodified, got %q", *v)
	}

	// Platform-admin-scoped: succeeds.
	var updated Jurisdiction
	err = pool.WithPlatformAdmin(context.Background(), f.platformAdmin, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		updated, err = SetJurisdictionCountryCode(ctx, tx, j.ID, "MT", ActorContext{ActorID: f.platformAdmin, ReasonCode: "test"})
		return err
	})
	if err != nil {
		t.Fatalf("expected a platform-admin-scoped SetJurisdictionCountryCode to succeed, got %v", err)
	}
	if updated.CountryCode != "MT" {
		t.Fatalf("expected country_code MT after the platform-admin write, got %q", updated.CountryCode)
	}
}

// TestMigration0076_LeavesExistingJurisdictionCountryCodesNull confirms
// migration 0076 auto-assigns/backfills nothing to PRE-EXISTING rows -
// genuinely, not merely by creating rows after the migration already ran
// (Phase E fix round item 3: the original version of this test only ever
// created jurisdiction rows AFTER migration 0076 had already run, so it
// never exercised the "no auto-assignment to PRE-EXISTING rows" property
// its own name and doc comment claimed to guard). This version, on a
// scratch database, inserts a jurisdiction row via raw SQL BEFORE running
// migration 0076 at all (mirroring migration_0075_integration_test.go's
// own scratch-DB pattern), then applies 0076, then asserts that
// pre-existing row's country_code is still NULL. It also independently
// asserts the migration's own .up.sql source text contains no `UPDATE
// jurisdictions` statement (mirroring this file's sibling
// country_code_test.go's static-source-grep approach) as a second,
// independent guard against the same property.
func TestMigration0076_LeavesExistingJurisdictionCountryCodesNull(t *testing.T) {
	scratchURL := migration0075ScratchDatabase(t)
	pool := migration0075ScratchPool(t, scratchURL)
	srcDir := migration0075MigrationsDir(t)

	stagedDir, addHeldBack0076 := stageMigrationsWithout0076(t, srcDir)

	if _, err := pool.MigrateUp(context.Background(), stagedDir); err != nil {
		t.Fatalf("migrate up through 0075 (0076 held back): %v", err)
	}

	// Insert a jurisdiction row via raw SQL BEFORE migration 0076 runs -
	// at this point jurisdictions.country_code does not even exist as a
	// column yet, so this row is genuinely "pre-existing" with respect to
	// the migration under test.
	preExistingID := uuid.New()
	err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Pre-0076 Jurisdiction')`,
			preExistingID, "PRE076-"+preExistingID.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed a pre-existing jurisdiction row before migration 0076: %v", err)
	}

	addHeldBack0076()
	if _, err := pool.MigrateUp(context.Background(), stagedDir); err != nil {
		t.Fatalf("apply migration 0076: %v", err)
	}

	var raw *string
	err = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT country_code FROM jurisdictions WHERE id = $1`, preExistingID).Scan(&raw)
	})
	if err != nil {
		t.Fatalf("read raw country_code for the pre-existing row: %v", err)
	}
	if raw != nil {
		t.Fatalf("expected the PRE-EXISTING jurisdiction row's country_code to remain NULL after migration 0076 ran, got %q - migration 0076 must never auto-assign/backfill it", *raw)
	}

	// Second, independent guard: the migration's own .up.sql source text
	// must contain no UPDATE jurisdictions statement of any kind.
	upSQL, err := os.ReadFile(filepath.Join(srcDir, "0076_operating_market_country_policy.up.sql"))
	if err != nil {
		t.Fatalf("read migration 0076 up.sql: %v", err)
	}
	if strings.Contains(strings.ToUpper(string(upSQL)), "UPDATE JURISDICTIONS") {
		t.Fatal("migration 0076's up.sql source contains an UPDATE JURISDICTIONS statement - it must never backfill/auto-assign country_code to existing rows")
	}
}

// stageMigrationsWithout0076 copies the real migrations into a temp
// directory, holding migration 0076's up/down files back, and returns the
// directory plus a function that adds them back in (to be applied by a
// subsequent MigrateUp call). Nothing in this migration chain depends on
// 0076's prior state for its OWN schema (it added new tables/columns, not
// a rewrite of an earlier one), so holding only 0076 back would be
// sufficient for 0076's own schema - unlike migration_0048_integration_
// test.go's own analogous helper, which must also hold back later
// migrations structurally dependent on 0048's prior state.
//
// STAGE 4I PHASE E-SECURITY UPDATE: migration 0077 must ALSO be held back
// here, even though its own SQL does not reference anything 0076 added -
// this test's whole premise is proving a raw, unscoped (WithoutTenant)
// INSERT into `jurisdictions` succeeds at the "before 0076" checkpoint,
// and migration 0077 is what makes that INSERT require a platform-admin-
// scoped transaction. Without also holding 0077 back, LoadMigrations
// would apply it in the SAME MigrateUp call that is meant to stop at
// 0075, and the raw INSERT below would fail with an RLS violation instead
// of proving the property this test exists to prove. Held-back versions
// are therefore every migration numbered 76 or above, not literally just
// "0076_" by name - this keeps the helper correct automatically if a
// future migration is added after 0077 without anyone remembering to
// update a hardcoded prefix list here.
func stageMigrationsWithout0076(t *testing.T, src string) (dir string, addHeld func()) {
	t.Helper()
	dir = t.TempDir()

	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var held []string
	copyFile := func(name string) {
		content, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), content, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	versionPattern := regexp.MustCompile(`^(\d+)_`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		if m := versionPattern.FindStringSubmatch(e.Name()); m != nil {
			version, err := strconv.Atoi(m[1])
			if err != nil {
				t.Fatalf("parse migration version from %q: %v", e.Name(), err)
			}
			if version >= 76 {
				held = append(held, e.Name())
				continue
			}
		}
		copyFile(e.Name())
	}
	if len(held) == 0 {
		t.Fatalf("expected to hold back at least migration 0076's up/down files, held none")
	}
	if len(held)%2 != 0 {
		t.Fatalf("expected an even number of held-back files (each migration has an up and a down), held %v", held)
	}
	return dir, func() {
		for _, name := range held {
			copyFile(name)
		}
	}
}
