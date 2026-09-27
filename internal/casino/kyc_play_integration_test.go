//go:build integration

// ADR 0096 (PRH-I3) call-site tests: postBet's KYC "play" gate (code
// review rv-prh-i3-code-review.md T1/T2/R1 - "zero tests with an active
// play policy on a licensed tenant"). Every other casino fixture in this
// package uses an unlicensed tenant, so the gate short-circuits to
// not_required before ever reading a policy row; these tests license the
// tenant and activate a real 'play' policy so the deny/allow branches are
// actually exercised at THIS call site, not only inside internal/kyc.
package casino

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

func mustLicenseCasinoTenant(t *testing.T, pool *db.Pool, f casinoFixture) uuid.UUID {
	t.Helper()
	jurisdictionID := mustSeedCasinoJurisdiction(t, pool)
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		lic, err := jurisdiction.CreateLicence(ctx, tx, jurisdiction.CreateLicenceParams{
			JurisdictionID: jurisdictionID, Licensee: "platform", LicenceNumber: "LIC-" + f.tenantID.String()[:8],
			Actor: jurisdiction.ActorContext{ActorID: uuid.New(), ReasonCode: "test"},
		})
		if err != nil {
			return err
		}
		_, err = jurisdiction.AssignTenantLicence(ctx, tx, jurisdiction.AssignTenantLicenceParams{
			TenantID: f.tenantID, LicenceID: &lic.ID, Actor: jurisdiction.ActorContext{ActorID: uuid.New(), ReasonCode: "test"},
		})
		return err
	})
	if err != nil {
		t.Fatalf("license casino tenant: %v", err)
	}
	return jurisdictionID
}

func mustSeedCasinoJurisdiction(t *testing.T, pool *db.Pool) uuid.UUID {
	t.Helper()
	id := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'Casino KYC Play Test Jurisdiction')`,
			id, "cas-kyc-"+id.String()[:8])
		return err
	})
	if err != nil {
		t.Fatalf("seed jurisdiction: %v", err)
	}
	return id
}

func mustActiveCasinoPlayPolicy(t *testing.T, pool *db.Pool, jurisdictionID uuid.UUID) {
	t.Helper()
	playOp := "casino_play"
	var id uuid.UUID
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		var err error
		id, err = kyc.CreateEnforcementPolicy(ctx, tx, kyc.CreateEnforcementPolicyParams{
			LicensingJurisdictionID: jurisdictionID, TriggerType: "play", PlayOperation: &playOp,
			LegalReviewReference: "test-legal-ref", ReasonCode: "test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("create play policy: %v", err)
	}
	err = pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return kyc.ActivateEnforcementPolicy(ctx, tx, id)
	})
	if err != nil {
		t.Fatalf("activate play policy: %v", err)
	}
}

func mustSeedCasinoVerification(t *testing.T, pool *db.Pool, f casinoFixture, status string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, $6, 'mock')`,
			uuid.New(), f.tenantID, f.brandID, f.playerAccountID, f.personID, status)
		return err
	})
	if err != nil {
		t.Fatalf("seed casino verification: %v", err)
	}
}

func TestReceiveCallback_BetDeniedByKYCPlayPolicy_NoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	jid := mustLicenseCasinoTenant(t, pool, f)
	mustActiveCasinoPlayPolicy(t, pool, jid)
	// seedCasinoFixture already seeds a default APPROVED verification (so
	// every OTHER casino test, none of which test KYC, is unaffected by
	// this gate) - override it here with a NEWER rejected row so the
	// latest-row-wins read (§2.6(a)) actually governs this bet with a
	// denied state, exactly like withdrawal's own kyc_gate tests do.
	time.Sleep(10 * time.Millisecond)
	mustSeedCasinoVerification(t, pool, f, "rejected")

	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-kyc-play-1", "", "round-kyc-play-1", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeDeclined {
		t.Fatalf("expected the bet to be declined by the active KYC play policy, got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 5000 {
		t.Fatalf("expected cash balance UNCHANGED at 5000 (a KYC-denied bet must post nothing), got %d", balance)
	}
	debits, credits := sumDebitsCredits(t, pool, f.tenantID)
	if debits != credits {
		t.Fatalf("invariant #1 violated: debits=%d credits=%d", debits, credits)
	}
}

func TestReceiveCallback_BetAllowedByKYCPlayPolicy_WhenApproved(t *testing.T) {
	pool := testPool(t)
	f := seedCasinoFixture(t, pool)
	fundWallet(t, pool, f, 5000)
	jid := mustLicenseCasinoTenant(t, pool, f)
	mustActiveCasinoPlayPolicy(t, pool, jid)
	mustSeedCasinoVerification(t, pool, f, "approved")
	provider := NewMockCasinoProvider("mock-casino", "EUR")
	registerCasinoCapability(t, pool, f, provider, 100)
	sessionID := mintSession(t, pool, f, "mock-casino", "EUR")
	orch := NewOrchestrator(map[string]CasinoProvider{"mock-casino": provider}, NewMockWebhookCredentials(provider))

	payload := provider.CallbackPayload(f.tenantID, CallbackEventBet, "bet-kyc-play-2", "", "round-kyc-play-2", "game-1", 1000, "EUR", OutcomeSucceeded, "", f.playerAccountID, sessionID)
	var result ReceiveCallbackResult
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		result, err = orch.receiveCallbackInTx(ctx, tx, f.tenantID, "mock-casino", payload)
		return err
	})
	if err != nil {
		t.Fatalf("ReceiveCallback: %v", err)
	}
	if result.Outcome != OutcomeSucceeded {
		t.Fatalf("expected the bet to succeed with an approved verification, got %+v", result)
	}
	if balance := cashBalance(t, pool, f); balance != 4000 {
		t.Fatalf("expected cash balance debited to 4000, got %d", balance)
	}
}
