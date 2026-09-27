package payments

import (
	"context"
	"errors"
	"testing"

	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

// nonSyntheticFakeProvider is a minimal PaymentProvider that deliberately
// does NOT implement syntheticPaymentsAdapter (SyntheticComponent) - unlike
// MockProvider, which always does. Every method beyond Capabilities is
// unused by validateManifest and returns a zero value; this type exists
// only to exercise the "production-eligible adapter" branch of
// validateManifest without needing a real vendor adapter.
type nonSyntheticFakeProvider struct {
	capability AdapterCapability
}

func (f nonSyntheticFakeProvider) Deposit(context.Context, DepositRequest) (DepositResult, error) {
	return DepositResult{}, errors.New("unused in this test")
}
func (f nonSyntheticFakeProvider) Withdraw(context.Context, WithdrawRequest) (WithdrawResult, error) {
	return WithdrawResult{}, errors.New("unused in this test")
}
func (f nonSyntheticFakeProvider) QueryStatus(context.Context, string) (StatusResult, error) {
	return StatusResult{}, errors.New("unused in this test")
}
func (f nonSyntheticFakeProvider) HandleCallback(context.Context, InboundCallback, WebhookCredential) (CallbackEvent, error) {
	return CallbackEvent{}, errors.New("unused in this test")
}
func (f nonSyntheticFakeProvider) WebhookScheme() webhookauth.VerificationScheme {
	return nil
}
func (f nonSyntheticFakeProvider) Capabilities() AdapterCapability { return f.capability }
func (f nonSyntheticFakeProvider) HealthStatus(context.Context) (ProviderHealth, error) {
	return ProviderHealth{}, errors.New("unused in this test")
}

func TestValidateManifest_SupportsRefundAlwaysRefused(t *testing.T) {
	mock := NewMockProvider("mock-refund-test", "EUR")
	mock.SetManifest(OperationManifest{SupportsDeposit: true, SupportsRefund: true})
	declared := mock.Capabilities()

	err := validateManifest(mock, declared)
	if !errors.Is(err, ErrManifestRegistrationRefused) {
		t.Fatalf("expected ErrManifestRegistrationRefused, got %v", err)
	}
}

func TestValidateManifest_SupportsRefundFalseIsFine(t *testing.T) {
	mock := NewMockProvider("mock-refund-test-2", "EUR")
	declared := mock.Capabilities()
	declared.Manifest.SupportsRefund = false
	if err := validateManifest(mock, declared); err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestValidateManifest_SyntheticAdapterExemptFromCallbackEchoRule(t *testing.T) {
	mock := NewMockProvider("mock-lf95c5-test", "EUR")
	declared := mock.Capabilities()
	declared.SupportsDeposit = true
	declared.Manifest.CallbackEchoesMerchantReference = false
	declared.Manifest.StatusQuery = "by_provider_reference"
	if err := validateManifest(mock, declared); err != nil {
		t.Fatalf("a Synthetic (MOCK) adapter must be exempt from LF95-C5, got %v", err)
	}
}

func TestValidateManifest_ProductionEligibleAdapterRequiresConvergence(t *testing.T) {
	declared := AdapterCapability{
		ProviderID:      "real-psp",
		SupportsDeposit: true,
		Manifest: OperationManifest{
			SupportsDeposit:                 true,
			CallbackEchoesMerchantReference: false,
			StatusQuery:                     "by_provider_reference",
		},
	}
	fake := nonSyntheticFakeProvider{capability: declared}
	err := validateManifest(fake, declared)
	if !errors.Is(err, ErrManifestRegistrationRefused) {
		t.Fatalf("expected ErrManifestRegistrationRefused (LF95-C5), got %v", err)
	}
}

func TestValidateManifest_ProductionEligibleAdapterSatisfiedByCallbackEcho(t *testing.T) {
	declared := AdapterCapability{
		ProviderID:      "real-psp-2",
		SupportsDeposit: true,
		Manifest: OperationManifest{
			SupportsDeposit:                 true,
			CallbackEchoesMerchantReference: true,
			StatusQuery:                     "by_provider_reference",
		},
	}
	fake := nonSyntheticFakeProvider{capability: declared}
	if err := validateManifest(fake, declared); err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestValidateManifest_ProductionEligibleAdapterSatisfiedByStatusQuery(t *testing.T) {
	declared := AdapterCapability{
		ProviderID:         "real-psp-3",
		SupportsWithdrawal: true,
		Manifest: OperationManifest{
			CallbackEchoesMerchantReference: false,
			StatusQuery:                     "by_provider_or_merchant_reference",
		},
	}
	fake := nonSyntheticFakeProvider{capability: declared}
	if err := validateManifest(fake, declared); err != nil {
		t.Fatalf("unexpected refusal: %v", err)
	}
}

func TestValidateManifest_ProductionEligibleAdapterNotSupportingMoneyMovementIsExempt(t *testing.T) {
	declared := AdapterCapability{
		ProviderID: "real-psp-4",
		Manifest: OperationManifest{
			CallbackEchoesMerchantReference: false,
			StatusQuery:                     "none",
		},
	}
	fake := nonSyntheticFakeProvider{capability: declared}
	if err := validateManifest(fake, declared); err != nil {
		t.Fatalf("an adapter supporting neither deposit nor withdrawal has nothing to converge; unexpected refusal: %v", err)
	}
}
