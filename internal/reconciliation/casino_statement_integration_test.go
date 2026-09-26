//go:build integration

// Stage 10.3 W3a (CAS-RECON-STMT-1): the casino_statement reconciliation
// stream.
//
// The only production source, casino.MockStatementSource, renders its
// statement from the very ledger the stream matches it against, so a clean
// run against it is TAUTOLOGICAL - it proves the plumbing only. The
// matching logic is proven here by casDivergentSource, a TEST-ONLY source
// that takes the MOCK statement and injects exactly one divergence per
// detection path (amount, kind, asset, round, rollback original, missing
// line, extra line, duplicate line, late original after its tombstone, GGR
// total, missing total, extra total, duplicate total, nil GGR), each
// asserted as exactly one cas_mock_statement_mismatch row of the right
// key; plus the one one-sided pattern that must NOT be a finding (a
// provider rollback paired with the ledger's tombstone) and its negative
// controls. Also: nil/erroring source fails closed, READ COMMITTED is
// refused, idempotent re-runs, the advisory lock, snapshot safety under a
// deterministic interleaving (with a READ COMMITTED kill control), live
// traffic, cross-tenant isolation, no financial effect, settlement after
// redelivery, and the sweep wiring (MOCK label in audit, log and
// actual_value; failure audited in a fresh transaction).
package reconciliation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/casino"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/reconciliation/statement"
	"github.com/Diansalas/igaming-platform/internal/sportsbook"
	"github.com/Diansalas/igaming-platform/internal/testsupport/noeffect"
)

// casDivergentLabel is the test-only source's label. It contains "MOCK": it
// is a mock (built on casino.MockStatementSource), and the label rule
// applies to it too.
const casDivergentLabel = "MOCK divergent test statement (test-only; injected divergence over the MOCK casino statement)"

// casDivergentSource is the TEST-ONLY statement source that proves detection:
// it renders the MOCK statement and then applies mutate. It never ships.
type casDivergentSource struct {
	mutate func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal)
	// after, when set, runs after the statement is rendered and before the
	// stream reads the ledger (the snapshot interleaving test).
	after func()
	err   error
}

func (casDivergentSource) Label() string { return casDivergentLabel }

func (d casDivergentSource) Statement(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, ps, pe time.Time) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal, error) {
	if d.err != nil {
		return nil, nil, d.err
	}
	lines, totals, err := casino.MockStatementSource{}.Statement(ctx, tx, tenantID, ps, pe)
	if err != nil {
		return nil, nil, err
	}
	if d.mutate != nil {
		lines, totals = d.mutate(lines, totals)
	}
	if d.after != nil {
		d.after()
	}
	return lines, totals, nil
}

func casMutateLine(ref string, f func(l *statement.CasinoStatementLine)) casDivergentSource {
	return casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
		for i := range lines {
			if lines[i].ProviderTxID == ref {
				f(&lines[i])
			}
		}
		return lines, totals
	}}
}

func casAddLine(l statement.CasinoStatementLine) casDivergentSource {
	return casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
		return append(lines, l), totals
	}}
}

func casMutateTotals(f func(totals []statement.CasinoStatementTotal) []statement.CasinoStatementTotal) casDivergentSource {
	return casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
		return lines, f(totals)
	}}
}

func (w *casWorld) runStmtErr(t *testing.T, src statement.CasinoStatementSource) (Run, []Mismatch, CasinoStatementInfo, error) {
	t.Helper()
	var run Run
	var ms []Mismatch
	var info CasinoStatementInfo
	err := w.pool.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		run, ms, info, err = RunCasinoStatement(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), src)
		return err
	})
	return run, ms, info, err
}

func (w *casWorld) runStmt(t *testing.T, src statement.CasinoStatementSource) (Run, []Mismatch, CasinoStatementInfo) {
	t.Helper()
	run, ms, info, err := w.runStmtErr(t, src)
	if err != nil {
		t.Fatalf("RunCasinoStatement: %v", err)
	}
	return run, ms, info
}

func (w *casWorld) mustStmtClean(t *testing.T, src statement.CasinoStatementSource) CasinoStatementInfo {
	t.Helper()
	run, ms, info := w.runStmt(t, src)
	if run.Status != StatusClean || len(ms) != 0 {
		t.Fatalf("expected a clean casino_statement run, got %s with %d mismatches: %+v", run.Status, len(ms), ms)
	}
	return info
}

// mustOneStmt asserts exactly one mismatch, of the statement kind, whose
// key contains every keyContains fragment and whose actual_value carries
// the source label.
func mustOneStmt(t *testing.T, ms []Mismatch, keyContains ...string) Mismatch {
	t.Helper()
	if len(ms) != 1 {
		t.Fatalf("expected exactly one mismatch, got %d: %+v", len(ms), ms)
	}
	m := mustOneOfKind(t, ms, MismatchKindCasMockStatement, keyContains...)
	if !strings.Contains(m.ActualValue, "MOCK") {
		t.Fatalf("actual_value must carry the MOCK label: %q", m.ActualValue)
	}
	return m
}

