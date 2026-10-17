//go:build integration

// F4 (A6 SR6 I-2): the database backstop of the WD-RG-1 gate at payout dispatch (migration 0169) and the
// dispatch-gate helpers of dispatchgate.go. The T1p / T2 / T12 integration (the gate actually re-run by the
// dispatch code) is tested in internal/payments (f4_payout_gate_integration_test.go).
package withdrawal

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/rg"
	"github.com/Diansalas/igaming-platform/internal/risk"
	"github.com/Diansalas/igaming-platform/internal/testsupport/negbal"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate"
	"github.com/Diansalas/igaming-platform/internal/withdrawalgate/standard"
)

var f4Allow = withdrawalgate.Decision{Allowed: true, Outcome: "allow"}

func f4Record(ctx context.Context, tx pgx.Tx, id uuid.UUID, d withdrawalgate.Decision) error {
	wr, err := GetByID(ctx, tx, id)
	if err != nil {
		return err
	}
	return RecordDispatchGateDecision(ctx, tx, wr, DispatchStageDispatch, nil, d)
}

// E7 without an allowing decision of the same transaction is refused WD053, whatever writer performs it, and nothing
// changes; with one it passes.
func TestF4_DB_E7_NeedsAnAllowingDecisionOfTheSameTransaction(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	wantCode(t, "MarkSubmittedPending with no decision", w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
	}), "WD053")
	wantCode(t, "MarkSubmitted with no decision", w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return MarkSubmitted(ctx, tx, wr.ID, a13PSP, "ref-"+uuid.NewString()[:8])
	}), "WD053")
	wantCode(t, "raw E7 with no decision", w.rtExec(`UPDATE withdrawal_requests SET state = 'submitted', provider_id = 'p' WHERE id = $1`, wr.ID), "WD053")
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return TranslateDispatchBackstopError(MarkSubmittedPending(ctx, tx, wr.ID, a13PSP))
	}); !errors.Is(err, ErrDispatchGateBackstop) || a13Code(err) != "WD053" {
		t.Fatalf("a WD053 refusal translates to ErrDispatchGateBackstop (keeping the SQLSTATE), got %v", err)
	}

	// A DENYING decision in the same transaction refuses even next to an allowing one.
	deny := withdrawalgate.Decision{GateCode: GateCodeRisk, Reason: risk.CodeHardLimitBreach, Outcome: "deny"}
	wantCode(t, "deny only", w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := f4Record(ctx, tx, wr.ID, deny); err != nil {
			return err
		}
		return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
	}), "WD053")
	wantCode(t, "allow + deny", w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := f4Record(ctx, tx, wr.ID, f4Allow); err != nil {
			return err
		}
		if err := f4Record(ctx, tx, wr.ID, deny); err != nil {
			return err
		}
		return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
	}), "WD053")

	// An allowing decision committed by an EARLIER transaction does not authorize a later transition (freshness = the
	// transaction itself).
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error { return f4Record(ctx, tx, wr.ID, f4Allow) }); err != nil {
		t.Fatalf("a committed allow row: %v", err)
	}
	wantCode(t, "allow from an earlier transaction", w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
	}), "WD053")
	if got := w.get(t, wr.ID); got.State != StateApproved {
		t.Fatalf("every refusal changes nothing, state %s", got.State)
	}

	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if err := f4Record(ctx, tx, wr.ID, f4Allow); err != nil {
			return err
		}
		return MarkSubmittedPending(ctx, tx, wr.ID, a13PSP)
	}); err != nil {
		t.Fatalf("an allowing decision of the same transaction admits E7: %v", err)
	}
	if got := w.get(t, wr.ID); got.State != StateSubmitted {
		t.Fatalf("want submitted, got %s", got.State)
	}
	// No decision can be added to a request that is no longer approved (replay of a dispatch decision).
	wantCode(t, "dispatch decision for a submitted request", w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return f4Record(ctx, tx, wr.ID, f4Allow)
	}), "WD054")
}

