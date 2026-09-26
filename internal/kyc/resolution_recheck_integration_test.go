//go:build integration

// ADR 0094 §5 / §9.3 test 8 (KYC variant), with the REAL resolver, on the
// ADR 0094 pool-10 fixture (QA §11 items 1 and 8): a handle revoked
// between VerifyCallback and the domain transaction is rejected by the
// re-check with no status change and no audit row.
package kyc

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

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
