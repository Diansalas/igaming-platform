// Fixed, UI-owned wording for the lic20 screening screens. No sentence recommends a value or states that anything is lawful.
import type { BadgeTone } from '../../components/Badge'
import { safeToken } from '../licensing/labels'

export { safeToken }

export const NO_POLICY_SHIPPED =
  'The platform ships no screening trigger policy, no interval, no grace and no gate. Every value here is a decision recorded by named staff of your compliance function, supported by a legal review you reference. This is a software capability, not a regulatory approval.'

export const GATE_WARNING =
  'WARNING: enabling the withdrawal gate while no working screening provider exists TRAPS PLAYER FUNDS. A gated withdrawal waits (parked) until screening clears the person, and with no working provider nothing can clear it. The platform refuses to enable a gate unless a screening worker has reported a recent heartbeat. That refusal is a safeguard, not permission: never enable a gate in production. Only the MOCK provider exists, and the production platform starts no screening worker.'

export const SERVER_ENFORCED_NOTE =
  'Enforced by the server, shown here for information. Writes need a fresh second-factor check (step-up), a signed proof bound to the exact request, an in-force AML officer grant and the officer quorum for the tenant. Authoring a trigger policy is for a platform administrator acting as an AML officer; resolving a request is for the owner-scope officer. This screen cannot grant or bypass any of that.'

export const RESOLUTION_NOTE =
  'Only a request that needs an officer or failed terminally, and is not yet resolved, can be resolved. A clear needs the lists you screened and clears the person only if no newer match exists. A potential match or confirmed match places a hold. An error outcome never resolves a request and is not offered. Replays are idempotent.'

export const SANCTIONS_RULE_NOTE =
  'A sanctions-list match can NOT be cleared by an officer alone: the server refuses a clear and requires the match to be recorded (a hold that is lifted only by a four-eyes proposal and approval). A false positive therefore cannot be cleared here.'

export const HOLDS_WITHDRAWALS_NOTE =
  'A request in needs officer or failed terminal that is not resolved holds the person\'s withdrawals until an officer resolves it.'

export const INTERVAL_GRACE_NOTE =
  'The re-screen interval and the backfill grace are values of the jurisdiction\'s screening policy, not of this trigger policy. Read and author them in the Screening policy section. A screening policy must already exist for the jurisdiction or the server refuses the trigger policy.'

export const SCREENING_POLICY_NOTE =
  'The screening policy says which lists are screened, how often a person is screened again (the re-screen interval) and how long a person who is already registered has before the first re-screen counts (the backfill grace). The platform ships NO value for any of them: every field is a decision recorded by named staff of your compliance function, supported by a legal review you reference. This is a software capability, not a regulatory approval.'

export const POLICY_SAFETY_RULES = [
  'Interval floor: the re-screen interval is at least 3600 seconds. This is an engineering safety floor of this software, not a legal value; below it every clear would expire within the hour and withdrawals would freeze.',
  'Gate threshold: where a withdrawal gate is in force for the jurisdiction, the interval must also be at least twice the gate alert threshold.',
  'Effective time: a version takes effect between now (2 minutes of clock skew are tolerated) and 30 days ahead.',
  'Tighten only: a version can never be looser than the latest one. A list cannot be removed, the interval and the grace cannot be longer, and the effective time cannot be earlier. Loosening has no approval path and is refused.',
  'A version is immutable. To change a policy, author the next version.',
] as const

export const EVENT_LABEL: Record<string, string> = {
  registration: 'Registration',
  kyc_approval: 'KYC approval',
  scheduled: 'Scheduled re-screen',
}
export const eventLabel = (e: unknown) => (typeof e === 'string' && Object.prototype.hasOwnProperty.call(EVENT_LABEL, e) ? EVENT_LABEL[e] : `Unrecognised (${safeToken(e)})`)

const STATUS: Record<string, { label: string; tone: BadgeTone }> = {
  pending: { label: 'Pending', tone: 'info' },
  claimed: { label: 'Claimed by worker', tone: 'info' },
  done: { label: 'Completed', tone: 'success' },
  needs_officer: { label: 'Needs officer', tone: 'warning' },
  failed_terminal: { label: 'Failed (terminal)', tone: 'danger' },
}
export const requestStatus = (s: unknown) => (typeof s === 'string' && Object.prototype.hasOwnProperty.call(STATUS, s) ? STATUS[s] : { label: `Unrecognised (${safeToken(s)})`, tone: 'danger' as BadgeTone })

export const LIST_LABEL: Record<string, string> = { sanctions: 'Sanctions list', pep: 'PEP list' }
export const listLabel = (l: unknown) => (typeof l === 'string' && Object.prototype.hasOwnProperty.call(LIST_LABEL, l) ? LIST_LABEL[l] : `Unrecognised (${safeToken(l)})`)

export const OUTCOME_LABEL: Record<string, string> = {
  clear: 'Clear (no match)',
  potential_match: 'Potential match (places a hold)',
  confirmed: 'Confirmed match (places a hold)',
}

export const isAwaitingOfficer = (status: string, resolved: boolean) => (status === 'needs_officer' || status === 'failed_terminal') && !resolved
