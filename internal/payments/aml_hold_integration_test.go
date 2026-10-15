//go:build integration

package payments

import (
	"context"
	"os"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/payoutinstrument/pitest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
	"github.com/Diansalas/igaming-platform/internal/withdrawal"
)

// ADR 0116 s5.1 at the payments gates: a held player's DEPOSIT initiation and PAYOUT claim are denied with the SHARED public
// result (account_review, never a kyc_required: reason); a deposit SETTLEMENT callback, and an in-flight payout's completion,
// are never gated and never recalled.

func amlProofs(t *testing.T) {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	prooftest.Install(t, u)
}

func personOf(t *testing.T, pool *db.Pool, f orchFixture) uuid.UUID {
	t.Helper()
	var p uuid.UUID
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, f.playerAccountID).Scan(&p)
	}); err != nil {
		t.Fatal(err)
	}
	return p
}

func holdAllFor(t *testing.T, pool *db.Pool, f orchFixture, scope aml.HoldScope) (*amlfix.Quorum, uuid.UUID) {
	t.Helper()
	amlProofs(t)
	return amlfix.HoldFor(t, launchfix.OwnerFor(t, pool), pool, f.tenantID, personOf(t, pool, f), nil, scope)
}

type depositAudit struct{ Reason, Declined string }

func depositAuditOf(t *testing.T, pool *db.Pool, f orchFixture, intent uuid.UUID) depositAudit {
	t.Helper()
	var a depositAudit
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT metadata->>'reason_code' FROM audit_log WHERE tenant_id = $1 AND action = 'payments.deposit_denied_by_kyc'
			AND metadata->>'deposit_intent_id' = $2`, f.tenantID, intent.String()).Scan(&a.Reason); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT metadata->>'decline_reason' FROM audit_log WHERE tenant_id = $1 AND action = 'deposit.declined' AND target_id = $2`,
			f.tenantID, intent.String()).Scan(&a.Declined)
	}); err != nil {
		t.Fatalf("deposit audit: %v", err)
	}
	return a
}

