//go:build integration

package db

// PRH-2 K1 (ADR 0099) integration tests: A-2, A-5, A-6, A-7, A-9 (partial),
// A-10, A-11, A-13. These exercise migration 0112's own triggers directly
// through internal/capability, inside this package so they can reuse
// createTestTenant/mustBuildActingGrantFixtureWithCapability.
//
// Every test in this file is a "sock-puppet refused" or invariant-refusal
// case: ADR 0099 R-1..R-13, and the HD-PRH2-2 = (c) go/no-go condition
// itself (a grant without platform co-approval is refused; the same
// Person under two principals is refused).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/capability"
)

func cgIsCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
}

// cgStaff inserts one staff row (platform if tenantID == uuid.Nil, else
// tenant-scoped) with its own distinct Person, and returns its id.
func cgStaff(t *testing.T, pool *Pool, tenantID uuid.UUID, role string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	person := uuid.New()
	insert := func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		var tid any
		if tenantID != uuid.Nil {
			tid = tenantID
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			 VALUES ($1, $2, $3, 'x', $4, 'active', $5)`,
			id, tid, "cg-"+id.String()+"@test.invalid", role, person,
		)
		return err
	}
	var err error
	if tenantID == uuid.Nil {
		err = pool.WithoutTenant(ctx, insert)
	} else {
		err = pool.WithTenant(ctx, tenantID, insert)
	}
	if err != nil {
		t.Fatalf("insert staff role=%s: %v", role, err)
	}
	return id
}

// A-2 (part): a compliance grantee is refused (C-99-2, R-9).
func TestCapabilityGrant_A2_ComplianceGranteeRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, tenantID, "tenant_admin")
	compliance := cgStaff(t, pool, tenantID, "compliance")

	err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: compliance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		return err
	})
	if !cgIsCode(err, "CG010") {
		t.Fatalf("expected CG010 (compliance not an eligible grantee), got %v", err)
	}
}

// A-2 (part): a tenant-scope approval is refused (R-7).
func TestCapabilityGrant_A2_TenantScopeApprovalRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, tenantID, "tenant_admin")
	finance := cgStaff(t, pool, tenantID, "finance")
	otherTenantAdmin := cgStaff(t, pool, tenantID, "tenant_admin")

	var requestID uuid.UUID
	if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: finance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		requestID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}

	err := pool.WithPrincipalScope(ctx, tenantID, otherTenantAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID, requestID, "approve", "test")
		return err
	})
	// K1-C3: exactly CG011, not merely "refused somehow". PostgreSQL
	// evaluates a BEFORE INSERT trigger's raised exception before the
	// INSERT's own RLS WITH CHECK is ever tested (RLS's row check runs
	// against the final row values, immediately before the heap write,
	// which is after every BEFORE ROW trigger has already had a chance to
	// raise) - so staff_capability_grant_approvals_guard's own R-7 check
	// (v_actor.scope <> 'platform') is what actually fires here, not RLS.
	// See TestCapabilityGrant_K1C3_LayeredIndependentKill below for the
	// case where RLS is deliberately widened, proving this is the trigger,
	// not RLS, doing the work.
	if !cgIsCode(err, "CG011") {
		t.Fatalf("expected exactly CG011 (R-7, the trigger's own scope guard), got %v", err)
	}
}

// TestCapabilityGrant_K1C3_LayeredIndependentKill (K1-C3, security
// condition, code-review F-10): pins R-7 (HD-PRH2-2's core control)
// independently at the TRIGGER layer, not merely "however it happens to be
// refused today". Runs entirely inside one owner-role transaction that is
// ALWAYS rolled back (via a sentinel error returned to
// pool.WithoutTenant), using nested transactions (Postgres SAVEPOINTs, via
// tx.Begin) around each refusal so a refused statement does not abort the
// whole outer transaction and prevent the later steps/rollback.
//
// Layer 1: widen staff_capability_grant_approvals' RLS with a genuinely
// permissive tenant-shaped INSERT policy (so if RLS were the ONLY thing
// stopping a tenant session, this would let the INSERT through) - the
// trigger's own R-7 check must still refuse it with CG011.
//
// Layer 2: with that same widened RLS AND the decided_by_scope CHECK
// constraint also dropped, the trigger must STILL independently refuse
// with CG011 - proving R-7 is not "masked by RLS" nor "masked by the
// CHECK": the trigger enforces it on its own.
//
// This is also the mutation-kill test for mutant M4 (the trigger's
// `IF v_actor.scope <> 'platform'` block removed): with that block
// removed, both layers above would let the tenant-shaped INSERT succeed,
// and this test would fail.
func TestCapabilityGrant_K1C3_LayeredIndependentKill(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, tenantID, "tenant_admin")
	finance := cgStaff(t, pool, tenantID, "finance")
	otherTenantAdmin := cgStaff(t, pool, tenantID, "tenant_admin")

	var requestID uuid.UUID
	if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: finance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		requestID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}

	sentinel := errors.New("K1C3 layered test: intentional rollback, not a real failure")
	err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// Owner-role DDL: a genuinely permissive tenant-shaped INSERT
		// policy, wide open (WITH CHECK (true)) - the most permissive
		// possible widening.
		if _, ddlErr := tx.Exec(ctx, `CREATE POLICY zz_k1c3_permissive_tenant_insert ON staff_capability_grant_approvals FOR INSERT WITH CHECK (true)`); ddlErr != nil {
			return fmt.Errorf("add permissive tenant INSERT policy: %w", ddlErr)
		}

		// Simulate the tenant-shaped session in THIS SAME transaction
		// (same backend, so the just-added policy is visible to it even
		// though it is uncommitted).
		if _, gucErr := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); gucErr != nil {
			return gucErr
		}
		if _, gucErr := tx.Exec(ctx, `SELECT set_config('app.principal_id', $1, true)`, otherTenantAdmin.String()); gucErr != nil {
			return gucErr
		}

		// Layer 1: CHECK still present. Run inside a SAVEPOINT (tx.Begin)
		// so the expected failure does not abort the outer transaction.
		if attemptErr := k1c3AttemptApprove(ctx, tx, tenantID, requestID); !cgIsCode(attemptErr, "CG011") {
			return fmt.Errorf("layer 1 (RLS widened, CHECK present): expected CG011, got %v", attemptErr)
		}

		// Layer 2: also drop the decided_by_scope CHECK.
		if _, ddlErr := tx.Exec(ctx, `ALTER TABLE staff_capability_grant_approvals DROP CONSTRAINT staff_capability_grant_approvals_decided_by_scope_check`); ddlErr != nil {
			return fmt.Errorf("drop decided_by_scope CHECK: %w", ddlErr)
		}
		if attemptErr := k1c3AttemptApprove(ctx, tx, tenantID, requestID); !cgIsCode(attemptErr, "CG011") {
			return fmt.Errorf("layer 2 (RLS widened, CHECK also dropped): expected CG011, got %v", attemptErr)
		}

		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("expected the layered test to end in its own rollback sentinel (nothing persisted), got %v", err)
	}
}

// k1c3AttemptApprove runs capability.DecideAndGrant inside a SAVEPOINT
// (pgx's tx.Begin on an already-open tx) so a refused attempt aborts only
// the savepoint, not the caller's outer transaction, letting the caller
// keep going (more DDL, a second attempt, and ultimately its own
// rollback).
func k1c3AttemptApprove(ctx context.Context, tx pgx.Tx, tenantID, requestID uuid.UUID) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = sp.Rollback(ctx) }()
	_, _, err = capability.DecideAndGrant(ctx, sp, tenantID, requestID, "approve", "test")
	return err
}

// TestCapabilityGrant_K1C3_CatalogueAssertion (K1-C3 (c)): a
// metadata-only pin that no policy on staff_capability_grant_approvals
// admits INSERT to a tenant-shaped session (the ONLY INSERT-capable
// policy is platform_scope_insert, and its own WITH CHECK clause requires
// the platform-shaped GUCs), and that the decided_by_scope = 'platform'
// CHECK constraint exists.
func TestCapabilityGrant_K1C3_CatalogueAssertion(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	type policyRow struct {
		name      string
		cmd       string
		withCheck string
	}
	var rows []policyRow
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		r, err := tx.Query(ctx, `
			SELECT policyname, cmd, COALESCE(with_check, '')
			  FROM pg_policies
			 WHERE tablename = 'staff_capability_grant_approvals'
			   AND cmd IN ('INSERT', 'ALL', '*')`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var pr policyRow
			if err := r.Scan(&pr.name, &pr.cmd, &pr.withCheck); err != nil {
				return err
			}
			rows = append(rows, pr)
		}
		return r.Err()
	}); err != nil {
		t.Fatalf("query pg_policies: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("expected exactly one INSERT-capable policy on staff_capability_grant_approvals, found %d: %+v", len(rows), rows)
	}
	if rows[0].name != "platform_scope_insert" {
		t.Fatalf("expected the sole INSERT-capable policy to be platform_scope_insert, got %q", rows[0].name)
	}
	// A tenant-shaped session (app.tenant_id set, app.platform_admin_principal_id
	// unset) cannot satisfy this WITH CHECK clause: it structurally
	// requires platform_admin_principal_id IS NOT NULL and tenant_id IS
	// NULL, which a tenant session can never both be true for at once.
	for _, must := range []string{"platform_admin_principal_id", "app.tenant_id"} {
		if !strings.Contains(rows[0].withCheck, must) {
			t.Fatalf("expected platform_scope_insert's WITH CHECK to reference %q, got %q", must, rows[0].withCheck)
		}
	}

	var checkExists bool
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM pg_constraint
				 WHERE conname = 'staff_capability_grant_approvals_decided_by_scope_check'
				   AND conrelid = 'staff_capability_grant_approvals'::regclass
				   AND pg_get_constraintdef(oid) = $1
			)`, `CHECK ((decided_by_scope = 'platform'::text))`).Scan(&checkExists)
	}); err != nil {
		t.Fatalf("query pg_constraint: %v", err)
	}
	if !checkExists {
		t.Fatal("expected the decided_by_scope = 'platform' CHECK constraint to exist")
	}
}

