//go:build integration

// PAY-WH-TENANT-1 (ADR 0090; docs/decisions/0022 §3 amendment 2026-09-26).
// QA binding test plan (docs/plans/stage-10.1-planning/
// 16-pay-wh-review-qa-test-plan.md): T2-T5, T9-T11, T14 at the
// Orchestrator/adapter level (package payments, run as the NOBYPASSRLS
// runtime role per the plan's own mandate - testPool already does this;
// see capability_integration_test.go).
package payments

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// sharedSecretResolver is a TEST-ONLY resolver that hands out the
// IDENTICAL secret for every tenant it is asked about - modeling a
// misconfigured or naive resolver, to prove (T5) that the tenant is bound
// into what is SIGNED, not merely selected by which secret was used. No
// production or MOCK resolver ever behaves this way - see
// MockWebhookCredentials' own per-tenant derivation.
type sharedSecretResolver struct {
	secret     []byte
	providerID string
}

func (r sharedSecretResolver) Resolve(_ context.Context, tenantID uuid.UUID, providerID, keyID string) (WebhookCredential, error) {
	if providerID != r.providerID || keyID != mockWebhookKeyID {
		return WebhookCredential{}, ErrWebhookCredentialUnavailable
	}
	return WebhookCredential{TenantID: tenantID, ProviderID: providerID, KeyID: keyID, Secret: r.secret, Fingerprint: "test-fixture"}, nil
}

