package stafflifecycle

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/actorproof"
	"github.com/Diansalas/igaming-platform/internal/audit"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/email"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

// DefaultLockTimeout is the T2 lock_timeout of the suspension phasing (ADR 0119 s9.5).
const DefaultLockTimeout = 5 * time.Second

// Service is the staff lifecycle. Build it with New.
type Service struct {
	pool        *db.Pool
	tokens      *store.Store
	mailer      email.Provider
	proofs      *actorproof.Issuer // nil: actorproof.Default()
	linkBase    string
	lockTimeout time.Duration
	logger      *slog.Logger
}

// New builds the service. keys must be the process staff MFA keys (staffmfa.LoadKeys): invite and reset tokens are
// keyed MACs. mailer nil makes every action that emails a token answer ErrUnavailable before anything is written.
// linkBase is the back-office origin the emailed link starts with (empty: a relative `/#/enrol?...` link).
func New(pool *db.Pool, keys *staffmfa.Keys, mailer email.Provider, linkBase string) (*Service, error) {
	if pool == nil || keys == nil {
		return nil, fmt.Errorf("stafflifecycle: a pool and the staff MFA keys are required")
	}
	st, err := store.New(keys)
	if err != nil {
		return nil, err
	}
	return &Service{pool: pool, tokens: st, mailer: mailer, linkBase: linkBase, lockTimeout: DefaultLockTimeout, logger: slog.Default()}, nil
}

// WithProofIssuer sets the actor-proof issuer (nil: the process default).
func (s *Service) WithProofIssuer(i *actorproof.Issuer) *Service { s.proofs = i; return s }

// WithLogger sets the logger used for post-commit email failures.
func (s *Service) WithLogger(l *slog.Logger) *Service {
	if l != nil {
		s.logger = l
	}
	return s
}

func (s *Service) issuer() *actorproof.Issuer {
	if s.proofs != nil {
		return s.proofs
	}
	return actorproof.Default()
}

// ---- scope -------------------------------------------------------------------------------------------------------

// inTargetScope runs fn in the transaction scope of a target in targetTenant (uuid.Nil: platform):
//   - a platform target: db.WithPlatformAdmin(actor) (app.tenant_id empty: platform rows; the actor's own row is
//     visible; SF013 compares app.platform_admin_principal_id);
//   - a tenant target, tenant actor of that tenant: db.WithPrincipalScope(tenant, actor) (SF013 compares
//     app.principal_id; SF021 applies: the actor is tenant-scoped);
//   - a tenant target, platform actor: db.WithTenant(target) + db.SetActorStaffIDForCurrentTx(actor) (the
//     migration 0139 staff_actor_self_* pair makes the actor's own platform row visible for its FOR SHARE
//     re-assertion; SF013 compares app.actor_staff_id; SF021 sees a platform actor and stands aside).
//
// A tenant actor naming another tenant is ErrNotFound (never data).
func (s *Service) inTargetScope(ctx context.Context, actor Actor, targetTenant uuid.UUID, fn db.TxFunc) error {
	switch {
	case targetTenant == uuid.Nil:
		if !actor.Platform() {
			return ErrForbidden
		}
		return s.pool.WithPlatformAdmin(ctx, actor.StaffID, fn)
	case actor.Platform():
		return s.pool.WithTenant(ctx, targetTenant, func(ctx context.Context, tx pgx.Tx) error {
			if err := db.SetActorStaffIDForCurrentTx(ctx, tx, actor.StaffID); err != nil {
				return err
			}
			return fn(ctx, tx)
		})
	case actor.TenantID == targetTenant:
		return s.pool.WithPrincipalScope(ctx, targetTenant, actor.StaffID, fn)
	default:
		return ErrNotFound
	}
}

// ---- rows and locks ----------------------------------------------------------------------------------------------

type staffRow struct {
	ID           uuid.UUID
	TenantID     uuid.UUID
	Email        string
	Role         string
	Status       string
	StaffVersion int64
	PersonID     *uuid.UUID
}

func (r staffRow) state() State {
	return State{Status: r.Status, Role: r.Role, StaffVersion: r.StaffVersion, PersonLinked: r.PersonID != nil}
}

