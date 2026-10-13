//go:build integration

// RLS, restrictive-fence parity, ACL and catalog tests of migration 0137 (ADR 0119 sections 13.3-13.5), every
// assertion as igaming_runtime, plus the GUC-forging adversarial suite (threat M5: arbitrary SQL as the runtime
// role). Where a forged GUC DOES widen visibility (a tenant GUC is, by design, trusted by RLS: the ADR 0119
// section 3 M5 row, and the same on staff_users itself), the test pins that fact AND proves that what the
// attacker then sees or plants is useless without the keys.
package store

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

var staffTables = []string{"staff_mfa_factors", "staff_mfa_recovery_codes", "staff_auth_challenges", "staff_enrolment_tokens"}

// seedAll gives every one of a1, b1 and platform a row in every staff MFA table.
func (w *world) seedAll() {
	w.t.Helper()
	for _, ref := range []staffmfa.StaffRef{w.a1, w.b1, w.platform} {
		w.enrol(ref, t0)
		w.issueCodes(ref, t0)
		w.issueChallenge(ref, ChallengeMFA, OriginLogin, t0)
		w.issueToken(ref, EnrolInvite, t0)
	}
}

// guc is a full set of session settings (an empty value is set to the empty string, which RLS reads as unset).
type guc map[string]string

// rtGUC runs fn in ONE runtime transaction with exactly the given session settings.
func (w *world) rtGUC(g guc, fn func(ctx context.Context, tx pgx.Tx) error) error {
	ctx := context.Background()
	tx, err := w.rt.Raw().Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	for k, v := range g {
		if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, k, v); err != nil {
			return err
		}
	}
	return fn(ctx, tx)
}

// visibleStaff returns which of the three seeded staff members have a row in table under g.
func (w *world) visibleStaff(g guc, table string) map[uuid.UUID]bool {
	w.t.Helper()
	out := map[uuid.UUID]bool{}
	err := w.rtGUC(g, func(ctx context.Context, tx pgx.Tx) error {
		col := "staff_user_id"
		if table == "staff_users" {
			col = "id"
		}
		rows, err := tx.Query(ctx, `SELECT DISTINCT `+col+` FROM `+table+` WHERE `+col+` = ANY($1)`,
			[]uuid.UUID{w.a1.StaffID, w.b1.StaffID, w.platform.StaffID})
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id uuid.UUID
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out[id] = true
		}
		return rows.Err()
	})
	if err != nil {
		w.t.Fatalf("%s under %v: %v", table, g, err)
	}
	return out
}

func idset(ids ...uuid.UUID) map[uuid.UUID]bool {
	m := map[uuid.UUID]bool{}
	for _, id := range ids {
		m[id] = true
	}
	return m
}

func sameSet(a, b map[uuid.UUID]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

func TestRLS_DualScope_EveryTable(t *testing.T) {
	w := newWorld(t)
	w.seedAll()
	A, B, P := w.a1.StaffID, w.b1.StaffID, w.platform.StaffID
	for _, table := range staffTables {
		for name, c := range map[string]struct {
			g    guc
			want map[uuid.UUID]bool
		}{
			"tenant A":          {guc{"app.tenant_id": w.tenantA.String()}, idset(A)},
			"tenant B":          {guc{"app.tenant_id": w.tenantB.String()}, idset(B)},
			"platform (unset)":  {guc{}, idset(P)},
			"platform (empty)":  {guc{"app.tenant_id": ""}, idset(P)},
			"unknown tenant":    {guc{"app.tenant_id": uuid.NewString()}, idset()},
			"principal only":    {guc{"app.principal_id": A.String()}, idset(P)}, // no tenant: platform arm only
			"player GUC, A":     {guc{"app.tenant_id": w.tenantA.String(), "app.player_account_id": uuid.NewString()}, idset()},
			"platform admin id": {guc{"app.platform_admin_principal_id": P.String()}, idset(P)},
		} {
			if got := w.visibleStaff(c.g, table); !sameSet(got, c.want) {
				t.Fatalf("%s / %s: visible staff %v, want %v", table, name, got, c.want)
			}
		}
	}
}

func TestRLS_CrossTenantWritesAreInvisible(t *testing.T) {
	w := newWorld(t)
	w.seedAll()
	// Tenant B's session cannot update, consume or revoke tenant A's rows: 0 rows affected, nothing changes.
	for table, set := range map[string]string{
		"staff_mfa_factors":        `failed_24h_count = 1, failed_24h_start = now()`,
		"staff_mfa_recovery_codes": `revoked_at = now()`,
		"staff_auth_challenges":    `consumed_at = now()`,
		"staff_enrolment_tokens":   `revoked_at = now()`,
	} {
		err := w.run(w.b1, func(ctx context.Context, tx pgx.Tx) error {
			tag, e := tx.Exec(ctx, `UPDATE `+table+` SET `+set+` WHERE staff_user_id = $1`, w.a1.StaffID)
			if e == nil && tag.RowsAffected() != 0 {
				t.Fatalf("%s: tenant B updated %d of tenant A's rows", table, tag.RowsAffected())
			}
			return e
		})
		wantOK(t, table+" cross-tenant update", err)
		// A platform session cannot write a tenant staff member's row either.
		err = w.run(w.platform, func(ctx context.Context, tx pgx.Tx) error {
			tag, e := tx.Exec(ctx, `UPDATE `+table+` SET `+set+` WHERE staff_user_id = $1`, w.a1.StaffID)
			if e == nil && tag.RowsAffected() != 0 {
				t.Fatalf("%s: platform session updated %d tenant rows", table, tag.RowsAffected())
			}
			return e
		})
		wantOK(t, table+" platform cross-scope update", err)
	}
	// FOR UPDATE across tenants locks nothing.
	err := w.run(w.b1, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		rows, e := tx.Query(ctx, `SELECT 1 FROM staff_mfa_factors WHERE staff_user_id = $1 FOR UPDATE`, w.a1.StaffID)
		if e != nil {
			return e
		}
		for rows.Next() {
			n++
		}
		rows.Close()
		if n != 0 {
			t.Fatalf("FOR UPDATE reached %d rows of another tenant", n)
		}
		return rows.Err()
	})
	wantOK(t, "cross-tenant FOR UPDATE", err)
}

