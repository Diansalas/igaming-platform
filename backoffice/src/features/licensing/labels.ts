// Fixed, UI-owned wording for the licensing and jurisdiction screens (lic5). No sentence states a legal conclusion:
// a record here is a decision recorded by named staff, never a licence verdict or a jurisdiction authorisation.
import type { BadgeTone } from '../../components/Badge'

export const LICENSING_DISCLAIMER =
  'A licensing change record is a decision recorded by named staff. It is not a legal or regulatory authorisation and does not determine whether any licence or jurisdiction permits operation.'

export const EVIDENCE_NOTE =
  'The licence evidence reference is a reference supplied by the operator. The platform does not verify it, stores no document, and a reference is not proof of anything.'

export const REGISTRY_NOTE =
  'Registry data is configuration recorded by platform staff. Its presence here does not mean a jurisdiction or licence has been checked or accepted by the platform.'

export const APPROVAL_RULES = [
  'A widening change needs 2 approvals from 2 different people (different Person records).',
  'The requester cannot approve or reject their own request, and neither can another account of the same person.',
  'Every counted approval must be at most 24 hours old when the change executes; otherwise the server refuses it and a new request is needed.',
  'An open request expires 72 hours after filing.',
  'A narrowing or neutral change filed by a platform administrator takes effect at once, without a second person.',
] as const

export const REASON_CODE_HELP = 'A code, not free text: 1 to 64 characters of letters, digits, _ . : - (it starts with a letter or digit).'

const KIND_LABEL: Record<string, string> = {
  licence_create: 'Create licence',
  tenant_licence_bind: 'Bind tenant to licence',
  tenant_licence_unbind: 'Unbind tenant licence',
  licence_ceiling_version: 'Licence country ceiling version',
  operating_policy_version: 'Operating country policy version',
  tenant_jurisdiction_config_version: 'Tenant jurisdiction configuration version',
  licence_status_change: 'Licence status change',
  licence_validity_dates_change: 'Licence validity dates change',
  licence_review_due_change: 'Licence review-due change',
  licence_evidence_reference_change: 'Licence evidence reference change',
  operating_basis_set: 'Set operating basis',
  operating_basis_withdraw: 'Withdraw operating basis',
  operating_basis_review_record: 'Record operating basis review',
}
const SAFE = /^[A-Za-z0-9][A-Za-z0-9:_.-]{0,63}$/
export function safeToken(v: unknown): string {
  return typeof v === 'string' && SAFE.test(v) ? v : 'unprintable'
}
export const kindLabel = (k: unknown) => (typeof k === 'string' && Object.prototype.hasOwnProperty.call(KIND_LABEL, k) ? KIND_LABEL[k] : `Unrecognised kind (${safeToken(k)})`)
export const KIND_FILTER_OPTIONS = Object.entries(KIND_LABEL).map(([value, label]) => ({ value, label }))

const CHANGE_STATUS: Record<string, { label: string; tone: BadgeTone }> = {
  requested: { label: 'Pending approval', tone: 'warning' },
  approved: { label: 'Approved, not yet executed', tone: 'info' },
  executed: { label: 'Executed', tone: 'success' },
  rejected: { label: 'Rejected', tone: 'neutral' },
  cancelled: { label: 'Cancelled', tone: 'neutral' },
  expired: { label: 'Expired', tone: 'neutral' },
}
export const changeStatus = (s: unknown) =>
  typeof s === 'string' && Object.prototype.hasOwnProperty.call(CHANGE_STATUS, s) ? CHANGE_STATUS[s] : { label: `Unrecognised (${safeToken(s)})`, tone: 'danger' as BadgeTone }

const LICENCE_STATUS: Record<string, { label: string; tone: BadgeTone }> = {
  active: { label: 'Active (recorded)', tone: 'success' },
  suspended: { label: 'Suspended (recorded)', tone: 'warning' },
  expired: { label: 'Expired (recorded)', tone: 'neutral' },
}
export const licenceStatus = (s: unknown) =>
  typeof s === 'string' && Object.prototype.hasOwnProperty.call(LICENCE_STATUS, s) ? LICENCE_STATUS[s] : { label: `Unrecognised (${safeToken(s)})`, tone: 'danger' as BadgeTone }

export const LICENSEE_LABEL: Record<string, string> = { platform: 'Platform is the licensee', tenant: 'Tenant is the licensee' }
export const licenseeLabel = (l: unknown) => (typeof l === 'string' && Object.prototype.hasOwnProperty.call(LICENSEE_LABEL, l) ? LICENSEE_LABEL[l] : `Unrecognised (${safeToken(l)})`)

export const LICENSING_MODEL_LABEL: Record<string, string> = {
  under_platform_licence: 'Under the platform licence',
  own_licence: 'Own licence (operator brings its own)',
}

export const dateText = (d: string | null | undefined) => (d ? d : 'Not recorded')
export const when = (iso: string | null | undefined) => (iso && !Number.isNaN(Date.parse(iso)) ? new Date(iso).toLocaleString() : 'None')
