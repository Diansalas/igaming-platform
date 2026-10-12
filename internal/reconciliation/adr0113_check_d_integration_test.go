//go:build integration

// ADR 0113 slice 3 (s4.4 / s7 D, C7): reconciliation check D, the standing detective control of
// invariant #12, run through the real ledger_vs_projection sweep. The sweeps run on the RUNTIME role
// (igaming_runtime: non-superuser, non-BYPASSRLS), as the production scheduler does; the owner pool
// only seeds fixtures and owner tamper plants (ADR 0110 9.4, history the 0132 triggers now refuse to
// every session). Plants are permanent (append-only ledger): those tests skip on a persistent shared
// database (wdpath.SkipOnPersistentDatabase) and run on a private or CI service database.
package reconciliation

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// dWithdrawal plants E0 + E0b (legacy NULL binding, owner, the sanctioned binding-guard plant) on the
// fixture wallet and walks it to target through the Go writers as igaming_runtime.
func dWithdrawal(t *testing.T, pool *db.Pool, f fixture, amount int64, target withdrawal.State) uuid.UUID {
	t.Helper()
	spec := wdpath.Spec{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
		AssetCode: "EUR", Amount: amount, IdempotencyKey: "d-" + uuid.NewString()}
	var res wdpath.Result
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return pitest.WithoutBindingGuard(ctx, tx, func() error {
			var err error
			res, err = wdpath.Plant(ctx, tx, spec)
			return err
		})
	}); err != nil {
		t.Fatalf("plant withdrawal: %v", err)
	}
	if target != withdrawal.StateRequested {
		wdpath.AdvanceAsRuntime(context.Background(), t, pool, f.tenantID, res.ID, target, spec)
	}
	return res.ID
}

// dSweep runs the full sweep for the fixture tenant on the RUNTIME role and returns its outcome.
func dSweep(t *testing.T, rt *db.Pool, f fixture) SweepOutcome {
	t.Helper()
	now := time.Now()
	out, err := RunSweepTenants(context.Background(), rt, nil, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatal(err)
	}
	return findOutcome(t, out, f)
}

type dRow struct {
	kind, key, expected, actual, status string
	run                                 uuid.UUID
}