// The restrictive fences: an acting session (financial_acting_gucs_present) and the KYC submission worker reach
// NO staff MFA material, for SELECT, INSERT and UPDATE, whatever else they set.
func TestRLS_RestrictiveFences_NoActingNoWorkerAccess(t *testing.T) {
	w := newWorld(t)
	w.seedAll()
	fenced := map[string]guc{
		"acting tenant + principal, tenant scope": {"app.tenant_id": w.tenantA.String(), "app.acting_tenant_id": w.tenantA.String(), "app.acting_platform_principal_id": w.platform.StaffID.String()},
		"acting tenant only":                      {"app.tenant_id": w.tenantA.String(), "app.acting_tenant_id": w.tenantA.String()},
		"acting principal only":                   {"app.tenant_id": w.tenantA.String(), "app.acting_platform_principal_id": w.platform.StaffID.String()},
		"acting, platform scope":                  {"app.acting_platform_principal_id": w.platform.StaffID.String()},
		"kyc worker, tenant scope":                {"app.tenant_id": w.tenantA.String(), "app.platform_service_id": "kyc_submission_worker"},
		"kyc worker, platform scope":              {"app.platform_service_id": "kyc_submission_worker"},
	}
	open := map[string]guc{"tenant A, unfenced": {"app.tenant_id": w.tenantA.String()}}
	// D14: player-scoped and EVERY platform-service session are fenced, not only the KYC worker.
	fenced["player scope, tenant"] = guc{"app.tenant_id": w.tenantA.String(), "app.player_account_id": uuid.NewString()}
	fenced["player scope, no tenant"] = guc{"app.player_account_id": uuid.NewString()}
	for _, svc := range []string{"alert_dispatcher", "sportsbook_catalogue_sync", "kyc_submission_worker", "any_future_service"} {
		fenced["service "+svc+", tenant"] = guc{"app.tenant_id": w.tenantA.String(), "app.platform_service_id": svc}
		fenced["service "+svc+", platform"] = guc{"app.platform_service_id": svc}
	}
	for _, table := range staffTables {
		for name, g := range open {
			if got := w.visibleStaff(g, table); !got[w.a1.StaffID] {
				t.Fatalf("%s / %s: the control session must see the row (otherwise the fence test is vacuous)", table, name)
			}
		}
		for name, g := range fenced {
			if got := w.visibleStaff(g, table); len(got) != 0 {
				t.Fatalf("%s / %s: a fenced session saw %v", table, name, got)
			}
		}
	}
	// Writes: INSERT is refused by the fence's WITH CHECK, UPDATE touches nothing.
	for name, g := range fenced {
		for table, q := range insertSQL {
			if g["app.tenant_id"] == "" {
				continue // an INSERT needs a tenant arm to pass the dual-scope policy for a tenant row
			}
			err := w.rtGUC(g, func(ctx context.Context, tx pgx.Tx) error {
				_, e := tx.Exec(ctx, q, nil, w.a2.StaffID, randMAC())
				return e
			})
			if pgCode(err) != "42501" && pgCode(err) != "SF001" {
				t.Fatalf("%s / %s: a fenced INSERT must be refused, got %v", table, name, err)
			}
		}
		err := w.rtGUC(g, func(ctx context.Context, tx pgx.Tx) error {
			tag, e := tx.Exec(ctx, `UPDATE staff_mfa_factors SET failed_24h_count = 1, failed_24h_start = now() WHERE staff_user_id = $1`, w.a1.StaffID)
			if e == nil && tag.RowsAffected() != 0 {
				t.Fatalf("%s: a fenced UPDATE touched %d rows", name, tag.RowsAffected())
			}
			return e
		})
		wantOK(t, name+" update", err)
	}
}

