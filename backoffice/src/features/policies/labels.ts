// Fixed, UI-owned wording for the lic16 policy screens. No sentence states a legal conclusion or recommends a value: every
// threshold, mode and action shown here was decided by named staff (compliance), not by the platform.
import type { BadgeTone } from '../../components/Badge'
import { safeToken } from '../licensing/labels'

export { safeToken }

export const NO_VALUE_FROM_PLATFORM =
  'The platform ships no policy and suggests no value. Every threshold, asset, operation and mode here comes from a decision of your compliance function, supported by a legal review you reference. Nothing on this page is a legal or regulatory authorisation.'

export const KYC_DORMANT_NOTE =
  'With no ACTIVE policy for a licence jurisdiction, KYC enforcement is dormant for it (nothing is required by this mechanism). That is the state of every jurisdiction until a policy is authored and activated.'

export const KYC_NOT_WIRED_NOTE =
  'Only the trigger types the evaluator consults can be authored: cumulative deposit and play. EDD amount and registration tier are NOT IMPLEMENTED and cannot be authored or activated.'

export const KYC_FOUR_EYES_NOTE =
  'Four-eyes is enforced by the database. A draft governs nothing. A DIFFERENT platform administrator than the author must activate it. To replace an active policy, one administrator authors a successor draft and a third administrator (different from both authors) supersedes. An active policy cannot be switched off; it can only be superseded.'

export const REASON_CODE_HELP = 'A code, not free text: 1 to 64 characters of letters, digits, _ . : - (it starts with a letter or digit).'

export const HARD_RESTRICTIONS_NOTE =
  'These restrictions are never affected by this policy: account or wallet status, KYC and AML or sanctions holds, risk decisions, the payout kill switch, provider capability and payout instrument checks. A policy can only ADD a refusal; "allow" never skips another gate.'

export const WRP_UNCONFIGURED_NOTE =
  'UNCONFIGURED: no policy is recorded for this cause, so the interim engineering default applies (withdrawals proceed at every stage, as before this policy existed). The default is not a decision about what any licence requires. Treat it as an open item for a tenant going live.'

export const WRP_STATUS_NOTE =
  'Tenant and brand suspension or closure, and market closure, are NOT configurable here. Tenant and brand status are governed by recorded owner decisions (new requests and payout submission stop; funds stay held; only the four-eyes hold release returns them). Market closure is not evaluated per withdrawal. They are shown for information only.'

export const WRP_SEMANTICS_NOTE =
  'Strictness order: allow, then hold for review, then block. A change that is at least as strict as the action in force at every stage is a tightening (or equal) change and applies at once with one platform administrator. Any less strict change is a loosening change: it needs approval by 2 different people (neither the requester). The database makes this classification; the label shown before filing is a hint.'

export const RESIDENCE_MODE_TEXT: Record<string, string> = {
  none: 'None: a single reviewer\'s residence determination is market evidence at once. This is the state of every tenant until a mode is configured.',
  into_open_market: 'Into open market: a determination into a market the brand serves needs a second, distinct Person before it is market evidence.',
  all: 'All: every residence determination needs a second, distinct Person before it is market evidence.',
}
export const RESIDENCE_MODE_SHORT: Record<string, string> = { none: 'None', into_open_market: 'Into open market', all: 'All' }
export const modeShort = (m: unknown) => (typeof m === 'string' && Object.prototype.hasOwnProperty.call(RESIDENCE_MODE_SHORT, m) ? RESIDENCE_MODE_SHORT[m] : `Unrecognised (${safeToken(m)})`)

export const RESIDENCE_PENDING_NOTE =
  'A pending determination is NOT market evidence. Until a second Person approves it, the player stays blocked at the market gate as if no residence were verified (an older approved determination, if any, stays in force). The country is not shown here: reading it is a separate permission.'

export const RESIDENCE_TIGHTEN_NOTE =
  'A mode change applies to FUTURE determinations only; each determination keeps the mode in force when it was made. Existing players are not re-checked.'

