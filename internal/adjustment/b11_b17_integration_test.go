//go:build integration

package adjustment

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
)

// B-11 (R), LF-13: a debit beyond the balance ends refused_insufficient_funds,
// committed, posting nothing; a debit equal to the balance posts (to zero).
func TestB11_NoNegativeBalance(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(1_000)
	r, err := w.submit(w.F1, w.debit(1_001, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil {
		t.Fatal(err)
	}
	if out.Executed || out.Request.State != StateRefusedInsufficientFunds || out.Request.LedgerTransactionID != nil {
		t.Fatalf("expected refused_insufficient_funds, got %+v", out.Request)
	}
	if got := w.request(r.ID); got.State != StateRefusedInsufficientFunds {
		t.Fatalf("refusal not committed: %s", got.State)
	}
	if got := w.playerCash(); got != 1_000 {
		t.Fatalf("balance moved on refusal: %d", got)
	}
	r2, err := w.submit(w.F1, w.debit(1_000, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err = w.decide(w.F2, r2, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("debit equal to balance must post: %v %+v", err, out)
	}
	if got := w.playerCash(); got != 0 {
		t.Fatalf("balance after exact debit: %d", got)
	}
	w.assertInvariants()
}

// forgeExecution runs, in one tenant-session transaction: the final
// approval (raw insert), state -> executing, a caller-built posting, and
// state -> executed linked to that posting. It returns the error of the
// final link UPDATE (the link trigger's verdict).
func (w *world) forgeExecution(t *testing.T, r Request, build func(ctx context.Context, tx pgx.Tx, playerCash, house uuid.UUID) (uuid.UUID, error)) error {
	t.Helper()
	var linkErr error
	err := w.tenantTx(w.F2, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'forge')`, w.Tenant, r.ID, r.PayloadHash, uuid.Nil); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executing' WHERE id = $1`, r.ID); err != nil {
			return err
		}
		wallet := r.WalletID
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: r.AssetCode},
			ledger.AccountSpec{AccountType: ledger.AccountManualAdjustment, AssetCode: r.AssetCode})
		if err != nil {
			return err
		}
		txID, err := build(ctx, tx, ids[0], ids[1])
		if err != nil {
			return err
		}
		_, linkErr = tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executed', ledger_transaction_id = $2 WHERE id = $1`, r.ID, txID)
		return linkErr
	})
	if linkErr != nil {
		return linkErr
	}
	return err
}

