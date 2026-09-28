//go:build integration

package adjustment

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/capability"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/scratchdb"
)

// grantTenantWindow issues a G-T grant with an explicit validity window.
func (w *world) grantTenantWindow(f staffMember, c capability.Capability, from time.Time, until *time.Time) uuid.UUID {
	w.t.Helper()
	ctx := context.Background()
	var reqID uuid.UUID
	if err := w.pool.WithPrincipalScope(ctx, w.Tenant, w.TenantAdmin.ID, func(ctx context.Context, tx pgx.Tx) error {
		r, err := capability.CreateRequest(ctx, tx, w.Tenant, capability.NewRequestInput{GranteeStaffID: f.ID, Capability: c, ValidFrom: from, ValidUntil: until, ReasonCode: "k2-window"})
		reqID = r.ID
		return err
	}); err != nil {
		w.t.Fatalf("windowed grant request: %v", err)
	}
	var gid uuid.UUID
	if err := w.pool.WithPlatformAdmin(ctx, w.GrantApprover.ID, func(ctx context.Context, tx pgx.Tx) error {
		_, g, err := capability.DecideAndGrant(ctx, tx, w.Tenant, reqID, "approve", "k2-window")
		if err == nil {
			gid = g.ID
		}
		return err
	}); err != nil {
		w.t.Fatalf("windowed grant approve: %v", err)
	}
	w.grantIDs[grantKey(f.ID, c)] = gid
	return gid
}

