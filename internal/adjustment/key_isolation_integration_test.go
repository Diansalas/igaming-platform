//go:build integration

// F1 of the platform-freeze work list (SRBX-3 / residual R1; owner decision 5; migration 0166): the actor-proof HMAC secrets live in the OWNER-ONLY
// schema actorproof_private, reachable only through the SECURITY DEFINER verifier actor_proof_require and the existence probe actor_proof_key_active.
//
//   - catalog: where the table is, who owns what, which ACLs exist, which functions reach into the schema and how they are pinned;
//   - the runtime role cannot read, write, lock, alter, create in or grant on the schema (SQLSTATE 42501 for every probe) and has no privilege by OID;
//   - every proof rule AP001..AP005 still holds against the moved key, at the SQL verifier, as igaming_runtime;
//   - the production startup gate admits the provisioned kid and refuses an unprovisioned or retired one;
//   - migration 0166 on a scratch database with key rows: up (rows intact, moved), down (the 0165 catalog byte for byte, rows intact), up again
//     (the first up byte for byte), and every up/down refusal (APM01 / APM02) leaves the catalog unchanged;
//   - deploy/backup/actorproof-private-schema.sql (the restore DDL) produces the same catalog as the migrations.
//
// Requires TEST_DATABASE_URL, TEST_RUNTIME_DATABASE_URL and TEST_ADMIN_DATABASE_URL (scratch databases); a skip is NOT evidence.
// It lives beside the K2 actor-proof tests (internal/adjustment), not in internal/actorproof: importing prooftest switches on the
// integration step-up fallback at init, which the actorproof package's own unit tests must not see.
package adjustment_test

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

var errRollback = errors.New("rollback")

func kiOwnerURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	return u
}

func kiConnect(t *testing.T, u, wantRole string) *db.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), u, 4, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	var role string
	if err := p.Raw().QueryRow(context.Background(), `SELECT current_user`).Scan(&role); err != nil || role != wantRole {
		t.Fatalf("pool connects as %q (%v), want %s", role, err, wantRole)
	}
	return p
}

// kiRuntimeURL is TEST_RUNTIME_DATABASE_URL pointed at dbName ("" keeps the shared test database).
func kiRuntimeURL(t *testing.T, dbName string) string {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Fatal("TEST_RUNTIME_DATABASE_URL is required: the key-isolation probes run as igaming_runtime")
	}
	if dbName == "" {
		return u
	}
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	pu.Path = "/" + dbName
	return pu.String()
}

func kiDBName(t *testing.T, u string) string {
	t.Helper()
	pu, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimPrefix(pu.Path, "/")
}

func sqlstate(err error) string {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return pg.Code
	}
	return ""
}

// ---- catalog --------------------------------------------------------------------------------------------------------