// B-12 (ADV): every non-§4 shape is unrepresentable and a forged link is
// refused (LF-10 link trigger, MA040): a wrong amount, a wrong counter
// account (house_gaming instead of manual_adjustment), a third entry, the
// wrong keys, or another request's transaction.
func TestB12_ShapeAndForgedLinkRefused(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	reason := func(r Request) *string { s := string(r.ReasonCode); return &s }

	// The account_type column is structurally player_cash only.
	var checkDef string
	if err := w.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT pg_get_constraintdef(oid) FROM pg_constraint
			WHERE conrelid = 'ledger_adjustment_requests'::regclass AND pg_get_constraintdef(oid) LIKE '%account_type%'`).Scan(&checkDef)
	}); err != nil || !strings.Contains(checkDef, "'player_cash'") {
		t.Fatalf("account_type CHECK missing: %q %v", checkDef, err)
	}
	r0, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET account_type = 'player_bonus' WHERE id = $1`, r0.ID)
		return err
	}); pgCode(err) != "MA030" && pgCode(err) != "23514" {
		t.Fatalf("account_type change: expected refusal, got %v", err)
	}

	type forge struct {
		name  string
		build func(r Request) func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) (uuid.UUID, error)
	}
	post := func(ctx context.Context, tx pgx.Tx, in ledger.TransactionInput) (uuid.UUID, error) {
		res, err := ledger.Post(ctx, tx, in)
		return res.TransactionID, err
	}
	forges := []forge{
		{"wrong amount", func(r Request) func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
			return func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) (uuid.UUID, error) {
				return post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
					IdempotencyKey: "manual_adjustment:" + r.ID.String(), CorrelationID: r.ID, ReasonCode: reason(r),
					Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: r.Amount + 1}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: r.Amount + 1}}})
			}
		}},
		{"wrong counter account", func(r Request) func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
			return func(ctx context.Context, tx pgx.Tx, pc, _ uuid.UUID) (uuid.UUID, error) {
				hg, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, nil, ledger.AccountHouseGaming, r.AssetCode)
				if err != nil {
					return uuid.Nil, err
				}
				return post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
					IdempotencyKey: "manual_adjustment:" + r.ID.String(), CorrelationID: r.ID, ReasonCode: reason(r),
					Entries: []ledger.EntryInput{{LedgerAccountID: hg, Direction: ledger.Debit, Amount: r.Amount}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: r.Amount}}})
			}
		}},
		{"third entry", func(r Request) func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
			return func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) (uuid.UUID, error) {
				return post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
					IdempotencyKey: "manual_adjustment:" + r.ID.String(), CorrelationID: r.ID, ReasonCode: reason(r),
					Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: r.Amount + 5},
						{LedgerAccountID: pc, Direction: ledger.Credit, Amount: r.Amount}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: 5}}})
			}
		}},
		{"wrong keys", func(r Request) func(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) (uuid.UUID, error) {
			return func(ctx context.Context, tx pgx.Tx, pc, house uuid.UUID) (uuid.UUID, error) {
				return post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
					IdempotencyKey: "manual_adjustment:forged-" + r.ID.String(), CorrelationID: r.ID, ReasonCode: reason(r),
					Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: r.Amount}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: r.Amount}}})
			}
		}},
	}
	for _, f := range forges {
		t.Run(f.name, func(t *testing.T) {
			r, err := w.submit(w.F1, w.credit(333, ReasonOperationalErrorCorrection))
			if err != nil {
				t.Fatal(err)
			}
			if err := w.forgeExecution(t, r, f.build(r)); pgCode(err) != "MA040" {
				t.Fatalf("%s: expected MA040, got %v", f.name, err)
			}
			if got := w.request(r.ID); got.State != StatePending {
				t.Fatalf("%s: forged execution committed: %s", f.name, got.State)
			}
		})
	}

	// Linking to ANOTHER executed request's transaction.
	done, err := w.submit(w.F1, w.credit(333, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	out, err := w.decide(w.F2, done, DecisionApprove)
	if err != nil || !out.Executed {
		t.Fatalf("execute donor: %v", err)
	}
	r, err := w.submit(w.F1, w.credit(333, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	err = w.forgeExecution(t, r, func(ctx context.Context, tx pgx.Tx, _, _ uuid.UUID) (uuid.UUID, error) {
		return *out.Request.LedgerTransactionID, nil
	})
	if pgCode(err) != "MA040" && pgCode(err) != "23505" {
		t.Fatalf("link to another request's transaction: expected MA040/23505, got %v", err)
	}
	w.assertInvariants()
}

// B-13 (R), LF ruling 1 (literal): compensating_entry with another wallet's,
// another asset's, a deposit / deposit_reversal / tombstone causation, a
// cumulative excess, or a missing hash -> refused; goodwill debit refused;
// external_instruction without a hash refused.
func TestB13_ReasonCodeCatalogueRules(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	w.fund(10_000)
	bet := w.postCasino(ledger.TxCasinoBet, 1_000)

	comp := func(cause uuid.UUID, amount int64) SubmitInput {
		in := w.credit(amount, ReasonCompensatingEntry)
		in.CausationTransactionID = &cause
		return in
	}
	// Another wallet (a second player's bet).
	_, _, otherWallet := w.newPlayer()
	saved := w.Wallet
	w.Wallet = otherWallet
	otherBet := w.postCasino(ledger.TxCasinoBet, 500)
	w.Wallet = saved
	if _, err := w.submit(w.F1, comp(otherBet, 10)); pgCode(err) != "MA022" {
		t.Fatalf("another wallet's causation: expected MA022, got %v", err)
	}
	// Another asset: the player's second-asset wallet has its own causation.
	other := newWorld(t, worldOpts{base: 1})
	otherAssetWallet := uuid.New()
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO wallets (id, tenant_id, brand_id, player_account_id, asset_code) VALUES ($1, $2, $3, $4, $5)`,
			otherAssetWallet, w.Tenant, w.Brand, w.Player, other.Asset)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	savedAsset := w.Asset
	w.Wallet, w.Asset = otherAssetWallet, other.Asset
	otherAssetBet := w.postCasino(ledger.TxCasinoBet, 300)
	w.Wallet, w.Asset = saved, savedAsset
	if _, err := w.submit(w.F1, comp(otherAssetBet, 10)); pgCode(err) != "MA022" {
		t.Fatalf("another asset's causation: expected MA022, got %v", err)
	}
	// deposit / deposit_reversal / tombstone causations.
	dep, rev, tomb := w.postDepositFamily(700)
	for name, cause := range map[string]uuid.UUID{"deposit": dep, "deposit_reversal": rev, "tombstone": tomb} {
		for _, reasonIn := range []SubmitInput{comp(cause, 10), func() SubmitInput {
			in := w.credit(10, ReasonOperationalErrorCorrection)
			in.CausationTransactionID = &cause
			return in
		}()} {
			if _, err := w.submit(w.F1, reasonIn); pgCode(err) != "MA022" {
				t.Fatalf("%s causation (%s): expected MA022, got %v", name, reasonIn.ReasonCode, err)
			}
		}
	}
	// Missing evidence hash.
	noHash := comp(bet, 10)
	noHash.EvidenceRefHash = nil
	if _, err := w.submit(w.F1, noHash); pgCode(err) != "MA022" {
		t.Fatalf("compensating_entry without evidence: expected MA022, got %v", err)
	}
	// Missing causation.
	noCause := w.credit(10, ReasonCompensatingEntry)
	if _, err := w.submit(w.F1, noCause); pgCode(err) != "MA022" {
		t.Fatalf("compensating_entry without causation: expected MA022, got %v", err)
	}
	// Cumulative excess: 600 executed, then 500 refused at submission.
	r, err := w.submit(w.F1, comp(bet, 600))
	if err != nil {
		t.Fatalf("first compensation: %v", err)
	}
	if out, err := w.decide(w.F2, r, DecisionApprove); err != nil || !out.Executed {
		t.Fatalf("execute first compensation: %v %+v", err, out)
	}
	if _, err := w.submit(w.F1, comp(bet, 500)); pgCode(err) != "MA022" {
		t.Fatalf("cumulative excess: expected MA022, got %v", err)
	}
	if r2, err := w.submit(w.F1, comp(bet, 400)); err != nil {
		t.Fatalf("remaining headroom (400) must be allowed: %v", err)
	} else if out, err := w.decide(w.F2, r2, DecisionApprove); err != nil || !out.Executed {
		t.Fatalf("execute to the cap: %v %+v", err, out)
	}
	// A causation is forbidden for goodwill; goodwill never debits.
	gw := w.credit(10, ReasonGoodwillCredit)
	gw.CausationTransactionID = &bet
	if _, err := w.submit(w.F1, gw); pgCode(err) != "MA022" && pgCode(err) != "MA021" {
		t.Fatalf("goodwill with causation: expected refusal, got %v", err)
	}
	if _, err := w.submit(w.F1, w.debit(10, ReasonGoodwillCredit)); pgCode(err) != "MA022" {
		t.Fatalf("goodwill debit: expected MA022, got %v", err)
	}
	ext := w.credit(10, ReasonExternalInstruction)
	ext.EvidenceRefHash = nil
	if _, err := w.submit(w.F1, ext); pgCode(err) != "MA022" {
		t.Fatalf("external_instruction without hash: expected MA022, got %v", err)
	}
	w.assertInvariants()
}

// postDepositFamily posts a deposit, its deposit_reversal and a tombstone
// for the world's wallet (psp_clearing <-> player_cash) and returns their ids.
func (w *world) postDepositFamily(amount int64) (dep, rev, tomb uuid.UUID) {
	w.t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		wallet := w.Wallet
		ids, err := ledger.GetOrCreateAccounts(ctx, tx, w.Tenant,
			ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: w.Asset},
			ledger.AccountSpec{AccountType: ledger.AccountPSPClearing, AssetCode: w.Asset})
		if err != nil {
			return err
		}
		pid := "k2-mock-psp"
		ref1, ref2, ref3 := uuid.NewString(), uuid.NewString(), uuid.NewString()
		d, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxDeposit, IdempotencyKey: pid + ":" + ref1,
			ProviderID: &pid, ProviderTxID: &ref1, CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{{LedgerAccountID: ids[1], Direction: ledger.Debit, Amount: amount}, {LedgerAccountID: ids[0], Direction: ledger.Credit, Amount: amount}}})
		if err != nil {
			return err
		}
		dep = d.TransactionID
		r, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxDepositReversal, IdempotencyKey: pid + ":" + ref2,
			ProviderID: &pid, ProviderTxID: &ref2, CorrelationID: uuid.New(), ReversesTransactionID: &dep,
			Entries: []ledger.EntryInput{{LedgerAccountID: ids[0], Direction: ledger.Debit, Amount: amount}, {LedgerAccountID: ids[1], Direction: ledger.Credit, Amount: amount}}})
		if err != nil {
			return err
		}
		rev = r.TransactionID
		tb, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxTombstone, IdempotencyKey: pid + ":tomb:" + ref3,
			ProviderID: &pid, ProviderTxID: &ref3, CorrelationID: uuid.New()})
		tomb = tb.TransactionID
		return err
	}); err != nil {
		w.t.Fatalf("deposit family: %v", err)
	}
	return dep, rev, tomb
}

// B-14 (R): an above-threshold requirement without an asset is a CHECK
// violation (LF-12); an amount of 2^63 is refused.
func TestB14_ThresholdNeedsAssetAndAmountBound(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	thr := "1000"
	_, err := w.proposePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
		BaseRequiredApprovals: intPtr(1), ThresholdMinorUnits: &thr, RequiredApprovalsAboveThreshold: intPtr(2)}, w.AdminA)
	if pgCode(err) != "23514" {
		t.Fatalf("threshold without asset: expected 23514, got %v", err)
	}
	// Synthetic, test-only threshold WITH the asset: accepted; above it,
	// the higher requirement applies.
	w.approvePolicy(PolicyChangeInput{ChangeKind: ChangeKindPolicy, OperationKind: OperationKind, Level: LevelPlatform,
		AssetCode: strPtr(w.Asset), BaseRequiredApprovals: intPtr(1), ThresholdMinorUnits: &thr, RequiredApprovalsAboveThreshold: intPtr(2)}, w.AdminA, w.AdminB)
	if ev := w.evaluate(OperationKind, i64(1000)); ev.Required != 1 {
		t.Fatalf("at threshold: %+v", ev)
	}
	if ev := w.evaluate(OperationKind, i64(1001)); ev.Required != 2 {
		t.Fatalf("above threshold: %+v", ev)
	}
	err = w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_requests (tenant_id, wallet_id, player_account_id, brand_id, asset_code, direction, amount, reason_code,
			note_hash, payload_hash, initiated_by, initiated_by_scope, initiated_by_person_id, tenant_status_at_submission, required_at_submission,
			contributing_policy_ids, expires_at)
			VALUES ($1, $2, $3, $3, $4, 'credit_player', 9223372036854775808, 'operational_error_correction', repeat('a', 64), '-', $3, 'tenant', $3, '-', 1, '{}', now())`,
			w.Tenant, w.Wallet, uuid.Nil, w.Asset)
		return err
	})
	if pgCode(err) != "23514" {
		t.Fatalf("2^63 amount: expected 23514, got %v", err)
	}
	if _, err := w.submit(w.F1, w.credit(0, ReasonOperationalErrorCorrection)); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("zero amount: expected ErrInvalidInput, got %v", err)
	}
}

