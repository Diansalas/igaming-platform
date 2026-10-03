//go:build integration

// ADR 0096 (PRH-I3) call-site tests: PlaceBet's KYC "play" gate, mirroring
// internal/casino/kyc_play_integration_test.go exactly (code review
// rv-prh-i3-code-review.md T1/T2/R1, security F2). Every other sportsbook
// fixture uses an unlicensed tenant, so the gate short-circuits to
// not_required before ever reading a policy row.
package sportsbook

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/kyc"
)

func mustLicenseSBTenant(t *testing.T, pool *db.Pool, f sbFixture) uuid.UUID {
	t.Helper()
	jurisdictionID := uuid.New()
	err := pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO jurisdictions (id, code, name) VALUES ($1, $2, 'SB KYC Play Test Jurisdiction')`,
			jurisdictionID, "sb-kyc-"+jurisdictionID.String()[:8]); err != nil {
			return err
		}
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
		t.Fatalf("license sportsbook tenant: %v", err)
	}
	return jurisdictionID
}

func mustActiveSportsbookPlayPolicy(t *testing.T, pool *db.Pool, jurisdictionID uuid.UUID) {
	t.Helper()
	playOp := "sportsbook_play"
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

func mustSeedSBVerification(t *testing.T, pool *db.Pool, f sbFixture, personID uuid.UUID, status string) {
	t.Helper()
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO kyc_verifications (id, tenant_id, brand_id, player_account_id, person_id, status, provider_id)
			 VALUES ($1, $2, $3, $4, $5, $6, 'mock')`,
			uuid.New(), f.tenantID, f.brandID, f.playerAccountID, personID, status)
		return err
	})
	if err != nil {
		t.Fatalf("seed sportsbook verification: %v", err)
	}
}

func countRows(t *testing.T, pool *db.Pool, tenantID uuid.UUID, query string, args ...any) int {
	t.Helper()
	var n int
	err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, query, args...).Scan(&n)
	})
	if err != nil {
		t.Fatalf("count query %q: %v", query, err)
	}
	return n
}

// sbPersonID resolves the Person id backing f.playerAccountID (sbFixture
// carries no personID field of its own).
func sbPersonID(t *testing.T, pool *db.Pool, f sbFixture) uuid.UUID {
	t.Helper()
	var personID uuid.UUID
	err := pool.WithTenant(context.Background(), f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM player_accounts WHERE id = $1`, f.playerAccountID).Scan(&personID)
	})
	if err != nil {
		t.Fatalf("resolve person id: %v", err)
	}
	return personID
}

func TestPlaceBet_DeniedByKYCPlayPolicy_NoLedgerEffect(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	jid := mustLicenseSBTenant(t, pool, f)
	mustActiveSportsbookPlayPolicy(t, pool, jid)
	personID := sbPersonID(t, pool, f)
	mustSeedSBVerification(t, pool, f, personID, "rejected")
	sel := seedSelection(t, pool, seedSelectionParams{OddsNumerator: 250, OddsDenominator: 100})

	result, err := placeBet(t, pool, f, sel, 1_000, "place-kyc-deny-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if result.Accepted {
		t.Fatal("expected the bet to be rejected by the active KYC play policy")
	}
	if result.RejectionCategory != RejectionKYCDenied {
		t.Fatalf("expected RejectionKYCDenied, got %q (%q)", result.RejectionCategory, result.RejectionMessage)
	}

	// MPLAYREC (code review rv-prh-i3-code-review.md, KYC-ENF-TESTPINS-1),
	// mutated on the sportsbook path too, per instruction: a REAL
	// evaluation (an active play policy exists) must write a
	// kyc_enforcement_decisions row for the deny, exactly like
	// internal/casino's own play-deny path and withdrawal's own deny path.
	decisionCount := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM kyc_enforcement_decisions WHERE tenant_id = $1 AND player_account_id = $2 AND operation = 'sportsbook_play' AND allowed = false`,
		f.tenantID, f.playerAccountID)
	if decisionCount != 1 {
		t.Fatalf("expected exactly 1 kyc_enforcement_decisions row for the denied sportsbook_play evaluation, got %d", decisionCount)
	}
	auditCount := countRows(t, pool, f.tenantID,
		`SELECT count(*) FROM audit_log WHERE tenant_id = $1 AND action = 'kyc.enforcement_denied' AND target_id = $2`,
		f.tenantID, f.playerAccountID.String())
	if auditCount != 1 {
		t.Fatalf("expected exactly 1 kyc.enforcement_denied audit row, got %d", auditCount)
	}
}

func TestPlaceBet_AllowedByKYCPlayPolicy_WhenApproved(t *testing.T) {
	pool := testPool(t)
	f := seedFixture(t, pool)
	fundWallet(t, pool, f, 10_000)
	jid := mustLicenseSBTenant(t, pool, f)
	mustActiveSportsbookPlayPolicy(t, pool, jid)
	personID := sbPersonID(t, pool, f)
	mustSeedSBVerification(t, pool, f, personID, "approved")
	sel := seedSelection(t, pool, seedSelectionParams{OddsNumerator: 250, OddsDenominator: 100})

	result, err := placeBet(t, pool, f, sel, 1_000, "place-kyc-allow-1")
	if err != nil {
		t.Fatalf("place bet: %v", err)
	}
	if !result.Accepted {
		t.Fatalf("expected the bet to be accepted with an approved verification, got rejection %q/%q: %s", result.RejectionCategory, result.RejectionCode, result.RejectionMessage)
	}
}
