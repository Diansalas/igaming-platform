//go:build integration

package payoutinstrument

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

const migration0123Version = 123

// migDir copies every migration file with version <= through into a temp dir.
func migDir(t *testing.T, through int64) string {
	t.Helper()
	src, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") || len(name) < 4 {
			continue
		}
		v, err := strconv.Atoi(name[:4])
		if err != nil {
			t.Fatalf("migration %q has no numeric prefix", name)
		}
		if int64(v) > through {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// scratchThrough migrates a fresh scratch database through `through` (the
// last on-disk version <= through).
func scratchThrough(t *testing.T, prefix string, through int64) *db.Pool {
	t.Helper()
	pool, err := db.Connect(context.Background(), scratchdb.New(t, prefix), 10, 5_000_000_000)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), migDir(t, through)); err != nil {
		t.Fatalf("migrate through %d: %v", through, err)
	}
	return pool
}

// schemaSnapshot renders every policy, trigger, constraint, table, function
// (body hash), index and runtime-role grant of the public schema as sorted
// text - the whole-schema snapshot of the repo's migration convention.
func schemaSnapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL
			SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || pg_get_triggerdef(t.oid)
			  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'constraint:' || conrelid::regclass::text || ':' || conname || ':' || pg_get_constraintdef(oid)
			  FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'table:' || tablename || ':rls=' || c.relrowsecurity || ':force=' || c.relforcerowsecurity
			  FROM pg_tables pt JOIN pg_class c ON c.relname = pt.tablename AND c.relnamespace = 'public'::regnamespace
			 WHERE pt.schemaname = 'public'
			UNION ALL
			SELECT 'column:' || table_name || ':' || column_name || ':' || data_type || ':' || is_nullable || ':' || coalesce(column_default, '')
			  FROM information_schema.columns WHERE table_schema = 'public'
			UNION ALL
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc) || ':' || coalesce(array_to_string(p.proconfig, ','), '')
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public'
			UNION ALL
			SELECT 'grant:' || table_name || ':' || grantee || ':' || privilege_type FROM information_schema.role_table_grants
			 WHERE table_schema = 'public' AND grantee = 'igaming_runtime') s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

func snapDiff(a, b string) string {
	as, bs := map[string]bool{}, map[string]bool{}
	for _, l := range strings.Split(a, "\n") {
		as[l] = true
	}
	for _, l := range strings.Split(b, "\n") {
		bs[l] = true
	}
	var out []string
	for l := range as {
		if !bs[l] {
			out = append(out, "- "+l)
		}
	}
	for l := range bs {
		if !as[l] {
			out = append(out, "+ "+l)
		}
	}
	sort.Strings(out)
	if len(out) > 20 {
		out = append(out[:20], fmt.Sprintf("... (%d differences)", len(out)))
	}
	return strings.Join(out, "\n")
}

func names(snap, kind string) map[string]bool {
	out := map[string]bool{}
	for _, l := range strings.Split(snap, "\n") {
		if strings.HasPrefix(l, kind+":") {
			parts := strings.SplitN(l, ":", 3)
			out[parts[1]+":"+strings.SplitN(parts[2], ":", 2)[0]] = true
		}
	}
	return out
}