func dMismatches(t *testing.T, pool *db.Pool, f fixture) []dRow {
	t.Helper()
	var out []dRow
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT mismatch_kind, reconciliation_key, expected_value, actual_value, investigation_status, reconciliation_run_id
			FROM reconciliation_mismatches
			WHERE tenant_id = $1 AND mismatch_kind IN ('withdrawal_hold_open_mismatch', 'withdrawal_request_ledger_link_invalid')
			ORDER BY created_at, mismatch_kind, reconciliation_key`, f.tenantID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var r dRow
			if err := rows.Scan(&r.kind, &r.key, &r.expected, &r.actual, &r.status, &r.run); err != nil {
				return err
			}
			out = append(out, r)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func holdAccount(t *testing.T, pool *db.Pool, f fixture) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerWithdrawalHold, "EUR")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// strandHoldD is the owner tamper plant of a hold credit that NO request names (key and correlation
// of no request): invariant #12 drift of `amount` on the fixture wallet.
func strandHoldD(t *testing.T, pool *db.Pool, f fixture, amount int64) {
	t.Helper()
	wdpath.SkipOnPersistentDatabase(t, pool)
	hold := holdAccount(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ownerTamperHoldLegs(ctx, tx, func() error {
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalRequested,
				IdempotencyKey: "d-stranded-" + uuid.NewString(), CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: f.cashAccountID, Direction: ledger.Debit, Amount: amount},
					{LedgerAccountID: hold, Direction: ledger.Credit, Amount: amount},
				},
			})
			return err
		})
	}); err != nil {
		t.Fatalf("owner tamper plant (stranded hold): %v", err)
	}
}

// ownerRelabel changes a request's state with the 0132 state guard and check A disabled for that one
// statement (owner tamper plant; both re-enabled in the same transaction).
func ownerRelabel(t *testing.T, pool *db.Pool, f fixture, id uuid.UUID, set string) {
	t.Helper()
	wdpath.SkipOnPersistentDatabase(t, pool)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_ledger_link_at_commit`,
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_approved_hold_freeze`,
			`UPDATE withdrawal_requests SET ` + set + ` WHERE id = '` + id.String() + `'`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_ledger_link_at_commit`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_approved_hold_freeze`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("owner tamper relabel: %v", err)
	}
}

func hasHoldAlert(rows []alertinject.Row) bool {
	for _, r := range rows {
		if r.Discriminator == discLedgerWithdrawalHold {
			return true
		}
	}
	return false
}

// Healthy tenant: requests in every reachable state (non-terminal and terminal, both release
// counter-accounts) reconcile clean, raise nothing, and D read what it had to (probe counts).
func TestADR0113D_HealthyTenantIsClean(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	for _, st := range []withdrawal.State{withdrawal.StateRequested, withdrawal.StatePendingReview, withdrawal.StateApproved,
		withdrawal.StateSubmitted, withdrawal.StateCancelled, withdrawal.StateRejected, withdrawal.StateCompleted, withdrawal.StateFailed} {
		dWithdrawal(t, pool, f, 10, st)
	}
	o := dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusClean {
		t.Fatalf("a healthy tenant must reconcile clean: status=%s err=%v", o.Run.Status, o.Err)
	}
	if rows := dMismatches(t, pool, f); len(rows) != 0 {
		t.Fatalf("no D finding expected: %+v", rows)
	}
	if rows := alertinject.ForSubject(t, pool, f.tenantID); len(rows) != 0 {
		t.Fatalf("a clean run raises nothing: %+v", rows)
	}
	// Non-vacuity: the probe saw the tenant's requests, hold transactions and hold entries.
	if err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var v withdrawalHoldVisibility
		var k, key, e, a *string
		if err := tx.QueryRow(ctx, withdrawalHoldCheckSQL, f.tenantID).Scan(&v.SessionOK, &v.Requests, &v.E0bTxs, &v.HoldEntries,
			&v.HoldProjectionRowsWithPostings, &k, &key, &e, &a); err != nil {
			return err
		}
		if !v.SessionOK || v.Requests != 8 || v.E0bTxs != 8 || v.HoldEntries != 12 || k != nil {
			return fmt.Errorf("probe %+v kind=%v", v, k)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Seeded drift (a stranded hold credit no request names): withdrawal_hold_open_mismatch on the hold
// account, P1 under the D discriminator (never the projection-drift one: the projection agrees with
// the ledger), standing (the next run re-raises: ONE open alert, two occurrences) and append-only
// (each run appends its own row; the first run's row is unchanged).
func TestADR0113D_SeededDriftDetected_Standing_AppendOnly(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	dWithdrawal(t, pool, f, 40, withdrawal.StateRequested) // a healthy open request on the same wallet
	strandHoldD(t, pool, f, 700)
	hold := holdAccount(t, pool, f)

	o1 := dSweep(t, rt, f)
	if o1.Err != nil || o1.Run.Status != StatusMismatchesFound {
		t.Fatalf("seeded drift must be found: status=%s err=%v", o1.Run.Status, o1.Err)
	}
	rows := dMismatches(t, pool, f)
	if len(rows) != 1 || rows[0].kind != string(MismatchKindWithdrawalHoldOpenMismatch) || rows[0].key != hold.String() ||
		!strings.Contains(rows[0].expected, "= 40 (requests=1)") || !strings.Contains(rows[0].actual, "= 740 ") || rows[0].status != "open" {
		t.Fatalf("one open-hold mismatch on the hold account (expected 40, rebuilt 740): %+v", rows)
	}
	o2 := dSweep(t, rt, f)
	if o2.Run.Status != StatusMismatchesFound {
		t.Fatalf("the finding is standing: %s", o2.Run.Status)
	}
	rows2 := dMismatches(t, pool, f)
	if len(rows2) != 2 || rows2[0] != rows[0] || rows2[1].run != o2.Run.ID || rows2[0].run != o1.Run.ID {
		t.Fatalf("each run appends its own finding row; the first is unchanged: %+v", rows2)
	}
	alerts := alertinject.ForSubject(t, pool, f.tenantID)
	r := mustOne(t, alerts, alerting.KindReconciliationLedgerProjectionDrift, discLedgerWithdrawalHold)
	if r.Occurrences != 2 || r.Attributes["run_id"] != o1.Run.ID.String() {
		t.Fatalf("a persisting condition is ONE open alert with 2 occurrences, run id of the first raise: %+v", r)
	}
	for _, a := range alerts {
		if a.Discriminator == discLedgerVsProjection {
			t.Fatalf("a hold invariant finding must not be raised as projection drift: %+v", a)
		}
	}
	// Audit once per run (the stream's own sweep_run row), never per finding.
	var audits int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run'
			AND metadata->>'stream' = 'ledger_vs_projection'`, f.tenantID).Scan(&audits)
	}); err != nil || audits != 2 {
		t.Fatalf("one ledger_vs_projection audit row per run: %d err=%v", audits, err)
	}
}

