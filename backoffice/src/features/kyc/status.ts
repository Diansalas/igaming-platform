import type { BadgeTone } from '../../components/Badge'
import type { KycStatus } from '../../api/kyc'

export const KYC_STATUS_OPTIONS: { value: KycStatus; label: string }[] = [
  { value: 'unverified', label: 'Unverified' },
  { value: 'pending', label: 'Pending' },
  { value: 'review_required', label: 'Review required' },
  { value: 'approved', label: 'Approved' },
  { value: 'rejected', label: 'Rejected' },
  { value: 'expired', label: 'Expired' },
]

export function kycStatusTone(status: string): BadgeTone {
  switch (status) {
    case 'approved':
      return 'success'
    case 'rejected':
    case 'expired':
      return 'danger'
    case 'review_required':
    case 'pending':
      return 'warning'
    default:
      return 'neutral'
  }
}