// setStaffStatus / setStaffRole mutate a tenant staff row by DB fixture
// (there is no suspend or role-change API: STAFF-LIFECYCLE-1).
func (w *world) setStaff(id uuid.UUID, column, value string) {
	w.t.Helper()
	if err := w.pool.WithTenant(context.Background(), w.Tenant, func(ctx context.Context, tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE staff_users SET `+column+` = $2 WHERE id = $1`, id, value)
		if err == nil && tag.RowsAffected() != 1 {
			w.t.Fatalf("set staff %s: %d rows", column, tag.RowsAffected())
		}
		return err
	}); err != nil {
		w.t.Fatalf("set staff %s: %v", column, err)
	}
}

// firstApprovalThenBreak submits a request needing 2 approvals, records a
// first approval by F2, breaks F2 with `breakIt`, then records the final
// approval by F3, returning the resulting outcome. F2's approval must NOT
// count at execution, so the request must stay pending.
func (w *world) firstApprovalThenBreak(t *testing.T, breakIt func()) Outcome {
	t.Helper()
	r, err := w.submit(w.F1, w.credit(100, ReasonOperationalErrorCorrection))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if r.RequiredAtSubmission != 2 {
		t.Fatalf("expected required 2, got %d", r.RequiredAtSubmission)
	}
	out, err := w.decide(w.F2, r, DecisionApprove)
	if err != nil || out.Executed || out.Counted != 1 {
		t.Fatalf("first approval: %v %+v", err, out)
	}
	breakIt()
	out, err = w.decide(w.F3, r, DecisionApprove)
	if err != nil {
		t.Fatalf("final approval: %v", err)
	}
	return out
}

func requireNotCounted(t *testing.T, w *world, out Outcome, what string) {
	t.Helper()
	if out.Executed || out.Request.State != StatePending || out.Counted != 1 {
		t.Fatalf("%s: the broken approval was counted (executed=%v state=%s counted=%d required=%d)",
			what, out.Executed, out.Request.State, out.Counted, out.Required)
	}
	w.assertInvariants()
}

// B-6 (AZ) and K2-G4 (use-time re-checks, security K2-P4 / architect I-6):
// an approval stops counting at execution when its approver is suspended
// (grantee active), demoted (eligible role), or its grant is revoked or has
// expired - each with its own mutant. The positive control executes.
func TestB6_K2G4_UseTimeRechecks(t *testing.T) {
	t.Run("positive control: two valid approvals execute", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 2})
		out := w.firstApprovalThenBreak(t, func() {})
		if !out.Executed || out.Counted != 2 {
			t.Fatalf("positive control did not execute: %+v", out)
		}
		w.assertInvariants()
	})
	t.Run("K2-G4 grantee active: suspended approver not counted", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 2})
		out := w.firstApprovalThenBreak(t, func() { w.setStaff(w.F2.ID, "status", "suspended") })
		requireNotCounted(t, w, out, "suspended approver")
	})
	t.Run("K2-G4 eligible role: demoted approver not counted", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 2})
		out := w.firstApprovalThenBreak(t, func() { w.setStaff(w.F2.ID, "role", "support") })
		requireNotCounted(t, w, out, "demoted approver")
	})
	t.Run("grant revoked before execution: not counted", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 2})
		out := w.firstApprovalThenBreak(t, func() { w.revokeGrant(w.F2.ID, capability.CapabilityLedgerAdjustmentApprove) })
		requireNotCounted(t, w, out, "revoked grant")
	})
	t.Run("grant expired before execution: not counted (in force at now())", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 2})
		f4 := w.staff(w.Tenant, "finance")
		until := time.Now().Add(3 * time.Second)
		w.grantTenantWindow(f4, capability.CapabilityLedgerAdjustmentApprove, time.Time{}, &until)
		r, err := w.submit(w.F1, w.credit(100, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		if out, err := w.decide(f4, r, DecisionApprove); err != nil || out.Counted != 1 {
			t.Fatalf("approval inside the window: %v %+v", err, out)
		}
		time.Sleep(time.Until(until) + 500*time.Millisecond)
		out, err := w.decide(w.F3, r, DecisionApprove)
		if err != nil {
			t.Fatal(err)
		}
		requireNotCounted(t, w, out, "expired grant")
	})
	t.Run("queued future grant is not in force: refused at insert", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 2})
		f5 := w.staff(w.Tenant, "finance")
		w.grantTenantWindow(f5, capability.CapabilityLedgerAdjustmentApprove, time.Now().Add(time.Hour), nil)
		r, err := w.submit(w.F1, w.credit(100, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.decide(f5, r, DecisionApprove); pgCode(err) != "MA003" {
			t.Fatalf("a not-yet-in-force (unrevoked) grant must not authorize: expected MA003, got %v", err)
		}
	})
	t.Run("suspended initiator: nothing executes", func(t *testing.T) {
		w := newWorld(t, worldOpts{base: 1})
		r, err := w.submit(w.F1, w.credit(100, ReasonOperationalErrorCorrection))
		if err != nil {
			t.Fatal(err)
		}
		w.setStaff(w.F1.ID, "status", "suspended")
		out, err := w.decide(w.F2, r, DecisionApprove)
		if err != nil {
			t.Fatal(err)
		}
		if out.Executed || out.Request.State != StatePending {
			t.Fatalf("suspended initiator's request executed: %+v", out)
		}
		w.assertInvariants()
	})
}

// scratchPoolThrough migrates a fresh scratch database with ONLY the
// migration files numbered <= version (ADR 0100 §19: K2 migration tests
// never depend on a later lane's migration).
func scratchPoolThrough(t *testing.T, prefix string, version int64) *db.Pool {
	t.Helper()
	pool, _ := scratchPoolThroughDir(t, prefix, version)
	return pool
}

// scratchPoolThroughDir is scratchPoolThrough that also returns the copied
// migrations directory (for MigrateDown/MigrateUp round trips).
func scratchPoolThroughDir(t *testing.T, prefix string, version int64) (*db.Pool, string) {
	t.Helper()
	src := "../../migrations"
	dir := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || len(name) < 4 {
			continue
		}
		n, perr := strconv.ParseInt(name[:4], 10, 64)
		if perr != nil || n > version {
			continue
		}
		b, rerr := os.ReadFile(filepath.Join(src, name))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(filepath.Join(dir, name), b, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	url := scratchdb.New(t, prefix)
	pool, err := db.Connect(context.Background(), url, 20, 5*time.Second)
	if err != nil {
		t.Fatalf("connect scratch: %v", err)
	}
	t.Cleanup(pool.Close)
	applied, err := pool.MigrateUp(context.Background(), dir)
	if err != nil {
		t.Fatalf("migrate scratch through %d: %v", version, err)
	}
	if len(applied) == 0 || applied[len(applied)-1] != version {
		t.Fatalf("expected %d last, got %v", version, applied)
	}
	return pool, dir
}

// K2-G4 tenant-unchanged and live-person = snapshot: both states are
// unreachable through any API or through ordinary RLS (a tenant session
// cannot move a staff row to another tenant; migration 0034 makes a
// non-NULL person_id append-only). Following security's scratch-DB
// methodology ruling (k1-security-recheck.md): on a scratch database we
// remove ONLY what makes the state unreachable (FORCE RLS on staff_users
// for the move; the 0034 trigger for the re-link), put the state in place,
// RESTORE the removed control, and only then exercise the real executor.
// Each has its own mutant in ledger_adjustment_eligible_grant.
func TestK2G4_TenantUnchangedAndLivePersonRechecks(t *testing.T) {
	ctx := context.Background()

	// Tenant unchanged is doubly masked in production: RLS never shows a
	// session another tenant's staff row, so a moved approver is simply
	// invisible. To prove the executor's OWN tenant check (the K2-G4
	// mutant target) is an independent layer, this scratch database also
	// removes the masking RLS layer (a permissive all-rows SELECT on
	// staff_users - the K1-C3 layered-test technique), so the moved row IS
	// visible and only ledger_adjustment_eligible_grant's
	// "s.tenant_id = g.tenant_id" can refuse it.
	t.Run("tenant unchanged: an approver moved to another tenant is not counted", func(t *testing.T) {
		pool := scratchPoolThrough(t, "k2g4t_", 113)
		w := newWorldOn(t, pool, worldOpts{base: 2})
		other := newWorldOn(t, pool, worldOpts{})
		if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `CREATE POLICY k2g4_layer_removed ON staff_users FOR SELECT USING (true)`)
			return err
		}); err != nil {
			t.Fatalf("remove masking layer (scratch): %v", err)
		}
		out := w.firstApprovalThenBreak(t, func() {
			if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `ALTER TABLE staff_users NO FORCE ROW LEVEL SECURITY`); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE staff_users SET tenant_id = $2 WHERE id = $1`, w.F2.ID, other.Tenant); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `ALTER TABLE staff_users FORCE ROW LEVEL SECURITY`)
				return err
			}); err != nil {
				t.Fatalf("move approver (scratch): %v", err)
			}
			// The moved row is visible to X's tenant session (layer removed).
			if err := w.tenantTx(w.F3, func(ctx context.Context, tx pgx.Tx) error {
				var n int
				if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE id = $1`, w.F2.ID).Scan(&n); err != nil {
					return err
				}
				if n != 1 {
					t.Fatalf("layer removal ineffective: moved approver not visible")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
		requireNotCounted(t, w, out, "moved approver")
	})

	pool := scratchPoolThrough(t, "k2g4p_", 113)
	t.Run("live person_id = grant snapshot: a re-linked approver is not counted", func(t *testing.T) {
		w := newWorldOn(t, pool, worldOpts{base: 2})
		newPerson := uuid.New()
		out := w.firstApprovalThenBreak(t, func() {
			if err := pool.WithoutTenant(ctx, func(ctx context.Context, tx pgx.Tx) error {
				if _, err := tx.Exec(ctx, `INSERT INTO persons (id) VALUES ($1)`, newPerson); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `ALTER TABLE staff_users DISABLE TRIGGER staff_users_person_id_append_only`); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `ALTER TABLE staff_users NO FORCE ROW LEVEL SECURITY`); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE staff_users SET person_id = $2 WHERE id = $1`, w.F2.ID, newPerson); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `ALTER TABLE staff_users FORCE ROW LEVEL SECURITY`); err != nil {
					return err
				}
				_, err := tx.Exec(ctx, `ALTER TABLE staff_users ENABLE TRIGGER staff_users_person_id_append_only`)
				return err
			}); err != nil {
				t.Fatalf("re-link approver (scratch): %v", err)
			}
		})
		requireNotCounted(t, w, out, "re-linked approver")
	})
}
