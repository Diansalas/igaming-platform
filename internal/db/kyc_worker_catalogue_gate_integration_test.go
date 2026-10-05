//go:build integration

// PRH-2 E1 (ADR 0106 section 10.4 test 31, security O-1): the PERMANENT
// catalogue gate. On the HEAD catalogue (pg_policies), for every table other
// than kyc_submission_outbox:
//
//   - every permissive INSERT / UPDATE / DELETE / ALL policy must be
//     UNSATISFIABLE by a session that has only
//     app.platform_service_id = 'kyc_submission_worker' set, OR the table must
//     carry all four kyc_worker_fence_* restrictive policies;
//   - every permissive SELECT policy satisfiable by that session must sit on a
//     fenced table or on the public reference allowlist.
//
// It is SEMANTIC, not lexical: each policy expression, exactly as stored in
// pg_policies, is re-created on a TEMP probe table (LIKE the real table, every
// NOT NULL dropped) and evaluated under the real worker session, as the runtime
// role. The probe row is the "defaults and NULLs" row, which is precisely the
// shape a NULL-tenant arm or a USING (true) arm admits. A new migration that
// adds such an arm without extending the fence therefore fails here. Its known
// limit (a guard that needs a non-NULL column value is not exercised by the
// all-NULL probe row) is covered by the lexical A-18 replay and by T-ID-3's
// real-row visibility probe.
package db

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

type catalogPolicy struct {
	table, name, cmd, qual, check string
	permissive                    bool
	roles                         []string
}