func (w *casWorld) stmtRunCount(t *testing.T) (runs, mismatches int) {
	t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE stream = 'casino_statement'`).Scan(&runs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE mismatch_kind = 'cas_mock_statement_mismatch'`).Scan(&mismatches)
	}); err != nil {
		t.Fatal(err)
	}
	return runs, mismatches
}

// ---------------------------------------------------------------------
// Normal: the MOCK source over a clean world (tautological by design).

func TestCasinoStatement_MockOverCleanWorldIsClean(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	info := w.mustStmtClean(t, casino.MockStatementSource{})
	// 11 keyed postings: b1 w1 b2 rb2 b3 w3 rbw3 b4a b4b w4 b6 (rb5
	// tombstoned: no ledger rollback, so the MOCK lists no line for it).
	if info.Lines != 11 || info.Totals != 1 || !info.TotalsProvided || info.TombstonePairings != 0 {
		t.Fatalf("unexpected statement shape: %+v", info)
	}
	// Run row written, with the stream name.
	if runs, mm := w.stmtRunCount(t); runs != 1 || mm != 0 {
		t.Fatalf("expected one clean run row, got runs=%d mismatches=%d", runs, mm)
	}
}

// The MOCK renders what the real posting path wrote: pin its line shapes
// (rollback original and amount, round, asset) and GGR, so the tautology
// is at least a faithful one.
func TestCasinoStatement_MockRendersLedgerFaithfully(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	var lines []statement.CasinoStatementLine
	var totals []statement.CasinoStatementTotal
	if err := w.pool.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		lines, totals, err = casino.MockStatementSource{}.Statement(ctx, tx, w.f.tenantID, time.Time{}, time.Time{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	by := map[string]statement.CasinoStatementLine{}
	for _, l := range lines {
		by[l.ProviderTxID] = l
	}
	want := map[string]statement.CasinoStatementLine{
		"b1":   {Kind: "bet", Amount: 1000, RoundID: "r1"},
		"w1":   {Kind: "win", Amount: 2500, RoundID: "r1"},
		"rb2":  {Kind: "rollback", Amount: 500, RoundID: "r2", OriginalProviderTxID: "b2"},
		"rbw3": {Kind: "rollback", Amount: 700, RoundID: "r3", OriginalProviderTxID: "w3"},
		"b4b":  {Kind: "bet", Amount: 200, RoundID: "r4"},
	}
	for ref, wl := range want {
		got, ok := by[ref]
		if !ok || got.Kind != wl.Kind || got.Amount != wl.Amount || got.RoundID != wl.RoundID ||
			got.OriginalProviderTxID != wl.OriginalProviderTxID || got.AssetCode != "EUR" || got.ProviderID != casProvider {
			t.Errorf("line %s: got %+v, want %+v", ref, got, wl)
		}
	}
	if _, ok := by["rb5"]; ok {
		t.Error("a tombstoning rollback has no ledger rollback and must not be rendered by the MOCK")
	}
	// GGR = bets - wins net of rollbacks:
	// (1000-2500) + (500-500) + (700-700-(-700)+... ) computed explicitly:
	// b1 +1000, w1 -2500, b2 +500, rb2 -500, b3 +700, w3 -700, rbw3 +700,
	// b4a +100, b4b +200, w4 -50, b6 +300 = -250.
	if len(totals) != 1 || totals[0].ProviderID != casProvider || totals[0].AssetCode != "EUR" || totals[0].GGR.Cmp(big.NewInt(-250)) != 0 {
		t.Fatalf("unexpected totals: %+v", totals)
	}
}

// ---------------------------------------------------------------------
// Detection: one test per path, each exactly one row.

func TestCasinoStatement_DetectsEveryKeyDivergence(t *testing.T) {
	cases := []struct {
		name string
		src  casDivergentSource
		key  []string
	}{
		{"amount", casMutateLine("b1", func(l *statement.CasinoStatementLine) { l.Amount = 999 }),
			[]string{"provider_tx_id=b1 ", "fields=amount"}},
		{"kind", casMutateLine("w1", func(l *statement.CasinoStatementLine) { l.Kind = statement.CasinoLineBet }),
			[]string{"provider_tx_id=w1 ", "fields=kind"}},
		{"asset", casMutateLine("b3", func(l *statement.CasinoStatementLine) { l.AssetCode = "USD" }),
			[]string{"provider_tx_id=b3 ", "fields=asset"}},
		{"round", casMutateLine("b4a", func(l *statement.CasinoStatementLine) { l.RoundID = "r-other" }),
			[]string{"provider_tx_id=b4a ", "fields=round"}},
		{"rollback original", casMutateLine("rb2", func(l *statement.CasinoStatementLine) { l.OriginalProviderTxID = "b1" }),
			[]string{"provider_tx_id=rb2 ", "fields=original"}},
		{"rollback amount", casMutateLine("rbw3", func(l *statement.CasinoStatementLine) { l.Amount = 1 }),
			[]string{"provider_tx_id=rbw3 ", "fields=amount"}},
		{"unknown kind", casMutateLine("b6", func(l *statement.CasinoStatementLine) { l.Kind = "jackpot" }),
			[]string{"provider_tx_id=b6 ", "fields=kind"}},
		{"missing line", casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
			out := lines[:0]
			for _, l := range lines {
				if l.ProviderTxID != "w4" {
					out = append(out, l)
				}
			}
			return out, totals
		}}, []string{"provider_tx_id=w4 ", "check=key"}},
		{"extra line", casAddLine(statement.CasinoStatementLine{ProviderID: casProvider, ProviderTxID: "ghost-1", Kind: "bet", RoundID: "r9", AssetCode: "EUR", Amount: 10}),
			[]string{"provider_tx_id=ghost-1 ", "check=key"}},
		{"extra line under another provider", casAddLine(statement.CasinoStatementLine{ProviderID: "other-casino", ProviderTxID: "b1", Kind: "bet", RoundID: "r1", AssetCode: "EUR", Amount: 1000}),
			[]string{"provider=other-casino provider_tx_id=b1 "}},
		{"duplicate line", casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
			for _, l := range lines {
				if l.ProviderTxID == "b2" {
					return append(lines, l), totals
				}
			}
			return lines, totals
		}}, []string{"provider_tx_id=b2 ", "check=key"}},
	}
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	w.mustStmtClean(t, casino.MockStatementSource{})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			run, ms, _ := w.runStmt(t, c.src)
			if run.Status != StatusMismatchesFound {
				t.Fatalf("expected mismatches_found, got %s", run.Status)
			}
			m := mustOneStmt(t, ms, c.key...)
			if !strings.Contains(m.ActualValue, casDivergentLabel) {
				t.Fatalf("actual_value must carry the source label: %q", m.ActualValue)
			}
		})
	}
	// Duplicate is reported as such.
	_, ms, _ := w.runStmt(t, cases[len(cases)-1].src)
	if !strings.Contains(ms[0].ActualValue, "duplicate statement line") {
		t.Fatalf("expected a duplicate-line finding, got %+v", ms)
	}
}