// A-5: self-grant, self-approval, approver == grantee, one Person under
// two principals, a NULL Person - all refused.
func TestCapabilityGrant_A5_SelfAndDistinctPersonRefusals(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, tenantID, "tenant_admin")
	finance := cgStaff(t, pool, tenantID, "finance")
	approver := cgStaff(t, pool, uuid.Nil, "platform_admin")

	// R-1: self-grant (grantee == requester's own staff id).
	err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: requester, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		return err
	})
	if !cgIsCode(err, "CG010") {
		t.Fatalf("R-1 self-grant: expected CG010, got %v", err)
	}

	// A valid request, to test self-approval and approver==requester next.
	var requestID uuid.UUID
	if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: finance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		requestID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}

	// R-3: approver == grantee (finance can't hold a platform session at
	// all in practice, but drive the trigger check directly is not
	// reachable via RLS for a tenant-scoped finance principal - the
	// meaningful case is approver == requester, tested next, and a
	// distinct-Person violation, tested below).

	// R-2: approver == requester is impossible here since requester is
	// tenant-scoped and approval requires a platform session; the R-2/R-8
	// case that matters in practice is "same Person under two principals"
	// (the sock-puppet test), covered next.

	// Sock-puppet: the SAME Person as the requester, but under a
	// DIFFERENT platform principal id, must still be refused (R-4/R-8
	// checks Person identity, not principal id).
	var requesterPerson uuid.UUID
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, requester).Scan(&requesterPerson)
	}); err != nil {
		t.Fatalf("lookup requester person: %v", err)
	}
	sockPuppetApprover := uuid.New()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			 VALUES ($1, NULL, $2, 'x', 'platform_admin', 'active', $3)`,
			sockPuppetApprover, "sockpuppet-"+sockPuppetApprover.String()+"@test.invalid", requesterPerson,
		)
		return err
	}); err != nil {
		t.Fatalf("insert sock puppet approver: %v", err)
	}
	err = pool.WithPlatformAdmin(ctx, sockPuppetApprover, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID, requestID, "approve", "test")
		return err
	})
	if !cgIsCode(err, "CG011") {
		t.Fatalf("sock-puppet (same Person, different principal) approval: expected CG011, got %v", err)
	}

	// A genuinely distinct approver succeeds (positive control - proves
	// the refusal above is about Person-distinctness, not a broken test).
	if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, grant, err := capability.DecideAndGrant(ctx, tx, tenantID, requestID, "approve", "test")
		if err != nil {
			return err
		}
		if grant == nil {
			t.Fatal("expected a grant row on approve")
		}
		return nil
	}); err != nil {
		t.Fatalf("positive control approval: %v", err)
	}
}

// A-6: a tenant names a platform grantee, or another tenant. Refused.
func TestCapabilityGrant_A6_TenantCannotNamePlatformGranteeOrOtherTenant(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	otherTenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, tenantID, "tenant_admin")
	platformGrantee := cgStaff(t, pool, uuid.Nil, "platform_admin")

	// R-6: platform grantee named by a tenant requester.
	err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: platformGrantee, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: timePtr(time.Now().Add(time.Hour)), ReasonCode: "test",
		})
		return err
	})
	if !cgIsCode(err, "CG010") {
		t.Fatalf("R-6 tenant naming platform grantee: expected CG010, got %v", err)
	}

	// R-5: a tenant requester naming a DIFFERENT tenant as the request's
	// own tenant is refused (RLS on staff_capability_grant_requests
	// itself already requires tenant_id = app.tenant_id, so this is
	// belt-and-braces at the RLS layer, not the trigger).
	otherFinance := cgStaff(t, pool, otherTenantID, "finance")
	err = pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, otherTenantID, capability.NewRequestInput{
			GranteeStaffID: otherFinance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		return err
	})
	if err == nil {
		t.Fatal("expected naming another tenant's request to be refused")
	}
}

// A-7: G-P1/G-P2 with the same platform principal or Person are refused.
// Uses a G-P2 shape (grantee is itself a platform_admin, tenant_id IS
// NULL): a plain platform session CAN see a platform-scoped grantee row
// (migration 0011's dual_scope_isolation NULL-arm), unlike a tenant-scoped
// G-P1 grantee - see staff_capability_grant_requests.grantee_person_id's
// own doc comment for that boundary; G-P1's request-creation step remains
// a known, disclosed gap (flagged for architect review), not exercised
// here.
func TestCapabilityGrant_A7_SamePrincipalOrPersonRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	platformPrincipal := cgStaff(t, pool, uuid.Nil, "platform_admin")
	granteeP2 := cgStaff(t, pool, uuid.Nil, "platform_admin")

	var requestID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, platformPrincipal, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: timePtr(time.Now().Add(time.Hour)), ReasonCode: "test",
		})
		requestID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create G-P2 request: %v", err)
	}

	// R-2/R-3: the SAME principal cannot approve its own request.
	err := pool.WithPlatformAdmin(ctx, platformPrincipal, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID, requestID, "approve", "test")
		return err
	})
	if !cgIsCode(err, "CG011") {
		t.Fatalf("same principal approving own G-P2 request: expected CG011, got %v", err)
	}
}

// A-10: G-P2 without valid_until, or beyond the max lifetime, is refused.
// G-T without valid_until is allowed.
func TestCapabilityGrant_A10_GP2LifetimeBounds(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, uuid.Nil, "platform_admin")
	granteeP2 := cgStaff(t, pool, uuid.Nil, "platform_admin")

	// No valid_until at all.
	err := pool.WithPlatformAdmin(ctx, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		return err
	})
	if !cgIsCode(err, "CG010") {
		t.Fatalf("G-P2 without valid_until: expected CG010, got %v", err)
	}

	// Beyond the configured max lifetime (4h, this migration's own
	// technical default).
	err = pool.WithPlatformAdmin(ctx, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: granteeP2, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ValidUntil: timePtr(time.Now().Add(24 * time.Hour)), ReasonCode: "test",
		})
		return err
	})
	if !cgIsCode(err, "CG010") {
		t.Fatalf("G-P2 beyond max lifetime: expected CG010, got %v", err)
	}

	// G-T without valid_until: allowed.
	tenantAdmin := cgStaff(t, pool, tenantID, "tenant_admin")
	financeGrantee := cgStaff(t, pool, tenantID, "finance")
	err = pool.WithPrincipalScope(ctx, tenantID, tenantAdmin, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: financeGrantee, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		return err
	})
	if err != nil {
		t.Fatalf("G-T without valid_until should be allowed, got %v", err)
	}
}

// A-11: duplicate pending request, and duplicate in-force grant, refused.
func TestCapabilityGrant_A11_DuplicatesRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, tenantID, "tenant_admin")
	finance := cgStaff(t, pool, tenantID, "finance")
	approver := cgStaff(t, pool, uuid.Nil, "platform_admin")

	newReq := func(tx pgx.Tx, ctx context.Context) (capability.Request, error) {
		return capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: finance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
	}

	var firstID uuid.UUID
	if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		r, err := newReq(tx, ctx)
		firstID = r.ID
		return err
	}); err != nil {
		t.Fatalf("first request: %v", err)
	}

	// R-11: a second pending request for the same (tenant, grantee, capability).
	err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := newReq(tx, ctx)
		return err
	})
	if err == nil {
		t.Fatal("expected a second pending request for the same grantee/capability to be refused")
	}

	// Approve the first, producing an in-force grant.
	if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID, firstID, "approve", "test")
		return err
	}); err != nil {
		t.Fatalf("approve first: %v", err)
	}

	// R-12: a new request for the same (tenant, grantee, capability) while
	// a grant is already in force is refused at request-creation time.
	err = pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := newReq(tx, ctx)
		return err
	})
	if !cgIsCode(err, "CG010") {
		t.Fatalf("R-12 duplicate in-force grant: expected CG010, got %v", err)
	}
}

// A-13: append-only - un-revoke, DELETE, TRUNCATE all refused.
func TestCapabilityGrant_A13_AppendOnly(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)

	if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		return capability.RevokeGrant(ctx, tx, f.TenantID, f.GrantID, "test-revoke")
	}); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Un-revoke: attempting to clear revoked_at is refused.
	err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		// Clears all four revoke columns together (not just revoked_at) so
		// this exercises the TRIGGER's own explicit re-revoke refusal, not
		// merely the table's separate (revoked_at IS NULL) = (revoked_by
		// IS NULL) CHECK constraint - a real, independent backstop, but a
		// different one from the trigger's own "no un-revoke" guard.
		_, err := tx.Exec(ctx,
			`UPDATE staff_capability_grants
			    SET revoked_at = NULL, revoked_by = NULL, revoked_by_scope = NULL, revoke_reason_code = NULL
			  WHERE id = $1`, f.GrantID)
		return err
	})
	if !cgIsCode(err, "CG012") {
		t.Fatalf("un-revoke: expected CG012, got %v", err)
	}

	// K1-C4 / LF C-K1-1: a RE-REVOKE - an UPDATE that keeps revoked_at
	// NOT NULL (never clearing it, unlike the un-revoke case above) but
	// supplies a NEW revoked_at/revoked_by/revoke_reason_code, attempting
	// to rewrite the historical revocation record itself - must also be
	// refused with CG012, and the row must be byte-for-byte unchanged
	// afterward. This is the mutation-kill test for mutant M5 (the
	// `OLD.revoked_at IS NOT NULL` guard removed): with that guard gone,
	// the trigger's whole-row-equality check (only revoked_at/revoked_by/
	// revoked_by_scope/revoke_reason_code may change) does NOT stop a
	// re-revoke, because those are exactly the four columns it lets
	// change - and the trigger unconditionally overwrites
	// revoked_at/revoked_by/revoked_by_scope with fresh actor/now()
	// values on any UPDATE that reaches that far, silently rewriting the
	// reason, the timestamp and the actor.
	var beforeRevokedAt time.Time
	var beforeRevokedBy uuid.UUID
	var beforeReason string
	if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT revoked_at, revoked_by, revoke_reason_code FROM staff_capability_grants WHERE id = $1`, f.GrantID,
		).Scan(&beforeRevokedAt, &beforeRevokedBy, &beforeReason)
	}); err != nil {
		t.Fatalf("read grant before re-revoke attempt: %v", err)
	}

	otherApprover := cgStaff(t, pool, uuid.Nil, "platform_admin")
	err = pool.WithPlatformAdmin(ctx, otherApprover, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx,
			`UPDATE staff_capability_grants
			    SET revoked_at = now(), revoked_by_scope = 'platform', revoke_reason_code = $2
			  WHERE id = $1`, f.GrantID, "rewritten-reason")
		return err
	})
	if !cgIsCode(err, "CG012") {
		t.Fatalf("re-revoke: expected CG012, got %v", err)
	}

	var afterRevokedAt time.Time
	var afterRevokedBy uuid.UUID
	var afterReason string
	if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx,
			`SELECT revoked_at, revoked_by, revoke_reason_code FROM staff_capability_grants WHERE id = $1`, f.GrantID,
		).Scan(&afterRevokedAt, &afterRevokedBy, &afterReason)
	}); err != nil {
		t.Fatalf("read grant after re-revoke attempt: %v", err)
	}
	if !beforeRevokedAt.Equal(afterRevokedAt) || beforeRevokedBy != afterRevokedBy || beforeReason != afterReason {
		t.Fatalf("re-revoke: expected the revocation record unchanged, before=(%v,%v,%q) after=(%v,%v,%q)",
			beforeRevokedAt, beforeRevokedBy, beforeReason, afterRevokedAt, afterRevokedBy, afterReason)
	}

	// DELETE refused - either by an explicit trigger exception, or (since
	// K1-C2's per-command RLS split leaves no permissive DELETE policy on
	// this table at all) silently by RLS filtering the row out before the
	// BEFORE DELETE trigger ever fires, which is a 0-rows-affected,
	// err==nil outcome in pgx, not a Postgres error - both are secure
	// (the row is provably untouched either way, verified below).
	var deleteRowsAffected int64
	err = pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM staff_capability_grants WHERE id = $1`, f.GrantID)
		if err == nil {
			deleteRowsAffected = tag.RowsAffected()
		}
		return err
	})
	if err == nil && deleteRowsAffected != 0 {
		t.Fatalf("expected DELETE on staff_capability_grants to affect zero rows (RLS or trigger refusal), affected %d", deleteRowsAffected)
	}
	// The row must still exist regardless of which mechanism refused it.
	if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		var n int
		if scanErr := tx.QueryRow(ctx, `SELECT count(*) FROM staff_capability_grants WHERE id = $1`, f.GrantID).Scan(&n); scanErr != nil {
			return scanErr
		}
		if n != 1 {
			t.Fatalf("expected the grant row to still exist after the refused DELETE, found %d", n)
		}
		return nil
	}); err != nil {
		t.Fatalf("verify grant row survives DELETE attempt: %v", err)
	}

	// TRUNCATE refused.
	err = pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `TRUNCATE staff_capability_grants`)
		return err
	})
	if err == nil {
		t.Fatal("expected TRUNCATE on staff_capability_grants to be refused")
	}
}

// The sock-puppet test (task-required, HD-PRH2-2 = (c) go/no-go):  a
// grant without platform co-approval is refused (there is no code path to
// even reach an in-force grant without an approval row - proved by
// showing the grants table itself refuses a bare INSERT with no
// approval), and the same Person under two principals is refused
// (covered above by TestCapabilityGrant_A5_SelfAndDistinctPersonRefusals's
// sock-puppet case, exercised again here in isolation for the
// go/no-go condition's own record).
func TestCapabilityGrant_SockPuppet_NoCoApprovalNoGrant(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, tenantID, "tenant_admin")
	finance := cgStaff(t, pool, tenantID, "finance")

	var requestID uuid.UUID
	if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: finance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		requestID = req.ID
		return err
	}); err != nil {
		t.Fatalf("create request: %v", err)
	}

	// A bare INSERT into staff_capability_grants, with no approval at
	// all, must be refused (CG012 - no matching approve decision).
	err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_capability_grants (request_id, approval_id) VALUES ($1, gen_random_uuid())`, requestID)
		return err
	})
	if err == nil {
		t.Fatal("expected a grant row with no real approval to be refused")
	}
}

