package rg

// ADR 0115 SLICE S2: the closed refusal vocabulary of the RG limit write APIs and the mapping of the
// migration-0135 SQLSTATEs onto it. The HTTP layer maps a RefusalCode onto an apierror code; nothing
// here carries database error text to a caller.

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// RefusalCode is the closed set of reasons an RG write is refused.
type RefusalCode string

const (
	RefusedInvalid             RefusalCode = "invalid" // structurally invalid input (Detail is a fixed sentence)
	RefusedNotFound            RefusalCode = "not_found"
	RefusedForbidden           RefusalCode = "forbidden"
	RefusedAboveCeiling        RefusalCode = "above_ceiling"
	RefusedPeriodNotAllowed    RefusalCode = "period_not_allowed"
	RefusedDurationNotAllowed  RefusalCode = "duration_not_allowed"
	RefusedNotOffered          RefusalCode = "not_offered"
	RefusedBackdated           RefusalCode = "backdated"
	RefusedSyntheticRefused    RefusalCode = "synthetic_refused"
	RefusedNoChange            RefusalCode = "no_change"
	RefusedChangeNotPending    RefusalCode = "change_not_pending"
	RefusedCoolingOff          RefusalCode = "cooling_off"
	RefusedConfigMissing       RefusalCode = "config_missing"
	RefusedPendingCap          RefusalCode = "pending_cap"
	RefusedIdempotencyMismatch RefusalCode = "idempotency_mismatch"
	RefusedOperatorLoosens     RefusalCode = "operator_loosens"
	RefusedNoFloor             RefusalCode = "no_floor"
	RefusedSelfApproval        RefusalCode = "self_approval"
	RefusedRequestNotPending   RefusalCode = "request_not_pending"
	RefusedGuard               RefusalCode = "guard_refused"
	RefusedStepUpRequired      RefusalCode = "step_up_required"
	RefusedSelfTarget          RefusalCode = "self_target"
	RefusedIdentityReview      RefusalCode = "identity_review"
	RefusedProof               RefusalCode = "proof_refused"
	RefusedUnavailable         RefusalCode = "unavailable"
)

// Refusal is a deliberate, classified refusal. Detail is a fixed, caller-safe sentence or a closed
// token (for example the loosened field name); it never contains database error text.
type Refusal struct {
	Code   RefusalCode
	Detail string
	// ConfigVersionIDs are the configuration rows in force when the refusal was decided (audit replay).
	ConfigVersionIDs []string
}

func (e *Refusal) Error() string {
	if e.Detail == "" {
		return "rg: refused: " + string(e.Code)
	}
	return fmt.Sprintf("rg: refused: %s: %s", e.Code, e.Detail)
}

// IsRefusal returns the Refusal inside err.
func IsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

func refuse(code RefusalCode, detail string) error { return &Refusal{Code: code, Detail: detail} }

func invalidf(format string, a ...any) error {
	return &Refusal{Code: RefusedInvalid, Detail: fmt.Sprintf(format, a...)}
}

var loosenedField = regexp.MustCompile(`field=([a-z_]+)`)

// MapDBError classifies a database error of an RG write into a Refusal. An error it does not
// recognise is returned unchanged (the caller treats it as internal and reveals nothing).
func MapDBError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := IsRefusal(err); ok {
		return err
	}
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return err
	}
	switch {
	case strings.HasPrefix(pg.Code, "AP"):
		return refuse(RefusedProof, pg.Code)
	}
	switch pg.Code {
	case SQLStateRGBackdated:
		return refuse(RefusedBackdated, "")
	case SQLStateRGOperatorLoosens:
		field := ""
		if m := loosenedField.FindStringSubmatch(pg.Detail); m != nil {
			field = m[1]
		}
		return refuse(RefusedOperatorLoosens, field)
	case SQLStateRGNoFloorInForce:
		return refuse(RefusedNoFloor, "")
	case SQLStateRGFloorNoApproval:
		return refuse(RefusedGuard, "floor_without_approved_request")
	case SQLStateRGFourEyesBinding:
		return refuse(RefusedSelfApproval, "")
	case SQLStateRGSyntheticRefused:
		return refuse(RefusedSyntheticRefused, "")
	case SQLStateRGAppendOnly, "RL026":
		return refuse(RefusedGuard, pg.Code)
	case SQLStateRGRequestTransition:
		return refuse(RefusedRequestNotPending, "")
	case SQLStateRGCalendarTZ:
		return refuse(RefusedInvalid, "calendar_tz is not a known time zone")
	case SQLStateRGClientAssertedTime:
		return refuse(RefusedGuard, pg.Code)
	case SQLStateRGNoChange:
		return refuse(RefusedNoChange, "")
	case SQLStateRGSupersedeInvalid:
		if strings.Contains(pg.Message, "cooling-off") {
			return refuse(RefusedCoolingOff, "")
		}
		return refuse(RefusedChangeNotPending, "")
	case SQLStateRGConfigMissing:
		return refuse(RefusedConfigMissing, "")
	case SQLStateRGIsolation:
		return refuse(RefusedUnavailable, "isolation")
	case SQLStateRGTenantActor:
		if strings.Contains(pg.Message, "own Person") {
			return refuse(RefusedSelfTarget, "")
		}
		return refuse(RefusedForbidden, "")
	case SQLStateRGActorSession:
		return refuse(RefusedForbidden, "")
	case "55P03", "40P01": // lock timeout / deadlock: retryable
		return refuse(RefusedUnavailable, "busy")
	case "23514": // CHECK violation: the configuration / limit shape is invalid
		return refuse(RefusedInvalid, "the request violates a configuration shape rule")
	case "23503": // FK: unknown asset / brand / jurisdiction
		return refuse(RefusedInvalid, "a referenced asset, brand or jurisdiction does not exist")
	case "22P02", "22003", "22007", "22008", "22015", "22021", "22P05": // malformed number / interval / timestamp
		return refuse(RefusedInvalid, "a value is malformed")
	}
	return err
}