func TestKeyIsolation_Catalog(t *testing.T) {
	ctx := context.Background()
	owner := kiConnect(t, kiOwnerURL(t), "igaming")
	q := func(sql string, args ...any) string {
		t.Helper()
		var s string
		if err := owner.Raw().QueryRow(ctx, sql, args...).Scan(&s); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
		return s
	}
	if got := q(`SELECT (to_regclass('public.actor_proof_keys') IS NULL)::text || '/' || (to_regclass('actorproof_private.actor_proof_keys') IS NOT NULL)::text`); got != "true/true" {
		t.Fatalf("the key table must live only in actorproof_private (public absent / private present): %s", got)
	}
	// one owner for the schema, the table and the verifier; no grant to anyone else on the schema, the table or a column; RLS enabled, no policy
	if got := q(`SELECT pg_get_userbyid(n.nspowner) || '/' || pg_get_userbyid(c.relowner) || '/' || pg_get_userbyid(p.proowner)
		FROM pg_namespace n JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = 'actor_proof_keys', pg_proc p
		WHERE n.nspname = 'actorproof_private' AND p.oid = 'public.actor_proof_require(uuid, text, uuid, text, text, text)'::regprocedure`); got != "igaming/igaming/igaming" {
		t.Fatalf("owners schema/table/verifier = %s, want the migration owner for all three", got)
	}
	if got := q(`SELECT (
		  (SELECT count(*) FROM pg_namespace n, LATERAL aclexplode(COALESCE(n.nspacl, acldefault('n', n.nspowner))) a WHERE n.nspname = 'actorproof_private' AND a.grantee <> n.nspowner)
		+ (SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private',
		          LATERAL aclexplode(COALESCE(c.relacl, acldefault('r', c.relowner))) a WHERE a.grantee <> c.relowner)
		+ (SELECT count(*) FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private' WHERE a.attacl IS NOT NULL)
		+ (SELECT count(*) FROM pg_policy po JOIN pg_class c ON c.oid = po.polrelid JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = 'actorproof_private'))::text`); got != "0" {
		t.Fatalf("non-owner grants / column grants / policies on actorproof_private: %s, want 0", got)
	}
	if got := q(`SELECT c.relrowsecurity::text || '/' || c.relforcerowsecurity::text FROM pg_class c WHERE c.oid = 'actorproof_private.actor_proof_keys'::regclass`); got != "true/false" {
		t.Fatalf("key table RLS enabled/forced = %s, want true/false (ADR 0110: the definer verifier runs as the owner)", got)
	}
	// the schema holds exactly the key table and its primary-key index; nothing else (no function, view, sequence, type of its own)
	if got := q(`SELECT string_agg(c.relname || ':' || c.relkind::text, ',' ORDER BY c.relname) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'actorproof_private'`); got != "actor_proof_keys:r,actor_proof_keys_pkey:i" {
		t.Fatalf("actorproof_private relations = %s", got)
	}
	if got := q(`SELECT ((SELECT count(*) FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname = 'actorproof_private')
		+ (SELECT count(*) FROM pg_type t JOIN pg_namespace n ON n.oid = t.typnamespace WHERE n.nspname = 'actorproof_private' AND t.typname NOT IN ('actor_proof_keys', '_actor_proof_keys')))::text`); got != "0" {
		t.Fatalf("functions/types in actorproof_private: %s, want 0", got)
	}
	// no default privilege can re-grant a future relation of the schema (schema-scoped or global entries)
	if got := q(`SELECT count(*)::text FROM pg_default_acl d WHERE d.defaclnamespace IN (0, (SELECT oid FROM pg_namespace WHERE nspname = 'actorproof_private'))
		AND EXISTS (SELECT 1 FROM aclexplode(d.defaclacl) a WHERE a.grantee <> d.defaclrole)`); got != "0" {
		t.Fatalf("default privileges that would re-grant objects of actorproof_private: %s", got)
	}
	// no view / rule / trigger / policy depends on the key table (no second path to the secret)
	if got := q(`SELECT count(*)::text FROM pg_depend d WHERE d.refclassid = 'pg_class'::regclass AND d.refobjid = 'actorproof_private.actor_proof_keys'::regclass
		AND d.classid IN ('pg_rewrite'::regclass, 'pg_trigger'::regclass, 'pg_policy'::regclass)`); got != "0" {
		t.Fatalf("objects depending on the key table: %s", got)
	}
	// exactly two functions reach into the schema: the verifier and the existence probe; both SECURITY DEFINER with the pinned search_path (the private
	// schema is NOT on it), returning void / boolean (they mint and disclose nothing); EXECUTE for the owner and igaming_runtime only
	if got := q(`SELECT string_agg(p.proname || ':' || p.prosecdef || ':' || array_to_string(p.proconfig, ';') || ':' || p.prorettype::regtype::text || ':'
		|| (SELECT string_agg(CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END, '+' ORDER BY CASE a.grantee WHEN 0 THEN 'PUBLIC' ELSE pg_get_userbyid(a.grantee) END)
		      FROM aclexplode(COALESCE(p.proacl, acldefault('f', p.proowner))) a), ',' ORDER BY p.proname)
		FROM pg_proc p WHERE p.prosrc LIKE '%actorproof_private%'`); got != "actor_proof_key_active:true:search_path=pg_catalog, public, pg_temp:boolean:igaming+igaming_runtime,actor_proof_require:true:search_path=pg_catalog, public, pg_temp:void:igaming+igaming_runtime" {
		t.Fatalf("functions reaching into actorproof_private:\n%s", got)
	}
	// ... and nothing still reads the old location
	if got := q(`SELECT count(*)::text FROM pg_proc p WHERE p.prosrc LIKE '%public.actor_proof_keys%' OR p.prosrc ~ '[^.]\mactor_proof_keys\M'`); got != "0" {
		t.Fatalf("functions still naming public.actor_proof_keys (or an unqualified actor_proof_keys): %s", got)
	}
	// the backup role (when it exists) holds nothing on the schema / table and may not execute the reaching functions
	if got := q(`SELECT COALESCE((SELECT (has_schema_privilege(r.oid, n.oid, 'USAGE') OR has_schema_privilege(r.oid, n.oid, 'CREATE')
		OR has_table_privilege(r.oid, c.oid, 'SELECT') OR has_any_column_privilege(r.oid, c.oid, 'SELECT')
		OR has_function_privilege(r.oid, 'public.actor_proof_key_active(text)'::regprocedure, 'EXECUTE'))::text
		FROM pg_roles r, pg_namespace n JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = 'actor_proof_keys'
		WHERE r.rolname = 'igaming_backup' AND n.nspname = 'actorproof_private'), 'no-role')`); got != "false" && got != "no-role" {
		t.Fatalf("igaming_backup privileges on the key schema/table/probe: %s", got)
	}
}

// ---- the runtime role ----------------------------------------------------------------------------------------------