// Link rules (check A, read-only) with parity against the 0132 function: each owner-tampered request
// is reported withdrawal_request_ledger_link_invalid with the SAME code withdrawal_request_ledger_link_
// violation returns for it, plus the aggregate finding where the balance drifts.
func TestADR0113D_LinkRules_ParityWithCheckA(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	// WD033: cancelled without a release (the hold stays: aggregate drift too).
	cancelled := dWithdrawal(t, pool, f, 11, withdrawal.StateRequested)
	ownerRelabel(t, pool, f, cancelled, `state = 'cancelled'`)
	// WD033: a non-terminal request carrying a release link (borrowed from a cancelled sibling).
	done := dWithdrawal(t, pool, f, 12, withdrawal.StateCancelled)
	var rel uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT release_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, done).Scan(&rel)
	}); err != nil {
		t.Fatal(err)
	}
	open := dWithdrawal(t, pool, f, 13, withdrawal.StateRequested)
	ownerRelabel(t, pool, f, open, `release_ledger_transaction_id = '`+rel.String()+`'`)
	// WD032: the hold link re-pointed to another request's hold transaction.
	other := dWithdrawal(t, pool, f, 14, withdrawal.StateRequested)
	var otherHold uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT hold_ledger_transaction_id FROM withdrawal_requests WHERE id = $1`, other).Scan(&otherHold)
	}); err != nil {
		t.Fatal(err)
	}
	repointed := dWithdrawal(t, pool, f, 15, withdrawal.StateRequested)
	ownerRelabel(t, pool, f, repointed, `hold_ledger_transaction_id = '`+otherHold.String()+`'`)
	// WD031: the hold link cleared.
	unlinked := dWithdrawal(t, pool, f, 16, withdrawal.StateRequested)
	ownerRelabel(t, pool, f, unlinked, `hold_ledger_transaction_id = NULL`)
	// reversed: no shape exists (WD033).
	rev := dWithdrawal(t, pool, f, 17, withdrawal.StateCompleted)
	ownerRelabel(t, pool, f, rev, `state = 'reversed'`)
	// WD033 (release shape only): rejected legally (release key `:rejected`), relabelled `cancelled`, whose
	// release must be keyed `:cancelled`; the entries are the right two legs, so only the shape rule sees it.
	relabelled := dWithdrawal(t, pool, f, 19, withdrawal.StateRejected)
	ownerRelabel(t, pool, f, relabelled, `state = 'cancelled'`)
	// WD034: a third correlated hold leg (owner, B-i / B-ii disabled) on an otherwise healthy request.
	third := dWithdrawal(t, pool, f, 18, withdrawal.StateRequested)
	hold := holdAccount(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ownerTamperHoldLegs(ctx, tx, func() error {
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalRequested,
				IdempotencyKey: third.String() + ":planted", CorrelationID: third,
				Entries: []ledger.EntryInput{
					{LedgerAccountID: f.cashAccountID, Direction: ledger.Debit, Amount: 18},
					{LedgerAccountID: hold, Direction: ledger.Credit, Amount: 18},
				},
			})
			return err
		})
	}); err != nil {
		t.Fatalf("owner tamper plant (third correlated hold leg): %v", err)
	}

	o := dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusMismatchesFound {
		t.Fatalf("tampered requests must be found: %s err=%v", o.Run.Status, o.Err)
	}
	got := map[string]string{}
	open1 := 0
	for _, r := range dMismatches(t, pool, f) {
		switch r.kind {
		case string(MismatchKindWithdrawalRequestLedgerLinkInvalid):
			got[r.key] = r.actual
		case string(MismatchKindWithdrawalHoldOpenMismatch):
			open1++
		}
	}
	for name, id := range map[string]uuid.UUID{"cancelled without release": cancelled, "release on non-terminal": open,
		"re-pointed hold": repointed, "hold link cleared": unlinked, "reversed": rev, "third correlated hold leg": third,
		"terminal relabel with the wrong release key": relabelled} {
		var v []string
		if err := rt.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT withdrawal_request_ledger_link_violation($1, $2)`, id, f.tenantID).Scan(&v)
		}); err != nil {
			t.Fatal(err)
		}
		if len(v) == 0 {
			t.Fatalf("%s: check A must flag the plant (non-vacuity)", name)
		}
		if want := "violation=" + v[0] + " "; !strings.HasPrefix(got[id.String()], want) {
			t.Fatalf("%s: D must report check A's code %s, got %q", name, v[0], got[id.String()])
		}
	}
	// Healthy rows next to the plants are not flagged: the cancelled request whose release another row
	// borrowed, and the request whose hold transaction another row re-pointed to (its own links hold).
	for name, id := range map[string]uuid.UUID{"healthy cancelled": done, "healthy open": other} {
		if a, ok := got[id.String()]; ok {
			t.Fatalf("%s must not be flagged: %s", name, a)
		}
	}
	if open1 != 1 {
		t.Fatalf("the wallet's aggregate drifts (cancelled-without-release hold, third leg): one open-hold finding, got %d", open1)
	}
}

