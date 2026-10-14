//go:build integration

package httpserver

// ADR 0119 S5 + S6, the S4b-R3 / ADR 0082 A9 HARD GATE for K1 (capability grant APPROVE and REVOKE), through the real
// HTTP routes (the only callers allowed to put an authenticated ProofActor in the context). Inside the handler's
// transaction, after the L1 lock (the request row / the grant row FOR UPDATE), the acting staff member's own row is
// read FOR SHARE and compared with the verified token's role and staff_version (capability.reassertActor). Both orders:
//
//   - the staff change first: the request passes the liveness check at the door, then waits on the L1 row (held by a
//     blocker transaction); the suspension / password change commits meanwhile; when the blocker lets go the
//     re-assertion sees the change and the call answers 401 session_revoked with NOTHING written;
//   - the K1 call first: parked after the re-assertion (a scratch-database-only waiting point, a BEFORE trigger on
//     the approvals / grants table that waits on an advisory lock), a suspension cannot commit (55P03 under a 300ms
//     lock_timeout); the call then succeeds and the suspension completes afterwards.
//
// Visibility (ADR 0119 section 9.5(b)): the platform approver's own platform row is visible in the K1 platform session
// (db.WithPlatformAdmin leaves app.tenant_id empty), so no app.actor_staff_id GUC is needed on this path; the
// refusals below would not happen if the re-assertion could not see the row (it fails closed on no row).

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/auth"
)

const k1PauseLock = 4_119_006

type k1World struct {
	a         *cgAPI
	tenant    uuid.UUID
	requester uuid.UUID
	grantee   uuid.UUID
	reqTok    string
}

