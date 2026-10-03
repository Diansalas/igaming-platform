//go:build integration

// ADR 0102 I-wire rows 3-7 and 14 (ALERT-DELIVERY-1): every reconciliation P1
// site raises a durable alert in addition to its log line. These tests assert
// on durable alert rows, never on log output, and prove the financial rules:
//   - the run and its mismatch rows commit even when the raise fails
//     persistently (LF test 6);
//   - the discriminators are stable "stream:" keys with the run id as an
//     attribute only (LF test 7), so a persisting condition is ONE open alert;
//   - the K2 unlinked-adjustment finding is never presented as projection
//     drift (C-K2-1).
package reconciliation

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

func mustOne(t *testing.T, rows []alertinject.Row, kind alerting.Kind, discriminator string) alertinject.Row {
	t.Helper()
	var hit []alertinject.Row
	for _, r := range rows {
		if r.Kind == string(kind) && r.Discriminator == discriminator {
			hit = append(hit, r)
		}
	}
	if len(hit) != 1 {
		t.Fatalf("want exactly one %s alert with discriminator %q, got %d (all alerts: %+v)", kind, discriminator, len(hit), rows)
	}
	if hit[0].Severity != "p1" {
		t.Fatalf("%s must be p1, got %s", kind, hit[0].Severity)
	}
	return hit[0]
}

func TestIWire_Recon_CleanRunRaisesNothing(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	now := time.Now()
	out, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatal(err)
	}
	if o := findOutcome(t, out, f); o.Run.Status != StatusClean {
		t.Fatalf("precondition: expected a clean run, got %s", o.Run.Status)
	}
	if rows := alertinject.ForSubject(t, pool, f.tenantID); len(rows) != 0 {
		t.Fatalf("a clean sweep must raise no alert, got %+v", rows)
	}
}

