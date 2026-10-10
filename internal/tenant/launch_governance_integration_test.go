//go:build integration

// ADR 0112 slice 1 (migration 0128): adversarial tests of the status-governance
// guards. Every assertion that is about what the RUNTIME role can or cannot do runs as
// igaming_runtime (non-superuser, non-BYPASSRLS, asserted by brandPinRuntimePoolAt);
// the owner pool is used only to seed fixtures (the LF2 path) and for owner-only probes.
// Requires TEST_DATABASE_URL and TEST_RUNTIME_DATABASE_URL; a skip is NOT evidence.
package tenant

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

type lgWorld struct {
	owner, rt *db.Pool
	ops       launchfix.OperatorIDs
}

func newLG(t *testing.T) *lgWorld {
	t.Helper()
	w := &lgWorld{owner: brandPinOwnerPool(t), rt: brandPinRuntimePool(t)}
	launchfix.SeedOperators(t)
	w.ops = launchfix.Operators()
	return w
}

func pgCodeOf(err error) string {
	var pe *pgconn.PgError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return ""
}

func wantCode(t *testing.T, what string, err error, code string) {
	t.Helper()
	if got := pgCodeOf(err); got != code {
		t.Fatalf("%s: want SQLSTATE %s, got %q (err=%v)", what, code, got, err)
	}
}

// platformRT runs fn as a platform principal on the RUNTIME pool.
func (w *lgWorld) platformRT(staff uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.rt.WithPlatformAdmin(context.Background(), staff, fn)
}

// tenantStaff creates an active tenant staff member linked to a Person (owner fixture).
func (w *lgWorld) tenantStaff(t *testing.T, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	var staff identity.StaffUser
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		p, err := identity.CreatePerson(ctx, tx)
		if err != nil {
			return err
		}
		staff, err = identity.CreateStaffUser(ctx, tx, tenantID, "lg-"+uuid.NewString()+"@fixture.invalid", "!x", identity.StaffRoleTenantAdmin, &p.ID)
		return err
	}); err != nil {
		t.Fatalf("seed tenant staff: %v", err)
	}
	return staff.ID
}

func (w *lgWorld) tenantRT(tenantID, staff uuid.UUID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	return w.rt.WithPrincipalScope(context.Background(), tenantID, staff, fn)
}

// pendingBrand creates a pending_launch brand of an existing tenant through the RUNTIME role.
func (w *lgWorld) pendingBrand(t *testing.T, tenantID uuid.UUID) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if err := w.rt.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'lg pending')`, id, tenantID, "lg-"+id.String())
		return err
	}); err != nil {
		t.Fatalf("create pending brand: %v", err)
	}
	return id
}

func (w *lgWorld) brandStatus(t *testing.T, tenantID, brandID uuid.UUID) string {
	t.Helper()
	var s string
	if err := w.owner.WithTenant(context.Background(), tenantID, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM brands WHERE id = $1`, brandID).Scan(&s)
	}); err != nil {
		t.Fatalf("read brand status: %v", err)
	}
	return s
}

func (w *lgWorld) tenantStatus(t *testing.T, tenantID uuid.UUID) string {
	t.Helper()
	var s string
	if err := w.owner.WithPlatformAdmin(context.Background(), w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM tenants WHERE id = $1`, tenantID).Scan(&s)
	}); err != nil {
		t.Fatalf("read tenant status: %v", err)
	}
	return s
}

// lgRequest inserts a request in the CURRENT transaction (whatever session shape it holds) and
// returns its id and payload hash.
func lgRequest(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, brandID *uuid.UUID, action string) (uuid.UUID, string, error) {
	kind := "tenant"
	if brandID != nil {
		kind = "brand"
	}
	needsLicensing := action == "activate" || action == "reactivate" || action == "ratify"
	var model, lstatus, operator *string
	var juris []uuid.UUID
	if needsLicensing {
		m, s, o := "other_manually_approved", "conditional", "lg operator"
		model, lstatus, operator = &m, &s, &o
		juris = []uuid.UUID{uuid.New()}
	}
	id := uuid.New()
	var hash string
	err := tx.QueryRow(ctx, `
		INSERT INTO launch_authorisation_requests
		    (id, tenant_id, subject_kind, brand_id, action, reason_code, readiness_snapshot, readiness_snapshot_hash,
		     licensing_model, licensing_status, responsible_operator_name, jurisdiction_ids)
		VALUES ($1, $2, $3, $4, $5, 'lg_test', '{}'::jsonb, repeat('0', 64), $6, $7, $8, COALESCE($9::uuid[], '{}'))
		RETURNING payload_hash`,
		id, tenantID, kind, brandID, action, model, lstatus, operator, juris).Scan(&hash)
	return id, hash, err
}

func lgSetPlatform(ctx context.Context, tx pgx.Tx, staff uuid.UUID) error {
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `SELECT set_config('app.platform_admin_principal_id', $1, true)`, staff.String())
	return err
}

func lgApprove(ctx context.Context, tx pgx.Tx, reqID uuid.UUID, hash, decision string) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO launch_authorisation_approvals (request_id, decision, payload_hash, readiness_snapshot_hash_at_decision, reason_code)
		VALUES ($1, $2, $3, repeat('0', 64), 'lg_test')`, reqID, decision, hash)
	return err
}

