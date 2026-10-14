package stafflifecycle

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

// dbNow is the transaction's clock (the store's token TTLs use it).
func dbNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var t time.Time
	err := tx.QueryRow(ctx, `SELECT now()`).Scan(&t)
	return t, err
}

// authorizeTarget applies the role authority to a TENANT target the actor wants to act on (ADR 0119 s10.1): the
// actor may act on the target's current role. Platform targets are reached only by platform callers
// (inTargetScope) and need no role check (platform_admin is the only platform role).
func authorizeTarget(actor Actor, target staffRow) error {
	if target.TenantID == uuid.Nil {
		if !actor.Platform() {
			return ErrForbidden
		}
		return nil
	}
	if !Grantable(actor, target.Role) {
		return ErrRoleNotGrantable
	}
	return nil
}

func refuseSelf(actor Actor, target uuid.UUID) error {
	if actor.StaffID == target {
		return ErrSelfTarget
	}
	return nil
}

// ---- invitation --------------------------------------------------------------------------------------------------

// InviteInput is a tenant invitation (ADR 0119 s10.1 "create invitation").
type InviteInput struct {
	Email    string
	Role     string
	PersonID *uuid.UUID
}

// Invite creates a tenant staff member `invited` (no password) and emails an invite token.
func (s *Service) Invite(ctx context.Context, actor Actor, targetTenant uuid.UUID, in InviteInput) (Result, error) {
	if err := s.needMailer(); err != nil {
		return Result{}, err
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if targetTenant == uuid.Nil || !validEmail(email) || !IsTenantRole(in.Role) {
		return Result{}, ErrInvalidInput
	}
	if !Grantable(actor, in.Role) {
		return Result{}, ErrRoleNotGrantable
	}
	if PersonRequired(in.Role) && in.PersonID == nil {
		return Result{}, ErrPersonRequired
	}
	var res Result
	var mails []mail
	err := s.inTargetScope(ctx, actor, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
		mails = nil
		if err := reassertActorOnly(ctx, tx, actor); err != nil {
			return err
		}
		id := uuid.New()
		if _, err := tx.Exec(ctx, `INSERT INTO staff_users (id, tenant_id, email, password_hash, role, status, person_id, invited_by)
			VALUES ($1, $2, $3, NULL, $4, 'invited', $5, $6)`, id, targetTenant, email, in.Role, in.PersonID, actor.StaffID); err != nil {
			return mapDBError(err)
		}
		target, err := readTarget(ctx, tx, id, targetTenant)
		if err != nil {
			return err
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		tok, err := s.tokens.IssueEnrolmentToken(ctx, tx, target.ref(), store.EnrolInvite, &actor.StaffID, nil, now)
		if err != nil {
			return err
		}
		mails = append(mails, mail{to: email, purpose: store.EnrolInvite, token: tok})
		if err := record(ctx, tx, eventIn{target: target, event: "invited", actorType: "staff", actor: actor.StaffID,
			after: target.state(), meta: actor.Meta}); err != nil {
			return err
		}
		res.Staff, err = loadStaff(ctx, tx, target.ID)
		return err
	})
	if err != nil {
		return Result{}, mapDBError(err)
	}
	res.Delivery = s.deliver(ctx, mails)
	return res, nil
}

func validEmail(e string) bool {
	if len(e) < 3 || len(e) > 320 || strings.ContainsAny(e, " \t\r\n") {
		return false
	}
	at := strings.IndexByte(e, '@')
	return at > 0 && at < len(e)-1 && strings.Count(e, "@") == 1
}

// reassertActorOnly is the re-assertion for a change that write-locks no existing staff row (an invitation): the
// actor's own row FOR SHARE against the verified token.
func reassertActorOnly(ctx context.Context, tx pgx.Tx, actor Actor) error {
	locked, err := db.ScanActorStaffRows(tx.Query(ctx,
		`SELECT id, role, status, staff_version FROM staff_users WHERE id = $1 FOR SHARE`, actor.StaffID))
	if err != nil {
		return err
	}
	return db.CheckActorStaffLive(locked, actor.StaffID, db.ActorExpectation{Role: actor.Role, StaffVersion: actor.StaffVersion})
}

// ResendInvite re-issues the invite token of an `invited` staff member (the earlier one is revoked) and emails it.
func (s *Service) ResendInvite(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID) (Result, error) {
	if err := s.needMailer(); err != nil {
		return Result{}, err
	}
	if err := refuseSelf(actor, staffID); err != nil {
		return Result{}, err
	}
	var res Result
	var mails []mail
	err := s.inTargetScope(ctx, actor, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
		mails = nil
		target, err := lockActorAndTarget(ctx, tx, actor, staffID, targetTenant)
		if err != nil {
			return err
		}
		if err := authorizeTarget(actor, target); err != nil {
			return err
		}
		if target.Status != "invited" {
			return ErrTransition
		}
		now, err := dbNow(ctx, tx)
		if err != nil {
			return err
		}
		tok, err := s.tokens.IssueEnrolmentToken(ctx, tx, target.ref(), store.EnrolInvite, &actor.StaffID, nil, now)
		if err != nil {
			return err
		}
		mails = append(mails, mail{to: target.Email, purpose: store.EnrolInvite, token: tok})
		if err := record(ctx, tx, eventIn{target: target, event: "invited", actorType: "staff", actor: actor.StaffID,
			before: target.state(), after: target.state(), meta: actor.Meta, auditAction: "staff.invitation_resent",
			extraMetadata: map[string]any{"resend": true}}); err != nil {
			return err
		}
		res.Staff, err = loadStaff(ctx, tx, target.ID)
		return err
	})
	if err != nil {
		return Result{}, mapDBError(err)
	}
	res.Delivery = s.deliver(ctx, mails)
	return res, nil
}

