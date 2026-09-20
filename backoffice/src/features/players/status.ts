import type { BadgeTone } from '../../components/Badge'

export const PLAYER_STATUS_OPTIONS = [
  { value: 'pending_verification', label: 'Pending verification' },
  { value: 'active', label: 'Active' },
  { value: 'suspended', label: 'Suspended' },
  { value: 'self_excluded', label: 'Self-excluded' },
  { value: 'closed', label: 'Closed' },
  { value: 'identity_review_required', label: 'Identity review required' },
]

export function playerStatusTone(status: string): BadgeTone {
  switch (status) {
    case 'active':
      return 'success'
    case 'suspended':
    case 'identity_review_required':
      return 'warning'
    case 'self_excluded':
    case 'closed':
      return 'danger'
    default:
      return 'neutral'
  }
}
