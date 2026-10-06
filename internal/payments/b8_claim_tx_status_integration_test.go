//go:build integration

// B8 (PAY-H-FOLLOWUPS-1 (1), security H-SEC-1, HD-CTF-10): the tenant-status read that
// gates NEW deposit dispatch must be inside the T2 claim transaction, and the phase-C
// cascade insert must be gated like the poll-path twin. These tests suspend the tenant in
// exactly the gap the old pre-read left open and prove nothing leaves the building.
package payments

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// insertRawDepositIntentAmount / insertRawCreatedAttemptAmount: like the shared raw helpers
// but with a chosen amount (the mock keys its scripted outcomes off the amount; the attempt
// amount is immutable after insert).
func insertRawDepositIntentAmount(t *testing.T, pool *db.Pool, f orchFixture, amount int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, idempotency_key, status)
			 VALUES ($1,$2,$3,$4,$5,'EUR',$6,'card',$7,'pending')`,
			id, f.tenantID, f.brandID, f.playerAccountID, f.walletID, amount, "raw-"+id.String())
		return err
	}); err != nil {
		t.Fatalf("insert raw deposit intent: %v", err)
	}
	return id
}

func insertRawCreatedAttemptAmount(t *testing.T, pool *db.Pool, tenantID, intentID uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO payment_attempts (id, tenant_id, operation, deposit_intent_id, attempt_no, payment_method, asset_code, amount, interactive, merchant_reference, external_idempotency_key, state, last_evidence_kind, created_at, next_action_at)
			 VALUES ($1,$2,'deposit',$3,1,'card','EUR',$4,false,$5,$6,'created','platform',$7,now())`,
			id, tenantID, intentID, amount, id.String(), "pa:"+id.String(), time.Now())
		return err
	}); err != nil {
		t.Fatalf("insert raw created attempt: %v", err)
	}
	return id
}

// The pre-read (deferIfResolutionOnly) sees an ACTIVE tenant; a suspension commits in the
// gap before driveCreatedAttempt. The claim transaction's own read must stop the dispatch:
// no claim, no adapter call, the attempt stays `created` and is rescheduled; reactivation
// resumes it. A control tenant in the same pass dispatches normally.
func TestB8_SuspensionAfterPreReadBeforeClaim_NoClaimNoDeposit_ThenResumes(t *testing.T) {
	for _, status := range []string{"suspended", "closed"} {
		t.Run(status, func(t *testing.T) {
			pool := depositV2ScratchPool(t)
			spy := newLoopProvider("mock-psp-b8a-" + status)
			orch := spy.orchestrator()
			fRace, fCtl := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
			var ids [2]uuid.UUID
			for i, f := range []orchFixture{fRace, fCtl} {
				registerCapability(t, pool, f, spy, 100)
				ids[i] = insertRawCreatedAttempt(t, pool, f.tenantID, insertRawDepositIntent(t, pool, f, "pending"), false, time.Now())
			}
			raceTenant := fRace.tenantID
			fired := 0
			testHookAfterDispatchStatusPreRead = func(tenantID uuid.UUID) {
				if tenantID == raceTenant {
					fired++
					setTenantStatus(t, pool, raceTenant, status)
				}
			}
			t.Cleanup(func() { testHookAfterDispatchStatusPreRead = nil })

			s := newLoopSweeper(pool, orch, true, nil)
			if st := s.RunPass(context.Background(), nil, 0); st.Errors != 0 {
				t.Fatalf("pass: %+v", st)
			}
			if fired != 1 {
				t.Fatalf("test premise: the interleaving hook must fire once for the racing tenant, fired=%d", fired)
			}
			if d, _, _ := spy.counts(); d != 1 {
				t.Fatalf("only the control tenant may reach the adapter, got %d Deposit calls", d)
			}
			a := mustGetAttempt(t, pool, raceTenant, ids[0])
			if a.State != AttemptCreated || a.ProviderID != nil || a.SubmitCount != 0 || a.ClaimToken != nil {
				t.Fatalf("raced attempt must be unclaimed and untouched: state=%s provider=%v submits=%d", a.State, a.ProviderID, a.SubmitCount)
			}
			assertDeferred(t, pool, raceTenant, ids[0], 0, 1)
			if c := mustGetAttempt(t, pool, fCtl.tenantID, ids[1]); c.State == AttemptCreated {
				t.Fatal("control: the active tenant's attempt must have been dispatched")
			}
			assertLedgerBalanced(t, pool, raceTenant)

			// Reactivation resumes the identical attempt.
			testHookAfterDispatchStatusPreRead = nil
			setTenantStatus(t, pool, raceTenant, "active")
			dueNow(t, pool, raceTenant, ids[0])
			if st := s.RunPass(context.Background(), nil, 1); st.Errors != 0 {
				t.Fatalf("pass 2: %+v", st)
			}
			if d, _, _ := spy.counts(); d != 2 {
				t.Fatalf("after reactivation the attempt must dispatch exactly once more, got %d Deposit calls", d)
			}
		})
	}
}