// The RG half is re-checked in SQL at the instant of E7 (WD052), even with an allowing decision recorded: account
// not active, wallet not active, negative wallet balance. A self-exclusion does NOT refuse (ADR 0115).
func TestF4_DB_E7_RGBackstop(t *testing.T) {
	w := newA13(t)
	submit := func(id uuid.UUID) error {
		return w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
			if err := f4Record(ctx, tx, id, f4Allow); err != nil {
				return err
			}
			return MarkSubmittedPending(ctx, tx, id, a13PSP)
		})
	}
	ownerExec := func(sql string, args ...any) {
		t.Helper()
		mustRunTx(t, w.owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, sql, args...)
			return err
		})
	}

	wr := w.at(t, StateApproved)
	for _, st := range []identity.PlayerAccountStatus{identity.PlayerStatusSuspended, identity.PlayerStatusPendingVerification} {
		setAccountStatus(t, w.owner, w.f, st)
		err := submit(wr.ID)
		wantCode(t, "account "+string(st), err, "WD052")
		wantMsg(t, "account "+string(st), err, "player_account_not_active")
	}
	setAccountStatus(t, w.owner, w.f, identity.PlayerStatusActive)

	ownerExec(`UPDATE wallets SET status = 'frozen' WHERE id = $1`, w.f.walletID)
	err := submit(wr.ID)
	wantCode(t, "frozen wallet", err, "WD052")
	wantMsg(t, "frozen wallet", err, "wallet_not_active")
	ownerExec(`UPDATE wallets SET status = 'active' WHERE id = $1`, w.f.walletID)

	// Self-excluded Person: allowed (the restriction does not block a withdrawal).
	if err := w.owner.WithPlayerScope(context.Background(), w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: w.f.tenantID, PlayerAccountID: w.f.playerAccountID})
		return e
	}); err != nil {
		t.Fatalf("self-exclude: %v", err)
	}
	if err := submit(wr.ID); err != nil {
		t.Fatalf("a self-exclusion does not block the dispatch of a withdrawal: %v", err)
	}
}

// The negative-balance rule (owner decision 13) at dispatch is the APPLICATION gate's (record residual R2: not mirrored
// in SQL): a wallet driven negative after approval (a chargeback) is a WD050 wallet_negative_balance denial.
func TestF4_EvaluateDispatchGate_NegativeBalanceAfterApproval_Refuses(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	mustRunTx(t, w.owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := negbal.Chargeback(ctx, tx, w.f.tenantID, w.f.walletID, "EUR", 200_000_000) // far below zero
		return err
	})
	var d withdrawalgate.Decision
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		var e error
		d, e = EvaluateDispatchGate(ctx, tx, standard.New(), wr)
		return e
	}); err != nil || d.Allowed || d.GateCode != GateCodeRG || d.Reason != rg.CodeWalletNegativeBalance {
		t.Fatalf("want a WD050 wallet_negative_balance denial at dispatch, got %+v %v", d, err)
	}
}

