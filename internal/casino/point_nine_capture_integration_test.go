//go:build integration

// Stage 10.3 W2a (ADR 0022 §3 point 9 as amended; QA W2a plan): with the
// REAL resolver (internal/providercred) wired, casino's ReceiveCallback may
// run exactly ONE statement before verification - the resolver's pinned,
// lock-free, read-only, tenant-predicated handle read - and nothing else.
// Any other pre-verification statement must still fail this capture.
package casino

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providercred/providercredtest"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func realCredentialSubsystem(t *testing.T) (*providercred.Subsystem, *memstore.Store) {
	t.Helper()
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
	return sub, mem
}

func TestPointNineCapture_Casino_AllowsExactlyOneHandleRead(t *testing.T) {
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sub, mem := realCredentialSubsystem(t)
	principals := providercredtest.SeedPrincipals(t, pool)
	_, secret := providercredtest.Register(t, pool, sub, mem.Put, principals, providercredtest.Spec{
		TenantID: f.tenantID, Domain: "casino", ProviderID: "mock-casino", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, sub.Resolver("casino"))

	signed := func(keyID string, key []byte) webhookauth.Inbound {
		in := provider.CallbackPayload(f.tenantID, CallbackEventBet, "cas-p9-"+uuid.NewString()[:8], "", "round-p9", "game-1",
			1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())
		in.Header = in.Header.Clone()
		webhookauth.CasinoScheme().SetHeaders(in.Header, keyID, webhookauth.CasinoScheme().Sign(key, f.tenantID, "mock-casino", keyID, in.Body))
		return in
	}
	// ADR 0094 §4.1/§9.3 test 9: phase 1 (VerifyCallback, no transaction
	// held) runs exactly the handle read in one READ ONLY transaction;
	// phase 2's domain transaction starts with HandleRecheckSQL.
	run := func(in webhookauth.Inbound) ([]phasecapture.Transaction, []string, error) {
		reader := phasecapture.NewReader(pool)
		v, err := orch.VerifyCallback(context.Background(), reader, f.tenantID, "mock-casino", in)
		if err != nil {
			return reader.Transactions(), nil, err
		}
		var captured *phasecapture.Tx
		err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			captured = phasecapture.NewTx(tx)
			_, err := orch.ReceiveVerifiedCallback(ctx, captured, f.tenantID, "mock-casino", v)
			return err
		})
		return reader.Transactions(), captured.Statements(), err
	}
	assertPreVerification := func(t *testing.T, pre []phasecapture.Transaction) {
		t.Helper()
		if len(pre) != 1 || len(pre[0].Statements) != 1 || pre[0].Statements[0] != providercred.HandleReadSQL || !pre[0].ReadOnly {
			t.Fatalf("pre-verification transactions = %+v, want exactly one READ ONLY [HandleReadSQL]", pre)
		}
	}

	t.Run("bad signature: exactly the handle read", func(t *testing.T) {
		wrong := make([]byte, 32)
		_, _ = rand.Read(wrong)
		pre, domain, err := run(signed(webhookauth.MockKeyID, wrong))
		var authErr *webhookauth.AuthError
		if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
			t.Fatalf("want signature_invalid, got %v", err)
		}
		assertPreVerification(t, pre)
		if domain != nil {
			t.Fatalf("a failed verification must not open the domain transaction, ran %q", domain)
		}
	})
	t.Run("unknown key id: exactly the handle read", func(t *testing.T) {
		pre, domain, err := run(signed("mock-v9", secret))
		var authErr *webhookauth.AuthError
		if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonCredentialUnavailable {
			t.Fatalf("want credential_unavailable, got %v", err)
		}
		assertPreVerification(t, pre)
		if domain != nil {
			t.Fatalf("a failed verification must not open the domain transaction, ran %q", domain)
		}
	})
	t.Run("verified: the handle read, then the re-check, then dispatch", func(t *testing.T) {
		pre, stmts, err := run(signed(webhookauth.MockKeyID, secret))
		var authErr *webhookauth.AuthError
		if errors.As(err, &authErr) {
			t.Fatalf("verification must succeed with the real resolver, got %v", err)
		}
		assertPreVerification(t, pre)
		if len(stmts) < 2 || stmts[0] != providercred.HandleRecheckSQL ||
			!strings.Contains(stmts[1], "pg_advisory_xact_lock") || !strings.Contains(stmts[1], "casino_bet_delivery") {
			t.Fatalf("domain statements = %q, want [HandleRecheckSQL, L0.1 lock, ...]", stmts)
		}
	})
}

// TestReceiveCallback_RealResolver_RevocationImmediate: a casino callback
// verifies with the real resolver, and after a single-actor revoke the very
// next callback fails closed (credential revocation is the casino emergency
// stop, R9/C14).
func TestReceiveCallback_RealResolver_RevocationImmediate(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sub, mem := realCredentialSubsystem(t)
	principals := providercredtest.SeedPrincipals(t, pool)
	h, secret := providercredtest.Register(t, pool, sub, mem.Put, principals, providercredtest.Spec{
		TenantID: f.tenantID, Domain: "casino", ProviderID: "mock-casino", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, sub.Resolver("casino"))
	call := func() error {
		in := provider.CallbackPayload(f.tenantID, CallbackEventBet, "cas-rev-"+uuid.NewString()[:8], "", "round-rev", "game-1",
			1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, uuid.New())
		in.Header = in.Header.Clone()
		webhookauth.CasinoScheme().SetHeaders(in.Header, webhookauth.MockKeyID,
			webhookauth.CasinoScheme().Sign(secret, f.tenantID, "mock-casino", webhookauth.MockKeyID, in.Body))
		v, err := orch.VerifyCallback(context.Background(), pool, f.tenantID, "mock-casino", in)
		if err != nil {
			return err
		}
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveVerifiedCallback(ctx, tx, f.tenantID, "mock-casino", v)
			return err
		})
	}
	var authErr *webhookauth.AuthError
	if err := call(); errors.As(err, &authErr) {
		t.Fatalf("before revocation the callback must verify, got %v", err)
	}
	providercredtest.Revoke(t, pool, f.tenantID, h.ID, principals.Requester)
	if err := call(); !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonCredentialUnavailable {
		t.Fatalf("after revocation the next callback must fail closed, got %v", err)
	}
}
