//go:build integration

// ADR 0118 (BRAND-REHOME-HOLD-1), migration 0136 on THROWAWAY scratch databases:
//
//   - the hole and its closure: at 0135 the one-statement re-home CTE commits (owner on the scratch
//     database: the RLS write check is tenant_id only, for every role); after 0136 the same statement
//     is refused with class BR;
//   - T8 catalog: exactly three triggers, enabled ('O'), BEFORE / ROW / UPDATE only, on exactly the
//     three tables, the only BEFORE UPDATE trigger on each; functions not SECURITY DEFINER with a pinned
//     search_path; the WHEN clause (parsed from pg_get_triggerdef) and the function body both list
//     exactly the pinned columns of ADR 0118 s2;
//   - T9 pre-flight: each class 1-3 planted (as the owner, inside a transaction that also runs the 0136
//     up script) refuses BR098 naming that class with count 1; no trigger is created, FORCE ROW LEVEL
//     SECURITY is still on and the planted rows are unchanged; class 4 (non-zero balances on a closed
//     brand) applies the migration and raises the counts-only NOTICE;
//   - T10 up / down / up: the down restores the 0135 schema exactly (whole-schema snapshot, including
//     trigger enablement, SECURITY DEFINER flags and ACLs) and up again equals the first up.
//
// Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package tenant

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/wallet"
)

const migration0136Version = 136

const m136UpFile = "0136_player_brand_binding_immutable.up.sql"

// m136Pinned is ADR 0118 s2's table of pinned columns.
var m136Pinned = map[string]struct {
	trigger, function, code string
	cols                    []string
}{
	"player_accounts":   {"player_accounts_identity_immutable", "player_accounts_enforce_identity_immutable", "BR001", []string{"brand_id", "created_at", "id", "tenant_id"}},
	"wallets":           {"wallets_identity_immutable", "wallets_enforce_identity_immutable", "BR002", []string{"asset_code", "brand_id", "created_at", "id", "player_account_id", "tenant_id"}},
	"kyc_verifications": {"kyc_verifications_identity_immutable", "kyc_verifications_enforce_identity_immutable", "BR003", []string{"brand_id", "player_account_id", "tenant_id"}},
}

const m136Rehome = `WITH pa AS (UPDATE player_accounts SET brand_id = $1 WHERE id = $2 RETURNING id)
	UPDATE wallets SET brand_id = $1 WHERE player_account_id = $2`

