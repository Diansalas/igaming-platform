package payments

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestMockProvider_ConformanceSuite proves the mock is not exempt from
// docs/decisions/0022 §6's conformance requirement - "the mock is not
// exempt from this suite and does not get its own bespoke test path."
func TestMockProvider_ConformanceSuite(t *testing.T) {
	RunProviderConformanceSuite(t, func() PaymentProvider {
		return NewMockProvider("mock-psp")
	})
}

func TestMockProvider_DepositMagicAmounts(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider("mock-psp")

	cases := []struct {
		name    string
		amount  int64
		outcome Outcome
	}{
		{"player decline", MockAmountPlayerDeclineNoCascade, OutcomeDeclined},
		{"provider decline", MockAmountProviderDeclineCascade, OutcomeDeclined},
		{"ambiguous", MockAmountAmbiguous, OutcomeAmbiguous},
		{"pending", 5000, OutcomePending},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := provider.Deposit(ctx, DepositRequest{Amount: tc.amount, AssetCode: "EUR", PaymentMethod: "card"})
			if err != nil {
				t.Fatalf("Deposit: %v", err)
			}
			if result.Outcome != tc.outcome {
				t.Fatalf("expected outcome %v, got %v", tc.outcome, result.Outcome)
			}
		})
	}
}

func TestMockProvider_CascadableFlagMatchesTaxonomy(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider("mock-psp")

	playerDecline, _ := provider.Deposit(ctx, DepositRequest{Amount: MockAmountPlayerDeclineNoCascade, AssetCode: "EUR", PaymentMethod: "card"})
	if playerDecline.Cascadable {
		t.Fatal("a player-specific decline (insufficient funds) must not be cascadable")
	}

	providerDecline, _ := provider.Deposit(ctx, DepositRequest{Amount: MockAmountProviderDeclineCascade, AssetCode: "EUR", PaymentMethod: "card"})
	if !providerDecline.Cascadable {
		t.Fatal("a provider-specific decline (outage) must be cascadable")
	}
}

func TestMockProvider_ResolveAmbiguousViaQueryStatus(t *testing.T) {
	ctx := context.Background()
	provider := NewMockProvider("mock-psp")

	result, err := provider.Deposit(ctx, DepositRequest{Amount: MockAmountAmbiguous, AssetCode: "EUR", PaymentMethod: "card"})
	if err != nil {
		t.Fatalf("Deposit: %v", err)
	}

	status, err := provider.QueryStatus(ctx, result.ProviderReference)
	if err != nil {
		t.Fatalf("QueryStatus: %v", err)
	}
	if status.Outcome != OutcomeAmbiguous {
		t.Fatalf("expected still-ambiguous before Resolve, got %v", status.Outcome)
	}

	provider.Resolve(result.ProviderReference, OutcomeSucceeded, "", false)

	status, err = provider.QueryStatus(ctx, result.ProviderReference)
	if err != nil {
		t.Fatalf("QueryStatus after resolve: %v", err)
	}
	if status.Outcome != OutcomeSucceeded {
		t.Fatalf("expected OutcomeSucceeded after Resolve, got %v", status.Outcome)
	}
}

func TestMockProvider_HandleCallback_RejectsNestedKeyMaterial(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()
	tenantID := uuid.New()

	nested := []byte(`{
		"event_type": "deposit",
		"provider_reference": "ref-1",
		"outcome": "succeeded",
		"amount": 1000,
		"asset_code": "EUR",
		"wallet_details": {"mnemonic": "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"}
	}`)
	// Stage 10.1 security review P2-1/code review F1/architect PW-1:
	// HandleCallback now verifies the signature BEFORE parsing the body at
	// all (any pre-verification parse failure must be indistinguishable
	// from a bad signature - see HandleCallback's own doc comment), so the
	// key-material scan is only reached once verification has ALREADY
	// succeeded. This body must therefore be genuinely, correctly signed -
	// SignRawBody (not CallbackPayload, whose fixed mockCallbackBody shape
	// has no room for a "wallet_details" field) signs these exact bytes.
	cred := mockCredentialFor(t, provider, tenantID)
	inbound := provider.SignRawBody(tenantID, nested)
	_, err := provider.HandleCallback(ctx, inbound, cred)
	if !errors.Is(err, ErrInboundKeyMaterial) {
		t.Fatalf("expected ErrInboundKeyMaterial for nested key material, got %v", err)
	}
}

