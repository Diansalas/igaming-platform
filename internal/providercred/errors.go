package providercred

import (
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
)

// Kind is the API-facing classification of a lifecycle failure (security
// review §6 error mapping). The HTTP layer maps a Kind to exactly one
// response; the specific Class goes to the application log only.
type Kind int

const (
	// KindInvalid: a syntactic request error (400 invalid_request).
	KindInvalid Kind = iota + 1
	// KindNotFound: the handle or request is not visible in scope (404).
	KindNotFound
	// KindRegistrationRejected: every semantic registration failure (one
	// byte-identical 409 credential_registration_rejected).
	KindRegistrationRejected
	// KindApprovalRejected: a decision refused (409 approval_rejected).
	KindApprovalRejected
	// KindActivationRejected: an apply refused (409
	// credential_activation_rejected).
	KindActivationRejected
	// KindTransitionRejected: a transition refused (409
	// credential_transition_rejected).
	KindTransitionRejected
)

func (k Kind) String() string {
	switch k {
	case KindInvalid:
		return "invalid_request"
	case KindNotFound:
		return "not_found"
	case KindRegistrationRejected:
		return "credential_registration_rejected"
	case KindApprovalRejected:
		return "approval_rejected"
	case KindActivationRejected:
		return "credential_activation_rejected"
	case KindTransitionRejected:
		return "credential_transition_rejected"
	default:
		return "unknown"
	}
}

// Error is a classified lifecycle failure. Class is a closed, loggable
// token (e.g. "confirmation_mismatch", "PC006") - never trigger text,
// store text, a secret or a confirmation value.
type Error struct {
	Kind  Kind
	Class string
}

func (e *Error) Error() string { return fmt.Sprintf("providercred: %s (%s)", e.Kind, e.Class) }

func newErr(k Kind, class string) error { return &Error{Kind: k, Class: class} }

// KindOf returns err's Kind, or 0 for an unclassified (internal) error.
func KindOf(err error) Kind {
	var e *Error
	if errors.As(err, &e) {
		return e.Kind
	}
	return 0
}

// ClassOf returns err's loggable class ("internal" when unclassified).
func ClassOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.Class
	}
	return "internal"
}

// dbClass classifies a database error by SQLSTATE only (review §6: never by
// message text). ok is false for an error that is not a *pgconn.PgError or
// whose code is not one this subsystem expects - an internal error (500).
func dbClass(err error) (class string, ok bool) {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return "", false
	}
	switch {
	case len(pg.Code) == 5 && pg.Code[:2] == "PC":
		return pg.Code, true
	case pg.Code == "23505": // unique_violation
		return "unique_violation", true
	case pg.Code == "23514": // check_violation
		return "check_violation", true
	case pg.Code == "23503": // foreign_key_violation
		return "foreign_key_violation", true
	case pg.Code == "42501": // insufficient_privilege / RLS WITH CHECK
		return "row_security", true
	case pg.Code == "P0001": // ledger_deny_mutation
		return "immutable", true
	default:
		return "", false
	}
}

// classify wraps a DB error as kind when its SQLSTATE is expected, or
// returns it unchanged (internal) otherwise.
func classify(err error, kind Kind) error {
	if err == nil {
		return nil
	}
	if KindOf(err) != 0 {
		return err
	}
	if class, ok := dbClass(err); ok {
		return newErr(kind, class)
	}
	return err
}
