//go:build integration

// Stage 10.3 W2a (ADR 0022 §3 point 9 as amended): with the REAL resolver
// wired, payments keeps its point-4 read ProviderAcceptsWebhook, plus the
// resolver's pinned handle read, and nothing else before verification.
package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providercred/providercredtest"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func TestPointNineCapture_Payments_AllowsExactlyOneHandleRead(t *testing.T) {
	pool := testPool(t)
	f := seedOrchFixture(t, pool)
	provider := NewMockProvider("mock-psp", "EUR")
	registerCapability(t, pool, f, provider, 100)

	mem := memstore.New()
	router, err := memstore.NewRouter(mem)
	if err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	sub, err := providercred.New(config.Config{ProviderCredentialFingerprintKey: config.NewSecretValue(hex.EncodeToString(key))},
		router, providercred.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil || sub == nil {
		t.Fatalf("subsystem: %v", err)
	}
	principals := providercredtest.SeedPrincipals(t, pool)
	_, secret := providercredtest.Register(t, pool, sub, mem.Put, principals, providercredtest.Spec{
		TenantID: f.tenantID, Domain: "payments", ProviderID: "mock-psp", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	orch := NewOrchestrator(map[string]PaymentProvider{"mock-psp": provider}, sub.Resolver("payments"))

	signed := func(keyID string, k []byte) InboundCallback {
		in := provider.CallbackPayload(f.tenantID, CallbackEventDeposit, "p9-ref", "", OutcomeSucceeded, 1000, "EUR", "", false)
		in.Header = in.Header.Clone()
		webhookauth.PaymentsScheme().SetHeaders(in.Header, keyID, webhookauth.PaymentsScheme().Sign(k, f.tenantID, "mock-psp", keyID, in.Body))
		return in
	}
	run := func(in InboundCallback) ([]string, error) {
		var captured *recordingTx
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			captured = newRecordingTx(tx)
			_, err := orch.ReceiveCallback(ctx, captured, f.tenantID, "mock-psp", in)
			return err
		})
		return captured.Statements(), err
	}
	wrong := make([]byte, 32)
	_, _ = rand.Read(wrong)
	for name, in := range map[string]InboundCallback{
		"bad signature":  signed(webhookauth.MockKeyID, wrong),
		"unknown key id": signed("mock-v9", secret),
	} {
		t.Run(name, func(t *testing.T) {
			stmts, err := run(in)
			var authErr *CallbackAuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("want an auth error, got %v", err)
			}
			if len(stmts) != 2 || !strings.Contains(stmts[0], "provider_capabilities") || stmts[1] != providercred.HandleReadSQL {
				t.Fatalf("pre-verification statements = %q, want [ProviderAcceptsWebhook, HandleReadSQL]", stmts)
			}
			for _, s := range stmts {
				if writeOrLockPattern.MatchString(s) {
					t.Fatalf("a write or lock ran before verification: %q", s)
				}
			}
		})
	}
	t.Run("verified", func(t *testing.T) {
		stmts, err := run(signed(webhookauth.MockKeyID, secret))
		var authErr *CallbackAuthError
		if errors.As(err, &authErr) {
			t.Fatalf("verification must succeed with the real resolver, got %v", err)
		}
		if len(stmts) < 3 || stmts[1] != providercred.HandleReadSQL {
			t.Fatalf("statements = %q", stmts)
		}
	})
}