// Parity with staff_users: in every non-fenced GUC situation the new tables expose exactly the staff members
// staff_users exposes; in the fenced ones they expose nothing (staff_users deliberately admits the acting
// principal's own row and the acted tenant's staff rows; MFA material has no such need).
func TestRLS_VisibilityParityWithStaffUsers(t *testing.T) {
	w := newWorld(t)
	w.seedAll()
	A, B, P := w.a1.StaffID.String(), w.b1.StaffID.String(), w.platform.StaffID.String()
	combos := map[string]struct {
		g      guc
		fenced bool
	}{
		"none":                      {guc{}, false},
		"tenant A":                  {guc{"app.tenant_id": w.tenantA.String()}, false},
		"tenant B":                  {guc{"app.tenant_id": w.tenantB.String()}, false},
		"tenant A + principal":      {guc{"app.tenant_id": w.tenantA.String(), "app.principal_id": A}, false},
		"platform admin principal":  {guc{"app.platform_admin_principal_id": P}, false},
		"tenant A + player":         {guc{"app.tenant_id": w.tenantA.String(), "app.player_account_id": uuid.NewString()}, true},
		"alert dispatcher":          {guc{"app.tenant_id": w.tenantA.String(), "app.platform_service_id": "alert_dispatcher"}, true},
		"session lookup hash":       {guc{"app.session_lookup_hash": "x"}, false},
		"credential lookup hash":    {guc{"app.credential_token_lookup_hash": "x"}, false},
		"acting full":               {guc{"app.tenant_id": w.tenantA.String(), "app.acting_tenant_id": w.tenantA.String(), "app.acting_platform_principal_id": P}, true},
		"acting platform principal": {guc{"app.acting_platform_principal_id": P}, true},
		"acting tenant only":        {guc{"app.acting_tenant_id": w.tenantB.String()}, true},
		"kyc worker":                {guc{"app.tenant_id": w.tenantA.String(), "app.platform_service_id": "kyc_submission_worker"}, true},
	}
	_ = B
	for name, c := range combos {
		su := w.visibleStaff(c.g, "staff_users")
		for _, table := range staffTables {
			got := w.visibleStaff(c.g, table)
			if c.fenced {
				if len(got) != 0 {
					t.Fatalf("%s / %s: fenced session saw %v", name, table, got)
				}
				continue
			}
			if !sameSet(got, su) {
				t.Fatalf("%s / %s: visibility %v differs from staff_users %v", name, table, got, su)
			}
		}
	}
}

// Catalog parity: every RESTRICTIVE policy on staff_users has a counterpart (same name, same command, same GUC
// family) on each new table, so a fence added to staff_users later cannot be silently missing (review N3).
func TestCatalog_RestrictiveFenceParity(t *testing.T) {
	w := newWorld(t)
	type pol struct{ name, cmd, qual, check string }
	read := func(table string) map[string]pol {
		out := map[string]pol{}
		w.mustRun(w.platform, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, `SELECT policyname, cmd, coalesce(qual, ''), coalesce(with_check, '') FROM pg_policies
				WHERE schemaname = 'public' AND tablename = $1 AND permissive = 'RESTRICTIVE'`, table)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var p pol
				if err := rows.Scan(&p.name, &p.cmd, &p.qual, &p.check); err != nil {
					return err
				}
				out[p.name+"/"+p.cmd] = p
			}
			return rows.Err()
		})
		return out
	}
	base := read("staff_users")
	if len(base) != 8 {
		t.Fatalf("staff_users has %d restrictive policies; this test pins 8 (0112 acting fence x4, 0114 KYC fence x4) - extend migration 0137 AND this test", len(base))
	}
	for _, table := range staffTables {
		got := read(table)
		// Superset (D14): every staff_users fence plus exactly the two extra named fences, pinned explicitly.
		extra := map[string]bool{"player_scope_fence/ALL": true, "platform_service_fence/ALL": true}
		if len(got) != len(base)+len(extra) {
			t.Fatalf("%s has %d restrictive policies, want staff_users' %d + %d extra", table, len(got), len(base), len(extra))
		}
		for k := range extra {
			if _, ok := got[k]; !ok {
				t.Fatalf("%s lacks the extra fence %s", table, k)
			}
		}
		for key, b := range base {
			g, ok := got[key]
			if !ok {
				t.Fatalf("%s lacks the counterpart of staff_users' restrictive policy %s", table, key)
			}
			// Same GUC family: a fence on the KYC worker GUC stays on that GUC; an acting fence on the acting predicate.
			for _, needle := range []string{"kyc_submission_worker", "financial_acting_gucs_present", "platform_service_id"} {
				if strings.Contains(b.qual+b.check, needle) != strings.Contains(g.qual+g.check, needle) {
					t.Fatalf("%s / %s: fence family differs on %q:\n staff_users: %s %s\n %s: %s %s", table, key, needle, b.qual, b.check, table, g.qual, g.check)
				}
			}
			if strings.HasPrefix(b.name, "kyc_worker_fence") && (b.qual != g.qual || b.check != g.check) {
				t.Fatalf("%s / %s: the KYC worker fence must be identical to staff_users'", table, key)
			}
		}
	}
}