// Row 3: drift P1; stable key; N sweeps of a persisting condition are ONE open
// alert with growing occurrences (LF test 7).
func TestIWire_Recon_LedgerDriftP1_StableKey(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE wallet_balance_projection SET credit_total = credit_total + 999 WHERE ledger_account_id = $1`, f.cashAccountID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var runIDs []uuid.UUID
	for i := 0; i < 2; i++ {
		now := time.Now()
		out, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now,
			sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
		if err != nil {
			t.Fatal(err)
		}
		o := findOutcome(t, out, f)
		if o.Run.Status != StatusMismatchesFound {
			t.Fatalf("expected drift to be detected, got %s", o.Run.Status)
		}
		runIDs = append(runIDs, o.Run.ID)
	}
	rows := alertinject.ForSubject(t, pool, f.tenantID)
	r := mustOne(t, rows, alerting.KindReconciliationLedgerProjectionDrift, discLedgerVsProjection)
	if r.Occurrences != 2 {
		t.Fatalf("a persisting condition must be one open alert with 2 occurrences, got %d", r.Occurrences)
	}
	if r.Attributes["run_id"] != runIDs[0].String() {
		t.Fatalf("run_id is an attribute of the first raise, want %s got %v", runIDs[0], r.Attributes["run_id"])
	}
	if c, _ := r.Attributes["mismatch_count"].(float64); c < 1 {
		t.Fatalf("mismatch_count must be recorded, got %v", r.Attributes["mismatch_count"])
	}
	if len(alertinject.Find(rows, string(alerting.KindReconciliationLedgerProjectionDrift))) != 1 {
		t.Fatalf("pure drift must not also raise the unlinked-adjustment alert: %+v", rows)
	}
}

// C-K2-1: an unlinked manual adjustment is a governance breach, never
// "projection drift": a distinct discriminator under the drift Kind, and the
// drift discriminator is NOT raised for it.
func TestIWire_Recon_UnlinkedAdjustmentIsNotDrift(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	sbFund(t, pool, f, 700) // a manual_adjustment posted outside any governed request
	now := time.Now()
	out, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatal(err)
	}
	if o := findOutcome(t, out, f); o.Run.Status != StatusMismatchesFound {
		t.Fatalf("expected the unlinked adjustment to be found, got %s", o.Run.Status)
	}
	rows := alertinject.ForSubject(t, pool, f.tenantID)
	mustOne(t, rows, alerting.KindReconciliationLedgerProjectionDrift, discLedgerUnlinkedManualAdjustment)
	for _, r := range rows {
		if r.Discriminator == discLedgerVsProjection {
			t.Fatalf("an unlinked adjustment must never be raised as projection drift: %+v", r)
		}
	}
}

// Row 4.
func TestIWire_Recon_SportsbookSettlementP1(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	betID := uuid.New()
	src := divergentSource{mutate: func(ls []sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine {
		return append(ls, sportsbook.SettlementStatementLine{BetID: betID, Void: true, AssetCode: "EUR"})
	}}
	now := time.Now()
	out, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, now.Add(-time.Hour), now, src, casino.MockStatementSource{})
	if err != nil {
		t.Fatal(err)
	}
	if o := findOutcome(t, out, f); o.Sportsbook.Run.Status != StatusMismatchesFound {
		t.Fatalf("expected a sportsbook mismatch, got %s", o.Sportsbook.Run.Status)
	}
	r := mustOne(t, alertinject.ForSubject(t, pool, f.tenantID), alerting.KindReconciliationSportsbookSettlement, "stream:sportsbook_settlement")
	if r.Attributes["statement_source"] != src.Label() {
		t.Fatalf("statement_source attribute: %v", r.Attributes)
	}
}

// Row 5.
func TestIWire_Recon_CasinoConsistencyP1(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	cash, house := w.accounts(t, w.f.walletID)
	w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoWin,
		ProviderID: strp(casProvider), ProviderTxID: strp("iw-orphan"), CorrelationID: corr(w.f.tenantID, "iw-none"),
		Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 5}, {LedgerAccountID: cash, Direction: ledger.Credit, Amount: 5}}})
	now := time.Now()
	out, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatal(err)
	}
	if o := findOutcome(t, out, w.f); o.Casino.Run.Status != StatusMismatchesFound {
		t.Fatalf("expected a casino consistency mismatch, got %s", o.Casino.Run.Status)
	}
	mustOne(t, alertinject.ForSubject(t, pool, w.f.tenantID), alerting.KindReconciliationCasinoConsistency, "stream:casino_consistency")
}

// Row 6 (REPEATABLE READ): the raise is post-commit. LF test 6: with a
// PERSISTENT failure injected into the alert path, the run and its mismatch
// rows still commit and the sweep result is unchanged.
func TestIWire_Recon_CasinoStatementP1_PostCommit_RunCommitsEvenWhenRaiseFailsPersistently(t *testing.T) {
	for _, tc := range []struct {
		name    string
		inject  bool
		errcode string
	}{{"raise_ok", false, ""}, {"persistent_P0001", true, "P0001"}, {"persistent_deadlock_40P01", true, "40P01"}} {
		t.Run(tc.name, func(t *testing.T) {
			pool := testPool(t)
			w := newCasWorld(t, pool)
			w.buildCleanWorld(t)
			if tc.inject {
				alertinject.Install(t, pool, w.f.tenantID, alertinject.Persistent, tc.errcode)
			}
			now := time.Now()
			out, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{w.f.tenantID}, now.Add(-time.Hour), now,
				sportsbook.MockSettlementStatementSource{}, casMutateLine("b1", func(l *statement.CasinoStatementLine) { l.Amount = 3 }))
			if err != nil {
				t.Fatal(err)
			}
			o := findOutcome(t, out, w.f)
			if o.CasinoStatement.Err != nil || o.CasinoStatement.Run.Status != StatusMismatchesFound {
				t.Fatalf("the run must commit with mismatches regardless of the alert path: %+v", o.CasinoStatement)
			}
			var persisted int
			if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE reconciliation_run_id = $1`, o.CasinoStatement.Run.ID).Scan(&persisted)
			}); err != nil || persisted == 0 {
				t.Fatalf("mismatch rows must be durable (n=%d err=%v)", persisted, err)
			}
			rows := alertinject.ForSubject(t, pool, w.f.tenantID)
			got := alertinject.Find(rows, string(alerting.KindReconciliationCasinoStatement))
			if tc.inject {
				if len(got) != 0 {
					t.Fatalf("with a persistent failure no tenant-visible statement alert can exist, got %+v", got)
				}
				return
			}
			r := mustOne(t, rows, alerting.KindReconciliationCasinoStatement, "stream:casino_statement")
			if r.Attributes["statement_source"] != casDivergentLabel {
				t.Fatalf("statement_source attribute: %v", r.Attributes)
			}
		})
	}
}

