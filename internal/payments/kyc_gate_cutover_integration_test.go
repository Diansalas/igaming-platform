//go:build integration

// PRH-I1 deposit cutover: proves the REAL DepositKYCGate
// (KYCEnforcementDepositGate, kycgate.go) wired to internal/kyc.
// EvaluateEnforcement (ADR 0096) actually gates InitiateDepositAttempt -
// not merely the test stand-ins (AllowAllDepositKYCGate/denyingKYCGate)
// deposit_v2_integration_test.go already exercises. This file is also the
// mutation-kill evidence for "skip the KYC gate in the live path" (see
// docs/plans/payment-readiness/evidence/prh-i1-mutation-kill.txt).
package payments

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// spyKYCGate is the mutation-kill instrument for "reject Amount <= 0
// before evaluating" (security requirement, PRH-I1 deposit cutover): it
// panics if ever called, so any test using it fails loudly instead of
// silently passing if a non-positive amount ever reached the KYC
// evaluation step.
type spyKYCGate struct{}

func (spyKYCGate) EvaluateDeposit(context.Context, pgx.Tx, uuid.UUID, uuid.UUID, uuid.UUID, uuid.UUID, int64, string) (bool, string, error) {
	panic("spyKYCGate: EvaluateDeposit must never be called for a non-positive amount")
}

// TestInitiateDepositAttempt_NonPositiveAmount_RejectedBeforeKYCEvaluation
// is the mutation-kill test for "reject Amount <= 0 before evaluating"
// (security requirement): InitiateDepositAttempt must return an error for
// Amount<=0 WITHOUT ever calling the KYC gate (spyKYCGate would panic) and
// without creating any deposit_intents/payment_attempts row.
func TestInitiateDepositAttempt_NonPositiveAmount_RejectedBeforeKYCEvaluation(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp-nonpositive", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-nonpositive": provider}, MultiWebhookCredentialResolver{"mock-psp-nonpositive": NewMockWebhookCredentials(provider)})

	for _, amount := range []int64{0, -1, -1_000_000} {
		_, err := orch.InitiateDepositAttempt(context.Background(), pool, spyKYCGate{}, MockCredentialResolver{}, InitiateDepositParams{
			Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: "nonpositive-" + uuid.New().String(),
		})
		if err == nil {
			t.Fatalf("expected an error for amount=%d, got nil", amount)
		}
	}
	if provider.AttemptCount() != 0 {
		t.Fatalf("expected zero provider calls, got %d", provider.AttemptCount())
	}
}

// seedActiveDepositEnforcementPolicy creates and ACTIVATES (four-eyes: a
// different platform-admin principal creates vs. activates, mirroring
// internal/kyc's own migration-0100 lifecycle trigger requirement) a
// cumulative_deposit kyc_enforcement_policy for a freshly seeded
// jurisdiction, and returns that jurisdiction's id.
func seedActiveDepositEnforcementPolicy(t *testing.T, pool *db.Pool, thresholdMinorUnits, assetCode string) uuid.UUID {
	t.Helper()
	jurisdictionID := uuid.New()
	creator := uuid.New()
	activator := uuid.New()

	if err := pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'PRH-I1 cutover test jurisdiction')`,
			jurisdictionID, "prhi1-"+jurisdictionID.String()[:8])
		return err
	}); err != nil {
		t.Fatalf("seed jurisdiction: %v", err)
	}

	var policyID uuid.UUID
	if err := pool.WithPlatformAdmin(context.Background(), creator, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			INSERT INTO kyc_enforcement_policies
				(licensing_jurisdiction_id, trigger_type, status, threshold_minor_units, asset_code,
				 legal_review_reference, reason_code, created_by_actor_type, created_by_actor_id)
			VALUES ($1, 'cumulative_deposit', 'draft', $2, $3, 'LR-PRH-I1', 'prh-i1-cutover-test', 'platform_admin', $4)
			RETURNING id`, jurisdictionID, thresholdMinorUnits, assetCode, creator).Scan(&policyID)
	}); err != nil {
		t.Fatalf("seed draft enforcement policy: %v", err)
	}

	if err := pool.WithPlatformAdmin(context.Background(), activator, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE kyc_enforcement_policies SET status = 'active' WHERE id = $1 AND status = 'draft'`, policyID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to activate exactly 1 policy row, activated %d", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("activate enforcement policy: %v", err)
	}
	return jurisdictionID
}

// bindTenantLicence seeds a platform licence for jurisdictionID and binds
// tenantID to it, so resolveLicensingJurisdictionID (internal/kyc)
// resolves a real jurisdiction instead of "tenant has no licence bound".
func bindTenantLicence(t *testing.T, pool *db.Pool, tenantID, jurisdictionID uuid.UUID) {
	t.Helper()
	var licenceID uuid.UUID
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`INSERT INTO licences (jurisdiction_id, licensee, licence_number) VALUES ($1, 'platform', $2) RETURNING id`,
			jurisdictionID, "LIC-"+jurisdictionID.String()[:8]).Scan(&licenceID)
	}); err != nil {
		t.Fatalf("seed licence: %v", err)
	}
	if err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE tenants SET licence_id = $1 WHERE id = $2`, licenceID, tenantID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("expected to bind exactly 1 tenant row, bound %d", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("bind tenant licence: %v", err)
	}
}

