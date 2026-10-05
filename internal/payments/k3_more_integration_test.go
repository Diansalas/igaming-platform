//go:build integration

package payments

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/adjustment"
	"github.com/Diansalas/igaming-platform/internal/reconciliation"
	"github.com/Diansalas/igaming-platform/internal/tenant"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// parkedDepositWithReason drives a real deposit to pending and parks it with the
// given dispute reason through the real T10 writer.
func (w *k3World) parkedDepositWithReason(reason string) PaymentAttempt {
	w.t.Helper()
	res := rvInit(w.t, w.pool, w.orch, w.f.orchFixture, 5000, "k3-park-"+uuid.NewString())
	if res.Attempt.State != AttemptPending && res.Attempt.State != AttemptAmbiguous && res.Attempt.State != AttemptSubmitting {
		w.t.Fatalf("setup: deposit attempt is %s", res.Attempt.State)
	}
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		return ApplyDisputeFromNonTerminal(ctx, tx, res.Attempt.ID, EvidenceQueryStatus, reason)
	})
	a := w.attempt(res.Attempt.ID)
	if a.State != AttemptDisputed || a.TerminalReason == nil || *a.TerminalReason != reason {
		w.t.Fatalf("setup: want disputed/%s, got %s/%v", reason, a.State, a.TerminalReason)
	}
	return a
}

// C-1 (ADV): a deposit `disputed` -> any state is refused, even with an executed
// M1 resolution, for every reason in DepositDisputeTerminalReasons(), under every
// evidence kind; and an executed M1 leaves the attempt disputed.
func TestK3_C1_DepositDisputedToAnyStateRefusedEvenAfterExecutedM1(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	targets := []AttemptState{AttemptCreated, AttemptSubmitting, AttemptPending, AttemptAmbiguous, AttemptSucceeded, AttemptDeclined, AttemptRejected}
	kinds := []EvidenceKind{EvidenceOperator, EvidenceCallback, EvidenceQueryStatus, EvidenceSync}
	checked := 0
	depositsBefore := w.ledgerTxCount("deposit")
	for _, r := range DepositDisputeTerminalReasons() {
		reason := r
		if strings.HasSuffix(reason, ":") {
			reason += "control_char"
		}
		a := w.parkedDepositWithReason(reason)
		resn := w.mustRequest(w.f1, w.m1In(a.ID, "awaiting_psp_refund"))
		out, err := w.decide(w.f2, resn, ResolutionApprove)
		if err != nil || !out.Executed {
			t.Fatalf("%s: M1 must execute: %v %+v", reason, err, out)
		}
		if got := w.attempt(a.ID); got.State != AttemptDisputed {
			t.Fatalf("%s: an executed M1 moved the attempt to %s", reason, got.State)
		}
		w.tx(func(ctx context.Context, tx pgx.Tx) error {
			for _, target := range targets {
				for _, k := range kinds {
					err := k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
						_, err := tx.Exec(ctx, `UPDATE payment_attempts SET state = $2, last_evidence_kind = $3, resolved_at = now(), next_action_at = NULL WHERE id = $1`, a.ID, string(target), string(k))
						return err
					})
					if err == nil {
						t.Errorf("%s: disputed -> %s under %s was ADMITTED", reason, target, k)
					} else if !strings.Contains(err.Error(), "is not permitted") {
						t.Errorf("%s: disputed -> %s under %s refused for another reason (vacuity): %v", reason, target, k, err)
					}
					checked++
				}
			}
			return nil
		})
	}
	if checked < 9*len(targets)*len(kinds) {
		t.Fatalf("only %d (reason,target,evidence) cases checked", checked)
	}
	if n := w.ledgerTxCount("deposit"); n != depositsBefore {
		t.Fatalf("M1 / refused moves produced %d new deposit postings", n-depositsBefore)
	}
	w.assertInvariants()
}

