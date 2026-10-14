package httpserver

// ADR 0119 slice S3: the staff MFA subsystem as the HTTP layer sees it, the
// shared transaction runner of the second-factor endpoints, and the
// post-commit alert raising.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/Diansalas/igaming-platform/internal/alerting"
	"github.com/Diansalas/igaming-platform/internal/apierror"
	"github.com/Diansalas/igaming-platform/internal/auth"
	"github.com/Diansalas/igaming-platform/internal/db"
	"github.com/Diansalas/igaming-platform/internal/identity"
	"github.com/Diansalas/igaming-platform/internal/staffmfa"
	"github.com/Diansalas/igaming-platform/internal/staffmfa/store"
)

// StaffMFA is the ADR 0119 staff MFA subsystem: the key families (built by
// staffmfa.LoadKeys, so the startup gate has run), the store-backed TOTP
// factor, the challenge / token / recovery-code store and the staff session
// lifetimes (section 7.4, bounded by config.Load). There is no enforcement
// flag: a nil *StaffMFA in Deps makes every staff login, second-factor,
// enrolment and staff refresh request answer 503.
type StaffMFA struct {
	keys        *staffmfa.Keys
	factor      *store.TOTPFactor
	store       *store.Store
	accessTTL   time.Duration
	idleTTL     time.Duration
	absoluteTTL time.Duration
	// stepUp is the step-up class max-age policy (ADR 0119 S4b, section 7.4):
	// the defaults unless SetStepUpPolicy tightened them.
	stepUp auth.StepUpPolicy

	// commitFault, when set (tests in this package only), runs at the very end
	// of every second-factor transaction closure, after every write and before
	// COMMIT; its error aborts the transaction. It stands in for a
	// serialization failure (40001) or a failed COMMIT (S3 entry gate G4).
	commitFault func(ctx context.Context, tx pgx.Tx) error
}

// NewStaffMFA builds the subsystem. keys must come from staffmfa.LoadKeys;
// the durations are refused outside their ADR 0119 section 7.4 bounds.
func NewStaffMFA(keys *staffmfa.Keys, issuer string, accessTTL, idleTTL, absoluteTTL time.Duration) (*StaffMFA, error) {
	if keys == nil {
		return nil, fmt.Errorf("httpserver: staff MFA keys are required")
	}
	for _, c := range []struct {
		d time.Duration
		b staffmfa.Bound
	}{{accessTTL, staffmfa.StaffAccessTokenTTLBound}, {idleTTL, staffmfa.StaffRefreshIdleTTLBound}, {absoluteTTL, staffmfa.StaffSessionAbsoluteTTLBound}} {
		if err := c.b.Check(c.d); err != nil {
			return nil, err
		}
	}
	factor, err := store.NewTOTPFactor(keys, issuer)
	if err != nil {
		return nil, err
	}
	st, err := store.New(keys)
	if err != nil {
		return nil, err
	}
	return &StaffMFA{keys: keys, factor: factor, store: st, accessTTL: accessTTL, idleTTL: idleTTL, absoluteTTL: absoluteTTL,
		stepUp: auth.DefaultStepUpPolicy()}, nil
}

// SetStepUpPolicy installs the configured step-up max-ages
// (STAFF_STEP_UP_CRITICAL_MAX_AGE_SECONDS / _FINANCIAL_, already bounded by
// config.Load). A value outside the section 7.4 bounds is refused.
func (m *StaffMFA) SetStepUpPolicy(critical, financial time.Duration) error {
	p := auth.StepUpPolicy{Critical: critical, Financial: financial}
	if !p.Valid() {
		return fmt.Errorf("httpserver: step-up max-ages out of bounds")
	}
	m.stepUp = p
	return nil
}

// sessionPolicy is the staff rotation policy of RotateSessionWithPolicy.
func (m *StaffMFA) sessionPolicy() *auth.StaffSessionPolicy {
	if m == nil {
		return nil
	}
	return &auth.StaffSessionPolicy{Keys: m.keys, IdleTTL: m.idleTTL}
}