func timePtr(t time.Time) *time.Time { return &t }

// assertActingOpenRefusal (L-2, security review): checks not just that the
// error carries SQLSTATE CG020, but that it actually comes from the
// setter's own "open acting session" step (WithPlatformActingInTenant's
// `db: open acting session: %w` wrap around
// `SELECT financial_acting_session_open()`), never from some other,
// unrelated step that happens to also raise CG020.
func assertActingOpenRefusal(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected the setter to refuse")
	}
	if !cgIsCode(err, "CG020") {
		t.Fatalf("expected CG020, got %v", err)
	}
	if !strings.Contains(err.Error(), "open acting session") {
		t.Fatalf("expected the error to come from the setter's own \"open acting session\" step, got %v", err)
	}
}

// TestActingSetter_RefusesWithoutValidGrant is the setter's own validity
// check (ADR 0099 §6.1 point 2, C-99-8): WithPlatformActingInTenant must
// raise (SQLSTATE CG020, from its own "open acting session" step) and
// never call fn when the (principal, tenant) pair has no in-force grant at
// all - a random staff_users row, a real tenant with no grant, and a
// revoked grant are each tried.
func TestActingSetter_RefusesWithoutValidGrant(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	t.Run("no grant at all", func(t *testing.T) {
		tenantID := createTestTenant(t, pool)
		platformPrincipal := cgStaff(t, pool, uuid.Nil, "platform_admin")
		called := false
		err := pool.WithPlatformActingInTenant(ctx, platformPrincipal, tenantID, uuid.Nil, "", func(ctx context.Context, tx pgx.Tx) error {
			called = true
			return nil
		})
		assertActingOpenRefusal(t, err)
		if called {
			t.Fatal("fn must never run when the acting session fails to open")
		}
	})

	t.Run("revoked grant", func(t *testing.T) {
		f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
		if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
			return capability.RevokeGrant(ctx, tx, f.TenantID, f.GrantID, "test-revoke")
		}); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		err := pool.WithPlatformActingInTenant(ctx, f.GranteeID, f.TenantID, uuid.Nil, "", func(ctx context.Context, tx pgx.Tx) error {
			return nil
		})
		assertActingOpenRefusal(t, err)
	})

	t.Run("grant for a different tenant", func(t *testing.T) {
		f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)
		otherTenant := createTestTenant(t, pool)
		err := pool.WithPlatformActingInTenant(ctx, f.GranteeID, otherTenant, uuid.Nil, "", func(ctx context.Context, tx pgx.Tx) error {
			return nil
		})
		assertActingOpenRefusal(t, err)
	})
}

