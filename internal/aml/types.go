// Package aml is the AML internal foundation (ADR 0116, slices S1 and S3): AML holds and their enforcement boundary,
// the officer-only case model, proof-bound four-eyes and the note lint.
//
// LABELS (docs/decisions/0116-aml-internal-foundation.md, implementation record): holds, enforcement, the case model,
// four-eyes, notes, the aml_officer grant path and the quorum are IMPLEMENTED for slices S1 and S3. Screening (S2),
// transaction monitoring (S4), SAR export (S5), Back Office screens (S6) and readiness / launch criteria (S7) are NOT
// IMPLEMENTED. There is NO AML vendor, NO real screening and NO legal value in this package: every threshold, retention
// period and rule is configuration that a human compliance decision owns.
//
// Two trust boundaries live here.
//
//   - The ENFORCEMENT READ (EvaluateHold): called by the money-boundary gates inside the caller's transaction, with no
//     proof, and fail-closed. It can only DENY.
//   - The OFFICER API (Service): every call reaches an OWNER-owned SECURITY DEFINER function with a freshly signed actor
//     proof (internal/actorproof, ADR 0110). The database re-checks the actor's aml_officer grant itself; no GUC grants
//     anything by itself (ADR 0116 s3.1 B1).
package aml

import (
	"errors"
	"fmt"
	"regexp"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// Operation is a gated operation as the hold evaluator names it (migration 0141 aml_valid_operation).
type Operation string

const (
	OpDeposit          Operation = "deposit"
	OpWithdrawal       Operation = "withdrawal"
	OpPayout           Operation = "payout"
	OpCasinoStake      Operation = "casino_stake"
	OpSportsbookStake  Operation = "sportsbook_stake"
	OpManualAdjustment Operation = "manual_adjustment"
)

// Valid reports whether o is one of the closed operation names.
func (o Operation) Valid() bool {
	switch o {
	case OpDeposit, OpWithdrawal, OpPayout, OpCasinoStake, OpSportsbookStake, OpManualAdjustment:
		return true
	}
	return false
}

// HoldScope is what a hold gates (ADR 0116 s4.3).
type HoldScope string

const (
	// ScopeAllValueMovement gates every value-in / value-out operation (confirmed sanctions).
	ScopeAllValueMovement HoldScope = "all_value_movement"
	// ScopeWithdrawalAndPayout gates withdrawal and payout only (the PEP default, Q7).
	ScopeWithdrawalAndPayout HoldScope = "withdrawal_and_payout"
)

// ReasonClass is why a hold exists. S1 ships the officer-placed classes; S2 (screening) is NOT IMPLEMENTED.
type ReasonClass string

const (
	ReasonManual             ReasonClass = "manual"
	ReasonConfirmedSanctions ReasonClass = "confirmed_sanctions"
)

// Scopes of the acting officer, as the actor proof and the database name them.
const (
	ScopeTenant         = "tenant"
	ScopePlatformActing = "platform_acting"
)

var reasonCodeRE = regexp.MustCompile(`^[a-z0-9_]{1,64}$`)

// ValidReasonCode reports whether s is an admissible reason code (a short machine token, never free text).
func ValidReasonCode(s string) bool { return reasonCodeRE.MatchString(s) }

// Sentinel errors. Every database refusal of class AM / AP is mapped to one of these (or a *GuardError).
var (
	// ErrNotFound: the row does not exist OR is outside the actor's scope (indistinguishable by design, ADR 0096 s2.6(f)).
	ErrNotFound = errors.New("aml: not found")
	// ErrForbidden: the actor does not hold an in-force aml_officer grant, or may not reach this tenant / owner scope.
	ErrForbidden = errors.New("aml: forbidden")
	// ErrQuorum: fewer than two distinct aml_officer Persons for the owner scope; nothing is activated.
	ErrQuorum = errors.New("aml: officer quorum not met")
	// ErrInvalidInput: a malformed or out-of-vocabulary argument.
	ErrInvalidInput = errors.New("aml: invalid input")
	// ErrProofUnavailable: no signing issuer is configured. Governed writes fail closed.
	ErrProofUnavailable = errors.New("aml: no actor-proof issuer configured")
	// ErrHoldRefused: the lift or placement is refused by a hold rule (e.g. a sanctions hold needs four-eyes).
	ErrHoldRefused = errors.New("aml: hold operation refused")
	// ErrFourEyes: the proof-bound four-eyes binding was refused (same actor, same person, subject person).
	ErrFourEyes = errors.New("aml: four-eyes binding refused")
	// ErrStale: the proposal / expected sequence is no longer current (any later event invalidates a pending proposal).
	ErrStale = errors.New("aml: stale proposal or sequence")
	// ErrIllegalTransition: the case state machine refuses the move.
	ErrIllegalTransition = errors.New("aml: illegal case transition")
	// ErrNoteRefused: the note failed the server-side lint or the database refused it.
	ErrNoteRefused = errors.New("aml: note refused")
	// ErrActorScope: the Actor value is malformed (nil ids, unknown scope).
	ErrActorScope = errors.New("aml: invalid actor")
)

// GuardError is a database refusal (SQLSTATE class AP or AM) with no more specific sentinel. Callers branch on Code.
type GuardError struct {
	Code    string
	Message string
}

func (e *GuardError) Error() string {
	return fmt.Sprintf("aml: database guard %s: %s", e.Code, e.Message)
}

// Actor is the authenticated officer, resolved by the caller from the verified token (never from a request body).
// Scope ScopeTenant: TenantID is the actor's own tenant. Scope ScopePlatformActing: TenantID is the route-validated
// target tenant (the db acting session re-validates the platform_admin + grant, migration 0112).
type Actor struct {
	StaffID   uuid.UUID
	Scope     string
	TenantID  uuid.UUID
	IPAddress string
	RequestID string
}

func (a Actor) validate() error {
	if a.StaffID == uuid.Nil || a.TenantID == uuid.Nil {
		return fmt.Errorf("%w: staff id and tenant id are required", ErrActorScope)
	}
	if a.Scope != ScopeTenant && a.Scope != ScopePlatformActing {
		return fmt.Errorf("%w: scope %q", ErrActorScope, a.Scope)
	}
	return nil
}

// classify maps a database error to a service error.
func classify(err error) error {
	if err == nil {
		return nil
	}
	var pe *pgconn.PgError
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case "AM040":
		return ErrNotFound
	case "AM001", "AM002", "AM020", "AM026":
		return fmt.Errorf("%w: %s", ErrForbidden, pe.Message)
	case "AM003":
		return ErrQuorum
	case "AM004":
		return fmt.Errorf("%w: %s", ErrInvalidInput, pe.Message)
	case "AM006", "AM007":
		return fmt.Errorf("%w: %s", ErrHoldRefused, pe.Message)
	case "AM010":
		return fmt.Errorf("%w: %s", ErrIllegalTransition, pe.Message)
	case "AM011":
		return fmt.Errorf("%w: %s", ErrFourEyes, pe.Message)
	case "AM012":
		return fmt.Errorf("%w: %s", ErrStale, pe.Message)
	case "AM013":
		return fmt.Errorf("%w: %s", ErrNoteRefused, pe.Message)
	}
	if len(pe.Code) == 5 && (pe.Code[:2] == "AM" || pe.Code[:2] == "AP") {
		return &GuardError{Code: pe.Code, Message: pe.Message}
	}
	return err
}

// GuardCode returns the SQLSTATE of a *GuardError, or "".
func GuardCode(err error) string {
	var ge *GuardError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ""
}