// Migration 0123 up / down / up, whole schema, on a scratch database.
func TestMigration0123_UpDownUp_WholeSchema(t *testing.T) {
	pool := scratchThrough(t, "b13mig_", migration0123Version-1)
	pre := schemaSnapshot(t, pool)
	dir := migDir(t, migration0123Version)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	up := schemaSnapshot(t, pool)
	if up == pre {
		t.Fatal("0123 changed nothing?")
	}

	// The exact object additions.
	added := func(kind string) []string {
		var out []string
		a, b := names(pre, kind), names(up, kind)
		for n := range b {
			if !a[n] {
				out = append(out, n)
			}
		}
		sort.Strings(out)
		return out
	}
	wantTables := []string{"payout_attempt_destination_snapshots", "payout_instrument_blocking_events", "payout_instrument_fingerprint_owners",
		"payout_instrument_kinds", "payout_instrument_verification_max_age", "payout_instrument_verifications", "payout_instruments"}
	var gotTables []string
	for _, n := range added("table") {
		gotTables = append(gotTables, strings.SplitN(n, ":", 2)[0])
	}
	if strings.Join(gotTables, ",") != strings.Join(wantTables, ",") {
		t.Fatalf("new tables = %v, want %v", gotTables, wantTables)
	}
	var gotFns []string
	for _, l := range strings.Split(up, "\n") {
		if strings.HasPrefix(l, "function:") && !strings.Contains(pre, l) {
			gotFns = append(gotFns, strings.SplitN(strings.TrimPrefix(l, "function:"), ":", 2)[0])
		}
	}
	sort.Strings(gotFns)
	wantFns := []string{
		"payment_attempts_require_destination_snapshot()", "payout_attempt_destination_snapshots_before_insert()",
		"payout_instrument_blocking_events_before_insert()", "payout_instrument_verifications_before_insert()",
		"payout_instruments_before_insert()", "payout_instruments_before_update()",
		"withdrawal_requests_enforce_immutable_fields()", "withdrawal_requests_payout_binding_guard()",
	}
	if strings.Join(gotFns, ",") != strings.Join(wantFns, ",") {
		t.Fatalf("new/changed functions = %v, want %v", gotFns, wantFns)
	}
	// Nothing pre-existing was removed except the one replaced function body.
	for _, l := range strings.Split(pre, "\n") {
		if !strings.Contains(up, l) && !strings.HasPrefix(l, "function:withdrawal_requests_enforce_immutable_fields()") {
			t.Errorf("0123 removed or changed a pre-existing object: %s", l)
		}
	}
	for _, want := range []string{"trigger:withdrawal_requests:withdrawal_requests_payout_binding_guard", "trigger:payment_attempts:payment_attempts_require_destination_snapshot",
		"column:withdrawal_requests:payout_instrument_id:uuid:YES", "column:withdrawal_requests:payout_instrument_fingerprint:text:YES"} {
		if !strings.Contains(up, want) {
			t.Errorf("missing %s", want)
		}
	}

	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down on an empty 0123: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != pre {
		t.Fatalf("0123 down did not restore the N-1 schema exactly:\n%s", snapDiff(pre, got))
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := schemaSnapshot(t, pool); got != up {
		t.Fatalf("0123 re-up differs from the first up:\n%s", snapDiff(up, got))
	}
}

// The down refuses (PI099) while an instrument row exists, and the refusal
// rolls back whole.
func TestMigration0123_DownRefusesWithEvidence(t *testing.T) {
	pool := scratchThrough(t, "b13down_", migration0123Version)
	dir := migDir(t, migration0123Version)
	tenantID, brandID, playerID, personID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1,$2,'t','under_platform_licence')`, tenantID, "d-"+tenantID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	keys := newTestKeys(t)
	svc, _ := NewService(keys, DefaultKinds(), NewMockVerifier())
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1,$2,'b','b')`, brandID, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,'d@example.test','x','active')`,
			playerID, tenantID, brandID, personID); err != nil {
			return err
		}
		_, err := svc.Register(ctx, tx, RegisterParams{TenantID: tenantID, PlayerAccountID: playerID, Kind: KindBankAccount, Rail: "sepa", AssetCodes: []string{"EUR"}, Detail: ibanDetail(ibanA)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(context.Background(), dir, 1)
	requireCode(t, unwrapPg(err), "PI099", "down with an instrument row")
	var n int
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payout_instruments`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("a refused down must leave the table and row: n=%d err=%v", n, err)
	}
}

func unwrapPg(err error) error { return err }

