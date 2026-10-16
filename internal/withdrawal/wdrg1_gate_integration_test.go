//go:build integration

// WD-RG-1 (owner decision 2026-10-15; completion audit A6): the RG + RISK gate on a NEW withdrawal request.
// Fail-closed, audited, tenant-scoped; allowed during a self-exclusion or a time-out (ADR 0115 keeps a
// player's own funds reachable), refused when the account or wallet is not active or when the risk engine
// says DENY/REVIEW; a missing or failing gate is a refusal that writes nothing and does not consume the
// idempotency key; a replay is never re-evaluated.
package withdrawal

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/rgfix"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate/standard"
)

// reqCommitDenial mirrors the HTTP handler: a *GateDeniedError / *KYCDeniedError is captured inside the closure
// (returning nil, so the denial audit COMMITS) and returned to the caller.
func reqCommitDenial(t *testing.T, pool *db.Pool, f fixture, p RequestParams) (WithdrawalRequest, error) {
	t.Helper()
	var wr WithdrawalRequest
	var denied *GateDeniedError
	err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var e error
		wr, e = RequestWithdrawal(ctx, tx, p)
		if errors.As(e, &denied) {
			return nil
		}
		return e
	})
	if denied != nil {
		return wr, denied
	}
	return wr, err
}