// B-15 (FL/RB): a crash between steps 7 and 10 commits nothing; a committed
// 'executing' row is refused by the deferred check (MA041).
func TestB15_CrashBetweenExecutingAndExecutedCommitsNothing(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	r, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	boom := errors.New("injected crash before ledger.Post")
	testHookBeforePost = func(ctx context.Context, id uuid.UUID) error { return boom }
	_, err = w.decide(w.F2, r, DecisionApprove)
	testHookBeforePost = nil
	if !errors.Is(err, boom) {
		t.Fatalf("expected the injected failure, got %v", err)
	}
	got := w.request(r.ID)
	if got.State != StatePending || got.LedgerTransactionID != nil {
		t.Fatalf("partial execution committed: %+v", got)
	}
	var approvals, txs int
	if err := w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_adjustment_approvals WHERE request_id = $1`, r.ID).Scan(&approvals); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE correlation_id = $1`, r.ID).Scan(&txs)
	}); err != nil {
		t.Fatal(err)
	}
	if approvals != 0 || txs != 0 {
		t.Fatalf("rolled-back execution left approvals=%d txs=%d", approvals, txs)
	}
	// Retry succeeds (idempotent recovery).
	if out, err := w.decide(w.F2, r, DecisionApprove); err != nil || !out.Executed {
		t.Fatalf("retry after crash: %v %+v", err, out)
	}

	// A committed 'executing' is refused at COMMIT.
	r2, err := w.submit(w.F1, w.credit(500, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	err = w.tenantTx(w.F3, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO ledger_adjustment_approvals (tenant_id, request_id, decision, payload_hash, decided_by, decided_by_scope, decided_by_person_id, decided_txid, reason_code)
			VALUES ($1, $2, 'approve', $3, $4, 'tenant', $4, 0, 'x')`, w.Tenant, r2.ID, r2.PayloadHash, uuid.Nil); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE ledger_adjustment_requests SET state = 'executing' WHERE id = $1`, r2.ID)
		return err
	})
	if pgCode(err) != "MA041" {
		t.Fatalf("committed executing: expected MA041, got %v", err)
	}
	if got := w.request(r2.ID); got.State != StatePending {
		t.Fatalf("executing committed: %s", got.State)
	}
	w.assertInvariants()
}