func TestCasinoStatement_DetectsEveryTotalsDivergence(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	cases := []struct {
		name   string
		src    casDivergentSource
		key    []string
		actual string
	}{
		{"GGR differs", casMutateTotals(func(ts []statement.CasinoStatementTotal) []statement.CasinoStatementTotal {
			ts[0].GGR = new(big.Int).Add(ts[0].GGR, big.NewInt(1))
			return ts
		}), []string{"total: provider=mock-casino asset=EUR", "check=total"}, "GGR=-249"},
		{"missing total", casMutateTotals(func(ts []statement.CasinoStatementTotal) []statement.CasinoStatementTotal {
			return append(ts, statement.CasinoStatementTotal{ProviderID: casProvider, AssetCode: "USD", GGR: big.NewInt(0)})[1:]
		}), []string{"total: provider=mock-casino asset=EUR"}, "no statement total"},
		{"extra non-zero total", casMutateTotals(func(ts []statement.CasinoStatementTotal) []statement.CasinoStatementTotal {
			return append(ts, statement.CasinoStatementTotal{ProviderID: "other-casino", AssetCode: "EUR", GGR: big.NewInt(5)})
		}), []string{"total: provider=other-casino asset=EUR"}, "GGR=5"},
		{"duplicate total", casMutateTotals(func(ts []statement.CasinoStatementTotal) []statement.CasinoStatementTotal {
			return append(ts, ts[0])
		}), []string{"total: provider=mock-casino asset=EUR"}, "duplicate statement total"},
		{"nil GGR", casMutateTotals(func(ts []statement.CasinoStatementTotal) []statement.CasinoStatementTotal {
			ts[0].GGR = nil
			return ts
		}), []string{"total: provider=mock-casino asset=EUR"}, "GGR=<nil>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, ms, _ := w.runStmt(t, c.src)
			m := mustOneStmt(t, ms, c.key...)
			if !strings.Contains(m.ActualValue, c.actual) {
				t.Fatalf("actual_value %q must contain %q", m.ActualValue, c.actual)
			}
		})
	}
	// A total stated as zero for a pair the ledger never moved is clean
	// (compared against zero, not treated as one-sided).
	w.mustStmtClean(t, casMutateTotals(func(ts []statement.CasinoStatementTotal) []statement.CasinoStatementTotal {
		return append(ts, statement.CasinoStatementTotal{ProviderID: casProvider, AssetCode: "USD", GGR: big.NewInt(0)})
	}))
	// A source that reports no totals: the totals match is skipped and the
	// run says so - the key match still runs.
	info := w.mustStmtClean(t, casMutateTotals(func([]statement.CasinoStatementTotal) []statement.CasinoStatementTotal { return nil }))
	if info.TotalsProvided || info.Totals != 0 {
		t.Fatalf("expected totals not provided: %+v", info)
	}
	_, ms, _ := w.runStmt(t, casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, _ []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
		lines[0].Amount++
		return lines, nil
	}})
	mustOneStmt(t, ms, "check=key", "fields=amount")
}