func TestKeyIsolation_RuntimeRoleDenied(t *testing.T) {
	ctx := context.Background()
	owner := kiOwnerURL(t)
	prooftest.Install(t, owner) // a key row exists, so a "denied" can never be an empty result
	rt := kiConnect(t, kiRuntimeURL(t, ""), "igaming_runtime")
	probes := []string{
		`SELECT secret FROM actorproof_private.actor_proof_keys`,
		`SELECT kid FROM actorproof_private.actor_proof_keys`,
		`SELECT count(*) FROM actorproof_private.actor_proof_keys`,
		`INSERT INTO actorproof_private.actor_proof_keys (kid, secret) VALUES ('rt-attacker', decode(repeat('ab', 32), 'hex'))`,
		`UPDATE actorproof_private.actor_proof_keys SET status = 'retired', retired_at = now()`,
		`DELETE FROM actorproof_private.actor_proof_keys`,
		`TRUNCATE actorproof_private.actor_proof_keys`,
		`LOCK TABLE actorproof_private.actor_proof_keys IN ACCESS SHARE MODE`,
		`COPY actorproof_private.actor_proof_keys TO STDOUT`,
		`CREATE TABLE actorproof_private.zz_rt (a int)`,
		`CREATE FUNCTION actorproof_private.zz_rt() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		`CREATE VIEW zz_rt_keys AS SELECT secret FROM actorproof_private.actor_proof_keys`,
		`ALTER TABLE actorproof_private.actor_proof_keys OWNER TO igaming_runtime`,
		`ALTER TABLE actorproof_private.actor_proof_keys DISABLE ROW LEVEL SECURITY`,
		`DROP TABLE actorproof_private.actor_proof_keys`,
		`ALTER SCHEMA actorproof_private RENAME TO zz_rt`,
		`DROP SCHEMA actorproof_private CASCADE`,
		`COMMENT ON TABLE actorproof_private.actor_proof_keys IS 'x'`,
		`GRANT USAGE ON SCHEMA actorproof_private TO igaming_runtime`,
		`GRANT SELECT ON actorproof_private.actor_proof_keys TO igaming_runtime`,
	}
	for _, sql := range probes {
		err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, sql)
			return e
		})
		if err == nil {
			t.Errorf("the runtime role was ALLOWED: %s", sql)
		} else if c := sqlstate(err); c != "42501" {
			t.Errorf("%s: want SQLSTATE 42501 (privilege denied), got %q: %v", sql, c, err)
		}
	}
	// the search_path detour: a schema without USAGE is skipped, so the unqualified name does not even resolve
	err := rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `SET LOCAL search_path = actorproof_private, public`); e != nil {
			return e
		}
		var b []byte
		return tx.QueryRow(ctx, `SELECT secret FROM actor_proof_keys LIMIT 1`).Scan(&b)
	})
	if c := sqlstate(err); c != "42P01" && c != "42501" {
		t.Errorf("search_path detour: want 42P01/42501, got %q (%v)", c, err)
	}
	// privileges by OID (a name lookup would itself be refused): none
	var held bool
	if err := rt.Raw().QueryRow(ctx, `SELECT has_schema_privilege(n.oid, 'USAGE') OR has_schema_privilege(n.oid, 'CREATE')
		OR has_table_privilege(c.oid, 'SELECT') OR has_table_privilege(c.oid, 'INSERT') OR has_table_privilege(c.oid, 'UPDATE')
		OR has_table_privilege(c.oid, 'DELETE') OR has_table_privilege(c.oid, 'TRUNCATE') OR has_table_privilege(c.oid, 'REFERENCES')
		OR has_table_privilege(c.oid, 'TRIGGER') OR has_any_column_privilege(c.oid, 'SELECT,INSERT,UPDATE,REFERENCES')
		FROM pg_namespace n JOIN pg_class c ON c.relnamespace = n.oid AND c.relname = 'actor_proof_keys' WHERE n.nspname = 'actorproof_private'`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if held {
		t.Fatal("igaming_runtime holds a privilege on actorproof_private or its key table")
	}
	// control: the role may still use the two definer entry points (they reach the key for it without disclosing it)
	var active bool
	kid, _ := prooftest.ProcessKey(t, owner)
	if err := rt.Raw().QueryRow(ctx, `SELECT actor_proof_key_active($1)`, kid).Scan(&active); err != nil || !active {
		t.Fatalf("control: actor_proof_key_active(%s) as igaming_runtime = %v (%v), want true", kid, active, err)
	}
}

// ---- proof semantics against the moved key ------------------------------------------------------------------------

// presentProof runs actor_proof_require as igaming_runtime with token in app.actor_proof, inside a transaction that is always rolled back (no nonce
// is left behind). twice presents the same proof a second time in the same transaction (the replay case). It returns the SQLSTATE ("" = accepted).
func presentProof(t *testing.T, rt *db.Pool, token string, actor uuid.UUID, tenant uuid.UUID, op, target, payload string, twice bool) string {
	t.Helper()
	call := func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT actor_proof_require($1::uuid, 'tenant', $2::uuid, $3, $4, $5)`, actor, tenant, op, target, payload)
		return e
	}
	err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if token != "" {
			if _, e := tx.Exec(ctx, `SELECT set_config('app.actor_proof', $1, true)`, token); e != nil {
				return e
			}
		}
		if e := call(ctx, tx); e != nil {
			return e
		}
		if twice {
			if e := call(ctx, tx); e != nil {
				return e
			}
		}
		return errRollback
	})
	if errors.Is(err, errRollback) {
		return ""
	}
	if c := sqlstate(err); c != "" {
		return c
	}
	t.Fatalf("unexpected error: %v", err)
	return ""
}

