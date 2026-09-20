import type { BadgeTone } from '../../components/Badge'
import type { AdminRoundStatus } from '../../api/casinoAdmin'

export function roundStatusTone(status: AdminRoundStatus): BadgeTone {
  switch (status) {
    case 'won':
      return 'success'
    case 'wagered':
      return 'info'
    case 'rolled_back':
      return 'warning'
    default:
      return 'neutral'
  }
}
