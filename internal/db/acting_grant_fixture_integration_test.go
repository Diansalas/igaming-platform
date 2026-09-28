//go:build integration

// Shared fixture builder for every K1 (ADR 0099) integration test in this
// package: a genuinely in-force G-P2 grant (a platform_admin granted a
// financial capability to act in one specific tenant), built through the
// REAL request -> platform co-approval -> grant flow (internal/capability),
// not by poking rows directly - so every test that uses it is also,
// incidentally, a positive test that the flow itself works end to end.
package db

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
)

// actingGrantFixture is everything mustBuildValidActingGrantFixture built,
// for tests that need more than just (principalID, tenantID) - e.g. to
// also exercise revoke, or to build a second, colliding fixture.
type actingGrantFixture struct {
	TenantID    uuid.UUID
	RequesterID uuid.UUID // a platform_admin (G-P2's requester is always platform - R-6)
	ApproverID  uuid.UUID // a platform_admin, distinct Person from both Requester and Grantee
	GranteeID   uuid.UUID // a platform_admin, the G-P2 grantee
	RequestID   uuid.UUID
	GrantID     uuid.UUID
	Capability  capability.Capability
}

// mustBuildValidActingGrantFixture creates a tenant and three distinct
// platform_admin staff (each with its own distinct Person): a requester,
// an approver and a grantee, then drives a real G-P2 request + platform
// co-approval (ADR 0099 §4's G-P2 row: "platform-originated grant for a
// platform_admin to act in X") to produce one in-force
// staff_capability_grants row. It returns (grantee principal id, tenant
// id) - the exact two arguments db.Pool.WithPlatformActingInTenant needs
// to open a VALID acting session. G-P2, not G-T, is required here because
// WithPlatformActingInTenant's own principal must resolve to an ACTIVE
// platform_admin (financial_acting_session_valid(), migration 0112
// §6.3) - a tenant-scoped finance grantee (G-T) could never open this
// session shape at all.
func mustBuildValidActingGrantFixture(t *testing.T, pool *Pool) (principalID, tenantID uuid.UUID) {
	t.Helper()
	f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
	return f.GranteeID, f.TenantID
}

func mustBuildActingGrantFixtureWithCapability(t *testing.T, pool *Pool, cap capability.Capability) actingGrantFixture {
	t.Helper()
	ctx := context.Background()
	f := actingGrantFixture{Capability: cap}
	f.TenantID = createTestTenant(t, pool)

	requesterPerson := uuid.New()
	granteePerson := uuid.New()
	approverPerson := uuid.New()
	f.RequesterID = uuid.New()
	f.GranteeID = uuid.New()
	f.ApproverID = uuid.New()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		for _, p := range []uuid.UUID{requesterPerson, granteePerson, approverPerson} {
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, p); err != nil {
				return err
			}
		}
		for _, s := range []struct {
			id     uuid.UUID
			person uuid.UUID
		}{
			{f.RequesterID, requesterPerson},
			{f.GranteeID, granteePerson},
			{f.ApproverID, approverPerson},
		} {
			if _, err := tx.Exec(ctx,
				`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
				 VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active', $3)`,
				s.id, "fixture-"+s.id.String()+"@test.invalid", s.person,
			); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("insert persons/staff: %v", err)
	}

	validUntil := time.Now().Add(1 * time.Hour)
	if err := pool.WithPlatformAdmin(ctx, f.RequesterID, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, f.TenantID, capability.NewRequestInput{
			GranteeStaffID: f.GranteeID, Capability: cap,
			ValidFrom: time.Now(), ValidUntil: &validUntil, ReasonCode: "fixture",
		})
		if err != nil {
			return err
		}
		f.RequestID = req.ID
		return nil
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}

	if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		_, grant, err := capability.DecideAndGrant(ctx, tx, f.TenantID, f.RequestID, "approve", "fixture")
		if err != nil {
			return err
		}
		f.GrantID = grant.ID
		return nil
	}); err != nil {
		t.Fatalf("approve request: %v", err)
	}

	return f
}