// D is rebuilt FROM ledger_entries, never from the projection: a tampered hold PROJECTION row (the
// ledger is consistent) is ledger_vs_projection's balance_mismatch only, never a D finding; and a hold
// ledger drift with a projection that agrees with the ledger is a D finding only.
func TestADR0113D_RebuiltFromLedger_NotFromTheProjection(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	dWithdrawal(t, pool, f, 25, withdrawal.StateRequested)
	hold := holdAccount(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE wallet_balance_projection SET credit_total = credit_total + 500 WHERE ledger_account_id = $1`, hold)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	o := dSweep(t, rt, f)
	if o.Run.Status != StatusMismatchesFound {
		t.Fatalf("the projection tamper is drift: %s err=%v", o.Run.Status, o.Err)
	}
	if rows := dMismatches(t, pool, f); len(rows) != 0 {
		t.Fatalf("a projection-only tamper must not be a D finding (D reads ledger_entries): %+v", rows)
	}
	alerts := alertinject.ForSubject(t, pool, f.tenantID)
	mustOne(t, alerts, alerting.KindReconciliationLedgerProjectionDrift, discLedgerVsProjection)
	if hasHoldAlert(alerts) {
		t.Fatalf("no D alert for a projection-only tamper: %+v", alerts)
	}
}

// C7 fail closed: a blind read is a RUN ERROR, never `clean`. (1) the session is not exactly the
// tenant scope; (2)-(4) a table goes RLS-blind (a RESTRICTIVE policy created inside the owner's own
// rolled-back transaction) while the owner-side count of the same rows is not zero.
func TestADR0113D_BlindRead_FailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	dWithdrawal(t, pool, f, 10, withdrawal.StateRequested)
	dWithdrawal(t, pool, f, 20, withdrawal.StateCancelled)
	ctx := context.Background()

	run := func(tx pgx.Tx) error {
		_, _, err := RunLedgerVsProjection(ctx, tx, f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	}
	// (0) a session of ANOTHER tenant: every count of this tenant reads zero, consistently, so only
	// the session probe can tell (without it the run would read nothing and report on nothing).
	other := seedFixture(t, pool)
	err := pool.WithTenant(ctx, other.tenantID, func(ctx context.Context, tx pgx.Tx) error { return run(tx) })
	if !errors.Is(err, ErrWithdrawalHoldCheckBlind) || !strings.Contains(err.Error(), "not exactly the tenant scope") {
		t.Fatalf("another tenant's session must fail closed on the session probe: %v", err)
	}
	// (1) a player-scoped session (sees the player's own rows only).
	err = pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.player_account_id', $1, true)`, f.playerAccountID.String()); err != nil {
			return err
		}
		return run(tx)
	})
	if !errors.Is(err, ErrWithdrawalHoldCheckBlind) {
		t.Fatalf("a player-scoped session must fail closed: %v", err)
	}

	for _, tc := range []struct{ table, want string }{
		{"withdrawal_requests", "no withdrawal request is visible"},
		{"ledger_transactions", "no withdrawal_requested hold transaction is visible"},
		{"ledger_entries", "no player_withdrawal_hold ledger entry is visible"},
	} {
		tx, err := pool.Raw().Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
			t.Fatal(err)
		}
		var owner int64
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+tc.table+` WHERE tenant_id = $1`, f.tenantID).Scan(&owner); err != nil || owner == 0 {
			t.Fatalf("owner-side count of %s must be non-zero (non-vacuity): %d err=%v", tc.table, owner, err)
		}
		if _, err := tx.Exec(ctx, `CREATE POLICY adr0113d_blind ON `+tc.table+` AS RESTRICTIVE FOR SELECT USING (tenant_id <> '`+f.tenantID.String()+`')`); err != nil {
			t.Fatal(err)
		}
		err = run(tx)
		_ = tx.Rollback(ctx)
		if !errors.Is(err, ErrWithdrawalHoldCheckBlind) || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s blind: the run must fail closed (%q), got %v", tc.table, tc.want, err)
		}
	}
}

