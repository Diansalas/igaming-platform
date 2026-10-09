//go:build integration

// H(8) fail-closed evidence for the brand read at its three payments call sites (sweeper claim tx,
// phase-C creation, poll-path creation), using the technique of
// internal/tenant TestBrandsGrantPin_GateFailsClosedWithoutUpdate_ScratchOnly: on a THROWAWAY scratch
// database only, the table OWNER revokes UPDATE on brands from igaming_runtime (an object ACL on the
// scratch database; no role, password or role attribute is touched), so the FOR SHARE brand read
// fails with 42501. The gate must then return an error, make no provider call, claim nothing and roll
// the decline transaction back, never read the failure as "active".
package payments

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
)

// h8RuntimeOnScratch connects the runtime role (igaming_runtime) to the scratch database behind owner.
func h8RuntimeOnScratch(t *testing.T, owner *db.Pool) *db.Pool {
	t.Helper()
	rtBase := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if rtBase == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping H(8) fail-closed scratch test")
	}
	var dbName string
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName)
	}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`GRANT CONNECT ON DATABASE ` + pgx.Identifier{dbName}.Sanitize() + ` TO igaming_runtime`,
		`GRANT USAGE ON SCHEMA public TO igaming_runtime`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO igaming_runtime`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO igaming_runtime`,
	} {
		if _, err := owner.Raw().Exec(context.Background(), stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	ru, err := url.Parse(rtBase)
	if err != nil {
		t.Fatal(err)
	}
	ru.Path = "/" + dbName
	rt, err := db.Connect(context.Background(), ru.String(), 10, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime on scratch: %v", err)
	}
	t.Cleanup(rt.Close)
	return rt
}

func h8RevokeBrandUpdate(t *testing.T, owner *db.Pool) {
	t.Helper()
	if _, err := owner.Raw().Exec(context.Background(), `REVOKE UPDATE ON public.brands FROM igaming_runtime`); err != nil {
		t.Fatalf("revoke on scratch: %v", err)
	}
	var has bool
	if err := owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT has_table_privilege('igaming_runtime','public.brands','UPDATE')`).Scan(&has)
	}); err != nil || has {
		t.Fatalf("revoke did not take effect: has=%v err=%v", has, err)
	}
}

// Sweeper claim tx: a brand read error is an error, not "active". Nothing is claimed or sent.
func TestH8_Scratch_ClaimTx_BrandReadError_FailsClosed(t *testing.T) {
	owner := depositV2ScratchPool(t)
	rt := h8RuntimeOnScratch(t, owner)
	spy := newLoopProvider("mock-psp-h8s-claim")
	orch := spy.orchestrator()
	ctl, f := seedOrchFixture(t, owner), seedOrchFixture(t, owner)
	registerCapability(t, owner, ctl, spy, 100)
	registerCapability(t, owner, f, spy, 100)
	ctlAtt := insertRawCreatedAttempt(t, owner, ctl.tenantID, insertRawDepositIntent(t, owner, ctl, "pending"), false, time.Now())
	s := newLoopSweeper(rt, orch, true, nil)

	// Control: with the intended grant the runtime-role sweeper dispatches.
	if st := s.RunPass(context.Background(), nil, 0); st.Errors != 0 {
		t.Fatalf("control pass: %+v", st)
	}
	if d, _, _ := spy.counts(); d != 1 || mustGetAttempt(t, owner, ctl.tenantID, ctlAtt).State == AttemptCreated {
		t.Fatalf("control must dispatch once: deposits=%d", d)
	}

	intentID := insertRawDepositIntent(t, owner, f, "pending")
	attID := insertRawCreatedAttempt(t, owner, f.tenantID, intentID, false, time.Now())
	h8RevokeBrandUpdate(t, owner)
	before := h8Snap(t, owner, f, intentID)
	st := s.RunPass(context.Background(), nil, 1)
	if st.Errors == 0 {
		t.Fatalf("a brand read error must surface as a pass error, got %+v", st)
	}
	if d, _, _ := spy.counts(); d != 1 {
		t.Fatalf("no provider call may follow a failed brand read, got %d total", d)
	}
	got := mustGetAttempt(t, owner, f.tenantID, attID)
	if got.State != AttemptCreated || got.ProviderID != nil || got.SubmitCount != 0 || got.ClaimToken != nil {
		t.Fatalf("attempt must be unclaimed and unsent: state=%s submits=%d", got.State, got.SubmitCount)
	}
	if after := h8Snap(t, owner, f, intentID); after.intentStatus != before.intentStatus || after.attempts != before.attempts || after.ledgerTx != before.ledgerTx {
		t.Fatalf("rows changed: before=%+v after=%+v", before, after)
	}
}

// Phase C (creation): the brand read fails AFTER the claim, during the provider call. The decline
// transaction is rolled back (no decline applied, no child, no skip audit), nothing reaches provider B.
func TestH8_Scratch_PhaseC_BrandReadError_RollsBackDecline(t *testing.T) {
	owner := depositV2ScratchPool(t)
	rt := h8RuntimeOnScratch(t, owner)
	provA, provB := newLoopProvider("mock-psp-h8s-pc-a"), newLoopProvider("mock-psp-h8s-pc-b")
	provB.AcceptAllAmounts = true
	orch := NewOrchestrator(
		map[string]PaymentProvider{provA.providerID: provA, provB.providerID: provB},
		MultiWebhookCredentialResolver{provA.providerID: NewMockWebhookCredentials(provA.MockProvider), provB.providerID: NewMockWebhookCredentials(provB.MockProvider)}).WithPayoutDestinations(pitest.Shared())
	f := seedOrchFixture(t, owner)
	registerCapability(t, owner, f, provA, 100)
	registerCapability(t, owner, f, provB, 200)
	intentID := insertRawDepositIntentAmount(t, owner, f, MockAmountProviderDeclineCascade)
	attID := insertRawCreatedAttemptAmount(t, owner, f.tenantID, intentID, MockAmountProviderDeclineCascade)
	provA.onDeposit = func(int) { h8RevokeBrandUpdate(t, owner) }
	before := h8Snap(t, owner, f, intentID)

	st := newLoopSweeper(rt, orch, true, nil).RunPass(context.Background(), nil, 0)
	if st.Errors == 0 {
		t.Fatalf("a brand read error in phase C must surface as a pass error, got %+v", st)
	}
	if d, _, _ := provA.counts(); d != 1 {
		t.Fatalf("test premise: provider A called once (revoke lands mid-call), got %d", d)
	}
	if d, _, _ := provB.counts(); d != 0 {
		t.Fatalf("no cascade child may reach provider B, got %d", d)
	}
	if n := attemptsForIntent(t, owner, f.tenantID, intentID); n != 1 {
		t.Fatalf("no child may be created, got %d attempts", n)
	}
	if got := mustGetAttempt(t, owner, f.tenantID, attID); got.State == AttemptDeclined {
		t.Fatal("the decline transaction must have rolled back (attempt must not be declined)")
	}
	after := h8Snap(t, owner, f, intentID)
	if after.intentStatus != before.intentStatus || after.ledgerTx != before.ledgerTx {
		t.Fatalf("intent/ledger changed: before=%+v after=%+v", before, after)
	}
	au := auditActions(t, owner, f.tenantID)
	if au["payment.cascade_skipped_brand_inactive"] != 0 || au["payment.cascade_skipped_resolution_only"] != 0 {
		t.Fatalf("no skip audit may be committed by a rolled-back tx: %v", au)
	}
}

// Poll path (creation): the same, for a cascadable decline found by QueryStatus.
func TestH8_Scratch_PollPath_BrandReadError_RollsBackDecline(t *testing.T) {
	owner := depositV2ScratchPool(t)
	rt := h8RuntimeOnScratch(t, owner)
	provA, provB := newLoopProvider("mock-psp-h8s-pp-a"), newLoopProvider("mock-psp-h8s-pp-b")
	provB.AcceptAllAmounts = true
	orch := NewOrchestrator(
		map[string]PaymentProvider{provA.providerID: provA, provB.providerID: provB},
		MultiWebhookCredentialResolver{provA.providerID: NewMockWebhookCredentials(provA.MockProvider), provB.providerID: NewMockWebhookCredentials(provB.MockProvider)}).WithPayoutDestinations(pitest.Shared())
	f := seedOrchFixture(t, owner)
	registerCapability(t, owner, f, provA, 100)
	registerCapability(t, owner, f, provB, 200)
	a, ref := pendingDeposit(t, owner, orch, f, "h8s-poll", 5000)
	provA.Resolve(ref, OutcomeDeclined, "provider_unavailable", true)
	intentID := *a.DepositIntentID
	h8RevokeBrandUpdate(t, owner)
	before := h8Snap(t, owner, f, intentID)

	st := newLoopSweeper(rt, orch, true, nil).RunPass(context.Background(), nil, 0)
	if st.Errors == 0 {
		t.Fatalf("a brand read error on the poll path must surface as a pass error, got %+v", st)
	}
	if d, _, _ := provB.counts(); d != 0 {
		t.Fatalf("no cascade child may reach provider B, got %d", d)
	}
	if got := mustGetAttempt(t, owner, f.tenantID, a.ID); got.State != AttemptPending {
		t.Fatalf("the decline tx must roll back: attempt must remain pending, got %s", got.State)
	}
	after := h8Snap(t, owner, f, intentID)
	if after != before {
		t.Fatalf("rows changed by a rolled-back tx: before=%+v after=%+v", before, after)
	}
	_ = uuid.Nil
}
