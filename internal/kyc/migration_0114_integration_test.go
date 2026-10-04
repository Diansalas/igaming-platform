//go:build integration

package kyc

// PRH-2 E1 (ADR 0106 section 10.7, tests 52-57): migration 0114 up, down and
// refusal behaviour. The scratch databases are owned by the non-superuser,
// non-BYPASSRLS test owner role (internal/testsupport/scratchdb); shared-DB
// assertions run as the runtime role.

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0114Version = int64(114)

// scratchThrough0114 migrates a fresh scratch DB through 0114 only, so "down one
// step" is always exactly 0114 however many later migrations exist on disk.
func scratchThrough0114(t *testing.T, prefix string) (*db.Pool, string) {
	t.Helper()
	src := "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < 4 {
			continue
		}
		n, perr := strconv.ParseInt(name[:4], 10, 64)
		if perr != nil || n > migration0114Version {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(src, name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(filepath.Join(dir, name), b, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch up through %d: %v", migration0114Version, err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != migration0114Version {
		t.Fatalf("expected %d to be the last applied migration, got %v", migration0114Version, applied)
	}
	return pool, dir
}

// scratchState captures what a refused down must leave intact.
type scratchState struct {
	table, forceOnOutbox bool
	fences               int
	kind                 bool
	forceOnKinds         bool
	denyTrigger          bool
	policies             []string
}

func readScratchState(t *testing.T, pool *db.Pool) scratchState {
	t.Helper()
	var s scratchState
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT to_regclass('public.kyc_submission_outbox') IS NOT NULL`).Scan(&s.table); err != nil {
			return err
		}
		if s.table {
			if err := tx.QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE oid = 'public.kyc_submission_outbox'::regclass`).Scan(&s.forceOnOutbox); err != nil {
				return err
			}
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_policies WHERE policyname LIKE 'kyc_worker_fence_%'`).Scan(&s.fences); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alert_kinds WHERE kind = 'kyc.submission_failed_terminal')`).Scan(&s.kind); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE oid = 'public.alert_kinds'::regclass`).Scan(&s.forceOnKinds); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = 'public.alert_kinds'::regclass AND tgname = 'alert_kinds_deny_update_delete' AND tgenabled = 'O')`).Scan(&s.denyTrigger); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT policyname FROM pg_policies WHERE tablename = 'alert_kinds' ORDER BY policyname`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				return err
			}
			s.policies = append(s.policies, n)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("read scratch state: %v", err)
	}
	return s
}

func assertIntact(t *testing.T, pool *db.Pool, label string) {
	t.Helper()
	s := readScratchState(t, pool)
	if !s.table || !s.forceOnOutbox || s.fences != 36 || !s.kind || !s.forceOnKinds || !s.denyTrigger {
		t.Fatalf("%s: a refused down must leave everything intact, got %+v", label, s)
	}
	if strings.Join(s.policies, ",") != "alert_kinds_read_all,alerts_platform_service_dispatcher" {
		t.Fatalf("%s: alert_kinds policies = %v, want exactly {alert_kinds_read_all, alerts_platform_service_dispatcher}", label, s.policies)
	}
}

// 52. Up (shared DB, runtime role for the grant/seed assertions).
func TestMigration0114_52_UpShape(t *testing.T) {
	rt := rtPool(t, 3)
	var (
		forced bool
		pols   []string
		privs  []string
	)
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT relforcerowsecurity AND relrowsecurity FROM pg_class WHERE oid = 'public.kyc_submission_outbox'::regclass`).Scan(&forced); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT policyname || ' ' || cmd FROM pg_policies WHERE tablename = 'kyc_submission_outbox' ORDER BY policyname`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				rows.Close()
				return err
			}
			pols = append(pols, s)
		}
		rows.Close()
		for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
			var has bool
			if err := tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime', 'kyc_submission_outbox', $1)`, p).Scan(&has); err != nil {
				return err
			}
			if has {
				privs = append(privs, p)
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if !forced {
		t.Error("kyc_submission_outbox must be ENABLE + FORCE ROW LEVEL SECURITY")
	}
	want := []string{"kso_kyc_worker_claim UPDATE", "kso_kyc_worker_select SELECT", "kso_tenant_insert INSERT", "kso_tenant_select SELECT", "kso_tenant_update UPDATE"}
	if strings.Join(pols, "|") != strings.Join(want, "|") {
		t.Errorf("outbox policies = %v, want exactly %v (no FOR ALL, no DELETE policy)", pols, want)
	}
	if strings.Join(privs, ",") != "SELECT,INSERT,UPDATE" {
		t.Errorf("the runtime role privileges on the outbox = %v, want exactly SELECT, INSERT, UPDATE", privs)
	}

	s := readScratchState(t, rt)
	if !s.forceOnKinds || strings.Join(s.policies, ",") != "alert_kinds_read_all,alerts_platform_service_dispatcher" {
		t.Errorf("alert_kinds after up: force=%v policies=%v (the temporary seed policy must be gone, FORCE never toggled)", s.forceOnKinds, s.policies)
	}
	// The Kind row, exactly as seeded.
	var severity, scope, mode string
	var sim, subj, tenantRaisable bool
	var keys []string
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode FROM alert_kinds WHERE kind = 'kyc.submission_failed_terminal'`).
			Scan(&severity, &scope, &sim, &subj, &tenantRaisable, &keys, &mode)
	}); err != nil {
		t.Fatalf("read the Kind: %v", err)
	}
	sort.Strings(keys)
	if severity != "p2" || scope != "platform" || sim || !subj || !tenantRaisable || mode != "in_tx" || strings.Join(keys, ",") != "last_error_class,operation,outbox_id,provider_id" {
		t.Errorf("Kind row = %s %s sim=%v subj=%v raisable=%v %s %v", severity, scope, sim, subj, tenantRaisable, mode, keys)
	}
	// The runtime role can never write alert_kinds (only a migration does).
	err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode)
		                         VALUES ('kyc.runtime_probe', 'p3', 'platform', false, true, true, ARRAY['operation'], 'in_tx')`)
		return err
	})
	if pgCode(err) != "42501" {
		t.Errorf("a runtime-role INSERT INTO alert_kinds must be refused (42501), got %v", err)
	}
}