func auditRows(t *testing.T, pool *db.Pool, f fixture, action string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT metadata FROM audit_log WHERE tenant_id = $1 AND action = $2 ORDER BY created_at`, f.tenantID, action)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var raw []byte
			if err := rows.Scan(&raw); err != nil {
				return err
			}
			m := map[string]any{}
			if err := json.Unmarshal(raw, &m); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func setAccountStatus(t *testing.T, pool *db.Pool, f fixture, s identity.PlayerAccountStatus) {
	t.Helper()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, f.playerAccountID, s)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestWDRG1_RG_RefusesAnInactiveAccount_WritesOnlyTheDenialAudit_AndDoesNotConsumeTheKey(t *testing.T) {
	pool := testPool(t)
	for _, st := range []identity.PlayerAccountStatus{identity.PlayerStatusSuspended, identity.PlayerStatusClosed,
		identity.PlayerStatusPendingVerification, identity.PlayerStatusIdentityReviewRequired} {
		f := seedFixture(t, pool, 10_000) // a fresh tenant per status
		setAccountStatus(t, pool, f, st)
		key := "wdrg1-" + string(st) + "-" + uuid.NewString()
		_, err := reqCommitDenial(t, pool, f, baseParams(f, key, 400))
		var denied *GateDeniedError
		if !errors.As(err, &denied) || denied.GateCode != GateCodeRG || denied.Reason != rg.CodePlayerAccountNotActive {
			t.Fatalf("%s: want a WD050 gate denial player_account_not_active, got %v", st, err)
		}
		nothingWritten(t, pool, f, 10_000, string(st))
		rows := auditRows(t, pool, f, "withdrawal.denied_by_rg")
		if len(rows) != 1 || rows[0]["gate_code"] != GateCodeRG || rows[0]["reason_code"] != rg.CodePlayerAccountNotActive || rows[0]["asset_code"] != "EUR" {
			t.Fatalf("%s: the denial audit must be committed with the closed gate vocabulary: %v", st, rows)
		}
		setAccountStatus(t, pool, f, identity.PlayerStatusActive)
		// The key was not consumed: the same key now succeeds.
		if wr, err := req(t, pool, f, baseParams(f, key, 400)); err != nil || wr.State != StateRequested {
			t.Fatalf("%s: after reactivation the same key must succeed, got %v %+v", st, err, wr)
		}
	}
}

func TestWDRG1_RG_RefusesAFrozenWallet(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `UPDATE wallets SET status = 'frozen' WHERE id = $1`, f.walletID)
		return e
	}); err != nil {
		t.Skipf("the wallet status cannot be changed in this fixture: %v", err)
	}
	_, err := reqCommitDenial(t, pool, f, baseParams(f, "wdrg1-frozen-"+uuid.NewString(), 400))
	var denied *GateDeniedError
	if !errors.As(err, &denied) || denied.GateCode != GateCodeRG || denied.Reason != rg.CodeWalletNotActive {
		t.Fatalf("want a WD050 denial wallet_not_active, got %v", err)
	}
	nothingWritten(t, pool, f, 10_000, "frozen wallet")
}

// The decision recorded for the owner/legal question (ADR 0115 2.3, 3.1, 3.2, 6): a withdrawal is NOT blocked by a
// self-exclusion or a time-out, by an exhausted limit, or by rg_config_missing - and the audit says which restriction
// was in force.
func TestWDRG1_Allowed_DuringSelfExclusionAndTimeOut_AndWithEnforcementOnAndNoFloor(t *testing.T) {
	pool := testPool(t)
	for _, c := range []struct{ name string }{{"self_exclusion"}, {"time_out"}} {
		f := seedFixture(t, pool, 10_000)
		if err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
			if c.name == "self_exclusion" {
				_, e := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
				return e
			}
			_, e := rg.NewLimits(rg.LimitsConfig{}).CreateTimeOut(ctx, tx, rg.TimeOutInput{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID,
				DurationHours: 24, IdempotencyKey: uuid.NewString()})
			return e
		}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		// enforcement ON with NO floor: gameplay would be rg_config_missing; a withdrawal must not be.
		staff := rgfix.Staff(t, launchfix.OwnerFor(t, pool), f.tenantID)
		rgfix.Activate(t, launchfix.OwnerFor(t, pool), f.tenantID, nil, staff)
		wr, err := req(t, pool, f, baseParams(f, "wdrg1-allowed-"+c.name, 400))
		if err != nil || wr.State != StateRequested {
			t.Fatalf("%s: a withdrawal must be allowed during the restriction, got %v %+v", c.name, err, wr)
		}
		rows := auditRows(t, pool, f, "withdrawal.requested")
		if len(rows) != 1 || rows[0]["rg_restriction_in_force"] != c.name {
			t.Fatalf("%s: the request audit must name the restriction in force: %v", c.name, rows)
		}
		if holdBalance(t, pool, f) != 400 {
			t.Fatalf("%s: the hold must be posted", c.name)
		}
	}
}

func TestWDRG1_Risk_DenyAndReviewRefuse_WithTheRiskGateCode(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &f.tenantID, Operation: risk.OperationWithdrawal, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction,
			Threshold: 500, AssetCode: "EUR", RuleKind: risk.RuleHardLimit, Action: risk.ActionDeny,
			CreatedByActorType: "staff", CreatedByActorID: uuid.New(), Description: "wdrg1 test",
		})
		return e
	}); err != nil {
		t.Fatalf("create the withdrawal risk rule: %v", err)
	}
	// within the rule: allowed
	if wr, err := req(t, pool, f, baseParams(f, "wdrg1-risk-ok-"+uuid.NewString(), 500)); err != nil || wr.State != StateRequested {
		t.Fatalf("500 is within the max_amount rule: %v %+v", err, wr)
	}
	key := "wdrg1-risk-deny-" + uuid.NewString()
	_, err := reqCommitDenial(t, pool, f, baseParams(f, key, 501))
	var denied *GateDeniedError
	if !errors.As(err, &denied) || denied.GateCode != GateCodeRisk || denied.Reason != risk.CodeHardLimitBreach {
		t.Fatalf("501 breaches the rule: want a WD051 gate denial %s, got %v", risk.CodeHardLimitBreach, err)
	}
	if n := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests`); n != 1 {
		t.Fatalf("only the allowed request exists, got %d rows", n)
	}
	if h := holdBalance(t, pool, f); h != 500 {
		t.Fatalf("only the allowed hold exists, got %d", h)
	}
	rows := auditRows(t, pool, f, "withdrawal.denied_by_risk")
	if len(rows) != 1 || rows[0]["gate_code"] != GateCodeRisk || rows[0]["risk_outcome"] != "deny" || rows[0]["amount"] != float64(501) {
		t.Fatalf("risk denial audit: %v", rows)
	}
	// The key was not consumed by the denial.
	if n := countRows(t, pool, f.tenantID, `SELECT count(*) FROM withdrawal_requests WHERE idempotency_key = $1`, key); n != 0 {
		t.Fatalf("a denied request must not consume its key, %d rows", n)
	}
}

// A risk REVIEW outcome refuses a withdrawal request exactly like a DENY (there is no review queue on the request path):
// a gate denial with the risk code, never a gate-unavailable error.
func TestWDRG1_Risk_ReviewOutcomeRefuses_WithTheRiskGateCode(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &f.tenantID, Operation: risk.OperationWithdrawal, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction,
			Threshold: 500, AssetCode: "EUR", RuleKind: risk.RuleHardLimit, Action: risk.ActionReview,
			CreatedByActorType: "staff", CreatedByActorID: uuid.New(), Description: "wdrg1 review test",
		})
		return e
	}); err != nil {
		t.Fatalf("create the review rule: %v", err)
	}
	_, err := reqCommitDenial(t, pool, f, baseParams(f, "wdrg1-risk-review-"+uuid.NewString(), 501))
	var denied *GateDeniedError
	if !errors.As(err, &denied) || denied.GateCode != GateCodeRisk {
		t.Fatalf("a review outcome must be a WD051 gate denial, got %v", err)
	}
	if errors.Is(err, ErrGateUnavailable) {
		t.Fatalf("a review outcome is a denial, not an unavailable gate: %v", err)
	}
	rows := auditRows(t, pool, f, "withdrawal.denied_by_risk")
	if len(rows) != 1 || rows[0]["risk_outcome"] != "review" {
		t.Fatalf("review denial audit: %v", rows)
	}
}