// Tenant isolation: another tenant's suspension never defers this tenant's claim, and an
// active tenant dispatches normally (covered by the control above); here the racing tenant
// being non-active must leave the OTHER tenant's created attempt claimable in the same pass.
func TestB8_OtherTenantSuspension_DoesNotDeferThisTenant(t *testing.T) {
	pool := depositV2ScratchPool(t)
	spy := newLoopProvider("mock-psp-b8b")
	orch := spy.orchestrator()
	fSusp, fAct := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	var actID uuid.UUID
	for i, f := range []orchFixture{fSusp, fAct} {
		registerCapability(t, pool, f, spy, 100)
		id := insertRawCreatedAttempt(t, pool, f.tenantID, insertRawDepositIntent(t, pool, f, "pending"), false, time.Now())
		if i == 1 {
			actID = id
		}
	}
	setTenantStatus(t, pool, fSusp.tenantID, "suspended")
	if st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	if got := mustGetAttempt(t, pool, fAct.tenantID, actID); got.State == AttemptCreated {
		t.Fatal("the active tenant's attempt must dispatch although another tenant is suspended")
	}
	if d, _, _ := spy.counts(); d != 1 {
		t.Fatalf("exactly one Deposit (the active tenant's), got %d", d)
	}
}

// Phase C cascade gate (drive.go twin of sweeper.go's poll-path gate): the tenant is suspended
// DURING the provider call; the cascadable decline is final but NO cascade child is inserted,
// and the skip is audited once (H-CR-6). The control (active) tenant gets its child and the
// second provider is reached.
func TestB8_CascadeChildNotInsertedWhenSuspendedDuringProviderCall(t *testing.T) {
	pool := depositV2ScratchPool(t)
	// One first-choice provider per tenant, so the mid-call suspension hook belongs to the
	// racing tenant's own Deposit call only; both fall back to the shared second provider.
	provNA, provAct, provB := newLoopProvider("mock-psp-b8c-na"), newLoopProvider("mock-psp-b8c-act"), newLoopProvider("mock-psp-b8c-b")
	provB.AcceptAllAmounts = true
	orch := NewOrchestrator(
		map[string]PaymentProvider{provNA.providerID: provNA, provAct.providerID: provAct, provB.providerID: provB},
		MultiWebhookCredentialResolver{
			provNA.providerID:  NewMockWebhookCredentials(provNA.MockProvider),
			provAct.providerID: NewMockWebhookCredentials(provAct.MockProvider),
			provB.providerID:   NewMockWebhookCredentials(provB.MockProvider),
		})
	fNA, fAct := seedOrchFixture(t, pool), seedOrchFixture(t, pool)
	var intents [2]uuid.UUID
	var atts [2]uuid.UUID
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
	naTenant := fNA.tenantID
	provNA.onDeposit = func(int) { setTenantStatus(t, pool, naTenant, "suspended") }

	if st := newLoopSweeper(pool, orch, true, nil).RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("pass: %+v", st)
	}
	if d, _, _ := provNA.counts(); d != 1 {
		t.Fatalf("test premise: the racing tenant's first provider must have been called once (suspension lands mid-call), got %d", d)
	}
	if got := mustGetAttempt(t, pool, fNA.tenantID, atts[0]); got.State != AttemptDeclined {
		t.Fatalf("the decline must stand as final, got %s", got.State)
	}
	if n := attemptsForIntent(t, pool, fNA.tenantID, intents[0]); n != 1 {
		t.Fatalf("a tenant suspended mid-call must get no cascade child, got %d attempts", n)
	}
	if n := auditActions(t, pool, fNA.tenantID)["payment.cascade_skipped_resolution_only"]; n != 1 {
		t.Fatalf("the skipped cascade must be audited once, got %d", n)
	}
	if n := attemptsForIntent(t, pool, fAct.tenantID, intents[1]); n != 2 {
		t.Fatalf("control: the active tenant must get its cascade child, got %d attempts", n)
	}
	if n := auditActions(t, pool, fAct.tenantID)["payment.cascade_skipped_resolution_only"]; n != 0 {
		t.Fatalf("an active tenant must not get that audit, got %d", n)
	}
	if d, _, _ := provB.counts(); d != 1 {
		t.Fatalf("only the active tenant's cascade child may reach the shared second provider, got %d", d)
	}
	// LF B8-T1: the intent is terminal-declined and no created sibling is left to resume.
	if st := depScan[string](t, pool, fNA.tenantID, `SELECT status FROM deposit_intents WHERE id = $1`, intents[0]); st != "declined" {
		t.Fatalf("the suspended tenant's intent must be declined (terminal), got %s", st)
	}
	if n := depScan[int64](t, pool, fNA.tenantID, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1 AND state = 'created'`, intents[0]); n != 0 {
		t.Fatalf("no created sibling may be left behind, got %d", n)
	}
	assertLedgerBalanced(t, pool, fNA.tenantID)
}
