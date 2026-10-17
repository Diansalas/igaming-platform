//go:build integration

// ADR 0113 slices 2-3 review (ledger-finance C1 and recommended tests): the unlinked hold-leg rule
// (WD042) of check D, the "wallet:<id>:<asset>" key path with a second-asset wallet of the same
// player, and the one-statement ledger_vs_projection compare against the ledger package's own reads.
package reconciliation

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/negbal"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// LF C1: a planted pair Dr player_cash / Cr hold then Dr hold / Cr psp_clearing (correlation ids of
// no request; owner, the 0132 hold-entry triggers disabled) nets to ZERO on the wallet, so the
// aggregate is silent, yet the run is NOT clean: each transaction is an unlinked hold leg (WD042).
func TestADR0113D_UnlinkedZeroNetPair_IsNotClean(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	wdpath.SkipOnPersistentDatabase(t, pool)
	hold := holdAccount(t, pool, f)
	var txs []string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return ownerTamperHoldLegs(ctx, tx, func() error {
			a, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalRequested,
				IdempotencyKey: "d-pair-" + uuid.NewString(), CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: f.cashAccountID, Direction: ledger.Debit, Amount: 55},
					{LedgerAccountID: hold, Direction: ledger.Credit, Amount: 55},
				},
			})
			if err != nil {
				return err
			}
			pid, ref := "d-pair-psp", "d-pair-"+uuid.NewString()
			b, err := ledger.Post(ctx, tx, ledger.TransactionInput{
				TenantID: f.tenantID, TransactionType: ledger.TxWithdrawalCompleted, IdempotencyKey: pid + ":" + ref,
				ProviderID: &pid, ProviderTxID: &ref, CorrelationID: uuid.New(),
				Entries: []ledger.EntryInput{
					{LedgerAccountID: hold, Direction: ledger.Debit, Amount: 55},
					{LedgerAccountID: f.clearingID, Direction: ledger.Credit, Amount: 55},
				},
			})
			txs = []string{a.TransactionID.String(), b.TransactionID.String()}
			return err
		})
	}); err != nil {
		t.Fatalf("owner tamper plant (pair): %v", err)
	}
	o := dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusMismatchesFound {
		t.Fatalf("a zero-net unlinked pair must not reconcile clean: status=%s err=%v", o.Run.Status, o.Err)
	}
	rows := dMismatches(t, pool, f)
	if len(dKind(rows, MismatchKindWithdrawalHoldOpenMismatch)) != 0 {
		t.Fatalf("the pair nets to zero: no aggregate finding expected: %+v", rows)
	}
	got := map[string]bool{}
	for _, r := range dKind(rows, MismatchKindWithdrawalRequestLedgerLinkInvalid) {
		if strings.HasPrefix(r.actual, "violation=WD042 ") {
			got[strings.TrimPrefix(r.key, "ledger_transaction:")] = true
		}
	}
	if len(got) != 2 || !got[txs[0]] || !got[txs[1]] {
		t.Fatalf("both transactions of the pair must be reported WD042: %v (rows %+v)", txs, rows)
	}
}