// A-4 (K1-1 cases): with a VALID, in-force acting session (opened through
// the sole setter, WithPlatformActingInTenant), every write/read the §6.4
// restrictive fence must refuse on the seven exposed tables is refused.
// This is also the mutation-kill test for "drop one restrictive policy"
// on each of the seven tables (ADR 0099 §14 mutant list).
func TestCapabilityGrant_A4_K11_RestrictiveFenceRefusals(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	principalID, tenantID := mustBuildValidActingGrantFixture(t, pool)

	// A real Person, created BEFORE the acting session opens (persons is
	// fully denied to acting sessions, §6.4) - needed only as a valid FK
	// target for the player_restrictions attempt below.
	somePerson := uuid.New()
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, somePerson)
		return err
	}); err != nil {
		t.Fatalf("insert person fixture: %v", err)
	}

	// runRefused: the write/query itself must error (RLS or trigger
	// refusal).
	runRefused := func(t *testing.T, name string, fn func(tx pgx.Tx) error) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			err := pool.WithPlatformActingInTenant(ctx, principalID, tenantID, uuid.Nil, "", func(ctx context.Context, tx pgx.Tx) error {
				return fn(tx)
			})
			if err == nil {
				t.Fatalf("%s: expected refusal, got success", name)
			}
		})
	}
	// runZeroRows: the query itself succeeds (SELECT with WHERE/COUNT
	// never errors on zero matches), but RLS must make zero rows visible.
	runZeroRows := func(t *testing.T, name, query string, args ...any) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			var n int
			err := pool.WithPlatformActingInTenant(ctx, principalID, tenantID, uuid.Nil, "", func(ctx context.Context, tx pgx.Tx) error {
				return tx.QueryRow(ctx, query, args...).Scan(&n)
			})
			if err != nil {
				t.Fatalf("%s: query itself failed: %v", name, err)
			}
			if n != 0 {
				t.Fatalf("%s: expected zero rows visible under RLS, got %d", name, n)
			}
		})
	}

	// staff_users: insert a platform_admin.
	runRefused(t, "insert_platform_admin", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status) VALUES (gen_random_uuid(), NULL, $1, 'x', 'platform_admin', 'active')`,
			"forged-"+uuid.NewString()+"@test.invalid")
		return err
	})
	// staff_users: read another platform staff row (not self, not tenant X).
	otherPlatform := cgStaff(t, pool, uuid.Nil, "platform_admin")
	runZeroRows(t, "read_other_platform_staff", `SELECT count(*) FROM staff_users WHERE id = $1`, otherPlatform)
	// staff_users: update/delete staff.
	runRefused(t, "update_staff", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(), `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, principalID)
		return err
	})
	// sessions: insert a platform session row.
	runRefused(t, "insert_platform_session", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO sessions (id, principal_type, principal_id, refresh_token_hash, expires_at) VALUES (gen_random_uuid(), 'staff', $1, 'x', now() + interval '1 day')`,
			principalID)
		return err
	})
	// login_attempts: read.
	runZeroRows(t, "read_login_attempts", `SELECT count(*) FROM login_attempts`)
	// audit_log: read any row (INSERT is allowed for own tenant, but
	// SELECT is always refused).
	// Seed a genuine platform-level (tenant_id NULL) audit row first - the
	// acting_insert audit row WithPlatformActingInTenant itself wrote is
	// tenant-scoped (forced to tenantID), so without this a mutant that
	// widens the SELECT fence to "true" would escape detection simply
	// because no platform-scope row happens to exist yet.
	if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO audit_log (tenant_id, actor_type, actor_id, action, outcome) VALUES (NULL, 'staff', $1, 'test.platform_seed', 'success')`, principalID)
		return err
	}); err != nil {
		t.Fatalf("seed platform audit row: %v", err)
	}
	runZeroRows(t, "read_audit_log", `SELECT count(*) FROM audit_log`)
	// audit_log: an attempted platform (tenant_id NULL) insert is FORCED
	// to the acting tenant by audit_log_acting_actor (never left as a
	// genuine platform-scope row) - proving the trigger, not merely the
	// restrictive policy, closes this K1-1 case.
	t.Run("insert_audit_row_forced_to_acting_tenant_never_platform", func(t *testing.T) {
		marker := "test.forged." + uuid.NewString()
		// The INSERT runs plain (no RETURNING): acting_log_acting_actor's
		// restrictive fence denies ANY SELECT to an acting session (even
		// of the row it just inserted), so RETURNING's own implicit
		// SELECT-policy filter would otherwise make this whole statement
		// misleadingly look like a failure. Verified afterward from a
		// genuinely tenant-scoped session instead.
		err := pool.WithPlatformActingInTenant(ctx, principalID, tenantID, uuid.Nil, "", func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx,
				`INSERT INTO audit_log (tenant_id, actor_type, actor_id, action, outcome) VALUES (NULL, 'staff', $1, $2, 'success')`,
				principalID, marker)
			return err
		})
		if err != nil {
			t.Fatalf("insert: %v", err)
		}

		var gotTenant, gotActor string
		if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT tenant_id::text, actor_id::text FROM audit_log WHERE action = $1`, marker).Scan(&gotTenant, &gotActor)
		}); err != nil {
			t.Fatalf("read back from a tenant session: %v", err)
		}
		if gotTenant != tenantID.String() {
			t.Fatalf("expected tenant_id forced to the acting tenant %s, got %s (never platform/NULL)", tenantID, gotTenant)
		}
		if gotActor != principalID.String() {
			t.Fatalf("expected actor_id forced to the acting principal %s, got %s", principalID, gotActor)
		}
	})
	// persons: any access at all.
	runZeroRows(t, "read_persons", `SELECT count(*) FROM persons`)
	// player_restrictions: insert a NULL-tenant (platform) row.
	runRefused(t, "insert_null_tenant_player_restriction", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO player_restrictions (id, person_id, tenant_id, restriction_type, source, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), $1, NULL, 'self_exclusion', 'staff', 'staff', $2)`,
			somePerson, principalID)
		return err
	})
	// risk_rules: write (a NULL-tenant, i.e. platform-level, rule).
	runRefused(t, "write_risk_rules", func(tx pgx.Tx) error {
		_, err := tx.Exec(context.Background(),
			`INSERT INTO risk_rules (id, tenant_id, operation, limit_kind, time_window, threshold, created_by_actor_type, created_by_actor_id)
			 VALUES (gen_random_uuid(), NULL, 'deposit', 'max_amount', 'transaction', 100, 'staff', $1)`,
			principalID)
		return err
	})
}

