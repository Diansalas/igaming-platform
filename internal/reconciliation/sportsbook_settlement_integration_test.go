//go:build integration

// Stage 10 W1 - the sportsbook_settlement reconciliation stream (ADR 0088
// §8): clean data after every real settle/void/rollback/tombstone flow
// reconciles to zero mismatches; drift injected per mismatch kind (in a
// scratch database, as its owner, with the relevant deny/validation
// trigger disabled where one would otherwise refuse the drift) is
// detected; a divergent statement fed through the injectable source
// (ADR 0088 §14 Q3(a)) is detected as sb_mock_statement_mismatch.
package reconciliation

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

const sbStake = int64(100)

// sbWorld is one tenant with every W1 lifecycle shape applied through the
// real sportsbook.SimulateSettlementEvent path.
type sbWorld struct {
	f     fixture
	actor uuid.UUID
	bets  map[string]sportsbook.Bet
	// settlementLedgerTx maps "<name>#g" to the settlement's ledger tx id.
	settlementLedgerTx map[string]uuid.UUID
}

func sbScratchPool(t *testing.T) *db.Pool {
	t.Helper()
	url := scratchdb.New(t, "recon_sb_")
	pool, err := db.Connect(context.Background(), url, 10, 5_000_000_000)
	if err != nil {
		t.Fatalf("connect to scratch database: %v", err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.MigrateUp(context.Background(), "../../migrations"); err != nil {
		t.Fatalf("migrate scratch database: %v", err)
	}
	return pool
}

func sbFund(t *testing.T, pool *db.Pool, f fixture, amount int64) {
	t.Helper()
	reason := "test fixture funding"
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		adj, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, nil, ledger.AccountManualAdjustment, "EUR")
		if err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: f.tenantID, TransactionType: ledger.TxManualAdjustment,
			IdempotencyKey: "fund-" + uuid.NewString(), CorrelationID: uuid.New(), ReasonCode: &reason,
			Entries: []ledger.EntryInput{
				{LedgerAccountID: adj, Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: f.cashAccountID, Direction: ledger.Credit, Amount: amount},
			},
		})
		return err
	})
	if err != nil {
		t.Fatalf("fund wallet: %v", err)
	}
}