func newK1World(t *testing.T) *k1World {
	t.Helper()
	a := newCGAPI(t)
	w := &k1World{a: a, tenant: a.tenant()}
	w.requester = a.staff(w.tenant, "tenant_admin")
	w.reqTok = a.token(w.requester, w.tenant, auth.RoleTenantAdmin)
	// A spare, enrolled platform admin: SF020 refuses suspending the last one with a usable factor.
	spare := a.staff(uuid.Nil, "platform_admin")
	_ = a.token(spare, uuid.Nil, auth.RolePlatformAdmin)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE FUNCTION s56_k1_pause() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		     PERFORM pg_advisory_xact_lock(4119006);
		     RETURN NEW;
		 END $$`,
		`CREATE TRIGGER aa_s56_k1_pause BEFORE INSERT ON staff_capability_grant_approvals FOR EACH ROW EXECUTE FUNCTION s56_k1_pause()`,
		`CREATE TRIGGER aa_s56_k1_pause BEFORE UPDATE ON staff_capability_grants FOR EACH ROW EXECUTE FUNCTION s56_k1_pause()`,
	} {
		if _, err := a.pool.Raw().Exec(ctx, q); err != nil {
			t.Fatalf("scratch waiting point: %v", err)
		}
	}
	return w
}

// approver returns a fresh platform approver and its live token.
func (w *k1World) approver() (uuid.UUID, string) {
	id := w.a.staff(uuid.Nil, "platform_admin")
	return id, w.a.token(id, uuid.Nil, auth.RolePlatformAdmin)
}

func (w *k1World) base() string {
	return "/v1/admin/tenants/" + w.tenant.String() + "/capability-grants"
}

func (w *k1World) newRequest(t *testing.T) string {
	t.Helper()
	grantee := w.a.staff(w.tenant, "finance") // a fresh grantee: one pending request per (grantee, capability)
	resp := w.a.do(http.MethodPost, w.base()+"/requests", w.reqTok, map[string]any{
		"grantee_staff_id": grantee.String(), "capability": "ledger_adjustment:initiate", "reason_code": "test",
	})
	if resp.status != http.StatusCreated {
		t.Fatalf("create request: %d %s", resp.status, resp.body)
	}
	var v struct {
		ID string `json:"id"`
	}
	resp.decode(t, &v)
	return v.ID
}

func (w *k1World) approve(reqID, tok string) cgResp {
	return w.a.do(http.MethodPost, w.base()+"/requests/"+reqID+"/approve", tok, map[string]any{"reason_code": "test"})
}

func (w *k1World) revoke(grantID, tok string) cgResp {
	return w.a.do(http.MethodPost, w.base()+"/"+grantID+"/revoke", tok, map[string]any{"reason_code": "test"})
}

func (w *k1World) grant(t *testing.T) string {
	t.Helper()
	_, tok := w.approver()
	resp := w.approve(w.newRequest(t), tok)
	if resp.status != http.StatusOK {
		t.Fatalf("approve: %d %s", resp.status, resp.body)
	}
	var g struct {
		ID string `json:"id"`
	}
	resp.decode(t, &g)
	return g.ID
}

// changePlatform runs one statement on a platform staff row (platform session: app.tenant_id empty).
func (w *k1World) changePlatform(sql string, id uuid.UUID, lockTimeout string) error {
	return w.a.pool.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
		if lockTimeout != "" {
			if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, lockTimeout); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, sql, id)
		return err
	})
}

func (w *k1World) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	// The platform session sees the tenant's K1 rows (a bare tenant session does not: the count would be vacuous).
	if err := w.a.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, sql, args...).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	return n
}

func (w *k1World) waitForLockWaiter(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := w.a.pool.Raw().QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the K1 call never parked on a lock")
}

// blockRow holds one tenant row FOR UPDATE in an open transaction; the returned func rolls it back.
func (w *k1World) blockRow(t *testing.T, table string, id string) func() {
	t.Helper()
	locked := make(chan error, 1)
	done := make(chan struct{})
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		// A platform session (the K1 platform path's own scope) sees the tenant's request / grant rows.
		_ = w.a.pool.WithPlatformAdmin(context.Background(), uuid.New(), func(ctx context.Context, tx pgx.Tx) error {
			var one int
			err := tx.QueryRow(ctx, `SELECT 1 FROM `+pgx.Identifier{table}.Sanitize()+` WHERE id = $1 FOR UPDATE`, id).Scan(&one)
			locked <- err
			if err != nil {
				return err
			}
			<-done
			return errors.New("blocker: roll back")
		})
	}()
	var once sync.Once
	undo := func() {
		once.Do(func() {
			close(done)
			<-finished
		})
	}
	t.Cleanup(undo) // never leave a connection checked out (the pool's Close would wait for it)
	if err := <-locked; err != nil {
		undo()
		t.Fatalf("blocker on %s: %v", table, err)
	}
	return undo
}

// pause holds the advisory lock the scratch trigger waits on; the returned func releases it (idempotent).
func (w *k1World) pause(t *testing.T) func() {
	t.Helper()
	ctx := context.Background()
	conn, err := w.a.pool.Raw().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, k1PauseLock); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	t.Cleanup(func() {
		once.Do(func() {
			_, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, k1PauseLock)
			conn.Release()
		})
	})
	return func() {
		once.Do(func() {
			if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, k1PauseLock); err != nil {
				t.Error(err)
			}
			conn.Release()
		})
	}
}

func wantSessionRevoked(t *testing.T, what string, r cgResp) {
	t.Helper()
	if r.status != http.StatusUnauthorized || !strings.Contains(string(r.body), `"session_revoked"`) {
		t.Fatalf("%s: want 401 session_revoked, got %d %s", what, r.status, r.body)
	}
}

func TestK1Reassertion_Approve_ChangeFirst_Refused(t *testing.T) {
	w := newK1World(t)
	for _, c := range []struct{ name, sql string }{
		{"suspended", `UPDATE staff_users SET status = 'suspended' WHERE id = $1`},
		{"staff_version_only", `UPDATE staff_users SET password_hash = 'y' WHERE id = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			approver, tok := w.approver()
			reqID := w.newRequest(t)
			unblock := w.blockRow(t, "staff_capability_grant_requests", reqID)
			var resp cgResp
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); resp = w.approve(reqID, tok) }()
			w.waitForLockWaiter(t)
			if err := w.changePlatform(c.sql, approver, ""); err != nil {
				unblock()
				t.Fatalf("change: %v", err)
			}
			unblock()
			wg.Wait()
			wantSessionRevoked(t, "approve after the approver changed", resp)
			if n := w.count(t, `SELECT count(*) FROM staff_capability_grant_approvals WHERE request_id = $1`, reqID); n != 0 {
				t.Fatalf("a refused approval wrote %d approval row(s)", n)
			}
			if n := w.count(t, `SELECT count(*) FROM staff_capability_grants WHERE request_id = $1`, reqID); n != 0 {
				t.Fatalf("a refused approval wrote %d grant row(s)", n)
			}
		})
	}
}