func TestCatalog_PoliciesFlagsTriggersFunctions(t *testing.T) {
	w := newWorld(t)
	q := func(sql string, args ...any) (out []string) {
		w.mustRun(w.platform, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			defer rows.Close()
			for rows.Next() {
				var s string
				if err := rows.Scan(&s); err != nil {
					return err
				}
				out = append(out, s)
			}
			return rows.Err()
		})
		sort.Strings(out)
		return out
	}
	eq := func(what string, got, want []string) {
		t.Helper()
		sort.Strings(want)
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Fatalf("%s:\n got  %v\n want %v", what, got, want)
		}
	}
	for _, table := range staffTables {
		// ENABLE + FORCE row level security.
		got := q(`SELECT relrowsecurity::text || ':' || relforcerowsecurity::text FROM pg_class WHERE oid = ('public.' || $1)::regclass`, table)
		eq(table+" RLS flags", got, []string{"true:true"})
		// The permissive policy set: dual scope (FOR ALL) plus the lookup policy where the table has one.
		want := []string{"dual_scope_isolation/ALL"}
		switch table {
		case "staff_auth_challenges":
			want = append(want, "staff_challenge_select_by_mac/SELECT")
		case "staff_enrolment_tokens":
			want = append(want, "staff_enrolment_select_by_mac/SELECT")
		}
		eq(table+" permissive policies", q(`SELECT policyname || '/' || cmd FROM pg_policies WHERE schemaname = 'public' AND tablename = $1 AND permissive = 'PERMISSIVE'`, table), want)
		// The dual-scope policy is the staff_users one, verbatim.
		var base, mine []string
		base = q(`SELECT coalesce(qual, '') || '|' || coalesce(with_check, '') FROM pg_policies WHERE schemaname = 'public' AND tablename = 'staff_users' AND policyname = 'dual_scope_isolation'`)
		mine = q(`SELECT coalesce(qual, '') || '|' || coalesce(with_check, '') FROM pg_policies WHERE schemaname = 'public' AND tablename = $1 AND policyname = 'dual_scope_isolation'`, table)
		eq(table+" dual scope policy == staff_users'", mine, base)
	}

	// Triggers: exactly the expected BEFORE triggers, enabled, with the ADR 0119 names; the SF guards fire in name order.
	trig := func(table string) []string {
		return q(`SELECT t.tgname || ':' || t.tgenabled::text || ':' || ((t.tgtype & 2) <> 0)::text || ':' || ((t.tgtype & 1) <> 0)::text || ':' ||
				((t.tgtype & 4) <> 0)::text || (((t.tgtype & 8) <> 0)::text) || ((t.tgtype & 16) <> 0)::text || ((t.tgtype & 32) <> 0)::text
			FROM pg_trigger t WHERE t.tgrelid = ('public.' || $1)::regclass AND NOT t.tgisinternal`, table)
	}
	// name : enabled : BEFORE : ROW : INSERT DELETE UPDATE TRUNCATE flags (insert, delete, update, truncate)
	eq("factor triggers", trig("staff_mfa_factors"), []string{
		"sf_factor_tenant_forced:O:true:true:truefalsefalsefalse", "sf_factor_guard:O:true:true:falsefalsetruefalse",
		"sf_no_delete:O:true:true:falsetruefalsefalse", "sf_no_truncate:O:true:false:falsefalsefalsetrue"})
	eq("recovery triggers", trig("staff_mfa_recovery_codes"), []string{
		"sf_recovery_tenant_forced:O:true:true:truefalsefalsefalse", "sf_recovery_guard:O:true:true:falsefalsetruefalse",
		"sf_no_delete:O:true:true:falsetruefalsefalse", "sf_no_truncate:O:true:false:falsefalsefalsetrue"})
	eq("challenge triggers", trig("staff_auth_challenges"), []string{
		"sf_challenge_tenant_forced:O:true:true:truefalsefalsefalse", "sf_challenge_guard:O:true:true:falsefalsetruefalse",
		"sf_no_delete:O:true:true:falsetruefalsefalse", "sf_no_truncate:O:true:false:falsefalsefalsetrue"})
	eq("token triggers", trig("staff_enrolment_tokens"), []string{
		"sf_enrolment_token_tenant_forced:O:true:true:truefalsefalsefalse", "sf_enrolment_token_guard:O:true:true:falsefalsetruefalse",
		"sf_no_delete:O:true:true:falsetruefalsefalse", "sf_no_truncate:O:true:false:falsefalsefalsetrue"})

	// Functions: plain plpgsql, NOT SECURITY DEFINER, search_path pinned, no PUBLIC-granted extras.
	fn := q(`SELECT p.proname || ':' || l.lanname || ':secdef=' || p.prosecdef::text || ':vol=' || p.provolatile::text || ':cfg=' || coalesce(array_to_string(p.proconfig, ','), '')
		FROM pg_proc p JOIN pg_language l ON l.oid = p.prolang
		WHERE p.pronamespace = 'public'::regnamespace AND p.proname LIKE 'sf\_%'`)
	wantFn := []string{}
	for _, n := range []string{"sf_force_tenant_from_staff", "sf_deny_delete", "sf_deny_truncate", "sf_factor_guard", "sf_recovery_guard", "sf_challenge_guard", "sf_enrolment_token_guard"} {
		wantFn = append(wantFn, n+":plpgsql:secdef=false:vol=v:cfg=search_path=pg_catalog, public, pg_temp")
	}
	eq("sf_* functions", fn, wantFn)

	// No function of any kind that raises an SF020/SF021 or SF030/SF040 exists yet: they belong to migrations
	// 0139 (staff_users), 0138 (sessions) and S-H1. When 0139 lands, flip this into the ADR 0119 s13.5 pin
	// "the SF020/SF021 trigger function is VOLATILE".
	eq("later-slice SF codes not created in S2", q(`SELECT proname FROM pg_proc WHERE pronamespace = 'public'::regnamespace
		AND (prosrc LIKE '%SF010%' OR prosrc LIKE '%SF020%' OR prosrc LIKE '%SF021%' OR prosrc LIKE '%SF030%' OR prosrc LIKE '%SF040%' OR prosrc LIKE '%SF007%')`), nil)
	// Every function that raises an SF code is non-SECURITY DEFINER with a pinned search_path (ADR 0108).
	eq("SF-raising functions are plain and pinned", q(`SELECT proname FROM pg_proc WHERE pronamespace = 'public'::regnamespace AND prosrc LIKE '%ERRCODE = ''SF%'
		AND (prosecdef OR proconfig IS NULL OR NOT (array_to_string(proconfig, ',') LIKE 'search_path=%'))`), nil)
}

