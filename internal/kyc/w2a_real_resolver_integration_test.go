//go:build integration

// Stage 10.3 W2a for KYC:
//   - ADR 0022 §3 point 9 (as amended): with the real resolver wired, the
//     only pre-verification statement is the pinned handle read;
//   - O4: player self-service KYC selects its provider from tenant
//     configuration (the tenant's active outbound_api KYC credential) and
//     fails closed when none is configured.
package kyc

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
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providercred/providercredtest"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func realKYCSubsystem(t *testing.T) (*providercred.Subsystem, *memstore.Store) {
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

func TestPointNineCapture_KYC_AllowsExactlyOneHandleRead(t *testing.T) {
	pool := testPool(t)
	f, provider, _, verificationID := newWebhookFixture(t, pool)
	sub, mem := realKYCSubsystem(t)
	principals := providercredtest.SeedPrincipals(t, pool)
	_, secret := providercredtest.Register(t, pool, sub, mem.Put, principals, providercredtest.Spec{
		TenantID: f.tenantID, Domain: "kyc", ProviderID: "mock", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	orch := NewOrchestrator(map[string]KYCProvider{"mock": provider}, sub.Resolver("kyc"))
	var ref string
	_ = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	})
	signed := func(keyID string, key []byte) webhookauth.Inbound {
		in := provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto_approved")
		in.Header = in.Header.Clone()
		webhookauth.KYCScheme().SetHeaders(in.Header, keyID, webhookauth.KYCScheme().Sign(key, f.tenantID, "mock", keyID, in.Body))
		return in
	}
	run := func(in webhookauth.Inbound) ([]string, error) {
		var captured *recordingTx
		err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			captured = newRecordingTx(tx)
			_, _, err := orch.ReceiveCallback(ctx, captured, f.tenantID, "mock", in)
			return err
		})
		return captured.Statements(), err
	}
	wrong := make([]byte, 32)
	_, _ = rand.Read(wrong)
	for name, in := range map[string]webhookauth.Inbound{
		"bad signature":  signed(webhookauth.MockKeyID, wrong),
		"unknown key id": signed("mock-v9", secret),
	} {
		t.Run(name, func(t *testing.T) {
			stmts, err := run(in)
			var authErr *webhookauth.AuthError
			if !errors.As(err, &authErr) {
				t.Fatalf("want an auth error, got %v", err)
			}
			if len(stmts) != 1 || stmts[0] != providercred.HandleReadSQL {
				t.Fatalf("pre-verification statements = %q, want exactly [HandleReadSQL]", stmts)
			}
		})
	}
	t.Run("verified", func(t *testing.T) {
		stmts, err := run(signed(webhookauth.MockKeyID, secret))
		if err != nil {
			t.Fatalf("verification must succeed with the real resolver: %v", err)
		}
		if len(stmts) < 2 || stmts[0] != providercred.HandleReadSQL {
			t.Fatalf("statements = %q, want HandleReadSQL first", stmts)
		}
		for _, s := range stmts[1:] {
			if s == providercred.HandleReadSQL {
				t.Fatal("the handle read must run exactly once")
			}
		}
	})
}

// renamedKYC is a second synthetic KYC adapter under another provider id.
type renamedKYC struct {
	*MockKYCProvider
	id string
}

func (r renamedKYC) ID() string { return r.id }

func registerKYCOutbound(t *testing.T, pool *db.Pool, sub *providercred.Subsystem, mem *memstore.Store, p providercredtest.Principals, tenant uuid.UUID, provider string) providercred.Handle {
	t.Helper()
	h, _ := providercredtest.Register(t, pool, sub, mem.Put, p, providercredtest.Spec{
		TenantID: tenant, Domain: "kyc", ProviderID: provider, Purpose: providercred.PurposeOutboundAPI, KeyID: "api-" + provider,
	})
	return h
}

func selectFor(t *testing.T, pool *db.Pool, orch *Orchestrator, tenant uuid.UUID) (KYCProvider, error) {
	t.Helper()
	var p KYCProvider
	err := pool.WithTenant(context.Background(), tenant, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		p, err = orch.SelectProvider(ctx, tx, tenant)
		return err
	})
	return p, err
}

// TestO4_KYCProviderSelectionFromTenantConfiguration (QA W2a: "two tenants
// configured for different (mock) providers").
func TestO4_KYCProviderSelectionFromTenantConfiguration(t *testing.T) {
	pool := testPool(t)
	sub, mem := realKYCSubsystem(t)
	principals := providercredtest.SeedPrincipals(t, pool)
	a, b, none, both := seedFixture(t, pool), seedFixture(t, pool), seedFixture(t, pool), seedFixture(t, pool)
	two := NewOrchestrator(map[string]KYCProvider{
		"mock":   NewMockKYCProvider(),
		"mock-b": renamedKYC{MockKYCProvider: NewMockKYCProvider(), id: "mock-b"},
	}, nil)

	registerKYCOutbound(t, pool, sub, mem, principals, a.tenantID, "mock")
	hb := registerKYCOutbound(t, pool, sub, mem, principals, b.tenantID, "mock-b")
	registerKYCOutbound(t, pool, sub, mem, principals, both.tenantID, "mock")
	registerKYCOutbound(t, pool, sub, mem, principals, both.tenantID, "mock-b")

	if p, err := selectFor(t, pool, two, a.tenantID); err != nil || p.ID() != "mock" {
		t.Fatalf("tenant A must get its configured provider mock: %v %v", p, err)
	}
	if p, err := selectFor(t, pool, two, b.tenantID); err != nil || p.ID() != "mock-b" {
		t.Fatalf("tenant B must get its configured provider mock-b: %v %v", p, err)
	}
	if _, err := selectFor(t, pool, two, none.tenantID); !errors.Is(err, ErrNoKYCProviderConfigured) {
		t.Fatalf("no configuration with several providers must fail closed, got %v", err)
	}
	if _, err := selectFor(t, pool, two, both.tenantID); !errors.Is(err, ErrKYCProviderAmbiguous) {
		t.Fatalf("two configured providers must fail closed, got %v", err)
	}
	// Revoking B's credential removes its configuration: fail closed.
	providercredtest.Revoke(t, pool, b.tenantID, hb.ID, principals.Requester)
	if _, err := selectFor(t, pool, two, b.tenantID); !errors.Is(err, ErrNoKYCProviderConfigured) {
		t.Fatalf("a revoked configuration must fail closed, got %v", err)
	}
	// A single registered SYNTHETIC adapter needs no credential (test-support
	// deployments; refused in production by the ADR 0085 guard).
	single := NewOrchestrator(map[string]KYCProvider{"mock": NewMockKYCProvider()}, nil)
	if p, err := selectFor(t, pool, single, none.tenantID); err != nil || p.ID() != "mock" {
		t.Fatalf("the lone synthetic adapter must be selected: %v %v", p, err)
	}
	// The selection query is tenant-predicated and read-only.
	if !strings.Contains(configuredKYCProvidersSQL, "tenant_id = $1") || strings.Contains(strings.ToUpper(configuredKYCProvidersSQL), "FOR UPDATE") {
		t.Fatal("the selection read must be tenant-predicated and lock-free")
	}
}