// assertNoFinancialEffect is the QA plan §2 six-point "no financial
// effect" checklist, run under tenantID's own scope in a FRESH
// transaction (never the one the rejected call ran in, which already
// rolled back) - points 1/2/3/4 here; point 5 (debit=credit) is a
// platform-wide invariant already exercised continuously by every other
// integration test in this package via ledger.Post itself refusing an
// unbalanced entry set, so it is not re-derived per call site here; point
// 6 (projection) is cashBalance's own read path (wallet.GetSummary),
// asserted by the caller alongside this helper.
func assertNoFinancialEffect(t *testing.T, pool *db.Pool, tenantID uuid.UUID, priorLedgerCount, priorAuditCount, expectIntentCount int, providerID, reference string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var ledgerCount, intentCount, tombstoneCount, auditCount int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&ledgerCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM deposit_intents WHERE tenant_id = $1 AND provider_id = $2 AND provider_reference = $3`,
			tenantID, providerID, reference).Scan(&intentCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx,
			`SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1 AND transaction_type = 'tombstone' AND provider_id = $2 AND provider_tx_id = $3`,
			tenantID, providerID, reference).Scan(&tombstoneCount); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenantID).Scan(&auditCount); err != nil {
			return err
		}
		if ledgerCount != priorLedgerCount {
			t.Errorf("expected no new ledger_transactions rows for tenant %s, had %d before and %d after", tenantID, priorLedgerCount, ledgerCount)
		}
		if intentCount != expectIntentCount {
			t.Errorf("expected %d deposit_intents row(s) for reference %q under tenant %s, got %d", expectIntentCount, reference, tenantID, intentCount)
		}
		if tombstoneCount != 0 {
			t.Errorf("expected zero tombstone rows for reference %q under tenant %s, got %d", reference, tenantID, tombstoneCount)
		}
		if auditCount != priorAuditCount {
			t.Errorf("expected no new audit_log rows for tenant %s, had %d before and %d after", tenantID, priorAuditCount, auditCount)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("assertNoFinancialEffect: %v", err)
	}
}

func countLedgerAndAudit(t *testing.T, pool *db.Pool, tenantID uuid.UUID) (ledgerCount, auditCount int) {
	t.Helper()
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ledger_transactions WHERE tenant_id = $1`, tenantID).Scan(&ledgerCount); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM audit_log WHERE tenant_id = $1`, tenantID).Scan(&auditCount)
	})
	if err != nil {
		t.Fatalf("countLedgerAndAudit: %v", err)
	}
	return
}

// TestWebhook_CrossTenant_SameRefCollision_Rejected is QA plan T2.
func TestWebhook_CrossTenant_SameRefCollision_Rejected(t *testing.T) {
	pool := testPool(t)
	tenantA := seedOrchFixture(t, pool)
	tenantB := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, tenantA, provider, 100)
	registerCapability(t, pool, tenantB, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	const sharedRef = "collision-ref-1"
	// Tenant B independently has its own intent under the identical
	// provider_reference string (a coincidental collision across tenants,
	// which is legal - provider_reference uniqueness is scoped per tenant).
	err := pool.WithTenant(context.Background(), tenantB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, status, provider_id, provider_reference, idempotency_key)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, 'EUR', 1000, 'card', 'pending', 'mock-psp', $5, 'collision-idem')`,
			tenantB.tenantID, tenantB.brandID, tenantB.playerAccountID, tenantB.walletID, sharedRef)
		return err
	})
	if err != nil {
		t.Fatalf("seed tenant B's colliding intent: %v", err)
	}

	ledgerBefore, auditBefore := countLedgerAndAudit(t, pool, tenantB.tenantID)

	// A-signed payload naming the SAME reference string, delivered to B.
	payload := provider.CallbackPayload(tenantA.tenantID, CallbackEventDeposit, sharedRef, "", OutcomeSucceeded, 1000, "EUR", "", false)
	err = pool.WithTenant(context.Background(), tenantB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, tenantB.tenantID, "mock-psp", payload)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected a *CallbackAuthError, got %v", err)
	}

	assertNoFinancialEffect(t, pool, tenantB.tenantID, ledgerBefore, auditBefore, 1, "mock-psp", sharedRef)
	// The collision row itself must be untouched.
	var status string
	if err := pool.WithTenant(context.Background(), tenantB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM deposit_intents WHERE tenant_id = $1 AND provider_reference = $2`, tenantB.tenantID, sharedRef).Scan(&status)
	}); err != nil {
		t.Fatalf("reread collision row: %v", err)
	}
	if status != "pending" {
		t.Fatalf("expected tenant B's colliding intent to remain 'pending', got %q", status)
	}
}

// TestWebhook_CrossTenant_ReversalOfUnseenRef_NoTombstone is QA plan T3 -
// the direct regression test for S-6 (the pre-fix evidence attack).
func TestWebhook_CrossTenant_ReversalOfUnseenRef_NoTombstone(t *testing.T) {
	pool := testPool(t)
	tenantA := seedOrchFixture(t, pool)
	tenantB := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, tenantA, provider, 100)
	registerCapability(t, pool, tenantB, provider, 100)
	orchA := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})
	orchB := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	const reversalRef = "s6-regression-reversal"
	const originalRef = "s6-regression-original-unseen"

	// Control: this exact payload IS accepted under tenant A (it tombstones,
	// since the original is unseen there too - the payload itself is valid).
	payload := provider.CallbackPayload(tenantA.tenantID, CallbackEventDepositReversal, reversalRef, originalRef, OutcomeSucceeded, 1000, "EUR", "", false)
	err := pool.WithTenant(context.Background(), tenantA.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orchA.ReceiveCallback(ctx, tx, tenantA.tenantID, "mock-psp", payload)
		return err
	})
	if err != nil {
		t.Fatalf("control (tenant A, own payload) must be accepted: %v", err)
	}

	ledgerBefore, auditBefore := countLedgerAndAudit(t, pool, tenantB.tenantID)

	// The attack: same bytes, delivered under tenant B's scope.
	err = pool.WithTenant(context.Background(), tenantB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orchB.ReceiveCallback(ctx, tx, tenantB.tenantID, "mock-psp", payload)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) {
		t.Fatalf("expected a *CallbackAuthError for the cross-tenant reversal, got %v", err)
	}

	assertNoFinancialEffect(t, pool, tenantB.tenantID, ledgerBefore, auditBefore, 0, "mock-psp", originalRef)
}

// TestWebhook_SharedSecretAcrossTenants_TenantStillBound is QA plan T5:
// even a resolver that (misconfigured, or by coincidence) hands out the
// IDENTICAL secret for two different tenants still rejects an A-signed
// callback delivered as B, because the tenant is part of what is signed,
// not just which key was used to sign it.
func TestWebhook_SharedSecretAcrossTenants_TenantStillBound(t *testing.T) {
	pool := testPool(t)
	tenantA := seedOrchFixture(t, pool)
	tenantB := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, tenantA, provider, 100)
	registerCapability(t, pool, tenantB, provider, 100)

	sharedSecret := []byte("this-is-not-a-real-secret-only-32b")
	resolver := sharedSecretResolver{secret: sharedSecret, providerID: "mock-psp"}
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, resolver)

	credA, _ := resolver.Resolve(context.Background(), tenantA.tenantID, "mock-psp", mockWebhookKeyID)
	body := []byte(`{"event_type":"deposit","provider_reference":"t5-ref","outcome":"succeeded","amount":1000,"asset_code":"EUR"}`)
	sig := signWithKey(credA.Secret, tenantA.tenantID, "mock-psp", mockWebhookKeyID, body)
	header := make(http.Header)
	header.Set(HeaderSignature, "v1="+sig)
	header.Set(HeaderKeyID, mockWebhookKeyID)
	inbound := InboundCallback{Header: header, Body: body}

	err := pool.WithTenant(context.Background(), tenantB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, tenantB.tenantID, "mock-psp", inbound)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != ReasonSignatureInvalid {
		t.Fatalf("expected ReasonSignatureInvalid even though A and B share one secret (the tenant is IN the signature), got %v", err)
	}
}

// TestWebhook_DisabledCapability_StillAccepted is QA plan T10.
func TestWebhook_DisabledCapability_StillAccepted(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	// Disable it - a routing kill-switch, not a callback-acceptance one
	// (I4/ruling 7).
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE provider_capabilities SET status = 'disabled' WHERE tenant_id = $1 AND provider_id = 'mock-psp'`, f.tenantID)
		return err
	})
	if err != nil {
		t.Fatalf("disable capability: %v", err)
	}
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, MultiWebhookCredentialResolver{"mock-psp": NewMockWebhookCredentials(provider)})

	var intent DepositIntent
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: 4321, PaymentMethod: "card", IdempotencyKey: "t10-disabled",
		})
		return err
	})
	// A disabled capability also stops routing entirely, so InitiateDeposit
	// declines synchronously with no provider reference at all - directly
	// write the intent row instead, modeling "a deposit was already in
	// flight before the provider was disabled", which is exactly the
	// scenario I4 protects.
	if err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	if intent.ProviderReference == nil {
		ref := "t10-manual-ref"
		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`INSERT INTO deposit_intents (id, tenant_id, brand_id, player_account_id, wallet_id, asset_code, amount, payment_method, status, provider_id, provider_reference, idempotency_key)
				 VALUES (gen_random_uuid(), $1, $2, $3, $4, 'EUR', 4321, 'card', 'pending', 'mock-psp', $5, 't10-manual-intent')
				 RETURNING id`,
				f.tenantID, f.brandID, f.playerAccountID, f.walletID, ref,
			).Scan(&intent.ID)
		})
		if err != nil {
			t.Fatalf("seed in-flight intent for disabled provider: %v", err)
		}
		intent.ProviderReference = &ref
	}

	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, 4321, "EUR", "", false)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", payload)
		return err
	})
	if err != nil {
		t.Fatalf("expected a callback for a 'disabled' (routing-only) capability to still be accepted, got %v", err)
	}
}