// RevokeInvite moves an `invited` staff member to `offboarded` and revokes the token.
func (s *Service) RevokeInvite(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID, reason Reason) (Result, error) {
	if err := ValidateReason(ActionInviteRevoke, reason); err != nil {
		return Result{}, err
	}
	if err := refuseSelf(actor, staffID); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.inTargetScope(ctx, actor, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
		target, err := lockActorAndTarget(ctx, tx, actor, staffID, targetTenant)
		if err != nil {
			return err
		}
		if err := authorizeTarget(actor, target); err != nil {
			return err
		}
		if target.Status != "invited" {
			return ErrTransition
		}
		after, err := updateStatus(ctx, tx, target, "offboarded", false)
		if err != nil {
			return err
		}
		if err := revokeAccess(ctx, tx, target.ID); err != nil {
			return err
		}
		if err := record(ctx, tx, eventIn{target: target, event: "invite_revoked", actorType: "staff", actor: actor.StaffID,
			before: target.state(), after: after, reason: reason, meta: actor.Meta}); err != nil {
			return err
		}
		res.Staff, err = loadStaff(ctx, tx, target.ID)
		return err
	})
	return res, mapDBError(err)
}

// updateStatus changes the status (and, for offboarding, removes the password) and returns the after-state; the
// SF012 trigger bumps staff_version.
func updateStatus(ctx context.Context, tx pgx.Tx, target staffRow, status string, clearPassword bool) (State, error) {
	q := `UPDATE staff_users SET status = $2, updated_at = now() WHERE id = $1 RETURNING ` + staffRowColumns
	if clearPassword {
		q = `UPDATE staff_users SET status = $2, password_hash = NULL, updated_at = now() WHERE id = $1 RETURNING ` + staffRowColumns
	}
	r, err := scanStaffRow(tx.QueryRow(ctx, q, target.ID, status))
	if err != nil {
		return State{}, mapDBError(err)
	}
	return r.state(), nil
}

// ---- suspend / offboard (two phases) -----------------------------------------------------------------------------

