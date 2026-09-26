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
	pool := testPool(t)
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
	run := func(in webhookauth.Inbound) ([]string, error) {
		var captured *recordingTx
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			captured = newRecordingTx(tx)
			_, err := orch.ReceiveCallback(ctx, captured, f.tenantID, "mock-casino", in)
			return err
		})
		return captured.Statements(), err
	}

	t.Run("bad signature: exactly the handle read", func(t *testing.T) {
		wrong := make([]byte, 32)
		_, _ = rand.Read(wrong)
		stmts, err := run(signed(webhookauth.MockKeyID, wrong))
		var authErr *webhookauth.AuthError
		if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonSignatureInvalid {
			t.Fatalf("want signature_invalid, got %v", err)
		}
		if len(stmts) != 1 || stmts[0] != providercred.HandleReadSQL {
			t.Fatalf("pre-verification statements = %q, want exactly [HandleReadSQL]", stmts)
		}
	})
	t.Run("unknown key id: exactly the handle read", func(t *testing.T) {
		stmts, err := run(signed("mock-v9", secret))
		var authErr *webhookauth.AuthError
		if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonCredentialUnavailable {
			t.Fatalf("want credential_unavailable, got %v", err)
		}
		if len(stmts) != 1 || stmts[0] != providercred.HandleReadSQL {
			t.Fatalf("pre-verification statements = %q, want exactly [HandleReadSQL]", stmts)
		}
	})
	t.Run("verified: the handle read, then dispatch", func(t *testing.T) {
		stmts, err := run(signed(webhookauth.MockKeyID, secret))
		var authErr *webhookauth.AuthError
		if errors.As(err, &authErr) {
			t.Fatalf("verification must succeed with the real resolver, got %v", err)
		}
		if len(stmts) < 2 || stmts[0] != providercred.HandleReadSQL ||
			!strings.Contains(stmts[1], "pg_advisory_xact_lock") || !strings.Contains(stmts[1], "casino_bet_delivery") {
			t.Fatalf("statements = %q, want [HandleReadSQL, L0.1 lock, ...]", stmts)
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
		return pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := orch.ReceiveCallback(ctx, tx, f.tenantID, "mock-casino", in)
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