// lgExecuteBrand runs executing -> governed transition -> S6 brand UPDATE -> executed in the
// current (platform-session) transaction.
func lgExecuteBrand(ctx context.Context, tx pgx.Tx, reqID, tenantID, brandID uuid.UUID, from, to string) error {
	if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, reqID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
		VALUES ($1, 'brand', $2, 'governed', $3, $4, $5)`, tenantID, brandID, from, to, reqID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, tenantID.String()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE brands SET status = $1 WHERE id = $2`, to, brandID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1`, reqID)
	return err
}

func (w *lgWorld) transitionKinds(t *testing.T, tenantID uuid.UUID, brandID *uuid.UUID) []string {
	t.Helper()
	var kinds []string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `SELECT kind || ':' || COALESCE(from_status, '-') || '>' || to_status FROM launch_status_transitions
			WHERE tenant_id = $1 AND brand_id IS NOT DISTINCT FROM $2 ORDER BY created_at, id`, tenantID, brandID)
		if err != nil {
			return err
		}
		kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatalf("read transitions: %v", err)
	}
	return kinds
}

// ---------------------------------------------------------------------------
// LF2 / LA021: the runtime role can only create pending_launch rows.
// ---------------------------------------------------------------------------

func TestLaunchGov_SubjectInsert_RuntimeCreatesPendingOnly_OwnerProvisioningIsRecorded(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()

	for _, st := range []string{"active", "suspended", "closed"} {
		err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model, status) VALUES ($1, 'lg', $2, 'under_platform_licence', $3)`,
				uuid.New(), "lg-"+uuid.NewString(), st)
			return err
		})
		wantCode(t, "runtime INSERT tenants status="+st, err, "LA021")
	}

	// Default and explicit pending_launch are admitted; no transition row is written for them.
	tid := uuid.New()
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'lg', $2, 'under_platform_licence')`, tid, "lg-"+tid.String())
		return err
	}); err != nil {
		t.Fatalf("runtime INSERT tenants (default status): %v", err)
	}
	if got := w.tenantStatus(t, tid); got != "pending_launch" {
		t.Fatalf("default tenant status = %q, want pending_launch", got)
	}
	if k := w.transitionKinds(t, tid, nil); len(k) != 0 {
		t.Fatalf("a pending_launch tenant must have no transition row, got %v", k)
	}

	// Brands: the runtime role under the tenant's own scope.
	for _, st := range []string{"active", "suspended", "closed"} {
		err := w.rt.WithTenant(ctx, tid, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name, status) VALUES ($1, $2, $3, 'lg', $4)`, uuid.New(), tid, "lg-"+uuid.NewString(), st)
			return err
		})
		wantCode(t, "runtime INSERT brands status="+st, err, "LA021")
	}
	b := w.pendingBrand(t, tid)
	if got := w.brandStatus(t, tid, b); got != "pending_launch" {
		t.Fatalf("default brand status = %q, want pending_launch", got)
	}

	// Owner provisioning (the LF2 fixture path) is allowed and recorded as owner_provisioned.
	f := seedBrandPinTenant(t, w.owner)
	if k := w.transitionKinds(t, f.tenantID, nil); len(k) != 1 || k[0] != "owner_provisioned:->active" {
		t.Fatalf("owner-provisioned active tenant must record one owner_provisioned transition, got %v", k)
	}
	if k := w.transitionKinds(t, f.tenantID, &f.brandID); len(k) != 1 || k[0] != "owner_provisioned:->active" {
		t.Fatalf("owner-provisioned active brand must record one owner_provisioned transition, got %v", k)
	}
}

// ---------------------------------------------------------------------------
// S10 / S6: nobody changes a status by SQL.
// ---------------------------------------------------------------------------