// Up-time assertion (ADR 0111 L-6 / A-11): a non-terminal pre-0123 withdrawal
// whose provider_id is outside the MOCK set stops the migration, changing
// nothing; MOCK, terminal and NULL-provider rows pass.
func TestMigration0123_UpRefusesLegacyNonMockWithdrawal(t *testing.T) {
	cases := []struct {
		name, provider, state string
		wantRefuse            bool
	}{
		{"non-mock approved", "real-psp", "approved", true},
		{"non-mock submitted", "real-psp", "submitted", true},
		{"non-mock requested", "real-psp", "requested", true},
		{"non-mock completed (terminal)", "real-psp", "completed", false},
		{"mock approved", "mock-payments", "approved", false},
		{"no provider yet", "", "approved", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pool := scratchThrough(t, "b13l6_", migration0123Version-1)
			tenantID, brandID, playerID, personID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1,$2,'t','under_platform_licence')`, tenantID, "l-"+tenantID.String()[:8]); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1,$2,'b','b')`, brandID, tenantID); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,'l@example.test','x','active')`,
					playerID, tenantID, brandID, personID); err != nil {
					return err
				}
				wl, err := wallet.GetOrCreate(ctx, tx, tenantID, brandID, playerID, "EUR")
				if err != nil {
					return err
				}
				var prov any
				if c.provider != "" {
					prov = c.provider
				}
				_, err = tx.Exec(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, state, provider_id)
					VALUES ($1,$2,$3,$4,'EUR',100,'legacy',$5,$6)`, tenantID, brandID, playerID, wl.ID, c.state, prov)
				return err
			}); err != nil {
				t.Fatalf("seed legacy withdrawal: %v", err)
			}
			_, err := pool.MigrateUp(context.Background(), migDir(t, migration0123Version))
			var exists bool
			if qerr := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT to_regclass('public.payout_instruments') IS NOT NULL`).Scan(&exists)
			}); qerr != nil {
				t.Fatal(qerr)
			}
			if c.wantRefuse {
				if err == nil || !strings.Contains(err.Error(), "PI098") {
					t.Fatalf("expected the PI098 pre-flight refusal, got %v", err)
				}
				if exists {
					t.Fatal("a refused 0123 must change nothing")
				}
				return
			}
			if err != nil || !exists {
				t.Fatalf("0123 must apply (err=%v exists=%v)", err, exists)
			}
		})
	}
}

// F-7: the down also refuses while max-age configuration rows exist.
func TestMigration0123_DownRefusesWithMaxAgeConfig(t *testing.T) {
	pool := scratchThrough(t, "b13age_", migration0123Version)
	dir := migDir(t, migration0123Version)
	jid := uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1,$2,'j')`, jid, "J-"+jid.String()[:8])
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO payout_instrument_verification_max_age (jurisdiction_id, max_age) VALUES ($1,'7 days')`, jid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, err := pool.MigrateDown(context.Background(), dir, 1)
	if err == nil || !strings.Contains(err.Error(), "PI099") || !strings.Contains(err.Error(), "max_age") {
		t.Fatalf("down with max-age config must refuse (PI099): %v", err)
	}
	var n int
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payout_instrument_verification_max_age`).Scan(&n)
	}); err != nil || n != 1 {
		t.Fatalf("refused down must leave the row: n=%d err=%v", n, err)
	}
}

// C-4: the pre-flight scan is serialised with writers. A withdrawal that gains a
// non-MOCK provider_id in a still-open transaction makes the migration WAIT for
// that transaction (SHARE ROW EXCLUSIVE lock) and then refuse; without the lock
// the scan would not see the uncommitted row and 0123 would apply.
func TestMigration0123_PreflightLockSerialisesWithWriters(t *testing.T) {
	pool := scratchThrough(t, "b13lock_", migration0123Version-1)
	tenantID, brandID, playerID, personID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO tenants (id, slug, name, licensing_model) VALUES ($1,$2,'t','under_platform_licence')`, tenantID, "k-"+tenantID.String()[:8]); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, personID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var wid uuid.UUID
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1,$2,'b','b')`, brandID, tenantID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO player_accounts (id, tenant_id, brand_id, person_id, email, password_hash, status) VALUES ($1,$2,$3,$4,'k@example.test','x','active')`, playerID, tenantID, brandID, personID); err != nil {
			return err
		}
		wl, err := wallet.GetOrCreate(ctx, tx, tenantID, brandID, playerID, "EUR")
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, state)
			VALUES ($1,$2,$3,$4,'EUR',100,'lock','approved') RETURNING id`, tenantID, brandID, playerID, wl.ID).Scan(&wid)
	}); err != nil {
		t.Fatal(err)
	}
	// An open writer that sets a non-MOCK provider id, not yet committed.
	ctx := context.Background()
	txA, err := pool.Raw().Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txA.Rollback(ctx) }()
	if _, err := txA.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		t.Fatal(err)
	}
	if _, err := txA.Exec(ctx, `UPDATE withdrawal_requests SET provider_id = 'real-psp' WHERE id = $1`, wid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := pool.MigrateUp(ctx, migDir(t, migration0123Version))
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("the migration must wait for the open writer (lock), returned %v", err)
	case <-time.After(1500 * time.Millisecond):
	}
	if err := txA.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "PI098") {
		t.Fatalf("after the writer committed the non-MOCK provider id, 0123 must refuse (PI098): %v", err)
	}
}