func TestMockProvider_HandleCallback_UnknownEventTypeRejected(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()
	tenantID := uuid.New()
	cred := mockCredentialFor(t, provider, tenantID)

	inbound := provider.CallbackPayload(tenantID, CallbackEventType("withdrawal_sent"), "ref-1", "", OutcomeSucceeded, 0, "", "", false)
	_, err := provider.HandleCallback(ctx, inbound, cred)
	if err == nil {
		t.Fatal("expected an error for an event_type this stage does not implement")
	}
}

// TestMockProvider_HandleCallback_RejectsForgedCallback closes the P0
// finding from the Stage 3B security review, extended by PAY-WH-TENANT-1
// (ADR 0090; docs/decisions/0022 §3 amendment): the webhook route
// (POST /v1/webhooks/payments/{tenantSlug}/{providerID}) has no bearer-
// auth middleware by design, and provider_reference values are
// sequential and even handed to the player in InitiateDeposit's
// redirect_url - so without signature verification, ANYONE could forge a
// "succeeded" callback for any reference and mint an arbitrary ledger
// credit. This proves HandleCallback rejects a payload with no signature
// header, a payload with a garbage signature, and a payload signed by a
// DIFFERENT MockProvider instance's own (different) derived key - none of
// which should ever reach outcome/amount interpretation.
func TestMockProvider_HandleCallback_RejectsForgedCallback(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	other := NewMockProvider("mock-psp") // distinct instance -> distinct masterSecret
	ctx := context.Background()
	tenantID := uuid.New()
	cred := mockCredentialFor(t, provider, tenantID)

	rawBody := []byte(`{"event_type":"deposit","provider_reference":"mock-psp-1","outcome":"succeeded","amount":100000000,"asset_code":"EUR"}`)

	unsigned := InboundCallback{TenantID: tenantID, ProviderID: "mock-psp", Header: http.Header{}, Body: rawBody}
	if _, err := provider.HandleCallback(ctx, unsigned, cred); !errors.Is(err, ErrCallbackSignatureInvalid) {
		t.Fatalf("expected ErrCallbackSignatureInvalid for a callback with no signature header, got %v", err)
	}

	garbage := http.Header{}
	garbage.Set(HeaderSignature, "v1="+strings.Repeat("a", 64))
	garbage.Set(HeaderKeyID, mockWebhookKeyID)
	garbageSig := InboundCallback{TenantID: tenantID, ProviderID: "mock-psp", Header: garbage, Body: rawBody}
	if _, err := provider.HandleCallback(ctx, garbageSig, cred); !errors.Is(err, ErrCallbackSignatureInvalid) {
		t.Fatalf("expected ErrCallbackSignatureInvalid for a garbage signature, got %v", err)
	}

	// Signed correctly, but by a DIFFERENT adapter instance entirely (its
	// own derived key, for the SAME tenant) - the forger who controls
	// "other" cannot produce a signature "provider" will accept merely by
	// running the same adapter code.
	wrongSecret := other.CallbackPayload(tenantID, CallbackEventDeposit, "mock-psp-1", "", OutcomeSucceeded, 100000000, "EUR", "", false)
	if _, err := provider.HandleCallback(ctx, wrongSecret, cred); !errors.Is(err, ErrCallbackSignatureInvalid) {
		t.Fatalf("expected ErrCallbackSignatureInvalid for a callback signed with a different instance's key, got %v", err)
	}

	// Sanity check: the identical payload, correctly signed by "provider"
	// itself for tenantID, must be accepted - proves the rejections above
	// are actually about the signature, not some other malformation.
	genuine := provider.CallbackPayload(tenantID, CallbackEventDeposit, "mock-psp-1", "", OutcomeSucceeded, 100000000, "EUR", "", false)
	if _, err := provider.HandleCallback(ctx, genuine, cred); err != nil {
		t.Fatalf("expected a genuinely-signed callback to be accepted, got %v", err)
	}
}

