// Fixed, UI-owned wording for every non-2xx on the four lic18 screening routes. The server `message` is NEVER displayed: only the
// HTTP status, the closed check code and the request id are used. The generic AML map handles transport, step-up, 401, 429, 5xx.
// The server gives no machine-readable sub-reason for the worker-heartbeat refusal (it is a database guard, shown as the closed
// check `guard_refused`), so the wording lists the usual causes and never claims to know which one applied.
import { ApiError } from '../../api/types'
import { describeAmlError, type AmlErrorView } from './errors'

export type ScreeningOp = 'policy_read' | 'policy_set' | 'requests' | 'resolve'

const NOTHING = 'Nothing was changed.'

export const WORKER_REFUSAL =
  'The server refused to enable or change this policy. A withdrawal gate is refused unless a screening worker has reported a recent heartbeat, and this environment may have none (the production platform starts no screening worker, by design). Other causes: no screening policy exists for the jurisdiction, the jurisdiction is not the one of the tenant licence, or the effective time is outside the allowed window. The server does not say which applied. ' + NOTHING

const FORBIDDEN: Record<ScreeningOp, string> = {
  policy_read: `The server refused this: your account does not hold an in-force AML officer grant for this tenant. ${NOTHING}`,
  policy_set: `The server refused this. Authoring a trigger policy is for a platform administrator acting as an AML officer for this tenant, with the officer quorum and a signed proof; your account does not qualify, or the proof was refused. ${NOTHING}`,
  requests: `The server refused this: your account does not hold an in-force AML officer grant for this tenant. ${NOTHING}`,
  resolve: `The server refused this: your account does not hold an in-force AML officer grant for this tenant's owner scope, or the signed proof was refused. ${NOTHING}`,
}

const CONFLICT: Partial<Record<ScreeningOp, string>> = {
  resolve: `The resolution was refused. Usual causes: the request is no longer awaiting an officer (already resolved), a sanctions-list match cannot be cleared by an officer and must be recorded as a match, or the officer rules refused it. The server does not say which applied. ${NOTHING} The list is being refreshed.`,
}

export function describeScreeningError(err: unknown, op: ScreeningOp): AmlErrorView {
  const generic = describeAmlError(err, op === 'policy_read' ? 'holds' : op === 'requests' ? 'holds' : 'place_hold')
  if (!(err instanceof ApiError)) return generic
  if (err.code === 'network_error' || err.code === 'step_up_cancelled' || err.code === 'step_up_failed') return generic
  if (err.checkCodes.includes('quorum_not_met') || err.checkCodes.includes('four_eyes_refused')) return generic
  if (err.status === 409) {
    if (op === 'policy_set') return { ...generic, stale: false, message: WORKER_REFUSAL }
    if (op === 'resolve') return { ...generic, stale: true, message: CONFLICT.resolve! }
  }
  if (err.status === 403) return { ...generic, message: FORBIDDEN[op] }
  if (err.status === 400) {
    return {
      ...generic,
      message:
        op === 'policy_set'
          ? `The server rejected the policy as invalid. Check: a withdrawal gate needs the scheduled event and an alert threshold of 60 to 2592000 seconds; the effective time may not be in the past nor more than 30 days ahead; the legal review reference is 1 to 200 characters without surrounding space. ${NOTHING}`
          : op === 'resolve'
            ? `The server rejected the resolution as invalid. Check the outcome, the lists screened and the reason code. An error outcome never resolves a request. ${NOTHING}`
            : `The server rejected the request as invalid. Check the identifiers. ${NOTHING}`,
    }
  }
  return generic
}
