package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Diansalas/igaming-platform/internal/staffmfa"
)

// Errors. None carries a secret, a code, a token or a row value.
var (
	// ErrNoActiveFactor: the staff member has no ACTIVE factor of the type.
	// It is "absent", never a verification outcome; an integrity failure of a
	// present factor is OutcomeIntegrity, not this.
	ErrNoActiveFactor = errors.New("staffmfa/store: no active factor")
	// ErrFactorNotFound: no pending factor with that id for that staff member.
	ErrFactorNotFound = errors.New("staffmfa/store: factor not found")
	// ErrPendingExpired: the pending factor's confirmation window has passed.
	ErrPendingExpired = errors.New("staffmfa/store: pending factor expired")
	// ErrStaffNotEligible: the staff row is not visible in this scope, is not
	// in the expected tenant, or is not active.
	ErrStaffNotEligible = errors.New("staffmfa/store: staff member not eligible")
	// ErrConflict: a concurrent writer won a unique index (for example two
	// enrolments begun at once for the same staff member).
	ErrConflict = errors.New("staffmfa/store: concurrent modification")
	// ErrInvalidInput: a malformed argument (nil ids, an unknown purpose).
	ErrInvalidInput = errors.New("staffmfa/store: invalid input")

	// The refusals below are verification outcomes. When a function returns
	// one, its failure counters have been written in the caller's transaction:
	// the caller MUST commit (ADR 0119 section 6.1, "record then return").

	// ErrRejected: a wrong, malformed or replayed value.
	ErrRejected = errors.New("staffmfa/store: assertion rejected")
	// ErrLocked: the factor is soft-locked; nothing was compared.
	ErrLocked = errors.New("staffmfa/store: factor locked")
	// ErrHardLocked: the factor is hard-locked; only an MFA reset clears it.
	ErrHardLocked = errors.New("staffmfa/store: factor hard-locked")
	// ErrIntegrity: the stored factor failed its AEAD check. Counts as a
	// failure and the caller raises staff_mfa_integrity.
	ErrIntegrity = errors.New("staffmfa/store: factor integrity failure")
)

// Refused reports whether err is one of the four verification refusals, that
// is whether the caller must COMMIT its transaction (the counters were
// written) and then answer the refusal.
func Refused(err error) bool {
	return errors.Is(err, ErrRejected) || errors.Is(err, ErrLocked) ||
		errors.Is(err, ErrHardLocked) || errors.Is(err, ErrIntegrity)
}

// VerifyResult is what VerifyDetailed reports. Result is the library result.
type VerifyResult struct {
	staffmfa.Result
	// HardLockSet: this failure set the hard lock (raise staff_mfa_hard_locked).
	HardLockSet bool
	// IntegrityFailure: the stored secret failed AEAD open (raise staff_mfa_integrity).
	IntegrityFailure bool
}

// TOTPFactor is the database-backed staffmfa.StaffFactor for TOTP.
type TOTPFactor struct {
	keys   *staffmfa.Keys
	issuer string
}

var _ staffmfa.StaffFactor = (*TOTPFactor)(nil)

// NewTOTPFactor returns the TOTP factor. issuer is the otpauth issuer
// (STAFF_MFA_ISSUER; never a brand name).
func NewTOTPFactor(keys *staffmfa.Keys, issuer string) (*TOTPFactor, error) {
	if keys == nil {
		return nil, ErrInvalidInput
	}
	if err := staffmfa.ValidateIssuer(issuer); err != nil {
		return nil, err
	}
	return &TOTPFactor{keys: keys, issuer: issuer}, nil
}

// Type implements staffmfa.StaffFactor.
func (*TOTPFactor) Type() staffmfa.FactorType { return staffmfa.FactorTOTP }

// ---- row access -------------------------------------------------------------

// tenantArg is the SQL value of a StaffRef's tenant (NULL for platform staff).
func tenantArg(t uuid.UUID) any {
	if t == uuid.Nil {
		return nil
	}
	return t
}