func sbSeedSelection(t *testing.T, pool *db.Pool) sportsbook.Selection {
	t.Helper()
	ref := uuid.NewString()[:8]
	sel := sportsbook.Selection{OddsNumerator: 250, OddsDenominator: 100}
	err := pool.WithPlatformService(context.Background(), db.ServiceSportsbookCatalogueSync, func(ctx context.Context, tx pgx.Tx) error {
		var sportID, compID, eventID uuid.UUID
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_sports (external_ref, code, name) VALUES ($1, $1, 'Recon Sport') RETURNING id`,
			"recon-sport-"+ref).Scan(&sportID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_competitions (sport_id, external_ref, name) VALUES ($1, $2, 'Recon Competition') RETURNING id`,
			sportID, "recon-comp-"+ref).Scan(&compID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_events (competition_id, external_ref, name, start_time, status) VALUES ($1, $2, 'Recon Event', $3, 'scheduled') RETURNING id`,
			compID, "recon-event-"+ref, time.Now().Add(48*time.Hour)).Scan(&eventID); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`INSERT INTO sb_markets (event_id, external_ref, name, status) VALUES ($1, $2, 'Recon Market', 'open') RETURNING id`,
			eventID, "recon-market-"+ref).Scan(&sel.MarketID); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`INSERT INTO sb_selections (market_id, external_ref, name, odds_numerator, odds_denominator, status)
			 VALUES ($1, $2, 'Recon Selection', $3, $4, 'active') RETURNING id`,
			sel.MarketID, "recon-sel-"+ref, sel.OddsNumerator, sel.OddsDenominator).Scan(&sel.ID)
	})
	if err != nil {
		t.Fatalf("seed selection: %v", err)
	}
	return sel
}

func sbPlace(t *testing.T, pool *db.Pool, f fixture, sel sportsbook.Selection) sportsbook.Bet {
	t.Helper()
	var res sportsbook.PlaceBetResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = sportsbook.PlaceBet(ctx, tx, sportsbook.PlaceBetParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID,
			SelectionID: sel.ID, AssetCode: "EUR", StakeAmount: sbStake,
			ExpectedOddsNumerator: sel.OddsNumerator, ExpectedOddsDenominator: sel.OddsDenominator,
			IdempotencyKey: "recon-sb-" + uuid.NewString(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if !res.Accepted {
		t.Fatalf("place bet rejected: %s/%s %s", res.RejectionCategory, res.RejectionCode, res.RejectionMessage)
	}
	return res.Bet
}

func sbStaff(t *testing.T, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		s, err := identity.CreateStaffUser(ctx, tx, tenantID, "rm-"+uuid.NewString()[:8]+"@example.test", "x", identity.StaffRoleRiskManager, nil)
		id = s.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed risk manager: %v", err)
	}
	return id
}

func (w *sbWorld) simulate(t *testing.T, pool *db.Pool, name string, ev sportsbook.SettlementEvent) sportsbook.SettlementResult {
	t.Helper()
	bet := w.bets[name]
	ev.TenantID, ev.BetID, ev.ActorStaffID = w.f.tenantID, bet.ID, w.actor
	if ev.EventType == sportsbook.SettlementEventSettle {
		ev.ClaimAssetCode = bet.AssetCode
		if ev.Outcome == sportsbook.SettlementOutcomeWon {
			ev.ClaimPayoutAmount = bet.PotentialReturn
		}
	}
	var res sportsbook.SettlementResult
	err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = sportsbook.SimulateSettlementEvent(ctx, tx, ev)
		return err
	})
	if err != nil {
		t.Fatalf("simulate %s on %s: %v", ev.EventType, name, err)
	}
	if res.Rejected() {
		t.Fatalf("simulate %s on %s: rejected %s", ev.EventType, name, res.RejectionCode)
	}
	if ev.EventType == sportsbook.SettlementEventSettle {
		w.settlementLedgerTx[fmt.Sprintf("%s#%d", name, ev.Generation)] = res.LedgerTransactionIDs[0]
	}
	return res
}

func settle(g int, outcome string) sportsbook.SettlementEvent {
	return sportsbook.SettlementEvent{EventType: sportsbook.SettlementEventSettle, Generation: g, Outcome: outcome}
}

func rollback(g int) sportsbook.SettlementEvent {
	return sportsbook.SettlementEvent{EventType: sportsbook.SettlementEventRollback, Generation: g}
}

func voidEv() sportsbook.SettlementEvent {
	return sportsbook.SettlementEvent{EventType: sportsbook.SettlementEventVoid, VoidReason: "market_cancelled"}
}

// seedSBWorld builds one tenant holding every §2.3 end state and every
// path to it, plus non-sportsbook activity the stream must ignore.
func seedSBWorld(t *testing.T, pool *db.Pool) *sbWorld {
	t.Helper()
	w := &sbWorld{f: seedFixture(t, pool), bets: map[string]sportsbook.Bet{}, settlementLedgerTx: map[string]uuid.UUID{}}
	sbFund(t, pool, w.f, 100_000)
	w.actor = sbStaff(t, pool, w.f.tenantID)
	sel := sbSeedSelection(t, pool)
	for _, name := range []string{"open", "won", "lost", "voidBefore", "wonRolledBack", "resettled",
		"tombThenSettled", "voidAfterWon", "voidAfterLost", "rollbackThenVoid", "tombstoneOnly"} {
		w.bets[name] = sbPlace(t, pool, w.f, sel)
	}
	won, lost := sportsbook.SettlementOutcomeWon, sportsbook.SettlementOutcomeLost

	w.simulate(t, pool, "won", settle(1, won))
	w.simulate(t, pool, "lost", settle(1, lost))
	w.simulate(t, pool, "voidBefore", voidEv())
	w.simulate(t, pool, "wonRolledBack", settle(1, won))
	w.simulate(t, pool, "wonRolledBack", rollback(1))
	w.simulate(t, pool, "resettled", settle(1, lost))
	w.simulate(t, pool, "resettled", rollback(1))
	w.simulate(t, pool, "resettled", settle(2, won))
	if res := w.simulate(t, pool, "tombThenSettled", rollback(1)); res.Result != sportsbook.SettlementResultTombstoned {
		t.Fatalf("expected a tombstone, got %q", res.Result)
	}
	w.simulate(t, pool, "tombThenSettled", settle(2, won))
	w.simulate(t, pool, "voidAfterWon", settle(1, won))
	w.simulate(t, pool, "voidAfterWon", voidEv())
	w.simulate(t, pool, "voidAfterLost", settle(1, lost))
	w.simulate(t, pool, "voidAfterLost", voidEv())
	w.simulate(t, pool, "rollbackThenVoid", settle(1, lost))
	w.simulate(t, pool, "rollbackThenVoid", rollback(1))
	w.simulate(t, pool, "rollbackThenVoid", voidEv())
	w.simulate(t, pool, "tombstoneOnly", rollback(1))
	// Replays post nothing and must not disturb reconciliation either.
	w.simulate(t, pool, "won", settle(1, won))
	w.simulate(t, pool, "tombstoneOnly", rollback(1))

	// Out-of-scope activity the stream must ignore (ADR 0088 §8.1/§8.3
	// scoping): a casino_bet on the same player_locked_cash account, and a
	// tombstone outside the sportsbook_settlement: key namespace.
	err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		locked, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerLockedCash, "EUR")
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxCasinoBet,
			IdempotencyKey: "recon-casino-" + uuid.NewString(), CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: 77},
				{LedgerAccountID: locked, Direction: ledger.Credit, Amount: 77},
			},
		}); err != nil {
			return err
		}
		_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: w.f.tenantID, TransactionType: ledger.TxTombstone,
			IdempotencyKey: "casino_rollback:" + uuid.NewString(), CorrelationID: uuid.New(),
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed out-of-scope activity: %v", err)
	}
	return w
}

func runSB(t *testing.T, pool *db.Pool, tenantID uuid.UUID, source sportsbook.SettlementStatementSource) (Run, []Mismatch) {
	t.Helper()
	if source == nil {
		source = sportsbook.MockSettlementStatementSource{}
	}
	var run Run
	var ms []Mismatch
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, ms, err = RunSportsbookSettlement(ctx, tx, tenantID, time.Now().Add(-time.Hour), time.Now(), source)
		return err
	})
	if err != nil {
		t.Fatalf("RunSportsbookSettlement: %v", err)
	}
	return run, ms
}

func sbDump(ms []Mismatch) string {
	var b strings.Builder
	for _, m := range ms {
		fmt.Fprintf(&b, "\n  %s key=%q expected=%q actual=%q", m.MismatchKind, m.ReconciliationKey, m.ExpectedValue, m.ActualValue)
	}
	return b.String()
}

func requireClean(t *testing.T, pool *db.Pool, tenantID uuid.UUID) {
	t.Helper()
	run, ms := runSB(t, pool, tenantID, nil)
	if run.Status != StatusClean || len(ms) != 0 {
		t.Fatalf("expected a clean sportsbook_settlement run, got %s:%s", run.Status, sbDump(ms))
	}
}

// requireDetected runs the stream and asserts at least one mismatch of
// kind whose key contains keyPart, and that it was persisted.
func requireDetected(t *testing.T, pool *db.Pool, tenantID uuid.UUID, source sportsbook.SettlementStatementSource, kind MismatchKind, keyPart string) []Mismatch {
	t.Helper()
	run, ms := runSB(t, pool, tenantID, source)
	if run.Status != StatusMismatchesFound {
		t.Fatalf("expected mismatches_found, got %s", run.Status)
	}
	found := false
	for _, m := range ms {
		if m.MismatchKind == kind && strings.Contains(m.ReconciliationKey, keyPart) {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a %s mismatch with key containing %q; got:%s", kind, keyPart, sbDump(ms))
	}
	var persisted int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(*) FROM reconciliation_mismatches m JOIN reconciliation_runs r ON r.id = m.reconciliation_run_id
			  WHERE m.reconciliation_run_id = $1 AND m.mismatch_kind = $2 AND r.stream = 'sportsbook_settlement'`,
			run.ID, string(kind)).Scan(&persisted)
	})
	if err != nil {
		t.Fatalf("count persisted mismatches: %v", err)
	}
	if persisted == 0 {
		t.Fatalf("the %s mismatch was not persisted", kind)
	}
	t.Logf("detected %s (all mismatches this run:%s)", kind, sbDump(ms))
	return ms
}