func TestKeyIsolation_ProofRulesHoldAgainstTheMovedKey(t *testing.T) {
	owner := kiOwnerURL(t)
	rt := kiConnect(t, kiRuntimeURL(t, ""), "igaming_runtime")
	f := prooftest.ProcessForger(t, owner)
	actor, tenant := uuid.New(), uuid.New()
	const op, target = "ledger_adjustment:initiate", "new"
	payload := strings.Repeat("0", 64)
	now := time.Now().Unix()

	retiredKID := prooftest.RandomKID(t)
	retiredKey := prooftest.NewKey(t)
	prooftest.Provision(t, owner, retiredKID, retiredKey)
	prooftest.Retire(t, owner, retiredKID)

	cases := []struct {
		name, token string
		twice       bool
		want        string
	}{
		{"valid proof under the moved key", f.Valid(t, actor, "tenant", tenant, op, target, payload), false, ""},
		{"replay of one proof (AP005)", f.Valid(t, actor, "tenant", tenant, op, target, payload), true, "AP005"},
		{"no proof (AP001)", "", false, "AP001"},
		{"malformed proof (AP001)", "v1|garbage", false, "AP001"},
		{"wrong key under the right kid (AP002)", prooftest.Forger{KID: f.KID, Key: prooftest.NewKey(t)}.Valid(t, actor, "tenant", tenant, op, target, payload), false, "AP002"},
		{"unknown kid (AP002)", prooftest.Forger{KID: prooftest.RandomKID(t), Key: f.Key}.Valid(t, actor, "tenant", tenant, op, target, payload), false, "AP002"},
		{"retired kid (AP002)", prooftest.Forger{KID: retiredKID, Key: retiredKey}.Valid(t, actor, "tenant", tenant, op, target, payload), false, "AP002"},
		{"expired (AP003)", f.Token(t, actor, "tenant", tenant, op, target, payload, now-120, now-90), false, "AP003"},
		{"lifetime above 60 s (AP003)", f.Token(t, actor, "tenant", tenant, op, target, payload, now, now+61), false, "AP003"},
		{"bound to another actor (AP004)", f.Valid(t, uuid.New(), "tenant", tenant, op, target, payload), false, "AP004"},
		{"bound to another payload (AP004)", f.Valid(t, actor, "tenant", tenant, op, target, strings.Repeat("1", 64)), false, "AP004"},
	}
	for _, c := range cases {
		if got := presentProof(t, rt, c.token, actor, tenant, op, target, payload, c.twice); got != c.want {
			t.Errorf("%s: got SQLSTATE %q, want %q", c.name, got, c.want)
		}
	}
}

// ---- the production startup gate ---------------------------------------------------------------------------------

func TestKeyIsolation_StartupGate(t *testing.T) {
	ctx := context.Background()
	owner := kiOwnerURL(t)
	rt := kiConnect(t, kiRuntimeURL(t, ""), "igaming_runtime")
	iss := prooftest.Install(t, owner)
	if err := actorproof.VerifyConfiguredInProduction(ctx, "production", iss, rt); err != nil {
		t.Fatalf("the provisioned process kid must be admitted: %v", err)
	}
	k := prooftest.RandomKID(t)
	unprov, err := actorproof.NewIssuer(k, map[string][]byte{k: prooftest.NewKey(t)})
	if err != nil {
		t.Fatal(err)
	}
	if err := actorproof.VerifyConfiguredInProduction(ctx, "production", unprov, rt); err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("an unprovisioned kid must be refused: %v", err)
	}
	rk, rkey := prooftest.RandomKID(t), prooftest.NewKey(t)
	prooftest.Provision(t, owner, rk, rkey)
	retired, err := actorproof.NewIssuer(rk, map[string][]byte{rk: rkey})
	if err != nil {
		t.Fatal(err)
	}
	if err := actorproof.VerifyConfiguredInProduction(ctx, "production", retired, rt); err != nil {
		t.Fatalf("control: the freshly provisioned kid must be admitted before it is retired: %v", err)
	}
	prooftest.Retire(t, owner, rk)
	if err := actorproof.VerifyConfiguredInProduction(ctx, "production", retired, rt); err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("a retired kid must be refused: %v", err)
	}
	if err := actorproof.VerifyConfiguredInProduction(ctx, "production", nil, rt); err == nil {
		t.Fatal("production without an issuer must be refused")
	}
	if err := actorproof.VerifyConfiguredInProduction(ctx, "staging", unprov, rt); err != nil {
		t.Fatalf("outside production the gate is a no-op: %v", err)
	}
}

// ---- migration 0166 on scratch databases ----------------------------------------------------------------------------