func TestK1Reassertion_Approve_ApprovalFirst_SuspensionWaits(t *testing.T) {
	w := newK1World(t)
	approver, tok := w.approver()
	reqID := w.newRequest(t)
	release := w.pause(t)
	defer release()
	var resp cgResp
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); resp = w.approve(reqID, tok) }()
	w.waitForLockWaiter(t)
	waitErr := w.changePlatform(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, approver, "300ms")
	release()
	wg.Wait()
	if pgCode(waitErr) != "55P03" {
		t.Fatalf("the suspension must wait on the approval's FOR SHARE (55P03), got %v", waitErr)
	}
	if resp.status != http.StatusOK {
		t.Fatalf("the approval holding the row first must stand: %d %s", resp.status, resp.body)
	}
	if n := w.count(t, `SELECT count(*) FROM staff_capability_grants WHERE request_id = $1`, reqID); n != 1 {
		t.Fatalf("the approval wrote %d grant row(s), want 1 (the count is non-vacuous)", n)
	}
	if err := w.changePlatform(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, approver, "2s"); err != nil {
		t.Fatalf("the suspension after the approval commits: %v", err)
	}
}

func TestK1Reassertion_Revoke_ChangeFirst_Refused(t *testing.T) {
	w := newK1World(t)
	for _, c := range []struct{ name, sql string }{
		{"suspended", `UPDATE staff_users SET status = 'suspended' WHERE id = $1`},
		{"staff_version_only", `UPDATE staff_users SET password_hash = 'y' WHERE id = $1`},
	} {
		t.Run(c.name, func(t *testing.T) {
			grantID := w.grant(t)
			revoker, tok := w.approver()
			unblock := w.blockRow(t, "staff_capability_grants", grantID)
			var resp cgResp
			var wg sync.WaitGroup
			wg.Add(1)
			go func() { defer wg.Done(); resp = w.revoke(grantID, tok) }()
			w.waitForLockWaiter(t)
			if err := w.changePlatform(c.sql, revoker, ""); err != nil {
				unblock()
				t.Fatalf("change: %v", err)
			}
			unblock()
			wg.Wait()
			wantSessionRevoked(t, "revoke after the revoker changed", resp)
			if n := w.count(t, `SELECT count(*) FROM staff_capability_grants WHERE id = $1 AND revoked_at IS NOT NULL`, grantID); n != 0 {
				t.Fatal("a refused revocation revoked the grant")
			}
		})
	}
}

func TestK1Reassertion_Revoke_RevokeFirst_SuspensionWaits(t *testing.T) {
	w := newK1World(t)
	grantID := w.grant(t)
	revoker, tok := w.approver()
	release := w.pause(t)
	defer release()
	var resp cgResp
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); resp = w.revoke(grantID, tok) }()
	w.waitForLockWaiter(t)
	waitErr := w.changePlatform(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, revoker, "300ms")
	release()
	wg.Wait()
	if pgCode(waitErr) != "55P03" {
		t.Fatalf("the suspension must wait on the revocation's FOR SHARE (55P03), got %v", waitErr)
	}
	if resp.status != http.StatusNoContent {
		t.Fatalf("the revocation holding the row first must stand: %d %s", resp.status, resp.body)
	}
	if n := w.count(t, `SELECT count(*) FROM staff_capability_grants WHERE id = $1 AND revoked_at IS NOT NULL`, grantID); n != 1 {
		t.Fatal("the revocation did not revoke")
	}
	if err := w.changePlatform(`UPDATE staff_users SET status = 'suspended' WHERE id = $1`, revoker, "2s"); err != nil {
		t.Fatalf("the suspension after the revocation commits: %v", err)
	}
}
