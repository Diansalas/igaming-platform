//go:build integration

package payments

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerref"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0115Version = 115

// scratchThrough migrates a fresh scratch database through version `through`
// (every on-disk migration numbered <= through) and returns the pool and the
// directory.
func scratchThrough(t *testing.T, prefix string, through int64) (*db.Pool, string) {
	t.Helper()
	pool, _, dir := migration0101ScratchApply(t, prefix, through)
	return pool, dir
}

// schemaSnapshot15 renders every policy, trigger, constraint, table, function,
// index and runtime-role grant of the public schema as sorted text - K2's
// whole-schema snapshot (ADR 0100 10.9), extended with indexes and grants (LF
// D-10, T-18).
func schemaSnapshot15(t *testing.T, pool *db.Pool) string {
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
			SELECT 'function:' || p.oid::regprocedure::text || ':' || md5(p.prosrc)
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
	if len(out) > 16 {
		out = append(out[:16], fmt.Sprintf("... (%d differences)", len(out)))
	}
	return strings.Join(out, "\n")
}

// objectNames lists the NAMES of tables, functions, triggers, policies,
// constraints and indexes, so the 0115 additions can be compared with the exact
// ADR 0101 20.1 object list.
func objectNames(t *testing.T, pool *db.Pool) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT 'table:' || tablename FROM pg_tables WHERE schemaname = 'public'
			UNION ALL SELECT 'function:' || p.proname FROM pg_proc p WHERE p.pronamespace = 'public'::regnamespace
			UNION ALL SELECT 'trigger:' || c.relname || '.' || t.tgname FROM pg_trigger t JOIN pg_class c ON c.oid = t.tgrelid
			           WHERE NOT t.tgisinternal AND c.relnamespace = 'public'::regnamespace
			UNION ALL SELECT 'policy:' || tablename || '.' || policyname FROM pg_policies WHERE schemaname = 'public'
			UNION ALL SELECT 'constraint:' || conrelid::regclass::text || '.' || conname FROM pg_constraint WHERE connamespace = 'public'::regnamespace
			UNION ALL SELECT 'index:' || indexname FROM pg_indexes WHERE schemaname = 'public'`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			out[s] = true
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

// expected0115Additions is ADR 0101 20.1 (as amended by section 26), item by item.
// Constraint and index entries for the new tables' own PK/UNIQUE/CHECK/FK
// machinery are matched by table prefix below.
var expected0115Additions = []string{
	// 1 prerequisite
	"constraint:payment_attempts.payment_attempts_id_tenant_key",
	// 2 reference
	"table:payment_manual_resolution_codes",
	// 3 functions
	"function:payment_reserved_ref_prefix", "function:payment_m2_admits", "function:payment_manual_resolution_execution_status",
	// 4, 5 force-resolution tables and their policies
	"table:payment_manual_resolutions", "table:payment_manual_resolution_approvals",
	"policy:payment_manual_resolution_codes.reference_read",
	"policy:payment_manual_resolutions.tenant_scope_select", "policy:payment_manual_resolutions.tenant_scope_insert",
	"policy:payment_manual_resolutions.tenant_scope_update", "policy:payment_manual_resolutions.tenant_system_read_executed",
	"policy:payment_manual_resolutions.acting_read", "policy:payment_manual_resolutions.acting_insert", "policy:payment_manual_resolutions.acting_update",
	"policy:payment_manual_resolution_approvals.tenant_scope_select", "policy:payment_manual_resolution_approvals.tenant_scope_insert",
	"policy:payment_manual_resolution_approvals.acting_read", "policy:payment_manual_resolution_approvals.acting_insert",
	// 6 triggers on 4/5
	"function:payment_manual_resolutions_guard", "function:payment_manual_resolutions_beneficiary_guard",
	"function:payment_manual_resolutions_no_executing_commit", "function:payment_manual_resolution_approvals_guard",
	"function:payment_manual_resolution_approvals_apply_reject",
	"trigger:payment_manual_resolutions.payment_manual_resolutions_guard",
	"trigger:payment_manual_resolutions.payment_manual_resolutions_beneficiary_guard",
	"trigger:payment_manual_resolutions.payment_manual_resolutions_no_executing_commit",
	"trigger:payment_manual_resolutions.payment_manual_resolutions_no_delete",
	"trigger:payment_manual_resolutions.payment_manual_resolutions_no_truncate",
	"trigger:payment_manual_resolution_approvals.payment_manual_resolution_approvals_guard",
	"trigger:payment_manual_resolution_approvals.payment_manual_resolution_approvals_beneficiary_guard",
	"trigger:payment_manual_resolution_approvals.payment_manual_resolution_approvals_apply_reject",
	"trigger:payment_manual_resolution_approvals.payment_manual_resolution_approvals_no_delete",
	"trigger:payment_manual_resolution_approvals.payment_manual_resolution_approvals_no_truncate",
	"trigger:payment_manual_resolution_codes.payment_manual_resolution_codes_immutable",
	"trigger:payment_manual_resolution_codes.payment_manual_resolution_codes_no_truncate",
	// 7 folded evidence
	"table:payment_attempt_reference_evidence",
	"function:payment_attempt_reference_evidence_guard", "function:payment_attempt_reference_evidence_bound_to_park",
	"trigger:payment_attempt_reference_evidence.payment_attempt_reference_evidence_guard",
	"trigger:payment_attempt_reference_evidence.payment_attempt_reference_evidence_bound_to_park",
	"trigger:payment_attempt_reference_evidence.payment_attempt_reference_evidence_immutable",
	"trigger:payment_attempt_reference_evidence.payment_attempt_reference_evidence_no_truncate",
	"policy:payment_attempt_reference_evidence.system_insert", "policy:payment_attempt_reference_evidence.system_select",
	// 8 folded indexes
	"index:payment_statement_lines_ref", "index:payment_statement_lines_merchant", "index:payment_statement_lines_reversal_original",
	// 10 column discipline
	"function:payment_attempts_operator_column_discipline", "trigger:payment_attempts.payment_attempts_operator_column_discipline",
	// 11 reserved prefix CHECKs
	"index:payment_attempt_reference_evi_tenant_id_attempt_id_evidence_key", "index:payment_attempts_id_tenant_key",
	"index:payment_manual_resolution_approval_resolution_id_decided_by_key",
	// (Postgres truncates the two "original_provider_reference" names to 63 bytes.)
	"constraint:payment_attempts.payment_attempts_provider_reference_no_reserved_prefix",
	"constraint:payment_provider_events.payment_provider_events_provider_reference_no_reserved_prefix",
	"constraint:payment_provider_events.payment_provider_events_original_provider_reference_no_reserved",
	"constraint:payment_provider_events.payment_provider_events_settlement_reference_no_reserved_prefix",
	"constraint:payment_statement_lines.payment_statement_lines_provider_reference_no_reserved_prefix",
	"constraint:payment_statement_lines.payment_statement_lines_original_provider_reference_no_reserved",
	"constraint:payment_statement_lines.payment_statement_lines_settlement_reference_no_reserved_prefix",
	"constraint:payment_attempt_reference_evidence.payment_attempt_reference_evidence_reference_no_reserved_prefix",
	"constraint:deposit_intents.deposit_intents_provider_reference_no_reserved_prefix",
	"constraint:withdrawal_requests.withdrawal_requests_provider_reference_no_reserved_prefix",
	// 12 ledger prefix trigger
	"function:ledger_transactions_reserved_prefix_guard", "trigger:ledger_transactions.ledger_transactions_reserved_prefix_guard",
	// 14 acting policies (and 13's ledger_accounts policy is a REPLACEMENT, not an addition)
	"policy:payment_attempts.acting_update", "policy:withdrawal_requests.acting_read", "policy:withdrawal_requests.acting_update",
	"policy:deposit_intents.acting_update",
	// section 26 RC-1
	"function:ledger_adjustment_requests_step_b_person_sep", "function:ledger_adjustment_approvals_step_b_person_sep",
	"trigger:ledger_adjustment_requests.ledger_adjustment_requests_step_b_person_sep",
	"trigger:ledger_adjustment_approvals.ledger_adjustment_approvals_step_b_person_sep",
}

// isTableOwnMachinery reports whether a constraint/index name belongs to the
// machinery of one of the NEW tables (PK, UNIQUE, CHECK, FK, partial indexes).
func isTableOwnMachinery(name string) bool {
	for _, t := range []string{"payment_manual_resolution_codes", "payment_manual_resolutions", "payment_manual_resolution_approvals", "payment_attempt_reference_evidence"} {
		if strings.HasPrefix(name, "constraint:"+t+".") || strings.HasPrefix(name, "index:"+t) {
			return true
		}
	}
	return false
}

// C-16 / T-18 (MIG; D-10): 0115 up/down/up on a scratch DB migrated to N-1. The
// whole-schema snapshot (policies, triggers, constraints, tables, functions,
// indexes, runtime grants) after down equals the one before up - covering the
// restored 0107 guard, 0113's fence/entries-fence/payload-refusal bodies, the
// ledger_accounts acting_insert policy, the kind CHECK, the 0107 index
// definitions, and every R-1/R-2/R-4/D-8/RC-1 object. The set of objects 0115
// ADDS equals the ADR 0101 20.1 list exactly.
func TestK3_C16_T18_Migration0115UpDownUp_WholeSchema(t *testing.T) {
	pool, _ := scratchThrough(t, "k3m0115_", migration0115Version-1)
	preSnap := schemaSnapshot15(t, pool)
	preNames := objectNames(t, pool)

	dir := migration0101Dir(t, migration0115Version)
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("up: %v", err)
	}
	upSnap := schemaSnapshot15(t, pool)
	if upSnap == preSnap {
		t.Fatal("0115 changed nothing?")
	}
	upNames := objectNames(t, pool)

	// The exact object list.
	want := map[string]bool{}
	for _, n := range expected0115Additions {
		want[n] = true
	}
	var unexpected, missing []string
	for n := range upNames {
		if !preNames[n] && !want[n] && !isTableOwnMachinery(n) && !strings.HasPrefix(n, "table:") {
			unexpected = append(unexpected, n)
		}
	}
	for n := range want {
		if !upNames[n] {
			missing = append(missing, n)
		}
	}
	sort.Strings(unexpected)
	sort.Strings(missing)
	if len(unexpected) > 0 || len(missing) > 0 {
		t.Fatalf("0115 objects differ from the ADR 0101 20.1 list:\n  not in the list: %v\n  missing: %v", unexpected, missing)
	}

	if _, err := pool.MigrateDown(context.Background(), dir, 1); err != nil {
		t.Fatalf("down on an empty 0115: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != preSnap {
		t.Fatalf("0115 down did not restore the N-1 schema exactly:\n%s", snapDiff(preSnap, got))
	}
	if _, err := pool.MigrateUp(context.Background(), dir); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if got := schemaSnapshot15(t, pool); got != upSnap {
		t.Fatalf("0115 re-up differs from the first up:\n%s", snapDiff(upSnap, got))
	}
}

// C-16 (MIG): the down REFUSES (MR099) while a resolution row, a new-kind
// mismatch row or an evidence row exists, and the refusal rolls back whole.
func TestK3_C16_DownRefusals(t *testing.T) {
	t.Run("resolution_row", func(t *testing.T) {
		pool, _ := scratchThrough(t, "k3down1_", migration0115Version)
		dir := migration0101Dir(t, migration0115Version)
		w := newK3WorldOn(t, pool, k3Opts{base: 1})
		_, a := w.ambiguousPayout(100)
		w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
		_, err := pool.MigrateDown(context.Background(), dir, 1)
		k3RequireCode(t, err, "MR099")
		// Whole rollback: the tables are still there.
		var n int
		if err := w.pool.WithPrincipalScope(context.Background(), w.f.tenantID, w.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.f.tenantID).Scan(&n)
		}); err != nil || n != 1 {
			t.Fatalf("resolution rows after a refused down = %d (%v)", n, err)
		}
	})
	t.Run("new_kind_mismatch_row", func(t *testing.T) {
		pool, _ := scratchThrough(t, "k3down2_", migration0115Version)
		dir := migration0101Dir(t, migration0115Version)
		w := newK3WorldOn(t, pool, k3Opts{base: 1})
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			runID := uuid.New()
			if _, err := tx.Exec(ctx, `INSERT INTO reconciliation_runs (id, tenant_id, stream, period_start, period_end, status)
				VALUES ($1, $2, 'payment_statement', now() - interval '1 hour', now(), 'mismatches_found')`, runID, w.f.tenantID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO reconciliation_mismatches (id, tenant_id, reconciliation_run_id, reconciliation_key, expected_value, actual_value, mismatch_kind)
				VALUES (gen_random_uuid(), $1, $2, 'k3 down check', 'x', 'y', 'pay_declared_paid_unconfirmed')`, w.f.tenantID, runID)
			return err
		})
		_, err := pool.MigrateDown(context.Background(), dir, 1)
		k3RequireCode(t, err, "MR099")
	})
	t.Run("evidence_row", func(t *testing.T) {
		pool, _ := scratchThrough(t, "k3down3_", migration0115Version)
		dir := migration0101Dir(t, migration0115Version)
		w := newK3WorldOn(t, pool, k3Opts{base: 1})
		w.parkPollMismatch()
		_, err := pool.MigrateDown(context.Background(), dir, 1)
		k3RequireCode(t, err, "MR099")
	})
}

