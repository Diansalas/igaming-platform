package aml

import "errors"

// lic29: closed, stable, machine-readable refusal codes for the generic 400 / 409 answers of the screening routes. The database
// raises them as the HINT of the refusing statement (migration 0181); the SQLSTATE and so the HTTP status of every refusal are
// unchanged. A hint outside this table is ignored (never copied into a response). Wording belongs to the Back Office, which maps the
// code to its text; the description here is for developers and the OpenAPI document.
var RefusalCodes = map[string]string{
	"platform_officer_required":          "screening configuration is authored by a platform officer",
	"effective_from_out_of_range":        "a trigger policy takes effect between now (2 minutes tolerance) and 30 days ahead",
	"gate_needs_scheduled_event":         "a withdrawal gate needs the scheduled re-screen event",
	"gate_needs_alert_threshold":         "a withdrawal gate needs gate_alert_after_seconds",
	"screening_worker_heartbeat_missing": "no screening worker has reported in, so a withdrawal gate cannot be enabled",
	"gate_environment_refused":           "a withdrawal gate cannot be enabled where the environment anchor is production or unrecorded",
	"alert_threshold_without_gate":       "gate_alert_after_seconds belongs to a gating policy only",
	"jurisdiction_not_in_scope":          "the jurisdiction is not that of the tenant's licence or of an approved operating basis",
	"screening_policy_missing":           "set the screening policy for this jurisdiction, in effect at or before the trigger policy, first",
	"tenant_not_in_jurisdiction":         "the tenant is not configured to operate in this jurisdiction",
	"sanctions_clear_refused":            "a sanctions-class match is resolved only by recording a sanctions match, never by a clear",
	"sanctions_match_needs_hold":         "a sanctions-class match is resolved only by a sanctions match that places a hold",
	"error_cannot_resolve":               "an error result cannot resolve a match awaiting an officer",
	"request_key_mismatch":               "the request belongs to another person or trigger",
	"request_not_awaiting_officer":       "the request is not awaiting an officer",
	"key_prefix_reserved":                "the idempotency key prefix is reserved for the automatic screening queue",
	"request_not_replaceable":            "only a pending or claimed request is replaced; a match or a terminal failure is resolved",
	"request_lease_live":                 "the request is being processed: its lease has not expired",
	"person_has_pending_request":         "the person has a pending request: name it to replace it",
	"no_gating_policy":                   "no trigger policy gates withdrawals for this tenant, so there is nothing to queue",
	// screening policy (lic30)
	"policy_effective_from_out_of_range":      "a screening policy takes effect between now (2 minutes tolerance) and 30 days ahead",
	"rescreen_interval_below_floor":           "the re-screen interval is below the engineering safety floor of 3600 seconds",
	"rescreen_interval_below_alert_threshold": "the re-screen interval is below twice the withdrawal gate alert threshold of the jurisdiction",
	// governed screening policy loosening (c4, migration 0190; raised by the licensing change executor, answered by the licensing change routes)
	"loosening_not_configured":      "screening policy loosening is not configured for this tenant (the capability is off until a platform administrator configures it)",
	"loosening_disabled":            "screening policy loosening is disabled for this tenant",
	"no_screening_policy_to_loosen": "there is no screening policy version to loosen; the first version is authored through the screening policy route",
	"not_a_loosening":               "the version is not less strict than the latest one; use the screening policy route",
	"loosening_cooling_period":      "the cooling period of the loosening, counted from the filing of the request, has not elapsed",
	// screening policy (lic29)
	"policy_loosening_refused": "the new screening policy version is less strict than the latest one (a list removed, a longer re-screen interval or grace, or an earlier effective time); this route only tightens, loosening is a governed licensing change request (aml_screening_policy_loosen) that is refused unless configured",
}

// refusalError carries the closed code next to the classified error; errors.Is / As still see the wrapped sentinel.
type refusalError struct {
	code string
	err  error
}

func (e *refusalError) Error() string { return e.err.Error() }
func (e *refusalError) Unwrap() error { return e.err }

// RefusalCode returns the closed refusal code of err, or "".
func RefusalCode(err error) string {
	var re *refusalError
	if errors.As(err, &re) {
		return re.code
	}
	return ""
}

// ClassifyDBError maps a database error to the service error exactly as every service call does (exported for the HTTP contract tests).
func ClassifyDBError(err error) error { return classify(err) }

// coded attaches a closed refusal code to an error raised by the Go-side validation, so the answer is identical to the database's
// (the Back Office sees one vocabulary whichever layer refused). The code must be in RefusalCodes.
func coded(code string, err error) error {
	if _, ok := RefusalCodes[code]; !ok {
		panic("aml: unknown refusal code " + code)
	}
	return &refusalError{code: code, err: err}
}