// A ledger total with real movement is compared: a corrupted house leg
// (a bypassing writer) moves the ledger net, and a statement that did not
// see it disagrees. The statement here is the MOCK captured BEFORE the
// corruption - i.e. a provider statement that genuinely differs.
func TestCasinoStatement_TotalsDetectLedgerSideDrift(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	var captured []statement.CasinoStatementLine
	var capturedTotals []statement.CasinoStatementTotal
	if err := w.pool.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		captured, capturedTotals, err = casino.MockStatementSource{}.Statement(ctx, tx, w.f.tenantID, time.Time{}, time.Time{})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// A bet posted through the real callback path that the (captured)
	// provider statement does not list.
	w.mustDeliver(t, casino.CallbackEventBet, "td-b", "", "td-r", 40)
	static := casDivergentSource{mutate: func([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
		return append([]statement.CasinoStatementLine{}, captured...), append([]statement.CasinoStatementTotal{}, capturedTotals...)
	}}
	_, ms, _ := w.runStmt(t, static)
	if len(ms) != 2 {
		t.Fatalf("expected the key and the total findings, got %+v", ms)
	}
	var sawKey, sawTotal bool
	for _, m := range ms {
		sawKey = sawKey || strings.Contains(m.ReconciliationKey, "provider_tx_id=td-b ") && strings.Contains(m.ActualValue, "no statement line")
		sawTotal = sawTotal || strings.Contains(m.ReconciliationKey, "total: provider=mock-casino asset=EUR") &&
			m.ExpectedValue == "ledger: house_gaming_net=-210" && strings.Contains(m.ActualValue, "GGR=-250")
	}
	if !sawKey || !sawTotal {
		t.Fatalf("expected a key finding for td-b and a total finding -210 vs -250, got %+v", ms)
	}
}

// ---------------------------------------------------------------------
// Rollback vs tombstone: the one one-sided pattern that is a match.

func TestCasinoStatement_RollbackPairedWithTombstoneIsAMatch(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t) // rb5 -> tombstone under never-5
	rb5 := statement.CasinoStatementLine{ProviderID: casProvider, ProviderTxID: "rb5", Kind: "rollback",
		OriginalProviderTxID: "never-5", RoundID: "r5", AssetCode: "EUR", Amount: 0}
	info := w.mustStmtClean(t, casAddLine(rb5))
	if info.TombstonePairings != 1 {
		t.Fatalf("expected one rollback/tombstone pairing, got %+v", info)
	}

	// Negative controls: the pairing needs a CASINO tombstone under THIS
	// provider for THIS original, and a rollback line.
	noTombstone := rb5
	noTombstone.ProviderTxID, noTombstone.OriginalProviderTxID = "rb-x", "never-x"
	_, ms, _ := w.runStmt(t, casAddLine(noTombstone))
	mustOneStmt(t, ms, "provider_tx_id=rb-x ")

	otherProvider := rb5
	otherProvider.ProviderID = "other-casino"
	_, ms, _ = w.runStmt(t, casAddLine(otherProvider))
	mustOneStmt(t, ms, "provider=other-casino provider_tx_id=rb5 ")

	notARollback := rb5
	notARollback.Kind = "win"
	_, ms, _ = w.runStmt(t, casAddLine(notARollback))
	mustOneStmt(t, ms, "provider_tx_id=rb5 ")

	// A NON-casino tombstone (a payments provider's, G-4: the key format
	// alone cannot tell them apart) never pairs with a casino statement
	// line, even under the same provider id and reference.
	w.post(t, ledger.TransactionInput{TransactionType: ledger.TxTombstone,
		IdempotencyKey: "tombstone:mock-payments:pay-x", ProviderID: strp("mock-payments"), ProviderTxID: strp("pay-x"),
		CorrelationID: uuid.New()})
	paymentsTombstone := statement.CasinoStatementLine{ProviderID: "mock-payments", ProviderTxID: "rb-pay", Kind: "rollback",
		OriginalProviderTxID: "pay-x", RoundID: "r-pay", AssetCode: "EUR"}
	_, ms, _ = w.runStmt(t, casAddLine(paymentsTombstone))
	mustOneStmt(t, ms, "provider=mock-payments provider_tx_id=rb-pay ")
}

// Disclosed behaviour (gate 10.3-W2/W3 code review #7), pinned so a change
// is deliberate: an unpaired casino tombstone is not a finding (the clean
// world holds one and the MOCK lists no rollback for it), and several
// statement rollbacks naming ONE tombstoned original all match (the E9
// many-to-one shape). A real source may want to flag both; see
// casino_statement.go.
func TestCasinoStatement_TombstonePairingDisclosedBehaviour(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t) // rb5 -> tombstone under never-5, unpaired under the MOCK
	if info := w.mustStmtClean(t, casino.MockStatementSource{}); info.TombstonePairings != 0 {
		t.Fatalf("unpaired tombstone: expected a clean run with no pairing, got %+v", info)
	}
	rb := func(ref string) statement.CasinoStatementLine {
		return statement.CasinoStatementLine{ProviderID: casProvider, ProviderTxID: ref, Kind: "rollback",
			OriginalProviderTxID: "never-5", RoundID: "r5", AssetCode: "EUR"}
	}
	two := casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
		return append(lines, rb("rb5"), rb("rb5-again")), totals
	}}
	if info := w.mustStmtClean(t, two); info.TombstonePairings != 2 {
		t.Fatalf("many-to-one: expected both rollback lines to pair with the one tombstone, got %+v", info)
	}
}