func TestLaunchGov_StatusGuard_S10_S6_NoSQLSessionChangesAStatus(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()
	a := seedBrandPinTenant(t, w.owner)
	b := seedBrandPinTenant(t, w.owner)

	// A tenant-scoped RUNTIME session cannot rewrite brands.status of its own brand.
	for _, to := range []string{"suspended", "closed", "pending_launch"} {
		err := w.rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE brands SET status = $1 WHERE id = $2`, to, a.brandID)
			return err
		})
		wantCode(t, "tenant-scoped brands.status -> "+to, err, "LA020")
	}
	// ... nor a principal-scoped (staff) tenant session.
	staff := w.tenantStaff(t, a.tenantID)
	err := w.tenantRT(a.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET status = 'closed' WHERE id = $1`, a.brandID)
		return err
	})
	wantCode(t, "tenant staff brands.status -> closed", err, "LA020")

	// A platform RUNTIME session cannot change tenants.status.
	for _, to := range []string{"suspended", "closed"} {
		err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE tenants SET status = $1 WHERE id = $2`, to, a.tenantID)
			return err
		})
		wantCode(t, "platform tenants.status -> "+to, err, "LA020")
	}
	if got := w.brandStatus(t, a.tenantID, a.brandID); got != "active" {
		t.Fatalf("a refused brand status change changed the row: %s", got)
	}
	if got := w.tenantStatus(t, a.tenantID); got != "active" {
		t.Fatalf("a refused tenant status change changed the row: %s", got)
	}

	// S10 semantics pinned: an UPDATE that sets status to its OWN value plus another column
	// is not refused and needs no transition.
	if err := w.rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET status = 'active', name = 'lg renamed' WHERE id = $1`, a.brandID)
		return err
	}); err != nil {
		t.Fatalf("same-value status UPDATE with another column must be admitted: %v", err)
	}
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'active', name = 'lg renamed' WHERE id = $1`, a.tenantID)
		return err
	}); err != nil {
		t.Fatalf("same-value tenants UPDATE with another column must be admitted: %v", err)
	}
	if k := w.transitionKinds(t, a.tenantID, &a.brandID); len(k) != 1 {
		t.Fatalf("a same-value UPDATE must not write a transition, got %v", k)
	}

	// S6 cross-tenant: tenant A's session cannot touch tenant B's brand at all (RLS filters
	// the row; 0 rows), and the status is unchanged.
	if err := w.rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE brands SET status = 'suspended' WHERE id = $1`, b.brandID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 0 {
			return fmt.Errorf("cross-tenant brands UPDATE affected %d rows", tag.RowsAffected())
		}
		return nil
	}); err != nil {
		t.Fatalf("cross-tenant brands update: %v", err)
	}
	if got := w.brandStatus(t, b.tenantID, b.brandID); got != "active" {
		t.Fatalf("tenant B's brand changed: %s", got)
	}

	// A PAST governed transition authorises nothing later: after a governed suspension (its own
	// transaction, long committed) a raw reactivation is still refused.
	launchfix.SetBrandStatus(t, a.tenantID, a.brandID, "suspended")
	err = w.rt.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET status = 'active' WHERE id = $1`, a.brandID)
		return err
	})
	wantCode(t, "raw reactivation after an earlier governed suspension", err, "LA020")
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, a.tenantID)
		return err
	})
	wantCode(t, "raw tenant suspension", err, "LA020")
	launchfix.SetBrandStatus(t, a.tenantID, a.brandID, "active")
	// ... including the SAME move again: a stale governed transition active -> suspended (committed
	// earlier) does not authorise a new raw active -> suspended.
	// (A principal-scoped session can READ the decision records of its tenant, so the refusal is the
	// guard's same-transaction binding, not merely invisibility of the rows; a bare tenant-GUC session
	// sees no decision record at all.)
	err = w.tenantRT(a.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET status = 'suspended' WHERE id = $1`, a.brandID)
		return err
	})
	wantCode(t, "raw suspension repeating a stale governed move", err, "LA020")
	// The same for a tenant row, from a platform session (which can read every decision record).
	launchfix.SetTenantStatus(t, a.tenantID, "suspended")
	launchfix.SetTenantStatus(t, a.tenantID, "active")
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE tenants SET status = 'suspended' WHERE id = $1`, a.tenantID)
		return err
	})
	wantCode(t, "raw tenant suspension repeating a stale governed move", err, "LA020")

	// Closed is never left, by any writer (the owner is bound by triggers too).
	launchfix.SetBrandStatus(t, a.tenantID, a.brandID, "closed")
	err = w.owner.WithTenant(ctx, a.tenantID, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE brands SET status = 'active' WHERE id = $1`, a.brandID)
		return err
	})
	wantCode(t, "owner reopening a closed brand", err, "LA020")
	if err := launchfix.TrySetBrandStatus(ctx, a.tenantID, a.brandID, "active"); err == nil {
		t.Fatal("the governed fixture must not be able to reopen a closed brand")
	}
	// A request to suspend a closed subject is illegal at INSERT time (LA010).
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := lgRequest(ctx, tx, a.tenantID, &a.brandID, "suspend")
		return err
	})
	wantCode(t, "suspend request on a closed brand", err, "LA010")
}

// ---------------------------------------------------------------------------
// S2: transition INSERT.
// ---------------------------------------------------------------------------