// TestMockProvider_HandleCallback_TamperedFieldAfterSigningRejected proves
// the signature actually covers the raw body bytes, not just an opaque
// token: a genuine signature computed over one amount must not verify once
// the body is edited afterward - the exact "attacker captures a legitimate
// low-value callback and edits the amount upward" scenario the signature
// exists to prevent (QA plan T6).
func TestMockProvider_HandleCallback_TamperedFieldAfterSigningRejected(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()
	tenantID := uuid.New()
	cred := mockCredentialFor(t, provider, tenantID)

	cases := []struct {
		name string
		from string
		to   string
	}{
		{"amount", `"amount":500`, `"amount":500000000`},
		{"outcome", `"outcome":"succeeded"`, `"outcome":"declined"`},
		{"provider_reference", `"provider_reference":"mock-psp-1"`, `"provider_reference":"mock-psp-2"`},
		{"whitespace byte", `"amount":500,`, `"amount":500 ,`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			genuine := provider.CallbackPayload(tenantID, CallbackEventDeposit, "mock-psp-1", "", OutcomeSucceeded, 500, "EUR", "", false)
			tampered := bytes.Replace(genuine.Body, []byte(tc.from), []byte(tc.to), 1)
			if bytes.Equal(genuine.Body, tampered) {
				t.Fatal("test setup bug: tampering did not change the payload")
			}
			genuine.Body = tampered
			if _, err := provider.HandleCallback(ctx, genuine, cred); !errors.Is(err, ErrCallbackSignatureInvalid) {
				t.Fatalf("expected ErrCallbackSignatureInvalid for a tampered %s, got %v", tc.name, err)
			}
		})
	}
}

// TestMockProvider_HandleCallback_LegacySignatureFieldRejected proves a
// body that still carries the Stage-3B-shaped "signature" field is
// rejected (design §2.2: "a body that still carries one is rejected"),
// even though it verifies fine as an ordinary body byte sequence - QA plan
// T7's last case.
func TestMockProvider_HandleCallback_LegacySignatureFieldRejected(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()
	tenantID := uuid.New()
	cred := mockCredentialFor(t, provider, tenantID)

	body := []byte(`{"event_type":"deposit","provider_reference":"ref-legacy","outcome":"succeeded","amount":1000,"asset_code":"EUR","signature":"deadbeef"}`)
	sig := signWithKey(cred.Secret, tenantID, "mock-psp", mockWebhookKeyID, body)
	header := http.Header{}
	header.Set(HeaderSignature, "v1="+sig)
	header.Set(HeaderKeyID, mockWebhookKeyID)
	inbound := InboundCallback{TenantID: tenantID, ProviderID: "mock-psp", Header: header, Body: body}

	if _, err := provider.HandleCallback(ctx, inbound, cred); !errors.Is(err, ErrCallbackSignatureInvalid) {
		t.Fatalf("expected ErrCallbackSignatureInvalid for a body still carrying a legacy signature field, got %v", err)
	}
}

