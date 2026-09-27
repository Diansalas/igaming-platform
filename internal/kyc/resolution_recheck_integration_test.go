//go:build integration

// ADR 0094 §5 / §9.3 test 8 (KYC variant), with the REAL resolver, on the
// ADR 0094 pool-10 fixture (QA §11 items 1 and 8): a handle revoked
// between VerifyCallback and the domain transaction is rejected by the
// re-check with no status change and no audit row.
package kyc

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"

	"github.com/Diansalas/igaming-platform/internal/providercred"
	"github.com/Diansalas/igaming-platform/internal/providercred/providercredtest"
	"github.com/Diansalas/igaming-platform/internal/testsupport/phasecapture"
	"github.com/Diansalas/igaming-platform/internal/webhookauth"
)

func TestReceiveVerified_RevokedBetweenVerifyAndDomainTx_KYC(t *testing.T) {
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
	f, provider, _, verificationID := newWebhookFixture(t, pool)
	sub, mem := realKYCSubsystem(t)
	principals := providercredtest.SeedPrincipals(t, pool)
	h, secret := providercredtest.Register(t, pool, sub, mem.Put, principals, providercredtest.Spec{
		TenantID: f.tenantID, Domain: "kyc", ProviderID: "mock", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	orch := NewOrchestrator(map[string]KYCProvider{"mock": provider}, sub.Resolver("kyc"))
	var ref string
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&ref)
	}); err != nil {
		t.Fatal(err)
	}
	in := provider.CallbackPayload(f.tenantID, ref, ProviderApproved, "auto_approved")
	in.Header = in.Header.Clone()
	webhookauth.KYCScheme().SetHeaders(in.Header, webhookauth.MockKeyID, webhookauth.KYCScheme().Sign(secret, f.tenantID, "mock", webhookauth.MockKeyID, in.Body))

	statusBefore := mustGetStatus(t, pool, f.tenantID, verificationID)
	auditBefore := mustCountAudit(t, pool, f.tenantID, verificationID)
	v, err := orch.VerifyCallback(context.Background(), pool, f.tenantID, "mock", in)
	if err != nil {
		t.Fatalf("phase 1 must verify: %v", err)
	}
	providercredtest.Revoke(t, pool, f.tenantID, h.ID, principals.Requester)
	err = pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := orch.ReceiveVerifiedCallback(ctx, tx, f.tenantID, "mock", v)
		return err
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonCredentialUnavailable {
		t.Fatalf("want credential_unavailable, got %v", err)
	}
	if s := mustGetStatus(t, pool, f.tenantID, verificationID); s != statusBefore {
		t.Fatalf("status moved %s -> %s on a rejected callback", statusBefore, s)
	}
	if n := mustCountAudit(t, pool, f.tenantID, verificationID); n != auditBefore {
		t.Fatalf("a rejected callback wrote %d audit rows", n-auditBefore)
	}
}

// kycRealWorld is a KYC verification with a real handle-backed credential
// on the ADR 0094 pool-10 fixture.
type kycRealWorld struct {
	pool           *db.Pool
	f              fixture
	provider       *MockKYCProvider
	orch           *Orchestrator
	secret         []byte
	verificationID uuid.UUID
	ref            string
	calls          func() int64
	logs           *bytes.Buffer
	logMu          *sync.Mutex
}

