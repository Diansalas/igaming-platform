//go:build integration

// ADR 0113 slice 2 (migration 0133, s4.3 / s7 C8): the LEDGER closure arm LA025. A closure is refused
// while a player_withdrawal_hold balance of the subject's player wallets is not zero, also when no
// withdrawal request still holds it (a stranded hold, the HD-CTF-4 population: closure refused,
// suspension is the lever, ADR 0113 s8 Q1). LA023 (hold-bearing requests) stays alongside it.
//
// The stranded hold is an OWNER TAMPER plant (ADR 0110 9.4): since migration 0132 every hold entry is
// the pinned leg of a request (B-i / B-ii), so a hold leg that no request names can only be history
// written with those two triggers disabled. The rows are permanent (append-only ledger): the tests
// that plant one skip on a persistent shared database (wdpath.SkipOnPersistentDatabase) and run on a
// private or CI service database.
package launchgov_test

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/launchgov"
	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/wdpath"
)

// strandHold posts, as the table OWNER with the two 0132 hold-entry triggers disabled for this one
// posting, a withdrawal_requested leg Dr player_cash / Cr player_withdrawal_hold on walletID that NO
// withdrawal request names (a stranded hold of `amount`).
func (w *world) strandHold(tenantID, walletID uuid.UUID, amount int64) {
	w.t.Helper()
	wdpath.SkipOnPersistentDatabase(w.t, w.owner)
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []string{
			`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
			`ALTER TABLE ledger_entries DISABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		accounts, err := ledger.GetOrCreateAccounts(ctx, tx, tenantID,
			ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerCash, AssetCode: "EUR"},
			ledger.AccountSpec{WalletID: &walletID, AccountType: ledger.AccountPlayerWithdrawalHold, AssetCode: "EUR"})
		if err != nil {
			return err
		}
		if _, err := ledger.Post(ctx, tx, ledger.TransactionInput{
			TenantID: tenantID, TransactionType: ledger.TxWithdrawalRequested,
			IdempotencyKey: "la025-stranded-" + uuid.NewString(), CorrelationID: uuid.New(),
			Entries: []ledger.EntryInput{
				{LedgerAccountID: accounts[0], Direction: ledger.Debit, Amount: amount},
				{LedgerAccountID: accounts[1], Direction: ledger.Credit, Amount: amount},
			},
		}); err != nil {
			return err
		}
		for _, q := range []string{
			`SET CONSTRAINTS ledger_entries_balanced IMMEDIATE`,
			`SET CONSTRAINTS ledger_entries_balanced DEFERRED`,
			`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_guard`,
			`ALTER TABLE ledger_entries ENABLE TRIGGER ledger_entries_withdrawal_hold_link_at_commit`,
		} {
			if _, err := tx.Exec(ctx, q); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		w.t.Fatalf("owner tamper plant (stranded hold): %v", err)
	}
}

// ownerCounts is the OWNER-side truth for a subject (tenant scope bound by the owner): hold-bearing
// requests and non-zero hold accounts. Used to prove a refusal or a pass is not a blind read.
func (w *world) ownerCounts(sub launchgov.Subject) (requests, holdAccounts int64) {
	w.t.Helper()
	if err := w.owner.WithTenant(context.Background(), sub.TenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_requests WHERE tenant_id = $1 AND ($2::uuid IS NULL OR brand_id = $2)
			AND state IN ('requested', 'pending_review', 'approved', 'submitted')`, sub.TenantID, sub.BrandID).Scan(&requests); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM ledger_accounts la
			WHERE la.tenant_id = $1 AND la.account_type = 'player_withdrawal_hold'
			  AND ($2::uuid IS NULL OR la.wallet_id IN (SELECT id FROM wallets WHERE brand_id = $2))
			  AND (SELECT COALESCE(sum(CASE e.direction WHEN 'credit' THEN e.amount ELSE -e.amount END), 0)
			         FROM ledger_entries e WHERE e.ledger_account_id = la.id) <> 0`, sub.TenantID, sub.BrandID).Scan(&holdAccounts)
	}); err != nil {
		w.t.Fatal(err)
	}
	return requests, holdAccounts
}

func wantLA025(t *testing.T, what string, err error, n int) {
	t.Helper()
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "LA025" {
		t.Fatalf("%s: want SQLSTATE LA025, got %v", what, err)
	}
	if want := "withdrawal_hold_accounts_non_zero=" + strconv.Itoa(n); pe.Detail != want {
		t.Fatalf("%s: detail must carry the count only: %q (want %q)", what, pe.Detail, want)
	}
}

