// c1: exact, UI-owned wording for the CLOSED refusal codes of the licensing change and tenant policy routes
// (licensingchange.ClosedRefusals in internal/licensingchange/refusal.go, carried in `check_codes`; the OpenAPI schema
// LicensingRefusalCode). The key set equals the server's table: licensingRefusal.test.ts reads the Go source and the OpenAPI enum
// and fails on any difference. The server text is never shown. Nothing here states that anything is lawful or authorised; the
// wording says which recorded precondition failed, never whether an operation is permitted by a regulator.
export const licensingRefusalWording: Record<string, string> = {
  // request-time prechecks
  jurisdiction_unknown: 'The jurisdiction named in the request does not exist. Pick a jurisdiction from the registry.',
  tenant_unknown: 'The tenant named in the request does not exist.',
  licence_unknown: 'The licence named in the request does not exist.',
  licence_not_active: 'The licence is not active (it is suspended or expired), so it cannot be bound to a tenant. Reinstate it first, or pick an active licence.',
  licence_licensee_mismatch: 'The licence belongs to a different kind of licensee than this tenant: a platform-licensed tenant takes a platform licence and a tenant with its own licence takes a tenant licence.',
  licence_bound_to_other_tenant: 'This tenant-held licence is already bound to another tenant. A licence held by a tenant belongs to exactly one tenant.',
  brand_not_owned: 'The brand is not a brand of this tenant. A brand of another tenant and a brand that does not exist are reported the same way.',
  tenant_without_licence_or_basis: 'This tenant has no licence bound and no operating basis for the country, so the country cannot be enabled for it. Bind a licence or file an operating basis first.',
  ceiling_not_enabled_for_country: 'The tenant\'s licence has no enabled country ceiling for this country (none is recorded, or it is disabled or withdrawn). A policy cannot enable a country that the licence ceiling does not enable. Record the ceiling first.',
  basis_clashes_with_licence_ceiling: 'The tenant\'s own licence already has a country ceiling for this country, so a documented no-licence basis would be ambiguous. Withdraw one of the two first.',
  currency_not_active_asset: 'Every allowed currency must be an active asset of the platform asset registry. At least one listed currency is not.',
  licence_status_unchanged: 'The licence already has this status. A change must change something.',
  licence_validity_dates_unchanged: 'The issue and expiry dates are the same as those recorded. A change must change something.',
  licence_review_due_unchanged: 'The review date is the same as the one recorded. A change must change something.',
  licence_evidence_reference_unchanged: 'The evidence reference is the same as the one recorded. A change must change something.',
  duplicate_jurisdiction_code: 'A jurisdiction with this code already exists. Codes are unique; choose another code.',
  // executor domain sentinels
  policy_exceeds_permitted_scope: 'At execution the policy would exceed the licence ceiling, a higher scope\'s policy, or a broader policy in force at the same scope. File a new request that fits within them.',
  policy_close_needs_successor: 'Closing an in-force disabled policy at an inheriting level needs an open successor version in the same change. File it with a successor.',
  // database guards
  session_not_permitted: 'The database does not accept this session for licensing changes (it must be an active staff member with a person record, acting within its own scope).',
  request_invalid: 'The database refused the request as invalid or as naming a record that does not exist.',
  request_immutable: 'A filed request cannot be edited. Cancel it and file a new one.',
  approvals_insufficient: 'The request does not hold the required number of fresh approvals by distinct eligible people, so it was not executed.',
  single_actor_execution_only: 'A change that needs no second approval is executed only by the person who filed it, in the same step.',
  only_requester_cancels: 'Only the person who filed a request can cancel it.',
  change_now_widens: 'Since this request was filed, the change has come to widen what is permitted, so a single approval is no longer enough. Reject or cancel it and file a new request.',
  approver_not_eligible: 'Only an eligible platform administrator decides a licensing change request.',
  governed_write_needs_executed_request: 'This data changes only through an executed licensing change request. The change was not recorded.',
  licence_identity_immutable: 'The identity of a licence (jurisdiction, licensee, number, permitted products and markets) can never change. File a new licence instead.',
  operating_basis_write_refused: 'The licence, tenant or operating basis records do not allow this write (for example the licence is bound to another tenant, or the licensee does not match the basis kind).',
  closed_basis_needs_successor: 'A closed operating basis version needs a successor version that starts when it ended. Closing it alone would remove a restriction silently.',
  records_append_only: 'Licensing change records are append-only and cannot be edited or deleted.',
}

/** Server `code`s on which a closed refusal code may arrive. Everything else (authorisation, step-up, 5xx) keeps its own wording. */
export const REFUSAL_BEARING_CODES = ['LICENSING_CHANGE_REFUSED', 'LICENSING_CHANGE_GUARD_REFUSED', 'validation_error', 'not_found']

/** The exact wording of the first closed code the server sent, or undefined (the generic wording then applies). */
export function licensingRefusalFor(code: string, status: number, checkCodes: string[]): string | undefined {
  if (!REFUSAL_BEARING_CODES.includes(code) || ![400, 404, 409].includes(status)) return undefined
  const known = checkCodes.find((c) => Object.prototype.hasOwnProperty.call(licensingRefusalWording, c))
  return known ? licensingRefusalWording[known] : undefined
}