// C-2 / LF-3: an executed M1 posts nothing, never clears pay_captured_unposted
// (standing on later runs), and the finding text says "acknowledges".
func TestK3_C2_M1NeverClearsCapturedUnposted(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	a := w.disputedDeposit(5000)
	ref := *a.ProviderReference
	depositsBefore := w.ledgerTxCount("deposit")
	line := w.depositLine(ref, a.MerchantReference, "succeeded", 4999)

	before := w.stmtRun(w.source(false, line))
	w.requireOne(before, reconciliation.MismatchKindPayCapturedUnposted, a.ID, "before M1")

	r := w.mustRequest(w.f1, w.m1In(a.ID, "investigated_no_platform_action"))
	out, err := w.decide(w.f2, r, ResolutionApprove)
	if err != nil || !out.Executed || out.Resolution.LedgerTransactionID != nil {
		t.Fatalf("M1: %v %+v", err, out)
	}
	for i := 0; i < 3; i++ {
		var ms []reconciliation.Mismatch
		if i == 0 {
			ms = w.stmtRun(w.source(false, line))
		} else {
			ms = w.stmtRun(w.pastSource(false))
		}
		m := w.requireOne(ms, reconciliation.MismatchKindPayCapturedUnposted, a.ID, "after M1 (run "+string(rune('0'+i))+")")
		if !strings.Contains(m.ExpectedValue, "M1 only acknowledges") {
			t.Errorf("detail must say M1 only acknowledges: %q", m.ExpectedValue)
		}
	}
	if n := w.ledgerTxCount("deposit"); n != depositsBefore {
		t.Fatalf("M1 posted %d deposit transactions", n-depositsBefore)
	}
	w.assertInvariants()
}

