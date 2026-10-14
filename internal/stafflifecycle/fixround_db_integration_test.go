//go:build integration

package stafflifecycle

// ADR 0119 S5 + S6 review fix round (adr0119-s5s6-security-review.md SR-2..SR-7, adr0119-s5s6-code-review.md F2, F7,
// F8, F11, F12): database-level tests through the real service and as igaming_runtime with forged GUCs.

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/actorproof/prooftest"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/testsupport/launchfix"
)

func (w *lworld) staffRow(tenant, id uuid.UUID) (status string, sv int64) {
	w.t.Helper()
	if err := w.scope(w.owner, tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT status, staff_version FROM staff_users WHERE id = $1`, id).Scan(&status, &sv)
	}); err != nil {
		w.t.Fatal(err)
	}
	return status, sv
}

func (w *lworld) person() uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	if err := w.exec(w.owner, uuid.Nil, `INSERT INTO persons (id) VALUES ($1)`, id); err != nil {
		w.t.Fatal(err)
	}
	return id
}

// F2: an offboarded staff member is re-invited with the SAME mailbox; two live rows with one email stay impossible.
func TestFixRound_F2_OffboardThenReinviteSameEmail(t *testing.T) {
	w := newLWorld(t)
	ta := w.staff(w.tenant, RoleTenantAdmin, true)
	actor := w.actor(ta, w.tenant, RoleTenantAdmin)
	mailbox := "f2-" + uuid.NewString() + "@example.com"
	first, err := w.svc.Invite(context.Background(), actor, w.tenant, InviteInput{Email: mailbox, Role: RoleSupport})
	if err != nil {
		t.Fatal(err)
	}
	// A second live invitation for the same mailbox is a conflict (the partial unique index).
	if _, err := w.svc.Invite(context.Background(), w.actor(ta, w.tenant, RoleTenantAdmin), w.tenant, InviteInput{Email: mailbox, Role: RoleSupport}); !errors.Is(err, ErrConflict) {
		t.Fatalf("a second live row for the mailbox must be ErrConflict, got %v", err)
	}
	if _, err := w.svc.RevokeInvite(context.Background(), w.actor(ta, w.tenant, RoleTenantAdmin), w.tenant, first.Staff.ID, Reason{Code: "wrong_details"}); err != nil {
		t.Fatal(err)
	}
	second, err := w.svc.Invite(context.Background(), w.actor(ta, w.tenant, RoleTenantAdmin), w.tenant, InviteInput{Email: mailbox, Role: RoleSupport})
	if err != nil {
		t.Fatalf("re-invite after offboarding must succeed: %v", err)
	}
	if second.Staff.ID == first.Staff.ID {
		t.Fatal("the re-invitation must be a new row")
	}
	// Direct SQL: a second live row is still refused (23505); the offboarded row keeps its email (SF011).
	if got := code(w.exec(w.owner, w.tenant, `INSERT INTO staff_users (tenant_id, email, password_hash, role, status) VALUES ($1, $2, NULL, 'support', 'invited')`, w.tenant, mailbox)); got != "23505" {
		t.Fatalf("two live rows for one mailbox: %q", got)
	}
	if got := code(w.exec(w.owner, w.tenant, `UPDATE staff_users SET email = 'x-`+uuid.NewString()+`@f.invalid' WHERE id = $1`, first.Staff.ID)); got != "SF011" {
		t.Fatalf("the offboarded row's email stays immutable: %q", got)
	}
	// The email lookup (login, seed-admin) finds the live row.
	var found uuid.UUID
	if err := w.scope(w.owner, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM staff_users WHERE email = $1 ORDER BY (status = 'offboarded'), created_at DESC, id LIMIT 1`, mailbox).Scan(&found)
	}); err != nil || found != second.Staff.ID {
		t.Fatalf("the live row wins the email lookup: %v %v", found, err)
	}
	// Platform: an offboarded platform mailbox can be requested again (the pre-check ignores offboarded rows).
	pa := w.staff(uuid.Nil, RolePlatformAdmin, true)
	gone := uuid.New()
	pmail := "f2p-" + uuid.NewString() + "@example.com"
	if err := w.exec(w.owner, uuid.Nil, `WITH p AS (INSERT INTO persons (id) VALUES ($1))
		INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id) VALUES ($1, NULL, $2, NULL, 'platform_admin', 'invited', $1)`, gone, pmail); err != nil {
		t.Fatal(err)
	}
	if err := w.exec(w.owner, uuid.Nil, `UPDATE staff_users SET status = 'offboarded' WHERE id = $1`, gone); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.CreateRequest(context.Background(), w.actor(pa, uuid.Nil, RolePlatformAdmin),
		RequestInput{Kind: KindPlatformInvite, Email: pmail, PersonID: ptr(w.person())}); err != nil {
		t.Fatalf("a platform re-invite after offboarding must be accepted: %v", err)
	}
}

