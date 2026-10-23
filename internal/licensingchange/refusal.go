package licensingchange

import (
	"errors"

	"github.com/Diansalas/igaming-platform/internal/jurisdiction"
	"github.com/Diansalas/igaming-platform/internal/operatingmarket"
)

// c1: closed, stable, machine-readable refusal codes for the licensing-change and tenant-policy refusals that used to share one
// generic answer (409 LICENSING_CHANGE_REFUSED, 400 validation_error, 409 LICENSING_CHANGE_GUARD_REFUSED, all without a reason).
// Same pattern as the AML refusal table: the HTTP status and the API error code of every refusal are UNCHANGED; the code travels in
// `check_codes`; classify keeps its sentinels; the wording belongs to the Back Office (licensingRefusalWording) and never shows
// the server text. A code is never renamed or reused. Adding one needs the Go branch that raises it, this table, the OpenAPI enum
// LicensingRefusalCode and the Back Office wording (a drift test pins the three together).
//
// Three sources feed the table:
//   - the request-time prechecks of the kinds (kinds.go, operating_basis_kinds.go, licence_lifecycle_kinds.go), which refuse with
//     the domain writer's own sentinel and now also a code;
//   - the executor's domain sentinels that a precheck cannot see coming (derived by sentinel, see ClosedRefusal);
//   - the SQLSTATE class LC of the database guards that reach classify (guardRefusals).
//
// Input SHAPE errors (a malformed payload, a missing field, a bad date) stay the generic 400 validation_error without a code: they
// are caught by the decode step before any database access and the Back Office form rules already block them.
var ClosedRefusals = map[string]string{
	// request-time prechecks
	"jurisdiction_unknown":                 "the jurisdiction does not exist",
	"tenant_unknown":                       "the tenant does not exist",
	"licence_unknown":                      "the licence does not exist",
	"licence_not_active":                   "a licence that is not active cannot be bound to a tenant",
	"licence_licensee_mismatch":            "the licence's licensee does not match the tenant's licensing model",
	"licence_bound_to_other_tenant":        "a licence held by a tenant is exclusively bound to one tenant, and it is bound to another",
	"brand_not_owned":                      "the brand is not a brand of the tenant (a foreign brand is indistinguishable from an unknown one)",
	"tenant_without_licence_or_basis":      "the tenant has no licence bound and no operating basis for the country, so no country may be enabled for it",
	"ceiling_not_enabled_for_country":      "the tenant's licence has no enabled ceiling for the country (a missing ceiling and a disabled one are the same answer)",
	"basis_clashes_with_licence_ceiling":   "the tenant's own licence already has a ceiling row for the country, so a documented no-licence basis would be ambiguous",
	"currency_not_active_asset":            "every allowed currency must be an active asset of the platform asset registry",
	"licence_status_unchanged":             "the licence already has this status",
	"licence_validity_dates_unchanged":     "the validity dates are unchanged",
	"licence_review_due_unchanged":         "the review date is unchanged",
	"licence_evidence_reference_unchanged": "the evidence reference is unchanged",
	"duplicate_jurisdiction_code":          "a jurisdiction with this code already exists",
	// executor domain sentinels
	"policy_exceeds_permitted_scope": "the policy would exceed the licence ceiling, a higher scope's policy, or a broader in-force policy at the same scope",
	"policy_close_needs_successor":   "closing an in-force disabled policy at an inheriting rung requires an open successor version",
	// database guards (SQLSTATE class LC)
	"session_not_permitted":                 "this session may not request or act on licensing changes",
	"request_invalid":                       "the database refused the request as invalid or naming an unknown record",
	"request_immutable":                     "a filed request cannot be edited",
	"approvals_insufficient":                "the request does not hold the required number of fresh approvals by distinct eligible people",
	"single_actor_execution_only":           "a single-actor change is executed only by its requester, in its own transaction",
	"only_requester_cancels":                "only the requester may cancel a request",
	"change_now_widens":                     "the change now widens what is permitted; a new request is needed",
	"approver_not_eligible":                 "only an eligible platform administrator decides a request",
	"governed_write_needs_executed_request": "governed data changes only through an executed licensing change request",
	"licence_identity_immutable":            "licence identity fields can never change",
	"operating_basis_write_refused":         "the operating basis, licence or tenant records do not allow this write",
	"closed_basis_needs_successor":          "a closed operating basis version needs a successor version that starts when it ended",
	"records_append_only":                   "licensing change records are append-only",
}

// guardRefusals maps the SQLSTATE of a database guard that reaches classify to its closed code. A state not listed (AP class,
// LC013/031 which have their own sentinels, LC022/032 self-approval) carries no refusal code. LC095 / LC098 are migration
// pre-flight refusals that no request can reach.
var guardRefusals = map[string]string{
	"LC001": "session_not_permitted",
	"LC002": "session_not_permitted",
	"LC010": "request_invalid",
	"LC012": "request_immutable",
	"LC020": "approvals_insufficient",
	"LC021": "single_actor_execution_only",
	"LC023": "only_requester_cancels",
	"LC024": "change_now_widens",
	"LC030": "approver_not_eligible",
	"LC040": "governed_write_needs_executed_request",
	"LC041": "governed_write_needs_executed_request",
	"LC042": "licence_identity_immutable",
	"LC043": "operating_basis_write_refused",
	"LC044": "closed_basis_needs_successor",
	"LC099": "records_append_only",
}

// refuse is a domain refusal carrying a closed code (the code must be in ClosedRefusals).
func refuse(code string, err error) *DomainError {
	if _, ok := ClosedRefusals[code]; !ok {
		panic("licensingchange: unknown refusal code " + code)
	}
	return &DomainError{Err: err, Code: code}
}

// ClosedRefusal returns the closed refusal code of err, or "" (a generic refusal, an authorisation refusal, an internal error).
// Order: the code a precheck attached; the database guard's code; the executor's domain sentinels.
func ClosedRefusal(err error) string {
	if err == nil {
		return ""
	}
	var de *DomainError
	if errors.As(err, &de) && de.Code != "" {
		return de.Code
	}
	if g := GuardCode(err); g != "" {
		return guardRefusals[g]
	}
	switch {
	case errors.Is(err, jurisdiction.ErrInactiveAsset):
		return "currency_not_active_asset"
	case errors.Is(err, operatingmarket.ErrCeilingExceeded):
		return "policy_exceeds_permitted_scope"
	case errors.Is(err, operatingmarket.ErrPolicyCloseRequiresSuccessor):
		return "policy_close_needs_successor"
	}
	return ""
}

// ClassifyDBError maps a database error to the service error exactly as every service call does (exported for the HTTP contract tests).
func ClassifyDBError(err error) error { return classify(err) }