// C-14: tenant isolation of the K3 tables and the service.
func TestK3_C14_TenantIsolation(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(100)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))

	// Another tenant's finance principal cannot see or touch the row (RLS) ...
	other := newK3WorldOn(t, w.pool, k3Opts{base: 1})
	err := w.pool.WithPrincipalScope(context.Background(), other.f.tenantID, other.f1.ID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM payment_manual_resolutions WHERE id = $1`, r.ID).Scan(&n); err != nil {
			return err
		}
		if n != 0 {
			t.Errorf("another tenant's principal sees %d rows", n)
		}
		_ = k3Try(ctx, tx, func(ctx context.Context, tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `UPDATE payment_manual_resolutions SET reason_code = 'x' WHERE id = $1`, r.ID)
			if err == nil && tag.RowsAffected() != 0 {
				t.Errorf("another tenant's principal updated %d rows", tag.RowsAffected())
			}
			return err
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// ... and the service refuses a tenant principal naming another tenant.
	if _, terr := NewResolutionTarget(tenant.Context{TenantID: other.f.tenantID}, w.f.tenantID); terr == nil {
		t.Error("a tenant principal naming another tenant must be refused at target construction")
	}
	// A plain platform caller (no acting grant for the tenant) is refused by the database.
	if _, perr := w.svc.Request(k3Ctx(w.adminA), w.target(w.adminA), w.m2In(a.ID, ResolutionM2DeclarePaid), ResolutionMeta{RequestID: "k3"}); perr == nil {
		t.Error("a platform admin with no grant for the tenant must be refused")
	}
}

// C-15 / T-15: audit content (IP, user agent, request id, actor, tenant status,
// before/after) and the LINK from the withdrawal.completed row (written with
// ActorSystem in the tenant session) to the resolution audit through the ledger
// transaction id and the reserved provider-tx id.
func TestK3_C15_T15_AuditContentAndLinkage(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(250)
	res := w.executeM2(a.ID, ResolutionM2DeclarePaid)

	rows := w.sysQuery(`SELECT action, actor_type::text AS actor_type, actor_id::text AS actor_id, ip_address::text AS ip, user_agent, request_id, outcome::text AS outcome, metadata::text AS md
		FROM audit_log WHERE tenant_id = $1 AND target_type = 'payment_manual_resolution' AND target_id = $2 ORDER BY created_at, id`, w.f.tenantID, res.ID.String())
	byAction := map[string]map[string]any{}
	for _, r := range rows {
		byAction[r["action"].(string)] = r
	}
	for _, act := range []string{"payment.manual_resolution_requested", "payment.manual_resolution_executed"} {
		r, ok := byAction[act]
		if !ok {
			t.Fatalf("no %s audit row (have %v)", act, rows)
		}
		if r["ip"] == nil || !strings.HasPrefix(r["ip"].(string), "203.0.113.7") || r["user_agent"] != "k3-test-agent" || r["request_id"] != "k3-test" {
			t.Errorf("%s: ip/ua/request id not recorded: %v %v %v", act, r["ip"], r["user_agent"], r["request_id"])
		}
		if r["actor_type"] != "staff" || r["outcome"] != "success" {
			t.Errorf("%s: actor/outcome = %v/%v", act, r["actor_type"], r["outcome"])
		}
		md := r["md"].(string)
		for _, want := range []string{"tenant_status_at_submit", "payload_hash", "kind"} {
			if !strings.Contains(md, want) {
				t.Errorf("%s: metadata lacks %s: %s", act, want, md)
			}
		}
	}
	if ex := byAction["payment.manual_resolution_executed"]; ex != nil {
		md := ex["md"].(string)
		for _, want := range []string{"ledger_transaction_id", "reserved_provider_tx_id", "before", "after"} {
			if !strings.Contains(md, want) {
				t.Errorf("executed audit lacks %s: %s", want, md)
			}
		}
		if !strings.Contains(md, res.LedgerTransactionID.String()) || !strings.Contains(md, *res.ReservedProviderTxID) {
			t.Errorf("executed audit does not carry the link ids: %s", md)
		}
		if ex["actor_id"] == nil || ex["actor_id"].(string) != w.f2.ID.String() {
			t.Errorf("the executing actor must be the final approver %s, got %v", w.f2.ID, ex["actor_id"])
		}
	}
	// The withdrawal.completed row: system actor, carrying the reserved id - the
	// join key to the resolution audit.
	wrows := w.sysQuery(`SELECT actor_type::text AS actor_type, metadata->>'provider_tx_id' AS ptx FROM audit_log
		WHERE tenant_id = $1 AND action = 'withdrawal.completed' AND target_id = $2`, w.f.tenantID, wr.ID.String())
	if len(wrows) != 1 || wrows[0]["actor_type"] != "system" || wrows[0]["ptx"] != *res.ReservedProviderTxID {
		t.Fatalf("withdrawal.completed audit link wrong: %v", wrows)
	}
	// ... and the ledger transaction of that very reserved id is the resolution's link.
	typ, ptx, _, _ := w.ledgerTxOf(*res.LedgerTransactionID)
	if typ != "withdrawal_completed" || ptx == nil || *ptx != wrows[0]["ptx"] {
		t.Fatalf("ledger tx %s: type=%s ptx=%v", res.LedgerTransactionID, typ, ptx)
	}
}

// C-12: concurrent final approvals execute exactly once.
func TestK3_C12_ConcurrentFinalApprovalsExecuteOnce(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(180)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid))
	var wg sync.WaitGroup
	outs := make([]ResolutionOutcome, 2)
	errs := make([]error, 2)
	for i, approver := range []k3Staff{w.f2, w.f3} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			outs[i], errs[i] = w.decide(approver, r, ResolutionApprove)
		}()
	}
	wg.Wait()
	executed := 0
	for i := range outs {
		if errs[i] == nil && outs[i].Executed {
			executed++
		}
	}
	if executed != 1 {
		t.Fatalf("executed = %d (errors %v / %v), want exactly one", executed, errs[0], errs[1])
	}
	if n := w.ledgerTxCount("withdrawal_completed"); n != 1 {
		t.Fatalf("withdrawal_completed postings = %d, want 1", n)
	}
	w.assertInvariants()
}

// C-12: M2 versus the sweeper's payout poll (and a late success): exactly one
// outcome, at most one Step B posting. Run with -race -count>=20.
func TestK3_C12_M2VersusSweeperPollRace(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(210)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclarePaid))
	// The provider will say "succeeded" with the attempt's own reference when polled.
	w.prov.setStatus(*a.ProviderReference, StatusResult{ProviderReference: *a.ProviderReference, Outcome: OutcomeSucceeded, Amount: 210, AssetCode: "EUR"})
	setNextActionNow(t, w.pool, w.f.tenantID, a.ID)

	var wg sync.WaitGroup
	var decideErr error
	var out ResolutionOutcome
	wg.Add(2)
	go func() {
		defer wg.Done()
		out, decideErr = w.decide(w.f2, r, ResolutionApprove)
	}()
	go func() {
		defer wg.Done()
		w.sweep()
	}()
	wg.Wait()

	if n := w.ledgerTxCount("withdrawal_completed"); n != 1 {
		t.Fatalf("withdrawal_completed postings = %d, want exactly 1 (decide err %v, out %+v)", n, decideErr, out)
	}
	if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("withdrawal = %s", got.State)
	}
	if got := w.attempt(a.ID); got.State != AttemptSucceeded {
		t.Fatalf("attempt = %s", got.State)
	}
	final := w.resolution(r.ID)
	switch final.State {
	case ResolutionExecuted:
		if final.LedgerTransactionID == nil {
			t.Fatal("an executed resolution must carry its ledger link")
		}
	case ResolutionRefusedAtExecute, ResolutionPending:
		if final.LedgerTransactionID != nil {
			t.Fatalf("a %s resolution must carry no ledger link", final.State)
		}
	default:
		t.Fatalf("unexpected resolution state %s", final.State)
	}
	w.assertInvariants()
}

// C-12b: a K2 compensation and an M2 execution on the same wallet concurrently:
// the lock order holds (no deadlock), both finish, the invariants hold.
func TestK3_C12b_K2CompensationAndM2ExecutionOnTheSameWallet(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	w.k2Setup()
	_, a1 := w.ambiguousPayout(400)
	res1 := w.executeM2(a1.ID, ResolutionM2DeclarePaid)
	_, a2 := w.ambiguousPayout(300)
	r2 := w.mustRequest(w.f1, w.m2In(a2.ID, ResolutionM2DeclarePaid))
	k2r, err := w.k2Submit(w.f3, adjustment.DirectionCreditPlayer, 100, *res1.LedgerTransactionID)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var m2out ResolutionOutcome
	var m2err, k2err error
	var k2out adjustment.Outcome
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); m2out, m2err = w.decide(w.f2, r2, ResolutionApprove) }()
	go func() { defer wg.Done(); k2out, k2err = w.k2Decide(w.f4, k2r) }()
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("deadlock: the K2 compensation and the M2 execution did not both finish")
	}
	if m2err != nil || !m2out.Executed {
		t.Fatalf("M2 execution: %v %+v", m2err, m2out)
	}
	if k2err != nil || !k2out.Executed {
		t.Fatalf("K2 compensation: %v %+v", k2err, k2out)
	}
	w.assertInvariants()
}

// C-49 for "not paid": the drift stream is clean and the hold is released.
func TestK3_C49_DriftCleanAfterNotPaid(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(260)
	w.executeM2(a.ID, ResolutionM2DeclareNotPaid)
	w.tx(func(ctx context.Context, tx pgx.Tx) error {
		_, ms, err := reconciliation.RunLedgerVsProjection(ctx, tx, w.f.tenantID, a.CreatedAt.Add(-time.Minute), time.Now().Add(time.Hour))
		if err != nil {
			return err
		}
		if len(ms) != 0 {
			t.Fatalf("RunLedgerVsProjection after M2 not-paid: %d mismatches", len(ms))
		}
		return nil
	})
	w.assertInvariants()
}
