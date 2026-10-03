//go:build integration

package payments

import (
	"testing"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/testsupport/alertinject"
)

// driveCreatedAttempt (cascade, drive.go) owns its phase C transaction through
// alerting.InTx too: a cascade-driven T10 park with an in-tx raise failure
// commits the park and the post-commit Flush persists the P1.
func TestIWire_CascadeDrivenPark_InjectedRaiseFailure_ParkCommits_DetachedP1Persists(t *testing.T) {
	pool := testPool(t)
	ma := NewMockProvider("mock-iw-cas-a", "EUR")
	mb := NewMockProvider("mock-iw-cas-b", "EUR")
	pa := &depRefProvider{MockProvider: ma}
	pb := &depRefProvider{MockProvider: mb}
	f := seedOrchFixture(t, pool)
	registerCapability(t, pool, f, pa, 100)
	registerCapability(t, pool, f, pb, 200)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-iw-cas-a": pa, "mock-iw-cas-b": pb},
		MultiWebhookCredentialResolver{"mock-iw-cas-a": NewMockWebhookCredentials(ma), "mock-iw-cas-b": NewMockWebhookCredentials(mb)})
	const boundRef = "iw-cascade-bound-ref"
	seedPayoutAttemptBoundTo(t, pool, f, "mock-iw-cas-b", boundRef)
	pa.setScript(func(req DepositRequest) DepositResult {
		return DepositResult{Outcome: OutcomeDeclined, DeclineReason: "provider_unavailable", Cascadable: true, Amount: req.Amount, AssetCode: req.AssetCode}
	})
	pb.setScript(scriptOutcome(OutcomePending, boundRef))
	alertinject.Install(t, pool, f.tenantID, alertinject.InTxOnly, "P0001")

	res := rvInit(t, pool, orch, f, 5000, "iw-cas")
	if res.Attempt.AttemptNo != 2 {
		t.Fatalf("expected the cascade child to be the driven attempt, got attempt_no %d", res.Attempt.AttemptNo)
	}
	a := mustGetAttempt(t, pool, f.tenantID, res.Attempt.ID)
	if a.State != AttemptDisputed || depTerminalReason(a) != TerminalReasonProviderReferenceConflict {
		t.Fatalf("child state=%s reason=%s", a.State, depTerminalReason(a))
	}
	var found bool
	for _, r := range alertinject.ForSubject(t, pool, f.tenantID) {
		if r.Kind == string(alerting.KindPaymentWebhookIntegrity) && r.Discriminator == "attempt:"+a.ID.String()+":reason:"+TerminalReasonProviderReferenceConflict {
			found = true
		}
	}
	if !found {
		t.Fatal("the post-commit Flush of driveCreatedAttempt must persist the cascade park's P1")
	}
}