func TestGrants_RuntimeMatrix(t *testing.T) {
	w := newWorld(t)
	updatable := map[string][]string{
		"staff_mfa_factors": {"activated_at", "failed_24h_count", "failed_24h_start", "failed_count", "failed_window_start", "hard_locked_at",
			"last_used_step", "locked_until", "pending_expires_at", "revoked_at", "revoked_reason", "status"},
		"staff_mfa_recovery_codes": {"consumed_at", "revoked_at"},
		"staff_auth_challenges":    {"attempts_left", "consumed_at"},
		"staff_enrolment_tokens":   {"consumed_at", "revoked_at"},
	}
	for _, table := range staffTables {
		var priv map[string]bool
		var cols []string
		allCols := map[string]bool{}
		w.mustRun(w.platform, func(ctx context.Context, tx pgx.Tx) error {
			priv = map[string]bool{}
			for _, p := range []string{"SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER"} {
				var ok bool
				if err := tx.QueryRow(ctx, `SELECT has_table_privilege(current_user, 'public.'||$1, $2)`, table, p).Scan(&ok); err != nil {
					return err
				}
				priv[p] = ok
			}
			rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $1`, table)
			if err != nil {
				return err
			}
			for rows.Next() {
				var c string
				if err := rows.Scan(&c); err != nil {
					rows.Close()
					return err
				}
				allCols[c] = true
			}
			rows.Close()
			for c := range allCols {
				var ok bool
				if err := tx.QueryRow(ctx, `SELECT has_column_privilege(current_user, 'public.'||$1, $2, 'UPDATE')`, table, c).Scan(&ok); err != nil {
					return err
				}
				if ok {
					cols = append(cols, c)
				}
			}
			return nil
		})
		// has_table_privilege(UPDATE) is true when ANY column is updatable at table level only; column-level grants
		// make it false. SELECT and INSERT are table level.
		if !priv["SELECT"] || !priv["INSERT"] || priv["UPDATE"] || priv["DELETE"] || priv["TRUNCATE"] || priv["REFERENCES"] || priv["TRIGGER"] {
			t.Fatalf("%s: runtime table privileges = %v (want SELECT, INSERT only)", table, priv)
		}
		sort.Strings(cols)
		want := append([]string(nil), updatable[table]...)
		sort.Strings(want)
		if strings.Join(cols, ",") != strings.Join(want, ",") {
			t.Fatalf("%s: runtime UPDATE columns = %v, want %v", table, cols, want)
		}
	}
	// PUBLIC holds nothing; the table ACL has no other grantee than the owner and the runtime role.
	for _, table := range staffTables {
		var acl string
		w.mustRun(w.platform, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT coalesce(relacl::text, '') FROM pg_class WHERE oid = ('public.'||$1)::regclass`, table).Scan(&acl)
		})
		if strings.Contains(acl, "=r") || strings.HasPrefix(strings.TrimPrefix(acl, "{"), "=") || strings.Contains(acl, ",=") {
			t.Fatalf("%s: PUBLIC holds a privilege: %s", table, acl)
		}
	}
}