// Suspend suspends a staff member (tenant target, or platform target with targetTenant = uuid.Nil).
func (s *Service) Suspend(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID, reason Reason) (Result, error) {
	if err := ValidateReason(ActionSuspend, reason); err != nil {
		return Result{}, err
	}
	return s.twoPhase(ctx, actor, targetTenant, staffID, reason, "suspended")
}

// Offboard offboards a staff member (terminal).
func (s *Service) Offboard(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID, reason Reason) (Result, error) {
	if err := ValidateReason(ActionOffboard, reason); err != nil {
		return Result{}, err
	}
	return s.twoPhase(ctx, actor, targetTenant, staffID, reason, "offboarded")
}

// twoPhase is the ADR 0119 s9.5 suspension phasing. T1 (no staff_users lock): the authority and state checks, a
// last-admin pre-check (so the last admin is not logged out by a change the database will refuse), then every
// session, live challenge and enrolment token of the target is revoked and audited; commit. T2: lock_timeout 5 s,
// the actor / target locks and re-assertion, the status change (SF010 / SF012 / SF020 / SF021 decide), factor and
// recovery-code revocation for offboarding or a compromise, a second session sweep, the event and the audit row.
// A lock timeout in T2 is retried once, then ErrBusy (the sessions stay revoked: fail-safe).
func (s *Service) twoPhase(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID, reason Reason, to string) (Result, error) {
	if err := refuseSelf(actor, staffID); err != nil {
		return Result{}, err
	}
	// T1
	err := s.inTargetScope(ctx, actor, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
		target, err := readTarget(ctx, tx, staffID, targetTenant)
		if err != nil {
			return err
		}
		if err := authorizeTarget(actor, target); err != nil {
			return err
		}
		if target.Status != "active" && (to != "offboarded" || target.Status != "suspended") {
			return ErrTransition
		}
		if target.Status == "active" {
			last, err := lastAdminWouldBeRemoved(ctx, tx, actor, target)
			if err != nil {
				return err
			}
			if last {
				return ErrLastAdmin
			}
		}
		if err := revokeAccess(ctx, tx, target.ID); err != nil {
			return err
		}
		// Phase 1 is audited only: the lifecycle event is written by T2 with the real before / after.
		return record(ctx, tx, eventIn{target: target, event: to, actorType: "staff", actor: actor.StaffID,
			before: target.state(), after: target.state(), reason: reason, meta: actor.Meta, auditOnly: true,
			auditAction: "staff.access_revoked", extraMetadata: map[string]any{"phase": 1, "action": to}})
	})
	if err != nil {
		return Result{}, mapDBError(err)
	}
	// T2, retried once on a lock timeout.
	var res Result
	for attempt := 0; attempt < 2; attempt++ {
		res, err = s.phaseTwo(ctx, actor, targetTenant, staffID, reason, to)
		if !errors.Is(err, errLockTimeout) {
			return res, err
		}
	}
	return Result{}, ErrBusy
}

var errLockTimeout = errors.New("stafflifecycle: lock timeout")

func (s *Service) phaseTwo(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID, reason Reason, to string) (Result, error) {
	var res Result
	err := s.inTargetScope(ctx, actor, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `SELECT set_config('lock_timeout', $1, true)`, pgInterval(s.lockTimeout)); err != nil {
			return err
		}
		target, err := lockActorAndTarget(ctx, tx, actor, staffID, targetTenant)
		if err != nil {
			return err
		}
		if err := authorizeTarget(actor, target); err != nil {
			return err
		}
		if target.Status != "active" && (to != "offboarded" || target.Status != "suspended") {
			return ErrTransition
		}
		after, err := updateStatus(ctx, tx, target, to, to == "offboarded")
		if err != nil {
			return err
		}
		var revoked []uuid.UUID
		if to == "offboarded" {
			if revoked, err = revokeFactors(ctx, tx, target.ID, "offboarded"); err != nil {
				return err
			}
		} else if reason.Code == "security_compromise" {
			if revoked, err = revokeFactors(ctx, tx, target.ID, "suspended_compromise"); err != nil {
				return err
			}
		}
		if err := revokeAccess(ctx, tx, target.ID); err != nil {
			return err
		}
		if err := record(ctx, tx, eventIn{target: target, event: to, actorType: "staff", actor: actor.StaffID,
			before: target.state(), after: after, reason: reason, meta: actor.Meta, revokedFactors: revoked}); err != nil {
			return err
		}
		res.Staff, err = loadStaff(ctx, tx, target.ID)
		return err
	})
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
		return Result{}, errLockTimeout
	}
	return res, mapDBError(err)
}