func (r staffRow) ref() staffmfa.StaffRef {
	return staffmfa.StaffRef{StaffID: r.ID, TenantID: r.TenantID}
}

const staffRowColumns = `id, tenant_id, email, role, status, staff_version, person_id`

func scanStaffRow(row pgx.Row) (staffRow, error) {
	var r staffRow
	var tenant *uuid.UUID
	if err := row.Scan(&r.ID, &tenant, &r.Email, &r.Role, &r.Status, &r.StaffVersion, &r.PersonID); err != nil {
		return staffRow{}, err
	}
	if tenant != nil {
		r.TenantID = *tenant
	}
	return r, nil
}

// readTarget reads the target (no lock: phase T1 of the suspension takes no staff_users lock).
func readTarget(ctx context.Context, tx pgx.Tx, targetID, targetTenant uuid.UUID) (staffRow, error) {
	r, err := scanStaffRow(tx.QueryRow(ctx, `SELECT `+staffRowColumns+` FROM staff_users WHERE id = $1`, targetID))
	if errors.Is(err, pgx.ErrNoRows) {
		return staffRow{}, ErrNotFound
	}
	if err != nil {
		return staffRow{}, err
	}
	if r.TenantID != targetTenant {
		return staffRow{}, ErrNotFound
	}
	return r, nil
}

// lockActorAndTarget is the ADR 0119 s9.5 staff lock of a lifecycle transaction: the actor's own row FOR SHARE and
// the target's FOR UPDATE, in ascending id order (ADR 0082 A9: the actor joins the ascending-id A8 set), then the
// re-assertion of the actor against the verified token (active, role, staff_version). It runs after any L1 row the
// flow takes (the four-eyes request FOR UPDATE).
func lockActorAndTarget(ctx context.Context, tx pgx.Tx, actor Actor, targetID, targetTenant uuid.UUID) (staffRow, error) {
	ids := []uuid.UUID{actor.StaffID, targetID}
	sort.Slice(ids, func(i, j int) bool { return lessUUID(ids[i], ids[j]) })
	var target staffRow
	found := false
	for _, id := range ids {
		if id == actor.StaffID {
			locked, err := db.ScanActorStaffRows(tx.Query(ctx,
				`SELECT id, role, status, staff_version FROM staff_users WHERE id = $1 FOR SHARE`, actor.StaffID))
			if err != nil {
				return staffRow{}, err
			}
			if err := db.CheckActorStaffLive(locked, actor.StaffID, db.ActorExpectation{Role: actor.Role, StaffVersion: actor.StaffVersion}); err != nil {
				return staffRow{}, err
			}
			continue
		}
		r, err := scanStaffRow(tx.QueryRow(ctx, `SELECT `+staffRowColumns+` FROM staff_users WHERE id = $1 FOR UPDATE`, id))
		if errors.Is(err, pgx.ErrNoRows) {
			return staffRow{}, ErrNotFound
		}
		if err != nil {
			return staffRow{}, err
		}
		target, found = r, true
	}
	if !found || target.TenantID != targetTenant {
		return staffRow{}, ErrNotFound
	}
	return target, nil
}

func lessUUID(a, b uuid.UUID) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// actorPerson reads the actor's own Person (visible in every lifecycle scope, see inTargetScope).
func actorPerson(ctx context.Context, tx pgx.Tx, actor uuid.UUID) (*uuid.UUID, error) {
	var p *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT person_id FROM staff_users WHERE id = $1`, actor).Scan(&p)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, db.ErrActorStaffNotLive
	}
	return p, err
}

// ---- revocations -------------------------------------------------------------------------------------------------

// revokeAccess ends every session, live challenge and live enrolment token of the target. It sets app.principal_id
// to the target (auth.RevokeAllSessionsForPrincipal), so it runs AFTER any staff_users update of the same
// transaction (SF013 reads that GUC).
func revokeAccess(ctx context.Context, tx pgx.Tx, target uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE staff_auth_challenges SET consumed_at = now() WHERE staff_user_id = $1 AND consumed_at IS NULL`, target); err != nil {
		return fmt.Errorf("stafflifecycle: revoke challenges: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE staff_enrolment_tokens SET revoked_at = now()
		WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, target); err != nil {
		return fmt.Errorf("stafflifecycle: revoke enrolment tokens: %w", err)
	}
	return auth.RevokeAllSessionsForPrincipal(ctx, tx, auth.PrincipalStaff, target)
}

