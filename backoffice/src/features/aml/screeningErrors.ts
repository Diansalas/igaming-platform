// Fixed, UI-owned wording for every non-2xx on the four lic18 screening routes. The server `message` is NEVER displayed: only the
// HTTP status, the closed check code and the request id are used. The generic AML map handles transport, step-up, 401, 429, 5xx.
// The server gives no machine-readable sub-reason for the worker-heartbeat refusal (it is a database guard, shown as the closed
// check `guard_refused`), so the wording lists the usual causes and never claims to know which one applied.
import { ApiError } from '../../api/types'
import { describeAmlError, type AmlErrorView } from './errors'

export type ScreeningOp = 'policy_read' | 'policy_set' | 'requests' | 'resolve' | 'screening_policy_read' | 'screening_policy_set'

const NOTHING = 'Nothing was changed.'

export const WORKER_REFUSAL =
  'The server refused to enable or change this policy. A withdrawal gate is refused unless a screening worker has reported a recent heartbeat, and this environment may have none (the production platform starts no screening worker, by design). Other causes: no screening policy exists for the jurisdiction, the jurisdiction is not the one of the tenant licence, or the effective time is outside the allowed window. The server does not say which applied. ' + NOTHING

/**
 * lic31: exact, UI-owned wording for the CLOSED refusal codes of the screening routes (aml.RefusalCodes, carried in `check_codes`;
 * AMLScreeningRefusalCode in the OpenAPI). The key set equals the server's table (pinned by a test that reads
 * internal/aml/refusal.go and the OpenAPI enum). The server text is never shown. Nothing here states that anything is lawful.
 */
export const REFUSAL_TEXT: Record<string, string> = {
  platform_officer_required: 'Screening configuration is authored by a platform administrator acting as an AML officer for this tenant. Your account is not acting in that capacity.',
  effective_from_out_of_range: 'A trigger policy takes effect between now (2 minutes of clock skew are tolerated) and 30 days ahead. The chosen time is outside that window.',
  gate_needs_scheduled_event: 'A withdrawal gate needs the scheduled re-screen event, otherwise a clear goes stale unattended. Add the scheduled event or turn the gate off.',
  gate_needs_alert_threshold: 'A withdrawal gate needs an alert threshold (gate_alert_after_seconds, 60 to 2592000 seconds).',
  screening_worker_heartbeat_missing: 'No screening worker has reported in, so a withdrawal gate cannot be enabled. A gate with no working worker would trap player funds. The production platform starts no screening worker, by design.',
  gate_environment_refused: 'A withdrawal gate cannot be enabled where the environment is production or the environment is not recorded.',
  alert_threshold_without_gate: 'An alert threshold belongs to a policy that gates withdrawals only. Remove it or turn the gate on.',
  jurisdiction_not_in_scope: 'The jurisdiction is not the one of the tenant\'s licence and is not one covered by an approved operating basis of the tenant.',
  screening_policy_missing: 'Set the screening policy (re-screen interval, grace and lists) for this jurisdiction first, in effect at or before the trigger policy.',
  tenant_not_in_jurisdiction: 'The tenant is not configured to operate in this jurisdiction. A tenant jurisdiction configuration for it must exist first.',
  sanctions_clear_refused: 'A sanctions-list match is never cleared by an officer. It is resolved by recording the match, which places a hold.',
  sanctions_match_needs_hold: 'A sanctions-list match is resolved only by recording a sanctions match that places a hold.',
  error_cannot_resolve: 'An error result cannot resolve a request that is waiting for an officer.',
  request_key_mismatch: 'The request belongs to another person or another trigger than the one named.',
  request_not_awaiting_officer: 'The request is not waiting for an officer (it may already be resolved).',
  key_prefix_reserved: 'That idempotency key prefix is reserved for the automatic screening queue.',
  request_not_replaceable: 'Only a pending or claimed request can be replaced. A match or a terminal failure is resolved, not replaced.',
  request_lease_live: 'The request is being processed right now: its lease has not expired yet.',
  person_has_pending_request: 'The person already has a pending request. Name that request to replace it.',
  no_gating_policy: 'No trigger policy gates withdrawals for this tenant, so there is nothing to queue.',
  policy_effective_from_out_of_range: 'A screening policy takes effect between now (2 minutes of clock skew are tolerated) and 30 days ahead. The chosen time is outside that window.',
  rescreen_interval_below_floor: 'The re-screen interval is below the engineering safety floor of 3600 seconds. The floor is a safety limit of this software, not a legal value.',
  rescreen_interval_below_alert_threshold: 'The re-screen interval is below twice the withdrawal gate alert threshold of this jurisdiction. Lengthen the interval, or change the gate threshold first.',
  policy_loosening_refused: 'The new version is less strict than the latest one: a list was removed, the re-screen interval or the grace is longer, or it takes effect earlier. This form only tightens. Loosening is a separate two-person request in the licensing changes area, and it is refused unless a platform administrator has switched that capability on for this tenant.',
  loosening_not_configured: 'Loosening the screening policy is not configured for this tenant. The capability is off until a platform administrator configures it through a two-person licensing change. Nothing is loosened by default.',
  loosening_disabled: 'Loosening the screening policy is switched off for this tenant. A platform administrator must enable it through a two-person licensing change first.',
  no_screening_policy_to_loosen: 'There is no screening policy version to loosen for this tenant and jurisdiction. The first version is authored on the screening policy form.',
  not_a_loosening: 'The request is not less strict than the latest screening policy version. Use the screening policy form to tighten or keep it.',
  loosening_cooling_period: 'The cooling period of this loosening request has not elapsed yet. The period is counted from the filing of the request; approve again once it has passed.',
}