// ---- adversarial suite: arbitrary SQL as igaming_runtime (threat M5) ---------------------------------------

func hexOf(b []byte) string { return hex.EncodeToString(b) }

// attackerCode is the code of a secret the attacker generated (they know it; the library keeps it opaque, so the
// test goes through the enrolment material that reveals it).
func attackerCode(t testing.TB, secret staffmfa.FactorSecret, at time.Time) string {
	t.Helper()
	m, err := staffmfa.NewTOTPEnrolmentMaterial("attacker", "attacker@example.invalid", secret)
	if err != nil {
		t.Fatal(err)
	}
	return codeAt(t, secretFrom(t, m), at)
}

// Residual, pinned (ADR 0119 s3 M5 / s12 R2): a runtime session that FORGES the tenant GUC reads another
// tenant's rows, exactly as it reads staff_users. What it reads is ciphertext and keyed MACs.
func TestAdversarial_Residual_ForgedTenantGUCReadsCiphertextOnly(t *testing.T) {
	w := newWorld(t)
	w.seedAll()
	got := w.visibleStaff(guc{"app.tenant_id": w.tenantB.String()}, "staff_mfa_factors")
	if !got[w.b1.StaffID] {
		t.Fatal("residual pin: a forged tenant GUC is trusted by RLS (as on staff_users); if this changes, update the ADR section 12 residual text")
	}
	// ...but the data is useless: no plaintext secret, and the MAC is not a usable credential.
	var ct, mac []byte
	_ = w.rtGUC(guc{"app.tenant_id": w.tenantB.String()}, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT secret_ct FROM staff_mfa_factors WHERE staff_user_id = $1`, w.b1.StaffID).Scan(&ct); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT token_mac FROM staff_auth_challenges WHERE staff_user_id = $1`, w.b1.StaffID).Scan(&mac)
	})
	if len(ct) == 0 || len(mac) != 32 {
		t.Fatalf("expected to read ciphertext and a MAC: %d %d", len(ct), len(mac))
	}
	// A token built from the stolen MAC (id || mac) is not the token: the keyed MAC of it is different.
	forged := append(w.b1.StaffID[:], mac...)
	enc := base64.RawURLEncoding.EncodeToString(forged)
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, enc); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("a token built from a stolen MAC must not resolve: %v", err)
	}
}