// C7's counterparty half: the provider still counts an original the
// platform tombstoned. The finding names the tombstone.
func TestCasinoStatement_LateOriginalAfterTombstoneIsAFinding(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	if res := w.mustDeliver(t, casino.CallbackEventBet, "never-5", "", "r5", 900); res.Outcome != casino.OutcomeDeclined {
		t.Fatalf("expected the E3 decline, got %+v", res)
	}
	src := casAddLine(statement.CasinoStatementLine{ProviderID: casProvider, ProviderTxID: "never-5", Kind: "bet", RoundID: "r5", AssetCode: "EUR", Amount: 900})
	_, ms, _ := w.runStmt(t, src)
	m := mustOneStmt(t, ms, "provider_tx_id=never-5 ")
	if !strings.Contains(m.ExpectedValue, "tombstone") {
		t.Fatalf("the finding must name the tombstone: %+v", m)
	}
}

// The MOCK is tautological for keys, amounts and totals, but the round is
// rendered from casino_provider_rounds and compared through the ledger
// correlation id: a posting with no round binding (a bypassing writer,
// also C1's finding) is visible even to the MOCK. Pinned so the docs'
// "tautological" statement stays accurate.
func TestCasinoStatement_MockSeesUnboundRound(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	cash, house := w.accounts(t, w.f.walletID)
	w.post(t, ledger.TransactionInput{TransactionType: ledger.TxCasinoBet,
		ProviderID: strp(casProvider), ProviderTxID: strp("unbound-b"), CorrelationID: corr(w.f.tenantID, "unbound-r"),
		Entries: []ledger.EntryInput{{LedgerAccountID: cash, Direction: ledger.Debit, Amount: 5}, {LedgerAccountID: house, Direction: ledger.Credit, Amount: 5}}})
	_, ms, _ := w.runStmt(t, casino.MockStatementSource{})
	m := mustOneStmt(t, ms, "provider_tx_id=unbound-b ", "fields=round")
	if !strings.Contains(m.ActualValue, casino.MockStatementLabel) {
		t.Fatalf("actual_value must carry the MOCK source label: %q", m.ActualValue)
	}
}

// ---------------------------------------------------------------------
// Fail closed.

func TestCasinoStatement_NilOrErroringSourceFailsClosed(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	if _, _, _, err := w.runStmtErr(t, nil); err == nil || !strings.Contains(err.Error(), "requires a casino statement source") {
		t.Fatalf("a nil source must fail the run, got %v", err)
	}
	if _, _, _, err := w.runStmtErr(t, casDivergentSource{err: errors.New("provider statement API down")}); err == nil ||
		!strings.Contains(err.Error(), "provider statement API down") || !strings.Contains(err.Error(), casDivergentLabel) {
		t.Fatalf("a source error must fail the run with the label, got %v", err)
	}
	if runs, mm := w.stmtRunCount(t); runs != 0 || mm != 0 {
		t.Fatalf("a failed run must leave no partial rows, got runs=%d mismatches=%d", runs, mm)
	}
}

func TestCasinoStatement_ReadCommittedTransactionIsRefused(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, _, err := RunCasinoStatement(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), casino.MockStatementSource{})
		return err
	})
	if err == nil || !strings.Contains(err.Error(), "REPEATABLE READ") {
		t.Fatalf("a READ COMMITTED run must be refused, got %v", err)
	}
	if runs, _ := w.stmtRunCount(t); runs != 0 {
		t.Fatalf("a refused run must record nothing, got %d runs", runs)
	}
}

// ---------------------------------------------------------------------
// Idempotency / duplicates.

func TestCasinoStatement_RerunsAreIdempotentAndStateTypeReDetects(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	w.mustStmtClean(t, casino.MockStatementSource{})
	w.mustStmtClean(t, casino.MockStatementSource{})
	if runs, mm := w.stmtRunCount(t); runs != 2 || mm != 0 {
		t.Fatalf("two clean re-runs must add two clean runs, got runs=%d mismatches=%d", runs, mm)
	}
	src := casMutateLine("b1", func(l *statement.CasinoStatementLine) { l.Amount = 1 })
	for i := 0; i < 2; i++ {
		_, ms, _ := w.runStmt(t, src)
		mustOneStmt(t, ms, "provider_tx_id=b1 ")
	}
	if runs, mm := w.stmtRunCount(t); runs != 4 || mm != 2 {
		t.Fatalf("state-type: each divergent run records its own finding, got runs=%d mismatches=%d", runs, mm)
	}
	// Redelivered callbacks change nothing (the ledger is idempotent), so
	// the MOCK stays clean.
	w.mustDeliver(t, casino.CallbackEventBet, "b1", "", "r1", 1000)
	w.mustDeliver(t, casino.CallbackEventWin, "w1", "", "r1", 2500)
	w.mustStmtClean(t, casino.MockStatementSource{})
}