// Completion, failure, rejection and cancellation are NOT gated: once money may have left, the truth is recorded.
func TestF4_DB_TerminalEdgesAreNotGated(t *testing.T) {
	w := newA13(t)
	toComplete := w.at(t, StateSubmitted)
	toFail := w.at(t, StateSubmitted)
	toReject := w.at(t, StateApproved)
	toCancel := w.at(t, StateRequested)
	setAccountStatus(t, w.owner, w.f, identity.PlayerStatusSuspended)
	mustRunTx(t, w.owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE wallets SET status = 'frozen' WHERE id = $1`, w.f.walletID)
		return err
	})
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return Complete(ctx, tx, toComplete.ID, a13PSP, "ptx-"+toComplete.ID.String())
	}); err != nil {
		t.Fatalf("E9 on a suspended account / frozen wallet: %v", err)
	}
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error { return Fail(ctx, tx, toFail.ID, "f4_failed") }); err != nil {
		t.Fatalf("E10 on a suspended account / frozen wallet: %v", err)
	}
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		if _, err := LockApprovedForSubmission(ctx, tx, toReject.ID); err != nil {
			return err
		}
		params, decision := denialDecision(w.f, toReject.ID)
		_, err := DenyForCompliance(ctx, tx, toReject.ID, decision, params)
		return err
	}); err != nil {
		t.Fatalf("E5 on a suspended account / frozen wallet: %v", err)
	}
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error { return Cancel(ctx, tx, toCancel.ID) }); err != nil {
		t.Fatalf("E2 on a suspended account / frozen wallet: %v", err)
	}
	for id, want := range map[uuid.UUID]State{toComplete.ID: StateCompleted, toFail.ID: StateFailed, toReject.ID: StateRejected, toCancel.ID: StateCancelled} {
		if got := w.get(t, id); got.State != want {
			t.Fatalf("%s: want %s, got %s", id, want, got.State)
		}
	}
}

// The decision table: append-only (WD055, owner included), trigger-owned id / instant / transaction, the row shape
// and stage rules (WD054, CHECKs), the runtime role's grants (SELECT, INSERT only).
func TestF4_DB_DecisionRows_AppendOnly_Shape_AndTriggerOwnedColumns(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	forged := uuid.New()
	var gotID uuid.UUID
	var sameXact bool
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `INSERT INTO withdrawal_gate_decisions (id, tenant_id, withdrawal_request_id, stage, allowed, decided_at, decided_xact)
			VALUES ($1, $2, $3, 'dispatch', true, '2000-01-01', '1') RETURNING id, decided_xact = pg_current_xact_id()`, forged, w.f.tenantID, wr.ID).Scan(&gotID, &sameXact)
	}); err != nil {
		t.Fatal(err)
	}
	if gotID == forged || !sameXact {
		t.Fatalf("id and decided_xact must be set by the trigger, never by the caller (id forged=%v, same xact=%v)", gotID == forged, sameXact)
	}
	var old bool
	mustRunTx(t, w.owner, w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT decided_at < now() - interval '1 day' FROM withdrawal_gate_decisions WHERE id = $1`, gotID).Scan(&old)
	})
	if old {
		t.Fatal("decided_at must be set by the trigger")
	}

	for _, sql := range []string{
		`UPDATE withdrawal_gate_decisions SET allowed = false`,
		`DELETE FROM withdrawal_gate_decisions`,
	} {
		if err := w.rtExec(sql); a13Code(err) != "42501" && a13Code(err) != "WD055" {
			t.Fatalf("runtime %q: want 42501 (no grant) or WD055, got %v", sql, err)
		}
		err := w.owner.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, e := tx.Exec(ctx, sql)
			return e
		})
		wantCode(t, "owner "+sql, err, "WD055")
	}
	err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `TRUNCATE withdrawal_gate_decisions`)
		return e
	})
	wantCode(t, "owner TRUNCATE", err, "WD055")

	ins := func(id uuid.UUID, stage string, attempt *uuid.UUID, allowed bool, gate, reason *string) error {
		return w.rtExec(`INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, payment_attempt_id, stage, allowed, gate_code, reason_code)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, w.f.tenantID, id, attempt, stage, allowed, gate, reason)
	}
	s := func(v string) *string { return &v }
	requested := w.at(t, StateRequested)
	wantCode(t, "dispatch decision for a requested request", ins(requested.ID, "dispatch", nil, true, nil, nil), "WD054")
	// (The BEFORE INSERT trigger runs before the CHECKs: an approved request refuses any re-dispatch row first.)
	wantCode(t, "re-dispatch decision without an attempt", ins(wr.ID, "redispatch", nil, true, nil, nil), "WD054")
	a := uuid.New()
	wantCode(t, "re-dispatch decision for an approved request", ins(wr.ID, "redispatch", &a, true, nil, nil), "WD054")
	wantCode(t, "denial without a reason", ins(wr.ID, "dispatch", nil, false, s("WD050"), nil), "23514")
	wantCode(t, "denial with an unknown gate code", ins(wr.ID, "dispatch", nil, false, s("WD099"), s("x")), "23514")
	wantCode(t, "allow with a gate code", ins(wr.ID, "dispatch", nil, true, s("WD050"), s("x")), "23514")
	wantCode(t, "unknown stage", ins(wr.ID, "request", nil, true, nil, nil), "23514")
	wantCode(t, "unknown request", ins(uuid.New(), "dispatch", nil, true, nil, nil), "WD054")
}

// Tenant isolation: another tenant's session sees none of the rows and cannot write one for a foreign request; a
// player-scope session neither reads nor writes.
func TestF4_DB_DecisionRows_TenantIsolation(t *testing.T) {
	w := newA13(t)
	wr := w.at(t, StateApproved)
	if err := w.rtTx(func(ctx context.Context, tx pgx.Tx) error { return f4Record(ctx, tx, wr.ID, f4Allow) }); err != nil {
		t.Fatal(err)
	}
	other := seedFixture(t, w.owner, 0)
	var n int
	if err := w.rt.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_gate_decisions WHERE withdrawal_request_id = $1`, wr.ID).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("another tenant must see no row: n=%d err=%v", n, err)
	}
	// Tenant B's session, tenant A's tenant id: RLS WITH CHECK.
	err := w.rt.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed) VALUES ($1, $2, 'dispatch', true)`, w.f.tenantID, wr.ID)
		return e
	})
	// The trigger reads the request under the session's RLS first (invisible: WD054); RLS WITH CHECK (42501) is the
	// second line. Either is a refusal.
	if c := a13Code(err); c != "WD054" && c != "42501" {
		t.Fatalf("foreign tenant id: want WD054 or 42501, got %v", err)
	}
	// Tenant B's own tenant id, tenant A's request: not visible.
	err = w.rt.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed) VALUES ($1, $2, 'dispatch', true)`, other.tenantID, wr.ID)
		return e
	})
	wantCode(t, "foreign request", err, "WD054")
	// Player scope: no row, no write.
	if err := w.rt.WithPlayerScope(context.Background(), w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM withdrawal_gate_decisions`).Scan(&n)
	}); err != nil || n != 0 {
		t.Fatalf("a player session must see no row: n=%d err=%v", n, err)
	}
	err = w.rt.WithPlayerScope(context.Background(), w.f.tenantID, w.f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO withdrawal_gate_decisions (tenant_id, withdrawal_request_id, stage, allowed) VALUES ($1, $2, 'dispatch', true)`, w.f.tenantID, wr.ID)
		return e
	})
	if err == nil {
		t.Fatal("a player session must not write a decision row")
	}
	// Another tenant's E7 attempt on tenant A's row matches nothing (RLS), it never reaches the guard as an allow.
	if err := w.rt.WithTenant(context.Background(), other.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, e := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'submitted', provider_id = 'p' WHERE id = $1`, wr.ID)
		if e == nil && tag.RowsAffected() != 0 {
			return errors.New("a foreign tenant updated the row")
		}
		return e
	}); err != nil {
		t.Fatal(err)
	}
}

// EvaluateDispatchGate: the request-time rules, verbatim. A nil, failing or unrecognised gate is ErrGateUnavailable
// (never an allow); the standard gate allows during a self-exclusion and reports it, refuses a suspended account
// (WD050) and a risk DENY (WD051).
func TestF4_EvaluateDispatchGate_SameRulesAsRequestTime(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool, 10_000)
	wr := mustRequestWithdrawal(t, pool, f, 600, "f4-eval-"+uuid.NewString())
	eval := func(g withdrawalgate.Gate) (withdrawalgate.Decision, error) {
		var d withdrawalgate.Decision
		err := runTx(pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var e error
			d, e = EvaluateDispatchGate(ctx, tx, g, wr)
			return e
		})
		return d, err
	}
	for name, g := range map[string]withdrawalgate.Gate{"nil": nil, "failing": failingGate{}, "unknown code": unknownCodeGate{}} {
		if _, err := eval(g); !errors.Is(err, ErrGateUnavailable) {
			t.Fatalf("%s gate: want ErrGateUnavailable, got %v", name, err)
		}
	}
	if d, err := eval(standard.New()); err != nil || !d.Allowed || d.RestrictionInForce != "" {
		t.Fatalf("active account, no rule: want allow, got %+v %v", d, err)
	}
	if err := pool.WithPlayerScope(context.Background(), f.tenantID, f.playerAccountID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := rg.CreateSelfExclusion(ctx, tx, rg.CreateSelfExclusionParams{TenantID: f.tenantID, PlayerAccountID: f.playerAccountID})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if d, err := eval(standard.New()); err != nil || !d.Allowed || d.RestrictionInForce != string(rg.RestrictionSelfExclusion) {
		t.Fatalf("self-excluded: want allow with the restriction reported, got %+v %v", d, err)
	}
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, e := risk.CreateRule(ctx, tx, risk.CreateRuleParams{
			TenantID: &f.tenantID, Operation: risk.OperationWithdrawal, LimitKind: risk.LimitMaxAmount, TimeWindow: risk.WindowTransaction,
			Threshold: 500, AssetCode: "EUR", RuleKind: risk.RuleHardLimit, Action: risk.ActionDeny,
			CreatedByActorType: "staff", CreatedByActorID: uuid.New(), Description: "f4 dispatch rule",
		})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if d, err := eval(standard.New()); err != nil || d.Allowed || d.GateCode != GateCodeRisk || d.Reason != risk.CodeHardLimitBreach || d.Outcome != "deny" {
		t.Fatalf("a rule added after the request: want a WD051 denial, got %+v %v", d, err)
	}
	setAccountStatus(t, pool, f, identity.PlayerStatusSuspended)
	if d, err := eval(standard.New()); err != nil || d.Allowed || d.GateCode != GateCodeRG || d.Reason != rg.CodePlayerAccountNotActive {
		t.Fatalf("suspended: want a WD050 denial (RG first), got %+v %v", d, err)
	}
	// TranslateDispatchBackstopError: WD052 / WD053 map to ErrDispatchGateBackstop; anything else is unchanged.
	if e := TranslateDispatchBackstopError(errors.New("x")); errors.Is(e, ErrDispatchGateBackstop) {
		t.Fatal("a non-backstop error must not be translated")
	}
}

// Catalog pins: the two guards fire LAST among the BEFORE ROW triggers of their tables (every existing refusal keeps
// its precedence), no function is SECURITY DEFINER and each pins search_path, the table is FORCE RLS and the
// runtime role holds exactly SELECT and INSERT.
func TestF4_DB_Catalog(t *testing.T) {
	pool := testPool(t)
	var lastWR, lastPA, lastPAIns string
	var secdef, unpinned int
	var force bool
	var privs string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		q := `SELECT tgname FROM pg_trigger WHERE tgrelid = $1::regclass AND NOT tgisinternal
		        AND (tgtype & 2) = 2 AND (tgtype & 1) = 1 AND (tgtype & 16) = 16 ORDER BY tgname DESC LIMIT 1`
		if err := tx.QueryRow(ctx, q, "withdrawal_requests").Scan(&lastWR); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, q, "payment_attempts").Scan(&lastPA); err != nil {
			return err
		}
		// RF4-1: the T1p-shape INSERT guard is the LAST BEFORE INSERT row trigger of payment_attempts.
		if err := tx.QueryRow(ctx, `SELECT tgname FROM pg_trigger WHERE tgrelid = 'payment_attempts'::regclass AND NOT tgisinternal
			AND (tgtype & 2) = 2 AND (tgtype & 1) = 1 AND (tgtype & 4) = 4 ORDER BY tgname DESC LIMIT 1`).Scan(&lastPAIns); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FILTER (WHERE prosecdef), count(*) FILTER (WHERE NOT EXISTS (
				SELECT 1 FROM unnest(proconfig) c WHERE c LIKE 'search_path=%'))
			FROM pg_proc WHERE proname IN ('withdrawal_gate_decisions_before_insert', 'withdrawal_gate_decisions_deny_mutation',
				'withdrawal_dispatch_rg_violation', 'withdrawal_dispatch_decision_ok', 'withdrawal_requests_dispatch_gate_guard',
				'payment_attempts_payout_redispatch_gate_guard', 'payment_attempts_payout_dispatch_insert_guard')`).Scan(&secdef, &unpinned); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT relforcerowsecurity FROM pg_class WHERE oid = 'withdrawal_gate_decisions'::regclass`).Scan(&force); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT coalesce(string_agg(privilege_type, ',' ORDER BY privilege_type), '')
			FROM information_schema.role_table_grants WHERE grantee = 'igaming_runtime' AND table_name = 'withdrawal_gate_decisions'`).Scan(&privs)
	}); err != nil {
		t.Fatal(err)
	}
	if lastWR != "withdrawal_requests_zz_dispatch_gate_guard" || lastPA != "payment_attempts_zz_payout_redispatch_gate_guard" {
		t.Fatalf("the F4 guards must be the last BEFORE UPDATE row triggers: %q, %q", lastWR, lastPA)
	}
	if lastPAIns != "payment_attempts_zz_payout_dispatch_insert_guard" {
		t.Fatalf("the RF4-1 guard must be the last BEFORE INSERT row trigger of payment_attempts, got %q", lastPAIns)
	}
	if secdef != 0 || unpinned != 0 {
		t.Fatalf("F4 functions: %d SECURITY DEFINER, %d without a pinned search_path", secdef, unpinned)
	}
	if !force {
		t.Fatal("withdrawal_gate_decisions must be FORCE RLS")
	}
	if privs != "INSERT,SELECT" {
		t.Fatalf("igaming_runtime must hold exactly INSERT, SELECT on withdrawal_gate_decisions, got %q", privs)
	}
}