// revokeFactors revokes every pending / active factor and every live recovery code of the target.
func revokeFactors(ctx context.Context, tx pgx.Tx, target uuid.UUID, reason string) ([]uuid.UUID, error) {
	rows, err := tx.Query(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = now(), revoked_reason = $2, pending_expires_at = NULL
		WHERE staff_user_id = $1 AND status IN ('pending', 'active') RETURNING id`, target, reason)
	if err != nil {
		return nil, fmt.Errorf("stafflifecycle: revoke factors: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[uuid.UUID])
	if err != nil {
		return nil, fmt.Errorf("stafflifecycle: revoke factors: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE staff_mfa_recovery_codes SET revoked_at = now()
		WHERE staff_user_id = $1 AND consumed_at IS NULL AND revoked_at IS NULL`, target); err != nil {
		return nil, fmt.Errorf("stafflifecycle: revoke recovery codes: %w", err)
	}
	return ids, nil
}

// usableFactor reports whether the target has an active factor that is not hard-locked (the liveness rule).
func usableFactor(ctx context.Context, tx pgx.Tx, target uuid.UUID) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM staff_mfa_factors WHERE staff_user_id = $1 AND status = 'active' AND hard_locked_at IS NULL)`, target).Scan(&ok)
	return ok, err
}

// ---- events and audit --------------------------------------------------------------------------------------------

type eventIn struct {
	target         staffRow
	event          string
	actorType      string
	actor          uuid.UUID // uuid.Nil for system / owner
	before, after  State
	reason         Reason
	requestID      *uuid.UUID
	meta           Meta
	auditAction    string
	extraMetadata  map[string]any
	revokedFactors []uuid.UUID
	auditOnly      bool // write the audit row only (suspension phase 1)
	eventOnly      bool // write the event row only (the caller writes its own audit row)
}

func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// record writes the staff_lifecycle_events row and the audit row of one change (ADR 0119 s10.6). The audit row is
// tenant-owned for a tenant target (the tenant sees the action on its own staff; a platform actor's identity is
// redacted by the tenant read paths, ADR 0114 H1) and platform-owned for a platform target.
func record(ctx context.Context, tx pgx.Tx, in eventIn) error {
	var actorStaff *uuid.UUID
	if in.actorType == "staff" {
		a := in.actor
		actorStaff = &a
	}
	before, after := in.before, in.after
	if !in.auditOnly {
		if _, err := tx.Exec(ctx, `INSERT INTO staff_lifecycle_events
				(staff_user_id, event, actor_type, actor_staff_id, before, after, reason_code, note, lifecycle_request_id, request_id, ip, user_agent)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
			in.target.ID, in.event, in.actorType, actorStaff, before, after, nullable(in.reason.Code), nullable(in.reason.Note),
			in.requestID, nullable(truncate(in.meta.RequestID, 128)), nullable(truncate(in.meta.IP, 64)), nullable(truncate(in.meta.UserAgent, 512))); err != nil {
			return fmt.Errorf("stafflifecycle: insert lifecycle event: %w", err)
		}
	}
	if in.eventOnly {
		return nil
	}
	md := map[string]any{"before": before, "after": after}
	if in.reason.Code != "" {
		md["reason_code"] = in.reason.Code
	}
	if in.requestID != nil {
		md["lifecycle_request_id"] = in.requestID.String()
	}
	if len(in.revokedFactors) > 0 {
		ids := make([]string, len(in.revokedFactors))
		for i, f := range in.revokedFactors {
			ids[i] = f.String()
		}
		md["revoked_factor_ids"] = ids
	}
	for k, v := range in.extraMetadata {
		md[k] = v
	}
	action := in.auditAction
	if action == "" {
		action = "staff." + in.event
	}
	actorType := audit.ActorStaff
	actorID := in.actor
	if in.actorType != "staff" {
		actorType, actorID = audit.ActorSystem, uuid.Nil
	}
	return audit.Record(ctx, tx, audit.Entry{
		TenantID: in.target.TenantID, ActorType: actorType, ActorID: actorID,
		Action: action, TargetType: "staff_user", TargetID: in.target.ID.String(), Outcome: audit.OutcomeSuccess,
		IPAddress: in.meta.IP, UserAgent: in.meta.UserAgent, RequestID: in.meta.RequestID, Metadata: md,
	})
}