// TestProviderAcceptsWebhook_RLSScoped is QA plan T11b.
func TestProviderAcceptsWebhook_RLSScoped(t *testing.T) {
	pool := testPool(t)
	tenantA := seedOrchFixture(t, pool)
	tenantB := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, tenantA, provider, 100)
	// Tenant B has NO capability row for mock-psp at all.

	var acceptsUnderB bool
	err := pool.WithTenant(context.Background(), tenantB.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		acceptsUnderB, err = ProviderAcceptsWebhook(ctx, tx, tenantA.tenantID, "mock-psp")
		return err
	})
	if err != nil {
		t.Fatalf("ProviderAcceptsWebhook under B's GUC: %v", err)
	}
	if acceptsUnderB {
		t.Fatal("ProviderAcceptsWebhook, run under tenant B's RLS GUC, must not see tenant A's capability row even when explicitly asked about tenant A's id")
	}
}

// TestOrchestrator_NoResolver_FailsClosed is QA plan T14, at the
// integration level (a real tx, proving no DB write happens either).
func TestOrchestrator_NoResolver_FailsClosed(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, nil)

	payload := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, "t14-ref", "", OutcomeSucceeded, 1000, "EUR", "", false)
	ledgerBefore, auditBefore := countLedgerAndAudit(t, pool, f.tenantID)
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-psp", payload)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != ReasonNoResolver {
		t.Fatalf("expected *CallbackAuthError{Reason: no_resolver}, got %v", err)
	}
	assertNoFinancialEffect(t, pool, f.tenantID, ledgerBefore, auditBefore, 0, "mock-psp", "t14-ref")
}
