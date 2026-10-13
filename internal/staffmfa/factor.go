package staffmfa

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/staffmfa/totp"
)

// FactorType is a closed vocabulary mirrored by a DB CHECK (ADR 0119 section
// 4.2); adding a value needs a migration and an ADR amendment.
type FactorType string

// Factor types. Only TOTP is implemented; "webauthn" is reserved and is NOT a
// valid value until its ADR amendment lands.
const (
	FactorTOTP FactorType = "totp"
)

// Valid reports whether t is in the closed vocabulary.
func (t FactorType) Valid() bool { return t == FactorTOTP }

// StaffRef identifies the staff member a factor operation is for. It is
// taken from a verified challenge or token, never from a request body.
// TenantID is uuid.Nil for a platform staff member.
type StaffRef struct {
	StaffID  uuid.UUID
	TenantID uuid.UUID
}

// Valid reports whether the reference names a staff member.
func (r StaffRef) Valid() bool { return r.StaffID != uuid.Nil }

// AMR values (RFC 8176) a factor contributes (ADR 0119 section 4.2).
const (
	AMRPassword = "pwd"
	AMROTP      = "otp"
	AMRMFA      = "mfa"
)

// Outcome is the closed set of verification outcomes.
type Outcome int

// Verification outcomes. The zero value is a refusal, so a Result that was
// never filled in fails closed.
const (
	// OutcomeRejected: wrong, malformed or replayed value. Counts as a failure.
	OutcomeRejected Outcome = iota
	// OutcomeAccepted: the assertion verified and was burned.
	OutcomeAccepted
	// OutcomeLocked: the factor is soft-locked; nothing was compared.
	OutcomeLocked
	// OutcomeHardLocked: the factor is hard-locked; nothing was compared.
	OutcomeHardLocked
	// OutcomeIntegrity: the stored factor failed its integrity check (AEAD
	// open failed). Counts as a failure and raises staff_mfa_integrity.
	OutcomeIntegrity
)

// Result is what StaffFactor.Verify reports.
type Result struct {
	Outcome Outcome
	// AcceptedStep is the TOTP step that was burned (OutcomeAccepted only).
	AcceptedStep int64
	// AMR is the RFC 8176 value the factor contributes on success.
	AMR string
}

// Accepted reports whether the assertion verified.
func (r Result) Accepted() bool { return r.Outcome == OutcomeAccepted }

// StaffFactor is the internal second-factor interface (ADR 0119 section 4.2).
// The TOTP implementation lands with the store layer (S2) and the flows (S3);
// S1 provides the pure primitives it is built from (VerifyTOTP, the key
// families, the counters). WebAuthn can be added later as another
// implementation.
type StaffFactor interface {
	Type() FactorType
	// Begin creates a PENDING factor and returns what the user needs to enrol
	// it, exactly once.
	Begin(ctx context.Context, tx pgx.Tx, staff StaffRef, now time.Time) (factorID uuid.UUID, enrolment EnrolmentMaterial, err error)
	// Confirm activates a PENDING factor after a valid assertion; it revokes
	// any previously active factor of the same type.
	Confirm(ctx context.Context, tx pgx.Tx, staff StaffRef, id uuid.UUID, a Assertion, now time.Time) error
	// Verify checks an assertion against the staff member's ACTIVE factor and
	// atomically burns it (replay protection), updating failure counters on
	// refusal. It locks the factor row FOR UPDATE.
	Verify(ctx context.Context, tx pgx.Tx, staff StaffRef, a Assertion, now time.Time) (Result, error)
}

// StoredTOTPFactor is the slice of a factor row TOTP verification needs.
type StoredTOTPFactor struct {
	AAD          FactorAAD
	Sealed       SealedSecret
	LastUsedStep int64
	Counters     Counters
}

// VerifyTOTP is the pure TOTP verification step of StaffFactor.Verify, in the
// order ADR 0119 sections 6.5 and 6.1 require:
//
//  1. a hard-locked or soft-locked factor (or invalid counters) is refused
//     BEFORE the secret is opened or any code is compared;
//  2. the sealed secret is opened; an open failure is OutcomeIntegrity;
//  3. the code is compared in constant time across T-1, T, T+1 with the
//     monotonic replay rule.
//
// The caller persists the outcome: on OutcomeAccepted the new last_used_step
// (a conditional UPDATE ... WHERE last_used_step < $step) and
// Counters.RecordSuccess; on OutcomeRejected or OutcomeIntegrity
// Counters.RecordFailure. A non-TOTP assertion is OutcomeRejected.
func (k *Keys) VerifyTOTP(f StoredTOTPFactor, a Assertion, now time.Time) Result {
	switch f.Counters.LockState(now) {
	case LockOpen:
	case LockHard:
		return Result{Outcome: OutcomeHardLocked}
	default: // LockSoft, LockInvalid
		return Result{Outcome: OutcomeLocked}
	}
	if f.AAD.FactorType != FactorTOTP {
		return Result{Outcome: OutcomeIntegrity}
	}
	secret, err := k.OpenFactorSecret(f.AAD, f.Sealed)
	if err != nil {
		return Result{Outcome: OutcomeIntegrity}
	}
	if a.kind != AssertionTOTP {
		return Result{Outcome: OutcomeRejected}
	}
	step, err := totp.Verify(secret.b, a.value, now, f.LastUsedStep)
	if err != nil {
		if errors.Is(err, totp.ErrInvalidSecret) {
			return Result{Outcome: OutcomeIntegrity}
		}
		return Result{Outcome: OutcomeRejected}
	}
	return Result{Outcome: OutcomeAccepted, AcceptedStep: step, AMR: AMROTP}
}
