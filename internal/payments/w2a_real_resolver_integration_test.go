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
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func TestPointNineCapture_Payments_AllowsExactlyOneHandleRead(t *testing.T) {
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
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
	// ADR 0094 §4.1/§9.3 test 9: phase 1 (VerifyCallback, no transaction
	// held) runs its reads in READ ONLY transactions; phase 2's domain
	// transaction starts with HandleRecheckSQL.
	run := func(in InboundCallback) ([]phasecapture.Transaction, []string, error) {
		reader := phasecapture.NewReader(pool)
		v, err := orch.VerifyCallback(context.Background(), reader, f.tenantID, "mock-psp", in)
		if err != nil {
			return reader.Transactions(), nil, err
		}
		var captured *phasecapture.Tx
		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			captured = phasecapture.NewTx(tx)
			_, err := orch.ReceiveVerifiedCallback(ctx, captured, f.tenantID, "mock-psp", v)
			return err
		})
		return reader.Transactions(), captured.Statements(), err
	}
	assertPreVerification := func(t *testing.T, pre []phasecapture.Transaction) {
		t.Helper()
		if len(pre) != 2 || len(pre[0].Statements) != 1 || !strings.Contains(pre[0].Statements[0], "provider_capabilities") ||
			len(pre[1].Statements) != 1 || pre[1].Statements[0] != providercred.HandleReadSQL {
			t.Fatalf("pre-verification transactions = %+v, want [[ProviderAcceptsWebhook], [HandleReadSQL]]", pre)
		}
		for _, tx := range pre {
			if !tx.ReadOnly {
				t.Fatalf("a pre-verification transaction was not READ ONLY: %+v", tx)
			}
			for _, s := range tx.Statements {
				if writeOrLockPattern.MatchString(s) {
					t.Fatalf("a write or lock ran before verification: %q", s)
				}
			}
		}
	}
	wrong := make([]byte, 32)
	_, _ = rand.Read(wrong)
	for name, in := range map[string]InboundCallback{
		"bad signature":  signed(webhookauth.MockKeyID, wrong),
		"unknown key id": signed("mock-v9", secret),
	} {
		t.Run(name, func(t *testing.T) {
			pre, domain, err := run(in)
			var authErr *CallbackAuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("want an auth error, got %v", err)
			}
			assertPreVerification(t, pre)
			if domain != nil {
				t.Fatalf("a failed verification must not open the domain transaction, ran %q", domain)
			}
		})
	}
	t.Run("verified", func(t *testing.T) {
		pre, domain, err := run(signed(webhookauth.MockKeyID, secret))
		var authErr *CallbackAuthError
		if errors.As(err, &authErr) {
			t.Fatalf("verification must succeed with the real resolver, got %v", err)
		}
		assertPreVerification(t, pre)
		if len(domain) < 2 || domain[0] != providercred.HandleRecheckSQL {
			t.Fatalf("domain statements = %q, want HandleRecheckSQL first", domain)
		}
		for _, s := range domain {
			if s == providercred.HandleReadSQL {
				t.Fatal("the handle read must not run in the domain transaction")
			}
		}
	})
}