// Row 7 (REPEATABLE READ).
func TestIWire_Recon_PaymentStatementP1_PostCommit(t *testing.T) {
	w := newPayWorld(t)
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, "iw-unknown-1", "", statement.PaymentLineDeposit, statement.PaymentStatusSucceeded, 1))}
	out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src, PaymentStatementOptions{})
	if out.Err != nil || out.Run.Status != StatusMismatchesFound {
		t.Fatalf("expected a payment_statement mismatch, got %+v", out)
	}
	r := mustOne(t, alertinject.ForSubject(t, w.pool, w.f.tenantID), alerting.KindReconciliationPaymentStatement,
		"stream:payment_statement:provider:"+payProvA)
	if r.Attributes["import_id"] == nil || r.Attributes["statement_source"] == nil {
		t.Fatalf("import_id and statement_source attributes are required: %v", r.Attributes)
	}
}

// Row 14: the failure path. The run transaction rolled back, so run_failed is
// a detached raise; never the error text.
func TestIWire_Recon_RunFailedP1_DetachedAndNoErrorText(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	now := time.Now()
	// end before start violates the run CHECK: a 23xxx failure inside the tx.
	if _, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{f.tenantID}, now, now.Add(-time.Hour),
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{}); err != nil {
		t.Fatal(err)
	}
	rows := alertinject.ForSubject(t, pool, f.tenantID)
	failed := alertinject.Find(rows, string(alerting.KindReconciliationRunFailed))
	if len(failed) == 0 {
		t.Fatalf("expected reconciliation.run_failed alerts, got %+v", rows)
	}
	var sawLedger bool
	for _, r := range failed {
		if r.Severity != "p1" {
			t.Fatalf("run_failed must be p1: %+v", r)
		}
		if r.Discriminator == "stream:ledger_vs_projection" {
			sawLedger = true
			if r.Attributes["sqlstate_class"] != "23" || r.Attributes["phase"] != "run" {
				t.Fatalf("attributes: %v", r.Attributes)
			}
		}
		for k, v := range r.Attributes {
			if s, ok := v.(string); ok && (strings.Contains(s, "violates") || strings.Contains(s, "constraint")) {
				t.Fatalf("attribute %s carries raw error text: %q", k, s)
			}
		}
	}
	if !sawLedger {
		t.Fatalf("the ledger_vs_projection failure must raise run_failed: %+v", failed)
	}
}

func TestIWire_Recon_PaymentStatementRunFailedP1_PerProvider(t *testing.T) {
	w := newPayWorld(t)
	src := payFixedSource{provider: payProvA, stmt: wideCoverage(
		payLineFor(payProvA, "r1", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1),
		payLineFor(payProvA, "r2", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1),
		payLineFor(payProvA, "r3", "", statement.PaymentLineDeposit, statement.PaymentStatusPending, 1))}
	out := ReconcilePaymentStatementForTenant(context.Background(), w.pool, nil, w.f.tenantID, time.Now(), time.Now(), src, PaymentStatementOptions{MaxLines: 2})
	if out.Err == nil {
		t.Fatal("expected the over-cap statement to fail")
	}
	r := mustOne(t, alertinject.ForSubject(t, w.pool, w.f.tenantID), alerting.KindReconciliationRunFailed,
		"stream:payment_statement:provider:"+payProvA)
	if r.Attributes["phase"] != "fetch" {
		t.Fatalf("phase attribute: %v", r.Attributes)
	}
}