func ptr(u uuid.UUID) *uuid.UUID { return &u }

// SR-2 (same scope): suspending a tenant admin offboards the rows it invited and revokes the tokens it issued; the
// acceptance then has nothing to accept. The issuer check also refuses on its own (the backstop).
func TestFixRound_SR2_SuspensionRevokesWhatTheIssuerIssued(t *testing.T) {
	w := newLWorld(t)
	ta1 := w.staff(w.tenant, RoleTenantAdmin, true)
	ta2 := w.staff(w.tenant, RoleTenantAdmin, true)
	inv, err := w.svc.Invite(context.Background(), w.actor(ta1, w.tenant, RoleTenantAdmin), w.tenant,
		InviteInput{Email: "sr2-" + uuid.NewString() + "@example.com", Role: RoleTenantAdmin, PersonID: ptr(w.person())})
	if err != nil {
		t.Fatal(err)
	}
	// A live mfa_reset token issued by ta1 to a support member.
	sup := w.staff(w.tenant, RoleSupport, true)
	if _, err := w.svc.ResetMFA(context.Background(), w.actor(ta1, w.tenant, RoleTenantAdmin), w.tenant, sup, Reason{Code: "device_lost"}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Suspend(context.Background(), w.actor(ta2, w.tenant, RoleTenantAdmin), w.tenant, ta1, Reason{Code: "security_compromise"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := w.staffRow(w.tenant, inv.Staff.ID); st != "offboarded" {
		t.Fatalf("the invitee of the suspended admin must be offboarded, is %s", st)
	}
	var live, events int
	if err := w.scope(w.owner, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_enrolment_tokens WHERE issued_by = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, ta1).Scan(&live); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_lifecycle_events WHERE staff_user_id = $1 AND event = 'invite_revoked' AND reason_code = 'issuer_removed'`, inv.Staff.ID).Scan(&events)
	}); err != nil {
		t.Fatal(err)
	}
	if live != 0 || events != 1 {
		t.Fatalf("issued tokens left live: %d; invite_revoked events: %d", live, events)
	}
}

// SR-2 backstop, both orders, through EnrolmentIssuersLiveInTx (what the accept / redeem handlers call before they lock
// the token): (a) the issuer suspended first -> not live; (b) the acceptance first, holding the issuer FOR SHARE -> the
// suspension's T2 waits for it (busy under a short lock_timeout) and completes afterwards.
func TestFixRound_SR2_IssuerCheck_BothOrders(t *testing.T) {
	w := newLWorld(t)
	w.svc.lockTimeout = 300 * time.Millisecond
	pa := w.staff(uuid.Nil, RolePlatformAdmin, true)
	pa2 := w.staff(uuid.Nil, RolePlatformAdmin, true)
	// A platform admin invites a TENANT staff member (cross scope: the platform-scoped suspension cannot see it).
	inv, err := w.svc.Invite(context.Background(), w.actor(pa, uuid.Nil, RolePlatformAdmin), w.tenant,
		InviteInput{Email: "sr2b-" + uuid.NewString() + "@example.com", Role: RoleSupport})
	if err != nil {
		t.Fatal(err)
	}
	var tokenID uuid.UUID
	if err := w.scope(w.owner, w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `SELECT id FROM staff_enrolment_tokens WHERE staff_user_id = $1 AND purpose = 'invite'`, inv.Staff.ID).Scan(&tokenID)
	}); err != nil {
		t.Fatal(err)
	}
	check := func() (bool, error) {
		var live bool
		err := w.rt.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
			var err error
			live, err = EnrolmentIssuersLiveInTx(ctx, tx, tokenID)
			return err
		})
		return live, err
	}
	if live, err := check(); err != nil || !live {
		t.Fatalf("a live issuer: %v %v", live, err)
	}

	// (b) acceptance first: hold the issuer FOR SHARE (as the accept transaction does) while pa2 suspends pa.
	release := make(chan struct{})
	held := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.rt.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
			live, err := EnrolmentIssuersLiveInTx(ctx, tx, tokenID)
			if err == nil && !live {
				err = errors.New("issuer not live")
			}
			held <- err
			<-release
			return nil
		})
	}()
	if err := <-held; err != nil {
		t.Fatal(err)
	}
	_, err = w.svc.Suspend(context.Background(), w.actor(pa2, uuid.Nil, RolePlatformAdmin), uuid.Nil, pa, Reason{Code: "security_compromise"})
	close(release)
	<-done
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("the suspension must wait for the acceptance holding the issuer FOR SHARE (busy), got %v", err)
	}
	if st, _ := w.staffRow(uuid.Nil, pa); st != "active" {
		t.Fatalf("the busy suspension must not have committed: %s", st)
	}
	if _, err := w.svc.Suspend(context.Background(), w.actor(pa2, uuid.Nil, RolePlatformAdmin), uuid.Nil, pa, Reason{Code: "security_compromise"}); err != nil {
		t.Fatalf("the suspension after the acceptance: %v", err)
	}
	// (a) the issuer suspended: the cross-scope invitation is still `invited` (not visible to the platform session),
	// so the backstop is what refuses it.
	if st, _ := w.staffRow(w.tenant, inv.Staff.ID); st != "invited" {
		t.Fatalf("a cross-scope invitation is not reachable by the platform-scoped suspension: %s", st)
	}
	if live, err := check(); err != nil || live {
		t.Fatalf("an invitation whose issuer is suspended must be refused: live=%v err=%v", live, err)
	}
}

// SR-3: the platform_invite Person may not already be bound to a live platform account (Go and SF031 by direct SQL).
func TestFixRound_SR3_InvitePersonInUseRefused(t *testing.T) {
	w := newLWorld(t)
	pa := w.staff(uuid.Nil, RolePlatformAdmin, true)
	other := w.staff(uuid.Nil, RolePlatformAdmin, true) // its Person is its own id (w.staff)
	_, err := w.svc.CreateRequest(context.Background(), w.actor(pa, uuid.Nil, RolePlatformAdmin),
		RequestInput{Kind: KindPlatformInvite, Email: "sr3-" + uuid.NewString() + "@example.com", PersonID: ptr(other)})
	if !errors.Is(err, ErrPersonInUse) {
		t.Fatalf("Go: an invite naming another live platform admin's Person must be ErrPersonInUse, got %v", err)
	}
	err = w.rt.WithPlatformAdmin(context.Background(), pa, func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, actorproof.OpStaffLifecycleRequest, actorproof.TargetNew, strings.Repeat("0", 64)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_lifecycle_requests (kind, invite_email, invite_person_id, payload_hash) VALUES ('platform_invite', $1, $2, NULL)`,
			"sr3sql-"+uuid.NewString()+"@example.com", other)
		return err
	})
	if code(err) != "SF031" {
		t.Fatalf("SF031: an invite naming a Person in use must be refused by the database, got %v", err)
	}
	// F12: an invitation naming a target at filing is refused by the database.
	err = w.rt.WithPlatformAdmin(context.Background(), pa, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO staff_lifecycle_requests (kind, invite_email, invite_person_id, target_staff_id) VALUES ('platform_invite', $1, $2, $3)`,
			"f12-"+uuid.NewString()+"@example.com", w.person(), other)
		return err
	})
	if code(err) != "SF031" {
		t.Fatalf("F12: an invitation with a target at filing must be SF031, got %v", err)
	}
	// A fresh Person is accepted, and the read shows the Person and its (empty) live links.
	fresh := w.person()
	req, err := w.svc.CreateRequest(context.Background(), w.actor(pa, uuid.Nil, RolePlatformAdmin),
		RequestInput{Kind: KindPlatformInvite, Email: "sr3ok-" + uuid.NewString() + "@example.com", PersonID: &fresh})
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.svc.GetRequest(context.Background(), w.actor(other, uuid.Nil, RolePlatformAdmin), req.ID)
	if err != nil || got.InvitePersonID == nil || *got.InvitePersonID != fresh || len(got.InvitePersonStaff) != 0 {
		t.Fatalf("the approver's view must carry the invite Person: %+v %v", got, err)
	}
}

// SR-5: a request whose requester was suspended cannot be approved (Go, and SF032 by direct SQL); a reject still can.
func TestFixRound_SR5_RequesterSuspendedNotApprovable(t *testing.T) {
	w := newLWorld(t)
	requester := w.staff(uuid.Nil, RolePlatformAdmin, true)
	approver := w.staff(uuid.Nil, RolePlatformAdmin, true)
	third := w.staff(uuid.Nil, RolePlatformAdmin, true)
	req, err := w.svc.CreateRequest(context.Background(), w.actor(requester, uuid.Nil, RolePlatformAdmin),
		RequestInput{Kind: KindPlatformInvite, Email: "sr5-" + uuid.NewString() + "@example.com", PersonID: ptr(w.person())})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.Suspend(context.Background(), w.actor(third, uuid.Nil, RolePlatformAdmin), uuid.Nil, requester, Reason{Code: "security_compromise"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := w.svc.Approve(context.Background(), w.actor(approver, uuid.Nil, RolePlatformAdmin), req.ID, req.PayloadHash); !errors.Is(err, ErrTransition) {
		t.Fatalf("Go: approving a suspended requester's request must be refused, got %v", err)
	}
	err = w.rt.WithPlatformAdmin(context.Background(), approver, func(ctx context.Context, tx pgx.Tx) error {
		if err := prooftest.AttachForSession(ctx, tx, actorproof.OpStaffLifecycleApprove, req.ID.String(), req.PayloadHash); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO staff_lifecycle_approvals (request_id, decision, payload_hash) VALUES ($1, 'approve', $2)`, req.ID, req.PayloadHash)
		return err
	})
	if code(err) != "SF032" {
		t.Fatalf("SF032: the database must refuse it too, got %v", err)
	}
	if _, err := w.svc.Reject(context.Background(), w.actor(approver, uuid.Nil, RolePlatformAdmin), req.ID); err != nil {
		t.Fatalf("a reject stays possible: %v", err)
	}
}

