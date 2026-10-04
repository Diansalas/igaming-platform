//go:build integration

package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// success_for_never_sent_attempt (T15): success evidence for an attempt the
// platform never sent. The receipt resolves the 'created' attempt by its
// merchant reference (the attempt id), disputes it, and raises the P1.
func TestIWire_ReceiptT15_SuccessForNeverSentAttempt_RaisesP1(t *testing.T) {
	pool := testPool(t)
	e := newDepRefEnv(t, pool, "mock-iw-nsa")
	e.p.setScript(scriptOutcome(OutcomeDeclined, ""))
	res := rvInit(t, pool, e.orch, e.f, 5000, "iw-nsa") // first attempt declined (terminal), so a second may be inserted

	// A second attempt of the same intent, never sent ('created').
	var created PaymentAttempt
	if err := pool.WithTenant(context.Background(), e.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		created, err = InsertCreatedAttempt(ctx, tx, NewCreatedAttempt{
			ID: uuid.New(), TenantID: e.f.tenantID, Operation: AttemptOperationDeposit, DepositIntentID: &res.Intent.ID,
			AttemptNo: 2, ExcludedProviderIDs: []string{}, PaymentMethod: "card", AssetCode: "EUR", Amount: 5000,
		})
		if err != nil {
			return err
		}
		// Make the never-sent attempt resolvable within the verified provider
		// (INV-IO-14 resolves a merchant reference only for a provider-bound attempt).
		_, err = tx.Exec(ctx, `UPDATE payment_attempts SET provider_id = $2 WHERE id = $1`, created.ID, e.id)
		return err
	}); err != nil {
		t.Fatalf("setup: insert created attempt: %v", err)
	}

	d, err := iwApplyReceiptInTx(t, e, ReceiptEvidence{
		EventType: "deposit", ProviderReference: "iw-nsa-ref-" + uuid.NewString(), MerchantReference: created.MerchantReference,
		Outcome: OutcomeSucceeded, Amount: 5000, AssetCode: "EUR",
	})
	if err != nil {
		t.Fatalf("receipt: %v", err)
	}
	got := mustGetAttempt(t, pool, e.f.tenantID, created.ID)
	if got.State != AttemptDisputed || got.TerminalReason == nil || *got.TerminalReason != TerminalReasonSuccessForNeverSentAttempt {
		t.Fatalf("disposition=%s state=%s reason=%v: the T15 dispute must have been applied", d, got.State, got.TerminalReason)
	}
	// provider id is unknown for a never-sent attempt: the alert carries no provider attribute.
	rows := iwAlerts(t, e)
	want := "attempt:" + created.ID.String() + ":reason:" + TerminalReasonSuccessForNeverSentAttempt
	for _, r := range rows {
		if r.Discriminator == want {
			if r.Severity != "p1" {
				t.Fatalf("severity %s", r.Severity)
			}
			return
		}
	}
	t.Fatalf("no P1 with discriminator %q: %+v", want, rows)
}