// Up-time refusal (security O-4): an existing reserved-prefix value in any
// provider-supplied reference column makes the up refuse (MR098) and change
// nothing.
func TestK3_UpRefusesWhenAReservedPrefixValueExists(t *testing.T) {
	prefix := providerref.ReservedOperatorPrefix
	cases := map[string]func(t *testing.T, pool *db.Pool, f m0101Fixture){
		"ledger_transactions.provider_tx_id": func(t *testing.T, pool *db.Pool, f m0101Fixture) {
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
					VALUES (gen_random_uuid(), $1, 'withdrawal_completed', 'p:'||$2::text, 'p', $2, gen_random_uuid())`, f.tenantID, prefix+"x")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		},
		"deposit_intents.provider_reference": func(t *testing.T, pool *db.Pool, f m0101Fixture) {
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, status, idempotency_key, provider_id, provider_reference)
					VALUES (gen_random_uuid(), $1, $2, $3, $4, 'EUR', 100, 'card', 'pending', 'k3-up-prefix', 'p', $5)`, f.tenantID, f.brandID, f.playerID, f.walletID, prefix+"y")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		},
		"payment_provider_events.settlement_reference": func(t *testing.T, pool *db.Pool, f m0101Fixture) {
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `INSERT INTO payment_provider_events (id, tenant_id, provider_id, event_type, provider_reference, settlement_reference, outcome, event_fingerprint, disposition_at_receipt)
					VALUES (gen_random_uuid(), $1, 'p', 'payout', 'r1', $2, 'pending', decode('00', 'hex'), 'anomaly')`, f.tenantID, prefix+"z")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, seed := range cases {
		name, seed := name, seed
		t.Run(name, func(t *testing.T) {
			pool, _ := scratchThrough(t, "k3up_", migration0115Version-1)
			f := seedM0101Fixture(t, pool)
			seed(t, pool, f)
			_, err := pool.MigrateUp(context.Background(), migration0101Dir(t, migration0115Version))
			if k3Code(err) != "MR098" {
				t.Fatalf("up with a reserved-prefix value in %s: want MR098, got %v", name, err)
			}
			var n int
			if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM pg_tables WHERE tablename = 'payment_manual_resolutions'`).Scan(&n)
			}); err != nil || n != 0 {
				t.Fatalf("a refused up changed the schema (tables=%d err=%v)", n, err)
			}
		})
	}
}

// guardBody extracts payment_attempts_guard()'s function text from a migration
// file.
func guardBody(t *testing.T, file string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(realMigrationsDir(t), file))
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, "CREATE OR REPLACE FUNCTION payment_attempts_guard()")
	if i < 0 {
		t.Fatalf("%s has no payment_attempts_guard()", file)
	}
	j := strings.Index(s[i:], "$$ LANGUAGE plpgsql;")
	return strings.Split(s[i:i+j], "\n")
}

// C-17 (R): F12 - a line diff of 0115's guard against 0107's is EXACTLY the ADR
// 0101 8.3 edit: two inserted whitelist lines and two rewritten evidence gates.
func TestK3_C17_GuardDiffIsExactlyTheApprovedEdit(t *testing.T) {
	oldL, newL := guardBody(t, "0107_deposit_intent_double_credit_backstop.up.sql"), guardBody(t, "0115_payment_force_resolution.up.sql")
	// Longest common subsequence over TRIMMED lines.
	trim := func(ls []string) []string {
		out := make([]string, len(ls))
		for i, l := range ls {
			out[i] = strings.TrimSpace(l)
		}
		return out
	}
	a, b := trim(oldL), trim(newL)
	n, m := len(a), len(b)
	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	var removed, added []string
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && a[i] == b[j]:
			i++
			j++
		case i < n && (j == m || dp[i+1][j] >= dp[i][j+1]):
			removed = append(removed, a[i])
			i++
		default:
			added = append(added, b[j])
			j++
		}
	}
	wantRemoved := []string{
		"IF NEW.state = 'succeeded' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status') THEN",
		"IF NEW.state = 'declined' AND OLD.operation = 'payout' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status') THEN",
	}
	const admits = "payment_m2_admits(OLD.id, OLD.state, OLD.terminal_reason, OLD.withdrawal_request_id, NEW.state, NEW.last_evidence_kind)"
	wantAdded := []string{
		"OR (OLD.state = 'disputed' AND NEW.state = 'succeeded' AND OLD.operation = 'payout' AND " + admits + ")",
		"OR (OLD.state = 'disputed' AND NEW.state = 'declined'  AND OLD.operation = 'payout' AND " + admits + ")",
		"IF NEW.state = 'succeeded' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status')",
		"AND NOT (OLD.operation = 'payout' AND " + admits + ") THEN",
		"IF NEW.state = 'declined' AND OLD.operation = 'payout' AND NEW.last_evidence_kind NOT IN ('sync', 'callback', 'query_status')",
		"AND NOT (OLD.operation = 'payout' AND " + admits + ") THEN",
	}
	// The head comment/CREATE line is identical in both; the diff must be only these.
	sort.Strings(removed)
	sort.Strings(wantRemoved)
	sort.Strings(added)
	sort.Strings(wantAdded)
	if strings.Join(removed, "\n") != strings.Join(wantRemoved, "\n") || strings.Join(added, "\n") != strings.Join(wantAdded, "\n") {
		t.Fatalf("the guard diff is not exactly the 8.3 edit.\nremoved: %q\nwant:    %q\nadded:   %q\nwant:    %q", removed, wantRemoved, added, wantAdded)
	}
}

// Pins: the Go and SQL reserved prefix are equal and 27 bytes (5.4).
func TestK3_ReservedPrefixPin(t *testing.T) {
	if len(providerref.ReservedOperatorPrefix) != 27 {
		t.Fatalf("prefix length %d, want 27", len(providerref.ReservedOperatorPrefix))
	}
	w := newK3World(t, k3Opts{base: 1})
	var sqlPrefix string
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT payment_reserved_ref_prefix()`).Scan(&sqlPrefix)
	})
	if sqlPrefix != providerref.ReservedOperatorPrefix {
		t.Fatalf("SQL prefix %q != Go prefix %q", sqlPrefix, providerref.ReservedOperatorPrefix)
	}
}