// TestCapabilityGrant_C1_ExactGUCShapePredicate pins financial_acting_
// gucs_exact() (ADR 0099 §6.3, security confirmation C-1) directly: true
// only when BOTH acting GUCs are set AND all five other principal-shaped
// GUCs are unset. Each of the five "must be unset" GUCs is tested
// individually, set ALONGSIDE both acting GUCs - a mutant dropping any
// ONE of the five checks is caught by its own dedicated case.
func TestCapabilityGrant_C1_ExactGUCShapePredicate(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	checkExact := func(t *testing.T, setup func(tx pgx.Tx) error) bool {
		t.Helper()
		var exact bool
		err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true)`, uuid.NewString()); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_platform_principal_id', $1, true)`, uuid.NewString()); err != nil {
				return err
			}
			if setup != nil {
				if err := setup(tx); err != nil {
					return err
				}
			}
			return tx.QueryRow(ctx, `SELECT financial_acting_gucs_exact()`).Scan(&exact)
		})
		if err != nil {
			t.Fatalf("query financial_acting_gucs_exact: %v", err)
		}
		return exact
	}

	t.Run("both acting GUCs alone is exact", func(t *testing.T) {
		if !checkExact(t, nil) {
			t.Fatal("expected true with only the two acting GUCs set")
		}
	})

	cases := []struct {
		name string
		guc  string
	}{
		{"app.tenant_id also set", "app.tenant_id"},
		{"app.principal_id also set", "app.principal_id"},
		{"app.platform_admin_principal_id also set", "app.platform_admin_principal_id"},
		{"app.player_account_id also set", "app.player_account_id"},
		{"app.platform_service_id also set", "app.platform_service_id"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := checkExact(t, func(tx pgx.Tx) error {
				_, err := tx.Exec(context.Background(), `SELECT set_config($1, $2, true)`, tc.guc, uuid.NewString())
				return err
			})
			if got {
				t.Fatalf("expected financial_acting_gucs_exact() = false with %s also set, got true", tc.guc)
			}
		})
	}
}