// m136Snapshot is the whole-schema snapshot (the 0132 test's richer form: trigger enablement, SECURITY
// DEFINER flag and ACLs included).
func m136Snapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL
			SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || t.tgenabled::text || ':' || pg_get_triggerdef(t.oid) || ':' || coalesce(obj_description(t.oid, 'pg_trigger'), '')
			  FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			 WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'constraint:' || conrelid::regclass::text || ':' || conname || ':' || pg_get_constraintdef(oid)
			  FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'table:' || tablename || ':rls=' || c.relrowsecurity || ':force=' || c.relforcerowsecurity || ':acl=' || coalesce(c.relacl::text, '')
			  FROM pg_tables pt JOIN pg_class c ON c.relname = pt.tablename AND c.relnamespace = 'public'::regnamespace
			 WHERE pt.schemaname = 'public'
			UNION ALL
			SELECT 'column:' || table_name || ':' || column_name || ':' || data_type || ':' || is_nullable || ':' || coalesce(column_default, '')
			  FROM information_schema.columns WHERE table_schema = 'public'
			UNION ALL
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc) || ':' || coalesce(array_to_string(p.proconfig, ','), '') || ':' || p.prosecdef || ':acl=' || coalesce(p.proacl::text, '')
			  FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL
			SELECT 'index:' || tablename || ':' || indexname || ':' || indexdef FROM pg_indexes WHERE schemaname = 'public') s`).Scan(&snap)
	}); err != nil {
		t.Fatalf("schema snapshot: %v", err)
	}
	return snap
}

// m136Fixture is one tenant with two active brands, a player with an EUR wallet funded with ordinary
// cash (manual adjustment: no brand-pinned child row) and a second player of the same brand.
type m136Fixture struct {
	tenant, b1, b2, player, player2, wallet, cash uuid.UUID
}

func m136Seed(t *testing.T, pool *db.Pool, url string) m136Fixture {
	t.Helper()
	ctx := context.Background()
	launchfix.SeedOperatorsAt(t, url)
	var f m136Fixture
	if err := pool.WithPlatformAdmin(ctx, launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "m0136", "m0136-"+uuid.NewString(), "under_platform_licence")
		f.tenant = tn.ID
		return err
	}); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if err := pool.WithTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		b1, err := launchfix.CreateBrand(ctx, tx, f.tenant, "m0136 b1", "m0136-b1-"+uuid.NewString())
		if err != nil {
			return err
		}
		b2, err := launchfix.CreateBrand(ctx, tx, f.tenant, "m0136 b2", "m0136-b2-"+uuid.NewString())
		if err != nil {
			return err
		}
		f.b1, f.b2 = b1.ID, b2.ID
		p, err := identity.RegisterPlayer(ctx, tx, b1, "m0136-"+uuid.NewString()+"@fixture.invalid", "!x")
		if err != nil {
			return err
		}
		p2, err := identity.RegisterPlayer(ctx, tx, b1, "m0136-"+uuid.NewString()+"@fixture.invalid", "!x")
		if err != nil {
			return err
		}
		f.player, f.player2 = p.ID, p2.ID
		wl, err := wallet.GetOrCreate(ctx, tx, f.tenant, f.b1, p.ID, "EUR")
		if err != nil {
			return err
		}
		f.wallet = wl.ID
		f.cash, err = ledger.GetOrCreateAccount(ctx, tx, f.tenant, &f.wallet, ledger.AccountPlayerCash, "EUR")
		if err != nil {
			return err
		}
		adj, err := ledger.GetOrCreateAccount(ctx, tx, f.tenant, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		reason := "m0136 fixture funding"
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenant, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "m0136-fund-" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adj, Direction: ledger.Debit, Amount: 5000},
				{LedgerAccountID: f.cash, Direction: ledger.Credit, Amount: 5000},
			},
		})
		return err
	}); err != nil {
		t.Fatalf("seed brands / players / wallet: %v", err)
	}
	return f
}

func m136Code(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

// m136Objects counts the 0136 triggers and functions, and the three guarded tables plus the five
// pre-flight tables still carrying FORCE ROW LEVEL SECURITY.
func m136Objects(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) (triggers, functions, forced int, err error) {
	err = q.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM pg_trigger WHERE tgname IN ('player_accounts_identity_immutable', 'wallets_identity_immutable', 'kyc_verifications_identity_immutable')),
		(SELECT count(*) FROM pg_proc WHERE proname LIKE '%\_enforce\_identity\_immutable'),
		(SELECT count(*) FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relforcerowsecurity
		    AND relname IN ('player_accounts', 'wallets', 'kyc_verifications', 'ledger_accounts', 'ledger_adjustment_requests', 'ledger_entries', 'brands'))`).
		Scan(&triggers, &functions, &forced)
	return
}