// run executes fn in the staff member's own scope (WithTenant / platform),
// then the test-only commit fault. A nil error means COMMIT succeeded.
func (m *StaffMFA) run(ctx context.Context, pool *db.Pool, staff staffmfa.StaffRef, fn db.TxFunc) error {
	return store.WithScope(ctx, pool, staff, func(ctx context.Context, tx pgx.Tx) error {
		if err := fn(ctx, tx); err != nil {
			return err
		}
		if f := m.commitFault; f != nil {
			return f(ctx, tx)
		}
		return nil
	})
}

// dbNow is the transaction's own clock: every staff MFA decision of a request
// uses it, so the application and the SF008 database-clock rule (migration
// 0138) always agree on whether a failure window has closed.
func dbNow(ctx context.Context, tx pgx.Tx) (time.Time, error) {
	var t time.Time
	if err := tx.QueryRow(ctx, `SELECT now()`).Scan(&t); err != nil {
		return time.Time{}, fmt.Errorf("httpserver: read database clock: %w", err)
	}
	return t, nil
}

// liveStaff re-reads the staff member inside the scoped transaction: the row
// must be visible, belong to ref's tenant, be active and still be at the
// staff version the challenge (or token) was issued under (ADR 0119 S5 + S6:
// staff_users.staff_version, migration 0139).
func liveStaff(ctx context.Context, tx pgx.Tx, ref staffmfa.StaffRef, versionAtIssue int64) (identity.StaffUser, bool, error) {
	s, err := identity.GetStaffUserByID(ctx, tx, ref.StaffID)
	if errors.Is(err, identity.ErrNotFound) {
		return identity.StaffUser{}, false, nil
	}
	if err != nil {
		return identity.StaffUser{}, false, err
	}
	if s.TenantID != ref.TenantID || s.Status != identity.StaffStatusActive || versionAtIssue != s.StaffVersion {
		return s, false, nil
	}
	return s, true, nil
}

// mintStaffTokens builds the aal2 token pair of a staff session. staffVersion
// is the staff member's CURRENT staff_users.staff_version, read in the same
// transaction that verified them (the `sv` claim liveness compares).
func (m *StaffMFA) mintStaffTokens(issuer *auth.Issuer, s auth.Session, role auth.Role, staffVersion int64) (staffTokenPair, error) {
	access, err := issuer.IssueStaff(auth.StaffTokenParams{
		StaffID: s.PrincipalID, TenantID: s.TenantID, Role: role, SessionID: s.ID,
		AAL: s.AAL, AMR: s.AMR, AuthTime: s.AuthTime, MFAAt: s.MFAAt, StaffVersion: staffVersion,
	}, m.accessTTL)
	if err != nil {
		return staffTokenPair{}, err
	}
	return staffTokenPair{AccessToken: newRedacted(access), RefreshToken: newRedacted(s.RefreshToken),
		ExpiresIn: int(m.accessTTL.Seconds())}, nil
}

// noStore marks a response as carrying a secret (ADR 0119 section 5.4).
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
}

const staffAuthUnavailableMessage = "authentication is temporarily unavailable; try again"

// writeStaffAuthUnavailable is THE answer to every fault of a second-factor
// request - a database error, a serialization failure, a failed COMMIT - with
// one fixed code and message whatever the verification outcome would have
// been (S3 entry gate G4).
func writeStaffAuthUnavailable(w http.ResponseWriter, requestID string) {
	apierror.Write(w, requestID, apierror.CodeUnavailable, staffAuthUnavailableMessage)
}

// ---- alerts -------------------------------------------------------------------

// staffSignal is one of the five ADR 0119 staff security signals.
type staffSignal int

const (
	signalMFAIntegrity staffSignal = iota + 1
	signalSessionIntegrity
	signalMFAHardLocked
	signalMFARecoveryUsed
	signalMFAFailurePattern
)