const FORBIDDEN: Record<ScreeningOp, string> = {
  screening_policy_read: `The server refused this: your account does not hold an in-force AML officer grant for this tenant. ${NOTHING}`,
  screening_policy_set: `The server refused this. Authoring a screening policy is for a platform administrator acting as an AML officer for this tenant, with the officer quorum and a signed proof; your account does not qualify, or the proof was refused. ${NOTHING}`,
  policy_read: `The server refused this: your account does not hold an in-force AML officer grant for this tenant. ${NOTHING}`,
  policy_set: `The server refused this. Authoring a trigger policy is for a platform administrator acting as an AML officer for this tenant, with the officer quorum and a signed proof; your account does not qualify, or the proof was refused. ${NOTHING}`,
  requests: `The server refused this: your account does not hold an in-force AML officer grant for this tenant. ${NOTHING}`,
  resolve: `The server refused this: your account does not hold an in-force AML officer grant for this tenant's owner scope, or the signed proof was refused. ${NOTHING}`,
}

const CONFLICT: Partial<Record<ScreeningOp, string>> = {
  resolve: `The resolution was refused. Usual causes: the request is no longer awaiting an officer (already resolved), a sanctions-list match cannot be cleared by an officer and must be recorded as a match, or the officer rules refused it. The server does not say which applied. ${NOTHING} The list is being refreshed.`,
}

export function describeScreeningError(err: unknown, op: ScreeningOp): AmlErrorView {
  const generic = describeAmlError(err, op === 'policy_read' || op === 'screening_policy_read' || op === 'requests' ? 'holds' : 'place_hold')
  if (!(err instanceof ApiError)) return generic
  if (err.code === 'network_error' || err.code === 'step_up_cancelled' || err.code === 'step_up_failed') return generic
  if (err.checkCodes.includes('quorum_not_met') || err.checkCodes.includes('four_eyes_refused')) return generic
  const exact = err.checkCodes.find((c) => Object.prototype.hasOwnProperty.call(REFUSAL_TEXT, c))
  if (exact) return { ...generic, stale: err.status === 409 && op === 'resolve', message: `${REFUSAL_TEXT[exact]} ${NOTHING}` }
  if (err.status === 409) {
    if (op === 'screening_policy_set') return { ...generic, stale: false, message: `The server refused the new screening policy version (a conflict). ${NOTHING}` }
    if (op === 'policy_set') return { ...generic, stale: false, message: WORKER_REFUSAL }
    if (op === 'resolve') return { ...generic, stale: true, message: CONFLICT.resolve! }
  }
  if (err.status === 403) return { ...generic, message: FORBIDDEN[op] }
  if (err.status === 400) {
    return {
      ...generic,
      message:
        op === 'screening_policy_set'
          ? `The server rejected the screening policy as invalid. Check: at least one list; every number is whole seconds (interval 3600 to 31536000, grace 0 to 31536000); the effective time is between now and 30 days ahead; the legal review reference is 1 to 200 characters without surrounding space. ${NOTHING}`
          : op === 'policy_set'
            ? `The server rejected the policy as invalid. Check: a withdrawal gate needs the scheduled event and an alert threshold of 60 to 2592000 seconds; the effective time may not be in the past nor more than 30 days ahead; the legal review reference is 1 to 200 characters without surrounding space. ${NOTHING}`
            : op === 'resolve'
              ? `The server rejected the resolution as invalid. Check the outcome, the lists screened and the reason code. An error outcome never resolves a request. ${NOTHING}`
              : `The server rejected the request as invalid. Check the identifiers. ${NOTHING}`,
    }
  }
  return generic
}