// TestMockProvider_HandleCallback_BindingTamperRejected is QA plan T7: every
// way the HEADER/tenant binding (rather than the body) can be tampered
// with must independently be rejected.
func TestMockProvider_HandleCallback_BindingTamperRejected(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()
	credA := mockCredentialFor(t, provider, tenantA)
	credB := mockCredentialFor(t, provider, tenantB)

	t.Run("signature computed for tenant B delivered as tenant A", func(t *testing.T) {
		inbound := provider.CallbackPayload(tenantB, CallbackEventDeposit, "ref-bind-1", "", OutcomeSucceeded, 1000, "EUR", "", false)
		inbound.TenantID = tenantA
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid, got %v", err)
		}
	})

	t.Run("wrong provider_id in the signing input", func(t *testing.T) {
		other := NewMockProvider("mock-other")
		inbound := other.CallbackPayload(tenantA, CallbackEventDeposit, "ref-bind-2", "", OutcomeSucceeded, 1000, "EUR", "", false)
		inbound.ProviderID = "mock-psp"
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid, got %v", err)
		}
	})

	t.Run("unknown key_id", func(t *testing.T) {
		inbound := provider.CallbackPayload(tenantA, CallbackEventDeposit, "ref-bind-3", "", OutcomeSucceeded, 1000, "EUR", "", false)
		inbound.Header.Set(HeaderKeyID, "mock-v2")
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid, got %v", err)
		}
	})

	t.Run("missing signature header", func(t *testing.T) {
		inbound := provider.CallbackPayload(tenantA, CallbackEventDeposit, "ref-bind-4", "", OutcomeSucceeded, 1000, "EUR", "", false)
		inbound.Header.Del(HeaderSignature)
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid, got %v", err)
		}
	})

	t.Run("missing key id header", func(t *testing.T) {
		inbound := provider.CallbackPayload(tenantA, CallbackEventDeposit, "ref-bind-5", "", OutcomeSucceeded, 1000, "EUR", "", false)
		inbound.Header.Del(HeaderKeyID)
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid, got %v", err)
		}
	})

	t.Run("63 hex characters", func(t *testing.T) {
		inbound := provider.CallbackPayload(tenantA, CallbackEventDeposit, "ref-bind-6", "", OutcomeSucceeded, 1000, "EUR", "", false)
		inbound.Header.Set(HeaderSignature, "v1="+strings.Repeat("a", 63))
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid, got %v", err)
		}
	})

	t.Run("65 hex characters", func(t *testing.T) {
		inbound := provider.CallbackPayload(tenantA, CallbackEventDeposit, "ref-bind-7", "", OutcomeSucceeded, 1000, "EUR", "", false)
		inbound.Header.Set(HeaderSignature, "v1="+strings.Repeat("a", 65))
		if _, err := provider.HandleCallback(ctx, inbound, credA); !errors.Is(err, ErrCallbackSignatureInvalid) {
			t.Fatalf("expected ErrCallbackSignatureInvalid, got %v", err)
		}
	})

	// Sanity: an untampered, correctly-bound callback for tenant B under
	// its own credential is accepted - proves the rejections above are
	// actually about the specific tamper, not a broken fixture.
	t.Run("untampered sanity check", func(t *testing.T) {
		inbound := provider.CallbackPayload(tenantB, CallbackEventDeposit, "ref-bind-sanity", "", OutcomeSucceeded, 1000, "EUR", "", false)
		if _, err := provider.HandleCallback(ctx, inbound, credB); err != nil {
			t.Fatalf("expected the untampered, correctly-bound callback to be accepted, got %v", err)
		}
	})
}