// SR-8: reject and cancel re-assert the decider in the transaction (a stale sv is refused, nothing written).
func TestFixRound_SR8_RejectCancelReassertActor(t *testing.T) {
	w := newLWorld(t)
	requester := w.staff(uuid.Nil, RolePlatformAdmin, true)
	decider := w.staff(uuid.Nil, RolePlatformAdmin, true)
	req, err := w.svc.CreateRequest(context.Background(), w.actor(requester, uuid.Nil, RolePlatformAdmin),
		RequestInput{Kind: KindPlatformInvite, Email: "sr8-" + uuid.NewString() + "@example.com", PersonID: ptr(w.person())})
	if err != nil {
		t.Fatal(err)
	}
	staleDecider := w.actor(decider, uuid.Nil, RolePlatformAdmin)
	staleRequester := w.actor(requester, uuid.Nil, RolePlatformAdmin)
	for _, id := range []uuid.UUID{decider, requester} {
		if err := w.exec(w.owner, uuid.Nil, `UPDATE staff_users SET password_hash = 'rotated' WHERE id = $1`, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.svc.Reject(context.Background(), staleDecider, req.ID); !errors.Is(err, db.ErrActorStaffNotLive) {
		t.Fatalf("reject with a stale sv: %v", err)
	}
	if _, err := w.svc.Cancel(context.Background(), staleRequester, req.ID); !errors.Is(err, db.ErrActorStaffNotLive) {
		t.Fatalf("cancel with a stale sv: %v", err)
	}
	got, err := w.svc.GetRequest(context.Background(), w.actor(decider, uuid.Nil, RolePlatformAdmin), req.ID)
	if err != nil || got.State != "pending" {
		t.Fatalf("nothing may be written: %+v %v", got, err)
	}
}

// SR-6: the runtime role cannot DELETE a staff row (42501).
func TestFixRound_SR6_RuntimeCannotDeleteStaff(t *testing.T) {
	w := newLWorld(t)
	id := w.staff(w.tenant, RoleSupport, false)
	if got := code(w.exec(w.rt, w.tenant, `DELETE FROM staff_users WHERE id = $1`, id)); got != "42501" {
		t.Fatalf("runtime DELETE of a staff row: %q", got)
	}
}

// SR-7: SF021's platform bypass needs an EXISTING active platform_admin row; a forged nonexistent GUC does not stand it
// aside. The self pair admits platform rows only: a forged app.actor_staff_id naming another tenant's staff member
// reveals nothing.
func TestFixRound_SR7_ForgedGUCs(t *testing.T) {
	w := newLWorld(t)
	ta := w.staff(w.tenant, RoleTenantAdmin, true)
	ta2 := w.staff(w.tenant, RoleTenantAdmin, false) // no usable factor: does not count
	// Control: a REAL active platform admin named in app.actor_staff_id (the inTargetScope path) does stand SF021
	// aside. The bypass reads staff_users under RLS, so a GUC naming a row the session cannot see fails closed.
	pa := w.staff(uuid.Nil, RolePlatformAdmin, true)
	for _, forged := range []struct {
		guc, value string
		bypass     bool
	}{
		{"app.acting_platform_principal_id", uuid.NewString(), false},
		{"app.platform_admin_principal_id", uuid.NewString(), false},
		{"app.actor_staff_id", uuid.NewString(), false},
		{"app.actor_staff_id", ta2.String(), false}, // a tenant row is not a platform admin
		{"app.actor_staff_id", pa.String(), true},   // visible through the staff_actor_self_* pair: the production path
	} {
		var affected int64
		err := w.rt.WithPrincipalScope(context.Background(), w.tenant, ta2, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, forged.guc, forged.value); err != nil {
				return err
			}
			ct, err := tx.Exec(ctx, `UPDATE staff_users SET status = 'suspended' WHERE id = $1`, ta)
			affected = ct.RowsAffected()
			if err == nil && forged.bypass {
				return errRollback // the control must not change the state the other cases need
			}
			return err
		})
		if forged.bypass {
			if !errors.Is(err, errRollback) || affected != 1 {
				t.Fatalf("control: a real platform admin in %s must stand SF021 aside: %v (%d rows)", forged.guc, err, affected)
			}
			continue
		}
		// Refused by SF021, or (an acting GUC without its open acting session) the row is not even visible to the
		// UPDATE: both fail closed. What may never happen is a committed suspension.
		if got := code(err); got != "SF021" && (err != nil || affected != 0) {
			t.Fatalf("a forged %s=%s must not bypass SF021, got %v (%d rows)", forged.guc, forged.value, err, affected)
		}
		t.Logf("%s forged: %v (%d rows)", forged.guc, err, affected)
		if st, _ := w.staffRow(w.tenant, ta); st != "active" {
			t.Fatalf("%s: the last tenant admin was suspended", forged.guc)
		}
	}
	// Cross-tenant read through a forged actor GUC.
	var other uuid.UUID
	if err := w.owner.WithPlatformAdmin(context.Background(), launchfix.Operators().Requester, func(ctx context.Context, tx pgx.Tx) error {
		tn, err := launchfix.CreateTenant(ctx, tx, "sl2", "sl2-"+uuid.NewString()[:12], "under_platform_licence")
		other = tn.ID
		return err
	}); err != nil {
		t.Fatal(err)
	}
	foreign := w.staff(other, RoleSupport, false)
	var n int
	if err := w.rt.WithTenant(context.Background(), w.tenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('app.actor_staff_id', $1, true)`, foreign.String()); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT count(*) FROM staff_users WHERE id = $1`, foreign).Scan(&n)
	}); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatal("a forged app.actor_staff_id naming another tenant's staff member revealed the row")
	}
}

