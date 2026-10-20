// Maps every failure of the lic16 policy routes to a fixed, UI-owned sentence. The server `message` is NEVER shown: only the
// closed `code`, the HTTP status and the request id are used. Context-specific wording explains the usual causes of a 403 / 409
// on each action, but never claims to know which one applied (the server decides).
import { ApiError } from '../../api/types'
import { STEP_UP_MESSAGES } from '../../auth/stepUpCoordinator'

const NOTHING = 'Nothing was changed.'

export type PolicyCtx =
  | 'read'
  | 'kyc_author'
  | 'kyc_activate'
  | 'kyc_withdraw'
  | 'kyc_supersede'
  | 'residence_set'
  | 'residence_policy_approve'
  | 'residence_determination_approve'
  | 'wrp_file'
  | 'wrp_approve'
  | 'wrp_reject'
  | 'wrp_cancel'

const FORBIDDEN: Record<PolicyCtx, string> = {
  read: `The server refused this: your account does not hold the permission to see it, or it is not available to this kind of session. ${NOTHING}`,
  kyc_author: `The server refused this: your account may not author KYC enforcement policies. ${NOTHING}`,
  kyc_activate: `The server refused this. Either your account may not manage KYC enforcement policies, or you authored this draft: the database requires a DIFFERENT platform administrator to activate it. ${NOTHING}`,
  kyc_withdraw: `The server refused this: your account may not manage KYC enforcement policies. ${NOTHING}`,
  kyc_supersede: `The server refused this. Either your account may not manage KYC enforcement policies, or you authored the active policy or the successor: the acting administrator must differ from BOTH authors. ${NOTHING}`,
  residence_set: `The server refused this: only compliance staff of this tenant may change the residence approval mode, and a platform session cannot. ${NOTHING}`,
  residence_policy_approve: `The server refused this. Either your account may not manage this policy, or you (or another account of your Person) filed the request, or your account is not linked to a Person: a different, linked Person must approve. ${NOTHING}`,
  residence_determination_approve: `The server refused this. Either your account may not review verifications, or you made this determination (a different staff account AND a different Person must approve it), or your account is not linked to a Person. ${NOTHING}`,
  wrp_file: `The server refused this: only platform administrators may file withdrawal restriction changes. ${NOTHING}`,
  wrp_approve: `The server refused this: only platform administrators who are not the requester's Person may approve, and your account must be linked to a Person. ${NOTHING}`,
  wrp_reject: `The server refused this: only platform administrators who are not the requester's Person may reject. ${NOTHING}`,
  wrp_cancel: `The server refused this: only the person who filed the request may cancel it. ${NOTHING}`,
}

const CONFLICT: Record<PolicyCtx, string> = {
  read: `The request conflicts with the current state. ${NOTHING}`,
  kyc_author: `The request conflicts with the current state. ${NOTHING}`,
  kyc_activate: `This policy cannot be activated now: it is not a draft, its trigger type is not consulted by the evaluator, or an active policy already exists for the same enforcement key (supersede that one instead). ${NOTHING}`,
  kyc_withdraw: `Only a never-activated draft can be withdrawn. An active policy is never switched off: replace it by superseding it with an activated successor. ${NOTHING}`,
  kyc_supersede: `This policy cannot be superseded: it is not active, or the successor is not a draft of the same enforcement key (jurisdiction, trigger type, play operation, asset). ${NOTHING}`,
  residence_set: `The requested mode is the mode already in force, or a change for this tenant is already open. ${NOTHING}`,
  residence_policy_approve: `Nothing to approve: the request is not pending, this Person already approved it, or the mode in force changed since it was filed (it must then be filed again). ${NOTHING}`,
  residence_determination_approve: `Nothing to approve: the determination needs no approval, or it already has one. Refresh the queue. ${NOTHING}`,
  wrp_file: `The change was refused: the cause is not configurable here (tenant and brand status are not), or an open change already exists for this tenant and cause, or the record is busy. ${NOTHING}`,
  wrp_approve: `The approval was refused: the request is no longer open, you (or your Person) filed it or already approved it, an earlier approval is older than 24 hours, or a database safeguard refused it. ${NOTHING}`,
  wrp_reject: `The rejection was refused: the request is no longer open, or you (or your Person) filed it. ${NOTHING}`,
  wrp_cancel: `The cancellation was refused: the request is no longer open. ${NOTHING}`,
}