// Settlement: a win the provider reports but the ledger lacks is a
// finding; after the provider redelivers it through the normal idempotent
// callback path the next run is clean, and the old finding stays open as
// evidence until a human resolves it.
func TestCasinoStatement_MissingWinThenRedeliveryIsClean(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	lateWin := statement.CasinoStatementLine{ProviderID: casProvider, ProviderTxID: "late-w6", Kind: "win", RoundID: "r6", AssetCode: "EUR", Amount: 450}
	provider := casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
		for _, l := range lines {
			if l.ProviderTxID == lateWin.ProviderTxID {
				return lines, totals // the ledger already has it
			}
		}
		// The provider's GGR already counts its win.
		totals[0].GGR = new(big.Int).Sub(totals[0].GGR, big.NewInt(lateWin.Amount))
		return append(lines, lateWin), totals
	}}
	_, ms, _ := w.runStmt(t, provider)
	if len(ms) != 2 {
		t.Fatalf("expected the missing-win key finding and its total finding, got %+v", ms)
	}
	w.mustDeliver(t, casino.CallbackEventWin, "late-w6", "", "r6", 450)
	w.mustStmtClean(t, provider)
	var open int
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches
			WHERE mismatch_kind = 'cas_mock_statement_mismatch' AND investigation_status = 'open'`).Scan(&open)
	}); err != nil || open != 2 {
		t.Fatalf("the earlier findings must remain open evidence: open=%d err=%v", open, err)
	}
}

// ---------------------------------------------------------------------
// Concurrency.

func TestCasinoStatement_ConcurrentRunsSerializedByAdvisoryLock(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	hold, firstIn := make(chan struct{}), make(chan bool, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = w.pool.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, _, _, acquired, err := TryRunCasinoStatementForTenant(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), casino.MockStatementSource{})
			firstIn <- acquired
			<-hold
			return err
		})
	}()
	if !<-firstIn {
		close(hold)
		wg.Wait()
		t.Fatal("first run must acquire the lock")
	}
	var second bool
	err := w.pool.WithTenantSnapshot(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, _, _, second, err = TryRunCasinoStatementForTenant(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now(), casino.MockStatementSource{})
		return err
	})
	// The casino_consistency stream's lock is a different key: it is free
	// while the statement lock is held.
	var consistencyAcquired bool
	cerr := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, _, _, consistencyAcquired, err = TryRunCasinoConsistencyForTenant(ctx, tx, w.f.tenantID, time.Now().Add(-time.Hour), time.Now())
		return err
	})
	close(hold)
	wg.Wait()
	if err != nil || second {
		t.Fatalf("second concurrent run must skip: acquired=%t err=%v", second, err)
	}
	if cerr != nil || !consistencyAcquired {
		t.Fatalf("casino_consistency must not contend with casino_statement: acquired=%t err=%v", consistencyAcquired, cerr)
	}
	if runs, _ := w.stmtRunCount(t); runs != 1 {
		t.Fatalf("expected exactly one casino_statement run, got %d", runs)
	}
}

// Deterministic snapshot proof: a bet and win commit AFTER the statement
// was read and BEFORE the stream reads the ledger. Under the enforced
// REPEATABLE READ snapshot the run is clean. Kill control: the identical
// interleaving matched under READ COMMITTED reports the false P1 the
// snapshot exists to prevent.
func TestCasinoStatement_SnapshotPreventsFalseMismatchUnderInterleaving(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	var n atomic.Int64
	interleave := func() {
		i := n.Add(1)
		round := fmt.Sprintf("il-%d", i)
		w.mustDeliver(t, casino.CallbackEventBet, round+"-b", "", round, 25)
		w.mustDeliver(t, casino.CallbackEventWin, round+"-w", "", round, 30)
	}
	w.mustStmtClean(t, casDivergentSource{after: interleave})

	// Kill control (READ COMMITTED, matcher called directly, bypassing the
	// isolation guard): the same interleaving produces false findings.
	var falseFindings []Mismatch
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		lines, _, err := casDivergentSource{after: interleave}.Statement(ctx, tx, w.f.tenantID, time.Time{}, time.Time{})
		if err != nil {
			return err
		}
		r := &sbRecorder{tenantID: w.f.tenantID}
		if _, err := casStmtMatchKeys(ctx, tx, w.f.tenantID, "[control] ", lines, r); err != nil {
			return err
		}
		falseFindings = r.mismatches
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(falseFindings) != 2 {
		t.Fatalf("control: under READ COMMITTED the interleaved bet and win must surface as 2 false findings, got %+v", falseFindings)
	}
	w.mustStmtClean(t, casino.MockStatementSource{})
}

// Live traffic: many runs concurrent with real bet/win/rollback/tombstone
// traffic never report a false mismatch.
func TestCasinoStatement_LiveTrafficProducesNoFalseMismatch(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var delivered atomic.Int64
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				round := fmt.Sprintf("stlive-%d-%d", g, i)
				if _, err := w.deliver(t, casino.CallbackEventBet, round+"-b", "", round, 10); err != nil {
					t.Errorf("live bet: %v", err)
					return
				}
				delivered.Add(1)
				switch i % 3 {
				case 0:
					_, _ = w.deliver(t, casino.CallbackEventWin, round+"-w", "", round, 15)
				case 1:
					_, _ = w.deliver(t, casino.CallbackEventRollback, round+"-rb", round+"-b", round, 0)
				default:
					_, _ = w.deliver(t, casino.CallbackEventRollback, round+"-tomb", round+"-unseen", round, 0)
				}
			}
		}(g)
	}
	for i := 0; i < 10 || delivered.Load() < 30; i++ {
		if i > 2000 {
			close(stop)
			wg.Wait()
			t.Fatal("live traffic did not progress")
		}
		run, ms, _ := w.runStmt(t, casino.MockStatementSource{})
		if run.Status != StatusClean {
			close(stop)
			wg.Wait()
			t.Fatalf("run %d under live traffic reported %+v", i, ms)
		}
	}
	close(stop)
	wg.Wait()
	w.mustStmtClean(t, casino.MockStatementSource{})
}

// ---------------------------------------------------------------------
// Authorization / tenant isolation.

func TestCasinoStatement_CrossTenantIsolation(t *testing.T) {
	pool := testPool(t)
	a := newCasWorld(t, pool)
	b := newCasWorld(t, pool)
	a.buildCleanWorld(t)
	b.buildCleanWorld(t)
	b.mustDeliver(t, casino.CallbackEventBet, "only-b", "", "rb-only", 10)

	// B's MOCK statement renders B's rows only (same references as A's,
	// never A's): 11 + 1 lines.
	info := b.mustStmtClean(t, casino.MockStatementSource{})
	if info.Lines != 12 {
		t.Fatalf("tenant B's statement must hold only B's 12 postings, got %+v", info)
	}
	// A statement line for a reference only B holds is a finding in A: A's
	// run cannot see B's ledger.
	_, ms, _ := a.runStmt(t, casAddLine(statement.CasinoStatementLine{ProviderID: casProvider, ProviderTxID: "only-b", Kind: "bet", RoundID: "rb-only", AssetCode: "EUR", Amount: 10}))
	mustOneStmt(t, ms, "provider_tx_id=only-b ")
	// B's connection reads none of A's statement runs or mismatches.
	var runs, mm int
	if err := pool.WithTenant(context.Background(), b.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE tenant_id = $1`, a.f.tenantID).Scan(&runs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_mismatches WHERE tenant_id = $1`, a.f.tenantID).Scan(&mm)
	}); err != nil || runs != 0 || mm != 0 {
		t.Fatalf("tenant B read A's runs=%d mismatches=%d (%v)", runs, mm, err)
	}
}

// ---------------------------------------------------------------------
// No-effect: detection never corrects and never touches money.

func TestCasinoStatement_DetectionHasNoFinancialEffect(t *testing.T) {
	w := newCasWorld(t, testPool(t))
	w.buildCleanWorld(t)
	before := noeffect.CaptureCasino(t, w.pool, []uuid.UUID{w.f.tenantID})
	var rejBefore int
	_ = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&rejBefore)
	})
	divergent := casDivergentSource{mutate: func(lines []statement.CasinoStatementLine, totals []statement.CasinoStatementTotal) ([]statement.CasinoStatementLine, []statement.CasinoStatementTotal) {
		lines[0].Amount += 7
		totals[0].GGR = new(big.Int).Add(totals[0].GGR, big.NewInt(7))
		return append(lines, statement.CasinoStatementLine{ProviderID: casProvider, ProviderTxID: "ne-ghost", Kind: "win", RoundID: "r1", AssetCode: "EUR", Amount: 99}), totals
	}}
	for i := 0; i < 2; i++ {
		if run, ms, _ := w.runStmt(t, divergent); run.Status != StatusMismatchesFound || len(ms) != 3 {
			t.Fatalf("expected 3 findings, got %s %+v", run.Status, ms)
		}
	}
	w.mustStmtClean(t, casino.MockStatementSource{})
	noeffect.AssertNoCasinoEffect(t, w.pool, []uuid.UUID{w.f.tenantID}, before)
	var rejAfter int
	_ = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM casino_callback_rejections`).Scan(&rejAfter)
	})
	if rejAfter != rejBefore {
		t.Fatalf("the stream must never write the rejection record: %d -> %d", rejBefore, rejAfter)
	}
}

