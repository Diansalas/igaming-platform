//go:build integration

package aml_test

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// ADR 0116 s7.1 gate 1 (B1): the catalogue pins. The runtime role has no privilege and no policy on any AML table; every AML
// table is ENABLE + FORCE RLS with exactly ONE policy, for the table OWNER only; the append-only triggers exist; every
// function is owner-owned with a pinned search_path, the officer / enforcement API is SECURITY DEFINER and executable by the
// runtime role only where intended, and no function is executable by PUBLIC.

func catalogQuery(t *testing.T, w *amlWorld, sql string, args ...any) [][]any {
	t.Helper()
	var out [][]any
	if err := w.Owner.WithoutTenant(bg, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, sql, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return err
			}
			out = append(out, vals)
		}
		return rows.Err()
	}); err != nil {
		t.Fatalf("catalogue query: %v", err)
	}
	return out
}

func TestCatalog_AMLTables_RLSPrivilegesPoliciesTriggers(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	rows := catalogQuery(t, w, `SELECT relname FROM pg_class WHERE relnamespace = 'public'::regnamespace AND relkind IN ('r', 'p') AND relname LIKE 'aml\_%' ORDER BY 1`)
	var got []string
	for _, r := range rows {
		got = append(got, r[0].(string))
	}
	want := append([]string(nil), amlTables...)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("AML tables = %v, want exactly %v (a new AML table needs a catalogue decision)", got, want)
	}
	owner := catalogQuery(t, w, `SELECT current_user`)[0][0].(string)
	for _, tbl := range amlTables {
		r := catalogQuery(t, w, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass`, tbl)[0]
		if r[0] != true || r[1] != true {
			t.Errorf("%s: ENABLE+FORCE RLS required, got %v", tbl, r)
		}
		// Zero privileges for the runtime role and for PUBLIC, for every privilege type.
		for _, who := range []string{"igaming_runtime", "public"} {
			for _, priv := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
				ok := catalogQuery(t, w, `SELECT has_table_privilege($1, $2::regclass, $3)`, who, tbl, priv)[0][0]
				if ok != false {
					t.Errorf("%s: %s must NOT hold %s", tbl, who, priv)
				}
			}
			cols := catalogQuery(t, w, `SELECT count(*) FROM information_schema.columns c WHERE c.table_schema='public' AND c.table_name = $1
				AND (has_column_privilege($2, $1::regclass, c.column_name, 'SELECT') OR has_column_privilege($2, $1::regclass, c.column_name, 'UPDATE')
				     OR has_column_privilege($2, $1::regclass, c.column_name, 'INSERT'))`, tbl, who)[0][0]
			if cols.(int64) != 0 {
				t.Errorf("%s: %s holds %v column privilege(s)", tbl, who, cols)
			}
		}
		// Exactly one policy: aml_owner_session, TO the owner only.
		pol := catalogQuery(t, w, `SELECT policyname, roles::text[], cmd, permissive FROM pg_policies WHERE schemaname='public' AND tablename = $1`, tbl)
		if len(pol) != 1 || pol[0][0] != "aml_owner_session" {
			t.Errorf("%s: want exactly one policy aml_owner_session, got %v", tbl, pol)
			continue
		}
		roles := pol[0][1].([]any)
		if len(roles) != 1 || roles[0] != owner {
			t.Errorf("%s: the policy must be TO the owner role only (%s), got %v", tbl, owner, roles)
		}
		if pol[0][2] != "ALL" || pol[0][3] != "PERMISSIVE" {
			t.Errorf("%s: policy shape %v", tbl, pol[0])
		}
		// Append-only triggers: a row trigger for UPDATE OR DELETE and a statement trigger for TRUNCATE, enabled.
		trg := catalogQuery(t, w, `SELECT count(*) FILTER (WHERE (tgtype & 1) = 1 AND (tgtype & 16) <> 0 AND (tgtype & 8) <> 0 AND tgenabled = 'O'),
				count(*) FILTER (WHERE (tgtype & 1) = 0 AND (tgtype & 32) <> 0 AND tgenabled = 'O')
			FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal AND tgfoid = 'aml_deny_mutation'::regproc`, tbl)[0]
		if trg[0].(int64) != 1 || trg[1].(int64) != 1 {
			t.Errorf("%s: want one UPDATE/DELETE row trigger and one TRUNCATE trigger of aml_deny_mutation, got %v", tbl, trg)
		}
		// Every FK to persons / player_accounts / tenants is ON DELETE RESTRICT (C9).
		fk := catalogQuery(t, w, `SELECT conname, confdeltype::text FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'f'`, tbl)
		for _, f := range fk {
			if f[1].(string) != "r" {
				t.Errorf("%s: foreign key %v is not ON DELETE RESTRICT", tbl, f)
			}
		}
	}
}

func TestCatalog_AMLFunctions_DefinerSearchPathAndPrivileges(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	apiRuntime := map[string]bool{ // executable by the runtime role, SECURITY DEFINER
		"aml_hold_active": true, "aml_hold_match": true, "aml_record_enforcement_decision": true, "aml_hold_place": true,
		"aml_hold_lift": true, "aml_hold_list": true, "aml_alert_raise": true, "aml_case_open": true, "aml_case_transition": true,
		"aml_case_add_note": true, "aml_case_flag_sar": true, "aml_case_propose": true, "aml_case_approve": true, "aml_case_read": true,
		"aml_case_events_list": true, "aml_case_notes_list": true, "aml_case_queue": true, "aml_person_has_records": true,
	}
	owner := catalogQuery(t, w, `SELECT pg_get_userbyid(c.relowner) FROM pg_class c WHERE c.oid = 'public.aml_holds'::regclass`)[0][0].(string)
	rows := catalogQuery(t, w, `SELECT p.oid::regprocedure::text, p.proname, p.prosecdef, coalesce(array_to_string(p.proconfig, ','), ''), pg_get_userbyid(p.proowner),
			has_function_privilege('igaming_runtime', p.oid, 'EXECUTE'), has_function_privilege('public', p.oid, 'EXECUTE')
		FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace AND p.proname LIKE 'aml\_%' ORDER BY 2`)
	if len(rows) < 30 {
		t.Fatalf("expected the full AML function set, got %d", len(rows))
	}
	seenAPI := map[string]bool{}
	for _, r := range rows {
		sig, name, secdef, cfg, powner, rt, pub := r[0].(string), r[1].(string), r[2].(bool), r[3].(string), r[4].(string), r[5].(bool), r[6].(bool)
		if powner != owner {
			t.Errorf("%s: owned by %s, want the table owner %s", sig, powner, owner)
		}
		if !strings.Contains(cfg, "search_path=pg_catalog, public, pg_temp") {
			t.Errorf("%s: search_path is not pinned (%q)", sig, cfg)
		}
		if pub {
			t.Errorf("%s: executable by PUBLIC", sig)
		}
		if apiRuntime[name] {
			seenAPI[name] = true
			if !secdef || !rt {
				t.Errorf("%s: API functions must be SECURITY DEFINER and executable by the runtime role (definer=%v runtime=%v)", sig, secdef, rt)
			}
			continue
		}
		if secdef {
			t.Errorf("%s: only the API functions may be SECURITY DEFINER", sig)
		}
		if rt {
			t.Errorf("%s: a helper / guard / trigger function must NOT be executable by the runtime role", sig)
		}
	}
	for name := range apiRuntime {
		if !seenAPI[name] {
			t.Errorf("API function %s is missing", name)
		}
	}
	// aml_authorize and the quorum helpers are not callable by the runtime role (the proof consumption cannot be probed).
	for _, fn := range []string{"aml_authorize(uuid, text, text, text)", "aml_officer_quorum(uuid, text)", "aml_assert_quorum(uuid, text)"} {
		if rt := catalogQuery(t, w, `SELECT has_function_privilege('igaming_runtime', $1::regprocedure, 'EXECUTE')`, fn)[0][0]; rt != false {
			t.Errorf("%s must not be executable by the runtime role", fn)
		}
	}
}

// Migration-text pins: every AML SECURITY DEFINER function calls aml_authorize (or is one of the three session-tenant-bound
// enforcement / erasure reads) BEFORE touching a table; the AM class is used only by 0140-0142.
func TestCatalog_EveryOfficerFunctionAuthorisesFirst(t *testing.T) {
	w := newWorld(t, "under_platform_licence")
	rows := catalogQuery(t, w, `SELECT p.proname, p.prosrc FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace AND p.proname LIKE 'aml\_%' AND p.prosecdef`)
	noProof := map[string]bool{"aml_hold_active": true, "aml_hold_match": true, "aml_record_enforcement_decision": true, "aml_person_has_records": true}
	for _, r := range rows {
		name, src := r[0].(string), r[1].(string)
		if noProof[name] {
			if !strings.Contains(src, "aml_session_tenant()") {
				t.Errorf("%s: a proof-less read must be bound to the session tenant", name)
			}
			if strings.Contains(src, "aml_authorize") {
				t.Errorf("%s: unexpected authorize in a gate read", name)
			}
			continue
		}
		idx := strings.Index(src, "aml_authorize(")
		if idx < 0 {
			t.Errorf("%s: a SECURITY DEFINER officer function must call aml_authorize", name)
			continue
		}
		before := src[:idx]
		for _, forbidden := range []string{"INSERT INTO", "UPDATE ", "DELETE FROM", "FROM public.aml_", "PERFORM public.aml_assert"} {
			if strings.Contains(before, forbidden) {
				t.Errorf("%s: %q appears before aml_authorize", name, forbidden)
			}
		}
	}
}