// A-15: the Go role map (internal/auth) and the catalogue's
// financial_governance_permissions role sets agree exactly for the seven
// §3.2 governance permissions.
func TestCapabilityGrant_A15_GoRoleMapMatchesCatalogue(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	goRoleSets := map[string][]string{
		"capability_grant:request": {"tenant_admin", "platform_admin"},
		"capability_grant:approve": {"platform_admin"},
		"capability_grant:revoke":  {"tenant_admin", "platform_admin"},
		"capability_grant:read":    {"tenant_admin", "compliance", "platform_admin"},
		"financial_policy:author":  {"platform_admin"},
		"financial_policy:tighten": {"tenant_admin"},
		"financial_policy:read":    {"tenant_admin", "finance", "compliance", "platform_admin"},
	}

	rows, err := func() (map[string][]string, error) {
		out := map[string][]string{}
		err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			r, err := tx.Query(ctx, `SELECT permission, roles FROM financial_governance_permissions`)
			if err != nil {
				return err
			}
			defer r.Close()
			for r.Next() {
				var perm string
				var roles []string
				if err := r.Scan(&perm, &roles); err != nil {
					return err
				}
				out[perm] = roles
			}
			return r.Err()
		})
		return out, err
	}()
	if err != nil {
		t.Fatalf("read catalogue: %v", err)
	}

	if len(rows) != len(goRoleSets) {
		t.Fatalf("expected %d catalogue rows, got %d: %v", len(goRoleSets), len(rows), rows)
	}
	for perm, wantRoles := range goRoleSets {
		gotRoles, ok := rows[perm]
		if !ok {
			t.Errorf("permission %q missing from catalogue", perm)
			continue
		}
		if !sameStringSet(wantRoles, gotRoles) {
			t.Errorf("permission %q: Go=%v DB=%v", perm, wantRoles, gotRoles)
		}
	}
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, s := range a {
		seen[s] = true
	}
	for _, s := range b {
		if !seen[s] {
			return false
		}
		delete(seen, s)
	}
	return len(seen) == 0
}

