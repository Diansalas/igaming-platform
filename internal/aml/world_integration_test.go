//go:build integration

package aml_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/aml"
	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/amlfix"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

// The tests in this package drive the REAL officer API as the RUNTIME role (igaming_runtime): the owner pool exists only to
// provision fixtures (tenants, brands, staff) exactly as every other integration suite does; every AML call under test goes
// through the runtime pool, so a missing REVOKE, a missing proof check or a GUC-only path fails here.

func ownerURL(t testing.TB) string {
	t.Helper()
	u := os.Getenv("TEST_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping integration test")
	}
	return u
}

func ownerPool(t testing.TB) *db.Pool {
	t.Helper()
	p, err := db.Connect(context.Background(), ownerURL(t), 12, 5*time.Second)
	if err != nil {
		t.Fatalf("connect owner pool: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func runtimePool(t testing.TB) *db.Pool {
	t.Helper()
	u := os.Getenv("TEST_RUNTIME_DATABASE_URL")
	if u == "" {
		t.Skip("TEST_RUNTIME_DATABASE_URL not set; skipping runtime-role integration test")
	}
	p, err := db.Connect(context.Background(), u, 12, 5*time.Second)
	if err != nil {
		t.Fatalf("connect runtime pool: %v", err)
	}
	t.Cleanup(p.Close)
	var super, bypass bool
	if err := p.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&super, &bypass)
	}); err != nil {
		t.Fatalf("inspect runtime role: %v", err)
	}
	if super || bypass {
		t.Fatalf("the runtime pool must be neither superuser nor BYPASSRLS (super=%v bypass=%v): the tests would pass for the wrong reason", super, bypass)
	}
	return p
}

// ownerSvc is the OWNER-ONLY fixture path to the break-glass screening policy setter (aml_screening_policy_set). Since migration 0191
// (d1, D7) the runtime role can no longer execute that function, so a fixture that needs its un-restricted behaviour (any interval,
// loosening, a past effective time) changes the ACTOR it runs as (the table-owner pool), not the assertion it makes. The officer
// authorisation, quorum and signed proof inside the function are unchanged.
func ownerSvc(t testing.TB) *aml.Service {
	t.Helper()
	return amlfix.OwnerService(t, ownerPool(t))
}

// amlWorld is one tenant with a brand, players and (on request) an officer quorum.
type amlWorld struct {
	t      *testing.T
	Owner  *db.Pool
	RT     *db.Pool
	Tenant uuid.UUID
	Brand  identity.Brand
	// Scope is the tenant's AML owner scope: 'platform' (under_platform_licence) or 'tenant' (own_licence).
	Scope string
}

type player struct {
	AccountID uuid.UUID
	PersonID  uuid.UUID
}

// newWorld creates a tenant of the given licensing model ('under_platform_licence' | 'own_licence') with one active brand.
func newWorld(t *testing.T, licensingModel string) *amlWorld {
	t.Helper()
	prooftest.Install(t, ownerURL(t))
	owner := ownerPool(t)
	launchfix.SeedOperators(t)
	w := &amlWorld{t: t, Owner: owner, RT: runtimePool(t)}
	if err := owner.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "aml-"+licensingModel, "aml-"+uuid.NewString(), licensingModel)
		w.Tenant = tn.ID
		return err
	}); err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if err := owner.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, w.Tenant, "aml brand", "aml-b-"+uuid.NewString())
		w.Brand = b
		return err
	}); err != nil {
		t.Fatalf("create brand: %v", err)
	}
	w.Scope = "platform"
	if licensingModel == "own_licence" {
		w.Scope = "tenant"
	}
	return w
}

// player registers a synthetic player (a new Person) in the world's brand.
func (w *amlWorld) player() player {
	w.t.Helper()
	var p player
	if err := w.Owner.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		a, err := identity.RegisterPlayer(ctx, tx, w.Brand, "aml-"+uuid.NewString()+"@fixture.invalid", "!x")
		p = player{AccountID: a.ID, PersonID: a.PersonID}
		return err
	}); err != nil {
		w.t.Fatalf("register player: %v", err)
	}
	return p
}

// secondAccount links another account of the same Person in the same tenant (a second brand is not needed: the
// account/person pairing is what the hold trigger checks).
func (w *amlWorld) secondBrandAccount(person uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	if err := w.Owner.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		b, err := launchfix.CreateBrand(ctx, tx, w.Tenant, "aml brand 2", "aml-b2-"+uuid.NewString())
		if err != nil {
			return err
		}
		a, err := identity.RegisterPlayerLinkedToPerson(ctx, tx, b, "aml-"+uuid.NewString()+"@fixture.invalid", "!x", person)
		id = a.ID
		return err
	}); err != nil {
		w.t.Fatalf("second account: %v", err)
	}
	return id
}

// asUnprivileged runs fn on the runtime pool in a plain tenant session (no officer shape at all).
func (w *amlWorld) tenantTx(fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.RT.WithTenant(context.Background(), w.Tenant, fn)
}

// requestAMLGrantAsSessionActor files an aml_officer grant request as whoever the open session resolves to (the test proof hook
// signs for the session's own actor, exactly as the real issuer signs for the authenticated principal).
func requestAMLGrantAsSessionActorID(ctx context.Context, tx pgx.Tx, tenant, grantee uuid.UUID) (uuid.UUID, error) {
	req, err := capability.CreateRequest(ctx, tx, tenant, capability.NewRequestInput{
		GranteeStaffID: grantee, Capability: capability.CapabilityAMLOfficer, ValidFrom: time.Now(), ReasonCode: "t",
	})
	return req.ID, err
}

// registerFixturePlayer registers a synthetic player (a new Person) in brand inside tx.
func registerFixturePlayer(ctx context.Context, tx pgx.Tx, brand identity.Brand) (identity.PlayerAccount, error) {
	return identity.RegisterPlayer(ctx, tx, brand, "aml-"+uuid.NewString()+"@fixture.invalid", "!x")
}