// C7 end to end through the scheduler: a tenant whose withdrawal_requests are RLS-blind to the run
// gets a FAILED run (no `clean` run row is recorded for the attempt), the failure audit row and the
// reconciliation.run_failed P1. The blinding policy names ONLY this test's tenant and is dropped at
// cleanup.
func TestADR0113D_BlindRead_ThroughTheSweep_RunFailedP1(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	dWithdrawal(t, pool, f, 10, withdrawal.StateRequested)
	ctx := context.Background()
	name := "adr0113d_blind_" + strings.ReplaceAll(f.tenantID.String()[:8], "-", "")
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `CREATE POLICY `+name+` ON withdrawal_requests AS RESTRICTIVE FOR SELECT USING (tenant_id <> '`+f.tenantID.String()+`')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `DROP POLICY IF EXISTS `+name+` ON withdrawal_requests`)
			return err
		})
	})
	o := dSweep(t, rt, f)
	if !errors.Is(o.Err, ErrWithdrawalHoldCheckBlind) || o.Run.Status == StatusClean {
		t.Fatalf("a blind read must be a run error, never clean: status=%q err=%v", o.Run.Status, o.Err)
	}
	var runs, failed int
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1 AND stream = 'ledger_vs_projection'`, f.tenantID).Scan(&runs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
			AND metadata->>'stream' = 'ledger_vs_projection'`, f.tenantID).Scan(&failed)
	}); err != nil || runs != 0 || failed != 1 {
		t.Fatalf("no ledger run row recorded, one failure audit: runs=%d failed=%d err=%v", runs, failed, err)
	}
	mustOne(t, alertinject.ForSubject(t, pool, f.tenantID), alerting.KindReconciliationRunFailed, "stream:ledger_vs_projection")
}

// A non-active tenant is observed: a CLOSED tenant (closed before the LA025 arm, launchfix.ForceClosed)
// that still holds a stranded hold gets D through the observation sweep, with the same P1.
func TestADR0113D_NonActiveTenantObserved(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	strandHoldD(t, pool, f, 300)
	if err := launchfix.ForceClosed(context.Background(), t, pool, f.tenantID, nil); err != nil {
		t.Fatal(err)
	}
	o := dSweep(t, rt, f)
	if !o.ObservationOnly || o.TenantStatus != "closed" || o.Err != nil || o.Run.Status != StatusMismatchesFound {
		t.Fatalf("a closed tenant is observed and D finds the stranded hold: %+v", o)
	}
	if rows := dMismatches(t, pool, f); len(rows) != 1 || rows[0].kind != string(MismatchKindWithdrawalHoldOpenMismatch) {
		t.Fatalf("one open-hold finding: %+v", rows)
	}
	mustOne(t, alertinject.ForSubject(t, pool, f.tenantID), alerting.KindReconciliationLedgerProjectionDrift, discLedgerWithdrawalHold)
}

// Concurrency with live withdrawals (C7): requests are created (E0 + E0b) and released (cancel, reject,
// submit + fail) continuously while the ledger stream runs repeatedly on the runtime role. Every run
// must be clean: the single-statement snapshot never sees a request without its leg or a leg without
// its request state, so no false P1.
func TestADR0113D_ConcurrentLiveWithdrawals_NoFalseP1(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var made atomic.Int64
	errs := make(chan error, 8)
	targets := []withdrawal.State{withdrawal.StateCancelled, withdrawal.StateRejected, withdrawal.StateFailed, withdrawal.StateRequested}
	// A real verified binding, so every writer runs as igaming_runtime with no owner DDL (the legacy
	// binding-guard plant takes an ACCESS EXCLUSIVE table lock and would deadlock the writers).
	inst := pitest.Bind(t, pool, f.tenantID, f.playerAccountID, "EUR")
	var fp string
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT fingerprint FROM payout_instruments WHERE id = $1`, inst).Scan(&fp)
	}); err != nil {
		t.Fatal(err)
	}
	for g := 0; g < 3; g++ {
		g := g
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				spec := wdpath.Spec{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
					AssetCode: "EUR", Amount: int64(1 + i%7), IdempotencyKey: fmt.Sprintf("dconc-%d-%d-%s", g, i, uuid.NewString()),
					InstrumentID: &inst, Fingerprint: &fp}
				var res wdpath.Result
				if err := rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
					var err error
					res, err = wdpath.Plant(ctx, tx, spec)
					return err
				}); err != nil {
					errs <- err
					return
				}
				if tgt := targets[(g+i)%len(targets)]; tgt != withdrawal.StateRequested {
					if err := rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
						return wdpath.Advance(ctx, tx, res.ID, tgt, spec)
					}); err != nil {
						errs <- err
						return
					}
				}
				made.Add(1)
			}
		}()
	}
	runs := 0
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		o := runLedgerStreamForTenant(ctx, rt, nil, f.tenantID, time.Now().Add(-time.Hour), time.Now(), "")
		if o.Err != nil {
			close(stop)
			wg.Wait()
			t.Fatalf("run %d failed: %v", runs, o.Err)
		}
		if !o.Skipped {
			if o.Run.Status != StatusClean {
				close(stop)
				wg.Wait()
				t.Fatalf("FALSE P1 during live withdrawals (run %d): %s %+v", runs, o.Run.Status, dMismatches(t, pool, f))
			}
			runs++
		}
	}
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("withdrawal writer: %v", err)
	}
	if runs < 5 || made.Load() < 20 {
		t.Fatalf("the race must be exercised: %d runs, %d withdrawals", runs, made.Load())
	}
	if hasHoldAlert(alertinject.ForSubject(t, pool, f.tenantID)) {
		t.Fatal("no D alert may be raised for a healthy tenant under load")
	}
	t.Logf("%d clean runs against %d live withdrawals", runs, made.Load())
}