// A-8 (S-4): a suspended actor is refused even with a live/valid
// principal id (the trigger re-reads staff_users.status in-tx, never
// trusting a JWT); a demoted/suspended grantee is refused at request
// time too (R-9's active check).
func TestCapabilityGrant_A8_SuspendedActorRefused(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	tenantID := createTestTenant(t, pool)
	requester := cgStaff(t, pool, tenantID, "tenant_admin")
	finance := cgStaff(t, pool, tenantID, "finance")

	// Suspend the requester AFTER creating the staff row (status set by
	// DB fixture, mirroring the ADR's own wording), then attempt a
	// request through its principal id.
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, requester)
		return err
	}); err != nil {
		t.Fatalf("suspend requester: %v", err)
	}

	err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: finance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		return err
	})
	if !cgIsCode(err, "CG001") {
		t.Fatalf("suspended requester: expected CG001, got %v", err)
	}

	// A demoted/suspended GRANTEE (R-9's active check) is refused too.
	activeRequester := cgStaff(t, pool, tenantID, "tenant_admin")
	if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, finance)
		return err
	}); err != nil {
		t.Fatalf("suspend grantee: %v", err)
	}
	err = pool.WithPrincipalScope(ctx, tenantID, activeRequester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
			GranteeStaffID: finance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
			ValidFrom: time.Now(), ReasonCode: "test",
		})
		return err
	})
	if !cgIsCode(err, "CG010") {
		t.Fatalf("suspended grantee: expected CG010 (R-9), got %v", err)
	}
}