// F8: a lock timeout INSIDE the status UPDATE (here the SF020 advisory lock, held by another session for longer than
// one lock_timeout but less than two) is retried once like any other T2 lock timeout.
func TestFixRound_F8_T2RetryCoversTheStatusUpdate(t *testing.T) {
	w := newLWorldScratch(t, "sl_f8_")
	w.svc.lockTimeout = 400 * time.Millisecond
	actor := w.staff(uuid.Nil, RolePlatformAdmin, true)
	target := w.staff(uuid.Nil, RolePlatformAdmin, true)
	_ = w.staff(uuid.Nil, RolePlatformAdmin, true) // a third usable admin: SF020 passes once it gets the lock
	conn, err := w.owner.Raw().Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(context.Background(), `SELECT pg_advisory_lock(hashtext('sf:last_admin:platform'))`); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	unlock := func() {
		once.Do(func() {
			_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('sf:last_admin:platform'))`)
		})
	}
	defer unlock()
	go func() {
		time.Sleep(600 * time.Millisecond)
		unlock()
	}()
	if _, err := w.svc.Suspend(context.Background(), w.actor(actor, uuid.Nil, RolePlatformAdmin), uuid.Nil, target, Reason{Code: "investigation"}); err != nil {
		t.Fatalf("a T2 timeout in the status UPDATE must be retried once: %v", err)
	}
}

// F11: the Go request digest equals the database's staff_lifecycle_request_digest, including a non-ASCII note.
func TestFixRound_F11_RequestDigestEqualsTheDatabase(t *testing.T) {
	w := newLWorld(t)
	target, person := uuid.New(), uuid.New()
	for _, c := range []struct {
		kind, email, reason, note string
		target, person            *uuid.UUID
	}{
		{kind: KindPlatformInvite, email: "d-1@example.com", person: &person, note: "Zoë Ångström - new hire, día 1"},
		{kind: KindPlatformReactivate, target: &target, reason: "other", note: "returned; ticket 42 / naïve note"},
		{kind: KindPlatformMFAReset, target: &target, reason: "device_lost"},
	} {
		var dbDigest string
		if err := w.owner.WithoutTenant(context.Background(), func(ctx context.Context, tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT staff_lifecycle_request_digest($1, $2, NULLIF($3, ''), $4, NULLIF($5, ''), NULLIF($6, ''))`,
				c.kind, c.target, c.email, c.person, c.reason, c.note).Scan(&dbDigest)
		}); err != nil {
			t.Fatal(err)
		}
		if got := RequestDigest(c.kind, c.target, c.email, c.person, c.reason, c.note); got != dbDigest {
			t.Fatalf("%s: Go digest %s != database %s", c.kind, got, dbDigest)
		}
	}
}

// errRollback aborts a test transaction on purpose.
var errRollback = errors.New("test: roll back")