// C4 at the deposit gate: a held deposit and a KYC-pending deposit decline with the SAME reason and the same audit rows; the
// held one never says kyc_required.
func TestAMLHold_Deposit_HeldAndKYCPending_AreIndistinguishable(t *testing.T) {
	pool := testPool(t)
	run := func(name string, arrange func(f orchFixture)) (DepositIntent, depositAudit, int) {
		f := seedOrchFixture(t, pool)
		provider := NewMockProvider("mock-aml-"+name, "EUR")
		registerCapability(t, pool, f, provider, 100)
		orch := NewOrchestrator(map[string]PaymentProvider{"mock-aml-" + name: provider}, MultiWebhookCredentialResolver{"mock-aml-" + name: NewMockWebhookCredentials(provider)})
		arrange(f)
		res, err := orch.InitiateDepositAttempt(context.Background(), pool, KYCEnforcementDepositGate{}, MockCredentialResolver{}, InitiateDepositParams{MarketGuard: testUngatedGuard,
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "aml-" + name,
		})
		if err != nil {
			t.Fatalf("%s: InitiateDepositAttempt: %v", name, err)
		}
		if res.AttemptCreated || res.Intent.Status != DepositIntentDeclined || provider.AttemptCount() != 0 {
			t.Fatalf("%s: a deny commits a declined intent with no attempt and no provider call: %+v", name, res)
		}
		return res.Intent, depositAuditOf(t, pool, f, res.Intent.ID), provider.AttemptCount()
	}

	_, pending, _ := run("pending", func(f orchFixture) {
		j := seedActiveDepositEnforcementPolicy(t, pool, "1", "EUR")
		bindTenantLicence(t, pool, f.tenantID, j)
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
				VALUES ($1, $2, $3, $4, $5, 'pending', 'mock')`, uuid.New(), f.tenantID, f.brandID, f.playerAccountID, personOf(t, pool, f))
			return err
		}); err != nil {
			t.Fatal(err)
		}
	})
	_, held, _ := run("held", func(f orchFixture) { holdAllFor(t, pool, f, aml.ScopeAllValueMovement) })

	want := depositAudit{Reason: "account_review", Declined: "account_review"}
	if pending != want || held != want {
		t.Fatalf("both denies must carry the shared reason and never kyc_required: pending=%+v held=%+v want=%+v", pending, held, want)
	}
}

func TestDepositKYCDeclineReason_AccountReviewIsNeverPrefixed(t *testing.T) {
	if got := depositKYCDeclineReason("account_review"); got != "account_review" {
		t.Fatalf("account_review must pass through unprefixed, got %q", got)
	}
	// Only the exact shared code is exempt.
	for _, d := range []string{"account_review2", "x_account_review", "Account_Review", ""} {
		if got := depositKYCDeclineReason(d); got != "kyc_required:"+d {
			t.Errorf("%q must keep the kyc_required: prefix, got %q", d, got)
		}
	}
}

// LF: a deposit SETTLEMENT callback is never gated: a deposit started before the hold still credits the wallet.
func TestAMLHold_DepositSettlementCallback_IsNeverGated(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-aml-settle", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-aml-settle": provider}, MultiWebhookCredentialResolver{"mock-aml-settle": NewMockWebhookCredentials(provider)})
	res, err := orch.InitiateDepositAttempt(context.Background(), pool, AllowAllDepositKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{MarketGuard: testUngatedGuard,
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "aml-settle",
	})
	if err != nil || !res.AttemptCreated {
		t.Fatalf("setup deposit: %+v %v", res, err)
	}
	holdAllFor(t, pool, f, aml.ScopeAllValueMovement)
	before := cashBalance(t, pool, f)
	ref := *res.Attempt.ProviderReference
	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, ref, "", OutcomeSucceeded, 5000, "EUR", "", false)
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-aml-settle", payload)
		return err
	}); err != nil {
		t.Fatalf("a settlement callback must not be gated by a hold: %v", err)
	}
	if got := cashBalance(t, pool, f); got-before != 5000 {
		t.Fatalf("the settled deposit credits the wallet under a hold: %d -> %d", before, got)
	}
}

// LF + C4 at the payout claim: a held payout claim goes through DenyForCompliance (terminal rejection, funds back to
// player_cash, money-neutral); the request is never left half-claimed.
func TestAMLHold_PayoutClaim_Held_RejectsAndReturnsFunds(t *testing.T) {
	pool := testPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-aml-payout", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-aml-payout": provider}, MultiWebhookCredentialResolver{"mock-aml-payout": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, 2_000, "aml-payout-held")
	cashAfterHold := cashBalance(t, pool, f.orchFixture)
	holdAllFor(t, pool, f.orchFixture, aml.ScopeWithdrawalAndPayout)

	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil {
		t.Fatalf("a held claim is a committed deny, not an error: %v", err)
	}
	if !claim.Denied {
		t.Fatalf("a held payout claim must be denied: %+v", claim)
	}
	got := mustGetWithdrawal(t, pool, f.tenantID, wr.ID)
	if got.State != withdrawal.StateRejected {
		t.Fatalf("the request must be terminally rejected, got %s", got.State)
	}
	if after := cashBalance(t, pool, f.orchFixture); after-cashAfterHold != 2_000 {
		t.Fatalf("the held funds return to player_cash: %d -> %d", cashAfterHold, after)
	}
	if provider.AttemptCount() != 0 || countAttempts(t, pool, f.tenantID, wr.ID) != 0 {
		t.Fatal("a held claim makes no attempt and no provider call")
	}
}

// LF: an in-flight payout (claimed BEFORE the hold) is never recalled: its completion posts normally under the hold.
func TestAMLHold_InFlightPayout_NotRecalled(t *testing.T) {
	pool := testPool(t)
	f := seedPayoutFixture(t, pool, 100_000, true)
	provider := NewMockProvider("mock-aml-inflight", "EUR")
	registerCapability(t, pool, f.orchFixture, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-aml-inflight": provider}, MultiWebhookCredentialResolver{"mock-aml-inflight": NewMockWebhookCredentials(provider)}).WithPayoutDestinations(pitest.Shared())

	wr := approvedWithdrawal(t, pool, f, MockAmountAmbiguous, "aml-payout-inflight")
	claim, err := orch.ClaimForDispatch(context.Background(), pool, KYCEnforcementPayoutGate{}, f.tenantID, wr.ID, "bank_transfer", testSubmitActor())
	if err != nil || claim.Denied {
		t.Fatalf("pre-hold claim: %+v %v", claim, err)
	}
	gr := DispatchWithdraw(context.Background(), pool, MockCredentialResolver{}, provider, claim.Attempt, WithDestinations(pitest.Shared()))
	if err := ApplyPayoutResult(context.Background(), pool, f.tenantID, wr.ID, claim.Attempt, gr, EvidenceSync, WithDestinations(pitest.Shared())); err != nil {
		t.Fatalf("ApplyPayoutResult: %v", err)
	}
	att := mustGetAttempt(t, pool, f.tenantID, claim.Attempt.ID)
	if att.ProviderReference == nil {
		t.Fatalf("setup: expected a provider reference, state=%s", att.State)
	}
	holdAllFor(t, pool, f.orchFixture, aml.ScopeAllValueMovement)

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := ApplyReceiptEvidence(ctx, tx, orch, f.tenantID, "mock-aml-inflight", ReceiptEvidence{
			EventType: "payout", ProviderReference: *att.ProviderReference, Outcome: OutcomeSucceeded, Amount: MockAmountAmbiguous, AssetCode: "EUR",
		})
		return err
	}); err != nil {
		t.Fatalf("a payout completion must not be gated: %v", err)
	}
	if got := mustGetWithdrawal(t, pool, f.tenantID, wr.ID); got.State != withdrawal.StateCompleted {
		t.Fatalf("an in-flight payout completes normally under a hold, got %s", got.State)
	}
}
