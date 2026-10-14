package auth

// ADR 0119 slice S4b (sections 6.6, 7.3, 9.1, 9.3): step-up.
//
//   - StepUpPolicy: the configured max-age per class (critical 5 min, financial
//     10 min by default; config.Load only tightens them).
//   - StepUpFresh: the one freshness rule (age <= max-age, a future mfa_at
//     beyond StepUpClockSkew is refused, a zero mfa_at is never fresh).
//   - StepUpAssertion: what the HTTP step-up gate places in the request context
//     after it enforced a route's class. The actor-proof signer backstop
//     (internal/actorproof) and the withdrawal policy (internal/withdrawal)
//     consume it. Its MFAAt is the OLDER of the token's mfa_at claim and the
//     sealed session row's mfa_at (conditions S3-R13 / S4a-R10: the sealed row is
//     authoritative; an access token minted before the latest step-up does not
//     inherit it).
//   - The step-up of a session row (StepUpStaffSession): chain head only, a new
//     mfa_at from the database clock and a re-seal (seal format v2).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/stepup"
)

// StepUpClockSkew is the tolerated amount by which an mfa_at may lie in the
// future (database clock vs application clock), section 7.3.
const StepUpClockSkew = stepup.ClockSkew

// StepUpPolicyMaxAge is the withdrawal policy's require_step_up freshness
// (ADR 0119 section 9.3, [ORCH-DEFAULT]): stricter than the route's financial
// window, for large multi-approval withdrawals.
const StepUpPolicyMaxAge = stepup.PolicyMaxAge

// SecondFactorAMR is the closed set of amr values that name a second factor
// (RFC 8176: otp is TOTP). A staff token or session whose amr names none of
// them is refused (condition S3-R13). WebAuthn (hwk / swk) joins with its ADR.
var SecondFactorAMR = map[string]bool{staffmfa.AMROTP: true}

// HasSecondFactorAMR reports whether amr names a second-factor method.
func HasSecondFactorAMR(amr []string) bool {
	for _, v := range amr {
		if SecondFactorAMR[v] {
			return true
		}
	}
	return false
}

// StepUpPolicy carries the configured class max-ages.
type StepUpPolicy struct {
	Critical  time.Duration
	Financial time.Duration
}

// DefaultStepUpPolicy is the section 7.4 default.
func DefaultStepUpPolicy() StepUpPolicy {
	return StepUpPolicy{Critical: staffmfa.StepUpCriticalMaxAgeBound.Default, Financial: staffmfa.StepUpFinancialMaxAgeBound.Default}
}

// Valid reports whether both max-ages lie inside their section 7.4 bounds.
func (p StepUpPolicy) Valid() bool {
	return staffmfa.StepUpCriticalMaxAgeBound.Check(p.Critical) == nil && staffmfa.StepUpFinancialMaxAgeBound.Check(p.Financial) == nil
}

// MaxAge returns the max-age of class; ok is false for StepUpNone and for any
// value outside the closed set (the caller then treats the route as none only
// if the class is exactly StepUpNone).
func (p StepUpPolicy) MaxAge(c StepUpClass) (time.Duration, bool) {
	switch c {
	case StepUpCritical:
		return p.Critical, true
	case StepUpFinancial:
		return p.Financial, true
	}
	return 0, false
}

// StepUpFresh is the single freshness rule (stepup.Fresh): mfaAt is set, not
// more than StepUpClockSkew in the future, and now - mfaAt <= maxAge.
func StepUpFresh(mfaAt, now time.Time, maxAge time.Duration) bool {
	return stepup.Fresh(mfaAt, now, maxAge)
}

// StepUpAssertion records that the step-up gate verified a fresh second factor
// for Subject's session SessionID at MFAAt, for a route of class Class
// (stepup.Assertion; Present and FreshFor are its methods).
type StepUpAssertion = stepup.Assertion

// ContextWithStepUpAssertion attaches a to ctx. Production code calls it only
// from the HTTP step-up gate (internal/httpserver); integration-test support
// (internal/actorproof/prooftest) places a synthetic one through
// stepup.ContextWith. A static test pins the callers.
func ContextWithStepUpAssertion(ctx context.Context, a StepUpAssertion) context.Context {
	return stepup.ContextWith(ctx, a)
}

// StepUpAssertionFrom returns the request's step-up assertion, if the step-up
// gate placed one.
func StepUpAssertionFrom(ctx context.Context) (StepUpAssertion, bool) { return stepup.From(ctx) }

// WriteStepUpRequired is the single step-up refusal: 403 step_up_required with
// max_age (seconds) and the RFC 9470 section 3 challenge.
func WriteStepUpRequired(w http.ResponseWriter, requestID string, maxAge time.Duration) {
	secs := int(maxAge / time.Second)
	w.Header().Set("WWW-Authenticate", `Bearer realm="platform-api", error="insufficient_user_authentication", `+
		`error_description="a fresh second-factor verification is required", max_age="`+strconv.Itoa(secs)+`"`)
	apierror.WriteStepUpRequired(w, requestID, secs)
}

// ---- the step-up of a session row (section 6.6) ------------------------------

var (
	// ErrStepUpNotChainHead: the session row is revoked, replaced (not the
	// newest link of its rotation chain), past its absolute expiry, not found in
	// the caller's scope, or does not belong to the caller. The HTTP layer
	// answers 401 session_revoked.
	ErrStepUpNotChainHead = errors.New("auth: step-up: the session is not the live head of its chain")
	// ErrStepUpSealInvalid: the row's assurance seal does not verify (tampering)
	// or is a legacy (pre-S4b) seal. 401 session_revoked; the caller audits.
	ErrStepUpSealInvalid = errors.New("auth: step-up: the session assurance seal does not verify")
)