// TestInitiateDepositAttempt_RealKYCEnforcementGate_DeniesOverThreshold:
// with the REAL gate wired and an ACTIVE cumulative_deposit policy this
// deposit's amount exceeds, and no KYC verification on file, the deposit
// must be denied as a COMMITTED decision (deposit_intents row exists,
// status=declined) with ZERO payment_attempts rows and ZERO provider
// calls - exactly the RG-deny shape (ADR 0095 §4.3 T1+T2).
func TestInitiateDepositAttempt_RealKYCEnforcementGate_DeniesOverThreshold(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	jurisdictionID := seedActiveDepositEnforcementPolicy(t, pool, "1000", "EUR")
	bindTenantLicence(t, pool, f.tenantID, jurisdictionID)

	provider := NewMockProvider("mock-psp-kyc-real", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-kyc-real": provider}, MultiWebhookCredentialResolver{"mock-psp-kyc-real": NewMockWebhookCredentials(provider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, KYCEnforcementDepositGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "real-kyc-deny",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if res.AttemptCreated {
		t.Fatalf("a real KYC deny must never create an attempt")
	}
	if res.Intent.Status != DepositIntentDeclined {
		t.Fatalf("expected intent status declined on a real KYC deny, got %s", res.Intent.Status)
	}
	if provider.AttemptCount() != 0 {
		t.Fatalf("expected zero provider calls on a real KYC deny, got %d", provider.AttemptCount())
	}

	var count int
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT count(*) FROM payment_attempts WHERE deposit_intent_id = $1`, res.Intent.ID).Scan(&count)
	}); err != nil {
		t.Fatalf("count attempts: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected zero payment_attempts rows for a KYC-denied intent, got %d", count)
	}
}

// TestInitiateDepositAttempt_RealKYCEnforcementGate_AllowsUnderThreshold
// proves the real gate is not simply always-deny once wired: a deposit
// whose cumulative total stays under the SAME active policy's threshold
// is allowed through to a real (mock) provider call.
func TestInitiateDepositAttempt_RealKYCEnforcementGate_AllowsUnderThreshold(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	jurisdictionID := seedActiveDepositEnforcementPolicy(t, pool, "1000000", "EUR")
	bindTenantLicence(t, pool, f.tenantID, jurisdictionID)

	provider := NewMockProvider("mock-psp-kyc-real-b", "EUR")
	registerCapability(t, pool, f, provider, 100)
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp-kyc-real-b": provider}, MultiWebhookCredentialResolver{"mock-psp-kyc-real-b": NewMockWebhookCredentials(provider)})

	res, err := orch.InitiateDepositAttempt(context.Background(), pool, KYCEnforcementDepositGate{}, MockCredentialResolver{}, InitiateDepositParams{
		Scope:     DepositScope{TenantID: f.tenantID, BrandID: f.brandID, PlayerAccountID: f.playerAccountID, WalletID: f.walletID},
		AssetCode: "EUR", Amount: 5000, PaymentMethod: "card", IdempotencyKey: "real-kyc-allow",
	})
	if err != nil {
		t.Fatalf("InitiateDepositAttempt: %v", err)
	}
	if !res.AttemptCreated {
		t.Fatalf("expected an attempt to be created when the deposit stays under the active threshold")
	}
	if provider.AttemptCount() != 1 {
		t.Fatalf("expected exactly 1 provider call, got %d", provider.AttemptCount())
	}
}