// The hole at 0135 and its closure at 0136: the re-home CTE (both orders) and the three ADR 0118 s1.1
// re-point shapes each SUCCEED at 0135 (every FK, UNIQUE and RLS check satisfied; executed and rolled
// back) and are refused at 0136 with the expected code, on the same fixture.
func TestMigration0136_RehomeCommitsAt0135_RefusedAt0136(t *testing.T) {
	ctx := context.Background()
	pool, url := lgScratch(t, "m0136h_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0136Version-1)); err != nil {
		t.Fatal(err)
	}
	f := m136Seed(t, pool, url)
	kv := uuid.New()
	if err := pool.WithTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			SELECT $1, tenant_id, brand_id, id, person_id, 'pending', 'mock' FROM player_accounts WHERE id = $2`, kv, f.player2)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	shapes := []struct {
		name, sql string
		args      []any
		check     string // a count that is 1 only if the statement took effect
		after     []string
	}{
		{"rehome player first", m136Rehome, []any{f.b2, f.player},
			`SELECT count(*) FROM wallets w JOIN player_accounts p ON p.id = w.player_account_id WHERE w.id = '` + f.wallet.String() + `' AND w.brand_id = '` + f.b2.String() + `' AND p.brand_id = w.brand_id`,
			[]string{"BR001", "BR002"}},
		{"rehome wallet first", `WITH wl AS (UPDATE wallets SET brand_id = $1 WHERE player_account_id = $2 RETURNING id)
			UPDATE player_accounts SET brand_id = $1 WHERE id = $2`, []any{f.b2, f.player},
			`SELECT count(*) FROM wallets w JOIN player_accounts p ON p.id = w.player_account_id WHERE w.id = '` + f.wallet.String() + `' AND w.brand_id = '` + f.b2.String() + `' AND p.brand_id = w.brand_id`,
			[]string{"BR001", "BR002"}},
		{"wallet re-pointed to a same-brand player", `UPDATE wallets SET player_account_id = $1 WHERE id = $2`, []any{f.player2, f.wallet},
			`SELECT count(*) FROM wallets WHERE id = '` + f.wallet.String() + `' AND player_account_id = '` + f.player2.String() + `'`,
			[]string{"BR002"}},
		{"wallet asset changed", `UPDATE wallets SET asset_code = 'USD' WHERE id = $1`, []any{f.wallet},
			`SELECT count(*) FROM wallets WHERE id = '` + f.wallet.String() + `' AND asset_code = 'USD'`,
			[]string{"BR002"}},
		// The verification belongs to player2 (a KYC row on f.player would pin it through its composite FK).
		{"kyc verification re-pointed", `UPDATE kyc_verifications SET player_account_id = $1 WHERE id = $2`, []any{f.player, kv},
			`SELECT count(*) FROM kyc_verifications WHERE id = '` + kv.String() + `' AND player_account_id = '` + f.player.String() + `'`,
			[]string{"BR003"}},
	}
	errRollback := errors.New("rollback by design")
	for _, s := range shapes {
		var took int
		err := pool.WithTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, s.sql, s.args...); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, s.check).Scan(&took); err != nil {
				return err
			}
			return errRollback
		})
		if !errors.Is(err, errRollback) || took != 1 {
			t.Fatalf("at 0135 %q must take effect (the hole ADR 0118 closes): took=%d err=%v", s.name, took, err)
		}
	}
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0136Version)); err != nil {
		t.Fatal(err)
	}
	for _, s := range shapes {
		err := pool.WithTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, s.sql, s.args...)
			return err
		})
		c := m136Code(err)
		ok := false
		for _, want := range s.after {
			ok = ok || c == want
		}
		if !ok {
			t.Fatalf("at 0136 %q must be refused with %v, got %v", s.name, s.after, err)
		}
	}
}

// T8: the catalog.
func TestMigration0136_Catalog(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0136c_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0136Version)); err != nil {
		t.Fatal(err)
	}
	type trg struct {
		table, name, enabled, def, fn, src string
		tgtype                             int16
		secdef                             bool
		config                             []string
	}
	var got []trg
	var beforeUpdateOthers []string
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT c.relname, t.tgname, t.tgenabled::text, pg_get_triggerdef(t.oid), p.proname, p.prosrc, t.tgtype, p.prosecdef, coalesce(p.proconfig, '{}')
			FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid JOIN pg_proc p ON p.oid = t.tgfoid
			WHERE NOT t.tgisinternal AND (t.tgname LIKE '%identity\_immutable' OR p.proname LIKE '%identity\_immutable')
			ORDER BY c.relname`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r trg
			if err := rows.Scan(&r.table, &r.name, &r.enabled, &r.def, &r.fn, &r.src, &r.tgtype, &r.secdef, &r.config); err != nil {
				rows.Close()
				return err
			}
			got = append(got, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		// No other BEFORE UPDATE row trigger on the three tables (ADR 0118 s4 item 4, security F3: BEFORE row
		// triggers fire in name order and a later one could modify NEW after the guard passed, so the guard must
		// fire LAST; a new BEFORE UPDATE trigger needs that order recorded and this pin updated).
		rows, err = tx.Query(ctx, `SELECT c.relname || '.' || t.tgname FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			WHERE NOT t.tgisinternal AND c.relname IN ('player_accounts', 'wallets', 'kyc_verifications')
			  AND (t.tgtype & 2) <> 0 AND (t.tgtype & 16) <> 0 AND t.tgname NOT LIKE '%identity\_immutable'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			beforeUpdateOthers = append(beforeUpdateOthers, s)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("want exactly three identity-immutable triggers, got %d: %+v", len(got), got)
	}
	if len(beforeUpdateOthers) != 0 {
		t.Fatalf("another BEFORE UPDATE trigger exists on a guarded table (the guard must fire LAST: a later BEFORE trigger could modify NEW after it passed; record the order): %v", beforeUpdateOthers)
	}
	whenRe := regexp.MustCompile(`(?i)\bWHEN \((.*)\) EXECUTE FUNCTION`)
	pairRe := regexp.MustCompile(`(?i)\(?new\.(\w+) IS DISTINCT FROM old\.(\w+)\)?`)
	bodyRe := regexp.MustCompile(`NEW\.(\w+) IS DISTINCT FROM OLD\.(\w+)`)
	cols := func(re *regexp.Regexp, s string) ([]string, bool) {
		var out []string
		for _, m := range re.FindAllStringSubmatch(s, -1) {
			if !strings.EqualFold(m[1], m[2]) {
				return nil, false
			}
			out = append(out, strings.ToLower(m[1]))
		}
		sort.Strings(out)
		return out, true
	}
	for _, r := range got {
		want, ok := m136Pinned[r.table]
		if !ok || r.name != want.trigger || r.fn != want.function {
			t.Fatalf("unexpected trigger %s.%s -> %s", r.table, r.name, r.fn)
		}
		// tgtype bits: 1 ROW, 2 BEFORE, 4 INSERT, 8 DELETE, 16 UPDATE, 32 TRUNCATE, 64 INSTEAD.
		if r.tgtype != 1|2|16 {
			t.Fatalf("%s: must be BEFORE UPDATE FOR EACH ROW only, tgtype=%d (%s)", r.name, r.tgtype, r.def)
		}
		if r.enabled != "O" {
			t.Fatalf("%s: must be enabled ('O'), got %q (owner DISABLE TRIGGER is the ADR 0110 s9.4 residual)", r.name, r.enabled)
		}
		if r.secdef {
			t.Fatalf("%s must not be SECURITY DEFINER", r.fn)
		}
		if strings.Join(r.config, ",") != "search_path=pg_catalog, public, pg_temp" {
			t.Fatalf("%s must pin search_path, got %v", r.fn, r.config)
		}
		m := whenRe.FindStringSubmatch(r.def)
		if m == nil {
			t.Fatalf("%s has no WHEN clause: %s", r.name, r.def)
		}
		whenCols, ok := cols(pairRe, m[1])
		if !ok || strings.Join(whenCols, ",") != strings.Join(want.cols, ",") {
			t.Fatalf("%s WHEN clause must list exactly %v (NEW.c IS DISTINCT FROM OLD.c), got %v from %s", r.name, want.cols, whenCols, m[1])
		}
		if strings.Count(m[1], " OR ") != len(want.cols)-1 || strings.Contains(strings.ToUpper(m[1]), " AND ") {
			t.Fatalf("%s WHEN clause must be a pure OR of the pinned columns: %s", r.name, m[1])
		}
		bodyCols, ok := cols(bodyRe, r.src)
		if !ok || strings.Join(bodyCols, ",") != strings.Join(want.cols, ",") {
			t.Fatalf("%s body must check exactly %v, got %v", r.fn, want.cols, bodyCols)
		}
		if !strings.Contains(r.src, "ERRCODE = '"+want.code+"'") || strings.Count(r.src, "ERRCODE") != 1 {
			t.Fatalf("%s must raise exactly %s", r.fn, want.code)
		}
	}
}

// T9: the pre-flight refuses each evidence class 1-3 (BR098, counts only), creates nothing, leaves
// FORCE RLS on and never edits the planted rows; class 4 applies with the NOTICE.
func TestMigration0136_Preflight(t *testing.T) {
	ctx := context.Background()
	pool, url := lgScratch(t, "m0136p_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0136Version-1)); err != nil {
		t.Fatal(err)
	}
	f := m136Seed(t, pool, url)
	upSQL, err := os.ReadFile(filepath.Join("..", "..", "migrations", m136UpFile))
	if err != nil {
		t.Fatal(err)
	}
	classes := []struct {
		class string
		plant func(ctx context.Context, tx pgx.Tx) error
		probe string // re-read after the refusal: must equal the value read right after the plant
	}{
		{"wallet_repointed", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE wallets SET player_account_id = $1 WHERE id = $2`, f.player2, f.wallet)
			return err
		}, `SELECT player_account_id::text FROM wallets WHERE id = '` + f.wallet.String() + `'`},
		{"wallet_asset_changed", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE wallets SET asset_code = 'USD' WHERE id = $1`, f.wallet)
			return err
		}, `SELECT asset_code FROM wallets WHERE id = '` + f.wallet.String() + `'`},
		// A request filed while the wallet was in b1 (insert-time snapshot b1; the owner plants the row with
		// the request's own guards and FORCE RLS lifted for that one insert, both restored in the same
		// transaction), then the 0135 re-home CTE moves the player and the wallet to b2: the snapshot is
		// now stale.
		{"adjustment_snapshot_stale", func(ctx context.Context, tx pgx.Tx) error {
			for _, q := range []string{
				`ALTER TABLE ledger_adjustment_requests DISABLE TRIGGER USER`,
				`ALTER TABLE ledger_adjustment_requests NO FORCE ROW LEVEL SECURITY`,
				`INSERT INTO ledger_adjustment_requests (tenant_id, wallet_id, player_account_id, brand_id, asset_code, direction, amount,
					reason_code, note_hash, payload_hash, initiated_by, initiated_by_scope, initiated_by_person_id,
					tenant_status_at_submission, required_at_submission, contributing_policy_ids, expires_at)
				 VALUES ('` + f.tenant.String() + `', '` + f.wallet.String() + `', '` + f.player.String() + `', '` + f.b1.String() + `', 'EUR', 'credit_player', 10,
					'goodwill_credit', repeat('a', 64), repeat('b', 64), gen_random_uuid(), 'tenant', gen_random_uuid(),
					'active', 2, '{}', now() + interval '1 day')`,
				`ALTER TABLE ledger_adjustment_requests FORCE ROW LEVEL SECURITY`,
				`ALTER TABLE ledger_adjustment_requests ENABLE TRIGGER USER`,
			} {
				if _, err := tx.Exec(ctx, q); err != nil {
					return err
				}
			}
			_, err := tx.Exec(ctx, m136Rehome, f.b2, f.player)
			return err
		}, `SELECT brand_id::text FROM wallets WHERE id = '` + f.wallet.String() + `'`},
	}
	all := []string{"wallet_repointed", "wallet_asset_changed", "adjustment_snapshot_stale"}
	for _, c := range classes {
		func() {
			tx, err := pool.Raw().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }() // nothing planted here ever commits
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant.String()); err != nil {
				t.Fatal(err)
			}
			if err := c.plant(ctx, tx); err != nil {
				t.Fatalf("%s: plant: %v", c.class, err)
			}
			var planted string
			if err := tx.QueryRow(ctx, c.probe).Scan(&planted); err != nil {
				t.Fatal(err)
			}
			// The migration session carries no tenant: FORCE RLS would hide every row from a plain count.
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
				t.Fatal(err)
			}
			// Flush the plant's deferred checks: ALTER TABLE (the pre-flight's NO FORCE) is refused on a
			// table with pending trigger events (a real migration transaction has none).
			if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
				t.Fatalf("%s: flush: %v", c.class, err)
			}
			if _, err := tx.Exec(ctx, `SAVEPOINT up`); err != nil {
				t.Fatal(err)
			}
			_, err = tx.Exec(ctx, string(upSQL))
			var pe *pgconn.PgError
			if !errors.As(err, &pe) || pe.Code != "BR098" {
				t.Fatalf("%s: want BR098, got %v", c.class, err)
			}
			for _, k := range all {
				want := k + "=0"
				if k == c.class {
					want = k + "=1"
				}
				if !strings.Contains(pe.Message, want) {
					t.Fatalf("%s: the refusal must carry %q: %q", c.class, want, pe.Message)
				}
			}
			for _, id := range []uuid.UUID{f.tenant, f.b1, f.b2, f.player, f.player2, f.wallet} {
				if strings.Contains(pe.Message, id.String()) {
					t.Fatalf("%s: the refusal must carry counts only, not ids: %q", c.class, pe.Message)
				}
			}
			if _, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT up`); err != nil {
				t.Fatal(err)
			}
			trg, fns, forced, err := m136Objects(ctx, tx)
			if err != nil || trg != 0 || fns != 0 || forced != 7 {
				t.Fatalf("%s: a refused up creates nothing and leaves FORCE RLS on: triggers=%d functions=%d forced=%d/7 err=%v", c.class, trg, fns, forced, err)
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenant.String()); err != nil {
				t.Fatal(err)
			}
			var after string
			if err := tx.QueryRow(ctx, c.probe).Scan(&after); err != nil || after != planted {
				t.Fatalf("%s: the pre-flight must never repair the planted row: %q -> %q err=%v", c.class, planted, after, err)
			}
		}()
	}

	// Class 4 (informational): b2 holds a second funded player and a stranded hold, then is closed WITHOUT
	// the governed sequence (launchfix.ForceClosed: the pre-0130 closed-with-funds population). The up
	// applies and raises the counts-only NOTICE.
	var w2 uuid.UUID
	if err := pool.WithTenant(ctx, f.tenant, func(ctx context.Context, tx pgx.Tx) error {
		br, err := identity.GetBrandByID(ctx, tx, f.b2)
		if err != nil {
			return err
		}
		p, err := identity.RegisterPlayer(ctx, tx, br, "m0136-c4-"+uuid.NewString()+"@fixture.invalid", "!x")
		if err != nil {
			return err
		}
		wl, err := wallet.GetOrCreate(ctx, tx, f.tenant, f.b2, p.ID, "EUR")
		if err != nil {
			return err
		}
		w2 = wl.ID
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, f.tenant,
			ledger.AccountSpec{WalletID: &w2, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{WalletID: &w2, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		reason := "m0136 class 4"
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenant, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "m0136-c4-" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[2], Direction: ledger.Debit, Amount: 900},
				{LedgerAccountID: accounts[0], Direction: ledger.Credit, Amount: 900},
			},
		}); err != nil {
			return err
		}
		// A stranded hold (no request names it): owner tamper plant with the two 0132 hold triggers off.
		for _, q := range []string{
			`SET CONSTRAINTS ALL IMMEDIATE`, // flush the funding posting's deferred checks before ALTER TABLE
			`SET CONSTRAINTS ALL DEFERRED`,
			`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
			`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenant, TransactionType: ledger.TxWithdrawalRequested,
			IdempotencyKey: "m0136-c4-hold-" + uuid.NewString(), CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[0], Direction: ledger.Debit, Amount: 300},
				{LedgerAccountID: accounts[1], Direction: ledger.Credit, Amount: 300},
			},
		}); err != nil {
			return err
		}
		for _, q := range []string{
			`SET CONSTRAINTS ledger_entries_balanced IMMEDIATE`,
			`SET CONSTRAINTS ledger_entries_balanced DEFERRED`,
			`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
			`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("class 4 fixture: %v", err)
	}
	if err := launchfix.ForceClosed(ctx, t, pool, f.tenant, &f.b2); err != nil {
		t.Fatalf("force-close b2: %v", err)
	}

	// Applied through the REAL migrate runner (db.MigrateUp on a pool with the notice handler that
	// cmd/migrate installs, security review F2): the class 4 NOTICE must reach the operator.
	var mu sync.Mutex
	var notices []string
	runner, err := db.ConnectWithNoticeHandler(ctx, url, 2, 5*time.Second, func(sev, msg string) {
		mu.Lock()
		notices = append(notices, sev+": "+msg)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	if applied, err := runner.MigrateUp(ctx, lgMigDir(t, migration0136Version)); err != nil || len(applied) != 1 || applied[0] != migration0136Version {
		t.Fatalf("class 4 must not refuse (runner): %v %v", applied, err)
	}
	trg, fns, forced, err := m136Objects(ctx, pool.Raw())
	if err != nil || trg != 3 || fns != 3 || forced != 7 {
		t.Fatalf("class 4: the up applies and restores FORCE: triggers=%d functions=%d forced=%d/7 err=%v", trg, fns, forced, err)
	}
	mu.Lock()
	defer mu.Unlock()
	var found string
	for _, n := range notices {
		if strings.HasPrefix(n, "NOTICE: migration 0136 pre-flight (informational") {
			found = n
		}
	}
	if !strings.Contains(found, "player_withdrawal_hold=1 player_cash=1") {
		t.Fatalf("class 4 NOTICE must carry the counts (one hold, one cash account): %q (all: %v)", found, notices)
	}
	for _, id := range []uuid.UUID{f.tenant, f.b2, w2} {
		if strings.Contains(found, id.String()) {
			t.Fatalf("class 4 NOTICE must carry counts only: %q", found)
		}
	}
}

// T10: up / down / up, whole-schema snapshot.
func TestMigration0136_UpDownUp_WholeSchema(t *testing.T) {
	ctx := context.Background()
	pool, _ := lgScratch(t, "m0136s_")
	if _, err := pool.MigrateUp(ctx, lgMigDir(t, migration0136Version-1)); err != nil {
		t.Fatal(err)
	}
	snap135 := m136Snapshot(t, pool)
	full := lgMigDir(t, migration0136Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	snap136 := m136Snapshot(t, pool)
	if snap135 == snap136 {
		t.Fatal("0136 changed nothing in the schema snapshot (vacuous)")
	}
	added := lgSnapDiff(snap135, snap136)
	for _, must := range []string{"player_accounts_identity_immutable", "wallets_identity_immutable", "kyc_verifications_identity_immutable",
		"player_accounts_enforce_identity_immutable()", "wallets_enforce_identity_immutable()", "kyc_verifications_enforce_identity_immutable()"} {
		if !strings.Contains(added, must) {
			t.Fatalf("0136 snapshot diff lacks %q:\n%s", must, added)
		}
	}
	for _, l := range strings.Split(added, "\n") {
		if strings.HasPrefix(l, "- ") {
			t.Fatalf("0136 must not change or remove an existing object: %s", l)
		}
	}
	if rolled, err := pool.MigrateDown(ctx, full, 1); err != nil || len(rolled) != 1 || rolled[0] != migration0136Version {
		t.Fatalf("down: %v %v", rolled, err)
	}
	if got := m136Snapshot(t, pool); got != snap135 {
		t.Fatalf("down did not restore the 0135 schema exactly:\n%s", lgSnapDiff(snap135, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := m136Snapshot(t, pool); got != snap136 {
		t.Fatalf("up after down differs from the first up:\n%s", lgSnapDiff(snap136, got))
	}
}
