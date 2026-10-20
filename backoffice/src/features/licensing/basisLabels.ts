// Fixed, UI-owned wording for the operating-basis screens (lic12). A basis is a recorded business decision. No sentence here states
// a legal conclusion, approves a jurisdiction, or says an operation is lawful.
import type { BadgeTone } from '../../components/Badge'
import { safeToken } from './labels'

export const BASIS_DISCLAIMER =
  'An operating basis is a recorded business decision. It is not legal proof, not a licence and not an approval of any jurisdiction. A country configuration is likewise only what the operator chose to switch on.'

export const NO_LICENCE_NOTICE =
  'This records a documented decision, not proof of legality. Nothing here has been checked by the platform and it does not say that operating in this country is lawful. Review metadata is mandatory: the determination reference, the responsible function, the jurisdiction relied on and a review date. After the review date has passed, the market stops permitting until a review is recorded.'

export const BASIS_EVIDENCE_NOTE =
  'The determination and evidence references are supplied by the operator. The platform does not verify them, stores no document, and a reference is not proof of anything.'

export const BASIS_APPROVAL_NOTE =
  'Setting a basis is always a widening change: it needs 2 approvals from 2 different people and takes effect only after the second one. Withdrawing a basis and a review that shortens the date or flags re-review are narrowing changes and take effect at once. The server classifies each change.'

export const BASIS_KIND_LABEL: Record<string, string> = {
  platform_licence: 'Relies on a licence held by the platform',
  partner_licence: 'Relies on the partner\'s own licence',
  no_licence_documented: 'Documented decision without a licence',
}
export const basisKindLabel = (k: unknown) => (typeof k === 'string' && Object.prototype.hasOwnProperty.call(BASIS_KIND_LABEL, k) ? BASIS_KIND_LABEL[k] : `Unrecognised (${safeToken(k)})`)

const STATUS: Record<string, { label: string; tone: BadgeTone }> = {
  active: { label: 'Active (recorded)', tone: 'info' },
  withdrawn: { label: 'Withdrawn', tone: 'neutral' },
}
export const basisStatus = (s: unknown) =>
  typeof s === 'string' && Object.prototype.hasOwnProperty.call(STATUS, s) ? STATUS[s] : { label: `Unrecognised (${safeToken(s)})`, tone: 'danger' as BadgeTone }

export const REVIEW_LABEL: Record<string, { label: string; tone: BadgeTone }> = {
  none: { label: 'No review date', tone: 'neutral' },
  ok: { label: 'Review not due', tone: 'neutral' },
  due_soon: { label: 'Review due soon', tone: 'warning' },
  overdue: { label: 'Review overdue: no longer permitting', tone: 'danger' },
  rereview: { label: 'Flagged for re-review: no longer permitting', tone: 'danger' },
}

export const REVIEW_HINT = 'Shown against this computer\'s date as a hint. The server decides against the database clock; the review date itself still stands and the day after it does not.'
