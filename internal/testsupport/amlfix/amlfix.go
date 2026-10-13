//go:build integration

// Package amlfix (integration builds ONLY: no production binary contains it) builds AML officers and an officer quorum for
// integration tests through the REAL proof-bound K1 capability-grant flow (a platform requester, a distinct platform
// approver, a grantee that is neither), never by writing grant rows directly, so every test that uses it is also a positive
// test that the aml_officer grant path works (ADR 0116 s6 B4). All identities and data are synthetic.
//
// The caller must install the per-process actor-proof issuer first (prooftest.Install(t, ownerURL)); this package does not
// import prooftest so the signing capability stays where the static allow-list puts it.
package amlfix

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
)

// Officer is one synthetic aml_officer.
type Officer struct {
	StaffID  uuid.UUID
	PersonID uuid.UUID
	// Actor is the officer as the aml.Service sees it (tenant scope for an own-licence tenant, platform_acting otherwise).
	Actor aml.Actor
}

// Quorum is a tenant with its owner scope and the officers granted for it.
type Quorum struct {
	Tenant     uuid.UUID
	OwnerScope string // 'platform' | 'tenant': the tenant's licensing owner scope
	Officers   []Officer

	pool *db.Pool
}

// OwnerScopeOf reads the tenant's AML owner scope.
func OwnerScopeOf(t testing.TB, pool *db.Pool, tenantID uuid.UUID) string {
	t.Helper()
	var s string
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT expected_licensee FROM tenants WHERE id = $1`, tenantID).Scan(&s)
	}); err != nil {
		t.Fatalf("amlfix: read tenant owner scope: %v", err)
	}
	return s
}

func newPlatformAdmin(t testing.TB, pool *db.Pool) (staff, person uuid.UUID) {
	t.Helper()
	return newPlatformAdminOfPerson(t, pool, uuid.New(), true)
}

func newPlatformAdminOfPerson(t testing.TB, pool *db.Pool, person uuid.UUID, createPerson bool) (staff, _ uuid.UUID) {
	t.Helper()
	staff = uuid.New()
	if err := pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if createPerson {
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, NULL, $2, '!amlfix-not-a-login', 'platform_admin', 'active', $3)`, staff, "amlfix-"+staff.String()+"@fixture.invalid", person)
		return err
	}); err != nil {
		t.Fatalf("amlfix: create platform admin: %v", err)
	}
	return staff, person
}

func newTenantCompliance(t testing.TB, pool *db.Pool, tenantID uuid.UUID) (staff, person uuid.UUID) {
	t.Helper()
	return newTenantComplianceOfPerson(t, pool, tenantID, uuid.New(), true)
}

func newTenantComplianceOfPerson(t testing.TB, pool *db.Pool, tenantID, person uuid.UUID, createPerson bool) (staff, _ uuid.UUID) {
	t.Helper()
	staff = uuid.New()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if createPerson {
			if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, '!amlfix-not-a-login', 'compliance', 'active', $4)`, staff, tenantID, "amlfix-"+staff.String()+"@fixture.invalid", person)
		return err
	}); err != nil {
		t.Fatalf("amlfix: create tenant compliance staff: %v", err)
	}
	return staff, person
}

// NewStaff creates one more synthetic staff member of the kind an officer of the tenant's owner scope would be (a tenant
// `compliance` user for an own-licence tenant, a `platform_admin` otherwise) WITHOUT granting anything.
func NewStaff(t testing.TB, pool *db.Pool, tenantID uuid.UUID) (staff, person uuid.UUID) {
	t.Helper()
	if OwnerScopeOf(t, pool, tenantID) == "tenant" {
		return newTenantCompliance(t, pool, tenantID)
	}
	return newPlatformAdmin(t, pool)
}

// newTenantAdmin creates a synthetic tenant_admin staff member (the REQUESTER of a tenant-scope grant: a platform session cannot
// read tenant staff rows, so K1 has the tenant request and the platform co-approve).
func newTenantAdmin(t testing.TB, pool *db.Pool, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	staff, person := uuid.New(), uuid.New()
	if err := pool.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, person); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id)
			VALUES ($1, $2, $3, '!amlfix-not-a-login', 'tenant_admin', 'active', $4)`, staff, tenantID, "amlfix-"+staff.String()+"@fixture.invalid", person)
		return err
	}); err != nil {
		t.Fatalf("amlfix: create tenant admin: %v", err)
	}
	return staff
}

