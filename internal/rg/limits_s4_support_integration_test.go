//go:build integration

// ADR 0115 S4-min test support: the S1 scratch world (runtime role everywhere) extended with enforced
// floors, the activation switch, the real player limit service and EvaluateAction under the session
// shape of the money paths (a tenant-scoped, non-player session).
package rg

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/ledger"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
)

// s4Fix creates a fresh isolated jurisdiction with enforced floors for deposit, loss and wager (USD),
// with `over[kind]` applied on top. It does NOT activate enforcement.
func (w *s1World) s4Fix(t *testing.T, over map[string]rgfix.Spec) s1Fix {
	t.Helper()
	f := w.newFix(t)
	for _, kind := range []string{"deposit", "loss", "wager"} {
		w.floor(t, rgfix.EnforcedFloor(f.jur, kind, "USD", over[kind]))
	}
	return f
}

// activate switches enforcement on for tenant A (brand nil = tenant-wide).
func (w *s1World) activate(t *testing.T, f s1Fix, brand *uuid.UUID) {
	t.Helper()
	wantOK(t, "activate enforcement", do(w.rt, staffSess(f.tenantA, f.staffA), func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := NewLimits(LimitsConfig{}).ActivateEnforcement(ctx, tx, ActivateEnforcementInput{
			TenantID: f.tenantA, StaffID: f.staffA, BrandID: brand, ReasonCode: "s4_test", AttestationRef: rgfix.TestAttestation})
		return err
	}))
}

func (w *s1World) staffFor(f s1Fix, tenant uuid.UUID) uuid.UUID {
	if tenant == f.tenantB {
		return f.staffB
	}
	return f.staffA
}

// eval runs EvaluateAction in the session shape of the money paths (tenant scope, no player).
func (w *s1World) eval(f s1Fix, tenant uuid.UUID, p ActionParams) (Decision, error) {
	var d Decision
	err := do(w.rt, staffSess(tenant, w.staffFor(f, tenant)), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		d, e = EvaluateAction(ctx, tx, p)
		return e
	})
	return d, err
}

func (w *s1World) mustEval(t *testing.T, f s1Fix, tenant uuid.UUID, p ActionParams) Decision {
	t.Helper()
	d, err := w.eval(f, tenant, p)
	if err != nil {
		t.Fatalf("EvaluateAction %s %d: %v", p.Action, p.Amount, err)
	}
	return d
}

func (f s1Fix) action(acct uuid.UUID, a Action, amount int64) ActionParams {
	brand := f.brandA1
	if acct == f.acctA2 {
		brand = f.brandA2
	}
	return ActionParams{TenantID: f.tenantA, BrandID: brand, PlayerAccountID: acct, Action: a, AssetCode: "USD", Amount: amount}
}

func wantDenied(t *testing.T, what string, d Decision, code string) {
	t.Helper()
	if d.Allowed || d.Code != code {
		t.Fatalf("%s: want denied %q, got allowed=%v code=%q msg=%q", what, code, d.Allowed, d.Code, d.Message)
	}
}

func wantAllowed(t *testing.T, what string, d Decision) {
	t.Helper()
	if !d.Allowed {
		t.Fatalf("%s: want allowed, got denied %q (%s) figures=%v", what, d.Code, d.Message, d.Figures)
	}
}

// setLimit records a player's own limit through the real S2 service (tenant_person or account scope
// follows the composed configuration).
func (w *s1World) setLimit(t *testing.T, tenant, acct uuid.UUID, kind ControlKind, period PeriodKind, value *string) ChangeResult {
	t.Helper()
	var res ChangeResult
	wantOK(t, "set limit", do(w.rt, playerSess(tenant, acct), func(ctx context.Context, tx pgx.Tx) error {
		var e error
		res, e = NewLimits(LimitsConfig{}).SetLimit(ctx, tx, SetLimitInput{TenantID: tenant, PlayerAccountID: acct,
			Limit: LimitInput{Kind: kind, AssetCode: strp("USD"), Period: &period, Value: value, IdempotencyKey: uuid.NewString()}})
		return e
	}))
	return res
}

// stake seeds a casino stake of amt (USD) for acct at `at` (zero = now).
func (w *s1World) stake(t *testing.T, tenant, acct uuid.UUID, amt string, at time.Time) uuid.UUID {
	t.Helper()
	return w.s3Post(t, tenant, s3Tx{typ: "casino_bet", at: at, asset: "USD", legs: []s3Leg{cashDr(acct, amt), houseCr(amt)}})
}

func (w *s1World) stakeWithWin(t *testing.T, tenant, acct uuid.UUID, stake, win string) {
	t.Helper()
	corr := uuid.New()
	w.s3Post(t, tenant, s3Tx{typ: "casino_bet", corr: corr, asset: "USD", legs: []s3Leg{cashDr(acct, stake), houseCr(stake)}})
	w.s3Post(t, tenant, s3Tx{typ: "casino_win", corr: corr, asset: "USD", legs: []s3Leg{houseDr(win), cashCr(acct, win)}})
}

// wallet makes sure acct has a USD wallet and returns its id (owner insert).
func (w *s1World) wallet(t *testing.T, tenant, acct uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	wantOK(t, "wallet", w.owner.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		id, e = s3Wallet(ctx, tx, tenant, acct, "USD")
		return e
	}))
	return id
}

// postStake posts a REAL ledger casino_bet inside tx (runtime role), the way postBet does after its RG check.
func postStake(ctx context.Context, tx pgx.Tx, tenant, wallet uuid.UUID, amount int64) error {
	accts, err := ledger.GetOrCreateAccounts(ctx, tx, tenant,
		ledger.AccountSpec{WalletID: &wallet, AccountType: ledger.AccountPlayerCash, AssetCode: "USD"},
		ledger.AccountSpec{AccountType: ledger.AccountHouseGaming, AssetCode: "USD"})
	if err != nil {
		return err
	}
	_, err = ledger.Post(ctx, tx, ledger.TransactionInput{
		TenantID: tenant, TransactionType: ledger.TxCasinoBet, IdempotencyKey: "s4-" + uuid.NewString(), CorrelationID: uuid.New(),
		Entries: []ledger.EntryInput{
			{LedgerAccountID: accts[0], Direction: ledger.Debit, Amount: amount},
			{LedgerAccountID: accts[1], Direction: ledger.Credit, Amount: amount},
		}})
	return err
}

// parallel runs fn(i) for i in [0,n) concurrently and returns the number of nil results.
func parallel(n int, fn func(i int) error) (ok int, errs []error) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			err := fn(i)
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				ok++
			} else {
				errs = append(errs, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	return ok, errs
}