// B-16 (ADV) + LF N-2: under an acting session the ledger fence refuses an
// unexecuting manual_adjustment and a casino_bet (CG030); with the 0113
// acting SELECT on ledger_accounts in place, a non-governed ledger_entries
// insert (appending to an existing transaction) is still refused (CG030);
// the projection fence refuses a direct update and a non-zero insert.
func TestB16_ActingFences(t *testing.T) {
	w := newWorld(t, worldOpts{base: 1})
	existing := w.fund(1_000)
	ctx := context.Background()
	var results []string
	err := w.pool.WithPlatformActingInTenant(ctx, w.Acting.ID, w.Tenant, uuid.Nil, OperationKind, func(ctx context.Context, tx pgx.Tx) error {
		wallet := w.Wallet
		pc, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, &wallet, ledger.AccountPlayerCash, w.Asset)
		if err != nil {
			return err
		}
		house, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, nil, ledger.AccountManualAdjustment, w.Asset)
		if err != nil {
			return err
		}
		probe := func(name, want string, fn func() error) {
			if _, err := tx.Exec(ctx, `SAVEPOINT f`); err != nil {
				results = append(results, name+": "+err.Error())
				return
			}
			err := fn()
			if pgCode(err) != want {
				results = append(results, name+": want "+want+", got "+errString(err))
			}
			_, _ = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT f`)
		}
		reason := "operational_error_correction"
		probe("unexecuting manual_adjustment", "CG030", func() error {
			id := uuid.New()
			_, err := ledger.Post(ctx, tx, ledger.TransactionInput{TenantID: w.Tenant, TransactionType: ledger.TxManualAdjustment,
				IdempotencyKey: "manual_adjustment:" + id.String(), CorrelationID: id, ReasonCode: &reason,
				Entries: []ledger.EntryInput{{LedgerAccountID: house, Direction: ledger.Debit, Amount: 5}, {LedgerAccountID: pc, Direction: ledger.Credit, Amount: 5}}})
			return err
		})
		probe("casino_bet", "CG030", func() error {
			pid, ref := "k2", uuid.NewString()
			_, err := tx.Exec(ctx, `INSERT INTO ledger_transactions (tenant_id, transaction_type, idempotency_key, provider_id, provider_tx_id, correlation_id)
				VALUES ($1, 'casino_bet', $2, $3, $4, $5)`, w.Tenant, "x:"+ref, pid, ref, uuid.New())
			return err
		})
		probe("N-2: append entries to an existing transaction", "CG030", func() error {
			_, err := tx.Exec(ctx, `INSERT INTO ledger_entries (ledger_transaction_id, ledger_account_id, tenant_id, asset_code, direction, amount)
				VALUES ($1, $2, $3, $4, 'credit', 1)`, existing, pc, w.Tenant, w.Asset)
			return err
		})
		probe("projection direct update", "CG031", func() error {
			_, err := tx.Exec(ctx, `UPDATE wallet_balance_projection SET credit_total = credit_total + 1 WHERE ledger_account_id = $1`, pc)
			return err
		})
		probe("projection RebuildProjectionRow", "CG031", func() error {
			_, err := ledger.RebuildProjectionRow(ctx, tx, pc)
			return err
		})
		probe("projection non-zero insert", "CG031", func() error {
			newAcct, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, nil, ledger.AccountManualAdjustment, w.Asset)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO wallet_balance_projection (ledger_account_id, tenant_id, asset_code, account_type, debit_total, credit_total)
				VALUES ($1, $2, $3, 'manual_adjustment', 0, 7) ON CONFLICT (ledger_account_id) DO UPDATE SET credit_total = EXCLUDED.credit_total`, newAcct, w.Tenant, w.Asset)
			return err
		})
		probe("house_gaming account creation", "42501", func() error {
			_, err := ledger.GetOrCreateAccount(ctx, tx, w.Tenant, nil, ledger.AccountHouseGaming, w.Asset+"X")
			return unwrapPg(err)
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) > 0 {
		t.Fatalf("acting fence probes failed:\n  %s", strings.Join(results, "\n  "))
	}
	w.assertInvariants()
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

func unwrapPg(err error) error { return err }

// B-17 (AU): one audit row per transition (submission, approval, execution;
// submission, rejection; submission, cancellation), with the bounded note
// in the submission's metadata; a note over 1000 bytes or with control
// characters is refused in Go AND by the DB function.
func TestB17_AuditPerTransitionAndNoteBound(t *testing.T) {
	w := newWorld(t, worldOpts{base: 2})
	r, err := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.decide(w.F2, r, DecisionApprove); err != nil {
		t.Fatal(err)
	}
	if out, err := w.decide(w.F3, r, DecisionApprove); err != nil || !out.Executed {
		t.Fatalf("execute: %v", err)
	}
	if got := strings.Join(w.auditActions(r.ID), ","); countOf(got, "ledger_adjustment.submitted") != 1 ||
		countOf(got, "ledger_adjustment.approved") != 1 || countOf(got, "ledger_adjustment.executed") != 1 {
		t.Fatalf("audit rows for an executed request: %s", got)
	}
	rj, _ := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
	if _, err := w.decide(w.F2, rj, DecisionReject); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.auditActions(rj.ID), ","); got != "ledger_adjustment.submitted,ledger_adjustment.rejected" {
		t.Fatalf("audit rows for a rejected request: %s", got)
	}
	if st := w.request(rj.ID).State; st != StateRejected {
		t.Fatalf("one reject must end the request: %s", st)
	}
	rc, _ := w.submit(w.F1, w.credit(10, ReasonOperationalErrorCorrection))
	if _, err := w.svc.Cancel(ctxFor(w.F2), w.target(), rc.ID, Meta{}); pgCode(err) != "MA030" {
		t.Fatalf("cancel by a non-initiator: expected MA030, got %v", err)
	}
	if _, err := w.svc.Cancel(ctxFor(w.F1), w.target(), rc.ID, Meta{}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(w.auditActions(rc.ID), ","); got != "ledger_adjustment.submitted,ledger_adjustment.cancelled" {
		t.Fatalf("audit rows for a cancelled request: %s", got)
	}
	// The note is in the submission metadata, bounded.
	var note string
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT metadata->>'note' FROM audit_log WHERE tenant_id = $1 AND target_id = $2 AND action = 'ledger_adjustment.submitted'`,
			w.Tenant, r.ID.String()).Scan(&note)
	}); err != nil || note != "k2 test adjustment" {
		t.Fatalf("note in audit metadata: %q %v", note, err)
	}
	for name, bad := range map[string]string{"too long": strings.Repeat("x", 1001), "control char": "bad\x07note", "empty": "", "C1 control": "bad\u0085note"} {
		in := w.credit(10, ReasonOperationalErrorCorrection)
		in.Note = bad
		if _, err := w.submit(w.F1, in); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("%s note (Go): expected ErrInvalidInput, got %v", name, err)
		}
		err := w.tenantTx(w.F1, func(ctx context.Context, tx pgx.Tx) error {
			var h string
			return tx.QueryRow(ctx, `SELECT ledger_adjustment_note_hash($1)`, bad).Scan(&h)
		})
		if pgCode(err) != "MA025" {
			t.Fatalf("%s note (DB): expected MA025, got %v", name, err)
		}
	}
	w.assertInvariants()
}

func countOf(s, sub string) int { return strings.Count(s, sub) }