// C8 + HD-CTF-4: a stranded hold balance with NO hold-bearing request refuses the brand closure and
// the tenant closure on the key-holder path (raw governed SQL, no Go service), for the brand that
// holds it only; another brand of the same tenant closes; suspension stays possible (the lever).
// The tenant closure runs from a platform session with app.tenant_id UNSET, so it passes only if the
// projection read runs inside the guard's RLS-bound window (a blind read counts 0 and lets the
// closure through); the brand closures prove the wallets.brand_id join is read (a blind join counts
// 0 for the holding brand).
func TestLA025_StrandedHold_NoHoldBearingRequest_RefusesClosure_KeyHolderPath(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	_, w1 := w.playerWithWallet(tid, b1)
	w.strandHold(tid, w1, 700)
	s1 := launchgov.Subject{TenantID: tid, BrandID: &b1}
	st := launchgov.Subject{TenantID: tid}

	if reqs, holds := w.ownerCounts(s1); reqs != 0 || holds != 1 {
		t.Fatalf("precondition (owner view): no hold-bearing request and one non-zero hold account, got %d / %d", reqs, holds)
	}
	wantLA025(t, "brand closure over a stranded hold", launchfix.TrySetBrandStatus(ctx, tid, b1, "closed"), 1)
	wantLA025(t, "tenant closure over a stranded hold", launchfix.TrySetTenantStatus(ctx, tid, "closed"), 1)
	if s := w.status(s1); s != "active" {
		t.Fatalf("the refused brand closure must leave it active, got %s", s)
	}
	if s := w.status(st); s != "active" {
		t.Fatalf("the refused tenant closure must leave it active, got %s", s)
	}
	// Brand scoping: brand 2 holds nothing and closes.
	if err := launchfix.TrySetBrandStatus(ctx, tid, b2, "closed"); err != nil {
		t.Fatalf("another brand's stranded hold must not block this brand's closure: %v", err)
	}
	// Suspension is the lever (ADR 0113 s8 Q1): it is not refused, and the closure stays refused
	// from the suspended state.
	if err := launchfix.TrySetBrandStatus(ctx, tid, b1, "suspended"); err != nil {
		t.Fatalf("suspension must stay possible over a stranded hold: %v", err)
	}
	wantLA025(t, "closure from suspended over a stranded hold", launchfix.TrySetBrandStatus(ctx, tid, b1, "closed"), 1)
}

// Two hold accounts in two brands: the tenant counts both, each brand only its own.
func TestLA025_TenantCountsEveryBrand_BrandCountsItsOwn(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	_, w1 := w.playerWithWallet(tid, b1)
	_, w2 := w.playerWithWallet(tid, b2)
	w.strandHold(tid, w1, 100)
	w.strandHold(tid, w2, 200)
	wantLA025(t, "tenant", launchfix.TrySetTenantStatus(ctx, tid, "closed"), 2)
	wantLA025(t, "brand 1", launchfix.TrySetBrandStatus(ctx, tid, b1, "closed"), 1)
	wantLA025(t, "brand 2", launchfix.TrySetBrandStatus(ctx, tid, b2, "closed"), 1)
}