// ---------------------------------------------------------------------
// Sweep wiring: after casino_consistency, audited with the MOCK label,
// P1 log, failure audited in a fresh transaction.

func TestRunSweep_CasinoStatementStreamWiredAuditedAndP1Logged(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)

	// MOCK source: clean, audited with the MOCK label and statement shape.
	outcomes, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{w.f.tenantID}, time.Now().Add(-time.Hour), time.Now(),
		sportsbook.MockSettlementStatementSource{}, casino.MockStatementSource{})
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	o := findOutcome(t, outcomes, w.f)
	if o.CasinoStatement.Err != nil || o.CasinoStatement.Skipped || o.CasinoStatement.Run.Status != StatusClean ||
		o.CasinoStatement.Run.Stream != StreamCasinoStatement {
		t.Fatalf("unexpected casino_statement outcome: %+v", o.CasinoStatement)
	}
	if o.Casino.Run.Stream != StreamCasinoConsistency || o.Casino.Err != nil {
		t.Fatalf("casino_consistency must still run: %+v", o.Casino)
	}
	var meta []byte
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run'
			AND metadata->>'stream' = 'casino_statement' AND target_id = $2`, w.f.tenantID, o.CasinoStatement.Run.ID.String()).Scan(&meta)
	}); err != nil {
		t.Fatalf("casino_statement sweep audit row: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(meta, &m); err != nil {
		t.Fatalf("audit metadata: %v", err)
	}
	if m["statement_source"] != casino.MockStatementLabel || m["statement_lines"] != float64(11) ||
		m["statement_totals_provided"] != true || m["rollback_tombstone_pairings"] != float64(0) || m["status"] != "clean" {
		t.Fatalf("unexpected casino_statement audit metadata: %s", meta)
	}

	// A divergent source: P1 log line carrying the label.
	lc := &logCapture{}
	logger := slog.New(slog.NewJSONHandler(lc, nil))
	outcomes, err = RunSweepTenants(context.Background(), pool, logger, []uuid.UUID{w.f.tenantID}, time.Now().Add(-time.Hour), time.Now(),
		sportsbook.MockSettlementStatementSource{}, casMutateLine("b1", func(l *statement.CasinoStatementLine) { l.Amount = 3 }))
	if err != nil {
		t.Fatalf("RunSweep: %v", err)
	}
	o = findOutcome(t, outcomes, w.f)
	if o.CasinoStatement.Run.Status != StatusMismatchesFound {
		t.Fatalf("expected mismatches_found, got %+v", o.CasinoStatement)
	}
	var p1 bool
	for _, l := range lc.lines() {
		if l["msg"] == "reconciliation sweep: MISMATCH FOUND" && l["stream"] == "casino_statement" &&
			l["tenant_id"] == w.f.tenantID.String() && l["statement_source"] == casDivergentLabel && l["level"] == "ERROR" {
			p1 = true
		}
	}
	if !p1 {
		t.Fatal("expected an Error-level P1 'MISMATCH FOUND' log line for casino_statement with the source label")
	}
}

func TestRunSweep_CasinoStatementNilSourceFailsClosedAndIsAudited(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	outcomes, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{w.f.tenantID}, time.Now().Add(-time.Hour), time.Now(),
		sportsbook.MockSettlementStatementSource{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := findOutcome(t, outcomes, w.f)
	if o.CasinoStatement.Err == nil || o.CasinoStatement.Run.Status == StatusClean {
		t.Fatalf("a nil source must fail the casino_statement run, never clean: %+v", o.CasinoStatement)
	}
	// The other streams are unaffected.
	if o.Err != nil || o.Casino.Err != nil || o.Casino.Run.Status != StatusClean {
		t.Fatalf("other streams must be unaffected: %+v", o)
	}
	var runs, failed int
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE stream = 'casino_statement'`).Scan(&runs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
			AND metadata->>'stream' = 'casino_statement' AND metadata->>'statement_source' = '<none>'`, w.f.tenantID).Scan(&failed)
	}); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || failed != 1 {
		t.Fatalf("expected no run and one failure audit, got runs=%d failed=%d", runs, failed)
	}
}

func TestRunSweep_CasinoStatementSourceErrorIsAuditedInFreshTransaction(t *testing.T) {
	pool := testPool(t)
	w := newCasWorld(t, pool)
	w.buildCleanWorld(t)
	outcomes, err := RunSweepTenants(context.Background(), pool, nil, []uuid.UUID{w.f.tenantID}, time.Now().Add(-time.Hour), time.Now(),
		sportsbook.MockSettlementStatementSource{}, casDivergentSource{err: errors.New("statement fetch timed out")})
	if err != nil {
		t.Fatal(err)
	}
	if o := findOutcome(t, outcomes, w.f); o.CasinoStatement.Err == nil {
		t.Fatal("expected the erroring source to fail the run")
	}
	var runs, failed int
	if err := pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM reconciliation_runs WHERE stream = 'casino_statement'`).Scan(&runs); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'reconciliation.sweep_run_failed'
			AND metadata->>'stream' = 'casino_statement' AND metadata->>'error' LIKE '%statement fetch timed out%'`, w.f.tenantID).Scan(&failed)
	}); err != nil {
		t.Fatal(err)
	}
	if runs != 0 || failed != 1 {
		t.Fatalf("expected no partial run and one failure audit, got runs=%d failed=%d", runs, failed)
	}
}