// NewStaffOfPerson creates another staff account (of the kind NewStaff creates) for an EXISTING Person, without granting
// anything: the shape of "the same human with a second account", or of a case subject who is also staff.
func NewStaffOfPerson(t testing.TB, pool *db.Pool, tenantID, person uuid.UUID) uuid.UUID {
	t.Helper()
	if OwnerScopeOf(t, pool, tenantID) == "tenant" {
		s, _ := newTenantComplianceOfPerson(t, pool, tenantID, person, false)
		return s
	}
	s, _ := newPlatformAdminOfPerson(t, pool, person, false)
	return s
}

// NewTenantComplianceStaff creates a tenant `compliance` staff member regardless of the tenant's licensing model (a
// tenant-scope officer inside a platform-licensed tenant: it must NOT see platform-owned AML data).
func NewTenantComplianceStaff(t testing.TB, pool *db.Pool, tenantID uuid.UUID) (staff, person uuid.UUID) {
	t.Helper()
	return newTenantCompliance(t, pool, tenantID)
}

// GrantOfficer grants the aml_officer capability to an existing staff member through the real request -> platform
// co-approval -> grant flow. A platform grantee (acting officer) is requested by a platform admin; a tenant grantee by a
// tenant admin of the tenant. The approver is always a distinct platform admin; every Person involved is distinct.
func GrantOfficer(t testing.TB, pool *db.Pool, tenantID, grantee uuid.UUID) {
	t.Helper()
	GrantOfficerAs(t, pool, tenantID, grantee, OwnerScopeOf(t, pool, tenantID))
}

// GrantOfficerAs is GrantOfficer with an explicit grantee scope: "tenant" (a tenant `compliance` grantee) or "platform" (a
// platform_admin acting grantee).
func GrantOfficerAs(t testing.TB, pool *db.Pool, tenantID, grantee uuid.UUID, scope string) {
	t.Helper()
	ctx := context.Background()
	approver, _ := newPlatformAdmin(t, pool)
	var validUntil *time.Time
	if scope == "platform" {
		v := time.Now().Add(time.Hour)
		validUntil = &v
	}
	in := capability.NewRequestInput{GranteeStaffID: grantee, Capability: capability.CapabilityAMLOfficer, ValidFrom: time.Now(), ValidUntil: validUntil, ReasonCode: "fixture"}
	var reqID uuid.UUID
	create := func(ctx context.Context, tx pgx.Tx) error {
		req, err := capability.CreateRequest(ctx, tx, tenantID, in)
		reqID = req.ID
		return err
	}
	var err error
	if scope == "tenant" {
		err = pool.WithPrincipalScope(ctx, tenantID, newTenantAdmin(t, pool, tenantID), create)
	} else {
		requester, _ := newPlatformAdmin(t, pool)
		err = pool.WithPlatformAdmin(ctx, requester, create)
	}
	if err != nil {
		t.Fatalf("amlfix: request aml_officer grant: %v", err)
	}
	if err := pool.WithPlatformAdmin(ctx, approver, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := capability.DecideAndGrant(ctx, tx, tenantID, reqID, "approve", "fixture")
		return err
	}); err != nil {
		t.Fatalf("amlfix: approve aml_officer grant: %v", err)
	}
}