func TestLaunchGov_TransitionInsert_S2(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()
	f := seedBrandPinTenant(t, w.owner)

	ins := func(kind string, from *string, req *uuid.UUID) string {
		return fmt.Sprintf(`INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
			VALUES ('%s', 'brand', '%s', '%s', $1, 'suspended', $2)`, f.tenantID, f.brandID, kind)
	}
	active := "active"

	// governed with no / unknown request.
	err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, ins("governed", &active, nil), &active, uuid.New())
		return err
	})
	wantCode(t, "governed transition for an unknown request", err, "LA013")

	// governed against a request that is merely pending (not executing).
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, ins("governed", &active, nil), &active, id)
		return err
	})
	wantCode(t, "governed transition for a pending request", err, "LA013")

	// governed with statuses that differ from the executing request's, or for another subject.
	other := w.pendingBrand(t, f.tenantID)
	governedFor := func(brand uuid.UUID, from, to string) error {
		return w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
				VALUES ($1, 'brand', $2, 'governed', $3, $4, $5)`, f.tenantID, brand, from, to, id)
			return err
		})
	}
	wantCode(t, "governed transition whose to_status differs from the request", governedFor(f.brandID, "active", "closed"), "LA013")
	wantCode(t, "governed transition whose from_status differs from the request", governedFor(f.brandID, "pending_launch", "suspended"), "LA013")
	wantCode(t, "governed transition for another subject than the request's", governedFor(other, "active", "suspended"), "LA013")

	// legacy_baseline / owner_provisioned from the RUNTIME role.
	for _, kind := range []string{"legacy_baseline", "owner_provisioned"} {
		err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, ins(kind, nil, nil), nil, nil)
			return err
		})
		// The platform INSERT policy admits only governed rows, so the runtime role is refused
		// by RLS (42501) or, were that arm ever widened, by the guard (LA013): either is a refusal.
		if c := pgCodeOf(err); c != "LA013" && c != "42501" {
			t.Fatalf("runtime %s transition: want LA013 or 42501, got %q (%v)", kind, c, err)
		}
		err = w.rt.WithTenant(ctx, f.tenantID, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, ins(kind, nil, nil), nil, nil)
			return err
		})
		if c := pgCodeOf(err); c != "LA013" && c != "42501" {
			t.Fatalf("tenant-scoped runtime %s transition: want LA013 or 42501, got %q (%v)", kind, c, err)
		}
	}

	// The owner: from_status must be NULL; legacy_baseline is refused if any transition exists.
	err = w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, ins("owner_provisioned", &active, nil), &active, nil)
		return err
	})
	wantCode(t, "owner owner_provisioned with from_status", err, "LA013")
	err = w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, ins("legacy_baseline", nil, nil), nil, nil)
		return err
	})
	wantCode(t, "owner legacy_baseline when a transition already exists", err, "LA013")
}

// ---------------------------------------------------------------------------
// LA030 deferred commit checks.
// ---------------------------------------------------------------------------

func TestLaunchGov_DeferredChecks_LA030(t *testing.T) {
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)

	// A request left 'executing' at commit.
	err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id)
		return err
	})
	wantCode(t, "request left executing at commit", err, "LA030")

	// A governed transition with no subject UPDATE: the request ends executed (the executed
	// guard only needs the transition) but the subject status does not match.
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
			VALUES ($1, 'brand', $2, 'governed', 'active', 'suspended', $3)`, f.tenantID, f.brandID, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1`, id)
		return err
	})
	wantCode(t, "governed transition without the subject UPDATE", err, "LA030")
	if got := w.brandStatus(t, f.tenantID, f.brandID); got != "active" {
		t.Fatalf("a refused commit changed the brand: %s", got)
	}

	// A refused_at_execution request that nevertheless has a governed transition.
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
			VALUES ($1, 'brand', $2, 'governed', 'active', 'suspended', $3)`, f.tenantID, f.brandID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE brands SET status = 'suspended' WHERE id = $1`, f.brandID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', '', true)`); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'refused_at_execution', refusal_code = 'lg' WHERE id = $1`, id)
		return err
	})
	wantCode(t, "refused_at_execution with a governed transition", err, "LA030")
	if got := w.brandStatus(t, f.tenantID, f.brandID); got != "active" {
		t.Fatalf("a refused commit changed the brand: %s", got)
	}
}

// ---------------------------------------------------------------------------
// S1 / S3 / S8: the request state machine; LA012 approvals; positive end-to-end flows.
// ---------------------------------------------------------------------------

func TestLaunchGov_Requests_S1_ExecutionConditions(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()
	f := seedBrandPinTenant(t, w.owner)
	brand := w.pendingBrand(t, f.tenantID)
	tid := f.tenantID

	// pending -> executing with no approvals.
	err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, tid, &brand, "activate")
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id)
		return err
	})
	wantCode(t, "executing with too few approvals", err, "LA011")

	// Approvals from an EARLIER transaction only: request + one approval commit; a later
	// transaction with no new approval cannot execute (the final approval must be in the
	// executing transaction).
	var reqID uuid.UUID
	var hash string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, hash, err = lgRequest(ctx, tx, tid, &brand, "activate")
		if err != nil {
			return err
		}
		if err := lgSetPlatform(ctx, tx, w.ops.Approver1); err != nil {
			return err
		}
		return lgApprove(ctx, tx, reqID, hash, "approve")
	}); err != nil {
		t.Fatalf("request + first approval: %v", err)
	}
	err = w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, reqID)
		return err
	})
	wantCode(t, "executing with approvals from an earlier transaction only", err, "LA011")

	// pending -> rejected without a same-transaction reject decision.
	err = w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'rejected' WHERE id = $1`, reqID)
		return err
	})
	wantCode(t, "rejected without a same-transaction reject", err, "LA011")

	// Only the requester may cancel; the statuses are otherwise terminal.
	err = w.platformRT(w.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1`, reqID)
		return err
	})
	wantCode(t, "cancel by someone other than the requester", err, "LA011")

	// Positive: a second distinct Person approves AND executes in one transaction.
	if err := w.platformRT(w.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error {
		if err := lgApprove(ctx, tx, reqID, hash, "approve"); err != nil {
			return err
		}
		return lgExecuteBrand(ctx, tx, reqID, tid, brand, "pending_launch", "active")
	}); err != nil {
		t.Fatalf("governed activation must succeed: %v", err)
	}
	if got := w.brandStatus(t, tid, brand); got != "active" {
		t.Fatalf("brand status after a governed activation: %s", got)
	}
	k := w.transitionKinds(t, tid, &brand)
	if len(k) != 1 || k[0] != "governed:pending_launch>active" {
		t.Fatalf("transition history: %v", k)
	}
	// The history row names the executor and both approvers.
	var executedBy uuid.UUID
	var approvers []uuid.UUID
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT executed_by, approver_ids FROM launch_status_transitions WHERE brand_id = $1`, brand).Scan(&executedBy, &approvers)
	}); err != nil {
		t.Fatal(err)
	}
	if executedBy != w.ops.Approver2 || len(approvers) != 2 {
		t.Fatalf("executed_by=%s approvers=%v", executedBy, approvers)
	}

	// Executed is terminal: the request cannot be touched again.
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1`, reqID)
		return err
	})
	wantCode(t, "touching an executed request", err, "LA011")

	// pending -> rejected WITH a same-transaction reject works (second brand).
	brand2 := w.pendingBrand(t, tid)
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, h, err := lgRequest(ctx, tx, tid, &brand2, "activate")
		if err != nil {
			return err
		}
		if err := lgSetPlatform(ctx, tx, w.ops.Approver1); err != nil {
			return err
		}
		if err := lgApprove(ctx, tx, id, h, "reject"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'rejected' WHERE id = $1`, id)
		return err
	}); err != nil {
		t.Fatalf("rejection with a same-transaction reject decision: %v", err)
	}

	// Content is immutable: even a privileged writer (owner, in platform scope) is refused by the
	// guard, and the runtime role has no UPDATE privilege on content columns at all.
	brand3 := w.pendingBrand(t, tid)
	var r3 uuid.UUID
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		r3, _, err = lgRequest(ctx, tx, tid, &brand3, "activate")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET reason_code = 'tampered' WHERE id = $1`, r3)
		return err
	})
	wantCode(t, "owner content tampering", err, "LA011")
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET reason_code = 'tampered' WHERE id = $1`, r3)
		return err
	})
	wantCode(t, "runtime content tampering (no column privilege)", err, "42501")
	// The requester can cancel; a second cancel is refused (terminal).
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1`, r3)
		return err
	}); err != nil {
		t.Fatalf("requester cancel: %v", err)
	}
}

func TestLaunchGov_Approvals_LA012_And_LA001(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()
	f := seedBrandPinTenant(t, w.owner)
	brand := w.pendingBrand(t, f.tenantID)
	var reqID uuid.UUID
	var hash string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, hash, err = lgRequest(ctx, tx, f.tenantID, &brand, "activate")
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Self-approval by staff id.
	err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") })
	wantCode(t, "self-approval by staff id", err, "LA012")

	// A SECOND account of the same Person as the requester.
	second := uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, person_id)
			VALUES ($1, NULL, $2, '!x', 'platform_admin', $3)`, second, "lg-second-"+second.String()+"@fixture.invalid", w.ops.Persons[0])
		return err
	}); err != nil {
		t.Fatalf("seed second account: %v", err)
	}
	err = w.platformRT(second, func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") })
	wantCode(t, "self-approval by a second account of the same Person", err, "LA012")

	// A tenant session (or a plain tenant principal) can never approve.
	ts := w.tenantStaff(t, f.tenantID)
	err = w.tenantRT(f.tenantID, ts, func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") })
	if c := pgCodeOf(err); c != "LA001" && c != "42501" {
		t.Fatalf("tenant-session approval: want LA001 (or an RLS refusal 42501), got %q (%v)", c, err)
	}

	// A different payload hash.
	err = w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		return lgApprove(ctx, tx, reqID, "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", "approve")
	})
	wantCode(t, "approval with a different payload_hash", err, "LA012")

	// A platform session whose principal is not an active platform staff row.
	err = w.platformRT(uuid.New(), func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") })
	wantCode(t, "approval by an unknown platform principal", err, "LA001")

	// An acting-session GUC shape is refused outright by the actor resolver (LA001) and sees
	// nothing through the restrictive acting fence.
	err = w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		// A valid platform principal PLUS the acting GUCs: the mixed shape must be refused by the
		// actor resolver itself (LA001), not merely by the RLS acting fence further down.
		if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true), set_config('app.acting_platform_principal_id', $2, true), set_config('app.platform_admin_principal_id', $2, true)`,
			f.tenantID.String(), w.ops.Approver1.String()); err != nil {
			return err
		}
		return lgApprove(ctx, tx, reqID, hash, "approve")
	})
	wantCode(t, "acting-session approval", err, "LA001")
	var visible int
	if err := w.rt.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.acting_tenant_id', $1, true), set_config('app.acting_platform_principal_id', $2, true)`,
			f.tenantID.String(), w.ops.Approver1.String()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM launch_authorisation_requests) + (SELECT count(*) FROM launch_authorisation_approvals) + (SELECT count(*) FROM launch_status_transitions)`).Scan(&visible)
	}); err != nil || visible != 0 {
		t.Fatalf("an acting-shaped session must see none of the three tables: n=%d err=%v", visible, err)
	}

	// One Person decides once.
	if err := w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") }); err != nil {
		t.Fatalf("a distinct Person's approval must be admitted: %v", err)
	}
	err = w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") })
	wantCode(t, "the same Person deciding twice", err, "LA012")

	// A decided (cancelled) request takes no further approval.
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'cancelled' WHERE id = $1`, reqID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err = w.platformRT(w.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") })
	wantCode(t, "approval on a cancelled request", err, "LA012")
}

