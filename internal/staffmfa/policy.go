package staffmfa

import (
	"fmt"
	"time"
)

// Policy constants of ADR 0119 sections 6.3, 6.5 and 7.4. Values marked
// [ORCH-DEFAULT] in the ADR are reversible orchestrator/security defaults,
// not owner-approved values. They are code, not configuration rows (section
// 4.1): there is no enforcement flag and no development bypass.
const (
	// ChallengeTTL and ChallengeAttempts: a login or enrolment challenge
	// (fixed, section 7.4).
	ChallengeTTL      = 5 * time.Minute
	ChallengeAttempts = 5
	// PendingFactorTTL: a factor that was begun but not confirmed (fixed).
	PendingFactorTTL = 10 * time.Minute

	// SoftLockThreshold failures within SoftLockWindow set
	// locked_until = now + SoftLockDuration [ORCH-DEFAULT].
	SoftLockThreshold = 5
	SoftLockWindow    = 15 * time.Minute
	SoftLockDuration  = 15 * time.Minute
	// HardLockThreshold failures within HardLockWindow hard-lock the factor;
	// only an MFA reset clears it [ORCH-DEFAULT].
	HardLockThreshold = 10
	HardLockWindow    = 24 * time.Hour

	// Enrolment-token lifetimes [ORCH-DEFAULT] (section 6.3).
	InviteTokenTTL    = 72 * time.Hour
	MFAResetTokenTTL  = 24 * time.Hour
	BootstrapTokenTTL = 24 * time.Hour
	// PasswordResetTokenTTL is not stated by the ADR; section 6.7 gives the
	// password_reset token the same authority as an MFA reset, so it takes
	// the MFA reset lifetime. Recorded in the S1 implementation record.
	PasswordResetTokenTTL = MFAResetTokenTTL

	// MaxEnrolmentTokenTTL bounds every enrolment token, and so how long an
	// old MAC kid must stay verify-only (section 5.3).
	MaxEnrolmentTokenTTL = InviteTokenTTL
)

// EnrolmentTokenTTL returns the lifetime of an enrolment token of kind, or
// an error for a kind that is not an enrolment token.
func EnrolmentTokenTTL(kind MACKind) (time.Duration, error) {
	switch kind {
	case MACEnrolInvite:
		return InviteTokenTTL, nil
	case MACEnrolMFAReset:
		return MFAResetTokenTTL, nil
	case MACEnrolPasswordReset:
		return PasswordResetTokenTTL, nil
	case MACEnrolBootstrap:
		return BootstrapTokenTTL, nil
	}
	return 0, ErrInvalidInput
}

// Session and step-up bounds (section 7.4). Configuration may only tighten
// the defaults; a value outside [Min, Max] refuses startup.
type Bound struct {
	Name     string
	Default  time.Duration
	Min, Max time.Duration
}

// Check refuses d outside the bound. The error names the setting only.
func (b Bound) Check(d time.Duration) error {
	if d < b.Min || d > b.Max {
		return fmt.Errorf("staffmfa: %s must be between %s and %s", b.Name, b.Min, b.Max)
	}
	return nil
}

// The section 7.4 bounds. Names are the environment variables that set them.
var (
	StaffAccessTokenTTLBound = Bound{Name: "STAFF_ACCESS_TOKEN_TTL_SECONDS",
		Default: 15 * time.Minute, Min: time.Minute, Max: 15 * time.Minute}
	StaffRefreshIdleTTLBound = Bound{Name: "STAFF_REFRESH_IDLE_TTL_SECONDS",
		Default: 30 * time.Minute, Min: 5 * time.Minute, Max: 60 * time.Minute}
	StaffSessionAbsoluteTTLBound = Bound{Name: "STAFF_SESSION_ABSOLUTE_TTL_SECONDS",
		Default: 12 * time.Hour, Min: time.Hour, Max: 12 * time.Hour}
	StepUpCriticalMaxAgeBound = Bound{Name: "STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS",
		Default: 5 * time.Minute, Min: time.Minute, Max: 5 * time.Minute}
	StepUpFinancialMaxAgeBound = Bound{Name: "STAFF_STEP_UP_FINANCIAL_MAX_AGE_SECONDS",
		Default: 10 * time.Minute, Min: time.Minute, Max: 10 * time.Minute}
)

