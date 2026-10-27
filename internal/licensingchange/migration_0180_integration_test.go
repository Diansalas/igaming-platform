//go:build integration

package licensingchange

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// 0180 up / down / up on a throwaway database: the down restores the 0179 schema byte for byte (the 0178 classifier, the 0179 row
// matcher, the 0076 policy-write trigger, the 0076 / 0077 read policies, no operating_bases, no kind rows), the re-up equals the
// first up, no operating_bases row ships, and the down is refused (LC095) while an operating_bases row or an operating-basis
// request exists, changing nothing.
func TestMigration0180_UpDownUp_SnapshotAndDownRefusal(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0180_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, _ := migrationsThrough(t, 180)
	base, _ := migrationsThrough(t, 179)
	if _, err := pool.MigrateUp(ctx, base); err != nil {
		t.Fatalf("migrate to 0179: %v", err)
	}
	before := schemaSnapshot(t, pool)
	if applied, err := pool.MigrateUp(ctx, full); err != nil || len(applied) != 1 || applied[0] != 180 {
		t.Fatalf("0180 up: %v %v", applied, err)
	}
	up := schemaSnapshot(t, pool)
	for _, want := range []string{"table:operating_bases:rls=true:force=true", "zz_licensing_change_governed", "operating_bases_a_validate",
		"operating_bases_b_append_only", "uq_operating_bases_open_brand", "operating_bases_tenant_read", "operating_bases_platform_insert"} {
		if !strings.Contains(up, want) {
			t.Fatalf("0180 up must create %q", want)
		}
	}
	// The ACL: the runtime role holds SELECT and INSERT plus UPDATE on effective_to ONLY; never DELETE, TRUNCATE or a blanket UPDATE.
	var delete_, truncate, update_, selectOK, insertOK, closeOK bool
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', 'operating_bases', 'DELETE'), has_table_privilege('igaming_runtime', 'operating_bases', 'TRUNCATE'),
			has_table_privilege('igaming_runtime', 'operating_bases', 'UPDATE'), has_table_privilege('igaming_runtime', 'operating_bases', 'SELECT'),
			has_table_privilege('igaming_runtime', 'operating_bases', 'INSERT'), has_column_privilege('igaming_runtime', 'operating_bases', 'effective_to', 'UPDATE')`).
			Scan(&delete_, &truncate, &update_, &selectOK, &insertOK, &closeOK)
	}); err != nil {
		t.Fatal(err)
	}
	if delete_ || truncate || update_ || !selectOK || !insertOK || !closeOK {
		t.Fatalf("runtime ACL on operating_bases: delete=%v truncate=%v update=%v select=%v insert=%v close=%v", delete_, truncate, update_, selectOK, insertOK, closeOK)
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("0180 down: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != before {
		t.Fatalf("0180 down must restore the 0179 schema byte for byte; %s", firstDiff(before, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0180 up again: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatalf("0180 up again must equal the first up; %s", firstDiff(up, got))
	}

	// No value ships: zero operating_bases rows and exactly three new kind rows.
	var rows, kinds int
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `ALTER TABLE operating_bases NO FORCE ROW LEVEL SECURITY`); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM operating_bases`).Scan(&rows); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `ALTER TABLE operating_bases FORCE ROW LEVEL SECURITY`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithPlatformAdmin(ctx, uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM licensing_change_kinds WHERE kind LIKE 'operating\_basis\_%'`).Scan(&kinds)
	}); err != nil {
		t.Fatal(err)
	}
	if rows != 0 || kinds != 3 {
		t.Fatalf("0180 must ship zero operating_bases rows and three kinds, found %d rows, %d kinds", rows, kinds)
	}

	// Refusal while an operating-basis request exists.
	prooftest.InstallVia(t, pool.Raw(), u)
	launchfix.SeedOperatorsAt(t, u)
	ops := launchfix.Operators()
	var tid, jid uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0180", "m0180-"+uuid.NewString(), "under_platform_licence")
		tid = tn.ID
		if err != nil {
			return err
		}
		jid = uuid.New()
		_, err = tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'm0180')`, jid, "M-"+strings.ToUpper(jid.String()[:8]))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	svc, err := New(Config{Pool: pool})
	if err != nil {
		t.Fatal(err)
	}
	due := dueIn(30)
	if _, err := svc.Request(ctx, Actor{StaffID: ops.Requester, Scope: ScopePlatform, Role: string(auth.RolePlatformAdmin)},
		RequestInput{Kind: KindOperatingBasisSet, Payload: payload(t, OperatingBasisSetPayload{
			TenantID: tid, CountryCode: "MT", OperationCode: "deposit", BasisKind: "no_licence_documented", JurisdictionID: &jid,
			DeterminationReference: "DET-M0180", DeterminedByRole: "head_of_compliance", RestrictedProductCodes: []string{}, ReviewDueAt: due}),
			ReasonCode: "m0180"}); err != nil {
		t.Fatalf("seed a governed change: %v", err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "LC095") {
		t.Fatalf("0180 down must refuse LC095 while an operating-basis request exists, got %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatal("a refused down must change nothing")
	}
}

// The down is also refused while an operating_bases row exists (here an owner-path row: the decision record would be lost).
func TestMigration0180_DownRefusedWhileRowsExist(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0180r_")
	pool, err := db.Connect(ctx, u, 8, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	full, _ := migrationsThrough(t, 180)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate to 0180: %v", err)
	}
	launchfix.SeedOperatorsAt(t, u)
	ops := launchfix.Operators()
	if err := pool.WithPlatformAdmin(ctx, ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0180r", "m0180r-"+uuid.NewString(), "under_platform_licence")
		if err != nil {
			return err
		}
		jid := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'm0180r')`, jid, "R-"+strings.ToUpper(jid.String()[:8])); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO operating_bases (tenant_id, country_code, operation_code, basis_kind, jurisdiction_id, determination_reference,
			determined_by_role, review_due_at, status, reason_code, policy_version)
			VALUES ($1, 'MT', 'deposit', 'no_licence_documented', $2, 'DET-R', 'head_of_compliance', current_date + 30, 'active', 'x', 'fixture')`, tn.ID, jid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err = pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "LC095") {
		t.Fatalf("0180 down must refuse LC095 while an operating_bases row exists, got %v", err)
	}
}