const NOT_FOUND: Partial<Record<PolicyCtx, string>> = {
  residence_determination_approve: 'This determination was not found. A newer determination may have replaced it. Refresh the queue; nothing was approved.',
  residence_policy_approve: 'This policy request was not found. Refresh the page. Nothing was approved.',
}

const WRP_CODES: Record<string, string> = {
  WITHDRAWAL_RESTRICTION_POLICY_SELF_APPROVAL: `You cannot decide a request you filed, and another account of the same person cannot either. A different person must decide it. ${NOTHING}`,
  WITHDRAWAL_RESTRICTION_POLICY_NOT_PENDING: `This request is no longer open (it was executed, rejected, cancelled or expired). Refresh to see its current state. ${NOTHING}`,
  WITHDRAWAL_RESTRICTION_POLICY_ALREADY_DECIDED: `You (this person) already approved this request. A second approval must come from a different person. ${NOTHING}`,
  WITHDRAWAL_RESTRICTION_POLICY_DUPLICATE_PENDING: `An open change already exists for this tenant and cause. Approve, reject or cancel that one instead of filing another. ${NOTHING}`,
  WITHDRAWAL_RESTRICTION_POLICY_APPROVALS_STALE: `An earlier approval is older than 24 hours and no longer counts. Cancel this request and file a new one. ${NOTHING}`,
  WITHDRAWAL_RESTRICTION_POLICY_NOT_CONFIGURABLE: `This cause cannot be configured here. Tenant and brand status, and market closure, are governed by recorded owner decisions, not by this policy. ${NOTHING}`,
  WITHDRAWAL_RESTRICTION_POLICY_GUARD_REFUSED: `A database safeguard refused the change. ${NOTHING} Contact platform support with the request ID.`,
  WITHDRAWAL_RESTRICTION_POLICY_RETRYABLE_LOCK_TIMEOUT: `The tenant's records are busy right now (in-flight withdrawals finish first). ${NOTHING} Try again in a moment.`,
  WITHDRAWAL_RESTRICTION_POLICY_PROOF_REFUSED: `The signed proof for this action was refused. ${NOTHING} Contact platform support with the request ID.`,
  WITHDRAWAL_RESTRICTION_POLICY_NO_PERSON: `Your account is not linked to a person record, so the two-person rule cannot be checked. ${NOTHING} Ask an administrator to link it.`,
  WITHDRAWAL_RESTRICTION_POLICY_UNAVAILABLE: `The withdrawal restriction policy service is not available in this environment. ${NOTHING}`,
}

export interface PolicyErrorView {
  message: string
  code?: string
  status?: number
  requestId?: string
}

export function describePolicyError(err: unknown, ctx: PolicyCtx): PolicyErrorView {
  if (!(err instanceof ApiError)) return { message: 'Something went wrong. Nothing was confirmed. Please try again.' }
  const base = { code: err.code as string, status: err.status, requestId: err.requestId }
  if (Object.prototype.hasOwnProperty.call(WRP_CODES, err.code)) return { ...base, message: WRP_CODES[err.code] }
  switch (err.code) {
    case 'step_up_cancelled':
      return { ...base, message: STEP_UP_MESSAGES.cancelled }
    case 'step_up_failed':
      return { ...base, message: STEP_UP_MESSAGES.locked }
    case 'step_up_required':
      return { ...base, message: STEP_UP_MESSAGES.not_effective }
    case 'network_error':
      return { ...base, message: 'Could not reach the server. Nothing was confirmed. Check your connection and try again.' }
    case 'unauthorized':
      return { ...base, message: 'Your session has ended. Sign in again.' }
    case 'validation_error':
    case 'invalid_request':
      return { ...base, message: `The server rejected the request as invalid (for example an unknown jurisdiction or asset, a value that is not allowed, or a malformed field). Check the fields. ${NOTHING}` }
    case 'service_unavailable':
      return { ...base, message: 'The service is not available right now. Nothing was confirmed. Try again later.' }
  }
  if (err.status === 403) return { ...base, message: FORBIDDEN[ctx] }
  if (err.status === 409) return { ...base, message: CONFLICT[ctx] }
  if (err.status === 404) return { ...base, message: NOT_FOUND[ctx] ?? 'Not found. It may not exist, or you may not be allowed to see it.' }
  return { ...base, message: 'The action could not be completed. Nothing was confirmed. Check the current state before trying again, and quote the request ID if it persists.' }
}