// RevokeOfficer revokes the officer's in-force aml_officer grant (the emergency stop) through the real K1 revoke path, as a
// platform principal, so the revoke carries its own signed proof.
func RevokeOfficer(t testing.TB, pool *db.Pool, tenantID, staffID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var grantID uuid.UUID
	if err := pool.WithPlatformAdmin(ctx, mustPlatformAdmin(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM staff_capability_grants WHERE tenant_id = $1 AND grantee_staff_id = $2
			AND capability = 'aml_officer' AND revoked_at IS NULL ORDER BY granted_at DESC LIMIT 1`, tenantID, staffID).Scan(&grantID)
	}); err != nil {
		t.Fatalf("amlfix: find grant to revoke: %v", err)
	}
	if err := pool.WithPlatformAdmin(ctx, mustPlatformAdmin(t, pool), func(ctx context.Context, tx pgx.Tx) error {
		return capability.RevokeGrant(ctx, tx, tenantID, grantID, "fixture_revoke")
	}); err != nil {
		t.Fatalf("amlfix: revoke aml_officer grant: %v", err)
	}
}

func mustPlatformAdmin(t testing.TB, pool *db.Pool) uuid.UUID {
	t.Helper()
	s, _ := newPlatformAdmin(t, pool)
	return s
}

// NewOfficer creates a staff member and grants it aml_officer.
func NewOfficer(t testing.TB, pool *db.Pool, tenantID uuid.UUID) Officer {
	t.Helper()
	scope := OwnerScopeOf(t, pool, tenantID)
	staff, person := NewStaff(t, pool, tenantID)
	GrantOfficer(t, pool, tenantID, staff)
	a := aml.Actor{StaffID: staff, TenantID: tenantID, Scope: aml.ScopePlatformActing}
	if scope == "tenant" {
		a.Scope = aml.ScopeTenant
	}
	return Officer{StaffID: staff, PersonID: person, Actor: a}
}

// Bootstrap grants n officers (n >= 2 gives the quorum) for the tenant's owner scope.
func Bootstrap(t testing.TB, pool *db.Pool, tenantID uuid.UUID, n int) *Quorum {
	t.Helper()
	q := &Quorum{Tenant: tenantID, OwnerScope: OwnerScopeOf(t, pool, tenantID), pool: pool}
	for i := 0; i < n; i++ {
		q.Officers = append(q.Officers, NewOfficer(t, pool, tenantID))
	}
	return q
}

// Service builds an aml.Service over p (the runtime-role pool in the real tests).
func Service(t testing.TB, p aml.Pool) *aml.Service {
	t.Helper()
	s, err := aml.New(aml.Config{Pool: p})
	if err != nil {
		t.Fatalf("amlfix: aml.New: %v", err)
	}
	return s
}

// PlaceHold places a hold through the real officer API as officer 0 of the quorum.
func (q *Quorum) PlaceHold(t testing.TB, svc *aml.Service, person uuid.UUID, account *uuid.UUID, scope aml.HoldScope, class aml.ReasonClass) uuid.UUID {
	t.Helper()
	id, err := svc.PlaceHold(context.Background(), q.Officers[0].Actor, aml.PlaceHoldInput{
		PersonID: person, PlayerAccountID: account, Scope: scope, ReasonClass: class, ReasonCode: "fixture_hold",
	})
	if err != nil {
		t.Fatalf("amlfix: place hold: %v", err)
	}
	return id
}

// LiftHold lifts a `manual` hold as officer 0.
func (q *Quorum) LiftHold(t testing.TB, svc *aml.Service, hold uuid.UUID) {
	t.Helper()
	if _, err := svc.LiftHold(context.Background(), q.Officers[0].Actor, hold, "fixture_lift"); err != nil {
		t.Fatalf("amlfix: lift hold: %v", err)
	}
}

// HoldFor is the one-call helper for the gate tests of other packages: it bootstraps a two-officer quorum for the tenant
// (once per call; tenants in those tests are single-use), places an all_value_movement `manual` hold on the person (and
// account when non-nil) and returns the quorum and the hold id.
func HoldFor(t testing.TB, ownerPool *db.Pool, rt aml.Pool, tenantID, personID uuid.UUID, account *uuid.UUID, scope aml.HoldScope) (*Quorum, uuid.UUID) {
	t.Helper()
	q := Bootstrap(t, ownerPool, tenantID, 2)
	svc := Service(t, rt)
	return q, q.PlaceHold(t, svc, personID, account, scope, aml.ReasonManual)
}