// A hold account whose legs net to zero (a request that was cancelled: hold and release) blocks
// nothing (the credit_total <> debit_total filter); LA023 still fires first for a hold-bearing
// request, and the subject that holds both is refused LA023 (LA023 stays alongside LA025).
func TestLA025_ZeroNetHoldDoesNotBlock_LA023StaysFirst(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	p1, w1 := w.playerWithWallet(tid, b1)
	w.withdrawal(tid, b1, p1, w1, "cancelled")
	var rows int64
	if err := w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM wallet_balance_projection WHERE wallet_id = $1 AND account_type = 'player_withdrawal_hold' AND credit_total = debit_total AND credit_total > 0`, w1).Scan(&rows)
	}); err != nil || rows != 1 {
		t.Fatalf("precondition: one zero-net hold projection row with history, got %d err=%v", rows, err)
	}
	if err := launchfix.TrySetBrandStatus(ctx, tid, b1, "closed"); err != nil {
		t.Fatalf("a zero-net hold account must not block the closure: %v", err)
	}
	// LA023 first: a hold-bearing request (its hold is also a non-zero balance) is reported LA023.
	p2, w2 := w.playerWithWallet(tid, b2)
	w.withdrawal(tid, b2, p2, w2, "requested")
	if got := pgCode(launchfix.TrySetBrandStatus(ctx, tid, b2, "closed")); got != "LA023" {
		t.Fatalf("a hold-bearing request must still be refused LA023 first, got %q", got)
	}
}

// The Go arm: readiness reports the stranded hold (Info + the closure notice), the closure request is
// refused at filing with the typed precondition H6HoldBalanceNonZero (ErrWithdrawalsHolding), audited
// once, and the executor re-checks under the subject locks: a closure filed while nothing was held is
// refused at the final approval when a stranded hold appeared in between (rolled back, stays pending).
func TestLA025_GoArm_ReadinessRequestAndExecutor(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	tid := w.activeTenant()
	b1, b2 := w.activeBrand(tid), w.activeBrand(tid)
	_, w1 := w.playerWithWallet(tid, b1)
	w.strandHold(tid, w1, 300)
	s1 := launchgov.Subject{TenantID: tid, BrandID: &b1}

	rd, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: s1, Action: launchgov.ActionClose})
	if err != nil {
		t.Fatal(err)
	}
	if rd.Info["withdrawal_hold_accounts_non_zero"] != 1 || rd.Info["hold_bearing_withdrawals"] != 0 {
		t.Fatalf("readiness must count the stranded hold account and no request: %+v", rd.Info)
	}
	if !containsStr(rd.Notices, launchgov.NoticeFundsHoldBlocksClosure) || !containsStr(rd.Notices, launchgov.NoticeFundsStranded) {
		t.Fatalf("a closure readiness over a stranded hold must carry the LA025 notice: %+v", rd.Notices)
	}
	rs, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: s1, Action: launchgov.ActionSuspend})
	if err != nil || containsStr(rs.Notices, launchgov.NoticeFundsHoldBlocksClosure) {
		t.Fatalf("a suspension readiness must not carry the closure notice: %+v err=%v", rs.Notices, err)
	}

	in := launchgov.RequestInput{Subject: s1, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
	in.AcknowledgedCheckCodes = w.nonPassing(in)
	var pe *launchgov.PreconditionError
	if _, err := w.svc.Request(ctx, w.req, in); !errors.Is(err, launchgov.ErrWithdrawalsHolding) || !errors.As(err, &pe) ||
		!contains(pe.Codes, launchgov.H6HoldBalanceNonZero) || contains(pe.Codes, launchgov.H6WithdrawalsHolding) {
		t.Fatalf("closing over a stranded hold must be refused with H6HoldBalanceNonZero only: %v", err)
	}
	if rows := w.audit(tid, "launch_authorisation.request_denied"); len(rows) != 1 || rows[0]["code"] != "LAUNCH_WITHDRAWALS_HOLDING_FUNDS" {
		t.Fatalf("the refusal is audited once: %+v", rows)
	}

	// Executor: filed and first-approved with nothing held; a stranded hold appears; the final
	// approval is refused under the locks and the request stays pending.
	s2 := launchgov.Subject{TenantID: tid, BrandID: &b2}
	_, w2 := w.playerWithWallet(tid, b2)
	in2 := launchgov.RequestInput{Subject: s2, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
	r2 := w.mustRequest(w.req, in2)
	if _, err := w.approve(w.ap1, r2); err != nil {
		t.Fatal(err)
	}
	w.strandHold(tid, w2, 50)
	_, err = w.approve(w.ap2, r2)
	if !errors.Is(err, launchgov.ErrWithdrawalsHolding) || !errors.As(err, &pe) || !contains(pe.Codes, launchgov.H6HoldBalanceNonZero) {
		t.Fatalf("a stranded hold appearing before the final approval must refuse the closure (H6HoldBalanceNonZero): %v", err)
	}
	got, apps, _ := w.svc.GetRequest(ctx, w.ap1, tid, r2.ID)
	if got.Status != launchgov.ReqPending || len(apps) != 1 {
		t.Fatalf("a refused closure stays pending with the earlier approval only: %+v apps=%d", got, len(apps))
	}
	if st := w.status(s2); st != "active" {
		t.Fatalf("brand 2 status %s", st)
	}

	// A zero-net hold account (a cancelled request: hold and release) is not a hold balance for the Go
	// arm either: readiness counts 0 and the closure request is admitted.
	b3 := w.activeBrand(tid)
	s3 := launchgov.Subject{TenantID: tid, BrandID: &b3}
	p3, w3 := w.playerWithWallet(tid, b3)
	w.withdrawal(tid, b3, p3, w3, "cancelled")
	rd3, err := w.svc.EvaluateReadiness(ctx, launchgov.EvalInput{Subject: s3, Action: launchgov.ActionClose})
	if err != nil || rd3.Info["withdrawal_hold_accounts_non_zero"] != 0 || containsStr(rd3.Notices, launchgov.NoticeFundsHoldBlocksClosure) {
		t.Fatalf("a zero-net hold account is not a hold balance: %+v err=%v", rd3.Info, err)
	}
	in3 := launchgov.RequestInput{Subject: s3, Action: launchgov.ActionClose, ReasonCode: "offboarding"}
	in3.AcknowledgedCheckCodes = w.nonPassing(in3)
	w.mustRequest(w.req, in3)
}

// Concurrency with a NEW hold (ADR 0113 s4.3 race), both orders, brand and tenant. The only
// hold-increasing leg is E0b, posted with E0, whose insert takes the gate keys SHARED (0131):
//   - hold first: the real withdrawal.RequestWithdrawal holds the keys; the closure WAITS, then is
//     refused (LA023, and the committed hold is a non-zero balance LA025 also sees: asserted from the
//     owner side, and the mutation run disables LA023 to show LA025 alone refuses);
//   - closure first: the closure holds the keys EXCLUSIVE; the request waits, then is refused (no
//     hold is posted), so a closed subject never acquires a hold balance.
func TestLA025_RaceWithANewHold_BothOrders(t *testing.T) {
	for _, kind := range []string{"brand", "tenant"} {
		kind := kind
		t.Run(kind+"/hold_first", func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			tid := w.activeTenant()
			b := w.activeBrand(tid)
			request := w.withdrawalFixture(tid, b)
			sub := launchgov.Subject{TenantID: tid}
			if kind == "brand" {
				sub.BrandID = &b
			}
			started, release := make(chan struct{}), make(chan struct{})
			wdDone := make(chan error, 1)
			go func() {
				wdDone <- w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
					if _, err := request(ctx, tx); err != nil {
						return err
					}
					close(started)
					<-release
					return nil
				})
			}()
			select {
			case <-started:
			case err := <-wdDone:
				t.Fatalf("the withdrawal request failed before the closure: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatal("the withdrawal request did not start")
			}
			closeDone := make(chan error, 1)
			go func() {
				if kind == "brand" {
					closeDone <- launchfix.TrySetBrandStatus(ctx, tid, b, "closed")
				} else {
					closeDone <- launchfix.TrySetTenantStatus(ctx, tid, "closed")
				}
			}()
			select {
			case err := <-closeDone:
				close(release)
				t.Fatalf("the closure must WAIT for the in-flight hold, but returned: %v", err)
			case <-time.After(800 * time.Millisecond):
			}
			close(release)
			if err := <-wdDone; err != nil {
				t.Fatalf("the withdrawal request must commit: %v", err)
			}
			select {
			case err := <-closeDone:
				if got := pgCode(err); got != "LA023" && got != "LA025" {
					t.Fatalf("after the hold committed the closure must be refused (LA023 / LA025), got %v", err)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the closure never returned")
			}
			if st := w.status(sub); st != "active" {
				t.Fatalf("the refused closure must leave the subject active, got %s", st)
			}
			if _, holds := w.ownerCounts(sub); holds != 1 {
				t.Fatalf("the committed hold is a non-zero hold balance LA025 counts: %d", holds)
			}
		})
		t.Run(kind+"/closure_first", func(t *testing.T) {
			w := newWorld(t)
			ctx := context.Background()
			tid := w.activeTenant()
			b := w.activeBrand(tid)
			request := w.withdrawalFixture(tid, b)
			sub := launchgov.Subject{TenantID: tid}
			if kind == "brand" {
				sub.BrandID = &b
			}
			var mu sync.Mutex
			wdDone := make(chan error, 1)
			blocked := false
			hold := func() {
				go func() {
					wdDone <- w.owner.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
						_, err := request(ctx, tx)
						return err
					})
				}()
				select {
				case err := <-wdDone:
					wdDone <- err
				case <-time.After(800 * time.Millisecond):
					mu.Lock()
					blocked = true
					mu.Unlock()
				}
			}
			var err error
			if kind == "brand" {
				err = launchfix.TrySetBrandStatusHoldingOn(ctx, t, w.owner, tid, b, "closed", hold)
			} else {
				err = launchfix.TrySetTenantStatusHoldingOn(ctx, t, w.owner, tid, "closed", hold)
			}
			if err != nil {
				t.Fatalf("the closure (no hold existed) must commit: %v", err)
			}
			mu.Lock()
			wasBlocked := blocked
			mu.Unlock()
			if !wasBlocked {
				t.Fatal("the withdrawal request must WAIT while the closure holds the gate")
			}
			select {
			case werr := <-wdDone:
				if werr == nil {
					t.Fatal("the request after the closure must be refused")
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the withdrawal request never returned")
			}
			if reqs, holds := w.ownerCounts(sub); reqs != 0 || holds != 0 {
				t.Fatalf("a closed subject acquired a hold: requests=%d hold accounts=%d", reqs, holds)
			}
		})
	}
}