// S8 + S14: a tenant session may execute only the suspension of its OWN brand; a tenant
// requester's platform_licence activation needs two platform approvers.
func TestLaunchGov_TenantSession_S8_S14(t *testing.T) {
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)
	other := seedBrandPinTenant(t, w.owner)
	staff := w.tenantStaff(t, f.tenantID)

	// Positive S8: the tenant suspends its own active brand alone (0 approvals), in one transaction.
	if err := w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
			VALUES ($1, 'brand', $2, 'governed', 'active', 'suspended', $3)`, f.tenantID, f.brandID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE brands SET status = 'suspended' WHERE id = $1`, f.brandID); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1`, id)
		return err
	}); err != nil {
		t.Fatalf("S8: a tenant session suspending its own brand must succeed: %v", err)
	}
	if got := w.brandStatus(t, f.tenantID, f.brandID); got != "suspended" {
		t.Fatalf("brand status %s", got)
	}

	// A tenant session requesting a TENANT subject (LA001) or another tenant's brand.
	err := w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := lgRequest(ctx, tx, f.tenantID, nil, "suspend")
		return err
	})
	wantCode(t, "tenant session requesting a tenant subject", err, "LA001")
	err = w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := lgRequest(ctx, tx, other.tenantID, &other.brandID, "suspend")
		return err
	})
	if c := pgCodeOf(err); c != "LA001" && c != "42501" {
		t.Fatalf("cross-tenant request: want LA001 or 42501, got %q (%v)", c, err)
	}

	// A tenant session may not execute an activation (S8): request is allowed, execution is not.
	pb := w.pendingBrand(t, f.tenantID)
	var reqID uuid.UUID
	if err := w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, _, err = lgRequest(ctx, tx, f.tenantID, &pb, "activate")
		return err
	}); err != nil {
		t.Fatalf("a tenant session may request an activation of its brand: %v", err)
	}
	err = w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, reqID)
		return err
	})
	wantCode(t, "tenant session executing an activation", err, "LA011")

	// Reactivating the suspended brand is four-eyes: a tenant-scoped suspender cannot do it alone.
	var re uuid.UUID
	if err := w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		re, _, err = lgRequest(ctx, tx, f.tenantID, &f.brandID, "reactivate")
		return err
	}); err != nil {
		t.Fatalf("reactivation request: %v", err)
	}
	err = w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, re)
		return err
	})
	wantCode(t, "tenant session executing a reactivation", err, "LA011")

	// S14: required_approvals is forced: a tenant-scoped platform_licence activation needs 2,
	// a platform-requested one 1; a caller-supplied value is overwritten.
	var n int
	if err := w.tenantRT(f.tenantID, staff, func(ctx context.Context, tx pgx.Tx) error {
		pb2 := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO brands (id, tenant_id, slug, name) VALUES ($1, $2, $3, 'lg s14')`, pb2, f.tenantID, "lg-"+pb2.String()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			INSERT INTO launch_authorisation_requests
			    (tenant_id, subject_kind, brand_id, action, reason_code, readiness_snapshot, readiness_snapshot_hash,
			     licensing_model, licensing_status, responsible_operator_name, jurisdiction_ids, required_approvals)
			VALUES ($1, 'brand', $2, 'activate', 'lg', '{}'::jsonb, repeat('0', 64), 'platform_licence', 'in_force', 'op', ARRAY[gen_random_uuid()], 0)
			RETURNING required_approvals`, f.tenantID, pb2).Scan(&n)
	}); err != nil {
		t.Fatalf("S14 request: %v", err)
	}
	if n != 2 {
		t.Fatalf("S14: a tenant-requested platform_licence activation needs 2 approvals (a caller value must be overwritten), got %d", n)
	}
}

// S1 concurrency at the database: two final approvals racing on the same request produce
// exactly one execution (the approval guard locks the request FOR UPDATE).
func TestLaunchGov_TwoFinalApprovalsRacing_OneExecution(t *testing.T) {
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)
	for i := 0; i < 20; i++ {
		brand := w.pendingBrand(t, f.tenantID)
		var reqID uuid.UUID
		var hash string
		if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			reqID, hash, err = lgRequest(ctx, tx, f.tenantID, &brand, "activate")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for j, staff := range []uuid.UUID{w.ops.Approver1, w.ops.Approver2} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[j] = w.platformRT(staff, func(ctx context.Context, tx pgx.Tx) error {
					if err := lgApprove(ctx, tx, reqID, hash, "approve"); err != nil {
						return err
					}
					return lgExecuteBrand(ctx, tx, reqID, f.tenantID, brand, "pending_launch", "active")
				})
			}()
		}
		wg.Wait()
		ok := 0
		for _, e := range errs {
			switch {
			case e == nil:
				ok++
			case pgCodeOf(e) == "LA012":
			default:
				t.Fatalf("iteration %d: the losing execution must be refused LA012, got %v", i, e)
			}
		}
		if ok != 1 {
			t.Fatalf("iteration %d: want exactly one execution, got %d (%v)", i, ok, errs)
		}
		if k := w.transitionKinds(t, f.tenantID, &brand); len(k) != 1 {
			t.Fatalf("iteration %d: want one governed transition, got %v", i, k)
		}
	}
}

// S3: a stale pending request does not block a new one; a suspension is never blocked by a
// pending activation; superseded only in the txid executing a suspension.
func TestLaunchGov_S3_ExpiryAndSuspensionNeverBlocked(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()
	f := seedBrandPinTenant(t, w.owner)
	pb := w.pendingBrand(t, f.tenantID)

	// One pending request per subject (activation); a second one is refused by the UNIQUE.
	var first uuid.UUID
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		first, _, err = lgRequest(ctx, tx, f.tenantID, &pb, "activate")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := lgRequest(ctx, tx, f.tenantID, &pb, "activate")
		return err
	})
	wantCode(t, "a second pending request for the same subject", err, "23505")

	// Expiry is computed against expires_at, which only the INSERT guard sets (72 h): a fresh
	// request cannot be expired early.
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'expired' WHERE id = $1`, first)
		return err
	})
	wantCode(t, "expiring a request before expires_at", err, "LA011")

	// To age a request the test needs a clock the product does not have: inside ONE owner
	// transaction the content guard is suspended just for the timestamp (transactional DDL; an
	// error aborts and the trigger stays enabled), exactly like the R3 closure fixtures.
	age := func(id uuid.UUID) {
		t.Helper()
		if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `ALTER TABLE launch_authorisation_requests DISABLE TRIGGER launch_requests_guard`); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET created_at = now() - interval '80 hours', expires_at = now() - interval '8 hours' WHERE id = $1`, id); err != nil {
				return err
			}
			// The UPDATE queued the deferred LA030 check; run it now (ALTER TABLE refuses while
			// trigger events are pending), then re-enable the guard.
			if _, err := tx.Exec(ctx, `SET CONSTRAINTS ALL IMMEDIATE`); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `ALTER TABLE launch_authorisation_requests ENABLE TRIGGER launch_requests_guard`)
			return err
		}); err != nil {
			t.Fatalf("age request: %v", err)
		}
	}
	age(first)
	// The stale request neither accepts an approval nor executes.
	err = w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		return lgApprove(ctx, tx, first, "0000000000000000000000000000000000000000000000000000000000000000", "approve")
	})
	wantCode(t, "approving an expired request", err, "LA012")
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, first)
		return err
	})
	wantCode(t, "executing an expired request", err, "LA011")
	// ... and it does not block a new request for the subject: the INSERT guard moves it to expired.
	var second uuid.UUID
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		second, _, err = lgRequest(ctx, tx, f.tenantID, &pb, "activate")
		return err
	}); err != nil {
		t.Fatalf("a pending request past expires_at must not block a new one: %v", err)
	}
	var firstStatus string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM launch_authorisation_requests WHERE id = $1`, first).Scan(&firstStatus)
	}); err != nil || firstStatus != "expired" {
		t.Fatalf("stale request status = %q err=%v, want expired", firstStatus, err)
	}
	first = second

	// A suspension request is not blocked by the pending activation of a DIFFERENT subject state:
	// use the active brand f.brandID with a pending reactivation-like closure first.
	var closeReq uuid.UUID
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		closeReq, _, err = lgRequest(ctx, tx, f.tenantID, &f.brandID, "close")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		susp, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		// Execute the suspension: it supersedes the pending closure in the same transaction.
		if err := lgExecuteBrand(ctx, tx, susp, f.tenantID, f.brandID, "active", "suspended"); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'superseded' WHERE id = $1`, closeReq)
		return err
	}); err != nil {
		t.Fatalf("a suspension must execute despite a pending closure and supersede it: %v", err)
	}
	var st string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status FROM launch_authorisation_requests WHERE id = $1`, closeReq).Scan(&st)
	}); err != nil || st != "superseded" {
		t.Fatalf("pending closure after the suspension: %q err=%v", st, err)
	}
	// Superseding outside such a transaction is refused.
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'superseded' WHERE id = $1`, first)
		return err
	})
	wantCode(t, "superseding without an executing suspension", err, "LA011")
}

// The owner-only predicate itself: true for the table owner, false for the runtime role.
func TestLaunchGov_IsTableOwner_Predicate(t *testing.T) {
	w := newLG(t)
	var asOwner, asRuntime bool
	if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT launch_is_table_owner()`).Scan(&asOwner)
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.rt.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT launch_is_table_owner()`).Scan(&asRuntime)
	}); err != nil {
		t.Fatal(err)
	}
	if !asOwner || asRuntime {
		t.Fatalf("launch_is_table_owner(): owner=%v runtime=%v, want true/false", asOwner, asRuntime)
	}
}

// Four-eyes counts are enforced by NUMBER, not only by "an approval exists in this transaction":
// a tenant activation needs two distinct Persons; one is refused (LA011), the second executes.
func TestLaunchGov_TenantActivation_TwoApprovals_Required(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()
	tid := uuid.New()
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO tenants (id, name, slug, licensing_model) VALUES ($1, 'lg pending tenant', $2, 'under_platform_licence')`, tid, "lg-"+tid.String())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var reqID uuid.UUID
	var hash string
	var need int
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, hash, err = lgRequest(ctx, tx, tid, nil, "activate")
		if err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT required_approvals FROM launch_authorisation_requests WHERE id = $1`, reqID).Scan(&need)
	}); err != nil {
		t.Fatal(err)
	}
	if need != 2 {
		t.Fatalf("a tenant activation needs 2 approvals, got %d", need)
	}
	// One approval, in the executing transaction: refused by the COUNT.
	err := w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error {
		if err := lgApprove(ctx, tx, reqID, hash, "approve"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, reqID)
		return err
	})
	wantCode(t, "tenant activation with one approval", err, "LA011")
	// The approval rolled back with the failed transaction; approve for real, then execute with the second.
	if err := w.platformRT(w.ops.Approver1, func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") }); err != nil {
		t.Fatal(err)
	}
	if err := w.platformRT(w.ops.Approver2, func(ctx context.Context, tx pgx.Tx) error {
		if err := lgApprove(ctx, tx, reqID, hash, "approve"); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, reqID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO launch_status_transitions (tenant_id, subject_kind, kind, from_status, to_status, request_id)
			VALUES ($1, 'tenant', 'governed', 'pending_launch', 'active', $2)`, tid, reqID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE tenants SET status = 'active' WHERE id = $1`, tid); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1`, reqID)
		return err
	}); err != nil {
		t.Fatalf("two approvals must execute the tenant activation: %v", err)
	}
	if got := w.tenantStatus(t, tid); got != "active" {
		t.Fatalf("tenant status %s", got)
	}
	if k := w.transitionKinds(t, tid, nil); len(k) != 1 || k[0] != "governed:pending_launch>active" {
		t.Fatalf("transitions %v", k)
	}
	// A request cannot end 'executed' without its governed transition.
	f := seedBrandPinTenant(t, w.owner)
	err = w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executed' WHERE id = $1`, id)
		return err
	})
	wantCode(t, "executed without a governed transition", err, "LA011")
	_ = ctx
}