func newKYCRealWorld(t *testing.T) *kycRealWorld {
	t.Helper()
	pool := phasecapture.Pool10(t, "TEST_DATABASE_URL")
	f, provider, _, verificationID := newWebhookFixture(t, pool)
	sub, mem := realKYCSubsystem(t)
	principals := providercredtest.SeedPrincipals(t, pool)
	_, secret := providercredtest.Register(t, pool, sub, mem.Put, principals, providercredtest.Spec{
		TenantID: f.tenantID, Domain: "kyc", ProviderID: "mock", Purpose: providercred.PurposeWebhookVerify, KeyID: webhookauth.MockKeyID,
	})
	w := &kycRealWorld{pool: pool, f: f, provider: provider, secret: secret, verificationID: verificationID,
		orch:  NewOrchestrator(map[string]KYCProvider{"mock": provider}, sub.Resolver("kyc")),
		calls: mem.Calls, logs: &bytes.Buffer{}, logMu: &sync.Mutex{}}
	w.orch.SetWebhookLogger(slog.New(slog.NewJSONHandler(kycLockedBuf{w.logMu, w.logs}, nil)))
	if err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT provider_reference FROM kyc_verifications WHERE id = $1`, verificationID).Scan(&w.ref)
	}); err != nil {
		t.Fatal(err)
	}
	return w
}

type kycLockedBuf struct {
	mu *sync.Mutex
	b  *bytes.Buffer
}

func (l kycLockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (w *kycRealWorld) approved() webhookauth.Inbound {
	in := w.provider.CallbackPayload(w.f.tenantID, w.ref, ProviderApproved, "auto_approved")
	in.Header = in.Header.Clone()
	webhookauth.KYCScheme().SetHeaders(in.Header, webhookauth.MockKeyID, webhookauth.KYCScheme().Sign(w.secret, w.f.tenantID, "mock", webhookauth.MockKeyID, in.Body))
	return in
}

// TestVerifyCallback_InsideTxRefused_KYC is security review 17, S-2 /
// code review R-4 for KYC.
func TestVerifyCallback_InsideTxRefused_KYC(t *testing.T) {
	w := newKYCRealWorld(t)
	in := w.approved()
	calls := w.calls()
	var err error
	var nested int64
	_ = w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, _ pgx.Tx) error {
		acq := w.pool.Raw().Stat().AcquireCount()
		_, err = w.orch.VerifyCallback(ctx, w.pool, w.f.tenantID, "mock", in)
		nested = w.pool.Raw().Stat().AcquireCount() - acq
		return nil
	})
	var authErr *CallbackAuthError
	if !errors.As(err, &authErr) || authErr.Reason != webhookauth.ReasonCredentialUnavailable {
		t.Fatalf("want credential_unavailable, got %v", err)
	}
	if nested != 0 || w.calls() != calls {
		t.Fatalf("a refused VerifyCallback acquired %d nested connections and made %d store calls", nested, w.calls()-calls)
	}
	w.logMu.Lock()
	n := strings.Count(w.logs.String(), `"entry_point":"kyc.Orchestrator.VerifyCallback"`)
	w.logMu.Unlock()
	if n != 1 {
		t.Fatalf("want exactly one secret_fetch_with_tx_held line from the KYC guard, got %d", n)
	}
	if _, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, "mock", in); err != nil {
		t.Fatalf("VerifyCallback with no transaction held: %v", err)
	}
}

// TestVerifyCallback_BodyMutationBetweenPhases_KYC is security review 17,
// S-1 for KYC: mutating the caller's body after phase 1 changes nothing
// phase 2 applies.
func TestVerifyCallback_BodyMutationBetweenPhases_KYC(t *testing.T) {
	w := newKYCRealWorld(t)
	in := w.approved()
	v, err := w.orch.VerifyCallback(context.Background(), w.pool, w.f.tenantID, "mock", in)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in.Body {
		in.Body[i] = ' '
	}
	in.Header.Set(webhookauth.KYCSignatureHeader, "v1=00")
	var applied bool
	if err := w.pool.WithTenant(context.Background(), w.f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		_, applied, err = w.orch.ReceiveVerifiedCallback(ctx, tx, w.f.tenantID, "mock", v)
		return err
	}); err != nil {
		t.Fatalf("phase 2 must handle the verified copy, not the mutated caller bytes: %v", err)
	}
	if !applied || mustGetStatus(t, w.pool, w.f.tenantID, w.verificationID) != StatusApproved {
		t.Fatalf("the verified approval must apply (applied=%v)", applied)
	}
}
