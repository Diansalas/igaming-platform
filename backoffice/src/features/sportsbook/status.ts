import type { BadgeTone } from '../../components/Badge'
import type { AdminBetStatus } from '../../api/sportsbookAdmin'

export function betStatusTone(status: AdminBetStatus): BadgeTone {
  switch (status) {
    case 'settled_won':
      return 'success'
    case 'open':
      return 'warning'
    case 'settled_lost':
    case 'void':
      return 'danger'
    default:
      return 'neutral'
  }
}

/** Renders an integer-fraction odds pair (e.g. 5/2) as a decimal string for display only - never fed back into a request. */
export function formatOdds(numerator: number, denominator: number): string {
  return (numerator / denominator).toFixed(2)
}
