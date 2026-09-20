import type { BadgeTone } from '../../components/Badge'
import type { WithdrawalState } from '../../api/withdrawals'

export const WITHDRAWAL_STATUS_OPTIONS: { value: WithdrawalState; label: string }[] = [
  { value: 'requested', label: 'Requested' },
  { value: 'pending_review', label: 'Pending review' },
  { value: 'approved', label: 'Approved' },
  { value: 'rejected', label: 'Rejected' },
  { value: 'submitted', label: 'Submitted' },
  { value: 'completed', label: 'Completed' },
  { value: 'failed', label: 'Failed' },
  { value: 'cancelled', label: 'Cancelled' },
  { value: 'reversed', label: 'Reversed' },
]

export function withdrawalStatusTone(state: string): BadgeTone {
  switch (state) {
    case 'completed':
    case 'approved':
      return 'success'
    case 'pending_review':
    case 'requested':
    case 'submitted':
      return 'warning'
    case 'rejected':
    case 'failed':
    case 'cancelled':
    case 'reversed':
      return 'danger'
    default:
      return 'neutral'
  }
}
