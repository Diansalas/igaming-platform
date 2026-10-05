//go:build integration

package payments

import (
	"errors"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// PAY-K3-STATEMENT-SOURCE-WIRING-1 service-level tests (the binary wiring is
// pinned in cmd/platform-api). The registry is keyed by provider id: "no
// non-MOCK source" is evaluated PER PROVIDER (owner ruling H-W2).

// Registered ONLY for some other provider id (as the MOCK source is for
// mock-payments while a real PSP has none): both kinds are refused at
// submission for this provider.
func TestWiring1_RegistryIsKeyedPerProvider(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1, noStatementSource: true})
	w.reg.Register("mock-payments") // the MOCK id, NOT this world's provider
	if w.reg.Registered(w.provider) {
		t.Fatal("setup: the world's provider must be unregistered")
	}
	_, a := w.ambiguousPayout(120)
	for _, kind := range []ResolutionKind{ResolutionM2DeclareNotPaid, ResolutionM2DeclarePaid} {
		if _, err := w.request(w.f1, w.m2In(a.ID, kind)); !errors.Is(err, ErrResolutionNoStatementSource) {
			t.Fatalf("%s with only another provider registered: want ErrResolutionNoStatementSource, got %v", kind, err)
		}
	}
	// Registering THIS provider unlocks exactly it, for both kinds.
	w.reg.Register(w.provider)
	_, b := w.ambiguousPayout(121) // one pending resolution per attempt
	if _, err := w.request(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid)); err != nil {
		t.Fatalf("not paid after registering the provider: %v", err)
	}
	if _, err := w.request(w.f1, w.m2In(b.ID, ResolutionM2DeclarePaid)); err != nil {
		t.Fatalf("paid after registering the provider: %v", err)
	}
	w.assertInvariants()
}

// Submission is fail closed in SHAPE: an unreadable/unknown attempt, a nil
// registry, and a nil-registry service all refuse; nothing is inserted.
func TestWiring1_SubmissionFailsClosed(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(130)
	for _, kind := range []ResolutionKind{ResolutionM2DeclareNotPaid, ResolutionM2DeclarePaid} {
		// Unknown attempt id: refused (not found), never falls through to the INSERT.
		_, err := w.request(w.f1, w.m2In(uuid.New(), kind))
		if err == nil || ClassifyResolutionError(err) != ResolutionErrNotFound {
			t.Fatalf("%s unknown attempt: want not-found refusal, got %v", kind, err)
		}
		// A service built with a NIL registry refuses every M2.
		saved := w.svc
		w.svc = NewManualResolutionService(w.pool, nil)
		_, err = w.request(w.f1, w.m2In(a.ID, kind))
		w.svc = saved
		if !errors.Is(err, ErrResolutionNoStatementSource) {
			t.Fatalf("%s with a nil registry: want ErrResolutionNoStatementSource, got %v", kind, err)
		}
	}
	if n := w.countRows(`SELECT count(*) FROM payment_manual_resolutions WHERE tenant_id = $1`, w.f.tenantID); n != 0 {
		t.Fatalf("refused submissions left %d resolution rows", n)
	}
}