// TestMockWebhookCredentials_KeyDerivation is QA plan T15.
func TestMockWebhookCredentials_KeyDerivation(t *testing.T) {
	provider := NewMockProvider("mock-psp")
	other := NewMockProvider("mock-psp")
	resolver := NewMockWebhookCredentials(provider)
	otherResolver := NewMockWebhookCredentials(other)
	ctx := context.Background()
	tenantA, tenantB := uuid.New(), uuid.New()

	credA1, err := resolver.Resolve(ctx, tenantA, "mock-psp", mockWebhookKeyID)
	if err != nil {
		t.Fatalf("resolve tenant A: %v", err)
	}
	credA2, err := resolver.Resolve(ctx, tenantA, "mock-psp", mockWebhookKeyID)
	if err != nil {
		t.Fatalf("resolve tenant A (again): %v", err)
	}
	credB, err := resolver.Resolve(ctx, tenantB, "mock-psp", mockWebhookKeyID)
	if err != nil {
		t.Fatalf("resolve tenant B: %v", err)
	}
	credOtherInstance, err := otherResolver.Resolve(ctx, tenantA, "mock-psp", mockWebhookKeyID)
	if err != nil {
		t.Fatalf("resolve tenant A on a different instance: %v", err)
	}

	if !bytes.Equal(credA1.Secret, credA2.Secret) {
		t.Fatal("the derived key for the SAME (tenant, provider) must be stable within one process")
	}
	if bytes.Equal(credA1.Secret, credB.Secret) {
		t.Fatal("derived keys for DIFFERENT tenants must differ")
	}
	if bytes.Equal(credA1.Secret, credOtherInstance.Secret) {
		t.Fatal("derived keys must differ across two MockProvider instances (distinct masterSecret)")
	}

	// A providerID/keyID this resolver does not own fails closed, never
	// falling back to any other credential (conformance-relevant: this is
	// exactly what a real resolver must also do).
	if _, err := resolver.Resolve(ctx, tenantA, "some-other-provider", mockWebhookKeyID); !errors.Is(err, ErrWebhookCredentialUnavailable) {
		t.Fatalf("expected ErrWebhookCredentialUnavailable for a foreign provider_id, got %v", err)
	}
	if _, err := resolver.Resolve(ctx, tenantA, "mock-psp", "mock-v2"); !errors.Is(err, ErrWebhookCredentialUnavailable) {
		t.Fatalf("expected ErrWebhookCredentialUnavailable for an unknown key_id, got %v", err)
	}

	// The secret must never leak through any of a WebhookCredential's
	// loggable representations.
	//
	// Security review P3-1 (Stage 10.1): the ORIGINAL version of this
	// check compared against string(credA1.Secret) - the raw byte string -
	// which %+v never renders literally even WITHOUT redaction (Go's
	// default struct formatting prints a byte slice as a decimal-element
	// list, e.g. "[18 52 ...]", never as the raw byte string), so the
	// assertion could never fail regardless of whether redaction worked.
	// This version instead checks for the hex encoding of the secret -
	// the actual substring a byte-slice-aware or "%x"-style rendering
	// WOULD produce - across every loggable representation, including
	// %#v (GoString, which bypasses Stringer entirely and is the one
	// representation the original test suite never exercised at all -
	// P3-1's GoString gap).
	secretHex := hex.EncodeToString(credA1.Secret)
	representations := map[string]string{
		"%+v":             fmt.Sprintf("%+v", credA1),
		"%v":              fmt.Sprintf("%v", credA1),
		"%#v":             fmt.Sprintf("%#v", credA1),
		"String()":        credA1.String(),
		"GoString()":      credA1.GoString(),
		"LogValue() slog": slogRenderAttrs(credA1),
	}
	for label, rendered := range representations {
		if strings.Contains(rendered, secretHex) {
			t.Fatalf("%s must never render the Secret bytes (hex), got %q", label, rendered)
		}
		if strings.Contains(rendered, string(credA1.Secret)) {
			t.Fatalf("%s must never render the raw Secret bytes, got %q", label, rendered)
		}
	}
}

// slogRenderAttrs renders a slog.LogValuer's group the way a real
// structured-logging handler would - a plain text.Handler capturing into
// a buffer - so the redaction check above exercises the ACTUAL logging
// path (LogValue -> slog.Handler), not just the Value's own String().
func slogRenderAttrs(v slog.LogValuer) string {
	var buf bytes.Buffer
	handler := slog.NewTextHandler(&buf, nil)
	logger := slog.New(handler)
	logger.Info("test", "credential", v)
	return buf.String()
}