// truncate cuts s to at most n bytes on a rune boundary, after replacing invalid UTF-8 (review fix F9: a byte cut
// inside a multi-byte character, or a non-UTF-8 header byte, would make the event INSERT fail with 22021).
func truncate(s string, n int) string {
	s = strings.ToValidUTF8(s, "?")
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ---- error mapping -----------------------------------------------------------------------------------------------

// mapDBError turns a guard SQLSTATE into the package error (the triggers are the backstop of the Go checks).
func mapDBError(err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return err
	}
	switch pgErr.Code {
	case "SF010", "SF031":
		return fmt.Errorf("%w (%s)", ErrTransition, pgErr.Code)
	case "SF013":
		return fmt.Errorf("%w (%s)", ErrSelfTarget, pgErr.Code)
	case "SF014":
		return fmt.Errorf("%w (%s)", ErrPersonRequired, pgErr.Code)
	case "SF020", "SF021":
		return fmt.Errorf("%w (%s)", ErrLastAdmin, pgErr.Code)
	case "SF032":
		return fmt.Errorf("%w (%s)", ErrFourEyes, pgErr.Code)
	case "55P03":
		return fmt.Errorf("%w (%s)", ErrBusy, pgErr.Code)
	case "AP001", "AP002", "AP003", "AP004", "AP005":
		return fmt.Errorf("%w (%s)", ErrProofRefused, pgErr.Code)
	case "23505":
		return fmt.Errorf("%w (%s)", ErrConflict, pgErr.Code)
	case "23503":
		if pgErr.ConstraintName == "staff_users_person_id_fkey" || pgErr.ConstraintName == "staff_lifecycle_requests_invite_person_id_fkey" {
			return fmt.Errorf("%w (%s)", ErrPersonNotFound, pgErr.Code)
		}
	}
	return err
}

// ---- email -------------------------------------------------------------------------------------------------------

// mail is a token to email after the commit (never before: a rolled-back token must not be delivered).
type mail struct {
	to      string
	purpose store.EnrolmentPurpose
	token   staffmfa.Token
}

// deliver hands each mail to the provider. The body carries the single-use token; it is never logged here or by
// the provider (internal/email). A failure is logged without the body and reported as DeliveryFailed.
func (s *Service) deliver(ctx context.Context, mails []mail) Delivery {
	if len(mails) == 0 {
		return DeliveryNone
	}
	out := DeliverySent
	// Review fix F5: the hand-off runs after the commit and must not be cut short by the caller disconnecting.
	ctx = context.WithoutCancel(ctx)
	for _, m := range mails {
		subject, body := s.compose(m)
		if err := s.mailer.Send(ctx, email.Message{To: m.to, Subject: subject, Body: body}); err != nil {
			s.logger.Error("staff_lifecycle_email_failed", "purpose", string(m.purpose), "error", err.Error())
			out = DeliveryFailed
		}
	}
	return out
}

func (s *Service) compose(m mail) (string, string) {
	tok := m.token.RevealForDelivery()
	switch m.purpose {
	case store.EnrolInvite:
		return "Your back-office invitation",
			"You have been invited to the back office. Open this single-use link within 72 hours to set your password and enrol your authenticator:\n\n" +
				s.linkBase + "/#/enrol?token=" + tok + "&invite=1\n\nIf you did not expect this invitation, ignore this email."
	default:
		return "Your back-office second factor was reset",
			"An administrator reset your second factor. Open this single-use link within 24 hours, sign in with your current password and enrol your authenticator again:\n\n" +
				s.linkBase + "/#/enrol?token=" + tok + "\n\nIf you did not expect this, contact your administrator."
	}
}

func (s *Service) needMailer() error {
	if s.mailer == nil {
		return ErrUnavailable
	}
	return nil
}
