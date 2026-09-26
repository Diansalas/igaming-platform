//go:build integration

// Stage 9 (Production Readiness), migration 0082 section 1.2. Proves
// deposit_intents_enforce_immutable_fields rejects a direct SQL UPDATE to
// a protected column, AND - equally important - that it does NOT freeze
// the columns this orchestrator legitimately rewrites.
//
// deposit_intents is withdrawal_requests' mirror image and had no
// immutability trigger, while withdrawal_requests has had one since
// migration 0026. The mutable/immutable split here was derived from
// orchestrator.go's actual write paths rather than copied from
// sportsbook_bets, because copying would have been WRONG: provider_id and
// provider_reference must stay fully mutable, since handleDecline()
// cascades a cascadable decline to the next provider on the SAME intent
// row. The two tests below pin BOTH halves of that decision, so a future
// "tighten it like the others" change fails loudly instead of silently
// disabling payment failover.
package payments

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

func seedDepositIntent(t *testing.T, pool *db.Pool, f orchFixture, key string) uuid.UUID {
	t.Helper()
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: key,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed deposit intent: %v", err)
	}
	return intent.ID
}

func TestMigration0082_DepositIntentsImmutableFields(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	intentID := seedDepositIntent(t, pool, f, "mig0082-immutable")

	cases := []struct {
		name string
		set  string
		arg  any
	}{
		// The money. Raising `amount` after a deposit has been initiated
		// (and, worse, after it has posted) is the deposit-side twin of
		// the withdrawal four-eyes bypass migration 0026's own comment
		// describes.
		{"amount", `amount = 999999`, nil},
		{"asset_code", `asset_code = 'USD'`, nil},
		{"payment_method", `payment_method = 'crypto'`, nil},
		// Ownership: which player's wallet this deposit credits.
		{"tenant_id", `tenant_id = $2`, uuid.New()},
		{"brand_id", `brand_id = $2`, uuid.New()},
		{"player_account_id", `player_account_id = $2`, uuid.New()},
		{"wallet_id", `wallet_id = $2`, uuid.New()},
		// Idempotency: migration 0025's (tenant, player, idempotency_key)
		// unique constraint is the only thing stopping a replayed deposit
		// request creating a second intent. Editing the key frees the
		// original value for reuse.
		{"idempotency_key", `idempotency_key = 'hijacked'`, nil},
		{"created_at", `created_at = now() - interval '1 year'`, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
				sql := `UPDATE deposit_intents SET ` + tc.set + ` WHERE id = $1`
				if tc.arg != nil {
					_, err := tx.Exec(ctx, sql, intentID, tc.arg)
					return err
				}
				_, err := tx.Exec(ctx, sql, intentID)
				return err
			})
			if err == nil {
				t.Fatalf("expected deposit_intents_immutable_fields to reject %q, got nil error", tc.set)
			}
			if !strings.Contains(err.Error(), "immutable after insert") {
				t.Fatalf("expected the trigger's own immutability message, got: %v", err)
			}
		})
	}
}

// TestMigration0082_DepositIntentsProviderColumnsStayMutable is the
// guard on the guard. internal/payments cascades a cascadable decline to
// the NEXT provider on the same intent row (handleDecline ->
// attemptDeposit -> setIntentAttempt), so provider_id and
// provider_reference change from one real value to a DIFFERENT real value
// in ordinary, correct operation. Applying sportsbook_bets' "immutable
// once set" treatment here would have broken payment failover outright -
// this test is what stops that from being "tidied up" later.
func TestMigration0082_DepositIntentsProviderColumnsStayMutable(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	intentID := seedDepositIntent(t, pool, f, "mig0082-cascade")

	// Attempt 1, then a cascade to a different provider with a different
	// reference - exactly the shape setIntentAttempt writes.
	for _, attempt := range []struct{ providerID, ref string }{
		{"mock-psp", "ref-attempt-1"},
		{"mock-psp-secondary", "ref-attempt-2"},
	} {
		if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`UPDATE deposit_intents SET provider_id = $2, provider_reference = $3, status = 'pending', updated_at = now() WHERE id = $1`,
				intentID, attempt.providerID, attempt.ref)
			return err
		}); err != nil {
			t.Fatalf("expected a provider cascade to %s to remain permitted, got: %v", attempt.providerID, err)
		}
	}

	// status must also stay free to move to a terminal outcome.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deposit_intents SET status = 'declined', updated_at = now() WHERE id = $1`, intentID)
		return err
	}); err != nil {
		t.Fatalf("expected a status transition to remain permitted, got: %v", err)
	}
}

// TestMigration0082_DepositIntentsLedgerTransactionWriteOnce covers the
// one column that IS frozen once set. postDepositSuccess is the only
// statement in the codebase that writes it, and a deposit that has posted
// must never be repointed at a different ledger transaction - that would
// relabel which money this deposit was, with the ledger itself (correctly)
// unable to be edited to agree.
func TestMigration0082_DepositIntentsLedgerTransactionWriteOnce(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	intentID := seedDepositIntent(t, pool, f, "mig0082-ledger-link")

	// Two real ledger transactions to point at, so the test exercises the
	// trigger rather than an FK violation.
	txA := seedStandaloneLedgerTransaction(t, pool, f, "mig0082-link-a")
	txB := seedStandaloneLedgerTransaction(t, pool, f, "mig0082-link-b")

	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deposit_intents SET ledger_transaction_id = $2 WHERE id = $1`, intentID, txA)
		return err
	}); err != nil {
		t.Fatalf("expected the first ledger_transaction_id write to be permitted, got: %v", err)
	}

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deposit_intents SET ledger_transaction_id = $2 WHERE id = $1`, intentID, txB)
		return err
	})
	if err == nil {
		t.Fatal("expected the trigger to reject repointing an already-set ledger_transaction_id, got nil error")
	}
	if !strings.Contains(err.Error(), "immutable once set") {
		t.Fatalf("expected the trigger's own once-set message, got: %v", err)
	}

	// An idempotent replay writing the SAME id must still pass.
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE deposit_intents SET ledger_transaction_id = $2 WHERE id = $1`, intentID, txA)
		return err
	}); err != nil {
		t.Fatalf("expected an identical-value rewrite (idempotent replay) to be permitted, got: %v", err)
	}
}

// seedStandaloneLedgerTransaction inserts an entry-less ledger_transactions
// row purely as an FK target for the test above. It is never read as a
// financial fact and never balanced-checked (it has no entries), so it
// does not assert anything about the ledger itself.
func seedStandaloneLedgerTransaction(t *testing.T, pool *db.Pool, f orchFixture, key string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO ledger_transactions (id, tenant_id, transaction_type, idempotency_key, correlation_id)
			 VALUES ($1, $2, 'deposit', $3, $4)`,
			id, f.tenantID, key, uuid.New())
		return err
	})
	if err != nil {
		t.Fatalf("seed standalone ledger transaction: %v", err)
	}
	return id
}

func TestMigration0082_DepositIntentsDenyTruncate(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	seedDepositIntent(t, pool, f, "mig0082-truncate")

	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE deposit_intents`)
		return err
	})
	if err == nil {
		t.Fatal("expected deposit_intents_no_truncate to reject TRUNCATE, got nil error")
	}
	if !strings.Contains(err.Error(), "append-only") {
		t.Fatalf("expected ledger_deny_mutation's own append-only message, got: %v", err)
	}
}
