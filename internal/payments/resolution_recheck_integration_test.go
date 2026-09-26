//go:build integration

// ADR 0094 §5 / §9.3 test 8 (payments variant) and security condition C3
// (the verified bytes are a private copy), with the REAL resolver, on the
// ADR 0094 pool-10 fixture (QA §11 items 1 and 8).
package payments

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/config"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providercred/providercredtest"
	"github.com/Diansalas/igaming-platform/internal/secretstore/memstore"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

type payRecheckWorld struct {
	pool       *db.Pool
	f          orchFixture
	provider   *MockProvider
	orch       *Orchestrator
	handle     providercred.Handle
	secret     []byte
	principals providercredtest.Principals
}

func newPayRecheckWorld(t *testing.T) *payRecheckWorld {
	t.Helper()
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
	w := &payRecheckWorld{pool: pool, f: seedOrchFixture(t, pool), provider: NewMockProvider("mock-psp", "EUR")}
	registerCapability(t, pool, w.f, w.provider, 100)
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
	w.principals = providercredtest.SeedPrincipals(t, pool)
	w.handle, w.secret = providercredtest.Register(t, pool, sub, mem.Put, w.principals, providercredtest.Spec{
		TenantID: w.f.tenantID, Domain: "payments", ProviderID: "mock-psp", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	w.orch = NewOrchestrator(map[string]PaymentProvider{"mock-psp": w.provider}, sub.Resolver("payments"))
	return w
}

// deposit creates a pending intent for amount and returns its signed
// success callback.
func (w *payRecheckWorld) deposit(t *testing.T, key string, amount int64) InboundCallback {
	t.Helper()
	var intent DepositIntent
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		intent, err = w.orch.InitiateDeposit(ctx, tx, InitiateDepositParams{
			Scope:     DepositScope{TenantID: w.f.tenantID, BrandID: w.f.brandID, PlayerAccountID: w.f.playerAccountID, WalletID: w.f.walletID},
			AssetCode: "EUR", Amount: amount, PaymentMethod: "card", IdempotencyKey: key,
		})
		return err
	}); err != nil {
		t.Fatalf("InitiateDeposit: %v", err)
	}
	in := w.provider.CallbackPayload(w.f.tenantID, CallbackEventDeposit, *intent.ProviderReference, "", OutcomeSucceeded, amount, "EUR", "", false)
	in.Header = in.Header.Clone()
	webhookauth.PaymentsScheme().SetHeaders(in.Header, webhookauth.MockKeyID,
		webhookauth.PaymentsScheme().Sign(w.secret, w.f.tenantID, "mock-psp", webhookauth.MockKeyID, in.Body))
	return in
}

func (w *payRecheckWorld) receive(v *VerifiedCallback) (ReceiveCallbackResult, error) {
	var res ReceiveCallbackResult
	err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		res, err = w.orch.ReceiveVerifiedCallback(ctx, tx, w.f.tenantID, "mock-psp", v)
		return err
	})
	return res, err
}

// TestReceiveVerified_RevokedBetweenVerifyAndDomainTx_Payments is ADR 0094
// §9.3 test 8 (payments): revoked between the phases is rejected with no
// posting; rotated to verify_only inside its window is accepted and posts
// once.
func TestReceiveVerified_RevokedBetweenVerifyAndDomainTx_Payments(t *testing.T) {
	t.Run("revoked", func(t *testing.T) {
		w := newPayRecheckWorld(t)
		v, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, "mock-psp", w.deposit(t, "rv-1", 5000))
		if err != nil {
			t.Fatal(err)
		}
		providercredtest.Revoke(t, w.pool, w.f.tenantID, w.handle.ID, w.principals.Requester)
		_, err = w.receive(v)
		var authErr *CallbackAuthError
		if !errors.As(err, &authErr) || authErr.Reason != ReasonCredentialUnavailable {
			t.Fatalf("want credential_unavailable, got %v", err)
		}
		if b := cashBalance(t, w.pool, w.f); b != 0 {
			t.Fatalf("a rejected callback posted: balance %d", b)
		}
	})
	t.Run("verify_only inside its window", func(t *testing.T) {
		w := newPayRecheckWorld(t)
		v, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, "mock-psp", w.deposit(t, "rv-2", 5000))
		if err != nil {
			t.Fatal(err)
		}
		na := time.Now().Add(time.Hour)
		if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := providercred.TransitionHandle(ctx, tx, w.handle.ID, providercred.ActionVerifyOnly, &na, providercred.TransitionReasonRotation,
				w.principals.Requester, providercred.AuditContext{ActorID: w.principals.Requester})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.receive(v); err != nil {
			t.Fatalf("a verify_only handle inside its window must still be accepted: %v", err)
		}
		if b := cashBalance(t, w.pool, w.f); b != 5000 {
			t.Fatalf("balance %d, want exactly one posting of 5000", b)
		}
	})
}

// TestVerifyCallback_BodyMutationBetweenPhases is security condition C3:
// VerifyCallback verifies and keeps a private copy, so mutating the
// caller's body/headers after phase 1 changes nothing phase 2 handles -
// the verified amount is what posts.
func TestVerifyCallback_BodyMutationBetweenPhases(t *testing.T) {
	w := newPayRecheckWorld(t)
	in := w.deposit(t, "mut-1", 5000)
	v, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, "mock-psp", in)
	if err != nil {
		t.Fatal(err)
	}
	// Tamper with every byte of the caller's slice and header map.
	for i := range in.Body {
		in.Body[i] = ' '
	}
	in.Header.Set(webhookauth.PaymentsSignatureHeader, "v1=00")
	res, err := w.receive(v)
	if err != nil {
		t.Fatalf("phase 2 must handle the verified copy, not the mutated caller bytes: %v", err)
	}
	if res.Status != DepositIntentSucceeded {
		t.Fatalf("status %v", res.Status)
	}
	if b := cashBalance(t, w.pool, w.f); b != 5000 {
		t.Fatalf("balance %d, want the verified 5000", b)
	}
}

// TestVerifyCallback_InsideTxRefused (ADR 0094 §4.1 guard): phase 1 called
// while the caller holds a pooled transaction fails closed as
// credential_unavailable before any read or store call - the pre-ADR-0094
// shape can no longer pin a connection.
func TestVerifyCallback_InsideTxRefused(t *testing.T) {
	w := newPayRecheckWorld(t)
	in := w.deposit(t, "guard-1", 100)
	var err error
	var nested int64
	_ = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, _ pgx.Tx) error {
		acq := w.pool.Raw().Stat().AcquireCount()
		_, err = w.orch.VerifyCallback(ctx, w.pool, w.f.tenantID, "mock-psp", in)
		nested = w.pool.Raw().Stat().AcquireCount() - acq
		return nil
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != ReasonCredentialUnavailable {
		t.Fatalf("VerifyCallback inside a held transaction must fail closed as credential_unavailable, got %v", err)
	}
	if nested != 0 {
		t.Fatalf("a refused VerifyCallback acquired %d nested connections", nested)
	}
	// Outside a transaction the same callback verifies.
	if _, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, "mock-psp", in); err != nil {
		t.Fatalf("VerifyCallback with no transaction held: %v", err)
	}
}