// Static structure of the seed block (Q-S2 conditions) and the down: literal
// SQL, FOR INSERT TO CURRENT_USER, equality on exactly one Kind, CREATE / INSERT
// / DROP inside ONE DO block, FORCE RLS never toggled on alert_kinds, the down
// refusal first, in one block.
func TestMigration0114_QS2_SeedAndDownStructure(t *testing.T) {
	root := kycRepoRoot(t)
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, "migrations", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	up, down := read("0114_kyc_submission_outbox.up.sql"), read("0114_kyc_submission_outbox.down.sql")

	idx := strings.Index(up, "CREATE POLICY alert_kinds_seed_0114")
	if idx < 0 {
		t.Fatal("seed policy not found")
	}
	// The DO block containing it.
	start := strings.LastIndex(up[:idx], "DO $$")
	end := idx + strings.Index(up[idx:], "$$;") + 3
	block := up[start:end]
	for _, must := range []string{
		"CREATE POLICY alert_kinds_seed_0114 ON alert_kinds",
		"FOR INSERT TO CURRENT_USER",
		"WITH CHECK (kind = 'kyc.submission_failed_terminal')",
		"INSERT INTO alert_kinds",
		"DROP POLICY alert_kinds_seed_0114 ON alert_kinds",
	} {
		if !strings.Contains(block, must) {
			t.Errorf("the seed DO block lacks %q", must)
		}
	}
	if strings.Contains(strings.ToUpper(block), "EXECUTE") {
		t.Error("the seed block must be literal SQL, never EXECUTE")
	}
	if strings.Count(block, "'kyc.submission_failed_terminal'") != 2 {
		t.Error("the seed block must name exactly the one Kind (policy + insert)")
	}
	for name, sql := range map[string]string{"up": up, "down": down} {
		if strings.Contains(sql, "ALTER TABLE alert_kinds NO FORCE") || strings.Contains(sql, "ALTER TABLE alert_kinds FORCE") {
			t.Errorf("%s: FORCE ROW LEVEL SECURITY on alert_kinds must never be toggled", name)
		}
	}
	// Down: one DO block, the refusal BEFORE any drop.
	if strings.Count(down, "DO $$") != 1 || strings.Count(down, "$$;") != 1 {
		t.Error("the down must be exactly ONE atomic DO block (F7)")
	}
	refuse, dropTable, unseed := strings.Index(down, "RAISE EXCEPTION 'migration 0114 down: refusing"), strings.Index(down, "DROP TABLE kyc_submission_outbox"), strings.Index(down, "DELETE FROM alert_kinds")
	if refuse < 0 || dropTable < 0 || unseed < 0 || refuse >= dropTable || dropTable >= unseed {
		t.Errorf("the outbox refusal must come first, then the drops, then the unseed (refuse=%d drop=%d unseed=%d)", refuse, dropTable, unseed)
	}
	if !strings.Contains(down, "SELECT count(*) INTO v_count FROM kyc_submission_outbox") || strings.Contains(down, "WHERE state") {
		t.Error("the down must refuse on ANY outbox row, not a subset")
	}
}

