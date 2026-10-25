//go:build integration

package aml_test

import (
	"context"
	"regexp"
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
				count(*) FILTER (WHERE (tgtype & 1) = 0 AND (tgtype & 32) <> 0 AND tgenabled = 'O'),
				count(*) FILTER (WHERE (tgtype & 1) = 1 AND (tgtype & 16) = 0 AND (tgtype & 8) <> 0 AND tgenabled = 'O')
			FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal AND tgfoid = 'aml_deny_mutation'::regproc`, tbl)[0]
		if amlStateMachineTables[tbl] {
			// Migration 0181: a guarded state machine: DELETE and TRUNCATE are denied, UPDATE goes through the guard trigger.
			if trg[0].(int64) != 0 || trg[1].(int64) != 1 || trg[2].(int64) != 1 {
				t.Errorf("%s: want a DELETE-only row trigger and a TRUNCATE trigger of aml_deny_mutation (no UPDATE denial), got %v", tbl, trg)
			}
			g := catalogQuery(t, w, `SELECT count(*) FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal AND tgenabled = 'O'
				AND (tgtype & 16) <> 0 AND (tgtype & 4) <> 0 AND tgfoid::regproc::text = $2`, tbl, tbl+"_guard")[0][0]
			if g.(int64) != 1 {
				t.Errorf("%s: want exactly one enabled INSERT OR UPDATE guard trigger %s_guard", tbl, tbl)
			}
		} else if trg[0].(int64) != 1 || trg[1].(int64) != 1 {
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
		"aml_case_events_list": true, "aml_case_notes_list": true, "aml_case_queue": true,
		// migration 0154 (A7): screening policy / record / list and the launch readiness reader.
		"aml_screening_policy_author": true, "aml_screening_policy_effective": true, "aml_screening_record": true, "aml_screening_list": true,
		"aml_readiness": true,
		// migration 0181 (lic10, G1): the trigger policy, the officer request read and the worker API.
		"aml_screening_trigger_policy_author": true, "aml_screening_trigger_policy_effective": true, "aml_screening_request_list": true,
		"aml_screening_enqueue_due": true, "aml_screening_request_claim": true, "aml_screening_request_finish": true,
		// lic18: the trapped-person count (a count only, session-tenant bound). lic23: the officer replace (authorises first).
		"aml_screening_gate_overdue": true, "aml_screening_request_replace": true, "aml_screening_parked_summary": true, "aml_screening_parked_overdue": true,
	}
	// SECURITY DEFINER but NOT executable by the runtime role: the erasure check (until the HD-RET-1 job is its caller, security F5)
	// and the platform-grantee liveness helper (reachable only through the owner-run definer chain, security F1/F3).
	// d1 (migration 0191, D7): the loosening-capable break-glass setter aml_screening_policy_set is OWNER-ONLY (pin moved here from apiRuntime); d1 D4 (0191): so is aml_screening_trigger_policy_set (the runtime route is the tighten-only aml_screening_trigger_policy_author).
	definerNoRuntime := map[string]bool{"aml_person_has_records": true, "aml_platform_grantee_live": true, "aml_screening_policy_set": true, "aml_screening_trigger_policy_set": true}
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
		if definerNoRuntime[name] {
			if !secdef || rt {
				t.Errorf("%s: must be SECURITY DEFINER and NOT executable by the runtime role (definer=%v runtime=%v)", sig, secdef, rt)
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
	// 0154: the policy and readiness readers are proof-less, session-tenant-bound reads (no result, hold, case or person is disclosed).
	noProof := map[string]bool{"aml_hold_active": true, "aml_hold_match": true, "aml_record_enforcement_decision": true, "aml_person_has_records": true,
		"aml_screening_policy_effective": true, "aml_readiness": true,
		// migration 0181: the trigger policy reader and the worker API are proof-less, session-tenant-bound (the worker is not an officer).
		"aml_screening_trigger_policy_effective": true, "aml_screening_enqueue_due": true, "aml_screening_request_claim": true,
		"aml_screening_request_finish": true,
		// lic18: the trapped-person count (a count only, session-tenant bound, proof-less like the worker API).
		"aml_screening_gate_overdue": true, "aml_screening_parked_summary": true, "aml_screening_parked_overdue": true}
	for _, r := range rows {
		name, src := r[0].(string), r[1].(string)
		if name == "aml_platform_grantee_live" {
			continue // the pinned liveness helper reads one platform staff row; it has no tenant argument
		}
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

// ADR 0116 s2.1 / s5.1 (C6, LF catalogue test): every ledger transaction type is classified as GATED (new value entering play or
// leaving the platform; the gate lives in the named place) or NEVER GATED (settlement / correction / release; stated reason).
// The test reads the LIVE transaction_type CHECK: a future type (a refund, ADR 0007 conversion) FAILS until it is classified
// and ledger-finance signs it off.
func TestCatalog_EveryLedgerTransactionTypeIsClassified(t *testing.T) {
	gated := map[string]string{
		"deposit":              "initiation gate (payments/kycgate.go); the SETTLEMENT posting of the same type is not gated",
		"withdrawal_requested": "withdrawal request (withdrawal/withdrawal.go); the payout claim is the second gate (payments/payout.go)",
		"casino_bet":           "new casino stake (casino/orchestrator.go)",
		"sportsbook_bet":       "new sportsbook stake (sportsbook/orchestrator.go)",
		"manual_adjustment":    "refused for a held player at submission and execution (adjustment/aml_hold.go)",
	}
	neverGated := map[string]string{
		"deposit_reversal":      "a reversal of a settled deposit returns value; never blocked",
		"withdrawal_completed":  "an in-flight payout completes; never recalled",
		"withdrawal_rejected":   "a release returning held funds to player_cash (also the DenyForCompliance outcome)",
		"withdrawal_failed":     "a release",
		"withdrawal_reversed":   "a reversal",
		"tombstone":             "a replay-protection marker moving no value",
		"casino_win":            "settlement",
		"casino_rollback":       "terminal stake return (Q-GP-5)",
		"bonus_grant":           "value created by the platform, not entering play from the player",
		"bonus_conversion":      "listed non-gated by ADR 0116 s5.1 (HD-KYC-4 / ADR 0007 conversion gates are separate owner questions)",
		"bonus_forfeiture":      "removal of bonus value",
		"bonus_reversal":        "a reversal",
		"sportsbook_settlement": "settlement",
		"sportsbook_void":       "terminal stake return",
		"sportsbook_rollback":   "a settlement rollback",
	}
	w := newWorld(t, "under_platform_licence")
	def := catalogQuery(t, w, `SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = 'ledger_transactions'::regclass AND conname LIKE '%transaction_type%'`)
	if len(def) != 1 {
		t.Fatalf("the transaction_type CHECK is not found: %v", def)
	}
	live := map[string]bool{}
	for _, m := range regexp.MustCompile(`'([a-z_]+)'::text`).FindAllStringSubmatch(def[0][0].(string), -1) {
		live[m[1]] = true
	}
	if len(live) < 15 {
		t.Fatalf("the live CHECK parse is broken: %v", live)
	}
	for typ := range live {
		_, g := gated[typ]
		_, n := neverGated[typ]
		switch {
		case g && n:
			t.Errorf("%s is classified both ways", typ)
		case !g && !n:
			t.Errorf("ledger transaction type %q is NOT classified for the AML hold (gated or never-gated): classify it and obtain ledger-finance sign-off", typ)
		}
	}
	for typ := range gated {
		if !live[typ] {
			t.Errorf("classified type %q no longer exists", typ)
		}
	}
	for typ := range neverGated {
		if !live[typ] {
			t.Errorf("classified type %q no longer exists", typ)
		}
	}
}