func pgInterval(d time.Duration) string {
	return d.String()
}

// lastAdminWouldBeRemoved is the T1 pre-check of SF020 / SF021 (the trigger in T2 decides; this keeps the last
// admin's sessions alive when the database would refuse the change anyway).
func lastAdminWouldBeRemoved(ctx context.Context, tx pgx.Tx, actor Actor, target staffRow) (bool, error) {
	var others int
	switch {
	case target.TenantID == uuid.Nil && target.Role == RolePlatformAdmin:
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_users su
			WHERE su.tenant_id IS NULL AND su.role = 'platform_admin' AND su.status = 'active' AND su.id <> $1
			  AND EXISTS (SELECT 1 FROM staff_mfa_factors f WHERE f.staff_user_id = su.id AND f.status = 'active' AND f.hard_locked_at IS NULL)`,
			target.ID).Scan(&others); err != nil {
			return false, err
		}
		return others == 0, nil
	case target.TenantID != uuid.Nil && target.Role == RoleTenantAdmin && !actor.Platform():
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM staff_users su
			WHERE su.tenant_id = $2 AND su.role = 'tenant_admin' AND su.status = 'active' AND su.id <> $1
			  AND EXISTS (SELECT 1 FROM staff_mfa_factors f WHERE f.staff_user_id = su.id AND f.status = 'active' AND f.hard_locked_at IS NULL)`,
			target.ID, target.TenantID).Scan(&others); err != nil {
			return false, err
		}
		return others == 0, nil
	}
	return false, nil
}

// ---- reactivate / role / MFA reset (tenant targets; platform targets go through the four-eyes) --------------------

// Reactivate moves a suspended tenant staff member back to active. A target without a usable second factor (a
// compromise suspension revoked it) gets an mfa_reset token by email.
func (s *Service) Reactivate(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID, reason Reason) (Result, error) {
	if err := ValidateReason(ActionReactivate, reason); err != nil {
		return Result{}, err
	}
	if targetTenant == uuid.Nil {
		return Result{}, ErrForbidden // platform staff: a platform_reactivate four-eyes request
	}
	if err := s.needMailer(); err != nil {
		return Result{}, err
	}
	if err := refuseSelf(actor, staffID); err != nil {
		return Result{}, err
	}
	var res Result
	var mails []mail
	err := s.inTargetScope(ctx, actor, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
		mails = nil
		target, err := lockActorAndTarget(ctx, tx, actor, staffID, targetTenant)
		if err != nil {
			return err
		}
		if err := authorizeTarget(actor, target); err != nil {
			return err
		}
		m, err := s.reactivateInTx(ctx, tx, target, "staff", actor.StaffID, reason, nil, actor.Meta)
		if err != nil {
			return err
		}
		mails = m
		res.Staff, err = loadStaff(ctx, tx, target.ID)
		return err
	})
	if err != nil {
		return Result{}, mapDBError(err)
	}
	res.Delivery = s.deliver(ctx, mails)
	return res, nil
}

