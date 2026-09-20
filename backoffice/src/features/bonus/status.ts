import type { BadgeTone } from '../../components/Badge'

export const CAMPAIGN_STATUS_OPTIONS = [
  { value: 'draft', label: 'Draft' },
  { value: 'active', label: 'Active' },
  { value: 'paused', label: 'Paused' },
  { value: 'ended', label: 'Ended' },
  { value: 'archived', label: 'Archived' },
]

export function campaignStatusTone(status: string): BadgeTone {
  switch (status) {
    case 'active':
      return 'success'
    case 'paused':
      return 'warning'
    case 'archived':
    case 'ended':
      return 'neutral'
    default:
      return 'info'
  }
}

export const CHANGE_REQUEST_STATUS_OPTIONS = [
  { value: 'pending', label: 'Pending' },
  { value: 'applied', label: 'Applied' },
  { value: 'rejected', label: 'Rejected' },
  { value: 'cancelled', label: 'Cancelled' },
]

export function changeRequestStatusTone(status: string): BadgeTone {
  switch (status) {
    case 'applied':
      return 'success'
    case 'rejected':
    case 'cancelled':
      return 'danger'
    default:
      return 'warning'
  }
}

export const GRANT_STATUS_OPTIONS = [
  { value: 'issued', label: 'Issued' },
  { value: 'activated', label: 'Activated' },
  { value: 'in_progress', label: 'In progress' },
  { value: 'pending_settlement', label: 'Pending settlement' },
  { value: 'completed', label: 'Completed' },
  { value: 'converted', label: 'Converted' },
  { value: 'expired', label: 'Expired' },
  { value: 'cancelled', label: 'Cancelled' },
  { value: 'forfeited', label: 'Forfeited' },
  { value: 'reversed', label: 'Reversed' },
]

export function grantStatusTone(status: string): BadgeTone {
  switch (status) {
    case 'completed':
    case 'converted':
    case 'activated':
    case 'in_progress':
      return 'success'
    case 'expired':
    case 'cancelled':
    case 'forfeited':
    case 'reversed':
      return 'danger'
    default:
      return 'neutral'
  }
}