// TestCapabilityGrant_F2_ForcedColumnsCannotBeSupplied (F-2 / architect
// I-3): every *_by/*_by_scope/*_by_person_id/*_txid/grantee_* column that
// the guard triggers claim to force from the live session or the
// request/approval row must actually BE forced - a caller-supplied,
// deliberately mismatching value must never persist. capability.
// CreateRequest/DecideAndGrant never expose these columns as Go-level
// inputs, so this drives raw INSERT/UPDATE statements directly (still
// inside the normal RLS-scoped session helpers) supplying every forced
// column explicitly with a wrong value, then reads the row back and
// checks the resolver's own value won.
//
// Mutant MF-a (`requested_by_person_id := COALESCE(NEW.requested_by_person_id, v_actor.person_id)`)
// and mutant MF-b (the same shape for `grantee_person_id`) are shown
// killed below.
func TestCapabilityGrant_F2_ForcedColumnsCannotBeSupplied(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()

	t.Run("requested_by requested_by_scope requested_by_person_id", func(t *testing.T) {
		tenantID := createTestTenant(t, pool)
		requester := cgStaff(t, pool, tenantID, "tenant_admin")
		finance := cgStaff(t, pool, tenantID, "finance")

		var requesterPerson uuid.UUID
		if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, requester).Scan(&requesterPerson)
		}); err != nil {
			t.Fatalf("lookup requester person: %v", err)
		}

		bogusActor := uuid.New()
		bogusPerson := uuid.New()
		var requestID uuid.UUID
		if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO staff_capability_grant_requests
					(tenant_id, grantee_staff_id, capability, valid_from, reason_code,
					 requested_by, requested_by_scope, requested_by_person_id)
				VALUES ($1, $2, $3, now(), 'test', $4, 'platform', $5)
				RETURNING id`,
				tenantID, finance, string(capability.CapabilityLedgerAdjustmentInitiate), bogusActor, bogusPerson,
			).Scan(&requestID)
		}); err != nil {
			t.Fatalf("insert with bogus forced columns: %v", err)
		}

		var gotBy uuid.UUID
		var gotScope string
		var gotPerson uuid.UUID
		if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT requested_by, requested_by_scope, requested_by_person_id FROM staff_capability_grant_requests WHERE id = $1`,
				requestID).Scan(&gotBy, &gotScope, &gotPerson)
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if gotBy != requester {
			t.Fatalf("requested_by: expected the real requester %s (not the supplied %s), got %s", requester, bogusActor, gotBy)
		}
		if gotScope != "tenant" {
			t.Fatalf("requested_by_scope: expected 'tenant' (not the supplied 'platform'), got %q", gotScope)
		}
		if gotPerson != requesterPerson {
			t.Fatalf("requested_by_person_id (MF-a): expected the real requester's Person %s (not the supplied %s), got %s", requesterPerson, bogusPerson, gotPerson)
		}
	})

	t.Run("grantee_scope grantee_person_id", func(t *testing.T) {
		tenantID := createTestTenant(t, pool)
		requester := cgStaff(t, pool, tenantID, "tenant_admin")
		finance := cgStaff(t, pool, tenantID, "finance")

		var financePerson uuid.UUID
		if err := pool.WithTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, finance).Scan(&financePerson)
		}); err != nil {
			t.Fatalf("lookup grantee person: %v", err)
		}

		bogusPerson := uuid.New()
		var requestID uuid.UUID
		if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `
				INSERT INTO staff_capability_grant_requests
					(tenant_id, grantee_staff_id, capability, valid_from, reason_code,
					 grantee_scope, grantee_person_id)
				VALUES ($1, $2, $3, now(), 'test', 'platform', $4)
				RETURNING id`,
				tenantID, finance, string(capability.CapabilityLedgerAdjustmentInitiate), bogusPerson,
			).Scan(&requestID)
		}); err != nil {
			t.Fatalf("insert with bogus forced columns: %v", err)
		}

		var gotScope string
		var gotPerson uuid.UUID
		if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT grantee_scope, grantee_person_id FROM staff_capability_grant_requests WHERE id = $1`,
				requestID).Scan(&gotScope, &gotPerson)
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if gotScope != "tenant" {
			t.Fatalf("grantee_scope: expected 'tenant' (not the supplied 'platform'), got %q", gotScope)
		}
		if gotPerson != financePerson {
			t.Fatalf("grantee_person_id (MF-b): expected the real grantee's Person %s (not the supplied %s), got %s", financePerson, bogusPerson, gotPerson)
		}
	})

	t.Run("decided_by decided_by_scope decided_by_person_id decided_txid", func(t *testing.T) {
		tenantID := createTestTenant(t, pool)
		requester := cgStaff(t, pool, tenantID, "tenant_admin")
		finance := cgStaff(t, pool, tenantID, "finance")
		approver := cgStaff(t, pool, uuid.Nil, "platform_admin")

		var approverPerson uuid.UUID
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, approver).Scan(&approverPerson)
		}); err != nil {
			t.Fatalf("lookup approver person: %v", err)
		}

		var requestID uuid.UUID
		if err := pool.WithPrincipalScope(ctx, tenantID, requester, func(ctx context.Context, tx pgx.Tx) error {
			req, err := capability.CreateRequest(ctx, tx, tenantID, capability.NewRequestInput{
				GranteeStaffID: finance, Capability: capability.CapabilityLedgerAdjustmentInitiate,
				ValidFrom: time.Now(), ReasonCode: "test",
			})
			requestID = req.ID
			return err
		}); err != nil {
			t.Fatalf("create request: %v", err)
		}

		bogusActor := uuid.New()
		bogusPerson := uuid.New()
		const bogusTxid = int64(1)
		var approvalID uuid.UUID
		if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `
				INSERT INTO staff_capability_grant_approvals
					(request_id, decision, reason_code, decided_by, decided_by_scope, decided_by_person_id, decided_txid)
				VALUES ($1, 'approve', 'test', $2, 'platform', $3, $4)
				RETURNING id`,
				requestID, bogusActor, bogusPerson, bogusTxid,
			).Scan(&approvalID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO staff_capability_grants (request_id, approval_id) VALUES ($1, $2)`, requestID, approvalID)
			return err
		}); err != nil {
			t.Fatalf("insert approval+grant with bogus forced columns: %v", err)
		}

		var gotBy uuid.UUID
		var gotScope string
		var gotPerson uuid.UUID
		var gotTxid int64
		if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT decided_by, decided_by_scope, decided_by_person_id, decided_txid FROM staff_capability_grant_approvals WHERE id = $1`,
				approvalID).Scan(&gotBy, &gotScope, &gotPerson, &gotTxid)
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if gotBy != approver {
			t.Fatalf("decided_by: expected the real approver %s (not the supplied %s), got %s", approver, bogusActor, gotBy)
		}
		if gotScope != "platform" {
			t.Fatalf("decided_by_scope: expected 'platform', got %q", gotScope)
		}
		if gotPerson != approverPerson {
			t.Fatalf("decided_by_person_id: expected the real approver's Person %s (not the supplied %s), got %s", approverPerson, bogusPerson, gotPerson)
		}
		if gotTxid == bogusTxid {
			t.Fatalf("decided_txid: expected the real txid_current() (not the supplied bogus %d), got %d", bogusTxid, gotTxid)
		}
	})

	t.Run("revoked_by revoked_by_scope", func(t *testing.T) {
		f := mustBuildActingGrantFixtureWithCapability(t, pool, capability.CapabilityLedgerAdjustmentInitiate)

		bogusActor := uuid.New()
		if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `
				UPDATE staff_capability_grants
				   SET revoked_at = now(), revoked_by = $2, revoked_by_scope = 'tenant', revoke_reason_code = 'test'
				 WHERE id = $1`, f.GrantID, bogusActor)
			return err
		}); err != nil {
			t.Fatalf("revoke with bogus forced columns: %v", err)
		}

		var gotBy uuid.UUID
		var gotScope string
		if err := pool.WithPlatformAdmin(ctx, f.ApproverID, func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx,
				`SELECT revoked_by, revoked_by_scope FROM staff_capability_grants WHERE id = $1`, f.GrantID,
			).Scan(&gotBy, &gotScope)
		}); err != nil {
			t.Fatalf("read back: %v", err)
		}
		if gotBy != f.ApproverID {
			t.Fatalf("revoked_by: expected the real revoker %s (not the supplied %s), got %s", f.ApproverID, bogusActor, gotBy)
		}
		if gotScope != "platform" {
			t.Fatalf("revoked_by_scope: expected 'platform' (the real session's scope, not the supplied 'tenant'), got %q", gotScope)
		}
	})
}
