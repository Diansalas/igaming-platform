//go:build integration

// Migration 0132 (ADR 0113 slice 1, M-A) on THROWAWAY scratch databases, plus the catalog pins on the
// shared test database:
//
//   - the pre-flight is not blind: on a 0131 database holding clean requests in EVERY reachable state
//     (driven by the real Go writers), one dirty row per class is planted (as the owner, under the
//     tenant GUC: FORCE RLS hides it from a plain SELECT) and the 0132 up script, executed in the same
//     transaction, must refuse WD098 naming that class with count 1; nothing is changed or deleted (the
//     transaction is rolled back; the class counts are the only output);
//   - the clean database then migrates; down restores the 0131 schema byte for byte; up again equals the
//     first up (whole-schema snapshot);
//   - a FUTURE column of withdrawal_requests is frozen by default (to_jsonb column discipline, WD022);
//   - catalog: trigger names / order / no WHEN / timing / deferrable, functions not SECURITY DEFINER with
//     a pinned search_path.
//
// Requires TEST_DATABASE_URL and TEST_ADMIN_DATABASE_URL (scratchdb); a skip is NOT evidence.
package withdrawal

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
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const migration0132Version = 132

func m132Dir(t *testing.T, through int64) string {
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

func m132Scratch(t *testing.T, prefix string) *db.Pool {
	t.Helper()
	u := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), u, 10, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func m132Snapshot(t *testing.T, pool *db.Pool) string {
	t.Helper()
	var snap string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT string_agg(x, E'\n' ORDER BY x) FROM (
			SELECT 'policy:' || tablename || ':' || policyname || ':' || permissive || ':' || cmd || ':' || coalesce(qual, '') || ':' || coalesce(with_check, '') AS x
			  FROM pg_policies WHERE schemaname = 'public'
			UNION ALL
			SELECT 'trigger:' || c.relname || ':' || t.tgname || ':' || t.tgenabled::text || ':' || pg_get_triggerdef(t.oid)
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

func m132Diff(a, b string) string {
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

func TestMigration0132_PreflightRefusesEveryDirtyClass_ThenUpDownUp(t *testing.T) {
	ctx := context.Background()
	pool := m132Scratch(t, "m0132p_")
	if _, err := pool.MigrateUp(ctx, m132Dir(t, migration0132Version-1)); err != nil {
		t.Fatal(err)
	}
	snap131 := m132Snapshot(t, pool)

	// Clean data in every reachable state, through the Go writers (the scratch owner drives: the
	// runtime role has no grants on a scratch database).
	w := newA13On(t, pool, pool)
	rows := map[State]WithdrawalRequest{}
	for _, s := range []State{StateRequested, StatePendingReview, StateApproved, StateSubmitted, StateCompleted, StateFailed, StateCancelled, StateRejected} {
		rows[s] = w.at(t, s)
	}
	upSQL, err := os.ReadFile(filepath.Join("..", "..", "migrations", "0132_withdrawal_state_machine_and_hold_invariant.up.sql"))
	if err != nil {
		t.Fatal(err)
	}

	// Each dirty class, planted in a transaction that also runs the 0132 up script and is rolled back.
	classes := []struct {
		class string
		plant func(ctx context.Context, tx pgx.Tx) error
	}{
		{"reversed", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'reversed' WHERE id = $1`, rows[StateCompleted].ID)
			return err
		}},
		{"columns", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET provider_id = 'p' WHERE id = $1`, rows[StateRequested].ID)
			return err
		}},
		{"hold_link", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO withdrawal_requests (tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, idempotency_key, payout_instrument_id, payout_instrument_fingerprint)
				VALUES ($1, $2, $3, $4, 'EUR', 10, $5, $6, $7)`, w.f.tenantID, w.f.brandID, w.f.playerAccountID, w.f.walletID, "dirty-"+uuid.NewString(), w.f.instrumentID, w.fingerprint)
			return err
		}},
		{"release_link", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET release_ledger_transaction_id = hold_ledger_transaction_id WHERE id = $1`, rows[StateCancelled].ID)
			return err
		}},
		{"hold_sum", func(ctx context.Context, tx pgx.Tx) error {
			r := rows[StateRequested]
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalRequested,
				IdempotencyKey: "dirty-" + r.ID.String(), CorrelationID: r.ID,
				Entries: []ledger.EntryInput{{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: r.Amount}, {LedgerAccountID: w.f.holdAccountID, Direction: ledger.Credit, Amount: r.Amount}}})
			return err
		}},
		{"hold_entry", func(ctx context.Context, tx pgx.Tx) error {
			p, k := "dirty-psp", "dirty-"+uuid.NewString()
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: k,
				ProviderID: &p, ProviderTxID: &k, CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{{LedgerAccountID: w.clearing(), Direction: ledger.Debit, Amount: 3}, {LedgerAccountID: w.f.holdAccountID, Direction: ledger.Credit, Amount: 3}}})
			return err
		}},
		{"walletless_player_account", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO ledger_accounts (tenant_id, account_type, asset_code, status) VALUES ($1, 'player_withdrawal_hold', 'EUR', 'active')`, w.f.tenantID)
			return err
		}},
		{"withdrawal_tx_without_hold_leg", func(ctx context.Context, tx pgx.Tx) error {
			k := "dirty-" + uuid.NewString()
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxWithdrawalFailed, IdempotencyKey: k,
				CorrelationID: uuid.New(),
				Entries:       []ledger.EntryInput{{LedgerAccountID: w.clearing(), Direction: ledger.Debit, Amount: 6}, {LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: 6}}})
			return err
		}},
		// Class 9b (ledger-finance): the same shape under a tenant id with NO tenants row - invisible to
		// the per-tenant RLS loop (its count stays 0, asserted below), caught only by the owner count.
		{"withdrawal_tx_without_hold_leg_unscoped", func(ctx context.Context, tx pgx.Tx) error {
			orphan := uuid.New()
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, orphan.String()); err != nil {
				return err
			}
			a, err := ledger.GetOrCreateAccount(ctx, tx, orphan, nil, ledger.AccountPSPClearing, "EUR")
			if err != nil {
				return err
			}
			b, err := ledger.GetOrCreateAccount(ctx, tx, orphan, nil, ledger.AccountManualAdjustment, "EUR")
			if err != nil {
				return err
			}
			k := "dirty-" + uuid.NewString()
			_, err = ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: orphan, TransactionType: ledger.TxWithdrawalFailed, IdempotencyKey: k,
				CorrelationID: uuid.New(),
				Entries:       []ledger.EntryInput{{LedgerAccountID: a, Direction: ledger.Debit, Amount: 7}, {LedgerAccountID: b, Direction: ledger.Credit, Amount: 7}}})
			return err
		}},
		{"hold_balance", func(ctx context.Context, tx pgx.Tx) error {
			p, k := "dirty-psp", "dirty-"+uuid.NewString()
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.f.tenantID, TransactionType: ledger.TxDeposit, IdempotencyKey: k,
				ProviderID: &p, ProviderTxID: &k, CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{{LedgerAccountID: w.clearing(), Direction: ledger.Debit, Amount: 4}, {LedgerAccountID: w.f.holdAccountID, Direction: ledger.Credit, Amount: 4}}})
			return err
		}},
	}
	for _, c := range classes {
		err := func() error {
			tx, err := pool.Raw().Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tx.Rollback(ctx) }() // always: nothing planted here ever commits
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, w.f.tenantID.String()); err != nil {
				t.Fatal(err)
			}
			if err := c.plant(ctx, tx); err != nil {
				t.Fatalf("%s: plant: %v", c.class, err)
			}
			// The FORCE RLS lesson: the planted row is invisible without the tenant GUC.
			if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
				t.Fatal(err)
			}
			// Flush the plant's deferred ledger checks: the pre-flight's 9b lifts FORCE RLS with ALTER
			// TABLE, which PostgreSQL refuses on a table with pending trigger events (a real migration
			// transaction has none).
			if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
				t.Fatalf("%s: flush: %v", c.class, err)
			}
			_, err = tx.Exec(ctx, string(upSQL))
			return err
		}()
		wantCode(t, c.class, err, "WD098")
		wantMsg(t, c.class, err, " "+c.class+"=1")
		switch c.class {
		case "withdrawal_tx_without_hold_leg":
			wantMsg(t, c.class, err, " withdrawal_tx_without_hold_leg_unscoped=1") // the backstop counts every row
		case "withdrawal_tx_without_hold_leg_unscoped":
			wantMsg(t, c.class, err, " withdrawal_tx_without_hold_leg=0") // the RLS loop is blind to it
		}
	}

	// Clean: the migration applies.
	full := m132Dir(t, migration0132Version)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("0132 on a clean database with requests in every state: %v", err)
	}
	snap132 := m132Snapshot(t, pool)
	if snap131 == snap132 {
		t.Fatal("0132 changed nothing in the schema snapshot (vacuous)")
	}
	// The data built at 0131 still satisfies A: touching every non-terminal row commits.
	for _, s := range []State{StateRequested, StatePendingReview, StateApproved, StateSubmitted} {
		if err := w.rtExec(`UPDATE withdrawal_requests SET updated_at = now() WHERE id = $1`, rows[s].ID); err != nil {
			t.Fatalf("touch a pre-0132 %s row: %v", s, err)
		}
	}
	// A future column is frozen by default (WD022).
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE withdrawal_requests ADD COLUMN zz_future_column text`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	wantCode(t, "a future column", w.rtExec(`UPDATE withdrawal_requests SET zz_future_column = 'x' WHERE id = $1`, rows[StateApproved].ID), "WD022")
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `ALTER TABLE withdrawal_requests DROP COLUMN zz_future_column`)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("down: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snap131 {
		t.Fatalf("down did not restore the 0131 schema byte for byte:\n%s", m132Diff(snap131, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatal(err)
	}
	if got := m132Snapshot(t, pool); got != snap132 {
		t.Fatalf("up after down differs from the first up:\n%s", m132Diff(snap132, got))
	}
}

// Catalog pins (shared test database at head).
func TestMigration0132_Catalog(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	q := func(sql string, args ...any) []string {
		t.Helper()
		var out []string
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			rows, err := tx.Query(ctx, sql, args...)
			if err != nil {
				return err
			}
			out, err = pgx.CollectRows(rows, pgx.RowTo[string])
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return out
	}
	// BEFORE row triggers, per event, in firing (name) order.
	order := func(rel string, bit int) []string {
		return q(`SELECT tgname::text FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal
			AND (tgtype & 1) = 1 AND (tgtype & 2) = 2 AND (tgtype & $2::int) = $2::int AND tgenabled = 'O' ORDER BY tgname`, rel, bit)
	}
	ins := order("withdrawal_requests", 4)
	if len(ins) < 3 || ins[0] != "withdrawal_requests_aa_state_transition_guard" || ins[1] != "withdrawal_requests_aa_subject_active_guard" {
		t.Fatalf("withdrawal_requests BEFORE INSERT order: %v", ins)
	}
	upd := order("withdrawal_requests", 16)
	if len(upd) < 4 || upd[0] != "withdrawal_requests_aa_state_transition_guard" {
		t.Fatalf("withdrawal_requests BEFORE UPDATE order (the guard must precede approved_hold_freeze / governed_release_guard / immutable_fields): %v", upd)
	}
	le := order("ledger_entries", 4)
	pos := map[string]int{}
	for i, n := range le {
		pos[n] = i
	}
	gi, ok1 := pos["ledger_entries_withdrawal_hold_guard"]
	pi, ok2 := pos["ledger_entries_populate_from_account_trigger"]
	if !ok1 || !ok2 || gi <= pi {
		t.Fatalf("ledger_entries BEFORE INSERT: the hold guard must fire AFTER populate_from_account (wallet_id): %v", le)
	}
	// No WHEN clause on any of the four (HN-4); the two at-commit triggers are deferred constraint triggers.
	for _, row := range q(`SELECT tgname || '|' || (tgqual IS NULL)::text || '|' || tgdeferrable::text || '|' || tginitdeferred::text || '|' || (tgconstraint <> 0)::text
		FROM pg_trigger WHERE tgname IN ('withdrawal_requests_aa_state_transition_guard', 'withdrawal_requests_ledger_link_at_commit',
		  'ledger_entries_withdrawal_hold_guard', 'ledger_entries_withdrawal_hold_link_at_commit', 'ledger_transactions_withdrawal_hold_leg_at_commit') ORDER BY tgname`) {
		parts := strings.Split(row, "|")
		atCommit := strings.HasSuffix(parts[0], "_at_commit")
		if parts[1] != "true" {
			t.Fatalf("%s carries a WHEN clause", parts[0])
		}
		if atCommit && (parts[2] != "true" || parts[3] != "true" || parts[4] != "true") {
			t.Fatalf("%s must be a DEFERRABLE INITIALLY DEFERRED constraint trigger: %s", parts[0], row)
		}
		if !atCommit && parts[4] != "false" {
			t.Fatalf("%s must be a plain trigger: %s", parts[0], row)
		}
	}
	if n := len(q(`SELECT tgname::text FROM pg_trigger WHERE tgname IN ('withdrawal_requests_aa_state_transition_guard', 'withdrawal_requests_ledger_link_at_commit',
		'ledger_entries_withdrawal_hold_guard', 'ledger_entries_withdrawal_hold_link_at_commit', 'ledger_transactions_withdrawal_hold_leg_at_commit') AND tgenabled = 'O'`)); n != 5 {
		t.Fatalf("expected the five 0132 triggers enabled, got %d", n)
	}
	for _, fn := range []string{"withdrawal_requests_state_transition_guard", "withdrawal_request_ledger_link_violation",
		"withdrawal_requests_ledger_link_at_commit", "ledger_entries_withdrawal_hold_guard", "ledger_entries_withdrawal_hold_link_at_commit",
		"ledger_transactions_withdrawal_hold_leg_at_commit"} {
		got := q(`SELECT prosecdef::text || '|' || coalesce(array_to_string(proconfig, ','), '') FROM pg_proc WHERE proname = $1`, fn)
		if len(got) != 1 || got[0] != "false|search_path=pg_catalog, public, pg_temp" {
			t.Fatalf("%s: want not SECURITY DEFINER with a pinned search_path, got %v", fn, got)
		}
	}
	// Security L3: the shared check function is not executable by PUBLIC.
	if got := q(`SELECT has_function_privilege('public', 'withdrawal_request_ledger_link_violation(uuid, uuid)', 'EXECUTE')::text`); got[0] != "false" {
		t.Fatal("withdrawal_request_ledger_link_violation must not be executable by PUBLIC")
	}
	if got := q(`SELECT coalesce(has_function_privilege('igaming_runtime', 'withdrawal_request_ledger_link_violation(uuid, uuid)', 'EXECUTE'), false)::text`); got[0] != "true" {
		t.Fatal("igaming_runtime needs EXECUTE on withdrawal_request_ledger_link_violation (its sessions fire A / B-ii)")
	}
	// Security C1: the player-types-have-a-wallet CHECK exists and is validated.
	if got := q(`SELECT convalidated::text FROM pg_constraint WHERE conname = 'ledger_accounts_player_types_have_wallet' AND conrelid = 'ledger_accounts'::regclass`); len(got) != 1 || got[0] != "true" {
		t.Fatalf("ledger_accounts_player_types_have_wallet: %v", got)
	}
	// Pre-flight 9b lifts FORCE RLS on three ledger tables for its owner count: it must be restored.
	if got := q(`SELECT relname::text || '=' || relforcerowsecurity::text FROM pg_class
		WHERE oid IN ('ledger_transactions'::regclass, 'ledger_entries'::regclass, 'ledger_accounts'::regclass) ORDER BY relname`); strings.Join(got, ",") != "ledger_accounts=true,ledger_entries=true,ledger_transactions=true" {
		t.Fatalf("FORCE ROW LEVEL SECURITY must be restored by the 0132 pre-flight: %v", got)
	}
	// B-i mechanics (C4): no lock, exactly one ledger_accounts read, the NULL fast exit first.
	src := q(`SELECT prosrc FROM pg_proc WHERE proname = 'ledger_entries_withdrawal_hold_guard'`)[0]
	if strings.Contains(strings.ToUpper(src), "FOR UPDATE") || strings.Contains(strings.ToUpper(src), "FOR SHARE") || strings.Contains(src, "pg_advisory") {
		t.Fatal("B-i must take no lock")
	}
	if strings.Count(src, "FROM ledger_accounts") != 1 {
		t.Fatalf("B-i must read ledger_accounts exactly once, found %d", strings.Count(src, "FROM ledger_accounts"))
	}
	if i, j := strings.Index(src, "NEW.wallet_id IS NULL"), strings.Index(src, "FROM ledger_accounts"); i < 0 || i > j {
		t.Fatal("B-i must exit on a NULL wallet_id before any read")
	}
}

// The deeper guards stay armed behind the new one. The state-transition guard now answers first for
// every frozen column (WD022) and every terminal row (WD020), so the 0026/0123 immutable-fields trigger
// and the 0124 governed-release guard are no longer the first refusal any UPDATE meets. With the 0132
// guard disabled by the owner inside a transaction that is always rolled back, each still refuses on
// its own (a mutant that dropped one of them would fail here).
func TestADR0113_DeeperGuardsStillArmedBehindTheStateGuard(t *testing.T) {
	w := newA13(t)
	req := w.at(t, StateRequested)
	appr := w.at(t, StateApproved)
	ctx := context.Background()
	try := func(name, stmt string, args ...any) error {
		t.Helper()
		tx, err := w.owner.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		for _, s := range []string{
			`SELECT set_config('app.tenant_id', '` + w.f.tenantID.String() + `', true)`,
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
		} {
			if _, err := tx.Exec(ctx, s); err != nil {
				t.Fatal(err)
			}
		}
		_, err = tx.Exec(ctx, stmt, args...)
		return err
	}
	for name, stmt := range map[string]string{
		"amount":  `UPDATE withdrawal_requests SET amount = amount + 1 WHERE id = $1`,
		"binding": `UPDATE withdrawal_requests SET payout_instrument_fingerprint = repeat('a', 64) WHERE id = $1`,
	} {
		wantMsg(t, "0026/0123 immutable fields: "+name, try(name, stmt, req.ID), "are immutable after insert")
	}
	// 0124 approved-hold freeze (HR050), re-driven through a LEGAL edge with the 0132 guard ENABLED:
	// E5 (approved -> rejected, DenyForCompliance, the genuine `:kyc_denied` leg) on a suspended brand
	// passes WD02x / B-i and is refused by the deeper freeze, as the runtime role.
	launchfix.SetBrandStatus(t, w.f.tenantID, w.f.brandID, "suspended")
	err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := LockApprovedForSubmission(ctx, tx, appr.ID); err != nil {
			return err
		}
		params, decision := denialDecision(w.f, appr.ID)
		_, err := DenyForCompliance(ctx, tx, appr.ID, decision, params)
		return err
	})
	wantCode(t, "0124 approved-hold freeze on a legal E5", err, "HR050")
}