func TestAdversarial_PlantedRowsWithAttackerKeysAreNeverAccepted(t *testing.T) {
	w := newWorld(t)
	attacker := testKeys(t, "attacker") // same kids, different key material
	victim := w.newStaff(&w.tenantB, "support")
	now := t0.Add(31 * time.Second)

	// 1. A planted ACTIVE factor: the attacker seals a secret THEY know under THEIR key with the victim's exact AAD.
	factorID := uuid.New()
	known, err := staffmfa.GenerateTOTPSecret()
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := attacker.SealFactorSecret(staffmfa.FactorAAD{FactorID: factorID, StaffUserID: victim.StaffID, TenantID: w.tenantB, FactorType: staffmfa.FactorTOTP}, known)
	if err != nil {
		t.Fatal(err)
	}
	w.mustRun(victim, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_mfa_factors (id, tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, activated_at)
			VALUES ($1, $2, $3, 'totp', 'active', $4, $5, $6, now())`, factorID, w.tenantB, victim.StaffID, sealed.KID, sealed.Nonce, sealed.Ciphertext)
		return e
	})
	// The attacker presents the code of THEIR secret. It must never be accepted.
	vr, err := w.verify(victim, attackerCode(t, known, now), now)
	if err != nil || vr.Accepted() || vr.Outcome != staffmfa.OutcomeIntegrity || !vr.IntegrityFailure {
		t.Fatalf("a factor sealed under another key must fail integrity: %+v %v", vr, err)
	}

	// 2. A planted recovery code: the attacker hashes a code of their choice with THEIR keys.
	codeBatch, _ := staffmfa.GenerateRecoveryCodes()
	chosen := codeBatch.Codes()[0]
	h, err := attacker.HashRecoveryCode(victim.StaffID, chosen)
	if err != nil {
		t.Fatal(err)
	}
	w.mustRun(victim, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_mfa_recovery_codes (tenant_id, staff_user_id, batch_id, code_kid, code_mac) VALUES ($1, $2, gen_random_uuid(), $3, $4)`,
			w.tenantB, victim.StaffID, h.KID, h.MAC)
		return e
	})
	// (The integrity-failed factor is still 'active' and lock state is open, so the recovery path reaches the lookup.)
	rv, err := w.recover(victim, chosen.RevealForDisplay(), now)
	if err != nil || rv.Accepted() {
		t.Fatalf("a recovery code hashed under another key must never be found: %+v %v", rv, err)
	}
	// The unkeyed variant: a plain SHA-256 of the code.
	w.mustRun(victim, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO staff_mfa_recovery_codes (tenant_id, staff_user_id, batch_id, code_kid, code_mac)
			VALUES ($1, $2, gen_random_uuid(), 'm1', sha256(convert_to($3, 'UTF8')))`, w.tenantB, victim.StaffID, chosen.RevealForDisplay())
		return e
	})
	rv, _ = w.recover(victim, chosen.RevealForDisplay(), now)
	if rv.Accepted() {
		t.Fatal("an unkeyed (SHA-256) recovery code must never be found")
	}

	// 3. A planted challenge and a planted enrolment token with attacker-keyed MACs.
	tok, _ := staffmfa.NewToken(victim.StaffID)
	ch, err := attacker.HashToken(staffmfa.MACChallengeMFA, tok)
	if err != nil {
		t.Fatal(err)
	}
	en, err := attacker.HashToken(staffmfa.MACEnrolBootstrap, tok)
	if err != nil {
		t.Fatal(err)
	}
	w.mustRun(victim, func(ctx context.Context, tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `INSERT INTO staff_auth_challenges (tenant_id, staff_user_id, purpose, token_kid, token_mac, staff_version_at_issue, attempts_left, expires_at, via)
			VALUES ($1, $2, 'mfa', $3, $4, 0, 5, now() + interval '5 minutes', 'login')`, w.tenantB, victim.StaffID, ch.KID, ch.MAC); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, `INSERT INTO staff_enrolment_tokens (tenant_id, staff_user_id, purpose, token_kid, token_mac, expires_at)
			VALUES ($1, $2, 'bootstrap', $3, $4, now() + interval '1 hour')`, w.tenantB, victim.StaffID, en.KID, en.MAC)
		return e
	})
	raw := tok.RevealForDelivery()
	if _, err := w.st.ResolveChallenge(context.Background(), w.rt, ChallengeMFA, raw); !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("a planted challenge must not resolve: %v", err)
	}
	if _, err := w.st.ResolveEnrolmentToken(context.Background(), w.rt, []EnrolmentPurpose{EnrolBootstrap, EnrolMFAReset}, raw); !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("a planted bootstrap token must not resolve: %v", err)
	}
	// Even handed the token directly in the right scope, locking validates the keyed MAC.
	parsed, _ := staffmfa.ParseToken(raw)
	err = w.run(victim, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.LockChallenge(ctx, tx, ChallengeMFA, parsed, now)
		return e
	})
	if !errors.Is(err, ErrInvalidChallenge) {
		t.Fatalf("locking a planted challenge: %v", err)
	}
	err = w.run(victim, func(ctx context.Context, tx pgx.Tx) error {
		_, e := w.st.LockEnrolmentToken(ctx, tx, EnrolBootstrap, parsed, now)
		return e
	})
	if !errors.Is(err, ErrInvalidEnrolmentToken) {
		t.Fatalf("locking a planted enrolment token: %v", err)
	}
}

// R3 (pinned): factor DELETION by SQL is a denial of service, never a bypass - the runtime role cannot delete at all.
// What it CAN do (revoke an active factor of the victim under a forged tenant GUC) leaves the victim with no factor:
// the store reports "absent" (login would say mfa_enrolment_required), and enrolment needs a keyed token.
func TestAdversarial_Residual_R3_RevokingAFactorIsDenialOfServiceNotBypass(t *testing.T) {
	w := newWorld(t)
	_, secret := w.enrol(w.a1, t0)
	// The statement is ALLOWED for the runtime role (the grant permits status / revoked_*): that is the pinned residual.
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = 'reset' WHERE staff_user_id = $1`, w.a1.StaffID)
		return e
	})
	now := t0.Add(31 * time.Second)
	_, err := w.verify(w.a1, codeAt(t, secret, now), now)
	if !errors.Is(err, ErrNoActiveFactor) {
		t.Fatalf("a revoked factor is absent, never accepted: %v", err)
	}
	// Re-enrolment needs the pending factor to be confirmed with a code of the NEW secret; nothing skips it.
	w.mustRun(w.a1, func(ctx context.Context, tx pgx.Tx) error {
		_, _, e := w.factor.Begin(ctx, tx, w.a1, now)
		return e
	})
	if _, err := w.verify(w.a1, codeAt(t, secret, now), now); !errors.Is(err, ErrNoActiveFactor) {
		t.Fatalf("a pending factor is not an active one: %v", err)
	}
}

