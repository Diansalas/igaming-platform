// UI-only mirror of the case rules in migration 0142 (aml_case_transition_allowed and the event guards), used to decide which
// action controls to OFFER. The server is authoritative: every action is re-validated there and a refusal is rendered through
// the closed error map. Hiding a control here is a convenience, never the control.
import type { AmlCase, AmlCaseEvent, CaseStatus, ProposalKind } from '../../api/aml'

export const STATUS_LABEL: Record<CaseStatus, string> = {
  open: 'Open',
  in_review: 'In review',
  escalated: 'Escalated',
  sar_decision_pending: 'SAR decision pending',
  closed_no_action: 'Closed, no action',
  closed_sar_not_filed: 'Closed, SAR not filed',
  closed_sar_filed: 'Closed, SAR filed',
}

export function statusLabel(s: string): string {
  return (STATUS_LABEL as Record<string, string>)[s] ?? 'Unrecognised status'
}

export function isClosed(s: CaseStatus): boolean {
  return s === 'closed_no_action' || s === 'closed_sar_not_filed' || s === 'closed_sar_filed'
}

/** Plain moves only: SAR closures go through propose / approve. A SAR-flagged case cannot close as no action. */
export function allowedTransitions(c: Pick<AmlCase, 'status' | 'sar_flagged'>): CaseStatus[] {
  switch (c.status) {
    case 'open':
      return ['in_review']
    case 'in_review':
      return c.sar_flagged ? ['escalated'] : ['escalated', 'closed_no_action']
    case 'escalated':
      return ['sar_decision_pending']
    case 'closed_no_action':
    case 'closed_sar_not_filed':
    case 'closed_sar_filed':
      return ['in_review']
    default:
      return []
  }
}

export function canFlagSar(c: Pick<AmlCase, 'status' | 'sar_flagged'>): boolean {
  return !c.sar_flagged && (c.status === 'in_review' || c.status === 'escalated' || c.status === 'sar_decision_pending')
}

export function canPropose(c: Pick<AmlCase, 'status'>): boolean {
  return c.status === 'in_review' || c.status === 'escalated' || c.status === 'sar_decision_pending'
}

/** A SAR closure is proposed from sar_decision_pending only; a hold lift whenever the case is being worked. */
export function proposalKinds(c: Pick<AmlCase, 'status'>): ProposalKind[] {
  if (!canPropose(c)) return []
  return c.status === 'sar_decision_pending' ? ['close_sar_not_filed', 'close_sar_filed', 'lift_hold'] : ['lift_hold']
}

export const PROPOSAL_LABEL: Record<ProposalKind, string> = {
  close_sar_not_filed: 'Close the case, SAR not filed',
  close_sar_filed: 'Close the case, SAR filed (external filing reference)',
  lift_hold: 'Lift a sanctions hold',
}

/**
 * The proposal an approver may act on now: the case's LAST event, and a proposal. Any later event (a note, a transition, a
 * hold change) invalidates it on the server, so nothing else is ever offered for approval.
 */
export function pendingProposal(c: Pick<AmlCase, 'last_seq'>, events: AmlCaseEvent[]): AmlCaseEvent | null {
  const last = events.find((e) => e.seq === c.last_seq)
  return last && last.type === 'proposal' && last.content_hash && last.proposal_kind ? last : null
}