// StepUpResult is the re-sealed row's assurance after a step-up.
type StepUpResult struct {
	SessionID uuid.UUID
	AAL       int
	AMR       []string
	AuthTime  time.Time
	MFAAt     time.Time
	Legacy    bool // set with ErrStepUpSealInvalid when the seal was a genuine pre-S4b (v1) seal
}

// StepUpStaffSession moves mfa_at of the caller's session row to now and
// re-seals it, inside tx (already scoped to the staff member's tenant or
// platform scope, after the second factor was verified in the same
// transaction). now must be the transaction's database clock. It writes ONLY
// a row that is the live chain head (revoked_at IS NULL, replaced_by_session_id
// IS NULL, absolute_expires_at > now) whose v2 seal verifies; the row is
// locked FOR UPDATE first, so a concurrent revocation either commits before
// (and the step-up is refused) or waits for this transaction. auth_time,
// aal, amr and absolute_expires_at are untouched (SF040).
func StepUpStaffSession(ctx context.Context, tx pgx.Tx, keys *staffmfa.Keys, sessionID, staffID, tenantID uuid.UUID, now time.Time) (StepUpResult, error) {
	if keys == nil {
		return StepUpResult{}, ErrStaffSessionUnavailable
	}
	if sessionID == uuid.Nil || staffID == uuid.Nil || now.IsZero() {
		return StepUpResult{}, fmt.Errorf("auth: step-up: incomplete parameters")
	}
	if err := db.SetSessionInternalOpID(ctx, tx, sessionID); err != nil {
		return StepUpResult{}, err
	}
	var tenantArg any
	if tenantID != uuid.Nil {
		tenantArg = tenantID
	}
	var (
		revoked, replaced              bool
		aal                            *int16
		amr                            []string
		authTime, mfaAt, absolute, exp *time.Time
		kid                            *string
		seal                           []byte
		hash                           string
	)
	err := tx.QueryRow(ctx, `
		SELECT revoked_at IS NOT NULL, replaced_by_session_id IS NOT NULL, aal, amr, auth_time, mfa_at, absolute_expires_at,
		       expires_at, assurance_kid, assurance_seal, refresh_token_hash
		  FROM sessions
		 WHERE id = $1 AND principal_id = $2 AND principal_type = 'staff' AND tenant_id IS NOT DISTINCT FROM $3::uuid
		 FOR UPDATE`, sessionID, staffID, tenantArg,
	).Scan(&revoked, &replaced, &aal, &amr, &authTime, &mfaAt, &absolute, &exp, &kid, &seal, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return StepUpResult{}, ErrStepUpNotChainHead
	}
	if err != nil {
		return StepUpResult{}, fmt.Errorf("auth: step-up: lock session: %w", err)
	}
	if revoked || replaced || absolute == nil || !now.Before(*absolute) {
		return StepUpResult{}, ErrStepUpNotChainHead
	}
	if aal == nil || amr == nil || authTime == nil || mfaAt == nil || exp == nil || kid == nil || seal == nil {
		return StepUpResult{}, ErrStepUpSealInvalid
	}
	cur := staffmfa.SessionAssurance{
		SessionID: sessionID, PrincipalType: string(PrincipalStaff), PrincipalID: staffID, TenantID: tenantID,
		AAL: int(*aal), AMR: amr, AuthTime: *authTime, MFAAt: *mfaAt, AbsoluteExpiresAt: *absolute,
		RefreshTokenHash: hash, ExpiresAt: *exp,
	}
	stored := staffmfa.KeyedHash{KID: *kid, MAC: seal}
	if !keys.VerifySessionSeal(cur, stored) {
		return StepUpResult{Legacy: keys.IsLegacySessionSeal(cur, stored)}, ErrStepUpSealInvalid
	}
	if cur.AAL != StaffAAL || !HasSecondFactorAMR(cur.AMR) {
		return StepUpResult{}, ErrStepUpSealInvalid
	}
	next := cur
	next.MFAAt = staffmfa.SessionTime(now)
	if next.MFAAt.Before(cur.MFAAt) {
		// SF040 refuses a backwards mfa_at; keep the newer value (a database clock never runs backwards within a
		// session's lifetime, so this is a defence only).
		next.MFAAt = cur.MFAAt
	}
	newSeal, err := keys.SealSession(next)
	if err != nil {
		return StepUpResult{}, fmt.Errorf("auth: step-up: seal: %w", err)
	}
	tag, err := tx.Exec(ctx, `
		UPDATE sessions SET mfa_at = $2, assurance_kid = $3, assurance_seal = $4
		 WHERE id = $1 AND revoked_at IS NULL AND replaced_by_session_id IS NULL`,
		sessionID, next.MFAAt, newSeal.KID, newSeal.MAC)
	if err != nil {
		return StepUpResult{}, fmt.Errorf("auth: step-up: update session: %w", err)
	}
	if tag.RowsAffected() != 1 {
		// Unreachable while the FOR UPDATE lock is held; fail closed.
		return StepUpResult{}, ErrStepUpNotChainHead
	}
	return StepUpResult{SessionID: sessionID, AAL: next.AAL, AMR: append([]string(nil), next.AMR...), AuthTime: next.AuthTime, MFAAt: next.MFAAt}, nil
}