func TestWDRG1_NoGate_And_FailingGate_FailClosed_NothingWritten(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	p := baseParams(f, "wdrg1-nogate-"+uuid.NewString(), 400)
	p.Gate = nil
	if _, err := req(t, pool, f, p); !errors.Is(err, ErrGateUnavailable) {
		t.Fatalf("a missing gate must be ErrGateUnavailable, got %v", err)
	}
	nothingWritten(t, pool, f, 10_000, "nil gate")

	p.Gate = failingGate{}
	if _, err := req(t, pool, f, p); !errors.Is(err, ErrGateUnavailable) {
		t.Fatalf("a gate that cannot decide must be ErrGateUnavailable, got %v", err)
	}
	nothingWritten(t, pool, f, 10_000, "failing gate")

	p.Gate = unknownCodeGate{}
	if _, err := req(t, pool, f, p); !errors.Is(err, ErrGateUnavailable) {
		t.Fatalf("a denial with an unrecognised gate code is not a decision, got %v", err)
	}
	nothingWritten(t, pool, f, 10_000, "unknown gate code")
}

type failingGate struct{}

func (failingGate) Evaluate(context.Context, pgx.Tx, withdrawalgate.Input) (withdrawalgate.Decision, error) {
	return withdrawalgate.Decision{}, errors.New("injected evaluation failure")
}

type unknownCodeGate struct{}

func (unknownCodeGate) Evaluate(context.Context, pgx.Tx, withdrawalgate.Input) (withdrawalgate.Decision, error) {
	return withdrawalgate.Decision{Allowed: false, GateCode: "WD999", Reason: "x"}, nil
}

// ADR 0096 s8 item 3: a REPLAY of an existing request returns the original even when the account is no longer
// active - the gate never turns an already-placed hold into a denial.
func TestWDRG1_Replay_IsNotReEvaluated(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	key := "wdrg1-replay-" + uuid.NewString()
	first, err := req(t, pool, f, baseParams(f, key, 400))
	if err != nil {
		t.Fatal(err)
	}
	setAccountStatus(t, pool, f, identity.PlayerStatusSuspended)
	again, err := req(t, pool, f, baseParams(f, key, 400))
	if err != nil || again.ID != first.ID {
		t.Fatalf("a replay must return the original request, got %v %+v", err, again)
	}
	// ...but a NEW key on the suspended account is refused.
	_, err = reqCommitDenial(t, pool, f, baseParams(f, "wdrg1-new-"+uuid.NewString(), 100))
	var denied *GateDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("a new request on a suspended account must be refused, got %v", err)
	}
}

// Tenant isolation: the gate evaluates the account inside the request tenant; another tenant's account is not
// found, which is a refusal (never an allow) and writes nothing.
func TestWDRG1_TenantIsolation_ForeignAccountIsRefused(t *testing.T) {
	pool := testPool(t)
	a := seedFixture(t, pool, 10_000)
	b := seedFixture(t, pool, 10_000)
	p := baseParams(a, "wdrg1-xt-"+uuid.NewString(), 400)
	// tenant B's session, tenant A's account / wallet ids
	err := runTx(pool, b.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p2 := p
		p2.TenantID = b.tenantID
		_, e := RequestWithdrawal(ctx, tx, p2)
		return e
	})
	if err == nil {
		t.Fatal("a cross-tenant request must be refused")
	}
	nothingWritten(t, pool, a, 10_000, "tenant A")
	nothingWritten(t, pool, b, 10_000, "tenant B")
}