// execOne runs a drift-injecting statement and requires it to touch
// exactly one row (an injection that silently matched nothing - e.g.
// filtered by RLS - would make a detection test vacuous).
func execOne(ctx context.Context, tx pgx.Tx, sql string, args ...any) error {
	tag, err := tx.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("drift statement affected %d rows, want 1: %s", tag.RowsAffected(), sql)
	}
	return nil
}

// withTriggerDisabled runs fn as the table owner inside one tenant-scoped
// transaction with trigger disabled for its duration only (DDL is
// transactional; the trigger is re-enabled before commit).
func withTriggerDisabled(t *testing.T, pool *db.Pool, tenantID uuid.UUID, table, trigger string, fn func(ctx context.Context, tx pgx.Tx) error) {
	t.Helper()
	tbl, trg := pgx.Identifier{table}.Sanitize(), pgx.Identifier{trigger}.Sanitize()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "ALTER TABLE "+tbl+" DISABLE TRIGGER "+trg); err != nil {
			return err
		}
		if err := fn(ctx, tx); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "ALTER TABLE "+tbl+" ENABLE TRIGGER "+trg)
		return err
	})
	if err != nil {
		t.Fatalf("inject drift with %s.%s disabled: %v", table, trigger, err)
	}
}

// TestSportsbookSettlementRecon_CleanAfterEveryLifecycleFlow: every W1
// flow (settle won/lost, void before/after settlement, rollback,
// re-settlement, tombstone, tombstone then settle, rollback then void,
// replays) plus out-of-scope casino activity on the same locked account
// reconciles to zero mismatches, and ledger_vs_projection stays clean.
func TestSportsbookSettlementRecon_CleanAfterEveryLifecycleFlow(t *testing.T) {
	pool := testPool(t)
	w := seedSBWorld(t, pool)
	requireClean(t, pool, w.f.tenantID)

	// The run row is recorded under the new stream name.
	var stream string
	err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT stream FROM reconciliation_runs WHERE tenant_id = $1 ORDER BY run_at DESC LIMIT 1`,
			w.f.tenantID).Scan(&stream)
	})
	if err != nil || stream != string(StreamSportsbookSettlement) {
		t.Fatalf("expected a %s run row, got %q (err %v)", StreamSportsbookSettlement, stream, err)
	}

	// No statement source: the run fails closed (nothing recorded), it
	// never silently skips the statement match.
	err = pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := RunSportsbookSettlement(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), nil)
		return err
	})
	if err == nil {
		t.Fatal("expected RunSportsbookSettlement to fail closed without a statement source")
	}

	err = pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		run, ms, err := RunLedgerVsProjection(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		if err == nil && (run.Status != StatusClean || len(ms) != 0) {
			return fmt.Errorf("ledger_vs_projection not clean: %v", ms)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestSportsbookSettlementRecon_MockStatementLabelAndShape pins the MOCK
// source's rendering: one line per un-reversed settlement, one per void,
// none for open bets (incl. rolled back or tombstone-only).
func TestSportsbookSettlementRecon_MockStatementLabelAndShape(t *testing.T) {
	pool := testPool(t)
	w := seedSBWorld(t, pool)
	src := sportsbook.MockSettlementStatementSource{}
	if !strings.Contains(src.Label(), "MOCK") || !strings.Contains(src.Label(), "PROVIDER DEPENDENT") {
		t.Fatalf("mock source label must say MOCK and PROVIDER DEPENDENT: %q", src.Label())
	}
	var lines []sportsbook.SettlementStatementLine
	err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		lines, err = src.StatementLines(ctx, tx, w.f.tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("statement: %v", err)
	}
	byBet := map[uuid.UUID][]sportsbook.SettlementStatementLine{}
	for _, l := range lines {
		byBet[l.BetID] = append(byBet[l.BetID], l)
	}
	want := map[string]string{
		"open": "", "wonRolledBack": "", "tombstoneOnly": "",
		"won": "won#1", "lost": "lost#1", "resettled": "won#2", "tombThenSettled": "won#2",
		"voidBefore": "void", "voidAfterWon": "void", "voidAfterLost": "void", "rollbackThenVoid": "void",
	}
	for name, shape := range want {
		ls := byBet[w.bets[name].ID]
		got := ""
		if len(ls) == 1 {
			switch {
			case ls[0].Void:
				got = "void"
			case ls[0].Generation != nil:
				got = fmt.Sprintf("%s#%d", ls[0].Outcome, *ls[0].Generation)
			}
		} else if len(ls) > 1 {
			got = fmt.Sprintf("%d lines", len(ls))
		}
		if got != shape {
			t.Errorf("%s: statement shape %q, want %q", name, got, shape)
		}
		if shape == "won#1" || shape == "won#2" {
			if ls[0].PayoutAmount != w.bets[name].PotentialReturn {
				t.Errorf("%s: statement payout %d, want %d", name, ls[0].PayoutAmount, w.bets[name].PotentialReturn)
			}
		}
	}
}

// divergentSource wraps the MOCK source and rewrites its lines - the
// injectable seam of ADR 0088 §14 Q3(a).
type divergentSource struct {
	mutate func([]sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine
}

func (divergentSource) Label() string {
	return "MOCK divergent test statement"
}

func (d divergentSource) StatementLines(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) ([]sportsbook.SettlementStatementLine, error) {
	lines, err := sportsbook.MockSettlementStatementSource{}.StatementLines(ctx, tx, tenantID)
	if err != nil {
		return nil, err
	}
	return d.mutate(lines), nil
}

// TestSportsbookSettlementRecon_DivergentStatementDetected: a statement
// that disagrees with the ledger in payout, outcome, asset, a missing
// line or an extra line is an sb_mock_statement_mismatch, labelled MOCK.
func TestSportsbookSettlementRecon_DivergentStatementDetected(t *testing.T) {
	pool := testPool(t)
	w := seedSBWorld(t, pool)
	wonID, lostID, voidID, openID := w.bets["won"].ID, w.bets["lost"].ID, w.bets["voidBefore"].ID, w.bets["open"].ID

	cases := []struct {
		name   string
		key    string
		mutate func([]sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine
	}{
		{"payout", wonID.String(), func(ls []sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine {
			for i := range ls {
				if ls[i].BetID == wonID {
					ls[i].PayoutAmount++
				}
			}
			return ls
		}},
		{"outcome", lostID.String(), func(ls []sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine {
			for i := range ls {
				if ls[i].BetID == lostID {
					ls[i].Outcome = sportsbook.SettlementOutcomeWon
				}
			}
			return ls
		}},
		{"asset", wonID.String(), func(ls []sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine {
			for i := range ls {
				if ls[i].BetID == wonID {
					ls[i].AssetCode = "USD"
				}
			}
			return ls
		}},
		{"missing_line", voidID.String(), func(ls []sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine {
			out := ls[:0]
			for _, l := range ls {
				if l.BetID != voidID {
					out = append(out, l)
				}
			}
			return out
		}},
		{"extra_line", openID.String(), func(ls []sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine {
			g := 1
			return append(ls, sportsbook.SettlementStatementLine{BetID: openID, Generation: &g, Outcome: "lost", AssetCode: "EUR"})
		}},
		{"void_flag", lostID.String(), func(ls []sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine {
			for i := range ls {
				if ls[i].BetID == lostID {
					ls[i].Void, ls[i].Generation, ls[i].Outcome = true, nil, ""
				}
			}
			return ls
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := requireDetected(t, pool, w.f.tenantID, divergentSource{mutate: tc.mutate}, MismatchKindSBMockStatement, tc.key)
			for _, m := range ms {
				if m.MismatchKind != MismatchKindSBMockStatement {
					t.Errorf("a divergent statement must raise only %s, also got %s", MismatchKindSBMockStatement, m.MismatchKind)
				}
				if !strings.Contains(m.ActualValue, "MOCK") {
					t.Errorf("statement mismatch record must be labelled MOCK: %q", m.ActualValue)
				}
			}
		})
	}
	// The unmodified MOCK source is still clean afterwards.
	requireClean(t, pool, w.f.tenantID)
}

// TestSportsbookSettlementRecon_DriftInjectionPerKind injects drift of
// each ADR 0088 §8 kind into a fresh scratch database (as its owner,
// bypassing the relevant deny/validation trigger where one exists) and
// asserts detection. Each subtest uses its own tenant.
func TestSportsbookSettlementRecon_DriftInjectionPerKind(t *testing.T) {
	pool := sbScratchPool(t)

	fresh := func(t *testing.T) *sbWorld {
		t.Helper()
		w := seedSBWorld(t, pool)
		requireClean(t, pool, w.f.tenantID)
		return w
	}

	t.Run("sb_locked_mismatch/player_locked_cash", func(t *testing.T) {
		w := fresh(t)
		// A sportsbook-typed lock with no bet behind it (a writer that
		// bypassed PlaceBet): the wallet's sportsbook locked net exceeds
		// its open stakes.
		err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			locked, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerLockedCash, "EUR")
			if err != nil {
				return err
			}
			_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: w.f.tenantID, TransactionType: ledger.TxSportsbookBet,
				IdempotencyKey: "drift-" + uuid.NewString(), CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Debit, Amount: 7},
					{LedgerAccountID: locked, Direction: ledger.Credit, Amount: 7},
				},
			})
			return err
		})
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBLocked, "wallet="+w.f.walletID.String()+" asset=EUR account_type=player_locked_cash")
	})

	t.Run("sb_locked_mismatch/player_locked_bonus", func(t *testing.T) {
		w := fresh(t)
		// A sportsbook-typed posting onto player_locked_bonus, written
		// directly (bypassing ledger.Post's Rule B2 mirror): ADR 0088 §8.1
		// requires the sportsbook-typed locked-bonus net to be 0.
		err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			lockedBonus, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, &w.f.walletID, ledger.AccountPlayerLockedBonus, "EUR")
			if err != nil {
				return err
			}
			txID := uuid.New()
			if _, err := tx.Exec(ctx,
				`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id)
				 VALUES ($1, $2, 'sportsbook_bet', $3, $4)`,
				txID, w.f.tenantID, "drift-"+uuid.NewString(), uuid.New()); err != nil {
				return err
			}
			for _, e := range []struct {
				account   uuid.UUID
				direction string
			}{{w.f.cashAccountID, "debit"}, {lockedBonus, "credit"}} {
				if _, err := tx.Exec(ctx,
					`INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
					 VALUES ($1, $2, $3, 'EUR', $4, 5)`,
					txID, e.account, w.f.tenantID, e.direction); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBLocked, "account_type=player_locked_bonus")
	})

	t.Run("sb_bet_net_mismatch", func(t *testing.T) {
		w := fresh(t)
		bet := w.bets["won"]
		// An extra house->cash movement correlated to a settled_won bet.
		err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			house, err := ledger.GetOrCreateAccount(ctx, tx, w.f.tenantID, nil, ledger.AccountHouseGaming, "EUR")
			if err != nil {
				return err
			}
			_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: w.f.tenantID, TransactionType: ledger.TxSportsbookBet,
				IdempotencyKey: "drift-" + uuid.NewString(), CorrelationID: bet.ID,
				Entries: []ledger.EntryInput{
					{LedgerAccountID: house, Direction: ledger.Debit, Amount: 3},
					{LedgerAccountID: w.f.cashAccountID, Direction: ledger.Credit, Amount: 3},
				},
			})
			return err
		})
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBBetNet, "bet="+bet.ID.String()+" check=end_state")
	})

	t.Run("sb_bet_net_mismatch/payout_row", func(t *testing.T) {
		w := fresh(t)
		bet := w.bets["won"]
		// Deny trigger disabled: the won settlement row's payout_amount
		// no longer equals the payout the ledger actually posted. The
		// table has no UPDATE policy under FORCE RLS, so the owner also
		// lifts FORCE for this transaction only - otherwise the UPDATE
		// would silently match zero rows.
		withTriggerDisabled(t, pool, w.f.tenantID, "sportsbook_bet_settlements", "sportsbook_bet_settlements_immutable",
			func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `ALTER TABLE sportsbook_bet_settlements NO FORCE ROW LEVEL SECURITY`); err != nil {
					return err
				}
				if err := execOne(ctx, tx,
					`UPDATE sportsbook_bet_settlements SET payout_amount = payout_amount + 1
					  WHERE bet_id = $1 AND event_kind = 'settlement'`, bet.ID); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `ALTER TABLE sportsbook_bet_settlements FORCE ROW LEVEL SECURITY`)
				return err
			})
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBBetNet, "bet="+bet.ID.String()+" check=payout")
	})

	t.Run("sb_orphan_ledger", func(t *testing.T) {
		w := fresh(t)
		// A tombstone in the sportsbook_settlement: namespace with no
		// history row (a writer that bypassed SimulateSettlementEvent).
		err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: w.f.tenantID, TransactionType: ledger.TxTombstone,
				IdempotencyKey: "sportsbook_settlement:" + w.bets["open"].ID.String() + "#1", CorrelationID: w.bets["open"].ID,
			})
			return err
		})
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBOrphanLedger, "ledger_transaction=")
	})

	t.Run("sb_orphan_history", func(t *testing.T) {
		w := fresh(t)
		bet := w.bets["open"]
		// T-1 disabled: a history row citing a ledger transaction of the
		// wrong type (the placement).
		withTriggerDisabled(t, pool, w.f.tenantID, "sportsbook_bet_settlements", "sportsbook_bet_settlements_validate",
			func(ctx context.Context, tx pgx.Tx) error {
				return execOne(ctx, tx,
					`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, asset_code, ledger_transaction_id)
					 VALUES ($1, $2, 'tombstone', 1, 'EUR', $3)`,
					w.f.tenantID, bet.ID, bet.LedgerTransactionID)
			})
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBOrphanHistory, "bet="+bet.ID.String()+" check=ledger_transaction")
	})

	t.Run("sb_orphan_history/causation", func(t *testing.T) {
		w := fresh(t)
		ltx := w.settlementLedgerTx["resettled#2"]
		// Ledger immutability trigger disabled: the re-settlement loses
		// its causation link to the generation-1 rollback.
		withTriggerDisabled(t, pool, w.f.tenantID, "ledger_transactions", "ledger_transactions_immutable",
			func(ctx context.Context, tx pgx.Tx) error {
				return execOne(ctx, tx, `UPDATE ledger_transactions SET causation_id = NULL WHERE id = $1`, ltx)
			})
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBOrphanHistory, "check=causation")
	})

	t.Run("sb_status_mismatch", func(t *testing.T) {
		w := fresh(t)
		bet := w.bets["open"]
		// T-2 disabled: the status cache moves without history.
		withTriggerDisabled(t, pool, w.f.tenantID, "sportsbook_bets", "sportsbook_bets_status_transition",
			func(ctx context.Context, tx pgx.Tx) error {
				return execOne(ctx, tx, `UPDATE sportsbook_bets SET status = 'settled_lost' WHERE id = $1`, bet.ID)
			})
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBStatus, "bet="+bet.ID.String())
	})

	t.Run("sb_mock_statement_mismatch/history_drift", func(t *testing.T) {
		w := fresh(t)
		bet := w.bets["open"]
		// T-1 disabled: a settlement history row whose ledger posting
		// never happened (it cites the placement). The MOCK statement,
		// rendered from history, now claims a settlement the ledger does
		// not hold.
		withTriggerDisabled(t, pool, w.f.tenantID, "sportsbook_bet_settlements", "sportsbook_bet_settlements_validate",
			func(ctx context.Context, tx pgx.Tx) error {
				return execOne(ctx, tx,
					`INSERT INTO sportsbook_bet_settlements (tenant_id, bet_id, event_kind, generation, outcome, payout_amount, asset_code, ledger_transaction_id)
					 VALUES ($1, $2, 'settlement', 1, 'lost', 0, 'EUR', $3)`,
					w.f.tenantID, bet.ID, bet.LedgerTransactionID)
			})
		requireDetected(t, pool, w.f.tenantID, nil, MismatchKindSBMockStatement, "bet="+bet.ID.String()+" line=settlement#1")
	})

	// The no-footprint fast path must never hide drift: a tenant with no
	// bets at all but one orphan sportsbook ledger transaction, and a
	// tenant with no data at all whose statement claims a settlement.
	t.Run("no_bets/sb_orphan_ledger", func(t *testing.T) {
		f := seedFixture(t, pool)
		requireClean(t, pool, f.tenantID)
		betID := uuid.New()
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxTombstone,
				IdempotencyKey: "sportsbook_settlement:" + betID.String() + "#1", CorrelationID: betID,
			})
			return err
		})
		if err != nil {
			t.Fatalf("inject: %v", err)
		}
		requireDetected(t, pool, f.tenantID, nil, MismatchKindSBOrphanLedger, "ledger_transaction=")
	})

	t.Run("no_data/sb_mock_statement_mismatch", func(t *testing.T) {
		f := seedFixture(t, pool)
		betID := uuid.New()
		src := divergentSource{mutate: func(ls []sportsbook.SettlementStatementLine) []sportsbook.SettlementStatementLine {
			return append(ls, sportsbook.SettlementStatementLine{BetID: betID, Void: true, AssetCode: "EUR"})
		}}
		requireDetected(t, pool, f.tenantID, src, MismatchKindSBMockStatement, "bet="+betID.String()+" line=void")
	})

	// Wiring: RunSweep runs the stream per tenant after
	// ledger_vs_projection, records its own run and an audit row
	// carrying the MOCK statement-source label, and isolates tenants.
	t.Run("RunSweep", func(t *testing.T) {
		clean := fresh(t)
		dirty := fresh(t)
		withTriggerDisabled(t, pool, dirty.f.tenantID, "sportsbook_bets", "sportsbook_bets_status_transition",
			func(ctx context.Context, tx pgx.Tx) error {
				return execOne(ctx, tx, `UPDATE sportsbook_bets SET status = 'void' WHERE id = $1`, dirty.bets["open"].ID)
			})
		outcomes, err := RunSweep(context.Background(), pool, nil, time.Now().Add(-time.Hour), time.Now(), sportsbook.MockSettlementStatementSource{})
		if err != nil {
			t.Fatalf("RunSweep: %v", err)
		}
		oc, od := findOutcome(t, outcomes, clean.f), findOutcome(t, outcomes, dirty.f)
		if oc.Err != nil || oc.Sportsbook.Err != nil || od.Err != nil || od.Sportsbook.Err != nil {
			t.Fatalf("sweep errors: %v %v %v %v", oc.Err, oc.Sportsbook.Err, od.Err, od.Sportsbook.Err)
		}
		if oc.Run.Status != StatusClean || oc.Sportsbook.Run.Status != StatusClean {
			t.Fatalf("clean tenant: ledger=%s sportsbook=%s", oc.Run.Status, oc.Sportsbook.Run.Status)
		}
		if od.Run.Status != StatusClean || od.Sportsbook.Run.Status != StatusMismatchesFound {
			t.Fatalf("dirty tenant: ledger=%s sportsbook=%s", od.Run.Status, od.Sportsbook.Run.Status)
		}
		if od.Sportsbook.Run.Stream != StreamSportsbookSettlement {
			t.Fatalf("sportsbook outcome stream = %s", od.Sportsbook.Run.Stream)
		}
		var audited int
		err = pool.WithTenant(context.Background(), dirty.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT count(*) FROM audit_log
				  WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run' AND target_id = $2
				    AND metadata->>'stream' = 'sportsbook_settlement' AND metadata->>'statement_source' LIKE '%MOCK%'`,
				dirty.f.tenantID, od.Sportsbook.Run.ID.String()).Scan(&audited)
		})
		if err != nil || audited != 1 {
			t.Fatalf("expected one sportsbook sweep audit row labelled MOCK, got %d (err %v)", audited, err)
		}
	})
}