func (s *Service) reactivateInTx(ctx context.Context, tx pgx.Tx, target staffRow, actorType string, actor uuid.UUID, reason Reason,
	requestID *uuid.UUID, meta Meta) ([]mail, error) {
	if target.Status != "suspended" {
		return nil, ErrTransition
	}
	after, err := updateStatus(ctx, tx, target, "active", false)
	if err != nil {
		return nil, err
	}
	usable, err := usableFactor(ctx, tx, target.ID)
	if err != nil {
		return nil, err
	}
	var mails []mail
	extra := map[string]any{"mfa_reset_issued": !usable}
	if !usable {
		now, err := dbNow(ctx, tx)
		if err != nil {
			return nil, err
		}
		var by *uuid.UUID
		if actor != uuid.Nil {
			by = &actor
		}
		tok, err := s.tokens.IssueEnrolmentToken(ctx, tx, target.ref(), store.EnrolMFAReset, by, requestID, now)
		if err != nil {
			return nil, err
		}
		mails = append(mails, mail{to: target.Email, purpose: store.EnrolMFAReset, token: tok})
	}
	return mails, record(ctx, tx, eventIn{target: target, event: "reactivated", actorType: actorType, actor: actor,
		before: target.state(), after: after, reason: reason, requestID: requestID, meta: meta, extraMetadata: extra})
}

// ChangeRole changes an active tenant staff member's role (both roles grantable by the caller); every session ends.
func (s *Service) ChangeRole(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID, role string, reason Reason) (Result, error) {
	if err := ValidateReason(ActionRoleChange, reason); err != nil {
		return Result{}, err
	}
	if targetTenant == uuid.Nil || !IsTenantRole(role) {
		return Result{}, ErrInvalidInput
	}
	if !Grantable(actor, role) {
		return Result{}, ErrRoleNotGrantable
	}
	if err := refuseSelf(actor, staffID); err != nil {
		return Result{}, err
	}
	var res Result
	err := s.inTargetScope(ctx, actor, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
		target, err := lockActorAndTarget(ctx, tx, actor, staffID, targetTenant)
		if err != nil {
			return err
		}
		if err := authorizeTarget(actor, target); err != nil {
			return err
		}
		if target.Status != "active" || target.Role == role {
			return ErrTransition
		}
		if PersonRequired(role) && target.PersonID == nil {
			return ErrPersonRequired
		}
		r, err := scanStaffRow(tx.QueryRow(ctx, `UPDATE staff_users SET role = $2, updated_at = now() WHERE id = $1 RETURNING `+staffRowColumns, target.ID, role))
		if err != nil {
			return mapDBError(err)
		}
		if err := revokeAccess(ctx, tx, target.ID); err != nil {
			return err
		}
		if err := record(ctx, tx, eventIn{target: target, event: "role_changed", actorType: "staff", actor: actor.StaffID,
			before: target.state(), after: r.state(), reason: reason, meta: actor.Meta}); err != nil {
			return err
		}
		res.Staff, err = loadStaff(ctx, tx, target.ID)
		return err
	})
	return res, mapDBError(err)
}

// ResetMFA resets an active tenant staff member's second factor and emails an mfa_reset token.
func (s *Service) ResetMFA(ctx context.Context, actor Actor, targetTenant, staffID uuid.UUID, reason Reason) (Result, error) {
	if err := ValidateReason(ActionMFAReset, reason); err != nil {
		return Result{}, err
	}
	if targetTenant == uuid.Nil {
		return Result{}, ErrForbidden // platform staff: a platform_mfa_reset four-eyes request
	}
	if err := s.needMailer(); err != nil {
		return Result{}, err
	}
	if err := refuseSelf(actor, staffID); err != nil {
		return Result{}, err
	}
	var res Result
	var mails []mail
	err := s.inTargetScope(ctx, actor, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
		mails = nil
		target, err := lockActorAndTarget(ctx, tx, actor, staffID, targetTenant)
		if err != nil {
			return err
		}
		if err := authorizeTarget(actor, target); err != nil {
			return err
		}
		m, err := s.resetMFAInTx(ctx, tx, target, actor.StaffID, reason, nil, actor.Meta)
		if err != nil {
			return err
		}
		mails = m
		res.Staff, err = loadStaff(ctx, tx, target.ID)
		return err
	})
	if err != nil {
		return Result{}, mapDBError(err)
	}
	res.Delivery = s.deliver(ctx, mails)
	return res, nil
}