func loadPolicies(t *testing.T, rt *Pool) []catalogPolicy {
	t.Helper()
	var out []catalogPolicy
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tablename, policyname, cmd, coalesce(qual, ''), coalesce(with_check, ''), permissive = 'PERMISSIVE', roles::text[]
			   FROM pg_policies WHERE schemaname = 'public' ORDER BY tablename, policyname`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p catalogPolicy
			if err := rows.Scan(&p.table, &p.name, &p.cmd, &p.qual, &p.check, &p.permissive, &p.roles); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("load policies: %v", err)
	}
	return out
}

// workerSatisfies reports whether, under the worker session, the expression is
// satisfiable by the defaults-and-NULLs probe row of table. asCheck selects the
// INSERT WITH CHECK probe (an INSERT must succeed) instead of the USING probe
// (a SELECT must see the row). A probe error other than a plain RLS refusal is
// returned so a blind spot is never silent.
func workerSatisfies(rt *Pool, table, expr string, asCheck bool) (bool, error) {
	var sat bool
	var probeErr error
	errRollbackProbe := errors.New("probe: always roll back")
	_ = rt.WithPlatformService(context.Background(), ServiceKYCSubmissionWorker, func(ctx context.Context, tx pgx.Tx) error {
		fail := func(err error) error { probeErr = err; return errRollbackProbe }
		ident := pgx.Identifier{table}.Sanitize()
		// The probe is named exactly like the real table, in pg_temp, so a policy
		// expression that qualifies a column with the table name (alerts.kind)
		// resolves to the probe; every other relation in the expression still
		// resolves to the real one.
		probe := `pg_temp.` + ident
		if _, err := tx.Exec(ctx, `CREATE TEMP TABLE `+ident+` (LIKE public.`+ident+` INCLUDING DEFAULTS) ON COMMIT DROP`); err != nil {
			return fail(fmt.Errorf("create probe: %w", err))
		}
		var cols []string
		rows, err := tx.Query(ctx, `SELECT attname FROM pg_attribute WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped AND attnotnull`, "public."+ident)
		if err != nil {
			return fail(err)
		}
		for rows.Next() {
			var c string
			if err := rows.Scan(&c); err != nil {
				rows.Close()
				return fail(err)
			}
			cols = append(cols, c)
		}
		rows.Close()
		for _, c := range cols {
			if _, err := tx.Exec(ctx, `ALTER TABLE `+probe+` ALTER COLUMN `+pgx.Identifier{c}.Sanitize()+` DROP NOT NULL`); err != nil {
				return fail(fmt.Errorf("drop not null: %w", err))
			}
		}
		if !asCheck {
			// The probe row exists BEFORE RLS is enabled (so it is not itself
			// subject to an INSERT policy); the policy under test then decides
			// whether the worker session can see it.
			if _, err := tx.Exec(ctx, `INSERT INTO `+probe+` DEFAULT VALUES`); err != nil {
				return fail(fmt.Errorf("insert probe row: %w", err))
			}
		}
		for _, stmt := range []string{`ALTER TABLE ` + probe + ` ENABLE ROW LEVEL SECURITY`, `ALTER TABLE ` + probe + ` FORCE ROW LEVEL SECURITY`} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return fail(err)
			}
		}
		if asCheck {
			if _, err := tx.Exec(ctx, `CREATE POLICY gate_p ON `+probe+` AS PERMISSIVE FOR INSERT WITH CHECK (`+expr+`)`); err != nil {
				return fail(fmt.Errorf("create check policy: %w", err))
			}
			// A refusal (including a raising guard) means unsatisfiable.
			_, insErr := tx.Exec(ctx, `INSERT INTO `+probe+` DEFAULT VALUES`)
			sat = insErr == nil
			return errRollbackProbe
		}
		if _, err := tx.Exec(ctx, `CREATE POLICY gate_p ON `+probe+` AS PERMISSIVE FOR SELECT USING (`+expr+`)`); err != nil {
			return fail(fmt.Errorf("create using policy: %w", err))
		}
		var n int64
		// If the expression itself raises for the worker session (for example
		// alerting_session_scope() refusing the identity) it is unsatisfiable.
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+probe).Scan(&n); err != nil {
			sat = false
			return errRollbackProbe
		}
		sat = n > 0
		return errRollbackProbe
	})
	return sat, probeErr
}

// fenceTables returns the tables carrying all four kyc_worker_fence_* policies.
func fenceTables(policies []catalogPolicy) map[string]bool {
	counts := map[string]map[string]bool{}
	for _, p := range policies {
		if p.permissive || !strings.HasPrefix(p.name, "kyc_worker_fence_") {
			continue
		}
		if counts[p.table] == nil {
			counts[p.table] = map[string]bool{}
		}
		counts[p.table][strings.TrimPrefix(p.name, "kyc_worker_fence_")] = true
	}
	out := map[string]bool{}
	for tbl, c := range counts {
		if c["select"] && c["insert"] && c["update"] && c["delete"] {
			out[tbl] = true
		}
	}
	return out
}

// gateViolations evaluates the gate and returns the offending policies. fenced
// is the set of tables whose permissive policies are exempt.
func gateViolations(t *testing.T, rt *Pool, policies []catalogPolicy, fenced map[string]bool) (violations []string, evaluated int) {
	t.Helper()
	for _, p := range policies {
		if !p.permissive || p.table == "kyc_submission_outbox" || fenced[p.table] {
			continue
		}
		type expr struct {
			text    string
			asCheck bool
			what    string
		}
		var exprs []expr
		switch p.cmd {
		case "SELECT":
			if p.qual != "" {
				exprs = append(exprs, expr{p.qual, false, "read"})
			}
		case "INSERT":
			if p.check != "" {
				exprs = append(exprs, expr{p.check, true, "write"})
			}
		case "UPDATE", "ALL", "DELETE":
			if p.qual != "" {
				exprs = append(exprs, expr{p.qual, false, "read/write (USING)"})
			}
			check := p.check
			if p.cmd == "ALL" && check == "" {
				check = p.qual
			}
			if check != "" && p.cmd != "DELETE" {
				exprs = append(exprs, expr{check, true, "write (WITH CHECK)"})
			}
		}
		for _, e := range exprs {
			evaluated++
			sat, err := workerSatisfies(rt, p.table, e.text, e.asCheck)
			if err != nil {
				violations = append(violations, fmt.Sprintf("%s.%s [%s]: probe error (blind spot): %v", p.table, p.name, p.cmd, err))
				continue
			}
			if !sat {
				continue
			}
			// Satisfiable by the worker session.
			if p.cmd == "SELECT" && workerReferenceAllowlist[p.table] {
				continue // public reference data
			}
			violations = append(violations, fmt.Sprintf("%s.%s [%s]: the worker session can satisfy this permissive policy (%s) and the table is neither fenced nor on the reference allowlist", p.table, p.name, p.cmd, e.what))
		}
	}
	sort.Strings(violations)
	return violations, evaluated
}

func TestKYCWorkerCatalogueGate_EveryPermissivePolicyIsUnsatisfiableOrFenced_31(t *testing.T) {
	rt := kycRuntimePool(t, 5)
	policies := loadPolicies(t, rt)
	if len(policies) < 300 {
		t.Fatalf("expected the full catalogue (>300 policies), got %d - the enumeration is broken", len(policies))
	}
	fenced := fenceTables(policies)

	// The nine tables are exactly the fenced set (the security-derived list).
	var got []string
	for tbl := range fenced {
		got = append(got, tbl)
	}
	sort.Strings(got)
	want := append([]string(nil), fencedTables...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("fenced tables = %v, want exactly %v", got, want)
	}

	violations, evaluated := gateViolations(t, rt, policies, fenced)
	for _, v := range violations {
		t.Error(v)
	}

	// Code review F6: the policy enumeration above never sees a table with NO
	// policies or with row-level security DISABLED, which the worker identity
	// could read and write in full. Every public table except schema_migrations
	// must have RLS enabled, or be fenced, or be on the reference allowlist.
	var noRLS []string
	var publicTables int
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT c.relname, c.relrowsecurity FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			  WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname <> 'schema_migrations'`) // partitions included: each must carry RLS too (security O-3)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var rls bool
			if err := rows.Scan(&name, &rls); err != nil {
				return err
			}
			publicTables++
			if !rls && !fenced[name] && !workerReferenceAllowlist[name] {
				noRLS = append(noRLS, name)
			}
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("enumerate public tables: %v", err)
	}
	if publicTables < 50 {
		t.Fatalf("expected the full public schema (>50 tables), got %d (the enumeration is broken)", publicTables)
	}
	sort.Strings(noRLS)
	if len(noRLS) != 0 {
		t.Errorf("public table(s) with row-level security DISABLED that are neither fenced nor on the worker reference allowlist: %v", noRLS)
	}
	if evaluated < 150 {
		t.Fatalf("expected to evaluate >150 policy expressions, evaluated %d (the gate is vacuous)", evaluated)
	}

	// Non-vacuity (the gate bites): evaluated WITHOUT the fence exemption, the
	// nine fenced tables' permissive policies ARE satisfiable by the worker
	// session. If the fence were the only thing standing and the probe were
	// blind, this would find nothing.
	unfenced, _ := gateViolations(t, rt, policies, map[string]bool{})
	tablesFlagged := map[string]bool{}
	for _, v := range unfenced {
		tablesFlagged[strings.SplitN(v, ".", 2)[0]] = true
	}
	for _, tbl := range []string{"staff_users", "audit_log", "persons", "risk_rules", "sessions", "login_attempts", "player_restrictions"} {
		if !tablesFlagged[tbl] {
			t.Errorf("non-vacuity: with the fence exemption removed the gate must flag %s (a NULL-arm table); it flagged %v", tbl, keys(tablesFlagged))
		}
	}
}

