// ADR 0112 slice 6 (d): the server tells the UI whether THE CALLER may approve a request now
// (can_approve / cannot_approve_reason, closed catalogue). The UI mirrors that answer and nothing else:
// it no longer guesses "same person" from a staff id. The answer is advisory; the decision route and the
// database (LA012) stay authoritative, and a refusal the UI did not predict is still shown (errors.ts).
import type { CannotApproveReason, LaunchRequest } from '../../api/launch'

export const CANNOT_APPROVE_TEXT: Record<CannotApproveReason, string> = {
  not_platform_approver: 'Only platform approvers can decide launch requests.',
  not_pending: 'This request is no longer pending.',
  expired: 'This request has expired and can no longer be decided.',
  self_approval: 'You filed this request. A different platform approver must decide it.',
  same_person: 'This request was filed by another account of the same person. A different platform approver must decide it.',
  no_person: 'Your account (or the requester account) is not linked to a person, so a four-eyes decision cannot be taken.',
  already_decided: 'You have already recorded a decision on this request.',
  prior_reject_requires_change_note: 'You rejected an earlier variant of this request, and this one carries no change note. You can reject it, but not approve it.',
}

const GENERIC = 'You cannot approve this request now.'

export function cannotApproveText(reason: string | undefined): string {
  if (reason && Object.prototype.hasOwnProperty.call(CANNOT_APPROVE_TEXT, reason)) return CANNOT_APPROVE_TEXT[reason as CannotApproveReason]
  return GENERIC
}

export interface ApprovalControls {
  approve: boolean
  reject: boolean
  /** Fixed sentence explaining why approve is off; null when nothing needs saying. */
  note: string | null
}

/**
 * Which decision buttons to show. `roleMayApprove` is the static role hint (launchApprove); the server's
 * per-request answer refines it. An absent answer (an older server) shows the buttons: the server decides.
 * A prior-reject restriction removes approve only (the server permits reject).
 */
export function approvalControls(r: Pick<LaunchRequest, 'can_approve' | 'cannot_approve_reason'>, roleMayApprove: boolean): ApprovalControls {
  if (!roleMayApprove) return { approve: false, reject: false, note: null }
  if (r.can_approve !== false) return { approve: true, reject: true, note: null }
  const reject = r.cannot_approve_reason === 'prior_reject_requires_change_note'
  return { approve: false, reject, note: cannotApproveText(r.cannot_approve_reason) }
}