// Migration 0169 on a THROWAWAY scratch database: up -> down restores the previous schema byte for byte -> up again
// equals the first up; the down refuses (WD095) while a decision row exists, changing nothing.
func TestMigration0169_UpDownUp_SnapshotAndDownRefusal(t *testing.T) {
	ctx := context.Background()
	pool := m132Scratch(t, "m0169_")
	if _, err := pool.MigrateUp(ctx, m132Dir(t, 168)); err != nil {
		t.Fatalf("migrate to 0168: %v", err)
	}
	snapBefore := m132Snapshot(t, pool)
	full := m132Dir(t, 169)
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0169 up: %v", err)
	}
	snapUp := m132Snapshot(t, pool)
	if snapUp == snapBefore {
		t.Fatal("0169 changed nothing (non-vacuity)")
	}
	for _, want := range []string{"withdrawal_gate_decisions", "withdrawal_requests_zz_dispatch_gate_guard", "payment_attempts_zz_payout_redispatch_gate_guard",
		"payment_attempts_zz_payout_dispatch_insert_guard"} {
		if !strings.Contains(snapUp, want) {
			t.Fatalf("0169 up must create %q", want)
		}
	}
	if _, err := pool.MigrateDown(ctx, full, 1); err != nil {
		t.Fatalf("migrate 0169 down: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapBefore {
		t.Fatalf("0169 down must restore the previous schema byte for byte:\n%s", m132Diff(snapBefore, got))
	}
	if _, err := pool.MigrateUp(ctx, full); err != nil {
		t.Fatalf("migrate 0169 up again: %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("0169 up again must equal the first up:\n%s", m132Diff(snapUp, got))
	}
	// Down refusal: one decision row (an approved request on the scratch database, as its owner).
	f := seedFixture(t, pool, 10_000)
	wr := mustRequestWithdrawal(t, pool, f, 100, "m0169-"+uuid.NewString())
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error { return MoveToPendingReview(ctx, tx, wr.ID) })
	approver := mustCreateApprover(t, pool, f.tenantID)
	mustRunTx(t, pool, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO withdrawal_approvals (tenant_id, withdrawal_request_id, approver_principal_id, decision,
			threshold_amount_at_decision, request_amount_at_decision) VALUES ($1, $2, $3, 'approve', 0, 100)`, f.tenantID, wr.ID, approver); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE withdrawal_requests SET state = 'approved' WHERE id = $1`, wr.ID); err != nil {
			return err
		}
		return f4Record(ctx, tx, wr.ID, f4Allow)
	})
	_, err := pool.MigrateDown(ctx, full, 1)
	if err == nil || !strings.Contains(err.Error(), "WD095") {
		t.Fatalf("0169 down must refuse WD095 while a decision row exists, got %v", err)
	}
	if got := m132Snapshot(t, pool); got != snapUp {
		t.Fatalf("a refused down must change nothing:\n%s", m132Diff(snapUp, got))
	}
}