// SR6-2: the gate's OWN tenant isolation. A tenant-B session handed tenant A's ids (and tenant A's id with a
// tenant-B session) must return an error and never Allowed, whatever the earlier request gates do.
func TestWDRG1_StandardGate_TenantIsolation_DirectEvaluate(t *testing.T) {
	pool := testPool(t)
	a := seedFixture(t, pool, 10_000)
	b := seedFixture(t, pool, 10_000)
	for name, tc := range map[string]struct {
		session uuid.UUID
		in      withdrawalgate.Input
	}{
		"tenant B session, tenant A ids": {b.tenantID, withdrawalgate.Input{TenantID: a.tenantID, BrandID: a.brandID, PlayerAccountID: a.playerAccountID, WalletID: a.walletID, AssetCode: "EUR", Amount: 100}},
		"tenant B ids, tenant A account": {b.tenantID, withdrawalgate.Input{TenantID: b.tenantID, BrandID: b.brandID, PlayerAccountID: a.playerAccountID, WalletID: a.walletID, AssetCode: "EUR", Amount: 100}},
	} {
		var d withdrawalgate.Decision
		err := runTx(pool, tc.session, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			d, e = standard.New().Evaluate(ctx, tx, tc.in)
			return e
		})
		if err == nil || d.Allowed {
			t.Fatalf("%s: the gate must return an error and never Allowed, got err=%v decision=%+v", name, err, d)
		}
	}
}

// SR6-3: the denial audit names the player as the actor and carries the request metadata.
func TestWDRG1_DenialAudit_RecordsPlayerActorAndRequestMetadata(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return identity.SetPlayerAccountStatus(ctx, tx, f.playerAccountID, identity.PlayerStatusSuspended)
	}); err != nil {
		t.Fatal(err)
	}
	p := baseParams(f, "wdrg1-meta-"+uuid.NewString(), 100)
	p.IPAddress, p.UserAgent, p.RequestID = "203.0.113.7", "wdrg1-agent", "req-wdrg1-meta"
	_, err := reqCommitDenial(t, pool, f, p)
	var denied *GateDeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("a suspended account must be a gate denial, got %v", err)
	}
	var actorType, actorID, ip, ua, rid string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT actor_type, COALESCE(actor_id::text, ''), COALESCE(host(ip_address), ''), COALESCE(user_agent, ''), COALESCE(request_id, '')
			FROM audit_log WHERE tenant_id = $1 AND action = 'withdrawal.denied_by_rg' ORDER BY created_at DESC LIMIT 1`, f.tenantID).Scan(&actorType, &actorID, &ip, &ua, &rid)
	}); err != nil {
		t.Fatal(err)
	}
	if actorType != "player" || actorID != f.playerAccountID.String() || ip != "203.0.113.7" || ua != "wdrg1-agent" || rid != "req-wdrg1-meta" {
		t.Fatalf("denial audit actor/metadata: %q %q %q %q %q", actorType, actorID, ip, ua, rid)
	}
}

// LF-A6-7: a withdrawal request (hold) and its release change neither the stake (wager / loss) nor the deposit usage.
func TestWDRG1_WithdrawalHoldAndRelease_DoNotChangeStakeOrDepositUsage(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	measure := func(what string) (string, string, string, string) {
		var wager, loss, dep, inflight string
		if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			win := []rg.WindowSpec{{Period: rg.PeriodRolling24h}}
			su, e := rg.MeasureStakeUsage(ctx, tx, rg.UsageRequest{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID, Scope: rg.LimitScopeAccount, AssetCode: "EUR", Windows: win})
			if e != nil {
				return e
			}
			du, e := rg.MeasureDepositUsage(ctx, tx, rg.UsageRequest{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID, Scope: rg.LimitScopeAccount, AssetCode: "EUR", Windows: win})
			if e != nil {
				return e
			}
			wager, loss = su.Windows[0].Wager.Account.String(), su.Windows[0].LossRaw.Account.String()
			dep, inflight = du.Windows[0].Credited.Account.String(), du.Windows[0].InFlight.Account.String()
			return nil
		}); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		return wager, loss, dep, inflight
	}
	w0, l0, d0, i0 := measure("before")
	wr, err := req(t, pool, f, baseParams(f, "wdrg1-usage-"+uuid.NewString(), 400))
	if err != nil {
		t.Fatal(err)
	}
	w1, l1, d1, i1 := measure("hold placed")
	if w0 != w1 || l0 != l1 || d0 != d1 || i0 != i1 {
		t.Fatalf("a withdrawal hold changed usage: before (%s %s %s %s) after hold (%s %s %s %s)", w0, l0, d0, i0, w1, l1, d1, i1)
	}
	if err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return Cancel(ctx, tx, wr.ID) }); err != nil {
		t.Fatalf("release the hold: %v", err)
	}
	w2, l2, d2, i2 := measure("hold released")
	if w0 != w2 || l0 != l2 || d0 != d2 || i0 != i2 {
		t.Fatalf("releasing a withdrawal hold changed usage: before (%s %s %s %s) after release (%s %s %s %s)", w0, l0, d0, i0, w2, l2, d2, i2)
	}
}
