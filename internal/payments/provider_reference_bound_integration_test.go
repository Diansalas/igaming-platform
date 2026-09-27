//go:build integration

// PROVIDER-REF-BOUND-1 (PRH-REF): the platform provider-reference bound at
// the payments verified-callback boundary.
package payments

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providerref"
)

type paymentRowCounts struct {
	ledgerTx, entries, audit int
	intentStatus             string
}

func countPaymentRows(t *testing.T, pool *db.Pool, f orchFixture, intentID string) paymentRowCounts {
	t.Helper()
	var c paymentRowCounts
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, f.tenantID).Scan(&c.ledgerTx); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_entries WHERE tenant_id = $1`, f.tenantID).Scan(&c.entries); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, f.tenantID).Scan(&c.audit); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE id = $1`, intentID).Scan(&c.intentStatus)
	})
	if err != nil {
		t.Fatalf("count payment rows: %v", err)
	}
	return c
}

func paymentsLedgerBalanced(t *testing.T, pool *db.Pool, f orchFixture) {
	t.Helper()
	var debits, credits int64
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT COALESCE(SUM(amount) FILTER (WHERE direction = 'debit'), 0),
		                                COALESCE(SUM(amount) FILTER (WHERE direction = 'credit'), 0)
		                           FROM ledger_entries WHERE tenant_id = $1`, f.tenantID).Scan(&debits, &credits)
	})
	if err != nil || debits != credits {
		t.Fatalf("ledger unbalanced: debits=%d credits=%d (%v)", debits, credits, err)
	}
}

func TestProviderRefBound_Payments_OversizeRejectedNothingWritten_ExactMaxAccepted(t *testing.T) {
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
			AssetCode: "EUR", Amount: 8000, PaymentMethod: "card", IdempotencyKey: "dep-ref-bound",
		})
		return err
	})
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	depositRef := *intent.ProviderReference
	deliver := func(in InboundCallback) (ReceiveCallbackResult, error) {
		var res ReceiveCallbackResult
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			res, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-psp", in)
			return err
		})
		return res, err
	}

	over := strings.Repeat("p", providerref.MaxBytes+1)
	overMB := strings.Repeat("€", 86) // 258 bytes, 86 runes
	rejects := []struct {
		name, field string
		reason      providerref.Reason
		in          InboundCallback
	}{
		{"deposit ref too long", "provider_reference", providerref.ReasonTooLong,
			provider.CallbackPayload(f.tenantID, CallbackEventDeposit, over, "", OutcomeSucceeded, 8000, "EUR", "", false)},
		{"deposit ref multibyte too long", "provider_reference", providerref.ReasonTooLong,
			provider.CallbackPayload(f.tenantID, CallbackEventDeposit, overMB, "", OutcomeSucceeded, 8000, "EUR", "", false)},
		{"deposit ref control char", "provider_reference", providerref.ReasonControlChar,
			provider.CallbackPayload(f.tenantID, CallbackEventDeposit, "ref\u0085x", "", OutcomeSucceeded, 8000, "EUR", "", false)},
		{"deposit asset too long", "asset_code", providerref.ReasonTooLong,
			provider.CallbackPayload(f.tenantID, CallbackEventDeposit, depositRef, "", OutcomeSucceeded, 8000, over, "", false)},
		{"reversal own ref too long", "provider_reference", providerref.ReasonTooLong,
			provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, over, depositRef, OutcomeDeclined, 8000, "EUR", "chargeback", false)},
		{"reversal original too long (would tombstone)", "original_provider_reference", providerref.ReasonTooLong,
			provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, "cb-new", over, OutcomeDeclined, 8000, "EUR", "chargeback", false)},
	}
	for _, c := range rejects {
		t.Run(c.name, func(t *testing.T) {
			before := countPaymentRows(t, pool, f, intent.ID.String())
			for i := 0; i < 2; i++ { // deterministic on redelivery
				_, err := deliver(c.in)
				if !errors.Is(err, ErrProviderReferenceInvalid) || !errors.Is(err, providerref.ErrInvalid) {
					t.Fatalf("delivery %d: expected ErrProviderReferenceInvalid, got %v", i, err)
				}
				refErr, ok := providerref.AsError(err)
				if !ok || refErr.Field != c.field || refErr.Reason != c.reason {
					t.Fatalf("expected %s/%s, got %+v", c.field, c.reason, refErr)
				}
			}
			if after := countPaymentRows(t, pool, f, intent.ID.String()); after != before {
				t.Fatalf("rows written by a rejected callback: %+v -> %+v", before, after)
			}
			paymentsLedgerBalanced(t, pool, f)
		})
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("no rejected callback may move money, balance %d", balance)
	}

	// The real success, then an exact-max (255-byte) reversal reference:
	// accepted, posted once, idempotent on redelivery, stored verbatim.
	if _, err := deliver(provider.CallbackPayload(f.tenantID, CallbackEventDeposit, depositRef, "", OutcomeSucceeded, 8000, "EUR", "", false)); err != nil {
		t.Fatalf("deposit success: %v", err)
	}
	exact := strings.Repeat("c", 252) + "€"
	if len(exact) != providerref.MaxBytes {
		t.Fatalf("fixture length %d", len(exact))
	}
	rev := provider.CallbackPayload(f.tenantID, CallbackEventDepositReversal, exact, depositRef, OutcomeDeclined, 8000, "EUR", "chargeback", false)
	first, err := deliver(rev)
	if err != nil || first.LedgerTransactionID == nil {
		t.Fatalf("exact-max reversal must post: %+v %v", first, err)
	}
	afterFirst := countPaymentRows(t, pool, f, intent.ID.String())
	second, err := deliver(rev)
	if err != nil {
		t.Fatalf("redelivered exact-max reversal: %v", err)
	}
	if second.LedgerTransactionID == nil || *second.LedgerTransactionID != *first.LedgerTransactionID {
		t.Fatalf("redelivery must resolve to the same ledger transaction: %+v vs %+v", second, first)
	}
	if after := countPaymentRows(t, pool, f, intent.ID.String()); after.ledgerTx != afterFirst.ledgerTx || after.entries != afterFirst.entries {
		t.Fatalf("redelivery wrote ledger rows: %+v -> %+v", afterFirst, after)
	}
	var stored string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_tx_id FROM ledger_transactions WHERE id = $1`, *first.LedgerTransactionID).Scan(&stored)
	}); err != nil || stored != exact {
		t.Fatalf("exact-max reference not stored verbatim (%d bytes stored, %v)", len(stored), err)
	}
	if balance := cashBalance(t, pool, f); balance != 0 {
		t.Fatalf("deposit 8000 then full reversal: want 0, got %d", balance)
	}
	paymentsLedgerBalanced(t, pool, f)
}