// C-9c (MIG): catalogue-driven pin. Every column whose name matches
// %provider_reference%, %provider_tx_id% or settlement_reference - plus
// payment_attempt_reference_evidence.reference BY NAME - carries the reserved-
// prefix CHECK (or, for ledger_transactions.provider_tx_id, the all-sessions
// trigger), or is listed as exempt with a reason. A new such column fails here
// until it is classified. merchant_reference columns are exempt (the platform
// generates them).
func TestK3_C9c_ReservedPrefixCatalogue(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	exempt := map[string]string{
		"payment_manual_resolutions.reserved_provider_tx_id": "the platform-generated reserved id itself (forced by the guard)",
		"casino_callback_rejections.provider_tx_id":          "casino rejection log of a refused provider value; never a ledger key (covered by the all-sessions ledger trigger for postings)",
		"casino_callback_rejections.original_provider_tx_id": "same: casino rejection log",
		"kyc_verifications.provider_reference":               "a KYC vendor reference, not a payment reference",
	}
	byName := []string{"payment_attempt_reference_evidence.reference"}
	var cols []string
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT table_name || '.' || column_name FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name IN (SELECT tablename FROM pg_tables WHERE schemaname = 'public')
			  AND (column_name LIKE '%provider\_reference%' OR column_name LIKE '%provider\_tx\_id%' OR column_name = 'settlement_reference')`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				return err
			}
			cols = append(cols, s)
		}
		return rows.Err()
	})
	cols = append(cols, byName...)
	if len(cols) < 12 {
		t.Fatalf("the catalogue query found only %d columns: the pin is vacuous", len(cols))
	}
	for _, c := range cols {
		table, col, _ := strings.Cut(c, ".")
		if _, ok := exempt[c]; ok {
			continue
		}
		if c == "ledger_transactions.provider_tx_id" {
			if n := w.countRows(`SELECT count(*) FROM pg_trigger WHERE tgname = 'ledger_transactions_reserved_prefix_guard' AND NOT tgisinternal`); n != 1 {
				t.Errorf("%s: the all-sessions trigger is missing", c)
			}
			continue
		}
		n := w.countRows(`SELECT count(*) FROM pg_constraint WHERE contype = 'c' AND conrelid = $1::regclass
			AND pg_get_constraintdef(oid) LIKE '%payment_reserved_ref_prefix%' AND pg_get_constraintdef(oid) LIKE '%' || $2 || '%'`, table, col)
		if n < 1 {
			t.Errorf("%s: no reserved-prefix CHECK and not exempt (classify it in ADR 0101 5.4 / this pin)", c)
		}
	}
}

// Runtime-role grants (ADR 0101 20.1 #18): the in-migration REVOKE ALL + GRANT
// block grants exactly the intended privileges to igaming_runtime, and
// deploy/init-app-role.sql lists the same four rows.
func TestK3_RuntimeGrants(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	want := map[string]string{
		"payment_manual_resolution_codes":     "SELECT",
		"payment_manual_resolutions":          "INSERT,SELECT,UPDATE",
		"payment_manual_resolution_approvals": "INSERT,SELECT",
		"payment_attempt_reference_evidence":  "INSERT,SELECT",
	}
	for table, privs := range want {
		var got string
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT COALESCE(string_agg(privilege_type, ',' ORDER BY privilege_type), '') FROM information_schema.role_table_grants
				WHERE table_schema = 'public' AND table_name = $1 AND grantee = 'igaming_runtime'`, table).Scan(&got)
		})
		if got != privs {
			t.Errorf("%s: igaming_runtime privileges = %q, want %q", table, got, privs)
		}
	}
	b, err := os.ReadFile(filepath.Join(realMigrationsDir(t), "..", "deploy", "init-app-role.sql"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`\('(payment_[a-z_]+)', '([A-Z, ]+)'\)`)
	got := map[string]string{}
	for _, m := range re.FindAllStringSubmatch(string(b), -1) {
		got[m[1]] = strings.ReplaceAll(m[2], " ", "")
	}
	for table, privs := range want {
		g := strings.Split(got[table], ",")
		sort.Strings(g)
		if strings.Join(g, ",") != privs {
			t.Errorf("deploy/init-app-role.sql %s = %q, want %q", table, got[table], privs)
		}
	}
	_ = scratchdb.New
}