func keys(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The fences themselves: 36 policies, RESTRICTIVE, literal predicate.
func TestKYCWorkerFencePolicies_ExactShape_52(t *testing.T) {
	rt := kycRuntimePool(t, 5)
	const pred = "(NULLIF(current_setting('app.platform_service_id'::text, true), ''::text) IS DISTINCT FROM 'kyc_submission_worker'::text)"
	type fp struct{ table, cmd, qual, check string }
	var all []fp
	if err := rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx,
			`SELECT tablename, cmd, coalesce(qual, ''), coalesce(with_check, '') FROM pg_policies
			  WHERE schemaname = 'public' AND policyname LIKE 'kyc_worker_fence_%' AND permissive = 'RESTRICTIVE'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var f fp
			if err := rows.Scan(&f.table, &f.cmd, &f.qual, &f.check); err != nil {
				return err
			}
			all = append(all, f)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(all) != 36 {
		t.Fatalf("expected 36 restrictive kyc_worker_fence_* policies, got %d", len(all))
	}
	for _, f := range all {
		switch f.cmd {
		case "SELECT", "DELETE":
			if f.qual != pred || f.check != "" {
				t.Errorf("%s %s: unexpected predicate %q / %q", f.table, f.cmd, f.qual, f.check)
			}
		case "INSERT":
			if f.check != pred || f.qual != "" {
				t.Errorf("%s INSERT: unexpected predicate %q / %q", f.table, f.qual, f.check)
			}
		case "UPDATE":
			if f.qual != pred || f.check != pred {
				t.Errorf("%s UPDATE: unexpected predicate %q / %q", f.table, f.qual, f.check)
			}
		default:
			t.Errorf("%s: unexpected command %s", f.table, f.cmd)
		}
	}
}
