//go:build integration

package aml_test

import (
	"context"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// d1 (D7): the unrouted, loosening-capable setter aml_screening_policy_set (0154) is OWNER-ONLY since migration 0191. These tests prove, on a
// real migrated database and as the REAL runtime role:
//   (a) neither the runtime role nor PUBLIC can execute it;
//   (b) calling it as the runtime role is 42501 (insufficient_privilege), also through the Go service, and nothing is written;
//   (c) of every function the runtime role can execute, the ONLY ones that can write aml_screening_policies are the tighten-only author and
//       the governed-loosening executor (the list is enumerated from the catalogue and pinned);
//   (d) an ordinary runtime actor cannot remove or weaken the sanctions list by any path.
// The owner path (a fixture or a break-glass operator) keeps working: the function body is unchanged.

const d7Setter = `aml_screening_policy_set(uuid, uuid, text[], bigint, bigint, timestamptz, text, text)`

func TestD7_BreakGlassSetter_IsNotExecutableByRuntimeOrPublic(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	r := catalogQuery(t, w, `SELECT has_function_privilege('igaming_runtime', $1::regprocedure, 'EXECUTE'), has_function_privilege('public', $1::regprocedure, 'EXECUTE'),
		has_function_privilege(current_user, $1::regprocedure, 'EXECUTE')`, d7Setter)[0]
	if r[0].(bool) || r[1].(bool) {
		t.Fatalf("the break-glass setter must be executable by neither the runtime role nor PUBLIC (runtime=%v public=%v)", r[0], r[1])
	}
	if !r[2].(bool) {
		t.Fatal("the OWNER path must keep working: the table owner can execute the setter")
	}
	// The ACL itself names neither the runtime role nor PUBLIC (grantee 0), whatever has_function_privilege infers from role membership.
	acl := catalogQuery(t, w, `SELECT count(*) FROM (SELECT (aclexplode(coalesce(p.proacl, acldefault('f', p.proowner)))).grantee AS g FROM pg_proc p WHERE p.oid = $1::regprocedure) x
		WHERE x.g = 0 OR x.g = (SELECT oid FROM pg_roles WHERE rolname = 'igaming_runtime')`, d7Setter)[0][0].(int64)
	if acl != 0 {
		t.Fatalf("the setter's ACL still names the runtime role or PUBLIC (%d entries)", acl)
	}
	// Its siblings keep the intended grants: the tighten-only author and the effective reader stay runtime-executable.
	for _, f := range []string{`aml_screening_policy_author(uuid, uuid, text[], bigint, bigint, timestamptz, text, text)`, `aml_screening_policy_effective(uuid, uuid)`} {
		if !catalogQuery(t, w, `SELECT has_function_privilege('igaming_runtime', $1::regprocedure, 'EXECUTE')`, f)[0][0].(bool) {
			t.Errorf("%s must stay executable by the runtime role", f)
		}
	}
}

func TestD7_RuntimeCallOfTheSetter_Is42501AndWritesNothing(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	before := policyCount(t, tw)
	// Raw SQL as the runtime role, in the tenant session shape every runtime caller has.
	err := tw.RT.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT aml_screening_policy_set($1, $2, ARRAY['pep']::text[], 3600, 0, now(), 'D7-TEST', 'd7_test')`, tw.Tenant, tw.jur)
		return err
	})
	if pgCode(err) != "42501" {
		t.Fatalf("the runtime role calling the setter must get 42501, got %v", err)
	}
	// Through the Go service over the runtime pool, as a real quorum officer with a valid proof: the same refusal, no row.
	_, err = tw.svc.SetScreeningPolicy(ctx, tw.q.Officers[0].Actor, policyInput(tw.jur, time.Now()))
	if pgCode(err) != "42501" {
		t.Fatalf("SetScreeningPolicy over the runtime pool must surface 42501, got %v", err)
	}
	if after := policyCount(t, tw); after != before {
		t.Fatalf("a refused call wrote a policy version: %d -> %d", before, after)
	}
}

func TestD7_OwnerPath_StillAuthorsAPolicyWithTheOfficerControls(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour)
	before := policyCount(t, tw)
	in := policyInput(tw.jur, time.Now())
	if _, err := ownerSvc(t).SetScreeningPolicy(ctx, tw.q.Officers[0].Actor, in); err != nil {
		t.Fatalf("the owner path must keep working for an authorised officer: %v", err)
	}
	if after := policyCount(t, tw); after != before+1 {
		t.Fatalf("the owner path wrote %d versions, want 1", after-before)
	}
	// The officer controls inside the function still apply to the owner caller: a non-officer actor is refused and nothing is written.
	other := tw.q.Officers[0].Actor
	other.StaffID = uuid.New()
	if _, err := ownerSvc(t).SetScreeningPolicy(ctx, other, in); err == nil {
		t.Fatal("an actor without the officer grant must be refused even on the owner path")
	}
	if after := policyCount(t, tw); after != before+1 {
		t.Fatalf("a refused owner call wrote a version: %d", after-before)
	}
}

// tableWriters enumerates, from the live catalogue, the functions that can WRITE the table: the ones whose source names a DML statement on it
// (or builds dynamic SQL that names it), closed transitively over the functions that call a writer by name. It returns the writers
// that the runtime role (or PUBLIC) can execute.
func tableWriters(t *testing.T, w *amlWorld, table string) map[string]bool {
	t.Helper()
	rows := catalogQuery(t, w, `SELECT p.proname, p.prosrc, (has_function_privilege('igaming_runtime', p.oid, 'EXECUTE') OR has_function_privilege('public', p.oid, 'EXECUTE')) AS rt
		FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		WHERE n.nspname NOT IN ('pg_catalog', 'information_schema') AND p.prokind IN ('f', 'p')`)
	type fn struct {
		src string
		rt  bool
	}
	fns := map[string]*fn{}
	for _, r := range rows {
		name := r[0].(string)
		if cur, ok := fns[name]; ok {
			cur.src += "\n" + r[1].(string)
			cur.rt = cur.rt || r[2].(bool)
			continue
		}
		fns[name] = &fn{src: r[1].(string), rt: r[2].(bool)}
	}
	dml := regexp.MustCompile(`(?is)\b(insert\s+into|update|delete\s+from|truncate(\s+table)?|merge\s+into|copy)\s+(only\s+)?("?public"?\.)?"?` + regexp.QuoteMeta(table) + `\b`)
	dyn := regexp.MustCompile(`(?is)\bexecute\b[^;]*` + regexp.QuoteMeta(table))
	writers := map[string]bool{}
	for name, f := range fns {
		if dml.MatchString(f.src) || dyn.MatchString(f.src) {
			writers[name] = true
		}
	}
	for changed := true; changed; {
		changed = false
		for name, f := range fns {
			if writers[name] {
				continue
			}
			for wn := range writers {
				if regexp.MustCompile(`\b` + regexp.QuoteMeta(wn) + `\s*\(`).MatchString(f.src) {
					writers[name] = true
					changed = true
					break
				}
			}
		}
	}
	out := map[string]bool{}
	for name := range writers {
		if fns[name].rt {
			out[name] = true
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestD7_OnlyAllowListedRuntimeFunctionsWriteTheScreeningPolicies(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	got := sortedKeys(tableWriters(t, w, "aml_screening_policies"))
	// The pinned allow-list: the tighten-only author (0189) and the executor of the governed two-Person loosening (0190). Anything else that
	// the runtime role can execute and that writes the table (directly, dynamically or through a call chain) is a new, ungoverned path.
	want := []string{"aml_screening_policy_author", "screening_policy_loosen_apply"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("runtime-executable writers of aml_screening_policies = %v, want exactly %v (a new writer needs a recorded decision and a pin change)", got, want)
	}
	// The detector is not vacuous: the owner-only setter IS a writer, it is simply not runtime-executable.
	all := catalogQuery(t, w, `SELECT count(*) FROM pg_proc WHERE proname = 'aml_screening_policy_set' AND prosrc ~* 'insert\s+into\s+public\.aml_screening_policies'`)[0][0].(int64)
	if all != 1 {
		t.Fatal("test premise: the setter's source must show the insert the detector looks for")
	}
}

func TestD7_ASanctionsListCannotBeRemovedOrWeakenedByARuntimeActor(t *testing.T) {
	ctx := context.Background()
	tw := newTrigWorld(t, 24*time.Hour, time.Hour) // the latest version: sanctions + pep
	A := tw.q.Officers[0].Actor
	lists := func() string {
		var s string
		if err := tw.Owner.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT array_to_string(lists, ',') FROM aml_screening_policies WHERE tenant_id = $1 AND licensing_jurisdiction_id = $2
				ORDER BY created_at DESC, id DESC LIMIT 1`, tw.Tenant, tw.jur).Scan(&s)
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if got := lists(); got != "sanctions,pep" && got != "pep,sanctions" {
		t.Fatalf("premise: %q", got)
	}
	before := policyCount(t, tw)
	same := func() {
		t.Helper()
		if n := policyCount(t, tw); n != before {
			t.Fatalf("a refused path wrote a version (%d -> %d)", before, n)
		}
	}
	// 1. The routed, tighten-only author refuses the removal with the closed code.
	in := policyInput(tw.jur, time.Now())
	in.Lists = []aml.ListType{aml.ListPEP}
	_, err := tw.svc.AuthorScreeningPolicy(ctx, A, in)
	if aml.RefusalCode(err) != "policy_loosening_refused" {
		t.Fatalf("the tighten-only route must refuse a list removal with policy_loosening_refused, got %v (code %q)", err, aml.RefusalCode(err))
	}
	same()
	// 2. The break-glass setter is not reachable (42501), as raw SQL and through the service.
	if _, err := tw.svc.SetScreeningPolicy(ctx, A, in); pgCode(err) != "42501" {
		t.Fatalf("the setter must be 42501 for the runtime role, got %v", err)
	}
	same()
	// 3. Direct table writes are denied to the runtime role by privilege (and FORCE RLS has no runtime policy behind it).
	for name, stmt := range map[string]string{
		"insert": `INSERT INTO aml_screening_policies (tenant_id, licensing_jurisdiction_id, lists, rescreen_interval_seconds, backfill_grace_seconds, effective_from,
			legal_review_reference, reason_code, created_by, created_by_person_id, created_by_scope) VALUES ($1, $2, ARRAY['pep'], 3600, 0, now(), 'x', 'x', $3, $3, 'platform_acting')`,
		"update": `UPDATE aml_screening_policies SET lists = ARRAY['pep'] WHERE tenant_id = $1`,
		"delete": `DELETE FROM aml_screening_policies WHERE tenant_id = $1`,
		"select": `SELECT count(*) FROM aml_screening_policies WHERE tenant_id = $1`,
	} {
		err := tw.RT.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
			if name == "insert" {
				_, err := tx.Exec(ctx, stmt, tw.Tenant, tw.jur, uuid.New())
				return err
			}
			_, err := tx.Exec(ctx, stmt, tw.Tenant)
			return err
		})
		if pgCode(err) != "42501" {
			t.Errorf("direct %s as the runtime role: want 42501, got %v", name, err)
		}
	}
	same()
	// 4. The executor of the governed loosening is runtime-executable but is a no-op without an executed two-Person request of the transaction.
	err = tw.RT.WithTenant(ctx, tw.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `SELECT screening_policy_loosen_apply($1)`, uuid.New())
		return err
	})
	if err == nil {
		t.Fatal("screening_policy_loosen_apply must refuse a request that was not executed in this transaction")
	}
	same()
	if got := lists(); got != "sanctions,pep" && got != "pep,sanctions" {
		t.Fatalf("the sanctions list changed: %q", got)
	}
}

// deploy/init-app-role.sql re-asserts the runtime grants whenever it is re-run (it is the bootstrap for a restored or older database). Its
// AML block must NOT hand the break-glass setter back to the runtime role. The block is run on a THROWAWAY database (no role statement is
// executed: only the AML grant block is extracted).
func TestD7_InitAppRoleSQL_DoesNotRegrantTheSetter(t *testing.T) {
	if os.Getenv("TEST_RUNTIME_DATABASE_URL") == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set")
	}
	ctx := context.Background()
	ownerURL := scratchdb.New(t, "d7init_")
	pool, err := db.Connect(ctx, ownerURL, 5, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(ctx, "../../migrations"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile("../../deploy/init-app-role.sql")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	start := strings.Index(s, "-- ADR 0116 (migrations 0140-0142, AML holds / cases / four-eyes): every aml_* table is DEFINER-ONLY.")
	if start < 0 {
		t.Fatal("the AML grant block of init-app-role.sql was not found")
	}
	end := strings.Index(s[start:], "\n$$;")
	if end < 0 {
		t.Fatal("the end of the AML grant block was not found")
	}
	block := s[start : start+end+len("\n$$;")]
	if !strings.Contains(block, "aml_screening_policy_author(") || strings.Contains(block, "'aml_screening_policy_set(") {
		t.Fatal("the AML grant block must grant the author and must not list the setter in its GRANT loop")
	}
	// Premise: a hostile earlier script (or a restore from before 0191) had granted the setter.
	if _, err := pool.Raw().Exec(ctx, `GRANT EXECUTE ON FUNCTION `+d7Setter+` TO igaming_runtime`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := pool.Raw().Exec(ctx, block); err != nil {
			t.Fatalf("run %d of the AML grant block: %v", i+1, err)
		}
		var setter, author bool
		if err := pool.Raw().QueryRow(ctx, `SELECT has_function_privilege('igaming_runtime', $1::regprocedure, 'EXECUTE'),
			has_function_privilege('igaming_runtime', 'aml_screening_policy_author(uuid, uuid, text[], bigint, bigint, timestamptz, text, text)'::regprocedure, 'EXECUTE')`, d7Setter).Scan(&setter, &author); err != nil {
			t.Fatal(err)
		}
		if setter || !author {
			t.Fatalf("after init-app-role.sql run %d: setter=%v (want false) author=%v (want true)", i+1, setter, author)
		}
	}
}