const KYC_STATUS: Record<string, { label: string; tone: BadgeTone }> = {
  draft: { label: 'Draft (governs nothing)', tone: 'warning' },
  active: { label: 'Active', tone: 'success' },
  withdrawn: { label: 'Withdrawn', tone: 'neutral' },
}
export const kycStatus = (s: unknown) => (typeof s === 'string' && Object.prototype.hasOwnProperty.call(KYC_STATUS, s) ? KYC_STATUS[s] : { label: `Unrecognised (${safeToken(s)})`, tone: 'danger' as BadgeTone })

const TRIGGER: Record<string, string> = {
  cumulative_deposit: 'Cumulative deposit',
  play: 'Play',
  edd_amount: 'EDD amount (not implemented)',
  registration_tier: 'Registration tier (not implemented)',
}
export const triggerLabel = (t: unknown) => (typeof t === 'string' && Object.prototype.hasOwnProperty.call(TRIGGER, t) ? TRIGGER[t] : `Unrecognised (${safeToken(t)})`)

const PLAY_OP: Record<string, string> = { casino_play: 'Casino play', sportsbook_play: 'Sportsbook play' }
export const playOpLabel = (t: unknown) => (typeof t === 'string' && Object.prototype.hasOwnProperty.call(PLAY_OP, t) ? PLAY_OP[t] : `Unrecognised (${safeToken(t)})`)

const CAUSE: Record<string, string> = {
  licence_suspended: 'Licence suspended',
  licence_expired: 'Licence expired',
  licence_not_yet_issued: 'Licence not yet issued',
  licence_missing: 'No licence bound',
  tenant_suspended: 'Tenant suspended',
  tenant_closed: 'Tenant closed',
  brand_suspended: 'Brand suspended',
  brand_closed: 'Brand closed',
  market_closed: 'Market closed',
}
export const causeLabel = (c: unknown) => (typeof c === 'string' && Object.prototype.hasOwnProperty.call(CAUSE, c) ? CAUSE[c] : `Unrecognised (${safeToken(c)})`)

const STAGE: Record<string, string> = { request: 'Request', dispatch: 'Dispatch', redispatch: 'Re-dispatch' }
export const stageLabel = (c: unknown) => (typeof c === 'string' && Object.prototype.hasOwnProperty.call(STAGE, c) ? STAGE[c] : `Unrecognised (${safeToken(c)})`)

const ACTION: Record<string, { label: string; tone: BadgeTone }> = {
  allow: { label: 'Allow', tone: 'neutral' },
  hold_for_review: { label: 'Hold for review', tone: 'warning' },
  block: { label: 'Block', tone: 'danger' },
}
export const actionLabel = (a: unknown) => (typeof a === 'string' && Object.prototype.hasOwnProperty.call(ACTION, a) ? ACTION[a] : { label: `Unrecognised (${safeToken(a)})`, tone: 'danger' as BadgeTone })

export const ACTION_MEANING: Record<string, string> = {
  allow: 'The withdrawal proceeds through this stage (every other gate still applies).',
  hold_for_review: 'Request: accepted with a hold placed. Dispatch and re-dispatch: the payout is parked or escalated for human review.',
  block: 'Request: refused. Dispatch and re-dispatch: parked or escalated, same as hold for review.',
}

const WRP_STATUS: Record<string, { label: string; tone: BadgeTone }> = {
  requested: { label: 'Pending approval', tone: 'warning' },
  approved: { label: 'Approved, not yet executed', tone: 'info' },
  executed: { label: 'Executed', tone: 'success' },
  rejected: { label: 'Rejected', tone: 'neutral' },
  cancelled: { label: 'Cancelled', tone: 'neutral' },
  expired: { label: 'Expired', tone: 'neutral' },
}
export const wrpStatus = (s: unknown) => (typeof s === 'string' && Object.prototype.hasOwnProperty.call(WRP_STATUS, s) ? WRP_STATUS[s] : { label: `Unrecognised (${safeToken(s)})`, tone: 'danger' as BadgeTone })

export const WRP_APPROVAL_RULES = [
  'A loosening change needs 2 approvals from 2 different people (different Person records); the final approval executes it.',
  'The requester cannot approve or reject their own request, and neither can another account of the same person.',
  'Every counted approval must be at most 24 hours old when the change executes; an open request expires 72 hours after filing.',
  'A tightening or equal change filed by a platform administrator takes effect at once, without a second person.',
] as const