// timeArg is the SQL value of a possibly-zero time (zero is NULL).
func timeArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func zeroIfNil(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// factorRow is a staff_mfa_factors row as the verification paths need it.
type factorRow struct {
	id             uuid.UUID
	tenant         uuid.UUID // uuid.Nil = platform
	staff          uuid.UUID
	status         string
	sealed         staffmfa.SealedSecret
	lastUsedStep   int64
	counters       staffmfa.Counters
	pendingExpires time.Time
}

const factorColumns = `id, tenant_id, staff_user_id, status, secret_kid, secret_nonce, secret_ct, last_used_step,
	failed_count, failed_window_start, locked_until, failed_24h_count, failed_24h_start, hard_locked_at, pending_expires_at`

func scanFactor(row pgx.Row) (factorRow, error) {
	var (
		f                     factorRow
		tenant                *uuid.UUID
		fws, lu, f24s, hl, pe *time.Time
		failedCount, failed24 int
		kid                   string
		nonce, ct             []byte
	)
	if err := row.Scan(&f.id, &tenant, &f.staff, &f.status, &kid, &nonce, &ct, &f.lastUsedStep,
		&failedCount, &fws, &lu, &failed24, &f24s, &hl, &pe); err != nil {
		return factorRow{}, err
	}
	if tenant != nil {
		f.tenant = *tenant
	}
	f.sealed = staffmfa.SealedSecret{KID: kid, Nonce: nonce, Ciphertext: ct}
	f.counters = staffmfa.Counters{
		FailedCount: failedCount, FailedWindowStart: zeroIfNil(fws), LockedUntil: zeroIfNil(lu),
		Failed24hCount: failed24, Failed24hStart: zeroIfNil(f24s), HardLockedAt: zeroIfNil(hl),
	}
	f.pendingExpires = zeroIfNil(pe)
	return f, nil
}

func (f factorRow) stored() staffmfa.StoredTOTPFactor {
	return staffmfa.StoredTOTPFactor{
		AAD:    staffmfa.FactorAAD{FactorID: f.id, StaffUserID: f.staff, TenantID: f.tenant, FactorType: staffmfa.FactorTOTP},
		Sealed: f.sealed, LastUsedStep: f.lastUsedStep, Counters: f.counters,
	}
}

// lockActive reads the staff member's ACTIVE factor FOR UPDATE. The row lock
// is what serialises concurrent verifiers: it makes the failure counters exact
// and it is the first of the two layers behind the replay burn (the second is
// the conditional UPDATE in burnStep).
func lockActive(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef) (factorRow, error) {
	f, err := scanFactor(tx.QueryRow(ctx, `SELECT `+factorColumns+` FROM staff_mfa_factors
		WHERE staff_user_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid
		  AND factor_type = 'totp' AND status = 'active'
		FOR UPDATE`, staff.StaffID, tenantArg(staff.TenantID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return factorRow{}, ErrNoActiveFactor
	}
	return f, err
}

// lockPending reads one PENDING factor of the staff member FOR UPDATE.
func lockPending(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, id uuid.UUID) (factorRow, error) {
	f, err := scanFactor(tx.QueryRow(ctx, `SELECT `+factorColumns+` FROM staff_mfa_factors
		WHERE id = $1 AND staff_user_id = $2 AND tenant_id IS NOT DISTINCT FROM $3::uuid
		  AND factor_type = 'totp' AND status = 'pending'
		FOR UPDATE`, id, staff.StaffID, tenantArg(staff.TenantID)))
	if errors.Is(err, pgx.ErrNoRows) {
		return factorRow{}, ErrFactorNotFound
	}
	return f, err
}

// persistCounters writes the six counter columns. A hard lock written here is
// write-once at the database (SF002).
func persistCounters(ctx context.Context, tx pgx.Tx, id uuid.UUID, c staffmfa.Counters) error {
	tag, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET failed_count = $2, failed_window_start = $3, locked_until = $4,
			failed_24h_count = $5, failed_24h_start = $6, hard_locked_at = $7
		WHERE id = $1 AND status IN ('active', 'pending')`,
		id, c.FailedCount, timeArg(c.FailedWindowStart), timeArg(c.LockedUntil),
		c.Failed24hCount, timeArg(c.Failed24hStart), timeArg(c.HardLockedAt))
	if err != nil {
		return fmt.Errorf("staffmfa/store: persist counters: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return fmt.Errorf("staffmfa/store: persist counters: %w", ErrConflict)
	}
	return nil
}

// burnStep burns an accepted TOTP step with a CONDITIONAL update: the step is
// recorded only if it is strictly greater than the stored last_used_step (the
// monotonic replay rule). It reports whether this call burned the step. The
// condition, not the row lock, is what guarantees that exactly one of N
// parallel verifiers of the same code wins; the lock makes the counters exact.
// The success path also resets the short failure window (Counters.RecordSuccess).
func burnStep(ctx context.Context, tx pgx.Tx, id uuid.UUID, step int64, c staffmfa.Counters) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET last_used_step = $2, failed_count = $3, failed_window_start = $4
		WHERE id = $1 AND status = 'active' AND last_used_step < $2`,
		id, step, c.FailedCount, timeArg(c.FailedWindowStart))
	if err != nil {
		return false, fmt.Errorf("staffmfa/store: burn step: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ---- StaffFactor ------------------------------------------------------------

// Begin implements staffmfa.StaffFactor: it creates a PENDING factor and
// returns the enrolment material exactly once. Any earlier pending factor of
// the staff member is revoked (pending_superseded, or pending_expired when its
// window had passed). The staff member must be visible in the transaction's
// scope, in staff.TenantID, and active.
func (f *TOTPFactor) Begin(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, now time.Time) (uuid.UUID, staffmfa.EnrolmentMaterial, error) {
	if !staff.Valid() || now.IsZero() {
		return uuid.Nil, staffmfa.EnrolmentMaterial{}, ErrInvalidInput
	}
	var email, status string
	err := tx.QueryRow(ctx, `SELECT email, status FROM staff_users WHERE id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid`,
		staff.StaffID, tenantArg(staff.TenantID)).Scan(&email, &status)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && status != "active") {
		return uuid.Nil, staffmfa.EnrolmentMaterial{}, ErrStaffNotEligible
	}
	if err != nil {
		return uuid.Nil, staffmfa.EnrolmentMaterial{}, fmt.Errorf("staffmfa/store: read staff: %w", err)
	}

	if _, err := tx.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = $3, pending_expires_at = NULL,
			revoked_reason = CASE WHEN pending_expires_at <= $3 THEN 'pending_expired' ELSE 'pending_superseded' END
		WHERE staff_user_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid AND factor_type = 'totp' AND status = 'pending'`,
		staff.StaffID, tenantArg(staff.TenantID), now.UTC()); err != nil {
		return uuid.Nil, staffmfa.EnrolmentMaterial{}, fmt.Errorf("staffmfa/store: supersede pending: %w", err)
	}

	secret, err := staffmfa.GenerateTOTPSecret()
	if err != nil {
		return uuid.Nil, staffmfa.EnrolmentMaterial{}, err
	}
	id := uuid.New()
	sealed, err := f.keys.SealFactorSecret(staffmfa.FactorAAD{
		FactorID: id, StaffUserID: staff.StaffID, TenantID: staff.TenantID, FactorType: staffmfa.FactorTOTP}, secret)
	if err != nil {
		return uuid.Nil, staffmfa.EnrolmentMaterial{}, err
	}
	material, err := staffmfa.NewTOTPEnrolmentMaterial(f.issuer, email, secret)
	if err != nil {
		return uuid.Nil, staffmfa.EnrolmentMaterial{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO staff_mfa_factors
			(id, tenant_id, staff_user_id, factor_type, status, secret_kid, secret_nonce, secret_ct, pending_expires_at, created_at)
		VALUES ($1, $2, $3, 'totp', 'pending', $4, $5, $6, $7, $8)`,
		id, tenantArg(staff.TenantID), staff.StaffID, sealed.KID, sealed.Nonce, sealed.Ciphertext,
		now.Add(staffmfa.PendingFactorTTL).UTC(), now.UTC()); err != nil {
		if isUniqueViolation(err) {
			return uuid.Nil, staffmfa.EnrolmentMaterial{}, ErrConflict
		}
		return uuid.Nil, staffmfa.EnrolmentMaterial{}, fmt.Errorf("staffmfa/store: insert pending factor: %w", err)
	}
	return id, material, nil
}

// Confirm implements staffmfa.StaffFactor: it activates the PENDING factor id
// after a valid TOTP assertion, burning the accepted step, and revokes the
// staff member's previously active factor of the type ('replaced'), all inside
// one savepoint so a failure cannot leave the staff member without a factor.
//
// A wrong, malformed, replayed or locked assertion returns a Refusal after
// writing the failure counters on the PENDING row (the caller commits).
func (f *TOTPFactor) Confirm(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, id uuid.UUID, a staffmfa.Assertion, now time.Time) error {
	if !staff.Valid() || id == uuid.Nil || now.IsZero() {
		return ErrInvalidInput
	}
	row, err := lockPending(ctx, tx, staff, id)
	if err != nil {
		return err
	}
	if !now.Before(row.pendingExpires) {
		return ErrPendingExpired
	}
	res := f.keys.VerifyTOTP(row.stored(), a, now)
	if res.Outcome == staffmfa.OutcomeAccepted {
		ok, err := f.activate(ctx, tx, staff, row, res.AcceptedStep, now)
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		res = staffmfa.Result{Outcome: staffmfa.OutcomeRejected} // lost the burn: a replay
	}
	_, err = recordOutcome(ctx, tx, row, res, now)
	if err != nil {
		return err
	}
	return refusalFor(res.Outcome)
}

// activate revokes the previous active factor and activates row in a savepoint.
func (f *TOTPFactor) activate(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, row factorRow, step int64, now time.Time) (bool, error) {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("staffmfa/store: savepoint: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = sp.Rollback(ctx)
		}
	}()
	if _, err := sp.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'revoked', revoked_at = $3, revoked_reason = 'replaced'
		WHERE staff_user_id = $1 AND tenant_id IS NOT DISTINCT FROM $2::uuid AND factor_type = 'totp' AND status = 'active'`,
		staff.StaffID, tenantArg(staff.TenantID), now.UTC()); err != nil {
		return false, fmt.Errorf("staffmfa/store: revoke replaced factor: %w", err)
	}
	// Conditional burn + activation: the pending row starts at last_used_step 0, so any real step passes, and a
	// replay of a step the row already burned does not.
	tag, err := sp.Exec(ctx, `UPDATE staff_mfa_factors SET status = 'active', activated_at = $3, pending_expires_at = NULL,
			last_used_step = $2, failed_count = 0, failed_window_start = NULL
		WHERE id = $1 AND status = 'pending' AND last_used_step < $2`, row.id, step, now.UTC())
	if err != nil {
		if isUniqueViolation(err) {
			return false, ErrConflict
		}
		return false, fmt.Errorf("staffmfa/store: activate factor: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return false, nil // rolled back by the deferred Rollback: the old factor is untouched
	}
	if err := sp.Commit(ctx); err != nil {
		return false, fmt.Errorf("staffmfa/store: release savepoint: %w", err)
	}
	committed = true
	return true, nil
}

// Verify implements staffmfa.StaffFactor. A refusal (wrong, replayed, locked,
// integrity) is reported ONLY through Result.Outcome with a nil error, so a
// caller that treats every error as a fault and rolls back cannot lose the
// failure counters of a refusal: the error is reserved for faults and for
// ErrNoActiveFactor. After any nil-error return the caller commits. Callers
// that need the alert flags use VerifyDetailed.
func (f *TOTPFactor) Verify(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, a staffmfa.Assertion, now time.Time) (staffmfa.Result, error) {
	vr, err := f.VerifyDetailed(ctx, tx, staff, a, now)
	return vr.Result, err
}

// VerifyDetailed checks an assertion against the staff member's ACTIVE factor
// and atomically burns it: it locks the factor row FOR UPDATE, refuses a
// locked factor before any comparison, and on success burns the step with a
// conditional update. On any non-accepted outcome the failure counters are
// written (locked outcomes write nothing); the outcome is in VerifyResult and
// the error is nil.
func (f *TOTPFactor) VerifyDetailed(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, a staffmfa.Assertion, now time.Time) (VerifyResult, error) {
	if !staff.Valid() || now.IsZero() {
		return VerifyResult{}, ErrInvalidInput
	}
	row, err := lockActive(ctx, tx, staff)
	if err != nil {
		return VerifyResult{}, err
	}
	return f.verifyLocked(ctx, tx, row, a, now)
}

// verifyLocked is everything after the row lock. It is separate so that the conditional burn can be tested against a
// STALE row (read before another transaction burned the step), which the lock makes unreachable in production: the
// conditional UPDATE, not the lock, is what refuses the second burn of one step.
func (f *TOTPFactor) verifyLocked(ctx context.Context, tx pgx.Tx, row factorRow, a staffmfa.Assertion, now time.Time) (VerifyResult, error) {
	res := f.keys.VerifyTOTP(row.stored(), a, now)
	if res.Outcome == staffmfa.OutcomeAccepted {
		burned, err := burnStep(ctx, tx, row.id, res.AcceptedStep, row.counters.RecordSuccess())
		if err != nil {
			return VerifyResult{}, err
		}
		if burned {
			return VerifyResult{Result: res}, nil
		}
		res = staffmfa.Result{Outcome: staffmfa.OutcomeRejected} // the step was already burned: a replay
	}
	hard, err := recordOutcome(ctx, tx, row, res, now)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{Result: res, HardLockSet: hard, IntegrityFailure: res.Outcome == staffmfa.OutcomeIntegrity}, nil
}

// recordOutcome writes the failure counters for a Rejected or Integrity
// outcome and nothing for a locked one. It reports whether the hard lock was
// set by this failure.
func recordOutcome(ctx context.Context, tx pgx.Tx, row factorRow, res staffmfa.Result, now time.Time) (bool, error) {
	switch res.Outcome {
	case staffmfa.OutcomeRejected, staffmfa.OutcomeIntegrity:
		next, hard := row.counters.RecordFailure(now)
		return hard, persistCounters(ctx, tx, row.id, next)
	}
	return false, nil
}

func refusalFor(o staffmfa.Outcome) error {
	switch o {
	case staffmfa.OutcomeAccepted:
		return nil
	case staffmfa.OutcomeLocked:
		return ErrLocked
	case staffmfa.OutcomeHardLocked:
		return ErrHardLocked
	case staffmfa.OutcomeIntegrity:
		return ErrIntegrity
	}
	return ErrRejected
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}