// Lookup policies: at most the one row whose MAC the caller already holds, SELECT only, only with no tenant GUC.
func TestRLS_PreTenantLookupPolicies(t *testing.T) {
	w := newWorld(t)
	w.seedAll()
	type probe struct{ table, guc, macCol string }
	for _, p := range []probe{{"staff_auth_challenges", "app.staff_challenge_lookup_mac", "token_mac"}, {"staff_enrolment_tokens", "app.staff_enrolment_lookup_mac", "token_mac"}} {
		var macA, macB []byte
		w.mustOwner(w.a1, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT `+p.macCol+` FROM `+p.table+` WHERE staff_user_id = $1`, w.a1.StaffID).Scan(&macA)
		})
		w.mustOwner(w.b1, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT `+p.macCol+` FROM `+p.table+` WHERE staff_user_id = $1`, w.b1.StaffID).Scan(&macB)
		})
		count := func(g guc, where string) (n int) {
			err := w.rtGUC(g, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM `+p.table+` `+where).Scan(&n)
			})
			if err != nil {
				t.Fatalf("%s %v: %v", p.table, g, err)
			}
			return n
		}
		// Exactly the one row, with no other filter at all (the policy, not the query, limits it).
		if n := count(guc{p.guc: hexOf(macA)}, "WHERE tenant_id IS NOT NULL"); n != 1 {
			t.Fatalf("%s: lookup by MAC saw %d rows, want exactly 1", p.table, n)
		}
		if n := count(guc{p.guc: hexOf(randMAC())}, "WHERE tenant_id IS NOT NULL"); n != 0 {
			t.Fatalf("%s: an unknown MAC saw %d rows", p.table, n)
		}
		if n := count(guc{}, "WHERE tenant_id IS NOT NULL"); n != 0 {
			t.Fatalf("%s: no GUC and no tenant must see no tenant rows, saw %d", p.table, n)
		}
		// With ANY tenant set, the lookup GUC adds nothing (migration 0040's "no tenant" conjunct).
		if n := count(guc{"app.tenant_id": w.tenantB.String(), p.guc: hexOf(macA)}, "WHERE staff_user_id = '"+w.a1.StaffID.String()+"'"); n != 0 {
			t.Fatalf("%s: tenant B + tenant A's MAC saw %d of A's rows", p.table, n)
		}
		// SELECT only: no UPDATE and no FOR UPDATE through the lookup policy.
		err := w.rtGUC(guc{p.guc: hexOf(macA)}, func(ctx context.Context, tx pgx.Tx) error {
			set := `revoked_at = now()`
			if p.table == "staff_auth_challenges" {
				set = `consumed_at = now()`
			}
			tag, e := tx.Exec(ctx, `UPDATE `+p.table+` SET `+set+` WHERE `+p.macCol+` = $1`, macA)
			if e == nil && tag.RowsAffected() != 0 {
				t.Fatalf("%s: UPDATE through the lookup policy affected %d rows", p.table, tag.RowsAffected())
			}
			rows, e := tx.Query(ctx, `SELECT 1 FROM `+p.table+` WHERE tenant_id IS NOT NULL FOR UPDATE`)
			if e != nil {
				return e
			}
			n := 0
			for rows.Next() {
				n++
			}
			rows.Close()
			if n != 0 {
				t.Fatalf("%s: FOR UPDATE through the lookup policy locked %d rows", p.table, n)
			}
			return rows.Err()
		})
		wantOK(t, p.table+" lookup write", err)
		// The challenge lookup GUC reveals no enrolment token and vice versa; neither reveals factors or codes.
		other := "staff_enrolment_tokens"
		if p.table == "staff_enrolment_tokens" {
			other = "staff_auth_challenges"
		}
		var n int
		_ = w.rtGUC(guc{p.guc: hexOf(macA)}, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM `+other+` WHERE tenant_id IS NOT NULL) + (SELECT count(*) FROM staff_mfa_factors WHERE tenant_id IS NOT NULL) + (SELECT count(*) FROM staff_mfa_recovery_codes WHERE tenant_id IS NOT NULL)`).Scan(&n)
		})
		if n != 0 {
			t.Fatalf("%s lookup GUC leaked %d rows of other tables", p.table, n)
		}
		_ = macB
		// The restrictive fences still apply to a lookup session.
		if n := count(guc{p.guc: hexOf(macA), "app.platform_service_id": "kyc_submission_worker"}, "WHERE tenant_id IS NOT NULL"); n != 0 {
			t.Fatalf("%s: the KYC worker fence must apply to the lookup policy, saw %d", p.table, n)
		}
		if n := count(guc{p.guc: hexOf(macA), "app.acting_tenant_id": w.tenantA.String()}, "WHERE tenant_id IS NOT NULL"); n != 0 {
			t.Fatalf("%s: the acting fence must apply to the lookup policy, saw %d", p.table, n)
		}
	}
	// The pool helpers refuse anything but 64 lowercase hex characters, before a transaction opens.
	for _, bad := range []string{"", "zz", strings64('A'), strings.Repeat("a", 63), strings.Repeat("a", 65), strings64('g')} {
		err := w.rt.WithStaffChallengeLookup(context.Background(), bad, func(context.Context, pgx.Tx) error { t.Fatal("fn must not run"); return nil })
		if err == nil {
			t.Fatalf("malformed MAC %q accepted", bad)
		}
	}
}

func strings64(c byte) string { return strings.Repeat(string(c), 64) }
