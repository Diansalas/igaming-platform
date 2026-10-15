package alerting

// NOTE: this file deliberately does NOT reference db.ServiceAlertDispatcher (that identity is confined to
// dispatcher.go and fallback.go by static_dispatcher_identity_test.go). The raise itself is
// raiseSweepFailedAs in fallback.go; this file holds the vocabulary and the validated entry point.

import (
	"context"
	"regexp"

	"github.com/Diansalas/igaming-platform/internal/db"
)

// Sweep-failure phases (the closed attribute vocabulary of KindReconciliationSweepFailed).
const (
	SweepPhaseListActive    = "list_active"
	SweepPhaseListNonActive = "list_non_active"
	SweepPhasePanic         = "panic"
	SweepPhaseUnknown       = "unknown" // a sweep error that is none of the above (cannot happen today; kept visible, never dropped)
)

var sweepFailedPhases = map[string]bool{SweepPhaseListActive: true, SweepPhaseListNonActive: true, SweepPhasePanic: true, SweepPhaseUnknown: true}

var sqlstateClassShape = regexp.MustCompile(`^[0-9A-Z]{2}$`)

// RaiseSweepFailed raises KindReconciliationSweepFailed (R3-LISTING-FAILURE-LOG-ONLY-1): the reconciliation
// scheduler could not list tenants, or a sweep panicked. The scheduler has no tenant and no platform-admin
// principal, so this is a platform, subjectless P1 raised under the alert_dispatcher platform-service identity
// (the 0156 dispatcher raise policies admit exactly the meta Kinds and this one).
//
// phase must be one of the SweepPhase* constants and sqlstateClass a 2-character SQLSTATE class; anything
// else is refused in Go before any SQL (and counted), never silently coerced. The discriminator is
// "phase:<phase>", so repeated failures of the same phase attach occurrences to ONE open alert instead of
// raising one alert per sweep. It makes the same bounded detached retry as RaiseDetached, then falls back to
// alerting.raise_failed; it returns the last error but a caller must treat it as log-and-continue (the
// scheduler loop never stops because an alert could not be written). The context is detached from caller
// cancellation, exactly like RaiseDetached, so a shutdown signal cannot skip the write.
func RaiseSweepFailed(ctx context.Context, pool *db.Pool, phase, sqlstateClass string) error {
	if !sweepFailedPhases[phase] || !sqlstateClassShape.MatchString(sqlstateClass) {
		dctx, cancel := detachedCtx(ctx)
		defer cancel()
		recordRaiseFailure(dctx, KindReconciliationSweepFailed, "detached")
		return &invalidAlertError{kind: KindReconciliationSweepFailed, msg: "phase or sqlstate_class outside the closed vocabulary"}
	}
	return raiseSweepFailedAs(ctx, pool, phase, sqlstateClass)
}