// 53. Down on an empty scratch DB succeeds; up again; migrate verify is clean.
func TestMigration0114_53_UpDownUp(t *testing.T) {
	pool, dir := scratchThrough0114(t, "kyc0114updown")
	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if s := readScratchState(t, pool); s.table || s.fences != 0 || s.kind || !s.forceOnKinds || !s.denyTrigger ||
		strings.Join(s.policies, ",") != "alert_kinds_read_all,alerts_platform_service_dispatcher" {
		t.Fatalf("after down: %+v", s)
	}
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil || len(applied) != 1 || applied[0] != migration0114Version {
		t.Fatalf("up again: %v %v", applied, err)
	}
	assertIntact(t, pool, "after the round trip")
	report, err := pool.VerifyMigrations(context.Background(), dir)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !report.OK() {
		t.Fatalf("expected a clean migration report after the round trip, got %+v", report)
	}
}

// 54. Down refuses with a pending row, a claimed row, and with ONLY terminal
// rows; after each refusal everything is intact (M20, M33).
func TestMigration0114_54_DownRefusesOnAnyRow(t *testing.T) {
	for _, mode := range []string{"pending", "claimed", "sent_only", "failed_terminal_only", "cancelled_only"} {
		t.Run(mode, func(t *testing.T) {
			pool, dir := scratchThrough0114(t, "kyc0114refuse")
			f := seedFixture(t, pool)
			v, _ := requestCreate(t, pool, f, "mock")
			switch mode {
			case "claimed":
				claimOne(t, workerFor(pool, NewMockOutboundResolver(), NewMockKYCProvider()))
			case "sent_only":
				passUntilQuiet(t, workerFor(pool, NewMockOutboundResolver(), NewMockKYCProvider()))
			case "failed_terminal_only":
				passUntilQuiet(t, workerFor(pool, mismatchedKYCOutboundResolver{}, NewMockKYCProvider()))
			case "cancelled_only":
				orch := NewOrchestrator(map[string]KYCProvider{"mock": NewMockKYCProvider(), "mock2": renamedMock{NewMockKYCProvider(), "mock2"}}, nil)
				passUntilQuiet(t, NewOutboxWorker(pool, orch, NewMockOutboundResolver()))
			}
			wantState := map[string]OutboxState{"pending": OutboxPending, "claimed": OutboxClaimed, "sent_only": OutboxSent,
				"failed_terminal_only": OutboxFailedTerminal, "cancelled_only": OutboxCancelled}[mode]
			if got := onlyRow(t, pool, f.tenantID, v.ID, OpCreate); got.State != wantState {
				t.Fatalf("setup: row = %s, want %s", got.State, wantState)
			}
			if _, err := pool.MigrateDown(context.Background(), dir, 1); err == nil {
				t.Fatal("the down must refuse while ANY outbox row exists")
			} else if !strings.Contains(err.Error(), "refusing") {
				t.Fatalf("the refusal must be legible, got %v", err)
			}
			assertIntact(t, pool, mode)
		})
	}
}

