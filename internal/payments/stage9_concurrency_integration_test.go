//go:build integration

// Stage 9 §10 - adversarial concurrency re-verification for the deposit
// money path (ledger-finance). adversarial_test.go already proves the
// DUPLICATE cases under genuine concurrency (N concurrent InitiateDeposit
// calls sharing one idempotency key reach the provider once; N concurrent
// redeliveries of one success callback post once). What no suite covered
// is the opposite failure mode: two genuinely DISTINCT deposits confirmed
// for the SAME wallet at the same instant. Deduplication and
// non-lost-update are different properties - an implementation can be
// perfect at collapsing duplicates and still lose one of two distinct
// credits, and a read-modify-write balance would fail exactly here while
// passing every duplicate test in the package.
//
// The file also carries Stage 9 §12's one testable gap in the payment-
// readiness review (webhook auth-before-trust asserted at the ORCHESTRATOR
// seam, not just as a unit assertion on the adapter's HandleCallback) -
// see TestStage9_TamperedWebhookNeverReachesTheLedger.
package payments

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// s9SumDebitsCredits proves invariant #1 (SUM(DEBITS) == SUM(CREDITS))
// over this tenant's own entries. Tenant-scoped so parallel packages
// sharing one scratch database cannot make it flaky.
func s9SumDebitsCredits(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (debits, credits int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT
				COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
				COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
			 FROM ledger_entries WHERE tenant_id = $1`, tenantID).Scan(&debits, &credits)
	})
	if err != nil {
		t.Fatalf("sum debits/credits: %v", err)
	}
	return debits, credits
}

// s9CountDepositTransactions counts this wallet's posted deposit
// transactions straight from ledger_transactions - "exactly two distinct
// postings" asserted against the table, never inferred from a balance
// that happens to add up.
func s9CountDepositTransactions(t *testing.T, pool *db.Pool, f orchFixture) int {
	t.Helper()
	var count int
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT count(DISTINCT lt.id)
			   FROM ledger_transactions lt
			   JOIN ledger_entries le ON le.ledger_transaction_id = lt.id
			  WHERE lt.tenant_id = $1 AND lt.transaction_type = 'deposit' AND le.wallet_id = $2`,
			f.tenantID, f.walletID).Scan(&count)
	})
	if err != nil {
		t.Fatalf("count deposit transactions: %v", err)
	}
	return count
}