func (s *Service) resetMFAInTx(ctx context.Context, tx pgx.Tx, target staffRow, actor uuid.UUID, reason Reason, requestID *uuid.UUID, meta Meta) ([]mail, error) {
	if target.Status != "active" {
		return nil, ErrTransition
	}
	revoked, err := revokeFactors(ctx, tx, target.ID, "reset")
	if err != nil {
		return nil, err
	}
	if err := revokeAccess(ctx, tx, target.ID); err != nil {
		return nil, err
	}
	now, err := dbNow(ctx, tx)
	if err != nil {
		return nil, err
	}
	tok, err := s.tokens.IssueEnrolmentToken(ctx, tx, target.ref(), store.EnrolMFAReset, &actor, requestID, now)
	if err != nil {
		return nil, err
	}
	if err := record(ctx, tx, eventIn{target: target, event: "mfa_reset", actorType: "staff", actor: actor,
		before: target.state(), after: target.state(), reason: reason, requestID: requestID, meta: meta, revokedFactors: revoked}); err != nil {
		return nil, err
	}
	return []mail{{to: target.Email, purpose: store.EnrolMFAReset, token: tok}}, nil
}

// ---- invitation acceptance (the invitee's own transaction) -------------------------------------------------------

// AcceptInvitationInTx moves the invitee's own row invited -> active with passwordHash, inside the caller's
// transaction (scoped to the invitee; internal/httpserver's accept handler has already locked and validated the
// invite token). It writes the invite_accepted event and audit row and returns the new staff_version (the enrol
// challenge is issued under it). Anything but an `invited` row is ErrTransition.
func AcceptInvitationInTx(ctx context.Context, tx pgx.Tx, staffID, tenantID uuid.UUID, passwordHash string, meta Meta) (int64, error) {
	target, err := scanStaffRow(tx.QueryRow(ctx, `SELECT `+staffRowColumns+` FROM staff_users WHERE id = $1 FOR UPDATE`, staffID))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if target.TenantID != tenantID || target.Status != "invited" || passwordHash == "" {
		return 0, ErrTransition
	}
	r, err := scanStaffRow(tx.QueryRow(ctx, `UPDATE staff_users SET status = 'active', password_hash = $2, updated_at = now() WHERE id = $1 RETURNING `+staffRowColumns,
		staffID, passwordHash))
	if err != nil {
		return 0, mapDBError(err)
	}
	if err := record(ctx, tx, eventIn{target: target, event: "invite_accepted", actorType: "staff", actor: staffID,
		before: target.state(), after: r.state(), meta: meta}); err != nil {
		return 0, err
	}
	return r.StaffVersion, nil
}

// PersonLinkedInTx completes a Person link (the existing person-link routes) inside the caller's transaction, right
// after identity.LinkStaffPersonID: the SF012 trigger has bumped staff_version, so every access token of the target
// is already refused by liveness; this also ends every session (a refresh would otherwise mint a token at the new
// version) and writes the person_linked lifecycle event. The route writes its own audit row.
func PersonLinkedInTx(ctx context.Context, tx pgx.Tx, staffID, tenantID, actor uuid.UUID, meta Meta) error {
	target, err := readTarget(ctx, tx, staffID, tenantID)
	if err != nil {
		return err
	}
	before := target.state()
	before.PersonLinked = false
	before.StaffVersion--
	if err := revokeAccess(ctx, tx, staffID); err != nil {
		return err
	}
	return record(ctx, tx, eventIn{target: target, event: "person_linked", actorType: "staff", actor: actor,
		before: before, after: target.state(), meta: meta, eventOnly: true})
}