// An approver whose staff row has no linked Person is refused (distinct-Person checks need a Person).
func TestLaunchGov_Approver_WithoutPerson_Refused(t *testing.T) {
	w := newLG(t)
	ctx := context.Background()
	f := seedBrandPinTenant(t, w.owner)
	brand := w.pendingBrand(t, f.tenantID)
	var reqID uuid.UUID
	var hash string
	if err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		var err error
		reqID, hash, err = lgRequest(ctx, tx, f.tenantID, &brand, "activate")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	noPerson := uuid.New()
	if err := w.owner.WithPlatformAdmin(ctx, w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role) VALUES ($1, NULL, $2, '!x', 'platform_admin')`,
			noPerson, "lg-noperson-"+noPerson.String()+"@fixture.invalid")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	err := w.platformRT(noPerson, func(ctx context.Context, tx pgx.Tx) error { return lgApprove(ctx, tx, reqID, hash, "approve") })
	wantCode(t, "approval by a staff member without a Person", err, "LA012")
	// ... and such a staff member cannot request either.
	err = w.platformRT(noPerson, func(ctx context.Context, tx pgx.Tx) error {
		_, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		return err
	})
	wantCode(t, "request by a staff member without a Person", err, "LA001")
}

// A governed suspension authorises exactly its own transition: inside the same transaction the
// brand cannot be CLOSED (or moved anywhere else) on the strength of a suspension request.
func TestLaunchGov_GovernedSuspension_DoesNotAuthoriseAnotherMove(t *testing.T) {
	w := newLG(t)
	f := seedBrandPinTenant(t, w.owner)
	err := w.platformRT(w.ops.Requester, func(ctx context.Context, tx pgx.Tx) error {
		id, _, err := lgRequest(ctx, tx, f.tenantID, &f.brandID, "suspend")
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE launch_authorisation_requests SET status = 'executing' WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO launch_status_transitions (tenant_id, subject_kind, brand_id, kind, from_status, to_status, request_id)
			VALUES ($1, 'brand', $2, 'governed', 'active', 'suspended', $3)`, f.tenantID, f.brandID, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('app.tenant_id', $1, true)`, f.tenantID.String()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE brands SET status = 'closed' WHERE id = $1`, f.brandID)
		return err
	})
	wantCode(t, "closing on the strength of a suspension decision", err, "LA020")
	if got := w.brandStatus(t, f.tenantID, f.brandID); got != "active" {
		t.Fatalf("brand status %s", got)
	}
}