// 55. Down refuses (legibly) when an alert of the Kind exists and the outbox is
// empty, leaving everything intact.
func TestMigration0114_55_DownRefusesWhenKindAlertsExist(t *testing.T) {
	pool, dir := scratchThrough0114(t, "kyc0114alert")
	f := seedFixture(t, pool)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO alerts (kind, severity, discriminator, subject_tenant_id) VALUES ('kyc.submission_failed_terminal', 'p2', 'create:mock', $1)`, f.tenantID)
		return err
	}); err != nil {
		t.Fatalf("seed alert: %v", err)
	}
	_, err := pool.MigrateDown(context.Background(), dir, 1)
	if err == nil {
		t.Fatal("the down must refuse while an alert of the Kind exists")
	}
	if !strings.Contains(err.Error(), "alerts of kind kyc.submission_failed_terminal exist") {
		t.Fatalf("the refusal must name the remedy, got %v", err)
	}
	assertIntact(t, pool, "alert refusal")
}

// 57. Seed atomicity: the seed pattern with an injected failure after CREATE
// POLICY (and after the INSERT) leaves no policy and no row; the temporary policy
// rejects any other Kind value.
func TestMigration0114_57_SeedAtomicityAndSingleKindPolicy(t *testing.T) {
	pool, _ := scratchThrough0114(t, "kyc0114seed")
	run := func(sql string) error {
		return pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql)
			return err
		})
	}
	// WithoutTenant carries no owner rights for DDL on a policy? The scratch
	// pool IS the owner role, so CREATE POLICY works.
	failAfterInsert := `DO $$ BEGIN
		CREATE POLICY alert_kinds_seed_probe ON alert_kinds FOR INSERT TO CURRENT_USER WITH CHECK (kind = 'kyc.seed_probe');
		INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode)
		VALUES ('kyc.seed_probe', 'p3', 'platform', false, true, true, ARRAY['operation'], 'in_tx');
		RAISE EXCEPTION 'injected failure after the insert';
	END $$;`
	if err := run(failAfterInsert); err == nil || !strings.Contains(err.Error(), "injected failure") {
		t.Fatalf("setup: the injected failure must fire, got %v", err)
	}
	s := readScratchState(t, pool)
	if strings.Join(s.policies, ",") != "alert_kinds_read_all,alerts_platform_service_dispatcher" {
		t.Fatalf("a failed seed block left a policy behind: %v", s.policies)
	}
	var n int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM alert_kinds WHERE kind = 'kyc.seed_probe'`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a failed seed block left a row behind: %d %v", n, err)
	}
	// The temporary policy admits exactly one Kind: any other value is refused.
	otherKind := `DO $$ BEGIN
		CREATE POLICY alert_kinds_seed_probe ON alert_kinds FOR INSERT TO CURRENT_USER WITH CHECK (kind = 'kyc.seed_probe');
		INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode)
		VALUES ('kyc.some_other_kind', 'p3', 'platform', false, true, true, ARRAY['operation'], 'in_tx');
	END $$;`
	err := run(otherKind)
	if err == nil || pgCode(err) != "42501" {
		t.Fatalf("the temporary policy must refuse any other Kind (42501), got %v", err)
	}
	if s := readScratchState(t, pool); strings.Join(s.policies, ",") != "alert_kinds_read_all,alerts_platform_service_dispatcher" {
		t.Fatalf("policies after the refused block: %v", s.policies)
	}
	// And without any policy a plain owner INSERT is refused (FORCE RLS binds the owner).
	err = run(`INSERT INTO alert_kinds (kind, severity, scope, simulation, requires_subject, in_tx_raisable_by_tenant, allowed_keys, raise_mode)
	           VALUES ('kyc.no_policy', 'p3', 'platform', false, true, true, ARRAY['operation'], 'in_tx')`)
	if pgCode(err) != "42501" {
		t.Fatalf("a plain owner INSERT into alert_kinds must be refused under FORCE RLS, got %v", err)
	}
}