// Multi-asset player and the "wallet:<id>:<asset>" key: the player holds a EUR and a GBP wallet, each
// with healthy requests. An owner tamper re-points one EUR request onto the GBP wallet (state guard,
// immutable-fields guard and check A disabled for that statement): (GBP wallet, EUR) has open
// requests but no hold account -> key "wallet:<gbp>:EUR"; the EUR hold account now carries a balance
// no open request explains -> keyed by that account; the GBP wallet's own GBP hold stays clean.
func TestADR0113D_MultiAssetWallets_AndTheWalletKeyPath(t *testing.T) {
	pool := testPool(t)
	rt := wdpath.RuntimeFor(t, pool)
	f := seedFixture(t, pool)
	wdpath.SkipOnPersistentDatabase(t, pool)
	ctx := context.Background()
	gbp := uuid.New()
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, 'GBP')`,
			gbp, f.tenantID, f.brandID, f.playerAccountID); err != nil {
			return err
		}
		// C3 (migration 0174): fund the GBP wallet so its holds are FUNDED (healthy, as the EUR wallet's are from the
		// fixture deposit). An unfunded hold leaves player_cash negative, which the WD050 gate and the C3 check report.
		return negbal.Deposit(ctx, tx, f.tenantID, gbp, "GBP", 130)
	}); err != nil {
		t.Fatal(err)
	}
	g := f
	g.walletID = gbp
	plant := func(fx fixture, asset string, amount int64) uuid.UUID {
		spec := wdpath.Spec{TenantID: fx.tenantID, BrandID: fx.brandID, PlayerAccountID: fx.playerAccountID, WalletID: fx.walletID,
			AssetCode: asset, Amount: amount, IdempotencyKey: "dma-" + uuid.NewString()}
		var res wdpath.Result
		if err := pool.WithTenant(ctx, fx.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return pitest.WithoutBindingGuard(ctx, tx, func() error {
				var err error
				res, err = wdpath.Plant(ctx, tx, spec)
				return err
			})
		}); err != nil {
			t.Fatalf("plant %s: %v", asset, err)
		}
		return res.ID
	}
	plant(f, "EUR", 30)
	moved := plant(f, "EUR", 45)
	plant(g, "GBP", 60)
	gbpDone := plant(g, "GBP", 70)
	wdpath.AdvanceAsRuntime(ctx, t, pool, f.tenantID, gbpDone, withdrawal.StateCancelled, wdpath.Spec{})
	if o := dSweep(t, rt, f); o.Err != nil || o.Run.Status != StatusClean {
		t.Fatalf("two healthy wallets of two assets reconcile clean: %s err=%v", o.Run.Status, o.Err)
	}
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_immutable_fields`,
			`ALTER TABLE withdrawal_requests DISABLE TRIGGER withdrawal_requests_ledger_link_at_commit`,
			`UPDATE withdrawal_requests SET wallet_id = '` + gbp.String() + `' WHERE id = '` + moved.String() + `'`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_aa_state_transition_guard`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_immutable_fields`,
			`ALTER TABLE withdrawal_requests ENABLE TRIGGER withdrawal_requests_ledger_link_at_commit`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("owner tamper (re-point): %v", err)
	}
	o := dSweep(t, rt, f)
	if o.Err != nil || o.Run.Status != StatusMismatchesFound {
		t.Fatalf("the re-pointed request must be found: %s err=%v", o.Run.Status, o.Err)
	}
	eurHold := holdAccount(t, pool, f)
	var gbpHold uuid.UUID
	if err := pool.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM ledger_accounts WHERE wallet_id = $1 AND account_type = 'player_withdrawal_hold' AND asset_code = 'GBP'`, gbp).Scan(&gbpHold)
	}); err != nil {
		t.Fatal(err)
	}
	keys := map[string]dRow{}
	for _, r := range dKind(dMismatches(t, pool, f), MismatchKindWithdrawalHoldOpenMismatch) {
		keys[r.key] = r
	}
	wk := "wallet:" + gbp.String() + ":EUR"
	if len(keys) != 2 {
		t.Fatalf("exactly two open-hold findings expected: %+v", keys)
	}
	if r, ok := keys[wk]; !ok || !strings.Contains(r.expected, "= 45 (requests=1)") || !strings.Contains(r.actual, "= 0 ") {
		t.Fatalf("the (GBP wallet, EUR) pair with no hold account must be keyed %s (expected 45, rebuilt 0): %+v", wk, keys)
	}
	if r, ok := keys[eurHold.String()]; !ok || !strings.Contains(r.expected, "= 30 (requests=1)") || !strings.Contains(r.actual, "= 75 ") {
		t.Fatalf("the EUR hold account (expected 30, rebuilt 75) must be reported: %+v", keys)
	}
	if _, ok := keys[gbpHold.String()]; ok {
		t.Fatalf("the GBP hold account is healthy and must not be reported: %+v", keys)
	}
}

// The one-statement compare (rebuildAndProject) returns exactly what ledger.RebuildBalance and
// ledger.GetProjectedBalance return, for posted accounts, a hold account, and an account never posted
// (no projection row: Found=false).
func TestRebuildAndProject_ParityWithTheLedgerReads(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	dWithdrawal(t, pool, f, 15, withdrawal.StateCancelled)
	hold := holdAccount(t, pool, f)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		never, err := ledger.GetOrCreateAccount(ctx, tx, f.tenantID, &f.walletID, ledger.AccountPlayerBonus, "EUR")
		if err != nil {
			return err
		}
		for _, id := range []uuid.UUID{f.cashAccountID, f.clearingID, hold, never} {
			r, p, err := rebuildAndProject(ctx, tx, id)
			if err != nil {
				return err
			}
			lr, err := ledger.RebuildBalance(ctx, tx, id)
			if err != nil {
				return err
			}
			lp, err := ledger.GetProjectedBalance(ctx, tx, id)
			if err != nil {
				return err
			}
			if r.AssetCode != lr.AssetCode || r.AccountType != string(lr.AccountType) || r.DebitTotal != lr.DebitTotal || r.CreditTotal != lr.CreditTotal {
				t.Errorf("account %s rebuild: %+v vs ledger %+v", id, r, lr)
			}
			if p.Found != lp.Found || p.AssetCode != lp.AssetCode || p.AccountType != string(lp.AccountType) ||
				p.DebitTotal != lp.DebitTotal || p.CreditTotal != lp.CreditTotal {
				t.Errorf("account %s projection: %+v vs ledger %+v", id, p, lp)
			}
			if id == never && (p.Found || r.DebitTotal != 0 || r.CreditTotal != 0) {
				t.Errorf("a never-posted account has no projection row and zero totals: %+v %+v", r, p)
			}
			if id == hold && (r.DebitTotal != 15 || r.CreditTotal != 15) {
				t.Errorf("the cancelled request's hold account rebuilds 15/15: %+v", r)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