func kiMigDir(t *testing.T, through int) string {
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
		if v > through {
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

// kiSnapshot renders the catalog of schema public and actorproof_private (and the list of every non-system schema with its ACL) by NAME:
// relations with owner/ACL/RLS flags, columns with defaults and column ACLs, constraints, indexes, triggers, policies, functions (source digest,
// ACL, config, definer flag, owner) and types.
func kiSnapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.Raw().QueryRow(context.Background(), `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
		SELECT 'ns:' || n.nspname || ':' || pg_get_userbyid(n.nspowner) || ':' || COALESCE(n.nspacl::text, '') AS x
		  FROM pg_namespace n WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
		UNION ALL
		SELECT 'rel:' || n.nspname || '.' || c.relname || ':' || c.relkind::text || ':' || pg_get_userbyid(c.relowner) || ':' || COALESCE(c.relacl::text, '')
		       || ':' || c.relrowsecurity || ':' || c.relforcerowsecurity
		  FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('public', 'actorproof_private')
		UNION ALL
		SELECT 'col:' || n.nspname || '.' || c.relname || '.' || a.attname || ':' || a.attnum || ':' || format_type(a.atttypid, a.atttypmod) || ':' || a.attnotnull
		       || ':' || COALESCE(pg_get_expr(d.adbin, d.adrelid), '') || ':' || COALESCE(a.attacl::text, '')
		  FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		 WHERE n.nspname IN ('public', 'actorproof_private') AND a.attnum > 0 AND NOT a.attisdropped AND c.relkind IN ('r', 'p', 'v', 'm')
		UNION ALL
		SELECT 'con:' || n.nspname || '.' || c.relname || ':' || co.conname || ':' || pg_get_constraintdef(co.oid)
		  FROM pg_constraint co JOIN pg_class c ON c.oid = co.conrelid JOIN pg_namespace n ON n.oid = co.connamespace WHERE n.nspname IN ('public', 'actorproof_private')
		UNION ALL
		SELECT 'idx:' || pg_get_indexdef(i.indexrelid)
		  FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname IN ('public', 'actorproof_private')
		UNION ALL
		SELECT 'trg:' || n.nspname || '.' || c.relname || ':' || tg.tgname || ':' || tg.tgenabled::text || ':' || pg_get_triggerdef(tg.oid)
		  FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE NOT tg.tgisinternal AND n.nspname IN ('public', 'actorproof_private')
		UNION ALL
		SELECT 'pol:' || schemaname || '.' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || COALESCE(qual, '') || ':' || COALESCE(with_check, '')
		  FROM pg_policies WHERE schemaname IN ('public', 'actorproof_private')
		UNION ALL
		SELECT 'fn:' || n.nspname || '.' || p.proname || '(' || pg_get_function_identity_arguments(p.oid) || '):' || md5(p.prosrc) || ':' || COALESCE(p.proacl::text, '')
		       || ':' || COALESCE(array_to_string(p.proconfig, ','), '') || ':' || p.prosecdef || ':' || pg_get_userbyid(p.proowner)
		  FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace WHERE n.nspname IN ('public', 'actorproof_private')
		UNION ALL
		SELECT 'type:' || n.nspname || '.' || ty.typname || ':' || ty.typtype::text
		  FROM pg_type ty JOIN pg_namespace n ON n.oid = ty.typnamespace WHERE n.nspname IN ('public', 'actorproof_private')) s`).Scan(&snap); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	return snap
}

func kiDiff(a, b string) string {
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
	if len(out) > 30 {
		out = append(out[:30], "...")
	}
	return strings.Join(out, "\n")
}

// kiKeys returns "count:digest" over every key row (kid, status, times, digest of the secret), wherever the table is.
func kiKeys(t *testing.T, pool *db.Pool) string {
	t.Helper()
	ctx := context.Background()
	var tbl string
	if err := pool.Raw().QueryRow(ctx, `SELECT COALESCE(to_regclass('actorproof_private.actor_proof_keys')::text, to_regclass('public.actor_proof_keys')::text)`).Scan(&tbl); err != nil {
		t.Fatal(err)
	}
	if tbl == "actor_proof_keys" {
		tbl = "public.actor_proof_keys"
	}
	var s string
	if err := pool.Raw().QueryRow(ctx, `SELECT count(*) || ':' || COALESCE(md5(string_agg(kid || '/' || status || '/' || created_at || '/' || COALESCE(retired_at::text, '')
		|| '/' || md5(secret), ',' ORDER BY kid)), '') FROM `+tbl).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return tbl + "|" + s
}

func kiExec(t *testing.T, pool *db.Pool, sql string) {
	t.Helper()
	if _, err := pool.Raw().Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func kiApplied(t *testing.T, pool *db.Pool, v int) bool {
	t.Helper()
	var ok bool
	if err := pool.Raw().QueryRow(context.Background(), `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, v).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

func TestMigration0166_UpDownUp_LegacyRows_Refusals(t *testing.T) {
	ctx := context.Background()
	u := scratchdb.New(t, "m0166_")
	pool, err := db.Connect(ctx, u, 6, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(ctx, kiMigDir(t, 165)); err != nil {
		t.Fatalf("migrate to 0165: %v", err)
	}
	// legacy rows: the process key (active; through the prooftest fallback to public) and a retired key
	prooftest.InstallVia(t, pool.Raw(), t.Name()+"/"+uuid.NewString())
	if _, err := pool.Raw().Exec(ctx, `INSERT INTO public.actor_proof_keys (kid, secret, status, retired_at) VALUES ($1, $2, 'retired', now())`,
		prooftest.RandomKID(t), prooftest.NewKey(t)); err != nil {
		t.Fatal(err)
	}
	snap0, keys0 := kiSnapshot(t, pool), kiKeys(t, pool)
	if !strings.HasPrefix(keys0, "public.actor_proof_keys|2:") {
		t.Fatalf("legacy key rows: %s", keys0)
	}
	dir := kiMigDir(t, 166)

	// up refusals (APM01): each leaves the catalog and the rows exactly as they were
	upRefusals := []struct{ name, plant, undo string }{
		{"schema already exists", `CREATE SCHEMA actorproof_private`, `DROP SCHEMA actorproof_private`},
		{"a grant to the runtime role", `GRANT SELECT ON public.actor_proof_keys TO igaming_runtime`, `REVOKE SELECT ON public.actor_proof_keys FROM igaming_runtime`},
		{"a grant to PUBLIC", `GRANT SELECT ON public.actor_proof_keys TO PUBLIC`, `REVOKE SELECT ON public.actor_proof_keys FROM PUBLIC`},
		{"a column grant", `GRANT SELECT (kid) ON public.actor_proof_keys TO igaming_runtime`, `REVOKE SELECT (kid) ON public.actor_proof_keys FROM igaming_runtime`},
		{"a dependent view", `CREATE VIEW public.zz_apk AS SELECT kid FROM public.actor_proof_keys`, `DROP VIEW public.zz_apk`},
	}
	for _, r := range upRefusals {
		kiExec(t, pool, r.plant)
		planted := kiSnapshot(t, pool)
		_, err := pool.MigrateUp(ctx, dir)
		if err == nil || !strings.Contains(err.Error(), "APM01") {
			t.Fatalf("up with %s: want APM01, got %v", r.name, err)
		}
		if got := kiSnapshot(t, pool); got != planted || kiApplied(t, pool, 166) || kiKeys(t, pool) != keys0 {
			t.Fatalf("a refused up (%s) must change nothing:\n%s", r.name, kiDiff(planted, got))
		}
		kiExec(t, pool, r.undo)
		if got := kiSnapshot(t, pool); got != snap0 {
			t.Fatalf("fixture cleanup (%s) did not restore the 0165 catalog:\n%s", r.name, kiDiff(snap0, got))
		}
	}

	// up: the rows move (never copied), the old location is gone, the moved key verifies
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatalf("migrate 0166 up: %v", err)
	}
	snap1 := kiSnapshot(t, pool)
	if got := kiKeys(t, pool); got != "actorproof_private.actor_proof_keys|"+strings.TrimPrefix(keys0, "public.actor_proof_keys|") {
		t.Fatalf("key rows after up: %s, want the same rows as %s", got, keys0)
	}
	for _, want := range []string{
		"ns:actorproof_private:igaming:{igaming=UC/igaming}",
		"rel:actorproof_private.actor_proof_keys:r:igaming:{igaming=arwdDxt/igaming}:true:false",
		"idx:CREATE UNIQUE INDEX actor_proof_keys_pkey ON actorproof_private.actor_proof_keys USING btree (kid)",
	} {
		if !strings.Contains(snap1, want) {
			t.Fatalf("0166 up must produce %q", want)
		}
	}
	if strings.Contains(snap1, "rel:public.actor_proof_keys:") {
		t.Fatal("0166 up must remove public.actor_proof_keys")
	}
	rt := kiConnect(t, kiRuntimeURL(t, kiDBName(t, u)), "igaming_runtime")
	f := prooftest.Forger{}
	f.KID, f.Key = prooftest.ProcessKey(t, kiOwnerURL(t))
	actor, tenant, payload := uuid.New(), uuid.New(), strings.Repeat("0", 64)
	if got := presentProof(t, rt, f.Valid(t, actor, "tenant", tenant, "ledger_adjustment:initiate", "new", payload), actor, tenant, "ledger_adjustment:initiate", "new", payload, true); got != "AP005" {
		t.Fatalf("the moved key: a valid proof then its replay = %q, want AP005 after one acceptance", got)
	}
	if got := presentProof(t, rt, f.Valid(t, actor, "tenant", tenant, "ledger_adjustment:initiate", "new", payload), actor, tenant, "ledger_adjustment:initiate", "new", payload, false); got != "" {
		t.Fatalf("the moved key must verify a valid proof, got %q", got)
	}
	err = rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `SELECT secret FROM actorproof_private.actor_proof_keys`)
		return e
	})
	if sqlstate(err) != "42501" {
		t.Fatalf("on the migrated scratch database the runtime role must be denied the key: %v", err)
	}

	// down: the 0165 catalog byte for byte, the rows intact
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("migrate 0166 down: %v", err)
	}
	if got := kiSnapshot(t, pool); got != snap0 {
		t.Fatalf("0166 down must restore the 0165 catalog byte for byte:\n%s", kiDiff(snap0, got))
	}
	if got := kiKeys(t, pool); got != keys0 {
		t.Fatalf("0166 down must keep the key rows: %s, want %s", got, keys0)
	}
	// up again equals the first up
	if _, err := pool.MigrateUp(ctx, dir); err != nil {
		t.Fatalf("migrate 0166 up again: %v", err)
	}
	if got := kiSnapshot(t, pool); got != snap1 {
		t.Fatalf("0166 up again must equal the first up:\n%s", kiDiff(snap1, got))
	}

	// down refusals (APM02): each leaves the catalog and the rows exactly as they were. Every grant plant must also be flagged by the backup
	// baseline's privilege invariant (deploy/backup/baseline.sql INV|actor_proof_key_reachable_by_non_owner), which is 0 on the clean schema.
	inv := baselineStatement(t, `SELECT 'INV\|actor_proof_key_reachable_by_non_owner\|'`)
	invOf := func() string {
		t.Helper()
		var line string
		if err := pool.Raw().QueryRow(ctx, inv).Scan(&line); err != nil {
			t.Fatal(err)
		}
		return strings.TrimPrefix(line, "INV|actor_proof_key_reachable_by_non_owner|")
	}
	if got := invOf(); got != "0" {
		t.Fatalf("baseline invariant on the clean 0166 schema = %s, want 0", got)
	}
	downRefusals := []struct {
		name, plant, undo string
		invFlags          bool
	}{
		{"another table in the schema", `CREATE TABLE actorproof_private.zz_extra (a int)`, `DROP TABLE actorproof_private.zz_extra`, false},
		{"a function in the schema", `CREATE FUNCTION actorproof_private.zz_f() RETURNS int LANGUAGE sql AS 'SELECT 1'`, `DROP FUNCTION actorproof_private.zz_f()`, false},
		{"USAGE granted on the schema", `GRANT USAGE ON SCHEMA actorproof_private TO igaming_runtime`, `REVOKE USAGE ON SCHEMA actorproof_private FROM igaming_runtime`, true},
		{"CREATE granted on the schema", `GRANT CREATE ON SCHEMA actorproof_private TO igaming_runtime`, `REVOKE CREATE ON SCHEMA actorproof_private FROM igaming_runtime`, true},
		{"a table grant", `GRANT SELECT ON actorproof_private.actor_proof_keys TO igaming_runtime`, `REVOKE SELECT ON actorproof_private.actor_proof_keys FROM igaming_runtime`, true},
		{"a write grant", `GRANT INSERT ON actorproof_private.actor_proof_keys TO igaming_runtime`, `REVOKE INSERT ON actorproof_private.actor_proof_keys FROM igaming_runtime`, true},
		{"a PUBLIC grant", `GRANT SELECT ON actorproof_private.actor_proof_keys TO PUBLIC`, `REVOKE SELECT ON actorproof_private.actor_proof_keys FROM PUBLIC`, true},
		{"a column grant", `GRANT SELECT (kid) ON actorproof_private.actor_proof_keys TO igaming_runtime`, `REVOKE SELECT (kid) ON actorproof_private.actor_proof_keys FROM igaming_runtime`, true},
		{"a dependent view", `CREATE VIEW public.zz_apk2 AS SELECT kid FROM actorproof_private.actor_proof_keys`, `DROP VIEW public.zz_apk2`, false},
		{"public.actor_proof_keys exists again", `CREATE TABLE public.actor_proof_keys (a int)`, `DROP TABLE public.actor_proof_keys`, false},
	}
	keys1 := kiKeys(t, pool)
	for _, r := range downRefusals {
		kiExec(t, pool, r.plant)
		if got := invOf(); r.invFlags && got == "0" {
			t.Fatalf("the baseline invariant must flag %s (got 0)", r.name)
		}
		planted := kiSnapshot(t, pool)
		_, err := pool.MigrateDown(ctx, dir, 1)
		if err == nil || !strings.Contains(err.Error(), "APM02") {
			t.Fatalf("down with %s: want APM02, got %v", r.name, err)
		}
		if got := kiSnapshot(t, pool); got != planted || !kiApplied(t, pool, 166) || kiKeys(t, pool) != keys1 {
			t.Fatalf("a refused down (%s) must change nothing:\n%s", r.name, kiDiff(planted, got))
		}
		kiExec(t, pool, r.undo)
		if got := kiSnapshot(t, pool); got != snap1 {
			t.Fatalf("fixture cleanup (%s) did not restore the 0166 catalog:\n%s", r.name, kiDiff(snap1, got))
		}
	}
	if _, err := pool.MigrateDown(ctx, dir, 1); err != nil {
		t.Fatalf("the clean down after the refusals: %v", err)
	}
	if got := kiSnapshot(t, pool); got != snap0 {
		t.Fatalf("the clean down must restore the 0165 catalog:\n%s", kiDiff(snap0, got))
	}
}

// kiPrivateSnapshot is kiSnapshot restricted to the actorproof_private schema (the part the restore DDL must reproduce).
func kiPrivateSnapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var keep []string
	for _, l := range strings.Split(kiSnapshot(t, pool), "\n") {
		if strings.Contains(l, "actorproof_private") {
			keep = append(keep, l)
		}
	}
	return strings.Join(keep, "\n")
}

// baselineStatement extracts ONE statement (from its quoted prefix to the next ';' at a line end) from deploy/backup/baseline.sql: the
// definition the backup and verify.sh use, not a copy.
func baselineStatement(t *testing.T, prefixRE string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "deploy", "backup", "baseline.sql"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)` + prefixRE + `.*?;\n`).FindString(string(b))
	if m == "" {
		t.Fatalf("baseline.sql has no statement starting %s", prefixRE)
	}
	return strings.TrimSuffix(strings.TrimSpace(m), ";")
}

func baselineFP(t *testing.T) string {
	t.Helper()
	return baselineStatement(t, `SELECT 'FP\|actorproof_private_md5\|'`)
}

func TestRestoreDDL_MatchesTheMigratedKeySchema(t *testing.T) {
	ctx := context.Background()
	migrated := kiConnect(t, kiOwnerURL(t), "igaming")
	u := scratchdb.New(t, "apkddl_")
	restored, err := db.Connect(ctx, u, 2, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.Close)
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "backup", "actorproof-private-schema.sql"))
	if err != nil {
		t.Fatal(err)
	}
	var sqlLines []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), `\`) {
			continue // psql meta-commands (\set ON_ERROR_STOP on)
		}
		sqlLines = append(sqlLines, l)
	}
	if _, err := restored.Raw().Exec(ctx, strings.Join(sqlLines, "\n")); err != nil {
		t.Fatalf("apply actorproof-private-schema.sql: %v", err)
	}
	if a, b := kiPrivateSnapshot(t, migrated), kiPrivateSnapshot(t, restored); a != b {
		t.Fatalf("the restore DDL differs from the migrated key schema:\n%s", kiDiff(a, b))
	}
	fp := baselineFP(t)
	var fa, fb string
	if err := migrated.Raw().QueryRow(ctx, fp).Scan(&fa); err != nil {
		t.Fatal(err)
	}
	if err := restored.Raw().QueryRow(ctx, fp).Scan(&fb); err != nil {
		t.Fatal(err)
	}
	if fa != fb || strings.HasSuffix(fa, "|count=0") {
		t.Fatalf("baseline FP of the key schema: migrated %s, restore DDL %s (must be equal and non-empty)", fa, fb)
	}
	// the FP is not blind: each drift of the restored schema (a grant, a column grant, RLS off, a changed CHECK, an extra object) changes it
	for _, d := range []struct{ do, undo string }{
		{`GRANT SELECT ON actorproof_private.actor_proof_keys TO igaming_runtime`, `REVOKE SELECT ON actorproof_private.actor_proof_keys FROM igaming_runtime`},
		{`GRANT USAGE ON SCHEMA actorproof_private TO igaming_runtime`, `REVOKE USAGE ON SCHEMA actorproof_private FROM igaming_runtime`},
		{`GRANT SELECT (kid) ON actorproof_private.actor_proof_keys TO igaming_runtime`, `REVOKE SELECT (kid) ON actorproof_private.actor_proof_keys FROM igaming_runtime`},
		{`ALTER TABLE actorproof_private.actor_proof_keys DISABLE ROW LEVEL SECURITY`, `ALTER TABLE actorproof_private.actor_proof_keys ENABLE ROW LEVEL SECURITY`},
		{`ALTER TABLE actorproof_private.actor_proof_keys DROP CONSTRAINT actor_proof_keys_secret_check, ADD CONSTRAINT actor_proof_keys_secret_check CHECK (octet_length(secret) >= 16)`,
			`ALTER TABLE actorproof_private.actor_proof_keys DROP CONSTRAINT actor_proof_keys_secret_check, ADD CONSTRAINT actor_proof_keys_secret_check CHECK (octet_length(secret) >= 32)`},
		{`CREATE TABLE actorproof_private.zz_extra (a int)`, `DROP TABLE actorproof_private.zz_extra`},
	} {
		kiExec(t, restored, d.do)
		var fd string
		if err := restored.Raw().QueryRow(ctx, fp).Scan(&fd); err != nil {
			t.Fatal(err)
		}
		kiExec(t, restored, d.undo)
		if fd == fa {
			t.Errorf("the key-schema FP did not change after: %s", d.do)
		}
	}
	if err := restored.Raw().QueryRow(ctx, fp).Scan(&fb); err != nil || fb != fa {
		t.Fatalf("after undoing the drifts the FP must equal the migrated one again: %s vs %s (%v)", fb, fa, err)
	}
	// ... and it records WHICH privilege WHO holds, not only how many grants exist (SELECT vs INSERT for the same role, same role vs PUBLIC)
	fpWith := func(do, undo string) string {
		t.Helper()
		kiExec(t, restored, do)
		defer kiExec(t, restored, undo)
		var v string
		if err := restored.Raw().QueryRow(ctx, fp).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	sel := fpWith(`GRANT SELECT ON actorproof_private.actor_proof_keys TO igaming_runtime`, `REVOKE SELECT ON actorproof_private.actor_proof_keys FROM igaming_runtime`)
	ins := fpWith(`GRANT INSERT ON actorproof_private.actor_proof_keys TO igaming_runtime`, `REVOKE INSERT ON actorproof_private.actor_proof_keys FROM igaming_runtime`)
	pub := fpWith(`GRANT SELECT ON actorproof_private.actor_proof_keys TO PUBLIC`, `REVOKE SELECT ON actorproof_private.actor_proof_keys FROM PUBLIC`)
	if sel == ins || sel == pub {
		t.Errorf("the key-schema FP must tell grants apart: SELECT->runtime %s, INSERT->runtime %s, SELECT->PUBLIC %s", sel, ins, pub)
	}
	var n int
	if err := restored.Raw().QueryRow(ctx, `SELECT count(*) FROM actorproof_private.actor_proof_keys`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("the restore DDL must create the key table EMPTY: %d (%v)", n, err)
	}
}