// kind returns the Kind of a signal for a tenant or platform staff member.
func (s staffSignal) kind(platform bool) alerting.Kind {
	pick := func(tenant, plat alerting.Kind) alerting.Kind {
		if platform {
			return plat
		}
		return tenant
	}
	switch s {
	case signalMFAIntegrity:
		return pick(alerting.KindStaffMFAIntegrity, alerting.KindStaffPlatformMFAIntegrity)
	case signalSessionIntegrity:
		return pick(alerting.KindStaffSessionIntegrity, alerting.KindStaffPlatformSessionIntegrity)
	case signalMFAHardLocked:
		return pick(alerting.KindStaffMFAHardLocked, alerting.KindStaffPlatformMFAHardLocked)
	case signalMFARecoveryUsed:
		return pick(alerting.KindStaffMFARecoveryUsed, alerting.KindStaffPlatformMFARecoveryUsed)
	case signalMFAFailurePattern:
		return pick(alerting.KindStaffMFAFailurePattern, alerting.KindStaffPlatformMFAFailurePattern)
	}
	return ""
}

// staffAlert is an alert collected inside a business transaction and raised
// only after it committed.
type staffAlert struct {
	signal       staffSignal
	staff        staffmfa.StaffRef
	discriminant string
	attrs        map[string]alerting.AttrValue
}

func newStaffAlert(sig staffSignal, staff staffmfa.StaffRef, requestID string, attrs map[string]alerting.AttrValue) staffAlert {
	a := map[string]alerting.AttrValue{"staff_id": staff.StaffID.String()}
	for k, v := range attrs {
		a[k] = v
	}
	if requestID != "" {
		a["request_id"] = requestID
	}
	return staffAlert{signal: sig, staff: staff, discriminant: "staff:" + staff.StaffID.String(), attrs: a}
}

// raiseStaffAlerts raises each alert post-commit, in the staff member's own
// scope: a tenant staff member's alert from its tenant session with that
// tenant as subject (the tenant can read it); a platform staff member's alert
// from its own platform session, without a subject. A raise failure is
// already logged, counted and turned into alerting.raise_failed by the
// alerting package; it never changes the response.
func raiseStaffAlerts(ctx context.Context, pool *db.Pool, alerts []staffAlert) {
	for _, a := range alerts {
		platform := a.staff.TenantID == uuid.Nil
		var runner alerting.ScopedRunner
		al := alerting.Alert{Kind: a.signal.kind(platform), Discriminator: a.discriminant, Attributes: a.attrs}
		if platform {
			runner = alerting.NewPlatformAdminRunner(pool, a.staff.StaffID)
		} else {
			runner = alerting.NewTenantRunner(pool, a.staff.TenantID)
			al.SubjectTenantID = a.staff.TenantID
		}
		_ = alerting.RaisePostCommit(ctx, runner, al)
	}
}

// failurePatternDays is S3 entry gate (e): second-factor failures after a
// correct password on this many distinct (UTC) days within failurePatternWindow
// raise staff.mfa_failure_pattern.
const (
	failurePatternDays   = 3
	failurePatternWindow = "7 days"
)

// failurePatternAlert counts the distinct days, within the window, on which the
// staff member's second-factor failures were audited (the current failure's
// row included; the caller has just written it) and returns the alert when the
// threshold is met.
func failurePatternAlert(ctx context.Context, tx pgx.Tx, staff staffmfa.StaffRef, requestID string) (*staffAlert, error) {
	var days int
	if err := tx.QueryRow(ctx, `SELECT count(DISTINCT date_trunc('day', created_at AT TIME ZONE 'UTC'))
		FROM audit_log
		WHERE actor_type = 'staff' AND actor_id = $1 AND action IN ('staff.mfa_verification_failed', 'staff.step_up_failed')
		  AND created_at > now() - $2::interval`, staff.StaffID, failurePatternWindow).Scan(&days); err != nil {
		return nil, fmt.Errorf("httpserver: count second-factor failure days: %w", err)
	}
	if days < failurePatternDays {
		return nil, nil
	}
	a := newStaffAlert(signalMFAFailurePattern, staff, requestID, map[string]alerting.AttrValue{"distinct_days": days})
	return &a, nil
}