// ---- per-factor failure counters (section 6.5) ------------------------------

// LockState is the closed set of factor lock states.
type LockState int

// Lock states. The zero value is LockInvalid, so an unset state fails closed.
const (
	LockInvalid LockState = iota
	LockOpen
	LockSoft
	LockHard
)

// Locked reports whether the state refuses verification (every state but
// LockOpen).
func (s LockState) Locked() bool { return s != LockOpen }

// Counters mirrors the factor row's failure columns: failed_count,
// failed_window_start, locked_until, failed_24h_count, failed_24h_start and
// hard_locked_at. A zero time is SQL NULL.
type Counters struct {
	FailedCount       int
	FailedWindowStart time.Time
	LockedUntil       time.Time
	Failed24hCount    int
	Failed24hStart    time.Time
	HardLockedAt      time.Time
}

// valid rejects states the database CHECKs and the transitions below can
// never produce; such a row is treated as locked (fail closed).
func (c Counters) valid() bool {
	return c.FailedCount >= 0 && c.FailedCount < SoftLockThreshold &&
		c.Failed24hCount >= 0 && c.Failed24hCount <= HardLockThreshold &&
		(c.FailedCount == 0) == c.FailedWindowStart.IsZero() &&
		(c.Failed24hCount == 0) == c.Failed24hStart.IsZero()
}

// LockState evaluates the counters at now. A hard lock wins over everything;
// a soft lock holds while now < locked_until; inconsistent counters are
// LockInvalid. A locked factor answers mfa_locked BEFORE any comparison.
func (c Counters) LockState(now time.Time) LockState {
	if !c.HardLockedAt.IsZero() {
		return LockHard
	}
	if !c.valid() {
		return LockInvalid
	}
	if !c.LockedUntil.IsZero() && now.Before(c.LockedUntil) {
		return LockSoft
	}
	return LockOpen
}

// windowOpen reports whether a counting window that started at start is
// still open at now. A clock that went backwards (now before start) keeps the
// window open, so a skewed clock never resets a counter early.
func windowOpen(start, now time.Time, length time.Duration) bool {
	return !start.IsZero() && now.Sub(start) < length
}

// RecordFailure returns the counters after one failed second-factor attempt
// at now (a wrong, malformed or replayed code, a wrong recovery code, a
// failed step-up, or an integrity failure). Callers only reach it for an
// unlocked factor. The second return value is true when this failure set the
// hard lock (the caller raises staff_mfa_hard_locked).
func (c Counters) RecordFailure(now time.Time) (Counters, bool) {
	out := c
	if windowOpen(c.FailedWindowStart, now, SoftLockWindow) {
		out.FailedCount = c.FailedCount + 1
	} else {
		out.FailedCount, out.FailedWindowStart = 1, now
	}
	if out.FailedCount >= SoftLockThreshold {
		// The lock starts a fresh window: after locked_until the count
		// starts again from zero.
		out.LockedUntil = now.Add(SoftLockDuration)
		out.FailedCount, out.FailedWindowStart = 0, time.Time{}
	}
	if windowOpen(c.Failed24hStart, now, HardLockWindow) {
		out.Failed24hCount = c.Failed24hCount + 1
	} else {
		out.Failed24hCount, out.Failed24hStart = 1, now
	}
	hard := false
	if out.Failed24hCount >= HardLockThreshold && c.HardLockedAt.IsZero() {
		out.HardLockedAt = now
		hard = true
	}
	return out, hard
}

// RecordSuccess returns the counters after an accepted assertion: the short
// window resets. The 24 hour count does not: ADR 0119 section 6.5 counts 10
// failures within 24 hours regardless of successes in between.
func (c Counters) RecordSuccess() Counters {
	out := c
	out.FailedCount, out.FailedWindowStart = 0, time.Time{}
	return out
}
