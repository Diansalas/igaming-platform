//go:build integration

// H(8) (ADR 0095 section 44 decisions 19-23, owner-approved 2026-10-08): the BRAND is a policy
// check separate from the tenant, at cascade-child creation (19) and at sweeper dispatch/claim
// (20); an existing child whose brand becomes inactive stays deferred and is never dispatched
// (21); nothing is cancelled or released because of it (22). MOCK provider only.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Diansalas/igaming-platform/internal/db"
)

type h8State struct {
	intentStatus string
	attempts     int
	ledgerTx     int
	auditRows    int64
}

func h8Snap(t *testing.T, pool *db.Pool, f orchFixture, intentID uuid.UUID) h8State {
	t.Helper()
	return h8State{
		intentStatus: depScan[string](t, pool, f.tenantID, `SELECT status FROM deposit_intents WHERE id = $1`, intentID),
		attempts:     attemptsForIntent(t, pool, f.tenantID, intentID),
		ledgerTx:     ledgerTxCount(t, pool, f.tenantID),
		auditRows:    depScan[int64](t, pool, f.tenantID, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, f.tenantID),
	}
}

// h8Refused asserts the "deferred" outcome: attempt untouched (created, unclaimed, no provider),
// rescheduled by the deferral itself, intent unchanged, no new attempt, no ledger row, and no
// cancellation/release audit of any kind.
func h8Refused(t *testing.T, pool *db.Pool, f orchFixture, intentID, attID uuid.UUID, before h8State, minPolls int) PaymentAttempt {
	t.Helper()
	got := mustGetAttempt(t, pool, f.tenantID, attID)
	if got.State != AttemptCreated || got.ProviderID != nil || got.SubmitCount != 0 || got.ClaimToken != nil {
		t.Fatalf("deferred attempt must be created/unclaimed/unsent: state=%s provider=%v submits=%d", got.State, got.ProviderID, got.SubmitCount)
	}
	assertDeferred(t, pool, f.tenantID, attID, 0, minPolls)
	after := h8Snap(t, pool, f, intentID)
	if after.intentStatus != before.intentStatus || after.attempts != before.attempts || after.ledgerTx != before.ledgerTx {
		t.Fatalf("intent/attempts/ledger changed by a deferral: before=%+v after=%+v", before, after)
	}
	if n := depScan[int64](t, pool, f.tenantID, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1 AND state IN ('rejected','declined','cancelled','failed')`, intentID); n != 0 {
		t.Fatalf("no attempt may be terminated by a deferral, got %d", n)
	}
	assertLedgerBalanced(t, pool, f.tenantID)
	return got
}

// Decisions 20/21/23 matrix through the real sweeper pass (pre-read + claim tx), as the runtime
// role. A control tenant with an active brand dispatches in the same pass.
func TestH8_SweeperDispatch_Matrix(t *testing.T) {
	for _, c := range []hsecCase{
		{"active_tenant_active_brand", "", ""},
		{"active_tenant_brand_suspended", "", "suspended"},
		{"active_tenant_brand_closed", "", "closed"},
		{"tenant_suspended_active_brand", "suspended", ""},
		{"tenant_closed_active_brand", "closed", ""},
		{"both_inactive", "suspended", "suspended"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			spy := newLoopProvider("mock-psp-h8m-" + uuid.NewString()[:8])
			orch := spy.orchestrator()
			f, ctl := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
			registerCapability(t, pool, f, spy, 100)
			registerCapability(t, pool, ctl, spy, 100)
			intentID := insertRawDepositIntent(t, pool, f, "pending")
			attID := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
			ctlAtt := insertRawCreatedAttempt(t, pool, ctl.tenantID, insertRawDepositIntent(t, pool, ctl, "pending"), false, time.Now())
			applyHsecCase(t, pool, f, c)
			before := h8Snap(t, pool, f, intentID)

			if st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
				t.Fatalf("pass: %+v", st)
			}
			if mustGetAttempt(t, pool, ctl.tenantID, ctlAtt).State == AttemptCreated {
				t.Fatal("control tenant/brand must dispatch in the same pass")
			}
			d, _, _ := spy.counts()
			if c.tenant == "" && c.brand == "" {
				if d != 2 || mustGetAttempt(t, pool, f.tenantID, attID).State == AttemptCreated {
					t.Fatalf("active tenant + active brand must dispatch: deposits=%d", d)
				}
				return
			}
			if d != 1 {
				t.Fatalf("only the control may reach the provider, got %d Deposit calls", d)
			}
			h8Refused(t, pool, f, intentID, attID, before, 1)
			if (c.brand != "") && depScan[string](t, pool, f.tenantID, `SELECT status FROM brands WHERE id = $1`, f.brandID) != c.brand {
				t.Fatal("test premise: brand status")
			}
		})
	}
}

// A brand suspended between the sweeper's pre-read and the claim tx (the exact gap) is honoured
// by the in-claim-tx FOR SHARE read: deferred, nothing claimed; reactivation dispatches next pass.
// The same-tenant control with another tenant's brand is untouched.
func TestH8_BrandSuspendedAfterPreRead_ClaimTxDefers_ReactivationDispatches(t *testing.T) {
	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			spy := newLoopProvider("mock-psp-h8r-" + status)
			orch := spy.orchestrator()
			fRace, fCtl := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
			registerCapability(t, pool, fRace, spy, 100)
			registerCapability(t, pool, fCtl, spy, 100)
			intentID := insertRawDepositIntent(t, pool, fRace, "pending")
			attID := insertRawCreatedAttempt(t, pool, fRace.tenantID, intentID, false, time.Now())
			ctlAtt := insertRawCreatedAttempt(t, pool, fCtl.tenantID, insertRawDepositIntent(t, pool, fCtl, "pending"), false, time.Now())
			before := h8Snap(t, pool, fRace, intentID)
			fired := 0
			testHookAfterDispatchStatusPreRead = func(tenantID uuid.UUID) {
				if tenantID == fRace.tenantID {
					fired++
					setBrandStatus(t, pool, fRace.tenantID, fRace.brandID, status)
				}
			}
			t.Cleanup(func() { testHookAfterDispatchStatusPreRead = nil })

			s := newLoopSweeper(pool, orch, true, nil)
			if st := s.RunPass(context.Background(), nil, 0); st.Errors != 0 {
				t.Fatalf("pass: %+v", st)
			}
			if fired != 1 {
				t.Fatalf("test premise: hook must fire once, fired=%d", fired)
			}
			if d, _, _ := spy.counts(); d != 1 {
				t.Fatalf("only the control may reach the provider, got %d", d)
			}
			if mustGetAttempt(t, pool, fCtl.tenantID, ctlAtt).State == AttemptCreated {
				t.Fatal("control must dispatch")
			}
			h8Refused(t, pool, fRace, intentID, attID, before, 1)

			testHookAfterDispatchStatusPreRead = nil
			setBrandStatus(t, pool, fRace.tenantID, fRace.brandID, "active")
			dueNow(t, pool, fRace.tenantID, attID)
			if st := s.RunPass(context.Background(), nil, 1); st.Errors != 0 {
				t.Fatalf("pass 2: %+v", st)
			}
			if d, _, _ := spy.counts(); d != 2 {
				t.Fatalf("after reactivation the same attempt must dispatch exactly once, got %d", d)
			}
			if mustGetAttempt(t, pool, fRace.tenantID, attID).State == AttemptCreated {
				t.Fatal("reactivated brand's attempt must have been dispatched")
			}
		})
	}
}

// Repeated sweeper runs over a brand-refused child: still deferred, poll_count grows by the
// deferral only, and no audit/attempt/ledger row accumulates (bounded, like the B8 NotSent precedent).
func TestH8_RepeatedSweeps_BrandRefusedChild_NoChurnBeyondReschedule(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-h8rep")
	orch := spy.orchestrator()
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, spy, 100)
	intentID := insertRawDepositIntent(t, pool, f, "pending")
	attID := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
	setBrandStatus(t, pool, f.tenantID, f.brandID, "suspended")
	before := h8Snap(t, pool, f, intentID)
	s := newLoopSweeper(pool, orch, true, nil)
	const passes = 4
	for i := 0; i < passes; i++ {
		dueNow(t, pool, f.tenantID, attID)
		if st := s.RunPass(context.Background(), nil, i); st.Errors != 0 {
			t.Fatalf("pass %d: %+v", i, st)
		}
	}
	if d, _, _ := spy.counts(); d != 0 {
		t.Fatalf("no provider call may happen, got %d", d)
	}
	got := h8Refused(t, pool, f, intentID, attID, before, passes)
	if got.PollCount != passes {
		t.Fatalf("poll_count must grow by exactly one per deferral: want %d got %d", passes, got.PollCount)
	}
	if after := h8Snap(t, pool, f, intentID); after.auditRows != before.auditRows {
		t.Fatalf("repeated deferrals must write no audit rows: before=%d after=%d", before.auditRows, after.auditRows)
	}
}

// Concurrent claim vs brand suspension: the status change is in flight (uncommitted) while the
// sweeper reaches the claim gate; the FOR SHARE read waits, the change commits, the claim is
// deferred (nothing dispatched). Control tenant unaffected.
func TestH8_ClaimWaitsAtGateForInFlightBrandSuspension_ThenDeferred(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-h8conc")
	orch := spy.orchestrator()
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, spy, 100)
	intentID := insertRawDepositIntent(t, pool, f, "pending")
	attID := insertRawCreatedAttempt(t, pool, f.tenantID, intentID, false, time.Now())
	before := h8Snap(t, pool, f, intentID)

	release := holdStatusChange(t, pool, f, "brand")
	done := make(chan SweepPassStats, 1)
	go func() { done <- newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0) }()
	select {
	case st := <-done:
		t.Fatalf("the claim must wait at the brand gate for the in-flight status change, pass finished: %+v", st)
	case <-time.After(1500 * time.Millisecond):
	}
	if d, _, _ := spy.counts(); d != 0 {
		t.Fatalf("nothing may be dispatched while the suspension is in flight, got %d", d)
	}
	release()
	if st := <-done; st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	if d, _, _ := spy.counts(); d != 0 {
		t.Fatalf("committed suspension must defer the claim, got %d Deposit calls", d)
	}
	h8Refused(t, pool, f, intentID, attID, before, 1)
}

// Decision 19 (phase C): the brand is suspended during the provider call; the cascadable decline
// stands, NO child is created, a brand-specific audit row is written once (NOT the tenant one),
// nothing is cancelled/released. The control (active brand) gets its child.
func TestH8_PhaseC_CascadeChildNotCreatedForInactiveBrand(t *testing.T) {
	pool := depositV2ScratchPool(t)
	provNA, provAct, provB := newLoopProvider("mock-psp-h8c-na"), newLoopProvider("mock-psp-h8c-act"), newLoopProvider("mock-psp-h8c-b")
	provB.AcceptAllAmounts = true
	orch := NewOrchestrator(
		map[string]PaymentProvider{provNA.providerID: provNA, provAct.providerID: provAct, provB.providerID: provB},
		MultiWebhookCredentialResolver{
			provNA.providerID:  NewMockWebhookCredentials(provNA.MockProvider),
			provAct.providerID: NewMockWebhookCredentials(provAct.MockProvider),
			provB.providerID:   NewMockWebhookCredentials(provB.MockProvider),
		})
	fNA, fAct := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	var intents, atts [2]uuid.UUID
	for i, f := range []orchFixture{fNA, fAct} {
		first := PaymentProvider(provNA)
		if i == 1 {
			first = provAct
		}
		registerCapability(t, pool, f, first, 100)
		registerCapability(t, pool, f, provB, 200)
		intents[i] = insertRawDepositIntentAmount(t, pool, f, MockAmountProviderDeclineCascade)
		atts[i] = insertRawCreatedAttemptAmount(t, pool, f.tenantID, intents[i], MockAmountProviderDeclineCascade)
	}
	provNA.onDeposit = func(int) { setBrandStatus(t, pool, fNA.tenantID, fNA.brandID, "suspended") }

	if st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	if d, _, _ := provNA.counts(); d != 1 {
		t.Fatalf("test premise: first provider called once, got %d", d)
	}
	if got := mustGetAttempt(t, pool, fNA.tenantID, atts[0]); got.State != AttemptDeclined {
		t.Fatalf("the decline must stand, got %s", got.State)
	}
	if n := attemptsForIntent(t, pool, fNA.tenantID, intents[0]); n != 1 {
		t.Fatalf("an inactive brand must get no cascade child, got %d attempts", n)
	}
	au := auditActions(t, pool, fNA.tenantID)
	if au["payment.cascade_skipped_brand_inactive"] != 1 || au["payment.cascade_skipped_resolution_only"] != 0 {
		t.Fatalf("brand skip must be audited once under its own action, never the tenant one: %v", au)
	}
	if st := depScan[string](t, pool, fNA.tenantID, `SELECT status FROM deposit_intents WHERE id = $1`, intents[0]); st != "declined" {
		t.Fatalf("intent stays declined (decline stands, nothing cancelled/released), got %s", st)
	}
	if ledgerTxCount(t, pool, fNA.tenantID) != 0 {
		t.Fatal("no ledger transaction may result from a skipped child")
	}
	assertLedgerBalanced(t, pool, fNA.tenantID)
	if n := attemptsForIntent(t, pool, fAct.tenantID, intents[1]); n != 2 {
		t.Fatalf("control: active brand must get its child, got %d", n)
	}
	if n := auditActions(t, pool, fAct.tenantID)["payment.cascade_skipped_brand_inactive"]; n != 0 {
		t.Fatalf("control must have no brand-skip audit, got %d", n)
	}
	if d, _, _ := provB.counts(); d != 1 {
		t.Fatalf("only the control's child may reach the second provider, got %d", d)
	}
}

// Decision 19, poll-path twin (sweeper.go): a cascadable poll decline for a brand-inactive
// brand creates no child; the tenant audit action is not used.
func TestH8_PollPath_CascadeChildNotCreatedForInactiveBrand(t *testing.T) {
	pool := depositV2ScratchPool(t)
	provA, provB := newLoopProvider("mock-psp-h8p-a"), newLoopProvider("mock-psp-h8p-b")
	provB.AcceptAllAmounts = true
	orch := NewOrchestrator(
		map[string]PaymentProvider{provA.providerID: provA, provB.providerID: provB},
		MultiWebhookCredentialResolver{provA.providerID: NewMockWebhookCredentials(provA.MockProvider), provB.providerID: NewMockWebhookCredentials(provB.MockProvider)})
	fNA, fAct := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	var atts [2]PaymentAttempt
	for i, f := range []orchFixture{fNA, fAct} {
		registerCapability(t, pool, f, provA, 100)
		registerCapability(t, pool, f, provB, 200)
		a, ref := pendingDeposit(t, pool, orch, f, "h8-poll", 5000)
		provA.Resolve(ref, OutcomeDeclined, "provider_unavailable", true)
		atts[i] = a
	}
	setBrandStatus(t, pool, fNA.tenantID, fNA.brandID, "suspended")
	if st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	for i, f := range []orchFixture{fNA, fAct} {
		if got := mustGetAttempt(t, pool, f.tenantID, atts[i].ID); got.State != AttemptDeclined {
			t.Fatalf("%d: the poll decline must apply, got %s", i, got.State)
		}
	}
	if n := attemptsForIntent(t, pool, fNA.tenantID, *atts[0].DepositIntentID); n != 1 {
		t.Fatalf("inactive brand: no child, got %d attempts", n)
	}
	au := auditActions(t, pool, fNA.tenantID)
	if au["payment.cascade_skipped_brand_inactive"] != 1 || au["payment.cascade_skipped_resolution_only"] != 0 {
		t.Fatalf("poll path audit: %v", au)
	}
	if n := attemptsForIntent(t, pool, fAct.tenantID, *atts[1].DepositIntentID); n != 2 {
		t.Fatalf("control child expected, got %d", n)
	}
	assertLedgerBalanced(t, pool, fNA.tenantID)
}
