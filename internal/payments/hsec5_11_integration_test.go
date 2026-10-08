//go:build integration

// H-SEC-5 / H-SEC-11 (owner/security rulings, PAY-H-FOLLOWUPS-1 (4)/(14)):
// HTTP deposit initiation and HTTP withdrawal/payout initiation fail closed
// unless the tenant AND the player's brand are 'active', decided INSIDE the
// transaction that creates the row. These tests exercise the domain entry
// points the HTTP handlers call (InitiateDepositAttempt, RequestWithdrawal,
// ClaimForDispatch) on the runtime role; the handler status mapping is pinned
// in internal/httpserver.
package payments

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

type hsecCase struct {
	name   string
	tenant string // status to set on the tenant ("" = leave active)
	brand  string // status to set on the brand ("" = leave active)
}

var hsecRefusals = []hsecCase{
	{"tenant_suspended", "suspended", ""},
	{"tenant_closed", "closed", ""},
	{"brand_suspended", "", "suspended"},
	{"brand_closed", "", "closed"},
	{"both_suspended", "suspended", "suspended"},
}

func setBrandStatus(t *testing.T, pool *db.Pool, tenantID, brandID uuid.UUID, status string) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE brands SET status = $2 WHERE id = $1`, brandID, status)
		if err == nil && tag.RowsAffected() != 1 {
			t.Fatalf("set brand status: %d rows", tag.RowsAffected())
		}
		return err
	}); err != nil {
		t.Fatalf("set brand status %s: %v", status, err)
	}
}

func applyHsecCase(t *testing.T, pool *db.Pool, f orchFixture, c hsecCase) {
	t.Helper()
	if c.tenant != "" {
		setTenantStatus(t, pool, f.tenantID, c.tenant)
	}
	if c.brand != "" {
		setBrandStatus(t, pool, f.tenantID, f.brandID, c.brand)
	}
}

func reactivate(t *testing.T, pool *db.Pool, f orchFixture) {
	t.Helper()
	setTenantStatus(t, pool, f.tenantID, "active")
	setBrandStatus(t, pool, f.tenantID, f.brandID, "active")
}

type hsecCounts struct{ intents, attempts, requests, ledgerTx, auditDepositRequested int }

func countsFor(t *testing.T, pool *db.Pool, tenantID uuid.UUID) hsecCounts {
	t.Helper()
	var c hsecCounts
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		for _, q := range []struct {
			sql string
			dst *int
		}{
			{`SELECT count(*) FROM deposit_intents`, &c.intents},
			{`SELECT count(*) FROM payment_attempts`, &c.attempts},
			{`SELECT count(*) FROM withdrawal_requests`, &c.requests},
			{`SELECT count(*) FROM ledger_transactions`, &c.ledgerTx},
			{`SELECT count(*) FROM audit_log WHERE action = 'deposit.requested'`, &c.auditDepositRequested},
		} {
			if err := tx.QueryRow(ctx, q.sql).Scan(q.dst); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("counts: %v", err)
	}
	return c
}

func initDeposit(orch *Orchestrator, pool *db.Pool, f orchFixture, key string) (InitiateDepositAttemptResult, error) {
	return orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: key,
	})
}

func requestWithdrawalTx(pool *db.Pool, f payoutFixture, key string, amount int64) (withdrawal.WithdrawalRequest, error) {
	var wr withdrawal.WithdrawalRequest
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID,
			WalletID: f.walletID, AssetCode: "EUR", Amount: amount, IdempotencyKey: key,
		})
		return err
	})
	return wr, err
}

func depositWorld(t *testing.T, id string) (*db.Pool, *loopProvider, *Orchestrator, orchFixture) {
	t.Helper()
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider(id)
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, spy, 100)
	return pool, spy, spy.orchestrator(), f
}

// ---- deposits (H-SEC-5) -------------------------------------------------

func TestHSEC5_Deposit_ActiveTenantActiveBrand_Succeeds(t *testing.T) {
	pool, spy, orch, f := depositWorld(t, "mock-psp-h5-ctl")
	res, err := initDeposit(orch, pool, f, "h5-ctl")
	if err != nil || !res.AttemptCreated {
		t.Fatalf("control must succeed: created=%v err=%v", res.AttemptCreated, err)
	}
	if d, _, _ := spy.counts(); d != 1 {
		t.Fatalf("control: want 1 Deposit call, got %d", d)
	}
	if c := countsFor(t, pool, f.tenantID); c.intents != 1 || c.attempts != 1 {
		t.Fatalf("control rows: %+v", c)
	}
}

func TestHSEC5_Deposit_NonActiveTenantOrBrand_RefusedNothingCreated_RetryAfterReactivation(t *testing.T) {
	for _, c := range hsecRefusals {
		t.Run(c.name, func(t *testing.T) {
			pool, spy, orch, f := depositWorld(t, "mock-psp-h5-"+strings.ReplaceAll(c.name, "_", "-"))
			before := countsFor(t, pool, f.tenantID)
			applyHsecCase(t, pool, f, c)

			res, err := initDeposit(orch, pool, f, "h5-key-"+c.name)
			if !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("want ErrNotActiveForPaymentInitiation, got %v", err)
			}
			if res.AttemptCreated {
				t.Fatal("no attempt may be created")
			}
			if d, _, _ := spy.counts(); d != 0 {
				t.Fatalf("no provider call allowed, got %d", d)
			}
			if after := countsFor(t, pool, f.tenantID); after != before {
				t.Fatalf("no intent/attempt/ledger/audit row may be created: before=%+v after=%+v", before, after)
			}

			// Idempotency: the refusal wrote nothing, so the SAME key is a normal
			// first request after reactivation.
			reactivate(t, pool, f)
			res, err = initDeposit(orch, pool, f, "h5-key-"+c.name)
			if err != nil || !res.AttemptCreated {
				t.Fatalf("retry after reactivation must succeed: created=%v err=%v", res.AttemptCreated, err)
			}
			if d, _, _ := spy.counts(); d != 1 {
				t.Fatalf("retry: want 1 Deposit call, got %d", d)
			}
			if after := countsFor(t, pool, f.tenantID); after.intents != 1 || after.attempts != 1 {
				t.Fatalf("retry rows: %+v", after)
			}
		})
	}
}

func TestHSEC5_Deposit_OtherTenantUnaffected(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-h5-other")
	orch := spy.orchestrator()
	fBad, fOK := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	registerCapability(t, pool, fBad, spy, 100)
	registerCapability(t, pool, fOK, spy, 100)
	setTenantStatus(t, pool, fBad.tenantID, "suspended")
	if _, err := initDeposit(orch, pool, fBad, "k"); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("suspended tenant must be refused: %v", err)
	}
	if res, err := initDeposit(orch, pool, fOK, "k"); err != nil || !res.AttemptCreated {
		t.Fatalf("other tenant must be unaffected: %v", err)
	}
	// A brand suspended in tenant A does not affect tenant B's brand either.
	setBrandStatus(t, pool, fOK.tenantID, fOK.brandID, "suspended")
	if _, err := initDeposit(orch, pool, fOK, "k2"); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("suspended brand must be refused: %v", err)
	}
	if res, err := initDeposit(orch, pool, fBad, "k3"); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) || res.AttemptCreated {
		t.Fatalf("still suspended: %v", err)
	}
}

// A brand that belongs to ANOTHER tenant (or does not exist) is not "this
// player's active brand": refused, never read through the other tenant's row.
func TestHSEC5_Deposit_BrandOfOtherTenantOrMissing_Refused(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-h5-xbrand")
	orch := spy.orchestrator()
	f, other := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	registerCapability(t, pool, f, spy, 100)
	for name, brand := range map[string]uuid.UUID{"other_tenants_brand": other.brandID, "missing_brand": uuid.New()} {
		t.Run(name, func(t *testing.T) {
			g := f
			g.brandID = brand
			before := countsFor(t, pool, f.tenantID)
			_, err := initDeposit(orch, pool, g, "xb-"+name)
			if !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("want the gate's refusal (not a downstream FK error), got %v", err)
			}
			if d, _, _ := spy.counts(); d != 0 {
				t.Fatalf("no provider call, got %d", d)
			}
			if after := countsFor(t, pool, f.tenantID); after != before {
				t.Fatalf("rows created: %+v -> %+v", before, after)
			}
		})
	}
}

// The status change commits WHILE the initiation is waiting at the gate: the
// shared advisory lock (tenant) / FOR SHARE row lock (brand) make the
// initiation wait for the changing transaction and then read the NEW status. A
// plain unlocked read would see 'active' and create the intent.
func TestHSEC5_Deposit_StatusChangeCommitsWhileInitiating_Refused(t *testing.T) {
	for _, kind := range []string{"tenant", "brand"} {
		t.Run(kind, func(t *testing.T) {
			pool, spy, orch, f := depositWorld(t, "mock-psp-h5-race-"+kind)
			release := holdStatusChange(t, pool, f, kind)
			done := make(chan error, 1)
			go func() { _, err := initDeposit(orch, pool, f, "race-"+kind); done <- err }()
			select {
			case err := <-done:
				t.Fatalf("initiation must wait for the in-flight status change, finished with %v", err)
			case <-time.After(400 * time.Millisecond):
			}
			release()
			if err := <-done; !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("want refusal after the status change commits, got %v", err)
			}
			if d, _, _ := spy.counts(); d != 0 {
				t.Fatalf("no provider call, got %d", d)
			}
			if c := countsFor(t, pool, f.tenantID); c.intents != 0 || c.attempts != 0 {
				t.Fatalf("rows created: %+v", c)
			}
		})
	}
}

// holdStatusChange starts a transaction that sets tenant|brand status to
// 'suspended' and keeps it open until the returned release func commits it.
func holdStatusChange(t *testing.T, pool *db.Pool, f orchFixture, kind string) (release func()) {
	t.Helper()
	ready, commit, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	run := func(ctx context.Context, tx pgx.Tx) error {
		var err error
		if kind == "tenant" {
			_, err = tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, f.tenantID)
		} else {
			_, err = tx.Exec(ctx, `UPDATE brands SET status = 'suspended' WHERE id = $1`, f.brandID)
		}
		if err != nil {
			return err
		}
		close(ready)
		<-commit
		return nil
	}
	go func() {
		if kind == "tenant" {
			finished <- pool.WithPlatformAdmin(context.Background(), uuid.New(), run)
		} else {
			finished <- pool.WithTenant(context.Background(), f.tenantID, run)
		}
	}()
	select {
	case <-ready:
	case err := <-finished:
		t.Fatalf("status-change tx failed before ready: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("status-change tx never became ready")
	}
	var once sync.Once
	rel := func() {
		once.Do(func() {
			close(commit)
			if err := <-finished; err != nil {
				t.Errorf("status-change tx: %v", err)
			}
		})
	}
	t.Cleanup(rel)
	return func() { rel() }
}

// HTTP-driven cascade child T2 claim: the same gate applies (player-request
// driver, sweeperDriven=false); a refused child is deferred, never dispatched.
func TestHSEC5_Deposit_HTTPCascadeChildT2Claim_RefusedWhenNotActive(t *testing.T) {
	for _, c := range append([]hsecCase{{"active_control", "", ""}}, hsecRefusals[:4]...) {
		t.Run(c.name, func(t *testing.T) {
			pool, spy, orch, f := depositWorld(t, "mock-psp-h5-casc-"+strings.ReplaceAll(c.name, "_", "-"))
			intentID := insertRawDepositIntent(t, pool, f, "pending")
			attID := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
			applyHsecCase(t, pool, f, c)
			var intent DepositIntent
			var att PaymentAttempt
			if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				var err error
				if intent, err = GetDepositIntentByID(ctx, tx, intentID); err != nil {
					return err
				}
				att, err = GetAttemptByID(ctx, tx, attID)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, _, _, _, _, err := orch.driveCreatedAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, intent, att, false); err != nil {
				t.Fatalf("driveCreatedAttempt: %v", err)
			}
			d, _, _ := spy.counts()
			got := mustGetAttempt(t, pool, f.tenantID, attID)
			if c.tenant == "" && c.brand == "" {
				if d != 1 || got.State == AttemptCreated {
					t.Fatalf("control must dispatch: deposits=%d state=%s", d, got.State)
				}
				return
			}
			if d != 0 || got.State != AttemptCreated || got.ClaimToken != nil || got.SubmitCount != 0 {
				t.Fatalf("refused child must stay unclaimed: deposits=%d state=%s", d, got.State)
			}
			if got.PollCount != 1 {
				t.Fatalf("refused child must be rescheduled exactly once (poll_count 0 -> 1), got %d", got.PollCount)
			}
		})
	}
}

// ---- withdrawal requests (H-SEC-11) -------------------------------------

func TestHSEC11_RequestWithdrawal_Control_ActiveSucceeds(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	wr, err := requestWithdrawalTx(pool, f, "h11-ctl", 500)
	if err != nil || wr.ID == uuid.Nil || wr.HoldLedgerTransactionID == nil {
		t.Fatalf("control must succeed with a hold: %+v err=%v", wr, err)
	}
}

func TestHSEC11_RequestWithdrawal_NonActive_RefusedNoRowNoHold_RetryAfterReactivation(t *testing.T) {
	for _, c := range hsecRefusals {
		t.Run(c.name, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedPayoutFixture(t, pool, 10_000, true)
			before := countsFor(t, pool, f.tenantID)
			cashBefore := cashBalance(t, pool, f.orchFixture)
			applyHsecCase(t, pool, f.orchFixture, c)

			if _, err := requestWithdrawalTx(pool, f, "h11-"+c.name, 500); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("want ErrNotActiveForPaymentInitiation, got %v", err)
			}
			if after := countsFor(t, pool, f.tenantID); after != before {
				t.Fatalf("no request/hold/ledger/audit row may be created: %+v -> %+v", before, after)
			}
			if cb := cashBalance(t, pool, f.orchFixture); cb != cashBefore {
				t.Fatalf("no hold: cash %d -> %d", cashBefore, cb)
			}

			reactivate(t, pool, f.orchFixture)
			wr, err := requestWithdrawalTx(pool, f, "h11-"+c.name, 500)
			if err != nil || wr.HoldLedgerTransactionID == nil {
				t.Fatalf("retry after reactivation must succeed: %v", err)
			}
			if after := countsFor(t, pool, f.tenantID); after.requests != 1 {
				t.Fatalf("retry rows: %+v", after)
			}
			loAssertBalanced(t, pool, f.tenantID)
		})
	}
}

// A status flip landing before the request transaction's gate (the request was
// already being handled) is read by the in-tx gate.
func TestHSEC11_RequestWithdrawal_FlipInsideOpenTxBeforeGate_Refused(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		setTenantStatus(t, pool, f.tenantID, "suspended") // commits on another connection mid-request
		_, err := withdrawal.RequestWithdrawal(ctx, tx, withdrawal.RequestParams{
			TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, PersonID: f.personID,
			WalletID: f.walletID, AssetCode: "EUR", Amount: 500, IdempotencyKey: "h11-flip",
		})
		return err
	})
	if !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("want refusal, got %v", err)
	}
	if c := countsFor(t, pool, f.tenantID); c.requests != 0 {
		t.Fatalf("request created: %+v", c)
	}
}

func TestHSEC11_RequestWithdrawal_StatusChangeCommitsWhileRequesting_Refused(t *testing.T) {
	for _, kind := range []string{"tenant", "brand"} {
		t.Run(kind, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedPayoutFixture(t, pool, 10_000, true)
			release := holdStatusChange(t, pool, f.orchFixture, kind)
			done := make(chan error, 1)
			go func() { _, err := requestWithdrawalTx(pool, f, "h11-race-"+kind, 500); done <- err }()
			select {
			case err := <-done:
				t.Fatalf("request must wait for the in-flight status change, finished with %v", err)
			case <-time.After(400 * time.Millisecond):
			}
			release()
			if err := <-done; !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("want refusal, got %v", err)
			}
			if c := countsFor(t, pool, f.tenantID); c.requests != 0 {
				t.Fatalf("request created: %+v", c)
			}
		})
	}
}

func TestHSEC11_RequestWithdrawal_OtherTenantUnaffected(t *testing.T) {
	pool := depositV2ScratchPool(t)
	bad, ok := seedPayoutFixture(t, pool, 10_000, true), seedPayoutFixture(t, pool, 10_000, true)
	setTenantStatus(t, pool, bad.tenantID, "suspended")
	if _, err := requestWithdrawalTx(pool, bad, "k", 500); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("suspended tenant must be refused: %v", err)
	}
	if _, err := requestWithdrawalTx(pool, ok, "k", 500); err != nil {
		t.Fatalf("other tenant must be unaffected: %v", err)
	}
}

// ---- payout claim (H-SEC-11, staff submit -> ClaimForDispatch) ------------

func payoutWorld(t *testing.T, id string) (*db.Pool, *withdrawCountingProvider, *Orchestrator, payoutFixture, withdrawal.WithdrawalRequest) {
	t.Helper()
	pool := depositV2ScratchPool(t)
	f := seedPayoutFixture(t, pool, 10_000, true)
	p := &withdrawCountingProvider{MockProvider: NewMockProvider(id, "EUR")}
	registerCapability(t, pool, f.orchFixture, p, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{id: p}, MultiWebhookCredentialResolver{id: NewMockWebhookCredentials(p.MockProvider)})
	return pool, p, orch, f, approvedWithdrawal(t, pool, f, 500, "h11-claim")
}

func requestState(t *testing.T, pool *db.Pool, f payoutFixture, id uuid.UUID) withdrawal.State {
	t.Helper()
	var wr withdrawal.WithdrawalRequest
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		wr, err = withdrawal.GetByID(ctx, tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return wr.State
}

func TestHSEC11_ClaimForDispatch_ActiveControl_Succeeds(t *testing.T) {
	pool, _, orch, f, wr := payoutWorld(t, "mock-payout-h11-ctl")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil || claim.Denied || claim.Attempt.State != AttemptSubmitting {
		t.Fatalf("control must claim: %+v err=%v", claim, err)
	}
}

func TestHSEC11_ClaimForDispatch_NonActive_RefusedStaysApproved_RetryAfterReactivation(t *testing.T) {
	for _, c := range hsecRefusals {
		t.Run(c.name, func(t *testing.T) {
			pool, p, orch, f, wr := payoutWorld(t, "mock-payout-h11-"+strings.ReplaceAll(c.name, "_", "-"))
			// A revoked KYC would make the gate DENY and RELEASE THE HOLD if it ran
			// first; the H-SEC-11 gate must come before that state change.
			revokeVerification(t, pool, f)
			before := countsFor(t, pool, f.tenantID)
			applyHsecCase(t, pool, f.orchFixture, c)

			_, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
			if !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("want ErrNotActiveForPaymentInitiation, got %v", err)
			}
			if s := requestState(t, pool, f, wr.ID); s != withdrawal.StateApproved {
				t.Fatalf("request must stay approved (no deny/hold release, no submit), got %s", s)
			}
			if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 0 {
				t.Fatalf("no attempt row, got %d", n)
			}
			if p.count() != 0 {
				t.Fatalf("no provider call, got %d", p.count())
			}
			if after := countsFor(t, pool, f.tenantID); after != before {
				t.Fatalf("no ledger/audit/attempt change: %+v -> %+v", before, after)
			}

			// Reactivation: the same request is claimable again (here: the KYC gate now
			// denies it for the revoked verification, which proves the claim path ran).
			reactivate(t, pool, f.orchFixture)
			claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
			if err != nil || !claim.Denied {
				t.Fatalf("after reactivation the claim must proceed normally (KYC deny expected): %+v err=%v", claim, err)
			}
		})
	}
}

func TestHSEC11_ClaimForDispatch_FlipAfterApprovalBeforeClaim_RefusedThenSucceeds(t *testing.T) {
	pool, p, orch, f, wr := payoutWorld(t, "mock-payout-h11-flip")
	setTenantStatus(t, pool, f.tenantID, "suspended") // after approval, before the claim tx
	if _, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor()); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("want refusal, got %v", err)
	}
	setTenantStatus(t, pool, f.tenantID, "active")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil || claim.Attempt.State != AttemptSubmitting {
		t.Fatalf("retry after reactivation must claim: %v", err)
	}
	if p.count() != 0 {
		t.Fatalf("ClaimForDispatch never calls the provider, got %d", p.count())
	}
}

func TestHSEC11_ClaimForDispatch_StatusChangeCommitsWhileClaiming_Refused(t *testing.T) {
	for _, kind := range []string{"tenant", "brand"} {
		t.Run(kind, func(t *testing.T) {
			pool, p, orch, f, wr := payoutWorld(t, "mock-payout-h11-race-"+kind)
			release := holdStatusChange(t, pool, f.orchFixture, kind)
			done := make(chan error, 1)
			go func() {
				_, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
				done <- err
			}()
			select {
			case err := <-done:
				t.Fatalf("claim must wait for the in-flight status change, finished with %v", err)
			case <-time.After(400 * time.Millisecond):
			}
			release()
			if err := <-done; !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("want refusal, got %v", err)
			}
			if n := countAttempts(t, pool, f.tenantID, wr.ID); n != 0 || p.count() != 0 {
				t.Fatalf("attempts=%d withdraws=%d", n, p.count())
			}
		})
	}
}

func TestHSEC11_ClaimForDispatch_OtherTenantUnaffected(t *testing.T) {
	pool, _, orch, bad, badWr := payoutWorld(t, "mock-payout-h11-oth")
	// A second tenant on the same orchestrator/provider.
	ok := seedPayoutFixture(t, pool, 10_000, true)
	p, _ := orch.Provider("mock-payout-h11-oth")
	registerCapability(t, pool, ok.orchFixture, p, 100)
	okWr := approvedWithdrawal(t, pool, ok, 500, "h11-other")
	setTenantStatus(t, pool, bad.tenantID, "suspended")
	if _, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, bad.tenantID, badWr.ID, "bank_transfer", testSubmitActor()); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("suspended tenant must be refused: %v", err)
	}
	if claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, ok.tenantID, okWr.ID, "bank_transfer", testSubmitActor()); err != nil || claim.Attempt.State != AttemptSubmitting {
		t.Fatalf("other tenant must be unaffected: %v", err)
	}
}

// ---- the shared helper's fail-closed edges ------------------------------

func TestHSEC5_11_Helper_FailsClosed_MissingUnreadable(t *testing.T) {
	pool := depositV2ScratchPool(t)
	f := seedOrchFixture(t, pool)
	check := func(tenantID, brandID uuid.UUID) error {
		scope := tenantID
		if scope == uuid.Nil {
			scope = f.tenantID // WithTenant refuses a nil scope; the gate argument stays nil
		}
		return pool.WithTenant(context.Background(), scope, func(ctx context.Context, tx pgx.Tx) error {
			return tenant.RequireActiveForPaymentInitiation(ctx, tx, tenantID, brandID)
		})
	}
	if err := check(f.tenantID, f.brandID); err != nil {
		t.Fatalf("active/active must pass: %v", err)
	}
	if err := check(uuid.New(), uuid.New()); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("missing tenant row must refuse: %v", err)
	}
	if err := check(f.tenantID, uuid.Nil); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("nil brand must refuse: %v", err)
	}
	if err := check(uuid.Nil, f.brandID); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
		t.Fatalf("nil tenant must refuse: %v", err)
	}
	// Unreadable: the transaction is already aborted when the gate runs; the read
	// errors and the gate must return that error (never nil = never "active").
	var gateErr error
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _ = tx.Exec(ctx, `SELECT 1/0`)
		gateErr = tenant.RequireActiveForPaymentInitiation(ctx, tx, f.tenantID, f.brandID)
		return gateErr
	})
	if gateErr == nil {
		t.Fatal("an unreadable status must refuse (non-nil), got nil")
	}
	// A vocabulary value other than 'active' can only be a closed enum member
	// (CHECK active|suspended|closed); each non-active one was covered above.
}

// ---- replay of an already-created row while the tenant/brand is non-active ----
//
// The gate runs before the idempotent-replay lookup (fail closed, documented in
// ADR 0095 section 43.1): a retry of an ALREADY-created intent/request is
// refused too, and returns the original after reactivation.

func TestHSEC5_Deposit_ReplayOfExistingIntentWhileNonActive_Refused(t *testing.T) {
	for _, c := range hsecRefusals[:4] {
		t.Run(c.name, func(t *testing.T) {
			pool, spy, orch, f := depositWorld(t, "mock-psp-h5-rp-"+strings.ReplaceAll(c.name, "_", "-"))
			first, err := initDeposit(orch, pool, f, "replay-key")
			if err != nil || !first.AttemptCreated {
				t.Fatalf("setup: %v", err)
			}
			applyHsecCase(t, pool, f, c)
			if _, err := initDeposit(orch, pool, f, "replay-key"); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("replay while non-active must be refused, got %v", err)
			}
			reactivate(t, pool, f)
			again, err := initDeposit(orch, pool, f, "replay-key")
			if err != nil || again.Intent.ID != first.Intent.ID {
				t.Fatalf("replay after reactivation must return the original intent: %v", err)
			}
			if d, _, _ := spy.counts(); d != 1 {
				t.Fatalf("exactly one Deposit call overall, got %d", d)
			}
		})
	}
}

func TestHSEC11_RequestWithdrawal_ReplayOfExistingRequestWhileNonActive_Refused(t *testing.T) {
	for _, c := range hsecRefusals[:4] {
		t.Run(c.name, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedPayoutFixture(t, pool, 10_000, true)
			first, err := requestWithdrawalTx(pool, f, "replay-key", 500)
			if err != nil {
				t.Fatalf("setup: %v", err)
			}
			applyHsecCase(t, pool, f.orchFixture, c)
			if _, err := requestWithdrawalTx(pool, f, "replay-key", 500); !errors.Is(err, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("replay while non-active must be refused, got %v", err)
			}
			reactivate(t, pool, f.orchFixture)
			again, err := requestWithdrawalTx(pool, f, "replay-key", 500)
			if err != nil || again.ID != first.ID {
				t.Fatalf("replay after reactivation must return the original request: %v", err)
			}
			if c := countsFor(t, pool, f.tenantID); c.requests != 1 {
				t.Fatalf("requests=%d", c.requests)
			}
		})
	}
}

// A read that FAILS (not "no row"): the status change in flight holds the lock the gate needs and the
// session's lock_timeout expires. Tenant half = the advisory-lock wait, brand half = the FOR SHARE wait. The
// gate must return a non-nil error (never "active"), and nothing is created.
func TestHSEC5_11_Helper_ReadErrorUnderLockTimeout_FailsClosed(t *testing.T) {
	for _, kind := range []string{"tenant", "brand"} {
		t.Run(kind, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			f := seedOrchFixture(t, pool)
			release := holdStatusChange(t, pool, f, kind)
			var gateErr error
			_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '300ms'`); err != nil {
					t.Fatalf("set lock_timeout: %v", err)
				}
				gateErr = tenant.RequireActiveForPaymentInitiation(ctx, tx, f.tenantID, f.brandID)
				return gateErr
			})
			release()
			if gateErr == nil {
				t.Fatal("a status read that errors must fail closed (non-nil error), got nil")
			}
			if errors.Is(gateErr, tenant.ErrNotActiveForPaymentInitiation) {
				t.Fatalf("test premise: this must be a READ error, not a status refusal: %v", gateErr)
			}
		})
	}
}