// Execution refuses BOTH kinds with no_statement_source when the provider is not
// registered at execution time (registered at submission, then dropped - e.g. a
// restart with the source removed): refused_at_execution, no ledger entry, the
// withdrawal still submitted, the hold intact, the refused audit row written.
func TestWiring1_ExecutionRefusedWhenProviderUnregistered_BothKinds(t *testing.T) {
	for _, kind := range []ResolutionKind{ResolutionM2DeclareNotPaid, ResolutionM2DeclarePaid} {
		t.Run(string(kind), func(t *testing.T) {
			w := newK3World(t, k3Opts{base: 1})
			wr, a := w.ambiguousPayout(210)
			holdBefore := w.walletBalance("player_withdrawal_hold")
			clearingBefore := w.pspClearing()
			r := w.mustRequest(w.f1, w.m2In(a.ID, kind))
			w.svc = NewManualResolutionService(w.pool, &StatementSourceRegistry{}) // provider gone
			out, err := w.decide(w.f2, r, ResolutionApprove)
			if err != nil {
				t.Fatalf("decide: %v", err)
			}
			if !out.Refused || out.Executed || out.Resolution.State != ResolutionRefusedAtExecute ||
				out.Resolution.RefusalCode == nil || *out.Resolution.RefusalCode != resolutionRefusedNoSource {
				t.Fatalf("want refused_at_execution/no_statement_source, got %+v", out)
			}
			if out.Resolution.LedgerTransactionID != nil {
				t.Fatal("a refused resolution links a ledger transaction")
			}
			if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateSubmitted {
				t.Fatalf("withdrawal = %s, want submitted", got.State)
			}
			for _, typ := range []string{"withdrawal_completed", "withdrawal_failed"} {
				if n := w.ledgerTxCount(typ); n != 0 {
					t.Fatalf("%d %s transactions after a refused execution", n, typ)
				}
			}
			if w.walletBalance("player_withdrawal_hold") != holdBefore || w.pspClearing() != clearingBefore {
				t.Fatal("a refused execution moved money")
			}
			if after := w.attempt(a.ID); after.State != a.State {
				t.Fatalf("attempt state changed %s -> %s", a.State, after.State)
			}
			if !contains(w.auditActions("payment_manual_resolution", r.ID.String()), "payment.manual_resolution_refused") {
				t.Fatal("no refused audit row")
			}
			w.assertInvariants()
		})
	}
}

// Concurrency: two approvers race the final approval of a resolution whose
// provider is unregistered at execution. Exactly one decision lands (refused),
// the other sees a non-pending resolution; no posting either way.
func TestWiring1_ConcurrentFinalApprovalsUnregistered_NoPosting(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	wr, a := w.ambiguousPayout(220)
	r := w.mustRequest(w.f1, w.m2In(a.ID, ResolutionM2DeclareNotPaid))
	w.svc = NewManualResolutionService(w.pool, &StatementSourceRegistry{})
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
	for i := range outs {
		if outs[i].Executed {
			t.Fatalf("approver %d executed with no registered source", i)
		}
	}
	if got := w.resolution(r.ID); got.State == ResolutionExecuted {
		t.Fatalf("resolution executed: %+v", got)
	}
	if got := w.withdrawalOf(wr.ID); got.State != withdrawal.StateSubmitted {
		t.Fatalf("withdrawal = %s", got.State)
	}
	if n := w.ledgerTxCount("withdrawal_completed") + w.ledgerTxCount("withdrawal_failed"); n != 0 {
		t.Fatalf("%d postings from a refused M2", n)
	}
	w.assertInvariants()
}

// The registered provider executes both kinds (positive control for the gate:
// the existing C-tests cover the postings; this pins that the registry lookup
// admits exactly a registered provider on the execution path).
func TestWiring1_RegisteredProviderExecutesBothKinds(t *testing.T) {
	w := newK3World(t, k3Opts{base: 1})
	_, a := w.ambiguousPayout(230)
	if res := w.executeM2(a.ID, ResolutionM2DeclareNotPaid); res.State != ResolutionExecuted {
		t.Fatalf("not paid: %+v", res)
	}
	_, b := w.ambiguousPayout(240)
	if res := w.executeM2(b.ID, ResolutionM2DeclarePaid); res.State != ResolutionExecuted {
		t.Fatalf("paid: %+v", res)
	}
	w.assertInvariants()
}

func TestWiring1_StatementSourceRegisteredIsFailClosed(t *testing.T) {
	reg := &StatementSourceRegistry{}
	reg.Register("p1")
	svc := NewManualResolutionService(nil, reg)
	empty, p1, p2 := "", "p1", "p2"
	for name, tc := range map[string]struct {
		id   *string
		want bool
	}{"nil": {nil, false}, "empty": {&empty, false}, "registered": {&p1, true}, "other": {&p2, false}} {
		if got := svc.statementSourceRegistered(tc.id); got != tc.want {
			t.Fatalf("%s: got %v want %v", name, got, tc.want)
		}
	}
	var nilSvc *ManualResolutionService
	if nilSvc.statementSourceRegistered(&p1) {
		t.Fatal("nil service must be fail closed")
	}
	if (*StatementSourceRegistry)(nil).Registered("p1") {
		t.Fatal("nil registry must be fail closed")
	}
	(*StatementSourceRegistry)(nil).Register("p1") // must not panic
	reg.Register("")                               // ignored
	if reg.Registered("") {
		t.Fatal("empty id registered")
	}
}