// s9CashProjectionRow reads the player_cash projection row's raw
// debit_total/credit_total. Asserted in addition to the derived balance
// because a lost update in the AFTER INSERT projection trigger and a lost
// LEDGER ENTRY are different bugs with the same symptom in the balance -
// comparing the projection against a recomputation from ledger_entries is
// the same drift check the hourly reconciliation job performs, applied
// here at the exact moment a race could have introduced the drift.
func s9CashProjectionVsRecomputed(t *testing.T, pool *db.Pool, f orchFixture) (projected, recomputed int64) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(credit_total - debit_total), 0)::bigint FROM wallet_balance_projection
			  WHERE wallet_id = $1 AND account_type = 'player_cash'`, f.walletID).Scan(&projected); err != nil {
			return err
		}
		return tx.QueryRow(ctx,
			`SELECT COALESCE(SUM(CASE WHEN le.direction = 'credit' THEN le.amount ELSE -le.amount END), 0)::bigint
			   FROM ledger_entries le
			   JOIN ledger_accounts la ON la.id = le.ledger_account_id
			  WHERE la.wallet_id = $1 AND la.account_type = 'player_cash'`, f.walletID).Scan(&recomputed)
	})
	if err != nil {
		t.Fatalf("read projection vs recomputed: %v", err)
	}
	return projected, recomputed
}

// TestStage9_ConcurrentDistinctDepositsSameWallet_NoLostUpdate confirms two
// DISTINCT deposits (distinct idempotency keys, distinct provider
// references, distinct amounts) for one wallet at the same instant, and
// requires BOTH to land: final player_cash must be the exact sum, two
// deposit transactions must exist, and the projection must still agree
// with a recomputation from ledger_entries.
//
// Distinct amounts (not two equal ones) are deliberate: with equal amounts
// a lost update and a correct single posting are indistinguishable from
// the balance alone, and 7_000 + 3_000 = 10_000 can only be reached by
// both postings landing.
func TestStage9_ConcurrentDistinctDepositsSameWallet_NoLostUpdate(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	amounts := []int64{7_000, 3_000}
	payloads := make([]InboundCallback, len(amounts))
	for i, amount := range amounts {
		var intent DepositIntent
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
				Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
				AssetCode: "EUR", Amount: amount, PaymentMethod: "card",
				IdempotencyKey: "s9-distinct-dep-" + uuid.NewString(),
			})
			return err
		})
		if err != nil {
			t.Fatalf("InitiateDeposit %d: %v", i, err)
		}
		if intent.ProviderReference == nil {
			t.Fatalf("intent %d has no provider reference", i)
		}
		payloads[i] = provider.CallbackPayload(f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, amount, "EUR", "", false)
	}

	errs := make([]error, len(payloads))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range payloads {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", payloads[i])
				return err
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("callback %d: two DISTINCT deposits must both post; got %v", i, err)
		}
	}

	const want = int64(10_000)
	if got := cashBalance(t, pool, f); got != want {
		t.Fatalf("expected player_cash %d after two simultaneous distinct deposits of %v, got %d "+
			"(a lower figure is a lost update - one credit was overwritten by the other)", want, amounts, got)
	}
	if got := s9CountDepositTransactions(t, pool, f); got != len(amounts) {
		t.Fatalf("expected %d distinct deposit ledger transactions, got %d", len(amounts), got)
	}
	projected, recomputed := s9CashProjectionVsRecomputed(t, pool, f)
	if projected != recomputed {
		t.Fatalf("projection drift introduced by the race: projection=%d recomputed-from-ledger=%d", projected, recomputed)
	}
	if projected != want {
		t.Fatalf("expected the player_cash projection to be %d, got %d", want, projected)
	}
	debits, credits := s9SumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

// --- Stage 9 §12: webhook auth-before-trust, at the ORCHESTRATOR seam ----

// TestStage9_TamperedWebhookNeverReachesTheLedger closes the §12
// "auth before trust" check at the layer that matters financially.
// mock_test.go already proves the ADAPTER refuses an unsigned or
// garbage-signed payload (HandleCallback -> ErrCallbackSignatureInvalid),
// but that is a unit assertion about one function's return value; nothing
// proved that a forged callback naming a REAL, pending, correctly-priced
// deposit intent produces no ledger effect when driven through
// Orchestrator.ReceiveCallback - which is the actual attack (an attacker
// who learns a provider reference, not one who posts random bytes).
//
// The three payloads below are the three realistic forgeries: no
// signature at all, a syntactically valid but wrong signature, and a
// correctly-signed payload whose AMOUNT was then edited (the case where
// signature verification must cover the effect-bearing fields, not just
// exist). Each must fail, and after all three the intent must still be
// pending with a zero balance - authentication strictly precedes any
// trust in the body.
func TestStage9_TamperedWebhookNeverReachesTheLedger(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = initiateDepositWithAttempt(ctx, tx, orch, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5_000, PaymentMethod: "card", IdempotencyKey: "s9-tampered-webhook",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.ProviderReference == nil {
		t.Fatal("intent has no provider reference")
	}

	genuine := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, 5_000, "EUR", "", false)

	unsigned := genuine
	unsigned.Header = genuine.Header.Clone()
	unsigned.Header.Del(HeaderSignature)

	wrongSig := genuine
	wrongSig.Header = genuine.Header.Clone()
	wrongSig.Header.Set(HeaderSignature, "v1="+strings.Repeat("a", 64))

	// A genuinely-signed callback whose amount was raised afterwards -
	// the signature is real, but no longer covers what the body now says.
	inflated := genuine
	inflated.Body = bytes.Replace(genuine.Body, []byte(`"amount":5000`), []byte(`"amount":500000`), 1)
	if bytes.Equal(inflated.Body, genuine.Body) {
		t.Fatal("test setup bug: tampering did not change the payload")
	}

	cases := map[string]struct {
		payload InboundCallback
		reason  CallbackAuthReason
	}{
		"unsigned":                {unsigned, ReasonSignatureMissing},
		"wrong_signature":         {wrongSig, ReasonSignatureInvalid},
		"amount_edited_after_sig": {inflated, ReasonSignatureInvalid},
	}
	for name, tc := range cases {
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", tc.payload)
			return err
		})
		var authErr *CallbackAuthError
		if !errors.As(err, &authErr) || authErr.Reason != tc.reason {
			t.Fatalf("%s: expected a *CallbackAuthError{Reason: %s} from ReceiveCallback, got %v", name, tc.reason, err)
		}
	}

	if got := cashBalance(t, pool, f); got != 0 {
		t.Fatalf("a forged webhook credited the wallet: player_cash=%d", got)
	}
	if got := s9CountDepositTransactions(t, pool, f); got != 0 {
		t.Fatalf("a forged webhook posted %d deposit ledger transactions", got)
	}
	var status DepositIntentStatus
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id = $1`, intent.ID).Scan(&status)
	}); err != nil {
		t.Fatalf("re-read intent: %v", err)
	}
	if status != intent.Status {
		t.Fatalf("a forged webhook mutated the intent status: %q -> %q", intent.Status, status)
	}

	// Control: the genuine payload, byte-for-byte, still posts - proving
	// the three refusals above were signature checks and not some
	// unrelated blanket rejection of this intent.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", genuine)
		return err
	}); err != nil {
		t.Fatalf("the genuine callback must post: %v", err)
	}
	if got := cashBalance(t, pool, f); got != 5_000 {
		t.Fatalf("expected player_cash 5000 after the genuine callback, got %d", got)
	}
	debits, credits := s9SumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}
